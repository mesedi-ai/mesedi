package store

import "embed"

// migrationsFS embeds every SQL file in the migrations/ directory into
// the binary at build time. This lets `go run` and `go build` deployments
// carry their schema with them, no separate migration tooling required
// to deliver the schema to a fresh local environment.
//
// Migration files are applied in lexical order; name them with a numeric
// prefix (`001_initial.sql`, `002_add_failure_groups.sql`, etc.) to
// guarantee correct ordering.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS
