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
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"mesedi/backend/internal/store"
)

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
func NewAuthChain(logger *slog.Logger, s store.Store) Middleware {
	return chain(
		authMiddleware(s),
		schemaVersionMiddleware(),
		rateLimitMiddleware(logger),
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

// requestLogMiddleware logs every request's method, path, status, and
// duration. Useful for local dev and as a permanent audit log line in
// production. Wraps the ResponseWriter to capture the status code that
// the downstream handler eventually writes.
func requestLogMiddleware(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			logger.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"remote", r.RemoteAddr,
			)
		})
	}
}

// statusRecorder wraps http.ResponseWriter to capture the status code
// for logging. Required because http.ResponseWriter does not expose a
// way to read back the status the handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteStatus bool
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
