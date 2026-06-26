package store

// SystemEvent + store methods for the system_events table introduced
// in migration 050 (#276.f). Separates "customer-visible admin
// actions" (audit_events) from "system operational events"
// (system_events).
//
// What writes here:
//
//   - config_fallback events when a per-project config read fails
//     and the handler falls back to a hardcoded default. Previously
//     these polluted audit_events alongside customer-initiated admin
//     actions; the dashboard chip surfaces these as operational
//     telemetry, not as audit history.
//
// Schema is intentionally narrow — no actor_* columns since system
// events have no human actor; the `actor` string identifies which
// subsystem produced the row.

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// SystemEvent is one row of the system_events table.
type SystemEvent struct {
	EventID     string
	ProjectID   string
	Actor       string // e.g. "config_fallback"
	Action      string
	TargetType  string
	TargetID    string
	PayloadJSON string
	CreatedAt   time.Time
}

// --- SQLite impl --------------------------------------------------

// InsertSystemEvent persists one system event. Best-effort: callers
// log on failure rather than fail the underlying business action.
func (s *SQLiteStore) InsertSystemEvent(
	ctx context.Context, e *SystemEvent,
) error {
	if e == nil {
		return errors.New("nil system event")
	}
	if e.EventID == "" || e.ProjectID == "" || e.Action == "" {
		return errors.New(
			"system event missing required field " +
				"(event_id, project_id, action)",
		)
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO system_events (
			event_id, project_id, actor,
			action, target_type, target_id,
			payload_json, created_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`,
		e.EventID, e.ProjectID, e.Actor,
		e.Action, e.TargetType, e.TargetID,
		nullString(e.PayloadJSON),
		e.CreatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("insert system event: %w", err)
	}
	return nil
}

// --- Postgres impl ------------------------------------------------

// InsertSystemEvent is the Postgres twin.
func (s *PostgresStore) InsertSystemEvent(
	ctx context.Context, e *SystemEvent,
) error {
	if e == nil {
		return errors.New("nil system event")
	}
	if e.EventID == "" || e.ProjectID == "" || e.Action == "" {
		return errors.New(
			"system event missing required field " +
				"(event_id, project_id, action)",
		)
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO system_events (
			event_id, project_id, actor,
			action, target_type, target_id,
			payload_json, created_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`,
		e.EventID, e.ProjectID, e.Actor,
		e.Action, e.TargetType, e.TargetID,
		nullString(e.PayloadJSON),
		e.CreatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("insert system event (postgres): %w", err)
	}
	return nil
}
