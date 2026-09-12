// Abuse signals and project suspension.
// Split out of postgres.go on 2026-09-12; every declaration moved verbatim.
package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (s *PostgresStore) CreateAbuseSignal(ctx context.Context, sig *AbuseSignal) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO abuse_signals
		    (signal_id, project_id, kind, severity, detail, detected_at)
		VALUES ($1, $2, $3, $4, $5, $6)
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

func (s *PostgresStore) ListAbuseSignals(ctx context.Context, unresolvedOnly bool, limit int) ([]*AbuseSignal, error) {
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

	var rows *sql.Rows
	var err error
	if limit > 0 {
		q += " LIMIT $1"
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

func (s *PostgresStore) GetAbuseSignal(ctx context.Context, signalID string) (*AbuseSignal, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT signal_id, project_id, kind, severity, detail,
		       detected_at, notified_at, suspended_at,
		       resolved_at, resolved_by, resolution_note
		FROM abuse_signals WHERE signal_id = $1
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

func (s *PostgresStore) MarkAbuseSignalNotified(ctx context.Context, signalID string, notifiedAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE abuse_signals SET notified_at = $1 WHERE signal_id = $2`,
		notifiedAt.Unix(), signalID,
	)
	return err
}

func (s *PostgresStore) MarkAbuseSignalSuspended(ctx context.Context, signalID, projectID, reason string, suspendedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE abuse_signals SET suspended_at = $1 WHERE signal_id = $2`,
		suspendedAt.Unix(), signalID,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE projects SET suspended_at = $1, suspension_reason = $2 WHERE project_id = $3`,
		suspendedAt.Unix(), reason, projectID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) ResolveAbuseSignal(ctx context.Context, signalID, resolvedBy, note string, resolvedAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE abuse_signals
		   SET resolved_at = $1, resolved_by = $2, resolution_note = $3
		 WHERE signal_id = $4`,
		resolvedAt.Unix(), resolvedBy, note, signalID,
	)
	return err
}

func (s *PostgresStore) UnsuspendProject(ctx context.Context, projectID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE projects SET suspended_at = NULL, suspension_reason = NULL WHERE project_id = $1`,
		projectID,
	)
	return err
}

func (s *PostgresStore) IsProjectSuspended(ctx context.Context, projectID string) (bool, string, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT suspended_at, suspension_reason FROM projects WHERE project_id = $1`,
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
