// Failure group read, resolve and unresolve endpoints.
// Split out of handlers.go on 2026-09-12; every declaration moved verbatim.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"mesedi/backend/internal/store"
)

// HandleListFailureGroups returns the calling project's failure groups,
// sorted by most-recent first. Supports `limit` (default 50, max 200) and
// `offset` (default 0) via query string.
func (h *Handlers) HandleListFailureGroups(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	limit := parseIntQuery(r, "limit", 50, 1, 200)
	offset := parseIntQuery(r, "offset", 0, 0, 1_000_000)
	q := normalizeListQuery(r.URL.Query().Get("q"))
	includeResolved := r.URL.Query().Get("include_resolved") == "true"

	groups, err := h.Store.ListFailureGroups(r.Context(), authProjectID, store.ListFailureGroupsOpts{
		Q:               q,
		IncludeResolved: includeResolved,
		Limit:           limit,
		Offset:          offset,
	})
	if err != nil {
		h.Logger.Error("list failure_groups failed",
			"project_id", authProjectID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "list failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"failure_groups":   groups,
		"count":            len(groups),
		"limit":            limit,
		"offset":           offset,
		"q":                q,
		"include_resolved": includeResolved,
	})
}

// HandleGetFailureGroup returns a single failure_group by id. Returns 404
// both when the group doesn't exist AND when the group belongs to a
// different project than the caller (don't leak group_id existence
// across tenants).
func (h *Handlers) HandleGetFailureGroup(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	if groupID == "" {
		writeError(w, http.StatusBadRequest, "group_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	group, err := h.Store.GetFailureGroup(r.Context(), groupID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "failure group not found")
			return
		}
		h.Logger.Error("get failure_group failed",
			"group_id", groupID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if group.ProjectID != authProjectID {
		// Don't reveal that the group exists in another project, return
		// 404 same as a non-existent group.
		writeError(w, http.StatusNotFound, "failure group not found")
		return
	}

	writeJSON(w, http.StatusOK, group)
}

// normalizeResolveReason sanitizes an optional customer-supplied
// reason on a resolve / unresolve action (failure-group-resolve-
// context wave). Trims whitespace, strips control chars, caps at
// 512 bytes, a sentence or two is the expected shape. SQL injection
// is not a concern (the value lands in metadata_json via json.Marshal,
// never in a SQL literal), but a multi-MB paste accident would bloat
// the audit row pointlessly.
func normalizeResolveReason(raw string) string {
	const maxLen = 512
	out := strings.TrimSpace(raw)
	if out == "" {
		return ""
	}
	out = strings.Map(func(r rune) rune {
		// Allow tab + newline + carriage return so multi-line
		// reasons render correctly in the dashboard audit log.
		if r == '\t' || r == '\n' || r == '\r' {
			return r
		}
		if r < 0x20 || r == 0x7F {
			return -1
		}
		return r
	}, out)
	if len(out) > maxLen {
		out = out[:maxLen]
	}
	return out
}

// resolveActionRequest is the optional JSON body shape accepted by
// both POST /failure-groups/{id}/resolve and /unresolve. Both fields
// are optional; an empty body is the legacy no-context behavior.
type resolveActionRequest struct {
	Reason string `json:"reason"`
}

// buildResolveAuditMetadata composes the metadata map persisted on
// audit_events.metadata_json for a resolve / unresolve action. Auto-
// captures failure_class + truncated signature so the audit log
// Detail column tells the customer/auditor WHAT was resolved without
// needing to cross-reference the group_id. If the caller passed a
// reason, it lands here too. nil-safe: a failed group lookup just
// degrades to a reason-only or empty metadata (best-effort, never
// fails the resolve action).
func buildResolveAuditMetadata(group *store.FailureGroup, reason string) map[string]any {
	meta := map[string]any{}
	if group != nil {
		meta["failure_class"] = group.FailureClass
		const sigMax = 120
		sig := group.Signature
		if len(sig) > sigMax {
			sig = sig[:sigMax-1] + "…"
		}
		meta["signature_snippet"] = sig
	}
	if reason != "" {
		meta["reason"] = reason
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

// parseResolveBody best-effort decodes the optional JSON body of a
// resolve / unresolve action into a normalized reason string. A
// missing, empty, or malformed body all return "", the action
// proceeds without a reason rather than failing on a bad body.
func parseResolveBody(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	defer r.Body.Close()
	var body resolveActionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return ""
	}
	return normalizeResolveReason(body.Reason)
}

// HandleResolveFailureGroup marks a failure_group as resolved
// (failure-group-resolve wave). Tenant-scoped via the project
// predicate on the store update. Idempotent. Emits an
// audit_events row with auto-captured failure_class + signature
// snippet + optional customer-supplied reason (failure-group-
// resolve-context wave).
func (h *Handlers) HandleResolveFailureGroup(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	if groupID == "" {
		writeError(w, http.StatusBadRequest, "group_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	actorUserID := userIDFromContextOrEmpty(r)
	reason := parseResolveBody(r)

	// Look up the group BEFORE the resolve so the audit metadata
	// can carry failure_class + signature. We tenant-check here too;
	// the resolve itself also tenant-checks (defense in depth) but
	// returning 404 here saves the unnecessary UPDATE and lets the
	// audit emission have full context.
	group, err := h.Store.GetFailureGroup(r.Context(), groupID)
	if err != nil || group == nil || group.ProjectID != authProjectID {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			h.Logger.Error("resolve failure_group: lookup failed",
				"group_id", groupID,
				"error", err.Error())
		}
		writeError(w, http.StatusNotFound, "failure group not found")
		return
	}

	if err := h.Store.ResolveFailureGroup(r.Context(), groupID, authProjectID, actorUserID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "failure group not found")
			return
		}
		h.Logger.Error("resolve failure_group failed",
			"group_id", groupID,
			"project_id", authProjectID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Audit emission is best-effort; a write failure here doesn't
	// fail the customer's resolve action.
	h.recordAuditEvent(r, AuditFailureGroupResolve, "failure_group", groupID,
		buildResolveAuditMetadata(group, reason))

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"group_id": groupID,
		"resolved": true,
	})
}

// HandleUnresolveFailureGroup clears the resolved state on a
// failure_group (failure-group-resolve wave). Same contracts as
// HandleResolveFailureGroup: tenant-scoped, idempotent, audited.
// Accepts the same optional reason body so customers can explain
// why they're re-opening a previously-resolved group.
func (h *Handlers) HandleUnresolveFailureGroup(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	if groupID == "" {
		writeError(w, http.StatusBadRequest, "group_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	reason := parseResolveBody(r)

	// Same pre-lookup as resolve so audit metadata has context.
	group, err := h.Store.GetFailureGroup(r.Context(), groupID)
	if err != nil || group == nil || group.ProjectID != authProjectID {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			h.Logger.Error("unresolve failure_group: lookup failed",
				"group_id", groupID,
				"error", err.Error())
		}
		writeError(w, http.StatusNotFound, "failure group not found")
		return
	}

	if err := h.Store.UnresolveFailureGroup(r.Context(), groupID, authProjectID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "failure group not found")
			return
		}
		h.Logger.Error("unresolve failure_group failed",
			"group_id", groupID,
			"project_id", authProjectID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.recordAuditEvent(r, AuditFailureGroupUnresolve, "failure_group", groupID,
		buildResolveAuditMetadata(group, reason))

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"group_id": groupID,
		"resolved": false,
	})
}
