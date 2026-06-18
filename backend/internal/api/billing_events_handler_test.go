// Unit tests for handleInvoicePaymentFailed and
// handleChargeDisputeCreated (#261).
//
// Why these tests exist:
//
//   - These handlers back the Security page's commitment that
//     charge.dispute.created + invoice.payment_failed Stripe webhooks
//     "feed into our admin dashboard." If the handler silently drops a
//     well-formed event, the admin dashboard surfaces nothing and ops
//     finds out about the dispute / dunning case days later by
//     opening Stripe Dashboard directly.
//
//   - The dispute handler does a real stripe.Charge.Get round-trip
//     to resolve customer from the charge ID; that path is covered
//     here via a httptest-backed Stripe API mock so a regression in
//     the lookup logic surfaces in CI rather than in production.
//
// Strategy: stub the Store interface (same pattern as
// billing_ai_analyses_usage_test.go) plus a httptest server standing
// in for api.stripe.com. stripe-go v82's SetBackend lets us point
// the SDK at our test server for the duration of the test, then
// restore the default.
package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stripe/stripe-go/v82"

	"mesedi/backend/internal/store"
)

// ── Stub Store ────────────────────────────────────────────────────

// stubBillingEventStore implements just enough of store.Store to
// drive handleInvoicePaymentFailed and handleChargeDisputeCreated.
// Captures the created BillingEvent so assertions can inspect it.
type stubBillingEventStore struct {
	store.Store

	// GetProjectByStripeCustomerID controls.
	projectByCustomer    *store.Project
	projectByCustomerErr error
	lastCustomerLookup   string

	// CreateBillingEvent controls.
	createErr     error
	capturedEvent *store.BillingEvent
}

func (s *stubBillingEventStore) GetProjectByStripeCustomerID(
	_ context.Context, customerID string,
) (*store.Project, error) {
	s.lastCustomerLookup = customerID
	return s.projectByCustomer, s.projectByCustomerErr
}

func (s *stubBillingEventStore) CreateBillingEvent(_ context.Context, e *store.BillingEvent) error {
	s.capturedEvent = e
	return s.createErr
}

// silentLogger returns a slog.Logger that discards all output so
// test runs don't pollute go test verbose output.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ── invoice.payment_failed ────────────────────────────────────────

// TestHandleInvoicePaymentFailed_HappyPath builds a realistic invoice
// JSON, runs it through the handler, and verifies the captured
// BillingEvent has the right shape: kind, severity, customer
// attribution, amount, and detail JSON containing the invoice fields
// the admin page renders.
func TestHandleInvoicePaymentFailed_HappyPath(t *testing.T) {
	t.Parallel()

	st := &stubBillingEventStore{
		projectByCustomer: &store.Project{
			ProjectID: "proj_team",
			Tier:      TierTeam,
		},
	}
	h := &Handlers{Store: st}

	invoiceJSON := []byte(`{
		"id": "in_test123",
		"customer": {"id": "cus_test_team"},
		"currency": "usd",
		"amount_due": 4900,
		"amount_remaining": 4900,
		"attempt_count": 2,
		"next_payment_attempt": 1750291200,
		"billing_reason": "subscription_cycle",
		"collection_method": "charge_automatically",
		"hosted_invoice_url": "https://example.com/i/test",
		"invoice_pdf": "https://example.com/i/test.pdf"
	}`)
	event := stripe.Event{
		ID:   "evt_pf_001",
		Type: "invoice.payment_failed",
	}
	event.Data = &stripe.EventData{Raw: invoiceJSON}

	if err := h.handleInvoicePaymentFailed(event, silentLogger()); err != nil {
		t.Fatalf("handleInvoicePaymentFailed: %v", err)
	}
	if st.capturedEvent == nil {
		t.Fatal("expected a BillingEvent to be created; got nil")
	}
	got := st.capturedEvent
	if got.EventID != "evt_pf_001" {
		t.Errorf("EventID: got %q want %q", got.EventID, "evt_pf_001")
	}
	if got.ProjectID != "proj_team" {
		t.Errorf("ProjectID: got %q want %q", got.ProjectID, "proj_team")
	}
	if got.StripeCustomerID != "cus_test_team" {
		t.Errorf("StripeCustomerID: got %q want %q", got.StripeCustomerID, "cus_test_team")
	}
	if got.Kind != store.BillingEventKindStripePaymentFailed {
		t.Errorf("Kind: got %q want %q", got.Kind, store.BillingEventKindStripePaymentFailed)
	}
	if got.Severity != store.BillingEventSeverityMedium {
		t.Errorf("Severity: got %q want %q (payment_failed is dunning, medium)",
			got.Severity, store.BillingEventSeverityMedium)
	}
	if got.StripeObjectID != "in_test123" {
		t.Errorf("StripeObjectID: got %q want %q", got.StripeObjectID, "in_test123")
	}
	if got.AmountCents != 4900 {
		t.Errorf("AmountCents: got %d want 4900", got.AmountCents)
	}
	if got.Currency != "usd" {
		t.Errorf("Currency: got %q want %q (must be lowercase)", got.Currency, "usd")
	}
	// Spot-check the detail JSON contains the fields admin renders.
	var detail map[string]any
	if err := json.Unmarshal([]byte(got.DetailJSON), &detail); err != nil {
		t.Fatalf("DetailJSON not valid JSON: %v (raw=%s)", err, got.DetailJSON)
	}
	for _, k := range []string{"invoice_id", "attempt_count", "amount_due", "billing_reason"} {
		if _, ok := detail[k]; !ok {
			t.Errorf("DetailJSON missing key %q (raw=%s)", k, got.DetailJSON)
		}
	}
}

// TestHandleInvoicePaymentFailed_NoCustomerDrops covers the
// non-subscription-invoice path: invoice has no customer attached
// (one-off Payment Intent etc.), nothing to attribute. Handler must
// log and return nil rather than fail the webhook.
func TestHandleInvoicePaymentFailed_NoCustomerDrops(t *testing.T) {
	t.Parallel()
	st := &stubBillingEventStore{}
	h := &Handlers{Store: st}
	event := stripe.Event{ID: "evt_pf_002", Type: "invoice.payment_failed"}
	event.Data = &stripe.EventData{Raw: []byte(`{"id": "in_test", "currency": "usd"}`)}

	if err := h.handleInvoicePaymentFailed(event, silentLogger()); err != nil {
		t.Fatalf("expected nil err on missing customer; got %v", err)
	}
	if st.capturedEvent != nil {
		t.Fatalf("expected no BillingEvent created; got %+v", st.capturedEvent)
	}
}

// TestHandleInvoicePaymentFailed_UnknownCustomerDrops covers the
// purged-project case: customer present on the invoice but no Mesedi
// project resolves (project hard-deleted via GDPR purge while the
// invoice was in flight). Handler must log and return nil so Stripe
// stops redelivering an event that will never succeed.
func TestHandleInvoicePaymentFailed_UnknownCustomerDrops(t *testing.T) {
	t.Parallel()
	st := &stubBillingEventStore{
		projectByCustomerErr: store.ErrNotFound,
	}
	h := &Handlers{Store: st}
	event := stripe.Event{ID: "evt_pf_003", Type: "invoice.payment_failed"}
	event.Data = &stripe.EventData{Raw: []byte(`{
		"id": "in_orphan",
		"customer": {"id": "cus_orphan"},
		"currency": "usd"
	}`)}

	if err := h.handleInvoicePaymentFailed(event, silentLogger()); err != nil {
		t.Fatalf("expected nil err on missing project; got %v", err)
	}
	if st.capturedEvent != nil {
		t.Fatalf("expected no BillingEvent created; got %+v", st.capturedEvent)
	}
}

// ── charge.dispute.created ────────────────────────────────────────

// stripeBackendOverride points stripe-go's API backend at a httptest
// server for the duration of the test, then restores the default in
// the t.Cleanup. Returned URL is the test server's address so the
// caller can also inspect /v1/charges/{id} request shape if desired.
func stripeBackendOverride(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	stripe.Key = "sk_test_unit"
	backend := stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL: stripe.String(srv.URL),
	})
	stripe.SetBackend(stripe.APIBackend, backend)
	t.Cleanup(func() { stripe.SetBackend(stripe.APIBackend, nil) })
	return srv
}

// TestHandleChargeDisputeCreated_HappyPath_FraudulentSeverityHigh
// exercises the full path: dispute webhook arrives, we GET the
// charge from Stripe (mocked), resolve customer, look up project,
// and write a BillingEvent. A "fraudulent" reason MUST land severity
// high so /admin/billing-events surfaces it as urgent.
func TestHandleChargeDisputeCreated_HappyPath_FraudulentSeverityHigh(t *testing.T) {
	// NOT t.Parallel: this test mutates the global stripe.Key and
	// stripe.Backends via stripeBackendOverride. Parallel runs would
	// race each other.
	st := &stubBillingEventStore{
		projectByCustomer: &store.Project{
			ProjectID: "proj_disputed",
			Tier:      TierTeam,
		},
	}
	h := &Handlers{Store: st}

	// Mock Stripe API: any GET /v1/charges/{id} returns a charge
	// whose customer.id is cus_disputed_team.
	stripeBackendOverride(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/v1/charges/") {
			http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "ch_disputed",
			"customer": {"id": "cus_disputed_team"},
			"amount": 4900,
			"currency": "usd"
		}`))
	}))

	disputeJSON := []byte(`{
		"id": "dp_test_001",
		"charge": {"id": "ch_disputed"},
		"amount": 4900,
		"currency": "usd",
		"reason": "fraudulent",
		"status": "warning_needs_response",
		"is_charge_refundable": true,
		"network_reason_code": "10.4",
		"evidence_details": {
			"due_by": 1750291200,
			"has_evidence": false,
			"submission_count": 0
		}
	}`)
	event := stripe.Event{ID: "evt_dp_001", Type: "charge.dispute.created"}
	event.Data = &stripe.EventData{Raw: disputeJSON}

	if err := h.handleChargeDisputeCreated(event, silentLogger()); err != nil {
		t.Fatalf("handleChargeDisputeCreated: %v", err)
	}
	if st.capturedEvent == nil {
		t.Fatal("expected a BillingEvent to be created; got nil")
	}
	got := st.capturedEvent
	if got.EventID != "evt_dp_001" {
		t.Errorf("EventID: got %q want %q", got.EventID, "evt_dp_001")
	}
	if got.Kind != store.BillingEventKindStripeDispute {
		t.Errorf("Kind: got %q want %q", got.Kind, store.BillingEventKindStripeDispute)
	}
	if got.Severity != store.BillingEventSeverityHigh {
		t.Errorf("Severity: got %q want %q (fraudulent must be HIGH)",
			got.Severity, store.BillingEventSeverityHigh)
	}
	if got.StripeObjectID != "dp_test_001" {
		t.Errorf("StripeObjectID: got %q want %q", got.StripeObjectID, "dp_test_001")
	}
	if got.StripeCustomerID != "cus_disputed_team" {
		t.Errorf("StripeCustomerID: got %q want %q (resolved via charge.Get)",
			got.StripeCustomerID, "cus_disputed_team")
	}
	if got.AmountCents != 4900 {
		t.Errorf("AmountCents: got %d want 4900", got.AmountCents)
	}
	if got.Currency != "usd" {
		t.Errorf("Currency: got %q want %q (must be lowercase)", got.Currency, "usd")
	}
}

// TestHandleChargeDisputeCreated_NonFraudReasonSeverityMedium proves
// the severity discrimination logic: a "duplicate" or
// "subscription_canceled" reason is a customer-service dispute, not
// a fraud signal, so we tag it medium and don't crowd the high-
// priority view.
func TestHandleChargeDisputeCreated_NonFraudReasonSeverityMedium(t *testing.T) {
	st := &stubBillingEventStore{
		projectByCustomer: &store.Project{ProjectID: "proj_norm", Tier: TierTeam},
	}
	h := &Handlers{Store: st}
	stripeBackendOverride(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "ch_norm",
			"customer": {"id": "cus_norm"},
			"amount": 100,
			"currency": "usd"
		}`))
	}))
	disputeJSON := []byte(`{
		"id": "dp_test_002",
		"charge": {"id": "ch_norm"},
		"amount": 100,
		"currency": "usd",
		"reason": "subscription_canceled",
		"status": "warning_needs_response"
	}`)
	event := stripe.Event{ID: "evt_dp_002", Type: "charge.dispute.created"}
	event.Data = &stripe.EventData{Raw: disputeJSON}

	if err := h.handleChargeDisputeCreated(event, silentLogger()); err != nil {
		t.Fatalf("handleChargeDisputeCreated: %v", err)
	}
	if st.capturedEvent == nil {
		t.Fatal("expected BillingEvent created; got nil")
	}
	if st.capturedEvent.Severity != store.BillingEventSeverityMedium {
		t.Errorf("Severity: got %q want %q (non-fraud dispute is medium)",
			st.capturedEvent.Severity, store.BillingEventSeverityMedium)
	}
}

// TestHandleChargeDisputeCreated_ChargeWithoutCustomerDrops covers
// the guest-checkout case: the charge resolves but has no customer
// attached. Handler must log and return nil without creating a
// BillingEvent so Stripe stops redelivering.
func TestHandleChargeDisputeCreated_ChargeWithoutCustomerDrops(t *testing.T) {
	st := &stubBillingEventStore{}
	h := &Handlers{Store: st}
	stripeBackendOverride(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "ch_guest",
			"amount": 100,
			"currency": "usd"
		}`))
	}))
	event := stripe.Event{ID: "evt_dp_003", Type: "charge.dispute.created"}
	event.Data = &stripe.EventData{Raw: []byte(`{
		"id": "dp_test_003",
		"charge": {"id": "ch_guest"},
		"amount": 100,
		"currency": "usd",
		"reason": "general"
	}`)}

	if err := h.handleChargeDisputeCreated(event, silentLogger()); err != nil {
		t.Fatalf("expected nil err on customerless charge; got %v", err)
	}
	if st.capturedEvent != nil {
		t.Fatalf("expected no BillingEvent created; got %+v", st.capturedEvent)
	}
}
