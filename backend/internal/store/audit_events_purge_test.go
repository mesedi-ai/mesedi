// Regression tests for PurgeAuditEventsForClosedProject's live-project
// guard. The bug that motivated these tests (task #231) was discovered
// on 2026-06-15 during the #225 Step 3.d.1 smoke test against a brand-
// new test project: the previous guard counted audit_events rows with
// project_deleted_at IS NULL and bypassed for any project that had
// zero audit history yet, allowing a destructive purge against a live
// customer with no API activity. The new guard checks the projects
// table directly — if a row exists for project_id, the project is
// still active and the purge must refuse with ErrProjectStillActive.
//
// These tests stand up a minimal in-memory schema (just projects +
// audit_events) rather than running OpenSQLite's full migration
// sequence. That keeps the test scoped to the guard's semantics and
// also avoids tripping over a separate latent bug — task #233 — where
// SQLite migrations/ is missing 003_project_webhooks.sql and any
// fresh SQLite install panics during applyMigrations.
package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// openMinimalAuditPurgeStore creates a SQLite store wrapping a fresh
// on-disk DB that contains only the projects + audit_events tables —
// the bare minimum PurgeAuditEventsForClosedProject reads from. Avoids
// the broken full-migration path (#233) entirely.
func openMinimalAuditPurgeStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "test.db") +
		"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(off)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	// Minimal schema: projects (the source of truth the new guard
	// queries) + audit_events shaped to match migration 031's final
	// state (project_deleted_at column included, no FK to projects).
	stmts := []string{
		`CREATE TABLE projects (
			project_id    TEXT PRIMARY KEY,
			name          TEXT NOT NULL,
			owner_user_id TEXT,
			created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE audit_events (
			event_id              TEXT PRIMARY KEY,
			project_id            TEXT NOT NULL,
			actor_key_id          TEXT,
			actor_key_name        TEXT,
			actor_email           TEXT,
			action                TEXT NOT NULL,
			target_type           TEXT,
			target_id             TEXT,
			metadata_json         TEXT,
			created_at            TIMESTAMP NOT NULL,
			project_name_snapshot TEXT,
			project_deleted_at    TIMESTAMP
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return &SQLiteStore{db: db}
}

// Regression for #231: a brand-new live project with ZERO audit_events
// rows must still be refused by PurgeAuditEventsForClosedProject. The
// pre-fix guard counted audit_events WHERE project_deleted_at IS NULL
// and bypassed when that count was 0; the fix checks the projects
// table directly.
func Test_PurgeAuditEventsForClosedProject_LiveProjectWithZeroAudits_Refuses(t *testing.T) {
	ctx := context.Background()
	st := openMinimalAuditPurgeStore(t)

	// Insert a live project. No audit_events at all (the bug case).
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO projects (project_id, name, created_at) VALUES (?, ?, ?)`,
		"proj-live-zero-audits",
		"Live project with no API usage yet",
		time.Now().UTC(),
	); err != nil {
		t.Fatalf("seed live project: %v", err)
	}

	deleted, err := st.PurgeAuditEventsForClosedProject(ctx, "proj-live-zero-audits")
	if !errors.Is(err, ErrProjectStillActive) {
		t.Fatalf("want ErrProjectStillActive, got err=%v deleted=%d", err, deleted)
	}
	if deleted != 0 {
		t.Errorf("deleted=%d on refused purge, want 0", deleted)
	}
}

// Sanity: a project that does not exist in the projects table (the
// closed-project case after DeleteProjectCascade ran) can be purged.
// Returns 0 rows deleted when there are no surviving audit_events,
// which is fine and idempotent.
func Test_PurgeAuditEventsForClosedProject_ClosedProjectWithZeroAudits_Succeeds(t *testing.T) {
	ctx := context.Background()
	st := openMinimalAuditPurgeStore(t)

	// No INSERT into projects — the project has been closed
	// (DeleteProjectCascade removed the row). No audit_events either.
	deleted, err := st.PurgeAuditEventsForClosedProject(ctx, "proj-already-closed")
	if err != nil {
		t.Fatalf("purge closed project: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted=%d on no-rows purge, want 0", deleted)
	}
}

// A live project that ALSO has audit_events must still refuse. This
// covers the "happy" pre-bug case: when API usage was non-zero the old
// guard happened to fire for the right reason, even though the
// underlying logic was wrong.
func Test_PurgeAuditEventsForClosedProject_LiveProjectWithAudits_Refuses(t *testing.T) {
	ctx := context.Background()
	st := openMinimalAuditPurgeStore(t)

	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO projects (project_id, name, created_at) VALUES (?, ?, ?)`,
		"proj-live-with-audits",
		"Live project with audit history",
		time.Now().UTC(),
	); err != nil {
		t.Fatalf("seed live project: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `
		INSERT INTO audit_events (event_id, project_id, action, created_at)
		VALUES (?, ?, ?, ?)
	`,
		"evt-1", "proj-live-with-audits", "api_key.created", time.Now().UTC(),
	); err != nil {
		t.Fatalf("seed audit event: %v", err)
	}

	deleted, err := st.PurgeAuditEventsForClosedProject(ctx, "proj-live-with-audits")
	if !errors.Is(err, ErrProjectStillActive) {
		t.Fatalf("want ErrProjectStillActive, got err=%v deleted=%d", err, deleted)
	}
	if deleted != 0 {
		t.Errorf("deleted=%d on refused purge, want 0", deleted)
	}
}

// Happy path: a closed project with surviving audit_events (the
// post-close, pre-GDPR-purge state) gets its rows hard-deleted and
// the returned count matches what was on disk.
func Test_PurgeAuditEventsForClosedProject_ClosedProjectWithSnapshottedAudits_Purges(t *testing.T) {
	ctx := context.Background()
	st := openMinimalAuditPurgeStore(t)

	// Two audit_events rows with project_deleted_at populated — this
	// is the shape SnapshotAuditEventsForClosedProject leaves behind
	// during the close flow. NO projects row — DeleteProjectCascade
	// already removed it.
	closedAt := time.Now().UTC()
	for _, evtID := range []string{"evt-a", "evt-b"} {
		if _, err := st.db.ExecContext(ctx, `
			INSERT INTO audit_events (
				event_id, project_id, action, created_at,
				project_name_snapshot, project_deleted_at
			) VALUES (?, ?, ?, ?, ?, ?)
		`,
			evtID, "proj-closed-with-audits", "api_key.created",
			time.Now().UTC().Add(-24*time.Hour),
			"Project that customer closed", closedAt,
		); err != nil {
			t.Fatalf("seed snapshotted audit event %s: %v", evtID, err)
		}
	}

	deleted, err := st.PurgeAuditEventsForClosedProject(ctx, "proj-closed-with-audits")
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted=%d, want 2", deleted)
	}
}
