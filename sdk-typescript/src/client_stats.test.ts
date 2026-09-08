/**
 * TelemetryStats: the client must be able to say what was lost.
 *
 * Twin of sdk-python/tests/test_shipper_stats.py, written from the same
 * 2026-09-08 session: a run with a bad API key had every submission
 * refused with 401, the SDK logged each refusal and carried on
 * (fail-open, by design), flush() truthfully reported the queue
 * drained, and the session concluded everything succeeded. Nothing in
 * the SDK could contradict it.
 *
 * fetch is stubbed per test; no network, no real backend.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { MesediClient } from "./client.js";
import type { Event } from "./events.js";

function makeClient(): MesediClient {
  return new MesediClient({
    apiKey: "mesedi_sk_stats_test",
    baseUrl: "http://fake.invalid",
    flushIntervalMs: 10,
    timeoutMs: 500,
  });
}

function fixtureEvent(): Event {
  return {
    event_id: "evt-stats-1",
    execution_id: "exec-stats-1",
    event_type: "llm_call",
    sequence: 1,
    timestamp: new Date().toISOString(),
    payload: { note: "stats fixture" },
  } as Event;
}

const realFetch = globalThis.fetch;
let warnSpy: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  // The rejection path logs via console.warn; keep test output clean
  // without hiding that the warning fired.
  warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});
});

afterEach(() => {
  globalThis.fetch = realFetch;
  warnSpy.mockRestore();
});

describe("TelemetryStats", () => {
  it("counts accepted submissions as delivered", async () => {
    globalThis.fetch = vi.fn(async () => new Response("{}", { status: 200 }));
    const c = makeClient();
    c.submitEvent(fixtureEvent());
    expect(await c.flush(2000)).toBe(true);
    const st = c.stats();
    expect(st.delivered).toBeGreaterThanOrEqual(1);
    expect(st.lost).toBe(0);
    expect(st.complete).toBe(true);
  });

  it("counts a 401 instead of only logging it", async () => {
    globalThis.fetch = vi.fn(
      async () =>
        new Response(JSON.stringify({ ok: false, error: "API key not recognized" }), {
          status: 401,
        }),
    );
    const c = makeClient();
    c.submitEvent(fixtureEvent());
    expect(await c.flush(2000)).toBe(true); // fail-open: refused still drains
    const st = c.stats();
    expect(st.rejected).toBeGreaterThanOrEqual(1);
    expect(st.complete).toBe(false);
  });

  it("keeps retry exhaustion separate from rejection", async () => {
    globalThis.fetch = vi.fn(async () => new Response("boom", { status: 500 }));
    const c = makeClient();
    c.submitEvent(fixtureEvent());
    await c.flush(15_000);
    const st = c.stats();
    expect(st.retriesExhausted).toBeGreaterThanOrEqual(1);
    // A transient failure that ran out of retries is not a rejection;
    // conflating them tells an operator to fix the wrong thing.
    expect(st.rejected).toBe(0);
    expect(st.complete).toBe(false);
  }, 30_000);

  it("flush=true must not be read as delivery", async () => {
    // The pair of answers that started this: drained yes, arrived no.
    // stats() existing is what makes the combination detectable.
    globalThis.fetch = vi.fn(async () => new Response("{}", { status: 401 }));
    const c = makeClient();
    c.submitEvent(fixtureEvent());
    const drained = await c.flush(2000);
    const st = c.stats();
    expect(drained).toBe(true);
    expect(st.lost).toBeGreaterThan(0);
  });
});
