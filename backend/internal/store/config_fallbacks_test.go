// Tests for GetConfigFallbackStats. Uses the same minimal
// in-memory schema pattern as audit_events_purge_test.go so the
// test stays scoped to the aggregator's semantics without dragging
// in the full migration sequence.

package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// openMinimalFallbackStatsStore creates a SQLite store with just
// the system_events table — the only table GetConfigFallbackStats
// reads from after migration 050. Schema matches the
// relevant subset of migration 050.
func openMinimalFallbackStatsStore(t *testing.T) *SQLiteStore {
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

	if _, err := db.Exec(`CREATE TABLE system_events (
			event_id     TEXT PRIMARY KEY,
			project_id   TEXT NOT NULL,
			actor        TEXT NOT NULL,
			action       TEXT NOT NULL,
			target_type  TEXT NOT NULL,
			target_id    TEXT NOT NULL,
			payload_json TEXT,
			created_at   TIMESTAMP NOT NULL
		)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return &SQLiteStore{db: db}
}

// _fallbackEventCounter generates unique event IDs across multiple
// insertFallback calls in the same test, since time.Now()-derived
// ids collide when calls happen within the same microsecond.
var _fallbackEventCounter int

// insertFallback seeds one system_events row mimicking what
// handlers.go's recordSystemEventForProject(... "config_fallback" ...)
// would write (after migration 050 / ).
func insertFallback(
	t *testing.T, st *SQLiteStore,
	projectID, targetID string, createdAt time.Time,
) {
	t.Helper()
	_fallbackEventCounter++
	eventID := "evt-test-" + targetID + "-" + projectID + "-" +
		time.Now().Format("150405.000000") + "-" +
		// Counter suffix guarantees uniqueness even when many
		// inserts share a microsecond.
		intToStr(_fallbackEventCounter)
	_, err := st.db.ExecContext(context.Background(), `
		INSERT INTO system_events (
			event_id, project_id, actor, action, target_type, target_id, created_at
		) VALUES (?, ?, 'config_fallback', 'config_fallback', 'project_config', ?, ?)
	`, eventID, projectID, targetID, createdAt.UTC())
	if err != nil {
		t.Fatalf("insert fallback: %v", err)
	}
}

func intToStr(n int) string {
	// fmt.Sprintf would work but adds an import for a one-line use;
	// build by hand to keep the test file's dep surface minimal.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestGetConfigFallbackStats_ZeroForEmptyProject(t *testing.T) {
	st := openMinimalFallbackStatsStore(t)
	stats, err := st.GetConfigFallbackStats(context.Background(), "proj-empty", 24)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.TimeBudgetCount != 0 ||
		stats.ProviderIncidentMinTenantsCount != 0 ||
		stats.ToolReturnValueMaxBytesCount != 0 ||
		stats.ClassSeverityOverrideCount != 0 {
		t.Errorf("expected all zero counts, got %+v", stats)
	}
}

func TestGetConfigFallbackStats_GroupsByTargetID(t *testing.T) {
	st := openMinimalFallbackStatsStore(t)
	now := time.Now().UTC()
	// 3 time_budget, 2 provider_incident, 1 tool_return_value,
	// 4 class_severity_override for proj-a; 1 unrelated config
	// type (should be silently ignored).
	for i := 0; i < 3; i++ {
		insertFallback(t, st, "proj-a", "time_budget_ms", now.Add(-time.Duration(i)*time.Minute))
	}
	for i := 0; i < 2; i++ {
		insertFallback(t, st, "proj-a", "provider_incident_min_tenants", now.Add(-time.Duration(i)*time.Minute))
	}
	insertFallback(t, st, "proj-a", "tool_return_value_max_bytes", now)
	for i := 0; i < 4; i++ {
		insertFallback(t, st, "proj-a", "class_severity_override", now.Add(-time.Duration(i)*time.Minute))
	}
	insertFallback(t, st, "proj-a", "some_future_config", now)

	stats, err := st.GetConfigFallbackStats(context.Background(), "proj-a", 24)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.TimeBudgetCount != 3 {
		t.Errorf("TimeBudgetCount: want 3 got %d", stats.TimeBudgetCount)
	}
	if stats.ProviderIncidentMinTenantsCount != 2 {
		t.Errorf("ProviderIncidentMinTenantsCount: want 2 got %d", stats.ProviderIncidentMinTenantsCount)
	}
	if stats.ToolReturnValueMaxBytesCount != 1 {
		t.Errorf("ToolReturnValueMaxBytesCount: want 1 got %d", stats.ToolReturnValueMaxBytesCount)
	}
	if stats.ClassSeverityOverrideCount != 4 {
		t.Errorf("ClassSeverityOverrideCount: want 4 got %d", stats.ClassSeverityOverrideCount)
	}
}

func TestGetConfigFallbackStats_ScopedByProject(t *testing.T) {
	st := openMinimalFallbackStatsStore(t)
	now := time.Now().UTC()
	insertFallback(t, st, "proj-a", "time_budget_ms", now)
	insertFallback(t, st, "proj-a", "time_budget_ms", now)
	insertFallback(t, st, "proj-b", "time_budget_ms", now)

	statsA, err := st.GetConfigFallbackStats(context.Background(), "proj-a", 24)
	if err != nil {
		t.Fatalf("proj-a: %v", err)
	}
	if statsA.TimeBudgetCount != 2 {
		t.Errorf("proj-a TimeBudgetCount: want 2 got %d", statsA.TimeBudgetCount)
	}
	statsB, err := st.GetConfigFallbackStats(context.Background(), "proj-b", 24)
	if err != nil {
		t.Fatalf("proj-b: %v", err)
	}
	if statsB.TimeBudgetCount != 1 {
		t.Errorf("proj-b TimeBudgetCount: want 1 got %d", statsB.TimeBudgetCount)
	}
}

func TestGetConfigFallbackStats_WindowExcludesOldEvents(t *testing.T) {
	st := openMinimalFallbackStatsStore(t)
	now := time.Now().UTC()
	insertFallback(t, st, "proj-a", "time_budget_ms", now)
	// Far outside the 24h window.
	insertFallback(t, st, "proj-a", "time_budget_ms", now.AddDate(0, 0, -2))

	stats, err := st.GetConfigFallbackStats(context.Background(), "proj-a", 24)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.TimeBudgetCount != 1 {
		t.Errorf("want 1 (in-window only), got %d", stats.TimeBudgetCount)
	}
}

func TestGetConfigFallbackStats_IgnoresNonConfigFallbackActions(t *testing.T) {
	st := openMinimalFallbackStatsStore(t)
	now := time.Now().UTC()
	// Insert an unrelated system_events row that happens to have
	// the same target_id. GetConfigFallbackStats must filter on
	// action='config_fallback' AND target_type='project_config'.
	if _, err := st.db.Exec(`
		INSERT INTO system_events (event_id, project_id, actor, action, target_type, target_id, created_at)
		VALUES ('evt-x', 'proj-a', 'pricing', 'pricing_unknown_model', 'execution', 'time_budget_ms', ?)
	`, now); err != nil {
		t.Fatalf("seed: %v", err)
	}
	stats, err := st.GetConfigFallbackStats(context.Background(), "proj-a", 24)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.TimeBudgetCount != 0 {
		t.Errorf("want 0 (non-config_fallback row excluded), got %d", stats.TimeBudgetCount)
	}
}

func TestGetConfigFallbackStats_WindowDefaultsTo24h(t *testing.T) {
	st := openMinimalFallbackStatsStore(t)
	now := time.Now().UTC()
	insertFallback(t, st, "proj-a", "time_budget_ms", now)
	// Window <= 0 should default to 24 hours.
	stats, err := st.GetConfigFallbackStats(context.Background(), "proj-a", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.WindowHours != 24 {
		t.Errorf("WindowHours: want 24 got %d", stats.WindowHours)
	}
	if stats.TimeBudgetCount != 1 {
		t.Errorf("TimeBudgetCount: want 1 got %d", stats.TimeBudgetCount)
	}
}
