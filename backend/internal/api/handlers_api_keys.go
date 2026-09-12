// API key endpoints on the customer path.
// Split out of handlers.go on 2026-09-12; every declaration moved verbatim.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"mesedi/backend/internal/store"
)

// HandleListAPIKeys returns the calling project's API keys (without
// the hash, never serialized). Each key is annotated with the
// effective org role of its owner so the dashboard can render an
// ADMIN / WRITE / READ badge and disable revoke on the last
// admin-role key. See api_key_role_resolver.go for the resolver
// semantics + legacy-key fallback.
func (h *Handlers) HandleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	keys, err := h.Store.ListAPIKeysForProject(r.Context(), authProjectID)
	if err != nil {
		h.Logger.Error("list api_keys failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "list failed: "+err.Error())
		return
	}
	// Resolve per-key roles for badge rendering. Wrap each key in a
	// small shape so we can add the `role` field without touching
	// store.APIKey (which stays a pure data-model type).
	roles := h.resolveKeyRoles(r.Context(), authProjectID, keys)
	type apiKeyListItem struct {
		*store.APIKey
		Role string `json:"role"`
	}
	items := make([]apiKeyListItem, 0, len(keys))
	for _, k := range keys {
		items = append(items, apiKeyListItem{APIKey: k, Role: roles[k.KeyID]})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"api_keys": items,
		"count":    len(items),
	})
}

// HandleCreateAPIKey mints a new API key for the calling project and
// returns the RAW KEY VALUE ONCE, this is the only moment a caller
// ever sees it. The server only persists the hash. Caller must store
// the raw key immediately; a lost raw key requires a new mint.
//
// Request body (optional): {"name": "human-readable label",
// "role": "admin|write|read"}. When role is omitted the minted key
// inherits the caller's user role (legacy behavior); when present,
// the minted key resolves to that role verbatim regardless of the
// caller's identity, the mechanism that lets an admin create
// scoped credentials for CI, monitoring scripts, or partners.
func (h *Handlers) HandleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "admin") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	var body struct {
		Name string `json:"name,omitempty"`
		Role string `json:"role,omitempty"`
	}
	// Empty body is fine, both fields are optional. Skip the strict-
	// decode path here because the shape is intentionally permissive.
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	// Normalize + validate role. Case-insensitive so a customer
	// pasting "ADMIN" from a form doesn't get a confusing 400.
	role := strings.ToLower(strings.TrimSpace(body.Role))
	switch role {
	case "", "admin", "write", "read":
		// ok
	default:
		writeError(w, http.StatusBadRequest,
			"role must be one of: admin, write, read (or omit to inherit caller's role)")
		return
	}

	rawKey, hash, prefix, err := MintAPIKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mint key: "+err.Error())
		return
	}

	keyID := "key-" + prefix[len("mesedi_sk_"):] + "-" + fmt.Sprintf("%d", time.Now().UnixNano())
	// Tag the new key with the caller's user_id so it authenticates
	// as the same org member that created it (RBAC). If the
	// caller is on a legacy key with no user_id, the new key inherits
	// the project's owner identity so the chain doesn't break.
	callerUserID, _ := UserIDFromContext(r.Context())
	if callerUserID == "" {
		if p, perr := h.Store.GetProject(r.Context(), authProjectID); perr == nil && p != nil {
			if p.OwnerUserID != "" {
				callerUserID = p.OwnerUserID
			} else {
				callerUserID = p.OwnerEmail
			}
		}
	}
	rec := &store.APIKey{
		KeyID:     keyID,
		ProjectID: authProjectID,
		KeyHash:   hash,
		KeyPrefix: prefix,
		Name:      body.Name,
		UserID:    callerUserID,
		Role:      role,
	}
	if err := h.Store.CreateAPIKey(r.Context(), rec); err != nil {
		writeError(w, http.StatusInternalServerError, "persist key: "+err.Error())
		return
	}

	h.Logger.Info("api key minted",
		"key_id", keyID,
		"prefix", prefix,
		"project_id", authProjectID,
		"name", body.Name,
		"role", role,
	)

	// audit log: API key creation is a top-tier admin action.
	// Recorded after the row lands so a failed mint never produces
	// a false-positive audit entry.
	h.recordAuditEvent(r, AuditAPIKeyCreate, "api_key", keyID, map[string]any{
		"name":   body.Name,
		"prefix": prefix,
		"role":   role,
	})

	// Return the raw key in this ONE response. The hash never leaves.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"key_id":  keyID,
		"raw_key": rawKey,
		"prefix":  prefix,
		"name":    body.Name,
		"role":    role,
		"warning": "Store this raw_key now, it will never be shown again.",
	})
}

// HandleRevokeAPIKey hard-deletes an API key. Project-scoped via the
// Store method's project_id guard. Admin-only (RBAC): a Read or
// Write member could otherwise revoke the admin's own key.
func (h *Handlers) HandleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "admin") {
		return
	}
	keyID := r.PathValue("id")
	if keyID == "" {
		writeError(w, http.StatusBadRequest, "key_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	// Last-key protection (Robert flagged): revoking the only
	// remaining key would leave the project unable to authenticate
	// against anything but admin endpoints. Only the close-account
	// flow is allowed to bring key count to zero, so refuse here.
	keys, lkErr := h.Store.ListAPIKeysForProject(r.Context(), authProjectID)
	if lkErr != nil {
		writeError(w, http.StatusInternalServerError,
			"could not verify remaining keys: "+lkErr.Error())
		return
	}
	if len(keys) <= 1 {
		writeError(w, http.StatusConflict,
			"cannot revoke the project's last API key; mint a new key first, or close the account from settings to remove the project entirely")
		return
	}
	// Admin-role guard: if this key belongs to an admin AND revoking
	// it would leave the project with zero admin-role keys, refuse.
	// Prevents a project on Team+ (with mixed-role members) from
	// silently losing all admin-authenticated SDK access when the
	// last admin key is revoked. Hobby-tier projects hit this via
	// the simpler "last key overall" branch above because a Hobby
	// project has exactly one user (admin) who owns every key.
	if h.wouldStrandProjectWithoutAdminKey(r.Context(), authProjectID, keyID, keys) {
		writeError(w, http.StatusConflict,
			"cannot revoke the project's last admin-role API key; mint another admin-role key first")
		return
	}
	// Batch 2: find the target key's UserID so we can kill that
	// user's dashboard sessions after the API key is gone. Robert's
	// rule: revoking a member's key MUST also log them out of every
	// browser they have open. We pluck the UserID from the existing
	// keys slice rather than make a new Store call.
	var revokedKeyUserID string
	for _, k := range keys {
		if k.KeyID == keyID {
			revokedKeyUserID = k.UserID
			break
		}
	}
	if err := h.Store.DeleteAPIKey(r.Context(), keyID, authProjectID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "api key not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Batch 2: kick the affected user out of every dashboard
	// browser tab they have open. Best-effort: a delete-sessions
	// failure does NOT undo the key revocation; the audit row
	// records sessions_revoked=0 so an operator can re-run a
	// cleanup if needed.
	sessionsRevoked := 0
	if revokedKeyUserID != "" {
		if n, sErr := h.Store.DeleteSessionsByUserID(r.Context(), revokedKeyUserID); sErr == nil {
			sessionsRevoked = n
		} else {
			h.Logger.Warn("api key revoke: kill sessions failed (key still revoked)",
				"key_id", keyID, "user_id", revokedKeyUserID, "error", sErr.Error())
		}
	}
	h.Logger.Info("api key revoked",
		"key_id", keyID,
		"project_id", authProjectID,
		"sessions_revoked", sessionsRevoked,
	)
	// audit log: key revocation is admin-tier. Batch 2
	// adds sessions_revoked so the row records the full effect.
	h.recordAuditEvent(r, AuditAPIKeyRevoke, "api_key", keyID, map[string]any{
		"sessions_revoked": sessionsRevoked,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"key_id": keyID,
	})
}
