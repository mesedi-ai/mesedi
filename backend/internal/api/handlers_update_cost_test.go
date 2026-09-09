package api

// Tests for the cost-velocity detectors AS WIRED through
// HandleUpdateExecution, written during the #35 Phase D carve after a
// mutation test proved the wiring had never been covered: deleting
// the h.runCostVelocityDetectors call compiled cleanly and the whole
// suite stayed green. A detector that can be deleted without a test
// noticing is not protected, whatever the unit tests around it say.
//
// Strategy: a real in-memory SQLite store (migrations applied by
// OpenSQLite) rather than a stub, because the point is the wiring.
// The request travels the same path production takes: PATCH body
// through decodeJSON, project auth from context, every sub-slice of
// the handler running against a live store. The assertion then reads
// failure groups back out of the store, so it can only pass if the
// handler actually reached the detectors and the detectors actually
// wrote.
//
// The scenario fires BOTH forms deliberately. One execution at
// $50 crosses the $1 default absolute threshold (bucket cost_$10+),
// and $50 inside the default 5-minute window is $10/min against the
// $5/min default rate threshold (bucket rate_$10+_per_min). Both
// groups must exist afterwards; asserting on exact signatures pins
// the threshold-and-bucket arithmetic, not just "something fired".

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mesedi/backend/internal/events"
	"mesedi/backend/internal/store"
)

func TestHandleUpdateExecution_FiresCostVelocityDetectors(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	// Deliberately NOT closed. maybeFireWebhook spawns a detached
	// dispatch goroutine (webhook_dispatch.go, spawn-and-forget by
	// design) that reads the store after this test returns; Close()
	// nils the db handle and the goroutine then panics the whole
	// test binary, which is exactly what happened on CI's slower
	// runner while local runs won the race. The in-memory store
	// lives until process exit, which is fine for a test binary.
	// The durable fix, tracking dispatch goroutines for shutdown,
	// is the B30 debt already scheduled in the #35 split.

	ctx := context.Background()
	const projectID = "proj_costvel_test"
	const executionID = "exec_costvel_1"

	if err := st.CreateProject(ctx, &store.Project{
		ProjectID: projectID,
		Name:      "cost-velocity wiring test",
		Tier:      "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := st.CreateExecution(ctx, &events.Execution{
		ExecutionID: executionID,
		ProjectID:   projectID,
		Status:      events.StatusStarted,
		StartedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create execution: %v", err)
	}

	h := &Handlers{
		Logger:   logger,
		Store:    st,
		HaltSubs: NewHaltSubscribers(),
	}

	// PATCH to terminal status with an SDK-rolled-up cost. No events
	// exist, so the backend cost walk yields zero and the handler
	// falls back to estimated_cost_usd, the documented last resort.
	body := `{"status":"completed","estimated_cost_usd":50.0}`
	req := httptest.NewRequest("PATCH", "/executions/"+executionID, strings.NewReader(body))
	req.SetPathValue("id", executionID)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyProjectID, projectID))

	rec := httptest.NewRecorder()
	h.HandleUpdateExecution(rec, req)
	if rec.Code != 200 {
		t.Fatalf("PATCH returned %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	groups, err := st.ListFailureGroups(ctx, projectID, store.ListFailureGroupsOpts{Limit: 50})
	if err != nil {
		t.Fatalf("list failure groups: %v", err)
	}
	found := map[string]bool{}
	for _, g := range groups {
		if g.FailureClass == store.FailureClassCostVelocity {
			found[g.Signature] = true
		}
	}

	wantAbsolute := store.CostVelocitySignature(50.0) // cost_$10+
	if !found[wantAbsolute] {
		t.Errorf("absolute cost-velocity detector did not fire: no %s group "+
			"with signature %q after a $50 execution against the $%.2f default "+
			"threshold; groups found: %v",
			store.FailureClassCostVelocity, wantAbsolute,
			store.DefaultCostVelocityThresholdUSD, found)
	}
	wantRate := store.CostVelocityRateSignature(10.0) // rate_$10+_per_min
	if !found[wantRate] {
		t.Errorf("rate cost-velocity detector did not fire: no %s group with "+
			"signature %q after $50 in the default window; groups found: %v",
			store.FailureClassCostVelocity, wantRate, found)
	}
}
