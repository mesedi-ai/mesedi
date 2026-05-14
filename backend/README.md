# Mesedi backend

Go HTTP service that ingests AI agent execution telemetry, runs detection engines, and surfaces alerts via webhook + dashboard.

**Status:** Phase 1 scaffolding (local development only — no public repo yet).

## Quickstart (local development)

Prerequisites: Go 1.25+ installed (`brew install go` on macOS).

```bash
cd backend

# First run only: fetch deps
go mod tidy

# Run the service (creates mesedi-dev.db + runs migrations + bootstraps dev project on first run)
go run cmd/api/main.go
```

### Verify `/health` (public, no auth)

```bash
curl http://localhost:8080/health
# Expected: {"ok":true,"service":"mesedi-backend","version":"0.0.1","time":"..."}
```

### Verify auth-required endpoints

The local dev environment auto-bootstraps a fixed API key for testing. **This key is non-secret and is hardcoded — never use it for anything other than local development.**

```bash
# The dev API key
DEV_KEY="mesedi_sk_dev_local_only"

# Create an execution (requires Authorization: Bearer ...)
curl -X POST http://localhost:8080/executions \
  -H "Authorization: Bearer $DEV_KEY" \
  -H "Content-Type: application/json" \
  -d '{"execution_id":"exec-001","status":"started","sdk_language":"python"}'

# Send a batch of events
curl -X POST http://localhost:8080/events \
  -H "Authorization: Bearer $DEV_KEY" \
  -H "Content-Type: application/json" \
  -d '[{"event_id":"evt-001","execution_id":"exec-001","event_type":"llm_call","sequence":1,"timestamp":"2026-05-14T14:00:00Z","payload":{"model":"claude-opus-4-6"}}]'

# Mark execution completed
curl -X PATCH http://localhost:8080/executions/exec-001 \
  -H "Authorization: Bearer $DEV_KEY" \
  -H "Content-Type: application/json" \
  -d '{"status":"completed","duration_ms":1234,"total_tokens_in":50,"total_tokens_out":200}'
```

Requests without `Authorization: Bearer` are rejected with 401. Requests with an unrecognized key are rejected with 401. Requests with the wrong project_id in the body (vs the auth-context project_id) are rejected with 403.

## Configuration

12-factor environment variables. Copy `.env.example` to `.env` and fill in values when needed. Flags override env vars.

| Variable | Flag | Default | Purpose |
|---|---|---|---|
| `MESEDI_PORT` | `--port` | `8080` | TCP port the HTTP server binds to |
| `MESEDI_LOG_LEVEL` | `--log-level` | `info` | `debug` / `info` / `warn` / `error` |
| `MESEDI_DB_URL` | `--db-url` | _(empty)_ | Postgres connection string (required Phase 1.5+) |

## Directory layout

```
backend/
├── cmd/
│   └── api/
│       └── main.go              # binary entry point
├── internal/
│   ├── api/                     # HTTP handlers, middleware, routing
│   ├── config/                  # 12-factor config loader
│   ├── events/                  # event type definitions (LLMCall, ToolCall, etc.)
│   └── store/
│       └── migrations/          # Postgres schema migrations (SQL files)
├── go.mod
├── .env.example
├── .gitignore
└── README.md
```

## Roadmap (from `mesedi/docs/DEVELOPMENT_CHECKLIST.md`)

Current: Phase 1 scaffold — `/health` endpoint serving locally.

Next:
- Phase 1 completion: `POST /executions`, `POST /events`, bearer-token auth, basic rate limiting
- Phase 2: Python SDK with `@wrap` decorator pattern
- Phase 3: crash detection + dashboard MVP

Full phase-by-phase plan in `../docs/DEVELOPMENT_CHECKLIST.md` (or `../docs/DEVELOPMENT_CHECKLIST.pdf`).

## Local-only posture

This codebase is intentionally not on GitHub or any public surface until Verdifax LOI signs. All development happens locally. Don't push to any remote, don't share screenshots publicly, don't mention the project name externally.
