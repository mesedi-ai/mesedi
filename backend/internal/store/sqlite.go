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
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // SQLite driver registers under name "sqlite"
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
	db, err := sql.Open("sqlite", withSQLiteTimeFormat(dsn))
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

// Ping verifies the database is reachable. Called by the /ready
// readiness probe (cmd/api/ready.go). The "(eventually)" that used to
// end this line was accurate and nobody read it: /health never called
// this at all. Wired up for real on 2026-08-27.
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
// Three constructs carry semicolons that do NOT terminate a statement,
// and all three arrived with the checkpoint-permanence migration:
// single-quoted string literals, Postgres dollar-quoted bodies
// ($tag$ ... $tag$), and SQLite trigger bodies (CREATE TRIGGER ...
// BEGIN ... END), whose inner statements each end with a semicolon of
// their own. The scanner below tracks all three so those semicolons
// stay inside their statement.
//
// Remaining limitation: `--` inside a string literal would still be
// taken for a comment, because comment stripping runs before the
// quote-aware pass. No migration puts `--` in a string; keep it that
// way, or move the comment stripping into the scanner.
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

	// Pass 2: scan for semicolons that actually end a statement.
	out := make([]string, 0, 4)
	var cur strings.Builder
	inQuote := false // inside a '...' literal ('' escapes toggle twice, harmlessly)
	dollarTag := ""  // the opening $tag$ while inside a dollar-quoted body
	for i := 0; i < len(cleanedBody); i++ {
		c := cleanedBody[i]
		if inQuote {
			cur.WriteByte(c)
			if c == '\'' {
				inQuote = false
			}
			continue
		}
		if dollarTag != "" {
			if c == '$' && strings.HasPrefix(cleanedBody[i:], dollarTag) {
				cur.WriteString(dollarTag)
				i += len(dollarTag) - 1
				dollarTag = ""
			} else {
				cur.WriteByte(c)
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
			cur.WriteByte(c)
		case '$':
			if tag, ok := dollarQuoteTagAt(cleanedBody, i); ok {
				dollarTag = tag
				cur.WriteString(tag)
				i += len(tag) - 1
			} else {
				cur.WriteByte(c)
			}
		case ';':
			if triggerBodyStillOpen(cur.String()) {
				cur.WriteByte(c)
				continue
			}
			if stmt := strings.TrimSpace(cur.String()); stmt != "" {
				out = append(out, stmt)
			}
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if stmt := strings.TrimSpace(cur.String()); stmt != "" {
		out = append(out, stmt)
	}
	return out
}

// dollarQuoteTagAt reports whether s[i:] opens a Postgres dollar-quote
// tag ($$, $fn$, ...) and returns the full tag including both dollar
// signs. A lone $ (or $1-style text that never closes with a second $)
// is not a tag.
func dollarQuoteTagAt(s string, i int) (string, bool) {
	j := i + 1
	for j < len(s) {
		c := s[j]
		if c == '$' {
			return s[i : j+1], true
		}
		isTagChar := c == '_' ||
			('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') ||
			('0' <= c && c <= '9')
		if !isTagChar {
			return "", false
		}
		j++
	}
	return "", false
}

// triggerBodyStillOpen reports whether stmt is a CREATE TRIGGER whose
// BEGIN ... END body has not closed yet, in which case a semicolon at
// this point ends a statement INSIDE the body, not the trigger itself.
// String literals are blanked before counting so a BEGIN or END inside
// a message string cannot skew the balance.
func triggerBodyStillOpen(stmt string) bool {
	blanked := make([]byte, len(stmt))
	inQ := false
	for i := 0; i < len(stmt); i++ {
		c := stmt[i]
		if c == '\'' {
			inQ = !inQ
			c = ' '
		} else if inQ {
			c = ' '
		}
		blanked[i] = c
	}
	fields := strings.Fields(strings.ToUpper(string(blanked)))
	if len(fields) == 0 || fields[0] != "CREATE" {
		return false
	}
	// TRIGGER must appear in the head: CREATE [TEMP|TEMPORARY|OR
	// REPLACE] TRIGGER. Postgres CREATE TRIGGER has no inline body,
	// so its BEGIN/END count stays 0/0 and it falls through below.
	isTrigger := false
	for k := 1; k < len(fields) && k <= 3; k++ {
		if fields[k] == "TRIGGER" {
			isTrigger = true
			break
		}
	}
	if !isTrigger {
		return false
	}
	begins, ends := 0, 0
	for _, f := range fields {
		switch strings.Trim(f, "();,") {
		case "BEGIN":
			begins++
		case "END":
			ends++
		}
	}
	return begins > ends
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

// The operations themselves live in topical sidecars since the
// 2026-09-12 split: sqlite_projects.go, sqlite_api_keys.go,
// sqlite_webhooks.go, sqlite_executions.go, sqlite_execution_queries.go,
// sqlite_failure_grouping.go, sqlite_detector_tool_queries.go,
// sqlite_detector_llm_queries.go, sqlite_failure_group_records.go and
// sqlite_abuse.go, each with a postgres_ twin. This file keeps the
// type, open/migrate machinery, and the helpers shared across them.

// nullableString wraps an empty string as a SQL NULL so the column
// reads back as NULL rather than the literal empty string. Used by
// the delivery-log writer where most fields are optional.
func nullableString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// ─────────────────────────────────────────────────────────────────────────
// Errors + null helpers
// ─────────────────────────────────────────────────────────────────────────

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// ErrProjectStillActive is returned by GDPR purge paths when
// the caller passes a project_id that still has a row in the
// `projects` table, i.e. the project has not been closed via
// HandleCloseAccount + DeleteProjectCascade. The purge surface refuses
// live projects: customer-initiated audit deletion must follow the
// normal HandleCloseAccount flow first; admin-initiated GDPR purge is
// for already-closed projects only. Pre-the guard counted
// audit_events with project_deleted_at IS NULL, which silently
// bypassed for newly-signed-up projects with no audit history, see
// audit_events.go for the bug history. The handler maps this to
// HTTP 422.
var ErrProjectStillActive = errors.New("project still active; close it before GDPR purge")

// ErrInvalidLifecycleTransition is returned when PauseExecution or
// ResumeExecution is called against an execution that is not in
// the expected prior state. The transition matrix is enforced at
// the store layer (rather than purely in the handler) so the
// invariant holds even if a future caller bypasses the HTTP API
// ().
var ErrInvalidLifecycleTransition = errors.New("invalid lifecycle transition")

// ErrAlreadyAccepted is returned by MarkInviteAccepted when the
// invite row's accepted_at column is already non-NULL. Single-use
// invariant: each invite token can only be redeemed once.
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

// DefaultCostVelocityThresholdUSD is the fallback threshold used when
// the handler cannot read the per-project value (migration 043 +
// HandleSetCostVelocityConfig). $1.00 captures "this execution was
// unusually expensive" without flooding on routine LLM calls (median
// real-world cost sits in the $0.001 - $0.10 range). Was 0.001 in
// v0.0.1 with a self-confessed "production would either raise this
// OR move to baseline-relative (Phase 5+)" comment; Phase 5+ never
// shipped, the floor remained, and every real execution tripped the
// detector. Exported so handlers + tests can reference the single
// source of truth.
const DefaultCostVelocityThresholdUSD = 1.00

// Cost-velocity signatures + grouping moved to costvelocity.go.

// rowScanner is satisfied by both *sql.Row and *sql.Rows, letting
// scanFailureGroup serve both single-row and iteration paths.
type rowScanner interface {
	Scan(dest ...any) error
}
