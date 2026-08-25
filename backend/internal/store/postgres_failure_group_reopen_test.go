// Postgres twin of failure_group_reopen_test.go.
//
// This one matters more than the SQLite version: production runs
// Postgres (Neon). The SQLite path serves self-hosters. The two stores
// carry SEPARATE hand-written upserts, so a fix applied to one and not
// the other is invisible to a test that only exercises the other — the
// exact shape of the migration-056 outage, where a SQLite-only change
// left production broken.
//
// Skips when Docker is unavailable; runs for real in CI.

package store

import (
	"context"
	"testing"
	"time"

	"mesedi/backend/internal/events"
)

func TestPostgres_FailureGroupReopensOnRecurrence(t *testing.T) {
	st := newTestPostgresStore(t)
	if st == nil {
		return // Docker unavailable; skipped above.
	}
	ctx := context.Background()

	const (
		projectID    = "proj-reopen-pg"
		failureClass = "crashes"
		signature    = "BoomError: reopen-test-pg"
	)

	if _, err := st.db.ExecContext(ctx, `
		INSERT INTO projects (project_id, name) VALUES ($1, 'reopen-pg');
	`, projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	mkExec := func(id string) {
		t.Helper()
		if err := st.CreateExecution(ctx, &events.Execution{
			ExecutionID: id,
			ProjectID:   projectID,
			Status:      events.StatusCrashed,
			StartedAt:   time.Now().UTC(),
		}); err != nil {
			t.Fatalf("CreateExecution %s: %v", id, err)
		}
	}

	// First occurrence.
	mkExec("exec-pg-reopen-1")
	if _, err := st.groupExecutionInternalPg(
		ctx, "exec-pg-reopen-1", projectID, failureClass, signature,
	); err != nil {
		t.Fatalf("first grouping: %v", err)
	}

	g, err := st.GetFailureGroupByClassSignature(ctx, projectID, failureClass, signature)
	if err != nil || g == nil {
		t.Fatalf("group not created: err=%v group=%v", err, g)
	}

	// Resolve it, and confirm the resolve actually took. Without this
	// assertion the reopen check below could pass for the wrong reason
	// — the group having never been resolved at all.
	if err := st.ResolveFailureGroup(ctx, g.GroupID, projectID, "operator@example.com"); err != nil {
		t.Fatalf("ResolveFailureGroup: %v", err)
	}
	g, err = st.GetFailureGroupByClassSignature(ctx, projectID, failureClass, signature)
	if err != nil || g == nil {
		t.Fatalf("group missing after resolve: err=%v", err)
	}
	if g.ResolvedAt == nil {
		t.Fatal("resolve did not set resolved_at; the rest of this test " +
			"would pass vacuously")
	}
	countBefore := g.EventCount

	// The failure happens again.
	mkExec("exec-pg-reopen-2")
	if _, err := st.groupExecutionInternalPg(
		ctx, "exec-pg-reopen-2", projectID, failureClass, signature,
	); err != nil {
		t.Fatalf("recurrence grouping: %v", err)
	}

	g, err = st.GetFailureGroupByClassSignature(ctx, projectID, failureClass, signature)
	if err != nil || g == nil {
		t.Fatalf("group missing after recurrence: err=%v", err)
	}

	if g.ResolvedAt != nil {
		t.Error("PRODUCTION PATH: group is still resolved after recurring. " +
			"The list filters resolved_at IS NULL and no webhook fires for " +
			"a non-new class, so an actively-firing failure is invisible to " +
			"the customer.")
	}
	if g.ResolvedBy != nil && *g.ResolvedBy != "" {
		t.Errorf("resolved_by should be cleared on reopen, got %q", *g.ResolvedBy)
	}
	if g.EventCount <= countBefore {
		t.Errorf("event_count should still increment on recurrence: "+
			"before=%d after=%d", countBefore, g.EventCount)
	}
}
