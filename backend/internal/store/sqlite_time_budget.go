package store

// Per-project time_budget detector threshold store methods, SQLite
// implementation (migration 041). Sidecar file matching the
// provider_incident layout, keeps the main sqlite.go projects-table
// scans unchanged. Reads/writes only the time_budget_ms column.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetProjectTimeBudgetMs returns the per-project time_budget
// detector threshold in milliseconds for projectID. Default after
// migration 041 is 60000 (60s), matching the historical hardcoded
// constant. A short-running chat-agent project might set 30000;
// a research-agent project might set 300000.
func (s *SQLiteStore) GetProjectTimeBudgetMs(
	ctx context.Context,
	projectID string,
) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT time_budget_ms FROM projects WHERE project_id = ?
	`, projectID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("get project time_budget_ms: %w", err)
	}
	return n, nil
}

// SetProjectTimeBudgetMs writes a positive threshold (ms). Caller
// has already validated thresholdMs >= 1.
func (s *SQLiteStore) SetProjectTimeBudgetMs(
	ctx context.Context,
	projectID string,
	thresholdMs int,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE projects SET time_budget_ms = ? WHERE project_id = ?
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
