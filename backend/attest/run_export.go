package attest

// The single-run file: one agent run's reports, their chain links and
// signatures, so a reader can check a run without trusting Mesedi
// (owner decision, 17 November 2026).
//
// The hourly chain export carries one digest per run, which proves the
// set of runs is complete and unaltered but not what any run said. This
// file carries the run's reports. A checker that holds both:
//
//  1. recomputes the run's digest from the anchored reports here and
//     compares it with the digest the hourly export anchored;
//  2. recomputes every chain fingerprint from its report and checks it
//     matches the chain record;
//  3. checks each report's content against the hash taken on arrival,
//     before Mesedi redacted any secret;
//  4. checks the links form one unbroken path; and
//  5. checks every signature against a public key, preferably the
//     reader's own copy rather than the one in this file.
//
// Payloads are carried as base64 of the exact stored bytes. JSON
// re-encoding can change bytes (Go escapes "<", ">" and "&"), and every
// hash here is over bytes.

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

// RunExportFormatV1 identifies this file format.
const RunExportFormatV1 = "mesedi.run-export.v1"

// RunExport is one run's reports and chain records.
type RunExport struct {
	Format      string    `json:"format"`
	ProjectID   string    `json:"project_id"`
	ExecutionID string    `json:"execution_id"`
	GeneratedAt time.Time `json:"generated_at"`

	Events      []RunExportEvent `json:"events"`
	ReportChain []RunExportLink  `json:"report_chain"`

	// ReportChainStatus is Mesedi's own verdict on the run's chain
	// (unchained, chained, signed or broken), recorded at completion.
	// The checker reaches its own and says when they differ.
	ReportChainStatus string `json:"report_chain_status,omitempty"`
	ReportChainDetail string `json:"report_chain_detail,omitempty"`

	// ReportChainEnd is the end the SDK declared when the run finished:
	// the last report's fingerprint, signed for a signed run (over
	// ReportChainEndMessage). Nil when none was declared.
	ReportChainEnd *RunExportEnd `json:"report_chain_end,omitempty"`

	// SigningKeys are the project's registered public keys, as Mesedi
	// holds them. A reader should check signatures against their own
	// copy; these are a convenience, and the report says which was used.
	SigningKeys []RunExportKey `json:"signing_keys,omitempty"`
}

// RunExportEvent is one stored report. AfterAnchoring marks a report
// that arrived after the run's digest was anchored: kept, but not
// covered by the anchor.
type RunExportEvent struct {
	EventID        string    `json:"event_id"`
	EventType      string    `json:"event_type"`
	Sequence       int       `json:"sequence"`
	Timestamp      time.Time `json:"timestamp"`
	DurationMs     int64     `json:"duration_ms,omitempty"`
	PayloadB64     string    `json:"payload_b64"`
	AfterAnchoring bool      `json:"after_anchoring,omitempty"`
}

// RunExportLink is one chained report as the server received it.
type RunExportLink struct {
	EventID       string `json:"event_id"`
	Prev          string `json:"prev"`
	Hash          string `json:"hash"`
	PayloadSHA256 string `json:"payload_sha256"`
	KeyID         string `json:"key_id,omitempty"`
	Signature     string `json:"signature,omitempty"`
}

// RunExportEnd is a run's declared end.
type RunExportEnd struct {
	Hash      string `json:"hash"`
	KeyID     string `json:"key_id,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// RunExportKey is a registered Ed25519 public key, base64.
type RunExportKey struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
	RevokedAt string `json:"revoked_at,omitempty"`
}

// RunVerification is what a check of one run established. Failures say
// something checked did not hold; Gaps say something could not be
// established; Unverified says what the check does not show.
type RunVerification struct {
	ExecutionID string        `json:"execution_id"`
	Checks      []CheckResult `json:"checks"`
	Failures    []string      `json:"failures,omitempty"`
	Gaps        []string      `json:"gaps,omitempty"`
	Unverified  []string      `json:"unverified"`
	// ChainStatus is the checker's own finding: unchained, chained,
	// signed or broken.
	ChainStatus string `json:"chain_status"`
}

func (v *RunVerification) check(name string, ok bool, format string, args ...any) {
	v.Checks = append(v.Checks, CheckResult{Name: name, OK: ok, Detail: fmt.Sprintf(format, args...)})
}

// RunKeys are the public keys signatures are checked against, by key id.
// FromReader says the keys came from the reader rather than the file.
type RunKeys struct {
	Keys       map[string]ed25519.PublicKey
	FromReader bool
}

// KeysFromRunExport reads the file's own copy of the keys.
func KeysFromRunExport(r RunExport) (RunKeys, error) {
	out := RunKeys{Keys: map[string]ed25519.PublicKey{}}
	for _, k := range r.SigningKeys {
		raw, err := base64.StdEncoding.DecodeString(k.PublicKey)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return out, fmt.Errorf("key %q in the file is not a base64 Ed25519 public key", k.KeyID)
		}
		out.Keys[k.KeyID] = ed25519.PublicKey(raw)
	}
	return out, nil
}

// VerifyRunExport checks one run against the digest the hourly export
// anchored for it (anchoredRoot; "" when the run is not in that file).
func VerifyRunExport(r RunExport, anchoredRoot string, keys RunKeys) RunVerification {
	v := RunVerification{ExecutionID: r.ExecutionID}
	if r.Format != RunExportFormatV1 {
		v.Failures = append(v.Failures, fmt.Sprintf("the run file claims format %q; this checker reads %q", r.Format, RunExportFormatV1))
		v.check("file format", false, "%q", r.Format)
		return v
	}
	v.check("file format", true, "%s", r.Format)

	// These maps live only for this call: max one entry per report in
	// the file, which the reader chose to open.
	byID := map[string]*events.Event{}
	payloads := map[string][]byte{}
	var anchored []*events.Event
	for _, re := range r.Events {
		raw, err := base64.StdEncoding.DecodeString(re.PayloadB64)
		if err != nil {
			v.Failures = append(v.Failures, fmt.Sprintf("report %s carries an unreadable payload", re.EventID))
			continue
		}
		e := &events.Event{EventID: re.EventID, ExecutionID: r.ExecutionID, EventType: events.EventType(re.EventType),
			Sequence: re.Sequence, Timestamp: re.Timestamp, DurationMs: re.DurationMs, Payload: raw}
		byID[re.EventID] = e
		payloads[re.EventID] = raw
		if !re.AfterAnchoring {
			anchored = append(anchored, e)
		}
	}

	// 1. The anchored reports are the ones the hour anchored.
	switch {
	case anchoredRoot == "":
		v.Gaps = append(v.Gaps, "this run is not in the hourly file given, so its reports cannot be tied to an anchor")
		v.check("reports match the anchor", false, "run not found in the hourly file")
	case len(anchored) == 0:
		v.Failures = append(v.Failures, "the hourly file anchors this run, but the run file holds no anchored reports")
		v.check("reports match the anchor", false, "no anchored reports in the run file")
	default:
		d, err := Compute(r.ExecutionID, anchored)
		if err != nil || !strings.EqualFold(d.Root, anchoredRoot) {
			v.Failures = append(v.Failures, "the run's anchored reports do not rebuild the digest the hour anchored, so they are not the reports that were anchored")
			v.check("reports match the anchor", false, "rebuilt %s, anchored %s", shortHex(d.Root), shortHex(anchoredRoot))
		} else {
			notes := 0
			for _, e := range anchored {
				if events.IsBackendMinted(e.EventType) {
					notes++
				}
			}
			what := plural(len(anchored)-notes, "report", "reports")
			if notes > 0 {
				what += " and " + plural(notes, "note Mesedi wrote", "notes Mesedi wrote")
			}
			v.check("reports match the anchor", true, "%s rebuild the anchored digest %s", what, shortHex(anchoredRoot))
		}
	}
	if late := len(r.Events) - len(anchored); late > 0 {
		v.Unverified = append(v.Unverified, fmt.Sprintf("%s after this run was anchored; checked against the chain, but no anchor covers %s.", agree(late, "record arrived", "records arrived"), verb(late, "it", "them")))
	}

	// 2 and 3. Each chain record matches its report, and each report's
	// content matches what was sent.
	var links []ReportChainLink
	chained := map[string]bool{}
	redacted, changed, unmatched, mismatched := 0, 0, 0, 0
	for _, l := range r.ReportChain {
		e, ok := byID[l.EventID]
		if !ok {
			unmatched++
			continue
		}
		chained[l.EventID] = true
		withPrev := *e
		withPrev.ChainPrev = l.Prev
		if ReportChainHash(&withPrev, l.PayloadSHA256) != l.Hash {
			mismatched++
		}
		sum := sha256.Sum256(payloads[l.EventID])
		if !strings.EqualFold(hex.EncodeToString(sum[:]), l.PayloadSHA256) {
			if strings.Contains(string(payloads[l.EventID]), "[REDACTED:") {
				redacted++
			} else {
				changed++
			}
		}
		links = append(links, ReportChainLink{EventID: l.EventID, Prev: l.Prev, Hash: l.Hash, Signed: l.Signature != ""})
	}
	if mismatched > 0 {
		v.Failures = append(v.Failures, fmt.Sprintf("the chain records of %s do not match their reports", plural(mismatched, "report", "reports")))
	}
	if unmatched > 0 {
		v.Gaps = append(v.Gaps, fmt.Sprintf("%s in the chain %s not in the run file", plural(unmatched, "report", "reports"), verb(unmatched, "is", "are")))
	}
	if changed > 0 {
		v.Failures = append(v.Failures, fmt.Sprintf("%s from what was sent and %s no redaction mark", agree(changed, "report differs", "reports differ"), verb(changed, "carries", "carry")))
	}
	if redacted > 0 {
		v.Unverified = append(v.Unverified, fmt.Sprintf("%s changed by Mesedi on arrival to remove a secret ([REDACTED:...] marks); the chain covers what was sent, so the removed text cannot be shown, only that the rest matches its fingerprint.", agree(redacted, "report was", "reports were")))
	}
	v.check("chain records match the reports", changed == 0 && mismatched == 0, "%s", plural(len(links), "chained report", "chained reports"))

	// 4. One unbroken path.
	unchained := 0
	for id, e := range byID {
		if !chained[id] && !events.IsBackendMinted(e.EventType) {
			unchained++
		}
	}
	end := ""
	if r.ReportChainEnd != nil {
		end = r.ReportChainEnd.Hash
	}
	cv := VerifyReportChain(r.ExecutionID, links, unchained, end)
	if cv.Chained && cv.EndUnconfirmed {
		v.Unverified = append(v.Unverified, "The run's end was not declared (an SDK that predates end confirmation, or a run closed by the server), so reports missing from the end of this run would not show here.")
	}
	switch {
	case !cv.Chained:
		v.ChainStatus = "unchained"
		v.Unverified = append(v.Unverified, "This run's reports carry no chain: they were sent by an SDK that predates report chaining, so a report removed or slipped in after sending would not show here.")
		v.check("chain unbroken", true, "no chain to check")
	case len(cv.Broken) > 0:
		v.ChainStatus = "broken"
		v.Gaps = append(v.Gaps, "the run's chain is broken: "+strings.Join(cv.Broken, "; ")+". A break can be a lost report as well as an altered record, and this file cannot say which")
		v.check("chain unbroken", false, "%s", strings.Join(cv.Broken, "; "))
	default:
		v.ChainStatus = "chained"
		v.check("chain unbroken", true, "one path from the run's first report")
	}

	// 5. Signatures.
	signatureProblem := false
	if cv.Signed {
		bad, missingKey, unsigned := 0, map[string]bool{}, cv.Unsigned
		for _, l := range r.ReportChain {
			if l.Signature == "" || !chained[l.EventID] {
				continue
			}
			pub, ok := keys.Keys[l.KeyID]
			if !ok {
				missingKey[l.KeyID] = true
				continue
			}
			sig, err := base64.StdEncoding.DecodeString(l.Signature)
			msg, herr := hex.DecodeString(l.Hash)
			if err != nil || herr != nil || !ed25519.Verify(pub, msg, sig) {
				bad++
			}
		}
		if bad > 0 {
			v.Failures = append(v.Failures, fmt.Sprintf("%s a signature that does not verify", agree(bad, "report carries", "reports carry")))
		}
		if len(missingKey) > 0 {
			ids := make([]string, 0, len(missingKey))
			for id := range missingKey {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			v.Gaps = append(v.Gaps, fmt.Sprintf("no public key was available for signing key %s, so those signatures were not checked", strings.Join(ids, ", ")))
		}
		if unsigned > 0 {
			v.Gaps = append(v.Gaps, fmt.Sprintf("%s in this signed run %s unsigned", plural(unsigned, "report", "reports"), verb(unsigned, "is", "are")))
		}
		// The declared end must be signed too, over its own message, or
		// a forger with the project key could cut the run short and
		// declare an earlier report its end.
		endProblem := ""
		switch {
		case r.ReportChainEnd == nil:
			endProblem = "this signed run declares no end, so its last reports could be missing unseen"
		case r.ReportChainEnd.Signature == "":
			endProblem = "this signed run's declared end is unsigned"
		default:
			if pub, ok := keys.Keys[r.ReportChainEnd.KeyID]; !ok {
				missingKey[r.ReportChainEnd.KeyID] = true
			} else if sig, err := base64.StdEncoding.DecodeString(r.ReportChainEnd.Signature); err != nil ||
				!ed25519.Verify(pub, ReportChainEndMessage(r.ExecutionID, r.ReportChainEnd.Hash), sig) {
				v.Failures = append(v.Failures, "the signature on the run's declared end does not verify")
				bad++
			}
		}
		if endProblem != "" {
			v.Gaps = append(v.Gaps, endProblem)
			unsigned++
		}
		ok := bad == 0 && len(missingKey) == 0 && unsigned == 0
		signatureProblem = bad > 0 || unsigned > 0
		if v.ChainStatus == "chained" && !signatureProblem {
			// Signed throughout, even if a missing key left some
			// signatures unchecked; that is a gap, reported above.
			v.ChainStatus = "signed"
		}
		source := "keys supplied by the reader"
		if !keys.FromReader {
			source = "the keys in this file, as Mesedi holds them"
			v.Unverified = append(v.Unverified, "Signatures were checked against the public keys in this file, which come from Mesedi. Check them against your own copy of the key with --report-key to make the check independent of Mesedi.")
		}
		v.check("signatures valid", ok, "checked against %s", source)
		v.Unverified = append(v.Unverified, "A valid signature shows the holder of the signing key sent these reports. It does not show the reports are true: whoever holds every key can still invent reports.")
	}

	// Mesedi's own verdict, compared with the checker's. A contradiction
	// is a finding: Mesedi said the chain was intact and it is not, or
	// the reverse. Broken here means a broken path or a signature
	// problem, the two things Mesedi's completion check also judges.
	if r.ReportChainStatus != "" {
		mesediBroken := r.ReportChainStatus == "broken"
		ourBroken := v.ChainStatus == "broken" || signatureProblem
		mesediChained := r.ReportChainStatus != "unchained"
		ourChained := v.ChainStatus != "unchained"
		if mesediBroken != ourBroken || mesediChained != ourChained {
			v.Failures = append(v.Failures, fmt.Sprintf("Mesedi recorded this run's chain as %q, but the run file shows %q%s",
				r.ReportChainStatus, v.ChainStatus, map[bool]string{true: " with a signature problem", false: ""}[signatureProblem]))
		}
	}

	v.Unverified = append(v.Unverified, "Nothing here judges whether the AI was right. An intact run means the record of what it did is intact, not that what it did was good.")
	return v
}

// agree is plural with the verb attached: "1 report was", "2 reports
// were". A report read by an auditor should not say "1 report were".
func agree(n int, one, many string) string {
	return plural(n, one, many)
}

// verb picks the word that agrees with n, without the number.
func verb(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func shortHex(h string) string {
	if len(h) <= 16 {
		return h
	}
	return h[:8] + "..." + h[len(h)-8:]
}
