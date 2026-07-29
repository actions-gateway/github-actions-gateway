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
| Q417 | **Shipped 2026-07-26.** Was the hard prerequisite for the scale-set half of 1, and for 3 and 5: `ProvisionScaleSetWorker` is fire-and-forget, so scale-set evictions were never detected. Detection now runs from the owning reconciler off the worker pod ([scaleset-eviction-recovery.md § Phase 2 as built](scaleset-eviction-recovery.md#phase-2-as-built)). All three are unblocked. |
| Q419 | **Shipped 2026-07-26** with Q417 — the docs half of the same gap. The tier-agnostic claims in the exec summary, README, and why-gag are now true of both tiers rather than needing a qualification. Independent of these experiments. |
| Q420 | **Shipped 2026-07-26**, ahead of Q417 and independently of it — the reap deadline came from a pod annotation, not a pod watch. Orphaned Running workers would otherwise have contaminated 3 and 5 by holding quota, which is exactly the idle-capacity signature those experiments measure. |
| [Q418](../STATUS.md#Q418) | Deferred, event-gated on experiment 1 attributing the delay. |
| [Q459](../STATUS.md#Q459) | **Filed by experiment 2**, 2026-07-27. Its residual: neither tier recovers a drained worker, and whether that matters turns on what GitHub does with the runner's own relayed report — a Tier C question. |

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
- **Unblocked** — Q417 shipped 2026-07-26, so the scale-set tier now detects evictions and fires the rerun this experiment measures.

## Experiment 2: the node-drain path (Q421)

**Done 2026-07-27** — jump to the [Result](#result-measured-2026-07-27). The heading
keeps its original slug because several docs link to it.

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
the relay, the provisioner records success, and nothing reruns. Q417's scale-set
detection (shipped 2026-07-26) reaches the same conclusion by construction: it fires
only on `PodFailed`/`Evicted`, and deliberately excludes deletion on the grounds that
the SIGTERM relay already owns that case. This experiment is what tests that
reasoning.

This is a code reading, not a measurement, and
[testing.md](../development/testing.md#diagnosing-failures-measure-before-asserting-a-root-cause)
is explicit that a
symptom match is a hypothesis until the failing system is measured. The
experiment is what settles it. If it holds, the outcome is a recovery gap on the *graceful*
path, which is worth more than confirming a zero. Q417 shipped without covering
deletion, on the reasoning above; a finding here that a drained worker does **not**
report its own result is the evidence that would reopen that decision on both tiers.

Assertions:

1. The wrapper relays SIGTERM and the runner reports its own terminal result
   ([terminationRelay](../../cmd/worker/main.go)). The relay is
   tier-independent; the scale-set `run.sh` branch has the same PID-1 handling,
   so this experiment runs on both tiers.
2. The report completes inside the grace period. The provisioner sets no
   `terminationGracePeriodSeconds`, so worker pods get the Kubernetes default
   of 30s unless a tenant overrides it in `podTemplate`. A runner that needs
   longer is truncated by SIGKILL and the case degrades to experiment 1's.
3. Whether the job requeues, and by what mechanism. Do **not** assume it does.
4. Classic only: whether the job lock is released without waiting out the
   lapse. Scale-set has no AGC-held per-job lock; the runner owns its session
   (see scaleset-eviction-recovery.md Phase 3, which fails to find a job-scoped
   credential on that tier), so this assertion does not port.

### Result, measured 2026-07-27

**The code reading held, on both tiers: a drain reaches no eviction recovery
whatsoever.** The prediction is now a measurement, taken at two venues.

**envtest, both tiers** —
[`drain_eviction_test.go`](../../cmd/agc/internal/controller/integration/drain_eviction_test.go).
Each test drives the real `pods/<name>/eviction` subresource against a worker pod
that came out of the real provisioning path, carrying a complete run identity, and
asserts the rerun API is never called. The classic case wires the production
`InformerPodWaiter` rather than the poll fallback, because the two agree on a pod
that reaches a terminal phase and disagree on one that is deleted without ever
reaching one. Both pass. Each is the exact twin of the eviction test that *does*
fire a rerun on the same wiring, so the eviction/deletion distinction is isolated
to one substitution rather than argued.

**Tier B on kind** —
[`worker_drain_test.go`](../../cmd/gmc/test/e2e/worker_drain_test.go)
(`E2E_AGC_WorkerNodeDrain`). A real `kubectl drain` against the node holding a live,
AGC-watched worker pod. Recorded:

| Observation | Value |
|---|---|
| `kubectl drain` output | `node/… cordoned` then `evicting pod tenant-drain/runner-…` |
| Worker pod phase/reason sequence, sampled at 200 ms across the drain | `Pending/` — and then gone |
| `cluster-autoscaler.kubernetes.io/safe-to-evict` on the drained pod | `false` — the drain evicted it regardless |
| `rerun-failed-jobs` calls for the run, over 45 s after removal | `0` |

The pod never published *any* terminal phase, let alone `PodFailed`/`Evicted`: it
went from `Pending` straight to absent. Nothing either tier's detection reads ever
existed. Observing the rerun at this tier needed fakegithub to answer and record
the call at all, which it now does (`/control/reruns`); the spec first asserts the
AGC's `GITHUB_API_BASE_URL` points at fakegithub, so the absence it measures cannot
be an absence of instrumentation.

**The second finding, which the experiment was not looking for.** The AGC stamps
every worker pod `safe-to-evict: false`, Karpenter `do-not-disrupt`, and the
descheduler's prefer-no-eviction marker
([`defaults.go`](../../cmd/agc/internal/provisioner/defaults.go)) precisely so a
mid-job worker is not disrupted. `kubectl drain` honours none of them — they are
advisory to autoscalers and deschedulers, not to the Eviction API — and worker pods
carry no PodDisruptionBudget. So an operator draining a node is the one disruption
source that is *neither* deflected by the disruption-safety markers *nor* recovered
by eviction recovery. Every other disruption path is covered by one or the other.

**Answers to the assertions.**

- **3 (does the job requeue, and by what mechanism):** answered, and the answer is
  *not by the AGC, on either tier*. This is the load-bearing result.
- **1, 2 (does the relay get the runner's own report out inside the grace period):**
  **not answered here, and not answerable at this tier.** A Tier B worker running the
  real runner image exits by itself within seconds — fakegithub's synthetic payload
  is not a job the runner can execute — so the spec deliberately drains a
  scheduled-but-`Pending` worker instead, which makes the drain the unambiguous cause
  of the pod's removal but leaves no live container to signal. The relay itself is
  covered by the `cmd/worker` unit tests (Q385/Q445); what remains unmeasured is
  whether a *real* job, reported by a *real* runner during the grace period, ends up
  in a state GitHub will retry.
- **4 (classic lock release):** the AGC's `provision()` returns as soon as the pod
  vanishes — the waiter reports a deleted pod as `PodSucceeded` — so the AGC does not
  hold the job open. What GitHub does with that is the same open question as 1–2.

**Consequence: the gap is real but its severity is not yet established, so it is
filed rather than fixed.** Q417 scoped scale-set eviction detection to
`PodFailed`/`Evicted` on the stated reasoning that the SIGTERM relay already owns
the deletion case. That reasoning is now known to be load-bearing on both tiers, and
its premise — that the relay's report leaves the job recoverable — is exactly the
part still unmeasured. Extending both tiers to treat a graceful deletion as
recoverable would be the fix, but doing it before knowing what GitHub does with a
relayed cancellation risks auto-rerunning jobs that a human deliberately cancelled
(a `kubectl delete pod`, or a run cancelled in the GitHub UI, arrives on the same
path as a drain). [Q459](../STATUS.md#Q459) carries the Tier C measurement and the
decision that follows from it.

**Update, 2026-07-28.** Q459 took the first half of that measurement, and the premise
holds: a real runner interrupted mid-job gets its report out inside the grace period,
GitHub concludes the job `failure` 15s later, and `rerun-failed-jobs` returns `201`
with a second attempt that runs. It also corrected one thing this section infers. The
claim above that a deliberate cancel "arrives on the same path as a drain" is true of
`kubectl delete pod` but **not** established for a GitHub-UI cancel, which reaches the
runner over its own broker connection rather than through the pod — and a *running*
worker, unlike the `Pending` one drained here, publishes `PodFailed` with an empty
reason before its object is removed rather than vanishing without a terminal phase.
Details in [q459-drained-worker-recovery.md](q459-drained-worker-recovery.md).

Worth noting for that decision: the drain path is currently *worse* for the user
than the ungraceful one. A kubelet node-pressure eviction auto-reruns the job; a
graceful operator drain does not.

## Experiment 3: oversubscription demo ([Q423](../STATUS.md#Q423))

Configure `priorityTiers` so low-priority CI runs inside capacity reserved for
higher-priority work. Force preemption. Assert the preempted job recovers with
no human action.

- **Proves:** the central claim, that tiering is only safe because recovery is
  automatic.
- **Unlocks:** turns the payoff section of the write-up from an argument into a
  result.
- **Unblocked** — both contaminants cleared: Q420 and Q417 shipped 2026-07-26.
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

- **Half A (envtest) — done 2026-07-26.** Covered by
  [`q422_quota_admission_test.go`](../../cmd/agc/internal/controller/integration/q422_quota_admission_test.go),
  one test per tier the rung serves. Findings below.
- **Half B (Tier-C):** two AGC sessions on the same RunnerGroup, one without
  headroom, and the job is picked up by the sibling. This is the half that
  needs live GitHub redelivery and cannot be faked.
- **Not blocked.**

### Half A findings

Both tests drive a live listener against the broker/scale-set fakes with a real
`ResourceQuota` read through the manager's informer cache, and both were
mutation-checked: disabling the rung fails each of them.

- **Classic tier.** With the quota full, three deliveries in a row are skipped:
  `acquirejob` is never called (asserted on the broker stub's server-side call
  counter, not on the absence of a pod — "no pod appeared" would also pass for
  the claim-and-stall this rung exists to prevent),
  `..._admission_rejected_total{reason="quota"}` increments once per delivery,
  and no worker pod or per-job Secret is staged.
- **The ceiling budget is untouched.** `maxWorkers` is 1, so a single leaked
  reservation would close the gate permanently. Once headroom returns a job is
  claimed and a pod is built, and the `reason="ceiling"` series never moves.
  Mutating `Admit` to reserve a slot before refusing for quota fails exactly
  this assertion.
- **Scale-set tier** (the rung reached it in Q443, and Q450 corrected the
  footprint). The existing Q443 test covers the all-or-nothing case; the gap was
  the *partial* one, where `AdvertiseCapacity` converts a headroom delta into a
  total. Under a half-consumed quota the invariant `advertised + withheld ==
  declared ceiling` holds, which is the scale-set expression of "the quota rung
  reserves nothing".
- **A caveat on what envtest can show.** There is no resourcequota controller,
  so the tests own `status.hard`/`status.used` outright — which is what lets
  them fill a quota the way a busy namespace does (`hard − used`, the arithmetic
  the gate actually runs) rather than declaring a hard limit too small to ever
  fit a worker, as every prior envtest does. The flip side is that `used` does
  not rise as worker pods are created, so these tests cannot assert an
  assignment count across more than one poll.

## Experiment 5: utilization delta ([Q424](../STATUS.md#Q424), deferred)

Same workload on dogfood, tiers off versus tiers on, occupancy measured over a
fixed window.

- **Proves:** the packing-density thesis directly, and it is the one number the
  whole argument is missing.
- **Deferred rather than queued.** Q417 shipped 2026-07-26, but this still needs a
  dogfood workload stable enough for a fixed-window comparison to mean anything. With
  no owner-actionable next step today, a Queue position would be fiction.

## Sequencing

Q417 shipped 2026-07-26, so nothing here is blocked on it any more.

1. ~~Q421 (experiment 2)~~ — **done 2026-07-27**; see
   [Result](#result-measured-2026-07-27). Its residual is
   [Q459](../STATUS.md#Q459), which needs Tier C and so sequences with the other
   Tier C work below rather than ahead of it.
2. [Q422](../STATUS.md#Q422) (experiment 4).
3. [Q396](../STATUS.md#Q396) (experiment 1), which then gates
   [Q418](../STATUS.md#Q418). Fold [Q459](../STATUS.md#Q459) in around here: both
   want a real GitHub run interrupted mid-job, and Q396 is already standing that
   up.
4. [Q423](../STATUS.md#Q423) (experiment 3), then revive
   [Q424](../STATUS.md#Q424) (experiment 5).

## Acceptance criteria

- A published eviction-recovery latency figure with the mechanism attributed,
  replacing the confounded U5 number everywhere it is cited.
- ~~A recorded answer for the drain path: either it recovers via the SIGTERM relay, as
  Q417 assumed when it scoped detection to `PodFailed`/`Evicted`, or the gap is filed
  and both tiers are extended to cover deletion.~~ **Met 2026-07-27** by the second
  branch: neither tier recovers a drained worker, and the gap is filed as
  [Q459](../STATUS.md#Q459). Extending the tiers is deliberately left to that row —
  the same code path carries deliberate cancellations, so it needs the Tier C answer
  before it can tell a drain from a `kubectl delete pod` worth honouring.
- The quota gate demonstrated under contention, with the rejection counter as
  the observable.
- Preemption recovery demonstrated end to end before the oversubscription claim
  is published.
