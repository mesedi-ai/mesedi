package api

// Unit tests for the detector-thresholds validators registry
// (). Covers:
//   - Registry sanity: every entry has Detector + ThresholdKey +
//     ValueType + Default + Parse populated.
//   - Parse rejects malformed value_json (wrong type, empty, etc).
//   - Parse rejects out-of-bound values per spec.
//   - Tier caps enforced (Hobby strictest, Enterprise loosest).
//   - Lookup + List behavior.

import (
	"strings"
	"testing"
)

func Test_DetectorThresholds_RegistryHasExpectedDetectors(t *testing.T) {
	// Lock the registry contents against accidental scope drift.
	want := []string{
		"semantic_loop:revisit_threshold",
		"token_waste:prefix_window_chars",
		"token_waste:min_repeats",
		"tool_schema_drift:min_history_calls",
		"grounding_failure:mean_floor",
		"drift:lexical_threshold_low",
		"drift:lexical_threshold_medium",
		"drift:lexical_threshold_high",
		"context_overflow:high_pct",
		"context_overflow:critical_pct",
		// loops-thresholds wave: extends primitive to the 4th
		// detector family. Closes loops.G2/G3/G4.
		"loops:step_count_threshold",
		"loops:identical_call_min_repeats",
		"loops:similar_call_distance_threshold",
		"loops:similar_call_min_cluster_size",
		// extensions wave: introduces 'bool' + 'json'
		// ValueTypes. Closes grounding_failure.G3 +
		// cascading_failure.G2/G3 + hitl_rejection_spike.G3.
		"grounding_failure:per_evaluator_floors",
		"cascading_failure:cascade_window_seconds",
		"cascading_failure:exclude_spawn_handoffs",
		"hitl_rejection_spike:measurement_window_minutes",
		// data_leakage.G5 wave: per-project severity-firing policy
		// (closed set; first json-string-slice ValueType usage).
		"data_leakage:severity_policy",
		// context_overflow.G3 wave: per-project custom model
		// windows (first json-int-map ValueType usage). Unlocks
		// detection for Ollama / fine-tuned models.
		"context_overflow:custom_model_windows",
		// hitl_timeout.G4 wave: per-project fire-mode toggle.
		// Second consumer of the json-string-slice helpers from
		// data_leakage.G5.
		"hitl_timeout:fire_modes",
		// — per-project custom_model_pricing override.
		// First non-detector entry in the registry (pricing is read
		// at execution close, not by any specific detector). Rides
		// the same JSON-keyed-map shape as custom_model_windows.
		"pricing:custom_model_pricing",
	}
	for _, key := range want {
		if _, ok := detectorThresholdRegistry[key]; !ok {
			t.Errorf("registry missing expected spec: %s", key)
		}
	}
	if got, w := len(detectorThresholdRegistry), len(want); got != w {
		t.Errorf("registry has %d specs, expected %d", got, w)
	}
}

func Test_DetectorThresholds_AllSpecsPopulated(t *testing.T) {
	// Every registered spec must have all required fields.
	for k, spec := range detectorThresholdRegistry {
		if spec.Detector == "" {
			t.Errorf("%s: Detector empty", k)
		}
		if spec.ThresholdKey == "" {
			t.Errorf("%s: ThresholdKey empty", k)
		}
		if spec.ValueType != "int" && spec.ValueType != "float" &&
			spec.ValueType != "bool" && spec.ValueType != "json" {
			t.Errorf("%s: ValueType=%q, want int|float|bool|json", k, spec.ValueType)
		}
		if spec.Description == "" {
			t.Errorf("%s: Description empty", k)
		}
		if spec.Default == nil {
			t.Errorf("%s: Default nil", k)
		}
		if spec.Parse == nil {
			t.Errorf("%s: Parse nil", k)
		}
	}
}

func Test_DetectorThresholds_LookupKnownAndUnknown(t *testing.T) {
	spec, ok := LookupDetectorThresholdSpec("token_waste", "min_repeats")
	if !ok || spec == nil {
		t.Fatalf("expected to find token_waste.min_repeats")
	}
	if spec.Default != 3 {
		t.Errorf("expected default 3, got %v", spec.Default)
	}
	if _, ok := LookupDetectorThresholdSpec("unknown_detector", "k"); ok {
		t.Errorf("expected miss on unknown detector")
	}
	if _, ok := LookupDetectorThresholdSpec("token_waste", "unknown_key"); ok {
		t.Errorf("expected miss on unknown threshold_key")
	}
}

func Test_DetectorThresholds_ListSortedDeterministic(t *testing.T) {
	specs := ListDetectorThresholdSpecs("")
	if len(specs) == 0 {
		t.Fatalf("expected non-empty list")
	}
	// Output must be sorted by (Detector, ThresholdKey).
	for i := 1; i < len(specs); i++ {
		prev, cur := specs[i-1], specs[i]
		if prev.Detector > cur.Detector {
			t.Errorf("output not sorted by detector: %q before %q",
				prev.Detector, cur.Detector)
		}
		if prev.Detector == cur.Detector && prev.ThresholdKey > cur.ThresholdKey {
			t.Errorf("output not sorted by threshold_key within detector: %q before %q",
				prev.ThresholdKey, cur.ThresholdKey)
		}
	}
	// Filter-by-detector returns only matching entries.
	tw := ListDetectorThresholdSpecs("token_waste")
	if len(tw) != 2 {
		t.Errorf("expected 2 token_waste specs, got %d", len(tw))
	}
	for _, s := range tw {
		if s.Detector != "token_waste" {
			t.Errorf("filter leaked detector %q", s.Detector)
		}
	}
}

func Test_DetectorThresholds_ParseIntHappyPath(t *testing.T) {
	spec, _ := LookupDetectorThresholdSpec("semantic_loop", "revisit_threshold")
	v, err := spec.Parse("5", TierTeam)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 5 {
		t.Errorf("expected 5, got %v", v)
	}
}

func Test_DetectorThresholds_ParseFloatHappyPath(t *testing.T) {
	spec, _ := LookupDetectorThresholdSpec("grounding_failure", "mean_floor")
	v, err := spec.Parse("0.7", TierTeam)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 0.7 {
		t.Errorf("expected 0.7, got %v", v)
	}
}

func Test_DetectorThresholds_RejectsWrongType(t *testing.T) {
	spec, _ := LookupDetectorThresholdSpec("token_waste", "min_repeats")
	if _, err := spec.Parse("\"three\"", TierTeam); err == nil {
		t.Errorf("expected error on string-where-int")
	}
	if _, err := spec.Parse("", TierTeam); err == nil {
		t.Errorf("expected error on empty value")
	}
}

func Test_DetectorThresholds_RejectsOutOfBound(t *testing.T) {
	// semantic_loop.revisit_threshold has bounds [2, 1000].
	spec, _ := LookupDetectorThresholdSpec("semantic_loop", "revisit_threshold")
	if _, err := spec.Parse("1", TierEnterprise); err == nil {
		t.Errorf("expected error: 1 below lower bound 2")
	}
	if _, err := spec.Parse("9999", TierEnterprise); err == nil {
		t.Errorf("expected error: 9999 above upper bound 1000")
	}
}

func Test_DetectorThresholds_PrefixWindowTierCapEnforced(t *testing.T) {
	// Only token_waste.prefix_window_chars is tier-capped (raising
	// it = more CPU per execution-close on the detector hot path).
	// Tier caps:
	//   Hobby      4096
	//   Team       16384
	//   Enterprise 65536
	spec, _ := LookupDetectorThresholdSpec("token_waste", "prefix_window_chars")

	if _, err := spec.Parse("8192", TierHobby); err == nil {
		t.Errorf("expected tier-cap error on Hobby with 8192 (cap 4096)")
	} else if !strings.Contains(err.Error(), "tier cap") {
		t.Errorf("expected tier-cap message, got %q", err.Error())
	}
	if _, err := spec.Parse("8192", TierTeam); err != nil {
		t.Errorf("unexpected error on Team with 8192: %v", err)
	}
	if _, err := spec.Parse("32768", TierEnterprise); err != nil {
		t.Errorf("unexpected error on Enterprise with 32768: %v", err)
	}
	// Unknown / empty tier falls back to strictest (Hobby) cap.
	if _, err := spec.Parse("8192", ""); err == nil {
		t.Errorf("expected tier-cap error on empty tier with 8192")
	}
	// Legacy "pro" tier maps to Team — 8192 should be accepted.
	if _, err := spec.Parse("8192", TierProLegacy); err != nil {
		t.Errorf("legacy pro tier should accept Team-cap values: %v", err)
	}
}

func Test_DetectorThresholds_SensitivityKnobsAreTierAgnostic(t *testing.T) {
	// The 9 non-prefix-window thresholds are pure alerting-sensitivity
	// knobs with no cost asymmetry. They MUST behave identically across
	// every tier (only global bounds enforced). Pins the "tier caps
	// only where economics demand them" doctrine.
	tierAgnostic := []struct {
		detector, key, value string
	}{
		{"semantic_loop", "revisit_threshold", "50"},
		{"token_waste", "min_repeats", "50"},
		{"tool_schema_drift", "min_history_calls", "500"},
		{"grounding_failure", "mean_floor", "0.95"},
		{"drift", "lexical_threshold_low", "0.65"},
		{"drift", "lexical_threshold_medium", "0.75"},
		{"drift", "lexical_threshold_high", "0.85"},
		{"context_overflow", "high_pct", "0.95"},
		{"context_overflow", "critical_pct", "0.99"},
	}
	for _, tc := range tierAgnostic {
		spec, ok := LookupDetectorThresholdSpec(tc.detector, tc.key)
		if !ok {
			t.Errorf("missing spec %s.%s", tc.detector, tc.key)
			continue
		}
		for _, tier := range []string{TierHobby, TierTeam, TierEnterprise, "", "bogus"} {
			if _, err := spec.Parse(tc.value, tier); err != nil {
				t.Errorf("%s.%s rejected %s on tier %q (should be tier-agnostic): %v",
					tc.detector, tc.key, tc.value, tier, err)
			}
		}
	}
}

func Test_DetectorThresholds_DefaultsMatchExistingHardcoded(t *testing.T) {
	// Lock defaults against accidental drift. Each must match the
	// detector's existing hardcoded value so customers who never
	// tune see zero behavior change after B.b ships.
	cases := []struct {
		detector, key string
		want          any
	}{
		{"semantic_loop", "revisit_threshold", 3},
		{"token_waste", "prefix_window_chars", 2048},
		{"token_waste", "min_repeats", 3},
		{"tool_schema_drift", "min_history_calls", 10},
		{"grounding_failure", "mean_floor", 0.5},
		{"drift", "lexical_threshold_low", 0.45},
		{"drift", "lexical_threshold_medium", 0.55},
		{"drift", "lexical_threshold_high", 0.70},
		{"context_overflow", "high_pct", 0.90},
		{"context_overflow", "critical_pct", 1.0},
	}
	for _, tc := range cases {
		spec, ok := LookupDetectorThresholdSpec(tc.detector, tc.key)
		if !ok {
			t.Errorf("missing spec %s.%s", tc.detector, tc.key)
			continue
		}
		if spec.Default != tc.want {
			t.Errorf("%s.%s default = %v, want %v",
				tc.detector, tc.key, spec.Default, tc.want)
		}
	}
}

func Test_DetectorThresholds_TierCapOrderingPreserved(t *testing.T) {
	// Hobby <= Team <= Enterprise for the one tier-capped
	// threshold. Drift guard against accidental cap inversion.
	hobby := tierCapTokenWastePrefixWindow(TierHobby)
	team := tierCapTokenWastePrefixWindow(TierTeam)
	ent := tierCapTokenWastePrefixWindow(TierEnterprise)
	if hobby > team {
		t.Errorf("prefix_window: hobby cap %d > team cap %d", hobby, team)
	}
	if team > ent {
		t.Errorf("prefix_window: team cap %d > enterprise cap %d", team, ent)
	}
}
