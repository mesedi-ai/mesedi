// HITL-rejection-spike detector.
//
// Cross-execution signal that detects agent quality regressions
// via human verdicts. When a high fraction of recent executions
// in a project had their human-intervention responses come back as
// "rejected" (humans saying NO outright) or "edited" (humans
// modifying the output before approving), the agent's behavior
// likely regressed. The customer's HITL operators are the canary.
//
// Two firing variants computed per-window:
//
//	"hitl_rejection_spike:rejected"
//	    fraction of recent HITL executions with at least one
//	    rejected response is above the configured threshold.
//
//	"hitl_rejection_spike:edited"
//	    fraction of recent HITL executions with at least one
//	    edited response is above the configured threshold.
//
// Both variants gate on a minimum sample size so the detector
// does not fire on the first three HITL executions of a brand-new
// project (where random noise can produce 100% rejection by
// accident). The defaults are intentionally conservative to
// minimize false positives at v1; they can be tuned per-project
// in a future iteration.
package detectors

import (
	"mesedi/backend/internal/store"
)

// Defaults applied when the handler calls
// DetectHITLRejectionSpike with the zero-value thresholds.
const (
	// MinHITLSampleForRejectionSpike is the minimum number of
	// distinct executions with at least one human_intervention
	// event in the recent window before the detector is willing
	// to fire. Five HITL executions is the smallest sample that
	// makes a 40% rate ("at least 2 of 5") statistically
	// meaningful in early production.
	MinHITLSampleForRejectionSpike = 5

	// RejectionSpikeRateBp is the rate threshold in basis
	// points (10_000 = 100%). 4_000 = 40%: "two out of five
	// recent HITL runs got rejected" is a strong enough signal
	// to surface without crying wolf on isolated bad runs.
	RejectionSpikeRateBp = 4_000

	// EditSpikeRateBp is the rate threshold for the edit variant.
	// Lower than rejected (3_000 = 30%) because edits are weaker
	// signals than rejections; we want to surface persistent
	// quality drift even when the agent isn't being outright
	// rejected.
	EditSpikeRateBp = 3_000
)

// HITLRejectionSpikeThresholds carries the per-project tunable knobs
// for this detector (extensions wave — closes G3).
// MeasurementWindowMinutes bounds the recency window over which the
// handler aggregates HITL outcomes; default 60 (1h) matches the
// historical hardcoded posture in handlers.go.
type HITLRejectionSpikeThresholds struct {
	MeasurementWindowMinutes int
}

// DefaultHITLRejectionSpikeThresholds returns the historical
// hardcoded window (60 minutes).
func DefaultHITLRejectionSpikeThresholds() HITLRejectionSpikeThresholds {
	return HITLRejectionSpikeThresholds{MeasurementWindowMinutes: 60}
}

// EffectiveWindowMinutes returns the validated window value —
// defensive against bad config that escaped the validators registry.
// Reverts to default 60 on out-of-bounds values.
func (t HITLRejectionSpikeThresholds) EffectiveWindowMinutes() int {
	if t.MeasurementWindowMinutes < 5 || t.MeasurementWindowMinutes > 1440 {
		return 60
	}
	return t.MeasurementWindowMinutes
}

// DetectHITLRejectionSpike consumes the project-window aggregate
// (rejected count, edited count, total HITL count) and reports
// the first firing variant. Rejected wins over edited because a
// rejection is a stronger negative verdict than an edit.
//
// Returns ("", false) when the sample size is below the minimum
// or when no variant exceeds its threshold.
func DetectHITLRejectionSpike(
	counts store.HITLOutcomeCounts,
	minSample, rejectedRateBp, editedRateBp int,
) (signature string, detected bool) {
	if minSample <= 0 {
		minSample = MinHITLSampleForRejectionSpike
	}
	if rejectedRateBp <= 0 {
		rejectedRateBp = RejectionSpikeRateBp
	}
	if editedRateBp <= 0 {
		editedRateBp = EditSpikeRateBp
	}
	if counts.TotalExecutionsWithHITL < minSample {
		return "", false
	}
	// Compute rates in basis points so we stay in integer arithmetic.
	// rate_bp = (numerator * 10_000) / denominator.
	rejectedBp := (counts.ExecutionsWithRejection * 10_000) / counts.TotalExecutionsWithHITL
	editedBp := (counts.ExecutionsWithEdit * 10_000) / counts.TotalExecutionsWithHITL
	if rejectedBp >= rejectedRateBp {
		return "hitl_rejection_spike:rejected", true
	}
	if editedBp >= editedRateBp {
		return "hitl_rejection_spike:edited", true
	}
	return "", false
}
