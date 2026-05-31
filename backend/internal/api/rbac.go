package api

// Role-based access control helpers for dashboard mutating endpoints
// (#263 RBAC enforcement, expanded scope).
//
// Background: v1 dashboard auth is API-key-based. Migration 014 tagged
// each api_key with a user_id so the auth middleware can stamp the
// caller's identity onto the request context. This file is the policy
// layer that turns "I know who you are" into "you may not do this."
//
// Roles (least to most privileged):
//   read  -- view dashboard data, no mutations
//   write -- everything read + create/edit webhooks, set retention,
//            set severity routing
//   admin -- everything write + invite/remove members, change roles,
//            manage api keys, manage billing, set budget ceilings
//
// SDK ingest paths (POST /executions, PATCH /executions/{id},
// /events) are deliberately NOT gated by these helpers because the
// SDK key is provisioned by the admin and runs in the customer's
// agent process; "Read" in this model is a dashboard-visibility
// concept, not an SDK-suppression concept.
//
// Backward compatibility: API keys minted before migration 014 have
// no user_id (= empty in context). The role resolver treats those as
// admin so existing customer integrations don't break overnight. The
// customer can rotate to per-member keys voluntarily.

import (
	"errors"
	"net/http"

	"mesedi/backend/internal/store"
)

// Role rank used for comparisons. Higher = more privileged.
var roleRank = map[string]int{
	"":      0, // unknown / not a member
	"read":  1,
	"write": 2,
	"admin": 3,
}

// resolveCallerRole returns the org-role of the API key making this
// request. Returns "admin" for legacy keys (pre-014, no user_id) and
// for projects with no tenant_id (legacy state) so existing flows
// keep working. Returns "" when the caller is authenticated but has
// no member row in the project's org -- which should never happen in
// practice, but the empty-string result makes the safety check
// explicit at the call site.
func (h *Handlers) resolveCallerRole(r *http.Request) (string, error) {
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		return "", errors.New("no project context")
	}

	tenantPtr, err := h.Store.GetProjectTenantID(r.Context(), projectID)
	if err != nil {
		return "", err
	}
	// Legacy project with no tenant_id. Self-heal will fix this on the
	// next /me/organization call; until then, treat as admin so the
	// customer isn't locked out of their own dashboard.
	if tenantPtr == nil || *tenantPtr == "" {
		return "admin", nil
	}

	caller, _ := UserIDFromContext(r.Context())
	if caller == "" {
		// Legacy key with no user_id. Falls through to admin so
		// existing SDK / dashboard integrations keep working.
		return "admin", nil
	}

	member, err := h.Store.GetOrganizationMember(r.Context(), *tenantPtr, caller)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return member.Role, nil
}

// requireRole writes a 403 + returns false if the caller's role is
// below minRole, otherwise returns true. Idiomatic usage:
//
//	if !h.requireRole(w, r, "write") {
//	    return
//	}
//
// The 403 body uses a stable English string so the dashboard can
// surface a meaningful message ("admin role required") instead of a
// generic "forbidden."
func (h *Handlers) requireRole(w http.ResponseWriter, r *http.Request, minRole string) bool {
	role, err := h.resolveCallerRole(r)
	if err != nil {
		h.Logger.Error("rbac: resolve caller role failed",
			"error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not resolve role")
		return false
	}
	if roleRank[role] < roleRank[minRole] {
		writeError(w, http.StatusForbidden, minRole+" role required")
		return false
	}
	return true
}
