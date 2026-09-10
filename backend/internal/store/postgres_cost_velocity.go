package store

// Per-project cost_velocity detector threshold store methods,
// Postgres implementation (migration 043). Sidecar matching the
// SQLite twin.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
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

// GroupCostVelocity is the Postgres twin of the SQLite method in
// costvelocity.go (attributed signature). Caller is responsible
// for the per-project threshold check (see HandleUpdateExecution +
// GetProjectCostVelocityThresholdUSD); the store layer just writes
// the cluster.
func (s *PostgresStore) GroupCostVelocity(ctx context.Context, executionID, projectID string, costUSD float64, identity string) (isNew bool, err error) {
	signature := CostVelocityAttributedSignature(costUSD, identity)
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassCostVelocity, signature)
}

// GroupCostVelocityRate is the Postgres twin of the SQLite method in
// costvelocity.go. Companion to GroupCostVelocity using the
// rate-bucketed signature; deliberately NOT attributed, the rate is
// the project's aggregate burn.
func (s *PostgresStore) GroupCostVelocityRate(ctx context.Context, executionID, projectID string, ratePerMinUSD float64) (isNew bool, err error) {
	signature := CostVelocityRateSignature(ratePerMinUSD)
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassCostVelocity, signature)
}

// GroupCostVelocityBaseline is the Postgres twin of the SQLite method
// in costvelocity.go: the learned-normal form's grouping, signature
// bucketed by the multiple of the project's own baseline.
func (s *PostgresStore) GroupCostVelocityBaseline(ctx context.Context, executionID, projectID string, multiple float64) (isNew bool, err error) {
	signature := CostVelocityBaselineSignature(multiple)
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassCostVelocity, signature)
}

// EarliestExecutionStart is the Postgres twin of the SQLite method in
// costvelocity.go.
func (s *PostgresStore) EarliestExecutionStart(ctx context.Context, projectID string) (time.Time, error) {
	var t sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT MIN(started_at) FROM executions WHERE project_id = $1`,
		projectID,
	).Scan(&t)
	if err != nil {
		return time.Time{}, fmt.Errorf("earliest execution start: %w", err)
	}
	if !t.Valid {
		return time.Time{}, nil
	}
	return t.Time.UTC(), nil
}
