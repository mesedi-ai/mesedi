#!/usr/bin/env python3
"""Fail CI when an em dash appears in a customer-visible doc comment.

WHY THIS EXISTS
Docstrings and JSDoc are not internal notes. They render in IDE tooltips
on hover, in `help()`, and on the PyPI and npm package pages. An em dash
there reads as machine-written prose to the developer evaluating whether
to trust the library, which is the opposite of the impression a hand-built
SDK should give.

SCOPE, AND WHY IT STOPS WHERE IT DOES
Only customer-visible doc comments are checked:

  - Python: triple-quoted docstrings in sdk-python/mesedi/
  - TypeScript: /** ... */ JSDoc blocks in sdk-typescript/src/

Ordinary line comments (# in Python, // in TypeScript) are deliberately
NOT checked. They are read by whoever is editing the file and by nobody
else, and sweeping them is a large diff with no reader-facing benefit.
This matches the decision made for the Go backend, where roughly 700 em
dashes in code comments were left alone while every customer-facing string
was cleaned.

WHAT IT MUST NOT DO
The SDK uses box-drawing characters for section dividers:

    # ── producer-side API ────────────────────────────────

That is U+2500 BOX DRAWINGS LIGHT HORIZONTAL, not U+2014 EM DASH. There
are over a thousand of them and they are structural. This check looks for
U+2014 only, and a sweep driven by it must do the same. Confusing the two
would mangle every section header in the package.

EXIT CODES
  0  clean
  1  at least one em dash in a doc comment
  2  could not run (no SDK source, or zero doc comments found, which would
     mean the extraction stopped matching)
"""
from __future__ import annotations

import glob
import os
import re
import sys

EM_DASH = "—"
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PY_GLOB = os.path.join(ROOT, "sdk-python", "mesedi", "**", "*.py")
TS_GLOB = os.path.join(ROOT, "sdk-typescript", "src", "**", "*.ts")

PY_DOCSTRING = re.compile(r'("""|\'\'\')(.*?)\1', re.S)
TS_JSDOC = re.compile(r"/\*\*(.*?)\*/", re.S)


def scan(paths: list[str], pattern: re.Pattern[str], skip_tests: bool
         ) -> tuple[int, list[tuple[str, int, str]]]:
    blocks = 0
    findings: list[tuple[str, int, str]] = []
    for path in paths:
        if skip_tests and (".test." in path or "/tests/" in path
                           or os.path.basename(path).startswith("test_")):
            continue
        try:
            src = open(path, encoding="utf-8", errors="replace").read()
        except OSError:
            continue
        for m in pattern.finditer(src):
            blocks += 1
            body = m.group(2) if pattern is PY_DOCSTRING else m.group(1)
            if EM_DASH not in body:
                continue
            base = src[: m.start()].count("\n")
            for offset, line in enumerate(body.split("\n")):
                if EM_DASH in line:
                    rel = os.path.relpath(path, ROOT)
                    findings.append((rel, base + offset + 1, line.strip()[:88]))
    return blocks, findings


def main() -> int:
    py_files = sorted(glob.glob(PY_GLOB, recursive=True))
    ts_files = sorted(glob.glob(TS_GLOB, recursive=True))
    if not py_files and not ts_files:
        print("could not run: no SDK source found", file=sys.stderr)
        return 2

    py_blocks, py_hits = scan(py_files, PY_DOCSTRING, skip_tests=True)
    ts_blocks, ts_hits = scan(ts_files, TS_JSDOC, skip_tests=True)

    if py_blocks == 0 and ts_blocks == 0:
        print("could not run: zero doc comments extracted, the patterns are "
              "probably broken", file=sys.stderr)
        return 2

    findings = py_hits + ts_hits
    print(f"=== doc comment style: {py_blocks} Python docstrings, "
          f"{ts_blocks} JSDoc blocks ===")
    if not findings:
        print("clean: no em dashes in customer-visible doc comments.")
        return 0

    for rel, line_no, text in findings:
        print(f"VIOLATION  {rel}:{line_no}  {text}")
    print()
    print(f"{len(findings)} em dash(es) in doc comments that render in IDE "
          f"tooltips and on the package pages.")
    print("Note: box-drawing dividers (U+2500) are a different character and "
          "are not flagged. Do not replace those.")
    return 1


if __name__ == "__main__":
    sys.exit(main())
