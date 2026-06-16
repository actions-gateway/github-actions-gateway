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

### Backfilled (estimated) days

The project's first commits (2026-05-16 to -18) predate the earliest surviving
transcript (2026-05-19) — those sessions were archived before any were saved, so
their token usage is **gone from the logs**. Rather than drop those days, the
script **backfills** them: it derives a per-commit token rate from the Pro-era
window (the measured days before the Pro→Max upgrade) and multiplies by the
number of commits authored each archived day. Every backfilled row is flagged
`estimated=1` in the CSVs, surfaced separately in `summary.json`
(`totals.measured` vs `totals.estimated`), and drawn distinctly on the charts
(hatched bars, dashed lines, shaded band). The backfill is recomputed
deterministically each run; measured rows are never overwritten by estimates.

## Quick start

```bash
# 1. Snapshot the latest data (stdlib only — no venv needed):
python3 claude-usage/compute_metrics.py

# 2. Render the charts (needs matplotlib + numpy):
python3 -m venv .venv && .venv/bin/pip install -r claude-usage/requirements.txt
.venv/bin/python claude-usage/make_charts.py
```

`compute_metrics.py` reads the transcripts for *this* machine's copy of the
project. Override the lookup with `CLAUDE_PROJECTS_GLOB` if your transcripts live
elsewhere. `make_charts.py` reads **only** the committed CSVs, so the charts
reproduce identically on any machine, even with no transcripts present.

## Results

Latest snapshot **2026-06-16** (project day 31; first commit 2026-05-16). "Day 7"
is the [original day-7 Bluesky post][post1]'s published figures; "Day 22" is the
[day-22 follow-up][post2]; "Day 31" is the current snapshot the charts here back.

| Metric | Day 7 | Day 22 | Day 31 | Source |
|---|--:|--:|--:|---|
| Tokens (input + output + cache-creation) | ~10M | 56.2M | **94.1M** | transcripts + est. |
| └ measured only | — | 53.7M | 91.6M | transcripts |
| └ estimated backfill (May 16–18) | — | +2.5M | +2.5M | per-commit estimate |
| └ incl. cache reads | — | 2.02B | **3.18B** | transcripts + est. |
| Cache reuse ratio (reads ÷ writes) | — | ~44× | **~41×** | transcripts |
| Git commits | 232 | 617 | **716** | git |
| Tests (`func Test*`) | 269 | 393 | **479** | git |
| Lines of Go (code) | 15.5k | 20.9k | **25.3k** | git |
| Lines of Go (comments) | 2.3k | 4.2k | **6.1k** | git |
| Markdown (non-blank) | 14.3k | 14.0k | **18.6k** | git |
| YAML (hand-written) | 1.5k | 2.3k | **4.4k** | git |
| Model mix | mostly Sonnet 4.6 | Sonnet 43% / Opus 57% | **Opus 70% / Sonnet 23% / Fable 7%** | transcripts |

The headline tokens figure **includes the ~2.5M estimated backfill** for the
archived first three days; the measured-only floor is 91.6M. Live totals (with
the measured / estimated split) are always in
[`data/summary.json`](data/summary.json).

## Charts

Rendered to [`charts/`](charts/) at 1× and `@2x` (for upload). Each is
regenerable from the CSVs.

### Daily token usage by model
![Daily token usage by model](charts/tokens_by_model.png)
The Pro→Max upgrade (dashed line, 2026-05-23) is visible as the hand-off from
Sonnet 4.6 (gold) to Opus 4.7 (purple), then Opus 4.8 (blue), with Fable 5
(teal) appearing from June 9.

### Tokens vs. the codebase: cumulative growth
![Cumulative growth vs the codebase](charts/growth_vs_codebase.png)
Each series normalized to its **day-7 post value** (1×), so the lines cross
near the post and fan apart. Token spend grew ~9× while the code grew ~1.6×.

### Anatomy of token usage (log scale)
![Token usage anatomy on a log scale](charts/token_anatomy.png)
Daily input / output / cache-creation / cache-read, log Y. Cache reads sit an
order of magnitude above everything else, every day.

### Cumulative cache traffic
![Cumulative cache traffic](charts/cumulative_cache.png)
Cumulative cache reads (3.1B) vs writes (75M). Write once, replay ~41×.

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
| `estimated` | `1` for backfilled (archived) days, `0` for measured |

### `model_daily.csv` — merge-preserved
Per-day, per-model `headline` (input+output+cache_creation), `output`,
`messages`, and an `estimated` flag. Backfilled archived days are attributed to
the Pro-era model (Sonnet 4.6). Drives the token-usage-by-model chart.

### `git_metrics.csv` — recomputed each run
Per-day (last commit of each day) cumulative `commits`, `tests`, and `go_code`
(non-blank minus line-comment Go lines, excluding `vendor/`).

### `summary.json`
Totals split into `measured` / `estimated` / `combined` (summed from the
persisted rows, so archival-safe), an `estimation` block documenting the
per-commit method, per-model split, an accurate HEAD working-tree snapshot, and
full provenance.

## Methodology & caveats

- **Dedup.** Resumed/compacted sessions replay earlier records verbatim. Token
  usage is deduped on `(message.id, requestId)`; without it cache-read totals
  roughly double. Message counts dedup on record `uuid`.
- **Archived early days are estimated, not measured.** The project's first
  commits (2026-05-16 to -18) predate the earliest surviving transcript
  (2026-05-19), so their token usage is gone from the logs. Those days are
  **backfilled** from the Pro-era per-commit rate and flagged `estimated=1`
  (see "Backfilled (estimated) days" above). The ~2.5M backfill is a modeled
  figure, not a measurement — the defensible measured-only floor is 91.6M. The
  git series is fully measured from 2026-05-16.
- **Growth-chart baseline.** Normalized against the *published* day-7 post values
  (10M / 232 / 269 / 15.5k), not a recomputed day-7 — so endpoints stay
  consistent with what was posted. The constants live in `make_charts.py`.
- **Date basis differs by source.** Token dates are UTC (from message
  timestamps); git dates are author-local (`--date=short`). Close enough at
  daily granularity, but they can disagree by a day at midnight boundaries.
- **`go_code` is approximate in the daily series** (non-blank minus line
  comments, so block comments count as code). `summary.json`'s HEAD snapshot
  uses an exact comment-aware split; the two agree to within ~0.1%.
- **Messages are fuzzy.** The original post's "20k messages" came from a
  counter that can't be reconstructed from these logs; treat `assistant_msgs` /
  `user_msgs` as the well-defined replacements, not as the same quantity.

[post1]: https://bsky.app/profile/karlkfi.bsky.social/post/3mmpo56ds6c23
[post2]: https://bsky.app/profile/karlkfi.bsky.social/post/3mnqx3gztwk2e
