package store

// Per-project detector threshold overrides — SQLite implementation
// (migration 048). Backs Theme B. Matches the
// postgres_detector_thresholds.go twin field-for-field.
//
// Schema lives in migrations/048_detector_thresholds.sql. One row
// per (project_id, detector, threshold_key) override; absent rows
// mean "use the registry default."
//
// The caller (API handler) is responsible for:
//   - Validating (detector, threshold_key) against the validators
//     registry before Set. Unknown tuples are rejected at 400.
//   - Parsing value_json from the request body and bounds-checking
//     it via the registry's per-spec validate function.
//   - Enforcing the tier cap (Hobby / Team / Enterprise) on top of
//     the registry validate fn. The store accepts whatever value
//     the handler hands it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DetectorThreshold is one row of the detector_thresholds table.
// Value is the raw JSON-encoded threshold value; the API handler
// (and B.b's hot-path resolver) is responsible for parsing it
// back into a typed value via the validators registry.
type DetectorThreshold struct {
	ProjectID    string `json:"-"`
	Detector     string `json:"detector"`
	ThresholdKey string `json:"threshold_key"`
	ValueJSON    string `json:"value_json"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// GetProjectDetectorThreshold reads the override for the given
// (projectID, detector, threshold_key). Returns ErrNotFound when
// no override row exists — the caller falls back to the registry
// default in that case.
func (s *SQLiteStore) GetProjectDetectorThreshold(
	ctx context.Context,
	projectID, detector, thresholdKey string,
) (*DetectorThreshold, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT project_id, detector, threshold_key, value_json, created_at, updated_at
		 FROM detector_thresholds
		 WHERE project_id = ? AND detector = ? AND threshold_key = ?`,
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
// given (projectID, detector) pair. Used by the dashboard editor
// and by the B.b hot-path bulk-read so a single SQL query fetches
// all of a detector's overrides at execution-close time.
func (s *SQLiteStore) ListProjectDetectorThresholds(
	ctx context.Context,
	projectID, detector string,
) ([]*DetectorThreshold, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT project_id, detector, threshold_key, value_json, created_at, updated_at
		 FROM detector_thresholds
		 WHERE project_id = ? AND detector = ?
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

// SetProjectDetectorThreshold upserts an override. Caller has
// already validated (detector, threshold_key) against the
// registry and bounds-checked + tier-capped valueJSON.
func (s *SQLiteStore) SetProjectDetectorThreshold(
	ctx context.Context,
	projectID, detector, thresholdKey, valueJSON string,
) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO detector_thresholds
		   (project_id, detector, threshold_key, value_json, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (project_id, detector, threshold_key) DO UPDATE SET
		   value_json = excluded.value_json,
		   updated_at = excluded.updated_at`,
		projectID, detector, thresholdKey, valueJSON, now, now,
	)
	if err != nil {
		return fmt.Errorf("upsert detector_threshold: %w", err)
	}
	return nil
}

// DeleteProjectDetectorThreshold removes an override row. The
// detector then falls back to the registry default for that
// (project, detector, threshold_key). Returns ErrNotFound when
// the override didn't exist (no information leak across projects;
// caller can swallow + return 204 if desired).
func (s *SQLiteStore) DeleteProjectDetectorThreshold(
	ctx context.Context,
	projectID, detector, thresholdKey string,
) error {
	res, err := s.db.ExecContext(
		ctx,
		`DELETE FROM detector_thresholds
		 WHERE project_id = ? AND detector = ? AND threshold_key = ?`,
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
