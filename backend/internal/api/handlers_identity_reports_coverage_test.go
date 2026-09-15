package api

// Coverage-debt handler batch four: the identity echoes and the
// reporting reads. HandleGetMe's legacy-admin fallback is the pin
// that matters most: a key with no user identity on a project with
// no tenant resolves to the owner with the admin role, which is what
// keeps the founder's own early integrations from rendering
// "Signed in as null" with every button greyed out.

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"mesedi/backend/internal/store"
)

func TestIdentityAndReportingEndpoints(t *testing.T) {
	const projectID = "proj_id"
	h, st := newHandlerCoverageHarness(t, projectID)

	t.Run("GetProject", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleGetProject(rec, coverageRequest("GET", "/me/project", "", projectID))
		body := decodeCoverageBody(t, rec)
		if rec.Code != 200 || body["project_id"] != projectID || body["owner_email"] != "owner@example.com" {
			t.Fatalf("get project = %d %v, want the identity echo", rec.Code, body)
		}
	})

	t.Run("GetMeLegacyAdminFallback", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleGetMe(rec, coverageRequest("GET", "/me", "", projectID))
		body := decodeCoverageBody(t, rec)
		if rec.Code != 200 || body["role"] != "admin" || body["email"] != "owner@example.com" {
			t.Fatalf("me = %d %v, want the owner identity with the legacy admin role", rec.Code, body)
		}
	})

	t.Run("PricingInfo", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleGetPricingInfo(rec, coverageRequest("GET", "/me/pricing-info", "", projectID))
		body := decodeCoverageBody(t, rec)
		if rec.Code != 200 || body["pricing_table_version"] == "" {
			t.Fatalf("pricing = %d %v, want a dated table version", rec.Code, body)
		}
		if models, ok := body["supported_models"].([]any); !ok || len(models) == 0 {
			t.Fatalf("supported_models = %v, want a non-empty list", body["supported_models"])
		}
	})

	t.Run("OrgRollupCountsSiblings", func(t *testing.T) {
		if err := st.CreateProject(context.Background(), &store.Project{
			ProjectID: "proj_id_sibling", Name: "sibling", Tier: "hobby",
			OwnerUserID: "owner@example.com", OwnerEmail: "owner@example.com",
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create sibling: %v", err)
		}
		rec := httptest.NewRecorder()
		h.HandleOrgRollup(rec, coverageRequest("GET", "/me/org-rollup", "", projectID))
		body := decodeCoverageBody(t, rec)
		if rec.Code != 200 || body["owner_user_id"] != "owner@example.com" || body["project_count"].(float64) != 2 {
			t.Fatalf("rollup = %d %v, want both sibling projects under one owner", rec.Code, body)
		}
	})

	t.Run("CostByTenant", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleReportCostByTenant(rec, coverageRequest("GET", "/me/reports/cost-by-tenant?since=notatime", "", projectID))
		if rec.Code != 400 {
			t.Fatalf("bad since = %d, want 400", rec.Code)
		}
		rec = httptest.NewRecorder()
		h.HandleReportCostByTenant(rec, coverageRequest("GET", "/me/reports/cost-by-tenant", "", projectID))
		body := decodeCoverageBody(t, rec)
		if rec.Code != 200 || body["count"].(float64) != 0 || body["limit"].(float64) != 25 {
			t.Fatalf("report = %d %v, want the empty window with the default limit", rec.Code, body)
		}
	})
}
