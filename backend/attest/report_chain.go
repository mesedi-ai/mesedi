package attest

// Report chaining: each report an SDK sends carries the fingerprint of
// the report it sent before it in the same run, so a report removed,
// changed or slipped in after sending breaks the chain.
//
// # WHAT THIS PROVES AND WHAT IT DOES NOT
//
// It proves the record Mesedi holds is the sequence the reporting
// process sent, in the order it sent it. It does not prove the reports
// are true: whoever holds the project key can send a chain of invented
// reports that links perfectly. Signing (ReportSignature) narrows that
// to whoever holds the signing key, which a team can keep in a
// reporting process the agent cannot reach. Whoever holds every key can
// still invent reports, and the docs say so.
//
// # WHY THE FINGERPRINT COVERS THE PAYLOAD AS SENT
//
// The payload is hashed as the exact bytes received, for the reason
// CanonicalLeaf gives: re-serializing JSON makes the hash depend on key
// order and whitespace. The server fingerprints a report before secret
// redaction rewrites its payload, and keeps the received payload hash
// beside the stored event, so a checker can tell "changed after
// sending" from "redacted by Mesedi on arrival".
//
// The SDKs serialize every payload compactly (no spaces) and hash the
// same bytes they send. Both SDKs carry the test vectors in
// report_chain_test.go, so the three implementations cannot drift
// apart silently.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

// ReportChainAlgorithmV1 names this construction. Any change to the
// fields or their encoding needs a new name.
const ReportChainAlgorithmV1 = "mesedi-report-chain-v1"

// ReportChainGenesis is the ChainPrev value of a run's first report:
// SHA-256 of the algorithm name, " genesis\n", and the execution id.
// Tied to the run, so the first report of one run cannot open another.
func ReportChainGenesis(executionID string) string {
	sum := sha256.Sum256([]byte(ReportChainAlgorithmV1 + " genesis\n" + executionID))
	return hex.EncodeToString(sum[:])
}

// ReportChainHash is a report's fingerprint, hex. payloadSHA256 is the
// hex SHA-256 of the payload bytes exactly as received; pass "" to have
// it computed from e.Payload (correct only before any rewrite).
//
// Fields are label:length:value lines, the CanonicalLeaf encoding, in
// this order: algorithm, event_id, execution_id, event_type, sequence,
// timestamp (UTC, microseconds, "2006-01-02T15:04:05.000000Z"),
// duration_ms, payload_sha256, chain_prev.
func ReportChainHash(e *events.Event, payloadSHA256 string) string {
	if e == nil {
		return ""
	}
	if payloadSHA256 == "" {
		sum := sha256.Sum256(e.Payload)
		payloadSHA256 = hex.EncodeToString(sum[:])
	}
	var buf []byte
	field := func(label, value string) {
		buf = append(buf, label...)
		buf = append(buf, ':')
		buf = append(buf, fmt.Sprintf("%d", len(value))...)
		buf = append(buf, ':')
		buf = append(buf, value...)
		buf = append(buf, '\n')
	}
	field("algorithm", ReportChainAlgorithmV1)
	field("event_id", e.EventID)
	field("execution_id", e.ExecutionID)
	field("event_type", string(e.EventType))
	field("sequence", fmt.Sprintf("%d", e.Sequence))
	field("timestamp", e.Timestamp.UTC().Truncate(storagePrecision).Format("2006-01-02T15:04:05.000000Z"))
	field("duration_ms", fmt.Sprintf("%d", e.DurationMs))
	field("payload_sha256", payloadSHA256)
	field("chain_prev", e.ChainPrev)
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// ReportChainEndMessage is the 32 bytes a signer signs to say which
// report ended a run: SHA-256 of the algorithm name, " end\n", the
// execution id, "\n", and the last report's fingerprint. Distinct from
// any report's own fingerprint on purpose: a report's signature says
// "this report was sent", and without the separate label anyone holding
// one could pass a report from the middle of a run off as its last.
func ReportChainEndMessage(executionID, lastFingerprint string) []byte {
	sum := sha256.Sum256([]byte(ReportChainAlgorithmV1 + " end\n" + executionID + "\n" + lastFingerprint))
	return sum[:]
}

// ReportChainLink is one stored report's place in its run's chain.
type ReportChainLink struct {
	EventID string
	Prev    string
	Hash    string
	Signed  bool
}

// ReportChainVerdict is the outcome of checking a run's links.
type ReportChainVerdict struct {
	// Chained is true when at least one report carried a chain_prev.
	Chained bool
	// Signed is true when at least one chained report was signed.
	Signed bool
	// Broken lists, in plain words, every way the links fail to form
	// one unbroken path from the genesis value. Empty means intact.
	Broken []string
	// Unsigned counts chained reports without a signature in a signed
	// run.
	Unsigned int
	// Unchained counts reports with no chain_prev in a chained run.
	Unchained int
	// EndUnconfirmed is true when no end was declared for the run (an
	// SDK that predates end confirmation, or a run closed by the server,
	// such as a timeout), so a missing last report would not show.
	EndUnconfirmed bool
}

// VerifyReportChain checks that the chained reports of one run form a
// single path: exactly one report starts from the genesis value, every
// other report links to a report that exists, and no two reports claim
// the same predecessor. unchainedReports is the number of the run's
// reports that carried no chain_prev at all. Order of links is
// irrelevant.
//
// end is the fingerprint the SDK said was the run's last report, sent
// when the run finished, or "" when none was sent. Without it a run
// whose last reports went missing still forms a clean path, because
// nothing points at a report that is gone; with it, a chain that stops
// short of the declared end is broken (owner decision, 17 November
// 2026).
func VerifyReportChain(executionID string, links []ReportChainLink, unchainedReports int, end string) ReportChainVerdict {
	v := ReportChainVerdict{}
	if len(links) == 0 {
		if end != "" {
			// The SDK chained the run, but Mesedi holds none of it.
			v.Chained = true
			v.Broken = append(v.Broken, "none of the run's chained reports are held, though the SDK said the run ended at one")
		}
		return v
	}
	v.Chained = true
	v.Unchained = unchainedReports
	genesis := ReportChainGenesis(executionID)
	// Both maps live only for this call: max one entry per link.
	byHash := make(map[string]bool, len(links))
	for _, l := range links {
		byHash[l.Hash] = true
		if l.Signed {
			v.Signed = true
		}
	}
	starts := 0
	prevUsed := make(map[string]int, len(links))
	missing := 0
	for _, l := range links {
		prevUsed[l.Prev]++
		switch {
		case l.Prev == genesis:
			starts++
		case !byHash[l.Prev]:
			missing++
		}
	}
	forks := 0
	for _, n := range prevUsed {
		if n > 1 {
			forks += n - 1
		}
	}
	if starts == 0 {
		v.Broken = append(v.Broken, "no report starts the chain: the first report is missing")
	}
	if starts > 1 {
		v.Broken = append(v.Broken, fmt.Sprintf("%d reports each claim to start the chain", starts))
	}
	if missing > 0 {
		v.Broken = append(v.Broken, fmt.Sprintf("%d report(s) link to a report Mesedi does not hold", missing))
	}
	if forks > 0 {
		v.Broken = append(v.Broken, fmt.Sprintf("%d report(s) claim a predecessor another report already claims", forks))
	}
	if v.Unchained > 0 {
		v.Broken = append(v.Broken, fmt.Sprintf("%d report(s) in this chained run carry no chain at all", v.Unchained))
	}
	if end == "" {
		v.EndUnconfirmed = true
	} else {
		var tips []string
		for _, l := range links {
			if prevUsed[l.Hash] == 0 {
				tips = append(tips, l.Hash)
			}
		}
		switch {
		case !byHash[end]:
			v.Broken = append(v.Broken, "the run's last report is missing: the chain stops before the report the SDK said ended the run")
		case len(tips) != 1 || tips[0] != end:
			v.Broken = append(v.Broken, "reports continue past the report the SDK said ended the run")
		}
	}
	if v.Signed {
		for _, l := range links {
			if !l.Signed {
				v.Unsigned++
			}
		}
	}
	return v
}
