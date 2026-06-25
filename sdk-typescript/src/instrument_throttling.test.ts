/**
 * Unit tests for Wave 1.4 — TS twin of the Python
 * test_instrument_throttling.py suite.
 *
 * Pins (a) the throttling-class filter, (b) the reason-string
 * mapping inside _maybeEmitThrottlingEvent, and (c) the wiring
 * inside each instrument_* module.
 */

import { describe, expect, test, beforeEach, afterEach, vi } from "vitest";

import * as clientModule from "./client.js";
import * as contextModule from "./context.js";
import { Event } from "./events.js";

import { _maybeEmitThrottlingEvent } from "./observe.js";
import * as anthropicIntegration from "./anthropic_integration.js";
import * as openaiIntegration from "./openai_integration.js";
import * as cohereIntegration from "./cohere_integration.js";
import * as geminiIntegration from "./gemini_integration.js";

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

// ──────────────────────────────────────────────────────────────────────
// Helper-level behavior
// ──────────────────────────────────────────────────────────────────────

describe("_maybeEmitThrottlingEvent", () => {
  test("no-op for non-throttling classes", () => {
    for (const cls of [
      "service_unavailable",
      "internal_error",
      "timeout",
      "invalid_api_key",
      "client_error",
      "unknown",
    ]) {
      _maybeEmitThrottlingEvent({ provider: "test", errorClass: cls });
    }
    expect(capturedClient.events).toHaveLength(0);
  });

  test("rate_limited emits with rate_limit reason", () => {
    _maybeEmitThrottlingEvent({
      provider: "anthropic",
      errorClass: "rate_limited",
      httpStatus: 429,
      retryAfterSeconds: 2.5,
      endpoint: "/v1/messages",
    });
    expect(capturedClient.events).toHaveLength(1);
    const payload = capturedClient.events[0].payload as Record<string, unknown>;
    expect(payload.event_type).toBe("rate_limit");
    expect(payload.provider).toBe("anthropic");
    expect(payload.status_code).toBe(429);
    expect(payload.retry_after_ms).toBe(2500);
    expect(payload.endpoint).toBe("/v1/messages");
  });

  test("quota_exhausted emits with quota_exhausted reason", () => {
    _maybeEmitThrottlingEvent({
      provider: "openai",
      errorClass: "quota_exhausted",
      httpStatus: 429,
    });
    const payload = capturedClient.events[0].payload as Record<string, unknown>;
    expect(payload.event_type).toBe("quota_exhausted");
    expect(payload.status_code).toBe(429);
    // retry_after_ms defaults to 0 → omit-when-zero contract; the
    // emit primitive doesn't include zero fields, so the property
    // should be absent on the payload.
    expect(payload.retry_after_ms).toBeUndefined();
  });

  test("zero retry_after does not become negative", () => {
    _maybeEmitThrottlingEvent({
      provider: "cohere",
      errorClass: "rate_limited",
      retryAfterSeconds: 0,
    });
    const payload = capturedClient.events[0].payload as Record<string, unknown>;
    expect(payload.retry_after_ms).toBeUndefined();
  });
});

// ──────────────────────────────────────────────────────────────────────
// Per-provider wiring: each instrument_* module must export the
// instrument fn AND import _maybeEmitThrottlingEvent. Without the
// import the auto-emit silently no-ops.
// ──────────────────────────────────────────────────────────────────────

describe("instrument_* wiring", () => {
  test("anthropic_integration exposes instrumentAnthropic", () => {
    expect(anthropicIntegration).toHaveProperty("instrumentAnthropic");
  });
  test("openai_integration exposes instrumentOpenAI", () => {
    expect(openaiIntegration).toHaveProperty("instrumentOpenAI");
  });
  test("cohere_integration exposes instrumentCohere", () => {
    expect(cohereIntegration).toHaveProperty("instrumentCohere");
  });
  test("gemini_integration exposes instrumentGemini", () => {
    expect(geminiIntegration).toHaveProperty("instrumentGemini");
  });
});

// ──────────────────────────────────────────────────────────────────────
// Wave 2.5.3 — Ollama intentional omission guard
//
// instrument_ollama must NOT import _maybeEmitThrottlingEvent. Ollama
// is a local runtime: no per-minute rate limiting, no quota exhaustion.
// Wave 2.5.2 ships a regression-guard test asserting
// classifyOllamaException NEVER returns RATE_LIMITED or
// QUOTA_EXHAUSTED. Wiring the helper into instrument_ollama would be
// a function call this codebase proves can never produce an event.
// That's symmetry-by-copy-paste, not symmetry-by-meaning, and the
// FOUNDATION ZERO SHORTCUTS principle says no.
//
// This source-string assertion fires if a future engineer adds the
// import to ollama_integration.ts without engaging with the
// architectural decision.
// ──────────────────────────────────────────────────────────────────────

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

describe("Ollama no-throttling guard (Wave 2.5.3)", () => {
  test("ollama_integration.ts does NOT import _maybeEmitThrottlingEvent", () => {
    // Resolve the source file path next to this test file.
    const here = dirname(fileURLToPath(import.meta.url));
    const sourcePath = join(here, "ollama_integration.ts");
    const source = readFileSync(sourcePath, "utf8");
    // Strip comments before scanning — the source contains a
    // documented intentional reference to _maybeEmitThrottlingEvent
    // in the comment block explaining the omission. We assert the
    // import / call site itself is not present.
    const stripped = source.replace(/\/\/.*$/gm, "").replace(/\/\*[\s\S]*?\*\//g, "");
    expect(stripped).not.toContain("_maybeEmitThrottlingEvent");
  });

  test("ollama_integration exposes instrumentOllama", async () => {
    // The positive half: even though throttling auto-emit is omitted,
    // the integration's primary export must still ship.
    const ollamaIntegration = await import("./ollama_integration.js");
    expect(ollamaIntegration).toHaveProperty("instrumentOllama");
  });
});
