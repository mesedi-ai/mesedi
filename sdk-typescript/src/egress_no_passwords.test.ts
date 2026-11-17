/**
 * Round 3, finding M2: the addressesNeverContainPasswords setting
 * reaches recorded egress addresses, from the option and from the
 * environment. Twin of the Python tests in
 * tests/test_egress_environment_events.py.
 */

import { afterEach, describe, expect, it, vi } from "vitest";

import type { Event } from "./events.js";

let setting = false;
const captured: Event[] = [];

vi.mock("./client.js", async (importOriginal) => {
  const real = await importOriginal<typeof import("./client.js")>();
  return {
    ...real,
    getClient: () => ({
      submitEvent: (e: Event) => captured.push(e),
      addressesNeverContainPasswords: setting,
    }),
  };
});

import { MesediClient } from "./client.js";
import { runInExecutionContext } from "./context.js";
import { emitEgress } from "./egress.js";

afterEach(() => {
  captured.length = 0;
  delete process.env["MESEDI_ADDRESSES_NEVER_CONTAIN_PASSWORDS"];
});

describe("addressesNeverContainPasswords", () => {
  it("decides host:8080/a@b", async () => {
    for (const [on, want] of [
      [false, "unreadable"],
      [true, "host.example.com:8080"],
    ] as const) {
      setting = on;
      captured.length = 0;
      await runInExecutionContext("exec-m2", async () => {
        emitEgress("https://host.example.com:8080/a@b");
      });
      expect(captured[0].payload["destination"], String(on)).toBe(want);
    }
  });

  it("is read from the option, then the environment, else false", async () => {
    const make = (opts: Record<string, unknown> = {}) =>
      new MesediClient({ apiKey: "mesedi_sk_m2", baseUrl: "http://127.0.0.1:9", ...opts });
    const clients: MesediClient[] = [];
    try {
      clients.push(make());
      expect(clients[0].addressesNeverContainPasswords).toBe(false);
      process.env["MESEDI_ADDRESSES_NEVER_CONTAIN_PASSWORDS"] = "1";
      clients.push(make());
      expect(clients[1].addressesNeverContainPasswords).toBe(true);
      clients.push(make({ addressesNeverContainPasswords: false }));
      expect(clients[2].addressesNeverContainPasswords).toBe(false);
    } finally {
      for (const c of clients) await c.shutdown(10);
    }
  });
});
