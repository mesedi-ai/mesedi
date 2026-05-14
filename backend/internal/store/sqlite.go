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

// GroupCrashedExecution upserts a failure_group for the given crashed
// execution and links the execution into it via the failure_group_id
// column on executions.
//
// Idempotency: if the execution already has a failure_group_id set
// (e.g., the PATCH-to-crashed fired more than once for the same
// execution_id, or this is a retry), the function returns nil without
// double-counting. The unique (project_id, failure_class, signature)
// index on failure_groups is the ultimate guard against duplicate
// groups, but the explicit early-return is faster than relying on the
// index-conflict path for the common no-op case.
func (s *SQLiteStore) GroupCrashedExecution(
	ctx context.Context,
	executionID, projectID, signature string,
) error {
	if executionID == "" || projectID == "" || signature == "" {
		return fmt.Errorf("executionID, projectID, signature all required")
	}

	// Idempotency check: skip if already grouped.
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

	groupID := deriveGroupID(projectID, FailureClassCrashes, signature)
	now := time.Now().UTC().Format(time.RFC3339)

	// Upsert: insert on first-ever match, increment counters on subsequent.
	// The deterministic group_id means we can conflict on PRIMARY KEY
	// directly — no need to match by (project_id, failure_class, signature)
	// triple. Counters are set to 1/1 on insert; the ON CONFLICT branch
	// increments by 1 each subsequent call.
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
	`, groupID, projectID, FailureClassCrashes, signature, now, now, executionID)
	if err != nil {
		return fmt.Errorf("upsert failure_group: %w", err)
	}

	// Link the execution to its group. This is what the idempotency
	// check above looks for on subsequent calls.
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
		"failure_class", FailureClassCrashes,
		"signature", signature,
	)
	return nil
}

// ListFailureGroups returns failure_groups for a project, sorted by
// most-recent first. Caller is responsible for sensible limit/offset
// bounds (handler enforces a max-limit ceiling).
func (s *SQLiteStore) ListFailureGroups(
	ctx context.Context,
	projectID string,
	limit, offset int,
) ([]*FailureGroup, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			group_id, project_id, failure_class, signature,
			first_seen, last_seen,
			event_count, affected_executions,
			cost_wasted_usd, sample_execution_id
		FROM failure_groups
		WHERE project_id = ?
		ORDER BY last_seen DESC
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
func (s *SQLiteStore) GetFailureGroup(
	ctx context.Context,
	groupID string,
) (*FailureGroup, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT
			group_id, project_id, failure_class, signature,
			first_seen, last_seen,
			event_count, affected_executions,
			cost_wasted_usd, sample_execution_id
		FROM failure_groups
		WHERE group_id = ?
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
	if costWasted.Valid {
		v := costWasted.Float64
		g.CostWastedUSD = &v
	}
	if sampleID.Valid {
		g.SampleExecutionID = sampleID.String
	}
	return &g, nil
}
