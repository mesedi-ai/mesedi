// Unit tests for the duplicate-account guard in HandleSignup.
//
// Behavior under test (Option A — one VERIFIED, STILL-EXISTING account
// per email). A new signup is rejected with 409 only when BOTH a project
// with that email still exists AND the email is verified. The tests pin
// the four corners of that truth table plus the fail-closed paths:
//
//   - live verified account          -> 409 (sign in instead)
//   - account was deleted (no project, but a stale verified_emails row
//     survives)                       -> PROCEEDS  (deletion frees email)
//   - live but UNVERIFIED account (a stranger pre-registered the address)
//                                     -> PROCEEDS  (real owner not locked out)
//   - fresh email                     -> PROCEEDS
//   - existing-project lookup errors  -> 500, fail closed (no account made)
//   - verified lookup errors          -> 500, fail closed
//   - email normalized (lower+trim) before the verified check

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

// stubSignupStore embeds store.Store so only the methods the guard path
// touches are implemented. existingProject drives GetMostRecentProjectBy-
// OwnerEmail (nil => ErrNotFound, i.e. no live account). createProjectCalled
// records whether the handler proceeded past the guard into project
// creation; createProjectErr lets a test short-circuit the happy path right
// after the guard without stubbing org bootstrap, retention, key mint, and
// the mailer.
type stubSignupStore struct {
	store.Store

	existingProject     *store.Project
	getByEmailErr       error
	verified            map[string]bool
	isVerifiedErr       error
	createProjectCalled bool
	createProjectErr    error
}

func (s *stubSignupStore) GetMostRecentProjectByOwnerEmail(_ context.Context, _ string) (*store.Project, error) {
	if s.getByEmailErr != nil {
		return nil, s.getByEmailErr
	}
	if s.existingProject == nil {
		return nil, store.ErrNotFound
	}
	return s.existingProject, nil
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

// liveProject is a minimal non-nil project row for "an account still exists".
func liveProject(email string) *store.Project {
	return &store.Project{ProjectID: "proj_existing", OwnerEmail: email}
}

func TestSignupDedup_VerifiedLiveAccountRejected(t *testing.T) {
	s := &stubSignupStore{
		existingProject: liveProject("dupe@example.com"),
		verified:        map[string]bool{"dupe@example.com": true},
	}
	rec := postSignup(t, s, `{"email":"dupe@example.com"}`, "203.0.113.10:5000")

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a live verified duplicate; got %d — body=%s", rec.Code, rec.Body.String())
	}
	if s.createProjectCalled {
		t.Error("guard leaked: CreateProject was called for a live verified duplicate")
	}
	var parsed map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("body not valid JSON: %v — raw=%s", err, rec.Body.String())
	}
	if msg, _ := parsed["error"].(string); !strings.Contains(msg, "already exists") {
		t.Errorf("expected an 'already exists' message; got %q", msg)
	}
}

func TestSignupDedup_DeletedAccountFreesEmail(t *testing.T) {
	// The account was deleted: no project row remains, but the
	// verified_emails ledger still carries the address (it is never
	// pruned). Option A must let the signup proceed. We short-circuit
	// CreateProject with a canned error so the handler stops right after
	// the guard; reaching CreateProject proves the guard passed.
	s := &stubSignupStore{
		existingProject:  nil,                                    // deleted -> ErrNotFound
		verified:         map[string]bool{"gone@example.com": true}, // stale ledger row
		createProjectErr: errors.New("stub: stop after guard"),
	}
	rec := postSignup(t, s, `{"email":"gone@example.com"}`, "203.0.113.11:5000")

	if !s.createProjectCalled {
		t.Fatalf("deleted account did NOT free the email: signup blocked before CreateProject (code=%d body=%s)", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusConflict {
		t.Errorf("a deleted account must not 409 on re-signup; got %d", rec.Code)
	}
}

func TestSignupDedup_UnverifiedLiveAccountNotBlocked(t *testing.T) {
	// A project exists but the email was never verified — e.g. someone
	// pre-registered a stranger's address. The real owner must be able to
	// sign up (and then verify), so the guard must NOT block.
	s := &stubSignupStore{
		existingProject:  liveProject("victim@example.com"),
		verified:         map[string]bool{}, // absent -> unverified
		createProjectErr: errors.New("stub: stop after guard"),
	}
	rec := postSignup(t, s, `{"email":"victim@example.com"}`, "203.0.113.12:5000")

	if !s.createProjectCalled {
		t.Fatalf("guard over-blocked an UNVERIFIED existing account (squat risk); code=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusConflict {
		t.Errorf("unverified existing account must not 409; got %d", rec.Code)
	}
}

func TestSignupDedup_FreshEmailProceeds(t *testing.T) {
	s := &stubSignupStore{
		existingProject:  nil, // no account
		verified:         map[string]bool{},
		createProjectErr: errors.New("stub: stop after guard"),
	}
	rec := postSignup(t, s, `{"email":"fresh@example.com"}`, "203.0.113.13:5000")

	if !s.createProjectCalled {
		t.Fatalf("fresh email was blocked; code=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusConflict {
		t.Errorf("fresh email must not 409; got %d", rec.Code)
	}
}

func TestSignupDedup_ExistingCheckErrorFailsClosed(t *testing.T) {
	s := &stubSignupStore{getByEmailErr: errors.New("db blip")}
	rec := postSignup(t, s, `{"email":"x@example.com"}`, "203.0.113.14:5000")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when the existing-project lookup errors; got %d — body=%s", rec.Code, rec.Body.String())
	}
	if s.createProjectCalled {
		t.Error("fail-open bug: account created despite an unresolved existence check")
	}
}

func TestSignupDedup_VerifiedCheckErrorFailsClosed(t *testing.T) {
	s := &stubSignupStore{
		existingProject: liveProject("x@example.com"),
		isVerifiedErr:   errors.New("db blip"),
	}
	rec := postSignup(t, s, `{"email":"x@example.com"}`, "203.0.113.15:5000")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when the verified lookup errors; got %d — body=%s", rec.Code, rec.Body.String())
	}
	if s.createProjectCalled {
		t.Error("fail-open bug: account created despite an unresolved verified check")
	}
}

func TestSignupDedup_NormalizesBeforeCheck(t *testing.T) {
	// Stored verified email is canonical lower+trim; the request uses
	// mixed case + surrounding whitespace. Must still collide -> 409.
	s := &stubSignupStore{
		existingProject: liveProject("dupe@example.com"),
		verified:        map[string]bool{"dupe@example.com": true},
	}
	rec := postSignup(t, s, `{"email":"  DUPE@Example.com "}`, "203.0.113.16:5000")

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 after normalization; got %d — body=%s", rec.Code, rec.Body.String())
	}
	if s.createProjectCalled {
		t.Error("normalized duplicate leaked past the guard into CreateProject")
	}
}
