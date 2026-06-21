/**
 * Tests for the structured return_value field added to tool()
 * tool_call payloads (#270).
 *
 * The backend's tool_schema_drift detector fingerprints the return
 * shape from this field. These tests pin the contract so a future
 * edit to tool() that drops or mis-types the field fails CI before
 * reaching customers.
 *
 * The structuredReturnValue function is not exported (private
 * helper). We test indirectly by spying on the submitted event
 * via a fake client.
 */

import { describe, expect, test, beforeEach, afterEach, vi } from "vitest";

import * as clientModule from "./client.js";
import * as contextModule from "./context.js";
import { Event } from "./events.js";
import { tool } from "./tool.js";

interface CapturedClient {
  submitEvent: (e: Event) => void;
  events: Event[];
}

function makeCapturedClient(): CapturedClient {
  const events: Event[] = [];
  return {
    events,
    submitEvent: (e: Event) => {
      events.push(e);
    },
  };
}

let capturedClient: CapturedClient;
const fakeCtx = {
  executionId: "exec-test",
  nextSequence: () => 1,
  checkBudget: () => {},
  budgetTracker: null,
};

beforeEach(() => {
  capturedClient = makeCapturedClient();
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  vi.spyOn(clientModule, "getClient").mockReturnValue(capturedClient as any);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  vi.spyOn(contextModule, "currentExecutionContext").mockReturnValue(fakeCtx as any);
});

afterEach(() => {
  vi.restoreAllMocks();
});

function lastPayload(): Record<string, unknown> {
  return capturedClient.events[capturedClient.events.length - 1].payload as Record<
    string,
    unknown
  >;
}

describe("tool() return_value field", () => {
  test("json-native object passes through as nested structure", async () => {
    const fetchItem = tool(async () => ({ id: "a", name: "widget", price: 1.99 }));
    await fetchItem();
    expect(lastPayload().return_value).toEqual({
      id: "a",
      name: "widget",
      price: 1.99,
    });
  });

  test("nested array shape is preserved", async () => {
    const fn = tool(async () => [1, "two", { three: 3 }]);
    await fn();
    expect(lastPayload().return_value).toEqual([1, "two", { three: 3 }]);
  });

  test("primitive return values pass through", async () => {
    const fn = tool(async () => 42);
    await fn();
    expect(lastPayload().return_value).toBe(42);
  });

  test("BigInt coerces to string via replacer", async () => {
    const fn = tool(async () => ({ big: BigInt(42) }));
    await fn();
    const rv = lastPayload().return_value as Record<string, unknown>;
    expect(rv.big).toBe("42");
  });

  test("oversized return returns the <truncated> sentinel", async () => {
    const huge = "x".repeat(3000);
    const fn = tool(async () => ({ data: huge }));
    await fn();
    expect(lastPayload().return_value).toBe("<truncated>");
  });

  test("circular reference omits return_value field cleanly", async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const circ: any = {};
    circ.self = circ;
    const fn = tool(async () => circ);
    await fn();
    // Field should be ABSENT (not undefined-explicit) when
    // serialization fails.
    expect("return_value" in lastPayload()).toBe(false);
    // result_summary should still be present for human display.
    expect("result_summary" in lastPayload()).toBe(true);
  });

  test("returned value is JSON.stringify-safe without replacer", async () => {
    // Shipper later calls JSON.stringify on the whole payload with
    // no replacer. Must not crash on what we put in return_value.
    const fn = tool(async () => ({ a: BigInt(1), b: 2 }));
    await fn();
    const payload = lastPayload();
    // This MUST not throw.
    expect(() => JSON.stringify(payload)).not.toThrow();
  });

  test("failed tool call omits return_value (no result to ship)", async () => {
    const fn = tool(async () => {
      throw new Error("boom");
    });
    await expect(fn()).rejects.toThrow("boom");
    expect("return_value" in lastPayload()).toBe(false);
    // result_summary also absent on failure path; status is "failed".
    expect(lastPayload().status).toBe("failed");
  });
});
