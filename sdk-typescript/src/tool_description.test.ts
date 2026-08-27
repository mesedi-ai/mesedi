/**
 * Tests for the tool_description field on tool() tool_call payloads.
 *
 * WHY THIS EXISTS
 * A tool's contract has two halves: the shape it returns, and the
 * description the model reads when deciding whether and how to call
 * it. Until 2026-08-27 the SDK sent only the first half, so a tool
 * whose description was rewritten to carry injected instructions,
 * with its return shape held byte-identical, produced no signal at
 * all. Verified against production rather than assumed: 50 failure
 * groups before the poisoned call, 50 after.
 *
 * That is the mechanism behind CVE-2026-75130 (Context7 MCP server,
 * published 2026-08-18) and MCP tool poisoning generally. The backend
 * detector can only see it if the SDK sends it.
 *
 * truncateDescription is a private helper, so these test through the
 * public tool() surface and read the submitted event, the same way
 * tool.test.ts does.
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
  return capturedClient.events[capturedClient.events.length - 1].payload as Record<
    string,
    unknown
  >;
}

describe("tool() tool_description field", () => {
  test("description is sent when provided", async () => {
    const fn = tool(
      { name: "lookup_docs", description: "Look up documentation for a library." },
      async () => ({ ok: true }),
    );
    await fn();
    expect(lastPayload().tool_description).toBe(
      "Look up documentation for a library.",
    );
  });

  test("field is ABSENT, not empty, when no description is given", async () => {
    /**
     * This is the rollout-safety case and it is the one most likely to
     * be got wrong.
     *
     * If a missing description arrived as "", that empty string would
     * form a majority baseline on every project running an older SDK.
     * The first call from an upgraded client would then read as drift
     * away from it, and shipping the detector would alert every
     * customer who had not upgraded, on the day they did.
     */
    const fn = tool({ name: "no_desc" }, async () => ({ ok: true }));
    await fn();
    expect("tool_description" in lastPayload()).toBe(false);
  });

  test("whitespace-only description is treated as absent", async () => {
    const fn = tool(
      { name: "blank_desc", description: "   \n\t  " },
      async () => ({ ok: true }),
    );
    await fn();
    expect("tool_description" in lastPayload()).toBe(false);
  });

  test("description is sent on the FAILURE path too", async () => {
    /**
     * A poisoned description is worth seeing even when the call it
     * rode in on threw. Sending it only on success would drop exactly
     * the noisiest attacks, and the backend history query
     * deliberately includes failed calls to match.
     */
    const fn = tool({ name: "flaky", description: "Fetches a thing." }, async () => {
      throw new Error("upstream 500");
    });
    await expect(fn()).rejects.toThrow("upstream 500");
    expect(lastPayload().tool_description).toBe("Fetches a thing.");
  });

  test("long description is truncated and marked", async () => {
    /**
     * The marker matters as much as the cap. Truncation changes the
     * hash, and without an inline marker a truncation-induced change
     * would be indistinguishable from a real edit in the alert.
     */
    const fn = tool(
      { name: "verbose", description: "x".repeat(2500) },
      async () => ({ ok: true }),
    );
    await fn();
    const desc = lastPayload().tool_description as string;
    expect(desc.endsWith("...[truncated]")).toBe(true);
    expect(desc.length).toBe(2000 + "...[truncated]".length);
  });

  test("description at exactly the cap is not truncated", async () => {
    // Off-by-one guard: a description sitting on the boundary must
    // hash stably rather than flipping between truncated and not.
    const fn = tool(
      { name: "boundary", description: "y".repeat(2000) },
      async () => ({ ok: true }),
    );
    await fn();
    const desc = lastPayload().tool_description as string;
    expect(desc).toBe("y".repeat(2000));
    expect(desc).not.toContain("[truncated]");
  });

  test("adding a description does not disturb return_value", async () => {
    /**
     * The two halves of the contract are fingerprinted independently
     * on the backend. If adding a description perturbed return_value,
     * every instrumented tool would appear to drift once on upgrade.
     */
    const fn = tool(
      { name: "priced", description: "Returns a priced item." },
      async () => ({ id: "a", price: 1.99 }),
    );
    await fn();
    expect(lastPayload().return_value).toEqual({ id: "a", price: 1.99 });
  });
});
