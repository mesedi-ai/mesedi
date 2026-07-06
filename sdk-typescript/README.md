# Mesedi TypeScript SDK

**Status:** v0.4.0. Live on npm.

The TypeScript companion to `sdk-python/`. Feature parity for the v1
surface (`configure()`, `wrap()`, `tool()`, async event shipper,
fail-open posture), built on Node 18+ native `fetch`,
`AsyncLocalStorage`, and `node:zlib` for opt-in gzip compression of
request bodies above 1 KB. **Zero runtime dependencies.** Compresses
or not transparently; small calls ship uncompressed as before.

## Install

```bash
npm install mesedi
```

## API

```typescript
import { configure, wrap, tool, flush } from "mesedi";

configure({
  apiKey: "mesedi_sk_...",
  baseUrl: "http://localhost:8080",
});

// Define a tool. Observed when called from inside a wrap()'d function.
const searchWeb = tool({ name: "search_web" }, async (q: string) => {
  return ["result1", "result2"];
});

// Wrap an agent function. Records start/complete/crash automatically.
const runAgent = wrap(async (query: string) => {
  const results = await searchWeb(query);
  return `found ${results.length} results`;
});

await runAgent("pickleball");

// At end-of-script, flush any in-flight events:
await flush();
```

## What lands at the backend

For each `wrap()`-decorated call:

- **On entry:** `POST /executions` (status=started, sdk_language=typescript,
  sdk_version=0.4.0).
- **On normal return:** `PATCH /executions/{id}` (status=completed,
  duration_ms, ended_at).
- **On thrown error:** `PATCH /executions/{id}` (status=crashed,
  crash_signature). The original error is then re-thrown with its
  original stack.

For each `tool()`-decorated call (from inside a `wrap()`):

- `POST /events` with event_type=tool_call, sequence number from the
  surrounding execution's context, payload includes tool_name +
  sanitized args + status + result_summary (or exception fields).

All HTTP is async via a single in-process queue + a `setInterval`
drainer. Network failures during observation NEVER throw back into the
wrapped agent. The SDK is fail-open: a Mesedi outage degrades to
invisibility, not to broken production code.

## Optional: hard-halt with local budgets

Cap a single execution across four axes — input tokens, output tokens,
wall-clock seconds, and step count. Pass any subset; unset fields impose
no limit on that axis. When any budget is exceeded, the SDK throws
`MesediHalt` at the next safe boundary (between LLM calls, tool calls,
or explicit `checkpoint()`s), never mid-call, so `try`/`finally` cleanup
runs and open resources release.

```typescript
import { wrap } from "mesedi";

export const myAgent = wrap(
  {
    budget: {
      maxWallClockSeconds: 600,   // 10 min real time
      maxSteps: 30,                // 30 tool/LLM/checkpoint boundaries
      maxTokensIn: 200_000,
      maxTokensOut: 50_000,
    },
  },
  async (query: string) => { /* ... */ },
);
```

When a budget is supplied, the SDK also opens an SSE subscription to
`GET /executions/{id}/halt-stream`. Operators can halt a running
execution from the dashboard. If the SSE connection fails (backend
unreachable, 4xx/5xx, network partition), the reader logs and returns
— the wrapped agent keeps running with local budgets still enforced
client-side. Mesedi never decides to halt on its own; operator intent
or your own budget rules are the only triggers. `MesediHalt` carries an
internal Symbol marker so `wrap()` detects it even if user code
accidentally re-wraps it via `throw new Error(...)`.

## Framework integrations

Adapter modules under `mesedi/integrations/*` translate each framework's
native callback or hook surface into Mesedi telemetry. They're optional;
importing `mesedi` itself never requires any framework to be installed.

Currently shipping: **LangChain.js**, **LangGraph**, **OpenAI Agents
SDK**, **Vercel AI SDK**, and **Mastra**. Each peer dependency is
opt-in.

### LangGraph

```typescript
import { configure, wrap } from "mesedi";
import { instrumentGraph } from "mesedi/integrations/langgraph";

configure({ apiKey: process.env.MESEDI_API_KEY! });

export const runMyGraph = wrap(async (question: string) => {
  const graph = buildGraph();
  instrumentGraph(graph);
  const result = await graph.invoke({ input: question });
  return result.output;
});
```

`instrumentGraph` attaches Mesedi telemetry to each node in the graph
and emits `llm_call` and `tool_call` events labeled with the node name,
so the dashboard timeline shows the graph's flow alongside per-step
detail.

### OpenAI Agents SDK

```typescript
import { configure, wrap } from "mesedi";
import { instrumentAgent } from "mesedi/integrations/openai_agents";

configure({ apiKey: process.env.MESEDI_API_KEY! });

export const runMyAgent = wrap(async (question: string) => {
  const agent = buildAgent();
  instrumentAgent(agent);
  return agent.run(question);
});
```

`instrumentAgent` subscribes to the OpenAI Agents SDK's lifecycle hooks
and emits `llm_call` + `tool_call` events with the same wire format as
the LangGraph and Vercel AI SDK adapters, so detectors see no
difference.

### Vercel AI SDK

If your agent uses Vercel's `ai` package (`generateText`, multi-step
ReAct with `tools` + `maxSteps`), you don't have to wrap every tool by
hand. `wrapGenerateText` is a one-line higher-order function that
returns a drop-in replacement for `generateText` with Mesedi telemetry
side effects.

```typescript
import { configure, wrap, flush } from "mesedi";
import { wrapGenerateText } from "mesedi/integrations/vercel_ai";
import { generateText } from "ai";
import { openai } from "@ai-sdk/openai";

configure({ apiKey: process.env.MESEDI_API_KEY! });

const generateTextM = wrapGenerateText(generateText);

export const runAgent = wrap(
  { name: "support-triage" },
  async (question: string) => {
    const result = await generateTextM({
      model: openai("gpt-4o"),
      prompt: question,
      tools: { lookup, search },
      maxSteps: 5,
    });
    return result.text;
  },
);
```

Per invocation, the wrapper emits:

- One `llm_call` event per step (Vercel's multi-step ReAct surfaces
  intermediate reasoning + final answer on `result.steps`). Model id,
  user message, system prompt, token usage, response text, all
  captured in the standard Mesedi wire format.
- One `tool_call` event per tool invocation in each step. Pairs
  `result.toolCalls[i]` to `result.toolResults` by `toolCallId`,
  detects failure mode (missing result OR `result.error` field) and
  records `status=failed` with `exception_type` / `exception_message`.

Detectors (drift, identical/similar-call loops, tool-failures,
cost-velocity, prompt-injection) see the same wire format as a
hand-written `mesedi` instrumentation produces.

`ai` is declared as an **optional peer dependency**. Installing
`mesedi` doesn't require it. If your project already has `ai`
installed for `generateText`, the integration just works.

Only `generateText` is wrapped today. `streamText` and `generateObject`
are not currently supported; use hand-instrumentation with
`emitLLMCall` / `tool()` for those code paths.

## Releases

This SDK is published to npm via OIDC Trusted Publishing from the
`release-sdk-typescript.yml` GitHub Actions workflow, with no long-lived
NPM_TOKEN secret. Every release carries an npm provenance attestation
linking it to a specific commit in `mesedi-ai/mesedi`.

To cut a new release, bump `version` in `package.json`, commit, then:

```bash
git tag -a sdk-typescript-v0.X.Y -m "Release sdk-typescript v0.X.Y"
git push origin sdk-typescript-v0.X.Y
```

The workflow installs, builds, and publishes with `--provenance`.
