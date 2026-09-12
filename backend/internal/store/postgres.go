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
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx" driver
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

// Ping verifies the database is reachable. Called by the /ready
// readiness probe (cmd/api/ready.go) and by cmd/mesedi-pg-smoke.
//
// This comment previously said "Used by /health". That was false for
// the entire life of the endpoint: /health returned a hardcoded
// {"ok":true} and never called this, so external uptime monitors could
// not detect a database outage. Corrected 2026-08-27 along with the
// endpoint itself.
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

// The operations themselves live in topical sidecars since the
// 2026-09-12 split: postgres_projects.go, postgres_api_keys.go,
// postgres_webhooks.go, postgres_executions.go,
// postgres_execution_queries.go, postgres_failure_grouping.go,
// postgres_detector_tool_queries.go, postgres_detector_llm_queries.go,
// postgres_failure_group_records.go and postgres_abuse.go, each the
// dialect twin of its sqlite_ counterpart. Note for the query files:
// started_at / ended_at are TIMESTAMPTZ here, not RFC3339 text like in
// SQLite, so the postgres scan helpers read into time.Time directly.
// This file keeps the type and the open/migrate machinery.
//
// Cost-velocity grouping twins moved to postgres_cost_velocity.go.
