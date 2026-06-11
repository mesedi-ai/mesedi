package api

// Customer-initiated payment method removal for Cloud Hobby and
// Cloud Team (#209).
//
// Policy ("no hostage"): a customer can always remove their saved
// card, but if pending charges (execution overage + AI analyses)
// > $0 we charge them first via an immediate off-session
// PaymentIntent and detach only on success. If $0 pending, detach
// immediately.
//
// Confirmation gate: the first POST returns 409 with the breakdown
// (so the dashboard can show "Charge $X.XX and remove card?" modal)
// unless the body opts in with {"confirm": true}.
//
// Tier semantics:
//   - Hobby: no subscription, just a saved card for overage billing.
//     Pending = exec overage ($0.002/unit) + AI analyses ($0.75 each
//     from analysis #1). Removal returns customer to "no card, free
//     quota only" state.
//   - Team: active $99/mo subscription PLUS overage. Pending = exec
//     overage ($0.001/unit) + AI analyses ($0.50 each above the
//     included 200). Removal SETTLES the overage but does NOT
//     cancel the subscription — Stripe's next invoice ($99) will
//     fail to charge and enter dunning. The response includes a
//     warning so the dashboard can flag this and offer
//     "Downgrade to Hobby" or "Close account" as the proper
//     subscription-termination paths.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/customer"
	"github.com/stripe/stripe-go/v82/paymentintent"
	"github.com/stripe/stripe-go/v82/paymentmethod"
	"github.com/stripe/stripe-go/v82/subscription"

	"mesedi/backend/internal/store"
)

// PaymentMethodRemoveRequest is the body of
// POST /billing/payment-method/remove.
type PaymentMethodRemoveRequest struct {
	// Confirm: when true, charges any pending balance immediately
	// then detaches the card. When false (the default), refuses
	// with 409 if pending > 0 so the frontend can show a
	// confirmation modal with the breakdown.
	Confirm bool `json:"confirm,omitempty"`
}

// PaymentMethodRemoveResponse is returned on a successful removal.
type PaymentMethodRemoveResponse struct {
	OK                     bool    `json:"ok"`
	Tier                   string  `json:"tier"`
	ChargedUSD             float64 `json:"charged_usd"`
	ExecutionOverageUnits  int64   `json:"execution_overage_units"`
	AIAnalysisOverageUnits int     `json:"ai_analysis_overage_units"`
	PaymentIntentID        string  `json:"payment_intent_id,omitempty"`
	// Warning is a human-readable note included on Team removes
	// to flag that the active $99/mo subscription will fail at the
	// next renewal if not explicitly canceled. Empty on Hobby.
	Warning string `json:"warning,omitempty"`
}

// PaymentMethodRemovePendingResponse is the 409 body the handler
// returns when pending > 0 and the customer has not yet confirmed
// the immediate charge. Drives the dashboard "Are you sure?" modal.
type PaymentMethodRemovePendingResponse struct {
	OK                     bool    `json:"ok"`
	RequiresConfirm        bool    `json:"requires_confirm"`
	Tier                   string  `json:"tier"`
	PendingUSD             float64 `json:"pending_usd"`
	ExecutionOverageUSD    float64 `json:"execution_overage_usd"`
	AIAnalysisOverageUSD   float64 `json:"ai_analysis_overage_usd"`
	ExecutionOverageUnits  int64   `json:"execution_overage_units"`
	AIAnalysisOverageUnits int     `json:"ai_analysis_overage_units"`
	BillingCapUSD          float64 `json:"billing_cap_usd"`
	// Warning is a human-readable note included on Team removes
	// so the modal can flag the subscription-failure risk before
	// the customer confirms.
	Warning string `json:"warning,omitempty"`
}

// HandleRemovePaymentMethod implements the "no hostage" card-removal
// policy for Cloud Hobby. See file-level comment.
func (h *Handlers) HandleRemovePaymentMethod(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "admin") {
		return
	}
	if !h.Stripe.Configured() {
		billingNotConfigured(w)
		return
	}
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	var req PaymentMethodRemoveRequest
	if r.Body != nil && r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "decode body: "+err.Error())
			return
		}
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
	tier := normalizeTier(p.Tier)
	if tier != TierHobby && tier != TierTeam {
		// Enterprise + any unknown tier: card removal isn't a
		// concept here. Enterprise customers manage payment via
		// contract / wire transfer; they don't have a saved card
		// on this path.
		writeError(w, http.StatusBadRequest,
			"Card removal is available only on Cloud Hobby and Cloud Team.")
		return
	}
	if p.StripeCustomerID == "" {
		writeError(w, http.StatusBadRequest, "no payment method on file")
		return
	}

	// Compute pending charges. Execution overage from the
	// in-memory counter; AI analyses from a per-project DB query.
	// Per-tier overage rates are baked into computeOverageCostUSD
	// (executions) and computeAIAnalysisPendingForTier (analyses).
	execCost := computeOverageCostUSD(p)
	aiCount, aiCost := computeAIAnalysisPendingForTier(
		r.Context(), h.Store, p, tier, h.Logger,
	)
	pending := execCost + aiCost

	// Apply the billing cap to the combined total (matches the
	// scheduler's cap-respect logic). Customer-set BillingCapUSD = 0
	// means uncapped.
	if p.BillingCapUSD > 0 && pending > p.BillingCapUSD {
		pending = p.BillingCapUSD
	}

	// Execution overage units (for breakdown + Stripe Description).
	included := tierExecutionLimit(TierHobby) + p.GrantedExecutions
	if included < 0 {
		included = 0
	}
	overUnits := p.ExecutionsThisPeriod - included
	if overUnits < 0 {
		overUnits = 0
	}

	// Team-only warning: card removal schedules an auto-downgrade
	// to Cloud Hobby at the end of the current billing cycle (the
	// next-renewal Stripe charge is canceled in advance, no
	// dunning). Hard-cap applies in the meantime so the customer
	// keeps everything they paid for through period end. Re-adding
	// a card before the deadline reverses the scheduled downgrade.
	teamWarning := ""
	if tier == TierTeam {
		periodEndCopy := "the end of your current billing cycle"
		if p.CurrentPeriodEnd != nil {
			periodEndCopy = p.CurrentPeriodEnd.Format("2006-01-02")
		}
		teamWarning = fmt.Sprintf(
			"You will keep Cloud Team access through %s (hard-capped at the included 100K executions and %d AI analyses since there is no card on file for overage). At that point, if you have not added a new payment method, your subscription will auto-downgrade to Cloud Hobby — no failed-charge emails, no dunning. To stay on Team, add a card on /app/billing before then.",
			periodEndCopy, TeamAIAnalysisLimit,
		)
	}

	// No pending: detach immediately. No PaymentIntent needed.
	if pending <= 0 {
		if err := h.detachCardWithReset(r.Context(), p); err != nil {
			writeError(w, http.StatusInternalServerError, "detach: "+err.Error())
			return
		}
		h.Logger.Info("card removed (no pending charges)",
			"project_id", p.ProjectID, "tier", tier)
		writeJSON(w, http.StatusOK, PaymentMethodRemoveResponse{
			OK:                     true,
			Tier:                   tier,
			ChargedUSD:             0,
			ExecutionOverageUnits:  overUnits,
			AIAnalysisOverageUnits: aiCount,
			Warning:                teamWarning,
		})
		return
	}

	// Pending > 0. Customer must confirm before we charge.
	if !req.Confirm {
		writeJSON(w, http.StatusConflict, PaymentMethodRemovePendingResponse{
			OK:                     false,
			RequiresConfirm:        true,
			Tier:                   tier,
			PendingUSD:             pending,
			ExecutionOverageUSD:    execCost,
			AIAnalysisOverageUSD:   aiCost,
			ExecutionOverageUnits:  overUnits,
			AIAnalysisOverageUnits: aiCount,
			BillingCapUSD:          p.BillingCapUSD,
			Warning:                teamWarning,
		})
		return
	}

	// Confirmed. Charge then detach.
	cents := int64(math.Round(pending * 100))
	if cents <= 0 {
		// Round-to-zero (less than half a cent of overage).
		// Treat as no-charge case.
		if err := h.detachCardWithReset(r.Context(), p); err != nil {
			writeError(w, http.StatusInternalServerError, "detach: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, PaymentMethodRemoveResponse{
			OK:                     true,
			Tier:                   tier,
			ChargedUSD:             0,
			ExecutionOverageUnits:  overUnits,
			AIAnalysisOverageUnits: aiCount,
			Warning:                teamWarning,
		})
		return
	}

	desc := buildRemovalChargeDescription(tier, overUnits, aiCount) +
		" (settled on card removal)"
	h.Stripe.applyKey()
	piParams := &stripe.PaymentIntentParams{
		Amount:      stripe.Int64(cents),
		Currency:    stripe.String(string(stripe.CurrencyUSD)),
		Customer:    stripe.String(p.StripeCustomerID),
		Confirm:     stripe.Bool(true),
		OffSession:  stripe.Bool(true),
		Description: stripe.String(desc),
		Metadata: map[string]string{
			"mesedi_project_id":  p.ProjectID,
			"mesedi_tier":        tier,
			"mesedi_overage":     fmt.Sprintf("%d", overUnits),
			"mesedi_ai_analyses": fmt.Sprintf("%d", aiCount),
			"mesedi_reason":      "card_removal_settle",
		},
	}
	// Idempotency: tier + project_id + period_start so a customer
	// retry within the same period returns the existing PI rather
	// than double-charging. After a successful removal the period
	// is reset (new period_start), so a future re-attach + re-remove
	// gets a fresh idempotency window.
	periodAnchor := int64(0)
	if p.CurrentPeriodStart != nil {
		periodAnchor = p.CurrentPeriodStart.Unix()
	}
	piParams.IdempotencyKey = stripe.String(
		fmt.Sprintf("%s-remove-%s-%d", tier, p.ProjectID, periodAnchor),
	)

	pi, piErr := paymentintent.New(piParams)
	if piErr != nil {
		h.Logger.Warn("card removal charge failed",
			"project_id", p.ProjectID, "tier", tier, "error", piErr.Error())
		writeError(w, http.StatusPaymentRequired,
			fmt.Sprintf("Could not charge $%.2f before removing your card: %s. Update or replace your card and try again.",
				pending, piErr.Error()))
		return
	}

	if err := h.detachCardWithReset(r.Context(), p); err != nil {
		// Charge succeeded but detach failed. Log loudly and tell
		// the customer to contact support with the PaymentIntent
		// ID so we can clean up state by hand.
		h.Logger.Error("card removal: charge succeeded but detach failed",
			"project_id", p.ProjectID,
			"tier", tier,
			"payment_intent_id", pi.ID,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError,
			"Charge succeeded but card detach failed. Contact support@mesedi.ai with PaymentIntent "+pi.ID)
		return
	}

	h.Logger.Info("card removed with settlement",
		"project_id", p.ProjectID,
		"tier", tier,
		"amount_cents", cents,
		"payment_intent_id", pi.ID,
		"execution_overage_units", overUnits,
		"ai_analysis_overage_units", aiCount,
	)
	writeJSON(w, http.StatusOK, PaymentMethodRemoveResponse{
		OK:                     true,
		Tier:                   tier,
		ChargedUSD:             pending,
		ExecutionOverageUnits:  overUnits,
		AIAnalysisOverageUnits: aiCount,
		PaymentIntentID:        pi.ID,
		Warning:                teamWarning,
	})
}

// detachCardWithReset does the actual Stripe-side detach + local
// clear + period reset, and on Team also schedules the subscription
// to cancel at period end so no $99 renewal charge fires against
// the missing card. Best-effort on each Stripe-side step so a
// transient Stripe outage doesn't block the customer from removing
// their card from Mesedi's perspective; the local clear is the
// source of truth for "we will not attempt to charge this customer
// again."
//
// After clearing the card:
//   - On Hobby: customer gets a fresh free quota for the remainder
//     of the period. Next scheduler tick when current_period_end
//     has passed sees no card and just rolls the period normally.
//   - On Team: the Stripe subscription is set to cancel_at_period_end
//     so no $99 renewal fires and no dunning emails go out. The
//     customer keeps Team service through the existing period_end
//     (hard-capped at the included 100K execs + 200 analyses since
//     capExceeded blocks overage without a card). At period end,
//     Stripe fires customer.subscription.deleted; the existing
//     webhook handler flips the project to Hobby. If they re-add a
//     card before then, the future Setup Intent flow can clear the
//     cancel_at_period_end flag and they stay on Team seamlessly.
func (h *Handlers) detachCardWithReset(ctx context.Context, p *store.Project) error {
	// Best-effort Stripe detach to avoid a phantom card lingering on
	// the customer's Stripe portal view.
	h.Stripe.applyKey()
	if cust, err := customer.Get(p.StripeCustomerID, nil); err == nil &&
		cust != nil &&
		cust.InvoiceSettings != nil &&
		cust.InvoiceSettings.DefaultPaymentMethod != nil &&
		cust.InvoiceSettings.DefaultPaymentMethod.ID != "" {
		pmID := cust.InvoiceSettings.DefaultPaymentMethod.ID
		if _, dErr := paymentmethod.Detach(pmID, nil); dErr != nil {
			h.Logger.Warn("stripe payment method detach failed (continuing with local clear)",
				"project_id", p.ProjectID,
				"payment_method_id", pmID,
				"error", dErr.Error())
		}
	} else if err != nil {
		h.Logger.Warn("stripe customer lookup before detach failed",
			"project_id", p.ProjectID, "error", err.Error())
	}

	// Team-only: schedule the active subscription to cancel at
	// period end. Prevents the next $99 renewal from firing against
	// the now-missing card. Best-effort: if this fails the customer
	// ends up with Stripe's natural dunning instead of clean
	// auto-downgrade, which is a degraded but recoverable state.
	if normalizeTier(p.Tier) == TierTeam && p.StripeSubscriptionID != "" {
		cancelAtPeriodEnd := true
		if _, sErr := subscription.Update(
			p.StripeSubscriptionID,
			&stripe.SubscriptionParams{CancelAtPeriodEnd: &cancelAtPeriodEnd},
		); sErr != nil {
			h.Logger.Warn("team card removal: subscription cancel-at-period-end failed (continuing; customer will see Stripe dunning instead of clean downgrade)",
				"project_id", p.ProjectID,
				"sub_id", p.StripeSubscriptionID,
				"error", sErr.Error())
		} else {
			h.Logger.Info("team card removal: subscription scheduled to cancel at period end",
				"project_id", p.ProjectID,
				"sub_id", p.StripeSubscriptionID)
		}
	}

	// Clear local card state. Tier-specific:
	//   - Hobby: full detach (null stripe_customer_id + clear
	//     billing failure counters via DetachHobbyCardForBillingFailure).
	//     There's no subscription to preserve linkage for.
	//   - Team: MarkCardDetached only — keep stripe_customer_id
	//     populated so the active subscription stays addressable
	//     and a future Setup Intent re-attach reuses the same
	//     customer (no duplicate customer records).
	if normalizeTier(p.Tier) == TierTeam {
		if err := h.Store.MarkCardDetached(ctx, p.ProjectID); err != nil {
			return fmt.Errorf("mark card detached: %w", err)
		}
	} else {
		if err := h.Store.DetachHobbyCardForBillingFailure(ctx, p.ProjectID); err != nil {
			return fmt.Errorf("clear local card state: %w", err)
		}
	}

	// Reset the executions counter + advance period start. Customer
	// gets a fresh free quota for the remainder of the period.
	now := time.Now().UTC()
	periodEnd := now.AddDate(0, 1, 0)
	if p.CurrentPeriodEnd != nil && p.CurrentPeriodEnd.After(now) {
		periodEnd = *p.CurrentPeriodEnd
	}
	if err := h.Store.ResetExecutionsThisPeriod(ctx, p.ProjectID, now, periodEnd); err != nil {
		return fmt.Errorf("reset period counter: %w", err)
	}
	return nil
}

// computeAIAnalysisPendingForTier returns (count, cost_USD) of AI
// root-cause analyses the project has accumulated this billing
// period that haven't been billed yet. Tier-aware:
//
//   - Hobby: every analysis bills (cost = count × $0.75). Per-project
//     count.
//   - Team: only analyses ABOVE the 200 included bill (cost =
//     max(0, count - 200) × $0.50). Per-tenant count to match
//     enforcement.
//
// Returns (0, 0) on DB error so a transient blip doesn't settle a
// number we can't trust. Mirrors HandleAnalyzeFailureGroup's window
// + scope logic.
func computeAIAnalysisPendingForTier(
	ctx context.Context, s store.Store, p *store.Project, tier string, logger *slog.Logger,
) (int, float64) {
	since := time.Now().UTC().AddDate(0, -1, 0)
	if p.CurrentPeriodStart != nil {
		since = *p.CurrentPeriodStart
	}

	// Scope selection mirrors HandleAnalyzeFailureGroup: Team uses
	// tenant scope (org-wide cap), Hobby uses per-project (single
	// project tier).
	var count int
	var err error
	if tier == TierTeam {
		tenantID, terr := s.GetProjectTenantID(ctx, p.ProjectID)
		switch {
		case terr == nil && tenantID != nil && *tenantID != "":
			count, err = s.CountAIAnalysesByTenantSince(ctx, *tenantID, since)
		default:
			count, err = s.CountAIAnalysesSincePeriodStart(ctx, p.ProjectID, since)
		}
	} else {
		count, err = s.CountAIAnalysesSincePeriodStart(ctx, p.ProjectID, since)
	}
	if err != nil {
		logger.Warn("AI analysis pending count failed (treating as 0)",
			"project_id", p.ProjectID, "tier", tier, "error", err.Error())
		return 0, 0
	}
	if count <= 0 {
		return 0, 0
	}

	switch tier {
	case TierHobby:
		// Every analysis is billable.
		return count, float64(count) * HobbyAIAnalysisPriceUSD
	case TierTeam:
		// Only overage above the 200 included is billable. Return
		// the OVERAGE count (the unit we'd put on the receipt), not
		// the full count.
		if count <= TeamAIAnalysisLimit {
			return 0, 0
		}
		overage := count - TeamAIAnalysisLimit
		return overage, float64(overage) * TeamAIAnalysisOveragePriceUSD
	}
	return 0, 0
}

// buildRemovalChargeDescription formats the Stripe Description that
// appears on the customer's bank statement and Stripe receipt when
// we settle pending overage on card removal. Tier-aware so the line
// items match the customer's mental model (Hobby pay-per-use vs
// Team overage above included).
//
// Single-line and combined cases match buildHobbyChargeDescription's
// pattern: clean string when only one line item is non-zero, joined
// with " + " when both are.
func buildRemovalChargeDescription(tier string, overUnits int64, aiCount int) string {
	var parts []string
	switch tier {
	case TierHobby:
		if overUnits > 0 {
			parts = append(parts, fmt.Sprintf(
				"Mesedi Hobby overage: %d executions x $%.3f",
				overUnits, HobbyOveragePriceUSD,
			))
		}
		if aiCount > 0 {
			parts = append(parts, fmt.Sprintf(
				"Mesedi Hobby AI root-cause: %d analyses x $%.2f",
				aiCount, HobbyAIAnalysisPriceUSD,
			))
		}
	case TierTeam:
		if overUnits > 0 {
			parts = append(parts, fmt.Sprintf(
				"Mesedi Team overage: %d executions x $%.3f",
				overUnits, TeamOveragePriceUSD,
			))
		}
		if aiCount > 0 {
			parts = append(parts, fmt.Sprintf(
				"Mesedi Team AI root-cause overage: %d analyses x $%.2f",
				aiCount, TeamAIAnalysisOveragePriceUSD,
			))
		}
	}
	if len(parts) == 0 {
		return "Mesedi accumulated charge"
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return parts[0] + " + " + parts[1]
}
