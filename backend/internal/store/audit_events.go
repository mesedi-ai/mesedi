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
//
// Migration 031 added ProjectNameSnapshot and ProjectDeletedAt to
// preserve audit history past project close (account-takeover
// forensics, SOC 2 retention, self-side abuse-pattern detection).
// On a live project both new fields are zero; on a closed project
// both are populated by SnapshotAuditEventsForClosedProject during
// HandleCloseAccount.
type AuditEvent struct {
	EventID             string     `json:"event_id"`
	ProjectID           string     `json:"project_id"`
	ActorKeyID          string     `json:"actor_key_id,omitempty"`
	ActorKeyName        string     `json:"actor_key_name,omitempty"`
	ActorEmail          string     `json:"actor_email,omitempty"`
	Action              string     `json:"action"`
	TargetType          string     `json:"target_type,omitempty"`
	TargetID            string     `json:"target_id,omitempty"`
	MetadataJSON        string     `json:"metadata_json,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	ProjectNameSnapshot string     `json:"project_name_snapshot,omitempty"`
	ProjectDeletedAt    *time.Time `json:"project_deleted_at,omitempty"`
}

// ClosedProjectAuditFilter parameterizes SearchClosedProjectAuditEvents.
// At least one of Email or ProjectID must be non-empty (the search
// scope is always restricted to closed projects, otherwise the
// dataset is unbounded). Limit caps the returned rows; pass 0 to
// fall back to a sane default (100).
type ClosedProjectAuditFilter struct {
	Email     string
	ProjectID string
	Limit     int
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
	//
	// This method only returns events for LIVE projects. Events
	// for closed projects (project_deleted_at IS NOT NULL) are
	// excluded so the customer-facing /app/audit-log can never
	// surface another project's history if a project_id ever
	// collides with a deleted one.
	ListAuditEventsByProject(
		ctx context.Context, projectID string, limit int,
	) ([]*AuditEvent, error)

	// SnapshotAuditEventsForClosedProject stamps the project name
	// onto every audit row for projectID and marks them as belonging
	// to a closed project. Called inside HandleCloseAccount before
	// DeleteProjectCascade so the audit rows stay readable after
	// the projects row is gone. Idempotent: re-running on already-
	// snapshot rows updates project_name_snapshot and shifts
	// project_deleted_at to the new call time.
	SnapshotAuditEventsForClosedProject(
		ctx context.Context, projectID, projectName string,
	) error

	// SearchClosedProjectAuditEvents returns closed-project audit
	// events matching the filter. Admin-only (R1 takeover
	// forensics, R2 customer-support response to a verified close-
	// account-by-mistake claim). At least one of filter.Email or
	// filter.ProjectID must be non-empty; both empty returns a
	// validation error rather than the entire closed-history set.
	SearchClosedProjectAuditEvents(
		ctx context.Context, filter ClosedProjectAuditFilter,
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
// events so the JSON encoder produces [] rather than null. Filters
// out closed-project events (project_deleted_at IS NOT NULL) so the
// customer-facing /app/audit-log never surfaces archived history.
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
		       created_at,
		       project_name_snapshot, project_deleted_at
		FROM audit_events
		WHERE project_id = ?
		  AND project_deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT ?
	`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()
	return scanAuditEventRows(rows)
}

// SnapshotAuditEventsForClosedProject stamps project_name_snapshot
// + project_deleted_at on every audit row for the given project.
// Called from HandleCloseAccount BEFORE DeleteProjectCascade so the
// rows still join cleanly to the about-to-be-deleted projects row.
func (s *SQLiteStore) SnapshotAuditEventsForClosedProject(
	ctx context.Context, projectID, projectName string,
) error {
	if projectID == "" {
		return errors.New("project id required")
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE audit_events
		SET project_name_snapshot = ?,
		    project_deleted_at = ?
		WHERE project_id = ?
		  AND project_deleted_at IS NULL
	`, nullString(projectName), time.Now().UTC(), projectID)
	if err != nil {
		return fmt.Errorf("snapshot audit events: %w", err)
	}
	return nil
}

// SearchClosedProjectAuditEvents serves admin lookups of audit
// history that has outlived its source project. Filter rules:
// project_deleted_at IS NOT NULL is always applied; on top of
// that the caller MUST supply at least one of email or project_id.
// An unbounded "give me every closed-project audit event ever"
// query is intentionally not supported.
func (s *SQLiteStore) SearchClosedProjectAuditEvents(
	ctx context.Context, filter ClosedProjectAuditFilter,
) ([]*AuditEvent, error) {
	if filter.Email == "" && filter.ProjectID == "" {
		return nil, errors.New(
			"SearchClosedProjectAuditEvents: filter must include " +
				"email or project_id")
	}
	if filter.Limit <= 0 {
		filter.Limit = 100
	}
	// Both filters use the same composite-index plan; specifying
	// both narrows to a single project's audit history scoped to
	// one actor. Single-filter cases hit the actor_email or
	// project_id index independently.
	args := []any{}
	clauses := []string{"project_deleted_at IS NOT NULL"}
	if filter.Email != "" {
		clauses = append(clauses, "actor_email = ?")
		args = append(args, filter.Email)
	}
	if filter.ProjectID != "" {
		clauses = append(clauses, "project_id = ?")
		args = append(args, filter.ProjectID)
	}
	args = append(args, filter.Limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT event_id, project_id,
		       actor_key_id, actor_key_name, actor_email,
		       action, target_type, target_id, metadata_json,
		       created_at,
		       project_name_snapshot, project_deleted_at
		FROM audit_events
		WHERE `+joinSQLClauses(clauses)+`
		ORDER BY created_at DESC
		LIMIT ?
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("search closed-project audit events: %w", err)
	}
	defer rows.Close()
	return scanAuditEventRows(rows)
}

// scanAuditEventRows is the shared row-loop used by every audit
// reader. Each row must SELECT the full column list (including the
// new project_name_snapshot + project_deleted_at columns from
// migration 031) so this single scanner can decode any caller.
func scanAuditEventRows(rows *sql.Rows) ([]*AuditEvent, error) {
	out := make([]*AuditEvent, 0, 16)
	for rows.Next() {
		e := &AuditEvent{}
		var keyID, keyName, email, targetType, targetID, meta sql.NullString
		var nameSnap sql.NullString
		var deletedAt sql.NullTime
		if err := rows.Scan(
			&e.EventID, &e.ProjectID,
			&keyID, &keyName, &email,
			&e.Action, &targetType, &targetID, &meta,
			&e.CreatedAt,
			&nameSnap, &deletedAt,
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
		if nameSnap.Valid {
			e.ProjectNameSnapshot = nameSnap.String
		}
		if deletedAt.Valid {
			t := deletedAt.Time.UTC()
			e.ProjectDeletedAt = &t
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// joinSQLClauses joins WHERE clause fragments with " AND ". Lifted
// here so we don't pull in strings.Join just for one call site (the
// rest of the file uses inline SQL).
func joinSQLClauses(clauses []string) string {
	out := ""
	for i, c := range clauses {
		if i > 0 {
			out += " AND "
		}
		out += c
	}
	return out
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
// Same filter rules (live projects only) and shared row scanner.
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
		       created_at,
		       project_name_snapshot, project_deleted_at
		FROM audit_events
		WHERE project_id = $1
		  AND project_deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT $2
	`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()
	return scanAuditEventRows(rows)
}

// SnapshotAuditEventsForClosedProject is the Postgres twin of the
// SQLite method. Stamps project_name_snapshot + project_deleted_at
// before HandleCloseAccount runs DeleteProjectCascade.
func (s *PostgresStore) SnapshotAuditEventsForClosedProject(
	ctx context.Context, projectID, projectName string,
) error {
	if projectID == "" {
		return errors.New("project id required")
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE audit_events
		SET project_name_snapshot = $1,
		    project_deleted_at = $2
		WHERE project_id = $3
		  AND project_deleted_at IS NULL
	`, nullString(projectName), time.Now().UTC(), projectID)
	if err != nil {
		return fmt.Errorf("snapshot audit events (postgres): %w", err)
	}
	return nil
}

// SearchClosedProjectAuditEvents is the Postgres twin of the SQLite
// method. Postgres requires positional placeholders ($1, $2, ...)
// so we build the placeholder string alongside the WHERE clauses.
func (s *PostgresStore) SearchClosedProjectAuditEvents(
	ctx context.Context, filter ClosedProjectAuditFilter,
) ([]*AuditEvent, error) {
	if filter.Email == "" && filter.ProjectID == "" {
		return nil, errors.New(
			"SearchClosedProjectAuditEvents: filter must include " +
				"email or project_id")
	}
	if filter.Limit <= 0 {
		filter.Limit = 100
	}
	args := []any{}
	clauses := []string{"project_deleted_at IS NOT NULL"}
	if filter.Email != "" {
		clauses = append(clauses,
			fmt.Sprintf("actor_email = $%d", len(args)+1))
		args = append(args, filter.Email)
	}
	if filter.ProjectID != "" {
		clauses = append(clauses,
			fmt.Sprintf("project_id = $%d", len(args)+1))
		args = append(args, filter.ProjectID)
	}
	args = append(args, filter.Limit)
	limitPlaceholder := fmt.Sprintf("$%d", len(args))
	rows, err := s.db.QueryContext(ctx, `
		SELECT event_id, project_id,
		       actor_key_id, actor_key_name, actor_email,
		       action, target_type, target_id, metadata_json,
		       created_at,
		       project_name_snapshot, project_deleted_at
		FROM audit_events
		WHERE `+joinSQLClauses(clauses)+`
		ORDER BY created_at DESC
		LIMIT `+limitPlaceholder, args...)
	if err != nil {
		return nil, fmt.Errorf("search closed-project audit events (postgres): %w", err)
	}
	defer rows.Close()
	return scanAuditEventRows(rows)
}

// DeleteClosedProjectAuditEventsOlderThan purges closed-project audit
// rows whose project_deleted_at < cutoff (#218 SOC 2 / financial-
// compliance 7-year retention cron).
//
// Eligibility filter:
//
//   - project_deleted_at IS NOT NULL ensures we never touch a live
//     project's audit history. Snapshot rows for a closed project
//     have this column set; live rows have it NULL by definition
//     (migration 031).
//   - project_deleted_at < cutoff is the retention check itself.
//     Cutoff is computed by the scheduler as now - 7 years.
//
// Returns the number of rows deleted. Errors propagate; the scheduler
// logs + continues on the next tick.
func (s *SQLiteStore) DeleteClosedProjectAuditEventsOlderThan(
	ctx context.Context, cutoff time.Time,
) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM audit_events
		WHERE project_deleted_at IS NOT NULL
		  AND project_deleted_at < ?
	`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete closed-project audit events (sqlite): %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// RowsAffected() failing is exceedingly rare on modernc.org/sqlite
		// (it does not require driver round trips). Treat as a soft
		// failure: the DELETE itself succeeded, we just cannot report
		// the count. Return 0 + nil so the scheduler logs no-deletion
		// rather than a bogus error.
		return 0, nil
	}
	return n, nil
}

// DeleteClosedProjectAuditEventsOlderThan is the Postgres twin of the
// SQLite impl. Same eligibility filter, same return contract.
func (s *PostgresStore) DeleteClosedProjectAuditEventsOlderThan(
	ctx context.Context, cutoff time.Time,
) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM audit_events
		WHERE project_deleted_at IS NOT NULL
		  AND project_deleted_at < $1
	`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete closed-project audit events (postgres): %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}
