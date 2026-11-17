package store

// MCP calls reach every tool check (radar gap G29, 17 November 2026).
// Until then every tool query read event_type 'tool_call' only, so an
// MCP call, reported as 'mcp_call' with its own field names, was read by
// no check. Migration 065's tool_events view presents each MCP call under
// the tool-call field names; these tests drive every tool query through
// it, on SQLite and, when MESEDI_TEST_POSTGRES_DSN is set, on Postgres.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

func seedMCPExecution(t *testing.T, c cascadeTarget, project, exec string, calls ...map[string]any) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	end := now.Add(time.Minute)
	if err := c.st.CreateExecution(ctx, &events.Execution{ExecutionID: exec, ProjectID: project,
		Status: "completed", StartedAt: now, EndedAt: &end}); err != nil {
		t.Fatal(err)
	}
	evts := make([]events.Event, 0, len(calls))
	for i, p := range calls {
		kind := events.EventTypeMCPCall
		if _, ok := p["tool_name"]; ok {
			kind = events.EventTypeToolCall
		}
		raw, _ := json.Marshal(p)
		evts = append(evts, events.Event{EventID: fmt.Sprintf("%s-e%d", exec, i), ExecutionID: exec,
			EventType: kind, Sequence: i + 1, Timestamp: now.Add(time.Duration(i) * time.Second), Payload: raw})
	}
	if err := c.st.SaveEvents(ctx, evts); err != nil {
		t.Fatal(err)
	}
}

func TestMCPCallsReachEveryToolQuery(t *testing.T) {
	for _, c := range cascadeTargets(t) {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			suffix := fmt.Sprintf("%d", time.Now().UnixNano())
			project := "proj-mcp-" + suffix
			if err := c.st.CreateProject(ctx, &Project{ProjectID: project, Name: project}); err != nil {
				t.Fatal(err)
			}
			read := map[string]any{"server_name": "github", "method": "read_file",
				"arguments": map[string]any{"path": "/etc/passwd"}, "return_value": map[string]any{"text": "root:x:0:0"},
				"input_schema_hash": "abc123", "tool_description": "Reads a file"}
			older := "exec-mcp-a-" + suffix
			seedMCPExecution(t, c, project, older, read)
			exec := "exec-mcp-b-" + suffix
			seedMCPExecution(t, c, project, exec,
				map[string]any{"tool_name": "local_tool", "status": "ok"},
				map[string]any{"server_name": "github", "method": "delete_repo", "error": "forbidden", "error_class": "hard_error"},
				read)

			name, exc, err := c.st.FindFirstFailedTool(ctx, exec)
			if err != nil || name != "github/delete_repo" || exc != "hard_error" {
				t.Errorf("first failed tool = (%q, %q, %v), want the failed MCP call", name, exc, err)
			}
			names, err := c.st.ListToolNamesInExecution(ctx, exec)
			if err != nil || !contains(names, "github/read_file") || !contains(names, "local_tool") || contains(names, "github/delete_repo") {
				t.Errorf("tool names = %v, %v; want both successful tools and not the failed one", names, err)
			}
			payloads, err := c.st.ListAllToolCallPayloads(ctx, exec)
			if err != nil || len(payloads) != 3 {
				t.Fatalf("all tool payloads = %d, %v; want 3, the MCP calls included", len(payloads), err)
			}
			var mcp struct {
				ToolName  string         `json:"tool_name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(payloads[2], &mcp); err != nil || mcp.ToolName != "github/read_file" || mcp.Arguments["path"] != "/etc/passwd" {
				t.Errorf("MCP payload read through the view = %s (%v); arguments must stay structured", payloads[2], err)
			}
			hashes, err := c.st.ListToolInputSchemaHashes(ctx, project, "github/read_file", exec, 10)
			if err != nil || len(hashes) != 1 || hashes[0] != "abc123" {
				t.Errorf("schema hashes = %v, %v; want the MCP call's", hashes, err)
			}
			descs, err := c.st.ListToolDescriptions(ctx, project, "github/read_file", exec, 10)
			if err != nil || len(descs) != 1 || descs[0] != "Reads a file" {
				t.Errorf("descriptions = %v, %v; want the MCP call's", descs, err)
			}
			returns, err := c.st.ListSuccessfulToolReturns(ctx, project, "github/read_file", exec, 10)
			if err != nil || len(returns) != 1 || !strings.Contains(string(returns[0]), "root:x:0:0") {
				t.Errorf("successful returns = %q, %v; want the MCP call's", returns, err)
			}
			counts, err := c.st.ListToolCallCountsForProject(ctx, project)
			found := false
			for _, cnt := range counts {
				found = found || (cnt.ToolName == "github/read_file" && cnt.CallCount == 2)
			}
			if err != nil || !found {
				t.Errorf("tool call counts = %+v, %v; want github/read_file twice", counts, err)
			}
		})
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
