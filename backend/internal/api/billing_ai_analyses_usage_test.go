// Unit tests for HandleGetAIAnalysesUsage.
//
// Why these tests exist:
//   - HandleGetAIAnalysesUsage is the read-side counter the dashboard
//     surfaces on /app/billing as the "AI root-cause usage" card. The
//     enforcement path is HandleAnalyzeFailureGroup; this read path
//     MUST match enforcement exactly or the user sees a number that
//     disagrees with what gets blocked. The tier-branching here is
//     the place that can drift, so we lock it down.
//
// Coverage targets (one subtest per branch in the handler):
//   - Hobby pay-per-use → Applicable=true, Limit=50, Count from
//     CountAIAnalysesSincePeriodStart (Hobby is single-project so tenant
//     scope = project scope), PricePerAnalysisUSD=$0.75,
//     EstimatedSpendUSD=count*$0.75.
//   - Enterprise → Applicable=false, Limit=0, Count=0 (dashboard hides card).
//   - Team with tenant_id present → Applicable=true, Count from
//     CountAIAnalysesByTenantSince (the canonical Team-tier query).
//   - Team with tenant_id NULL → Applicable=true, Count from
//     CountAIAnalysesSincePeriodStart (legacy backfill fallback).
//   - Team with CurrentPeriodStart populated → PeriodStart/End surfaced
//     and the `since` value pinned to that period (not the 30-day
//     fallback) so the count matches the customer's actual Stripe period.
//
// Strategy: stub the store. The Store interface is large (200+
// methods) and seeding a real SQLite DB to hit four methods is
// overkill; a struct that embeds store.Store and overrides only the
// four methods this handler calls is enough. Any unstubbed method
// will panic at runtime if the handler ever grows a dependency it
// shouldn't have, which is a useful safety net.
//
// This file establishes the test pattern other handler tests in this
// package can follow. internal/api had no unit tests before this one.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mesedi/backend/internal/store"
)

// stubAIUsageStore implements just enough of store.Store to drive
// HandleGetAIAnalysesUsage. Embedding store.Store satisfies the
// interface; any other method invoked at runtime will panic on the
// nil embedded value, which is the desired behavior (a handler test
// hitting an unstubbed method is testing the wrong handler).
type stubAIUsageStore struct {
	store.Store

	project    *store.Project
	projectErr error

	tenantID  *string
	tenantErr error

	tenantCount    int
	tenantCountErr error

	projectCount    int
	projectCountErr error

	// Capture the `since` value passed into the count call so the
	// "uses CurrentPeriodStart" assertion can verify the handler is
	// pinning to the customer's Stripe period rather than the 30-day
	// fallback.
	gotSince time.Time
}

func (s *stubAIUsageStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	return s.project, s.projectErr
}

func (s *stubAIUsageStore) GetProjectTenantID(ctx context.Context, id string) (*string, error) {
	return s.tenantID, s.tenantErr
}

func (s *stubAIUsageStore) CountAIAnalysesByTenantSince(
	ctx context.Context, tenantID string, since time.Time,
) (int, error) {
	s.gotSince = since
	return s.tenantCount, s.tenantCountErr
}

func (s *stubAIUsageStore) CountAIAnalysesSincePeriodStart(
	ctx context.Context, projectID string, since time.Time,
) (int, error) {
	s.gotSince = since
	return s.projectCount, s.projectCountErr
}

// callHandler wires the project_id into context the same way
// AuthMiddleware does in production (ctxKeyProjectID is unexported,
// so this file lives in package api specifically to reach it).
func callHandler(t *testing.T, h *Handlers, projectID string) (*httptest.ResponseRecorder, AIAnalysesUsageResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/billing/ai-analyses-usage", nil)
	if projectID != "" {
		ctx := context.WithValue(req.Context(), ctxKeyProjectID, projectID)
		req = req.WithContext(ctx)
	}
	rec := httptest.NewRecorder()
	h.HandleGetAIAnalysesUsage(rec, req)

	var body AIAnalysesUsageResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v (body=%q)", err, rec.Body.String())
		}
	}
	return rec, body
}

func TestHandleGetAIAnalysesUsage_NoProjectContext(t *testing.T) {
	h := &Handlers{Store: &stubAIUsageStore{}}
	rec, _ := callHandler(t, h, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestHandleGetAIAnalysesUsage_Hobby_PayPerUse(t *testing.T) {
	// Pre-, Hobby flips from "gated entirely, Applicable=false"
	// to pay-per-use ($0.75 each, 50 / period cap). The handler now
	// returns Applicable=true on Hobby with the pay-per-use shape:
	// Limit=50, Count from the project-scoped query, plus
	// PricePerAnalysisUSD and EstimatedSpendUSD pointers populated.
	st := &stubAIUsageStore{
		project: &store.Project{ProjectID: "proj_h", Tier: TierHobby},
		// Hobby uses per-project count (single-project tier); tenant
		// query should NOT be called. Set tenantCount to a sentinel
		// and verify it's not picked up.
		tenantCount:  999,
		projectCount: 12,
	}
	h := &Handlers{Store: st}

	rec, body := callHandler(t, h, "proj_h")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if !body.OK {
		t.Errorf("OK: got %v, want true", body.OK)
	}
	if !body.Applicable {
		t.Errorf("Applicable: got false, want true for hobby pay-per-use")
	}
	if body.Tier != TierHobby {
		t.Errorf("Tier: got %q, want %q", body.Tier, TierHobby)
	}
	if body.Limit != HobbyAIAnalysisLimit {
		t.Errorf("Limit: got %d, want %d", body.Limit, HobbyAIAnalysisLimit)
	}
	if body.Count != 12 {
		t.Errorf("Count: got %d, want 12 (per-project query) -- tenant query may have been called by mistake", body.Count)
	}
	if body.PricePerAnalysisUSD == nil {
		t.Fatal("PricePerAnalysisUSD: got nil, want pointer to HobbyAIAnalysisPriceUSD")
	}
	if *body.PricePerAnalysisUSD != HobbyAIAnalysisPriceUSD {
		t.Errorf("PricePerAnalysisUSD: got %v, want %v", *body.PricePerAnalysisUSD, HobbyAIAnalysisPriceUSD)
	}
	if body.EstimatedSpendUSD == nil {
		t.Fatal("EstimatedSpendUSD: got nil, want pointer to count*price")
	}
	wantSpend := float64(12) * HobbyAIAnalysisPriceUSD
	if *body.EstimatedSpendUSD != wantSpend {
		t.Errorf("EstimatedSpendUSD: got %v, want %v (count * price)", *body.EstimatedSpendUSD, wantSpend)
	}
}

func TestHandleGetAIAnalysesUsage_Hobby_ZeroCountStillSurfacesPrice(t *testing.T) {
	// A fresh Hobby project with no analyses should still receive
	// PricePerAnalysisUSD so the dashboard can render "Pay-per-use:
	// $0.75 each" without first triggering an analysis. Spend is 0
	// (count*price=0) but the pointer is non-nil so the chip
	// renders the running total.
	st := &stubAIUsageStore{
		project:      &store.Project{ProjectID: "proj_h_fresh", Tier: TierHobby},
		projectCount: 0,
	}
	h := &Handlers{Store: st}

	rec, body := callHandler(t, h, "proj_h_fresh")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if body.Count != 0 {
		t.Errorf("Count: got %d, want 0", body.Count)
	}
	if body.PricePerAnalysisUSD == nil || *body.PricePerAnalysisUSD != HobbyAIAnalysisPriceUSD {
		t.Errorf("PricePerAnalysisUSD: got %v, want %v", body.PricePerAnalysisUSD, HobbyAIAnalysisPriceUSD)
	}
	if body.EstimatedSpendUSD == nil || *body.EstimatedSpendUSD != 0 {
		t.Errorf("EstimatedSpendUSD: got %v, want pointer to 0", body.EstimatedSpendUSD)
	}
}

func TestHandleGetAIAnalysesUsage_Team_WithinIncluded_OverageSpendIsZero(t *testing.T) {
	// : Team now surfaces PricePerAnalysisUSD as the OVERAGE
	// rate ($0.50) plus EstimatedSpendUSD = max(0, count - included)
	// * rate. When count <= 200 (included), spend is zero but the
	// price pointer is still populated so the dashboard knows what
	// the overage rate will be if they cross over.
	tenant := "ten_under"
	st := &stubAIUsageStore{
		project:     &store.Project{ProjectID: "proj_t_under", Tier: TierTeam},
		tenantID:    &tenant,
		tenantCount: 47, // well under 200
	}
	h := &Handlers{Store: st}

	rec, body := callHandler(t, h, "proj_t_under")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if body.PricePerAnalysisUSD == nil {
		t.Fatal("PricePerAnalysisUSD: got nil, want pointer to TeamAIAnalysisOveragePriceUSD even when under quota")
	}
	if *body.PricePerAnalysisUSD != TeamAIAnalysisOveragePriceUSD {
		t.Errorf("PricePerAnalysisUSD: got %v, want %v (overage rate)",
			*body.PricePerAnalysisUSD, TeamAIAnalysisOveragePriceUSD)
	}
	if body.EstimatedSpendUSD == nil {
		t.Fatal("EstimatedSpendUSD: got nil, want pointer to 0 when under quota")
	}
	if *body.EstimatedSpendUSD != 0 {
		t.Errorf("EstimatedSpendUSD: got %v, want 0 when count (%d) < included (%d)",
			*body.EstimatedSpendUSD, body.Count, body.Limit)
	}
}

func TestHandleGetAIAnalysesUsage_Team_AboveIncluded_OverageSpendIsAccurate(t *testing.T) {
	// : When count > 200, EstimatedSpendUSD = (count - 200) *
	// $0.50. This is the line item that will hit the customer's
	// upcoming invoice via handleInvoiceUpcoming.
	tenant := "ten_over"
	st := &stubAIUsageStore{
		project:     &store.Project{ProjectID: "proj_t_over", Tier: TierTeam},
		tenantID:    &tenant,
		tenantCount: 250, // 50 over included
	}
	h := &Handlers{Store: st}

	rec, body := callHandler(t, h, "proj_t_over")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	wantSpend := float64(250-TeamAIAnalysisLimit) * TeamAIAnalysisOveragePriceUSD
	if body.EstimatedSpendUSD == nil || *body.EstimatedSpendUSD != wantSpend {
		t.Errorf("EstimatedSpendUSD: got %v, want %v (50 overage analyses × $0.50)",
			body.EstimatedSpendUSD, wantSpend)
	}
	if body.Count != 250 {
		t.Errorf("Count: got %d, want 250", body.Count)
	}
	if body.Limit != TeamAIAnalysisLimit {
		t.Errorf("Limit: got %d, want %d (included threshold, NOT a hard cap)",
			body.Limit, TeamAIAnalysisLimit)
	}
}

func TestHandleGetAIAnalysesUsage_Enterprise_NotApplicable(t *testing.T) {
	st := &stubAIUsageStore{
		project: &store.Project{ProjectID: "proj_e", Tier: TierEnterprise},
	}
	h := &Handlers{Store: st}

	rec, body := callHandler(t, h, "proj_e")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if body.Applicable {
		t.Errorf("Applicable: got true, want false for enterprise")
	}
	if body.Tier != TierEnterprise {
		t.Errorf("Tier: got %q, want %q", body.Tier, TierEnterprise)
	}
}

func TestHandleGetAIAnalysesUsage_Team_WithTenant_UsesTenantQuery(t *testing.T) {
	tenant := "ten_abc"
	st := &stubAIUsageStore{
		project: &store.Project{
			ProjectID: "proj_t",
			Tier:      TierTeam,
		},
		tenantID:    &tenant,
		tenantCount: 47,
		// Set a sentinel project-count that should NOT be returned;
		// if the handler picks the wrong branch the assertion fails.
		projectCount: 999,
	}
	h := &Handlers{Store: st}

	rec, body := callHandler(t, h, "proj_t")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if !body.Applicable {
		t.Errorf("Applicable: got false, want true for team")
	}
	if body.Tier != TierTeam {
		t.Errorf("Tier: got %q, want %q", body.Tier, TierTeam)
	}
	if body.Limit != TeamAIAnalysisLimit {
		t.Errorf("Limit: got %d, want %d", body.Limit, TeamAIAnalysisLimit)
	}
	if body.Count != 47 {
		t.Errorf("Count: got %d, want 47 (tenant-scoped query) -- handler may have picked project query instead", body.Count)
	}
}

func TestHandleGetAIAnalysesUsage_Team_NoTenant_FallsBackToProjectQuery(t *testing.T) {
	st := &stubAIUsageStore{
		project: &store.Project{
			ProjectID: "proj_t2",
			Tier:      TierTeam,
		},
		tenantID:     nil, // legacy row, tenant_id not backfilled
		tenantCount:  999, // should NOT be returned
		projectCount: 12,
	}
	h := &Handlers{Store: st}

	rec, body := callHandler(t, h, "proj_t2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if !body.Applicable {
		t.Errorf("Applicable: got false, want true for team")
	}
	if body.Count != 12 {
		t.Errorf("Count: got %d, want 12 (project-scoped fallback)", body.Count)
	}
}

func TestHandleGetAIAnalysesUsage_Team_EmptyTenantString_FallsBackToProjectQuery(t *testing.T) {
	// Defensive case: GetProjectTenantID returns a non-nil pointer to an
	// empty string. The switch in the handler treats `*tenantID == ""`
	// as missing and falls back to the per-project query, so this
	// shouldn't crash or accidentally query an empty tenant.
	empty := ""
	st := &stubAIUsageStore{
		project: &store.Project{
			ProjectID: "proj_t3",
			Tier:      TierTeam,
		},
		tenantID:     &empty,
		tenantCount:  999,
		projectCount: 7,
	}
	h := &Handlers{Store: st}

	rec, body := callHandler(t, h, "proj_t3")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if body.Count != 7 {
		t.Errorf("Count: got %d, want 7 (empty-string tenant should fall back to project query)", body.Count)
	}
}

func TestHandleGetAIAnalysesUsage_Team_WithStripePeriod_PinsSinceToPeriodStart(t *testing.T) {
	// When CurrentPeriodStart is populated the handler should use it
	// as `since` (not the 30-day fallback) and surface PeriodStart /
	// PeriodEnd in the response so the dashboard can render the
	// period boundaries next to the counter.
	tenant := "ten_xyz"
	periodStart := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	st := &stubAIUsageStore{
		project: &store.Project{
			ProjectID:          "proj_p",
			Tier:               TierTeam,
			CurrentPeriodStart: &periodStart,
			CurrentPeriodEnd:   &periodEnd,
		},
		tenantID:    &tenant,
		tenantCount: 5,
	}
	h := &Handlers{Store: st}

	rec, body := callHandler(t, h, "proj_p")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if body.PeriodStart == nil || !body.PeriodStart.Equal(periodStart) {
		t.Errorf("PeriodStart: got %v, want %v", body.PeriodStart, periodStart)
	}
	if body.PeriodEnd == nil || !body.PeriodEnd.Equal(periodEnd) {
		t.Errorf("PeriodEnd: got %v, want %v", body.PeriodEnd, periodEnd)
	}
	if !st.gotSince.Equal(periodStart) {
		t.Errorf("count called with since=%v, want pinned to CurrentPeriodStart=%v (the 30-day fallback would have produced a different value)",
			st.gotSince, periodStart)
	}
}

func TestHandleGetAIAnalysesUsage_Team_NoStripePeriod_Uses30DayFallback(t *testing.T) {
	// New customer between checkout and the first
	// customer.subscription.updated webhook. CurrentPeriodStart is
	// nil. The handler should fall back to "30 days ago" so the
	// counter shows the recent past instead of returning zero or
	// erroring out.
	tenant := "ten_new"
	st := &stubAIUsageStore{
		project: &store.Project{
			ProjectID:          "proj_new",
			Tier:               TierTeam,
			CurrentPeriodStart: nil,
		},
		tenantID:    &tenant,
		tenantCount: 3,
	}
	h := &Handlers{Store: st}

	before := time.Now().UTC().AddDate(0, -1, 0)
	rec, body := callHandler(t, h, "proj_new")
	after := time.Now().UTC().AddDate(0, -1, 0)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if body.PeriodStart != nil {
		t.Errorf("PeriodStart: got %v, want nil when no Stripe period", body.PeriodStart)
	}
	if st.gotSince.Before(before) || st.gotSince.After(after) {
		t.Errorf("since: got %v, want between %v and %v (30-day fallback)",
			st.gotSince, before, after)
	}
}
