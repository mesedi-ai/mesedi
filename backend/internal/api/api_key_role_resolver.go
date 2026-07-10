package api

// API key role resolution + last-admin-key guard.
//
// Background: the RBAC model in rbac.go computes a caller's role by
// looking up their user_id in the organization_members table. API keys
// carry a user_id (see store.APIKey.UserID) and inherit the current
// org role of that user. This file exposes that mapping at the handler
// layer so:
//
//   1. GET /api-keys can return the effective role per key for the
//      dashboard to render as an ADMIN / WRITE / READ badge.
//   2. DELETE /api-keys/{id} can refuse revoking the last key whose
//      owner has admin role — preventing a project from losing all
//      admin authentication (the customer would still have dashboard
//      access via session cookies, but their SDK calls that rely on
//      admin-only endpoints would break silently).
//
// Legacy compatibility: keys minted before migration 014 have no
// user_id. rbac.go resolves those as admin so existing customer
// integrations don't break. This resolver mirrors that behavior:
// missing user_id → "admin".

import (
	"context"

	"mesedi/backend/internal/store"
)

// apiKeyRole is the role a specific key carries at request-time,
// resolved via that key's owner's org membership.
const (
	apiKeyRoleAdmin   = "admin"
	apiKeyRoleWrite   = "write"
	apiKeyRoleRead    = "read"
	apiKeyRoleUnknown = ""
)

// resolveKeyRoles returns a map from key_id to the current effective
// role of each key. Errors from the org / member lookups are treated
// as unknown-role rather than fatal — the caller may prefer to render
// keys with no badge instead of failing the whole listing over one
// stale membership row. The project's tenant_id is looked up ONCE
// even if there are many keys (batch-friendly).
func (h *Handlers) resolveKeyRoles(
	ctx context.Context, projectID string, keys []*store.APIKey,
) map[string]string {
	out := make(map[string]string, len(keys))
	if len(keys) == 0 {
		return out
	}

	tenantPtr, err := h.Store.GetProjectTenantID(ctx, projectID)
	if err != nil || tenantPtr == nil || *tenantPtr == "" {
		// Legacy project with no tenant_id — same fall-through as
		// resolveCallerRole. Every key gets treated as admin so the
		// customer isn't unexpectedly downgraded in the UI.
		for _, k := range keys {
			out[k.KeyID] = apiKeyRoleAdmin
		}
		return out
	}
	tenantID := *tenantPtr

	// Cache per-user role lookups so N keys owned by the same user
	// don't cost N DB round trips (common case: solo Hobby customer
	// where every key is owned by them).
	roleByUser := map[string]string{}
	for _, k := range keys {
		if k.UserID == "" {
			// Pre-migration-014 keys with no user_id inherit admin
			// per the rbac.go legacy path.
			out[k.KeyID] = apiKeyRoleAdmin
			continue
		}
		if r, ok := roleByUser[k.UserID]; ok {
			out[k.KeyID] = r
			continue
		}
		member, err := h.Store.GetOrganizationMember(ctx, tenantID, k.UserID)
		if err != nil || member == nil {
			// Owner was removed from the org or the row is otherwise
			// unresolvable. The key still authenticates (the auth
			// middleware doesn't gate on membership); we just can't
			// display a role badge for it. Downstream: the guard
			// treats "unknown" as NOT-admin so revoking such a key
			// doesn't accidentally satisfy the last-admin check.
			roleByUser[k.UserID] = apiKeyRoleUnknown
			out[k.KeyID] = apiKeyRoleUnknown
			continue
		}
		roleByUser[k.UserID] = member.Role
		out[k.KeyID] = member.Role
	}
	return out
}

// wouldStrandProjectWithoutAdminKey reports whether revoking the
// target key would leave the project with zero admin-role keys. Used
// by HandleRevokeAPIKey to refuse the revoke with 409 in that case.
//
// The check compares against the ROLE-resolved key set from
// resolveKeyRoles, so a project with 5 total keys (4 read, 1 admin)
// correctly refuses to revoke the single admin key. The existing
// "last key overall" guard in HandleRevokeAPIKey handles the
// simpler total-count-1 case; this function focuses on the
// role-aware slice.
//
// Returns false (safe to revoke) when the target key itself is NOT
// admin — non-admin keys can be revoked freely because they don't
// affect admin-role coverage.
func (h *Handlers) wouldStrandProjectWithoutAdminKey(
	ctx context.Context, projectID, targetKeyID string, keys []*store.APIKey,
) bool {
	roles := h.resolveKeyRoles(ctx, projectID, keys)
	targetRole := roles[targetKeyID]
	if targetRole != apiKeyRoleAdmin {
		return false
	}
	adminCount := 0
	for _, k := range keys {
		if k.KeyID == targetKeyID {
			continue
		}
		if roles[k.KeyID] == apiKeyRoleAdmin {
			adminCount++
		}
	}
	return adminCount == 0
}
