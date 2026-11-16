"""Runs the shared egress destination vectors (spec/egress_destination_vectors.json).

The TypeScript SDK runs the same file in src/egress_destination_vectors.test.ts,
so the two normalizers cannot drift apart. Added 2026-11-15 after the outside
review found both SDKs sending user:password@ to the backend (finding D13).
"""

from __future__ import annotations

import json
from pathlib import Path

from mesedi.egress import _normalize_egress_destination

VECTORS = Path(__file__).resolve().parents[2] / "spec" / "egress_destination_vectors.json"


def _cases() -> list:
    with VECTORS.open(encoding="utf-8") as fh:
        cases = json.load(fh)["cases"]
    assert cases, "the shared vector file has no cases; this test would pass vacuously"
    return cases


def test_every_shared_vector_normalizes_as_specified() -> None:
    for case in _cases():
        got = _normalize_egress_destination(case["in"])
        assert got == case["out"], f"{case['in']!r} -> {got!r}, want {case['out']!r} ({case['why']})"


def test_no_secret_ever_survives_normalization() -> None:
    for case in _cases():
        if "SECRET" in case["in"]:
            assert "SECRET" not in _normalize_egress_destination(case["in"]), case["in"]


def test_the_vectors_cover_the_credentials_case() -> None:
    """The file must keep the case that was broken, or the test proves nothing about it."""
    ins = [c["in"] for c in _cases()]
    assert any("@" in i and ":SECRET@" in i for i in ins)
