// Admin API key operations.
// Split out of admin.go on 2026-09-12; every declaration moved verbatim.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mesedi/backend/internal/store"
)

// adminCreateKeyRequest is the POST body for /admin/api-keys.
// Common admin key-issuance shape.
// so the dashboard JS can be written once with one mental model.
type adminCreateKeyRequest struct {
	// Name is the human-chosen label. Required, max 200 chars.
	// For customer-scope keys the handler does NOT add the
	// "customer:" scope prefix; Mesedi separates scope into a
	// dedicated column so a marker prefix in the name is redundant.
	Name string `json:"name"`
	// Scope: "customer" (default, project-scoped) or "admin"
	// (privileged). Empty / missing defaults to "customer" so an old
	// client cannot silently escalate.
	Scope string `json:"scope,omitempty"`
	// ConfirmAdminScope MUST be true when Scope=="admin". A typo in
	// the scope field is not enough to mint a privileged credential;
	// the dashboard explicitly opts in via a confirmation checkbox.
	ConfirmAdminScope bool `json:"confirm_admin_scope,omitempty"`
	// ProjectID is required when Scope=="customer" (the project the
	// new key authenticates as). Ignored when Scope=="admin"; the
	// handler always assigns admin keys to store.APIKeyAdminProjectID.
	ProjectID string `json:"project_id,omitempty"`
	// ExpiresAt is optional. Accepts either RFC3339Nano
	// ("2026-12-31T23:59:59Z") or YYYY-MM-DD (parsed as end-of-day
	// UTC). Empty == never expires. Past timestamps are rejected.
	ExpiresAt string `json:"expires_at,omitempty"`
}

// HandleAdminCreateAPIKey mints a new API key. See adminCreateKeyRequest.
// Returns the raw secret ONCE; the operator stores it immediately or
// has to revoke + remint.
func (h *Handlers) HandleAdminCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var req adminCreateKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name required")
		return
	}
	if len(name) > 200 {
		writeError(w, http.StatusBadRequest, "name too long (max 200 chars)")
		return
	}
	scope := strings.ToLower(strings.TrimSpace(req.Scope))
	if scope == "" {
		scope = store.APIKeyScopeCustomer
	}
	if scope != store.APIKeyScopeCustomer && scope != store.APIKeyScopeAdmin {
		writeError(w, http.StatusBadRequest, `scope must be "customer" or "admin"`)
		return
	}
	if scope == store.APIKeyScopeAdmin && !req.ConfirmAdminScope {
		writeError(w, http.StatusBadRequest,
			"admin scope requires confirm_admin_scope:true")
		return
	}

	// Resolve project_id by scope.
	var projectID string
	if scope == store.APIKeyScopeAdmin {
		projectID = store.APIKeyAdminProjectID
	} else {
		projectID = strings.TrimSpace(req.ProjectID)
		if projectID == "" {
			writeError(w, http.StatusBadRequest, "project_id required for customer-scope keys")
			return
		}
		if projectID == store.APIKeyAdminProjectID {
			writeError(w, http.StatusBadRequest,
				`project_id "_admin" is reserved for admin-scope keys`)
			return
		}
		// Verify the project exists so we fail at 400 (operator typo)
		// rather than 500 (FK constraint violation deep in store.go).
		if _, err := h.Store.GetProject(r.Context(), projectID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, "project not found: "+projectID)
				return
			}
			writeError(w, http.StatusInternalServerError, "project lookup failed: "+err.Error())
			return
		}
	}

	expiresAt, err := parseAdminKeyExpiresAt(req.ExpiresAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "expires_at: "+err.Error())
		return
	}

	rawKey, hash, prefix, err := MintAPIKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mint key: "+err.Error())
		return
	}
	now := time.Now().UTC()
	keyID := "key-" + prefix[len("mesedi_sk_"):] + "-" + strconv.FormatInt(now.UnixNano(), 10)

	k := &store.APIKey{
		KeyID:     keyID,
		ProjectID: projectID,
		KeyHash:   hash,
		KeyPrefix: prefix,
		Name:      name,
		CreatedAt: now,
		Scope:     scope,
		ExpiresAt: expiresAt,
	}
	if err := h.Store.CreateAPIKey(r.Context(), k); err != nil {
		writeError(w, http.StatusInternalServerError, "create key: "+err.Error())
		return
	}

	resp := map[string]any{
		"ok":         true,
		"key_id":     keyID,
		"project_id": projectID,
		"name":       name,
		"scope":      scope,
		"key_prefix": prefix,
		"secret":     rawKey,
		"warning":    "Store this secret now. It will never be shown again.",
		"created_at": now.Format(time.RFC3339Nano),
	}
	if expiresAt != "" {
		resp["expires_at"] = expiresAt
	}
	// Attribution log line (the audit trail until we add a proper table).
	method, _ := AdminAuthMethodFromContext(r.Context())
	actorKeyID, _ := AdminKeyIDFromContext(r.Context())
	h.Logger.Info("admin: api key minted",
		"key_id", keyID,
		"scope", scope,
		"project_id", projectID,
		"actor_method", method,
		"actor_key_id", actorKeyID,
	)
	writeJSON(w, http.StatusCreated, resp)
}

// HandleAdminListAPIKeys returns every API key in the system, NEWEST
// first. key_hash is never serialized. Admin-only.
func (h *Handlers) HandleAdminListAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.Store.ListAllAPIKeys(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list api keys: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"api_keys": keys,
		"count":    len(keys),
	})
}

// HandleAdminRevokeAPIKey hard-deletes any API key by key_id. Admin-
// only. The dashboard layers an admin-key-paste confirmation on top
// of this for admin-scope deletions.
func (h *Handlers) HandleAdminRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	keyID := r.PathValue("id")
	if keyID == "" {
		writeError(w, http.StatusBadRequest, "missing key id")
		return
	}
	// Self-revoke guard: if this revoke target is the same key the
	// operator is currently authenticated with, refuse so the operator
	// can't accidentally lock themselves out. The dashboard already
	// warns about this; this is the server-side fail-safe.
	if actorKeyID, ok := AdminKeyIDFromContext(r.Context()); ok && actorKeyID == keyID {
		writeError(w, http.StatusBadRequest,
			"refusing to revoke the key authenticating this request; "+
				"mint a replacement admin key first, then revoke this one with the new key")
		return
	}
	// Resolve the owning project BEFORE the delete. Afterwards the row
	// is gone and the audit event has nothing to attach to, which is
	// precisely how a destructive action becomes unattributable.
	revokedProjectID := ""
	if k, kerr := h.Store.GetAPIKeyByID(r.Context(), keyID); kerr == nil && k != nil {
		revokedProjectID = k.ProjectID
	}
	if err := h.Store.DeleteAPIKeyByID(r.Context(), keyID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "key not found: "+keyID)
			return
		}
		writeError(w, http.StatusInternalServerError, "revoke key: "+err.Error())
		return
	}
	method, _ := AdminAuthMethodFromContext(r.Context())
	actorKeyID, _ := AdminKeyIDFromContext(r.Context())
	h.recordAuditEventForProject(
		context.Background(),
		revokedProjectID,
		AuditActorPlatformAdmin,
		AuditAdminAPIKeyRevoke,
		"api_key",
		keyID,
		map[string]any{"actor_method": method, "actor_key_id": actorKeyID},
	)
	h.Logger.Info("admin: api key revoked",
		"key_id", keyID,
		"actor_method", method,
		"actor_key_id", actorKeyID,
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"key_id": keyID,
	})
}

// parseAdminKeyExpiresAt accepts either RFC3339Nano / RFC3339 or
// YYYY-MM-DD and returns the canonical RFC3339Nano UTC string that
// gets stored in api_keys.expires_at. Empty input is allowed and
// returns "" (never expires).
//
// Date-only inputs are interpreted as end-of-day UTC (23:59:59.999...)
// so "expires today" works as the operator's intuition expects.
// Past timestamps are rejected; minting an already-dead credential is
// never what the operator meant.
func parseAdminKeyExpiresAt(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	// Try date-only first since that's the common case from a
	// <input type="date"> picker.
	if t, err := time.ParseInLocation("2006-01-02", s, time.UTC); err == nil {
		t = t.Add(24*time.Hour - time.Nanosecond)
		if !time.Now().UTC().Before(t) {
			return "", fmt.Errorf("date is in the past")
		}
		return t.Format(time.RFC3339Nano), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		if !time.Now().UTC().Before(t) {
			return "", fmt.Errorf("timestamp is in the past")
		}
		return t.UTC().Format(time.RFC3339Nano), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		if !time.Now().UTC().Before(t) {
			return "", fmt.Errorf("timestamp is in the past")
		}
		return t.UTC().Format(time.RFC3339Nano), nil
	}
	return "", fmt.Errorf("expected YYYY-MM-DD or RFC3339")
}
