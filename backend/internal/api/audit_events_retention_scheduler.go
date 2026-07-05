package api

// Closed-project audit_events retention scheduler.
//
// Runs as a background goroutine started from cmd/api/main.go. Every
// 24 hours it issues a single DELETE against audit_events whose
// project_deleted_at < (now - retention window). The default retention
// window is 7 years, picked to match SOC 2 / SOX / IRS audit-trail
// norms; configurable via the AuditEventsRetention env-driven knob
// in main.go for environments with different compliance baselines.
//
// Why daily, not hourly:
//
//   The retention window is measured in YEARS. The worst-case delay
//   between "a row crossed the 7-year threshold" and "the row was
//   deleted" is 24 hours. That is plenty for compliance posture and
//   keeps DB pressure light.
//
// Why a single DELETE, not a per-project sweep like the per-project
// retention scheduler:
//
//   The eligibility filter (project_deleted_at IS NOT NULL AND
//   project_deleted_at < cutoff) is a global predicate. One indexed
//   DELETE finishes in a handful of milliseconds even with millions
//   of rows because the idx_audit_events_actor_email_deleted_at
//   composite index from migration 031 is selective on
//   project_deleted_at. No need to fan out per-project.
//
// Why this is safe to run unattended:
//
//   - Live-project audit history is untouched: project_deleted_at IS
//     NULL on every live row by definition (migration 031).
//   - Cutoff is far enough back (7 years) that nothing short of a
//     wall-clock regression can put live rows at risk.
//   - The same SOC 2 reviewer that wants 7-year retention also wants
//     "no PII older than 7 years sitting around". This satisfies both.
//
// GDPR purge lives on a separate endpoint; that is an
// on-demand right-to-be-forgotten path triggered by a customer support
// ticket, NOT this scheduler. The two coexist: GDPR purge wipes one
// project on request, the scheduler wipes anything past the window.

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"mesedi/backend/internal/store"
)

// DefaultAuditEventsRetention is the closed-project audit retention
// window. Matches SOC 2 / financial-services norms. Override via
// AuditEventsRetentionScheduler.Retention to tighten (e.g. EU-only
// deploys may want a shorter window for GDPR proportionality).
const DefaultAuditEventsRetention = 7 * 365 * 24 * time.Hour

// AuditEventsRetentionScheduler walks closed-project audit history
// and prunes rows whose project_deleted_at is older than the
// configured retention window. Daily tick by default.
type AuditEventsRetentionScheduler struct {
	Store  store.Store
	Logger *slog.Logger

	// Retention is how far back closed-project audit rows are kept
	// before being purged. Default DefaultAuditEventsRetention (7
	// years) when zero. Tests dial it down to hours.
	Retention time.Duration

	// TickInterval governs how often the prune runs. Default 24h
	// when zero. Tests dial it down to milliseconds.
	TickInterval time.Duration

	once   sync.Once
	cancel context.CancelFunc
}

// Start launches the worker goroutine. Idempotent.
func (s *AuditEventsRetentionScheduler) Start(ctx context.Context) {
	s.once.Do(func() {
		if s.Retention == 0 {
			s.Retention = DefaultAuditEventsRetention
		}
		if s.TickInterval == 0 {
			s.TickInterval = 24 * time.Hour
		}
		runCtx, cancel := context.WithCancel(ctx)
		s.cancel = cancel
		go s.run(runCtx)
	})
}

// Shutdown cancels the run context. Safe to call multiple times.
func (s *AuditEventsRetentionScheduler) Shutdown() {
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *AuditEventsRetentionScheduler) run(ctx context.Context) {
	s.Logger.Info("audit_events_retention_scheduler: started",
		"retention", s.Retention.String(),
		"tick_interval", s.TickInterval.String())

	// First tick on a delay so a brand-new boot does not immediately
	// delete data before the operator has finished verifying the
	// deploy. 60-second grace mirrors RetentionScheduler.
	select {
	case <-ctx.Done():
		s.Logger.Info("audit_events_retention_scheduler: shutting down before first tick")
		return
	case <-time.After(60 * time.Second):
	}

	s.tick(ctx)

	t := time.NewTicker(s.TickInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			s.Logger.Info("audit_events_retention_scheduler: shutting down")
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// tick fires one DELETE pass. Errors are logged but never stop the
// loop; transient DB issues should self-heal by the next tick.
func (s *AuditEventsRetentionScheduler) tick(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-s.Retention)
	deleted, err := s.Store.DeleteClosedProjectAuditEventsOlderThan(ctx, cutoff)
	if err != nil {
		s.Logger.Warn("audit_events_retention_scheduler: delete failed",
			"cutoff", cutoff.Format(time.RFC3339),
			"retention", s.Retention.String(),
			"error", err.Error())
		return
	}
	if deleted > 0 {
		s.Logger.Info("audit_events_retention_scheduler: pruned closed-project audit rows",
			"cutoff", cutoff.Format(time.RFC3339),
			"retention", s.Retention.String(),
			"rows_deleted", deleted)
	} else {
		s.Logger.Debug("audit_events_retention_scheduler: nothing to prune",
			"cutoff", cutoff.Format(time.RFC3339))
	}
}
