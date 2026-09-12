// Budget ceiling and retention configuration endpoints.
// Split out of handlers.go on 2026-09-12; every declaration moved verbatim.
package api

import (
	"errors"
	"net/http"
	"strconv"

	"mesedi/backend/internal/store"
)

// HandleGetBudgetCeiling returns the configured tenant budget ceiling
// for the authenticated project's owner. 404 with a structured
// body when no ceiling has been configured, so the UI can render an
// empty state and prompt the user to set one up.
func (h *Handlers) HandleGetBudgetCeiling(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	ctx := r.Context()

	authProject, err := h.Store.GetProject(ctx, authProjectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load authenticated project")
		return
	}
	if authProject.OwnerUserID == "" {
		writeError(w, http.StatusNotFound, "no ceiling configured")
		return
	}

	c, err := h.Store.GetTenantBudgetCeiling(ctx, authProject.OwnerUserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no ceiling configured")
			return
		}
		h.Logger.Error("get budget ceiling failed",
			"owner_user_id", authProject.OwnerUserID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load ceiling")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// HandleUpsertBudgetCeiling configures or updates the tenant budget
// ceiling for the authenticated project's owner.
//
// Body shape:
//
//	{
//	  "monthly_ceiling_usd": 1000.0,
//	  "breach_action": "warn" | "halt",
//	  "notify_email": "ops@example.com",     // optional
//	  "notify_webhook_url": "https://..."     // optional
//	}
func (h *Handlers) HandleUpsertBudgetCeiling(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "admin") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	ctx := r.Context()

	authProject, err := h.Store.GetProject(ctx, authProjectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load authenticated project")
		return
	}
	if authProject.OwnerUserID == "" {
		writeError(w, http.StatusForbidden, "this project has no owner; cannot configure a tenant ceiling")
		return
	}

	var body struct {
		MonthlyCeilingUSD float64 `json:"monthly_ceiling_usd"`
		BreachAction      string  `json:"breach_action"`
		NotifyEmail       string  `json:"notify_email"`
		NotifyWebhookURL  string  `json:"notify_webhook_url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.MonthlyCeilingUSD <= 0 {
		writeError(w, http.StatusBadRequest, "monthly_ceiling_usd must be > 0")
		return
	}
	if body.BreachAction == "" {
		body.BreachAction = "warn"
	}
	if body.BreachAction != "warn" && body.BreachAction != "halt" {
		writeError(w, http.StatusBadRequest, "breach_action must be 'warn' or 'halt'")
		return
	}

	c := &store.TenantBudgetCeiling{
		OwnerUserID:       authProject.OwnerUserID,
		MonthlyCeilingUSD: body.MonthlyCeilingUSD,
		BreachAction:      body.BreachAction,
		NotifyEmail:       body.NotifyEmail,
		NotifyWebhookURL:  body.NotifyWebhookURL,
	}
	if err := h.Store.UpsertTenantBudgetCeiling(ctx, c); err != nil {
		h.Logger.Error("upsert budget ceiling failed",
			"owner_user_id", authProject.OwnerUserID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not save ceiling")
		return
	}

	// Echo back the saved row (now includes server-set created_at /
	// updated_at) for the UI to re-render without a follow-up GET.
	saved, err := h.Store.GetTenantBudgetCeiling(ctx, authProject.OwnerUserID)
	if err != nil {
		// Save succeeded but read-back failed; return what we have.
		writeJSON(w, http.StatusOK, c)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// HandleGetRetention returns the configured retention for the
// authenticated project. The response includes the tier's
// caps so the UI can disable the indefinite checkbox / cap the day
// input without an extra round-trip.
//
// Response:
//
//	{
//	  "ok": true,
//	  "retention_days": 30,        // or null when indefinite
//	  "is_indefinite": false,
//	  "tier": "team",
//	  "max_days": 30,              // tier-specific cap
//	  "allow_indefinite": false    // only true on enterprise
//	}
func (h *Handlers) HandleGetRetention(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	p, err := h.Store.GetProject(r.Context(), authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("get retention: load project failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load retention")
		return
	}
	days, err := h.Store.GetProjectRetentionDays(r.Context(), authProjectID)
	if err != nil {
		h.Logger.Error("get retention failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load retention")
		return
	}
	maxDays, allowIndefinite := tierRetentionCap(p.Tier)
	resp := map[string]any{
		"ok":               true,
		"retention_days":   days,
		"is_indefinite":    days == nil,
		"tier":             p.Tier,
		"max_days":         maxDays,
		"allow_indefinite": allowIndefinite,
	}
	writeJSON(w, http.StatusOK, resp)
}

// HandleSetRetention updates the retention window for the
// authenticated project. Tier-gated:
//
//	Hobby:      max 7 days,    no indefinite
//	Pro:        max 30 days,   no indefinite
//	Enterprise: max 3650 days, indefinite allowed
//
// Returns 403 with the tier's caps in the response when the customer
// asks for more than their tier permits, so the dashboard can render
// a clear "upgrade for longer retention" message instead of a generic
// validation failure.
func (h *Handlers) HandleSetRetention(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	var body struct {
		RetentionDays *int `json:"retention_days"`
		IsIndefinite  bool `json:"is_indefinite"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// Load project so we can enforce tier caps. Done BEFORE generic
	// validation so a Hobby customer asking for indefinite gets the
	// tier-specific 403 instead of a generic 400.
	p, err := h.Store.GetProject(r.Context(), authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("set retention: load project failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load project")
		return
	}
	maxDays, allowIndefinite := tierRetentionCap(p.Tier)

	// Tier guard: indefinite only on Enterprise.
	if body.IsIndefinite && !allowIndefinite {
		writeError(w, http.StatusForbidden,
			"indefinite retention is an Enterprise feature; current tier '"+p.Tier+
				"' is capped at "+strconv.Itoa(maxDays)+" days")
		return
	}

	// Tier guard: requested days must be within the tier's cap.
	var days *int
	if !body.IsIndefinite && body.RetentionDays != nil {
		v := *body.RetentionDays
		if v < 1 {
			writeError(w, http.StatusBadRequest,
				"retention_days must be at least 1")
			return
		}
		if v > maxDays {
			writeError(w, http.StatusForbidden,
				"current tier '"+p.Tier+"' is capped at "+strconv.Itoa(maxDays)+
					" days; upgrade for longer retention")
			return
		}
		days = &v
	}

	if err := h.Store.SetProjectRetentionDays(r.Context(), authProjectID, days); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("set retention failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not save retention")
		return
	}

	h.Logger.Info("retention updated",
		"project_id", authProjectID,
		"tier", p.Tier,
		"retention_days", days,
		"is_indefinite", days == nil)
	// step C, retention is a data-handling control. Customers and
	// auditors want to see when it changes and by whom. days is *int
	// so we record nil as is_indefinite=true and skip retention_days.
	retentionMeta := map[string]any{
		"tier":          p.Tier,
		"is_indefinite": days == nil,
	}
	if days != nil {
		retentionMeta["retention_days"] = *days
	}
	h.recordAuditEvent(r, AuditRetentionUpdate, "project", authProjectID, retentionMeta)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"retention_days":   days,
		"is_indefinite":    days == nil,
		"tier":             p.Tier,
		"max_days":         maxDays,
		"allow_indefinite": allowIndefinite,
	})
}
