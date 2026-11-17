package store

// Account closure (DeleteProjectCascade) must remove every row that holds
// a project's data, on both databases.
//
// Until 2026-11-16 the cascade's events statement read
// "DELETE FROM events WHERE project_id", a column events has never had, so
// closing an account failed on SQLite and on Postgres alike (outside review
// the outside review of 2026-11-15; the review reproduced SQLite only). Behind it, six tables
// with no foreign key to projects were never cleaned at all.
//
// The rule now (Robert's decision, 2026-11-16): only the audit log outlives
// a closed account, plus checkpoint leaves until the evidence-chain batch
// decides them. These tests hold the rule for the whole class:
//
//   - every table with a project_id or execution_id column must be emptied
//     by a cascade statement, removed by a foreign key, or named in
//     keptAfterClose with its reason;
//   - both databases' cascades delete from the same tables;
//   - a real close removes the project's rows and leaves other projects'.
//
// The Postgres cases run when MESEDI_TEST_POSTGRES_DSN points at an empty
// database, which CI provides.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"testing"
	"time"
)

// keptAfterClose lists the tables that deliberately outlive a closed
// project, with the reason. Adding a table here is a privacy decision.
var keptAfterClose = map[string]string{
	"projects":                 "the row the cascade itself deletes last",
	"audit_events":             "kept for seven years after closure (migration 031; privacy page)",
	"checkpoint_tenant_leaves": "hashes and counts only; other projects' proofs depend on them; decided in the evidence-chain batch",
}

// reachedByOwner lists cascade tables keyed by the project's owner (email,
// user or organization) rather than by project_id or execution_id.
var reachedByOwner = map[string]string{
	"organizations":             "created_by_user_id of the project's owner",
	"organization_members":      "members of the owner's organizations",
	"organization_invites":      "invites of the owner's organizations",
	"email_verification_tokens": "the owner's email, once no other project uses it",
	"verified_emails":           "the owner's email, once no other project uses it",
}

var cascadeTableRE = regexp.MustCompile(`DELETE FROM (\w+)`)

func cascadeTables(stmts []string) map[string]bool {
	out := map[string]bool{}
	for _, q := range stmts {
		if m := cascadeTableRE.FindStringSubmatch(q); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

type cascadeTarget struct {
	name    string
	db      *sql.DB
	st      Store
	stmts   []string
	postgre bool
}

func (c cascadeTarget) q(query string) string {
	if !c.postgre {
		return query
	}
	n := 0
	return regexp.MustCompile(`\?`).ReplaceAllStringFunc(query, func(string) string {
		n++
		return fmt.Sprintf("$%d", n)
	})
}

// projectTables returns, for every table with a project_id or
// execution_id column, whether a foreign key on that column cascades
// from projects or executions.
func (c cascadeTarget) projectTables(t *testing.T) map[string]bool {
	t.Helper()
	ctx := context.Background()
	out := map[string]bool{}
	if c.postgre {
		rows, err := c.db.QueryContext(ctx, `
			SELECT DISTINCT col.table_name
			  FROM information_schema.columns col
			  JOIN information_schema.tables tab
			    ON tab.table_schema = col.table_schema
			   AND tab.table_name = col.table_name
			 WHERE col.table_schema = current_schema()
			   AND tab.table_type = 'BASE TABLE'
			   AND col.column_name IN ('project_id', 'execution_id')`)
		// Base tables only, as the SQLite half below lists only
		// type 'table': a view (tool_events, migration 065) holds no
		// rows of its own, so there is nothing for a close to delete.
		if err != nil {
			t.Fatalf("list tables: %v", err)
		}
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			names = append(names, n)
		}
		rows.Close()
		for _, n := range names {
			var cascades bool
			err := c.db.QueryRowContext(ctx, `
				SELECT EXISTS (
				  SELECT 1
				    FROM information_schema.referential_constraints rc
				    JOIN information_schema.key_column_usage kcu
				      ON kcu.constraint_name = rc.constraint_name
				     AND kcu.constraint_schema = rc.constraint_schema
				    JOIN information_schema.constraint_column_usage ccu
				      ON ccu.constraint_name = rc.constraint_name
				     AND ccu.constraint_schema = rc.constraint_schema
				   WHERE kcu.table_name = $1
				     AND kcu.column_name IN ('project_id', 'execution_id')
				     AND ccu.table_name IN ('projects', 'executions')
				     AND rc.delete_rule = 'CASCADE')`, n).Scan(&cascades)
			if err != nil {
				t.Fatalf("foreign keys of %s: %v", n, err)
			}
			out[n] = cascades
		}
		return out
	}
	rows, err := c.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		var hasCol int
		if err := c.db.QueryRowContext(ctx,
			`SELECT count(*) FROM pragma_table_info(?) WHERE name IN ('project_id', 'execution_id')`, n).Scan(&hasCol); err != nil {
			t.Fatal(err)
		}
		if hasCol == 0 {
			continue
		}
		var cascades int
		if err := c.db.QueryRowContext(ctx, `
			SELECT count(*) FROM pragma_foreign_key_list(?)
			 WHERE "from" IN ('project_id', 'execution_id')
			   AND "table" IN ('projects', 'executions')
			   AND on_delete = 'CASCADE'`, n).Scan(&cascades); err != nil {
			t.Fatal(err)
		}
		out[n] = cascades > 0
	}
	return out
}

func cascadeTargets(t *testing.T) []cascadeTarget {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sq, err := OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = sq.Close() })
	targets := []cascadeTarget{{name: "sqlite", db: sq.db, st: sq, stmts: sqliteProjectCascadeStatements}}
	if dsn := os.Getenv("MESEDI_TEST_POSTGRES_DSN"); dsn != "" {
		pg, err := OpenPostgres(dsn, logger)
		if err != nil {
			t.Fatalf("open postgres: %v", err)
		}
		t.Cleanup(func() { _ = pg.Close() })
		targets = append(targets, cascadeTarget{name: "postgres", db: pg.db, st: pg, stmts: postgresProjectCascadeStatements, postgre: true})
	}
	return targets
}

func TestProjectCascadeCoversEveryProjectTable(t *testing.T) {
	for _, c := range cascadeTargets(t) {
		t.Run(c.name, func(t *testing.T) {
			listed := cascadeTables(c.stmts)
			tables := c.projectTables(t)
			var missing []string
			for table, fkCascade := range tables {
				if listed[table] || fkCascade {
					continue
				}
				if _, kept := keptAfterClose[table]; kept {
					continue
				}
				missing = append(missing, table)
			}
			sort.Strings(missing)
			if len(missing) > 0 {
				t.Fatalf("closing an account leaves these tables' rows behind: %v. "+
					"Add each to the cascade, or to keptAfterClose with the decision that keeps it.", missing)
			}
			for table := range listed {
				if _, ok := tables[table]; ok {
					continue
				}
				if _, ok := reachedByOwner[table]; ok {
					continue
				}
				t.Errorf("cascade deletes from %s, which has no project_id or execution_id column "+
					"and is not in reachedByOwner", table)
			}
		})
	}
}

func TestProjectCascadeStatementsMatchAcrossDatabases(t *testing.T) {
	a, b := cascadeTables(sqliteProjectCascadeStatements), cascadeTables(postgresProjectCascadeStatements)
	for table := range a {
		if !b[table] {
			t.Errorf("SQLite cascade deletes %s; Postgres does not", table)
		}
	}
	for table := range b {
		if !a[table] {
			t.Errorf("Postgres cascade deletes %s; SQLite does not", table)
		}
	}
}

// TestDeleteProjectCascadeRemovesProjectData closes one project that has a
// row in every table the 2026-11-16 fix covers, next to a second project
// with the same rows, and checks that only the first project's data goes.
func TestDeleteProjectCascadeRemovesProjectData(t *testing.T) {
	ctx := context.Background()
	for _, c := range cascadeTargets(t) {
		t.Run(c.name, func(t *testing.T) {
			suffix := fmt.Sprintf("%d", time.Now().UnixNano())
			closing, keeping := "proj-close-"+suffix, "proj-keep-"+suffix
			now := time.Now().UTC()
			for _, p := range []string{closing, keeping} {
				if err := c.st.CreateProject(ctx, &Project{ProjectID: p, Name: p}); err != nil {
					t.Fatalf("create %s: %v", p, err)
				}
				inserts := []struct {
					sql  string
					args []any
				}{
					{`INSERT INTO executions (execution_id, project_id, status, started_at) VALUES (?, ?, 'completed', ?)`, []any{"exec-" + p, p, now}},
					{`INSERT INTO events (event_id, execution_id, event_type, sequence, timestamp) VALUES (?, ?, 'llm_call', 1, ?)`, []any{"evt-" + p, "exec-" + p, now}},
					{`INSERT INTO project_patterns (pattern_id, project_id, detector, pattern, created_at) VALUES (?, ?, 'prompt_injection', 'x', ?)`, []any{"pat-" + p, p, now}},
					{`INSERT INTO project_detector_allowlist (allowlist_id, project_id, detector, allowlist_key, created_at) VALUES (?, ?, 'crashes', 'ValueError', ?)`, []any{"allow-" + p, p, now}},
					{`INSERT INTO detector_thresholds (project_id, detector, threshold_key, value_json, created_at, updated_at) VALUES (?, 'cost_velocity', 'k', '1', ?, ?)`, []any{p, now, now}},
					{`INSERT INTO tool_contract_pins (project_id, tool_name, kind, pinned_hash, created_at, updated_at) VALUES (?, 'search', 'schema', 'h', ?, ?)`, []any{p, now, now}},
					{`INSERT INTO system_events (event_id, project_id, actor, action, target_type, target_id, created_at) VALUES (?, ?, 'test', 'test', 'project', ?, ?)`, []any{"sys-" + p, p, p, now}},
					{`INSERT INTO request_log (project_id, api_key_id, ip_address, method, path, status_code, received_at) VALUES (?, 'key', '203.0.113.7', 'POST', '/events', 200, ?)`, []any{p, now}},
				}
				for _, in := range inserts {
					if _, err := c.db.ExecContext(ctx, c.q(in.sql), in.args...); err != nil {
						t.Fatalf("seed %s: %v (%s)", p, err, in.sql)
					}
				}
			}

			if err := c.st.DeleteProjectCascade(ctx, closing); err != nil {
				t.Fatalf("close account: %v", err)
			}

			counts := map[string]string{
				"executions":                 `SELECT count(*) FROM executions WHERE project_id = ?`,
				"events":                     `SELECT count(*) FROM events WHERE execution_id = 'exec-' || ?`,
				"project_patterns":           `SELECT count(*) FROM project_patterns WHERE project_id = ?`,
				"project_detector_allowlist": `SELECT count(*) FROM project_detector_allowlist WHERE project_id = ?`,
				"detector_thresholds":        `SELECT count(*) FROM detector_thresholds WHERE project_id = ?`,
				"tool_contract_pins":         `SELECT count(*) FROM tool_contract_pins WHERE project_id = ?`,
				"system_events":              `SELECT count(*) FROM system_events WHERE project_id = ?`,
				"request_log":                `SELECT count(*) FROM request_log WHERE project_id = ?`,
				"projects":                   `SELECT count(*) FROM projects WHERE project_id = ?`,
			}
			for table, query := range counts {
				var gone, kept int
				if err := c.db.QueryRowContext(ctx, c.q(query), closing).Scan(&gone); err != nil {
					t.Fatalf("count %s: %v", table, err)
				}
				if err := c.db.QueryRowContext(ctx, c.q(query), keeping).Scan(&kept); err != nil {
					t.Fatalf("count %s: %v", table, err)
				}
				if gone != 0 {
					t.Errorf("%s: %d row(s) of the closed project remain", table, gone)
				}
				if kept != 1 {
					t.Errorf("%s: the other project has %d row(s), want 1", table, kept)
				}
			}
		})
	}
}
