package store

// Coverage-debt batch one, the auth-critical set: every request's
// authentication rides GetAPIKeyByHash and TouchAPIKey, revocation
// rides the delete paths, and until the 2026-09-12 split surfaced
// it, no test anywhere named them. This walks the full key
// lifecycle: minted, found by hash, touched, listed, deleted by id,
// gone.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func newAPIKeyTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.CreateProject(context.Background(), &Project{
		ProjectID: "proj_keys", Name: "api key lifecycle", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return st
}

func TestAPIKeyLifecycleMintFindTouchDelete(t *testing.T) {
	st := newAPIKeyTestStore(t)
	ctx := context.Background()

	key := &APIKey{
		KeyID: "key_life_1", ProjectID: "proj_keys",
		KeyHash: "hash_life_1", KeyPrefix: "mesedi_sk_abc", Name: "lifecycle",
	}
	if err := st.CreateAPIKey(ctx, key); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Found by hash, the auth middleware's exact call, and the hash
	// itself never leaks in the answer's serialized form (the field
	// is json:"-"; what matters here is the row round-trips).
	got, err := st.GetAPIKeyByHash(ctx, "hash_life_1")
	if err != nil {
		t.Fatalf("get by hash: %v", err)
	}
	if got.KeyID != "key_life_1" || got.ProjectID != "proj_keys" {
		t.Fatalf("got %+v, want the minted key", got)
	}
	if got.Scope != APIKeyScopeCustomer {
		t.Fatalf("scope defaulted to %q, want %q", got.Scope, APIKeyScopeCustomer)
	}
	if got.LastUsedAt != nil {
		t.Fatal("a never-used key must have no last_used_at")
	}

	// Unknown hash must be ErrNotFound, not a nil row: the auth
	// middleware branches on exactly this error.
	if _, err := st.GetAPIKeyByHash(ctx, "hash_never_minted"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown hash error = %v, want ErrNotFound", err)
	}

	// Touch stamps last_used_at; the fire-and-forget path in auth
	// depends on this being non-fatal and visible on next read.
	if err := st.TouchAPIKey(ctx, "key_life_1"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	touched, err := st.GetAPIKeyByHash(ctx, "hash_life_1")
	if err != nil {
		t.Fatalf("get after touch: %v", err)
	}
	if touched.LastUsedAt == nil {
		t.Fatal("touch did not stamp last_used_at")
	}

	// The admin inventory lists it.
	all, err := st.ListAllAPIKeys(ctx)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	found := false
	for _, k := range all {
		if k.KeyID == "key_life_1" {
			found = true
		}
	}
	if !found {
		t.Fatal("minted key missing from ListAllAPIKeys")
	}

	// Deleted by id, then the hash lookup must refuse: revocation is
	// only real if the auth path stops honoring the credential.
	if err := st.DeleteAPIKeyByID(ctx, "key_life_1"); err != nil {
		t.Fatalf("delete by id: %v", err)
	}
	if _, err := st.GetAPIKeyByHash(ctx, "hash_life_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted key lookup error = %v, want ErrNotFound", err)
	}
}
