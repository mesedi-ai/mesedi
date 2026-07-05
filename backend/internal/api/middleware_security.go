// SecurityHeaders middleware: sets the four production hardening
// response headers (HSTS, X-Content-Type-Options, X-Frame-Options,
// Referrer-Policy) on every response, plus a generic "Server: mesedi"
// stamp so the underlying platform (Fly) doesn't leak its own
// fingerprint when we control it.
//
// Standard security middleware pattern. Wire it as the
// outermost (or one of the outermost) middlewares so the headers are
// set even on auth-failed 401 responses.

package api

import "net/http"

func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Server", "mesedi")
		next.ServeHTTP(w, r)
	})
}
