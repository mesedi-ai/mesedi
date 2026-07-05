package store

// Per-project provider_incident detector threshold store methods,
// SQLite implementation (migration 040). Sidecar file matching the
// retention helper layout — keeps the main sqlite.go projects-table
// scans unchanged. Reads/writes only the
// provider_incident_min_tenants column.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetProjectProviderIncidentMinTenants returns the per-project
// minimum-tenants threshold the provider_incident detector should
// use for projectID. Default after migration 040 is 2; single-
// tenant customers typically set it to 1 so any provider error
// fires the detector.
func (s *SQLiteStore) GetProjectProviderIncidentMinTenants(
	ctx context.Context,
	projectID string,
) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT provider_incident_min_tenants FROM projects WHERE project_id = ?
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
// Caller has already validated minTenants >= 1.
func (s *SQLiteStore) SetProjectProviderIncidentMinTenants(
	ctx context.Context,
	projectID string,
	minTenants int,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE projects SET provider_incident_min_tenants = ? WHERE project_id = ?
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
