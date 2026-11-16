#!/usr/bin/env python3
"""Tests for check-npm-pins.py.

The check exists to FAIL when an override is an exact version or holds a
high-severity advisory, so most tests assert a non-zero exit. The clean
case is there to prove the fixtures are not failing for an unrelated
reason. Exit codes are asserted precisely:
    0 = clean, 1 = a violation, 2 = the check could not run

Run:  python3 tools/test_check_npm_pins.py
Standard library only, like the other guard tests in this repository.
"""

from __future__ import annotations

import importlib.util
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "check-npm-pins.py")
if not os.path.exists(SCRIPT):
    SCRIPT = os.path.join(HERE, "check_npm_pins.py")

_spec = importlib.util.spec_from_file_location("check_npm_pins", SCRIPT)
pins = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(pins)


def audit_with(name: str, severity: str, title: str = "Example advisory") -> dict:
    return {"vulnerabilities": {name: {
        "name": name, "severity": severity, "range": "<9.9.9",
        "via": [{"title": title, "url": "https://github.com/advisories/GHSA-test", "severity": severity}],
    }}}


class Fixture:
    def __init__(self, overrides: dict, audit: dict):
        self.dir = tempfile.mkdtemp(prefix="npm-pins-")
        with open(os.path.join(self.dir, "package.json"), "w") as fh:
            json.dump({"name": "fixture", "overrides": overrides}, fh)
        self.audit = os.path.join(self.dir, "audit.json")
        with open(self.audit, "w") as fh:
            json.dump(audit, fh)

    def run(self):
        proc = subprocess.run([sys.executable, SCRIPT, self.dir, "--audit-json", self.audit],
                              capture_output=True, text=True)
        return proc.returncode, proc.stdout + proc.stderr

    def close(self):
        shutil.rmtree(self.dir, ignore_errors=True)


class EndToEnd(unittest.TestCase):
    def _run(self, overrides, audit):
        f = Fixture(overrides, audit)
        try:
            return f.run()
        finally:
            f.close()

    def test_clean_floors_pass(self):
        code, out = self._run({"undici": "^7.30.1", "glob": {"minimatch": "^11.0.1"}}, {"vulnerabilities": {}})
        self.assertEqual(code, 0, out)

    def test_exact_pin_fails_and_names_it(self):
        code, out = self._run({"undici": "7.29.1"}, {"vulnerabilities": {}})
        self.assertEqual(code, 1, out)
        self.assertIn("EXACT  undici", out)
        self.assertIn("'^7.29.1'", out)

    def test_nested_exact_pin_fails(self):
        code, out = self._run({"@node-minify/core": {"glob": "10.5.0"}}, {"vulnerabilities": {}})
        self.assertEqual(code, 1, out)
        self.assertIn("@node-minify/core > glob", out)

    def test_high_advisory_on_pinned_package_fails(self):
        code, out = self._run({"hono": "^4.13.7"}, audit_with("hono", "high", "Hono csrf bypass"))
        self.assertEqual(code, 1, out)
        self.assertIn("ADVISORY  hono", out)
        self.assertIn("Hono csrf bypass", out)

    def test_critical_advisory_fails(self):
        code, _out = self._run({"hono": "^4.13.7"}, audit_with("hono", "critical"))
        self.assertEqual(code, 1)

    def test_moderate_advisory_does_not_fail(self):
        code, out = self._run({"hono": "^4.13.7"}, audit_with("hono", "moderate"))
        self.assertEqual(code, 0, out)

    def test_advisory_on_unpinned_package_does_not_fail(self):
        # The rule is about pins; other advisories are Dependabot's job.
        code, out = self._run({"hono": "^4.13.16"}, audit_with("zod", "high"))
        self.assertEqual(code, 0, out)

    def test_unreadable_audit_is_exit_2(self):
        f = Fixture({"hono": "^4.13.16"}, {})
        try:
            with open(f.audit, "w") as fh:
                fh.write("not json")
            code, _out = f.run()
            self.assertEqual(code, 2)
        finally:
            f.close()

    def test_audit_without_vulnerabilities_key_is_exit_2(self):
        code, _out = self._run({"hono": "^4.13.16"}, {"error": "registry unreachable"})
        self.assertEqual(code, 2)

    def test_missing_package_json_is_exit_2(self):
        d = tempfile.mkdtemp()
        try:
            proc = subprocess.run([sys.executable, SCRIPT, d], capture_output=True, text=True)
            self.assertEqual(proc.returncode, 2)
        finally:
            shutil.rmtree(d)


class Helpers(unittest.TestCase):
    def test_is_exact(self):
        for spec in ("7.29.1", "=7.29.1", "v7.29.1", "1.0.0-beta.2"):
            self.assertTrue(pins.is_exact(spec), spec)
        for spec in ("^7.30.1", "~1.2.3", ">=1.2.3", "1.x", "*", "$hono", "npm:other@^1"):
            self.assertFalse(pins.is_exact(spec), spec)

    def test_flatten_overrides(self):
        flat = pins.flatten_overrides({"a": "^1.0.0", "p": {"b": "^2.0.0", ".": "^3.0.0"}, "c@1": "^4.0.0"})
        self.assertEqual(flat, [("a", "a", "^1.0.0"), ("p > b", "b", "^2.0.0"),
                                ("p > .", "p", "^3.0.0"), ("c@1", "c", "^4.0.0")])

    def test_package_name(self):
        self.assertEqual(pins._package_name("@scope/pkg@1.2"), "@scope/pkg")
        self.assertEqual(pins._package_name("@scope/pkg"), "@scope/pkg")
        self.assertEqual(pins._package_name("pkg@^1"), "pkg")

    def test_blocking_advisories(self):
        lines = pins.blocking_advisories(audit_with("hono", "high", "T"), ["hono", "zod"])
        self.assertEqual(len(lines), 1)
        self.assertTrue(lines[0].startswith("hono <9.9.9 [high]: T"))
        with self.assertRaises(pins.CannotRun):
            pins.blocking_advisories({}, ["hono"])

    def test_check_and_main_and_report(self):
        f = Fixture({"undici": "^7.30.1"}, {"vulnerabilities": {}})
        try:
            self.assertEqual(pins.check(f.dir, f.audit), 0)
            self.assertEqual(pins.main([f.dir, "--audit-json", f.audit]), 0)
        finally:
            f.close()
        self.assertTrue(callable(pins.report))

    def test_run_npm_audit_reports_cannot_run_without_npm(self):
        saved = os.environ.get("PATH", "")
        os.environ["PATH"] = ""
        try:
            with self.assertRaises(pins.CannotRun):
                pins.run_npm_audit(HERE)
        finally:
            os.environ["PATH"] = saved


if __name__ == "__main__":
    unittest.main(verbosity=1)
