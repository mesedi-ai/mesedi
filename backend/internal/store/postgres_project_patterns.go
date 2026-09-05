package store

// Per-project custom pattern storage for the three security
// detectors (prompt_injection, data_leakage, sandbox_escape).
// Postgres implementation (migration 045). Twin of
// sqlite_project_patterns.go, same shape, same contract, just
// $1/$2 placeholders instead of ? and Postgres boolean handling.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *PostgresStore) ListProjectPatterns(
	ctx context.Context,
	projectID, detector string,
	enabledOnly bool,
) ([]*ProjectPattern, error) {
	q := `
		SELECT pattern_id, project_id, detector, pattern, severity,
		       description, enabled, created_by, created_at, match_count,
		       last_matched_at
		FROM project_patterns
		WHERE project_id = $1 AND detector = $2
	`
	if enabledOnly {
		q += " AND enabled = TRUE"
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
		var lastMatchedAt sql.NullTime
		if err := rows.Scan(
			&p.PatternID, &p.ProjectID, &p.Detector, &p.Pattern,
			&p.Severity, &p.Description, &p.Enabled, &p.CreatedBy,
			&p.CreatedAt, &p.MatchCount, &lastMatchedAt,
		); err != nil {
			return nil, fmt.Errorf("scan project_pattern row: %w", err)
		}
		if lastMatchedAt.Valid {
			s := lastMatchedAt.Time.UTC().Format(time.RFC3339)
			p.LastMatchedAt = &s
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows.Err on project_patterns scan: %w", err)
	}
	return out, nil
}

func (s *PostgresStore) CreateProjectPattern(
	ctx context.Context,
	p *ProjectPattern,
) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO project_patterns (
			pattern_id, project_id, detector, pattern, severity,
			description, enabled, created_by, created_at, match_count
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`,
		p.PatternID, p.ProjectID, p.Detector, p.Pattern, p.Severity,
		p.Description, p.Enabled, p.CreatedBy, p.CreatedAt, p.MatchCount,
	)
	if err != nil {
		return fmt.Errorf("create project_pattern: %w", err)
	}
	return nil
}

func (s *PostgresStore) UpdateProjectPattern(
	ctx context.Context,
	p *ProjectPattern,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE project_patterns
		SET pattern = $1, severity = $2, description = $3, enabled = $4
		WHERE project_id = $5 AND pattern_id = $6
	`,
		p.Pattern, p.Severity, p.Description, p.Enabled,
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

func (s *PostgresStore) DeleteProjectPattern(
	ctx context.Context,
	projectID, patternID string,
) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM project_patterns
		WHERE project_id = $1 AND pattern_id = $2
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

func (s *PostgresStore) IncrementPatternMatchCount(
	ctx context.Context,
	projectID, patternID string,
	delta int,
) error {
	//  update last_matched_at alongside the counter so
	// the dashboard's 'dormant' badge stays accurate.
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE project_patterns
		SET match_count = match_count + $1,
		    last_matched_at = $2
		WHERE project_id = $3 AND pattern_id = $4
	`, delta, now, projectID, patternID)
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

func (s *PostgresStore) CountProjectPatterns(
	ctx context.Context,
	projectID, detector string,
) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM project_patterns
		WHERE project_id = $1 AND detector = $2
	`, projectID, detector).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("count project_patterns: %w", err)
	}
	return n, nil
}
