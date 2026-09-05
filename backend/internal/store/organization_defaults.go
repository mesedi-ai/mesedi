package store

// Org-level defaults for per-project threshold sidecars (,
// migration 051). One row per (org_id, default_key); orgs that
// never set defaults have ZERO rows. The handler's resolver reads
// the project-level value first, then the org default, then a
// hardcoded constant.
//
// Known default_key values (validated at the API layer; the store
// accepts any string):
//   - "time_budget_ms"                    int64
//   - "provider_incident_min_tenants"     int
//   - "tool_return_value_max_bytes"       int

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// OrgDefault is one (org_id, default_key) row.
type OrgDefault struct {
	OrgID      string
	DefaultKey string
	ValueJSON  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// OrgConfigFallbackRollupTarget is one entry of the top-targets
// list returned alongside the aggregate count. Identifies which
// config key the customer's projects fell back on most.
type OrgConfigFallbackRollupTarget struct {
	TargetID string
	Count    int
}

// OrgConfigFallbackRollup aggregates config-fallback events across
// every project owned by an org., closes the 50-project
// enterprise UX where the customer would otherwise have to click
// into 50 tiles to spot operational degradation.
type OrgConfigFallbackRollup struct {
	WindowHours          int
	AffectedProjectCount int
	TotalEvents          int
	TopTargets           []OrgConfigFallbackRollupTarget
}

// --- SQLite impl --------------------------------------------------

// GetOrgDefaults returns every (default_key → value_json) override
// for the org. Empty map (not nil) when the org has no defaults set.
func (s *SQLiteStore) GetOrgDefaults(
	ctx context.Context, orgID string,
) (map[string]string, error) {
	out := map[string]string{}
	if orgID == "" {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT default_key, value_json
		FROM organization_defaults
		WHERE org_id = ?
	`, orgID)
	if err != nil {
		return out, fmt.Errorf("get org defaults: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return out, fmt.Errorf("scan org default: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetOrgDefault upserts the value_json for (org_id, default_key).
func (s *SQLiteStore) SetOrgDefault(
	ctx context.Context, orgID, defaultKey, valueJSON string,
) error {
	if orgID == "" || defaultKey == "" {
		return errors.New("SetOrgDefault: org_id and default_key required")
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO organization_defaults (
			org_id, default_key, value_json, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(org_id, default_key) DO UPDATE SET
			value_json = excluded.value_json,
			updated_at = excluded.updated_at
	`, orgID, defaultKey, valueJSON, now, now)
	if err != nil {
		return fmt.Errorf("set org default: %w", err)
	}
	return nil
}

// GetOrgConfigFallbackRollup aggregates system_events
// action="config_fallback" rows across every project owned by
// orgID over the recent window. Returns zero counts + empty
// top_targets when the org has no projects OR no fallbacks.
func (s *SQLiteStore) GetOrgConfigFallbackRollup(
	ctx context.Context, orgID string, windowHours int,
) (OrgConfigFallbackRollup, error) {
	if windowHours <= 0 {
		windowHours = 24
	}
	out := OrgConfigFallbackRollup{
		WindowHours: windowHours,
		TopTargets:  []OrgConfigFallbackRollupTarget{},
	}
	if orgID == "" {
		return out, nil
	}

	// Single aggregate query: join system_events ↔ projects on
	// project_id, restrict to org_id + action="config_fallback" +
	// window. Returns (target_id, COUNT(*)) per target_id +
	// COUNT(DISTINCT project_id).
	cutoff := fmt.Sprintf("-%d hours", windowHours)
	totals := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(DISTINCT system_events.project_id) AS affected_projects,
			COUNT(*) AS total_events
		FROM system_events
		JOIN projects ON projects.project_id = system_events.project_id
		WHERE projects.org_id = ?
		  AND system_events.action = 'config_fallback'
		  AND system_events.target_type = 'project_config'
		  AND system_events.created_at >= datetime('now', ?)
	`, orgID, cutoff)
	if err := totals.Scan(&out.AffectedProjectCount, &out.TotalEvents); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return out, fmt.Errorf("get org rollup totals: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT system_events.target_id, COUNT(*) AS n
		FROM system_events
		JOIN projects ON projects.project_id = system_events.project_id
		WHERE projects.org_id = ?
		  AND system_events.action = 'config_fallback'
		  AND system_events.target_type = 'project_config'
		  AND system_events.created_at >= datetime('now', ?)
		GROUP BY system_events.target_id
		ORDER BY n DESC
		LIMIT 5
	`, orgID, cutoff)
	if err != nil {
		return out, fmt.Errorf("get org rollup top_targets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t OrgConfigFallbackRollupTarget
		if err := rows.Scan(&t.TargetID, &t.Count); err != nil {
			return out, fmt.Errorf("scan org rollup top_target: %w", err)
		}
		out.TopTargets = append(out.TopTargets, t)
	}
	return out, rows.Err()
}

// --- Postgres impl ------------------------------------------------

// GetOrgDefaults is the Postgres twin.
func (s *PostgresStore) GetOrgDefaults(
	ctx context.Context, orgID string,
) (map[string]string, error) {
	out := map[string]string{}
	if orgID == "" {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT default_key, value_json
		FROM organization_defaults
		WHERE org_id = $1
	`, orgID)
	if err != nil {
		return out, fmt.Errorf("get org defaults (postgres): %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return out, fmt.Errorf("scan org default (postgres): %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetOrgDefault is the Postgres twin.
func (s *PostgresStore) SetOrgDefault(
	ctx context.Context, orgID, defaultKey, valueJSON string,
) error {
	if orgID == "" || defaultKey == "" {
		return errors.New("SetOrgDefault: org_id and default_key required")
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO organization_defaults (
			org_id, default_key, value_json, created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT(org_id, default_key) DO UPDATE SET
			value_json = EXCLUDED.value_json,
			updated_at = EXCLUDED.updated_at
	`, orgID, defaultKey, valueJSON, now, now)
	if err != nil {
		return fmt.Errorf("set org default (postgres): %w", err)
	}
	return nil
}

// GetOrgConfigFallbackRollup is the Postgres twin.
func (s *PostgresStore) GetOrgConfigFallbackRollup(
	ctx context.Context, orgID string, windowHours int,
) (OrgConfigFallbackRollup, error) {
	if windowHours <= 0 {
		windowHours = 24
	}
	out := OrgConfigFallbackRollup{
		WindowHours: windowHours,
		TopTargets:  []OrgConfigFallbackRollupTarget{},
	}
	if orgID == "" {
		return out, nil
	}
	cutoff := time.Now().UTC().Add(-time.Duration(windowHours) * time.Hour)

	totals := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(DISTINCT system_events.project_id) AS affected_projects,
			COUNT(*) AS total_events
		FROM system_events
		JOIN projects ON projects.project_id = system_events.project_id
		WHERE projects.org_id = $1
		  AND system_events.action = 'config_fallback'
		  AND system_events.target_type = 'project_config'
		  AND system_events.created_at >= $2
	`, orgID, cutoff)
	if err := totals.Scan(&out.AffectedProjectCount, &out.TotalEvents); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return out, fmt.Errorf("get org rollup totals (postgres): %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT system_events.target_id, COUNT(*) AS n
		FROM system_events
		JOIN projects ON projects.project_id = system_events.project_id
		WHERE projects.org_id = $1
		  AND system_events.action = 'config_fallback'
		  AND system_events.target_type = 'project_config'
		  AND system_events.created_at >= $2
		GROUP BY system_events.target_id
		ORDER BY n DESC
		LIMIT 5
	`, orgID, cutoff)
	if err != nil {
		return out, fmt.Errorf("get org rollup top_targets (postgres): %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t OrgConfigFallbackRollupTarget
		if err := rows.Scan(&t.TargetID, &t.Count); err != nil {
			return out, fmt.Errorf("scan org rollup top_target (postgres): %w", err)
		}
		out.TopTargets = append(out.TopTargets, t)
	}
	return out, rows.Err()
}
