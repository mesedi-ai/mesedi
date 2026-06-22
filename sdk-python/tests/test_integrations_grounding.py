"""Unit tests for the Wave 1.3 grounding-evaluator integration helpers.

Each helper in ``mesedi/integrations/{ragas,promptfoo,vectara}.py``
is a thin wrapper around :func:`mesedi.emit_eval_score`. These tests
pin the wire-format contract by intercepting the emit_eval_score
call and asserting the helper produced the right evaluator_id,
metric_type, threshold, and pass/fail verdict.

If a future refactor changes the evaluator_id strings, drops a
parameter, or flips a higher_is_better flag, the grounding_failure
detector will silently misroute scores into the wrong clusters —
these tests catch that BEFORE the SDK ships.
"""

from __future__ import annotations

from typing import Any, Dict, List
from unittest.mock import patch

import pytest

from mesedi.integrations import promptfoo, ragas, vectara


@pytest.fixture
def captured_emits() -> List[Dict[str, Any]]:
    """Returns a list that captures every emit_eval_score call across
    all three integration modules. Patches the import each module did
    at import time so the helpers route to our capture instead of the
    real shipper queue."""
    captured: List[Dict[str, Any]] = []

    def fake_emit(**kwargs: Any) -> None:
        captured.append(kwargs)

    # Each integration module did `from mesedi.observe import
    # emit_eval_score` at import time, which means the module-local
    # name is what we have to patch (patching mesedi.observe directly
    # wouldn't intercept the call site).
    with (
        patch.object(ragas, "emit_eval_score", side_effect=fake_emit),
        patch.object(promptfoo, "emit_eval_score", side_effect=fake_emit),
        patch.object(vectara, "emit_eval_score", side_effect=fake_emit),
    ):
        yield captured


# ──────────────────────────────────────────────────────────────────────
# Ragas helpers
# ──────────────────────────────────────────────────────────────────────


def test_ragas_faithfulness_passed(captured_emits: List[Dict[str, Any]]) -> None:
    ragas.report_faithfulness(0.85)
    assert len(captured_emits) == 1
    call = captured_emits[0]
    assert call["evaluator_id"] == "ragas/faithfulness"
    assert call["metric_type"] == "faithfulness"
    assert call["score"] == 0.85
    assert call["passed"] is True  # 0.85 >= 0.7 default threshold
    assert call["threshold"] == 0.7
    assert call["higher_is_better"] is True


def test_ragas_faithfulness_failed_below_threshold(
    captured_emits: List[Dict[str, Any]],
) -> None:
    ragas.report_faithfulness(0.4, threshold=0.7)
    call = captured_emits[0]
    assert call["passed"] is False  # 0.4 < 0.7


def test_ragas_faithfulness_custom_threshold(
    captured_emits: List[Dict[str, Any]],
) -> None:
    """Passing a custom threshold flows through to both the emitted
    `threshold` field and the pass/fail verdict."""
    ragas.report_faithfulness(0.6, threshold=0.5, reason="judge confident")
    call = captured_emits[0]
    assert call["threshold"] == 0.5
    assert call["passed"] is True
    assert call["reason"] == "judge confident"


def test_ragas_answer_relevance(captured_emits: List[Dict[str, Any]]) -> None:
    ragas.report_answer_relevance(0.9)
    call = captured_emits[0]
    assert call["evaluator_id"] == "ragas/answer_relevance"
    assert call["metric_type"] == "answer_relevance"
    assert call["passed"] is True


def test_ragas_context_precision(captured_emits: List[Dict[str, Any]]) -> None:
    ragas.report_context_precision(0.6, threshold=0.5)
    call = captured_emits[0]
    assert call["evaluator_id"] == "ragas/context_precision"
    assert call["metric_type"] == "context_precision"
    assert call["passed"] is True


# ──────────────────────────────────────────────────────────────────────
# Promptfoo helpers
# ──────────────────────────────────────────────────────────────────────


def test_promptfoo_factuality_passed(
    captured_emits: List[Dict[str, Any]],
) -> None:
    promptfoo.report_factuality(score=0.9, passed=True)
    call = captured_emits[0]
    assert call["evaluator_id"] == "promptfoo/factuality"
    assert call["metric_type"] == "factuality"
    assert call["passed"] is True
    assert call["higher_is_better"] is True


def test_promptfoo_factuality_respects_explicit_passed_flag(
    captured_emits: List[Dict[str, Any]],
) -> None:
    """Promptfoo's pass/fail verdict is supplied by Promptfoo itself,
    NOT inferred from the score. The helper must honor the passed
    arg even if the numeric score is high."""
    promptfoo.report_factuality(score=0.95, passed=False, threshold=0.99)
    call = captured_emits[0]
    assert call["passed"] is False
    assert call["score"] == 0.95


def test_promptfoo_llm_rubric_uses_metric_name(
    captured_emits: List[Dict[str, Any]],
) -> None:
    promptfoo.report_llm_rubric(
        metric_name="clarity",
        score=0.75,
        passed=True,
    )
    call = captured_emits[0]
    assert call["evaluator_id"] == "promptfoo/llm-rubric/clarity"
    assert call["metric_type"] == "clarity"


def test_promptfoo_llm_rubric_requires_metric_name() -> None:
    """Empty metric_name is the bug class this helper is designed to
    prevent — without it the grounding_failure clustering collapses
    every rubric into one signal."""
    with pytest.raises(ValueError, match="metric_name is required"):
        promptfoo.report_llm_rubric(metric_name="", score=0.8, passed=True)


# ──────────────────────────────────────────────────────────────────────
# Vectara HHEM helper
# ──────────────────────────────────────────────────────────────────────


def test_vectara_hhem_passes_when_faithful(
    captured_emits: List[Dict[str, Any]],
) -> None:
    """HHEM's score is HIGHER = more faithful. A 0.9 score should
    pass against the default 0.5 threshold."""
    vectara.report_hhem(0.9)
    call = captured_emits[0]
    assert call["evaluator_id"] == "vectara/hhem"
    assert call["metric_type"] == "hallucination"
    assert call["passed"] is True
    # higher_is_better=True even though metric_type is "hallucination",
    # because HHEM's documented output is a *faithfulness* probability
    # (the inversion happens in the customer's pipeline, not here).
    assert call["higher_is_better"] is True


def test_vectara_hhem_fails_when_hallucinated(
    captured_emits: List[Dict[str, Any]],
) -> None:
    """HHEM faithfulness below 0.5 → not faithful → passed=False."""
    vectara.report_hhem(0.3)
    call = captured_emits[0]
    assert call["passed"] is False


def test_vectara_hhem_custom_threshold(
    captured_emits: List[Dict[str, Any]],
) -> None:
    vectara.report_hhem(0.55, threshold=0.6, reason="judge confident")
    call = captured_emits[0]
    assert call["threshold"] == 0.6
    assert call["passed"] is False  # 0.55 < 0.6
    assert call["reason"] == "judge confident"
