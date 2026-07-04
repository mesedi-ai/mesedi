/**
 * Unit tests for the Mastra exporter (Wave 4.B).
 *
 * Strategy: mock the MesediClient module so the exporter's
 * getClient() call returns a stub that captures every
 * submitExecutionStart / submitExecutionEnd / submitEvent call. Feed
 * synthetic TracingEvent objects into the exporter and assert the
 * expected Mesedi state emerges.
 *
 * Integration coverage (real Mastra agent → real Mesedi backend) is
 * out of scope for this wave; that ships in the synthetic-customer
 * repo as a follow-up.
 */

import { beforeEach, describe, expect, test, vi } from "vitest";

import { Event, Execution } from "../events.js";

// ── Stub client ──────────────────────────────────────────────────────
//
// vi.mock hoists to the top of the file, so the captures object has
// to be created inside vi.hoisted() to share the same hoisting phase.
// The exporter's getClient() call then hits the stub, which appends
// to these shared arrays.

type Captures = {
  starts: Execution[];
  ends: Execution[];
  events: Event[];
};

const captures = vi.hoisted<Captures>(() => ({
  starts: [],
  ends: [],
  events: [],
}));

vi.mock("../client.js", () => ({
  getClient: () => ({
    submitExecutionStart: (e: Execution) => captures.starts.push(e),
    submitExecutionEnd: (e: Execution) => captures.ends.push(e),
    submitEvent: (ev: Event) => captures.events.push(ev),
  }),
}));

// Import AFTER the mock so the exporter picks up the stubbed client.
import { MesediExporter } from "./mastra.js";

function getCaps(): Captures {
  return captures;
}

function resetCaps(): void {
  const caps = getCaps();
  caps.starts.length = 0;
  caps.ends.length = 0;
  caps.events.length = 0;
}

// ── Fixture builders ─────────────────────────────────────────────────
//
// TracingEvent + AnyExportedSpan are structural. The exporter reads
// only a handful of fields off each span, so we construct minimal
// literals and cast through `unknown` to satisfy TypeScript at the
// test boundary. This mirrors what the runtime shape looks like when
// Mastra fires an actual event.

function makeSpan(overrides: Record<string, unknown>): unknown {
  return {
    traceId: "trace-1",
    spanId: "span-1",
    parentSpanId: undefined,
    attributes: {},
    ...overrides,
  };
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function startedEvt(span: unknown): any {
  return { type: "span_started", exportedSpan: span };
}
// eslint-disable-next-line @typescript-eslint/no-explicit-any
function endedEvt(span: unknown): any {
  return { type: "span_ended", exportedSpan: span };
}

beforeEach(() => {
  resetCaps();
});

// ──────────────────────────────────────────────────────────────────────
// Root-span lifecycle (AGENT_RUN)
// ──────────────────────────────────────────────────────────────────────

describe("MesediExporter — root span lifecycle", () => {
  test("AGENT_RUN start opens execution + emits enter checkpoint", async () => {
    const exporter = new MesediExporter();
    const span = makeSpan({
      type: "agent_run",
      name: "planner",
      attributes: { prompt: "hello" },
    });

    await exporter.exportTracingEvent(startedEvt(span));

    const caps = getCaps();
    expect(caps.starts).toHaveLength(1);
    expect(caps.starts[0]?.status).toBe("started");
    expect(caps.starts[0]?.sdk_language).toBe("typescript");
    expect(caps.events).toHaveLength(1);
    expect(caps.events[0]?.event_type).toBe("checkpoint");
    expect(caps.events[0]?.payload["agent_event"]).toBe("enter");
    expect(caps.events[0]?.payload["agent"]).toBe("planner");
    expect(caps.events[0]?.sequence).toBe(1);
  });

  test("AGENT_RUN end closes execution + emits exit checkpoint", async () => {
    const exporter = new MesediExporter();
    const span = makeSpan({
      type: "agent_run",
      name: "planner",
      attributes: { output: "done" },
    });

    await exporter.exportTracingEvent(startedEvt(span));
    resetCaps();
    await exporter.exportTracingEvent(endedEvt(span));

    const caps = getCaps();
    expect(caps.events).toHaveLength(1);
    expect(caps.events[0]?.event_type).toBe("checkpoint");
    expect(caps.events[0]?.payload["agent_event"]).toBe("exit");
    expect(caps.ends).toHaveLength(1);
    expect(caps.ends[0]?.status).toBe("completed");
    expect(caps.ends[0]?.duration_ms).toBeGreaterThanOrEqual(0);
  });

  test("WORKFLOW_RUN behaves like AGENT_RUN as a root span", async () => {
    const exporter = new MesediExporter();
    const span = makeSpan({
      type: "workflow_run",
      name: "ingest",
      attributes: {},
    });

    await exporter.exportTracingEvent(startedEvt(span));
    await exporter.exportTracingEvent(endedEvt(span));

    const caps = getCaps();
    expect(caps.starts).toHaveLength(1);
    expect(caps.ends).toHaveLength(1);
  });

  test("non-root child span does NOT open a new execution", async () => {
    const exporter = new MesediExporter();
    const child = makeSpan({
      type: "agent_run",
      parentSpanId: "parent-span-id",
    });

    await exporter.exportTracingEvent(startedEvt(child));

    expect(getCaps().starts).toHaveLength(0);
  });

  test("duplicate SPAN_STARTED on the same trace is idempotent", async () => {
    const exporter = new MesediExporter();
    const span = makeSpan({ type: "agent_run" });

    await exporter.exportTracingEvent(startedEvt(span));
    await exporter.exportTracingEvent(startedEvt(span));

    expect(getCaps().starts).toHaveLength(1);
  });
});

// ──────────────────────────────────────────────────────────────────────
// Descendant span mapping
// ──────────────────────────────────────────────────────────────────────

describe("MesediExporter — descendant spans", () => {
  test("MODEL_GENERATION emits llm_call with tokens", async () => {
    const exporter = new MesediExporter();
    await exporter.exportTracingEvent(
      startedEvt(makeSpan({ type: "agent_run" })),
    );
    resetCaps();

    const modelSpan = makeSpan({
      type: "model_generation",
      attributes: {
        model: "claude-haiku-4-5",
        provider: "anthropic",
        prompt: "hi",
        output: "hello",
        internalUsage: { inputTokens: 20, outputTokens: 5 },
      },
    });
    await exporter.exportTracingEvent(endedEvt(modelSpan));

    const caps = getCaps();
    expect(caps.events).toHaveLength(1);
    const evt = caps.events[0]!;
    expect(evt.event_type).toBe("llm_call");
    expect(evt.payload["model"]).toBe("claude-haiku-4-5");
    expect(evt.payload["provider"]).toBe("anthropic");
    expect(evt.payload["input_tokens"]).toBe(20);
    expect(evt.payload["output_tokens"]).toBe(5);
    // SW#280-3.f: status field distinguishes ok from failed.
    expect(evt.payload["status"]).toBe("ok");
  });

  // ──────────────────────────────────────────────────────────────────
  // SW#280-3.g: Mastra 1.x MODEL_GENERATION shape extraction
  // ──────────────────────────────────────────────────────────────────
  //
  // Live probe evidence (SW#280-3.g investigation):
  //   input = { messages: [
  //     {role:"system", content:"..."},
  //     {role:"user", content:[{type:"text", text:"actual prompt",
  //                             providerOptions:{mastra:{createdAt:...}}}]}
  //   ]}
  //   output = { files, reasoning, sources, text:"response", warnings }
  //   attributes.usage = {inputTokens, outputTokens, ...}
  //   attributes.internalUsage = undefined
  //
  // Extractors must unwrap these so identical_call / drift lexical /
  // token_waste can score user_message + response_text on the
  // human-visible content, not the per-call timestamp-poisoned
  // envelope. The `usage` (not `internalUsage`) field carries
  // per-call tokens for cost_velocity.

  test("SW#280-3.g: Mastra input.messages array → user_message extracts last user's text", async () => {
    const exporter = new MesediExporter();
    await exporter.exportTracingEvent(
      startedEvt(makeSpan({ type: "agent_run" })),
    );
    resetCaps();

    const modelSpan = makeSpan({
      type: "model_generation",
      attributes: {
        model: "claude-haiku-4-5",
        provider: "anthropic",
        usage: { inputTokens: 42, outputTokens: 7 },
      },
      input: {
        messages: [
          { role: "system", content: "Reply in one word." },
          {
            role: "user",
            content: [
              {
                type: "text",
                text: "Say hello in one word.",
                providerOptions: { mastra: { createdAt: 1783184625090 } },
              },
            ],
          },
        ],
      },
      output: {
        files: [],
        reasoning: [],
        sources: [],
        text: "Hello.",
        warnings: [],
      },
    });
    await exporter.exportTracingEvent(endedEvt(modelSpan));

    const evt = getCaps().events[0]!;
    expect(evt.payload["user_message"]).toBe("Say hello in one word.");
    expect(evt.payload["response_text"]).toBe("Hello.");
    // usage (not internalUsage) is the correct per-call token source.
    expect(evt.payload["input_tokens"]).toBe(42);
    expect(evt.payload["output_tokens"]).toBe(7);
  });

  test("SW#280-3.g: string-content user message extracts as-is (no wrapping)", async () => {
    const exporter = new MesediExporter();
    await exporter.exportTracingEvent(
      startedEvt(makeSpan({ type: "agent_run" })),
    );
    resetCaps();

    const modelSpan = makeSpan({
      type: "model_generation",
      attributes: { model: "gpt-4o", provider: "openai", usage: {} },
      input: {
        messages: [
          { role: "system", content: "You are helpful." },
          { role: "user", content: "What is 2+2?" },
        ],
      },
      output: { text: "4" },
    });
    await exporter.exportTracingEvent(endedEvt(modelSpan));

    const evt = getCaps().events[0]!;
    expect(evt.payload["user_message"]).toBe("What is 2+2?");
    expect(evt.payload["response_text"]).toBe("4");
  });

  test("SW#280-3.g: multiple user turns → picks the LAST user message", async () => {
    const exporter = new MesediExporter();
    await exporter.exportTracingEvent(
      startedEvt(makeSpan({ type: "agent_run" })),
    );
    resetCaps();

    const modelSpan = makeSpan({
      type: "model_generation",
      attributes: { model: "gpt-4o", provider: "openai" },
      input: {
        messages: [
          { role: "user", content: "First question" },
          { role: "assistant", content: "First answer" },
          { role: "user", content: "Second question" },
        ],
      },
      output: { text: "Second answer" },
    });
    await exporter.exportTracingEvent(endedEvt(modelSpan));

    const evt = getCaps().events[0]!;
    expect(evt.payload["user_message"]).toBe("Second question");
  });

  test("SW#280-3.g: multi-part text content joined with spaces", async () => {
    const exporter = new MesediExporter();
    await exporter.exportTracingEvent(
      startedEvt(makeSpan({ type: "agent_run" })),
    );
    resetCaps();

    const modelSpan = makeSpan({
      type: "model_generation",
      attributes: { model: "gpt-4o", provider: "openai" },
      input: {
        messages: [
          {
            role: "user",
            content: [
              { type: "text", text: "Describe" },
              { type: "image_url", image_url: "..." },
              { type: "text", text: "this image" },
            ],
          },
        ],
      },
      output: { text: "cat" },
    });
    await exporter.exportTracingEvent(endedEvt(modelSpan));

    const evt = getCaps().events[0]!;
    expect(evt.payload["user_message"]).toBe("Describe this image");
  });

  test("SW#280-3.g: non-Mastra shape (raw string input) still works", async () => {
    const exporter = new MesediExporter();
    await exporter.exportTracingEvent(
      startedEvt(makeSpan({ type: "agent_run" })),
    );
    resetCaps();

    const modelSpan = makeSpan({
      type: "model_generation",
      attributes: { model: "gpt-4o", provider: "openai", prompt: "hi", output: "hey" },
    });
    await exporter.exportTracingEvent(endedEvt(modelSpan));

    const evt = getCaps().events[0]!;
    // Pre-Mastra-1.x shape still recognized via attributes.prompt +
    // attributes.output — the extractor falls back cleanly.
    expect(evt.payload["user_message"]).toBe("hi");
    expect(evt.payload["response_text"]).toBe("hey");
  });

  test("SW#280-3.g: internalUsage still consulted when usage absent (ancestor-rollup case)", async () => {
    const exporter = new MesediExporter();
    await exporter.exportTracingEvent(
      startedEvt(makeSpan({ type: "agent_run" })),
    );
    resetCaps();

    const modelSpan = makeSpan({
      type: "model_generation",
      attributes: {
        model: "claude-haiku-4-5",
        provider: "anthropic",
        internalUsage: { inputTokens: 100, outputTokens: 20 },
      },
    });
    await exporter.exportTracingEvent(endedEvt(modelSpan));

    const evt = getCaps().events[0]!;
    expect(evt.payload["input_tokens"]).toBe(100);
    expect(evt.payload["output_tokens"]).toBe(20);
  });

  // ──────────────────────────────────────────────────────────────────
  // SW#280-3.f: failed MODEL_GENERATION → llm_call with error_class
  // ──────────────────────────────────────────────────────────────────
  //
  // Mastra 1.x populates SpanErrorInfo (name + message + details) on
  // failed spans. The MesediExporter reads it and emits the canonical
  // failure-path fields that feed the provider_incident detector's
  // cross-tenant (provider, error_class) clustering, matching what
  // SW#280-3.a shipped for LangChain.

  test("SW#280-3.f: failed MODEL_GENERATION with Anthropic RateLimitError → error_class=rate_limited", async () => {
    const exporter = new MesediExporter();
    await exporter.exportTracingEvent(
      startedEvt(makeSpan({ type: "agent_run" })),
    );
    resetCaps();

    const modelSpan = makeSpan({
      type: "model_generation",
      attributes: {
        model: "claude-haiku-4-5",
        provider: "anthropic",
        prompt: "hi",
      },
      errorInfo: {
        name: "RateLimitError",
        message: "429 Too Many Requests",
        details: { status: 429, retryAfter: 30 },
      },
    });
    await exporter.exportTracingEvent(endedEvt(modelSpan));

    const evt = getCaps().events[0]!;
    expect(evt.event_type).toBe("llm_call");
    expect(evt.payload["status"]).toBe("failed");
    expect(evt.payload["provider"]).toBe("anthropic");
    expect(evt.payload["error_class"]).toBe("rate_limited");
    expect(evt.payload["exception_type"]).toBe("RateLimitError");
    expect(evt.payload["exception_message"]).toBe("429 Too Many Requests");
    expect(evt.payload["http_status"]).toBe(429);
    expect(evt.payload["retry_after"]).toBe(30);
    // Failed-path drops token counts by design.
    expect(evt.payload["input_tokens"]).toBeUndefined();
    expect(evt.payload["output_tokens"]).toBeUndefined();
  });

  test("SW#280-3.f: OpenAI APITimeoutError → error_class=timeout", async () => {
    const exporter = new MesediExporter();
    await exporter.exportTracingEvent(
      startedEvt(makeSpan({ type: "agent_run" })),
    );
    resetCaps();

    const modelSpan = makeSpan({
      type: "model_generation",
      attributes: { model: "gpt-4o", provider: "openai" },
      errorInfo: { name: "APITimeoutError", message: "request timed out" },
    });
    await exporter.exportTracingEvent(endedEvt(modelSpan));

    const evt = getCaps().events[0]!;
    expect(evt.payload["error_class"]).toBe("timeout");
    expect(evt.payload["provider"]).toBe("openai");
  });

  test("SW#280-3.f: unknown provider with known exception name still classifies", async () => {
    const exporter = new MesediExporter();
    await exporter.exportTracingEvent(
      startedEvt(makeSpan({ type: "agent_run" })),
    );
    resetCaps();

    const modelSpan = makeSpan({
      type: "model_generation",
      attributes: { model: "custom-model" },
      errorInfo: { name: "RateLimitError", message: "throttled" },
    });
    await exporter.exportTracingEvent(endedEvt(modelSpan));

    const evt = getCaps().events[0]!;
    expect(evt.payload["provider"]).toBe("unknown");
    expect(evt.payload["error_class"]).toBe("rate_limited");
  });

  test("SW#280-3.f: unrecognized exception name → error_class=unknown", async () => {
    const exporter = new MesediExporter();
    await exporter.exportTracingEvent(
      startedEvt(makeSpan({ type: "agent_run" })),
    );
    resetCaps();

    const modelSpan = makeSpan({
      type: "model_generation",
      attributes: { model: "gpt-4o", provider: "openai" },
      errorInfo: { name: "TotallyMadeUpError", message: "weird" },
    });
    await exporter.exportTracingEvent(endedEvt(modelSpan));

    const evt = getCaps().events[0]!;
    expect(evt.payload["error_class"]).toBe("unknown");
    expect(evt.payload["exception_type"]).toBe("TotallyMadeUpError");
  });

  test("SW#280-3.f: errorInfo with only message (no name) still marked failed", async () => {
    const exporter = new MesediExporter();
    await exporter.exportTracingEvent(
      startedEvt(makeSpan({ type: "agent_run" })),
    );
    resetCaps();

    const modelSpan = makeSpan({
      type: "model_generation",
      attributes: { model: "claude-haiku-4-5", provider: "anthropic" },
      errorInfo: { message: "opaque error, no name field" },
    });
    await exporter.exportTracingEvent(endedEvt(modelSpan));

    const evt = getCaps().events[0]!;
    expect(evt.payload["status"]).toBe("failed");
    // Without a class name, we can't classify — falls to UNKNOWN.
    expect(evt.payload["error_class"]).toBe("unknown");
    expect(evt.payload["exception_type"]).toBeUndefined();
    expect(evt.payload["exception_message"]).toBe("opaque error, no name field");
  });

  test("SW#280-3.f: http_status reads statusCode variant too", async () => {
    const exporter = new MesediExporter();
    await exporter.exportTracingEvent(
      startedEvt(makeSpan({ type: "agent_run" })),
    );
    resetCaps();

    const modelSpan = makeSpan({
      type: "model_generation",
      attributes: { model: "claude-haiku-4-5", provider: "anthropic" },
      errorInfo: {
        name: "InternalServerError",
        message: "500",
        details: { statusCode: 500 },
      },
    });
    await exporter.exportTracingEvent(endedEvt(modelSpan));

    const evt = getCaps().events[0]!;
    expect(evt.payload["http_status"]).toBe(500);
    expect(evt.payload["error_class"]).toBe("internal_error");
  });

  test("TOOL_CALL emits tool_call event", async () => {
    const exporter = new MesediExporter();
    await exporter.exportTracingEvent(
      startedEvt(makeSpan({ type: "agent_run" })),
    );
    resetCaps();

    const toolSpan = makeSpan({
      type: "tool_call",
      attributes: {
        name: "search",
        input: { q: "cats" },
        output: "found 42",
      },
    });
    await exporter.exportTracingEvent(endedEvt(toolSpan));

    const caps = getCaps();
    expect(caps.events).toHaveLength(1);
    const evt = caps.events[0]!;
    expect(evt.event_type).toBe("tool_call");
    expect(evt.payload["tool_name"]).toBe("search");
    expect(evt.payload["status"]).toBe("ok");
  });

  test("MCP_TOOL_CALL emits mcp_call event type", async () => {
    const exporter = new MesediExporter();
    await exporter.exportTracingEvent(
      startedEvt(makeSpan({ type: "agent_run" })),
    );
    resetCaps();

    const mcpSpan = makeSpan({
      type: "mcp_tool_call",
      attributes: { name: "vector-store.query", input: {}, output: [] },
    });
    await exporter.exportTracingEvent(endedEvt(mcpSpan));

    const caps = getCaps();
    expect(caps.events).toHaveLength(1);
    expect(caps.events[0]?.event_type).toBe("mcp_call");
  });

  test("WORKFLOW_STEP emits checkpoint event", async () => {
    const exporter = new MesediExporter();
    await exporter.exportTracingEvent(
      startedEvt(makeSpan({ type: "workflow_run" })),
    );
    resetCaps();

    const stepSpan = makeSpan({
      type: "workflow_step",
      attributes: { stepId: "chunk", output: { count: 10 } },
    });
    await exporter.exportTracingEvent(endedEvt(stepSpan));

    const caps = getCaps();
    expect(caps.events).toHaveLength(1);
    expect(caps.events[0]?.event_type).toBe("checkpoint");
    expect(caps.events[0]?.payload["name"]).toBe(
      "mastra.workflow_step.chunk",
    );
  });

  test("descendant span on unknown trace is dropped silently", async () => {
    const exporter = new MesediExporter();
    // No root span opened, so no traceId → execution mapping exists.
    const orphan = makeSpan({
      type: "model_generation",
      traceId: "orphan-trace",
      attributes: { model: "x" },
    });
    await exporter.exportTracingEvent(endedEvt(orphan));

    expect(getCaps().events).toHaveLength(0);
    expect(getCaps().starts).toHaveLength(0);
  });

  test("sequence numbers are monotonic per execution", async () => {
    const exporter = new MesediExporter();
    await exporter.exportTracingEvent(
      startedEvt(makeSpan({ type: "agent_run" })),
    );
    await exporter.exportTracingEvent(
      endedEvt(makeSpan({ type: "tool_call", attributes: { name: "a" } })),
    );
    await exporter.exportTracingEvent(
      endedEvt(makeSpan({ type: "tool_call", attributes: { name: "b" } })),
    );

    const seqs = getCaps().events.map((e) => e.sequence);
    expect(seqs).toEqual([1, 2, 3]);
  });
});

// ──────────────────────────────────────────────────────────────────────
// Status mapping + tenant + fail-open
// ──────────────────────────────────────────────────────────────────────

describe("MesediExporter — status + tenant + fail-open", () => {
  test("execution end status is COMPLETED when span has no error", async () => {
    const exporter = new MesediExporter();
    const span = makeSpan({ type: "agent_run" });
    await exporter.exportTracingEvent(startedEvt(span));
    await exporter.exportTracingEvent(endedEvt(span));

    expect(getCaps().ends[0]?.status).toBe("completed");
  });

  test("execution end status is CRASHED when span has error attr", async () => {
    const exporter = new MesediExporter();
    const startSpan = makeSpan({ type: "agent_run" });
    const endSpan = makeSpan({
      type: "agent_run",
      attributes: { error: "runtime blew up" },
    });
    await exporter.exportTracingEvent(startedEvt(startSpan));
    await exporter.exportTracingEvent(endedEvt(endSpan));

    expect(getCaps().ends[0]?.status).toBe("crashed");
  });

  test("execution end status is HALTED on tripwire abort", async () => {
    const exporter = new MesediExporter();
    const startSpan = makeSpan({ type: "agent_run" });
    const endSpan = makeSpan({
      type: "agent_run",
      attributes: {
        tripwireAbort: { reason: "prompt injection", processorId: "safety" },
      },
    });
    await exporter.exportTracingEvent(startedEvt(startSpan));
    await exporter.exportTracingEvent(endedEvt(endSpan));

    expect(getCaps().ends[0]?.status).toBe("halted");
  });

  test("tenant_id from constructor flows onto execution", async () => {
    const exporter = new MesediExporter({ tenantId: "acme-corp" });
    await exporter.exportTracingEvent(
      startedEvt(makeSpan({ type: "agent_run" })),
    );

    expect(getCaps().starts[0]?.tenant_id).toBe("acme-corp");
  });

  test("swallowed error inside handler doesn't propagate", async () => {
    const exporter = new MesediExporter();
    // A malformed event (missing exportedSpan) should not throw.
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const bad = { type: "span_started" } as any;
    await expect(exporter.exportTracingEvent(bad)).resolves.not.toThrow();
    expect(getCaps().starts).toHaveLength(0);
  });
});
