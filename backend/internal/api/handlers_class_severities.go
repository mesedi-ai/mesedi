// Failure-class severity configuration endpoints.
// Split out of handlers.go on 2026-09-12; every declaration moved verbatim.
package api

import (
	"net/http"

	"mesedi/backend/internal/severity"
	"mesedi/backend/internal/store"
)

// severityClassList is the canonical class list the severity settings
// surface renders and accepts. It mirrors the failure-class registry;
// the strings here match what the detectors emit. Ordering is
// opinionated: critical-by-default first (so the most dangerous
// classes anchor the top of the table on /app/settings), then
// warning-by-default cost/quality signals, then info-by-default
// behavioral signals. severity.Default() is the source of truth for
// the initial value; this slice controls both which classes the UI
// renders AND which class names the upsert accepts, so a setting can
// never be stored under a name the list will not show.
var severityClassList = []string{
	// critical-by-default
	"crashes",
	"tool_failures",
	"validator_failures",
	"prompt_injection",
	"data_leakage",
	"tool_schema_drift",
	"grounding_failure",
	"cascading_failure",
	"coordination_deadlock",
	"sandbox_escape",
	// warning-by-default
	"cost_velocity",
	"time_budget",
	"step_count",
	"infrastructure_throttled",
	"context_overflow",
	"token_waste",
	"provider_incident",
	"hitl_timeout",
	"hitl_rejection_spike",
	// info-by-default
	"identical_call_loop",
	"similar_call_loop",
	"semantic_loop",
	"drift",
}

// isKnownSeverityClass reports whether the class name is one the
// severity settings surface renders. Linear scan over twenty-three
// entries on an admin-tier settings endpoint; a map would be
// optimizing a code path that runs when a human clicks Save.
func isKnownSeverityClass(class string) bool {
	for _, c := range severityClassList {
		if c == class {
			return true
		}
	}
	return false
}

// HandleListClassSeverities returns the full map of failure classes
// to their currently-effective severity for the authenticated project
// . The map includes EVERY known failure class with its current
// value, sourced from:
//
//  1. project_class_severities row, if one exists for that class
//  2. severity.Default(class), otherwise
//
// The response also carries an `is_override` flag per class so the UI
// can render "(default)" vs "(custom)" badges next to each value.
//
// Response shape:
//
//	{
//	  "classes": [
//	    {"failure_class": "crashes", "severity": "critical", "is_override": false},
//	    {"failure_class": "loops",   "severity": "warning",  "is_override": true},
//	    ...
//	  ],
//	  "valid_severities": ["critical", "warning", "info"]
//	}
func (h *Handlers) HandleListClassSeverities(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	ctx := r.Context()

	overrides, err := h.Store.ListProjectClassSeverityOverrides(ctx, authProjectID)
	if err != nil {
		h.Logger.Error("list class severity overrides failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load overrides")
		return
	}
	overrideMap := make(map[string]string, len(overrides))
	for _, o := range overrides {
		overrideMap[o.FailureClass] = o.Severity
	}

	classes := severityClassList

	type classRow struct {
		FailureClass string `json:"failure_class"`
		Severity     string `json:"severity"`
		IsOverride   bool   `json:"is_override"`
	}
	out := make([]classRow, 0, len(classes))
	for _, c := range classes {
		if sev, ok := overrideMap[c]; ok {
			out = append(out, classRow{FailureClass: c, Severity: sev, IsOverride: true})
		} else {
			out = append(out, classRow{
				FailureClass: c,
				Severity:     string(severity.Default(c)),
				IsOverride:   false,
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"classes":          out,
		"valid_severities": severity.All(),
	})
}

// HandleUpsertClassSeverity sets or updates the severity override for
// a single failure class. The class name comes from the URL
// path; the body carries the new severity value.
//
//	PUT /me/class-severities/loops
//	{ "severity": "critical" }
func (h *Handlers) HandleUpsertClassSeverity(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	class := r.PathValue("class")
	if class == "" {
		writeError(w, http.StatusBadRequest, "class path parameter required")
		return
	}
	// Validate the class against the same list the GET renders.
	// Before this check, an override stored under any other string
	// (including the raw storage-level "loops" class) was accepted,
	// persisted, and then invisible everywhere: the list endpoint
	// renders only known classes, so the customer's setting silently
	// did nothing. Rejecting loudly is the only honest answer for a
	// settings endpoint.
	if !isKnownSeverityClass(class) {
		writeError(w, http.StatusBadRequest,
			"unknown failure class '"+class+"'; use one of the classes returned by GET /me/class-severities")
		return
	}

	var body struct {
		Severity string `json:"severity"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if !severity.Valid(body.Severity) {
		writeError(w, http.StatusBadRequest,
			"severity must be one of critical|warning|info")
		return
	}

	o := &store.ProjectClassSeverity{
		ProjectID:    authProjectID,
		FailureClass: class,
		Severity:     body.Severity,
	}
	if err := h.Store.UpsertProjectClassSeverity(r.Context(), o); err != nil {
		h.Logger.Error("upsert class severity failed",
			"project_id", authProjectID, "class", class, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not save override")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"failure_class": class,
		"severity":      body.Severity,
		"is_override":   true,
	})
}

// HandleDeleteClassSeverity removes a per-project severity override
// so the dispatcher reverts to severity.Default for that class.
// Idempotent: 200 OK even if no override existed.
func (h *Handlers) HandleDeleteClassSeverity(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	class := r.PathValue("class")
	if class == "" {
		writeError(w, http.StatusBadRequest, "class path parameter required")
		return
	}
	if err := h.Store.DeleteProjectClassSeverity(r.Context(), authProjectID, class); err != nil {
		h.Logger.Error("delete class severity failed",
			"project_id", authProjectID, "class", class, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not delete override")
		return
	}
	h.recordAuditEvent(r, AuditClassSeverityDelete, "failure_class", class, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"failure_class": class,
		"severity":      string(severity.Default(class)),
		"is_override":   false,
	})
}
