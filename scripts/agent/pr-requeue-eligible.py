#!/usr/bin/env python3
"""pr-requeue-eligible.py — decide whether a session may re-enqueue an open PR
that a merge queue evicted, without waking the maintainer.

The maintainer reviews and enqueues; no agent does either. This is the one
carve-out: restoring a state the maintainer already chose, after the queue
dropped the PR for a reason that has nothing to do with the change. It never
performs a *first* enqueue, so review still gates every merge.

Why a script rather than a rule in a prompt: the decision is four checks against
two APIs plus a driver-off merge probe, re-derived on every eviction. Prose gets
it wrong silently, and the failure mode is an unattended merge of something
nobody read.

Eligible means all of:

1. The PR is OPEN and not a draft.
2. A human enqueued it before — `added_to_merge_queue` by a non-bot actor. That
   is what makes a re-enqueue a restoration rather than a decision.
3. It is not in the queue right now, so this cannot double-enqueue.
4. The rebase it is about to take conflicts ONLY in files the repo's merge
   drivers own. A conflict in code, tests or workflows is a human's to read,
   because the rebase changes what the maintainer approved.

Both sides of that merge are named by the PR, not by the checkout: the head
comes from `headRefOid`. Read as a local `HEAD` it answers about whatever the
caller happened to have checked out, so two different PRs assessed from one
worktree return the same verdict.

Check 4 runs BEFORE the rebase, but the enqueue happens after it and after CI.
So `--assess` records its verdict and `--confirm` reads it back; a missing or
stale record fails closed to "wake the maintainer", the safe direction when a
session loses context mid-flight.

That assessment is also the only contemporaneous record of *why* the queue
evicted a PR. The rebase heals the branch, and the same probe against current
refs then reports a clean merge, so a later read can neither confirm nor refute
what was measured. The record therefore carries the two commits it merged:
`git merge-tree --write-tree <base_oid> <head_oid>` re-derives the conflict set
from those objects at any later time.

Every probe distinguishes a measured negative from a read that never happened.
A failed API call leaves an empty value, and every check reads emptiness as an
answer: no state as "not OPEN", no queue entry as "not queued", no timeline as
"nobody enqueued it". An unmeasurable probe therefore exits 2 rather than
deciding.

Exit: 0 eligible, 1 not eligible (reason on stdout), 2 usage error or a probe
      that could not run (reason on stderr).
"""

import argparse
import json
import re
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

OID = re.compile(r"^[0-9a-f]{40}$")
# Bumped when the record's shape changes. An older record is then refused as
# skew, naming both versions, rather than parsed into an empty verdict and
# reported as corruption.
RECORD_VERSION = 1


class Unmeasurable(Exception):
    """A probe that could not run. Never a verdict."""


class Wake(Exception):
    """Not eligible: hand back to a human."""

    def __init__(self, reason, conflicts=()):
        super().__init__(reason)
        self.reason = reason
        self.conflicts = list(conflicts)


# --- merge-driver ownership, derived rather than declared ------------------

def driver_config(gitattributes):
    """(owned_paths, driver_names) from every `merge=` attribute.

    Derived rather than hand-listed because a stale list under-reports
    conflicts, and under-reporting is the verdict that lets an unattended
    enqueue through. A path missing from the list is not discounted as
    driver-owned; a driver name missing from it stays live during the probe and
    silently resolves its own file.
    """
    paths, names = [], []
    try:
        text = Path(gitattributes).read_text(encoding="utf-8")
    except OSError:
        return paths, names
    for line in text.splitlines():
        line = line.split("#", 1)[0].strip()
        if not line:
            continue
        fields = line.split()
        merge = [f for f in fields[1:] if f.startswith("merge=")]
        if not merge:
            continue
        paths.append(fields[0])
        for m in merge:
            name = m.split("=", 1)[1]
            if name not in names:
                names.append(name)
    return paths, names


# --- GitHub reads ---------------------------------------------------------

class Gh:
    """The three GitHub reads, parsed in-process.

    Asks for `--json` and parses here rather than delegating to `gh --jq`: an
    expression inside a gh invocation is invisible to a test that stubs gh, so a
    malformed one fails in production instead of in the suite.
    """

    def __init__(self, pr, repo=None, run=None):
        self.pr = pr
        self.repo = repo
        self._run = run or self._subprocess

    @staticmethod
    def _subprocess(cmd):
        p = subprocess.run(cmd, capture_output=True, text=True)
        return p.returncode, p.stdout, p.stderr

    def _json(self, cmd, what):
        rc, out, err = self._run(cmd)
        if rc != 0:
            raise Unmeasurable(f"{what}: {(err or out).strip()}")
        try:
            return json.loads(out)
        except json.JSONDecodeError as e:
            raise Unmeasurable(f"{what}: unparseable JSON ({e})") from e

    def pr_fields(self):
        cmd = ["gh", "pr", "view", str(self.pr), "--json",
               "state,isDraft,baseRefName,headRefOid"]
        if self.repo:
            cmd += ["--repo", self.repo]
        d = self._json(cmd, f"PR {self.pr}'s state")
        state, base = d.get("state"), d.get("baseRefName")
        head, draft = d.get("headRefOid"), d.get("isDraft")
        if not state or not base or not head or draft is None:
            raise Unmeasurable(f"PR {self.pr}'s state: the read answered {d!r}")
        # Shape-checked rather than left to rev-parse: a mangled OID would
        # otherwise surface as "the conflict set is unmeasurable", naming the
        # probe instead of the read that broke it.
        if not OID.match(head):
            raise Unmeasurable(
                f"PR {self.pr}'s head commit: the read answered {head!r}")
        return state, bool(draft), base, head

    def in_queue(self):
        """True when GitHub reports a live merge-queue entry.

        `gh pr view` exposes no queue field, so this is the GraphQL one. The
        node id is selected alongside the state because a PR outside the queue
        and a read that never ran would otherwise answer identically — a
        successful read always carries an id.
        """
        owner_repo = self.repo
        if not owner_repo:
            d = self._json(["gh", "repo", "view", "--json", "nameWithOwner"],
                           f"which repo PR {self.pr} belongs to")
            owner_repo = d.get("nameWithOwner") or ""
        if "/" not in owner_repo:
            raise Unmeasurable(
                f"which repo PR {self.pr} belongs to: got {owner_repo!r}")
        owner, name = owner_repo.split("/", 1)
        query = ("query($owner:String!,$name:String!,$pr:Int!){"
                 "repository(owner:$owner,name:$name){"
                 "pullRequest(number:$pr){ id mergeQueueEntry { state } }}}")
        d = self._json(["gh", "api", "graphql", "-f", f"owner={owner}",
                        "-f", f"name={name}", "-F", f"pr={self.pr}",
                        "-f", f"query={query}"],
                       f"whether PR {self.pr} is in the merge queue")
        node = (((d.get("data") or {}).get("repository") or {})
                .get("pullRequest") or {})
        if not node.get("id"):
            raise Unmeasurable(
                f"whether PR {self.pr} is in the merge queue: no PR id in the answer")
        entry = node.get("mergeQueueEntry")
        return bool(entry and entry.get("state"))

    def human_enqueued(self):
        """True when a non-bot actor has added this PR to the queue.

        Every page is read and counted here. gh's `--paginate` applies a `--jq`
        filter per page and prints one count per page, which fed to arithmetic
        as a multi-line value is a syntax error that reads as "nobody enqueued
        it" — a false negative on a healthy network.
        """
        path = (f"repos/{self.repo}/issues/{self.pr}/timeline" if self.repo
                else f"repos/{{owner}}/{{repo}}/issues/{self.pr}/timeline")
        events = self._json(["gh", "api", path, "--paginate", "--slurp"],
                            f"whether a human enqueued PR {self.pr}")
        # --slurp gives one array of pages; tolerate a flat array too.
        pages = events if events and isinstance(events[0], list) else [events]
        for page in pages:
            for ev in page:
                if ev.get("event") != "added_to_merge_queue":
                    continue
                if (ev.get("actor") or {}).get("type", "User") != "Bot":
                    return True
        return False


# --- the merge probe ------------------------------------------------------

def run_git(args, cwd=None):
    p = subprocess.run(["git"] + args, capture_output=True, text=True, cwd=cwd)
    return p.returncode, p.stdout, p.stderr


def resolve_commits(base_ref, head_oid, pr, git=run_git):
    """Pin both sides to an OID, so the record is re-runnable.

    A ref pair is not a measurement anyone can repeat: the base and the PR's
    head have both moved by the time the question is asked. The head need not be
    local — a session assessing another's PR has never had the branch — so an
    absent one is fetched from the pull ref before it is called missing.
    """
    rc, out, _ = git(["rev-parse", "--verify", "--quiet", f"{base_ref}^{{commit}}"])
    if rc != 0 or not out.strip():
        raise Unmeasurable(f"{base_ref} does not resolve to a commit")
    base_oid = out.strip()
    rc, out, _ = git(["rev-parse", "--verify", "--quiet", f"{head_oid}^{{commit}}"])
    if rc != 0 or not out.strip():
        git(["fetch", "origin", "--quiet", f"refs/pull/{pr}/head"])
        rc, out, _ = git(["rev-parse", "--verify", "--quiet", f"{head_oid}^{{commit}}"])
    if rc != 0 or not out.strip():
        raise Unmeasurable(
            f"PR {pr} head {head_oid} is not in this clone and "
            f"refs/pull/{pr}/head did not fetch it")
    return base_oid, out.strip()


def conflicting_paths(base_oid, head_oid, driver_names, git=run_git):
    """Paths this merge cannot resolve without a driver.

    Each declared driver is replaced by a failing command rather than unset:
    `-c merge.<name>.driver=false` runs a command that exits non-zero, so git
    records a conflict for any driver-owned path both sides touched instead of
    attempting the built-in merge. That errs toward reporting — a driver left
    live resolves its own file inside the probe and drops a real conflict from
    the record, which is the direction that cannot be detected afterwards.

    A probe that never ran yields no CONFLICT lines, which would read as a clean
    merge and hand back ELIGIBLE. The output is therefore required to open with
    the merged tree's OID, which merge-tree prints only when it actually merged.
    """
    off = []
    for name in driver_names:
        off += ["-c", f"merge.{name}.driver=false"]
    rc, out, err = git(off + ["merge-tree", "--write-tree", base_oid, head_oid])
    combined = out + err
    first = combined.splitlines()[0] if combined.splitlines() else ""
    if rc > 1 or not OID.match(first.strip()):
        raise Unmeasurable(
            f"merge-tree of {base_oid} into {head_oid} did not run "
            f"(rc={rc}): {combined.strip()}")
    paths = set()
    for line in combined.splitlines():
        if not line.startswith("CONFLICT"):
            continue
        m = re.search(r" in (.+)$", line)
        if m:
            paths.add(m.group(1).strip())
    return sorted(paths)


# --- the record -----------------------------------------------------------

def write_record(path, verdict, reason, base, base_oid, head_oid, conflicts):
    """Append one keyed record.

    Appended rather than overwritten: a later assessment that refuses before it
    probes would otherwise erase the measurement the first one took, which is
    the eviction's only evidence. `--confirm` reads the last record, so the
    fail-closed reading of current state is unchanged by keeping earlier ones.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    stamp = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    lines = [f"version {RECORD_VERSION}", f"verdict {verdict}", f"at {stamp}",
             f"base {base or '-'}", f"base_oid {base_oid or '-'}",
             f"head_oid {head_oid or '-'}", f"reason {reason}"]
    lines += [f"conflict {c}" for c in conflicts]
    with path.open("a", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")


def read_last_record(path):
    """The most recent record, or raise Wake naming what is wrong with it.

    A record whose shape this version does not recognise is reported as skew,
    naming both versions. Parsing it into an empty verdict and reporting
    "the recorded assessment was '', not ELIGIBLE" describes corruption, which
    sends a reader looking for a damaged file rather than a version bump.
    """
    if not path.exists():
        raise Wake("no recorded assessment; a re-enqueue only follows an "
                   "--assess that ran before the rebase")
    records, cur = [], None
    for line in path.read_text(encoding="utf-8").splitlines():
        key, _, value = line.partition(" ")
        if key == "version":
            cur = {"version": value.strip(), "conflict": []}
            records.append(cur)
        elif cur is None:
            continue
        elif key == "conflict":
            cur["conflict"].append(value.strip())
        else:
            cur[key] = value.strip()
    if not records:
        raise Wake(f"the assessment record at {path} carries no recognisable "
                   f"record; it predates version {RECORD_VERSION} of this "
                   f"format, so re-run --assess rather than trusting it")
    last = records[-1]
    if last.get("version") != str(RECORD_VERSION):
        raise Wake(f"the assessment record is format version "
                   f"{last.get('version')!r}, and this tool writes "
                   f"{RECORD_VERSION}; re-run --assess")
    return last


# --- the two modes --------------------------------------------------------

def assess(pr, gh, record_path, gitattributes, git=run_git):
    state, draft, base, head = gh.pr_fields()
    if state != "OPEN":
        raise Wake(f"the PR is {state}, not OPEN")
    if draft:
        raise Wake("the PR is a draft")

    owned, names = driver_config(gitattributes)

    # The probe runs BEFORE the eligibility checks, which is the fix for the
    # capture that used to be lost on the wake that most needed it: a session
    # healing its own not-yet-enqueued PR fails the human-enqueued check, and
    # the record then carried no OIDs and no conflict set — the measurement the
    # capture exists to preserve, missing from exactly the ordinary dirty wake.
    #
    # The original ordering was cheapest-first, and that intent survives: the
    # merge probe is local, while the timeline read is paginated network. Only
    # the local half moved ahead of the checks.
    git(["fetch", "origin", base, "--quiet"])
    base_oid, head_oid = resolve_commits(f"origin/{base}", head, pr, git)
    conflicts = conflicting_paths(base_oid, head_oid, names, git)
    not_owned = [p for p in conflicts if p not in owned]

    def record(verdict, reason, paths):
        write_record(record_path, verdict, reason, base, base_oid, head_oid, paths)

    # Printed before the verdict, so a wake carries the re-runnable measurement
    # whichever way the verdict goes.
    measured = (f"measured: git merge-tree --write-tree {base_oid} {head_oid}\n"
                f"conflicts: {' '.join(conflicts) if conflicts else 'none'}")

    try:
        if gh.in_queue():
            raise Wake("the PR is already in the merge queue; nothing to restore")
        if not gh.human_enqueued():
            raise Wake("no human has enqueued this PR, so a re-enqueue would be "
                       "a first enqueue")
        if not_owned:
            raise Wake("the rebase resolves conflicts outside the "
                       f"merge-driver-owned files: {' '.join(not_owned)}",
                       conflicts)
    except Wake as w:
        record("WAKE", w.reason, w.conflicts or conflicts)
        w.measured = measured
        raise

    reason = ("conflicts confined to merge-driver-owned files" if conflicts
              else "the rebase resolves no conflicts at all")
    record("ELIGIBLE", reason, conflicts)
    detail = f" ({' '.join(conflicts)})" if conflicts else ""
    return f"{measured}\nELIGIBLE: {reason}{detail}"


def confirm(pr, gh, record_path):
    state, draft, base, head = gh.pr_fields()
    if state != "OPEN":
        raise Wake(f"the PR is {state}, not OPEN")
    if draft:
        raise Wake("the PR is a draft")
    last = read_last_record(record_path)
    if last.get("verdict") != "ELIGIBLE":
        raise Wake(f"the recorded assessment was {last.get('verdict')!r}, "
                   f"not ELIGIBLE")
    if last.get("base") != base:
        raise Wake(f"the PR's base changed from {last.get('base')} to {base} "
                   f"since the assessment")
    if gh.in_queue():
        raise Wake("the PR is already in the merge queue; nothing to restore")
    if not gh.human_enqueued():
        raise Wake("no human has enqueued this PR, so there is nothing to restore")
    conflicts = last.get("conflict") or []
    return ("ELIGIBLE: re-enqueue restores the maintainer's own earlier enqueue\n"
            f"measured: git merge-tree --write-tree {last.get('base_oid')} "
            f"{last.get('head_oid')}\n"
            f"conflicts: {' '.join(conflicts) if conflicts else 'none'}")


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    mode = p.add_mutually_exclusive_group(required=True)
    mode.add_argument("--assess", action="store_true",
                      help="before rebasing; records a verdict")
    mode.add_argument("--confirm", action="store_true",
                      help="after CI is green; gates the enqueue")
    p.add_argument("pr", type=int)
    p.add_argument("--repo", help="OWNER/NAME")
    p.add_argument("--state-dir", default="tmp/requeue")
    p.add_argument("--gitattributes", default=".gitattributes")
    args = p.parse_args(argv)
    if args.pr <= 0:
        p.error("a positive PR number is required")

    gh = Gh(args.pr, args.repo)
    record_path = Path(args.state_dir) / f"{args.pr}.verdict"
    try:
        if args.assess:
            print(assess(args.pr, gh, record_path, args.gitattributes))
        else:
            print(confirm(args.pr, gh, record_path))
        return 0
    except Wake as w:
        if getattr(w, "measured", None):
            print(w.measured)
        print(f"WAKE: {w.reason}")
        return 1
    except Unmeasurable as e:
        print(f"pr-requeue-eligible: could not measure {e}; refusing to guess",
              file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
