// Grounding-failure detector (Mesedi #14).
//
// Aggregates eval_score events (shipped in Mesedi #9) per execution
// and fires when the run's evaluator verdicts fall below threshold.
// This is the ingestion-only-becomes-actionable bridge: customers
// run Ragas / Promptfoo / HHEM / a custom judge, emit per-call
// scores via emit_eval_score, and on the back end we cluster the
// runs whose evaluators consistently said "not grounded."
//
// Fire conditions (any of):
//
//  1. Any individual eval_score on the execution has passed=false
//     AND severity-equivalent (we treat the customer's own
//     pass/fail verdict as authoritative; no second-guessing).
//
//  2. Mean score across all eval_score events is below a configured
//     floor AND higher_is_better=true. The default floor is 0.5
//     (a coin-flip); customers can override per-evaluator via
//     payload.threshold once we surface the configuration.
//
// Signature shape: "grounding_failure:<evaluator_id>:<metric_type>"
// so the dashboard clusters per (evaluator, metric). A single
// project running both Ragas faithfulness and Vectara HHEM
// hallucination produces two distinct groups, even if both fire on
// the same executions.
package detectors

import (
	"encoding/json"
	"fmt"
)

// DetectGroundingFailure scans the supplied eval_score payloads on
// an execution and reports the first failing evaluator. Returns
// ("", false) when no payload indicated failure.
//
// First-match priority is: explicit passed=false beats
// score-below-mean-floor. This matches customer intent: the
// evaluator's own verdict outranks our heuristic interpretation
// of its numeric output.
// GroundingFailureThresholds carries the per-project tunable values
// for this detector (Theme B.b). MeanFloor defaults to 0.5 — half
// the evaluator's pass band. Customers who don't tune see the
// historical behavior.
type GroundingFailureThresholds struct {
	MeanFloor float64
}

// DefaultGroundingFailureThresholds returns the historical hardcoded
// default. Used by legacy call sites and tests.
func DefaultGroundingFailureThresholds() GroundingFailureThresholds {
	return GroundingFailureThresholds{MeanFloor: 0.5}
}

// DetectGroundingFailure scans eval_score payloads for grounding
// failures. Preserved verbatim for backward compatibility; the
// production execution-close path uses
// DetectGroundingFailureWithThresholds.
func DetectGroundingFailure(payloads []json.RawMessage) (signature string, detected bool) {
	return DetectGroundingFailureWithThresholds(payloads, DefaultGroundingFailureThresholds())
}

// DetectGroundingFailureWithThresholds is the per-project-aware
// variant. Defensive: MeanFloor outside [0.0, 1.0] reverts to the
// 0.5 default (validators registry rejects this at write time).
func DetectGroundingFailureWithThresholds(
	payloads []json.RawMessage,
	t GroundingFailureThresholds,
) (signature string, detected bool) {
	floor := t.MeanFloor
	if floor < 0.0 || floor > 1.0 {
		floor = 0.5
	}
	if len(payloads) == 0 {
		return "", false
	}
	// First pass: any explicit passed=false. The first failing
	// evaluator wins so the signature is deterministic across
	// rerunsorderings.
	for _, raw := range payloads {
		var p struct {
			EvaluatorID string `json:"evaluator_id"`
			MetricType  string `json:"metric_type"`
			Passed      bool   `json:"passed"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		if !p.Passed && p.EvaluatorID != "" {
			return fmt.Sprintf("grounding_failure:%s:%s", p.EvaluatorID, p.MetricType), true
		}
	}
	// Second pass: mean-below-floor heuristic, grouped per
	// (evaluator, metric) so a single bad evaluator doesn't drag
	// the others down. Only consider higher_is_better=true runs;
	// inverse metrics like hallucination_rate need their own
	// threshold semantics.
	type rollup struct {
		sum   float64
		count int
	}
	per := map[string]*rollup{}
	for _, raw := range payloads {
		var p struct {
			EvaluatorID    string  `json:"evaluator_id"`
			MetricType     string  `json:"metric_type"`
			Score          float64 `json:"score"`
			HigherIsBetter bool    `json:"higher_is_better"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		if !p.HigherIsBetter || p.EvaluatorID == "" {
			continue
		}
		key := p.EvaluatorID + ":" + p.MetricType
		r, ok := per[key]
		if !ok {
			r = &rollup{}
			per[key] = r
		}
		r.sum += p.Score
		r.count++
	}
	// Use lexicographic order over key for deterministic tie-break.
	var firingKey string
	for key, r := range per {
		if r.count == 0 {
			continue
		}
		mean := r.sum / float64(r.count)
		if mean >= floor {
			continue
		}
		if firingKey == "" || key < firingKey {
			firingKey = key
		}
	}
	if firingKey == "" {
		return "", false
	}
	return "grounding_failure:" + firingKey, true
}
