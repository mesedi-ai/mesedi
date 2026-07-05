package api

// HTTP middleware that persists one row per authenticated request to
// the request_log table. Backs the Terms commitment to "share
// the information we have about the key's recent use" on a compromise
// report by capturing arbitrary URL traffic, not just the run-creation
// calls covered by executions.api_key_id.
//
// Scope:
//
//   - Logs ONLY when the request is authenticated. ProjectID and
//     APIKeyID must both be present in the request context (set by
//     AuthAPIKey). Unauthenticated paths (signup, login, marketing
//     redirects) are skipped.
//
//   - Logs ONLY for Team-tier projects. Hobby and Enterprise traffic
//     is skipped. The volume from free-tier Hobby users would dominate
//     the table and balloon Neon storage cost; Enterprise gets a
//     bespoke retention/forensic arrangement contractually and does
//     not need this table. The tier lookup hits GetProject once per
//     authenticated request; for production-typical request volumes
//     the marginal cost is in the low single-digit milliseconds.
//
// Write path:
//
//   The middleware wraps the next handler with a captureResponseWriter
//   so we can read the response's status code after the handler runs.
//   Insert is synchronous; for Team-tier customers it adds a few
//   milliseconds per request. A non-blocking write context with a
//   short timeout protects against rare Neon stalls; a transient
//   insert failure is logged but does not affect the response.
//
// Retention:
//
//   The companion request_log_retention_scheduler purges rows older
//   than 90 days on a daily tick. See request_log_retention_scheduler.go.

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"mesedi/backend/internal/store"
)

// RequestLogger writes a row to the request_log table for each
// authenticated Team-tier request that passes through. Hobby and
// Enterprise tiers are silently skipped.
type RequestLogger struct {
	Store  store.Store
	Logger *slog.Logger

	// WriteTimeout caps the insert. Default 250ms when zero. The
	// insert is on the hot path so this guards against rare Neon
	// stalls turning into request-tail latency spikes.
	WriteTimeout time.Duration
}

// Middleware returns a handler that wraps next. Apply after AuthAPIKey
// so the request context already has project + key IDs.
func (rl *RequestLogger) Middleware(next http.Handler) http.Handler {
	if rl.WriteTimeout == 0 {
		rl.WriteTimeout = 250 * time.Millisecond
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Wrap the writer so we can read the status code after the
		// handler runs. http.ResponseWriter has no public accessor
		// for the code; this is the standard idiom.
		crw := &captureResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(crw, r)

		// Only log when the request was authenticated. Unauth paths
		// (signup, login, etc.) drop out here.
		projectID, hasProject := ProjectIDFromContext(r.Context())
		if !hasProject || projectID == "" {
			return
		}
		keyID, hasKey := APIKeyIDFromContext(r.Context())
		if !hasKey || keyID == "" {
			return
		}

		// Team-tier gate. The GetProject lookup also gives us the
		// most-current tier, which matters because a customer might
		// have downgraded since their last request; we honor the
		// current tier on every request.
		writeCtx, cancel := context.WithTimeout(context.Background(), rl.WriteTimeout)
		defer cancel()

		project, err := rl.Store.GetProject(writeCtx, projectID)
		if err != nil {
			// Project lookup failed (rare; usually a transient Neon
			// stall). Drop the log silently; we'd rather lose a row
			// than fail the request.
			rl.Logger.Debug("request_log: project lookup failed; skipping",
				"project_id", projectID, "error", err.Error())
			return
		}
		if normalizeTier(project.Tier) != TierTeam {
			return
		}

		row := &store.RequestLog{
			ProjectID:  projectID,
			APIKeyID:   keyID,
			IPAddress:  clientIP(r),
			Method:     r.Method,
			Path:       r.URL.Path,
			StatusCode: crw.status,
			ReceivedAt: time.Now().UTC(),
		}
		if err := rl.Store.CreateRequestLog(writeCtx, row); err != nil {
			rl.Logger.Warn("request_log: insert failed",
				"project_id", projectID,
				"api_key_id", keyID,
				"method", r.Method,
				"path", r.URL.Path,
				"error", err.Error())
			return
		}
	})
}

// captureResponseWriter wraps http.ResponseWriter to record the
// status code that downstream handlers wrote. WriteHeader is the
// only mutation point; Write implicitly writes 200 if WriteHeader
// was never called, and the default status field handles that case.
type captureResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *captureResponseWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// clientIP returns the client IP as best as we can determine it
// behind Fly's proxy. Fly-Client-IP is set by the proxy on every
// inbound request; fall back to X-Forwarded-For's first entry, then
// to RemoteAddr's host portion.
func clientIP(r *http.Request) string {
	if v := r.Header.Get("Fly-Client-IP"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		// First entry is the original client; subsequent entries are
		// intermediate proxies.
		if i := strings.IndexByte(v, ','); i >= 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
