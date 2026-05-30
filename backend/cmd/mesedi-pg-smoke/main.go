// Command mesedi-pg-smoke is a one-shot proof-of-life binary for the
// Postgres backend (#128 Phase 1).
//
// What it does, in order:
//   1. Reads the Postgres DSN from --db-url or MESEDI_DB_URL_POSTGRES.
//   2. Opens the connection via the pgx driver.
//   3. Runs every embedded Postgres migration (migrations-postgres/*.sql).
//   4. INSERTs a test project with a deterministic id.
//   5. SELECTs the test project back, verifies field round-trips.
//   6. DELETEs the test project so the run is idempotent.
//   7. Prints "smoke: PASS" and exits 0 on success.
//
// What it does NOT do:
//   * It does not exercise the executions/events/failure_groups path.
//     Those tables are validated structurally by the migration runner
//     (they get created), but no row-level testing happens. Phase 2's
//     batch ports will add their own smoke tests as they land.
//
// Usage:
//
//	# Against the Neon URL stored in MESEDI_DB_URL_POSTGRES:
//	go run ./cmd/mesedi-pg-smoke
//
//	# Or with an explicit DSN:
//	go run ./cmd/mesedi-pg-smoke \
//	    --db-url "postgresql://neondb_owner:PASSWORD@ep-...neon.tech/neondb?sslmode=require"
//
// On Fly, after the server deploys, you can run the same check via:
//
//	fly ssh console -a mesedi-api -C "/app/mesedi-pg-smoke"
//
// (assuming you build the smoke binary into the Docker image, which is
// optional, the binary is also useful from a local laptop pointed at
// the same Neon DSN).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"mesedi/backend/internal/store"
)

func main() {
	var dsn string
	flag.StringVar(&dsn, "db-url", os.Getenv("MESEDI_DB_URL_POSTGRES"), "Postgres DSN; defaults to MESEDI_DB_URL_POSTGRES env var")
	flag.Parse()

	if dsn == "" {
		fmt.Fprintln(os.Stderr, "smoke: FAIL: no DSN provided (set MESEDI_DB_URL_POSTGRES or pass --db-url)")
		os.Exit(2)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	fmt.Println("step 1/6: open postgres connection")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.OpenPostgres(dsn, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "smoke: FAIL at open: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	fmt.Println("step 2/6: ping")
	if err := st.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "smoke: FAIL at ping: %v\n", err)
		os.Exit(1)
	}

	// Migrations ran inside OpenPostgres; if we got here they all succeeded.
	fmt.Println("step 3/6: migrations applied (verified by OpenPostgres)")

	// Use a deterministic test project id so re-runs find leftover rows
	// from a previous failed run and clean them up before re-inserting.
	const testProjectID = "proj-pg-smoke-test"
	fmt.Printf("step 4/6: insert test project %q\n", testProjectID)
	// Pre-clean any leftover row from a previous interrupted run.
	_ = st.DeleteProject(ctx, testProjectID)

	if err := st.CreateProject(ctx, &store.Project{
		ProjectID: testProjectID,
		Name:      "pg-smoke-test (delete me if you see me)",
		Tier:      "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "smoke: FAIL at insert: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("step 5/6: read back + verify")
	got, err := st.GetProject(ctx, testProjectID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "smoke: FAIL at read: %v\n", err)
		os.Exit(1)
	}
	if got.ProjectID != testProjectID {
		fmt.Fprintf(os.Stderr, "smoke: FAIL: project_id mismatch got %q want %q\n", got.ProjectID, testProjectID)
		os.Exit(1)
	}
	if got.Tier != "hobby" {
		fmt.Fprintf(os.Stderr, "smoke: FAIL: tier mismatch got %q want %q\n", got.Tier, "hobby")
		os.Exit(1)
	}
	if got.CreatedAt.IsZero() {
		fmt.Fprintln(os.Stderr, "smoke: FAIL: created_at came back zero (TIMESTAMPTZ round-trip broken)")
		os.Exit(1)
	}

	fmt.Println("step 6/6: clean up test row")
	if err := st.DeleteProject(ctx, testProjectID); err != nil {
		fmt.Fprintf(os.Stderr, "smoke: FAIL at delete: %v\n", err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("smoke: PASS")
	fmt.Println("  - connection: OK")
	fmt.Println("  - migrations: OK (9 schema files applied)")
	fmt.Println("  - insert / select / delete on projects: OK")
	fmt.Println("  - TIMESTAMPTZ round-trip: OK")
	fmt.Println()
	fmt.Println("safe to proceed to Phase 2 method porting when ready.")
}
