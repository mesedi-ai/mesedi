# v0.6.0 — Mastra adapter + accumulated provider, framework, and reliability work

First public TypeScript SDK release since **v0.2.0**. If you were on
v0.2.0, upgrading to v0.6.0 gives you everything below in one step.

## Framework adapters

- **Mastra.** `MesediExporter` for `@mastra/observability`. Wire it
  into your `Observability({ exporters: { mesedi: new
  MesediExporter() } })` and Mastra agent runs, workflow runs, LLM
  calls, tool calls, and workflow steps flow into Mesedi with no
  changes to your agent code. Docs:
  https://mesedi.ai/docs/integrations/mastra
- **Vercel AI SDK.** `mesedi/integrations/vercel_ai` for `generateText`.
- **LangGraph.** `MesediLangGraphHandler` +
  `instrumentLangGraph()` one-call setup. Node-level checkpoint
  events + sub-graph agent-handoff detection.
- **OpenAI Agents SDK.** `MesediRunHooks` implementing the
  `RunHooks` interface — checkpoints on agent enter/exit,
  agent-handoff events, and tool-call events.

## LLM provider auto-instrumentation

- **Anthropic, OpenAI, Cohere, Gemini, Ollama.** `instrument*()`
  functions patch each provider's client to emit `llm_call` events
  for sync, async, and streaming calls. Exceptions are mapped to
  Mesedi's canonical error-class vocabulary
  (`rate_limited`, `quota_exhausted`, `internal_error`,
  `service_unavailable`, `timeout`, `invalid_api_key`,
  `client_error`, `unknown`).
- **Vertex AI Gemini** surface covered separately for teams using
  the Vertex client instead of the direct Gemini SDK.
- `retry_after` header extraction on rate-limit responses so your
  own retry logic can honor the provider's throttling window.

## RAG grounding evaluators

Emit `eval_score` events with the shape Mesedi's `grounding_failure`
detector expects, from these three evaluators:

- **Ragas** (`mesedi/integrations/ragas`)
- **Promptfoo** (`mesedi/integrations/promptfoo`)
- **Vectara HHEM** (`mesedi/integrations/vectara`)

Per-evaluator threshold configuration on the backend so different
evaluators can use different sensitivities without cross-contamination.

## Human-in-the-loop

- `requestHumanIntervention()` / `waitForHumanDecision()` /
  `submitHumanDecision()` primitives for the full HITL request /
  response cycle.
- `human_intervention` event captures the ask + the decided answer
  when the human's decision lands.
- Execution lifecycle rework adds a `paused` state with paused-time
  accounting so long HITL waits don't inflate an execution's
  `duration_ms`.

## Multi-agent

- `emitAgentHandoff(from, to, kind, taskSummary)` for cross-agent
  task delegation.
- Ergonomic surface: `wrap({ agentName })` at the entry point and
  handoffs auto-fill `fromAgent`.

## Cost + reliability

- **gzip request compression** on every event batch above 1 KB.
- **Payload truncation** with a configurable per-event cap. Longest
  string fields are smart-truncated to fit, with marker fields so
  downstream readers know what happened.
- **Cost-per-tenant attribution**: pass `tenant_id` on `wrap()` and
  cost rolls up correctly on the dashboard's cost-by-tenant report.
- **Infrastructure event emission** for HTTP 429, circuit-breaker
  trips, and hard-quota exhaustion. Consumed by the
  `infrastructure_throttled` detector on the backend.

## Failure signature quality

- Granular signatures for `tool_failures` and `validator_failures`
  so recurring failures cluster more tightly.
- Structured `return_value` field on `tool_call` events with a
  schema-preserving coercion pass and a per-project size cap.

## Security

- Regex ReDoS + URL-substring fixes across the SDK's built-in
  security helpers (CodeQL sweep).

## Compatibility

- **Node 18+** minimum.
- Zero required runtime dependencies (as before). Optional peer
  dependencies for `@mastra/core`, `@mastra/observability`, and
  `ai` (Vercel AI SDK).
- Existing v0.2.0 `wrap()` / `tool()` call sites keep working
  without modification.
