#!/usr/bin/env python3
"""Tests for tools/check_sdk_versions.py's TypeScript VERSION rule.

The exported VERSION constant said 0.0.4 while the package was 0.9.0,
because nothing compared it with anything (outside review, 2026-11-15).
These tests hold the rule: the check reads VERSION, and fails when it
disagrees with package.json.

Run:  python3 tools/test_check_sdk_versions.py   (standard library only)
"""

from __future__ import annotations

import importlib.util
import io
import os
import tempfile
import unittest
from contextlib import redirect_stdout

HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location("check_sdk_versions", os.path.join(HERE, "check_sdk_versions.py"))
mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(mod)


class TsExportedVersion(unittest.TestCase):
    def setUp(self) -> None:
        self.saved = mod.INDEX_TS
        self.tmp = tempfile.NamedTemporaryFile("w", suffix=".ts", delete=False)

    def tearDown(self) -> None:
        mod.INDEX_TS = self.saved
        os.unlink(self.tmp.name)

    def _index(self, text: str) -> None:
        self.tmp.write(text)
        self.tmp.flush()
        mod.INDEX_TS = self.tmp.name

    def test_reads_the_exported_constant(self) -> None:
        self._index('export { wrap } from "./wrap.js";\nexport const VERSION = "1.2.3";\n')
        self.assertEqual(mod.read_ts_exported_version(), "1.2.3")

    def test_missing_constant_is_none(self) -> None:
        self._index('export { wrap } from "./wrap.js";\n')
        self.assertIsNone(mod.read_ts_exported_version())

    def test_stale_version_fails_the_check(self) -> None:
        real = mod.read_package_version()
        self._index('export const VERSION = "0.0.4";\n')
        self.assertNotEqual(real, "0.0.4")
        out = io.StringIO()
        with redirect_stdout(out):
            code = self._run_main()
        self.assertEqual(code, 1)
        self.assertIn("exports VERSION=0.0.4", out.getvalue())

    def _run_main(self) -> int:
        import sys
        saved_argv = sys.argv
        sys.argv = ["check_sdk_versions.py"]
        try:
            return mod.main()
        finally:
            sys.argv = saved_argv

    def test_real_repository_agrees(self) -> None:
        out = io.StringIO()
        with redirect_stdout(out):
            code = self._run_main()
        self.assertEqual(code, 0, out.getvalue())

    def test_report_writes_one_line(self) -> None:
        out = io.StringIO()
        mod.report("a", 1, file=out)
        self.assertEqual(out.getvalue(), "a 1\n")


if __name__ == "__main__":
    unittest.main(verbosity=1)
