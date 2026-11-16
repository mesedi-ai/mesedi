#!/usr/bin/env python3
"""Every failure class has exactly one display tier on the dashboard.

The dashboard (mesedi-web, dashboard/lib/failureClass.ts) decides per
failure class whether a failure group's header shows tokens, cost or
neither. Its tier lists were kept by hand and, by 15 November 2026,
missed environment_misapprehension, covert_coordination and
record_integrity (the outside review of 2026-11-15). Nothing compared them
with the backend, so new classes fell silently into the default.

This check reads the backend's classes from store.go and the three tier
sets from failureClass.ts, and fails when a class is in no set, in more
than one, or a set names a class the backend does not have.

It needs both repositories side by side (mesedi/ and mesedi-web/), as
tools/check-tier-constants.sh does, and runs from mesedi-web's
Guard tests workflow, which checks this public repository out without a
token.

Run:  python3 tools/check_dashboard_failure_classes.py [path/to/failureClass.ts]
Exit: 0 in step, 1 drift found, 2 a file is missing or unreadable.
"""

from __future__ import annotations

import os
import re
import sys
from typing import Dict, List

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STORE_GO = os.path.join(ROOT, "backend", "internal", "store", "store.go")
DEFAULT_TS = os.path.join(os.path.dirname(ROOT), "mesedi-web", "dashboard", "lib", "failureClass.ts")
TIER_SETS = ("TIER_1_TOKEN_CLASSES", "TIER_2_COST_CLASSES", "TIER_3_HIDE_CLASSES")


def report(*parts: object) -> None:
    """Write one line of output. All output goes through here rather than
    bare print calls, which the commit audit treats as leftover debugging
    (B2), as in tools/check_sdk_versions.py."""
    sys.stdout.write(" ".join(str(p) for p in parts) + "\n")


def backend_classes(store_go_text: str) -> List[str]:
    return re.findall(r'^\s*FailureClass\w*\s*=\s*"([a-z_]+)"', store_go_text, re.M)


def dashboard_tiers(ts_text: str) -> Dict[str, List[str]]:
    """Each tier set's entries, read from its `new Set<string>([...])`."""
    tiers: Dict[str, List[str]] = {}
    for name in TIER_SETS:
        m = re.search(r"\b" + name + r"\s*=\s*new Set<string>\(\[(.*?)\]\)", ts_text, re.S)
        if m is None:
            continue
        body = re.sub(r"//[^\n]*", "", m.group(1))
        tiers[name] = re.findall(r'"([a-z_]+)"', body)
    return tiers


def problems(classes: List[str], tiers: Dict[str, List[str]]) -> List[str]:
    out: List[str] = []
    for name in TIER_SETS:
        if name not in tiers:
            out.append(f"{name} is not defined as a Set in failureClass.ts")
    known = set(classes)
    for c in classes:
        homes = [n for n in TIER_SETS if c in tiers.get(n, [])]
        if not homes:
            out.append(f"{c} has no display tier on the dashboard")
        elif len(homes) > 1:
            out.append(f"{c} is in more than one tier: {', '.join(homes)}")
    for name, entries in tiers.items():
        for e in entries:
            if e not in known:
                out.append(f"{name} names {e}, which is not a backend failure class")
    return out


def main(argv: List[str]) -> int:
    ts_path = argv[1] if len(argv) > 1 else DEFAULT_TS
    try:
        with open(STORE_GO, encoding="utf-8") as fh:
            classes = backend_classes(fh.read())
        with open(ts_path, encoding="utf-8") as fh:
            tiers = dashboard_tiers(fh.read())
    except OSError as e:
        report(f"could not run: {e}")
        return 2
    if len(classes) < 20:
        report(f"could not run: found only {len(classes)} failure classes in {STORE_GO}")
        return 2
    found = problems(classes, tiers)
    for p in found:
        report(p)
    if found:
        report(f"{len(found)} dashboard failure-class problem(s); fix {ts_path}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
