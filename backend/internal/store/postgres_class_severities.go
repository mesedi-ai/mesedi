package store

// Per-project failure-class severity override store methods, Postgres
// implementation (#261). Postgres counterpart to
// sqlite_class_severities.go.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GetProjectClassSeverity returns the override or ErrNotFound.
func (s *PostgresStore) GetProjectClassSeverity(
	ctx context.Context,
	projectID, failureClass string,
) (*ProjectClassSeverity, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT project_id, failure_class, severity, updated_at
		FROM project_class_severities
		WHERE project_id = $1 AND failure_class = $2
	`, projectID, failureClass)
	return scanProjectClassSeverityPg(row)
}

// UpsertProjectClassSeverity inserts or updates.
func (s *PostgresStore) UpsertProjectClassSeverity(
	ctx context.Context,
	o *ProjectClassSeverity,
) error {
	if o == nil || o.ProjectID == "" || o.FailureClass == "" || o.Severity == "" {
		return fmt.Errorf("project_id, failure_class, severity all required")
	}
	now := time.Now().UTC().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO project_class_severities (
			project_id, failure_class, severity, updated_at
		) VALUES ($1, $2, $3, $4)
		ON CONFLICT (project_id, failure_class) DO UPDATE SET
			severity   = EXCLUDED.severity,
			updated_at = EXCLUDED.updated_at
	`, o.ProjectID, o.FailureClass, o.Severity, now)
	if err != nil {
		return fmt.Errorf("upsert project_class_severity: %w", err)
	}
	return nil
}

// DeleteProjectClassSeverity removes the override; idempotent.
func (s *PostgresStore) DeleteProjectClassSeverity(
	ctx context.Context,
	projectID, failureClass string,
) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM project_class_severities
		WHERE project_id = $1 AND failure_class = $2
	`, projectID, failureClass)
	if err != nil {
		return fmt.Errorf("delete project_class_severity: %w", err)
	}
	return nil
}

// ListProjectClassSeverityOverrides returns every override for the
// project, sorted by failure_class ASC.
func (s *PostgresStore) ListProjectClassSeverityOverrides(
	ctx context.Context,
	projectID string,
) ([]*ProjectClassSeverity, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT project_id, failure_class, severity, updated_at
		FROM project_class_severities
		WHERE project_id = $1
		ORDER BY failure_class ASC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list project_class_severities: %w", err)
	}
	defer rows.Close()

	out := make([]*ProjectClassSeverity, 0, 8)
	for rows.Next() {
		o := &ProjectClassSeverity{}
		var updatedAt int64
		if err := rows.Scan(&o.ProjectID, &o.FailureClass, &o.Severity, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan project_class_severity: %w", err)
		}
		o.UpdatedAt = time.Unix(updatedAt, 0).UTC()
		out = append(out, o)
	}
	return out, rows.Err()
}

func scanProjectClassSeverityPg(row *sql.Row) (*ProjectClassSeverity, error) {
	o := &ProjectClassSeverity{}
	var updatedAt int64
	err := row.Scan(&o.ProjectID, &o.FailureClass, &o.Severity, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan project_class_severity: %w", err)
	}
	o.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return o, nil
}
