#!/usr/bin/env python3
"""Tests for tools/check_detector_test_coverage.py's two file readers.

The gate's hand-kept class list missed two classes until 2026-11-16
(outside review finding E16); it now reads store.go. On the same day
detector tests began to live in more than one file. These tests hold both:
the class list comes from store.go, and the gate reads every
test_detectors*.py file, with the canonical one first.

Run:  python3 tools/test_check_detector_test_coverage.py   (standard library only)
"""

from __future__ import annotations

import importlib.util
import os
import tempfile
import unittest
from pathlib import Path

HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location(
    "check_detector_test_coverage", os.path.join(HERE, "check_detector_test_coverage.py"))
mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(mod)


class StoreFailureClassesTests(unittest.TestCase):
    def test_reads_every_class_from_store_go(self):
        classes = mod._store_failure_classes()
        for c in ("environment_misapprehension", "covert_coordination", "record_integrity", "crashes"):
            self.assertIn(c, classes)
        self.assertEqual(len(classes), len(set(classes)))
        self.assertEqual(mod.CANONICAL_DETECTORS, classes)


class DetectorTestFilesTests(unittest.TestCase):
    def test_reads_the_canonical_file_first_then_the_split_files(self):
        with tempfile.TemporaryDirectory() as d:
            integration = Path(d) / "backend" / "test" / "integration"
            integration.mkdir(parents=True)
            for name in ("test_detectors.py", "test_detectors_egress.py", "test_detectors_b.py",
                         "test_other.py"):
                (integration / name).write_text("")
            got = [p.name for p in mod._detector_test_files(Path(d))]
        self.assertEqual(got, ["test_detectors.py", "test_detectors_b.py", "test_detectors_egress.py"])

    def test_the_canonical_file_is_listed_even_when_missing(self):
        with tempfile.TemporaryDirectory() as d:
            got = mod._detector_test_files(Path(d))
        self.assertEqual([p.name for p in got], ["test_detectors.py"])

    def test_this_repository_passes(self):
        self.assertEqual(mod.main(), 0)


if __name__ == "__main__":
    unittest.main()
