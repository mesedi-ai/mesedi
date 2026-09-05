package api

import (
	"io"
	"log/slog"
	"testing"
)

// discardLogger is a slog logger that drops every record. Keeps
// InitTestOverrides warn logs out of the test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Reset clears any prior override state. Called at the top of each
// override-touching test so tests don't leak into each other.
func resetOverrides(t *testing.T) {
	t.Helper()
	hobbyExecutionLimitOverride.Store(0)
	teamExecutionIncludedOverride.Store(0)
	hobbyAIAnalysisLimitOverride.Store(0)
	teamAIAnalysisLimitOverride.Store(0)
}

func TestEffectiveLimits_NoOverride_ReturnsCompileTimeConstants(t *testing.T) {
	resetOverrides(t)
	if got := EffectiveHobbyExecutionLimit(); got != HobbyExecutionLimit {
		t.Errorf("EffectiveHobbyExecutionLimit() = %d, want %d", got, HobbyExecutionLimit)
	}
	if got := EffectiveTeamExecutionIncluded(); got != TeamExecutionIncluded {
		t.Errorf("EffectiveTeamExecutionIncluded() = %d, want %d", got, TeamExecutionIncluded)
	}
	if got := EffectiveHobbyAIAnalysisLimit(); got != HobbyAIAnalysisLimit {
		t.Errorf("EffectiveHobbyAIAnalysisLimit() = %d, want %d", got, HobbyAIAnalysisLimit)
	}
	if got := EffectiveTeamAIAnalysisLimit(); got != TeamAIAnalysisLimit {
		t.Errorf("EffectiveTeamAIAnalysisLimit() = %d, want %d", got, TeamAIAnalysisLimit)
	}
}

func TestEffectiveLimits_WithOverride_ReturnsOverride(t *testing.T) {
	resetOverrides(t)
	InitTestOverrides(TestOverrides{
		HobbyExecutionLimit:   10,
		TeamExecutionIncluded: 20,
		HobbyAIAnalysisLimit:  3,
		TeamAIAnalysisLimit:   5,
	}, discardLogger())
	t.Cleanup(func() { resetOverrides(t) })

	if got := EffectiveHobbyExecutionLimit(); got != 10 {
		t.Errorf("EffectiveHobbyExecutionLimit() = %d, want 10", got)
	}
	if got := EffectiveTeamExecutionIncluded(); got != 20 {
		t.Errorf("EffectiveTeamExecutionIncluded() = %d, want 20", got)
	}
	if got := EffectiveHobbyAIAnalysisLimit(); got != 3 {
		t.Errorf("EffectiveHobbyAIAnalysisLimit() = %d, want 3", got)
	}
	if got := EffectiveTeamAIAnalysisLimit(); got != 5 {
		t.Errorf("EffectiveTeamAIAnalysisLimit() = %d, want 5", got)
	}
}

func TestEffectiveLimits_PartialOverride_UnsetFieldsFallBack(t *testing.T) {
	resetOverrides(t)
	InitTestOverrides(TestOverrides{
		HobbyExecutionLimit: 10, // set
		// other three left at 0 (unset), should fall back to constants
	}, discardLogger())
	t.Cleanup(func() { resetOverrides(t) })

	if got := EffectiveHobbyExecutionLimit(); got != 10 {
		t.Errorf("EffectiveHobbyExecutionLimit() = %d, want 10", got)
	}
	if got := EffectiveTeamExecutionIncluded(); got != TeamExecutionIncluded {
		t.Errorf("EffectiveTeamExecutionIncluded() = %d, want %d (fallback)",
			got, TeamExecutionIncluded)
	}
	if got := EffectiveHobbyAIAnalysisLimit(); got != HobbyAIAnalysisLimit {
		t.Errorf("EffectiveHobbyAIAnalysisLimit() = %d, want %d (fallback)",
			got, HobbyAIAnalysisLimit)
	}
	if got := EffectiveTeamAIAnalysisLimit(); got != TeamAIAnalysisLimit {
		t.Errorf("EffectiveTeamAIAnalysisLimit() = %d, want %d (fallback)",
			got, TeamAIAnalysisLimit)
	}
}

func TestAnyTestOverrideActive(t *testing.T) {
	resetOverrides(t)
	if AnyTestOverrideActive() {
		t.Fatal("no overrides installed but AnyTestOverrideActive() returned true")
	}
	InitTestOverrides(TestOverrides{HobbyExecutionLimit: 10}, discardLogger())
	t.Cleanup(func() { resetOverrides(t) })

	if !AnyTestOverrideActive() {
		t.Fatal("HobbyExecutionLimit override installed but AnyTestOverrideActive() returned false")
	}
}

func TestActiveTestOverrides_OnlyReturnsSetFields(t *testing.T) {
	resetOverrides(t)
	InitTestOverrides(TestOverrides{
		HobbyExecutionLimit: 10,
		TeamAIAnalysisLimit: 5,
	}, discardLogger())
	t.Cleanup(func() { resetOverrides(t) })

	m := ActiveTestOverrides()
	if len(m) != 2 {
		t.Errorf("expected 2 active overrides, got %d: %v", len(m), m)
	}
	if v := m["hobby_execution_limit"]; v != 10 {
		t.Errorf("hobby_execution_limit = %d, want 10", v)
	}
	if v := m["team_ai_analysis_limit"]; v != 5 {
		t.Errorf("team_ai_analysis_limit = %d, want 5", v)
	}
	if _, ok := m["team_execution_included"]; ok {
		t.Error("team_execution_included should NOT appear when unset")
	}
}

// TestTierExecutionLimit_HonorsOverride covers the routing through
// tierExecutionLimit, the central helper, as an end-to-end proof
// that the plumbing wires up.
func TestTierExecutionLimit_HonorsOverride(t *testing.T) {
	resetOverrides(t)
	InitTestOverrides(TestOverrides{
		HobbyExecutionLimit:   10,
		TeamExecutionIncluded: 20,
	}, discardLogger())
	t.Cleanup(func() { resetOverrides(t) })

	if got := tierExecutionLimit(TierHobby); got != 10 {
		t.Errorf("tierExecutionLimit(hobby) = %d, want 10", got)
	}
	if got := tierExecutionLimit(TierTeam); got != 20 {
		t.Errorf("tierExecutionLimit(team) = %d, want 20", got)
	}
	if got := tierExecutionLimit(TierEnterprise); got != 0 {
		t.Errorf("tierExecutionLimit(enterprise) = %d, want 0 (uncapped)", got)
	}
}
