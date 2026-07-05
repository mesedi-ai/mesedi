// Tests for the per-project cost_velocity RATE config store methods
// (migration 044, ) AND the CostVelocityRateSignature
// bucketing function. Uses the same minimal in-memory schema pattern
// as sqlite_cost_velocity_test.go.

package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// openMinimalCostVelocityRateStore creates a SQLite store with just a
// projects table carrying the two cost_velocity_rate_* columns.
// Schema mirrors the relevant subset of migrations 001 + 044.
func openMinimalCostVelocityRateStore(t *testing.T) *SQLiteStore {
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
			project_id                                TEXT PRIMARY KEY,
			cost_velocity_rate_threshold_usd_per_min  REAL NOT NULL DEFAULT 5.00,
			cost_velocity_rate_window_minutes         INTEGER NOT NULL DEFAULT 5
		)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return &SQLiteStore{db: db}
}

func seedRateProject(t *testing.T, st *SQLiteStore, projectID string) {
	t.Helper()
	_, err := st.db.ExecContext(context.Background(),
		`INSERT INTO projects (project_id) VALUES (?)`, projectID)
	if err != nil {
		t.Fatalf("seedRateProject: %v", err)
	}
}

func TestCostVelocityRateConfig_DefaultValuesAfterMigration(t *testing.T) {
	t.Parallel()
	st := openMinimalCostVelocityRateStore(t)
	seedRateProject(t, st, "proj-default")

	got, err := st.GetProjectCostVelocityRateConfig(
		context.Background(), "proj-default")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ThresholdUSDPerMin != DefaultCostVelocityRateConfig.ThresholdUSDPerMin {
		t.Errorf("default threshold: want %v got %v",
			DefaultCostVelocityRateConfig.ThresholdUSDPerMin, got.ThresholdUSDPerMin)
	}
	if got.WindowMinutes != DefaultCostVelocityRateConfig.WindowMinutes {
		t.Errorf("default window: want %v got %v",
			DefaultCostVelocityRateConfig.WindowMinutes, got.WindowMinutes)
	}
}

func TestCostVelocityRateConfig_SetGetRoundtrip(t *testing.T) {
	t.Parallel()
	st := openMinimalCostVelocityRateStore(t)
	seedRateProject(t, st, "proj-rt")

	cases := []CostVelocityRateConfig{
		// floor on threshold; mid-range window
		{ThresholdUSDPerMin: 0.10, WindowMinutes: 5},
		// cost-sensitive customer at default window
		{ThresholdUSDPerMin: 1.00, WindowMinutes: 5},
		// default
		{ThresholdUSDPerMin: 5.00, WindowMinutes: 5},
		// batch-tolerant customer with short window
		{ThresholdUSDPerMin: 50.00, WindowMinutes: 1},
		// ceiling on both
		{ThresholdUSDPerMin: 10_000.00, WindowMinutes: 60},
	}
	for _, want := range cases {
		if err := st.SetProjectCostVelocityRateConfig(
			context.Background(), "proj-rt", want,
		); err != nil {
			t.Fatalf("Set(%+v): %v", want, err)
		}
		got, err := st.GetProjectCostVelocityRateConfig(
			context.Background(), "proj-rt")
		if err != nil {
			t.Fatalf("Get after Set(%+v): %v", want, err)
		}
		if got != want {
			t.Errorf("roundtrip want %+v got %+v", want, got)
		}
	}
}

func TestCostVelocityRateConfig_GetMissingProjectReturnsNotFound(t *testing.T) {
	t.Parallel()
	st := openMinimalCostVelocityRateStore(t)

	_, err := st.GetProjectCostVelocityRateConfig(
		context.Background(), "proj-does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound; got %v", err)
	}
}

func TestCostVelocityRateConfig_SetMissingProjectReturnsNotFound(t *testing.T) {
	t.Parallel()
	st := openMinimalCostVelocityRateStore(t)

	err := st.SetProjectCostVelocityRateConfig(
		context.Background(), "proj-does-not-exist",
		CostVelocityRateConfig{ThresholdUSDPerMin: 2.50, WindowMinutes: 5})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound; got %v", err)
	}
}

func TestCostVelocityRateConfig_PerProjectIsolation(t *testing.T) {
	t.Parallel()
	st := openMinimalCostVelocityRateStore(t)
	seedRateProject(t, st, "proj-a")
	seedRateProject(t, st, "proj-b")

	if err := st.SetProjectCostVelocityRateConfig(
		context.Background(), "proj-a",
		CostVelocityRateConfig{ThresholdUSDPerMin: 25.00, WindowMinutes: 15},
	); err != nil {
		t.Fatalf("Set proj-a: %v", err)
	}

	gotA, err := st.GetProjectCostVelocityRateConfig(
		context.Background(), "proj-a")
	if err != nil {
		t.Fatalf("Get proj-a: %v", err)
	}
	if gotA.ThresholdUSDPerMin != 25.00 || gotA.WindowMinutes != 15 {
		t.Errorf("proj-a: want {25.00, 15} got %+v", gotA)
	}

	gotB, err := st.GetProjectCostVelocityRateConfig(
		context.Background(), "proj-b")
	if err != nil {
		t.Fatalf("Get proj-b: %v", err)
	}
	if gotB != DefaultCostVelocityRateConfig {
		t.Errorf("proj-b should still be default %+v; got %+v",
			DefaultCostVelocityRateConfig, gotB)
	}
}

// TestCostVelocityRateSignature_BucketBoundaries exercises every
// boundary in the order-of-magnitude bucketing function. Same shape
// as the absolute CostVelocitySignature so customers see consistent
// dashboard surfaces across both detectors.
func TestCostVelocityRateSignature_BucketBoundaries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ratePerMinUSD float64
		want          string
	}{
		// At the global floor — should land in the first bucket.
		{0.10, "rate_$0.10+_per_min"},
		// Just below the next bucket.
		{0.99, "rate_$0.10+_per_min"},
		// Exactly at the next bucket boundary.
		{1.00, "rate_$1+_per_min"},
		// Mid second bucket.
		{2.50, "rate_$1+_per_min"},
		// At the default-threshold bucket.
		{5.00, "rate_$5+_per_min"},
		{9.99, "rate_$5+_per_min"},
		// Order-of-magnitude up.
		{10.00, "rate_$10+_per_min"},
		{99.99, "rate_$10+_per_min"},
		{100.00, "rate_$100+_per_min"},
		{999.99, "rate_$100+_per_min"},
		// Top bucket — anything pathologically high lands here.
		{1000.00, "rate_$1000+_per_min"},
		{10_000.00, "rate_$1000+_per_min"},
	}
	for _, c := range cases {
		got := CostVelocityRateSignature(c.ratePerMinUSD)
		if got != c.want {
			t.Errorf("CostVelocityRateSignature(%v): want %q got %q",
				c.ratePerMinUSD, c.want, got)
		}
	}
}
