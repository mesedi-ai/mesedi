package store

// Per-project cost_velocity RATE detector configuration store methods,
// SQLite implementation (migration 044). Sidecar matching the
// time_budget / cost_velocity (absolute) / tool_return_value layout.
// Reads/writes only the two cost_velocity_rate_* columns. Get returns
// a single struct so the handler does one round-trip per execution
// close instead of two.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CostVelocityRateConfig is the per-project rate-detector configuration
// pair. ThresholdUSDPerMin is the burn-rate at or above which the
// detector fires; WindowMinutes is the rolling lookback window the
// rate is computed over (SUM of execution costs in the window ÷
// window minutes).
type CostVelocityRateConfig struct {
	ThresholdUSDPerMin float64
	WindowMinutes      int
}

// DefaultCostVelocityRateConfig is the fallback returned to the
// handler when the per-project read fails. Matches the migration 044
// defaults so behavior is identical to a freshly-inserted project.
// $5/min over a 5-minute window, clearly anomalous for any real
// workload without flooding on routine bursts.
var DefaultCostVelocityRateConfig = CostVelocityRateConfig{
	ThresholdUSDPerMin: 5.00,
	WindowMinutes:      5,
}

// GetProjectCostVelocityRateConfig returns the per-project rate
// configuration. Default after migration 044 is {5.00, 5} matching
// DefaultCostVelocityRateConfig. Returns ErrNotFound when the
// project does not exist so HandleGetCostVelocityRateConfig can map
// to 404.
func (s *SQLiteStore) GetProjectCostVelocityRateConfig(
	ctx context.Context,
	projectID string,
) (CostVelocityRateConfig, error) {
	var cfg CostVelocityRateConfig
	err := s.db.QueryRowContext(ctx, `
		SELECT cost_velocity_rate_threshold_usd_per_min,
		       cost_velocity_rate_window_minutes
		FROM projects
		WHERE project_id = ?
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

// SetProjectCostVelocityRateConfig writes both rate-config fields in
// one statement. Caller (HandleSetCostVelocityRateConfig) has already
// validated bounds: threshold ∈ [0.10, 10000.00] $/min; window ∈
// [1, 60] minutes.
func (s *SQLiteStore) SetProjectCostVelocityRateConfig(
	ctx context.Context,
	projectID string,
	cfg CostVelocityRateConfig,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET cost_velocity_rate_threshold_usd_per_min = ?,
		    cost_velocity_rate_window_minutes        = ?
		WHERE project_id = ?
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
