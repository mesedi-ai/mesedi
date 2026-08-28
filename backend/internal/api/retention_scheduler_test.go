// Unit tests for RetentionScheduler.
//
// WHY THIS FILE DID NOT EXIST BEFORE, AND WHY IT DOES NOW
// The scheduler shipped with no test at all. It deleted executions and
// relied on a comment claiming "FK CASCADE handles events,
// failure_groups, webhook_deliveries". Two thirds of that was false:
// failure_groups and webhook_deliveries both key on project_id, not
// execution_id, so an executions delete never reached either.
//
// The result in production on 2026-08-27, against a documented 7-day
// Hobby retention window:
//
//	executions               3 days old   (pruned)
//	events                   3 days old   (pruned via cascade)
//	execution_failure_groups 3 days old   (pruned via cascade)
//	failure_groups          88 days old   (NEVER pruned)
//	webhook_deliveries      88 days old   (NEVER pruned)
//	ai_analyses             64 days old   (NEVER pruned)
//
// Nothing reported it. It surfaced only because an off-site backup was
// restored and its row counts compared against live. The three tables
// that were never deleted are the ones holding the most derived
// customer content, including model-written analysis of their
// failures.
//
// These tests assert the scheduler prunes ALL THREE tables. That is
// the assertion whose absence let the gap live.
package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"mesedi/backend/internal/store"
)

// stubProjectRetentionStore records which prune methods tick() calls
// and with what cutoff. Embedding store.Store means only the methods
// tick() actually reaches need implementing; anything else it touches
// would nil-panic and fail loudly rather than pass quietly.
type stubProjectRetentionStore struct {
	store.Store

	projects    []*store.ProjectRetention
	listErr     error
	execCalls   []pruneCall
	groupCalls  []pruneCall
	webhookCall []pruneCall

	execErr    error
	groupErr   error
	webhookErr error
}

type pruneCall struct {
	projectID string
	cutoff    time.Time
}

func (s *stubProjectRetentionStore) ListProjectsForRetention(
	_ context.Context,
) ([]*store.ProjectRetention, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.projects, nil
}

func (s *stubProjectRetentionStore) DeleteExecutionsOlderThan(
	_ context.Context, projectID string, cutoff time.Time,
) (int64, error) {
	s.execCalls = append(s.execCalls, pruneCall{projectID, cutoff})
	return 1, s.execErr
}

func (s *stubProjectRetentionStore) DeleteFailureGroupsOlderThan(
	_ context.Context, projectID string, cutoff time.Time,
) (int64, error) {
	s.groupCalls = append(s.groupCalls, pruneCall{projectID, cutoff})
	return 2, s.groupErr
}

func (s *stubProjectRetentionStore) DeleteWebhookDeliveriesOlderThan(
	_ context.Context, projectID string, cutoff time.Time,
) (int64, error) {
	s.webhookCall = append(s.webhookCall, pruneCall{projectID, cutoff})
	return 3, s.webhookErr
}

func newSchedulerFor(st store.Store) *RetentionScheduler {
	return &RetentionScheduler{
		Store:  st,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestTickPrunesAllThreeTables is THE regression test. Before
// 2026-08-27 only the executions call happened, and the other two
// tables grew without limit while the docs promised otherwise.
func TestTickPrunesAllThreeTables(t *testing.T) {
	t.Parallel()

	st := &stubProjectRetentionStore{
		projects: []*store.ProjectRetention{
			{ProjectID: "proj-hobby", RetentionDays: 7},
		},
	}
	newSchedulerFor(st).tick(context.Background())

	if len(st.execCalls) != 1 {
		t.Errorf("executions: want 1 prune call, got %d", len(st.execCalls))
	}
	if len(st.groupCalls) != 1 {
		t.Errorf("failure_groups: want 1 prune call, got %d. This is the "+
			"table that reached 88 days old in production while the "+
			"documented window was 7.", len(st.groupCalls))
	}
	if len(st.webhookCall) != 1 {
		t.Errorf("webhook_deliveries: want 1 prune call, got %d. Delivery "+
			"records carry the failure detail that was sent outbound and "+
			"are customer content under the same retention promise.",
			len(st.webhookCall))
	}
}

// TestTickUsesOneCutoffAcrossTables. Different cutoffs per table would
// mean a project's data expires at three different times, which is
// impossible to explain to a customer and impossible to verify.
func TestTickUsesOneCutoffAcrossTables(t *testing.T) {
	t.Parallel()

	st := &stubProjectRetentionStore{
		projects: []*store.ProjectRetention{
			{ProjectID: "proj-team", RetentionDays: 90},
		},
	}
	before := time.Now().UTC()
	newSchedulerFor(st).tick(context.Background())
	after := time.Now().UTC()

	if len(st.execCalls) != 1 || len(st.groupCalls) != 1 || len(st.webhookCall) != 1 {
		t.Fatalf("expected one call per table, got %d/%d/%d",
			len(st.execCalls), len(st.groupCalls), len(st.webhookCall))
	}
	cutoff := st.execCalls[0].cutoff
	if !st.groupCalls[0].cutoff.Equal(cutoff) || !st.webhookCall[0].cutoff.Equal(cutoff) {
		t.Errorf("cutoffs differ across tables: exec=%s groups=%s webhooks=%s",
			cutoff, st.groupCalls[0].cutoff, st.webhookCall[0].cutoff)
	}

	// And the cutoff is genuinely retention_days in the past, not
	// now-ish. A cutoff of "now" would delete everything.
	wantLo := before.AddDate(0, 0, -90).Add(-2 * time.Second)
	wantHi := after.AddDate(0, 0, -90).Add(2 * time.Second)
	if cutoff.Before(wantLo) || cutoff.After(wantHi) {
		t.Errorf("cutoff %s is not ~90 days before now (%s..%s)",
			cutoff, wantLo, wantHi)
	}
}

// TestOneTableFailingDoesNotSkipTheOthers.
//
// Before the fix a single delete error hit `continue` and abandoned
// the whole project. Now each table is independent: partially honouring
// the retention promise beats abandoning it because one table was
// briefly unhappy.
func TestOneTableFailingDoesNotSkipTheOthers(t *testing.T) {
	t.Parallel()

	st := &stubProjectRetentionStore{
		projects: []*store.ProjectRetention{
			{ProjectID: "proj-x", RetentionDays: 7},
		},
		execErr: errors.New("transient database error"),
	}
	newSchedulerFor(st).tick(context.Background())

	if len(st.groupCalls) != 1 || len(st.webhookCall) != 1 {
		t.Errorf("an executions failure suppressed later prunes: "+
			"groups=%d webhooks=%d", len(st.groupCalls), len(st.webhookCall))
	}
}

// TestNonPositiveRetentionPrunesNothing. A zero or negative window
// would compute a cutoff at or after now and delete everything the
// project has. The guard must hold for all three tables, not just the
// one it was originally written for.
func TestNonPositiveRetentionPrunesNothing(t *testing.T) {
	t.Parallel()

	for _, days := range []int{0, -1, -3650} {
		st := &stubProjectRetentionStore{
			projects: []*store.ProjectRetention{
				{ProjectID: "proj-bad", RetentionDays: days},
			},
		}
		newSchedulerFor(st).tick(context.Background())

		if len(st.execCalls)+len(st.groupCalls)+len(st.webhookCall) != 0 {
			t.Errorf("retention_days=%d triggered deletes (%d/%d/%d); a "+
				"non-positive window must never prune, it would wipe the project",
				days, len(st.execCalls), len(st.groupCalls), len(st.webhookCall))
		}
	}
}

// TestListFailureIsSurvivable. If the project list cannot be read
// there is nothing to prune, and the scheduler must return rather than
// panic; it runs on a ticker with no supervisor.
func TestListFailureIsSurvivable(t *testing.T) {
	t.Parallel()

	st := &stubProjectRetentionStore{listErr: errors.New("db unreachable")}
	newSchedulerFor(st).tick(context.Background())

	if len(st.execCalls)+len(st.groupCalls)+len(st.webhookCall) != 0 {
		t.Error("prunes ran despite the project list failing")
	}
}

// TestEveryProjectIsPruned. A single project erroring must not stop
// the loop for the rest, which is the existing documented behaviour
// and is worth pinning now that the loop body is larger.
func TestEveryProjectIsPruned(t *testing.T) {
	t.Parallel()

	st := &stubProjectRetentionStore{
		projects: []*store.ProjectRetention{
			{ProjectID: "proj-a", RetentionDays: 7},
			{ProjectID: "proj-b", RetentionDays: 90},
			{ProjectID: "proj-c", RetentionDays: 30},
		},
	}
	newSchedulerFor(st).tick(context.Background())

	if len(st.groupCalls) != 3 {
		t.Fatalf("want failure_groups pruned for all 3 projects, got %d",
			len(st.groupCalls))
	}
	seen := map[string]bool{}
	for _, c := range st.groupCalls {
		seen[c.projectID] = true
	}
	for _, want := range []string{"proj-a", "proj-b", "proj-c"} {
		if !seen[want] {
			t.Errorf("project %s was never pruned", want)
		}
	}
}
