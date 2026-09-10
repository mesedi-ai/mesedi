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
	"fmt"
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

	// The execution was created without tenant or API key, so the
	// #48 attributed signature resolves to the unattributed marker.
	wantAbsolute := store.CostVelocityAttributedSignature(50.0, "unattributed")
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

// TestHandleUpdateExecution_NewActorMakesNewCostVelocityGroup drives
// the #48 property through the real handler: two tenants each cross
// the absolute threshold in the same magnitude bucket, and each must
// get its OWN failure group. Pre-attribution both folded into one
// cost_$10+ group, so a never-seen actor's spend read as routine
// recurrence, the radar's exact complaint.
func TestHandleUpdateExecution_NewActorMakesNewCostVelocityGroup(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	// Not closed, same reason as above: detached dispatch goroutines.

	ctx := context.Background()
	const projectID = "proj_costvel_actors"
	if err := st.CreateProject(ctx, &store.Project{
		ProjectID: projectID, Name: "new-actor test", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	h := &Handlers{Logger: logger, Store: st, HaltSubs: NewHaltSubscribers()}

	finish := func(execID, tenant string) {
		t.Helper()
		tenantVal := tenant
		if err := st.CreateExecution(ctx, &events.Execution{
			ExecutionID: execID, ProjectID: projectID,
			Status: events.StatusStarted, StartedAt: time.Now().UTC(),
			TenantID: &tenantVal,
		}); err != nil {
			t.Fatalf("create execution %s: %v", execID, err)
		}
		body := `{"status":"completed","estimated_cost_usd":15.0}`
		req := httptest.NewRequest("PATCH", "/executions/"+execID, strings.NewReader(body))
		req.SetPathValue("id", execID)
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyProjectID, projectID))
		rec := httptest.NewRecorder()
		h.HandleUpdateExecution(rec, req)
		if rec.Code != 200 {
			t.Fatalf("PATCH %s returned %d: %s", execID, rec.Code, rec.Body.String())
		}
	}
	finish("exec_known_actor", "long-standing-customer")
	finish("exec_new_actor", "never-seen-before")

	groups, err := st.ListFailureGroups(ctx, projectID, store.ListFailureGroupsOpts{Limit: 50})
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	sigs := map[string]bool{}
	for _, g := range groups {
		if g.FailureClass == store.FailureClassCostVelocity {
			sigs[g.Signature] = true
		}
	}
	for _, want := range []string{
		store.CostVelocityAttributedSignature(15.0, "tenant:long-standing-customer"),
		store.CostVelocityAttributedSignature(15.0, "tenant:never-seen-before"),
	} {
		if !sigs[want] {
			t.Errorf("missing per-actor group %q; signatures found: %v", want, sigs)
		}
	}
}

// TestHandleUpdateExecution_BaselineFiresOnAccelerationPastNormal
// drives the learned-normal form through the real handler: a project
// four days old with 55 cheap runs establishes a normal of roughly a
// cent a minute, then one $50 execution lands, a burn rate hundreds
// of times normal. A baseline_x* group must exist afterwards,
// independent of the fixed-threshold groups that also fire.
func TestHandleUpdateExecution_BaselineFiresOnAccelerationPastNormal(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	// Not closed: detached dispatch goroutines, as above.

	ctx := context.Background()
	const projectID = "proj_costvel_baseline"
	if err := st.CreateProject(ctx, &store.Project{
		ProjectID: projectID, Name: "baseline test", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	now := time.Now().UTC()
	// 55 modest runs spread over four days: normal is established.
	for i := 0; i < 55; i++ {
		if err := st.CreateExecution(ctx, &events.Execution{
			ExecutionID: fmt.Sprintf("exec_bl_hist_%d", i), ProjectID: projectID,
			Status:           events.StatusStarted,
			StartedAt:        now.Add(-time.Duration(96-i) * time.Hour),
			EstimatedCostUSD: 0.10,
		}); err != nil {
			t.Fatalf("create history execution %d: %v", i, err)
		}
	}
	if err := st.CreateExecution(ctx, &events.Execution{
		ExecutionID: "exec_bl_burst", ProjectID: projectID,
		Status: events.StatusStarted, StartedAt: now.Add(-30 * time.Second),
	}); err != nil {
		t.Fatalf("create burst execution: %v", err)
	}

	h := &Handlers{Logger: logger, Store: st, HaltSubs: NewHaltSubscribers()}
	req := httptest.NewRequest("PATCH", "/executions/exec_bl_burst",
		strings.NewReader(`{"status":"completed","estimated_cost_usd":50.0}`))
	req.SetPathValue("id", "exec_bl_burst")
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
		if g.FailureClass == store.FailureClassCostVelocity &&
			strings.HasPrefix(g.Signature, "baseline_x") {
			found = true
		}
	}
	if !found {
		t.Errorf("no baseline_x* cost_velocity group after a burst hundreds "+
			"of times a mature project's normal; groups: %v", groupSignatures(groups))
	}
}

// TestHandleUpdateExecution_BaselineSilentWhileLearning: an
// hour-old project with a handful of runs takes the same $50 burst.
// The fixed-threshold detectors may fire; the baseline form must
// NOT, because a project with no learned normal cannot be abnormal.
func TestHandleUpdateExecution_BaselineSilentWhileLearning(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	// Not closed: detached dispatch goroutines, as above.

	ctx := context.Background()
	const projectID = "proj_costvel_learning"
	if err := st.CreateProject(ctx, &store.Project{
		ProjectID: projectID, Name: "learning gate test", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		if err := st.CreateExecution(ctx, &events.Execution{
			ExecutionID: fmt.Sprintf("exec_lg_hist_%d", i), ProjectID: projectID,
			Status:           events.StatusStarted,
			StartedAt:        now.Add(-time.Duration(60-i) * time.Minute),
			EstimatedCostUSD: 0.10,
		}); err != nil {
			t.Fatalf("create history execution %d: %v", i, err)
		}
	}
	if err := st.CreateExecution(ctx, &events.Execution{
		ExecutionID: "exec_lg_burst", ProjectID: projectID,
		Status: events.StatusStarted, StartedAt: now.Add(-30 * time.Second),
	}); err != nil {
		t.Fatalf("create burst execution: %v", err)
	}

	h := &Handlers{Logger: logger, Store: st, HaltSubs: NewHaltSubscribers()}
	req := httptest.NewRequest("PATCH", "/executions/exec_lg_burst",
		strings.NewReader(`{"status":"completed","estimated_cost_usd":50.0}`))
	req.SetPathValue("id", "exec_lg_burst")
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
	for _, g := range groups {
		if strings.HasPrefix(g.Signature, "baseline_x") {
			t.Errorf("baseline group %q fired for an hour-old project with "+
				"ten runs; the learning gate exists to prevent exactly this", g.Signature)
		}
	}
}

func groupSignatures(groups []*store.FailureGroup) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, g.Signature)
	}
	return out
}
