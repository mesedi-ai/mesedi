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
//
// WHY IT REPORTS FIPS POSTURE
// The binary is built with GOFIPS140=certified, which links the Go
// Cryptographic Module holding CMVP Certificate #5247. Until now the
// only evidence of that was a line in the startup log, which you can
// read only if you already have log access — that is, only if you are
// us. A compliance claim that just its author can confirm is worth
// very little, so the posture is reported here, where any monitor or
// third-party evaluator can poll it without credentials.
//
// The endpoint is unauthenticated, so this does publish the posture to
// anyone. That was weighed and accepted: the leak is one boolean, and
// this same response already carries `version`, which is a strictly
// larger disclosure because it maps the binary to its known CVEs. A
// false value reveals no more than the parameters of a TLS handshake
// with the host would.

package main

import (
	"context"
	"crypto/fips140"
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
	// Crypto is a value, not a pointer, and carries no omitempty: it is
	// present on every response including the 503s. A compliance
	// attribute that vanishes when the database is down is worse than
	// no attribute, because an evaluator polling during an incident
	// cannot tell "not in FIPS mode" from "field not implemented".
	Crypto readyCrypto `json:"crypto"`
	Time   string      `json:"time"`
}

type readyMigrations struct {
	Embedded int `json:"embedded"`
	Applied  int `json:"applied"`
}

// readyCrypto is an object rather than a bare top-level boolean so the
// module version can join it without another API-shape change. That
// version is deliberately absent today: crypto/fips140.Version() needs
// go1.26 and backend/go.mod declares go1.25. Add it when the go
// directive moves, alongside the same note in the Dockerfile.
type readyCrypto struct {
	// FIPS140 is what crypto/fips140.Enabled() reports for THIS binary.
	// A build without GOFIPS140=certified honestly reports false rather
	// than omitting the field; silence would be indistinguishable from
	// a monitor that was never updated.
	FIPS140 bool `json:"fips140"`
}

// readinessProbe evaluates readiness and caches the verdict.
type readinessProbe struct {
	store   store.Store
	logger  *slog.Logger
	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time

	// fipsEnabled is read once at construction because it is fixed at
	// link time and cannot change while the process runs. Held on the
	// struct for the same reason `now` is: it makes both values
	// substitutable in tests, so the false branch is reachable on a
	// machine whose own binary was built in FIPS mode.
	fipsEnabled bool

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
		timeout:     3 * time.Second,
		now:         time.Now,
		fipsEnabled: fips140.Enabled(),
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
		// Set here, above every early return, so the posture is
		// reported on the 503 paths as well as the 200. It is a fact
		// about the binary, not a result of the checks below, and it
		// never contributes to res.OK: FIPS mode is a compliance
		// property, not a readiness condition. Wiring it into OK would
		// make every non-FIPS local and CI build report unready, and
		// developers would learn to ignore this endpoint.
		Crypto: readyCrypto{FIPS140: p.fipsEnabled},
		Time:   time.Now().UTC().Format(time.RFC3339),
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
