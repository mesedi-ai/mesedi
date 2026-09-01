#!/usr/bin/env python3
"""Fail when the Python SDK's two version declarations disagree.

WHY THIS EXISTS
The Python SDK states its version twice:

    sdk-python/pyproject.toml     version = "0.5.4"
    sdk-python/mesedi/__init__.py __version__ = "0.5.4"

The release workflow verifies the git tag against pyproject.toml only.
Nothing verified __init__.py, and __init__.py is the one that matters at
runtime: every execution the SDK ships carries `sdk_version` read from
`mesedi.__version__`. So a drift between the two would publish a package
whose telemetry claims a version that was never released, silently, with
the release workflow green.

Found on 2026-09-01 while cutting 0.5.4. The two files happened to agree,
but only because they were bumped by hand in the same edit.

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


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--tag", help="release tag, e.g. sdk-python-v0.5.4")
    args = ap.parse_args()

    py_proj = read_pyproject_version()
    py_init = read_init_version()
    ts = read_package_version()

    if py_proj is None or py_init is None:
        return fail("could not parse a Python version from pyproject.toml "
                    "or mesedi/__init__.py")
    if ts is None:
        return fail("could not parse a version from sdk-typescript/package.json")

    print("=== SDK version consistency ===")
    print(f"  sdk-python/pyproject.toml      {py_proj}")
    print(f"  sdk-python/mesedi/__init__.py  {py_init}")
    print(f"  sdk-typescript/package.json    {ts}")

    problems: list[str] = []
    if py_proj != py_init:
        problems.append(
            f"pyproject.toml says {py_proj} but mesedi/__init__.py says "
            f"{py_init}. __init__.py is what every event's sdk_version "
            f"field reports at runtime.")

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
