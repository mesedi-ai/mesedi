package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

func newToolPinStore(t *testing.T) *SQLiteStore {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestToolContractPins_RoundTrip(t *testing.T) {
	st := newToolPinStore(t)
	ctx := context.Background()
	const proj = "proj_pins"

	// Empty project: empty map and empty list, no errors.
	pins, err := st.GetToolContractPins(ctx, proj, "crm_lookup")
	if err != nil || len(pins) != 0 {
		t.Fatalf("get on empty = (%v, %v), want empty map", pins, err)
	}

	if err := st.UpsertToolContractPin(ctx, proj, "crm_lookup", "definition", "hash_v1"); err != nil {
		t.Fatalf("upsert definition: %v", err)
	}
	if err := st.UpsertToolContractPin(ctx, proj, "crm_lookup", "description", "hash_d1"); err != nil {
		t.Fatalf("upsert description: %v", err)
	}
	// Upsert replaces in place: same triple, new hash.
	if err := st.UpsertToolContractPin(ctx, proj, "crm_lookup", "definition", "hash_v2"); err != nil {
		t.Fatalf("re-upsert definition: %v", err)
	}

	pins, err = st.GetToolContractPins(ctx, proj, "crm_lookup")
	if err != nil {
		t.Fatalf("get pins: %v", err)
	}
	if pins["definition"] != "hash_v2" || pins["description"] != "hash_d1" || len(pins) != 2 {
		t.Errorf("pins = %v, want definition hash_v2 and description hash_d1", pins)
	}

	rows, err := st.ListToolContractPins(ctx, proj)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list = %d rows (%v), want 2", len(rows), err)
	}
	if rows[0].Kind != "definition" || rows[0].UpdatedAt == "" || rows[0].CreatedAt == "" {
		t.Errorf("first row = %+v, want ordered kinds with timestamps set", rows[0])
	}
}

func TestToolContractPins_ProjectIsolationAndDelete(t *testing.T) {
	st := newToolPinStore(t)
	ctx := context.Background()

	if err := st.UpsertToolContractPin(ctx, "proj_a", "tool", "definition", "hash_a"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Another project sees nothing, and deleting its absent pin is
	// ErrNotFound, indistinguishable from the pin never existing.
	pins, err := st.GetToolContractPins(ctx, "proj_b", "tool")
	if err != nil || len(pins) != 0 {
		t.Errorf("cross-project get = (%v, %v), want empty", pins, err)
	}
	if err := st.DeleteToolContractPin(ctx, "proj_b", "tool", "definition"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-project delete = %v, want ErrNotFound", err)
	}

	// The owner's delete removes it, and a second delete is ErrNotFound.
	if err := st.DeleteToolContractPin(ctx, "proj_a", "tool", "definition"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.DeleteToolContractPin(ctx, "proj_a", "tool", "definition"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
}
