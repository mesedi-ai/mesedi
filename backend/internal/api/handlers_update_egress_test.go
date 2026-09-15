package api

// Wiring tests for the two egress-derived detectors AS REACHED
// through HandleUpdateExecution, in the shape every carved detector
// block carries: a test that fails if the wired call is ever
// dropped. One execution declares itself a simulation and reports
// egress to a live destination; completing it must produce an
// environment_misapprehension group. Three executions report egress
// to the same destination; completing the third must produce a
// covert_coordination group.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/store"
)

func newEgressWiringHarness(t *testing.T, projectID string) (*Handlers, *store.SQLiteStore) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	if err := st.CreateProject(context.Background(), &store.Project{
		ProjectID: projectID, Name: "egress wiring test", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	h := &Handlers{Logger: logger, Store: st, HaltSubs: NewHaltSubscribers()}
	t.Cleanup(func() { h.DrainDispatches(); _ = st.Close() })
	return h, st
}

func completeExecution(t *testing.T, h *Handlers, projectID, executionID string) {
	t.Helper()
	req := httptest.NewRequest("PATCH", "/executions/"+executionID,
		strings.NewReader(`{"status":"completed"}`))
	req.SetPathValue("id", executionID)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyProjectID, projectID))
	rec := httptest.NewRecorder()
	h.HandleUpdateExecution(rec, req)
	if rec.Code != 200 {
		t.Fatalf("PATCH %s returned %d: %s", executionID, rec.Code, rec.Body.String())
	}
}

func TestHandleUpdateExecution_FiresEnvironmentMisapprehension(t *testing.T) {
	const projectID = "proj_env_wiring"
	const executionID = "exec_env_mismatch"
	h, st := newEgressWiringHarness(t, projectID)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := st.CreateExecution(ctx, &events.Execution{
		ExecutionID: executionID, ProjectID: projectID,
		Status: events.StatusStarted, StartedAt: now.Add(-5 * time.Minute),
	}); err != nil {
		t.Fatalf("create execution: %v", err)
	}
	declPayload, _ := json.Marshal(map[string]any{"mode": "simulation", "declared_by": "harness"})
	egressPayload, _ := json.Marshal(map[string]any{"destination": "api.example-corp.com:443", "protocol": "https"})
	if err := st.SaveEvents(ctx, []events.Event{
		{EventID: "evt_env_1", ExecutionID: executionID, EventType: "environment_declaration",
			Sequence: 1, Timestamp: now.Add(-4 * time.Minute), Payload: declPayload},
		{EventID: "evt_env_2", ExecutionID: executionID, EventType: "egress",
			Sequence: 2, Timestamp: now.Add(-3 * time.Minute), Payload: egressPayload},
	}); err != nil {
		t.Fatalf("save events: %v", err)
	}

	completeExecution(t, h, projectID, executionID)

	groups, err := st.ListFailureGroups(ctx, projectID, store.ListFailureGroupsOpts{Limit: 50})
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	found := false
	for _, g := range groups {
		if g.FailureClass == store.FailureClassEnvironmentMisapprehension {
			found = true
			if g.Signature != "env_mismatch:simulation" {
				t.Errorf("signature = %q, want env_mismatch:simulation", g.Signature)
			}
		}
	}
	if !found {
		t.Errorf("no %s group after a declared-simulation run reported egress "+
			"to a live destination; the wired detector call may have been dropped",
			store.FailureClassEnvironmentMisapprehension)
	}
}

func TestHandleUpdateExecution_FiresCovertCoordination(t *testing.T) {
	const projectID = "proj_covert_wiring"
	h, st := newEgressWiringHarness(t, projectID)
	ctx := context.Background()
	now := time.Now().UTC()

	// Three distinct executions, one shared destination: exactly the
	// default threshold. The third to complete must see the other
	// two in the window and fire.
	const sharedDest = "drop.example-corp.com"
	for i := 1; i <= 3; i++ {
		execID := fmt.Sprintf("exec_covert_%d", i)
		if err := st.CreateExecution(ctx, &events.Execution{
			ExecutionID: execID, ProjectID: projectID,
			Status: events.StatusStarted, StartedAt: now.Add(-10 * time.Minute),
		}); err != nil {
			t.Fatalf("create execution %d: %v", i, err)
		}
		payload, _ := json.Marshal(map[string]any{"destination": sharedDest, "protocol": "https"})
		if err := st.SaveEvents(ctx, []events.Event{{
			EventID: fmt.Sprintf("evt_covert_%d", i), ExecutionID: execID,
			EventType: "egress", Sequence: 1,
			Timestamp: now.Add(-9 * time.Minute), Payload: payload,
		}}); err != nil {
			t.Fatalf("save events %d: %v", i, err)
		}
	}

	completeExecution(t, h, projectID, "exec_covert_3")

	groups, err := st.ListFailureGroups(ctx, projectID, store.ListFailureGroupsOpts{Limit: 50})
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	found := false
	for _, g := range groups {
		if g.FailureClass == store.FailureClassCovertCoordination {
			found = true
			if g.Signature != "covert_coordination:"+sharedDest {
				t.Errorf("signature = %q, want covert_coordination:%s", g.Signature, sharedDest)
			}
		}
	}
	if !found {
		t.Errorf("no %s group after three distinct executions shared one egress "+
			"destination; the wired counter call may have been dropped",
			store.FailureClassCovertCoordination)
	}

	// Two runs must NOT be enough: a fresh project with only two
	// convergent executions stays quiet, or the threshold is fake.
	const p2 = "proj_covert_below"
	h2, st2 := newEgressWiringHarness(t, p2)
	for i := 1; i <= 2; i++ {
		execID := fmt.Sprintf("exec_below_%d", i)
		if err := st2.CreateExecution(ctx, &events.Execution{
			ExecutionID: execID, ProjectID: p2,
			Status: events.StatusStarted, StartedAt: now.Add(-10 * time.Minute),
		}); err != nil {
			t.Fatalf("create below-threshold execution %d: %v", i, err)
		}
		payload, _ := json.Marshal(map[string]any{"destination": sharedDest})
		if err := st2.SaveEvents(ctx, []events.Event{{
			EventID: fmt.Sprintf("evt_below_%d", i), ExecutionID: execID,
			EventType: "egress", Sequence: 1,
			Timestamp: now.Add(-9 * time.Minute), Payload: payload,
		}}); err != nil {
			t.Fatalf("save below-threshold events %d: %v", i, err)
		}
	}
	completeExecution(t, h2, p2, "exec_below_2")
	groups2, err := st2.ListFailureGroups(ctx, p2, store.ListFailureGroupsOpts{Limit: 50})
	if err != nil {
		t.Fatalf("list below-threshold groups: %v", err)
	}
	for _, g := range groups2 {
		if g.FailureClass == store.FailureClassCovertCoordination {
			t.Errorf("covert_coordination fired at two distinct runs; threshold is three")
		}
	}
}
