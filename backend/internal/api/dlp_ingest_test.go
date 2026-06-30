// Unit tests for applyDLPToBatch sibling-event emission. The key
// invariant under test: sibling dlp_scan_result events fire for ANY
// hit severity (critical, high, OR medium). Pre-Wave-3.B the
// ingest layer hardcoded `critical OR high` gating, which silently
// suppressed the medium tier even for customers who explicitly
// opted into it via data_leakage.severity_policy=["critical","high",
// "medium"]. The fix lets sibling events flow for all severities;
// promotion to failure_group is gated downstream at the handler via
// FindFirstDLPSignalForSeverities (which honors the per-project
// AllowedSeverities knob).
package api

import (
	"context"
	"encoding/json"
	"testing"

	"mesedi/backend/internal/dlp"
	"mesedi/backend/internal/events"
	"mesedi/backend/internal/store"
)

// stubDLPStore is the narrowest Store implementation that
// applyDLPToBatch reaches: only ListProjectPatterns is called (to
// load custom patterns). All other methods panic if invoked —
// applyDLPToBatch must not exercise any other store path.
type stubDLPStore struct {
	store.Store
}

func (s *stubDLPStore) ListProjectPatterns(
	_ context.Context, _ string, _ string, _ bool,
) ([]*store.ProjectPattern, error) {
	return nil, nil
}

// buildHandlersForDLPTest constructs a Handlers ready to call
// applyDLPToBatch. The scanner is built from a single synthetic
// medium-severity rule so the test doesn't depend on the
// production rule list (which currently has zero medium-severity
// rules; if a medium-severity rule ships later this test stays
// stable because it uses its own rule).
func buildHandlersForDLPTest(t *testing.T) *Handlers {
	t.Helper()
	scanner, err := dlp.NewScanner([]dlp.Rule{
		{
			ID:       "test_medium_secret",
			Label:    "Synthetic medium-severity rule for tests",
			Severity: dlp.SeverityMedium,
			Pattern:  `TESTMEDSECRET_[A-Z0-9]{8}`,
		},
	})
	if err != nil {
		t.Fatalf("dlp.NewScanner: %v", err)
	}
	return &Handlers{
		Logger:     quietLogger(),
		Store:      &stubDLPStore{},
		DLPScanner: scanner,
	}
}

// makeLLMCallEvent constructs a minimal llm_call event whose
// user_message field contains the supplied secret. applyDLPToBatch
// scans user_message per dlp_ingest.go scanFieldKeys.
func makeLLMCallEvent(secret string) events.Event {
	payload, _ := json.Marshal(map[string]any{
		"user_message": "please process " + secret + " for me",
	})
	return events.Event{
		EventID:     "evt_test_1",
		ExecutionID: "exec_test_1",
		EventType:   events.EventTypeLLMCall,
		Sequence:    1,
		Payload:     payload,
	}
}

// Test_applyDLPToBatch_MediumHit_EmitsSibling pins the Wave 3.B
// behavior: a medium-severity hit produces a dlp_scan_result sibling
// event. The previous behavior hardcoded `critical OR high` gating
// which silently dropped medium hits before the per-project
// AllowedSeverities knob could be consulted.
func Test_applyDLPToBatch_MediumHit_EmitsSibling(t *testing.T) {
	h := buildHandlersForDLPTest(t)
	batch := []events.Event{makeLLMCallEvent("TESTMEDSECRET_ABCD1234")}

	out, _ := h.applyDLPToBatch(context.Background(), "proj_test", batch)

	// Find the sibling dlp_scan_result event.
	var sibling *events.Event
	for i := range out {
		if out[i].EventType == events.EventTypeDLPScanResult {
			sibling = &out[i]
			break
		}
	}
	if sibling == nil {
		t.Fatalf("expected dlp_scan_result sibling event for medium hit; got %d events of types: %v",
			len(out), eventTypes(out))
	}

	var payload events.DLPScanResultPayload
	if err := json.Unmarshal(sibling.Payload, &payload); err != nil {
		t.Fatalf("decode sibling payload: %v", err)
	}
	if payload.HighestSeverity != string(dlp.SeverityMedium) {
		t.Errorf("sibling highest_severity = %q, want %q",
			payload.HighestSeverity, dlp.SeverityMedium)
	}
	if payload.HitCount == 0 {
		t.Errorf("sibling hit_count = 0, want > 0")
	}
}

// Test_applyDLPToBatch_NoHits_NoSibling pins the unchanged behavior:
// when there are zero hits, no sibling event is generated. Guards
// against accidental over-emission during the gating-removal change.
func Test_applyDLPToBatch_NoHits_NoSibling(t *testing.T) {
	h := buildHandlersForDLPTest(t)
	clean, _ := json.Marshal(map[string]any{
		"user_message": "no secrets here, just a normal message",
	})
	batch := []events.Event{{
		EventID:     "evt_test_2",
		ExecutionID: "exec_test_2",
		EventType:   events.EventTypeLLMCall,
		Sequence:    1,
		Payload:     clean,
	}}

	out, _ := h.applyDLPToBatch(context.Background(), "proj_test", batch)

	for _, e := range out {
		if e.EventType == events.EventTypeDLPScanResult {
			t.Errorf("expected NO sibling event for hit-free batch; got one with payload: %s",
				string(e.Payload))
		}
	}
}

func eventTypes(evts []events.Event) []string {
	out := make([]string, len(evts))
	for i, e := range evts {
		out[i] = string(e.EventType)
	}
	return out
}
