#!/usr/bin/env python3
"""Render the four kept charts from the persisted CSVs in usage-stats/data/.

Reads only the committed CSVs (never the raw transcripts), so the charts are
reproducible even after the source sessions are archived. Run:

    python3 usage-stats/make_charts.py        # needs matplotlib + numpy

Outputs PNGs (1x + @2x) to usage-stats/charts/:
    A_model_inflection      daily token usage by model, with the Pro->Max marker
    E_growth_timeseries     cumulative growth vs the day-7 tweet baseline (fan)
    F_token_anatomy         daily input/output/cache tokens on a log scale
    I_cumulative_cache      cumulative cache reads vs writes (stacked area)
"""

import csv
import os
from datetime import date

import matplotlib
matplotlib.use("Agg")
import matplotlib.dates as mdates
import matplotlib.pyplot as plt
import numpy as np

HERE = os.path.dirname(os.path.abspath(__file__))
DATA = os.path.join(HERE, "data")
CHARTS = os.path.join(HERE, "charts")

PRO_TO_MAX = date(2026, 5, 23)
# Published day-7 tweet values; chart E plots growth relative to these.
BASELINE = {"tokens": 10_000_000, "commits": 232, "tests": 269, "go_code": 15500}
MODEL_COLORS = {
    "Sonnet 4.6": "#D4A24E", "Opus 4.7": "#7C5CBF",
    "Opus 4.8": "#4361A8", "Haiku 4.5": "#9AA0A6", "Other": "#BBBBBB", "Unknown": "#DDDDDD",
}


def load(name):
    with open(os.path.join(DATA, name)) as fh:
        return list(csv.DictReader(fh))


def save(fig, stem):
    os.makedirs(CHARTS, exist_ok=True)
    fig.savefig(os.path.join(CHARTS, f"{stem}.png"), dpi=160, bbox_inches="tight")
    fig.savefig(os.path.join(CHARTS, f"{stem}@2x.png"), dpi=320, bbox_inches="tight")
    plt.close(fig)


def dparse(s):
    return date.fromisoformat(s)


def chart_A():
    rows = load("model_daily.csv")
    days = sorted({r["date"] for r in rows})
    models = ["Sonnet 4.6", "Opus 4.7", "Opus 4.8", "Haiku 4.5", "Other", "Unknown"]
    by = {(r["date"], r["model"]): int(r["headline"]) for r in rows}
    xs = list(range(len(days)))
    fig, ax = plt.subplots(figsize=(11, 5.2))
    bottom = [0.0] * len(days)
    for m in models:
        vals = [by.get((d, m), 0) / 1e6 for d in days]
        if sum(vals) == 0:
            continue
        ax.bar(xs, vals, bottom=bottom, label=m, color=MODEL_COLORS[m], width=0.82,
               edgecolor="white", linewidth=0.4)
        bottom = [b + v for b, v in zip(bottom, vals)]
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
    ax.legend(frameon=False, fontsize=10, ncol=4, loc="upper center", bbox_to_anchor=(0.5, -0.18))
    for s in ("top", "right"):
        ax.spines[s].set_visible(False)
    ax.grid(axis="y", alpha=0.25)
    fig.tight_layout()
    save(fig, "A_model_inflection")


def _cumulative_token_headline():
    rows = {r["date"]: r for r in load("token_metrics.csv")}
    days = sorted(rows)
    cum = []
    run = 0
    for d in days:
        r = rows[d]
        run += int(r["input"]) + int(r["output"]) + int(r["cache_creation"])
        cum.append((d, run))
    return cum


def chart_E():
    tok = dict(_cumulative_token_headline())
    git = {r["date"]: r for r in load("git_metrics.csv")}
    # union of dates, carry git forward, tokens default to last known
    all_dates = sorted(set(tok) | set(git))
    series = {"Tokens used": [], "Git commits": [], "Tests": [], "Lines of Go": []}
    last_tok = 0
    last_git = {"commits": 0, "tests": 0, "go_code": 0}
    for d in all_dates:
        if d in tok:
            last_tok = tok[d]
        if d in git:
            last_git = {k: int(git[d][k]) for k in ("commits", "tests", "go_code")}
        series["Tokens used"].append(last_tok / BASELINE["tokens"])
        series["Git commits"].append(last_git["commits"] / BASELINE["commits"])
        series["Tests"].append(last_git["tests"] / BASELINE["tests"])
        series["Lines of Go"].append(last_git["go_code"] / BASELINE["go_code"])

    # only plot from the first day with token data (earlier days had archived sessions)
    first_tok = min(tok)
    i0 = all_dates.index(first_tok)
    dts = [dparse(d) for d in all_dates]
    styles = {"Tokens used": ("#C9922E", 3.2), "Git commits": ("#5C9E7C", 2.0),
              "Tests": ("#7C5CBF", 2.0), "Lines of Go": ("#4361A8", 2.0)}
    lbl_dy = {"Tests": 7, "Lines of Go": -7}
    fig, ax = plt.subplots(figsize=(11, 5.6))
    for name, (col, lw) in styles.items():
        vals = series[name]
        ax.plot(dts[i0:], vals[i0:], color=col, lw=lw, solid_capstyle="round",
                zorder=3 if name == "Tokens used" else 2, label=name)
        ax.annotate(f"{vals[-1]:.1f}×", (dts[-1], vals[-1]), xytext=(8, lbl_dy.get(name, 0)),
                    textcoords="offset points", va="center", fontsize=11, fontweight="bold", color=col)
    ax.axhline(1.0, color="#999", ls=":", lw=1.2, zorder=1)
    ax.text(dts[i0], 1.06, "day-7 tweet baseline (1×)", fontsize=9, color="#777")
    ax.axvline(PRO_TO_MAX, color="#222", ls="--", lw=1.1, zorder=1)
    ax.text(PRO_TO_MAX, 5.4, " Pro→Max", fontsize=9.5, fontweight="bold", color="#222")
    ax.set_title("Tokens vs. the codebase: cumulative growth", fontsize=14, fontweight="bold", loc="left")
    ax.set_ylabel("growth multiple  (× day-7 tweet value)", fontsize=11)
    ax.xaxis.set_major_formatter(mdates.DateFormatter("%b %-d"))
    ax.xaxis.set_major_locator(mdates.DayLocator(interval=2))
    plt.setp(ax.get_xticklabels(), rotation=45, ha="right", fontsize=8.5)
    ax.set_ylim(0, 6.0)
    ax.set_xlim(dts[i0], dts[-1])
    ax.legend(frameon=False, fontsize=10.5, loc="upper left")
    for s in ("top", "right"):
        ax.spines[s].set_visible(False)
    ax.grid(alpha=0.22)
    fig.text(0.012, 0.01, f"pre-{first_tok} sessions archived — token line starts {first_tok}",
             fontsize=7.5, color="#999")
    fig.tight_layout()
    save(fig, "E_growth_timeseries")


def chart_F():
    rows = {r["date"]: r for r in load("token_metrics.csv")}
    days = [dparse(d) for d in sorted(rows)]

    def arr(k):
        a = np.array([int(rows[d.isoformat()][k]) for d in days], float)
        a[a <= 0] = np.nan
        return a

    spec = [("cache_read", "Cache read", "#2E5A9C"), ("cache_creation", "Cache creation", "#5BA3C4"),
            ("output", "Output", "#D4A24E"), ("input", "Input (fresh)", "#C25B5B")]
    totals = {k: sum(int(r[k]) for r in rows.values()) for k, _, _ in spec}
    fig, ax = plt.subplots(figsize=(11, 5.6))
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
    fig.tight_layout()
    save(fig, "F_token_anatomy")


def chart_I():
    rows = {r["date"]: r for r in load("token_metrics.csv")}
    days = [dparse(d) for d in sorted(rows)]
    cc = np.cumsum([int(rows[d.isoformat()]["cache_creation"]) for d in days]) / 1e9
    cr = np.cumsum([int(rows[d.isoformat()]["cache_read"]) for d in days]) / 1e9
    fig, ax = plt.subplots(figsize=(11, 5.6))
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
    fig.tight_layout()
    save(fig, "I_cumulative_cache")


def main():
    chart_A()
    chart_E()
    chart_F()
    chart_I()
    print(f"wrote charts to {CHARTS}")


if __name__ == "__main__":
    main()
