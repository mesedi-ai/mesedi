/**
 * Unit tests for the grounding-evaluator integration
 * helpers in mesedi/integrations/{ragas,promptfoo,vectara}.ts.
 *
 * Each helper is a thin wrapper around emitEvalScore. These tests
 * pin the wire-format contract by capturing the emitted Event and
 * asserting the helper produced the right evaluator_id, metric_type,
 * threshold, and pass/fail verdict. If a future refactor changes
 * the evaluator_id strings or flips a higher_is_better flag, the
 * grounding_failure detector will silently misroute scores into the
 * wrong clusters — these tests catch that BEFORE the SDK ships.
 */

import { describe, expect, test, beforeEach, afterEach, vi } from "vitest";

import * as clientModule from "../client.js";
import * as contextModule from "../context.js";
import { Event } from "../events.js";

import * as ragas from "./ragas.js";
import * as promptfoo from "./promptfoo.js";
import * as vectara from "./vectara.js";

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
// Ragas helpers
// ──────────────────────────────────────────────────────────────────────

describe("ragas helpers", () => {
  test("reportFaithfulness emits ragas/faithfulness with passed=true when score >= threshold", () => {
    ragas.reportFaithfulness(0.85);
    expect(capturedClient.events).toHaveLength(1);
    const payload = capturedClient.events[0].payload as Record<string, unknown>;
    expect(payload.evaluator_id).toBe("ragas/faithfulness");
    expect(payload.metric_type).toBe("faithfulness");
    expect(payload.score).toBe(0.85);
    expect(payload.passed).toBe(true);
    expect(payload.threshold).toBe(0.7);
    expect(payload.higher_is_better).toBe(true);
  });

  test("reportFaithfulness with score below threshold emits passed=false", () => {
    ragas.reportFaithfulness(0.4);
    const payload = capturedClient.events[0].payload as Record<string, unknown>;
    expect(payload.passed).toBe(false);
  });

  test("reportFaithfulness honors custom threshold and reason", () => {
    ragas.reportFaithfulness(0.6, { threshold: 0.5, reason: "judge confident" });
    const payload = capturedClient.events[0].payload as Record<string, unknown>;
    expect(payload.threshold).toBe(0.5);
    expect(payload.passed).toBe(true);
    expect(payload.reason).toBe("judge confident");
  });

  test("reportAnswerRelevance emits ragas/answer_relevance", () => {
    ragas.reportAnswerRelevance(0.9);
    const payload = capturedClient.events[0].payload as Record<string, unknown>;
    expect(payload.evaluator_id).toBe("ragas/answer_relevance");
    expect(payload.metric_type).toBe("answer_relevance");
    expect(payload.passed).toBe(true);
  });

  test("reportContextPrecision emits ragas/context_precision", () => {
    ragas.reportContextPrecision(0.6, { threshold: 0.5 });
    const payload = capturedClient.events[0].payload as Record<string, unknown>;
    expect(payload.evaluator_id).toBe("ragas/context_precision");
    expect(payload.metric_type).toBe("context_precision");
    expect(payload.passed).toBe(true);
  });
});

// ──────────────────────────────────────────────────────────────────────
// Promptfoo helpers
// ──────────────────────────────────────────────────────────────────────

describe("promptfoo helpers", () => {
  test("reportFactuality emits promptfoo/factuality with explicit passed flag", () => {
    promptfoo.reportFactuality(0.9, true);
    const payload = capturedClient.events[0].payload as Record<string, unknown>;
    expect(payload.evaluator_id).toBe("promptfoo/factuality");
    expect(payload.metric_type).toBe("factuality");
    expect(payload.passed).toBe(true);
    expect(payload.higher_is_better).toBe(true);
  });

  test("reportFactuality respects explicit passed=false even when score is high", () => {
    promptfoo.reportFactuality(0.95, false, { threshold: 0.99 });
    const payload = capturedClient.events[0].payload as Record<string, unknown>;
    expect(payload.passed).toBe(false);
    expect(payload.score).toBe(0.95);
  });

  test("reportLlmRubric incorporates metric name into evaluator_id and metric_type", () => {
    promptfoo.reportLlmRubric("clarity", 0.75, true);
    const payload = capturedClient.events[0].payload as Record<string, unknown>;
    expect(payload.evaluator_id).toBe("promptfoo/llm-rubric/clarity");
    expect(payload.metric_type).toBe("clarity");
  });

  test("reportLlmRubric throws on empty metricName", () => {
    expect(() => {
      promptfoo.reportLlmRubric("", 0.8, true);
    }).toThrow(/metricName is required/);
  });
});

// ──────────────────────────────────────────────────────────────────────
// Vectara HHEM helper
// ──────────────────────────────────────────────────────────────────────

describe("vectara hhem helper", () => {
  test("reportHhem with high faithfulness score passes", () => {
    vectara.reportHhem(0.9);
    const payload = capturedClient.events[0].payload as Record<string, unknown>;
    expect(payload.evaluator_id).toBe("vectara/hhem");
    expect(payload.metric_type).toBe("hallucination");
    expect(payload.passed).toBe(true);
    // higher_is_better stays true at the wire level; the inversion is
    // the customer's responsibility per the module docstring.
    expect(payload.higher_is_better).toBe(true);
  });

  test("reportHhem with low faithfulness score fails", () => {
    vectara.reportHhem(0.3);
    const payload = capturedClient.events[0].payload as Record<string, unknown>;
    expect(payload.passed).toBe(false);
  });

  test("reportHhem honors custom threshold and reason", () => {
    vectara.reportHhem(0.55, { threshold: 0.6, reason: "judge confident" });
    const payload = capturedClient.events[0].payload as Record<string, unknown>;
    expect(payload.threshold).toBe(0.6);
    expect(payload.passed).toBe(false);
    expect(payload.reason).toBe("judge confident");
  });
});
