/**
 * Unit tests for the LangChain.js callback handler (Wave 4.C).
 *
 * Strategy: mock the MesediClient module so the emitters' getClient()
 * call returns a stub that captures every submitEvent call. Wrap each
 * test in `runInExecutionContext` so the handler's emissions land
 * (outside a wrap context the handler no-ops, which is separately
 * tested).
 *
 * Integration coverage (real LangChain agent → real Mesedi backend)
 * is out of scope for this wave and ships in the synthetic-customer
 * repo as a follow-up.
 */

import { beforeEach, describe, expect, test, vi } from "vitest";

import { runInExecutionContext } from "../context.js";
import { Event, Execution } from "../events.js";

// ── Stub client ──────────────────────────────────────────────────────
//
// vi.mock hoists to the top of the file, so the captures object has
// to be created inside vi.hoisted() to share the same hoisting phase.
// The emitters' getClient() call then hits the stub, which appends to
// these shared arrays.

type Captures = {
  starts: Execution[];
  ends: Execution[];
  events: Event[];
};

const captures = vi.hoisted<Captures>(() => ({
  starts: [],
  ends: [],
  events: [],
}));

vi.mock("../client.js", () => ({
  getClient: () => ({
    submitExecutionStart: (e: Execution) => captures.starts.push(e),
    submitExecutionEnd: (e: Execution) => captures.ends.push(e),
    submitEvent: (ev: Event) => captures.events.push(ev),
  }),
}));

// Import AFTER the mock so the handler picks up the stubbed client.
import { MesediLangChainCallbackHandler } from "./langchain.js";

function getCaps(): Captures {
  return captures;
}

function resetCaps(): void {
  const caps = getCaps();
  caps.starts.length = 0;
  caps.ends.length = 0;
  caps.events.length = 0;
}

// Wrap a test body in a synthetic execution context so emitters land.
async function inCtx<T>(fn: () => Promise<T> | T): Promise<T> {
  return runInExecutionContext("exec-test000000", async () => await fn());
}

beforeEach(() => {
  resetCaps();
});

// ──────────────────────────────────────────────────────────────────────
// LLM lifecycle — plain (non-chat) model
// ──────────────────────────────────────────────────────────────────────

describe("MesediLangChainCallbackHandler — LLM (plain)", () => {
  test("handleLLMStart → handleLLMEnd emits ok llm_call with tokens", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleLLMStart(
        { name: "OpenAI", kwargs: { model: "gpt-4o" } },
        ["What is 2+2?"],
        "run-1",
      );
      h.handleLLMEnd(
        {
          generations: [[{ text: "4" }]],
          llmOutput: { tokenUsage: { promptTokens: 10, completionTokens: 1 } },
        },
        "run-1",
      );

      const caps = getCaps();
      expect(caps.events).toHaveLength(1);
      const evt = caps.events[0]!;
      expect(evt.event_type).toBe("llm_call");
      expect(evt.payload["model"]).toBe("gpt-4o");
      expect(evt.payload["user_message"]).toBe("What is 2+2?");
      expect(evt.payload["response_text"]).toBe("4");
      expect(evt.payload["input_tokens"]).toBe(10);
      expect(evt.payload["output_tokens"]).toBe(1);
      expect(evt.payload["status"]).toBe("ok");
      expect(evt.duration_ms).toBeGreaterThanOrEqual(0);
    });
  });

  test("handleLLMStart → handleLLMError emits failed llm_call", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleLLMStart(
        { name: "OpenAI", kwargs: { model: "gpt-4o" } },
        ["Prompt"],
        "run-e",
      );
      h.handleLLMError(new Error("rate limited"), "run-e");

      const caps = getCaps();
      expect(caps.events).toHaveLength(1);
      const evt = caps.events[0]!;
      expect(evt.payload["status"]).toBe("failed");
      expect(evt.payload["model"]).toBe("gpt-4o");
      // Failed-path payloads exclude response_text and token counts,
      // matching vercel_ai.ts / emit_llm_call.
      expect(evt.payload["response_text"]).toBeUndefined();
      expect(evt.payload["input_tokens"]).toBeUndefined();
    });
  });

  test("handleLLMEnd without matching start is a no-op", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleLLMEnd({ generations: [[{ text: "x" }]] }, "run-nomatch");
      expect(getCaps().events).toHaveLength(0);
    });
  });
});

// ──────────────────────────────────────────────────────────────────────
// LLM lifecycle — chat model
// ──────────────────────────────────────────────────────────────────────

describe("MesediLangChainCallbackHandler — chat model", () => {
  test("handleChatModelStart extracts user + system messages", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      const messages = [
        [
          { type: "system", content: "You are a helpful assistant." },
          { type: "human", content: "Hi." },
        ],
      ];
      h.handleChatModelStart(
        { kwargs: { model: "claude-sonnet-4-6" } },
        messages,
        "run-chat",
      );
      h.handleLLMEnd(
        {
          generations: [[{ message: { content: "Hello!" } }]],
          llmOutput: { tokenUsage: { promptTokens: 5, completionTokens: 2 } },
        },
        "run-chat",
      );

      const caps = getCaps();
      expect(caps.events).toHaveLength(1);
      const p = caps.events[0]!.payload;
      expect(p["model"]).toBe("claude-sonnet-4-6");
      expect(p["user_message"]).toBe("Hi.");
      expect(p["system_prompt"]).toBe("You are a helpful assistant.");
      expect(p["response_text"]).toBe("Hello!");
    });
  });

  test("chat model handles multi-modal content list (text blocks only)", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      const messages = [
        [
          {
            type: "human",
            content: [
              { type: "text", text: "Describe:" },
              { type: "image_url", image_url: "..." },
              { type: "text", text: "this image" },
            ],
          },
        ],
      ];
      h.handleChatModelStart({ kwargs: { model: "gpt-4o" } }, messages, "run-mm");
      h.handleLLMEnd({ generations: [[{ text: "cat" }]] }, "run-mm");

      const caps = getCaps();
      expect(caps.events[0]?.payload["user_message"]).toBe(
        "Describe: this image",
      );
    });
  });

  test("token usage from usage_metadata on the generation", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleChatModelStart(
        { kwargs: { model: "claude-haiku-4-5" } },
        [[{ type: "human", content: "hi" }]],
        "run-um",
      );
      h.handleLLMEnd(
        {
          generations: [
            [
              {
                message: {
                  content: "hello",
                  usage_metadata: { input_tokens: 7, output_tokens: 3 },
                },
              },
            ],
          ],
        },
        "run-um",
      );

      const caps = getCaps();
      expect(caps.events[0]?.payload["input_tokens"]).toBe(7);
      expect(caps.events[0]?.payload["output_tokens"]).toBe(3);
    });
  });
});

// ──────────────────────────────────────────────────────────────────────
// Tool lifecycle
// ──────────────────────────────────────────────────────────────────────

describe("MesediLangChainCallbackHandler — tools", () => {
  test("handleToolStart → handleToolEnd emits ok tool_call", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleToolStart({ name: "search" }, "cats", "tool-1");
      h.handleToolEnd("42 results", "tool-1");

      const caps = getCaps();
      expect(caps.events).toHaveLength(1);
      const evt = caps.events[0]!;
      expect(evt.event_type).toBe("tool_call");
      expect(evt.payload["tool_name"]).toBe("search");
      expect(evt.payload["status"]).toBe("ok");
      expect(evt.payload["result_summary"]).toBe("42 results");
      const args = evt.payload["arguments"] as {
        args: string[];
        kwargs: Record<string, string>;
      };
      expect(args.args).toEqual(["cats"]);
      expect(args.kwargs).toEqual({});
    });
  });

  test("handleToolError emits failed tool_call with exception fields", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleToolStart({ name: "risky" }, "input", "tool-e");
      class CustomError extends Error {}
      h.handleToolError(new CustomError("boom"), "tool-e");

      const caps = getCaps();
      const evt = caps.events[0]!;
      expect(evt.payload["status"]).toBe("failed");
      expect(evt.payload["exception_type"]).toBe("CustomError");
      expect(evt.payload["exception_message"]).toBe("boom");
      expect(evt.payload["result_summary"]).toBeUndefined();
    });
  });

  test("tool name falls back to id chain then unknown_tool", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleToolStart(
        { id: ["langchain", "tools", "MyTool"] },
        "x",
        "tool-fb1",
      );
      h.handleToolEnd("done", "tool-fb1");
      h.handleToolStart(undefined, "y", "tool-fb2");
      h.handleToolEnd("done", "tool-fb2");

      const caps = getCaps();
      expect(caps.events[0]?.payload["tool_name"]).toBe("MyTool");
      expect(caps.events[1]?.payload["tool_name"]).toBe("unknown_tool");
    });
  });
});

// ──────────────────────────────────────────────────────────────────────
// Context / fail-open / truncation
// ──────────────────────────────────────────────────────────────────────

describe("MesediLangChainCallbackHandler — context + fail-open", () => {
  test("outside a wrap execution context, all emits are no-ops", () => {
    const h = new MesediLangChainCallbackHandler();
    h.handleLLMStart({ kwargs: { model: "x" } }, ["p"], "no-ctx");
    h.handleLLMEnd({ generations: [[{ text: "r" }]] }, "no-ctx");
    h.handleToolStart({ name: "t" }, "i", "no-ctx-t");
    h.handleToolEnd("o", "no-ctx-t");
    expect(getCaps().events).toHaveLength(0);
  });

  test("truncates oversized fields to matching wire budgets", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      const bigPrompt = "a".repeat(1500);
      const bigResponse = "b".repeat(1500);
      h.handleLLMStart(
        { kwargs: { model: "gpt-4o" } },
        [bigPrompt],
        "big",
      );
      h.handleLLMEnd(
        {
          generations: [[{ text: bigResponse }]],
          llmOutput: { tokenUsage: { promptTokens: 1, completionTokens: 1 } },
        },
        "big",
      );

      const p = getCaps().events[0]!.payload;
      expect((p["user_message"] as string).length).toBe(1000);
      expect((p["response_text"] as string).length).toBe(1000);
      expect((p["user_message"] as string).endsWith("...")).toBe(true);
    });
  });

  test("truncates oversized tool input + output", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      const bigInput = "x".repeat(500);
      const bigOutput = "y".repeat(2000);
      h.handleToolStart({ name: "big-tool" }, bigInput, "tool-big");
      h.handleToolEnd(bigOutput, "tool-big");

      const p = getCaps().events[0]!.payload;
      const args = p["arguments"] as { args: string[] };
      expect(args.args[0]!.length).toBe(200);
      expect((p["result_summary"] as string).length).toBe(500);
    });
  });

  test("sequence numbers are monotonic per execution context", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleLLMStart({ kwargs: { model: "m" } }, ["a"], "r1");
      h.handleLLMEnd({ generations: [[{ text: "b" }]] }, "r1");
      h.handleToolStart({ name: "t" }, "i", "r2");
      h.handleToolEnd("o", "r2");

      const seqs = getCaps().events.map((e) => e.sequence);
      expect(seqs).toEqual([1, 2]);
    });
  });

  test("concurrent runs stay paired by runId", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleLLMStart({ kwargs: { model: "A" } }, ["prompt-A"], "run-A");
      h.handleLLMStart({ kwargs: { model: "B" } }, ["prompt-B"], "run-B");
      h.handleLLMEnd({ generations: [[{ text: "resp-B" }]] }, "run-B");
      h.handleLLMEnd({ generations: [[{ text: "resp-A" }]] }, "run-A");

      const events = getCaps().events;
      expect(events).toHaveLength(2);
      // First end was for run-B — payload should carry model B.
      expect(events[0]?.payload["model"]).toBe("B");
      expect(events[0]?.payload["response_text"]).toBe("resp-B");
      // Second end was for run-A.
      expect(events[1]?.payload["model"]).toBe("A");
      expect(events[1]?.payload["response_text"]).toBe("resp-A");
    });
  });

  test("model fallback: kwargs.model → id chain → name → unknown", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleLLMStart(
        { id: ["langchain", "chat_models", "ChatOpenAI"] },
        ["p"],
        "m1",
      );
      h.handleLLMEnd({ generations: [[{ text: "r" }]] }, "m1");
      h.handleLLMStart({ name: "MyLLM" }, ["p"], "m2");
      h.handleLLMEnd({ generations: [[{ text: "r" }]] }, "m2");
      h.handleLLMStart(undefined, ["p"], "m3");
      h.handleLLMEnd({ generations: [[{ text: "r" }]] }, "m3");

      const events = getCaps().events;
      expect(events[0]?.payload["model"]).toBe("ChatOpenAI");
      expect(events[1]?.payload["model"]).toBe("MyLLM");
      expect(events[2]?.payload["model"]).toBe("unknown");
    });
  });
});

// ──────────────────────────────────────────────────────────────────────
// SW#280-3.a: Provider + error_class + structured return_value
// ──────────────────────────────────────────────────────────────────────
//
// Backend detectors need three fields the pre-SW#280-3 handler dropped:
//   - `provider` on every llm_call (provider_incident clustering)
//   - `error_class` on every failed llm_call (provider_incident bucket)
//   - `return_value` structured shape on tool_call (tool_schema_drift)
//
// These tests lock in each field's presence and content shape.

describe("MesediLangChainCallbackHandler — provider extraction", () => {
  test("Anthropic serialized.id → provider=anthropic on ok llm_call", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleChatModelStart(
        {
          id: ["langchain", "chat_models", "anthropic", "ChatAnthropic"],
          kwargs: { model: "claude-sonnet-4-6" },
        },
        [[{ type: "human", content: "hi" }]],
        "run-anth",
      );
      h.handleLLMEnd({ generations: [[{ text: "hello" }]] }, "run-anth");

      const p = getCaps().events[0]!.payload;
      expect(p["provider"]).toBe("anthropic");
    });
  });

  test("OpenAI serialized.id → provider=openai", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleLLMStart(
        {
          id: ["langchain", "chat_models", "openai", "ChatOpenAI"],
          kwargs: { model: "gpt-4o" },
        },
        ["p"],
        "run-oai",
      );
      h.handleLLMEnd({ generations: [[{ text: "r" }]] }, "run-oai");

      const p = getCaps().events[0]!.payload;
      expect(p["provider"]).toBe("openai");
    });
  });

  test("scoped package id (@langchain/anthropic) → provider=anthropic", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleChatModelStart(
        {
          id: ["@langchain/anthropic", "chat_models", "ChatAnthropic"],
          kwargs: { model: "claude-haiku-4-5" },
        },
        [[{ type: "human", content: "hi" }]],
        "run-scoped",
      );
      h.handleLLMEnd({ generations: [[{ text: "ok" }]] }, "run-scoped");

      const p = getCaps().events[0]!.payload;
      expect(p["provider"]).toBe("anthropic");
    });
  });

  test("class-name-only fallback (ChatCohere) → provider=cohere", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleChatModelStart(
        {
          id: ["langchain", "chat_models", "ChatCohere"],
          kwargs: { model: "command-r-plus" },
        },
        [[{ type: "human", content: "hi" }]],
        "run-cohere",
      );
      h.handleLLMEnd({ generations: [[{ text: "ok" }]] }, "run-cohere");

      const p = getCaps().events[0]!.payload;
      expect(p["provider"]).toBe("cohere");
    });
  });

  test("unrecognized serialized → provider=unknown", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleLLMStart(
        {
          id: ["langchain", "chat_models", "SomethingCustom"],
          kwargs: { model: "custom-model" },
        },
        ["p"],
        "run-unk",
      );
      h.handleLLMEnd({ generations: [[{ text: "r" }]] }, "run-unk");

      const p = getCaps().events[0]!.payload;
      expect(p["provider"]).toBe("unknown");
    });
  });
});

describe("MesediLangChainCallbackHandler — error_class classification", () => {
  test("Anthropic RateLimitError → error_class=rate_limited", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleChatModelStart(
        {
          id: ["langchain", "chat_models", "anthropic", "ChatAnthropic"],
          kwargs: { model: "claude-sonnet-4-6" },
        },
        [[{ type: "human", content: "hi" }]],
        "run-rl",
      );
      class RateLimitError extends Error {}
      h.handleLLMError(new RateLimitError("429"), "run-rl");

      const p = getCaps().events[0]!.payload;
      expect(p["status"]).toBe("failed");
      expect(p["provider"]).toBe("anthropic");
      expect(p["error_class"]).toBe("rate_limited");
      expect(p["exception_type"]).toBe("RateLimitError");
    });
  });

  test("APITimeoutError on Anthropic → error_class=timeout", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleChatModelStart(
        {
          id: ["langchain", "chat_models", "anthropic", "ChatAnthropic"],
          kwargs: { model: "claude-sonnet-4-6" },
        },
        [[{ type: "human", content: "hi" }]],
        "run-to",
      );
      class APITimeoutError extends Error {}
      h.handleLLMError(new APITimeoutError("deadline"), "run-to");

      const p = getCaps().events[0]!.payload;
      expect(p["error_class"]).toBe("timeout");
    });
  });

  test("cross-provider fallback: unknown provider still classifies known exception name", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleLLMStart(
        { kwargs: { model: "mystery-1" } },
        ["p"],
        "run-fb",
      );
      class RateLimitError extends Error {}
      h.handleLLMError(new RateLimitError("boom"), "run-fb");

      const p = getCaps().events[0]!.payload;
      expect(p["provider"]).toBe("unknown");
      expect(p["error_class"]).toBe("rate_limited");
    });
  });

  test("truly unknown exception → error_class=unknown", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleLLMStart(
        { kwargs: { model: "x" } },
        ["p"],
        "run-unk-e",
      );
      class TotallyMadeUpError extends Error {}
      h.handleLLMError(new TotallyMadeUpError("weird"), "run-unk-e");

      const p = getCaps().events[0]!.payload;
      expect(p["error_class"]).toBe("unknown");
    });
  });

  test("wrapped exception via err.cause is unwrapped for classification", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleChatModelStart(
        {
          id: ["langchain", "chat_models", "anthropic", "ChatAnthropic"],
          kwargs: { model: "claude-sonnet-4-6" },
        },
        [[{ type: "human", content: "hi" }]],
        "run-wrap",
      );
      class RateLimitError extends Error {}
      const wrapped: Error & { cause?: unknown } = new Error(
        "wrapped by langchain",
      );
      wrapped.cause = new RateLimitError("underlying");
      h.handleLLMError(wrapped, "run-wrap");

      const p = getCaps().events[0]!.payload;
      expect(p["error_class"]).toBe("rate_limited");
    });
  });

  test("failed llm_call carries http_status when exception exposes .status", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleChatModelStart(
        {
          id: ["langchain", "chat_models", "anthropic", "ChatAnthropic"],
          kwargs: { model: "claude-sonnet-4-6" },
        },
        [[{ type: "human", content: "hi" }]],
        "run-http",
      );
      class InternalServerError extends Error {
        status = 500;
      }
      h.handleLLMError(new InternalServerError("5xx"), "run-http");

      const p = getCaps().events[0]!.payload;
      expect(p["http_status"]).toBe(500);
      expect(p["error_class"]).toBe("internal_error");
    });
  });

  test("failed llm_call surfaces retry_after when exception exposes it", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleChatModelStart(
        {
          id: ["langchain", "chat_models", "anthropic", "ChatAnthropic"],
          kwargs: { model: "claude-sonnet-4-6" },
        },
        [[{ type: "human", content: "hi" }]],
        "run-ra",
      );
      class RateLimitError extends Error {
        retryAfter = 45;
      }
      h.handleLLMError(new RateLimitError("429"), "run-ra");

      const p = getCaps().events[0]!.payload;
      expect(p["retry_after"]).toBe(45);
    });
  });
});

describe("MesediLangChainCallbackHandler — structured tool_call return_value", () => {
  test("object return emits structured return_value alongside result_summary", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleToolStart({ name: "get_user" }, "42", "tool-obj");
      h.handleToolEnd({ id: 42, email: "a@b.com", active: true }, "tool-obj");

      const p = getCaps().events[0]!.payload;
      // Structured return preserves shape so tool_schema_drift can
      // fingerprint fields.
      expect(p["return_value"]).toEqual({
        id: 42,
        email: "a@b.com",
        active: true,
      });
      // result_summary still present for the dashboard UI.
      expect(typeof p["result_summary"]).toBe("string");
    });
  });

  test("JSON-string return is parsed into structured return_value", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleToolStart({ name: "search" }, "cats", "tool-js");
      h.handleToolEnd(
        JSON.stringify({ hits: 3, results: ["a", "b", "c"] }),
        "tool-js",
      );

      const p = getCaps().events[0]!.payload;
      expect(p["return_value"]).toEqual({
        hits: 3,
        results: ["a", "b", "c"],
      });
    });
  });

  test("plain-text return has no return_value coercion (string preserved as-is)", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleToolStart({ name: "echo" }, "hello", "tool-plain");
      h.handleToolEnd("just plain text, not json", "tool-plain");

      const p = getCaps().events[0]!.payload;
      // Falls into the structured return_value slot as a bare string.
      // What matters for tool_schema_drift is that the SHAPE stays
      // stable ('string') across calls; the string content itself is
      // caps-safe under MAX_RETURN_VALUE_JSON.
      expect(p["return_value"]).toBe("just plain text, not json");
    });
  });

  test("failed tool_call omits return_value entirely", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleToolStart({ name: "risky" }, "input", "tool-f");
      h.handleToolError(new Error("boom"), "tool-f");

      const p = getCaps().events[0]!.payload;
      expect(p["status"]).toBe("failed");
      expect(p["return_value"]).toBeUndefined();
    });
  });

  test("JSON-string tool input is parsed into structured arguments", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleToolStart(
        { name: "query" },
        JSON.stringify({ q: "widgets", limit: 5 }),
        "tool-jsargs",
      );
      h.handleToolEnd("done", "tool-jsargs");

      const p = getCaps().events[0]!.payload;
      const args = p["arguments"] as { args: unknown[]; kwargs: unknown };
      // The single positional slot now carries the parsed object so
      // tool_schema_drift can fingerprint the argument shape.
      expect(args.args[0]).toEqual({ q: "widgets", limit: 5 });
    });
  });

  test("plain-string tool input keeps the truncated string branch", async () => {
    await inCtx(async () => {
      const h = new MesediLangChainCallbackHandler();
      h.handleToolStart({ name: "search" }, "cats", "tool-plainargs");
      h.handleToolEnd("done", "tool-plainargs");

      const p = getCaps().events[0]!.payload;
      const args = p["arguments"] as { args: string[]; kwargs: unknown };
      // Non-JSON input stays a string in args[0] — schema stability
      // for plain-text tools.
      expect(args.args[0]).toBe("cats");
    });
  });
});
