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

Run from anywhere:

    python3 claude-usage/compute_metrics.py

Environment:
    CLAUDE_PROJECTS_GLOB  Override the transcript glob. Default:
                          ~/.claude/projects/*github-actions-gateway*

Outputs (all under claude-usage/data/):
    token_metrics.csv   daily input/output/cache tokens + message counts (merge-preserved)
    model_daily.csv     daily per-model headline tokens (merge-preserved)
    git_metrics.csv     daily cumulative commits, test count, Go code LOC (recomputed)
    summary.json        headline totals, per-model split, HEAD snapshot, provenance
"""

import csv
import glob
import json
import os
import subprocess
from collections import defaultdict
from datetime import datetime

HERE = os.path.dirname(os.path.abspath(__file__))
DATA = os.path.join(HERE, "data")
REPO = subprocess.run(
    ["git", "-C", HERE, "rev-parse", "--show-toplevel"],
    capture_output=True, text=True,
).stdout.strip()

DEFAULT_GLOB = os.path.join(os.path.expanduser("~/.claude/projects"), "*github-actions-gateway*")
PROJECTS_GLOB = os.environ.get("CLAUDE_PROJECTS_GLOB", DEFAULT_GLOB)

# Date the plan upgraded Pro -> Max. Used both as a chart annotation and to bound
# the "Pro-era" window from which the archived-day backfill rate is derived.
PRO_TO_MAX = "2026-05-23"

# Token usage is deduped on (message.id, requestId): resumed/compacted sessions
# replay earlier assistant records verbatim, and counting them twice would inflate
# every total (cache_read especially). Message records are deduped on their uuid.
GO_PATHS = ["*.go", ":!vendor/**", ":!**/vendor/**"]
TEST_PATHS = ["*_test.go", ":!vendor/**", ":!**/vendor/**"]


def model_family(m):
    """Map a raw model id to a stable display family."""
    if not m:
        return "Unknown"
    if "sonnet" in m:
        return "Sonnet 4.6"
    if "opus-4-8" in m:
        return "Opus 4.8"
    if "opus-4-7" in m:
        return "Opus 4.7"
    if "haiku" in m:
        return "Haiku 4.5"
    return "Other"


def aggregate_logs():
    """Aggregate per-day token + message metrics from the session transcripts."""
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
        token_rows[dk] = {
            "date": dk,
            "input": fields.get("input", 0),
            "output": fields.get("output", 0),
            "cache_creation": fields.get("cache_creation", 0),
            "cache_read": fields.get("cache_read", 0),
            "assistant_msgs": asst_msgs.get(dk, 0),
            "user_msgs": user_msgs.get(dk, 0),
        }
    model_rows = {
        (dk, fam): {
            "date": dk, "model": fam,
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


def git_series():
    """Per-day cumulative commits, test count, and Go code LOC at each day's last commit."""
    rows = {}
    log = git("log", "--reverse", "--format=%H|%ad", "--date=short").splitlines()
    day_commits = defaultdict(int)
    last_hash = {}
    for ln in log:
        if "|" not in ln:
            continue
        h, d = ln.split("|", 1)
        day_commits[d] += 1
        last_hash[d] = h  # --reverse => last write wins => latest commit that day

    cum = 0
    for d in sorted(day_commits):
        cum += day_commits[d]
        rev = last_hash[d]
        nonblank = grep_count(rev, "[^[:space:]]", GO_PATHS)
        line_comments = grep_count(rev, "^[[:space:]]*//", GO_PATHS)
        rows[d] = {
            "date": d,
            "commits": cum,
            "tests": grep_count(rev, "^func Test", TEST_PATHS),
            "go_code": nonblank - line_comments,
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


def load_csv(path, key_cols):
    rows = {}
    if not os.path.exists(path):
        return rows
    with open(path) as fh:
        for r in csv.DictReader(fh):
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


def load_measured(path, key_cols, num_cols):
    """Load existing rows, keeping only the *measured* ones (drops old estimates)."""
    merged = {}
    for k, r in load_csv(path, key_cols).items():
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
    token_rows, model_rows, n_files = aggregate_logs()

    token_csv = os.path.join(DATA, "token_metrics.csv")
    model_csv = os.path.join(DATA, "model_daily.csv")
    git_csv = os.path.join(DATA, "git_metrics.csv")

    git_rows = git_series()
    write_csv(git_csv, ["date", "commits", "tests", "go_code"],
              [git_rows[d] for d in sorted(git_rows)])
    deltas = commit_deltas(git_rows)

    # --- tokens: preserve measured days, then backfill archived days as estimated ---
    tnum = ["input", "output", "cache_creation", "cache_read", "assistant_msgs", "user_msgs"]
    measured = load_measured(token_csv, ["date"], tnum)
    merge_max_into(measured, token_rows, ["date"], tnum)
    measured = {k[0]: v for k, v in measured.items()}  # tuple key -> date string
    measured_dates = sorted(measured)

    # Per-commit rate from the Pro-era window (measured days before the Max upgrade)
    # — the archived days were that same Pro/Sonnet era, so the rate transfers.
    window = [d for d in measured_dates if d < PRO_TO_MAX] or measured_dates[:4]
    win_commits = sum(deltas.get(d, 0) for d in window) or 1
    rates = {c: sum(measured[d][c] for d in window) / win_commits for c in tnum}

    # Archived = project days (from durable git history) before the first measured day.
    first_measured = measured_dates[0] if measured_dates else None
    archived = [d for d in sorted(git_rows) if first_measured and d < first_measured]
    est_rows = []
    for d in archived:
        row = {"date": d, "estimated": 1}
        for c in tnum:
            row[c] = int(round(rates[c] * deltas.get(d, 0)))
        est_rows.append(row)

    out_rows = est_rows + [{**measured[d], "estimated": 0} for d in measured_dates]
    out_rows.sort(key=lambda r: r["date"])
    write_csv(token_csv, ["date"] + tnum + ["estimated"], out_rows)

    # --- model_daily: preserve measured, backfill archived as Pro-era Sonnet 4.6 ---
    mnum = ["headline", "output", "messages"]
    m_measured = load_measured(model_csv, ["date", "model"], mnum)
    merge_max_into(m_measured, model_rows, ["date", "model"], mnum)
    head_rate = rates["input"] + rates["output"] + rates["cache_creation"]
    est_model = [
        {"date": d, "model": "Sonnet 4.6",
         "headline": int(round(head_rate * deltas.get(d, 0))),
         "output": int(round(rates["output"] * deltas.get(d, 0))),
         "messages": int(round(rates["assistant_msgs"] * deltas.get(d, 0))),
         "estimated": 1}
        for d in archived
    ]
    m_out = est_model + [{**m_measured[k], "estimated": 0} for k in sorted(m_measured)]
    m_out.sort(key=lambda r: (r["date"], r["model"]))
    write_csv(model_csv, ["date", "model"] + mnum + ["estimated"], m_out)

    # --- totals: measured vs estimated, summed from the persisted rows ---
    def total(rows, cols):
        return {c: sum(int(r[c]) for r in rows) for c in cols}

    meas = total([r for r in out_rows if not r["estimated"]], tnum)
    est = total([r for r in out_rows if r["estimated"]], tnum)
    comb = {c: meas[c] + est[c] for c in tnum}

    def headline(t):
        return t["input"] + t["output"] + t["cache_creation"]

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
            "token_date_basis": "UTC date of message timestamp",
            "git_date_basis": "author date (local), --date=short",
            "token_dedup": "(message.id, requestId)",
            "message_dedup": "record uuid (user) / (message.id, requestId) (assistant)",
            "first_measured_date": first_measured,
            "last_measured_date": measured_dates[-1] if measured_dates else None,
            "first_project_date": min(git_rows) if git_rows else None,
            "chart_E_baseline": {"tokens": 10_000_000, "commits": 232, "tests": 269, "go_code": 15500,
                                 "note": "published day-7 Bluesky post values; chart E plots growth vs these"},
            "pro_to_max_date": PRO_TO_MAX,
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
        "head_snapshot": head_snapshot(),
    }
    with open(os.path.join(DATA, "summary.json"), "w") as fh:
        json.dump(summary, fh, indent=2)
        fh.write("\n")

    print(f"transcripts read   : {n_files}")
    print(f"measured span      : {first_measured} -> {summary['provenance']['last_measured_date']}")
    print(f"backfilled (est.)  : {archived} ({summary['estimation']['archived_commits']} commits)")
    print(f"headline measured  : {headline(meas):,}")
    print(f"headline estimated : +{headline(est):,}")
    print(f"headline combined  : {headline(comb):,}")
    print(f"  + cache_read     : {headline(comb) + comb['cache_read']:,}")
    print(f"cache reuse        : {summary['totals']['combined']['cache_reuse_ratio']}x")
    print(f"wrote {token_csv}, {model_csv}, {git_csv}, summary.json")


if __name__ == "__main__":
    main()
