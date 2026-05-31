package api

// Team / multi-seat handlers (#263 Phase 3).
//
// Admin-side endpoints (require admin role on the auth project's
// org):
//   GET    /me/organization                  org info + member count
//   GET    /me/organization/members          list members
//   PATCH  /me/organization/members/{user}   update role
//   DELETE /me/organization/members/{user}   remove member
//   GET    /me/organization/invites          list pending invites
//   POST   /me/organization/invites          create + email invite
//   DELETE /me/organization/invites/{id}     revoke pending invite
//
// Public invite-accept endpoints (no auth required, token is the
// authentication):
//   GET  /invites/{token}        info about the invite (org, role, expires)
//   POST /invites/{token}/accept redeem; body carries the user's email
//
// Authorization model (v1):
//
//   The dashboard authenticates with a project API key (the existing
//   pre-#263 chain). The handler resolves project_id -> tenant_id
//   (orgs), then verifies the project's owner_user_id is a member of
//   the org with role='admin'. That user is the "current admin" for
//   the duration of this request.
//
//   The invite flow creates organization_members rows tied to the
//   invitee's email (used as user_id until session auth ships). Once
//   session auth (Phase 4+) lands, the dashboard accepts session
//   cookies in addition to API keys, and the role check pivots to
//   "session_user_id is admin in this tenant" without changing the
//   schema or the data model below.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"mesedi/backend/internal/mail"
	"mesedi/backend/internal/store"
)

// inviteTokenBytes is the random-byte size for invite tokens. 24 bytes
// = 48 hex chars; that's well over 2^160 of unguessability.
const inviteTokenBytes = 24

// inviteTTL is how long an invite remains acceptable after creation.
// 7 days matches the most common SaaS norm.
const inviteTTL = 7 * 24 * time.Hour

// validRoles enumerates what UpdateMemberRole + CreateInvite accept.
var validRoles = map[string]bool{"admin": true, "write": true, "read": true}

// resolveAdminContext is the gatekeeper for every admin-only Team
// endpoint. It returns (orgID, callerUserID, ok). On any failure it
// has already written the appropriate HTTP error and the caller
// should return immediately.
func (h *Handlers) resolveAdminContext(w http.ResponseWriter, r *http.Request) (orgID, callerUserID string, ok bool) {
	projectID, hasProject := ProjectIDFromContext(r.Context())
	if !hasProject {
		writeError(w, http.StatusUnauthorized, "no project context")
		return "", "", false
	}

	tenantPtr, err := h.Store.GetProjectTenantID(r.Context(), projectID)
	if err != nil {
		h.Logger.Error("team: get project tenant_id failed",
			"project_id", projectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not resolve org")
		return "", "", false
	}
	if tenantPtr == nil || *tenantPtr == "" {
		// Legacy project with no tenant_id (escaped the migration 013
		// backfill). The customer needs to upgrade to a tier+state
		// that gives them an org; this is a corner case the founder
		// can fix manually.
		writeError(w, http.StatusConflict, "project has no organization (legacy state, contact support)")
		return "", "", false
	}

	p, err := h.Store.GetProject(r.Context(), projectID)
	if err != nil {
		h.Logger.Error("team: get auth project failed",
			"project_id", projectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load project")
		return "", "", false
	}
	if p.OwnerUserID == "" {
		writeError(w, http.StatusConflict, "project has no owner_user_id (legacy state)")
		return "", "", false
	}

	member, err := h.Store.GetOrganizationMember(r.Context(), *tenantPtr, p.OwnerUserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusForbidden, "not a member of this organization")
			return "", "", false
		}
		h.Logger.Error("team: get member failed",
			"org_id", *tenantPtr, "user_id", p.OwnerUserID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load membership")
		return "", "", false
	}
	if member.Role != "admin" {
		writeError(w, http.StatusForbidden, "admin role required")
		return "", "", false
	}

	return *tenantPtr, p.OwnerUserID, true
}

// HandleGetOrganization returns the current org info for the auth
// project's tenant.
func (h *Handlers) HandleGetOrganization(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := h.resolveAdminContext(w, r)
	if !ok {
		return
	}
	org, err := h.Store.GetOrganization(r.Context(), orgID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "organization not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not load organization")
		return
	}
	members, _ := h.Store.ListOrganizationMembers(r.Context(), orgID)
	pendingInvites, _ := h.Store.ListOrganizationInvites(r.Context(), orgID, true)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"organization":   org,
		"member_count":   len(members),
		"pending_invite_count": len(pendingInvites),
	})
}

// HandleListMembers returns every member of the org.
func (h *Handlers) HandleListMembers(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := h.resolveAdminContext(w, r)
	if !ok {
		return
	}
	members, err := h.Store.ListOrganizationMembers(r.Context(), orgID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list members")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"members": members,
	})
}

// HandleUpdateMemberRole flips a member's role. Admin-only.
func (h *Handlers) HandleUpdateMemberRole(w http.ResponseWriter, r *http.Request) {
	orgID, callerUserID, ok := h.resolveAdminContext(w, r)
	if !ok {
		return
	}
	targetUserID := r.PathValue("user")
	if targetUserID == "" {
		writeError(w, http.StatusBadRequest, "user path parameter required")
		return
	}
	// Don't let the caller demote themselves out of admin; would
	// leave the org admin-less if they're the last admin.
	if targetUserID == callerUserID {
		writeError(w, http.StatusForbidden, "cannot change your own role")
		return
	}

	var body struct {
		Role string `json:"role"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if !validRoles[body.Role] {
		writeError(w, http.StatusBadRequest, "role must be admin, write, or read")
		return
	}
	if err := h.Store.UpdateOrganizationMemberRole(r.Context(), orgID, targetUserID, body.Role); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "member not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not update role")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"user_id": targetUserID,
		"role":    body.Role,
	})
}

// HandleRemoveMember kicks a member from the org. Admin-only.
func (h *Handlers) HandleRemoveMember(w http.ResponseWriter, r *http.Request) {
	orgID, callerUserID, ok := h.resolveAdminContext(w, r)
	if !ok {
		return
	}
	targetUserID := r.PathValue("user")
	if targetUserID == "" {
		writeError(w, http.StatusBadRequest, "user path parameter required")
		return
	}
	if targetUserID == callerUserID {
		writeError(w, http.StatusForbidden, "cannot remove yourself; transfer admin first")
		return
	}
	if err := h.Store.RemoveOrganizationMember(r.Context(), orgID, targetUserID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "member not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not remove member")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"user_id": targetUserID,
	})
}

// HandleListInvites returns pending invites for the org.
func (h *Handlers) HandleListInvites(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := h.resolveAdminContext(w, r)
	if !ok {
		return
	}
	invites, err := h.Store.ListOrganizationInvites(r.Context(), orgID, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list invites")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"invites": invites,
	})
}

// HandleCreateInvite generates a token, persists the invite, and
// ships the email via the Mailer.
func (h *Handlers) HandleCreateInvite(w http.ResponseWriter, r *http.Request) {
	orgID, callerUserID, ok := h.resolveAdminContext(w, r)
	if !ok {
		return
	}

	var body struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	body.Email = strings.TrimSpace(strings.ToLower(body.Email))
	if body.Email == "" || !strings.Contains(body.Email, "@") {
		writeError(w, http.StatusBadRequest, "valid email required")
		return
	}
	if body.Role == "" {
		body.Role = "read" // safe default
	}
	if !validRoles[body.Role] {
		writeError(w, http.StatusBadRequest, "role must be admin, write, or read")
		return
	}

	// Generate token + invite_id. Token is the unguessable URL
	// secret; invite_id is the admin-facing handle for revoke.
	tokenBytes := make([]byte, inviteTokenBytes)
	if _, err := rand.Read(tokenBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "could not generate token")
		return
	}
	token := hex.EncodeToString(tokenBytes)
	inviteIDBytes := make([]byte, 8)
	if _, err := rand.Read(inviteIDBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "could not generate id")
		return
	}
	inviteID := "inv_" + hex.EncodeToString(inviteIDBytes)

	now := time.Now().UTC()
	expiresAt := now.Add(inviteTTL)
	inv := &store.OrganizationInvite{
		InviteID:        inviteID,
		OrgID:           orgID,
		Email:           body.Email,
		Role:            body.Role,
		Token:           token,
		InvitedByUserID: callerUserID,
		ExpiresAt:       expiresAt,
	}
	if err := h.Store.CreateOrganizationInvite(r.Context(), inv); err != nil {
		h.Logger.Error("create invite failed",
			"org_id", orgID, "email", body.Email, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not save invite")
		return
	}

	// Fire the email best-effort. Resend failure doesn't roll back
	// the invite row (admin can still resend manually by re-creating).
	if h.Mailer != nil {
		org, _ := h.Store.GetOrganization(r.Context(), orgID)
		orgName := orgID
		if org != nil {
			orgName = org.Name
		}
		acceptURL := strings.TrimRight(h.DashboardURL, "/") +
			"/invites/" + token
		if mailErr := h.Mailer.SendOrgInvite(r.Context(), mail.OrgInviteInput{
			ToEmail:      body.Email,
			OrgName:      orgName,
			InviterEmail: callerUserID, // best handle we have until session auth
			Role:         body.Role,
			AcceptURL:    acceptURL,
			ExpiresAt:    expiresAt,
		}); mailErr != nil {
			h.Logger.Warn("invite email send failed (invite persisted, admin can resend)",
				"invite_id", inviteID, "error", mailErr.Error())
		}
	}

	h.Logger.Info("invite created",
		"invite_id", inviteID,
		"org_id", orgID,
		"email", body.Email,
		"role", body.Role,
		"expires_at", expiresAt.Format(time.RFC3339))

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"invite_id":  inviteID,
		"email":      body.Email,
		"role":       body.Role,
		"expires_at": expiresAt.Format(time.RFC3339),
	})
}

// HandleRevokeInvite deletes a pending invite. Admin-only.
func (h *Handlers) HandleRevokeInvite(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := h.resolveAdminContext(w, r)
	if !ok {
		return
	}
	inviteID := r.PathValue("invite")
	if inviteID == "" {
		writeError(w, http.StatusBadRequest, "invite path parameter required")
		return
	}
	if err := h.Store.RevokeOrganizationInvite(r.Context(), inviteID, orgID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "invite not found or already accepted")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not revoke invite")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"invite_id": inviteID,
	})
}

// HandleGetInviteByToken is the public lookup the accept page uses to
// render "you've been invited to X as Y". No auth required: the
// token IS the authentication.
func (h *Handlers) HandleGetInviteByToken(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		writeError(w, http.StatusBadRequest, "token required")
		return
	}
	inv, err := h.Store.GetOrganizationInviteByToken(r.Context(), token)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "invite not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not load invite")
		return
	}
	if inv.AcceptedAt != nil {
		writeError(w, http.StatusGone, "invite already accepted")
		return
	}
	if time.Now().UTC().After(inv.ExpiresAt) {
		writeError(w, http.StatusGone, "invite expired")
		return
	}
	org, _ := h.Store.GetOrganization(r.Context(), inv.OrgID)
	orgName := inv.OrgID
	if org != nil {
		orgName = org.Name
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"email":       inv.Email,
		"role":        inv.Role,
		"org_id":      inv.OrgID,
		"org_name":    orgName,
		"invited_by":  inv.InvitedByUserID,
		"expires_at":  inv.ExpiresAt.Format(time.RFC3339),
	})
}

// HandleAcceptInvite is the public redeem endpoint. The invitee POSTs
// {email} matching the invite's email, and the row is marked accepted
// + a membership row is created.
//
// Until session auth ships (Phase 4), we use the invitee's email as
// user_id. After session auth, this endpoint will require an active
// session and use the session's user_id instead of the email-as-id.
func (h *Handlers) HandleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		writeError(w, http.StatusBadRequest, "token required")
		return
	}

	var body struct {
		Email string `json:"email"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	body.Email = strings.TrimSpace(strings.ToLower(body.Email))
	if body.Email == "" {
		writeError(w, http.StatusBadRequest, "email required")
		return
	}

	inv, err := h.Store.GetOrganizationInviteByToken(r.Context(), token)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "invite not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not load invite")
		return
	}
	if inv.AcceptedAt != nil {
		writeError(w, http.StatusGone, "invite already accepted")
		return
	}
	if time.Now().UTC().After(inv.ExpiresAt) {
		writeError(w, http.StatusGone, "invite expired")
		return
	}
	if !strings.EqualFold(body.Email, inv.Email) {
		writeError(w, http.StatusForbidden, "email does not match invite")
		return
	}

	// Use the email as user_id for v1 (pre-session-auth). When session
	// auth lands, swap to session_user_id (which will be the user
	// table's primary key seeded with the email at signup).
	userID := body.Email

	// Add the member first; if email-as-user-id matches an existing
	// member, ON CONFLICT updates the role to the invited value.
	if err := h.Store.AddOrganizationMember(r.Context(), &store.OrganizationMember{
		OrgID:           inv.OrgID,
		UserID:          userID,
		Role:            inv.Role,
		Email:           body.Email,
		AddedByUserID:   inv.InvitedByUserID,
	}); err != nil {
		h.Logger.Error("accept invite: add member failed",
			"invite_id", inv.InviteID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not add member")
		return
	}

	// Then mark the invite as accepted. Single-use atomic guarantee
	// in the store; double-clicks return ErrAlreadyAccepted.
	if err := h.Store.MarkInviteAccepted(r.Context(), inv.InviteID, userID); err != nil {
		if errors.Is(err, store.ErrAlreadyAccepted) {
			// Race: another concurrent accept won. Membership row
			// already exists (or just got upserted), so report success
			// to keep the UX clean.
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":      true,
				"org_id":  inv.OrgID,
				"role":    inv.Role,
				"warning": "already accepted",
			})
			return
		}
		h.Logger.Error("accept invite: mark accepted failed",
			"invite_id", inv.InviteID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not mark accepted")
		return
	}

	h.Logger.Info("invite accepted",
		"invite_id", inv.InviteID,
		"org_id", inv.OrgID,
		"email", body.Email,
		"role", inv.Role)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"org_id": inv.OrgID,
		"role":   inv.Role,
	})
}

