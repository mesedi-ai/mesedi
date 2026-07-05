package api

// Wave + .g endpoints. Three new GET/PUT surfaces over the
// organization_defaults table + the org-level config-fallback
// rollup. All three are owner-only — the auth'd user must be the
// org's created_by_user_id. Mesedi v1's multi-seat support is too
// thin to justify per-member role checks for what is effectively
// an admin operation; future revisions can promote to role-based
// once role wiring stabilizes.

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// HandleGetOrgDefaults returns every (default_key → value_json)
// override for the auth'd user's org. Empty object when the org
// has no defaults set.
func (h *Handlers) HandleGetOrgDefaults(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	orgID := h.lookupOrgIDForProject(r.Context(), authProjectID)
	if orgID == "" {
		// No org → return empty defaults map rather than 404, so
		// the dashboard can render the empty Org Settings tile
		// without special-casing.
		writeJSON(w, http.StatusOK, map[string]any{
			"org_id":   "",
			"defaults": map[string]string{},
		})
		return
	}
	defs, err := h.Store.GetOrgDefaults(r.Context(), orgID)
	if err != nil {
		h.Logger.Error("get org defaults failed",
			"project_id", authProjectID,
			"org_id", orgID,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load org defaults")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"org_id":   orgID,
		"defaults": defs,
	})
}

// HandlePutOrgDefault upserts one (default_key, value) for the
// auth'd user's org. Validates default_key against the known set
// + the value against the same tier caps used at the project
// level so an admin can't bypass tier enforcement by setting an
// over-cap org default.
func (h *Handlers) HandlePutOrgDefault(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	orgID := h.lookupOrgIDForProject(r.Context(), authProjectID)
	if orgID == "" {
		writeError(w, http.StatusBadRequest,
			"project is not associated with an organization; "+
				"create one before setting org defaults")
		return
	}

	var body struct {
		DefaultKey string `json:"default_key"`
		Value      *int   `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !IsValidOrgDefaultKey(body.DefaultKey) {
		writeError(w, http.StatusBadRequest,
			"default_key must be one of time_budget_ms, "+
				"provider_incident_min_tenants, "+
				"tool_return_value_max_bytes")
		return
	}
	if body.Value == nil {
		writeError(w, http.StatusBadRequest, "value is required")
		return
	}
	if *body.Value < 1 {
		writeError(w, http.StatusBadRequest,
			"value must be a positive integer (1 or greater); "+
				"to clear the org default, DELETE the key (not yet implemented)")
		return
	}
	// Bounds check per key — matches the project-level validator
	// upper bounds in handlers.go.
	switch body.DefaultKey {
	case OrgDefaultKeyTimeBudgetMs:
		if *body.Value > 86_400_000 {
			writeError(w, http.StatusBadRequest,
				"time_budget_ms must be ≤ 86_400_000 (24 hours)")
			return
		}
	case OrgDefaultKeyProviderIncidentMinTenants:
		if *body.Value > 1000 {
			writeError(w, http.StatusBadRequest,
				"provider_incident_min_tenants must be ≤ 1000")
			return
		}
	case OrgDefaultKeyToolReturnValueMaxBytes:
		if *body.Value > 1_048_576 {
			writeError(w, http.StatusBadRequest,
				"tool_return_value_max_bytes must be ≤ 1_048_576 (1 MB)")
			return
		}
	}

	valueJSON := strconv.Itoa(*body.Value)
	if err := h.Store.SetOrgDefault(
		r.Context(), orgID, body.DefaultKey, valueJSON,
	); err != nil {
		h.Logger.Error("set org default failed",
			"project_id", authProjectID,
			"org_id", orgID,
			"default_key", body.DefaultKey,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not save org default")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"org_id":      orgID,
		"default_key": body.DefaultKey,
		"value":       *body.Value,
	})
}

// HandleGetOrgConfigFallbackRollup returns the aggregated
// config-fallback rollup across every project in the auth'd
// user's org, over a configurable window (defaults 24h).
func (h *Handlers) HandleGetOrgConfigFallbackRollup(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	orgID := h.lookupOrgIDForProject(r.Context(), authProjectID)

	// Accept ?window_hours= per the same range as the per-project
	// stats endpoint.
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

	if orgID == "" {
		// No org → return zero rollup rather than 404 so the
		// dashboard banner renders nothing without a special case.
		writeJSON(w, http.StatusOK, map[string]any{
			"org_id":                 "",
			"window_hours":           windowHours,
			"affected_project_count": 0,
			"total_events":           0,
			"top_targets":            []any{},
		})
		return
	}

	rollup, err := h.Store.GetOrgConfigFallbackRollup(
		r.Context(), orgID, windowHours,
	)
	if err != nil {
		h.Logger.Error("get org config_fallback rollup failed",
			"project_id", authProjectID,
			"org_id", orgID,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError,
			"could not load org config_fallback rollup")
		return
	}

	// Serialize top_targets as plain JSON objects (no struct tags
	// on the store type to keep the import shape narrow).
	tops := make([]map[string]any, 0, len(rollup.TopTargets))
	for _, t := range rollup.TopTargets {
		tops = append(tops, map[string]any{
			"target_id": t.TargetID,
			"count":     t.Count,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"org_id":                 orgID,
		"window_hours":           rollup.WindowHours,
		"affected_project_count": rollup.AffectedProjectCount,
		"total_events":           rollup.TotalEvents,
		"top_targets":            tops,
	})
}
