package store

// The migration runner splits each file into statements on semicolons.
// The checkpoint-permanence migration was the first to carry semicolons
// that do not end a statement: inside a SQLite trigger body, inside a
// Postgres dollar-quoted function body, and (in principle) inside a
// string literal. These tests pin the splitter against exactly those
// shapes, plus the behavior every earlier migration relies on. If a
// case here fails, a migration file is about to be applied in
// fragments, which fails on a fresh install where no idempotency
// tolerance can paper over it.

import (
	"strings"
	"testing"
)

func TestSplitPlainStatementsAndComments(t *testing.T) {
	t.Parallel()
	// The pre-trigger behavior all sixty earlier migrations depend on:
	// split on semicolons, drop comment lines, trim inline comments,
	// tolerate a semicolon inside a comment (migration 005's header).
	body := `
-- header comment; with a semicolon in it
CREATE TABLE a (id INTEGER PRIMARY KEY); -- trailing comment
CREATE INDEX idx_a ON a(id);

ALTER TABLE a ADD COLUMN note TEXT NOT NULL DEFAULT ''
`
	got := splitSQLStatements(body)
	if len(got) != 3 {
		t.Fatalf("got %d statements, want 3: %q", len(got), got)
	}
	if got[0] != "CREATE TABLE a (id INTEGER PRIMARY KEY)" {
		t.Errorf("statement 1 = %q", got[0])
	}
	if !strings.HasPrefix(got[2], "ALTER TABLE") {
		t.Errorf("unterminated final statement lost: %q", got[2])
	}
}

func TestSplitKeepsTriggerBodyWhole(t *testing.T) {
	t.Parallel()
	// The exact shape of migration 061's SQLite triggers. The
	// semicolon after RAISE ends a statement inside the body; only
	// the one after END ends the trigger.
	body := `
CREATE TRIGGER IF NOT EXISTS checkpoints_are_permanent
BEFORE DELETE ON checkpoints
BEGIN
    SELECT RAISE(ABORT,
        'checkpoints are permanent evidence, deletion is refused (migration 061)');
END;

CREATE TABLE after_trigger (id INTEGER);
`
	got := splitSQLStatements(body)
	if len(got) != 2 {
		t.Fatalf("got %d statements, want 2 (trigger + table): %q", len(got), got)
	}
	if !strings.Contains(got[0], "RAISE(ABORT") || !strings.HasSuffix(got[0], "END") {
		t.Errorf("trigger body was fragmented: %q", got[0])
	}
	if !strings.HasPrefix(got[1], "CREATE TABLE after_trigger") {
		t.Errorf("statement after the trigger lost: %q", got[1])
	}
}

func TestSplitKeepsDollarQuotedBodyWhole(t *testing.T) {
	t.Parallel()
	// The exact shape of migration 061's Postgres function. Both
	// semicolons between the $$ markers belong to the plpgsql body.
	body := `
CREATE OR REPLACE FUNCTION refuse_checkpoint_delete() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% rows are permanent evidence (migration 061)', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS checkpoints_are_permanent ON checkpoints;
`
	got := splitSQLStatements(body)
	if len(got) != 2 {
		t.Fatalf("got %d statements, want 2 (function + drop): %q", len(got), got)
	}
	if !strings.Contains(got[0], "RAISE EXCEPTION") ||
		!strings.HasSuffix(got[0], "$$ LANGUAGE plpgsql") {
		t.Errorf("dollar-quoted body was fragmented: %q", got[0])
	}
}

func TestSplitKeepsQuotedSemicolonWhole(t *testing.T) {
	t.Parallel()
	got := splitSQLStatements(`INSERT INTO t (v) VALUES ('a; b');INSERT INTO t (v) VALUES ('c')`)
	if len(got) != 2 {
		t.Fatalf("got %d statements, want 2: %q", len(got), got)
	}
	if !strings.Contains(got[0], "'a; b'") {
		t.Errorf("semicolon inside a string literal split the statement: %q", got[0])
	}
}

func TestSplitDoesNotMistakeLoneDollarForATag(t *testing.T) {
	t.Parallel()
	// A $ that never closes into a $tag$ must not swallow the rest of
	// the file into one statement.
	got := splitSQLStatements(`UPDATE t SET v = 1 WHERE k = $1;UPDATE t SET v = 2 WHERE k = $2`)
	if len(got) != 2 {
		t.Fatalf("got %d statements, want 2: %q", len(got), got)
	}
}
