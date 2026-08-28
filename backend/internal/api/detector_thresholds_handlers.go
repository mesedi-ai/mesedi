package api

// REST handlers for per-project detector thresholds.
//
// Endpoint surface:
//
//	GET    /me/detector-thresholds/{detector}
//	    → list every registered threshold for the detector with
//	      its current value (override if set, otherwise registry
//	      default) + the spec metadata (default, value type,
//	      description, tier cap for the caller's tier).
//
//	GET    /me/detector-thresholds/{detector}/{threshold_key}
//	    → 200 { project_id, detector, threshold_key, value,
//	            is_default, tier_cap }
//	    → 404 if (detector, threshold_key) is not in the registry.
//
//	PUT    /me/detector-thresholds/{detector}/{threshold_key}
//	    Body: { "value": <typed JSON value> }
//	    → 200 with the upserted value
//	    → 400 on bad value (parse, bound, or tier-cap)
//	    → 404 unknown (detector, threshold_key).
//
//	DELETE /me/detector-thresholds/{detector}/{threshold_key}
//	    → 204 on success (override removed; detector reads default
//	      on the next execution-close).
//	    → 404 if no override row existed (idempotent caller can
//	      treat 404 as "already default").
//
// Caller is authenticated through the existing project-scoped
// middleware; tier is read from the project row to enforce the
// per-tier cap inside the validators registry.

import (
	"encoding/json"
	"errors"
	"net/http"

	"mesedi/backend/internal/store"
)

// HandleListDetectorThresholds returns every registered threshold
// for the supplied detector with its current effective value
// (override if set, otherwise the registry default).
func (h *Handlers) HandleListDetectorThresholds(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	detector := r.PathValue("detector")
	specs := ListDetectorThresholdSpecs(detector)
	if len(specs) == 0 {
		writeError(w, http.StatusNotFound,
			"no thresholds registered for detector: "+detector)
		return
	}

	// Bulk-fetch overrides for this (project, detector) in one query
	// so the response is O(specs) without N+1 store hits.
	overrideRows, err := h.Store.ListProjectDetectorThresholds(
		r.Context(), authProjectID, detector,
	)
	if err != nil {
		h.Logger.Error("list detector_thresholds failed",
			"project_id", authProjectID, "detector", detector,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError,
			"could not list detector thresholds")
		return
	}
	overrides := map[string]string{}
	for _, row := range overrideRows {
		overrides[row.ThresholdKey] = row.ValueJSON
	}

	tier := h.projectTierOrDefault(r, authProjectID)

	items := make([]map[string]any, 0, len(specs))
	for _, spec := range specs {
		item := map[string]any{
			"detector":      spec.Detector,
			"threshold_key": spec.ThresholdKey,
			"value_type":    spec.ValueType,
			"description":   spec.Description,
			"default":       spec.Default,
		}
		if rawOverride, hasOverride := overrides[spec.ThresholdKey]; hasOverride {
			// Already-validated override; parse + tier-clamp for the
			// effective value the dashboard renders. Parse errors here
			// should be impossible (write path validates), but we
			// surface the parse failure rather than silently lying
			// about the value.
			parsed, err := spec.Parse(rawOverride, tier)
			if err != nil {
				h.Logger.Warn("stored detector_threshold failed re-parse",
					"project_id", authProjectID, "detector", spec.Detector,
					"threshold_key", spec.ThresholdKey, "error", err.Error())
				item["value"] = spec.Default
				item["is_default"] = true
				item["parse_error"] = err.Error()
			} else {
				item["value"] = parsed
				item["is_default"] = false
			}
		} else {
			item["value"] = spec.Default
			item["is_default"] = true
		}
		items = append(items, item)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": authProjectID,
		"detector":   detector,
		"tier":       tier,
		"thresholds": items,
	})
}

// HandleGetDetectorThreshold returns one (detector, threshold_key)
// with its current effective value.
func (h *Handlers) HandleGetDetectorThreshold(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	detector := r.PathValue("detector")
	thresholdKey := r.PathValue("threshold_key")
	spec, ok := LookupDetectorThresholdSpec(detector, thresholdKey)
	if !ok {
		writeError(w, http.StatusNotFound,
			"unknown threshold: "+detector+"."+thresholdKey)
		return
	}
	tier := h.projectTierOrDefault(r, authProjectID)
	row, err := h.Store.GetProjectDetectorThreshold(
		r.Context(), authProjectID, detector, thresholdKey,
	)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		h.Logger.Error("get detector_threshold failed",
			"project_id", authProjectID, "detector", detector,
			"threshold_key", thresholdKey, "error", err.Error())
		writeError(w, http.StatusInternalServerError,
			"could not load detector threshold")
		return
	}
	resp := map[string]any{
		"project_id":    authProjectID,
		"detector":      detector,
		"threshold_key": thresholdKey,
		"value_type":    spec.ValueType,
		"description":   spec.Description,
		"default":       spec.Default,
		"tier":          tier,
	}
	if row == nil {
		resp["value"] = spec.Default
		resp["is_default"] = true
	} else {
		parsed, perr := spec.Parse(row.ValueJSON, tier)
		if perr != nil {
			h.Logger.Warn("stored detector_threshold failed re-parse",
				"project_id", authProjectID, "detector", detector,
				"threshold_key", thresholdKey, "error", perr.Error())
			resp["value"] = spec.Default
			resp["is_default"] = true
			resp["parse_error"] = perr.Error()
		} else {
			resp["value"] = parsed
			resp["is_default"] = false
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// HandleSetDetectorThreshold upserts an override.
func (h *Handlers) HandleSetDetectorThreshold(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	detector := r.PathValue("detector")
	thresholdKey := r.PathValue("threshold_key")
	spec, ok := LookupDetectorThresholdSpec(detector, thresholdKey)
	if !ok {
		writeError(w, http.StatusNotFound,
			"unknown threshold: "+detector+"."+thresholdKey)
		return
	}
	var body struct {
		Value json.RawMessage `json:"value"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(body.Value) == 0 {
		writeError(w, http.StatusBadRequest, "missing value")
		return
	}
	tier := h.projectTierOrDefault(r, authProjectID)
	valueJSON := string(body.Value)
	parsed, err := spec.Parse(valueJSON, tier)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.Store.SetProjectDetectorThreshold(
		r.Context(), authProjectID, detector, thresholdKey, valueJSON,
	); err != nil {
		h.Logger.Error("upsert detector_threshold failed",
			"project_id", authProjectID, "detector", detector,
			"threshold_key", thresholdKey, "error", err.Error())
		writeError(w, http.StatusInternalServerError,
			"could not save detector threshold")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id":    authProjectID,
		"detector":      detector,
		"threshold_key": thresholdKey,
		"value":         parsed,
		"is_default":    false,
		"tier":          tier,
	})
}

// HandleDeleteDetectorThreshold removes an override row, reverting
// the detector to the registry default on the next execution-close.
func (h *Handlers) HandleDeleteDetectorThreshold(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	detector := r.PathValue("detector")
	thresholdKey := r.PathValue("threshold_key")
	if _, ok := LookupDetectorThresholdSpec(detector, thresholdKey); !ok {
		writeError(w, http.StatusNotFound,
			"unknown threshold: "+detector+"."+thresholdKey)
		return
	}
	if err := h.Store.DeleteProjectDetectorThreshold(
		r.Context(), authProjectID, detector, thresholdKey,
	); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Already at default; idempotent success.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.Logger.Error("delete detector_threshold failed",
			"project_id", authProjectID, "detector", detector,
			"threshold_key", thresholdKey, "error", err.Error())
		writeError(w, http.StatusInternalServerError,
			"could not delete detector threshold")
		return
	}
	h.recordAuditEvent(r, AuditDetectorThresholdDelete, "detector_threshold",
		detector+"."+thresholdKey, nil)
	w.WriteHeader(http.StatusNoContent)
}

// projectTierOrDefault looks up the calling project's tier so the
// validators registry can enforce the per-tier cap. Returns
// TierHobby on store error (strictest cap, safest fallback).
func (h *Handlers) projectTierOrDefault(r *http.Request, projectID string) string {
	proj, err := h.Store.GetProject(r.Context(), projectID)
	if err != nil || proj == nil {
		return TierHobby
	}
	return normalizeTier(proj.Tier)
}
