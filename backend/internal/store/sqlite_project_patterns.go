package store

// Per-project custom pattern storage for the three security
// detectors (prompt_injection, data_leakage, sandbox_escape).
// SQLite implementation (migration 045). Matches the
// postgres_project_patterns.go twin field-for-field.
//
// Schema lives in migrations/045_project_patterns.sql. One row per
// pattern, indexed by (project_id, detector) for the per-event
// hot-path read.
//
// The caller (API handler) is responsible for:
//   - RE2-validating pattern strings BEFORE Create / Update.
//     Stored patterns are trusted to compile.
//   - Enforcing PROJECT_PATTERN_MAX per (project_id, detector).
//   - Validating detector ∈ allow-list ('prompt_injection',
//     'data_leakage', 'sandbox_escape'). The store layer accepts
//     any string and would happily store an unknown detector;
//     keeping the allow-list at the API edge means a future
//     fourth pattern-based detector drops in with no store change.
//   - Validating severity ∈ {'low', 'medium', 'high'}.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ProjectPattern is one row of the project_patterns table.
type ProjectPattern struct {
	PatternID   string `json:"pattern_id"`
	ProjectID   string `json:"-"`
	Detector    string `json:"detector"`
	Pattern     string `json:"pattern"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	CreatedBy   string `json:"created_by"`
	CreatedAt   string `json:"created_at"`
	MatchCount  int    `json:"match_count"`
}

// ListProjectPatterns returns the customer-defined custom patterns
// for the given (projectID, detector) pair, in created_at order
// (oldest first — preserves customer-perceived row stability across
// dashboard reloads).
func (s *SQLiteStore) ListProjectPatterns(
	ctx context.Context,
	projectID, detector string,
	enabledOnly bool,
) ([]*ProjectPattern, error) {
	q := `
		SELECT pattern_id, project_id, detector, pattern, severity,
		       description, enabled, created_by, created_at, match_count
		FROM project_patterns
		WHERE project_id = ? AND detector = ?
	`
	if enabledOnly {
		q += " AND enabled = 1"
	}
	q += " ORDER BY created_at ASC"

	rows, err := s.db.QueryContext(ctx, q, projectID, detector)
	if err != nil {
		return nil, fmt.Errorf("list project_patterns: %w", err)
	}
	defer rows.Close()

	var out []*ProjectPattern
	for rows.Next() {
		p := &ProjectPattern{}
		var enabledInt int
		if err := rows.Scan(
			&p.PatternID, &p.ProjectID, &p.Detector, &p.Pattern,
			&p.Severity, &p.Description, &enabledInt, &p.CreatedBy,
			&p.CreatedAt, &p.MatchCount,
		); err != nil {
			return nil, fmt.Errorf("scan project_pattern row: %w", err)
		}
		p.Enabled = enabledInt != 0
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows.Err on project_patterns scan: %w", err)
	}
	return out, nil
}

// CreateProjectPattern inserts a new pattern row.
func (s *SQLiteStore) CreateProjectPattern(
	ctx context.Context,
	p *ProjectPattern,
) error {
	enabledInt := 0
	if p.Enabled {
		enabledInt = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO project_patterns (
			pattern_id, project_id, detector, pattern, severity,
			description, enabled, created_by, created_at, match_count
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		p.PatternID, p.ProjectID, p.Detector, p.Pattern, p.Severity,
		p.Description, enabledInt, p.CreatedBy, p.CreatedAt, p.MatchCount,
	)
	if err != nil {
		return fmt.Errorf("create project_pattern: %w", err)
	}
	return nil
}

// UpdateProjectPattern overwrites the mutable fields of an existing
// pattern row. project_id + pattern_id must match (cross-project
// updates return ErrNotFound so a leaked pattern_id from one project
// cannot mutate another project's rows).
func (s *SQLiteStore) UpdateProjectPattern(
	ctx context.Context,
	p *ProjectPattern,
) error {
	enabledInt := 0
	if p.Enabled {
		enabledInt = 1
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE project_patterns
		SET pattern = ?, severity = ?, description = ?, enabled = ?
		WHERE project_id = ? AND pattern_id = ?
	`,
		p.Pattern, p.Severity, p.Description, enabledInt,
		p.ProjectID, p.PatternID,
	)
	if err != nil {
		return fmt.Errorf("update project_pattern: %w", err)
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

// DeleteProjectPattern removes a pattern row by (projectID, patternID).
func (s *SQLiteStore) DeleteProjectPattern(
	ctx context.Context,
	projectID, patternID string,
) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM project_patterns
		WHERE project_id = ? AND pattern_id = ?
	`, projectID, patternID)
	if err != nil {
		return fmt.Errorf("delete project_pattern: %w", err)
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

// IncrementPatternMatchCount adds delta to match_count.
func (s *SQLiteStore) IncrementPatternMatchCount(
	ctx context.Context,
	projectID, patternID string,
	delta int,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE project_patterns
		SET match_count = match_count + ?
		WHERE project_id = ? AND pattern_id = ?
	`, delta, projectID, patternID)
	if err != nil {
		return fmt.Errorf("increment project_pattern match_count: %w", err)
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

// CountProjectPatterns returns the row count for (projectID, detector).
// Used by the handler to enforce PROJECT_PATTERN_MAX before INSERT.
func (s *SQLiteStore) CountProjectPatterns(
	ctx context.Context,
	projectID, detector string,
) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM project_patterns
		WHERE project_id = ? AND detector = ?
	`, projectID, detector).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("count project_patterns: %w", err)
	}
	return n, nil
}
