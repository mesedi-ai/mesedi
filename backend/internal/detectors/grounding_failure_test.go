// Unit tests for the grounding_failure detector.
//
// Fire conditions:
//  1. Any payload with passed=false (customer's own verdict, authoritative)
//  2. Mean score below configured floor on higher_is_better evaluators
//
// Two API surfaces:
//   - DetectGroundingFailure(With Thresholds): legacy first-match-wins
//   - DetectGroundingFailureAllMatchesWithThresholds: 2 all-matches
//
// Per-evaluator floor map (extensions / G3) lets customers
// tune per (evaluator, metric) pair; falls back to MeanFloor on
// missing key. Defensive: MeanFloor outside [0.0, 1.0] reverts to 0.5.
package detectors

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

func rawScore(payload map[string]any) json.RawMessage {
	b, _ := json.Marshal(payload)
	return b
}

// ─────────────────────────────────────────────────────────────────────
// DetectGroundingFailure (legacy first-match-wins)
// ─────────────────────────────────────────────────────────────────────

func Test_DetectGroundingFailure_NoPayloads(t *testing.T) {
	sig, detected := DetectGroundingFailure(nil)
	if detected {
		t.Errorf("nil payloads should not fire, got %q", sig)
	}
}

func Test_DetectGroundingFailure_ExplicitPassFalse(t *testing.T) {
	payloads := []json.RawMessage{
		rawScore(map[string]any{
			"evaluator_id": "ragas",
			"metric_type":  "faithfulness",
			"passed":       false,
		}),
	}
	sig, detected := DetectGroundingFailure(payloads)
	if !detected {
		t.Fatal("explicit passed=false should fire")
	}
	if sig != "grounding_failure:ragas:faithfulness" {
		t.Errorf("expected 'grounding_failure:ragas:faithfulness', got %q", sig)
	}
}

func Test_DetectGroundingFailure_ExplicitPassFalseEmptyEvaluatorIgnored(t *testing.T) {
	// passed=false with empty evaluator_id is unactionable, must not fire.
	payloads := []json.RawMessage{
		rawScore(map[string]any{
			"evaluator_id": "",
			"metric_type":  "faithfulness",
			"passed":       false,
		}),
	}
	if _, detected := DetectGroundingFailure(payloads); detected {
		t.Error("passed=false with empty evaluator_id should not fire")
	}
}

func Test_DetectGroundingFailure_MeanBelowFloor(t *testing.T) {
	// All passed=true; but mean score (0.2 + 0.3 + 0.4)/3 = 0.3 < 0.5 floor.
	payloads := []json.RawMessage{
		rawScore(map[string]any{
			"evaluator_id":     "ragas",
			"metric_type":      "faithfulness",
			"passed":           true,
			"score":            0.2,
			"higher_is_better": true,
		}),
		rawScore(map[string]any{
			"evaluator_id":     "ragas",
			"metric_type":      "faithfulness",
			"passed":           true,
			"score":            0.3,
			"higher_is_better": true,
		}),
		rawScore(map[string]any{
			"evaluator_id":     "ragas",
			"metric_type":      "faithfulness",
			"passed":           true,
			"score":            0.4,
			"higher_is_better": true,
		}),
	}
	sig, detected := DetectGroundingFailure(payloads)
	if !detected {
		t.Fatal("mean 0.3 below default floor 0.5 should fire")
	}
	if sig != "grounding_failure:ragas:faithfulness" {
		t.Errorf("expected 'grounding_failure:ragas:faithfulness', got %q", sig)
	}
}

func Test_DetectGroundingFailure_MeanAboveFloor_NoFire(t *testing.T) {
	payloads := []json.RawMessage{
		rawScore(map[string]any{
			"evaluator_id":     "ragas",
			"metric_type":      "faithfulness",
			"passed":           true,
			"score":            0.8,
			"higher_is_better": true,
		}),
	}
	if _, detected := DetectGroundingFailure(payloads); detected {
		t.Error("score 0.8 above floor 0.5 should not fire")
	}
}

func Test_DetectGroundingFailure_HigherIsBetterFalseIgnored(t *testing.T) {
	// Inverse metrics (e.g. hallucination_rate) need their own
	// threshold semantics, should NOT contribute to mean-floor logic.
	payloads := []json.RawMessage{
		rawScore(map[string]any{
			"evaluator_id":     "hhem",
			"metric_type":      "hallucination",
			"passed":           true,
			"score":            0.1, // low = good for inverse metrics
			"higher_is_better": false,
		}),
	}
	if _, detected := DetectGroundingFailure(payloads); detected {
		t.Error("higher_is_better=false payload must not fire on mean-below-floor heuristic")
	}
}

func Test_DetectGroundingFailure_ExplicitPriorityOverMean(t *testing.T) {
	// One explicit failure AND one below-mean situation, explicit wins.
	payloads := []json.RawMessage{
		rawScore(map[string]any{
			"evaluator_id":     "ragas",
			"metric_type":      "faithfulness",
			"passed":           true,
			"score":            0.1, // would trigger mean-below-floor
			"higher_is_better": true,
		}),
		rawScore(map[string]any{
			"evaluator_id": "promptfoo",
			"metric_type":  "factuality",
			"passed":       false,
		}),
	}
	sig, detected := DetectGroundingFailure(payloads)
	if !detected {
		t.Fatal("expected fire")
	}
	if sig != "grounding_failure:promptfoo:factuality" {
		t.Errorf("explicit priority: expected promptfoo:factuality, got %q", sig)
	}
}

// ─────────────────────────────────────────────────────────────────────
// DetectGroundingFailureAllMatchesWithThresholds (2)
// ─────────────────────────────────────────────────────────────────────

func Test_DetectGroundingFailureAllMatches_MultipleEvaluators(t *testing.T) {
	// Three different evaluators failing, all-matches returns all 3
	// signatures (closes G2 vs legacy first-match-wins).
	payloads := []json.RawMessage{
		rawScore(map[string]any{
			"evaluator_id": "ragas",
			"metric_type":  "faithfulness",
			"passed":       false,
		}),
		rawScore(map[string]any{
			"evaluator_id": "promptfoo",
			"metric_type":  "factuality",
			"passed":       false,
		}),
		rawScore(map[string]any{
			"evaluator_id": "hhem",
			"metric_type":  "consistency",
			"passed":       false,
		}),
	}
	sigs := DetectGroundingFailureAllMatchesWithThresholds(payloads, DefaultGroundingFailureThresholds())
	want := []string{
		"grounding_failure:hhem:consistency",
		"grounding_failure:promptfoo:factuality",
		"grounding_failure:ragas:faithfulness",
	}
	sort.Strings(sigs)
	sort.Strings(want)
	if !reflect.DeepEqual(sigs, want) {
		t.Errorf("expected %v, got %v", want, sigs)
	}
}

func Test_DetectGroundingFailureAllMatches_DedupSameEvaluatorMetric(t *testing.T) {
	// Same evaluator:metric flagged by BOTH passed=false AND
	// mean-below-floor must emit ONCE (dedup by key).
	payloads := []json.RawMessage{
		rawScore(map[string]any{
			"evaluator_id":     "ragas",
			"metric_type":      "faithfulness",
			"passed":           false,
			"score":            0.1,
			"higher_is_better": true,
		}),
		rawScore(map[string]any{
			"evaluator_id":     "ragas",
			"metric_type":      "faithfulness",
			"passed":           true,
			"score":            0.2,
			"higher_is_better": true,
		}),
	}
	sigs := DetectGroundingFailureAllMatchesWithThresholds(payloads, DefaultGroundingFailureThresholds())
	want := []string{"grounding_failure:ragas:faithfulness"}
	if !reflect.DeepEqual(sigs, want) {
		t.Errorf("dedup expected, got %v", sigs)
	}
}

func Test_DetectGroundingFailureAllMatches_RespectsMaxCap(t *testing.T) {
	// Confirm MaxGroundingFailureMatchesPerExecution is respected.
	// We generate 25 distinct failing evaluators; result is capped at 20.
	var payloads []json.RawMessage
	for i := 0; i < 25; i++ {
		payloads = append(payloads, rawScore(map[string]any{
			"evaluator_id": "eval_" + string(rune('A'+i)),
			"metric_type":  "metric",
			"passed":       false,
		}))
	}
	sigs := DetectGroundingFailureAllMatchesWithThresholds(payloads, DefaultGroundingFailureThresholds())
	if len(sigs) != MaxGroundingFailureMatchesPerExecution {
		t.Errorf("expected cap at %d, got %d signatures", MaxGroundingFailureMatchesPerExecution, len(sigs))
	}
}

// ─────────────────────────────────────────────────────────────────────
// GroundingFailureThresholds, per-evaluator floor overrides
// ─────────────────────────────────────────────────────────────────────

func Test_GroundingFailureThresholds_PerEvaluatorOverride(t *testing.T) {
	thresh := GroundingFailureThresholds{
		MeanFloor: 0.5,
		PerEvaluatorFloors: map[string]float64{
			"ragas:faithfulness": 0.9, // stricter than default
		},
	}
	payloads := []json.RawMessage{
		rawScore(map[string]any{
			"evaluator_id":     "ragas",
			"metric_type":      "faithfulness",
			"passed":           true,
			"score":            0.7,
			"higher_is_better": true,
		}),
	}
	sig, detected := DetectGroundingFailureWithThresholds(payloads, thresh)
	if !detected {
		t.Fatal("per-evaluator override 0.9 should fire on score 0.7")
	}
	if sig != "grounding_failure:ragas:faithfulness" {
		t.Errorf("got %q", sig)
	}
}

func Test_GroundingFailureThresholds_OutOfRangeMeanFloorReverts(t *testing.T) {
	// MeanFloor outside [0.0, 1.0] must revert to 0.5 default.
	cases := []float64{-0.1, 1.1, 99.0}
	for _, bad := range cases {
		t.Run("bad_floor", func(t *testing.T) {
			thresh := GroundingFailureThresholds{MeanFloor: bad}
			payloads := []json.RawMessage{
				rawScore(map[string]any{
					"evaluator_id":     "ragas",
					"metric_type":      "faithfulness",
					"passed":           true,
					"score":            0.4, // below default 0.5
					"higher_is_better": true,
				}),
			}
			sig, detected := DetectGroundingFailureWithThresholds(payloads, thresh)
			if !detected {
				t.Errorf("bad floor=%f should revert to 0.5; score 0.4 must fire", bad)
			}
			if sig != "grounding_failure:ragas:faithfulness" {
				t.Errorf("got %q", sig)
			}
		})
	}
}

func Test_DefaultGroundingFailureThresholds_LocksDocumentedDefault(t *testing.T) {
	d := DefaultGroundingFailureThresholds()
	if d.MeanFloor != 0.5 {
		t.Errorf("DefaultGroundingFailureThresholds.MeanFloor = %f, want 0.5", d.MeanFloor)
	}
	if d.PerEvaluatorFloors != nil {
		t.Errorf("DefaultGroundingFailureThresholds.PerEvaluatorFloors should default to nil, got %v", d.PerEvaluatorFloors)
	}
}

func Test_MaxGroundingFailureMatchesPerExecution_LocksDocumentedValue(t *testing.T) {
	if MaxGroundingFailureMatchesPerExecution != 20 {
		t.Errorf("MaxGroundingFailureMatchesPerExecution = %d, want 20", MaxGroundingFailureMatchesPerExecution)
	}
}
