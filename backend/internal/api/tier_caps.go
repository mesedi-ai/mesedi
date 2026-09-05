package api

import "context"

// Tier-aware caps for per-project configuration knobs.
//
// Some per-project configs map directly to cost or abuse vectors
// (e.g. time_budget_ms, a 24h cap on a free tier means a runaway
// agent can rack up 24h of storage+detection cost). Caps here ensure
// each tier can only configure within an upper bound that matches
// what their plan economics support. Hobby customers can't burn
// Enterprise budgets; Enterprise customers retain the full range.
//
// Design principle (FOUNDATION longterm-best): apply tier caps only
// where they solve a REAL cost/abuse problem. provider_incident_
// min_tenants is intentionally NOT tier-capped because it's an
// architectural metric (how many tenants must hit a provider error
// to count as cross-tenant signal), not a cost vector, Hobby
// customers legitimately need to set it to 1 for their single-tenant
// setup.
//
// Cap rationale:
//
//   time_budget_ms:
//     Hobby      5 minutes  (chat-agent workloads; alarming on a
//                            longer running execution would imply
//                            paying for a workload the tier isn't
//                            priced to support)
//     Team       1 hour     (research-agent workloads)
//     Enterprise 24 hours   (batch / orchestration)
//
//   tool_return_value_max_bytes:
//     Hobby      4 KB       (typical small structured returns)
//     Team       32 KB      (richer nested objects)
//     Enterprise 1 MB       (matches the wire-format payload cap)
//
// All caps are upper bounds; customers can always configure BELOW
// the cap. The hardcoded default value used when a customer hasn't
// configured anything (8192 for tool_return_value, 60_000 for
// time_budget) stays the same across tiers, sensible defaults
// don't need tier discrimination.

const (
	// time_budget_ms tier caps (milliseconds).
	tierCapTimeBudgetHobbyMs      = 5 * 60 * 1_000       // 300_000
	tierCapTimeBudgetTeamMs       = 60 * 60 * 1_000      // 3_600_000
	tierCapTimeBudgetEnterpriseMs = 24 * 60 * 60 * 1_000 // 86_400_000

	// tool_return_value_max_bytes tier caps.
	tierCapToolReturnValueHobbyBytes      = 4 * 1024    // 4 KB
	tierCapToolReturnValueTeamBytes       = 32 * 1024   // 32 KB
	tierCapToolReturnValueEnterpriseBytes = 1024 * 1024 // 1 MB

	// ─── detector-threshold tier caps ───────────────────
	//
	// Tier caps apply ONLY where raising the value maps to a real
	// cost or abuse vector. The other 9 thresholds are
	// pure alerting-sensitivity knobs (raising them just changes
	// when the detector fires, with no asymmetric cost to Mesedi);
	// they ship with global bounds in the validators registry, no
	// tier discrimination. Same doctrine as cost_velocity (
	// shipped with "no tier cap; global floor/ceiling").
	//
	// token_waste prefix_window_chars IS tier-capped: bigger window
	// means hashing more text per event and a larger shingle set on
	// the near-duplicate fallback. Both are real CPU vectors on the
	// detector hot path, and an Enterprise customer's 64KB window
	// would burn through a Hobby customer's per-execution budget.

	// token_waste prefix_window_chars tier caps (characters).
	tierCapTokenWastePrefixWindowHobby      = 4096  // 4 KB
	tierCapTokenWastePrefixWindowTeam       = 16384 // 16 KB
	tierCapTokenWastePrefixWindowEnterprise = 65536 // 64 KB
)

// TierCapTimeBudgetMs returns the maximum time_budget_ms a customer
// on the given tier may configure. Falls back to the Hobby cap for
// unknown / empty tiers, the strictest cap, safest default.
func TierCapTimeBudgetMs(tier string) int {
	switch normalizeTier(tier) {
	case TierProduction, TierEnterprise:
		return tierCapTimeBudgetEnterpriseMs
	case TierTeam:
		return tierCapTimeBudgetTeamMs
	default:
		return tierCapTimeBudgetHobbyMs
	}
}

// TierCapToolReturnValueBytes returns the maximum
// tool_return_value_max_bytes a customer on the given tier may
// configure. Falls back to the Hobby cap for unknown / empty
// tiers, the strictest cap.
func TierCapToolReturnValueBytes(tier string) int {
	switch normalizeTier(tier) {
	case TierProduction, TierEnterprise:
		return tierCapToolReturnValueEnterpriseBytes
	case TierTeam:
		return tierCapToolReturnValueTeamBytes
	default:
		return tierCapToolReturnValueHobbyBytes
	}
}

// tierCapTokenWastePrefixWindow returns the maximum
// token_waste.prefix_window_chars a customer on the given tier may
// configure. Larger windows mean hashing more text per event AND a
// larger shingle set on the near-duplicate fallback, both real CPU
// vectors on the detector hot path. Unknown / empty tier falls back
// to the Hobby cap (strictest).
func tierCapTokenWastePrefixWindow(tier string) int {
	switch normalizeTier(tier) {
	case TierProduction, TierEnterprise:
		return tierCapTokenWastePrefixWindowEnterprise
	case TierTeam:
		return tierCapTokenWastePrefixWindowTeam
	default:
		return tierCapTokenWastePrefixWindowHobby
	}
}

// lookupProjectTier resolves the project's tier as a normalized
// string (TierHobby / TierTeam / TierEnterprise). Falls back to
// TierHobby on any error, strictest cap, safest default. Used by
// the time_budget / tool_return_value handlers (and any future
// tier-aware endpoint) to wire the tier_caps constants into the
// API surface.
func (h *Handlers) lookupProjectTier(
	ctx context.Context, projectID string,
) string {
	proj, err := h.Store.GetProject(ctx, projectID)
	if err != nil || proj == nil {
		return TierHobby
	}
	return normalizeTier(proj.Tier)
}
