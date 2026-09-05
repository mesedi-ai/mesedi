package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"mesedi/backend/internal/attest"
)

// Did the log only ever grow, or is it a different log wearing the same
// name?
//
// Every other check in this program is about one entry: is it in the
// tree, at the position it claims, under a summary Sigstore signed.
// None of them notice a log that quietly rebuilt its tree with
// different contents and signed the new head. In that log, every entry
// verifies. So does every altered one. An inclusion proof cannot see
// the difference, and neither could this verifier until now.
//
// A consistency proof can. Given the tree head observed when hour N was
// published and the tree head observed when hour N+1 was published, it
// shows the later tree contains the earlier one unchanged, as a prefix.
// Chain that across the export and the covered span becomes the whole
// range the report speaks about.
//
// WHAT IT STILL DOES NOT DO. It says nothing about whether the log
// showed somebody else a different tree at the same moment. Only
// independent witnesses co-signing the same heads close that, and
// Sigstore's checkpoints today carry exactly one signature line, the
// log's own. The report says so, in the section that says what it does
// not show, and that sentence must not be deleted when this one lands.

// consistencyProofWire is the proof as Verdifax serialises it.
//
// The two ROOT hashes are deliberately absent, on the source side as
// well as here. They are taken from the signed checkpoints inside each
// interval's inclusion proof, whose signatures this program has already
// verified against Sigstore's public key. A root supplied alongside the
// proof would be a number checked against itself.
type consistencyProofWire struct {
	FirstSize int64
	LastSize  int64
	Hashes    []string
}

// anchorProofWithConsistency extends anchorProof with the new field.
// Kept separate rather than widening anchorProof so the existing
// offline path cannot start depending on a value that is absent from
// almost every export.
type anchorProofWithConsistency struct {
	InclusionProof   json.RawMessage `json:"inclusion_proof"`
	ConsistencyProof json.RawMessage `json:"consistency_proof"`
}

// treeHeadOf pulls the size and root the log signed for one interval,
// out of the checkpoint rather than out of the proof's own RootHash
// field.
//
// Those two should agree, and verifyAnchorOffline has already required
// that they do for this interval. Reading the checkpoint again here is
// not redundancy for its own sake: the checkpoint is the part Sigstore
// signed, and this function is the one place where a wrong root would
// turn a failed consistency check into a false accusation of tampering.
func treeHeadOf(iv attest.ExportedInterval) (size int64, root []byte, ok bool) {
	if len(iv.AnchorProof) == 0 {
		return 0, nil, false
	}
	var env anchorProof
	if json.Unmarshal(iv.AnchorProof, &env) != nil {
		return 0, nil, false
	}
	var proof wireInclusionProof
	if json.Unmarshal(env.InclusionProof, &proof) != nil {
		return 0, nil, false
	}
	if proof.TreeSize <= 0 || proof.RootHash == "" {
		return 0, nil, false
	}
	b, err := hex.DecodeString(proof.RootHash)
	if err != nil || len(b) != sha256.Size {
		return 0, nil, false
	}
	return proof.TreeSize, b, true
}

// consistencyProofOf returns the stored proof for one interval, if any.
func consistencyProofOf(iv attest.ExportedInterval) (*consistencyProofWire, bool) {
	if len(iv.AnchorProof) == 0 {
		return nil, false
	}
	var env anchorProofWithConsistency
	if json.Unmarshal(iv.AnchorProof, &env) != nil || len(env.ConsistencyProof) == 0 {
		return nil, false
	}
	var cp consistencyProofWire
	if json.Unmarshal(env.ConsistencyProof, &cp) != nil {
		return nil, false
	}
	return &cp, true
}

// The interior-node hash is rekorproof.go's hashChildren, reused rather
// than redeclared. Two copies of the RFC 6962 node hash in one binary
// would be two places for a domain-separation byte to drift, and the
// inclusion walk and the consistency walk MUST agree on it or they
// would be verifying against different trees while appearing to agree.

// verifyConsistency implements the RFC 9162 section 2.1.4.2 algorithm
// (RFC 6962 section 2.1.2), which decides whether the tree of size
// size2 with root root2 contains the tree of size size1 with root root1
// as a prefix.
//
// Written out rather than pulled from a dependency for the same reason
// the inclusion proof walk is: this is the file an auditor reads to
// decide whether to believe the verdict, and a reader who has to go
// fetch a module to see what was computed has not been given the
// evidence, only a citation.
//
// The algorithm was validated against real production data before this
// function existed: Rekor's proof between the trees at checkpoints 24
// and 27 of the 5 September 2026 run verifies against the two roots
// stored in that export, and fails against any other root.
func verifyConsistency(size1 int64, root1 []byte, size2 int64, root2 []byte, proof [][]byte) error {
	switch {
	case size1 < 0 || size2 < 0:
		return fmt.Errorf("tree sizes must not be negative (%d, %d)", size1, size2)
	case size1 > size2:
		return fmt.Errorf("the earlier tree is larger than the later one (%d then %d), "+
			"which would mean the log shrank", size1, size2)
	case size1 == 0:
		// An empty tree is a prefix of everything. Nothing to check.
		return nil
	case size1 == size2:
		if len(proof) != 0 {
			return fmt.Errorf("a proof was supplied between a tree and itself")
		}
		if !equalBytes(root1, root2) {
			return fmt.Errorf("two observations of tree size %d have different roots, "+
				"so the log served two different trees at the same size", size1)
		}
		return nil
	case len(proof) == 0:
		return fmt.Errorf("no proof path was supplied between tree sizes %d and %d",
			size1, size2)
	}

	fn, sn := size1-1, size2-1
	for fn&1 == 1 {
		fn >>= 1
		sn >>= 1
	}

	var fr, sr []byte
	path := proof
	if size1&(size1-1) == 0 {
		// size1 is an exact power of two, so the first node of the proof
		// is implicit: it is root1 itself.
		fr, sr = root1, root1
	} else {
		fr, sr = path[0], path[0]
		path = path[1:]
	}

	for _, c := range path {
		if sn == 0 {
			return fmt.Errorf("the proof path is longer than the trees allow, so it does " +
				"not describe these two trees")
		}
		if fn&1 == 1 || fn == sn {
			fr = hashChildren(c, fr)
			sr = hashChildren(c, sr)
			for fn != 0 && fn&1 == 0 {
				fn >>= 1
				sn >>= 1
			}
		} else {
			sr = hashChildren(sr, c)
		}
		fn >>= 1
		sn >>= 1
	}

	if sn != 0 {
		return fmt.Errorf("the proof path ended early, so it does not reach the later tree")
	}
	if !equalBytes(fr, root1) {
		return fmt.Errorf("the proof does not rebuild the earlier tree's root, so it is " +
			"not a proof about the tree this record was published in")
	}
	if !equalBytes(sr, root2) {
		return fmt.Errorf("the proof does not rebuild the later tree's root. The log did " +
			"not simply grow between these two moments")
	}
	return nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// growthResult is what the run established about the log itself, as
// opposed to about the entries in it.
type growthResult struct {
	// Proven is the number of consecutive hour-to-hour links where the
	// log demonstrated it only grew.
	Proven int

	// Unproven is the number of links where no consistency proof was
	// stored. Every checkpoint anchored before 5 September 2026 is in
	// this bucket, and so is any anchor where the log declined to
	// answer.
	Unproven int

	// Failures are links where a proof WAS stored and did not hold.
	// These are findings, not gaps, and they make the report fail.
	Failures []string
}

// checkLogGrowth walks consecutive intervals and verifies whatever
// consistency proofs the export carries.
//
// Deliberately reports Unproven separately from Failures. A missing
// proof and a broken proof are opposite claims: one says the question
// was never put to the log, the other says it was put and the answer
// was no. Collapsing them, in either direction, is the mistake this
// codebase has made three times and now names in its own report.
func checkLogGrowth(intervals []attest.ExportedInterval) growthResult {
	var res growthResult
	for i := 1; i < len(intervals); i++ {
		prev, cur := intervals[i-1], intervals[i]
		if cur.Checkpoint.Seq != prev.Checkpoint.Seq+1 {
			continue // not adjacent; the chain check already said so
		}

		cp, ok := consistencyProofOf(cur)
		if !ok {
			res.Unproven++
			continue
		}
		prevSize, prevRoot, okPrev := treeHeadOf(prev)
		curSize, curRoot, okCur := treeHeadOf(cur)
		if !okPrev || !okCur {
			res.Unproven++
			continue
		}
		// The proof must be about THESE two trees. A proof between two
		// other sizes could verify perfectly and say nothing about this
		// pair, which is the same trap as an inclusion proof for an
		// entry that is not yours.
		if cp.FirstSize != prevSize || cp.LastSize != curSize {
			res.Failures = append(res.Failures, fmt.Sprintf(
				"hour %d carries a growth proof about trees of %d and %d entries, but "+
					"hour %d was published in a tree of %d and hour %d in a tree of %d",
				cur.Checkpoint.Seq, cp.FirstSize, cp.LastSize,
				prev.Checkpoint.Seq, prevSize, cur.Checkpoint.Seq, curSize))
			continue
		}
		path, err := decodeProofPath(cp.Hashes)
		if err != nil {
			res.Failures = append(res.Failures, fmt.Sprintf(
				"hour %d carries a malformed growth proof: %v", cur.Checkpoint.Seq, err))
			continue
		}
		if err := verifyConsistency(prevSize, prevRoot, curSize, curRoot, path); err != nil {
			res.Failures = append(res.Failures, fmt.Sprintf(
				"between hour %d and hour %d: %v",
				prev.Checkpoint.Seq, cur.Checkpoint.Seq, err))
			continue
		}
		res.Proven++
	}
	return res
}

// countOf is a local plural helper. attest.plural is unexported and
// this is a different package; exporting it to save four lines would
// widen that package's surface for a formatting detail.
func countOf(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// growthSummary is the one sentence this goes into the report as.
func growthSummary(res growthResult) string {
	switch {
	case len(res.Failures) > 0:
		return "The public log did NOT only grow between the moments this record was " +
			"published. " + strings.Join(res.Failures, "; ") +
			". This is a finding about the log itself, not about Mesedi."
	case res.Proven == 0:
		return ""
	case res.Unproven == 0:
		return fmt.Sprintf("Across all %s covered here, the public log proved it had "+
			"only been added to since the hour before, never rewritten. Checked here "+
			"against summaries Sigstore signed, without contacting it.",
			countOf(res.Proven, "hour-to-hour step", "hour-to-hour steps"))
	default:
		return fmt.Sprintf("For %s the public log proved it had only been added to "+
			"since the hour before, never rewritten. %s carry no such proof and are "+
			"not covered by that statement.",
			countOf(res.Proven, "hour-to-hour step", "hour-to-hour steps"),
			countOf(res.Unproven, "step does", "steps do"))
	}
}

// decodeProofPath turns the hex path into bytes, refusing anything that
// is not a SHA-256 hash rather than letting a short value silently
// produce a wrong root and read as tampering.
func decodeProofPath(hexes []string) ([][]byte, error) {
	out := make([][]byte, 0, len(hexes))
	for i, h := range hexes {
		b, err := hex.DecodeString(h)
		if err != nil {
			return nil, fmt.Errorf("proof step %d is not hex", i)
		}
		if len(b) != sha256.Size {
			return nil, fmt.Errorf("proof step %d is %d bytes, not 32", i, len(b))
		}
		out = append(out, b)
	}
	return out, nil
}
