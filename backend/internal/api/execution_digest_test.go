package api

// Handler tests for GET /executions/{id}/digest.
//
// The digest itself is exercised in internal/attest. What is tested
// here is the part only the handler can get wrong: project scoping,
// the refusal cases, and the shape of what goes over the wire.
//
// The tenancy test is the one that matters. Everything else is an
// error path; that one is a data-disclosure boundary, and the digest
// endpoint reads an execution's entire event record to build its
// answer. A scoping mistake here leaks across tenants.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mesedi/backend/internal/attest"
	"mesedi/backend/internal/events"
	"mesedi/backend/internal/store"
)

type stubDigestStore struct {
	store.Store

	exec    *events.Execution
	evts    []*events.Event
	execErr error
	evtsErr error
}

func (s *stubDigestStore) GetExecution(_ context.Context, _ string) (*events.Execution, error) {
	if s.execErr != nil {
		return nil, s.execErr
	}
	return s.exec, nil
}

func (s *stubDigestStore) ListEventsForExecution(_ context.Context, _ string) ([]*events.Event, error) {
	if s.evtsErr != nil {
		return nil, s.evtsErr
	}
	return s.evts, nil
}

func digestMux(h *Handlers) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /executions/{id}/digest", h.HandleGetExecutionDigest)
	return mux
}

func digestRequest(t *testing.T, mux *http.ServeMux, url, projectID string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, url, nil)
	if projectID != "" {
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyProjectID, projectID))
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func digestFixture(projectID string) *stubDigestStore {
	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	return &stubDigestStore{
		exec: &events.Execution{ExecutionID: "exec-1", ProjectID: projectID},
		evts: []*events.Event{
			{EventID: "a", ExecutionID: "exec-1", EventType: events.EventTypeCheckpoint,
				Sequence: 1, Timestamp: base, Payload: json.RawMessage(`{"n":1}`)},
			{EventID: "b", ExecutionID: "exec-1", EventType: events.EventTypeCheckpoint,
				Sequence: 2, Timestamp: base.Add(time.Second), Payload: json.RawMessage(`{"n":2}`)},
			{EventID: "c", ExecutionID: "exec-1", EventType: events.EventTypeCheckpoint,
				Sequence: 3, Timestamp: base.Add(2 * time.Second), Payload: json.RawMessage(`{"n":3}`)},
		},
	}
}

func Test_HandleGetExecutionDigest_HappyPath(t *testing.T) {
	h := &Handlers{Store: digestFixture("proj-a"), Logger: quietLogger()}
	w := digestRequest(t, digestMux(h), "/executions/exec-1/digest", "proj-a")

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body %s)", w.Code, w.Body.String())
	}
	var body struct {
		OK     bool          `json:"ok"`
		Digest attest.Digest `json:"digest"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.OK {
		t.Error("ok should be true")
	}
	if body.Digest.LeafCount != 3 {
		t.Errorf("leaf_count = %d, want 3", body.Digest.LeafCount)
	}
	if body.Digest.Root == "" {
		t.Error("root must not be empty")
	}
	// The algorithm identifier is part of the published contract; a
	// verifier reads it to know which rules to apply.
	if body.Digest.Algorithm != attest.AlgorithmV1 {
		t.Errorf("algorithm = %q, want %q", body.Digest.Algorithm, attest.AlgorithmV1)
	}
}

// A caller must be able to recompute the root from what the response
// gives them. If they cannot, the digest is only as good as our word
// for it, which is the thing this whole feature is trying to avoid.
func Test_HandleGetExecutionDigest_ResponseIsIndependentlyVerifiable(t *testing.T) {
	h := &Handlers{Store: digestFixture("proj-a"), Logger: quietLogger()}
	w := digestRequest(t, digestMux(h), "/executions/exec-1/digest?leaf=1", "proj-a")

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body %s)", w.Code, w.Body.String())
	}
	var body struct {
		Digest attest.Digest         `json:"digest"`
		Proof  attest.InclusionProof `json:"inclusion_proof"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Proof.LeafIndex != 1 {
		t.Errorf("leaf_index = %d, want 1", body.Proof.LeafIndex)
	}
	ok, err := attest.VerifyInclusion(body.Proof)
	if err != nil {
		t.Fatalf("VerifyInclusion: %v", err)
	}
	if !ok {
		t.Error("the proof returned over the wire does not verify against " +
			"the root returned over the wire")
	}
}

// The boundary that matters. An execution belonging to another project
// must look absent, not forbidden — a 403 would confirm it exists.
func Test_HandleGetExecutionDigest_CrossProjectLooksAbsent(t *testing.T) {
	h := &Handlers{Store: digestFixture("proj-owner"), Logger: quietLogger()}
	w := digestRequest(t, digestMux(h), "/executions/exec-1/digest", "proj-attacker")

	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-project request must return 404, got %d (body %s)",
			w.Code, w.Body.String())
	}
	if body := w.Body.String(); len(body) > 0 &&
		(contains(body, "root") || contains(body, "leaves")) {
		t.Errorf("cross-project 404 leaked digest material: %s", body)
	}
}

func Test_HandleGetExecutionDigest_NoProjectContextIs401(t *testing.T) {
	h := &Handlers{Store: digestFixture("proj-a"), Logger: quietLogger()}
	w := digestRequest(t, digestMux(h), "/executions/exec-1/digest", "")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 with no project context, got %d", w.Code)
	}
}

// An execution with no events must 404 rather than return the
// empty-tree root. "We have no record of this run" and "here is the
// record" must not produce the same answer.
func Test_HandleGetExecutionDigest_NoEventsIs404(t *testing.T) {
	s := digestFixture("proj-a")
	s.evts = nil
	h := &Handlers{Store: s, Logger: quietLogger()}
	w := digestRequest(t, digestMux(h), "/executions/exec-1/digest", "proj-a")

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404 for an execution with no events, got %d (body %s)",
			w.Code, w.Body.String())
	}
}

func Test_HandleGetExecutionDigest_BadLeafParamIs400(t *testing.T) {
	h := &Handlers{Store: digestFixture("proj-a"), Logger: quietLogger()}

	for _, leaf := range []string{"not-a-number", "-1", "99"} {
		w := digestRequest(t, digestMux(h),
			"/executions/exec-1/digest?leaf="+leaf, "proj-a")
		if w.Code != http.StatusBadRequest {
			t.Errorf("leaf=%q: want 400, got %d (body %s)",
				leaf, w.Code, w.Body.String())
		}
	}
}

// contains() is deliberately NOT redefined here — admin_ai_analyses_detail_test.go
// already declares it in this package, and a second declaration would
// not compile.
