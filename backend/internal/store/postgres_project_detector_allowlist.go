package store

// Per-project detector-allowlist storage (Wave Allowlist.a).
// Postgres implementation (migration 049). Twin of
// sqlite_project_detector_allowlist.go — same shape, same contract,
// just $1/$2 placeholders instead of ? and Postgres handling.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *PostgresStore) ListProjectAllowlist(
	ctx context.Context,
	projectID, detector string,
) ([]*AllowlistEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT allowlist_id, project_id, detector, allowlist_key,
		       reason, created_by, created_at, match_count
		FROM project_detector_allowlist
		WHERE project_id = $1 AND detector = $2
		ORDER BY created_at ASC
	`, projectID, detector)
	if err != nil {
		return nil, fmt.Errorf("list project_detector_allowlist: %w", err)
	}
	defer rows.Close()

	var out []*AllowlistEntry
	for rows.Next() {
		e := &AllowlistEntry{}
		if err := rows.Scan(
			&e.AllowlistID, &e.ProjectID, &e.Detector, &e.AllowlistKey,
			&e.Reason, &e.CreatedBy, &e.CreatedAt, &e.MatchCount,
		); err != nil {
			return nil, fmt.Errorf("scan project_detector_allowlist row: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows.Err on project_detector_allowlist scan: %w", err)
	}
	return out, nil
}

func (s *PostgresStore) GetProjectAllowlistEntry(
	ctx context.Context,
	projectID, allowlistID string,
) (*AllowlistEntry, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT allowlist_id, project_id, detector, allowlist_key,
		       reason, created_by, created_at, match_count
		FROM project_detector_allowlist
		WHERE project_id = $1 AND allowlist_id = $2
	`, projectID, allowlistID)

	e := &AllowlistEntry{}
	if err := row.Scan(
		&e.AllowlistID, &e.ProjectID, &e.Detector, &e.AllowlistKey,
		&e.Reason, &e.CreatedBy, &e.CreatedAt, &e.MatchCount,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get project_detector_allowlist: %w", err)
	}
	return e, nil
}

func (s *PostgresStore) CreateProjectAllowlistEntry(
	ctx context.Context,
	e *AllowlistEntry,
) error {
	createdAt := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO project_detector_allowlist
		  (allowlist_id, project_id, detector, allowlist_key,
		   reason, created_by, created_at, match_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 0)
	`,
		e.AllowlistID, e.ProjectID, e.Detector, e.AllowlistKey,
		e.Reason, e.CreatedBy, createdAt,
	)
	if err != nil {
		return fmt.Errorf("create project_detector_allowlist: %w", err)
	}
	e.CreatedAt = createdAt
	e.MatchCount = 0
	return nil
}

func (s *PostgresStore) UpdateProjectAllowlistEntry(
	ctx context.Context,
	e *AllowlistEntry,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE project_detector_allowlist
		   SET allowlist_key = $1, reason = $2
		 WHERE project_id = $3 AND allowlist_id = $4
	`, e.AllowlistKey, e.Reason, e.ProjectID, e.AllowlistID)
	if err != nil {
		return fmt.Errorf("update project_detector_allowlist: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update project_detector_allowlist rows-affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) DeleteProjectAllowlistEntry(
	ctx context.Context,
	projectID, allowlistID string,
) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM project_detector_allowlist
		 WHERE project_id = $1 AND allowlist_id = $2
	`, projectID, allowlistID)
	if err != nil {
		return fmt.Errorf("delete project_detector_allowlist: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete project_detector_allowlist rows-affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) CountProjectAllowlistEntries(
	ctx context.Context,
	projectID, detector string,
) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM project_detector_allowlist
		WHERE project_id = $1 AND detector = $2
	`, projectID, detector).Scan(&n); err != nil {
		return 0, fmt.Errorf("count project_detector_allowlist: %w", err)
	}
	return n, nil
}

func (s *PostgresStore) CheckAllowlistMatch(
	ctx context.Context,
	projectID, detector, signature string,
) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM project_detector_allowlist
		WHERE project_id = $1
		  AND detector = $2
		  AND allowlist_key = $3
	`, projectID, detector, signature).Scan(&n); err != nil {
		return false, fmt.Errorf("check allowlist match: %w", err)
	}
	return n > 0, nil
}

func (s *PostgresStore) IncrementAllowlistMatchCount(
	ctx context.Context,
	projectID, detector, allowlistKey string,
	delta int,
) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE project_detector_allowlist
		   SET match_count = match_count + $1
		 WHERE project_id = $2
		   AND detector = $3
		   AND allowlist_key = $4
	`, delta, projectID, detector, allowlistKey)
	if err != nil {
		return fmt.Errorf("increment allowlist match_count: %w", err)
	}
	return nil
}

// GetAllowlistStats — Postgres twin of the SQLite implementation.
// One indexed scan filtered by project_id, GROUP BY detector. See
// the SQLite version's docstring for the full contract.
func (s *PostgresStore) GetAllowlistStats(
	ctx context.Context,
	projectID string,
) ([]AllowlistDetectorStats, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
		  detector,
		  COUNT(*) AS entry_count,
		  COALESCE(SUM(match_count), 0) AS total_match_count,
		  SUM(CASE WHEN match_count = 0 THEN 1 ELSE 0 END) AS dormant_count
		FROM project_detector_allowlist
		WHERE project_id = $1
		GROUP BY detector
		ORDER BY detector ASC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("get allowlist stats: %w", err)
	}
	defer rows.Close()

	var out []AllowlistDetectorStats
	for rows.Next() {
		var r AllowlistDetectorStats
		if err := rows.Scan(
			&r.Detector, &r.EntryCount, &r.TotalMatchCount, &r.DormantCount,
		); err != nil {
			return nil, fmt.Errorf("scan allowlist stats row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows.Err on allowlist stats scan: %w", err)
	}
	return out, nil
}
