package store

// Per-project cost_velocity detector threshold store methods, SQLite
// implementation (migration 043). Sidecar file matching the
// time_budget / provider_incident / tool_return_value layout — keeps
// the main sqlite.go projects-table scans unchanged. Reads/writes
// only the cost_velocity_threshold_usd column.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetProjectCostVelocityThresholdUSD returns the per-project
// cost_velocity detector threshold in USD for projectID. Default
// after migration 043 is 1.00 (one dollar per execution). Customers
// can tune lower (cost-sensitive workloads), higher (batch / tolerant
// of expensive single calls), or to the floor of $0.01 (every real
// call fires, the v0.0.1 demo behavior). Returns ErrNotFound when
// the project does not exist.
func (s *SQLiteStore) GetProjectCostVelocityThresholdUSD(
	ctx context.Context,
	projectID string,
) (float64, error) {
	var n float64
	err := s.db.QueryRowContext(ctx, `
		SELECT cost_velocity_threshold_usd FROM projects WHERE project_id = ?
	`, projectID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("get project cost_velocity_threshold_usd: %w", err)
	}
	return n, nil
}

// SetProjectCostVelocityThresholdUSD writes a positive threshold (USD).
// Caller (HandleSetCostVelocityConfig) has already validated bounds
// [0.01, 10000.00]. The store accepts what's passed.
func (s *SQLiteStore) SetProjectCostVelocityThresholdUSD(
	ctx context.Context,
	projectID string,
	thresholdUSD float64,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE projects SET cost_velocity_threshold_usd = ? WHERE project_id = ?
	`, thresholdUSD, projectID)
	if err != nil {
		return fmt.Errorf("set project cost_velocity_threshold_usd: %w", err)
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
