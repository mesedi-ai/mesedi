// SQLite implementation of the Store interface.
//
// Uses `modernc.org/sqlite` — a pure-Go SQLite driver, no cgo required.
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

		// Read + apply.
		body, err := fs.ReadFile(migrationsFS, path.Join("migrations", name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := s.db.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		s.logger.Info("migration applied", "migration_version", version, "file", name)
	}
	return nil
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
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (project_id, name, owner_user_id, created_at)
		VALUES (?, ?, ?, ?)
	`, p.ProjectID, p.Name, nullString(p.OwnerUserID), p.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert project: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetProject(ctx context.Context, projectID string) (*Project, error) {
	p := &Project{}
	var owner sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT project_id, name, owner_user_id, created_at FROM projects WHERE project_id = ?
	`, projectID).Scan(&p.ProjectID, &p.Name, &owner, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if owner.Valid {
		p.OwnerUserID = owner.String
	}
	return p, nil
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
// returned structs — that field is never serialized to clients or
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
// across tenants). After deletion the key's hash is gone — re-minting
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
	defer tx.Rollback() // safe to call after Commit — becomes a no-op

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
// (this method does not enforce project scoping — the failure_group_id
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
// execution, sorted by sequence ASC (oldest first — matching the order
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
// Phase 3a — Failure groups (crash detection)
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
// produce the same output, across runs and across restarts — so no
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
// are thin wrappers around this — they just supply the appropriate
// failure_class + signature.
//
// Idempotency: if the execution already has a failure_group_id set
// (because it was already linked to a different group, or a previous
// call already linked it to this group), the function returns nil
// without double-counting. This is also how "crash classification
// wins over time-budget overlap" is enforced — the crash grouping
// runs first in the handler, sets failure_group_id, then the
// subsequent time-budget call short-circuits here.
func (s *SQLiteStore) groupExecutionInternal(
	ctx context.Context,
	executionID, projectID, failureClass, signature string,
) error {
	if executionID == "" || projectID == "" || failureClass == "" || signature == "" {
		return fmt.Errorf("executionID, projectID, failureClass, signature all required")
	}

	// Idempotency check: skip if already grouped (any class).
	var existing sql.NullString
	err := s.db.QueryRowContext(
		ctx,
		`SELECT failure_group_id FROM executions WHERE execution_id = ?`,
		executionID,
	).Scan(&existing)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read execution failure_group_id: %w", err)
	}
	if existing.Valid && existing.String != "" {
		return nil // already grouped; no-op
	}

	groupID := deriveGroupID(projectID, failureClass, signature)
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
		return fmt.Errorf("upsert failure_group: %w", err)
	}

	_, err = s.db.ExecContext(
		ctx,
		`UPDATE executions SET failure_group_id = ? WHERE execution_id = ?`,
		groupID,
		executionID,
	)
	if err != nil {
		return fmt.Errorf("link execution to failure_group: %w", err)
	}

	s.logger.Info("execution grouped",
		"execution_id", executionID,
		"failure_group_id", groupID,
		"failure_class", failureClass,
		"signature", signature,
	)
	return nil
}

// GroupCrashedExecution upserts a failure_group with failure_class=crashes
// for the given execution. Thin wrapper around groupExecutionInternal.
func (s *SQLiteStore) GroupCrashedExecution(
	ctx context.Context,
	executionID, projectID, signature string,
) error {
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassCrashes, signature)
}

// timeBudgetThresholdMs is the hardcoded cutoff for "this execution
// took too long" detection in v0.0.1. Set artificially low (1s) for
// local-dev visibility; production default will be 60s (or 10min per
// the concept-doc step-budget detector spec) and configurable per
// project once the projects table gets per-project policy columns.
const timeBudgetThresholdMs int64 = 1000

// timeBudgetSignature returns a coarse duration-bucket label so that
// "long-running executions" cluster into a small number of groups
// rather than one group per unique millisecond. Buckets: 1s+, 10s+,
// 60s+, 10m+, 1h+. Anything below the threshold is filtered upstream
// in the handler; this function assumes a positive duration that has
// already exceeded the threshold.
func timeBudgetSignature(durationMs int64) string {
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
) error {
	signature := timeBudgetSignature(durationMs)
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassLoops, signature)
}

// stepCountSignature buckets event counts so high-step-count executions
// cluster into a small number of groups rather than one group per
// distinct count. Buckets: 10+, 50+, 100+, 500+, 5000+. Anything below
// the threshold is filtered upstream in the handler.
func stepCountSignature(count int) string {
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
// idempotency contract as the other groupers — runs in the handler
// AFTER both crash and time-budget checks, so it's the lowest-priority
// classification of the three.
func (s *SQLiteStore) GroupStepCountExceedance(
	ctx context.Context,
	executionID, projectID string,
	eventCount int,
) error {
	signature := stepCountSignature(eventCount)
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
// contract as the other groupers — if the execution is already linked
// to a higher-priority group (crash, time-budget, step-count), this is
// a no-op.
func (s *SQLiteStore) GroupToolFailure(
	ctx context.Context,
	executionID, projectID, toolName string,
) error {
	if toolName == "" {
		return fmt.Errorf("toolName required")
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
) error {
	if validatorName == "" {
		return fmt.Errorf("validatorName required")
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
) error {
	if patternName == "" {
		return fmt.Errorf("patternName required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassInjection, patternName)
}

// costVelocityThresholdUSD is the absolute cost threshold at which an
// execution is flagged as cost_velocity. Artificially low for v0.0.1
// demo visibility — production would either raise this OR move to a
// baseline-relative detector (Phase 5+).
const costVelocityThresholdUSD = 0.001

// costVelocitySignature buckets execution cost into order-of-magnitude
// signatures so high-cost runs cluster sensibly. The lowest bucket
// (cost_$0.001+) matches the threshold; anything cheaper is filtered
// upstream in the handler.
func costVelocitySignature(costUSD float64) string {
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
// idempotency contract — if the execution is already in a higher-
// priority group (crash, loop, tool/validator failure), this is a
// no-op.
func (s *SQLiteStore) GroupCostVelocity(
	ctx context.Context,
	executionID, projectID string,
	costUSD float64,
) error {
	if costUSD < costVelocityThresholdUSD {
		return nil
	}
	signature := costVelocitySignature(costUSD)
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassCostVelocity, signature)
}

// GroupIdenticalCallLoop upserts a failure_group with
// failure_class=loops and signature="identical_call_<callHash>".
// callHash is computed in the handler from (model + user_message) and
// truncated to a short hex prefix. Same idempotency contract.
func (s *SQLiteStore) GroupIdenticalCallLoop(
	ctx context.Context,
	executionID, projectID, callHash string,
) error {
	if callHash == "" {
		return fmt.Errorf("callHash required")
	}
	signature := "identical_call_" + callHash
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassLoops, signature)
}

// GroupSimilarCallLoop upserts a failure_group with
// failure_class=loops and signature="similar_call_<callHash>".
// callHash is computed in the handler as a hash of the dominant
// trigrams in the cluster — different stuck-pattern clusters get
// different signatures so they aggregate as distinct rows in the
// dashboard.
func (s *SQLiteStore) GroupSimilarCallLoop(
	ctx context.Context,
	executionID, projectID, callHash string,
) error {
	if callHash == "" {
		return fmt.Errorf("callHash required")
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
// other groupers — if the execution is already in a higher-priority
// group (crash, injection), this is a no-op.
func (s *SQLiteStore) GroupDriftSignal(
	ctx context.Context,
	executionID, projectID, signature string,
) error {
	if signature == "" {
		return fmt.Errorf("drift signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassDrift, signature)
}

// ListLLMUserMessagesForExecution returns user_messages from this
// execution's llm_call events, in sequence order. Empty / NULL
// user_messages are filtered out — they don't contribute lexical
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
// Bounded by limit. Pass 0 for "no limit" — typically callers should
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
// failure_groups.cost_wasted_usd column is currently unused — kept for
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

// rowScanner is satisfied by both *sql.Row and *sql.Rows — letting
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
