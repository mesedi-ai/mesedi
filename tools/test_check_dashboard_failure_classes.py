#!/usr/bin/env python3
"""Tests for tools/check_dashboard_failure_classes.py.

The dashboard's tier lists missed three failure classes because nothing
compared them with the backend (the outside review of 2026-11-15). These tests
hold the check: a class in no tier fails, a class in two tiers fails, a
tier naming an unknown class fails, and commented-out entries do not
count.

Run:  python3 tools/test_check_dashboard_failure_classes.py   (standard library only)
"""

from __future__ import annotations

import importlib.util
import os
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location(
    "check_dashboard_failure_classes", os.path.join(HERE, "check_dashboard_failure_classes.py"))
mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(mod)

TS = '''
const TIER_1_TOKEN_CLASSES = new Set<string>([
  "token_waste",
]);
export const TIER_2_COST_CLASSES = new Set<string>([
  "crashes",
  // "loops",
]);
const TIER_3_HIDE_CLASSES = new Set<string>([
  "record_integrity",
]);
'''


class DashboardFailureClassTests(unittest.TestCase):
    def test_every_class_in_one_tier_passes(self):
        tiers = mod.dashboard_tiers(TS)
        self.assertEqual(mod.problems(["token_waste", "crashes", "record_integrity"], tiers), [])

    def test_a_class_in_no_tier_fails(self):
        got = mod.problems(["token_waste", "crashes", "record_integrity", "covert_coordination"],
                           mod.dashboard_tiers(TS))
        self.assertEqual(got, ["covert_coordination has no display tier on the dashboard"])

    def test_a_commented_out_entry_does_not_count(self):
        got = mod.problems(["token_waste", "crashes", "record_integrity", "loops"], mod.dashboard_tiers(TS))
        self.assertEqual(got, ["loops has no display tier on the dashboard"])

    def test_a_class_in_two_tiers_fails(self):
        ts = TS.replace('"crashes",', '"crashes",\n  "record_integrity",')
        got = mod.problems(["token_waste", "crashes", "record_integrity"], mod.dashboard_tiers(ts))
        self.assertEqual(len(got), 1)
        self.assertIn("record_integrity is in more than one tier", got[0])

    def test_a_tier_naming_an_unknown_class_fails(self):
        got = mod.problems(["token_waste", "crashes"], mod.dashboard_tiers(TS))
        self.assertEqual(got, ["TIER_3_HIDE_CLASSES names record_integrity, which is not a backend failure class"])

    def test_a_missing_tier_set_fails(self):
        ts = TS.replace("TIER_1_TOKEN_CLASSES", "TIER_ONE")
        got = mod.problems(["crashes", "record_integrity"], mod.dashboard_tiers(ts))
        self.assertIn("TIER_1_TOKEN_CLASSES is not defined as a Set in failureClass.ts", got)

    def test_backend_classes_are_read_from_store_go(self):
        with open(mod.STORE_GO, encoding="utf-8") as fh:
            classes = mod.backend_classes(fh.read())
        self.assertIn("record_integrity", classes)
        self.assertIn("covert_coordination", classes)
        self.assertEqual(len(classes), len(set(classes)))


if __name__ == "__main__":
    unittest.main()
