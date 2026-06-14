// Unit tests for HandleAdminGetAnalyticsSummary (#214).
//
// Coverage:
//   - MRR counts only Team projects with a Stripe subscription id.
//     Admin-comped tier flips (tier='team' AND
//     stripe_subscription_id='') do NOT count toward MRR.
//   - CompedTeamProjects surfaces the comped count separately.
//   - Hobby + Enterprise rows are ignored entirely.
//   - Tier comparison is case-insensitive (matches the existing
//     strings.EqualFold call in the handler).
//
// The Stripe-derived fields (ThisMonthGrossUSD, etc.) are NOT
// exercised here: those require a live Stripe.Configured() == true
// and would need an HTTP-level Stripe mock. They sit behind a clear
// branch in the handler and the local-DB MRR path is what #214
// changed.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"mesedi/backend/internal/store"
)

// stubAnalyticsStore embeds store.Store so it satisfies the
// interface; only ListAllProjects is implemented (which is all the
// handler reaches when Stripe.Configured() is false).
type stubAnalyticsStore struct {
	store.Store
	projects []*store.AdminProjectRow
	err      error
}

func (s *stubAnalyticsStore) ListAllProjects(_ context.Context) ([]*store.AdminProjectRow, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.projects, nil
}

func Test_HandleAdminGetAnalyticsSummary_CompedTeamExcludedFromMRR(t *testing.T) {
	// Two paying Team subs, one comped Team (no sub), one Hobby.
	// Expected: ActiveTeamSubscriptions=2, CompedTeamProjects=1,
	// MRR = 2 * 99 = $198, ARR = $2376.
	st := &stubAnalyticsStore{
		projects: []*store.AdminProjectRow{
			{
				ProjectID:            "proj-pay-1",
				Tier:                 TierTeam,
				StripeSubscriptionID: "sub_paying1",
			},
			{
				ProjectID:            "proj-pay-2",
				Tier:                 TierTeam,
				StripeSubscriptionID: "sub_paying2",
			},
			{
				ProjectID:            "proj-comped",
				Tier:                 TierTeam,
				StripeSubscriptionID: "",
			},
			{
				ProjectID: "proj-hobby",
				Tier:      TierHobby,
			},
		},
	}
	// StripeConfig left empty => Stripe.Configured() returns false
	// and the handler short-circuits before the live Stripe call.
	h := &Handlers{Store: st, Logger: quietLogger()}

	r := httptest.NewRequest(http.MethodGet, "/admin/analytics-summary", nil)
	w := httptest.NewRecorder()

	h.HandleAdminGetAnalyticsSummary(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	var resp AdminAnalyticsSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if resp.ActiveTeamSubscriptions != 2 {
		t.Errorf("ActiveTeamSubscriptions = %d, want 2",
			resp.ActiveTeamSubscriptions)
	}
	if resp.CompedTeamProjects != 1 {
		t.Errorf("CompedTeamProjects = %d, want 1",
			resp.CompedTeamProjects)
	}
	const wantMRR = 2 * 99.0
	if resp.MRRUsd != wantMRR {
		t.Errorf("MRRUsd = %.2f, want %.2f (paying Team count × $99)",
			resp.MRRUsd, wantMRR)
	}
	if resp.ARRUsd != wantMRR*12 {
		t.Errorf("ARRUsd = %.2f, want %.2f (MRR × 12)",
			resp.ARRUsd, wantMRR*12)
	}
}

func Test_HandleAdminGetAnalyticsSummary_AllCompedNoMRR(t *testing.T) {
	// Three comped Team projects, no paying subs anywhere.
	// Expected: ActiveTeamSubscriptions=0, MRR=$0,
	// CompedTeamProjects=3.
	st := &stubAnalyticsStore{
		projects: []*store.AdminProjectRow{
			{ProjectID: "c1", Tier: TierTeam},
			{ProjectID: "c2", Tier: TierTeam},
			{ProjectID: "c3", Tier: TierTeam},
		},
	}
	h := &Handlers{Store: st, Logger: quietLogger()}

	r := httptest.NewRequest(http.MethodGet, "/admin/analytics-summary", nil)
	w := httptest.NewRecorder()
	h.HandleAdminGetAnalyticsSummary(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	var resp AdminAnalyticsSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.MRRUsd != 0 {
		t.Errorf("MRRUsd = %.2f, want 0 (no paying Team subs)",
			resp.MRRUsd)
	}
	if resp.ActiveTeamSubscriptions != 0 {
		t.Errorf("ActiveTeamSubscriptions = %d, want 0",
			resp.ActiveTeamSubscriptions)
	}
	if resp.CompedTeamProjects != 3 {
		t.Errorf("CompedTeamProjects = %d, want 3",
			resp.CompedTeamProjects)
	}
}

func Test_HandleAdminGetAnalyticsSummary_TierCaseInsensitive(t *testing.T) {
	// Older rows may have been written with "Team" (capital T)
	// before the normalization layer. Stay tolerant: case-insensitive
	// compare on tier.
	st := &stubAnalyticsStore{
		projects: []*store.AdminProjectRow{
			{
				ProjectID:            "proj-mixed-case",
				Tier:                 "Team",
				StripeSubscriptionID: "sub_paying",
			},
		},
	}
	h := &Handlers{Store: st, Logger: quietLogger()}

	r := httptest.NewRequest(http.MethodGet, "/admin/analytics-summary", nil)
	w := httptest.NewRecorder()
	h.HandleAdminGetAnalyticsSummary(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	var resp AdminAnalyticsSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.ActiveTeamSubscriptions != 1 {
		t.Errorf("ActiveTeamSubscriptions = %d, want 1 "+
			"(case-insensitive tier compare)",
			resp.ActiveTeamSubscriptions)
	}
}
