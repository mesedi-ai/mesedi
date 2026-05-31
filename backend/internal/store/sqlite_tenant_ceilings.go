package store

// Tenant budget ceiling store methods, SQLite implementation (#252).
//
// Lives in its own file because the ceiling story is self-contained:
// CRUD + two evaluator-managed columns (last_evaluated_at, breached_at).
// Keeping it separate from sqlite.go's mega-file makes the surface
// easy to grep for when the scheduler is the thing acting weird.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GetTenantBudgetCeiling returns the ceiling row for ownerUserID, or
// ErrNotFound when no row exists. The scheduler treats ErrNotFound
// as "no ceiling configured for this tenant" and skips evaluation.
func (s *SQLiteStore) GetTenantBudgetCeiling(
	ctx context.Context,
	ownerUserID string,
) (*TenantBudgetCeiling, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT owner_user_id, monthly_ceiling_usd, breach_action,
		       notify_email, notify_webhook_url,
		       created_at, updated_at, last_evaluated_at, breached_at
		FROM tenant_budget_ceilings
		WHERE owner_user_id = ?
	`, ownerUserID)
	return scanTenantBudgetCeilingSQLite(row)
}

// UpsertTenantBudgetCeiling inserts on first save, updates on
// subsequent saves. created_at is preserved across updates; updated_at
// is bumped to now. last_evaluated_at and breached_at are NEVER
// touched by this method, the scheduler owns them.
func (s *SQLiteStore) UpsertTenantBudgetCeiling(
	ctx context.Context,
	c *TenantBudgetCeiling,
) error {
	if c == nil || c.OwnerUserID == "" {
		return fmt.Errorf("owner_user_id required")
	}
	if c.BreachAction == "" {
		c.BreachAction = "warn"
	}
	now := time.Now().UTC().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tenant_budget_ceilings (
			owner_user_id, monthly_ceiling_usd, breach_action,
			notify_email, notify_webhook_url,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(owner_user_id) DO UPDATE SET
			monthly_ceiling_usd = excluded.monthly_ceiling_usd,
			breach_action       = excluded.breach_action,
			notify_email        = excluded.notify_email,
			notify_webhook_url  = excluded.notify_webhook_url,
			updated_at          = excluded.updated_at
	`,
		c.OwnerUserID, c.MonthlyCeilingUSD, c.BreachAction,
		nullableString(c.NotifyEmail), nullableString(c.NotifyWebhookURL),
		now, now,
		// (nullableString is the existing helper in sqlite.go that
		// maps "" -> sql.NullString{}; both signatures interop
		// because database/sql accepts sql.NullString as a positional
		// parameter.)
	)
	if err != nil {
		return fmt.Errorf("upsert tenant_budget_ceiling: %w", err)
	}
	return nil
}

// ListTenantBudgetCeilings returns every ceiling row. The scheduler
// reads this list each tick to evaluate burn against ceiling per
// tenant. Order is unspecified.
func (s *SQLiteStore) ListTenantBudgetCeilings(
	ctx context.Context,
) ([]*TenantBudgetCeiling, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT owner_user_id, monthly_ceiling_usd, breach_action,
		       notify_email, notify_webhook_url,
		       created_at, updated_at, last_evaluated_at, breached_at
		FROM tenant_budget_ceilings
	`)
	if err != nil {
		return nil, fmt.Errorf("list tenant_budget_ceilings: %w", err)
	}
	defer rows.Close()

	out := make([]*TenantBudgetCeiling, 0, 8)
	for rows.Next() {
		c, scanErr := scanTenantBudgetCeilingRowsSQLite(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkTenantCeilingEvaluated bumps last_evaluated_at to `at`. Called
// by the scheduler after each successful tick.
func (s *SQLiteStore) MarkTenantCeilingEvaluated(
	ctx context.Context,
	ownerUserID string,
	at time.Time,
) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE tenant_budget_ceilings
		SET last_evaluated_at = ?
		WHERE owner_user_id = ?
	`, at.UTC().Unix(), ownerUserID)
	if err != nil {
		return fmt.Errorf("mark ceiling evaluated: %w", err)
	}
	return nil
}

// SetTenantCeilingBreached updates breached_at. Pass non-nil to record
// a breach (typically time.Now()); pass nil to clear at month rollover.
func (s *SQLiteStore) SetTenantCeilingBreached(
	ctx context.Context,
	ownerUserID string,
	breachedAt *time.Time,
) error {
	var arg any
	if breachedAt == nil {
		arg = nil
	} else {
		arg = breachedAt.UTC().Unix()
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE tenant_budget_ceilings
		SET breached_at = ?
		WHERE owner_user_id = ?
	`, arg, ownerUserID)
	if err != nil {
		return fmt.Errorf("set ceiling breached: %w", err)
	}
	return nil
}

// scanTenantBudgetCeilingSQLite scans a single sql.Row (from GetX) into
// a *TenantBudgetCeiling. Returns ErrNotFound on sql.ErrNoRows.
func scanTenantBudgetCeilingSQLite(row *sql.Row) (*TenantBudgetCeiling, error) {
	c := &TenantBudgetCeiling{}
	var notifyEmail, notifyWebhookURL sql.NullString
	var createdAt, updatedAt int64
	var lastEvaluatedAt, breachedAt sql.NullInt64

	err := row.Scan(
		&c.OwnerUserID, &c.MonthlyCeilingUSD, &c.BreachAction,
		&notifyEmail, &notifyWebhookURL,
		&createdAt, &updatedAt, &lastEvaluatedAt, &breachedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan tenant_budget_ceiling: %w", err)
	}
	if notifyEmail.Valid {
		c.NotifyEmail = notifyEmail.String
	}
	if notifyWebhookURL.Valid {
		c.NotifyWebhookURL = notifyWebhookURL.String
	}
	c.CreatedAt = time.Unix(createdAt, 0).UTC()
	c.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	if lastEvaluatedAt.Valid {
		t := time.Unix(lastEvaluatedAt.Int64, 0).UTC()
		c.LastEvaluatedAt = &t
	}
	if breachedAt.Valid {
		t := time.Unix(breachedAt.Int64, 0).UTC()
		c.BreachedAt = &t
	}
	return c, nil
}

// scanTenantBudgetCeilingRowsSQLite scans a single row from sql.Rows
// during a list query. Identical column ordering to the GetX scanner.
func scanTenantBudgetCeilingRowsSQLite(rows *sql.Rows) (*TenantBudgetCeiling, error) {
	c := &TenantBudgetCeiling{}
	var notifyEmail, notifyWebhookURL sql.NullString
	var createdAt, updatedAt int64
	var lastEvaluatedAt, breachedAt sql.NullInt64

	if err := rows.Scan(
		&c.OwnerUserID, &c.MonthlyCeilingUSD, &c.BreachAction,
		&notifyEmail, &notifyWebhookURL,
		&createdAt, &updatedAt, &lastEvaluatedAt, &breachedAt,
	); err != nil {
		return nil, fmt.Errorf("scan tenant_budget_ceiling row: %w", err)
	}
	if notifyEmail.Valid {
		c.NotifyEmail = notifyEmail.String
	}
	if notifyWebhookURL.Valid {
		c.NotifyWebhookURL = notifyWebhookURL.String
	}
	c.CreatedAt = time.Unix(createdAt, 0).UTC()
	c.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	if lastEvaluatedAt.Valid {
		t := time.Unix(lastEvaluatedAt.Int64, 0).UTC()
		c.LastEvaluatedAt = &t
	}
	if breachedAt.Valid {
		t := time.Unix(breachedAt.Int64, 0).UTC()
		c.BreachedAt = &t
	}
	return c, nil
}

// nullableString is defined in sqlite.go and shared across this
// package; do not redefine it here.
