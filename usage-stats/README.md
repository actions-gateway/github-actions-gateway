# Claude Code usage stats

A reproducible record of how much Claude Code work went into this project over
time — token usage, message counts, model mix — alongside the durable git output
(commits, tests, lines of Go), plus the charts derived from them.

This is **development-process metadata**, not part of the github-actions-gateway
product. It lives here so the numbers are preserved and recomputable.

## Why it's snapshotted

Token and message data comes from local Claude Code session transcripts
(`~/.claude/projects/*github-actions-gateway*/*.jsonl`). Those transcripts can be
**archived or deleted**, which would permanently lose the history. So the fragile
series are written to committed CSVs under [`data/`](data/) using a merge rule
that only ever revises a past day's value *upward*
([`compute_metrics.py`](compute_metrics.py) `merge_max`). Re-running after some
sessions are gone can never erase data already recorded.

Git-derived series are **recomputed from scratch** each run — git history is
durable, and counts like test totals or lines of Go can legitimately go down
(code gets deleted), so taking a max would be wrong for them.

## Quick start

```bash
# 1. Snapshot the latest data (stdlib only — no venv needed):
python3 usage-stats/compute_metrics.py

# 2. Render the charts (needs matplotlib + numpy):
python3 -m venv .venv && .venv/bin/pip install -r usage-stats/requirements.txt
.venv/bin/python usage-stats/make_charts.py
```

`compute_metrics.py` reads the transcripts for *this* machine's copy of the
project. Override the lookup with `CLAUDE_PROJECTS_GLOB` if your transcripts live
elsewhere. `make_charts.py` reads **only** the committed CSVs, so the charts
reproduce identically on any machine, even with no transcripts present.

## Results

Snapshot at **2026-06-07** (project day 22; first commit 2026-05-16). "Day 7" is
the [original tweet](https://twitter.com)'s published figures.

| Metric | Day 7 | Day 22 | Source |
|---|--:|--:|---|
| Tokens (input + output + cache-creation) | ~10M | **53.6M** | transcripts |
| └ incl. cache reads | — | **1.95B** | transcripts |
| Cache reuse ratio (reads ÷ writes) | — | **~45×** | transcripts |
| Git commits | 232 | **617** | git |
| Tests (`func Test*`) | 269 | **393** | git |
| Lines of Go (code) | 15.5k | **20.9k** | git |
| Lines of Go (comments) | 2.3k | **4.2k** | git |
| Markdown (non-blank) | 14.3k | **13.9k** | git |
| YAML (hand-written) | 1.5k | **2.3k** | git |
| Model mix | mostly Sonnet 4.6 | **Sonnet 43% / Opus 57%** | transcripts |

Live totals are always in [`data/summary.json`](data/summary.json).

## Charts

Rendered to [`charts/`](charts/) at 1× and `@2x` (for upload). Each is
regenerable from the CSVs.

### A — Daily token usage by model
![A](charts/A_model_inflection.png)
The Pro→Max upgrade (dashed line, 2026-05-23) is visible as the hand-off from
Sonnet 4.6 (gold) to Opus 4.7 (purple), then Opus 4.8 (blue).

### E — Tokens vs. the codebase: cumulative growth
![E](charts/E_growth_timeseries.png)
Each series normalized to its **day-7 tweet value** (1×), so the lines cross
near the tweet and fan apart. Token spend grew ~5× while the code grew ~1.4×.

### F — Anatomy of token usage (log scale)
![F](charts/F_token_anatomy.png)
Daily input / output / cache-creation / cache-read, log Y. Cache reads sit an
order of magnitude above everything else, every day.

### I — Cumulative cache traffic
![I](charts/I_cumulative_cache.png)
Cumulative cache reads (1.9B) vs writes (42M). Write once, replay ~45×.

## Data files

All under [`data/`](data/).

### `token_metrics.csv` — merge-preserved
| column | meaning |
|---|---|
| `date` | UTC date of the message timestamp |
| `input` / `output` | non-cached input and output tokens |
| `cache_creation` / `cache_read` | cache write and cache read tokens |
| `assistant_msgs` | assistant API responses (deduped on `message.id`+`requestId`) |
| `user_msgs` | user/tool-result records (deduped on record `uuid`) |

### `model_daily.csv` — merge-preserved
Per-day, per-model `headline` (input+output+cache_creation), `output`, and
`messages`. Drives chart A.

### `git_metrics.csv` — recomputed each run
Per-day (last commit of each day) cumulative `commits`, `tests`, and `go_code`
(non-blank minus line-comment Go lines, excluding `vendor/`).

### `summary.json`
Headline totals (summed from the *merged* CSV, so archival-safe), per-model
split, an accurate HEAD working-tree snapshot, and full provenance.

## Methodology & caveats

- **Dedup.** Resumed/compacted sessions replay earlier records verbatim. Token
  usage is deduped on `(message.id, requestId)`; without it cache-read totals
  roughly double. Message counts dedup on record `uuid`.
- **Archived early days.** The project's first commits (2026-05-16 to -18)
  predate the earliest surviving transcript (2026-05-19). Those days have **no
  token data and never will** — the token series and chart E start 2026-05-19,
  while the git series covers the full history from 2026-05-16.
- **Chart E baseline.** Normalized against the *published* day-7 tweet values
  (10M / 232 / 269 / 15.5k), not a recomputed day-7 — so endpoints stay
  consistent with what was tweeted. The constants live in `make_charts.py`.
- **Date basis differs by source.** Token dates are UTC (from message
  timestamps); git dates are author-local (`--date=short`). Close enough at
  daily granularity, but they can disagree by a day at midnight boundaries.
- **`go_code` is approximate in the daily series** (non-blank minus line
  comments, so block comments count as code). `summary.json`'s HEAD snapshot
  uses an exact comment-aware split; the two agree to within ~0.1%.
- **Messages are fuzzy.** The original tweet's "20k messages" came from a
  counter that can't be reconstructed from these logs; treat `assistant_msgs` /
  `user_msgs` as the well-defined replacements, not as the same quantity.
