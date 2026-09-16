package api

// Coverage-debt handler batch two: budget ceiling, retention read,
// and class-severity overrides. The ceiling endpoints pin the
// owner-keyed contract (a project with no owner cannot configure a
// tenant ceiling and reads an empty state, never an error), the
// retention read pins the hobby tier caps the dashboard renders, and
// the severity trio pins the override lifecycle including the
// idempotent delete that reverts to the compiled-in default.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mesedi/backend/internal/store"
)

func TestBudgetCeilingAndRetentionEndpoints(t *testing.T) {
	const projectID = "proj_bc"
	h, st := newHandlerCoverageHarness(t, projectID)

	t.Run("CeilingEmptyStateIs404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleGetBudgetCeiling(rec, coverageRequest("GET", "/me/budget-ceiling", "", projectID))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("unconfigured ceiling = %d, want the 404 empty state", rec.Code)
		}
	})

	t.Run("CeilingValidation", func(t *testing.T) {
		for _, bad := range []string{
			`{"monthly_ceiling_usd":0,"breach_action":"warn"}`,
			`{"monthly_ceiling_usd":100,"breach_action":"explode"}`,
		} {
			rec := httptest.NewRecorder()
			h.HandleUpsertBudgetCeiling(rec, coverageRequest("PUT", "/me/budget-ceiling", bad, projectID))
			if rec.Code != 400 {
				t.Fatalf("upsert %s = %d, want 400", bad, rec.Code)
			}
		}
	})

	t.Run("CeilingRoundTrip", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleUpsertBudgetCeiling(rec, coverageRequest("PUT", "/me/budget-ceiling",
			`{"monthly_ceiling_usd":1000,"breach_action":"halt"}`, projectID))
		if rec.Code != 200 {
			t.Fatalf("upsert = %d (%s), want 200", rec.Code, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		h.HandleGetBudgetCeiling(rec, coverageRequest("GET", "/me/budget-ceiling", "", projectID))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "halt") {
			t.Fatalf("read back = %d (%s), want the saved halt action", rec.Code, rec.Body.String())
		}
	})

	t.Run("OwnerlessProjectCannotConfigure", func(t *testing.T) {
		if err := st.CreateProject(context.Background(), &store.Project{
			ProjectID: "proj_bc_noowner", Name: "no owner", Tier: "hobby",
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create ownerless project: %v", err)
		}
		rec := httptest.NewRecorder()
		h.HandleUpsertBudgetCeiling(rec, coverageRequest("PUT", "/me/budget-ceiling",
			`{"monthly_ceiling_usd":100,"breach_action":"warn"}`, "proj_bc_noowner"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("ownerless upsert = %d, want 403", rec.Code)
		}
		rec = httptest.NewRecorder()
		h.HandleGetBudgetCeiling(rec, coverageRequest("GET", "/me/budget-ceiling", "", "proj_bc_noowner"))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("ownerless get = %d, want the quiet 404, not an error", rec.Code)
		}
	})

	t.Run("RetentionSurfacesHobbyCaps", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleGetRetention(rec, coverageRequest("GET", "/me/retention", "", projectID))
		body := decodeCoverageBody(t, rec)
		if rec.Code != 200 || body["tier"] != "hobby" ||
			body["max_days"].(float64) != 7 || body["allow_indefinite"] != false {
			t.Fatalf("retention = %d %v, want the hobby caps the dashboard clamps on", rec.Code, body)
		}
	})
}

func TestClassSeverityOverrideLifecycle(t *testing.T) {
	const projectID = "proj_sev"
	h, _ := newHandlerCoverageHarness(t, projectID)

	list := func() (int, map[string]any) {
		rec := httptest.NewRecorder()
		h.HandleListClassSeverities(rec, coverageRequest("GET", "/me/class-severities", "", projectID))
		return rec.Code, decodeCoverageBody(t, rec)
	}
	severityOf := func(body map[string]any, class string) (string, bool) {
		for _, raw := range body["classes"].([]any) {
			row := raw.(map[string]any)
			if row["failure_class"] == class {
				return row["severity"].(string), row["is_override"].(bool)
			}
		}
		return "", false
	}

	code, body := list()
	if code != 200 || len(body["classes"].([]any)) != 23 {
		t.Fatalf("list = %d with %d classes, want all 23 registry classes", code, len(body["classes"].([]any)))
	}
	for _, raw := range body["classes"].([]any) {
		if raw.(map[string]any)["is_override"] == true {
			t.Fatalf("fresh project shows an override: %v", raw)
		}
	}
	defaultSev, _ := severityOf(body, "semantic_loop")

	upsert := func(class, payload string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := coverageRequest("PUT", "/me/class-severities/"+class, payload, projectID)
		req.SetPathValue("class", class)
		h.HandleUpsertClassSeverity(rec, req)
		return rec
	}
	if rec := upsert("semantic_loop", `{"severity":"bogus"}`); rec.Code != 400 {
		t.Fatalf("bogus severity = %d, want 400", rec.Code)
	}
	// Unknown class names are refused rather than stored invisibly:
	// before this check, an override under any unlisted string
	// (including the raw storage-level "loops" class) was persisted
	// and then rendered nowhere, so the customer's setting silently
	// did nothing.
	if rec := upsert("loops", `{"severity":"critical"}`); rec.Code != 400 {
		t.Fatalf("unlisted class = %d, want 400 instead of an invisible override", rec.Code)
	}
	if rec := upsert("not_a_class", `{"severity":"critical"}`); rec.Code != 400 {
		t.Fatalf("unknown class = %d, want 400", rec.Code)
	}
	if rec := upsert("semantic_loop", `{"severity":"critical"}`); rec.Code != 200 {
		t.Fatalf("valid upsert = %d, want 200", rec.Code)
	}
	_, body = list()
	if sev, isOverride := severityOf(body, "semantic_loop"); sev != "critical" || !isOverride {
		t.Fatalf("after upsert semantic_loop = (%s, %v), want the critical override", sev, isOverride)
	}

	del := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := coverageRequest("DELETE", "/me/class-severities/semantic_loop", "", projectID)
		req.SetPathValue("class", "semantic_loop")
		h.HandleDeleteClassSeverity(rec, req)
		return rec
	}
	if rec := del(); rec.Code != 200 {
		t.Fatalf("delete = %d, want 200", rec.Code)
	}
	_, body = list()
	if sev, isOverride := severityOf(body, "semantic_loop"); sev != defaultSev || isOverride {
		t.Fatalf("after delete semantic_loop = (%s, %v), want the default back", sev, isOverride)
	}
	// The delete is documented idempotent: deleting a nonexistent
	// override still answers 200 with the default.
	if rec := del(); rec.Code != 200 {
		t.Fatalf("second delete = %d, want the idempotent 200", rec.Code)
	}
}
