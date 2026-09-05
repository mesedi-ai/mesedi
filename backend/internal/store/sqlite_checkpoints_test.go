package store

// Tests for checkpoint chain persistence (migration 058).
//
// THE ONE THAT MATTERS MOST is TestCheckpointRoundTripsWithItsHashIntact.
//
// The store persists attest.Checkpoint field by field. Add a field to
// that struct, forget to persist it, and a reloaded checkpoint carries
// a zero value where the original had data, so it recomputes to a
// DIFFERENT hash, chain verification fails, and a verifier reports
// tampering on a row nobody touched. That failure would point an
// auditor at a crime that did not happen, which is worse than missing a
// real one: it burns the credibility the whole mechanism runs on.
//
// The round-trip test is the only thing standing between that struct
// and that outcome. If it fails after someone adds a field, the fix is
// to persist the field, not to loosen the test.
//
// Postgres twin omitted per the project's documented B18 exemption in
// .git/foundation_audit.conf. The Postgres IMPLEMENTATION exists.

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"mesedi/backend/internal/attest"
)

func cpStore(t *testing.T, name string) *SQLiteStore {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := OpenSQLite(filepath.Join(t.TempDir(), name+".db"), logger)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func cpHour(n int) time.Time {
	return time.Date(2026, 9, 3, 12+n, 0, 0, 0, time.UTC)
}

func cpLeaf(project string, execCount int, cumulative uint64, prev string) attest.TenantLeaf {
	root := ""
	if execCount > 0 {
		root = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	}
	return attest.TenantLeaf{
		ProjectID:       project,
		IntervalRoot:    root,
		ExecutionCount:  execCount,
		CumulativeCount: cumulative,
		PrevLeafHash:    prev,
	}
}

// buildCP produces a genesis checkpoint over the given leaves.
func buildCP(t *testing.T, n int, prev *attest.Checkpoint, prevEntry string,
	leaves []attest.TenantLeaf) attest.Checkpoint {
	t.Helper()
	cp, err := attest.BuildCheckpoint(attest.CheckpointParams{
		Prev:           prev,
		PrevLogEntryID: prevEntry,
		IntervalStart:  cpHour(n),
		IntervalEnd:    cpHour(n + 1),
		Interval:       time.Hour,
		Leaves:         leaves,
		Now:            cpHour(n + 1).Add(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("BuildCheckpoint: %v", err)
	}
	return cp
}

// ── the guard ────────────────────────────────────────────────────────

func TestCheckpointRoundTripsWithItsHashIntact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := cpStore(t, "roundtrip")

	leaves := []attest.TenantLeaf{
		cpLeaf("proj-alpha", 3, 3, attest.ZeroHash),
		cpLeaf("proj-beta", 2, 2, attest.ZeroHash),
	}
	cp := buildCP(t, 0, nil, "", leaves)

	if err := s.InsertCheckpoint(ctx, cp, leaves); err != nil {
		t.Fatalf("InsertCheckpoint: %v", err)
	}

	loaded, err := s.LatestCheckpoint(ctx)
	if err != nil {
		t.Fatalf("LatestCheckpoint: %v", err)
	}
	if loaded == nil {
		t.Fatal("no checkpoint returned after inserting one")
	}

	// The assertion the whole design rests on: what came back out
	// hashes to what went in.
	if got := attest.CheckpointHash(*loaded); got != cp.Hash {
		t.Fatalf("a checkpoint that round-tripped through the database no longer "+
			"hashes to the same value.\n stored:    %s\n recomputed: %s\n"+
			"A field on attest.Checkpoint is almost certainly not being persisted. "+
			"Fix the persistence, do NOT loosen this test: the chain would report "+
			"tampering on a row nobody touched.", cp.Hash, got)
	}

	// And field by field, so a failure says WHICH one.
	if loaded.Seq != cp.Seq {
		t.Errorf("Seq: %d != %d", loaded.Seq, cp.Seq)
	}
	if loaded.Format != cp.Format {
		t.Errorf("Format: %q != %q", loaded.Format, cp.Format)
	}
	if loaded.PrevCheckpointHash != cp.PrevCheckpointHash {
		t.Errorf("PrevCheckpointHash: %q != %q", loaded.PrevCheckpointHash, cp.PrevCheckpointHash)
	}
	if loaded.PrevLogEntryID != cp.PrevLogEntryID {
		t.Errorf("PrevLogEntryID: %q != %q", loaded.PrevLogEntryID, cp.PrevLogEntryID)
	}
	if !loaded.IntervalStart.Equal(cp.IntervalStart) {
		t.Errorf("IntervalStart: %s != %s", loaded.IntervalStart, cp.IntervalStart)
	}
	if !loaded.IntervalEnd.Equal(cp.IntervalEnd) {
		t.Errorf("IntervalEnd: %s != %s", loaded.IntervalEnd, cp.IntervalEnd)
	}
	if !loaded.CreatedAtUnattested.Equal(cp.CreatedAtUnattested) {
		t.Errorf("CreatedAtUnattested: %s != %s", loaded.CreatedAtUnattested, cp.CreatedAtUnattested)
	}
	if loaded.TenantLeafCount != cp.TenantLeafCount {
		t.Errorf("TenantLeafCount: %d != %d", loaded.TenantLeafCount, cp.TenantLeafCount)
	}
	if loaded.MerkleRoot != cp.MerkleRoot {
		t.Errorf("MerkleRoot: %q != %q", loaded.MerkleRoot, cp.MerkleRoot)
	}
	if loaded.CumulativeCount != cp.CumulativeCount {
		t.Errorf("CumulativeCount: %d != %d", loaded.CumulativeCount, cp.CumulativeCount)
	}
}

// An empty interval must survive the round trip too: empty MerkleRoot
// must come back as empty, not as NULL-turned-something-else.
func TestEmptyCheckpointRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := cpStore(t, "empty-roundtrip")

	cp := buildCP(t, 0, nil, "", nil)
	if cp.MerkleRoot != "" {
		t.Fatalf("test setup: expected an empty root, got %q", cp.MerkleRoot)
	}
	if err := s.InsertCheckpoint(ctx, cp, nil); err != nil {
		t.Fatalf("InsertCheckpoint: %v", err)
	}
	loaded, err := s.LatestCheckpoint(ctx)
	if err != nil || loaded == nil {
		t.Fatalf("LatestCheckpoint: %v", err)
	}
	if loaded.MerkleRoot != "" {
		t.Errorf("empty root came back as %q", loaded.MerkleRoot)
	}
	if got := attest.CheckpointHash(*loaded); got != cp.Hash {
		t.Errorf("empty checkpoint no longer hashes the same: %s vs %s", got, cp.Hash)
	}
}

// Leaves must come back IN HASHED ORDER, and that order must reproduce
// the anchored root.
func TestCheckpointLeavesReturnInHashedOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := cpStore(t, "leaf-order")

	// Deliberately NOT alphabetical, so an accidental ORDER BY
	// project_id would produce a different order and a different root.
	leaves := []attest.TenantLeaf{
		cpLeaf("zeta", 1, 1, attest.ZeroHash),
		cpLeaf("alpha", 2, 2, attest.ZeroHash),
		cpLeaf("mid", 3, 3, attest.ZeroHash),
	}
	cp := buildCP(t, 0, nil, "", leaves)
	if err := s.InsertCheckpoint(ctx, cp, leaves); err != nil {
		t.Fatalf("InsertCheckpoint: %v", err)
	}

	got, err := s.GetCheckpointLeaves(ctx, cp.Seq)
	if err != nil {
		t.Fatalf("GetCheckpointLeaves: %v", err)
	}
	if len(got) != len(leaves) {
		t.Fatalf("got %d leaves, want %d", len(got), len(leaves))
	}
	for i := range leaves {
		if got[i].ProjectID != leaves[i].ProjectID {
			t.Fatalf("leaf %d is %q, want %q, leaves are not coming back in the "+
				"order they were hashed, so the tree cannot be reproduced",
				i, got[i].ProjectID, leaves[i].ProjectID)
		}
	}
	// The real proof: reconstructing the tree from what came back
	// produces the root that was anchored.
	if err := attest.VerifyIntervalLeaves(*mustLoad(t, s, cp.Seq), got); err != nil {
		t.Errorf("leaves loaded from the database do not reproduce the anchored "+
			"root: %v", err)
	}
}

func mustLoad(t *testing.T, s *SQLiteStore, seq uint64) *attest.Checkpoint {
	t.Helper()
	cp, err := s.GetCheckpoint(context.Background(), seq)
	if err != nil || cp == nil {
		t.Fatalf("GetCheckpoint(%d): %v", seq, err)
	}
	return cp
}

// ── chain continuity across a restart ────────────────────────────────

func TestChainExtendsFromWhatWasPersisted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := cpStore(t, "extend")

	l1 := cpLeaf("proj-alpha", 2, 2, attest.ZeroHash)
	cp1 := buildCP(t, 0, nil, "", []attest.TenantLeaf{l1})
	if err := s.InsertCheckpoint(ctx, cp1, []attest.TenantLeaf{l1}); err != nil {
		t.Fatalf("insert cp1: %v", err)
	}
	if err := s.MarkCheckpointAnchored(ctx, cp1.Seq, CheckpointAnchor{
		LogEntryID: "rekor-111", LedgerBackend: "mock",
		LeafPreimage: "verdifax.ledger.input.v2.env." + cp1.Hash + ".bind.1",
	}, cpHour(1)); err != nil {
		t.Fatalf("MarkCheckpointAnchored: %v", err)
	}

	// Simulate a restart: everything the scheduler needs comes from
	// the database, not from memory.
	head, err := s.LatestCheckpoint(ctx)
	if err != nil || head == nil {
		t.Fatalf("LatestCheckpoint: %v", err)
	}
	prevLeaf, err := s.LatestTenantLeaf(ctx, "proj-alpha")
	if err != nil || prevLeaf == nil {
		t.Fatalf("LatestTenantLeaf: %v", err)
	}
	if prevLeaf.CumulativeCount != 2 {
		t.Errorf("prev leaf cumulative = %d, want 2", prevLeaf.CumulativeCount)
	}

	l2 := cpLeaf("proj-alpha", 1, prevLeaf.CumulativeCount+1, attest.TenantLeafHash(*prevLeaf))
	cp2 := buildCP(t, 1, head, "rekor-111", []attest.TenantLeaf{l2})
	if err := s.InsertCheckpoint(ctx, cp2, []attest.TenantLeaf{l2}); err != nil {
		t.Fatalf("insert cp2: %v", err)
	}

	// The chain built across a "restart" must verify end to end.
	if err := attest.VerifyChain([]attest.Checkpoint{*head, *mustLoad(t, s, cp2.Seq)},
		time.Hour); err != nil {
		t.Errorf("chain rebuilt from persisted state does not verify: %v", err)
	}
}

func TestLatestTenantLeafIsNilForAnUnseenProject(t *testing.T) {
	t.Parallel()
	s := cpStore(t, "unseen")
	l, err := s.LatestTenantLeaf(context.Background(), "never-seen")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l != nil {
		t.Errorf("expected nil for a project with no leaves, got %+v", l)
	}
}

func TestLatestCheckpointIsNilBeforeGenesis(t *testing.T) {
	t.Parallel()
	s := cpStore(t, "pre-genesis")
	cp, err := s.LatestCheckpoint(context.Background())
	if err != nil {
		t.Fatalf("an empty chain must not be an error, got: %v", err)
	}
	if cp != nil {
		t.Errorf("expected nil before genesis, got %+v", cp)
	}
}

// ── refusals ─────────────────────────────────────────────────────────

func TestInsertCheckpointRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := cpStore(t, "refusals")
	leaves := []attest.TenantLeaf{cpLeaf("proj-alpha", 1, 1, attest.ZeroHash)}
	cp := buildCP(t, 0, nil, "", leaves)

	t.Run("leaf count disagreeing with the checkpoint", func(t *testing.T) {
		if err := s.InsertCheckpoint(ctx, cp, nil); err == nil {
			t.Error("stored 0 leaves for a checkpoint committing to 1; the tree " +
				"would be unreconstructable")
		}
	})

	t.Run("hash not matching contents", func(t *testing.T) {
		bad := cp
		bad.CumulativeCount = 999 // Hash now describes different contents
		if err := s.InsertCheckpoint(ctx, bad, leaves); err == nil {
			t.Error("stored a checkpoint whose hash does not describe its own row")
		}
	})

	// Insert for real, then prove history cannot be rewritten.
	if err := s.InsertCheckpoint(ctx, cp, leaves); err != nil {
		t.Fatalf("InsertCheckpoint: %v", err)
	}
	t.Run("re-inserting an existing sequence", func(t *testing.T) {
		other := buildCP(t, 0, nil, "",
			[]attest.TenantLeaf{cpLeaf("proj-beta", 5, 5, attest.ZeroHash)})
		if err := s.InsertCheckpoint(ctx, other, []attest.TenantLeaf{cpLeaf("proj-beta", 5, 5, attest.ZeroHash)}); err == nil {
			t.Error("overwrote an existing checkpoint sequence; a checkpoint is an " +
				"anchored fact and replacing one rewrites history")
		}
	})
}

func TestMarkCheckpointAnchoredRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := cpStore(t, "anchor-refusals")
	leaves := []attest.TenantLeaf{cpLeaf("proj-alpha", 1, 1, attest.ZeroHash)}
	cp := buildCP(t, 0, nil, "", leaves)
	if err := s.InsertCheckpoint(ctx, cp, leaves); err != nil {
		t.Fatalf("InsertCheckpoint: %v", err)
	}

	if err := s.MarkCheckpointAnchored(ctx, cp.Seq,
		CheckpointAnchor{LogEntryID: "", LedgerBackend: "mock"}, cpHour(1)); err == nil {
		t.Error("accepted an empty log entry id; an anchor with nothing to point " +
			"at cannot be verified")
	}
	if err := s.MarkCheckpointAnchored(ctx, 999,
		CheckpointAnchor{LogEntryID: "rekor-x", LedgerBackend: "mock"}, cpHour(1)); err == nil {
		t.Error("marked a nonexistent checkpoint as anchored")
	}
	if err := s.MarkCheckpointAnchored(ctx, cp.Seq,
		CheckpointAnchor{LogEntryID: "rekor-1", LedgerBackend: "mock"}, cpHour(1)); err != nil {
		t.Fatalf("first anchor should succeed: %v", err)
	}
	if err := s.MarkCheckpointAnchored(ctx, cp.Seq,
		CheckpointAnchor{LogEntryID: "rekor-2", LedgerBackend: "mock"}, cpHour(1)); err == nil {
		t.Error("overwrote an existing anchor; two log entries for one checkpoint " +
			"is evidence, and keeping only the second discards it")
	}
}

// Leaves get the same treatment as the checkpoint itself. Storing
// leaf_hash and never recomputing it would make it decoration, and a
// row edited by someone with database access would be returned clean.
func TestEditedLeafRowIsDetectedOnLoad(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := cpStore(t, "edited-leaf")
	leaves := []attest.TenantLeaf{
		cpLeaf("proj-alpha", 2, 2, attest.ZeroHash),
		cpLeaf("proj-beta", 3, 3, attest.ZeroHash),
	}
	cp := buildCP(t, 0, nil, "", leaves)
	if err := s.InsertCheckpoint(ctx, cp, leaves); err != nil {
		t.Fatalf("InsertCheckpoint: %v", err)
	}

	// Understate one tenant's activity, leaving leaf_hash alone, the
	// edit someone would make to shrink what a customer appears to have
	// run.
	if _, err := s.db.ExecContext(ctx, `
		UPDATE checkpoint_tenant_leaves SET execution_count = 1
		 WHERE checkpoint_seq = ? AND project_id = 'proj-beta'
	`, int64(cp.Seq)); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	if _, err := s.GetCheckpointLeaves(ctx, cp.Seq); err == nil {
		t.Error("GetCheckpointLeaves returned an edited leaf without complaint")
	}
	if _, err := s.LatestTenantLeaf(ctx, "proj-beta"); err == nil {
		t.Error("LatestTenantLeaf returned an edited leaf without complaint; the " +
			"next leaf would be built on a corrupted predecessor")
	}
	// An untouched project must still read fine, the check has to be
	// per row, not a blanket failure that hides which row is bad.
	if _, err := s.LatestTenantLeaf(ctx, "proj-alpha"); err != nil {
		t.Errorf("an untouched project's leaf failed to load: %v", err)
	}
}

// A checkpoint edited directly in the database must be caught on read,
// as a distinct event from the chain merely failing to verify later.
func TestEditedRowIsDetectedOnLoad(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := cpStore(t, "edited")
	leaves := []attest.TenantLeaf{cpLeaf("proj-alpha", 1, 1, attest.ZeroHash)}
	cp := buildCP(t, 0, nil, "", leaves)
	if err := s.InsertCheckpoint(ctx, cp, leaves); err != nil {
		t.Fatalf("InsertCheckpoint: %v", err)
	}

	// Tamper the way someone with database access would: change a
	// value, leave the stored hash alone.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE checkpoints SET cumulative_count = 4242 WHERE seq = ?`,
		int64(cp.Seq)); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	if _, err := s.LatestCheckpoint(ctx); err == nil {
		t.Error("an edited checkpoint row loaded without complaint; storing the " +
			"hash buys nothing if nothing recomputes it")
	}
}
