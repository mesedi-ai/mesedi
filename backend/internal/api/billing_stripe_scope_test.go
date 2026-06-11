// Stripe scope integration tests.
//
// PURPOSE
//
// These tests exercise every Stripe API operation the Mesedi backend makes
// against the live Stripe API in test mode. They exist to verify that a
// given API key (Secret key or Restricted key) has the exact scopes Mesedi
// needs and nothing it doesn't.
//
// Built specifically to gate the production sk_live_ → rk_live_ Restricted-
// key swap (task #200, #201): run with the candidate Restricted key in
// test mode FIRST, confirm all subtests pass, only then roll the
// production secret. If any subtest fails, the failure message tells us
// exactly which Stripe permission is missing on the Restricted key.
//
// COVERAGE
//
// The sweep in #201 cataloged every stripe-go SDK call in the backend.
// Each subtest below maps 1:1 to a call site:
//
//   Subtest                       │ Maps to call site in production code
//   ──────────────────────────────┼─────────────────────────────────────
//   CustomerCreate                │ billing.go:1194 customer.New
//   CustomerRead                  │ billing.go:545  customer.Get
//   CustomerUpdate                │ billing.go:1306 customer.Update
//   CheckoutSessionSubscription   │ billing.go:658  checkoutsession.New
//   CheckoutSessionSetup          │ billing.go:1251 checkoutsession.New
//   BillingPortalSession          │ billing.go:711  portalsession.New
//   InvoiceItem                   │ billing.go:1091 invoiceitem.New
//   PaymentIntent                 │ hobby_billing_scheduler.go:261 paymentintent.New
//   SubscriptionUpdate            │ billing.go:1415 subscription.Update
//   SubscriptionCancel            │ billing.go:1542 subscription.Cancel
//
// Webhook signature verification is local-only (no Stripe API call) so it
// needs no scope and no test here.
//
// ENVIRONMENT REQUIRED
//
//   STRIPE_TEST_KEY              sk_test_... or rk_test_... — the candidate
//                                key under test. The test passes ⇒ this
//                                key has all the scopes Mesedi needs.
//   STRIPE_TEST_TEAM_PRICE_ID    price_... — a test-mode price ID for the
//                                Cloud Team subscription tier. Create once
//                                in Stripe Dashboard test mode and reuse.
//   STRIPE_TEST_SUBSCRIPTION_ID  sub_... — a test-mode subscription to
//                                exercise Update / Cancel against. Create
//                                once via Stripe Dashboard (Customers →
//                                Add subscription on a test customer) and
//                                reuse. The test cancels it then leaves
//                                it cancelled; you'll need to re-create
//                                for the next run, or wire a setup helper
//                                that recreates it before SubscriptionUpdate.
//
// HOW TO RUN
//
//   # Baseline (production unrestricted key analog — should ALWAYS pass):
//   STRIPE_TEST_KEY=sk_test_... \
//   STRIPE_TEST_TEAM_PRICE_ID=price_... \
//   STRIPE_TEST_SUBSCRIPTION_ID=sub_... \
//   go test -tags=stripe_integration -v -run=TestStripeScopes \
//     ./internal/api/...
//
//   # Candidate restricted key (proves scopes are right):
//   STRIPE_TEST_KEY=rk_test_... \
//   ... (same other vars) ...
//   go test -tags=stripe_integration -v -run=TestStripeScopes \
//     ./internal/api/...
//
// The build tag `stripe_integration` keeps these tests out of the default
// `go test ./...` run (which would fail loudly without the env vars set).

//go:build stripe_integration
// +build stripe_integration

package api

import (
	"os"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"
	portalsession "github.com/stripe/stripe-go/v82/billingportal/session"
	checkoutsession "github.com/stripe/stripe-go/v82/checkout/session"
	"github.com/stripe/stripe-go/v82/customer"
	"github.com/stripe/stripe-go/v82/invoiceitem"
	"github.com/stripe/stripe-go/v82/paymentintent"
	"github.com/stripe/stripe-go/v82/subscription"
)

// loadEnv reads the required env vars and skips the test if any are
// missing. We skip rather than fail so accidental `go test` invocations
// don't blow up on machines without Stripe test creds set.
func loadEnv(t *testing.T) (key, teamPriceID, subID string) {
	t.Helper()
	key = os.Getenv("STRIPE_TEST_KEY")
	teamPriceID = os.Getenv("STRIPE_TEST_TEAM_PRICE_ID")
	subID = os.Getenv("STRIPE_TEST_SUBSCRIPTION_ID")
	if key == "" || teamPriceID == "" || subID == "" {
		t.Skip("set STRIPE_TEST_KEY + STRIPE_TEST_TEAM_PRICE_ID + STRIPE_TEST_SUBSCRIPTION_ID to run")
	}
	if key[:3] != "sk_" && key[:3] != "rk_" {
		t.Fatalf("STRIPE_TEST_KEY must begin with sk_ or rk_, got %q", key[:3])
	}
	return key, teamPriceID, subID
}

// runID is a unique-ish suffix appended to test resource metadata so a
// developer reading the Stripe test dashboard can tell which run created
// what. Not a security feature, just observability.
func runID() string {
	return time.Now().UTC().Format("20060102T150405")
}

// TestStripeScopes is the parent. Each subtest exercises one Mesedi
// production call site against the configured Stripe key. A passing
// subtest proves the key has the scope that call needs.
func TestStripeScopes(t *testing.T) {
	key, teamPriceID, existingSubID := loadEnv(t)
	stripe.Key = key

	// We need at least one customer to exercise customer.Get and to
	// hang other resources off. Create it once, reuse, clean up via
	// t.Cleanup so a failed early subtest doesn't leak Stripe test
	// objects.
	rid := runID()
	custParams := &stripe.CustomerParams{
		Email: stripe.String("scope-test-" + rid + "@mesedi.test"),
	}
	custParams.AddMetadata("created_by", "billing_stripe_scope_test")
	custParams.AddMetadata("run_id", rid)

	t.Run("CustomerCreate", func(t *testing.T) {
		_, err := customer.New(custParams)
		if err != nil {
			t.Fatalf("customer.New (Customers:Write): %v", err)
		}
	})

	cust, err := customer.New(&stripe.CustomerParams{
		Email: stripe.String("scope-test-reuse-" + rid + "@mesedi.test"),
	})
	if err != nil {
		t.Fatalf("setup: create reusable customer: %v", err)
	}
	t.Cleanup(func() {
		_, _ = customer.Del(cust.ID, nil)
	})

	t.Run("CustomerRead", func(t *testing.T) {
		got, err := customer.Get(cust.ID, nil)
		if err != nil {
			t.Fatalf("customer.Get (Customers:Read): %v", err)
		}
		if got.ID != cust.ID {
			t.Fatalf("customer.Get returned wrong ID: want %s got %s", cust.ID, got.ID)
		}
	})

	t.Run("CustomerUpdate", func(t *testing.T) {
		params := &stripe.CustomerParams{
			Description: stripe.String("updated by scope test " + rid),
		}
		_, err := customer.Update(cust.ID, params)
		if err != nil {
			t.Fatalf("customer.Update (Customers:Write): %v", err)
		}
	})

	t.Run("CheckoutSessionSubscription", func(t *testing.T) {
		params := &stripe.CheckoutSessionParams{
			Customer:           stripe.String(cust.ID),
			Mode:               stripe.String(string(stripe.CheckoutSessionModeSubscription)),
			SuccessURL:         stripe.String("https://app.mesedi.ai/app/billing?success=1"),
			CancelURL:          stripe.String("https://app.mesedi.ai/app/billing?cancelled=1"),
			LineItems: []*stripe.CheckoutSessionLineItemParams{
				{
					Price:    stripe.String(teamPriceID),
					Quantity: stripe.Int64(1),
				},
			},
		}
		_, err := checkoutsession.New(params)
		if err != nil {
			t.Fatalf("checkoutsession.New subscription mode (Checkout Sessions:Write): %v", err)
		}
	})

	t.Run("CheckoutSessionSetup", func(t *testing.T) {
		params := &stripe.CheckoutSessionParams{
			Customer:           stripe.String(cust.ID),
			Mode:               stripe.String(string(stripe.CheckoutSessionModeSetup)),
			SuccessURL:         stripe.String("https://app.mesedi.ai/app/billing?setup=success"),
			CancelURL:          stripe.String("https://app.mesedi.ai/app/billing?setup=cancelled"),
			PaymentMethodTypes: stripe.StringSlice([]string{"card"}),
		}
		_, err := checkoutsession.New(params)
		if err != nil {
			t.Fatalf("checkoutsession.New setup mode (Checkout Sessions:Write): %v", err)
		}
	})

	t.Run("BillingPortalSession", func(t *testing.T) {
		params := &stripe.BillingPortalSessionParams{
			Customer:  stripe.String(cust.ID),
			ReturnURL: stripe.String("https://app.mesedi.ai/app/billing"),
		}
		_, err := portalsession.New(params)
		if err != nil {
			t.Fatalf("portalsession.New (Customer Portal:Write): %v", err)
		}
	})

	t.Run("InvoiceItem", func(t *testing.T) {
		params := &stripe.InvoiceItemParams{
			Customer:    stripe.String(cust.ID),
			Amount:      stripe.Int64(100), // $1.00, dummy amount
			Currency:    stripe.String(string(stripe.CurrencyUSD)),
			Description: stripe.String("scope-test invoice item " + rid),
		}
		ii, err := invoiceitem.New(params)
		if err != nil {
			t.Fatalf("invoiceitem.New (Invoice Items:Write): %v", err)
		}
		t.Cleanup(func() {
			_, _ = invoiceitem.Del(ii.ID, nil)
		})
	})

	t.Run("PaymentIntent", func(t *testing.T) {
		// We don't confirm or capture, just create — that's all the
		// scope check needs. Mesedi's hobby_billing_scheduler creates
		// then lets Stripe webhooks drive the rest of the lifecycle.
		params := &stripe.PaymentIntentParams{
			Customer: stripe.String(cust.ID),
			Amount:   stripe.Int64(100),
			Currency: stripe.String(string(stripe.CurrencyUSD)),
		}
		pi, err := paymentintent.New(params)
		if err != nil {
			t.Fatalf("paymentintent.New (PaymentIntents:Write): %v", err)
		}
		t.Cleanup(func() {
			// PaymentIntents can't be deleted, only canceled while
			// still in a cancellable state. Ignore cleanup error if
			// already past that state.
			_, _ = paymentintent.Cancel(pi.ID, nil)
		})
	})

	// Subscription tests use the pre-existing test-mode subscription
	// supplied via STRIPE_TEST_SUBSCRIPTION_ID. Run them last because
	// SubscriptionCancel terminates the subscription, which would
	// break SubscriptionUpdate if it ran after.
	t.Run("SubscriptionUpdate", func(t *testing.T) {
		params := &stripe.SubscriptionParams{
			CancelAtPeriodEnd: stripe.Bool(true),
		}
		_, err := subscription.Update(existingSubID, params)
		if err != nil {
			t.Fatalf("subscription.Update cancel_at_period_end (Subscriptions:Write): %v", err)
		}
		// Reset so subsequent reads see CancelAtPeriodEnd: false again,
		// keeping the test sub reusable for the next run UNTIL the
		// Cancel subtest below.
		reset := &stripe.SubscriptionParams{
			CancelAtPeriodEnd: stripe.Bool(false),
		}
		if _, rerr := subscription.Update(existingSubID, reset); rerr != nil {
			t.Logf("warn: could not reset CancelAtPeriodEnd: %v", rerr)
		}
	})

	t.Run("SubscriptionCancel", func(t *testing.T) {
		// This is destructive: once cancelled, the test subscription
		// is gone. After a successful run, recreate via Stripe
		// Dashboard test mode for the next iteration. Mesedi's
		// close-account flow calls subscription.Cancel exactly this
		// way (no params, immediate cancellation).
		_, err := subscription.Cancel(existingSubID, nil)
		if err != nil {
			t.Fatalf("subscription.Cancel (Subscriptions:Write): %v", err)
		}
	})
}
