package main

import (
	"strings"
	"testing"
)

// The caveat that replaces the library's offline warning is the sentence
// a reader is most likely to take as the report's headline claim. An
// earlier version substituted "Each checkpoint hash was confirmed
// PRESENT in the public log" on every online run regardless of how many
// entries had actually been resolved, so a run against production —
// where nothing could be checked at all — printed a claim of
// verification a few lines above a RESULT saying the opposite.
//
// A false claim of verification inside the section whose whole job is to
// prevent overclaiming is the worst place in the report to have one.
// These tests exist so it cannot come back.

// entriesFor builds a slate of checks: `verified` of `total` passing,
// of which `offlineProved` were settled by an inclusion proof rather
// than a network lookup. The rest fail, which is enough for the counts;
// the caveat does not distinguish failed from unverifiable.
func entriesFor(verified, total, offlineProved int) []LogEntryCheck {
	out := make([]LogEntryCheck, 0, total)
	for i := 0; i < total; i++ {
		e := LogEntryCheck{Seq: uint64(i)}
		switch {
		case i < offlineProved:
			e.Status, e.Method = StatusVerified, MethodOfflineProof
		case i < verified:
			e.Status, e.Method = StatusVerified, MethodLogLookup
		default:
			e.Status = StatusFailed
		}
		out = append(out, e)
	}
	return out
}

func TestCaveatNeverClaimsVerificationThatDidNotHappen(t *testing.T) {
	cases := []struct {
		name              string
		verified, total   int
		mustNotContain    []string
		mustContain       []string
		allowsTreeHeadPar bool
	}{
		{
			name:     "nothing could be checked",
			verified: 0, total: 8,
			mustNotContain: []string{"confirmed PRESENT"},
			mustContain: []string{
				"NONE of the 8", "not as evidence",
			},
		},
		{
			name:     "some checked, some not",
			verified: 3, total: 8,
			mustContain:       []string{"3 of 8", "The remaining 5"},
			allowsTreeHeadPar: true,
		},
		{
			name:     "everything checked",
			verified: 8, total: 8,
			mustContain:       []string{"All 8", "confirmed PRESENT"},
			allowsTreeHeadPar: true,
		},
		{
			name:     "no checkpoints at all",
			verified: 0, total: 0,
			mustNotContain: []string{"confirmed PRESENT"},
			mustContain:    []string{"no checkpoints to resolve"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := logConfirmation(entriesFor(tc.verified, tc.total, 0)) + " " +
				logResolutionCaveat(entriesFor(tc.verified, tc.total, 0), false)
			for _, s := range tc.mustNotContain {
				if strings.Contains(got, s) {
					t.Errorf("caveat claims %q when %d of %d were verified:\n%s",
						s, tc.verified, tc.total, got)
				}
			}
			for _, s := range tc.mustContain {
				if !strings.Contains(got, s) {
					t.Errorf("caveat should contain %q, got:\n%s", s, got)
				}
			}

			// "What was not ADDITIONALLY checked" presumes a baseline. It
			// must not appear when nothing was checked at all, because it
			// invites the reader to infer verification that never happened.
			hasTreeHead := strings.Contains(got, "additionally checked")
			if hasTreeHead && !tc.allowsTreeHeadPar {
				t.Errorf("the tree-head paragraph implies a baseline of verified "+
					"entries, but %d of %d were verified:\n%s",
					tc.verified, tc.total, got)
			}
		})
	}
}

// Whatever the counts, the caveat must never be empty: it replaces a
// standing warning, and replacing a warning with nothing is how a limit
// silently disappears from a report.
func TestCaveatIsNeverEmpty(t *testing.T) {
	for _, c := range [][3]int{
		{0, 0, 0}, {0, 1, 0}, {1, 1, 0}, {1, 5, 0}, {5, 5, 0},
		{1, 1, 1}, {5, 5, 5}, {3, 5, 2}, {5, 5, 2},
	} {
		for _, offline := range []bool{false, true} {
			got := logResolutionCaveat(entriesFor(c[0], c[1], c[2]), offline)
			if strings.TrimSpace(got) == "" {
				t.Errorf("logResolutionCaveat(%d of %d, %d offline, offline=%v) returned "+
					"nothing, silently removing a stated limit from the report",
					c[0], c[1], c[2], offline)
			}
		}
	}
}

// The tree-head sentence is a claim about what was NOT checked, and for
// an entry settled by an inclusion proof it is false: verifying
// Sigstore's signature over the tree head is exactly what that path
// does. Printing it anyway would be the same defect as the caveat that
// once claimed entries were confirmed when none were, only inverted —
// understating rather than overstating, and still wrong.
func TestCaveatDoesNotDenyCheckingTheTreeHeadWhenItCheckedTheTreeHead(t *testing.T) {
	all := logConfirmation(entriesFor(4, 4, 4))
	if strings.Contains(all, "was not additionally checked") {
		t.Errorf("every checkpoint was proven with an inclusion proof, which verifies "+
			"the signed tree head, yet the caveat says the tree head was not "+
			"checked:\n%s", all)
	}
	if !strings.Contains(all, "signed tree head") {
		t.Errorf("the caveat does not mention that the tree head WAS checked:\n%s", all)
	}
	if !strings.Contains(all, "retired") {
		t.Errorf("the caveat omits that an offline proof survives the log being "+
			"retired, which is the practical reason it is stronger:\n%s", all)
	}

	// Mixed: the clause names which entries had the tree head checked
	// and which did not, so the BLANKET denial must not also appear.
	// Printing both made the caveat contradict itself one sentence after
	// drawing the distinction.
	mixed := logConfirmation(entriesFor(4, 4, 2))
	if !strings.Contains(mixed, "2 of those were") {
		t.Errorf("the caveat does not say how many were proven offline:\n%s", mixed)
	}
	if !strings.Contains(mixed, "The rest were confirmed by asking the log, which does not") {
		t.Errorf("the caveat does not say the lookup half lacks the tree-head "+
			"check:\n%s", mixed)
	}
	if strings.Contains(mixed, "What was not additionally checked is") {
		t.Errorf("the caveat draws the distinction and then denies it wholesale one "+
			"sentence later:\n%s", mixed)
	}
	// The explanation of why the tree head matters is a LIMITATION, so it
	// belongs to the caveat rather than the confirmation. It must survive
	// in every branch of that half.
	for _, off := range [][3]int{{4, 4, 2}, {4, 4, 0}, {4, 4, 4}} {
		c := logResolutionCaveat(entriesFor(off[0], off[1], off[2]), false)
		if !strings.Contains(c, "guards against Sigstore itself being dishonest") {
			t.Errorf("the caveat for %d/%d (%d offline) dropped the explanation of why "+
				"the tree head matters:\n%s", off[0], off[1], off[2], c)
		}
	}

	// Singular must read as English. "1 of those were proven" shipped.
	one := logConfirmation(entriesFor(7, 7, 1))
	if strings.Contains(one, "1 of those were") {
		t.Errorf("singular case reads \"1 of those were\":\n%s", one)
	}
	if !strings.Contains(one, "One of those was") {
		t.Errorf("singular case is not phrased for one entry:\n%s", one)
	}

	// None proven offline: the original denial, unchanged.
	none := logResolutionCaveat(entriesFor(4, 4, 0), false)
	if !strings.Contains(none, "was not additionally checked") {
		t.Errorf("nothing was proven offline, so the tree-head denial must stand:\n%s",
			none)
	}

	// All proven offline: denying the tree head would be false, so the
	// caveat states the honest NEXT limit instead — an inclusion proof
	// binds an entry to one signed tree head and says nothing about
	// whether the log has only ever been appended to across tree heads.
	// Leaving no caveat at all would be worse than the old false one.
	allOff := logResolutionCaveat(entriesFor(4, 4, 4), true)
	if strings.Contains(allOff, "was not additionally checked") {
		t.Errorf("the tree head WAS checked for every entry, yet the caveat still "+
			"denies it:\n%s", allOff)
	}
	if !strings.Contains(allOff, "not its history") {
		t.Errorf("with the tree head checked, the caveat must name the remaining "+
			"limit — that one signed tree head says nothing about the log's "+
			"history:\n%s", allOff)
	}
}

// "Each was checked against Sigstore's own signed tree head" was true
// and unreadable. It landed one sentence after "The remaining 6 were
// not", where "Each" takes the whole export as its antecedent rather
// than the one confirmed checkpoint. A sentence that is true as written
// and false as read is false, and this section is the worst place in the
// report to have one.
func TestCaveatNamesHowManyItIsTalkingAbout(t *testing.T) {
	one := logConfirmation(entriesFor(1, 7, 1))
	if strings.Contains(one, "Each was checked") {
		t.Errorf("with 1 of 7 confirmed, the caveat says \"Each was checked\" directly "+
			"after saying six were not:\n%s", one)
	}
	if !strings.Contains(one, "That one was") {
		t.Errorf("the caveat does not name its antecedent; a reader cannot tell "+
			"whether it covers one checkpoint or all seven:\n%s", one)
	}

	many := logConfirmation(entriesFor(4, 7, 4))
	if !strings.Contains(many, "Those 4 were") {
		t.Errorf("with 4 of 7 confirmed, the caveat does not say how many it "+
			"describes:\n%s", many)
	}
}

// An offline run that confirmed nothing must say so, and must not
// borrow the online wording, which asserts the log was contacted.
func TestCaveatDoesNotClaimTheLogWasContactedOnAnOfflineRun(t *testing.T) {
	got := logResolutionCaveat(entriesFor(0, 3, 0), true)
	if strings.Contains(got, "log was contacted") {
		t.Errorf("an offline run claims the transparency log was contacted:\n%s", got)
	}
	if !strings.Contains(got, "No network was used") {
		t.Errorf("an offline run does not say the network was unused:\n%s", got)
	}
}
