package attest

// Interval proofs carry their own construction name (outside review
// the outside review of 2026-11-15, 2026-11-16).

import (
	"strings"
	"testing"
)

func TestIntervalProofsNameTheirOwnTree(t *testing.T) {
	e := buildExport(t, []int{2})
	iv := e.Intervals[0]
	if iv.Proof == nil {
		t.Fatal("export carries no interval proof")
	}
	if iv.Proof.Algorithm != AlgorithmIntervalV1 {
		t.Fatalf("interval proof names %q, want %q", iv.Proof.Algorithm, AlgorithmIntervalV1)
	}
	if err := VerifyTenantLeafInclusion(iv.Checkpoint, *iv.Leaf, *iv.Proof); err != nil {
		t.Fatalf("a proof with the new name failed: %v", err)
	}
}

func TestIntervalProofsIssuedBeforeTheRenameStillVerify(t *testing.T) {
	e := buildExport(t, []int{2})
	p := *e.Intervals[0].Proof
	p.Algorithm = AlgorithmV1
	if err := VerifyTenantLeafInclusion(e.Intervals[0].Checkpoint, *e.Intervals[0].Leaf, p); err != nil {
		t.Fatalf("a proof issued under the old name no longer verifies: %v", err)
	}
}

func TestIntervalProofNamingAnotherConstructionIsRefused(t *testing.T) {
	e := buildExport(t, []int{2})
	for _, alg := range []string{AlgorithmNoEventsV1, "", "mesedi-interval-merkle-v2/rfc6962-sha256"} {
		p := *e.Intervals[0].Proof
		p.Algorithm = alg
		err := VerifyTenantLeafInclusion(e.Intervals[0].Checkpoint, *e.Intervals[0].Leaf, p)
		if err == nil || !strings.Contains(err.Error(), "not an interval tree") {
			t.Errorf("algorithm %q: err = %v, want a refusal", alg, err)
		}
	}
}
