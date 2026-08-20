"""The SDK version is written in three places. They must agree.

    pyproject.toml   [project].version   -> what PyPI publishes
    mesedi/__init__  __version__         -> what users introspect
    mesedi/events.py Execution.sdk_version default -> what goes ON THE WIRE
                                            and lands in the backend's
                                            executions.sdk_version column

The third one is the reason this test exists. It is a hardcoded default
rather than an import (importing __init__ from events would be circular),
so nothing structurally keeps it in step. It has drifted before — see the
"sdks: align sdk_version constants" commit — and when it drifts the
symptom is silent: telemetry keeps flowing, it is just labelled with the
wrong version, so every version-scoped question the backend asks ("which
SDK release is producing these failures?") gets a wrong answer.

The release workflow already checks the tag against pyproject.toml. This
covers the other two.
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

import mesedi
from mesedi.events import Execution

if sys.version_info >= (3, 11):
    import tomllib
else:  # pragma: no cover - 3.9/3.10 path
    tomllib = pytest.importorskip(
        "tomli", reason="tomli needed to read pyproject on <3.11"
    )

PYPROJECT = Path(__file__).resolve().parent.parent / "pyproject.toml"


def _pyproject_version() -> str:
    with PYPROJECT.open("rb") as fh:
        return str(tomllib.load(fh)["project"]["version"])


def test_dunder_version_matches_pyproject() -> None:
    assert mesedi.__version__ == _pyproject_version(), (
        f"mesedi.__version__ ({mesedi.__version__}) != pyproject.toml "
        f"version ({_pyproject_version()}). Bump both together."
    )


def test_wire_sdk_version_matches_dunder_version() -> None:
    """Execution.sdk_version is the value the backend persists. If it
    lags __version__, every execution is mislabelled."""
    wire_default = Execution(execution_id="v-check").sdk_version
    assert wire_default == mesedi.__version__, (
        f"Execution.sdk_version default ({wire_default}) != "
        f"mesedi.__version__ ({mesedi.__version__}). The wire field in "
        f"mesedi/events.py is a hardcoded copy — bump it too, or every "
        f"execution ships a stale version label to the backend."
    )
