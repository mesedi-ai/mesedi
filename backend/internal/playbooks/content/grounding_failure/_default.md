# Grounding failure

An external evaluator that you wired into your agent (Ragas, Promptfoo, Vectara HHEM, an LLM-judge of your own, or any other tool) reported that this execution's output drifted from the retrieved context. Mesedi does not run the evaluation itself: you emit `eval_score` events with the evaluator's verdict, and Mesedi aggregates them to fire this detector when the score signals a problem.

The signature is `grounding_failure:<evaluator_id>:<metric_type>` so each unique (evaluator, metric) pair clusters separately. A project running both Ragas faithfulness and HHEM hallucination produces two distinct groups, even if both fire on the same executions.

## What's usually happening

Two firing conditions:

1. **Any single `eval_score` event has `passed=false`.** The evaluator's own pass/fail verdict is authoritative; Mesedi does not second-guess it.

2. **Mean score across the execution's `higher_is_better` evaluators falls below 0.5.** A coin-flip floor for the heuristic interpretation when the evaluator does not declare an explicit verdict.

In both cases the agent's output diverged enough from the retrieved context (or whatever ground truth the evaluator is checking against) that the evaluator flagged it.

## What's usually causing the divergence

Three common root causes:

1. **Retrieval is good but the agent ignored it.** The relevant document was retrieved and included in the prompt; the agent still produced an answer based on its parametric knowledge instead of the retrieved content. This is a prompting fix: make the instruction to cite the retrieved content explicit and unforgiving.

2. **Retrieval is wrong.** The wrong documents were retrieved. The agent did its job grounding to what it was given, but what it was given did not match the question. This is a RAG-pipeline fix: tune the retrieval (better embeddings, better re-ranking, better filtering).

3. **Evaluator calibration drift.** The evaluator's threshold is too strict for your domain. This is the rarest cause but worth checking when failure rates spike on legitimate outputs.

## How to investigate

Open the execution and read the `eval_score` events. The evaluator records its `reason` field (when provided), which usually names the specific claim it found unsupported. Cross-reference with the retrieved context in the same execution to see whether the missing support was present in the retrieval and ignored, or whether the retrieval simply did not include it.

If your evaluator emits multiple per-claim scores, the granular ones are more diagnostic than the rolled-up verdict. Look for patterns: are the same kinds of claims consistently failing?

## How to fix

The remediation depends on the root cause:

- **Agent ignored the retrieved context.** Tighten the prompt. Require the agent to quote or cite the retrieved content for every factual claim, and reject answers that fail the citation requirement at the validator layer. A good citation requirement looks like "for every factual claim, include the document_id from the retrieved context that supports it; if no document supports it, say so explicitly."

- **Retrieval is wrong.** Audit the embeddings, the retrieval scoring, and the re-ranker. Common fixes: switch to a domain-tuned embedding model, add a metadata filter that the question implies, lower the retrieval similarity threshold so more candidates are considered before re-ranking.

- **Evaluator calibration.** Lower the threshold or switch evaluators. Validate the change against a hand-labeled golden set before deploying.

## How to test the fix

After deploying, run the same workload and watch the `eval_score` events on new executions. The failure_group should plateau and eventually stop accumulating new entries. If your evaluator emits continuous scores, watch the rolling mean across executions; it should trend upward.

## A note on what eval_score events are not

Mesedi does not run the evaluation, and Mesedi does not interpret the evaluator's reasoning. The detector is a thin aggregator over verdicts the customer's own evaluator produced. The quality of the signal is bounded by the quality of the evaluator. A poorly-calibrated judge will produce noisy `grounding_failure` alerts; fix the judge before fixing the agent.
