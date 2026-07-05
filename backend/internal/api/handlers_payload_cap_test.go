package api

import (
	"encoding/json"
	"testing"

	"mesedi/backend/internal/events"
)

// TestPayloadOverCap_UnderCapAccepted covers the common case: a
// normal-sized event payload is well under MaxEventPayloadBytes and
// the helper returns false (event is fine, do not reject).
func TestPayloadOverCap_UnderCapAccepted(t *testing.T) {
	smallPayload, err := json.Marshal(map[string]string{
		"prompt":   "summarize this short doc",
		"response": "the doc is about cats",
	})
	if err != nil {
		t.Fatalf("marshal small payload: %v", err)
	}
	evt := &events.Event{Payload: smallPayload}
	if payloadOverCap(evt) {
		t.Fatalf("small payload (%d bytes) flagged as over cap of %d",
			len(smallPayload), MaxEventPayloadBytes)
	}
}

// TestPayloadOverCap_AtCapBoundaryAccepted documents the inclusive
// boundary: a payload exactly equal to MaxEventPayloadBytes passes.
// The reject branch fires only when len(payload) > cap, NOT >=.
func TestPayloadOverCap_AtCapBoundaryAccepted(t *testing.T) {
	atCap := make(json.RawMessage, MaxEventPayloadBytes)
	for i := range atCap {
		atCap[i] = 'x'
	}
	evt := &events.Event{Payload: atCap}
	if payloadOverCap(evt) {
		t.Fatalf("payload at exactly the cap (%d bytes) must NOT be rejected", MaxEventPayloadBytes)
	}
}

// TestPayloadOverCap_OverCapRejected covers the actual rejection
// path: anything strictly larger than MaxEventPayloadBytes trips the
// helper. This is the case the SDK-side truncation should always
// pre-empt; the backend enforces as defense in depth for old SDKs,
// curl-direct integrations, and any bypass.
func TestPayloadOverCap_OverCapRejected(t *testing.T) {
	overCap := make(json.RawMessage, MaxEventPayloadBytes+1)
	for i := range overCap {
		overCap[i] = 'x'
	}
	evt := &events.Event{Payload: overCap}
	if !payloadOverCap(evt) {
		t.Fatalf("payload one byte over the cap (%d bytes) must be rejected", len(overCap))
	}
}

// TestPayloadOverCap_FarOverCapRejected covers a realistic "the SDK
// truncation was skipped and the customer sent a multi-MB blob" case.
// Should reject just like the one-byte-over case.
func TestPayloadOverCap_FarOverCapRejected(t *testing.T) {
	farOver := make(json.RawMessage, MaxEventPayloadBytes*100)
	for i := range farOver {
		farOver[i] = 'x'
	}
	evt := &events.Event{Payload: farOver}
	if !payloadOverCap(evt) {
		t.Fatalf("payload far over the cap (%d bytes) must be rejected", len(farOver))
	}
}

// TestPayloadOverCap_EmptyPayloadAccepted documents that empty
// payloads are fine. Some event types (heartbeats, no-op markers)
// legitimately have no payload; the helper must not false-positive
// them.
func TestPayloadOverCap_EmptyPayloadAccepted(t *testing.T) {
	evt := &events.Event{Payload: json.RawMessage{}}
	if payloadOverCap(evt) {
		t.Fatalf("empty payload must not be flagged as over cap")
	}
}
