/**
 * End-to-end smoke test for the Vercel AI SDK adapter.
 *
 * Doesn't require the `ai` package or any provider package — uses a
 * mock `generateText` function that returns Vercel-shaped results.
 * Verifies the adapter end-to-end: a wrap()'d entry point that uses
 * wrapGenerateText() should produce one llm_call event per step
 * plus one tool_call event per tool invocation in each step, with
 * the same wire format as hand-written Mesedi instrumentation.
 *
 * Prereqs:
 *   - Backend running on :8080
 *   - Package built: cd sdk-typescript && npm run build
 *
 * Run (after build):
 *   node sandbox/vercel_ai_adapter_test.js
 *
 * Verify at http://localhost:8080/ui/:
 *   - Execution 'vercel_ai_smoke' completed.
 *   - Timeline (4 events) in order:
 *       llm_call   gpt-4o · 100 in / 8 out  (step 1)
 *       tool_call  search_web · status=ok
 *       llm_call   gpt-4o · 80 in / 12 out  (step 2 — final answer)
 *       tool_call  lookup · status=failed
 */

import { configure, flush, wrap } from "../src/index.js";
import { wrapGenerateText } from "../src/integrations/vercel_ai.js";

configure({
  apiKey: "mesedi_sk_dev_local_only",
  baseUrl: "http://localhost:8080",
});

// ─────────────────────────────────────────────────────────────────────
// Mock Vercel `generateText`. Returns a multi-step result mimicking
// what a real `generateText({ maxSteps: 5, tools: { ... } })` call
// would return on a successful ReAct loop with two intermediate
// tool calls (one ok, one failed).
// ─────────────────────────────────────────────────────────────────────

interface MockOptions {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  model?: any;
  prompt?: string;
  system?: string;
  [key: string]: unknown;
}

async function mockGenerateText(opts: MockOptions) {
  void opts; // silence unused-param under strict tsconfig — real impl reads opts
  // Simulate a 25ms LLM round-trip.
  await new Promise((r) => setTimeout(r, 25));
  return {
    text: "The capital of France is Paris.",
    finishReason: "stop",
    usage: { inputTokens: 180, outputTokens: 20 },
    toolCalls: [],
    toolResults: [],
    steps: [
      {
        text: "I should look up the capital and verify with a search.",
        usage: { inputTokens: 100, outputTokens: 8 },
        toolCalls: [
          { toolCallId: "tc-1", toolName: "search_web", args: { query: "capital of France" } },
        ],
        toolResults: [
          { toolCallId: "tc-1", toolName: "search_web", result: "Paris is the capital of France." },
        ],
      },
      {
        text: "The capital of France is Paris.",
        usage: { inputTokens: 80, outputTokens: 12 },
        toolCalls: [
          { toolCallId: "tc-2", toolName: "lookup", args: { entity: "Eiffel Tower" } },
        ],
        toolResults: [
          {
            toolCallId: "tc-2",
            toolName: "lookup",
            result: { error: { name: "NotFoundError", message: "entity not in cache" } },
          },
        ],
      },
    ],
  };
}

// ─────────────────────────────────────────────────────────────────────
// Smoke driver
// ─────────────────────────────────────────────────────────────────────

const generateTextM = wrapGenerateText(mockGenerateText);

const runSmoke = wrap(
  { name: "vercel_ai_smoke" },
  async (question: string): Promise<string> => {
    const result = await generateTextM({
      // Fake model object — wrapGenerateText reads .modelId.
      model: { modelId: "gpt-4o", id: "gpt-4o" },
      system: "You answer geography questions concisely.",
      prompt: question,
    });
    return result.text || "";
  },
);

async function main(): Promise<void> {
  const answer = await runSmoke("What is the capital of France?");
  console.log(`smoke result: ${answer}`);
  await flush(5000);
  console.log("flushed; inspect http://localhost:8080/ui/");
}

main().catch((err) => {
  console.error("smoke FAILED:", err);
  process.exit(1);
});
