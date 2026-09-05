// Unit tests for HITL-rejection-spike.
//
// Detector covers two firing variants over a rolling project window:
//
//   - hitl_rejection_spike:rejected (rate >= 40% by default)
//   - hitl_rejection_spike:edited   (rate >= 30% by default)
//
// Rejected priority: when both rates clear their thresholds, the
// rejected signature wins (rejection is a stronger negative verdict
// than an edit). Min-sample-size gate: fewer than 5 HITL executions
// in the window suppresses both signals (early-project noise guard).
package detectors

import (
	"testing"

	"mesedi/backend/internal/store"
)

func Test_DetectHITLRejectionSpike_BelowMinSample(t *testing.T) {
	// Even with 100% rejection rate, < 5 sample size suppresses.
	counts := store.HITLOutcomeCounts{
		TotalExecutionsWithHITL: 4,
		ExecutionsWithRejection: 4,
		ExecutionsWithEdit:      0,
	}
	sig, detected := DetectHITLRejectionSpike(counts, 0, 0, 0)
	if detected {
		t.Errorf("4-sample 100%% rejection should NOT fire (min sample=5), got sig=%q", sig)
	}
}

func Test_DetectHITLRejectionSpike_RejectedAtThreshold(t *testing.T) {
	// 2/5 = 40% exactly == 4000 bp threshold. >= comparison must fire.
	counts := store.HITLOutcomeCounts{
		TotalExecutionsWithHITL: 5,
		ExecutionsWithRejection: 2,
		ExecutionsWithEdit:      0,
	}
	sig, detected := DetectHITLRejectionSpike(counts, 0, 0, 0)
	if !detected {
		t.Fatal("2-of-5 rejected (40%) at threshold should fire")
	}
	if sig != "hitl_rejection_spike:rejected" {
		t.Errorf("expected 'hitl_rejection_spike:rejected', got %q", sig)
	}
}

func Test_DetectHITLRejectionSpike_RejectedBelowEditedAbove(t *testing.T) {
	// 1/5 rejected = 20% (below 40%); 2/5 edited = 40% (above 30%).
	// Edited variant fires.
	counts := store.HITLOutcomeCounts{
		TotalExecutionsWithHITL: 5,
		ExecutionsWithRejection: 1,
		ExecutionsWithEdit:      2,
	}
	sig, detected := DetectHITLRejectionSpike(counts, 0, 0, 0)
	if !detected {
		t.Fatal("edited rate 40% should fire (threshold 30%)")
	}
	if sig != "hitl_rejection_spike:edited" {
		t.Errorf("expected 'hitl_rejection_spike:edited', got %q", sig)
	}
}

func Test_DetectHITLRejectionSpike_RejectedPriorityOverEdited(t *testing.T) {
	// Both rates clear thresholds; rejected wins.
	counts := store.HITLOutcomeCounts{
		TotalExecutionsWithHITL: 10,
		ExecutionsWithRejection: 4, // 40%, at threshold
		ExecutionsWithEdit:      5, // 50%, well above edited threshold
	}
	sig, detected := DetectHITLRejectionSpike(counts, 0, 0, 0)
	if !detected {
		t.Fatal("expected detected=true")
	}
	if sig != "hitl_rejection_spike:rejected" {
		t.Errorf("rejected priority: expected 'hitl_rejection_spike:rejected', got %q", sig)
	}
}

func Test_DetectHITLRejectionSpike_BothBelowThresholds(t *testing.T) {
	counts := store.HITLOutcomeCounts{
		TotalExecutionsWithHITL: 10,
		ExecutionsWithRejection: 1, // 10%
		ExecutionsWithEdit:      2, // 20%, below 30%
	}
	sig, detected := DetectHITLRejectionSpike(counts, 0, 0, 0)
	if detected {
		t.Errorf("both rates below threshold should not fire, got sig=%q", sig)
	}
}

func Test_DetectHITLRejectionSpike_CustomThresholdsOverride(t *testing.T) {
	// 50% rejected, 60% edited; custom thresholds 80%/80% should suppress.
	counts := store.HITLOutcomeCounts{
		TotalExecutionsWithHITL: 10,
		ExecutionsWithRejection: 5,
		ExecutionsWithEdit:      6,
	}
	sig, detected := DetectHITLRejectionSpike(counts, 5, 8000, 8000)
	if detected {
		t.Errorf("custom 80%% thresholds should suppress 50%%/60%% rates, got sig=%q", sig)
	}
}

func Test_DetectHITLRejectionSpike_ZeroArgsUseDefaults(t *testing.T) {
	// Passing 0 for any threshold falls back to the package default.
	counts := store.HITLOutcomeCounts{
		TotalExecutionsWithHITL: 5,
		ExecutionsWithRejection: 2,
		ExecutionsWithEdit:      0,
	}
	sig, detected := DetectHITLRejectionSpike(counts, 0, 0, 0)
	if !detected {
		t.Fatal("zero args should use defaults (min 5, rejected 40%, edited 30%) and fire on 2/5 rejected")
	}
	if sig != "hitl_rejection_spike:rejected" {
		t.Errorf("expected rejected signature, got %q", sig)
	}
}

func Test_HITLRejectionSpikeThresholds_EffectiveWindowMinutes(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"in_range_60", 60, 60},
		{"in_range_min_5", 5, 5},
		{"in_range_max_1440", 1440, 1440},
		{"below_min_reverts", 4, 60},
		{"zero_reverts", 0, 60},
		{"negative_reverts", -1, 60},
		{"above_max_reverts", 1441, 60},
		{"very_large_reverts", 100000, 60},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			thresh := HITLRejectionSpikeThresholds{MeasurementWindowMinutes: tc.in}
			if got := thresh.EffectiveWindowMinutes(); got != tc.want {
				t.Errorf("EffectiveWindowMinutes(in=%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func Test_DefaultHITLRejectionSpikeThresholds_LocksDocumentedDefault(t *testing.T) {
	d := DefaultHITLRejectionSpikeThresholds()
	if d.MeasurementWindowMinutes != 60 {
		t.Errorf("DefaultHITLRejectionSpikeThresholds.MeasurementWindowMinutes = %d, want 60", d.MeasurementWindowMinutes)
	}
}

func Test_HITLRejectionSpikeConstants_LockDocumentedValues(t *testing.T) {
	// Locked constants: changing these requires updating the docstring,
	// the dashboard tile, and the validators registry in lockstep.
	if MinHITLSampleForRejectionSpike != 5 {
		t.Errorf("MinHITLSampleForRejectionSpike = %d, want 5", MinHITLSampleForRejectionSpike)
	}
	if RejectionSpikeRateBp != 4000 {
		t.Errorf("RejectionSpikeRateBp = %d, want 4000 (40%%)", RejectionSpikeRateBp)
	}
	if EditSpikeRateBp != 3000 {
		t.Errorf("EditSpikeRateBp = %d, want 3000 (30%%)", EditSpikeRateBp)
	}
}
