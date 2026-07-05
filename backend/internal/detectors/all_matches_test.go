package detectors

import (
	"encoding/json"
	"testing"
)

// All-matches-recorded wave tests (closes sandbox_escape.G1 +
// grounding_failure.G2 + hitl_timeout.G3). One test file covering
// all 3 detectors' new AllMatches variants — same code shape so
// the tests are best read together.

// ──────────────────────────────────────────────────────────────────────
// sandbox_escape — DetectSandboxEscapeAllMatchesWithCustom
// ──────────────────────────────────────────────────────────────────────

func Test_DetectSandboxEscapeAllMatches_EmptyInput(t *testing.T) {
	got := DetectSandboxEscapeAllMatchesWithCustom(nil, nil)
	if got != nil {
		t.Errorf("empty payloads should return nil, got %v", got)
	}
}

func Test_DetectSandboxEscapeAllMatches_MultiplePatterns(t *testing.T) {
	// Tool call argument that matches python_os_import AND
	// host_secret_read in one payload. Both should fire.
	payload := json.RawMessage(`{
		"arguments": "import os\nopen('.aws/credentials').read()"
	}`)
	matches := DetectSandboxEscapeAllMatchesWithCustom(
		[]json.RawMessage{payload}, nil,
	)
	if len(matches) < 2 {
		t.Errorf("expected ≥2 matches, got %d: %v", len(matches), matches)
	}
	seen := make(map[string]bool)
	for _, m := range matches {
		seen[m.Signature] = true
	}
	if !seen["sandbox_escape:python_os_import"] {
		t.Errorf("missing python_os_import match in %v", matches)
	}
	if !seen["sandbox_escape:host_secret_read"] {
		t.Errorf("missing host_secret_read match in %v", matches)
	}
}

func Test_DetectSandboxEscapeAllMatches_DedupSameSig(t *testing.T) {
	// Two payloads both matching python_os_import → one match.
	payloads := []json.RawMessage{
		json.RawMessage(`{"arguments": "import os"}`),
		json.RawMessage(`{"arguments": "from os import path"}`),
	}
	matches := DetectSandboxEscapeAllMatchesWithCustom(payloads, nil)
	if len(matches) != 1 {
		t.Errorf("expected exactly 1 dedup'd match, got %d: %v", len(matches), matches)
	}
	if matches[0].Signature != "sandbox_escape:python_os_import" {
		t.Errorf("wrong sig: %q", matches[0].Signature)
	}
}

// ──────────────────────────────────────────────────────────────────────
// grounding_failure — DetectGroundingFailureAllMatchesWithThresholds
// ──────────────────────────────────────────────────────────────────────

func Test_DetectGroundingFailureAllMatches_EmptyInput(t *testing.T) {
	got := DetectGroundingFailureAllMatchesWithThresholds(nil, DefaultGroundingFailureThresholds())
	if got != nil {
		t.Errorf("empty payloads should return nil, got %v", got)
	}
}

func Test_DetectGroundingFailureAllMatches_MultipleExplicitFails(t *testing.T) {
	// 2 different evaluators both reporting passed=false. Both
	// should fire as distinct clusters.
	payloads := []json.RawMessage{
		json.RawMessage(`{"evaluator_id":"ragas/faithfulness","metric_type":"faithfulness","passed":false}`),
		json.RawMessage(`{"evaluator_id":"promptfoo","metric_type":"relevance","passed":false}`),
	}
	sigs := DetectGroundingFailureAllMatchesWithThresholds(payloads, DefaultGroundingFailureThresholds())
	if len(sigs) < 2 {
		t.Errorf("expected ≥2 matches, got %d: %v", len(sigs), sigs)
	}
}

func Test_DetectGroundingFailureAllMatches_DedupSameEvalMetric(t *testing.T) {
	// Same (evaluator, metric) failing twice → one cluster.
	payloads := []json.RawMessage{
		json.RawMessage(`{"evaluator_id":"ragas","metric_type":"faithfulness","passed":false}`),
		json.RawMessage(`{"evaluator_id":"ragas","metric_type":"faithfulness","passed":false}`),
	}
	sigs := DetectGroundingFailureAllMatchesWithThresholds(payloads, DefaultGroundingFailureThresholds())
	if len(sigs) != 1 {
		t.Errorf("dedup failed: got %d sigs, want 1: %v", len(sigs), sigs)
	}
}

// ──────────────────────────────────────────────────────────────────────
// hitl_timeout — DetectHITLTimeoutAllMatches
// ──────────────────────────────────────────────────────────────────────

func Test_DetectHITLTimeoutAllMatches_EmptyInput(t *testing.T) {
	got := DetectHITLTimeoutAllMatches(nil)
	if got != nil {
		t.Errorf("empty payloads should return nil, got %v", got)
	}
}

func Test_DetectHITLTimeoutAllMatches_BothConditionsFire(t *testing.T) {
	// One execution with BOTH an explicit timeout AND an SLA
	// exceedance. Legacy first-match suppressed the SLA cluster;
	// all-matches surfaces both.
	payloads := []json.RawMessage{
		json.RawMessage(`{"response_kind":"timeout"}`),
		json.RawMessage(`{"sla_seconds":10,"wait_duration_ms":20000}`),
	}
	sigs := DetectHITLTimeoutAllMatches(payloads)
	if len(sigs) != 2 {
		t.Errorf("expected 2 sigs (explicit + sla_exceeded), got %d: %v", len(sigs), sigs)
	}
	if sigs[0] != "hitl_timeout:explicit" {
		t.Errorf("expected explicit first, got %q", sigs[0])
	}
	if sigs[1] != "hitl_timeout:sla_exceeded" {
		t.Errorf("expected sla_exceeded second, got %q", sigs[1])
	}
}

func Test_DetectHITLTimeoutAllMatches_OnlyExplicit(t *testing.T) {
	// Just an explicit timeout — only that one sig.
	payloads := []json.RawMessage{
		json.RawMessage(`{"response_kind":"timeout"}`),
	}
	sigs := DetectHITLTimeoutAllMatches(payloads)
	if len(sigs) != 1 || sigs[0] != "hitl_timeout:explicit" {
		t.Errorf("expected [explicit], got %v", sigs)
	}
}
