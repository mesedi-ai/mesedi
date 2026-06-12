package store

// AuditEvent + store methods that back the Cloud Team audit-log
// feature (#207 v1). See migrations/024_audit_events.sql for the
// schema rationale.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AuditEvent is one row of the audit_events table. Stable shape
// surfaced to the customer dashboard at /app/audit-log.
type AuditEvent struct {
	EventID      string    `json:"event_id"`
	ProjectID    string    `json:"project_id"`
	ActorKeyID   string    `json:"actor_key_id,omitempty"`
	ActorKeyName string    `json:"actor_key_name,omitempty"`
	ActorEmail   string    `json:"actor_email,omitempty"`
	Action       string    `json:"action"`
	TargetType   string    `json:"target_type,omitempty"`
	TargetID     string    `json:"target_id,omitempty"`
	MetadataJSON string    `json:"metadata_json,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// AuditEventLister is the optional Store interface for audit log
// reads + writes. Kept on its own interface so test stubs can opt
// in via embedding only when relevant.
type AuditEventLister interface {
	// CreateAuditEvent inserts one row. Caller fills in EventID,
	// ProjectID, Action, and CreatedAt at minimum. Other fields
	// (ActorKeyID, ActorKeyName, ActorEmail, TargetType, TargetID,
	// MetadataJSON) are optional and omitted from the column list
	// when zero.
	CreateAuditEvent(ctx context.Context, e *AuditEvent) error

	// ListAuditEventsByProject returns the most recent audit events
	// for projectID ordered by created_at DESC. limit caps the
	// returned rows; pass 0 to fall back to a sane default (100).
	// Returns ErrNotFound only when projectID itself doesn't exist;
	// an empty list is a successful read with zero events.
	ListAuditEventsByProject(
		ctx context.Context, projectID string, limit int,
	) ([]*AuditEvent, error)
}

// --- SQLite impl --------------------------------------------------

// CreateAuditEvent persists one audit row. Best-effort use case:
// callers are encouraged to log on failure rather than fail the
// underlying business action (an inability to write the audit log
// should not block, e.g., an API key revocation).
func (s *SQLiteStore) CreateAuditEvent(ctx context.Context, e *AuditEvent) error {
	if e == nil {
		return errors.New("nil audit event")
	}
	if e.EventID == "" || e.ProjectID == "" || e.Action == "" {
		return errors.New("audit event missing required field (event_id, project_id, action)")
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_events (
			event_id, project_id,
			actor_key_id, actor_key_name, actor_email,
			action, target_type, target_id, metadata_json,
			created_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		e.EventID, e.ProjectID,
		nullString(e.ActorKeyID), nullString(e.ActorKeyName), nullString(e.ActorEmail),
		e.Action, nullString(e.TargetType), nullString(e.TargetID),
		nullString(e.MetadataJSON),
		e.CreatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}

// ListAuditEventsByProject reads the most recent audit events for
// projectID. Returns an empty slice (not nil) when there are no
// events so the JSON encoder produces [] rather than null.
func (s *SQLiteStore) ListAuditEventsByProject(
	ctx context.Context, projectID string, limit int,
) ([]*AuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT event_id, project_id,
		       actor_key_id, actor_key_name, actor_email,
		       action, target_type, target_id, metadata_json,
		       created_at
		FROM audit_events
		WHERE project_id = ?
		ORDER BY created_at DESC
		LIMIT ?
	`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()

	out := make([]*AuditEvent, 0, 16)
	for rows.Next() {
		e := &AuditEvent{}
		var keyID, keyName, email, targetType, targetID, meta sql.NullString
		if err := rows.Scan(
			&e.EventID, &e.ProjectID,
			&keyID, &keyName, &email,
			&e.Action, &targetType, &targetID, &meta,
			&e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		if keyID.Valid {
			e.ActorKeyID = keyID.String
		}
		if keyName.Valid {
			e.ActorKeyName = keyName.String
		}
		if email.Valid {
			e.ActorEmail = email.String
		}
		if targetType.Valid {
			e.TargetType = targetType.String
		}
		if targetID.Valid {
			e.TargetID = targetID.String
		}
		if meta.Valid {
			e.MetadataJSON = meta.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- Postgres impl ------------------------------------------------

// CreateAuditEvent is the Postgres twin of the SQLite method.
func (s *PostgresStore) CreateAuditEvent(ctx context.Context, e *AuditEvent) error {
	if e == nil {
		return errors.New("nil audit event")
	}
	if e.EventID == "" || e.ProjectID == "" || e.Action == "" {
		return errors.New("audit event missing required field (event_id, project_id, action)")
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_events (
			event_id, project_id,
			actor_key_id, actor_key_name, actor_email,
			action, target_type, target_id, metadata_json,
			created_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`,
		e.EventID, e.ProjectID,
		nullString(e.ActorKeyID), nullString(e.ActorKeyName), nullString(e.ActorEmail),
		e.Action, nullString(e.TargetType), nullString(e.TargetID),
		nullString(e.MetadataJSON),
		e.CreatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("insert audit event (postgres): %w", err)
	}
	return nil
}

// ListAuditEventsByProject is the Postgres twin of the SQLite method.
func (s *PostgresStore) ListAuditEventsByProject(
	ctx context.Context, projectID string, limit int,
) ([]*AuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT event_id, project_id,
		       actor_key_id, actor_key_name, actor_email,
		       action, target_type, target_id, metadata_json,
		       created_at
		FROM audit_events
		WHERE project_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()

	out := make([]*AuditEvent, 0, 16)
	for rows.Next() {
		e := &AuditEvent{}
		var keyID, keyName, email, targetType, targetID, meta sql.NullString
		if err := rows.Scan(
			&e.EventID, &e.ProjectID,
			&keyID, &keyName, &email,
			&e.Action, &targetType, &targetID, &meta,
			&e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		if keyID.Valid {
			e.ActorKeyID = keyID.String
		}
		if keyName.Valid {
			e.ActorKeyName = keyName.String
		}
		if email.Valid {
			e.ActorEmail = email.String
		}
		if targetType.Valid {
			e.TargetType = targetType.String
		}
		if targetID.Valid {
			e.TargetID = targetID.String
		}
		if meta.Valid {
			e.MetadataJSON = meta.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
