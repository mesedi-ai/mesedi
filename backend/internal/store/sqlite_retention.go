package store

// Per-project data retention store methods, SQLite implementation
//. Sidecar file: keeps the existing projects-table scans in
// sqlite.go unchanged. Reads/writes only the retention_days column.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GetProjectRetentionDays returns the retention window for projectID.
// nil = indefinite (no pruning); positive int = number of days.
// Returns ErrNotFound when the project does not exist (different from
// "exists but column is NULL", which returns (nil, nil)).
func (s *SQLiteStore) GetProjectRetentionDays(
	ctx context.Context,
	projectID string,
) (*int, error) {
	var days sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT retention_days FROM projects WHERE project_id = ?
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

// SetProjectRetentionDays writes nil for indefinite or a positive
// int for a finite window. Caller has already validated value > 0.
func (s *SQLiteStore) SetProjectRetentionDays(
	ctx context.Context,
	projectID string,
	days *int,
) error {
	var arg any
	if days != nil {
		arg = *days
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE projects SET retention_days = ? WHERE project_id = ?
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

// ListProjectsForRetention returns one row per project where
// retention_days IS NOT NULL. The scheduler iterates this list each
// nightly tick.
func (s *SQLiteStore) ListProjectsForRetention(
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

// DeleteExecutionsOlderThan removes executions for projectID where
// started_at < cutoff.
//
// The cascade reaches events and execution_failure_groups only. This
// comment previously also claimed failure_groups and
// webhook_deliveries; both are declared FOREIGN KEY (project_id)
// REFERENCES projects, so neither was ever reachable from an
// executions delete. See migrations/002_failure_groups.sql and
// 004_webhook_deliveries.sql. Those tables are pruned by their own
// methods below.
//
// Returns the count of executions deleted (useful for logging
// prune volume).
func (s *SQLiteStore) DeleteExecutionsOlderThan(
	ctx context.Context,
	projectID string,
	cutoff time.Time,
) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM executions
		WHERE project_id = ? AND started_at < ?
	`, projectID, cutoff.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("delete executions older than: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

// DeleteFailureGroupsOlderThan is the SQLite twin. Written in the same
// change as the Postgres one on purpose: these two files carry
// separate hand-written SQL, and a retention fix applied to one engine
// and not the other would leave self-hosted deployments quietly
// hoarding data the docs promise to delete.
//
// See the interface doc in store.go for why last_seen rather than
// first_seen, and why the comparison is textual.
func (s *SQLiteStore) DeleteFailureGroupsOlderThan(
	ctx context.Context,
	projectID string,
	cutoff time.Time,
) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM failure_groups
		WHERE project_id = ? AND last_seen < ?
	`, projectID, cutoff.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("delete failure_groups older than: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

// DeleteWebhookDeliveriesOlderThan is the SQLite twin.
//
// NOTE for self-hosters: SQLite enforces ON DELETE CASCADE only when
// PRAGMA foreign_keys is ON. The cascade from failure_groups to
// ai_analyses therefore depends on that pragma being set on the
// connection, which OpenSQLite does. Flagged here because a cascade
// that silently does not fire is exactly the failure this change
// exists to correct.
func (s *SQLiteStore) DeleteWebhookDeliveriesOlderThan(
	ctx context.Context,
	projectID string,
	cutoff time.Time,
) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM webhook_deliveries
		WHERE project_id = ? AND created_at < ?
	`, projectID, cutoff.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("delete webhook_deliveries older than: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}
