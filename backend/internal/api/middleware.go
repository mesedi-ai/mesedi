// Generic HTTP middleware utilities and a composition helper.
//
// Each middleware here is independent and composable. The chain() helper
// is the typical idiomatic way to apply multiple middlewares in order:
//
//	mux := http.NewServeMux()
//	mux.HandleFunc("POST /events", h.HandleIngestEvents)
//	handler := chain(
//	    recoverMiddleware(logger),
//	    requestLogMiddleware(logger),
//	    authMiddleware(store),
//	)(mux)
//
// The middlewares execute in the order listed, recover first (so a panic
// in any subsequent middleware is caught), then logging (so even panic-
// recovery actions are logged), then auth.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"mesedi/backend/internal/store"
)

// AlertWebhookURL is set by main.go when MESEDI_ALERT_WEBHOOK_URL is
// configured. When non-empty, the request-log middleware POSTs a
// JSON payload to this URL on every 5xx response so the operator
// gets paged before customers notice. Auto-detects Slack and Discord
// webhook URL shapes and renders native-looking messages for each.
//
// Set as a package-level var (rather than threaded through every
// middleware constructor) because it's read-only after startup and
// keeps the middleware signatures unchanged.
var AlertWebhookURL atomic.Value // string

// SetAlertWebhookURL is the single setter, called once from main.go
// at startup. Empty string disables alerting.
func SetAlertWebhookURL(url string) {
	AlertWebhookURL.Store(url)
}

// NewTopChain returns the outer middleware chain that wraps EVERY request,
// public and private alike. Order matters: recover is outermost so it
// catches panics in any subsequent middleware; request-log runs second so
// even panic-handled responses are logged.
func NewTopChain(logger *slog.Logger) Middleware {
	return chain(
		recoverMiddleware(logger),
		requestLogMiddleware(logger),
		CORSMiddleware(),
	)
}

// NewAuthChain returns the inner middleware that runs ONLY on protected
// routes (POST /executions, POST /events, PATCH /executions/{id}). Today
// this is bearer-token auth, schema-version enforcement, and per-project
// rate limiting (in that order, auth must run first to attach project_id
// to context; the rate limiter consumes that). Request-ID generation
// will layer on top in a future slice without touching callers.
//
// detector is the process-wide AbuseDetector carried on Handlers. It
// feeds the rate-limit-sustained detector from inside the rate limit
// middleware and the key-leak detector from inside the auth middleware.
func NewAuthChain(logger *slog.Logger, s store.Store, detector *AbuseDetector) Middleware {
	return chain(
		authMiddleware(s, detector),
		schemaVersionMiddleware(),
		oversizedPayloadMiddleware(detector),
		rateLimitMiddleware(logger, detector),
	)
}

// CurrentSchemaVersion is the wire-format version this backend speaks.
// Bump when a breaking change to event shape, execution shape, or response
// envelope is introduced. SDKs SHOULD send X-Mesedi-Schema-Version on
// every request; backends MUST reject requests whose declared version is
// unsupported.
const CurrentSchemaVersion = "1"

// schemaVersionMiddleware enforces the X-Mesedi-Schema-Version header.
//
// Today's policy is "enforced if present": a missing header is accepted
// (assumed to be the current version, for backward compat with curl smoke
// tests and the bring-up README). A present-but-unsupported version is
// rejected with 400.
//
// Once the real SDK ships with the header by default, i.e. once we can
// assume any legitimate caller will set it, this policy tightens to
// "missing → 400" as well. Until then, soft-mode minimizes friction
// during local exploration.
func schemaVersionMiddleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			v := r.Header.Get("X-Mesedi-Schema-Version")
			if v != "" && v != CurrentSchemaVersion {
				writeError(w, http.StatusBadRequest,
					"unsupported X-Mesedi-Schema-Version "+v+" (this backend accepts: "+CurrentSchemaVersion+")")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Middleware is the standard http.Handler decorator signature.
type Middleware func(http.Handler) http.Handler

// chain composes multiple middlewares into a single one. The first
// middleware listed is the outermost (executes first on the way in,
// last on the way out).
func chain(mws ...Middleware) Middleware {
	return func(final http.Handler) http.Handler {
		// Apply in reverse so the listed order matches execution order.
		for i := len(mws) - 1; i >= 0; i-- {
			final = mws[i](final)
		}
		return final
	}
}

// recoverMiddleware catches any panic in downstream handlers, logs the
// stack trace, and returns a generic 500 to the client. Without this,
// a panic would crash the entire process because Go's default behavior
// is to abort on unrecovered panic in a goroutine, and each HTTP
// request runs in its own goroutine.
func recoverMiddleware(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic recovered",
						"panic", rec,
						"method", r.Method,
						"path", r.URL.Path,
						"stack", string(debug.Stack()),
					)
					// If headers haven't been sent yet, return 500. If they
					// have, the connection is already partially written and
					// the client will see a truncated response, nothing we
					// can do about that besides log loudly.
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"ok":false,"error":"internal server error"}`))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// requestLogMiddleware logs every request's method, path, status,
// duration, and (when authenticated) project_id. Severity scales with
// status code: info for 2xx/3xx, warn for 4xx, error for 5xx. When a
// 5xx response is observed and MESEDI_ALERT_WEBHOOK_URL is configured,
// the middleware also POSTs a JSON alert payload to the operator
// webhook so the team finds out before customers do (#130).
//
// Wraps the ResponseWriter to capture both the status code and an
// optional project_id stamp written by the auth middleware after a
// successful key lookup.
func requestLogMiddleware(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			durationMS := time.Since(start).Milliseconds()
			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", durationMS,
				"remote", r.RemoteAddr,
			}
			if rec.projectID != "" {
				attrs = append(attrs, "project_id", rec.projectID)
			}

			switch {
			case rec.status >= 500:
				logger.Error("http request", attrs...)
				fireAlertWebhook(r, rec.status, durationMS, rec.projectID, logger)
			case rec.status >= 400:
				logger.Warn("http request", attrs...)
			default:
				logger.Info("http request", attrs...)
			}
		})
	}
}

// fireAlertWebhook POSTs a 5xx alert to the configured operator
// webhook. Runs on a goroutine so a slow webhook never adds latency
// to the original error response (which has already been written to
// the client by the time we're called). Auto-detects Slack and
// Discord URL shapes and renders native attachments/embeds; falls
// back to a plain JSON payload for any other URL.
//
// Fails open: any error logging or HTTP failure is swallowed, the
// last thing we want is a webhook outage causing the backend to
// crash. The 5xx itself has already been logged above.
func fireAlertWebhook(r *http.Request, status int, durationMS int64, projectID string, logger *slog.Logger) {
	urlVal := AlertWebhookURL.Load()
	if urlVal == nil {
		return
	}
	url, _ := urlVal.(string)
	if url == "" {
		return
	}

	method := r.Method
	path := r.URL.Path
	host := r.Host

	go func() {
		body, contentType := alertWebhookPayload(url, status, method, path, host, projectID, durationMS)
		req, err := http.NewRequestWithContext(
			context.Background(), "POST", url, bytes.NewReader(body),
		)
		if err != nil {
			logger.Warn("alert webhook: build request failed", "error", err.Error())
			return
		}
		req.Header.Set("Content-Type", contentType)

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			logger.Warn("alert webhook: post failed", "error", err.Error())
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			logger.Warn("alert webhook: non-2xx response",
				"status", resp.StatusCode, "webhook_host", req.URL.Host)
		}
	}()
}

// alertWebhookPayload picks the right wire format for the webhook URL.
// Slack and Discord URLs get native messages so the operator can read
// the alert at a glance without parsing JSON; any other URL gets a
// generic payload that any HTTP-aware receiver can consume.
func alertWebhookPayload(url string, status int, method, path, host, projectID string, durationMS int64) ([]byte, string) {
	title := fmt.Sprintf("Mesedi backend %d", status)
	detail := fmt.Sprintf("`%s %s` on `%s` took %dms", method, path, host, durationMS)
	if projectID != "" {
		detail += fmt.Sprintf(" (project `%s`)", projectID)
	}

	// Slack incoming webhooks share host hooks.slack.com.
	if isSlackWebhook(url) {
		payload := map[string]any{
			"attachments": []map[string]any{{
				"color":  "#dc2626",
				"title":  title,
				"text":   detail,
				"footer": "mesedi-api on Fly.io",
				"ts":     time.Now().Unix(),
			}},
		}
		body, _ := json.Marshal(payload)
		return body, "application/json"
	}

	// Discord webhooks share host discord.com/api/webhooks/...
	if isDiscordWebhook(url) {
		payload := map[string]any{
			"embeds": []map[string]any{{
				"title":       title,
				"description": detail,
				"color":       0xdc2626,
				"footer":      map[string]string{"text": "mesedi-api on Fly.io"},
				"timestamp":   time.Now().UTC().Format(time.RFC3339),
			}},
		}
		body, _ := json.Marshal(payload)
		return body, "application/json"
	}

	// Generic JSON.
	payload := map[string]any{
		"service":     "mesedi-api",
		"status":      status,
		"method":      method,
		"path":        path,
		"host":        host,
		"project_id":  projectID,
		"duration_ms": durationMS,
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
	}
	body, _ := json.Marshal(payload)
	return body, "application/json"
}

func isSlackWebhook(url string) bool {
	return strings.Contains(url, "hooks.slack.com")
}

func isDiscordWebhook(url string) bool {
	return strings.Contains(url, "discord.com") || strings.Contains(url, "discordapp.com")
}

// SetProjectIDForLogging stamps the authenticated project_id onto the
// underlying statusRecorder so the request-log middleware can include
// it in the structured log line. Called by the auth middleware after
// the bearer token resolves to a project. No-op if the writer isn't a
// statusRecorder (which only happens in test harnesses that don't
// install the top chain).
func SetProjectIDForLogging(w http.ResponseWriter, projectID string) {
	if rec, ok := w.(*statusRecorder); ok {
		rec.projectID = projectID
	}
}

// statusRecorder wraps http.ResponseWriter to capture the status code
// for logging. Required because http.ResponseWriter does not expose a
// way to read back the status the handler wrote.
//
// projectID is stamped by the auth middleware after a successful
// bearer-token lookup so the request log line can attribute the
// request to the calling project.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteStatus bool
	projectID   string
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteStatus {
		return
	}
	s.status = code
	s.wroteStatus = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteStatus {
		// Default status is 200 if WriteHeader is never called explicitly.
		s.status = http.StatusOK
		s.wroteStatus = true
	}
	return s.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer's Flush implementation so
// SSE handlers (which call w.(http.Flusher).Flush() after each frame
// to push bytes to the client) can punch through this wrapper. Without
// this, the type assertion in HandleHaltStream fails and the SSE
// stream falls back to "streaming unsupported". Go's net/http stdlib
// writers implement Flusher; intermediate wrappers like statusRecorder
// must explicitly forward.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// oversizedPayloadMiddleware peeks at the Content-Length header and
// feeds the oversized-payload detector when a request body exceeds
// the configured byte threshold. Does NOT block the request: we let
// the handler run normally so the detector signal is informational,
// not a hard block. If a project starts spamming megabyte payloads,
// the worker auto-suspends 24h later just like any other abuse kind.
//
// Must run AFTER auth so we know which project is calling.
func oversizedPayloadMiddleware(detector *AbuseDetector) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > oversizedPayloadByteThreshold && detector != nil {
				if projectID, ok := ProjectIDFromContext(r.Context()); ok {
					detector.RecordOversizedPayload(r.Context(), projectID, r.ContentLength, r.Method, r.URL.Path)
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
