# A velocity chart that survives a reformat and a workflow change

## Why

`claude-usage/` measures spend (tokens) and output (lines).
Neither carries a velocity story on its own: tokens are an input, and the lines series stepped down 18.6k on 2026-08-09 when every doc reflowed to sentence-per-line, so the most striking curve in the set has a reformat baked into it.

The question this plan answers: **what in this project's history is a defensible proxy for how much work shipped per day, and what does it say about the 2026-07-26 machine handover?**

## The methodology traps, measured first

Three candidate proxies, and what is wrong with each.

| candidate | reflow-immune | uniform method across the project | verdict |
|---|---|---|---|
| Commits | yes | **no** | Unit changes mid-project |
| PRs merged | yes | **no** | Series does not exist before June |
| Tests added (`func Test*`) | yes | yes | Clean from day one |
| Queue rows closed | yes | from 2026-05-31 | Clean once `docs/STATUS.md` exists |
| Active hours (commit timestamps) | yes | yes | Clean from day one |

The commits trap is the one worth stating plainly, because it is the obvious choice and it is contaminated in the same way the lines series is.
This repo moved from direct commits on `main` to PR squash-merges partway through: the share of commits on `main` whose subject ends in `(#N)` runs 0% through 2026-05-31, 26% in the first half of June, and 100% from 2026-06-09 on.
So an early "commit" is one raw commit and a later one is a whole squashed PR.
Charting commits across that boundary deflates the recent half against the older one.
It is the mirror image of the reflow, and just as invisible in the drawn line.

`git_metrics.csv` already carries cumulative `commits`, so the trap is one chart call away from being drawn today.

## What the clean proxies say

Measured 2026-08-15 on `origin/main`.
The eras are bounded by the plan upgrade (2026-07-05) and by Opus 5 (2026-07-25) landing one day before `mac-2` (2026-07-26).

| per day | Pro + Max 5x | Max 20x, pre-Opus 5 | mac-2 + Opus 5 |
|---|--:|--:|--:|
| Tests added | 20.3 | 19.4 | **37.9** |
| Go lines added | 1,058 | 963 | **2,135** |
| Queue rows closed | 4.2 | 6.1 | **15.1** |
| Hours of the day with a commit landing | 7.8 | 6.8 | **9.6** |
| Commits per such hour | 2.5 | 2.1 | **3.2** |

Two readings, both of which the chart has to support:

**The plan upgrade bought no velocity.** Max 5x → 20x on 2026-07-05 raised the token ceiling and moved none of these series: tests per day went 20.3 to 19.4, Go lines 1,058 to 963.
Whatever happened three weeks later is not the allowance.

**The jump decomposes into spread and density.** Output roughly doubles at the 07-25/26 boundary, and it is not one effect: commits land across 1.4× more hours of the day, and 1.5× more of them land in each of those hours.

**What that last row is not.** It is tempting to read "hours with a commit" as hours worked, and it is not.
Sessions here run unattended, committing while nobody is watching, and merges are cleared in bulk when the human returns, so a burst of PR-merge commits can compress an afternoon's review into one hour.
The series measures **when work was landing**, which is a property of the system, not of anyone's day.
Spread widening is as consistent with more sessions running unsupervised for longer as it is with longer hours, and this data cannot tell those apart.

That makes the panel worth drawing anyway: unattended spread is the mechanism the machine change is supposed to have moved, and it is the closest thing here to a signal for it.
It just cannot be labelled as human time, in the chart or the caption.

The same error is already published: the `parallel_sessions` chart titled its wall-clock band "at the keyboard", and `session_metrics.csv`'s hours are transcript activity, which unattended sessions generate on their own.
Fixing that wording is in scope.

## What cannot be attributed, and must be said on the chart

**Opus 5 and `mac-2` are one day apart.** Opus 5 first appears 2026-07-25, `mac-2`'s first row is 2026-07-26.
Every series above jumps at that boundary and none can separate the model from the machine.
The single Opus-5-on-`mac-1` day is one data point.

What *is* separable is that the machine had a large, measured, model-independent effect.
[`local-gate-throughput.md`](archive/local-gate-throughput.md) re-baselined the local gate on the replacement machine the week it arrived: a cold `make check` end to end went from ~21 min on the Intel i7 (4 cores / 32 GB) to **102 s** on the M5 Max (18 cores / 128 GB).
A ~12× faster gate is not something a model does.
[#1110](https://github.com/actions-gateway/github-actions-gateway/pull/1110) then sized the parallel-dispatch worker cap from RAM and cores rather than a constant (ceiling 12; a 16 GB laptop gets 1), so parallel capacity rose in two steps rather than one.

So the defensible claim is: **output doubled where the model and the machine changed within a day of each other, and the machine's contribution to it is documented elsewhere in the repo even though this data cannot size it.**

The current `tokens_by_model` caption in [`claude-usage/README.md`](../../claude-usage/README.md) says the jump is "the model and the plan".
The plan half is refuted by the table above.
That caption is in scope for this change.

## Scope

### 1. Two new git-derived series

Both go in `git_metrics.csv`, which is recomputed from scratch each run (git history is durable, and these counts can legitimately fall).

| column | meaning |
|---|---|
| `prs` | cumulative commits on `HEAD` whose subject ends in `(#N)`, the squash-merge signature |
| `queue_closed` | cumulative `Q` anchors that have disappeared from `docs/STATUS.md` across its history |
| `active_hours` | distinct clock hours that day with at least one commit landing |

`queue_closed` counts a row leaving the file, which is completion in the common case but also catches a declined or pruned row.
It is a work-shipped proxy, not a completion ledger, and the README says so.

`active_hours` is a per-day count rather than a cumulative one, unlike every other column here.
It is the only series whose daily value is the quantity of interest.

Walking `docs/STATUS.md` history must be **one** `git log -p` call, not a `git show` per revision: there are 1,135 revisions of that file and the per-revision form takes ~40 s.

### 2. A `velocity` chart, four panels on one timeline

1. **PRs merged per week**, as bars.
   The pre-2026-06-09 region is shaded and labelled as no-PR-workflow rather than drawn as zero.
2. **Tests added per week**, as bars, full project.
3. **Queue rows closed per week**, as bars, shaded before 2026-05-31.
4. **When work landed**: hours-with-a-commit per day as bars, with commits-per-such-hour as a line over them, the idiom `parallel_sessions` already uses for a mean over a peak.
   Titled for what it measures, never as hours worked.

Event lines across all four, and this chart needs two kinds the existing charts do not have:

- **Opus 5 arrives (2026-07-25)**, a model change one day before the machine change.
  Drawing both is the whole point: adjacent lines are what make the confound visible instead of arguable.
- **Methodology steps**: "PR workflow begins" and "backlog begins", styled like the reflow marker (a change in what the series counts, not in what was done).

The reflow marker is deliberately **not** drawn here: it moved the lines denominator and none of these four series.
That follows the rule already in `make_charts.py`: a marker appears only on the charts it distorts.

### 3. Docs

- New chart section in `claude-usage/README.md`, with the confound stated in the caption rather than buried in Methodology.
- New rows in the `git_metrics.csv` column table.
- Fix the `tokens_by_model` caption's "the model and the plan" claim.
- A Methodology bullet for the commits/PR-squash trap, since the trap outlives this chart.

### 4. Tests

`test_compute_metrics.py` covers the merge semantics of the token series.
The new columns are git-derived and recomputed, so what needs covering is the parsing:

- A `(#N)` subject counts as a PR merge; a subject merely mentioning `#N` mid-sentence does not.
- A Q anchor removed between two revisions counts once; one that reappears later is not double-counted on the second removal.
- `active_hours` counts distinct hours, not commits.

## Q824: the row's remedy is refuted, and words are the fix

[Q824](../STATUS.md#Q824) proposes cumulative diff additions as the reformat-proof denominator.
Measured 2026-08-15 over `*.md`, that inverts the problem rather than fixing it: the reflow day adds **+28,250** lines against a normal 100–4,000, because unwrapping a paragraph rewrites every line in it.
The current-tree series steps *down* 18.6k; the proposed remedy steps *up* 28.2k and, being cumulative, never comes back.

Words are the unit that survives, and the check that settles it is per-file on the reflow commit:

| file | lines | words |
|---|--:|--:|
| `docs/design/appendix-h-v2-api-decomposition.md` | +520 / −1,225 | **+0 / −9** |
| `docs/plan/security.md` | +541 / −1,137 | **+8 / −16** |
| `docs/operations/troubleshooting.md` | +1,300 / −1,457 | **+55 / −119** |

Thousands of lines move, the words barely do.
One file disagrees. `docs/development/testing.md` reads +3,221/−3,221 words, an alignment failure rather than a rewrite, so the unit is far more robust, not perfect.

The implementation does **not** need a diff walk.
The existing series counts lines *in the tree* at each day's last commit; the parallel is words in the tree, which a rewrap cannot move by construction and costs 0.24 s a day.
Shipped as `md_words` and `words`.

**What this leaves open.** The headline ratio is still tokens-per-line, with tokens-per-word beside it rather than replacing it.
Swapping the denominator changes a published figure across three charts and every post in the announcement chain, which is a story decision rather than a correctness one.
The `lines_vs_words` chart is what makes it decidable: cost per word rose ~5.6× over the project against ~6.4× per line, so the trend is real in both units and only the 2026-08-09 jump is an artifact.

## Status

Shipped 2026-08-15, except the denominator swap above.

| item | state |
|---|---|
| `prs`, `queue_closed`, `active_hours` in `git_metrics.csv` | ✅ |
| `md_words`, `words` (Q824's real remedy) | ✅ |
| `velocity` chart, four panels | ✅ |
| `lines_vs_words` chart | ✅ |
| Opus 5 arrival marker, derived from the leading model | ✅ |
| README chart sections, column table, commits-trap caveat | ✅ |
| `tokens_by_model` caption's refuted "the model and the plan" claim | ✅ |
| "at the keyboard" corrected wherever it claimed human presence | ✅ |
| Parser tests, verified red by deleting each guard | ✅ |
| Replace tokens-per-line with tokens-per-word as the headline | ❌ open decision |
