#!/usr/bin/env python3
"""Tests for the merge semantics in ``compute_metrics.py``.

    python3 -m unittest discover -s claude-usage

The merge rule is the load-bearing property of this dataset: it decides what a
re-run may and may not change about already-committed history. These pin the two
directions it has to get right — never revise one machine's day downward, and
never collapse two machines' shares of the same day into the larger of the two.
"""

import csv
import importlib.util
import os
import tempfile
import unittest

_SPEC = importlib.util.spec_from_file_location(
    "compute_metrics",
    os.path.join(os.path.dirname(os.path.abspath(__file__)), "compute_metrics.py"),
)
cm = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(cm)

TNUM = ["input", "output", "cache_creation", "cache_read", "assistant_msgs", "user_msgs"]
TKEY = ["date", "host"]


def row(date, host, **vals):
    """A token row with every numeric column zeroed except those named."""
    return {"date": date, "host": host, **{c: 0 for c in TNUM}, **vals}


def total(merged, date, col="input"):
    return cm.sum_by_date(merged.values(), TNUM)[date][col]


class MergeAcrossMachines(unittest.TestCase):
    def test_two_machines_on_one_day_sum(self):
        """The bug the host dimension exists to fix: a MAX here drops the smaller
        machine's share of the day permanently."""
        merged = {("2026-07-26", "mac-1"): row("2026-07-26", "mac-1", input=60)}
        cm.merge_max_into(
            merged, {("2026-07-26", "mac-2"): row("2026-07-26", "mac-2", input=40)}, TKEY, TNUM)
        self.assertEqual(sorted(merged), [("2026-07-26", "mac-1"), ("2026-07-26", "mac-2")])
        self.assertEqual(total(merged, "2026-07-26"), 100)

    def test_one_machine_never_revises_down_or_duplicates(self):
        """A re-run after that machine's sessions were archived sees less, and
        must neither lower the recorded value nor add a second row."""
        merged = {("2026-07-26", "mac-1"): row("2026-07-26", "mac-1", input=60)}
        cm.merge_max_into(
            merged, {("2026-07-26", "mac-1"): row("2026-07-26", "mac-1", input=25)}, TKEY, TNUM)
        self.assertEqual(len(merged), 1)
        self.assertEqual(total(merged, "2026-07-26"), 60)

    def test_history_from_an_absent_machine_survives(self):
        """Running on a machine that holds none of the other's transcripts keeps
        the other's days intact."""
        merged = {("2026-06-01", "mac-1"): row("2026-06-01", "mac-1", input=60)}
        cm.merge_max_into(
            merged, {("2026-07-26", "mac-2"): row("2026-07-26", "mac-2", input=40)}, TKEY, TNUM)
        self.assertEqual(total(merged, "2026-06-01"), 60)
        self.assertEqual(total(merged, "2026-07-26"), 40)


class LegacyRows(unittest.TestCase):
    def test_hostless_rows_load_as_the_legacy_machine(self):
        """Rows predating the host column must key as LEGACY_HOST — otherwise the
        machine that wrote them re-measures the same history under its own id and
        the two copies are summed."""
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "token_metrics.csv")
            with open(path, "w", newline="") as fh:
                w = csv.DictWriter(fh, fieldnames=["date"] + TNUM + ["estimated"])
                w.writeheader()
                w.writerow({"date": "2026-06-01", **{c: 0 for c in TNUM},
                            "input": 60, "estimated": 0})

            merged = cm.load_measured(path, TKEY, TNUM, {"host": cm.LEGACY_HOST})
            self.assertEqual(list(merged), [("2026-06-01", cm.LEGACY_HOST)])

            cm.merge_max_into(
                merged,
                {("2026-06-01", cm.LEGACY_HOST): row("2026-06-01", cm.LEGACY_HOST, input=60)},
                TKEY, TNUM)
            self.assertEqual(len(merged), 1)
            self.assertEqual(total(merged, "2026-06-01"), 60)


class HostResolution(unittest.TestCase):
    def setUp(self):
        self._env = os.environ.pop("CLAUDE_METRICS_HOST", None)
        self._file = cm.HOST_FILE

    def tearDown(self):
        cm.HOST_FILE = self._file
        os.environ.pop("CLAUDE_METRICS_HOST", None)
        if self._env is not None:
            os.environ["CLAUDE_METRICS_HOST"] = self._env

    def test_env_overrides_the_file(self):
        cm.HOST_FILE = os.devnull
        os.environ["CLAUDE_METRICS_HOST"] = "mac-9"
        self.assertEqual(cm.resolve_host(), "mac-9")

    def test_falls_back_to_the_local_file(self):
        with tempfile.TemporaryDirectory() as d:
            cm.HOST_FILE = os.path.join(d, "host")
            with open(cm.HOST_FILE, "w") as fh:
                fh.write("mac-7\n")
            self.assertEqual(cm.resolve_host(), "mac-7")

    def test_aborts_rather_than_guessing(self):
        """No hostname fallback: an id that drifts double-counts silently, so an
        unconfigured machine must fail loudly instead."""
        with tempfile.TemporaryDirectory() as d:
            cm.HOST_FILE = os.path.join(d, "absent")
            with self.assertRaises(SystemExit):
                cm.resolve_host()

    def test_rejects_ids_that_would_corrupt_the_csv(self):
        cm.HOST_FILE = os.devnull
        for bad in ("mac 1", "mac,1", 'mac"1', cm.EST_HOST):
            os.environ["CLAUDE_METRICS_HOST"] = bad
            with self.assertRaises(SystemExit, msg=bad):
                cm.resolve_host()


class ModelFamilies(unittest.TestCase):
    """An unmapped id lands in ``Other``, and ``model_daily.csv`` is
    merge-preserved — so the mislabelled rows survive every later run. These pin
    the ids actually seen in the transcripts to their display families."""

    def test_known_ids_map_to_their_family(self):
        for raw, want in (
            ("claude-sonnet-4-6", "Sonnet 4.6"),
            ("claude-opus-4-7", "Opus 4.7"),
            ("claude-opus-4-8", "Opus 4.8"),
            ("claude-opus-5", "Opus 5"),
            ("claude-fable-5", "Fable 5"),
            ("claude-haiku-4-5-20251001", "Haiku 4.5"),
        ):
            self.assertEqual(cm.model_family(raw), want, raw)

    def test_unmapped_and_missing_ids(self):
        self.assertEqual(cm.model_family("<synthetic>"), "Other")
        self.assertEqual(cm.model_family(""), "Unknown")
        self.assertEqual(cm.model_family(None), "Unknown")


if __name__ == "__main__":
    unittest.main()
