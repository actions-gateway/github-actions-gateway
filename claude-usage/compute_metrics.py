#!/usr/bin/env python3
"""Compute and persist Claude Code usage metrics for this project over time.

Reads the local Claude Code session transcripts (``~/.claude/projects/*<repo>*``)
and this repo's git history, then writes daily time series to ``claude-usage/data/``.

Why this exists: session transcripts can be **archived or deleted**, which would
permanently lose the token/message history. So the token + message series are
SNAPSHOTTED into committed CSVs using a merge rule that only ever revises a past
day's values *upward* — re-running after some sessions are archived can never
erase data already recorded. Git-derived series (commits / tests / lines of Go)
are recomputed from scratch each run, because git history is durable and those
counts represent the state at a date (which can legitimately go down).

Transcripts are per-machine, so every token/message row carries the ``host`` that
measured it. A machine only ever sees its own sessions: the upward-only merge
applies *within* a machine, and a day's true total is the SUM across machines.
Each machine must declare a stable id (see ``resolve_host``) — without one the
run aborts rather than guess, because an id that drifts between runs re-counts
every day whose transcripts that machine can still read.

Run from anywhere:

    python3 claude-usage/compute_metrics.py

Environment:
    CLAUDE_PROJECTS_GLOB    Override the transcript glob. Default:
                            ~/.claude/projects/*github-actions-gateway*
    CLAUDE_METRICS_HOST     This machine's id. Default: first line of HOST_FILE.
    CLAUDE_METRICS_HOST_FILE  Override where that id is stored. Default:
                            ~/.config/claude-usage/host

Outputs (all under claude-usage/data/):
    token_metrics.csv   daily input/output/cache tokens + message counts (merge-preserved)
    model_daily.csv     daily per-model headline tokens (merge-preserved)
    session_metrics.csv daily session concurrency (merge-preserved, never estimated)
    git_metrics.csv     daily commits, test count, Go/Markdown/YAML LOC (recomputed)
    summary.json        headline totals, per-model split, HEAD snapshot, provenance
"""

import csv
import glob
import json
import os
import re
import subprocess
from collections import defaultdict
from datetime import datetime, timedelta, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
DATA = os.path.join(HERE, "data")
REPO = subprocess.run(
    ["git", "-C", HERE, "rev-parse", "--show-toplevel"],
    capture_output=True, text=True,
).stdout.strip()

DEFAULT_GLOB = os.path.join(os.path.expanduser("~/.claude/projects"), "*github-actions-gateway*")
PROJECTS_GLOB = os.environ.get("CLAUDE_PROJECTS_GLOB", DEFAULT_GLOB)

# Where this machine's id lives when it isn't passed in the environment. Local
# only — never committed, and deliberately not derived from the hostname, which
# is neither stable across a machine's life nor distinct between two similar
# laptops.
HOST_FILE = os.path.expanduser(
    os.environ.get("CLAUDE_METRICS_HOST_FILE", "~/.config/claude-usage/host"))

# Machine id for rows written before the host column existed — all of which came
# from one machine. That machine's config must name it, so its next run merges
# into those rows instead of adding a second, duplicate set.
LEGACY_HOST = "mac-1"

# Host column for backfilled rows: estimated from git history, measured nowhere.
EST_HOST = "-"

# Date the plan upgraded Pro -> Max. Used both as a chart annotation and to bound
# the "Pro-era" window from which the archived-day backfill rate is derived.
PRO_TO_MAX = "2026-05-23"

# Date the plan upgraded Max 5x -> Max 20x. Annotation-only (no computational
# role) — recorded in provenance and drawn on the by-model chart.
MAX_5X_TO_20X = "2026-07-05"

# Date every tracked doc reflowed to sentence-per-line. Annotation-only, like
# MAX_5X_TO_20X, but about the git series rather than the token one: unwrapping
# hard-wrapped paragraphs cut ~18.6k non-blank Markdown lines without deleting a
# word, so `md` steps down here for a reason no reader could infer from the CSV.
DOCS_REFLOW = "2026-08-09"

# Bucket width for session concurrency. Wide enough that a session waiting on a
# build or a test run still counts as in flight, narrow enough that two sessions
# worked on hours apart never collide.
SESSION_BUCKET_MIN = 10

# Token usage is deduped on (message.id, requestId): resumed/compacted sessions
# replay earlier assistant records verbatim, and counting them twice would inflate
# every total (cache_read especially). Message records are deduped on their uuid.
GO_PATHS = ["*.go", ":!vendor/**", ":!**/vendor/**"]
TEST_PATHS = ["*_test.go", ":!vendor/**", ":!**/vendor/**"]
MD_PATHS = ["*.md", ":!vendor/**", ":!**/vendor/**"]
YAML_PATHS = ["*.yaml", "*.yml", ":!vendor/**", ":!**/vendor/**"]
# Other hand-authored source: shell, Python, website (CSS/JS/HTML), and build files
# (Makefile/Dockerfile). Excludes vendor, binaries, generated, and lockfiles.
SCRIPT_PATHS = [
    "*.sh", "*.py", "*.css", "*.js", "*.mjs", "*.html", "*.tpl",
    "Makefile", "**/Makefile", "*.mk", "Dockerfile*", "**/Dockerfile*",
    ":!vendor/**", ":!**/vendor/**",
]


def resolve_host():
    """This machine's id, from ``$CLAUDE_METRICS_HOST`` or ``HOST_FILE``.

    Aborts when neither is set instead of falling back to a guess. Rows are keyed
    by ``(date, host)`` and summed across hosts, so a machine that silently
    adopted a *fresh* id would re-measure every day it still holds transcripts
    for and have those totals added to the originals — a doubling that looks
    like real growth. Refusing to run is the cheap failure.
    """
    host = (os.environ.get("CLAUDE_METRICS_HOST") or "").strip()
    source = "$CLAUDE_METRICS_HOST"
    if not host:
        source = HOST_FILE
        try:
            with open(HOST_FILE) as fh:
                host = fh.readline().strip()
        except OSError:
            host = ""
    if not host:
        raise SystemExit(
            "No machine id configured — refusing to guess (see resolve_host).\n"
            "\nName this machine once. Any short stable label works; ids land in the\n"
            "committed CSVs, so prefer 'mac-2' over a real hostname:\n"
            f"\n    mkdir -p {os.path.dirname(HOST_FILE)} && echo mac-2 > {HOST_FILE}\n"
            "\nUse the SAME id on every run from this machine. The machine that produced\n"
            f"the pre-existing rows must be named {LEGACY_HOST!r}, or its next run will\n"
            "add a duplicate copy of that history."
        )
    if host == EST_HOST or any(c.isspace() for c in host) or set(host) & set(",\"'"):
        raise SystemExit(
            f"Invalid machine id {host!r} from {source}: no whitespace, commas or "
            f"quotes, and {EST_HOST!r} is reserved for estimated rows."
        )
    return host


def model_family(m):
    """Map a raw model id to a stable display family."""
    if not m:
        return "Unknown"
    if "sonnet" in m:
        return "Sonnet 4.6"
    if "opus-5" in m:
        return "Opus 5"
    if "opus-4-8" in m:
        return "Opus 4.8"
    if "opus-4-7" in m:
        return "Opus 4.7"
    if "haiku" in m:
        return "Haiku 4.5"
    if "fable" in m:
        return "Fable 5"
    return "Other"


def aggregate_logs(host):
    """Aggregate per-day token + message metrics from this machine's transcripts.

    Rows are keyed by ``(date, host)`` (and ``(date, model, host)``): the glob
    only ever reaches the local machine's sessions, so what this returns is one
    machine's share of each day, not the day's total.
    """
    files = []
    for d in glob.glob(PROJECTS_GLOB):
        files += glob.glob(os.path.join(d, "*.jsonl"))

    tok = defaultdict(lambda: defaultdict(int))     # date -> field -> tokens
    model = defaultdict(lambda: defaultdict(int))   # (date, family) -> field -> value
    asst_msgs = defaultdict(int)
    user_msgs = defaultdict(int)
    seen_usage = set()  # (message.id, requestId)
    seen_uuid = set()

    for f in files:
        try:
            fh = open(f, errors="replace")
        except OSError:
            continue
        with fh:
            for line in fh:
                line = line.strip()
                if not line:
                    continue
                try:
                    rec = json.loads(line)
                except ValueError:
                    continue
                ts = rec.get("timestamp")
                if not ts:
                    continue
                try:
                    dk = datetime.fromisoformat(ts.replace("Z", "+00:00")).date().isoformat()
                except ValueError:
                    continue

                t = rec.get("type")
                if t == "user":
                    uuid = rec.get("uuid")
                    if uuid:
                        if uuid in seen_uuid:
                            continue
                        seen_uuid.add(uuid)
                    user_msgs[dk] += 1
                    continue
                if t != "assistant":
                    continue

                msg = rec.get("message")
                if not isinstance(msg, dict):
                    continue
                u = msg.get("usage")
                if not isinstance(u, dict):
                    continue
                key = (msg.get("id"), rec.get("requestId"))
                if key != (None, None):
                    if key in seen_usage:
                        continue
                    seen_usage.add(key)

                inp = u.get("input_tokens", 0) or 0
                out = u.get("output_tokens", 0) or 0
                cc = u.get("cache_creation_input_tokens", 0) or 0
                cr = u.get("cache_read_input_tokens", 0) or 0
                asst_msgs[dk] += 1
                tok[dk]["input"] += inp
                tok[dk]["output"] += out
                tok[dk]["cache_creation"] += cc
                tok[dk]["cache_read"] += cr

                fam = model_family(msg.get("model"))
                model[(dk, fam)]["headline"] += inp + out + cc
                model[(dk, fam)]["output"] += out
                model[(dk, fam)]["messages"] += 1

    token_rows = {}
    for dk in set(tok) | set(user_msgs):
        fields = tok.get(dk, {})
        token_rows[(dk, host)] = {
            "date": dk,
            "host": host,
            "input": fields.get("input", 0),
            "output": fields.get("output", 0),
            "cache_creation": fields.get("cache_creation", 0),
            "cache_read": fields.get("cache_read", 0),
            "assistant_msgs": asst_msgs.get(dk, 0),
            "user_msgs": user_msgs.get(dk, 0),
        }
    model_rows = {
        (dk, fam, host): {
            "date": dk, "model": fam, "host": host,
            "headline": v["headline"], "output": v["output"], "messages": v["messages"],
        }
        for (dk, fam), v in model.items()
    }
    return token_rows, model_rows, len(files)


def git(*args):
    return subprocess.run(["git", "-C", REPO, *args], capture_output=True, text=True).stdout


def grep_count(rev, pattern, paths):
    """Sum ``git grep -c`` line counts across files at a revision."""
    out = git("grep", "-c", "-E", pattern, rev, "--", *paths)
    total = 0
    for ln in out.splitlines():
        if not ln:
            continue
        try:
            total += int(ln.rsplit(":", 1)[1])
        except (ValueError, IndexError):
            pass
    return total


def grep_word_count(rev, pattern, paths):
    """Whitespace-separated words across matching lines at a revision.

    The reformat-proof half of the lines series: rewrapping a paragraph moves
    every line count that spans it and leaves the word count identical, so a
    ratio built on words survives a reflow that a per-line ratio cannot.
    ``-h`` drops the ``rev:path:`` prefix so it isn't counted as content.
    """
    out = git("grep", "-h", "-E", pattern, rev, "--", *paths)
    return sum(len(ln.split()) for ln in out.splitlines())


def grep_words_per_file(rev, pattern, paths):
    """Word counts as a ``{path: words}`` map, so a path filter can be applied.

    The YAML band needs this: ``grep_word_count`` over every ``*.yaml`` sums the
    generated CRDs too, which the line series drops. Counting them would dilute
    the cost ratio with output nobody authored.
    """
    out = git("grep", "-E", pattern, rev, "--", *paths)
    counts = {}
    for ln in out.splitlines():
        parts = ln.split(":", 2)  # rev:path:content
        if len(parts) < 3:
            continue
        counts[parts[1]] = counts.get(parts[1], 0) + len(parts[2].split())
    return counts


def grep_lines_per_file(rev, pattern, paths):
    """``git grep -c`` line counts as a ``{path: count}`` map at a revision."""
    out = git("grep", "-c", "-E", pattern, rev, "--", *paths)
    counts = {}
    for ln in out.splitlines():
        parts = ln.split(":")  # rev:path:count (paths here never contain ':')
        if len(parts) < 3:
            continue
        try:
            counts[":".join(parts[1:-1])] = int(parts[-1])
        except ValueError:
            pass
    return counts


def grep_files_matching(rev, pattern, paths):
    """Set of file paths whose contents match ``pattern`` (case-insensitive) at a revision."""
    out = git("grep", "-l", "-i", "-E", pattern, rev, "--", *paths)
    return {ln.split(":", 1)[1] for ln in out.splitlines() if ":" in ln}


PR_SUBJECT = re.compile(r"\(#\d+\)$")  # the squash-merge signature this repo lands with
CONVENTIONAL = re.compile(r"^(\w+)[(:]")   # the type in a Conventional Commits subject


def queue_flow():
    """``(closed, filed)`` maps of ``{date: count}`` for rows leaving and entering.

    Closures alone cannot tell progress from treading water: this project closed
    15 rows a day in its busiest stretch while filing 20, so the backlog grew.
    Both directions are counted from one walk.

    A row's anchor disappearing is work shipped in the common case, and a decline
    or a prune in the rest — a work proxy, not a completion ledger. Only a Q-id's
    *first* removal counts, so a row re-filed under a shipped id (the defect Q775
    describes) can't book the same work twice.

    One ``git log -p`` walk, not a ``git show`` per revision: that file has over a
    thousand revisions and the per-revision form takes ~40 s.
    """
    out = git("log", "--reverse", "--format=%x00%ad", "--date=short",
              "-p", "--", "docs/STATUS.md")
    anchor = re.compile(r'id="(Q\d+)"')
    closed, filed = defaultdict(int), defaultdict(int)
    seen_closed, seen_filed, date = set(), set(), None
    removed, added = set(), set()

    def flush():
        # An anchor on both sides moved within the file (Queue -> Deferred); only
        # one gone from the revision entirely has closed, and only one that was not
        # there before has been filed.
        for q in removed - added:
            if q not in seen_closed:
                seen_closed.add(q)
                closed[date] += 1
        for q in added - removed:
            if q not in seen_filed:
                seen_filed.add(q)
                filed[date] += 1
        removed.clear()
        added.clear()

    for ln in out.splitlines():
        if ln.startswith("\x00"):
            if date:
                flush()
            date = ln[1:]
        elif ln.startswith("-") and not ln.startswith("---"):
            removed.update(anchor.findall(ln))
        elif ln.startswith("+") and not ln.startswith("+++"):
            added.update(anchor.findall(ln))
    if date:
        flush()
    return closed, filed


# Bounds on the pull-request fetch. Measured 2026-08-15: one 100-PR page costs 3
# GraphQL points against a 5,000/hour budget, so a full 1,521-PR backfill is ~48
# and an incremental run is 3. The floor still exists because this budget is
# shared with every other tool on the account, and the page cap keeps a bug from
# walking the whole repo history on every run.
PR_RATE_FLOOR = 500
PR_PAGE = 200
PR_BACKFILL = 3000


def pr_series(existing_max):
    """Merged pull requests newer than ``existing_max``, as ``{number: row}``.

    Returns ``{}`` and leaves the caller's data alone whenever the fetch cannot be
    trusted: no ``gh``, no network, an API error, or a rate budget too low to
    spend. Every other series is computed from git and transcripts, so a failure
    here costs this one column and never the run.

    A merged PR's timestamps never change, so rows are immutable once written and
    the merge is an insert rather than the upward-only maximum the token series
    needs.
    """
    try:
        out = subprocess.run(
            ["gh", "api", "rate_limit", "--jq", ".resources.graphql.remaining"],
            capture_output=True, text=True, timeout=30)
        remaining = int((out.stdout or "0").strip())
    except (OSError, ValueError, subprocess.SubprocessError):
        return {}
    if remaining < PR_RATE_FLOOR:
        print(f"pr_series: skipped, graphql budget {remaining} below floor {PR_RATE_FLOOR}")
        return {}

    limit = PR_BACKFILL if existing_max == 0 else PR_PAGE
    try:
        out = subprocess.run(
            ["gh", "pr", "list", "--state", "merged", "--limit", str(limit),
             "--json", "number,createdAt,mergedAt"],
            capture_output=True, text=True, timeout=300, cwd=REPO)
        rows = json.loads(out.stdout or "[]")
    except (OSError, ValueError, subprocess.SubprocessError):
        return {}

    fresh = {}
    for r in rows:
        n = int(r["number"])
        if n <= existing_max or not r.get("mergedAt"):
            continue
        try:
            a = datetime.fromisoformat(r["createdAt"].replace("Z", "+00:00"))
            b = datetime.fromisoformat(r["mergedAt"].replace("Z", "+00:00"))
        except (ValueError, AttributeError):
            continue
        fresh[n] = {
            "number": n,
            "merged_date": r["mergedAt"][:10],
            "created_at": r["createdAt"],
            "merged_at": r["mergedAt"],
            "cycle_hours": f"{(b - a).total_seconds() / 3600:.4f}",
        }
    # Every row on the page was new, so the gap is wider than one page. Say so
    # rather than silently recording a hole the next run cannot detect.
    if fresh and len(fresh) == len(rows) and existing_max:
        print(f"pr_series: {len(fresh)} new PRs filled a full page; re-run to catch any older gap")
    return fresh


# How long after a week closes its survival is judged. Fixed horizon rather than
# survival-to-HEAD so every week is measured over the same window and is therefore
# comparable, and so a week's value is final once the horizon passes and can be
# stored instead of recomputed.
SURVIVAL_HORIZON_DAYS = 14


def survival_series(existing_weeks):
    """``{week_start: row}`` of code that outlived its first fortnight.

    For each 7-day bin, what fraction of the Go lines written in it were still
    present ``SURVIVAL_HORIZON_DAYS`` later. Volume says how much was produced;
    this says how much of it was still standing two weeks on.

    Restricted to non-test Go, the output whose churn the ratio is about: docs and
    YAML move for reasons (a reflow, a regenerated CRD) that have nothing to do
    with whether the work held up.

    Weeks already in ``existing_weeks`` are skipped, and a week whose horizon is
    still in the future is left alone until it is final.
    """
    log = git("log", "--reverse", "--format=%H|%ad", "--date=short").splitlines()
    days = []
    for ln in log:
        if "|" in ln:
            days.append(ln.split("|", 1))
    if not days:
        return {}
    start = datetime.strptime(days[0][1], "%Y-%m-%d").date()
    today = datetime.strptime(days[-1][1], "%Y-%m-%d").date()

    added_by_week = {}
    stat = git("log", "--reverse", "--format=%x00%ad", "--date=short", "--numstat",
               "--", "*.go", ":!vendor/**", ":!**/vendor/**", ":!*_test.go")
    cur = None
    for ln in stat.splitlines():
        if ln.startswith("\x00"):
            d = datetime.strptime(ln[1:], "%Y-%m-%d").date()
            cur = (start + timedelta(days=7 * ((d - start).days // 7))).isoformat()
        elif cur and ln and ln[0].isdigit():
            parts = ln.split("\t")
            if len(parts) == 3 and parts[0].isdigit():
                added_by_week[cur] = added_by_week.get(cur, 0) + int(parts[0])

    rows = {}
    week = 0
    while True:
        w0 = start + timedelta(days=7 * week)
        w1 = w0 + timedelta(days=6)
        horizon = w1 + timedelta(days=SURVIVAL_HORIZON_DAYS)
        if w0 > today:
            break
        week += 1
        key = w0.isoformat()
        if key in existing_weeks or horizon > today:
            continue
        # Lines of non-test Go added during the week, bucketed by AUTHOR date.
        # `--since/--until` filter on commit date, and this repo rebases constantly,
        # so a commit authored in one week is frequently committed in the next. Blame
        # reports author-time, so selecting on commit date would compare two clocks
        # and report survivors the week never wrote.
        added = added_by_week.get(key, 0)
        if not added:
            continue
        # The tree as it stood at the horizon, blamed and bucketed by the date the
        # line was introduced.
        rev = git("rev-list", "-1", f"--before={horizon} 23:59:59", "HEAD").strip()
        if not rev:
            continue
        files = git("ls-tree", "-r", "--name-only", rev, "--").splitlines()
        survived = 0
        for f in files:
            if not f.endswith(".go") or f.endswith("_test.go") or "vendor/" in f:
                continue
            for bl in git("blame", "--line-porcelain", rev, "--", f).splitlines():
                if bl.startswith("author-time "):
                    d = datetime.utcfromtimestamp(int(bl.split()[1])).date()
                    if w0 <= d <= w1:
                        survived += 1
        rows[key] = {
            "week_start": key,
            "added": added,
            "survived": survived,
            "horizon_days": SURVIVAL_HORIZON_DAYS,
            # Not clamped: a rate above 1 would mean the two sides disagree, and
            # capping it would hide exactly the defect worth seeing.
            "rate": f"{survived / added:.4f}",
        }
    return rows


def local_offset():
    """The repo's usual UTC offset, in hours, from git author timestamps.

    Derived rather than configured: a day-of-week or hour-of-day view keyed on UTC
    puts an evening in one zone onto the next day, and this project's work runs
    late. Taking the commonest offset in the history follows the machine if it
    moves, and needs no setting anyone can forget.
    """
    seen = defaultdict(int)
    for ln in git("log", "--format=%ad", "--date=format:%z").splitlines():
        ln = ln.strip()
        if len(ln) == 5 and ln[0] in "+-":
            seen[ln] += 1
    if not seen:
        return 0
    best = max(seen, key=seen.get)
    return (1 if best[0] == "+" else -1) * (int(best[1:3]) + int(best[3:5]) / 60)


def rhythm_series(offset_hours):
    """Per weekday and per local hour, what the project does in it.

    Two questions the daily series cannot answer: which days the work lands on,
    and which hours. Both are aggregates over the whole span rather than a time
    series, so they get their own table keyed ``(dim, bucket)``.

    Everything is bucketed in local time. Every transcript timestamp is UTC and
    this project works evenings, so a UTC key would move most of a night's work
    onto the following day and flatten exactly the pattern being looked for.
    """
    tz = timezone(timedelta(hours=offset_hours))
    rows = {}

    def cell(dim, bucket):
        return rows.setdefault((dim, bucket), {
            "dim": dim, "bucket": bucket, "days": 0, "commits": 0, "prs": 0,
            "feat": 0, "fix": 0, "tokens": 0, "attended_buckets": 0,
            "active_buckets": 0,
        })

    # git: commits and their conventional type, by author time
    dates_seen = defaultdict(set)
    for ln in git("log", "--format=%ad|%s", "--date=format:%Y-%m-%d %u %H").splitlines():
        stamp, _, subj = ln.partition("|")
        parts = stamp.split()
        if len(parts) != 3:
            continue
        d, u, h = parts
        for dim, b in (("weekday", int(u) - 1), ("hour", int(h))):
            c = cell(dim, b)
            c["commits"] += 1
            dates_seen[(dim, b)].add(d)
            m = CONVENTIONAL.match(subj.strip())
            if m and m.group(1) in ("feat", "fix"):
                c[m.group(1)] += 1
    for k, ds in dates_seen.items():
        rows[k]["days"] = len(ds)

    # pull requests: merged when, and how long one opened in this hour waited
    try:
        with open(os.path.join(DATA, "pr_metrics.csv")) as fh:
            for r in csv.DictReader(fh):
                try:
                    op = datetime.fromisoformat(r["created_at"].replace("Z", "+00:00")).astimezone(tz)
                    mg = datetime.fromisoformat(r["merged_at"].replace("Z", "+00:00")).astimezone(tz)
                    hrs = float(r["cycle_hours"])
                except (ValueError, KeyError):
                    continue
                for dim, b in (("weekday", mg.weekday()), ("hour", mg.hour)):
                    cell(dim, b)["prs"] += 1
                # Wait times are NOT aggregated here. They are heavily skewed, so a
                # stored sum and count can only yield a mean, and the mean says the
                # opposite of the truth: PRs opened at 05:00 average 19h and have a
                # median of 0.32h, the fastest of any hour, because a handful waited
                # days. The charts read pr_metrics.csv and take percentiles.
                del op, hrs
    except OSError:
        pass

    # transcripts: spend, and whether a person was there
    files = []
    for d in glob.glob(PROJECTS_GLOB):
        files += glob.glob(os.path.join(d, "*.jsonl"))
    seen_msg = set()
    active = defaultdict(set)
    attended = defaultdict(set)
    for f in files:
        try:
            fh = open(f, errors="replace")
        except OSError:
            continue
        with fh:
            for line in fh:
                try:
                    rec = json.loads(line)
                except ValueError:
                    continue
                ts = rec.get("timestamp")
                if not ts:
                    continue
                try:
                    dt = datetime.fromisoformat(ts.replace("Z", "+00:00")).astimezone(tz)
                except ValueError:
                    continue
                b10 = dt.replace(minute=(dt.minute // SESSION_BUCKET_MIN) * SESSION_BUCKET_MIN,
                                 second=0, microsecond=0)
                for dim, b in (("weekday", dt.weekday()), ("hour", dt.hour)):
                    active[(dim, b)].add(b10)
                    if is_human_prompt(rec):
                        attended[(dim, b)].add(b10)
                if rec.get("type") == "assistant":
                    m = rec.get("message") or {}
                    key = (m.get("id"), rec.get("requestId"))
                    if key in seen_msg:
                        continue
                    seen_msg.add(key)
                    u = m.get("usage") or {}
                    for dim, b in (("weekday", dt.weekday()), ("hour", dt.hour)):
                        cell(dim, b)["tokens"] += (u.get("input_tokens", 0)
                                                   + u.get("output_tokens", 0)
                                                   + u.get("cache_creation_input_tokens", 0))
    for k, v in active.items():
        cell(*k)["active_buckets"] = len(v)
    for k, v in attended.items():
        cell(*k)["attended_buckets"] = len(v)
    return rows


def git_series():
    """Per-day cumulative commits, test count, and authored LOC at each day's last commit.

    ``go_code`` is Go (code + tests, block comments counted as code); ``md`` is
    non-blank Markdown; ``yaml`` is non-blank *hand-written* YAML — generated YAML
    (CRDs and other controller-gen output) is excluded with the same heuristic the
    HEAD snapshot uses, so it isn't credited as authored output.

    ``prs`` and ``queue_closed`` are cumulative like the rest. ``active_hours`` is
    not: it is the count of distinct clock hours that day with a commit landing in
    them, a per-day quantity that means nothing accumulated. It says when work was
    landing across the whole project, including the era whose transcripts are gone.

    It is not hours worked. Sessions sometimes run unattended and keep committing
    with nobody watching, and merges get cleared in bulk, so the spread of the day
    this covers is a property of the system rather than of anyone's presence, and
    the attended share of it varies with what kind of work is in flight.
    """
    rows = {}
    log = git("log", "--reverse", "--format=%H|%ad|%s",
              "--date=format:%Y-%m-%d %H").splitlines()
    day_commits = defaultdict(int)
    day_prs = defaultdict(int)
    day_hours = defaultdict(set)
    day_kind = defaultdict(lambda: defaultdict(int))
    last_hash = {}
    for ln in log:
        if "|" not in ln:
            continue
        h, stamp, subj = ln.split("|", 2)
        d, _, hour = stamp.partition(" ")
        day_commits[d] += 1
        day_hours[d].add(hour)
        if PR_SUBJECT.search(subj.strip()):
            day_prs[d] += 1
        m = CONVENTIONAL.match(subj.strip())
        if m and m.group(1) in ("feat", "fix"):
            day_kind[d][m.group(1)] += 1
        last_hash[d] = h  # --reverse => last write wins => latest commit that day

    closures, filings = queue_flow()
    cum_prs = 0
    cum_closed = 0
    cum_filed = 0

    cum = 0
    for d in sorted(day_commits):
        cum += day_commits[d]
        cum_prs += day_prs[d]
        cum_closed += closures.get(d, 0)
        cum_filed += filings.get(d, 0)
        rev = last_hash[d]
        nonblank = grep_count(rev, "[^[:space:]]", GO_PATHS)
        line_comments = grep_count(rev, "^[[:space:]]*//", GO_PATHS)
        test_nonblank = grep_count(rev, "[^[:space:]]", TEST_PATHS)
        test_comments = grep_count(rev, "^[[:space:]]*//", TEST_PATHS)
        md = sum(grep_lines_per_file(rev, "[^[:space:]]", MD_PATHS).values())
        yaml_counts = grep_lines_per_file(rev, "[^[:space:]]", YAML_PATHS)
        generated = grep_files_matching(rev, "code generated|controller-gen", YAML_PATHS)
        yaml_hand = sum(c for p, c in yaml_counts.items()
                        if p not in generated and "/crd/" not in p)
        go_w = grep_word_count(rev, "[^[:space:]]", GO_PATHS)
        md_w = grep_word_count(rev, "[^[:space:]]", MD_PATHS)
        yaml_words_per_file = grep_words_per_file(rev, "[^[:space:]]", YAML_PATHS)
        yaml_w = sum(c for p, c in yaml_words_per_file.items()
                     if p not in generated and "/crd/" not in p)
        scripts_w = grep_word_count(rev, "[^[:space:]]", SCRIPT_PATHS)
        rows[d] = {
            "date": d,
            "commits": cum,
            "tests": grep_count(rev, "^func Test", TEST_PATHS),
            "go_code": nonblank - line_comments,           # all Go: non-test + test
            "go_test": test_nonblank - test_comments,      # the test subset of go_code
            "md": md,
            "yaml": yaml_hand,
            "scripts": grep_count(rev, "[^[:space:]]", SCRIPT_PATHS),  # shell/python/web/build
            "prs": cum_prs,
            "queue_closed": cum_closed,
            "queue_filed": cum_filed,
            "active_hours": len(day_hours[d]),  # per-day, not cumulative
            # Also per-day: the churn signal. Coarse in two directions that do not
            # cancel predictably, since a `fix` may repair something ancient and
            # more gates catch more defects without more existing.
            "feat": day_kind[d]["feat"],
            "fix": day_kind[d]["fix"],
            # The reformat-proof twins of the line counts, per band so the cost
            # ratio's denominator can be decomposed the same way. Every one counts
            # non-blank words including comments, so `words` is not `go_code + md +
            # yaml + scripts` in another unit — it is the same corpus with nothing
            # subtracted.
            "go_words": go_w,
            "md_words": md_w,
            "yaml_words": yaml_w,
            "scripts_words": scripts_w,
            "words": go_w + md_w + yaml_w + scripts_w,
        }
    return rows


def go_split(path):
    """Split a Go file into (code, comment, blank) line counts."""
    code = comment = blank = 0
    in_block = False
    try:
        lines = open(path, errors="replace").read().split("\n")
    except OSError:
        return (0, 0, 0)
    for ln in lines:
        s = ln.strip()
        if in_block:
            comment += 1
            if "*/" in s:
                in_block = False
        elif s == "":
            blank += 1
        elif s.startswith("//"):
            comment += 1
        elif s.startswith("/*"):
            comment += 1
            if "*/" not in s[2:]:
                in_block = True
        else:
            code += 1
    return (code, comment, blank)


def head_snapshot():
    """Accurate line/test counts for the current working tree (excludes vendor)."""
    tracked = [
        f for f in git("ls-files").splitlines()
        if f and "/vendor/" not in f and not f.startswith("vendor/")
    ]
    go = [0, 0, 0]
    md_nonblank = 0
    yaml_hand = 0
    yaml_gen = 0
    tests = 0
    for rel in tracked:
        path = os.path.join(REPO, rel)
        if rel.endswith(".go"):
            c = go_split(path)
            go = [a + b for a, b in zip(go, c)]
            if rel.endswith("_test.go"):
                for ln in open(path, errors="replace"):
                    if ln.startswith("func Test"):
                        tests += 1
        elif rel.endswith(".md"):
            md_nonblank += sum(1 for ln in open(path, errors="replace") if ln.strip())
        elif rel.endswith((".yaml", ".yml")):
            txt = open(path, errors="replace").read()
            n = sum(1 for ln in txt.split("\n") if ln.strip())
            head = txt.lower()[:500]
            generated = ("code generated" in head) or ("/crd/" in rel) or ("controller-gen" in head)
            if generated:
                yaml_gen += n
            else:
                yaml_hand += n
    return {
        "go_code": go[0], "go_comments": go[1],
        "markdown_nonblank": md_nonblank,
        "yaml_handwritten": yaml_hand, "yaml_generated": yaml_gen,
        "tests": tests,
        "commits": int(git("rev-list", "--count", "HEAD").strip() or 0),
    }


def load_csv(path, key_cols, defaults=None):
    """Read a CSV into ``{key tuple: row}``.

    ``defaults`` fills key columns a file predates: rows written before ``host``
    existed carry no machine, and all came from ``LEGACY_HOST``.
    """
    rows = {}
    if not os.path.exists(path):
        return rows
    with open(path) as fh:
        for r in csv.DictReader(fh):
            for c, v in (defaults or {}).items():
                if not r.get(c):
                    r[c] = v
            rows[tuple(r[c] for c in key_cols)] = r
    return rows


def write_csv(path, fieldnames, rows):
    with open(path, "w", newline="") as fh:
        w = csv.DictWriter(fh, fieldnames=fieldnames)
        w.writeheader()
        for r in rows:
            w.writerow(r)


def is_estimated(row):
    return str(row.get("estimated", "0")) in ("1", "true", "True")


# Text that arrives as a user record carrying ``origin.kind == "human"`` without a
# human having typed anything: a hook's denial, a bash line and its output, a
# slash command's expansion, an injected reminder. Q687's predicate, in code.
NOT_A_PROMPT = ("<bash-input>", "<bash-stdout>", "<bash-stderr>", "<create-pr-command>",
                "<system-reminder>", "<command-name>", "<local-command")


def record_text(rec):
    """The text of a user record, whether it arrived as a string or as blocks."""
    c = (rec.get("message") or {}).get("content")
    if isinstance(c, str):
        return c
    if isinstance(c, list):
        return " ".join(b.get("text", "") for b in c if isinstance(b, dict))
    return ""


def is_human_prompt(rec):
    """True when a person actually typed this.

    The only unambiguous presence signal in the transcripts. Everything else the
    session series counts, including every assistant record, is produced whether
    or not anyone is watching, so a bucket holding one of these is the one kind
    that says somebody was at the keyboard.

    ``origin.kind`` alone is not enough: four classes carry it without a person
    typing, which is why they are stripped rather than trusted.
    """
    if rec.get("type") != "user" or not rec.get("timestamp"):
        return False
    if (rec.get("origin") or {}).get("kind") != "human":
        return False
    return not record_text(rec).lstrip().startswith(NOT_A_PROMPT)


def session_series(host):
    """Per-day session concurrency for this machine, keyed ``(date, host)``.

    A session is *active* in a bucket if it produced a record there; concurrency
    is how many were active in the same bucket. Resumed sessions replay earlier
    records verbatim, which would credit the resuming session with work it only
    re-read, so each record is attributed to the earliest-starting session
    holding it. Only records carrying a ``uuid`` count, since those are the ones
    a replay can be recognised by.

    Every measure is a count that can only rise as more transcripts become
    visible, so the upward-only merge is right for all of them. This series has
    no estimated rows: concurrency cannot be modelled from commit counts the way
    token volume can, so it begins at the first day with surviving transcripts.
    """
    files = []
    for d in glob.glob(PROJECTS_GLOB):
        files += glob.glob(os.path.join(d, "*.jsonl"))

    recs = []    # (session, uuid, bucket)
    start = {}   # session -> its earliest bucket
    attended = set()   # buckets a person typed in
    for f in files:
        sess = os.path.basename(f)[: -len(".jsonl")]
        try:
            fh = open(f, errors="replace")
        except OSError:
            continue
        with fh:
            for line in fh:
                line = line.strip()
                if not line:
                    continue
                try:
                    rec = json.loads(line)
                except ValueError:
                    continue
                uuid, ts = rec.get("uuid"), rec.get("timestamp")
                if not uuid or not ts:
                    continue
                try:
                    dt = datetime.fromisoformat(ts.replace("Z", "+00:00"))
                except ValueError:
                    continue
                b = dt.replace(minute=(dt.minute // SESSION_BUCKET_MIN) * SESSION_BUCKET_MIN,
                               second=0, microsecond=0)
                if is_human_prompt(rec):
                    attended.add(b)
                recs.append((sess, uuid, b))
                if sess not in start or b < start[sess]:
                    start[sess] = b

    owner = {}
    for sess, uuid, _ in recs:
        cur = owner.get(uuid)
        if cur is None or start[sess] < start[cur]:
            owner[uuid] = sess

    active = defaultdict(set)  # bucket -> sessions that did work in it
    for sess, uuid, b in recs:
        if owner[uuid] == sess:
            active[b].add(sess)

    day = defaultdict(lambda: {"sessions": set(), "peak": 0, "active": 0, "parallel": 0,
                               "session_buckets": 0, "attended": 0})
    for b, sessions in active.items():
        row = day[b.date().isoformat()]
        row["sessions"] |= sessions
        row["peak"] = max(row["peak"], len(sessions))
        row["active"] += 1
        if b in attended:
            row["attended"] += 1
        row["session_buckets"] += len(sessions)
        if len(sessions) > 1:
            row["parallel"] += 1

    return {
        (dk, host): {
            "date": dk, "host": host,
            "sessions": len(v["sessions"]),
            "peak_concurrent": v["peak"],
            "active_buckets": v["active"],
            "parallel_buckets": v["parallel"],
            # Mean concurrency and total session time are this / active_buckets and
            # this / buckets-per-hour. Stored as the integer rather than as either
            # derived figure: a ratio is not monotone, so it cannot be max-merged.
            "session_buckets": v["session_buckets"],
            # Buckets a person typed in. Zero for any day before the transcripts
            # began carrying `origin.kind`, which is why it is reported with its
            # own first date rather than compared against the token series.
            "attended_buckets": v["attended"],
        }
        for dk, v in day.items()
    }


def sessions_summary(rows):
    """Headline session figures, derived from the persisted bucket counts.

    ``hours_using_claude`` is wall-clock: buckets where at least one session did
    something, so a session left open overnight adds nothing. ``session_hours``
    sums concurrent sessions over the same buckets, and their ratio is the mean
    concurrency — the multiplier between elapsed time and work in flight.

    Neither figure is human presence. A session left to run unattended produces
    records the whole time it works, so these count hours the *system* was active
    rather than hours anyone was watching it, and how much of it was attended
    varies with the work and with whatever else the day held.
    """
    per_hour = 60 / SESSION_BUCKET_MIN
    active = sum(r["active_buckets"] for r in rows)
    sess_b = sum(r["session_buckets"] for r in rows)
    att = sum(int(r.get("attended_buckets") or 0) for r in rows)
    return {
        "bucket_minutes": SESSION_BUCKET_MIN,
        "first_date": min((r["date"] for r in rows), default=None),
        "last_date": max((r["date"] for r in rows), default=None),
        # Per-day counts summed: a session spanning midnight counts in both days,
        # so this is session-days, not a distinct-session total.
        "session_days": sum(r["sessions"] for r in rows),
        "peak_concurrent": max((r["peak_concurrent"] for r in rows), default=0),
        "mean_concurrent": round(sess_b / active, 2) if active else 0,
        "hours_using_claude": round(active / per_hour, 1),
        "session_hours": round(sess_b / per_hour, 1),
        "parallel_share_pct": (round(100 * sum(r["parallel_buckets"] for r in rows) / active)
                               if active else 0),
        # Presence, not activity. Its own first date: the transcripts only began
        # carrying the marker that identifies a typed prompt partway through, so
        # this series starts later than the rest and cannot be compared with them
        # across that boundary.
        #
        # The hours are a scale rather than a measurement. Prompts are sparse point
        # events and each one credits a whole bucket, so the total scales with the
        # bucket width: 18.4h at 1 minute, 102h at 10, 224h at 60. The wall-clock
        # series above is far less sensitive because its records are dense. What
        # holds regardless is the prompt count and the shape across buckets, since
        # every bucket everywhere uses the one width.
        "attended_hours": round(att / per_hour, 1),
        "attended_share_pct": round(100 * att / active) if active else 0,
        "attended_first_date": min((r["date"] for r in rows
                                    if int(r.get("attended_buckets") or 0) > 0), default=None),
        "prompts_note": ("attended_hours scales with bucket_minutes; the parameter-free "
                         "figures are the prompt count and the cross-bucket shape"),
        "note": ("Concurrency needs session-level transcripts, which no earlier CSV "
                 "preserved, so the series starts at the first day whose transcripts "
                 "survive rather than at the first project day. It is never estimated."),
    }


def load_measured(path, key_cols, num_cols, defaults=None):
    """Load existing rows, keeping only the *measured* ones (drops old estimates)."""
    merged = {}
    for k, r in load_csv(path, key_cols, defaults).items():
        if is_estimated(r):
            continue
        merged[k] = {c: int(float(r.get(c) or 0)) for c in num_cols}
        for kc, kv in zip(key_cols, k):
            merged[k][kc] = kv
    return merged


def merge_max_into(merged, new_rows, key_cols, num_cols):
    """Fold ``new_rows`` into ``merged`` in place, taking the per-column MAX.

    Preserves keys present in ``merged`` but absent from ``new_rows`` (dates whose
    source sessions were archived), and only ever revises a value upward.

    Keys carry the machine id, so the MAX is *within* one machine — it guards a
    re-run after that machine's sessions were archived, which must never revise a
    value downward. Two machines hold disjoint sessions and land on different
    keys, so their shares of the same day stay as separate rows and are summed by
    consumers (``sum_by_date``); taking a MAX across machines would silently keep
    only the busier one.
    """
    for k, r in new_rows.items():
        kk = k if isinstance(k, tuple) else (k,)
        if kk in merged:
            for c in num_cols:
                merged[kk][c] = max(merged[kk][c], int(r[c]))
        else:
            merged[kk] = {c: int(r[c]) for c in num_cols}
            for kc, kv in zip(key_cols, kk):
                merged[kk][kc] = kv
    return merged


def sum_by_date(rows, num_cols):
    """Collapse per-machine rows into one total per date."""
    daily = defaultdict(lambda: defaultdict(int))
    for r in rows:
        for c in num_cols:
            daily[r["date"]][c] += int(r[c])
    return daily


def commit_deltas(git_rows):
    """Commits authored on each day, from the cumulative commit series."""
    deltas = {}
    prev = 0
    for d in sorted(git_rows):
        c = int(git_rows[d]["commits"])
        deltas[d] = c - prev
        prev = c
    return deltas


def main():
    os.makedirs(DATA, exist_ok=True)
    host = resolve_host()
    token_rows, model_rows, n_files = aggregate_logs(host)

    token_csv = os.path.join(DATA, "token_metrics.csv")
    model_csv = os.path.join(DATA, "model_daily.csv")
    git_csv = os.path.join(DATA, "git_metrics.csv")
    sess_csv = os.path.join(DATA, "session_metrics.csv")
    pr_csv = os.path.join(DATA, "pr_metrics.csv")
    surv_csv = os.path.join(DATA, "survival_metrics.csv")
    rhythm_csv = os.path.join(DATA, "rhythm_metrics.csv")

    git_rows = git_series()
    write_csv(git_csv, ["date", "commits", "tests", "go_code", "go_test", "md", "yaml", "scripts",
                        "prs", "queue_closed", "queue_filed", "active_hours", "feat", "fix",
                        "go_words", "md_words", "yaml_words", "scripts_words", "words"],
              [git_rows[d] for d in sorted(git_rows)])
    # Pull requests: merge-preserved and fetched incrementally, because the source
    # is a remote API rather than something recomputable offline. Existing rows are
    # never re-fetched; a failed fetch leaves them exactly as they are.
    pr_rows = {}
    try:
        with open(pr_csv) as fh:
            for r in csv.DictReader(fh):
                pr_rows[int(r["number"])] = r
    except (OSError, ValueError, KeyError):
        pass
    before = len(pr_rows)
    pr_rows.update(pr_series(max(pr_rows) if pr_rows else 0))
    if pr_rows:
        write_csv(pr_csv, ["number", "merged_date", "created_at", "merged_at", "cycle_hours"],
                  [pr_rows[n] for n in sorted(pr_rows)])
    print(f"pull requests      : {len(pr_rows)} on record (+{len(pr_rows) - before} this run)")

    # Code survival: immutable once a week's horizon passes, so stored weeks are
    # never recomputed and only newly-final ones cost a blame pass.
    surv_rows = {}
    try:
        with open(surv_csv) as fh:
            for r in csv.DictReader(fh):
                surv_rows[r["week_start"]] = r
    except (OSError, KeyError):
        pass
    s_before = len(surv_rows)
    surv_rows.update(survival_series(set(surv_rows)))
    if surv_rows:
        write_csv(surv_csv, ["week_start", "added", "survived", "horizon_days", "rate"],
                  [surv_rows[w] for w in sorted(surv_rows)])
    print(f"code survival      : {len(surv_rows)} week(s) final (+{len(surv_rows) - s_before} this run)")

    # Weekday and hour-of-day aggregates. Merge-preserved on the counts for the same
    # reason the token series is: the hourly resolution exists only in the
    # transcripts, so a recompute after they are archived would silently lower it.
    rnum = ["days", "commits", "prs", "feat", "fix", "tokens", "attended_buckets",
            "active_buckets"]
    rkey = ["dim", "bucket"]
    offset = local_offset()
    r_measured = load_measured(rhythm_csv, rkey, rnum)
    merge_max_into(r_measured, rhythm_series(offset), rkey, rnum)
    # One row records the offset everything else was bucketed in, so make_charts
    # can bucket PR timestamps identically without re-deriving or assuming it.
    r_measured[("offset_hours", offset)] = {"dim": "offset_hours", "bucket": offset,
                                            **{c: 0 for c in rnum}}
    write_csv(rhythm_csv, rkey + rnum, [r_measured[k] for k in sorted(r_measured, key=str)])
    print(f"rhythm             : local UTC{offset:+g}, {len(r_measured)} weekday/hour buckets")

    deltas = commit_deltas(git_rows)

    # --- tokens: preserve measured days, then backfill archived days as estimated ---
    tnum = ["input", "output", "cache_creation", "cache_read", "assistant_msgs", "user_msgs"]
    tkey = ["date", "host"]
    measured = load_measured(token_csv, tkey, tnum, {"host": LEGACY_HOST})
    merge_max_into(measured, token_rows, tkey, tnum)
    daily = sum_by_date(measured.values(), tnum)  # per-machine rows -> day totals
    measured_dates = sorted(daily)

    # Per-commit rate from the Pro-era window (measured days before the Max upgrade)
    # — the archived days were that same Pro/Sonnet era, so the rate transfers.
    window = [d for d in measured_dates if d < PRO_TO_MAX] or measured_dates[:4]
    win_commits = sum(deltas.get(d, 0) for d in window) or 1
    rates = {c: sum(daily[d][c] for d in window) / win_commits for c in tnum}

    # Archived = project days (from durable git history) before the first measured day.
    first_measured = measured_dates[0] if measured_dates else None
    archived = [d for d in sorted(git_rows) if first_measured and d < first_measured]
    est_rows = []
    for d in archived:
        row = {"date": d, "host": EST_HOST, "estimated": 1}
        for c in tnum:
            row[c] = int(round(rates[c] * deltas.get(d, 0)))
        est_rows.append(row)

    out_rows = est_rows + [{**measured[k], "estimated": 0} for k in sorted(measured)]
    out_rows.sort(key=lambda r: (r["date"], r["host"]))
    write_csv(token_csv, tkey + tnum + ["estimated"], out_rows)

    # --- model_daily: preserve measured, backfill archived as Pro-era Sonnet 4.6 ---
    mnum = ["headline", "output", "messages"]
    mkey = ["date", "model", "host"]
    m_measured = load_measured(model_csv, mkey, mnum, {"host": LEGACY_HOST})
    merge_max_into(m_measured, model_rows, mkey, mnum)
    head_rate = rates["input"] + rates["output"] + rates["cache_creation"]
    est_model = [
        {"date": d, "model": "Sonnet 4.6", "host": EST_HOST,
         "headline": int(round(head_rate * deltas.get(d, 0))),
         "output": int(round(rates["output"] * deltas.get(d, 0))),
         "messages": int(round(rates["assistant_msgs"] * deltas.get(d, 0))),
         "estimated": 1}
        for d in archived
    ]
    m_out = est_model + [{**m_measured[k], "estimated": 0} for k in sorted(m_measured)]
    m_out.sort(key=lambda r: (r["date"], r["model"], r["host"]))
    write_csv(model_csv, mkey + mnum + ["estimated"], m_out)

    # --- sessions: measured only, no backfill (see session_series) ---
    # attended_buckets joins the upward-only merge like the rest: it is a count that
    # can only rise as more transcripts become visible.
    snum = ["sessions", "peak_concurrent", "active_buckets", "parallel_buckets",
            "session_buckets", "attended_buckets"]
    skey = ["date", "host"]
    s_measured = load_measured(sess_csv, skey, snum)
    merge_max_into(s_measured, session_series(host), skey, snum)
    s_out = [s_measured[k] for k in sorted(s_measured)]
    write_csv(sess_csv, skey + snum, s_out)

    # --- totals: measured vs estimated, summed from the persisted rows ---
    def total(rows, cols):
        return {c: sum(int(r[c]) for r in rows) for c in cols}

    meas = total([r for r in out_rows if not r["estimated"]], tnum)
    est = total([r for r in out_rows if r["estimated"]], tnum)
    comb = {c: meas[c] + est[c] for c in tnum}

    def headline(t):
        return t["input"] + t["output"] + t["cache_creation"]

    host_tot = defaultdict(lambda: defaultdict(int))
    for r in out_rows:
        for c in tnum:
            host_tot[r["host"]][c] += int(r[c])

    model_tot = defaultdict(lambda: defaultdict(int))
    for r in m_out:
        model_tot[r["model"]]["headline"] += int(r["headline"])
        model_tot[r["model"]]["output"] += int(r["output"])
        model_tot[r["model"]]["messages"] += int(r["messages"])

    summary = {
        "provenance": {
            "snapshot_date": datetime.now().date().isoformat(),
            "projects_glob": PROJECTS_GLOB,
            "transcript_files_read": n_files,
            "snapshot_host": host,
            "hosts": sorted({r["host"] for r in out_rows if r["host"] != EST_HOST}),
            "host_basis": ("token/message rows are per (date, machine); a day's total is "
                           "the sum across machines, and the upward-only merge applies "
                           "within a machine"),
            "token_date_basis": "UTC date of message timestamp",
            "git_date_basis": "author date (local), --date=short",
            "token_dedup": "(message.id, requestId)",
            "message_dedup": "record uuid (user) / (message.id, requestId) (assistant)",
            "first_measured_date": first_measured,
            "last_measured_date": measured_dates[-1] if measured_dates else None,
            "first_project_date": min(git_rows) if git_rows else None,
            "growth_chart_baseline": {"tokens": 10_000_000, "commits": 232, "tests": 269, "go_code": 15500,
                                 "note": "published day-7 Bluesky post values; the growth chart plots growth vs these"},
            "pro_to_max_date": PRO_TO_MAX,
            "max_5x_to_20x_date": MAX_5X_TO_20X,
            "docs_reflow_date": DOCS_REFLOW,
            "docs_reflow_note": ("every tracked doc reflowed to sentence-per-line; the `md` "
                                 "series steps down ~18.6k lines here with no words removed"),
        },
        "estimation": {
            "method": "per-commit Pro-era rate x commits authored that day",
            "rate_window_dates": window,
            "rate_window_commits": win_commits,
            "headline_tokens_per_commit": round(head_rate),
            "archived_dates": archived,
            "archived_commits": sum(deltas.get(d, 0) for d in archived),
            "note": ("Pre-transcript days (sessions archived before any were saved) are "
                     "backfilled from the Pro-era per-commit rate and flagged estimated=1 "
                     "in the CSVs. Measured days are never overwritten by estimates."),
        },
        "totals": {
            "measured": {**meas, "headline_input_output_cachecreation": headline(meas)},
            "estimated": {**est, "headline_input_output_cachecreation": headline(est)},
            "combined": {
                **comb,
                "headline_input_output_cachecreation": headline(comb),
                "grand_total_incl_cache_read": headline(comb) + comb["cache_read"],
                "cache_reuse_ratio": round(comb["cache_read"] / comb["cache_creation"], 2) if comb["cache_creation"] else None,
            },
        },
        "by_model": {m: dict(v) for m, v in model_tot.items()},
        "by_host": {
            h: {**dict(v), "headline_input_output_cachecreation": headline(v)}
            for h, v in sorted(host_tot.items())
        },
        "sessions": sessions_summary(s_out),
        "head_snapshot": head_snapshot(),
    }
    with open(os.path.join(DATA, "summary.json"), "w") as fh:
        json.dump(summary, fh, indent=2)
        fh.write("\n")

    print(f"machine            : {host}")
    print(f"transcripts read   : {n_files}")
    print(f"machines on record : {', '.join(summary['provenance']['hosts']) or '(none)'}")
    print(f"measured span      : {first_measured} -> {summary['provenance']['last_measured_date']}")
    print(f"backfilled (est.)  : {archived} ({summary['estimation']['archived_commits']} commits)")
    ss = summary["sessions"]
    print(f"sessions           : {ss['session_days']} session-days over "
          f"{ss['first_date']} -> {ss['last_date']}")
    print(f"  concurrency      : mean {ss['mean_concurrent']}, peak {ss['peak_concurrent']}, "
          f"{ss['parallel_share_pct']}% of active time parallel")
    print(f"  at the keyboard  : {ss['attended_hours']}h attended "
          f"({ss['attended_share_pct']}%), from {ss['attended_first_date']}")
    print(f"  time on claude   : {ss['hours_using_claude']}h wall-clock, "
          f"{ss['session_hours']}h session-time")
    print(f"headline measured  : {headline(meas):,}")
    print(f"headline estimated : +{headline(est):,}")
    print(f"headline combined  : {headline(comb):,}")
    print(f"  + cache_read     : {headline(comb) + comb['cache_read']:,}")
    print(f"cache reuse        : {summary['totals']['combined']['cache_reuse_ratio']}x")
    print(f"wrote {token_csv}, {model_csv}, {git_csv}, {sess_csv}, {pr_csv}, {surv_csv}, {rhythm_csv}, summary.json")


if __name__ == "__main__":
    main()
