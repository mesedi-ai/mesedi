package api

// Wiring test for the tool-schema-drift detector AS REACHED through
// HandleUpdateExecution, written when the second carve of the split
// surfaced that no test drove this path (the same gap the first
// carve surfaced for cost velocity: unit tests existed for the pure
// detection functions, none for the wiring that makes them fire).
//
// Scenario: one history execution carries ten successful tool_call
// returns of a stable shape for the same tool, comfortably past the
// ten-call minimum and the two-thirds majority. The current
// execution's single return changes a field's type, the breaking
// direction. PATCHing the current execution to completed must
// produce a tool_schema_drift failure group whose signature starts
// with the tool's name.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mesedi/backend/internal/events"
	"mesedi/backend/internal/store"
)

func TestHandleUpdateExecution_FiresToolSchemaDrift(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	ctx := context.Background()
	const projectID = "proj_drift_wiring"
	const toolName = "weather_lookup"
	if err := st.CreateProject(ctx, &store.Project{
		ProjectID: projectID, Name: "drift wiring test", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	now := time.Now().UTC()
	mkExec := func(id string, startedAt time.Time) {
		t.Helper()
		if err := st.CreateExecution(ctx, &events.Execution{
			ExecutionID: id, ProjectID: projectID,
			Status: events.StatusStarted, StartedAt: startedAt,
		}); err != nil {
			t.Fatalf("create execution %s: %v", id, err)
		}
	}
	mkExec("exec_drift_history", now.Add(-1*time.Hour))
	mkExec("exec_drift_current", now.Add(-1*time.Minute))

	toolEvent := func(execID string, seq int, ts time.Time, returnValue string) events.Event {
		payload, _ := json.Marshal(map[string]any{
			"tool_name":    toolName,
			"return_value": json.RawMessage(returnValue),
		})
		return events.Event{
			EventID:     fmt.Sprintf("evt_drift_%s_%d", execID, seq),
			ExecutionID: execID, EventType: "tool_call",
			Sequence: seq, Timestamp: ts, Payload: payload,
		}
	}
	var batch []events.Event
	// Ten stable-shape history returns: {"temp": number, "city": string}.
	for i := 1; i <= 10; i++ {
		batch = append(batch, toolEvent("exec_drift_history", i,
			now.Add(-time.Duration(60-i)*time.Minute),
			`{"temp": 21.5, "city": "melbourne"}`))
	}
	// The current execution's return changes temp's TYPE, the breaking
	// direction, and is the project's most recent return.
	batch = append(batch, toolEvent("exec_drift_current", 1,
		now.Add(-30*time.Second),
		`{"temp": "hot", "city": "melbourne"}`))
	if err := st.SaveEvents(ctx, batch); err != nil {
		t.Fatalf("save events: %v", err)
	}

	h := &Handlers{Logger: logger, Store: st, HaltSubs: NewHaltSubscribers()}
	t.Cleanup(func() { h.DrainDispatches(); _ = st.Close() })
	body := `{"status":"completed"}`
	req := httptest.NewRequest("PATCH", "/executions/exec_drift_current", strings.NewReader(body))
	req.SetPathValue("id", "exec_drift_current")
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyProjectID, projectID))
	rec := httptest.NewRecorder()
	h.HandleUpdateExecution(rec, req)
	if rec.Code != 200 {
		t.Fatalf("PATCH returned %d: %s", rec.Code, rec.Body.String())
	}

	groups, err := st.ListFailureGroups(ctx, projectID, store.ListFailureGroupsOpts{Limit: 50})
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	var driftSigs []string
	for _, g := range groups {
		if g.FailureClass == store.FailureClassToolSchemaDrift {
			driftSigs = append(driftSigs, g.Signature)
		}
	}
	if len(driftSigs) != 1 || !strings.HasPrefix(driftSigs[0], toolName+":") {
		t.Errorf("want exactly one tool_schema_drift group with signature "+
			"prefix %q after a type change against a 10-call stable "+
			"majority, got %v", toolName+":", driftSigs)
	}
	// The ranked signature: a field changing type is the breaking
	// direction, and the kind must ride the signature so breaking and
	// compatible drift form separate groups.
	if len(driftSigs) == 1 && !strings.HasSuffix(driftSigs[0], ":breaking") {
		t.Errorf("drift signature %q should end in :breaking for a "+
			"number -> string type change", driftSigs[0])
	}
}

// TestHandleUpdateExecution_RanksCompatibleDriftSeparately drives the
// additive direction: a new field appears, everything the baseline
// had survives. The group signature must end in :compatible, so an
// operator can route additive drift to a quiet channel while
// breaking drift pages.
func TestHandleUpdateExecution_RanksCompatibleDriftSeparately(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	ctx := context.Background()
	const projectID = "proj_drift_compat"
	const toolName = "stock_quote"
	if err := st.CreateProject(ctx, &store.Project{
		ProjectID: projectID, Name: "compatible drift test", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	now := time.Now().UTC()
	for _, e := range []struct {
		id string
		at time.Time
	}{{"exec_compat_history", now.Add(-1 * time.Hour)}, {"exec_compat_current", now.Add(-1 * time.Minute)}} {
		if err := st.CreateExecution(ctx, &events.Execution{
			ExecutionID: e.id, ProjectID: projectID,
			Status: events.StatusStarted, StartedAt: e.at,
		}); err != nil {
			t.Fatalf("create execution %s: %v", e.id, err)
		}
	}
	var batch []events.Event
	mkEvent := func(execID string, seq int, ts time.Time, rv string) {
		payload, _ := json.Marshal(map[string]any{
			"tool_name": toolName, "return_value": json.RawMessage(rv),
		})
		batch = append(batch, events.Event{
			EventID:     fmt.Sprintf("evt_compat_%s_%d", execID, seq),
			ExecutionID: execID, EventType: "tool_call",
			Sequence: seq, Timestamp: ts, Payload: payload,
		})
	}
	for i := 1; i <= 10; i++ {
		mkEvent("exec_compat_history", i, now.Add(-time.Duration(60-i)*time.Minute),
			`{"price": 101.5}`)
	}
	// Additive: price survives with its type, currency appears.
	mkEvent("exec_compat_current", 1, now.Add(-30*time.Second),
		`{"price": 99.0, "currency": "USD"}`)
	if err := st.SaveEvents(ctx, batch); err != nil {
		t.Fatalf("save events: %v", err)
	}

	h := &Handlers{Logger: logger, Store: st, HaltSubs: NewHaltSubscribers()}
	t.Cleanup(func() { h.DrainDispatches(); _ = st.Close() })
	req := httptest.NewRequest("PATCH", "/executions/exec_compat_current",
		strings.NewReader(`{"status":"completed"}`))
	req.SetPathValue("id", "exec_compat_current")
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyProjectID, projectID))
	rec := httptest.NewRecorder()
	h.HandleUpdateExecution(rec, req)
	if rec.Code != 200 {
		t.Fatalf("PATCH returned %d: %s", rec.Code, rec.Body.String())
	}

	groups, err := st.ListFailureGroups(ctx, projectID, store.ListFailureGroupsOpts{Limit: 50})
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	var driftSigs []string
	for _, g := range groups {
		if g.FailureClass == store.FailureClassToolSchemaDrift {
			driftSigs = append(driftSigs, g.Signature)
		}
	}
	if len(driftSigs) != 1 || !strings.HasSuffix(driftSigs[0], ":compatible") {
		t.Errorf("want one drift group ending :compatible for an added "+
			"field with all baseline fields intact, got %v", driftSigs)
	}
}
