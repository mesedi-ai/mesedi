package api

// Coverage-debt handler batch three: execution ingest and the
// failure-group read/resolve surface. The recurring contract under
// pin here is the anti-probing posture: every cross-tenant read
// answers 404 exactly as a missing id would, so a prober learns
// nothing from the difference.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/store"
)

func TestExecutionIngestAndReads(t *testing.T) {
	const projectID = "proj_ex"
	h, _ := newHandlerCoverageHarness(t, projectID)

	t.Run("CreateExecution", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleCreateExecution(rec, coverageRequest("POST", "/executions", `{"status":"started"}`, projectID))
		if rec.Code != 400 {
			t.Fatalf("missing execution_id = %d, want 400", rec.Code)
		}
		rec = httptest.NewRecorder()
		h.HandleCreateExecution(rec, coverageRequest("POST", "/executions",
			`{"execution_id":"exec_cov_1","project_id":"proj_other"}`, projectID))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("mismatched project = %d, want 403: this catches wrong-key SDK bugs", rec.Code)
		}
		rec = httptest.NewRecorder()
		h.HandleCreateExecution(rec, coverageRequest("POST", "/executions",
			`{"execution_id":"exec_cov_1"}`, projectID))
		body := decodeCoverageBody(t, rec)
		if rec.Code != 200 || body["execution_id"] != "exec_cov_1" || body["status"] != "started" {
			t.Fatalf("create = %d %v, want the started execution echoed", rec.Code, body)
		}
	})

	t.Run("ListExecutions", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleListExecutions(rec, coverageRequest("GET", "/executions?limit=999", "", projectID))
		body := decodeCoverageBody(t, rec)
		if rec.Code != 200 || body["count"].(float64) != 1 || body["limit"].(float64) != 200 {
			t.Fatalf("list = %d %v, want one row and the limit clamped to 200", rec.Code, body)
		}
	})

	t.Run("Topology", func(t *testing.T) {
		req := coverageRequest("GET", "/executions/exec_cov_1/topology?depth=999", "", projectID)
		req.SetPathValue("id", "exec_cov_1")
		rec := httptest.NewRecorder()
		h.HandleGetExecutionTopology(rec, req)
		body := decodeCoverageBody(t, rec)
		if rec.Code != 200 || body["count"].(float64) != 1 || body["depth"].(float64) != 32 {
			t.Fatalf("topology = %d %v, want the single root and depth clamped to 32", rec.Code, body)
		}
	})

	t.Run("Stats", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleStats(rec, coverageRequest("GET", "/stats", "", projectID))
		body := decodeCoverageBody(t, rec)
		if rec.Code != 200 || body["total_executions"].(float64) != 1 ||
			body["crashed_24h"].(float64) != 0 || body["open_failure_groups"].(float64) != 0 {
			t.Fatalf("stats = %d %v, want the seeded counts", rec.Code, body)
		}
	})
}

func TestFailureGroupReadAndResolveSurface(t *testing.T) {
	const projectID = "proj_fg"
	h, st := newHandlerCoverageHarness(t, projectID)
	ctx := context.Background()
	seedCoverageExecution(t, st, projectID, "exec_fg_1", events.StatusCrashed)
	if _, err := st.GroupCrashedExecution(ctx, "exec_fg_1", projectID, "cov-sig"); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	groupID := store.DeriveFailureGroupID(projectID, store.FailureClassCrashes, "cov-sig")
	if err := st.CreateProject(ctx, &store.Project{
		ProjectID: "proj_fg_other", Name: "other tenant", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create other project: %v", err)
	}

	withID := func(fn http.HandlerFunc, method, target, id, asProject string) (*httptest.ResponseRecorder, map[string]any) {
		req := coverageRequest(method, target, "", asProject)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		fn(rec, req)
		return rec, decodeCoverageBody(t, rec)
	}

	t.Run("GetGroup", func(t *testing.T) {
		rec, body := withID(h.HandleGetFailureGroup, "GET", "/failure-groups/"+groupID, groupID, projectID)
		if rec.Code != 200 || body["group_id"] != groupID {
			t.Fatalf("get = %d %v, want the seeded group", rec.Code, body)
		}
		if rec, _ := withID(h.HandleGetFailureGroup, "GET", "/failure-groups/grp-missing", "grp-missing", projectID); rec.Code != 404 {
			t.Fatalf("missing group = %d, want 404", rec.Code)
		}
		// Cross-tenant probe answers exactly like a missing id.
		if rec, _ := withID(h.HandleGetFailureGroup, "GET", "/failure-groups/"+groupID, groupID, "proj_fg_other"); rec.Code != 404 {
			t.Fatalf("cross-tenant get = %d, want the anti-probing 404", rec.Code)
		}
	})

	t.Run("ListExecutionsInGroup", func(t *testing.T) {
		rec, body := withID(h.HandleListExecutionsInFailureGroup, "GET", "/failure-groups/"+groupID+"/executions", groupID, projectID)
		if rec.Code != 200 || body["count"].(float64) != 1 {
			t.Fatalf("list = %d %v, want the one grouped execution", rec.Code, body)
		}
		if rec, _ := withID(h.HandleListExecutionsInFailureGroup, "GET", "/failure-groups/"+groupID+"/executions", groupID, "proj_fg_other"); rec.Code != 404 {
			t.Fatalf("cross-tenant list = %d, want 404", rec.Code)
		}
	})

	t.Run("ResolveUnresolveCycle", func(t *testing.T) {
		listCount := func(query string) float64 {
			rec := httptest.NewRecorder()
			h.HandleListFailureGroups(rec, coverageRequest("GET", "/failure-groups"+query, "", projectID))
			return decodeCoverageBody(t, rec)["count"].(float64)
		}
		if got := listCount(""); got != 1 {
			t.Fatalf("open list = %v, want the seeded group", got)
		}
		rec, body := withID(h.HandleResolveFailureGroup, "POST", "/failure-groups/"+groupID+"/resolve", groupID, projectID)
		if rec.Code != 200 || body["resolved"] != true {
			t.Fatalf("resolve = %d %v, want resolved true", rec.Code, body)
		}
		if got := listCount(""); got != 0 {
			t.Fatalf("open list after resolve = %v, want the group hidden", got)
		}
		if got := listCount("?include_resolved=true"); got != 1 {
			t.Fatalf("include_resolved list = %v, want the group visible", got)
		}
		if rec, _ := withID(h.HandleResolveFailureGroup, "POST", "/failure-groups/"+groupID+"/resolve", groupID, "proj_fg_other"); rec.Code != 404 {
			t.Fatalf("cross-tenant resolve = %d, want 404", rec.Code)
		}
		rec, body = withID(h.HandleUnresolveFailureGroup, "POST", "/failure-groups/"+groupID+"/unresolve", groupID, projectID)
		if rec.Code != 200 || body["resolved"] != false {
			t.Fatalf("unresolve = %d %v, want resolved false", rec.Code, body)
		}
		if got := listCount(""); got != 1 {
			t.Fatalf("open list after unresolve = %v, want the group back", got)
		}
	})
}
