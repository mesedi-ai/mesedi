/**
 * Direct tests of sendUnref (src/_transport.ts), the shipper's sender
 * added 2026-11-15 for the outside review of 2026-11-15. The exit behaviour it enables is
 * tested end to end in exit_silent_backend.test.ts; these pin the
 * sender's own contract against a local server.
 */

import { createServer, type Server } from "node:http";
import { createServer as createTcpServer, type AddressInfo, type Socket } from "node:net";

import { afterEach, describe, expect, it } from "vitest";

import { sendUnref } from "./_transport.js";

const servers: Array<() => Promise<void>> = [];

afterEach(async () => {
  while (servers.length) await servers.pop()!();
});

async function answering(status: number, body: string): Promise<string> {
  let seen = "";
  const srv: Server = createServer((req, res) => {
    req.on("data", (c) => (seen += c));
    req.on("end", () => {
      res.writeHead(status, { "X-Seen-Length": String(seen.length) });
      res.end(body);
    });
  });
  await new Promise<void>((r) => srv.listen(0, "127.0.0.1", r));
  servers.push(async () => {
    srv.closeAllConnections();
    await new Promise<void>((r) => srv.close(() => r()));
  });
  return `http://127.0.0.1:${(srv.address() as AddressInfo).port}`;
}

describe("sendUnref", () => {
  it("returns status and body text", async () => {
    const url = await answering(201, "created");
    const r = await sendUnref(url + "/events", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: new TextEncoder().encode('{"a":1}'),
      timeoutMs: 2000,
    });
    expect(r.status).toBe(201);
    expect(r.text).toBe("created");
  });

  it("rejects when the server never answers within timeoutMs", async () => {
    const held: Socket[] = [];
    const srv = createTcpServer((s) => held.push(s));
    await new Promise<void>((r) => srv.listen(0, "127.0.0.1", r));
    servers.push(async () => {
      for (const s of held) s.destroy();
      await new Promise<void>((r) => srv.close(() => r()));
    });
    const url = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`;
    const started = Date.now();
    await expect(
      sendUnref(url, { method: "POST", headers: {}, body: new Uint8Array(), timeoutMs: 300 }),
    ).rejects.toThrow("no response within 300 ms");
    expect(Date.now() - started).toBeLessThan(3000);
  });

  it("rejects promptly when aborted", async () => {
    const held: Socket[] = [];
    const srv = createTcpServer((s) => held.push(s));
    await new Promise<void>((r) => srv.listen(0, "127.0.0.1", r));
    servers.push(async () => {
      for (const s of held) s.destroy();
      await new Promise<void>((r) => srv.close(() => r()));
    });
    const url = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`;
    const controller = new AbortController();
    const pending = sendUnref(url, {
      method: "POST",
      headers: {},
      body: new Uint8Array(),
      timeoutMs: 10_000,
      signal: controller.signal,
    });
    setTimeout(() => controller.abort(), 100);
    await expect(pending).rejects.toThrow();
  });
});
