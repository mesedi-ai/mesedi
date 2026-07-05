package store

// Per-project data retention store methods, Postgres implementation
//. Postgres counterpart to sqlite_retention.go.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GetProjectRetentionDays returns nil for indefinite, *int for finite.
func (s *PostgresStore) GetProjectRetentionDays(
	ctx context.Context,
	projectID string,
) (*int, error) {
	var days sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT retention_days FROM projects WHERE project_id = $1
	`, projectID).Scan(&days)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get project retention_days: %w", err)
	}
	if !days.Valid {
		return nil, nil
	}
	d := int(days.Int64)
	return &d, nil
}

// SetProjectRetentionDays writes nil for indefinite or a positive int.
func (s *PostgresStore) SetProjectRetentionDays(
	ctx context.Context,
	projectID string,
	days *int,
) error {
	var arg sql.NullInt64
	if days != nil {
		arg = sql.NullInt64{Int64: int64(*days), Valid: true}
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE projects SET retention_days = $1 WHERE project_id = $2
	`, arg, projectID)
	if err != nil {
		return fmt.Errorf("set project retention_days: %w", err)
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

// ListProjectsForRetention returns projects with finite retention.
func (s *PostgresStore) ListProjectsForRetention(
	ctx context.Context,
) ([]*ProjectRetention, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT project_id, retention_days
		FROM projects
		WHERE retention_days IS NOT NULL
	`)
	if err != nil {
		return nil, fmt.Errorf("list projects for retention: %w", err)
	}
	defer rows.Close()

	out := make([]*ProjectRetention, 0, 8)
	for rows.Next() {
		pr := &ProjectRetention{}
		if err := rows.Scan(&pr.ProjectID, &pr.RetentionDays); err != nil {
			return nil, fmt.Errorf("scan project retention: %w", err)
		}
		out = append(out, pr)
	}
	return out, rows.Err()
}

// DeleteExecutionsOlderThan removes executions older than cutoff.
// FK CASCADE handles events, failure_groups, webhook_deliveries.
func (s *PostgresStore) DeleteExecutionsOlderThan(
	ctx context.Context,
	projectID string,
	cutoff time.Time,
) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM executions
		WHERE project_id = $1 AND started_at < $2
	`, projectID, cutoff.UTC())
	if err != nil {
		return 0, fmt.Errorf("delete executions older than: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}
