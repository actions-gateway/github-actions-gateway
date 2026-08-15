# Merge queue: kill the freshness-invalidation loop

**Goal:** stop paying a full heavy-gate re-run (e2e ~9–30 min × two CNI lanes) every time `main` moves under an in-flight PR, without weakening the nothing-red-reaches-`main` guarantee.

**Approach:** adopt GitHub's merge queue on `main`.
The queue constructs each candidate merge result and runs the required checks on *that* (the `merge_group` event), so the jointly-red hazard the manual freshness rule guards against is checked by machine, once per batch, instead of by every session rebasing and re-running CI on every push.
Decision approved 2026-08-03: adopt, and keep e2e per-PR (sessions keep early signal while iterating; the queue run is the authoritative combined check).
Revised 2026-08-12 by the Phase 3 re-measure below: e2e is now merge-group-only, because the early signal was measurably not worth its multiplier.

## Status

| Phase | State |
|---|---|
| 1. `merge_group` triggers on the 9 required-check workflows | ✅ #1256, merged 2026-08-03 |
| 2. Activation: add the `merge_queue` rule to the `default-protect` ruleset | ✅ 2026-08-03 — rule live, verified by GET after PUT; queue-era process-doc rewrite in the same change as this row |
| 3. Re-measure after ~1 week; decide follow-ups | ✅ 2026-08-12 — re-measured, e2e demoted to merge-group-only; the other three candidates declined |

## The measurement (2026-08-03)

Taken from the last 30 days of CI runs, merged PRs, and `git log` contention:

- 26–34 PRs merge per day: `main` advances roughly every 25–30 min during active hours.
- The e2e gate averages ~9 min on success (13.2 min on a representative executed run, 7.6 min of it `make e2e` proper; max observed 30 min).
  `e2e-gate` and `e2e-calico-gate` are both required checks.
- 18 of 51 recent branches needed ≥2 e2e runs (worst: 5); 10 of the last 50 e2e runs were cancelled mid-flight by a superseding push.
  A third or more of e2e compute is redo, not first-run validation.
- Contention is process files, not code: `docs/STATUS.md` appears in 542 of 702 commits (77%), `docs/plan/README.md` in 22%.
  No component source file is in the top-20 contention list, which also answers "should we split the monorepo": no, the conflict surface wouldn't move.
- The ruleset's `strict_required_status_checks_policy` is already `false`; the freshness requirement is process (CONTRIBUTING.md's re-check rule), not GitHub.
  The queue replaces the process rule's mechanism, not a GitHub setting.

The constraint is therefore not e2e duration but the multiplier on it: every merge invalidates every in-flight green PR, and re-validation costs a full gate cycle whose duration matches the inter-merge interval, so re-runs cascade.
Amortizing one combined run per batch removes the quadratic term.

## Phase 1: `merge_group` triggers (this PR)

All nine workflows behind required checks gain a `merge_group:` trigger: `e2e-test`, `e2e-calico`, `integration-test`, `unit-test`, `license-notices`, `manifest-validate`, `plan-hygiene`, `security-scan`, `status-lint`.

Why this is the whole change:

- Every workflow already uses the always-trigger + internal `changes` paths-filter + aggregate-gate pattern, so the gate contexts report on any event the workflow triggers on.
- dorny/paths-filter v4 supports `merge_group` natively: `base`/`ref` default to the event's commit hashes and detection runs via git against the already-present checkout (verified against the v4 README).
  Docs-only queue entries therefore stay cheap: the heavy legs skip exactly as they do on a docs-only PR.
- Every heavy job's condition has the shape `needs.changes.outputs.X == 'true' || push || workflow_dispatch`, so a `merge_group` run takes the path-filtered branch, the desired behavior.
- Concurrency groups key on `github.ref`; merge-group refs (`gh-readonly-queue/...`) are unique per queue entry, and `cancel-in-progress` stays scoped to `pull_request`.
- status-lint's per-PR isolation check reads `github.event.pull_request`; it is already gated `if: github.event_name == 'pull_request'` and simply skips on the queue run (the PR event already enforced it).

## Phase 2: activation (post-merge, ordering matters)

Enabling the queue before Phase 1 reaches `main` wedges every merge: the required checks would never report on `merge_group` refs.
After Phase 1 merges, add the rule to the existing `default-protect` ruleset (id 17350763):

```bash
gh api -X PUT repos/actions-gateway/github-actions-gateway/rulesets/17350763 \
  --input tmp/ruleset.json
```

where `tmp/ruleset.json` is the current ruleset body (GET it first) plus:

```json
{
  "type": "merge_queue",
  "parameters": {
    "merge_method": "SQUASH",
    "grouping_strategy": "ALLGREEN",
    "max_entries_to_build": 5,
    "max_entries_to_merge": 5,
    "min_entries_to_merge": 1,
    "min_entries_to_merge_wait_minutes": 5,
    "check_response_timeout_minutes": 60
  }
}
```

- `SQUASH` preserves the repo's squash-merge convention and satisfies the ruleset's `required_linear_history`.
- `min_entries_to_merge: 1` with a 5-minute wait lets batches form at the observed merge rate (~2 PRs per 5-min window at peak) without stalling a lone PR.
- `check_response_timeout_minutes: 60` clears the worst observed e2e run (30 min) with headroom for queue-time image-cache misses.

After activation, `gh pr merge --squash` enqueues instead of merging directly; a PR whose queue run fails is kicked back out with the failure on the PR, which is the self-heal signal pr-sentinel already reacts to.

Rollback: delete the `merge_queue` rule from the ruleset; behavior reverts to direct merges and the process rules in CONTRIBUTING.md.

## Phase 3: re-measure, then decide

### The re-measurement (2026-08-12)

Same three measurements as 2026-08-03, over the last 200 `e2e-test.yml` runs:

| Measurement | 2026-08-03 | 2026-08-12 | |
|---|---|---|---|
| Branches needing ≥2 e2e runs | 18 of 51 (35%), worst 5 | 23 of 57 (40%), worst 9 | worse |
| Runs cancelled mid-flight | 10 of 50 (20%) | 8 of 200 (4%) | much better |
| Median PR open→merge | — | 45 min (p25 22, p75 131, max 878) | |

The queue removed the cancellation churn it was adopted to remove, and did not touch per-branch redo, which got slightly worse.
The e2e volume split explains why: of those 200 runs, 116 (58%) were `pull_request`, 57 `merge_group`, 25 push, 2 dispatch.
Every one of the 116 was a second verdict on a commit the queue would validate again, and the branches that pushed repeatedly paid for it repeatedly.

Two supporting numbers, from the same window: e2e failed on 1% of runs (2 of 200), and CI execution accounts for only ~5% of PR open→merge latency.
Full workings in [ci-wait-time-analysis.md](ci-wait-time-analysis.md).

### The decision

**Adopted: demote e2e to merge-group-only** (both lanes), implemented by gating each heavy job on `github.event_name == 'merge_group'` so a pull request leaves it skipped and the `gate` job still reports its required check.
That removes 58% of e2e runs and takes the PR-side critical path from ~870s to ~630s (`integration-test`, the next longest).
The trade-off the candidate named is accepted on the 1% failure rate: an e2e break now costs a queue kickback on roughly 1 push in 100, against ~4 minutes saved on the other 99.

**Declined, all three, and not merely deferred again:** the STATUS.md-contention candidates (per-item backlog files, GitHub Issues + Projects, split status PRs + `pr-deps-gate`) all address the cost of a *kickback*, and the measured kickback driver was cancellation churn, which the queue already fixed at 20% → 4%.
Revisit only if that fraction climbs.

The candidate list as recorded on 2026-08-03:

- Demote e2e from per-PR to merge-group-only (halves e2e volume again; trade-off: sessions learn of an e2e failure only at queue time).
  **Adopted 2026-08-12.**
- `docs/STATUS.md` contention (77% of commits) still forces manual conflict resolution before a PR can enqueue; a per-item-file backlog format would remove it structurally, but is a large format change touching the backlog skill and lint, and only worth it if post-queue measurement shows it binding.
- GitHub Issues + a Projects board would also zero the conflicts, but trades them for a sync problem: today a Queue row is deleted in the same PR that ships the work (atomic with the code, reviewable in the diff, greppable in one file, and the bare-Q-ID anchor fabric across docs and code depends on it), while an issue close is a separate mutation that can drift from code state.
  Priority ordering is also weaker through `gh` (Projects v2 positions are drag-first, not CLI-first).
  Considered 2026-08-03; behind the same re-measurement, and per-item files in-repo rank ahead of it because they keep every property above.
- Splitting the STATUS.md edit out of a code PR into its own docs-only PR is the cheap half of the same idea.
  The backlog merge driver is local-only (`.gitattributes` maps `docs/STATUS.md` to `merge=backlog`; GitHub cannot run custom drivers), so the queue builds candidates with plain 3-way and an adjacent-row edit conflicts server-side even though it auto-resolves locally.
  A combined PR kicked back that way re-runs its e2e on the fixup push; a split status PR re-runs only the cheap path-filtered gates, and the code PR stays queued.
  Cost: GitHub has no native PR dependency ordering, so a completion row's deletion trails the code merge as a follow-up PR, with a short window where the row is open and no PR covers it.
  Same re-measurement decides; per-item files dominate this too, closing the conflict itself rather than routing around it.
- The ordering gap above has a buildable fix, parked 2026-08-03 behind the same re-measurement: a `pr-deps-gate` required check that reads `Depends-on: #N` from the PR body, reports green when no dependency is named, and red while any named PR is unmerged.
  The queue enforces the sequencing for free, since a PR cannot enqueue until its required checks pass; the one extra piece is a push-to-main workflow that re-requests the check on open PRs whose dependencies just merged, because GitHub emits no event to a dependent PR.
  This closes the no-covering-PR window (the status PR is open, just unqueueable) and makes the split machine-enforced.
  Build it only if the measured kickback rate says split PRs are worth having.
