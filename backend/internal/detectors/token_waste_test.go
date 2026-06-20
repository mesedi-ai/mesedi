// Tests for the token_waste detector. Coverage targets:
//   - Three identical user_messages fire detection.
//   - Two identical user_messages (under threshold) do NOT fire.
//   - Different short suffixes on the same long prefix still cluster
//     (this is the actual production accumulation pattern).
//   - Empty / malformed payloads no-op cleanly.
//   - Signatures are deterministic across iteration order.
package detectors

import (
	"encoding/json"
	"strings"
	"testing"
)

func userPrompt(text string) json.RawMessage {
	// Field name is `user_message` to match what the SDK ships;
	// changed alongside the detector when the integration suite
	// caught the user_prompt/user_message mismatch. Helper name kept
	// as userPrompt for callsite churn-minimization.
	return json.RawMessage(`{"user_message":` + jsonString(text) + `}`)
}

// jsonString escapes a Go string into a JSON string literal.
// Minimal, only the test inputs we care about. Saves importing
// encoding/json's Marshal in test helpers.
func jsonString(s string) string {
	out := []byte{'"'}
	for _, r := range s {
		switch r {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		case '\n':
			out = append(out, '\\', 'n')
		default:
			out = append(out, []byte(string(r))...)
		}
	}
	out = append(out, '"')
	return string(out)
}

func Test_TokenWaste_FiresAtThreshold(t *testing.T) {
	payloads := []json.RawMessage{
		userPrompt("Summarize this article: ..."),
		userPrompt("Summarize this article: ..."),
		userPrompt("Summarize this article: ..."),
	}
	sig, detected := DetectTokenWaste(payloads)
	if !detected {
		t.Fatalf("expected detection at 3 identical prompts, got none")
	}
	if !strings.HasPrefix(sig, "token_waste:") {
		t.Errorf("signature missing canonical prefix: %q", sig)
	}
}

func Test_TokenWaste_DoesNotFireBelowThreshold(t *testing.T) {
	payloads := []json.RawMessage{
		userPrompt("Summarize this article: ..."),
		userPrompt("Summarize this article: ..."),
	}
	if _, detected := DetectTokenWaste(payloads); detected {
		t.Errorf("did not expect detection at 2 identical prompts")
	}
}

func Test_TokenWaste_AccumulationPattern(t *testing.T) {
	// The real production case: same leading context, different
	// trailing question. All three should cluster as token_waste
	// because their leading 2048 chars are identical.
	prefix := strings.Repeat("System context that the agent re-sends every turn. ", 50)
	payloads := []json.RawMessage{
		userPrompt(prefix + "Turn 1: please do X."),
		userPrompt(prefix + "Turn 2: please do Y."),
		userPrompt(prefix + "Turn 3: please do Z."),
	}
	sig, detected := DetectTokenWaste(payloads)
	if !detected {
		t.Fatalf("expected detection on accumulation pattern, got none. sig=%q", sig)
	}
}

func Test_TokenWaste_DistinctPromptsDoNotFire(t *testing.T) {
	payloads := []json.RawMessage{
		userPrompt("What is the weather in NYC?"),
		userPrompt("Translate 'hello' to French."),
		userPrompt("Suggest a baby name starting with R."),
	}
	if _, detected := DetectTokenWaste(payloads); detected {
		t.Errorf("did not expect detection across distinct prompts")
	}
}

func Test_TokenWaste_MixedSequence(t *testing.T) {
	payloads := []json.RawMessage{
		userPrompt("Wasted prompt body content goes here."),
		userPrompt("Different prompt 1"),
		userPrompt("Wasted prompt body content goes here."),
		userPrompt("Different prompt 2"),
		userPrompt("Wasted prompt body content goes here."),
	}
	sig, detected := DetectTokenWaste(payloads)
	if !detected {
		t.Fatalf("expected detection on 3-of-5 wasted, got none")
	}
	if !strings.HasPrefix(sig, "token_waste:") {
		t.Errorf("signature shape: %q", sig)
	}
}

func Test_TokenWaste_DeterministicSignature(t *testing.T) {
	payloads := []json.RawMessage{
		userPrompt("repeating content"),
		userPrompt("repeating content"),
		userPrompt("repeating content"),
	}
	first, _ := DetectTokenWaste(payloads)
	for i := 0; i < 50; i++ {
		got, _ := DetectTokenWaste(payloads)
		if got != first {
			t.Fatalf("signature drift on iteration %d: %q vs %q", i, got, first)
		}
	}
}

func Test_TokenWaste_EmptyInputs(t *testing.T) {
	cases := []struct {
		name     string
		payloads []json.RawMessage
	}{
		{"nil", nil},
		{"empty", []json.RawMessage{}},
		{"single", []json.RawMessage{userPrompt("solo prompt")}},
		{"two only", []json.RawMessage{
			userPrompt("only twice"),
			userPrompt("only twice"),
		}},
		{"no user_message field", []json.RawMessage{
			json.RawMessage(`{"system_prompt":"foo"}`),
			json.RawMessage(`{"system_prompt":"foo"}`),
			json.RawMessage(`{"system_prompt":"foo"}`),
		}},
		{"malformed", []json.RawMessage{
			json.RawMessage(`{bad`),
			json.RawMessage(`{bad`),
			json.RawMessage(`{bad`),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, detected := DetectTokenWaste(tc.payloads); detected {
				t.Errorf("expected no detection on %s input", tc.name)
			}
		})
	}
}
