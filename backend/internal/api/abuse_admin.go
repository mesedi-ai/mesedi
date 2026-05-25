// Admin handlers for the abuse_signals audit table (#172).
//
// GET  /admin/abuse                  list signals (?include_resolved=true)
// POST /admin/abuse/{id}/resolve     mark resolved + optionally reactivate

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"mesedi/backend/internal/store"
)

// adminAbuseSignal mirrors store.AbuseSignal but with ISO8601-formatted
// timestamps so the dashboard renders them cleanly without parsing
// unix epoch ints client-side.
type adminAbuseSignal struct {
	SignalID       string  `json:"signal_id"`
	ProjectID      string  `json:"project_id"`
	Kind           string  `json:"kind"`
	Severity       string  `json:"severity"`
	Detail         string  `json:"detail,omitempty"`
	DetectedAt     string  `json:"detected_at"`
	NotifiedAt     *string `json:"notified_at,omitempty"`
	SuspendedAt    *string `json:"suspended_at,omitempty"`
	ResolvedAt     *string `json:"resolved_at,omitempty"`
	ResolvedBy     string  `json:"resolved_by,omitempty"`
	ResolutionNote string  `json:"resolution_note,omitempty"`
}

func toAdminAbuse(sig *store.AbuseSignal) adminAbuseSignal {
	out := adminAbuseSignal{
		SignalID:       sig.SignalID,
		ProjectID:      sig.ProjectID,
		Kind:           sig.Kind,
		Severity:       sig.Severity,
		Detail:         sig.Detail,
		DetectedAt:     sig.DetectedAt.UTC().Format(time.RFC3339),
		ResolvedBy:     sig.ResolvedBy,
		ResolutionNote: sig.ResolutionNote,
	}
	if sig.NotifiedAt != nil {
		s := sig.NotifiedAt.UTC().Format(time.RFC3339)
		out.NotifiedAt = &s
	}
	if sig.SuspendedAt != nil {
		s := sig.SuspendedAt.UTC().Format(time.RFC3339)
		out.SuspendedAt = &s
	}
	if sig.ResolvedAt != nil {
		s := sig.ResolvedAt.UTC().Format(time.RFC3339)
		out.ResolvedAt = &s
	}
	return out
}

// HandleAdminListAbuseSignals returns abuse signals sorted by most
// recent first. By default returns unresolved signals only; pass
// ?include_resolved=true to include the full audit trail.
func (h *Handlers) HandleAdminListAbuseSignals(w http.ResponseWriter, r *http.Request) {
	includeResolved := r.URL.Query().Get("include_resolved") == "true"
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	sigs, err := h.Store.ListAbuseSignals(r.Context(), !includeResolved, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list abuse signals: "+err.Error())
		return
	}

	out := make([]adminAbuseSignal, 0, len(sigs))
	for _, s := range sigs {
		out = append(out, toAdminAbuse(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"signals": out,
	})
}

// adminResolveRequest is the JSON body for POST /admin/abuse/{id}/resolve.
type adminResolveRequest struct {
	Note       string `json:"note,omitempty"`
	Reactivate bool   `json:"reactivate,omitempty"`
}

// HandleAdminResolveAbuseSignal closes out a signal with the operator's
// note. When the signal had triggered a suspension and Reactivate is
// true, the project's suspended_at is cleared in the same call so the
// customer can immediately resume sending telemetry.
func (h *Handlers) HandleAdminResolveAbuseSignal(w http.ResponseWriter, r *http.Request) {
	signalID := r.PathValue("id")
	if signalID == "" {
		writeError(w, http.StatusBadRequest, "missing signal id")
		return
	}

	var req adminResolveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, context.Canceled) {
		// Body is optional, an empty POST means "resolve with no note,
		// don't reactivate". Only complain about malformed JSON.
		if err.Error() != "EOF" {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}

	sig, err := h.Store.GetAbuseSignal(r.Context(), signalID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "abuse signal not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "abuse lookup: "+err.Error())
		return
	}

	now := time.Now().UTC()
	if err := h.Store.ResolveAbuseSignal(r.Context(), signalID, "admin", req.Note, now); err != nil {
		writeError(w, http.StatusInternalServerError, "resolve abuse signal: "+err.Error())
		return
	}

	reactivated := false
	if req.Reactivate && sig.SuspendedAt != nil {
		if err := h.Store.UnsuspendProject(r.Context(), sig.ProjectID); err != nil {
			writeError(w, http.StatusInternalServerError, "unsuspend project: "+err.Error())
			return
		}
		reactivated = true
		h.Logger.Info("admin: project reactivated via abuse resolve",
			"project_id", sig.ProjectID,
			"signal_id", signalID,
		)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"signal_id":   signalID,
		"reactivated": reactivated,
	})
}
