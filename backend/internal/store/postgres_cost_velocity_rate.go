package store

// Per-project cost_velocity RATE detector configuration store methods,
// Postgres implementation (migration 044). Sidecar matching the
// SQLite twin.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetProjectCostVelocityRateConfig returns the per-project rate
// configuration.
func (s *PostgresStore) GetProjectCostVelocityRateConfig(
	ctx context.Context,
	projectID string,
) (CostVelocityRateConfig, error) {
	var cfg CostVelocityRateConfig
	err := s.db.QueryRowContext(ctx, `
		SELECT cost_velocity_rate_threshold_usd_per_min,
		       cost_velocity_rate_window_minutes
		FROM projects
		WHERE project_id = $1
	`, projectID).Scan(&cfg.ThresholdUSDPerMin, &cfg.WindowMinutes)
	if errors.Is(err, sql.ErrNoRows) {
		return CostVelocityRateConfig{}, ErrNotFound
	}
	if err != nil {
		return CostVelocityRateConfig{},
			fmt.Errorf("get project cost_velocity_rate config: %w", err)
	}
	return cfg, nil
}

// SetProjectCostVelocityRateConfig writes both rate-config fields.
func (s *PostgresStore) SetProjectCostVelocityRateConfig(
	ctx context.Context,
	projectID string,
	cfg CostVelocityRateConfig,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET cost_velocity_rate_threshold_usd_per_min = $1,
		    cost_velocity_rate_window_minutes        = $2
		WHERE project_id = $3
	`, cfg.ThresholdUSDPerMin, cfg.WindowMinutes, projectID)
	if err != nil {
		return fmt.Errorf("set project cost_velocity_rate config: %w", err)
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
