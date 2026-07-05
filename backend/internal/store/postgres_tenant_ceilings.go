package store

// Tenant budget ceiling store methods, Postgres implementation.
// Postgres counterpart to sqlite_tenant_ceilings.go. Same contract,
// $N placeholders, BIGINT epoch-second timestamps to match the 010
// migration.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GetTenantBudgetCeiling returns the ceiling row for ownerUserID, or
// ErrNotFound when no row exists.
func (s *PostgresStore) GetTenantBudgetCeiling(
	ctx context.Context,
	ownerUserID string,
) (*TenantBudgetCeiling, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT owner_user_id, monthly_ceiling_usd, breach_action,
		       notify_email, notify_webhook_url,
		       created_at, updated_at, last_evaluated_at, breached_at
		FROM tenant_budget_ceilings
		WHERE owner_user_id = $1
	`, ownerUserID)
	return scanTenantBudgetCeilingPg(row)
}

// UpsertTenantBudgetCeiling inserts on first save, updates on
// subsequent saves. last_evaluated_at and breached_at are NEVER
// touched by this method.
func (s *PostgresStore) UpsertTenantBudgetCeiling(
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
	var notifyEmail, notifyWebhookURL sql.NullString
	if c.NotifyEmail != "" {
		notifyEmail = sql.NullString{String: c.NotifyEmail, Valid: true}
	}
	if c.NotifyWebhookURL != "" {
		notifyWebhookURL = sql.NullString{String: c.NotifyWebhookURL, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tenant_budget_ceilings (
			owner_user_id, monthly_ceiling_usd, breach_action,
			notify_email, notify_webhook_url,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (owner_user_id) DO UPDATE SET
			monthly_ceiling_usd = EXCLUDED.monthly_ceiling_usd,
			breach_action       = EXCLUDED.breach_action,
			notify_email        = EXCLUDED.notify_email,
			notify_webhook_url  = EXCLUDED.notify_webhook_url,
			updated_at          = EXCLUDED.updated_at
	`,
		c.OwnerUserID, c.MonthlyCeilingUSD, c.BreachAction,
		notifyEmail, notifyWebhookURL,
		now, now,
	)
	if err != nil {
		return fmt.Errorf("upsert tenant_budget_ceiling: %w", err)
	}
	return nil
}

// ListTenantBudgetCeilings returns every ceiling row.
func (s *PostgresStore) ListTenantBudgetCeilings(
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
		c, scanErr := scanTenantBudgetCeilingRowsPg(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkTenantCeilingEvaluated bumps last_evaluated_at.
func (s *PostgresStore) MarkTenantCeilingEvaluated(
	ctx context.Context,
	ownerUserID string,
	at time.Time,
) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE tenant_budget_ceilings
		SET last_evaluated_at = $1
		WHERE owner_user_id = $2
	`, at.UTC().Unix(), ownerUserID)
	if err != nil {
		return fmt.Errorf("mark ceiling evaluated: %w", err)
	}
	return nil
}

// SetTenantCeilingBreached updates breached_at. Pass non-nil to record
// a breach (typically time.Now()); pass nil to clear at month rollover.
func (s *PostgresStore) SetTenantCeilingBreached(
	ctx context.Context,
	ownerUserID string,
	breachedAt *time.Time,
) error {
	var arg sql.NullInt64
	if breachedAt != nil {
		arg = sql.NullInt64{Int64: breachedAt.UTC().Unix(), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE tenant_budget_ceilings
		SET breached_at = $1
		WHERE owner_user_id = $2
	`, arg, ownerUserID)
	if err != nil {
		return fmt.Errorf("set ceiling breached: %w", err)
	}
	return nil
}

// scanTenantBudgetCeilingPg scans a single sql.Row into a *TenantBudgetCeiling.
func scanTenantBudgetCeilingPg(row *sql.Row) (*TenantBudgetCeiling, error) {
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

// scanTenantBudgetCeilingRowsPg scans one row from sql.Rows.
func scanTenantBudgetCeilingRowsPg(rows *sql.Rows) (*TenantBudgetCeiling, error) {
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
