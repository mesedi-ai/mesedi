// Postgres integration tests for GetToolReturnValueStats.
//
// Regression coverage for the actual production 500 source:
// events.payload is TEXT (intentional per migrations-postgres/
// 001_initial.sql line 8), so the query must cast via
// ev.payload::jsonb->>'<key>' before extracting JSON keys. Without
// the cast Postgres raises SQLSTATE 42883 ("operator does not exist:
// text ->> unknown") and the handler 500s.
//
// Mirrors sqlite_tool_return_stats_test.go for twin coverage, but
// runs against a real Postgres container (via newTestPostgresStore)
// because SQLite is permissive about TEXT-as-JSON coercion, only
// Postgres catches the cast bug.

package store

import (
	"context"
	"testing"
)

// TestPostgres_GetToolReturnValueStats_EmptyWindowReturnsZeroCounts
// reproduces the prod 500 fingerprint: a project with zero tool_call
// events in the 24h window. Before the ::jsonb cast was added, this
// query never even reached the SUM stage, Postgres rejected the ->>
// operator at type-check. After the cast, the query returns clean
// zero counts.
func TestPostgres_GetToolReturnValueStats_EmptyWindowReturnsZeroCounts(t *testing.T) {
	st := newTestPostgresStore(t)
	if st == nil {
		return // Docker unavailable; skipped above.
	}

	// Seed only the project row (FK target for executions). No
	// executions, no events, this is the synthetic-customer's
	// normal state most of the day.
	if _, err := st.db.ExecContext(context.Background(), `
		INSERT INTO projects (project_id, name) VALUES ('proj-quiet', 'quiet');
	`); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	stats, err := st.GetToolReturnValueStats(
		context.Background(), "proj-quiet", 24, 8192,
	)
	if err != nil {
		t.Fatalf("GetToolReturnValueStats (empty window): %v", err)
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

// TestPostgres_GetToolReturnValueStats_CountsTruncatedAndOversized
// seeds 6 events with mixed payload shapes and verifies the
// aggregate counts come back correctly. This is the populated-window
// case, guards against a future refactor breaking the WHERE/SUM
// arithmetic while keeping the empty-window test green by accident.
func TestPostgres_GetToolReturnValueStats_CountsTruncatedAndOversized(t *testing.T) {
	st := newTestPostgresStore(t)
	if st == nil {
		return
	}

	// Use a maxBytes cap of 64 so the second event's repeat(80) payload
	// blows past the oversized threshold. Other events are crafted to
	// hit specific WHERE-clause branches:
	//   ev-1: <truncated> sentinel → counts in truncated, not oversized
	//   ev-2: long payload         → counts in oversized, not truncated
	//   ev-3: small ok payload     → counts in total only
	//   ev-4: status=failed         → excluded entirely
	//   ev-5: non-tool_call         → excluded entirely
	//   ev-6: outside 24h window   → excluded entirely
	if _, err := st.db.ExecContext(context.Background(), `
		INSERT INTO projects (project_id, name) VALUES ('proj-busy', 'busy');
		INSERT INTO executions (execution_id, project_id, status, started_at)
		VALUES ('exec-1', 'proj-busy', 'completed', NOW() - INTERVAL '1 hour');
		INSERT INTO events (event_id, execution_id, event_type, sequence, timestamp, payload) VALUES
		  ('ev-1', 'exec-1', 'tool_call', 1, NOW() - INTERVAL '1 hour',
		   '{"return_value":"<truncated>","status":"ok"}'),
		  ('ev-2', 'exec-1', 'tool_call', 2, NOW() - INTERVAL '2 hour',
		   '{"return_value":"' || repeat('x', 80) || '","status":"ok"}'),
		  ('ev-3', 'exec-1', 'tool_call', 3, NOW() - INTERVAL '3 hour',
		   '{"return_value":"ok","status":"ok"}'),
		  ('ev-4', 'exec-1', 'tool_call', 4, NOW() - INTERVAL '4 hour',
		   '{"return_value":"err","status":"failed"}'),
		  ('ev-5', 'exec-1', 'llm_call', 5, NOW() - INTERVAL '5 hour',
		   '{"return_value":"<truncated>"}'),
		  ('ev-6', 'exec-1', 'tool_call', 6, NOW() - INTERVAL '48 hour',
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
	// 3 tool_call events in window with status != failed (ev-1, ev-2, ev-3).
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
