// Tests for the semantic_loop detector. Coverage targets:
//
//   - Three identical state payloads fire detection.
//   - Two identical state payloads (under threshold) do NOT fire.
//   - State payloads that LOOK distinct but canonicalize identically
//     (different key order, different whitespace, mixed casing in
//     strings) still cluster correctly.
//   - Canonicalization is deterministic: the same logical input
//     always produces the same hash.
//   - Empty / malformed payloads no-op cleanly.
//   - Signatures are stable across runs (regression guard for the
//     dashboard's "Flagged by" banner deep-link).
package detectors

import (
	"encoding/json"
	"strings"
	"testing"
)

func cp(state string) json.RawMessage {
	return json.RawMessage(`{"state":` + state + `}`)
}

func Test_SemanticLoop_FiresAtThreshold(t *testing.T) {
	payloads := []json.RawMessage{
		cp(`{"query":"weather in NYC"}`),
		cp(`{"query":"weather in NYC"}`),
		cp(`{"query":"weather in NYC"}`),
	}
	sig, detected := DetectSemanticLoop(payloads)
	if !detected {
		t.Fatalf("expected detection at 3 identical states, got none")
	}
	if !strings.HasPrefix(sig, "semantic_loop:") {
		t.Errorf("signature missing canonical prefix: %q", sig)
	}
}

func Test_SemanticLoop_DoesNotFireBelowThreshold(t *testing.T) {
	payloads := []json.RawMessage{
		cp(`{"query":"weather in NYC"}`),
		cp(`{"query":"weather in NYC"}`),
	}
	_, detected := DetectSemanticLoop(payloads)
	if detected {
		t.Errorf("did not expect detection at 2 identical states")
	}
}

func Test_SemanticLoop_KeyOrderInsensitive(t *testing.T) {
	// Same logical state, different JSON key order. Canonicalization
	// sorts keys so all three hash identically.
	payloads := []json.RawMessage{
		cp(`{"query":"weather","city":"NYC"}`),
		cp(`{"city":"NYC","query":"weather"}`),
		cp(`{"query":"weather","city":"NYC"}`),
	}
	sig, detected := DetectSemanticLoop(payloads)
	if !detected {
		t.Fatalf("expected detection across key-order variants, got none. sig=%q", sig)
	}
}

func Test_SemanticLoop_CaseAndWhitespaceInsensitive(t *testing.T) {
	// Same conceptual state. String values differ in case + spacing
	// but canonicalize identically.
	payloads := []json.RawMessage{
		cp(`{"query":"Weather in NYC"}`),
		cp(`{"query":"  WEATHER IN NYC  "}`),
		cp(`{"query":"weather in nyc"}`),
	}
	sig, detected := DetectSemanticLoop(payloads)
	if !detected {
		t.Fatalf("expected detection across casing/whitespace variants, got none. sig=%q", sig)
	}
}

func Test_SemanticLoop_DistinctStatesDoNotFire(t *testing.T) {
	// Three checkpoints, three genuinely different states. No hash
	// repeats 3+ times so detector should NOT fire.
	payloads := []json.RawMessage{
		cp(`{"query":"weather"}`),
		cp(`{"query":"news"}`),
		cp(`{"query":"sports"}`),
	}
	_, detected := DetectSemanticLoop(payloads)
	if detected {
		t.Errorf("did not expect detection across distinct queries")
	}
}

func Test_SemanticLoop_MixedSequence(t *testing.T) {
	// Realistic mix: 3 occurrences of one query plus 2 of another.
	// Only the 3+ one should drive detection.
	payloads := []json.RawMessage{
		cp(`{"q":"loop me"}`),
		cp(`{"q":"distinct1"}`),
		cp(`{"q":"loop me"}`),
		cp(`{"q":"distinct2"}`),
		cp(`{"q":"loop me"}`),
	}
	sig, detected := DetectSemanticLoop(payloads)
	if !detected {
		t.Fatalf("expected detection on 3-occurrence subset, got none")
	}
	if !strings.HasPrefix(sig, "semantic_loop:") {
		t.Errorf("signature shape: %q", sig)
	}
}

func Test_SemanticLoop_DeterministicSignature(t *testing.T) {
	// Same input must produce the same signature across calls. Guards
	// against accidental map-iteration-order leaks into the signature.
	payloads := []json.RawMessage{
		cp(`{"x":1}`),
		cp(`{"x":1}`),
		cp(`{"x":1}`),
	}
	first, _ := DetectSemanticLoop(payloads)
	for i := 0; i < 50; i++ {
		got, _ := DetectSemanticLoop(payloads)
		if got != first {
			t.Fatalf("signature drifted on iteration %d: %q vs %q", i, got, first)
		}
	}
}

func Test_SemanticLoop_TieBreakStable(t *testing.T) {
	// Two distinct loops both at threshold. The detector must
	// deterministically pick the same winner across runs (lexico-
	// graphic on hash; we just want stability, not a specific value).
	payloads := []json.RawMessage{
		cp(`{"q":"alpha"}`),
		cp(`{"q":"alpha"}`),
		cp(`{"q":"alpha"}`),
		cp(`{"q":"beta"}`),
		cp(`{"q":"beta"}`),
		cp(`{"q":"beta"}`),
	}
	first, ok := DetectSemanticLoop(payloads)
	if !ok {
		t.Fatalf("expected detection on tied case")
	}
	for i := 0; i < 50; i++ {
		got, _ := DetectSemanticLoop(payloads)
		if got != first {
			t.Fatalf("tie-break drifted on iteration %d: %q vs %q", i, got, first)
		}
	}
}

func Test_SemanticLoop_EmptyInputs(t *testing.T) {
	cases := []struct {
		name     string
		payloads []json.RawMessage
	}{
		{"nil", nil},
		{"empty", []json.RawMessage{}},
		{"single", []json.RawMessage{cp(`{"x":1}`)}},
		{"missing state field", []json.RawMessage{
			json.RawMessage(`{}`),
			json.RawMessage(`{}`),
			json.RawMessage(`{}`),
		}},
		{"malformed JSON", []json.RawMessage{
			json.RawMessage(`{bad`),
			json.RawMessage(`{bad`),
			json.RawMessage(`{bad`),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, detected := DetectSemanticLoop(tc.payloads); detected {
				t.Errorf("expected no detection on %s input", tc.name)
			}
		})
	}
}

func Test_SemanticLoop_NumericRounding(t *testing.T) {
	// Trivial floating-point noise should not break clustering.
	payloads := []json.RawMessage{
		cp(`{"score":1.50}`),
		cp(`{"score":1.5}`),
		cp(`{"score":1.500001}`),
	}
	sig, detected := DetectSemanticLoop(payloads)
	if !detected {
		t.Fatalf("expected detection across float noise, got none. sig=%q", sig)
	}
}
