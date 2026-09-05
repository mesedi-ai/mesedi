// Command mesedi-verify is the standalone, independent verifier for a
// Mesedi chain export.
//
// It exists so an auditor never has to take Mesedi's word for anything.
// Given an export file, it recomputes every hash from the export's own
// contents, then resolves each checkpoint against a public transparency
// log that neither Mesedi nor Verdifax controls. Mesedi is not contacted
// and no Mesedi credential is used.
//
// USAGE
//
//	mesedi-verify export.json              # verify, print a human report
//	cat export.json | mesedi-verify        # verify from stdin
//	mesedi-verify --json export.json       # machine-readable
//	mesedi-verify --pdf out.pdf export.json # also write a PDF report
//	mesedi-verify --offline export.json    # no network; see below
//	mesedi-verify --rekor <url> export.json
//
// EXIT CODES
//
//	0  Every check passed and every checkpoint was actually checked.
//	1  Either a check failed, or a checkpoint could not be checked at
//	   all. Both are non-zero on purpose: a pipeline gating on exit 0
//	   must not go green for a chain nobody was able to verify. The
//	   report distinguishes the two in words; the exit code does not,
//	   because "do not rely on this" is the same instruction either way.
//	2  The export could not be read or parsed, or a requested report
//	   could not be written. Separate from 1 on purpose: "we could not
//	   run the check" and "the check failed" must not look alike.
//
// # WHAT --offline PROVES, WHICH DEPENDS ON THE EXPORT
//
// This used to be simple: offline meant structural checks only, which a
// lying Mesedi could satisfy because Mesedi wrote every byte, so an
// offline run was not evidence. That is still true for a checkpoint
// carrying nothing but a leaf preimage, and such checkpoints are
// reported UNVERIFIABLE by name rather than passed over.
//
// It is no longer true for a checkpoint carrying an inclusion proof.
// That proof is Sigstore's signature over a Merkle tree head, and
// verifying it needs no network and no trust in Mesedi — the signature
// either checks out against a key compiled into this binary or it does
// not. For those checkpoints an offline run is STRONGER than a lookup:
// a lookup asks the log what it holds today and believes the answer,
// which trusts whoever is serving that endpoint, while a signed tree
// head cannot be forged by them and stays valid after the log is retired.
//
// So the report no longer states a blanket offline caveat. It states,
// per checkpoint, which of the two checks ran, and the summary sentence
// is chosen from those counts rather than from the flag.
//
// One thing an inclusion proof does NOT establish on its own is that the
// entry it proves has anything to do with your checkpoint. See offline.go
// for the binding step that closes that gap, and why omitting it would
// produce a verifier that is confidently green about somebody else's data.
//
// # LICENSE
//
// MIT (see LICENSE in this directory). The verifier is open source on
// purpose: an auditor must be able to read the code that adjudicates the
// evidence. A closed verifier is the vendor asserting again, one level up.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"mesedi/backend/internal/attest"
)

// buildVersion is set at link time for released binaries. It is never
// fabricated into something that looks like a release when it isn't one.
var buildVersion = "unknown"

// resolveVersion answers "which source produced this verdict?"
//
// The report tells the reader to rebuild this binary and reproduce the
// result. That instruction is worthless if the document will not say what
// to rebuild. A release sets buildVersion at link time; everything else
// falls back to the VCS stamp Go embeds automatically when building
// inside a checkout, which names the exact commit.
func resolveVersion() string {
	var rev, modified string
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				modified = s.Value
			}
		}
	}
	return formatVersion(buildVersion, rev, modified)
}

// formatVersion is split out so the interesting cases are testable
// without building a binary to inspect.
func formatVersion(stamped, rev, modified string) string {
	if stamped != "" && stamped != "unknown" {
		return stamped
	}
	if rev == "" {
		return "unknown (built outside a source checkout)"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if modified == "true" {
		// Load-bearing, not pedantry. A verdict produced by a binary whose
		// source matches no commit cannot be reproduced by anyone, which is
		// the one thing this report asks its reader to be able to do.
		return rev + " + UNCOMMITTED CHANGES"
	}
	return rev
}

// versionIsReproducible reports whether the named version identifies
// source a reader could actually obtain.
func versionIsReproducible(v string) bool {
	return !strings.Contains(v, "UNCOMMITTED") &&
		!strings.HasPrefix(v, "unknown")
}

type report struct {
	Format    string `json:"format"`
	ProjectID string `json:"project_id"`

	// ExportSHA256 binds this verdict to one specific file. Without it a
	// report is a floating claim that could be presented alongside any
	// export; with it, the reader can hash the JSON they were handed and
	// confirm this is a report about that file and no other.
	ExportSHA256 string `json:"export_sha256"`

	GeneratedAt time.Time                 `json:"export_generated_at"`
	VerifiedAt  time.Time                 `json:"verified_at"`
	Verifier    string                    `json:"verifier_version"`
	Online      bool                      `json:"log_was_consulted"`
	Structural  attest.ExportVerification `json:"structural"`
	LogEntries  []LogEntryCheck           `json:"log_entries,omitempty"`
	OK          bool                      `json:"ok"`
}

func main() {
	var (
		asJSON  = flag.Bool("json", false, "machine-readable output")
		offline = flag.Bool("offline", false,
			"do not contact the transparency log (proves substantially less)")
		rekorURL = flag.String("rekor", DefaultRekorURL, "transparency log base URL")
		pdfPath  = flag.String("pdf", "",
			"also write a PDF report to this path (for a reader who will not run a CLI)")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	version := resolveVersion()
	if *showVersion {
		fmt.Fprintln(os.Stdout, "mesedi-verify", version)
		return
	}

	raw, err := readExport(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "mesedi-verify:", err)
		os.Exit(2)
	}

	var export attest.ChainExport
	if err := json.Unmarshal(raw, &export); err != nil {
		fmt.Fprintln(os.Stderr, "mesedi-verify: this file is not a chain export:", err)
		os.Exit(2)
	}

	sum := sha256.Sum256(raw)

	rep := report{
		Format:       export.Format,
		ProjectID:    export.ProjectID,
		ExportSHA256: hex.EncodeToString(sum[:]),

		GeneratedAt: export.GeneratedAt,
		VerifiedAt:  time.Now().UTC(),
		Verifier:    version,
		Online:      !*offline,
		Structural:  attest.VerifyChainExport(export),
	}
	rep.OK = rep.Structural.OK

	// A limit on the REPORT rather than on the chain, and one the reader
	// cannot discover for themselves. If the verifier's own source is not
	// identifiable, "rebuild this and reproduce the result" is an
	// instruction nobody can follow, and the report should say so instead
	// of implying an auditability it does not have.
	if !versionIsReproducible(version) {
		rep.Structural.Unverified = append(rep.Structural.Unverified,
			"This report was produced by a verifier build that cannot be tied to a "+
				"published commit ("+version+"). The result above is therefore not "+
				"independently reproducible. Use a released binary, or one built from a "+
				"clean checkout, for anything that will be relied on.")
	}

	{
		// Run for both modes. An offline run now checks every stored
		// inclusion proof rather than skipping the anchors entirely, so
		// the old "offline shows only internal consistency" branch would
		// understate what happened — which is a falsehood in the safe
		// direction, but still a falsehood in a report whose job is to
		// say precisely what was established.
		rep.LogEntries = resolveLogEntries(export, *rekorURL, *offline)

		// A failed entry is a finding. An unverifiable one is not — but
		// it still means this report does not establish what it set out
		// to, so neither may leave rep.OK true. Exit 0 on a chain nobody
		// can check would be the single most misleading thing this tool
		// could do.
		var unverifiable []uint64
		for _, e := range rep.LogEntries {
			switch {
			case e.failed():
				rep.OK = false
			case e.unverifiable():
				unverifiable = append(unverifiable, e.Seq)
				rep.OK = false
			}
		}
		if len(unverifiable) > 0 {
			// No cause is asserted here. This used to name one — "anchored
			// before the leaf preimage was retained" — which was the only
			// cause that existed when it was written and is now one of
			// several: a mock ledger, a missing preimage, or an offline run
			// against a checkpoint that carries no inclusion proof. Printing
			// a confident wrong reason next to a correct verdict is how a
			// reader stops believing the correct parts, so the reasons stay
			// where they are individually true, per checkpoint, above.
			rep.Structural.Unverified = append(rep.Structural.Unverified, fmt.Sprintf(
				"Checkpoints %v could NOT be checked. That is not a finding against "+
					"the record — nothing here says they are wrong — but nothing here "+
					"says they are right either. The reason differs per checkpoint and "+
					"is given against each one in the PUBLIC TRANSPARENCY LOG section.",
				unverifiable))
		}
		// Replace the library's standing caveat, which is written for the
		// offline case, with one that describes what THIS run actually
		// did.
		//
		// The replacement must be driven by the counts, and it was not.
		// An earlier version substituted "Each checkpoint hash was
		// confirmed PRESENT in the public log" whenever the run was
		// online, regardless of whether a single entry had been resolved
		// — so a run in which nothing could be checked printed a claim of
		// verification directly above a RESULT line saying the opposite.
		// A false claim of verification inside the section whose entire
		// job is to prevent overclaiming is the worst place in the report
		// for one, and it survived because the substitution was written
		// at a moment when every run happened to resolve everything.
		rep.Structural.Unverified = replacePrefix(
			rep.Structural.Unverified,
			"Transparency log entries were NOT resolved",
			logResolutionCaveat(rep.LogEntries, *offline))

		// An anchor proof that was present and unusable must not vanish.
		// Reported separately from the verdict, because it describes what
		// was OFFERED, and the verdict describes what was established.
		for _, e := range rep.LogEntries {
			if e.ProofNote != "" {
				rep.Structural.Unverified = append(rep.Structural.Unverified,
					fmt.Sprintf("Checkpoint %d: %s.", e.Seq, e.ProofNote))
			}
		}
	}

	if *asJSON {
		out, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(os.Stdout, string(out))
	} else {
		printReport(os.Stdout, rep)
	}

	// The verdict is printed before the PDF is attempted, so a failure to
	// write the file never costs the reader the result they came for.
	if *pdfPath != "" {
		if err := writePDF(*pdfPath, rep); err != nil {
			fmt.Fprintln(os.Stderr, "mesedi-verify:", err)
			os.Exit(2)
		}
	}

	if !rep.OK {
		os.Exit(1)
	}
}

func writePDF(path string, rep report) error {
	out, err := renderPDF(buildPDFDoc(rep, rep.ExportSHA256))
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// logResolutionCaveat states what an online run actually established.
//
// Split out and given its own tests because it is the sentence most
// likely to be read as the report's headline claim, and because getting
// it wrong produces a falsehood rather than an omission. It must never
// assert that entries were confirmed when none were.
//
// The tree-head paragraph is appended only when at least one entry was
// genuinely resolved. Explaining what "was not ADDITIONALLY checked"
// after checking nothing invites the reader to infer a baseline that
// does not exist.
// The offline count is the reason this takes the entries rather than two
// integers. The tree-head sentence below says what was NOT checked, and
// for an entry settled by an inclusion proof that sentence is false —
// checking Sigstore's signature over the tree head is exactly what
// VerifyAnchor does. Deciding the wording from a bare verified count
// would reprint the old falsehood in a new place.
func logResolutionCaveat(entries []LogEntryCheck, offline bool) string {
	// Split into the DENIAL and the EXPLANATION, because the two used to
	// be one string and the mixed case then printed the denial one
	// sentence after distinguishing which entries it did not apply to —
	// contradicting itself on the page. Whether the tree head was checked
	// now has exactly one statement per run, and the explanation of why
	// it matters is appended in every case, since it is true regardless.
	const treeHeadDenial = " What was not additionally checked is the log's own " +
		"signed tree head."

	const whyTreeHeadMatters = " That further step would guard against Sigstore " +
		"itself being dishonest; it is not what protects you from Mesedi. For a hash " +
		"to be found there at all, Mesedi must genuinely have published it to an " +
		"append-only log it does not control. If Sigstore is dishonest, this system's " +
		"guarantee is gone by design, and that premise is stated up front rather than " +
		"papered over."

	const treeHead = treeHeadDenial + whyTreeHeadMatters

	// The antecedent is stated explicitly rather than left to "Each".
	// This sentence lands immediately after "the remaining N were not",
	// where a bare "Each" reads as covering every checkpoint in the
	// export rather than only the confirmed ones — true as written and
	// wrong as read, which in this section is the same thing.
	treeHeadChecked := func(n int) string {
		subject := fmt.Sprintf("Those %d were", n)
		if n == 1 {
			subject = "That one was"
		}
		return fmt.Sprintf(" %s additionally checked against Sigstore's own signed "+
			"tree head, using a public key compiled into this verifier, so that much "+
			"does not depend on the log being reachable or truthful at the moment you "+
			"read this — and it will still hold if that log is ever retired.", subject)
	}

	total := len(entries)
	var verified, offlineVerified int
	for _, e := range entries {
		if !e.verified() {
			continue
		}
		verified++
		if e.Method == MethodOfflineProof {
			offlineVerified++
		}
	}

	// How the confirmed entries were confirmed, appended to the counts
	// sentence. Three states, because "all", "none" and "some" license
	// three different claims and merging any two of them overstates one.
	tail := treeHead
	switch {
	case offlineVerified == verified && verified > 0:
		tail = treeHeadChecked(offlineVerified)
	case offlineVerified > 0:
		subject := fmt.Sprintf("%d of those were", offlineVerified)
		if offlineVerified == 1 {
			subject = "One of those was"
		}
		// No denial here: the clause below already says which entries the
		// tree head was and was not checked for. Appending the blanket
		// denial as well, which is what this did, contradicts the sentence
		// immediately before it.
		tail = fmt.Sprintf(" %s proven with an inclusion proof carried in the export, "+
			"which does include Sigstore's signed tree head. The rest were confirmed "+
			"by asking the log, which does not.%s", subject, whyTreeHeadMatters)
	}

	switch {
	case total == 0:
		return "This export contains no checkpoints to resolve against a transparency log."

	case verified == 0 && offline:
		return fmt.Sprintf(
			"No network was used, and NONE of the %d checkpoints in this export "+
				"carried an inclusion proof this verifier could check. This report "+
				"therefore rests entirely on the export's internal consistency, which "+
				"an export built to be self-consistent would also satisfy. Treat it as "+
				"a structural check, not as evidence. Re-run without --offline.", total)

	case verified == 0:
		return fmt.Sprintf(
			"The transparency log was contacted, but NONE of the %d checkpoints in "+
				"this export could be confirmed present in it. This report therefore "+
				"rests entirely on the export's internal consistency, which an export "+
				"built to be self-consistent would also satisfy. Treat it as a "+
				"structural check, not as evidence.", total)

	case verified < total:
		return fmt.Sprintf(
			"%d of %d checkpoints were confirmed PRESENT in the public log at the "+
				"index each claims. The remaining %d were not, and are listed above "+
				"with the reason for each.%s",
			verified, total, total-verified, tail)

	default:
		return fmt.Sprintf(
			"All %d checkpoint leaves were confirmed PRESENT in the public log at the "+
				"index each claims.%s", total, tail)
	}
}

func readExport(path string) ([]byte, error) {
	if path == "" || path == "-" {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("reading stdin: %w", err)
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("no export given. Pass a file, or pipe one in")
		}
		return raw, nil
	}
	return os.ReadFile(path)
}

// resolveLogEntries asks the log about every checkpoint.
//
// Continues past failures rather than stopping at the first. An auditor
// needs to know whether one checkpoint is missing from the log or all of
// them; those are very different findings and stopping early hides which.
// resolveLogEntries asks the log about every checkpoint.
//
// # WHAT IS ACTUALLY COMPARED, AND WHY IT IS NOT THE CHECKPOINT HASH
//
// The transparency log does not record the checkpoint hash. It records
// sha256 of a canonical leaf built by the anchoring service, and the
// checkpoint hash is one field inside that leaf. An earlier version of
// this function compared the log's value against Checkpoint.Hash
// directly and reported a mismatch on every checkpoint in production —
// correctly, in the sense that the two are never equal, and uselessly,
// in the sense that it could not distinguish that from real tampering.
//
// So the export now carries the leaf preimage, and the check is:
//
//	sha256(leaf_preimage) == what the log records at the claimed index
//	AND the checkpoint's own hash appears inside leaf_preimage
//
// Both halves are needed. The first alone proves the export knows a
// string the log committed to; the second is what ties that string to
// THIS checkpoint. Neither requires trusting Mesedi.
//
// The preimage is searched, never parsed: its fields are joined without
// length prefixing, so it cannot be split back into parts unambiguously.
//
// # OFFLINE IS NO LONGER A WEAKER RUN, WHEN A PROOF IS PRESENT
//
// Every checkpoint is now attempted against its stored inclusion proof
// first, whether or not the network is allowed. A proof settles the
// question outright and more strongly than a lookup does — see
// offline.go — so the lookup is a fallback for checkpoints that carry no
// proof, which is every checkpoint anchored before 2026-09-05.
//
// offline=true therefore does not mean "prove less". It means "use only
// what the export carries", and for a checkpoint with a proof that is a
// complete result. For one without, the entry is reported UNVERIFIABLE
// by name rather than silently omitted.
func resolveLogEntries(
	export attest.ChainExport, rekorURL string, offline bool,
) []LogEntryCheck {
	// A nil client is the signal that no lookup may happen. Passing nil
	// rather than a flag means an accidental network call in this path
	// panics in a test instead of quietly reaching the internet during a
	// run the operator asked to keep local.
	var client *rekorClient
	if !offline {
		client = newRekorClient(rekorURL)
	}
	ctx := context.Background()

	out := make([]LogEntryCheck, 0, len(export.Intervals))
	for _, iv := range export.Intervals {
		out = append(out, resolveOne(ctx, client, iv))
	}
	return out
}

func resolveOne(
	ctx context.Context, client *rekorClient, iv attest.ExportedInterval,
) LogEntryCheck {
	seq := iv.Checkpoint.Seq
	check := LogEntryCheck{Seq: seq, LogIndex: iv.LogEntryID}

	if iv.LogEntryID == "" {
		check.Status = StatusFailed
		check.Detail = "the export names no log entry for this checkpoint, so it was never published"
		return check
	}

	// A mock-ledger anchor makes no public-log claim at all. Looking it
	// up would fail, and reporting that failure would accuse the record
	// of something untrue. Refuse to check it, and say why.
	if iv.LedgerBackend != "" && iv.LedgerBackend != "rekor" {
		check.Status = StatusUnverifiable
		check.Detail = fmt.Sprintf(
			"anchored to the %q ledger, not a public transparency log. Nothing was "+
				"published for this interval, so there is nothing to check against. "+
				"This is not a finding about the record",
			iv.LedgerBackend)
		return check
	}

	// No preimage, no path from this checkpoint to that log entry. The
	// entry may well be genuine; it simply cannot be tied to anything.
	if iv.LeafPreimage == "" {
		check.Status = StatusUnverifiable
		check.Detail = "the export carries no leaf preimage for this checkpoint, so its " +
			"log entry cannot be tied back to it. Checkpoints anchored before " +
			"2026-09-04 are permanently in this state and cannot be repaired. " +
			"This is NOT evidence of tampering"
		return check
	}

	// The leaf must commit to this checkpoint. Checked before the network
	// call: if the preimage does not mention this checkpoint, whatever
	// the log holds is irrelevant to it.
	if !strings.Contains(iv.LeafPreimage, iv.Checkpoint.Hash) {
		check.Status = StatusFailed
		check.Detail = fmt.Sprintf(
			"the leaf that was anchored does not contain this checkpoint's hash (%s), "+
				"so the log entry describes a different record",
			shorten(iv.Checkpoint.Hash))
		return check
	}

	// Offline first, because it is strictly stronger than the lookup
	// below and costs nothing. The lookup asks the log what it holds
	// today and believes the answer; the proof carries Sigstore's
	// signature over a tree head, which whoever answers the request
	// cannot forge.
	off := verifyAnchorOffline(iv)
	check.ProofNote = off.Note
	if off.Decided {
		check.Method = MethodOfflineProof
		check.Status = off.Status
		check.Detail = off.Detail
		return check
	}

	// No usable proof and no network. Unverifiable, and named as such:
	// an offline run that quietly reported nothing for this checkpoint
	// would be indistinguishable from one that checked it.
	if client == nil {
		check.Status = StatusUnverifiable
		check.Detail = "this run did not contact the log, and this checkpoint carries " +
			"no usable inclusion proof, so nothing here checked it. Re-run without " +
			"--offline. This is NOT a finding about the record"
		return check
	}

	check.Method = MethodLogLookup
	recorded, integrated, err := client.lookup(ctx, iv.LogEntryID)
	if err != nil {
		check.Status = StatusFailed
		check.Detail = err.Error()
		return check
	}
	check.Integrated = integrated

	sum := sha256.Sum256([]byte(iv.LeafPreimage))
	computed := hex.EncodeToString(sum[:])
	if !strings.EqualFold(recorded, computed) {
		check.Status = StatusFailed
		check.Detail = fmt.Sprintf(
			"the log entry records %s but this checkpoint's leaf hashes to %s. The "+
				"published record and the record you were given are not the same",
			shorten(recorded), shorten(computed))
		return check
	}

	check.Status = StatusVerified
	check.Detail = "the leaf committing to this checkpoint is present in the public log " +
		"at the index it claims"
	return check
}

func printReport(w io.Writer, rep report) {
	fmt.Fprintf(w, "Mesedi chain verification\n")
	fmt.Fprintf(w, "  project        %s\n", rep.ProjectID)
	fmt.Fprintf(w, "  export sha256  %s\n", rep.ExportSHA256)
	fmt.Fprintf(w, "  export written %s\n", rep.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "  verified       %s by mesedi-verify %s\n\n",
		rep.VerifiedAt.Format(time.RFC3339), rep.Verifier)

	fmt.Fprintln(w, "STRUCTURE")
	for _, c := range rep.Structural.Checks {
		fmt.Fprintf(w, "  %s %-34s %s\n", mark(c.OK), c.Name, c.Detail)
	}

	// Gated on there being results, NOT on the run having been online.
	// It was gated on Online, and once offline runs began producing real
	// per-checkpoint verdicts that meant the report printed a caveat
	// saying the reasons were "listed above" while suppressing the list.
	// Found by running against production; no test noticed, because the
	// section's contents were what got tested and never its presence.
	if len(rep.LogEntries) > 0 {
		fmt.Fprintln(w, "\nPUBLIC TRANSPARENCY LOG")
		for _, e := range rep.LogEntries {
			label := fmt.Sprintf("checkpoint %d", e.Seq)
			if e.LogIndex != "" {
				label += " @ " + e.LogIndex
			}
			fmt.Fprintf(w, "  %s %-34s %s\n", statusMark(e.Status), label,
				wrap(e.Detail, 74, strings.Repeat(" ", 42)))
		}
	}

	// Printed for passing runs too, and at the same weight as the results.
	// A report that lists only what passed invites the reader to assume
	// everything was checked.
	fmt.Fprintln(w, "\nWHAT THIS REPORT DOES NOT SHOW")
	for _, u := range rep.Structural.Unverified {
		fmt.Fprintf(w, "  - %s\n", wrap(u, 74, "    "))
	}

	fmt.Fprintln(w)

	// Three verdicts, because "we found a problem" and "we could not
	// look" must never print the same sentence.
	var failed, unchecked int
	for _, e := range rep.LogEntries {
		switch {
		case e.failed():
			failed++
		case e.unverifiable():
			unchecked++
		}
	}

	switch {
	case failed > 0 || !rep.Structural.OK:
		fmt.Fprintln(w, "RESULT: FAILED. See the lines marked FAIL above.")
	case unchecked > 0:
		fmt.Fprintf(w, "RESULT: INCOMPLETE. %d checkpoint(s) could not be checked "+
			"against the public log.\n", unchecked)
		fmt.Fprintln(w, "Nothing here says the record is wrong. Nothing here says it is right")
		fmt.Fprintln(w, "either. Do not present this as a verified chain.")
	default:
		fmt.Fprintln(w, "RESULT: every check that ran passed.")
		fmt.Fprintln(w, "The record is intact. That is not the same as the AI having been correct.")
	}
}

func mark(ok bool) string {
	if ok {
		return "[ ok ]"
	}
	return "[FAIL]"
}

// statusMark renders the three log-entry outcomes. "unverifiable" gets
// its own marker rather than borrowing [FAIL], because a reader
// skimming the left column is exactly the reader who would otherwise
// carry away "Mesedi failed" from a checkpoint nobody can check.
func statusMark(status string) string {
	switch status {
	case StatusVerified:
		return "[ ok ]"
	case StatusUnverifiable:
		return "[ ?? ]"
	default:
		return "[FAIL]"
	}
}

func shorten(h string) string {
	if len(h) <= 16 {
		return h
	}
	return h[:8] + "..." + h[len(h)-8:]
}

func replacePrefix(lines []string, prefix, replacement string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			out = append(out, replacement)
			continue
		}
		out = append(out, l)
	}
	return out
}

// wrap keeps the limits readable in a terminal. They are the part of the
// report most likely to be skimmed, so they should not arrive as one
// unbroken line.
func wrap(s string, width int, indent string) string {
	var (
		b    strings.Builder
		line int
	)
	for i, word := range strings.Fields(s) {
		if i > 0 {
			if line+1+len(word) > width {
				b.WriteString("\n" + indent)
				line = 0
			} else {
				b.WriteString(" ")
				line++
			}
		}
		b.WriteString(word)
		line += len(word)
	}
	return b.String()
}
