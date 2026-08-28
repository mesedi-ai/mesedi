// Tests for the /ready readiness probe.
//
// The bug these exist to prevent is not a crash. It is an endpoint that
// returns 200 while the system underneath it is broken, which is what
// /health did for the whole life of the service. So the assertions here
// are mostly about FAILING correctly: every one of them would pass
// against a handler that always returns 200 unless it explicitly checks
// the failure path, and several do exactly that.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mesedi/backend/internal/store"
)

// stubReadyStore implements only what the probe touches. Embedding
// store.Store means any other method the probe starts calling will
// nil-panic and fail loudly rather than pass quietly.
type stubReadyStore struct {
	store.Store

	pingErr    error
	pingCalls  int
	status     store.SchemaStatus
	statusErr  error
	statusCall int
}

func (s *stubReadyStore) Ping(context.Context) error {
	s.pingCalls++
	return s.pingErr
}

func (s *stubReadyStore) SchemaStatus(context.Context) (store.SchemaStatus, error) {
	s.statusCall++
	return s.status, s.statusErr
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func doReady(t *testing.T, st store.Store) (*http.Response, readyResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	handleReady(st, quietLogger())(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))

	res := rec.Result()
	var body readyResponse
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return res, body
}

func TestReadyReturns200WhenEverythingWorks(t *testing.T) {
	t.Parallel()

	st := &stubReadyStore{status: store.SchemaStatus{Embedded: 56, Applied: 56}}
	res, body := doReady(t, st)

	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if !body.OK {
		t.Error("ok = false, want true")
	}
	if body.Checks["database"] != "ok" || body.Checks["schema"] != "ok" {
		t.Errorf("checks = %v, want both ok", body.Checks)
	}
	if body.Reason != "" {
		t.Errorf("reason = %q, want empty on success", body.Reason)
	}
	if st.pingCalls == 0 {
		t.Error("Ping was never called; the probe is not actually checking the database, " +
			"which is precisely the /health bug this endpoint replaces")
	}
	if st.statusCall == 0 {
		t.Error("SchemaStatus was never called; a reachable database with a half-applied " +
			"migration would report ready")
	}
}

// THE REGRESSION TEST. A database that cannot be reached must produce a
// 503, not a cheerful 200.
func TestReadyReturns503WhenDatabaseIsUnreachable(t *testing.T) {
	t.Parallel()

	st := &stubReadyStore{pingErr: errors.New("dial tcp 10.0.0.1:5432: connect: connection refused")}
	res, body := doReady(t, st)

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503. An uptime monitor polling this endpoint "+
			"would have stayed green through a total database outage.", res.StatusCode)
	}
	if body.OK {
		t.Error("ok = true with an unreachable database")
	}
	if body.Reason != readyReasonDatabaseUnreachable {
		t.Errorf("reason = %q, want %q", body.Reason, readyReasonDatabaseUnreachable)
	}
}

// The response is unauthenticated. Driver errors carry hosts, ports and
// usernames, so none of it may reach the body.
func TestReadyDoesNotLeakDriverErrorDetail(t *testing.T) {
	t.Parallel()

	// FABRICATED ON PURPOSE. Every value below is invented.
	//
	// The first version of this test used the real Neon endpoint hostname and
	// the real database username, so that the fixture would "look realistic".
	// This repository is public. GitHub secret scanning raised it the same day
	// (alert #5, 2026-08-28). The password in it was always fake, but the
	// endpoint and the username were not, and publishing those turns "find
	// their database" into "guess one password".
	//
	// A test asserting that a DSN never reaches the response body needs a
	// DSN-SHAPED string. It does not need a real one, and there was no moment
	// where using the real one made this test stronger.
	secret := "postgres://db_user:not-a-real-password@ep-example-00000000.us-east-1.aws.neon.tech:5432/appdb"
	st := &stubReadyStore{pingErr: errors.New("failed to connect to " + secret)}

	rec := httptest.NewRecorder()
	handleReady(st, quietLogger())(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))

	raw := rec.Body.String()
	for _, leak := range []string{"not-a-real-password", "db_user", "ep-example", "neon.tech"} {
		if strings.Contains(raw, leak) {
			t.Errorf("response body leaks %q to an unauthenticated caller:\n%s", leak, raw)
		}
	}
}

// A reachable database whose schema is behind this binary is the
// migration-056 failure shape: the socket answers, every query against
// a new column does not.
func TestReadyReturns503WhenSchemaIsBehind(t *testing.T) {
	t.Parallel()

	st := &stubReadyStore{status: store.SchemaStatus{Embedded: 56, Applied: 55}}
	res, body := doReady(t, st)

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the database is a migration behind", res.StatusCode)
	}
	if body.Reason != readyReasonSchemaBehind {
		t.Errorf("reason = %q, want %q", body.Reason, readyReasonSchemaBehind)
	}
	// The counts are the most useful thing to see during an incident,
	// so they must survive into the failure body.
	if body.Migrations == nil || body.Migrations.Embedded != 56 || body.Migrations.Applied != 55 {
		t.Errorf("migrations = %+v, want embedded 56 applied 55", body.Migrations)
	}
}

// A database AHEAD of this binary is healthy. During a rolling deploy
// the new machine applies migration N+1 while an old machine still
// serves; failing readiness there would pull working machines out of
// rotation on every single deploy.
func TestReadyStaysHealthyWhenDatabaseIsAheadOfBinary(t *testing.T) {
	t.Parallel()

	st := &stubReadyStore{status: store.SchemaStatus{Embedded: 56, Applied: 57}}
	res, body := doReady(t, st)

	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200. Applied > Embedded happens on every rolling "+
			"deploy and is not a failure.", res.StatusCode)
	}
	if !body.OK {
		t.Error("ok = false while the database is merely ahead of this binary")
	}
}

func TestReadyReports503WhenSchemaCannotBeRead(t *testing.T) {
	t.Parallel()

	st := &stubReadyStore{statusErr: errors.New("relation \"schema_migrations\" does not exist")}
	res, body := doReady(t, st)

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", res.StatusCode)
	}
	if body.Reason != readyReasonSchemaUnreadable {
		t.Errorf("reason = %q, want %q", body.Reason, readyReasonSchemaUnreadable)
	}
}

// Caching must bound the query rate without ever masking a state
// change for longer than the TTL.
func TestReadyCachesWithinTTLAndRefreshesAfter(t *testing.T) {
	t.Parallel()

	st := &stubReadyStore{status: store.SchemaStatus{Embedded: 56, Applied: 56}}
	probe := newReadinessProbe(st, quietLogger())

	clock := time.Now()
	probe.now = func() time.Time { return clock }

	for i := 0; i < 5; i++ {
		probe.evaluate(context.Background())
	}
	if st.pingCalls != 1 {
		t.Errorf("Ping called %d times for 5 requests inside the TTL, want 1; an "+
			"unauthenticated endpoint must not issue a query per request", st.pingCalls)
	}

	clock = clock.Add(probe.ttl + time.Millisecond)
	probe.evaluate(context.Background())
	if st.pingCalls != 2 {
		t.Errorf("Ping called %d times after the TTL expired, want 2; a cached verdict "+
			"that never refreshes is a check that cannot fail", st.pingCalls)
	}
}

// /health must NOT depend on the database. fly.toml polls it every 30s
// as a liveness check, and cycling machines on a database blip turns a
// partial outage into a total one. This pins that separation.
func TestHealthStaysIndependentOfTheDatabase(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	handleHealth(quietLogger())(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// If someone later wires a store into handleHealth, this test will
	// not compile, which is the intended alarm. The comment is the
	// other half of the guard: read cmd/api/ready.go before changing it.
}
