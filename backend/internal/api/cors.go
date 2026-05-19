// CORS middleware for the Mesedi backend.
//
// Allows the browser-based Next.js dashboard hosted on Vercel to call
// the Fly-hosted API directly. Production origin is mesedi.vercel.app;
// preview deployments use mesedi-*-mesediai.vercel.app; local dev uses
// localhost:3000 (default Next.js dev server).
//
// Handles the CORS preflight OPTIONS request inline (returns 204 with
// the appropriate Access-Control-* headers) and forwards everything
// else to the next handler with Access-Control-Allow-Origin and
// Access-Control-Allow-Credentials set on the response.
//
// This middleware runs at the top of the chain so it sees and answers
// the preflight before authMiddleware would reject the OPTIONS request
// for missing Authorization.
package api

import (
	"net/http"
	"strings"
)

// allowedOrigins is the set of explicit, exact-match origins permitted
// to call this API from a browser. Add new origins (e.g. custom domains
// like mesedi.ai or app.mesedi.ai) by extending this list.
var allowedOrigins = map[string]struct{}{
	"https://mesedi.vercel.app": {},
	"http://localhost:3000":     {},
	"http://localhost:3001":     {},
	"http://127.0.0.1:3000":     {},
}

// isAllowedOrigin reports whether origin should be allowed by CORS.
// Matches exact entries in allowedOrigins, plus any Vercel preview
// deployment URL for the mesedi project (mesedi-*-mesediai.vercel.app).
func isAllowedOrigin(origin string) bool {
	if _, ok := allowedOrigins[origin]; ok {
		return true
	}
	// Vercel preview origins: https://mesedi-<hash>-mesediai.vercel.app
	if strings.HasPrefix(origin, "https://mesedi-") &&
		strings.HasSuffix(origin, "-mesediai.vercel.app") {
		return true
	}
	return false
}

// CORSMiddleware returns the middleware that sets CORS headers and
// short-circuits OPTIONS preflight requests with 204.
func CORSMiddleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			if origin != "" && isAllowedOrigin(origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Access-Control-Allow-Methods",
					"GET, POST, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers",
					"Authorization, Content-Type, X-Mesedi-Schema-Version")
				w.Header().Set("Access-Control-Max-Age", "86400")
			}

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
