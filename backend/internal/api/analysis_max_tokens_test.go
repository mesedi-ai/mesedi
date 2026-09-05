package api

import "testing"

// The premium model must get a larger output budget than the standard
// one. This is not a style preference, it is the difference between
// an analysis and a blank card.
//
// On 2026-08-24, coordination_deadlock (the most reasoning-heavy class
// in the taxonomy: a cycle in a handoff graph across named agents)
// returned HTTP 200 with the correct model recorded and ZERO text,
// reproducibly, while 19 simpler classes succeeded through the exact
// same code path. The output budget, 1024, a number chosen when Haiku
// was the only model, was consumed before any prose was emitted.
func TestAnalysisMaxTokens_PremiumGetsMoreThanStandard(t *testing.T) {
	t.Parallel()
	std := analysisMaxTokensForModel(AnalysisModelStandard)
	prem := analysisMaxTokensForModel(AnalysisModelPremium)
	if prem <= std {
		t.Fatalf("premium budget (%d) must exceed standard (%d); a larger "+
			"model asked a harder question needs room to reason before "+
			"it writes", prem, std)
	}
}

// The standard cap must not drift. Changing it silently alters output
// length for every self-serve customer, which is the majority of
// traffic.
func TestAnalysisMaxTokens_StandardUnchanged(t *testing.T) {
	t.Parallel()
	if got := analysisMaxTokensForModel(AnalysisModelStandard); got != 1024 {
		t.Errorf("standard budget changed to %d; it has been 1024 since "+
			"the analysis feature shipped", got)
	}
}

// Unknown models fall back to the conservative cap rather than the
// expensive one. Raising a budget should be a deliberate act.
func TestAnalysisMaxTokens_UnknownModelIsConservative(t *testing.T) {
	t.Parallel()
	for _, m := range []string{"", "some-future-model", "claude-opus-4-6"} {
		if got := analysisMaxTokensForModel(m); got != analysisMaxTokensStandard {
			t.Errorf("model %q: expected the standard cap %d, got %d",
				m, analysisMaxTokensStandard, got)
		}
	}
}
