// Unit tests for the drift detector.
//
// drift.go ships two distinct drift signals:
//
//   - DetectModelDrift: categorical drift — a model in `current` is
//     not present in `historical`. Returns a "new_model:<id>"
//     signature. Day-one safety: returns false when historical is
//     empty (every model would otherwise look new).
//
//   - DetectLexicalDrift / DetectLexicalDriftWithThresholds: pure
//     character-3-gram cosine-distance drift over user-messages.
//     Returns a bucketed "lexical_drift_<cutoff>+" signature
//     embedding the customer-tuned cutoff (defaults: 0.45 / 0.55 /
//     0.70). DefaultDriftThresholds + DriftThresholds.validForBucketing
//     guard against bad customer-supplied overrides.
//
// These tests are 's drift_test.go coverage gap closeout —
// drift.go had 608 lines of production logic with zero unit tests
// before this commit (integration tests in test_detectors.py only
// covered the happy path via test_lexical_drift). The test cases
// below are deliberately edge-case-heavy: empty corpora, identical
// corpora, disjoint corpora, case-insensitive normalization, exact
// bucket boundaries, invalid thresholds falling back to defaults.
package detectors

import (
	"math"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────
// DetectModelDrift
// ─────────────────────────────────────────────────────────────────────

func Test_DetectModelDrift_EmptyInputs(t *testing.T) {
	// Day-one safety: both empty current and empty historical return
	// (detected=false). Without this guard, the first execution in a
	// project would falsely flag every model as drift.
	cases := []struct {
		name       string
		current    []string
		historical []string
	}{
		{"both_empty", nil, nil},
		{"current_empty", nil, []string{"claude-sonnet-4-6"}},
		{"historical_empty", []string{"claude-sonnet-4-6"}, nil},
		{"current_empty_slice", []string{}, []string{"claude-sonnet-4-6"}},
		{"historical_empty_slice", []string{"claude-sonnet-4-6"}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig, detected := DetectModelDrift(tc.current, tc.historical)
			if detected {
				t.Errorf("expected detected=false, got detected=true with signature=%q", sig)
			}
			if sig != "" {
				t.Errorf("expected empty signature, got %q", sig)
			}
		})
	}
}

func Test_DetectModelDrift_NoNewModels(t *testing.T) {
	// Every current model is in the historical set → no drift.
	sig, detected := DetectModelDrift(
		[]string{"claude-sonnet-4-6", "claude-haiku-4-5"},
		[]string{"claude-sonnet-4-6", "claude-haiku-4-5", "claude-opus-4-6"},
	)
	if detected {
		t.Errorf("expected detected=false, got detected=true with signature=%q", sig)
	}
}

func Test_DetectModelDrift_SingleNewModel(t *testing.T) {
	sig, detected := DetectModelDrift(
		[]string{"claude-sonnet-4-6", "gpt-5"},
		[]string{"claude-sonnet-4-6"},
	)
	if !detected {
		t.Fatal("expected detected=true, got detected=false")
	}
	if sig != "new_model:gpt-5" {
		t.Errorf("expected signature 'new_model:gpt-5', got %q", sig)
	}
}

func Test_DetectModelDrift_AlphabeticalTiebreaker(t *testing.T) {
	// When multiple new models appear, the signature uses the
	// alphabetically-first so the same combination groups
	// deterministically. claude-opus comes before gpt-5 alphabetically.
	sig, detected := DetectModelDrift(
		[]string{"gpt-5", "claude-opus-4-6"},
		[]string{"claude-sonnet-4-6"},
	)
	if !detected {
		t.Fatal("expected detected=true")
	}
	if sig != "new_model:claude-opus-4-6" {
		t.Errorf("expected alphabetically-first new model in signature, got %q", sig)
	}
}

func Test_DetectModelDrift_CaseInsensitive(t *testing.T) {
	// Mixed-case current models should match lowercase historical
	// entries (Anthropic returns mixed-case identifiers; we treat
	// case-equivalent forms as the same model).
	sig, detected := DetectModelDrift(
		[]string{"Claude-Sonnet-4-6"},
		[]string{"claude-sonnet-4-6"},
	)
	if detected {
		t.Errorf("case-insensitive match should yield no drift, got signature=%q", sig)
	}
}

func Test_DetectModelDrift_SkipsEmptyStrings(t *testing.T) {
	// Empty / whitespace-only current entries must be ignored, not
	// flagged as new models (would fire on every execution with a
	// missing model_id field).
	sig, detected := DetectModelDrift(
		[]string{"", "   ", "claude-sonnet-4-6"},
		[]string{"claude-sonnet-4-6"},
	)
	if detected {
		t.Errorf("expected detected=false (only empty strings as 'new'), got signature=%q", sig)
	}
}

// ─────────────────────────────────────────────────────────────────────
// DetectLexicalDrift (default thresholds)
// ─────────────────────────────────────────────────────────────────────

func Test_DetectLexicalDrift_EmptyInputs(t *testing.T) {
	// Day-one safety AND insufficient-data guard: empty corpora
	// return (false, 0) rather than firing.
	cases := []struct {
		name       string
		current    []string
		historical []string
	}{
		{"both_empty", nil, nil},
		{"current_empty", nil, []string{"hello world"}},
		{"historical_empty", []string{"hello world"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig, dist, detected := DetectLexicalDrift(tc.current, tc.historical)
			if detected {
				t.Errorf("expected detected=false, got detected=true sig=%q dist=%f", sig, dist)
			}
			if dist != 0 {
				t.Errorf("expected distance=0 for empty input, got %f", dist)
			}
		})
	}
}

func Test_DetectLexicalDrift_IdenticalCorpora(t *testing.T) {
	// Identical messages → cosine distance ≈ 0 → no drift.
	messages := []string{
		"The system processed the customer request successfully.",
		"Looking up the order status in our database now.",
	}
	sig, dist, detected := DetectLexicalDrift(messages, messages)
	if detected {
		t.Errorf("identical corpora should not flag drift, got sig=%q dist=%f", sig, dist)
	}
	if dist > 0.1 {
		t.Errorf("identical corpora should produce distance ~0, got %f", dist)
	}
}

func Test_DetectLexicalDrift_DisjointCorpora(t *testing.T) {
	// Maximally different lexical territories → distance close to 1
	// → severe drift bucket.
	current := []string{
		"zzz qqq xxx zzz qqq xxx zzz qqq xxx",
	}
	historical := []string{
		"abc def ghi abc def ghi abc def ghi",
	}
	sig, dist, detected := DetectLexicalDrift(current, historical)
	if !detected {
		t.Fatalf("disjoint corpora should flag drift, got sig=%q dist=%f detected=false", sig, dist)
	}
	if dist < 0.7 {
		t.Errorf("disjoint corpora should produce distance ≥ 0.70, got %f", dist)
	}
	if !strings.HasPrefix(sig, "lexical_drift_0.70") {
		t.Errorf("disjoint corpora should land in 0.70+ bucket, got signature %q", sig)
	}
}

// ─────────────────────────────────────────────────────────────────────
// DetectLexicalDriftWithThresholds
// ─────────────────────────────────────────────────────────────────────

func Test_DetectLexicalDrift_InvalidThresholdsFallBackToDefaults(t *testing.T) {
	// Per the docstring: "if t fails validForBucketing, the detector
	// falls back to defaults rather than producing bucketing chaos."
	// Verify by passing thresholds that violate ordering, then
	// confirming the result matches DefaultDriftThresholds behavior.
	current := []string{"zzz qqq xxx zzz qqq xxx"}
	historical := []string{"abc def ghi abc def ghi"}

	bad := DriftThresholds{
		LexicalLow:    0.80, // higher than Medium — invalid
		LexicalMedium: 0.60,
		LexicalHigh:   0.70,
	}
	sigBad, distBad, detectedBad := DetectLexicalDriftWithThresholds(current, historical, bad)
	sigDefault, distDefault, detectedDefault := DetectLexicalDrift(current, historical)

	if detectedBad != detectedDefault {
		t.Errorf("invalid thresholds should fall back to default behavior; detected mismatch: bad=%v default=%v", detectedBad, detectedDefault)
	}
	if math.Abs(distBad-distDefault) > 0.001 {
		t.Errorf("invalid thresholds should fall back; distance mismatch: bad=%f default=%f", distBad, distDefault)
	}
	if sigBad != sigDefault {
		t.Errorf("invalid thresholds should fall back; signature mismatch: bad=%q default=%q", sigBad, sigDefault)
	}
}

func Test_DetectLexicalDrift_CustomThresholdsEmbedInSignature(t *testing.T) {
	// Customers who tune thresholds should see signatures embedding
	// THEIR cutoff values (per the docstring) so the dashboard
	// signature filter remains meaningful for tuned projects.
	current := []string{"zzz qqq xxx zzz qqq xxx"}
	historical := []string{"abc def ghi abc def ghi"}

	custom := DriftThresholds{
		LexicalLow:    0.20,
		LexicalMedium: 0.40,
		LexicalHigh:   0.60,
	}
	sig, _, detected := DetectLexicalDriftWithThresholds(current, historical, custom)
	if !detected {
		t.Fatal("custom thresholds should detect on disjoint corpora")
	}
	if !strings.HasPrefix(sig, "lexical_drift_0.60") {
		t.Errorf("signature should embed customer's high cutoff (0.60), got %q", sig)
	}
}

// ─────────────────────────────────────────────────────────────────────
// DriftThresholds.validForBucketing
// ─────────────────────────────────────────────────────────────────────

func Test_DriftThresholds_validForBucketing(t *testing.T) {
	cases := []struct {
		name string
		t    DriftThresholds
		want bool
	}{
		{
			name: "valid_defaults",
			t:    DefaultDriftThresholds(),
			want: true,
		},
		{
			name: "valid_custom",
			t:    DriftThresholds{LexicalLow: 0.30, LexicalMedium: 0.50, LexicalHigh: 0.80},
			want: true,
		},
		{
			name: "out_of_range_low_negative",
			t:    DriftThresholds{LexicalLow: -0.1, LexicalMedium: 0.5, LexicalHigh: 0.7},
			want: false,
		},
		{
			name: "out_of_range_high_over_one",
			t:    DriftThresholds{LexicalLow: 0.3, LexicalMedium: 0.5, LexicalHigh: 1.1},
			want: false,
		},
		{
			name: "ordering_violation_low_eq_medium",
			t:    DriftThresholds{LexicalLow: 0.5, LexicalMedium: 0.5, LexicalHigh: 0.7},
			want: false,
		},
		{
			name: "ordering_violation_low_gt_medium",
			t:    DriftThresholds{LexicalLow: 0.6, LexicalMedium: 0.5, LexicalHigh: 0.7},
			want: false,
		},
		{
			name: "ordering_violation_medium_eq_high",
			t:    DriftThresholds{LexicalLow: 0.3, LexicalMedium: 0.7, LexicalHigh: 0.7},
			want: false,
		},
		{
			name: "all_zeros",
			t:    DriftThresholds{LexicalLow: 0, LexicalMedium: 0, LexicalHigh: 0},
			want: false, // ordering: 0 < 0 < 0 is false
		},
		{
			name: "boundary_one_each",
			// 0.0 / 0.5 / 1.0 satisfies all bounds and ordering
			t:    DriftThresholds{LexicalLow: 0.0, LexicalMedium: 0.5, LexicalHigh: 1.0},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.t.validForBucketing()
			if got != tc.want {
				t.Errorf("DriftThresholds%+v.validForBucketing() = %v, want %v", tc.t, got, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// DefaultDriftThresholds — guard against accidental default drift
// ─────────────────────────────────────────────────────────────────────

func Test_DefaultDriftThresholds_LocksDocumentedValues(t *testing.T) {
	// The detector's docstring documents defaults as 0.45 / 0.55 /
	// 0.70 (empirical-tuning). If someone changes these
	// without updating the docstring + dashboard tile, this test
	// fails the build. Drift-from-the-defaults is its own class of
	// silent breakage.
	d := DefaultDriftThresholds()
	if d.LexicalLow != 0.45 {
		t.Errorf("DefaultDriftThresholds.LexicalLow = %f, want 0.45", d.LexicalLow)
	}
	if d.LexicalMedium != 0.55 {
		t.Errorf("DefaultDriftThresholds.LexicalMedium = %f, want 0.55", d.LexicalMedium)
	}
	if d.LexicalHigh != 0.70 {
		t.Errorf("DefaultDriftThresholds.LexicalHigh = %f, want 0.70", d.LexicalHigh)
	}
	if !d.validForBucketing() {
		t.Error("DefaultDriftThresholds() must satisfy validForBucketing()")
	}
}

// ─────────────────────────────────────────────────────────────────────
// normalize helper — covered indirectly elsewhere, locked here
// ─────────────────────────────────────────────────────────────────────

func Test_normalize(t *testing.T) {
	cases := map[string]string{
		"":                   "",
		"  ":                 "",
		"claude-sonnet-4-6":  "claude-sonnet-4-6",
		"Claude-Sonnet-4-6":  "claude-sonnet-4-6",
		"  GPT-5  ":          "gpt-5",
		"\tgemini-2.5-pro\n": "gemini-2.5-pro",
	}
	for in, want := range cases {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}
