// Unit tests for buildFailureGroupAnalysisPrompt — verifies the
// playbook-injection behavior added when the AI-analysis quality
// audit (internal-extract/ai-analyses-vs-playbooks-audit.md) found
// the previous prompt was sending only failure-group metadata to
// the model with no Mesedi-specific framework. The model produced
// generic distributed-systems advice that missed allowlists,
// per-project thresholds, redaction-at-ingest guarantees, and the
// topology view.
//
// Coverage:
//   - Playbook IS found for the (failure_class, signature) pair →
//     prompt contains the playbook content AND the task instruction
//     references it.
//   - Playbook IS NOT found (registered for an unknown class) →
//     prompt falls back to its pre-playbook shape; analysis still
//     runs cleanly.
package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"mesedi/backend/internal/events"
	"mesedi/backend/internal/store"
)

func TestBuildFailureGroupAnalysisPrompt_InjectsPlaybook(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	group := &store.FailureGroup{
		FailureClass:       "data_leakage",
		Signature:          "aws_access_key",
		FirstSeen:          now.Add(-2 * time.Hour),
		LastSeen:           now,
		AffectedExecutions: 3,
		EventCount:         3,
		SampleExecutionID:  "exec-sample-1",
	}
	execs := []*events.Execution{
		{
			ExecutionID: "exec-sample-1",
			Status:      "completed",
			DurationMs:  4200,
			SDKLanguage: "python",
		},
	}

	prompt := buildFailureGroupAnalysisPrompt(group, execs, nil)

	// (1) Playbook header is present, anchoring the model on the
	//     interpretation framework before it sees the failure data.
	if !strings.Contains(prompt, "# Mesedi playbook for this failure class") {
		t.Errorf("expected playbook header in prompt; got:\n%s", prompt)
	}

	// (2) Playbook body landed — verify by checking for content
	//     unique to the data_leakage playbook (the DLP-redaction-at-
	//     ingest framing that the AI audit specifically called out
	//     as missing from the model's outputs).
	if !strings.Contains(prompt, "REDACTED") {
		t.Errorf("expected data_leakage playbook body (REDACTED marker) in prompt; got:\n%s", prompt)
	}

	// (3) Task instruction references the playbook explicitly so the
	//     model is anchored on "apply the playbook" rather than
	//     "invent a root cause from scratch."
	if !strings.Contains(prompt, "Use the Mesedi playbook above as your interpretation framework") {
		t.Errorf("expected playbook-anchored task instruction; got:\n%s", prompt)
	}

	// (4) Existing failure-group context is still present (we did
	//     not regress the pre-playbook information).
	if !strings.Contains(prompt, "**failure_class**: data_leakage") {
		t.Errorf("expected failure_class metadata in prompt; got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "exec-sample-1") {
		t.Errorf("expected sample execution id in prompt; got:\n%s", prompt)
	}

	// (5) Output-shape contract preserved (3 sections, 250 words cap).
	if !strings.Contains(prompt, "**Likely cause**") {
		t.Errorf("expected 'Likely cause' section in task instructions; got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Keep the entire output under 250 words") {
		t.Errorf("expected 250-word cap in task instructions; got:\n%s", prompt)
	}
}

func TestBuildFailureGroupAnalysisPrompt_FallsBackWhenPlaybookMissing(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	group := &store.FailureGroup{
		// Unknown failure class — playbooks.Resolve returns false,
		// playbooks.Load returns ErrNotFound. The prompt builder
		// should silently fall back to its pre-playbook shape.
		FailureClass:       "nonexistent_class_for_test",
		Signature:          "synthetic",
		FirstSeen:          now.Add(-1 * time.Hour),
		LastSeen:           now,
		AffectedExecutions: 1,
		EventCount:         1,
	}

	prompt := buildFailureGroupAnalysisPrompt(group, nil, nil)

	// (1) Playbook header is NOT present — we fell back.
	if strings.Contains(prompt, "# Mesedi playbook for this failure class") {
		t.Errorf("did not expect playbook header on fallback path; got:\n%s", prompt)
	}

	// (2) Playbook-anchored task instruction is NOT present — the
	//     fallback uses the original prompt that doesn't reference
	//     a playbook.
	if strings.Contains(prompt, "Use the Mesedi playbook above") {
		t.Errorf("did not expect playbook-anchored instruction on fallback path; got:\n%s", prompt)
	}

	// (3) Original prompt structure intact — failure-group metadata
	//     + the standard task instruction still ship.
	if !strings.Contains(prompt, "# Failure group context") {
		t.Errorf("expected failure_group context section on fallback path; got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "**Likely cause**") {
		t.Errorf("expected 'Likely cause' task instruction on fallback path; got:\n%s", prompt)
	}
}

func TestAnalysisSystemPrompt_MentionsKnobsAndSignatureDecomposition(t *testing.T) {
	t.Parallel()
	// Wave K.1 — the two tail sentences in analysisSystemPrompt are
	// what lift the two B-grade analyses (context_overflow,
	// cost_velocity) to A- in the post-audit. If either of these
	// sentences ever drops out of the constant, this test fails
	// loudly so the system-prompt regression doesn't ship silently.

	// (1) The per-project tuning knobs nudge. The exact phrasing
	//     matters less than the keywords — if the wording changes,
	//     update the test to match, but the intent (mention knobs
	//     by name when relevant) must remain.
	if !strings.Contains(analysisSystemPrompt, "per-project tuning knobs") {
		t.Errorf("expected analysisSystemPrompt to mention 'per-project tuning knobs'; got:\n%s", analysisSystemPrompt)
	}
	if !strings.Contains(analysisSystemPrompt, "custom model windows") {
		t.Errorf("expected analysisSystemPrompt to name custom_model_windows as a sample knob; got:\n%s", analysisSystemPrompt)
	}

	// (2) The signature decomposition nudge. Examples used in the
	//     prompt act as anchors so the model parses bucket / level /
	//     model / agent pair rather than treating the signature as
	//     an opaque string.
	if !strings.Contains(analysisSystemPrompt, "signature decomposes") {
		t.Errorf("expected analysisSystemPrompt to mention 'signature decomposes'; got:\n%s", analysisSystemPrompt)
	}
	if !strings.Contains(analysisSystemPrompt, "cost_$0.10+") {
		t.Errorf("expected analysisSystemPrompt to use cost_$0.10+ as an anchor example; got:\n%s", analysisSystemPrompt)
	}

	// (3) The existing system prompt directives are preserved
	//     (regression guard — the new instructions extend, they
	//     don't replace).
	if !strings.Contains(analysisSystemPrompt, "Be precise, opinionated") {
		t.Errorf("expected pre-existing 'Be precise, opinionated' directive to remain; got:\n%s", analysisSystemPrompt)
	}
	if !strings.Contains(analysisSystemPrompt, "frame recommendations as hypotheses") {
		t.Errorf("expected pre-existing 'frame recommendations as hypotheses' directive to remain; got:\n%s", analysisSystemPrompt)
	}
}

func TestBuildFailureGroupAnalysisPrompt_HandlesSubSignaturePlaybook(t *testing.T) {
	t.Parallel()
	// The playbooks registry maps loops/similar_call_<hex> →
	// loops/similar_call.md via prefix matching. Verify the right
	// sub-playbook is selected, not the (nonexistent) default.
	now := time.Now().UTC()
	group := &store.FailureGroup{
		FailureClass:       "loops",
		Signature:          "similar_call_902f7be6",
		FirstSeen:          now.Add(-2 * time.Hour),
		LastSeen:           now,
		AffectedExecutions: 2,
		EventCount:         2,
	}

	prompt := buildFailureGroupAnalysisPrompt(group, nil, nil)

	// The similar_call playbook is specifically titled with that
	// phrase; the identical_call sibling playbook is titled
	// "Identical-call loop." Catching the right one proves prefix
	// matching works.
	if !strings.Contains(prompt, "Similar-call loop") {
		t.Errorf("expected similar_call playbook in prompt; got:\n%s", prompt)
	}
	if strings.Contains(prompt, "Identical-call loop") {
		t.Errorf("did not expect identical_call playbook for similar_call signature; got:\n%s", prompt)
	}
}

func TestBuildFailureGroupAnalysisPrompt_RendersSampleEvents(t *testing.T) {
	t.Parallel()
	// Wave K.2 — verifies that when sampleEvents is non-empty, the
	// prompt includes a `## Sample events from execution <id>` section
	// with event type, sequence, timestamp, duration, and payload (with
	// 500-char truncation). This is the structural difference that
	// justifies the $0.75 AI analysis charge: the playbook can't know
	// about specific tool_call return shapes; the AI can, because it
	// sees the actual event payloads.

	now := time.Now().UTC()
	group := &store.FailureGroup{
		FailureClass:       "tool_failures",
		Signature:          "tool_failure:fetch_item",
		FirstSeen:          now.Add(-1 * time.Hour),
		LastSeen:           now,
		AffectedExecutions: 1,
		EventCount:         2,
		SampleExecutionID:  "exec-evt-1",
	}
	execs := []*events.Execution{
		{
			ExecutionID: "exec-evt-1",
			Status:      "failed",
			DurationMs:  1234,
			SDKLanguage: "python",
		},
	}

	// Two events: a small payload (no truncation expected) and a
	// large payload (truncation expected — past the 500-char cap).
	smallPayload, _ := json.Marshal(map[string]any{
		"tool_name": "fetch_item",
		"error":     "timeout",
	})
	largePayload := []byte(`{"prompt":"` + strings.Repeat("x", 600) + `"}`)

	evs := []*events.Event{
		{
			EventID:     "evt-1",
			ExecutionID: "exec-evt-1",
			EventType:   "tool_call",
			Sequence:    1,
			Timestamp:   now.Add(-5 * time.Minute),
			DurationMs:  42,
			Payload:     smallPayload,
		},
		{
			EventID:     "evt-2",
			ExecutionID: "exec-evt-1",
			EventType:   "llm_call",
			Sequence:    2,
			Timestamp:   now.Add(-4 * time.Minute),
			DurationMs:  1800,
			Payload:     largePayload,
		},
	}

	prompt := buildFailureGroupAnalysisPrompt(group, execs, evs)

	// (1) Events section header is anchored on the execution ID so
	//     operators reading the analysis can correlate it with the
	//     execution detail page.
	if !strings.Contains(prompt, "## Sample events from execution exec-evt-1") {
		t.Errorf("expected events section header in prompt; got:\n%s", prompt)
	}

	// (2) Both event types land — the model needs the type to know
	//     how to interpret the payload schema.
	if !strings.Contains(prompt, "tool_call") {
		t.Errorf("expected tool_call event type in prompt; got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "llm_call") {
		t.Errorf("expected llm_call event type in prompt; got:\n%s", prompt)
	}

	// (3) Small payload renders in full (substring of the marshalled
	//     JSON is present).
	if !strings.Contains(prompt, `"tool_name":"fetch_item"`) {
		t.Errorf("expected small payload to render in full; got:\n%s", prompt)
	}

	// (4) Large payload is truncated. The truncation marker must
	//     appear AND the full 600-char prompt body must NOT be in
	//     the rendered output (we only allow MaxEventPayloadCharsInPrompt = 500 chars).
	if !strings.Contains(prompt, "...[truncated]") {
		t.Errorf("expected truncation marker for large payload; got:\n%s", prompt)
	}
	if strings.Contains(prompt, strings.Repeat("x", 600)) {
		t.Errorf("large payload should be truncated; full 600-char body should not be in prompt")
	}

	// (5) Sequence numbers ship — for ordered reasoning ("the third
	//     call was the one that failed").
	if !strings.Contains(prompt, "seq=1") {
		t.Errorf("expected seq=1 in events section; got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "seq=2") {
		t.Errorf("expected seq=2 in events section; got:\n%s", prompt)
	}
}

func TestBuildFailureGroupAnalysisPrompt_OmitsEventsSectionWhenEmpty(t *testing.T) {
	t.Parallel()
	// Wave K.2 — when sampleEvents is nil/empty (event-fetch failed,
	// or the execution had no captured events), the prompt must
	// silently omit the events section. The pre-K.2 prompt shape is
	// preserved exactly so legacy analyses still ship cleanly.

	now := time.Now().UTC()
	group := &store.FailureGroup{
		FailureClass:       "tool_failures",
		Signature:          "tool_failure:fetch_item",
		FirstSeen:          now.Add(-1 * time.Hour),
		LastSeen:           now,
		AffectedExecutions: 1,
		EventCount:         0,
		SampleExecutionID:  "exec-empty-1",
	}
	execs := []*events.Execution{
		{ExecutionID: "exec-empty-1", Status: "failed"},
	}

	prompt := buildFailureGroupAnalysisPrompt(group, execs, nil)

	if strings.Contains(prompt, "## Sample events from execution") {
		t.Errorf("did not expect events section when sampleEvents is nil; got:\n%s", prompt)
	}
	// Sanity — the rest of the prompt is intact.
	if !strings.Contains(prompt, "**Likely cause**") {
		t.Errorf("expected task instruction to still be present; got:\n%s", prompt)
	}
}

func TestBuildFailureGroupAnalysisPrompt_CapsAtMaxSampleEvents(t *testing.T) {
	t.Parallel()
	// Wave K.2 — when sampleEvents exceeds MaxSampleEventsInPrompt,
	// the prompt renders only the first MaxSampleEventsInPrompt and
	// includes a "showing first N of M events" footer so the model
	// knows it's looking at a truncated window.

	now := time.Now().UTC()
	group := &store.FailureGroup{
		FailureClass:       "tool_failures",
		Signature:          "tool_failure:fetch_item",
		FirstSeen:          now.Add(-1 * time.Hour),
		LastSeen:           now,
		AffectedExecutions: 1,
		EventCount:         50,
		SampleExecutionID:  "exec-cap-1",
	}
	execs := []*events.Execution{
		{ExecutionID: "exec-cap-1", Status: "failed"},
	}

	// Build MaxSampleEventsInPrompt+5 events so we can confirm the cap.
	overflowCount := MaxSampleEventsInPrompt + 5
	evs := make([]*events.Event, overflowCount)
	for i := 0; i < overflowCount; i++ {
		evs[i] = &events.Event{
			EventID:     "evt-cap",
			ExecutionID: "exec-cap-1",
			EventType:   "tool_call",
			Sequence:    i + 1,
			Timestamp:   now,
			Payload:     []byte(`{}`),
		}
	}

	prompt := buildFailureGroupAnalysisPrompt(group, execs, evs)

	// (1) Footer present, mentioning both the cap and the actual count.
	if !strings.Contains(prompt, "showing first 30 of 35 events") {
		t.Errorf("expected truncation footer mentioning 30 of 35 events; got:\n%s", prompt)
	}

	// (2) Last in-cap event's sequence number lands.
	if !strings.Contains(prompt, "seq=30") {
		t.Errorf("expected seq=30 (the last in-cap event) in prompt; got:\n%s", prompt)
	}

	// (3) First over-cap event's sequence number does NOT land.
	if strings.Contains(prompt, "seq=31") {
		t.Errorf("did not expect seq=31 (over the cap) in prompt; got:\n%s", prompt)
	}
}
