// Admin AI-analysis usage and Anthropic credit reporting.
// Split out of admin.go on 2026-09-12; every declaration moved verbatim.
package api

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"mesedi/backend/internal/anthropic"
	"mesedi/backend/internal/store"
)

// adminHaikuCostPerAnalysisUSD is the per-analysis Anthropic cost
// surfaced on the admin breakdown. Matches the Cap-math number in
// billing.go's TeamAIAnalysisLimit comment (~$0.03 Haiku 4.5
// input+output for a typical failure group). Used to compute the
// estimated operator-cost column so operators can spot heavy
// users whose Anthropic burn outpaces their subscription revenue.
const adminHaikuCostPerAnalysisUSD = 0.03

// AdminAIAnalysesByProjectRow is the response payload row for
// GET /admin/ai-analyses-by-project. Wraps the store row with the
// estimated Anthropic cost so the dashboard can render a single
// table without extra math.
type AdminAIAnalysesByProjectRow struct {
	ProjectID        string  `json:"project_id"`
	Name             string  `json:"name"`
	OwnerEmail       string  `json:"owner_email,omitempty"`
	Tier             string  `json:"tier"`
	TenantID         string  `json:"tenant_id,omitempty"`
	Count            int     `json:"count"`
	EstimatedCostUSD float64 `json:"estimated_cost_usd"`
	// FailureClasses powers the per-row chip filter on the admin
	// dashboard. Distinct failure_class slugs the project
	// ran analyses against during this window. Empty omitted.
	FailureClasses []string `json:"failure_classes,omitempty"`
}

// AdminAIAnalysesByProjectResponse is the JSON body of
// GET /admin/ai-analyses-by-project. Surfaces the per-project
// breakdown plus aggregate totals so the dashboard can render the
// summary chip without a second round trip.
type AdminAIAnalysesByProjectResponse struct {
	Since                 string                        `json:"since"`
	TotalCount            int                           `json:"total_count"`
	TotalEstimatedCostUSD float64                       `json:"total_estimated_cost_usd"`
	Projects              []AdminAIAnalysesByProjectRow `json:"projects"`
}

// HandleAdminAIAnalysesByProject returns the per-project AI
// root-cause analysis breakdown. Default window is the
// start of the current calendar month UTC; override via ?since=
// query param as RFC3339. Used by the founder dashboard to spot
// heavy AI users for billing reconciliation and abuse detection.
func (h *Handlers) HandleAdminAIAnalysesByProject(w http.ResponseWriter, r *http.Request) {
	since := startOfCurrentMonthUTC()
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest,
				"since must be RFC3339 (e.g., 2026-06-01T00:00:00Z): "+err.Error())
			return
		}
		since = t.UTC()
	}

	rows, err := h.Store.ListAIAnalysesUsageByProject(r.Context(), since)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"list ai analyses by project: "+err.Error())
		return
	}

	out := AdminAIAnalysesByProjectResponse{
		Since:    since.Format(time.RFC3339),
		Projects: make([]AdminAIAnalysesByProjectRow, 0, len(rows)),
	}
	for _, r := range rows {
		est := float64(r.Count) * adminHaikuCostPerAnalysisUSD
		out.Projects = append(out.Projects, AdminAIAnalysesByProjectRow{
			ProjectID:        r.ProjectID,
			Name:             r.Name,
			OwnerEmail:       r.OwnerEmail,
			Tier:             r.Tier,
			TenantID:         r.TenantID,
			Count:            r.Count,
			EstimatedCostUSD: est,
			FailureClasses:   r.FailureClasses,
		})
		out.TotalCount += r.Count
		out.TotalEstimatedCostUSD += est
	}
	writeJSON(w, http.StatusOK, out)
}

// startOfCurrentMonthUTC returns the first instant of the current
// UTC calendar month. Used as the default window for the admin
// AI-analyses breakdown so the dashboard "this month" view matches
// what most billing reconciliation flows expect.
func startOfCurrentMonthUTC() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// AdminAnthropicCreditResponse is the JSON body of
// GET /admin/anthropic-credit. Fields are pointer-typed when they
// might legitimately be absent so the dashboard can branch on
// "not configured" vs "no balance recorded yet" without inspecting
// string fields.
//
// Auto-decrement model (redesign): the founder records a
// balance ONCE per top-up. From that snapshot onward, the
// CURRENT balance the dashboard shows is computed as
//
//	current_balance = recorded_balance − spend_since_snapshot
//
// where spend_since_snapshot is the sum of Anthropic Cost Report
// buckets between the snapshot timestamp and now. The recorded
// raw value is still surfaced (CreditBalanceRecordedUSD) for
// transparency and a "as of" disclosure on the UI.
type AdminAnthropicCreditResponse struct {
	// AdminAPIConfigured reflects whether ANTHROPIC_ADMIN_KEY is set.
	// When false, burn-rate + auto-decrement fields are nil because
	// we can't compute them without calling the Cost Report endpoint.
	AdminAPIConfigured bool `json:"admin_api_configured"`

	// CreditBalanceRecordedUSD is the most recently entered raw
	// balance (manual entry; Anthropic does not expose this via API).
	// Nil when no snapshot has been recorded yet.
	CreditBalanceRecordedUSD *float64 `json:"credit_balance_recorded_usd,omitempty"`
	// CreditSnapshotAt is when the founder pasted the recorded
	// balance above. Nil when no snapshot recorded.
	CreditSnapshotAt *string `json:"credit_snapshot_at,omitempty"`
	// CreditActorEmail records who recorded the snapshot. Nil when
	// no snapshot or when actor identity was not captured.
	CreditActorEmail *string `json:"credit_actor_email,omitempty"`
	// CreditNote is the optional free-text reason ("after top-up").
	CreditNote *string `json:"credit_note,omitempty"`

	// CreditSpentSinceSnapshotUSD is the Anthropic-side spend
	// between CreditSnapshotAt and now, pulled from the Cost Report
	// API. Nil when admin API isn't configured or no snapshot exists.
	CreditSpentSinceSnapshotUSD *float64 `json:"credit_spent_since_snapshot_usd,omitempty"`
	// CreditBalanceCurrentUSD is the live displayed balance:
	// CreditBalanceRecordedUSD − CreditSpentSinceSnapshotUSD.
	// Clamped at zero so we never render negative dollars. Nil
	// when either input is missing.
	CreditBalanceCurrentUSD *float64 `json:"credit_balance_current_usd,omitempty"`

	// BurnRateUSDPerDay is the 7-day rolling burn computed from the
	// Anthropic Cost Report endpoint. Nil when admin API isn't
	// configured. Zero is a valid value (no burn in the window).
	BurnRateUSDPerDay *float64 `json:"burn_rate_usd_per_day,omitempty"`
	// TotalSpentLast7DUSD is the raw total over the 7-day window
	// used to compute BurnRateUSDPerDay; surfaced so the dashboard
	// can show "spent $X.XX in the last 7 days" alongside the daily
	// rate.
	TotalSpentLast7DUSD *float64 `json:"total_spent_last_7d_usd,omitempty"`

	// RunwayDays is CreditBalanceCurrentUSD / BurnRateUSDPerDay,
	// computed here so the dashboard renders one number. Nil when
	// either input is missing OR when burn rate is zero (avoid
	// div-by-zero and "infinity days of runway" surprises).
	RunwayDays *float64 `json:"runway_days,omitempty"`

	// DailySpend is the last-14-days per-day spend in USD, oldest
	// first. Powers the inline bar chart on the admin card so the
	// founder can spot a spike at a glance. Empty (omitted)
	// when admin API isn't configured.
	//
	// The final entry is TODAY and carries Estimated=true when
	// today's usage was retrievable. Anthropic's cost report omits
	// the in-progress day entirely, so today is reconstructed from
	// hourly token counts priced with our own rate table.
	DailySpend []AdminDailySpendBucket `json:"daily_spend,omitempty"`

	// TodaySpendUSD is spend so far during the current UTC day, our
	// own computation from token counts rather than a figure
	// Anthropic returned. Nil when the usage report was unavailable.
	//
	// This field is the whole reason the Usage Report path exists.
	// On 2026-08-25 the panel showed "$0.00 / day" while real spend
	// was happening, because every dollar landed on the in-progress
	// day that the cost report refuses to return. A burn widget that
	// cannot see today is a burn widget that cannot catch a runaway
	// on the day it runs away.
	TodaySpendUSD *float64 `json:"today_spend_usd,omitempty"`

	// TodaySpendByModel breaks today's estimate down per model, so a
	// spike can be attributed without opening the Anthropic console.
	TodaySpendByModel map[string]float64 `json:"today_spend_by_model,omitempty"`

	// TodayUnpricedModels lists model ids seen in today's usage that
	// have no explicit entry in the rate table. Non-empty means the
	// estimate used a fallback rate for those models and is
	// therefore approximate, surfaced so the UI can say so rather
	// than presenting a confidently wrong number. The usual cause is
	// Anthropic shipping a new model.
	TodayUnpricedModels []string `json:"today_unpriced_models,omitempty"`
}

// AdminDailySpendBucket is one bar of the 14-day spend chart.
type AdminDailySpendBucket struct {
	Date string  `json:"date"` // YYYY-MM-DD UTC
	USD  float64 `json:"usd"`  // total spend that day
	// Estimated marks a bucket we priced ourselves from token counts
	// rather than one Anthropic returned as dollars. Only ever true
	// for the current in-progress UTC day, see the Usage Report
	// notes in internal/anthropic/admin.go. The UI must render these
	// differently; a partial day that looks settled invites the
	// wrong conclusion about a spend spike.
	Estimated bool `json:"estimated,omitempty"`
}

// HandleAdminGetAnthropicCredit assembles the founder burn-rate
// widget payload. Three pieces:
//  1. Latest recorded balance snapshot (manual entry, infrequent).
//  2. Spend SINCE that snapshot (Anthropic Cost Report). Auto-
//     decrements the displayed balance so the founder rarely has
//     to re-enter anything.
//  3. Last 14 days of daily spend (chart + 7-day burn rate).
//
// Cost Report failures degrade gracefully: the snapshot fields are
// always returned even if Anthropic's API is down, and the
// derived numbers are simply omitted with a logged warning.
func (h *Handlers) HandleAdminGetAnthropicCredit(w http.ResponseWriter, r *http.Request) {
	out := AdminAnthropicCreditResponse{
		AdminAPIConfigured: h.AnthropicAdmin != nil && h.AnthropicAdmin.Configured(),
	}

	// 1. Latest credit-balance snapshot (manual entry).
	snap, err := h.Store.GetLatestAnthropicCreditSnapshot(r.Context())
	if err == nil && snap != nil {
		bal := snap.BalanceUSD
		out.CreditBalanceRecordedUSD = &bal
		ts := snap.SnapshottedAt.UTC().Format(time.RFC3339)
		out.CreditSnapshotAt = &ts
		if snap.ActorEmail != "" {
			email := snap.ActorEmail
			out.CreditActorEmail = &email
		}
		if snap.Note != "" {
			note := snap.Note
			out.CreditNote = &note
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError,
			"load credit snapshot: "+err.Error())
		return
	}

	if !out.AdminAPIConfigured {
		writeJSON(w, http.StatusOK, out)
		return
	}

	now := time.Now().UTC()

	// 2. Spend since snapshot -> auto-decremented current balance.
	if snap != nil {
		report, cerr := h.AnthropicAdmin.GetCostReport(r.Context(),
			snap.SnapshottedAt.UTC(), now)
		if cerr != nil {
			h.Logger.Warn("anthropic cost report (since-snapshot) failed; current balance omitted",
				"error", cerr.Error())
		} else {
			spent := report.TotalUSD
			out.CreditSpentSinceSnapshotUSD = &spent
			current := snap.BalanceUSD - spent
			if current < 0 {
				current = 0 // clamp; recorded balance is now stale.
			}
			out.CreditBalanceCurrentUSD = &current
		}
	}

	// 3. Last 14 days: chart + 7-day burn rate.
	startingAt := now.AddDate(0, 0, -14)
	report14, cerr := h.AnthropicAdmin.GetCostReport(r.Context(), startingAt, now)
	if cerr != nil {
		h.Logger.Warn("anthropic cost report (14-day) failed; chart + burn rate omitted",
			"error", cerr.Error())
	} else {
		// Sort buckets oldest-first so the chart renders left-to-right
		// in chronological order even if Anthropic reorders them.
		buckets := append([]anthropic.DailyCostBucket(nil), report14.DailyBuckets...)
		sort.Slice(buckets, func(i, j int) bool {
			return buckets[i].Date.Before(buckets[j].Date)
		})

		out.DailySpend = make([]AdminDailySpendBucket, 0, len(buckets)+1)
		for _, b := range buckets {
			out.DailySpend = append(out.DailySpend, AdminDailySpendBucket{
				Date: b.Date.Format("2006-01-02"),
				USD:  b.USD,
			})
		}

		// Today, from the Usage Report. The cost report above cannot
		// return the in-progress UTC day at all, so without this the
		// chart's newest bar is yesterday and the burn widget is blind
		// to a runaway happening right now. Priced from token counts
		// against our own rate table, hence Estimated=true.
		//
		// A failure here degrades to "no today bar" rather than failing
		// the endpoint: settled history is still worth showing, and
		// this is the newer of the two Anthropic calls.
		startOfTodayUTC := time.Date(
			now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC,
		)
		todayUsage, uerr := h.AnthropicAdmin.GetUsageCost(
			r.Context(), startOfTodayUTC, now,
		)
		switch {
		case uerr != nil:
			h.Logger.Warn("anthropic usage report (today) failed; today's spend omitted",
				"error", uerr.Error())
		case todayUsage != nil:
			todaySpend := todayUsage.TotalUSD
			out.TodaySpendUSD = &todaySpend
			if len(todayUsage.ByModel) > 0 {
				out.TodaySpendByModel = todayUsage.ByModel
			}
			out.TodayUnpricedModels = todayUsage.UnpricedModels
			if len(todayUsage.UnpricedModels) > 0 {
				h.Logger.Warn("usage report contained models with no explicit rate; today's estimate uses the fallback rate",
					"models", strings.Join(todayUsage.UnpricedModels, ","))
			}
			out.DailySpend = append(out.DailySpend, AdminDailySpendBucket{
				Date:      startOfTodayUTC.Format("2006-01-02"),
				USD:       todaySpend,
				Estimated: true,
			})
		}

		// 7-day burn rate from the LAST 7 buckets (newest end). If
		// the response had fewer than 7 buckets (e.g., a brand new
		// org), divide by however many we got so we don't under-
		// estimate the rate.
		windowSize := 7
		if len(buckets) < windowSize {
			windowSize = len(buckets)
		}
		var last7Total float64
		if windowSize > 0 {
			last7 := buckets[len(buckets)-windowSize:]
			for _, b := range last7 {
				last7Total += b.USD
			}
			total := last7Total
			out.TotalSpentLast7DUSD = &total
			rate := last7Total / float64(windowSize)
			out.BurnRateUSDPerDay = &rate
			// Runway: use the auto-decremented current balance when
			// available, otherwise fall back to the recorded value.
			var bal float64
			var have bool
			switch {
			case out.CreditBalanceCurrentUSD != nil:
				bal = *out.CreditBalanceCurrentUSD
				have = true
			case out.CreditBalanceRecordedUSD != nil:
				bal = *out.CreditBalanceRecordedUSD
				have = true
			}
			if have && rate > 0 {
				days := bal / rate
				out.RunwayDays = &days
			}
		}
	}

	writeJSON(w, http.StatusOK, out)
}

// adminCreateCreditSnapshotRequest is the POST body shape.
type adminCreateCreditSnapshotRequest struct {
	// BalanceUSD is the dollar amount the founder pasted from the
	// Anthropic Console sidebar. Required. Must be >= 0.
	BalanceUSD float64 `json:"balance_usd"`
	// ActorEmail is who recorded the snapshot. Optional but
	// recommended so the audit trail is human-readable.
	ActorEmail string `json:"actor_email,omitempty"`
	// Note is an optional reason ("after $100 top-up"). Free-form.
	Note string `json:"note,omitempty"`
}

// HandleAdminCreateAnthropicCreditSnapshot inserts a new manual
// credit-balance snapshot. One row per call; the GET endpoint
// always reads the most recent. History is preserved so a future
// "snapshot history" page can chart balance over time.
func (h *Handlers) HandleAdminCreateAnthropicCreditSnapshot(w http.ResponseWriter, r *http.Request) {
	var body adminCreateCreditSnapshotRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.BalanceUSD < 0 {
		writeError(w, http.StatusBadRequest, "balance_usd must be >= 0")
		return
	}
	// Sanity ceiling: nobody is going to manually enter $1M+, and a
	// typo there would scramble runway math.
	const maxBalance = 100_000.0
	if body.BalanceUSD > maxBalance {
		writeError(w, http.StatusBadRequest,
			"balance_usd above $100,000 looks like a typo; double-check the value")
		return
	}

	snap := &store.AnthropicCreditSnapshot{
		SnapshotID:    newCreditSnapshotID(),
		BalanceUSD:    body.BalanceUSD,
		SnapshottedAt: time.Now().UTC(),
		ActorEmail:    strings.TrimSpace(body.ActorEmail),
		Note:          strings.TrimSpace(body.Note),
	}
	if err := h.Store.CreateAnthropicCreditSnapshot(r.Context(), snap); err != nil {
		writeError(w, http.StatusInternalServerError,
			"persist credit snapshot: "+err.Error())
		return
	}
	h.Logger.Info("anthropic credit snapshot recorded",
		"snapshot_id", snap.SnapshotID,
		"balance_usd", snap.BalanceUSD,
		"actor_email", snap.ActorEmail)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"snapshot_id": snap.SnapshotID,
		"balance_usd": snap.BalanceUSD,
	})
}

// newCreditSnapshotID produces a "credit_<32 hex>" identifier
// matching the prefix-plus-random pattern used elsewhere in Mesedi.
// crypto/rand fallback to nanosecond hex is safe here because
// snapshots are low-volume admin-only writes.
func newCreditSnapshotID() string {
	return "credit_" + newAuditEventID()[len("audit_"):]
}

// AdminListAIAnalysesResponse is the JSON body of
// GET /admin/ai-analyses. One row per Anthropic call ever made by
// the analyze handler. Sorted recent-first; paginated via limit +
// offset query params.
type AdminListAIAnalysesResponse struct {
	OK       bool                `json:"ok"`
	Analyses []*store.AIAnalysis `json:"analyses"`
	Limit    int                 `json:"limit"`
	Offset   int                 `json:"offset"`
}

// HandleAdminListAIAnalyses returns a flat cross-tenant list of
// every analysis, newest first. Default limit 100 with a hard
// ceiling of 500 so a runaway pagination loop on the frontend
// can't pull a giant payload.
func (h *Handlers) HandleAdminListAIAnalyses(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}
	offset := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("offset")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			offset = n
		}
	}
	analyses, err := h.Store.ListAIAnalyses(r.Context(), limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"list ai analyses: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, AdminListAIAnalysesResponse{
		OK:       true,
		Analyses: analyses,
		Limit:    limit,
		Offset:   offset,
	})
}

// HandleAdminGetAIAnalysesTotals returns lifetime + this-month
// summary stats for the founder tile at the top of /admin/ai-analyses.
func (h *Handlers) HandleAdminGetAIAnalysesTotals(w http.ResponseWriter, r *http.Request) {
	totals, err := h.Store.GetAIAnalysesTotals(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"ai analyses totals: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, totals)
}

// AdminProjectAIAnalysesDetailRow is one analyzed failure group on
// the per-project breakdown surface. Trims store.FailureGroup
// down to the columns the admin actually needs to spot heavy users
// and reconcile Anthropic spend.
type AdminProjectAIAnalysesDetailRow struct {
	GroupID            string  `json:"group_id"`
	FailureClass       string  `json:"failure_class"`
	Signature          string  `json:"signature"`
	EventCount         int     `json:"event_count"`
	AffectedExecutions int     `json:"affected_executions"`
	AnalyzedAt         string  `json:"analyzed_at"`
	AnalysisModel      string  `json:"analysis_model,omitempty"`
	EstimatedCostUSD   float64 `json:"estimated_cost_usd"`
	LastSeen           string  `json:"last_seen"`
}

// AdminProjectAIAnalysesDetailResponse is the JSON body of
// GET /admin/projects/{id}/ai-analyses-detail. Surfaces the per-group
// breakdown plus this-project's aggregate totals so the dashboard
// can render an inset summary inside the expanded row without a
// second round trip.
type AdminProjectAIAnalysesDetailResponse struct {
	ProjectID             string                            `json:"project_id"`
	Since                 string                            `json:"since"`
	TotalCount            int                               `json:"total_count"`
	TotalEstimatedCostUSD float64                           `json:"total_estimated_cost_usd"`
	Groups                []AdminProjectAIAnalysesDetailRow `json:"groups"`
}

// HandleAdminProjectAIAnalysesDetail returns the analyzed failure
// groups for one project since the given window. The default
// window matches the parent admin AI-analyses page (start of current
// UTC month) so the expanded row's totals add up to the table row.
func (h *Handlers) HandleAdminProjectAIAnalysesDetail(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "project_id path parameter required")
		return
	}
	since := startOfCurrentMonthUTC()
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest,
				"since must be RFC3339 (e.g., 2026-06-01T00:00:00Z): "+err.Error())
			return
		}
		since = t.UTC()
	}

	groups, err := h.Store.ListAnalyzedFailureGroupsByProject(r.Context(), projectID, since, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"list analyzed failure groups: "+err.Error())
		return
	}

	out := AdminProjectAIAnalysesDetailResponse{
		ProjectID: projectID,
		Since:     since.Format(time.RFC3339),
		Groups:    make([]AdminProjectAIAnalysesDetailRow, 0, len(groups)),
	}
	for _, g := range groups {
		row := AdminProjectAIAnalysesDetailRow{
			GroupID:            g.GroupID,
			FailureClass:       g.FailureClass,
			Signature:          g.Signature,
			EventCount:         g.EventCount,
			AffectedExecutions: g.AffectedExecutions,
			EstimatedCostUSD:   adminHaikuCostPerAnalysisUSD,
			LastSeen:           g.LastSeen.UTC().Format(time.RFC3339),
		}
		if g.AnalyzedAt != nil {
			row.AnalyzedAt = g.AnalyzedAt.UTC().Format(time.RFC3339)
		}
		if g.AnalysisModel != nil {
			row.AnalysisModel = *g.AnalysisModel
		}
		out.Groups = append(out.Groups, row)
		out.TotalCount++
		out.TotalEstimatedCostUSD += adminHaikuCostPerAnalysisUSD
	}
	writeJSON(w, http.StatusOK, out)
}
