package api

// Per-project detector-allowlist API for the three detectors that
// share the Allowlist primitive (crashes, tool_failures,
// validator_failures). Closes crashes.G3 + tool_failures.G4 +
// validator_failures.G5 with one shared feature. Storage + REST
// + validation here in Allowlist.a; detector wiring in
// Allowlist.b; dashboard editor in Allowlist.c.
//
// Endpoint surface:
//
//	GET    /me/allowlist/{detector}                list entries
//	POST   /me/allowlist/{detector}                create entry
//	PATCH  /me/allowlist/{detector}/{allowlist_id} update entry
//	DELETE /me/allowlist/{detector}/{allowlist_id} delete entry
//
// {detector} must be one of: crashes, tool_failures,
// validator_failures. Unknown detector returns 404.
//
// allowlist_key semantics:
//   - crashes → exception_type (e.g. "ValueError")
//   - tool_failures → tool_name (e.g. "my_search_tool")
//   - validator_failures → validator_name
//
// If detector signature granularity later expands (tool_failures.G3
// / validator_failures.G3), the allowlist_key column stays as
// opaque string and the new signature shape just becomes the new
// allowlist_key — no schema change. Backward-compat for existing
// allowlist entries preserved.
//
// Server-side enforcement:
//   - PROJECT_ALLOWLIST_MAX = 200 per (project, detector). Generous
//     for any realistic allowlist size; tight enough to prevent
//     runaway-write DoS.
//   - allowlist_key required + capped at 200 chars.
//   - reason capped at 500 chars.
//   - allowlist_key control characters stripped (defensive).

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"

	"mesedi/backend/internal/store"
)

// Detectors that share the Allowlist primitive. Allow-list at the
// API edge so a future fourth allowlist-supporting detector drops
// in with no store-layer change.
var allowlistDetectors = map[string]bool{
	"crashes":            true,
	"tool_failures":      true,
	"validator_failures": true,
}

// projectAllowlistMax caps the number of allowlist entries per
// (project, detector). Same value + same rationale as
// projectPatternMax for the Wave 2.1 pattern-config primitive.
const projectAllowlistMax = 200

const (
	projectAllowlistKeyMaxLen    = 200
	projectAllowlistReasonMaxLen = 500
)

// HandleListAllowlist returns every allowlist entry for the
// calling project's selected detector.
//
//	GET /me/allowlist/{detector}
//	→ 200 {"project_id": "...", "detector": "...", "entries": [...]}
//	→ 404 if detector is not in the allow-list
func (h *Handlers) HandleListAllowlist(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	detector := r.PathValue("detector")
	if !allowlistDetectors[detector] {
		writeError(w, http.StatusNotFound,
			"detector must be one of: crashes, tool_failures, validator_failures")
		return
	}
	entries, err := h.Store.ListProjectAllowlist(
		r.Context(), authProjectID, detector,
	)
	if err != nil {
		h.Logger.Error("list project_detector_allowlist failed",
			"project_id", authProjectID, "detector", detector,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError,
			"could not list allowlist entries")
		return
	}
	if entries == nil {
		entries = []*store.AllowlistEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": authProjectID,
		"detector":   detector,
		"entries":    entries,
	})
}

// HandleCreateAllowlist inserts a new allowlist row for the
// calling project's selected detector.
//
//	POST /me/allowlist/{detector}
//	Body: {"allowlist_key": "...", "reason": "..."}
//	→ 201 with new entry (including server-assigned allowlist_id)
//	→ 400 on missing/invalid key or oversized reason
//	→ 404 unknown detector
//	→ 429 PROJECT_ALLOWLIST_MAX reached
func (h *Handlers) HandleCreateAllowlist(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	detector := r.PathValue("detector")
	if !allowlistDetectors[detector] {
		writeError(w, http.StatusNotFound,
			"detector must be one of: crashes, tool_failures, validator_failures")
		return
	}
	var body struct {
		AllowlistKey string `json:"allowlist_key"`
		Reason       string `json:"reason"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	key, validationErr := sanitizeAllowlistKey(body.AllowlistKey)
	if validationErr != "" {
		writeError(w, http.StatusBadRequest, validationErr)
		return
	}
	if len(body.Reason) > projectAllowlistReasonMaxLen {
		writeError(w, http.StatusBadRequest,
			"reason exceeds 500 characters")
		return
	}
	// Enforce PROJECT_ALLOWLIST_MAX before insert.
	count, err := h.Store.CountProjectAllowlistEntries(
		r.Context(), authProjectID, detector,
	)
	if err != nil {
		h.Logger.Error("count project_detector_allowlist failed",
			"project_id", authProjectID, "detector", detector,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError,
			"could not check allowlist count")
		return
	}
	if count >= projectAllowlistMax {
		writeError(w, http.StatusTooManyRequests,
			"max allowlist entries per detector reached (200); delete an existing entry first")
		return
	}
	entry := &store.AllowlistEntry{
		AllowlistID:  newAllowlistID(),
		ProjectID:    authProjectID,
		Detector:     detector,
		AllowlistKey: key,
		Reason:       body.Reason,
		CreatedBy:    userIDFromContextOrEmpty(r),
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		MatchCount:   0,
	}
	if err := h.Store.CreateProjectAllowlistEntry(r.Context(), entry); err != nil {
		h.Logger.Error("create project_detector_allowlist failed",
			"project_id", authProjectID, "detector", detector,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError,
			"could not create allowlist entry")
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

// HandleUpdateAllowlist overwrites the mutable fields (allowlist_key,
// reason) of an existing entry.
//
//	PATCH /me/allowlist/{detector}/{allowlist_id}
//	Body: {"allowlist_key": "...", "reason": "..."}
//	→ 200 with updated entry
//	→ 400 on missing/invalid key or oversized reason
//	→ 404 entry not found OR detector mismatch
func (h *Handlers) HandleUpdateAllowlist(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	detector := r.PathValue("detector")
	allowlistID := r.PathValue("allowlist_id")
	if !allowlistDetectors[detector] {
		writeError(w, http.StatusNotFound,
			"detector must be one of: crashes, tool_failures, validator_failures")
		return
	}
	if allowlistID == "" {
		writeError(w, http.StatusBadRequest, "missing allowlist_id")
		return
	}
	// Confirm the row exists AND belongs to this (project, detector).
	// Prevents cross-detector updates via path manipulation.
	existing, err := h.Store.GetProjectAllowlistEntry(
		r.Context(), authProjectID, allowlistID,
	)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "allowlist entry not found")
			return
		}
		h.Logger.Error("get project_detector_allowlist failed",
			"project_id", authProjectID, "allowlist_id", allowlistID,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError,
			"could not load allowlist entry")
		return
	}
	if existing.Detector != detector {
		writeError(w, http.StatusNotFound,
			"allowlist entry not found in this detector's allowlist")
		return
	}
	var body struct {
		AllowlistKey string `json:"allowlist_key"`
		Reason       string `json:"reason"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	key, validationErr := sanitizeAllowlistKey(body.AllowlistKey)
	if validationErr != "" {
		writeError(w, http.StatusBadRequest, validationErr)
		return
	}
	if len(body.Reason) > projectAllowlistReasonMaxLen {
		writeError(w, http.StatusBadRequest,
			"reason exceeds 500 characters")
		return
	}
	existing.AllowlistKey = key
	existing.Reason = body.Reason
	if err := h.Store.UpdateProjectAllowlistEntry(r.Context(), existing); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "allowlist entry not found")
			return
		}
		h.Logger.Error("update project_detector_allowlist failed",
			"project_id", authProjectID, "allowlist_id", allowlistID,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError,
			"could not update allowlist entry")
		return
	}
	writeJSON(w, http.StatusOK, existing)
}

// HandleDeleteAllowlist removes an allowlist entry.
//
//	DELETE /me/allowlist/{detector}/{allowlist_id}
//	→ 204 on success
//	→ 404 entry not found OR detector mismatch
func (h *Handlers) HandleDeleteAllowlist(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	detector := r.PathValue("detector")
	allowlistID := r.PathValue("allowlist_id")
	if !allowlistDetectors[detector] {
		writeError(w, http.StatusNotFound,
			"detector must be one of: crashes, tool_failures, validator_failures")
		return
	}
	if allowlistID == "" {
		writeError(w, http.StatusBadRequest, "missing allowlist_id")
		return
	}
	// Detector-mismatch guard (same shape as UPDATE).
	existing, err := h.Store.GetProjectAllowlistEntry(
		r.Context(), authProjectID, allowlistID,
	)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "allowlist entry not found")
			return
		}
		h.Logger.Error("get project_detector_allowlist failed",
			"project_id", authProjectID, "allowlist_id", allowlistID,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError,
			"could not load allowlist entry")
		return
	}
	if existing.Detector != detector {
		writeError(w, http.StatusNotFound,
			"allowlist entry not found in this detector's allowlist")
		return
	}
	if err := h.Store.DeleteProjectAllowlistEntry(
		r.Context(), authProjectID, allowlistID,
	); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "allowlist entry not found")
			return
		}
		h.Logger.Error("delete project_detector_allowlist failed",
			"project_id", authProjectID, "allowlist_id", allowlistID,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError,
			"could not delete allowlist entry")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sanitizeAllowlistKey validates + cleans a customer-supplied
// allowlist_key. Returns (cleaned, "") on success; ("", error
// message) on validation failure. Control characters are stripped
// defensively (an allowlist_key with \x00 or \t shouldn't match
// any real signature, but rejecting them up-front is clearer than
// silently mangling).
func sanitizeAllowlistKey(raw string) (string, string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "allowlist_key is required"
	}
	if len(trimmed) > projectAllowlistKeyMaxLen {
		return "", "allowlist_key exceeds 200 characters"
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return "", "allowlist_key contains control characters"
		}
	}
	return trimmed, ""
}

// newAllowlistID produces a server-side allowlist identifier in the
// "allow_<32 hex>" format (same shape as patternID + auditID).
func newAllowlistID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Reader failure is exceptionally rare on Linux/macOS;
		// fall back to a timestamp-only ID rather than panic.
		ts := time.Now().UTC().UnixNano()
		return "allow_" + hexFromInt64(ts)
	}
	return "allow_" + hex.EncodeToString(b[:])
}
