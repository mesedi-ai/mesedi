package store

// Per-project time_budget detector threshold store methods,
// Postgres implementation (migration 041). Sidecar matching the
// SQLite twin.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetProjectTimeBudgetMs returns the per-project time_budget
// detector threshold in milliseconds for projectID.
func (s *PostgresStore) GetProjectTimeBudgetMs(
	ctx context.Context,
	projectID string,
) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT time_budget_ms FROM projects WHERE project_id = $1
	`, projectID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("get project time_budget_ms: %w", err)
	}
	return n, nil
}

// SetProjectTimeBudgetMs writes a positive threshold (ms).
func (s *PostgresStore) SetProjectTimeBudgetMs(
	ctx context.Context,
	projectID string,
	thresholdMs int,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE projects SET time_budget_ms = $1 WHERE project_id = $2
	`, thresholdMs, projectID)
	if err != nil {
		return fmt.Errorf("set project time_budget_ms: %w", err)
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
