package api

// HobbyBillingScheduler is the daily worker that closes Hobby-tier
// billing periods, charges accrued overage off the customer's saved
// payment method, and handles the retry / auto-detach lifecycle for
// failed charges.
//
// Lifecycle per tick (daily):
//
//   1. Pull every Hobby project whose current_period_end has passed
//      OR whose current_period_start is NULL (legacy unbootstrapped
//      rows that need their first billing window assigned).
//
//   2. For projects with NULL bounds: bootstrap to (now, now+1 month)
//      and reset the executions counter. No charge attempted; these
//      customers may not have a card yet, and even if they do, there
//      is no past usage to bill.
//
//   3. For projects with past period_end AND no stripe_customer_id:
//      just rollover (advance bounds by 1 month, reset counter). The
//      customer was hard-capped at the free quota for the period so
//      there is nothing to charge.
//
//   4. For projects with past period_end AND a stripe_customer_id:
//      enforce the every-other-day retry cadence (skip if last
//      attempt < 48h ago), then attempt the off-session PaymentIntent.
//      On success: advance bounds by 1 month, reset counter, clear
//      failure state, send a Resend receipt email if Resend is wired.
//      On failure: increment consecutive_failures, set last_attempt,
//      send a Resend dunning email. If failures cross
//      HobbyBillingFailureCeiling, detach the saved card and send a
//      final "your card has been removed, attach a new one" email.
//
// New billing period anchor: new_period_start = old_period_end (mirror
// Stripe pattern for subscriptions). This preserves the customer's
// original anchor date across rollovers; a scheduler outage of one
// or two days does not shift their anchor.

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/paymentintent"

	"mesedi/backend/internal/mail"
	"mesedi/backend/internal/store"
)

// HobbyBillingScheduler walks Hobby projects daily and closes their
// billing periods.
type HobbyBillingScheduler struct {
	Store        store.Store
	Stripe       StripeConfig
	Mailer       mail.Mailer
	DashboardURL string // base URL for links in email bodies; e.g. "https://app.mesedi.ai"
	Logger       *slog.Logger

	// TickInterval governs how often the worker runs. Default 24h
	// when zero. Tests dial it down to milliseconds.
	TickInterval time.Duration

	once   sync.Once
	cancel context.CancelFunc
}

// Start launches the worker goroutine. Idempotent.
func (s *HobbyBillingScheduler) Start(ctx context.Context) {
	s.once.Do(func() {
		if s.TickInterval == 0 {
			s.TickInterval = 24 * time.Hour
		}
		runCtx, cancel := context.WithCancel(ctx)
		s.cancel = cancel
		go s.run(runCtx)
	})
}

// Shutdown cancels the worker. Safe to call multiple times.
func (s *HobbyBillingScheduler) Shutdown() {
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *HobbyBillingScheduler) run(ctx context.Context) {
	s.Logger.Info("hobby_billing_scheduler: started",
		"tick_interval", s.TickInterval.String())

	// First tick on a 60-second delay so a brand-new boot has time
	// to finish migrations and serve traffic before we start doing
	// charge work. The scheduler is idempotent, so missing the first
	// tick costs nothing.
	select {
	case <-ctx.Done():
		s.Logger.Info("hobby_billing_scheduler: shutting down before first tick")
		return
	case <-time.After(60 * time.Second):
	}

	if err := s.tick(ctx); err != nil {
		s.Logger.Error("hobby_billing_scheduler: first tick failed",
			"error", err.Error())
	}

	t := time.NewTicker(s.TickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.Logger.Info("hobby_billing_scheduler: shutting down")
			return
		case <-t.C:
			if err := s.tick(ctx); err != nil {
				s.Logger.Error("hobby_billing_scheduler: tick failed",
					"error", err.Error())
			}
		}
	}
}

func (s *HobbyBillingScheduler) tick(ctx context.Context) error {
	if !s.Stripe.Configured() {
		// Backend running without Stripe (CE / local dev). The
		// scheduler has nothing useful to do.
		return nil
	}
	now := time.Now().UTC()
	projects, err := s.Store.ListProjectsForHobbyBillingTick(ctx, now)
	if err != nil {
		return fmt.Errorf("list projects for tick: %w", err)
	}
	for _, p := range projects {
		s.processProject(ctx, p, now)
	}
	return nil
}

// processProject handles one Hobby project's per-tick logic.
// Returns are intentionally swallowed: a single bad row should not
// stop the scheduler from processing every other row.
func (s *HobbyBillingScheduler) processProject(
	ctx context.Context, p *store.Project, now time.Time,
) {
	log := s.Logger.With("project_id", p.ProjectID)

	// Case A: NULL period bounds. Bootstrap to (now, now+1mo).
	// Per Robert's product decision: every existing Hobby customer
	// gets a fresh window assigned on first scheduler sight, even
	// those without cards. Counter is reset to 0 as part of this.
	if p.CurrentPeriodStart == nil || p.CurrentPeriodEnd == nil {
		end := now.AddDate(0, 1, 0)
		if rErr := s.Store.ResetExecutionsThisPeriod(
			ctx, p.ProjectID, now, end,
		); rErr != nil {
			log.Error("bootstrap period bounds failed", "error", rErr.Error())
			return
		}
		log.Info("hobby period bounds bootstrapped",
			"period_start", now, "period_end", end)
		return
	}

	// Case B: period_end still in the future. Nothing to do.
	if !p.CurrentPeriodEnd.Before(now) && !p.CurrentPeriodEnd.Equal(now) {
		return
	}

	// Case C: period_end has passed. Three sub-cases follow based on
	// whether the customer has a card on file and whether the retry
	// cadence allows a fresh attempt today.
	newStart := *p.CurrentPeriodEnd
	newEnd := newStart.AddDate(0, 1, 0)

	if p.StripeCustomerID == "" {
		// No card on file: customer was hard-capped at the free
		// quota all period. Nothing to charge. Just rollover the
		// bounds so the next period starts cleanly.
		if rErr := s.Store.ResetExecutionsThisPeriod(
			ctx, p.ProjectID, newStart, newEnd,
		); rErr != nil {
			log.Error("rollover (no-card) failed", "error", rErr.Error())
			return
		}
		log.Info("hobby period rolled over (no card on file)",
			"new_period_start", newStart, "new_period_end", newEnd)
		return
	}

	// Retry cadence guard: skip if a charge was attempted within the
	// last HobbyBillingRetryCadence (48 hours). The daily tick still
	// fires every 24h, but consecutive same-day or next-day attempts
	// are suppressed so we get the "day 1, 3, 5, 7..." pattern.
	if p.HobbyBillingLastAttemptAt != nil &&
		now.Sub(*p.HobbyBillingLastAttemptAt) < HobbyBillingRetryCadence {
		log.Info("hobby billing skipped: within retry cadence window",
			"last_attempt_at", *p.HobbyBillingLastAttemptAt,
			"cadence", HobbyBillingRetryCadence.String())
		return
	}

	// Compute total period-end cost: execution overage + AI analysis
	// usage (Hobby pay-per-analysis). Both line items share the
	// project's billing_cap_usd, so a $200 cap protects against
	// surprise bills whether the customer pushed executions, analyses,
	// or both.
	executionCost := computeOverageCostUSD(p)
	analysisCost, analysisCount := s.computeHobbyAnalysisCostUSD(ctx, p, log)
	cost := executionCost + analysisCost
	if p.BillingCapUSD > 0 && cost > p.BillingCapUSD {
		cost = p.BillingCapUSD
	}
	cents := int64(math.Round(cost * 100))

	if cents <= 0 {
		// Zero or near-zero combined cost: don't ping Stripe; just
		// rollover. No attempt counted (we did not actually try
		// to charge).
		if rErr := s.Store.ResetExecutionsThisPeriod(
			ctx, p.ProjectID, newStart, newEnd,
		); rErr != nil {
			log.Error("rollover (zero charge) failed", "error", rErr.Error())
			return
		}
		log.Info("hobby period rolled over (no overage or analyses to charge)",
			"new_period_start", newStart, "new_period_end", newEnd)
		return
	}

	// Attempt the off-session charge.
	included := tierExecutionLimit(TierHobby) + p.GrantedExecutions
	if included < 0 {
		included = 0
	}
	overUnits := p.ExecutionsThisPeriod - included
	if overUnits < 0 {
		overUnits = 0
	}

	// Build a human-readable description that covers whatever line
	// items contributed to this period's charge. Single-line-item
	// charges (just executions OR just analyses) get a clean
	// description; combined charges get a "+" join so the Stripe
	// receipt + dashboard show the breakdown.
	desc := buildHobbyChargeDescription(overUnits, analysisCount)

	s.Stripe.applyKey()
	piParams := &stripe.PaymentIntentParams{
		Amount:      stripe.Int64(cents),
		Currency:    stripe.String(string(stripe.CurrencyUSD)),
		Customer:    stripe.String(p.StripeCustomerID),
		Confirm:     stripe.Bool(true),
		OffSession:  stripe.Bool(true),
		Description: stripe.String(desc),
		Metadata: map[string]string{
			"mesedi_project_id":     p.ProjectID,
			"mesedi_tier":           TierHobby,
			"mesedi_overage":        fmt.Sprintf("%d", overUnits),
			"mesedi_ai_analyses":    fmt.Sprintf("%d", analysisCount),
			"mesedi_execution_cost": fmt.Sprintf("%.2f", executionCost),
			"mesedi_analysis_cost":  fmt.Sprintf("%.2f", analysisCost),
		},
	}
	// Idempotency key: project_id + period_end + failure_count so a
	// scheduler restart mid-tick does not double-charge, but each
	// retry attempt at a different failure count is a fresh charge.
	idemKey := fmt.Sprintf("hobby-%s-%d-%d",
		p.ProjectID, p.CurrentPeriodEnd.Unix(), p.HobbyBillingConsecutiveFailures)
	piParams.IdempotencyKey = stripe.String(idemKey)

	pi, piErr := paymentintent.New(piParams)
	attemptAt := time.Now().UTC()

	if piErr != nil {
		// Charge failed. Record the failure, send a dunning email if
		// Resend is wired, and bail (no rollover; we retry next
		// allowed tick).
		if uErr := s.Store.UpdateHobbyBillingState(
			ctx, p.ProjectID, attemptAt, false,
		); uErr != nil {
			log.Error("update billing state (failure) failed", "error", uErr.Error())
		}
		log.Warn("hobby charge failed",
			"error", piErr.Error(),
			"consecutive_failures", p.HobbyBillingConsecutiveFailures+1)
		s.sendDunningEmail(p, p.HobbyBillingConsecutiveFailures+1, cost)

		// Auto-detach if we have crossed the ceiling.
		if p.HobbyBillingConsecutiveFailures+1 >= HobbyBillingFailureCeiling {
			if dErr := s.Store.DetachHobbyCardForBillingFailure(
				ctx, p.ProjectID,
			); dErr != nil {
				log.Error("detach card failed", "error", dErr.Error())
			} else {
				log.Warn("hobby card auto-detached after consecutive failures",
					"failures", p.HobbyBillingConsecutiveFailures+1)
				s.sendCardDetachedEmail(p)
			}
		}
		return
	}

	// Charge succeeded. Record success (resets consecutive_failures
	// to 0), advance period, send receipt email if wired.
	if uErr := s.Store.UpdateHobbyBillingState(
		ctx, p.ProjectID, attemptAt, true,
	); uErr != nil {
		log.Error("update billing state (success) failed", "error", uErr.Error())
	}
	if rErr := s.Store.ResetExecutionsThisPeriod(
		ctx, p.ProjectID, newStart, newEnd,
	); rErr != nil {
		log.Error("rollover (post-success) failed", "error", rErr.Error())
		return
	}
	log.Info("hobby overage charged + period rolled over",
		"amount_cents", cents,
		"payment_intent_id", pi.ID,
		"new_period_start", newStart, "new_period_end", newEnd)
	s.sendReceiptEmail(p, cost, pi.ID)
}

// sendDunningEmail asks the customer to update their card after a
// failed charge. Best-effort: silent no-op if Resend is not wired
// (CE / local dev) or if the project has no owner_email recorded.
// The email is sent on every failure (per the product decision to
// notify on first failure and every retry after that).
func (s *HobbyBillingScheduler) sendDunningEmail(
	p *store.Project, failureCount int, attemptedUSD float64,
) {
	if !s.mailerReady(p) {
		return
	}
	if err := s.Mailer.SendHobbyBillingNotification(context.Background(),
		mail.HobbyBillingNotificationInput{
			Kind:           mail.HobbyBillingNotificationChargeFailed,
			ToEmail:        p.OwnerEmail,
			ProjectName:    p.Name,
			DashboardURL:   s.dashboardURL(),
			AmountUSD:      attemptedUSD,
			FailureCount:   failureCount,
			FailureCeiling: HobbyBillingFailureCeiling,
		},
	); err != nil {
		s.Logger.Warn("hobby dunning email failed",
			"project_id", p.ProjectID, "error", err.Error())
	}
}

// sendCardDetachedEmail tells the customer their card was removed
// after the configured ceiling of consecutive failures.
func (s *HobbyBillingScheduler) sendCardDetachedEmail(p *store.Project) {
	if !s.mailerReady(p) {
		return
	}
	if err := s.Mailer.SendHobbyBillingNotification(context.Background(),
		mail.HobbyBillingNotificationInput{
			Kind:               mail.HobbyBillingNotificationCardDetached,
			ToEmail:            p.OwnerEmail,
			ProjectName:        p.Name,
			DashboardURL:       s.dashboardURL(),
			FailureCeiling:     HobbyBillingFailureCeiling,
			IncludedExecutions: HobbyExecutionLimit,
		},
	); err != nil {
		s.Logger.Warn("hobby card-detached email failed",
			"project_id", p.ProjectID, "error", err.Error())
	}
}

// sendReceiptEmail sends a short confirmation to the customer after
// a successful overage charge.
func (s *HobbyBillingScheduler) sendReceiptEmail(
	p *store.Project, chargedUSD float64, paymentIntentID string,
) {
	if !s.mailerReady(p) {
		return
	}
	if err := s.Mailer.SendHobbyBillingNotification(context.Background(),
		mail.HobbyBillingNotificationInput{
			Kind:            mail.HobbyBillingNotificationReceipt,
			ToEmail:         p.OwnerEmail,
			ProjectName:     p.Name,
			DashboardURL:    s.dashboardURL(),
			AmountUSD:       chargedUSD,
			PaymentIntentID: paymentIntentID,
		},
	); err != nil {
		s.Logger.Warn("hobby receipt email failed",
			"project_id", p.ProjectID, "error", err.Error())
	}
}

// mailerReady is a small guard the three send* helpers share.
func (s *HobbyBillingScheduler) mailerReady(p *store.Project) bool {
	if s.Mailer == nil || !s.Mailer.Enabled() {
		return false
	}
	if p.OwnerEmail == "" {
		return false
	}
	return true
}

// dashboardURL resolves the dashboard base URL for email links,
// falling back to a sensible default when not configured.
func (s *HobbyBillingScheduler) dashboardURL() string {
	if s.DashboardURL != "" {
		return s.DashboardURL
	}
	return "https://app.mesedi.ai"
}

// computeHobbyAnalysisCostUSD counts AI root-cause analyses the
// Hobby project ran since its current period started and returns
// (cost_USD, count). Zero on a DB lookup error so a transient blip
// does NOT result in surprise period-end billing of a number we
// can't trust; the next period's tick will re-count and pick up
// whatever was missed (the analyses themselves stay on the
// failure_groups row indefinitely).
//
// Window selection mirrors HandleAnalyzeFailureGroup: use
// CurrentPeriodStart when set, fall back to 30-day rolling window
// otherwise (race between Stripe checkout and webhook). We count
// per-project, not per-tenant, because Hobby is single-project by
// design.
func (s *HobbyBillingScheduler) computeHobbyAnalysisCostUSD(
	ctx context.Context, p *store.Project, log *slog.Logger,
) (float64, int) {
	since := time.Now().UTC().AddDate(0, -1, 0)
	if p.CurrentPeriodStart != nil {
		since = *p.CurrentPeriodStart
	}
	count, err := s.Store.CountAIAnalysesSincePeriodStart(
		ctx, p.ProjectID, since,
	)
	if err != nil {
		log.Warn("hobby analysis cost count failed (treating as zero)",
			"error", err.Error())
		return 0, 0
	}
	if count <= 0 {
		return 0, 0
	}
	return float64(count) * HobbyAIAnalysisPriceUSD, count
}

// buildHobbyChargeDescription formats the Stripe PaymentIntent
// Description for a Hobby period-end charge. The shape covers the
// three cases that produce a non-zero charge:
//
//   - Executions only          → "Mesedi Hobby overage: N executions x $0.002"
//   - Analyses only            → "Mesedi Hobby AI root-cause: N analyses x $0.75"
//   - Executions AND analyses  → both lines joined with " + "
//
// Kept package-level (not a method) so it's trivially unit-testable
// without standing up a scheduler.
func buildHobbyChargeDescription(overUnits int64, analysisCount int) string {
	var parts []string
	if overUnits > 0 {
		parts = append(parts, fmt.Sprintf(
			"Mesedi Hobby overage: %d executions x $%.3f",
			overUnits, HobbyOveragePriceUSD,
		))
	}
	if analysisCount > 0 {
		parts = append(parts, fmt.Sprintf(
			"Mesedi Hobby AI root-cause: %d analyses x $%.2f",
			analysisCount, HobbyAIAnalysisPriceUSD,
		))
	}
	if len(parts) == 0 {
		// Should never reach here (caller skips zero-cost charges
		// before computing the description), but keep the fallback
		// defensive so a malformed period never sends an empty
		// description string to Stripe.
		return "Mesedi Hobby period charge"
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return parts[0] + " + " + parts[1]
}
