package api

// Per-project data retention scheduler.
//
// Runs as a background goroutine started from cmd/api/main.go. Every
// 24 hours it walks every projects row where retention_days IS NOT
// NULL and deletes executions whose started_at is older than
// (now - retention_days). FK ON DELETE CASCADE handles events,
// failure_groups, and webhook_deliveries owned by those executions.
//
// Indefinite-retention projects (retention_days IS NULL) are
// excluded at the query layer in ListProjectsForRetention so the
// scheduler never even considers them. That's important for
// Enterprise customers who explicitly need full audit history.
//
// Why daily, not hourly: the prune cutoff is measured in days, so
// the worst-case delay between "the row crossed the retention
// window" and "the row was deleted" is 24h. Customers can compare
// that against the value of running the prune more often and
// reach their own conclusion; we picked daily as the sweet spot
// between freshness and DB load.
//
// Logging: each tick emits one summary log line per project showing
// (project_id, retention_days, deleted_count). Operators can grep
// these in Fly logs to verify the scheduler is healthy and to
// reconstruct prune history.

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"mesedi/backend/internal/store"
)

// RetentionScheduler walks every project with a finite retention_days
// and deletes executions outside the window. Daily tick by default.
type RetentionScheduler struct {
	Store  store.Store
	Logger *slog.Logger

	// TickInterval governs how often the prune runs. Default 24h if
	// zero. Tests dial it down to milliseconds.
	TickInterval time.Duration

	once   sync.Once
	cancel context.CancelFunc
}

// Start launches the worker goroutine. Idempotent.
func (s *RetentionScheduler) Start(ctx context.Context) {
	s.once.Do(func() {
		if s.TickInterval == 0 {
			s.TickInterval = 24 * time.Hour
		}
		runCtx, cancel := context.WithCancel(ctx)
		s.cancel = cancel
		go s.run(runCtx)
	})
}

// Shutdown cancels the run context. Safe to call multiple times.
func (s *RetentionScheduler) Shutdown() {
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *RetentionScheduler) run(ctx context.Context) {
	s.Logger.Info("retention_scheduler: started",
		"tick_interval", s.TickInterval.String())

	// First tick on a delay so a brand-new boot doesn't immediately
	// delete data before the operator has finished verifying the
	// deploy. 60-second grace is plenty: the scheduler is idempotent,
	// so missing the first tick costs nothing.
	select {
	case <-ctx.Done():
		s.Logger.Info("retention_scheduler: shutting down before first tick")
		return
	case <-time.After(60 * time.Second):
	}

	s.tick(ctx)

	t := time.NewTicker(s.TickInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			s.Logger.Info("retention_scheduler: shutting down")
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// tick prunes every project with a finite retention window. Errors
// on individual projects are logged but never stop the loop, one
// project's bad state should not freeze prune for the rest.
func (s *RetentionScheduler) tick(ctx context.Context) {
	projects, err := s.Store.ListProjectsForRetention(ctx)
	if err != nil {
		s.Logger.Error("retention_scheduler: list failed",
			"error", err.Error())
		return
	}
	if len(projects) == 0 {
		s.Logger.Debug("retention_scheduler: no projects with finite retention")
		return
	}

	now := time.Now().UTC()
	for _, p := range projects {
		if p.RetentionDays <= 0 {
			// Defensive: should never happen since the WHERE clause
			// excludes NULL and SetProjectRetentionDays validates > 0,
			// but if a row somehow ended up with 0/negative, skip
			// instead of deleting everything.
			s.Logger.Warn("retention_scheduler: skipping non-positive retention",
				"project_id", p.ProjectID,
				"retention_days", p.RetentionDays)
			continue
		}
		cutoff := now.AddDate(0, 0, -p.RetentionDays)

		// Three separate deletes, because the data does not all hang
		// off executions.
		//
		// Until 2026-08-27 this loop called only DeleteExecutionsOlderThan
		// and trusted a comment claiming the cascade covered
		// failure_groups and webhook_deliveries. It does not: both key
		// on project_id, so neither was ever reachable. The observable
		// result in production was executions and events 3 days old
		// while failure_groups and webhook_deliveries sat at 88 days,
		// against a documented 7-day Hobby window. Nothing reported it;
		// it surfaced only when a backup was compared against live.
		//
		// Each delete is independent and a failure in one does not skip
		// the others. Partially pruning is strictly better than
		// abandoning the project's prune entirely, and the alternative
		// is one unlucky table stalling the whole retention promise.
		type pruneStep struct {
			label string
			run   func() (int64, error)
		}
		steps := []pruneStep{
			// executions first: its cascade clears events and
			// execution_failure_groups, so the later deletes have less
			// to do.
			{"executions", func() (int64, error) {
				return s.Store.DeleteExecutionsOlderThan(ctx, p.ProjectID, cutoff)
			}},
			// failure_groups cascades to ai_analyses, which holds
			// model-written analysis of the customer's failures and is
			// the most sensitive content in the prune set.
			{"failure_groups", func() (int64, error) {
				return s.Store.DeleteFailureGroupsOlderThan(ctx, p.ProjectID, cutoff)
			}},
			{"webhook_deliveries", func() (int64, error) {
				return s.Store.DeleteWebhookDeliveriesOlderThan(ctx, p.ProjectID, cutoff)
			}},
		}

		for _, step := range steps {
			deleted, derr := step.run()
			if derr != nil {
				s.Logger.Warn("retention_scheduler: delete failed",
					"project_id", p.ProjectID,
					"table", step.label,
					"retention_days", p.RetentionDays,
					"cutoff", cutoff.Format(time.RFC3339),
					"error", derr.Error())
				continue
			}
			if deleted > 0 {
				s.Logger.Info("retention_scheduler: pruned",
					"project_id", p.ProjectID,
					"table", step.label,
					"retention_days", p.RetentionDays,
					"cutoff", cutoff.Format(time.RFC3339),
					"rows_deleted", deleted)
			}
		}
	}
}
