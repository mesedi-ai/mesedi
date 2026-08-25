// Per-tier model selection for AI root-cause analysis.
//
// Until 2026-08-20 the model was a hardcoded string at the call site in
// HandleAnalyzeFailureGroup, so every tier — free Hobby through
// hand-sold Enterprise — got identical analysis quality. That left the
// most natural premium differentiator on the table: root-cause
// reasoning over structured failure telemetry is exactly the workload
// where a larger model produces visibly better output, and the cost
// delta is negligible at any volume Mesedi will see.
//
// The economics, measured rather than assumed (see the rate table in
// internal/anthropic/pricing.go):
//
//	Haiku 4.5   $1/$5 per MTok   ->  ~$0.007 per analysis typical
//	Sonnet 5    $2/$10 per MTok  ->  ~$0.014 per analysis typical
//
// Output is capped at 1024 tokens, so the worst case is bounded too.
// A Production customer running 2,000 analyses a month on Sonnet 5
// costs roughly $28 against $1,500 of revenue — under 2%. The included
// 200 analyses on Team cost about $1.40 in total. AI analysis is not a
// cost center at this scale; it is a quality lever.
//
// Tier mapping note. Both hand-sold tiers get the premium model:
// TierProduction (Cloud Production, from $1,500/mo) and TierEnterprise.
// Production is a real backend tier rather than an alias for
// enterprise, because the customer's own dashboard renders the tier
// name and a Production customer seeing "Cloud Enterprise" would
// reasonably think they had been billed wrong.

package api

const (
	// AnalysisModelStandard is the model used for self-serve tiers
	// (Hobby, Team). Fast and cheap; adequate for the common case.
	AnalysisModelStandard = "claude-haiku-4-5"

	// AnalysisModelPremium is the model used for hand-sold tiers
	// (Production, Enterprise). Stronger reasoning on multi-signal
	// failure groups, at roughly 2x the per-analysis cost of
	// AnalysisModelStandard — single-digit dollars per month per
	// customer at realistic volumes.
	AnalysisModelPremium = "claude-sonnet-5"
)

// premiumAnalysisTiers is the set of tiers that receive
// AnalysisModelPremium. Declared as data rather than inline cases so
// the pricing-card promise ("analyses run on a more capable model")
// has exactly one place it can drift from.
var premiumAnalysisTiers = map[string]bool{
	TierProduction: true,
	TierEnterprise: true,
}

const (
	// analysisMaxTokensStandard is the long-standing output cap. Sized
	// for Haiku, which answers directly and rarely approaches it.
	analysisMaxTokensStandard = 1024

	// analysisMaxTokensPremium gives the premium model room to reason
	// BEFORE it writes.
	//
	// Why this exists: on 2026-08-24, coordination_deadlock — the most
	// reasoning-heavy class in the taxonomy, a cycle in a handoff graph
	// across named agents — came back HTTP 200, 13 seconds, correct
	// model recorded, and completely EMPTY. Nineteen simpler classes
	// succeeded through the identical path. The output budget was
	// consumed before any prose was emitted, so the response carried no
	// text block at all.
	//
	// 1024 was chosen when Haiku was the only model. A larger model
	// asked a harder question needs more headroom, and the cost of
	// headroom is nothing: output is billed per token USED, not per
	// token allowed, so raising the ceiling costs $0 on the analyses
	// that never approach it.
	analysisMaxTokensPremium = 4096
)

// analysisMaxTokensForModel returns the output cap for an analysis
// call. Unknown models get the conservative standard cap.
func analysisMaxTokensForModel(model string) int {
	if model == AnalysisModelPremium {
		return analysisMaxTokensPremium
	}
	return analysisMaxTokensStandard
}

// analysisModelForTier returns the Anthropic model id used for AI
// root-cause analysis on the given tier.
//
// tier is expected to be already normalized (see normalizeTier), so the
// legacy "pro" value has been folded into TierTeam before it arrives.
// Any unrecognized tier falls back to the standard model: an unknown
// tier must never silently escalate a customer onto the more expensive
// model.
func analysisModelForTier(tier string) string {
	if premiumAnalysisTiers[normalizeTier(tier)] {
		return AnalysisModelPremium
	}
	return AnalysisModelStandard
}
