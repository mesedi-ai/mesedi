// Unit tests for requireEmailVerified — the shared email-verified
// gate that runs on every authenticated request from both auth
// paths (Bearer API key + session cookie).
//
// Coverage:
//   - Customer routes gate correctly on verified/unverified email.
//   - /admin/* routes bypass the gate entirely (the bug this test
//     file was written to prevent: admin login was 403'ing
//     "email_not_verified" because the internal _admin project's
//     OwnerEmail had never been through the customer email-verify
//     flow — a category error, admin auth is not customer
//     onboarding).
//   - The specific exempt endpoint /me/email-verification-status
//     stays bypassed so the dashboard interstitial can poll it.
//   - MESEDI_DISABLE_EMAIL_VERIFY_GATE=1 env-var bypass still
//     wins over every path check (integration-test posture).
//   - Fail-open on transient DB error (missing project or
//     IsEmailVerified error) — a healthy request must not be
//     blocked by a DB blip.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mesedi/backend/internal/store"
)

// stubEmailVerifyStore embeds store.Store so we only implement the
// two methods requireEmailVerified actually calls (GetProject +
// IsEmailVerified). Missing entries + injectable errors let us
// exercise every branch of the gate.
type stubEmailVerifyStore struct {
	store.Store

	// projects keyed by project_id. Missing keys → ErrNotFound
	// (unless getProjectErr is set to a canned error).
	projects       map[string]*store.Project
	getProjectErr  error
	verifiedEmails map[string]bool
	// If non-nil, IsEmailVerified returns this error regardless of
	// what's in verifiedEmails. Exercises the fail-open transient-
	// error branch.
	isVerifiedErr error
}

func (s *stubEmailVerifyStore) GetProject(_ context.Context, projectID string) (*store.Project, error) {
	if s.getProjectErr != nil {
		return nil, s.getProjectErr
	}
	p, ok := s.projects[projectID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return p, nil
}

func (s *stubEmailVerifyStore) IsEmailVerified(_ context.Context, email string) (bool, error) {
	if s.isVerifiedErr != nil {
		return false, s.isVerifiedErr
	}
	return s.verifiedEmails[email], nil
}

// newGateRequest builds a synthetic authenticated request at the
// given path. requireEmailVerified only reads r.URL.Path and the
// request context, so no body / headers needed.
func newGateRequest(path string) *http.Request {
	return httptest.NewRequest(http.MethodGet, path, nil)
}

// runGate is a small wrapper that captures the response writer and
// returns (allowed, statusCode, bodyBytes). Keeps the per-test
// assertion block short.
func runGate(t *testing.T, s store.Store, path, projectID string) (bool, int, []byte) {
	t.Helper()
	req := newGateRequest(path)
	rec := httptest.NewRecorder()
	allowed := requireEmailVerified(rec, req, s, projectID)
	return allowed, rec.Code, rec.Body.Bytes()
}

func TestRequireEmailVerified_CustomerRouteUnverifiedIs403(t *testing.T) {
	t.Parallel()
	s := &stubEmailVerifyStore{
		projects: map[string]*store.Project{
			"proj_customer": {ProjectID: "proj_customer", OwnerEmail: "alice@example.com"},
		},
		verifiedEmails: map[string]bool{
			// alice@example.com deliberately absent → not verified.
		},
	}
	allowed, code, body := runGate(t, s, "/executions", "proj_customer")
	if allowed {
		t.Fatal("expected gate to BLOCK request for unverified customer email; got allowed=true")
	}
	if code != http.StatusForbidden {
		t.Errorf("expected 403; got %d", code)
	}
	// Body must contain the machine-readable error code so the
	// dashboard + SDKs can surface a precise message.
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body not valid JSON: %v — raw=%s", err, string(body))
	}
	if parsed["error"] != "email_not_verified" {
		t.Errorf("expected error=email_not_verified; got %q", parsed["error"])
	}
}

func TestRequireEmailVerified_CustomerRouteVerifiedIsAllowed(t *testing.T) {
	t.Parallel()
	s := &stubEmailVerifyStore{
		projects: map[string]*store.Project{
			"proj_customer": {ProjectID: "proj_customer", OwnerEmail: "alice@example.com"},
		},
		verifiedEmails: map[string]bool{"alice@example.com": true},
	}
	allowed, code, _ := runGate(t, s, "/executions", "proj_customer")
	if !allowed {
		t.Fatalf("expected gate to ALLOW verified customer; got allowed=false code=%d", code)
	}
}

// TestRequireEmailVerified_AdminPathBypassesEvenIfEmailUnverified is
// the regression guard for the bug that motivated this test file.
// The internal _admin project's OwnerEmail was never routed through
// the customer email-verify flow, so admin login 403'd on every
// authenticated request. Admin routes must bypass the gate.
func TestRequireEmailVerified_AdminPathBypassesEvenIfEmailUnverified(t *testing.T) {
	t.Parallel()
	s := &stubEmailVerifyStore{
		projects: map[string]*store.Project{
			store.APIKeyAdminProjectID: {ProjectID: store.APIKeyAdminProjectID, OwnerEmail: "admin@example.com"},
		},
		verifiedEmails: map[string]bool{
			// admin@example.com deliberately absent.
		},
	}
	// Every admin sub-path we know about.
	adminPaths := []string{
		"/admin/projects",
		"/admin/storage",
		"/admin/abuse",
		"/admin/api-keys",
		"/admin/audit",
		"/admin/whoami",
	}
	for _, p := range adminPaths {
		p := p
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			allowed, code, _ := runGate(t, s, p, store.APIKeyAdminProjectID)
			if !allowed {
				t.Errorf("expected admin path %s to BYPASS gate; got allowed=false code=%d", p, code)
			}
		})
	}
}

// TestRequireEmailVerified_AdminPrefixIsExact prevents an
// over-broad prefix match. "/administrator" or a customer route
// like "/orgs/admin/foo" must NOT be exempt — only paths that begin
// exactly with "/admin/".
func TestRequireEmailVerified_AdminPrefixIsExact(t *testing.T) {
	t.Parallel()
	s := &stubEmailVerifyStore{
		projects: map[string]*store.Project{
			"proj_customer": {ProjectID: "proj_customer", OwnerEmail: "alice@example.com"},
		},
		verifiedEmails: map[string]bool{
			// alice@example.com deliberately absent → not verified.
		},
	}
	// Paths that look admin-adjacent but are NOT admin routes and
	// therefore must still gate.
	notAdmin := []string{
		"/administrator",  // does NOT start with "/admin/"
		"/admin",          // bare "/admin" (no trailing slash) — not exempt
		"/orgs/admin/foo", // "admin" segment inside customer path
	}
	for _, p := range notAdmin {
		p := p
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			allowed, code, _ := runGate(t, s, p, "proj_customer")
			if allowed {
				t.Errorf("path %s must NOT bypass the gate (only /admin/ prefix is exempt); got allowed=true code=%d", p, code)
			}
		})
	}
}

// TestRequireEmailVerified_StatusEndpointExempt guards the one
// customer-facing exempt path — the dashboard's poll endpoint that
// the verify interstitial uses to detect when the user has clicked
// the magic link in another tab. If this regresses the interstitial
// loops forever.
func TestRequireEmailVerified_StatusEndpointExempt(t *testing.T) {
	t.Parallel()
	s := &stubEmailVerifyStore{
		projects: map[string]*store.Project{
			"proj_customer": {ProjectID: "proj_customer", OwnerEmail: "alice@example.com"},
		},
		verifiedEmails: map[string]bool{
			// alice@example.com deliberately absent → not verified.
		},
	}
	allowed, code, _ := runGate(t, s, "/me/email-verification-status", "proj_customer")
	if !allowed {
		t.Errorf("expected /me/email-verification-status to bypass gate; got allowed=false code=%d", code)
	}
}

// TestRequireEmailVerified_EnvBypass exercises the integration-test
// escape hatch. When MESEDI_DISABLE_EMAIL_VERIFY_GATE=1 the gate
// skips regardless of path, project, or verification state.
func TestRequireEmailVerified_EnvBypass(t *testing.T) {
	// Intentionally NOT t.Parallel — this test mutates process env.
	t.Setenv("MESEDI_DISABLE_EMAIL_VERIFY_GATE", "1")
	s := &stubEmailVerifyStore{
		projects: map[string]*store.Project{
			"proj_customer": {ProjectID: "proj_customer", OwnerEmail: "alice@example.com"},
		},
		verifiedEmails: map[string]bool{},
	}
	allowed, _, _ := runGate(t, s, "/executions", "proj_customer")
	if !allowed {
		t.Errorf("MESEDI_DISABLE_EMAIL_VERIFY_GATE=1 should bypass gate for customer routes; got allowed=false")
	}
}

// TestRequireEmailVerified_MissingProjectFailsOpen guards the
// documented fail-open posture: a transient DB error (project row
// not found) must NOT lock a healthy verified customer out. The
// project-suspended check already ran upstream in authViaBearer;
// the gate here is best-effort.
func TestRequireEmailVerified_MissingProjectFailsOpen(t *testing.T) {
	t.Parallel()
	s := &stubEmailVerifyStore{
		projects: map[string]*store.Project{},
		// getProjectErr not set — GetProject returns ErrNotFound.
	}
	allowed, code, _ := runGate(t, s, "/executions", "proj_missing")
	if !allowed {
		t.Errorf("missing project must fail open (allow request); got allowed=false code=%d", code)
	}
}

// TestRequireEmailVerified_IsEmailVerifiedErrorFailsOpen — same
// posture for the second lookup failing.
func TestRequireEmailVerified_IsEmailVerifiedErrorFailsOpen(t *testing.T) {
	t.Parallel()
	s := &stubEmailVerifyStore{
		projects: map[string]*store.Project{
			"proj_customer": {ProjectID: "proj_customer", OwnerEmail: "alice@example.com"},
		},
		isVerifiedErr: errors.New("connection reset by peer"),
	}
	allowed, code, _ := runGate(t, s, "/executions", "proj_customer")
	if !allowed {
		t.Errorf("IsEmailVerified error must fail open; got allowed=false code=%d", code)
	}
}

// Compile-time sanity: ensures the /admin/ prefix constant hasn't
// been renamed underneath us. Any future refactor of
// emailVerifyExemptPathPrefixes will trip this if /admin/ is
// dropped or renamed without updating the test intent.
func TestRequireEmailVerified_AdminPrefixIsRegistered(t *testing.T) {
	t.Parallel()
	found := false
	for _, prefix := range emailVerifyExemptPathPrefixes {
		if prefix == "/admin/" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("/admin/ prefix missing from emailVerifyExemptPathPrefixes; " +
			"admin login will 403 on any project whose OwnerEmail is unverified")
	}
	// Also assert the prefix has a trailing slash — a bare "/admin"
	// would over-match "/administrator" and any customer path
	// containing "admin" as a substring.
	for _, prefix := range emailVerifyExemptPathPrefixes {
		if !strings.HasSuffix(prefix, "/") {
			t.Errorf("prefix %q lacks trailing slash; risks over-matching customer routes", prefix)
		}
	}
}
