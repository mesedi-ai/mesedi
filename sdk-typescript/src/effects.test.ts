/**
 * Wire contract for effect declarations and per-call effects (the
 * backend's effect_aliasing check), and the MCP call's description and
 * effects fields. Twin of sdk-python/tests/test_effects.py.
 */

import { beforeEach, describe, expect, it, vi } from "vitest";

import type { Event } from "./events.js";

const captured: Event[] = [];

vi.mock("./client.js", () => ({
  getClient: () => ({
    submitEvent: (e: Event) => {
      captured.push(e);
    },
    submitExecutionStart: () => {},
    submitExecutionEnd: () => {},
    baseUrl: "http://fake.invalid",
    apiKey: "mesedi_sk_test",
  }),
}));

import { runInExecutionContext } from "./context.js";
import { declareEffects, recordEffects } from "./effects.js";
import { emitMcpCall } from "./mcp.js";
import { tool } from "./tool.js";

beforeEach(() => {
  captured.length = 0;
});

describe("declareEffects", () => {
  it("sends the trimmed destinations and via lists", async () => {
    await runInExecutionContext("exec-effects-1", async () => {
      declareEffects(
        [" github.com/acme/app ", { destination: "db:prod/customers", via: ["crm/update", " "] }, "", { destination: "" }],
        { note: "release run" },
      );
    });
    expect(captured).toHaveLength(1);
    expect(captured[0].event_type).toBe("effect_declaration");
    expect(captured[0].payload).toEqual({
      destinations: [
        { destination: "github.com/acme/app" },
        { destination: "db:prod/customers", via: ["crm/update"] },
      ],
      declared_by: "operator",
      note: "release run",
    });
  });

  it("is a no-op outside wrap()", () => {
    declareEffects(["x"]);
    expect(captured).toHaveLength(0);
  });
});

describe("recordEffects inside tool()", () => {
  it("rides on the tool call, even across awaits", async () => {
    const pushBranch = tool(async function pushBranch(repo: string) {
      await new Promise((r) => setTimeout(r, 1));
      recordEffects(`github.com/${repo}`, "", "  ");
      return "ok";
    });
    await runInExecutionContext("exec-effects-2", async () => {
      await pushBranch("acme/app");
    });
    expect(captured[0].payload["effects"]).toEqual(["github.com/acme/app"]);
  });

  it("a failed call still reports what it changed", async () => {
    const halfDone = tool(async function halfDone() {
      recordEffects("s3://acme/partial");
      throw new Error("boom");
    });
    await runInExecutionContext("exec-effects-3", async () => {
      await expect(halfDone()).rejects.toThrow("boom");
    });
    expect(captured[0].payload["status"]).toBe("failed");
    expect(captured[0].payload["effects"]).toEqual(["s3://acme/partial"]);
  });

  it("does not leak between calls or outside a tool", async () => {
    const writes = tool(async function writes() {
      recordEffects("a");
      return "ok";
    });
    const reads = tool(async function reads() {
      return "ok";
    });
    await runInExecutionContext("exec-effects-4", async () => {
      recordEffects("outside");
      await Promise.all([writes(), reads()]);
    });
    const byName = Object.fromEntries(captured.map((e) => [e.payload["tool_name"], e.payload]));
    expect(byName["writes"]["effects"]).toEqual(["a"]);
    expect(byName["reads"]["effects"]).toBeUndefined();
  });
});

describe("emitMcpCall", () => {
  it("carries the description and effects", async () => {
    await runInExecutionContext("exec-effects-5", async () => {
      emitMcpCall("github", "create_or_update_file", {
        returnValue: "ok",
        toolDescription: "Creates or updates a file.",
        effects: ["github.com/acme/app", ""],
      });
      emitMcpCall("fs", "read", { returnValue: "x" });
    });
    expect(captured[0].payload["tool_description"]).toBe("Creates or updates a file.");
    expect(captured[0].payload["effects"]).toEqual(["github.com/acme/app"]);
    expect(captured[1].payload["tool_description"]).toBeUndefined();
    expect(captured[1].payload["effects"]).toBeUndefined();
  });
});

describe("cleanEffects", () => {
  it("trims and drops empty destinations", async () => {
    const { cleanEffects } = await import("./effects.js");
    expect(cleanEffects([" a ", "", "  ", "b"])).toEqual(["a", "b"]);
    expect(cleanEffects(undefined)).toEqual([]);
  });
});
