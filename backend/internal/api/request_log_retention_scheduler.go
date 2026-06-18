package api

// Daily retention scheduler for the request_log table (#256). Runs
// as a background goroutine started from cmd/api/main.go. Every 24
// hours it issues a single DELETE against rows whose received_at is
// older than the configured retention window. Default is 90 days,
// picked to cover typical compromise-investigation windows (leaks
// are usually noticed within days to weeks, not months) while
// keeping Neon storage cost bounded.
//
// Why daily, not hourly: the retention window is measured in months.
// Worst-case 24-hour delay between "row crossed 90-day threshold" and
// "row deleted" is acceptable. Hourly would just spread the same
// DELETE work over more, smaller passes.
//
// Why a single DELETE: the eligibility filter (received_at < cutoff)
// is a global predicate. The idx_request_log_received_at index from
// migration 036 makes this an indexed range delete that finishes in
// a handful of milliseconds even at hundreds of millions of rows.

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"mesedi/backend/internal/store"
)

// DefaultRequestLogRetention is the request_log retention window.
const DefaultRequestLogRetention = 90 * 24 * time.Hour

// RequestLogRetentionScheduler walks request_log on a daily tick and
// prunes rows older than the configured retention window.
type RequestLogRetentionScheduler struct {
	Store  store.Store
	Logger *slog.Logger

	// Retention is how far back rows are kept before being purged.
	// Default DefaultRequestLogRetention (90 days) when zero. Tests
	// dial it down to milliseconds.
	Retention time.Duration

	// TickInterval governs how often the prune runs. Default 24h
	// when zero. Tests dial it down.
	TickInterval time.Duration

	once   sync.Once
	cancel context.CancelFunc
}

// Start launches the worker goroutine. Idempotent.
func (s *RequestLogRetentionScheduler) Start(ctx context.Context) {
	s.once.Do(func() {
		if s.Retention == 0 {
			s.Retention = DefaultRequestLogRetention
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
func (s *RequestLogRetentionScheduler) Shutdown() {
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *RequestLogRetentionScheduler) run(ctx context.Context) {
	s.Logger.Info("request_log_retention_scheduler: started",
		"retention", s.Retention.String(),
		"tick_interval", s.TickInterval.String())

	// 60-second grace before the first tick, mirrors the audit-events
	// scheduler. Gives the operator room to verify a fresh deploy
	// before any DELETE fires.
	select {
	case <-ctx.Done():
		s.Logger.Info("request_log_retention_scheduler: shutting down before first tick")
		return
	case <-time.After(60 * time.Second):
	}

	s.tick(ctx)

	t := time.NewTicker(s.TickInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			s.Logger.Info("request_log_retention_scheduler: shutting down")
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// tick fires one DELETE pass. Errors are logged but never stop the
// loop; transient DB issues should self-heal by the next tick.
func (s *RequestLogRetentionScheduler) tick(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-s.Retention)
	deleted, err := s.Store.DeleteRequestLogOlderThan(ctx, cutoff)
	if err != nil {
		s.Logger.Warn("request_log_retention_scheduler: delete failed",
			"cutoff", cutoff.Format(time.RFC3339),
			"retention", s.Retention.String(),
			"error", err.Error())
		return
	}
	if deleted > 0 {
		s.Logger.Info("request_log_retention_scheduler: pruned request log rows",
			"cutoff", cutoff.Format(time.RFC3339),
			"retention", s.Retention.String(),
			"rows_deleted", deleted)
	} else {
		s.Logger.Debug("request_log_retention_scheduler: nothing to prune",
			"cutoff", cutoff.Format(time.RFC3339))
	}
}
