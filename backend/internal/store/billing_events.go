package store

// BillingEvent + store methods backing the Stripe webhook
// billing-event signal table. See
// migrations/034_billing_events.sql for the schema rationale.
//
// One row per Stripe webhook event we care about for fraud /
// dunning signaling. Today: charge.dispute.created (potential
// fraud) and invoice.payment_failed (dunning). Stripe's evt_xxx
// ID is the natural primary key so redelivered events become
// no-op INSERTs without any explicit dedupe lookup.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// BillingEvent kinds. Stable strings persisted in the kind column.
// Add new constants here when the dispatcher learns to handle
// additional Stripe event types.
const (
	BillingEventKindStripeDispute       = "stripe_dispute"
	BillingEventKindStripePaymentFailed = "stripe_payment_failed"
)

// BillingEvent severities. Surfaced on the admin tile and used to
// sort the unresolved list.
const (
	BillingEventSeverityHigh   = "high"
	BillingEventSeverityMedium = "medium"
	BillingEventSeverityLow    = "low"
)

// BillingEvent is one row of the billing_events table.
//
// EventID is the Stripe evt_xxx identifier; it's the natural
// primary key so a Stripe redelivery of the same webhook is an
// idempotent INSERT OR IGNORE on the handler.
//
// StripeObjectID points at the dispute_xxx or in_xxx that the
// event referenced so ops can deeplink into the Stripe Dashboard
// without parsing detail_json.
//
// DetailJSON carries per-kind context (dispute reason, invoice
// attempt_count, next_payment_attempt, etc.). Schema is owned by
// the handler code; nothing in this package validates it.
type BillingEvent struct {
	EventID          string     `json:"event_id"`
	ProjectID        string     `json:"project_id"`
	StripeCustomerID string     `json:"stripe_customer_id"`
	Kind             string     `json:"kind"`
	Severity         string     `json:"severity"`
	StripeObjectID   string     `json:"stripe_object_id"`
	AmountCents      int64      `json:"amount_cents"`
	Currency         string     `json:"currency,omitempty"`
	DetailJSON       string     `json:"detail_json,omitempty"`
	ReceivedAt       time.Time  `json:"received_at"`
	ResolvedAt       *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy       string     `json:"resolved_by,omitempty"`
	ResolutionNote   string     `json:"resolution_note,omitempty"`
}

// BillingEventFilter parameterizes ListBillingEvents. Zero-value
// filter (UnresolvedOnly=false, ProjectID="", Limit=0) returns the
// 100 most-recent events across all projects.
type BillingEventFilter struct {
	// UnresolvedOnly, when true, restricts the result to events
	// where resolved_at IS NULL. Backs the "needs attention" view
	// on the admin tile.
	UnresolvedOnly bool

	// ProjectID, when non-empty, restricts the result to events
	// for that project. Backs the per-project drill-down.
	ProjectID string

	// Limit caps the returned rows. Zero falls back to 100.
	Limit int
}

// --- SQLite impl --------------------------------------------------

// CreateBillingEvent persists one billing-event row. Uses
// INSERT OR IGNORE so a Stripe redelivery of the same evt_xxx
// silently becomes a no-op rather than a primary-key error.
func (s *SQLiteStore) CreateBillingEvent(ctx context.Context, e *BillingEvent) error {
	if e == nil {
		return errors.New("nil billing event")
	}
	if e.EventID == "" || e.ProjectID == "" || e.Kind == "" || e.Severity == "" {
		return errors.New("billing event missing required field (event_id, project_id, kind, severity)")
	}
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO billing_events (
			event_id, project_id, stripe_customer_id,
			kind, severity, stripe_object_id,
			amount_cents, currency, detail_json,
			received_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		e.EventID, e.ProjectID, e.StripeCustomerID,
		e.Kind, e.Severity, e.StripeObjectID,
		e.AmountCents, e.Currency, nullString(e.DetailJSON),
		e.ReceivedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("insert billing event: %w", err)
	}
	return nil
}

// ListBillingEvents reads billing events newest-first. Filter
// semantics:
//
//   - UnresolvedOnly=true: only resolved_at IS NULL rows.
//   - ProjectID non-empty: scope to one project.
//   - Both can combine (one project's unresolved events).
//
// Returns an empty slice (not nil) when no rows match so the JSON
// encoder produces [] rather than null.
func (s *SQLiteStore) ListBillingEvents(
	ctx context.Context, filter BillingEventFilter,
) ([]*BillingEvent, error) {
	if filter.Limit <= 0 {
		filter.Limit = 100
	}
	args := []any{}
	clauses := []string{}
	if filter.UnresolvedOnly {
		clauses = append(clauses, "resolved_at IS NULL")
	}
	if filter.ProjectID != "" {
		clauses = append(clauses, "project_id = ?")
		args = append(args, filter.ProjectID)
	}
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	args = append(args, filter.Limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT event_id, project_id, stripe_customer_id,
		       kind, severity, stripe_object_id,
		       amount_cents, currency, detail_json,
		       received_at, resolved_at, resolved_by, resolution_note
		FROM billing_events
		`+where+`
		ORDER BY received_at DESC
		LIMIT ?
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("list billing events: %w", err)
	}
	defer rows.Close()
	return scanBillingEventRows(rows)
}

// GetBillingEvent returns one row by event_id. Returns ErrNotFound
// when the row is absent.
func (s *SQLiteStore) GetBillingEvent(ctx context.Context, eventID string) (*BillingEvent, error) {
	if eventID == "" {
		return nil, errors.New("empty event id")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT event_id, project_id, stripe_customer_id,
		       kind, severity, stripe_object_id,
		       amount_cents, currency, detail_json,
		       received_at, resolved_at, resolved_by, resolution_note
		FROM billing_events
		WHERE event_id = ?
	`, eventID)
	e, err := scanOneBillingEventRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get billing event: %w", err)
	}
	return e, nil
}

// ResolveBillingEvent stamps resolved_at, resolved_by, and the
// human note on the row. Idempotent: a second call updates the
// fields again (which is fine for editing a typo in the note).
func (s *SQLiteStore) ResolveBillingEvent(
	ctx context.Context, eventID, resolvedBy, note string,
) error {
	if eventID == "" {
		return errors.New("empty event id")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE billing_events
		SET resolved_at = ?,
		    resolved_by = ?,
		    resolution_note = ?
		WHERE event_id = ?
	`, time.Now().UTC(), nullString(resolvedBy), nullString(note), eventID)
	if err != nil {
		return fmt.Errorf("resolve billing event: %w", err)
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// scanBillingEventRows is the shared row-loop used by every
// billing-event reader.
func scanBillingEventRows(rows *sql.Rows) ([]*BillingEvent, error) {
	out := make([]*BillingEvent, 0, 16)
	for rows.Next() {
		e := &BillingEvent{}
		var currency, detail, resolvedBy, note sql.NullString
		var resolvedAt sql.NullTime
		if err := rows.Scan(
			&e.EventID, &e.ProjectID, &e.StripeCustomerID,
			&e.Kind, &e.Severity, &e.StripeObjectID,
			&e.AmountCents, &currency, &detail,
			&e.ReceivedAt, &resolvedAt, &resolvedBy, &note,
		); err != nil {
			return nil, fmt.Errorf("scan billing event: %w", err)
		}
		if currency.Valid {
			e.Currency = currency.String
		}
		if detail.Valid {
			e.DetailJSON = detail.String
		}
		if resolvedAt.Valid {
			t := resolvedAt.Time.UTC()
			e.ResolvedAt = &t
		}
		if resolvedBy.Valid {
			e.ResolvedBy = resolvedBy.String
		}
		if note.Valid {
			e.ResolutionNote = note.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// scanOneBillingEventRow is the single-row scanner used by
// GetBillingEvent. Mirrors scanBillingEventRows but works against
// a *sql.Row rather than *sql.Rows.
func scanOneBillingEventRow(row *sql.Row) (*BillingEvent, error) {
	e := &BillingEvent{}
	var currency, detail, resolvedBy, note sql.NullString
	var resolvedAt sql.NullTime
	if err := row.Scan(
		&e.EventID, &e.ProjectID, &e.StripeCustomerID,
		&e.Kind, &e.Severity, &e.StripeObjectID,
		&e.AmountCents, &currency, &detail,
		&e.ReceivedAt, &resolvedAt, &resolvedBy, &note,
	); err != nil {
		return nil, err
	}
	if currency.Valid {
		e.Currency = currency.String
	}
	if detail.Valid {
		e.DetailJSON = detail.String
	}
	if resolvedAt.Valid {
		t := resolvedAt.Time.UTC()
		e.ResolvedAt = &t
	}
	if resolvedBy.Valid {
		e.ResolvedBy = resolvedBy.String
	}
	if note.Valid {
		e.ResolutionNote = note.String
	}
	return e, nil
}

// --- Postgres impl ------------------------------------------------

// CreateBillingEvent is the Postgres twin of the SQLite method.
// Uses ON CONFLICT (event_id) DO NOTHING for the same idempotency
// guarantee.
func (s *PostgresStore) CreateBillingEvent(ctx context.Context, e *BillingEvent) error {
	if e == nil {
		return errors.New("nil billing event")
	}
	if e.EventID == "" || e.ProjectID == "" || e.Kind == "" || e.Severity == "" {
		return errors.New("billing event missing required field (event_id, project_id, kind, severity)")
	}
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO billing_events (
			event_id, project_id, stripe_customer_id,
			kind, severity, stripe_object_id,
			amount_cents, currency, detail_json,
			received_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (event_id) DO NOTHING
	`,
		e.EventID, e.ProjectID, e.StripeCustomerID,
		e.Kind, e.Severity, e.StripeObjectID,
		e.AmountCents, e.Currency, nullString(e.DetailJSON),
		e.ReceivedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("insert billing event (postgres): %w", err)
	}
	return nil
}

// ListBillingEvents is the Postgres twin of the SQLite method.
// Postgres requires positional placeholders ($1, $2, ...) so we
// build the placeholder string alongside the WHERE clauses.
func (s *PostgresStore) ListBillingEvents(
	ctx context.Context, filter BillingEventFilter,
) ([]*BillingEvent, error) {
	if filter.Limit <= 0 {
		filter.Limit = 100
	}
	args := []any{}
	clauses := []string{}
	if filter.UnresolvedOnly {
		clauses = append(clauses, "resolved_at IS NULL")
	}
	if filter.ProjectID != "" {
		clauses = append(clauses,
			fmt.Sprintf("project_id = $%d", len(args)+1))
		args = append(args, filter.ProjectID)
	}
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	args = append(args, filter.Limit)
	limitPlaceholder := fmt.Sprintf("$%d", len(args))
	rows, err := s.db.QueryContext(ctx, `
		SELECT event_id, project_id, stripe_customer_id,
		       kind, severity, stripe_object_id,
		       amount_cents, currency, detail_json,
		       received_at, resolved_at, resolved_by, resolution_note
		FROM billing_events
		`+where+`
		ORDER BY received_at DESC
		LIMIT `+limitPlaceholder, args...)
	if err != nil {
		return nil, fmt.Errorf("list billing events (postgres): %w", err)
	}
	defer rows.Close()
	return scanBillingEventRows(rows)
}

// GetBillingEvent is the Postgres twin of the SQLite method.
func (s *PostgresStore) GetBillingEvent(ctx context.Context, eventID string) (*BillingEvent, error) {
	if eventID == "" {
		return nil, errors.New("empty event id")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT event_id, project_id, stripe_customer_id,
		       kind, severity, stripe_object_id,
		       amount_cents, currency, detail_json,
		       received_at, resolved_at, resolved_by, resolution_note
		FROM billing_events
		WHERE event_id = $1
	`, eventID)
	e, err := scanOneBillingEventRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get billing event (postgres): %w", err)
	}
	return e, nil
}

// ResolveBillingEvent is the Postgres twin of the SQLite method.
func (s *PostgresStore) ResolveBillingEvent(
	ctx context.Context, eventID, resolvedBy, note string,
) error {
	if eventID == "" {
		return errors.New("empty event id")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE billing_events
		SET resolved_at = $1,
		    resolved_by = $2,
		    resolution_note = $3
		WHERE event_id = $4
	`, time.Now().UTC(), nullString(resolvedBy), nullString(note), eventID)
	if err != nil {
		return fmt.Errorf("resolve billing event (postgres): %w", err)
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}
