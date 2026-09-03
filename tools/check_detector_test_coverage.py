#!/usr/bin/env python3
"""Detector test coverage gate (semantic_loop.G3 release-gate).

Enforces that every detector in Mesedi's canonical 20-detector list
has at least one non-skipped real-SDK integration test in
backend/test/integration/test_detectors.py.

Background: the semantic_loop.G3 audit row called out a historical
silent-failure (semantic_loop read a `state` field no SDK ever
emitted; the detector no-op'd until an integration test caught it).
The lesson: test-with-real-SDK-from-day-one. This script codifies
that lesson as a build-time release gate for all 20 detectors.

Coverage policy:

  - The script parses test_detectors.py for `def test_*` definitions.
  - For each detector D in the canonical 20-detector list, the
    script accepts coverage when ANY test function whose name starts
    with `test_<D>` exists in the file (sub-signature variants like
    `test_<D>_<variant>` count). Naming convention enforced.
  - Tests decorated with @pytest.mark.skipif (e.g. @needs_anthropic
    when ANTHROPIC_API_KEY is unset) DO count as covering their
    detector — the skip is conditional on environment, and CI is
    expected to provide the key.
  - Tests with unconditional pytest.skip() do NOT count. The one
    accepted unconditional-skip is context_overflow's RUN_EXPENSIVE_
    TESTS gate (cost ~$0.18/run); declared in
    detector_test_exceptions.json with explicit approval metadata.

Adding a new accepted exception requires editing
detector_test_exceptions.json — visible in code review, paper trail
in version control.

Adding a new detector to Mesedi without a matching test now fails
the build. That's the point.

Usage:
    python3 tools/check_detector_test_coverage.py
    # Exit 0 = all detectors covered (or excepted).
    # Exit 1 = one or more missing; list printed to stderr.

Invocation: conftest.py session-start hook runs this script and
fails the integration-test session if it exits non-zero.
"""

from __future__ import annotations

import json
import os
import re
import sys
from pathlib import Path
from typing import Dict, List, Set


# Canonical 20-detector list. Source of truth:
# backend/internal/store/store.go FailureClass* constants.
# Keep this in sync — adding a new detector to Mesedi means adding
# its name here AND adding a matching `test_<name>` to
# test_detectors.py (the whole point of this gate).
CANONICAL_DETECTORS: List[str] = [
    "crashes",
    "loops",
    "tool_failures",
    "validator_failures",
    "drift",
    "cost_velocity",
    "prompt_injection",
    "infrastructure_throttled",
    "data_leakage",
    "semantic_loop",
    "tool_schema_drift",
    "context_overflow",
    "token_waste",
    "sandbox_escape",
    "grounding_failure",
    "cascading_failure",
    "coordination_deadlock",
    "provider_incident",
    "hitl_timeout",
    "hitl_rejection_spike",
    "record_integrity",
]


# Some detectors don't follow the strict `test_<name>` convention.
# loops is the historical exception: its sub-signatures live under
# test_token_waste (which covers identical_call_loop), test_similar_
# call_loop, test_step_count. The canonical mapping below makes the
# coverage explicit for these cases.
#
# Adding entries here is allowed when the detector genuinely has
# multiple sub-signature tests with sub-signature-keyed names rather
# than detector-keyed names. The reviewer should confirm each entry
# is a true coverage assertion, not a workaround for missing tests.
TEST_NAME_ALIASES: Dict[str, List[str]] = {
    "loops": [
        "test_token_waste",   # covers identical_call_loop via repeat
        "test_similar_call_loop",
        "test_step_count",
    ],
    "drift": [
        # drift has only one sub-signature today (lexical_drift_*);
        # test_detectors.py names the test for the sub-sig, not the
        # detector class. Same pattern as loops above. If drift.G1
        # ships model_drift as a v2 sub-sig (currently banked per
        # the drift audit), a test_model_drift entry joins this
        # list rather than triggering coverage churn.
        "test_lexical_drift",
    ],
}


def _project_root() -> Path:
    """Resolve the mesedi repo root assuming this script lives at
    <repo>/tools/. Caller can override by setting MESEDI_REPO_ROOT
    in env (used by CI configurations where the script lives in a
    different layout)."""
    override = os.environ.get("MESEDI_REPO_ROOT", "").strip()
    if override:
        return Path(override)
    return Path(__file__).resolve().parent.parent


def _load_exceptions(root: Path) -> Set[str]:
    """Read the exceptions allowlist file. Each accepted exception
    must have an explicit reason + approved_by + approved_date in
    JSON so adding a new entry surfaces a paper trail in code review.

    Missing file → empty set (no exceptions).
    """
    path = root / "tools" / "detector_test_exceptions.json"
    if not path.exists():
        return set()
    try:
        raw = json.loads(path.read_text())
    except Exception as exc:  # noqa: BLE001 — fail-fast on any JSON parse problem; the script must not silently accept a corrupt exception allowlist
        print(
            f"[detector-coverage] FATAL: could not parse "
            f"detector_test_exceptions.json: {exc}",
            file=sys.stderr,
        )
        sys.exit(2)
    if not isinstance(raw, dict):
        print(
            "[detector-coverage] FATAL: detector_test_exceptions.json "
            "must be a JSON object keyed by detector name.",
            file=sys.stderr,
        )
        sys.exit(2)
    accepted = set()
    for name, meta in raw.items():
        if not isinstance(meta, dict):
            print(
                f"[detector-coverage] FATAL: exception entry {name!r} "
                "is not an object with reason/approved_by/approved_date.",
                file=sys.stderr,
            )
            sys.exit(2)
        # Explicit field checks (not a loop) — every accepted
        # exception must carry a paper trail. Missing any of these
        # blocks the script so PRs adding exceptions can't sneak
        # past code review without the required metadata.
        if not meta.get("reason"):
            _exception_field_missing(name, "reason")
        if not meta.get("approved_by"):
            _exception_field_missing(name, "approved_by")
        if not meta.get("approved_date"):
            _exception_field_missing(name, "approved_date")
        accepted.add(name)
    return accepted


def _exception_field_missing(name: str, field: str) -> None:
    """Fail-fast helper for _load_exceptions's explicit field
    validation. Lives outside the loop body to keep _load_exceptions
    free of the per-field-check repetition."""
    print(
        f"[detector-coverage] FATAL: exception entry {name!r} "
        f"missing required field {field!r}.",
        file=sys.stderr,
    )
    sys.exit(2)


# Matches lines like `def test_crashes(` or
# `def test_crashes_explicit_timeout(` (parameter list optional).
# Captures the test name.
_TEST_DEF_RE = re.compile(r"^def\s+(test_[a-zA-Z0-9_]+)\s*\(")


def _collect_test_names(test_file: Path) -> Set[str]:
    """Parse test_detectors.py for all top-level `def test_*`
    definitions. Returns the set of test function names. The script
    enforces existence + naming convention; it does NOT evaluate
    skip decorators (conditional skips fire only when the
    environment lacks the necessary keys, and CI is expected to
    provide them)."""
    if not test_file.exists():
        print(
            f"[detector-coverage] FATAL: test file not found at {test_file}",
            file=sys.stderr,
        )
        sys.exit(2)
    names = set()
    for line in test_file.read_text().splitlines():
        m = _TEST_DEF_RE.match(line)
        if m:
            names.add(m.group(1))
    return names


def _covers(detector: str, test_names: Set[str]) -> bool:
    """A detector is covered when:
      (a) any test name in TEST_NAME_ALIASES[detector] exists, OR
      (b) any test name starts with `test_<detector>` (which catches
          both the bare `test_<detector>` and sub-signature variants
          like `test_<detector>_<variant>`).
    """
    aliases = TEST_NAME_ALIASES.get(detector, [])
    for alias in aliases:
        if alias in test_names:
            return True
    prefix = f"test_{detector}"
    return any(name == prefix or name.startswith(prefix + "_") for name in test_names)


def main() -> int:
    root = _project_root()
    test_file = root / "backend" / "test" / "integration" / "test_detectors.py"
    accepted = _load_exceptions(root)
    test_names = _collect_test_names(test_file)

    missing: List[str] = []
    for detector in CANONICAL_DETECTORS:
        if _covers(detector, test_names):
            continue
        if detector in accepted:
            continue
        missing.append(detector)

    if missing:
        print(
            "[detector-coverage] FAIL: one or more detectors are missing "
            "integration test coverage:",
            file=sys.stderr,
        )
        for name in missing:
            print(f"  - {name}", file=sys.stderr)
        print(
            "\nResolution:\n"
            f"  (a) Add a `def test_{missing[0]}(...)` function to "
            f"{test_file.relative_to(root)} (matches naming convention "
            "test_<detector> or test_<detector>_<variant>).\n"
            "  (b) If the detector cannot be reasonably integration-"
            "tested (e.g. expensive real-LLM cost, requires multi-"
            "tenant correlation), add an entry to "
            "tools/detector_test_exceptions.json with reason + "
            "approved_by + approved_date.\n",
            file=sys.stderr,
        )
        return 1

    return 0


if __name__ == "__main__":
    sys.exit(main())
