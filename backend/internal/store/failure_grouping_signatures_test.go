package store

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// The group id is the join key between a failure and every later
// occurrence of the same failure; if its derivation drifts, existing
// groups silently stop accumulating and every recurrence opens a new
// group. Pin the exact construction, not just determinism.
func TestDeriveFailureGroupIDPinsTheExactConstruction(t *testing.T) {
	got := DeriveFailureGroupID("proj-1", "crash", "sig-abc")
	h := sha256.Sum256([]byte("proj-1|crash|sig-abc"))
	want := "grp-" + hex.EncodeToString(h[:8])
	if got != want {
		t.Fatalf("DeriveFailureGroupID = %q, want %q", got, want)
	}
	if got == DeriveFailureGroupID("proj-1", "crash", "sig-DIFFERENT") {
		t.Fatal("different signatures produced the same group id")
	}
	if got != deriveGroupID("proj-1", "crash", "sig-abc") {
		t.Fatal("exported and internal derivations disagree")
	}
}

// The bucket boundaries ARE the grouping behavior: a run of 9s and a
// run of 59s must land in the same group, 59s and 61s must not. Walk
// every boundary from both sides.
func TestTimeBudgetSignatureBucketBoundaries(t *testing.T) {
	cases := map[int64]string{
		1_000:     "time_budget_1s+",
		9_999:     "time_budget_1s+",
		10_000:    "time_budget_10s+",
		59_999:    "time_budget_10s+",
		60_000:    "time_budget_60s+",
		599_999:   "time_budget_60s+",
		600_000:   "time_budget_10m+",
		3_599_999: "time_budget_10m+",
		3_600_000: "time_budget_1h+",
	}
	for ms, want := range cases {
		if got := TimeBudgetSignature(ms); got != want {
			t.Errorf("TimeBudgetSignature(%d) = %q, want %q", ms, got, want)
		}
	}
}

func TestStepCountSignatureBucketBoundaries(t *testing.T) {
	cases := map[int]string{
		10:    "step_count_10+",
		49:    "step_count_10+",
		50:    "step_count_50+",
		99:    "step_count_50+",
		100:   "step_count_100+",
		499:   "step_count_100+",
		500:   "step_count_500+",
		4_999: "step_count_500+",
		5_000: "step_count_5000+",
	}
	for n, want := range cases {
		if got := StepCountSignature(n); got != want {
			t.Errorf("StepCountSignature(%d) = %q, want %q", n, got, want)
		}
	}
}
