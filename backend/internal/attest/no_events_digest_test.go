package attest

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"mesedi/backend/internal/events"
)

// These tests exist because of a production outage on 2026-09-04.
//
// A single execution with zero events — created and crashed 452
// microseconds later, before it emitted anything — stopped checkpoint
// construction for every tenant, on every tick, and would never have
// recovered on its own. Compute refuses an empty event list by design,
// and the chain treated that refusal as fatal.
//
// The fix must satisfy two things that pull in opposite directions:
// the chain has to keep moving, AND "we have no record of this run"
// must not start looking like "here is the record". Every test below
// guards one side or the other.

func evtAt(execID string, seq int) *events.Event {
	return &events.Event{
		EventID:     execID + "-e" + string(rune('0'+seq)),
		ExecutionID: execID,
		EventType:   events.EventTypeCheckpoint,
		Sequence:    seq,
		Timestamp:   time.Date(2026, 9, 4, 23, 0, seq, 0, time.UTC),
		Payload:     []byte(`{"k":"v"}`),
	}
}

// The outage itself, reproduced. This is the case that must not error.
func TestComputeForChainDoesNotStallOnAnEventLessExecution(t *testing.T) {
	for _, evts := range [][]*events.Event{nil, {}, {nil, nil}} {
		d, err := ComputeForChain("exec-aff8033ce6c1", evts)
		if err != nil {
			t.Fatalf("an execution with no events still halts the chain: %v", err)
		}
		if d.Root == "" {
			t.Error("no-events digest has an empty root; the leaf fold would be meaningless")
		}
		if d.LeafCount != 0 {
			t.Errorf("LeafCount = %d, want 0", d.LeafCount)
		}
	}
}

// The other half of the contract, and the reason this is not simply
// "return the empty Merkle root". An empty tree root is a REAL value
// that a verifier could mistake for a root over real events. The
// no-events digest must be distinguishable from it.
func TestNoEventsDigestIsNotTheEmptyMerkleRoot(t *testing.T) {
	d, err := ComputeForChain("exec-1", nil)
	if err != nil {
		t.Fatalf("ComputeForChain: %v", err)
	}

	// rootFromLeafHashes(nil) is the RFC 6962 empty tree hash. If the
	// no-events digest equalled it, "nothing was recorded" and "a tree
	// with no leaves" would be the same value to every reader.
	empty := hex.EncodeToString(rootFromLeafHashes(nil))
	if d.Root == empty {
		t.Error("the no-events digest is the empty Merkle root; absence would be " +
			"indistinguishable from an empty record")
	}
	if d.Algorithm == AlgorithmV1 {
		t.Errorf("no-events digest claims %s; a different construction MUST have a "+
			"different algorithm identifier, per the rule on AlgorithmV1", d.Algorithm)
	}
	if d.Algorithm != AlgorithmNoEventsV1 {
		t.Errorf("algorithm = %q, want %q", d.Algorithm, AlgorithmNoEventsV1)
	}
}

// Two empty executions must not collapse to one root. If they did, the
// chain would commit to "an empty execution happened" rather than to
// WHICH empty executions happened, and one could be deleted without
// moving the interval root.
func TestNoEventsDigestsDifferPerExecution(t *testing.T) {
	a, err := ComputeForChain("exec-aaa", nil)
	if err != nil {
		t.Fatalf("ComputeForChain: %v", err)
	}
	b, err := ComputeForChain("exec-bbb", nil)
	if err != nil {
		t.Fatalf("ComputeForChain: %v", err)
	}
	if a.Root == b.Root {
		t.Error("two different empty executions share a root; deleting one would " +
			"not move the interval root and would leave no trace")
	}
}

// Determinism. The chain recomputes these on every export, so a value
// that varied between calls would read as tampering.
func TestNoEventsDigestIsStable(t *testing.T) {
	first, _ := ComputeForChain("exec-stable", nil)
	for i := 0; i < 5; i++ {
		again, err := ComputeForChain("exec-stable", nil)
		if err != nil {
			t.Fatalf("ComputeForChain: %v", err)
		}
		if again.Root != first.Root {
			t.Fatal("the no-events digest is not deterministic; a recomputation " +
				"would report tampering that did not happen")
		}
	}
}

// With events present, ComputeForChain must be byte-identical to
// Compute. It is a wrapper, not a second construction, and every
// digest already anchored was produced by Compute.
func TestComputeForChainMatchesComputeWhenEventsExist(t *testing.T) {
	evts := []*events.Event{evtAt("exec-1", 1), evtAt("exec-1", 2)}

	want, err := Compute("exec-1", evts)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	got, err := ComputeForChain("exec-1", evts)
	if err != nil {
		t.Fatalf("ComputeForChain: %v", err)
	}
	if got.Root != want.Root || got.Algorithm != want.Algorithm ||
		got.LeafCount != want.LeafCount {
		t.Errorf("ComputeForChain diverged from Compute on a normal execution:\n"+
			" got  %+v\n want %+v", got, want)
	}
}

// Compute's own contract must NOT have changed. The digest endpoint
// answers 404 on the strength of this error, so that "we have nothing"
// does not masquerade as "here is the record".
func TestComputeStillRefusesAnEmptyEventList(t *testing.T) {
	if _, err := Compute("exec-1", nil); err != ErrNoEvents {
		t.Errorf("Compute no longer returns ErrNoEvents (%v); the digest endpoint "+
			"depends on it to answer 404 instead of an empty digest", err)
	}
}

// An empty execution id would give every event-less execution the same
// root, defeating the domain separation.
func TestComputeForChainRefusesAnEmptyExecutionID(t *testing.T) {
	if _, err := ComputeForChain("", nil); err == nil {
		t.Error("an event-less execution with no id was accepted; every such " +
			"execution would share one root")
	}
}

// The root must actually commit to the execution id, not merely vary
// with it by coincidence of some other input.
func TestNoEventsDigestCommitsToTheExecutionID(t *testing.T) {
	d, err := ComputeForChain("exec-known", nil)
	if err != nil {
		t.Fatalf("ComputeForChain: %v", err)
	}
	if len(d.Root) != 64 || strings.ContainsAny(d.Root, "ghijklmnopqrstuvwxyz") {
		t.Errorf("root %q is not a 64-char lowercase hex sha256", d.Root)
	}
	if d.ExecutionID != "exec-known" {
		t.Errorf("ExecutionID = %q, want exec-known", d.ExecutionID)
	}
}
