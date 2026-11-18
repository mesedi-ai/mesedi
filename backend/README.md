# Mesedi backend

Go HTTP service that ingests AI agent execution telemetry, runs detection engines, and surfaces alerts via webhook + dashboard.

**Status:** v1. The hosted API (api.mesedi.ai, on Fly.io) has been paused since 19 October 2026 while Mesedi Cloud is in private beta. Self-hostable from this directory.

## Quickstart (local development)

Prerequisites: Go 1.21 or later (`brew install go` on macOS). go.mod asks for Go 1.27.4, and Go downloads that version automatically on first run.

```bash
cd backend

# First run only: fetch deps
go mod download

# Run the service with the local dev project switched on
# (creates mesedi-dev.db, runs migrations, creates the dev project and key)
MESEDI_DEV_BOOTSTRAP=1 go run ./cmd/api
```

`MESEDI_DEV_BOOTSTRAP=1` is for local development only. Without it, the dev project is not created, and the dev key below is removed on start if it exists, because its value is public. It is ignored on Fly.

### Verify `/health` (public, no auth)

```bash
curl http://localhost:8080/health
# Expected: {"ok":true,"service":"mesedi-backend","version":"0.0.1","time":"..."}
```

### Verify auth-required endpoints

With `MESEDI_DEV_BOOTSTRAP=1`, the server creates a fixed API key for testing, with a verified local owner email so the requests below are accepted. **This key is non-secret and is hardcoded, never use it for anything other than local development.**

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

Once the real SDK ships with the header set by default, the policy tightens to "missing → 400" too, at that point any unversioned caller is a legacy bug worth surfacing loudly.

### Rate limiting (per-project token bucket)

Every authenticated request consumes 1 token from the calling project's bucket. The defaults are:

- **Burst capacity:** 100 tokens (each project starts with a full bucket)
- **Refill rate:** 10 tokens / second

A well-behaved SDK (events buffered client-side, flushed in batches of ~100 every few hundred ms) never sees a 429. An infinite-loop agent without backoff hits 429 within ~10 seconds.

Every response (200 and 429 alike) includes the standard headers:

- `X-RateLimit-Limit`, bucket capacity
- `X-RateLimit-Remaining`, tokens left after this request
- `X-RateLimit-Reset`, Unix timestamp when the bucket would refill to full

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

## Building without internet access

The SQLite driver (`modernc.org/sqlite` and its libraries) is served from
gitlab.com, and every module normally comes through proxy.golang.org. On a
machine that can reach neither, pack the modules on a connected machine
from a clean checkout:

```bash
python3 tools/backend_module_bundle.py
```

It writes `mesedi-backend-modules-<commit>.tar.gz` and prints its SHA-256.
On the other machine, from a checkout of the same commit:

```bash
mkdir -p /tmp/mesedi-modules
tar -xzf mesedi-backend-modules-<commit>.tar.gz -C /tmp/mesedi-modules
cd backend
GOPROXY=file:///tmp/mesedi-modules/download GOSUMDB=off go build ./...
```

Go checks every module it reads from the file against `backend/go.sum`
from the checkout, so a file with an altered module is refused with
"checksum mismatch". The file holds modules only: the machine needs the
Go version in `go.mod`, or newer, already installed. CI builds the
backend this way on every push.

## Configuration

12-factor environment variables. Copy `.env.example` to `.env` and fill in values when needed. Flags override env vars.

| Variable | Flag | Default | Purpose |
|---|---|---|---|
| `MESEDI_PORT` | `--port` | `8080` | TCP port the HTTP server binds to |
| `MESEDI_LOG_LEVEL` | `--log-level` | `info` | `debug` / `info` / `warn` / `error` |
| `MESEDI_DB_URL` | `--db-url` | _(empty)_ | Postgres connection string (required for production) |
| `MESEDI_ANCHOR_SIGNING_KEY` | | _(empty: chain off)_ | ECDSA P-256 private key (PEM) that signs each hourly checkpoint published on Sigstore's public log. A secret; publish only its public key |
| `MESEDI_REKOR_URL` | | `https://rekor.sigstore.dev` | The Rekor log to anchor on |
| `MESEDI_OTS_CALENDARS` | | public calendars | OpenTimestamps calendars for the Bitcoin anchor, comma separated; `off` turns it off |

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

## Status

All failure-class detectors are live: 20+ named classes (24 detector signatures, counting the loop sub-variants: identical-call, similar-call, step-count, time-budget). See `internal/severity/severity.go` for the canonical list. Failure-group deduplication, webhook escalation on first occurrence, hard-halt with local budgets + SSE remote channel, multi-agent topology tracking, human-in-the-loop lifecycle, and OpenTelemetry parallel emission are all shipped.

Postgres is the primary storage backend in production. SQLite is supported for single-node self-hosting via the `MESEDI_DB_URL` `file:` scheme. The store interface abstracts both behind the same call shape.

Framework adapters ship across both SDKs: LangChain, LangGraph, OpenAI Agents SDK, and CrewAI in Python; LangGraph, OpenAI Agents SDK, and the Vercel AI SDK in TypeScript.
