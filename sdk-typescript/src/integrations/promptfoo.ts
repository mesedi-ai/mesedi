/**
 * Promptfoo evaluator one-liner helpers (Mesedi #14, Wave 1.3).
 *
 * Promptfoo (https://www.promptfoo.dev) is a CLI + library for
 * prompt evaluation. Customers using its assertion types
 * (model-graded-factuality, llm-rubric, ...) can use these helpers
 * to report verdicts into Mesedi without hand-writing the
 * boilerplate around {@link emitEvalScore}.
 *
 * Report-only design: Mesedi does NOT import the `promptfoo` package
 * as a dependency. The customer runs Promptfoo on their side; these
 * helpers accept the resulting score/verdict and emit a
 * correctly-shaped `eval_score` event.
 *
 * Typical usage:
 *
 *     import { wrap } from "mesedi";
 *     import { reportFactuality } from "mesedi/integrations/promptfoo";
 *
 *     export const myAgent = wrap(async (question: string) => {
 *       const answer = await runAgent(question);
 *       const verdict = await promptfooAssertFactuality(answer, expectedFacts);
 *       reportFactuality(verdict.score, verdict.pass);
 *       return answer;
 *     });
 *
 * Helpers cover the two most common Promptfoo assertion shapes. For
 * other assertion types (similar, contains, equals, ...) call
 * {@link emitEvalScore} directly with
 * `evaluatorId: "promptfoo/<assertion>"`.
 */

import { emitEvalScore } from "../observe.js";

export interface PromptfooReportOptions {
  /** Threshold the Promptfoo assertion used. Default 0.5. */
  threshold?: number;
  /** Optional explanation the judge returned. */
  reason?: string;
}

/**
 * Report a Promptfoo `model-graded-factuality` assertion.
 *
 * Promptfoo's model-graded factuality assertion uses an LLM judge
 * to score how factually accurate the answer is. Higher is better.
 */
export function reportFactuality(
  score: number,
  passed: boolean,
  opts: PromptfooReportOptions = {},
): void {
  const threshold = opts.threshold ?? 0.5;
  emitEvalScore(
    "promptfoo/factuality",
    "factuality",
    score,
    passed,
    {
      threshold,
      higherIsBetter: true,
      reason: opts.reason,
    },
  );
}

/**
 * Report a Promptfoo `llm-rubric` assertion.
 *
 * The llm-rubric assertion runs a customer-defined rubric through
 * an LLM judge. Because a single execution can run multiple rubrics
 * against the same output, `metricName` distinguishes them so
 * grounding_failure clusters per-rubric instead of collapsing every
 * rubric into one bucket.
 *
 * @param metricName Short identifier for the specific rubric
 *   ("clarity", "helpfulness", "tone_appropriate", etc.). Becomes
 *   part of the eval_score's metric_type field.
 * @param score The judge's numeric score for this rubric.
 * @param passed Promptfoo's own pass/fail verdict for this rubric.
 * @param opts Threshold + reason.
 */
export function reportLlmRubric(
  metricName: string,
  score: number,
  passed: boolean,
  opts: PromptfooReportOptions = {},
): void {
  if (!metricName) {
    throw new Error(
      "reportLlmRubric: metricName is required so the " +
        "grounding_failure detector can cluster per-rubric " +
        "instead of collapsing every rubric into one bucket.",
    );
  }
  const threshold = opts.threshold ?? 0.5;
  emitEvalScore(
    `promptfoo/llm-rubric/${metricName}`,
    metricName,
    score,
    passed,
    {
      threshold,
      higherIsBetter: true,
      reason: opts.reason,
    },
  );
}
