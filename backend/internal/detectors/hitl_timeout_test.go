// Unit tests for HITL-timeout.
//
// Two firing modes:
//   - "explicit" (response_kind == "timeout")
//   - "sla_exceeded" (wait_duration_ms > sla_seconds * 1000)
//
// Legacy DetectHITLTimeout: first-match-wins, explicit > sla_exceeded.
// All-matches variant DetectHITLTimeoutAllMatches: returns BOTH
// signatures when both conditions present.
// Per-project knob FireModes restricts which modes fire (closed set
// {"explicit","sla_exceeded"}; invalid input falls back to default).
package detectors

import (
	"encoding/json"
	"reflect"
	"testing"
)

func raw(payload map[string]any) json.RawMessage {
	b, _ := json.Marshal(payload)
	return b
}

// ─────────────────────────────────────────────────────────────────────
// DetectHITLTimeout (legacy first-match-wins)
// ─────────────────────────────────────────────────────────────────────

func Test_DetectHITLTimeout_NoPayloads(t *testing.T) {
	sig, detected := DetectHITLTimeout(nil)
	if detected {
		t.Errorf("nil payloads should not fire, got sig=%q", sig)
	}
	sig, detected = DetectHITLTimeout([]json.RawMessage{})
	if detected {
		t.Errorf("empty payloads should not fire, got sig=%q", sig)
	}
}

func Test_DetectHITLTimeout_ExplicitTimeout(t *testing.T) {
	payloads := []json.RawMessage{
		raw(map[string]any{"response_kind": "approved"}),
		raw(map[string]any{"response_kind": "timeout"}),
	}
	sig, detected := DetectHITLTimeout(payloads)
	if !detected {
		t.Fatal("expected explicit timeout to fire")
	}
	if sig != "hitl_timeout:explicit" {
		t.Errorf("expected 'hitl_timeout:explicit', got %q", sig)
	}
}

func Test_DetectHITLTimeout_SLAExceeded(t *testing.T) {
	payloads := []json.RawMessage{
		raw(map[string]any{
			"response_kind":    "approved",
			"sla_seconds":      1,
			"wait_duration_ms": 2000, // 2s > 1s SLA
		}),
	}
	sig, detected := DetectHITLTimeout(payloads)
	if !detected {
		t.Fatal("expected sla_exceeded to fire")
	}
	if sig != "hitl_timeout:sla_exceeded" {
		t.Errorf("expected 'hitl_timeout:sla_exceeded', got %q", sig)
	}
}

func Test_DetectHITLTimeout_ExplicitWinsOverSLA(t *testing.T) {
	// Explicit timeout must beat SLA exceeded even when both present.
	payloads := []json.RawMessage{
		raw(map[string]any{
			"response_kind":    "approved",
			"sla_seconds":      1,
			"wait_duration_ms": 5000, // SLA exceeded
		}),
		raw(map[string]any{"response_kind": "timeout"}),
	}
	sig, detected := DetectHITLTimeout(payloads)
	if !detected {
		t.Fatal("expected fire")
	}
	if sig != "hitl_timeout:explicit" {
		t.Errorf("explicit priority: expected 'hitl_timeout:explicit', got %q", sig)
	}
}

func Test_DetectHITLTimeout_NoSLAdeclared_NoFire(t *testing.T) {
	// Without sla_seconds we cannot detect a breach, must not fire.
	payloads := []json.RawMessage{
		raw(map[string]any{
			"response_kind":    "approved",
			"wait_duration_ms": 60_000_000, // 60s, would breach any reasonable SLA
		}),
	}
	sig, detected := DetectHITLTimeout(payloads)
	if detected {
		t.Errorf("no SLA declared should not fire, got sig=%q", sig)
	}
}

func Test_DetectHITLTimeout_ZeroSLA_NoFire(t *testing.T) {
	// sla_seconds == 0 means "no SLA" and should not fire.
	payloads := []json.RawMessage{
		raw(map[string]any{
			"response_kind":    "approved",
			"sla_seconds":      0,
			"wait_duration_ms": 5000,
		}),
	}
	if _, detected := DetectHITLTimeout(payloads); detected {
		t.Error("sla_seconds=0 should not fire")
	}
}

func Test_DetectHITLTimeout_MalformedPayloadSkipped(t *testing.T) {
	// Bad JSON in the middle should be skipped, not panic.
	payloads := []json.RawMessage{
		json.RawMessage(`{not-valid-json`),
		raw(map[string]any{"response_kind": "timeout"}),
	}
	sig, detected := DetectHITLTimeout(payloads)
	if !detected {
		t.Fatal("malformed payload should be skipped; valid timeout should still fire")
	}
	if sig != "hitl_timeout:explicit" {
		t.Errorf("expected explicit signature, got %q", sig)
	}
}

// ─────────────────────────────────────────────────────────────────────
// DetectHITLTimeoutAllMatches (Wave hitl_timeout.G3, all matches)
// ─────────────────────────────────────────────────────────────────────

func Test_DetectHITLTimeoutAllMatches_BothFire(t *testing.T) {
	// Different payloads triggering different modes, all-matches
	// returns BOTH signatures (closes G3 vs legacy first-match-wins).
	payloads := []json.RawMessage{
		raw(map[string]any{"response_kind": "timeout"}),
		raw(map[string]any{
			"response_kind":    "approved",
			"sla_seconds":      1,
			"wait_duration_ms": 3000,
		}),
	}
	sigs := DetectHITLTimeoutAllMatches(payloads)
	want := []string{"hitl_timeout:explicit", "hitl_timeout:sla_exceeded"}
	if !reflect.DeepEqual(sigs, want) {
		t.Errorf("expected %v, got %v", want, sigs)
	}
}

func Test_DetectHITLTimeoutAllMatches_OrderingExplicitFirst(t *testing.T) {
	// Even if the SLA-exceeded payload comes first in the slice, the
	// ordering of the result must put explicit before sla_exceeded.
	payloads := []json.RawMessage{
		raw(map[string]any{
			"response_kind":    "approved",
			"sla_seconds":      1,
			"wait_duration_ms": 3000,
		}),
		raw(map[string]any{"response_kind": "timeout"}),
	}
	sigs := DetectHITLTimeoutAllMatches(payloads)
	if len(sigs) != 2 || sigs[0] != "hitl_timeout:explicit" || sigs[1] != "hitl_timeout:sla_exceeded" {
		t.Errorf("deterministic ordering broken; got %v", sigs)
	}
}

// ─────────────────────────────────────────────────────────────────────
// HITLTimeoutThresholds, FireModes per-project knob
// ─────────────────────────────────────────────────────────────────────

func Test_HITLTimeoutThresholds_EffectiveFireModes(t *testing.T) {
	defaults := []string{"explicit", "sla_exceeded"}
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty_reverts", []string{}, defaults},
		{"nil_reverts", nil, defaults},
		{"explicit_only", []string{"explicit"}, []string{"explicit"}},
		{"sla_only", []string{"sla_exceeded"}, []string{"sla_exceeded"}},
		{"both_valid", []string{"explicit", "sla_exceeded"}, []string{"explicit", "sla_exceeded"}},
		{"unknown_mode_reverts_whole", []string{"explicit", "BOGUS"}, defaults},
		{"typo_reverts", []string{"explicit_timeout"}, defaults},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			thresh := HITLTimeoutThresholds{FireModes: tc.in}
			got := thresh.EffectiveFireModes()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("EffectiveFireModes(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func Test_DetectHITLTimeoutAllMatchesWithThresholds_RestrictsToExplicit(t *testing.T) {
	// Customer who tunes FireModes to ["explicit"] only, the
	// sla_exceeded payload must be suppressed at scoring time.
	payloads := []json.RawMessage{
		raw(map[string]any{"response_kind": "timeout"}),
		raw(map[string]any{
			"response_kind":    "approved",
			"sla_seconds":      1,
			"wait_duration_ms": 3000,
		}),
	}
	thresh := HITLTimeoutThresholds{FireModes: []string{"explicit"}}
	sigs := DetectHITLTimeoutAllMatchesWithThresholds(payloads, thresh)
	want := []string{"hitl_timeout:explicit"}
	if !reflect.DeepEqual(sigs, want) {
		t.Errorf("expected only explicit, got %v", sigs)
	}
}

func Test_DetectHITLTimeoutAllMatchesWithThresholds_RestrictsToSLA(t *testing.T) {
	payloads := []json.RawMessage{
		raw(map[string]any{"response_kind": "timeout"}),
		raw(map[string]any{
			"response_kind":    "approved",
			"sla_seconds":      1,
			"wait_duration_ms": 3000,
		}),
	}
	thresh := HITLTimeoutThresholds{FireModes: []string{"sla_exceeded"}}
	sigs := DetectHITLTimeoutAllMatchesWithThresholds(payloads, thresh)
	want := []string{"hitl_timeout:sla_exceeded"}
	if !reflect.DeepEqual(sigs, want) {
		t.Errorf("expected only sla_exceeded, got %v", sigs)
	}
}

func Test_DefaultHITLTimeoutThresholds_LocksDocumentedDefault(t *testing.T) {
	d := DefaultHITLTimeoutThresholds()
	want := []string{"explicit", "sla_exceeded"}
	if !reflect.DeepEqual(d.FireModes, want) {
		t.Errorf("DefaultHITLTimeoutThresholds.FireModes = %v, want %v", d.FireModes, want)
	}
}
