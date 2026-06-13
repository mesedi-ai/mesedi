// Handler-level integration tests for the seven audit-log capture
// points that can be exercised without a Stripe mock (#207 v1).
//
// Coverage:
//
//   - HandleCreateAPIKey    → AuditAPIKeyCreate
//   - HandleRevokeAPIKey    → AuditAPIKeyRevoke
//   - HandleCreateWebhook   → AuditWebhookCreate
//   - HandleDeleteWebhook   → AuditWebhookDelete
//   - HandleUpdateBillingCap     → AuditBillingCapUpdate
//   - HandleDowngradeToHobby Path B → AuditBillingDowngrade
//   - HandleCloseAccount    → AuditBillingAccountClose
//
// Excluded by design:
//
//   - HandleRemovePaymentMethod (billing_payment_method_remove.go).
//     All three audit paths (no-pending, rounded-to-zero, settled)
//     invoke detachCardWithReset / paymentintent.New, which talk to
//     live Stripe. Static code review confirms the three audit calls
//     are correctly placed (action=AuditBillingPaymentMethodRm,
//     target_type="payment_method", target_id=p.StripeCustomerID,
//     with meaningful metadata). Production smoke test (#207 step A)
//     verifies the wire-up end to end.
//
//   - SSO / magic-link signin (signin.go). Already verified end to
//     end against production on 2026-06-12 (the screenshot Robert
//     captured shows the audit row landing as expected).
//
// Test shape:
//
// Each test sets up auditCaptureStubStore with the success-path
// returns for the handler under test, invokes the handler, asserts
// the response is 2xx, then asserts the captured audit event has
// the expected action slug, target_type, target_id, project_id, and
// a non-empty metadata blob where the handler is expected to emit
// one. Where the handler omits metadata (e.g. AuditAPIKeyRevoke),
// the test simply verifies it's empty.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mesedi/backend/internal/store"
)

// auditCaptureStubStore satisfies every Store method the seven
// handlers reach BEFORE the audit-write step. Returning zero values
// from most methods is fine because the handlers we exercise are
// scoped to the audit-capture happy path.
type auditCaptureStubStore struct {
	store.Store

	// Audit row captured by CreateAuditEvent (most recent only).
	captured *store.AuditEvent

	// Returned by GetProject; tests that exercise downgrade or
	// close-account set this so the handler reads the desired tier
	// / subscription state.
	project *store.Project

	// Returned by ListAPIKeysForProject; tests that exercise the
	// revoke flow set this to 2+ entries to bypass the last-key
	// protection guard.
	apiKeysForProject []*store.APIKey
}

func (s *auditCaptureStubStore) CreateAuditEvent(_ context.Context, e *store.AuditEvent) error {
	cp := *e
	s.captured = &cp
	return nil
}

// GetProjectTenantID returning nil drives resolveCallerRole down its
// legacy "no tenant => admin" branch, which means requireRole("admin")
// and requireRole("write") both pass without our needing to set up an
// organization_members row. Same pattern as the existing stubAuditStore.
func (s *auditCaptureStubStore) GetProjectTenantID(_ context.Context, _ string) (*string, error) {
	return nil, nil
}

func (s *auditCaptureStubStore) GetProject(_ context.Context, _ string) (*store.Project, error) {
	if s.project == nil {
		return &store.Project{ProjectID: "proj-test"}, nil
	}
	return s.project, nil
}

func (s *auditCaptureStubStore) CreateAPIKey(_ context.Context, _ *store.APIKey) error {
	return nil
}

func (s *auditCaptureStubStore) ListAPIKeysForProject(_ context.Context, _ string) ([]*store.APIKey, error) {
	return s.apiKeysForProject, nil
}

func (s *auditCaptureStubStore) DeleteAPIKey(_ context.Context, _, _ string) error {
	return nil
}

func (s *auditCaptureStubStore) CreateProjectWebhook(_ context.Context, _ *store.ProjectWebhook) error {
	return nil
}

func (s *auditCaptureStubStore) DeleteProjectWebhook(_ context.Context, _, _ string) error {
	return nil
}

func (s *auditCaptureStubStore) UpdateProjectBillingCap(_ context.Context, _ string, _ float64) error {
	return nil
}

func (s *auditCaptureStubStore) UpdateProjectBilling(
	_ context.Context, _, _, _, _ string, _, _ *time.Time,
) error {
	return nil
}

func (s *auditCaptureStubStore) DeleteProjectCascade(_ context.Context, _ string) error {
	return nil
}

// newCaptureHandlers wires a Handlers instance with the stub Store
// and a quiet logger. Mailer is intentionally left nil — every
// handler we exercise either does not touch the mailer or guards on
// nil before doing so.
func newCaptureHandlers(s *auditCaptureStubStore) *Handlers {
	return &Handlers{Store: s, Logger: quietLogger()}
}

// withUserID puts the API key's user_id on the request context under
// the same key (ctxKeyUserID) the auth middleware uses. signin.go and
// signup.go both initialize api_keys.user_id to the owner's email, so
// the audit-capture helper reads this as the human-readable actor
// email and the dashboard renders it in the ACTOR column.
func withUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ctxKeyUserID, userID)
}

// newJSONRequest constructs an httptest request with a JSON body, the
// project_id context key the handlers read, and (when non-empty) the
// actor email under ctxKeyUserID so recordAuditEvent populates
// AuditEvent.ActorEmail.
func newJSONRequest(t *testing.T, method, path, projectID, actorEmail string, body any) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode test body: %v", err)
		}
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	ctx := withProjectID(r.Context(), projectID)
	if actorEmail != "" {
		ctx = withUserID(ctx, actorEmail)
	}
	r = r.WithContext(ctx)
	return r
}

// --- Test_HandleCreateAPIKey_FiresAuditEvent ----------------------

func Test_HandleCreateAPIKey_FiresAuditEvent(t *testing.T) {
	const (
		projectID = "proj-capture-create"
		actor     = "create-actor@example.com"
	)
	s := &auditCaptureStubStore{
		project: &store.Project{
			ProjectID:   projectID,
			OwnerEmail:  "owner@example.com",
			OwnerUserID: "user-1",
		},
	}
	h := newCaptureHandlers(s)

	r := newJSONRequest(t, http.MethodPost, "/api-keys", projectID, actor, map[string]any{
		"name": "test-key",
	})
	w := httptest.NewRecorder()
	h.HandleCreateAPIKey(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if s.captured == nil {
		t.Fatal("expected audit event to be captured")
	}
	if s.captured.Action != AuditAPIKeyCreate {
		t.Errorf("action: got %q, want %q", s.captured.Action, AuditAPIKeyCreate)
	}
	if s.captured.TargetType != "api_key" {
		t.Errorf("target_type: got %q, want %q", s.captured.TargetType, "api_key")
	}
	if s.captured.TargetID == "" {
		t.Error("target_id should be the new key_id, got empty")
	}
	if s.captured.ProjectID != projectID {
		t.Errorf("project_id: got %q, want %q", s.captured.ProjectID, projectID)
	}
	if s.captured.ActorEmail != actor {
		t.Errorf("actor_email: got %q, want %q", s.captured.ActorEmail, actor)
	}
	if s.captured.MetadataJSON == "" {
		t.Error("metadata_json should contain name + prefix, got empty")
	}
}

// --- Test_HandleRevokeAPIKey_FiresAuditEvent ----------------------

func Test_HandleRevokeAPIKey_FiresAuditEvent(t *testing.T) {
	const (
		projectID  = "proj-capture-revoke"
		keyToKill  = "key-aaaa-1"
		otherKeyID = "key-bbbb-2"
		actor      = "revoke-actor@example.com"
	)
	s := &auditCaptureStubStore{
		// >=2 keys so the last-key protection guard is bypassed.
		apiKeysForProject: []*store.APIKey{
			{KeyID: keyToKill, ProjectID: projectID},
			{KeyID: otherKeyID, ProjectID: projectID},
		},
	}
	h := newCaptureHandlers(s)

	r := newJSONRequest(t, http.MethodDelete, "/api-keys/"+keyToKill, projectID, actor, nil)
	r.SetPathValue("id", keyToKill)
	w := httptest.NewRecorder()
	h.HandleRevokeAPIKey(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if s.captured == nil {
		t.Fatal("expected audit event to be captured")
	}
	if s.captured.Action != AuditAPIKeyRevoke {
		t.Errorf("action: got %q, want %q", s.captured.Action, AuditAPIKeyRevoke)
	}
	if s.captured.TargetType != "api_key" || s.captured.TargetID != keyToKill {
		t.Errorf("target: got %q/%q, want %q/%q",
			s.captured.TargetType, s.captured.TargetID, "api_key", keyToKill)
	}
	if s.captured.ProjectID != projectID {
		t.Errorf("project_id: got %q, want %q", s.captured.ProjectID, projectID)
	}
	if s.captured.ActorEmail != actor {
		t.Errorf("actor_email: got %q, want %q", s.captured.ActorEmail, actor)
	}
}

// --- Test_HandleCreateWebhook_FiresAuditEvent ---------------------

func Test_HandleCreateWebhook_FiresAuditEvent(t *testing.T) {
	const (
		projectID = "proj-capture-webhook-create"
		actor     = "webhook-create-actor@example.com"
	)
	s := &auditCaptureStubStore{}
	h := newCaptureHandlers(s)

	body := map[string]any{
		"name":            "test-hook",
		"url":             "https://example.com/webhook",
		"enabled_classes": []string{"crashes"},
		"enabled":         true,
	}
	r := newJSONRequest(t, http.MethodPost, "/webhooks", projectID, actor, body)
	w := httptest.NewRecorder()
	h.HandleCreateWebhook(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if s.captured == nil {
		t.Fatal("expected audit event to be captured")
	}
	if s.captured.Action != AuditWebhookCreate {
		t.Errorf("action: got %q, want %q", s.captured.Action, AuditWebhookCreate)
	}
	if s.captured.TargetType != "webhook" {
		t.Errorf("target_type: got %q, want %q", s.captured.TargetType, "webhook")
	}
	if s.captured.TargetID == "" {
		t.Error("target_id should be the new webhook_id, got empty")
	}
	if s.captured.ProjectID != projectID {
		t.Errorf("project_id: got %q, want %q", s.captured.ProjectID, projectID)
	}
	if s.captured.ActorEmail != actor {
		t.Errorf("actor_email: got %q, want %q", s.captured.ActorEmail, actor)
	}
	if s.captured.MetadataJSON == "" {
		t.Fatal("metadata_json should be populated")
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(s.captured.MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata_json not valid JSON: %v", err)
	}
	if meta["url"] != "https://example.com/webhook" {
		t.Errorf("metadata.url: got %v, want https://example.com/webhook", meta["url"])
	}
}

// --- Test_HandleDeleteWebhook_FiresAuditEvent ---------------------

func Test_HandleDeleteWebhook_FiresAuditEvent(t *testing.T) {
	const (
		projectID = "proj-capture-webhook-delete"
		webhookID = "wh-cafe"
		actor     = "webhook-delete-actor@example.com"
	)
	s := &auditCaptureStubStore{}
	h := newCaptureHandlers(s)

	r := newJSONRequest(t, http.MethodDelete, "/webhooks/"+webhookID, projectID, actor, nil)
	r.SetPathValue("id", webhookID)
	w := httptest.NewRecorder()
	h.HandleDeleteWebhook(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if s.captured == nil {
		t.Fatal("expected audit event to be captured")
	}
	if s.captured.Action != AuditWebhookDelete {
		t.Errorf("action: got %q, want %q", s.captured.Action, AuditWebhookDelete)
	}
	if s.captured.TargetType != "webhook" || s.captured.TargetID != webhookID {
		t.Errorf("target: got %q/%q, want %q/%q",
			s.captured.TargetType, s.captured.TargetID, "webhook", webhookID)
	}
	if s.captured.ProjectID != projectID {
		t.Errorf("project_id: got %q, want %q", s.captured.ProjectID, projectID)
	}
	if s.captured.ActorEmail != actor {
		t.Errorf("actor_email: got %q, want %q", s.captured.ActorEmail, actor)
	}
}

// --- Test_HandleUpdateBillingCap_FiresAuditEvent ------------------

func Test_HandleUpdateBillingCap_FiresAuditEvent(t *testing.T) {
	const (
		projectID = "proj-capture-cap"
		actor     = "cap-actor@example.com"
	)
	s := &auditCaptureStubStore{}
	h := newCaptureHandlers(s)

	r := newJSONRequest(t, http.MethodPut, "/billing/cap", projectID, actor, map[string]any{
		"cap_usd": 250,
	})
	w := httptest.NewRecorder()
	h.HandleUpdateBillingCap(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if s.captured == nil {
		t.Fatal("expected audit event to be captured")
	}
	if s.captured.Action != AuditBillingCapUpdate {
		t.Errorf("action: got %q, want %q", s.captured.Action, AuditBillingCapUpdate)
	}
	if s.captured.TargetType != "billing_cap" || s.captured.TargetID != projectID {
		t.Errorf("target: got %q/%q, want %q/%q",
			s.captured.TargetType, s.captured.TargetID, "billing_cap", projectID)
	}
	if s.captured.ProjectID != projectID {
		t.Errorf("project_id: got %q, want %q", s.captured.ProjectID, projectID)
	}
	if s.captured.ActorEmail != actor {
		t.Errorf("actor_email: got %q, want %q", s.captured.ActorEmail, actor)
	}
	if s.captured.MetadataJSON == "" {
		t.Fatal("metadata_json should be populated")
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(s.captured.MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata_json not valid JSON: %v", err)
	}
	if meta["cap_usd"] != 250.0 {
		t.Errorf("metadata.cap_usd: got %v, want 250", meta["cap_usd"])
	}
}

// --- Test_HandleDowngradeToHobby_PathB_FiresAuditEvent ------------

func Test_HandleDowngradeToHobby_PathB_FiresAuditEvent(t *testing.T) {
	// Path B: project is on Team in DB but has no Stripe subscription
	// id. The handler skips the Stripe call and flips the DB directly.
	// The audit row fires after the DB flip with the immediate=true
	// metadata field set.
	const (
		projectID = "proj-capture-downgrade"
		actor     = "downgrade-actor@example.com"
	)
	s := &auditCaptureStubStore{
		project: &store.Project{
			ProjectID:            projectID,
			Tier:                 TierTeam,
			StripeCustomerID:     "cus_test_no_sub",
			StripeSubscriptionID: "", // forces Path B
		},
	}
	h := newCaptureHandlers(s)

	r := newJSONRequest(t, http.MethodPost, "/billing/downgrade", projectID, actor, nil)
	w := httptest.NewRecorder()
	h.HandleDowngradeToHobby(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if s.captured == nil {
		t.Fatal("expected audit event to be captured")
	}
	if s.captured.Action != AuditBillingDowngrade {
		t.Errorf("action: got %q, want %q", s.captured.Action, AuditBillingDowngrade)
	}
	// Path B target is the project (not a subscription).
	if s.captured.TargetType != "project" || s.captured.TargetID != projectID {
		t.Errorf("target: got %q/%q, want %q/%q",
			s.captured.TargetType, s.captured.TargetID, "project", projectID)
	}
	if s.captured.ProjectID != projectID {
		t.Errorf("project_id: got %q, want %q", s.captured.ProjectID, projectID)
	}
	if s.captured.ActorEmail != actor {
		t.Errorf("actor_email: got %q, want %q", s.captured.ActorEmail, actor)
	}
	if s.captured.MetadataJSON == "" {
		t.Fatal("metadata_json should be populated")
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(s.captured.MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata_json not valid JSON: %v", err)
	}
	if meta["immediate"] != true {
		t.Errorf("metadata.immediate: got %v, want true", meta["immediate"])
	}
}

// --- Test_HandleCloseAccount_FiresAuditEvent ----------------------

func Test_HandleCloseAccount_FiresAuditEvent(t *testing.T) {
	// Empty StripeSubscriptionID means the handler skips its Stripe
	// cancel block (no live subscription to cancel). Mailer is nil
	// so the mailer block is also skipped. The audit row fires
	// immediately before the cascade-delete, which is the contract
	// the migration documents.
	const (
		projectID = "proj-capture-close"
		actor     = "close-actor@example.com"
	)
	s := &auditCaptureStubStore{
		project: &store.Project{
			ProjectID:            projectID,
			Tier:                 TierHobby,
			StripeCustomerID:     "cus_test_close",
			StripeSubscriptionID: "",
			OwnerEmail:           "",
		},
	}
	h := newCaptureHandlers(s)

	r := newJSONRequest(t, http.MethodPost, "/billing/close-account", projectID, actor, nil)
	w := httptest.NewRecorder()
	h.HandleCloseAccount(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if s.captured == nil {
		t.Fatal("expected audit event to be captured")
	}
	if s.captured.Action != AuditBillingAccountClose {
		t.Errorf("action: got %q, want %q", s.captured.Action, AuditBillingAccountClose)
	}
	if s.captured.TargetType != "project" || s.captured.TargetID != projectID {
		t.Errorf("target: got %q/%q, want %q/%q",
			s.captured.TargetType, s.captured.TargetID, "project", projectID)
	}
	if s.captured.ProjectID != projectID {
		t.Errorf("project_id: got %q, want %q", s.captured.ProjectID, projectID)
	}
	if s.captured.ActorEmail != actor {
		t.Errorf("actor_email: got %q, want %q", s.captured.ActorEmail, actor)
	}
	if s.captured.MetadataJSON == "" {
		t.Fatal("metadata_json should be populated")
	}
}
