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
// for this detector. MeanFloor (Theme B.b) is the global default
// floor used for any evaluator:metric pair not present in
// PerEvaluatorFloors. PerEvaluatorFloors (Theme B extensions wave —
// closes grounding_failure.G3) is the per-(evaluator_id:metric_type)
// override map; lookups fall back to MeanFloor when a key is absent.
// Customers who don't tune see the historical behavior.
type GroundingFailureThresholds struct {
	MeanFloor          float64
	PerEvaluatorFloors map[string]float64
}

// DefaultGroundingFailureThresholds returns the historical hardcoded
// default. Used by legacy call sites and tests. PerEvaluatorFloors
// defaults to nil — equivalent to an empty map; every evaluator
// falls back to MeanFloor.
func DefaultGroundingFailureThresholds() GroundingFailureThresholds {
	return GroundingFailureThresholds{MeanFloor: 0.5}
}

// effectiveFloor returns the floor for the given evaluator:metric
// key — the per-evaluator override if present and in range, else
// the global MeanFloor. Defensive against bad config that escaped
// the validators registry (out-of-range values fall through to
// MeanFloor with no panic).
func (t GroundingFailureThresholds) effectiveFloor(evaluatorMetricKey string) float64 {
	if t.PerEvaluatorFloors != nil {
		if v, ok := t.PerEvaluatorFloors[evaluatorMetricKey]; ok && v >= 0.0 && v <= 1.0 {
			return v
		}
	}
	if t.MeanFloor < 0.0 || t.MeanFloor > 1.0 {
		return 0.5
	}
	return t.MeanFloor
}

// DetectGroundingFailure scans eval_score payloads for grounding
// failures. Preserved verbatim for backward compatibility; the
// production execution-close path uses
// DetectGroundingFailureWithThresholds.
func DetectGroundingFailure(payloads []json.RawMessage) (signature string, detected bool) {
	return DetectGroundingFailureWithThresholds(payloads, DefaultGroundingFailureThresholds())
}

// MaxGroundingFailureMatchesPerExecution caps the per-execution
// emit to defensive 20. Real executions evaluate ~1-5 evaluators;
// 20 leaves headroom without unbounded growth.
const MaxGroundingFailureMatchesPerExecution = 20

// DetectGroundingFailureAllMatchesWithThresholds returns ALL
// distinct (evaluator, metric) failures found in the execution's
// eval_score payloads, up to MaxGroundingFailureMatchesPerExecution.
// Closes grounding_failure.G2: the legacy first-failing-evaluator-
// wins variant loses the multi-judge picture (a RAG pipeline
// failing 3 evaluators surfaces 3 clusters now, was 1).
//
// Combines both legacy paths: explicit pass=false matches AND
// mean-below-floor matches across higher_is_better evaluators.
// Dedup by (evaluator, metric) so a single evaluator with both
// signals fires once.
func DetectGroundingFailureAllMatchesWithThresholds(
	payloads []json.RawMessage,
	t GroundingFailureThresholds,
) []string {
	if len(payloads) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var sigs []string
	add := func(evaluator, metric string) {
		if evaluator == "" {
			return
		}
		key := evaluator + ":" + metric
		if seen[key] {
			return
		}
		seen[key] = true
		sigs = append(sigs, "grounding_failure:"+key)
	}
	// First pass: explicit pass=false events.
	for _, raw := range payloads {
		if len(sigs) >= MaxGroundingFailureMatchesPerExecution {
			return sigs
		}
		var p struct {
			EvaluatorID string `json:"evaluator_id"`
			MetricType  string `json:"metric_type"`
			Passed      bool   `json:"passed"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		if !p.Passed {
			add(p.EvaluatorID, p.MetricType)
		}
	}
	// Second pass: mean-below-floor rollup. Only higher_is_better
	// scores participate; inverse metrics need their own threshold
	// semantics (banked).
	type rollup struct {
		sum   float64
		count int
	}
	per := map[string]*rollup{}
	keyToEval := map[string]string{}
	keyToMetric := map[string]string{}
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
			keyToEval[key] = p.EvaluatorID
			keyToMetric[key] = p.MetricType
		}
		r.sum += p.Score
		r.count++
	}
	// Iterate per map in lexicographic order so result ordering
	// is deterministic across runs (Go map iteration is randomized).
	var orderedKeys []string
	for key := range per {
		orderedKeys = append(orderedKeys, key)
	}
	sortStringsLex(orderedKeys)
	for _, key := range orderedKeys {
		if len(sigs) >= MaxGroundingFailureMatchesPerExecution {
			break
		}
		r := per[key]
		if r.count == 0 {
			continue
		}
		mean := r.sum / float64(r.count)
		// Per-evaluator floor lookup (Theme B extensions wave —
		// closes grounding_failure.G3). Falls back to MeanFloor when
		// no per-evaluator override exists for this key.
		floor := t.effectiveFloor(key)
		if mean >= floor {
			continue
		}
		add(keyToEval[key], keyToMetric[key])
	}
	return sigs
}

// sortStringsLex sorts in lexicographic order. Inlined to avoid
// pulling sort into this file (the package already imports
// sort elsewhere; this is just a tiny local helper for the
// all-matches result determinism).
func sortStringsLex(s []string) {
	// Insertion sort — fast for tiny slices (typical N ≤ 5
	// evaluators per execution).
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// DetectGroundingFailureWithThresholds is the per-project-aware
// variant. Defensive: MeanFloor outside [0.0, 1.0] reverts to the
// 0.5 default (validators registry rejects this at write time).
//
// LEGACY first-match-wins API kept for backward-compat with existing
// tests. The handler now uses
// DetectGroundingFailureAllMatchesWithThresholds per the
// all-matches-recorded wave (grounding_failure.G2).
func DetectGroundingFailureWithThresholds(
	payloads []json.RawMessage,
	t GroundingFailureThresholds,
) (signature string, detected bool) {
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
		// Per-evaluator floor lookup (Theme B extensions wave —
		// closes grounding_failure.G3). Falls back to MeanFloor when
		// no per-evaluator override exists for this key.
		floor := t.effectiveFloor(key)
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
