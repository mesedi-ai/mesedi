// Abuse signals and project suspension.
// Split out of sqlite.go on 2026-09-12; every declaration moved verbatim.
package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// CreateAbuseSignal inserts a new row. Caller sets SignalID and
// DetectedAt; the worker updates the lifecycle columns via the Mark
// methods below.
func (s *SQLiteStore) CreateAbuseSignal(ctx context.Context, sig *AbuseSignal) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO abuse_signals
		    (signal_id, project_id, kind, severity, detail, detected_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`,
		sig.SignalID,
		sig.ProjectID,
		sig.Kind,
		sig.Severity,
		sig.Detail,
		sig.DetectedAt.Unix(),
	)
	return err
}

// ListAbuseSignals returns signals sorted by detected_at DESC. When
// unresolvedOnly is true the WHERE clause restricts to resolved_at
// IS NULL.
func (s *SQLiteStore) ListAbuseSignals(ctx context.Context, unresolvedOnly bool, limit int) ([]*AbuseSignal, error) {
	q := `
		SELECT signal_id, project_id, kind, severity, detail,
		       detected_at, notified_at, suspended_at,
		       resolved_at, resolved_by, resolution_note
		FROM abuse_signals
	`
	if unresolvedOnly {
		q += " WHERE resolved_at IS NULL"
	}
	q += " ORDER BY detected_at DESC"
	if limit > 0 {
		q += " LIMIT ?"
	}

	var rows *sql.Rows
	var err error
	if limit > 0 {
		rows, err = s.db.QueryContext(ctx, q, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, q)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*AbuseSignal
	for rows.Next() {
		sig := &AbuseSignal{}
		var detail, resolvedBy, note sql.NullString
		var detected, notified, suspended, resolved sql.NullInt64
		if err := rows.Scan(
			&sig.SignalID, &sig.ProjectID, &sig.Kind, &sig.Severity,
			&detail, &detected, &notified, &suspended,
			&resolved, &resolvedBy, &note,
		); err != nil {
			return nil, err
		}
		if detail.Valid {
			sig.Detail = detail.String
		}
		if detected.Valid {
			sig.DetectedAt = time.Unix(detected.Int64, 0).UTC()
		}
		if notified.Valid {
			t := time.Unix(notified.Int64, 0).UTC()
			sig.NotifiedAt = &t
		}
		if suspended.Valid {
			t := time.Unix(suspended.Int64, 0).UTC()
			sig.SuspendedAt = &t
		}
		if resolved.Valid {
			t := time.Unix(resolved.Int64, 0).UTC()
			sig.ResolvedAt = &t
		}
		if resolvedBy.Valid {
			sig.ResolvedBy = resolvedBy.String
		}
		if note.Valid {
			sig.ResolutionNote = note.String
		}
		out = append(out, sig)
	}
	return out, rows.Err()
}

// GetAbuseSignal fetches one row by id.
func (s *SQLiteStore) GetAbuseSignal(ctx context.Context, signalID string) (*AbuseSignal, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT signal_id, project_id, kind, severity, detail,
		       detected_at, notified_at, suspended_at,
		       resolved_at, resolved_by, resolution_note
		FROM abuse_signals WHERE signal_id = ?
	`, signalID)

	sig := &AbuseSignal{}
	var detail, resolvedBy, note sql.NullString
	var detected, notified, suspended, resolved sql.NullInt64
	if err := row.Scan(
		&sig.SignalID, &sig.ProjectID, &sig.Kind, &sig.Severity,
		&detail, &detected, &notified, &suspended,
		&resolved, &resolvedBy, &note,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if detail.Valid {
		sig.Detail = detail.String
	}
	if detected.Valid {
		sig.DetectedAt = time.Unix(detected.Int64, 0).UTC()
	}
	if notified.Valid {
		t := time.Unix(notified.Int64, 0).UTC()
		sig.NotifiedAt = &t
	}
	if suspended.Valid {
		t := time.Unix(suspended.Int64, 0).UTC()
		sig.SuspendedAt = &t
	}
	if resolved.Valid {
		t := time.Unix(resolved.Int64, 0).UTC()
		sig.ResolvedAt = &t
	}
	if resolvedBy.Valid {
		sig.ResolvedBy = resolvedBy.String
	}
	if note.Valid {
		sig.ResolutionNote = note.String
	}
	return sig, nil
}

// MarkAbuseSignalNotified stamps notified_at on the row.
func (s *SQLiteStore) MarkAbuseSignalNotified(ctx context.Context, signalID string, notifiedAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE abuse_signals SET notified_at = ? WHERE signal_id = ?`,
		notifiedAt.Unix(), signalID,
	)
	return err
}

// MarkAbuseSignalSuspended stamps suspended_at on the signal row AND
// flips projects.suspended_at + suspension_reason in the same
// transaction. The auth middleware's IsProjectSuspended check picks
// up the project flip on the next request.
func (s *SQLiteStore) MarkAbuseSignalSuspended(ctx context.Context, signalID, projectID, reason string, suspendedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE abuse_signals SET suspended_at = ? WHERE signal_id = ?`,
		suspendedAt.Unix(), signalID,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE projects SET suspended_at = ?, suspension_reason = ? WHERE project_id = ?`,
		suspendedAt.Unix(), reason, projectID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// ResolveAbuseSignal stamps the resolution columns. Does NOT touch
// projects.suspended_at; the caller is responsible for calling
// UnsuspendProject if reactivation is desired.
func (s *SQLiteStore) ResolveAbuseSignal(ctx context.Context, signalID, resolvedBy, note string, resolvedAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE abuse_signals
		   SET resolved_at = ?, resolved_by = ?, resolution_note = ?
		 WHERE signal_id = ?`,
		resolvedAt.Unix(), resolvedBy, note, signalID,
	)
	return err
}

// UnsuspendProject clears suspended_at + suspension_reason.
func (s *SQLiteStore) UnsuspendProject(ctx context.Context, projectID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE projects SET suspended_at = NULL, suspension_reason = NULL WHERE project_id = ?`,
		projectID,
	)
	return err
}

// IsProjectSuspended is the hot-path check for the auth middleware.
// Returns (false, "", nil) if active, (true, reason, nil) if not.
func (s *SQLiteStore) IsProjectSuspended(ctx context.Context, projectID string) (bool, string, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT suspended_at, suspension_reason FROM projects WHERE project_id = ?`,
		projectID,
	)
	var suspended sql.NullInt64
	var reason sql.NullString
	if err := row.Scan(&suspended, &reason); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, "", nil
		}
		return false, "", err
	}
	if !suspended.Valid {
		return false, "", nil
	}
	r := ""
	if reason.Valid {
		r = reason.String
	}
	return true, r, nil
}
