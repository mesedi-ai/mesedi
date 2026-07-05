package store

// Store methods that back the Anthropic credit-balance snapshot
// surface. See migrations/025_anthropic_credit_snapshots.sql
// for schema rationale.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AnthropicCreditSnapshot is one manual-entry record of the
// remaining credit balance shown in the Anthropic Console sidebar.
type AnthropicCreditSnapshot struct {
	SnapshotID    string    `json:"snapshot_id"`
	BalanceUSD    float64   `json:"balance_usd"`
	SnapshottedAt time.Time `json:"snapshotted_at"`
	ActorEmail    string    `json:"actor_email,omitempty"`
	Note          string    `json:"note,omitempty"`
}

// --- SQLite impl --------------------------------------------------

// CreateAnthropicCreditSnapshot inserts one manually-entered
// snapshot of the remaining Anthropic credit balance. Caller fills
// in SnapshotID, BalanceUSD, and SnapshottedAt at minimum.
func (s *SQLiteStore) CreateAnthropicCreditSnapshot(
	ctx context.Context, snap *AnthropicCreditSnapshot,
) error {
	if snap == nil {
		return errors.New("nil credit snapshot")
	}
	if snap.SnapshotID == "" {
		return errors.New("credit snapshot missing snapshot_id")
	}
	if snap.SnapshottedAt.IsZero() {
		snap.SnapshottedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO anthropic_credit_snapshots (
			snapshot_id, balance_usd, snapshotted_at, actor_email, note
		)
		VALUES (?, ?, ?, ?, ?)
	`,
		snap.SnapshotID, snap.BalanceUSD, snap.SnapshottedAt.UTC(),
		nullString(snap.ActorEmail), nullString(snap.Note),
	)
	if err != nil {
		return fmt.Errorf("insert credit snapshot: %w", err)
	}
	return nil
}

// GetLatestAnthropicCreditSnapshot returns the most recent snapshot.
// Returns ErrNotFound when no snapshot has ever been recorded so
// the admin endpoint can render a "no balance recorded yet" UX.
func (s *SQLiteStore) GetLatestAnthropicCreditSnapshot(
	ctx context.Context,
) (*AnthropicCreditSnapshot, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT snapshot_id, balance_usd, snapshotted_at, actor_email, note
		FROM anthropic_credit_snapshots
		ORDER BY snapshotted_at DESC
		LIMIT 1
	`)
	return scanAnthropicCreditSnapshot(row)
}

// --- Postgres impl ------------------------------------------------

// CreateAnthropicCreditSnapshot is the Postgres twin.
func (s *PostgresStore) CreateAnthropicCreditSnapshot(
	ctx context.Context, snap *AnthropicCreditSnapshot,
) error {
	if snap == nil {
		return errors.New("nil credit snapshot")
	}
	if snap.SnapshotID == "" {
		return errors.New("credit snapshot missing snapshot_id")
	}
	if snap.SnapshottedAt.IsZero() {
		snap.SnapshottedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO anthropic_credit_snapshots (
			snapshot_id, balance_usd, snapshotted_at, actor_email, note
		)
		VALUES ($1, $2, $3, $4, $5)
	`,
		snap.SnapshotID, snap.BalanceUSD, snap.SnapshottedAt.UTC(),
		nullString(snap.ActorEmail), nullString(snap.Note),
	)
	if err != nil {
		return fmt.Errorf("insert credit snapshot (postgres): %w", err)
	}
	return nil
}

// GetLatestAnthropicCreditSnapshot is the Postgres twin.
func (s *PostgresStore) GetLatestAnthropicCreditSnapshot(
	ctx context.Context,
) (*AnthropicCreditSnapshot, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT snapshot_id, balance_usd, snapshotted_at, actor_email, note
		FROM anthropic_credit_snapshots
		ORDER BY snapshotted_at DESC
		LIMIT 1
	`)
	return scanAnthropicCreditSnapshot(row)
}

// scanAnthropicCreditSnapshot reads one row off a *sql.Row, mapping
// the nullable optional columns into the struct's zero-value-on-
// absent fields. ErrNotFound is returned when the underlying query
// produced no rows so the admin endpoint can render an empty-state.
func scanAnthropicCreditSnapshot(row *sql.Row) (*AnthropicCreditSnapshot, error) {
	snap := &AnthropicCreditSnapshot{}
	var actor, note sql.NullString
	err := row.Scan(
		&snap.SnapshotID, &snap.BalanceUSD, &snap.SnapshottedAt,
		&actor, &note,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan credit snapshot: %w", err)
	}
	if actor.Valid {
		snap.ActorEmail = actor.String
	}
	if note.Valid {
		snap.Note = note.String
	}
	return snap, nil
}
