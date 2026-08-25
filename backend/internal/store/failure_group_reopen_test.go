package store

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"mesedi/backend/internal/events"
)

// A resolved failure group that fires again must REOPEN.
//
// The bug this pins: the recurrence upsert incremented event_count,
// affected_executions and last_seen but never cleared resolved_at. The
// group list filters `resolved_at IS NULL`, so a resolved group that
// kept failing became invisible — counters climbing behind a row the
// customer could not see, and no webhook, because the failure class
// was not new.
//
// The lived consequence: ship a fix that does not work, click Resolve,
// and get no signal that the failure is still happening. The product
// told you it was handled.
//
// Sentry reopens an issue on regression; that is what "Resolve" means
// to an engineer, and Mesedi is positioned as Sentry for AI agents.
func TestFailureGroupReopensOnRecurrence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := OpenSQLite(filepath.Join(dir, "reopen.db"), logger)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const (
		projectID    = "proj-reopen"
		failureClass = "crashes"
		signature    = "BoomError: reopen-test"
	)

	if err := s.CreateProject(ctx, &Project{
		ProjectID:  projectID,
		Name:       "reopen",
		OwnerEmail: "reopen@example.com",
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	mkExec := func(id string) {
		t.Helper()
		if err := s.CreateExecution(ctx, &events.Execution{
			ExecutionID: id,
			ProjectID:   projectID,
			Status:      events.StatusCrashed,
			StartedAt:   time.Now().UTC(),
		}); err != nil {
			t.Fatalf("CreateExecution %s: %v", id, err)
		}
	}

	// ── First occurrence ────────────────────────────────────────────
	mkExec("exec-reopen-1")
	if _, err := s.groupExecutionInternal(
		ctx, "exec-reopen-1", projectID, failureClass, signature,
	); err != nil {
		t.Fatalf("first grouping: %v", err)
	}

	g, err := s.GetFailureGroupByClassSignature(ctx, projectID, failureClass, signature)
	if err != nil || g == nil {
		t.Fatalf("group not created: err=%v group=%v", err, g)
	}
	if g.ResolvedAt != nil {
		t.Fatal("a brand-new group must not be resolved")
	}

	// ── Customer resolves it ────────────────────────────────────────
	if err := s.ResolveFailureGroup(ctx, g.GroupID, projectID, "operator@example.com"); err != nil {
		t.Fatalf("ResolveFailureGroup: %v", err)
	}
	g, err = s.GetFailureGroupByClassSignature(ctx, projectID, failureClass, signature)
	if err != nil || g == nil {
		t.Fatalf("group missing after resolve: err=%v", err)
	}
	if g.ResolvedAt == nil {
		t.Fatal("resolve did not set resolved_at — the rest of this " +
			"test would pass vacuously")
	}
	countBefore := g.EventCount

	// ── The failure happens again ───────────────────────────────────
	mkExec("exec-reopen-2")
	if _, err := s.groupExecutionInternal(
		ctx, "exec-reopen-2", projectID, failureClass, signature,
	); err != nil {
		t.Fatalf("recurrence grouping: %v", err)
	}

	g, err = s.GetFailureGroupByClassSignature(ctx, projectID, failureClass, signature)
	if err != nil || g == nil {
		t.Fatalf("group missing after recurrence: err=%v", err)
	}

	if g.ResolvedAt != nil {
		t.Error("group is STILL RESOLVED after recurring. The customer " +
			"would see nothing: the list filters resolved_at IS NULL and " +
			"no webhook fires for a non-new class, so a failure that is " +
			"actively happening is invisible.")
	}
	if g.ResolvedBy != nil && *g.ResolvedBy != "" {
		t.Errorf("resolved_by should be cleared on reopen, got %q; a "+
			"reopened group must not credit its state to whoever closed "+
			"it last", *g.ResolvedBy)
	}
	if g.EventCount <= countBefore {
		t.Errorf("event_count should still increment on recurrence: "+
			"before=%d after=%d", countBefore, g.EventCount)
	}
}
