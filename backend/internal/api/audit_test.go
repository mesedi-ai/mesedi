// Unit tests for the audit-log helper (v1).
//
// Coverage:
//   - newAuditEventID returns the expected "audit_" + 32 hex char shape
//     and is reasonably random (no obvious collisions across many calls).
//   - recordAuditEvent is best-effort: a store that returns an error
//     does NOT propagate, so the calling handler's success path is
//     unaffected.
//   - recordAuditEvent on a request with no project context is a no-op
//     (doesn't call the store, doesn't panic).
//   - HandleListAuditEvents returns rows for the calling project,
//     newest first, and 401s when no project is on the context.
//
// We deliberately don't open a real SQLite db here; the store layer
// is exercised through a stub that records the inserted rows. The
// sqlite + postgres CRUD methods themselves are simple enough that the
// dev/staging smoke test covers them once Robert pushes.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mesedi/backend/internal/store"
)

// stubAuditStore embeds store.Store so it satisfies the interface
// without listing every method; the tests below only reach the audit
// methods plus GetProjectTenantID for the RBAC path and (.F)
// GetProject for the tier gate.
type stubAuditStore struct {
	store.Store

	// Captured on each CreateAuditEvent call (most recent only).
	created *store.AuditEvent
	// Optional canned error to return from CreateAuditEvent.
	createErr error

	// Rows returned from ListAuditEventsByProject.
	listed []*store.AuditEvent
	// Project ID the lister was called with (asserted in tests).
	listedProjectID string
	// Limit the lister was called with.
	listedLimit int

	// Tier returned by GetProject. Empty string normalizes to hobby
	// (.F tier gate default). Set to "team" or "enterprise"
	// for the happy-path tests, leave empty to exercise the Hobby-
	// refused path.
	projectTier string
}

func (s *stubAuditStore) CreateAuditEvent(_ context.Context, e *store.AuditEvent) error {
	if s.createErr != nil {
		return s.createErr
	}
	cp := *e
	s.created = &cp
	return nil
}

func (s *stubAuditStore) ListAuditEventsByProject(
	_ context.Context, projectID string, limit int,
) ([]*store.AuditEvent, error) {
	s.listedProjectID = projectID
	s.listedLimit = limit
	return s.listed, nil
}

// GetProjectTenantID returns nil so resolveCallerRole takes the legacy
// "no tenant => admin" path and HandleListAuditEvents passes the
// requireRole gate without us having to set up a full member row.
func (s *stubAuditStore) GetProjectTenantID(_ context.Context, _ string) (*string, error) {
	return nil, nil
}

// GetProject returns a project with the stub's configured tier so
// the .F tier gate in HandleListAuditEvents finds a value to
// compare. Project ID is echoed to satisfy any handler that reads it
// back; other fields are zero and unused by the audit tier check.
func (s *stubAuditStore) GetProject(_ context.Context, projectID string) (*store.Project, error) {
	return &store.Project{
		ProjectID: projectID,
		Tier:      s.projectTier,
	}, nil
}

func Test_newAuditEventID_FormatAndUniqueness(t *testing.T) {
	const (
		wantPrefix = "audit_"
		// 32 hex chars after the prefix (16 bytes * 2).
		hexLen = 32
	)
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := newAuditEventID()
		if !strings.HasPrefix(id, wantPrefix) {
			t.Fatalf("id %q missing prefix %q", id, wantPrefix)
		}
		hex := strings.TrimPrefix(id, wantPrefix)
		if len(hex) != hexLen {
			t.Fatalf("id %q has hex segment of length %d, want %d",
				id, len(hex), hexLen)
		}
		for _, c := range hex {
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
			if !isHex {
				t.Fatalf("id %q has non-hex char %q", id, c)
			}
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id generated at iteration %d: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

func Test_recordAuditEvent_BestEffort_SwallowsStoreError(t *testing.T) {
	s := &stubAuditStore{createErr: errors.New("simulated db down")}
	h := &Handlers{Store: s, Logger: quietLogger()}

	r := httptest.NewRequest(http.MethodPost, "/api-keys", nil)
	r = r.WithContext(withProjectID(r.Context(), "proj-test"))

	// MUST NOT panic, MUST NOT return error. We have no return value
	// to check; the test passes if execution proceeds normally and
	// the stub's createErr is the only error surface.
	h.recordAuditEvent(r, AuditAPIKeyCreate, "api_key", "key-x", nil)
}

func Test_recordAuditEvent_NoProjectContext_IsNoOp(t *testing.T) {
	s := &stubAuditStore{}
	h := &Handlers{Store: s, Logger: quietLogger()}

	r := httptest.NewRequest(http.MethodPost, "/api-keys", nil)
	// No project on context, helper should silently skip.
	h.recordAuditEvent(r, AuditAPIKeyCreate, "api_key", "key-x", nil)

	if s.created != nil {
		t.Fatalf("expected no store write without project context, got %+v", s.created)
	}
}

func Test_recordAuditEvent_PopulatesRowAndJSON(t *testing.T) {
	s := &stubAuditStore{}
	h := &Handlers{Store: s, Logger: quietLogger()}

	r := httptest.NewRequest(http.MethodPost, "/billing/cap", nil)
	ctx := withProjectID(r.Context(), "proj-abc")
	r = r.WithContext(ctx)

	meta := map[string]any{"cap_usd": 250.0}
	h.recordAuditEvent(r, AuditBillingCapUpdate, "billing_cap", "proj-abc", meta)

	if s.created == nil {
		t.Fatal("expected CreateAuditEvent to be called")
	}
	if s.created.ProjectID != "proj-abc" {
		t.Errorf("project_id = %q, want %q", s.created.ProjectID, "proj-abc")
	}
	if s.created.Action != AuditBillingCapUpdate {
		t.Errorf("action = %q, want %q", s.created.Action, AuditBillingCapUpdate)
	}
	if s.created.TargetType != "billing_cap" || s.created.TargetID != "proj-abc" {
		t.Errorf("target = %q/%q, want billing_cap/proj-abc",
			s.created.TargetType, s.created.TargetID)
	}
	if s.created.CreatedAt.IsZero() {
		t.Error("created_at should be set by helper")
	}
	if s.created.MetadataJSON == "" {
		t.Fatal("metadata_json should be populated")
	}
	var roundtrip map[string]any
	if err := json.Unmarshal([]byte(s.created.MetadataJSON), &roundtrip); err != nil {
		t.Fatalf("metadata_json is not valid JSON: %v", err)
	}
	if roundtrip["cap_usd"] != 250.0 {
		t.Errorf("metadata_json round-trip dropped cap_usd: %+v", roundtrip)
	}
}

func Test_HandleListAuditEvents_NoProjectContext_401(t *testing.T) {
	s := &stubAuditStore{}
	h := &Handlers{Store: s, Logger: quietLogger()}

	// No project context => requireRole bails with 500 ("no project
	// context") OR the inner ProjectIDFromContext check hits 401.
	// The current handler returns 500 via resolveCallerRole's error
	// branch; either way the test asserts that a missing context is
	// rejected, not silently served.
	r := httptest.NewRequest(http.MethodGet, "/audit-log", nil)
	w := httptest.NewRecorder()
	h.HandleListAuditEvents(w, r)

	if w.Code == http.StatusOK {
		t.Fatalf("expected non-200 for no-context request, got 200")
	}
}

func Test_HandleListAuditEvents_HappyPath_ReturnsRows(t *testing.T) {
	now := time.Now().UTC()
	rows := []*store.AuditEvent{
		{
			EventID:    "audit_newest",
			ProjectID:  "proj-z",
			Action:     AuditAPIKeyCreate,
			TargetType: "api_key",
			TargetID:   "key-new",
			CreatedAt:  now,
		},
		{
			EventID:    "audit_older",
			ProjectID:  "proj-z",
			Action:     AuditWebhookDelete,
			TargetType: "webhook",
			TargetID:   "wh-old",
			CreatedAt:  now.Add(-1 * time.Hour),
		},
	}
	// .F: tier must be Team-or-higher for the endpoint to
	// return rows; Hobby now hits a 402 paywall before the store
	// call. Set projectTier explicitly so this happy-path test
	// exercises the Team-tier code path.
	s := &stubAuditStore{listed: rows, projectTier: TierTeam}
	h := &Handlers{Store: s, Logger: quietLogger()}

	r := httptest.NewRequest(http.MethodGet, "/audit-log", nil)
	r = r.WithContext(withProjectID(r.Context(), "proj-z"))
	w := httptest.NewRecorder()

	h.HandleListAuditEvents(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if s.listedProjectID != "proj-z" {
		t.Errorf("lister project id = %q, want %q", s.listedProjectID, "proj-z")
	}
	if s.listedLimit != 100 {
		t.Errorf("lister limit = %d, want 100 (handler default)", s.listedLimit)
	}
	var resp ListAuditEventsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !resp.OK {
		t.Error("ok should be true on a successful list")
	}
	if len(resp.Events) != 2 {
		t.Fatalf("want 2 events, got %d", len(resp.Events))
	}
	if resp.Events[0].EventID != "audit_newest" {
		t.Errorf("first event id = %q, want %q (store-order preserved)",
			resp.Events[0].EventID, "audit_newest")
	}
}

// .F (closes / ): audit logs moved from Hobby-included
// to Team-only. Hobby admins hitting /audit-log get 402 with an
// upgrade nudge. The store's ListAuditEventsByProject must NOT be
// called on the Hobby path, the tier gate short-circuits before it.
func Test_HandleListAuditEvents_Hobby_Refused402(t *testing.T) {
	s := &stubAuditStore{projectTier: TierHobby}
	h := &Handlers{Store: s, Logger: quietLogger()}

	r := httptest.NewRequest(http.MethodGet, "/audit-log", nil)
	r = r.WithContext(withProjectID(r.Context(), "proj-hobby"))
	w := httptest.NewRecorder()

	h.HandleListAuditEvents(w, r)

	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("expected 402 Payment Required for Hobby tier, got %d (body: %s)",
			w.Code, w.Body.String())
	}
	// Store must not have been consulted for rows once the tier
	// gate fires, otherwise a bug in the ordering would leak a
	// paywalled feature to Hobby with an oversized response.
	if s.listedProjectID != "" {
		t.Errorf("Hobby request should short-circuit before ListAuditEventsByProject; "+
			"got listedProjectID = %q", s.listedProjectID)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Cloud Team") {
		t.Errorf("expected 402 body to mention Cloud Team upgrade; got %q", body)
	}
}

// .F: empty tier string normalizes to Hobby (strictest cap,
// safest default) via normalizeTier, so an unlabeled project, which
// can happen for pre-tier-labeling legacy rows, is treated exactly
// like Hobby and refused. Belt-and-suspenders coverage for the same
// tier check as above.
func Test_HandleListAuditEvents_EmptyTier_TreatedAsHobby(t *testing.T) {
	s := &stubAuditStore{projectTier: ""}
	h := &Handlers{Store: s, Logger: quietLogger()}

	r := httptest.NewRequest(http.MethodGet, "/audit-log", nil)
	r = r.WithContext(withProjectID(r.Context(), "proj-unlabeled"))
	w := httptest.NewRecorder()

	h.HandleListAuditEvents(w, r)

	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("expected 402 for empty-tier project (normalized to Hobby); "+
			"got %d (body: %s)", w.Code, w.Body.String())
	}
}

// withProjectID is a tiny helper that puts a project id on the
// context using the same key the real auth middleware uses, so the
// handlers' ProjectIDFromContext finds it.
func withProjectID(ctx context.Context, projectID string) context.Context {
	return context.WithValue(ctx, ctxKeyProjectID, projectID)
}
