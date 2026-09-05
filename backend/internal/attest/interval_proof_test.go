package attest

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"mesedi/backend/internal/events"
)

// Tree sizes are swept 1..17 rather than spot-checked at powers of two.
// The note on VerifyInclusion records that the first draft of the sibling
// walk was correct for perfect trees and wrong for leaf 2 of 3, leaf 4 of
// 5, leaves 4 and 5 of 6, the trailing positions where a node is
// promoted. Powers of two would have passed every one of those.

func intervalLeaves(t *testing.T, n int) []TenantLeaf {
	t.Helper()
	out := make([]TenantLeaf, 0, n)
	for i := range n {
		out = append(out, TenantLeaf{
			// Zero-padded so lexical order matches numeric order; the
			// checkpoint builder commits leaves sorted by project id and
			// proofs are index-based, so a mismatch here would be a test
			// bug masquerading as a proof bug.
			ProjectID:       fmt.Sprintf("proj-%03d", i),
			IntervalRoot:    strings.Repeat(fmt.Sprintf("%x", i%16), 64),
			ExecutionCount:  i + 1,
			CumulativeCount: uint64(i + 1),
			PrevLeafHash:    ZeroHash,
		})
	}
	return out
}

func checkpointOver(t *testing.T, leaves []TenantLeaf) Checkpoint {
	t.Helper()
	c, err := BuildCheckpoint(CheckpointParams{
		IntervalStart: hour(0),
		IntervalEnd:   hour(1),
		Interval:      testInterval,
		Leaves:        leaves,
		Now:           hour(1),
	})
	if err != nil {
		t.Fatalf("BuildCheckpoint over %d leaves: %v", len(leaves), err)
	}
	return c
}

func TestTenantLeafProofRoundTripAcrossTreeSizes(t *testing.T) {
	for n := 1; n <= 17; n++ {
		leaves := intervalLeaves(t, n)
		c := checkpointOver(t, leaves)

		for i, leaf := range leaves {
			p, err := ProveTenantLeaf(c, leaves, leaf.ProjectID)
			if err != nil {
				t.Fatalf("tree of %d, leaf %d: ProveTenantLeaf: %v", n, i, err)
			}
			if p.LeafIndex != i {
				t.Errorf("tree of %d: project %q proved at index %d, want %d",
					n, leaf.ProjectID, p.LeafIndex, i)
			}
			if err := VerifyTenantLeafInclusion(c, leaf, p); err != nil {
				t.Errorf("tree of %d, leaf %d: %v", n, i, err)
			}
		}
	}
}

// The privacy claim is the reason this exists at all, so assert it rather
// than assume it: a proof must carry far fewer hashes than there are
// tenants, or an agency verifying its own inclusion still learns how many
// customers there are.
func TestTenantLeafProofDoesNotDiscloseEveryTenant(t *testing.T) {
	const n = 64
	leaves := intervalLeaves(t, n)
	c := checkpointOver(t, leaves)

	p, err := ProveTenantLeaf(c, leaves, "proj-000")
	if err != nil {
		t.Fatalf("ProveTenantLeaf: %v", err)
	}
	// log2(64) = 6. Allow a little slack for promotion levels, but nothing
	// close to n; the point is the proof is logarithmic, not linear.
	if len(p.Path) > 8 {
		t.Errorf("proof carries %d sibling hashes for %d tenants; it should be "+
			"logarithmic, otherwise the auditor learns the customer count",
			len(p.Path), n)
	}
	if len(p.Path) == 0 {
		t.Error("proof carries no siblings for a 64-leaf tree, which cannot be right")
	}
}

// Keyed by project id specifically so this cannot happen: a proof that
// verifies cleanly but describes a different tenant's leaf.
func TestVerifyTenantLeafInclusionRejectsAMismatchedLeaf(t *testing.T) {
	leaves := intervalLeaves(t, 8)
	c := checkpointOver(t, leaves)

	p, err := ProveTenantLeaf(c, leaves, "proj-003")
	if err != nil {
		t.Fatalf("ProveTenantLeaf: %v", err)
	}

	// A genuine proof paired with someone else's leaf.
	err = VerifyTenantLeafInclusion(c, leaves[4], p)
	if err == nil {
		t.Fatal("a valid proof for one tenant verified against another tenant's leaf")
	}

	// A genuine proof paired with a leaf whose counts have been edited.
	tampered := leaves[3]
	tampered.ExecutionCount += 1000
	if err := VerifyTenantLeafInclusion(c, tampered, p); err == nil {
		t.Error("a leaf with an inflated execution count verified against an untampered proof")
	}
}

func TestProveTenantLeafRefusesLeavesThatDoNotProduceTheRoot(t *testing.T) {
	leaves := intervalLeaves(t, 5)
	c := checkpointOver(t, leaves)

	// Reordered: exactly what a caller gets from an unsorted query, and
	// exactly the case that would otherwise yield a confident wrong proof.
	shuffled := []TenantLeaf{leaves[2], leaves[0], leaves[1], leaves[3], leaves[4]}
	if _, err := ProveTenantLeaf(c, shuffled, "proj-000"); err == nil {
		t.Error("proving against reordered leaves succeeded; the resulting proof " +
			"would be against a root nobody anchored")
	}

	// One leaf silently dropped.
	if _, err := ProveTenantLeaf(c, leaves[:4], "proj-000"); err == nil {
		t.Error("proving against a truncated leaf set succeeded")
	}
}

// "You had no activity this hour" is a legitimate answer and must be
// distinguishable from "verification failed", or an agency that simply ran
// nothing will think the chain is broken.
func TestProveTenantLeafSaysAbsentRatherThanFailing(t *testing.T) {
	leaves := intervalLeaves(t, 4)
	c := checkpointOver(t, leaves)

	_, err := ProveTenantLeaf(c, leaves, "proj-not-present")
	if !errors.Is(err, ErrLeafNotInInterval) {
		t.Errorf("absent project should return ErrLeafNotInInterval, got %v", err)
	}
}

// A proof against a root from a different hour must not verify, even
// though both roots are well-formed and both proofs are internally valid.
func TestVerifyTenantLeafInclusionRejectsACrossIntervalProof(t *testing.T) {
	leavesA := intervalLeaves(t, 6)
	cA := checkpointOver(t, leavesA)

	leavesB := intervalLeaves(t, 6)
	leavesB[0].ExecutionCount = 999 // different hour, different activity
	cB, err := BuildCheckpoint(CheckpointParams{
		Prev:           &cA,
		PrevLogEntryID: "2712467820",
		IntervalStart:  hour(1),
		IntervalEnd:    hour(2),
		Interval:       testInterval,
		Leaves:         leavesB,
		Now:            hour(2),
	})
	if err != nil {
		t.Fatalf("BuildCheckpoint B: %v", err)
	}

	pA, err := ProveTenantLeaf(cA, leavesA, "proj-002")
	if err != nil {
		t.Fatalf("ProveTenantLeaf: %v", err)
	}
	if err := VerifyTenantLeafInclusion(cB, leavesA[2], pA); err == nil {
		t.Error("a proof from interval A verified against interval B's checkpoint")
	}
}

func TestProveTenantLeafRefusesEmptyProjectID(t *testing.T) {
	leaves := intervalLeaves(t, 3)
	c := checkpointOver(t, leaves)
	if _, err := ProveTenantLeaf(c, leaves, ""); err == nil {
		t.Error("empty project id was accepted")
	}
}

// An empty interval has no leaves and an empty root. Proving anything
// against it must fail cleanly rather than panic or return a proof over a
// zero-length tree.
func TestProveTenantLeafOnAnEmptyInterval(t *testing.T) {
	c, err := BuildCheckpoint(CheckpointParams{
		IntervalStart: hour(0),
		IntervalEnd:   hour(1),
		Interval:      testInterval,
		Leaves:        nil,
		Now:           hour(1),
	})
	if err != nil {
		t.Fatalf("BuildCheckpoint empty: %v", err)
	}
	if c.MerkleRoot != "" {
		t.Fatalf("empty interval should have an empty root, got %q", c.MerkleRoot)
	}
	if _, err := ProveTenantLeaf(c, nil, "proj-000"); err == nil {
		t.Error("proving against an empty interval succeeded")
	}
}

// Guards the refactor that extracted provePath: the execution-level proofs
// in digest.go must be byte-identical to what they were before, since they
// now share a code path with the interval tree.
func TestExecutionProofsStillWorkAfterSharingTheProver(t *testing.T) {
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	// Sizes 6, 7 and 8 specifically: 7 is the smallest tree with promotion
	// at more than one level, and it is the shape most likely to expose a
	// desync between prover and verifier if the extraction went wrong.
	for _, size := range []int{6, 7, 8} {
		var e []*events.Event
		for i := range size {
			e = append(e, evt(fmt.Sprintf("evt-%03d", i), i+1,
				base.Add(time.Duration(i)*time.Second),
				fmt.Sprintf(`{"i":%d}`, i)))
		}
		d, err := Compute("exec-shared-prover", e)
		if err != nil {
			t.Fatalf("Compute size %d: %v", size, err)
		}
		for i := range d.Leaves {
			p, err := Prove(d, i)
			if err != nil {
				t.Fatalf("size %d, Prove(%d): %v", size, i, err)
			}
			ok, err := VerifyInclusion(p)
			if err != nil || !ok {
				t.Errorf("size %d: execution leaf %d no longer verifies after the "+
					"provePath extraction: ok=%v err=%v", size, i, ok, err)
			}
		}
	}
}
