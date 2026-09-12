// Project read and rename endpoints, and the identity echo.
// Split out of handlers.go on 2026-09-12; every declaration moved verbatim.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"mesedi/backend/internal/store"
)

// HandleGetProject returns the authenticated project's identity. Used
// by the dashboard to show "Project: <name>" in the topbar and welcome
// screens, and by /app/settings to display + (eventually) rename the
// project.
//
// Returns project_id, name, owner_email, created_at. Does not return
// the API key prefix or any sensitive material, the calling client
// already has the key in localStorage and any rename/revoke flows
// happen through other endpoints that already audit-log by key_id.
func (h *Handlers) HandleGetProject(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	project, err := h.Store.GetProject(r.Context(), authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "get project: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"project_id":  project.ProjectID,
		"name":        project.Name,
		"owner_email": project.OwnerEmail,
		"created_at":  project.CreatedAt,
	})
}

// HandleSetProjectName updates the calling project's display name.
// Admin role required (anyone with read scope can call GET /project,
// but a rename is a mutation; only admins can perform it).
//
// Body: { "name": "<new name>" }
// Returns the same payload shape as HandleGetProject after the update.
//
// Validation:
//   - name is trimmed; must be 1-80 characters after trim
//   - empty / whitespace-only is rejected
//   - same 80-char cap as signup so a rename cannot bypass the
//     signup-time bound
//
// Added ship: SSO signup defaults all projects to
// "Default project" because the OAuth flow does not collect a name.
// Customers need a way to rename without re-signing-up.
func (h *Handlers) HandleSetProjectName(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "admin") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	newName := strings.TrimSpace(body.Name)
	if newName == "" {
		writeError(w, http.StatusBadRequest,
			"name is required and cannot be only whitespace")
		return
	}
	if len(newName) > 80 {
		writeError(w, http.StatusBadRequest,
			"name must be 80 characters or fewer")
		return
	}
	if err := h.Store.UpdateProjectName(r.Context(), authProjectID, newName); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError,
			"update project name: "+err.Error())
		return
	}
	h.Logger.Info("project renamed",
		"project_id", authProjectID, "new_name", newName)
	// step C, project rename is admin-tier and changes how the
	// project surfaces in invoices, emails, and the dashboard. Worth
	// the audit row.
	h.recordAuditEvent(r, AuditProjectRename, "project", authProjectID, map[string]any{
		"new_name": newName,
	})
	project, err := h.Store.GetProject(r.Context(), authProjectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"reload project after rename: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"project_id":  project.ProjectID,
		"name":        project.Name,
		"owner_email": project.OwnerEmail,
		"created_at":  project.CreatedAt,
	})
}

// HandleGetMe returns the calling user's identity + role for the
// dashboard. Used by the topbar / overview to show "Signed in as X"
// with the correct member email (not the project owner), and by every
// page to grey out mutation buttons when the role doesn't permit
// them. Without this endpoint, the dashboard renders mutation
// affordances unconditionally and the user only learns they lack
// permission after clicking.
//
// Response shape:
//
//	{
//	  "user_id":     "alice@example.com",  // who is calling
//	  "email":       "alice@example.com",  // same in v1 (email-as-user-id)
//	  "role":        "read",               // read | write | admin
//	  "project_id":  "proj_...",
//	  "project_name":"Acme Production",
//	  "owner_email": "owner@example.com"   // project creator, for context
//	}
//
// Legacy keys (no user_id) and projects without a tenant_id resolve
// to role=admin via the same fallback resolveCallerRole uses, so the
// founder's own integrations don't show "Signed in as null."
func (h *Handlers) HandleGetMe(w http.ResponseWriter, r *http.Request) {
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	project, err := h.Store.GetProject(r.Context(), projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get project: "+err.Error())
		return
	}

	// Resolve the caller's user_id from the API key (post-014 keys
	// carry it). Falls back to the project owner so legacy keys still
	// render meaningfully.
	userID, _ := UserIDFromContext(r.Context())
	if userID == "" {
		userID = project.OwnerUserID
		if userID == "" {
			userID = project.OwnerEmail
		}
	}

	role, err := h.resolveCallerRole(r)
	if err != nil {
		h.Logger.Warn("me: resolve role failed (defaulting to read)",
			"error", err.Error())
		role = "read"
	}
	if role == "" {
		role = "read"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"user_id":      userID,
		"email":        userID,
		"role":         role,
		"project_id":   project.ProjectID,
		"project_name": project.Name,
		"owner_email":  project.OwnerEmail,
	})
}
