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

### Wire-format version (`X-Mesedi-Schema-Version`)

The Mesedi SDK SHOULD send `X-Mesedi-Schema-Version: 1` on every request to the protected ingest endpoints. The backend's current policy is "enforced if present":

- Header missing → request accepted (assumed to be the current version; soft-mode for local curl smoke tests and the bring-up flow).
- Header present and equals `1` → request accepted.
- Header present and not `1` → rejected with 400 and a message naming the supported version(s).

Smoke-test the negative path:

```bash
# Should return 400 with an informative error.
curl -X POST http://localhost:8080/executions \
  -H "Authorization: Bearer mesedi_sk_dev_local_only" \
  -H "X-Mesedi-Schema-Version: 99" \
  -H "Content-Type: application/json" \
  -d '{"execution_id":"exec-schema-test","status":"started"}'
```

Once the real SDK ships with the header set by default, the policy tightens to "missing → 400" too — at that point any unversioned caller is a legacy bug worth surfacing loudly.

### Rate limiting (per-project token bucket)

Every authenticated request consumes 1 token from the calling project's bucket. The defaults are:

- **Burst capacity:** 100 tokens (each project starts with a full bucket)
- **Refill rate:** 10 tokens / second

A well-behaved SDK (events buffered client-side, flushed in batches of ~100 every few hundred ms) never sees a 429. An infinite-loop agent without backoff hits 429 within ~10 seconds.

Every response (200 and 429 alike) includes the standard headers:

- `X-RateLimit-Limit` — bucket capacity
- `X-RateLimit-Remaining` — tokens left after this request
- `X-RateLimit-Reset` — Unix timestamp when the bucket would refill to full

On 429, AWS-style `Retry-After: 1` is also set.

Smoke-test the limit:

```bash
# Inspect headers on a single request:
curl -i -X POST http://localhost:8080/executions \
  -H "Authorization: Bearer mesedi_sk_dev_local_only" \
  -H "X-Mesedi-Schema-Version: 1" \
  -H "Content-Type: application/json" \
  -d '{"execution_id":"exec-rl-headers","status":"started"}' \
  2>&1 | grep -i "HTTP\|ratelimit\|retry"

# Burst test: fire 200 requests, count status codes.
# Expect ~100-140 200s and ~60-100 429s (depends on how fast curl runs).
DEV_KEY="mesedi_sk_dev_local_only"
for i in $(seq 1 200); do
  curl -s -o /dev/null -w "%{http_code}\n" \
    -X POST http://localhost:8080/executions \
    -H "Authorization: Bearer $DEV_KEY" \
    -H "X-Mesedi-Schema-Version: 1" \
    -H "Content-Type: application/json" \
    -d "{\"execution_id\":\"exec-rl-$i\",\"status\":\"started\"}"
done | sort | uniq -c
```

Storage is in-memory today (single-instance only). Per-project overrides will eventually come from columns on the `projects` table; multi-instance deployments will swap the in-memory map for a Redis-backed implementation behind the same interface.

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
