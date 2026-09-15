package api

// Coverage-debt handler batch six: the admin surface. These are the
// founder-facing endpoints whose response types double as the wire
// contract for the admin dashboard, so the tests decode into the
// exported response structs rather than loose maps: a renamed field
// breaks the decode here before it breaks the dashboard. The
// destructive ones pin their guards: the grant delta ceiling, the
// admin-scope confirmation, the self-revoke refusal, and the GDPR
// purge refusing to touch a still-active project.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/store"
)

func adminRequest(method, target, body string) *http.Request {
	if body == "" {
		return httptest.NewRequest(method, target, nil)
	}
	return httptest.NewRequest(method, target, strings.NewReader(body))
}

func TestAdminProjectAndStorageEndpoints(t *testing.T) {
	const projectID = "proj_adm"
	h, st := newHandlerCoverageHarness(t, projectID)
	seedCoverageExecution(t, st, projectID, "exec_adm_1", events.StatusStarted)

	t.Run("ListProjects", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleAdminListProjects(rec, adminRequest("GET", "/admin/projects", ""))
		body := decodeCoverageBody(t, rec)
		if rec.Code != 200 || body["count"].(float64) != 1 {
			t.Fatalf("list = %d %v, want the one project", rec.Code, body)
		}
	})

	t.Run("ProjectDetail", func(t *testing.T) {
		req := adminRequest("GET", "/admin/projects/proj_missing", "")
		req.SetPathValue("id", "proj_missing")
		rec := httptest.NewRecorder()
		h.HandleAdminGetProjectDetail(rec, req)
		if rec.Code != 404 {
			t.Fatalf("missing project detail = %d, want 404", rec.Code)
		}
		req = adminRequest("GET", "/admin/projects/"+projectID, "")
		req.SetPathValue("id", projectID)
		rec = httptest.NewRecorder()
		h.HandleAdminGetProjectDetail(rec, req)
		var detail AdminProjectDetail
		if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || rec.Code != 200 {
			t.Fatalf("detail = %d (%v), want a decodable AdminProjectDetail", rec.Code, err)
		}
		if detail.Project == nil || detail.Project.ProjectID != projectID || len(detail.RecentExecutions) != 1 {
			t.Fatalf("detail = %+v, want the project with its one execution", detail)
		}
	})

	t.Run("ExportProject", func(t *testing.T) {
		req := adminRequest("GET", "/admin/projects/"+projectID+"/export", "")
		req.SetPathValue("id", projectID)
		rec := httptest.NewRecorder()
		h.HandleAdminExportProject(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Disposition"), projectID) {
			t.Fatalf("export = %d disposition %q, want a file download named for the project",
				rec.Code, rec.Header().Get("Content-Disposition"))
		}
		var archive AdminProjectExport
		if err := json.Unmarshal(rec.Body.Bytes(), &archive); err != nil {
			t.Fatalf("decode archive: %v", err)
		}
		if archive.SchemaVersion != 1 || len(archive.Executions) != 1 || archive.Project == nil {
			t.Fatalf("archive = schema %d with %d executions, want the versioned full archive",
				archive.SchemaVersion, len(archive.Executions))
		}
	})

	t.Run("GrantExecutions", func(t *testing.T) {
		grant := func(id, body string) *httptest.ResponseRecorder {
			req := adminRequest("POST", "/admin/projects/"+id+"/grant", body)
			req.SetPathValue("id", id)
			rec := httptest.NewRecorder()
			h.HandleAdminGrantExecutions(rec, req)
			return rec
		}
		if rec := grant(projectID, `{"executions":0}`); rec.Code != 400 {
			t.Fatalf("zero delta with no expiry = %d, want 400", rec.Code)
		}
		if rec := grant(projectID, `{"executions":20000000}`); rec.Code != 400 {
			t.Fatalf("over the 10M guardrail = %d, want 400", rec.Code)
		}
		if rec := grant("proj_missing", `{"executions":100}`); rec.Code != 404 {
			t.Fatalf("missing project = %d, want 404", rec.Code)
		}
		if rec := grant(projectID, `{"executions":50000}`); rec.Code != 200 {
			t.Fatalf("valid grant = %d, want 200", rec.Code)
		}
		p, err := st.GetProject(context.Background(), projectID)
		if err != nil || p.GrantedExecutions != 50_000 {
			t.Fatalf("granted = %d (%v), want the full grant persisted", p.GrantedExecutions, err)
		}
	})

	t.Run("Storage", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleAdminStorage(rec, adminRequest("GET", "/admin/storage", ""))
		var resp AdminStorageResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != 200 {
			t.Fatalf("storage = %d (%v), want a decodable AdminStorageResponse", rec.Code, err)
		}
		if len(resp.Projects) != 1 || resp.Projects[0].Executions != 1 {
			t.Fatalf("storage projects = %+v, want the per-project breakdown", resp.Projects)
		}
	})
}

func TestAdminAPIKeyEndpoints(t *testing.T) {
	const projectID = "proj_admkey"
	h, _ := newHandlerCoverageHarness(t, projectID)

	create := func(body string) (*httptest.ResponseRecorder, map[string]any) {
		rec := httptest.NewRecorder()
		h.HandleAdminCreateAPIKey(rec, adminRequest("POST", "/admin/api-keys", body))
		return rec, decodeCoverageBody(t, rec)
	}
	for name, bad := range map[string]string{
		"missing name":             `{"scope":"customer","project_id":"proj_admkey"}`,
		"admin without confirm":    `{"name":"k","scope":"admin"}`,
		"customer without project": `{"name":"k","scope":"customer"}`,
		"reserved _admin project":  `{"name":"k","scope":"customer","project_id":"_admin"}`,
	} {
		if rec, _ := create(bad); rec.Code != 400 {
			t.Fatalf("%s = %d, want 400", name, rec.Code)
		}
	}
	if rec, _ := create(`{"name":"k","scope":"customer","project_id":"proj_nope"}`); rec.Code != 404 {
		t.Fatalf("unknown project = %d, want 404", rec.Code)
	}
	rec, body := create(`{"name":"coverage admin-minted","scope":"customer","project_id":"proj_admkey"}`)
	if rec.Code != http.StatusCreated || body["secret"] == "" || body["scope"] != "customer" {
		t.Fatalf("valid mint = %d %v, want 201 with the one-time secret", rec.Code, body)
	}
	mintedKeyID := body["key_id"].(string)

	rec = httptest.NewRecorder()
	h.HandleAdminListAPIKeys(rec, adminRequest("GET", "/admin/api-keys", ""))
	body = decodeCoverageBody(t, rec)
	if rec.Code != 200 || body["count"].(float64) != 1 {
		t.Fatalf("admin key list = %d %v, want the minted key", rec.Code, body)
	}
	if strings.Contains(rec.Body.String(), "key_hash") {
		t.Fatalf("admin key list serialized the hash: %s", rec.Body.String())
	}

	revoke := func(id string, ctxKeyID string) *httptest.ResponseRecorder {
		req := adminRequest("DELETE", "/admin/api-keys/"+id, "")
		req.SetPathValue("id", id)
		if ctxKeyID != "" {
			req = req.WithContext(context.WithValue(req.Context(), ctxKeyAdminKeyID, ctxKeyID))
		}
		rec := httptest.NewRecorder()
		h.HandleAdminRevokeAPIKey(rec, req)
		return rec
	}
	if rec := revoke(mintedKeyID, mintedKeyID); rec.Code != 400 {
		t.Fatalf("self-revoke = %d, want the 400 lockout guard", rec.Code)
	}
	if rec := revoke("key-missing", ""); rec.Code != 404 {
		t.Fatalf("missing key = %d, want 404", rec.Code)
	}
	if rec := revoke(mintedKeyID, "key-someone-else"); rec.Code != 200 {
		t.Fatalf("revoke = %d, want 200", rec.Code)
	}
}

func TestAdminAIAnalysesAndCreditEndpoints(t *testing.T) {
	const projectID = "proj_admai"
	h, st := newHandlerCoverageHarness(t, projectID)
	ctx := context.Background()
	seedCoverageExecution(t, st, projectID, "exec_admai_1", events.StatusCrashed)
	if _, err := st.GroupCrashedExecution(ctx, "exec_admai_1", projectID, "admai-sig"); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	groupID := store.DeriveFailureGroupID(projectID, store.FailureClassCrashes, "admai-sig")
	if err := st.SaveFailureGroupAnalysis(ctx, groupID,
		"analysis body", "claude-haiku-4-5", time.Now().UTC(), ""); err != nil {
		t.Fatalf("save analysis: %v", err)
	}

	t.Run("ByProject", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleAdminAIAnalysesByProject(rec, adminRequest("GET", "/admin/ai-analyses-by-project?since=notatime", ""))
		if rec.Code != 400 {
			t.Fatalf("bad since = %d, want 400", rec.Code)
		}
		rec = httptest.NewRecorder()
		h.HandleAdminAIAnalysesByProject(rec, adminRequest("GET", "/admin/ai-analyses-by-project?since=2020-01-01T00:00:00Z", ""))
		var resp AdminAIAnalysesByProjectResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != 200 {
			t.Fatalf("by-project = %d (%v), want a decodable response", rec.Code, err)
		}
		if resp.TotalCount != 1 || len(resp.Projects) != 1 {
			t.Fatalf("resp = %+v, want the one analyzed group counted", resp)
		}
		var row AdminAIAnalysesByProjectRow = resp.Projects[0]
		if row.ProjectID != projectID || row.Count != 1 || row.EstimatedCostUSD != 0.03 {
			t.Fatalf("row = %+v, want the per-analysis cost applied", row)
		}
	})

	t.Run("PerProjectDetail", func(t *testing.T) {
		req := adminRequest("GET", "/admin/projects/"+projectID+"/ai-analyses-detail?since=2020-01-01T00:00:00Z", "")
		req.SetPathValue("id", projectID)
		rec := httptest.NewRecorder()
		h.HandleAdminProjectAIAnalysesDetail(rec, req)
		var resp AdminProjectAIAnalysesDetailResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != 200 {
			t.Fatalf("detail = %d (%v), want a decodable response", rec.Code, err)
		}
		if resp.TotalCount != 1 || len(resp.Groups) != 1 {
			t.Fatalf("resp = %+v, want the one analyzed group", resp)
		}
		var row AdminProjectAIAnalysesDetailRow = resp.Groups[0]
		if row.GroupID != groupID || row.FailureClass != store.FailureClassCrashes || row.AnalyzedAt == "" {
			t.Fatalf("row = %+v, want the analyzed crashes group", row)
		}
	})

	t.Run("TotalsAndFlatList", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleAdminGetAIAnalysesTotals(rec, adminRequest("GET", "/admin/ai-analyses-totals", ""))
		if rec.Code != 200 {
			t.Fatalf("totals = %d (%s), want 200", rec.Code, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		h.HandleAdminListAIAnalyses(rec, adminRequest("GET", "/admin/ai-analyses?limit=9999", ""))
		var resp AdminListAIAnalysesResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != 200 {
			t.Fatalf("flat list = %d (%v), want a decodable response", rec.Code, err)
		}
		if !resp.OK || resp.Limit != 500 {
			t.Fatalf("resp = %+v, want the runaway limit clamped to 500", resp)
		}
	})

	t.Run("CreditSnapshotRoundTrip", func(t *testing.T) {
		snapshot := func(body string) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			h.HandleAdminCreateAnthropicCreditSnapshot(rec, adminRequest("POST", "/admin/anthropic-credit", body))
			return rec
		}
		if rec := snapshot(`{"balance_usd":-1}`); rec.Code != 400 {
			t.Fatalf("negative balance = %d, want 400", rec.Code)
		}
		if rec := snapshot(`{"balance_usd":200000}`); rec.Code != 400 {
			t.Fatalf("typo-sized balance = %d, want 400", rec.Code)
		}
		if rec := snapshot(`{"balance_usd":500,"note":"coverage snapshot"}`); rec.Code != 200 {
			t.Fatalf("valid snapshot = %d, want 200", rec.Code)
		}
		rec := httptest.NewRecorder()
		h.HandleAdminGetAnthropicCredit(rec, adminRequest("GET", "/admin/anthropic-credit", ""))
		var resp AdminAnthropicCreditResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != 200 {
			t.Fatalf("credit = %d (%v), want a decodable response", rec.Code, err)
		}
		if resp.AdminAPIConfigured {
			t.Fatalf("admin API reported configured with no client wired")
		}
		if resp.CreditBalanceRecordedUSD == nil || *resp.CreditBalanceRecordedUSD != 500 {
			t.Fatalf("recorded balance = %v, want the snapshot just written", resp.CreditBalanceRecordedUSD)
		}
		// An unconfigured admin API cannot produce spend buckets.
		var buckets []AdminDailySpendBucket = resp.DailySpend
		if len(buckets) != 0 {
			t.Fatalf("daily spend = %+v, want none without the admin API", buckets)
		}
	})
}

func TestAdminClosedProjectAuditSurface(t *testing.T) {
	const projectID = "proj_admgdpr"
	h, _ := newHandlerCoverageHarness(t, projectID)

	t.Run("SearchRequiresAFilter", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleAdminSearchClosedProjectAudit(rec, adminRequest("GET", "/admin/audit-search", ""))
		if rec.Code != 400 {
			t.Fatalf("filterless search = %d, want 400", rec.Code)
		}
	})

	t.Run("SearchEchoesTheAppliedFilter", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.HandleAdminSearchClosedProjectAudit(rec,
			adminRequest("GET", "/admin/audit-search?project_id="+projectID, ""))
		var resp AdminClosedProjectAuditResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != 200 {
			t.Fatalf("search = %d (%v), want a decodable response", rec.Code, err)
		}
		var rows []AdminClosedProjectAuditRow = resp.Rows
		if resp.ProjectID != projectID || resp.Count != len(rows) {
			t.Fatalf("resp = %+v, want the echoed filter with a consistent count", resp)
		}
	})

	t.Run("PurgeRefusesActiveProject", func(t *testing.T) {
		payload, _ := json.Marshal(AdminGDPRPurgeRequest{Reason: "coverage ticket"})
		req := httptest.NewRequest("POST", "/admin/projects/"+projectID+"/audit-events/purge",
			bytes.NewReader(payload))
		req.SetPathValue("id", projectID)
		rec := httptest.NewRecorder()
		h.HandleAdminGDPRPurgeClosedProjectAudit(rec, req)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("purge of a live project = %d, want the 422 footgun guard", rec.Code)
		}
	})
}
