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
 * Each test answers from a local HTTP server on 127.0.0.1 with a fixed
 * status; no outside network, no real backend. Until 2026-11-15 this
 * stubbed global fetch; the shipper now sends through node:http (see
 * _transport.ts), so a real local server exercises the real sender.
 */

import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { MesediClient } from "./client.js";
import type { Event } from "./events.js";

let server: Server | null = null;

/** Start a local server answering every request with `status` and `body`. */
async function answerWith(status: number, body = "{}"): Promise<string> {
  server = createServer((req, res) => {
    req.resume();
    req.on("end", () => {
      res.writeHead(status, { "Content-Type": "application/json" });
      res.end(body);
    });
  });
  await new Promise<void>((r) => server!.listen(0, "127.0.0.1", r));
  return `http://127.0.0.1:${(server!.address() as AddressInfo).port}`;
}

function makeClient(baseUrl: string): MesediClient {
  return new MesediClient({
    apiKey: "mesedi_sk_stats_test",
    baseUrl,
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

let warnSpy: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  // The rejection path logs via console.warn; keep test output clean
  // without hiding that the warning fired.
  warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});
});

afterEach(async () => {
  warnSpy.mockRestore();
  if (server) {
    server.closeAllConnections();
    await new Promise<void>((r) => server!.close(() => r()));
    server = null;
  }
});

describe("TelemetryStats", () => {
  it("counts accepted submissions as delivered", async () => {
    const c = makeClient(await answerWith(200));
    c.submitEvent(fixtureEvent());
    expect(await c.flush(2000)).toBe(true);
    const st = c.stats();
    expect(st.delivered).toBeGreaterThanOrEqual(1);
    expect(st.lost).toBe(0);
    expect(st.complete).toBe(true);
  });

  it("counts a 401 instead of only logging it", async () => {
    const c = makeClient(
      await answerWith(401, JSON.stringify({ ok: false, error: "API key not recognized" })),
    );
    c.submitEvent(fixtureEvent());
    expect(await c.flush(2000)).toBe(true); // fail-open: refused still drains
    const st = c.stats();
    expect(st.rejected).toBeGreaterThanOrEqual(1);
    expect(st.complete).toBe(false);
  });

  it("keeps retry exhaustion separate from rejection", async () => {
    const c = makeClient(await answerWith(500, "boom"));
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
    const c = makeClient(await answerWith(401));
    c.submitEvent(fixtureEvent());
    const drained = await c.flush(2000);
    const st = c.stats();
    expect(drained).toBe(true);
    expect(st.lost).toBeGreaterThan(0);
  });
});
