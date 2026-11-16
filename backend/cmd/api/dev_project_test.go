package main

// Tests for the opt-in dev project (2026-11-16). The public dev key
// (plaintext in the README) must exist only where MESEDI_DEV_BOOTSTRAP=1
// asked for it, must work there, and must be removed everywhere else.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"mesedi/backend/internal/store"
)

func devTestStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	st, err := store.OpenSQLite(":memory:", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func quietDevLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func devKeyPresent(t *testing.T, st store.Store) bool {
	t.Helper()
	_, err := st.GetAPIKeyByHash(context.Background(), devKeyHash)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lookup: %v", err)
	}
	return err == nil
}

func TestReconcileDevProjectOptedInCreatesWorkingDevProject(t *testing.T) {
	ctx := context.Background()
	st := devTestStore(t)
	if err := reconcileDevProject(ctx, st, quietDevLogger(), true, false); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !devKeyPresent(t, st) {
		t.Fatal("opted in, but the dev key was not created")
	}
	p, err := st.GetProject(ctx, devProjectID)
	if err != nil || p.OwnerEmail != devOwnerEmail {
		t.Fatalf("dev project owner email = %q, err %v; want %q", p.OwnerEmail, err, devOwnerEmail)
	}
	verified, err := st.IsEmailVerified(ctx, p.OwnerEmail)
	if err != nil || !verified {
		t.Fatalf("dev owner email verified = %v, err %v; the quickstart would get email_not_verified", verified, err)
	}
	// Idempotent: a second start changes nothing and does not fail.
	if err := reconcileDevProject(ctx, st, quietDevLogger(), true, false); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
}

func TestReconcileDevProjectNotOptedInCreatesNothing(t *testing.T) {
	st := devTestStore(t)
	if err := reconcileDevProject(context.Background(), st, quietDevLogger(), false, false); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if devKeyPresent(t, st) {
		t.Fatal("not opted in, but the public dev key exists")
	}
}

func TestReconcileDevProjectRemovesExistingKeyWhenNotOptedIn(t *testing.T) {
	ctx := context.Background()
	st := devTestStore(t)
	if err := bootstrapDevProject(ctx, st, quietDevLogger()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := reconcileDevProject(ctx, st, quietDevLogger(), false, false); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if devKeyPresent(t, st) {
		t.Fatal("a server that did not opt in still holds the public dev key")
	}
}

func TestReconcileDevProjectIgnoresOptInOnFly(t *testing.T) {
	ctx := context.Background()
	st := devTestStore(t)
	if err := bootstrapDevProject(ctx, st, quietDevLogger()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := reconcileDevProject(ctx, st, quietDevLogger(), true, true); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if devKeyPresent(t, st) {
		t.Fatal("on Fly the public dev key must never exist, even with the switch set")
	}
}

func TestRemovePublicDevKeyFindsItUnderAnyKeyID(t *testing.T) {
	ctx := context.Background()
	st := devTestStore(t)
	if err := st.CreateProject(ctx, &store.Project{ProjectID: "proj-other", Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAPIKey(ctx, &store.APIKey{KeyID: "renamed", ProjectID: "proj-other",
		KeyHash: devKeyHash, KeyPrefix: "mesedi_sk_dev", Name: "copied"}); err != nil {
		t.Fatal(err)
	}
	if err := removePublicDevKey(ctx, st, quietDevLogger()); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if devKeyPresent(t, st) {
		t.Fatal("the public key survived under a different key_id")
	}
	if err := removePublicDevKey(ctx, st, quietDevLogger()); err != nil {
		t.Fatalf("second remove must be a no-op: %v", err)
	}
}

func TestBootstrapDevProjectLegacyProjectWithoutEmailIsLeftAlone(t *testing.T) {
	ctx := context.Background()
	st := devTestStore(t)
	if err := st.CreateProject(ctx, &store.Project{ProjectID: devProjectID, Name: "Local Dev"}); err != nil {
		t.Fatal(err)
	}
	if err := bootstrapDevProject(ctx, st, quietDevLogger()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if devKeyPresent(t, st) {
		t.Fatal("a legacy dev project with no owner email should be left alone, with a warning")
	}
}
