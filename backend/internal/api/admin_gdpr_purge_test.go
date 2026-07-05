// Unit tests for HandleAdminGDPRPurgeClosedProjectAudit.
//
// Coverage:
//   - Missing project_id in path returns 400.
//   - Store returns ErrProjectStillActive => handler responds 422
//     with a clear "close the account first" message.
//   - Store returns a generic error => 500.
//   - Happy path: response includes rows_purged + purged_at + the
//     meta-audit-event is recorded against _admin (we verify the
//     CreateAuditEvent call shape).
//   - Empty body is allowed (POST without JSON is a valid spelling).
//   - Idempotency: zero rows deleted is still 200 with rows_purged=0.
//
// We do not exercise the real DB. The store stub captures the
// PurgeAuditEventsForClosedProject + CreateAuditEvent calls and the
// tests assert on what the handler did with the responses.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mesedi/backend/internal/store"
)

// stubGDPRPurgeStore embeds store.Store so it satisfies the
// interface without implementing every method. Only Purge +
// CreateAuditEvent are reached by the handler.
type stubGDPRPurgeStore struct {
	store.Store

	purgeProjectID string
	purgeReturn    int64
	purgeErr       error

	createdAudit *store.AuditEvent
}

func (s *stubGDPRPurgeStore) PurgeAuditEventsForClosedProject(
	_ context.Context, projectID string,
) (int64, error) {
	s.purgeProjectID = projectID
	return s.purgeReturn, s.purgeErr
}

func (s *stubGDPRPurgeStore) CreateAuditEvent(_ context.Context, e *store.AuditEvent) error {
	cp := *e
	s.createdAudit = &cp
	return nil
}

func newPurgeRequest(t *testing.T, projectID string, body string) *http.Request {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(http.MethodPost, "/admin/projects/"+projectID+"/audit-events/purge", nil)
	} else {
		r = httptest.NewRequest(http.MethodPost, "/admin/projects/"+projectID+"/audit-events/purge",
			bytes.NewReader([]byte(body)))
		r.Header.Set("Content-Type", "application/json")
	}
	// Stamp the path-value the way Go's mux populates it.
	r.SetPathValue("id", projectID)
	return r
}

func Test_HandleAdminGDPRPurge_HappyPath_RecordsMetaAuditEvent(t *testing.T) {
	st := &stubGDPRPurgeStore{purgeReturn: 42}
	h := &Handlers{Store: st, Logger: quietLogger()}

	r := newPurgeRequest(t, "proj-closed-abc", `{"reason":"ticket verified"}`)
	// Plant the admin-key context the way AdminAuth would.
	ctx := context.WithValue(r.Context(), ctxKeyAdminAuthMethod, AdminAuthMethodAPIKey)
	ctx = context.WithValue(ctx, ctxKeyAdminKeyID, "key-admin-1")
	ctx = context.WithValue(ctx, ctxKeyAdminKeyName, "robert-admin-laptop")
	r = r.WithContext(ctx)

	w := httptest.NewRecorder()
	h.HandleAdminGDPRPurgeClosedProjectAudit(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if st.purgeProjectID != "proj-closed-abc" {
		t.Errorf("purge called with project_id %q, want proj-closed-abc",
			st.purgeProjectID)
	}

	var resp AdminGDPRPurgeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.RowsPurged != 42 {
		t.Errorf("RowsPurged = %d, want 42", resp.RowsPurged)
	}
	if resp.ProjectID != "proj-closed-abc" {
		t.Errorf("ProjectID = %q, want proj-closed-abc", resp.ProjectID)
	}
	if resp.PurgedAt == "" {
		t.Error("PurgedAt should be populated")
	}
	if resp.PurgedBy != "robert-admin-laptop" {
		t.Errorf("PurgedBy = %q, want robert-admin-laptop", resp.PurgedBy)
	}

	// Meta-audit-event should be attached to the _admin system
	// project with the right action and metadata.
	if st.createdAudit == nil {
		t.Fatal("expected CreateAuditEvent to be called (meta-audit-event)")
	}
	if st.createdAudit.ProjectID != store.APIKeyAdminProjectID {
		t.Errorf("meta-audit-event project_id = %q, want %q (_admin system project)",
			st.createdAudit.ProjectID, store.APIKeyAdminProjectID)
	}
	if st.createdAudit.Action != AuditAuditGDPRPurge {
		t.Errorf("meta-audit-event action = %q, want %q",
			st.createdAudit.Action, AuditAuditGDPRPurge)
	}
	if st.createdAudit.TargetType != "project" {
		t.Errorf("meta-audit-event target_type = %q, want project",
			st.createdAudit.TargetType)
	}
	if st.createdAudit.TargetID != "proj-closed-abc" {
		t.Errorf("meta-audit-event target_id = %q, want proj-closed-abc",
			st.createdAudit.TargetID)
	}
	// Metadata should round-trip the reason + the admin context.
	var md map[string]any
	if err := json.Unmarshal([]byte(st.createdAudit.MetadataJSON), &md); err != nil {
		t.Fatalf("decode metadata_json: %v", err)
	}
	if md["reason"] != "ticket verified" {
		t.Errorf("metadata.reason = %v, want \"ticket verified\"",
			md["reason"])
	}
	if md["rows_purged"].(float64) != 42 {
		t.Errorf("metadata.rows_purged = %v, want 42", md["rows_purged"])
	}
	if md["admin_key_name"] != "robert-admin-laptop" {
		t.Errorf("metadata.admin_key_name = %v, want robert-admin-laptop",
			md["admin_key_name"])
	}
}

func Test_HandleAdminGDPRPurge_LiveProject_422(t *testing.T) {
	st := &stubGDPRPurgeStore{purgeErr: store.ErrProjectStillActive}
	h := &Handlers{Store: st, Logger: quietLogger()}

	r := newPurgeRequest(t, "proj-still-alive", "")
	w := httptest.NewRecorder()
	h.HandleAdminGDPRPurgeClosedProjectAudit(w, r)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for live project, got %d (body: %s)",
			w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "close the account") {
		t.Errorf("422 body should hint at the fix; got %s", w.Body.String())
	}
	if st.createdAudit != nil {
		t.Error("meta-audit-event should NOT be recorded when the purge was refused")
	}
}

func Test_HandleAdminGDPRPurge_MissingProjectID_400(t *testing.T) {
	st := &stubGDPRPurgeStore{}
	h := &Handlers{Store: st, Logger: quietLogger()}

	r := httptest.NewRequest(http.MethodPost,
		"/admin/projects//audit-events/purge", nil)
	// Do not call SetPathValue: empty id is what we are testing.
	w := httptest.NewRecorder()
	h.HandleAdminGDPRPurgeClosedProjectAudit(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for missing project_id, got %d", w.Code)
	}
}

func Test_HandleAdminGDPRPurge_GenericStoreError_500(t *testing.T) {
	st := &stubGDPRPurgeStore{purgeErr: errors.New("simulated db down")}
	h := &Handlers{Store: st, Logger: quietLogger()}

	r := newPurgeRequest(t, "proj-x", "")
	w := httptest.NewRecorder()
	h.HandleAdminGDPRPurgeClosedProjectAudit(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 for generic db error, got %d (body: %s)",
			w.Code, w.Body.String())
	}
}

func Test_HandleAdminGDPRPurge_NoBody_Allowed(t *testing.T) {
	st := &stubGDPRPurgeStore{purgeReturn: 7}
	h := &Handlers{Store: st, Logger: quietLogger()}

	r := newPurgeRequest(t, "proj-quiet", "")
	w := httptest.NewRecorder()
	h.HandleAdminGDPRPurgeClosedProjectAudit(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200 for body-less purge, got %d (body: %s)",
			w.Code, w.Body.String())
	}
}

func Test_HandleAdminGDPRPurge_ZeroDeletes_Is200_Idempotent(t *testing.T) {
	st := &stubGDPRPurgeStore{purgeReturn: 0}
	h := &Handlers{Store: st, Logger: quietLogger()}

	r := newPurgeRequest(t, "proj-already-clean", "")
	w := httptest.NewRecorder()
	h.HandleAdminGDPRPurgeClosedProjectAudit(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200 for zero-row purge (idempotent), got %d (body: %s)",
			w.Code, w.Body.String())
	}
	var resp AdminGDPRPurgeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.RowsPurged != 0 {
		t.Errorf("RowsPurged = %d, want 0 (idempotent re-run)", resp.RowsPurged)
	}
}
