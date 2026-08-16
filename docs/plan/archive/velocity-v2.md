# Velocity, second pass: progress rather than volume

## Why

The [first pass](velocity-proxies.md) shipped four proxies that all measure the same thing: how much came out.
None can tell 85 PRs of progress from 85 PRs of churn, and one of them is misleading as drawn.

Measured 2026-08-15 on `origin/main`:

| per day | Pro + Max 5x | Max 20x, pre-Opus 5 | mac-2 + Opus 5 |
|---|--:|--:|--:|
| Backlog rows filed | 6.9 | 6.7 | **20.1** |
| Backlog rows closed | 5.7 | 6.0 | **15.0** |
| Net backlog | **+1.1** | **+0.7** | **+5.0** |
| fix / feat commits | 1.51 | 1.00 | **1.82** |
| PR cycle time (median) | 0.37h | 0.35h | **0.53h** |

The backlog panel currently draws the 15 and omits the 20.
Closing 2.5× more per day while filing 3× more grew the backlog by 106 rows over that window, faster than in any earlier era.
That is not automatically bad, since filing more can mean finding more real work, but it is invisible as the chart stands.

## What each new series is for, and what it is not

**Net backlog** turns a volume measure into a direction.
Filed and closed are drawn together so the reader sees which is larger.

**fix/feat ratio** is the durability dimension none of the volume proxies have.
It is coarse in two directions that cancel unpredictably: a `fix` commit may repair something ancient rather than something just written, and this repo added gates over the same period, so more caught defects can raise the ratio without more defects existing.

**PR cycle time** is open-to-merge, from the GitHub API, and it took three passes to state honestly.

It does not isolate the machine effect, which is why it was first proposed: the local gate runs before a PR is ever opened, so it never contained the hardware change.
The rise from 0.35h to 0.53h was then called unexplained, after queue contention was proposed and refuted (the correlation between a day's merged-PR count and its median cycle time is −0.19, the wrong sign).

The maintainer supplied what the measure actually is.
Merging needs a human to enqueue, and PRs sit when nobody is at the laptop, so open-to-merge tracks availability at least as much as speed.
The distribution confirms it and refutes "unexplained" as well: the p99 runs 33.7h, 90.0h, 23.4h across the eras, which is days away rather than anything slow.
The median moved because the sub-30-minute share fell from 58% to 48%, while p10 held flat and both p90 and p99 fell, so the distribution tightened rather than slowed.

Charted as p25 against p90 on a log axis: the first is the part availability cannot stretch, the second is mostly a record of being elsewhere.
The median is not drawn at all, because it blends the two.

**Code survival** at a fixed horizon: what fraction of the lines added in a week were still present 14 days later.
Fixed-horizon rather than survival-to-HEAD, for two reasons.
Every week is then measured over the same window, so the numbers are comparable; and once a week's horizon has passed its value is final, which makes it cacheable instead of recomputed from scratch each run.

**Commits per day** is drawn despite being contaminated, at the maintainer's request, with the PR-workflow marker on it so the unit change is visible rather than omitted.
An early commit is one commit and a later one is a whole squashed PR; the vertical line is where that changes.

## The input none of these measure

Every proxy here is downstream of one number nothing in the repo records: how many hours the maintainer chose to give the project.
Over this window that rose for reasons outside the data.
The replacement machine is more enjoyable to work on, which drew more hours to it, and the project began to look professionally useful, which made pushing it further worth more.

That makes the 2026-07-26 step a three-way coincidence rather than the two-way one the charts already caveat: model, machine, and time invested all change within a day.
It also reclaims the hours panel.
Its 1.4× was described as system spread, a property of unattended sessions; the maintainer's account is that the hours themselves went up, which is a motivation effect and not a tooling one.
No series here can weigh the two, so the README states both and claims neither.

## Scope

### Data

`git_metrics.csv` gains `queue_filed`, `feat`, `fix`, and the survival column.
A new `pr_metrics.csv` holds one row per merged PR (`number`, `created_at`, `merged_at`, `cycle_hours`), fetched with `gh` and **merge-preserved**, the same treatment `token_metrics.csv` gets and for the same reason: the source is not reproducible offline.

The fetch is incremental.
Each run reads the highest PR number already stored and asks only for what is newer, so the one-time cost (1,521 PRs in 7.5 s) is paid once and later runs cost a fraction of a second.
A failed or unavailable `gh` leaves the existing CSV untouched and the run continues, so every other series keeps working without the network and the charts still rebuild from committed data alone.

### Charts

Seven panels is too tall for one figure, so this splits by question.

- `velocity.png` — **what shipped**: commits, PRs merged, tests added, backlog filed against closed.
- `velocity_quality.png` — **how it went**: fix/feat ratio, PR cycle time, code survival.

Both keep the daily bars, the 7-day centered mean, and the event and methodology markers from the first pass.

## Status

Shipped 2026-08-15.

| item | state |
|---|---|
| `queue_filed`, `feat`, `fix` in `git_metrics.csv` | ✅ |
| `pr_metrics.csv`, incremental and rate-guarded | ✅ |
| `survival_metrics.csv`, fixed 14-day horizon | ✅ |
| `velocity.png` gains commits and filed-against-closed | ✅ |
| `velocity_quality.png` | ✅ |
| README chart sections and data-file docs | ✅ |

Two defects were caught while building, both of the kind that would have shipped looking plausible.

The survival series first mixed clocks: `git log --since/--until` filter on **commit** date while `git blame` reports **author** time, and this repo rebases constantly, so several weeks reported more surviving lines than the week had written.
It was visible only because the rate exceeded 1.
The first version clamped the rate to 1.0, which would have hidden it completely; the clamp is gone for that reason.

The PR fetch was proposed as a way to separate the machine from the model and cannot do it, since the local gate runs before a PR opens.
The follow-up explanation offered for the rise, queue contention, is also unsupported: the correlation is −0.19, the wrong sign.
