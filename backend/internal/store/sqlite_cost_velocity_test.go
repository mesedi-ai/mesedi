// Tests for the per-project cost_velocity threshold store methods
// (migration 043, Wave 0.1). Uses the same minimal in-memory schema
// pattern as config_fallbacks_test.go and audit_events_purge_test.go
// so the test stays scoped to the get/set semantics without dragging
// in the full migration sequence.

package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// openMinimalCostVelocityStore creates a SQLite store with just a
// projects table carrying the cost_velocity_threshold_usd column.
// Schema mirrors the relevant subset of migrations 001 + 043.
func openMinimalCostVelocityStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "test.db") +
		"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(off)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`CREATE TABLE projects (
			project_id                    TEXT PRIMARY KEY,
			cost_velocity_threshold_usd   REAL NOT NULL DEFAULT 1.00
		)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return &SQLiteStore{db: db}
}

// seedProject inserts a project row at the default threshold so
// Get/Set tests have something to operate on.
func seedProject(t *testing.T, st *SQLiteStore, projectID string) {
	t.Helper()
	_, err := st.db.ExecContext(context.Background(),
		`INSERT INTO projects (project_id) VALUES (?)`, projectID)
	if err != nil {
		t.Fatalf("seedProject: %v", err)
	}
}

// TestCostVelocityThreshold_DefaultValueAfterMigration verifies that
// a project inserted without an explicit threshold gets the migration
// default ($1.00) — the post-Wave-0 sensible value, raised from the
// broken $0.001 v0.0.1 floor.
func TestCostVelocityThreshold_DefaultValueAfterMigration(t *testing.T) {
	t.Parallel()
	st := openMinimalCostVelocityStore(t)
	seedProject(t, st, "proj-default")

	got, err := st.GetProjectCostVelocityThresholdUSD(
		context.Background(), "proj-default")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != DefaultCostVelocityThresholdUSD {
		t.Errorf("default threshold: want %v got %v",
			DefaultCostVelocityThresholdUSD, got)
	}
}

// TestCostVelocityThreshold_SetGetRoundtrip exercises the write path
// across the full valid range — floor, default, ceiling.
func TestCostVelocityThreshold_SetGetRoundtrip(t *testing.T) {
	t.Parallel()
	st := openMinimalCostVelocityStore(t)
	seedProject(t, st, "proj-rt")

	cases := []float64{
		0.01,      // floor
		0.50,      // cost-sensitive customer
		1.00,      // default
		25.00,     // batch-tolerant customer
		10_000.00, // ceiling
	}
	for _, want := range cases {
		if err := st.SetProjectCostVelocityThresholdUSD(
			context.Background(), "proj-rt", want,
		); err != nil {
			t.Fatalf("Set(%v): %v", want, err)
		}
		got, err := st.GetProjectCostVelocityThresholdUSD(
			context.Background(), "proj-rt")
		if err != nil {
			t.Fatalf("Get after Set(%v): %v", want, err)
		}
		if got != want {
			t.Errorf("roundtrip want %v got %v", want, got)
		}
	}
}

// TestCostVelocityThreshold_GetMissingProjectReturnsNotFound proves
// the store contract used by HandleGetCostVelocityConfig: ErrNotFound
// for unknown project IDs so the handler can map to 404.
func TestCostVelocityThreshold_GetMissingProjectReturnsNotFound(t *testing.T) {
	t.Parallel()
	st := openMinimalCostVelocityStore(t)

	_, err := st.GetProjectCostVelocityThresholdUSD(
		context.Background(), "proj-does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound; got %v", err)
	}
}

// TestCostVelocityThreshold_SetMissingProjectReturnsNotFound mirrors
// the Get test for the write path — also relied on by the handler
// (PUT against unknown project returns 404).
func TestCostVelocityThreshold_SetMissingProjectReturnsNotFound(t *testing.T) {
	t.Parallel()
	st := openMinimalCostVelocityStore(t)

	err := st.SetProjectCostVelocityThresholdUSD(
		context.Background(), "proj-does-not-exist", 2.50)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound; got %v", err)
	}
}

// TestCostVelocityThreshold_PerProjectIsolation verifies a Set on
// project A does not affect project B — important because the
// detector's effective threshold depends on per-execution project
// lookup, not on any global value.
func TestCostVelocityThreshold_PerProjectIsolation(t *testing.T) {
	t.Parallel()
	st := openMinimalCostVelocityStore(t)
	seedProject(t, st, "proj-a")
	seedProject(t, st, "proj-b")

	if err := st.SetProjectCostVelocityThresholdUSD(
		context.Background(), "proj-a", 5.00,
	); err != nil {
		t.Fatalf("Set proj-a: %v", err)
	}

	gotA, err := st.GetProjectCostVelocityThresholdUSD(
		context.Background(), "proj-a")
	if err != nil {
		t.Fatalf("Get proj-a: %v", err)
	}
	if gotA != 5.00 {
		t.Errorf("proj-a: want 5.00 got %v", gotA)
	}

	gotB, err := st.GetProjectCostVelocityThresholdUSD(
		context.Background(), "proj-b")
	if err != nil {
		t.Fatalf("Get proj-b: %v", err)
	}
	if gotB != DefaultCostVelocityThresholdUSD {
		t.Errorf("proj-b should still be default %v; got %v",
			DefaultCostVelocityThresholdUSD, gotB)
	}
}
