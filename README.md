# Mesedi

**Guardians for autonomous AI agents.**

Mesedi observes agent executions in production, detects when an agent is going wrong (loops, crashes, runaway costs, prompt-injection attacks, validator failures, drift, silent tool failures), clusters related failures so the same problem doesn't page you a hundred times, optionally halts a misbehaving execution before it burns more budget, and notifies you out-of-band via webhook the first time a new failure class appears.

The Mesedi v1 surface is "detect, cluster, optionally stop, escalate, drop-in adopt." Per-failure-class canonical fix descriptions (Tier 1 Playbooks) ship in the dashboard; auto-fix (Tier 3) is on the v2 roadmap.

## Repository layout

This is a monorepo. Each top-level directory is independently buildable; cross-directory changes (e.g. backend wire-format updates that ripple to both SDKs) land as single atomic commits.

```
backend/         — Go HTTP service. Ingests telemetry, runs the seven
                   failure-class detectors, dedupes into failure_groups,
                   serves the dashboard, fires webhooks on first
                   occurrence of a new failure class. SQLite for local
                   development; Postgres for production (deferred).
sdk-python/      — Python SDK shipped to PyPI as `mesedi`. @wrap +
                   @tool decorators, AsyncShipper, Anthropic auto-
                   instrument, hard-halt with budgets + SSE remote
                   channel. Optional `[langchain]` / `[crewai]`
                   extras for framework adapters.
sdk-typescript/  — TypeScript / Node SDK shipped to npm as `mesedi`.
                   Feature-parity with Python — wrap()/tool(),
                   AsyncShipper, Anthropic patch, hard-halt + SSE.
                   `mesedi/integrations/vercel_ai` adapter for Vercel
                   AI SDK's generateText.
synthetic-org/   — Five-industry dogfood substrate (financial-research,
                   support-triage, clinical-summary, contract-review,
                   incident-response). Runs hourly via launchd to keep
                   the dashboard continuously populated during dev.
docs/            — Roadmap, repair-tier strategy, project registry,
                   pilot pitch, development checklist.
branding/        — Logo and favicon assets.
```

## Local quickstart

Prerequisites:

- Go 1.22+
- Python 3.9+
- Node 18+
- A local SQLite (bundled; no install)

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

For continuous synthetic-org traffic via launchd (populates the dashboard hourly):

```bash
bash synthetic-org/scripts/install_continuous_traffic.sh
# Stop: bash synthetic-org/scripts/stop_continuous_traffic.sh
```

## Status and posture

**Phase:** v1 in flight (Sunday, 2026-05-17). All seven failure-class detectors shipped, failure-group deduplication, hard-halt with local budgets plus SSE remote channel, dashboard with collapse-by-class view, continuous synthetic-org traffic, operator Halt button, webhook escalation on first-occurrence, framework adapters for LangChain / CrewAI / Vercel AI SDK, Tier 1 Playbooks v1 covering every detector-signature shape.

**Remaining v1 work:** docs and quickstart polish, production deployment (Fly.io + Postgres + KMS), PyPI / npm publication.

**Repository posture:** Private during local-dev. SDK repositories will go public when they ship to PyPI / npm. Backend service stays private indefinitely.

## Key documents

- [`docs/REPAIR_TIER_ROADMAP.md`](docs/REPAIR_TIER_ROADMAP.md) — Tier 1 (Recommendation) → Tier 2 (Suggested diff) → Tier 3 (Auto-fix) → Tier 4 (Closed loop) and what ships when
- [`docs/PROJECT_REGISTRY.md`](docs/PROJECT_REGISTRY.md) — separation of concerns between Mesedi and Verdifax, what lives in each
- [`docs/PILOT_PITCH.md`](docs/PILOT_PITCH.md) — first-customer outreach narrative
- [`docs/DEVELOPMENT_CHECKLIST.md`](docs/DEVELOPMENT_CHECKLIST.md) — discipline and slicing principles used across the codebase
- [`docs/V2_DEFERRAL_NOTES.md`](docs/V2_DEFERRAL_NOTES.md) — what's intentionally not in v1
- [`backend/README.md`](backend/README.md) — backend-specific runbook
- [`sdk-python/README.md`](sdk-python/README.md) — Python SDK API and framework adapters
- [`sdk-typescript/README.md`](sdk-typescript/README.md) — TypeScript SDK API and Vercel AI SDK adapter

## Brand

Mesedi is operated by **Canary Systems, LLC** — a holding company anchored around the canary-in-a-coal-mine metaphor (early-warning systems for high-stakes failure modes). See `docs/canary systems/` for the holding-company brand assets and posture.

## License

Proprietary. All rights reserved. Public license terms for the SDK repositories will be announced when those repositories ship to their respective package registries.
