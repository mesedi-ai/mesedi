// Backend-side validation tests for PagerDuty webhook creation.
//
// The key invariant: HandleCreateWebhook must refuse to persist a
// webhook whose URL is PagerDuty's Events API v2 endpoint if the
// customer didn't supply an integration key (auth_token). PagerDuty
// authenticates every inbound event via a routing_key inside the
// request body, so a PagerDuty webhook without a routing_key would
// silently fail delivery, Mesedi caches nothing about that, and the
// customer would just see "202 accepted, no incident opened" with
// no clue why. Failing loud at create-time prevents this.
//
// We piggyback on the audit-capture test scaffolding
// (auditCaptureStubStore + newCaptureHandlers + newJSONRequest) so
// the setup here is minimal and matches the shape of the sibling
// Test_HandleCreateWebhook_FiresAuditEvent test.

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func Test_HandleCreateWebhook_PagerDutyRequiresAuthToken(t *testing.T) {
	const (
		projectID = "proj-pd-required"
		actor     = "pd-required@example.com"
	)
	s := &auditCaptureStubStore{}
	h := newCaptureHandlers(s)

	body := map[string]any{
		"url":  "https://events.pagerduty.com/v2/enqueue",
		"name": "pd-hook",
		// auth_token intentionally omitted, this is the specific
		// misconfiguration we want to catch at create-time.
	}
	r := newJSONRequest(t, http.MethodPost, "/webhooks", projectID, actor, body)
	w := httptest.NewRecorder()
	h.HandleCreateWebhook(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(strings.ToLower(w.Body.String()), "pagerduty") {
		t.Errorf("expected error body to mention PagerDuty; got %q", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "auth_token") &&
		!strings.Contains(w.Body.String(), "routing_key") {
		t.Errorf(
			"expected error body to name the required field (auth_token / routing_key); got %q",
			w.Body.String())
	}
	if s.captured != nil {
		t.Error("no audit event should have been recorded on 400 rejection")
	}
}

func Test_HandleCreateWebhook_PagerDutyRejectsShortAuthToken(t *testing.T) {
	const (
		projectID = "proj-pd-short-token"
		actor     = "pd-short@example.com"
	)
	s := &auditCaptureStubStore{}
	h := newCaptureHandlers(s)

	// Length gate: real PagerDuty integration keys are ~32 chars.
	// Anything under 20 is almost certainly a fat-finger paste (a
	// half-copied key, a subscription id, or debug scaffolding).
	body := map[string]any{
		"url":        "https://events.pagerduty.com/v2/enqueue",
		"auth_token": "too-short",
	}
	r := newJSONRequest(t, http.MethodPost, "/webhooks", projectID, actor, body)
	w := httptest.NewRecorder()
	h.HandleCreateWebhook(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(strings.ToLower(w.Body.String()), "length") &&
		!strings.Contains(strings.ToLower(w.Body.String()), "auth_token") {
		t.Errorf("expected error body to mention the length problem; got %q",
			w.Body.String())
	}
}

func Test_HandleCreateWebhook_PagerDutyAcceptsValidAuthToken(t *testing.T) {
	const (
		projectID = "proj-pd-happy"
		actor     = "pd-happy@example.com"
	)
	s := &auditCaptureStubStore{}
	h := newCaptureHandlers(s)

	body := map[string]any{
		"url": "https://events.pagerduty.com/v2/enqueue",
		// 32-char alphanumeric, real PagerDuty integration key shape.
		"auth_token": "abcdef0123456789abcdef0123456789",
		"name":       "pd-hook",
	}
	r := newJSONRequest(t, http.MethodPost, "/webhooks", projectID, actor, body)
	w := httptest.NewRecorder()
	h.HandleCreateWebhook(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if s.captured == nil {
		t.Fatal("expected AuditWebhookCreate to be captured on happy path")
	}
	if s.captured.Action != AuditWebhookCreate {
		t.Errorf("action: got %q, want %q", s.captured.Action, AuditWebhookCreate)
	}
}

func Test_HandleCreateWebhook_NonPagerDutyIgnoresAuthToken(t *testing.T) {
	// Non-PagerDuty URLs should ignore the auth_token field, even a
	// completely empty one, and land 200. Guards against a future
	// refactor that starts requiring auth_token globally.
	const (
		projectID = "proj-non-pd"
		actor     = "non-pd@example.com"
	)
	s := &auditCaptureStubStore{}
	h := newCaptureHandlers(s)

	body := map[string]any{
		"url":  "https://hooks.slack.com/services/T0/B0/xxx",
		"name": "slack-hook",
	}
	r := newJSONRequest(t, http.MethodPost, "/webhooks", projectID, actor, body)
	w := httptest.NewRecorder()
	h.HandleCreateWebhook(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("Slack URL without auth_token should succeed; got %d body=%q",
			w.Code, w.Body.String())
	}
}
