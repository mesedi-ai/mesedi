// Unit tests for HobbyBillingScheduler additions:
//   - buildHobbyChargeDescription (pure)
//   - computeHobbyAnalysisCostUSD (needs a Store stub)
//
// Why these matter:
//   - The Description string lands on the customer's credit-card
//     statement + Stripe dashboard. A regression that shows
//     "Mesedi Hobby period charge" instead of an itemized
//     breakdown burns customer trust faster than almost any other
//     billing bug.
//   - The analysis-cost compute path is the source of truth that
//     turns row-count-in-the-DB into dollars-on-the-PaymentIntent.
//     Off-by-one here means either over-billing or under-billing.
package api

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"mesedi/backend/internal/store"
)

// stubBillingStore is the minimal Store implementation that
// computeHobbyAnalysisCostUSD calls. Other methods panic if
// invoked (see stubAIUsageStore for the same defensive pattern).
type stubBillingStore struct {
	store.Store

	count    int
	countErr error
	gotSince time.Time
}

func (s *stubBillingStore) CountAIAnalysesSincePeriodStart(
	ctx context.Context, projectID string, since time.Time,
) (int, error) {
	s.gotSince = since
	return s.count, s.countErr
}

func TestBuildHobbyChargeDescription_ExecutionsOnly(t *testing.T) {
	got := buildHobbyChargeDescription(2500, 0)
	want := "Mesedi Hobby overage: 2500 executions x $0.002"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestBuildHobbyChargeDescription_AnalysesOnly(t *testing.T) {
	got := buildHobbyChargeDescription(0, 12)
	want := "Mesedi Hobby AI root-cause: 12 analyses x $0.75"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestBuildHobbyChargeDescription_Both(t *testing.T) {
	got := buildHobbyChargeDescription(2500, 12)
	want := "Mesedi Hobby overage: 2500 executions x $0.002 + Mesedi Hobby AI root-cause: 12 analyses x $0.75"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestBuildHobbyChargeDescription_ZeroIsDefensiveFallback(t *testing.T) {
	// Caller is supposed to skip zero-cost charges before calling
	// this. Defensive fallback should still produce a non-empty,
	// recognizable string so the Stripe Description field never
	// gets an empty value (which Stripe rejects).
	got := buildHobbyChargeDescription(0, 0)
	if got == "" {
		t.Fatal("got empty string for zero/zero; want defensive fallback")
	}
	if got != "Mesedi Hobby period charge" {
		t.Errorf("got %q, want %q (defensive fallback)", got, "Mesedi Hobby period charge")
	}
}

func TestComputeHobbyAnalysisCostUSD_WithPeriodStart(t *testing.T) {
	periodStart := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	st := &stubBillingStore{count: 17}
	s := &HobbyBillingScheduler{Store: st}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cost, count := s.computeHobbyAnalysisCostUSD(
		context.Background(),
		&store.Project{
			ProjectID:          "proj_h",
			CurrentPeriodStart: &periodStart,
		},
		log,
	)
	if count != 17 {
		t.Errorf("count: got %d, want 17", count)
	}
	want := 17 * HobbyAIAnalysisPriceUSD
	if cost != want {
		t.Errorf("cost: got %v, want %v", cost, want)
	}
	if !st.gotSince.Equal(periodStart) {
		t.Errorf("since: got %v, want %v (pinned to CurrentPeriodStart)",
			st.gotSince, periodStart)
	}
}

func TestComputeHobbyAnalysisCostUSD_NoPeriodStart_Uses30DayFallback(t *testing.T) {
	st := &stubBillingStore{count: 3}
	s := &HobbyBillingScheduler{Store: st}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	before := time.Now().UTC().AddDate(0, -1, 0)
	_, _ = s.computeHobbyAnalysisCostUSD(
		context.Background(),
		&store.Project{ProjectID: "proj_h", CurrentPeriodStart: nil},
		log,
	)
	after := time.Now().UTC().AddDate(0, -1, 0)

	if st.gotSince.Before(before) || st.gotSince.After(after) {
		t.Errorf("since: got %v, want between %v and %v (30-day fallback)",
			st.gotSince, before, after)
	}
}

func TestComputeHobbyAnalysisCostUSD_ZeroCount_ReturnsZero(t *testing.T) {
	st := &stubBillingStore{count: 0}
	s := &HobbyBillingScheduler{Store: st}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cost, count := s.computeHobbyAnalysisCostUSD(
		context.Background(),
		&store.Project{ProjectID: "proj_h"},
		log,
	)
	if cost != 0 || count != 0 {
		t.Errorf("got cost=%v count=%d, want both zero", cost, count)
	}
}

func TestComputeHobbyAnalysisCostUSD_DBError_TreatsAsZero(t *testing.T) {
	// A DB blip on the count query must NOT produce a surprise
	// charge from a number we can't trust. Treat as zero; the
	// next tick will re-count.
	st := &stubBillingStore{countErr: errors.New("boom")}
	s := &HobbyBillingScheduler{Store: st}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cost, count := s.computeHobbyAnalysisCostUSD(
		context.Background(),
		&store.Project{ProjectID: "proj_h"},
		log,
	)
	if cost != 0 || count != 0 {
		t.Errorf("got cost=%v count=%d on DB error, want both zero (fail safe)",
			cost, count)
	}
}
