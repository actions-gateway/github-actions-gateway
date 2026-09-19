#!/usr/bin/env bash
#
# Unit tests for scripts/agent/pr-requeue-eligible.py.
#
# The GitHub reads are stubbed by returning *raw JSON*, the same shape gh
# prints, and the script parses it in-process. That is the point, and it is what
# closed Q694: a stub returning post-filter text left the parsing untested, so a
# malformed read reached production instead of the suite.
#
# The merge probe is not stubbed at all. It runs `git merge-tree` against real
# commits in a real temporary repository, so a conflict here is one git actually
# found.
#
# Q834 is covered by the fixture rather than by a named case: the repo is parked
# on `main` while the assessed head is `feature`, so an implementation reading
# the caller's HEAD would measure a clean merge and report no conflict. The
# control below asserts that parked checkout really does merge clean, without
# which the coverage would hold only by accident.
set -euo pipefail
shopt -s inherit_errexit

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
S="$HERE/pr-requeue-eligible.py"

if python3 - "$S" <<'PY'
import importlib.util
import json
import subprocess
import sys
import tempfile
from pathlib import Path

spec = importlib.util.spec_from_file_location("r", sys.argv[1])
r = importlib.util.module_from_spec(spec)
spec.loader.exec_module(r)

fails = []


def check(name, got, want):
    if got == want:
        print(f"ok   {name}")
    else:
        fails.append(f"{name}: got {got!r} want {want!r}")


def ok(name):
    print(f"ok   {name}")


# --- a real repository, so the merge probe measures a real merge -----------

def git(args, cwd):
    p = subprocess.run(["git"] + args, cwd=cwd, capture_output=True, text=True)
    return p.returncode, p.stdout, p.stderr


def build_repo(tmp, conflict_paths):
    """base and head that genuinely conflict on each named path."""
    repo = Path(tmp) / "repo"
    repo.mkdir()
    git(["init", "-q", "-b", "main"], repo)
    # Q820: no detached maintenance racing the next command in a fixture repo.
    git(["config", "maintenance.auto", "false"], repo)
    git(["config", "user.email", "t@example.com"], repo)
    git(["config", "user.name", "T"], repo)
    for p in conflict_paths:
        f = repo / p
        f.parent.mkdir(parents=True, exist_ok=True)
        f.write_text("base\n")
    git(["add", "-A"], repo)
    git(["commit", "-qm", "base"], repo)
    root = git(["rev-parse", "HEAD"], repo)[1].strip()

    git(["checkout", "-q", "-b", "feature"], repo)
    for p in conflict_paths:
        (repo / p).write_text("theirs\n")
    git(["add", "-A"], repo)
    git(["commit", "-qm", "feature"], repo)
    head = git(["rev-parse", "HEAD"], repo)[1].strip()

    git(["checkout", "-q", "main"], repo)
    for p in conflict_paths:
        (repo / p).write_text("ours\n")
    git(["add", "-A"], repo)
    git(["commit", "-qm", "main moves"], repo)
    base = git(["rev-parse", "HEAD"], repo)[1].strip()
    # assess() resolves the base as origin/<branch>, which is what production
    # does; the fixture has to carry that ref rather than the script relaxing.
    git(["update-ref", "refs/remotes/origin/main", base], repo)
    return repo, root, base, head


# --- a gh stub that answers with raw JSON ----------------------------------

class FakeGh(r.Gh):
    def __init__(self, pr=1, state="OPEN", draft=False, base="main", head=None,
                 queued=False, enqueued_by="User", pages=1, fail=None):
        self.calls = []
        self._cfg = dict(state=state, draft=draft, base=base, head=head,
                         queued=queued, enqueued_by=enqueued_by, pages=pages,
                         fail=fail)
        super().__init__(pr, repo="o/n", run=self._run)

    def _run(self, cmd):
        joined = " ".join(cmd)
        c = self._cfg
        if "pr" in cmd and "view" in cmd:
            kind = "view"
        elif "graphql" in joined:
            kind = "graphql"
        elif "timeline" in joined:
            kind = "timeline"
        else:
            kind = "repo"
        self.calls.append(kind)
        if c["fail"] == kind:
            return 1, "", "simulated transport failure"
        if kind == "view":
            return 0, json.dumps({"state": c["state"], "isDraft": c["draft"],
                                  "baseRefName": c["base"],
                                  "headRefOid": c["head"]}), ""
        if kind == "graphql":
            entry = {"state": "QUEUED"} if c["queued"] else None
            return 0, json.dumps({"data": {"repository": {"pullRequest": {
                "id": "PR_kwTEST", "mergeQueueEntry": entry}}}}), ""
        if kind == "timeline":
            # One page of noise, then the enqueue on the LAST page — a reader
            # that stops after page one reports "nobody enqueued it".
            noise = [{"event": "committed"}] * 2
            hit = [{"event": "added_to_merge_queue",
                    "actor": {"type": c["enqueued_by"]}}]
            pages = [noise] * (c["pages"] - 1) + [hit]
            return 0, json.dumps(pages), ""
        return 0, json.dumps({"nameWithOwner": "o/n"}), ""


# --- .gitattributes drives ownership --------------------------------------

with tempfile.TemporaryDirectory() as tmp:
    ga = Path(tmp) / ".gitattributes"
    ga.write_text(
        "# a comment\n"
        "docs/STATUS.md merge=backlog\n"
        "docs/plan/README.md   merge=planindex\n"
        "*.png binary\n"
        "docs/roadmap.md merge=roadmap\n")
    owned, names = r.driver_config(ga)
    check("gitattributes: owned paths derived", owned,
          ["docs/STATUS.md", "docs/plan/README.md", "docs/roadmap.md"])
    check("gitattributes: driver names derived", names,
          ["backlog", "planindex", "roadmap"])
    check("gitattributes: a non-merge attribute is ignored",
          "*.png" in owned, False)
    check("gitattributes: a missing file yields nothing",
          r.driver_config(Path(tmp) / "nope"), ([], []))

# --- the probe: driver-owned vs not ---------------------------------------

with tempfile.TemporaryDirectory() as tmp:
    repo, _, base, head = build_repo(tmp, ["docs/STATUS.md"])
    ga = repo / ".gitattributes"
    ga.write_text("docs/STATUS.md merge=backlog\n")

    def rgit(args, cwd=None):
        return git(args, repo)

    # Q834's control. The fixture is parked on `main`, so an implementation
    # reading the caller's HEAD instead of the PR's would measure this clean
    # merge and report no conflict. Asserting the checkout really is clean is
    # what stops the case below passing for the wrong reason.
    parked = git(["rev-parse", "--abbrev-ref", "HEAD"], repo)[1].strip()
    check("Q834 control: the fixture is parked off the PR's head",
          parked, "main")
    check("Q834 control: and that parked checkout merges clean",
          r.conflicting_paths(base, parked, ["backlog"], rgit), [])

    paths = r.conflicting_paths(base, head, ["backlog"], rgit)
    check("probe: finds the real conflict", paths, ["docs/STATUS.md"])

    gh = FakeGh(head=head, enqueued_by="User")
    rec = Path(tmp) / "v" / "1.verdict"
    out = r.assess(1, gh, rec, ga, rgit)
    check("driver-owned conflict is ELIGIBLE", "ELIGIBLE" in out, True)
    check("the verdict carries a re-runnable measurement",
          f"merge-tree --write-tree {base} {head}" in out, True)

with tempfile.TemporaryDirectory() as tmp:
    repo, _, base, head = build_repo(tmp, ["docs/STATUS.md", "cmd/main.go"])
    ga = repo / ".gitattributes"
    ga.write_text("docs/STATUS.md merge=backlog\n")

    def rgit(args, cwd=None):
        return git(args, repo)

    gh = FakeGh(head=head)
    rec = Path(tmp) / "v" / "1.verdict"
    try:
        r.assess(1, gh, rec, ga, rgit)
        fails.append("a code conflict was not refused")
    except r.Wake as w:
        check("a conflict outside driver-owned files wakes",
              "cmd/main.go" in w.reason, True)
        check("the wake still carries the measurement",
              "merge-tree --write-tree" in w.measured, True)

# --- Q814: the probe runs before the eligibility checks -------------------
# The ordinary dirty wake is a session healing its own not-yet-enqueued PR. It
# fails human_enqueued, and that is exactly the wake whose record used to carry
# no OIDs and no conflict set.

with tempfile.TemporaryDirectory() as tmp:
    repo, _, base, head = build_repo(tmp, ["docs/STATUS.md"])
    ga = repo / ".gitattributes"
    ga.write_text("docs/STATUS.md merge=backlog\n")

    def rgit(args, cwd=None):
        return git(args, repo)

    gh = FakeGh(head=head, enqueued_by="Bot")   # nobody human enqueued it
    rec = Path(tmp) / "v" / "1.verdict"
    try:
        r.assess(1, gh, rec, ga, rgit)
        fails.append("a bot-only enqueue was not refused")
    except r.Wake:
        pass
    body = rec.read_text()
    check("Q814: an ineligible assess still records the base OID",
          f"base_oid {base}" in body, True)
    check("Q814: and the head OID", f"head_oid {head}" in body, True)
    check("Q814: and the conflict set",
          "conflict docs/STATUS.md" in body, True)

# --- Q828: an unrecognised record is skew, not corruption ------------------

with tempfile.TemporaryDirectory() as tmp:
    rec = Path(tmp) / "1.verdict"
    rec.write_text("ELIGIBLE main\n")          # the pre-versioning shape
    try:
        r.read_last_record(rec)
        fails.append("an old-format record was accepted")
    except r.Wake as w:
        check("Q828: an old record is reported as format skew",
              "predates version" in w.reason or "format version" in w.reason,
              True)
        check("Q828: and never as an empty verdict",
              "'', not ELIGIBLE" not in w.reason, True)

    rec.write_text("version 99\nverdict ELIGIBLE\nbase main\n")
    try:
        r.read_last_record(rec)
        fails.append("a future-version record was accepted")
    except r.Wake as w:
        check("Q828: a newer record names both versions",
              "99" in w.reason and str(r.RECORD_VERSION) in w.reason, True)

    rec.write_text("")
    try:
        r.read_last_record(rec)
        fails.append("an empty record was accepted")
    except r.Wake:
        ok("Q828: an empty record wakes rather than parsing to nothing")

# --- a read that never happened is never a verdict ------------------------

with tempfile.TemporaryDirectory() as tmp:
    repo, _, base, head = build_repo(tmp, ["docs/STATUS.md"])
    ga = repo / ".gitattributes"
    ga.write_text("docs/STATUS.md merge=backlog\n")

    def rgit(args, cwd=None):
        return git(args, repo)

    for kind, what in (("view", "the PR state read"),
                       ("graphql", "the queue read"),
                       ("timeline", "the timeline read")):
        gh = FakeGh(head=head, fail=kind)
        rec = Path(tmp) / "v" / "1.verdict"
        try:
            r.assess(1, gh, rec, ga, rgit)
            fails.append(f"{what} failed and still produced a verdict")
        except r.Unmeasurable:
            ok(f"a failed {what} exits unmeasurable, not a verdict")
        except r.Wake:
            fails.append(f"{what} failed and was reported as a refusal")

    # A merge probe that did not run must not read as a clean merge.
    def broken_git(args, cwd=None):
        if "merge-tree" in args:
            return 128, "", "fatal: not a tree object"
        return git(args, repo)

    try:
        r.conflicting_paths(base, head, ["backlog"], broken_git)
        fails.append("a probe that never ran reported a clean merge")
    except r.Unmeasurable:
        ok("a merge probe that did not run is unmeasurable, not clean")

# --- the queue read distinguishes 'not queued' from 'never read' ----------

with tempfile.TemporaryDirectory() as tmp:
    gh = FakeGh(head="0" * 40)
    check("not queued reads as false", gh.in_queue(), False)
    gh = FakeGh(head="0" * 40, queued=True)
    check("a live queue entry reads as true", gh.in_queue(), True)

    class NoId(FakeGh):
        def _run(self, cmd):
            if "graphql" in " ".join(cmd):
                return 0, json.dumps({"data": {"repository": {
                    "pullRequest": {"mergeQueueEntry": None}}}}), ""
            return super()._run(cmd)

    try:
        NoId(head="0" * 40).in_queue()
        fails.append("an answer with no PR id was read as 'not queued'")
    except r.Unmeasurable:
        ok("an answer carrying no PR id is unmeasurable, not 'not queued'")

# --- the timeline is read to the last page --------------------------------

check("a bot enqueue does not count",
      FakeGh(head="0" * 40, enqueued_by="Bot").human_enqueued(), False)
check("a human enqueue counts",
      FakeGh(head="0" * 40, enqueued_by="User").human_enqueued(), True)
check("an enqueue on the last of several pages is still found",
      FakeGh(head="0" * 40, enqueued_by="User", pages=4).human_enqueued(), True)

# --- confirm fails closed -------------------------------------------------
#
# Every confirm case below varies the LIVE head against the recorded one. The
# suite this replaced held it at "0"*40 in every case while the records said
# "b"*40, so the happy path passed with the two already disagreeing — the
# fixture encoded the defect it was meant to exclude.

RECORDED_HEAD = "b" * 40

with tempfile.TemporaryDirectory() as tmp:
    rec = Path(tmp) / "1.verdict"
    gh = FakeGh(head=RECORDED_HEAD, enqueued_by="User")
    try:
        r.confirm(1, gh, rec)
        fails.append("confirm passed with no record at all")
    except r.Wake:
        ok("confirm with no record wakes")

    r.write_record(rec, "WAKE", "something", "main", "a" * 40, RECORDED_HEAD, [])
    try:
        r.confirm(1, gh, rec)
        fails.append("confirm passed on a recorded WAKE")
    except r.Wake:
        ok("confirm on a recorded WAKE wakes")

    r.write_record(rec, "ELIGIBLE", "clean", "main", "a" * 40, RECORDED_HEAD, [])
    out = r.confirm(1, gh, rec)
    check("confirm passes on the last record, not the first",
          "ELIGIBLE" in out, True)
    check("confirm replays the assessment's measurement",
          "a" * 40 in out and RECORDED_HEAD in out, True)
    # A replayed pair printed as a bare "measured:" reads as provenance this run
    # took, which is how two superseded OIDs passed for current.
    check("confirm dates the replay rather than claiming a measurement",
          "replaying the assessment recorded at" in out and "\nmeasured:" not in out,
          True)

    # The head moving is what invalidates an assessment: it measured a merge
    # between two named commits and says nothing about any other head.
    moved_head = FakeGh(head="c" * 40, enqueued_by="User")
    try:
        r.confirm(1, moved_head, rec)
        fails.append("confirm ignored a head that moved since the assessment")
    except r.Wake as w:
        check("confirm refuses when the head moved",
              "c" * 40 in w.reason and RECORDED_HEAD in w.reason, True)

    # A retarget is the base change that invalidates the merge. The base branch
    # NAME is what baseRefName carries, and it survives both a rebase and the
    # base commit advancing, so this check sees a retarget and nothing else.
    retargeted = FakeGh(head=RECORDED_HEAD, base="release-1.5", enqueued_by="User")
    try:
        r.confirm(1, retargeted, rec)
        fails.append("confirm ignored a retarget")
    except r.Wake as w:
        check("confirm refuses when the PR was retargeted",
              "retargeted" in w.reason, True)

    # Already queued means there is nothing to restore.
    queued = FakeGh(head=RECORDED_HEAD, enqueued_by="User", queued=True)
    try:
        r.confirm(1, queued, rec)
        fails.append("confirm re-enqueued a PR already in the queue")
    except r.Wake:
        ok("confirm refuses a PR already in the queue")

# --- --rebased re-binds the record to the commit the rebase produced -------
#
# The rebase always moves the head, so a confirm that wakes on head drift would
# refuse every re-enqueue without this mode. These cases are what make the head
# check usable rather than merely strict.

def build_rebase_repo(tmp):
    """A dirty pair, plus a clean head standing in for the rebase result."""
    repo, _, base, head = build_repo(tmp, ["docs/STATUS.md"])
    git(["checkout", "-q", "-b", "rebased", "main"], repo)
    (repo / "unrelated.txt").write_text("new\n")
    git(["add", "unrelated.txt"], repo)
    git(["commit", "-qm", "rebased"], repo)
    clean = git(["rev-parse", "HEAD"], repo)[1].strip()
    git(["checkout", "-q", "main"], repo)
    # Written last and left untracked: staged on a branch, checking main back
    # out deletes it, and driver_config then reads nothing and reports every
    # conflict as unowned.
    (repo / ".gitattributes").write_text("docs/STATUS.md merge=backlog\n")
    return repo, base, head, clean


with tempfile.TemporaryDirectory() as tmp:
    repo, base, head, clean = build_rebase_repo(tmp)
    ga = repo / ".gitattributes"

    def rgit(args, cwd=None):
        return git(args, repo)

    rec = Path(tmp) / "v" / "1.verdict"
    r.assess(1, FakeGh(head=head, enqueued_by="User"), rec, ga, rgit)

    # Before --rebased, the recorded head is the pre-rebase one, so confirm on
    # the rebased head has to wake. This is the live PR #1965 case.
    try:
        r.confirm(1, FakeGh(head=clean, enqueued_by="User"), rec)
        fails.append("confirm passed on a head the assessment never measured")
    except r.Wake as w:
        check("a post-rebase head wakes until --rebased re-binds it",
              head in w.reason, True)

    out = r.rebased(1, FakeGh(head=clean, enqueued_by="User"), rec, ga, rgit)
    check("--rebased names the new head", clean in out, True)
    check("--rebased carries the eviction's own measurement forward",
          f"merge-tree --write-tree {base} {head}" in out, True)
    body = rec.read_text()
    check("--rebased keeps the eviction's conflict set in the record",
          body.count("conflict docs/STATUS.md"), 2)

    out = r.confirm(1, FakeGh(head=clean, enqueued_by="User"), rec)
    check("confirm passes once the record names the rebased head",
          "ELIGIBLE" in out, True)

    # A force-push after --rebased is exactly what the head check is for.
    try:
        r.confirm(1, FakeGh(head="d" * 40, enqueued_by="User"), rec)
        fails.append("a force-push after --rebased was not caught")
    except r.Wake as w:
        check("a head moving after --rebased wakes", clean in w.reason, True)

with tempfile.TemporaryDirectory() as tmp:
    repo, base, head, _ = build_rebase_repo(tmp)
    ga = repo / ".gitattributes"

    def rgit(args, cwd=None):
        return git(args, repo)

    rec = Path(tmp) / "v" / "1.verdict"
    r.assess(1, FakeGh(head=head, enqueued_by="User"), rec, ga, rgit)
    # The unrebased head still conflicts with origin/main, so pointing --rebased
    # at it is the "the rebase is not actually done" case.
    try:
        r.rebased(1, FakeGh(head=head, enqueued_by="User"), rec, ga, rgit)
        fails.append("--rebased accepted a head that still conflicts")
    except r.Wake as w:
        check("--rebased refuses a head that did not heal the branch",
              "docs/STATUS.md" in w.reason, True)
    try:
        r.confirm(1, FakeGh(head=head, enqueued_by="User"), rec)
        fails.append("confirm passed after a refused --rebased")
    except r.Wake:
        ok("a refused --rebased leaves confirm waking")

with tempfile.TemporaryDirectory() as tmp:
    repo, base, head, clean = build_rebase_repo(tmp)
    ga = repo / ".gitattributes"

    def rgit(args, cwd=None):
        return git(args, repo)

    rec = Path(tmp) / "v" / "1.verdict"
    r.write_record(rec, "WAKE", "a code conflict", "main", base, head, [])
    try:
        r.rebased(1, FakeGh(head=clean, enqueued_by="User"), rec, ga, rgit)
        fails.append("--rebased re-bound a record that was never ELIGIBLE")
    except r.Wake as w:
        check("--rebased refuses to re-bind a non-ELIGIBLE record",
              "nothing to re-bind" in w.reason, True)

for f in fails:
    print(f"FAIL {f}")
sys.exit(1 if fails else 0)
PY
then
    printf '\npr-requeue-eligible-test: ok\n'
else
    printf '\npr-requeue-eligible-test: FAILED\n'
    exit 1
fi
