package api

import (
	"testing"

	"mesedi/backend/internal/detectors"
	"mesedi/backend/internal/events"
)

// The bug, and the thing that must not be over-corrected.
//
// A DLP sibling reuses its parent's sequence number so the dashboard can
// render the pair together. record_integrity reads the SET of sequence
// values, so that shared number looked like the customer claiming one
// sequence twice, and the signal fired on EVERY execution with a
// critical or high-severity DLP hit. The customer was told their records
// were corrupt, by us, about an event we wrote, at the moment they had a
// real leak to deal with.
//
// The over-correction is just as bad and is tested alongside: if the fix
// suppresses duplicates generally rather than only Mesedi's own events,
// the detector stops catching the thing it exists for and does so
// silently.

func evt(id string, t events.EventType, seq int) *events.Event {
	return &events.Event{EventID: id, ExecutionID: "exec-1", EventType: t, Sequence: seq}
}

func TestDLPSiblingDoesNotAccuseTheCustomerOfADuplicate(t *testing.T) {
	// What ingest actually persists for a two-event execution whose
	// first event tripped DLP: parent, Mesedi's sibling at the same
	// sequence, then the next real event.
	stream := []*events.Event{
		evt("e1", events.EventTypeLLMCall, 1),
		evt("dlp-1", events.EventTypeDLPScanResult, 1), // Mesedi's, shares seq 1
		evt("e2", events.EventTypeToolCall, 2),
	}

	seqs := customerSequences(stream)
	if got, want := len(seqs), 2; got != want {
		t.Fatalf("customerSequences returned %d values (%v), want %d", got, seqs, want)
	}
	if sigs := detectors.DetectRecordIntegrityAllMatches(seqs); len(sigs) != 0 {
		t.Errorf("a clean two-event execution was reported as %v because Mesedi's "+
			"own DLP sibling shares its parent's sequence. The customer gets a "+
			"corruption signal for every DLP hit", sigs)
	}

	// And the proof this test is not passing for a trivial reason: the
	// unfiltered stream DOES fire, which is exactly what production did.
	raw := make([]int, 0, len(stream))
	for _, e := range stream {
		raw = append(raw, e.Sequence)
	}
	if sigs := detectors.DetectRecordIntegrityAllMatches(raw); len(sigs) == 0 {
		t.Error("the unfiltered stream no longer fires, so this test would pass " +
			"even with the filter removed and is no longer guarding anything")
	}
}

// The over-correction. A customer who really did send the same sequence
// twice must still be told.
func TestARealCustomerDuplicateIsStillReported(t *testing.T) {
	stream := []*events.Event{
		evt("e1", events.EventTypeLLMCall, 1),
		evt("e2", events.EventTypeToolCall, 2),
		evt("e3", events.EventTypeToolCall, 2), // the customer's own duplicate
	}
	sigs := detectors.DetectRecordIntegrityAllMatches(customerSequences(stream))
	if len(sigs) == 0 {
		t.Fatal("a genuine duplicate in the customer's own events was not reported; " +
			"the fix suppressed the detector rather than correcting its input")
	}
	if sigs[0] != detectors.RecordIntegrityDuplicateSequence &&
		(len(sigs) < 2 || sigs[1] != detectors.RecordIntegrityDuplicateSequence) {
		t.Errorf("expected a duplicate_sequence signal, got %v", sigs)
	}
}

// A real gap must survive the filter too. Removing backend events must
// not close a hole in the customer's numbering.
func TestARealGapIsStillReported(t *testing.T) {
	stream := []*events.Event{
		evt("e1", events.EventTypeLLMCall, 1),
		evt("dlp-1", events.EventTypeDLPScanResult, 1),
		evt("e2", events.EventTypeToolCall, 4), // 2 and 3 missing
	}
	sigs := detectors.DetectRecordIntegrityAllMatches(customerSequences(stream))
	if len(sigs) != 1 || sigs[0] != detectors.RecordIntegritySequenceGap {
		t.Fatalf("expected exactly a sequence_gap, got %v. The DLP sibling must not "+
			"mask a hole in the customer's numbering, nor add a duplicate to it", sigs)
	}
	if missing := detectors.MissingSequences(customerSequences(stream)); len(missing) != 2 {
		t.Errorf("missing sequences = %v, want 2 and 3", missing)
	}
}

// An execution that is nothing but Mesedi's own events has no customer
// claim to check. It must not be reported on either way.
func TestAnExecutionOfOnlyBackendEventsYieldsNoCustomerClaim(t *testing.T) {
	stream := []*events.Event{
		evt("dlp-1", events.EventTypeDLPScanResult, 1),
		evt("dlp-2", events.EventTypeDLPScanResult, 1),
	}
	if seqs := customerSequences(stream); len(seqs) != 0 {
		t.Errorf("customerSequences = %v, want empty", seqs)
	}
}

func TestCustomerSequencesSkipsNilsWithoutLosingTheRest(t *testing.T) {
	stream := []*events.Event{
		evt("e1", events.EventTypeLLMCall, 1), nil,
		evt("e2", events.EventTypeToolCall, 2),
	}
	if got := customerSequences(stream); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("customerSequences = %v, want [1 2]", got)
	}
}
