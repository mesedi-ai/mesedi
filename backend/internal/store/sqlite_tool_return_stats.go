package store

// Tool-return-value telemetry queries. Surfaces what
// fraction of recent tool_call events are being clipped, either by
// the SDK shipping a "<truncated>" sentinel, or by exceeding the
// per-project byte cap and getting excluded from the
// tool_schema_drift fingerprint comparison.
//
// The customer-facing answer the dashboard tile asks is "is my cap
// too tight?", a high rate means raising tool_return_value_max_bytes
// would recover more drift signal.

import (
	"context"
	"fmt"
)

// ToolReturnValueStats aggregates clipping signal across a recent
// window of tool_call events for one project.
type ToolReturnValueStats struct {
	WindowHours    int
	TotalCalls     int
	TruncatedCount int // SDK shipped the literal "<truncated>" sentinel
	OversizedCount int // return_value JSON exceeded the backend cap
}

// GetToolReturnValueStats counts tool_call events in the last
// windowHours that were clipped by either the SDK (literal
// "<truncated>" string in return_value) or by the backend cap
// (return_value JSON length > maxBytes). Returns zero counts when
// no tool_call events exist in the window.
func (s *SQLiteStore) GetToolReturnValueStats(
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
	// One query, three filtered aggregates. Excludes failed
	// tool_call events because schema_drift only operates on
	// successful returns.
	//
	// COALESCE wraps every SUM(CASE...) because SQLite returns NULL
	// when SUM runs over zero matching rows (zero tool_call events in
	// the window, the synthetic-customer's normal state most of the
	// day). Without COALESCE, Scan into a non-pointer int errors with
	// "converting NULL to int is unsupported" and the handler 500s.
	// COUNT(*) is unaffected (it returns 0, not NULL).
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) AS total,
			COALESCE(SUM(CASE
				WHEN json_extract(ev.payload, '$.return_value') = '<truncated>'
				THEN 1 ELSE 0
			END), 0) AS truncated,
			COALESCE(SUM(CASE
				WHEN length(json_extract(ev.payload, '$.return_value')) > ?
				THEN 1 ELSE 0
			END), 0) AS oversized
		FROM events ev
		JOIN executions ex ON ex.execution_id = ev.execution_id
		WHERE ex.project_id = ?
		  AND ev.event_type = 'tool_call'
		  AND COALESCE(json_extract(ev.payload, '$.status'), 'ok') != 'failed'
		  AND ev.timestamp >= datetime('now', ?)
	`,
		maxBytes,
		projectID,
		fmt.Sprintf("-%d hours", windowHours),
	).Scan(&stats.TotalCalls, &stats.TruncatedCount, &stats.OversizedCount)
	if err != nil {
		return stats, fmt.Errorf("get tool_return_value stats: %w", err)
	}
	return stats, nil
}
