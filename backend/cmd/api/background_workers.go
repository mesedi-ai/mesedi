package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
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
	// The smoke-harness admin trigger reaches back to the scheduler via
	// handlers.HobbyBillingScheduler.
	handlers.HobbyBillingScheduler = hobbyBillingScheduler

	startCheckpointChain(ctx, st, logger)
}

// startCheckpointChain starts the hourly checkpoint scheduler, which seals
// executions, closes each elapsed interval into a checkpoint over one leaf
// per project, signs that checkpoint with the deployment's own key and
// anchors it on Sigstore's public Rekor log (api.DirectRekorAnchorer).
//
// STRICTLY OPT-IN, AND THAT IS THE POINT. With no signing key in
// MESEDI_ANCHOR_SIGNING_KEY the scheduler is NOT STARTED AT ALL, which is
// the default for a self-hosted backend. Not started, rather than started
// with nowhere to anchor, because a scheduler with no anchorer still
// builds a genesis checkpoint and then stalls, leaving an orphan
// unanchored checkpoint that looks like a half-working feature.
//
// It also publishes. Every interval becomes a permanent public log entry
// signed with the configured key, so it takes a deliberate secret to turn
// on. MESEDI_REKOR_URL overrides the log address; empty means Sigstore's
// public log. Until 2026-11-16 anchoring went through a third-party
// service configured by VERDIFAX_BASE_URL and VERDIFAX_API_KEY; those
// settings are no longer read.
func startCheckpointChain(ctx context.Context, st store.Store, logger *slog.Logger) {
	keyPEM := strings.TrimSpace(os.Getenv("MESEDI_ANCHOR_SIGNING_KEY"))
	if keyPEM == "" {
		// Info, not Warn. For most deployments this is the correct and
		// intended state, and crying wolf here would train operators to
		// ignore the log line that matters.
		logger.Info("checkpoint chain disabled",
			"reason", "MESEDI_ANCHOR_SIGNING_KEY is not set",
			"effect", "executions are recorded and analysed as normal, but no "+
				"tamper-evident chain is built and nothing is anchored",
		)
		return
	}
	// The anchorer is nil-checked as its concrete type before it reaches the
	// scheduler's interface field: a typed nil pointer in an interface is
	// non-nil, and would start a scheduler that panics on its first anchor.
	anchorer, err := api.NewDirectRekorAnchorer(os.Getenv("MESEDI_REKOR_URL"), []byte(keyPEM), logger)
	if err != nil || anchorer == nil {
		// Error, and not started: a configured key that does not parse is
		// a deployment mistake someone must see. The error names what is
		// wrong with the key and never contains it.
		logger.Error("checkpoint chain disabled", "reason", fmt.Sprint(err))
		return
	}

	// The second anchor. MESEDI_OTS_CALENDARS lists calendars (comma
	// separated); empty uses the public defaults, "off" turns it off. A bad
	// list stops nothing: it is logged and the chain runs without it.
	calendars, calErr := api.ParseOTSCalendars(os.Getenv("MESEDI_OTS_CALENDARS"))
	if calErr != nil {
		logger.Error("timestamping disabled", "reason", calErr.Error())
	}
	var timestamper *api.CheckpointTimestamper
	if len(calendars) > 0 {
		timestamper = &api.CheckpointTimestamper{Store: st, Calendars: calendars, Logger: logger}
	}

	checkpointScheduler := &api.CheckpointScheduler{
		Store:       st,
		Logger:      logger,
		Anchorer:    anchorer,
		Timestamper: timestamper,
	}
	checkpointScheduler.Start(ctx)

	// The public key's fingerprint, never the key: it is what the verifier
	// pins and what the public pages publish, so an operator can confirm
	// the deployment signs with the key they expect.
	logger.Info("checkpoint chain enabled",
		"rekor_url", anchorer.RekorURL,
		"signing_key_sha256", api.AnchorKeyFingerprint(&anchorer.Key.PublicKey),
		"checkpoint_interval", api.CheckpointInterval.String(),
		"timestamp_calendars", len(calendars),
	)
}
