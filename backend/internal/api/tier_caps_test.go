package api

// Unit tests for tier-aware caps. The caps are policy
// numbers, so the tests document them as much as they verify them —
// a future change to the cap that doesn't update the test is the
// canary that someone needs to reason about why.

import "testing"

func TestTierCapTimeBudgetMs(t *testing.T) {
	cases := []struct {
		tier string
		want int
	}{
		// Documented caps.
		{TierHobby, 300_000},         // 5 minutes
		{TierTeam, 3_600_000},        // 1 hour
		{TierEnterprise, 86_400_000}, // 24 hours
		// Legacy alias from pre-migration-019 days; normalizeTier
		// folds it to Team.
		{TierProLegacy, 3_600_000},
		// Unknown / empty / lowercase variants — fall back to the
		// strictest cap.
		{"", 300_000},
		{"unknown_tier", 300_000},
	}
	for _, tc := range cases {
		t.Run(tc.tier, func(t *testing.T) {
			got := TierCapTimeBudgetMs(tc.tier)
			if got != tc.want {
				t.Errorf("TierCapTimeBudgetMs(%q) = %d, want %d", tc.tier, got, tc.want)
			}
		})
	}
}

func TestTierCapToolReturnValueBytes(t *testing.T) {
	cases := []struct {
		tier string
		want int
	}{
		{TierHobby, 4 * 1024},         // 4 KB
		{TierTeam, 32 * 1024},         // 32 KB
		{TierEnterprise, 1024 * 1024}, // 1 MB
		{TierProLegacy, 32 * 1024},    // legacy alias -> Team
		{"", 4 * 1024},                // unknown -> Hobby
		{"random", 4 * 1024},
	}
	for _, tc := range cases {
		t.Run(tc.tier, func(t *testing.T) {
			got := TierCapToolReturnValueBytes(tc.tier)
			if got != tc.want {
				t.Errorf("TierCapToolReturnValueBytes(%q) = %d, want %d", tc.tier, got, tc.want)
			}
		})
	}
}

func TestTierCaps_AscendingByTier(t *testing.T) {
	// Hard property: caps must increase monotonically as tier value
	// increases. If a future edit inverts this (e.g. accidentally
	// giving Hobby a higher cap than Team), this test catches it.
	if TierCapTimeBudgetMs(TierHobby) >= TierCapTimeBudgetMs(TierTeam) {
		t.Errorf("time_budget Hobby cap >= Team cap; should be strictly less")
	}
	if TierCapTimeBudgetMs(TierTeam) >= TierCapTimeBudgetMs(TierEnterprise) {
		t.Errorf("time_budget Team cap >= Enterprise cap; should be strictly less")
	}
	if TierCapToolReturnValueBytes(TierHobby) >= TierCapToolReturnValueBytes(TierTeam) {
		t.Errorf("tool_return_value Hobby cap >= Team cap; should be strictly less")
	}
	if TierCapToolReturnValueBytes(TierTeam) >= TierCapToolReturnValueBytes(TierEnterprise) {
		t.Errorf("tool_return_value Team cap >= Enterprise cap; should be strictly less")
	}
}
