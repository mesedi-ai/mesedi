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
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (
			project_id, name, owner_user_id, owner_email, created_at, tier
		)
		VALUES (?, ?, ?, ?, ?, ?)
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
		       granted_executions, granted_executions_expires_at, tier_expires_at
		FROM projects WHERE project_id = ?
	`, projectID).Scan(
		&p.ProjectID, &p.Name, &owner, &email, &p.CreatedAt,
		&p.Tier, &stripeCust, &stripeSub,
		&periodStart, &periodEnd, &p.ExecutionsThisPeriod,
		&p.GrantedExecutions, &grantExpires, &tierExpires,
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
		GROUP BY p.project_id
		ORDER BY p.created_at DESC
	`)
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
// TierHobby/TierPro/TierEnterprise constants.
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
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys (key_id, project_id, key_hash, key_prefix, name, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, k.KeyID, k.ProjectID, k.KeyHash, k.KeyPrefix, nullString(k.Name), k.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert api_key: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetAPIKeyByHash(ctx context.Context, keyHash string) (*APIKey, error) {
	k := &APIKey{}
	var name sql.NullString
	var lastUsed sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT key_id, project_id, key_hash, key_prefix, name, created_at, last_used_at
		FROM api_keys WHERE key_hash = ?
	`, keyHash).Scan(&k.KeyID, &k.ProjectID, &k.KeyHash, &k.KeyPrefix, &name, &k.CreatedAt, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if name.Valid {
		k.Name = name.String
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
	rows, err := s.db.QueryContext(ctx, `
		SELECT key_id, project_id, key_prefix, name, created_at, last_used_at
		FROM api_keys
		WHERE project_id = ?
		ORDER BY created_at DESC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("query api_keys: %w", err)
	}
	defer rows.Close()

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
			&name, &createdAt, &lastUsedAt,
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

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO project_webhooks (
			webhook_id, project_id, name, url, secret,
			enabled_classes, enabled, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`,
		wh.WebhookID, wh.ProjectID, wh.Name, wh.URL, wh.Secret,
		classesJSON, enabled, wh.CreatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("insert project_webhook: %w", err)
	}
	return nil
}

// ListProjectWebhooksForProject returns every webhook for a project,
// sorted newest first. The Secret field is intentionally NOT populated
//, it's only ever surfaced once at creation time, never on list.
func (s *SQLiteStore) ListProjectWebhooksForProject(
	ctx context.Context,
	projectID string,
) ([]*ProjectWebhook, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT webhook_id, project_id, name, url,
		       enabled_classes, enabled, created_at
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
		if err := rows.Scan(
			&wh.WebhookID, &wh.ProjectID, &wh.Name, &wh.URL,
			&classesJSON, &enabled, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan project_webhook: %w", err)
		}
		wh.Enabled = enabled != 0
		wh.EnabledClasses = parseEnabledClasses(classesJSON)
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
		       enabled_classes, enabled, created_at
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
		if err := rows.Scan(
			&wh.WebhookID, &wh.ProjectID, &wh.Name, &wh.URL, &wh.Secret,
			&classesJSON, &enabled, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan project_webhook: %w", err)
		}
		wh.Enabled = enabled != 0
		wh.EnabledClasses = parseEnabledClasses(classesJSON)
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
	err := s.db.QueryRowContext(ctx, `
		SELECT webhook_id, project_id, name, url, secret,
		       enabled_classes, enabled, created_at
		FROM project_webhooks
		WHERE webhook_id = ? AND project_id = ?
	`, webhookID, projectID).Scan(
		&wh.WebhookID, &wh.ProjectID, &wh.Name, &wh.URL, &wh.Secret,
		&classesJSON, &enabled, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get project_webhook: %w", err)
	}
	wh.Enabled = enabled != 0
	wh.EnabledClasses = parseEnabledClasses(classesJSON)
	if t, perr := time.Parse(time.RFC3339, createdAt); perr == nil {
		wh.CreatedAt = t
	}
	return &wh, nil
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
	if limit <= 0 {
		limit = 50
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

	out := make([]*WebhookDelivery, 0, limit)
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
			sdk_version, sdk_language
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		e.ExecutionID, e.ProjectID, nullStringPtr(e.ParentExecutionID), e.Status,
		e.StartedAt, nullTime(e.EndedAt), nullInt64(e.DurationMs),
		nullInt(e.TotalTokensIn), nullInt(e.TotalTokensOut), nullFloat(e.EstimatedCostUSD),
		nullString(e.InputSummary), nullString(e.OutputSummary), nullString(e.CrashSignature),
		nullString(e.SDKVersion), nullString(e.SDKLanguage),
	)
	if err != nil {
		return fmt.Errorf("insert execution: %w", err)
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
	var parent, inputSum, outputSum, crashSig, sdkVer, sdkLang sql.NullString
	var endedAt sql.NullTime
	var durationMs, tokensIn, tokensOut sql.NullInt64
	var costUSD sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `
		SELECT
			execution_id, project_id, parent_execution_id, status,
			started_at, ended_at, duration_ms,
			total_tokens_in, total_tokens_out, estimated_cost_usd,
			input_summary, output_summary, crash_signature,
			sdk_version, sdk_language
		FROM executions WHERE execution_id = ?
	`, executionID).Scan(
		&e.ExecutionID, &e.ProjectID, &parent, &e.Status,
		&e.StartedAt, &endedAt, &durationMs,
		&tokensIn, &tokensOut, &costUSD,
		&inputSum, &outputSum, &crashSig,
		&sdkVer, &sdkLang,
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

// ListExecutionsByFailureGroup returns executions whose failure_group_id
// matches groupID, sorted by started_at DESC. Caller is expected to have
// already verified that the group belongs to the auth context's project
// (this method does not enforce project scoping, the failure_group_id
// column on executions IS already scoped to a project by virtue of the
// failure_groups foreign key, but we never reach into that here).
func (s *SQLiteStore) ListExecutionsByFailureGroup(
	ctx context.Context,
	groupID string,
	limit, offset int,
) ([]*events.Execution, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			execution_id, project_id, status,
			started_at, ended_at,
			duration_ms, total_tokens_in, total_tokens_out,
			estimated_cost_usd, sdk_language, sdk_version, crash_signature
		FROM executions
		WHERE failure_group_id = ?
		ORDER BY started_at DESC
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

	// Idempotency check: skip if already grouped (any class).
	var existing sql.NullString
	err = s.db.QueryRowContext(
		ctx,
		`SELECT failure_group_id FROM executions WHERE execution_id = ?`,
		executionID,
	).Scan(&existing)
	if err == sql.ErrNoRows {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("read execution failure_group_id: %w", err)
	}
	if existing.Valid && existing.String != "" {
		return false, nil // already grouped; no-op (and not new from this caller's perspective)
	}

	groupID := deriveGroupID(projectID, failureClass, signature)

	// Newness probe: does the failure_group already exist? If not, this
	// call is about to create it and we'll report isNew=true to the
	// caller so it can fire webhook escalation. Racy under concurrent
	// writers for the same signature, both observers could see "not
	// found" and both report isNew=true. For v1 with low concurrency the
	// worst case is duplicate webhook deliveries, not data corruption.
	// Production hardening (RETURNING-clause or transactional upsert)
	// is deferred.
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

	now := time.Now().UTC().Format(time.RFC3339)

	// Upsert: insert on first-ever match, increment counters on subsequent.
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

	_, err = s.db.ExecContext(
		ctx,
		`UPDATE executions SET failure_group_id = ? WHERE execution_id = ?`,
		groupID,
		executionID,
	)
	if err != nil {
		return false, fmt.Errorf("link execution to failure_group: %w", err)
	}

	s.logger.Info("execution grouped",
		"execution_id", executionID,
		"failure_group_id", groupID,
		"failure_class", failureClass,
		"signature", signature,
		"is_new_group", isNew,
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
			fg.sample_execution_id
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
			fg.sample_execution_id
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
		g          FailureGroup
		firstSeen  string
		lastSeen   string
		costWasted sql.NullFloat64
		sampleID   sql.NullString
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
	return &g, nil
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
