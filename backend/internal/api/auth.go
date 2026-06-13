// Bearer-token authentication middleware.
//
// The Mesedi SDK authenticates by sending `Authorization: Bearer mesedi_sk_...`
// on every request. The middleware:
//
//  1. Extracts the bearer token from the Authorization header.
//  2. Hashes it with SHA-256.
//  3. Looks up the hash in the api_keys table via the Store.
//  4. Attaches the resulting project_id and api_key_id to the request
//     context so downstream handlers can use it.
//  5. Asynchronously updates last_used_at on the matched key.
//
// Phase 1.5: auth is REQUIRED for /executions and /events. /health is
// public (used by load-balancer probes, should never require auth).
// Phase 2+: per-project rate limiting layers on top of this.
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"mesedi/backend/internal/store"
)

// ctxKey is a private type used to attach values to a request context.
// Using a private type prevents collision with other packages' context keys.
type ctxKey int

const (
	ctxKeyProjectID ctxKey = iota + 1
	ctxKeyAPIKeyID
	ctxKeyUserID
	// ctxKeyAdminAuthMethod records how the request reached an /admin/*
	// handler: either "legacy_token" (MESEDI_ADMIN_TOKEN env, deprecated)
	// or "api_key" (a row in api_keys with scope='admin'). Surfaced by
	// /admin/whoami so the operator can see which credential they're
	// holding.
	ctxKeyAdminAuthMethod
	// ctxKeyAdminKeyID is the api_keys.key_id of the admin-scope key
	// that authenticated this request. Empty when the request used the
	// legacy MESEDI_ADMIN_TOKEN path.
	ctxKeyAdminKeyID
	// ctxKeyAdminKeyName mirrors the human-chosen name on the admin
	// key (e.g. "founder-laptop-2026-06"). Empty for the legacy-token
	// path.
	ctxKeyAdminKeyName
)

// Admin auth method constants surfaced via /admin/whoami.
const (
	AdminAuthMethodLegacyToken = "legacy_token"
	AdminAuthMethodAPIKey      = "api_key"
)

// AdminAuthMethodFromContext returns the auth method that gated this
// request through AdminAuth. Empty + false when the handler is not
// behind AdminAuth (e.g. unit test calling the handler directly).
func AdminAuthMethodFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyAdminAuthMethod).(string)
	return v, ok
}

// AdminKeyIDFromContext returns the api_keys.key_id of the admin-scope
// key behind this request, or empty + false if the request used the
// legacy MESEDI_ADMIN_TOKEN path.
func AdminKeyIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyAdminKeyID).(string)
	return v, ok
}

// AdminKeyNameFromContext mirrors AdminKeyIDFromContext for the
// human-chosen name on the admin key.
func AdminKeyNameFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyAdminKeyName).(string)
	return v, ok
}

// ProjectIDFromContext returns the authenticated project ID associated
// with the request, or empty + false if no project ID was attached
// (which means the middleware did not authorize this request, caller
// should never have reached this code path under normal middleware
// ordering, but the false return makes the safety check explicit).
func ProjectIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyProjectID).(string)
	return v, ok
}

// APIKeyIDFromContext returns the authenticated API key ID. Useful for
// audit logging, every action a key takes can be traced back.
func APIKeyIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyAPIKeyID).(string)
	return v, ok
}

// UserIDFromContext returns the org-member user_id this request is
// authenticated as (#263 RBAC). Empty string + false when the key
// pre-dates migration 014 (legacy keys without a user_id). Callers
// that need to enforce roles should fall back to project.OwnerUserID
// or OwnerEmail when this is empty.
func UserIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyUserID).(string)
	return v, ok
}

// HashAPIKey returns the SHA-256 hex digest of the raw key. The same
// hash is used both at mint time (stored in api_keys.key_hash) and at
// verification time (computed from the bearer token, looked up against
// the stored hash). SHA-256 is sufficient here, the secret never leaves
// the customer's machine, and rainbow-table risk is mitigated by the
// keys being long random strings, not passwords.
func HashAPIKey(rawKey string) string {
	sum := sha256.Sum256([]byte(rawKey))
	return hex.EncodeToString(sum[:])
}

// MintAPIKey generates a new random key suitable for handing to a customer
// at project-creation time. Returns the raw key (show once, never store),
// the SHA-256 hash (store in api_keys.key_hash), and the public prefix
// (store in api_keys.key_prefix for display).
//
// Format: `mesedi_sk_<32-char base64url-encoded random>`. The "sk" suffix
// mirrors Stripe's "sk" (secret key) convention so developers instinctively
// treat it as sensitive.
func MintAPIKey() (rawKey, hash, prefix string, err error) {
	buf := make([]byte, 24) // 24 bytes → 32 chars of base64url
	if _, err := rand.Read(buf); err != nil {
		return "", "", "", err
	}
	rand64 := base64.RawURLEncoding.EncodeToString(buf)
	rawKey = "mesedi_sk_" + rand64
	hash = HashAPIKey(rawKey)
	// Prefix shows "mesedi_sk_" + first 4 chars of the random portion.
	// Long enough for a developer to identify the key visually, short
	// enough that knowing the prefix doesn't help an attacker brute-force
	// the remaining 28 characters.
	prefix = rawKey[:14]
	return rawKey, hash, prefix, nil
}

// SessionCookieName is the HttpOnly cookie that carries the raw
// session token for cookie-based dashboard auth (#213). The value
// in the cookie is the raw token; the DB stores only its SHA-256
// hash so a leaked sessions table does not yield usable cookies.
const SessionCookieName = "mesedi_session"

// SessionTTL is the sliding-window TTL applied to a session on
// every authenticated request. An active customer stays signed in
// indefinitely; an idle browser tab expires after this window.
const SessionTTL = 7 * 24 * time.Hour

// HashSessionToken returns the SHA-256 hex digest of the raw
// session cookie value. Exported so signin.go (Batch 2) can hash
// the freshly-minted raw token before inserting into the sessions
// table.
func HashSessionToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// authMiddleware constructs the request-auth middleware. It accepts
// two credential paths:
//
//   1. Bearer API key (SDK / curl) — Authorization: Bearer mesedi_sk_*
//   2. Session cookie (dashboard) — Cookie: mesedi_session=<raw token>
//
// API key path is tried first because the SDK is the higher-volume
// caller; the cookie path runs only when no Authorization header is
// present. Either path produces the same context shape (projectID +
// userID + optional keyID) so downstream handlers do not branch on
// which credential was used.
//
// detector (may be nil) is the process-wide AbuseDetector. After a
// successful API key lookup, the middleware feeds it (a) the
// project + IP pair so the key-leak detector can spot keys seen
// from too many IPs, and (b) the project-suspension check so
// suspended projects get 403 instead of being allowed through.
// Cookie auth runs the suspension check too but skips the key-leak
// detector (which only makes sense for API keys).
func authMiddleware(s store.Store, detector *AbuseDetector) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Path 1: Bearer API key (existing SDK contract).
			if token, ok := extractBearer(r.Header.Get("Authorization")); ok {
				authViaBearer(w, r, s, detector, next, token)
				return
			}

			// Path 2: session cookie (dashboard, #213).
			if cookie, err := r.Cookie(SessionCookieName); err == nil && cookie.Value != "" {
				authViaSessionCookie(w, r, s, next, cookie.Value)
				return
			}

			writeError(w, http.StatusUnauthorized,
				"missing credentials (expected Authorization: Bearer mesedi_sk_... or a mesedi_session cookie)")
		})
	}
}

// authViaBearer is the Bearer-API-key half of authMiddleware. Kept
// in a separate function so the cookie path can be added without
// turning the middleware closure into a 100-line block.
func authViaBearer(
	w http.ResponseWriter, r *http.Request,
	s store.Store, detector *AbuseDetector,
	next http.Handler, token string,
) {
	if !strings.HasPrefix(token, "mesedi_sk_") {
		writeError(w, http.StatusUnauthorized, "invalid API key format (must start with mesedi_sk_)")
		return
	}

	hash := HashAPIKey(token)
	key, err := s.GetAPIKeyByHash(r.Context(), hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "API key not recognized")
			return
		}
		writeError(w, http.StatusInternalServerError, "auth lookup failed: "+err.Error())
		return
	}

	// Suspension check (#172).
	suspended, reason, sErr := s.IsProjectSuspended(r.Context(), key.ProjectID)
	if sErr != nil {
		writeError(w, http.StatusInternalServerError, "suspension lookup failed: "+sErr.Error())
		return
	}
	if suspended {
		msg := "project suspended"
		if reason != "" {
			msg = msg + " (" + reason + "). Contact hello@mesedi.ai to appeal."
		}
		writeError(w, http.StatusForbidden, msg)
		return
	}

	ctx := context.WithValue(r.Context(), ctxKeyProjectID, key.ProjectID)
	ctx = context.WithValue(ctx, ctxKeyAPIKeyID, key.KeyID)
	if key.UserID != "" {
		ctx = context.WithValue(ctx, ctxKeyUserID, key.UserID)
	}

	SetProjectIDForLogging(w, key.ProjectID)

	if detector != nil {
		detector.RecordRequestForKeyLeak(r.Context(), key.ProjectID, key.KeyPrefix, extractClientIP(r))
	}

	go func(keyID string) {
		_ = s.TouchAPIKey(context.Background(), keyID)
	}(key.KeyID)

	next.ServeHTTP(w, r.WithContext(ctx))
}

// authViaSessionCookie is the cookie half of authMiddleware (#213).
// Hashes the raw cookie value, looks up the session row, validates
// expiry, runs the suspension check, sliding-window-extends the
// session, and forwards the request with project_id + user_id on
// the context.
func authViaSessionCookie(
	w http.ResponseWriter, r *http.Request,
	s store.Store, next http.Handler, rawToken string,
) {
	tokenHash := HashSessionToken(rawToken)
	sess, err := s.GetSessionByTokenHash(r.Context(), tokenHash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "session not recognized; sign in again")
			return
		}
		writeError(w, http.StatusInternalServerError, "session lookup failed: "+err.Error())
		return
	}

	now := time.Now().UTC()
	if sess.ExpiresAt.Before(now) {
		// Best-effort delete the stale row so the next request from
		// this browser does not pay for the same lookup again.
		go func(h string) { _ = s.DeleteSession(context.Background(), h) }(tokenHash)
		writeError(w, http.StatusUnauthorized, "session expired; sign in again")
		return
	}

	suspended, reason, sErr := s.IsProjectSuspended(r.Context(), sess.ProjectID)
	if sErr != nil {
		writeError(w, http.StatusInternalServerError, "suspension lookup failed: "+sErr.Error())
		return
	}
	if suspended {
		msg := "project suspended"
		if reason != "" {
			msg = msg + " (" + reason + "). Contact hello@mesedi.ai to appeal."
		}
		writeError(w, http.StatusForbidden, msg)
		return
	}

	// Sliding-window refresh. Fire-and-forget so a slow DB write
	// does not add latency to the request hot path.
	go func(h string, t time.Time) {
		_ = s.TouchSession(context.Background(), h, t, t.Add(SessionTTL))
	}(tokenHash, now)

	ctx := context.WithValue(r.Context(), ctxKeyProjectID, sess.ProjectID)
	if sess.UserID != "" {
		ctx = context.WithValue(ctx, ctxKeyUserID, sess.UserID)
	}

	SetProjectIDForLogging(w, sess.ProjectID)

	next.ServeHTTP(w, r.WithContext(ctx))
}

// extractBearer parses an Authorization header value, returning the bearer
// token (without the "Bearer " prefix) and whether parsing succeeded.
// Accepts both "Bearer xxx" and "bearer xxx" (case-insensitive scheme).
func extractBearer(header string) (string, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", false
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 {
		return "", false
	}
	if !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", false
	}
	return token, true
}
