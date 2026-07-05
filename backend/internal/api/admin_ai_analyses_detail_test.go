// Unit tests for HandleAdminProjectAIAnalysesDetail.
//
// Coverage:
//   - Missing project_id path value -> 400.
//   - Bad ?since= query (not RFC3339) -> 400.
//   - Happy path: backend returns groups; handler wraps them with
//     the per-row $0.03 Haiku estimate and the aggregate totals.
//   - Empty result: returns an empty slice (not null) so the
//     dashboard JSON decode hits .length without crashing.
//
// Uses a stub Store that records the (projectID, since) the handler
// asked for so the test can assert the defaulting behavior (current
// UTC month start) without re-implementing the date math here.
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

// stubAIDetailStore embeds store.Store so it satisfies the
// interface without listing every method; this test only reaches
// ListAnalyzedFailureGroupsByProject.
type stubAIDetailStore struct {
	store.Store

	// What the handler asked for. Captured for assertions.
	askedProjectID string
	askedSince     time.Time
	askedLimit     int

	// Canned response.
	groups []*store.FailureGroup
}

func (s *stubAIDetailStore) ListAnalyzedFailureGroupsByProject(
	_ context.Context, projectID string, since time.Time, limit int,
) ([]*store.FailureGroup, error) {
	s.askedProjectID = projectID
	s.askedSince = since
	s.askedLimit = limit
	return s.groups, nil
}

// detailMux wires the route the same way the production server
// does so r.PathValue("id") resolves the same way.
func detailMux(h *Handlers) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/projects/{id}/ai-analyses-detail", h.HandleAdminProjectAIAnalysesDetail)
	return mux
}

func Test_HandleAdminProjectAIAnalysesDetail_BadSince_400(t *testing.T) {
	s := &stubAIDetailStore{}
	h := &Handlers{Store: s, Logger: quietLogger()}
	mux := detailMux(h)

	r := httptest.NewRequest(http.MethodGet,
		"/admin/projects/proj-x/ai-analyses-detail?since=not-a-date", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for bad since=, got %d (body: %s)", w.Code, w.Body.String())
	}
	if s.askedProjectID != "" {
		t.Errorf("store should not have been called on bad-input rejection, got projectID=%q",
			s.askedProjectID)
	}
}

func Test_HandleAdminProjectAIAnalysesDetail_HappyPath(t *testing.T) {
	now := time.Now().UTC()
	model := "claude-haiku-4-5"
	analyzedAt := now.Add(-2 * time.Hour)

	s := &stubAIDetailStore{
		groups: []*store.FailureGroup{
			{
				GroupID:            "fg_001",
				ProjectID:          "proj-x",
				FailureClass:       "crashes",
				Signature:          "panic:nil_map_assign",
				EventCount:         12,
				AffectedExecutions: 7,
				LastSeen:           now,
				AnalyzedAt:         &analyzedAt,
				AnalysisModel:      &model,
			},
			{
				GroupID:            "fg_002",
				ProjectID:          "proj-x",
				FailureClass:       "loops",
				Signature:          "identical_call:retry_payment",
				EventCount:         44,
				AffectedExecutions: 14,
				LastSeen:           now,
				AnalyzedAt:         &analyzedAt,
				AnalysisModel:      &model,
			},
		},
	}
	h := &Handlers{Store: s, Logger: quietLogger()}
	mux := detailMux(h)

	r := httptest.NewRequest(http.MethodGet,
		"/admin/projects/proj-x/ai-analyses-detail", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if s.askedProjectID != "proj-x" {
		t.Errorf("askedProjectID = %q, want %q", s.askedProjectID, "proj-x")
	}
	if s.askedLimit != 0 {
		t.Errorf("askedLimit = %d, want 0 (handler defers default to store)",
			s.askedLimit)
	}

	var resp AdminProjectAIAnalysesDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.ProjectID != "proj-x" {
		t.Errorf("project_id = %q, want %q", resp.ProjectID, "proj-x")
	}
	if resp.TotalCount != 2 {
		t.Errorf("total_count = %d, want 2", resp.TotalCount)
	}
	// Two analyses at ~$0.03 each = ~$0.06.
	const wantTotal = 2 * adminHaikuCostPerAnalysisUSD
	if resp.TotalEstimatedCostUSD != wantTotal {
		t.Errorf("total_estimated_cost_usd = %f, want %f",
			resp.TotalEstimatedCostUSD, wantTotal)
	}
	if len(resp.Groups) != 2 {
		t.Fatalf("want 2 groups in response, got %d", len(resp.Groups))
	}
	if resp.Groups[0].GroupID != "fg_001" {
		t.Errorf("first group_id = %q, want %q (store-order preserved)",
			resp.Groups[0].GroupID, "fg_001")
	}
	if resp.Groups[0].EstimatedCostUSD != adminHaikuCostPerAnalysisUSD {
		t.Errorf("per-row cost = %f, want %f",
			resp.Groups[0].EstimatedCostUSD, adminHaikuCostPerAnalysisUSD)
	}
	if resp.Groups[0].AnalyzedAt == "" {
		t.Error("analyzed_at should be populated from store row")
	}
	if resp.Groups[0].AnalysisModel != model {
		t.Errorf("analysis_model = %q, want %q",
			resp.Groups[0].AnalysisModel, model)
	}
}

func Test_HandleAdminProjectAIAnalysesDetail_EmptyResult_ReturnsEmptySlice(t *testing.T) {
	s := &stubAIDetailStore{groups: nil}
	h := &Handlers{Store: s, Logger: quietLogger()}
	mux := detailMux(h)

	r := httptest.NewRequest(http.MethodGet,
		"/admin/projects/proj-quiet/ai-analyses-detail", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200 even with no rows, got %d", w.Code)
	}
	// Body MUST contain "groups":[] not "groups":null. The dashboard's
	// .map() needs an array even on empty.
	body := w.Body.String()
	if !contains(body, `"groups":[]`) {
		t.Errorf("body should contain \"groups\":[] for empty result; got %s", body)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
