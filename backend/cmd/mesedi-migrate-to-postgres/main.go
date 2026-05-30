// Command mesedi-migrate-to-postgres copies live data from a Mesedi
// SQLite database into a Postgres database, table by table, with
// row-count verification at the end.
//
// PHASE 1 STATUS (this version): SKELETON ONLY. The framing, the table
// list, the verification pattern, and the CLI flags are in place. The
// actual per-table copy logic ports during Phase 2 of #128 alongside
// the PostgresStore method ports, so the data shape understood by the
// migrator stays in sync with what PostgresStore writes.
//
// Why a custom tool instead of pg_loader or `pg_dump` + sed?
//   * SQLite stores booleans as INTEGER 0/1; Postgres uses real BOOLEAN.
//     pg_loader handles this but only with explicit casts that are
//     easy to miss.
//   * Several timestamp columns are TEXT in SQLite (RFC3339) and
//     TIMESTAMPTZ in Postgres; we want to parse + re-emit in transit.
//   * Some columns are INTEGER epochs in SQLite and BIGINT epochs in
//     Postgres, which copies cleanly. We can keep semantics identical.
//   * Doing it in Go means we share the time-parsing + null-handling
//     helpers used by the Store implementations themselves, no
//     dialect drift across the migration.
//
// Usage (when Phase 2 ships):
//
//	go run ./cmd/mesedi-migrate-to-postgres \
//	    --sqlite "file:./mesedi-dev.db" \
//	    --postgres "$MESEDI_DB_URL_POSTGRES" \
//	    --dry-run    # show counts only, no writes
//
// In production cutover, the operator stops the Fly app, scp's the
// SQLite file out, runs this tool locally pointing at Neon, verifies
// counts, then flips the MESEDI_DB_URL_POSTGRES secret on Fly so the
// next restart picks up Postgres.
package main

import (
	"flag"
	"fmt"
	"os"
)

const phase1Notice = `
mesedi-migrate-to-postgres: PHASE 1 SKELETON

This binary is intentionally a no-op in Phase 1 of #128. The Postgres
backend (PostgresStore) currently implements only CreateProject /
GetProject / DeleteProject; copying data without the rest of the
write methods would leave you with orphaned partial state.

Phase 2 ports the remaining Store methods batch by batch. When each
batch lands, the corresponding section of this migrator's per-table
copy logic lights up. The plan:

  table              ported with batch
  -----              -----------------
  projects           Phase 1 (today)
  api_keys           Phase 2.1 (auth)
  executions         Phase 2.2 (ingest)
  events             Phase 2.2 (ingest)
  failure_groups     Phase 2.4 (failure-class)
  project_webhooks   Phase 2.6 (webhooks)
  webhook_deliveries Phase 2.7 (delivery log)
  abuse_signals      Phase 2.9 (abuse)
  schema_migrations  copied verbatim, version rows preserved

For now, use the mesedi-pg-smoke binary to verify the connection +
migration runner end to end against Neon:

  go run ./cmd/mesedi-pg-smoke
`

func main() {
	var sqlitePath string
	var pgURL string
	var dryRun bool
	flag.StringVar(&sqlitePath, "sqlite", "", "SQLite DSN (file:./mesedi-dev.db)")
	flag.StringVar(&pgURL, "postgres", os.Getenv("MESEDI_DB_URL_POSTGRES"), "Postgres DSN; defaults to MESEDI_DB_URL_POSTGRES env var")
	flag.BoolVar(&dryRun, "dry-run", false, "show row counts only, no writes")
	flag.Parse()

	fmt.Println(phase1Notice)
	if sqlitePath == "" || pgURL == "" {
		// Print the notice and exit 0; the operator gets the message.
		return
	}
	_ = dryRun
	fmt.Println("(flags accepted; copy logic will activate in Phase 2)")
}
