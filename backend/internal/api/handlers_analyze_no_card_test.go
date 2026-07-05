// Unit tests for HandleAnalyzeFailureGroup's no-card gate (
// hotfix). This is the test that would have caught the migration-022
// DEFAULT-1 bug — a Hobby project with no real card on file should
// receive a 402 from the analyze endpoint, NOT have an analysis run.
//
// Why this test was missing in the original ship:
//   - billing_ai_analyses_usage_test.go covers the READ endpoint
//     (HandleGetAIAnalysesUsage). It checks tier branching but not
//     the analyze gate.
//   - The analyze endpoint itself had no unit test. The handler is
//     larger and touches an Anthropic client, but the gate runs
//     BEFORE any LLM call so it can be tested without a stub
//     Anthropic.
//
// Coverage:
//   - Hobby + !CardOnFile  → 402 "add a payment method"
//   - Hobby + CardOnFile   → passes the gate (proceeds to count check)
//   - Team + !CardOnFile + past included → 402 "add a payment method"
//   - Team + !CardOnFile + within included → passes (allowed at included quota)
package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mesedi/backend/internal/anthropic"
	"mesedi/backend/internal/events"
	"mesedi/backend/internal/store"
)

// quietLogger returns a slog.Logger that drops every line. Test
// handlers need a non-nil Logger because the analyze path may log
// warnings (e.g., "anthropic call failed" on the post-gate sad path)
// and a nil receiver segfaults inside (*slog.Logger).Error.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubAnalyzeStore implements just the Store methods that
// HandleAnalyzeFailureGroup reaches before the no-card gate fires.
// Other methods panic if invoked — a test exercising the gate must
// not depend on count queries or LLM round-trips.
type stubAnalyzeStore struct {
	store.Store

	project      *store.Project
	failureGroup *store.FailureGroup

	// Count returned by the AI analyses count query (used only after
	// the no-card gate passes). Default zero.
	aiAnalysesCount int
}

func (s *stubAnalyzeStore) GetFailureGroup(ctx context.Context, id string) (*store.FailureGroup, error) {
	return s.failureGroup, nil
}

func (s *stubAnalyzeStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	return s.project, nil
}

func (s *stubAnalyzeStore) GetProjectTenantID(ctx context.Context, id string) (*string, error) {
	return nil, nil
}

func (s *stubAnalyzeStore) CountAIAnalysesSincePeriodStart(
	ctx context.Context, projectID string, since time.Time,
) (int, error) {
	return s.aiAnalysesCount, nil
}

func (s *stubAnalyzeStore) CountAIAnalysesByTenantSince(
	ctx context.Context, tenantID string, since time.Time,
) (int, error) {
	return s.aiAnalysesCount, nil
}

// ListExecutionsByFailureGroup is called inside the handler ONLY
// after the gates pass, to build the LLM prompt. Returning empty
// keeps the post-gate path predictable without forcing a real LLM
// call (the Anthropic client below has a fake key, so Call would
// fail — but the gates we test return 402 before reaching Call).
func (s *stubAnalyzeStore) ListExecutionsByFailureGroup(
	ctx context.Context, groupID string, limit, offset int,
) ([]*events.Execution, error) {
	return nil, nil
}

func callAnalyzeHandler(t *testing.T, h *Handlers, projectID, groupID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/failure-groups/"+groupID+"/analyze", nil)
	req.SetPathValue("id", groupID)
	ctx := context.WithValue(req.Context(), ctxKeyProjectID, projectID)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.HandleAnalyzeFailureGroup(rec, req)
	return rec
}

func newAnalyzeStubProject(tier string, cardOnFile bool) *store.Project {
	return &store.Project{
		ProjectID:  "proj_test",
		Tier:       tier,
		CardOnFile: cardOnFile,
	}
}

func newAnalyzeStubGroup() *store.FailureGroup {
	return &store.FailureGroup{
		GroupID:   "fg_test",
		ProjectID: "proj_test",
	}
}

// fakeAnthropicClient returns a client with a non-empty key so
// Enabled() reports true. We never reach the actual Call in these
// tests because the no-card gates return 402 first.
func fakeAnthropicClient() *anthropic.Client {
	return anthropic.New("sk-test-fake", "", "")
}

func TestHandleAnalyze_Hobby_NoCard_Returns402(t *testing.T) {
	// The regression test for : this is the exact scenario
	// Robert hit on production — Hobby project, no card on file,
	// clicked Analyze, got an analysis. The fix makes this 402.
	st := &stubAnalyzeStore{
		project:      newAnalyzeStubProject(TierHobby, false),
		failureGroup: newAnalyzeStubGroup(),
	}
	h := &Handlers{Store: st, Anthropic: fakeAnthropicClient(), Logger: quietLogger()}

	rec := callAnalyzeHandler(t, h, "proj_test", "fg_test")

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status: got %d, want 402; body=%q", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 402 body: %v", err)
	}
	errMsg, _ := body["error"].(string)
	// The message should specifically guide the customer to add a
	// payment method; a generic 402 isn't enough.
	if !strings.Contains(errMsg, "Add a payment method") {
		t.Errorf("error message should mention 'Add a payment method', got %q", errMsg)
	}
}

func TestHandleAnalyze_Hobby_WithCard_PassesNoCardGate(t *testing.T) {
	// Sanity check: a legitimate Hobby with card SHOULD pass the
	// no-card gate. Doesn't have to fully succeed (the LLM call
	// will fail in test mode), but it must NOT return 402 with the
	// no-card message. Since the test Anthropic client has a fake
	// key, the actual LLM call will 502; that's expected and means
	// the gate passed.
	st := &stubAnalyzeStore{
		project:         newAnalyzeStubProject(TierHobby, true),
		failureGroup:    newAnalyzeStubGroup(),
		aiAnalysesCount: 0, // under 50 cap
	}
	h := &Handlers{Store: st, Anthropic: fakeAnthropicClient(), Logger: quietLogger()}

	rec := callAnalyzeHandler(t, h, "proj_test", "fg_test")

	// Must NOT be 402 for no-card reason. Any other outcome (502
	// from LLM, 500, etc.) means the gate let it through, which is
	// what we want.
	if rec.Code == http.StatusPaymentRequired {
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		errMsg, _ := body["error"].(string)
		if strings.Contains(errMsg, "Add a payment method") {
			t.Errorf("Hobby with card on file should NOT get the no-card 402; got: %q", errMsg)
		}
	}
}

func TestHandleAnalyze_Team_NoCard_PastIncluded_Returns402(t *testing.T) {
	// Team that removed their card mid-cycle, past the included
	// 200 → 402 with "Add a payment method." The Team branch in
	// HandleAnalyzeFailureGroup has its own no-card check inside
	// the count >= TeamAIAnalysisLimit branch.
	st := &stubAnalyzeStore{
		project:         newAnalyzeStubProject(TierTeam, false),
		failureGroup:    newAnalyzeStubGroup(),
		aiAnalysesCount: TeamAIAnalysisLimit, // exactly at included
	}
	h := &Handlers{Store: st, Anthropic: fakeAnthropicClient(), Logger: quietLogger()}

	rec := callAnalyzeHandler(t, h, "proj_test", "fg_test")

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status: got %d, want 402; body=%q", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 402 body: %v", err)
	}
	errMsg, _ := body["error"].(string)
	if !strings.Contains(errMsg, "Add a payment method") {
		t.Errorf("error message should mention 'Add a payment method', got %q", errMsg)
	}
}

func TestHandleAnalyze_Team_NoCard_WithinIncluded_PassesNoCardGate(t *testing.T) {
	// Team without a card but still within the included 200 → the
	// no-card gate does NOT apply because the included quota is paid
	// for by the $99 flat fee, not by overage billing. The handler
	// passes the gate and reaches the LLM (which will fail with the
	// fake test key, but that's a separate outcome).
	st := &stubAnalyzeStore{
		project:         newAnalyzeStubProject(TierTeam, false),
		failureGroup:    newAnalyzeStubGroup(),
		aiAnalysesCount: 50, // well under 200
	}
	h := &Handlers{Store: st, Anthropic: fakeAnthropicClient(), Logger: quietLogger()}

	rec := callAnalyzeHandler(t, h, "proj_test", "fg_test")

	if rec.Code == http.StatusPaymentRequired {
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		errMsg, _ := body["error"].(string)
		if strings.Contains(errMsg, "Add a payment method") {
			t.Errorf("Team within included quota should NOT get the no-card 402; got: %q", errMsg)
		}
	}
}
