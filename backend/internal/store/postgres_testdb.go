// Postgres integration test harness. Spins up a real Postgres
// container per test via testcontainers-go, runs every migration
// through OpenPostgres, returns a connected *PostgresStore.
//
// Why this exists:
//
//	SQLite unit tests are fast but they silently accept TEXT-as-JSON
//	coercions that Postgres rejects (e.g. the ->> operator on a TEXT
//	column raises SQLSTATE 42883: "operator does not exist: text ->>
//	unknown"). The /me/tool-return-value-stats prod 500 was exactly
//	this class of bug, the SQLite twin's tests passed but the
//	Postgres path crashed every invocation. This harness lets every
//	postgres_*.go file have a real-engine test against the same
//	migrations the production stack runs.
//
// Usage:
//
//	st := newTestPostgresStore(t)
//	// run queries against st as you would in prod
//
// Cleanup (container teardown + connection close) is automatic via
// t.Cleanup. If Docker is not running on the host, the helper calls
// t.Skip so the rest of the test suite still runs cleanly, developers
// without Docker get a yellow SKIP, CI with Docker gets a real run.
//
// Closes pending roadmap item (Postgres CI test path for store
// sidecars) by establishing the reusable harness.

package store

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// newTestPostgresStore returns a *PostgresStore connected to a fresh
// Postgres container with every migrations-postgres/*.sql file applied.
// Skips the test if Docker is unavailable. Container + connection are
// torn down via t.Cleanup. The returned store is safe to seed via
// st.db.ExecContext and exercise via the normal Store-interface
// methods.
//
// The image is pinned to postgres:16-alpine to match the major
// version Neon is on in production. Each test gets its own container
// so tests are fully independent (no shared schema state, no risk
// of one test's seed leaking into another).
func newTestPostgresStore(t *testing.T) *PostgresStore {
	t.Helper()
	ctx := context.Background()

	pgC, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("mesedi_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		// Docker unreachable or postgres image pull failed, skip
		// cleanly so a dev without Docker doesn't see a hard failure.
		// CI runners (GitHub Actions ubuntu-latest) have Docker
		// preinstalled so this skip only fires on local laptops
		// without Docker Desktop running.
		t.Skipf("postgres container unavailable (Docker not running?): %v", err)
		return nil
	}
	t.Cleanup(func() {
		// Best-effort termination; if it fails the next test run
		// will GC the container via testcontainers' Reaper sidecar.
		_ = pgC.Terminate(context.Background())
	})

	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("get connection string: %v", err)
	}

	// Discard the migration-applied INFO line so test output stays
	// clean; OpenPostgres emits one slog.Info call per startup.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := OpenPostgres(dsn, logger)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	return st
}
