/**
 * Wire-format contract for the two event types added 2026-09-14 when
 * the egress deferral was reopened: `egress` and
 * `environment_declaration`. Twin of
 * sdk-python/tests/test_egress_environment_events.py: pins the
 * payload keys the backend detectors read and the destination
 * normalization that keeps URL paths and query strings, where
 * secrets ride, from ever leaving the process.
 */

import { beforeEach, describe, expect, it, vi } from "vitest";

import type { Event } from "./events.js";

const captured: Event[] = [];

vi.mock("./client.js", () => ({
  getClient: () => ({
    submitEvent: (e: Event) => {
      captured.push(e);
    },
  }),
}));

import { runInExecutionContext } from "./context.js";
import { emitEgress, emitEnvironmentDeclaration } from "./egress.js";

beforeEach(() => {
  captured.length = 0;
});

describe("emitEgress", () => {
  it("normalizes the destination and never ships path or query", async () => {
    await runInExecutionContext("exec-egress-1", async () => {
      emitEgress("https://api.example.com/v1/users?token=SECRET", {
        protocol: "https",
        toolName: "web_fetch",
        bytesOut: 512,
      });
    });
    expect(captured).toHaveLength(1);
    const evt = captured[0];
    expect(evt.event_type).toBe("egress");
    expect(evt.execution_id).toBe("exec-egress-1");
    expect(evt.payload["destination"]).toBe("api.example.com");
    expect(JSON.stringify(evt.payload)).not.toContain("SECRET");
    expect(evt.payload["protocol"]).toBe("https");
    expect(evt.payload["tool_name"]).toBe("web_fetch");
    expect(evt.payload["bytes_out"]).toBe(512);
  });

  it("keeps host:port when no scheme or path is present", async () => {
    await runInExecutionContext("exec-egress-2", async () => {
      emitEgress("10.2.3.4:8443");
    });
    expect(captured[0].payload["destination"]).toBe("10.2.3.4:8443");
  });

  it("is a silent no-op outside wrap", () => {
    emitEgress("api.example.com");
    expect(captured).toHaveLength(0);
  });
});

describe("emitEnvironmentDeclaration", () => {
  it("lowercases the mode and defaults declared_by to operator", async () => {
    await runInExecutionContext("exec-env-1", async () => {
      emitEnvironmentDeclaration("  Simulation ");
    });
    expect(captured).toHaveLength(1);
    const evt = captured[0];
    expect(evt.event_type).toBe("environment_declaration");
    expect(evt.payload["mode"]).toBe("simulation");
    expect(evt.payload["declared_by"]).toBe("operator");
  });

  it("is a silent no-op outside wrap", () => {
    emitEnvironmentDeclaration("live");
    expect(captured).toHaveLength(0);
  });
});
