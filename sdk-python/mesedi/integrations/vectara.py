"""Vectara HHEM (Hughes Hallucination Evaluation Model) helper
(Mesedi #14, Wave 1.3).

Vectara HHEM (https://huggingface.co/vectara/hallucination_evaluation_model)
is a small open-source model that detects hallucinations in RAG agent
outputs. Customers running HHEM against their answers can use this
helper to report scores into Mesedi without hand-writing the
boilerplate around :func:`mesedi.emit_eval_score`.

IMPORTANT — direction note. HHEM scores have INVERTED semantics
compared to Ragas / Promptfoo metrics: HIGHER means MORE faithful to
context (less hallucination). The helper passes ``higher_is_better=True``
to the backend and the parameter is named ``faithfulness_score`` to
mirror HHEM's documented output. Some HHEM consumers transform the
output into a "hallucination_score" where LOWER is better — if your
pipeline does that transformation, invert your value before calling
this helper (i.e. pass ``1 - hallucination_score``).

Report-only design: Mesedi does NOT import the ``vectara`` /
``transformers`` packages as dependencies. The customer runs HHEM on
their side; this helper accepts the resulting score and emits a
correctly-shaped ``eval_score`` event.

Typical usage::

    from mesedi.integrations import vectara as mesedi_vectara

    @mesedi.wrap
    def my_agent(question):
        answer, contexts = run_rag(question)
        # Run HHEM via transformers / your local pipeline
        hhem_score = run_hhem(answer, contexts)  # 0-1, higher = better
        mesedi_vectara.report_hhem(hhem_score, threshold=0.5)
        return answer
"""

from __future__ import annotations

from mesedi.observe import emit_eval_score


def report_hhem(
    faithfulness_score: float,
    threshold: float = 0.5,
    reason: str = "",
) -> None:
    """Report a Vectara HHEM faithfulness score.

    HHEM produces a faithfulness probability in [0, 1]; HIGHER means
    more faithful to the retrieved context (i.e. LESS hallucinated).
    Read the module docstring for direction-sensitivity notes.

    Args:
        faithfulness_score: HHEM's faithfulness probability in [0, 1].
            HIGHER is better.
        threshold: Cutoff below which the call counts as a failure
            (default 0.5; HHEM's documented "neutral" point).
        reason: Optional explanation text.
    """
    emit_eval_score(
        evaluator_id="vectara/hhem",
        metric_type="hallucination",
        score=float(faithfulness_score),
        passed=float(faithfulness_score) >= float(threshold),
        threshold=float(threshold),
        higher_is_better=True,
        reason=reason,
    )
