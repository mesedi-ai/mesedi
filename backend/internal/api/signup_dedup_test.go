// Unit tests for the duplicate-account guard in HandleSignup.
//
// Behavior under test (verified-only block): a new signup is rejected
// with 409 ONLY when the email is already verified. This is deliberate
// and the tests pin the reasoning in place:
//
//   - A VERIFIED email cannot mint a second parallel account; it is
//     told to sign in instead. (TestSignupDedup_VerifiedEmailRejected)
//   - An UNVERIFIED email is NOT blocked, so a typo / throwaway / or
//     someone pre-registering a stranger's address can never lock the
//     real owner out, and there is no enumeration oracle on unverified
//     addresses. (TestSignupDedup_UnverifiedEmailPassesGuard)
//   - If the verified-email lookup itself errors, signup FAILS CLOSED
//     (500, no project created) rather than silently creating a
//     possible duplicate. (TestSignupDedup_VerifiedCheckErrorFailsClosed)
//   - The email is normalized (lower + trim) BEFORE the check, so
//     "  DUPE@Example.com " collides with "dupe@example.com".
//     (TestSignupDedup_NormalizesBeforeCheck)

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

// stubSignupStore embeds store.Store so only the two methods the guard
// path touches are implemented. createProjectCalled records whether the
// handler proceeded past the guard into project creation; createProjectErr
// lets a test short-circuit the happy path right after the guard without
// having to stub org bootstrap, retention, key mint, and the mailer.
type stubSignupStore struct {
	store.Store

	verified            map[string]bool
	isVerifiedErr       error
	createProjectCalled bool
	createProjectErr    error
}

func (s *stubSignupStore) IsEmailVerified(_ context.Context, email string) (bool, error) {
	if s.isVerifiedErr != nil {
		return false, s.isVerifiedErr
	}
	return s.verified[email], nil
}

func (s *stubSignupStore) CreateProject(_ context.Context, _ *store.Project) error {
	s.createProjectCalled = true
	if s.createProjectErr != nil {
		return s.createProjectErr
	}
	return nil
}

// postSignup runs HandleSignup with the given JSON body and a unique
// client IP (so the 5/hour IP limiter never interferes across tests).
func postSignup(t *testing.T, s store.Store, body, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handlers{Store: s, Logger: quietLogger()}
	req := httptest.NewRequest(http.MethodPost, "/signup", strings.NewReader(body))
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.HandleSignup(rec, req)
	return rec
}

func TestSignupDedup_VerifiedEmailRejected(t *testing.T) {
	s := &stubSignupStore{verified: map[string]bool{"dupe@example.com": true}}
	rec := postSignup(t, s, `{"email":"dupe@example.com"}`, "203.0.113.10:5000")

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for a verified duplicate; got %d — body=%s", rec.Code, rec.Body.String())
	}
	if s.createProjectCalled {
		t.Error("guard leaked: CreateProject was called for a verified duplicate email (an orphan account would be created)")
	}
	var parsed map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("body not valid JSON: %v — raw=%s", err, rec.Body.String())
	}
	if msg, _ := parsed["error"].(string); !strings.Contains(msg, "already exists") {
		t.Errorf("expected an 'already exists' message; got %q", msg)
	}
}

func TestSignupDedup_UnverifiedEmailPassesGuard(t *testing.T) {
	// Unverified email: the guard must let it through to project
	// creation. We short-circuit CreateProject with a canned error so
	// the handler stops right after the guard (no mailer / key mint
	// needed); reaching CreateProject at all proves the guard passed.
	s := &stubSignupStore{
		verified:         map[string]bool{}, // fresh@ is absent → unverified
		createProjectErr: errors.New("stub: stop after guard"),
	}
	rec := postSignup(t, s, `{"email":"fresh@example.com"}`, "203.0.113.11:5000")

	if !s.createProjectCalled {
		t.Fatalf("guard over-blocked: an UNVERIFIED email never reached CreateProject (code=%d body=%s)", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusConflict {
		t.Errorf("unverified email must not get 409; got %d", rec.Code)
	}
}

func TestSignupDedup_VerifiedCheckErrorFailsClosed(t *testing.T) {
	s := &stubSignupStore{isVerifiedErr: errors.New("db blip")}
	rec := postSignup(t, s, `{"email":"x@example.com"}`, "203.0.113.12:5000")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when the uniqueness lookup errors; got %d — body=%s", rec.Code, rec.Body.String())
	}
	if s.createProjectCalled {
		t.Error("fail-open bug: an account was created even though the uniqueness check could not be evaluated")
	}
}

func TestSignupDedup_NormalizesBeforeCheck(t *testing.T) {
	// Stored verified email is canonical lower+trim; the request uses
	// mixed case + surrounding whitespace. Must still collide → 409.
	s := &stubSignupStore{verified: map[string]bool{"dupe@example.com": true}}
	rec := postSignup(t, s, `{"email":"  DUPE@Example.com "}`, "203.0.113.13:5000")

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 after normalization; got %d — body=%s", rec.Code, rec.Body.String())
	}
	if s.createProjectCalled {
		t.Error("normalized duplicate leaked past the guard into CreateProject")
	}
}
