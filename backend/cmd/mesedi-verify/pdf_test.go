package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"mesedi/backend/internal/attest"
)

// The PDF is tested at the content layer, not the pixel layer.
//
// buildPDFDoc is a pure function, so every property that actually
// matters, that a failure says FAILED, that the caveats survive
// verbatim, that an offline run does not claim the log was consulted ,
// can be asserted without parsing PDF bytes. renderPDF gets a smoke
// test, because the useful question there is "does it produce a valid
// file without panicking on awkward input", and that is answerable.

func logStatus(ok bool) string {
	if ok {
		return StatusVerified
	}
	return StatusFailed
}

func sampleReport(ok, online bool) report {
	return report{
		Format:       attest.ChainExportFormatV1,
		ProjectID:    "proj-agency-a",
		ExportSHA256: strings.Repeat("ab", 32),
		GeneratedAt:  time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC),
		VerifiedAt:   time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC),
		Verifier:     "test",
		Online:       online,
		OK:           ok,
		Structural: attest.ExportVerification{
			OK: ok,
			Checks: []attest.CheckResult{
				{Name: "export format", OK: true, Detail: attest.ChainExportFormatV1},
				{Name: "chain continuity", OK: ok, Detail: "3 consecutive checkpoints"},
			},
			Unverified: []string{
				"Execution digests were not opened, that requires the events.",
				"Nothing here judges whether the AI was correct.",
			},
		},
		LogEntries: []LogEntryCheck{
			{Seq: 1, LogIndex: "10000001", Status: logStatus(ok),
				Detail: "present in the public log"},
		},
	}
}

func TestPDFVerdictFollowsTheResult(t *testing.T) {
	if got := buildPDFDoc(sampleReport(true, true), "deadbeef").Verdict; got != "VERIFIED" {
		t.Errorf("a passing report rendered verdict %q", got)
	}
	d := buildPDFDoc(sampleReport(false, true), "deadbeef")
	if d.Verdict != "FAILED" || !d.Failed {
		t.Errorf("a failing report rendered verdict %q (failed=%v)", d.Verdict, d.Failed)
	}
}

// A chain nobody could check must render INCOMPLETE, never FAILED.
//
// This is the document an auditor keeps, so getting it wrong here is
// worse than getting it wrong in a terminal: a PDF saying FAILED over a
// record that is merely uncheckable accuses Mesedi in writing of
// something it is not known to have done. Every checkpoint anchored
// before 2026-09-04 is permanently in this state, so this is not a
// hypothetical path.
func TestPDFRendersAnUncheckableChainAsIncompleteNotFailed(t *testing.T) {
	rep := sampleReport(true, true)
	rep.OK = false // the roll-up refuses to call an unchecked chain OK
	rep.LogEntries = []LogEntryCheck{{
		Seq: 1, LogIndex: "10000001", Status: StatusUnverifiable,
		Detail: "no leaf preimage; cannot be tied to its log entry",
	}}

	d := buildPDFDoc(rep, "deadbeef")
	if d.Verdict == "FAILED" || d.Failed {
		t.Fatal("a chain that merely could not be checked was rendered as FAILED, " +
			"which accuses the record in writing of tampering it is not known to have done")
	}
	if d.Verdict != "INCOMPLETE" {
		t.Errorf("verdict = %q, want INCOMPLETE", d.Verdict)
	}
	for _, want := range []string{"could not be checked", "as a verified record"} {
		if !strings.Contains(d.VerdictNote, want) {
			t.Errorf("the note should contain %q, got: %s", want, d.VerdictNote)
		}
	}
}

// A real failure must still win over an unchecked one. An export with
// both a tampered checkpoint and an uncheckable one is FAILED, because
// the finding is the thing that matters.
func TestPDFRealFailureOutranksIncomplete(t *testing.T) {
	rep := sampleReport(true, true)
	rep.OK = false
	rep.LogEntries = []LogEntryCheck{
		{Seq: 1, Status: StatusUnverifiable, Detail: "cannot be checked"},
		{Seq: 2, Status: StatusFailed, Detail: "the published record and the record you were given are not the same"},
	}

	if d := buildPDFDoc(rep, "deadbeef"); d.Verdict != "FAILED" || !d.Failed {
		t.Errorf("an export containing a real failure rendered %q; a finding must "+
			"outrank an unchecked entry", d.Verdict)
	}
}

// The caveats are the part a vendor would be tempted to soften on the way
// into a printable document. They must arrive byte-identical.
func TestPDFCarriesTheCaveatsVerbatim(t *testing.T) {
	rep := sampleReport(true, true)
	d := buildPDFDoc(rep, rep.ExportSHA256)

	if len(d.Limits) != len(rep.Structural.Unverified) {
		t.Fatalf("report stated %d limits, the PDF carries %d",
			len(rep.Structural.Unverified), len(d.Limits))
	}
	for i, want := range rep.Structural.Unverified {
		if d.Limits[i] != want {
			t.Errorf("limit %d was reworded on its way into the PDF:\n got %q\nwant %q",
				i, d.Limits[i], want)
		}
	}
}

// A passing verdict must not read as an endorsement of the AI's output.
// This is the sentence a procurement officer will quote, so it is worth
// an assertion rather than a hope.
func TestPDFPassingVerdictDoesNotClaimCorrectness(t *testing.T) {
	note := buildPDFDoc(sampleReport(true, true), "deadbeef").VerdictNote
	if !strings.Contains(note, "does not say the AI was right") {
		t.Errorf("a passing verdict should distinguish intact from correct, got: %s", note)
	}
}

// An offline run that confirmed NOTHING proves substantially less, and
// the PDF must say so. If it said the same thing either way, --offline
// would be a way to get a clean-looking document without doing the work
// that makes it evidence.
//
// Narrowed on 2026-09-05. This used to assert that ANY offline run
// disclaimed the log, which was correct until offline runs began
// checking inclusion proofs and then became actively false: a run that
// proved every checkpoint against Sigstore's signature printed "this run
// does not show the record was ever published. It is a structural check,
// not evidence." Understating a real verification is not the safe
// direction, it is the line a procurement officer quotes.
//
// So the disclaimer is now tied to nothing having been confirmed, which
// is what it was always trying to express.
func TestPDFSaysSoWhenNothingWasCheckedAgainstTheLog(t *testing.T) {
	// No log entries at all, the state an offline run reached before it
	// could check inclusion proofs, and still the state when a run
	// resolves nothing. Entries marked UNVERIFIABLE are a different case
	// and get the stronger INCOMPLETE verdict, tested separately.
	rep := sampleReport(true, false)
	rep.LogEntries = nil
	d := buildPDFDoc(rep, "deadbeef")

	if !strings.Contains(d.VerdictNote, "not evidence") {
		t.Errorf("a run that confirmed nothing should refuse the word evidence, got: %s",
			d.VerdictNote)
	}
	if !strings.Contains(d.VerdictNote, "consistency check") {
		t.Errorf("a run that confirmed nothing should name itself a consistency check, got: %s",
			d.VerdictNote)
	}
	var found bool
	for _, f := range d.Fields {
		if f[0] == "Transparency log" {
			found = true
			if !strings.Contains(f[1], "not queried") {
				t.Errorf("transparency log field reads %q when nothing was checked", f[1])
			}
		}
	}
	if !found {
		t.Error("the subject block does not state whether the log was consulted")
	}
}

// The inverse, and the one that was wrong in production: an offline run
// whose checkpoints carried proofs verified them cryptographically, and
// the document must say that rather than disclaiming it.
func TestPDFCreditsAnOfflineRunThatProvedEveryCheckpoint(t *testing.T) {
	rep := sampleReport(true, false) // offline
	for i := range rep.LogEntries {
		rep.LogEntries[i].Status = StatusVerified
		rep.LogEntries[i].Method = MethodOfflineProof
	}
	d := buildPDFDoc(rep, "deadbeef")

	if strings.Contains(d.VerdictNote, "not evidence") {
		t.Errorf("every checkpoint was proven against Sigstore's signed tree head and "+
			"the verdict still calls the run 'not evidence'. Understating a real "+
			"verification is not the safe direction; this is the sentence a "+
			"procurement officer quotes:\n%s", d.VerdictNote)
	}
	if !strings.Contains(d.VerdictNote, "proven present") {
		t.Errorf("the verdict does not say the checkpoints were proven present:\n%s",
			d.VerdictNote)
	}
	if !strings.Contains(d.VerdictNote, "without contacting Sigstore") {
		t.Errorf("the verdict does not say this held without contacting Sigstore:\n%s",
			d.VerdictNote)
	}
	// And the section it used to suppress entirely on an offline run.
	var sawLogSection bool
	for _, s := range d.Sections {
		if strings.Contains(s.Title, "TRANSPARENCY LOG") {
			sawLogSection = true
		}
	}
	if !sawLogSection {
		t.Error("the PDF omitted the per-checkpoint transparency-log section on an " +
			"offline run, so the copy handed to an auditor drops every verdict it " +
			"is supposed to carry")
	}
}

// Without the export hash the document is a verdict about nothing in
// particular, and could be shown next to a different export entirely.
func TestPDFBindsItselfToTheExportItVerified(t *testing.T) {
	const sum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	d := buildPDFDoc(sampleReport(true, true), sum)

	var inFields bool
	for _, f := range d.Fields {
		if f[0] == "Export SHA-256" && f[1] == sum {
			inFields = true
		}
	}
	if !inFields {
		t.Error("the full export sha256 is not in the subject block")
	}
	if !strings.Contains(d.FooterID, shorten(sum)) {
		t.Errorf("the footer does not carry the export hash, got: %s", d.FooterID)
	}
}

// A section with no rows must explain itself. Printing an empty heading
// invites the reader to conclude there was nothing to find, when it may
// mean nothing was looked at.
func TestPDFEmptySectionsExplainThemselves(t *testing.T) {
	rep := sampleReport(true, true)
	rep.Structural.Checks = nil
	rep.LogEntries = nil

	for _, s := range buildPDFDoc(rep, "deadbeef").Sections {
		if len(s.Rows) != 0 {
			continue
		}
		if strings.TrimSpace(s.Empty) == "" {
			t.Errorf("section %q would print as a bare heading with no rows", s.Title)
		}
	}
}

func TestRenderPDFProducesAReadableFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		rep  report
	}{
		{"passing online", sampleReport(true, true)},
		{"failing online", sampleReport(false, true)},
		{"passing offline", sampleReport(true, false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := renderPDF(buildPDFDoc(tc.rep, tc.rep.ExportSHA256))
			if err != nil {
				t.Fatalf("renderPDF: %v", err)
			}
			if !bytes.HasPrefix(out, []byte("%PDF-")) {
				t.Error("output is not a PDF")
			}
			if !bytes.Contains(out, []byte("%%EOF")) {
				t.Error("the PDF was not closed out; it would open as corrupt")
			}
			if len(out) < 1000 {
				t.Errorf("PDF is %d bytes, which is too small to contain the report", len(out))
			}
		})
	}
}

// Long single-token details and empty strings are the two shapes most
// likely to break a fixed-width layout: one cannot be wrapped, the other
// has no lines to measure. Both come from real fields, a sha256 is a
// 64-character unbreakable token, and a Detail can be empty.
func TestRenderPDFSurvivesAwkwardContent(t *testing.T) {
	rep := sampleReport(false, true)
	rep.Structural.Checks = []attest.CheckResult{
		{Name: "unbreakable", OK: false, Detail: strings.Repeat("f", 400)},
		{Name: "", OK: false, Detail: ""},
		{Name: strings.Repeat("long name ", 12), OK: true, Detail: "ok"},
	}
	// Enough rows to force pagination, which is where row-splitting bugs
	// show up.
	for i := 0; i < 60; i++ {
		rep.Structural.Checks = append(rep.Structural.Checks, attest.CheckResult{
			Name: "interval", OK: true, Detail: strings.Repeat("wrapping text ", 10),
		})
	}

	out, err := renderPDF(buildPDFDoc(rep, rep.ExportSHA256))
	if err != nil {
		t.Fatalf("renderPDF: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("%PDF-")) {
		t.Fatal("output is not a PDF")
	}
}
