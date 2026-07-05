package store

// Per-project tool_return_value byte cap store methods, Postgres
// implementation (migration 042). Sidecar matching the SQLite twin.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetProjectToolReturnValueMaxBytes returns the per-project cap on
// tool_call return_value size (bytes) for projectID.
func (s *PostgresStore) GetProjectToolReturnValueMaxBytes(
	ctx context.Context,
	projectID string,
) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT tool_return_value_max_bytes FROM projects WHERE project_id = $1
	`, projectID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("get project tool_return_value_max_bytes: %w", err)
	}
	return n, nil
}

// SetProjectToolReturnValueMaxBytes writes a positive cap.
func (s *PostgresStore) SetProjectToolReturnValueMaxBytes(
	ctx context.Context,
	projectID string,
	maxBytes int,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE projects SET tool_return_value_max_bytes = $1 WHERE project_id = $2
	`, maxBytes, projectID)
	if err != nil {
		return fmt.Errorf("set project tool_return_value_max_bytes: %w", err)
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
