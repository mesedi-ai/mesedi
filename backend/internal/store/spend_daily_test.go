package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

// The query is written separately for each database.
func TestListLLMCallCostsInRange(t *testing.T) {
	for _, c := range cascadeTargets(t) {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			sfx := fmt.Sprintf("%d", time.Now().UnixNano())
			project, exec := "proj-spend-"+sfx, "exec-spend-"+sfx
			if err := c.st.CreateProject(ctx, &Project{ProjectID: project, Name: project}); err != nil {
				t.Fatal(err)
			}
			at := time.Date(2026, 11, 10, 23, 30, 0, 0, time.UTC)
			if err := c.st.CreateExecution(ctx, &events.Execution{ExecutionID: exec, ProjectID: project, Status: "completed", StartedAt: at}); err != nil {
				t.Fatal(err)
			}
			mk := func(i int, ts time.Time, typ events.EventType, p string) events.Event {
				return events.Event{EventID: fmt.Sprintf("%s-%d", exec, i), ExecutionID: exec, EventType: typ, Sequence: i,
					Timestamp: ts, Payload: json.RawMessage(p)}
			}
			if err := c.st.SaveEvents(ctx, []events.Event{
				mk(1, at, events.EventTypeLLMCall, `{"model":"m1","provider":"openai","input_tokens":10,"output_tokens":20,"estimated_cost_usd":0.5}`),
				mk(2, at.Add(time.Hour), events.EventTypeLLMCall, `{"model":"m2"}`),
				mk(3, at, events.EventTypeToolCall, `{"tool_name":"t"}`),
				mk(4, at.Add(-48*time.Hour), events.EventTypeLLMCall, `{"model":"old"}`),
			}); err != nil {
				t.Fatal(err)
			}
			got, err := c.st.ListLLMCallCostsInRange(ctx, project, at.Add(-time.Hour), at.Add(2*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 {
				t.Fatalf("got %+v; want the two model calls in range", got)
			}
			byModel := map[string]LLMCallCost{}
			for _, g := range got {
				byModel[g.Model] = g
			}
			m1 := byModel["m1"]
			if m1.Provider != "openai" || m1.InputTokens != 10 || m1.OutputTokens != 20 || m1.EstimatedCostUSD != 0.5 || !m1.At.Equal(at) {
				t.Errorf("m1 = %+v", m1)
			}
			if m2 := byModel["m2"]; m2.Provider != "" || m2.InputTokens != 0 || m2.At.Format("2006-01-02") != "2026-11-11" {
				t.Errorf("m2 = %+v", m2)
			}
		})
	}
}
