package store

import "embed"

// migrationsFS embeds the SQLite-dialect migrations into the binary at
// build time. Used by the SQLite backend (sqlite.go) and the data-
// migration tool that reads SQLite source data.
//
// Migration files are applied in lexical order; name them with a numeric
// prefix (`001_initial.sql`, `002_add_failure_groups.sql`, etc.) to
// guarantee correct ordering.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationsPostgresFS embeds the Postgres-dialect migrations. Same
// numbering scheme, same logical schema; only the dialect differs
// (TIMESTAMPTZ vs TIMESTAMP, ON CONFLICT vs OR IGNORE, BOOLEAN vs
// INTEGER, etc.). Used by postgres.go's migration runner. Adding a
// new migration requires writing both flavors and incrementing in
// lockstep, the schema_migrations.version counter is shared semantically.
//
//go:embed migrations-postgres/*.sql
var migrationsPostgresFS embed.FS
