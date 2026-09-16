package store

// Checkpoint permanence: the delete-refusal triggers installed by the
// checkpoint-permanence migration.
//
// No code path in this repository deletes from checkpoints or
// checkpoint_tenant_leaves; DeleteProject's explicit table list omits
// them on purpose. These tests are about the path that ISN'T code:
// someone with database access issuing a DELETE by hand, or a future
// retention feature reaching for the wrong table. The original schema
// made that failure mode worse, not better: the leaves table declared
// ON DELETE CASCADE to checkpoints, so one mistaken delete of a parent
// row would have silently erased every tenant leaf sealed under it.
// With the parent delete refused outright, that cascade is unreachable.
//
// Postgres twin omitted per the project's documented B18 exemption in
// .git/foundation_audit.conf. The Postgres trigger pair exists in
// migrations-postgres and refuses the same statements.

import (
	"context"
	"strings"
	"testing"

	"github.com/mesedi-ai/mesedi/backend/attest"
)

func TestCheckpointRowsRefuseDeletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := cpStore(t, "permanence")

	leaves := []attest.TenantLeaf{
		cpLeaf("proj-alpha", 2, 2, attest.ZeroHash),
		cpLeaf("proj-beta", 1, 1, attest.ZeroHash),
	}
	cp := buildCP(t, 0, nil, "", leaves)
	if err := s.InsertCheckpoint(ctx, cp, leaves); err != nil {
		t.Fatalf("InsertCheckpoint: %v", err)
	}

	mustRefuse := func(t *testing.T, query string, args ...any) {
		t.Helper()
		_, err := s.db.ExecContext(ctx, query, args...)
		if err == nil {
			t.Fatalf("delete succeeded, evidence is erasable: %s", query)
		}
		if !strings.Contains(err.Error(), "permanent") {
			t.Fatalf("delete refused but not by the permanence trigger: %v", err)
		}
	}

	t.Run("targeted checkpoint delete", func(t *testing.T) {
		mustRefuse(t, `DELETE FROM checkpoints WHERE seq = ?`, int64(cp.Seq))
	})
	t.Run("targeted leaf delete", func(t *testing.T) {
		mustRefuse(t, `DELETE FROM checkpoint_tenant_leaves WHERE checkpoint_seq = ? AND project_id = 'proj-beta'`, int64(cp.Seq))
	})
	t.Run("blanket deletes", func(t *testing.T) {
		mustRefuse(t, `DELETE FROM checkpoint_tenant_leaves`)
		mustRefuse(t, `DELETE FROM checkpoints`)
	})

	// The refusals must have left everything in place and readable.
	loaded, err := s.LatestCheckpoint(ctx)
	if err != nil || loaded == nil {
		t.Fatalf("checkpoint unreadable after refused deletes: %v", err)
	}
	if loaded.Hash != cp.Hash {
		t.Fatalf("checkpoint changed across refused deletes: %s vs %s", loaded.Hash, cp.Hash)
	}
	got, err := s.GetCheckpointLeaves(ctx, cp.Seq)
	if err != nil {
		t.Fatalf("GetCheckpointLeaves after refused deletes: %v", err)
	}
	if len(got) != len(leaves) {
		t.Fatalf("%d leaves survive, want %d", len(got), len(leaves))
	}
}

// A DELETE matching no rows is a no-op, not a refusal, because the
// trigger is row-level on both engines. This pins that the guard
// refuses destruction of evidence, not the DELETE keyword.
func TestZeroRowDeleteIsANoOpNotARefusal(t *testing.T) {
	t.Parallel()
	s := cpStore(t, "permanence-noop")
	if _, err := s.db.ExecContext(context.Background(),
		`DELETE FROM checkpoints WHERE seq = 424242`); err != nil {
		t.Fatalf("zero-row delete should be a quiet no-op, got: %v", err)
	}
}
