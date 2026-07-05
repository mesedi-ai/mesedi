/**
 * Ragas grounding-evaluator one-liner helpers (, ).
 *
 * Ragas (https://docs.ragas.io) is a popular RAG evaluation library.
 * Customers running Ragas against their agent outputs can use these
 * helpers to report scores into Mesedi without hand-writing the
 * boilerplate around {@link emitEvalScore}.
 *
 * Report-only design: Mesedi does NOT import the `ragas` package as
 * a dependency. The customer runs the evaluator on their side with
 * their own version pinning and configuration; these helpers accept
 * the resulting score and emit a correctly-shaped `eval_score` event
 * so the backend's grounding_failure detector aggregates it under
 * the right evaluator_id + metric_type cluster.
 *
 * Typical usage (Ragas in TypeScript is typically called via a
 * Python sidecar; the helper signature is identical regardless of
 * how the customer obtained the score):
 *
 *     import { wrap } from "mesedi";
 *     import { reportFaithfulness } from "mesedi/integrations/ragas";
 *
 *     export const myAgent = wrap(async (question: string) => {
 *       const { answer, contexts } = await runRag(question);
 *       const score = await scoreWithRagas(question, answer, contexts);
 *       reportFaithfulness(score, { threshold: 0.7 });
 *       return answer;
 *     });
 *
 * Helpers cover the three most common Ragas metrics. For niche
 * metrics (answer_correctness, answer_similarity, context_recall, ...)
 * call {@link emitEvalScore} directly with
 * `evaluatorId: "ragas/<metric>"`.
 */

import { emitEvalScore } from "../observe.js";

export interface RagasReportOptions {
  /**
   * Cutoff below which the call counts as a failure. Defaults to
   * 0.7 (Ragas's documented working threshold).
   */
  threshold?: number;
  /** Optional explanation Ragas returned. */
  reason?: string;
}

/**
 * Report a Ragas `faithfulness` score.
 *
 * Faithfulness measures how well the agent's answer can be inferred
 * from the retrieved context. Higher is better; range [0, 1].
 */
export function reportFaithfulness(
  score: number,
  opts: RagasReportOptions = {},
): void {
  const threshold = opts.threshold ?? 0.7;
  emitEvalScore(
    "ragas/faithfulness",
    "faithfulness",
    score,
    score >= threshold,
    {
      threshold,
      higherIsBetter: true,
      reason: opts.reason,
    },
  );
}

/**
 * Report a Ragas `answer_relevancy` score.
 *
 * Answer relevance measures whether the answer addresses the
 * question that was asked. Higher is better; range [0, 1].
 */
export function reportAnswerRelevance(
  score: number,
  opts: RagasReportOptions = {},
): void {
  const threshold = opts.threshold ?? 0.7;
  emitEvalScore(
    "ragas/answer_relevance",
    "answer_relevance",
    score,
    score >= threshold,
    {
      threshold,
      higherIsBetter: true,
      reason: opts.reason,
    },
  );
}

/**
 * Report a Ragas `context_precision` score.
 *
 * Context precision measures whether the retrieved context contained
 * only relevant chunks. Higher is better; range [0, 1].
 */
export function reportContextPrecision(
  score: number,
  opts: RagasReportOptions = {},
): void {
  const threshold = opts.threshold ?? 0.7;
  emitEvalScore(
    "ragas/context_precision",
    "context_precision",
    score,
    score >= threshold,
    {
      threshold,
      higherIsBetter: true,
      reason: opts.reason,
    },
  );
}
