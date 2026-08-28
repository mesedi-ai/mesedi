// /ready — the readiness probe that /health only pretended to be.
//
// WHY THIS IS A SEPARATE ENDPOINT FROM /health
// fly.toml points Fly's machine health check at /health, every 30s with
// a 5s timeout. If /health started depending on the database, a
// transient Neon blip would make Fly judge the machine unhealthy and
// cycle it, converting a brief database problem into a full outage and
// dropping in-flight requests. So the split is deliberate:
//
//	/health  liveness   is this process alive and serving?   Fly polls this.
//	/ready   readiness  can it actually do its job?          Uptime monitoring polls this.
//
// WHY IT REPORTS NO ERROR DETAIL
// /ready is unauthenticated, because an external uptime monitor has no
// credentials. Raw driver errors routinely contain the database host,
// user and port, so the response carries a fixed reason code and the
// detail goes to the log instead.
//
// WHY THE RESULT IS CACHED
// An unauthenticated endpoint that issues a query per request is a
// free amplifier against our own database. The result is cached for a
// few seconds, which is invisible at any sane monitor interval and
// caps the query rate no matter who is calling.

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"mesedi/backend/internal/store"
)

// Reason codes. Fixed strings, safe to expose, greppable in logs.
const (
	readyReasonDatabaseUnreachable = "database_unreachable"
	readyReasonSchemaUnreadable    = "schema_unreadable"
	readyReasonSchemaBehind        = "schema_behind"
)

// readyResponse is the JSON body of /ready. Field order and names are
// part of the contract an uptime monitor may assert on, so treat them
// as public API.
type readyResponse struct {
	OK      bool              `json:"ok"`
	Service string            `json:"service"`
	Version string            `json:"version"`
	Checks  map[string]string `json:"checks"`
	// Reason is empty when OK. It is a fixed code, never a raw error.
	Reason string `json:"reason,omitempty"`
	// Migrations is included on success and on schema failure because
	// "56 embedded, 55 applied" is the single most useful line to read
	// during an incident.
	Migrations *readyMigrations `json:"migrations,omitempty"`
	Time       string           `json:"time"`
}

type readyMigrations struct {
	Embedded int `json:"embedded"`
	Applied  int `json:"applied"`
}

// readinessProbe evaluates readiness and caches the verdict.
type readinessProbe struct {
	store   store.Store
	logger  *slog.Logger
	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time

	mu       sync.Mutex
	cached   *readyResponse
	cachedAt time.Time
}

func newReadinessProbe(st store.Store, logger *slog.Logger) *readinessProbe {
	return &readinessProbe{
		store:  st,
		logger: logger,
		// Well under any monitor interval, so a monitor never reads a
		// stale verdict, while a flood of requests still collapses to
		// at most one query every few seconds.
		ttl: 5 * time.Second,
		// Shorter than Fly's 5s check timeout and far shorter than any
		// monitor's, so a hung database returns a clean 503 rather than
		// letting the caller time out with no answer at all.
		timeout: 3 * time.Second,
		now:     time.Now,
	}
}

// evaluate runs the checks, or returns the cached verdict if it is
// still fresh.
func (p *readinessProbe) evaluate(ctx context.Context) *readyResponse {
	p.mu.Lock()
	if p.cached != nil && p.now().Sub(p.cachedAt) < p.ttl {
		cached := *p.cached
		p.mu.Unlock()
		return &cached
	}
	p.mu.Unlock()

	res := p.check(ctx)

	p.mu.Lock()
	p.cached, p.cachedAt = res, p.now()
	p.mu.Unlock()

	out := *res
	return &out
}

func (p *readinessProbe) check(ctx context.Context) *readyResponse {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	res := &readyResponse{
		Service: serviceName,
		Version: serviceVersion,
		Checks:  map[string]string{},
		Time:    time.Now().UTC().Format(time.RFC3339),
	}

	if err := p.store.Ping(ctx); err != nil {
		// The detail goes here and nowhere else. This is the log line
		// that did not exist during the outage the endpoint missed.
		p.logger.Error("readiness: database ping failed", "error", err.Error())
		res.OK = false
		res.Checks["database"] = "unreachable"
		res.Checks["schema"] = "unknown"
		res.Reason = readyReasonDatabaseUnreachable
		return res
	}
	res.Checks["database"] = "ok"

	status, err := p.store.SchemaStatus(ctx)
	if err != nil {
		p.logger.Error("readiness: schema status query failed", "error", err.Error())
		res.OK = false
		res.Checks["schema"] = "unknown"
		res.Reason = readyReasonSchemaUnreadable
		return res
	}
	res.Migrations = &readyMigrations{Embedded: status.Embedded, Applied: status.Applied}

	if status.Behind() {
		p.logger.Error("readiness: database is behind this binary's migrations",
			"embedded", status.Embedded, "applied", status.Applied)
		res.OK = false
		res.Checks["schema"] = "behind"
		res.Reason = readyReasonSchemaBehind
		return res
	}

	res.OK = true
	res.Checks["schema"] = "ok"
	return res
}

// handleReady serves GET /ready. 200 when ready, 503 when not, so a
// monitor needs no body parsing to alert correctly.
func handleReady(st store.Store, logger *slog.Logger) http.HandlerFunc {
	probe := newReadinessProbe(st, logger)
	return func(w http.ResponseWriter, r *http.Request) {
		res := probe.evaluate(r.Context())

		w.Header().Set("Content-Type", "application/json")
		// Readiness must never be served from a proxy or browser cache;
		// the whole value of the endpoint is that it reflects now.
		w.Header().Set("Cache-Control", "no-store")
		if res.OK {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		if err := json.NewEncoder(w).Encode(res); err != nil {
			logger.Warn("readiness: encode response failed", "error", err.Error())
		}
	}
}
