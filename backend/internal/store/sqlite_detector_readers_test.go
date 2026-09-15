package store

// Coverage-debt batch five: the event readers the detectors consume,
// exercised over one richly seeded execution so each reader's field
// extraction and filtering is asserted against known rows. These are
// the queries whose silent drift makes detectors go quiet without a
// single failing test, which is precisely how the rate detector was
// found dead on arrival of its wiring test.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

func seedRichExecution(t *testing.T) (*SQLiteStore, string, string) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	const projectID, execID = "proj_dr", "exec_dr"
	if err := st.CreateProject(ctx, &Project{
		ProjectID: projectID, Name: "detector readers", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	tenant := "tenant-a"
	if err := st.CreateExecution(ctx, &events.Execution{
		ExecutionID: execID, ProjectID: projectID, TenantID: &tenant,
		Status: events.StatusStarted, StartedAt: time.Now().UTC().Add(-10 * time.Minute),
	}); err != nil {
		t.Fatalf("create execution: %v", err)
	}

	now := time.Now().UTC()
	mk := func(seq int, eventType string, payload map[string]any) events.Event {
		raw, _ := json.Marshal(payload)
		return events.Event{
			EventID: fmt.Sprintf("evt_dr_%d", seq), ExecutionID: execID,
			EventType: events.EventType(eventType), Sequence: seq,
			Timestamp: now.Add(time.Duration(seq) * time.Second), Payload: raw,
		}
	}
	evts := []events.Event{
		mk(1, "llm_call", map[string]any{"provider": "anthropic", "model": "claude-opus-4-6",
			"user_message": "find the invoice", "error_class": "timeout"}),
		mk(2, "tool_call", map[string]any{"tool_name": "db_lookup", "status": "ok",
			"return_value": map[string]any{"rows": 3}}),
		mk(3, "tool_call", map[string]any{"tool_name": "shell_exec", "status": "failed",
			"exception_type": "PermissionError"}),
		mk(4, "checkpoint", map[string]any{"state": map[string]any{"step": "planning"}, "step_number": 1}),
		mk(5, "validator_result", map[string]any{"name": "schema_check", "passed": false,
			"severity": "high", "category": "output"}),
		mk(6, "infrastructure_event", map[string]any{"event_type": "rate_limit",
			"provider": "anthropic", "reason": "rate_limit"}),
		mk(7, "eval_score", map[string]any{"evaluator_id": "ragas/faithfulness",
			"metric_type": "faithfulness", "score": 0.4, "passed": false}),
		mk(8, "human_intervention", map[string]any{"request_id": "req1",
			"question": "approve?", "response_kind": "rejected",
			"requested_at": now.Format(time.RFC3339), "decided_at": now.Format(time.RFC3339),
			"wait_duration_ms": 5000}),
		mk(9, "agent_handoff", map[string]any{"from_agent": "planner", "to_agent": "worker",
			"handoff_kind": "delegate"}),
	}
	if err := st.SaveEvents(context.Background(), evts); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	return st, projectID, execID
}

func TestDetectorReadersOverASeededExecution(t *testing.T) {
	st, projectID, execID := seedRichExecution(t)
	ctx := context.Background()

	t.Run("FindFirstFailedTool", func(t *testing.T) {
		name, exc, err := st.FindFirstFailedTool(ctx, execID)
		if err != nil || name != "shell_exec" || exc != "PermissionError" {
			t.Fatalf("= (%q, %q, %v), want the seeded failure", name, exc, err)
		}
	})
	t.Run("FindFirstFailedValidator", func(t *testing.T) {
		name, severity, category, err := st.FindFirstFailedValidator(ctx, execID)
		if err != nil || name != "schema_check" || severity != "high" || category != "output" {
			t.Fatalf("= (%q, %q, %q, %v), want the seeded validator", name, severity, category, err)
		}
	})
	t.Run("FindFirstThrottlingSignal", func(t *testing.T) {
		sig, err := st.FindFirstThrottlingSignal(ctx, execID)
		if err != nil || sig != "rate_limit:anthropic" {
			t.Fatalf("= (%q, %v), want the provider-scoped rate_limit signature", sig, err)
		}
	})
	t.Run("ListAllToolCallPayloads", func(t *testing.T) {
		payloads, err := st.ListAllToolCallPayloads(ctx, execID)
		if err != nil || len(payloads) != 2 {
			t.Fatalf("= %d payloads (%v), want both tool calls including the failed one", len(payloads), err)
		}
	})
	t.Run("ListToolNamesInExecution", func(t *testing.T) {
		// Deliberately excludes failed calls: the schema-drift
		// detector enumerates tools that returned, so the failed
		// shell_exec must NOT appear here even though it does in
		// ListAllToolCallPayloads above.
		names, err := st.ListToolNamesInExecution(ctx, execID)
		if err != nil || len(names) != 1 || names[0] != "db_lookup" {
			t.Fatalf("= %v (%v), want only the successful tool", names, err)
		}
	})
	t.Run("ListCheckpointPayloads", func(t *testing.T) {
		payloads, err := st.ListCheckpointPayloads(ctx, execID)
		if err != nil || len(payloads) != 1 {
			t.Fatalf("= %d (%v), want the seeded checkpoint", len(payloads), err)
		}
	})
	t.Run("ListLLMCallPayloads", func(t *testing.T) {
		payloads, err := st.ListLLMCallPayloads(ctx, execID)
		if err != nil || len(payloads) != 1 {
			t.Fatalf("= %d (%v), want the seeded llm_call", len(payloads), err)
		}
	})
	t.Run("ListEvalScorePayloads", func(t *testing.T) {
		payloads, err := st.ListEvalScorePayloads(ctx, execID)
		if err != nil || len(payloads) != 1 {
			t.Fatalf("= %d (%v), want the seeded eval_score", len(payloads), err)
		}
	})
	t.Run("ListHumanInterventionPayloads", func(t *testing.T) {
		payloads, err := st.ListHumanInterventionPayloads(ctx, execID)
		if err != nil || len(payloads) != 1 {
			t.Fatalf("= %d (%v), want the seeded intervention", len(payloads), err)
		}
	})
	t.Run("ListModelsForExecution", func(t *testing.T) {
		models, err := st.ListModelsForExecution(ctx, execID)
		if err != nil || len(models) != 1 || models[0] != "claude-opus-4-6" {
			t.Fatalf("= %v (%v), want the seeded model", models, err)
		}
	})
	t.Run("ListLLMUserMessagesForExecution", func(t *testing.T) {
		msgs, err := st.ListLLMUserMessagesForExecution(ctx, execID)
		if err != nil || len(msgs) != 1 || msgs[0] != "find the invoice" {
			t.Fatalf("= %v (%v), want the seeded user_message", msgs, err)
		}
	})
	t.Run("ListHandoffsWithChildStatus", func(t *testing.T) {
		handoffs, err := st.ListHandoffsWithChildStatus(ctx, execID, projectID)
		if err != nil || len(handoffs) != 1 {
			t.Fatalf("= %d (%v), want the seeded handoff", len(handoffs), err)
		}
		h := handoffs[0]
		if h.FromAgent != "planner" || h.ToAgent != "worker" || h.ChildExists {
			t.Fatalf("handoff = %+v, want planner→worker with no resolved child", h)
		}
	})
	t.Run("ListHandoffEdgesInTopology", func(t *testing.T) {
		edges, err := st.ListHandoffEdgesInTopology(ctx, execID, projectID, 0)
		if err != nil || len(edges) != 1 {
			t.Fatalf("= %d edges (%v), want the seeded handoff edge", len(edges), err)
		}
		e := edges[0]
		if e.FromAgent != "planner" || e.ToAgent != "worker" || e.EmittingExecutionID != execID {
			t.Fatalf("edge = %+v, want planner to worker emitted by the root", e)
		}
	})
	t.Run("CountHITLOutcomesInWindow", func(t *testing.T) {
		counts, err := st.CountHITLOutcomesInWindow(ctx, projectID, time.Now().UTC().Add(-time.Hour))
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if counts.TotalExecutionsWithHITL != 1 || counts.ExecutionsWithRejection != 1 || counts.ExecutionsWithEdit != 0 {
			t.Fatalf("counts = %+v, want one execution with one rejection and no edits", counts)
		}
	})
	t.Run("CountDistinctTenantsWithProviderError", func(t *testing.T) {
		n, err := st.CountDistinctTenantsWithProviderError(
			ctx, projectID, "anthropic", "timeout", time.Now().UTC().Add(-time.Hour))
		if err != nil || n != 1 {
			t.Fatalf("= (%d, %v), want the one seeded tenant", n, err)
		}
	})
	t.Run("GetExecutionTopology", func(t *testing.T) {
		nodes, err := st.GetExecutionTopology(ctx, projectID, execID, 0)
		if err != nil || len(nodes) != 1 {
			t.Fatalf("= %d nodes (%v), want the single root", len(nodes), err)
		}
		if nodes[0].ExecutionID != execID || nodes[0].Depth != 0 {
			t.Fatalf("root = %+v, want the seeded execution at depth 0", nodes[0])
		}
	})
}
