// Readiness reporting: does the connected database actually work, and
// does its schema match the binary that is talking to it?
//
// WHY THIS EXISTS
// Until 2026-08-27 the API's /health endpoint returned a hardcoded
// {"ok":true} and never touched the database. Store.Ping was
// implemented for both engines and carried a comment saying "Used by
// /health"; the only caller in the entire repo was a standalone smoke
// CLI. The API server never called it.
//
// The consequence is the same shape as every other problem found that
// day: a check that reports success without checking anything. Three
// external uptime monitors stayed green through a database outage,
// because the endpoint they poll cannot fail while the process is
// alive.
//
// Ping alone would not have been enough either. A failed or partially
// applied migration leaves the socket perfectly answerable while every
// write fails, which is exactly what happened with migration 056. So
// readiness compares the number of migrations compiled into this
// binary against the number the database has recorded.

package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strings"
)

// SchemaStatus reports the migration counts on both sides of the
// connection.
type SchemaStatus struct {
	// Embedded is how many migration files this binary was built with.
	Embedded int
	// Applied is how many rows schema_migrations currently holds.
	Applied int
}

// Behind reports whether the database is missing migrations this binary
// expects.
//
// The comparison is deliberately one-sided. Applied > Embedded is NOT a
// failure: during a rolling deploy a new machine applies migration N+1
// while an old machine is still serving, and that old binary is
// perfectly healthy, it simply predates the new migration. Failing
// readiness there would pull working machines out of rotation in the
// middle of every deploy.
//
// Applied < Embedded is the real problem. It means this binary shipped
// with migrations the database never took, so it will issue queries
// against columns and tables that do not exist.
func (s SchemaStatus) Behind() bool {
	return s.Applied < s.Embedded
}

// countEmbeddedMigrations counts .sql files in an embedded migrations
// directory. Shared by both engines so the two counts can never drift
// apart through a copy-paste edit to one of them.
func countEmbeddedMigrations(fsys embed.FS, dir string) (int, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", dir, err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			n++
		}
	}
	if n == 0 {
		// A zero count would make Behind() permanently false and turn
		// this whole check into another endpoint that cannot fail,
		// which is the bug being fixed. Fail loudly instead.
		return 0, fmt.Errorf("no .sql files embedded under %s", dir)
	}
	return n, nil
}

// SchemaStatus reports embedded vs applied migration counts for Postgres.
func (s *PostgresStore) SchemaStatus(ctx context.Context) (SchemaStatus, error) {
	embedded, err := countEmbeddedMigrations(migrationsPostgresFS, "migrations-postgres")
	if err != nil {
		return SchemaStatus{}, err
	}
	var applied int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations`,
	).Scan(&applied); err != nil {
		return SchemaStatus{}, fmt.Errorf("count applied migrations: %w", err)
	}
	return SchemaStatus{Embedded: embedded, Applied: applied}, nil
}

// SchemaStatus reports embedded vs applied migration counts for SQLite.
//
// Present for parity rather than for production: self-hosted installs
// run this engine and deserve the same readiness signal. Writing only
// the Postgres half is how the migration 056 outage happened.
func (s *SQLiteStore) SchemaStatus(ctx context.Context) (SchemaStatus, error) {
	embedded, err := countEmbeddedMigrations(migrationsFS, "migrations")
	if err != nil {
		return SchemaStatus{}, err
	}
	var applied int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations`,
	).Scan(&applied); err != nil {
		return SchemaStatus{}, fmt.Errorf("count applied migrations: %w", err)
	}
	return SchemaStatus{Embedded: embedded, Applied: applied}, nil
}
