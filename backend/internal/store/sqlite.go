// SQLite implementation of the Store interface.
//
// Uses `modernc.org/sqlite`, a pure-Go SQLite driver, no cgo required.
// Slightly slower than the cgo variant under heavy write load, but for
// local development and the eventual Phase 1.5 acceptance criterion
// (events survive process restart), performance is not the constraint.
// Postgres comes online for production-scale writes via a separate Store
// implementation in a later slice.
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

	_ "modernc.org/sqlite" // SQLite driver registers under name "sqlite"

	"mesedi/backend/internal/events"
)

// SQLiteStore is the SQLite-backed Store implementation. Safe for
// concurrent use; the underlying *sql.DB handles connection pooling
// (SQLite has a single-writer lock but readers concurrent under WAL).
type SQLiteStore struct {
	db     *sql.DB
	logger *slog.Logger
}

// OpenSQLite opens (or creates) a SQLite database at the given DSN and
// runs all pending migrations from the embedded migrations/ directory.
// The DSN typically points to a file path with pragmas attached, e.g.:
//
//	file:./mesedi-dev.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)
func OpenSQLite(dsn string, logger *slog.Logger) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite is single-writer; pool size > 1 wastes memory without helping.
	// Setting max-open=1 also avoids "database is locked" errors under load.
	db.SetMaxOpenConns(1)

	// Enforce foreign keys explicitly regardless of what the DSN
	// includes. SQLite defaults to FK enforcement OFF, which silently
	// allows orphan inserts and lets production bugs slip past tests
	// that use a no-pragma in-memory DSN. Postgres enforces FKs by
	// default; this PRAGMA makes the SQLite store behave the same so
	// localhost integration tests catch the same class of bug
	// production would surface.
	if _, err := db.ExecContext(context.Background(), `PRAGMA foreign_keys = ON`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable foreign_keys pragma: %w", err)
	}

	s := &SQLiteStore{db: db, logger: logger}
	if err := s.applyMigrations(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply migrations: %w", err)
	}
	return s, nil
}

// Close releases the underlying connection pool. Idempotent.
func (s *SQLiteStore) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Ping verifies the database is reachable. Used by /health (eventually).
func (s *SQLiteStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// applyMigrations runs every embedded migration file in lexical order.
// Each file is wrapped in a transaction; if any statement fails, the
// whole file rolls back. Already-applied migrations (tracked in the
// schema_migrations table) are skipped.
func (s *SQLiteStore) applyMigrations(ctx context.Context) error {
	// Bootstrap schema_migrations table so we can track what's been applied.
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		return fmt.Errorf("bootstrap schema_migrations: %w", err)
	}

	// Enumerate embedded migrations and sort lexically.
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
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
			s.logger.Warn("skipping migration with unparseable name", "file", name)
			continue
		}

		// Has this version already been applied? Skip if so.
		var existing int
		err := s.db.QueryRowContext(ctx,
			"SELECT version FROM schema_migrations WHERE version = ?", version,
		).Scan(&existing)
		if err == nil {
			s.logger.Debug("migration already applied", "migration_version", version, "file", name)
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check migration %d: %w", version, err)
		}

		// Read + apply. Split on semicolons so each statement runs
		// independently. Bare-Exec on multi-statement SQL stops at the
		// first failed statement, which means an idempotency error on
		// statement N skips statements N+1..M. Per-statement application
		// with idempotency-error tolerance lets us be fully forgiving.
		body, err := fs.ReadFile(migrationsFS, path.Join("migrations", name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		statements := splitSQLStatements(string(body))
		for stmtIdx, stmt := range statements {
			if _, err := s.db.ExecContext(ctx, stmt); err != nil {
				// Tolerate idempotency errors. SQLite raises these when
				// a migration tries to add a column/table/index/etc.
				// that already exists. Most common cause is the partial
				// state created by older versions of this runner that
				// failed to record migrations after successful apply,
				// so every restart re-ran everything and migrations
				// that weren't purely CREATE-IF-NOT-EXISTS would crash.
				errMsg := strings.ToLower(err.Error())
				isIdempotencyErr := strings.Contains(errMsg, "duplicate column name") ||
					strings.Contains(errMsg, "already exists")
				if !isIdempotencyErr {
					return fmt.Errorf("apply migration %s statement %d: %w", name, stmtIdx+1, err)
				}
				s.logger.Warn("migration statement produced idempotency error, treating as already-applied",
					"migration_version", version, "file", name, "statement_index", stmtIdx+1, "error", err.Error())
			}
		}
		s.logger.Info("migration applied", "migration_version", version, "file", name)

		// Record the version as applied. This was missing from the
		// original runner. The check above would always go through
		// the apply path, which silently relied on every migration
		// being purely idempotent DDL (CREATE TABLE IF NOT EXISTS,
		// etc.). Adding the explicit record here closes the gap.
		if _, err := s.db.ExecContext(ctx,
			"INSERT OR IGNORE INTO schema_migrations (version) VALUES (?)",
			version); err != nil {
			return fmt.Errorf("record migration %d: %w", version, err)
		}
	}
	return nil
}

// splitSQLStatements splits a SQL string into individual statements
// on semicolons. Comments are stripped FIRST so semicolons inside `--`
// line comments don't cause spurious splits (migration 005 has a `;`
// inside its header comment text, which broke an earlier version of
// this splitter).
//
// Limitation: does NOT handle semicolons inside string literals. Our
// migration files are simple DDL with no embedded semicolons in
// strings, so this is sufficient. Switch to a proper SQL tokenizer
// if that ever changes.
func splitSQLStatements(body string) []string {
	// Pass 1: strip line comments. A `--` makes the rest of the line
	// a comment in SQL. Drop entirely-comment lines and trim in-line
	// comment suffixes from non-comment lines.
	cleaned := make([]string, 0)
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		cleaned = append(cleaned, line)
	}
	cleanedBody := strings.Join(cleaned, "\n")

	// Pass 2: split on semicolons now that comments are gone.
	out := make([]string, 0, 4)
	for _, raw := range strings.Split(cleanedBody, ";") {
		stmt := strings.TrimSpace(raw)
		if stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

// parseMigrationVersion extracts the integer prefix from `NNN_name.sql`.
// Returns (0, false) on malformed names.
func parseMigrationVersion(filename string) (int, bool) {
	base := strings.TrimSuffix(filename, ".sql")
	parts := strings.SplitN(base, "_", 2)
	if len(parts) == 0 {
		return 0, false
	}
	var version int
	if _, err := fmt.Sscanf(parts[0], "%d", &version); err != nil {
		return 0, false
	}
	return version, true
}

// ─────────────────────────────────────────────────────────────────────────
// Project + API key operations (bootstrap / admin path)
// ─────────────────────────────────────────────────────────────────────────

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
	// #209 hotfix: explicitly insert card_on_file=0 instead of relying
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
// customer's newest project. Used by the #196 /signin handler after
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
// ordered created_at ASC. v0.1 of the org-rollup feature (#259) uses
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
// definition in store.go for the use case (admin demo reset, #270).
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
// (#150); the customer-facing API has no equivalent endpoint.
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
// touching the Stripe columns. Founder admin lever (#150). Returns
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
	`, projectID, since.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339))
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

func (s *SQLiteStore) CreateAPIKey(ctx context.Context, k *APIKey) error {
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now().UTC()
	}
	scope := k.Scope
	if scope == "" {
		scope = APIKeyScopeCustomer
	}
	// source defaults to "manual" (long-lived, customer-visible) to
	// match the migration 028 default. Callers that need a different
	// classification (signup flow, /signin from OAuth callback, magic
	// link verify) set k.Source explicitly before calling this.
	source := k.Source
	if source == "" {
		source = APIKeySourceManual
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys (key_id, project_id, key_hash, key_prefix, name, created_at, user_id, scope, expires_at, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, k.KeyID, k.ProjectID, k.KeyHash, k.KeyPrefix, nullString(k.Name), k.CreatedAt, nullString(k.UserID), scope, k.ExpiresAt, source)
	if err != nil {
		return fmt.Errorf("insert api_key: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetAPIKeyByHash(ctx context.Context, keyHash string) (*APIKey, error) {
	k := &APIKey{}
	var name, userID sql.NullString
	var lastUsed sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT key_id, project_id, key_hash, key_prefix, name, created_at, last_used_at, user_id, scope, expires_at, source
		FROM api_keys WHERE key_hash = ?
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

func (s *SQLiteStore) TouchAPIKey(ctx context.Context, keyID string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE api_keys SET last_used_at = ? WHERE key_id = ?",
		time.Now().UTC(), keyID,
	)
	return err
}

// ListAPIKeysForProject returns every API key bound to the given
// project, NEWEST first. key_hash is intentionally omitted from the
// returned structs, that field is never serialized to clients or
// callers; only the hash on the server's authoritative copy ever
// touches the auth path.
func (s *SQLiteStore) ListAPIKeysForProject(
	ctx context.Context,
	projectID string,
) ([]*APIKey, error) {
	// Filter session-grade keys (sso_login, magic_link) out of the
	// customer-facing listing. They are minted invisibly by the SSO
	// callback / magic-link verify routes and the customer never
	// consciously created them; surfacing them in /admin/api-keys
	// would clutter the list and the "revoke" affordance would
	// silently log the customer out (#196).
	rows, err := s.db.QueryContext(ctx, `
		SELECT key_id, project_id, key_prefix, name, created_at, last_used_at, scope, expires_at, source
		FROM api_keys
		WHERE project_id = ?
		  AND source NOT IN ('sso_login', 'magic_link')
		ORDER BY created_at DESC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("query api_keys: %w", err)
	}
	defer rows.Close()
	return scanAPIKeyList(rows)
}

// ListAllAPIKeys returns every API key in the system, NEWEST first.
// Admin-only: used by the /admin/api-keys page to surface keys across
// every project (including the synthetic _admin project that holds
// admin-scope keys). key_hash is intentionally not selected.
//
// Session-grade keys (sso_login, magic_link) are excluded from this
// listing for the same reason ListAPIKeysForProject excludes them:
// they are invisible session credentials, not credentials the
// operator should be reasoning about in the admin UI.
func (s *SQLiteStore) ListAllAPIKeys(ctx context.Context) ([]*APIKey, error) {
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
	return scanAPIKeyList(rows)
}

// scanAPIKeyList consumes a *sql.Rows produced by one of the
// list-api_keys queries and returns the materialized slice. Centralized
// so list-by-project and list-all share identical scan semantics
// (column order MUST match the SELECT lists above).
func scanAPIKeyList(rows *sql.Rows) ([]*APIKey, error) {
	var out []*APIKey
	for rows.Next() {
		var (
			k          APIKey
			createdAt  string
			lastUsedAt sql.NullString
			name       sql.NullString
		)
		if err := rows.Scan(
			&k.KeyID, &k.ProjectID, &k.KeyPrefix,
			&name, &createdAt, &lastUsedAt, &k.Scope, &k.ExpiresAt, &k.Source,
		); err != nil {
			return nil, err
		}
		if name.Valid {
			k.Name = name.String
		}
		k.CreatedAt = parseFlexTime(createdAt)
		if lastUsedAt.Valid {
			t := parseFlexTime(lastUsedAt.String)
			if !t.IsZero() {
				k.LastUsedAt = &t
			}
		}
		out = append(out, &k)
	}
	return out, rows.Err()
}

// DeleteAPIKeyByID hard-deletes any API key by its key_id, with no
// project_id guard. Admin-only. Used by the /admin/api-keys page to
// revoke keys across every project (including admin-scope keys in
// project _admin). Returns ErrNotFound if the key does not exist.
func (s *SQLiteStore) DeleteAPIKeyByID(ctx context.Context, keyID string) error {
	res, err := s.db.ExecContext(
		ctx,
		`DELETE FROM api_keys WHERE key_id = ?`,
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

// DeleteProjectCascade hard-deletes a project and every row whose
// existence depends on it. Wired up for the customer-facing "Close
// account" flow on /app/settings (#188). Runs everything in a single
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
	// Real table names verified against migrations/* (#188 fix: prior
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
				// (#188 prior version did `q[:60]` which panicked on
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
// (#187). 0 is allowed and means "no project-level override; fall
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

// DeleteAPIKeysByUserID hard-deletes every API key whose user_id
// matches. Called from HandleRemoveMember so a removed team member's
// existing credentials stop working immediately (#187). Returns the
// number of rows deleted; never returns ErrNotFound (0 deletions is a
// valid outcome when the removed user never minted a key).
func (s *SQLiteStore) DeleteAPIKeysByUserID(
	ctx context.Context,
	userID string,
) (int, error) {
	res, err := s.db.ExecContext(
		ctx,
		`DELETE FROM api_keys WHERE user_id = ?`,
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

// DeleteAPIKey hard-deletes an API key, but ONLY if the key belongs
// to the given project. Returns ErrNotFound if the key doesn't exist
// OR if it belongs to a different project (don't leak existence
// across tenants). After deletion the key's hash is gone, re-minting
// requires a new random key.
func (s *SQLiteStore) DeleteAPIKey(
	ctx context.Context,
	keyID, projectID string,
) error {
	res, err := s.db.ExecContext(
		ctx,
		`DELETE FROM api_keys WHERE key_id = ? AND project_id = ?`,
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

// ─────────────────────────────────────────────────────────────────────────
// Project webhook operations (failure-class escalation, task #83)
// ─────────────────────────────────────────────────────────────────────────

// CreateProjectWebhook inserts a new webhook configuration row. The
// caller is expected to set WebhookID + Secret; CreatedAt is set if
// zero. EnabledClasses is JSON-encoded into the TEXT column, nil/empty
// slice persists as NULL (interpreted as "all classes" by the
// dispatcher).
func (s *SQLiteStore) CreateProjectWebhook(ctx context.Context, wh *ProjectWebhook) error {
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

	enabled := 0
	if wh.Enabled {
		enabled = 1
	}

	recurrenceMode := wh.RecurrenceMode
	if recurrenceMode == "" {
		recurrenceMode = RecurrenceModeOff
	}
	var windowSeconds sql.NullInt64
	if recurrenceMode == RecurrenceModeThrottled && wh.RecurrenceWindowSeconds > 0 {
		windowSeconds = sql.NullInt64{Int64: int64(wh.RecurrenceWindowSeconds), Valid: true}
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO project_webhooks (
			webhook_id, project_id, name, url, secret,
			enabled_classes, enabled, created_at, severity_filter,
			recurrence_mode, recurrence_window_seconds
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		wh.WebhookID, wh.ProjectID, wh.Name, wh.URL, wh.Secret,
		classesJSON, enabled, wh.CreatedAt.UTC().Format(time.RFC3339),
		wh.SeverityFilter,
		recurrenceMode, windowSeconds,
	)
	if err != nil {
		return fmt.Errorf("insert project_webhook: %w", err)
	}
	return nil
}

// ListProjectWebhooksForProject returns every webhook for a project,
// sorted newest first. The Secret field is intentionally NOT populated
// , it's only ever surfaced once at creation time, never on list.
func (s *SQLiteStore) ListProjectWebhooksForProject(
	ctx context.Context,
	projectID string,
) ([]*ProjectWebhook, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT webhook_id, project_id, name, url,
		       enabled_classes, enabled, created_at, severity_filter,
		       recurrence_mode, recurrence_window_seconds
		FROM project_webhooks
		WHERE project_id = ?
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
		var createdAt string
		var enabled int
		var windowSeconds sql.NullInt64
		if err := rows.Scan(
			&wh.WebhookID, &wh.ProjectID, &wh.Name, &wh.URL,
			&classesJSON, &enabled, &createdAt, &wh.SeverityFilter,
			&wh.RecurrenceMode, &windowSeconds,
		); err != nil {
			return nil, fmt.Errorf("scan project_webhook: %w", err)
		}
		wh.Enabled = enabled != 0
		wh.EnabledClasses = parseEnabledClasses(classesJSON)
		if windowSeconds.Valid {
			wh.RecurrenceWindowSeconds = int(windowSeconds.Int64)
		}
		if t, perr := time.Parse(time.RFC3339, createdAt); perr == nil {
			wh.CreatedAt = t
		}
		out = append(out, &wh)
	}
	return out, rows.Err()
}

// ListEnabledProjectWebhooks returns only the enabled webhooks for a
// project, WITH the Secret populated. Used by the dispatcher to sign
// payloads. Never call this from a handler that returns the result to
// a client, the secret is sensitive.
func (s *SQLiteStore) ListEnabledProjectWebhooks(
	ctx context.Context,
	projectID string,
) ([]*ProjectWebhook, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT webhook_id, project_id, name, url, secret,
		       enabled_classes, enabled, created_at, severity_filter,
		       recurrence_mode, recurrence_window_seconds
		FROM project_webhooks
		WHERE project_id = ? AND enabled = 1
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
		var createdAt string
		var enabled int
		var windowSeconds sql.NullInt64
		if err := rows.Scan(
			&wh.WebhookID, &wh.ProjectID, &wh.Name, &wh.URL, &wh.Secret,
			&classesJSON, &enabled, &createdAt, &wh.SeverityFilter,
			&wh.RecurrenceMode, &windowSeconds,
		); err != nil {
			return nil, fmt.Errorf("scan project_webhook: %w", err)
		}
		wh.Enabled = enabled != 0
		wh.EnabledClasses = parseEnabledClasses(classesJSON)
		if windowSeconds.Valid {
			wh.RecurrenceWindowSeconds = int(windowSeconds.Int64)
		}
		if t, perr := time.Parse(time.RFC3339, createdAt); perr == nil {
			wh.CreatedAt = t
		}
		out = append(out, &wh)
	}
	return out, rows.Err()
}

// DeleteProjectWebhook hard-deletes a webhook by id, scoped to project.
// Returns ErrNotFound if the webhook is absent OR belongs to another
// project, don't leak cross-tenant existence via id-guessing.
func (s *SQLiteStore) DeleteProjectWebhook(
	ctx context.Context,
	webhookID, projectID string,
) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM project_webhooks
		WHERE webhook_id = ? AND project_id = ?
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

// GetProjectWebhook returns one webhook by id with the Secret
// populated. Project-scoped, passing a webhook_id that belongs to a
// different project returns ErrNotFound (not 403) so we don't leak
// cross-tenant existence.
func (s *SQLiteStore) GetProjectWebhook(
	ctx context.Context,
	webhookID, projectID string,
) (*ProjectWebhook, error) {
	var wh ProjectWebhook
	var classesJSON sql.NullString
	var createdAt string
	var enabled int
	var windowSeconds sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT webhook_id, project_id, name, url, secret,
		       enabled_classes, enabled, created_at, severity_filter,
		       recurrence_mode, recurrence_window_seconds
		FROM project_webhooks
		WHERE webhook_id = ? AND project_id = ?
	`, webhookID, projectID).Scan(
		&wh.WebhookID, &wh.ProjectID, &wh.Name, &wh.URL, &wh.Secret,
		&classesJSON, &enabled, &createdAt, &wh.SeverityFilter,
		&wh.RecurrenceMode, &windowSeconds,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get project_webhook: %w", err)
	}
	wh.Enabled = enabled != 0
	wh.EnabledClasses = parseEnabledClasses(classesJSON)
	if windowSeconds.Valid {
		wh.RecurrenceWindowSeconds = int(windowSeconds.Int64)
	}
	if t, perr := time.Parse(time.RFC3339, createdAt); perr == nil {
		wh.CreatedAt = t
	}
	return &wh, nil
}

// GetWebhookRecurrenceLastFired returns when this webhook last fired
// for this failure group. Returns ErrNotFound if there is no row yet
// (the dispatcher treats that as "the window has elapsed" so the
// first recurrence ping always goes out).
func (s *SQLiteStore) GetWebhookRecurrenceLastFired(
	ctx context.Context,
	webhookID, groupID string,
) (time.Time, error) {
	var ts string
	err := s.db.QueryRowContext(ctx, `
		SELECT last_fired_at FROM webhook_recurrence_state
		WHERE webhook_id = ? AND group_id = ?
	`, webhookID, groupID).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("get webhook_recurrence_state: %w", err)
	}
	t, perr := time.Parse(time.RFC3339, ts)
	if perr != nil {
		return time.Time{}, fmt.Errorf("parse last_fired_at: %w", perr)
	}
	return t, nil
}

// UpsertWebhookRecurrenceLastFired records or updates the timestamp
// of the most recent fire for (webhook, group). Called from the
// dispatcher on every successful fire so the next throttle check
// observes the right baseline.
func (s *SQLiteStore) UpsertWebhookRecurrenceLastFired(
	ctx context.Context,
	webhookID, groupID string,
	t time.Time,
) error {
	if webhookID == "" || groupID == "" {
		return fmt.Errorf("webhook_id and group_id required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO webhook_recurrence_state (webhook_id, group_id, last_fired_at)
		VALUES (?, ?, ?)
		ON CONFLICT(webhook_id, group_id) DO UPDATE SET
			last_fired_at = excluded.last_fired_at
	`, webhookID, groupID, t.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("upsert webhook_recurrence_state: %w", err)
	}
	return nil
}

// RecordWebhookDelivery persists one delivery-attempt row. DeliveryID
// and CreatedAt are set if zero. ResponseBody is truncated to ~2KB to
// bound storage growth from chatty receivers.
func (s *SQLiteStore) RecordWebhookDelivery(
	ctx context.Context,
	d *WebhookDelivery,
) error {
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
		// Deterministic-ish: hash (webhook_id + created_at_nano + attempt).
		// Doesn't need to be cryptographically unique, collision space is
		// minuscule and a collision would just upsert one row.
		raw := d.WebhookID + d.CreatedAt.Format(time.RFC3339Nano) +
			fmt.Sprintf("/%d", d.Attempt)
		sum := sha256.Sum256([]byte(raw))
		d.DeliveryID = "del-" + hex.EncodeToString(sum[:8])
	}

	// Truncate response body to bound storage.
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
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		d.DeliveryID, d.WebhookID, d.ProjectID,
		nullableString(d.FailureClass), nullableString(d.Signature), nullableString(d.GroupID),
		d.Attempt, d.Status, httpStatus, nullableString(d.Error), nullableString(body),
		d.DurationMs, d.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert webhook_delivery: %w", err)
	}
	return nil
}

// ListDeliveriesForWebhook returns the most recent N delivery attempts
// for a webhook, newest first. limit <= 0 defaults to 50.
func (s *SQLiteStore) ListDeliveriesForWebhook(
	ctx context.Context,
	webhookID string,
	limit int,
) ([]*WebhookDelivery, error) {
	// Mirror Postgres clamp (#204 alert #10). Negative / zero falls
	// back to a sane default; anything above the package ceiling is
	// clamped so a future-bug caller can not drive an unbounded
	// allocation -- and CodeQL sees a constant upper bound at the
	// make() site below.
	if limit <= 0 {
		limit = 50
	}
	if limit > WebhookDeliveryListLimitMax {
		limit = WebhookDeliveryListLimitMax
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT delivery_id, webhook_id, project_id,
		       failure_class, signature, group_id,
		       attempt, status, http_status, error, response_body,
		       duration_ms, created_at
		FROM webhook_deliveries
		WHERE webhook_id = ?
		ORDER BY created_at DESC
		LIMIT ?
	`, webhookID, limit)
	if err != nil {
		return nil, fmt.Errorf("list webhook_deliveries: %w", err)
	}
	defer rows.Close()

	// Capacity hint uses the package-level constant; see Postgres
	// counterpart for the static-analysis rationale.
	out := make([]*WebhookDelivery, 0, WebhookDeliveryListLimitMax)
	for rows.Next() {
		var d WebhookDelivery
		var failureClass, signature, groupID, errMsg, respBody sql.NullString
		var httpStatus sql.NullInt64
		var createdAt string
		if err := rows.Scan(
			&d.DeliveryID, &d.WebhookID, &d.ProjectID,
			&failureClass, &signature, &groupID,
			&d.Attempt, &d.Status, &httpStatus, &errMsg, &respBody,
			&d.DurationMs, &createdAt,
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
		if t, perr := time.Parse(time.RFC3339Nano, createdAt); perr == nil {
			d.CreatedAt = t
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

// nullableString wraps an empty string as a SQL NULL so the column
// reads back as NULL rather than the literal empty string. Used by
// the delivery-log writer where most fields are optional.
func nullableString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// parseEnabledClasses converts the JSON TEXT column into a Go slice.
// Returns nil if the column is NULL or malformed, both are
// interpreted as "all classes" by the dispatcher, so the conservative
// fallback is safe.
func parseEnabledClasses(s sql.NullString) []string {
	if !s.Valid || s.String == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s.String), &out); err != nil {
		return nil
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────
// Execution operations
// ─────────────────────────────────────────────────────────────────────────

func (s *SQLiteStore) CreateExecution(ctx context.Context, e *events.Execution) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO executions (
			execution_id, project_id, parent_execution_id, status,
			started_at, ended_at, duration_ms,
			total_tokens_in, total_tokens_out, estimated_cost_usd,
			input_summary, output_summary, crash_signature,
			sdk_version, sdk_language, tenant_id, api_key_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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

// PauseExecution transitions a started execution into the
// awaiting_human state (Mesedi #18). Atomic: the WHERE clause
// guarantees the transition only succeeds from `started`; if the
// execution is in any other state, RowsAffected is 0 and we
// translate that into ErrInvalidLifecycleTransition (vs. plain
// ErrNotFound). pause_count is incremented unconditionally on the
// successful transition so the hitl_rejection_spike detector (#21)
// can read it as cumulative HITL cycle count without further
// computation.
func (s *SQLiteStore) PauseExecution(ctx context.Context, executionID, projectID string, pausedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE executions SET
			status      = 'awaiting_human',
			paused_at   = ?,
			pause_count = pause_count + 1
		WHERE execution_id = ?
		  AND project_id   = ?
		  AND status       = 'started'
	`, pausedAt, executionID, projectID)
	if err != nil {
		return fmt.Errorf("pause execution: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		// Distinguish "execution does not exist in this project"
		// from "execution exists but is not in `started` state".
		var status sql.NullString
		qErr := s.db.QueryRowContext(ctx, `
			SELECT status FROM executions
			WHERE execution_id = ? AND project_id = ?
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

// ResumeExecution transitions an awaiting_human execution back to
// started (Mesedi #18). Computes (resumedAt - paused_at) in
// milliseconds and adds it to total_paused_ms, then clears
// paused_at. SQLite stores TIMESTAMP as ISO8601 text; the
// (resumedAt - paused_at) arithmetic is performed in Go after
// reading the prior paused_at, then written in the same statement
// to keep the operation a single round-trip. We read the prior
// paused_at via a returning sub-select pattern (SQLite supports
// this in 3.35+; the deployed binary is well past that).
func (s *SQLiteStore) ResumeExecution(ctx context.Context, executionID, projectID string, resumedAt time.Time) error {
	// Read prior paused_at to compute delta. Doing this in two
	// statements rather than one expression keeps SQLite's
	// julianday() arithmetic out of the write path (its precision
	// is documented as "to within milliseconds" but loses fidelity
	// at millisecond granularity for very short pauses).
	var pausedAt sql.NullTime
	var status sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT paused_at, status FROM executions
		WHERE execution_id = ? AND project_id = ?
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
		// Clock skew defensive: a backwards delta would corrupt
		// total_paused_ms. Treat as zero rather than abort the
		// resume; the agent should not stay paused due to wall
		// clock issues.
		deltaMs = 0
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE executions SET
			status          = 'started',
			paused_at       = NULL,
			total_paused_ms = total_paused_ms + ?
		WHERE execution_id = ?
		  AND project_id   = ?
		  AND status       = 'awaiting_human'
	`, deltaMs, executionID, projectID)
	if err != nil {
		return fmt.Errorf("resume execution: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		// Raced with another writer; the status changed between
		// our probe and our update.
		return ErrInvalidLifecycleTransition
	}
	return nil
}

func (s *SQLiteStore) UpdateExecution(ctx context.Context, e *events.Execution) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE executions SET
			status              = COALESCE(NULLIF(?, ''), status),
			ended_at            = COALESCE(?, ended_at),
			duration_ms         = COALESCE(?, duration_ms),
			total_tokens_in     = COALESCE(?, total_tokens_in),
			total_tokens_out    = COALESCE(?, total_tokens_out),
			estimated_cost_usd  = COALESCE(?, estimated_cost_usd),
			output_summary      = COALESCE(NULLIF(?, ''), output_summary),
			crash_signature     = COALESCE(NULLIF(?, ''), crash_signature)
		WHERE execution_id = ?
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

func (s *SQLiteStore) GetExecution(ctx context.Context, executionID string) (*events.Execution, error) {
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
		FROM executions WHERE execution_id = ?
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

// ─────────────────────────────────────────────────────────────────────────
// Event operations
// ─────────────────────────────────────────────────────────────────────────

func (s *SQLiteStore) SaveEvents(ctx context.Context, batch []events.Event) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() // safe to call after Commit, becomes a no-op

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO events (event_id, execution_id, event_type, sequence, timestamp, duration_ms, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?)
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
// Errors + null helpers
// ─────────────────────────────────────────────────────────────────────────

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// ErrProjectStillActive is returned by GDPR purge paths (#219) when
// the caller passes a project_id that still has a row in the
// `projects` table — i.e. the project has not been closed via
// HandleCloseAccount + DeleteProjectCascade. The purge surface refuses
// live projects: customer-initiated audit deletion must follow the
// normal HandleCloseAccount flow first; admin-initiated GDPR purge is
// for already-closed projects only. Pre-#231 the guard counted
// audit_events with project_deleted_at IS NULL, which silently
// bypassed for newly-signed-up projects with no audit history — see
// audit_events.go for the bug history. The handler maps this to
// HTTP 422.
var ErrProjectStillActive = errors.New("project still active; close it before GDPR purge")

// ErrInvalidLifecycleTransition is returned when PauseExecution or
// ResumeExecution is called against an execution that is not in
// the expected prior state. The transition matrix is enforced at
// the store layer (rather than purely in the handler) so the
// invariant holds even if a future caller bypasses the HTTP API
// (Mesedi #18).
var ErrInvalidLifecycleTransition = errors.New("invalid lifecycle transition")

// ErrAlreadyAccepted is returned by MarkInviteAccepted when the
// invite row's accepted_at column is already non-NULL. Single-use
// invariant: each invite token can only be redeemed once (#263).
var ErrAlreadyAccepted = errors.New("invite already accepted")

// ErrExpired is returned by the invite-accept path when an invite's
// expires_at has passed. Distinct from ErrNotFound (which means the
// token never existed) so the handler can produce a more helpful
// "this invite has expired, ask for a new one" message.
var ErrExpired = errors.New("expired")

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullStringPtr(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return *t
}

func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullFloat(v float64) any {
	if v == 0 {
		return nil
	}
	return v
}

// ─────────────────────────────────────────────────────────────────────────
// Read-side executions queries (Phase 3b dashboard)
// ─────────────────────────────────────────────────────────────────────────

// ListExecutions returns the project's executions sorted by
// started_at DESC (most recent first), paginated.
func (s *SQLiteStore) ListExecutions(
	ctx context.Context,
	projectID string,
	limit, offset int,
) ([]*events.Execution, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			execution_id, project_id, status,
			started_at, ended_at,
			duration_ms, total_tokens_in, total_tokens_out,
			estimated_cost_usd, sdk_language, sdk_version, crash_signature
		FROM executions
		WHERE project_id = ?
		ORDER BY started_at DESC
		LIMIT ? OFFSET ?
	`, projectID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query executions: %w", err)
	}
	defer rows.Close()
	return scanExecutionRows(rows)
}

// ListActiveExecutionsByProject returns executions that are still
// running (status = "started"). Used by the budget-ceiling halt
// fan-out (#252) to enumerate halt targets when a tenant breaches.
func (s *SQLiteStore) ListActiveExecutionsByProject(
	ctx context.Context,
	projectID string,
) ([]*events.Execution, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			execution_id, project_id, status,
			started_at, ended_at,
			duration_ms, total_tokens_in, total_tokens_out,
			estimated_cost_usd, sdk_language, sdk_version, crash_signature
		FROM executions
		WHERE project_id = ? AND status = ?
		ORDER BY started_at DESC
	`, projectID, string(events.StatusStarted))
	if err != nil {
		return nil, fmt.Errorf("query active executions: %w", err)
	}
	defer rows.Close()
	return scanExecutionRows(rows)
}

// ListExecutionsByFailureGroup returns executions classified into
// groupID, sorted by started_at DESC. Joins through the
// execution_failure_groups link table so executions whose PRIMARY
// classification was a different group still surface here when this
// group was a SECONDARY classification on them. Before migration 039
// this query was a direct equality scan on executions.failure_group_id,
// which silently dropped every execution whose primary detector ran
// first and claimed the slot — that's the bug that motivated 039.
//
// Caller is expected to have already verified the group belongs to
// the auth context's project; this method does not enforce project
// scoping (the failure_groups foreign key on the link table provides
// it transitively).
func (s *SQLiteStore) ListExecutionsByFailureGroup(
	ctx context.Context,
	groupID string,
	limit, offset int,
) ([]*events.Execution, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			e.execution_id, e.project_id, e.status,
			e.started_at, e.ended_at,
			e.duration_ms, e.total_tokens_in, e.total_tokens_out,
			e.estimated_cost_usd, e.sdk_language, e.sdk_version, e.crash_signature
		FROM executions e
		INNER JOIN execution_failure_groups efg
			ON efg.execution_id = e.execution_id
		WHERE efg.group_id = ?
		ORDER BY e.started_at DESC
		LIMIT ? OFFSET ?
	`, groupID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query executions by failure_group: %w", err)
	}
	defer rows.Close()
	return scanExecutionRows(rows)
}

// scanExecutionRows is the shared row-iteration helper for both the
// project-scoped and failure-group-scoped execution list queries. Both
// queries return identical column ordering, so the scanning logic is
// truly shared (not just copy-paste).
func scanExecutionRows(rows *sql.Rows) ([]*events.Execution, error) {
	var out []*events.Execution
	for rows.Next() {
		var (
			e          events.Execution
			startedAt  string
			endedAt    sql.NullString
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
			&startedAt, &endedAt,
			&durationMs, &tokensIn, &tokensOut,
			&costUSD, &sdkLang, &sdkVer, &crashSig,
		); err != nil {
			return nil, err
		}
		e.StartedAt, _ = time.Parse(time.RFC3339, startedAt)
		if endedAt.Valid {
			t, _ := time.Parse(time.RFC3339, endedAt.String)
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

// ListEventsForExecution returns the events recorded against a single
// execution, sorted by sequence ASC (oldest first, matching the order
// they were emitted by the agent).
func (s *SQLiteStore) ListEventsForExecution(
	ctx context.Context,
	executionID string,
) ([]*events.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			event_id, execution_id, event_type, sequence,
			timestamp, duration_ms, payload
		FROM events
		WHERE execution_id = ?
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
			ts           string
			durationMs   sql.NullInt64
			payloadBytes []byte
		)
		if err := rows.Scan(
			&e.EventID, &e.ExecutionID, &e.EventType, &e.Sequence,
			&ts, &durationMs, &payloadBytes,
		); err != nil {
			return nil, err
		}
		e.Timestamp, _ = time.Parse(time.RFC3339, ts)
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

// CountExecutionsByStatusSince returns the number of executions for the
// given project, optionally filtered by status and/or cutoff. An empty
// status string means "any status." A zero cutoff means "all time."
// All four combinations are supported.
func (s *SQLiteStore) CountExecutionsByStatusSince(
	ctx context.Context,
	projectID, status string,
	cutoff time.Time,
) (int, error) {
	query := "SELECT COUNT(*) FROM executions WHERE project_id = ?"
	args := []any{projectID}

	if status != "" {
		query += " AND status = ?"
		args = append(args, status)
	}
	if !cutoff.IsZero() {
		query += " AND started_at >= ?"
		args = append(args, cutoff.UTC().Format(time.RFC3339))
	}

	var n int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count executions: %w", err)
	}
	return n, nil
}

// SumExecutionCostByProjectSince aggregates SUM(estimated_cost_usd) and
// COUNT(*) across executions of projectID. Used by the org-rollup
// endpoint (#259) for per-project burn. Reads the persisted
// estimated_cost_usd column directly, which matches what the existing
// project-scoped dashboard surfaces show.
func (s *SQLiteStore) SumExecutionCostByProjectSince(
	ctx context.Context,
	projectID string,
	since time.Time,
) (float64, int, error) {
	query := "SELECT COALESCE(SUM(estimated_cost_usd), 0), COUNT(*) FROM executions WHERE project_id = ?"
	args := []any{projectID}
	if !since.IsZero() {
		query += " AND started_at >= ?"
		args = append(args, since.UTC().Format(time.RFC3339))
	}
	var cost float64
	var count int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&cost, &count); err != nil {
		return 0, 0, fmt.Errorf("sum execution cost: %w", err)
	}
	return cost, count, nil
}

// GetCostByTenant aggregates per-tenant cost across the project's
// executions within the requested time window. NULL tenant_id rows
// collapse into a single TenantID="" bucket so the dashboard can
// render unattributed cost as a distinct row instead of dropping it
// silently. since/until are inclusive lower / exclusive upper bounds
// matched against executions.started_at; zero values disable the
// respective bound. limit caps row count (0 = unlimited).
func (s *SQLiteStore) GetCostByTenant(
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
		WHERE project_id = ?
	`
	args := []any{projectID}
	if !since.IsZero() {
		query += " AND started_at >= ?"
		args = append(args, since.UTC().Format(time.RFC3339))
	}
	if !until.IsZero() {
		query += " AND started_at < ?"
		args = append(args, until.UTC().Format(time.RFC3339))
	}
	query += `
		GROUP BY COALESCE(tenant_id, '')
		ORDER BY total_cost_usd DESC, execution_count DESC
	`
	if limit > 0 {
		query += " LIMIT ?"
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
// Phase 3a, Failure groups (crash detection)
// ─────────────────────────────────────────────────────────────────────────

// parseFlexTime parses a timestamp written by either of the two
// formats SQLite stores in our timestamp columns: RFC 3339 (our app-
// inserted rows) or "YYYY-MM-DD HH:MM:SS" (rows inserted via SQLite's
// datetime('now') default, like the bootstrap dev key). Returns zero
// time if neither parse succeeds.
func parseFlexTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t
	}
	return time.Time{}
}

// deriveGroupID returns a deterministic group_id for a given
// (project_id, failure_class, signature) tuple. Same inputs always
// produce the same output, across runs and across restarts, so no
// coordination is needed to look up "the" group for a signature.
//
// 16 hex chars from SHA-256 = 64 bits of entropy, which is comfortably
// collision-resistant for any realistic per-project failure-group
// volume (billions of distinct signatures before birthday-paradox
// collisions become measurable).
func deriveGroupID(projectID, failureClass, signature string) string {
	h := sha256.Sum256([]byte(projectID + "|" + failureClass + "|" + signature))
	return "grp-" + hex.EncodeToString(h[:8])
}

// groupExecutionInternal is the shared upsert path for all detection
// classes. Both GroupCrashedExecution and GroupTimeBudgetExceedance
// are thin wrappers around this, they just supply the appropriate
// failure_class + signature.
//
// Idempotency: if the execution already has a failure_group_id set
// (because it was already linked to a different group, or a previous
// call already linked it to this group), the function returns nil
// without double-counting. This is also how "crash classification
// wins over time-budget overlap" is enforced, the crash grouping
// runs first in the handler, sets failure_group_id, then the
// subsequent time-budget call short-circuits here.
func (s *SQLiteStore) groupExecutionInternal(
	ctx context.Context,
	executionID, projectID, failureClass, signature string,
) (isNew bool, err error) {
	if executionID == "" || projectID == "" || failureClass == "" || signature == "" {
		return false, fmt.Errorf("executionID, projectID, failureClass, signature all required")
	}

	// Confirm the execution row exists; capture its current primary
	// failure_group_id so we know whether to claim the primary slot.
	var primaryExisting sql.NullString
	err = s.db.QueryRowContext(
		ctx,
		`SELECT failure_group_id FROM executions WHERE execution_id = ?`,
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

	// Newness probe BEFORE the upsert so we can report isNew=true to
	// the caller for webhook escalation. Racy under concurrent
	// writers — both observers could see "not found" and both report
	// isNew=true. At Mesedi's current volume the worst case is
	// duplicate webhook deliveries, not data corruption.
	var existedBefore int
	err = s.db.QueryRowContext(
		ctx,
		`SELECT 1 FROM failure_groups WHERE group_id = ? LIMIT 1`,
		groupID,
	).Scan(&existedBefore)
	if err != nil && err != sql.ErrNoRows {
		return false, fmt.Errorf("probe failure_group existence: %w", err)
	}
	isNew = err == sql.ErrNoRows

	// CRITICAL ORDERING: upsert the failure_groups row BEFORE
	// inserting into the link table. The link table has a foreign
	// key on group_id; doing the link insert first against a
	// not-yet-created group violates the FK and the whole detector
	// silently fails ("link execution to failure_group: ERROR:
	// violates foreign key constraint"). The original migration-039
	// commit got this order backwards; SQLite tolerated it because
	// FK enforcement defaults off, Postgres surfaced it in
	// production. See lessons-learned for the full trace.
	//
	// Counters get +1 here on the assumption that the link insert
	// below WILL succeed (the common case). If the link turns out to
	// already exist — a true idempotent retry or a concurrent
	// writer winning the race — the decrement at the bottom of this
	// function compensates.
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO failure_groups (
			group_id, project_id, failure_class, signature,
			first_seen, last_seen,
			event_count, affected_executions,
			sample_execution_id
		)
		VALUES (?, ?, ?, ?, ?, ?, 1, 1, ?)
		ON CONFLICT(group_id) DO UPDATE SET
			event_count = event_count + 1,
			affected_executions = affected_executions + 1,
			last_seen = excluded.last_seen
	`, groupID, projectID, failureClass, signature, now, now, executionID)
	if err != nil {
		return false, fmt.Errorf("upsert failure_group: %w", err)
	}

	// Now insert the link. PK (execution_id, group_id) +
	// ON CONFLICT DO NOTHING gives true idempotency.
	isPrimaryInt := 0
	if isPrimary {
		isPrimaryInt = 1
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO execution_failure_groups (
			execution_id, group_id, is_primary, classified_at
		)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(execution_id, group_id) DO NOTHING
	`, executionID, groupID, isPrimaryInt, now)
	if err != nil {
		return false, fmt.Errorf("link execution to failure_group: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected on membership insert: %w", err)
	}
	if inserted == 0 {
		// Link already existed (true idempotent retry, or a
		// concurrent writer beat us). The counter +1 above was
		// speculative; undo it here so affected_executions
		// continues to mean "distinct executions classified into
		// this group."
		_, derr := s.db.ExecContext(ctx, `
			UPDATE failure_groups
			SET event_count = event_count - 1,
			    affected_executions = affected_executions - 1
			WHERE group_id = ?
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

	// Claim the primary slot if no detector has yet. Skipping when
	// already populated preserves the v002 first-detector-wins
	// behavior the dashboard's executions detail page still renders.
	if isPrimary {
		_, err = s.db.ExecContext(
			ctx,
			`UPDATE executions SET failure_group_id = ? WHERE execution_id = ?`,
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

// GroupCrashedExecution upserts a failure_group with failure_class=crashes
// for the given execution. Thin wrapper around groupExecutionInternal.
func (s *SQLiteStore) GroupCrashedExecution(
	ctx context.Context,
	executionID, projectID, signature string,
) (isNew bool, err error) {
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassCrashes, signature)
}

// timeBudgetThresholdMs is the hardcoded cutoff for "this execution
// took too long" detection in v0.0.1. Set artificially low (1s) for
// local-dev visibility; production default will be 60s (or 10min per
// the concept-doc step-budget detector spec) and configurable per
// project once the projects table gets per-project policy columns.
const timeBudgetThresholdMs int64 = 1000

// TimeBudgetSignature returns a coarse duration-bucket label so that
// "long-running executions" cluster into a small number of groups
// rather than one group per unique millisecond. Buckets: 1s+, 10s+,
// 60s+, 10m+, 1h+. Anything below the threshold is filtered upstream
// in the handler; this function assumes a positive duration that has
// already exceeded the threshold.
func TimeBudgetSignature(durationMs int64) string {
	switch {
	case durationMs < 10_000:
		return "time_budget_1s+"
	case durationMs < 60_000:
		return "time_budget_10s+"
	case durationMs < 600_000:
		return "time_budget_60s+"
	case durationMs < 3_600_000:
		return "time_budget_10m+"
	default:
		return "time_budget_1h+"
	}
}

// GroupTimeBudgetExceedance upserts a failure_group with
// failure_class=loops and a duration-bucketed signature. Called from
// HandleUpdateExecution after the crash check, so crash-classified
// executions are already linked to a crashes group and this call
// becomes a no-op via the idempotency check.
func (s *SQLiteStore) GroupTimeBudgetExceedance(
	ctx context.Context,
	executionID, projectID string,
	durationMs int64,
) (isNew bool, err error) {
	signature := TimeBudgetSignature(durationMs)
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassLoops, signature)
}

// StepCountSignature buckets event counts so high-step-count executions
// cluster into a small number of groups rather than one group per
// distinct count. Buckets: 10+, 50+, 100+, 500+, 5000+. Anything below
// the threshold is filtered upstream in the handler.
func StepCountSignature(count int) string {
	switch {
	case count < 50:
		return "step_count_10+"
	case count < 100:
		return "step_count_50+"
	case count < 500:
		return "step_count_100+"
	case count < 5_000:
		return "step_count_500+"
	default:
		return "step_count_5000+"
	}
}

// ThrottlingSignature builds the cluster signature for an
// infrastructure_throttled grouping from the (provider, dimension,
// circuit-state) tuple captured on an InfrastructureEventPayload.
// Signatures intentionally collapse:
//
//   - All "rate_limit" events for the same (provider, quota_dimension)
//     into one group. SREs care about "Anthropic is rate-limiting our
//     tokens_per_minute," not about which exact agent's call hit it.
//
//   - "circuit_breaker" trips by (provider, circuit_state) so the
//     "half_open re-test failed" pattern stays distinct from the
//     "open trip" pattern.
//
//   - "quota_exhausted" by provider only (these are hard caps that
//     don't care about dimension).
//
// Format: "<reason>:<provider>" or "<reason>:<provider>:<dim>".
// Unknown providers fall back to "unknown" so the signature is still
// stable. The handler filters out events with empty Provider before
// reaching this function.
func ThrottlingSignature(reason, provider, dimension, circuitState string) string {
	if provider == "" {
		provider = "unknown"
	}
	switch reason {
	case "rate_limit":
		if dimension == "" {
			return "rate_limit:" + provider
		}
		return "rate_limit:" + provider + ":" + dimension
	case "circuit_breaker":
		if circuitState == "" {
			circuitState = "open"
		}
		return "circuit_breaker:" + provider + ":" + circuitState
	case "quota_exhausted":
		return "quota_exhausted:" + provider
	default:
		// Future-proof: unknown reasons cluster by provider so they're
		// at least groupable, not exploded one-per-execution.
		return reason + ":" + provider
	}
}

// GroupStepCountExceedance upserts a failure_group with
// failure_class=loops and an event-count-bucketed signature. Same
// idempotency contract as the other groupers, runs in the handler
// AFTER both crash and time-budget checks, so it's the lowest-priority
// classification of the three.
func (s *SQLiteStore) GroupStepCountExceedance(
	ctx context.Context,
	executionID, projectID string,
	eventCount int,
) (isNew bool, err error) {
	signature := StepCountSignature(eventCount)
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassLoops, signature)
}

// CountEventsForExecution returns the number of event rows recorded
// against a single execution_id. Used by the step-count detector and
// (eventually) the replay UI's header.
func (s *SQLiteStore) CountEventsForExecution(
	ctx context.Context,
	executionID string,
) (int, error) {
	var n int
	err := s.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM events WHERE execution_id = ?`,
		executionID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count events: %w", err)
	}
	return n, nil
}

// SetExecutionCost writes a computed estimated_cost_usd onto an
// execution row. No-op if cost is non-positive (we don't want to
// overwrite an existing positive cost with 0 from a model whose
// pricing isn't in the table). Used by the post-PATCH cost aggregator
// in HandleUpdateExecution.
func (s *SQLiteStore) SetExecutionCost(
	ctx context.Context,
	executionID string,
	cost float64,
) error {
	if cost <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE executions SET estimated_cost_usd = ? WHERE execution_id = ?`,
		cost,
		executionID,
	)
	if err != nil {
		return fmt.Errorf("set execution cost: %w", err)
	}
	return nil
}

// FindFirstFailedToolName returns the tool_name of the first (lowest
// sequence) tool_call event with payload.status = "failed" for the
// given execution. Returns "" with nil error if no failed tool calls
// exist.
//
// Uses SQLite's JSON1 extension (json_extract) so we don't have to
// scan-and-unmarshal Go-side. The events table's payload column is
// stored as BLOB but JSON1 reads it transparently as JSON text.
func (s *SQLiteStore) FindFirstFailedToolName(
	ctx context.Context,
	executionID string,
) (string, error) {
	var toolName sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT json_extract(payload, '$.tool_name')
		FROM events
		WHERE execution_id = ?
		  AND event_type = 'tool_call'
		  AND json_extract(payload, '$.status') = 'failed'
		ORDER BY sequence ASC
		LIMIT 1
	`, executionID).Scan(&toolName)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find first failed tool: %w", err)
	}
	if !toolName.Valid {
		return "", nil
	}
	return toolName.String, nil
}

// GroupToolFailure upserts a failure_group with
// failure_class=tool_failures and signature=toolName. Same idempotency
// contract as the other groupers, if the execution is already linked
// to a higher-priority group (crash, time-budget, step-count), this is
// a no-op.
func (s *SQLiteStore) GroupToolFailure(
	ctx context.Context,
	executionID, projectID, toolName string,
) (isNew bool, err error) {
	if toolName == "" {
		return false, fmt.Errorf("toolName required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassToolFailures, toolName)
}

// FindFirstThrottlingSignal returns the cluster signature derived
// from the first (lowest-sequence) infrastructure_event row for the
// given execution. Returns "" with nil error when no such event
// exists or when the row's payload is missing required fields.
//
// Calls ThrottlingSignature internally to assemble the signature
// from the payload fields, so the handler only needs to pass the
// result to GroupInfrastructureThrottled. This keeps the signature
// assembly logic in one place rather than duplicating field-name
// knowledge in the handler.
func (s *SQLiteStore) FindFirstThrottlingSignal(
	ctx context.Context,
	executionID string,
) (string, error) {
	var reason, provider, dimension, circuitState sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT
			json_extract(payload, '$.event_type'),
			json_extract(payload, '$.provider'),
			json_extract(payload, '$.quota_dimension'),
			json_extract(payload, '$.circuit_state')
		FROM events
		WHERE execution_id = ?
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

// GroupInfrastructureThrottled upserts a failure_group with
// failure_class=infrastructure_throttled and the caller-supplied
// signature (built by ThrottlingSignature). Same idempotency
// contract as the other groupers: if the execution is already linked
// to a higher-priority group (crash, time-budget, step-count), this
// is a no-op.
func (s *SQLiteStore) GroupInfrastructureThrottled(
	ctx context.Context,
	executionID, projectID, signature string,
) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassInfraThrottled, signature)
}

// FindFirstDLPSignal returns the rule_id of the highest-priority
// dlp_scan_result hit recorded against this execution, or empty
// string if no DLP events fired. "Highest priority" means: prefer
// the first critical-severity hit by sequence; if none exist, fall
// back to the first high-severity hit. medium-severity hits never
// cluster and are filtered out here.
//
// The returned rule_id is the data_leakage cluster signature
// (e.g. "aws_access_key"), one failure_group per rule per project.
func (s *SQLiteStore) FindFirstDLPSignal(
	ctx context.Context,
	executionID string,
) (string, error) {
	var ruleID sql.NullString
	// JSON1 path: payload.highest_severity tells us at the event
	// level whether to even consider it. payload.hits is a sorted
	// array; the first element's rule_id is the canonical signature
	// (Summarize() sorts alphabetically so reads are deterministic).
	// We CASE on severity so a single SQL pass returns the
	// best-priority signal across all DLP events on this execution.
	err := s.db.QueryRowContext(ctx, `
		SELECT json_extract(payload, '$.hits[0].rule_id')
		FROM events
		WHERE execution_id = ?
		  AND event_type = 'dlp_scan_result'
		  AND json_extract(payload, '$.highest_severity') IN ('critical', 'high')
		ORDER BY
			CASE json_extract(payload, '$.highest_severity')
				WHEN 'critical' THEN 0
				WHEN 'high'     THEN 1
				ELSE 2
			END ASC,
			sequence ASC
		LIMIT 1
	`, executionID).Scan(&ruleID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find first dlp signal: %w", err)
	}
	if !ruleID.Valid {
		return "", nil
	}
	return ruleID.String, nil
}

// GroupDataLeakage upserts a failure_group with
// failure_class=data_leakage and signature=ruleID (e.g.
// "aws_access_key"). One group per rule per project. Idempotent: if
// the execution is already linked to a higher-priority group, this
// is a no-op.
func (s *SQLiteStore) GroupDataLeakage(
	ctx context.Context,
	executionID, projectID, ruleID string,
) (isNew bool, err error) {
	if ruleID == "" {
		return false, fmt.Errorf("ruleID required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassDataLeakage, ruleID)
}

// ListCheckpointPayloads returns the payloads of all checkpoint
// events on the given execution in sequence order. Returns an empty
// slice (not nil) when no checkpoints exist, so the semantic_loop
// detector can range over the result unconditionally.
func (s *SQLiteStore) ListCheckpointPayloads(
	ctx context.Context,
	executionID string,
) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = ?
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

// GroupSemanticLoop upserts a failure_group with
// failure_class=semantic_loop and the caller-supplied signature.
func (s *SQLiteStore) GroupSemanticLoop(
	ctx context.Context,
	executionID, projectID, signature string,
) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassSemanticLoop, signature)
}

// ListSuccessfulToolReturns returns recent return_value payloads
// from successful tool_call events for a (project, tool). Used by
// the schema-drift detector to build the historical baseline.
// Excludes the calling execution so we compare against PRIOR runs.
func (s *SQLiteStore) ListSuccessfulToolReturns(
	ctx context.Context,
	projectID, toolName, excludeExecutionID string,
	limit int,
) ([][]byte, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT json_extract(ev.payload, '$.return_value')
		FROM events ev
		JOIN executions ex ON ex.execution_id = ev.execution_id
		WHERE ex.project_id = ?
		  AND ev.event_type = 'tool_call'
		  AND json_extract(ev.payload, '$.tool_name') = ?
		  AND COALESCE(json_extract(ev.payload, '$.status'), 'ok') != 'failed'
		  AND ev.execution_id != ?
		ORDER BY ev.timestamp DESC
		LIMIT ?
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

// ListToolNamesInExecution returns the distinct tool_names invoked
// successfully in the execution. Used by the schema-drift detector
// to enumerate tools to query history for.
func (s *SQLiteStore) ListToolNamesInExecution(
	ctx context.Context,
	executionID string,
) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT json_extract(payload, '$.tool_name')
		FROM events
		WHERE execution_id = ?
		  AND event_type = 'tool_call'
		  AND COALESCE(json_extract(payload, '$.status'), 'ok') != 'failed'
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

// GroupToolSchemaDrift upserts a failure_group with
// failure_class=tool_schema_drift and the caller-supplied signature.
func (s *SQLiteStore) GroupToolSchemaDrift(
	ctx context.Context,
	executionID, projectID, signature string,
) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassToolSchemaDrift, signature)
}

// ListLLMCallPayloads returns every llm_call payload on the
// execution in sequence order. Shared by context_overflow (#3) and
// token_waste (#4); kept as one query so the handler only hits the
// DB once per execution-terminal update for both detectors.
func (s *SQLiteStore) ListLLMCallPayloads(
	ctx context.Context,
	executionID string,
) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = ?
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

// GroupContextOverflow upserts a failure_group with
// failure_class=context_overflow and the caller-supplied signature.
func (s *SQLiteStore) GroupContextOverflow(
	ctx context.Context,
	executionID, projectID, signature string,
) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassContextOverflow, signature)
}

// GroupTokenWaste upserts a failure_group with
// failure_class=token_waste and the caller-supplied signature.
func (s *SQLiteStore) GroupTokenWaste(
	ctx context.Context,
	executionID, projectID, signature string,
) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassTokenWaste, signature)
}

// ListAllToolCallPayloads returns every tool_call event payload on
// the execution in sequence order, including failed calls. Used by
// the sandbox_escape detector.
func (s *SQLiteStore) ListAllToolCallPayloads(
	ctx context.Context,
	executionID string,
) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = ?
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

// GroupSandboxEscape upserts a failure_group with
// failure_class=sandbox_escape and the caller-supplied signature.
func (s *SQLiteStore) GroupSandboxEscape(
	ctx context.Context,
	executionID, projectID, signature string,
) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassSandboxEscape, signature)
}

// ListEvalScorePayloads returns every eval_score event payload on
// the execution in sequence order. Used by the grounding_failure
// detector.
func (s *SQLiteStore) ListEvalScorePayloads(ctx context.Context, executionID string) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = ?
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

// GroupGroundingFailure upserts a failure_group with
// failure_class=grounding_failure and the caller-supplied signature.
func (s *SQLiteStore) GroupGroundingFailure(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassGroundingFailure, signature)
}

// ListHandoffsWithChildStatus joins every agent_handoff event on the
// parent execution with the terminal status of the referenced child
// execution. Used by the cascading_failure detector (#12).
//
// Implementation notes:
//
//   - The join uses LEFT JOIN so handoffs that did not populate
//     child_execution_id (e.g. the SDK could not resolve it at
//     emit-time) still appear in the result with ChildExists=false.
//
//   - The child execution must live in the same project_id as the
//     parent. Cross-project child ids would represent a tenant
//     leak and are silently dropped by the JOIN's WHERE clause.
//
//   - SQLite's JSON1 extension is used to pull the typed fields out
//     of the payload column. This matches the pattern used by
//     FindFirstFailedValidator.
func (s *SQLiteStore) ListHandoffsWithChildStatus(
	ctx context.Context,
	parentExecutionID, projectID string,
) ([]HandoffWithChildStatus, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			json_extract(e.payload, '$.from_agent')         AS from_agent,
			json_extract(e.payload, '$.to_agent')           AS to_agent,
			json_extract(e.payload, '$.handoff_kind')       AS handoff_kind,
			json_extract(e.payload, '$.child_execution_id') AS child_execution_id,
			e.timestamp                                     AS handoff_emitted_at,
			c.execution_id                                  AS child_id_found,
			c.status                                        AS child_status,
			c.ended_at                                      AS child_ended_at
		FROM events e
		LEFT JOIN executions c
		  ON c.execution_id = json_extract(e.payload, '$.child_execution_id')
		 AND c.project_id   = ?
		WHERE e.execution_id = ?
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

// GroupCascadingFailure upserts a failure_group with
// failure_class=cascading_failure and the detector-supplied
// signature.
func (s *SQLiteStore) GroupCascadingFailure(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassCascadingFailure, signature)
}

// ListHandoffEdgesInTopology returns every agent_handoff edge
// emitted by the rootExecutionID's topology subtree (root +
// descendants reachable via parent_execution_id). Implemented as
// a recursive CTE collecting the subtree, then joined with
// events filtered to agent_handoff to pull the from/to labels.
func (s *SQLiteStore) ListHandoffEdgesInTopology(
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
			WHERE execution_id = ? AND project_id = ?
			UNION ALL
			SELECT e.execution_id, s.depth + 1
			FROM executions e
			JOIN subtree s ON e.parent_execution_id = s.execution_id
			WHERE e.project_id = ? AND s.depth < ?
		)
		SELECT
			e.execution_id,
			json_extract(e.payload, '$.from_agent') AS from_agent,
			json_extract(e.payload, '$.to_agent')   AS to_agent,
			e.timestamp                             AS emitted_at
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
		// Drop edges with missing labels; they can't participate
		// in a meaningful cycle and would corrupt the signature.
		if edge.FromAgent == "" || edge.ToAgent == "" {
			continue
		}
		out = append(out, edge)
	}
	return out, rows.Err()
}

// GroupCoordinationDeadlock upserts a failure_group with
// failure_class=coordination_deadlock and the detector-supplied
// signature.
func (s *SQLiteStore) GroupCoordinationDeadlock(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassCoordinationDeadlock, signature)
}

// CountDistinctTenantsWithProviderError counts distinct tenant_id
// values across executions that emitted at least one llm_call
// event with the given provider + error_class since the supplied
// time. NULL tenant_id collapses to a single bucket and counts as
// one tenant when present. Used by the provider_incident detector
// (#16).
//
// SQLite-flavor implementation note: the query uses a sub-select
// to extract distinct (execution_id × tenant_id) pairs that match
// the predicate, then COUNT(DISTINCT tenant_id) over the outer
// query. COALESCE(tenant_id, ”) folds NULLs into the empty
// string so the outer DISTINCT collapses them together rather
// than treating each NULL as its own value.
func (s *SQLiteStore) CountDistinctTenantsWithProviderError(
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
			WHERE x.project_id = ?
			  AND ev.event_type = 'llm_call'
			  AND json_extract(ev.payload, '$.provider')    = ?
			  AND json_extract(ev.payload, '$.error_class') = ?
			  AND x.started_at >= ?
		) AS x
	`, projectID, provider, errorClass, since).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count distinct tenants with provider error: %w", err)
	}
	return n, nil
}

// GroupProviderIncident upserts a failure_group with
// failure_class=provider_incident and the detector-supplied
// signature.
func (s *SQLiteStore) GroupProviderIncident(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassProviderIncident, signature)
}

// ListHumanInterventionPayloads returns every human_intervention
// event payload on the execution in sequence order. Used by the
// hitl_timeout detector (#20) and the hitl_rejection_spike
// detector (#21).
func (s *SQLiteStore) ListHumanInterventionPayloads(ctx context.Context, executionID string) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = ?
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

// GroupHITLTimeout upserts a failure_group with
// failure_class=hitl_timeout and the detector-supplied signature.
func (s *SQLiteStore) GroupHITLTimeout(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassHITLTimeout, signature)
}

// CountHITLOutcomesInWindow aggregates human_intervention
// verdicts across the project's recent executions. Counts are
// over distinct executions: an execution with multiple human
// inputs still counts once per outcome category. Used by the
// hitl_rejection_spike detector (#21).
func (s *SQLiteStore) CountHITLOutcomesInWindow(
	ctx context.Context,
	projectID string,
	since time.Time,
) (HITLOutcomeCounts, error) {
	var counts HITLOutcomeCounts
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(DISTINCT x.execution_id),
			COUNT(DISTINCT CASE
				WHEN json_extract(ev.payload, '$.response_kind') = 'rejected'
				THEN x.execution_id
			END),
			COUNT(DISTINCT CASE
				WHEN json_extract(ev.payload, '$.response_kind') = 'edited'
				THEN x.execution_id
			END)
		FROM executions x
		JOIN events ev ON ev.execution_id = x.execution_id
		WHERE x.project_id   = ?
		  AND ev.event_type  = 'human_intervention'
		  AND x.started_at  >= ?
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

// GroupHITLRejectionSpike upserts a failure_group with
// failure_class=hitl_rejection_spike and the detector-supplied
// signature.
func (s *SQLiteStore) GroupHITLRejectionSpike(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassHITLRejectionSpike, signature)
}

// GetExecutionTopology walks the parent_execution_id graph rooted at
// the seed execution and returns every reachable node within the
// project. The traversal goes UP (ancestors) and DOWN (descendants)
// in two SQLite recursive CTEs unioned into one result set.
//
// SQLite-flavor implementation note: the two CTEs share the same
// starting execution but differ in direction; ancestors join on
// parent_execution_id, descendants on children-of-current. The depth
// guard is implemented as a level counter incremented per recursive
// step; the LIMIT is enforced via WHERE level < ?.
func (s *SQLiteStore) GetExecutionTopology(
	ctx context.Context,
	projectID, executionID string,
	maxDepth int,
) ([]TopologyNode, error) {
	if maxDepth <= 0 {
		maxDepth = 8 // safe default for visualization-friendly trees
	}
	// One query that unions ancestor traversal + descendant
	// traversal, dedupes on execution_id, and sorts by depth then
	// started_at. Depth is computed relative to the seed: negative
	// values for ancestors, zero for the seed, positive for
	// descendants. The dashboard normalizes depths to the smallest
	// observed value so the root renders at depth 0 visually.
	query := `
		WITH RECURSIVE
		ancestors(execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, depth) AS (
			SELECT execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, 0
			FROM executions
			WHERE execution_id = ? AND project_id = ?
			UNION ALL
			SELECT e.execution_id, e.parent_execution_id, e.status, e.started_at, e.ended_at, e.duration_ms, e.sdk_language, e.failure_group_id, a.depth - 1
			FROM executions e
			JOIN ancestors a ON e.execution_id = a.parent_execution_id
			WHERE e.project_id = ? AND a.depth > ?
		),
		descendants(execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, depth) AS (
			SELECT execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, 0
			FROM executions
			WHERE execution_id = ? AND project_id = ?
			UNION ALL
			SELECT e.execution_id, e.parent_execution_id, e.status, e.started_at, e.ended_at, e.duration_ms, e.sdk_language, e.failure_group_id, d.depth + 1
			FROM executions e
			JOIN descendants d ON e.parent_execution_id = d.execution_id
			WHERE e.project_id = ? AND d.depth < ?
		)
		SELECT execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, depth FROM ancestors
		UNION
		SELECT execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, depth FROM descendants
		ORDER BY depth ASC, started_at ASC
	`
	// Parameter 4 is the depth FLOOR for the ancestors CTE (a
	// negative value); parameter 8 is the depth CEILING for the
	// descendants CTE (positive). Negation happens in Go so the
	// query stays portable between SQLite and Postgres (Postgres
	// can't infer the type of a unary-minus on a placeholder).
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

// FindFirstFailedValidator returns the validator name from the first
// (lowest-sequence) validator_result event with payload.passed = false
// for the given execution. JSON1 boolean comparison: SQLite stores
// JSON booleans as `true`/`false` text, so we compare against the
// JSON-equivalent value json('false').
func (s *SQLiteStore) FindFirstFailedValidator(
	ctx context.Context,
	executionID string,
) (string, error) {
	var name sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT json_extract(payload, '$.name')
		FROM events
		WHERE execution_id = ?
		  AND event_type = 'validator_result'
		  AND json_extract(payload, '$.passed') = 0
		ORDER BY sequence ASC
		LIMIT 1
	`, executionID).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find first failed validator: %w", err)
	}
	if !name.Valid {
		return "", nil
	}
	return name.String, nil
}

// GroupValidatorFailure upserts a failure_group with
// failure_class=validator_failures and signature=validatorName. Same
// idempotency contract.
func (s *SQLiteStore) GroupValidatorFailure(
	ctx context.Context,
	executionID, projectID, validatorName string,
) (isNew bool, err error) {
	if validatorName == "" {
		return false, fmt.Errorf("validatorName required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassValidator, validatorName)
}

// GroupPromptInjection upserts a failure_group with
// failure_class=prompt_injection and signature=patternName. Detection
// logic (regex pattern matching) lives in detectors/injection.go;
// this method just records the classification.
func (s *SQLiteStore) GroupPromptInjection(
	ctx context.Context,
	executionID, projectID, patternName string,
) (isNew bool, err error) {
	if patternName == "" {
		return false, fmt.Errorf("patternName required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassInjection, patternName)
}

// costVelocityThresholdUSD is the absolute cost threshold at which an
// execution is flagged as cost_velocity. Artificially low for v0.0.1
// demo visibility, production would either raise this OR move to a
// baseline-relative detector (Phase 5+).
const costVelocityThresholdUSD = 0.001

// CostVelocitySignature buckets execution cost into order-of-magnitude
// signatures so high-cost runs cluster sensibly. The lowest bucket
// (cost_$0.001+) matches the threshold; anything cheaper is filtered
// upstream in the handler.
func CostVelocitySignature(costUSD float64) string {
	switch {
	case costUSD < 0.01:
		return "cost_$0.001+"
	case costUSD < 0.10:
		return "cost_$0.01+"
	case costUSD < 1.00:
		return "cost_$0.10+"
	case costUSD < 10.00:
		return "cost_$1+"
	default:
		return "cost_$10+"
	}
}

// GroupCostVelocity upserts a failure_group with
// failure_class=cost_velocity and a cost-bucketed signature. Same
// idempotency contract, if the execution is already in a higher-
// priority group (crash, loop, tool/validator failure), this is a
// no-op.
func (s *SQLiteStore) GroupCostVelocity(
	ctx context.Context,
	executionID, projectID string,
	costUSD float64,
) (isNew bool, err error) {
	if costUSD < costVelocityThresholdUSD {
		return false, nil
	}
	signature := CostVelocitySignature(costUSD)
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassCostVelocity, signature)
}

// GroupIdenticalCallLoop upserts a failure_group with
// failure_class=loops and signature="identical_call_<callHash>".
// callHash is computed in the handler from (model + user_message) and
// truncated to a short hex prefix. Same idempotency contract.
func (s *SQLiteStore) GroupIdenticalCallLoop(
	ctx context.Context,
	executionID, projectID, callHash string,
) (isNew bool, err error) {
	if callHash == "" {
		return false, fmt.Errorf("callHash required")
	}
	signature := "identical_call_" + callHash
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassLoops, signature)
}

// GroupSimilarCallLoop upserts a failure_group with
// failure_class=loops and signature="similar_call_<callHash>".
// callHash is computed in the handler as a hash of the dominant
// trigrams in the cluster, different stuck-pattern clusters get
// different signatures so they aggregate as distinct rows in the
// dashboard.
func (s *SQLiteStore) GroupSimilarCallLoop(
	ctx context.Context,
	executionID, projectID, callHash string,
) (isNew bool, err error) {
	if callHash == "" {
		return false, fmt.Errorf("callHash required")
	}
	signature := "similar_call_" + callHash
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassLoops, signature)
}

// ListModelsForExecution returns the distinct set of model names from
// this execution's llm_call events, sorted alphabetically. Uses SQLite
// JSON1's json_extract to read payload.model. Returns empty slice (not
// nil) if no llm_call events have a model field.
func (s *SQLiteStore) ListModelsForExecution(
	ctx context.Context,
	executionID string,
) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT json_extract(payload, '$.model') AS model
		FROM events
		WHERE execution_id = ?
		  AND event_type = 'llm_call'
		  AND json_extract(payload, '$.model') IS NOT NULL
		  AND json_extract(payload, '$.model') != ''
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

// ListModelsForProjectSince returns distinct models seen in this
// project's llm_call events since the cutoff, EXCLUDING the given
// execution. Used by the drift detector to compute the historical
// model-mix baseline. Joins events ↔ executions on execution_id to
// scope by project; an indexed query on a hot path, but llm_call
// volume is modest enough at MVP scale that the join cost is fine.
func (s *SQLiteStore) ListModelsForProjectSince(
	ctx context.Context,
	projectID string,
	cutoff time.Time,
	excludeExecutionID string,
) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT json_extract(e.payload, '$.model') AS model
		FROM events e
		JOIN executions x ON x.execution_id = e.execution_id
		WHERE x.project_id = ?
		  AND e.event_type = 'llm_call'
		  AND e.timestamp >= ?
		  AND e.execution_id != ?
		  AND json_extract(e.payload, '$.model') IS NOT NULL
		  AND json_extract(e.payload, '$.model') != ''
		ORDER BY model ASC
	`, projectID, cutoff.UTC().Format(time.RFC3339), excludeExecutionID)
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

// GroupDriftSignal upserts a failure_group with failure_class=drift
// and the caller-supplied signature. Same idempotency contract as the
// other groupers, if the execution is already in a higher-priority
// group (crash, injection), this is a no-op.
func (s *SQLiteStore) GroupDriftSignal(
	ctx context.Context,
	executionID, projectID, signature string,
) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("drift signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassDrift, signature)
}

// ListLLMUserMessagesForExecution returns user_messages from this
// execution's llm_call events, in sequence order. Empty / NULL
// user_messages are filtered out, they don't contribute lexical
// signal.
func (s *SQLiteStore) ListLLMUserMessagesForExecution(
	ctx context.Context,
	executionID string,
) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT json_extract(payload, '$.user_message') AS user_message
		FROM events
		WHERE execution_id = ?
		  AND event_type = 'llm_call'
		  AND json_extract(payload, '$.user_message') IS NOT NULL
		  AND json_extract(payload, '$.user_message') != ''
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

// ListLLMUserMessagesForProjectSince returns user_messages from every
// llm_call event in this project's history since cutoff, excluding
// the current execution. Sorted by timestamp DESC (most recent first)
// so when callers apply a limit, they get the freshest signal.
//
// Bounded by limit. Pass 0 for "no limit", typically callers should
// pass 500 or 1000 for v0.0.1; once we have a project-volume signal,
// the limit becomes adaptive.
func (s *SQLiteStore) ListLLMUserMessagesForProjectSince(
	ctx context.Context,
	projectID string,
	cutoff time.Time,
	excludeExecutionID string,
	limit int,
) ([]string, error) {
	query := `
		SELECT json_extract(e.payload, '$.user_message') AS user_message
		FROM events e
		JOIN executions x ON x.execution_id = e.execution_id
		WHERE x.project_id = ?
		  AND e.event_type = 'llm_call'
		  AND e.timestamp >= ?
		  AND e.execution_id != ?
		  AND json_extract(e.payload, '$.user_message') IS NOT NULL
		  AND json_extract(e.payload, '$.user_message') != ''
		ORDER BY e.timestamp DESC
	`
	args := []interface{}{projectID, cutoff.UTC().Format(time.RFC3339), excludeExecutionID}
	if limit > 0 {
		query += " LIMIT ?"
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

// ListFailureGroups returns failure_groups for a project, sorted by
// most-recent first. Caller is responsible for sensible limit/offset
// bounds (handler enforces a max-limit ceiling).
//
// cost_wasted_usd is computed live as SUM(executions.estimated_cost_usd)
// across all executions linked to the group. The stored
// failure_groups.cost_wasted_usd column is currently unused, kept for
// a future "manual override / human-adjusted" path. For now the
// computed sum always wins.
func (s *SQLiteStore) ListFailureGroups(
	ctx context.Context,
	projectID string,
	limit, offset int,
) ([]*FailureGroup, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			fg.group_id, fg.project_id, fg.failure_class, fg.signature,
			fg.first_seen, fg.last_seen,
			fg.event_count, fg.affected_executions,
			COALESCE(SUM(e.estimated_cost_usd), 0) AS computed_cost,
			fg.sample_execution_id,
			fg.analysis_markdown, fg.analyzed_at, fg.analysis_model
		FROM failure_groups fg
		LEFT JOIN executions e ON e.failure_group_id = fg.group_id
		WHERE fg.project_id = ?
		GROUP BY fg.group_id
		ORDER BY fg.last_seen DESC
		LIMIT ? OFFSET ?
	`, projectID, limit, offset)
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

// GetFailureGroup returns a single failure_group by its deterministic id.
// Same cost-computation path as ListFailureGroups.
func (s *SQLiteStore) GetFailureGroup(
	ctx context.Context,
	groupID string,
) (*FailureGroup, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT
			fg.group_id, fg.project_id, fg.failure_class, fg.signature,
			fg.first_seen, fg.last_seen,
			fg.event_count, fg.affected_executions,
			COALESCE(SUM(e.estimated_cost_usd), 0) AS computed_cost,
			fg.sample_execution_id,
			fg.analysis_markdown, fg.analyzed_at, fg.analysis_model
		FROM failure_groups fg
		LEFT JOIN executions e ON e.failure_group_id = fg.group_id
		WHERE fg.group_id = ?
		GROUP BY fg.group_id
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

// GetFailureGroupByClassSignature returns a failure_group by its
// natural key (project_id, failure_class, signature). Used by the
// webhook dispatcher to fetch the canonical sample_execution_id for
// the payload at first-occurrence time.
func (s *SQLiteStore) GetFailureGroupByClassSignature(
	ctx context.Context,
	projectID, failureClass, signature string,
) (*FailureGroup, error) {
	groupID := deriveGroupID(projectID, failureClass, signature)
	return s.GetFailureGroup(ctx, groupID)
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows, letting
// scanFailureGroup serve both single-row and iteration paths.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanFailureGroup(r rowScanner) (*FailureGroup, error) {
	var (
		g                FailureGroup
		firstSeen        string
		lastSeen         string
		costWasted       sql.NullFloat64
		sampleID         sql.NullString
		analysisMarkdown sql.NullString
		analyzedAt       sql.NullTime
		analysisModel    sql.NullString
	)
	if err := r.Scan(
		&g.GroupID,
		&g.ProjectID,
		&g.FailureClass,
		&g.Signature,
		&firstSeen,
		&lastSeen,
		&g.EventCount,
		&g.AffectedExecutions,
		&costWasted,
		&sampleID,
		&analysisMarkdown,
		&analyzedAt,
		&analysisModel,
	); err != nil {
		return nil, err
	}
	g.FirstSeen, _ = time.Parse(time.RFC3339, firstSeen)
	g.LastSeen, _ = time.Parse(time.RFC3339, lastSeen)
	if costWasted.Valid && costWasted.Float64 > 0 {
		// Only surface a positive computed cost. The COALESCE on the
		// SQL side makes Valid always true, so this prevents zero
		// values from leaking into the JSON as "cost_wasted_usd: 0"
		// when there's no actual cost to show.
		v := costWasted.Float64
		g.CostWastedUSD = &v
	}
	if sampleID.Valid {
		g.SampleExecutionID = sampleID.String
	}
	if analysisMarkdown.Valid && analysisMarkdown.String != "" {
		v := analysisMarkdown.String
		g.AnalysisMarkdown = &v
	}
	if analyzedAt.Valid {
		t := analyzedAt.Time
		g.AnalyzedAt = &t
	}
	if analysisModel.Valid && analysisModel.String != "" {
		v := analysisModel.String
		g.AnalysisModel = &v
	}
	return &g, nil
}

// SaveFailureGroupAnalysis persists the LLM-generated root-cause
// analysis on a failure_group row (Mesedi #27). Idempotent overwrite:
// repeated calls replace the previous analysis. Returns ErrNotFound
// when the group_id does not exist.
func (s *SQLiteStore) SaveFailureGroupAnalysis(
	ctx context.Context,
	groupID, analysisMarkdown, analysisModel string,
	analyzedAt time.Time,
) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE failure_groups
		SET analysis_markdown = ?,
		    analyzed_at       = ?,
		    analysis_model    = ?
		WHERE group_id = ?
	`, analysisMarkdown, analyzedAt, analysisModel, groupID)
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
// tenant_id (legacy unbackfilled row). For all other projects the
// canonical query is CountAIAnalysesByTenantSince.
func (s *SQLiteStore) CountAIAnalysesSincePeriodStart(
	ctx context.Context, projectID string, since time.Time,
) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM failure_groups
		WHERE project_id = ?
		  AND analyzed_at IS NOT NULL
		  AND analyzed_at >= ?
	`, projectID, since).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count ai analyses since: %w", err)
	}
	return count, nil
}

// ListAIAnalysesUsageByProject returns per-project AI analysis
// counts for the admin breakdown view (#197). One row per project
// with at least one analyzed failure_group since `since`. Sorted
// by count descending. The join hits failure_groups (small table)
// and projects (small table); ungroupable without a project_id
// because we LEFT JOIN projects to surface name/owner/tier even
// when failure_groups.project_id no longer resolves (shouldn't
// happen given FK constraints but guards us if it ever does).
func (s *SQLiteStore) ListAIAnalysesUsageByProject(
	ctx context.Context, since time.Time,
) ([]*AIAnalysesByProjectRow, error) {
	// #211 filter chips: group_concat(DISTINCT) returns the comma-
	// joined list of failure_class slugs this project ran analyses
	// against in the window. Empty string when none (shouldn't
	// happen given the WHERE clause but defensive). Frontend splits
	// the CSV.
	rows, err := s.db.QueryContext(ctx, `
		SELECT fg.project_id, p.name, p.owner_email, p.tier, p.tenant_id,
		       COUNT(*) AS n,
		       group_concat(DISTINCT fg.failure_class) AS classes
		FROM failure_groups fg
		JOIN projects p ON p.project_id = fg.project_id
		WHERE fg.analyzed_at IS NOT NULL
		  AND fg.analyzed_at >= ?
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

// splitFailureClassesCSV parses the comma-delimited string from
// group_concat / string_agg into a deduped + non-empty slice. Same
// helper used by both sqlite + postgres so the slice contract is
// identical regardless of driver.
func splitFailureClassesCSV(s sql.NullString) []string {
	if !s.Valid || s.String == "" {
		return nil
	}
	parts := strings.Split(s.String, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// ListAnalyzedFailureGroupsByProject returns one row per failure
// group on the given project that has been analyzed (analyzed_at
// IS NOT NULL) on or after `since`. Used by the admin AI analyses
// breakdown (#211) so the founder can click a project row and see
// WHICH failure groups generated the count, not just the total.
// Ordered analyzed_at DESC so the most recent analysis lands at
// the top. Default limit 200 covers a heavy month even for the
// largest projected customer; callers can pass 0 to use the default.
//
// Column order MUST match scanFailureGroup so the canonical helper
// can do the string -> time.Time parse for first_seen / last_seen.
// Those columns are stored as TEXT on the Postgres side (#79 latent
// scan-bug fix); scanning directly into time.Time blew up the live
// driver with "storing driver.Value type string into type *time.Time".
func (s *SQLiteStore) ListAnalyzedFailureGroupsByProject(
	ctx context.Context, projectID string, since time.Time, limit int,
) ([]*FailureGroup, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT group_id, project_id, failure_class, signature,
		       first_seen, last_seen, event_count, affected_executions,
		       cost_wasted_usd, sample_execution_id,
		       analysis_markdown, analyzed_at, analysis_model
		FROM failure_groups
		WHERE project_id = ?
		  AND analyzed_at IS NOT NULL
		  AND analyzed_at >= ?
		ORDER BY analyzed_at DESC
		LIMIT ?
	`, projectID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("list analyzed failure groups: %w", err)
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
// every project owned by tenantID whose analyzed_at >= since. This
// is the canonical Team-tier rate-limit query because the cap is
// per-organization per-period, not per-project; without the JOIN
// here a Team customer could trivially bypass the cap by spawning
// additional projects under the same org.
func (s *SQLiteStore) CountAIAnalysesByTenantSince(
	ctx context.Context, tenantID string, since time.Time,
) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM failure_groups fg
		JOIN projects p ON p.project_id = fg.project_id
		WHERE p.tenant_id = ?
		  AND fg.analyzed_at IS NOT NULL
		  AND fg.analyzed_at >= ?
	`, tenantID, since).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count ai analyses by tenant since: %w", err)
	}
	return count, nil
}

// ---------------------------------------------------------------------
// Abuse signals + project suspension (#172).
// ---------------------------------------------------------------------

// CreateAbuseSignal inserts a new row. Caller sets SignalID and
// DetectedAt; the worker updates the lifecycle columns via the Mark
// methods below.
func (s *SQLiteStore) CreateAbuseSignal(ctx context.Context, sig *AbuseSignal) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO abuse_signals
		    (signal_id, project_id, kind, severity, detail, detected_at)
		VALUES (?, ?, ?, ?, ?, ?)
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

// ListAbuseSignals returns signals sorted by detected_at DESC. When
// unresolvedOnly is true the WHERE clause restricts to resolved_at
// IS NULL.
func (s *SQLiteStore) ListAbuseSignals(ctx context.Context, unresolvedOnly bool, limit int) ([]*AbuseSignal, error) {
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
	if limit > 0 {
		q += " LIMIT ?"
	}

	var rows *sql.Rows
	var err error
	if limit > 0 {
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

// GetAbuseSignal fetches one row by id.
func (s *SQLiteStore) GetAbuseSignal(ctx context.Context, signalID string) (*AbuseSignal, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT signal_id, project_id, kind, severity, detail,
		       detected_at, notified_at, suspended_at,
		       resolved_at, resolved_by, resolution_note
		FROM abuse_signals WHERE signal_id = ?
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

// MarkAbuseSignalNotified stamps notified_at on the row.
func (s *SQLiteStore) MarkAbuseSignalNotified(ctx context.Context, signalID string, notifiedAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE abuse_signals SET notified_at = ? WHERE signal_id = ?`,
		notifiedAt.Unix(), signalID,
	)
	return err
}

// MarkAbuseSignalSuspended stamps suspended_at on the signal row AND
// flips projects.suspended_at + suspension_reason in the same
// transaction. The auth middleware's IsProjectSuspended check picks
// up the project flip on the next request.
func (s *SQLiteStore) MarkAbuseSignalSuspended(ctx context.Context, signalID, projectID, reason string, suspendedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE abuse_signals SET suspended_at = ? WHERE signal_id = ?`,
		suspendedAt.Unix(), signalID,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE projects SET suspended_at = ?, suspension_reason = ? WHERE project_id = ?`,
		suspendedAt.Unix(), reason, projectID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// ResolveAbuseSignal stamps the resolution columns. Does NOT touch
// projects.suspended_at; the caller is responsible for calling
// UnsuspendProject if reactivation is desired.
func (s *SQLiteStore) ResolveAbuseSignal(ctx context.Context, signalID, resolvedBy, note string, resolvedAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE abuse_signals
		   SET resolved_at = ?, resolved_by = ?, resolution_note = ?
		 WHERE signal_id = ?`,
		resolvedAt.Unix(), resolvedBy, note, signalID,
	)
	return err
}

// UnsuspendProject clears suspended_at + suspension_reason.
func (s *SQLiteStore) UnsuspendProject(ctx context.Context, projectID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE projects SET suspended_at = NULL, suspension_reason = NULL WHERE project_id = ?`,
		projectID,
	)
	return err
}

// IsProjectSuspended is the hot-path check for the auth middleware.
// Returns (false, "", nil) if active, (true, reason, nil) if not.
func (s *SQLiteStore) IsProjectSuspended(ctx context.Context, projectID string) (bool, string, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT suspended_at, suspension_reason FROM projects WHERE project_id = ?`,
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
