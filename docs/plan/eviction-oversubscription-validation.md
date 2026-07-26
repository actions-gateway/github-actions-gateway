# Eviction and Oversubscription Validation

Turn the eviction-recovery and oversubscription capabilities from arguments
into measurements. Five experiments, sequenced by what blocks what.

Filed 2026-07-26 from
[issue #819](https://github.com/actions-gateway/github-actions-gateway/issues/819),
which proposed the five and asked for triage. This plan is the reviewed
version: two of the five changed shape under review, and the corrections are
recorded below with the evidence that produced them.

## Why this matters

Both capabilities are currently argued rather than measured. The recovery story
is design prose in [01-executive-summary.md](../design/01-executive-summary.md)
and the flows doc; the oversubscription story rests on `priorityTiers` doing
what the design says it does. The planned blog post
([go-to-market.md](go-to-market.md), "Recovering stuck Actions jobs after pod
eviction") can state the design but cannot state a result.

The one number available is confounded. Per
[scaleset-eviction-recovery.md](scaleset-eviction-recovery.md#the-baseline-number-is-confounded),
the U5 probe's ~9.5 minutes coincided with the job's 10-minute
`timeout-minutes` boundary, leaving lock TTL, GitHub's liveness detection, and
the workflow timeout indistinguishable. Any published latency figure needs a
clean run with no `timeout-minutes` set.

## What is already tracked

| Row | Relationship to these experiments |
|---|---|
| [Q396](../STATUS.md#Q396) | **Is** experiment 1. Already covers both tiers as of #815; only the retry-budget assertion is additive. |
| [Q417](../STATUS.md#Q417) | Hard prerequisite for the scale-set half of 1, and for 3 and 5. `ProvisionScaleSetWorker` is fire-and-forget, so scale-set evictions are never detected. |
| Q419 | **Shipped 2026-07-26.** The docs half of the same gap: every eviction-recovery claim now names the classic tier. Independent of these experiments, but it means the docs these experiments measure against no longer overstate the scale-set tier. |
| Q420 | **Shipped 2026-07-26**, ahead of Q417 and independently of it — the reap deadline came from a pod annotation, not a pod watch. Orphaned Running workers would otherwise have contaminated 3 and 5 by holding quota, which is exactly the idle-capacity signature those experiments measure. |
| [Q418](../STATUS.md#Q418) | Deferred, event-gated on experiment 1 attributing the delay. |

## Experiment 1: mid-job eviction latency, both tiers ([Q396](../STATUS.md#Q396))

Evict a worker mid-build with no `timeout-minutes` set; timestamp the kill and
GitHub's conclusion; assert the rerun fires and the per-run retry budget
decrements exactly once.

**Correction under review.** Issue #819 framed this as "extend Q396 to
scale-set". That already happened: #815 widened Q396's scope to both tiers on
2026-07-25, and
[scaleset-eviction-recovery.md](scaleset-eviction-recovery.md#phase-1-measure-the-real-baseline-gate)
Phase 1 records the same. The genuinely additive assertion is the retry budget
(the Q106 sharded-reservation invariant), which the row did not name.

- **Venue:** Tier-C on kind, per the row.
- **Proves:** the real eviction-to-conclusion latency, attributed to a mechanism.
- **Unlocks:** a defensible number in place of the confounded one, and the
  [Q418](../STATUS.md#Q418) trigger.
- **Blocked on** [Q417](../STATUS.md#Q417) for the scale-set tier only.

## Experiment 2: the node-drain path ([Q421](../STATUS.md#Q421))

Cordon and drain a node mid-job. Assert what the wrapper, the runner, the
provisioner, and GitHub each do.

**Correction under review, and the reason this experiment got more valuable.**
Issue #819 predicted "a measured zero" on the grounds that this is the good
path and [Q385](https://github.com/actions-gateway/github-actions-gateway/pull/747)'s
SIGTERM relay already covers it. Reading the code suggests the drain path does
not reach eviction recovery at all:

- `kubectl drain` uses the Eviction API, which **deletes** the pod. A deleted
  pod never lands in `PodFailed` with `reason: Evicted`; that phase comes from
  kubelet-initiated node-pressure eviction.
- [`waitForPodCompletion`](../../cmd/agc/internal/provisioner/completion.go)
  treats a pod that has vanished as `PodSucceeded`.
- [`provision`](../../cmd/agc/internal/provisioner/provisioner.go) calls
  `handleEviction` only on `phase == PodFailed && reason == "Evicted"`.

So on classic, a drained worker most likely reports its own terminal result via
the relay, the provisioner records success, and nothing reruns. On scale-set
there is no detection at all until [Q417](../STATUS.md#Q417).

This is a code reading, not a measurement, and
[testing.md](../development/testing.md#diagnosing-failures-measure-before-asserting-a-root-cause)
is explicit that a
symptom match is a hypothesis until the failing system is measured. The
experiment is what settles it. If it holds, the outcome is a recovery gap on the *graceful*
path, which is worth more than confirming a zero, and it feeds Q417's design:
the pod watch must cover deletion, not only terminal phase.

Assertions:

1. The wrapper relays SIGTERM and the runner reports its own terminal result
   ([relayTerminationSignals](../../cmd/worker/main.go)). The relay is
   tier-independent; the scale-set `run.sh` branch has the same PID-1 handling,
   so this experiment runs on both tiers today without Q417.
2. The report completes inside the grace period. The provisioner sets no
   `terminationGracePeriodSeconds`, so worker pods get the Kubernetes default
   of 30s unless a tenant overrides it in `podTemplate`. A runner that needs
   longer is truncated by SIGKILL and the case degrades to experiment 1's.
3. Whether the job requeues, and by what mechanism. Do **not** assume it does.
4. Classic only: whether the job lock is released without waiting out the
   lapse. Scale-set has no AGC-held per-job lock; the runner owns its session
   (see scaleset-eviction-recovery.md Phase 3, which fails to find a job-scoped
   credential on that tier), so this assertion does not port.

- **Not blocked.** Sized M rather than S, because a failed assertion 3 turns
  this into a design question about the delete path.

## Experiment 3: oversubscription demo ([Q423](../STATUS.md#Q423))

Configure `priorityTiers` so low-priority CI runs inside capacity reserved for
higher-priority work. Force preemption. Assert the preempted job recovers with
no human action.

- **Proves:** the central claim, that tiering is only safe because recovery is
  automatic.
- **Unlocks:** turns the payoff section of the write-up from an argument into a
  result.
- **Blocked on** [Q417](../STATUS.md#Q417). (Q420, the other contaminant, shipped 2026-07-26.)
- Preemption is kubelet-initiated, so unlike experiment 2 it does produce
  `PodFailed`/`Evicted` and does exercise `handleEviction` on classic.

## Experiment 4: quota gate under real pressure ([Q422](../STATUS.md#Q422))

Fill the namespace `ResourceQuota`, submit more jobs than fit, and assert they
stay queued server-side rather than claimed-and-stalled.

**Correction under review: this is cheaper than #819 assumed, and it splits.**
The "visible in metrics or Events" half is already instrumented.
`actions_gateway_jobs_admission_rejected_total{reason="quota"}` ships with
[#793](https://github.com/actions-gateway/github-actions-gateway/pull/793) and
is documented in
[§4.2](../design/04-operational-flows.md#42-job-execution-flow-agc) step 2a, so
asserting it needs no new plumbing.

- **Half A (envtest):** quota exhausted, `acquirejob` is skipped, the counter
  increments with `reason="quota"`, and the ceiling budget is untouched (the
  quota rung reserves nothing). Existing coverage is unit-level
  (`worker_quota_test.go`) plus the capacity integration suites; none of it
  exercises the skip under real contention.
- **Half B (Tier-C):** two AGC sessions on the same RunnerGroup, one without
  headroom, and the job is picked up by the sibling. This is the half that
  needs live GitHub redelivery and cannot be faked.
- **Not blocked.**

## Experiment 5: utilization delta ([Q424](../STATUS.md#Q424), deferred)

Same workload on dogfood, tiers off versus tiers on, occupancy measured over a
fixed window.

- **Proves:** the packing-density thesis directly, and it is the one number the
  whole argument is missing.
- **Deferred rather than queued.** It is blocked on Q417, and it additionally
  needs a dogfood workload stable enough for a fixed-window comparison to mean
  anything. With no owner-actionable next step today, a Queue position would be
  fiction.

## Sequencing

1. [Q421](../STATUS.md#Q421) (experiment 2) and [Q422](../STATUS.md#Q422)
   (experiment 4) are unblocked today. Q421 first: its result feeds Q417's
   detection design.
2. [Q417](../STATUS.md#Q417), the prerequisite for everything scale-set.
3. [Q396](../STATUS.md#Q396) (experiment 1), which then gates
   [Q418](../STATUS.md#Q418).
4. [Q423](../STATUS.md#Q423) (experiment 3), then revive
   [Q424](../STATUS.md#Q424) (experiment 5).

## Acceptance criteria

- A published eviction-recovery latency figure with the mechanism attributed,
  replacing the confounded U5 number everywhere it is cited.
- A recorded answer for the drain path: either it recovers, or the gap is filed
  and Q417's design covers deletion.
- The quota gate demonstrated under contention, with the rejection counter as
  the observable.
- Preemption recovery demonstrated end to end before the oversubscription claim
  is published.
