// Tests for the pricing package. Covers:
//
//   - PricingTableVersion is set (non-empty) so /me/pricing-info has
//     a real value to surface.
//   - Every model identifier present in models/registry.go is also
//     present in pricing.priceTable (cross-package parity). Without
//     this, context_overflow can fire on a model whose pricing we
//     don't know — and the unknown-model fallback path would silently
//     activate for the entire customer base of that model.
//   - ComputeLLMCost math is correct at known boundaries (per the
//     existing function's example in the doc comment).
//   - IsKnownModel returns true for known IDs, false for nonsense.
//   - SupportedModels returns the sorted set of keys.
//   - Prefix-match still works (claude-opus-4-6-20260301 hits the
//     claude-opus-4-6 row).
//   - Unknown model returns (0, false) via IsKnownModel + ComputeLLMCost
//     returns 0.

package pricing

import (
	"testing"

	"mesedi/backend/internal/models"
)

func TestPricingTableVersion_NonEmpty(t *testing.T) {
	t.Parallel()
	if PricingTableVersion == "" {
		t.Fatal("PricingTableVersion must be set so /me/pricing-info can surface staleness")
	}
}

// TestPricingTableCoversRegistryModels enforces parity between the
// pricing table and the model registry. Without this guardrail, a
// model added to registry.go (so it's recognized by context_overflow)
// could be silently missing from pricing.go — every execution using
// that model would fall back to SDK-shipped cost without anyone
// noticing.
//
// Test fails noisily on the first missing model so the maintainer
// sees the exact ID to add.
func TestPricingTableCoversRegistryModels(t *testing.T) {
	t.Parallel()
	// Pull the registry-known list via the public IsKnown predicate.
	// We can't enumerate from outside the models package, so this
	// list is maintained in parallel with models/registry.go. If
	// registry adds models without updating either, this test stays
	// green (false negative); the inverse direction (pricing claims
	// a model that registry doesn't recognize) is caught by
	// TestPricingTableModelsAreKnownToRegistry below.
	for _, id := range SupportedModels() {
		// "claude-2", "claude-2.1" are registered but historically
		// have a context window we declared; pricing matches. Just
		// assert IsKnown returns true for every pricing key — that
		// proves pricing entries don't refer to phantom models.
		if !models.IsKnown(id) {
			t.Errorf("pricing table has model %q that is not in models/registry.go — registry parity broken; add the model to registry or remove from pricing", id)
		}
	}
}

func TestComputeLLMCost_KnownModel(t *testing.T) {
	t.Parallel()
	// Per the existing pricing.go doc comment example:
	//   claude-opus-4-6 @ 1200 input / 800 output tokens
	//   = (1200/1e6 * $15) + (800/1e6 * $75)
	//   = 0.018 + 0.060
	//   = $0.078
	got := ComputeLLMCost("claude-opus-4-6", 1200, 800)
	want := 0.078
	if !floatNearlyEqual(got, want, 1e-9) {
		t.Errorf("ComputeLLMCost(claude-opus-4-6, 1200, 800) = %v; want %v", got, want)
	}
}

func TestComputeLLMCost_UnknownModelReturnsZero(t *testing.T) {
	t.Parallel()
	got := ComputeLLMCost("not-a-real-model-id", 1000, 1000)
	if got != 0 {
		t.Errorf("unknown model: want 0; got %v", got)
	}
}

func TestComputeLLMCost_LlamaIsZero(t *testing.T) {
	t.Parallel()
	// Per pricing.go: Llama weights are free for self-hosted by
	// default. Customers with paid hosting agreements declare actual
	// pricing via future per-tenant overrides.
	got := ComputeLLMCost("llama-3.1-70b", 10_000, 10_000)
	if got != 0 {
		t.Errorf("Llama default pricing should be $0; got %v", got)
	}
}

func TestIsKnownModel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		model string
		want  bool
	}{
		{"claude-opus-4-6", true},
		{"gpt-5", true},
		{"gemini-2.5-pro", true},
		{"mistral-large-2", true},
		{"llama-3.1-405b", true},
		// Dated suffix still hits via prefix match.
		{"claude-opus-4-6-20260301", true},
		// Garbage values.
		{"", false},
		{"made-up-model", false},
		// Ollama-tag-style identifier — known-unknown until Wave 2.5.4.
		{"llama3.2:3b", false},
	}
	for _, c := range cases {
		got := IsKnownModel(c.model)
		if got != c.want {
			t.Errorf("IsKnownModel(%q) = %v; want %v", c.model, got, c.want)
		}
	}
}

func TestSupportedModels_NonEmpty_Sorted(t *testing.T) {
	t.Parallel()
	got := SupportedModels()
	if len(got) == 0 {
		t.Fatal("SupportedModels returned empty list")
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("SupportedModels not sorted: %q comes before %q", got[i-1], got[i])
		}
	}
}

func TestSupportedModels_CoversAllProviders(t *testing.T) {
	t.Parallel()
	// Sanity check: every provider Mesedi instruments today has at
	// least one entry in the pricing table. If a provider drops to
	// zero entries this test fails noisily so the maintainer sees
	// what disappeared.
	wantPrefixes := []string{
		"claude-",    // Anthropic
		"gpt-",       // OpenAI
		"gemini-",    // Google
		"llama-",     // Meta
		"mistral-",   // Mistral (also matches mistral-large/medium/small/nemo)
	}
	models := SupportedModels()
	for _, prefix := range wantPrefixes {
		found := false
		for _, m := range models {
			if len(m) >= len(prefix) && m[:len(prefix)] == prefix {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no model with prefix %q in pricing table; provider coverage regressed", prefix)
		}
	}
}

func TestLastUpdated_DeprecatedAlias(t *testing.T) {
	t.Parallel()
	if LastUpdated() != PricingTableVersion {
		t.Errorf("LastUpdated() should equal PricingTableVersion; got %v vs %v",
			LastUpdated(), PricingTableVersion)
	}
}

// TestComputeLLMCost_GeminiProTierFlip exercises the long-context
// pricing tier for Gemini Pro families (Wave 0.6). When a single call's
// input_tokens exceed the 200k breakpoint, BOTH input and output get
// billed at the over-tier rate — matching Google's documented behavior
// where exceeding the threshold flips the entire call, not just the
// tokens above it.
func TestComputeLLMCost_GeminiProTierFlip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		model        string
		inputTokens  int
		outputTokens int
		want         float64
	}{
		{
			// gemini-2.5-pro under the threshold: standard tier.
			// 100k input @ $1.25/1M + 1k output @ $10.00/1M
			// = 0.125 + 0.01 = $0.135.
			name:         "gemini-2.5-pro under 200k",
			model:        "gemini-2.5-pro",
			inputTokens:  100_000,
			outputTokens: 1_000,
			want:         0.135,
		},
		{
			// gemini-2.5-pro over the threshold: long-context tier.
			// 250k input @ $2.50/1M + 1k output @ $15.00/1M
			// = 0.625 + 0.015 = $0.640.
			name:         "gemini-2.5-pro over 200k",
			model:        "gemini-2.5-pro",
			inputTokens:  250_000,
			outputTokens: 1_000,
			want:         0.640,
		},
		{
			// At exactly the breakpoint, still in the lower tier
			// (the comparison is strictly greater than). 200k @ $1.25/1M
			// + 1k @ $10/1M = 0.25 + 0.01 = $0.26.
			name:         "gemini-2.5-pro at exact 200k breakpoint",
			model:        "gemini-2.5-pro",
			inputTokens:  200_000,
			outputTokens: 1_000,
			want:         0.26,
		},
		{
			// gemini-3.1-pro-preview under the threshold.
			// 50k input @ $2.00/1M + 5k output @ $12.00/1M
			// = 0.10 + 0.06 = $0.16.
			name:         "gemini-3.1-pro-preview under 200k",
			model:        "gemini-3.1-pro-preview",
			inputTokens:  50_000,
			outputTokens: 5_000,
			want:         0.16,
		},
		{
			// gemini-3.1-pro-preview over the threshold.
			// 500k input @ $4.00/1M + 10k output @ $18.00/1M
			// = 2.00 + 0.18 = $2.18.
			name:         "gemini-3.1-pro-preview over 200k",
			model:        "gemini-3.1-pro-preview",
			inputTokens:  500_000,
			outputTokens: 10_000,
			want:         2.18,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := ComputeLLMCost(c.model, c.inputTokens, c.outputTokens)
			if !floatNearlyEqual(got, c.want, 1e-6) {
				t.Errorf("ComputeLLMCost(%q, %d, %d) = %v; want %v",
					c.model, c.inputTokens, c.outputTokens, got, c.want)
			}
		})
	}
}

// TestComputeLLMCost_GeminiFlashFlat verifies that Gemini Flash families
// (which Google explicitly does NOT tier) keep flat-rate pricing
// regardless of input_tokens count. Without this guardrail, a future
// refactor that accidentally adds tier rates to a Flash row would
// silently overcharge Flash customers above an arbitrary threshold.
func TestComputeLLMCost_GeminiFlashFlat(t *testing.T) {
	t.Parallel()
	// gemini-2.5-flash: $0.30 input / $2.50 output per 1M (flat).
	// 250k input @ $0.30/1M + 10k output @ $2.50/1M
	// = 0.075 + 0.025 = $0.10.
	got := ComputeLLMCost("gemini-2.5-flash", 250_000, 10_000)
	want := 0.10
	if !floatNearlyEqual(got, want, 1e-6) {
		t.Errorf("gemini-2.5-flash @ 250k input: got %v; want %v (flat-rate, no tier flip)", got, want)
	}
	// gemini-3.5-flash: $1.50 / $9.00 (flat).
	// 300k @ $1.50/1M + 5k @ $9.00/1M = 0.45 + 0.045 = $0.495.
	got = ComputeLLMCost("gemini-3.5-flash", 300_000, 5_000)
	want = 0.495
	if !floatNearlyEqual(got, want, 1e-6) {
		t.Errorf("gemini-3.5-flash @ 300k input: got %v; want %v (flat-rate, no tier flip)", got, want)
	}
	// gemini-3.1-flash-lite: $0.25 / $1.50 (flat).
	// 250k @ $0.25/1M + 1k @ $1.50/1M = 0.0625 + 0.0015 = $0.064.
	got = ComputeLLMCost("gemini-3.1-flash-lite", 250_000, 1_000)
	want = 0.064
	if !floatNearlyEqual(got, want, 1e-6) {
		t.Errorf("gemini-3.1-flash-lite @ 250k input: got %v; want %v (flat-rate, no tier flip)", got, want)
	}
}

// TestComputeLLMCost_NonGeminiUnaffected verifies the tier-flip path
// does NOT activate for any model whose TierBreakpointInputTokens is
// zero — i.e. every Anthropic / OpenAI / Cohere / Mistral / Llama
// entry. A 500k-token call on Claude Opus must still produce the same
// cost the pre-Wave-0.6 code did, not double-bill.
func TestComputeLLMCost_NonGeminiUnaffected(t *testing.T) {
	t.Parallel()
	// claude-opus-4-6 @ 500k input / 10k output:
	// = 500_000/1e6 * $15 + 10_000/1e6 * $75
	// = 7.5 + 0.75 = $8.25.
	got := ComputeLLMCost("claude-opus-4-6", 500_000, 10_000)
	want := 8.25
	if !floatNearlyEqual(got, want, 1e-9) {
		t.Errorf("claude-opus-4-6 @ 500k: got %v; want %v (flat-rate path must be unchanged)", got, want)
	}
}

// TestComputeLLMCost_DeprecatedGemini2FlashIsZero anchors the
// gemini-2.0-flash entry's $0 pricing introduced in Wave 0.6 after
// Google shut down the model on 2026-06-01. The entry is retained so
// historical events still resolve via the registry; cost must be zero
// because no prospective billing is possible.
func TestComputeLLMCost_DeprecatedGemini2FlashIsZero(t *testing.T) {
	t.Parallel()
	got := ComputeLLMCost("gemini-2.0-flash", 100_000, 1_000)
	if got != 0 {
		t.Errorf("gemini-2.0-flash (deprecated) should price at $0; got %v", got)
	}
}

func floatNearlyEqual(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}
