package api

// Mesedi savings (#265): translate the dashboard's failure-group data
// into "dollars Mesedi saved you this month" so a CFO can justify the
// subscription without doing the math themselves.
//
// The fundamental challenge: savings are counterfactuals. We're
// estimating cost AVOIDED, which means modeling "what would have
// happened without Mesedi." That's inherently speculative. The
// counter to speculation is transparency: every bucket below carries
// a human-readable assumption string that the dashboard surfaces in
// a tooltip. Customers can disagree with the numbers and still see
// the methodology — vs. a black-box "you saved $X" claim that gets
// dismissed as marketing.
//
// v1 scope: month-to-date savings on five buckets:
//
//   1. Loop catch — loops:* failure groups. Without Mesedi, an
//      identical-call loop continues until a human notices, typically
//      hours. We assume 1 additional hour of runaway at the loop's
//      observed call rate.
//
//   2. Drift catch — drift:* failure groups. A model-mix shift
//      (e.g., Haiku silently routed to Sonnet) costs ~5x more per
//      token. We project 1 hour of drift at 5x cost multiplier.
//
//   3. First-occurrence escalation — every failure_group whose
//      webhook fired on call #1 means the customer didn't pay for
//      the next N occurrences. v1 assumes N=10 (conservative; a
//      typical "fail loud and human-notice" cycle in production is
//      tens to hundreds of occurrences).
//
//   4. Budget ceiling — if a tenant ceiling is configured AND
//      breached_at is set, project remaining-month burn at current
//      rate minus the ceiling. That's the spend the customer didn't
//      take because Mesedi halted.
//
//   5. Hard-halt — v1 returns $0 with a "tracking improvement
//      pending" assumption string. Currently halts aren't a discrete
//      execution status, so the data shape doesn't support a clean
//      query. When status='halted' lands, swap in the real
//      computation.
//
// Numbers are intentionally conservative: it's better to under-
// promise the ROI than to overstate it and lose credibility on the
// first slide. The dashboard renders "Mesedi paid for itself Nx
// this month" using subscription_cost_usd as the denominator. For
// Hobby (subscription = $0), the ratio is undefined and the UI
// shows the absolute saved amount only.

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"mesedi/backend/internal/store"
)

// ─────────────────────────────────────────────────────────────
// Assumption constants. Tune these in one place; the assumption
// strings on each bucket reference these values so docs stay
// in sync with code.
// ─────────────────────────────────────────────────────────────

const (
	// firstOccurrenceMultiplier is N in "if Mesedi hadn't caught it,
	// the same failure would have recurred N more times before a
	// human noticed and fixed it." Conservative because most teams
	// run agents in fire-and-forget production for hours before
	// noticing a regression in their cost dashboard.
	firstOccurrenceMultiplier = 10.0

	// loopRunawayHours is how long an identical-call loop runs before
	// human detection. 1 hour is conservative for an agent calling
	// every few seconds; in practice the founder dogfood account
	// caught the first loop in ~6 hours before Mesedi existed.
	loopRunawayHours = 1.0

	// driftCostMultiplier reflects the typical price-per-token jump
	// when an agent silently moves model. Haiku 4.5 → Sonnet 4.6 is
	// roughly 5x ($1/1M → $3/1M input + $5/1M → $15/1M output).
	driftCostMultiplier = 5.0

	// driftDetectionHours is wall-clock until the drift would have
	// been noticed by a human via the cost dashboard. 1 hour is
	// conservative for a model-mix shift on a moderate-volume agent.
	driftDetectionHours = 1.0

	// subscriptionCostHobby is what a Hobby customer pays. ROI
	// multiple is undefined for $0 subscription; the UI handles that
	// case by showing absolute saved $ only.
	subscriptionCostHobby float64 = 0

	// subscriptionCostTeamMonthly is what a Team customer pays per
	// month. Keep this in sync with the pricing page; when pricing
	// changes, update this constant.
	subscriptionCostTeamMonthly float64 = 99

	// subscriptionCostEnterpriseMonthly is the placeholder monthly
	// rate used in ROI math for Enterprise customers. Real Enterprise
	// pricing is contract-driven; the savings card only uses this as
	// a "what an Enterprise customer plausibly pays" baseline. Keep
	// generous so ROI ratios stay meaningful.
	subscriptionCostEnterpriseMonthly float64 = 25000.0 / 12.0
)

// ─────────────────────────────────────────────────────────────
// Response types
// ─────────────────────────────────────────────────────────────

// savingsBucket is one line in the savings card. count is how many
// underlying signals fed the calculation (failure groups, breaches,
// etc.); the UI shows it as supporting evidence ("based on 8 caught
// loops").
type savingsBucket struct {
	USD        float64 `json:"usd"`
	Count      int     `json:"count"`
	Assumption string  `json:"assumption"`
}

type savingsResponse struct {
	OK                  bool                     `json:"ok"`
	PeriodStart         string                   `json:"period_start"`
	PeriodEnd           string                   `json:"period_end"`
	Buckets             map[string]savingsBucket `json:"buckets"`
	TotalUSD            float64                  `json:"total_usd"`
	SubscriptionCostUSD float64                  `json:"subscription_cost_usd"`
	// ROIMultiple is total_usd / subscription_cost_usd. Capped at
	// 999 to avoid Inf when subscription is 0 (Hobby). The frontend
	// treats 999 as "infinity / hobby tier" and renders accordingly.
	ROIMultiple float64 `json:"roi_multiple"`
	// Tier is echoed so the frontend can render different copy for
	// Hobby (no ratio shown) vs. Pro / Enterprise.
	Tier string `json:"tier"`
}

// HandleSavings is the GET /me/savings handler. Computes month-to-
// date savings across the five buckets above for the auth project.
// Read-only, never gated (read role is sufficient).
func (h *Handlers) HandleSavings(w http.ResponseWriter, r *http.Request) {
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	ctx := r.Context()
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	// Load project for tier (drives subscription_cost denominator).
	project, err := h.Store.GetProject(ctx, projectID)
	if err != nil {
		h.Logger.Error("savings: get project failed",
			"project_id", projectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load project")
		return
	}

	// Load all failure groups for the project. The cost-wasted column
	// is the seed data for the loop and drift buckets. Pagination
	// not relevant here: even an enormously failure-rich account
	// has at most a few hundred groups per month.
	failureGroups, err := h.Store.ListFailureGroups(ctx, projectID, "", 1000, 0)
	if err != nil {
		h.Logger.Error("savings: list failure groups failed",
			"project_id", projectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load failure groups")
		return
	}

	// Filter to groups whose last_seen is within the current month.
	// A failure group from last month with no new occurrences this
	// month doesn't represent ongoing savings.
	groupsThisMonth := make([]*store.FailureGroup, 0, len(failureGroups))
	for _, g := range failureGroups {
		if g.LastSeen.After(monthStart) || g.LastSeen.Equal(monthStart) {
			groupsThisMonth = append(groupsThisMonth, g)
		}
	}

	// ─────────────────────────────────────────────────────────────
	// Bucket: loop catch
	// ─────────────────────────────────────────────────────────────
	// For each loop failure group caught this month, project what
	// the unchecked runaway would have cost. We use the group's
	// observed cost-per-affected-execution as the per-call cost
	// and extrapolate over the loopRunawayHours window at the
	// observed event rate.
	var loopUSD float64
	var loopCount int
	for _, g := range groupsThisMonth {
		if !strings.HasPrefix(g.FailureClass, "loops") {
			continue
		}
		loopCount++
		// Conservative: assume the loop would have run for loopRunawayHours
		// more at the same rate. Saved = current_cost_per_event ×
		// event_rate_per_hour × runaway_hours.
		costPerEvent := 0.0
		if g.CostWastedUSD != nil && g.EventCount > 0 {
			costPerEvent = *g.CostWastedUSD / float64(g.EventCount)
		}
		// event_rate_per_hour: events over the group's observed
		// window (last_seen - first_seen). If the window is too
		// short to measure (< 60s), default to 1 event/min as a
		// safe lower bound.
		windowHours := g.LastSeen.Sub(g.FirstSeen).Hours()
		if windowHours < (1.0 / 60.0) {
			windowHours = 1.0 / 60.0
		}
		eventsPerHour := float64(g.EventCount) / windowHours
		loopUSD += costPerEvent * eventsPerHour * loopRunawayHours
	}

	// ─────────────────────────────────────────────────────────────
	// Bucket: drift catch
	// ─────────────────────────────────────────────────────────────
	// For each drift failure group, project the cost delta over the
	// driftDetectionHours window at the driftCostMultiplier.
	var driftUSD float64
	var driftCount int
	for _, g := range groupsThisMonth {
		if !strings.HasPrefix(g.FailureClass, "drift") {
			continue
		}
		driftCount++
		costPerEvent := 0.0
		if g.CostWastedUSD != nil && g.EventCount > 0 {
			costPerEvent = *g.CostWastedUSD / float64(g.EventCount)
		}
		windowHours := g.LastSeen.Sub(g.FirstSeen).Hours()
		if windowHours < (1.0 / 60.0) {
			windowHours = 1.0 / 60.0
		}
		eventsPerHour := float64(g.EventCount) / windowHours
		// (multiplier - 1) because the customer would have paid the
		// base cost regardless; the saved amount is only the
		// premium.
		driftUSD += costPerEvent * eventsPerHour * driftDetectionHours * (driftCostMultiplier - 1)
	}

	// ─────────────────────────────────────────────────────────────
	// Bucket: first-occurrence escalation
	// ─────────────────────────────────────────────────────────────
	// Every failure group represents a webhook fired on call #1 of
	// that pattern. If Mesedi hadn't fired, the customer would have
	// burned through firstOccurrenceMultiplier additional executions
	// before noticing. Each at avg cost-per-affected-execution for
	// that group.
	var firstOccurrenceUSD float64
	var firstOccurrenceCount int
	for _, g := range groupsThisMonth {
		// Exclude loops and drift here — those are scored in their
		// own buckets above, double-counting would inflate. The
		// escalation bucket captures crashes, time_budget, validator
		// failures, tool failures, prompt injection, cost velocity,
		// and step count.
		if strings.HasPrefix(g.FailureClass, "loops") ||
			strings.HasPrefix(g.FailureClass, "drift") {
			continue
		}
		firstOccurrenceCount++
		costPerAffected := 0.0
		if g.CostWastedUSD != nil && g.AffectedExecutions > 0 {
			costPerAffected = *g.CostWastedUSD / float64(g.AffectedExecutions)
		}
		firstOccurrenceUSD += costPerAffected * firstOccurrenceMultiplier
	}

	// ─────────────────────────────────────────────────────────────
	// Bucket: budget ceiling
	// ─────────────────────────────────────────────────────────────
	// If a tenant budget ceiling is configured AND has breached this
	// month, the savings is the spend that DIDN'T happen because
	// Mesedi halted. v1 uses the simple projection: (current burn
	// rate per day × days remaining in month) − configured ceiling.
	// Negative result clamps to 0 (no savings, no breach).
	var ceilingUSD float64
	var ceilingCount int
	if project.OwnerUserID != "" {
		ceiling, cErr := h.Store.GetTenantBudgetCeiling(ctx, project.OwnerUserID)
		if cErr == nil && ceiling != nil && ceiling.BreachedAt != nil {
			ceilingCount = 1
			// Current burn rate: MTD spend / days elapsed in month.
			mtdCost, _, _ := h.Store.SumExecutionCostByProjectSince(ctx, projectID, monthStart)
			daysElapsed := now.Sub(monthStart).Hours() / 24.0
			if daysElapsed < 1 {
				daysElapsed = 1
			}
			burnPerDay := mtdCost / daysElapsed
			// Days remaining in the calendar month.
			nextMonth := monthStart.AddDate(0, 1, 0)
			daysRemaining := nextMonth.Sub(now).Hours() / 24.0
			projectedAdditional := burnPerDay * daysRemaining
			currentBurn := mtdCost
			projectedTotalIfUncapped := currentBurn + projectedAdditional
			saved := projectedTotalIfUncapped - ceiling.MonthlyCeilingUSD
			if saved > 0 {
				ceilingUSD = saved
			}
		}
	}

	// ─────────────────────────────────────────────────────────────
	// Bucket: hard halt — v1 stub
	// ─────────────────────────────────────────────────────────────
	// Future enhancement: once status='halted' lands on executions,
	// sum the saved = (avg_completed_cost - actual_halted_cost) ×
	// halted_count. For v1 this bucket reports zero.
	hardHaltUSD := 0.0
	hardHaltCount := 0

	// ─────────────────────────────────────────────────────────────
	// Aggregate + ROI
	// ─────────────────────────────────────────────────────────────
	total := loopUSD + driftUSD + firstOccurrenceUSD + ceilingUSD + hardHaltUSD
	subscriptionCost := subscriptionCostFor(project.Tier)
	roiMultiple := 999.0
	if subscriptionCost > 0 {
		roiMultiple = total / subscriptionCost
		// Cap at 999 for display sanity. A customer at $29/mo who
		// saved $300k from a single Stripe-key leak halt isn't going
		// to feel underserved by "999x" as the displayed ratio.
		roiMultiple = math.Min(roiMultiple, 999.0)
	}

	resp := savingsResponse{
		OK:          true,
		PeriodStart: monthStart.Format(time.RFC3339),
		PeriodEnd:   now.Format(time.RFC3339),
		Buckets: map[string]savingsBucket{
			"loop_catch": {
				USD:        round2(loopUSD),
				Count:      loopCount,
				Assumption: fmt.Sprintf("Each loop would have run %.0f more hour at observed event rate. Conservative; production loops typically run several hours before a human notices.", loopRunawayHours),
			},
			"drift_catch": {
				USD:        round2(driftUSD),
				Count:      driftCount,
				Assumption: fmt.Sprintf("Each drift event would have run %.0f more hour at %.0fx the per-token cost. Reflects typical Haiku to Sonnet model-mix shift.", driftDetectionHours, driftCostMultiplier),
			},
			"first_occurrence": {
				USD:        round2(firstOccurrenceUSD),
				Count:      firstOccurrenceCount,
				Assumption: fmt.Sprintf("Each escalation fired on call #1 of the pattern. Without it, the same failure would have recurred %.0f more times before manual detection.", firstOccurrenceMultiplier),
			},
			"budget_ceiling": {
				USD:        round2(ceilingUSD),
				Count:      ceilingCount,
				Assumption: "If a tenant ceiling has breached this month, projects remaining-month burn at current rate minus the configured ceiling.",
			},
			"hard_halt": {
				USD:        0,
				Count:      hardHaltCount,
				Assumption: "Coming in a follow-up: hard-halt savings will populate once executions track a discrete halted status.",
			},
		},
		TotalUSD:            round2(total),
		SubscriptionCostUSD: subscriptionCost,
		ROIMultiple:         round2(roiMultiple),
		Tier:                project.Tier,
	}

	writeJSON(w, http.StatusOK, resp)
}

// subscriptionCostFor returns the customer's monthly subscription
// cost in USD. Keep in sync with the pricing page; the constants at
// the top of this file are the single source of truth.
func subscriptionCostFor(tier string) float64 {
	switch normalizeTier(tier) {
	case TierTeam:
		return subscriptionCostTeamMonthly
	case TierEnterprise:
		return subscriptionCostEnterpriseMonthly
	default:
		return subscriptionCostHobby
	}
}

// round2 rounds to two decimal places so the response is friendly
// to UI rendering without forcing the frontend to format-string.
func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
