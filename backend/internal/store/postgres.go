// Postgres implementation of the Store interface.
//
// Phase 2 (this file as shipped, ): all 50 Store methods ported.
// Mirrors sqlite.go method-for-method with the SQL translated to
// Postgres dialect ($N placeholders instead of ?, ON CONFLICT instead
// of OR IGNORE/REPLACE, jsonb->>'key' instead of json_extract, real
// BOOLEAN instead of INTEGER 0/1, TIMESTAMPTZ instead of TEXT for
// columns the postgres migrations promoted).
//
// Driver: github.com/jackc/pgx/v5/stdlib, the modern pure-Go,
// database/sql-compatible Postgres driver. Registered under the name
// "pgx".
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx" driver

	"mesedi/backend/internal/events"
)

// ErrPostgresNotYetPorted is retained as a documented sentinel even
// though all Store methods are now ported. Keeps the symbol available
// for any future Store-interface additions whose Postgres
// implementation lags by a session or two.
var ErrPostgresNotYetPorted = errors.New(
	"postgres: this Store method has not yet been ported. Run against " +
		"SQLite (unset MESEDI_DB_URL_POSTGRES) until the port lands.",
)

// PostgresStore is the Postgres-backed Store implementation. Safe for
// concurrent use; the underlying *sql.DB handles connection pooling.
type PostgresStore struct {
	db     *sql.DB
	logger *slog.Logger
}

// OpenPostgres opens a Postgres connection at the given DSN and runs
// all pending migrations from the embedded migrations-postgres/
// directory. Neon DSNs include sslmode=require natively.
func OpenPostgres(dsn string, logger *slog.Logger) (*PostgresStore, error) {
	if dsn == "" {
		return nil, fmt.Errorf("postgres dsn is empty")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	s := &PostgresStore{db: db, logger: logger}
	if err := s.applyMigrations(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply postgres migrations: %w", err)
	}
	logger.Info("postgres store ready", "driver", "pgx")
	return s, nil
}

// Close releases the underlying connection pool. Idempotent.
func (s *PostgresStore) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Ping verifies the database is reachable. Used by /health.
func (s *PostgresStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// applyMigrations runs every embedded migrations-postgres/*.sql file in
// lexical order. Already-applied migrations are skipped via the shared
// schema_migrations.version counter.
func (s *PostgresStore) applyMigrations(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`); err != nil {
		return fmt.Errorf("bootstrap schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationsPostgresFS, "migrations-postgres")
	if err != nil {
		return fmt.Errorf("read migrations-postgres dir: %w", err)
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, name := range files {
		version, ok := parseMigrationVersion(name)
		if !ok {
			s.logger.Warn("skipping postgres migration with unparseable name", "file", name)
			continue
		}

		var existing int
		err := s.db.QueryRowContext(ctx,
			"SELECT version FROM schema_migrations WHERE version = $1", version,
		).Scan(&existing)
		if err == nil {
			s.logger.Debug("postgres migration already applied", "migration_version", version, "file", name)
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check postgres migration %d: %w", version, err)
		}

		body, err := fs.ReadFile(migrationsPostgresFS, path.Join("migrations-postgres", name))
		if err != nil {
			return fmt.Errorf("read postgres migration %s: %w", name, err)
		}
		statements := splitSQLStatements(string(body))
		for stmtIdx, stmt := range statements {
			if _, err := s.db.ExecContext(ctx, stmt); err != nil {
				errMsg := strings.ToLower(err.Error())
				isIdempotencyErr := strings.Contains(errMsg, "already exists") ||
					strings.Contains(errMsg, "duplicate") ||
					strings.Contains(errMsg, "42p07") ||
					strings.Contains(errMsg, "42701")
				if !isIdempotencyErr {
					return fmt.Errorf("apply postgres migration %s statement %d: %w", name, stmtIdx+1, err)
				}
				s.logger.Warn("postgres migration statement produced idempotency error, treating as already-applied",
					"migration_version", version, "file", name, "statement_index", stmtIdx+1, "error", err.Error())
			}
		}
		s.logger.Info("postgres migration applied", "migration_version", version, "file", name)

		if _, err := s.db.ExecContext(ctx,
			"INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT (version) DO NOTHING",
			version); err != nil {
			return fmt.Errorf("record postgres migration %d: %w", version, err)
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────
// Project + API key operations
// ─────────────────────────────────────────────────────────────────────────

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

func (s *PostgresStore) CreateAPIKey(ctx context.Context, k *APIKey) error {
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now().UTC()
	}
	scope := k.Scope
	if scope == "" {
		scope = APIKeyScopeCustomer
	}
	source := k.Source
	if source == "" {
		source = APIKeySourceManual
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys (key_id, project_id, key_hash, key_prefix, name, created_at, user_id, scope, expires_at, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, k.KeyID, k.ProjectID, k.KeyHash, k.KeyPrefix, nullString(k.Name), k.CreatedAt, nullString(k.UserID), scope, k.ExpiresAt, source)
	if err != nil {
		return fmt.Errorf("insert api_key: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetAPIKeyByHash(ctx context.Context, keyHash string) (*APIKey, error) {
	k := &APIKey{}
	var name, userID sql.NullString
	var lastUsed sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT key_id, project_id, key_hash, key_prefix, name, created_at, last_used_at, user_id, scope, expires_at, source
		FROM api_keys WHERE key_hash = $1
	`, keyHash).Scan(&k.KeyID, &k.ProjectID, &k.KeyHash, &k.KeyPrefix, &name, &k.CreatedAt, &lastUsed, &userID, &k.Scope, &k.ExpiresAt, &k.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if name.Valid {
		k.Name = name.String
	}
	if userID.Valid {
		k.UserID = userID.String
	}
	if lastUsed.Valid {
		t := lastUsed.Time
		k.LastUsedAt = &t
	}
	return k, nil
}

func (s *PostgresStore) TouchAPIKey(ctx context.Context, keyID string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE api_keys SET last_used_at = $1 WHERE key_id = $2",
		time.Now().UTC(), keyID,
	)
	return err
}

func (s *PostgresStore) ListAPIKeysForProject(ctx context.Context, projectID string) ([]*APIKey, error) {
	// Filter session-grade keys (sso_login, magic_link) out of the
	// customer-facing listing. See sqlite.go counterpart.
	rows, err := s.db.QueryContext(ctx, `
		SELECT key_id, project_id, key_prefix, name, created_at, last_used_at, scope, expires_at, source
		FROM api_keys
		WHERE project_id = $1
		  AND source NOT IN ('sso_login', 'magic_link')
		ORDER BY created_at DESC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("query api_keys: %w", err)
	}
	defer rows.Close()
	return scanPostgresAPIKeyList(rows)
}

// ListAllAPIKeys returns every API key in the system, NEWEST first.
// Admin-only. See sqlite.go counterpart for full docs (session-grade
// keys are filtered out for the same reason there).
func (s *PostgresStore) ListAllAPIKeys(ctx context.Context) ([]*APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT key_id, project_id, key_prefix, name, created_at, last_used_at, scope, expires_at, source
		FROM api_keys
		WHERE source NOT IN ('sso_login', 'magic_link')
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("query api_keys (all): %w", err)
	}
	defer rows.Close()
	return scanPostgresAPIKeyList(rows)
}

// scanPostgresAPIKeyList is the postgres-side counterpart of
// scanAPIKeyList in sqlite.go. Centralized so list-by-project and
// list-all share identical scan semantics.
func scanPostgresAPIKeyList(rows *sql.Rows) ([]*APIKey, error) {
	var out []*APIKey
	for rows.Next() {
		var (
			k          APIKey
			lastUsedAt sql.NullTime
			name       sql.NullString
		)
		if err := rows.Scan(
			&k.KeyID, &k.ProjectID, &k.KeyPrefix,
			&name, &k.CreatedAt, &lastUsedAt, &k.Scope, &k.ExpiresAt, &k.Source,
		); err != nil {
			return nil, err
		}
		if name.Valid {
			k.Name = name.String
		}
		if lastUsedAt.Valid {
			t := lastUsedAt.Time
			k.LastUsedAt = &t
		}
		out = append(out, &k)
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

// DeleteAPIKeysByUserID hard-deletes every API key whose user_id
// matches. Postgres counterpart to the SQLiteStore method; called by
// HandleRemoveMember to revoke a removed team member's credentials
// . Returns the number of rows deleted.
func (s *PostgresStore) DeleteAPIKeysByUserID(
	ctx context.Context,
	userID string,
) (int, error) {
	res, err := s.db.ExecContext(
		ctx,
		`DELETE FROM api_keys WHERE user_id = $1`,
		userID,
	)
	if err != nil {
		return 0, fmt.Errorf("delete api_keys by user_id: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func (s *PostgresStore) DeleteAPIKey(ctx context.Context, keyID, projectID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM api_keys WHERE key_id = $1 AND project_id = $2`,
		keyID, projectID,
	)
	if err != nil {
		return fmt.Errorf("delete api_key: %w", err)
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

// DeleteAPIKeyByID hard-deletes any API key with no project_id guard.
// Admin-only. See sqlite.go counterpart for full docs.
func (s *PostgresStore) DeleteAPIKeyByID(ctx context.Context, keyID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM api_keys WHERE key_id = $1`,
		keyID,
	)
	if err != nil {
		return fmt.Errorf("delete api_key: %w", err)
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

// ─────────────────────────────────────────────────────────────────────────
// Project webhook operations
// ─────────────────────────────────────────────────────────────────────────

func (s *PostgresStore) CreateProjectWebhook(ctx context.Context, wh *ProjectWebhook) error {
	if wh.WebhookID == "" {
		return fmt.Errorf("webhook_id required")
	}
	if wh.ProjectID == "" {
		return fmt.Errorf("project_id required")
	}
	if wh.URL == "" {
		return fmt.Errorf("url required")
	}
	if wh.Secret == "" {
		return fmt.Errorf("secret required")
	}
	if wh.CreatedAt.IsZero() {
		wh.CreatedAt = time.Now().UTC()
	}

	var classesJSON sql.NullString
	if len(wh.EnabledClasses) > 0 {
		b, err := json.Marshal(wh.EnabledClasses)
		if err != nil {
			return fmt.Errorf("marshal enabled_classes: %w", err)
		}
		classesJSON = sql.NullString{String: string(b), Valid: true}
	}

	recurrenceMode := wh.RecurrenceMode
	if recurrenceMode == "" {
		recurrenceMode = RecurrenceModeOff
	}
	var windowSeconds sql.NullInt64
	if recurrenceMode == RecurrenceModeThrottled && wh.RecurrenceWindowSeconds > 0 {
		windowSeconds = sql.NullInt64{Int64: int64(wh.RecurrenceWindowSeconds), Valid: true}
	}

	var authTokenNS sql.NullString
	if wh.AuthToken != "" {
		authTokenNS = sql.NullString{String: wh.AuthToken, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO project_webhooks (
			webhook_id, project_id, name, url, secret, auth_token,
			enabled_classes, enabled, created_at, severity_filter,
			recurrence_mode, recurrence_window_seconds
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`,
		wh.WebhookID, wh.ProjectID, wh.Name, wh.URL, wh.Secret, authTokenNS,
		classesJSON, wh.Enabled, wh.CreatedAt.UTC(),
		wh.SeverityFilter,
		recurrenceMode, windowSeconds,
	)
	if err != nil {
		return fmt.Errorf("insert project_webhook: %w", err)
	}
	return nil
}

func (s *PostgresStore) ListProjectWebhooksForProject(ctx context.Context, projectID string) ([]*ProjectWebhook, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT webhook_id, project_id, name, url,
		       enabled_classes, enabled, created_at, severity_filter,
		       recurrence_mode, recurrence_window_seconds
		FROM project_webhooks
		WHERE project_id = $1
		ORDER BY created_at DESC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list project_webhooks: %w", err)
	}
	defer rows.Close()

	out := make([]*ProjectWebhook, 0, 8)
	for rows.Next() {
		var wh ProjectWebhook
		var classesJSON sql.NullString
		var windowSeconds sql.NullInt64
		if err := rows.Scan(
			&wh.WebhookID, &wh.ProjectID, &wh.Name, &wh.URL,
			&classesJSON, &wh.Enabled, &wh.CreatedAt, &wh.SeverityFilter,
			&wh.RecurrenceMode, &windowSeconds,
		); err != nil {
			return nil, fmt.Errorf("scan project_webhook: %w", err)
		}
		wh.EnabledClasses = parseEnabledClasses(classesJSON)
		if windowSeconds.Valid {
			wh.RecurrenceWindowSeconds = int(windowSeconds.Int64)
		}
		out = append(out, &wh)
	}
	return out, rows.Err()
}

func (s *PostgresStore) ListEnabledProjectWebhooks(ctx context.Context, projectID string) ([]*ProjectWebhook, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT webhook_id, project_id, name, url, secret, auth_token,
		       enabled_classes, enabled, created_at, severity_filter,
		       recurrence_mode, recurrence_window_seconds
		FROM project_webhooks
		WHERE project_id = $1 AND enabled = TRUE
		ORDER BY created_at ASC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list enabled project_webhooks: %w", err)
	}
	defer rows.Close()

	out := make([]*ProjectWebhook, 0, 8)
	for rows.Next() {
		var wh ProjectWebhook
		var classesJSON sql.NullString
		var authTokenNS sql.NullString
		var windowSeconds sql.NullInt64
		if err := rows.Scan(
			&wh.WebhookID, &wh.ProjectID, &wh.Name, &wh.URL, &wh.Secret, &authTokenNS,
			&classesJSON, &wh.Enabled, &wh.CreatedAt, &wh.SeverityFilter,
			&wh.RecurrenceMode, &windowSeconds,
		); err != nil {
			return nil, fmt.Errorf("scan project_webhook: %w", err)
		}
		if authTokenNS.Valid {
			wh.AuthToken = authTokenNS.String
		}
		wh.EnabledClasses = parseEnabledClasses(classesJSON)
		if windowSeconds.Valid {
			wh.RecurrenceWindowSeconds = int(windowSeconds.Int64)
		}
		out = append(out, &wh)
	}
	return out, rows.Err()
}

func (s *PostgresStore) DeleteProjectWebhook(ctx context.Context, webhookID, projectID string) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM project_webhooks
		WHERE webhook_id = $1 AND project_id = $2
	`, webhookID, projectID)
	if err != nil {
		return fmt.Errorf("delete project_webhook: %w", err)
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

func (s *PostgresStore) GetProjectWebhook(ctx context.Context, webhookID, projectID string) (*ProjectWebhook, error) {
	var wh ProjectWebhook
	var classesJSON sql.NullString
	var authTokenNS sql.NullString
	var windowSeconds sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT webhook_id, project_id, name, url, secret, auth_token,
		       enabled_classes, enabled, created_at, severity_filter,
		       recurrence_mode, recurrence_window_seconds
		FROM project_webhooks
		WHERE webhook_id = $1 AND project_id = $2
	`, webhookID, projectID).Scan(
		&wh.WebhookID, &wh.ProjectID, &wh.Name, &wh.URL, &wh.Secret, &authTokenNS,
		&classesJSON, &wh.Enabled, &wh.CreatedAt, &wh.SeverityFilter,
		&wh.RecurrenceMode, &windowSeconds,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get project_webhook: %w", err)
	}
	if authTokenNS.Valid {
		wh.AuthToken = authTokenNS.String
	}
	wh.EnabledClasses = parseEnabledClasses(classesJSON)
	if windowSeconds.Valid {
		wh.RecurrenceWindowSeconds = int(windowSeconds.Int64)
	}
	return &wh, nil
}

// GetWebhookRecurrenceLastFired returns when this webhook last fired
// for this failure group. ErrNotFound means "no row yet"; dispatcher
// treats that as "window elapsed."
func (s *PostgresStore) GetWebhookRecurrenceLastFired(
	ctx context.Context,
	webhookID, groupID string,
) (time.Time, error) {
	var t time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT last_fired_at FROM webhook_recurrence_state
		WHERE webhook_id = $1 AND group_id = $2
	`, webhookID, groupID).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("get webhook_recurrence_state: %w", err)
	}
	return t, nil
}

// UpsertWebhookRecurrenceLastFired records or updates the last-fired
// timestamp for (webhook, group).
func (s *PostgresStore) UpsertWebhookRecurrenceLastFired(
	ctx context.Context,
	webhookID, groupID string,
	t time.Time,
) error {
	if webhookID == "" || groupID == "" {
		return fmt.Errorf("webhook_id and group_id required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO webhook_recurrence_state (webhook_id, group_id, last_fired_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (webhook_id, group_id) DO UPDATE SET
			last_fired_at = EXCLUDED.last_fired_at
	`, webhookID, groupID, t.UTC())
	if err != nil {
		return fmt.Errorf("upsert webhook_recurrence_state: %w", err)
	}
	return nil
}

func (s *PostgresStore) RecordWebhookDelivery(ctx context.Context, d *WebhookDelivery) error {
	if d.WebhookID == "" {
		return fmt.Errorf("webhook_id required")
	}
	if d.ProjectID == "" {
		return fmt.Errorf("project_id required")
	}
	if d.Status == "" {
		return fmt.Errorf("status required")
	}
	if d.Attempt <= 0 {
		d.Attempt = 1
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	if d.DeliveryID == "" {
		raw := d.WebhookID + d.CreatedAt.Format(time.RFC3339Nano) +
			fmt.Sprintf("/%d", d.Attempt)
		sum := sha256.Sum256([]byte(raw))
		d.DeliveryID = "del-" + hex.EncodeToString(sum[:8])
	}

	const maxBodyBytes = 2048
	body := d.ResponseBody
	if len(body) > maxBodyBytes {
		body = body[:maxBodyBytes] + "…[truncated]"
	}

	var httpStatus sql.NullInt64
	if d.HTTPStatus != nil {
		httpStatus = sql.NullInt64{Int64: int64(*d.HTTPStatus), Valid: true}
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO webhook_deliveries (
			delivery_id, webhook_id, project_id,
			failure_class, signature, group_id,
			attempt, status, http_status, error, response_body,
			duration_ms, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`,
		d.DeliveryID, d.WebhookID, d.ProjectID,
		nullableString(d.FailureClass), nullableString(d.Signature), nullableString(d.GroupID),
		d.Attempt, d.Status, httpStatus, nullableString(d.Error), nullableString(body),
		d.DurationMs, d.CreatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("insert webhook_delivery: %w", err)
	}
	return nil
}

func (s *PostgresStore) ListDeliveriesForWebhook(ctx context.Context, webhookID string, limit int) ([]*WebhookDelivery, error) {
	// Clamp limit to the package ceiling (alert ). Caller is
	// trusted to pass a sane value but a defensive cap here keeps a
	// future-bug caller from driving an unbounded allocation -- and
	// lets CodeQL see the upper bound at the make() site below.
	if limit <= 0 || limit > WebhookDeliveryListLimitMax {
		limit = WebhookDeliveryListLimitMax
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT delivery_id, webhook_id, project_id,
		       failure_class, signature, group_id,
		       attempt, status, http_status, error, response_body,
		       duration_ms, created_at
		FROM webhook_deliveries
		WHERE webhook_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, webhookID, limit)
	if err != nil {
		return nil, fmt.Errorf("list webhook_deliveries: %w", err)
	}
	defer rows.Close()

	// Capacity hint uses the package-level constant so the upper
	// bound is visible to static analysis; `limit` is guaranteed
	// <= WebhookDeliveryListLimitMax by the clamp at function entry.
	out := make([]*WebhookDelivery, 0, WebhookDeliveryListLimitMax)
	for rows.Next() {
		var d WebhookDelivery
		var failureClass, signature, groupID, errMsg, respBody sql.NullString
		var httpStatus sql.NullInt64
		if err := rows.Scan(
			&d.DeliveryID, &d.WebhookID, &d.ProjectID,
			&failureClass, &signature, &groupID,
			&d.Attempt, &d.Status, &httpStatus, &errMsg, &respBody,
			&d.DurationMs, &d.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan webhook_delivery: %w", err)
		}
		if failureClass.Valid {
			d.FailureClass = failureClass.String
		}
		if signature.Valid {
			d.Signature = signature.String
		}
		if groupID.Valid {
			d.GroupID = groupID.String
		}
		if errMsg.Valid {
			d.Error = errMsg.String
		}
		if respBody.Valid {
			d.ResponseBody = respBody.String
		}
		if httpStatus.Valid {
			v := int(httpStatus.Int64)
			d.HTTPStatus = &v
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

// ─────────────────────────────────────────────────────────────────────────
// Execution operations
// ─────────────────────────────────────────────────────────────────────────

func (s *PostgresStore) CreateExecution(ctx context.Context, e *events.Execution) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO executions (
			execution_id, project_id, parent_execution_id, status,
			started_at, ended_at, duration_ms,
			total_tokens_in, total_tokens_out, estimated_cost_usd,
			input_summary, output_summary, crash_signature,
			sdk_version, sdk_language, tenant_id, api_key_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
	`,
		e.ExecutionID, e.ProjectID, nullStringPtr(e.ParentExecutionID), e.Status,
		e.StartedAt, nullTime(e.EndedAt), nullInt64(e.DurationMs),
		nullInt(e.TotalTokensIn), nullInt(e.TotalTokensOut), nullFloat(e.EstimatedCostUSD),
		nullString(e.InputSummary), nullString(e.OutputSummary), nullString(e.CrashSignature),
		nullString(e.SDKVersion), nullString(e.SDKLanguage), nullStringPtr(e.TenantID),
		nullStringPtr(e.APIKeyID),
	)
	if err != nil {
		return fmt.Errorf("insert execution: %w", err)
	}
	return nil
}

// PauseExecution is the Postgres twin of the SQLite method of the
// same name. .
func (s *PostgresStore) PauseExecution(ctx context.Context, executionID, projectID string, pausedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE executions SET
			status      = 'awaiting_human',
			paused_at   = $1,
			pause_count = pause_count + 1
		WHERE execution_id = $2
		  AND project_id   = $3
		  AND status       = 'started'
	`, pausedAt, executionID, projectID)
	if err != nil {
		return fmt.Errorf("pause execution: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		var status sql.NullString
		qErr := s.db.QueryRowContext(ctx, `
			SELECT status FROM executions
			WHERE execution_id = $1 AND project_id = $2
		`, executionID, projectID).Scan(&status)
		if errors.Is(qErr, sql.ErrNoRows) {
			return ErrNotFound
		}
		if qErr != nil {
			return fmt.Errorf("pause execution status probe: %w", qErr)
		}
		return ErrInvalidLifecycleTransition
	}
	return nil
}

// ResumeExecution is the Postgres twin of the SQLite method of the
// same name. .
func (s *PostgresStore) ResumeExecution(ctx context.Context, executionID, projectID string, resumedAt time.Time) error {
	var pausedAt sql.NullTime
	var status sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT paused_at, status FROM executions
		WHERE execution_id = $1 AND project_id = $2
	`, executionID, projectID).Scan(&pausedAt, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("resume execution status probe: %w", err)
	}
	if status.String != "awaiting_human" || !pausedAt.Valid {
		return ErrInvalidLifecycleTransition
	}
	deltaMs := resumedAt.Sub(pausedAt.Time).Milliseconds()
	if deltaMs < 0 {
		deltaMs = 0
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE executions SET
			status          = 'started',
			paused_at       = NULL,
			total_paused_ms = total_paused_ms + $1
		WHERE execution_id = $2
		  AND project_id   = $3
		  AND status       = 'awaiting_human'
	`, deltaMs, executionID, projectID)
	if err != nil {
		return fmt.Errorf("resume execution: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrInvalidLifecycleTransition
	}
	return nil
}

func (s *PostgresStore) UpdateExecution(ctx context.Context, e *events.Execution) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE executions SET
			status              = COALESCE(NULLIF($1, ''), status),
			ended_at            = COALESCE($2, ended_at),
			duration_ms         = COALESCE($3, duration_ms),
			total_tokens_in     = COALESCE($4, total_tokens_in),
			total_tokens_out    = COALESCE($5, total_tokens_out),
			estimated_cost_usd  = COALESCE($6, estimated_cost_usd),
			output_summary      = COALESCE(NULLIF($7, ''), output_summary),
			crash_signature     = COALESCE(NULLIF($8, ''), crash_signature)
		WHERE execution_id = $9
	`,
		string(e.Status), nullTime(e.EndedAt), nullInt64(e.DurationMs),
		nullInt(e.TotalTokensIn), nullInt(e.TotalTokensOut), nullFloat(e.EstimatedCostUSD),
		e.OutputSummary, e.CrashSignature,
		e.ExecutionID,
	)
	if err != nil {
		return fmt.Errorf("update execution: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) GetExecution(ctx context.Context, executionID string) (*events.Execution, error) {
	e := &events.Execution{}
	var parent, inputSum, outputSum, crashSig, sdkVer, sdkLang, failureGroupID, tenantID sql.NullString
	var endedAt, pausedAt sql.NullTime
	var durationMs, tokensIn, tokensOut, totalPausedMs sql.NullInt64
	var pauseCount sql.NullInt64
	var costUSD sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `
		SELECT
			execution_id, project_id, parent_execution_id, status,
			started_at, ended_at, duration_ms,
			total_tokens_in, total_tokens_out, estimated_cost_usd,
			input_summary, output_summary, crash_signature,
			sdk_version, sdk_language, failure_group_id, tenant_id,
			paused_at, total_paused_ms, pause_count
		FROM executions WHERE execution_id = $1
	`, executionID).Scan(
		&e.ExecutionID, &e.ProjectID, &parent, &e.Status,
		&e.StartedAt, &endedAt, &durationMs,
		&tokensIn, &tokensOut, &costUSD,
		&inputSum, &outputSum, &crashSig,
		&sdkVer, &sdkLang, &failureGroupID, &tenantID,
		&pausedAt, &totalPausedMs, &pauseCount,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if parent.Valid {
		v := parent.String
		e.ParentExecutionID = &v
	}
	if endedAt.Valid {
		t := endedAt.Time
		e.EndedAt = &t
	}
	if durationMs.Valid {
		e.DurationMs = durationMs.Int64
	}
	if tokensIn.Valid {
		e.TotalTokensIn = int(tokensIn.Int64)
	}
	if tokensOut.Valid {
		e.TotalTokensOut = int(tokensOut.Int64)
	}
	if costUSD.Valid {
		e.EstimatedCostUSD = costUSD.Float64
	}
	if inputSum.Valid {
		e.InputSummary = inputSum.String
	}
	if outputSum.Valid {
		e.OutputSummary = outputSum.String
	}
	if crashSig.Valid {
		e.CrashSignature = crashSig.String
	}
	if sdkVer.Valid {
		e.SDKVersion = sdkVer.String
	}
	if sdkLang.Valid {
		e.SDKLanguage = sdkLang.String
	}
	if failureGroupID.Valid {
		v := failureGroupID.String
		e.FailureGroupID = &v
	}
	if tenantID.Valid {
		v := tenantID.String
		e.TenantID = &v
	}
	if pausedAt.Valid {
		t := pausedAt.Time
		e.PausedAt = &t
	}
	if totalPausedMs.Valid {
		e.TotalPausedMs = totalPausedMs.Int64
	}
	if pauseCount.Valid {
		e.PauseCount = int(pauseCount.Int64)
	}
	return e, nil
}

func (s *PostgresStore) SaveEvents(ctx context.Context, batch []events.Event) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO events (event_id, execution_id, event_type, sequence, timestamp, duration_ms, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`)
	if err != nil {
		return fmt.Errorf("prepare event insert: %w", err)
	}
	defer stmt.Close()

	for i := range batch {
		evt := &batch[i]
		payload := []byte(evt.Payload)
		if len(payload) == 0 {
			payload = []byte("null")
		}
		if !json.Valid(payload) {
			return fmt.Errorf("event %s: invalid JSON payload", evt.EventID)
		}
		if _, err := stmt.ExecContext(ctx,
			evt.EventID, evt.ExecutionID, evt.EventType, evt.Sequence,
			evt.Timestamp, nullInt64(evt.DurationMs), string(payload),
		); err != nil {
			return fmt.Errorf("insert event %s: %w", evt.EventID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit events: %w", err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────
// Read-side executions queries (postgres-specific scan helpers because
// started_at / ended_at are TIMESTAMPTZ in postgres, not RFC3339 text
// like in SQLite — scanning into time.Time directly is correct here.)
// ─────────────────────────────────────────────────────────────────────────

func (s *PostgresStore) ListExecutions(ctx context.Context, projectID string, q string, limit, offset int) ([]*events.Execution, error) {
	// Search filter (list-search-paginate wave): when q is non-empty,
	// restrict to rows whose execution_id OR crash_signature ILIKE q.
	// Postgres' ILIKE is case-insensitive by definition (vs SQLite's
	// LOWER(col) LIKE LOWER(?)) — twin behavior, different SQL.
	args := []any{projectID}
	whereClause := "project_id = $1"
	if q != "" {
		whereClause += " AND (execution_id ILIKE '%' || $2 || '%'" +
			" OR crash_signature ILIKE '%' || $2 || '%')"
		args = append(args, q)
	}
	args = append(args, limit, offset)
	limitPlaceholder := fmt.Sprintf("$%d", len(args)-1)
	offsetPlaceholder := fmt.Sprintf("$%d", len(args))

	// G202: whereClause is built above from allowlisted fragments
	// (project_id / status / crash_signature ILIKE with parameter
	// placeholders). limitPlaceholder / offsetPlaceholder are
	// server-controlled $N tokens, never user input. All user-supplied
	// values flow through args... as parameterized placeholders.
	//nolint:gosec // G202: false positive — see comment above.
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			execution_id, project_id, status,
			started_at, ended_at,
			duration_ms, total_tokens_in, total_tokens_out,
			estimated_cost_usd, sdk_language, sdk_version, crash_signature
		FROM executions
		WHERE `+whereClause+`
		ORDER BY started_at DESC
		LIMIT `+limitPlaceholder+` OFFSET `+offsetPlaceholder+`
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("query executions: %w", err)
	}
	defer rows.Close()
	return scanExecutionRowsPg(rows)
}

// ListActiveExecutionsByProject is the Postgres counterpart to
// SQLiteStore.ListActiveExecutionsByProject. See that method's doc
// comment for contract.
func (s *PostgresStore) ListActiveExecutionsByProject(ctx context.Context, projectID string) ([]*events.Execution, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			execution_id, project_id, status,
			started_at, ended_at,
			duration_ms, total_tokens_in, total_tokens_out,
			estimated_cost_usd, sdk_language, sdk_version, crash_signature
		FROM executions
		WHERE project_id = $1 AND status = $2
		ORDER BY started_at DESC
	`, projectID, string(events.StatusStarted))
	if err != nil {
		return nil, fmt.Errorf("query active executions: %w", err)
	}
	defer rows.Close()
	return scanExecutionRowsPg(rows)
}

func (s *PostgresStore) ListExecutionsByFailureGroup(ctx context.Context, groupID string, limit, offset int) ([]*events.Execution, error) {
	// JOIN through execution_failure_groups so we surface executions
	// whose PRIMARY classification went to a different group but
	// where this group was a SECONDARY classification. See sqlite.go
	// counterpart and migration 039 for the rationale.
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			e.execution_id, e.project_id, e.status,
			e.started_at, e.ended_at,
			e.duration_ms, e.total_tokens_in, e.total_tokens_out,
			e.estimated_cost_usd, e.sdk_language, e.sdk_version, e.crash_signature
		FROM executions e
		INNER JOIN execution_failure_groups efg
			ON efg.execution_id = e.execution_id
		WHERE efg.group_id = $1
		ORDER BY e.started_at DESC
		LIMIT $2 OFFSET $3
	`, groupID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query executions by failure_group: %w", err)
	}
	defer rows.Close()
	return scanExecutionRowsPg(rows)
}

// scanExecutionRowsPg is the postgres counterpart to scanExecutionRows.
// The only difference: started_at / ended_at come back as time.Time
// directly (TIMESTAMPTZ columns), not as RFC3339 strings.
func scanExecutionRowsPg(rows *sql.Rows) ([]*events.Execution, error) {
	var out []*events.Execution
	for rows.Next() {
		var (
			e          events.Execution
			endedAt    sql.NullTime
			durationMs sql.NullInt64
			tokensIn   sql.NullInt64
			tokensOut  sql.NullInt64
			costUSD    sql.NullFloat64
			sdkLang    sql.NullString
			sdkVer     sql.NullString
			crashSig   sql.NullString
		)
		if err := rows.Scan(
			&e.ExecutionID, &e.ProjectID, &e.Status,
			&e.StartedAt, &endedAt,
			&durationMs, &tokensIn, &tokensOut,
			&costUSD, &sdkLang, &sdkVer, &crashSig,
		); err != nil {
			return nil, err
		}
		if endedAt.Valid {
			t := endedAt.Time
			e.EndedAt = &t
		}
		if durationMs.Valid {
			e.DurationMs = durationMs.Int64
		}
		if tokensIn.Valid {
			e.TotalTokensIn = int(tokensIn.Int64)
		}
		if tokensOut.Valid {
			e.TotalTokensOut = int(tokensOut.Int64)
		}
		if costUSD.Valid {
			e.EstimatedCostUSD = costUSD.Float64
		}
		if sdkLang.Valid {
			e.SDKLanguage = sdkLang.String
		}
		if sdkVer.Valid {
			e.SDKVersion = sdkVer.String
		}
		if crashSig.Valid {
			e.CrashSignature = crashSig.String
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *PostgresStore) ListEventsForExecution(ctx context.Context, executionID string) ([]*events.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			event_id, execution_id, event_type, sequence,
			timestamp, duration_ms, payload
		FROM events
		WHERE execution_id = $1
		ORDER BY sequence ASC
	`, executionID)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	var out []*events.Event
	for rows.Next() {
		var (
			e            events.Event
			durationMs   sql.NullInt64
			payloadBytes []byte
		)
		if err := rows.Scan(
			&e.EventID, &e.ExecutionID, &e.EventType, &e.Sequence,
			&e.Timestamp, &durationMs, &payloadBytes,
		); err != nil {
			return nil, err
		}
		if durationMs.Valid {
			e.DurationMs = durationMs.Int64
		}
		if len(payloadBytes) > 0 {
			e.Payload = json.RawMessage(payloadBytes)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *PostgresStore) CountExecutionsByStatusSince(ctx context.Context, projectID, status string, cutoff time.Time) (int, error) {
	query := "SELECT COUNT(*) FROM executions WHERE project_id = $1"
	args := []any{projectID}
	placeholderIdx := 2

	if status != "" {
		query += fmt.Sprintf(" AND status = $%d", placeholderIdx)
		args = append(args, status)
		placeholderIdx++
	}
	if !cutoff.IsZero() {
		query += fmt.Sprintf(" AND started_at >= $%d", placeholderIdx)
		args = append(args, cutoff.UTC())
	}

	var n int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count executions: %w", err)
	}
	return n, nil
}

// SumExecutionCostByProjectSince is the Postgres counterpart to
// SQLiteStore.SumExecutionCostByProjectSince. See that method's doc
// comment for contract.
func (s *PostgresStore) SumExecutionCostByProjectSince(
	ctx context.Context,
	projectID string,
	since time.Time,
) (float64, int, error) {
	query := "SELECT COALESCE(SUM(estimated_cost_usd), 0), COUNT(*) FROM executions WHERE project_id = $1"
	args := []any{projectID}
	if !since.IsZero() {
		query += " AND started_at >= $2"
		args = append(args, since.UTC())
	}
	var cost float64
	var count int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&cost, &count); err != nil {
		return 0, 0, fmt.Errorf("sum execution cost: %w", err)
	}
	return cost, count, nil
}

// GetCostByTenant is the Postgres counterpart to
// SQLiteStore.GetCostByTenant. See that method's doc comment for the
// contract. Uses $N placeholder syntax for parameters; the placeholder
// indices are assembled dynamically because the time-window bounds
// are optional.
func (s *PostgresStore) GetCostByTenant(
	ctx context.Context,
	projectID string,
	since time.Time,
	until time.Time,
	limit int,
) ([]TenantCostRow, error) {
	query := `
		SELECT
			COALESCE(tenant_id, '') AS tenant_id,
			COALESCE(SUM(estimated_cost_usd), 0) AS total_cost_usd,
			COUNT(*) AS execution_count,
			COALESCE(SUM(total_tokens_in), 0) AS total_tokens_in,
			COALESCE(SUM(total_tokens_out), 0) AS total_tokens_out
		FROM executions
		WHERE project_id = $1
	`
	args := []any{projectID}
	next := 2
	if !since.IsZero() {
		query += fmt.Sprintf(" AND started_at >= $%d", next)
		args = append(args, since.UTC())
		next++
	}
	if !until.IsZero() {
		query += fmt.Sprintf(" AND started_at < $%d", next)
		args = append(args, until.UTC())
		next++
	}
	query += `
		GROUP BY COALESCE(tenant_id, '')
		ORDER BY total_cost_usd DESC, execution_count DESC
	`
	if limit > 0 {
		// G202 false positive: $%d expands to a Postgres parameter
		// PLACEHOLDER index (e.g. "$4"), not the actual value. The
		// real `limit` int is bound through args below, the standard
		// safe-parameterized-query pattern.
		query += fmt.Sprintf(" LIMIT $%d", next) //nolint:gosec // G202: $N is a placeholder index, value is parameterized
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get cost by tenant: %w", err)
	}
	defer rows.Close()

	out := []TenantCostRow{}
	for rows.Next() {
		var r TenantCostRow
		if err := rows.Scan(
			&r.TenantID,
			&r.TotalCostUSD,
			&r.ExecutionCount,
			&r.TotalTokensIn,
			&r.TotalTokensOut,
		); err != nil {
			return nil, fmt.Errorf("scan tenant cost row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tenant cost rows: %w", err)
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────
// Failure groups
// ─────────────────────────────────────────────────────────────────────────

// groupExecutionInternalPg is the postgres counterpart to
// SQLiteStore.groupExecutionInternal. Same idempotency contract, same
// isNew semantics, but uses $N placeholders.
func (s *PostgresStore) groupExecutionInternalPg(
	ctx context.Context,
	executionID, projectID, failureClass, signature string,
) (isNew bool, err error) {
	// Postgres twin of sqlite.go groupExecutionInternal. The order
	// of operations is CRITICAL: failure_groups must be upserted
	// BEFORE the link insert because the link table has a foreign
	// key on group_id, and Postgres enforces FKs by default. The
	// original migration-039 commit had the order reversed; SQLite
	// tolerated it (FKs default off), Postgres surfaced it in
	// production as "violates foreign key constraint" warnings on
	// every brand-new group. See lessons-learned for the trace.
	if executionID == "" || projectID == "" || failureClass == "" || signature == "" {
		return false, fmt.Errorf("executionID, projectID, failureClass, signature all required")
	}

	var primaryExisting sql.NullString
	err = s.db.QueryRowContext(
		ctx,
		`SELECT failure_group_id FROM executions WHERE execution_id = $1`,
		executionID,
	).Scan(&primaryExisting)
	if err == sql.ErrNoRows {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("read execution failure_group_id: %w", err)
	}
	isPrimary := !primaryExisting.Valid || primaryExisting.String == ""

	groupID := deriveGroupID(projectID, failureClass, signature)
	now := time.Now().UTC().Format(time.RFC3339)

	// Newness probe BEFORE the upsert for webhook escalation
	// semantics. Racy but acceptable at current volume.
	var existedBefore int
	err = s.db.QueryRowContext(
		ctx,
		`SELECT 1 FROM failure_groups WHERE group_id = $1 LIMIT 1`,
		groupID,
	).Scan(&existedBefore)
	if err != nil && err != sql.ErrNoRows {
		return false, fmt.Errorf("probe failure_group existence: %w", err)
	}
	isNew = err == sql.ErrNoRows

	// Step 1: upsert failure_groups so the FK target exists. Counters
	// get +1 speculatively on the assumption that the link insert
	// below will succeed (the common case). If the link turns out to
	// already exist, the decrement at the bottom of this function
	// compensates.
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO failure_groups (
			group_id, project_id, failure_class, signature,
			first_seen, last_seen,
			event_count, affected_executions,
			sample_execution_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, 1, 1, $7)
		ON CONFLICT(group_id) DO UPDATE SET
			event_count = failure_groups.event_count + 1,
			affected_executions = failure_groups.affected_executions + 1,
			last_seen = excluded.last_seen
	`, groupID, projectID, failureClass, signature, now, now, executionID)
	if err != nil {
		return false, fmt.Errorf("upsert failure_group: %w", err)
	}

	// Step 2: insert the link. FK on group_id is now satisfied.
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO execution_failure_groups (
			execution_id, group_id, is_primary, classified_at
		)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT(execution_id, group_id) DO NOTHING
	`, executionID, groupID, isPrimary, now)
	if err != nil {
		return false, fmt.Errorf("link execution to failure_group: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected on membership insert: %w", err)
	}
	if inserted == 0 {
		// Link already existed; undo the speculative counter
		// increment so affected_executions stays accurate.
		_, derr := s.db.ExecContext(ctx, `
			UPDATE failure_groups
			SET event_count = failure_groups.event_count - 1,
			    affected_executions = failure_groups.affected_executions - 1
			WHERE group_id = $1
		`, groupID)
		if derr != nil {
			s.logger.Warn("failed to undo speculative counter increment after link conflict",
				"execution_id", executionID,
				"failure_group_id", groupID,
				"error", derr.Error(),
			)
		}
		return false, nil
	}

	if isPrimary {
		_, err = s.db.ExecContext(
			ctx,
			`UPDATE executions SET failure_group_id = $1 WHERE execution_id = $2`,
			groupID,
			executionID,
		)
		if err != nil {
			return false, fmt.Errorf("set primary failure_group on execution: %w", err)
		}
	}

	s.logger.Info("execution grouped",
		"execution_id", executionID,
		"failure_group_id", groupID,
		"failure_class", failureClass,
		"signature", signature,
		"is_new_group", isNew,
		"is_primary", isPrimary,
	)
	return isNew, nil
}

func (s *PostgresStore) GroupCrashedExecution(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassCrashes, signature)
}

func (s *PostgresStore) GroupTimeBudgetExceedance(ctx context.Context, executionID, projectID string, durationMs int64) (isNew bool, err error) {
	signature := TimeBudgetSignature(durationMs)
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassLoops, signature)
}

func (s *PostgresStore) GroupStepCountExceedance(ctx context.Context, executionID, projectID string, eventCount int) (isNew bool, err error) {
	signature := StepCountSignature(eventCount)
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassLoops, signature)
}

func (s *PostgresStore) CountEventsForExecution(ctx context.Context, executionID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM events WHERE execution_id = $1`,
		executionID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count events: %w", err)
	}
	return n, nil
}

func (s *PostgresStore) SetExecutionCost(ctx context.Context, executionID string, cost float64) error {
	if cost <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE executions SET estimated_cost_usd = $1 WHERE execution_id = $2`,
		cost,
		executionID,
	)
	if err != nil {
		return fmt.Errorf("set execution cost: %w", err)
	}
	return nil
}

// FindFirstFailedTool uses Postgres jsonb operators. See the SQLite
// twin's docstring for the granular-sig contract (also returns
// exception_type).
func (s *PostgresStore) FindFirstFailedTool(ctx context.Context, executionID string) (toolName, exceptionType string, err error) {
	var name sql.NullString
	var exc sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT (payload::jsonb->>'tool_name'),
		       (payload::jsonb->>'exception_type')
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'tool_call'
		  AND (payload::jsonb->>'status') = 'failed'
		ORDER BY sequence ASC
		LIMIT 1
	`, executionID).Scan(&name, &exc)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("find first failed tool: %w", err)
	}
	if !name.Valid {
		return "", "", nil
	}
	if exc.Valid {
		exceptionType = exc.String
	}
	return name.String, exceptionType, nil
}

func (s *PostgresStore) GroupToolFailure(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassToolFailures, signature)
}

// FindFirstThrottlingSignal is the Postgres twin of the SQLite method
// of the same name. Pulls the four signature pieces from the first
// infrastructure_event row's payload and hands them to
// ThrottlingSignature for assembly. Returns "" with nil error when no
// such event exists.
func (s *PostgresStore) FindFirstThrottlingSignal(ctx context.Context, executionID string) (string, error) {
	var reason, provider, dimension, circuitState sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT
			(payload::jsonb->>'event_type'),
			(payload::jsonb->>'provider'),
			(payload::jsonb->>'quota_dimension'),
			(payload::jsonb->>'circuit_state')
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'infrastructure_event'
		ORDER BY sequence ASC
		LIMIT 1
	`, executionID).Scan(&reason, &provider, &dimension, &circuitState)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find first throttling signal: %w", err)
	}
	if !reason.Valid || reason.String == "" {
		return "", nil
	}
	return ThrottlingSignature(reason.String, provider.String, dimension.String, circuitState.String), nil
}

// GroupInfrastructureThrottled is the Postgres twin of the SQLite
// method of the same name.
func (s *PostgresStore) GroupInfrastructureThrottled(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassInfraThrottled, signature)
}

// FindFirstDLPSignal is the Postgres twin of the SQLite method of
// the same name. LEGACY — delegates to FindFirstDLPSignalForSeverities
// with the historical default.
func (s *PostgresStore) FindFirstDLPSignal(ctx context.Context, executionID string) (string, error) {
	return s.FindFirstDLPSignalForSeverities(ctx, executionID,
		[]string{"critical", "high"})
}

// FindFirstDLPSignalForSeverities — Postgres twin (data_leakage.G5
// wave). Same shape as the sqlite version: builds the IN clause
// dynamically from the customer's allowed-severity slice. Postgres
// numbered placeholders ($1, $2, ...) drive the IN clause; the
// CASE ORDER BY is constant so priority remains deterministic.
func (s *PostgresStore) FindFirstDLPSignalForSeverities(
	ctx context.Context,
	executionID string,
	allowed []string,
) (string, error) {
	if len(allowed) == 0 {
		return "", fmt.Errorf("FindFirstDLPSignalForSeverities: allowed severities slice required")
	}
	// Postgres uses numbered placeholders; execution_id is $1, then
	// each severity is $2, $3, ... Build the IN clause to match.
	placeholders := make([]string, len(allowed))
	args := make([]any, 0, len(allowed)+1)
	args = append(args, executionID)
	for i, sev := range allowed {
		placeholders[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, sev)
	}
	query := `
		SELECT (payload::jsonb #>> '{hits,0,rule_id}')
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'dlp_scan_result'
		  AND (payload::jsonb->>'highest_severity') IN (` +
		strings.Join(placeholders, ",") + `)
		ORDER BY
			CASE (payload::jsonb->>'highest_severity')
				WHEN 'critical' THEN 0
				WHEN 'high'     THEN 1
				WHEN 'medium'   THEN 2
				ELSE 3
			END ASC,
			sequence ASC
		LIMIT 1
	`
	var ruleID sql.NullString
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&ruleID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find first dlp signal for severities: %w", err)
	}
	if !ruleID.Valid {
		return "", nil
	}
	return ruleID.String, nil
}

// GroupDataLeakage is the Postgres twin of the SQLite method of the
// same name.
func (s *PostgresStore) GroupDataLeakage(ctx context.Context, executionID, projectID, ruleID string) (isNew bool, err error) {
	if ruleID == "" {
		return false, fmt.Errorf("ruleID required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassDataLeakage, ruleID)
}

// ListCheckpointPayloads is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) ListCheckpointPayloads(ctx context.Context, executionID string) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'checkpoint'
		ORDER BY sequence ASC
	`, executionID)
	if err != nil {
		return nil, fmt.Errorf("list checkpoint payloads: %w", err)
	}
	defer rows.Close()
	out := [][]byte{}
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan checkpoint payload: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate checkpoint payloads: %w", err)
	}
	return out, nil
}

// GroupSemanticLoop is the Postgres twin of the SQLite method of the
// same name.
func (s *PostgresStore) GroupSemanticLoop(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassSemanticLoop, signature)
}

// ListSuccessfulToolReturns is the Postgres twin of the SQLite
// method of the same name.
func (s *PostgresStore) ListSuccessfulToolReturns(
	ctx context.Context,
	projectID, toolName, excludeExecutionID string,
	limit int,
) ([][]byte, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT (ev.payload::jsonb->>'return_value')
		FROM events ev
		JOIN executions ex ON ex.execution_id = ev.execution_id
		WHERE ex.project_id = $1
		  AND ev.event_type = 'tool_call'
		  AND (ev.payload::jsonb->>'tool_name') = $2
		  AND COALESCE(ev.payload::jsonb->>'status', 'ok') != 'failed'
		  AND ev.execution_id != $3
		ORDER BY ev.timestamp DESC
		LIMIT $4
	`, projectID, toolName, excludeExecutionID, limit)
	if err != nil {
		return nil, fmt.Errorf("list successful tool returns: %w", err)
	}
	defer rows.Close()
	out := [][]byte{}
	for rows.Next() {
		var p sql.NullString
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan tool return: %w", err)
		}
		if !p.Valid || p.String == "" {
			continue
		}
		out = append(out, []byte(p.String))
	}
	return out, rows.Err()
}

// ListToolNamesInExecution is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) ListToolNamesInExecution(ctx context.Context, executionID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT (payload::jsonb->>'tool_name')
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'tool_call'
		  AND COALESCE(payload::jsonb->>'status', 'ok') != 'failed'
	`, executionID)
	if err != nil {
		return nil, fmt.Errorf("list tool names: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n sql.NullString
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("scan tool name: %w", err)
		}
		if !n.Valid || n.String == "" {
			continue
		}
		out = append(out, n.String)
	}
	return out, rows.Err()
}

// GroupToolSchemaDrift is the Postgres twin of the SQLite method of
// the same name.
func (s *PostgresStore) GroupToolSchemaDrift(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassToolSchemaDrift, signature)
}

// ListLLMCallPayloads is the Postgres twin of the SQLite method of
// the same name.
func (s *PostgresStore) ListLLMCallPayloads(ctx context.Context, executionID string) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'llm_call'
		ORDER BY sequence ASC
	`, executionID)
	if err != nil {
		return nil, fmt.Errorf("list llm_call payloads: %w", err)
	}
	defer rows.Close()
	out := [][]byte{}
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan llm_call payload: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GroupContextOverflow is the Postgres twin of the SQLite method of
// the same name.
func (s *PostgresStore) GroupContextOverflow(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassContextOverflow, signature)
}

// GroupTokenWaste is the Postgres twin of the SQLite method of the
// same name.
func (s *PostgresStore) GroupTokenWaste(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassTokenWaste, signature)
}

// ListAllToolCallPayloads is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) ListAllToolCallPayloads(ctx context.Context, executionID string) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'tool_call'
		ORDER BY sequence ASC
	`, executionID)
	if err != nil {
		return nil, fmt.Errorf("list all tool_call payloads: %w", err)
	}
	defer rows.Close()
	out := [][]byte{}
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan tool_call payload: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GroupSandboxEscape is the Postgres twin of the SQLite method of
// the same name.
func (s *PostgresStore) GroupSandboxEscape(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassSandboxEscape, signature)
}

// ListEvalScorePayloads is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) ListEvalScorePayloads(ctx context.Context, executionID string) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'eval_score'
		ORDER BY sequence ASC
	`, executionID)
	if err != nil {
		return nil, fmt.Errorf("list eval_score payloads: %w", err)
	}
	defer rows.Close()
	out := [][]byte{}
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan eval_score payload: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GroupGroundingFailure is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) GroupGroundingFailure(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassGroundingFailure, signature)
}

// ListHandoffsWithChildStatus is the Postgres twin of the SQLite
// method of the same name. Uses ->> to pull typed fields out of
// the jsonb payload column. The LEFT JOIN keeps handoffs whose
// child_execution_id did not resolve (either because the SDK
// could not provide one at emit-time, or because the referenced
// execution is in a different project_id and tenant isolation
// dropped it from the join).
func (s *PostgresStore) ListHandoffsWithChildStatus(
	ctx context.Context,
	parentExecutionID, projectID string,
) ([]HandoffWithChildStatus, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			(e.payload::jsonb->>'from_agent')         AS from_agent,
			(e.payload::jsonb->>'to_agent')           AS to_agent,
			(e.payload::jsonb->>'handoff_kind')       AS handoff_kind,
			(e.payload::jsonb->>'child_execution_id') AS child_execution_id,
			e.timestamp                               AS handoff_emitted_at,
			c.execution_id                            AS child_id_found,
			c.status                                  AS child_status,
			c.ended_at                                AS child_ended_at
		FROM events e
		LEFT JOIN executions c
		  ON c.execution_id = (e.payload::jsonb->>'child_execution_id')
		 AND c.project_id   = $1
		WHERE e.execution_id = $2
		  AND e.event_type   = 'agent_handoff'
		ORDER BY e.sequence ASC
	`, projectID, parentExecutionID)
	if err != nil {
		return nil, fmt.Errorf("list handoffs with child status: %w", err)
	}
	defer rows.Close()
	out := []HandoffWithChildStatus{}
	for rows.Next() {
		var (
			fromAgent  sql.NullString
			toAgent    sql.NullString
			handoffKnd sql.NullString
			childID    sql.NullString
			emittedAt  time.Time
			foundID    sql.NullString
			childStat  sql.NullString
			childEnded sql.NullTime
		)
		if err := rows.Scan(
			&fromAgent, &toAgent, &handoffKnd, &childID,
			&emittedAt, &foundID, &childStat, &childEnded,
		); err != nil {
			return nil, fmt.Errorf("scan handoff row: %w", err)
		}
		row := HandoffWithChildStatus{
			FromAgent:        fromAgent.String,
			ToAgent:          toAgent.String,
			HandoffKind:      handoffKnd.String,
			ChildExecutionID: childID.String,
			HandoffEmittedAt: emittedAt,
		}
		if foundID.Valid && foundID.String != "" {
			row.ChildExists = true
			row.ChildStatus = childStat.String
			if childEnded.Valid {
				t := childEnded.Time
				row.ChildEndedAt = &t
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// GroupCascadingFailure is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) GroupCascadingFailure(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassCascadingFailure, signature)
}

// ListHandoffEdgesInTopology is the Postgres twin of the SQLite
// method of the same name. Uses standard SQL:1999 recursive CTE
// syntax (no Postgres-specific quirks vs SQLite for this shape).
func (s *PostgresStore) ListHandoffEdgesInTopology(
	ctx context.Context,
	rootExecutionID, projectID string,
	maxDepth int,
) ([]HandoffEdge, error) {
	if maxDepth <= 0 {
		maxDepth = 8
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE subtree(execution_id, depth) AS (
			SELECT execution_id, 0
			FROM executions
			WHERE execution_id = $1 AND project_id = $2
			UNION ALL
			SELECT e.execution_id, s.depth + 1
			FROM executions e
			JOIN subtree s ON e.parent_execution_id = s.execution_id
			WHERE e.project_id = $3 AND s.depth < $4
		)
		SELECT
			e.execution_id,
			(e.payload::jsonb->>'from_agent') AS from_agent,
			(e.payload::jsonb->>'to_agent')   AS to_agent,
			e.timestamp                       AS emitted_at
		FROM events e
		JOIN subtree s ON s.execution_id = e.execution_id
		WHERE e.event_type = 'agent_handoff'
		ORDER BY e.timestamp ASC
	`, rootExecutionID, projectID, projectID, maxDepth)
	if err != nil {
		return nil, fmt.Errorf("list handoff edges in topology: %w", err)
	}
	defer rows.Close()
	out := []HandoffEdge{}
	for rows.Next() {
		var (
			edge HandoffEdge
			from sql.NullString
			to   sql.NullString
		)
		if err := rows.Scan(&edge.EmittingExecutionID, &from, &to, &edge.EmittedAt); err != nil {
			return nil, fmt.Errorf("scan handoff edge: %w", err)
		}
		edge.FromAgent = from.String
		edge.ToAgent = to.String
		if edge.FromAgent == "" || edge.ToAgent == "" {
			continue
		}
		out = append(out, edge)
	}
	return out, rows.Err()
}

// GroupCoordinationDeadlock is the Postgres twin of the SQLite
// method of the same name.
func (s *PostgresStore) GroupCoordinationDeadlock(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassCoordinationDeadlock, signature)
}

// CountDistinctTenantsWithProviderError is the Postgres twin of the
// SQLite method of the same name.
func (s *PostgresStore) CountDistinctTenantsWithProviderError(
	ctx context.Context,
	projectID, provider, errorClass string,
	since time.Time,
) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT COALESCE(x.tenant_id, ''))
		FROM (
			SELECT DISTINCT x.execution_id, x.tenant_id
			FROM executions x
			JOIN events ev ON ev.execution_id = x.execution_id
			WHERE x.project_id = $1
			  AND ev.event_type = 'llm_call'
			  AND (ev.payload::jsonb->>'provider')    = $2
			  AND (ev.payload::jsonb->>'error_class') = $3
			  AND x.started_at >= $4
		) AS x
	`, projectID, provider, errorClass, since).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count distinct tenants with provider error: %w", err)
	}
	return n, nil
}

// GroupProviderIncident is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) GroupProviderIncident(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassProviderIncident, signature)
}

// ListHumanInterventionPayloads is the Postgres twin of the
// SQLite method of the same name.
func (s *PostgresStore) ListHumanInterventionPayloads(ctx context.Context, executionID string) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = $1
		  AND event_type   = 'human_intervention'
		ORDER BY sequence ASC
	`, executionID)
	if err != nil {
		return nil, fmt.Errorf("list human_intervention payloads: %w", err)
	}
	defer rows.Close()
	out := [][]byte{}
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan human_intervention payload: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GroupHITLTimeout is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) GroupHITLTimeout(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassHITLTimeout, signature)
}

// CountHITLOutcomesInWindow is the Postgres twin of the SQLite
// method of the same name.
func (s *PostgresStore) CountHITLOutcomesInWindow(
	ctx context.Context,
	projectID string,
	since time.Time,
) (HITLOutcomeCounts, error) {
	var counts HITLOutcomeCounts
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(DISTINCT x.execution_id),
			COUNT(DISTINCT CASE
				WHEN (ev.payload::jsonb->>'response_kind') = 'rejected'
				THEN x.execution_id
			END),
			COUNT(DISTINCT CASE
				WHEN (ev.payload::jsonb->>'response_kind') = 'edited'
				THEN x.execution_id
			END)
		FROM executions x
		JOIN events ev ON ev.execution_id = x.execution_id
		WHERE x.project_id   = $1
		  AND ev.event_type  = 'human_intervention'
		  AND x.started_at  >= $2
	`, projectID, since).Scan(
		&counts.TotalExecutionsWithHITL,
		&counts.ExecutionsWithRejection,
		&counts.ExecutionsWithEdit,
	)
	if err != nil {
		return counts, fmt.Errorf("count hitl outcomes in window: %w", err)
	}
	return counts, nil
}

// GroupHITLRejectionSpike is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) GroupHITLRejectionSpike(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassHITLRejectionSpike, signature)
}

// GetExecutionTopology is the Postgres twin of the SQLite method of
// the same name. Uses standard SQL:1999 recursive CTE syntax which
// Postgres supports natively (no quirks vs SQLite for the two-CTE
// shape we're using here).
func (s *PostgresStore) GetExecutionTopology(
	ctx context.Context,
	projectID, executionID string,
	maxDepth int,
) ([]TopologyNode, error) {
	if maxDepth <= 0 {
		maxDepth = 8
	}
	query := `
		WITH RECURSIVE
		ancestors(execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, depth) AS (
			SELECT execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, 0
			FROM executions
			WHERE execution_id = $1 AND project_id = $2
			UNION ALL
			SELECT e.execution_id, e.parent_execution_id, e.status, e.started_at, e.ended_at, e.duration_ms, e.sdk_language, e.failure_group_id, a.depth - 1
			FROM executions e
			JOIN ancestors a ON e.execution_id = a.parent_execution_id
			WHERE e.project_id = $3 AND a.depth > $4
		),
		descendants(execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, depth) AS (
			SELECT execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, 0
			FROM executions
			WHERE execution_id = $5 AND project_id = $6
			UNION ALL
			SELECT e.execution_id, e.parent_execution_id, e.status, e.started_at, e.ended_at, e.duration_ms, e.sdk_language, e.failure_group_id, d.depth + 1
			FROM executions e
			JOIN descendants d ON e.parent_execution_id = d.execution_id
			WHERE e.project_id = $7 AND d.depth < $8
		)
		SELECT execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, depth FROM ancestors
		UNION
		SELECT execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, depth FROM descendants
		ORDER BY depth ASC, started_at ASC
	`
	// $4 is the depth floor for the ancestors CTE (negative); $8 is
	// the depth ceiling for the descendants CTE (positive). We
	// precompute the negation in Go so Postgres can resolve the
	// parameter type without needing a unary-minus operator (which
	// triggers "operator is not unique: - unknown" against an
	// unparameterized integer).
	rows, err := s.db.QueryContext(ctx, query,
		executionID, projectID,
		projectID, -maxDepth,
		executionID, projectID,
		projectID, maxDepth,
	)
	if err != nil {
		return nil, fmt.Errorf("get execution topology: %w", err)
	}
	defer rows.Close()
	out := []TopologyNode{}
	for rows.Next() {
		var (
			n          TopologyNode
			parent     sql.NullString
			endedAt    sql.NullTime
			durationMs sql.NullInt64
			sdkLang    sql.NullString
			fgID       sql.NullString
		)
		if err := rows.Scan(
			&n.ExecutionID, &parent, &n.Status, &n.StartedAt,
			&endedAt, &durationMs, &sdkLang, &fgID, &n.Depth,
		); err != nil {
			return nil, fmt.Errorf("scan topology row: %w", err)
		}
		if parent.Valid {
			v := parent.String
			n.ParentExecutionID = &v
		}
		if endedAt.Valid {
			t := endedAt.Time
			n.EndedAt = &t
		}
		if durationMs.Valid {
			n.DurationMs = durationMs.Int64
		}
		if sdkLang.Valid {
			n.SDKLanguage = sdkLang.String
		}
		if fgID.Valid {
			v := fgID.String
			n.FailureGroupID = &v
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// FindFirstFailedValidator compares the jsonb-extracted 'passed' field
// to the literal text 'false'. In SQLite the same comparison was
// against integer 0 because SQLite's JSON1 returns 0 for false; in
// Postgres jsonb the text form is 'false'.
func (s *PostgresStore) FindFirstFailedValidator(
	ctx context.Context,
	executionID string,
) (validatorName, severityHint, category string, err error) {
	var name sql.NullString
	var sev sql.NullString
	var cat sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT (payload::jsonb->>'name'),
		       (payload::jsonb->>'severity'),
		       (payload::jsonb->>'category')
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'validator_result'
		  AND (payload::jsonb->>'passed') = 'false'
		ORDER BY sequence ASC
		LIMIT 1
	`, executionID).Scan(&name, &sev, &cat)
	if err == sql.ErrNoRows {
		return "", "", "", nil
	}
	if err != nil {
		return "", "", "", fmt.Errorf("find first failed validator: %w", err)
	}
	if !name.Valid {
		return "", "", "", nil
	}
	if sev.Valid {
		severityHint = sev.String
	}
	if cat.Valid {
		category = cat.String
	}
	return name.String, severityHint, category, nil
}

// UpdateFailureGroupSeverityHint — Postgres twin (validator_failures.G1).
func (s *PostgresStore) UpdateFailureGroupSeverityHint(
	ctx context.Context,
	groupID string,
	severityHint string,
) error {
	if groupID == "" {
		return fmt.Errorf("groupID required")
	}
	var hint sql.NullString
	if severityHint != "" {
		hint = sql.NullString{String: severityHint, Valid: true}
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE failure_groups
		SET severity_hint = $1
		WHERE group_id = $2
	`, hint, groupID)
	if err != nil {
		return fmt.Errorf("update failure_group severity_hint: %w", err)
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

// GetFailureGroupSeverityHint — Postgres twin.
func (s *PostgresStore) GetFailureGroupSeverityHint(
	ctx context.Context,
	groupID string,
) (string, error) {
	var hint sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT severity_hint FROM failure_groups WHERE group_id = $1
	`, groupID).Scan(&hint)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get failure_group severity_hint: %w", err)
	}
	if !hint.Valid {
		return "", nil
	}
	return hint.String, nil
}

func (s *PostgresStore) GroupValidatorFailure(ctx context.Context, executionID, projectID, validatorName string) (isNew bool, err error) {
	if validatorName == "" {
		return false, fmt.Errorf("validatorName required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassValidator, validatorName)
}

func (s *PostgresStore) GroupPromptInjection(ctx context.Context, executionID, projectID, patternName string) (isNew bool, err error) {
	if patternName == "" {
		return false, fmt.Errorf("patternName required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassInjection, patternName)
}

// GroupCostVelocity is the Postgres twin of the SQLite method of the
// same name. Caller is responsible for the per-project threshold
// check (see HandleUpdateExecution + GetProjectCostVelocityThresholdUSD);
// the store layer just writes the cluster.
func (s *PostgresStore) GroupCostVelocity(ctx context.Context, executionID, projectID string, costUSD float64) (isNew bool, err error) {
	signature := CostVelocitySignature(costUSD)
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassCostVelocity, signature)
}

// GroupCostVelocityRate is the Postgres twin of the SQLite method of
// the same name. Companion to GroupCostVelocity using the rate-bucketed
// signature.
func (s *PostgresStore) GroupCostVelocityRate(ctx context.Context, executionID, projectID string, ratePerMinUSD float64) (isNew bool, err error) {
	signature := CostVelocityRateSignature(ratePerMinUSD)
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassCostVelocity, signature)
}

func (s *PostgresStore) GroupIdenticalCallLoop(ctx context.Context, executionID, projectID, callHash string) (isNew bool, err error) {
	if callHash == "" {
		return false, fmt.Errorf("callHash required")
	}
	signature := "identical_call_" + callHash
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassLoops, signature)
}

func (s *PostgresStore) GroupSimilarCallLoop(ctx context.Context, executionID, projectID, callHash string) (isNew bool, err error) {
	if callHash == "" {
		return false, fmt.Errorf("callHash required")
	}
	signature := "similar_call_" + callHash
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassLoops, signature)
}

func (s *PostgresStore) ListModelsForExecution(ctx context.Context, executionID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT (payload::jsonb->>'model') AS model
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'llm_call'
		  AND (payload::jsonb->>'model') IS NOT NULL
		  AND (payload::jsonb->>'model') != ''
		ORDER BY model ASC
	`, executionID)
	if err != nil {
		return nil, fmt.Errorf("list models for execution: %w", err)
	}
	defer rows.Close()

	models := make([]string, 0, 4)
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("scan model: %w", err)
		}
		if m != "" {
			models = append(models, m)
		}
	}
	return models, rows.Err()
}

func (s *PostgresStore) ListModelsForProjectSince(ctx context.Context, projectID string, cutoff time.Time, excludeExecutionID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT (e.payload::jsonb->>'model') AS model
		FROM events e
		JOIN executions x ON x.execution_id = e.execution_id
		WHERE x.project_id = $1
		  AND e.event_type = 'llm_call'
		  AND e.timestamp >= $2
		  AND e.execution_id != $3
		  AND (e.payload::jsonb->>'model') IS NOT NULL
		  AND (e.payload::jsonb->>'model') != ''
		ORDER BY model ASC
	`, projectID, cutoff.UTC(), excludeExecutionID)
	if err != nil {
		return nil, fmt.Errorf("list models for project since: %w", err)
	}
	defer rows.Close()

	models := make([]string, 0, 8)
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("scan model: %w", err)
		}
		if m != "" {
			models = append(models, m)
		}
	}
	return models, rows.Err()
}

func (s *PostgresStore) GroupDriftSignal(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("drift signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassDrift, signature)
}

func (s *PostgresStore) ListLLMUserMessagesForExecution(ctx context.Context, executionID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT (payload::jsonb->>'user_message') AS user_message
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'llm_call'
		  AND (payload::jsonb->>'user_message') IS NOT NULL
		  AND (payload::jsonb->>'user_message') != ''
		ORDER BY sequence ASC
	`, executionID)
	if err != nil {
		return nil, fmt.Errorf("list user messages for execution: %w", err)
	}
	defer rows.Close()

	out := make([]string, 0, 4)
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("scan user message: %w", err)
		}
		if m != "" {
			out = append(out, m)
		}
	}
	return out, rows.Err()
}

func (s *PostgresStore) ListLLMUserMessagesForProjectSince(ctx context.Context, projectID string, cutoff time.Time, excludeExecutionID string, limit int) ([]string, error) {
	query := `
		SELECT (e.payload::jsonb->>'user_message') AS user_message
		FROM events e
		JOIN executions x ON x.execution_id = e.execution_id
		WHERE x.project_id = $1
		  AND e.event_type = 'llm_call'
		  AND e.timestamp >= $2
		  AND e.execution_id != $3
		  AND (e.payload::jsonb->>'user_message') IS NOT NULL
		  AND (e.payload::jsonb->>'user_message') != ''
		ORDER BY e.timestamp DESC
	`
	args := []any{projectID, cutoff.UTC(), excludeExecutionID}
	if limit > 0 {
		query += " LIMIT $4"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list user messages for project since: %w", err)
	}
	defer rows.Close()

	out := make([]string, 0, 64)
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("scan user message: %w", err)
		}
		if m != "" {
			out = append(out, m)
		}
	}
	return out, rows.Err()
}

func (s *PostgresStore) ListFailureGroups(ctx context.Context, projectID string, opts ListFailureGroupsOpts) ([]*FailureGroup, error) {
	// Search + resolved-visibility filters (list-search-paginate
	// + failure-group-resolve waves). When Q is non-empty: ILIKE
	// substring on signature + failure_class. When IncludeResolved
	// is false (default): WHERE resolved_at IS NULL hides resolved
	// rows. ILIKE is Postgres' native case-insensitive substring;
	// the SQLite twin uses LOWER() + LIKE for the same effect.
	args := []any{projectID}
	whereClause := "fg.project_id = $1"
	if opts.Q != "" {
		whereClause += " AND (fg.signature ILIKE '%' || $2 || '%'" +
			" OR fg.failure_class ILIKE '%' || $2 || '%')"
		args = append(args, opts.Q)
	}
	if !opts.IncludeResolved {
		whereClause += " AND fg.resolved_at IS NULL"
	}
	args = append(args, opts.Limit, opts.Offset)
	limitPlaceholder := fmt.Sprintf("$%d", len(args)-1)
	offsetPlaceholder := fmt.Sprintf("$%d", len(args))

	// G202: whereClause is built above from allowlisted fragments
	// (project_id / severity / resolved-status filters). limitPlaceholder
	// and offsetPlaceholder are server-controlled $N tokens, never user
	// input. All user-supplied values flow through args... as
	// parameterized placeholders.
	//nolint:gosec // G202: false positive — see comment above.
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			fg.group_id, fg.project_id, fg.failure_class, fg.signature,
			fg.first_seen, fg.last_seen,
			fg.event_count, fg.affected_executions,
			COALESCE(SUM(e.estimated_cost_usd), 0) AS computed_cost,
			COALESCE(SUM(e.total_tokens_in), 0) AS computed_tokens_in,
			COALESCE(SUM(e.total_tokens_out), 0) AS computed_tokens_out,
			fg.sample_execution_id,
			fg.analysis_markdown, fg.analyzed_at, fg.analysis_model,
			fg.analysis_playbook_signature,
			fg.severity_hint,
			fg.resolved_at, fg.resolved_by
		FROM failure_groups fg
		LEFT JOIN executions e ON e.failure_group_id = fg.group_id
		WHERE `+whereClause+`
		GROUP BY fg.group_id, fg.project_id, fg.failure_class, fg.signature,
		         fg.first_seen, fg.last_seen, fg.event_count,
		         fg.affected_executions, fg.sample_execution_id,
		         fg.analysis_markdown, fg.analyzed_at, fg.analysis_model,
		         fg.analysis_playbook_signature,
		         fg.severity_hint, fg.resolved_at, fg.resolved_by
		ORDER BY fg.last_seen DESC
		LIMIT `+limitPlaceholder+` OFFSET `+offsetPlaceholder+`
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("query failure_groups: %w", err)
	}
	defer rows.Close()

	var out []*FailureGroup
	for rows.Next() {
		g, err := scanFailureGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ResolveFailureGroup — Postgres twin of SQLiteStore.ResolveFailureGroup.
// See that method's doc comment.
func (s *PostgresStore) ResolveFailureGroup(
	ctx context.Context,
	groupID, projectID, actorUserID string,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE failure_groups
		SET resolved_at = NOW(), resolved_by = $1
		WHERE group_id = $2 AND project_id = $3
	`, actorUserID, groupID, projectID)
	if err != nil {
		return fmt.Errorf("resolve failure_group: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("resolve failure_group rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UnresolveFailureGroup — Postgres twin of
// SQLiteStore.UnresolveFailureGroup. See that method's doc comment.
func (s *PostgresStore) UnresolveFailureGroup(
	ctx context.Context,
	groupID, projectID string,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE failure_groups
		SET resolved_at = NULL, resolved_by = NULL
		WHERE group_id = $1 AND project_id = $2
	`, groupID, projectID)
	if err != nil {
		return fmt.Errorf("unresolve failure_group: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("unresolve failure_group rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) GetFailureGroup(ctx context.Context, groupID string) (*FailureGroup, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT
			fg.group_id, fg.project_id, fg.failure_class, fg.signature,
			fg.first_seen, fg.last_seen,
			fg.event_count, fg.affected_executions,
			COALESCE(SUM(e.estimated_cost_usd), 0) AS computed_cost,
			COALESCE(SUM(e.total_tokens_in), 0) AS computed_tokens_in,
			COALESCE(SUM(e.total_tokens_out), 0) AS computed_tokens_out,
			fg.sample_execution_id,
			fg.analysis_markdown, fg.analyzed_at, fg.analysis_model,
			fg.analysis_playbook_signature,
			fg.severity_hint,
			fg.resolved_at, fg.resolved_by
		FROM failure_groups fg
		LEFT JOIN executions e ON e.failure_group_id = fg.group_id
		WHERE fg.group_id = $1
		GROUP BY fg.group_id, fg.project_id, fg.failure_class, fg.signature,
		         fg.first_seen, fg.last_seen, fg.event_count,
		         fg.affected_executions, fg.sample_execution_id,
		         fg.analysis_markdown, fg.analyzed_at, fg.analysis_model,
		         fg.analysis_playbook_signature,
		         fg.severity_hint, fg.resolved_at, fg.resolved_by
	`, groupID)
	g, err := scanFailureGroup(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query failure_group: %w", err)
	}
	return g, nil
}

// SaveFailureGroupAnalysis is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) SaveFailureGroupAnalysis(
	ctx context.Context,
	groupID, analysisMarkdown, analysisModel string,
	analyzedAt time.Time,
	playbookSignature string,
) error {
	var sig any
	if playbookSignature == "" {
		sig = nil
	} else {
		sig = playbookSignature
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE failure_groups
		SET analysis_markdown           = $1,
		    analyzed_at                 = $2,
		    analysis_model              = $3,
		    analysis_playbook_signature = $4
		WHERE group_id = $5
	`, analysisMarkdown, analyzedAt, analysisModel, sig, groupID)
	if err != nil {
		return fmt.Errorf("save failure_group analysis: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// CountAIAnalysesSincePeriodStart counts failure_groups for projectID
// whose analyzed_at >= since. Fallback when a project has no
// tenant_id (legacy unbackfilled row). Postgres twin of the SQLite
// method.
func (s *PostgresStore) CountAIAnalysesSincePeriodStart(
	ctx context.Context, projectID string, since time.Time,
) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM failure_groups
		WHERE project_id = $1
		  AND analyzed_at IS NOT NULL
		  AND analyzed_at >= $2
	`, projectID, since).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count ai analyses since: %w", err)
	}
	return count, nil
}

// ListAIAnalysesUsageByProject is the Postgres twin of the SQLite
// method. See sqlite.go for the contract.
func (s *PostgresStore) ListAIAnalysesUsageByProject(
	ctx context.Context, since time.Time,
) ([]*AIAnalysesByProjectRow, error) {
	// filter chips: string_agg(DISTINCT ...) is the Postgres
	// twin of SQLite's group_concat(DISTINCT). Returns the comma-
	// joined list of failure_class slugs; splitFailureClassesCSV
	// parses it into a deduped slice.
	rows, err := s.db.QueryContext(ctx, `
		SELECT fg.project_id, p.name, p.owner_email, p.tier, p.tenant_id,
		       COUNT(*) AS n,
		       string_agg(DISTINCT fg.failure_class, ',') AS classes
		FROM failure_groups fg
		JOIN projects p ON p.project_id = fg.project_id
		WHERE fg.analyzed_at IS NOT NULL
		  AND fg.analyzed_at >= $1
		GROUP BY fg.project_id, p.name, p.owner_email, p.tier, p.tenant_id
		ORDER BY n DESC, p.name ASC
	`, since)
	if err != nil {
		return nil, fmt.Errorf("list ai analyses by project: %w", err)
	}
	defer rows.Close()

	out := make([]*AIAnalysesByProjectRow, 0, 8)
	for rows.Next() {
		r := &AIAnalysesByProjectRow{}
		var email, tenantID, classes sql.NullString
		if err := rows.Scan(&r.ProjectID, &r.Name, &email, &r.Tier, &tenantID, &r.Count, &classes); err != nil {
			return nil, fmt.Errorf("scan ai analyses by project row: %w", err)
		}
		if email.Valid {
			r.OwnerEmail = email.String
		}
		if tenantID.Valid {
			r.TenantID = tenantID.String
		}
		r.FailureClasses = splitFailureClassesCSV(classes)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListAnalyzedFailureGroupsByProject is the Postgres twin of the
// SQLite method. See sqlite.go for the contract.
//
// Uses the canonical scanFailureGroup helper because first_seen and
// last_seen are TEXT in the Postgres schema (not TIMESTAMP), and the
// driver returns them as strings; scanning directly into time.Time
// throws "storing driver.Value type string into type *time.Time"
// against the live Neon database.
func (s *PostgresStore) ListAnalyzedFailureGroupsByProject(
	ctx context.Context, projectID string, since time.Time, limit int,
) ([]*FailureGroup, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT group_id, project_id, failure_class, signature,
		       first_seen, last_seen, event_count, affected_executions,
		       cost_wasted_usd, sample_execution_id,
		       analysis_markdown, analyzed_at, analysis_model,
		       analysis_playbook_signature,
		       severity_hint
		FROM failure_groups
		WHERE project_id = $1
		  AND analyzed_at IS NOT NULL
		  AND analyzed_at >= $2
		ORDER BY analyzed_at DESC
		LIMIT $3
	`, projectID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("list analyzed failure groups (postgres): %w", err)
	}
	defer rows.Close()

	out := make([]*FailureGroup, 0, 8)
	for rows.Next() {
		g, err := scanFailureGroup(rows)
		if err != nil {
			return nil, fmt.Errorf("scan analyzed failure group: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CountAIAnalysesByTenantSince counts failure_groups summed across
// every project owned by tenantID whose analyzed_at >= since.
// Canonical Team-tier rate-limit query. Postgres twin of the
// SQLite method.
func (s *PostgresStore) CountAIAnalysesByTenantSince(
	ctx context.Context, tenantID string, since time.Time,
) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM failure_groups fg
		JOIN projects p ON p.project_id = fg.project_id
		WHERE p.tenant_id = $1
		  AND fg.analyzed_at IS NOT NULL
		  AND fg.analyzed_at >= $2
	`, tenantID, since).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count ai analyses by tenant since: %w", err)
	}
	return count, nil
}

func (s *PostgresStore) GetFailureGroupByClassSignature(ctx context.Context, projectID, failureClass, signature string) (*FailureGroup, error) {
	groupID := deriveGroupID(projectID, failureClass, signature)
	return s.GetFailureGroup(ctx, groupID)
}

// ─────────────────────────────────────────────────────────────────────────
// Abuse signals + project suspension
// ─────────────────────────────────────────────────────────────────────────

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
