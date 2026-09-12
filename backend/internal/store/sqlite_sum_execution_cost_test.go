package store

// Regression test for SumExecutionCostByProjectSince against rows
// written by the REAL CreateExecution write path.
//
// Why this wording matters: the method had no test at all, and the
// pre-existing cost-velocity store tests build their own minimal
// schemas and never touch the executions table. That gap hid a bug
// where the since-bound was passed as an RFC3339 string while
// started_at is stored in the driver's own time.Time format; the two
// formats never compare equal-order (' ' < 'T'), so the window
// matched zero rows forever and the rate detector was silent on
// SQLite. Any test that inserts rows with hand-formatted timestamp
// strings would have "passed" right over it, which is why this one
// goes through CreateExecution and nothing else.

import (
	"context"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

func TestSumExecutionCostByProjectSince_SeesDriverWrittenRows(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	const projectID = "proj_sum_cost_test"
	if err := st.CreateProject(ctx, &Project{
		ProjectID: projectID,
		Name:      "sum cost window test",
		Tier:      "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	now := time.Now().UTC()
	mk := func(id string, startedAt time.Time, cost float64) {
		t.Helper()
		if err := st.CreateExecution(ctx, &events.Execution{
			ExecutionID:      id,
			ProjectID:        projectID,
			Status:           events.StatusStarted,
			StartedAt:        startedAt,
			EstimatedCostUSD: cost,
		}); err != nil {
			t.Fatalf("create execution %s: %v", id, err)
		}
	}
	mk("exec_in_window_1", now.Add(-1*time.Minute), 3.5)
	mk("exec_in_window_2", now.Add(-3*time.Minute), 1.5)
	mk("exec_before_window", now.Add(-2*time.Hour), 10.0)

	cost, count, err := st.SumExecutionCostByProjectSince(ctx, projectID, now.Add(-5*time.Minute))
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if count != 2 {
		t.Errorf("window count = %d, want 2 (the two recent executions, "+
			"not the 2h-old one)", count)
	}
	if math.Abs(cost-5.0) > 1e-9 {
		t.Errorf("window cost = %v, want 5.0 (3.5 + 1.5, excluding the "+
			"$10 execution outside the window)", cost)
	}

	// Zero since disables the bound entirely: all three rows.
	cost, count, err = st.SumExecutionCostByProjectSince(ctx, projectID, time.Time{})
	if err != nil {
		t.Fatalf("sum unbounded: %v", err)
	}
	if count != 3 || math.Abs(cost-15.0) > 1e-9 {
		t.Errorf("unbounded sum = (%v, %d), want (15.0, 3)", cost, count)
	}
}
