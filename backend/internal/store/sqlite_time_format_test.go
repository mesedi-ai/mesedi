package store

// Regression tests for the #57 timestamp-format family. Every test
// writes through the REAL store write paths (CreateExecution,
// SaveEvents) and reads through the query that was broken, because
// the entire family survived years of green suites by being tested,
// when tested at all, against hand-formatted fixture strings that
// never matched what the driver actually writes.
//
// The family: executions.started_at and events.timestamp are written
// by the driver's own time.Time serialization, and six queries
// compared them against RFC3339 strings that can never match
// (' ' < 'T' lexically). Retention's DeleteExecutionsOlderThan had
// the worst failure direction: every driver-written row compared
// BELOW the cutoff, so one retention run would have purged a
// project's entire execution history. GetDailyExecutionCounts was
// doubly broken because SQLite's date() cannot parse the old driver
// format at all; OpenSQLite now sets _time_format=sqlite (see
// sqlitedsn.go) so date() works.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"mesedi/backend/internal/events"
)

// newTimeFormatFixture opens a real store and seeds one project with
// three executions written through CreateExecution: two recent (1h
// and 2h old, costs 3.5 and 1.5, tenants A and B) and one ancient
// (240h old, cost 10, tenant A).
func newTimeFormatFixture(t *testing.T) (*SQLiteStore, string, time.Time) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	projectID := fmt.Sprintf("proj_tf_%s", t.Name())
	if err := st.CreateProject(ctx, &Project{
		ProjectID: projectID, Name: "time format test", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	now := time.Now().UTC()
	mk := func(id string, age time.Duration, cost float64, tenant string) {
		t.Helper()
		if err := st.CreateExecution(ctx, &events.Execution{
			ExecutionID: id + "_" + t.Name(), ProjectID: projectID,
			Status: events.StatusStarted, StartedAt: now.Add(-age),
			EstimatedCostUSD: cost, TenantID: &tenant,
		}); err != nil {
			t.Fatalf("create execution %s: %v", id, err)
		}
	}
	mk("exec_recent_1", 1*time.Hour, 3.5, "tenant_a")
	mk("exec_recent_2", 2*time.Hour, 1.5, "tenant_b")
	mk("exec_ancient", 240*time.Hour, 10.0, "tenant_a")
	return st, projectID, now
}

func TestDeleteExecutionsOlderThan_DeletesOnlyOldRows(t *testing.T) {
	// Own fixture with FIXED times, not now-relative ones. The #57
	// defect only decides the comparison when both sides share the
	// same calendar-day prefix (' ' vs 'T' sits at position 10, after
	// the date), so a cutoff on a different day than every row orders
	// correctly by accident and cannot catch a regression. The row
	// that matters is the one on the SAME day as the cutoff but after
	// it: the broken string bound deletes it, the time.Time bound
	// keeps it. That bounds the real-world blast radius too: up to
	// one day of over-deletion per retention run, not the whole
	// history.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	const projectID = "proj_retention_tf"
	if err := st.CreateProject(ctx, &Project{
		ProjectID: projectID, Name: "retention window test", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	cutoff := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	mk := func(id string, at time.Time) {
		t.Helper()
		if err := st.CreateExecution(ctx, &events.Execution{
			ExecutionID: id, ProjectID: projectID,
			Status: events.StatusStarted, StartedAt: at,
		}); err != nil {
			t.Fatalf("create execution %s: %v", id, err)
		}
	}
	mk("exec_same_day_after_cutoff", cutoff.Add(30*time.Minute))
	mk("exec_same_day_before_cutoff", cutoff.Add(-30*time.Minute))
	mk("exec_ten_days_older", cutoff.AddDate(0, 0, -10))

	n, err := st.DeleteExecutionsOlderThan(ctx, projectID, cutoff)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted %d executions, want exactly 2 (same-day-before + "+
			"ten-days-older). 3 means the same-day-after row was deleted, "+
			"the #57 over-deletion direction.", n)
	}
	if _, err := st.GetExecution(ctx, "exec_same_day_after_cutoff"); err != nil {
		t.Errorf("same-day-after-cutoff execution gone after retention delete: %v", err)
	}
}

func TestGetDailyExecutionCounts_RealDaysWithinBounds(t *testing.T) {
	st, projectID, now := newTimeFormatFixture(t)
	ctx := context.Background()

	// Add one execution safely on a different calendar day (49h back)
	// so the grouping has two distinct days inside the window.
	yesterTenant := "tenant_a"
	if err := st.CreateExecution(ctx, &events.Execution{
		ExecutionID: "exec_prior_day_" + t.Name(), ProjectID: projectID,
		Status: events.StatusStarted, StartedAt: now.Add(-49 * time.Hour),
		EstimatedCostUSD: 0.5, TenantID: &yesterTenant,
	}); err != nil {
		t.Fatalf("create prior-day execution: %v", err)
	}

	counts, err := st.GetDailyExecutionCounts(ctx, projectID,
		now.Add(-7*24*time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("daily counts: %v", err)
	}
	var total int64
	for _, c := range counts {
		if c.Date.IsZero() {
			t.Errorf("zero date in daily counts; date(started_at) returned "+
				"NULL, the pre-#57 symptom: %+v", c)
		}
		total += c.Count
	}
	if total != 3 {
		t.Errorf("daily counts sum = %d, want 3 (the two recent fixture "+
			"executions plus the prior-day one; the 240h-old one is outside "+
			"the 7-day window)", total)
	}
	if len(counts) < 2 {
		t.Errorf("got %d distinct days, want at least 2 (recent day + 49h back)", len(counts))
	}
}

func TestCountExecutionsByStatusSince_RespectsCutoff(t *testing.T) {
	st, projectID, now := newTimeFormatFixture(t)

	n, err := st.CountExecutionsByStatusSince(context.Background(), projectID,
		"", now.Add(-3*time.Hour))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("count since 3h = %d, want 2 (the two recent executions, "+
			"not the 240h-old one)", n)
	}
}

func TestGetCostByTenant_RespectsWindow(t *testing.T) {
	st, projectID, now := newTimeFormatFixture(t)

	rows, err := st.GetCostByTenant(context.Background(), projectID,
		now.Add(-3*time.Hour), now.Add(time.Minute), 0)
	if err != nil {
		t.Fatalf("cost by tenant: %v", err)
	}
	got := map[string]float64{}
	for _, r := range rows {
		got[r.TenantID] = r.TotalCostUSD
	}
	if got["tenant_a"] != 3.5 {
		t.Errorf("tenant_a windowed cost = %v, want 3.5 (the recent one only; "+
			"the ancient $10 execution is outside the window)", got["tenant_a"])
	}
	if got["tenant_b"] != 1.5 {
		t.Errorf("tenant_b windowed cost = %v, want 1.5", got["tenant_b"])
	}
}

func TestListModelsAndUserMessages_SeeDriverWrittenEventTimestamps(t *testing.T) {
	st, projectID, now := newTimeFormatFixture(t)
	ctx := context.Background()

	execID := "exec_recent_1_" + t.Name()
	payload := func(m map[string]any) json.RawMessage {
		b, _ := json.Marshal(m)
		return b
	}
	batch := []events.Event{
		{EventID: "evt_tf_1_" + t.Name(), ExecutionID: execID, EventType: "llm_call",
			Sequence: 1, Timestamp: now.Add(-30 * time.Minute),
			Payload: payload(map[string]any{"model": "claude-sonnet-5", "user_message": "recent question"})},
		{EventID: "evt_tf_2_" + t.Name(), ExecutionID: execID, EventType: "llm_call",
			Sequence: 2, Timestamp: now.Add(-72 * time.Hour),
			Payload: payload(map[string]any{"model": "gpt-ancient", "user_message": "old question"})},
	}
	if err := st.SaveEvents(ctx, batch); err != nil {
		t.Fatalf("save events: %v", err)
	}

	models, err := st.ListModelsForProjectSince(ctx, projectID, now.Add(-24*time.Hour), "")
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	if len(models) != 1 || models[0] != "claude-sonnet-5" {
		t.Errorf("models within 24h = %v, want exactly [claude-sonnet-5]; "+
			"empty means the timestamp bound matches nothing (pre-#57), "+
			"two means it matches everything", models)
	}

	msgs, err := st.ListLLMUserMessagesForProjectSince(ctx, projectID, now.Add(-24*time.Hour), "", 0)
	if err != nil {
		t.Fatalf("list user messages: %v", err)
	}
	if len(msgs) != 1 || msgs[0] != "recent question" {
		t.Errorf("user messages within 24h = %v, want exactly [recent question]", msgs)
	}
}
