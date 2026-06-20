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

Latest snapshot **2026-06-20** (project day 35; first commit 2026-05-16). "Day 7"
is the [original day-7 Bluesky post][post1]'s published figures; "Day 22" is the
[day-22 follow-up][post2]; "Day 35" is the current snapshot the charts here back.

> **Frozen snapshot.** The committed CSVs, `summary.json`, and charts are the
> 2026-06-20 snapshot. Re-running `compute_metrics.py` advances the token/message
> series as new sessions accrue (the merge rule only ever revises upward); leave
> it un-run to keep these figures, or re-run and refresh the charts to roll
> forward to a new dated snapshot.

| Metric | Day 7 | Day 22 | Day 35 | Source |
|---|--:|--:|--:|---|
| Tokens (input + output + cache-creation) | ~10M | 56.2M | **114.7M** | transcripts + est. |
| └ measured only | — | 53.7M | 112.2M | transcripts |
| └ estimated backfill (May 16–18) | — | +2.5M | +2.5M | per-commit estimate |
| └ incl. cache reads | — | 2.02B | **3.91B** | transcripts + est. |
| Cache reuse ratio (reads ÷ writes) | — | ~44× | **~42×** | transcripts |
| Git commits | 232 | 617 | **792** | git |
| Tests (`func Test*`) | 269 | 393 | **547** | git |
| Lines of Go (code) | 15.5k | 20.9k | **28.9k** | git |
| Lines of Go (comments) | 2.3k | 4.2k | **7.5k** | git |
| Markdown (non-blank) | 14.3k | 14.0k | **20.3k** | git |
| YAML (hand-written) | 1.5k | 2.3k | **4.2k** | git |
| Scripts & web (shell/Python/Make/Docker/CSS/JS) | — | — | **6.4k** | git |
| Model mix | mostly Sonnet 4.6 | Sonnet 43% / Opus 57% | **Opus 75% / Sonnet 19% / Fable 5%** | transcripts |

The headline tokens figure **includes the ~2.5M estimated backfill** for the
archived first three days; the measured-only floor is 112.2M. Live totals (with
the measured / estimated split) are always in
[`data/summary.json`](data/summary.json).

## Charts

Rendered to [`charts/`](charts/) at 1× and `@2x` (for upload). Each is
regenerable from the CSVs.

### Overview — all three tokens/lines views together
![Tokens vs lines, cost per line, and the lines composition on one timeline](charts/tokens_overview.png)
The three tokens-vs-lines views combined into one shared-timeline figure:
**(1)** magnitude — tokens vs lines authored on a log axis (gap = cost/line);
**(2)** breakdown — what those lines are (the composition);
**(3)** cost — cumulative tokens ÷ line, with the value at each weekly guide.
The standalone versions follow below.

### Daily token usage by model
![Daily token usage by model](charts/tokens_by_model.png)
The Pro→Max upgrade (dashed line, 2026-05-23) is visible as the hand-off from
Sonnet 4.6 (orange) to Opus 4.7 (purple), then Opus 4.8 (blue), with Fable 5
(green) appearing from June 9. Charts use the Okabe–Ito colourblind-safe palette,
and each model also carries its own hatch pattern.

### Tokens spent vs. lines authored (the magnitude)
![Cumulative tokens far above cumulative lines authored, log scale](charts/tokens_vs_lines.png)
Log y so both ends are visible at once: ~115M cumulative tokens ride well above
~60k lines authored (a linear axis crushes the lines to an invisible sliver). The
gold-shaded gap between the two curves is the ~1,900 tokens/line — on a log axis a
ratio is a vertical gap. "Lines authored" is all hand-written output — Go (code +
tests), Markdown, hand-written YAML, and scripts & web; generated CRD YAML,
binaries, and lockfiles excluded. The undistorted breakdown of those lines is in
the next chart.

### Tokens per line authored (the trend & the breakdown)
![Cost per line over time above a stacked breakdown of the lines](charts/tokens_per_line.png)
**Top:** cumulative tokens ÷ lines authored, by day (measured days only). It climbs
from ~410 tokens/line in week one to ~1,900 a month in — each line costs ~5× more
once the easy scaffolding is done and the work shifts to logic, tests, review, and
debugging. **Bottom:** the denominator itself, decomposed — Go code, Go tests,
Markdown docs, hand-written YAML, scripts & web. Its total height at any date *is*
the divisor above, so "a line" is shown, not just named; tests and docs together
dwarf non-test Go code.

### Anatomy of token usage (log scale)
![Token usage anatomy on a log scale](charts/token_anatomy.png)
Daily input / output / cache-creation / cache-read, log Y. Cache reads sit an
order of magnitude above everything else, every day.

### Cumulative cache traffic
![Cumulative cache traffic](charts/cumulative_cache.png)
Cumulative cache reads (3.79B) vs writes (91M). Write once, replay ~42×.

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
Per-day (last commit of each day) cumulative `commits`, `tests` (count of
`func Test*`), `go_code` (non-blank minus line-comment Go lines, code + tests),
`go_test` (the test-file subset of `go_code`), `md` (non-blank Markdown),
`yaml` (non-blank hand-written YAML — generated CRD/controller-gen YAML excluded),
and `scripts` (non-blank shell, Python, CSS/JS/HTML, Makefile, Dockerfile).
All exclude `vendor/`.

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
  figure, not a measurement — the defensible measured-only floor is 111.5M. The
  git series is fully measured from 2026-05-16.
- **Tokens-per-line is a proxy.** The denominator is all hand-authored output —
  Go (code + tests), Markdown, hand-written YAML, and scripts & web (shell,
  Python, Make/Docker, CSS/JS) — but tokens also go into review, debugging, and
  exploration that never lands as a line, so the ratio tracks overall
  effort-per-output, not the literal cost of one line. Generated YAML
  (CRDs/controller-gen, ~44k lines), binaries, lockfiles, and license boilerplate
  are excluded so non-authored content doesn't dilute it. Estimated
  (pre-transcript) days are excluded so it's measured-only.
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
