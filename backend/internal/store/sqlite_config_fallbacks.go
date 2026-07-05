package store

// Config-fallback telemetry queries. Since 
// (migration 050) these rows live in the dedicated system_events
// table instead of audit_events — they're operational telemetry,
// not customer-initiated admin actions.
//
// Surfaces in the dashboard so customers see when their config is
// being silently ignored — e.g. a bad migration that drops a column
// would otherwise be invisible to them.

import (
	"context"
	"fmt"
)

// ConfigFallbackStats aggregates fallback events for one project
// over a recent window, grouped by which config column was read.
// Adding a new tracked config: instrument the fallback site with
// recordAuditEventForProject(target_id="your_config_key"), add the
// field below, and extend the switch in the SQLite + Postgres
// implementations.
type ConfigFallbackStats struct {
	WindowHours                     int
	TimeBudgetCount                 int
	ProviderIncidentMinTenantsCount int
	ToolReturnValueMaxBytesCount    int
	ClassSeverityOverrideCount      int
	// DetectorThresholdsCount aggregates ALL detector-
	// threshold fallback events (any (detector, threshold_key)
	// pair). Per-detector breakdown is intentionally NOT exposed
	// at the dashboard tile level — the customer-facing question
	// is "is my config being silently eaten?" and a single
	// counter answers that. Per-detector telemetry lives in the
	// audit_events table for ops to query when needed.
	DetectorThresholdsCount int
}

// GetConfigFallbackStats counts system_events rows where
// action="config_fallback" and project_id matches, grouped by
// target_id (which records the config column name). Returns zero
// counts for projects with no fallback events.
func (s *SQLiteStore) GetConfigFallbackStats(
	ctx context.Context,
	projectID string,
	windowHours int,
) (ConfigFallbackStats, error) {
	if windowHours <= 0 {
		windowHours = 24
	}
	stats := ConfigFallbackStats{WindowHours: windowHours}
	rows, err := s.db.QueryContext(ctx, `
		SELECT target_id, COUNT(*) AS n
		FROM system_events
		WHERE project_id = ?
		  AND action = 'config_fallback'
		  AND target_type = 'project_config'
		  AND created_at >= datetime('now', ?)
		GROUP BY target_id
	`, projectID, fmt.Sprintf("-%d hours", windowHours))
	if err != nil {
		return stats, fmt.Errorf("get config_fallback stats: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var targetID string
		var n int
		if err := rows.Scan(&targetID, &n); err != nil {
			return stats, fmt.Errorf("scan config_fallback stats: %w", err)
		}
		switch {
		case targetID == "time_budget_ms":
			stats.TimeBudgetCount = n
		case targetID == "provider_incident_min_tenants":
			stats.ProviderIncidentMinTenantsCount = n
		case targetID == "tool_return_value_max_bytes":
			stats.ToolReturnValueMaxBytesCount = n
		case targetID == "class_severity_override":
			stats.ClassSeverityOverrideCount = n
		case len(targetID) > len("detector_threshold:") &&
			targetID[:len("detector_threshold:")] == "detector_threshold:":
			//  every detector_threshold:<detector>:<key>
			// fallback rolls up into the same aggregate counter.
			// Per-detector breakdown stays in audit_events for ops.
			stats.DetectorThresholdsCount += n
		}
		// Unknown target_id values are silently ignored — keeps
		// the query forward-compatible if a future config is
		// added without updating this aggregator.
	}
	return stats, rows.Err()
}
