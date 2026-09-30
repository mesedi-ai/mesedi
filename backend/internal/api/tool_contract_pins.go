package api

// Approval-time tool contract pins: detection hot path plus the REST
// surface (migration 062, store methods in sqlite_tool_pins.go).
//
// Why this exists, in one paragraph: the history-based description
// and definition drift checks need ten calls of baseline, and that
// baseline is built from the traffic itself, so a tool whose
// contract flips on call three is invisible at call three and IS
// the majority by roughly call four. The 2026-09-15 radar named
// this the sharpest open finding. A pin replaces the baseline with
// the operator's approved hash, so the first deviating call fires.
//
// Endpoint surface, all project-scoped through the existing auth
// middleware (ProjectIDFromContext), riding privateHandler's global
// rate limiting with no per-tier cap or tier gating: pins are a
// safety control and every tier gets them.
//
//	GET    /me/tool-pins
//	    → 200 { "pins": [ {tool_name, kind, pinned_hash,
//	            created_at, updated_at}, ... ] }
//
//	PUT    /me/tool-pins/{tool_name}/{kind}
//	    Body: { "pinned_hash": "<64 hex chars>" }
//	    → 200 with the upserted pin
//	    → 400 on unknown kind or malformed hash
//
//	DELETE /me/tool-pins/{tool_name}/{kind}
//	    → 204 on success, 404 when no pin existed
//
// Set and delete both write an audit_event: a pin decides what the
// drift detector treats as the approved contract, so changing one
// is a security-relevant act that belongs in the customer-visible
// audit trail.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"mesedi/backend/internal/detectors"
	"mesedi/backend/internal/store"
)

// checkToolContractPins runs the pinned-contract comparison for one
// tool at execution close. Returns true when a violation fired (and
// has been grouped + webhooked), so the caller can stop with the
// one-drift-signal-per-execution rule. Description before
// definition, mirroring the history checks' precedence: rewritten
// help text is the tool-poisoning read and outranks a schema move.
func (h *Handlers) checkToolContractPins(
	r *http.Request,
	executionID, authProjectID, toolName string,
	pins map[string]string,
) bool {
	type pinCheck struct {
		kind        string
		currentHash func() string
	}
	checks := []pinCheck{
		{
			kind: detectors.PinKindDescription,
			currentHash: func() string {
				current, err := h.Store.ListToolDescriptions(
					r.Context(), authProjectID, toolName, "", 1,
				)
				if err != nil || len(current) == 0 {
					return ""
				}
				return detectors.DescriptionHash(current[0])
			},
		},
		{
			kind: detectors.PinKindDefinition,
			currentHash: func() string {
				current, err := h.Store.ListToolInputSchemaHashes(
					r.Context(), authProjectID, toolName, "", 1,
				)
				if err != nil || len(current) == 0 {
					return ""
				}
				return current[0]
			},
		},
	}
	for _, c := range checks {
		pinnedHash, ok := pins[c.kind]
		if !ok {
			continue
		}
		// An empty current hash (pre-upgrade SDK, no declared
		// schema, store error) is inconclusive, not a violation;
		// DetectPinnedContractDrift declines on it.
		sig, fired := detectors.DetectPinnedContractDrift(
			toolName, c.kind, c.currentHash(), pinnedHash,
		)
		if !fired {
			continue
		}
		isNew, gErr := h.Store.GroupToolSchemaDrift(
			r.Context(), executionID, authProjectID, sig,
		)
		if gErr != nil {
			h.Logger.Warn("tool-contract-pin grouping failed (continuing)",
				"execution_id", executionID,
				"signature", sig,
				"error", gErr.Error(),
			)
		}
		h.maybeFireWebhook(r, authProjectID, store.FailureClassToolSchemaDrift, sig, isNew, gErr)
		return true
	}
	return false
}

// validPinKind reports whether kind is one of the two contract
// halves a pin can cover.
func validPinKind(kind string) bool {
	return kind == detectors.PinKindDefinition ||
		kind == detectors.PinKindDescription
}

// validPinHash reports whether s looks like the SHA-256 hex both
// SDK-computed hashes use: exactly 64 characters, hex alphabet.
// Case-insensitive on input; callers store the lowercased form so
// comparisons against SDK output (lowercase hex) never miss on
// case.
func validPinHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// HandleListToolContractPins returns every pin for the caller's
// project.
func (h *Handlers) HandleListToolContractPins(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	pins, err := h.Store.ListToolContractPins(r.Context(), authProjectID)
	if err != nil {
		h.Logger.Error("list tool_contract_pins failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not list tool pins")
		return
	}
	if pins == nil {
		pins = []*store.ToolContractPin{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pins": pins})
}

// HandleSetToolContractPin upserts the approved contract hash for
// (tool, kind).
func (h *Handlers) HandleSetToolContractPin(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	toolName := r.PathValue("tool_name")
	kind := r.PathValue("kind")
	if toolName == "" {
		writeError(w, http.StatusBadRequest, "tool_name is required")
		return
	}
	if !validPinKind(kind) {
		writeError(w, http.StatusBadRequest,
			"kind must be 'definition' or 'description'")
		return
	}
	var body struct {
		PinnedHash string `json:"pinned_hash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !validPinHash(body.PinnedHash) {
		writeError(w, http.StatusBadRequest,
			"pinned_hash must be 64 hex characters (SHA-256)")
		return
	}
	pinnedHash := strings.ToLower(body.PinnedHash)
	if err := h.Store.UpsertToolContractPin(
		r.Context(), authProjectID, toolName, kind, pinnedHash,
	); err != nil {
		h.Logger.Error("upsert tool_contract_pin failed",
			"project_id", authProjectID, "tool_name", toolName,
			"kind", kind, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not save tool pin")
		return
	}
	h.recordAuditEvent(r, "tool_contract_pin_set", "tool", toolName,
		map[string]any{"kind": kind, "pinned_hash": pinnedHash})
	writeJSON(w, http.StatusOK, map[string]any{
		"tool_name":   toolName,
		"kind":        kind,
		"pinned_hash": pinnedHash,
	})
}

// HandleDeleteToolContractPin removes a pin, reverting that (tool,
// kind) to history-based drift detection.
func (h *Handlers) HandleDeleteToolContractPin(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	toolName := r.PathValue("tool_name")
	kind := r.PathValue("kind")
	if toolName == "" || !validPinKind(kind) {
		writeError(w, http.StatusBadRequest, "unknown tool_name or kind")
		return
	}
	err := h.Store.DeleteToolContractPin(r.Context(), authProjectID, toolName, kind)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no pin for that tool and kind")
		return
	}
	if err != nil {
		h.Logger.Error("delete tool_contract_pin failed",
			"project_id", authProjectID, "tool_name", toolName,
			"kind", kind, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not delete tool pin")
		return
	}
	h.recordAuditEvent(r, "tool_contract_pin_deleted", "tool", toolName,
		map[string]any{"kind": kind})
	w.WriteHeader(http.StatusNoContent)
}
