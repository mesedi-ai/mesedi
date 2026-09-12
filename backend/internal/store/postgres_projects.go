// Project lifecycle, billing and usage-counter persistence.
// Split out of postgres.go on 2026-09-12; every declaration moved verbatim.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *PostgresStore) CreateProject(ctx context.Context, p *Project) error {
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	if p.Tier == "" {
		p.Tier = "hobby"
	}
	// hotfix: see sqlite.go for the explicit card_on_file=FALSE
	// rationale.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (
			project_id, name, owner_user_id, owner_email, created_at, tier,
			card_on_file
		)
		VALUES ($1, $2, $3, $4, $5, $6, FALSE)
	`, p.ProjectID, p.Name, nullString(p.OwnerUserID), nullString(p.OwnerEmail), p.CreatedAt, p.Tier)
	if err != nil {
		return fmt.Errorf("insert project (postgres): %w", err)
	}
	return nil
}

func (s *PostgresStore) GetProject(ctx context.Context, projectID string) (*Project, error) {
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
		FROM projects WHERE project_id = $1
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

// GetMostRecentProjectByOwnerEmail is the Postgres counterpart to
// SQLiteStore.GetMostRecentProjectByOwnerEmail. See store.go for the
// contract -- used by /signin after SSO/magic-link proves email
// ownership.
func (s *PostgresStore) GetMostRecentProjectByOwnerEmail(ctx context.Context, email string) (*Project, error) {
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
		WHERE LOWER(owner_email) = LOWER($1)
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

// ListProjectsByOwner is the Postgres counterpart to
// SQLiteStore.ListProjectsByOwner. See that method's doc comment for
// the contract and the tenant-model rationale.
func (s *PostgresStore) ListProjectsByOwner(ctx context.Context, ownerUserID string) ([]*Project, error) {
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
		WHERE owner_user_id = $1
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

func (s *PostgresStore) GetProjectStorageStats(ctx context.Context) ([]*ProjectStorage, error) {
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
				SELECT SUM(COALESCE(LENGTH(e.input_summary),0) +
				           COALESCE(LENGTH(e.output_summary),0) +
				           COALESCE(LENGTH(e.crash_signature),0))
				FROM executions e WHERE e.project_id = p.project_id
			), 0) +
			COALESCE((
				SELECT SUM(COALESCE(LENGTH(ev.payload),0))
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
	return out, rows.Err()
}

func (s *PostgresStore) DeleteProject(ctx context.Context, projectID string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE project_id = $1`, projectID)
	if err != nil {
		return fmt.Errorf("delete project (postgres): %w", err)
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

// DeleteFailureGroupsByProject wipes every failure_group for a single
// project and returns the number of rows deleted. It is non-failing
// when the count is zero (caller may wipe an already-empty project).
// Used by the admin reset endpoint, see store.go interface docs.
func (s *PostgresStore) DeleteFailureGroupsByProject(ctx context.Context, projectID string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM failure_groups WHERE project_id = $1`, projectID)
	if err != nil {
		return 0, fmt.Errorf("delete failure_groups by project (postgres): %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

func (s *PostgresStore) ListAllProjects(ctx context.Context) ([]*AdminProjectRow, error) {
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
		WHERE p.project_id != $1
		GROUP BY p.project_id, p.name, p.owner_email, p.created_at,
		         p.tier, p.stripe_customer_id, p.stripe_subscription_id,
		         p.current_period_start, p.current_period_end,
		         p.executions_this_period, p.granted_executions,
		         p.granted_executions_expires_at, p.tier_expires_at
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
			lastActivity                 sql.NullTime
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
		if lastActivity.Valid {
			t := lastActivity.Time.UTC()
			row.LastActivityAt = &t
		}
		out = append(out, &row)
	}
	return out, rows.Err()
}

func (s *PostgresStore) UpdateProjectTier(ctx context.Context, projectID, tier string, expiresAt *time.Time) error {
	var expires sql.NullInt64
	if expiresAt != nil {
		expires = sql.NullInt64{Int64: expiresAt.Unix(), Valid: true}
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET tier = $1, tier_expires_at = $2
		WHERE project_id = $3
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

func (s *PostgresStore) UpdateProjectName(ctx context.Context, projectID, name string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET name = $1
		WHERE project_id = $2
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

func (s *PostgresStore) AddGrantedExecutions(ctx context.Context, projectID string, delta int64, expiresAt *time.Time) error {
	var expires sql.NullInt64
	if expiresAt != nil {
		expires = sql.NullInt64{Int64: expiresAt.Unix(), Valid: true}
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET granted_executions = granted_executions + $1,
		    granted_executions_expires_at = $2
		WHERE project_id = $3
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

func (s *PostgresStore) UpdateProjectBilling(
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
		SET tier = $1,
		    stripe_customer_id = $2,
		    stripe_subscription_id = $3,
		    current_period_start = $4,
		    current_period_end = $5
		WHERE project_id = $6
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

func (s *PostgresStore) GetProjectByStripeCustomerID(ctx context.Context, stripeCustomerID string) (*Project, error) {
	if stripeCustomerID == "" {
		return nil, ErrNotFound
	}
	var projectID string
	err := s.db.QueryRowContext(ctx,
		`SELECT project_id FROM projects WHERE stripe_customer_id = $1 LIMIT 1`,
		stripeCustomerID,
	).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.GetProject(ctx, projectID)
}

func (s *PostgresStore) IncrementExecutionsThisPeriod(ctx context.Context, projectID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET executions_this_period = executions_this_period + 1
		WHERE project_id = $1
	`, projectID)
	if err != nil {
		return fmt.Errorf("increment executions counter: %w", err)
	}
	return nil
}

func (s *PostgresStore) ResetExecutionsThisPeriod(ctx context.Context, projectID string, periodStart, periodEnd time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET executions_this_period = 0,
		    current_period_start = $1,
		    current_period_end = $2
		WHERE project_id = $3
	`, periodStart.UTC().Unix(), periodEnd.UTC().Unix(), projectID)
	if err != nil {
		return fmt.Errorf("reset executions counter: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetDailyExecutionCounts(ctx context.Context, projectID string, since, until time.Time) ([]DailyExecutionCount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
		    (started_at AT TIME ZONE 'UTC')::date AS day,
		    COUNT(*) AS n
		FROM executions
		WHERE project_id = $1
		  AND started_at >= $2
		  AND started_at <  $3
		GROUP BY day
		ORDER BY day ASC
	`, projectID, since.UTC(), until.UTC())
	if err != nil {
		return nil, fmt.Errorf("query daily execution counts: %w", err)
	}
	defer rows.Close()

	var out []DailyExecutionCount
	for rows.Next() {
		var day time.Time
		var n int64
		if err := rows.Scan(&day, &n); err != nil {
			return nil, fmt.Errorf("scan daily count: %w", err)
		}
		out = append(out, DailyExecutionCount{Date: day.UTC(), Count: n})
	}
	return out, rows.Err()
}

// DeleteProjectCascade hard-deletes a project and every dependent row
// in one transaction. Postgres counterpart to the SQLiteStore method
// .
func (s *PostgresStore) DeleteProjectCascade(
	ctx context.Context,
	projectID string,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin cascade delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmts := []string{
		`DELETE FROM webhook_deliveries WHERE project_id = $1`,
		`DELETE FROM project_webhooks WHERE project_id = $1`,
		`DELETE FROM project_class_severities WHERE project_id = $1`,
		`DELETE FROM abuse_signals WHERE project_id = $1`,
		`DELETE FROM events WHERE project_id = $1`,
		`DELETE FROM executions WHERE project_id = $1`,
		`DELETE FROM failure_groups WHERE project_id = $1`,
		`DELETE FROM api_keys WHERE project_id = $1`,
		`DELETE FROM organization_members WHERE org_id IN (SELECT org_id FROM organizations WHERE created_by_user_id IN (SELECT owner_user_id FROM projects WHERE project_id = $1))`,
		`DELETE FROM organization_invites WHERE org_id IN (SELECT org_id FROM organizations WHERE created_by_user_id IN (SELECT owner_user_id FROM projects WHERE project_id = $1))`,
		`DELETE FROM organizations WHERE created_by_user_id IN (SELECT owner_user_id FROM projects WHERE project_id = $1)`,
	}
	// Postgres aborts the entire transaction on ANY query error
	// (SQLSTATE 25P02): subsequent queries all return "current
	// transaction is aborted" until ROLLBACK. SQLite is more
	// forgiving and continues after a per-statement error. To make
	// the "ignore missing table" path work on Postgres, we wrap each
	// statement in a SAVEPOINT so a missing-table error rolls back
	// only that one statement, not the whole cascade. Real errors
	// still abort everything.
	for i, q := range stmts {
		spName := fmt.Sprintf("cascade_sp_%d", i)
		if _, spErr := tx.ExecContext(ctx, "SAVEPOINT "+spName); spErr != nil {
			return fmt.Errorf("savepoint create: %w", spErr)
		}
		_, qerr := tx.ExecContext(ctx, q, projectID)
		if qerr != nil {
			msg := qerr.Error()
			if strings.Contains(msg, "does not exist") ||
				strings.Contains(msg, "42P01") {
				// Missing relation: roll back this savepoint and move on.
				if _, rbErr := tx.ExecContext(ctx,
					"ROLLBACK TO SAVEPOINT "+spName); rbErr != nil {
					return fmt.Errorf("savepoint rollback: %w", rbErr)
				}
				continue
			}
			// Real error: propagate. The whole outer transaction will
			// roll back via the deferred Rollback above.
			preview := q
			if len(preview) > 80 {
				preview = preview[:80] + "..."
			}
			return fmt.Errorf("cascade delete (%s): %w", preview, qerr)
		}
		// Statement succeeded; release the savepoint to free server
		// resources (no-op semantically).
		if _, relErr := tx.ExecContext(ctx,
			"RELEASE SAVEPOINT "+spName); relErr != nil {
			return fmt.Errorf("savepoint release: %w", relErr)
		}
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM projects WHERE project_id = $1`, projectID)
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

// UpdateProjectBillingCap sets projects.billing_cap_usd. Postgres
// counterpart to the SQLiteStore method.
func (s *PostgresStore) UpdateProjectBillingCap(
	ctx context.Context,
	projectID string,
	capUSD float64,
) error {
	res, err := s.db.ExecContext(
		ctx,
		`UPDATE projects SET billing_cap_usd = $1 WHERE project_id = $2`,
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
