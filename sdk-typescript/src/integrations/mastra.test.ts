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
