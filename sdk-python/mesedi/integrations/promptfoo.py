"""Promptfoo evaluator one-liner helpers (Mesedi #14, Wave 1.3).

Promptfoo (https://www.promptfoo.dev) is a CLI + library for
prompt evaluation. Customers using its assertion types
(model-graded-factuality, llm-rubric, ...) can use these helpers
to report verdicts into Mesedi without hand-writing the boilerplate
around :func:`mesedi.emit_eval_score`.

Report-only design: Mesedi does NOT import the ``promptfoo`` package
as a dependency. The customer runs Promptfoo on their side; these
helpers accept the resulting score/verdict and emit a correctly-shaped
``eval_score`` event.

Typical usage::

    from mesedi.integrations import promptfoo as mesedi_promptfoo

    @mesedi.wrap
    def my_agent(question):
        answer = run_agent(question)
        # Run Promptfoo model-graded-factuality assertion
        verdict = promptfoo.assert_factuality(answer, expected_facts)
        mesedi_promptfoo.report_factuality(
            score=verdict.score,
            passed=verdict.pass_,
        )
        return answer

Helpers cover the two most common Promptfoo assertion shapes. For
other assertion types (similar, contains, equals, ...) call
:func:`mesedi.emit_eval_score` directly with
``evaluator_id="promptfoo/<assertion>"``.
"""

from __future__ import annotations

from mesedi.observe import emit_eval_score


def report_factuality(
    score: float,
    passed: bool,
    threshold: float = 0.5,
    reason: str = "",
) -> None:
    """Report a Promptfoo ``model-graded-factuality`` assertion.

    Promptfoo's model-graded factuality assertion uses an LLM judge
    to score how factually accurate the answer is. Higher is better.

    Args:
        score: The judge's numeric score (range varies per Promptfoo
            config; typically [0, 1] but pass through as-given).
        passed: Promptfoo's own pass/fail verdict for the assertion.
        threshold: The cutoff Promptfoo used (default 0.5).
        reason: Optional explanation the judge returned.
    """
    emit_eval_score(
        evaluator_id="promptfoo/factuality",
        metric_type="factuality",
        score=float(score),
        passed=bool(passed),
        threshold=float(threshold),
        higher_is_better=True,
        reason=reason,
    )


def report_llm_rubric(
    metric_name: str,
    score: float,
    passed: bool,
    threshold: float = 0.5,
    reason: str = "",
) -> None:
    """Report a Promptfoo ``llm-rubric`` assertion.

    The llm-rubric assertion runs a customer-defined rubric through
    an LLM judge. Because a single execution can run multiple rubrics
    against the same output, ``metric_name`` distinguishes them so
    the grounding_failure detector clusters per-rubric instead of
    collapsing every rubric into one bucket.

    Args:
        metric_name: Short identifier for the specific rubric
            ("clarity", "helpfulness", "tone_appropriate", etc.).
            Becomes part of the eval_score's metric_type field.
        score: The judge's numeric score for this rubric.
        passed: Promptfoo's own pass/fail verdict for this rubric.
        threshold: The cutoff Promptfoo used (default 0.5).
        reason: Optional explanation the judge returned.
    """
    if not metric_name:
        raise ValueError(
            "report_llm_rubric: metric_name is required so the "
            "grounding_failure detector can cluster per-rubric "
            "instead of collapsing every rubric into one bucket."
        )
    emit_eval_score(
        evaluator_id=f"promptfoo/llm-rubric/{metric_name}",
        metric_type=metric_name,
        score=float(score),
        passed=bool(passed),
        threshold=float(threshold),
        higher_is_better=True,
        reason=reason,
    )
