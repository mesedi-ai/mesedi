package store

// Tests for execution sealing (migration 057).
//
// The two that carry the correctness property:
//
//   TestSealExecutions_DoesNotSealAnExecutionStillSettling
//     — sealing early anchors a digest over a partial event record.
//       When the remaining events land the root changes, and a changed
//       root reads as TAMPERING rather than as a race. False alarms are
//       worse than silence: an auditor who watches the chain cry wolf
//       stops believing it when it is right.
//
//   TestSealExecutions_IsIdempotent
//     — interval membership must be recorded once and never
//       recomputed, or the chain anchors a fact that moves underneath
//       it.
//
// Postgres twin not included, per the project's documented B18
// exemption in .git/foundation_audit.conf: no store sidecar has a
// Postgres twin test yet, the SQLite pattern is the established style,
// and the Postgres CI path is tracked as its own task. The Postgres
// IMPLEMENTATION does exist (postgres_execution_sealing.go) — shipping
// one backend and not the other is what caused the 056 outage.

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"mesedi/backend/internal/events"
)

func sealingStore(t *testing.T, name string) *SQLiteStore {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := OpenSQLite(filepath.Join(t.TempDir(), name+".db"), logger)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Named sealSeedProject, not seedProject: sqlite_cost_velocity_test.go
// already declares a seedProject with an identical signature in this
// package. Reusing theirs would couple these tests to that file's
// setup choices; a distinct helper keeps the two independent.
func sealSeedProject(t *testing.T, s *SQLiteStore, projectID string) {
	t.Helper()
	if err := s.CreateProject(context.Background(), &Project{
		ProjectID:  projectID,
		Name:       projectID,
		OwnerEmail: projectID + "@example.com",
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateProject(%s): %v", projectID, err)
	}
}

// seedExecution creates one execution. endedAt nil means "still
// running", which is the case the timeout path exists for.
func seedExecution(t *testing.T, s *SQLiteStore, projectID, execID string,
	startedAt time.Time, endedAt *time.Time) {
	t.Helper()
	// StatusStarted, not StatusRunning — the latter does not exist in
	// this codebase. The status set is StatusStarted, StatusAwaitingHuman,
	// StatusCompleted, StatusCrashed, StatusHalted, StatusTimeout,
	// StatusValidationFailed.
	status := events.StatusCompleted
	if endedAt == nil {
		status = events.StatusStarted
	}
	if err := s.CreateExecution(context.Background(), &events.Execution{
		ExecutionID: execID,
		ProjectID:   projectID,
		Status:      status,
		StartedAt:   startedAt,
		EndedAt:     endedAt,
	}); err != nil {
		t.Fatalf("CreateExecution(%s): %v", execID, err)
	}
}

func ptr(t time.Time) *time.Time { return &t }

// ── sealing eligibility ──────────────────────────────────────────────

// An execution that ended only moments ago must NOT be sealed: events
// can still arrive, and anchoring a digest that later changes produces
// a false tamper alarm.
func TestSealExecutions_DoesNotSealAnExecutionStillSettling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := sealingStore(t, "settling")
	sealSeedProject(t, s, "proj-a")

	now := time.Date(2026, 9, 3, 15, 0, 0, 0, time.UTC)
	const settle = 15 * time.Minute
	const timeout = 6 * time.Hour

	// Ended one minute ago — well inside the grace period.
	seedExecution(t, s, "proj-a", "exec-fresh",
		now.Add(-10*time.Minute), ptr(now.Add(-time.Minute)))

	n, err := s.SealExecutions(ctx, now, settle, timeout)
	if err != nil {
		t.Fatalf("SealExecutions: %v", err)
	}
	if n != 0 {
		t.Fatalf("sealed %d executions; an execution one minute past ending is "+
			"still settling and sealing it would anchor a partial record", n)
	}

	// Once the grace period has elapsed, it seals.
	later := now.Add(settle + time.Minute)
	n, err = s.SealExecutions(ctx, later, settle, timeout)
	if err != nil {
		t.Fatalf("SealExecutions (later): %v", err)
	}
	if n != 1 {
		t.Errorf("sealed %d after the grace period, want 1", n)
	}
}

// An execution that never ends must not stay outside the chain
// forever: an omission nobody can see is the exact attack the chain
// exists to expose.
func TestSealExecutions_TimesOutAnExecutionThatNeverEnded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := sealingStore(t, "timeout")
	sealSeedProject(t, s, "proj-a")

	now := time.Date(2026, 9, 3, 15, 0, 0, 0, time.UTC)
	const settle = 15 * time.Minute
	const timeout = 6 * time.Hour

	seedExecution(t, s, "proj-a", "exec-recent-running", now.Add(-time.Hour), nil)
	seedExecution(t, s, "proj-a", "exec-abandoned", now.Add(-7*time.Hour), nil)

	n, err := s.SealExecutions(ctx, now, settle, timeout)
	if err != nil {
		t.Fatalf("SealExecutions: %v", err)
	}
	if n != 1 {
		t.Fatalf("sealed %d, want 1 (only the abandoned one has timed out)", n)
	}

	ids, err := s.ListSealedExecutionIDs(ctx, "proj-a", now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ListSealedExecutionIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "exec-abandoned" {
		t.Errorf("sealed the wrong execution: %v", ids)
	}
}

// Interval membership is recorded once. A second pass must not move an
// execution, or the chain anchors a fact that changes underneath it.
func TestSealExecutions_IsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := sealingStore(t, "idempotent")
	sealSeedProject(t, s, "proj-a")

	firstPass := time.Date(2026, 9, 3, 15, 0, 0, 0, time.UTC)
	seedExecution(t, s, "proj-a", "exec-1",
		firstPass.Add(-2*time.Hour), ptr(firstPass.Add(-time.Hour)))

	n, err := s.SealExecutions(ctx, firstPass, 15*time.Minute, 6*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("first pass sealed %d (err %v), want 1", n, err)
	}

	// A second pass an hour later must seal nothing...
	secondPass := firstPass.Add(time.Hour)
	n, err = s.SealExecutions(ctx, secondPass, 15*time.Minute, 6*time.Hour)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if n != 0 {
		t.Errorf("second pass re-sealed %d rows; sealed_at is being overwritten "+
			"and executions can move between checkpoints", n)
	}

	// ...and the execution must still sit in the FIRST pass's interval.
	ids, err := s.ListSealedExecutionIDs(ctx, "proj-a",
		firstPass.Add(-time.Minute), firstPass.Add(time.Minute))
	if err != nil {
		t.Fatalf("ListSealedExecutionIDs: %v", err)
	}
	if len(ids) != 1 {
		t.Errorf("execution left its original interval: %v", ids)
	}
}

func TestSealExecutions_RejectsNonsenseWindows(t *testing.T) {
	t.Parallel()
	s := sealingStore(t, "bad-args")
	now := time.Now().UTC()
	if _, err := s.SealExecutions(context.Background(), now, -time.Minute, time.Hour); err == nil {
		t.Error("negative settle should be refused")
	}
	if _, err := s.SealExecutions(context.Background(), now, time.Minute, 0); err == nil {
		t.Error("zero timeout should be refused")
	}
}

// ── interval queries ─────────────────────────────────────────────────

// Adjacent checkpoints share a boundary instant. A closed upper bound
// would count a row sealed exactly on the hour in BOTH intervals,
// inflating one cumulative count so the chain's own arithmetic fails to
// reconcile.
func TestCountSealedByProject_BoundaryIsHalfOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := sealingStore(t, "boundary")
	sealSeedProject(t, s, "proj-a")

	boundary := time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC)
	// Seal one execution exactly ON the boundary instant.
	seedExecution(t, s, "proj-a", "exec-on-boundary",
		boundary.Add(-2*time.Hour), ptr(boundary.Add(-time.Hour)))
	if _, err := s.SealExecutions(ctx, boundary, 15*time.Minute, 6*time.Hour); err != nil {
		t.Fatalf("SealExecutions: %v", err)
	}

	earlier, err := s.CountSealedByProject(ctx, boundary.Add(-time.Hour), boundary)
	if err != nil {
		t.Fatalf("CountSealedByProject (earlier): %v", err)
	}
	later, err := s.CountSealedByProject(ctx, boundary, boundary.Add(time.Hour))
	if err != nil {
		t.Fatalf("CountSealedByProject (later): %v", err)
	}

	if earlier["proj-a"]+later["proj-a"] != 1 {
		t.Errorf("an execution sealed exactly on the boundary was counted %d times "+
			"across two adjacent intervals, want exactly 1 (earlier=%d later=%d)",
			earlier["proj-a"]+later["proj-a"], earlier["proj-a"], later["proj-a"])
	}
	if later["proj-a"] != 1 {
		t.Errorf("the boundary instant should belong to the interval that STARTS "+
			"with it, got earlier=%d later=%d", earlier["proj-a"], later["proj-a"])
	}
}

func TestCountSealedByProject_SeparatesProjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := sealingStore(t, "projects")
	sealSeedProject(t, s, "proj-a")
	sealSeedProject(t, s, "proj-b")

	now := time.Date(2026, 9, 3, 15, 0, 0, 0, time.UTC)
	ended := ptr(now.Add(-time.Hour))
	seedExecution(t, s, "proj-a", "a-1", now.Add(-2*time.Hour), ended)
	seedExecution(t, s, "proj-a", "a-2", now.Add(-2*time.Hour), ended)
	seedExecution(t, s, "proj-b", "b-1", now.Add(-2*time.Hour), ended)

	if _, err := s.SealExecutions(ctx, now, 15*time.Minute, 6*time.Hour); err != nil {
		t.Fatalf("SealExecutions: %v", err)
	}
	counts, err := s.CountSealedByProject(ctx, now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("CountSealedByProject: %v", err)
	}
	if counts["proj-a"] != 2 || counts["proj-b"] != 1 {
		t.Errorf("counts = %v, want proj-a:2 proj-b:1", counts)
	}

	// And the id list must never cross projects.
	ids, err := s.ListSealedExecutionIDs(ctx, "proj-b", now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ListSealedExecutionIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "b-1" {
		t.Errorf("proj-b list leaked another project's executions: %v", ids)
	}
}

// The Merkle root depends on leaf order, and a single seal pass stamps
// every row it touches with the SAME instant — so ties are routine, and
// without the execution_id tiebreak two runs could order them
// differently and produce two different roots for identical data. The
// second would look like tampering.
func TestListSealedExecutionIDs_OrderIsDeterministicOnTies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := sealingStore(t, "ordering")
	sealSeedProject(t, s, "proj-a")

	now := time.Date(2026, 9, 3, 15, 0, 0, 0, time.UTC)
	ended := ptr(now.Add(-time.Hour))
	// Inserted out of lexical order on purpose.
	for _, id := range []string{"exec-c", "exec-a", "exec-b"} {
		seedExecution(t, s, "proj-a", id, now.Add(-2*time.Hour), ended)
	}
	// One pass, so all three share a sealed_at.
	if _, err := s.SealExecutions(ctx, now, 15*time.Minute, 6*time.Hour); err != nil {
		t.Fatalf("SealExecutions: %v", err)
	}

	want := []string{"exec-a", "exec-b", "exec-c"}
	for attempt := 0; attempt < 3; attempt++ {
		ids, err := s.ListSealedExecutionIDs(ctx, "proj-a",
			now.Add(-time.Minute), now.Add(time.Minute))
		if err != nil {
			t.Fatalf("ListSealedExecutionIDs: %v", err)
		}
		if len(ids) != len(want) {
			t.Fatalf("got %d ids, want %d", len(ids), len(want))
		}
		for i := range want {
			if ids[i] != want[i] {
				t.Fatalf("attempt %d: order = %v, want %v — ties on sealed_at are "+
					"not being broken deterministically, so the Merkle root would "+
					"differ between runs on identical data", attempt, ids, want)
			}
		}
	}
}

// ── memory bounds ────────────────────────────────────────────────────

// The caps must REFUSE, never truncate. A short result set would build
// a perfectly valid-looking checkpoint over an incomplete set of
// executions, and no verifier could ever tell — which is exactly the
// omission this whole mechanism exists to expose. Loud failure is
// recoverable; a silently truncated tree is not.
//
// Not t.Parallel(): these mutate package-level caps.
func TestIntervalQueries_RefuseRatherThanTruncate(t *testing.T) {
	ctx := context.Background()
	s := sealingStore(t, "caps")
	sealSeedProject(t, s, "proj-a")
	sealSeedProject(t, s, "proj-b")

	now := time.Date(2026, 9, 3, 15, 0, 0, 0, time.UTC)
	ended := ptr(now.Add(-time.Hour))
	seedExecution(t, s, "proj-a", "a-1", now.Add(-2*time.Hour), ended)
	seedExecution(t, s, "proj-a", "a-2", now.Add(-2*time.Hour), ended)
	seedExecution(t, s, "proj-b", "b-1", now.Add(-2*time.Hour), ended)
	if _, err := s.SealExecutions(ctx, now, 15*time.Minute, 6*time.Hour); err != nil {
		t.Fatalf("SealExecutions: %v", err)
	}
	from, to := now.Add(-time.Minute), now.Add(time.Minute)

	t.Run("execution list refuses past the cap", func(t *testing.T) {
		orig := MaxSealedExecutionsPerProjectPerInterval
		MaxSealedExecutionsPerProjectPerInterval = 1
		defer func() { MaxSealedExecutionsPerProjectPerInterval = orig }()

		ids, err := s.ListSealedExecutionIDs(ctx, "proj-a", from, to)
		if err == nil {
			t.Fatalf("proj-a has 2 executions with a cap of 1 but the call "+
				"succeeded, returning %d ids — a truncated list would exclude "+
				"executions from the Merkle root invisibly", len(ids))
		}
		if ids != nil {
			t.Error("a refusal must not also return partial results; a caller " +
				"ignoring the error would anchor an incomplete tree")
		}
	})

	t.Run("project map refuses past the cap", func(t *testing.T) {
		orig := MaxProjectsPerInterval
		MaxProjectsPerInterval = 1
		defer func() { MaxProjectsPerInterval = orig }()

		counts, err := s.CountSealedByProject(ctx, from, to)
		if err == nil {
			t.Fatalf("2 projects active with a cap of 1 but the call succeeded, "+
				"returning %d entries — dropping a tenant from the tree is the "+
				"omission attack", len(counts))
		}
		if counts != nil {
			t.Error("a refusal must not also return partial results")
		}
	})

	// And with the real caps in place, the same data comes back whole.
	ids, err := s.ListSealedExecutionIDs(ctx, "proj-a", from, to)
	if err != nil || len(ids) != 2 {
		t.Errorf("under the real cap: %d ids, err %v; want 2 and nil", len(ids), err)
	}
}

func TestIntervalQueries_RejectInvertedWindows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := sealingStore(t, "inverted")
	now := time.Now().UTC()

	if _, err := s.CountSealedByProject(ctx, now, now.Add(-time.Hour)); err == nil {
		t.Error("CountSealedByProject accepted an inverted window")
	}
	if _, err := s.CountSealedByProject(ctx, now, now); err == nil {
		t.Error("CountSealedByProject accepted a zero-width window")
	}
	if _, err := s.ListSealedExecutionIDs(ctx, "proj-a", now, now.Add(-time.Hour)); err == nil {
		t.Error("ListSealedExecutionIDs accepted an inverted window")
	}
	if _, err := s.ListSealedExecutionIDs(ctx, "", now.Add(-time.Hour), now); err == nil {
		t.Error("ListSealedExecutionIDs accepted an empty project_id")
	}
}
