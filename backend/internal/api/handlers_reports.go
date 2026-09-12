// Reporting endpoints: stats, org rollup, cost by tenant, pricing info.
// Split out of handlers.go on 2026-09-12; every declaration moved verbatim.
package api

import (
	"context"
	"net/http"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/pricing"
	"mesedi/backend/internal/store"
)

// HandleReportCostByTenant returns the calling project's cost
// aggregated by tenant_id over a time window. Query parameters:
//
//   - since   RFC3339 lower bound on executions.started_at (optional)
//   - until   RFC3339 upper bound on executions.started_at (optional)
//   - limit   max rows returned, 1-200, default 25
//
// Executions with NULL tenant_id collapse into a single row with
// tenant_id="" so the dashboard renders "unattributed" cost
// separately rather than silently dropping it. Rows are ordered by
// total cost descending.
//
//	bridges the gap between project-scoped cost_velocity
//
// alerts ("something is expensive") and the SaaS host's customer
// granularity ("Customer X drove $Y of that spend"). The host
// application sets tenant_id on POST /executions; this endpoint reads
// it back.
func (h *Handlers) HandleReportCostByTenant(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	since, err := parseRFC3339Query(r, "since")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid since: "+err.Error())
		return
	}
	until, err := parseRFC3339Query(r, "until")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid until: "+err.Error())
		return
	}
	limit := parseIntQuery(r, "limit", 25, 1, 200)

	rows, err := h.Store.GetCostByTenant(r.Context(), authProjectID, since, until, limit)
	if err != nil {
		h.Logger.Error("get cost by tenant failed",
			"project_id", authProjectID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "report failed: "+err.Error())
		return
	}

	resp := map[string]any{
		"ok":    true,
		"rows":  rows,
		"count": len(rows),
		"limit": limit,
	}
	if !since.IsZero() {
		resp["since"] = since.UTC().Format(time.RFC3339)
	}
	if !until.IsZero() {
		resp["until"] = until.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

// HandleStats returns top-line stat-card numbers for the dashboard:
// total executions, total failure groups, crashed-in-last-24h,
// average duration_ms across completed executions.
//
// Implementation is deliberately simple: a few small COUNT queries.
// When the executions table grows beyond hand-friendly size, we'll
// either cache these or migrate to time-bucketed aggregates.
func (h *Handlers) HandleStats(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	ctx := r.Context()

	totalExecutions, err := h.Store.CountExecutionsByStatusSince(ctx, authProjectID, "", time.Time{})
	if err != nil {
		// Fall back to 0 on individual count failures so the dashboard
		// degrades gracefully rather than 500-ing for one bad query.
		h.Logger.Warn("count total executions failed", "error", err.Error())
	}
	cutoff24h := time.Now().Add(-24 * time.Hour)
	crashed24h, err := h.Store.CountExecutionsByStatusSince(ctx, authProjectID, string(events.StatusCrashed), cutoff24h)
	if err != nil {
		h.Logger.Warn("count crashed-24h failed", "error", err.Error())
	}
	completedAllTime, err := h.Store.CountExecutionsByStatusSince(ctx, authProjectID, string(events.StatusCompleted), time.Time{})
	if err != nil {
		h.Logger.Warn("count completed failed", "error", err.Error())
	}

	groups, err := h.Store.ListFailureGroups(ctx, authProjectID, store.ListFailureGroupsOpts{
		Limit:           1000,
		IncludeResolved: true,
	})
	if err != nil {
		h.Logger.Warn("list failure_groups for stats failed", "error", err.Error())
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                   true,
		"total_executions":     totalExecutions,
		"completed_executions": completedAllTime,
		"crashed_24h":          crashed24h,
		"open_failure_groups":  len(groups),
	})
}

// HandleOrgRollup returns tenant-wide burn rollup across every project
// owned by the same user as the authenticated project.
//
// v0.1 tenant model: a "tenant" is a single user account
// (owner_user_id). Multi-seat organizations come later. The endpoint
// resolves the authenticated project's owner, lists sibling projects,
// and aggregates SUM(estimated_cost_usd) + COUNT(executions) across
// three buckets: month-to-date, prior month, and last 24h.
//
// Auth: any valid project API key. The endpoint returns ALL projects
// belonging to the same owner_user_id, which is fine because each
// project's API key is already proof of access to that user account.
//
// Response shape (frontend renders KPI cards + per-project table):
//
//	{
//	  "owner_user_id": "...",
//	  "project_count": 3,
//	  "mtd": {"total_cost_usd": 12.34, "total_executions": 1234},
//	  "prior_month": {"total_cost_usd": 9.10, "total_executions": 987},
//	  "last_24h": {"total_cost_usd": 0.42, "total_executions": 42},
//	  "projects": [
//	    {"project_id": "...", "name": "...", "tier": "...",
//	     "mtd_cost_usd": 6.20, "mtd_executions": 600,
//	     "prior_month_cost_usd": 4.50, "prior_month_executions": 450},
//	    ...
//	  ]
//	}
func (h *Handlers) HandleOrgRollup(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	ctx := r.Context()

	// Resolve owner_user_id from the authenticated project.
	authProject, err := h.Store.GetProject(ctx, authProjectID)
	if err != nil {
		h.Logger.Error("org rollup: get auth project failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load authenticated project")
		return
	}
	if authProject.OwnerUserID == "" {
		// Some legacy projects have no owner; still return a single-
		// project rollup rather than erroring.
		h.Logger.Warn("org rollup: project has no owner_user_id, returning single-project rollup",
			"project_id", authProjectID)
		writeOrgRollup(w, ctx, h, "", []*store.Project{authProject})
		return
	}

	siblings, err := h.Store.ListProjectsByOwner(ctx, authProject.OwnerUserID)
	if err != nil {
		h.Logger.Error("org rollup: list projects by owner failed",
			"owner_user_id", authProject.OwnerUserID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not enumerate tenant projects")
		return
	}
	if len(siblings) == 0 {
		// Defensive: the auth project should always show up under its
		// own owner_user_id, but if it doesn't, fall back to the
		// authenticated project alone.
		siblings = []*store.Project{authProject}
	}

	writeOrgRollup(w, ctx, h, authProject.OwnerUserID, siblings)
}

// writeOrgRollup aggregates burn across `projects` (assumed to all
// belong to the same owner) and serializes the response. Extracted so
// HandleOrgRollup can take both the normal path and the legacy
// no-owner fallback path with the same body.
func writeOrgRollup(
	w http.ResponseWriter,
	ctx context.Context,
	h *Handlers,
	ownerUserID string,
	projects []*store.Project,
) {
	now := time.Now().UTC()
	startOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	startOfPriorMonth := startOfMonth.AddDate(0, -1, 0)
	cutoff24h := now.Add(-24 * time.Hour)

	type ProjectRollup struct {
		ProjectID            string  `json:"project_id"`
		Name                 string  `json:"name"`
		Tier                 string  `json:"tier"`
		MTDCostUSD           float64 `json:"mtd_cost_usd"`
		MTDExecutions        int     `json:"mtd_executions"`
		PriorMonthCostUSD    float64 `json:"prior_month_cost_usd"`
		PriorMonthExecutions int     `json:"prior_month_executions"`
		Last24hCostUSD       float64 `json:"last_24h_cost_usd"`
		Last24hExecutions    int     `json:"last_24h_executions"`
	}

	rollups := make([]ProjectRollup, 0, len(projects))
	var totalMTDCost, totalPriorCost, total24hCost float64
	var totalMTDExec, totalPriorExec, total24hExec int

	for _, p := range projects {
		pr := ProjectRollup{ProjectID: p.ProjectID, Name: p.Name, Tier: p.Tier}

		mtdCost, mtdExec, err := h.Store.SumExecutionCostByProjectSince(ctx, p.ProjectID, startOfMonth)
		if err != nil {
			h.Logger.Warn("org rollup: mtd sum failed",
				"project_id", p.ProjectID, "error", err.Error())
		} else {
			pr.MTDCostUSD = mtdCost
			pr.MTDExecutions = mtdExec
		}

		// Prior month: SUM between startOfPriorMonth and startOfMonth.
		// SumExecutionCostByProjectSince only has a `since` cutoff, so
		// we compute (since_priorMonth) - (since_thisMonth) to get the
		// prior-month bucket without a new store method.
		priorPlusCurrentCost, priorPlusCurrentExec, err := h.Store.SumExecutionCostByProjectSince(ctx, p.ProjectID, startOfPriorMonth)
		if err != nil {
			h.Logger.Warn("org rollup: prior-month sum failed",
				"project_id", p.ProjectID, "error", err.Error())
		} else {
			pr.PriorMonthCostUSD = priorPlusCurrentCost - pr.MTDCostUSD
			pr.PriorMonthExecutions = priorPlusCurrentExec - pr.MTDExecutions
			if pr.PriorMonthCostUSD < 0 {
				pr.PriorMonthCostUSD = 0
			}
			if pr.PriorMonthExecutions < 0 {
				pr.PriorMonthExecutions = 0
			}
		}

		last24hCost, last24hExec, err := h.Store.SumExecutionCostByProjectSince(ctx, p.ProjectID, cutoff24h)
		if err != nil {
			h.Logger.Warn("org rollup: 24h sum failed",
				"project_id", p.ProjectID, "error", err.Error())
		} else {
			pr.Last24hCostUSD = last24hCost
			pr.Last24hExecutions = last24hExec
		}

		totalMTDCost += pr.MTDCostUSD
		totalMTDExec += pr.MTDExecutions
		totalPriorCost += pr.PriorMonthCostUSD
		totalPriorExec += pr.PriorMonthExecutions
		total24hCost += pr.Last24hCostUSD
		total24hExec += pr.Last24hExecutions

		rollups = append(rollups, pr)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"owner_user_id": ownerUserID,
		"project_count": len(projects),
		"mtd": map[string]any{
			"total_cost_usd":   totalMTDCost,
			"total_executions": totalMTDExec,
		},
		"prior_month": map[string]any{
			"total_cost_usd":   totalPriorCost,
			"total_executions": totalPriorExec,
		},
		"last_24h": map[string]any{
			"total_cost_usd":   total24hCost,
			"total_executions": total24hExec,
		},
		"projects": rollups,
	})
}

// HandleGetPricingInfo returns the backend pricing-table metadata so
// customers can see exactly which models Mesedi prices server-side
// AND when those prices were last reviewed. made the backend
// authoritative for known models; this endpoint is how customers
// verify which models that includes.
//
// Models not in `supported_models` fall back to the SDK-shipped
// per-event estimated_cost_usd at execution close. When that happens,
// a `pricing_unknown_model` audit_event is recorded; the dashboard
// surfaces it via the existing config-fallback chip.
//
// Response:
//
//	{
//	  "pricing_table_version": "2026-06-21",
//	  "supported_models":      ["claude-3-haiku", "claude-3-opus", ...]
//	}
//
// No auth scope beyond project context, the pricing table is the
// same for every project (per Decision 5, tier-agnostic).
func (h *Handlers) HandleGetPricingInfo(w http.ResponseWriter, r *http.Request) {
	if _, ok := ProjectIDFromContext(r.Context()); !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pricing_table_version": pricing.PricingTableVersion,
		"supported_models":      pricing.SupportedModels(),
	})
}
