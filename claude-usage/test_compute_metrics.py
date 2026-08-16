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
import json
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


class SessionConcurrency(unittest.TestCase):
    """Concurrency is counted from timestamps rather than summed, so the ways it
    can go wrong are its own: mis-bucketing, and crediting a resumed session with
    work it only replayed."""

    def setUp(self):
        self._glob = cm.PROJECTS_GLOB
        self._dir = tempfile.TemporaryDirectory()
        self.addCleanup(self._dir.cleanup)
        proj = os.path.join(self._dir.name, "proj")
        os.makedirs(proj)
        cm.PROJECTS_GLOB = os.path.join(self._dir.name, "*")
        self.proj = proj

    def tearDown(self):
        cm.PROJECTS_GLOB = self._glob

    def write(self, session, records):
        """records: (uuid, "HH:MM") on 2026-07-26."""
        with open(os.path.join(self.proj, f"{session}.jsonl"), "w") as fh:
            for uuid, hhmm in records:
                fh.write(json.dumps(
                    {"uuid": uuid, "timestamp": f"2026-07-26T{hhmm}:00.000Z"}) + "\n")

    def rows(self):
        return cm.session_series("mac-x")[("2026-07-26", "mac-x")]

    def test_sessions_in_one_bucket_are_concurrent(self):
        self.write("a", [("a1", "09:00")])
        self.write("b", [("b1", "09:07")])   # same 10-min bucket as a1
        r = self.rows()
        self.assertEqual(r["peak_concurrent"], 2)
        self.assertEqual(r["sessions"], 2)
        self.assertEqual(r["parallel_buckets"], 1)
        self.assertEqual(r["session_buckets"], 2)   # two sessions x one bucket

    def test_session_buckets_carries_mean_and_total(self):
        """The stored integer has to distinguish two sessions sharing a bucket from
        one session spanning two — same active_buckets, different concurrency."""
        self.write("a", [("a1", "09:00"), ("a2", "09:15")])   # two buckets, alone in one
        self.write("b", [("b1", "09:02")])                    # shares the first
        r = self.rows()
        self.assertEqual(r["active_buckets"], 2)
        self.assertEqual(r["session_buckets"], 3)             # 2 + 1
        self.assertEqual(r["session_buckets"] / r["active_buckets"], 1.5)   # mean concurrency
        # Total session time: buckets x bucket width.
        self.assertEqual(r["session_buckets"] * cm.SESSION_BUCKET_MIN, 30)

    def test_sessions_in_different_buckets_are_not(self):
        """Two sessions the same day are not two sessions at the same time."""
        self.write("a", [("a1", "09:00")])
        self.write("b", [("b1", "11:30")])
        r = self.rows()
        self.assertEqual(r["peak_concurrent"], 1)
        self.assertEqual(r["sessions"], 2)
        self.assertEqual(r["parallel_buckets"], 0)
        self.assertEqual(r["active_buckets"], 2)

    def test_a_replayed_record_does_not_invent_concurrency(self):
        """A resume replays the earlier session's records verbatim. Counting them
        as the resuming session's own work would show two sessions running at a
        time only one existed."""
        self.write("a", [("a1", "09:00")])
        self.write("b", [("a1", "09:00"), ("b1", "14:00")])  # b resumes a
        r = self.rows()
        self.assertEqual(r["peak_concurrent"], 1)
        self.assertEqual(r["parallel_buckets"], 0)

    def test_records_without_a_uuid_are_skipped(self):
        """Only uuid-bearing records can be recognised as replays, so they are the
        only ones counted — an untracked record would evade the dedup above."""
        with open(os.path.join(self.proj, "c.jsonl"), "w") as fh:
            fh.write(json.dumps({"timestamp": "2026-07-26T09:00:00.000Z"}) + "\n")
        self.assertEqual(cm.session_series("mac-x"), {})


class SessionKinds(unittest.TestCase):
    """The split runs on two clocks and one dedup, and each can go wrong alone."""

    def setUp(self):
        self._glob = cm.PROJECTS_GLOB
        self._dir = tempfile.TemporaryDirectory()
        self.addCleanup(self._dir.cleanup)
        self.proj = os.path.join(self._dir.name, "proj")
        os.makedirs(self.proj)
        cm.PROJECTS_GLOB = os.path.join(self._dir.name, "*")

    def tearDown(self):
        cm.PROJECTS_GLOB = self._glob

    def prompt(self, ts, text):
        return {"type": "user", "uuid": text[:8], "timestamp": ts,
                "origin": {"kind": "human"}, "message": {"content": text}}

    def spend(self, ts, msg_id, tokens):
        return {"type": "assistant", "uuid": msg_id, "timestamp": ts,
                "requestId": "r-" + msg_id,
                "message": {"id": msg_id, "usage": {"input_tokens": tokens}}}

    def write(self, session, records):
        with open(os.path.join(self.proj, f"{session}.jsonl"), "w") as fh:
            for r in records:
                fh.write(json.dumps(r) + "\n")

    def rows(self):
        return cm.session_kind_series("mac-x")

    def test_the_opening_prompt_decides_the_kind(self):
        self.write("a", [self.prompt("2026-07-26T09:00:00Z", "fix the flake"),
                         self.spend("2026-07-26T09:01:00Z", "m1", 100)])
        self.write("b", [self.prompt("2026-07-26T09:00:00Z",
                                     "Read `.claude/skills/session-worker/SKILL.md` first."),
                         self.spend("2026-07-26T09:01:00Z", "m2", 400)])
        r = self.rows()
        self.assertEqual(r[("2026-07-26", "mac-x", "manual")]["sessions"], 1)
        self.assertEqual(r[("2026-07-26", "mac-x", "manual")]["headline"], 100)
        self.assertEqual(r[("2026-07-26", "mac-x", "dispatched")]["headline"], 400)

    def test_a_session_with_no_human_prompt_is_its_own_kind(self):
        """Counting these as manual would credit a person with opening them."""
        self.write("a", [self.spend("2026-07-26T09:00:00Z", "m1", 100)])
        r = self.rows()
        self.assertEqual(r[("2026-07-26", "mac-x", "unprompted")]["sessions"], 1)
        self.assertNotIn(("2026-07-26", "mac-x", "manual"), r)

    def test_spend_lands_on_its_own_day_but_the_session_on_its_first(self):
        """A session running past midnight spends on both days. Crediting it all to
        the opening would move spend to a day it was not spent."""
        self.write("a", [self.prompt("2026-07-26T23:00:00Z", "keep going"),
                         self.spend("2026-07-26T23:30:00Z", "m1", 100),
                         self.spend("2026-07-27T00:30:00Z", "m2", 700)])
        r = self.rows()
        self.assertEqual(r[("2026-07-26", "mac-x", "manual")]["sessions"], 1)
        self.assertEqual(r[("2026-07-26", "mac-x", "manual")]["headline"], 100)
        self.assertEqual(r[("2026-07-27", "mac-x", "manual")]["sessions"], 0)
        self.assertEqual(r[("2026-07-27", "mac-x", "manual")]["headline"], 700)

    def test_a_replayed_record_is_credited_to_the_earlier_session(self):
        """A resume replays the earlier session's records verbatim. Counting them
        again would double the spend, and crediting them to the resuming session
        would move it to whichever kind resumed."""
        self.write("early", [self.prompt("2026-07-26T09:00:00Z", "start here"),
                             self.spend("2026-07-26T09:01:00Z", "m1", 100)])
        self.write("late", [self.prompt("2026-07-26T14:00:00Z",
                                        "Read `.claude/skills/dispatch-worker/SKILL.md` first."),
                            self.spend("2026-07-26T09:01:00Z", "m1", 100),   # replayed
                            self.spend("2026-07-26T14:01:00Z", "m2", 50)])
        # The walk order is forced to the wrong one. glob returns directory order,
        # which is creation order on one filesystem here and alphabetical on
        # another, so naming the files cannot reliably produce the order that
        # breaks this — and a test that only fails on some machines is one that
        # passes on luck on the rest. Handing the resuming session over first is
        # what makes the missing sort fail here rather than somewhere else.
        real = cm.glob.glob
        self.addCleanup(setattr, cm.glob, "glob", real)
        cm.glob.glob = lambda pat: sorted(real(pat), reverse=True)
        r = self.rows()
        self.assertEqual(r[("2026-07-26", "mac-x", "manual")]["headline"], 100)
        self.assertEqual(r[("2026-07-26", "mac-x", "dispatched")]["headline"], 50)


class PullRequestSubjects(unittest.TestCase):
    """``prs`` counts squash merges, which are recognised only by a trailing ``(#N)``.

    A subject that merely mentions an issue must not count, or the series silently
    inflates on exactly the commits that talk about PRs rather than being one.
    """

    def test_a_trailing_reference_is_a_merge(self):
        for subj in ("fix(agc): stand down a re-run (Q811) (#1515)",
                     "docs: reflow (#1)"):
            self.assertTrue(cm.PR_SUBJECT.search(subj), subj)

    def test_a_reference_anywhere_else_is_not(self):
        for subj in ("fix: revert the change from (#1515) that broke CI",
                     "docs: explain why #1515 was reverted",
                     "chore(metrics): refresh the snapshot (mac-2)",
                     "feat: add a (#) placeholder"):
            self.assertIsNone(cm.PR_SUBJECT.search(subj), subj)


class QueueClosures(unittest.TestCase):
    """``queue_closed`` counts a row leaving ``docs/STATUS.md``, once per id.

    The walk reads one ``git log -p`` stream, so these drive it through the module's
    own parser with a captured stream rather than asserting on a live repo, whose
    history would make the expected counts move under the test.
    """

    def setUp(self):
        self._git = cm.git

    def tearDown(self):
        cm.git = self._git

    def feed(self, stream):
        cm.git = lambda *a, **k: stream
        return cm.queue_flow()[0]   # this class asserts on closures only

    def test_a_removed_anchor_closes_on_its_date(self):
        closed = self.feed(
            '\x002026-06-01\n+| <a id="Q1"></a>Q1 | x |\n'
            '\x002026-06-02\n-| <a id="Q1"></a>Q1 | x |\n')
        self.assertEqual(dict(closed), {"2026-06-02": 1})

    def test_a_row_moved_within_the_file_has_not_closed(self):
        """Queue -> Deferred rewrites the row in place: gone from one table, present
        in the other. The anchor is on both sides of the diff, so nothing closed."""
        closed = self.feed(
            '\x002026-06-02\n-| <a id="Q2"></a>Q2 | queue |\n+| <a id="Q2"></a>Q2 | deferred |\n')
        self.assertEqual(dict(closed), {})

    def test_a_refiled_id_cannot_close_twice(self):
        """Q775's defect: a shipped id re-filed as a new row. Only the first removal
        counts, so landing both can't book the same work again."""
        closed = self.feed(
            '\x002026-06-02\n-| <a id="Q3"></a>Q3 | x |\n'
            '\x002026-06-05\n+| <a id="Q3"></a>Q3 | refiled |\n'
            '\x002026-06-09\n-| <a id="Q3"></a>Q3 | refiled |\n')
        self.assertEqual(dict(closed), {"2026-06-02": 1})

    def test_diff_file_headers_are_not_rows(self):
        closed = self.feed(
            '\x002026-06-02\n--- a/docs/STATUS.md\n+++ b/docs/STATUS.md\n'
            '-| <a id="Q4"></a>Q4 | x |\n')
        self.assertEqual(dict(closed), {"2026-06-02": 1})


class WordCounts(unittest.TestCase):
    """``grep_word_count`` is the reformat-proof half: same text, unit that a rewrap
    cannot move."""

    def setUp(self):
        self._git = cm.git

    def tearDown(self):
        cm.git = self._git

    def test_a_rewrap_leaves_the_word_count_alone(self):
        wrapped = "the quick brown\nfox jumps over\nthe lazy dog\n"
        unwrapped = "the quick brown fox jumps over the lazy dog\n"
        cm.git = lambda *a, **k: wrapped
        before = cm.grep_word_count("rev", "x", ["*.md"])
        cm.git = lambda *a, **k: unwrapped
        after = cm.grep_word_count("rev", "x", ["*.md"])
        self.assertEqual(before, after)
        self.assertEqual(before, 9)
        self.assertNotEqual(len(wrapped.splitlines()), len(unwrapped.splitlines()))


class QueueFlow(unittest.TestCase):
    """Both directions come from one walk, and a moved row is neither."""

    def setUp(self):
        self._git = cm.git

    def tearDown(self):
        cm.git = self._git

    def feed(self, stream):
        cm.git = lambda *a, **k: stream
        return cm.queue_flow()

    def test_added_is_filed_and_removed_is_closed(self):
        closed, filed = self.feed(
            '\x002026-06-01\n+| <a id="Q1"></a>Q1 | x |\n'
            '\x002026-06-02\n-| <a id="Q1"></a>Q1 | x |\n')
        self.assertEqual((dict(filed), dict(closed)),
                         ({"2026-06-01": 1}, {"2026-06-02": 1}))

    def test_a_moved_row_is_neither_filed_nor_closed(self):
        closed, filed = self.feed(
            '\x002026-06-02\n-| <a id="Q2"></a>Q2 | queue |\n+| <a id="Q2"></a>Q2 | deferred |\n')
        self.assertEqual((dict(filed), dict(closed)), ({}, {}))


class ConventionalSubjects(unittest.TestCase):
    """The churn ratio counts commit types, so the type has to be the subject's."""

    def test_types_are_read_from_the_prefix(self):
        for subj, want in (("feat(agc): add a thing", "feat"),
                           ("fix: repair it", "fix"),
                           ("docs(metrics): explain", "docs")):
            m = cm.CONVENTIONAL.match(subj)
            self.assertIsNotNone(m, subj)
            self.assertEqual(m.group(1), want)

    def test_a_type_named_mid_subject_is_not_the_type(self):
        m = cm.CONVENTIONAL.match("docs: why we fix: things")
        self.assertEqual(m.group(1), "docs")


class AuthoredPrompts(unittest.TestCase):
    """Both dispatcher opening shapes are machine-composed, and the second one is
    why the split is a set of tests rather than one prefix."""

    def rec(self, text, kind="human"):
        return {"type": "user", "timestamp": "2026-08-10T00:00:00Z",
                "origin": {"kind": kind}, "message": {"content": text}}

    def test_a_persona_opening_is_not_authored(self):
        r = self.rec("You are a worker session in a parallel-dispatch run. ...")
        self.assertTrue(cm.is_human_prompt(r))
        self.assertFalse(cm.is_authored_prompt(r))

    def test_a_prose_opening_naming_the_worker_skill_is_not_authored(self):
        for skill in cm.WORKER_SKILLS:
            r = self.rec("Work Q741 from the Queue in docs/STATUS.md. Read "
                         "`.claude/skills/%s/SKILL.md` first for the worker "
                         "contract." % skill)
            self.assertFalse(cm.is_authored_prompt(r), skill)

    def test_a_prose_opening_is_still_presence(self):
        r = self.rec("Read `.claude/skills/session-worker/SKILL.md` first.")
        self.assertTrue(cm.is_human_prompt(r),
                        "accepting a chip is a keystroke, so it counts as presence")

    def test_an_ordinary_prompt_is_authored(self):
        for text in ("update the claude usage metrics",
                     "Work Q741 from the Queue in docs/STATUS.md.",
                     "the worker contract is fine, ship it"):
            self.assertTrue(cm.is_authored_prompt(self.rec(text)), text)

    def test_the_persona_prefix_misfiles_a_prompt_that_opens_that_way(self):
        # Known and accepted: the prefix is broad, and the era it covers is closed
        # (unused since 2026-08-04), so tightening it would only re-cut history.
        self.assertFalse(cm.is_authored_prompt(self.rec("You are right, drop it.")))

    def test_a_renamed_skill_leaves_the_older_name_classified(self):
        self.assertIn("dispatch-worker", cm.WORKER_SKILLS)
        self.assertIn("session-worker", cm.WORKER_SKILLS)
        old = "Read `.claude/skills/dispatch-worker/SKILL.md` first."
        self.assertFalse(cm.is_authored_prompt(self.rec(old)))


class PullRequestFetch(unittest.TestCase):
    """The fetch must fail soft, and must never re-ask for what it already holds."""

    def setUp(self):
        self._run = cm.subprocess.run

    def tearDown(self):
        cm.subprocess.run = self._run

    def test_a_budget_under_the_floor_skips_the_fetch(self):
        calls = []

        class R:
            stdout = str(cm.PR_RATE_FLOOR - 1)

        def fake(cmd, *a, **k):
            calls.append(cmd)
            return R()

        cm.subprocess.run = fake
        self.assertEqual(cm.pr_series(0), {})
        self.assertEqual(len(calls), 1, "must not call gh pr list after skipping")

    def test_a_broken_gh_leaves_the_caller_empty_handed(self):
        def fake(cmd, *a, **k):
            raise OSError("no gh")
        cm.subprocess.run = fake
        self.assertEqual(cm.pr_series(0), {})

    def test_rows_at_or_below_the_high_water_mark_are_not_refetched(self):
        class R:
            def __init__(self, out):
                self.stdout = out

        def fake(cmd, *a, **k):
            if "rate_limit" in cmd:
                return R("5000")
            return R(json.dumps([
                {"number": 7, "createdAt": "2026-08-01T00:00:00Z",
                 "mergedAt": "2026-08-01T02:00:00Z"},
                {"number": 5, "createdAt": "2026-07-01T00:00:00Z",
                 "mergedAt": "2026-07-01T01:00:00Z"},
            ]))

        cm.subprocess.run = fake
        got = cm.pr_series(5)
        self.assertEqual(list(got), [7])
        self.assertEqual(got[7]["cycle_hours"], "2.0000")


if __name__ == "__main__":
    unittest.main()
