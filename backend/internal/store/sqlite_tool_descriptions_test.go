// SQLite half of the ListToolDescriptions coverage.
//
// The Postgres twin lives in postgres_tool_descriptions_test.go and
// asserts the same behaviours against the real engine. Both were
// written in the same change on purpose: these two stores carry
// SEPARATE hand-written SQL, and on 2026-08-24 a fix applied to one
// and not the other took production down (migration 056). A test that
// only covers SQLite would have passed then too.

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"mesedi/backend/internal/events"
)

func openToolDescriptionStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "desc.db") +
		"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(off)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`
		CREATE TABLE executions (
			execution_id TEXT PRIMARY KEY,
			project_id   TEXT NOT NULL
		);
		CREATE TABLE events (
			event_id     TEXT PRIMARY KEY,
			execution_id TEXT NOT NULL,
			event_type   TEXT NOT NULL,
			timestamp    TIMESTAMP NOT NULL,
			payload      TEXT
		);
	`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return &SQLiteStore{db: db}
}

// insertToolCall writes one tool_call event. payload is written raw so
// a test can express "the field is absent" as distinct from "the field
// is empty", which is the distinction the whole query turns on.
func insertToolCall(t *testing.T, s *SQLiteStore, eventID, execID, payload string, ts time.Time) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO events (event_id, execution_id, event_type, timestamp, payload)
		 VALUES (?, ?, 'tool_call', ?, ?)`,
		eventID, execID, ts, payload,
	); err != nil {
		t.Fatalf("insert event %s: %v", eventID, err)
	}
}

func insertExecution(t *testing.T, s *SQLiteStore, execID, projectID string) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO executions (execution_id, project_id) VALUES (?, ?)`,
		execID, projectID,
	); err != nil {
		t.Fatalf("insert execution %s: %v", execID, err)
	}
}

// TestListToolDescriptions_ExcludesCallingExecution.
//
// The detector's baseline must be PRIOR runs. If the calling
// execution's own rows are in the history, a poisoned call helps
// establish the baseline it is supposed to be measured against, and
// the attack partly hides itself. This is also the mistake that made
// seed_tool_schema_drift silently produce nothing on 2026-08-24: all
// its calls were in one execution, so once that execution was
// excluded there was no history left at all.
func TestListToolDescriptions_ExcludesCallingExecution(t *testing.T) {
	t.Parallel()
	s := openToolDescriptionStore(t)
	ctx := context.Background()
	base := time.Now().UTC()

	insertExecution(t, s, "exec-old", "proj-1")
	insertExecution(t, s, "exec-current", "proj-1")

	insertToolCall(t, s, "e1", "exec-old",
		`{"tool_name":"lookup","tool_description":"clean"}`, base)
	insertToolCall(t, s, "e2", "exec-current",
		`{"tool_name":"lookup","tool_description":"poisoned"}`, base.Add(time.Second))

	got, err := s.ListToolDescriptions(ctx, "proj-1", "lookup", "exec-current", 100)
	if err != nil {
		t.Fatalf("ListToolDescriptions: %v", err)
	}
	if len(got) != 1 || got[0] != "clean" {
		t.Fatalf("history should be prior runs only, got %v", got)
	}

	// With no exclusion the newest row is the current one. That is
	// how the caller reads the CURRENT description, so it has to work
	// in the opposite direction too.
	got, err = s.ListToolDescriptions(ctx, "proj-1", "lookup", "", 1)
	if err != nil {
		t.Fatalf("ListToolDescriptions (current): %v", err)
	}
	if len(got) != 1 || got[0] != "poisoned" {
		t.Fatalf("newest-first ordering broken, got %v", got)
	}
}

// TestListToolDescriptions_SkipsMissingAndEmpty is the rollout-safety
// test. Every customer on an SDK predating tool_description sends
// tool_call events with no description field. If those came back as
// "", they would form an overwhelming majority baseline, and the first
// call from an UPGRADED client would read as drift away from it. The
// deploy itself would page everyone.
func TestListToolDescriptions_SkipsMissingAndEmpty(t *testing.T) {
	t.Parallel()
	s := openToolDescriptionStore(t)
	ctx := context.Background()
	base := time.Now().UTC()

	insertExecution(t, s, "exec-a", "proj-1")

	// Field entirely absent, as an older SDK sends it.
	insertToolCall(t, s, "e1", "exec-a", `{"tool_name":"lookup"}`, base)
	// Field present but empty, as a tool with no docstring sends it.
	insertToolCall(t, s, "e2", "exec-a",
		`{"tool_name":"lookup","tool_description":""}`, base.Add(time.Second))
	// Explicit null.
	insertToolCall(t, s, "e3", "exec-a",
		`{"tool_name":"lookup","tool_description":null}`, base.Add(2*time.Second))
	// One real one.
	insertToolCall(t, s, "e4", "exec-a",
		`{"tool_name":"lookup","tool_description":"real"}`, base.Add(3*time.Second))

	got, err := s.ListToolDescriptions(ctx, "proj-1", "lookup", "", 100)
	if err != nil {
		t.Fatalf("ListToolDescriptions: %v", err)
	}
	if len(got) != 1 || got[0] != "real" {
		t.Fatalf("absent/empty/null descriptions must be skipped, got %v", got)
	}
}

// TestListToolDescriptions_ScopedToProjectAndTool. Cross-tenant leakage
// here would be both a correctness bug and a privacy one: descriptions
// are customer source text.
func TestListToolDescriptions_ScopedToProjectAndTool(t *testing.T) {
	t.Parallel()
	s := openToolDescriptionStore(t)
	ctx := context.Background()
	base := time.Now().UTC()

	insertExecution(t, s, "exec-mine", "proj-1")
	insertExecution(t, s, "exec-theirs", "proj-2")

	insertToolCall(t, s, "e1", "exec-mine",
		`{"tool_name":"lookup","tool_description":"mine"}`, base)
	insertToolCall(t, s, "e2", "exec-mine",
		`{"tool_name":"other_tool","tool_description":"different tool"}`, base)
	insertToolCall(t, s, "e3", "exec-theirs",
		`{"tool_name":"lookup","tool_description":"theirs"}`, base)

	got, err := s.ListToolDescriptions(ctx, "proj-1", "lookup", "", 100)
	if err != nil {
		t.Fatalf("ListToolDescriptions: %v", err)
	}
	if len(got) != 1 || got[0] != "mine" {
		t.Fatalf("results not scoped to (project, tool), got %v", got)
	}
}

// TestListToolDescriptions_IncludesFailedCalls. A poisoned description
// is worth seeing even when the call it rode in on threw. Filtering to
// successful calls, as ListSuccessfulToolReturns does, would drop
// exactly the noisiest attacks.
func TestListToolDescriptions_IncludesFailedCalls(t *testing.T) {
	t.Parallel()
	s := openToolDescriptionStore(t)
	ctx := context.Background()
	base := time.Now().UTC()

	insertExecution(t, s, "exec-a", "proj-1")
	insertToolCall(t, s, "e1", "exec-a",
		`{"tool_name":"lookup","tool_description":"failed one","status":"failed"}`, base)

	got, err := s.ListToolDescriptions(ctx, "proj-1", "lookup", "", 100)
	if err != nil {
		t.Fatalf("ListToolDescriptions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("failed calls must still contribute a description, got %v", got)
	}
}

// TestListToolDescriptions_LimitDefaultsAndCaps. limit <= 0 must not
// mean "unbounded": a busy project has millions of tool_call rows and
// an accidental zero would pull all of them into memory on the ingest
// path.
func TestListToolDescriptions_LimitDefaultsAndCaps(t *testing.T) {
	t.Parallel()
	s := openToolDescriptionStore(t)
	ctx := context.Background()
	base := time.Now().UTC()

	insertExecution(t, s, "exec-a", "proj-1")
	for i := 0; i < 150; i++ {
		insertToolCall(t, s,
			"e"+string(rune('a'+i%26))+string(rune('a'+i/26)),
			"exec-a",
			`{"tool_name":"lookup","tool_description":"d"}`,
			base.Add(time.Duration(i)*time.Second),
		)
	}

	got, err := s.ListToolDescriptions(ctx, "proj-1", "lookup", "", 0)
	if err != nil {
		t.Fatalf("ListToolDescriptions: %v", err)
	}
	if len(got) != 100 {
		t.Errorf("limit 0 should default to 100, got %d", len(got))
	}

	got, err = s.ListToolDescriptions(ctx, "proj-1", "lookup", "", 5)
	if err != nil {
		t.Fatalf("ListToolDescriptions: %v", err)
	}
	if len(got) != 5 {
		t.Errorf("explicit limit not honoured, got %d", len(got))
	}
}

// TestListToolInputSchemaHashes_RealWritePath: the definition-drift
// extraction, through real SaveEvents rows, skipping rows without
// the field so pre-upgrade traffic forms no baseline.
func TestListToolInputSchemaHashes_RealWritePath(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	const projectID = "proj_schema_hashes"
	if err := st.CreateProject(ctx, &Project{
		ProjectID: projectID, Name: "schema hash test", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	for _, id := range []string{"exec_sh_hist", "exec_sh_cur"} {
		if err := st.CreateExecution(ctx, &events.Execution{
			ExecutionID: id, ProjectID: projectID,
			Status: events.StatusStarted, StartedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create execution %s: %v", id, err)
		}
	}
	now := time.Now().UTC()
	mk := func(execID string, seq int, payload map[string]any) events.Event {
		b, _ := json.Marshal(payload)
		return events.Event{
			EventID:     fmt.Sprintf("evt_sh_%s_%d", execID, seq),
			ExecutionID: execID, EventType: "tool_call",
			Sequence: seq, Timestamp: now.Add(time.Duration(seq) * time.Second),
			Payload: b,
		}
	}
	batch := []events.Event{
		mk("exec_sh_hist", 1, map[string]any{"tool_name": "crm_lookup", "input_schema_hash": "hash_a"}),
		mk("exec_sh_hist", 2, map[string]any{"tool_name": "crm_lookup", "input_schema_hash": "hash_a"}),
		mk("exec_sh_hist", 3, map[string]any{"tool_name": "crm_lookup"}), // pre-upgrade row: skipped
		mk("exec_sh_hist", 4, map[string]any{"tool_name": "other_tool", "input_schema_hash": "hash_x"}),
		// Sequence 9 puts the current execution's call LAST in time;
		// the first draft of this fixture accidentally made the
		// field-less history row the most recent crm_lookup call,
		// which correctly (and confusingly) made "current" empty:
		// if the latest call declares no schema, there is no
		// current declared schema. That semantics is right and
		// deliberate; this fixture now tests the intended case.
		mk("exec_sh_cur", 9, map[string]any{"tool_name": "crm_lookup", "input_schema_hash": "hash_b"}),
	}
	if err := st.SaveEvents(ctx, batch); err != nil {
		t.Fatalf("save events: %v", err)
	}

	got, err := st.ListToolInputSchemaHashes(ctx, projectID, "crm_lookup", "exec_sh_cur", 50)
	if err != nil {
		t.Fatalf("list hashes: %v", err)
	}
	if len(got) != 2 || got[0] != "hash_a" || got[1] != "hash_a" {
		t.Errorf("history = %v, want [hash_a hash_a]: current execution "+
			"excluded, field-less row skipped, other tool ignored", got)
	}
	cur, err := st.ListToolInputSchemaHashes(ctx, projectID, "crm_lookup", "", 1)
	if err != nil {
		t.Fatalf("list current: %v", err)
	}
	if len(cur) != 1 || cur[0] != "hash_b" {
		t.Errorf("current = %v, want [hash_b] (most recent project-wide)", cur)
	}
}
