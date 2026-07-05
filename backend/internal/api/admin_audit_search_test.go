// Unit tests for HandleAdminSearchClosedProjectAudit.
//
// Coverage:
//   - Missing both email + project_id returns 400 (the store would
//     also refuse, but we fail fast at the edge with a clearer
//     message).
//   - Bad limit (non-numeric or negative) returns 400.
//   - Happy path: filter passed to store verbatim, rows projected to
//     wire shape, project_deleted_at + metadata_json formatted.
//   - Store-error path returns 500 with the error message in body
//     so operators don't have to grep server logs to triage.
//   - Default limit echo: pass 0 → response Limit is 100 (handler
//     convention, matches store-side default).
//
// We don't open a real SQLite db here. The stub records the
// filter the handler passed in and returns canned rows; that's
// enough to verify the handler-level contract. Store-level
// behavior (filter combination semantics, ordering) is tested in
// the migration smoke test Robert runs against staging.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mesedi/backend/internal/store"
)

// stubClosedAuditStore embeds store.Store to satisfy the interface
// without implementing every method. The handler only reaches
// SearchClosedProjectAuditEvents; everything else stays nil and
// would panic if accidentally invoked (which is what we want for
// a focused test).
type stubClosedAuditStore struct {
	store.Store

	gotFilter store.ClosedProjectAuditFilter
	rows      []*store.AuditEvent
	err       error
}

func (s *stubClosedAuditStore) SearchClosedProjectAuditEvents(
	_ context.Context, filter store.ClosedProjectAuditFilter,
) ([]*store.AuditEvent, error) {
	s.gotFilter = filter
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

func Test_HandleAdminSearchClosedProjectAudit_NoFilter_400(t *testing.T) {
	s := &stubClosedAuditStore{}
	h := &Handlers{Store: s, Logger: quietLogger()}

	r := httptest.NewRequest(http.MethodGet, "/admin/audit-events", nil)
	w := httptest.NewRecorder()

	h.HandleAdminSearchClosedProjectAudit(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (body: %s)", w.Code, w.Body.String())
	}
	if s.gotFilter.Email != "" || s.gotFilter.ProjectID != "" {
		t.Errorf("store should not be called when no filter is supplied; "+
			"got filter %+v", s.gotFilter)
	}
}

func Test_HandleAdminSearchClosedProjectAudit_BadLimit_400(t *testing.T) {
	cases := []string{"abc", "-5"}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			s := &stubClosedAuditStore{}
			h := &Handlers{Store: s, Logger: quietLogger()}

			r := httptest.NewRequest(http.MethodGet,
				"/admin/audit-events?email=x@y.com&limit="+raw, nil)
			w := httptest.NewRecorder()

			h.HandleAdminSearchClosedProjectAudit(w, r)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("want 400 for limit=%q, got %d (body: %s)",
					raw, w.Code, w.Body.String())
			}
		})
	}
}

func Test_HandleAdminSearchClosedProjectAudit_HappyPath(t *testing.T) {
	deletedAt := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	createdAt := time.Date(2026, 4, 30, 23, 59, 0, 0, time.UTC)
	rows := []*store.AuditEvent{
		{
			EventID:             "audit_abc",
			ProjectID:           "proj-closed",
			ProjectNameSnapshot: "Closed Project Name",
			ProjectDeletedAt:    &deletedAt,
			ActorEmail:          "owner@example.com",
			Action:              AuditBillingAccountClose,
			TargetType:          "project",
			TargetID:            "proj-closed",
			MetadataJSON:        `{"tier":"hobby","had_subscription":false}`,
			CreatedAt:           createdAt,
		},
	}
	s := &stubClosedAuditStore{rows: rows}
	h := &Handlers{Store: s, Logger: quietLogger()}

	r := httptest.NewRequest(http.MethodGet,
		"/admin/audit-events?email=owner%40example.com&limit=50", nil)
	w := httptest.NewRecorder()

	h.HandleAdminSearchClosedProjectAudit(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if s.gotFilter.Email != "owner@example.com" {
		t.Errorf("filter.Email = %q, want %q",
			s.gotFilter.Email, "owner@example.com")
	}
	if s.gotFilter.Limit != 50 {
		t.Errorf("filter.Limit = %d, want 50", s.gotFilter.Limit)
	}

	var resp AdminClosedProjectAuditResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.Count != 1 || len(resp.Rows) != 1 {
		t.Fatalf("want 1 row, got count=%d len=%d", resp.Count, len(resp.Rows))
	}
	row := resp.Rows[0]
	if row.EventID != "audit_abc" {
		t.Errorf("EventID = %q, want audit_abc", row.EventID)
	}
	if row.ProjectNameSnapshot != "Closed Project Name" {
		t.Errorf("ProjectNameSnapshot = %q, want %q",
			row.ProjectNameSnapshot, "Closed Project Name")
	}
	if row.ProjectDeletedAt == "" {
		t.Error("ProjectDeletedAt should be populated when source row has it")
	}
	if row.Metadata["tier"] != "hobby" {
		t.Errorf("metadata.tier = %v, want \"hobby\" (json round-trip): %+v",
			row.Metadata["tier"], row.Metadata)
	}
	if resp.Email != "owner@example.com" {
		t.Errorf("echoed Email = %q, want %q",
			resp.Email, "owner@example.com")
	}
}

func Test_HandleAdminSearchClosedProjectAudit_DefaultLimitEcho(t *testing.T) {
	s := &stubClosedAuditStore{rows: nil}
	h := &Handlers{Store: s, Logger: quietLogger()}

	r := httptest.NewRequest(http.MethodGet,
		"/admin/audit-events?project_id=proj-closed", nil)
	w := httptest.NewRecorder()

	h.HandleAdminSearchClosedProjectAudit(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	var resp AdminClosedProjectAuditResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.Limit != 100 {
		t.Errorf("echoed Limit = %d, want 100 (handler default)", resp.Limit)
	}
	if resp.ProjectID != "proj-closed" {
		t.Errorf("echoed ProjectID = %q, want proj-closed", resp.ProjectID)
	}
}

func Test_HandleAdminSearchClosedProjectAudit_StoreError_500(t *testing.T) {
	s := &stubClosedAuditStore{err: errors.New("simulated db down")}
	h := &Handlers{Store: s, Logger: quietLogger()}

	r := httptest.NewRequest(http.MethodGet,
		"/admin/audit-events?email=foo@bar.com", nil)
	w := httptest.NewRecorder()

	h.HandleAdminSearchClosedProjectAudit(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d (body: %s)", w.Code, w.Body.String())
	}
}
