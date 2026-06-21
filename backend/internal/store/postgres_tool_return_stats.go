package store

// Tool-return-value telemetry queries (#270.c), Postgres twin.
// See sqlite_tool_return_stats.go for the rationale.

import (
	"context"
	"fmt"
	"time"
)

// GetToolReturnValueStats counts tool_call events in the last
// windowHours that were clipped by either the SDK or the backend
// cap. Postgres twin of the SQLite implementation.
func (s *PostgresStore) GetToolReturnValueStats(
	ctx context.Context,
	projectID string,
	windowHours int,
	maxBytes int,
) (ToolReturnValueStats, error) {
	if windowHours <= 0 {
		windowHours = 24
	}
	if maxBytes <= 0 {
		maxBytes = 8192
	}
	stats := ToolReturnValueStats{WindowHours: windowHours}
	cutoff := time.Now().UTC().Add(-time.Duration(windowHours) * time.Hour)
	// Postgres JSON access uses ->>'key' for text extraction.
	// COUNT(*) FILTER (WHERE ...) is more idiomatic than SUM(CASE)
	// in Postgres but the SUM(CASE) form is portable enough to
	// stay in lock-step with the SQLite twin and avoids subtle
	// differences in COUNT semantics.
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) AS total,
			SUM(CASE
				WHEN ev.payload->>'return_value' = '<truncated>'
				THEN 1 ELSE 0
			END) AS truncated,
			SUM(CASE
				WHEN octet_length(ev.payload->>'return_value') > $1
				THEN 1 ELSE 0
			END) AS oversized
		FROM events ev
		JOIN executions ex ON ex.execution_id = ev.execution_id
		WHERE ex.project_id = $2
		  AND ev.event_type = 'tool_call'
		  AND COALESCE(ev.payload->>'status', 'ok') != 'failed'
		  AND ev.timestamp >= $3
	`,
		maxBytes,
		projectID,
		cutoff,
	).Scan(&stats.TotalCalls, &stats.TruncatedCount, &stats.OversizedCount)
	if err != nil {
		return stats, fmt.Errorf("get tool_return_value stats: %w", err)
	}
	return stats, nil
}
