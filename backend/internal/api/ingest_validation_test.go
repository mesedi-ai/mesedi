package api

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"mesedi/backend/internal/events"
)

// Before this file, the ingest first pass had no test of any kind — no
// Go test constructs HandleIngestEvents — so four rules deciding whether
// a customer's events are kept or silently dropped were unverified.
//
// The reserved-type rule is the one this change added, and it carries
// more weight than it looks: the record_integrity filter excludes
// backend-minted events from the customer's sequence set, which is only
// safe because a customer cannot submit one. If this rule regresses, the
// filter becomes a way for a customer to hide a genuine duplicate from
// their own integrity check and receive a Mesedi report calling the
// record clean.

func capturingLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func okEvent(id string, t events.EventType) events.Event {
	return events.Event{
		EventID: id, ExecutionID: "exec-1", EventType: t, Sequence: 1,
		Timestamp: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
	}
}

func TestIngestRejectsBackendMintedTypesFromCustomers(t *testing.T) {
	logger, logs := capturingLogger()
	accepted, rejected := validateIngestBatch([]events.Event{
		okEvent("e1", events.EventTypeLLMCall),
		okEvent("forged", events.EventTypeDLPScanResult),
		okEvent("e2", events.EventTypeToolCall),
	}, logger)

	if rejected != 1 {
		t.Fatalf("rejected = %d, want 1: a customer submitted a dlp_scan_result, "+
			"which Mesedi writes itself. Accepting it lets a customer exclude their "+
			"own events from the integrity check that is supposed to catch them",
			rejected)
	}
	if len(accepted) != 2 {
		t.Fatalf("accepted %d events, want 2", len(accepted))
	}
	for _, e := range accepted {
		if e.EventID == "forged" {
			t.Error("the forged dlp_scan_result was stored")
		}
	}
	if !strings.Contains(logs.String(), "reserved event type") {
		t.Error("the rejection was not logged; an event vanishing from a customer's " +
			"record with no trace is the worst outcome this path has")
	}
}

// The rest of the first pass, which was equally untested.
func TestIngestRejectsEventsThatCannotBeStored(t *testing.T) {
	logger, _ := capturingLogger()

	big := okEvent("big", events.EventTypeLLMCall)
	big.Payload = make([]byte, MaxEventPayloadBytes+1)

	noID := okEvent("", events.EventTypeLLMCall)
	noExec := okEvent("e-noexec", events.EventTypeLLMCall)
	noExec.ExecutionID = ""
	noType := okEvent("e-notype", events.EventTypeLLMCall)
	noType.EventType = ""

	accepted, rejected := validateIngestBatch(
		[]events.Event{noID, noExec, noType, big, okEvent("good", events.EventTypeToolCall)},
		logger)

	if rejected != 4 {
		t.Errorf("rejected = %d, want 4", rejected)
	}
	if len(accepted) != 1 || accepted[0].EventID != "good" {
		t.Errorf("accepted = %+v, want only the well-formed event", accepted)
	}
}

// One bad event must not cost the customer the rest of the batch. This
// is the property the per-event loop exists for and it was asserted
// only in a comment.
func TestOneBadEventDoesNotPoisonTheBatch(t *testing.T) {
	logger, _ := capturingLogger()
	batch := make([]events.Event, 0, 10)
	for i := 0; i < 9; i++ {
		batch = append(batch, okEvent(string(rune('a'+i)), events.EventTypeLLMCall))
	}
	batch = append(batch, okEvent("", events.EventTypeLLMCall)) // malformed, last

	accepted, rejected := validateIngestBatch(batch, logger)
	if len(accepted) != 9 || rejected != 1 {
		t.Errorf("accepted %d and rejected %d; one malformed event took others with "+
			"it", len(accepted), rejected)
	}
}

// A missing timestamp is defaulted rather than rejected, and a supplied
// one is never overwritten — the customer's clock is their claim.
func TestIngestDefaultsOnlyAMissingTimestamp(t *testing.T) {
	logger, _ := capturingLogger()
	supplied := okEvent("has-ts", events.EventTypeLLMCall)
	missing := okEvent("no-ts", events.EventTypeLLMCall)
	missing.Timestamp = time.Time{}

	accepted, rejected := validateIngestBatch([]events.Event{supplied, missing}, logger)
	if rejected != 0 || len(accepted) != 2 {
		t.Fatalf("accepted %d, rejected %d, want 2 and 0", len(accepted), rejected)
	}
	if !accepted[0].Timestamp.Equal(supplied.Timestamp) {
		t.Errorf("a supplied timestamp was overwritten: %v", accepted[0].Timestamp)
	}
	if accepted[1].Timestamp.IsZero() {
		t.Error("a missing timestamp was not defaulted, so the event stores with a " +
			"zero time")
	}
}

func TestValidateIngestBatchHandlesAnEmptyBatch(t *testing.T) {
	logger, _ := capturingLogger()
	accepted, rejected := validateIngestBatch(nil, logger)
	if len(accepted) != 0 || rejected != 0 {
		t.Errorf("empty batch produced %d accepted / %d rejected", len(accepted), rejected)
	}
}
