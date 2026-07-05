package store

// Per-project provider_incident detector threshold store methods,
// Postgres implementation (migration 040). Sidecar matching the
// SQLite twin.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetProjectProviderIncidentMinTenants returns the per-project
// minimum-tenants threshold the provider_incident detector should
// use for projectID.
func (s *PostgresStore) GetProjectProviderIncidentMinTenants(
	ctx context.Context,
	projectID string,
) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT provider_incident_min_tenants FROM projects WHERE project_id = $1
	`, projectID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("get project provider_incident_min_tenants: %w", err)
	}
	return n, nil
}

// SetProjectProviderIncidentMinTenants writes a positive threshold.
func (s *PostgresStore) SetProjectProviderIncidentMinTenants(
	ctx context.Context,
	projectID string,
	minTenants int,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE projects SET provider_incident_min_tenants = $1 WHERE project_id = $2
	`, minTenants, projectID)
	if err != nil {
		return fmt.Errorf("set project provider_incident_min_tenants: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
