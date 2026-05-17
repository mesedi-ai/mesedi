# Mesedi TypeScript SDK

**Status:** Phase 11 alpha (v0.0.1). Local-only development; not yet on npm.

The TypeScript companion to `sdk-python/`. Feature parity for the v1
surface — `configure()`, `wrap()`, `tool()`, async event shipper,
fail-open posture — built on Node 18+ native `fetch` and
`AsyncLocalStorage`. **Zero runtime dependencies.**

## Quickstart (local development)

Prerequisites: Mesedi backend running on `localhost:8080`. See
`../backend/README.md` if not yet running.

```bash
cd ~/mesedi/sdk-typescript
npm install              # installs only devDeps (TypeScript)
npm run build            # compile src/ → dist/
node sandbox/real_agent.js
```

Or run the sandbox in one shot:

```bash
npm run test:sandbox
```

## API

```typescript
import { configure, wrap, tool, flush } from "mesedi";

configure({
  apiKey: "mesedi_sk_...",
  baseUrl: "http://localhost:8080",
});

// Define a tool — observed when called from inside a wrap()'d function
const searchWeb = tool({ name: "search_web" }, async (q: string) => {
  return ["result1", "result2"];
});

// Wrap an agent function — records start/complete/crash automatically
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
  sdk_version=0.0.1).
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
wrapped agent — the SDK is fail-open: a Mesedi outage degrades to
invisibility, not to broken production code.

## Framework integrations

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
  user message, system prompt, token usage, response text — all
  captured in the standard Mesedi wire format.
- One `tool_call` event per tool invocation in each step. Pairs
  `result.toolCalls[i]` to `result.toolResults` by `toolCallId`,
  detects failure mode (missing result OR `result.error` field) and
  records `status=failed` with `exception_type` / `exception_message`.

Detectors — drift, identical / similar-call loops, tool-failures,
cost-velocity, prompt-injection — see the same wire format as a
hand-written `mesedi` instrumentation produces.

`ai` is declared as an **optional peer dependency** — installing
`mesedi` doesn't require it. If your project already has `ai`
installed for `generateText`, the integration just works.

`streamText` and `generateObject` ship in a later slice.

## Posture

Same local-only posture as `sdk-python/`. npm publication via the
trusted-publishing flow ships post-LOI alongside the SDK ecosystem
work in Phase 16.
