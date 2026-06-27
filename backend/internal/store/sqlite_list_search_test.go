// Tests for the q-filter parameter on ListFailureGroups +
// ListExecutions (list-search-paginate wave). Uses a minimal
// schema following the same pattern as sqlite_cost_velocity_test
// so the test stays scoped to the search semantics.
//
// Coverage focus: q-filter behavior. The existing
// limit/offset/ordering paths are not re-tested here — those have
// been in production for months and a regression would surface
// the existing /failure-groups and /executions endpoints, not
// the q-filter.

package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openMinimalListStore(t *testing.T) *SQLiteStore {
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

	// failure_groups + executions schemas — just the columns
	// ListFailureGroups + ListExecutions touch. Keep it minimal so
	// the test isn't dragged into the full migration sequence.
	if _, err := db.Exec(`
		CREATE TABLE failure_groups (
			group_id              TEXT PRIMARY KEY,
			project_id            TEXT NOT NULL,
			failure_class         TEXT NOT NULL,
			signature             TEXT NOT NULL,
			first_seen            DATETIME NOT NULL,
			last_seen             DATETIME NOT NULL,
			event_count           INTEGER NOT NULL DEFAULT 0,
			affected_executions   INTEGER NOT NULL DEFAULT 0,
			sample_execution_id   TEXT,
			analysis_markdown     TEXT,
			analyzed_at           DATETIME,
			analysis_model        TEXT,
			severity_hint         TEXT,
			resolved_at           DATETIME,
			resolved_by           TEXT
		);
		CREATE TABLE executions (
			execution_id          TEXT PRIMARY KEY,
			project_id            TEXT NOT NULL,
			status                TEXT NOT NULL,
			started_at            DATETIME NOT NULL,
			ended_at              DATETIME,
			duration_ms           INTEGER NOT NULL DEFAULT 0,
			total_tokens_in       INTEGER NOT NULL DEFAULT 0,
			total_tokens_out      INTEGER NOT NULL DEFAULT 0,
			estimated_cost_usd    REAL NOT NULL DEFAULT 0,
			sdk_language          TEXT,
			sdk_version           TEXT,
			crash_signature       TEXT,
			failure_group_id      TEXT
		);
	`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return &SQLiteStore{db: db}
}

func insertGroup(t *testing.T, st *SQLiteStore, gid, class, sig string) {
	t.Helper()
	now := time.Now().UTC()
	_, err := st.db.ExecContext(context.Background(),
		`INSERT INTO failure_groups (group_id, project_id, failure_class, signature, first_seen, last_seen)
		 VALUES (?, 'proj', ?, ?, ?, ?)`,
		gid, class, sig, now, now)
	if err != nil {
		t.Fatalf("insertGroup: %v", err)
	}
}

func insertExec(t *testing.T, st *SQLiteStore, eid, crashSig string) {
	t.Helper()
	now := time.Now().UTC()
	_, err := st.db.ExecContext(context.Background(),
		`INSERT INTO executions (execution_id, project_id, status, started_at, crash_signature)
		 VALUES (?, 'proj', 'crashed', ?, ?)`,
		eid, now, crashSig)
	if err != nil {
		t.Fatalf("insertExec: %v", err)
	}
}

func TestListFailureGroups_EmptyQReturnsAll(t *testing.T) {
	t.Parallel()
	st := openMinimalListStore(t)
	insertGroup(t, st, "g1", "crashes", "ValueError:foo")
	insertGroup(t, st, "g2", "token_waste", "near_dup:bar")

	got, err := st.ListFailureGroups(context.Background(), "proj", ListFailureGroupsOpts{Limit: 50})
	if err != nil {
		t.Fatalf("ListFailureGroups: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
}

func TestListFailureGroups_QFiltersBySignature(t *testing.T) {
	t.Parallel()
	st := openMinimalListStore(t)
	insertGroup(t, st, "g1", "crashes", "ValueError:nginx_timeout")
	insertGroup(t, st, "g2", "crashes", "RuntimeError:other")

	got, err := st.ListFailureGroups(context.Background(), "proj", ListFailureGroupsOpts{Q: "nginx", Limit: 50})
	if err != nil {
		t.Fatalf("ListFailureGroups: %v", err)
	}
	if len(got) != 1 || got[0].GroupID != "g1" {
		t.Fatalf("want g1, got %+v", got)
	}
}

func TestListFailureGroups_QFiltersByFailureClass(t *testing.T) {
	t.Parallel()
	st := openMinimalListStore(t)
	insertGroup(t, st, "g1", "token_waste", "near_dup:foo")
	insertGroup(t, st, "g2", "crashes", "ValueError:foo")

	got, err := st.ListFailureGroups(context.Background(), "proj", ListFailureGroupsOpts{Q: "token", Limit: 50})
	if err != nil {
		t.Fatalf("ListFailureGroups: %v", err)
	}
	if len(got) != 1 || got[0].GroupID != "g1" {
		t.Fatalf("want g1, got %+v", got)
	}
}

func TestListFailureGroups_QIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	st := openMinimalListStore(t)
	insertGroup(t, st, "g1", "crashes", "ValueError:NginxTimeout")

	got, err := st.ListFailureGroups(context.Background(), "proj", ListFailureGroupsOpts{Q: "nginxtimeout", Limit: 50})
	if err != nil {
		t.Fatalf("ListFailureGroups: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row (case-insensitive), got %d", len(got))
	}
}

func TestListFailureGroups_QNoMatchReturnsEmpty(t *testing.T) {
	t.Parallel()
	st := openMinimalListStore(t)
	insertGroup(t, st, "g1", "crashes", "ValueError:foo")

	got, err := st.ListFailureGroups(context.Background(), "proj", ListFailureGroupsOpts{Q: "zzzz", Limit: 50})
	if err != nil {
		t.Fatalf("ListFailureGroups: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 rows, got %d", len(got))
	}
}

func TestListExecutions_QFiltersByExecutionID(t *testing.T) {
	t.Parallel()
	st := openMinimalListStore(t)
	insertExec(t, st, "exec_abc123", "")
	insertExec(t, st, "exec_xyz789", "")

	got, err := st.ListExecutions(context.Background(), "proj", "abc", 50, 0)
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	if len(got) != 1 || got[0].ExecutionID != "exec_abc123" {
		t.Fatalf("want exec_abc123, got %+v", got)
	}
}

func TestListExecutions_QFiltersByCrashSignature(t *testing.T) {
	t.Parallel()
	st := openMinimalListStore(t)
	insertExec(t, st, "exec_1", "ValueError:foo_bar")
	insertExec(t, st, "exec_2", "RuntimeError:baz")

	got, err := st.ListExecutions(context.Background(), "proj", "foo_bar", 50, 0)
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	if len(got) != 1 || got[0].ExecutionID != "exec_1" {
		t.Fatalf("want exec_1, got %+v", got)
	}
}

func TestListExecutions_EmptyQReturnsAll(t *testing.T) {
	t.Parallel()
	st := openMinimalListStore(t)
	insertExec(t, st, "exec_a", "")
	insertExec(t, st, "exec_b", "")

	got, err := st.ListExecutions(context.Background(), "proj", "", 50, 0)
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
}

// ─── failure-group-resolve wave: resolve/unresolve + list filter ───

func TestListFailureGroups_HidesResolvedByDefault(t *testing.T) {
	t.Parallel()
	st := openMinimalListStore(t)
	insertGroup(t, st, "g1", "crashes", "ValueError:foo")
	insertGroup(t, st, "g2", "crashes", "RuntimeError:bar")

	if err := st.ResolveFailureGroup(context.Background(), "g1", "proj", "user_x"); err != nil {
		t.Fatalf("ResolveFailureGroup: %v", err)
	}

	got, err := st.ListFailureGroups(context.Background(), "proj",
		ListFailureGroupsOpts{Limit: 50})
	if err != nil {
		t.Fatalf("ListFailureGroups: %v", err)
	}
	if len(got) != 1 || got[0].GroupID != "g2" {
		t.Fatalf("want only g2 (g1 resolved + hidden), got %+v", got)
	}
}

func TestListFailureGroups_IncludeResolvedShowsAll(t *testing.T) {
	t.Parallel()
	st := openMinimalListStore(t)
	insertGroup(t, st, "g1", "crashes", "ValueError:foo")
	insertGroup(t, st, "g2", "crashes", "RuntimeError:bar")
	if err := st.ResolveFailureGroup(context.Background(), "g1", "proj", "user_x"); err != nil {
		t.Fatalf("ResolveFailureGroup: %v", err)
	}

	got, err := st.ListFailureGroups(context.Background(), "proj",
		ListFailureGroupsOpts{Limit: 50, IncludeResolved: true})
	if err != nil {
		t.Fatalf("ListFailureGroups: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want both rows when IncludeResolved=true, got %d", len(got))
	}
}

func TestResolveFailureGroup_PopulatesFields(t *testing.T) {
	t.Parallel()
	st := openMinimalListStore(t)
	insertGroup(t, st, "g1", "crashes", "ValueError:foo")

	if err := st.ResolveFailureGroup(context.Background(), "g1", "proj", "user_x"); err != nil {
		t.Fatalf("ResolveFailureGroup: %v", err)
	}

	g, err := st.GetFailureGroup(context.Background(), "g1")
	if err != nil {
		t.Fatalf("GetFailureGroup: %v", err)
	}
	if g.ResolvedAt == nil {
		t.Errorf("want ResolvedAt populated, got nil")
	}
	if g.ResolvedBy == nil || *g.ResolvedBy != "user_x" {
		t.Errorf("want ResolvedBy=user_x, got %v", g.ResolvedBy)
	}
}

func TestResolveFailureGroup_TenantScopedReturnsNotFound(t *testing.T) {
	t.Parallel()
	st := openMinimalListStore(t)
	insertGroup(t, st, "g1", "crashes", "ValueError:foo")

	// Attempt to resolve from a different project — must return
	// ErrNotFound, no cross-tenant leak.
	err := st.ResolveFailureGroup(context.Background(), "g1", "other_proj", "user_y")
	if err != ErrNotFound {
		t.Errorf("want ErrNotFound for cross-tenant resolve, got %v", err)
	}
}

func TestResolveFailureGroup_Idempotent(t *testing.T) {
	t.Parallel()
	st := openMinimalListStore(t)
	insertGroup(t, st, "g1", "crashes", "ValueError:foo")

	if err := st.ResolveFailureGroup(context.Background(), "g1", "proj", "user_x"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	// Second resolve refreshes timestamp and should succeed.
	if err := st.ResolveFailureGroup(context.Background(), "g1", "proj", "user_y"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	g, err := st.GetFailureGroup(context.Background(), "g1")
	if err != nil {
		t.Fatalf("GetFailureGroup: %v", err)
	}
	if g.ResolvedBy == nil || *g.ResolvedBy != "user_y" {
		t.Errorf("want last writer user_y to win, got %v", g.ResolvedBy)
	}
}

func TestUnresolveFailureGroup_ClearsFields(t *testing.T) {
	t.Parallel()
	st := openMinimalListStore(t)
	insertGroup(t, st, "g1", "crashes", "ValueError:foo")
	if err := st.ResolveFailureGroup(context.Background(), "g1", "proj", "user_x"); err != nil {
		t.Fatalf("ResolveFailureGroup: %v", err)
	}

	if err := st.UnresolveFailureGroup(context.Background(), "g1", "proj"); err != nil {
		t.Fatalf("UnresolveFailureGroup: %v", err)
	}

	g, err := st.GetFailureGroup(context.Background(), "g1")
	if err != nil {
		t.Fatalf("GetFailureGroup: %v", err)
	}
	if g.ResolvedAt != nil {
		t.Errorf("want ResolvedAt cleared, got %v", g.ResolvedAt)
	}
	if g.ResolvedBy != nil {
		t.Errorf("want ResolvedBy cleared, got %v", g.ResolvedBy)
	}
}

func TestUnresolveFailureGroup_TenantScopedReturnsNotFound(t *testing.T) {
	t.Parallel()
	st := openMinimalListStore(t)
	insertGroup(t, st, "g1", "crashes", "ValueError:foo")
	err := st.UnresolveFailureGroup(context.Background(), "g1", "other_proj")
	if err != ErrNotFound {
		t.Errorf("want ErrNotFound for cross-tenant unresolve, got %v", err)
	}
}
