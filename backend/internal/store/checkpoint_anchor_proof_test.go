package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"mesedi/backend/internal/attest"
)

// Persistence of checkpoints.anchor_proof_json (migration 060), the
// envelope that makes a checkpoint verifiable without contacting the
// transparency log.
//
// THE FAILURE THIS FILE EXISTS TO CATCH is a column added to the write
// path and to ONE of the two read paths. GetCheckpointAnchor and
// listCheckpointAnchorRange carry separate SQL and separate scan lists,
// and nothing connects them. Update one, and single-checkpoint reads
// carry the proof while range reads, which is what the chain export
// and mesedi-verify actually use, silently return it empty. Empty is
// not a loud state here: it is the honest value for every checkpoint
// anchored before this column existed, so the loss reads as history
// rather than as a bug, for as long as anyone cares to look.
//
// So both read paths are asserted, on the same row, in one test.

const testAnchorProof = `{"log_id":"c0d23d6ad406973f9559f3ba2d1ca01f8414` +
	`7d8ffc5b8445c224f98b9591801d","entry_body":"eyJhcGlWZXJzaW9uIjoiMC4wLjEifQ==",` +
	`"inclusion_proof":{"logIndex":2718374165,"rootHash":"dd44","treeSize":2718374166,` +
	`"hashes":["ee55","ff66"]}}`

func TestAnchorProofSurvivesBothReadPaths(t *testing.T) {
	ctx := context.Background()
	s := cpStore(t, "anchor-proof")

	cp := buildCP(t, 0, nil, "", []attest.TenantLeaf{cpLeaf("proj-a", 2, 2, attest.ZeroHash)})
	if err := s.InsertCheckpoint(ctx, cp, []attest.TenantLeaf{cpLeaf("proj-a", 2, 2, attest.ZeroHash)}); err != nil {
		t.Fatalf("InsertCheckpoint: %v", err)
	}

	want := CheckpointAnchor{
		LogEntryID:      "2718374165",
		LedgerBackend:   "rekor",
		LeafPreimage:    "verdifax.ledger.input.v2.env." + cp.Hash + ".bind.7",
		AnchorProofJSON: testAnchorProof,
	}
	if err := s.MarkCheckpointAnchored(ctx, cp.Seq, want, time.Now().UTC()); err != nil {
		t.Fatalf("MarkCheckpointAnchored: %v", err)
	}

	// Read path 1: the single-checkpoint lookup.
	got, err := s.GetCheckpointAnchor(ctx, cp.Seq)
	if err != nil {
		t.Fatalf("GetCheckpointAnchor: %v", err)
	}
	assertAnchorProof(t, "GetCheckpointAnchor", got, want)

	// Read path 2: the range query, which is what the chain export and
	// therefore the independent verifier actually go through.
	list, err := s.ListCheckpointRange(ctx, cp.Seq, cp.Seq)
	if err != nil {
		t.Fatalf("ListCheckpointRange: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("range returned %d checkpoints, want 1", len(list))
	}
	assertAnchorProof(t, "ListCheckpointRange", list[0].Anchor, want)
}

func assertAnchorProof(t *testing.T, via string, got, want CheckpointAnchor) {
	t.Helper()
	if got.AnchorProofJSON != want.AnchorProofJSON {
		t.Errorf("%s: anchor proof was lost or altered.\n  got:  %q\n  want: %q\n"+
			"Without it this checkpoint can only be verified by contacting the "+
			"log, and an empty value is indistinguishable from a checkpoint "+
			"anchored before the column existed", via, got.AnchorProofJSON,
			want.AnchorProofJSON)
	}
	// Adjacent columns, to catch a scan-order slip rather than a
	// straight omission.
	if got.LeafPreimage != want.LeafPreimage {
		t.Errorf("%s: leaf preimage = %q, want %q", via, got.LeafPreimage, want.LeafPreimage)
	}
	if got.LogEntryID != want.LogEntryID {
		t.Errorf("%s: log entry id = %q, want %q", via, got.LogEntryID, want.LogEntryID)
	}
	if got.LedgerBackend != want.LedgerBackend {
		t.Errorf("%s: ledger backend = %q, want %q", via, got.LedgerBackend, want.LedgerBackend)
	}
	if !got.Anchored {
		t.Errorf("%s: Anchored is false for a checkpoint with a log entry id", via)
	}
}

// An anchor with no proof must persist and read back cleanly. This is
// the state of every checkpoint anchored before migration 060 and of
// every mock-backend anchor, and a store that rejected it would turn a
// missing proof into a stalled chain, strictly worse, because the
// anchor it refused was real.
func TestAnchorWithoutAProofStillPersists(t *testing.T) {
	ctx := context.Background()
	s := cpStore(t, "anchor-no-proof")

	cp := buildCP(t, 0, nil, "", []attest.TenantLeaf{cpLeaf("proj-a", 1, 1, attest.ZeroHash)})
	if err := s.InsertCheckpoint(ctx, cp, []attest.TenantLeaf{cpLeaf("proj-a", 1, 1, attest.ZeroHash)}); err != nil {
		t.Fatalf("InsertCheckpoint: %v", err)
	}
	if err := s.MarkCheckpointAnchored(ctx, cp.Seq, CheckpointAnchor{
		LogEntryID:    "rekor-abc123",
		LedgerBackend: "mock",
		LeafPreimage:  "verdifax.ledger.input.v2.env." + cp.Hash + ".bind.7",
	}, time.Now().UTC()); err != nil {
		t.Fatalf("an anchor with no proof was refused: %v", err)
	}

	got, err := s.GetCheckpointAnchor(ctx, cp.Seq)
	if err != nil {
		t.Fatalf("GetCheckpointAnchor: %v", err)
	}
	if got.AnchorProofJSON != "" {
		t.Errorf("a proof appeared for an anchor that had none: %q", got.AnchorProofJSON)
	}
	if !got.Anchored {
		t.Error("a proofless anchor is still an anchor; Anchored must be true")
	}
}

// The proof must be stored byte-for-byte as it arrived. It is the log's
// evidence passing through Mesedi, and a store that reformatted it
// would be substituting its own rendering for the original.
func TestAnchorProofIsStoredVerbatim(t *testing.T) {
	ctx := context.Background()
	s := cpStore(t, "anchor-verbatim")

	cp := buildCP(t, 0, nil, "", []attest.TenantLeaf{cpLeaf("proj-a", 1, 1, attest.ZeroHash)})
	if err := s.InsertCheckpoint(ctx, cp, []attest.TenantLeaf{cpLeaf("proj-a", 1, 1, attest.ZeroHash)}); err != nil {
		t.Fatalf("InsertCheckpoint: %v", err)
	}
	// Deliberately awkward: unusual spacing and an unknown field, the
	// kind of thing a future Sigstore response could contain.
	raw := `{ "log_id" : "abc" ,  "future_field": {"nested": [1,2,3]} }`
	if err := s.MarkCheckpointAnchored(ctx, cp.Seq, CheckpointAnchor{
		LogEntryID: "1", LedgerBackend: "rekor", AnchorProofJSON: raw,
	}, time.Now().UTC()); err != nil {
		t.Fatalf("MarkCheckpointAnchored: %v", err)
	}

	got, err := s.GetCheckpointAnchor(ctx, cp.Seq)
	if err != nil {
		t.Fatalf("GetCheckpointAnchor: %v", err)
	}
	if got.AnchorProofJSON != raw {
		t.Errorf("the stored proof was normalised on the way through.\n  got:  %s\n"+
			"  want: %s", got.AnchorProofJSON, raw)
	}
	if !strings.Contains(got.AnchorProofJSON, "future_field") {
		t.Error("a field Mesedi does not know about was dropped; the proof must " +
			"survive whatever the log and Verdifax add to it later")
	}
}
