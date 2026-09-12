// Admin closed-project audit search and GDPR purge.
// Split out of admin.go on 2026-09-12; every declaration moved verbatim.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mesedi/backend/internal/store"
)

// AdminClosedProjectAuditRow is the wire shape returned by the
// closed-project audit search endpoint. We project AuditEvent into
// a flat shape so the caller does not have to know about
// sql.NullString / sql.NullTime serialization quirks.
type AdminClosedProjectAuditRow struct {
	EventID             string         `json:"event_id"`
	ProjectID           string         `json:"project_id"`
	ProjectNameSnapshot string         `json:"project_name_snapshot,omitempty"`
	ProjectDeletedAt    string         `json:"project_deleted_at,omitempty"`
	ActorKeyID          string         `json:"actor_key_id,omitempty"`
	ActorKeyName        string         `json:"actor_key_name,omitempty"`
	ActorEmail          string         `json:"actor_email,omitempty"`
	Action              string         `json:"action"`
	TargetType          string         `json:"target_type,omitempty"`
	TargetID            string         `json:"target_id,omitempty"`
	Metadata            map[string]any `json:"metadata,omitempty"`
	CreatedAt           string         `json:"created_at"`
}

// AdminClosedProjectAuditResponse wraps the search results with the
// echoed filter so a caller paging through results can confirm what
// the server actually applied (e.g. limit fell back to the default).
type AdminClosedProjectAuditResponse struct {
	Email     string                       `json:"email,omitempty"`
	ProjectID string                       `json:"project_id,omitempty"`
	Limit     int                          `json:"limit"`
	Count     int                          `json:"count"`
	Rows      []AdminClosedProjectAuditRow `json:"rows"`
}

// HandleAdminSearchClosedProjectAudit serves R1 + R2 lookups against
// closed-project audit history. Migration 031 added the survival
// columns (project_name_snapshot, project_deleted_at); this endpoint
// is the read path.
//
// Auth: AdminAuth (legacy token OR admin-scope API key). Customer
// keys do not reach this route.
//
// Query params:
//
//	email     , search every closed project where actor_email = X
//	             (powers R1 account-takeover forensics: "show me
//	              every Close action this victim's email ever fired").
//	project_id, search every audit row for a specific closed project
//	             (powers R2 customer-support response: "user X says
//	              they did not close project Y; show me the close
//	              event and who pressed it").
//	limit     , cap rows (default 100, store-side enforced).
//
// At least one of email or project_id must be present; both empty
// returns 400 (the store would also refuse, but we fail fast at the
// edge with a clearer message).
//
// Response shape: AdminClosedProjectAuditResponse. Rows are ordered
// by created_at DESC (store contract). metadata_json is parsed into
// a typed map; on parse failure we drop the field rather than 500
// the whole row (an unreadable metadata blob shouldn't hide the
// audit row itself from the operator).
//
// Follow-up: ships the dashboard UI on top of this
// endpoint. For now staff curl it directly.
func (h *Handlers) HandleAdminSearchClosedProjectAudit(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.URL.Query().Get("email"))
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	if email == "" && projectID == "" {
		writeError(w, http.StatusBadRequest,
			"at least one of email or project_id is required")
		return
	}

	limit := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest,
				"limit must be a non-negative integer")
			return
		}
		limit = n
	}

	// NOTE: do not name this slice "events" -- the package imports
	// github.com/mesedi-ai/mesedi/backend/attest/events and the local
	// name would shadow that package for the rest of the function.
	auditRows, err := h.Store.SearchClosedProjectAuditEvents(
		r.Context(), store.ClosedProjectAuditFilter{
			Email:     email,
			ProjectID: projectID,
			Limit:     limit,
		},
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"search closed project audit: "+err.Error())
		return
	}

	rows := make([]AdminClosedProjectAuditRow, 0, len(auditRows))
	for _, ev := range auditRows {
		row := AdminClosedProjectAuditRow{
			EventID:             ev.EventID,
			ProjectID:           ev.ProjectID,
			ProjectNameSnapshot: ev.ProjectNameSnapshot,
			ActorKeyID:          ev.ActorKeyID,
			ActorKeyName:        ev.ActorKeyName,
			ActorEmail:          ev.ActorEmail,
			Action:              ev.Action,
			TargetType:          ev.TargetType,
			TargetID:            ev.TargetID,
			CreatedAt:           ev.CreatedAt.UTC().Format(time.RFC3339),
		}
		if ev.ProjectDeletedAt != nil {
			row.ProjectDeletedAt = ev.ProjectDeletedAt.UTC().Format(time.RFC3339)
		}
		if ev.MetadataJSON != "" {
			var md map[string]any
			if jerr := json.Unmarshal([]byte(ev.MetadataJSON), &md); jerr == nil {
				row.Metadata = md
			}
		}
		rows = append(rows, row)
	}

	effectiveLimit := limit
	if effectiveLimit == 0 {
		effectiveLimit = 100
	}
	writeJSON(w, http.StatusOK, AdminClosedProjectAuditResponse{
		Email:     email,
		ProjectID: projectID,
		Limit:     effectiveLimit,
		Count:     len(rows),
		Rows:      rows,
	})
}

// AdminGDPRPurgeRequest is the JSON body shape for the GDPR purge
// endpoint. The reason field is optional but strongly encouraged for
// the meta-audit-event recorded against the _admin system project.
// Carrying the support ticket number or the original customer email
// here is good practice; the field is stored verbatim.
type AdminGDPRPurgeRequest struct {
	Reason string `json:"reason,omitempty"`
}

// AdminGDPRPurgeResponse is the success shape. The handler also emits
// a meta-audit-event so a future regulator audit can prove "we deleted
// N rows for project X on Y by admin actor Z".
type AdminGDPRPurgeResponse struct {
	ProjectID  string `json:"project_id"`
	RowsPurged int64  `json:"rows_purged"`
	PurgedAt   string `json:"purged_at"`
	PurgedBy   string `json:"purged_by,omitempty"`
}

// HandleAdminGDPRPurgeClosedProjectAudit hard-deletes every audit_events
// row for the supplied projectID and records a meta-audit-event for
// the deletion itself.
//
// Auth: AdminAuth (legacy token OR admin-scope API key). Customer
// keys do not reach this route.
//
// Path: POST /admin/projects/{id}/audit-events/purge
//
// Body shape: AdminGDPRPurgeRequest (optional reason).
//
// Response codes:
//
//	200 OK on success with AdminGDPRPurgeResponse body.
//	400 Bad Request when projectID is missing from path.
//	422 Unprocessable Entity when projectID still exists in the
//	    projects table (i.e. the project has not been closed). The
//	    customer must close the account first via the normal
//	    HandleCloseAccount flow before GDPR purge is allowed.
//	500 Internal Server Error for DB failures.
//
// Idempotency: re-running the purge after a previous successful run
// returns 200 with RowsPurged=0 (no rows left to delete). That makes
// retries safe.
//
// Compliance posture (the meta-audit-event):
//
//	Action     = AuditAuditGDPRPurge
//	ProjectID  = store.APIKeyAdminProjectID ("_admin" system project)
//	TargetType = "project"
//	TargetID   = <the purged project id>
//	ActorEmail = AuditActorPlatformAdmin (synthetic sentinel)
//	Metadata   = { "rows_purged": N, "reason": "...", "admin_key_id": "...",
//	               "admin_key_name": "...", "admin_auth_method": "..." }
//
// This row survives the deletion because it is owned by the _admin
// system project, not the purged project. It is also covered by the
// 7-year retention scheduler so it eventually rotates out per
// the same policy.
func (h *Handlers) HandleAdminGDPRPurgeClosedProjectAudit(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "missing project id")
		return
	}

	// Body is optional. An empty/missing body is allowed (reason ""):
	// the support ticket may have all the context elsewhere. We do
	// NOT require a body so curl/scripted operators can fire-and-
	// forget when needed.
	var req AdminGDPRPurgeRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "decode body: "+err.Error())
			return
		}
	}

	deleted, err := h.Store.PurgeAuditEventsForClosedProject(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, store.ErrProjectStillActive) {
			// Operator footgun guard: refusing prevents an irreversible
			// purge of a paying customer's audit history. Phrased in
			// terms of the projects table because that is what the
			// store guard actually checks after, a project with
			// zero audit_events yet but a row in `projects` is still
			// active and must be closed first.
			writeError(w, http.StatusUnprocessableEntity,
				"project still active; close the account first via /app/settings before requesting GDPR purge")
			return
		}
		writeError(w, http.StatusInternalServerError,
			"purge audit events: "+err.Error())
		return
	}

	// Meta-audit-event for the purge itself (paper trail). We use the
	// no-request variant because it cleanly attaches to the _admin
	// system project regardless of the caller's request context.
	// Synthetic actor email (AuditActorPlatformAdmin) matches the
	// convention for platform-admin actions; the specific admin key
	// name + id go into the metadata blob.
	metadata := map[string]any{
		"rows_purged": deleted,
		"reason":      req.Reason,
	}
	if v := r.Context().Value(ctxKeyAdminAuthMethod); v != nil {
		if m, ok := v.(string); ok && m != "" {
			metadata["admin_auth_method"] = m
		}
	}
	if v := r.Context().Value(ctxKeyAdminKeyID); v != nil {
		if s, ok := v.(string); ok && s != "" {
			metadata["admin_key_id"] = s
		}
	}
	if v := r.Context().Value(ctxKeyAdminKeyName); v != nil {
		if s, ok := v.(string); ok && s != "" {
			metadata["admin_key_name"] = s
		}
	}
	h.recordAuditEventForProject(
		r.Context(),
		store.APIKeyAdminProjectID,
		AuditActorPlatformAdmin,
		AuditAuditGDPRPurge,
		"project",
		projectID,
		metadata,
	)

	purgedBy, _ := metadata["admin_key_name"].(string)
	writeJSON(w, http.StatusOK, AdminGDPRPurgeResponse{
		ProjectID:  projectID,
		RowsPurged: deleted,
		PurgedAt:   time.Now().UTC().Format(time.RFC3339),
		PurgedBy:   purgedBy,
	})
}
