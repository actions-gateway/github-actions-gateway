# CI speed: what is measured, and what has already been tried

Read this before scoping any work on CI or test-tier speed.
Three rounds have already run, and the traps below are the ones that cost time in each of them.
Every number here is dated and names where it came from; nothing is inferred from workflow source.

## Current state

Measured 2026-08-12 on PR #1416's head commit, a change touching Go, workflows and docs, so every tier ran:

| Check | Wall |
|---|---|
| `integration-test (agc)` | **392s** (the pole) |
| `integration-test (gmc)` | 342s |
| `unit-test` | 235s |
| `lint` | 213s |
| `coverage` | 186s |

The e2e lanes are absent because they no longer run on pull requests (Q675); their cost moved to the merge-queue entry, where it is paid once per merge instead of once per push.

Below `integration-test` the next three sit within ~50s of each other and **swap order between runs**: an earlier sample the same day had `lint` at 221s above `unit-test` at 200s.
That is a flat profile, not a queue behind one slow stage.
Cutting any one of them alone moves the workflow's wall clock by roughly nothing.

## Rounds already run

| Round | Scope | Outcome |
|---|---|---|
| [e2e-tests-speed.md](e2e-tests-speed.md) | Ginkgo parallelism, port-per-process, suite structure | Done, §1 to §18 |
| [e2e-ci-speed-round-2.md](e2e-ci-speed-round-2.md) | The image bake (~570s of contended compile) and the suite's `Serial` tail (150s, over half the suite wall) | Done, both poles shipped |
| [merge-queue.md](merge-queue.md) | Adopt the queue; Phase 3 then demoted e2e to merge-group-only | Done, all three phases |
| Integration matrix (#1416) | GMC and AGC ran back to back in one job | 617s to 392s, measured |

## Four traps, one per wasted attempt

1. **The critical path is `max()` across parallel workflows, not any one job.** `e2e` and `e2e-calico` were separate workflows on the same event, so they ran concurrently, and calico was the *longer* lane (870s against 804s) on ~70% of branches.
   A proposal to trim 104s of chart checks off the kindnet lane would have saved ~0, because calico simply became the pole.
   On #1402 the two lanes finished 6 seconds apart; on #1401 calico finished 375s after kindnet.
2. **A flat profile means there is no constraint, and the honest answer is "nothing here".** See the current-state table: three checks within 50s that reorder run to run.
   Manufacturing a target out of the nominally-largest one produces work that measures as zero.
3. **Do not derive suite parallelism from spec-time ÷ wall.** That ratio mixes compile, the parallel phase and the `Serial` tail, and reads about 2.77× against `--procs 6`, which looks like ~295s of headroom.
   Round 2 measured the parallel phase properly at **4.5×** and recorded it as healthy.
   There is no parallelism work left in the suite; the lever round 2 left open is a larger runner.
4. **CI execution is about 5% of PR open-to-merge latency.** Any round of this work has a low ceiling on the thing that actually matters, so size the effort accordingly.
   Details under [the latency split](#the-latency-split) below.

## What is left

1. **Larger e2e/CI runner.** The lever round 2 identified and deliberately left: both of its poles were CPU-bound on a 4-vCPU `ubuntu-latest` and both scaled close to linearly, and `vars.GAG_E2E_RUNNER` already routes the job when set, so it is a config decision rather than a code change.
   It is the one remaining item with real headroom, and it costs money on a public repo.
2. **Q808, the flat profile above.** Filed rather than acted on.
   If it is ever worth attacking, the target is whichever of the three is the pole *that week*, and `lint` carries 159s of golangci-lint.
3. **Re-derive the chart-check split before scoping it.** An earlier version of this doc listed splitting the kindnet lane's three chart checks (104s) into a parallel job, and scored it ~0 because calico ran beside it on PRs.
   That premise died with Q675: e2e is merge-queue-only now, so the question is whether 104s off a queue entry is worth a new job and a new required status check in a workflow with documented wedging history (Q363).
   Do not carry the old score forward.
4. **Narrowing the calico lane is declined, not pending.** It would take ~420s off that lane, but the 75 non-gated specs are the only proof the product still *functions* under real NetworkPolicy enforcement: kindnet accepts policy objects without dropping egress, so an allowlist too narrow for legitimate traffic passes kindnet and fails only under Calico.
   That is a live bug class for a secure-by-default repo.

## The measurements

### The latency split

Across the 12 most recently merged PRs, on 2026-08-12:

| Segment | Minutes | Share |
|---|---|---|
| CI actually executing | 195 | 5% |
| PR green, not yet landed | 2,226 | 59% |
| PR open, final push not yet made | 1,326 | 36% |
| **Total open → merge** | **3,747** | |

Median open→merge 45 min; p25 22, p75 131, max 878.
PR #1382 spent 4 minutes in CI and 875 waiting; #1385, 4 and 759.
Both single-push, no re-runs.

**The 59% segment is owned elsewhere and this doc proposes no mechanism for it.** It is being closed on the attention side rather than by relaxing the enqueue gate, so do not scope an auto-merge change from these numbers.
They are recorded because they set the scale of everything else here.

### Heavy-gate failure rates

Sampled over 100 runs each, 2026-08-12.
These are the inputs Q675's Phase 3 decision turned on, and they are what makes "pay a queue kickback instead of a PR-time failure" a good trade:

| Workflow | Failures | Rate |
|---|---|---|
| `e2e` | 1 | 1% |
| `integration-test` | 1 | 1% |
| `unit-test` | 5 | 5% |

### Five specs are CNI-gated, not four

`egressEnforcingCNI()` gates five specs, totalling 139.8s of spec time: the two `TenantProvisioning` egress negatives, the two `ManagerMetricsNP` specs, and `E2E_V2_DirectEgress_NonGitHubBlocked`.
Prose copies of that list in `testing.md` and `e2e-calico.yml` had both gone stale at four, missing the direct-egress spec from the v2 work; Q809 corrected both and made `egressEnforcingCNI()` the stated authority.
Grep the call sites rather than trusting any count, including this one.

`E2E_GMC_CrossTenantNetworkBlocked` looks like a sixth and is not: it asserts *ingress* blocking, which kindnet does enforce.

## Reproducing

    gh pr list --state merged --limit 40 --json number,createdAt,mergedAt
    gh pr view <n> --json statusCheckRollup
    gh api repos/{owner}/{repo}/actions/runs/<id>/jobs
    gh api repos/{owner}/{repo}/actions/artifacts/<id>/zip > report.zip

The per-job breakdown is what distinguishes a real pole from a flat profile; the JUnit artifact gives per-spec times but see trap 3 before dividing anything by it.
