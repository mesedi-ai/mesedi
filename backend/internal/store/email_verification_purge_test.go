// Regression tests for the email-verification purge inside
// DeleteProjectCascade.
//
// The behavior under test, decided 2026-09-24: verification is keyed
// to the ADDRESS (verified_emails, one row per email that ever proved
// mailbox ownership), and before this change an account deletion
// deliberately left that row behind, so a later re-signup with the
// same address showed VERIFIED with nothing to click and the retained
// row was personal data with no account behind it. The cascade now
// deletes the verified_emails row and any in-flight verification
// tokens, but ONLY when the deleted project is the LAST live project
// owned by that email. The guard matters because the signup dedup
// truth table explicitly permits two live projects to share one email
// (a stranger pre-registers unverified, the real owner signs up
// anyway), and the survivor's auth gate reads the same row.
//
// Same minimal-schema pattern as audit_events_purge_test.go: only the
// tables the assertions need are created, and the cascade's other
// statements no-op through its missing-table tolerance.

package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openMinimalEmailPurgeStore(t *testing.T) *SQLiteStore {
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

	stmts := []string{
		`CREATE TABLE projects (
			project_id    TEXT PRIMARY KEY,
			name          TEXT NOT NULL,
			owner_user_id TEXT,
			owner_email   TEXT,
			created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE verified_emails (
			email       TEXT PRIMARY KEY,
			verified_at TIMESTAMP NOT NULL,
			method      TEXT NOT NULL
		)`,
		`CREATE TABLE email_verification_tokens (
			token      TEXT PRIMARY KEY,
			email      TEXT NOT NULL,
			project_id TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL,
			expires_at TIMESTAMP NOT NULL,
			used_at    TIMESTAMP
		)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}
	return &SQLiteStore{db: db}
}

func seedEmailPurgeProject(t *testing.T, s *SQLiteStore, projectID, ownerEmail string) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO projects (project_id, name, owner_email) VALUES (?, ?, ?)`,
		projectID, "proj "+projectID, ownerEmail,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

func seedVerifiedEmail(t *testing.T, s *SQLiteStore, email string) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO verified_emails (email, verified_at, method)
		 VALUES (?, CURRENT_TIMESTAMP, 'email_link')`, email,
	); err != nil {
		t.Fatalf("seed verified email: %v", err)
	}
}

func countRows(t *testing.T, s *SQLiteStore, table, whereCol, val string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM `+table+` WHERE `+whereCol+` = ?`, val,
	).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// Deleting the last project owned by an email removes the
// verified_emails row and every token for that address.
func TestCascadeDeletePurgesVerifiedEmailWithLastProject(t *testing.T) {
	s := openMinimalEmailPurgeStore(t)
	ctx := context.Background()

	seedEmailPurgeProject(t, s, "proj_a", "owner@example.com")
	seedVerifiedEmail(t, s, "owner@example.com")
	if _, err := s.db.Exec(
		`INSERT INTO email_verification_tokens
		 (token, email, project_id, created_at, expires_at)
		 VALUES ('tok1', 'owner@example.com', 'proj_a',
		         CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	if err := s.DeleteProjectCascade(ctx, "proj_a"); err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if n := countRows(t, s, "verified_emails", "email", "owner@example.com"); n != 0 {
		t.Errorf("verified_emails rows after last-project delete = %d, want 0", n)
	}
	if n := countRows(t, s, "email_verification_tokens", "email", "owner@example.com"); n != 0 {
		t.Errorf("verification tokens after last-project delete = %d, want 0", n)
	}
}

// Two live projects share the email: deleting one keeps the row,
// because the survivor's auth gate reads it. Deleting the survivor
// removes it.
func TestCascadeDeleteKeepsVerifiedEmailWhileAnotherProjectRemains(t *testing.T) {
	s := openMinimalEmailPurgeStore(t)
	ctx := context.Background()

	seedEmailPurgeProject(t, s, "proj_a", "shared@example.com")
	seedEmailPurgeProject(t, s, "proj_b", "shared@example.com")
	seedVerifiedEmail(t, s, "shared@example.com")

	if err := s.DeleteProjectCascade(ctx, "proj_a"); err != nil {
		t.Fatalf("cascade first: %v", err)
	}
	if n := countRows(t, s, "verified_emails", "email", "shared@example.com"); n != 1 {
		t.Fatalf("verified_emails rows with a live project remaining = %d, want 1", n)
	}

	if err := s.DeleteProjectCascade(ctx, "proj_b"); err != nil {
		t.Fatalf("cascade second: %v", err)
	}
	if n := countRows(t, s, "verified_emails", "email", "shared@example.com"); n != 0 {
		t.Errorf("verified_emails rows after last delete = %d, want 0", n)
	}
}

// The comparison normalizes case and whitespace, because older
// project rows may carry owner_email in whatever form signup stored
// at the time, while verified_emails is keyed lower+trim.
func TestCascadeDeletePurgeNormalizesEmailCase(t *testing.T) {
	s := openMinimalEmailPurgeStore(t)
	ctx := context.Background()

	seedEmailPurgeProject(t, s, "proj_a", "  Mixed.Case@Example.COM ")
	seedVerifiedEmail(t, s, "mixed.case@example.com")

	if err := s.DeleteProjectCascade(ctx, "proj_a"); err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if n := countRows(t, s, "verified_emails", "email", "mixed.case@example.com"); n != 0 {
		t.Errorf("verified_emails rows after mixed-case delete = %d, want 0", n)
	}
}

// An unrelated verified email is untouched, and deleting a project
// whose email was never verified is a quiet no-op on these tables.
func TestCascadeDeleteLeavesUnrelatedEmailsAlone(t *testing.T) {
	s := openMinimalEmailPurgeStore(t)
	ctx := context.Background()

	seedEmailPurgeProject(t, s, "proj_a", "deleted@example.com")
	seedEmailPurgeProject(t, s, "proj_b", "bystander@example.com")
	seedVerifiedEmail(t, s, "bystander@example.com")

	if err := s.DeleteProjectCascade(ctx, "proj_a"); err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if n := countRows(t, s, "verified_emails", "email", "bystander@example.com"); n != 1 {
		t.Errorf("bystander verified_emails rows = %d, want 1", n)
	}
}
