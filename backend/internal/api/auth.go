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

	"mesedi/backend/internal/store"
)

// ctxKey is a private type used to attach values to a request context.
// Using a private type prevents collision with other packages' context keys.
type ctxKey int

const (
	ctxKeyProjectID ctxKey = iota + 1
	ctxKeyAPIKeyID
)

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

// authMiddleware constructs the bearer-token verification middleware.
// Returns a function that wraps an http.Handler; failed auth returns
// 401 without calling the wrapped handler.
func authMiddleware(s store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := extractBearer(r.Header.Get("Authorization"))
			if !ok {
				writeError(w, http.StatusUnauthorized, "missing or malformed Authorization header (expected: Bearer mesedi_sk_...)")
				return
			}
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

			// Attach project + key IDs to the request context for handlers.
			ctx := context.WithValue(r.Context(), ctxKeyProjectID, key.ProjectID)
			ctx = context.WithValue(ctx, ctxKeyAPIKeyID, key.KeyID)

			// Stamp project_id onto the wrapped ResponseWriter so the
			// request-log middleware (which runs in the outer chain
			// without context access) can include it in the log line.
			SetProjectIDForLogging(w, key.ProjectID)

			// Touch last_used_at asynchronously, fire-and-forget so a slow
			// DB write doesn't add latency to the request hot path.
			go func(keyID string) {
				_ = s.TouchAPIKey(context.Background(), keyID)
			}(key.KeyID)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
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
