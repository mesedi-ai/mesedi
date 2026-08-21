// Production is a duplicate of Enterprise with a different customer-
// facing name. These tests pin that equivalence.
//
// The risk being guarded: Production was added by adding
// `case TierProduction, TierEnterprise:` at eight existing switch
// sites. Miss one and Production silently falls through to the
// `default:` branch, which in every one of those switches is the
// HOBBY behavior — the strictest caps and the shortest retention. A
// customer paying $1,500/mo would quietly get free-tier limits with
// no error anywhere. Asserting "Production == Enterprise" across the
// whole surface catches a missed site immediately, and catches any
// future switch that adds an Enterprise case without a Production one.

package api

import "testing"

func TestProductionMatchesEnterprise_TierCaps(t *testing.T) {
	t.Parallel()
	if got, want := TierCapTimeBudgetMs(TierProduction),
		TierCapTimeBudgetMs(TierEnterprise); got != want {
		t.Errorf("TierCapTimeBudgetMs: production=%d enterprise=%d "+
			"(production likely fell through to the hobby default)", got, want)
	}
	if got, want := TierCapToolReturnValueBytes(TierProduction),
		TierCapToolReturnValueBytes(TierEnterprise); got != want {
		t.Errorf("TierCapToolReturnValueBytes: production=%d enterprise=%d", got, want)
	}
	if got, want := tierCapTokenWastePrefixWindow(TierProduction),
		tierCapTokenWastePrefixWindow(TierEnterprise); got != want {
		t.Errorf("tierCapTokenWastePrefixWindow: production=%d enterprise=%d", got, want)
	}
}

func TestProductionMatchesEnterprise_RetentionCap(t *testing.T) {
	t.Parallel()
	pDays, pIndef := tierRetentionCap(TierProduction)
	eDays, eIndef := tierRetentionCap(TierEnterprise)
	if pDays != eDays || pIndef != eIndef {
		t.Fatalf("tierRetentionCap: production=(%d,%v) enterprise=(%d,%v); "+
			"a Production customer must not be capped at the Hobby default",
			pDays, pIndef, eDays, eIndef)
	}
}

func TestProductionMatchesEnterprise_ExecutionLimit(t *testing.T) {
	t.Parallel()
	// 0 means "no enforced included quota" — volume is negotiated.
	if got, want := tierExecutionLimit(TierProduction),
		tierExecutionLimit(TierEnterprise); got != want {
		t.Fatalf("tierExecutionLimit: production=%d enterprise=%d", got, want)
	}
	if tierExecutionLimit(TierProduction) == tierExecutionLimit(TierHobby) {
		t.Fatal("production is being metered like hobby — the switch case is missing")
	}
}

func TestProductionGetsPremiumAnalysisModel(t *testing.T) {
	t.Parallel()
	if got, want := analysisModelForTier(TierProduction),
		analysisModelForTier(TierEnterprise); got != want {
		t.Fatalf("analysisModelForTier: production=%s enterprise=%s", got, want)
	}
}

// Production has a real listed price, so unlike Enterprise its ROI
// baseline is the actual floor of the published range rather than a
// placeholder. It must still be non-zero — a zero cost makes the ROI
// multiple infinite and the savings card meaningless.
func TestProductionSubscriptionCostIsRealAndNonZero(t *testing.T) {
	t.Parallel()
	got := subscriptionCostFor(TierProduction)
	if got <= 0 {
		t.Fatalf("subscriptionCostFor(production) = %v; must be > 0", got)
	}
	if got != subscriptionCostProductionMonthly {
		t.Errorf("expected the listed floor %v, got %v",
			subscriptionCostProductionMonthly, got)
	}
	if got <= subscriptionCostFor(TierTeam) {
		t.Errorf("production (%v) should cost more than team (%v)",
			got, subscriptionCostFor(TierTeam))
	}
}
