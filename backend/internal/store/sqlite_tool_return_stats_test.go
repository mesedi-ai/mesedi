// Regression test for the NULL-into-int Scan crash on
// GetToolReturnValueStats when no tool_call events exist in the
// window. SUM(CASE WHEN ... THEN 1 ELSE 0 END) over zero matching rows
// returns NULL in both SQLite and Postgres; without the COALESCE wraps
// this query would 500 the /me/tool-return-value-stats endpoint for
// any quiet project (incl. the synthetic-customer most of the day).

package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// openMinimalToolReturnValueStatsStore creates a SQLite store with just
// the events + executions tables needed by GetToolReturnValueStats.
// Schema mirrors the relevant subset of migration 001.
func openMinimalToolReturnValueStatsStore(t *testing.T) *SQLiteStore {
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

	if _, err := db.Exec(`
		CREATE TABLE executions (
			execution_id TEXT PRIMARY KEY,
			project_id   TEXT NOT NULL
		);
		CREATE TABLE events (
			event_id     TEXT PRIMARY KEY,
			execution_id TEXT NOT NULL,
			event_type   TEXT NOT NULL,
			sequence     INTEGER NOT NULL DEFAULT 0,
			timestamp    TIMESTAMP NOT NULL,
			duration_ms  INTEGER,
			payload      TEXT
		);
	`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return &SQLiteStore{db: db}
}

// TestGetToolReturnValueStats_EmptyWindowReturnsZeroCounts reproduces
// the production 500 reported on /me/tool-return-value-stats for the
// synthetic-customer (and any quiet project). Before the COALESCE
// wraps, this test's Scan would fail with
// "converting NULL to int is unsupported". With COALESCE, the stats
// row carries clean zero counts.
func TestGetToolReturnValueStats_EmptyWindowReturnsZeroCounts(t *testing.T) {
	t.Parallel()
	st := openMinimalToolReturnValueStatsStore(t)

	stats, err := st.GetToolReturnValueStats(
		context.Background(), "proj-quiet", 24, 8192,
	)
	if err != nil {
		t.Fatalf("GetToolReturnValueStats: %v", err)
	}
	if stats.WindowHours != 24 {
		t.Errorf("WindowHours: want 24 got %d", stats.WindowHours)
	}
	if stats.TotalCalls != 0 {
		t.Errorf("TotalCalls: want 0 got %d", stats.TotalCalls)
	}
	if stats.TruncatedCount != 0 {
		t.Errorf("TruncatedCount: want 0 got %d", stats.TruncatedCount)
	}
	if stats.OversizedCount != 0 {
		t.Errorf("OversizedCount: want 0 got %d", stats.OversizedCount)
	}
}

// TestGetToolReturnValueStats_CountsTruncatedAndOversized seeds a
// handful of tool_call events with mixed payload shapes and verifies
// the aggregate counts come back correctly. Guards against a future
// refactor breaking the SUM-CASE arithmetic while keeping the empty
// case green by accident.
func TestGetToolReturnValueStats_CountsTruncatedAndOversized(t *testing.T) {
	t.Parallel()
	st := openMinimalToolReturnValueStatsStore(t)

	if _, err := st.db.Exec(`
		INSERT INTO executions (execution_id, project_id) VALUES
		  ('exec-1', 'proj-busy');
		INSERT INTO events (event_id, execution_id, event_type, timestamp, payload) VALUES
		  -- One truncated sentinel (counts in truncated, not oversized).
		  ('ev-1', 'exec-1', 'tool_call', datetime('now', '-1 hour'),
		   '{"return_value":"<truncated>","status":"ok"}'),
		  -- One oversized payload (return_value length > maxBytes=64).
		  ('ev-2', 'exec-1', 'tool_call', datetime('now', '-2 hour'),
		   '{"return_value":"' || hex(randomblob(64)) || '","status":"ok"}'),
		  -- One small ok payload (counts in total only).
		  ('ev-3', 'exec-1', 'tool_call', datetime('now', '-3 hour'),
		   '{"return_value":"ok","status":"ok"}'),
		  -- One failed call (excluded entirely by the WHERE clause).
		  ('ev-4', 'exec-1', 'tool_call', datetime('now', '-4 hour'),
		   '{"return_value":"err","status":"failed"}'),
		  -- One non-tool_call event (excluded entirely).
		  ('ev-5', 'exec-1', 'llm_call', datetime('now', '-5 hour'),
		   '{"return_value":"<truncated>"}'),
		  -- One tool_call outside the 24h window (excluded entirely).
		  ('ev-6', 'exec-1', 'tool_call', datetime('now', '-48 hour'),
		   '{"return_value":"<truncated>","status":"ok"}');
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	stats, err := st.GetToolReturnValueStats(
		context.Background(), "proj-busy", 24, 64,
	)
	if err != nil {
		t.Fatalf("GetToolReturnValueStats: %v", err)
	}
	// 3 tool_call events in window, status != failed.
	if stats.TotalCalls != 3 {
		t.Errorf("TotalCalls: want 3 got %d", stats.TotalCalls)
	}
	if stats.TruncatedCount != 1 {
		t.Errorf("TruncatedCount: want 1 got %d", stats.TruncatedCount)
	}
	if stats.OversizedCount != 1 {
		t.Errorf("OversizedCount: want 1 got %d", stats.OversizedCount)
	}
}
