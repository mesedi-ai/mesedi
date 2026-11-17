/**
 * Report chaining and signing (mesedi-report-chain-v1), checked against
 * spec/report_chain_vectors.json, which the backend and the Python SDK
 * check too. Twin of sdk-python/tests/test_report_chain.py.
 */

import { readFileSync } from "node:fs";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";

import { afterEach, describe, expect, it, vi } from "vitest";

import {
  canonicalTimestamp,
  ed25519Signer,
  payloadBytes,
  ReportChainer,
  reportChainGenesis,
  reportFingerprint,
} from "./chain.js";
import { MesediClient } from "./client.js";
import type { Event } from "./events.js";

interface Vectors {
  execution_id: string;
  genesis: string;
  events: Array<{
    event_id: string;
    event_type: string;
    sequence: number;
    timestamp: string;
    duration_ms: number | null;
    payload: string;
    fingerprint: string;
  }>;
  signature: { private_key_seed_hex: string; signature_b64: string };
}

const V: Vectors = JSON.parse(
  readFileSync(new URL("../../spec/report_chain_vectors.json", import.meta.url), "utf8"),
);

describe("shared report chain vectors", () => {
  it("genesis and every fingerprint", () => {
    expect(reportChainGenesis(V.execution_id)).toBe(V.genesis);
    let prev = V.genesis;
    for (const e of V.events) {
      const got = reportFingerprint({
        eventId: e.event_id,
        executionId: V.execution_id,
        eventType: e.event_type,
        sequence: e.sequence,
        timestamp: e.timestamp,
        durationMs: e.duration_ms ?? undefined,
        payload: new TextEncoder().encode(e.payload),
        chainPrev: prev,
      });
      expect(got, e.event_id).toBe(e.fingerprint);
      prev = got;
    }
  });

  it("the signature from the published test key", () => {
    const seed = Uint8Array.from(Buffer.from(V.signature.private_key_seed_hex, "hex"));
    const signer = ed25519Signer("test", seed);
    const result = signer(Uint8Array.from(Buffer.from(V.events[0].fingerprint, "hex")));
    expect(result).not.toBeInstanceOf(Promise);
    const { keyId, signature } = result as { keyId: string; signature: Uint8Array };
    expect(keyId).toBe("test");
    expect(Buffer.from(signature).toString("base64")).toBe(V.signature.signature_b64);
  });
});

describe("canonicalTimestamp", () => {
  it("normalizes to UTC with six fractional digits", () => {
    expect(canonicalTimestamp("2026-11-17T14:00:01.5+02:00")).toBe("2026-11-17T12:00:01.500000Z");
    expect(canonicalTimestamp("2026-11-17T12:00:00.123456Z")).toBe("2026-11-17T12:00:00.123456Z");
    expect(canonicalTimestamp("2026-11-17T12:00:00Z")).toBe("2026-11-17T12:00:00.000000Z");
    expect(canonicalTimestamp("2026-11-17T12:00:00.123Z")).toBe("2026-11-17T12:00:00.123000Z");
    expect(canonicalTimestamp("2026-11-17T00:30:00.1234567-01:00")).toBe("2026-11-17T01:30:00.123456Z");
  });
});

function ev(exec: string, n: number): Event {
  return {
    event_id: `${exec}-e${n}`,
    execution_id: exec,
    event_type: "tool_call",
    sequence: n,
    timestamp: "2026-11-17T12:00:00.000000Z",
    payload: { tool_name: "t", n, note: "café ✓" },
  } as Event;
}

describe("ReportChainer", () => {
  it("links each run separately, in call order", async () => {
    const c = new ReportChainer();
    const a1 = ev("a", 1);
    const b1 = ev("b", 1);
    const a2 = ev("a", 2);
    for (const e of [a1, b1, a2]) await c.chain(e);
    expect(a1.chain_prev).toBe(reportChainGenesis("a"));
    expect(b1.chain_prev).toBe(reportChainGenesis("b"));
    const fpA1 = reportFingerprint({
      eventId: a1.event_id, executionId: "a", eventType: "tool_call", sequence: 1,
      timestamp: a1.timestamp, payload: payloadBytes(a1.payload), chainPrev: a1.chain_prev!,
    });
    expect(a2.chain_prev).toBe(fpA1);
    expect(a1.signature).toBeUndefined();
  });

  it("keeps call order even when a slow signer lets calls overlap", async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    const c = new ReportChainer(async () => {
      await gate;
      return { keyId: "k", signature: new Uint8Array(64) };
    });
    const e1 = ev("r", 1);
    const e2 = ev("r", 2);
    const p1 = c.chain(e1);
    const p2 = c.chain(e2);
    release();
    await Promise.all([p1, p2]);
    expect(e1.chain_prev).toBe(reportChainGenesis("r"));
    expect(e2.chain_prev).not.toBe(reportChainGenesis("r"));
  });

  it("a failing signer leaves the report chained but unsigned", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const e = ev("x", 1);
    await new ReportChainer(() => {
      throw new Error("signing service down");
    }).chain(e);
    expect(e.chain_prev).toBe(reportChainGenesis("x"));
    expect(e.signature).toBeUndefined();
    warn.mockRestore();
  });
});

let server: Server | null = null;
afterEach(async () => {
  if (server) await new Promise<void>((r) => server!.close(() => r()));
  server = null;
});

describe("the client sends chained, signed reports", () => {
  it("puts chain_prev and signature on the wire, in send order", async () => {
    const bodies: unknown[] = [];
    server = createServer((req, res) => {
      const chunks: Buffer[] = [];
      req.on("data", (c) => chunks.push(c));
      req.on("end", () => {
        if (req.url === "/events") bodies.push(JSON.parse(Buffer.concat(chunks).toString("utf8")));
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end("{}");
      });
    });
    await new Promise<void>((r) => server!.listen(0, "127.0.0.1", r));
    const signed: string[] = [];
    const client = new MesediClient({
      apiKey: "mesedi_sk_chain_test",
      baseUrl: `http://127.0.0.1:${(server!.address() as AddressInfo).port}`,
      flushIntervalMs: 10,
      batchSize: 2,
      reportSigner: (fp) => {
        signed.push(Buffer.from(fp).toString("hex"));
        return { keyId: "k1", signature: new Uint8Array(64).fill(1) };
      },
    });
    for (const n of [1, 2, 3]) client.submitEvent(ev("run", n));
    expect(await client.flush(5_000)).toBe(true);
    await client.shutdown(1_000);
    const sent = (bodies as Array<Array<Record<string, unknown>>>).flat();
    expect(sent.map((e) => e["event_id"])).toEqual(["run-e1", "run-e2", "run-e3"]);
    expect(sent[0]["chain_prev"]).toBe(reportChainGenesis("run"));
    expect(sent[1]["chain_prev"]).toBe(signed[0]);
    expect(sent[2]["chain_prev"]).toBe(signed[1]);
    for (const e of sent) {
      expect(e["signature"]).toEqual({ key_id: "k1", sig: Buffer.from(new Uint8Array(64).fill(1)).toString("base64") });
    }
  });
});

describe("a rate-limited batch", () => {
  it("is retried, not dropped, and other 4xx answers stay permanent", async () => {
    const answers = [429, 200];
    const seen: number[] = [];
    server = createServer((req, res) => {
      req.resume();
      req.on("end", () => {
        const code = answers.shift() ?? 200;
        seen.push(code);
        res.writeHead(code, { "Content-Type": "application/json", "Retry-After": "0" });
        res.end("{}");
      });
    });
    await new Promise<void>((r) => server!.listen(0, "127.0.0.1", r));
    const client = new MesediClient({
      apiKey: "mesedi_sk_rl_test",
      baseUrl: `http://127.0.0.1:${(server!.address() as AddressInfo).port}`,
      flushIntervalMs: 10,
    });
    client.submitEvent(ev("rl", 1));
    expect(await client.flush(5_000)).toBe(true);
    await client.shutdown(1_000);
    expect(seen).toEqual([429, 200]);
    expect(client.stats().delivered).toBe(1);
    expect(client.stats().rejected).toBe(0);
  });
});

describe("the declared end", () => {
  it("matches the shared end vector", async () => {
    const { reportChainEndMessage } = await import("./chain.js");
    const e = (JSON.parse(readFileSync(new URL("../../spec/report_chain_vectors.json", import.meta.url), "utf8")) as {
      execution_id: string; end: { last_fingerprint: string; end_message_hex: string; signature_b64: string };
    });
    const msg = reportChainEndMessage(e.execution_id, e.end.last_fingerprint);
    expect(Buffer.from(msg).toString("hex")).toBe(e.end.end_message_hex);
    const seed = Uint8Array.from(Buffer.from(V.signature.private_key_seed_hex, "hex"));
    const signed = ed25519Signer("t", seed)(msg) as { signature: Uint8Array };
    expect(Buffer.from(signed.signature).toString("base64")).toBe(e.end.signature_b64);
  });

  it("rides on the run-finished message, signed over the end message", async () => {
    const { reportChainEndMessage } = await import("./chain.js");
    const patches: Array<Record<string, unknown>> = [];
    server = createServer((req, res) => {
      const chunks: Buffer[] = [];
      req.on("data", (c) => chunks.push(c));
      req.on("end", () => {
        if (req.method === "PATCH") patches.push(JSON.parse(Buffer.concat(chunks).toString("utf8")));
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end("{}");
      });
    });
    await new Promise<void>((r) => server!.listen(0, "127.0.0.1", r));
    const signed: string[] = [];
    const client = new MesediClient({
      apiKey: "mesedi_sk_end_test",
      baseUrl: `http://127.0.0.1:${(server!.address() as AddressInfo).port}`,
      flushIntervalMs: 10,
      reportSigner: (m) => {
        signed.push(Buffer.from(m).toString("hex"));
        return { keyId: "k1", signature: new Uint8Array(64).fill(2) };
      },
    });
    client.submitEvent(ev("end-run", 1));
    client.submitEvent(ev("end-run", 2));
    client.submitExecutionEnd({ execution_id: "end-run", status: "completed" } as never);
    expect(await client.flush(5_000)).toBe(true);
    await client.shutdown(1_000);
    const end = patches[0]!["report_chain_end"] as { hash: string; signature: { key_id: string; sig: string } };
    expect(end.hash).toBe(signed[1]);
    expect(signed[2]).toBe(Buffer.from(reportChainEndMessage("end-run", signed[1]!)).toString("hex"));
    expect(end.signature).toEqual({ key_id: "k1", sig: Buffer.from(new Uint8Array(64).fill(2)).toString("base64") });
  });
});
