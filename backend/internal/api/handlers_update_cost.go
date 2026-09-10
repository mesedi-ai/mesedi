// Cost-velocity detection, both forms, carved out of
// HandleUpdateExecution on 2026-09-09 (task #35 Phase D, first cut)
// because #48's attribution work needs somewhere to land that is not
// the interior of a 1,500-line function.
//
// The absolute form fires when one execution's resolved cost exceeds
// the per-project threshold; the rate form sums costs over a rolling
// per-project window and fires on burn rate. They are independent by
// design and both can fire on the same execution. Bodies moved
// verbatim; only the indentation depth and the enclosing braces are
// new.
package api

import (
	"net/http"
	"time"

	"mesedi/backend/internal/store"
)

// runCostVelocityDetectors runs both cost-velocity forms for one
// terminal execution. effectiveCost is the resolved cost the caller
// already computed and persisted; zero-cost executions never reach
// here with a signal worth firing on, but the guards inside keep
// their original behaviour regardless.
//
// tenantID and apiKeyID come from the execution row (not the PATCH
// request's auth context, which may be a different key than the one
// that ran the work) and resolve to the #48 attribution identity:
// the absolute detector's group signature carries WHO, so spend from
// a never-seen tenant or credential creates a NEW group and the
// new-group webhook escalation fires structurally instead of folding
// into the familiar magnitude bucket as a recurrence. The rate
// detector stays project-wide: burn rate is aggregate spend.
func (h *Handlers) runCostVelocityDetectors(r *http.Request, executionID, authProjectID string, effectiveCost float64, tenantID, apiKeyID *string) {
	tenant, apiKey := "", ""
	if tenantID != nil {
		tenant = *tenantID
	}
	if apiKeyID != nil {
		apiKey = *apiKeyID
	}
	identity := store.CostVelocityIdentity(tenant, apiKey)
	// Sub-slice 16: cost-velocity detector. Any execution whose
	// resolved cost exceeds the per-project threshold gets
	// grouped as cost_velocity with a cost-bucketed signature.
	//
	// Migration 043: threshold is per-project. Default 1.00 USD
	// (raised from the broken $0.001 v0.0.1 floor that fired
	// on every real execution). Cost-sensitive customers can
	// lower it (e.g. $0.10); batch customers can raise it.
	// Falls back to the package default on store error so a
	// transient DB blip never silences the detector, with a
	// durable audit_event so persistent failures surface in
	// the dashboard config-fallback chip.
	//
	// Migration 044 () adds the rate-based ($/min)
	// detector immediately after this block. Both can fire on
	// the same execution because they answer different
	// questions (single expensive call vs sustained burn
	// rate); idempotent failure_group writes ensure no
	// double-counting under the absolute signature.
	if effectiveCost > 0 {
		costThresholdUSD := store.DefaultCostVelocityThresholdUSD
		if cvUSD, cvErr := h.Store.GetProjectCostVelocityThresholdUSD(r.Context(), authProjectID); cvErr == nil {
			costThresholdUSD = cvUSD
		} else {
			h.Logger.Warn("get cost_velocity_threshold_usd failed; using default",
				"execution_id", executionID,
				"project_id", authProjectID,
				"error", cvErr.Error(),
			)
			h.recordSystemEventForProject(
				r.Context(),
				authProjectID, "config_fallback",
				"config_fallback", "project_config", "cost_velocity_threshold_usd",
				map[string]any{
					"error":          cvErr.Error(),
					"fallback_value": costThresholdUSD,
				},
			)
		}
		if effectiveCost >= costThresholdUSD {
			isNew, gErr := h.Store.GroupCostVelocity(r.Context(), executionID, authProjectID, effectiveCost, identity)
			if gErr != nil {
				h.Logger.Warn("cost-velocity grouping failed (continuing)",
					"execution_id", executionID,
					"cost_usd", effectiveCost,
					"threshold_usd", costThresholdUSD,
					"error", gErr.Error(),
				)
			}
			h.maybeFireWebhook(r, authProjectID, store.FailureClassCostVelocity, store.CostVelocityAttributedSignature(effectiveCost, identity), isNew, gErr)
		}
	}

	// Sub-slice 16b: cost-velocity RATE detector. Sums execution
	// costs over a per-project rolling window and fires when
	// the burn-rate ($/minute) exceeds the per-project threshold.
	// Closes the marketing-vs-implementation gap from the audit
	// (cost_velocity.G2): marketing promised "$/minute rate
	// detection" but only per-execution magnitude existed.
	//
	// Migration 044: threshold + window are per-project. Defaults
	// {5.00 USD/min, 5 min}. Same fallback-to-default-with-audit
	// pattern as the absolute block above. Independent of the
	// absolute detector, both can fire on the same execution.
	//
	// Aggregator reuses SumExecutionCostByProjectSince, it
	// already exists (org-rollup endpoint), so no new store
	// API surface and no new index requirements (the existing
	// (project_id, started_at) covers the scan).
	rateCfg := store.DefaultCostVelocityRateConfig
	if rc, rcErr := h.Store.GetProjectCostVelocityRateConfig(r.Context(), authProjectID); rcErr == nil {
		rateCfg = rc
	} else {
		h.Logger.Warn("get cost_velocity_rate_config failed; using default",
			"execution_id", executionID,
			"project_id", authProjectID,
			"error", rcErr.Error(),
		)
		h.recordSystemEventForProject(
			r.Context(),
			authProjectID, "config_fallback",
			"config_fallback", "project_config", "cost_velocity_rate_config",
			map[string]any{
				"error":                rcErr.Error(),
				"fallback_threshold":   rateCfg.ThresholdUSDPerMin,
				"fallback_window_mins": rateCfg.WindowMinutes,
			},
		)
	}
	windowStart := time.Now().UTC().Add(-time.Duration(rateCfg.WindowMinutes) * time.Minute)
	windowCostUSD, _, rcAggErr := h.Store.SumExecutionCostByProjectSince(r.Context(), authProjectID, windowStart)
	if rcAggErr != nil {
		// Aggregator failures must NOT break the request path.
		// Log + audit-telemetry + skip the rate fire for this
		// execution. The absolute detector above has already
		// run; rate is additive signal, not the only one.
		h.Logger.Warn("cost-velocity rate aggregator failed (continuing)",
			"execution_id", executionID,
			"project_id", authProjectID,
			"window_minutes", rateCfg.WindowMinutes,
			"error", rcAggErr.Error(),
		)
		h.recordSystemEventForProject(
			r.Context(),
			authProjectID, "config_fallback",
			"config_fallback", "project_config", "cost_velocity_rate_aggregator",
			map[string]any{
				"error": rcAggErr.Error(),
			},
		)
	} else if rateCfg.WindowMinutes > 0 {
		ratePerMin := windowCostUSD / float64(rateCfg.WindowMinutes)

		// Baseline (learned-normal) form, independent of the fixed
		// threshold below: fires on the MULTIPLE of this project's
		// own trailing-week burn rate, so an attacker sitting just
		// under the operator's fixed number still surfaces. The
		// learning gate, floor and multiplier live as constants
		// beside the decision function in the store package; a
		// project too young to have a normal stays silent here and
		// keeps the fixed thresholds' protection.
		h.runCostVelocityBaseline(r, executionID, authProjectID, ratePerMin)

		if ratePerMin >= rateCfg.ThresholdUSDPerMin {
			isNew, gErr := h.Store.GroupCostVelocityRate(r.Context(), executionID, authProjectID, ratePerMin)
			if gErr != nil {
				h.Logger.Warn("cost-velocity rate grouping failed (continuing)",
					"execution_id", executionID,
					"rate_usd_per_min", ratePerMin,
					"threshold_usd_per_min", rateCfg.ThresholdUSDPerMin,
					"window_minutes", rateCfg.WindowMinutes,
					"error", gErr.Error(),
				)
			}
			// Webhook fires under the rate signature
			// distinctly from the absolute one so SREs can
			// route them differently if they choose.
			h.maybeFireWebhook(r, authProjectID, store.FailureClassCostVelocity, store.CostVelocityRateSignature(ratePerMin), isNew, gErr)
		}
	}
}

// runCostVelocityBaseline runs the learned-normal form for one
// terminal execution. Baseline is the project's average dollars per
// minute over its trailing week, with the week shortened to the
// project's actual age when younger, so a four-day-old project's
// normal is not diluted by three empty days. Store errors degrade to
// silence with a warning; the fixed-threshold forms have already run
// and this signal is additive.
func (h *Handlers) runCostVelocityBaseline(r *http.Request, executionID, authProjectID string, ratePerMin float64) {
	now := time.Now().UTC()
	earliest, err := h.Store.EarliestExecutionStart(r.Context(), authProjectID)
	if err != nil || earliest.IsZero() {
		if err != nil {
			h.Logger.Warn("cost-velocity baseline: earliest-start lookup failed (skipping)",
				"execution_id", executionID, "error", err.Error())
		}
		return
	}
	projectAgeHours := now.Sub(earliest).Hours()

	windowStart := now.AddDate(0, 0, -store.CostVelocityBaselineWindowDays)
	if earliest.After(windowStart) {
		windowStart = earliest
	}
	baselineSpanMinutes := now.Sub(windowStart).Minutes()
	if baselineSpanMinutes <= 0 {
		return
	}
	weekCostUSD, weekRuns, err := h.Store.SumExecutionCostByProjectSince(r.Context(), authProjectID, windowStart)
	if err != nil {
		h.Logger.Warn("cost-velocity baseline: trailing-window sum failed (skipping)",
			"execution_id", executionID, "error", err.Error())
		return
	}
	baselineRate := weekCostUSD / baselineSpanMinutes

	multiple, fired := store.CostVelocityBaselineExceeded(ratePerMin, baselineRate, weekRuns, projectAgeHours)
	if !fired {
		return
	}
	isNew, gErr := h.Store.GroupCostVelocityBaseline(r.Context(), executionID, authProjectID, multiple)
	if gErr != nil {
		h.Logger.Warn("cost-velocity baseline grouping failed (continuing)",
			"execution_id", executionID,
			"rate_usd_per_min", ratePerMin,
			"baseline_usd_per_min", baselineRate,
			"multiple", multiple,
			"error", gErr.Error(),
		)
	}
	h.Logger.Info("cost-velocity baseline fired",
		"execution_id", executionID,
		"rate_usd_per_min", ratePerMin,
		"baseline_usd_per_min", baselineRate,
		"multiple", multiple,
	)
	h.maybeFireWebhook(r, authProjectID, store.FailureClassCostVelocity, store.CostVelocityBaselineSignature(multiple), isNew, gErr)
}
