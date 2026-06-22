"""Ragas grounding-evaluator one-liner helpers (Mesedi #14, Wave 1.3).

Ragas (https://docs.ragas.io) is a popular RAG evaluation library.
Customers running Ragas against their agent outputs can use these
helpers to report scores into Mesedi without hand-writing the
boilerplate around :func:`mesedi.emit_eval_score`.

Report-only design: Mesedi does NOT import the ``ragas`` package as a
dependency. The customer runs the evaluator on their side with their
own version pinning and configuration; these helpers accept the
resulting score and emit a correctly-shaped ``eval_score`` event so
the backend's grounding_failure detector aggregates it under the
right evaluator_id + metric_type cluster.

Typical usage::

    from ragas.metrics import faithfulness
    from mesedi.integrations import ragas as mesedi_ragas

    @mesedi.wrap
    def my_agent(question):
        answer, contexts = run_rag(question)
        score = faithfulness.score(question, answer, contexts)
        mesedi_ragas.report_faithfulness(score, threshold=0.7)
        return answer

Helpers cover the three most common Ragas metrics. For niche metrics
(answer_correctness, answer_similarity, context_recall, ...) call
:func:`mesedi.emit_eval_score` directly with ``evaluator_id="ragas/<metric>"``.
"""

from __future__ import annotations

from mesedi.observe import emit_eval_score


def report_faithfulness(
    score: float,
    threshold: float = 0.7,
    reason: str = "",
) -> None:
    """Report a Ragas ``faithfulness`` score.

    Faithfulness measures how well the agent's answer can be inferred
    from the retrieved context. Higher is better; range [0, 1].

    Args:
        score: The Ragas-computed faithfulness score.
        threshold: Cutoff below which the call counts as a failure
            (default 0.7, Ragas's documented working threshold).
        reason: Optional explanation Ragas returned.
    """
    emit_eval_score(
        evaluator_id="ragas/faithfulness",
        metric_type="faithfulness",
        score=float(score),
        passed=float(score) >= float(threshold),
        threshold=float(threshold),
        higher_is_better=True,
        reason=reason,
    )


def report_answer_relevance(
    score: float,
    threshold: float = 0.7,
    reason: str = "",
) -> None:
    """Report a Ragas ``answer_relevancy`` score.

    Answer relevance measures whether the answer addresses the
    question that was asked. Higher is better; range [0, 1].

    Args:
        score: The Ragas-computed answer_relevancy score.
        threshold: Cutoff below which the call counts as a failure
            (default 0.7).
        reason: Optional explanation Ragas returned.
    """
    emit_eval_score(
        evaluator_id="ragas/answer_relevance",
        metric_type="answer_relevance",
        score=float(score),
        passed=float(score) >= float(threshold),
        threshold=float(threshold),
        higher_is_better=True,
        reason=reason,
    )


def report_context_precision(
    score: float,
    threshold: float = 0.7,
    reason: str = "",
) -> None:
    """Report a Ragas ``context_precision`` score.

    Context precision measures whether the retrieved context contained
    only relevant chunks. Higher is better; range [0, 1].

    Args:
        score: The Ragas-computed context_precision score.
        threshold: Cutoff below which the call counts as a failure
            (default 0.7).
        reason: Optional explanation Ragas returned.
    """
    emit_eval_score(
        evaluator_id="ragas/context_precision",
        metric_type="context_precision",
        score=float(score),
        passed=float(score) >= float(threshold),
        threshold=float(threshold),
        higher_is_better=True,
        reason=reason,
    )
