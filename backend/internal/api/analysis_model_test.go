package api

import (
	"testing"

	"mesedi/backend/internal/anthropic"
)

// Self-serve tiers must stay on the cheap model. A regression here
// silently multiplies per-analysis cost across the entire free and
// $99 customer base, where the included volumes are largest.
func TestAnalysisModelForTier_SelfServeTiersUseStandardModel(t *testing.T) {
	t.Parallel()
	for _, tier := range []string{TierHobby, TierTeam} {
		if got := analysisModelForTier(tier); got != AnalysisModelStandard {
			t.Errorf("tier %q: expected %s, got %s",
				tier, AnalysisModelStandard, got)
		}
	}
}

// Both hand-sold tiers deliver the premium-model promise printed on
// the pricing card. Production is the one customers actually buy at
// $1,500/mo, so a regression here is a paid-for feature silently not
// being delivered.
func TestAnalysisModelForTier_HandSoldTiersUsePremiumModel(t *testing.T) {
	t.Parallel()
	for _, tier := range []string{TierProduction, TierEnterprise} {
		if got := analysisModelForTier(tier); got != AnalysisModelPremium {
			t.Errorf("tier %q: expected %s, got %s",
				tier, AnalysisModelPremium, got)
		}
	}
}

// Fail SAFE, not expensive: an unrecognized tier string must not
// escalate a customer onto the pricier model.
func TestAnalysisModelForTier_UnknownTierFallsBackToStandard(t *testing.T) {
	t.Parallel()
	for _, tier := range []string{"", "self_hosted", "gibberish", "ENTERPRISE"} {
		if got := analysisModelForTier(tier); got != AnalysisModelStandard {
			t.Errorf("tier %q should fall back to %s, got %s",
				tier, AnalysisModelStandard, got)
		}
	}
}

// The legacy "pro" tier normalizes to team before reaching model
// selection. Asserted end-to-end so a change to normalizeTier can't
// quietly promote legacy Pro customers onto the premium model.
func TestAnalysisModelForTier_LegacyProNormalizesToStandard(t *testing.T) {
	t.Parallel()
	if got := analysisModelForTier(normalizeTier(TierProLegacy)); got != AnalysisModelStandard {
		t.Fatalf("legacy pro should resolve to %s, got %s", AnalysisModelStandard, got)
	}
}

// Both models MUST be present in the rate table. If a model id isn't
// there, LookupRate silently falls back to UnknownModelRate and every
// estimated_cost_usd we report for that model is a guess.
//
// Uses HasExplicitRate rather than comparing rates by value: sonnet-5's
// real price ($2/$10) is identical to UnknownModelRate, so a value
// comparison reports it missing even when it is correctly listed. That
// is not hypothetical, this test failed that exact way on first run.
func TestAnalysisModelsHaveExplicitPricing(t *testing.T) {
	t.Parallel()
	for _, model := range []string{AnalysisModelStandard, AnalysisModelPremium} {
		if !anthropic.HasExplicitRate(model) {
			t.Errorf("%s is not in modelRates, add an explicit entry in "+
				"internal/anthropic/pricing.go so cost reporting isn't a "+
				"coincidence", model)
		}
		rate := anthropic.LookupRate(model)
		if rate.InputUSDPerMTok <= 0 || rate.OutputUSDPerMTok <= 0 {
			t.Errorf("%s has non-positive rate %+v", model, rate)
		}
	}
}

// The premium model should genuinely cost more than the standard one.
// If this ever inverts, the tier split is upside down and the cheap
// tier is burning more money than the expensive one.
func TestPremiumAnalysisModelCostsMoreThanStandard(t *testing.T) {
	t.Parallel()
	std := anthropic.LookupRate(AnalysisModelStandard)
	prem := anthropic.LookupRate(AnalysisModelPremium)
	if prem.InputUSDPerMTok <= std.InputUSDPerMTok ||
		prem.OutputUSDPerMTok <= std.OutputUSDPerMTok {
		t.Fatalf("premium (%+v) should cost more than standard (%+v); "+
			"if this fails the tier split is inverted", prem, std)
	}
}
