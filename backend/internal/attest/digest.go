// Package attest turns an execution's event record into a single
// reproducible digest, and produces the proofs that let a third party
// check one event against it.
//
// # THIS PACKAGE PROVES NOTHING BY ITSELF, AND SAYING SO IS THE POINT
//
// A digest Mesedi computes over a record Mesedi stores is exactly as
// trustworthy as Mesedi. If the record changes, the digest changes with
// it, and nobody outside can tell. Publishing that digest to a customer
// and calling it evidence would be circular.
//
// The value appears only when the digest is anchored somewhere Mesedi
// does not control — a public transparency log. Then the claim becomes
// checkable: "this exact record existed at the time that log entry was
// integrated, and has not changed since." That anchoring is Verdifax's
// job and is deliberately NOT in this package. What lives here is the
// half Mesedi is entitled to compute: the canonical summary of what it
// received.
//
// # WHY A MERKLE TREE AND NOT A SINGLE HASH
//
// A flat hash over the whole execution answers one question: has any of
// this changed. A Merkle root answers a second one that customers
// actually need: was THIS ONE EVENT part of that record, provable
// without disclosing the rest.
//
// That difference is not academic. A customer showing a regulator why
// one decision was made should not have to hand over the entire agent
// transcript to do it. With an inclusion proof they disclose one event
// and a handful of sibling hashes; everything else stays private and
// the root still verifies.
//
// # WHY RFC 6962 SPECIFICALLY
//
// The construction here is byte-for-byte the one already implemented in
// verdifax-orchestrator/internal/adapters/airgap_checkpoint.go, and the
// one Sigstore Rekor uses: leaves prefixed 0x00, internal nodes 0x01,
// an odd node at any level promoted unchanged.
//
// Copying it exactly is deliberate. Two nearly-identical tree
// implementations that disagree on one edge case — usually the odd-node
// rule — produce roots that differ only for some inputs, which is the
// worst possible failure: it works in testing and fails in production
// on the first execution with an odd event count at the wrong level.
// The domain-separation prefixes matter too; without them a leaf hash
// can be forged as an internal node.
package attest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"mesedi/backend/internal/events"
)

// ErrNoEvents means the execution recorded nothing to digest. Returned
// rather than a digest of the empty tree, because an empty root is a
// real value that would silently stand in for "we have no record of
// this run" — and those two must not look the same to a caller.
var ErrNoEvents = errors.New("attest: execution has no events to digest")

// Digest is the canonical summary of one execution's event record.
type Digest struct {
	// ExecutionID the digest covers.
	ExecutionID string `json:"execution_id"`

	// Root is the hex RFC 6962 Merkle root over the execution's event
	// leaves, in canonical order. This is the value that gets anchored.
	Root string `json:"root"`

	// LeafCount is the number of events covered. Published because a
	// root alone cannot tell a verifier how many leaves it should
	// expect, and a root over a truncated record is still a valid root.
	LeafCount int `json:"leaf_count"`

	// Leaves are the per-event leaf hashes in canonical order, hex.
	// Returned so a caller can recompute the root independently rather
	// than taking this service's word for it — which is the only way a
	// digest from an untrusted party is worth anything.
	Leaves []string `json:"leaves"`

	// Algorithm names the construction so a verifier written later, or
	// by someone else, does not have to infer it. Changing the
	// construction MUST change this string.
	Algorithm string `json:"algorithm"`
}

// AlgorithmV1 identifies this canonicalisation and tree construction.
// Any change to leaf encoding or tree shape requires a new identifier,
// because a verifier that assumes v1 rules would otherwise silently
// compute a different root and report tampering that did not happen.
const AlgorithmV1 = "mesedi-exec-merkle-v1/rfc6962-sha256"

// AlgorithmNoEventsV1 identifies the digest of an execution that
// recorded no events at all. A different construction from AlgorithmV1
// — it is not a Merkle root over anything — so per the rule above it
// gets its own identifier rather than pretending to be a v1 tree.
const AlgorithmNoEventsV1 = "mesedi-exec-no-events-v1/sha256"

// noEventsDomain separates the no-events construction from every other
// hash in this package. Without it, a value computed here could collide
// with some future construction over the same input.
const noEventsDomain = "mesedi.execution.no-events.v1"

// ComputeForChain digests an execution for inclusion in the checkpoint
// chain, including the case where it has no events.
//
// # WHY THIS EXISTS SEPARATELY FROM Compute
//
// Compute deliberately refuses an empty event list, and that refusal is
// correct: an empty Merkle root is a real value, and returning it would
// let "we have no record of this run" masquerade as "here is the
// record". The digest endpoint depends on that and answers 404.
//
// But the CHAIN cannot refuse. On 2026-09-04 a single execution with
// zero events — created and crashed 452 microseconds later, before it
// emitted anything — stopped checkpoint construction for every tenant,
// on every tick, permanently, because that execution will never acquire
// events. Any customer could cause it, deliberately or by having an SDK
// die between execution creation and its first flush.
//
// Skipping such executions instead would be worse: the chain would stop
// committing to their existence, and a row deleted afterwards would
// leave no trace. That is the omission this whole mechanism exists to
// make visible.
//
// So an event-less execution gets a digest that is honest about being
// one: domain-separated, derived from the execution id, carrying
// LeafCount 0 and its own algorithm identifier. It cannot be mistaken
// for a root over real events, it differs per execution so two empty
// runs do not collapse into one value, and the chain still commits to
// the fact that the execution happened.
func ComputeForChain(executionID string, evts []*events.Event) (Digest, error) {
	d, err := Compute(executionID, evts)
	if err == nil {
		return d, nil
	}
	if !errors.Is(err, ErrNoEvents) {
		return Digest{}, err
	}
	if executionID == "" {
		// The whole construction is derived from the id; without one
		// there is nothing to distinguish this from any other empty
		// execution, and a shared root across distinct runs is exactly
		// what the domain separation is here to prevent.
		return Digest{}, errors.New(
			"attest: cannot digest an event-less execution with no execution id")
	}

	// NUL between the domain and the id: execution ids are printable
	// and never contain it, so the preimage cannot be ambiguous the way
	// a bare concatenation could.
	sum := sha256.Sum256([]byte(noEventsDomain + "\x00" + executionID))
	return Digest{
		ExecutionID: executionID,
		Root:        hex.EncodeToString(sum[:]),
		LeafCount:   0,
		Leaves:      nil,
		Algorithm:   AlgorithmNoEventsV1,
	}, nil
}

// CanonicalLeaf returns the exact bytes hashed for one event.
//
// FIELD CHOICE IS THE SECURITY-RELEVANT PART OF THIS FILE. Anything
// omitted here can be changed after the fact without moving the root,
// so the omissions are as much a decision as the inclusions:
//
//   - event_id, execution_id, event_type, sequence: identity and
//     position. Without sequence, events could be reordered freely.
//   - timestamp in RFC 3339 nanosecond UTC: normalised to UTC so the
//     same instant expressed in two zones produces one leaf. A
//     verifier in another timezone must reach the same bytes.
//   - duration_ms: part of what the event asserts.
//   - payload digest, NOT the payload: the raw bytes are hashed rather
//     than re-encoded. Re-serialising JSON would make the leaf depend
//     on key ordering and whitespace, which Go's map iteration
//     randomises — the root would differ between two calls on
//     identical data. Hashing the bytes as received removes that
//     entirely, and has the side effect that a proof can be checked
//     without disclosing the payload.
//
// Length-prefixing every field prevents the classic concatenation
// ambiguity where two different field splits produce identical bytes.
func CanonicalLeaf(e *events.Event) []byte {
	if e == nil {
		return nil
	}
	payloadDigest := sha256.Sum256(e.Payload)

	var buf []byte
	appendField := func(label, value string) {
		// label, then length, then value. The length is what stops
		// ("ab","c") and ("a","bc") hashing identically.
		buf = append(buf, label...)
		buf = append(buf, byte(':'))
		buf = append(buf, fmt.Sprintf("%d", len(value))...)
		buf = append(buf, byte(':'))
		buf = append(buf, value...)
		buf = append(buf, byte('\n'))
	}

	appendField("event_id", e.EventID)
	appendField("execution_id", e.ExecutionID)
	appendField("event_type", string(e.EventType))
	appendField("sequence", fmt.Sprintf("%d", e.Sequence))
	appendField("timestamp", e.Timestamp.UTC().Format("2006-01-02T15:04:05.000000000Z"))
	appendField("duration_ms", fmt.Sprintf("%d", e.DurationMs))
	appendField("payload_sha256", hex.EncodeToString(payloadDigest[:]))

	return buf
}

// Compute builds the digest for one execution's events.
//
// Input order is irrelevant: the events are sorted into canonical order
// first. That matters because events legitimately arrive and are read
// back in varying orders, and a digest that depended on retrieval order
// would report tampering every time a query plan changed.
func Compute(executionID string, evts []*events.Event) (Digest, error) {
	ordered := make([]*events.Event, 0, len(evts))
	for _, e := range evts {
		if e == nil {
			continue
		}
		ordered = append(ordered, e)
	}
	if len(ordered) == 0 {
		return Digest{}, ErrNoEvents
	}

	// Canonical order is sequence, then event_id as the tiebreak.
	// Sequence alone is not a total order: duplicate sequence numbers
	// are a real condition this system detects rather than rejects
	// (see the record_integrity detector), so without the tiebreak two
	// events sharing a sequence would sort non-deterministically and
	// the root would flip between calls on identical data.
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Sequence != ordered[j].Sequence {
			return ordered[i].Sequence < ordered[j].Sequence
		}
		return ordered[i].EventID < ordered[j].EventID
	})

	leaves := make([][]byte, 0, len(ordered))
	leafHex := make([]string, 0, len(ordered))
	for _, e := range ordered {
		lh := merkleLeafHash(CanonicalLeaf(e))
		leaves = append(leaves, lh)
		leafHex = append(leafHex, hex.EncodeToString(lh))
	}

	return Digest{
		ExecutionID: executionID,
		Root:        hex.EncodeToString(rootFromLeafHashes(leaves)),
		LeafCount:   len(leaves),
		Leaves:      leafHex,
		Algorithm:   AlgorithmV1,
	}, nil
}

// InclusionProof is the sibling path proving one leaf sits under a root.
type InclusionProof struct {
	LeafIndex int      `json:"leaf_index"`
	LeafHash  string   `json:"leaf_hash"`
	TreeSize  int      `json:"tree_size"`
	Root      string   `json:"root"`
	Path      []string `json:"path"`
	Algorithm string   `json:"algorithm"`
}

// Prove returns the inclusion proof for the leaf at index i of a digest.
//
// Takes the digest rather than the events so a caller can produce a
// proof from a previously published digest without re-reading the
// record — which is exactly the position a verifier is in.
func Prove(d Digest, index int) (InclusionProof, error) {
	path, err := provePath(d.Leaves, index)
	if err != nil {
		return InclusionProof{}, err
	}
	return InclusionProof{
		LeafIndex: index,
		LeafHash:  d.Leaves[index],
		TreeSize:  len(d.Leaves),
		Root:      d.Root,
		Path:      path,
		Algorithm: AlgorithmV1,
	}, nil
}

// provePath is the sibling-path walk, over any ordered list of leaf
// hashes. Extracted from Prove so the interval tree over tenant leaves
// can reuse it instead of carrying a second copy.
//
// A second copy is exactly what this avoids. The odd-node promotion rule
// below is the single most common Merkle implementation bug, and it was
// got wrong once already in this file — see the long note on
// VerifyInclusion about leaf 2 of 3 and leaf 4 of 5. Duplicating this
// loop for the tenant tree would have been duplicating that bug's hiding
// place, and the two copies would drift the first time either was
// touched.
func provePath(leafHex []string, index int) ([]string, error) {
	if index < 0 || index >= len(leafHex) {
		return nil, fmt.Errorf(
			"attest: leaf index %d out of range for tree of %d",
			index, len(leafHex))
	}
	level := make([][]byte, 0, len(leafHex))
	for _, h := range leafHex {
		raw, err := hex.DecodeString(h)
		if err != nil {
			return nil, fmt.Errorf("attest: malformed leaf hash: %w", err)
		}
		level = append(level, raw)
	}

	var path []string
	idx := index
	for len(level) > 1 {
		next := make([][]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 == len(level) {
				// Odd node promoted unchanged. It has no sibling, so
				// nothing is added to the path — the promoted node
				// simply moves up a level. Getting this wrong is the
				// single most common Merkle implementation bug.
				next = append(next, level[i])
				continue
			}
			next = append(next, merkleNodeHash(level[i], level[i+1]))
		}
		if idx < len(level) {
			sibling := idx ^ 1
			if sibling < len(level) {
				path = append(path, hex.EncodeToString(level[sibling]))
			}
		}
		idx /= 2
		level = next
	}
	return path, nil
}

// VerifyInclusion recomputes the root from a leaf and its path.
//
// Present in this package on purpose rather than only in a verifier
// elsewhere: a proof format nobody can check from the same repository
// that produced it tends to drift, and the round-trip test that uses
// this is what catches a change to Prove that Compute did not follow.
// This is RFC 6962 section 2.1.1 verbatim, and it is verbatim on
// purpose. The first version of this function was hand-rolled: it
// consumed one path entry per level and halved the index each time.
// That is correct only for perfect trees. At a level where a node is
// PROMOTED — an odd count, last node has no sibling — Prove emits no
// path entry, so a verifier that consumes one anyway desyncs its index
// from the prover's and every promoted leaf fails.
//
// It failed for leaf 2 of 3, leaf 4 of 5, leaves 4 and 5 of 6, and so
// on: exactly the trailing positions. The package comment above warns
// that the odd-node rule is where Merkle implementations diverge, and
// the first draft of this file diverged on it anyway. The size-1
// through size-17 round-trip test is what caught it, which is the
// argument for testing every size rather than the powers of two.
//
// The RFC formulation tracks a second value, sn (the index of the last
// node at the current level), and uses it to recognise a promotion:
// when fn == sn the node is the rightmost, so its parent is itself and
// the walk skips levels until fn is odd again. Do not "simplify" this.
func VerifyInclusion(p InclusionProof) (bool, error) {
	if p.TreeSize <= 0 || p.LeafIndex < 0 || p.LeafIndex >= p.TreeSize {
		return false, fmt.Errorf(
			"attest: leaf index %d out of range for tree of %d",
			p.LeafIndex, p.TreeSize)
	}
	cur, err := hex.DecodeString(p.LeafHash)
	if err != nil {
		return false, fmt.Errorf("attest: malformed leaf hash: %w", err)
	}

	fn := p.LeafIndex    // index of the node being verified, at this level
	sn := p.TreeSize - 1 // index of the LAST node at this level

	for _, sibHex := range p.Path {
		sib, err := hex.DecodeString(sibHex)
		if err != nil {
			return false, fmt.Errorf("attest: malformed path hash: %w", err)
		}
		if sn == 0 {
			// Already at the root with path entries left over. A proof
			// carrying more hashes than the tree can justify is
			// rejected rather than ignored: extra hashes are how a
			// forged proof pads its way to a chosen root.
			return false, nil
		}
		if fn%2 == 1 || fn == sn {
			// Sibling is on the LEFT: either we are an odd-indexed
			// node, or we are the rightmost node at this level.
			cur = merkleNodeHash(sib, cur)
			// Climb past every level where this node is promoted
			// unchanged. This loop is the part the hand-rolled
			// version was missing.
			for fn != 0 && fn%2 == 0 {
				fn >>= 1
				sn >>= 1
			}
		} else {
			// Sibling is on the right.
			cur = merkleNodeHash(cur, sib)
		}
		fn >>= 1
		sn >>= 1
	}

	// sn must have reached 0 — anything else means the path was too
	// short for the declared tree size, and a short path would let a
	// subtree root masquerade as the whole tree.
	return sn == 0 && hex.EncodeToString(cur) == p.Root, nil
}

// ── RFC 6962 primitives ──────────────────────────────────────────────
//
// Copied deliberately from verdifax-orchestrator's
// internal/adapters/airgap_checkpoint.go so both sides of the eventual
// anchor agree byte-for-byte. If either changes, both must.

// merkleLeafHash is SHA-256(0x00 || data). The prefix is what stops a
// leaf being reinterpreted as an internal node.
func merkleLeafHash(data []byte) []byte {
	h := sha256.New()
	h.Write([]byte{0x00})
	h.Write(data)
	return h.Sum(nil)
}

// merkleNodeHash is SHA-256(0x01 || left || right).
func merkleNodeHash(left, right []byte) []byte {
	h := sha256.New()
	h.Write([]byte{0x01})
	h.Write(left)
	h.Write(right)
	return h.Sum(nil)
}

// rootFromLeafHashes folds already-hashed leaves into a root.
func rootFromLeafHashes(level [][]byte) []byte {
	if len(level) == 0 {
		h := sha256.Sum256(nil)
		return h[:]
	}
	for len(level) > 1 {
		next := make([][]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 == len(level) {
				next = append(next, level[i])
				continue
			}
			next = append(next, merkleNodeHash(level[i], level[i+1]))
		}
		level = next
	}
	return level[0]
}
