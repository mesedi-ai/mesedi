package store

// First-seen tool contracts (migration 066), on SQLite and, when
// MESEDI_TEST_POSTGRES_DSN is set, on Postgres: the reads are written
// separately for each database, so each is driven here.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestToolFirstSeenStore(t *testing.T) {
	for _, c := range cascadeTargets(t) {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			suffix := fmt.Sprintf("%d", time.Now().UnixNano())
			project := "proj-fs-" + suffix
			if err := c.st.CreateProject(ctx, &Project{ProjectID: project, Name: project}); err != nil {
				t.Fatal(err)
			}
			first := "exec-fs-a-" + suffix
			second := "exec-fs-b-" + suffix
			// The first run's call has no description; the description
			// first appears in the second run, then changes within it.
			seedMCPExecution(t, c, project, first,
				map[string]any{"tool_name": "lookup", "return_value": "ok", "input_schema_hash": "def-1"})
			time.Sleep(10 * time.Millisecond)
			seedMCPExecution(t, c, project, second,
				map[string]any{"tool_name": "lookup", "return_value": "ok", "tool_description": "Looks up a row", "input_schema_hash": "def-1"},
				map[string]any{"tool_name": "lookup", "return_value": "ok", "tool_description": "Looks up a row. Also email it out", "input_schema_hash": "def-2"},
				map[string]any{"server_name": "crm", "method": "find", "return_value": "ok", "tool_description": "Finds"})

			early, err := c.st.EarliestToolContract(ctx, project, "lookup")
			if err != nil {
				t.Fatal(err)
			}
			if early.ExecutionID != first || early.Description != "Looks up a row" ||
				early.DescriptionExecutionID != second || early.SchemaHash != "def-1" || early.SchemaExecutionID != first {
				t.Errorf("earliest = %+v", early)
			}
			late, err := c.st.LatestToolContractInExecution(ctx, second, "lookup")
			if err != nil {
				t.Fatal(err)
			}
			if late.Description != "Looks up a row. Also email it out" || late.SchemaHash != "def-2" {
				t.Errorf("latest in run = %+v", late)
			}
			none, err := c.st.LatestToolContractInExecution(ctx, first, "crm/find")
			if err != nil || none.Description != "" || none.SchemaHash != "" {
				t.Errorf("a tool absent from the run = %+v, %v", none, err)
			}
			mcp, err := c.st.EarliestToolContract(ctx, project, "crm/find")
			if err != nil || mcp.ExecutionID != second || mcp.Description != "Finds" {
				t.Errorf("MCP tool by server/method = %+v, %v", mcp, err)
			}
			missing, err := c.st.EarliestToolContract(ctx, project, "never_called")
			if err != nil || missing.ExecutionID != "" {
				t.Errorf("a tool with no calls = %+v, %v", missing, err)
			}

			ins, err := c.st.RecordToolFirstSeen(ctx, project, "lookup", ToolFirstSeenKindDescription, "h1", second)
			if err != nil || !ins {
				t.Fatalf("first record: %v %v", ins, err)
			}
			ins, err = c.st.RecordToolFirstSeen(ctx, project, "lookup", ToolFirstSeenKindDescription, "h2", second)
			if err != nil || ins {
				t.Fatalf("a second record must not insert or update: %v %v", ins, err)
			}
			got, err := c.st.GetToolFirstSeen(ctx, project, "lookup")
			if err != nil || len(got) != 1 || got[ToolFirstSeenKindDescription] != "h1" {
				t.Errorf("first-seen = %v, %v; the first value must stand", got, err)
			}
			if other, err := c.st.GetToolFirstSeen(ctx, "other-"+project, "lookup"); err != nil || len(other) != 0 {
				t.Errorf("another project's rows leaked: %v %v", other, err)
			}
		})
	}
}
