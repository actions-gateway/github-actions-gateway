#!/usr/bin/env python3
"""Render the kept charts from the persisted CSVs in claude-usage/data/.

Reads only the committed CSVs (never the raw transcripts), so the charts are
reproducible even after the source sessions are archived. Rows flagged
``estimated=1`` (the backfilled pre-transcript days) are drawn distinctly —
hatched bars, dashed lines, and a shaded band — so estimates are never passed
off as measured data.

Colours use the Okabe–Ito colourblind-safe palette, and every multi-series chart
adds a non-colour cue too (hatch patterns on stacked fills, distinct line styles
and markers on line charts) so hue is never the only thing distinguishing series.
Run:

    python3 claude-usage/make_charts.py        # needs matplotlib + numpy

Outputs PNGs (1x + @2x) to claude-usage/charts/:
    tokens_by_model      daily token usage by model, with the plan-upgrade markers
    tokens_per_word      cost-per-word ratio + the words composition (stacked)
    tokens_vs_words      cumulative tokens vs words authored (log scale)
    tokens_overview      all three tokens/words views stacked on one timeline
    token_anatomy        daily input/output/cache tokens on a log scale
    cumulative_cache     cumulative cache reads vs writes (stacked area)
    parallel_sessions    peak concurrency + the parallel share (own shorter timeline)
    velocity             work shipped per week on reformat- and workflow-proof proxies
    lines_vs_words       the same corpus in lines and in words, and each cost ratio
"""

import csv
import json
import os
from datetime import date, timedelta

import matplotlib
matplotlib.use("Agg")
matplotlib.rcParams["hatch.linewidth"] = 0.6  # thinner hatches read as texture, not noise
import matplotlib.dates as mdates
import matplotlib.patches as mpatches
import matplotlib.patheffects as pe
import matplotlib.pyplot as plt
import numpy as np
from matplotlib.lines import Line2D

HERE = os.path.dirname(os.path.abspath(__file__))
DATA = os.path.join(HERE, "data")
CHARTS = os.path.join(HERE, "charts")

PRO_TO_MAX = date(2026, 5, 23)
MAX_5X_TO_20X = date(2026, 7, 5)  # Max 5x -> Max 20x plan upgrade
# Every tracked doc reflowed to sentence-per-line (#1357). Unwrapping hard-wrapped
# paragraphs cut ~18.6k non-blank Markdown lines without deleting a word, so the
# lines denominator steps down here and the cost-per-line ratio steps up.
DOCS_REFLOW = date(2026, 8, 9)

# Event markers. A plan upgrade raises the ceiling on what one machine can spend;
# a machine starting changes which machine is doing the measuring; a reflow moves
# the lines series with no change in what was spent. Different causes, so
# different styling — and the reflow is drawn only on the charts it distorts.
PLAN_STYLE = ("#222", "--")
MODEL_STYLE = ("#8C4A00", (0, (7, 2)))
MACHINE_STYLE = ("#005E44", (0, (5, 2, 1, 2)))
REFLOW_STYLE = ("#5B5B5B", (0, (1, 1.8)))

# Event lines are drawn at zorder 5, after the series they cross. A value label
# left at the default zorder is painted over and reads as struck through, so
# every one of them sits above the lines and cuts them with its white halo —
# the same trade `event_label` makes for the event's own text.
EVENT_Z = 5
LABEL_Z = 7
HALO = [pe.Stroke(linewidth=3, foreground="white"), pe.Normal()]

# Okabe–Ito colourblind-safe palette.
OI = {
    "orange": "#E69F00", "skyblue": "#56B4E9", "green": "#009E73",
    "yellow": "#F0E442", "blue": "#0072B2", "vermillion": "#D55E00",
    "purple": "#CC79A7", "grey": "#999999",
}
GOLD = "#9A6E1E"  # single-series "cost / line" accent (no CB clash — used alone)

MODEL_COLORS = {
    "Sonnet 4.6": OI["orange"], "Opus 4.7": OI["purple"],
    "Opus 4.8": OI["blue"], "Opus 5": OI["vermillion"], "Fable 5": OI["green"],
    "Haiku 4.5": OI["skyblue"], "Other": OI["grey"], "Unknown": "#CCCCCC",
}
# A distinct hatch per model so stacked bars are separable without colour.
MODEL_HATCH = {
    "Sonnet 4.6": "", "Opus 4.7": "..", "Opus 4.8": "xx", "Opus 5": "--",
    "Fable 5": "\\\\", "Haiku 4.5": "++", "Other": "oo", "Unknown": "",
}
# The lines-authored breakdown: (label, colour, hatch, extractor). Maximally
# distinct OI hues, each paired with its own hatch.
LINE_BANDS = [
    ("Go code",       OI["blue"],       "",   lambda g: int(g["go_code"]) - int(g.get("go_test") or 0)),
    ("Go tests",      OI["purple"],     ".",  lambda g: int(g.get("go_test") or 0)),
    ("Docs",          OI["green"],      "/",  lambda g: int(g.get("md") or 0)),
    ("YAML",          OI["yellow"],     "x",  lambda g: int(g.get("yaml") or 0)),
    ("Scripts & web", OI["vermillion"], "\\", lambda g: int(g.get("scripts") or 0)),
]
# The same five bands counted in words. Words are the headline denominator: they
# are the unit closest to what a token is, and a reformat cannot move them. The
# honest cost is that a per-word ratio rewards length, so prose trimmed to say the
# same thing in fewer words scores as less output.
WORD_BANDS = [
    ("Go",            OI["blue"],       "",   lambda g: int(g.get("go_words") or 0)),
    ("Docs",          OI["green"],      "/",  lambda g: int(g.get("md_words") or 0)),
    ("YAML",          OI["yellow"],     "x",  lambda g: int(g.get("yaml_words") or 0)),
    ("Scripts & web", OI["vermillion"], "\\", lambda g: int(g.get("scripts_words") or 0)),
]
EST_NOTE = "shaded / hatched = pre-transcript days estimated from the Pro-era per-commit rate"


def load(name):
    with open(os.path.join(DATA, name)) as fh:
        return list(csv.DictReader(fh))


def summary():
    """The committed summary.json — provenance the CSVs don't carry."""
    with open(os.path.join(DATA, "summary.json")) as fh:
        return json.load(fh)


def is_est(r):
    return str(r.get("estimated", "0")) == "1"


def machine_starts():
    """``(date, label)`` for each machine after the first, from its earliest row.

    Derived rather than hardcoded so a third machine marks itself. Estimated
    backfill rows carry no machine (``-``) and are skipped.

    "begins", not "joins": the data shows where a machine's rows start, and can't
    tell a replacement from an addition — silence from the old machine is not
    evidence it retired. Which one it was belongs in the README.
    """
    first = {}
    for r in load("token_metrics.csv"):
        h = r.get("host", "")
        if h in ("", "-"):
            continue
        if h not in first or r["date"] < first[h]:
            first[h] = r["date"]
    order = sorted(first.items(), key=lambda kv: (kv[1], kv[0]))
    return [(dparse(d), f"{h} begins") for h, d in order[1:]]


def leading_model_arrival():
    """``[(date, "<model> arrives")]`` for the model holding the largest share.

    Derived like ``machine_starts``, so the marker follows whichever model has
    taken over rather than naming one in code. Only the leader is marked: every
    model's first day would be six lines of clutter, and the one that matters is
    the one an era is attributed to.
    """
    rows = load("model_daily.csv")
    if not rows:
        return []
    total, first = {}, {}
    for r in rows:
        m = r.get("model", "")
        if m in ("", "Other", "Unknown") or is_est(r):
            continue
        total[m] = total.get(m, 0) + int(r.get("headline") or 0)
        if m not in first or r["date"] < first[m]:
            first[m] = r["date"]
    if not total:
        return []
    lead = max(total, key=total.get)
    return [(dparse(first[lead]), f"{lead} arrives")]


def event_markers():
    """Plan upgrades, the leading model's arrival, and machine starts, in date order.

    Three causes, three styles. They are returned together because a chart that
    draws one has to draw the others: two events a day apart are only separable
    to a reader who can see both, and this project has exactly that pair.
    """
    events = [(PRO_TO_MAX, "Pro → Max 5x", *PLAN_STYLE),
              (MAX_5X_TO_20X, "Max 5x → 20x", *PLAN_STYLE)]
    events += [(d, lbl, *MODEL_STYLE) for d, lbl in leading_model_arrival()]
    events += [(d, lbl, *MACHINE_STYLE) for d, lbl in machine_starts()]
    return sorted(events, key=lambda e: e[0])


def event_label(ax, x, y, label, col, yc="data"):
    """Rotated event label reading upward, clear of the line it annotates.

    ``rotation_mode="anchor"`` with va="bottom" rotates the glyph body to the left
    of the anchor, so the line doesn't strike through the text.
    """
    ax.annotate(label, (x, y), xytext=(-3, 4), textcoords="offset points",
                xycoords=("data", yc), rotation=90, rotation_mode="anchor",
                ha="left", va="bottom", fontsize=9.5, fontweight="bold",
                color=col, zorder=6,
                path_effects=[pe.Stroke(linewidth=2.5, foreground="white"), pe.Normal()])


def reflow_marker(axes, label_ax, y=0.01):
    """Mark the sentence-per-line reflow across ``axes``, labelled on ``label_ax``.

    Only the lines and cost-per-line charts call this. The reformat changed the
    denominator, not the spend, so marking it on a token chart would invite the
    reading that it cost or saved something.
    """
    col, ls = REFLOW_STYLE
    for a in axes:
        a.axvline(DOCS_REFLOW, color=col, ls=ls, lw=1.6, zorder=EVENT_Z)
    event_label(label_ax, DOCS_REFLOW, y, "docs reflow", col, yc="axes fraction")


def rolling_mean(vals, window=7):
    """Centered rolling mean, ``None`` where the full window doesn't fit.

    Centered, not trailing: a trailing mean lags by half its window, which would
    slide every inflection later than the day it happened — fatal on a chart whose
    point is that two events are one day apart. Edges stay ``None`` rather than
    averaging a short window, so the line never ends on a partial figure that
    swings and reads as a trend.
    """
    half = window // 2
    out = []
    for i in range(len(vals)):
        if i < half or i + half >= len(vals):
            out.append(None)
        else:
            out.append(sum(vals[i - half:i + half + 1]) / window)
    return out


def stagger_labels(fig, texts, step):
    """Step a label down until it clears the ones already placed.

    Horizontal event labels share one height, so two events close together on the
    x axis collide, and they get closer every time the timeline lengthens under a
    fixed figure width. Overlap is measured in display space, so a pair that fits
    is left alone at any date spacing.
    """
    fig.canvas.draw()  # a renderer must exist before a text can be measured
    rend = fig.canvas.get_renderer()
    placed = []
    for t in texts:
        for _ in range(len(texts)):
            box = t.get_window_extent(rend)
            if not any(box.overlaps(p) for p in placed):
                break
            x, y = t.get_position()
            t.set_position((x, y - step))
        placed.append(t.get_window_extent(rend))


def save(fig, stem):
    os.makedirs(CHARTS, exist_ok=True)
    fig.savefig(os.path.join(CHARTS, f"{stem}.png"), dpi=160, bbox_inches="tight")
    fig.savefig(os.path.join(CHARTS, f"{stem}@2x.png"), dpi=320, bbox_inches="tight")
    plt.close(fig)


def dparse(s):
    return date.fromisoformat(s)


def darken(hexc, f=0.6):
    """Return a darker shade of a #rrggbb colour — for legible hatches and labels."""
    h = hexc.lstrip("#")
    r, g, b = (int(h[i:i + 2], 16) for i in (0, 2, 4))
    return "#%02x%02x%02x" % (int(r * f), int(g * f), int(b * f))


def shade_estimated(ax, est_dates, dts):
    """Shade the estimated region (through the first measured day) on a date axis."""
    if not est_dates:
        return
    lo = min(dparse(d) for d in est_dates)
    hi = max(dts)
    first_measured = min(d for d in dts if d.isoformat() not in est_dates)
    ax.axvspan(lo, first_measured, color="#999", alpha=0.10, lw=0, zorder=0)


def chart_tokens_by_model():
    rows = load("model_daily.csv")
    days = sorted({r["date"] for r in rows})
    # a day is estimated only if every row for it is (rows are per machine)
    est_dates = {r["date"] for r in rows if is_est(r)} - {r["date"] for r in rows if not is_est(r)}
    models = ["Sonnet 4.6", "Opus 4.7", "Opus 4.8", "Opus 5", "Fable 5", "Haiku 4.5",
              "Other", "Unknown"]
    by = {}
    for r in rows:  # sum each (day, model) across machines
        k = (r["date"], r["model"])
        by[k] = by.get(k, 0) + int(r["headline"])
    xs = list(range(len(days)))
    fig, ax = plt.subplots(figsize=(11, 5.2))
    bottom = [0.0] * len(days)
    containers = []
    drawn = []
    for m in models:
        vals = [by.get((d, m), 0) / 1e6 for d in days]
        if sum(vals) == 0:
            continue
        bc = ax.bar(xs, vals, bottom=bottom, color=MODEL_COLORS[m], width=0.82,
                    edgecolor="white", linewidth=0.4, hatch=MODEL_HATCH[m] or None)
        containers.append(bc)
        drawn.append(m)
        bottom = [b + v for b, v in zip(bottom, vals)]
    # mark the estimated (backfilled) bars: cross-hatch overrides the model hatch.
    for bc in containers:
        for xi, d in enumerate(days):
            if d in est_dates:
                bc.patches[xi].set_hatch("////")
                bc.patches[xi].set_alpha(0.55)
    # clean legend proxies (so the hatched May-16 bar doesn't bleed into the swatches)
    handles = [mpatches.Patch(facecolor=MODEL_COLORS[m], hatch=MODEL_HATCH[m] or None,
                              edgecolor="white", label=m) for m in drawn]
    handles.append(mpatches.Patch(facecolor="#cccccc", hatch="////", edgecolor="white", label="estimated"))
    labels = []
    for ev_date, label, col, ls in event_markers():
        ev = ev_date.isoformat()
        if ev not in days:
            continue
        xi = days.index(ev)
        ax.axvline(xi - 0.5, color=col, ls=ls, lw=1.4)
        # Labels near the right edge read inward so they stay on the axes.
        right = xi > len(days) * 0.78
        labels.append(ax.text(xi + (-0.9 if right else -0.4), max(bottom) * 0.92, label,
                              ha="right" if right else "left",
                              fontsize=10, fontweight="bold", color=col))
    ax.set_title("Daily Claude Code token usage by model", fontsize=14, fontweight="bold", loc="left")
    ax.set_ylabel("tokens / day  (millions)", fontsize=11)
    ax.set_xticks(xs)
    ax.set_xticklabels([dparse(d).strftime("%b %-d") if i % 2 == 0 else "" for i, d in enumerate(days)],
                       rotation=45, ha="right", fontsize=8)
    ax.legend(handles=handles, frameon=False, fontsize=10, ncol=5, loc="upper center",
              bbox_to_anchor=(0.5, -0.18))
    for s in ("top", "right"):
        ax.spines[s].set_visible(False)
    ax.grid(axis="y", alpha=0.25)
    fig.text(0.012, 0.005, "hatched bars (May 16–18) estimated from archived sessions", fontsize=7.5, color="#999")
    fig.tight_layout()
    stagger_labels(fig, labels, max(bottom) * 0.075)  # after layout: it measures the final axes box
    save(fig, "tokens_by_model")


def _token_rows():
    """Token rows summed across machines — one total per date.

    The CSV carries one row per (date, machine); every chart wants the day.
    """
    num = ["input", "output", "cache_creation", "cache_read", "assistant_msgs", "user_msgs"]
    rows = {}
    for r in load("token_metrics.csv"):
        cur = rows.setdefault(r["date"], {"date": r["date"], "estimated": "1",
                                          **{c: 0 for c in num}})
        for c in num:
            cur[c] += int(r[c])
        if not is_est(r):
            cur["estimated"] = "0"  # a day is estimated only if every row is
    return rows, sorted(rows)


def _per_line_series():
    """Shared series for the tokens-vs-lines charts.

    Returns ``(git, dates, xs, ys, cum_on, lines)`` where ``git`` is the git_metrics
    rows by date, ``dates`` the measured days that have authored lines, ``xs`` their
    datetimes, ``ys`` cumulative tokens ÷ words authored, ``cum_on`` cumulative
    headline tokens carried forward onto every date, and ``lines(g)`` the
    all-hand-authored line count for a git row.
    """
    trows, tdays = _token_rows()
    cum, run = {}, 0
    for d in tdays:
        r = trows[d]
        run += int(r["input"]) + int(r["output"]) + int(r["cache_creation"])
        cum[d] = run
    git = {r["date"]: r for r in load("git_metrics.csv")}
    est_dates = {d for d in tdays if is_est(trows[d])}

    def lines(g):  # all hand-authored: Go (code+tests) + Markdown + YAML + scripts/web
        return (int(g["go_code"]) + int(g.get("md") or 0)
                + int(g.get("yaml") or 0) + int(g.get("scripts") or 0))

    all_dates = sorted(set(tdays) | set(git))
    cum_on, last = {}, 0
    for d in all_dates:
        if d in cum:
            last = cum[d]
        cum_on[d] = last
    # Measured days that have authored lines (avoids divide-by-zero on day 1).
    def words(g):
        return int(g.get("words") or 0)

    dates = [d for d in sorted(git) if words(git[d]) > 0 and d not in est_dates]
    xs = [dparse(d) for d in dates]
    ys = [cum_on[d] / words(git[d]) for d in dates]      # the headline ratio
    ys_line = [cum_on[d] / lines(git[d]) for d in dates]  # kept for lines_vs_words
    return git, dates, xs, ys, cum_on, lines, words, ys_line


def chart_tokens_per_word():
    """Two panels: cost-per-word ratio on top, the word denominator decomposed below.

    Top: total headline tokens to date ÷ words authored that day — a single ratio
    that climbs as the project matures (each line costs more once the easy
    scaffolding is done and work shifts to logic, tests, review, debugging).
    Bottom: the denominator itself as a stacked area — Go code, Go tests, Markdown
    docs, hand-written YAML, scripts & web — so "a line" is shown, not described.
    Estimated (pre-transcript) days are excluded so the ratio is measured-only.
    """
    git, dates, xs, ys, _, _, _, _ = _per_line_series()
    gold = GOLD

    # Weekly guide dates = project day 7, 14, ... (from the first commit), so the
    # marks line up with the "Day 7 / 22 / 35" milestones.
    start = min(dparse(d) for d in git)
    val_at = dict(zip(xs, ys))
    week_dates, k = [], 1
    while start + timedelta(days=7 * k) <= xs[-1]:
        wd = start + timedelta(days=7 * k)
        if wd >= xs[0]:
            week_dates.append(wd)
        k += 1

    def _val_on(wd):  # cost/line at the latest measured day on or before wd
        cand = [x for x in xs if x <= wd]
        return val_at[max(cand)] if cand else None

    fig, (ax, axb) = plt.subplots(
        2, 1, figsize=(11, 8.4), sharex=True,
        gridspec_kw=dict(height_ratios=[1, 1], hspace=0.13))

    # --- top: the cost-per-line ratio ---
    ax.plot(xs, ys, color=gold, lw=3.2, solid_capstyle="round", zorder=3)
    ax.fill_between(xs, ys, 0, color=gold, alpha=0.10, zorder=2)
    ax.annotate(f"{ys[-1]:,.0f} tokens / word", (xs[-1], ys[-1]), xytext=(-8, 14),
                textcoords="offset points", ha="right", fontsize=13, fontweight="bold",
                color="#8A6216", path_effects=HALO, zorder=LABEL_Z)
    ax.annotate(f"{ys[0]:,.0f} / word", (xs[0], ys[0]), xytext=(6, -15),
                textcoords="offset points", ha="left", fontsize=10.5, color="#8A6216",
                path_effects=HALO, zorder=LABEL_Z)
    ax.set_title("Each word costs more tokens as the project matures",
                 fontsize=14, fontweight="bold", loc="left")
    ax.set_ylabel("cumulative tokens ÷ word", fontsize=11)
    ax.set_ylim(0, max(ys) * 1.12)
    ax.yaxis.set_major_formatter(plt.FuncFormatter(lambda v, _: f"{v:,.0f}"))
    ax.grid(axis="y", alpha=0.22)
    for s in ("top", "right"):
        ax.spines[s].set_visible(False)

    # --- bottom: the denominator, decomposed (its height = the divisor above) ---
    # Muted fills + a bold boundary line per band, so the composition reads from
    # crisp colour-coded lines rather than busy texture. Faint hatch stays as a
    # secondary, colourblind-safe cue.
    stacks = [[fn(git[d]) / 1e3 for d in dates] for _, _, _, fn in WORD_BANDS]
    polys = axb.stackplot(xs, *stacks, colors=[c for _, c, _, _ in WORD_BANDS],
                          alpha=0.28, zorder=2)
    for poly, (_, col, hatch, _) in zip(polys, WORD_BANDS):
        poly.set_hatch(hatch)
        poly.set_edgecolor(darken(col))  # hatch draws in the edge colour
        poly.set_linewidth(0.0)
    cumtop = np.cumsum(np.array(stacks), axis=0)
    running = 0.0
    for i, (label, col, _, _) in enumerate(WORD_BANDS):
        axb.plot(xs, cumtop[i], color=darken(col), lw=2.4, solid_capstyle="round", zorder=4)
        s = stacks[i]
        mid = running + s[-1] / 2
        running += s[-1]
        axb.annotate(f"{label}  {s[-1]:.0f}k", (xs[-1], mid), xytext=(7, 0),
                     textcoords="offset points", va="center", fontsize=9.5,
                     fontweight="bold", color=darken(col))
    axb.set_ylabel("words authored (thousands)", fontsize=11)
    axb.set_ylim(0, running * 1.05)
    axb.set_xlim(xs[0], xs[-1])
    axb.xaxis.set_major_formatter(mdates.DateFormatter("%b %-d"))
    axb.xaxis.set_major_locator(mdates.DayLocator(interval=2))
    plt.setp(axb.get_xticklabels(), rotation=45, ha="right", fontsize=8.5)
    axb.grid(axis="y", alpha=0.22)
    for s in ("top", "right"):
        axb.spines[s].set_visible(False)

    # Day (subtle, minor) and week (prominent, dashed) vertical guides on both
    # panels; the weekly cost/line value called out where each week line crosses.
    for a in (ax, axb):
        a.xaxis.set_minor_locator(mdates.DayLocator(interval=1))
        a.grid(which="minor", axis="x", color="#000000", alpha=0.05, lw=0.5)
        for wd in week_dates:
            a.axvline(wd, color="#9AA0A6", ls=(0, (4, 3)), lw=1.0, alpha=0.7, zorder=1)
    for wd in week_dates:
        v = _val_on(wd)
        if v is None or wd == xs[-1]:  # the final point already carries a big label
            continue
        ax.plot([wd], [v], "o", color=gold, ms=5, zorder=6)
        ax.annotate(f"{v:,.0f}", (wd, v), xytext=(0, 9), textcoords="offset points",
                    ha="center", fontsize=8.5, fontweight="bold", color=gold,
                    path_effects=[pe.Stroke(linewidth=2.5, foreground="white"), pe.Normal()],
                    zorder=LABEL_Z)

    fig.text(0.012, 0.008, "generated CRD YAML, binaries & lockfiles excluded · tokens = input + output + cache writes",
             fontsize=7.5, color="#999")
    save(fig, "tokens_per_word")  # save() crops with bbox_inches="tight"


def chart_tokens_vs_words():
    """Log-scale magnitude: total tokens far above total words authored.

    Log y so both the ~115M tokens and the ~60k lines are visible at once (a linear
    axis would crush the lines to an invisible sliver). Two curves — tokens up top,
    lines below — and the gold-shaded gap between them is the cost per line, since on
    a log axis a ratio is a vertical gap. No composition here: the undistorted
    breakdown of the lines lives in the tokens-per-line chart.
    """
    git, dates, xs, ys, cum_on, lines, words, ys_line = _per_line_series()
    tok = [cum_on[d] for d in dates]
    total = [words(git[d]) for d in dates]

    fig, ax = plt.subplots(figsize=(11, 6.2))
    ax.set_yscale("log")
    ax.set_ylim(1e3, max(tok) * 1.2)  # follows the data; a fixed top clips as it grows
    ax.set_xlim(xs[0], xs[-1])
    # The cost per line is the gap between the two curves — shade it gold.
    ax.fill_between(xs, total, tok, color=OI["orange"], alpha=0.12, lw=0, zorder=1)
    ax.plot(xs, tok, color=OI["blue"], lw=3.2, ls="-", solid_capstyle="round", zorder=4,
            path_effects=[pe.Stroke(linewidth=5.5, foreground="white"), pe.Normal()],
            label="Tokens spent (cumulative)")
    ax.plot(xs, total, color=OI["green"], lw=3.2, ls=(0, (6, 2)), zorder=4,
            path_effects=[pe.Stroke(linewidth=5.5, foreground="white"), pe.Normal()],
            label="Words authored (cumulative)")

    ax.annotate(f"{tok[-1] / 1e6:,.0f}M tokens", (xs[-1], tok[-1]), xytext=(-8, 10),
                textcoords="offset points", ha="right", fontsize=12.5, fontweight="bold",
                color=OI["blue"], path_effects=HALO, zorder=LABEL_Z)
    ax.annotate(f"{total[-1] / 1e3:,.0f}k words", (xs[-1], total[-1]), xytext=(-8, -14),
                textcoords="offset points", ha="right", fontsize=12.5, fontweight="bold",
                color="#1B7A5A", path_effects=HALO, zorder=LABEL_Z)
    gap_mid = (total[-1] * tok[-1]) ** 0.5  # geometric mid of the gap, on a log axis
    ax.annotate(f"≈ {ys[-1]:,.0f} tokens / word", (xs[-1], gap_mid), xytext=(-10, 0),
                textcoords="offset points", ha="right", fontsize=13, fontweight="bold",
                color=GOLD, path_effects=HALO, zorder=LABEL_Z)

    ax.set_title("Tokens spent vs. words authored (log scale)",
                 fontsize=14, fontweight="bold", loc="left")
    ax.set_ylabel("cumulative count (log scale)", fontsize=11)
    ax.xaxis.set_major_formatter(mdates.DateFormatter("%b %-d"))
    ax.xaxis.set_major_locator(mdates.DayLocator(interval=2))
    plt.setp(ax.get_xticklabels(), rotation=45, ha="right", fontsize=8.5)
    ax.legend(loc="upper center", bbox_to_anchor=(0.5, -0.12), ncol=2, frameon=False, fontsize=10)
    ax.spines["top"].set_visible(False)
    ax.spines["right"].set_visible(False)
    fig.text(0.012, 0.008, "words authored = Go + Markdown + hand-written YAML + scripts & web, comments included · tokens = input + output + cache writes",
             fontsize=7.5, color="#999")
    save(fig, "tokens_vs_words")


def chart_overview():
    """All three tokens/lines views in one shared-x figure (top → bottom):

    1. magnitude — tokens vs words authored on a log axis, gap = cost/word;
    2. breakdown — the lines composition as a muted stacked area with bold edges;
    3. cost — cumulative tokens ÷ word over time ("what those words cost"), with
       the value at each weekly guide.
    """
    git, dates, xs, ys, cum_on, lines, words, ys_line = _per_line_series()
    gold = GOLD
    tok = [cum_on[d] for d in dates]
    total = [words(git[d]) for d in dates]

    start = min(dparse(d) for d in git)
    val_at = dict(zip(xs, ys))
    week_dates, k = [], 1
    while start + timedelta(days=7 * k) <= xs[-1]:
        wd = start + timedelta(days=7 * k)
        if wd >= xs[0]:
            week_dates.append(wd)
        k += 1

    def _val_on(wd):
        cand = [x for x in xs if x <= wd]
        return val_at[max(cand)] if cand else None

    fig, (a1, a2, a3) = plt.subplots(
        3, 1, figsize=(11, 12.5), sharex=True,
        gridspec_kw=dict(height_ratios=[1, 1, 1], hspace=0.16))

    # --- panel 1: magnitude (log) ---
    a1.set_yscale("log")
    a1.set_ylim(1e3, max(tok) * 1.2)  # follows the data; a fixed top clips as it grows
    a1.fill_between(xs, total, tok, color=OI["orange"], alpha=0.12, lw=0, zorder=1)
    a1.plot(xs, tok, color=OI["blue"], lw=3.0, solid_capstyle="round", zorder=4,
            path_effects=[pe.Stroke(linewidth=5, foreground="white"), pe.Normal()])
    a1.plot(xs, total, color=OI["green"], lw=3.0, ls=(0, (6, 2)), zorder=4,
            path_effects=[pe.Stroke(linewidth=5, foreground="white"), pe.Normal()])
    a1.annotate(f"{tok[-1] / 1e6:,.0f}M tokens", (xs[-1], tok[-1]), xytext=(-8, 9),
                textcoords="offset points", ha="right", fontsize=12, fontweight="bold",
                color=OI["blue"], path_effects=HALO, zorder=LABEL_Z)
    a1.annotate(f"{total[-1] / 1e3:,.0f}k words", (xs[-1], total[-1]), xytext=(-8, -13),
                textcoords="offset points", ha="right", fontsize=12, fontweight="bold",
                color="#1B7A5A", path_effects=HALO, zorder=LABEL_Z)
    a1.annotate(f"≈ {ys[-1]:,.0f} tokens / word", (xs[-1], (total[-1] * tok[-1]) ** 0.5),
                xytext=(-10, 0), textcoords="offset points", ha="right", fontsize=12.5,
                fontweight="bold", color=gold, path_effects=HALO, zorder=LABEL_Z)
    a1.set_ylabel("count (log scale)", fontsize=11)
    a1.set_title("Tokens spent vs. words authored", fontsize=12.5, fontweight="bold", loc="left")
    a1.grid(axis="y", which="both", alpha=0.16)

    # --- panel 2: composition ---
    stacks = [[fn(git[d]) / 1e3 for d in dates] for _, _, _, fn in WORD_BANDS]
    polys = a2.stackplot(xs, *stacks, colors=[c for _, c, _, _ in WORD_BANDS], alpha=0.28, zorder=2)
    for poly, (_, col, hatch, _) in zip(polys, WORD_BANDS):
        poly.set_hatch(hatch)
        poly.set_edgecolor(darken(col))
        poly.set_linewidth(0.0)
    cumtop = np.cumsum(np.array(stacks), axis=0)
    running = 0.0
    for i, (label, col, _, _) in enumerate(WORD_BANDS):
        a2.plot(xs, cumtop[i], color=darken(col), lw=2.2, solid_capstyle="round", zorder=4)
        s = stacks[i]
        mid = running + s[-1] / 2
        running += s[-1]
        a2.annotate(f"{label}  {s[-1]:.0f}k", (xs[-1], mid), xytext=(7, 0),
                    textcoords="offset points", va="center", fontsize=9, fontweight="bold",
                    color=darken(col))
    a2.set_ylim(0, running * 1.05)
    a2.set_ylabel("lines authored (thousands)", fontsize=11)
    a2.set_title("What those words are", fontsize=12.5, fontweight="bold", loc="left")
    a2.grid(axis="y", alpha=0.22)

    # --- panel 3: what those lines cost (tokens per line), with weekly values ---
    a3.plot(xs, ys, color=gold, lw=3.0, solid_capstyle="round", zorder=3)
    a3.fill_between(xs, ys, 0, color=gold, alpha=0.10, zorder=2)
    a3.set_ylim(0, max(ys) * 1.16)
    a3.yaxis.set_major_formatter(plt.FuncFormatter(lambda v, _: f"{v:,.0f}"))
    a3.annotate(f"{ys[-1]:,.0f} tokens / word", (xs[-1], ys[-1]), xytext=(-8, 12),
                textcoords="offset points", ha="right", fontsize=12.5, fontweight="bold",
                color=gold, path_effects=HALO, zorder=LABEL_Z)
    a3.annotate(f"{ys[0]:,.0f}", (xs[0], ys[0]), xytext=(4, -13), textcoords="offset points",
                ha="left", fontsize=10, fontweight="bold", color=gold,
                path_effects=HALO, zorder=LABEL_Z)
    for wd in week_dates:
        v = _val_on(wd)
        if v is None or wd == xs[-1]:
            continue
        a3.plot([wd], [v], "o", color=gold, ms=5, zorder=6)
        a3.annotate(f"{v:,.0f}", (wd, v), xytext=(0, 9), textcoords="offset points",
                    ha="center", fontsize=8.5, fontweight="bold", color=gold,
                    path_effects=[pe.Stroke(linewidth=2.5, foreground="white"), pe.Normal()],
                    zorder=LABEL_Z)
    a3.set_ylabel("cumulative tokens ÷ word", fontsize=11)
    a3.set_title("What those words cost in tokens", fontsize=12.5, fontweight="bold", loc="left")
    a3.grid(axis="y", alpha=0.22)
    a3.xaxis.set_major_formatter(mdates.DateFormatter("%b %-d"))
    a3.xaxis.set_major_locator(mdates.DayLocator(interval=2))
    plt.setp(a3.get_xticklabels(), rotation=45, ha="right", fontsize=8.5)

    # shared x + day/week guides on all panels
    for a in (a1, a2, a3):
        a.set_xlim(xs[0], xs[-1])
        a.xaxis.set_minor_locator(mdates.DayLocator(interval=1))
        a.grid(which="minor", axis="x", color="#000000", alpha=0.05, lw=0.5)
        for wd in week_dates:
            a.axvline(wd, color="#9AA0A6", ls=(0, (4, 3)), lw=1.0, alpha=0.7, zorder=1)
        for s in ("top", "right"):
            a.spines[s].set_visible(False)
    # Event lines span all three panels; the label sits in the empty band along the
    # bottom of panel 1, below the lines curve, which is clear at every event date.
    for ev_date, label, col, ls in event_markers():
        if not xs[0] <= ev_date <= xs[-1]:
            continue
        for a in (a1, a2, a3):
            a.axvline(ev_date, color=col, ls=ls, lw=1.4, zorder=EVENT_Z)
        event_label(a1, ev_date, 0.01, label, col, yc="axes fraction")
    for a in (a1, a2):
        plt.setp(a.get_xticklabels(), visible=False)

    fig.text(0.012, 0.006, "generated CRD YAML, binaries & lockfiles excluded · tokens = input + output + cache writes",
             fontsize=7.5, color="#999")
    save(fig, "tokens_overview")


def chart_token_anatomy():
    trows, tdays = _token_rows()
    days = [dparse(d) for d in tdays]
    est_dates = {d for d in tdays if is_est(trows[d])}

    def arr(k):
        a = np.array([int(trows[d][k]) for d in tdays], float)
        a[a <= 0] = np.nan
        return a

    # (key, label, colour, linestyle, marker) — each series differs in all three.
    spec = [
        ("cache_read", "Cache read", OI["blue"], "-", "o"),
        ("cache_creation", "Cache creation", OI["skyblue"], (0, (6, 2)), "s"),
        ("output", "Output", OI["orange"], (0, (1, 1)), "^"),
        ("input", "Input (fresh)", OI["vermillion"], (0, (3, 1, 1, 1)), "D"),
    ]
    totals = {k: sum(int(r[k]) for r in trows.values()) for k, *_ in spec}
    fig, ax = plt.subplots(figsize=(11, 5.6))
    shade_estimated(ax, est_dates, days)
    for k, lbl, col, ls, mk in spec:
        ax.plot(days, arr(k), color=col, lw=2.4, ls=ls, marker=mk, ms=3.5,
                label=f"{lbl}  ({totals[k] / 1e6:,.0f}M total)")
        ax.fill_between(days, arr(k), 0.1, color=col, alpha=0.06)
    ax.set_yscale("log")
    # Follows the data — a fixed top clipped the cache-read peak once volume grew.
    ax.set_ylim(1e3, max(np.nanmax(arr(k)) for k, *_ in spec) * 1.5)
    ax.set_ylabel("tokens / day  (log scale)", fontsize=11)
    ax.set_title("Anatomy of token usage", fontsize=14, fontweight="bold", loc="left")
    ax.xaxis.set_major_formatter(mdates.DateFormatter("%b %-d"))
    ax.xaxis.set_major_locator(mdates.DayLocator(interval=2))
    plt.setp(ax.get_xticklabels(), rotation=45, ha="right", fontsize=8.5)
    ax.legend(frameon=False, fontsize=10, loc="lower right", ncol=2)
    for s in ("top", "right"):
        ax.spines[s].set_visible(False)
    ax.grid(alpha=0.22, which="both")
    fig.text(0.012, 0.01, "shaded band (May 16–18) estimated from archived sessions", fontsize=7.5, color="#999")
    fig.tight_layout()
    save(fig, "token_anatomy")


def chart_cumulative_cache():
    trows, tdays = _token_rows()
    days = [dparse(d) for d in tdays]
    est_dates = {d for d in tdays if is_est(trows[d])}
    cc = np.cumsum([int(trows[d]["cache_creation"]) for d in tdays]) / 1e9
    cr = np.cumsum([int(trows[d]["cache_read"]) for d in tdays]) / 1e9
    fig, ax = plt.subplots(figsize=(11, 5.6))
    shade_estimated(ax, est_dates, days)
    writes = ax.fill_between(days, 0, cc, color=OI["orange"], label="cache writes  (writes once)", zorder=2)
    reads = ax.fill_between(days, cc, cc + cr, color=OI["blue"], label="cache reads  (replayed context)", zorder=2)
    writes.set_hatch("..")
    writes.set_edgecolor(darken(OI["orange"]))
    reads.set_hatch("")
    ax.plot(days, cc + cr, color="#0A2647", lw=1.5, zorder=3)
    ax.annotate(f"{cr[-1]:.2f}B", (days[-1], (cc + cr)[-1]), xytext=(10, -2),
                textcoords="offset points", va="center", fontsize=15, fontweight="bold", color=OI["blue"])
    ax.annotate(f"cache writes: {cc[-1] * 1000:.0f}M", (days[-1], cc[-1] * 0.5), xytext=(-12, 80),
                textcoords="offset points", ha="right", fontsize=11, color="#5A3A00", fontweight="bold",
                arrowprops=dict(arrowstyle="-|>", color="#5A3A00", lw=1.3))
    ax.set_title("Cumulative cache traffic", fontsize=14, fontweight="bold", loc="left")
    ax.set_ylabel("cumulative tokens  (billions)", fontsize=11)
    ax.xaxis.set_major_formatter(mdates.DateFormatter("%b %-d"))
    ax.xaxis.set_major_locator(mdates.DayLocator(interval=2))
    plt.setp(ax.get_xticklabels(), rotation=45, ha="right", fontsize=8.5)
    ax.set_xlim(days[0], days[-1])
    ax.set_ylim(0, (cc + cr)[-1] * 1.10)
    # Event lines, labelled in the empty wedge above the curve.
    total = cc + cr
    for ev_date, label, col, ls in event_markers():
        if not days[0] <= ev_date <= days[-1]:
            continue
        ax.axvline(ev_date, color=col, ls=ls, lw=1.4, zorder=4)
        y = total[days.index(ev_date)] if ev_date in days else 0.0
        event_label(ax, ev_date, y + total[-1] * 0.03, label, col)
    ax.legend(frameon=False, fontsize=10.5, loc="upper left")
    for s in ("top", "right"):
        ax.spines[s].set_visible(False)
    ax.grid(axis="y", alpha=0.22)
    fig.text(0.012, 0.01, "shaded band (May 16–18) estimated from archived sessions", fontsize=7.5, color="#999")
    fig.tight_layout()
    save(fig, "cumulative_cache")


def chart_parallel_sessions():
    """Concurrency over time spent, on its own shorter timeline.

    Peak is the dramatic number but a burst of it lasts one bucket; the mean is
    what multiplies a day's output, so both are drawn on one axis. Below, the gap
    between session-hours and wall-clock hours *is* that mean.

    Its own timeline: concurrency needs session-level transcripts, so the series
    starts where those survive rather than at the first project day. Drawing it
    on the project timeline would show 71 empty days and read as idleness.
    """
    rows = load("session_metrics.csv")
    if not rows:
        return
    sess_meta = summary().get("sessions", {})
    per_hour = 60 / sess_meta.get("bucket_minutes", 10)
    by_day = {}
    for r in rows:  # combine machines
        d = by_day.setdefault(r["date"], {"peak": 0, "active": 0, "buckets": 0})
        # Peak is a max, not a sum: two machines' peaks need not coincide in time.
        d["peak"] = max(d["peak"], int(r["peak_concurrent"]))
        d["active"] += int(r["active_buckets"])
        d["buckets"] += int(r["session_buckets"])
    days = sorted(by_day)
    xs = list(range(len(days)))
    peak = [by_day[d]["peak"] for d in days]
    mean = [by_day[d]["buckets"] / by_day[d]["active"] if by_day[d]["active"] else 0 for d in days]
    wall = [by_day[d]["active"] / per_hour for d in days]
    shrs = [by_day[d]["buckets"] / per_hour for d in days]

    fig, (a1, a2) = plt.subplots(2, 1, figsize=(11, 7.8), sharex=True,
                                 gridspec_kw=dict(height_ratios=[1.2, 1], hspace=0.30))
    halo = [pe.Stroke(linewidth=2.5, foreground="white"), pe.Normal()]

    a1.bar(xs, peak, color=OI["green"], width=0.62, edgecolor="white", linewidth=0.5,
           alpha=0.45, zorder=2)
    a1.plot(xs, mean, color=darken(OI["green"]), lw=2.6, marker="o", ms=5, zorder=4,
            path_effects=halo)
    for x, v in zip(xs, mean):
        a1.annotate(f"{v:.1f}", (x, v), xytext=(0, 7), textcoords="offset points", ha="center",
                    fontsize=9, fontweight="bold", color=darken(OI["green"]), zorder=6,
                    path_effects=halo)
    a1.set_title("Parallel Claude Code sessions", fontsize=14, fontweight="bold", loc="left")
    a1.set_ylabel("sessions running at once", fontsize=11)
    a1.set_ylim(0, max(peak) * 1.22)
    a1.legend(handles=[Line2D([], [], color=darken(OI["green"]), lw=2.6, marker="o", ms=5,
                              label="mean concurrent  (what multiplies a day's output)"),
                       mpatches.Patch(facecolor=OI["green"], alpha=0.45, edgecolor="white",
                                      label="peak concurrent  (one bucket's burst)")],
              frameon=False, fontsize=10, loc="upper left", ncol=2)

    a2.fill_between(xs, 0, shrs, color=OI["skyblue"], alpha=0.40, zorder=1,
                    label="session-hours  (summed over concurrent sessions)")
    a2.plot(xs, shrs, color=OI["blue"], lw=2.4, marker="s", ms=4, zorder=3, path_effects=halo)
    a2.fill_between(xs, 0, wall, color=OI["orange"], alpha=0.55, zorder=2)
    a2.plot(xs, wall, color=darken(OI["orange"]), lw=2.2, marker="o", ms=4, zorder=4,
            path_effects=halo)
    a2.set_ylabel("hours", fontsize=11)
    a2.set_title(f"Time on Claude each day — {sum(wall):.0f}h elapsed, "
                 f"{sum(shrs):.0f}h of session-time ({sum(shrs) / sum(wall):.1f}×)",
                 fontsize=12, fontweight="bold", loc="left")
    a2.set_ylim(0, max(shrs) * 1.22)
    a2.legend(handles=[mpatches.Patch(facecolor=OI["skyblue"], alpha=0.40,
                                      label="session-hours (summed over concurrent sessions)"),
                       mpatches.Patch(facecolor=OI["orange"], alpha=0.55,
                                      label="hours using Claude (wall-clock)")],
              frameon=False, fontsize=9.5, loc="upper left", ncol=2)

    for a in (a1, a2):
        a.set_xlim(-0.6, len(days) - 0.4)
        a.grid(axis="y", alpha=0.22)
        for s in ("top", "right"):
            a.spines[s].set_visible(False)
    a2.set_xticks(xs)
    a2.set_xticklabels([dparse(d).strftime("%b %-d") for d in days], rotation=45, ha="right",
                       fontsize=9)
    fig.text(0.012, 0.01,
             "session-level data survives only from " + days[0] +
             " — earlier transcripts were not retained, so this series cannot be backfilled",
             fontsize=7.5, color="#999")
    # No tight_layout: it warns on this figure and the explicit hspace already
    # spaces the panels. save() trims the margins with bbox_inches="tight".
    save(fig, "parallel_sessions")


def pr_workflow_start(git):
    """First date after which every commit is a PR squash, or ``None``.

    Derived rather than hardcoded, the way ``machine_starts`` is: a project that
    always used PRs marks nothing, and one that switches later marks itself. The
    run length keeps a single quiet day from being read as the switch.
    """
    days = sorted(git)
    run, first = 0, None
    for a, b in zip(days, days[1:]):
        dc = int(git[b]["commits"]) - int(git[a]["commits"])
        dp = int(git[b].get("prs") or 0) - int(git[a].get("prs") or 0)
        if dc <= 0:
            continue
        if dp == dc:
            run += 1
            if run == 1:
                first = b
            if run >= 5:
                return dparse(first)
        else:
            run = 0
    return None


def chart_velocity():
    """Work shipped over time, on proxies a reformat and a workflow change can't move.

    The lines series carries the 2026-08-09 reflow, and commits change units
    mid-project when a repo adopts PR squash-merges, so neither tells a velocity
    story on its own. These four do — with the two methodology steps drawn where
    they fall rather than smoothed over.

    Panel 4 is when work *landed*, not hours worked: sessions sometimes run
    unattended and merges get cleared in bulk, so the spread of the day it shows
    belongs to the system, not to anyone's presence.
    """
    git = {r["date"]: r for r in load("git_metrics.csv")}
    if not git or "prs" not in next(iter(git.values())):
        return
    days = sorted(git)

    def delta(col):
        out, prev = {}, 0
        for d in days:
            cur = int(git[d].get(col) or 0)
            out[d] = max(0, cur - prev)
            prev = cur
        return out

    d_prs, d_tests, d_queue, d_commits = (delta(c) for c in ("prs", "tests", "queue_closed", "commits"))

    # Per day, matching every other chart here. Weekly bars smooth the series into
    # something easier to read and lose what the reader needs: a day is the unit
    # the events are dated in, and a bin wide enough to hold two of them hides that
    # Opus 5 and mac-2 are a day apart.
    hours = [int(git[d].get("active_hours") or 0) for d in days]
    dxs = [dparse(d) for d in days]
    per_hour = [d_commits[d] / h if h else 0 for d, h in zip(days, hours)]

    fig, axes = plt.subplots(4, 1, figsize=(11, 13.5), sharex=True,
                             gridspec_kw=dict(height_ratios=[1, 1, 1, 1.15], hspace=0.22))
    a1, a2, a3, a4 = axes

    # Where each series starts meaning what its axis says. The trend line is
    # suppressed before it: a mean spanning the boundary drags a real trend toward
    # a period the series could not measure.
    pr_start = pr_workflow_start(git)
    backlog_start = next((dparse(d) for d in days
                          if int(git[d].get("queue_closed") or 0) > 0), None)
    trend_from = {a1: pr_start, a3: backlog_start}

    panels = [
        (a1, d_prs, OI["blue"], "PRs merged", "Pull requests merged per day"),
        (a2, d_tests, OI["purple"], "tests added", "Tests added per day"),
        (a3, d_queue, OI["vermillion"], "rows closed", "Backlog rows closed per day"),
    ]
    for ax, series, col, ylab, title in panels:
        raw = [series[d] for d in days]
        ax.bar(dxs, raw, width=0.72, color=col, alpha=0.5,
               edgecolor="white", linewidth=0.4, zorder=3)
        # Daily bars carry the truth; the 7-day centered mean is what makes an
        # inflection legible, which the bars alone are too noisy to show.
        trend = rolling_mean(raw)
        since = trend_from.get(ax)
        if since:
            trend = [None if d < since else v for d, v in zip(dxs, trend)]
        # No per-panel legend: all four panels use the one encoding and the footnote
        # states it, while a legend box in the corner lands on an event label.
        ax.plot([x for x, v in zip(dxs, trend) if v is not None],
                [v for v in trend if v is not None],
                color=darken(col), lw=2.6, zorder=5, path_effects=HALO)
        ax.set_ylabel(ylab, fontsize=10.5)
        ax.set_title(title, fontsize=12.5, fontweight="bold", loc="left")
        ax.grid(axis="y", alpha=0.22)

    # Panel 4: when the work landed, plus how densely it landed in those hours.
    a4.bar(dxs, hours, width=0.72, color=OI["green"], alpha=0.38, edgecolor="white",
           linewidth=0.4, zorder=2)
    h_trend = rolling_mean(hours)
    a4.plot([x for x, v in zip(dxs, h_trend) if v is not None],
            [v for v in h_trend if v is not None],
            color=darken(OI["green"]), lw=2.6, zorder=5, path_effects=HALO)
    a4b = a4.twinx()
    # The ratio was the noisiest series on the figure and is derived anyway; the
    # bars below it carry the daily truth, so only its trend is drawn.
    #
    # Suppressed before the PR-workflow switch for the same reason panel 1 is: its
    # numerator is `commits`, whose unit changes there. Drawn across the boundary
    # it runs high through May on raw commits and drops as they become squashed
    # PRs, which reads as intensity falling when nothing about the work changed.
    # The hours it divides by are unaffected, so only this line is cut.
    r_trend = rolling_mean(per_hour)
    if pr_start:
        r_trend = [None if d < pr_start else v for d, v in zip(dxs, r_trend)]
    a4b.plot([x for x, v in zip(dxs, r_trend) if v is not None],
             [v for v in r_trend if v is not None],
             color=OI["skyblue"], lw=2.4, ls=(0, (6, 2)), zorder=4, path_effects=HALO)
    a4.set_ylabel("hours with a commit", fontsize=10.5)
    a4b.set_ylabel("commits / hour", fontsize=10.5, color=darken(OI["skyblue"]))
    a4.set_title("When work landed — not hours worked (sessions sometimes run unattended)",
                 fontsize=12.5, fontweight="bold", loc="left")
    a4.grid(axis="y", alpha=0.22)
    for s in ("top",):
        a4b.spines[s].set_visible(False)

    # Methodology steps: a change in what the series counts, not in what was done.
    # Styled like the reflow marker for that reason, and shading the region where
    # the series cannot mean what its axis says.
    mcol, mls = REFLOW_STYLE
    for ax, since, label in ((a1, pr_start, "PR workflow begins"),
                             (a3, backlog_start, "backlog begins")):
        if since is None:
            continue
        ax.axvspan(dxs[0], since, color="#000000", alpha=0.05, zorder=1)
        ax.axvline(since, color=mcol, ls=mls, lw=1.6, zorder=EVENT_Z)
        event_label(ax, since, 0.04, label, mcol, yc="axes fraction")

    for ax in axes:
        ax.set_xlim(dxs[0], dxs[-1])
        for s in ("top", "right"):
            ax.spines[s].set_visible(False)
        for ev_date, _, col, ls in event_markers():
            if dxs[0] <= ev_date <= dxs[-1]:
                ax.axvline(ev_date, color=col, ls=ls, lw=1.4, zorder=EVENT_Z)
    a4.spines["right"].set_visible(False)
    # Event labels on panel 1 only, staggered so the Opus 5 / mac-2 pair — one day
    # apart, and the whole point of drawing both — stays readable.
    labels = [event_label(a1, ev, 0.55, lbl, col, yc="axes fraction")
              for ev, lbl, col, ls in event_markers() if dxs[0] <= ev <= dxs[-1]]
    stagger_labels(fig, [t for t in labels if t is not None], 0.11)

    a4.xaxis.set_major_formatter(mdates.DateFormatter("%b %-d"))
    a4.xaxis.set_major_locator(mdates.DayLocator(interval=4))
    plt.setp(a4.get_xticklabels(), rotation=45, ha="right", fontsize=8.5)
    # One figure-level key rather than four inline ones: every panel shares the
    # encoding, and a legend box inside any of them lands on an event label.
    fig.legend(handles=[
        mpatches.Patch(facecolor="#8A8A8A", alpha=0.5, label="daily value"),
        Line2D([], [], color="#333333", lw=2.6, label="7-day centered mean"),
        Line2D([], [], color=OI["skyblue"], lw=2.4, ls=(0, (6, 2)),
               label="commits per such hour, 7-day mean (bottom panel, right axis)"),
    ], loc="upper center", bbox_to_anchor=(0.5, 0.997), ncol=3, frameon=False, fontsize=9.5)
    fig.subplots_adjust(top=0.958)
    fig.text(0.012, 0.006,
             "bars are daily · lines are a 7-day centered mean, stopping 3 days short at each end "
             "rather than averaging a partial window · shaded = the series cannot mean what its axis "
             "says yet · Opus 5 and mac-2 land one day apart, so this data cannot separate model from machine",
             fontsize=7.5, color="#999")
    save(fig, "velocity")


def chart_lines_vs_words():
    """The same corpus counted two ways, so a reformat is visible as a reformat.

    A rewrap moves every line count that spans a paragraph and leaves the words
    alone, so drawing both puts the 2026-08-09 reflow where it belongs: a cliff in
    one series and nothing in the other.

    Log y, absolute counts, the same choice `chart_tokens_vs_lines` makes: words
    outnumber lines by an order of magnitude, and on a linear axis the smaller
    series is a sliver. Indexing both to a common baseline was worse still — it
    reads as magnitude and is not, so two series 17x apart drew on top of each
    other. On a log axis the vertical gap *is* the ratio, which is the mechanism
    here: sentence-per-line put the same words on fewer lines, so the gap widens
    at the reflow rather than either curve behaving strangely.
    """
    git, dates, xs, ys, cum_on, lines, words, ys_line = _per_line_series()
    if not dates or "words" not in git[dates[0]]:
        return

    md_l = [int(git[d].get("md") or 0) for d in dates]
    md_w = [int(git[d].get("md_words") or 0) for d in dates]
    per_word = ys          # the headline ratio
    per_line = ys_line     # what it replaced

    fig, (a1, a2) = plt.subplots(2, 1, figsize=(11, 8.4), sharex=True,
                                 gridspec_kw=dict(height_ratios=[1, 1], hspace=0.22))

    a1.set_yscale("log")
    a1.fill_between(xs, md_l, md_w, color=GOLD, alpha=0.12, lw=0, zorder=1)
    a1.plot(xs, md_w, color=OI["blue"], lw=2.8, ls=(0, (6, 2)), zorder=4,
            path_effects=HALO, label="Docs, in words")
    a1.plot(xs, md_l, color=OI["green"], lw=2.8, zorder=4, path_effects=HALO,
            label="Docs, in lines")
    a1.set_ylabel("count (log scale)", fontsize=10.5)
    a1.set_title("The same docs, counted in lines and in words", fontsize=12.5,
                 fontweight="bold", loc="left")
    a1.annotate(f"{md_w[-1] / md_l[-1]:.1f} words / line", (xs[-1], (md_l[-1] * md_w[-1]) ** 0.5),
                xytext=(-10, 0), textcoords="offset points", ha="right", fontsize=11.5,
                fontweight="bold", color=GOLD, path_effects=HALO, zorder=LABEL_Z)

    a2.set_yscale("log")
    a2.plot(xs, per_line, color=GOLD, lw=2.8, zorder=4, path_effects=HALO,
            label="Tokens ÷ line (retired)")
    a2.plot(xs, per_word, color=OI["vermillion"], lw=2.8, ls=(0, (6, 2)), zorder=4,
            path_effects=HALO, label="Tokens ÷ word (headline)")
    a2.set_ylabel("tokens (log scale)", fontsize=10.5)
    a2.set_title("What each costs — only the per-line ratio steps at the reflow",
                 fontsize=12.5, fontweight="bold", loc="left")

    # Panel 2's legend goes upper-left: the reflow marker's rotated label sits along
    # the bottom, and lower-right put the two on top of each other.
    for ax, loc in ((a1, "lower right"), (a2, "upper left")):
        ax.grid(axis="y", which="both", alpha=0.16)
        ax.legend(loc=loc, fontsize=9.5, frameon=False)
        ax.set_xlim(xs[0], xs[-1])
        for sp in ("top", "right"):
            ax.spines[sp].set_visible(False)

    reflow_marker((a1, a2), a2, y=0.05)
    a2.xaxis.set_major_formatter(mdates.DateFormatter("%b %-d"))
    a2.xaxis.set_major_locator(mdates.DayLocator(interval=4))
    plt.setp(a2.get_xticklabels(), rotation=45, ha="right", fontsize=8.5)
    fig.text(0.012, 0.006,
             "same non-blank text both times, only the unit differs · "
             "on a log axis the gap between two curves is their ratio",
             fontsize=7.5, color="#999")
    save(fig, "lines_vs_words")


def main():
    chart_tokens_by_model()
    chart_tokens_per_word()
    chart_tokens_vs_words()
    chart_overview()
    chart_token_anatomy()
    chart_cumulative_cache()
    chart_parallel_sessions()
    chart_velocity()
    chart_lines_vs_words()
    print(f"wrote charts to {CHARTS}")


if __name__ == "__main__":
    main()
