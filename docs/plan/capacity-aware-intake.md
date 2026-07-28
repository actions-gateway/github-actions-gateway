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
| 0a | Port the shipped quota rung to the scale-set tier, as the integer form of the ladder | M | ✅ Done ([§9a](#9a-the-shipped-quota-rung-was-classic-only-q443)/[§9b](#9b-what-the-port-shipped), Q443, 2026-07-26) |
| 1 | `SchedulerVerdict` mode: gate on the scheduler's verdict, for clusters that cannot grow | M | ✅ Done ([§6](#6-phase-1--schedulerverdict-q405), Q405, 2026-07-27) — unvalidated on a live cluster ([§9](#9-validation-to-be-measured-not-asserted)) |
| 2 | `AutoscalerVerdict` mode: gate on the autoscaler's own declination, for elastic clusters | M | ✅ Done ([§7](#7-phase-2--autoscalerverdict-q406), Q406, 2026-07-27) — unvalidated on a live cluster ([§9](#9-validation-to-be-measured-not-asserted)) |
| 3 | `Probe`/`Provision` modes: `ProvisioningRequest` `check-capacity` | L | 💤 Deferred ([Q407](../STATUS.md#Q407), [Appendix G.16](../design/appendix-g-future-enhancements.md#g16-provisioningrequest-pre-acquire-capacity-probe-check-capacity)) |

Nothing here has been validated on a live cluster, items 0a, 1 and 2 included: each
carries an envtest proof of the mechanism, not a measurement of its effect. §9 is the
measurement step, and its numbers are the input to the Phase 3 decision; no
effectiveness claim belongs in this doc, or in public copy, before it exists.

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
  `Ceiling`/`QuotaExhausted`/`QuotaCapacity` in
  [`target.go`](../../cmd/agc/internal/provisioner/target.go), fail-open by
  contract like `QuotaExhausted`.
* **And its integer counterpart, in the same change.** Q443 established that a rung
  reaches the default acquisition tier only if it is also expressed in
  `AdvertiseCapacity` — as a bound on the advertised total, the way `QuotaCapacity`
  expresses rung 1 ([§9b](#9b-what-the-port-shipped)). A phase that ships only
  `CapacityDeclined` ships classic-only and inherits the exact defect Q443 fixed. The
  arithmetic is the same shape: `declined` ⇒ contribute a bound of the set's own
  in-flight worker pods (no room for more), otherwise no bound.
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
   rung's shape — **and the matching `AdvertiseCapacity` rung**, or the mode is
   inert on the default tier (§5, [§9b](#9b-what-the-port-shipped)). A
   `SchedulerVerdict` that only lands in `Admit` is not Phase 1 shipped, it is
   Phase 1 shipped to the deprecated tier.
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

## 7a. What phase 2 shipped

Shipped 2026-07-27. §7's scope landed as specified; four things were decided during
implementation that the spec did not settle, and each is a constraint on Q407 rather
than a local choice.

**The verdict is the newest relevant event, not the presence of a declination.** §7
specified two declination matchers and stopped there, which would have let a stale
`NotTriggerScaleUp` gate a set the autoscaler had since started scaling for — the
transient-declination risk §7 flagged as "unknown until measured", turned from a
measurement question into a correctness one. So the matcher also recognizes each
project's *acting* signal (CA `TriggeredScaleUp`, Karpenter `Nominated`) and returns
the class of the newest event by timestamp, with same-instant ties resolving open.
Ordering reads every timestamp field an Event may carry, because the two recorder
generations populate different ones and reading only `LastTimestamp` would sort every
new-style event at the zero time. This costs nothing — the events are already in hand —
and removes the failure mode outright rather than deferring it to §9.

**An unrecognized mode fails open, with its own reason.** Q405's condition writer
treated any non-`Off` mode as `SchedulerVerdict`, which was safe while the enum held
exactly those two values and is not once it holds three. The CRDs ship as their own
chart and can be upgraded ahead of the AGC, so a `Probe` selected against an AGC that
predates Q407 would have silently applied `SchedulerVerdict`'s semantics — on an
elastic cluster, precisely the tenant-starving outcome the mode split exists to
prevent. The mode dispatch is now an explicit switch whose default publishes
`WorkerCapacityDeclined=False/GateModeUnsupported`. **Q407 must add its arm to that
switch**, not rely on a fallthrough.

**A re-check interval, because nothing watches Events.** The gate's signal lives in
objects the AGC deliberately does not cache, so neither a declination arriving nor a
later scale-up would re-trigger a reconcile on its own. A set in `AutoscalerVerdict`
mode with any stuck pod therefore requeues every 30s while stuck. Both directions
matter and the second is the safety one: without it the gate would stay closed after
the autoscaler started acting, which is exactly what §9 step 3 checks for.

**The read budget is bounded and doubly scoped.** Events are read only for pods the
`WorkersUnschedulable` evaluation already found stuck past the scheduling grace
(carried out of that evaluation rather than re-derived, so the two cannot disagree),
oldest-first, field-selected server-side to one pod by name and UID, and capped at 8
pods per reconcile with the truncation stated in the condition message. A healthy set
costs zero reads. RBAC is `get`/`list` on core `events` only — **not** `watch`, which
§7 listed: there is no informer, and granting a verb the code does not use would be a
gratuitous widening.

**Not measured yet.** §9 owns the live validation and none of it has run for this mode.
Specifically unmeasured: the event's latency from pod creation (§9 step 4), which sizes
the condition's staleness bound and is the number that says whether 30s is the right
re-check; and how often CA emits `NotTriggerScaleUp` for a condition it resolves on its
next loop — the recency rule bounds the damage of that case but does not tell us how
common it is.

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

## 9a. The shipped quota rung was classic-only (Q443)

Found while qualifying the eviction-recovery claims for Q419 (shipped
2026-07-26) — same defect class, same public sentences, other half. Recorded here
rather than fixed there, because the two capabilities have different owners and
different remedies. The copy correction shipped 2026-07-26 (Q439); the port it
revealed is Q443, specified below and shipped the same day.

**Shipped 2026-07-26.** The port described below is in; what remains of this section
is the finding, the decision, and the design it settled on, which §6/§7 inherit. What
shipped is recorded in [§9b](#9b-what-the-port-shipped).

The rung this plan builds on — rung 1, live namespace-`ResourceQuota` headroom
checked *before* the claim — is wired into `Provisioner.Admit`, and `Admit` is
reached from two call sites: `AdmitFor` (v1 `RunnerGroup`) and the classic branch
of the v2 `RunnerSet` reconciler. `reconcileScaleSetListener` returns before that
wiring. What a `ScaleSet` set advertises instead is
`scaleSetCapacityFunc` → `X-ScaleSetMaxCapacity` → `target.Ceiling`: the set's
configured worker ceiling (max tier threshold, else `maxWorkers`, else the
default). That is the Q59 concurrency rung, not the quota rung. Nothing on that
tier consults quota headroom before a job is assigned.

So on the default acquisition tier a quota-blocked job **is** claimed. It then
falls to `createPodWithQuotaRetry`, the in-place backstop this plan's §1 calls
out as the failure mode the gate exists to prevent: the job lock is held across
up to `maxQuotaRetries × quotaRetryDelay`, and on exhaustion the pod is abandoned
with the lock held.

Two doc claims were wrong as a result, both in public copy. Both are corrected as
of 2026-07-26; recorded here because the second was a fabricated mechanism, not a
scope error, and that distinction should survive:

* *"won't claim a job it can't place … live quota headroom checked before the
  claim"* ([why-gag](../why-gag.md) comparison table; [README](../../README.md)
  "The Solution") — true on classic, not on the default tier. **Scoped**, not
  removed.
* *"if headroom is lost after the claim, auto lock-cancel + re-queue"*
  (why-gag, same row; also [runbook.md](../operations/runbook.md)) — no code path
  did this, on any tier. `createPodWithQuotaRetry` retries in place and abandons
  the pod on budget exhaustion; there is no rerun call. The
  lock-cancel-and-re-queue language was borrowed from the eviction path, which is
  itself classic-only. **Replaced** with what the code does.

`actions_gateway_jobs_admission_rejected_total` is emitted from the same classic
call site ([listener/job.go](../../cmd/agc/internal/listener/job.go)), so both its
`reason="quota"` and `reason="ceiling"` series read a flat zero on the default
tier — the same "healthy dashboard, lost jobs" shape Q419 found on the eviction
counters.

### The decision: port the rung, and treat it as a 2.0 gate (Q443)

Settled 2026-07-26, because the copy fix could not be written honestly without
it.

**Port it.** The mechanism is expressible on the scale-set tier and the cost of
not porting is losing a headline capability at `v2.0.0`.

Expressible, because `X-ScaleSetMaxCapacity` is not a free-slot delta but a
*total* ceiling — GitHub holds `totalAssignedJobs` at or below the advertised
value — so a quota-derived bound composes with the Q59 ceiling as a `min()` on
the integer `scaleSetCapacityFunc` already returns. Jobs beyond it stay queued
server-side, which is precisely the classic rung's outcome.

Three things make it more than a one-line change, and they are the design work
this phase owns:

* **Delta-to-total conversion.** Headroom answers "how many *more* pods fit";
  the advertisement wants a total. That is roughly `activeWorkers + headroom`,
  and the AGC's count of its own in-flight pods is not GitHub's
  `totalAssignedJobs` — the two diverge across an assignment the AGC has not yet
  provisioned. Under-advertising merely delays jobs; over-advertising reproduces
  today's claim-and-stall. Bias low, and measure the divergence.
* **Granularity loss.** Classic re-decides per delivered job. Scale-set decides
  once per long-poll, for the whole set. Recovery from a stale read is a poll
  interval, not a job.
* **Interaction with §6/§7.** The `SchedulerVerdict` and `AutoscalerVerdict`
  rungs are specified as per-delivery `Admit` rungs. If the scale-set tier gets
  a capacity-integer expression of rung 1, those two need the same treatment or
  they ship classic-only and inherit this exact defect on arrival. Design the
  integer path once, for all three rungs.

**Why it is a 2.0 gate.** Classic acquisition is removed in `v2.0.0`
([v2-ga.md](v2-ga.md#phase-3--the-coupled-removals)). Rung 1 exists only on
classic. So the removal deletes the pre-claim quota gate outright unless this
lands first — structurally identical to Q417 for eviction recovery, which cleared
the same risk on 2026-07-26, and until now undeclared. Two of the four capabilities the README
leads with were in this position. Tracked as Q443, labelled `2.0-gate` to match, and
shipped 2026-07-26 ([§9b](#9b-what-the-port-shipped)).

**What ships without waiting:** the copy correction. Every claim above is now
scoped to the classic tier, and the "auto lock-cancel + re-queue" sentence is
removed rather than qualified, since no code path implements it on any tier.

## 9b. What the port shipped

Shipped 2026-07-26. The three design questions §9a raised were answered as follows;
each answer is a constraint on Q405/Q406, not a local choice.

**One ladder, two shapes.** `Provisioner.AdvertiseCapacity(target, unboundedDefault)`
sits beside `Provisioner.Admit(target)` in
[`admission.go`](../../cmd/agc/internal/provisioner/admission.go), walks the same rungs
against the same `Target`, and returns a `CapacityAdvertisement` — the integer plus the
per-rung accounting that produced it. Both godoc comments state the invariant that
caused this bug: **a rung added to one and not the other ships to one tier.** §6/§7
therefore each land in both, which is the "design the integer path once, for all three
rungs" requirement discharged rather than deferred.

**Delta-to-total, biased low.** `Target.QuotaCapacity(ctx, max)` joins
`Ceiling`/`QuotaExhausted` with the same fail-open contract. The v2 adapter converts
observed headroom to a total as `own non-terminal worker pods + headroom`, capped at
`max`; v1 returns unbounded (it is terminal, and `Admit`'s boolean is its authoritative
form). The pod count, not GitHub's `totalAssignedJobs`, is deliberate and is the
bias-low choice §9a asked for: an assignment the AGC has not provisioned yet is inside
`totalAssignedJobs` but not inside the quota's `used`, so counting assignments would
over-advertise by exactly the in-flight gap.

**The integer arithmetic reuses the boolean's.** `QuotaHeadroomPods` binary-searches
`WorkerFootprint`/`QuotaHeadroomViolations` for the largest fitting count rather than
dividing headroom by a per-pod footprint. Division would have been a second
implementation of a multi-resource, multi-format comparison, free to drift from the
rung it is supposed to mirror; the search is exact, bounded by the caller's ceiling, and
`TestQuotaHeadroomPods_AgreesWithTheBooleanRung` asserts the two answers cannot
disagree.

**Observability.** `actions_gateway_scaleset_advertised_capacity` and
`actions_gateway_scaleset_capacity_withheld{reason}` are the tier's counterpart to
`jobs_admission_rejected_total{reason}`, which is structurally unreachable here — a
declined job is never assigned, so there is no rejected delivery to count. Gauges, not
counters, for the same reason, and every evaluated rung publishes an explicit zero each
poll so a series never freezes at its last non-zero reading. Both are dropped when the
set is deleted.

**Not measured yet.** §9 still owns the live validation, and none of it has run for this
rung; it is tracked as [Q462](../STATUS.md#Q462). Specifically unmeasured: the divergence between the AGC's pod count and GitHub's
`totalAssignedJobs` under burst (the bias-low margin), and whether one poll interval of
recovery latency is noticeable to a tenant whose quota frees up mid-burst. The envtest
proof asserts the mechanism (a quota-blocked job is never assigned, and assignment
resumes when headroom returns), not its behaviour at scale.

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
