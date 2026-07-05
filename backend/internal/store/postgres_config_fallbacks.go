package store

// Config-fallback telemetry queries, Postgres twin. See
// sqlite_config_fallbacks.go for rationale.

import (
	"context"
	"fmt"
	"time"
)

// GetConfigFallbackStats counts system_events rows where
// action="config_fallback" and project_id matches, grouped by
// target_id. Postgres twin of the SQLite implementation.
// Reads from system_events after migration 050.
func (s *PostgresStore) GetConfigFallbackStats(
	ctx context.Context,
	projectID string,
	windowHours int,
) (ConfigFallbackStats, error) {
	if windowHours <= 0 {
		windowHours = 24
	}
	stats := ConfigFallbackStats{WindowHours: windowHours}
	cutoff := time.Now().UTC().Add(-time.Duration(windowHours) * time.Hour)
	rows, err := s.db.QueryContext(ctx, `
		SELECT target_id, COUNT(*) AS n
		FROM system_events
		WHERE project_id = $1
		  AND action = 'config_fallback'
		  AND target_type = 'project_config'
		  AND created_at >= $2
		GROUP BY target_id
	`, projectID, cutoff)
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
			//  detector-threshold fallbacks roll up.
			stats.DetectorThresholdsCount += n
		}
	}
	return stats, rows.Err()
}
