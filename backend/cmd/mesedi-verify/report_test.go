package main

import (
	"bytes"
	"strings"
	"testing"
)

// These three tests come from one run against production. Every unit
// test passed, the cryptography was correct, checkpoint 24 verified
// offline exactly as designed — and the report printed around that
// correct result contained three false statements.
//
// The lesson is not about wording. It is that the report's PROSE was
// tested and its STRUCTURE was not: the per-checkpoint section had tests
// for what it contained and none for whether it was printed at all, so a
// stale `if rep.Online` gate suppressed the whole section on an offline
// run while a caveat elsewhere told the reader to read it.

func offlineReportWithMixedResults() report {
	return report{
		Format:    "mesedi.chain-export.v1",
		ProjectID: "proj_test",
		Online:    false,
		LogEntries: []LogEntryCheck{
			{Seq: 23, LogIndex: "2725077059", Status: StatusUnverifiable,
				Detail: "this run did not contact the log, and this checkpoint carries " +
					"no usable inclusion proof"},
			{Seq: 24, LogIndex: "2725800899", Status: StatusVerified,
				Method: MethodOfflineProof,
				Detail: "the leaf committing to this checkpoint is proven present"},
		},
	}
}

// The gate bug itself. An offline run now produces real per-checkpoint
// verdicts, so suppressing the section hides the only place the reasons
// are individually true.
func TestReportPrintsPerCheckpointResultsOnAnOfflineRun(t *testing.T) {
	var buf bytes.Buffer
	printReport(&buf, offlineReportWithMixedResults())
	got := buf.String()

	if !strings.Contains(got, "PUBLIC TRANSPARENCY LOG") {
		t.Fatal("an offline run with per-checkpoint results printed no results " +
			"section; the caveats refer the reader to reasons that are not on the page")
	}
	for _, want := range []string{"checkpoint 23", "checkpoint 24", "2725800899"} {
		if !strings.Contains(got, want) {
			t.Errorf("the results section omits %q:\n%s", want, got)
		}
	}
}

// A report that says the reasons are given per checkpoint must actually
// give them. The two are one claim, and only one of them was tested.
func TestReportDoesNotPointAtASectionItDidNotPrint(t *testing.T) {
	var buf bytes.Buffer
	rep := offlineReportWithMixedResults()
	rep.Structural.Unverified = []string{
		"Checkpoints [23] could NOT be checked. The reason differs per checkpoint " +
			"and is given against each one in the PUBLIC TRANSPARENCY LOG section.",
	}
	printReport(&buf, rep)
	got := buf.String()

	if strings.Contains(got, "in the PUBLIC TRANSPARENCY LOG section") &&
		!strings.Contains(got, "\nPUBLIC TRANSPARENCY LOG") {
		t.Error("the report directs the reader to a section it did not print")
	}
}

// The caveat must not name a cause it cannot know. It once asserted the
// missing leaf preimage, which was the only possible cause when written
// and is now one of at least three — so on this run it stated, with
// confidence, a reason that was false for all six checkpoints.
func TestUnverifiableCaveatDoesNotAssertACauseItCannotKnow(t *testing.T) {
	var buf bytes.Buffer
	rep := offlineReportWithMixedResults()
	rep.Structural.Unverified = []string{
		"Checkpoints [23] could NOT be checked. That is not a finding against the " +
			"record. The reason differs per checkpoint and is given against each one " +
			"in the PUBLIC TRANSPARENCY LOG section.",
	}
	printReport(&buf, rep)
	got := buf.String()

	for _, forbidden := range []string{
		"The usual cause",
		"anchored before the leaf preimage was retained",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("the report asserts %q as the cause. It is one of several, and "+
				"on an offline run against checkpoints that DO have preimages it is "+
				"simply wrong:\n%s", forbidden, got)
		}
	}
}
