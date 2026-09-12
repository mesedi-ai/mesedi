package attest

import (
	"strings"
	"testing"
)

// "The chain cannot advance without publishing" was the last major claim
// in the report that rested on trusting us.
//
// The mechanism was always there: every checkpoint carries the
// public-log position of the checkpoint before it, so an hour that was
// never published leaves the next hour with nothing to name. But
// VerifyChain only asserted that field was NON-EMPTY. Any string passed.
// Nothing compared it against where the previous checkpoint was actually
// published, which meant the property was true in our scheduler and
// unverifiable in the artifact a reader holds.
//
// The fixture had the same defect. buildExport minted PrevLogEntryID
// from one number series and LogEntryID from another, so the two never
// matched, and no test noticed for the life of the file. A fixture that
// cannot satisfy the property is a fixture nobody was checking the
// property against.
//
// This check only reaches full strength combined with the transparency
// log check: each position named here is separately proven to hold the
// leaf committing to that specific checkpoint. Alone, a forger could
// mint self-consistent fake positions. Together, hour N+1 names a public
// position, that position really holds hour N, and hour N really holds
// hour N-1.

func publicationCheck(t *testing.T, v ExportVerification) CheckResult {
	t.Helper()
	for _, c := range v.Checks {
		if strings.Contains(c.Name, "publication never skipped") {
			return c
		}
	}
	t.Fatalf("the export verification has no publication check at all. "+
		"Checks present: %v", checkNames(v))
	return CheckResult{}
}

func checkNames(v ExportVerification) []string {
	out := make([]string, 0, len(v.Checks))
	for _, c := range v.Checks {
		out = append(out, c.Name)
	}
	return out
}

// The one that matters. An hour recorded without the hour before it
// being published must not verify clean.
func TestAnHourPublishedNowhereIsCaught(t *testing.T) {
	e := buildExport(t, []int{2, 3, 1})

	// Hour 2 keeps its own valid record, but the hour after it now names
	// a position that is not where hour 2 went. This is what a chain
	// that advanced without publishing looks like from the outside: a
	// plausible number in a field nobody was checking.
	e.Intervals[2].Checkpoint.PrevLogEntryID = "2712467999"
	e.Intervals[2].Checkpoint.Hash = CheckpointHash(e.Intervals[2].Checkpoint)

	c := publicationCheck(t, VerifyChainExport(e))
	if c.OK {
		t.Fatalf("an hour naming the wrong publication position for its predecessor "+
			"passed verification. The claim that the chain cannot advance without "+
			"publishing is not being checked. Detail was: %s", c.Detail)
	}
	if !strings.Contains(c.Detail, "hour 3") {
		t.Errorf("the failure does not say which hour broke the link: %s", c.Detail)
	}
}

// Recomputing the hash above is what makes the previous test honest: a
// tampered checkpoint whose hash no longer matches would be caught by an
// entirely different check, and the test would pass for the wrong
// reason. This asserts the tampered export is otherwise clean, so the
// publication check is doing the work.
func TestTheCaughtHourFailsOnlyThePublicationCheck(t *testing.T) {
	e := buildExport(t, []int{2, 3, 1})
	e.Intervals[2].Checkpoint.PrevLogEntryID = "2712467999"
	e.Intervals[2].Checkpoint.Hash = CheckpointHash(e.Intervals[2].Checkpoint)

	for _, name := range failedChecks(VerifyChainExport(e)) {
		if !strings.Contains(name, "publication never skipped") {
			t.Errorf("a second check also failed, so the publication test above may be "+
				"passing for the wrong reason: %s", name)
		}
	}
}

// A clean export must report the property, not stay silent about it. A
// check that only ever speaks on failure gives a reader no reason to
// believe it ran.
func TestACleanChainSaysPublicationWasNeverSkipped(t *testing.T) {
	c := publicationCheck(t, VerifyChainExport(buildExport(t, []int{1, 2, 3, 4})))
	if !c.OK {
		t.Fatalf("a well-formed export failed the publication check: %s", c.Detail)
	}
	// Four hours, three links between them.
	if !strings.Contains(c.Detail, "3 hours") {
		t.Errorf("the detail does not say how many links were checked, so a reader "+
			"cannot tell whether it checked one or all of them: %s", c.Detail)
	}
}

// A ranged export cannot check its own first element, and must say so
// rather than counting an unchecked link as checked.
func TestARangedExportAdmitsItsFirstLinkIsUnchecked(t *testing.T) {
	e := buildExport(t, []int{1, 2, 3})
	e.Intervals = e.Intervals[1:] // starts at hour 2, not genesis

	c := publicationCheck(t, VerifyChainExport(e))
	if !c.OK {
		t.Fatalf("a legitimate ranged export failed: %s", c.Detail)
	}
	if !strings.Contains(c.Detail, "not checked") {
		t.Errorf("the export starts partway through, so its first link cannot be "+
			"checked, and the report does not say so: %s", c.Detail)
	}
}

// A single hour has no earlier hour to link to. That is a real state,
// not a pass, and must not be reported as though links were verified.
func TestASingleHourDoesNotClaimAVerifiedLink(t *testing.T) {
	c := publicationCheck(t, VerifyChainExport(buildExport(t, []int{5})))
	if !c.OK {
		t.Fatalf("a single-hour export failed the publication check: %s", c.Detail)
	}
	if !strings.Contains(c.Detail, "no earlier hour") {
		t.Errorf("a one-hour export should say there was nothing to link to, got: %s",
			c.Detail)
	}
}

// The fixture regression, guarded directly. If buildExport ever again
// mints these from two different series, every test above passes
// vacuously and this one fails.
func TestTheFixtureActuallyChains(t *testing.T) {
	e := buildExport(t, []int{1, 1, 1})
	for i := 1; i < len(e.Intervals); i++ {
		got := e.Intervals[i].Checkpoint.PrevLogEntryID
		want := e.Intervals[i-1].LogEntryID
		if got != want {
			t.Fatalf("interval %d names %q as its predecessor's publication position, "+
				"but interval %d was published at %q. The fixture cannot satisfy the "+
				"property the tests in this file exist to check", i, got, i-1, want)
		}
	}
}
