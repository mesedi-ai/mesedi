package store

// SQLite implementation of the detector-status surface (empty-states
// wave). Reads are project-scoped via the executions table because
// the events table only carries execution_id; the JOIN pattern
// matches ListSuccessfulToolReturns + ListProjectModelsSince.
//
// Both methods are intentionally read-only observability queries —
// they run on dashboard-overview page load (~once per visit per
// project), not on the per-execution hot path. The 2 indexed
// SELECTs each touch the (event_type, timestamp) index on events
// joined to (project_id) on executions.

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ToolCallCount carries the count of non-failed tool_call events per
// tool_name for one project. Used by the detector-status surface to
// render tool_schema_drift priming progress per tool. The dashboard
// compares Count against MinHistoryCalls (the per-project threshold
// from the primitive; 10 by default) to decide whether to
// render "priming N/10" or "drift detection active".
type ToolCallCount struct {
	ToolName  string `json:"tool_name"`
	CallCount int    `json:"call_count"`
}

// CountCheckpointEventsForProject — SQLite implementation. Joins
// events to executions (events itself has no project_id column) and
// filters event_type='checkpoint'. Returns (0, nil, nil) when the
// project has never emitted a checkpoint event — the dashboard
// detector-status surface uses count=0 to render the semantic_loop
// "no checkpoint data yet — instrument mesedi.checkpoint() to
// enable" empty state.
func (s *SQLiteStore) CountCheckpointEventsForProject(
	ctx context.Context,
	projectID string,
) (count int, lastAt *time.Time, err error) {
	if projectID == "" {
		return 0, nil, fmt.Errorf("projectID required")
	}
	var ts sql.NullTime
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) AS cnt, MAX(ev.timestamp) AS last_at
		FROM events ev
		JOIN executions ex ON ex.execution_id = ev.execution_id
		WHERE ex.project_id = ?
		  AND ev.event_type = 'checkpoint'
	`, projectID)
	if err = row.Scan(&count, &ts); err != nil {
		return 0, nil, fmt.Errorf("count checkpoint events: %w", err)
	}
	if ts.Valid {
		t := ts.Time
		lastAt = &t
	}
	return count, lastAt, nil
}

// CountLLMCallsByProviderSince — SQLite implementation. Returns a
// map of provider → count of llm_call events for the project over
// the last `since` window. Used by the detector-status surface
// () to determine whether a project is Ollama-only and
// should show skip-reason chips on the 3 N/A detectors
// (provider_incident, infrastructure_throttled, cost_velocity).
// Empty map → no llm_call activity in the window.
func (s *SQLiteStore) CountLLMCallsByProviderSince(
	ctx context.Context,
	projectID string,
	since time.Time,
) (map[string]int, error) {
	if projectID == "" {
		return nil, fmt.Errorf("projectID required")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(json_extract(ev.payload, '$.provider'), '') AS provider,
		       COUNT(*) AS cnt
		FROM events ev
		JOIN executions ex ON ex.execution_id = ev.execution_id
		WHERE ex.project_id = ?
		  AND ev.event_type = 'llm_call'
		  AND ev.timestamp >= ?
		GROUP BY provider
	`, projectID, since.UTC().Format("2006-01-02T15:04:05Z"))
	if err != nil {
		return nil, fmt.Errorf("count llm_calls by provider: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var provider string
		var cnt int
		if err := rows.Scan(&provider, &cnt); err != nil {
			return nil, fmt.Errorf("scan provider count: %w", err)
		}
		if provider == "" {
			continue
		}
		out[provider] = cnt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate provider counts: %w", err)
	}
	return out, nil
}

// ListToolCallCountsForProject — SQLite implementation. Returns the
// per-tool count of non-failed tool_call events across all
// executions for the project. Ordered by call_count DESC, tool_name
// ASC so the dashboard shows busiest-first; ties broken
// deterministically. Skips tool_calls with status='failed' so the
// priming counter reflects only history that schema-drift detection
// would actually evaluate against.
func (s *SQLiteStore) ListToolCallCountsForProject(
	ctx context.Context,
	projectID string,
) ([]ToolCallCount, error) {
	if projectID == "" {
		return nil, fmt.Errorf("projectID required")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT json_extract(ev.payload, '$.tool_name') AS tool_name,
		       COUNT(*) AS call_count
		FROM events ev
		JOIN executions ex ON ex.execution_id = ev.execution_id
		WHERE ex.project_id = ?
		  AND ev.event_type = 'tool_call'
		  AND COALESCE(json_extract(ev.payload, '$.status'), 'ok') != 'failed'
		  AND json_extract(ev.payload, '$.tool_name') IS NOT NULL
		GROUP BY json_extract(ev.payload, '$.tool_name')
		ORDER BY call_count DESC, tool_name ASC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list tool call counts: %w", err)
	}
	defer rows.Close()
	out := []ToolCallCount{}
	for rows.Next() {
		var tc ToolCallCount
		var name sql.NullString
		if err := rows.Scan(&name, &tc.CallCount); err != nil {
			return nil, fmt.Errorf("scan tool call count: %w", err)
		}
		if !name.Valid || name.String == "" {
			continue
		}
		tc.ToolName = name.String
		out = append(out, tc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tool call counts: %w", err)
	}
	return out, nil
}
