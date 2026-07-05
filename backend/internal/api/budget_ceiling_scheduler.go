package api

// Tenant budget ceiling scheduler.
//
// Runs as a background goroutine started from cmd/api/main.go. Every
// 5 minutes it walks every row in tenant_budget_ceilings and:
//
//   1. Sums burn-this-month across all projects owned by the tenant
//      (owner_user_id) since the start of the calendar month.
//
//   2. Compares the sum to MonthlyCeilingUSD.
//
//   3. If burn >= ceiling AND breached_at is NULL (no prior breach
//      recorded this month), fire the breach: email + webhook +
//      (when BreachAction == "halt") halt all active executions
//      across all tenant projects.
//
//   4. If burn < ceiling AND breached_at points to a prior month, clear
//      breached_at (month rollover). This lets the same tenant
//      breach again next month without operator intervention.
//
//   5. Always update last_evaluated_at so the dashboard can show
//      "last checked X seconds ago".
//
// Notification dedupe: by setting breached_at and skipping evaluation
// when it's non-nil within the same calendar month, we guarantee one
// email + one webhook per breach event, not one every 5 minutes for
// the rest of the month.
//
// Halt fan-out semantics: for each tenant project we list all
// executions with status="started" and call HaltSubs.TriggerHalt on
// each. The SSE channel delivers the halt to any agent that's still
// alive and connected; agents that already finished or never opened
// the channel are no-ops. We do NOT block on delivery confirmation;
// the halt is fire-and-forget at the SSE layer.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"mesedi/backend/internal/mail"
	"mesedi/backend/internal/store"
)

// BudgetCeilingScheduler ticks every tickInterval and evaluates every
// tenant_budget_ceilings row.
type BudgetCeilingScheduler struct {
	Store        store.Store
	Logger       *slog.Logger
	HaltSubs     *HaltSubscribers
	Mailer       mail.Mailer
	WebhookHTTP  *http.Client
	DashboardURL string

	// TickInterval governs how often the evaluator runs. Default 5
	// minutes if zero. Production override comes from main.go; tests
	// dial it down to milliseconds.
	TickInterval time.Duration

	// Stop closes to signal the worker goroutine to exit. main.go
	// closes this on graceful shutdown.
	Stop <-chan struct{}

	once   sync.Once
	cancel context.CancelFunc
}

// Start launches the worker goroutine. Idempotent: subsequent calls
// after the first are no-ops.
func (s *BudgetCeilingScheduler) Start(ctx context.Context) {
	s.once.Do(func() {
		if s.TickInterval == 0 {
			s.TickInterval = 5 * time.Minute
		}
		runCtx, cancel := context.WithCancel(ctx)
		s.cancel = cancel
		go s.run(runCtx)
	})
}

// Shutdown cancels the run context. Safe to call multiple times.
func (s *BudgetCeilingScheduler) Shutdown() {
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *BudgetCeilingScheduler) run(ctx context.Context) {
	s.Logger.Info("budget_ceiling_scheduler: started",
		"tick_interval", s.TickInterval.String())

	// Tick once immediately so a brand-new ceiling row gets evaluated
	// without waiting 5 minutes.
	s.tick(ctx)

	t := time.NewTicker(s.TickInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			s.Logger.Info("budget_ceiling_scheduler: shutting down")
			return
		case <-s.Stop:
			s.Logger.Info("budget_ceiling_scheduler: stop signal received")
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// tick evaluates every ceiling row exactly once. Logs (does not return)
// per-tenant errors so one tenant's failure doesn't stop the others.
func (s *BudgetCeilingScheduler) tick(ctx context.Context) {
	ceilings, err := s.Store.ListTenantBudgetCeilings(ctx)
	if err != nil {
		s.Logger.Error("budget_ceiling_scheduler: list failed",
			"error", err.Error())
		return
	}
	for _, c := range ceilings {
		s.evaluate(ctx, c)
	}
}

func (s *BudgetCeilingScheduler) evaluate(
	ctx context.Context,
	c *store.TenantBudgetCeiling,
) {
	now := time.Now().UTC()
	startOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	// Month rollover: if a breach was recorded in a prior calendar
	// month, clear it. This lets the same tenant breach again in the
	// new month.
	if c.BreachedAt != nil {
		if c.BreachedAt.Before(startOfMonth) {
			if err := s.Store.SetTenantCeilingBreached(ctx, c.OwnerUserID, nil); err != nil {
				s.Logger.Warn("budget_ceiling_scheduler: clear stale breach failed",
					"owner_user_id", c.OwnerUserID, "error", err.Error())
			} else {
				c.BreachedAt = nil
				s.Logger.Info("budget_ceiling_scheduler: cleared month-rollover breach",
					"owner_user_id", c.OwnerUserID)
			}
		}
	}

	projects, err := s.Store.ListProjectsByOwner(ctx, c.OwnerUserID)
	if err != nil {
		s.Logger.Warn("budget_ceiling_scheduler: list projects failed",
			"owner_user_id", c.OwnerUserID, "error", err.Error())
		return
	}

	var totalBurn float64
	for _, p := range projects {
		cost, _, err := s.Store.SumExecutionCostByProjectSince(ctx, p.ProjectID, startOfMonth)
		if err != nil {
			s.Logger.Warn("budget_ceiling_scheduler: sum cost failed",
				"project_id", p.ProjectID, "error", err.Error())
			continue
		}
		totalBurn += cost
	}

	// Update evaluation timestamp regardless of breach state, so the
	// dashboard's "last checked" indicator stays fresh.
	if err := s.Store.MarkTenantCeilingEvaluated(ctx, c.OwnerUserID, now); err != nil {
		s.Logger.Warn("budget_ceiling_scheduler: mark evaluated failed",
			"owner_user_id", c.OwnerUserID, "error", err.Error())
	}

	// Breach detection. Only fires when we cross from "not breached"
	// to "breached" within the current calendar month.
	if totalBurn < c.MonthlyCeilingUSD || c.BreachedAt != nil {
		return
	}

	s.Logger.Warn("budget_ceiling_scheduler: BREACH",
		"owner_user_id", c.OwnerUserID,
		"burn_usd", totalBurn,
		"ceiling_usd", c.MonthlyCeilingUSD,
		"action", c.BreachAction,
		"project_count", len(projects))

	// Persist the breach moment FIRST so a crash mid-notification
	// doesn't cause duplicate sends on next tick.
	if err := s.Store.SetTenantCeilingBreached(ctx, c.OwnerUserID, &now); err != nil {
		s.Logger.Error("budget_ceiling_scheduler: persist breach failed",
			"owner_user_id", c.OwnerUserID, "error", err.Error())
		// Don't return: still try to notify, the operator will reset
		// breached_at manually if needed.
	}

	s.fireBreachNotifications(ctx, c, projects, totalBurn)

	if c.BreachAction == "halt" {
		s.haltAllTenantExecutions(ctx, c, projects)
	}
}

// fireBreachNotifications sends the email + webhook for one breach.
// Both are best-effort; failure of one does not block the other.
func (s *BudgetCeilingScheduler) fireBreachNotifications(
	ctx context.Context,
	c *store.TenantBudgetCeiling,
	projects []*store.Project,
	totalBurn float64,
) {
	// Email
	if s.Mailer != nil {
		recipient := c.NotifyEmail
		if recipient == "" && len(projects) > 0 {
			// Fall back to first project's owner_email so v0.1 still
			// notifies even when the customer didn't fill in the
			// optional override.
			recipient = projects[0].OwnerEmail
		}
		if recipient != "" {
			if err := s.Mailer.SendBudgetCeilingBreach(ctx, mail.BudgetCeilingBreachInput{
				ToEmail:      recipient,
				BurnUSD:      totalBurn,
				CeilingUSD:   c.MonthlyCeilingUSD,
				BreachAction: c.BreachAction,
				ProjectCount: len(projects),
				DashboardURL: s.DashboardURL,
			}); err != nil {
				s.Logger.Warn("budget_ceiling_scheduler: email send failed",
					"owner_user_id", c.OwnerUserID,
					"recipient", recipient,
					"error", err.Error())
			}
		}
	}

	// Webhook
	if c.NotifyWebhookURL != "" && s.WebhookHTTP != nil {
		payload := map[string]any{
			"event":         "tenant_budget_ceiling.breached",
			"owner_user_id": c.OwnerUserID,
			"burn_usd":      totalBurn,
			"ceiling_usd":   c.MonthlyCeilingUSD,
			"action":        c.BreachAction,
			"project_count": len(projects),
			"breached_at":   time.Now().UTC().Format(time.RFC3339),
			"dashboard_url": s.DashboardURL + "/app/org",
		}
		buf, err := json.Marshal(payload)
		if err != nil {
			s.Logger.Warn("budget_ceiling_scheduler: webhook marshal failed",
				"error", err.Error())
			return
		}
		req, err := http.NewRequestWithContext(ctx, "POST", c.NotifyWebhookURL, bytes.NewReader(buf))
		if err != nil {
			s.Logger.Warn("budget_ceiling_scheduler: webhook request failed",
				"url", c.NotifyWebhookURL, "error", err.Error())
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "mesedi-budget-ceiling-scheduler")
		resp, err := s.WebhookHTTP.Do(req)
		if err != nil {
			s.Logger.Warn("budget_ceiling_scheduler: webhook send failed",
				"url", c.NotifyWebhookURL, "error", err.Error())
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			s.Logger.Warn("budget_ceiling_scheduler: webhook returned error",
				"url", c.NotifyWebhookURL, "status", resp.StatusCode)
		}
	}
}

// haltAllTenantExecutions enumerates every active execution across
// every tenant project and broadcasts SSE halt signals. Fire-and-forget:
// we don't block on agent acknowledgment.
//
// This is the load-bearing piece of the "automatic hard-halt on
// breach" Enterprise tier promise. The SSE channel was built for the
// existing per-execution halt button; we reuse it here at the
// tenant scope.
func (s *BudgetCeilingScheduler) haltAllTenantExecutions(
	ctx context.Context,
	c *store.TenantBudgetCeiling,
	projects []*store.Project,
) {
	if s.HaltSubs == nil {
		s.Logger.Warn("budget_ceiling_scheduler: HaltSubs not wired, skipping halt fan-out",
			"owner_user_id", c.OwnerUserID)
		return
	}

	haltedTotal := 0
	for _, p := range projects {
		execs, err := s.Store.ListActiveExecutionsByProject(ctx, p.ProjectID)
		if err != nil {
			s.Logger.Warn("budget_ceiling_scheduler: list active executions failed",
				"project_id", p.ProjectID, "error", err.Error())
			continue
		}
		for _, e := range execs {
			s.HaltSubs.TriggerHalt(e.ExecutionID, "tenant_budget_ceiling_breached")
			haltedTotal++
		}
	}

	s.Logger.Info("budget_ceiling_scheduler: halt fan-out complete",
		"owner_user_id", c.OwnerUserID,
		"halted_executions", haltedTotal,
		"projects_scanned", len(projects))
}
