// Per-detector configuration endpoints and playbook reads.
// Split out of handlers.go on 2026-09-12; every declaration moved verbatim.
package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"mesedi/backend/internal/playbooks"
	"mesedi/backend/internal/store"
)

// HandleGetPlaybook returns the markdown content for the playbook
// matching (failure_class, signature) query parameters. Returns:
//
//	200 + text/markdown: content
//	400: missing or empty query params
//	404: no playbook matches this (class, signature)
//
// No auth-context project check needed, playbook content is
// universal (doesn't reference any particular project's data), so
// authenticated callers in any project can read any playbook.
func (h *Handlers) HandleGetPlaybook(w http.ResponseWriter, r *http.Request) {
	failureClass := r.URL.Query().Get("failure_class")
	signature := r.URL.Query().Get("signature")
	if failureClass == "" || signature == "" {
		writeError(w, http.StatusBadRequest,
			"failure_class and signature query parameters are both required")
		return
	}

	body, err := playbooks.Load(failureClass, signature)
	if err != nil {
		if errors.Is(err, playbooks.ErrNotFound) {
			writeError(w, http.StatusNotFound,
				"no playbook for failure_class="+failureClass+" signature="+signature)
			return
		}
		h.Logger.Error("playbook load failed",
			"failure_class", failureClass,
			"signature", signature,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "playbook load failed: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// HandleGetProviderIncidentConfig returns the per-project
// provider_incident detector configuration (migration 040). Default
// 2 mirrors the historical hardcoded constant. Single-tenant
// customers should set min_tenants to 1 so any provider error
// fires the detector.
//
// Response shape matches HandleGetTimeBudgetConfig so the dashboard
// can use one shared pattern for per-project threshold tiles.
//
// Response:
//
//	{
//	  "project_id": "...",
//	  "min_tenants": 2
//	}
func (h *Handlers) HandleGetProviderIncidentConfig(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	n, err := h.Store.GetProjectProviderIncidentMinTenants(r.Context(), authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("get provider_incident config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load provider_incident config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id":  authProjectID,
		"min_tenants": n,
	})
}

// HandleSetProviderIncidentConfig updates the per-project
// provider_incident threshold. Body:
//
//	{"min_tenants": 1}
//
// Validation: min_tenants must be >= 1. Higher values are accepted
// without an upper cap, a 1000-tenant customer might want a
// threshold of 10 before a single provider blip pages on-call.
func (h *Handlers) HandleSetProviderIncidentConfig(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	var body struct {
		MinTenants int `json:"min_tenants"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.MinTenants < 1 {
		writeError(w, http.StatusBadRequest, "min_tenants must be >= 1")
		return
	}
	// Same upper-bound discipline as time_budget / tool_return_value
	//. The dashboard tile caps at 1000; match server-side.
	const maxMinTenants = 1000
	if body.MinTenants > maxMinTenants {
		writeError(w, http.StatusBadRequest,
			"min_tenants must be <= 1000")
		return
	}
	if err := h.Store.SetProjectProviderIncidentMinTenants(
		r.Context(), authProjectID, body.MinTenants,
	); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("set provider_incident config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not update provider_incident config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"min_tenants": body.MinTenants,
	})
}

// HandleGetTimeBudgetConfig returns the per-project time_budget
// detector threshold (migration 041) in milliseconds. Default 60000
// mirrors the historical hardcoded constant. Chat-agent projects
// often lower it (e.g. 30000); research-agent projects often raise
// it (e.g. 300000).
//
// Response:
//
//	{
//	  "project_id": "...",
//	  "threshold_ms": 60000
//	}
func (h *Handlers) HandleGetTimeBudgetConfig(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	n, err := h.Store.GetProjectTimeBudgetMs(r.Context(), authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("get time_budget config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load time_budget config")
		return
	}
	// Wire tier-cap constants into the response. The dashboard
	// reads tier + max_ms_for_tier to render the cap label and clamp
	// over-cap input. Before this fix the endpoint returned neither,
	// so the dashboard showed 'tier (unknown)' and silently fell back
	// to the highest cap.
	tier := h.lookupProjectTier(r.Context(), authProjectID)
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id":      authProjectID,
		"threshold_ms":    n,
		"tier":            tier,
		"max_ms_for_tier": TierCapTimeBudgetMs(tier),
	})
}

// HandleSetTimeBudgetConfig updates the per-project time_budget
// threshold. Body:
//
//	{"threshold_ms": 30000}
//
// Validation: threshold_ms must be >= 1 (zero would fire on every
// terminal execution). No upper cap, a batch-processing project
// might legitimately set hours-long thresholds.
func (h *Handlers) HandleSetTimeBudgetConfig(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	var body struct {
		ThresholdMs int `json:"threshold_ms"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.ThresholdMs < 1 {
		writeError(w, http.StatusBadRequest, "threshold_ms must be >= 1")
		return
	}
	// Server-side upper bound matches the dashboard tile's 24h cap
	// (86_400_000 ms). Without this, a typo of 999999999999 was
	// silently accepted by the backend and mishandled downstream
	//.
	const maxThresholdMs = 86_400_000 // 24 hours
	if body.ThresholdMs > maxThresholdMs {
		writeError(w, http.StatusBadRequest,
			"threshold_ms must be <= 86400000 (24 hours)")
		return
	}
	// Tier-aware enforcement. Today's tier-cap helper falls back to
	// Hobby (strictest) for unknown tiers; that's the safest default
	//, better to refuse a save and let the customer ask support than
	// to silently allow Enterprise budgets on a Hobby plan.
	projectTier := h.lookupProjectTier(r.Context(), authProjectID)
	tierCap := TierCapTimeBudgetMs(projectTier)
	if body.ThresholdMs > tierCap {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"threshold_ms %d exceeds your tier cap of %d ms (%s tier)",
			body.ThresholdMs, tierCap, projectTier,
		))
		return
	}
	if err := h.Store.SetProjectTimeBudgetMs(
		r.Context(), authProjectID, body.ThresholdMs,
	); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("set time_budget config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not update time_budget config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"threshold_ms": body.ThresholdMs,
	})
}

// HandleGetCostVelocityConfig returns the per-project cost_velocity
// detector threshold (migration 043) in USD. Default 1.00 mirrors
// the post-Wave-0 sensible default, raised from the broken v0.0.1
// hardcoded $0.001 that fired on every real execution. Cost-sensitive
// customers often lower it; batch / tolerant customers often raise it.
//
// Response shape matches HandleGetTimeBudgetConfig /
// HandleGetProviderIncidentConfig so the dashboard can use one
// shared pattern for per-project threshold tiles.
//
// Response:
//
//	{
//	  "project_id": "...",
//	  "threshold_usd": 1.00
//	}
func (h *Handlers) HandleGetCostVelocityConfig(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	n, err := h.Store.GetProjectCostVelocityThresholdUSD(r.Context(), authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("get cost_velocity config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load cost_velocity config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id":    authProjectID,
		"threshold_usd": n,
	})
}

// HandleSetCostVelocityConfig updates the per-project cost_velocity
// threshold. Body:
//
//	{"threshold_usd": 0.50}
//
// Validation: threshold_usd must be in [0.01, 10000.00]. The floor
// prevents fires-on-every-execution storage abuse (a customer
// setting threshold near zero would create a failure_group for
// every execution). The ceiling prevents typo / float overflow.
//
// NOT tier-capped: cost_velocity threshold is the customer's alarm
// sensitivity, not a Mesedi-side cost vector. Same reasoning as
// provider_incident_min_tenants (see tier_caps.go).
func (h *Handlers) HandleSetCostVelocityConfig(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	var body struct {
		ThresholdUSD float64 `json:"threshold_usd"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	const (
		minThresholdUSD = 0.01
		maxThresholdUSD = 10_000.00
	)
	if body.ThresholdUSD < minThresholdUSD {
		writeError(w, http.StatusBadRequest,
			"threshold_usd must be >= 0.01 (lower values would fire on every execution and create storage abuse)")
		return
	}
	if body.ThresholdUSD > maxThresholdUSD {
		writeError(w, http.StatusBadRequest,
			"threshold_usd must be <= 10000.00")
		return
	}
	if err := h.Store.SetProjectCostVelocityThresholdUSD(
		r.Context(), authProjectID, body.ThresholdUSD,
	); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("set cost_velocity config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not update cost_velocity config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"threshold_usd": body.ThresholdUSD,
	})
}

// HandleGetPlaybookSignatures returns the SHA-256 digest of every
// in-binary playbook keyed by either "<failure_class>" (for catch-all
// playbooks) or "<failure_class>:<signature_prefix>" (for detectors
// with multiple signature variants like loops / drift /
// prompt_injection). The dashboard compares the value for a failure
// group against group.analysis_playbook_signature; mismatch surfaces
// the "re-analyze to refresh" badge (Wave ai-analysis-staleness-
// tracking).
//
// Tier-agnostic: the playbook content is identical across projects
// + tiers (compiled into the binary). The /me/ scope is structural ,
// keeping all customer-facing reads under one auth chain, not a
// per-project differentiator.
//
// Response:
//
//	{
//	  "signatures": {
//	    "data_leakage":           "abc...",
//	    "loops:identical_call_":  "def...",
//	    "drift:lexical_drift_":   "ghi...",
//	    ...
//	  }
//	}
func (h *Handlers) HandleGetPlaybookSignatures(w http.ResponseWriter, r *http.Request) {
	if _, ok := ProjectIDFromContext(r.Context()); !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"signatures": playbooks.AllSignatures(),
	})
}

// HandleGetCostVelocityRateConfig returns the per-project rate
// detector configuration (migration 044): the $/minute threshold and
// the rolling lookback window in minutes. Defaults {5.00, 5} ,
// $300/hr sustained burn over a 5-minute window. Pairs with the
// absolute threshold (HandleGetCostVelocityConfig); both detectors
// fire independently because they answer different questions.
//
// Response:
//
//	{
//	  "project_id":             "...",
//	  "threshold_usd_per_min":  5.00,
//	  "window_minutes":         5
//	}
func (h *Handlers) HandleGetCostVelocityRateConfig(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	cfg, err := h.Store.GetProjectCostVelocityRateConfig(r.Context(), authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("get cost_velocity_rate config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load cost_velocity_rate config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id":            authProjectID,
		"threshold_usd_per_min": cfg.ThresholdUSDPerMin,
		"window_minutes":        cfg.WindowMinutes,
	})
}

// HandleSetCostVelocityRateConfig updates the per-project rate
// configuration. Body:
//
//	{"threshold_usd_per_min": 2.50, "window_minutes": 10}
//
// Validation:
//   - threshold_usd_per_min ∈ [0.10, 10000.00]. The floor prevents
//     fires-on-every-minute storage abuse; the ceiling prevents
//     typo / float overflow.
//   - window_minutes ∈ [1, 60]. The floor avoids noise from sub-minute
//     spikes; the ceiling bounds aggregator scan size.
//
// NOT tier-capped: same reasoning as the absolute threshold and
// provider_incident_min_tenants, alarm sensitivity is the customer's
// choice, not a Mesedi-side cost vector. See tier_caps.go.
func (h *Handlers) HandleSetCostVelocityRateConfig(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	var body struct {
		ThresholdUSDPerMin float64 `json:"threshold_usd_per_min"`
		WindowMinutes      int     `json:"window_minutes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	const (
		minThresholdUSDPerMin = 0.10
		maxThresholdUSDPerMin = 10_000.00
		minWindowMinutes      = 1
		maxWindowMinutes      = 60
	)
	if body.ThresholdUSDPerMin < minThresholdUSDPerMin {
		writeError(w, http.StatusBadRequest,
			"threshold_usd_per_min must be >= 0.10 (lower values would fire on every minute and create storage abuse)")
		return
	}
	if body.ThresholdUSDPerMin > maxThresholdUSDPerMin {
		writeError(w, http.StatusBadRequest,
			"threshold_usd_per_min must be <= 10000.00")
		return
	}
	if body.WindowMinutes < minWindowMinutes {
		writeError(w, http.StatusBadRequest,
			"window_minutes must be >= 1")
		return
	}
	if body.WindowMinutes > maxWindowMinutes {
		writeError(w, http.StatusBadRequest,
			"window_minutes must be <= 60 (longer windows make aggregator scans pathological)")
		return
	}
	cfg := store.CostVelocityRateConfig{
		ThresholdUSDPerMin: body.ThresholdUSDPerMin,
		WindowMinutes:      body.WindowMinutes,
	}
	if err := h.Store.SetProjectCostVelocityRateConfig(
		r.Context(), authProjectID, cfg,
	); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("set cost_velocity_rate config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not update cost_velocity_rate config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                    true,
		"threshold_usd_per_min": cfg.ThresholdUSDPerMin,
		"window_minutes":        cfg.WindowMinutes,
	})
}

// HandleGetToolReturnValueConfig returns the per-project byte cap
// on tool_call return_value payloads used by the tool_schema_drift
// detector (migration 042 / ). Default 8192 (8 KB), covers
// typical tool returns while bounding pathological cases. The full
// event payload is still stored regardless of this cap; only the
// detector's fingerprint comparison is bounded.
//
// Response shape matches HandleGetTimeBudgetConfig /
// HandleGetProviderIncidentConfig so the dashboard can use one
// shared pattern for per-project threshold tiles.
//
// Response:
//
//	{
//	  "project_id": "...",
//	  "max_bytes": 8192
//	}
func (h *Handlers) HandleGetToolReturnValueConfig(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	n, err := h.Store.GetProjectToolReturnValueMaxBytes(r.Context(), authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("get tool_return_value config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load tool_return_value config")
		return
	}
	tier := h.lookupProjectTier(r.Context(), authProjectID)
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id":         authProjectID,
		"max_bytes":          n,
		"tier":               tier,
		"max_bytes_for_tier": TierCapToolReturnValueBytes(tier),
	})
}

// HandleSetToolReturnValueConfig updates the per-project cap. Body:
//
//	{"max_bytes": 16384}
//
// Validation: max_bytes must be >= 1. Server-side upper bound is
// 1 MB (matches the existing payload cap from ), a per-project
// fingerprint cap larger than the wire-format max is meaningless
// because the SDK can't ship more than 1 MB anyway.
func (h *Handlers) HandleSetToolReturnValueConfig(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	var body struct {
		MaxBytes int `json:"max_bytes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.MaxBytes < 1 {
		writeError(w, http.StatusBadRequest, "max_bytes must be >= 1")
		return
	}
	const oneMegabyte = 1 << 20
	if body.MaxBytes > oneMegabyte {
		writeError(w, http.StatusBadRequest, "max_bytes must be <= 1048576 (the wire-format payload cap)")
		return
	}
	// Tier-aware enforcement (finally wired). Hobby 4KB, Team
	// 32KB, Enterprise 1MB. Helper falls back to Hobby on unknown
	// tier, same safe default as time_budget.
	projectTier := h.lookupProjectTier(r.Context(), authProjectID)
	tierCap := TierCapToolReturnValueBytes(projectTier)
	if body.MaxBytes > tierCap {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"max_bytes %d exceeds your tier cap of %d bytes (%s tier)",
			body.MaxBytes, tierCap, projectTier,
		))
		return
	}
	if err := h.Store.SetProjectToolReturnValueMaxBytes(
		r.Context(), authProjectID, body.MaxBytes,
	); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("set tool_return_value config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not update tool_return_value config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"max_bytes": body.MaxBytes,
	})
}

// HandleGetToolReturnValueStats returns recent-window telemetry on
// how often tool_call return_values are being clipped.
// Two clip sources are counted:
//
//   - SDK truncation: the SDK shipped the literal "<truncated>"
//     string after the return_value JSON exceeded its on-wire cap
//     (16 KB in v0.5.0+).
//   - Backend exclusion: the return_value JSON length exceeds the
//     per-project tool_return_value_max_bytes cap; the detector
//     drops these from the schema-drift fingerprint comparison.
//
// Window defaults to the last 24 hours. A high rate means the
// customer should raise their cap (or refactor the tool to return
// less) so schema_drift has more comparable signal.
//
// Response shape:
//
//	{
//	  "project_id": "...",
//	  "window_hours": 24,
//	  "total_calls": 1500,
//	  "truncated_sentinel_count": 12,
//	  "oversized_count": 8,
//	  "max_bytes": 8192
//	}
func (h *Handlers) HandleGetToolReturnValueStats(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	maxBytes, err := h.Store.GetProjectToolReturnValueMaxBytes(r.Context(), authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		// Fall through with default 8192 on transient errors so
		// the stats query doesn't fail outright, the customer
		// just sees stats computed against the default cap rather
		// than their configured one.
		maxBytes = 8192
	}
	stats, err := h.Store.GetToolReturnValueStats(
		r.Context(), authProjectID, 24, maxBytes,
	)
	if err != nil {
		h.Logger.Error("get tool_return_value stats failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load tool_return_value stats")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id":               authProjectID,
		"window_hours":             stats.WindowHours,
		"total_calls":              stats.TotalCalls,
		"truncated_sentinel_count": stats.TruncatedCount,
		"oversized_count":          stats.OversizedCount,
		"max_bytes":                maxBytes,
	})
}

// HandleGetConfigFallbackStats returns recent-window counts of
// per-project config-read fallbacks. The dashboard uses
// this to render a warning chip on each Settings tile when the
// corresponding config has fallen back to the default in the last
// 24 h, a signal that something operational is going wrong with
// the backend (transient DB blip, dropped column, etc.) and the
// customer's tuning is being silently ignored.
//
// Response:
//
//	{
//	  "project_id": "...",
//	  "window_hours": 24,
//	  "time_budget_count": 0,
//	  "provider_incident_min_tenants_count": 0,
//	  "tool_return_value_max_bytes_count": 0
//	}
func (h *Handlers) HandleGetConfigFallbackStats(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	// : optional ?window_hours=<int> query param. Range 1..168
	// (1 hour to 7 days). Default 24 when absent. Out-of-range
	// returns 400 with an explicit message so dashboard callers
	// don't silently get the default for typo'd values.
	windowHours := 24
	if raw := r.URL.Query().Get("window_hours"); raw != "" {
		parsed, perr := strconv.Atoi(raw)
		if perr != nil || parsed < 1 || parsed > 168 {
			writeError(w, http.StatusBadRequest,
				"window_hours must be an integer in range 1..168")
			return
		}
		windowHours = parsed
	}
	stats, err := h.Store.GetConfigFallbackStats(r.Context(), authProjectID, windowHours)
	if err != nil {
		h.Logger.Error("get config_fallback stats failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load config_fallback stats")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id":                          authProjectID,
		"window_hours":                        stats.WindowHours,
		"time_budget_count":                   stats.TimeBudgetCount,
		"provider_incident_min_tenants_count": stats.ProviderIncidentMinTenantsCount,
		"tool_return_value_max_bytes_count":   stats.ToolReturnValueMaxBytesCount,
		"class_severity_override_count":       stats.ClassSeverityOverrideCount,
	})
}
