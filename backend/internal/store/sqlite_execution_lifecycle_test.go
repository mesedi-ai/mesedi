package store

// Coverage-debt batch two: the pause/resume transition matrix the
// store enforces (the invariant holds even if a caller bypasses the
// HTTP API, which is the documented reason it lives here), the cost
// setter the cost-velocity path writes through, the event counter
// the step-count detector reads, and the per-period usage counters
// billing depends on.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

func newLifecycleTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.CreateProject(context.Background(), &Project{
		ProjectID: "proj_lc", Name: "execution lifecycle", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := st.CreateExecution(context.Background(), &events.Execution{
		ExecutionID: "exec_lc", ProjectID: "proj_lc",
		Status: events.StatusStarted, StartedAt: time.Now().UTC().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("create execution: %v", err)
	}
	return st
}

func TestPauseResumeTransitionMatrix(t *testing.T) {
	st := newLifecycleTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// A started run cannot resume: resume's precondition is paused.
	if err := st.ResumeExecution(ctx, "exec_lc", "proj_lc", now); !errors.Is(err, ErrInvalidLifecycleTransition) {
		t.Fatalf("resume of a started run = %v, want ErrInvalidLifecycleTransition", err)
	}
	if err := st.PauseExecution(ctx, "exec_lc", "proj_lc", now); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// Double pause is exactly as invalid as premature resume.
	if err := st.PauseExecution(ctx, "exec_lc", "proj_lc", now); !errors.Is(err, ErrInvalidLifecycleTransition) {
		t.Fatalf("double pause = %v, want ErrInvalidLifecycleTransition", err)
	}
	if err := st.ResumeExecution(ctx, "exec_lc", "proj_lc", now.Add(time.Second)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got, err := st.GetExecution(ctx, "exec_lc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != events.StatusStarted {
		t.Fatalf("status after resume = %q, want started", got.Status)
	}
	// The wrong project id must never move another tenant's run, and
	// the refusal is deliberately ErrNotFound rather than the
	// transition error: a caller probing with a foreign project id
	// learns nothing about whether the run exists or what state it
	// is in.
	if err := st.PauseExecution(ctx, "exec_lc", "proj_other", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project pause = %v, want ErrNotFound", err)
	}
}

func TestExecutionCostAndEventCount(t *testing.T) {
	st := newLifecycleTestStore(t)
	ctx := context.Background()

	if err := st.SetExecutionCost(ctx, "exec_lc", 1.25); err != nil {
		t.Fatalf("set cost: %v", err)
	}
	got, err := st.GetExecution(ctx, "exec_lc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.EstimatedCostUSD != 1.25 {
		t.Fatalf("cost = %v, want 1.25", got.EstimatedCostUSD)
	}

	payload, _ := json.Marshal(map[string]any{"note": "count me"})
	for i := 1; i <= 3; i++ {
		if err := st.SaveEvents(ctx, []events.Event{{
			EventID: "evt_lc_" + string(rune('0'+i)), ExecutionID: "exec_lc",
			EventType: events.EventTypeCheckpoint, Sequence: i,
			Timestamp: time.Now().UTC(), Payload: payload,
		}}); err != nil {
			t.Fatalf("save event %d: %v", i, err)
		}
	}
	n, err := st.CountEventsForExecution(ctx, "exec_lc")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("event count = %d, want 3", n)
	}
}

func TestPeriodUsageCountersIncrementAndReset(t *testing.T) {
	st := newLifecycleTestStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := st.IncrementExecutionsThisPeriod(ctx, "proj_lc"); err != nil {
			t.Fatalf("increment %d: %v", i, err)
		}
	}
	p, err := st.GetProject(ctx, "proj_lc")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if p.ExecutionsThisPeriod != 3 {
		t.Fatalf("executions_this_period = %d, want 3", p.ExecutionsThisPeriod)
	}
	periodStart := time.Now().UTC()
	if err := st.ResetExecutionsThisPeriod(ctx, "proj_lc", periodStart, periodStart.AddDate(0, 1, 0)); err != nil {
		t.Fatalf("reset: %v", err)
	}
	p, err = st.GetProject(ctx, "proj_lc")
	if err != nil {
		t.Fatalf("get project after reset: %v", err)
	}
	if p.ExecutionsThisPeriod != 0 {
		t.Fatalf("after reset = %d, want 0; billing depends on this being exact", p.ExecutionsThisPeriod)
	}
}
