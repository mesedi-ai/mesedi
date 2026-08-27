// Postgres twin of sqlite_tool_descriptions_test.go.
//
// WHY BOTH EXIST
// PostgresStore and SQLiteStore carry separate hand-written SQL for
// every query. A change applied to one and not the other passes the
// whole suite if only one is tested. That is not hypothetical: it is
// the shape of the migration-056 outage on 2026-08-24.
//
// SQLite alone is also not enough for THIS query specifically.
// events.payload is TEXT in the Postgres schema (deliberately, see
// migrations-postgres/001_initial.sql), so extracting a JSON key
// requires an explicit ::jsonb cast. Omit it and Postgres raises
// SQLSTATE 42883 at type-check time. SQLite, which is permissive
// about coercing TEXT to JSON, would never surface that.
//
// Skips cleanly when Docker is unavailable.

package store

import (
	"context"
	"testing"
)

// TestPostgres_ListToolDescriptions_CastAndExclusion covers the two
// things only the real engine can prove: that the ::jsonb cast is
// present, and that execution exclusion works against Postgres'
// parameter binding rather than SQLite's.
//
// The scenario is the synthetic-customer one that exposed the gap on
// 2026-08-27: a clean baseline in prior executions, then a poisoned
// description in the current one. The detector must see only the
// baseline as history.
func TestPostgres_ListToolDescriptions_CastAndExclusion(t *testing.T) {
	st := newTestPostgresStore(t)
	if st == nil {
		return // Docker unavailable; skipped above.
	}
	ctx := context.Background()

	if _, err := st.db.ExecContext(ctx, `
		INSERT INTO projects (project_id, name) VALUES ('proj-1', 'p1'), ('proj-2', 'p2');
		INSERT INTO executions (execution_id, project_id, status, started_at) VALUES
		  ('exec-old',     'proj-1', 'completed', NOW() - INTERVAL '2 hour'),
		  ('exec-current', 'proj-1', 'completed', NOW() - INTERVAL '1 minute'),
		  ('exec-other',   'proj-2', 'completed', NOW() - INTERVAL '1 hour');
		INSERT INTO events (event_id, execution_id, event_type, sequence, timestamp, payload) VALUES
		  ('ev-1', 'exec-old', 'tool_call', 1, NOW() - INTERVAL '2 hour',
		   '{"tool_name":"lookup","tool_description":"clean"}'),
		  ('ev-2', 'exec-current', 'tool_call', 2, NOW() - INTERVAL '1 minute',
		   '{"tool_name":"lookup","tool_description":"poisoned"}'),
		  ('ev-3', 'exec-other', 'tool_call', 3, NOW() - INTERVAL '1 hour',
		   '{"tool_name":"lookup","tool_description":"other tenant"}'),
		  ('ev-4', 'exec-old', 'tool_call', 4, NOW() - INTERVAL '2 hour',
		   '{"tool_name":"different_tool","tool_description":"wrong tool"}');
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// History: prior executions only, and only this tenant's rows.
	got, err := st.ListToolDescriptions(ctx, "proj-1", "lookup", "exec-current", 100)
	if err != nil {
		t.Fatalf("ListToolDescriptions (history): %v", err)
	}
	if len(got) != 1 || got[0] != "clean" {
		t.Fatalf("history must exclude the calling execution and other "+
			"tenants, got %v", got)
	}

	// Current: newest project-wide, which is the execution being
	// evaluated. This is how the caller reads the description under
	// test, so both directions have to work.
	got, err = st.ListToolDescriptions(ctx, "proj-1", "lookup", "", 1)
	if err != nil {
		t.Fatalf("ListToolDescriptions (current): %v", err)
	}
	if len(got) != 1 || got[0] != "poisoned" {
		t.Fatalf("newest-first ordering broken, got %v", got)
	}
}

// TestPostgres_ListToolDescriptions_SkipsMissingAndEmpty is the
// rollout-safety assertion, run against the engine that actually
// serves production.
//
// Customers on an SDK predating tool_description send tool_call
// events with no description. If those returned "", the empty string
// would form a majority baseline and the first call from an upgraded
// client would read as drift away from it. Shipping the detector
// would then page every customer who had not upgraded, on the day
// they did.
func TestPostgres_ListToolDescriptions_SkipsMissingAndEmpty(t *testing.T) {
	st := newTestPostgresStore(t)
	if st == nil {
		return
	}
	ctx := context.Background()

	if _, err := st.db.ExecContext(ctx, `
		INSERT INTO projects (project_id, name) VALUES ('proj-mixed', 'mixed');
		INSERT INTO executions (execution_id, project_id, status, started_at)
		VALUES ('exec-a', 'proj-mixed', 'completed', NOW() - INTERVAL '1 hour');
		INSERT INTO events (event_id, execution_id, event_type, sequence, timestamp, payload) VALUES
		  ('ev-a', 'exec-a', 'tool_call', 1, NOW() - INTERVAL '5 minute',
		   '{"tool_name":"lookup"}'),
		  ('ev-b', 'exec-a', 'tool_call', 2, NOW() - INTERVAL '4 minute',
		   '{"tool_name":"lookup","tool_description":""}'),
		  ('ev-c', 'exec-a', 'tool_call', 3, NOW() - INTERVAL '3 minute',
		   '{"tool_name":"lookup","tool_description":null}'),
		  ('ev-d', 'exec-a', 'tool_call', 4, NOW() - INTERVAL '2 minute',
		   '{"tool_name":"lookup","tool_description":"real","status":"failed"}');
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := st.ListToolDescriptions(ctx, "proj-mixed", "lookup", "", 100)
	if err != nil {
		t.Fatalf("ListToolDescriptions: %v", err)
	}
	// ev-d is included despite status=failed: a poisoned description
	// is worth seeing even when the call it rode in on threw.
	if len(got) != 1 || got[0] != "real" {
		t.Fatalf("want only the one real description (including the "+
			"failed call), got %v", got)
	}
}
