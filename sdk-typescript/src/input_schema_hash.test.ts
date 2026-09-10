/**
 * input_schema_hash: the declared-definition capture for the
 * backend's definition-drift detector (the mcp-pin class: a tool
 * whose declared input schema changes while its description stays
 * byte-identical).
 *
 * The cross-SDK fixture below pins hash agreement with the Python
 * SDK: the expected hex was computed by mesedi/_jcs.py. If either
 * SDK's canonicalization drifts, this constant catches it, because a
 * project mixing Python and TypeScript agents must never see phantom
 * definition drift from SDK disagreement.
 */

import { describe, expect, test, beforeEach, afterEach, vi } from "vitest";

import * as clientModule from "./client.js";
import * as contextModule from "./context.js";
import { Event } from "./events.js";
import { inputSchemaHash } from "./jcs.js";
import { emitMcpCall } from "./observe.js";
import { tool } from "./tool.js";

// Computed by the Python SDK (mesedi/_jcs.py) for this exact schema.
const CROSS_SDK_SCHEMA = {
  type: "object",
  properties: { city: { type: "string" } },
  required: ["city"],
};
const CROSS_SDK_EXPECTED_HASH =
  "be45ab5b6d9f7d0b36dea2e7e355170888fb49d00124420f9e6f76c5d5620fc4";

interface CapturedClient {
  submitEvent: (e: Event) => void;
  events: Event[];
}

let capturedClient: CapturedClient;

const fakeCtx = {
  executionId: "exec-test",
  nextSequence: () => 1,
  checkBudget: () => {},
  budgetTracker: null,
};

beforeEach(() => {
  const events: Event[] = [];
  capturedClient = {
    events,
    submitEvent: (e: Event) => {
      events.push(e);
    },
  };
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  vi.spyOn(clientModule, "getClient").mockReturnValue(capturedClient as any);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  vi.spyOn(contextModule, "currentExecutionContext").mockReturnValue(fakeCtx as any);
});

afterEach(() => {
  vi.restoreAllMocks();
});

function lastPayload(): Record<string, unknown> {
  expect(capturedClient.events.length).toBeGreaterThan(0);
  return capturedClient.events[capturedClient.events.length - 1]
    .payload as Record<string, unknown>;
}

describe("inputSchemaHash", () => {
  test("matches the Python SDK for the cross-SDK fixture", () => {
    expect(inputSchemaHash(CROSS_SDK_SCHEMA)).toBe(CROSS_SDK_EXPECTED_HASH);
  });

  test("is key-order invariant", () => {
    const a = { type: "object", properties: { city: { type: "string" } } };
    const b = { properties: { city: { type: "string" } }, type: "object" };
    expect(inputSchemaHash(a)).toBe(inputSchemaHash(b));
  });

  test("different schemas hash differently", () => {
    const a = { properties: { city: { type: "string" } } };
    const b = { properties: { city: { type: "number" } } };
    expect(inputSchemaHash(a)).not.toBe(inputSchemaHash(b));
  });

  test("null, undefined and unserializable yield empty", () => {
    expect(inputSchemaHash(null)).toBe("");
    expect(inputSchemaHash(undefined)).toBe("");
    expect(inputSchemaHash({ bad: () => 1 })).toBe("");
  });
});

describe("tool() input schema capture", () => {
  test("no declared schema sends no field", async () => {
    const lookup = tool(async (city: string) => ({ city }));
    await lookup("melbourne");
    expect(lastPayload()).not.toHaveProperty("input_schema_hash");
  });

  test("declared schema is hashed onto the payload", async () => {
    const lookup = tool(
      { name: "lookup", inputSchema: CROSS_SDK_SCHEMA },
      async (city: string) => ({ city }),
    );
    await lookup("melbourne");
    expect(lastPayload().input_schema_hash).toBe(CROSS_SDK_EXPECTED_HASH);
  });

  test("schema swapped between calls is visible (the regression shape)", async () => {
    const opts = { name: "lookup", inputSchema: { v: 1 } as unknown };
    const lookup = tool(opts, async (city: string) => ({ city }));
    await lookup("melbourne");
    const first = lastPayload().input_schema_hash;
    opts.inputSchema = { v: 1, ssn: { type: "string" } };
    await lookup("melbourne");
    const second = lastPayload().input_schema_hash;
    expect(first).toBeTruthy();
    expect(second).toBeTruthy();
    expect(first).not.toBe(second);
  });

  test("failure path carries the hash too", async () => {
    const boom = tool(
      { name: "boom", inputSchema: CROSS_SDK_SCHEMA },
      async () => {
        throw new Error("kaput");
      },
    );
    await expect(boom()).rejects.toThrow("kaput");
    expect(lastPayload().input_schema_hash).toBe(CROSS_SDK_EXPECTED_HASH);
  });
});

describe("emitMcpCall input schema capture", () => {
  test("declared schema is hashed onto the payload", () => {
    emitMcpCall("filesystem", "read_file", {
      arguments: { path: "/etc/motd" },
      inputSchema: CROSS_SDK_SCHEMA,
    });
    expect(lastPayload().input_schema_hash).toBe(CROSS_SDK_EXPECTED_HASH);
  });

  test("without a schema the field is absent", () => {
    emitMcpCall("filesystem", "read_file");
    expect(lastPayload()).not.toHaveProperty("input_schema_hash");
  });
});
