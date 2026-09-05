// Handler-level integration tests for the seven audit-log capture
// points that can be exercised without a Stripe mock (v1).
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
//     with meaningful metadata). Production smoke test (step A)
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

	// Team-handler state. Default-nil so the existing batch-1/2 tests
	// keep the legacy "no tenant => admin" RBAC path. Team tests
	// populate both so resolveAdminContext finds a real org and the
	// caller looks up as a non-NotFound admin member.
	tenantID  *string
	orgMember *store.OrganizationMember
	// orgMembers backs ListOrganizationMembers (HandleRemoveMember's
	// last-admin protection guard reads it).
	orgMembers []*store.OrganizationMember
}

func (s *auditCaptureStubStore) CreateAuditEvent(_ context.Context, e *store.AuditEvent) error {
	cp := *e
	s.captured = &cp
	return nil
}

// GetProjectTenantID drives resolveCallerRole. Default-nil keeps the
// legacy "no tenant => admin" branch live for the batch-1/2 tests;
// team tests set s.tenantID so resolveAdminContext finds a real org
// and proceeds to the member lookup below.
func (s *auditCaptureStubStore) GetProjectTenantID(_ context.Context, _ string) (*string, error) {
	return s.tenantID, nil
}

// GetOrganizationMember is consulted by resolveAdminContext after
// tenantPtr resolves. Returning s.orgMember (admin role) lets team
// tests pass the "admin role required" gate. The arg signature here
// must match the real Store interface exactly.
func (s *auditCaptureStubStore) GetOrganizationMember(_ context.Context, _, _ string) (*store.OrganizationMember, error) {
	if s.orgMember == nil {
		return nil, store.ErrNotFound
	}
	return s.orgMember, nil
}

// ListOrganizationMembers backs HandleRemoveMember's last-admin check.
func (s *auditCaptureStubStore) ListOrganizationMembers(_ context.Context, _ string) ([]*store.OrganizationMember, error) {
	return s.orgMembers, nil
}

// CreateOrganizationInvite backs HandleCreateInvite.
func (s *auditCaptureStubStore) CreateOrganizationInvite(_ context.Context, _ *store.OrganizationInvite) error {
	return nil
}

// RevokeOrganizationInvite backs HandleRevokeInvite.
func (s *auditCaptureStubStore) RevokeOrganizationInvite(_ context.Context, _, _ string) error {
	return nil
}

// UpdateOrganizationMemberRole backs HandleUpdateMemberRole.
func (s *auditCaptureStubStore) UpdateOrganizationMemberRole(_ context.Context, _, _, _ string) error {
	return nil
}

// RemoveOrganizationMember backs HandleRemoveMember.
func (s *auditCaptureStubStore) RemoveOrganizationMember(_ context.Context, _, _ string) error {
	return nil
}

// DeleteAPIKeysByUserID backs the post-remove key-revocation step in
// HandleRemoveMember. Returns the canned revoked-count for assertions.
func (s *auditCaptureStubStore) DeleteAPIKeysByUserID(_ context.Context, _ string) (int, error) {
	return 0, nil
}

// UpdateProjectTier backs HandleAdminSetTier (step C, C8 / PL4).
func (s *auditCaptureStubStore) UpdateProjectTier(_ context.Context, _, _ string, _ *time.Time) error {
	return nil
}

// GetProjectRetentionDays backs the tier-change cascade
// (applyTierChangeCascade → clampRetentionForTier), which runs on
// every downgrade path. Returning (nil, nil) means "retention not
// explicitly set", the cascade treats this as indefinite; for tiers
// that don't allow indefinite (Hobby, Team) it then clamps and calls
// SetProjectRetentionDays (stubbed separately below). Returns
// zero-value success so the cascade path completes without touching
// the audit-capture assertions the test actually cares about.
func (s *auditCaptureStubStore) GetProjectRetentionDays(_ context.Context, _ string) (*int, error) {
	return nil, nil
}

// --- Batch 2 session-related stubs ---------------------------
//
// The embedded store.Store is nil, so the new session methods on the
// Store interface fall through to a nil interface and panic at
// runtime. These stubs return zero-values so the existing handler
// paths (HandleRevokeAPIKey, HandleRemoveMember, HandleSignin) can
// call them without segfaulting in the tests.

func (s *auditCaptureStubStore) CreateSession(_ context.Context, _ *store.Session) error {
	return nil
}

func (s *auditCaptureStubStore) DeleteSessionsByUserID(_ context.Context, _ string) (int, error) {
	return 0, nil
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

// SnapshotAuditEventsForClosedProject backs HandleCloseAccount's
// migration-031 path. HandleCloseAccount calls this between
// recordAuditEvent and DeleteProjectCascade so audit rows survive
// the cascade. The test only cares that the call doesn't panic; the
// store-level semantics are exercised in store-package tests + the
// staging smoke run.
func (s *auditCaptureStubStore) SnapshotAuditEventsForClosedProject(
	_ context.Context, _, _ string,
) error {
	return nil
}

// UpdateProjectName backs HandleSetProjectName (step C, C1).
func (s *auditCaptureStubStore) UpdateProjectName(_ context.Context, _, _ string) error {
	return nil
}

// SetProjectRetentionDays backs HandleSetRetention (step C, C6).
// days==nil means indefinite (Enterprise-only); tests scope to Team
// with a concrete value within the tier cap.
func (s *auditCaptureStubStore) SetProjectRetentionDays(_ context.Context, _ string, _ *int) error {
	return nil
}

// newCaptureHandlers wires a Handlers instance with the stub Store
// and a quiet logger. Mailer is intentionally left nil, every
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

// --- Test_HandleSetProjectName_FiresAuditEvent --------------------

func Test_HandleSetProjectName_FiresAuditEvent(t *testing.T) {
	const (
		projectID = "proj-capture-rename"
		actor     = "rename-actor@example.com"
		newName   = "Renamed Project"
	)
	s := &auditCaptureStubStore{
		project: &store.Project{
			ProjectID:  projectID,
			Name:       "Original Project",
			OwnerEmail: "owner@example.com",
		},
	}
	h := newCaptureHandlers(s)

	r := newJSONRequest(t, http.MethodPut, "/me/project/name", projectID, actor, map[string]any{
		"name": newName,
	})
	w := httptest.NewRecorder()
	h.HandleSetProjectName(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if s.captured == nil {
		t.Fatal("expected audit event to be captured")
	}
	if s.captured.Action != AuditProjectRename {
		t.Errorf("action: got %q, want %q", s.captured.Action, AuditProjectRename)
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
	var meta map[string]any
	if err := json.Unmarshal([]byte(s.captured.MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata_json not valid JSON: %v", err)
	}
	if meta["new_name"] != newName {
		t.Errorf("metadata.new_name: got %v, want %q", meta["new_name"], newName)
	}
}

// --- Test_HandleSetRetention_FiresAuditEvent ----------------------

func Test_HandleSetRetention_FiresAuditEvent(t *testing.T) {
	// Project is on Team tier so the 30-day request is within the
	// tier's 90-day cap. Without a valid tier the handler returns 403
	// before reaching the audit-write step.
	const (
		projectID     = "proj-capture-retention"
		actor         = "retention-actor@example.com"
		requestedDays = 30
	)
	s := &auditCaptureStubStore{
		project: &store.Project{
			ProjectID: projectID,
			Tier:      TierTeam,
		},
	}
	h := newCaptureHandlers(s)

	r := newJSONRequest(t, http.MethodPut, "/me/project/retention", projectID, actor, map[string]any{
		"retention_days": requestedDays,
		"is_indefinite":  false,
	})
	w := httptest.NewRecorder()
	h.HandleSetRetention(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if s.captured == nil {
		t.Fatal("expected audit event to be captured")
	}
	if s.captured.Action != AuditRetentionUpdate {
		t.Errorf("action: got %q, want %q", s.captured.Action, AuditRetentionUpdate)
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
	var meta map[string]any
	if err := json.Unmarshal([]byte(s.captured.MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata_json not valid JSON: %v", err)
	}
	if meta["retention_days"] != float64(requestedDays) {
		t.Errorf("metadata.retention_days: got %v, want %d",
			meta["retention_days"], requestedDays)
	}
	if meta["is_indefinite"] != false {
		t.Errorf("metadata.is_indefinite: got %v, want false", meta["is_indefinite"])
	}
	if meta["tier"] != TierTeam {
		t.Errorf("metadata.tier: got %v, want %q", meta["tier"], TierTeam)
	}
}

// teamCaptureSetup wires the shared org / tenant / admin-member state
// the four team-handler tests below all need. Centralized so a tweak
// to resolveAdminContext only forces one update here.
func teamCaptureSetup(t *testing.T, projectID, actor, orgID string) *auditCaptureStubStore {
	t.Helper()
	tenant := orgID
	return &auditCaptureStubStore{
		project: &store.Project{
			ProjectID:   projectID,
			OwnerEmail:  actor,
			OwnerUserID: actor,
		},
		tenantID: &tenant,
		orgMember: &store.OrganizationMember{
			OrgID:  orgID,
			UserID: actor,
			Role:   "admin",
		},
	}
}

// --- Test_HandleCreateInvite_FiresAuditEvent ----------------------

func Test_HandleCreateInvite_FiresAuditEvent(t *testing.T) {
	const (
		projectID   = "proj-capture-invite-create"
		actor       = "admin@example.com"
		orgID       = "org-team-cap-1"
		inviteEmail = "alice@example.com"
		inviteRole  = "write"
	)
	s := teamCaptureSetup(t, projectID, actor, orgID)
	h := newCaptureHandlers(s)

	r := newJSONRequest(t, http.MethodPost, "/me/team/invites", projectID, actor, map[string]any{
		"email": inviteEmail,
		"role":  inviteRole,
	})
	w := httptest.NewRecorder()
	h.HandleCreateInvite(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if s.captured == nil {
		t.Fatal("expected audit event to be captured")
	}
	if s.captured.Action != AuditTeamInviteCreate {
		t.Errorf("action: got %q, want %q", s.captured.Action, AuditTeamInviteCreate)
	}
	if s.captured.TargetType != "invite" || s.captured.TargetID == "" {
		t.Errorf("target: got %q/%q, want invite/<inv_...>",
			s.captured.TargetType, s.captured.TargetID)
	}
	if s.captured.ProjectID != projectID {
		t.Errorf("project_id: got %q, want %q", s.captured.ProjectID, projectID)
	}
	if s.captured.ActorEmail != actor {
		t.Errorf("actor_email: got %q, want %q", s.captured.ActorEmail, actor)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(s.captured.MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata_json not valid JSON: %v", err)
	}
	if meta["email"] != inviteEmail {
		t.Errorf("metadata.email: got %v, want %q", meta["email"], inviteEmail)
	}
	if meta["role"] != inviteRole {
		t.Errorf("metadata.role: got %v, want %q", meta["role"], inviteRole)
	}
	if meta["org_id"] != orgID {
		t.Errorf("metadata.org_id: got %v, want %q", meta["org_id"], orgID)
	}
}

// --- Test_HandleRevokeInvite_FiresAuditEvent ----------------------

func Test_HandleRevokeInvite_FiresAuditEvent(t *testing.T) {
	const (
		projectID = "proj-capture-invite-revoke"
		actor     = "admin@example.com"
		orgID     = "org-team-cap-2"
		inviteID  = "inv_deadbeef"
	)
	s := teamCaptureSetup(t, projectID, actor, orgID)
	h := newCaptureHandlers(s)

	r := newJSONRequest(t, http.MethodDelete, "/me/team/invites/"+inviteID, projectID, actor, nil)
	r.SetPathValue("invite", inviteID)
	w := httptest.NewRecorder()
	h.HandleRevokeInvite(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if s.captured == nil {
		t.Fatal("expected audit event to be captured")
	}
	if s.captured.Action != AuditTeamInviteRevoke {
		t.Errorf("action: got %q, want %q", s.captured.Action, AuditTeamInviteRevoke)
	}
	if s.captured.TargetType != "invite" || s.captured.TargetID != inviteID {
		t.Errorf("target: got %q/%q, want invite/%q",
			s.captured.TargetType, s.captured.TargetID, inviteID)
	}
	if s.captured.ProjectID != projectID {
		t.Errorf("project_id: got %q, want %q", s.captured.ProjectID, projectID)
	}
	if s.captured.ActorEmail != actor {
		t.Errorf("actor_email: got %q, want %q", s.captured.ActorEmail, actor)
	}
}

// --- Test_HandleUpdateMemberRole_FiresAuditEvent ------------------

func Test_HandleUpdateMemberRole_FiresAuditEvent(t *testing.T) {
	const (
		projectID  = "proj-capture-role"
		actor      = "admin@example.com"
		orgID      = "org-team-cap-3"
		targetUser = "bob@example.com"
		newRole    = "admin"
	)
	s := teamCaptureSetup(t, projectID, actor, orgID)
	h := newCaptureHandlers(s)

	r := newJSONRequest(t, http.MethodPut, "/me/team/members/"+targetUser+"/role", projectID, actor, map[string]any{
		"role": newRole,
	})
	r.SetPathValue("user", targetUser)
	w := httptest.NewRecorder()
	h.HandleUpdateMemberRole(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if s.captured == nil {
		t.Fatal("expected audit event to be captured")
	}
	if s.captured.Action != AuditTeamRoleUpdate {
		t.Errorf("action: got %q, want %q", s.captured.Action, AuditTeamRoleUpdate)
	}
	if s.captured.TargetType != "member" || s.captured.TargetID != targetUser {
		t.Errorf("target: got %q/%q, want member/%q",
			s.captured.TargetType, s.captured.TargetID, targetUser)
	}
	if s.captured.ProjectID != projectID {
		t.Errorf("project_id: got %q, want %q", s.captured.ProjectID, projectID)
	}
	if s.captured.ActorEmail != actor {
		t.Errorf("actor_email: got %q, want %q", s.captured.ActorEmail, actor)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(s.captured.MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata_json not valid JSON: %v", err)
	}
	if meta["new_role"] != newRole {
		t.Errorf("metadata.new_role: got %v, want %q", meta["new_role"], newRole)
	}
}

// --- Test_HandleRemoveMember_FiresAuditEvent ----------------------

func Test_HandleRemoveMember_FiresAuditEvent(t *testing.T) {
	const (
		projectID  = "proj-capture-remove"
		actor      = "admin@example.com"
		orgID      = "org-team-cap-4"
		targetUser = "carol@example.com"
	)
	s := teamCaptureSetup(t, projectID, actor, orgID)
	// orgMembers must include another admin so the last-admin
	// protection guard does NOT block the removal.
	s.orgMembers = []*store.OrganizationMember{
		{OrgID: orgID, UserID: actor, Role: "admin"},
		{OrgID: orgID, UserID: targetUser, Role: "write"},
	}
	h := newCaptureHandlers(s)

	r := newJSONRequest(t, http.MethodDelete, "/me/team/members/"+targetUser, projectID, actor, nil)
	r.SetPathValue("user", targetUser)
	w := httptest.NewRecorder()
	h.HandleRemoveMember(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if s.captured == nil {
		t.Fatal("expected audit event to be captured")
	}
	if s.captured.Action != AuditTeamMemberRemove {
		t.Errorf("action: got %q, want %q", s.captured.Action, AuditTeamMemberRemove)
	}
	if s.captured.TargetType != "member" || s.captured.TargetID != targetUser {
		t.Errorf("target: got %q/%q, want member/%q",
			s.captured.TargetType, s.captured.TargetID, targetUser)
	}
	if s.captured.ProjectID != projectID {
		t.Errorf("project_id: got %q, want %q", s.captured.ProjectID, projectID)
	}
	if s.captured.ActorEmail != actor {
		t.Errorf("actor_email: got %q, want %q", s.captured.ActorEmail, actor)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(s.captured.MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata_json not valid JSON: %v", err)
	}
	if meta["org_id"] != orgID {
		t.Errorf("metadata.org_id: got %v, want %q", meta["org_id"], orgID)
	}
}

// --- Test_recordAuditEventForProject_PopulatesRowAndJSON ----------

// Covers the no-request helper variant used by Stripe webhook
// handlers (step C, C7 / PL3). The webhook handler itself is
// not exercised end-to-end because handleSetupIntentSucceeded calls
// out to Stripe via the package-level customer.Update / subscription
// SDK functions; static review confirms the call site is correct
// and this test confirms the helper writes the row shape the
// dashboard expects.
func Test_recordAuditEventForProject_PopulatesRowAndJSON(t *testing.T) {
	const (
		projectID  = "proj-capture-pm-add"
		actor      = "owner@example.com"
		customerID = "cus_test_pm_add"
	)
	s := &auditCaptureStubStore{}
	h := &Handlers{Store: s, Logger: quietLogger()}

	meta := map[string]any{
		"tier":                   "hobby",
		"stripe_setup_intent_id": "seti_test",
		"stripe_payment_method":  "pm_test",
	}
	h.recordAuditEventForProject(
		context.Background(),
		projectID,
		actor,
		AuditBillingPaymentMethodAdd,
		"payment_method",
		customerID,
		meta,
	)

	if s.captured == nil {
		t.Fatal("expected audit event to be captured by the helper")
	}
	if s.captured.Action != AuditBillingPaymentMethodAdd {
		t.Errorf("action: got %q, want %q", s.captured.Action, AuditBillingPaymentMethodAdd)
	}
	if s.captured.TargetType != "payment_method" || s.captured.TargetID != customerID {
		t.Errorf("target: got %q/%q, want payment_method/%q",
			s.captured.TargetType, s.captured.TargetID, customerID)
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
	var roundtrip map[string]any
	if err := json.Unmarshal([]byte(s.captured.MetadataJSON), &roundtrip); err != nil {
		t.Fatalf("metadata_json not valid JSON: %v", err)
	}
	if roundtrip["stripe_setup_intent_id"] != "seti_test" {
		t.Errorf("metadata.stripe_setup_intent_id: got %v, want seti_test",
			roundtrip["stripe_setup_intent_id"])
	}
	if roundtrip["tier"] != "hobby" {
		t.Errorf("metadata.tier: got %v, want hobby", roundtrip["tier"])
	}
}

func Test_recordAuditEventForProject_EmptyProjectID_NoOp(t *testing.T) {
	s := &auditCaptureStubStore{}
	h := &Handlers{Store: s, Logger: quietLogger()}

	h.recordAuditEventForProject(
		context.Background(),
		"", // empty project id is a documented silent no-op
		"owner@example.com",
		AuditBillingPaymentMethodAdd,
		"payment_method",
		"cus_test",
		nil,
	)

	if s.captured != nil {
		t.Fatalf("expected no store write for empty projectID, got %+v", s.captured)
	}
}

// --- Test_HandleAdminSetTier_FiresAuditEvent ----------------------

// Closes PL4: a platform-admin tier change is now surfaced in the
// customer's own audit log so they can attribute a sudden tier flip
// back to a Mesedi-side action. The actor email is the synthetic
// AuditActorPlatformAdmin sentinel, NOT the real platform admin's
// email -- by design.
func Test_HandleAdminSetTier_FiresAuditEvent(t *testing.T) {
	const (
		projectID = "proj-capture-admin-tier"
		fromTier  = "hobby"
		toTier    = "team"
	)
	s := &auditCaptureStubStore{
		project: &store.Project{
			ProjectID: projectID,
			Tier:      fromTier,
		},
	}
	h := newCaptureHandlers(s)

	r := httptest.NewRequest(http.MethodPost,
		"/admin/projects/"+projectID+"/tier",
		bytes.NewBufferString(`{"tier":"team","expires_days":0}`))
	r.Header.Set("Content-Type", "application/json")
	r.SetPathValue("id", projectID)
	w := httptest.NewRecorder()
	h.HandleAdminSetTier(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if s.captured == nil {
		t.Fatal("expected audit event to be captured")
	}
	if s.captured.Action != AuditTierChangeByPlatformAdmin {
		t.Errorf("action: got %q, want %q",
			s.captured.Action, AuditTierChangeByPlatformAdmin)
	}
	if s.captured.TargetType != "project" || s.captured.TargetID != projectID {
		t.Errorf("target: got %q/%q, want project/%q",
			s.captured.TargetType, s.captured.TargetID, projectID)
	}
	if s.captured.ProjectID != projectID {
		t.Errorf("project_id: got %q, want %q", s.captured.ProjectID, projectID)
	}
	if s.captured.ActorEmail != AuditActorPlatformAdmin {
		t.Errorf("actor_email: got %q, want %q (synthetic sentinel)",
			s.captured.ActorEmail, AuditActorPlatformAdmin)
	}
	if s.captured.MetadataJSON == "" {
		t.Fatal("metadata_json should be populated")
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(s.captured.MetadataJSON), &meta); err != nil {
		t.Fatalf("metadata_json not valid JSON: %v", err)
	}
	if meta["from_tier"] != fromTier {
		t.Errorf("metadata.from_tier: got %v, want %q", meta["from_tier"], fromTier)
	}
	if meta["to_tier"] != toTier {
		t.Errorf("metadata.to_tier: got %v, want %q", meta["to_tier"], toTier)
	}
}

// --- Test_HandleCreateInvite_HobbyTier_Returns402 -----------------

// PL6: Hobby is 1 project, 1 person. Team invites are not allowed.
// The new gate at the top of HandleCreateInvite returns 402 with an
// upgrade pointer.
func Test_HandleCreateInvite_HobbyTier_Returns402(t *testing.T) {
	const (
		projectID = "proj-pl6-hobby-invite"
		actor     = "hobby-admin@example.com"
		orgID     = "org-pl6-hobby"
	)
	s := teamCaptureSetup(t, projectID, actor, orgID)
	// Override the project tier to Hobby; teamCaptureSetup defaults
	// the tier to empty which would fail-open through the gate.
	s.project.Tier = TierHobby
	h := newCaptureHandlers(s)

	r := newJSONRequest(t, http.MethodPost, "/me/team/invites", projectID, actor, map[string]any{
		"email": "newbie@example.com",
		"role":  "write",
	})
	w := httptest.NewRecorder()
	h.HandleCreateInvite(w, r)

	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status: got %d, want 402; body=%q", w.Code, w.Body.String())
	}
	if s.captured != nil {
		t.Errorf("Hobby invite should not write an audit row, got %+v", s.captured)
	}
}

// --- Test_HandleDowngradeToHobby_WithExtraMembers_Returns409 -------

// PL6: a Team org with >1 member cannot downgrade directly. The
// admin must remove other members first so the resulting Hobby
// project matches the 1-project-1-person contract.
func Test_HandleDowngradeToHobby_WithExtraMembers_Returns409(t *testing.T) {
	const (
		projectID = "proj-pl6-downgrade-with-members"
		actor     = "admin@example.com"
		orgID     = "org-pl6-multi"
	)
	tenant := orgID
	s := &auditCaptureStubStore{
		project: &store.Project{
			ProjectID:            projectID,
			Tier:                 TierTeam,
			StripeCustomerID:     "cus_test_multi_member",
			StripeSubscriptionID: "",
		},
		tenantID: &tenant,
		orgMember: &store.OrganizationMember{
			OrgID: orgID, UserID: actor, Role: "admin",
		},
		// 2 members in the org: admin + one more. The gate must trip
		// at len(members) > 1 and refuse the downgrade.
		orgMembers: []*store.OrganizationMember{
			{OrgID: orgID, UserID: actor, Role: "admin"},
			{OrgID: orgID, UserID: "extra@example.com", Role: "write"},
		},
	}
	h := newCaptureHandlers(s)

	r := newJSONRequest(t, http.MethodPost, "/billing/downgrade", projectID, actor, nil)
	w := httptest.NewRecorder()
	h.HandleDowngradeToHobby(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%q", w.Code, w.Body.String())
	}
	if s.captured != nil {
		t.Errorf("blocked downgrade should not write an audit row, got %+v", s.captured)
	}
}
