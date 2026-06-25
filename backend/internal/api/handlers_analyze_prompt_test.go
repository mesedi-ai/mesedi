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

	prompt := buildFailureGroupAnalysisPrompt(group, execs)

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

	prompt := buildFailureGroupAnalysisPrompt(group, nil)

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

	prompt := buildFailureGroupAnalysisPrompt(group, nil)

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
