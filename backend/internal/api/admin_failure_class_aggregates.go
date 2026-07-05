package api

// Founder LinkedIn-trend aggregate surface.
//
// Two endpoints:
//   - GET  /admin/failure-class-aggregates?k=3&limit=500
//     Returns rows where distinct_tenants_count >= k, ordered
//     period DESC. Default k=3.
//   - POST /admin/failure-class-aggregates/run?period=YYYY-MM
//     Re-aggregates the named period (current month if omitted).
//     Used to refresh the current-month row right before drafting
//     a LinkedIn post, or to backfill a historical month.
//
// A background worker also runs the aggregation daily, see
// StartFailureClassAggregateWorker below.

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mesedi/backend/internal/store"
)

// DefaultKAnonymityThreshold is the privacy floor for the
// failure-class aggregate surface. A row is publishable only when
// it covers at least this many distinct tenants. k=3 trades a bit
// of conservatism for the ability to actually publish trend data
// early in Mesedi's life. Override via the ?k= query param when
// needed.
const DefaultKAnonymityThreshold = 3

// AdminFailureClassAggregatesResponse is the JSON body returned by
// GET /admin/failure-class-aggregates.
type AdminFailureClassAggregatesResponse struct {
	OK         bool                              `json:"ok"`
	K          int                               `json:"k"`
	Aggregates []*store.FailureClassAggregateRow `json:"aggregates"`
}

// HandleAdminListFailureClassAggregates returns the publishable
// rows. Query params: k (k-anonymity threshold, default 3), limit
// (default 500, ceiling 2000).
func (h *Handlers) HandleAdminListFailureClassAggregates(w http.ResponseWriter, r *http.Request) {
	k := DefaultKAnonymityThreshold
	if raw := strings.TrimSpace(r.URL.Query().Get("k")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			k = n
		}
	}
	limit := 500
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 2000 {
		limit = 2000
	}

	rows, err := h.Store.ListFailureClassAggregates(r.Context(), k, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"list failure_class aggregates: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, AdminFailureClassAggregatesResponse{
		OK:         true,
		K:          k,
		Aggregates: rows,
	})
}

// AdminRunAggregationResponse is the JSON body returned by POST
// /admin/failure-class-aggregates/run.
type AdminRunAggregationResponse struct {
	OK          bool   `json:"ok"`
	Period      string `json:"period"`
	RowsWritten int    `json:"rows_written"`
}

// HandleAdminRunFailureClassAggregation re-aggregates the named
// period (default: current UTC month). Idempotent; safe to run
// multiple times. The dashboard surfaces this as a "Refresh now"
// button on the aggregates page.
func (h *Handlers) HandleAdminRunFailureClassAggregation(w http.ResponseWriter, r *http.Request) {
	period := strings.TrimSpace(r.URL.Query().Get("period"))
	if period == "" {
		period = currentMonthPeriod()
	}
	if !validPeriod(period) {
		writeError(w, http.StatusBadRequest,
			"period must be YYYY-MM (e.g. 2026-06)")
		return
	}
	start, end, err := monthBounds(period)
	if err != nil {
		writeError(w, http.StatusBadRequest,
			"could not parse period: "+err.Error())
		return
	}
	written, aerr := h.Store.AggregateFailureClassesForMonth(
		r.Context(), period, start, end,
	)
	if aerr != nil {
		writeError(w, http.StatusInternalServerError,
			"aggregate: "+aerr.Error())
		return
	}
	h.Logger.Info("failure_class aggregation run",
		"period", period, "rows_written", written)
	writeJSON(w, http.StatusOK, AdminRunAggregationResponse{
		OK:          true,
		Period:      period,
		RowsWritten: written,
	})
}

// currentMonthPeriod returns "YYYY-MM" for the current UTC month.
func currentMonthPeriod() string {
	now := time.Now().UTC()
	return now.Format("2006-01")
}

// validPeriod returns true iff p matches YYYY-MM with a real month
// (01..12) and a 4-digit year. Cheap regex-free check.
func validPeriod(p string) bool {
	if len(p) != 7 || p[4] != '-' {
		return false
	}
	if _, err := strconv.Atoi(p[:4]); err != nil {
		return false
	}
	m, err := strconv.Atoi(p[5:])
	if err != nil || m < 1 || m > 12 {
		return false
	}
	return true
}

// monthBounds returns [startInclusive, endExclusive) for the given
// "YYYY-MM" period in UTC. endExclusive is the first instant of the
// next month so the range matches the SQL filter expectations.
func monthBounds(period string) (time.Time, time.Time, error) {
	t, err := time.Parse("2006-01", period)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	return start, end, nil
}

// StartFailureClassAggregateWorker runs the aggregation in the
// background once on startup and then every 24 hours thereafter.
// Re-aggregates the CURRENT month so mid-month account closures do
// not lose data: by the time a cascade-delete fires on close,
// today's aggregate row is already up-to-date.
//
// The goroutine exits when ctx is canceled (typical at SIGTERM).
// Errors are logged but do not stop the loop; a transient DB error
// should not break this worker permanently.
func StartFailureClassAggregateWorker(
	ctx context.Context,
	s store.Store,
	logger *slog.Logger,
) {
	go func() {
		runOnce := func() {
			period := currentMonthPeriod()
			start, end, err := monthBounds(period)
			if err != nil {
				logger.Error("failure_class worker: month bounds failed",
					"period", period, "error", err.Error())
				return
			}
			written, aerr := s.AggregateFailureClassesForMonth(ctx, period, start, end)
			if aerr != nil {
				logger.Warn("failure_class worker: aggregation failed",
					"period", period, "error", aerr.Error())
				return
			}
			logger.Info("failure_class worker: aggregation ran",
				"period", period, "rows_written", written)
		}

		runOnce()

		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runOnce()
			}
		}
	}()
}
