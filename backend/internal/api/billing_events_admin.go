// Admin handlers for the billing_events table.
//
// GET  /admin/billing-events                   list events (?include_resolved=true&project_id=)
// POST /admin/billing-events/{id}/resolve      mark resolved with operator note
//
// Backs the Security page's commitment that charge.dispute.created and
// invoice.payment_failed Stripe webhooks "feed into our admin dashboard
// so we can act on fraud signals and dunning cases without polling
// Stripe." The /admin/billing-events page in the dashboard reads
// from these endpoints and posts the resolve when an operator clears a
// signal.

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

// adminBillingEvent mirrors store.BillingEvent but with ISO8601-
// formatted timestamps so the dashboard renders them cleanly without
// parsing unix epoch ints client-side. Same shape used by
// adminAbuseSignal next door.
type adminBillingEvent struct {
	EventID          string  `json:"event_id"`
	ProjectID        string  `json:"project_id"`
	StripeCustomerID string  `json:"stripe_customer_id"`
	Kind             string  `json:"kind"`
	Severity         string  `json:"severity"`
	StripeObjectID   string  `json:"stripe_object_id"`
	AmountCents      int64   `json:"amount_cents"`
	Currency         string  `json:"currency,omitempty"`
	DetailJSON       string  `json:"detail_json,omitempty"`
	ReceivedAt       string  `json:"received_at"`
	ResolvedAt       *string `json:"resolved_at,omitempty"`
	ResolvedBy       string  `json:"resolved_by,omitempty"`
	ResolutionNote   string  `json:"resolution_note,omitempty"`
}

func toAdminBillingEvent(be *store.BillingEvent) adminBillingEvent {
	out := adminBillingEvent{
		EventID:          be.EventID,
		ProjectID:        be.ProjectID,
		StripeCustomerID: be.StripeCustomerID,
		Kind:             be.Kind,
		Severity:         be.Severity,
		StripeObjectID:   be.StripeObjectID,
		AmountCents:      be.AmountCents,
		Currency:         be.Currency,
		DetailJSON:       be.DetailJSON,
		ReceivedAt:       be.ReceivedAt.UTC().Format(time.RFC3339),
		ResolvedBy:       be.ResolvedBy,
		ResolutionNote:   be.ResolutionNote,
	}
	if be.ResolvedAt != nil {
		s := be.ResolvedAt.UTC().Format(time.RFC3339)
		out.ResolvedAt = &s
	}
	return out
}

// HandleAdminListBillingEvents returns billing events sorted newest
// first. By default returns unresolved events only; pass
// ?include_resolved=true to see the full audit trail. Pass
// ?project_id=proj_xxx to scope to a single project's events.
func (h *Handlers) HandleAdminListBillingEvents(w http.ResponseWriter, r *http.Request) {
	includeResolved := r.URL.Query().Get("include_resolved") == "true"
	projectID := r.URL.Query().Get("project_id")
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	events, err := h.Store.ListBillingEvents(r.Context(), store.BillingEventFilter{
		UnresolvedOnly: !includeResolved,
		ProjectID:      projectID,
		Limit:          limit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list billing events: "+err.Error())
		return
	}

	out := make([]adminBillingEvent, 0, len(events))
	for _, e := range events {
		out = append(out, toAdminBillingEvent(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"events": out,
	})
}

// adminResolveBillingEventRequest is the JSON body for
// POST /admin/billing-events/{id}/resolve. Note is the human-written
// explanation surfaced on the resolved-events view (e.g., "refunded
// the dispute via Stripe Dashboard").
type adminResolveBillingEventRequest struct {
	Note string `json:"note,omitempty"`
}

// HandleAdminResolveBillingEvent stamps resolved_at + resolved_by +
// resolution_note on the row. Idempotent on the data layer; a second
// resolve just overwrites the note (operator typo fixes).
func (h *Handlers) HandleAdminResolveBillingEvent(w http.ResponseWriter, r *http.Request) {
	eventID := r.PathValue("id")
	if eventID == "" {
		writeError(w, http.StatusBadRequest, "missing event id")
		return
	}

	var req adminResolveBillingEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, context.Canceled) {
		// Body is optional; an empty POST means "resolve with no
		// note". Only complain about malformed JSON.
		if err.Error() != "EOF" {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}

	// Pre-flight existence check so a stale resolve POST gets a clean
	// 404 instead of a silently-successful UPDATE-of-zero-rows.
	if _, err := h.Store.GetBillingEvent(r.Context(), eventID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "billing event not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "billing event lookup: "+err.Error())
		return
	}

	if err := h.Store.ResolveBillingEvent(r.Context(), eventID, "admin", req.Note); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Race: the row was deleted between our GET and the
			// UPDATE. Vanishingly rare on billing_events (rows are
			// never deleted from the admin path) but report it
			// correctly if it happens.
			writeError(w, http.StatusNotFound, "billing event not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "resolve billing event: "+err.Error())
		return
	}

	h.Logger.Info("admin: billing event resolved",
		"event_id", eventID,
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"event_id": eventID,
	})
}
