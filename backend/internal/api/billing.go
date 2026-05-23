// Stripe billing endpoints. This file implements the four backend
// surfaces #120 needs:
//
//   POST /billing/checkout   — auth-required. Creates a Stripe
//                              Checkout session for the calling
//                              project to upgrade Hobby → Pro;
//                              returns the hosted-Checkout URL.
//
//   POST /billing/portal     — auth-required. Creates a Stripe
//                              Customer Portal session so an
//                              already-paying project can update
//                              card, see invoices, or cancel.
//
//   GET  /billing            — auth-required. Returns the calling
//                              project's tier, current-period
//                              executions used, period bounds, and
//                              tier-defined limits. Drives the
//                              dashboard /app/billing page.
//
//   GET  /billing/usage      — auth-required. Returns daily
//                              execution counts for the last 30
//                              days. Drives the usage chart on the
//                              billing page.
//
//   POST /billing/webhook    — PUBLIC (no bearer). Receives Stripe
//                              events. Authenticity is verified via
//                              the Stripe-Signature header and the
//                              shared webhook secret. Dispatches
//                              checkout.session.completed,
//                              customer.subscription.updated,
//                              customer.subscription.deleted, and
//                              invoice.paid.
//
// Enforcement (Hobby silent-drop, Pro overage usage records) is
// deliberately not wired in this slice — the counter increments on
// every POST /executions but nothing gates on it yet. The follow-up
// enforcement slice adds those gates without changing the schema.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v82"
	portalsession "github.com/stripe/stripe-go/v82/billingportal/session"
	checkoutsession "github.com/stripe/stripe-go/v82/checkout/session"
	"github.com/stripe/stripe-go/v82/webhook"

	"mesedi/backend/internal/store"
)

// ── tier constants (mirror /pricing page) ───────────────────────

const (
	TierHobby      = "hobby"
	TierPro        = "pro"
	TierEnterprise = "enterprise"

	// HobbyExecutionLimit is the included monthly quota on Hobby.
	// Used by the dashboard's "X of N" display today; will be the
	// silent-drop threshold in the enforcement slice.
	HobbyExecutionLimit = 5000
	// ProExecutionIncluded is the included monthly quota on Pro.
	// Executions past this number bill at ProOveragePriceUSD each in
	// the enforcement slice.
	ProExecutionIncluded = 100000
	// ProOveragePriceUSD is the per-execution overage cost on Pro.
	// Surfaced in dashboard copy; used by the enforcement slice to
	// post Stripe metered-usage records.
	ProOveragePriceUSD = 0.001
)

// ── config ─────────────────────────────────────────────────────

// StripeConfig is the minimum set of Stripe identifiers and shared
// secrets the billing handlers need at runtime. Constructed from
// environment variables in main; passed into api.New so handler
// methods can access it via h.Stripe.
//
// SecretKey: the test/live secret API key. Begins with "sk_test_" or
// "sk_live_". Set via MESEDI_STRIPE_SECRET_KEY.
//
// WebhookSecret: the signing secret for the configured webhook
// endpoint. Begins with "whsec_". Set via MESEDI_STRIPE_WEBHOOK_SECRET.
//
// ProPriceID: the Stripe Price ID for the $29/mo Pro plan. Begins
// with "price_". Set via MESEDI_STRIPE_PRO_PRICE_ID.
//
// If any of the three is empty the billing endpoints respond with
// 503; this lets the backend run in local-dev without Stripe configured.
type StripeConfig struct {
	SecretKey     string
	WebhookSecret string
	ProPriceID    string
}

// Configured returns true iff all three required Stripe values are
// present. When false, billing endpoints return 503 with a clear
// message instead of crashing on missing config.
func (c StripeConfig) Configured() bool {
	return c.SecretKey != "" && c.WebhookSecret != "" && c.ProPriceID != ""
}

// applyKey sets the package-global stripe.Key once per request from
// the configured SecretKey. The Stripe Go SDK uses a global for the
// default backend; setting it on each call is cheap and safe (no
// hidden mutation across goroutines beyond the assignment itself,
// which is a same-value write after the first call).
func (c StripeConfig) applyKey() {
	stripe.Key = c.SecretKey
}

// ── helpers ────────────────────────────────────────────────────

// tierExecutionLimit returns the included monthly execution quota
// for a tier. Enterprise returns 0 meaning "no fixed limit"; the
// dashboard renders that as the unicode infinity sign.
func tierExecutionLimit(tier string) int64 {
	switch tier {
	case TierHobby:
		return HobbyExecutionLimit
	case TierPro:
		return ProExecutionIncluded
	case TierEnterprise:
		return 0
	default:
		return HobbyExecutionLimit
	}
}

// billingNotConfigured writes the standard 503 used when an
// endpoint is called before STRIPE_* env vars are set.
func billingNotConfigured(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable,
		"Stripe is not configured on this backend; billing endpoints are disabled")
}

// ── response payloads ──────────────────────────────────────────

type BillingStatusResponse struct {
	OK                       bool       `json:"ok"`
	ProjectID                string     `json:"project_id"`
	Tier                     string     `json:"tier"`
	ExecutionsThisPeriod     int64      `json:"executions_this_period"`
	IncludedExecutions       int64      `json:"included_executions"`
	OveragePricePerExecution float64    `json:"overage_price_per_execution_usd"`
	CurrentPeriodStart       *time.Time `json:"current_period_start,omitempty"`
	CurrentPeriodEnd         *time.Time `json:"current_period_end,omitempty"`
	StripeCustomerID         string     `json:"stripe_customer_id,omitempty"`
	StripeSubscriptionID     string     `json:"stripe_subscription_id,omitempty"`
	// CanUpgrade is true when this project can go from Hobby to Pro
	// via POST /billing/checkout. False if already on Pro or
	// Enterprise.
	CanUpgrade bool `json:"can_upgrade"`
	// CanManage is true when this project has an existing Stripe
	// customer / subscription it can manage via POST /billing/portal.
	CanManage bool `json:"can_manage"`
}

type CheckoutResponse struct {
	OK        bool   `json:"ok"`
	URL       string `json:"url"`
	SessionID string `json:"session_id"`
}

type PortalResponse struct {
	OK  bool   `json:"ok"`
	URL string `json:"url"`
}

type UsageResponse struct {
	OK     bool                        `json:"ok"`
	Days   []store.DailyExecutionCount `json:"days"`
	Since  time.Time                   `json:"since"`
	Until  time.Time                   `json:"until"`
}

// ── handlers ───────────────────────────────────────────────────

// HandleGetBilling returns the calling project's billing snapshot.
// Auth-required. Always returns 200 with the current tier even if
// Stripe is not configured (Hobby projects don't need Stripe).
func (h *Handlers) HandleGetBilling(w http.ResponseWriter, r *http.Request) {
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	p, err := h.Store.GetProject(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "load project: "+err.Error())
		return
	}

	resp := BillingStatusResponse{
		OK:                       true,
		ProjectID:                p.ProjectID,
		Tier:                     p.Tier,
		ExecutionsThisPeriod:     p.ExecutionsThisPeriod,
		IncludedExecutions:       tierExecutionLimit(p.Tier),
		OveragePricePerExecution: ProOveragePriceUSD,
		CurrentPeriodStart:       p.CurrentPeriodStart,
		CurrentPeriodEnd:         p.CurrentPeriodEnd,
		StripeCustomerID:         p.StripeCustomerID,
		StripeSubscriptionID:     p.StripeSubscriptionID,
		CanUpgrade:               p.Tier == TierHobby && h.Stripe.Configured(),
		CanManage:                p.StripeCustomerID != "" && h.Stripe.Configured(),
	}
	writeJSON(w, http.StatusOK, resp)
}

// HandleGetBillingUsage returns daily execution counts for the last
// 30 days, used by the usage chart on /app/billing. Days with zero
// executions are omitted server-side; the dashboard fills gaps
// client-side.
func (h *Handlers) HandleGetBillingUsage(w http.ResponseWriter, r *http.Request) {
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	until := time.Now().UTC()
	since := until.Add(-30 * 24 * time.Hour)
	days, err := h.Store.GetDailyExecutionCounts(r.Context(), projectID, since, until)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"load daily execution counts: "+err.Error())
		return
	}
	if days == nil {
		days = []store.DailyExecutionCount{}
	}
	writeJSON(w, http.StatusOK, UsageResponse{
		OK:    true,
		Days:  days,
		Since: since,
		Until: until,
	})
}

// HandleCreateCheckout creates a Stripe Checkout session for the
// calling project to upgrade Hobby → Pro. Returns the hosted-page
// URL the dashboard will window.location.assign to.
//
// Idempotency: clicking the upgrade button twice creates two
// sessions; that's fine — Stripe expires unused sessions after 24h.
// On the success URL Stripe redirects with ?session_id={CHECKOUT_SESSION_ID}
// so the dashboard can read the current /billing state immediately.
func (h *Handlers) HandleCreateCheckout(w http.ResponseWriter, r *http.Request) {
	if !h.Stripe.Configured() {
		billingNotConfigured(w)
		return
	}
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	p, err := h.Store.GetProject(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "load project: "+err.Error())
		return
	}
	if p.Tier != TierHobby {
		writeError(w, http.StatusBadRequest,
			"project is already on a paid tier; use /billing/portal to manage")
		return
	}

	h.Stripe.applyKey()
	dashboardBase := h.resolveDashboardBase(r)
	successURL := dashboardBase + "/app/billing?status=success&session_id={CHECKOUT_SESSION_ID}"
	cancelURL := dashboardBase + "/app/billing?status=canceled"

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    stripe.String(h.Stripe.ProPriceID),
				Quantity: stripe.Int64(1),
			},
		},
		SuccessURL: stripe.String(successURL),
		CancelURL:  stripe.String(cancelURL),
		// ClientReferenceID lets the webhook handler find our
		// project from the Checkout completion event without needing
		// a Stripe Customer lookup first.
		ClientReferenceID: stripe.String(projectID),
		Metadata: map[string]string{
			"mesedi_project_id": projectID,
		},
	}
	// Prefill email when available so the customer doesn't retype.
	if p.OwnerEmail != "" {
		params.CustomerEmail = stripe.String(p.OwnerEmail)
	}

	session, err := checkoutsession.New(params)
	if err != nil {
		h.Logger.Error("stripe checkout create failed",
			"project_id", projectID, "error", err.Error())
		writeError(w, http.StatusBadGateway,
			"create Stripe Checkout session: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, CheckoutResponse{
		OK:        true,
		URL:       session.URL,
		SessionID: session.ID,
	})
}

// HandleCreatePortal creates a Stripe Customer Portal session for
// the calling project, redirecting them to update payment method,
// view invoices, or cancel. Requires that the project has a Stripe
// customer id already (set after the first successful Checkout).
func (h *Handlers) HandleCreatePortal(w http.ResponseWriter, r *http.Request) {
	if !h.Stripe.Configured() {
		billingNotConfigured(w)
		return
	}
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	p, err := h.Store.GetProject(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "load project: "+err.Error())
		return
	}
	if p.StripeCustomerID == "" {
		writeError(w, http.StatusBadRequest,
			"project has no Stripe customer yet; upgrade first via /billing/checkout")
		return
	}

	h.Stripe.applyKey()
	returnURL := h.resolveDashboardBase(r) + "/app/billing"
	params := &stripe.BillingPortalSessionParams{
		Customer:  stripe.String(p.StripeCustomerID),
		ReturnURL: stripe.String(returnURL),
	}
	session, err := portalsession.New(params)
	if err != nil {
		h.Logger.Error("stripe portal create failed",
			"project_id", projectID, "error", err.Error())
		writeError(w, http.StatusBadGateway,
			"create Stripe Portal session: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, PortalResponse{
		OK:  true,
		URL: session.URL,
	})
}

// HandleStripeWebhook receives Stripe events. Public (no bearer);
// authenticity is verified via the Stripe-Signature header against
// the configured webhook secret. Returns 200 on every recognized
// event (even when we choose not to act) so Stripe stops retrying.
//
// The handler must read the raw request body for signature
// verification — make sure no upstream middleware consumes it
// before this runs. Today the chain is recover → log → router, and
// none of those touch the body.
func (h *Handlers) HandleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	if !h.Stripe.Configured() {
		// Best to fail quietly with a 503 here rather than 200; we
		// don't want Stripe to record success for events we won't
		// actually process. Stripe will retry while config is missing.
		billingNotConfigured(w)
		return
	}

	// Cap body size to prevent abuse — Stripe events are kilobytes,
	// not megabytes; 1 MB is comfortable headroom.
	const maxBody = 1 << 20
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}

	sig := r.Header.Get("Stripe-Signature")
	if sig == "" {
		writeError(w, http.StatusBadRequest, "missing Stripe-Signature header")
		return
	}

	event, err := webhook.ConstructEvent(body, sig, h.Stripe.WebhookSecret)
	if err != nil {
		// Invalid signature is the normal case for misconfigured
		// secrets or replay attempts; return 400 (not 401, which
		// Stripe treats as "endpoint dead").
		h.Logger.Warn("stripe webhook signature verify failed", "error", err.Error())
		writeError(w, http.StatusBadRequest, "signature verification failed")
		return
	}

	logger := h.Logger.With("stripe_event_id", event.ID, "stripe_event_type", string(event.Type))
	logger.Info("stripe webhook received")

	if err := h.dispatchStripeEvent(r.Context(), event, logger); err != nil { //nolint:contextcheck
		// Logged at the dispatch site; respond 200 anyway so Stripe
		// doesn't endlessly retry events we can't currently process
		// (e.g., transient DB outage will be re-fired by a future
		// related event; long retry storms aren't useful).
		// Stripe's "Failed delivery" UI surfaces non-2xx, so for now
		// log + 200 is the right trade-off. Re-evaluate if we
		// implement idempotency keys later.
		logger.Error("stripe webhook dispatch failed", "error", err.Error())
	}
	w.WriteHeader(http.StatusOK)
}

// dispatchStripeEvent routes one verified Stripe event to its
// per-type handler. Unknown event types are logged at info and
// silently acknowledged. The context passed in is the request
// context; per-handler database calls use context.Background()
// because Stripe should not see request cancellation as "event
// failed" — the response status is what controls Stripe's retry
// behavior, not whether the side effects finished.
func (h *Handlers) dispatchStripeEvent(
	ctx context.Context,
	event stripe.Event,
	logger *slog.Logger,
) error {
	_ = ctx // reserved for future per-event tracing; see comment above
	switch event.Type {
	case "checkout.session.completed":
		return h.handleCheckoutCompleted(event)
	case "customer.subscription.updated":
		return h.handleSubscriptionUpdated(event)
	case "customer.subscription.deleted":
		return h.handleSubscriptionDeleted(event)
	case "invoice.paid":
		return h.handleInvoicePaid(event)
	default:
		logger.Info("stripe event ignored (not handled)")
		return nil
	}
}

// handleCheckoutCompleted upgrades the project to Pro and records
// the Stripe customer + subscription identifiers. The
// ClientReferenceID we set on session creation gives us the project_id
// without an extra Stripe round-trip.
func (h *Handlers) handleCheckoutCompleted(event stripe.Event) error {
	var session stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &session); err != nil {
		return fmt.Errorf("unmarshal checkout.session: %w", err)
	}
	projectID := strings.TrimSpace(session.ClientReferenceID)
	if projectID == "" {
		// Fall back to metadata if ClientReferenceID is empty.
		if session.Metadata != nil {
			projectID = strings.TrimSpace(session.Metadata["mesedi_project_id"])
		}
	}
	if projectID == "" {
		return fmt.Errorf("checkout.session.completed missing project_id")
	}
	customerID := ""
	if session.Customer != nil {
		customerID = session.Customer.ID
	}
	subscriptionID := ""
	if session.Subscription != nil {
		subscriptionID = session.Subscription.ID
	}
	// Period bounds may not be on the Checkout session itself; the
	// subsequent customer.subscription.updated event will fill them
	// in. For now, set to nil — the dashboard handles missing bounds
	// gracefully.
	return h.Store.UpdateProjectBilling(
		context.Background(),
		projectID, TierPro, customerID, subscriptionID, nil, nil,
	)
}

// handleSubscriptionUpdated refreshes period bounds and (if the
// subscription was canceled at period end) records the downgrade
// signal. Actual downgrade happens on customer.subscription.deleted.
func (h *Handlers) handleSubscriptionUpdated(event stripe.Event) error {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		return fmt.Errorf("unmarshal subscription: %w", err)
	}
	if sub.Customer == nil {
		return fmt.Errorf("subscription.updated missing customer")
	}
	p, err := h.Store.GetProjectByStripeCustomerID(context.Background(), sub.Customer.ID)
	if err != nil {
		return fmt.Errorf("lookup project by customer %s: %w", sub.Customer.ID, err)
	}
	periodStart, periodEnd := subscriptionPeriodBounds(&sub)
	tier := p.Tier
	if sub.Status == stripe.SubscriptionStatusActive ||
		sub.Status == stripe.SubscriptionStatusTrialing {
		tier = TierPro
	}
	return h.Store.UpdateProjectBilling(
		context.Background(),
		p.ProjectID, tier, sub.Customer.ID, sub.ID, periodStart, periodEnd,
	)
}

// handleSubscriptionDeleted downgrades the project back to Hobby
// when the Stripe subscription is canceled (either at period end or
// immediately).
func (h *Handlers) handleSubscriptionDeleted(event stripe.Event) error {
	var sub stripe.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		return fmt.Errorf("unmarshal subscription: %w", err)
	}
	if sub.Customer == nil {
		return fmt.Errorf("subscription.deleted missing customer")
	}
	p, err := h.Store.GetProjectByStripeCustomerID(context.Background(), sub.Customer.ID)
	if err != nil {
		return fmt.Errorf("lookup project by customer %s: %w", sub.Customer.ID, err)
	}
	// Keep the customer id so the user can re-subscribe later
	// without re-collecting card data; clear the subscription id and
	// period bounds. Tier returns to Hobby.
	return h.Store.UpdateProjectBilling(
		context.Background(),
		p.ProjectID, TierHobby, sub.Customer.ID, "", nil, nil,
	)
}

// handleInvoicePaid resets the per-period execution counter when a
// new invoice is paid (i.e., a new billing period begins). Idempotent:
// if Stripe re-delivers the same event the counter resets to zero
// again on the (already-active) new period.
func (h *Handlers) handleInvoicePaid(event stripe.Event) error {
	var invoice stripe.Invoice
	if err := json.Unmarshal(event.Data.Raw, &invoice); err != nil {
		return fmt.Errorf("unmarshal invoice: %w", err)
	}
	if invoice.Customer == nil {
		// Not all invoices have a customer (one-off payments etc.);
		// for a subscription product we expect one. Log and move on.
		return nil
	}
	p, err := h.Store.GetProjectByStripeCustomerID(context.Background(), invoice.Customer.ID)
	if err != nil {
		return fmt.Errorf("lookup project by customer %s: %w", invoice.Customer.ID, err)
	}
	periodStart, periodEnd := invoicePeriodBounds(&invoice)
	if periodStart == nil || periodEnd == nil {
		// Invoice without period bounds — nothing to roll over.
		return nil
	}
	return h.Store.ResetExecutionsThisPeriod(
		context.Background(),
		p.ProjectID, *periodStart, *periodEnd,
	)
}

// ── small helpers ───────────────────────────────────────────────

// subscriptionPeriodBounds extracts current_period_start and
// current_period_end from a Stripe subscription as time.Time
// pointers. Returns (nil, nil) if either is zero (Stripe represents
// "unset" as 0 unix timestamp).
func subscriptionPeriodBounds(sub *stripe.Subscription) (*time.Time, *time.Time) {
	if sub == nil {
		return nil, nil
	}
	var startPtr, endPtr *time.Time
	// In v82 the period fields live on each subscription item.
	if len(sub.Items.Data) > 0 {
		item := sub.Items.Data[0]
		if item.CurrentPeriodStart > 0 {
			t := time.Unix(item.CurrentPeriodStart, 0).UTC()
			startPtr = &t
		}
		if item.CurrentPeriodEnd > 0 {
			t := time.Unix(item.CurrentPeriodEnd, 0).UTC()
			endPtr = &t
		}
	}
	return startPtr, endPtr
}

// invoicePeriodBounds extracts the most-likely line-item period
// from an Invoice. Stripe invoices carry per-line period bounds;
// for a simple one-product subscription the first line item is the
// subscription line and its period is the billing period we want.
func invoicePeriodBounds(inv *stripe.Invoice) (*time.Time, *time.Time) {
	if inv == nil || inv.Lines == nil || len(inv.Lines.Data) == 0 {
		return nil, nil
	}
	line := inv.Lines.Data[0]
	if line.Period == nil {
		return nil, nil
	}
	var startPtr, endPtr *time.Time
	if line.Period.Start > 0 {
		t := time.Unix(line.Period.Start, 0).UTC()
		startPtr = &t
	}
	if line.Period.End > 0 {
		t := time.Unix(line.Period.End, 0).UTC()
		endPtr = &t
	}
	return startPtr, endPtr
}
