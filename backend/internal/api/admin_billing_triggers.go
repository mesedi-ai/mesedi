// Admin trigger endpoints for the billing schedulers.
//
// Two POST endpoints expose the periodic billing job cycle as a
// per-project, on-demand invocation:
//
//   POST /admin/projects/{id}/trigger-hobby-billing-run
//   POST /admin/projects/{id}/trigger-team-billing-run
//
// The primary use case is the payment-smoke harness (#366): once the
// harness has pushed executions and AI analyses over the tier quotas
// (test-mode overrides make this cheap; see billing_test_overrides.go),
// the harness POSTs one of these endpoints to synchronously push the
// resulting overage as Stripe InvoiceItems / UsageRecords, then
// verifies against Stripe's API that the push landed.
//
// Founder-side these are also useful as a debug tool for force-
// closing a billing period on a real customer without waiting for
// the nightly scheduler tick — the same code path as the automated
// job, just triggered on demand.
//
// Both endpoints are admin-authed and mutate real billing state.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"mesedi/backend/internal/store"

	stripego "github.com/stripe/stripe-go/v82"
)

// HandleAdminTriggerHobbyBillingRun invokes the Hobby billing
// scheduler's processProject step for one specific project.
// Response: {"ok": true, "project_id": "..."}. Any per-project
// errors are logged but not surfaced in the response body since
// the scheduler itself is best-effort (a partial run must not
// panic).
func (h *Handlers) HandleAdminTriggerHobbyBillingRun(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "missing project id")
		return
	}
	if h.HobbyBillingScheduler == nil {
		writeError(w, http.StatusServiceUnavailable,
			"hobby billing scheduler not configured on this deploy")
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

	// Reuse the same code path the nightly scheduler runs. The
	// scheduler's processProject already logs everything of interest
	// and does not return; the harness verifies the outcome via
	// GET /billing + direct Stripe API calls.
	h.HobbyBillingScheduler.processProject(r.Context(), p, time.Now().UTC())

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"project_id": projectID,
		"note":       "hobby billing scheduler invoked; check GET /billing + Stripe for effects",
	})
}

// HandleAdminTriggerTeamBillingRun invokes the Team overage-push
// logic that normally runs off the `invoice.upcoming` Stripe webhook.
// Constructs a synthetic Stripe event using the project's stored
// stripe_customer_id and passes it through the existing webhook
// handler. The synthetic event ID includes a nanosecond timestamp
// so retries produce fresh Stripe InvoiceItems (rather than being
// deduped by the idempotency key).
func (h *Handlers) HandleAdminTriggerTeamBillingRun(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "missing project id")
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
		writeError(w, http.StatusPreconditionFailed,
			"project has no stripe_customer_id; attach a card via /billing/payment-method/setup first")
		return
	}
	if normalizeTier(p.Tier) != TierTeam {
		writeError(w, http.StatusPreconditionFailed,
			fmt.Sprintf("project tier is %q; trigger-team-billing-run only applies to Team", p.Tier))
		return
	}

	// Synthesize an `invoice.upcoming`-shaped Stripe event with the
	// customer id we need. handleInvoiceUpcoming unmarshals the
	// event body into stripe.Invoice; the only field it reads for
	// lookup is Customer.ID, so a minimal payload suffices.
	invoice := stripego.Invoice{
		Customer: &stripego.Customer{ID: p.StripeCustomerID},
	}
	rawInvoice, err := json.Marshal(invoice)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"marshal synthetic invoice: "+err.Error())
		return
	}
	// Event ID includes ns-precision timestamp so repeat calls
	// produce fresh Stripe InvoiceItems. The real
	// invoice.upcoming path uses Stripe's evt_... id, which is
	// unique per delivery; ours is unique per call, matching the
	// same idempotency semantics.
	eventID := fmt.Sprintf("evt_admin_trigger_team_%s_%d",
		projectID, time.Now().UnixNano())
	event := stripego.Event{
		ID:   eventID,
		Type: "invoice.upcoming",
		Data: &stripego.EventData{Raw: rawInvoice},
	}

	logger := h.Logger.With(
		"path", "/admin/trigger-team-billing-run",
		"project_id", projectID,
		"stripe_customer_id", p.StripeCustomerID,
		"synthetic_event_id", eventID,
	)

	if err := h.handleInvoiceUpcoming(event, logger); err != nil {
		// The handler itself already logs. Return 500 so admin
		// callers can see the error; production real-webhook path
		// swallows errors for Stripe retry hygiene, but a debug
		// trigger should be loud.
		writeError(w, http.StatusInternalServerError,
			"team billing run failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                 true,
		"project_id":         projectID,
		"stripe_customer_id": p.StripeCustomerID,
		"synthetic_event_id": eventID,
		"note":               "team invoice.upcoming path invoked; check Stripe InvoiceItems on the customer for the push",
	})
}

// HandleAdminResetPeriodCounter zeros a project's
// executions_this_period counter and advances the current billing
// period to a fresh 30-day window starting at now.
//
// Motivation: the payment-smoke harness runs multiple destructive
// scenarios back-to-back and needs the mesedi-side execution counter
// reset between runs so state doesn't leak across sessions.
// Otherwise the previous run's overage bleeds into the next run's
// "under quota" checks. Founder-side this is also useful as a debug
// tool for wiping a project's counter without waiting for a billing
// period to naturally roll over.
//
// Idempotent: safe to call from any tier and any pending state. Does
// NOT touch Stripe or the subscription — that lives on the trigger
// endpoints above. Does NOT clear granted_executions since those are
// a distinct product concern (bonus credits).
//
// Response: {"ok": true, "project_id": "...",
// "previous_executions_this_period": N,
// "new_period_start": "...", "new_period_end": "..."}.
func (h *Handlers) HandleAdminResetPeriodCounter(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "missing project id")
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

	previousExecutions := p.ExecutionsThisPeriod
	now := time.Now().UTC()
	// 30-day window matches the Hobby-tier natural period. On Team
	// the actual billing period is Stripe-driven and reasserted on
	// the next invoice.paid webhook, so this window is a placeholder
	// that any real webhook will overwrite.
	newEnd := now.AddDate(0, 0, 30)

	if err := h.Store.ResetExecutionsThisPeriod(r.Context(), projectID, now, newEnd); err != nil {
		writeError(w, http.StatusInternalServerError,
			"reset period counter: "+err.Error())
		return
	}

	h.recordAuditEventForProject(
		context.Background(),
		projectID,
		AuditActorPlatformAdmin,
		AuditAdminPeriodCounterReset,
		"project",
		projectID,
		map[string]any{
			"previous_executions_this_period": previousExecutions,
			"new_period_start":                now.Format(time.RFC3339),
			"new_period_end":                  newEnd.Format(time.RFC3339),
		},
	)
	h.Logger.Info("admin reset period counter",
		"project_id", projectID,
		"previous_executions_this_period", previousExecutions,
		"new_period_start", now.Format(time.RFC3339),
		"new_period_end", newEnd.Format(time.RFC3339))

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                              true,
		"project_id":                      projectID,
		"previous_executions_this_period": previousExecutions,
		"new_period_start":                now.Format(time.RFC3339),
		"new_period_end":                  newEnd.Format(time.RFC3339),
		"note":                            "counter reset; period window advanced to a fresh 30-day span. Stripe not touched.",
	})
}

// ── register ────────────────────────────────────────────────

// Registered from admin.go's RegisterAdminRoutes.
var _ = context.Background // silence unused import if the refactor
// ever removes the ctx usage above.
