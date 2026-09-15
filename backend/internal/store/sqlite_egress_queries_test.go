package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

func newEgressQueryStore(t *testing.T) *SQLiteStore {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func saveEgressEvent(t *testing.T, st *SQLiteStore, execID string, seq int, payload map[string]any, eventType string) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	if err := st.SaveEvents(context.Background(), []events.Event{{
		EventID: fmt.Sprintf("evt_%s_%d", execID, seq), ExecutionID: execID,
		EventType: events.EventType(eventType), Sequence: seq,
		Timestamp: time.Now().UTC(), Payload: raw,
	}}); err != nil {
		t.Fatalf("save event: %v", err)
	}
}

// The FIRST declaration wins by design: the declaration is the
// operator's claim at run start, and a later event must not let a
// confused run rewrite its own boundary.
func TestGetEnvironmentDeclarationModeFirstWins(t *testing.T) {
	st := newEgressQueryStore(t)
	ctx := context.Background()
	const projectID, execID = "proj_eq", "exec_eq_mode"
	mustCreateProjectAndExecution(t, st, projectID, execID)

	if mode, err := st.GetEnvironmentDeclarationMode(ctx, execID); err != nil || mode != "" {
		t.Fatalf("no declaration = (%q, %v), want empty and nil", mode, err)
	}
	saveEgressEvent(t, st, execID, 1, map[string]any{"mode": "simulation"}, "environment_declaration")
	saveEgressEvent(t, st, execID, 2, map[string]any{"mode": "live"}, "environment_declaration")
	mode, err := st.GetEnvironmentDeclarationMode(ctx, execID)
	if err != nil || mode != "simulation" {
		t.Fatalf("mode = (%q, %v), want the FIRST declaration, simulation", mode, err)
	}
}

func TestListEgressDestinationsDistinctFirstSeen(t *testing.T) {
	st := newEgressQueryStore(t)
	ctx := context.Background()
	const projectID, execID = "proj_eq2", "exec_eq_dests"
	mustCreateProjectAndExecution(t, st, projectID, execID)

	saveEgressEvent(t, st, execID, 1, map[string]any{"destination": "b.example-corp.com"}, "egress")
	saveEgressEvent(t, st, execID, 2, map[string]any{"destination": "a.example-corp.com"}, "egress")
	saveEgressEvent(t, st, execID, 3, map[string]any{"destination": "b.example-corp.com"}, "egress")

	dests, err := st.ListEgressDestinations(ctx, execID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(dests) != 2 || dests[0] != "b.example-corp.com" || dests[1] != "a.example-corp.com" {
		t.Fatalf("dests = %v, want distinct in first-seen order [b a]", dests)
	}
}

// The rendezvous counter's whole design is that the threshold lives
// server-side in one query, so this pins the HAVING clause and the
// distinct-execution counting against a hand-built scenario.
func TestListCoordinatingEgressDestinationsThreshold(t *testing.T) {
	st := newEgressQueryStore(t)
	ctx := context.Background()
	const projectID = "proj_eq3"
	for i := 1; i <= 3; i++ {
		execID := fmt.Sprintf("exec_conv_%d", i)
		mustCreateProjectAndExecution(t, st, projectID, execID)
		saveEgressEvent(t, st, execID, 1, map[string]any{"destination": "drop.example-corp.com"}, "egress")
	}
	// A destination only this one execution contacted must not count.
	saveEgressEvent(t, st, "exec_conv_3", 2, map[string]any{"destination": "solo.example-corp.com"}, "egress")

	since := time.Now().UTC().Add(-time.Hour)
	got, err := st.ListCoordinatingEgressDestinations(ctx, "exec_conv_3", projectID, since, 3)
	if err != nil {
		t.Fatalf("list coordinating: %v", err)
	}
	if len(got) != 1 || got[0].Destination != "drop.example-corp.com" || got[0].DistinctRuns != 3 {
		t.Fatalf("rendezvous = %+v, want exactly drop.example-corp.com with 3 runs", got)
	}
	if below, _ := st.ListCoordinatingEgressDestinations(ctx, "exec_conv_3", projectID, since, 4); len(below) != 0 {
		t.Fatalf("threshold 4 over 3 runs returned %+v, want nothing", below)
	}
}

func TestEgressGroupWritersCreateTheirClasses(t *testing.T) {
	st := newEgressQueryStore(t)
	ctx := context.Background()
	const projectID, execID = "proj_eq4", "exec_eq_groups"
	mustCreateProjectAndExecution(t, st, projectID, execID)

	if isNew, err := st.GroupEnvironmentMisapprehension(ctx, execID, projectID, "env_mismatch:simulation"); err != nil || !isNew {
		t.Fatalf("GroupEnvironmentMisapprehension = (%v, %v), want new group", isNew, err)
	}
	if isNew, err := st.GroupCovertCoordination(ctx, execID, projectID, "covert_coordination:drop.example-corp.com"); err != nil || !isNew {
		t.Fatalf("GroupCovertCoordination = (%v, %v), want new group", isNew, err)
	}
	if _, err := st.GroupEnvironmentMisapprehension(ctx, execID, projectID, ""); err == nil {
		t.Fatal("empty signature must be refused")
	}
	groups, err := st.ListFailureGroups(ctx, projectID, ListFailureGroupsOpts{Limit: 10})
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	classes := map[string]bool{}
	for _, g := range groups {
		classes[g.FailureClass] = true
	}
	if !classes[FailureClassEnvironmentMisapprehension] || !classes[FailureClassCovertCoordination] {
		t.Fatalf("classes = %v, want both egress-derived classes present", classes)
	}
}

func mustCreateProjectAndExecution(t *testing.T, st *SQLiteStore, projectID, execID string) {
	t.Helper()
	ctx := context.Background()
	_ = st.CreateProject(ctx, &Project{
		ProjectID: projectID, Name: "egress query test", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	})
	if err := st.CreateExecution(ctx, &events.Execution{
		ExecutionID: execID, ProjectID: projectID,
		Status: events.StatusStarted, StartedAt: time.Now().UTC().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("create execution: %v", err)
	}
}
