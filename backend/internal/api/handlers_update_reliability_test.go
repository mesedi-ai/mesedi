package api

// Wiring test for the HITL-timeout detector AS REACHED through
// HandleUpdateExecution, written with the third carve so the moved
// block gains what cost-velocity and drift gained with theirs: a
// test that fails if the carved call is ever dropped. One execution
// carries a human_intervention event whose response_kind is the
// explicit "timeout"; completing the execution must produce an
// hitl_timeout failure group.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/store"
)

func TestHandleUpdateExecution_FiresHITLTimeout(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}

	ctx := context.Background()
	const projectID = "proj_hitl_wiring"
	const executionID = "exec_hitl_timeout"
	if err := st.CreateProject(ctx, &store.Project{
		ProjectID: projectID, Name: "hitl wiring test", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	now := time.Now().UTC()
	if err := st.CreateExecution(ctx, &events.Execution{
		ExecutionID: executionID, ProjectID: projectID,
		Status: events.StatusStarted, StartedAt: now.Add(-10 * time.Minute),
	}); err != nil {
		t.Fatalf("create execution: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"request_id":    "req_hitl_1",
		"question":      "approve the wire transfer?",
		"sla_seconds":   60,
		"requested_at":  now.Add(-9 * time.Minute).Format(time.RFC3339),
		"response_kind": "timeout",
		"decided_at":    now.Add(-1 * time.Minute).Format(time.RFC3339),
	})
	if err := st.SaveEvents(ctx, []events.Event{{
		EventID: "evt_hitl_1", ExecutionID: executionID,
		EventType: "human_intervention", Sequence: 1,
		Timestamp: now.Add(-1 * time.Minute), Payload: payload,
	}}); err != nil {
		t.Fatalf("save events: %v", err)
	}

	h := &Handlers{Logger: logger, Store: st, HaltSubs: NewHaltSubscribers()}
	t.Cleanup(func() { h.DrainDispatches(); _ = st.Close() })

	req := httptest.NewRequest("PATCH", "/executions/"+executionID,
		strings.NewReader(`{"status":"completed"}`))
	req.SetPathValue("id", executionID)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyProjectID, projectID))
	rec := httptest.NewRecorder()
	h.HandleUpdateExecution(rec, req)
	if rec.Code != 200 {
		t.Fatalf("PATCH returned %d: %s", rec.Code, rec.Body.String())
	}

	groups, err := st.ListFailureGroups(ctx, projectID, store.ListFailureGroupsOpts{Limit: 50})
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	found := false
	for _, g := range groups {
		if g.FailureClass == store.FailureClassHITLTimeout {
			found = true
		}
	}
	if !found {
		t.Errorf("no %s group after an explicit response_kind=timeout "+
			"human_intervention event on a completed execution; the carved "+
			"detector call may have been dropped", store.FailureClassHITLTimeout)
	}
}
