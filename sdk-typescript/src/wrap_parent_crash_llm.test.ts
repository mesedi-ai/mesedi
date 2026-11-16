/**
 * Fixes from the 2026-11-15 outside review, shipped in 0.11.0:
 *
 * - D5: a wrap() opened inside another sends parent_execution_id.
 * - D2: a crash sends crash_type (the error's class name) next to the
 *   crash_signature hash, so the crash allowlist can match "TypeError".
 * - D34: emitLlmCall, the manual llm_call emitter Python already had,
 *   with provider and errorClass for the provider_incident detector.
 * - D19: validator severity "info" is accepted; an unknown value becomes
 *   "warning" (it used to become "error", a critical alert).
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import * as clientMod from "./client.js";
import { executionEndPayload, executionStartPayload, type Execution } from "./events.js";
import { emitLlmCall, validatorResult } from "./observe.js";
import { VERSION } from "./index.js";
import { crashType, wrap } from "./wrap.js";

class Capture {
  starts: Record<string, unknown>[] = [];
  ends: Record<string, unknown>[] = [];
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  events: any[] = [];
  baseUrl = "";
  apiKey = "";
  submitExecutionStart(e: Execution): void {
    this.starts.push({ ...executionStartPayload(e), execution_id: e.execution_id });
  }
  submitExecutionEnd(e: Execution): void {
    this.ends.push({ ...executionEndPayload(e), execution_id: e.execution_id });
  }
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  submitEvent(event: any): void {
    this.events.push(event);
  }
}

let cap: Capture;

beforeEach(() => {
  cap = new Capture();
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  vi.spyOn(clientMod, "getClient").mockReturnValue(cap as any);
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("parent_execution_id", () => {
  it("is set on a run opened inside another", async () => {
    const child = wrap(async () => "c");
    const parent = wrap(async () => child());
    await parent();
    const [outer, inner] = cap.starts;
    expect(outer).not.toHaveProperty("parent_execution_id");
    expect(inner!["parent_execution_id"]).toBe(outer!["execution_id"]);
  });

  it("is the same for concurrent children", async () => {
    const child = wrap(async (n: number) => {
      await new Promise((r) => setTimeout(r, 5));
      return n;
    });
    const parent = wrap(async () => Promise.all([child(1), child(2), child(3)]));
    expect(await parent()).toEqual([1, 2, 3]);
    const outer = cap.starts[0]!["execution_id"];
    expect(cap.starts.slice(1).map((s) => s["parent_execution_id"])).toEqual([outer, outer, outer]);
  });

  it("is absent for runs one after another", async () => {
    const agent = wrap(async () => "x");
    await agent();
    await agent();
    expect(cap.starts.every((s) => !("parent_execution_id" in s))).toBe(true);
  });
});

describe("crash_type", () => {
  it("names the error's class", async () => {
    const agent = wrap(async () => {
      throw new TypeError("bad");
    });
    await expect(agent()).rejects.toThrow(TypeError);
    expect(cap.ends[0]!["status"]).toBe("crashed");
    expect(cap.ends[0]!["crash_type"]).toBe("TypeError");
    expect(String(cap.ends[0]!["crash_signature"])).toHaveLength(16);
  });

  it("is absent on a completed run", async () => {
    await wrap(async () => "ok")();
    expect(cap.ends[0]).not.toHaveProperty("crash_type");
  });

  it("falls back to the value's type for non-errors", () => {
    expect(crashType(new RangeError("x"))).toBe("RangeError");
    expect(crashType("a string")).toBe("string");
  });
});

describe("emitLlmCall", () => {
  it("emits an llm_call with provider and error class", async () => {
    await wrap(async () => {
      emitLlmCall({ model: "gpt-4o", userMessage: "hi", status: "failed", provider: "openai", errorClass: "rate_limited", durationMs: 12 });
    })();
    const ev = cap.events.find((e) => e.event_type === "llm_call");
    expect(ev.payload).toMatchObject({ model: "gpt-4o", user_message: "hi", status: "failed", provider: "openai", error_class: "rate_limited" });
    expect(ev.payload).not.toHaveProperty("response_text");
    expect(ev.duration_ms).toBe(12);
  });

  it("matches Python's payload for a successful call", async () => {
    await wrap(async () => {
      emitLlmCall({ model: "m", userMessage: "u".repeat(1200), responseText: "r", inputTokens: 3, outputTokens: 4 });
    })();
    const ev = cap.events.find((e) => e.event_type === "llm_call");
    expect(ev.payload).toEqual({
      model: "m", system_prompt: "", user_message: "u".repeat(1000), status: "ok",
      response_text: "r", input_tokens: 3, output_tokens: 4,
    });
  });

  it("does nothing outside wrap()", () => {
    emitLlmCall({ model: "m", userMessage: "u" });
    expect(cap.events).toHaveLength(0);
  });
});

describe("validator severity", () => {
  const severityOf = async (value: string): Promise<string> => {
    await wrap(async () => {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      validatorResult("v", false, { severity: value as any });
    })();
    return cap.events.at(-1).payload.severity;
  };

  it.each(["info", "warning", "error", "critical"])("passes %s through", async (value) => {
    expect(await severityOf(value)).toBe(value);
  });

  it("records an unknown value as warning, not critical, and warns once", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    expect(await severityOf("fatal-x")).toBe("warning");
    expect(await severityOf("fatal-x")).toBe("warning");
    expect(warn.mock.calls.filter((c) => String(c[0]).includes("fatal-x"))).toHaveLength(1);
  });
});

describe("VERSION", () => {
  it("is the package version", async () => {
    const { readFileSync } = await import("node:fs");
    const pkg = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8"));
    expect(VERSION).toBe(pkg.version);
  });
});

describe("eval helper exports (the outside review of 2026-11-15)", () => {
  // Needs the built package (npm run build), like exit_silent_backend.test.ts.
  it.each([
    ["./integrations/ragas", ["reportFaithfulness", "reportAnswerRelevance", "reportContextPrecision"]],
    ["./integrations/promptfoo", ["reportFactuality", "reportLlmRubric"]],
    ["./integrations/vectara", ["reportHhem"]],
  ])("%s is importable and exports its helpers", async (subpath, names) => {
    const { readFileSync } = await import("node:fs");
    const pkg = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8"));
    const entry = pkg.exports[subpath];
    expect(entry, `${subpath} missing from package.json exports`).toBeDefined();
    const mod = await import(new URL(`../${entry.import}`, import.meta.url).href);
    for (const n of names) expect(typeof mod[n], `${subpath} ${n}`).toBe("function");
  });
});
