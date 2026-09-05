package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"mesedi/backend/internal/api"
	"mesedi/backend/internal/mail"
	"mesedi/backend/internal/store"
)

// startBackgroundWorkers starts every long-running worker the API process
// owns. It was lifted out of main() unchanged: main() had grown past 1000
// lines and roughly an eighth of it was this one responsibility, deciding
// which periodic jobs run and with what dependencies. That is a question
// worth being able to answer by opening one file.
//
// ctx is a parameter rather than a context.Background() baked in here, so
// the lifetime is the caller's decision and a test can stop these workers.
// main() passes context.Background(), which is the behaviour these workers
// have always had: they run for the life of the process and the OS kills
// them on SIGTERM along with the HTTP server. That is adequate today
// because every worker's unit of work is either idempotent on retry or
// transactional, so an abrupt death costs at most one tick. If a worker is
// ever added for which a half-finished tick is NOT recoverable, this
// signature is already the place to thread a real shutdown context through,
// and that worker must not be added until it is.
//
// Adding a worker here rather than in main() is deliberate: it keeps the
// full inventory in one place, so "what runs in the background?" has a
// single answer instead of being scattered through 1000 lines of setup.
func startBackgroundWorkers(
	ctx context.Context,
	st store.Store,
	handlers *api.Handlers,
	mailer mail.Mailer,
	dashboardURL string,
	logger *slog.Logger,
) {
	// Abuse-detection background worker. Reads unresolved signals every
	// few minutes, sends the 24h-warning email, then auto-suspends the
	// project 24h later if still unresolved.
	api.StartAbuseWorker(ctx, st, mailer, logger, dashboardURL)

	// failure_class aggregate worker. Runs once on startup + every 24h,
	// refreshing the current-month row so mid-month account closures
	// don't lose data.
	api.StartFailureClassAggregateWorker(ctx, st, logger)

	// Tenant budget-ceiling scheduler. Walks every tenant_budget_ceilings
	// row every 5 minutes, evaluates MTD burn against the configured
	// ceiling, and (on first crossing within the calendar month) fires
	// email + webhook notifications and (when BreachAction == "halt")
	// halts every active execution under the tenant.
	budgetCeilingScheduler := &api.BudgetCeilingScheduler{
		Store:        st,
		Logger:       logger,
		HaltSubs:     handlers.HaltSubs,
		Mailer:       mailer,
		WebhookHTTP:  &http.Client{Timeout: 10 * time.Second},
		DashboardURL: dashboardURL,
	}
	budgetCeilingScheduler.Start(ctx)

	// Data retention scheduler. Daily tick walks every project with
	// retention_days set and prunes executions outside the window.
	// Indefinite-retention projects are excluded at the query level
	// (ListProjectsForRetention).
	retentionScheduler := &api.RetentionScheduler{
		Store:  st,
		Logger: logger,
	}
	retentionScheduler.Start(ctx)

	// Closed-project audit_events retention scheduler. Daily tick prunes
	// audit rows whose project_deleted_at is older than the 7-year
	// SOC 2 / financial-services retention window. Only closed-project
	// rows (project_deleted_at IS NOT NULL) are eligible; live-project
	// audit history is untouched. The default retention window comes from
	// api.DefaultAuditEventsRetention; override here for stricter
	// (e.g. EU-only) deploys.
	auditEventsRetentionScheduler := &api.AuditEventsRetentionScheduler{
		Store:  st,
		Logger: logger,
	}
	auditEventsRetentionScheduler.Start(ctx)

	// request_log retention scheduler. Daily tick prunes rows older than
	// 90 days from the request_log table. Keeps the forensic-attribution
	// window long enough for typical compromise investigations without
	// letting the table balloon Neon storage.
	requestLogRetentionScheduler := &api.RequestLogRetentionScheduler{
		Store:  st,
		Logger: logger,
	}
	requestLogRetentionScheduler.Start(ctx)

	// Hobby billing scheduler. Daily tick walks every Hobby-tier project
	// whose billing period has rolled over, attempts the off-session
	// overage charge against the saved Stripe payment method, and
	// advances the period bounds on success. Retries every 48 hours on
	// failed charges; auto-detaches the saved card after 5 consecutive
	// failures. Also bootstraps NULL period bounds on existing Hobby
	// projects on first sight. No-op when Stripe is not configured
	// (CE / local dev).
	hobbyBillingScheduler := &api.HobbyBillingScheduler{
		Store:        st,
		Stripe:       handlers.Stripe,
		Mailer:       mailer,
		DashboardURL: dashboardURL,
		Logger:       logger,
	}
	hobbyBillingScheduler.Start(ctx)
	// #366 admin trigger endpoint reaches back to the scheduler via
	// handlers.HobbyBillingScheduler.
	handlers.HobbyBillingScheduler = hobbyBillingScheduler

	startCheckpointChain(ctx, st, logger)
}

// startCheckpointChain starts the hourly checkpoint scheduler, which seals
// executions, closes each elapsed interval into a checkpoint over one leaf
// per project, and anchors that checkpoint's hash in a public transparency
// log through Verdifax.
//
// STRICTLY OPT-IN, AND THAT IS THE POINT. With no Verdifax base URL and key
// configured, NewVerdifaxAnchorer returns nil and the scheduler is NOT
// STARTED AT ALL.
//
// Not started, rather than started with a nil anchorer, because a scheduler
// with nowhere to anchor still builds a genesis checkpoint and then stalls.
// That would leave an orphan un-anchored checkpoint in the database of
// every deployment that never configured Verdifax, including self-hosters
// , and an un-anchored chain is worth nothing. Building one with no
// prospect of anchoring is noise that looks like a half-working feature.
//
// It also spends money. Once VERDIFAX_LEDGER_MODE=rekor on the Verdifax
// side, every interval is a real Sigstore submission. A worker that starts
// spending the moment it is deployed is not a decision anyone made
// deliberately, so it takes two explicit environment variables to turn on.
func startCheckpointChain(ctx context.Context, st store.Store, logger *slog.Logger) {
	baseURL := os.Getenv("VERDIFAX_BASE_URL")

	anchorer := api.NewVerdifaxAnchorer(baseURL, os.Getenv("VERDIFAX_API_KEY"), logger)
	if anchorer == nil {
		// Info, not Warn. For most deployments this is the correct and
		// intended state, and crying wolf here would train operators to
		// ignore the log line that matters.
		logger.Info("checkpoint chain disabled",
			"reason", "VERDIFAX_BASE_URL and VERDIFAX_API_KEY are not both set",
			"effect", "executions are recorded and analysed as normal, but no "+
				"tamper-evident chain is built and nothing is anchored",
		)
		return
	}

	checkpointScheduler := &api.CheckpointScheduler{
		Store:    st,
		Logger:   logger,
		Anchorer: anchorer,
	}
	checkpointScheduler.Start(ctx)

	// The API key is deliberately absent from this line. The base URL is
	// operationally useful (which Verdifax is this pointed at?); the key
	// is a credential and never belongs in a log.
	logger.Info("checkpoint chain enabled",
		"verdifax_base_url", baseURL,
		"checkpoint_interval", api.CheckpointInterval.String(),
	)
}
