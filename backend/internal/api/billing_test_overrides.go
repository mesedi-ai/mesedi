// Test-only runtime overrides for tier-quota constants.
//
// Purpose
// -------
// The compile-time constants HobbyExecutionLimit (5000),
// TeamExecutionIncluded (100000), HobbyAIAnalysisLimit (25), and
// TeamAIAnalysisLimit (200) make overage math impossible to exercise
// in an automated smoke test — pushing 5000+ executions through the
// ingest hot path is too slow and pollutes the DB. This file exposes
// runtime overrides that let staging tests replace those quotas with
// small values (e.g. 10) so overage math + Stripe metered-billing
// push can be verified end-to-end in seconds.
//
// Safety
// ------
// Overrides are read exactly once at startup from environment
// variables and installed via InitTestOverrides. Production deploys
// never set the env vars, so the effective behaviour is bit-for-bit
// identical to referencing the raw constants. A startup log line
// prints "test-mode tier overrides active: ..." when any override
// is installed, so an accidental production deploy with an override
// set is immediately visible in the logs.
//
// The tools/check-tier-constants.sh drift guard continues to assert
// the compile-time constants match the TS TIER_CONSTANTS — the
// override is a runtime-only concept and does not affect the values
// customers see in the dashboard or the marketing page.

package api

import (
	"log/slog"
	"sync/atomic"
)

// Package-level override storage. Zero means "override not set;
// use the compile-time constant." All reads go through the
// Effective* helpers below so callsites don't have to remember to
// check.
var (
	hobbyExecutionLimitOverride   atomic.Int64
	teamExecutionIncludedOverride atomic.Int64
	hobbyAIAnalysisLimitOverride  atomic.Int32
	teamAIAnalysisLimitOverride   atomic.Int32
)

// TestOverrides carries the staging-only test hooks. Populate from
// MESEDI_HOBBY_EXECUTION_LIMIT_OVERRIDE,
// MESEDI_TEAM_EXECUTION_LIMIT_OVERRIDE,
// MESEDI_HOBBY_AI_ANALYSIS_LIMIT_OVERRIDE,
// MESEDI_TEAM_AI_ANALYSIS_LIMIT_OVERRIDE. Any field left at zero
// means "no override; use the compile-time constant."
type TestOverrides struct {
	HobbyExecutionLimit   int64
	TeamExecutionIncluded int64
	HobbyAIAnalysisLimit  int
	TeamAIAnalysisLimit   int
}

// InitTestOverrides installs the runtime overrides and logs which
// (if any) are active. Idempotent for re-init in tests — each call
// replaces the prior values. Callers should invoke exactly once at
// startup.
func InitTestOverrides(t TestOverrides, logger *slog.Logger) {
	hobbyExecutionLimitOverride.Store(t.HobbyExecutionLimit)
	teamExecutionIncludedOverride.Store(t.TeamExecutionIncluded)
	hobbyAIAnalysisLimitOverride.Store(int32(t.HobbyAIAnalysisLimit))
	teamAIAnalysisLimitOverride.Store(int32(t.TeamAIAnalysisLimit))

	if !AnyTestOverrideActive() {
		return
	}
	active := ActiveTestOverrides()
	logger.Warn(
		"TEST-MODE TIER OVERRIDES ACTIVE: this build will use synthetic "+
			"quotas instead of production values. Confirm you are on "+
			"staging or CI, NEVER on prod.",
		"overrides", active,
	)
}

// AnyTestOverrideActive returns true if any of the four overrides
// is currently non-zero. Used by the /health/config surface (for
// harness verification) and by the startup log.
func AnyTestOverrideActive() bool {
	return hobbyExecutionLimitOverride.Load() > 0 ||
		teamExecutionIncludedOverride.Load() > 0 ||
		hobbyAIAnalysisLimitOverride.Load() > 0 ||
		teamAIAnalysisLimitOverride.Load() > 0
}

// ActiveTestOverrides returns a map of only the overrides that are
// currently set (i.e., non-zero). Convenient for structured logging
// and for the harness's boot-time sanity check.
func ActiveTestOverrides() map[string]int64 {
	out := map[string]int64{}
	if v := hobbyExecutionLimitOverride.Load(); v > 0 {
		out["hobby_execution_limit"] = v
	}
	if v := teamExecutionIncludedOverride.Load(); v > 0 {
		out["team_execution_included"] = v
	}
	if v := hobbyAIAnalysisLimitOverride.Load(); v > 0 {
		out["hobby_ai_analysis_limit"] = int64(v)
	}
	if v := teamAIAnalysisLimitOverride.Load(); v > 0 {
		out["team_ai_analysis_limit"] = int64(v)
	}
	return out
}

// EffectiveHobbyExecutionLimit returns the runtime override if set,
// else the compile-time HobbyExecutionLimit constant. Every callsite
// that computes overage against the Hobby quota should use this
// helper instead of the raw constant.
func EffectiveHobbyExecutionLimit() int64 {
	if v := hobbyExecutionLimitOverride.Load(); v > 0 {
		return v
	}
	return HobbyExecutionLimit
}

// EffectiveTeamExecutionIncluded is the Team-tier counterpart to
// EffectiveHobbyExecutionLimit.
func EffectiveTeamExecutionIncluded() int64 {
	if v := teamExecutionIncludedOverride.Load(); v > 0 {
		return v
	}
	return TeamExecutionIncluded
}

// EffectiveHobbyAIAnalysisLimit returns the runtime override if set,
// else the compile-time HobbyAIAnalysisLimit. Used by the Hobby AI-
// analysis pay-per-use gate.
func EffectiveHobbyAIAnalysisLimit() int {
	if v := hobbyAIAnalysisLimitOverride.Load(); v > 0 {
		return int(v)
	}
	return HobbyAIAnalysisLimit
}

// EffectiveTeamAIAnalysisLimit is the Team-tier counterpart to
// EffectiveHobbyAIAnalysisLimit.
func EffectiveTeamAIAnalysisLimit() int {
	if v := teamAIAnalysisLimitOverride.Load(); v > 0 {
		return int(v)
	}
	return TeamAIAnalysisLimit
}
