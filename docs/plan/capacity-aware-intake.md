# Capacity-Aware Job Intake

The admission ladder in
[`cmd/agc/internal/provisioner/admission.go`](../../cmd/agc/internal/provisioner/admission.go)
gates job acquisition on two rungs today: observed namespace-ResourceQuota
headroom (#784) and the owner's declared worker ceiling (Q59). Neither rung knows
whether the cluster can actually *place* the worker pod the job needs. A tenant
whose worker shape has become unplaceable (a drained GPU pool, a changed taint,
spot capacity gone) keeps claiming jobs, and each claim spends a single-use JIT
runner record, holds a GitHub job lock until `pendingPodDeadline`, and ends in a
reaped pod plus a cancelled workflow run. This plan closes that rung, in three
increments ordered cheapest-first, each independently shippable and off by
default.

The reason it takes three increments rather than one is that "can this pod be
placed" is not one question. The safe answer depends on **whether another actor
is waiting on the unplaceable pod to make capacity appear**, i.e. a cluster
autoscaler that would add a node. That principle, and why it makes the quota rung
safe to gate on while the scheduler's verdict is conditional, is recorded as
durable rationale in
[Appendix D §D.8](../design/appendix-d-alternatives-considered.md#d8-gating-intake-on-capacity-which-signals-are-safe-to-gate-on).

## Status at a glance

| # | Item | Sz | Status |
|---|---|---|---|
| 0 | Design rationale recorded (D.8 asymmetry principle, G.16 deferral + triggers) | S | ✅ Done — this change |
| 1 | `SchedulerVerdict` mode: gate on the scheduler's verdict, for clusters that cannot grow | M | ❌ Open ([Q405](../STATUS.md#Q405)) |
| 2 | `AutoscalerVerdict` mode: gate on the autoscaler's own declination, for elastic clusters | M | ❌ Open ([Q406](../STATUS.md#Q406)) |
| 3 | `Probe`/`Provision` modes: `ProvisioningRequest` `check-capacity` | L | 💤 Deferred ([Q407](../STATUS.md#Q407), [Appendix G.16](../design/appendix-g-future-enhancements.md#g16-provisioningrequest-pre-acquire-capacity-probe-check-capacity)) |

Nothing here has been validated on a live cluster. Phase 1 carries a measurement
step (§9) whose numbers are the input to the Phase 2/3 decision, and no
effectiveness claim belongs in this doc before that measurement exists.

---

## 1. The gap

`WorkersUnschedulable` (Q157 for v1, Q303 for v2) already reads the scheduler's
verdict off worker pods:
[`runnergroup_unschedulable.go`](../../cmd/agc/internal/controller/runnergroup_unschedulable.go)
computes it from `PodScheduled=False`/`Reason=Unschedulable`, and
[`runnerset_capacity.go`](../../cmd/agc/internal/controller/runnerset_capacity.go)
publishes the v2 counterpart. It is pure observability: it reaches a condition, an
Event, and (for v1 only, per [Q319](../STATUS.md#Q319)) a gauge, and never
reaches `Provisioner.Admit`.

So the intake path has no capacity awareness at all, and the per-claim cost of
that is:

* one single-use JIT runner record spent (Q114), unrecoverable,
* one GitHub job lock held from `AcquireJob` until the pod is reaped,
* up to `pendingPodDeadline` of wall-clock latency for that job, and
* a **cancelled** workflow run rather than a redelivered one, because a lock that
  lapses without renewal cancels.

That cost repeats per delivered job, so it scales with burst size. The existing
backstops (`pendingPodDeadline` plus the reaper, with `WorkersUnschedulable`
warning at half the deadline) bound the damage *per job*; nothing bounds the
number of jobs.

## 2. Why build it rather than only document it

**No CI runner controller does capacity-aware intake.** ARC's
[#3647](https://github.com/actions/actions-runner-controller/issues/3647) asks
for a *longer wait after claiming*, never for fewer claims. GitLab documents
over-claiming as intentional (`poll_timeout`: "Use this setting for queueing more
builds than the cluster can handle at a time"). Kueue does consume
`ProvisioningRequest`, but as an `AdmissionCheck` below the claim boundary, and it
needs cluster-admin ([§D.5](../design/appendix-d-alternatives-considered.md#d5-kueue-and-kubernetes-job-queue--quota-managers)).

**GAG is structurally positioned to do it and ARC is not.** The decision point
has to exist before the claim, and in GAG it already does: the admission gate is
called at
[`job.go:50`](../../cmd/agc/internal/listener/job.go), before `AcquireJob`. ARC
has no equivalent seam, because the runner process itself claims the job. This is
the same shape of durable differentiator as worker right-sizing
([§D.7](../design/appendix-d-alternatives-considered.md#d7-worker-right-sizing-why-built-in-not-bolted-on)):
a capability that must live inside the control plane that owns the boundary.

**The marginal cost is low because #793 built the machinery.** The rung pattern,
the `reason`-labelled rejection metric, the fail-open `Target` contract, the
condition/gauge plumbing, and the operator-doc pattern all exist. Phase 1 adds a
third rung to a ladder that already has two.

**Where a tenant feels it.** Two shapes, and they are the shapes GAG targets:
GPU/spot tenants, where one wrongly claimed job costs a cancelled run on
expensive hardware, and burst tenants, where a fan-out of *N* jobs currently
becomes *N* wasted claims.

**The honest bound on the value.** None of the three phases eliminates the first
wasted claim, and none removes the need for `pendingPodDeadline` and the reaper.
Phases 1 and 2 bound the *rate* of wasted claims (§5, the trickle property);
phase 3 removes most of the claims but buys a dependency. That bound is the
reason this is worth doing incrementally rather than as one large feature.

## 3. What changes on a fixed-size cluster

An on-prem cluster with a contracted node count is not a degenerate case of the
elastic one, it is the case where **the complexity mostly collapses**, and it is
a core deployment shape for self-hosted runners, so the plan is ordered around
it.

With no autoscaler running, no actor is waiting on the unplaceable pod. The
Pending pod is not a request for anything; it is pure waste. The rung the
[issue](https://github.com/actions-gateway/github-actions-gateway/issues/785)
rejected as "simplest and wrong" (gate directly on `WorkersUnschedulable`) is
simply **right** there, and it needs no new signal, no new CRD, and no new
dependency, because the condition is already computed in the tree.

The corollary matters for the other two phases: on a fixed-size cluster,
`AutoscalerVerdict` would never fire (no autoscaler, no event) and
`Probe`/`Provision` would fail open (no CRD, nothing to answer), so both degrade
to today's behavior. A plan that shipped only those two would deliver **nothing**
to the fixed-size deployment. Hence the ordering below.

Two nuances worth keeping straight:

* **"Fixed-size" is a property of the cluster, not of the signal.** Soundness
  therefore has to be established out of band. This plan takes it as an operator
  assertion (choosing the mode *is* the assertion), not an auto-detection: a
  wrong auto-detection starves a tenant, and the operator knows their node
  contract. §10 keeps a detected-autoscaler *warning* as an open question.
* **An elastic cluster at its contracted ceiling reports itself.** Cluster
  autoscaler's declination message includes `max total nodes in cluster reached`
  and per-pool `max node group size reached`, so the on-prem-with-a-bounded-CA
  case is covered by Phase 2's mechanism without asserting anything.

## 4. What changes when RunnerSets target different node pools

Placeability is a property of the pod shape, so a verdict is only ever valid for
the shape that produced it: a RunnerSet pinned to `gpu-a100` being unplaceable
says nothing about one on `cpu-standard`.

The design gets this right by construction rather than by extra machinery,
because **the verdict is keyed to the RunnerSet**, and a RunnerSet resolves to
exactly one worker template at a time (its own `templateRef`, or the gateway or
cluster default it inherits, §H.4), and therefore one pod shape and one set of
pools that shape can land on. Each set computes its condition from its own
worker pods (`LabelRunnerSet`), so a drained GPU pool gates the GPU sets while CPU sets keep
claiming. Per-pool behavior falls out of per-object keying; there is no need for
the per-template granularity the issue proposed, and no cluster-wide gate to
blast-radius.

Consequences to record:

* **Quota cannot substitute for this rung on a multi-pool cluster.** Namespace
  `ResourceQuota` is namespace-wide and pool-blind: it answers "does the
  namespace have room", not "does the pool this shape needs have room". On a
  contracted on-prem cluster with separate CPU and GPU pools, those two answers
  routinely disagree, which is exactly the gap Phase 1 closes.
* **Residual: a shared, genuinely full pool.** Every set targeting it gates and
  trickles independently, so aggregate wasted claims scale with the number of
  gated RunnerSets (≈1 per set per deadline window) rather than with burst size.
  A large improvement, not a total one. Cross-set coordination is a non-goal
  (§10); it would need a namespace-level shared verdict and is not worth the
  coupling until measured.
* **The autoscaler's message is per-pool and worth surfacing.** CA aggregates
  rejection reasons per node group ("2 node(s) had untolerated taint", "1 max
  node group size reached"), which is what makes the Phase 2 condition
  *actionable* rather than merely true. Put it in the condition message.

## 5. Design shared by all phases

**One API field, one enum, additive across phases.** `RunnerSet.spec.capacityGate.mode`:

| Value | Meaning | Phase |
|---|---|---|
| `Off` | Default. No capacity rung; today's behavior exactly. | — |
| `SchedulerVerdict` | Gate when the scheduler has declared this set's worker pods unplaceable. Sound only where no autoscaler will act on them. | 1 |
| `AutoscalerVerdict` | Gate when the cluster autoscaler has itself declined to scale up for them. | 2 |
| `Probe` / `Provision` | Ask before claiming, via `ProvisioningRequest` `check-capacity` / `best-effort-atomic-scale-up`. | 3 |

A single growing enum avoids a second field when a later phase lands, and makes
the choice mutually exclusive, which it is. v2 only: v1 `RunnerGroup` is terminal
(Q273/Q264), so its adapter gets a no-op method.

**One rung, one condition, one metric label.**

* `Target.CapacityDeclined(ctx) (declined bool, detail string)` joins
  `Ceiling`/`QuotaExhausted` in
  [`target.go`](../../cmd/agc/internal/provisioner/target.go), fail-open by
  contract like `QuotaExhausted`.
* `Admit` gains a third rung, ordered **after** quota and **before** the ceiling
  (like quota it reserves nothing, so the reservation arithmetic is untouched),
  rejecting with a new `runnercore.AdmitReasonCapacity = "capacity"` on the
  existing `actions_gateway_jobs_admission_rejected_total{reason}` metric.
* A new `WorkerCapacityDeclined` condition in
  [`api/apiconditions/conditions.go`](../../api/apiconditions/conditions.go) with
  one-line re-exports per version (`check-v2-api-sync.sh` gates every shared v2
  file, and `conditions_test.go`'s exhaustive name list must be extended).
  Reasons name the source: `PodsUnschedulable`, `ScaleUpDeclined`,
  `CapacityUnavailable`, cleared by `CapacityAvailable`.
* **Not** added to `ImpairingConditionTypes()`: `WorkersUnschedulable` is already
  in that rollup and is derived from the same underlying fact, so adding this one
  would double-count into the GMC's `RunnerSetsDegraded` summary (Q304).

A separate condition rather than gating on `WorkersUnschedulable` itself, for
three reasons: it means something different to an operator ("intake is being
refused" versus "pods are stuck"), it stays stable across all three phases while
the source underneath it changes, and `WorkersUnschedulable` is already an
impairing rollup input, so overloading it would tangle the gateway-level Degraded
summary with an intake decision.

**The trickle property, and why gating stays safe.** The gate reads a condition
that is *derived from the existence of a stuck pod*, which makes it
self-clearing and self-probing: condition True stops intake, the reaper deletes
the pod at `pendingPodDeadline`, the condition clears, one job is claimed, and if
capacity is still absent the new pod trips it again. A burst of *N* wasted claims
becomes roughly one per deadline window, and a Pending pod is present for much of
that window, so an autoscaler (if any) keeps being asked. This is what makes even
the Phase 1 mode non-suppressing in practice, and it is also the recovery
mechanism on a fixed-size cluster, where capacity returns silently when in-flight
jobs finish. **It must be asserted by a test, not assumed.**

**Fail-open everywhere.** Mode `Off`, an unreadable owner, an unresolved template
chain, an unreadable pod list, an absent autoscaler, a missing CRD: all yield
`declined=false`, leaving `ceilingCheck` and the `pendingPodDeadline` reaper as
the backstops. The gate may under-gate freely (that is today's behavior); it must
never over-gate, because over-gating starves a tenant.

## 6. Phase 1 — `SchedulerVerdict` (Q405)

The gateable signal already exists as a published condition, so this phase is
mostly API surface and wiring.

**Scope**

1. `spec.capacityGate.mode` on `RunnerSet` (`Off`|`SchedulerVerdict`), with the
   remaining values reserved and documented as not-yet-implemented; CRD +
   deepcopy + the two chart copies + the GMC-bundled copy (see
   [reference](../development/code-generation.md)).
2. `WorkerCapacityDeclined` condition + reasons (§5), computed alongside the
   existing evaluations in `applyWorkerCapacityConditions`
   ([`runnerset_capacity.go`](../../cmd/agc/internal/controller/runnerset_capacity.go)),
   reusing `evalWorkersUnschedulableForPods` rather than re-deriving. Skipped
   entirely when mode is `Off` so there is no cost for the default.
3. `runnerSetTarget.CapacityDeclined` reading the condition off the cached
   RunnerSet (a cheap per-delivery read, matching `Ceiling`/`QuotaExhausted`);
   `runnerGroupTarget.CapacityDeclined` returning false.
4. The `Admit` rung + reason constant + the Debug log line, mirroring the quota
   rung's shape.
5. Field doc **and** operator doc stating the soundness precondition in the same
   words: this mode assumes no autoscaler will act on the pods, and an elastic
   cluster wants `AutoscalerVerdict` instead.

**Tests.** Unit: the rung's ordering and fail-open paths in
`admission_internal_test.go`; the mode-`Off` no-op; the trickle cycle
(gate closes, pod reaped, condition clears, exactly one claim admitted, gate
closes again). Integration (envtest, `cmd/agc/internal/controller/integration/`):
a Pending worker pod with `PodScheduled=False/Unschedulable` produces the
condition, and the admission metric increments with `reason="capacity"`.

**Docs.** `03-api-contracts` (field), `04-operational-flows` (the ladder now has
three rungs), `operations/observability-metrics` (the new reason label value),
`operations/troubleshooting` (what an operator sees, why claims stopped, how to
turn it off), plus the website positioning page in the same PR per the standing
rule.

**Estimate.** ~450–700 lines net across ~25 files, 1–2 PRs, Sz M. The v2 gauge
for the new condition is deliberately out of scope: exporting v2 RunnerSet
capacity conditions as gauges is already tracked as
[Q319](../STATUS.md#Q319) and should land once for all of them.

## 7. Phase 2 — `AutoscalerVerdict` (Q406)

Same rung, different source: the autoscaler's own declination, read from Events
on the stuck pod. Verified upstream 2026-07-25:

* **Cluster autoscaler** emits `Normal NotTriggerScaleUp`, "pod didn't trigger
  scale-up: `<per-node-group reasons>`", from
  `cluster-autoscaler/processors/status/eventing_scale_up_processor.go`, only when
  a loop concluded *without* attempting a scale-up. GKE's autoscaler is
  CA-derived and emits it, so this is verifiable on the existing dogfood cluster
  with no new node-pool configuration.
* **Karpenter** emits `Warning FailedScheduling`, "Failed to schedule pod,
  `<err>`", from `pkg/controllers/provisioning/scheduling/events.go`, plus
  `NoCompatibleInstanceTypes` on the NodePool. **`FailedScheduling` is also
  kube-scheduler's own reason**, so the discriminator must be the reporting
  controller, never the reason string alone.

**One matcher per autoscaler project, not per cloud provider, and no plugin
interface.** The obvious worry is a combinatorial integration surface, and the
counts say otherwise, because both projects emit their events from shared core
code that every provider vendors (verified 2026-07-25):

| Project | Provider implementations | Event vocabularies |
|---|---|---|
| cluster-autoscaler | 36 dirs under `cloudprovider/`, ~30 real clouds (the rest are `builder`/`mocks`/`test`/`kubemark`/`kwok`) | 1 |
| Karpenter | 16 listed in the `kubernetes-sigs/karpenter` README (AWS, Azure, GCP, IBM, OCI, Cluster API, Alibaba, Hetzner, Proxmox, Linode, …) | 1 |

Searching either organization's provider code for those event reasons returns
zero hits: CA emits from `processors/status/eventing_scale_up_processor.go` and
Karpenter from `pkg/controllers/provisioning/scheduling/events.go`. So ~46
providers collapse to two string matchers. The managed offerings are not a third
vocabulary either: GKE's autoscaler is CA-derived, EKS runs upstream CA or
Karpenter, AKS runs CA or Node Auto Provisioning (which is Karpenter), and
OpenShift's MachineAutoscaler wraps CA.

A provider abstraction is therefore not warranted, and the distinguishing rule is
worth stating because this repo has legitimately built one before: **pluggable
backends earn their keep when the *behavior* differs per provider, not when only
the recognized input differs.** Q245's `--fqdn-policy-backend=cilium|calico|gke`
needed real backends because each CNI requires emitting a different CRD. Here
every autoscaler yields the same boolean and the same action, so a registry plus
an interface plus a config field would be pure overhead over two `case` arms.

**The proprietary tail is handled by the fail-open contract, not by research.**
Commercial optimizers (CAST AI, Spot Ocean, Zesty) may emit their own events or
none, and cannot be enumerated. An unrecognized autoscaler produces no match,
which yields `declined=false`, which is exactly today's behavior. That asymmetry
is the safety argument: a missed match costs nothing, a wrong match starves a
tenant, so the matcher stays deliberately narrow and broad coverage is explicitly
not a goal. If demand for a specific proprietary autoscaler ever appears, the
extension point is data rather than code (an operator-settable list of extra
`(reason, reportingController)` pairs, following the
`--allowed-infra-priority-classes` allowlist pattern, ~20 lines and no per-vendor
code). Documented here as an extension point; not built until asked for.

**Scope.** A matcher over pod Events (new
`cmd/agc/internal/controller/autoscaler_verdict.go`), read with a live
(uncached) client **only** for pods already Pending past the scheduling grace and
only when mode is `AutoscalerVerdict`, so there is no event informer and no
steady-state cost; the aggregated reason text into the condition message; `events`
`get;list;watch` added to
[`doc.go`](../../cmd/agc/internal/controller/doc.go)'s markers **and** the
chart's hand-maintained `charts/actions-gateway/files/agc-*-rules.yaml`.

**Tests.** Table-driven matcher unit tests: the CA message matches, the Karpenter
message matches, a `FailedScheduling` from `default-scheduler` does **not** match,
no events and unreadable events fail open. Plus the envtest counterpart stamping
a real Event.

**Estimate.** ~350–550 lines net across ~12 files, 1 PR, Sz M.

**Risks.** Events expire (`--event-ttl`, commonly 1h), which is acceptable: the
signal is only consulted inside the `pendingPodDeadline` window and absence fails
open. `max total nodes in cluster reached` is a cluster-wide verdict that will
trip every set at once; correct (no node is coming) but worth calling out in the
operator doc. Unknown until measured: how often CA emits the event for a
*transient* condition it would have resolved on its next loop.

## 8. Phase 3 — `Probe`/`Provision` (Q407, deferred)

Full cost, correctness landmines (the `PodTemplate` object each `podSet` must
reference, booking/`BookingExpired` semantics, the `CapacityAvailable` vs
`Provisioned` condition-name drift, provider support), and the triggers that
would revive it are recorded in
[Appendix G.16](../design/appendix-g-future-enhancements.md#g16-provisioningrequest-pre-acquire-capacity-probe-check-capacity).
Not duplicated here.

The decision rule: build it only when at least one of (a) Phase 1/2 measurement
shows residual burn that rate-bounding cannot make acceptable (the GPU/spot case,
where even one wasted claim per window is expensive), (b) an operator asks with a
cluster that already runs CA with provisioning requests enabled, or (c) provider
support broadens enough that the fail-open path stops being the common one.
Phases 1 and 2 are not throwaway work under any of those: phase 3 swaps the
rung's data source from an observed verdict to a solicited answer and reuses the
field, the condition, the metric label, the trickle, and the fail-open contract
unchanged.

## 9. Validation (to be measured, not asserted)

On the GKE dogfood cluster, per phase:

1. Induce unplaceability with a template the cluster cannot satisfy (a
   `nodeSelector` for a nonexistent pool, or a CPU request beyond any machine
   type). Dispatch a burst of *N* jobs.
2. Count wasted claims as
   `actions_gateway_worker_pods_reaped_total{reason="pending_deadline"}` over the
   burst, which is exactly one per claim that could not be placed. Compare
   `mode: Off` against the phase's mode; with the mode on, the drop should be
   accounted for by
   `actions_gateway_jobs_admission_rejected_total{reason="capacity"}`, and the
   residual should be ≈1 per deadline window (the trickle). Both counters are on
   the admission/reaper paths, not on job completion.
3. **The check that actually matters:** with a shape the autoscaler *can*
   satisfy, confirm a scale-up still happens and the jobs still run, i.e. the
   gate did not suppress the trigger. A pass on step 2 with a fail here is a
   regression, not a win.
4. Measure the event's latency from pod creation (Phase 2) to size the condition's
   staleness bound honestly in the operator doc.

Q399 (most dogfood jobs started but never completed) would have perturbed burst
runs. It is fixed: the tenant moved off the Classic protocol to a single-label
ScaleSet. The counters above come from the admission path rather than from
completions, so they were never blocked by it either way. Detail:
[gke-dogfood B7](gke-dogfood.md#b7-create-the-v2-tenant-objects).

## 10. Non-goals

* Predicting schedulability in-process from node allocatable. Rejected: it
  reimplements the scheduler's filter plugins and will drift
  ([D.8](../design/appendix-d-alternatives-considered.md#d8-gating-intake-on-capacity-which-signals-are-safe-to-gate-on)).
* Pre-warmed placeholder/pause pods. That trade (idle compute for correct
  optimism) is the warm-worker-pool item,
  [G.12](../design/appendix-g-future-enhancements.md#g12-warm-worker-pool-minidleworkers)/[Q268](../STATUS.md#Q268).
* Cross-RunnerSet coordination on a shared pool (§4 residual).
* A pluggable autoscaler-provider interface, or coverage of every autoscaler.
  Two matchers cover both open-source event vocabularies across ~46 provider
  implementations, and fail-open covers the rest (§7).
* Auto-detecting cluster elasticity to pick a mode. Open question, not scope: a
  reconcile-time *warning* when a mode's precondition looks violated (an
  autoscaler is present but the set asked for `SchedulerVerdict`) is cheap and
  would prevent the one real footgun here.
* Making any mode the default, in any phase. `Off` stays the default until live
  measurement says otherwise, per the secure/conservative-default stance.
* v1 `RunnerGroup` support.
