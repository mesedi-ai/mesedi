// Tests for BillingEvent store methods (#261).
//
// Why these tests exist:
//
//   - billing_events is the persistence layer behind the Security
//     page's commitment that charge.dispute.created and
//     invoice.payment_failed Stripe webhooks "feed into our admin
//     dashboard." If create/list/get/resolve diverge from the
//     documented contracts, the /admin/billing-events page will
//     either drop signals on the floor or surface them with the
//     wrong attribution.
//
//   - The Stripe event_id is the natural primary key on this table;
//     idempotency is the entire "Stripe redelivered the webhook"
//     defense, so we test the no-op redelivery path explicitly.
//
// Strategy: minimal in-memory schema (projects + billing_events
// only) rather than the full migration sequence. Same approach
// audit_events_purge_test.go takes — keeps tests scoped to the
// methods under test and avoids #233-style cross-migration coupling.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openMinimalBillingEventStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "test.db") +
		"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	// Minimal schema. billing_events column shapes mirror
	// migrations/034_billing_events.sql exactly.
	stmts := []string{
		`CREATE TABLE projects (
			project_id    TEXT PRIMARY KEY,
			name          TEXT NOT NULL,
			created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE billing_events (
			event_id           TEXT PRIMARY KEY,
			project_id         TEXT NOT NULL REFERENCES projects(project_id) ON DELETE CASCADE,
			stripe_customer_id TEXT NOT NULL,
			kind               TEXT NOT NULL,
			severity           TEXT NOT NULL,
			stripe_object_id   TEXT NOT NULL,
			amount_cents       INTEGER NOT NULL DEFAULT 0,
			currency           TEXT NOT NULL DEFAULT '',
			detail_json        TEXT,
			received_at        TIMESTAMP NOT NULL,
			resolved_at        TIMESTAMP,
			resolved_by        TEXT,
			resolution_note    TEXT
		)`,
		`CREATE INDEX idx_billing_events_project_received
			ON billing_events (project_id, received_at DESC)`,
		`CREATE INDEX idx_billing_events_unresolved
			ON billing_events (received_at)
			WHERE resolved_at IS NULL`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	// Seed two projects so tests can use BOTH and the per-project
	// filter has something to discriminate on.
	for _, pid := range []string{"proj_a", "proj_b"} {
		if _, err := db.Exec(
			`INSERT INTO projects (project_id, name) VALUES (?, ?)`,
			pid, pid,
		); err != nil {
			t.Fatalf("seed project %s: %v", pid, err)
		}
	}
	return &SQLiteStore{db: db}
}

func sampleBillingEvent(eventID, projectID, kind string) *BillingEvent {
	detail, _ := json.Marshal(map[string]any{
		"invoice_id":    "in_test123",
		"attempt_count": 2,
	})
	return &BillingEvent{
		EventID:          eventID,
		ProjectID:        projectID,
		StripeCustomerID: "cus_" + projectID,
		Kind:             kind,
		Severity:         BillingEventSeverityMedium,
		StripeObjectID:   "in_test123",
		AmountCents:      4900,
		Currency:         "usd",
		DetailJSON:       string(detail),
		ReceivedAt:       time.Now().UTC(),
	}
}

// TestCreateBillingEvent_HappyPath proves that a well-formed event
// is persisted and that GetBillingEvent returns it with every field
// round-tripped intact.
func TestCreateBillingEvent_HappyPath(t *testing.T) {
	t.Parallel()
	s := openMinimalBillingEventStore(t)
	ctx := context.Background()
	be := sampleBillingEvent("evt_001", "proj_a", BillingEventKindStripePaymentFailed)

	if err := s.CreateBillingEvent(ctx, be); err != nil {
		t.Fatalf("CreateBillingEvent: %v", err)
	}
	got, err := s.GetBillingEvent(ctx, "evt_001")
	if err != nil {
		t.Fatalf("GetBillingEvent: %v", err)
	}
	if got.EventID != be.EventID || got.ProjectID != be.ProjectID {
		t.Errorf("id mismatch: got %s/%s want %s/%s",
			got.EventID, got.ProjectID, be.EventID, be.ProjectID)
	}
	if got.Kind != be.Kind || got.Severity != be.Severity {
		t.Errorf("kind/severity mismatch: got %s/%s want %s/%s",
			got.Kind, got.Severity, be.Kind, be.Severity)
	}
	if got.AmountCents != 4900 {
		t.Errorf("AmountCents: got %d want 4900", got.AmountCents)
	}
	if got.Currency != "usd" {
		t.Errorf("Currency: got %q want %q", got.Currency, "usd")
	}
	if got.DetailJSON != be.DetailJSON {
		t.Errorf("DetailJSON mismatch")
	}
	if got.ResolvedAt != nil {
		t.Errorf("ResolvedAt should be nil on fresh event, got %v", *got.ResolvedAt)
	}
}

// TestCreateBillingEvent_Idempotent proves Stripe's redelivery
// behavior is absorbed. Second INSERT with same event_id MUST be a
// no-op, not a primary-key error.
func TestCreateBillingEvent_Idempotent(t *testing.T) {
	t.Parallel()
	s := openMinimalBillingEventStore(t)
	ctx := context.Background()
	be := sampleBillingEvent("evt_dup", "proj_a", BillingEventKindStripeDispute)
	be.Severity = BillingEventSeverityHigh

	if err := s.CreateBillingEvent(ctx, be); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// Mutate the in-memory copy to prove the second insert is silently
	// dropped rather than overwriting (Stripe redelivery semantics:
	// existing row wins).
	dup := *be
	dup.Severity = BillingEventSeverityLow
	dup.AmountCents = 1
	if err := s.CreateBillingEvent(ctx, &dup); err != nil {
		t.Fatalf("redelivery should be silent no-op, got: %v", err)
	}
	got, err := s.GetBillingEvent(ctx, "evt_dup")
	if err != nil {
		t.Fatalf("GetBillingEvent after redelivery: %v", err)
	}
	if got.Severity != BillingEventSeverityHigh || got.AmountCents != 4900 {
		t.Errorf("redelivered insert must NOT overwrite; got severity=%s amount=%d",
			got.Severity, got.AmountCents)
	}
}

// TestCreateBillingEvent_RequiredFields rejects empty event_id,
// project_id, kind, or severity. Catches handler bugs that would
// otherwise produce orphan rows.
func TestCreateBillingEvent_RequiredFields(t *testing.T) {
	t.Parallel()
	s := openMinimalBillingEventStore(t)
	ctx := context.Background()
	cases := []struct {
		name string
		mut  func(*BillingEvent)
	}{
		{"missing_event_id", func(b *BillingEvent) { b.EventID = "" }},
		{"missing_project_id", func(b *BillingEvent) { b.ProjectID = "" }},
		{"missing_kind", func(b *BillingEvent) { b.Kind = "" }},
		{"missing_severity", func(b *BillingEvent) { b.Severity = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			be := sampleBillingEvent("evt_x_"+tc.name, "proj_a", BillingEventKindStripeDispute)
			tc.mut(be)
			if err := s.CreateBillingEvent(ctx, be); err == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

// TestGetBillingEvent_NotFound returns ErrNotFound, not a wrapped
// sql.ErrNoRows. Admin endpoint relies on this to return HTTP 404.
func TestGetBillingEvent_NotFound(t *testing.T) {
	t.Parallel()
	s := openMinimalBillingEventStore(t)
	_, err := s.GetBillingEvent(context.Background(), "evt_missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// TestListBillingEvents_Filters exercises every combination of the
// BillingEventFilter discriminators against a fixed dataset.
func TestListBillingEvents_Filters(t *testing.T) {
	t.Parallel()
	s := openMinimalBillingEventStore(t)
	ctx := context.Background()

	seed := []*BillingEvent{
		// Two unresolved events on proj_a, one resolved on proj_a,
		// one unresolved on proj_b.
		sampleBillingEvent("evt_a1", "proj_a", BillingEventKindStripePaymentFailed),
		sampleBillingEvent("evt_a2", "proj_a", BillingEventKindStripeDispute),
		sampleBillingEvent("evt_a3", "proj_a", BillingEventKindStripePaymentFailed),
		sampleBillingEvent("evt_b1", "proj_b", BillingEventKindStripeDispute),
	}
	// Stagger ReceivedAt so the ORDER BY received_at DESC is
	// deterministic. Newest at the end of the seed slice.
	base := time.Now().UTC().Add(-time.Hour)
	for i, be := range seed {
		be.ReceivedAt = base.Add(time.Duration(i) * time.Minute)
		if err := s.CreateBillingEvent(ctx, be); err != nil {
			t.Fatalf("seed %s: %v", be.EventID, err)
		}
	}
	// Mark evt_a3 resolved so UnresolvedOnly excludes it.
	if err := s.ResolveBillingEvent(ctx, "evt_a3", "ops@mesedi.ai", "test_resolution"); err != nil {
		t.Fatalf("resolve evt_a3: %v", err)
	}

	tests := []struct {
		name    string
		filter  BillingEventFilter
		wantIDs []string // order matters: newest first
	}{
		{
			"no_filter_returns_all_newest_first",
			BillingEventFilter{},
			[]string{"evt_b1", "evt_a3", "evt_a2", "evt_a1"},
		},
		{
			"by_project_a",
			BillingEventFilter{ProjectID: "proj_a"},
			[]string{"evt_a3", "evt_a2", "evt_a1"},
		},
		{
			"by_project_b",
			BillingEventFilter{ProjectID: "proj_b"},
			[]string{"evt_b1"},
		},
		{
			"unresolved_only",
			BillingEventFilter{UnresolvedOnly: true},
			[]string{"evt_b1", "evt_a2", "evt_a1"},
		},
		{
			"unresolved_and_by_project",
			BillingEventFilter{UnresolvedOnly: true, ProjectID: "proj_a"},
			[]string{"evt_a2", "evt_a1"},
		},
		{
			"limit_caps_result",
			BillingEventFilter{Limit: 2},
			[]string{"evt_b1", "evt_a3"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ListBillingEvents(ctx, tc.filter)
			if err != nil {
				t.Fatalf("ListBillingEvents: %v", err)
			}
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("len: got %d want %d (ids=%v)",
					len(got), len(tc.wantIDs), idsOf(got))
			}
			for i, want := range tc.wantIDs {
				if got[i].EventID != want {
					t.Errorf("pos %d: got %s want %s (full=%v)",
						i, got[i].EventID, want, idsOf(got))
				}
			}
		})
	}
}

// TestResolveBillingEvent_StampsAllFields proves the resolve path
// records both the actor, the note, AND a non-nil resolved_at.
// /admin/billing-events filters on resolved_at IS NULL; a partial
// stamp would leave the event stuck in the unresolved view forever.
func TestResolveBillingEvent_StampsAllFields(t *testing.T) {
	t.Parallel()
	s := openMinimalBillingEventStore(t)
	ctx := context.Background()
	be := sampleBillingEvent("evt_resolve", "proj_a", BillingEventKindStripeDispute)
	if err := s.CreateBillingEvent(ctx, be); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.ResolveBillingEvent(ctx, "evt_resolve", "robert@mesedi.ai", "refunded via Stripe"); err != nil {
		t.Fatalf("ResolveBillingEvent: %v", err)
	}
	got, err := s.GetBillingEvent(ctx, "evt_resolve")
	if err != nil {
		t.Fatalf("GetBillingEvent: %v", err)
	}
	if got.ResolvedAt == nil {
		t.Fatal("ResolvedAt: got nil, want non-nil after resolve")
	}
	if got.ResolvedBy != "robert@mesedi.ai" {
		t.Errorf("ResolvedBy: got %q want %q", got.ResolvedBy, "robert@mesedi.ai")
	}
	if got.ResolutionNote != "refunded via Stripe" {
		t.Errorf("ResolutionNote: got %q want %q", got.ResolutionNote, "refunded via Stripe")
	}
}

// TestResolveBillingEvent_MissingReturnsErrNotFound proves the admin
// endpoint can map a stale resolve POST to HTTP 404 without scanning
// the table first.
func TestResolveBillingEvent_MissingReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	s := openMinimalBillingEventStore(t)
	err := s.ResolveBillingEvent(context.Background(), "evt_ghost", "anyone", "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// idsOf is a tiny helper for clearer error messages on list tests.
func idsOf(es []*BillingEvent) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.EventID
	}
	return out
}
