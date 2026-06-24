package store

// Postgres twin of sqlite_detector_status.go. Same query semantics
// using the jsonb-cast operator path (payload::jsonb->>'<key>')
// matching the existing tool-history queries (ListSuccessfulToolReturns,
// ListToolNamesInExecution).

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CountCheckpointEventsForProject — Postgres twin.
func (s *PostgresStore) CountCheckpointEventsForProject(
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
		WHERE ex.project_id = $1
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

// ListToolCallCountsForProject — Postgres twin.
func (s *PostgresStore) ListToolCallCountsForProject(
	ctx context.Context,
	projectID string,
) ([]ToolCallCount, error) {
	if projectID == "" {
		return nil, fmt.Errorf("projectID required")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT (ev.payload::jsonb->>'tool_name') AS tool_name,
		       COUNT(*) AS call_count
		FROM events ev
		JOIN executions ex ON ex.execution_id = ev.execution_id
		WHERE ex.project_id = $1
		  AND ev.event_type = 'tool_call'
		  AND COALESCE(ev.payload::jsonb->>'status', 'ok') != 'failed'
		  AND (ev.payload::jsonb->>'tool_name') IS NOT NULL
		GROUP BY (ev.payload::jsonb->>'tool_name')
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
