#!/usr/bin/env python3
"""Fail when an SDK's version declarations disagree.

WHY THIS EXISTS
Each SDK states its version in more than one place, and the one that
matters most is the least obvious:

    sdk-python/pyproject.toml       version              -> what PyPI publishes
    sdk-python/mesedi/__init__.py   __version__          -> what users introspect
    sdk-python/mesedi/events.py     sdk_version default  -> WHAT GOES ON THE WIRE
    sdk-typescript/package.json     version              -> what npm publishes
    sdk-typescript/src/wrap.ts      sdk_version literal  -> WHAT GOES ON THE WIRE

The wire values are hardcoded (importing __init__ from events would be
circular), so nothing structurally keeps them in step, and when they drift
the symptom is silent. Telemetry keeps flowing, it is just labelled with
the wrong version, so every version-scoped question the backend asks gets
a wrong answer.

This is not hypothetical. On 2026-09-01, 0.5.4 and 0.7.2 were both cut
after bumping only pyproject/package.json and __init__. Published
sdk-python 0.5.4 reported sdk_version 0.5.3 on every execution, and
published sdk-typescript 0.7.2 reported 0.7.1. Both needed an immediate
patch release.

sdk-python/tests/test_version_sync.py already covered the Python side and
caught it. It does not run in the release workflow, and TypeScript had no
equivalent at all. This script covers both SDKs and runs in CI *and* as a
publish gate.

USAGE
    python3 tools/check_sdk_versions.py                  # consistency only
    python3 tools/check_sdk_versions.py --tag sdk-python-v0.5.4
    python3 tools/check_sdk_versions.py --tag sdk-typescript-v0.7.2

With --tag, the version embedded in the tag must also match. That makes
this usable as a publish gate in both release workflows, not just CI.

EXIT CODES
  0  consistent
  1  a mismatch
  2  could not run (a file is missing or a version could not be parsed)
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PYPROJECT = os.path.join(ROOT, "sdk-python", "pyproject.toml")
INIT = os.path.join(ROOT, "sdk-python", "mesedi", "__init__.py")
PACKAGE_JSON = os.path.join(ROOT, "sdk-typescript", "package.json")
EVENTS_PY = os.path.join(ROOT, "sdk-python", "mesedi", "events.py")
WRAP_TS = os.path.join(ROOT, "sdk-typescript", "src", "wrap.ts")


def fail(msg: str) -> int:
    print(f"could not run: {msg}", file=sys.stderr)
    return 2


def read_pyproject_version() -> str | None:
    if not os.path.exists(PYPROJECT):
        return None
    # Deliberately a regex rather than tomllib: this script must run on
    # any Python 3, including the 3.9 the SDK still supports, and tomllib
    # only landed in 3.11.
    m = re.search(r'^version\s*=\s*"([^"]+)"', open(PYPROJECT, encoding="utf-8").read(), re.M)
    return m.group(1) if m else None


def read_init_version() -> str | None:
    if not os.path.exists(INIT):
        return None
    m = re.search(r'^__version__\s*=\s*"([^"]+)"', open(INIT, encoding="utf-8").read(), re.M)
    return m.group(1) if m else None


def read_package_version() -> str | None:
    if not os.path.exists(PACKAGE_JSON):
        return None
    try:
        return json.load(open(PACKAGE_JSON, encoding="utf-8")).get("version")
    except (json.JSONDecodeError, OSError):
        return None


def read_python_wire_version() -> str | None:
    """The Execution.sdk_version default: the value the backend persists."""
    if not os.path.exists(EVENTS_PY):
        return None
    m = re.search(r'sdk_version:\s*str\s*=\s*"([^"]+)"',
                  open(EVENTS_PY, encoding="utf-8").read())
    return m.group(1) if m else None


def read_ts_wire_version() -> str | None:
    if not os.path.exists(WRAP_TS):
        return None
    m = re.search(r'sdk_version:\s*"([^"]+)"',
                  open(WRAP_TS, encoding="utf-8").read())
    return m.group(1) if m else None


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--tag", help="release tag, e.g. sdk-python-v0.5.4")
    args = ap.parse_args()

    py_proj = read_pyproject_version()
    py_init = read_init_version()
    py_wire = read_python_wire_version()
    ts = read_package_version()
    ts_wire = read_ts_wire_version()

    if py_proj is None or py_init is None:
        return fail("could not parse a Python version from pyproject.toml "
                    "or mesedi/__init__.py")
    if ts is None:
        return fail("could not parse a version from sdk-typescript/package.json")
    if py_wire is None:
        return fail("could not parse Execution.sdk_version from mesedi/events.py")
    if ts_wire is None:
        return fail("could not parse sdk_version from sdk-typescript/src/wrap.ts")

    print("=== SDK version consistency ===")
    print(f"  sdk-python/pyproject.toml       {py_proj}")
    print(f"  sdk-python/mesedi/__init__.py   {py_init}")
    print(f"  sdk-python/mesedi/events.py     {py_wire}   (on the wire)")
    print(f"  sdk-typescript/package.json     {ts}")
    print(f"  sdk-typescript/src/wrap.ts      {ts_wire}   (on the wire)")

    problems: list[str] = []
    if py_proj != py_init:
        problems.append(
            f"pyproject.toml says {py_proj} but mesedi/__init__.py says "
            f"{py_init}.")
    if py_wire != py_init:
        problems.append(
            f"mesedi/events.py ships sdk_version={py_wire} on the wire but "
            f"mesedi/__init__.py says {py_init}. Every execution would be "
            f"labelled with the wrong version in the backend.")
    if ts_wire != ts:
        problems.append(
            f"sdk-typescript/src/wrap.ts ships sdk_version={ts_wire} on the "
            f"wire but package.json says {ts}. Every execution would be "
            f"labelled with the wrong version in the backend.")

    if args.tag:
        if args.tag.startswith("sdk-python-v"):
            want, got, which = args.tag[len("sdk-python-v"):], py_proj, "sdk-python"
            if want != got:
                problems.append(f"tag {args.tag} does not match {which} version {got}")
            elif want != py_init:
                problems.append(f"tag {args.tag} does not match __init__.py {py_init}")
        elif args.tag.startswith("sdk-typescript-v"):
            want = args.tag[len("sdk-typescript-v"):]
            if want != ts:
                problems.append(f"tag {args.tag} does not match package.json {ts}")
        else:
            return fail(f"unrecognised tag shape: {args.tag}")
        print(f"  tag under test                 {args.tag}")

    if not problems:
        print("clean: all version declarations agree.")
        return 0

    print()
    for p in problems:
        print(f"VIOLATION  {p}")
    return 1


if __name__ == "__main__":
    sys.exit(main())
