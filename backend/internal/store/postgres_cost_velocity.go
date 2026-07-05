package store

// Per-project cost_velocity detector threshold store methods,
// Postgres implementation (migration 043). Sidecar matching the
// SQLite twin.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetProjectCostVelocityThresholdUSD returns the per-project
// cost_velocity detector threshold in USD for projectID.
func (s *PostgresStore) GetProjectCostVelocityThresholdUSD(
	ctx context.Context,
	projectID string,
) (float64, error) {
	var n float64
	err := s.db.QueryRowContext(ctx, `
		SELECT cost_velocity_threshold_usd FROM projects WHERE project_id = $1
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
func (s *PostgresStore) SetProjectCostVelocityThresholdUSD(
	ctx context.Context,
	projectID string,
	thresholdUSD float64,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE projects SET cost_velocity_threshold_usd = $1 WHERE project_id = $2
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
