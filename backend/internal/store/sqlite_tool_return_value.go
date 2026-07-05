package store

// Per-project tool_return_value byte cap store methods, SQLite
// implementation (migration 042). Sidecar matching the layout used
// for retention / provider_incident / time_budget. Reads/writes only
// the tool_return_value_max_bytes column.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetProjectToolReturnValueMaxBytes returns the per-project cap on
// tool_call return_value size (bytes) that the tool_schema_drift
// detector will fingerprint. Default after migration 042 is 8192
// (8 KB) — matches the original SDK cap's "typical case" sizing
// without being as aggressive as the 2 KB hardcode.
func (s *SQLiteStore) GetProjectToolReturnValueMaxBytes(
	ctx context.Context,
	projectID string,
) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT tool_return_value_max_bytes FROM projects WHERE project_id = ?
	`, projectID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("get project tool_return_value_max_bytes: %w", err)
	}
	return n, nil
}

// SetProjectToolReturnValueMaxBytes writes a positive cap. Caller
// has already validated maxBytes >= 1.
func (s *SQLiteStore) SetProjectToolReturnValueMaxBytes(
	ctx context.Context,
	projectID string,
	maxBytes int,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE projects SET tool_return_value_max_bytes = ? WHERE project_id = ?
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
