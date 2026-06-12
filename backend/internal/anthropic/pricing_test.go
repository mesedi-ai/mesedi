// Tests for the per-model pricing registry (#199).
//
// Coverage:
//   - Known model exact match returns the published rate.
//   - Prefix-matched variant (e.g., snapshot id) falls through to
//     the longest known prefix.
//   - Unknown model returns the conservative UnknownModelRate.
//   - Empty / whitespace input also returns UnknownModelRate.
//   - ComputeCostUSD math: 1M input tokens at $1/MTok = $1, etc.
//   - Mixed input+output sums correctly.
package anthropic

import (
	"math"
	"testing"
)

func Test_LookupRate_KnownModel(t *testing.T) {
	r := LookupRate("claude-haiku-4-5")
	if r.InputUSDPerMTok != 1.00 {
		t.Errorf("haiku input rate = %.2f, want 1.00", r.InputUSDPerMTok)
	}
	if r.OutputUSDPerMTok != 5.00 {
		t.Errorf("haiku output rate = %.2f, want 5.00", r.OutputUSDPerMTok)
	}
}

func Test_LookupRate_PrefixMatch(t *testing.T) {
	// A snapshot-suffixed id like Anthropic uses internally should
	// fall through to the base model's rate.
	r := LookupRate("claude-haiku-4-5-snapshot-20251001-experimental")
	if r.InputUSDPerMTok != 1.00 {
		t.Errorf("prefix-matched haiku input = %.2f, want 1.00", r.InputUSDPerMTok)
	}
}

func Test_LookupRate_UnknownModel(t *testing.T) {
	r := LookupRate("claude-future-99")
	if r != UnknownModelRate {
		t.Errorf("unknown model should return UnknownModelRate, got %+v", r)
	}
}

func Test_LookupRate_EmptyInput(t *testing.T) {
	if LookupRate("") != UnknownModelRate {
		t.Error("empty model id should return UnknownModelRate")
	}
	if LookupRate("   ") != UnknownModelRate {
		t.Error("whitespace model id should return UnknownModelRate")
	}
}

func Test_LookupRate_CaseInsensitive(t *testing.T) {
	r := LookupRate("Claude-Haiku-4-5")
	if r.InputUSDPerMTok != 1.00 {
		t.Errorf("case-insensitive lookup failed, input rate = %.2f, want 1.00",
			r.InputUSDPerMTok)
	}
}

func Test_ComputeCostUSD_Haiku(t *testing.T) {
	// 1M input + 200k output @ Haiku rates ($1/MTok input, $5/MTok output).
	// Expected: 1.00 * 1.00 + 0.20 * 5.00 = 1.00 + 1.00 = $2.00
	got := ComputeCostUSD("claude-haiku-4-5", 1_000_000, 200_000)
	want := 2.00
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("ComputeCostUSD(haiku, 1M, 200k) = %.6f, want %.2f", got, want)
	}
}

func Test_ComputeCostUSD_TypicalAnalysis(t *testing.T) {
	// Real-world: ~2000 input tokens, ~600 output tokens on Haiku.
	// 2000/1M * $1 + 600/1M * $5 = $0.002 + $0.003 = $0.005
	got := ComputeCostUSD("claude-haiku-4-5", 2000, 600)
	want := 0.005
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("typical analysis cost = %.6f, want %.6f", got, want)
	}
}

func Test_ComputeCostUSD_ZeroTokens(t *testing.T) {
	got := ComputeCostUSD("claude-haiku-4-5", 0, 0)
	if got != 0 {
		t.Errorf("zero tokens should yield $0, got %.6f", got)
	}
}

func Test_ComputeCostUSD_UnknownModel(t *testing.T) {
	// Unknown model falls back to UnknownModelRate ($2/$10).
	// 1M input + 1M output = $2 + $10 = $12.
	got := ComputeCostUSD("claude-future-99", 1_000_000, 1_000_000)
	want := 12.00
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("unknown model cost = %.6f, want %.2f", got, want)
	}
}
