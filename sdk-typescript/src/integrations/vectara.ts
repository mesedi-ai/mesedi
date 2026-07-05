/**
 * Vectara HHEM (Hughes Hallucination Evaluation Model) helper
 * (, ).
 *
 * Vectara HHEM
 * (https://huggingface.co/vectara/hallucination_evaluation_model)
 * is a small open-source model that detects hallucinations in RAG
 * agent outputs. Customers running HHEM against their answers can
 * use this helper to report scores into Mesedi without hand-writing
 * the boilerplate around {@link emitEvalScore}.
 *
 * IMPORTANT — direction note. HHEM scores have INVERTED semantics
 * compared to Ragas / Promptfoo metrics: HIGHER means MORE faithful
 * to context (less hallucination). The helper passes
 * `higherIsBetter: true` to the backend and the parameter is named
 * `faithfulnessScore` to mirror HHEM's documented output. Some HHEM
 * consumers transform the output into a "hallucination_score" where
 * LOWER is better — if your pipeline does that transformation,
 * invert your value before calling this helper (i.e. pass
 * `1 - hallucinationScore`).
 *
 * Report-only design: Mesedi does NOT import the vectara /
 * transformers packages as dependencies. The customer runs HHEM on
 * their side; this helper accepts the resulting score and emits a
 * correctly-shaped `eval_score` event.
 *
 * Typical usage:
 *
 *     import { wrap } from "mesedi";
 *     import { reportHhem } from "mesedi/integrations/vectara";
 *
 *     export const myAgent = wrap(async (question: string) => {
 *       const { answer, contexts } = await runRag(question);
 *       const hhemScore = await runHhem(answer, contexts); // 0-1, higher = better
 *       reportHhem(hhemScore, { threshold: 0.5 });
 *       return answer;
 *     });
 */

import { emitEvalScore } from "../observe.js";

export interface VectaraReportOptions {
  /**
   * Cutoff below which the call counts as a failure. Default 0.5
   * (HHEM's documented "neutral" point).
   */
  threshold?: number;
  /** Optional explanation text. */
  reason?: string;
}

/**
 * Report a Vectara HHEM faithfulness score.
 *
 * HHEM produces a faithfulness probability in [0, 1]; HIGHER means
 * more faithful to the retrieved context (i.e. LESS hallucinated).
 * Read the module docstring for direction-sensitivity notes.
 *
 * @param faithfulnessScore HHEM's faithfulness probability in [0, 1].
 *   HIGHER is better.
 * @param opts Threshold + reason.
 */
export function reportHhem(
  faithfulnessScore: number,
  opts: VectaraReportOptions = {},
): void {
  const threshold = opts.threshold ?? 0.5;
  emitEvalScore(
    "vectara/hhem",
    "hallucination",
    faithfulnessScore,
    faithfulnessScore >= threshold,
    {
      threshold,
      higherIsBetter: true,
      reason: opts.reason,
    },
  );
}
