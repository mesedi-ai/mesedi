// CORS middleware for the Mesedi backend.
//
// Allows the browser-based Next.js dashboard to call the Fly-hosted API
// directly. Production origins are mesedi.ai (marketing) and
// app.mesedi.ai (dashboard). Preview deployments are served from
// Cloudflare Workers under *.workers.dev; local dev uses
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
// to call this API from a browser. Add new origins by extending this list.
var allowedOrigins = map[string]struct{}{
	"https://mesedi.ai":     {},
	"https://app.mesedi.ai": {},
	"http://localhost:3000": {},
	"http://localhost:3001": {},
	"http://127.0.0.1:3000": {},
}

// isAllowedOrigin reports whether origin should be allowed by CORS.
// Matches exact entries in allowedOrigins, plus any Cloudflare Workers
// preview deployment URL for the mesedi-web project. CF preview URLs
// look like https://mesedi-web.<account-subdomain>.workers.dev or
// https://<hash>-mesedi-web.<account-subdomain>.workers.dev.
func isAllowedOrigin(origin string) bool {
	if _, ok := allowedOrigins[origin]; ok {
		return true
	}
	// Cloudflare Workers preview origins.
	if strings.HasPrefix(origin, "https://") &&
		strings.HasSuffix(origin, ".workers.dev") &&
		strings.Contains(origin, "mesedi-web") {
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
				// Include PUT because three dashboard endpoints use it:
				// PUT /me/retention (#262), PUT /me/class-severities/{class}
				// (#261), and PUT /me/budget-ceiling (#252). Without PUT in
				// this list, the browser preflight rejects those calls with
				// a generic "Failed to fetch" -- the bug user spotted 2026-
				// 05-31 that masked the underlying 403 role-required.
				w.Header().Set("Access-Control-Allow-Methods",
					"GET, POST, PUT, PATCH, DELETE, OPTIONS")
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
