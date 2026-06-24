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
	// Postgres JSON access uses ->>'key' for text extraction. The
	// events.payload column is intentionally TEXT (not JSONB) per
	// migrations-postgres/001_initial.sql line 8, so every extraction
	// must cast first: `ev.payload::jsonb->>'key'`. Same convention as
	// postgres_detector_status.go. Without the cast Postgres raises
	// "operator does not exist: text ->> unknown" (SQLSTATE 42883) and
	// the handler 500s — which is exactly what was happening in prod on
	// /me/tool-return-value-stats until this wave.
	//
	// COUNT(*) FILTER (WHERE ...) is more idiomatic than SUM(CASE)
	// in Postgres but the SUM(CASE) form is portable enough to
	// stay in lock-step with the SQLite twin and avoids subtle
	// differences in COUNT semantics.
	//
	// COALESCE wraps every SUM(CASE...) because Postgres returns NULL
	// when SUM runs over zero matching rows (zero tool_call events in
	// the window). Without COALESCE, Scan into a non-pointer int errors
	// with "converting NULL to int is unsupported" and the handler 500s.
	// COUNT(*) is unaffected (it returns 0, not NULL).
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) AS total,
			COALESCE(SUM(CASE
				WHEN ev.payload::jsonb->>'return_value' = '<truncated>'
				THEN 1 ELSE 0
			END), 0) AS truncated,
			COALESCE(SUM(CASE
				WHEN octet_length(ev.payload::jsonb->>'return_value') > $1
				THEN 1 ELSE 0
			END), 0) AS oversized
		FROM events ev
		JOIN executions ex ON ex.execution_id = ev.execution_id
		WHERE ex.project_id = $2
		  AND ev.event_type = 'tool_call'
		  AND COALESCE(ev.payload::jsonb->>'status', 'ok') != 'failed'
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
