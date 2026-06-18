# Mesedi

**Guardians for autonomous AI agents.**

Mesedi observes agent executions in production and detects ~30 named failure classes — loops, runaway costs, prompt-injection attacks, validator failures, drift, silent tool failures, data leakage, context overflow, semantic loops, MCP tool drift, multi-agent cascading failures and coordination deadlocks, HITL timeouts, sandbox escapes, and more. We cluster related failures so the same problem doesn't page you a hundred times, and notify you out-of-band via webhook the first time a new failure class appears. Per-failure-class canonical fix descriptions (Tier 1 Playbooks) ship in the dashboard.

Mesedi observes out-of-band via a one-line SDK wrap. We never reach into your agent's process. The SDK includes an optional hard-halt feature: the customer configures local budgets (input tokens, output tokens, wall-clock time, or step count) and the SDK enforces them at safe boundaries. Operators can also trigger a halt remotely from the dashboard. Mesedi never decides to halt your agent on its own. A Mesedi outage cannot break your production agent.

## Repository layout

This is a monorepo. Each top-level directory is independently buildable; cross-directory changes (e.g. backend wire-format updates that ripple to both SDKs) land as single atomic commits.

```
backend/         Go HTTP service. Ingests telemetry, runs the
                 failure-class detectors, dedupes into failure_groups,
                 serves the dashboard, fires webhooks on first
                 occurrence of a new failure class. SQLite for
                 single-instance self-host; Postgres for production
                 multi-tenant.
sdk-python/      Python SDK shipped to PyPI as `mesedi`. @wrap +
                 @tool decorators, AsyncShipper, Anthropic auto-
                 instrument, hard-halt with budgets + SSE remote
                 channel, LangGraph and OpenAI Agents SDK
                 integrations. Optional `[langchain]` / `[crewai]`
                 extras for framework adapters.
sdk-typescript/  TypeScript / Node SDK shipped to npm as `mesedi`.
                 Feature-parity with Python: wrap()/tool(),
                 AsyncShipper, Anthropic patch, hard-halt + SSE.
                 `mesedi/integrations/vercel_ai` adapter for Vercel
                 AI SDK's generateText.
branding/        Logo and favicon assets.
```

The customer-facing marketing site, pricing, signup, and live dashboard live in a separate repository: [`mesedi-ai/mesedi-web`](https://github.com/mesedi-ai/mesedi-web). Hosted at https://mesedi.ai.

## Local quickstart

Prerequisites:

- Go 1.22+
- Python 3.9+
- Node 18+
- A local SQLite (bundled, no install)

Start the backend (Terminal 1):

```bash
cd backend
go run ./cmd/api
```

Run a sample Python agent (Terminal 2):

```bash
cd sdk-python
python3 -m pip install -e .
python3 sandbox/real_agent.py
```

Or run a sample TypeScript agent (Terminal 2):

```bash
cd sdk-typescript
npm install
npm run build
npm run test:sandbox
```

Open the dashboard:

```
http://localhost:8080/ui/
```

You should see executions, events, failure groups, the playbook surface per failure class, the webhook escalation page, and the API-key management page. Default bootstrap API key for local dev: `mesedi_sk_dev_local_only`.

## Status

All thirty-plus failure-class detectors are live across the Mesedi detector categories (lifecycle, cost, semantic, multi-agent, HITL, security, cross-tenant). Hard-halt with local budgets plus SSE remote channel, dashboard with collapse-by-class view, operator Halt button, webhook escalation on first-occurrence, framework adapters for LangChain / CrewAI / Vercel AI SDK / LangGraph / OpenAI Agents, OpenTelemetry parallel emission, and per-failure-class Tier 1 Playbooks.

The hosted Cloud version runs on Fly.io for the backend and Cloudflare Workers for the dashboard. Self-host instructions are in `backend/README.md`.

## Documentation

Each subdirectory has its own README:

- [`backend/README.md`](backend/README.md): backend runbook, configuration, schema versioning, rate limiting
- [`sdk-python/README.md`](sdk-python/README.md): Python SDK API, framework adapters, hard-halt
- [`sdk-typescript/README.md`](sdk-typescript/README.md): TypeScript SDK API, Vercel AI SDK adapter

## License

MIT. See [`LICENSE`](LICENSE). Mesedi is operated by Verdifax, LLC d/b/a Mesedi, a Delaware limited liability company.
