package api

// Per-project custom-pattern API for the three security detectors
// that share the Wave 2.1 pattern-config primitive
// (prompt_injection, data_leakage, sandbox_escape). Closes
// prompt_injection.G1 + data_leakage.G1 + sandbox_escape.G2.
//
// Endpoint surface:
//
//	GET    /me/pattern-config/{detector}           list (all patterns)
//	POST   /me/pattern-config/{detector}           create one pattern
//	PATCH  /me/pattern-config/{detector}/{pattern_id}  update one pattern
//	DELETE /me/pattern-config/{detector}/{pattern_id}  delete one pattern
//
// {detector} must be one of: prompt_injection, data_leakage,
// sandbox_escape. Unknown detector returns 404.
//
// Customer-supplied patterns are RE2-validated at POST/PATCH time
// before storage. A malformed regex stored unchecked would crash
// every event in the detector hot path; the validation step is the
// only defense.
//
// Server-side enforcement:
//   - PROJECT_PATTERN_MAX = 200 per (project, detector). Generous
//     for prototyping; tight enough to prevent runaway-write DoS.
//   - Severity ∈ {"low", "medium", "high"}.
//   - Description capped at 500 chars.
//   - Pattern source capped at 1000 chars (RE2 patterns longer than
//     this are almost certainly bugs).

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"mesedi/backend/internal/store"
)

// Detectors that share the pattern-config primitive. Allow-list is
// at the API edge so a future fourth pattern-based detector drops
// in with no store-layer change.
var patternConfigDetectors = map[string]bool{
	"prompt_injection": true,
	"data_leakage":     true,
	"sandbox_escape":   true,
}

// PROJECT_PATTERN_MAX caps the number of custom patterns per
// (project, detector). Prevents runaway-write DoS and keeps the
// detector hot-path SELECT bounded.
const projectPatternMax = 200

// projectPatternSeverities is the allow-list for the severity field.
var projectPatternSeverities = map[string]bool{
	"low":    true,
	"medium": true,
	"high":   true,
}

const (
	projectPatternMaxLen     = 1000
	projectPatternMaxDescLen = 500
)

// HandleListPatternConfig returns every pattern (enabled + disabled)
// for the calling project's selected detector. The detector hot
// path uses enabledOnly=true; this surface uses enabledOnly=false
// so the dashboard editor (Wave 2.1.c) can show disabled rules too.
//
//	GET /me/pattern-config/{detector}
//	→ 200 {"project_id": "...", "detector": "...", "patterns": [...]}
//	→ 404 if detector is not in the allow-list
func (h *Handlers) HandleListPatternConfig(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	detector := r.PathValue("detector")
	if !patternConfigDetectors[detector] {
		writeError(w, http.StatusNotFound,
			"detector must be one of: prompt_injection, data_leakage, sandbox_escape")
		return
	}
	patterns, err := h.Store.ListProjectPatterns(
		r.Context(), authProjectID, detector, false,
	)
	if err != nil {
		h.Logger.Error("list project_patterns failed",
			"project_id", authProjectID, "detector", detector,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not list patterns")
		return
	}
	if patterns == nil {
		patterns = []*store.ProjectPattern{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": authProjectID,
		"detector":   detector,
		"patterns":   patterns,
	})
}

// HandleCreatePatternConfig inserts a new pattern row for the
// calling project's selected detector.
//
//	POST /me/pattern-config/{detector}
//	Body: {"pattern": "...", "severity": "...", "description": "...", "enabled": true}
//	→ 201 with the new pattern (including server-assigned pattern_id)
//	→ 400 on RE2-invalid pattern, missing pattern, bad severity, etc.
//	→ 404 unknown detector
//	→ 429 PROJECT_PATTERN_MAX reached
func (h *Handlers) HandleCreatePatternConfig(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	detector := r.PathValue("detector")
	if !patternConfigDetectors[detector] {
		writeError(w, http.StatusNotFound,
			"detector must be one of: prompt_injection, data_leakage, sandbox_escape")
		return
	}
	var body struct {
		Pattern     string `json:"pattern"`
		Severity    string `json:"severity"`
		Description string `json:"description"`
		Enabled     *bool  `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if validationErr := validatePatternFields(
		body.Pattern, body.Severity, body.Description,
	); validationErr != "" {
		writeError(w, http.StatusBadRequest, validationErr)
		return
	}
	// Enforce PROJECT_PATTERN_MAX before insert.
	count, err := h.Store.CountProjectPatterns(r.Context(), authProjectID, detector)
	if err != nil {
		h.Logger.Error("count project_patterns failed",
			"project_id", authProjectID, "detector", detector,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not check pattern count")
		return
	}
	if count >= projectPatternMax {
		writeError(w, http.StatusTooManyRequests,
			"max patterns per detector reached (200); delete an existing pattern first")
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	p := &store.ProjectPattern{
		PatternID:   newPatternID(),
		ProjectID:   authProjectID,
		Detector:    detector,
		Pattern:     body.Pattern,
		Severity:    body.Severity,
		Description: body.Description,
		Enabled:     enabled,
		CreatedBy:   userIDFromContextOrEmpty(r),
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		MatchCount:  0,
	}
	if err := h.Store.CreateProjectPattern(r.Context(), p); err != nil {
		h.Logger.Error("create project_pattern failed",
			"project_id", authProjectID, "detector", detector,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not create pattern")
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// HandleUpdatePatternConfig overwrites the mutable fields of an
// existing pattern.
//
//	PATCH /me/pattern-config/{detector}/{pattern_id}
//	Body: same as POST (all fields optional in semantics; we
//	      require pattern/severity to remain present for consistency).
//	→ 200 with updated pattern
//	→ 400 on RE2-invalid / missing fields
//	→ 404 pattern not found (or belongs to another project)
func (h *Handlers) HandleUpdatePatternConfig(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	detector := r.PathValue("detector")
	patternID := r.PathValue("pattern_id")
	if !patternConfigDetectors[detector] {
		writeError(w, http.StatusNotFound,
			"detector must be one of: prompt_injection, data_leakage, sandbox_escape")
		return
	}
	if patternID == "" {
		writeError(w, http.StatusBadRequest, "pattern_id is required")
		return
	}
	var body struct {
		Pattern     string `json:"pattern"`
		Severity    string `json:"severity"`
		Description string `json:"description"`
		Enabled     *bool  `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if validationErr := validatePatternFields(
		body.Pattern, body.Severity, body.Description,
	); validationErr != "" {
		writeError(w, http.StatusBadRequest, validationErr)
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	p := &store.ProjectPattern{
		PatternID:   patternID,
		ProjectID:   authProjectID,
		Detector:    detector,
		Pattern:     body.Pattern,
		Severity:    body.Severity,
		Description: body.Description,
		Enabled:     enabled,
	}
	if err := h.Store.UpdateProjectPattern(r.Context(), p); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "pattern not found")
			return
		}
		h.Logger.Error("update project_pattern failed",
			"project_id", authProjectID, "detector", detector,
			"pattern_id", patternID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not update pattern")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// HandleDeletePatternConfig removes a pattern row.
//
//	DELETE /me/pattern-config/{detector}/{pattern_id}
//	→ 204 on success
//	→ 404 pattern not found (or belongs to another project)
func (h *Handlers) HandleDeletePatternConfig(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	detector := r.PathValue("detector")
	patternID := r.PathValue("pattern_id")
	if !patternConfigDetectors[detector] {
		writeError(w, http.StatusNotFound,
			"detector must be one of: prompt_injection, data_leakage, sandbox_escape")
		return
	}
	if patternID == "" {
		writeError(w, http.StatusBadRequest, "pattern_id is required")
		return
	}
	if err := h.Store.DeleteProjectPattern(
		r.Context(), authProjectID, patternID,
	); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "pattern not found")
			return
		}
		h.Logger.Error("delete project_pattern failed",
			"project_id", authProjectID, "detector", detector,
			"pattern_id", patternID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not delete pattern")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validatePatternFields runs the shared input validation. Returns
// an empty string when input is valid, or a user-facing error
// message describing the first issue found.
func validatePatternFields(pattern, severity, description string) string {
	if strings.TrimSpace(pattern) == "" {
		return "pattern is required"
	}
	if len(pattern) > projectPatternMaxLen {
		return "pattern is too long (max 1000 chars)"
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return "pattern is not a valid RE2 regex: " + err.Error()
	}
	if !projectPatternSeverities[severity] {
		return "severity must be one of: low, medium, high"
	}
	if len(description) > projectPatternMaxDescLen {
		return "description is too long (max 500 chars)"
	}
	return ""
}

// newPatternID generates a server-side pattern identifier.
func newPatternID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "ppat-" + hex.EncodeToString(b[:])
}

// userIDFromContextOrEmpty returns the caller's user_id if the
// request context carries one (session-based auth), or empty
// string for API-key callers. The CreatedBy column is best-effort
// audit metadata, not a security boundary.
func userIDFromContextOrEmpty(r *http.Request) string {
	if uid, ok := UserIDFromContext(r.Context()); ok {
		return uid
	}
	return ""
}
