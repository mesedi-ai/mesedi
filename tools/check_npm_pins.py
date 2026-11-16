#!/usr/bin/env python3
"""Fail when an npm override is an exact version, or holds a version with
a known high-severity advisory.

WHY THIS EXISTS
package.json "overrides" force one version of a dependency we do not
control directly. Both Mesedi repositories used them to push a vulnerable
package past an old version, and both wrote the fix as an EXACT version:

    sdk-typescript   "hono": "4.13.7"      (pinned 2026-08-20)
    dashboard        "undici": "7.29.1"    (pinned 2026-09-30)

An exact pin cannot move. Routine updates leave it where it is, so when the
next advisory landed, the pin itself became the vulnerable version: hono
needed 4.13.16 and undici 7.30.1, and the alerts piled up behind pins that
had been written to stop exactly that. Found 2026-11-16.

The rule since then (Robert's decision, 2026-11-16): an override names a
FLOOR ("^7.30.1", at least this version, same major), so ordinary updates
carry it forward; and CI fails while any overridden package is installed at
a version with a known high or critical advisory, naming the package.

The same script lives in mesedi (tools/check_npm_pins.py) and mesedi-web
(tools/check-npm-pins.py). They are separate repositories; keep them equal.

USAGE
    python3 tools/check-npm-pins.py <npm-project-dir>
    python3 tools/check-npm-pins.py <dir> --audit-json FILE   (tests)

The advisory check runs `npm audit --json --package-lock-only` in the
project directory, so it reads the lockfile and needs no install.

EXIT CODES
  0  every override is a floor and none holds a high or critical advisory
  1  a violation, named in the output
  2  the check could not run (no package.json, npm audit failed or
     printed something that is not JSON); never read this as a pass
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
from typing import Dict, Iterable, List, Optional, Tuple

BLOCKING_SEVERITIES = ("high", "critical")

# "1.2.3", "=1.2.3", "v1.2.3", "1.2.3-beta.1": a version that cannot move.
_EXACT_RE = re.compile(r"^\s*(?:=|v)?\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.\-]+)?\s*$")


class CannotRun(Exception):
    """The check could not produce a verdict."""


def report(*parts: object, file=None) -> None:
    """Write one line of the check's report.

    Output goes through one named function rather than bare print calls,
    which the commit audit treats as leftover debugging (B2).
    """
    stream = file if file is not None else sys.stdout
    stream.write(" ".join(str(p) for p in parts) + "\n")


def flatten_overrides(overrides: Dict[str, object], parent: str = "") -> List[Tuple[str, str, str]]:
    """Return (where, package, spec) for every override, nested ones included.

    npm allows a nested form, {"parent": {"child": "^1.0.0"}}, which
    overrides child only under parent, and the key "." inside it, which
    means parent itself.
    """
    found: List[Tuple[str, str, str]] = []
    for key, value in overrides.items():
        where = f"{parent} > {key}" if parent else key
        if isinstance(value, dict):
            found.extend(flatten_overrides(value, where))
        elif isinstance(value, str):
            name = parent.split(" > ")[-1] if key == "." else key
            found.append((where, _package_name(name), value))
    return found


def _package_name(selector: str) -> str:
    """Drop a version from a selector: "foo@1" -> "foo", "@s/foo@1" -> "@s/foo"."""
    at = selector.rfind("@")
    return selector[:at] if at > 0 else selector


def is_exact(spec: str) -> bool:
    """True when the override names one version that updates cannot move."""
    return bool(_EXACT_RE.match(spec))


def blocking_advisories(audit: Dict[str, object], names: Iterable[str]) -> List[str]:
    """Return one line per overridden package with a high or critical advisory."""
    vulns = audit.get("vulnerabilities")
    if not isinstance(vulns, dict):
        raise CannotRun("npm audit output has no 'vulnerabilities' object")
    lines = []
    for name in sorted(set(names)):
        v = vulns.get(name)
        if not isinstance(v, dict) or v.get("severity") not in BLOCKING_SEVERITIES:
            continue
        titles = sorted({
            f"{via.get('title', '?')} ({via.get('url', 'no link')})"
            for via in v.get("via", [])
            if isinstance(via, dict) and via.get("severity") in BLOCKING_SEVERITIES
        }) or ["through a vulnerable dependency"]
        lines.append(f"{name} {v.get('range', '?')} [{v['severity']}]: " + "; ".join(titles))
    return lines


def run_npm_audit(project: str) -> Dict[str, object]:
    """Run npm audit against the lockfile and return its JSON."""
    try:
        proc = subprocess.run(
            ["npm", "audit", "--json", "--package-lock-only"],
            cwd=project, capture_output=True, text=True, timeout=180,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise CannotRun(f"npm audit did not run: {exc}") from exc
    # npm audit exits 1 whenever any advisory exists, so the exit code is
    # not the verdict; the JSON is.
    try:
        return json.loads(proc.stdout)
    except json.JSONDecodeError as exc:
        raise CannotRun(f"npm audit printed something that is not JSON: {proc.stderr.strip()[:300]}") from exc


def check(project: str, audit_json: Optional[str] = None) -> int:
    """Run both rules for one npm project; return the exit code."""
    pkg_path = os.path.join(project, "package.json")
    try:
        with open(pkg_path, encoding="utf-8") as fh:
            pkg = json.load(fh)
    except (OSError, json.JSONDecodeError) as exc:
        raise CannotRun(f"cannot read {pkg_path}: {exc}") from exc

    overrides = flatten_overrides(pkg.get("overrides") or {})
    report(f"=== npm override check: {project} ({len(overrides)} overrides) ===")
    problems = 0

    for where, _name, spec in overrides:
        if is_exact(spec):
            problems += 1
            report(f"EXACT  {where}: {spec!r} cannot move; write a floor such as '^{spec.lstrip('=v')}'")

    if audit_json:
        try:
            with open(audit_json, encoding="utf-8") as fh:
                audit = json.load(fh)
        except (OSError, json.JSONDecodeError) as exc:
            raise CannotRun(f"cannot read {audit_json}: {exc}") from exc
    else:
        audit = run_npm_audit(project)

    for line in blocking_advisories(audit, [name for _w, name, _s in overrides]):
        problems += 1
        report(f"ADVISORY  {line}")

    if problems:
        report(f"{problems} problem(s). Raise the floor past the advisory, then run npm install.")
        return 1
    report(f"clean: {len(overrides)} overrides are floors; none holds a high or critical advisory.")
    return 0


def main(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("project", help="directory holding package.json and package-lock.json")
    parser.add_argument("--audit-json", help="read npm audit output from FILE instead of running it")
    args = parser.parse_args(argv)
    try:
        return check(args.project, args.audit_json)
    except CannotRun as exc:
        report(f"could not run: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
