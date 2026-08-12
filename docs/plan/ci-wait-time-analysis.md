# CI wait-time analysis

Measurements behind the question "how do we spend less time waiting for CI tests".
Every number here came from `gh` against this repo on 2026-08-12; nothing is inferred from workflow source.

## Status

Analysis complete, and Option 1 shipped.
Q675's re-measure ran on these numbers and demoted e2e to merge-group-only on both lanes; the decision and the re-measurement table live in [merge-queue.md](merge-queue.md#phase-3-re-measure-then-decide).
Options 2 through 4 remain open and un-started.
Finding 1's segment, the largest one, is owned by separate in-flight work and is not an option here.

## Finding 1: CI execution is 5% of PR latency

Across the 12 most recently merged PRs:

| Segment | Minutes | Share |
|---|---|---|
| CI actually executing | 195 | 5% |
| PR green, not yet landed | 2,226 | 59% |
| PR open, final push not yet made | 1,326 | 36% |
| **Total open → merge** | **3,747** | |

Median open→merge 45 min; p25 22, p75 131, max 878.
PR #1382 spent 4 minutes in CI and 875 waiting; #1385, 4 and 759.
Both single-push, no re-runs.

The gate is that enqueueing is the maintainer's action taken after review ([parallel-dispatch.md](../development/parallel-dispatch.md), Q692), combined with `allow_auto_merge=false` on the repo.
The landing decision therefore cannot be made until after CI is already green, which forces review latency and CI time into series.

`reviewDecision` is empty on every PR sampled and the ruleset requires no approving review, so the gate is the enqueue action itself, not a review step.

**This segment is owned elsewhere and this doc proposes no mechanism for it.** Closing it is in flight in separate work, on the attention side rather than by relaxing the enqueue gate, so do not scope an auto-merge change from the numbers above.
They are recorded here because they set the scale of every other finding: anything that shortens CI is working on the 5%.

## Finding 2: the critical path is `max(kindnet, calico)`

`e2e` and `e2e-calico` are separate workflows on the same PR event, so they run concurrently.
Calico is the longer lane and already skips the chart checks.

| Lane | Job | Test step | Chart checks |
|---|---|---|---|
| `e2e` (kindnet) | 804s | 468s | 104s |
| `e2e-calico` | 870s | 548s | disabled ([e2e-calico.yml](../../.github/workflows/e2e-calico.yml)) |

Calico runs on 21 of 30 sampled PR branches (70%).
Observed finish times:

- #1402: kindnet 03:24:24, calico 03:24:30, 6s apart
- #1401: calico finished 375s *after* kindnet
- #1405: kindnet 90s after calico

So removing the kindnet lane's 104s of chart checks saves ~0 on the 70% of PRs where calico runs, and ~50s per PR weighted across the split.

## Finding 3: suite parallelism is already tuned, do not re-open it

A naive read of the calico JUnit artifact gives 1,519s of spec time in 548s of wall, which looks like 2.77× against `--procs 6` and therefore like ~295s of lost parallelism. **That number is wrong as a parallelism figure** and no work should be scoped from it: the 548s step covers compile, the parallel phase, and the `Serial` tail, so dividing total spec time by it mixes three phases.

[e2e-ci-speed-round-2.md](e2e-ci-speed-round-2.md) already measured this properly.
The parallel phase does 646s of spec time in ~144s, i.e. **4.5× on `--procs 6` against 4 vCPUs**, and that round records it as "healthy and not worth further tuning".
It targeted the two real poles instead, the image bake (~570s of contended compile) and the `Serial` tail (150s, over half the suite wall), and shipped both.

Two speed rounds have already run here ([e2e-tests-speed.md](e2e-tests-speed.md), [e2e-ci-speed-round-2.md](e2e-ci-speed-round-2.md)).
The lever they left explicitly open is a **larger runner**: both poles are CPU-bound on a 4-vCPU `ubuntu-latest`, both scale close to linearly, and `vars.GAG_E2E_RUNNER` already routes the job when set, so it is a config decision rather than a code change.

CPU starvation on this runner is separately documented as the mechanism behind the Q300/Q747 kindnet flake family, which is why [e2e-reusable.yml](../../.github/workflows/e2e-reusable.yml) dumps `cpu.stat` CFS throttle ratios on failure.

## Finding 4: five specs are CNI-gated, not four

`egressEnforcingCNI()` gates exactly five specs:

| Spec | Spec time |
|---|---|
| `ManagerMetricsNP_DeniesUnlabeledNamespace` ([manager_np_test.go:106](../../cmd/gmc/test/e2e/manager_np_test.go)) | 40.9s |
| `V2_DirectEgress_NonGitHubBlocked` ([direct_egress_test.go:204](../../cmd/gmc/test/e2e/direct_egress_test.go)) | 41.8s |
| `TenantProvisioning_WorkloadEgressBlockedToNonProxyPod` ([provisioning_test.go:478](../../cmd/gmc/test/e2e/provisioning_test.go)) | 21.0s |
| `TenantProvisioning_WorkerCannotReachK8sAPI` ([provisioning_test.go:516](../../cmd/gmc/test/e2e/provisioning_test.go)) | 18.0s |
| `ManagerMetricsNP_AllowsLabeledNamespace` ([manager_np_test.go:123](../../cmd/gmc/test/e2e/manager_np_test.go)) | 18.1s |
| **Total** | **139.8s** |

`e2e-calico.yml`'s header comment lists only four.
It misses `V2_DirectEgress_NonGitHubBlocked`, added with the v2 direct-egress work, so the comment is stale regardless of what else changes here.

`E2E_GMC_CrossTenantNetworkBlocked` looks like a sixth but is not: it asserts *ingress* blocking, which kindnet does enforce, and carries its own FailOpen retry logic for the Q747 enforcer-restart case.

### Why narrowing the calico lane costs real coverage

Scoping calico to those five specs would cut its test step from 548s to roughly 150–250s.
But the other 75 specs are not redundant there: they are the only proof that the product still *functions* when NetworkPolicy is actually enforced.
Kindnet accepts policy objects without dropping egress, so a NetworkPolicy allowlist that is too narrow for legitimate traffic passes kindnet and fails only under Calico.

That is a real bug class for a repo whose security posture is secure-by-default, and narrowing the lane retires the only gate that covers it.

## Finding 5: Q675 is due, and these measurements are its inputs

Q675 deferred the merge-queue Phase 3 re-measure behind an event trigger of "~1 week of queue operation, on/after 2026-08-10".
That trigger fired, the re-measure ran on the numbers above, and the row is closed.
Its first recorded candidate was:

> Demote e2e from per-PR to merge-group-only (halves e2e volume again; trade-off: sessions learn of an e2e failure only at queue time).

That candidate is the one change on the list that directly targets session wait, because the session's blocking wait *is* the PR-side e2e.
Demoting it would take the PR-side critical path from ~870s (e2e-calico) to ~630s (integration-test, the next longest), and drop each branch's heavy-gate runs from two e2e cycles to one.

The measured failure rates below are what [merge-queue.md](merge-queue.md#phase-3-re-measure-then-decide) asks the re-measure to establish, so the decision no longer needs them re-derived:

| Workflow | Runs sampled | Failures | Rate |
|---|---|---|---|
| `e2e` | 100 | 1 | 1% |
| `integration-test` | 100 | 1 | 1% |
| `unit-test` | 100 | 5 | 5% |

At a 1% e2e failure rate, the trade the candidate names, paying a queue kickback instead of a PR-time failure, costs a full cycle on 1 push in 100 and saves ~4 minutes on the other 99.

## Options

Ordered by return per unit of risk.

1. ~~**Work Q675.**~~ **Shipped 2026-08-12.** Both e2e lanes are merge-group-only; the decision and the re-measurement table are in [merge-queue.md](merge-queue.md#phase-3-re-measure-then-decide).
2. **Larger e2e runner.** The lever round 2 left open, unchanged by this analysis.
   Elevate rather than exploit: it costs money on a public repo.
3. **Narrow the calico lane.** Takes ~420s off the longer lane, but retires the full-product-under-enforcement gate described in Finding 4.
   Not recommended without a replacement for that coverage.
4. **Split the kindnet chart checks into a parallel job.** Takes ~104s off the kindnet lane, but saves ~0 while calico is the critical path (Finding 2).
   Only worth doing once calico is shorter than kindnet, and it adds a job plus a required status check to a workflow with documented wedging history (Q363).

Not on the list: re-tuning `--procs` or the suite's parallel phase.
Finding 3 explains why.

## Reproducing

    gh pr list --state merged --limit 40 --json number,createdAt,mergedAt
    gh pr view <n> --json statusCheckRollup
    gh api repos/{owner}/{repo}/actions/runs/<id>/jobs
    gh api repos/{owner}/{repo}/actions/artifacts/<id>/zip > report.zip
