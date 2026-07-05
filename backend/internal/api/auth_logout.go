package api

// POST /auth/logout — destroys the caller's session (Batch 2).
//
// The endpoint is intentionally not behind authMiddleware: a customer
// who has already lost their session row (expired, kicked from the
// org, key revoked) should still be able to click Sign Out without
// hitting a 401. The cookie clearing happens regardless of whether
// the lookup found a row.
//
// The contract is server-side delete + Set-Cookie-with-Max-Age=0
// so the browser drops the cookie too. Returns 204 No Content on
// success and on idempotent retry.

import (
	"context"
	"net/http"
	"time"
)

// HandleAuthLogout is the POST /auth/logout handler.
func (h *Handlers) HandleAuthLogout(w http.ResponseWriter, r *http.Request) {
	// Read the cookie; missing or empty is a no-op success.
	cookie, err := r.Cookie(SessionCookieName)
	if err == nil && cookie.Value != "" {
		tokenHash := HashSessionToken(cookie.Value)
		// Fire-and-forget: a slow DB write should not delay the
		// logout response. The expired Set-Cookie below takes
		// effect regardless of whether the DB delete succeeds.
		go func(hash string) {
			_ = h.Store.DeleteSession(context.Background(), hash)
		}(tokenHash)
	}

	// Clear the cookie on the browser regardless. Matching attrs
	// must equal the original Set-Cookie or the browser keeps the
	// old cookie alive alongside the cleared one.
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})

	w.WriteHeader(http.StatusNoContent)
}
