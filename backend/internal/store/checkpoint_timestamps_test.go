package store

// Anchor kinds and OpenTimestamps receipts (migration 064), on SQLite and,
// when MESEDI_TEST_POSTGRES_DSN is set, on Postgres.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest"
)

func TestAnchorKindIsStoredAndRead(t *testing.T) {
	for _, c := range cascadeTargets(t) {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			cp, leaf, digests := digestChain(t, c)
			if err := c.st.InsertCheckpointWithDigests(ctx, cp, []attest.TenantLeaf{leaf}, map[string][]ExecutionDigest{leaf.ProjectID: digests}); err != nil {
				t.Fatal(err)
			}
			a := CheckpointAnchor{LogEntryID: "777", LedgerBackend: "rekor", LeafPreimage: "mesedi.anchor.v1.x", AnchorKind: AnchorKindMesediDirect}
			if err := c.st.MarkCheckpointAnchored(ctx, cp.Seq, a, time.Now()); err != nil {
				t.Fatal(err)
			}
			got, err := c.st.GetCheckpointAnchor(ctx, cp.Seq)
			if err != nil || got.AnchorKind != AnchorKindMesediDirect {
				t.Fatalf("GetCheckpointAnchor kind = %q, err %v", got.AnchorKind, err)
			}
			byRange, err := c.st.ListCheckpointRange(ctx, cp.Seq, cp.Seq)
			if err != nil || len(byRange) != 1 || byRange[0].Anchor.AnchorKind != AnchorKindMesediDirect {
				t.Fatalf("ListCheckpointRange = %+v, err %v", byRange, err)
			}
		})
	}
}

func TestMigrationLabelsEarlierAnchorsAsThirdParty(t *testing.T) {
	for _, c := range cascadeTargets(t) {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			cp, leaf, digests := digestChain(t, c)
			if err := c.st.InsertCheckpointWithDigests(ctx, cp, []attest.TenantLeaf{leaf}, map[string][]ExecutionDigest{leaf.ProjectID: digests}); err != nil {
				t.Fatal(err)
			}
			// An anchor recorded the way every pre-2026-11-16 anchor was:
			// no kind. The migration's own UPDATE, read from its file, is
			// run again to label it.
			if err := c.st.MarkCheckpointAnchored(ctx, cp.Seq, CheckpointAnchor{LogEntryID: "555", LedgerBackend: "rekor"}, time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, err := c.db.ExecContext(ctx, migration064Update(t, c.postgre)); err != nil {
				t.Fatal(err)
			}
			got, _ := c.st.GetCheckpointAnchor(ctx, cp.Seq)
			if got.AnchorKind != AnchorKindVerdifaxAttest {
				t.Fatalf("an earlier anchor is labelled %q, want %q", got.AnchorKind, AnchorKindVerdifaxAttest)
			}
		})
	}
}

func TestTimestampReceiptsArePendingThenCompleteAndNeverDeleted(t *testing.T) {
	for _, c := range cascadeTargets(t) {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			cp, leaf, digests := digestChain(t, c)
			if err := c.st.InsertCheckpointWithDigests(ctx, cp, []attest.TenantLeaf{leaf}, map[string][]ExecutionDigest{leaf.ProjectID: digests}); err != nil {
				t.Fatal(err)
			}
			untimed, _ := c.st.ListUntimestampedCheckpoints(ctx, 1000)
			for _, u := range untimed {
				if u.Seq == cp.Seq {
					t.Fatal("an unanchored checkpoint was offered for timestamping")
				}
			}
			if err := c.st.MarkCheckpointAnchored(ctx, cp.Seq, CheckpointAnchor{LogEntryID: "888", LedgerBackend: "rekor", AnchorKind: AnchorKindMesediDirect}, time.Now()); err != nil {
				t.Fatal(err)
			}
			if !containsSeq(t, c, cp.Seq) {
				t.Fatal("an anchored checkpoint with no receipt is not offered for timestamping")
			}
			at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
			pending := CheckpointTimestamp{CheckpointSeq: cp.Seq, CalendarURL: "https://a.example", Receipt: []byte{1, 2, 3}, SubmittedAt: at}
			if err := c.st.RecordCheckpointTimestamp(ctx, pending); err != nil {
				t.Fatal(err)
			}
			pending.Receipt = []byte{9}
			if err := c.st.RecordCheckpointTimestamp(ctx, pending); err != nil {
				t.Fatal(err)
			}
			if containsSeq(t, c, cp.Seq) {
				t.Fatal("a checkpoint with a receipt is still offered for timestamping")
			}
			all, err := c.st.ListCheckpointTimestamps(ctx, cp.Seq, cp.Seq)
			if err != nil || len(all[cp.Seq]) != 1 || string(all[cp.Seq][0].Receipt) != string([]byte{1, 2, 3}) {
				t.Fatalf("receipts = %+v, err %v; a retried submission must not replace the first receipt", all[cp.Seq], err)
			}
			if !all[cp.Seq][0].SubmittedAt.Equal(at) || all[cp.Seq][0].Complete {
				t.Fatalf("pending receipt read back as %+v", all[cp.Seq][0])
			}
			if !pendingIncludes(t, c, cp.Seq) {
				t.Fatal("a pending receipt is not listed for upgrading")
			}
			if err := c.st.CompleteCheckpointTimestamp(ctx, cp.Seq, "https://a.example", []byte{4, 5}, at.Add(3*time.Hour)); err != nil {
				t.Fatal(err)
			}
			if pendingIncludes(t, c, cp.Seq) {
				t.Fatal("a complete receipt is still listed for upgrading")
			}
			if err := c.st.CompleteCheckpointTimestamp(ctx, cp.Seq, "https://a.example", []byte{6}, at.Add(4*time.Hour)); err == nil {
				t.Fatal("a complete receipt was overwritten")
			}
			all, _ = c.st.ListCheckpointTimestamps(ctx, cp.Seq, cp.Seq)
			if got := all[cp.Seq][0]; !got.Complete || string(got.Receipt) != string([]byte{4, 5}) || !got.UpgradedAt.Equal(at.Add(3*time.Hour)) {
				t.Fatalf("completed receipt read back as %+v", got)
			}
			if _, err := c.db.ExecContext(ctx, c.q(`DELETE FROM checkpoint_timestamps WHERE checkpoint_seq = ?`), int64(cp.Seq)); err == nil {
				t.Fatal("a timestamp receipt was deleted; receipts are permanent evidence")
			}
		})
	}
}

func containsSeq(t *testing.T, c cascadeTarget, seq uint64) bool {
	t.Helper()
	untimed, err := c.st.ListUntimestampedCheckpoints(context.Background(), 100000)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range untimed {
		if u.Seq == seq {
			return true
		}
	}
	return false
}

// migration064Update returns the labelling UPDATE exactly as migration
// 064 runs it on this database.
func migration064Update(t *testing.T, postgres bool) string {
	t.Helper()
	dir := "migrations"
	if postgres {
		dir = "migrations-postgres"
	}
	raw, err := os.ReadFile(filepath.Join(dir, "064_anchor_kind_and_timestamps.sql"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	start := strings.Index(src, "UPDATE checkpoints")
	if start < 0 {
		t.Fatal("migration 064 has no UPDATE checkpoints statement")
	}
	end := strings.Index(src[start:], ";")
	return src[start : start+end]
}

func pendingIncludes(t *testing.T, c cascadeTarget, seq uint64) bool {
	t.Helper()
	pending, err := c.st.ListPendingTimestamps(context.Background(), 100000)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p.CheckpointSeq == seq {
			return true
		}
	}
	return false
}
