package store

// Per-project failure-class severity override store methods, SQLite
// implementation.
//
// Tiny surface: each project gets at most one row per failure_class
// (PRIMARY KEY constraint). Absence of a row means "use the
// hardcoded default from internal/severity/defaults.go".

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GetProjectClassSeverity returns the override for (projectID,
// failureClass). ErrNotFound when no override exists, in which case
// the caller should fall back to severity.Default(failureClass).
func (s *SQLiteStore) GetProjectClassSeverity(
	ctx context.Context,
	projectID, failureClass string,
) (*ProjectClassSeverity, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT project_id, failure_class, severity, updated_at
		FROM project_class_severities
		WHERE project_id = ? AND failure_class = ?
	`, projectID, failureClass)
	return scanProjectClassSeveritySQLite(row)
}

// UpsertProjectClassSeverity inserts on first save, updates on
// subsequent saves. updated_at is bumped to now() unconditionally.
func (s *SQLiteStore) UpsertProjectClassSeverity(
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
		) VALUES (?, ?, ?, ?)
		ON CONFLICT(project_id, failure_class) DO UPDATE SET
			severity   = excluded.severity,
			updated_at = excluded.updated_at
	`, o.ProjectID, o.FailureClass, o.Severity, now)
	if err != nil {
		return fmt.Errorf("upsert project_class_severity: %w", err)
	}
	return nil
}

// DeleteProjectClassSeverity removes the override row. Returns nil
// even when no row existed (idempotent delete).
func (s *SQLiteStore) DeleteProjectClassSeverity(
	ctx context.Context,
	projectID, failureClass string,
) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM project_class_severities
		WHERE project_id = ? AND failure_class = ?
	`, projectID, failureClass)
	if err != nil {
		return fmt.Errorf("delete project_class_severity: %w", err)
	}
	return nil
}

// ListProjectClassSeverityOverrides returns every override for the
// project, sorted by failure_class ASC so the UI renders in a stable
// order across loads.
func (s *SQLiteStore) ListProjectClassSeverityOverrides(
	ctx context.Context,
	projectID string,
) ([]*ProjectClassSeverity, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT project_id, failure_class, severity, updated_at
		FROM project_class_severities
		WHERE project_id = ?
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

// scanProjectClassSeveritySQLite is the shared sql.Row scanner.
// Returns ErrNotFound on sql.ErrNoRows.
func scanProjectClassSeveritySQLite(row *sql.Row) (*ProjectClassSeverity, error) {
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
