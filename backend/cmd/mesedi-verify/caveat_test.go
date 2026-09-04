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
			got := logResolutionCaveat(tc.verified, tc.total)
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
	for _, c := range [][2]int{{0, 0}, {0, 1}, {1, 1}, {1, 5}, {5, 5}} {
		if strings.TrimSpace(logResolutionCaveat(c[0], c[1])) == "" {
			t.Errorf("logResolutionCaveat(%d, %d) returned nothing, silently removing "+
				"a stated limit from the report", c[0], c[1])
		}
	}
}
