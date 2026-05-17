# Mesedi synthetic-org

Multi-industry agent simulation that produces realistic Mesedi telemetry on demand. The point of this directory is to **replace the "need a real customer to validate" cold-start problem with a self-generated load source** that exercises every Mesedi detector, dashboard surface, and SDK code path across diverse industries.

If Mesedi is "the eyes," synthetic-org is "the agents we watch."

## Why this exists

Every Mesedi feature shipped so far has been validated against `slow_tool()` sandboxes — toys that don't behave like real agents. The synthetic-org gives Mesedi:

- Continuous, varied, realistic telemetry to dogfood every dashboard surface
- Diverse failure modes (real model variance + brittle tools + adversarial inputs) so the 7 detector classes have something genuine to detect
- Cross-industry coverage so the failure-group taxonomy gets exercised broadly, not narrowly
- A demo asset that beats every competitor's localhost-screenshot pitch: *"here is Mesedi instrumented against a 5-industry synthetic enterprise running continuously"*
- A foundation for future framework-adapter validation (LangChain, CrewAI, Vercel AI SDK) — once each industry's agent is re-implemented on a different framework, Phase 12's adapters get tested *in the process* instead of in a separate sprint

This is not a separate product. It is Mesedi's R&D substrate. If it ever proves valuable enough to spin out as its own product, that's a future-quarter conversation.

## The five industries

Each agent was chosen for a distinct combination of (a) realistic enterprise workflow shape and (b) Mesedi detector it naturally exercises hardest. Together they cover every detector class.

| Agent | Industry | Workflow simulated | Detector(s) it stresses |
|---|---|---|---|
| `support_triager` | SaaS customer support | Ticket arrives → classify → CRM lookup → KB search → draft response → quality check → escalate or send | tool_failures (CRM/KB occasionally fail), validator_failures (response-quality check), prompt_injection (tickets contain attacks) |
| `clinical_summarizer` | Healthcare / hospital chart review | Clinical note arrives → extract vitals → drug-interaction check → patient-history lookup → generate summary → schema validation | validator_failures (schema + no-PHI checks), prompt_injection (free-text notes), cost_velocity (long notes) |
| `financial_research` | Buy-side equity research | Query arrives → fetch 10-K → fetch price → segment analysis → consensus cross-check → thesis generation | cost_velocity (long filings → big token bills), step_count (deep analysis), drift (markets shift) |
| `contract_reviewer` | Legal / contract redline | Clause arrives → classify type → playbook lookup → precedent fetch → propose redlines | identical_call loops (re-reading same clause), step_count, validator_failures (redline schema) |
| `incident_responder` | DevOps / SRE on-call | Alert fires → fetch logs → check recent deploys → diagnose → suggest mitigation → execute or page human | wall_clock halts (SLA pressure), tool_failures (k8s/AWS APIs flake), crashes |

## Failure-mode coverage matrix

| Mesedi detector class | Exercised by |
|---|---|
| `crashes` | All five (unhandled exceptions in tool flake or LLM call) |
| `loops:time_budget` | `financial_research`, `incident_responder` |
| `loops:step_count` | `financial_research`, `contract_reviewer`, `support_triager` |
| `loops:identical_call` | `contract_reviewer` (re-reads same clause across multiple drafts) |
| `tool_failures` | `support_triager` (CRM/KB), `clinical_summarizer` (drug-interaction API), `incident_responder` (k8s/AWS) |
| `validator_failures` | `support_triager` (response quality), `clinical_summarizer` (schema + no-PHI), `contract_reviewer` (redline schema) |
| `prompt_injection` | `support_triager` (customer ticket bodies), `clinical_summarizer` (note free-text), `incident_responder` (alert log lines) |
| `cost_velocity` | `financial_research` (10-K = ~150K tokens), `clinical_summarizer` (long notes), `contract_reviewer` (long contracts) |
| `drift` *(once Phase 7 ships)* | `financial_research` (market context evolves), `support_triager` (KB articles drift) |
| `halt:wall_clock` | `incident_responder` (20s SLA budget) |
| `halt:step_count` | Tunable per-agent via `runner.py --budget-steps` |

Failure modes are **not deliberately planted**. The agents are intentionally **under-engineered** — short timeouts, narrow prompts, no retry logic, brittle tools — so real model variance and tool variance produce real failures. The synthetic inputs include realistic-rate adversarial cases (a small fraction of tickets contain prompt injection, a small fraction of clinical notes have conflicting info, etc.). Planting failures would be circular validation; this approach is honest.

## Framework mix

For v1, all five agents are raw-Anthropic-SDK Python. This keeps the scaffold small and exercises the base Mesedi `instrument_anthropic()` path thoroughly before adding framework variety.

For v2 (Phase 12 adapter validation), the plan is:

- `clinical_summarizer` → re-implement on CrewAI (multi-agent: extractor + summarizer + validator)
- `financial_research` → re-implement on LangGraph (state machine for research workflow)
- `incident_responder` → re-implement in TypeScript (exercises the TS SDK + cross-language failure-group dedup)
- `support_triager` and `contract_reviewer` stay on raw Anthropic so we always have a "no framework" reference

## Running it

```bash
# install once
cd /Users/robertcanario/mesedi/synthetic-org
python3 -m pip install -r requirements.txt --break-system-packages

# make sure backend is running
cd ../backend && go run cmd/api/main.go &

# single agent, bounded iterations
cd ../synthetic-org
python3 runner.py --agent support_triager --iterations 5

# single agent, time-bounded
python3 runner.py --agent financial_research --duration 5m

# all agents in round-robin
python3 runner.py --agent all --iterations 20

# dry-run mode (no Anthropic API calls — uses mock LLM responses)
MESEDI_SYNTHETIC_ORG_DRY_RUN=1 python3 runner.py --agent all --iterations 5
```

`--agent all` rotates across the five agents so the dashboard fills with mixed-industry traffic, which is what makes the failure-group view interesting.

## Cost guardrails

Continuous agent simulation burns real Anthropic tokens. Three guardrails:

1. **`MESEDI_SYNTHETIC_ORG_DRY_RUN=1`** — replaces every LLM call with a deterministic mock response. Structure runs free. Use this for any test where the LLM output content doesn't matter (every test except "does the prompt actually work").
2. **Per-agent `Budget`** — every agent is wrapped with a default `Budget(max_wall_clock_seconds=..., max_steps=...)` so a runaway agent halts before it burns the API key. Defaults are tight; bump them on the CLI if needed.
3. **`--max-spend-usd`** — runner-level kill switch. Tracks `total_input_tokens` and `total_output_tokens` across all agents in a session, multiplies by the price table in `backend/internal/pricing`, and halts the loop when the threshold is hit.

Sensible starting cap for a fresh-day run: `--max-spend-usd 1.00`. That's $1 of tokens, ~30 minutes of mixed traffic, enough to populate every dashboard surface with rich data.

## Layout

```
synthetic-org/
├── README.md                    ← you are here
├── requirements.txt
├── runner.py                    ← CLI orchestrator
├── synthetic_inputs.py          ← input generators for each industry
├── agents/
│   ├── __init__.py
│   ├── support_triager.py       ← SaaS support (FULL reference implementation)
│   ├── clinical_summarizer.py   ← healthcare
│   ├── financial_research.py    ← finance / equities
│   ├── contract_reviewer.py     ← legal
│   └── incident_responder.py    ← devops / SRE
└── .gitignore
```

## Continuous traffic (capstone)

Once synthetic-org is exercising every detector reliably, the next operational step is making it run continuously without manual invocation so the dashboard stays always-populated and any Mesedi regression surfaces within an hour. See `CONTINUOUS_TRAFFIC.md` for the macOS launchd-based setup — one-time install of a plist that fires the runner hourly in dry-run mode, with rotating logs and zero API spend.

## Roadmap

- **v1 (today, this scaffold):** all 5 agents on raw Anthropic, runner.py orchestrator, dry-run mode, basic budgets.
- **v1.1:** real Anthropic integration tested end-to-end on the API key, first ~$5 burn run with the full dashboard populated.
- **v1.2:** session-level metrics summary printed by the runner at exit (executions, halts, total spend, dominant failure groups).
- **v2 (Phase 12 prep):** re-implement two agents on CrewAI + LangGraph, one in TypeScript, to drive framework-adapter validation.
- **v3 (post-LOI):** containerized, runnable as a Fly.io background process so synthetic-org traffic flows 24/7 against the production Mesedi backend.
- **v4 (speculative):** publish a stripped-down public version as a demo sandbox — every developer who clones it has now seen Mesedi instrumentation working. Marketing surface, not a product.

## What this is not

- **Not a benchmark.** No standardized inputs, no leaderboards, no scoring of model quality. The point is *load*, not evaluation.
- **Not a customer-facing demo yet.** The agents are under-engineered on purpose. A polished demo agent will come later, separately.
- **Not a substitute for real users.** When a real pilot user lands (see `docs/PILOT_PITCH.md`), their telemetry is still strictly better than this. Synthetic-org closes the gap during the cold-start window; it doesn't eliminate the need for real users forever.
