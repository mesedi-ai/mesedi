// Project lifecycle, billing and usage-counter persistence.
// Split out of sqlite.go on 2026-09-12; every declaration moved verbatim.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *SQLiteStore) CreateProject(ctx context.Context, p *Project) error {
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	// Default Tier to "hobby" when the caller did not specify one.
	// Migration 006 sets the column default at the schema level, but
	// being explicit here keeps reads consistent with the in-memory
	// struct the caller passed in.
	if p.Tier == "" {
		p.Tier = "hobby"
	}
	// hotfix: explicitly insert card_on_file=0 instead of relying
	// on the migration-022 column default of TRUE. The default existed
	// for migration backfill reasons; new projects always start without
	// a card on file and flip to TRUE only via handleSetupIntentSucceeded.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (
			project_id, name, owner_user_id, owner_email, created_at, tier,
			card_on_file
		)
		VALUES (?, ?, ?, ?, ?, ?, 0)
	`, p.ProjectID, p.Name, nullString(p.OwnerUserID), nullString(p.OwnerEmail), p.CreatedAt, p.Tier)
	if err != nil {
		return fmt.Errorf("insert project: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetProject(ctx context.Context, projectID string) (*Project, error) {
	p := &Project{}
	var owner, email, stripeCust, stripeSub sql.NullString
	var periodStart, periodEnd sql.NullInt64
	var grantExpires, tierExpires sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT project_id, name, owner_user_id, owner_email, created_at,
		       tier, stripe_customer_id, stripe_subscription_id,
		       current_period_start, current_period_end, executions_this_period,
		       granted_executions, granted_executions_expires_at, tier_expires_at,
		       billing_cap_usd, card_on_file
		FROM projects WHERE project_id = ?
	`, projectID).Scan(
		&p.ProjectID, &p.Name, &owner, &email, &p.CreatedAt,
		&p.Tier, &stripeCust, &stripeSub,
		&periodStart, &periodEnd, &p.ExecutionsThisPeriod,
		&p.GrantedExecutions, &grantExpires, &tierExpires,
		&p.BillingCapUSD, &p.CardOnFile,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if owner.Valid {
		p.OwnerUserID = owner.String
	}
	if email.Valid {
		p.OwnerEmail = email.String
	}
	if stripeCust.Valid {
		p.StripeCustomerID = stripeCust.String
	}
	if stripeSub.Valid {
		p.StripeSubscriptionID = stripeSub.String
	}
	if periodStart.Valid {
		t := time.Unix(periodStart.Int64, 0).UTC()
		p.CurrentPeriodStart = &t
	}
	if periodEnd.Valid {
		t := time.Unix(periodEnd.Int64, 0).UTC()
		p.CurrentPeriodEnd = &t
	}
	if grantExpires.Valid {
		t := time.Unix(grantExpires.Int64, 0).UTC()
		p.GrantedExecutionsExpiresAt = &t
	}
	if tierExpires.Valid {
		t := time.Unix(tierExpires.Int64, 0).UTC()
		p.TierExpiresAt = &t
	}
	return p, nil
}

// GetMostRecentProjectByOwnerEmail resolves an email back to the
// customer's newest project. Used by the /signin handler after
// SSO/magic-link proves email ownership. See store.go interface for
// rationale; case-insensitive matching mirrors the signup handler
// which lowercases at write time.
func (s *SQLiteStore) GetMostRecentProjectByOwnerEmail(ctx context.Context, email string) (*Project, error) {
	if email == "" {
		return nil, ErrNotFound
	}
	p := &Project{}
	var owner, dbEmail, stripeCust, stripeSub sql.NullString
	var periodStart, periodEnd sql.NullInt64
	var grantExpires, tierExpires sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT project_id, name, owner_user_id, owner_email, created_at,
		       tier, stripe_customer_id, stripe_subscription_id,
		       current_period_start, current_period_end, executions_this_period,
		       granted_executions, granted_executions_expires_at, tier_expires_at,
		       billing_cap_usd, card_on_file
		FROM projects
		WHERE LOWER(owner_email) = LOWER(?)
		ORDER BY created_at DESC
		LIMIT 1
	`, email).Scan(
		&p.ProjectID, &p.Name, &owner, &dbEmail, &p.CreatedAt,
		&p.Tier, &stripeCust, &stripeSub,
		&periodStart, &periodEnd, &p.ExecutionsThisPeriod,
		&p.GrantedExecutions, &grantExpires, &tierExpires,
		&p.BillingCapUSD, &p.CardOnFile,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if owner.Valid {
		p.OwnerUserID = owner.String
	}
	if dbEmail.Valid {
		p.OwnerEmail = dbEmail.String
	}
	if stripeCust.Valid {
		p.StripeCustomerID = stripeCust.String
	}
	if stripeSub.Valid {
		p.StripeSubscriptionID = stripeSub.String
	}
	if periodStart.Valid {
		t := time.Unix(periodStart.Int64, 0).UTC()
		p.CurrentPeriodStart = &t
	}
	if periodEnd.Valid {
		t := time.Unix(periodEnd.Int64, 0).UTC()
		p.CurrentPeriodEnd = &t
	}
	if grantExpires.Valid {
		t := time.Unix(grantExpires.Int64, 0).UTC()
		p.GrantedExecutionsExpiresAt = &t
	}
	if tierExpires.Valid {
		t := time.Unix(tierExpires.Int64, 0).UTC()
		p.TierExpiresAt = &t
	}
	return p, nil
}

// ListProjectsByOwner returns every project belonging to ownerUserID,
// ordered created_at ASC. v0.1 of the org-rollup feature uses
// owner_user_id as the tenant boundary, so this is THE query that
// defines "what's in this tenant". When the real organizations table
// arrives, the call signature stays the same but the WHERE clause
// pivots to organization_members.
func (s *SQLiteStore) ListProjectsByOwner(ctx context.Context, ownerUserID string) ([]*Project, error) {
	if ownerUserID == "" {
		return []*Project{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT project_id, name, owner_user_id, owner_email, created_at,
		       tier, stripe_customer_id, stripe_subscription_id,
		       current_period_start, current_period_end, executions_this_period,
		       granted_executions, granted_executions_expires_at, tier_expires_at,
		       billing_cap_usd
		FROM projects
		WHERE owner_user_id = ?
		ORDER BY created_at ASC
	`, ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("list projects by owner: %w", err)
	}
	defer rows.Close()

	out := make([]*Project, 0, 4)
	for rows.Next() {
		p := &Project{}
		var owner, email, stripeCust, stripeSub sql.NullString
		var periodStart, periodEnd sql.NullInt64
		var grantExpires, tierExpires sql.NullInt64
		if err := rows.Scan(
			&p.ProjectID, &p.Name, &owner, &email, &p.CreatedAt,
			&p.Tier, &stripeCust, &stripeSub,
			&periodStart, &periodEnd, &p.ExecutionsThisPeriod,
			&p.GrantedExecutions, &grantExpires, &tierExpires,
			&p.BillingCapUSD,
		); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		if owner.Valid {
			p.OwnerUserID = owner.String
		}
		if email.Valid {
			p.OwnerEmail = email.String
		}
		if stripeCust.Valid {
			p.StripeCustomerID = stripeCust.String
		}
		if stripeSub.Valid {
			p.StripeSubscriptionID = stripeSub.String
		}
		if periodStart.Valid {
			t := time.Unix(periodStart.Int64, 0).UTC()
			p.CurrentPeriodStart = &t
		}
		if periodEnd.Valid {
			t := time.Unix(periodEnd.Int64, 0).UTC()
			p.CurrentPeriodEnd = &t
		}
		if grantExpires.Valid {
			t := time.Unix(grantExpires.Int64, 0).UTC()
			p.GrantedExecutionsExpiresAt = &t
		}
		if tierExpires.Valid {
			t := time.Unix(tierExpires.Int64, 0).UTC()
			p.TierExpiresAt = &t
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProjectStorageStats returns per-project counts + an estimated
// bytes total computed from SUM(LENGTH()) over the large text
// columns. Multiple correlated subqueries, fine at our scale,
// would warrant a rewrite if projects grow past a few thousand.
//
// Bytes are estimated, not exact: SQLite stores text with overhead
// (NULL terminator, variable-length row encoding), and there are
// indexes that take additional space the LENGTH sum doesn't see.
// The number is "close enough" for capacity planning, within
// maybe 30% of real disk footprint.
func (s *SQLiteStore) GetProjectStorageStats(ctx context.Context) ([]*ProjectStorage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			p.project_id,
			p.name,
			COALESCE(p.owner_email, ''),
			p.tier,
			COALESCE((
				SELECT COUNT(*) FROM executions e
				WHERE e.project_id = p.project_id
			), 0) AS executions,
			COALESCE((
				SELECT COUNT(*)
				FROM events ev
				JOIN executions e ON ev.execution_id = e.execution_id
				WHERE e.project_id = p.project_id
			), 0) AS events,
			COALESCE((
				SELECT COUNT(*) FROM failure_groups fg
				WHERE fg.project_id = p.project_id
			), 0) AS failure_groups,
			COALESCE((
				SELECT COUNT(*) FROM webhook_deliveries wd
				WHERE wd.project_id = p.project_id
			), 0) AS webhook_deliveries,
			COALESCE((
				SELECT SUM(LENGTH(e.input_summary) +
				           LENGTH(e.output_summary) +
				           LENGTH(e.crash_signature))
				FROM executions e WHERE e.project_id = p.project_id
			), 0) +
			COALESCE((
				SELECT SUM(LENGTH(ev.payload))
				FROM events ev
				JOIN executions e ON ev.execution_id = e.execution_id
				WHERE e.project_id = p.project_id
			), 0) AS estimated_bytes
		FROM projects p
		ORDER BY estimated_bytes DESC, executions DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("query project storage stats: %w", err)
	}
	defer rows.Close()

	out := []*ProjectStorage{}
	for rows.Next() {
		var row ProjectStorage
		if err := rows.Scan(
			&row.ProjectID, &row.Name, &row.OwnerEmail, &row.Tier,
			&row.Executions, &row.Events,
			&row.FailureGroups, &row.WebhookDeliveries,
			&row.EstimatedBytes,
		); err != nil {
			return nil, fmt.Errorf("scan storage row: %w", err)
		}
		out = append(out, &row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate storage rows: %w", err)
	}
	return out, nil
}

// DeleteProject hard-deletes a project. Schema has ON DELETE CASCADE
// on every child table's project_id FK (api_keys, executions,
// failure_groups, project_webhooks, webhook_deliveries) and on the
// events→executions FK, so the cascade is complete without manual
// child-table cleanup.
//
// Returns ErrNotFound when no rows were deleted (project never
// existed). The admin handler turns that into a 404, same behavior
// as the read path.
func (s *SQLiteStore) DeleteProject(ctx context.Context, projectID string) error {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM projects WHERE project_id = ?
	`, projectID)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteFailureGroupsByProject wipes every failure_group row owned by
// projectID. Returns the number of rows deleted. Non-failing on zero
// (caller may reset an already-empty project). See the interface
// definition in store.go for the use case (admin demo reset, ).
func (s *SQLiteStore) DeleteFailureGroupsByProject(ctx context.Context, projectID string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM failure_groups WHERE project_id = ?
	`, projectID)
	if err != nil {
		return 0, fmt.Errorf("delete failure_groups by project: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

// ListAllProjects returns every project plus activity aggregates from
// the executions table. Used only by the founder-side admin dashboard
// ; the customer-facing API has no equivalent endpoint.
//
// The LEFT JOIN preserves projects that have never produced an
// execution (signup-without-integration accounts), they show up with
// NULL last_activity and zero total_executions. SQLite's MAX/COUNT on
// an outer-joined NULL-rich relation correctly returns NULL/0.
//
// Ordering by created_at DESC puts newest signups at the top, which is
// what the founder wants to see first when checking for new activity.
func (s *SQLiteStore) ListAllProjects(ctx context.Context) ([]*AdminProjectRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			p.project_id, p.name, p.owner_email, p.created_at,
			p.tier, p.stripe_customer_id, p.stripe_subscription_id,
			p.current_period_start, p.current_period_end,
			p.executions_this_period, p.granted_executions,
			p.granted_executions_expires_at, p.tier_expires_at,
			MAX(e.started_at) AS last_activity_at,
			COUNT(e.execution_id) AS total_executions
		FROM projects p
		LEFT JOIN executions e ON e.project_id = p.project_id
		WHERE p.project_id != ?
		GROUP BY p.project_id
		ORDER BY p.created_at DESC
	`, APIKeyAdminProjectID)
	if err != nil {
		return nil, fmt.Errorf("query all projects: %w", err)
	}
	defer rows.Close()

	out := []*AdminProjectRow{}
	for rows.Next() {
		var (
			row                          AdminProjectRow
			email, stripeCust, stripeSub sql.NullString
			periodStart, periodEnd       sql.NullInt64
			grantExpires, tierExpires    sql.NullInt64
			// last_activity_at comes from MAX(e.started_at). The driver
			// returns it as a string because executions.started_at is
			// stored as TEXT (RFC3339Nano) in SQLite. Scanning into
			// sql.NullTime fails with "unsupported Scan, storing
			// driver.Value type string into type *time.Time"; use the
			// NullString + parseFlexTime pattern that the rest of the
			// store layer uses for TEXT timestamps.
			lastActivity sql.NullString
		)
		if err := rows.Scan(
			&row.ProjectID, &row.Name, &email, &row.CreatedAt,
			&row.Tier, &stripeCust, &stripeSub,
			&periodStart, &periodEnd,
			&row.ExecutionsThisPeriod, &row.GrantedExecutions,
			&grantExpires, &tierExpires,
			&lastActivity, &row.TotalExecutions,
		); err != nil {
			return nil, fmt.Errorf("scan project row: %w", err)
		}
		if email.Valid {
			row.OwnerEmail = email.String
		}
		if stripeCust.Valid {
			row.StripeCustomerID = stripeCust.String
		}
		if stripeSub.Valid {
			row.StripeSubscriptionID = stripeSub.String
		}
		if periodStart.Valid {
			t := time.Unix(periodStart.Int64, 0).UTC()
			row.CurrentPeriodStart = &t
		}
		if periodEnd.Valid {
			t := time.Unix(periodEnd.Int64, 0).UTC()
			row.CurrentPeriodEnd = &t
		}
		if lastActivity.Valid && lastActivity.String != "" {
			t := parseFlexTime(lastActivity.String)
			if !t.IsZero() {
				t = t.UTC()
				row.LastActivityAt = &t
			}
		}
		out = append(out, &row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project rows: %w", err)
	}
	return out, nil
}

// UpdateProjectTier flips a project to a different tier without
// touching the Stripe columns. Founder admin lever. Returns
// ErrNotFound if the project doesn't exist; the admin handler turns
// that into a 404. Permissible tier values are not enforced at the
// store layer, the API layer validates against the canonical
// TierHobby/TierTeam/TierEnterprise constants.
func (s *SQLiteStore) UpdateProjectTier(
	ctx context.Context,
	projectID, tier string,
	expiresAt *time.Time,
) error {
	var expires sql.NullInt64
	if expiresAt != nil {
		expires = sql.NullInt64{Int64: expiresAt.Unix(), Valid: true}
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET tier = ?, tier_expires_at = ?
		WHERE project_id = ?
	`, tier, expires, projectID)
	if err != nil {
		return fmt.Errorf("update project tier: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateProjectName writes a new display name on a project row. The
// store does no validation; the API handler enforces 1-80 char +
// trimmed-non-empty before calling here.
func (s *SQLiteStore) UpdateProjectName(
	ctx context.Context,
	projectID, name string,
) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET name = ?
		WHERE project_id = ?
	`, name, projectID)
	if err != nil {
		return fmt.Errorf("update project name: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AddGrantedExecutions adjusts the granted_executions column by delta.
// Positive delta grants additional quota; negative delta revokes a
// prior grant. The column is signed INTEGER so the result may go
// negative (e.g., admin granted 100K then revoked 200K); effective-
// quota math in billing.go floors at zero so a negative value never
// produces a "negative available" condition.
func (s *SQLiteStore) AddGrantedExecutions(
	ctx context.Context,
	projectID string,
	delta int64,
	expiresAt *time.Time,
) error {
	var expires sql.NullInt64
	if expiresAt != nil {
		expires = sql.NullInt64{Int64: expiresAt.Unix(), Valid: true}
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET granted_executions = granted_executions + ?,
		    granted_executions_expires_at = ?
		WHERE project_id = ?
	`, delta, expires, projectID)
	if err != nil {
		return fmt.Errorf("update granted executions: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateProjectBilling sets the tier, Stripe identifiers, and period
// bounds in one UPDATE. nullPtrTime treats nil pointers as NULL in the
// database (used to clear period bounds on subscription cancellation).
func (s *SQLiteStore) UpdateProjectBilling(
	ctx context.Context,
	projectID, tier, stripeCustomerID, stripeSubscriptionID string,
	periodStart, periodEnd *time.Time,
) error {
	if tier == "" {
		return fmt.Errorf("tier required")
	}
	var startUnix, endUnix sql.NullInt64
	if periodStart != nil {
		startUnix.Int64 = periodStart.UTC().Unix()
		startUnix.Valid = true
	}
	if periodEnd != nil {
		endUnix.Int64 = periodEnd.UTC().Unix()
		endUnix.Valid = true
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET tier = ?,
		    stripe_customer_id = ?,
		    stripe_subscription_id = ?,
		    current_period_start = ?,
		    current_period_end = ?
		WHERE project_id = ?
	`, tier, nullString(stripeCustomerID), nullString(stripeSubscriptionID), startUnix, endUnix, projectID)
	if err != nil {
		return fmt.Errorf("update project billing: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetProjectByStripeCustomerID resolves a Stripe customer id to the
// owning project. Used by the webhook handler when Stripe sends an
// event keyed by customer rather than by Mesedi project_id.
func (s *SQLiteStore) GetProjectByStripeCustomerID(
	ctx context.Context, stripeCustomerID string,
) (*Project, error) {
	if stripeCustomerID == "" {
		return nil, ErrNotFound
	}
	var projectID string
	err := s.db.QueryRowContext(ctx, `
		SELECT project_id FROM projects WHERE stripe_customer_id = ? LIMIT 1
	`, stripeCustomerID).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.GetProject(ctx, projectID)
}

// IncrementExecutionsThisPeriod atomically adds 1 to the counter.
// Best-effort: a failure does not propagate to the ingest path; the
// caller logs and continues.
func (s *SQLiteStore) IncrementExecutionsThisPeriod(
	ctx context.Context, projectID string,
) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET executions_this_period = executions_this_period + 1
		WHERE project_id = ?
	`, projectID)
	if err != nil {
		return fmt.Errorf("increment executions counter: %w", err)
	}
	return nil
}

// ResetExecutionsThisPeriod zeros the counter and updates the period
// bounds. Called on billing-period rollover (invoice.paid webhook or
// lazy reset when handlers notice current_period_end has passed).
func (s *SQLiteStore) ResetExecutionsThisPeriod(
	ctx context.Context, projectID string, periodStart, periodEnd time.Time,
) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET executions_this_period = 0,
		    current_period_start = ?,
		    current_period_end = ?
		WHERE project_id = ?
	`, periodStart.UTC().Unix(), periodEnd.UTC().Unix(), projectID)
	if err != nil {
		return fmt.Errorf("reset executions counter: %w", err)
	}
	return nil
}

// GetDailyExecutionCounts groups executions by UTC date for the
// billing-page usage chart. Date is the calendar day at UTC midnight;
// Count is the number of executions started on that day. Days with
// zero executions are omitted (the dashboard fills gaps client-side).
func (s *SQLiteStore) GetDailyExecutionCounts(
	ctx context.Context, projectID string, since, until time.Time,
) ([]DailyExecutionCount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
		    date(started_at) AS day,
		    COUNT(*) AS n
		FROM executions
		WHERE project_id = ?
		  AND started_at >= ?
		  AND started_at <  ?
		GROUP BY day
		ORDER BY day ASC
	`, projectID, since.UTC(), until.UTC()) // time.Time bounds, NEVER Format(...): formats can't compare
	if err != nil {
		return nil, fmt.Errorf("query daily execution counts: %w", err)
	}
	defer rows.Close()

	var out []DailyExecutionCount
	for rows.Next() {
		var dayStr string
		var n int64
		if err := rows.Scan(&dayStr, &n); err != nil {
			return nil, fmt.Errorf("scan daily count: %w", err)
		}
		t, err := time.Parse("2006-01-02", dayStr)
		if err != nil {
			return nil, fmt.Errorf("parse day %q: %w", dayStr, err)
		}
		out = append(out, DailyExecutionCount{Date: t.UTC(), Count: n})
	}
	return out, rows.Err()
}

// DeleteProjectCascade hard-deletes a project and every row whose
// existence depends on it. Wired up for the customer-facing "Close
// account" flow on /app/settings. Runs everything in a single
// transaction so a partial-delete state is impossible. Since the v0.1
// schema declares FK ON DELETE CASCADE on most child tables, we COULD
// just delete from projects, but only when SQLite is opened with
// foreign_keys=on; for safety we issue explicit deletes in the
// FK-respecting order so the function is correct regardless of the
// pragma state.
func (s *SQLiteStore) DeleteProjectCascade(
	ctx context.Context,
	projectID string,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin cascade delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Order: deepest children first. Best-effort on each statement:
	// a missing table (e.g. an optional feature wasn't migrated on
	// this deployment) is fine; we ignore "no such table" but propagate
	// other errors. The lazy "DELETE FROM X WHERE project_id = ?"
	// pattern handles both empty and populated tables uniformly.
	// Real table names verified against migrations/* (fix: prior
	// version used "webhooks", "class_severities", "project_settings",
	// "project_retention" - none of which exist). Retention is a column
	// on projects (migration 012, 020), not its own table.
	stmts := []string{
		`DELETE FROM webhook_deliveries WHERE project_id = ?`,
		`DELETE FROM project_webhooks WHERE project_id = ?`,
		`DELETE FROM project_class_severities WHERE project_id = ?`,
		`DELETE FROM abuse_signals WHERE project_id = ?`,
		`DELETE FROM events WHERE project_id = ?`,
		`DELETE FROM executions WHERE project_id = ?`,
		`DELETE FROM failure_groups WHERE project_id = ?`,
		`DELETE FROM api_keys WHERE project_id = ?`,
		`DELETE FROM organization_members WHERE org_id IN (SELECT org_id FROM organizations WHERE created_by_user_id IN (SELECT owner_user_id FROM projects WHERE project_id = ?))`,
		`DELETE FROM organization_invites WHERE org_id IN (SELECT org_id FROM organizations WHERE created_by_user_id IN (SELECT owner_user_id FROM projects WHERE project_id = ?))`,
		`DELETE FROM organizations WHERE created_by_user_id IN (SELECT owner_user_id FROM projects WHERE project_id = ?)`,
	}
	for _, q := range stmts {
		if _, qerr := tx.ExecContext(ctx, q, projectID); qerr != nil {
			// SQLite reports missing tables as "no such table: X". Ignore
			// these since not every deployment migrates every optional
			// surface (e.g. abuse_signals was added later).
			if !strings.Contains(qerr.Error(), "no such table") {
				// Truncate query for the error message defensively
				// (prior version did `q[:60]` which panicked on
				// queries shorter than 60 chars).
				preview := q
				if len(preview) > 80 {
					preview = preview[:80] + "..."
				}
				return fmt.Errorf("cascade delete (%s): %w", preview, qerr)
			}
		}
	}
	// Finally the project row itself.
	res, err := tx.ExecContext(ctx,
		`DELETE FROM projects WHERE project_id = ?`, projectID)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit cascade delete: %w", err)
	}
	return nil
}

// UpdateProjectBillingCap sets projects.billing_cap_usd. Called from
// HandleUpdateBillingCap to honor the customer's overage spend cap
// . 0 is allowed and means "no project-level override; fall
// back to the constants default that the hobby billing scheduler
// applies elsewhere."
func (s *SQLiteStore) UpdateProjectBillingCap(
	ctx context.Context,
	projectID string,
	capUSD float64,
) error {
	res, err := s.db.ExecContext(
		ctx,
		`UPDATE projects SET billing_cap_usd = ? WHERE project_id = ?`,
		capUSD, projectID,
	)
	if err != nil {
		return fmt.Errorf("update project billing_cap_usd: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
