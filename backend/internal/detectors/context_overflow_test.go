// Tests for the context_overflow detector. Coverage targets:
//   - High-water input_tokens at 90% fires WARN, at 100% fires FAIL.
//   - Multi-model executions report the worst offender.
//   - Unknown models (not in the context-window registry) silently
//     no-op rather than over-firing.
//   - Signatures are deterministic across iteration order.
package detectors

import (
	"encoding/json"
	"strings"
	"testing"
)

func llm(model string, inputTokens int) json.RawMessage {
	return json.RawMessage(`{"model":"` + model + `","input_tokens":` + itoa(inputTokens) + `}`)
}

func itoa(n int) string {
	// tiny stdlib-free helper so tests stay focused on the detector,
	// not on number formatting.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

func Test_ContextOverflow_FiresAtFail(t *testing.T) {
	// claude-sonnet-4-6 = 200k. 200_000 tokens hits 100%.
	payloads := []json.RawMessage{
		llm("claude-sonnet-4-6", 200_000),
	}
	sig, detected := DetectContextOverflow(payloads)
	if !detected {
		t.Fatalf("expected detection at 100%% utilization, got none")
	}
	if !strings.HasPrefix(sig, "context_overflow:fail:") {
		t.Errorf("expected fail-level signature, got %q", sig)
	}
}

func Test_ContextOverflow_FiresAtWarn(t *testing.T) {
	// 180_000 / 200_000 = 90%.
	payloads := []json.RawMessage{
		llm("claude-sonnet-4-6", 180_000),
	}
	sig, detected := DetectContextOverflow(payloads)
	if !detected {
		t.Fatalf("expected detection at 90%% utilization, got none")
	}
	if !strings.HasPrefix(sig, "context_overflow:warn:") {
		t.Errorf("expected warn-level signature, got %q", sig)
	}
}

func Test_ContextOverflow_BelowThreshold(t *testing.T) {
	// 179_999 tokens = 89.9% on a 200k window, below warn.
	payloads := []json.RawMessage{
		llm("claude-sonnet-4-6", 179_999),
	}
	if _, detected := DetectContextOverflow(payloads); detected {
		t.Errorf("did not expect detection below warn threshold")
	}
}

func Test_ContextOverflow_UnknownModelSkipped(t *testing.T) {
	// Unknown model: registry returns false, detector should not
	// fire even at apparent-high token count.
	payloads := []json.RawMessage{
		llm("definitely-not-a-real-model", 1_000_000),
	}
	if _, detected := DetectContextOverflow(payloads); detected {
		t.Errorf("expected detector to skip unknown models, but it fired")
	}
}

func Test_ContextOverflow_HighWaterMark(t *testing.T) {
	// Multiple llm_calls; the highest one drives the verdict, not
	// the last one or an average.
	payloads := []json.RawMessage{
		llm("claude-sonnet-4-6", 10_000),
		llm("claude-sonnet-4-6", 200_000), // 100% on this turn
		llm("claude-sonnet-4-6", 50_000),
	}
	sig, detected := DetectContextOverflow(payloads)
	if !detected {
		t.Fatalf("expected detection on high-water mark, got none")
	}
	if !strings.HasPrefix(sig, "context_overflow:fail:") {
		t.Errorf("expected fail-level signature on high-water 100%%, got %q", sig)
	}
}

func Test_ContextOverflow_FailWinsOverWarn(t *testing.T) {
	// Same execution uses two models; one is at warn level, the
	// other at fail level. The fail signal must win.
	payloads := []json.RawMessage{
		llm("claude-sonnet-4-6", 180_000), // warn (90%)
		llm("claude-haiku-4-5", 200_000),  // fail (100%)
	}
	sig, detected := DetectContextOverflow(payloads)
	if !detected {
		t.Fatalf("expected detection, got none")
	}
	if !strings.HasPrefix(sig, "context_overflow:fail:") {
		t.Errorf("expected fail to win over warn, got %q", sig)
	}
}

func Test_ContextOverflow_DeterministicSignature(t *testing.T) {
	payloads := []json.RawMessage{
		llm("claude-sonnet-4-6", 200_000),
	}
	first, _ := DetectContextOverflow(payloads)
	for i := 0; i < 50; i++ {
		got, _ := DetectContextOverflow(payloads)
		if got != first {
			t.Fatalf("signature drift on iteration %d: %q vs %q", i, got, first)
		}
	}
}

func Test_ContextOverflow_EmptyInputs(t *testing.T) {
	cases := []struct {
		name     string
		payloads []json.RawMessage
	}{
		{"nil", nil},
		{"empty", []json.RawMessage{}},
		{"zero tokens", []json.RawMessage{llm("claude-sonnet-4-6", 0)}},
		{"no model", []json.RawMessage{json.RawMessage(`{"input_tokens":150000}`)}},
		{"malformed", []json.RawMessage{json.RawMessage(`{bad`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, detected := DetectContextOverflow(tc.payloads); detected {
				t.Errorf("expected no detection on %s input", tc.name)
			}
		})
	}
}
