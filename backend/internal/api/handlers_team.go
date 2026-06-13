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
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
//
// Self-heal path: projects created in the brief window between deploy
// and migration 013, or projects whose 013 backfill skipped them
// because their owner_user_id was NULL, end up with no tenant_id and
// hit a hard 409 on first /me/organization call. Instead of stranding
// the customer, this function auto-bootstraps an org for any project
// that has at least an owner_email. The bootstrap is idempotent:
// org_id = "org_" + project_id, so a repeat call after a partial
// failure picks up where it left off.
func (h *Handlers) resolveAdminContext(w http.ResponseWriter, r *http.Request) (orgID, callerUserID string, ok bool) {
	projectID, hasProject := ProjectIDFromContext(r.Context())
	if !hasProject {
		writeError(w, http.StatusUnauthorized, "no project context")
		return "", "", false
	}

	p, err := h.Store.GetProject(r.Context(), projectID)
	if err != nil {
		h.Logger.Error("team: get auth project failed",
			"project_id", projectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load project")
		return "", "", false
	}

	tenantPtr, err := h.Store.GetProjectTenantID(r.Context(), projectID)
	if err != nil {
		h.Logger.Error("team: get project tenant_id failed",
			"project_id", projectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not resolve org")
		return "", "", false
	}

	// Self-heal: project has no tenant_id but we know its owner_email
	// (signup always sets this). Bootstrap a fresh org + admin member,
	// then point projects.tenant_id at the new row. The bootstrap mirrors
	// the 013 backfill exactly so legacy and self-healed projects are
	// indistinguishable downstream.
	if tenantPtr == nil || *tenantPtr == "" {
		bootstrapped, hErr := h.bootstrapOrgForProject(r.Context(), p)
		if hErr != nil {
			h.Logger.Error("team: bootstrap org failed",
				"project_id", projectID, "error", hErr.Error())
			writeError(w, http.StatusConflict, "project has no organization (legacy state, contact support)")
			return "", "", false
		}
		tenantPtr = &bootstrapped
		h.Logger.Info("team: bootstrapped org for legacy project",
			"project_id", projectID, "org_id", bootstrapped)
	}

	// The caller's identity comes from the API key's user_id (set at
	// mint time for post-014 keys). Falls back to project.OwnerUserID
	// or OwnerEmail for legacy pre-014 keys that haven't been
	// re-minted -- those keys will still resolve to the project
	// owner / admin until the customer rotates them. This is the
	// pivot from "all keys authenticate as project owner" to
	// "each key authenticates as a specific member."
	caller, _ := UserIDFromContext(r.Context())
	if caller == "" {
		caller = p.OwnerUserID
		if caller == "" {
			caller = p.OwnerEmail
		}
	}
	if caller == "" {
		writeError(w, http.StatusConflict, "project has no owner identity (legacy state, contact support)")
		return "", "", false
	}

	member, err := h.Store.GetOrganizationMember(r.Context(), *tenantPtr, caller)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusForbidden, "not a member of this organization")
			return "", "", false
		}
		h.Logger.Error("team: get member failed",
			"org_id", *tenantPtr, "user_id", caller, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load membership")
		return "", "", false
	}
	if member.Role != "admin" {
		writeError(w, http.StatusForbidden, "admin role required")
		return "", "", false
	}

	return *tenantPtr, caller, true
}

// bootstrapOrgForProject creates a fresh organization for a project
// that escaped migration 013, then attaches the project to it. The
// project's owner becomes the admin member. Idempotent: org_id is
// deterministic from project_id, and CreateOrganization is a no-op
// if the row already exists (returns nil for duplicate primary key).
//
// Returns the org_id on success. Used by:
//   - resolveAdminContext (self-heal on first /me/organization call)
//   - signup handler (bootstrap at project-create time so subsequent
//     calls never hit the self-heal path)
func (h *Handlers) bootstrapOrgForProject(ctx context.Context, p *store.Project) (string, error) {
	// Pick a user_id for the admin row. Use owner_user_id if it's
	// already populated, else fall back to owner_email (the same
	// convention the invite-accept flow uses pre-session-auth).
	adminUserID := p.OwnerUserID
	if adminUserID == "" {
		adminUserID = p.OwnerEmail
	}
	if adminUserID == "" {
		return "", errors.New("project has no owner identity to use as admin")
	}

	orgID := "org_" + p.ProjectID
	name := p.Name
	if name == "" {
		name = "Personal"
	}

	// Create the org. If it already exists (idempotent retry path), the
	// store layer surfaces a duplicate-key error which we swallow.
	if err := h.Store.CreateOrganization(ctx, &store.Organization{
		OrgID:           orgID,
		Name:            name,
		CreatedByUserID: adminUserID,
	}); err != nil && !isDuplicateOrgErr(err) {
		return "", err
	}

	// Add admin member. AddOrganizationMember is upsert-on-conflict, so
	// a repeat call refreshes the role/email without erroring.
	if err := h.Store.AddOrganizationMember(ctx, &store.OrganizationMember{
		OrgID:  orgID,
		UserID: adminUserID,
		Role:   "admin",
		Email:  p.OwnerEmail,
	}); err != nil {
		return "", err
	}

	// Link the project. If tenant_id was already set to something other
	// than this org (unlikely), we'd overwrite it; the only way this
	// helper runs is when tenant_id is empty.
	if err := h.Store.SetProjectTenantID(ctx, p.ProjectID, orgID); err != nil {
		return "", err
	}
	return orgID, nil
}

// isDuplicateOrgErr returns true when the store returns a primary-key
// conflict for organizations. Different drivers surface this
// differently, so we string-match the common shapes.
func isDuplicateOrgErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value") ||
		strings.Contains(msg, "already exists")
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
		"ok":                   true,
		"organization":         org,
		"member_count":         len(members),
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
	// #207 step C — role changes alter who can manage billing, keys,
	// webhooks, and team. High security value; recorded against the
	// calling admin's project audit log.
	h.recordAuditEvent(r, AuditTeamRoleUpdate, "member", targetUserID, map[string]any{
		"org_id":   orgID,
		"new_role": body.Role,
	})
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
	// Last-admin protection (#188 Robert flagged): if removing this
	// member would leave the org with zero admin-role members, refuse.
	// An org without an admin can't manage billing, settings, or
	// invites; only the close-account flow is allowed to nuke the
	// last admin.
	members, mlErr := h.Store.ListOrganizationMembers(r.Context(), orgID)
	if mlErr != nil {
		writeError(w, http.StatusInternalServerError,
			"could not verify admin count: "+mlErr.Error())
		return
	}
	adminsRemainingAfter := 0
	targetIsAdmin := false
	for _, m := range members {
		if m.UserID == targetUserID {
			if m.Role == "admin" {
				targetIsAdmin = true
			}
			continue
		}
		if m.Role == "admin" {
			adminsRemainingAfter++
		}
	}
	if targetIsAdmin && adminsRemainingAfter == 0 {
		writeError(w, http.StatusConflict,
			"cannot remove the only admin; promote another member to admin first, or close the account from settings")
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

	// Revoke every API key the removed member holds. Without this, a
	// removed teammate's existing key continues to authenticate against
	// /me/* until it's manually deleted, so they can still browse the
	// dashboard and (worse) call write endpoints (#187 Robert flagged).
	// Hard-deleting matches the existing DeleteAPIKey pattern; a new
	// invite is required for them to re-join, which mints a fresh key.
	// Best-effort: if the revoke fails we still report success on the
	// member removal but log the failure so an operator can clean up.
	revoked, revokeErr := h.Store.DeleteAPIKeysByUserID(r.Context(), targetUserID)
	if revokeErr != nil {
		h.Logger.Error("revoke api_keys after member remove failed",
			"org_id", orgID,
			"removed_user_id", targetUserID,
			"removed_by", callerUserID,
			"error", revokeErr.Error())
	} else if revoked > 0 {
		h.Logger.Info("revoked api_keys for removed member",
			"org_id", orgID,
			"removed_user_id", targetUserID,
			"removed_by", callerUserID,
			"keys_revoked", revoked)
	}
	// #213 Batch 2: ALSO kill every dashboard session the removed
	// member has open. Without this, the kicked-out member can
	// continue using whatever browser tab they had pointed at
	// /app until the cookie TTL expires (up to 7 days). Best-
	// effort: log on failure but never block the removal.
	sessionsRevoked := 0
	if n, sErr := h.Store.DeleteSessionsByUserID(r.Context(), targetUserID); sErr != nil {
		h.Logger.Error("kill sessions after member remove failed",
			"org_id", orgID,
			"removed_user_id", targetUserID,
			"error", sErr.Error())
	} else {
		sessionsRevoked = n
	}

	// #207 step C — removing a member is a top-tier security action
	// (revokes their dashboard access AND every API key they hold).
	// Captured after the keys are best-effort revoked so a partial
	// removal still leaves a row, with revoked-count in the metadata
	// for forensics. #213 Batch 2 adds sessions_revoked.
	h.recordAuditEvent(r, AuditTeamMemberRemove, "member", targetUserID, map[string]any{
		"org_id":           orgID,
		"keys_revoked":     revoked,
		"sessions_revoked": sessionsRevoked,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"user_id":      targetUserID,
		"keys_revoked": revoked,
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
	// PL6 — Hobby is single-user (1 project, 1 person). Multi-seat
	// is a Team-tier capability. We look up the calling project's
	// tier and refuse invite creation if it is Hobby. A failed
	// GetProject is treated as fail-open (rather than block the
	// invite incorrectly) since the caller already passed admin
	// auth and the project clearly exists.
	if authProjectID, hasProject := ProjectIDFromContext(r.Context()); hasProject {
		if p, err := h.Store.GetProject(r.Context(), authProjectID); err == nil && p != nil {
			if normalizeTier(p.Tier) == TierHobby {
				writeError(w, http.StatusPaymentRequired,
					"team invites are a Cloud Team feature; Hobby is 1 project, 1 person. Upgrade at /app/billing to invite teammates.")
				return
			}
		}
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
	// #207 step C — invite-create is the moment a new email is granted
	// future access at a specific role. The single most useful row in
	// the team-management audit set. Captured after the row persists
	// so a failed insert never produces a misleading audit entry.
	h.recordAuditEvent(r, AuditTeamInviteCreate, "invite", inviteID, map[string]any{
		"org_id":      orgID,
		"email":       body.Email,
		"role":        body.Role,
		"expires_at":  expiresAt.Format(time.RFC3339),
	})

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
	// #207 step C — invite-revoke kills the pending access grant. Worth
	// the row so the audit reader can see "invite for alice@... was
	// revoked before she accepted." No metadata payload; the invite_id
	// alone is enough context (the original create row holds the rest).
	h.recordAuditEvent(r, AuditTeamInviteRevoke, "invite", inviteID, map[string]any{
		"org_id": orgID,
	})
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
		"ok":         true,
		"email":      inv.Email,
		"role":       inv.Role,
		"org_id":     inv.OrgID,
		"org_name":   orgName,
		"invited_by": inv.InvitedByUserID,
		"expires_at": inv.ExpiresAt.Format(time.RFC3339),
	})
}

// HandleAcceptInvite is the public redeem endpoint. The invitee POSTs
// {email} matching the invite's email, and the row is marked accepted
// + a membership row is created.
//
// Until session auth ships (Phase 4), we use the invitee's email as
// user_id. After session auth, this endpoint will require an active
// session and use the session's user_id instead of the email-as-id.
//
// #207 step C — NOT audit-logged in v1.5. The audit_events table
// requires project_id NOT NULL, but the accept endpoint runs with no
// caller project context (token-only public auth, the invitee has no
// API key yet) and a multi-project org has no canonical primary
// project to attach the row to. Parking-lot item PL5 tracks this:
// either move accept rows to a separate org-scoped audit table, or
// pick a deterministic project per org (oldest by created_at). The
// accept is partially observable today via the prior invite_create
// audit row plus the org_members row inserted here.
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
		OrgID:         inv.OrgID,
		UserID:        userID,
		Role:          inv.Role,
		Email:         body.Email,
		AddedByUserID: inv.InvitedByUserID,
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
			// to keep the UX clean. Skip the API-key mint on the
			// duplicate path -- we can't know which key the first
			// winner saw, so re-minting would just confuse the user
			// with a second one. They can rotate from /app/api-keys
			// if they need fresh credentials.
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

	// Mint a fresh API key for the invitee so they can sign in to the
	// dashboard. v0.1 auth is project-API-key-based: an invite without
	// a key is a dead end. We attach the key to the first project in
	// the org (today this is always exactly one project; when multi-
	// project orgs land, the admin can choose the scope at invite time).
	// Best-effort: if mint or persist fails, the invitee is still a
	// member and can ask the admin to generate a key from /app/api-keys.
	var rawKey, keyPrefix string
	projects, _ := h.Store.ListProjectsByTenant(r.Context(), inv.OrgID)
	if len(projects) > 0 {
		targetProject := projects[0]
		raw, hash, prefix, mintErr := MintAPIKey()
		if mintErr != nil {
			h.Logger.Warn("accept invite: mint key failed (member still added)",
				"invite_id", inv.InviteID, "error", mintErr.Error())
		} else {
			now := time.Now().UTC()
			keyID := fmt.Sprintf("key-%s-%d", prefix[len("mesedi_sk_"):], now.UnixNano())
			rec := &store.APIKey{
				KeyID:     keyID,
				ProjectID: targetProject.ProjectID,
				KeyHash:   hash,
				KeyPrefix: prefix,
				Name:      "Invite key for " + body.Email,
				// Stamp the invitee's identity (email-as-user-id pre-
				// session-auth). This is what makes the role check in
				// resolveAdminContext actually scope to the invitee
				// instead of the project owner. Without this line,
				// every minted key effectively grants admin via the
				// owner_user_id fallback.
				UserID: userID, // userID := body.Email above
			}
			if persistErr := h.Store.CreateAPIKey(r.Context(), rec); persistErr != nil {
				h.Logger.Warn("accept invite: persist key failed (member still added)",
					"invite_id", inv.InviteID, "error", persistErr.Error())
			} else {
				rawKey = raw
				keyPrefix = prefix
				h.Logger.Info("accept invite: minted key for new member",
					"invite_id", inv.InviteID,
					"project_id", targetProject.ProjectID,
					"key_prefix", prefix)
			}
		}
	}

	h.Logger.Info("invite accepted",
		"invite_id", inv.InviteID,
		"org_id", inv.OrgID,
		"email", body.Email,
		"role", inv.Role)

	resp := map[string]any{
		"ok":     true,
		"org_id": inv.OrgID,
		"role":   inv.Role,
	}
	if rawKey != "" {
		resp["api_key"] = rawKey
		resp["key_prefix"] = keyPrefix
	}
	writeJSON(w, http.StatusOK, resp)
}
