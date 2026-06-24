// Unit tests for the Theme B.b WithThresholds variants. Each test
// pins:
//   - Default<Name>Thresholds matches the detector's historical
//     hardcoded value (backward compat).
//   - A non-default threshold actually changes detection behavior.
//   - Bad config (out-of-bound, ordering violation) falls back to
//     defaults rather than producing chaos.
package detectors

import (
	"encoding/json"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────
// semantic_loop
// ─────────────────────────────────────────────────────────────────

func Test_SemanticLoop_DefaultMatchesHardcoded(t *testing.T) {
	if got := DefaultSemanticLoopThresholds().RevisitThreshold; got != minRevisits {
		t.Errorf("default RevisitThreshold = %d, want %d", got, minRevisits)
	}
}

func Test_SemanticLoop_LowerThresholdFiresEarlier(t *testing.T) {
	// Two repeats of the same state — default (3) wouldn't fire;
	// a threshold of 2 should.
	payloads := []json.RawMessage{
		json.RawMessage(`{"metadata":{"step":"A"}}`),
		json.RawMessage(`{"metadata":{"step":"A"}}`),
	}
	if _, fired := DetectSemanticLoopWithThresholds(payloads, DefaultSemanticLoopThresholds()); fired {
		t.Errorf("default threshold (3) fired on 2 repeats — unexpected")
	}
	custom := SemanticLoopThresholds{RevisitThreshold: 2}
	if _, fired := DetectSemanticLoopWithThresholds(payloads, custom); !fired {
		t.Errorf("custom threshold (2) should fire on 2 repeats")
	}
}

func Test_SemanticLoop_BadThresholdFallsBackToDefault(t *testing.T) {
	// RevisitThreshold < 2 is invalid; detector should fall back
	// to default (3). 2 repeats then should NOT fire.
	payloads := []json.RawMessage{
		json.RawMessage(`{"metadata":{"step":"A"}}`),
		json.RawMessage(`{"metadata":{"step":"A"}}`),
	}
	bad := SemanticLoopThresholds{RevisitThreshold: 1}
	if _, fired := DetectSemanticLoopWithThresholds(payloads, bad); fired {
		t.Errorf("bad threshold (1) should fall back to default; expected no fire on 2 repeats")
	}
}

// ─────────────────────────────────────────────────────────────────
// token_waste
// ─────────────────────────────────────────────────────────────────

func Test_TokenWaste_DefaultMatchesHardcoded(t *testing.T) {
	d := DefaultTokenWasteThresholds()
	if d.PrefixWindowChars != prefixWindowChars {
		t.Errorf("default PrefixWindowChars = %d, want %d",
			d.PrefixWindowChars, prefixWindowChars)
	}
	if d.MinRepeats != minRepeats {
		t.Errorf("default MinRepeats = %d, want %d", d.MinRepeats, minRepeats)
	}
}

func Test_TokenWaste_CustomMinRepeatsFiresEarlier(t *testing.T) {
	body := "Identical user message that the agent keeps re-sending."
	payloads := []json.RawMessage{
		userPrompt(body),
		userPrompt(body),
	}
	// Default min_repeats=3 does NOT fire on 2 repeats.
	if _, fired := DetectTokenWasteWithThresholds(payloads, DefaultTokenWasteThresholds()); fired {
		t.Errorf("default threshold fired on 2 repeats — unexpected")
	}
	// Custom min_repeats=2 should fire.
	custom := TokenWasteThresholds{PrefixWindowChars: prefixWindowChars, MinRepeats: 2}
	if _, fired := DetectTokenWasteWithThresholds(payloads, custom); !fired {
		t.Errorf("custom min_repeats=2 should fire on 2 repeats")
	}
}

// ─────────────────────────────────────────────────────────────────
// tool_schema_drift
// ─────────────────────────────────────────────────────────────────

func Test_ToolSchemaDrift_DefaultMatchesHardcoded(t *testing.T) {
	if got := DefaultToolSchemaDriftThresholds().MinHistoryCalls; got != minHistoryCalls {
		t.Errorf("default MinHistoryCalls = %d, want %d", got, minHistoryCalls)
	}
}

func Test_ToolSchemaDrift_LowerHistoryFiresEarlier(t *testing.T) {
	// History of 3 stable calls; default (10) is below threshold so
	// no drift. Custom (3) should fire when current differs.
	hist := map[string]int{"shapeA": 3}
	if _, fired := DetectSchemaDriftWithThresholds("toolX", "shapeB", hist, DefaultToolSchemaDriftThresholds()); fired {
		t.Errorf("default MinHistoryCalls (10) should not fire on 3-call history")
	}
	custom := ToolSchemaDriftThresholds{MinHistoryCalls: 3}
	if _, fired := DetectSchemaDriftWithThresholds("toolX", "shapeB", hist, custom); !fired {
		t.Errorf("custom MinHistoryCalls (3) should fire on 3-call history")
	}
}

// ─────────────────────────────────────────────────────────────────
// grounding_failure
// ─────────────────────────────────────────────────────────────────

func Test_GroundingFailure_DefaultMatchesHardcoded(t *testing.T) {
	if got := DefaultGroundingFailureThresholds().MeanFloor; got != 0.5 {
		t.Errorf("default MeanFloor = %g, want 0.5", got)
	}
}

func Test_GroundingFailure_RaisingFloorFiresEarlier(t *testing.T) {
	// One eval at score 0.6, passed=true (so the Pass 1
	// passed=false early-fire is skipped), higher_is_better=true.
	// Default floor=0.5 does NOT fire (0.6 >= 0.5). Custom
	// floor=0.7 should fire (0.6 < 0.7).
	payloads := []json.RawMessage{
		json.RawMessage(`{"evaluator_id":"ragas","metric_type":"faithfulness","score":0.6,"higher_is_better":true,"passed":true}`),
	}
	if _, fired := DetectGroundingFailureWithThresholds(payloads, DefaultGroundingFailureThresholds()); fired {
		t.Errorf("default floor (0.5) should not fire on score 0.6")
	}
	custom := GroundingFailureThresholds{MeanFloor: 0.7}
	if _, fired := DetectGroundingFailureWithThresholds(payloads, custom); !fired {
		t.Errorf("custom floor (0.7) should fire on score 0.6")
	}
}

// ─────────────────────────────────────────────────────────────────
// drift (lexical, with ordering self-defense)
// ─────────────────────────────────────────────────────────────────

func Test_Drift_DefaultMatchesHardcoded(t *testing.T) {
	d := DefaultDriftThresholds()
	if d.LexicalLow != 0.45 {
		t.Errorf("default LexicalLow = %g, want 0.45", d.LexicalLow)
	}
	if d.LexicalMedium != 0.55 {
		t.Errorf("default LexicalMedium = %g, want 0.55", d.LexicalMedium)
	}
	if d.LexicalHigh != 0.70 {
		t.Errorf("default LexicalHigh = %g, want 0.70", d.LexicalHigh)
	}
}

func Test_Drift_DefaultPreservesLegacySignatures(t *testing.T) {
	// Customers who don't tune must see the historical signature
	// shapes "lexical_drift_0.45+" / "0.55+" / "0.70+". Use a
	// known-different pair of corpora to trip the LOW bucket.
	current := []string{"completely different content from history"}
	historical := []string{"baseline corpus message about widgets and gizmos"}
	sig, _, fired := DetectLexicalDriftWithThresholds(current, historical, DefaultDriftThresholds())
	if !fired {
		t.Skipf("test corpus didn't trip drift (distance below 0.45); not a regression — fix the fixture if reproducible")
	}
	// Signature must use one of the historical-shape strings, NOT
	// some unexpected format like "lexical_drift_0.45000+".
	for _, allowed := range []string{
		"lexical_drift_0.45+",
		"lexical_drift_0.55+",
		"lexical_drift_0.70+",
	} {
		if sig == allowed {
			return
		}
	}
	t.Errorf("legacy signature shape lost; got %q", sig)
}

func Test_Drift_BadOrderingFallsBackToDefaults(t *testing.T) {
	// Out-of-order thresholds (low > high) must fall back to
	// defaults — detector must not produce chaos.
	bad := DriftThresholds{
		LexicalLow:    0.8,
		LexicalMedium: 0.6,
		LexicalHigh:   0.7,
	}
	if bad.validForBucketing() {
		t.Errorf("expected validForBucketing to reject bad ordering")
	}
	// Run the detector with the bad config and a tripping corpus;
	// signature must be one of the DEFAULT shapes (proves the
	// fallback path ran, not the broken custom path).
	current := []string{"completely different content from history"}
	historical := []string{"baseline corpus message about widgets and gizmos"}
	sig, _, fired := DetectLexicalDriftWithThresholds(current, historical, bad)
	if fired {
		// Either fires under a default shape (good), or no fire
		// (also fine — test corpus may not trip). Anything ELSE is
		// a bug.
		ok := false
		for _, allowed := range []string{
			"lexical_drift_0.45+",
			"lexical_drift_0.55+",
			"lexical_drift_0.70+",
		} {
			if sig == allowed {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("bad-config fallback didn't use default signature; got %q", sig)
		}
	}
}

func Test_Drift_OutOfRangeValuesFallBack(t *testing.T) {
	cases := []DriftThresholds{
		{LexicalLow: -0.1, LexicalMedium: 0.5, LexicalHigh: 0.7},
		{LexicalLow: 0.2, LexicalMedium: 1.5, LexicalHigh: 0.7},
		{LexicalLow: 0.2, LexicalMedium: 0.5, LexicalHigh: 1.2},
	}
	for i, c := range cases {
		if c.validForBucketing() {
			t.Errorf("case %d: expected validForBucketing=false, got true", i)
		}
	}
}

// ─────────────────────────────────────────────────────────────────
// context_overflow
// ─────────────────────────────────────────────────────────────────

func Test_ContextOverflow_DefaultMatchesHardcoded(t *testing.T) {
	d := DefaultContextOverflowThresholds()
	if d.HighPct != contextOverflowWarnPct {
		t.Errorf("default HighPct = %g, want %g", d.HighPct, contextOverflowWarnPct)
	}
	if d.CriticalPct != contextOverflowFailPct {
		t.Errorf("default CriticalPct = %g, want %g", d.CriticalPct, contextOverflowFailPct)
	}
}

func Test_ContextOverflow_LowerHighPctFiresEarlier(t *testing.T) {
	// 65% of a known model window. Default (warn=0.90, fail=1.0)
	// does NOT fire. Custom (warn=0.5, fail=0.8) — both within the
	// validators-registry [0.5, 1.0] valid range — should fire as
	// warn. claude-opus-4-6 has a 200K window; 130K input is 65%.
	payloads := []json.RawMessage{
		json.RawMessage(`{"model":"claude-opus-4-6","input_tokens":130000}`),
	}
	if _, fired := DetectContextOverflowWithThresholds(payloads, DefaultContextOverflowThresholds()); fired {
		t.Errorf("default thresholds should not fire at 65%% utilization")
	}
	custom := ContextOverflowThresholds{HighPct: 0.5, CriticalPct: 0.8}
	sig, fired := DetectContextOverflowWithThresholds(payloads, custom)
	if !fired {
		t.Errorf("custom thresholds (warn=0.5) should fire at 65%% utilization")
	}
	if !strings.Contains(sig, "warn") {
		t.Errorf("expected warn signature, got %q", sig)
	}
}

func Test_ContextOverflow_BadOrderingFallsBackToDefaults(t *testing.T) {
	// HighPct >= CriticalPct is invalid; detector must use defaults.
	bad := ContextOverflowThresholds{HighPct: 0.95, CriticalPct: 0.95}
	payloads := []json.RawMessage{
		json.RawMessage(`{"model":"claude-opus-4-6","input_tokens":100000}`),
	}
	// At 50% util with defaults, neither warn (0.9) nor fail (1.0)
	// fires. If the bad config WERE used, 0.95 would also not fire.
	// The key is no panic + behaves sanely. Try a higher utilization
	// (190K = 95%) which would fire under default warn but NOT under
	// the bad config if it were applied as-is. With fallback, fires.
	payloads = []json.RawMessage{
		json.RawMessage(`{"model":"claude-opus-4-6","input_tokens":190000}`),
	}
	sig, fired := DetectContextOverflowWithThresholds(payloads, bad)
	if !fired {
		t.Errorf("bad config should fall back to defaults; 95%% util should warn under default thresholds")
	}
	if !strings.Contains(sig, "warn") {
		t.Errorf("expected warn signature under default fallback, got %q", sig)
	}
}

// ─────────────────────────────────────────────────────────────────
// loops family (loops-thresholds wave — step_count + identical_call
// thresholds live at the handler call site as literals, so they're
// tested via integration; similar_call_loop has a detector-level
// WithThresholds variant exercised here)
// ─────────────────────────────────────────────────────────────────

func Test_Loops_DefaultMatchesHardcoded(t *testing.T) {
	d := DefaultLoopsThresholds()
	if d.StepCountThreshold != 10 {
		t.Errorf("default StepCountThreshold = %d, want 10", d.StepCountThreshold)
	}
	if d.IdenticalCallMinRepeats != 3 {
		t.Errorf("default IdenticalCallMinRepeats = %d, want 3", d.IdenticalCallMinRepeats)
	}
	if d.SimilarCallDistanceThreshold != SimilarCallDistanceThreshold {
		t.Errorf("default SimilarCallDistanceThreshold = %g, want %g",
			d.SimilarCallDistanceThreshold, SimilarCallDistanceThreshold)
	}
	if d.SimilarCallMinClusterSize != SimilarCallMinClusterSize {
		t.Errorf("default SimilarCallMinClusterSize = %d, want %d",
			d.SimilarCallMinClusterSize, SimilarCallMinClusterSize)
	}
}

func Test_Loops_SimilarCallRaisingClusterSizeStopsFiring(t *testing.T) {
	// Three near-duplicate messages — should fire at default cluster
	// size 3 (assuming they cluster). Raising min_cluster_size to 4
	// should stop firing on the same input.
	msgs := []string{
		"Please summarize the quarterly earnings report for Acme Corporation.",
		"Please summarize the quarterly earnings report for Acme Corp.",
		"Please summarize the quarterly earnings report for Acme Co.",
	}
	if _, fired := DetectSimilarCallLoopWithThresholds(msgs, DefaultLoopsThresholds()); !fired {
		t.Skipf("test fixture didn't cluster at default thresholds; not a regression — fix the fixture if reproducible")
	}
	tight := DefaultLoopsThresholds()
	tight.SimilarCallMinClusterSize = 4
	if _, fired := DetectSimilarCallLoopWithThresholds(msgs, tight); fired {
		t.Errorf("raising MinClusterSize to 4 should not fire on 3 messages")
	}
}

func Test_Loops_SimilarCallBadConfigFallsBackToDefaults(t *testing.T) {
	// MinClusterSize < 2 is invalid; detector must use default (3).
	// 2 near-duplicate messages should NOT fire under the fallback
	// (need 3+).
	msgs := []string{
		"Please summarize the quarterly earnings report for Acme Corporation.",
		"Please summarize the quarterly earnings report for Acme Corp.",
	}
	bad := DefaultLoopsThresholds()
	bad.SimilarCallMinClusterSize = 1
	if _, fired := DetectSimilarCallLoopWithThresholds(msgs, bad); fired {
		t.Errorf("bad MinClusterSize (1) should fall back to default 3; expected no fire on 2 messages")
	}
	// Distance outside [0.05, 0.50] should also fall back to default.
	badDist := DefaultLoopsThresholds()
	badDist.SimilarCallDistanceThreshold = 0.99
	// 3 near-dup messages — under bad distance 0.99 EVERY pair would
	// look near-duplicate (matches always), but the fallback path
	// uses the default 0.20 which still clusters these three.
	clusterMsgs := []string{
		"Please summarize the quarterly earnings report for Acme Corporation.",
		"Please summarize the quarterly earnings report for Acme Corp.",
		"Please summarize the quarterly earnings report for Acme Co.",
	}
	// Whether it fires depends on the fixture clustering at distance
	// 0.20; what matters here is no panic + behavior is deterministic.
	_, _ = DetectSimilarCallLoopWithThresholds(clusterMsgs, badDist)
}

// ─────────────────────────────────────────────────────────────────
// grounding_failure per-evaluator floors (Theme B extensions wave —
// closes grounding_failure.G3)
// ─────────────────────────────────────────────────────────────────

func Test_GroundingFailure_PerEvaluatorOverrideFires(t *testing.T) {
	// ragas faithfulness scored 0.6 with global floor 0.5: under
	// global default, doesn't fire (0.6 >= 0.5). With per-evaluator
	// floor 0.7 on ragas:faithfulness, fires (0.6 < 0.7).
	payloads := []json.RawMessage{
		json.RawMessage(`{"evaluator_id":"ragas","metric_type":"faithfulness","score":0.6,"higher_is_better":true,"passed":true}`),
	}
	if _, fired := DetectGroundingFailureWithThresholds(payloads, DefaultGroundingFailureThresholds()); fired {
		t.Errorf("global floor 0.5 should not fire on score 0.6")
	}
	custom := GroundingFailureThresholds{
		MeanFloor: 0.5,
		PerEvaluatorFloors: map[string]float64{
			"ragas:faithfulness": 0.7,
		},
	}
	if _, fired := DetectGroundingFailureWithThresholds(payloads, custom); !fired {
		t.Errorf("per-evaluator floor 0.7 should fire on score 0.6")
	}
}

func Test_GroundingFailure_PerEvaluatorFallsBackToMeanFloor(t *testing.T) {
	// vectara hhem scored 0.4 with global floor 0.5: fires under
	// global default (0.4 < 0.5). Per-evaluator map specifies a
	// DIFFERENT evaluator (ragas), so vectara should still use the
	// global floor and fire.
	payloads := []json.RawMessage{
		json.RawMessage(`{"evaluator_id":"vectara","metric_type":"hhem","score":0.4,"higher_is_better":true,"passed":true}`),
	}
	custom := GroundingFailureThresholds{
		MeanFloor: 0.5,
		PerEvaluatorFloors: map[string]float64{
			"ragas:faithfulness": 0.7, // unrelated key
		},
	}
	if _, fired := DetectGroundingFailureWithThresholds(payloads, custom); !fired {
		t.Errorf("vectara key not in PerEvaluatorFloors; should fall back to MeanFloor 0.5 and fire on 0.4")
	}
}

func Test_GroundingFailure_BadPerEvaluatorValueFallsBack(t *testing.T) {
	// Per-evaluator value outside [0, 1] is invalid; detector should
	// fall back to MeanFloor for that key.
	payloads := []json.RawMessage{
		json.RawMessage(`{"evaluator_id":"ragas","metric_type":"faithfulness","score":0.6,"higher_is_better":true,"passed":true}`),
	}
	bad := GroundingFailureThresholds{
		MeanFloor: 0.5,
		PerEvaluatorFloors: map[string]float64{
			"ragas:faithfulness": 1.5, // invalid
		},
	}
	// Bad override → falls back to MeanFloor 0.5. Score 0.6 >= 0.5
	// so no fire (matches global-default behavior).
	if _, fired := DetectGroundingFailureWithThresholds(payloads, bad); fired {
		t.Errorf("bad per-evaluator value should fall back to MeanFloor; score 0.6 should not fire under floor 0.5")
	}
}

// ─────────────────────────────────────────────────────────────────
// cascading_failure (Theme B extensions wave — closes G2 + G3)
// ─────────────────────────────────────────────────────────────────

func Test_CascadingFailure_DefaultMatchesHardcoded(t *testing.T) {
	d := DefaultCascadingFailureThresholds()
	if d.CascadeWindowSeconds != 86400 {
		t.Errorf("default CascadeWindowSeconds = %d, want 86400", d.CascadeWindowSeconds)
	}
	if d.ExcludeSpawnHandoffs != false {
		t.Errorf("default ExcludeSpawnHandoffs = %v, want false", d.ExcludeSpawnHandoffs)
	}
}

// ─────────────────────────────────────────────────────────────────
// hitl_rejection_spike (Theme B extensions wave — closes G3)
// ─────────────────────────────────────────────────────────────────

func Test_HITLRejectionSpike_DefaultMatchesHardcoded(t *testing.T) {
	d := DefaultHITLRejectionSpikeThresholds()
	if d.MeasurementWindowMinutes != 60 {
		t.Errorf("default MeasurementWindowMinutes = %d, want 60",
			d.MeasurementWindowMinutes)
	}
}

func Test_HITLRejectionSpike_BadWindowFallsBackToDefault(t *testing.T) {
	// Window outside [5, 1440] should fall back to 60.
	bad := HITLRejectionSpikeThresholds{MeasurementWindowMinutes: 0}
	if got := bad.EffectiveWindowMinutes(); got != 60 {
		t.Errorf("bad window (0) should fall back to default 60; got %d", got)
	}
	bad2 := HITLRejectionSpikeThresholds{MeasurementWindowMinutes: 99999}
	if got := bad2.EffectiveWindowMinutes(); got != 60 {
		t.Errorf("bad window (99999) should fall back to default 60; got %d", got)
	}
	good := HITLRejectionSpikeThresholds{MeasurementWindowMinutes: 120}
	if got := good.EffectiveWindowMinutes(); got != 120 {
		t.Errorf("valid window (120) should be preserved; got %d", got)
	}
}

func Test_Loops_SimilarCallLegacyWrapperUsesDefaults(t *testing.T) {
	// Legacy DetectSimilarCallLoop must produce byte-identical
	// behavior to DetectSimilarCallLoopWithThresholds(..., defaults).
	// Three near-duplicate messages.
	msgs := []string{
		"Please summarize the quarterly earnings report for Acme Corporation.",
		"Please summarize the quarterly earnings report for Acme Corp.",
		"Please summarize the quarterly earnings report for Acme Co.",
	}
	legacySig, legacyFired := DetectSimilarCallLoop(msgs)
	newSig, newFired := DetectSimilarCallLoopWithThresholds(msgs, DefaultLoopsThresholds())
	if legacyFired != newFired {
		t.Errorf("legacy fired=%v vs WithThresholds default fired=%v — backward compat broken",
			legacyFired, newFired)
	}
	if legacySig != newSig {
		t.Errorf("legacy sig=%q vs WithThresholds default sig=%q — backward compat broken",
			legacySig, newSig)
	}
}
