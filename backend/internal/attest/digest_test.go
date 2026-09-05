package attest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"mesedi/backend/internal/events"
)

func evt(id string, seq int, ts time.Time, payload string) *events.Event {
	return &events.Event{
		EventID:     id,
		ExecutionID: "exec-1",
		EventType:   events.EventTypeCheckpoint,
		Sequence:    seq,
		Timestamp:   ts,
		Payload:     json.RawMessage(payload),
	}
}

func sample() []*events.Event {
	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	return []*events.Event{
		evt("a", 1, base, `{"n":1}`),
		evt("b", 2, base.Add(time.Second), `{"n":2}`),
		evt("c", 3, base.Add(2*time.Second), `{"n":3}`),
	}
}

// The root must not depend on the order events came back from the
// datastore. Without this, a query-plan change would look like
// tampering to every customer at once.
func TestRootIsIndependentOfInputOrder(t *testing.T) {
	e := sample()
	forward, err := Compute("exec-1", e)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	reversed, err := Compute("exec-1", []*events.Event{e[2], e[0], e[1]})
	if err != nil {
		t.Fatalf("Compute reversed: %v", err)
	}
	if forward.Root != reversed.Root {
		t.Errorf("root changed with input order:\n forward  %s\n reversed %s",
			forward.Root, reversed.Root)
	}
}

// Same data, many calls, same root. Go randomises map iteration, and
// the leaf encoding hashes raw payload bytes precisely so no map is
// ever walked, this test is what would catch a future change that
// re-serialises the payload instead.
func TestRootIsStableAcrossRepeatedCalls(t *testing.T) {
	e := sample()
	first, err := Compute("exec-1", e)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	for i := 0; i < 100; i++ {
		got, err := Compute("exec-1", e)
		if err != nil {
			t.Fatalf("Compute run %d: %v", i, err)
		}
		if got.Root != first.Root {
			t.Fatalf("run %d root %s, first %s", i, got.Root, first.Root)
		}
	}
}

// A timestamp expressed in another zone is the same instant and must
// produce the same leaf. A verifier running in a different timezone
// has to reach identical bytes.
func TestTimestampZoneDoesNotChangeTheRoot(t *testing.T) {
	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	utc := []*events.Event{evt("a", 1, base, `{}`)}

	east := time.FixedZone("UTC+5", 5*3600)
	shifted := []*events.Event{evt("a", 1, base.In(east), `{}`)}

	a, err := Compute("exec-1", utc)
	if err != nil {
		t.Fatalf("Compute utc: %v", err)
	}
	b, err := Compute("exec-1", shifted)
	if err != nil {
		t.Fatalf("Compute shifted: %v", err)
	}
	if a.Root != b.Root {
		t.Errorf("same instant in a different zone changed the root:\n %s\n %s",
			a.Root, b.Root)
	}
}

// Every field in the canonical leaf must actually move the root.
// A field that is encoded but does not affect the hash is a field an
// attacker can rewrite for free, and it would look exactly like a
// working implementation.
func TestEveryCanonicalFieldChangesTheRoot(t *testing.T) {
	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	original := evt("a", 1, base, `{"n":1}`)
	originalDigest, err := Compute("exec-1", []*events.Event{original})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}

	mutations := map[string]func(e *events.Event){
		"event_id":     func(e *events.Event) { e.EventID = "different" },
		"execution_id": func(e *events.Event) { e.ExecutionID = "exec-other" },
		"event_type":   func(e *events.Event) { e.EventType = events.EventTypeToolCall },
		"sequence":     func(e *events.Event) { e.Sequence = 99 },
		"timestamp":    func(e *events.Event) { e.Timestamp = base.Add(time.Nanosecond) },
		"duration_ms":  func(e *events.Event) { e.DurationMs = 5 },
		"payload":      func(e *events.Event) { e.Payload = json.RawMessage(`{"n":2}`) },
	}

	for field, mutate := range mutations {
		t.Run(field, func(t *testing.T) {
			mutated := evt("a", 1, base, `{"n":1}`)
			mutate(mutated)
			got, err := Compute("exec-1", []*events.Event{mutated})
			if err != nil {
				t.Fatalf("Compute: %v", err)
			}
			if got.Root == originalDigest.Root {
				t.Errorf("changing %s did not change the root, that field "+
					"is not actually covered and can be rewritten freely",
					field)
			}
		})
	}
}

// Length-prefixing exists to stop two different field splits producing
// identical bytes. Without it, moving a character from the end of one
// field to the start of the next would be invisible.
func TestFieldBoundariesAreUnambiguous(t *testing.T) {
	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	a := evt("ab", 1, base, `{}`)
	a.ExecutionID = "c"
	b := evt("a", 1, base, `{}`)
	b.ExecutionID = "bc"

	da, err := Compute("x", []*events.Event{a})
	if err != nil {
		t.Fatalf("Compute a: %v", err)
	}
	db, err := Compute("x", []*events.Event{b})
	if err != nil {
		t.Fatalf("Compute b: %v", err)
	}
	if da.Root == db.Root {
		t.Error(`("ab","c") and ("a","bc") produced the same root, ` +
			`field lengths are not being encoded`)
	}
}

// Duplicate sequence numbers are a real condition this system detects
// rather than rejects. Sorting on sequence alone would leave two such
// events in nondeterministic order and flip the root between calls.
func TestDuplicateSequencesStillProduceAStableRoot(t *testing.T) {
	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	e := []*events.Event{
		evt("zzz", 2, base, `{"n":1}`),
		evt("aaa", 2, base, `{"n":2}`),
		evt("mmm", 1, base, `{"n":3}`),
	}
	first, err := Compute("exec-1", e)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	shuffled := []*events.Event{e[1], e[2], e[0]}
	got, err := Compute("exec-1", shuffled)
	if err != nil {
		t.Fatalf("Compute shuffled: %v", err)
	}
	if got.Root != first.Root {
		t.Errorf("duplicate sequences made the root order-dependent:\n %s\n %s",
			first.Root, got.Root)
	}
}

// An execution with no events must not silently digest to the empty
// tree. "No record of this run" and "here is the record" must not
// return the same value.
func TestNoEventsIsAnErrorNotAnEmptyRoot(t *testing.T) {
	if _, err := Compute("exec-1", nil); err != ErrNoEvents {
		t.Errorf("expected ErrNoEvents, got %v", err)
	}
	if _, err := Compute("exec-1", []*events.Event{nil, nil}); err != ErrNoEvents {
		t.Errorf("nil-only slice: expected ErrNoEvents, got %v", err)
	}
}

// Round-trip across every tree size that exercises the odd-node rule.
// Odd sizes are where Merkle implementations diverge, and a proof that
// only works for powers of two passes a naive test suite.
func TestProofRoundTripAcrossTreeSizes(t *testing.T) {
	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	for size := 1; size <= 17; size++ {
		t.Run(fmt.Sprintf("size-%d", size), func(t *testing.T) {
			var e []*events.Event
			for i := 0; i < size; i++ {
				e = append(e, evt(fmt.Sprintf("evt-%03d", i), i+1,
					base.Add(time.Duration(i)*time.Second),
					fmt.Sprintf(`{"i":%d}`, i)))
			}
			d, err := Compute("exec-1", e)
			if err != nil {
				t.Fatalf("Compute: %v", err)
			}
			for i := 0; i < size; i++ {
				p, err := Prove(d, i)
				if err != nil {
					t.Fatalf("Prove(%d): %v", i, err)
				}
				ok, err := VerifyInclusion(p)
				if err != nil {
					t.Fatalf("VerifyInclusion(%d): %v", i, err)
				}
				if !ok {
					t.Errorf("leaf %d of %d did not verify against the root",
						i, size)
				}
			}
		})
	}
}

// A proof must fail when the leaf is not the one that was proved.
// Without this the round-trip test above could pass against a verifier
// that returns true unconditionally.
func TestProofFailsForAWrongLeaf(t *testing.T) {
	d, err := Compute("exec-1", sample())
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p, err := Prove(d, 0)
	if err != nil {
		t.Fatalf("Prove: %v", err)
	}
	p.LeafHash = d.Leaves[1] // claim a different leaf, keep leaf 0's path
	ok, err := VerifyInclusion(p)
	if err != nil {
		t.Fatalf("VerifyInclusion: %v", err)
	}
	if ok {
		t.Error("a proof verified for a leaf it was not built for, " +
			"VerifyInclusion is not actually checking anything")
	}
}

func TestProveRejectsAnOutOfRangeIndex(t *testing.T) {
	d, err := Compute("exec-1", sample())
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if _, err := Prove(d, -1); err == nil {
		t.Error("negative index must be refused")
	}
	if _, err := Prove(d, len(d.Leaves)); err == nil {
		t.Error("index past the end must be refused")
	}
}

// The algorithm identifier is part of the published contract. If the
// construction changes without changing this string, a verifier
// assuming v1 rules computes a different root and reports tampering
// that never happened.
func TestAlgorithmIdentifierIsPublished(t *testing.T) {
	d, err := Compute("exec-1", sample())
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if d.Algorithm != AlgorithmV1 {
		t.Errorf("digest algorithm = %q, want %q", d.Algorithm, AlgorithmV1)
	}
	p, err := Prove(d, 0)
	if err != nil {
		t.Fatalf("Prove: %v", err)
	}
	if p.Algorithm != AlgorithmV1 {
		t.Errorf("proof algorithm = %q, want %q", p.Algorithm, AlgorithmV1)
	}
}

// Leaves are published so a caller can recompute the root themselves
// rather than trusting the one returned. If they did not fold to the
// published root, that independent check would be impossible.
func TestPublishedLeavesFoldToThePublishedRoot(t *testing.T) {
	d, err := Compute("exec-1", sample())
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(d.Leaves) != d.LeafCount {
		t.Fatalf("LeafCount %d but %d leaves published", d.LeafCount, len(d.Leaves))
	}
	// Recompute exactly as an outside verifier would.
	p, err := Prove(d, 0)
	if err != nil {
		t.Fatalf("Prove: %v", err)
	}
	ok, err := VerifyInclusion(p)
	if err != nil || !ok {
		t.Errorf("published leaves do not reproduce the published root "+
			"(ok=%v err=%v)", ok, err)
	}
}

// CanonicalLeaf is exported because it IS the specification: anyone
// writing an independent verifier has to reproduce these exact bytes.
// Testing it only through Compute would leave the wire format itself
// unpinned, so a change to the encoding would pass as long as the two
// sides changed together, which is precisely the drift that makes an
// outside verifier stop agreeing.
func TestCanonicalLeafIsSelfDelimiting(t *testing.T) {
	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	e := evt("a", 1, base, `{"n":1}`)
	leaf := string(CanonicalLeaf(e))

	// Every field appears with an explicit byte length. The length is
	// what makes the encoding unambiguous; without it, moving a
	// character across a field boundary would be invisible.
	for _, want := range []string{
		"event_id:1:a\n",
		"execution_id:6:exec-1\n",
		"event_type:10:checkpoint\n",
		"sequence:1:1\n",
		"duration_ms:1:0\n",
	} {
		if !strings.Contains(leaf, want) {
			t.Errorf("canonical leaf missing %q\ngot:\n%s", want, leaf)
		}
	}

	// The payload is committed by digest, never inlined. That is what
	// lets an inclusion proof be checked without disclosing content.
	if strings.Contains(leaf, `{"n":1}`) {
		t.Error("raw payload appears in the canonical leaf; it must be " +
			"committed by hash so a proof can be shown without " +
			"revealing the payload")
	}
	if !strings.Contains(leaf, "payload_sha256:64:") {
		t.Errorf("expected a 64-hex-char payload digest field\ngot:\n%s", leaf)
	}

	// Timestamps are normalised to UTC with fixed nanosecond width, so
	// a verifier in another zone reaches identical bytes.
	if !strings.Contains(leaf, "timestamp:30:2026-09-02T12:00:00.000000000Z\n") {
		t.Errorf("timestamp not encoded as fixed-width UTC\ngot:\n%s", leaf)
	}
}

func TestCanonicalLeafOfNilIsNil(t *testing.T) {
	if got := CanonicalLeaf(nil); got != nil {
		t.Errorf("CanonicalLeaf(nil) = %v, want nil", got)
	}
}

// A proof padded with extra hashes must be refused, not ignored.
// Surplus hashes are how a forged proof pads its way to a chosen root,
// and the RFC's sn==0 check is what stops it. Without this test that
// check looks like defensive noise and would be removed in a cleanup.
func TestProofWithSurplusHashesIsRefused(t *testing.T) {
	d, err := Compute("exec-1", sample())
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p, err := Prove(d, 0)
	if err != nil {
		t.Fatalf("Prove: %v", err)
	}
	p.Path = append(p.Path, d.Leaves[0]) // one hash too many
	ok, err := VerifyInclusion(p)
	if err != nil {
		t.Fatalf("VerifyInclusion: %v", err)
	}
	if ok {
		t.Error("a proof carrying more hashes than the tree can justify verified")
	}
}

// A truncated path must be refused too. A short path would let the
// root of a subtree stand in for the root of the whole tree.
func TestTruncatedProofIsRefused(t *testing.T) {
	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	var e []*events.Event
	for i := 0; i < 8; i++ {
		e = append(e, evt(fmt.Sprintf("evt-%d", i), i+1,
			base.Add(time.Duration(i)*time.Second), `{}`))
	}
	d, err := Compute("exec-1", e)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p, err := Prove(d, 0)
	if err != nil {
		t.Fatalf("Prove: %v", err)
	}
	if len(p.Path) < 2 {
		t.Fatalf("expected a multi-level path, got %d entries", len(p.Path))
	}
	p.Path = p.Path[:len(p.Path)-1]
	ok, err := VerifyInclusion(p)
	if err != nil {
		t.Fatalf("VerifyInclusion: %v", err)
	}
	if ok {
		t.Error("a truncated proof verified, a subtree root passed as the tree root")
	}
}

// A single-leaf tree has an empty path and the leaf IS the root.
// Worth pinning: it is the one case where the loop never runs, so a
// verifier that only works because of the loop would pass everything.
func TestSingleLeafTreeVerifies(t *testing.T) {
	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	d, err := Compute("exec-1", []*events.Event{evt("only", 1, base, `{}`)})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	p, err := Prove(d, 0)
	if err != nil {
		t.Fatalf("Prove: %v", err)
	}
	if len(p.Path) != 0 {
		t.Errorf("single-leaf proof should have an empty path, got %v", p.Path)
	}
	ok, err := VerifyInclusion(p)
	if err != nil || !ok {
		t.Errorf("single-leaf proof did not verify (ok=%v err=%v)", ok, err)
	}
}
