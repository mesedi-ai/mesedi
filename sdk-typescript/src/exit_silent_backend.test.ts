/**
 * Exit behaviour when the backend accepts connections but never answers.
 * Twin of sdk-python/tests/test_exit_against_silent_backend.py.
 *
 * Until 2026-11-15 a one-second script against such a backend (which is
 * how api.mesedi.ai behaved while paused) stayed open for 86 seconds
 * (the outside review of 2026-11-15). The rule now, the same in both SDKs: wait
 * at most `shutdownTimeoutMs` at exit, then say in one warning how many
 * items were not delivered and where they were going.
 *
 * Each case runs a real child Node process against the BUILT package in
 * dist/, because the behaviour under test is what happens at process
 * exit. CI builds before it tests; locally, run `npm run build` first.
 */

import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import { createServer, type Server } from "node:http";
import { createServer as createTcpServer, type AddressInfo, type Server as TcpServer, type Socket } from "node:net";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

const DIST = fileURLToPath(new URL("../dist/index.js", import.meta.url));

function script(url: string, waitMs: number, agentMs = 0): string {
  return `
    import { configure, wrap } from ${JSON.stringify(DIST)};
    configure({ apiKey: "mesedi_sk_test_exit", baseUrl: ${JSON.stringify(url)}, shutdownTimeoutMs: ${waitMs} });
    const agent = wrap(async () => {
      await new Promise((r) => setTimeout(r, ${agentMs}));
      return "ok";
    });
    console.log("agent returned:", await agent());
  `;
}

/** Run the child in a separate process; the parent's own servers keep answering meanwhile. */
async function runChild(
  url: string,
  waitMs: number,
  agentMs = 0,
): Promise<{ out: string; err: string; seconds: number }> {
  const started = Date.now();
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, ["--input-type=module", "-e", script(url, waitMs, agentMs)]);
    let out = "";
    let err = "";
    child.stdout.on("data", (d) => (out += d));
    child.stderr.on("data", (d) => (err += d));
    const kill = setTimeout(() => child.kill("SIGKILL"), 60_000);
    child.on("error", reject);
    child.on("close", () => {
      clearTimeout(kill);
      resolve({ out, err, seconds: (Date.now() - started) / 1000 });
    });
  });
}

/** Accepts TCP connections and never sends a byte back. */
async function silentServer(): Promise<{ url: string; close: () => Promise<void> }> {
  const held: Socket[] = [];
  const srv: TcpServer = createTcpServer((s) => held.push(s));
  await new Promise<void>((r) => srv.listen(0, "127.0.0.1", r));
  const url = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`;
  return {
    url,
    close: async () => {
      for (const s of held) s.destroy();
      await new Promise<void>((r) => srv.close(() => r()));
    },
  };
}

describe("exit against a backend that never answers", () => {
  it("needs the built package", () => {
    expect(existsSync(DIST), `${DIST} missing: run npm run build`).toBe(true);
  });

  it("is bounded by shutdownTimeoutMs and reported once", async () => {
    const s = await silentServer();
    try {
      const { out, err, seconds } = await runChild(s.url, 1000);
      expect(out).toContain("agent returned: ok");
      expect(seconds).toBeLessThan(6);
      expect(err).toContain(`were not delivered to ${s.url}`);
      expect(err).toContain("did not answer within 1.0s");
      expect(err.split("were not delivered").length - 1).toBe(1);
    } finally {
      await s.close();
    }
  }, 30_000);

  it("is bounded even when a send was already hanging as the program ended", async () => {
    // The agent runs past the 250 ms flush interval, so the first send
    // is already waiting on the silent backend when the agent returns.
    // A socket that kept the process alive would block "beforeExit"
    // until the request timed out, and the exit wait would never start.
    const s = await silentServer();
    try {
      const { out, err, seconds } = await runChild(s.url, 1000, 1500);
      expect(out).toContain("agent returned: ok");
      expect(seconds).toBeLessThan(7);
      expect(err).toContain(`were not delivered to ${s.url}`);
    } finally {
      await s.close();
    }
  }, 60_000);

  it("waits as long as the caller chose", async () => {
    const s = await silentServer();
    try {
      const { err, seconds } = await runChild(s.url, 3000);
      expect(seconds).toBeGreaterThanOrEqual(3);
      expect(err).toContain("did not answer within 3.0s");
    } finally {
      await s.close();
    }
  }, 30_000);

  it("exits without a warning when the backend answers", async () => {
    const srv: Server = createServer((req, res) => {
      req.resume();
      req.on("end", () => {
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end('{"ok":true,"accepted":1,"rejected":0}');
      });
    });
    await new Promise<void>((r) => srv.listen(0, "127.0.0.1", r));
    try {
      const url = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`;
      const { out, err } = await runChild(url, 5000);
      expect(out).toContain("agent returned: ok");
      expect(err).not.toContain("were not delivered");
    } finally {
      srv.closeAllConnections();
      await new Promise<void>((r) => srv.close(() => r()));
    }
  }, 30_000);
});
