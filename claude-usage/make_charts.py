#!/usr/bin/env python3
"""Render the four kept charts from the persisted CSVs in claude-usage/data/.

Reads only the committed CSVs (never the raw transcripts), so the charts are
reproducible even after the source sessions are archived. Rows flagged
``estimated=1`` (the backfilled pre-transcript days) are drawn distinctly —
hatched bars, dashed lines, and a shaded band — so estimates are never passed
off as measured data. Run:

    python3 claude-usage/make_charts.py        # needs matplotlib + numpy

Outputs PNGs (1x + @2x) to claude-usage/charts/:
    tokens_by_model      daily token usage by model, with the Pro->Max marker
    growth_vs_codebase     cumulative growth vs the day-7 post baseline (fan)
    token_anatomy         daily input/output/cache tokens on a log scale
    cumulative_cache      cumulative cache reads vs writes (stacked area)
"""

import csv
import os
from datetime import date

import matplotlib
matplotlib.use("Agg")
import matplotlib.dates as mdates
import matplotlib.patches as mpatches
import matplotlib.pyplot as plt
import numpy as np

HERE = os.path.dirname(os.path.abspath(__file__))
DATA = os.path.join(HERE, "data")
CHARTS = os.path.join(HERE, "charts")

PRO_TO_MAX = date(2026, 5, 23)
# Published day-7 Bluesky post values; the growth chart plots growth relative to these.
BASELINE = {"tokens": 10_000_000, "commits": 232, "tests": 269, "go_code": 15500}
MODEL_COLORS = {
    "Sonnet 4.6": "#D4A24E", "Opus 4.7": "#7C5CBF",
    "Opus 4.8": "#4361A8", "Haiku 4.5": "#9AA0A6", "Other": "#BBBBBB", "Unknown": "#DDDDDD",
}
EST_NOTE = "shaded / hatched = pre-transcript days estimated from the Pro-era per-commit rate"


def load(name):
    with open(os.path.join(DATA, name)) as fh:
        return list(csv.DictReader(fh))


def is_est(r):
    return str(r.get("estimated", "0")) == "1"


def save(fig, stem):
    os.makedirs(CHARTS, exist_ok=True)
    fig.savefig(os.path.join(CHARTS, f"{stem}.png"), dpi=160, bbox_inches="tight")
    fig.savefig(os.path.join(CHARTS, f"{stem}@2x.png"), dpi=320, bbox_inches="tight")
    plt.close(fig)


def dparse(s):
    return date.fromisoformat(s)


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
    est_dates = {r["date"] for r in rows if is_est(r)}
    models = ["Sonnet 4.6", "Opus 4.7", "Opus 4.8", "Haiku 4.5", "Other", "Unknown"]
    by = {(r["date"], r["model"]): int(r["headline"]) for r in rows}
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
                    edgecolor="white", linewidth=0.4)
        containers.append(bc)
        drawn.append(m)
        bottom = [b + v for b, v in zip(bottom, vals)]
    # hatch the estimated (backfilled) bars
    for bc in containers:
        for xi, d in enumerate(days):
            if d in est_dates:
                bc.patches[xi].set_hatch("////")
                bc.patches[xi].set_alpha(0.55)
    # clean legend proxies (so the hatched May-16 bar doesn't bleed into the swatches)
    handles = [mpatches.Patch(color=MODEL_COLORS[m], label=m) for m in drawn]
    handles.append(mpatches.Patch(facecolor="#cccccc", hatch="////", label="estimated"))
    upg = PRO_TO_MAX.isoformat()
    if upg in days:
        xi = days.index(upg)
        ax.axvline(xi - 0.5, color="#222", ls="--", lw=1.4)
        ax.text(xi - 0.4, max(bottom) * 0.92, "  Pro → Max", fontsize=10, fontweight="bold", color="#222")
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
    save(fig, "tokens_by_model")


def _token_rows():
    rows = {r["date"]: r for r in load("token_metrics.csv")}
    return rows, sorted(rows)


def chart_growth_vs_codebase():
    trows, tdays = _token_rows()
    cum = {}
    run = 0
    for d in tdays:
        r = trows[d]
        run += int(r["input"]) + int(r["output"]) + int(r["cache_creation"])
        cum[d] = run
    git = {r["date"]: r for r in load("git_metrics.csv")}
    est_dates = {d for d in tdays if is_est(trows[d])}

    all_dates = sorted(set(tdays) | set(git))
    series = {"Tokens used": [], "Git commits": [], "Tests": [], "Lines of Go": []}
    last_tok = 0
    last_git = {"commits": 0, "tests": 0, "go_code": 0}
    for d in all_dates:
        if d in cum:
            last_tok = cum[d]
        if d in git:
            last_git = {k: int(git[d][k]) for k in ("commits", "tests", "go_code")}
        series["Tokens used"].append(last_tok / BASELINE["tokens"])
        series["Git commits"].append(last_git["commits"] / BASELINE["commits"])
        series["Tests"].append(last_git["tests"] / BASELINE["tests"])
        series["Lines of Go"].append(last_git["go_code"] / BASELINE["go_code"])

    dts = [dparse(d) for d in all_dates]
    first_measured = min(d for d in tdays if not is_est(trows[d]))
    i_fm = all_dates.index(first_measured)
    styles = {"Tokens used": ("#C9922E", 3.2), "Git commits": ("#5C9E7C", 2.0),
              "Tests": ("#7C5CBF", 2.0), "Lines of Go": ("#4361A8", 2.0)}
    lbl_dy = {"Tests": 7, "Lines of Go": -7}
    fig, ax = plt.subplots(figsize=(11, 5.6))
    shade_estimated(ax, {d.isoformat() for d in dts if d.isoformat() in est_dates}, dts)
    for name, (col, lw) in styles.items():
        vals = series[name]
        if name == "Tokens used":
            # estimated prefix dashed, measured remainder solid
            ax.plot(dts[:i_fm + 1], vals[:i_fm + 1], color=col, lw=lw, ls=(0, (5, 2)), zorder=3)
            ax.plot(dts[i_fm:], vals[i_fm:], color=col, lw=lw, solid_capstyle="round", zorder=3, label=name)
        else:
            ax.plot(dts, vals, color=col, lw=lw, solid_capstyle="round", zorder=2, label=name)
        ax.annotate(f"{vals[-1]:.1f}×", (dts[-1], vals[-1]), xytext=(8, lbl_dy.get(name, 0)),
                    textcoords="offset points", va="center", fontsize=11, fontweight="bold", color=col)
    ax.axhline(1.0, color="#999", ls=":", lw=1.2, zorder=1)
    ax.text(dts[i_fm], 1.06, "day-7 post baseline (1×)", fontsize=9, color="#777")
    ax.axvline(PRO_TO_MAX, color="#222", ls="--", lw=1.1, zorder=1)
    ax.text(PRO_TO_MAX, 5.4, " Pro→Max", fontsize=9.5, fontweight="bold", color="#222")
    ax.set_title("Tokens vs. the codebase: cumulative growth", fontsize=14, fontweight="bold", loc="left")
    ax.set_ylabel("growth multiple  (× day-7 post value)", fontsize=11)
    ax.xaxis.set_major_formatter(mdates.DateFormatter("%b %-d"))
    ax.xaxis.set_major_locator(mdates.DayLocator(interval=2))
    plt.setp(ax.get_xticklabels(), rotation=45, ha="right", fontsize=8.5)
    ax.set_ylim(0, 6.0)
    ax.set_xlim(dts[0], dts[-1])
    ax.legend(frameon=False, fontsize=10.5, loc="upper left")
    for s in ("top", "right"):
        ax.spines[s].set_visible(False)
    ax.grid(alpha=0.22)
    fig.text(0.012, 0.01, "dashed token segment / shading (May 16–18) estimated from archived sessions",
             fontsize=7.5, color="#999")
    fig.tight_layout()
    save(fig, "growth_vs_codebase")


def chart_token_anatomy():
    trows, tdays = _token_rows()
    days = [dparse(d) for d in tdays]
    est_dates = {d for d in tdays if is_est(trows[d])}

    def arr(k):
        a = np.array([int(trows[d][k]) for d in tdays], float)
        a[a <= 0] = np.nan
        return a

    spec = [("cache_read", "Cache read", "#2E5A9C"), ("cache_creation", "Cache creation", "#5BA3C4"),
            ("output", "Output", "#D4A24E"), ("input", "Input (fresh)", "#C25B5B")]
    totals = {k: sum(int(r[k]) for r in trows.values()) for k, _, _ in spec}
    fig, ax = plt.subplots(figsize=(11, 5.6))
    shade_estimated(ax, est_dates, days)
    for k, lbl, col in spec:
        ax.plot(days, arr(k), color=col, lw=2.4, marker="o", ms=3,
                label=f"{lbl}  ({totals[k] / 1e6:,.0f}M total)")
        ax.fill_between(days, arr(k), 0.1, color=col, alpha=0.07)
    ax.set_yscale("log")
    ax.set_ylim(1e3, 5e8)
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
    ax.fill_between(days, 0, cc, color="#7FB2D6", label="cache writes  (writes once)", zorder=2)
    ax.fill_between(days, cc, cc + cr, color="#23467E", label="cache reads  (replayed context)", zorder=2)
    ax.plot(days, cc + cr, color="#15294d", lw=1.5, zorder=3)
    ax.annotate(f"{cr[-1]:.2f}B", (days[-1], (cc + cr)[-1]), xytext=(10, -2),
                textcoords="offset points", va="center", fontsize=15, fontweight="bold", color="#23467E")
    ax.annotate(f"cache writes: {cc[-1] * 1000:.0f}M", (days[-1], cc[-1] * 0.5), xytext=(-12, 80),
                textcoords="offset points", ha="right", fontsize=11, color="white", fontweight="bold",
                arrowprops=dict(arrowstyle="-|>", color="white", lw=1.3))
    ax.set_title("Cumulative cache traffic", fontsize=14, fontweight="bold", loc="left")
    ax.set_ylabel("cumulative tokens  (billions)", fontsize=11)
    ax.xaxis.set_major_formatter(mdates.DateFormatter("%b %-d"))
    ax.xaxis.set_major_locator(mdates.DayLocator(interval=2))
    plt.setp(ax.get_xticklabels(), rotation=45, ha="right", fontsize=8.5)
    ax.set_xlim(days[0], days[-1])
    ax.set_ylim(0, (cc + cr)[-1] * 1.10)
    ax.legend(frameon=False, fontsize=10.5, loc="upper left")
    for s in ("top", "right"):
        ax.spines[s].set_visible(False)
    ax.grid(axis="y", alpha=0.22)
    fig.text(0.012, 0.01, "shaded band (May 16–18) estimated from archived sessions", fontsize=7.5, color="#999")
    fig.tight_layout()
    save(fig, "cumulative_cache")


def main():
    chart_tokens_by_model()
    chart_growth_vs_codebase()
    chart_token_anatomy()
    chart_cumulative_cache()
    print(f"wrote charts to {CHARTS}")


if __name__ == "__main__":
    main()
