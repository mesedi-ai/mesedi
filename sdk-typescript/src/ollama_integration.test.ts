/**
 * Unit tests for Wave 2.5.1 — instrument_ollama TS twin.
 *
 * Mirrors the Python test_ollama_integration.py file: field
 * extraction + idempotent patching. End-to-end "wrap() + chat()
 * emits event" coverage lives in Wave 2.5.6 integration tests.
 */

import { describe, expect, test } from "vitest";

import {
  instrumentOllama,
  _testing,
  type OllamaClassLike,
} from "./ollama_integration.js";

const {
  PROVIDER,
  extractFirstSystemMessage,
  extractLastUserMessage,
  contentToText,
  extractResponseFields,
  numberOrZero,
  truncate,
} = _testing;

// ──────────────────────────────────────────────────────────────────────
// PROVIDER constant stability
// ──────────────────────────────────────────────────────────────────────

describe("PROVIDER constant", () => {
  test("is stable across SDK versions", () => {
    // Backend's provider_incident detector clusters cross-tenant
    // signals on (provider, error_class). Changing this string
    // silently misroutes every Ollama customer's events.
    expect(PROVIDER).toBe("ollama");
  });
});

// ──────────────────────────────────────────────────────────────────────
// extractResponseFields: Ollama → canonical token-field translation
// ──────────────────────────────────────────────────────────────────────

describe("extractResponseFields", () => {
  test("translates prompt_eval_count + eval_count to input_tokens + output_tokens", () => {
    const response = {
      model: "llama3.1:8b",
      message: { role: "assistant", content: "Hello." },
      prompt_eval_count: 23,
      eval_count: 47,
      done: true,
    };
    const { responseText, inputTokens, outputTokens } =
      extractResponseFields(response);
    expect(responseText).toBe("Hello.");
    expect(inputTokens).toBe(23);
    expect(outputTokens).toBe(47);
  });

  test("degrades gracefully when token counts are missing", () => {
    const response = { message: { content: "ok" } };
    const { responseText, inputTokens, outputTokens } =
      extractResponseFields(response);
    expect(responseText).toBe("ok");
    expect(inputTokens).toBe(0);
    expect(outputTokens).toBe(0);
  });

  test("coerces string token counts (defensive)", () => {
    const response = {
      message: { content: "ok" },
      // @ts-expect-error — runtime test of unexpected wire shape
      prompt_eval_count: "12",
      // @ts-expect-error — garbage shouldn't crash the wrapped call
      eval_count: "garbage",
    };
    const { inputTokens, outputTokens } = extractResponseFields(response);
    expect(inputTokens).toBe(12);
    expect(outputTokens).toBe(0);
  });
});

// ──────────────────────────────────────────────────────────────────────
// Message-array walkers (OpenAI-shaped messages)
// ──────────────────────────────────────────────────────────────────────

describe("extractFirstSystemMessage", () => {
  test("returns the FIRST system message even if more follow", () => {
    const msgs = [
      { role: "system", content: "you are a help bot" },
      { role: "user", content: "hi" },
      { role: "system", content: "should not be picked" },
    ];
    expect(extractFirstSystemMessage(msgs)).toBe("you are a help bot");
  });

  test("returns empty string when no system message present", () => {
    expect(extractFirstSystemMessage([{ role: "user", content: "hi" }])).toBe("");
  });
});

describe("extractLastUserMessage", () => {
  test("walks backwards so multi-turn returns the most recent user turn", () => {
    const msgs = [
      { role: "user", content: "first turn" },
      { role: "assistant", content: "ok" },
      { role: "user", content: "second turn" },
      { role: "assistant", content: "still ok" },
      { role: "user", content: "third turn" },
    ];
    expect(extractLastUserMessage(msgs)).toBe("third turn");
  });

  test("returns empty string when no user message present", () => {
    expect(extractLastUserMessage([{ role: "system", content: "hi" }])).toBe("");
  });
});

describe("contentToText", () => {
  test("returns string content unchanged", () => {
    expect(contentToText("plain")).toBe("plain");
  });

  test("joins multimodal text parts with newlines", () => {
    const multimodal = [{ text: "hello" }, { text: "world" }];
    expect(contentToText(multimodal)).toBe("hello\nworld");
  });

  test("treats undefined as empty", () => {
    expect(contentToText(undefined)).toBe("");
  });
});

// ──────────────────────────────────────────────────────────────────────
// numberOrZero: defensive coercion for token-count fields
// ──────────────────────────────────────────────────────────────────────

describe("numberOrZero", () => {
  test("passes through finite numbers", () => {
    expect(numberOrZero(42)).toBe(42);
    expect(numberOrZero(0)).toBe(0);
  });

  test("coerces numeric strings", () => {
    expect(numberOrZero("17")).toBe(17);
  });

  test("returns 0 for non-finite or non-numeric values", () => {
    expect(numberOrZero(undefined)).toBe(0);
    expect(numberOrZero(null)).toBe(0);
    expect(numberOrZero("garbage")).toBe(0);
    expect(numberOrZero(NaN)).toBe(0);
    expect(numberOrZero(Infinity)).toBe(0);
  });
});

// ──────────────────────────────────────────────────────────────────────
// truncate: bounded payload fields
// ──────────────────────────────────────────────────────────────────────

describe("truncate", () => {
  test("returns short strings unchanged", () => {
    expect(truncate("hi", 100)).toBe("hi");
  });

  test("appends marker on overflow", () => {
    const s = "x".repeat(1500);
    const out = truncate(s, 1000);
    expect(out.startsWith("x".repeat(1000))).toBe(true);
    expect(out.endsWith("...[truncated]")).toBe(true);
  });

  test("returns empty for empty input", () => {
    expect(truncate("", 100)).toBe("");
  });
});

// ──────────────────────────────────────────────────────────────────────
// instrumentOllama: idempotent patching via dependency injection
// ──────────────────────────────────────────────────────────────────────

describe("instrumentOllama", () => {
  test("patches the chat method when a class is provided", async () => {
    class FakeOllama {
      async chat(_args: unknown): Promise<unknown> {
        return { message: { content: "from real" } };
      }
    }
    const originalChat = FakeOllama.prototype.chat;
    const result = await instrumentOllama(FakeOllama as unknown as OllamaClassLike);
    expect(result).toBe(true);
    expect(FakeOllama.prototype.chat).not.toBe(originalChat);
  });

  test("is idempotent per class — second call is a no-op", async () => {
    class FakeOllama {
      async chat(_args: unknown): Promise<unknown> {
        return null;
      }
    }
    await instrumentOllama(FakeOllama as unknown as OllamaClassLike);
    const firstPatched = FakeOllama.prototype.chat;
    await instrumentOllama(FakeOllama as unknown as OllamaClassLike);
    expect(FakeOllama.prototype.chat).toBe(firstPatched);
  });
});
