package store

// Per-project detector threshold overrides — Postgres
// implementation (migration 048). Twin of
// sqlite_detector_thresholds.go: same shape, same contract, $1/$2
// placeholders instead of ?, ON CONFLICT syntax adjusted.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GetProjectDetectorThreshold reads the override for the given
// (projectID, detector, threshold_key). Returns ErrNotFound when
// no override row exists.
func (s *PostgresStore) GetProjectDetectorThreshold(
	ctx context.Context,
	projectID, detector, thresholdKey string,
) (*DetectorThreshold, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT project_id, detector, threshold_key, value_json, created_at, updated_at
		 FROM detector_thresholds
		 WHERE project_id = $1 AND detector = $2 AND threshold_key = $3`,
		projectID, detector, thresholdKey,
	)
	t := &DetectorThreshold{}
	if err := row.Scan(
		&t.ProjectID, &t.Detector, &t.ThresholdKey,
		&t.ValueJSON, &t.CreatedAt, &t.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get detector_threshold: %w", err)
	}
	return t, nil
}

// ListProjectDetectorThresholds returns every override row for the
// given (projectID, detector) pair.
func (s *PostgresStore) ListProjectDetectorThresholds(
	ctx context.Context,
	projectID, detector string,
) ([]*DetectorThreshold, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT project_id, detector, threshold_key, value_json, created_at, updated_at
		 FROM detector_thresholds
		 WHERE project_id = $1 AND detector = $2
		 ORDER BY threshold_key ASC`,
		projectID, detector,
	)
	if err != nil {
		return nil, fmt.Errorf("list detector_thresholds: %w", err)
	}
	defer rows.Close()

	var out []*DetectorThreshold
	for rows.Next() {
		t := &DetectorThreshold{}
		if err := rows.Scan(
			&t.ProjectID, &t.Detector, &t.ThresholdKey,
			&t.ValueJSON, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan detector_threshold row: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows.Err on detector_thresholds scan: %w", err)
	}
	return out, nil
}

// SetProjectDetectorThreshold upserts an override. Postgres
// ON CONFLICT DO UPDATE keyed on the PK.
func (s *PostgresStore) SetProjectDetectorThreshold(
	ctx context.Context,
	projectID, detector, thresholdKey, valueJSON string,
) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO detector_thresholds
		   (project_id, detector, threshold_key, value_json, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (project_id, detector, threshold_key) DO UPDATE SET
		   value_json = EXCLUDED.value_json,
		   updated_at = EXCLUDED.updated_at`,
		projectID, detector, thresholdKey, valueJSON, now, now,
	)
	if err != nil {
		return fmt.Errorf("upsert detector_threshold: %w", err)
	}
	return nil
}

// DeleteProjectDetectorThreshold removes an override row.
// Returns ErrNotFound when no row matched.
func (s *PostgresStore) DeleteProjectDetectorThreshold(
	ctx context.Context,
	projectID, detector, thresholdKey string,
) error {
	res, err := s.db.ExecContext(
		ctx,
		`DELETE FROM detector_thresholds
		 WHERE project_id = $1 AND detector = $2 AND threshold_key = $3`,
		projectID, detector, thresholdKey,
	)
	if err != nil {
		return fmt.Errorf("delete detector_threshold: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete detector_threshold rows-affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
