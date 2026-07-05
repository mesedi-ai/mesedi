// Regression test for : SQLite fresh-install migration sequence
// must apply cleanly from 001 through the latest file in migrations/.
//
// Background: 003_project_webhooks.sql was deleted from the repo
// before its Postgres twin was authored. Development and production
// SQLite databases carried the table forward because they had applied
// 003 long before the deletion, so the gap stayed invisible until
// 's store-test setup tried to spin up a fresh SQLite database
// and faceplanted on migration 011 (which adds severity_routing rows
// that reference project_webhooks).
//
// This test stands up an empty on-disk SQLite database via the public
// OpenSQLite path that customers and CE installs use, and verifies:
//
//   1. OpenSQLite returns no error (the migration sequence completes).
//   2. project_webhooks exists at the end of the run.
//   3. schema_migrations records version=3.
//
// If anyone re-introduces the gap (or breaks another migration) this
// test fails loudly on `go test ./...` before the CE staging image
// gets cut, instead of surfacing as a Docker entrypoint panic during
// a customer install.

package store

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

func TestSQLiteFreshInstallRunsAllMigrations(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fresh.db")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := OpenSQLite(dbPath, logger)
	if err != nil {
		t.Fatalf("OpenSQLite on a fresh db must succeed (fresh-install path); got: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// project_webhooks is what was missing . If 003 was lost
	// again the schema would not have this table.
	var tableName string
	err = s.db.QueryRowContext(
		context.Background(),
		`SELECT name FROM sqlite_master WHERE type='table' AND name='project_webhooks'`,
	).Scan(&tableName)
	if err != nil {
		t.Fatalf("project_webhooks must exist after migrations; got: %v", err)
	}
	if tableName != "project_webhooks" {
		t.Fatalf("expected project_webhooks table; got %q", tableName)
	}

	// schema_migrations should record version 3 specifically — proves
	// the new 003 file was found, applied, and recorded by the runner
	// (and is not just a side-effect of a later migration creating the
	// table inline).
	var version int
	err = s.db.QueryRowContext(
		context.Background(),
		`SELECT version FROM schema_migrations WHERE version = 3`,
	).Scan(&version)
	if err != nil {
		t.Fatalf("schema_migrations must contain version=3; got: %v", err)
	}
	if version != 3 {
		t.Fatalf("expected schema_migrations.version=3; got %d", version)
	}
}
