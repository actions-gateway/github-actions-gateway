# Capacity-Aware Job Intake

> **Status: ✅ Complete (2026-07-31).** Every build item (0a–2d) and every measurement row (V1–V3) is done.
> The one residual is item 3 — the `ProvisioningRequest` probe — deferred as [Q407](../STATUS.md#Q407) with its triggers in [Appendix G.16](../design/appendix-g-future-enhancements.md#g16-provisioningrequest-pre-acquire-capacity-probe-check-capacity); [§9h](#9h-what-the-dogfood-re-run-measured-for-the-latch-q513) supplies the number its trigger (a) reads against (~1 probe claim per window).

The admission ladder in [`cmd/agc/internal/provisioner/admission.go`](../../cmd/agc/internal/provisioner/admission.go) gates job acquisition on two rungs today: observed namespace-ResourceQuota headroom (#784) and the owner's declared worker ceiling (Q59).
Neither rung knows whether the cluster can actually *place* the worker pod the job needs.
A tenant whose worker shape has become unplaceable (a drained GPU pool, a changed taint, spot capacity gone) keeps claiming jobs, and each claim spends a single-use JIT runner record, holds a GitHub job lock until `pendingPodDeadline`, and ends in a reaped pod plus a cancelled workflow run.
This plan closes that rung, in three increments ordered cheapest-first, each independently shippable and off by default.

The reason it takes three increments rather than one is that "can this pod be placed" is not one question.
The safe answer depends on **whether another actor is waiting on the unplaceable pod to make capacity appear**, i.e. a cluster autoscaler that would add a node.
That principle, and why it makes the quota rung safe to gate on while the scheduler's verdict is conditional, is recorded as durable rationale in [Appendix D §D.8](../design/appendix-d-alternatives-considered.md#d8-gating-intake-on-capacity-which-signals-are-safe-to-gate-on).

## Status at a glance

| # | Item | Sz | Status |
|---|---|---|---|
| 0 | Design rationale recorded (D.8 asymmetry principle, G.16 deferral + triggers) | S | ✅ Done — this change |
| 0a | Port the shipped quota rung to the scale-set tier, as the integer form of the ladder | M | ✅ Done ([§9a](#9a-the-shipped-quota-rung-was-classic-only-q443)/[§9b](#9b-what-the-port-shipped), Q443, 2026-07-26) — effect measured (V1) |
| 1 | Gate on the scheduler's verdict, for clusters that cannot grow | M | ✅ Done ([§6](#6-phase-1--the-scheduler-verdict-signal-q405), Q405, 2026-07-27) — effect measured null on ScaleSet (V2) |
| 2 | Gate on the autoscaler's own declination, for elastic clusters | M | ✅ Done ([§7](#7-phase-2--the-autoscaler-declination-signal-q406), Q406, 2026-07-27) — effect measured null on ScaleSet (V2) |
| 2a | Split the mode enum's two axes: tenant policy on the set, cluster fact on the gateway | S | ✅ Done ([§5a](#5a-the-single-enum-was-two-axes-q470), Q470, 2026-07-27) |
| 2b | Assert phase 2's matcher against a real autoscaler's events, in kind | M | ✅ Done ([§9c](#9c-the-live-autoscaler-harness-and-what-it-measured-q474), Q474, 2026-07-28) — cluster-autoscaler only (V3) |
| 2c | Stop one loop's two verdicts from gating: the concurrency window | S | ✅ Done ([§9d](#9d-the-concurrency-window-q478), Q478, 2026-07-28) |
| 2d | The latch: a bound that survives the reap, with a probe slot on both tiers | M | ✅ Done ([§9g](#9g-the-latch--a-bound-that-survives-the-reap-q512), Q512, 2026-07-30) — effect measured (V2 re-run) |
| V1 | Measure item 0a's effect — the scale-set quota rung — on dogfood | M | ✅ Measured ([§9f](#9f-what-the-dogfood-run-measured-for-the-quota-rung-q462), Q462, 2026-07-31) — rung binds; bias-low margin 0–2 jobs, never inverted |
| V2 | Measure items 1 and 2's effect — both capacity-gate signals — on dogfood | M | ✅ Measured ([§9e](#9e-what-the-dogfood-run-measured-q469), Q469, 2026-07-31) — **no reduction on the ScaleSet tier**; fixed by item 2d, re-run measured ([§9h](#9h-what-the-dogfood-re-run-measured-for-the-latch-q513), Q513) |
| V3 | Extend item 2b's live-autoscaler drift gate to Karpenter | M | ✅ Done ([§9i](#9i-the-karpenter-arm-of-the-drift-gate-and-what-it-measured-q479), Q479, 2026-07-31) — vocabulary/attribution hold; recorder-generation premise corrected |
| 3 | `Probe`/`Provision` modes: `ProvisioningRequest` `check-capacity` | L | 💤 Deferred ([Q407](../STATUS.md#Q407), [Appendix G.16](../design/appendix-g-future-enhancements.md#g16-provisioningrequest-pre-acquire-capacity-probe-check-capacity)) |

A numbered row means code shipped; a `V` row means that code's effect was measured where it runs. **All V rows have run — item 3's deferral is the plan's only residual.** A ✅ on 0a, 1 or 2 records an envtest proof of the *mechanism*, never a measurement of what it removes.

**V2 ran twice.** The first run ([§9e](#9e-what-the-dogfood-run-measured-q469)) measured **no reduction** on the ScaleSet tier — the integer expression of the rung could not bound waste while its bound was the set's own in-flight pods and the reaper deleted those.
The remedy — the latch, item 2d — shipped as [§9g](#9g-the-latch--a-bound-that-survives-the-reap-q512) (Q512), and the re-run ([§9h](#9h-what-the-dogfood-re-run-measured-for-the-latch-q513), Q513) measured it working as designed: steady-state waste fell from 7 per window to **exactly 1 probe claim per ~5-min window**, the condition held `True/AwaitingProbe` across reap cycles, and the advertisement stayed pinned at the probe slot instead of sawtoothing to the full ceiling. **The effectiveness claim this supports is scale-set-tier-specific and rate-shaped** — a burst's first batch is still claimed whole, and the classic tier remains unmeasured live.

§8's trigger (a) now has its number: residual burn under rate-bounding is ~1 claim per window ([§9h](#9h-what-the-dogfood-re-run-measured-for-the-latch-q513)), which arms item 3 only for operators (GPU/spot) for whom even that rate is expensive.
What item 2b added is narrower and does not substitute for V2: phase 2's *input* — the strings a real cluster-autoscaler emits — is now asserted against a live autoscaler on every run of the drift gate ([§9c](#9c-the-live-autoscaler-harness-and-what-it-measured-q474)).

---

## 1. The gap

`WorkersUnschedulable` (Q157 for v1, Q303 for v2) already reads the scheduler's verdict off worker pods: [`runnergroup_unschedulable.go`](../../cmd/agc/internal/controller/runnergroup_unschedulable.go) computes it from `PodScheduled=False`/`Reason=Unschedulable`, and [`runnerset_capacity.go`](../../cmd/agc/internal/controller/runnerset_capacity.go) publishes the v2 counterpart.
It is pure observability: it reaches a condition, an Event, and a gauge, and never reaches `Provisioner.Admit`.
The `WorkerQuota` ladder and `WorkersUnschedulable` are gauged on both owners — v1 via `actions_gateway_worker_*`, v2 via the `actions_gateway_runnerset_*` twins Q319 added; `WorkerCapacityDeclined` joined them in Q643, with a `reason` label.

So the intake path has no capacity awareness at all, and the per-claim cost of that is:

* one single-use JIT runner record spent (Q114), unrecoverable,
* one GitHub job lock held from `AcquireJob` until the pod is reaped,
* up to `pendingPodDeadline` of wall-clock latency for that job, and
* a **cancelled** workflow run rather than a redelivered one, because a lock that lapses without renewal cancels.

That cost repeats per delivered job, so it scales with burst size.
The existing backstops (`pendingPodDeadline` plus the reaper, with `WorkersUnschedulable` warning at half the deadline) bound the damage *per job*; nothing bounds the number of jobs.

## 2. Why build it rather than only document it

**No CI runner controller does capacity-aware intake.** ARC's [#3647](https://github.com/actions/actions-runner-controller/issues/3647) asks for a *longer wait after claiming*, never for fewer claims.
GitLab documents over-claiming as intentional (`poll_timeout`: "Use this setting for queueing more builds than the cluster can handle at a time").
Kueue does consume `ProvisioningRequest`, but as an `AdmissionCheck` below the claim boundary, and it needs cluster-admin ([§D.5](../design/appendix-d-alternatives-considered.md#d5-kueue-and-kubernetes-job-queue--quota-managers)).

**GAG is structurally positioned to do it and ARC is not.** The decision point has to exist before the claim, and in GAG it already does: the admission gate is called at [`job.go:50`](../../cmd/agc/internal/listener/job.go), before `AcquireJob`.
ARC has no equivalent seam, because the runner process itself claims the job.
This is the same shape of durable differentiator as worker right-sizing ([§D.7](../design/appendix-d-alternatives-considered.md#d7-worker-right-sizing-why-built-in-not-bolted-on)): a capability that must live inside the control plane that owns the boundary.

**The marginal cost is low because #793 built the machinery.** The rung pattern, the `reason`-labelled rejection metric, the fail-open `Target` contract, the condition/gauge plumbing, and the operator-doc pattern all exist.
Phase 1 adds a third rung to a ladder that already has two.

**Where a tenant feels it.** Two shapes, and they are the shapes GAG targets: GPU/spot tenants, where one wrongly claimed job costs a cancelled run on expensive hardware, and burst tenants, where a fan-out of *N* jobs currently becomes *N* wasted claims.

**The honest bound on the value.** None of the three phases eliminates the first wasted claim, and none removes the need for `pendingPodDeadline` and the reaper.
Phases 1 and 2 bound the *rate* of wasted claims (§5, the trickle property); phase 3 removes most of the claims but buys a dependency.
That bound is the reason this is worth doing incrementally rather than as one large feature.

## 3. What changes on a fixed-size cluster

An on-prem cluster with a contracted node count is not a degenerate case of the elastic one, it is the case where **the complexity mostly collapses**, and it is a core deployment shape for self-hosted runners, so the plan is ordered around it.

With no autoscaler running, no actor is waiting on the unplaceable pod.
The Pending pod is not a request for anything; it is pure waste.
The rung the [issue](https://github.com/actions-gateway/github-actions-gateway/issues/785) rejected as "simplest and wrong" (gate directly on `WorkersUnschedulable`) is simply **right** there, and it needs no new signal, no new CRD, and no new dependency, because the condition is already computed in the tree.

The corollary matters for the other two phases: on a fixed-size cluster, the autoscaler-declination signal would never fire (no autoscaler, no event) and `Probe`/`Provision` would fail open (no CRD, nothing to answer), so both degrade to today's behavior.
A plan that shipped only those two would deliver **nothing** to the fixed-size deployment.
Hence the ordering below.

Two nuances worth keeping straight:

* **"Fixed-size" is a property of the cluster, not of the signal.** Soundness therefore has to be established out of band.
  This plan takes it as an operator assertion — the gateway's `clusterCapacity.nodeAutoscaling` IS that assertion (§5a) — not an auto-detection: a wrong auto-detection starves a tenant, and the operator knows their node contract. §10 keeps a detected-autoscaler *warning* as an open question.
* **An elastic cluster at its contracted ceiling reports itself.** Cluster autoscaler's declination message includes `max total nodes in cluster reached` and per-pool `max node group size reached`, so the on-prem-with-a-bounded-CA case is covered by Phase 2's mechanism without asserting anything.

## 4. What changes when RunnerSets target different node pools

Placeability is a property of the pod shape, so a verdict is only ever valid for the shape that produced it: a RunnerSet pinned to `gpu-a100` being unplaceable says nothing about one on `cpu-standard`.

The design gets this right by construction rather than by extra machinery, because **the verdict is keyed to the RunnerSet**, and a RunnerSet resolves to exactly one worker template at a time (its own `templateRef`, or the gateway or cluster default it inherits, §H.4), and therefore one pod shape and one set of pools that shape can land on.
Each set computes its condition from its own worker pods (`LabelRunnerSet`), so a drained GPU pool gates the GPU sets while CPU sets keep claiming.
Per-pool behavior falls out of per-object keying; there is no need for the per-template granularity the issue proposed, and no cluster-wide gate to blast-radius.

Consequences to record:

* **Quota cannot substitute for this rung on a multi-pool cluster.** Namespace `ResourceQuota` is namespace-wide and pool-blind: it answers "does the namespace have room", not "does the pool this shape needs have room".
  On a contracted on-prem cluster with separate CPU and GPU pools, those two answers routinely disagree, which is exactly the gap Phase 1 closes.
* **Residual: a shared, genuinely full pool.** Every set targeting it gates and trickles independently, so aggregate wasted claims scale with the number of gated RunnerSets (≈1 per set per deadline window) rather than with burst size.
  A large improvement, not a total one.
  Cross-set coordination is a non-goal (§10); it would need a namespace-level shared verdict and is not worth the coupling until measured.
* **The autoscaler's message is per-pool and worth surfacing.** CA aggregates rejection reasons per node group ("1 node(s) had untolerated taint(s)", "1 max node group size reached" — measured, [§9c](#9c-the-live-autoscaler-harness-and-what-it-measured-q474)), which is what makes the Phase 2 condition *actionable* rather than merely true.
  Put it in the condition message.
  Note what the sample does *not* contain: the taint's key and value stay in CA's logs, so the condition names the ceiling but only the category of taint.

## 5. Design shared by all phases

**Two fields on two objects, one axis each.** Revised by Q470; the original single-enum design and why it was wrong are in [§5a](#5a-the-single-enum-was-two-axes-q470).

`RunnerSet.spec.capacityGate.mode` — the tenant's policy:

| Value | Meaning | Phase |
|---|---|---|
| `Off` | Default. No capacity rung; today's behavior exactly. | — |
| `Observe` | Refuse jobs this set cannot run, deciding from evidence an already-stuck worker pod produced — on whichever signal is sound for the cluster. | 1, 2 |
| `Probe` / `Provision` | Ask before claiming, via `ProvisioningRequest` `check-capacity` / `best-effort-atomic-scale-up`. | 3 |

Every value but `Off` refuses jobs; they differ in *how the AGC learns* the cluster cannot place the pod, which is why they are named for the method (Q476). `Observe` reads evidence that already exists; `Probe`/`Provision` solicit an answer.
A bare `On` stopped distinguishing those the moment the axis grew, and it was renamed while the value was days old and unreleased.

`ActionsGateway.spec.clusterCapacity.nodeAutoscaling` — the platform operator's fact, which selects the signal:

| Value | Meaning | Signal |
|---|---|---|
| `Present` | Default. Something may add nodes. | The autoscaler's own declination (phase 2). |
| `Absent` | Nothing will add a node. | The scheduler's verdict (phase 1). |

v2 only: v1 `RunnerGroup` is terminal (Q273/Q264), so its adapter gets a no-op method.

**One rung, one condition, one metric label.**

* `Target.CapacityDeclined(ctx) (declined bool, detail string)` joins `Ceiling`/`QuotaExhausted`/`QuotaCapacity` in [`target.go`](../../cmd/agc/internal/provisioner/target.go), fail-open by contract like `QuotaExhausted`.
* **And its integer counterpart, in the same change.** Q443 established that a rung reaches the default acquisition tier only if it is also expressed in `AdvertiseCapacity` — as a bound on the advertised total, the way `QuotaCapacity` expresses rung 1 ([§9b](#9b-what-the-port-shipped)).
  A phase that ships only `CapacityDeclined` ships classic-only and inherits the exact defect Q443 fixed.
  The arithmetic is the same shape: `declined` ⇒ contribute a bound of the set's own in-flight worker pods (no room for more), otherwise no bound.
* `Admit` gains a third rung, ordered **after** quota and **before** the ceiling (like quota it reserves nothing, so the reservation arithmetic is untouched), rejecting with a new `runnercore.AdmitReasonCapacity = "capacity"` on the existing `actions_gateway_jobs_admission_rejected_total{reason}` metric.
* A new `WorkerCapacityDeclined` condition in [`api/apiconditions/conditions.go`](../../api/apiconditions/conditions.go) with one-line re-exports per version (`check-v2-api-sync.sh` gates every shared v2 file, and `conditions_test.go`'s exhaustive name list must be extended).
  Reasons name the source: `PodsUnschedulable`, `ScaleUpDeclined`, `CapacityUnavailable`, cleared by `CapacityAvailable`.
* **Not** added to `ImpairingConditionTypes()`: `WorkersUnschedulable` is already in that rollup and is derived from the same underlying fact, so adding this one would double-count into the GMC's `RunnerSetsDegraded` summary (Q304).

A separate condition rather than gating on `WorkersUnschedulable` itself, for three reasons: it means something different to an operator ("intake is being refused" versus "pods are stuck"), it stays stable across all three phases while the source underneath it changes, and `WorkersUnschedulable` is already an impairing rollup input, so overloading it would tangle the gateway-level Degraded summary with an intake decision.

**The trickle property, and why gating stays safe.** The gate reads a condition that is *derived from the existence of a stuck pod*, which makes it self-clearing and self-probing: condition True stops intake, the reaper deletes the pod at `pendingPodDeadline`, the condition clears, one job is claimed, and if capacity is still absent the new pod trips it again.
A burst of *N* wasted claims becomes roughly one per deadline window, and a Pending pod is present for much of that window, so an autoscaler (if any) keeps being asked.
This is what makes even the Phase 1 mode non-suppressing in practice, and it is also the recovery mechanism on a fixed-size cluster, where capacity returns silently when in-flight jobs finish. **It must be asserted by a test, not assumed.** As originally shipped this property held only on the classic tier — clearing the condition restored the scale-set tier's whole advertisement, measured as a no-op in [§9e](#9e-what-the-dogfood-run-measured-q469) — until [§9g](#9g-the-latch--a-bound-that-survives-the-reap-q512)'s latch gave both tiers a true one-probe-per-window trickle (Q512).

**Fail-open everywhere.** Mode `Off`, an unreadable owner, an unresolved template chain, an unreadable pod list, an absent autoscaler, a missing CRD: all yield `declined=false`, leaving `ceilingCheck` and the `pendingPodDeadline` reaper as the backstops.
The gate may under-gate freely (that is today's behavior); it must never over-gate, because over-gating starves a tenant.

## 5a. The single enum was two axes (Q470)

Shipped 2026-07-27, immediately after phase 2, while the field was still pre-GA and one day old.
Recorded rather than silently rewritten, because §5's original decision was deliberate and the reason it was wrong is the durable part.

**What §5 chose.** One growing enum on the `RunnerSet`: `Off` | `SchedulerVerdict` | `AutoscalerVerdict` | `Probe` | `Provision`.
The stated rationale — avoid a second field, keep the choice mutually exclusive — is true as far as it goes.

**Why it was the wrong cut.** The enum was carrying two independent things:

1. *Should this set refuse work it cannot run?* — a policy choice, and the tenant's.
2. *Which signal may be trusted here?* — a consequence of whether the cluster can grow, which is a property of the **cluster**, identical for every set in it.

Conflating them has three costs, in ascending order of seriousness.
It multiplies values (phase 3 adds two more on axis 1, and `ProvisioningRequest` availability is a third fact on axis 2).
It makes two of the values the same policy with different evidence, which reads as redundancy.
And — the one that actually matters — it asks each **tenant** to assert a fact about infrastructure they may not own.
In a multi-tenant gateway the person writing the `RunnerSet` is routinely not the person who knows the node contract, and `SchedulerVerdict` on an elastic cluster is the single genuinely harmful misconfiguration this feature has.
Under the enum it was reachable by exactly the party least equipped to avoid it, prevented only by documentation.

**The cut Q470 made.** Axis 1 stays on the `RunnerSet` as `mode: Off|On`.
Axis 2 moves to `ActionsGateway.spec.clusterCapacity.nodeAutoscaling` (`Present`|`Absent`, default `Present`), the object the platform operator owns.
The AGC picks the signal from the fact.
The harmful combination is now **unrepresentable**: no value a tenant can write produces scheduler-verdict gating where a node may still arrive (`TestCapacityGate_TheDangerousCombinationIsUnrepresentable`).

**What it cost.** Almost nothing, because the gateway is already resolved on both call sites of `applyWorkerCapacityConditions` — it was a parameter, not a new lookup — and the mechanism (matcher, condition, rung, metric) was untouched.
The API break was free too: pre-GA on two alpha/beta versions, default `Off`, and `SchedulerVerdict` had existed for one day.
Doing it later would have meant a conversion shim and a deprecation window.

**What it costs going forward.** A `RunnerSet` is no longer self-describing — reading one no longer tells you which signal gates it, and an operator has to look at the gateway too.
That is a real loss, mitigated by the condition's `reason` naming the signal on the set itself, and it is the price of putting the assertion where the knowledge is.

**Constraint on Q407.** `Probe`/`Provision` extend axis 1 (solicit rather than observe), not axis 2.
Their cluster-side prerequisite — whether the `ProvisioningRequest` API is available — is another `clusterCapacity` fact, not another mode.
And the mode dispatch is an explicit switch whose default fails open on an unknown value, so phase 3 must add its own arm rather than rely on a fallthrough.

**Rejected on the way.** Deleting axis 2 entirely by inferring elasticity — gate on the scheduler's verdict unless an autoscaler is acting, treating *silence* from any autoscaler as "no autoscaler here".
It looks like the bigger simplification and is a trap: cluster-autoscaler is legitimately silent during backoff, during a cooldown after a failed scale-up, and for pods it filters out of its evaluation, so silence is absence of evidence.
Gating on it would starve a tenant, and it would invert the rule the rest of this rung follows — an unreadable pod list, a missing Event, an unrecognized vocabulary all resolve to *don't gate*. §10 already ruled auto-detection out of scope; this is the same call, restated because the collapsed enum makes it tempting again.

## 6. Phase 1 — the scheduler-verdict signal (Q405)

> The mode name `SchedulerVerdict` in this section is historical: Q470 replaced it with `mode: Observe` plus `clusterCapacity.nodeAutoscaling: Absent` on the gateway ([§5a](#5a-the-single-enum-was-two-axes-q470)).
> The signal, the rung, and the condition are unchanged.

The gateable signal already exists as a published condition, so this phase is mostly API surface and wiring.

**Scope**

1. `spec.capacityGate.mode` on `RunnerSet` (`Off`|`SchedulerVerdict`), with the remaining values reserved and documented as not-yet-implemented; CRD + deepcopy + the two chart copies + the GMC-bundled copy (see [reference](../development/code-generation.md)).
2. `WorkerCapacityDeclined` condition + reasons (§5), computed alongside the existing evaluations in `applyWorkerCapacityConditions` ([`runnerset_capacity.go`](../../cmd/agc/internal/controller/runnerset_capacity.go)), reusing `evalWorkersUnschedulableForPods` rather than re-deriving.
   Skipped entirely when mode is `Off` so there is no cost for the default.
3. `runnerSetTarget.CapacityDeclined` reading the condition off the cached RunnerSet (a cheap per-delivery read, matching `Ceiling`/`QuotaExhausted`); `runnerGroupTarget.CapacityDeclined` returning false.
4. The `Admit` rung + reason constant + the Debug log line, mirroring the quota rung's shape — **and the matching `AdvertiseCapacity` rung**, or the mode is inert on the default tier (§5, [§9b](#9b-what-the-port-shipped)).
   A `SchedulerVerdict` that only lands in `Admit` is not Phase 1 shipped, it is Phase 1 shipped to the deprecated tier.
5. Field doc **and** operator doc stating the soundness precondition in the same words: this mode assumes no autoscaler will act on the pods, and an elastic cluster wants `AutoscalerVerdict` instead.

**Tests.** Unit: the rung's ordering and fail-open paths in `admission_internal_test.go`; the mode-`Off` no-op; the trickle cycle (gate closes, pod reaped, condition clears, exactly one claim admitted, gate closes again).
Integration (envtest, `cmd/agc/internal/controller/integration/`): a Pending worker pod with `PodScheduled=False/Unschedulable` produces the condition, and the admission metric increments with `reason="capacity"`.

**Docs.** `03-api-contracts` (field), `04-operational-flows` (the ladder now has three rungs), `operations/observability-metrics` (the new reason label value), `operations/troubleshooting` (what an operator sees, why claims stopped, how to turn it off), plus the website positioning page in the same PR per the standing rule.

**Estimate.** ~450–700 lines net across ~25 files, 1–2 PRs, Sz M. The v2 gauge for the new condition was deliberately out of scope here, and Q319 left it out of the `actions_gateway_runnerset_*` family for the same reason: the condition is conditionally *absent* rather than False when the gate is off, which is its own design question.
Q643 settled it — the gauge follows the condition, so an ungated set emits no series at all, and a `reason` label carries which of the five reasons is current so the latched `AwaitingProbe` state is distinguishable from a live decline.

## 7. Phase 2 — the autoscaler-declination signal (Q406)

> The mode name `AutoscalerVerdict` in this section is historical: Q470 replaced it with `mode: Observe` plus the gateway default `clusterCapacity.nodeAutoscaling: Present` ([§5a](#5a-the-single-enum-was-two-axes-q470)).
> The signal is unchanged.

Same rung, different source: the autoscaler's own declination, read from Events on the stuck pod.
Verified upstream 2026-07-25:

* **Cluster autoscaler** emits `Normal NotTriggerScaleUp`, "pod didn't trigger scale-up: `<per-node-group reasons>`", from `cluster-autoscaler/processors/status/eventing_scale_up_processor.go`, only when a loop concluded *without* attempting a scale-up.
  GKE's autoscaler is CA-derived and emits it, so this is verifiable on the existing dogfood cluster with no new node-pool configuration.
* **Karpenter** emits `Warning FailedScheduling`, "Failed to schedule pod, `<err>`", from `pkg/controllers/provisioning/scheduling/events.go`, plus `NoCompatibleInstanceTypes` on the NodePool. **`FailedScheduling` is also kube-scheduler's own reason**, so the discriminator must be the reporting controller, never the reason string alone.

**One matcher per autoscaler project, not per cloud provider, and no plugin interface.** The obvious worry is a combinatorial integration surface, and the counts say otherwise, because both projects emit their events from shared core code that every provider vendors (verified 2026-07-25):

| Project | Provider implementations | Event vocabularies |
|---|---|---|
| cluster-autoscaler | 36 dirs under `cloudprovider/`, ~30 real clouds (the rest are `builder`/`mocks`/`test`/`kubemark`/`kwok`) | 1 |
| Karpenter | 16 listed in the `kubernetes-sigs/karpenter` README (AWS, Azure, GCP, IBM, OCI, Cluster API, Alibaba, Hetzner, Proxmox, Linode, …) | 1 |

Searching either organization's provider code for those event reasons returns zero hits: CA emits from `processors/status/eventing_scale_up_processor.go` and Karpenter from `pkg/controllers/provisioning/scheduling/events.go`.
So ~46 providers collapse to two string matchers.
The managed offerings are not a third vocabulary either: GKE's autoscaler is CA-derived, EKS runs upstream CA or Karpenter, AKS runs CA or Node Auto Provisioning (which is Karpenter), and OpenShift's MachineAutoscaler wraps CA.

A provider abstraction is therefore not warranted, and the distinguishing rule is worth stating because this repo has legitimately built one before: **pluggable backends earn their keep when the *behavior* differs per provider, not when only the recognized input differs.** Q245's `--fqdn-policy-backend=cilium|calico|gke` needed real backends because each CNI requires emitting a different CRD.
Here every autoscaler yields the same boolean and the same action, so a registry plus an interface plus a config field would be pure overhead over two `case` arms.

**The proprietary tail is handled by the fail-open contract, not by research.** Commercial optimizers (CAST AI, Spot Ocean, Zesty) may emit their own events or none, and cannot be enumerated.
An unrecognized autoscaler produces no match, which yields `declined=false`, which is exactly today's behavior.
That asymmetry is the safety argument: a missed match costs nothing, a wrong match starves a tenant, so the matcher stays deliberately narrow and broad coverage is explicitly not a goal.
If demand for a specific proprietary autoscaler ever appears, the extension point is data rather than code (an operator-settable list of extra `(reason, reportingController)` pairs, following the `--allowed-infra-priority-classes` allowlist pattern, ~20 lines and no per-vendor code).
Documented here as an extension point; not built until asked for.

**Scope.** A matcher over pod Events (new `cmd/agc/internal/controller/autoscaler_verdict.go`), read with a live (uncached) client **only** for pods already Pending past the scheduling grace and only when mode is `AutoscalerVerdict`, so there is no event informer and no steady-state cost; the aggregated reason text into the condition message; `events` `get;list;watch` added to [`doc.go`](../../cmd/agc/internal/controller/doc.go)'s markers **and** the chart's hand-maintained `charts/actions-gateway/files/agc-*-rules.yaml`.

**Tests.** Table-driven matcher unit tests: the CA message matches, the Karpenter message matches, a `FailedScheduling` from `default-scheduler` does **not** match, no events and unreadable events fail open.
Plus the envtest counterpart stamping a real Event.

**Estimate.** ~350–550 lines net across ~12 files, 1 PR, Sz M.

**Risks.** Events expire (`--event-ttl`, commonly 1h), which is acceptable: the signal is only consulted inside the `pendingPodDeadline` window and absence fails open. `max total nodes in cluster reached` is a cluster-wide verdict that will trip every set at once; correct (no node is coming) but worth calling out in the operator doc.
Unknown until measured: how often CA emits the event for a *transient* condition it would have resolved on its next loop.

## 7a. What phase 2 shipped

Shipped 2026-07-27. §7's scope landed as specified; four things were decided during implementation that the spec did not settle, and each is a constraint on Q407 rather than a local choice.

**The verdict is the newest relevant event, not the presence of a declination.** §7 specified two declination matchers and stopped there, which would have let a stale `NotTriggerScaleUp` gate a set the autoscaler had since started scaling for — the transient-declination risk §7 flagged as "unknown until measured", turned from a measurement question into a correctness one.
So the matcher also recognizes each project's *acting* signal (CA `TriggeredScaleUp`, Karpenter `Nominated`) and returns the class of the newest event by timestamp, with same-instant ties resolving open (generalized to a one-second concurrency window by [§9d](#9d-the-concurrency-window-q478), once a live autoscaler was seen emitting both verdicts inside one loop).
Ordering reads every timestamp field an Event may carry, because the two recorder generations populate different ones and reading only `LastTimestamp` would sort every new-style event at the zero time.
This costs nothing — the events are already in hand — and removes the failure mode outright rather than deferring it to §9.

**An unrecognized mode fails open, with its own reason.** Q405's condition writer treated any non-`Off` mode as `SchedulerVerdict`, which was safe while the enum held exactly those two values and is not once it holds three.
The CRDs ship as their own chart and can be upgraded ahead of the AGC, so a `Probe` selected against an AGC that predates Q407 would have silently applied `SchedulerVerdict`'s semantics — on an elastic cluster, precisely the tenant-starving outcome the mode split exists to prevent.
The mode dispatch is now an explicit switch whose default publishes `WorkerCapacityDeclined=False/GateModeUnsupported`. **Q407 must add its arm to that switch**, not rely on a fallthrough.

**A re-check interval, because nothing watches Events.** The gate's signal lives in objects the AGC deliberately does not cache, so neither a declination arriving nor a later scale-up would re-trigger a reconcile on its own.
A set in `AutoscalerVerdict` mode with any stuck pod therefore requeues every 30s while stuck.
Both directions matter and the second is the safety one: without it the gate would stay closed after the autoscaler started acting, which is exactly what §9 step 3 checks for.

**The read budget is bounded and doubly scoped.** Events are read only for pods the `WorkersUnschedulable` evaluation already found stuck past the scheduling grace (carried out of that evaluation rather than re-derived, so the two cannot disagree), oldest-first, field-selected server-side to one pod by name and UID, and capped at 8 pods per reconcile with the truncation stated in the condition message.
A healthy set costs zero reads.
RBAC is `get`/`list` on core `events` only — **not** `watch`, which §7 listed: there is no informer, and granting a verb the code does not use would be a gratuitous widening.

**Not measured yet.** §9 owns the live validation and none of it has run for this mode.
Specifically unmeasured: the event's latency from pod creation (§9 step 4), which sizes the condition's staleness bound and is the number that says whether 30s is the right re-check; and how often CA emits `NotTriggerScaleUp` for a condition it resolves on its next loop — the recency rule bounds the damage of that case but does not tell us how common it is.

## 8. Phase 3 — `Probe`/`Provision` (Q407, deferred)

Full cost, correctness landmines (the `PodTemplate` object each `podSet` must reference, booking/`BookingExpired` semantics, the `CapacityAvailable` vs `Provisioned` condition-name drift, provider support), and the triggers that would revive it are recorded in [Appendix G.16](../design/appendix-g-future-enhancements.md#g16-provisioningrequest-pre-acquire-capacity-probe-check-capacity).
Not duplicated here.

The decision rule: build it only when at least one of (a) Phase 1/2 measurement shows residual burn that rate-bounding cannot make acceptable (the GPU/spot case, where even one wasted claim per window is expensive), (b) an operator asks with a cluster that already runs CA with provisioning requests enabled, or (c) provider support broadens enough that the fail-open path stops being the common one.
Phases 1 and 2 are not throwaway work under any of those: phase 3 swaps the rung's data source from an observed verdict to a solicited answer and reuses the field, the condition, the metric label, the trickle, and the fail-open contract unchanged.

## 9. Validation (to be measured, not asserted)

On the GKE dogfood cluster, per phase:

1. Induce unplaceability with a template the cluster cannot satisfy (a `nodeSelector` for a nonexistent pool, or a CPU request beyond any machine type).
   Dispatch a burst of *N* jobs.
2. Count wasted claims as `actions_gateway_worker_pods_reaped_total{reason="pending_deadline"}` over the burst, which is exactly one per claim that could not be placed.
   Compare `mode: Off` against the phase's mode; with the mode on, the drop should be accounted for by `actions_gateway_jobs_admission_rejected_total{reason="capacity"}`, and the residual should be ≈1 per deadline window (the trickle).
   Both counters are on the admission/reaper paths, not on job completion.
3. **The check that actually matters:** with a shape the autoscaler *can* satisfy, confirm a scale-up still happens and the jobs still run, i.e. the gate did not suppress the trigger.
   A pass on step 2 with a fail here is a regression, not a win.
4. Measure the event's latency from pod creation (Phase 2) to size the condition's staleness bound honestly in the operator doc.

Q399 (most dogfood jobs started but never completed) would have perturbed burst runs.
It is fixed: the tenant moved off the Classic protocol to a single-label ScaleSet.
The counters above come from the admission path rather than from completions, so they were never blocked by it either way.
Detail: [gke-dogfood B7](gke-dogfood.md#b7-create-the-v2-tenant-objects).

## 9a. The shipped quota rung was classic-only (Q443)

Found while qualifying the eviction-recovery claims for Q419 (shipped 2026-07-26) — same defect class, same public sentences, other half.
Recorded here rather than fixed there, because the two capabilities have different owners and different remedies.
The copy correction shipped 2026-07-26 (Q439); the port it revealed is Q443, specified below and shipped the same day.

**Shipped 2026-07-26.** The port described below is in; what remains of this section is the finding, the decision, and the design it settled on, which §6/§7 inherit.
What shipped is recorded in [§9b](#9b-what-the-port-shipped).

The rung this plan builds on — rung 1, live namespace-`ResourceQuota` headroom checked *before* the claim — is wired into `Provisioner.Admit`, and `Admit` is reached from two call sites: `AdmitFor` (v1 `RunnerGroup`) and the classic branch of the v2 `RunnerSet` reconciler. `reconcileScaleSetListener` returns before that wiring.
What a `ScaleSet` set advertises instead is `scaleSetCapacityFunc` → `X-ScaleSetMaxCapacity` → `target.Ceiling`: the set's configured worker ceiling (max tier threshold, else `maxWorkers`, else the default).
That is the Q59 concurrency rung, not the quota rung.
Nothing on that tier consults quota headroom before a job is assigned.

So on the default acquisition tier a quota-blocked job **is** claimed.
It then falls to `createPodWithQuotaRetry`, the in-place backstop this plan's §1 calls out as the failure mode the gate exists to prevent: the job lock is held across up to `maxQuotaRetries × quotaRetryDelay`, and on exhaustion the pod is abandoned with the lock held.

Two doc claims were wrong as a result, both in public copy.
Both are corrected as of 2026-07-26; recorded here because the second was a fabricated mechanism, not a scope error, and that distinction should survive:

* *"won't claim a job it can't place … live quota headroom checked before the claim"* ([why-gag](../why-gag.md) comparison table; [README](../../README.md) "The Solution") — true on classic, not on the default tier. **Scoped**, not removed.
* *"if headroom is lost after the claim, auto lock-cancel + re-queue"* (why-gag, same row; also [runbook.md](../operations/runbook.md)) — no code path did this, on any tier. `createPodWithQuotaRetry` retries in place and abandons the pod on budget exhaustion; there is no rerun call.
  The lock-cancel-and-re-queue language was borrowed from the eviction path, which is itself classic-only. **Replaced** with what the code does.

`actions_gateway_jobs_admission_rejected_total` is emitted from the same classic call site ([listener/job.go](../../cmd/agc/internal/listener/job.go)), so both its `reason="quota"` and `reason="ceiling"` series read a flat zero on the default tier — the same "healthy dashboard, lost jobs" shape Q419 found on the eviction counters.

### The decision: port the rung, and treat it as a 2.0 gate (Q443)

Settled 2026-07-26, because the copy fix could not be written honestly without it.

**Port it.** The mechanism is expressible on the scale-set tier and the cost of not porting is losing a headline capability at `v2.0.0`.

Expressible, because `X-ScaleSetMaxCapacity` is not a free-slot delta but a *total* ceiling — GitHub holds `totalAssignedJobs` at or below the advertised value — so a quota-derived bound composes with the Q59 ceiling as a `min()` on the integer `scaleSetCapacityFunc` already returns.
Jobs beyond it stay queued server-side, which is precisely the classic rung's outcome.

Three things make it more than a one-line change, and they are the design work this phase owns:

* **Delta-to-total conversion.** Headroom answers "how many *more* pods fit"; the advertisement wants a total.
  That is roughly `activeWorkers + headroom`, and the AGC's count of its own in-flight pods is not GitHub's `totalAssignedJobs` — the two diverge across an assignment the AGC has not yet provisioned.
  Under-advertising merely delays jobs; over-advertising reproduces today's claim-and-stall.
  Bias low, and measure the divergence.
* **Granularity loss.** Classic re-decides per delivered job.
  Scale-set decides once per long-poll, for the whole set.
  Recovery from a stale read is a poll interval, not a job.
* **Interaction with §6/§7.** The `SchedulerVerdict` and `AutoscalerVerdict` rungs are specified as per-delivery `Admit` rungs.
  If the scale-set tier gets a capacity-integer expression of rung 1, those two need the same treatment or they ship classic-only and inherit this exact defect on arrival.
  Design the integer path once, for all three rungs.

**Why it is a 2.0 gate.** Classic acquisition is removed in `v2.0.0` ([v2-ga.md](v2-ga.md#phase-3--the-coupled-removals)).
Rung 1 exists only on classic.
So the removal deletes the pre-claim quota gate outright unless this lands first — structurally identical to Q417 for eviction recovery, which cleared the same risk on 2026-07-26, and until now undeclared.
Two of the four capabilities the README leads with were in this position.
Tracked as Q443, labelled `2.0-gate` to match, and shipped 2026-07-26 ([§9b](#9b-what-the-port-shipped)).

**What ships without waiting:** the copy correction.
Every claim above is now scoped to the classic tier, and the "auto lock-cancel + re-queue" sentence is removed rather than qualified, since no code path implements it on any tier.

## 9b. What the port shipped

Shipped 2026-07-26.
The three design questions §9a raised were answered as follows; each answer is a constraint on Q405/Q406, not a local choice.

**One ladder, two shapes.** `Provisioner.AdvertiseCapacity(target, unboundedDefault)` sits beside `Provisioner.Admit(target)` in [`admission.go`](../../cmd/agc/internal/provisioner/admission.go), walks the same rungs against the same `Target`, and returns a `CapacityAdvertisement` — the integer plus the per-rung accounting that produced it.
Both godoc comments state the invariant that caused this bug: **a rung added to one and not the other ships to one tier.** §6/§7 therefore each land in both, which is the "design the integer path once, for all three rungs" requirement discharged rather than deferred.

**Delta-to-total, biased low.** `Target.QuotaCapacity(ctx, max)` joins `Ceiling`/`QuotaExhausted` with the same fail-open contract.
The v2 adapter converts observed headroom to a total as `own non-terminal worker pods + headroom`, capped at `max`; v1 returns unbounded (it is terminal, and `Admit`'s boolean is its authoritative form).
The pod count, not GitHub's `totalAssignedJobs`, is deliberate and is the bias-low choice §9a asked for: an assignment the AGC has not provisioned yet is inside `totalAssignedJobs` but not inside the quota's `used`, so counting assignments would over-advertise by exactly the in-flight gap.

**The integer arithmetic reuses the boolean's.** `QuotaHeadroomPods` binary-searches `WorkerFootprint`/`QuotaHeadroomViolations` for the largest fitting count rather than dividing headroom by a per-pod footprint.
Division would have been a second implementation of a multi-resource, multi-format comparison, free to drift from the rung it is supposed to mirror; the search is exact, bounded by the caller's ceiling, and `TestQuotaHeadroomPods_AgreesWithTheBooleanRung` asserts the two answers cannot disagree.

**Observability.** `actions_gateway_scaleset_advertised_capacity` and `actions_gateway_scaleset_capacity_withheld{reason}` are the tier's counterpart to `jobs_admission_rejected_total{reason}`, which is structurally unreachable here — a declined job is never assigned, so there is no rejected delivery to count.
Gauges, not counters, for the same reason, and every evaluated rung publishes an explicit zero each poll so a series never freezes at its last non-zero reading.
Both are dropped when the set is deleted.

**Measured 2026-07-31 ([§9f](#9f-what-the-dogfood-run-measured-for-the-quota-rung-q462)).** Both quantities this section left open came back inside their intended bounds: the bias-low margin between the AGC's pod count and GitHub's `totalAssignedJobs` was 0–2 jobs and never inverted, and recovery from freed quota mid-burst was ~30s rather than a painful poll interval.
The rung binds continuously on the scale-set tier — unlike the capacity rung, whose live result was null ([§9e](#9e-what-the-dogfood-run-measured-q469)).
Still unmeasured: a CPU/memory-shaped quota, where `QuotaHeadroomPods`' binary search does the real work, and the classic tier's boolean rung on a live cluster.

## 9c. The live-autoscaler harness, and what it measured (Q474)

Shipped 2026-07-28. §9 above is about the gate's *effect*, since measured by a real burst on dogfood ([§9e](#9e-what-the-dogfood-run-measured-q469)).
This is the other half, and it is about the gate's *input*: phase 2 recognizes cluster-autoscaler by strings upstream owns, pinned in `autoscaler_verdict_test.go` from recorded samples.

**Why a reword is the dangerous kind of drift.** Everywhere else in this rung, fail-open is the safety property.
Here it is also the concealment: an unrecognized vocabulary yields `declined=false`, which is exactly today's ungated behavior, so a reword does not break a test, raise an alarm, or change a metric — it silently turns the mode into a no-op on every elastic cluster.
No tier that runs against recorded samples can notice, by construction.

**The harness.** `make autoscaler-cluster` stands up a throwaway kind cluster running a real upstream cluster-autoscaler on its [kwok cloud provider](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler/cloudprovider/kwok), whose stated purpose is exactly this: real CA, real scheduler-framework evaluation, real events, fake machines. `make test-autoscaler` then drives three pods through it and asserts the matcher's verdict on the events that come back (`autoscaler_verdict_live_test.go`, build tag `autoscaler`).
It is its own cluster rather than the e2e one because an autoscaler creating and deleting nodes underneath the e2e suite would perturb every spec in it.
Run cadence and commands: [testing.md](../development/testing.md#the-live-autoscaler-drift-gate).

Three cases, chosen so each asserts something the recorded table cannot:

1. **A declination is still recognized** — and the kube-scheduler `FailedScheduling` that always accompanies a stuck pod is in the same list, so the pass also shows the matcher picking the autoscaler's verdict out of a list it does not control, rather than matching "some event exists".
2. **A scale-up still keeps the gate open** — §9 step 3 in miniature, and the only case here whose failure would be a regression rather than a stale sample.
   It asserts the outcome, not just the event: the pod must land on a node that did not exist when it was created.
3. **The node-group ceiling is still named** — the one part of the message body the operator docs promise by name.

**Measured against cluster-autoscaler v1.36.1 / Kubernetes 1.36.1, 2026-07-28.**

* **The vocabulary holds.** `NotTriggerScaleUp`, `TriggeredScaleUp`, the `pod didn't trigger scale-up:` / `pod triggered scale-up:` prefixes, and `max node group size reached` are all intact.
* **The reporter is attributed on both fields.** CA sets `source.component` *and* `reportingComponent` to `cluster-autoscaler`, so `reportedByScheduler`'s new-style-then-legacy fallback reads the same answer either way.
* **CA still records through the legacy recorder** — `firstTimestamp`/ `lastTimestamp` set, `eventTime` null. `eventTime()`'s decision to take the latest of *every* timestamp field is therefore load-bearing today, not defensive: reading only `EventTime` would sort every CA event at the zero time.
* **The taint is no longer named, the ceiling still is.** v1.36.1 emits `1 node(s) had untolerated taint(s)` — the key and value are in CA's *logs* (`debugInfo`), not in the event.
  The recorded sample in the unit table said `2 node(s) had untolerated taint {dedicated: gpu}`; both spellings are now rows, because the matcher never parses a body and must classify them identically.
  No operator-doc claim had to be withdrawn: the troubleshooting page promises the *category*, and §4's parenthetical sample is now the measured one.
* **A real autoscaler emitted both verdicts for one pod inside one loop.** With three pods contending for a two-node group, CA recorded `TriggeredScaleUp` for pod `big-a` at `14:05:19.348` and `NotTriggerScaleUp` for the *same* pod at `14:05:19.352` — the first scale-up round found a plan, and a second round with the upcoming node still could not place it. §7a introduced the newest-wins rule precisely for a declination the autoscaler had superseded; this is the case it did not anticipate, where the *declination is the later one* and a node is nonetheless arriving.
  Today it resolves correctly, but only because CA's legacy recorder has one-second resolution, so the two events tie and the tie resolves open.
  The recency rule alone would have said "declined".
  That makes the tie-break the thing carrying correctness here, and it does not carry it for a microsecond-resolution recorder — believed at the time to be the generation Karpenter uses ([§9i](#9i-the-karpenter-arm-of-the-drift-gate-and-what-it-measured-q479) later measured that belief wrong: Karpenter records legacy too).
  Not fixed here, because Q474 is a test and the fix is a behavior change to a shipped gate that wants its own negative control; tracked as Q478 and shipped the same day as **[§9d](#9d-the-concurrency-window-q478)**, which replaces the tie-break with a concurrency window.

**What this initially did not cover.** Karpenter — the arm that genuinely needs reporter discrimination, since it shares kube-scheduler's reason string.
Its harness landed as [§9i](#9i-the-karpenter-arm-of-the-drift-gate-and-what-it-measured-q479) (Q479).
This CA harness is also the thing [G.16](../design/appendix-g-future-enhancements.md#g16-provisioningrequest-pre-acquire-capacity-probe-check-capacity) named as the local-half prerequisite for ever validating `Probe`/`Provision` (Q407): a cluster with a real CA in it is where `--enable-provisioning-requests` could be turned on.

## 9d. The concurrency window (Q478)

Shipped 2026-07-28.
[§9c](#9c-the-live-autoscaler-harness-and-what-it-measured-q474) measured a real cluster-autoscaler emitting `TriggeredScaleUp` and then `NotTriggerScaleUp` for the same pod 4ms apart, and observed that §7a's newest-wins rule reads the later declination — so the pair resolved correctly only because CA's legacy recorder quantizes both events into one second and ties already resolved open.
This replaces that accident with a rule.

**The rule.** Recency is asymmetric.
A scale-up supersedes a declination the instant it is newer, as before; a declination supersedes a scale-up only from more than `autoscalerConcurrencyWindow` (one second) later.
A closer pair is one loop's own output rather than a sequence, and resolves not-declined.

**Why one second.** It has to exceed the spread of one loop's own events — milliseconds, measured — and stay well below the gap *between* loops, so a declination on a later loop still gates.
Both projects leave a wide margin: CA's default `--scan-interval` is 10s and Karpenter's provisioning batch is bounded at 10s.
One second is also the legacy recorder's own quantum, so the generation running today decides identically with the window as without it; what changes is the microsecond generation — believed then to be Karpenter's, measured otherwise in [§9i](#9i-the-karpenter-arm-of-the-drift-gate-and-what-it-measured-q479).

**The direction of the error, in both readings.** The window can only *open* a gate that recency would have closed, which is the fail-open direction the whole rung is built around — and it opens it for at most one second past a scale-up that a genuinely-later declination will re-close on the next loop.

**What it is tested by, and what it is not.** Six unit rows: the measured 4ms pair in the recorder generation that can resolve it, its Karpenter-vocabulary twin, both sides of the boundary, a declination a full CA loop later that must still gate, and an old scale-up that must not shelter a later declination (the window is measured from the newest *acting* event, not the newest event).
No live harness covers it, and today none can: CA records at second resolution, so its harness cannot produce a pair the rule would decide differently — and the Karpenter harness ([§9i](#9i-the-karpenter-arm-of-the-drift-gate-and-what-it-measured-q479), Q479) measured that Karpenter records at second resolution too, despite what this section assumed when it shipped.
The microsecond arm is therefore asserted only against synthetic events by the nature of what ships upstream, not by a coverage gap; §9i's recorder-generation case is what will notice if either project migrates.

## 9e. What the dogfood run measured (Q469)

Measured 2026-07-30/31 on `gag-dogfood` (GKE, Dataplane V2, cluster-autoscaler present), against a control plane built from `45670972` — the first time any of this rung has run outside envtest. **The headline is negative: on the ScaleSet tier the gate did not reduce wasted claims at all.** It is not a harness artifact; the rung evaluated, published its condition, and withheld capacity, and the arithmetic that makes it a no-op is in the shipped code.

**The harness.** Two ScaleSet RunnerSets differing *only* in `capacityGate.mode`, both on a `RunnerTemplate` whose `nodeSelector` names a node pool that does not exist, `maxWorkers: 8`, and `pendingPodDeadline: 2m` — shortened from the 10m default so the trickle is observable inside a session.
The deadline is part of the result, not a detail: the residual §5 predicts is *per deadline window*.
One burst = one `unit-test.yml` dispatch = **7** GAG-routed jobs.
The gateway leaves `clusterCapacity.nodeAutoscaling` unset (default `Present`), which is truthful here, so the signal under test is phase 2's autoscaler declination, not phase 1's scheduler verdict.

Phase B ran on a **new** set rather than a re-mode of Phase A's: after Phase A's run was cancelled the set still reported `7 job(s) assigned` for minutes with zero worker pods, and reusing the name would have replayed its unacked `JobAssigned` messages (gke-dogfood B7).
Both sets were otherwise identical.

### Step 2 — no reduction

| | `mode: Off` | `mode: Observe` |
|---|---|---|
| `worker_pods_reaped_total{reason="pending_deadline"}`, ~12 min | **21** | **21** |
| Shape | 3 batches × 7 | 3 batches × 7 |
| Batch period | ~5 min | ~5 min |

The gate was not inert — it reached `WorkerCapacityDeclined=True` with reason `ScaleUpDeclined` (so the phase 2 matcher recognized a live GKE autoscaler's declination, unprompted) and it withheld capacity.
It withheld **1 slot**.

**Why 1, and why that is structural.** [`DeclinedCapacity`](../../cmd/agc/internal/controller/runnerset_target.go) returns `(active, true)` when the gate declines and `(0, false)` when it does not, where `active` is the set's own non-terminal worker pods.
So the bound is only ever *the batch already in flight*: with 7 pods against a ceiling of 8 it withheld `8 − 7 = 1`.
Its godoc says "the advertisement falls to zero once those drain" — **that state is unreachable**, because the gate's evidence is the stuck pod itself.
One measured cycle, sampled every 10s:

```
00:14:51  adv=7  withheld{capacity}=1  pods=7  WorkerCapacityDeclined=True/ScaleUpDeclined
00:15:02  adv=7  withheld{capacity}=1  pods=0  WorkerCapacityDeclined=False/CapacityAvailable   <- reaper fires
00:15:35  adv=8  withheld{capacity}=0  pods=0  WorkerCapacityDeclined=False/CapacityAvailable   <- full ceiling restored
```

The reap drains `active` to 0 and clears `declined` together — both were already true in the first sample after the reaper fired — so the advertisement returns to the **full** ceiling before the next assignment, and the next batch is claimed whole. §5's trickle property — "the condition clears, *one job* is claimed" — is the classic per-delivery behaviour.
The integer tier has no per-job decision point to trickle through: clearing the condition restores the entire advertisement, so a burst of *N* becomes *N* again, every window.

Two further timings, both measured, both making the window worse rather than better:

* **The gate closes ~60s after the first pod goes Pending**, not immediately — it waits the `WorkersUnschedulable` scheduling grace, which is `pendingPodDeadline/2`.
  On the shipped 10m default that is **5 minutes**.
* **Assignment is batch-granular per long-poll**, so all 7 jobs were assigned before the gate could close under any grace. §5's "none of the phases eliminates the first wasted claim" is, on this tier, the first *batch*.

**What this does not say.** It does not retract the mechanism: the condition, the reason, the matcher against a real GKE autoscaler, and the withheld gauge all behaved as specified.
It says the integer expression of the rung cannot bound claim waste while its bound is coupled to the pods the reaper deletes.
The fix was a design question, not a patch — a bound that survives the reap needs evidence that outlives the pod — and shipped as [§9g](#9g-the-latch--a-bound-that-survives-the-reap-q512)'s latch (Q512); the re-measurement ([§9h](#9h-what-the-dogfood-re-run-measured-for-the-latch-q513), Q513) confirmed it bounds the rate to 1 per window.

### Step 3 — scale-up is not suppressed (pass)

The check §9 calls the one that actually matters.
Same gate mode (`Observe`), same gateway, a shape the autoscaler *can* satisfy (the tenant's `default` template, tolerating the workers taint, 2 vCPU) against a `workers` pool sitting at **0 nodes**, so placement *required* a scale-up.

```
00:25:40  pod created, FailedScheduling (0/2 nodes: Insufficient cpu)
00:25:41  TriggeredScaleUp                       <- 1s, gate on
00:26:49  worker_nodes 0 -> 2
00:28:10  2 worker pods Running
```

`WorkerCapacityDeclined` stayed `False/CapacityAvailable` for the entire run — the newest event was CA's *acting* signal, and §7a's newest-wins rule kept the gate open, which is precisely the case §9d's concurrency window exists to protect.
Jobs ran green (`unit-test`, `coverage`, `vendor-check`, `tidy-check`, `path-filters`; `shellcheck` was the unrelated Q482 red — the job lacked the `setup-go` step its `make scripts-test` step needs, since fixed).

A pass here alongside a null step 2 is not a contradiction: the gate correctly declines to suppress a scale-up, it simply does not bound waste when no scale-up is coming.

### Step 4 — the autoscaler event's latency is 0–1s

Across all 7 pods of one burst, CA's `NotTriggerScaleUp` landed **0–1s** after pod creation (the spread is the legacy recorder's one-second quantum, §9c).
The `TriggeredScaleUp` of step 3 landed at 1s.
So the 30s re-check of §7a is conservative by more than an order of magnitude, and the condition's staleness is bounded by the re-check rather than by the autoscaler — the operator doc's staleness wording needs no change.

Corroborating §9c on this cluster, unprompted: `reportingComponent` *and* `source.component` both read `cluster-autoscaler`, `eventTime` was null with `firstTimestamp`/`lastTimestamp` set (so `eventTime()`'s all-fields rule stays load-bearing), and the taint case again named only a category.
One **new** message body not in the recorded table appeared, from the nonexistent-pool shape: `1 node(s) didn't match Pod's node affinity/selector`.
The matcher never parses a body, so it classifies identically — recorded because the sample set is the thing Q474's drift gate pins.

### What the method itself got wrong

§9 step 2 names `actions_gateway_jobs_admission_rejected_total{reason="capacity"}` as the counter that should account for the drop. **That counter cannot fire on this tier.** It is emitted from one site, [`listener/job.go`](../../cmd/agc/internal/listener/job.go), on the classic path; [`scalesetlistener/metrics.go`](../../cmd/agc/internal/scalesetlistener/metrics.go) states it is structurally unreachable for a scale set, because a job the ladder declines is never assigned rather than claimed and rejected.
The scale-set counterparts are `actions_gateway_scaleset_advertised_capacity` and `actions_gateway_scaleset_capacity_withheld{reason}`, and those are what the table above reports. §9 was written before §9b's port and was never reconciled with it; read as written it would have produced a flat-zero series and the false reading that the gate never engaged.

Two observability snags found on the way, since filed as their own Queue rows (Q514, Q515 — both since fixed):

* `worker_pods_reaped_total` is labelled `runner_group`, while every `scaleset_*` gauge is labelled `runner_set`.
  A dashboard that joins the reaper counter to the advertisement gauges by label silently returns nothing.
  Fixed by Q514: the counter now also stamps a `runner_set` label on scale-set reaps (empty on classic; `runner_group` unchanged on both tiers), so the join works without breaking existing `runner_group`-keyed queries.
* The `v2alpha1 is deprecated` warning is logged at info level per API read.
  On a set reconciling under a stuck burst it dominates the AGC log to the point of making it unusable for diagnosis.
  Fixed (Q515): both managers install a deduplicating warning handler — one line per unique message per process.

## 9f. What the dogfood run measured for the quota rung (Q462)

Same cluster and control plane as [§9e](#9e-what-the-dogfood-run-measured-q469), 2026-07-31.
Unlike the capacity rung, **the quota rung works on the scale-set tier**, and both of the quantities [§9b](#9b-what-the-port-shipped) left open came back inside their intended bounds.

**The harness.** The tenant `ResourceQuota` was tightened from `pods: 12` to `pods: 6` (restored afterwards) so the rung binds; the set used the tenant's normal placeable `default` template with `maxWorkers: 8` and `capacityGate.mode: Off`, so the *only* rung under test is quota.
Two `unit-test.yml` dispatches plus one `integration-test.yml`, sampled every 5s for ~11 min.

**The rung binds, continuously and correctly.** With 2 infrastructure pods against a hard limit of 6, the advertisement tracked `own active pods + headroom` capped at the ceiling, oscillating 3↔4 against a declared ceiling of 8:

```
adv=4  withheld{quota}=4     adv=3  withheld{quota}=5
```

`actions_gateway_scaleset_capacity_withheld{reason="quota"}` and `actions_gateway_scaleset_advertised_capacity` summed to the ceiling at every sample, which is the invariant §9b's "every evaluated rung publishes an explicit zero each poll" is there to keep readable.

**The bias-low margin is 0–2 jobs and never inverted.** Sampling GitHub's `totalAssignedJobs` against the set's own non-terminal worker pods across 75 samples:

| margin (`assigned − active pods`) | samples |
|---|---|
| 0 | 58 (77%) |
| +1 | 15 (20%) |
| +2 | 2 (3%) |

It was **never negative**.
That is the answer §9a asked for: counting the AGC's own pods rather than `totalAssignedJobs` under-advertises by at most 2 slots on this workload, and the over-advertising direction — the one that reproduces claim-and-stall — did not occur.
The bias-low choice costs little and holds.

Two readings that look like violations and are not, worth stating so the next person does not re-open them:

* **`assigned` briefly exceeded `advertised`** (4 against 3, at 00:51:33).
  The advertisement caps *new* assignment; GitHub does not un-assign work when the number drops, so a ceiling lowered under existing assignments is transiently below them by construction.
* **Terminal pods must be excluded from the AGC side.** Counting them made the margin read negative — `active` appeared to exceed `assigned` — purely because `Succeeded` pods linger.
  Neither `AdvertiseCapacity` nor the `ResourceQuota` counts them, so the margin must not either.

**Recovery from freed quota is ~30s, not a poll interval of pain.** §9b asked whether one poll interval of recovery latency is noticeable to a tenant whose quota frees mid-burst.
Measured: quota fell to 4/6 at 00:51:03 with 2 active pods against 4 assigned, and the AGC was back to 3 active by 00:51:15 and 4 by 00:51:33 — ~30s from headroom appearing to the pods existing.
Against dogfood CI job durations that is not a tenant-visible delay.

**What this does not cover.** One tenant, one namespace quota on `pods` only — not a CPU/memory-shaped quota, where `QuotaHeadroomPods`' binary search over `WorkerFootprint` does the real work and this run exercised only its trivial case.
And the classic tier's boolean rung remains unmeasured on a live cluster; this measures the integer form §9a ported.

## 9g. The latch — a bound that survives the reap (Q512)

[§9e](#9e-what-the-dogfood-run-measured-q469) measured the capacity rung removing zero wasted claims on the ScaleSet tier and named the arithmetic: the gate's only evidence is the stuck pod itself, the reaper deletes that pod at `pendingPodDeadline`, and clearing the condition restores the **full** advertisement — so a burst of *N* becomes *N* again, every window.
The classic tier survives the same clearing because it re-decides per delivered job; the integer tier has no per-job decision point, so the clearing must not mean "back to ceiling".
This section is the design for the bound that survives the reap; [§9h](#9h-what-the-dogfood-re-run-measured-for-the-latch-q513) is its dogfood measurement.

**The rule.** When every stuck pod has vanished *without evidence that capacity returned*, the condition does not clear — it **latches**: `WorkerCapacityDeclined` stays `True` with a new reason, `AwaitingProbe`.
The latch is broken only by evidence, in either direction:

* **Capacity returned** — a worker pod of this set has *scheduled* since the condition became `True` (its `PodScheduled=True` transition post-dates the condition's `lastTransitionTime`).
  The condition clears fully; the whole advertisement is restored.
* **Capacity still absent** — a new stuck pod re-earns the live verdict (`PodsUnschedulable` / `ScaleUpDeclined`), and the gate is back where it started, with fresh evidence.

Latch entry is deliberately narrow: only the *no-stuck-pods* outcome latches.
A not-declined verdict reached **with** stuck pods present — the autoscaler's acting signal, an unreadable Event list, an unrecognized vocabulary — clears the condition exactly as today, because there the fail-open contract (§5) and the concurrency-window protection ([§9d](#9d-the-concurrency-window-q478)) own the answer.
Scheduling evidence is compared by the pod's `PodScheduled` transition time, not its creation time, so a burst pod that finally squeezes in also counts as capacity returning — whenever it was created.

**The probe slot.** While latched, the rung's two forms both allow exactly one job through per deadline window — the integer trickle the tier never had:

* `DeclinedCapacity` (scale-set): bound = own non-terminal worker pods, **plus one** when no *probe* — a non-terminal worker pod created since the condition became `True` — is outstanding.
  The slot admits one job; the pod it produces is the probe.
  If it schedules, the latch clears; if it sticks, the live verdict returns and the bound drops back to `active`.
* `CapacityDeclined` (classic): while latched, declined *iff* a probe is outstanding — so exactly one delivery is admitted per window here too.
  This is Q443's invariant applied to the latch itself: a latch expressed only in the integer form would ship the fix to one tier, and it would also *starve* the classic tier outright (a latched `True` with no probe path would refuse every delivery, and no probe pod would ever exist to clear it).

Under a live (non-latched) decline both forms behave exactly as shipped: bound = `active`, declined = true.
The probe slot exists only in the latched state.

**Why this cannot starve a tenant.** The latch never fully closes intake — its floor is one probe slot per window, which is the classic tier's designed trickle rate (§5).
A wrongly-held latch costs at most one poll-plus-reconcile of delay: the probe schedules, the Pod watch fires on the phase change, and the reconciler clears the condition.
That bounded cost is why the latch may hold on *absence* of evidence even though the rest of the rung fails open on it — the failure mode is a briefly-throttled tenant, never a starved one.

**What this deliberately does not fix.** The first batch is still wasted — the gate cannot close before the first pod sticks, and assignment is batch-granular per long-poll (§9e).
Steady-state waste drops from *N* per window to ~1 per window, which is the rate §8's trigger (a) asks to re-measure before Phase 3 is armed.
The ~`pendingPodDeadline/2` scheduling grace before the gate first closes is also unchanged: the grace is what keeps the sibling `WorkersUnschedulable` condition from flapping, and shortening it for the gate alone would re-split the "one stuck-set definition" §5 unified.

**Operator surface.** `AwaitingProbe` joins the condition vocabulary (`api/apiconditions`, re-exported per version): `True/AwaitingProbe` reads "the declined evidence was reaped; intake is limited to one probe job until a worker pod schedules".
On an idle gated set whose shape stays unplaceable the condition now persists `True` indefinitely — truthfully — where it previously flapped with every reap cycle.
The withheld gauge (`actions_gateway_scaleset_capacity_withheld{reason="capacity"}`) now shows sustained withholding (`ceiling − active − 1`) across windows instead of the §9e sawtooth that always returned to zero.

## 9h. What the dogfood re-run measured for the latch (Q513)

Measured 2026-07-31 on `gag-dogfood`, against a control plane built from `2715e7f8` — the first post-latch (Q512) main.
Same harness as [§9e](#9e-what-the-dogfood-run-measured-q469): two ScaleSet RunnerSets differing only in `capacityGate.mode`, a `RunnerTemplate` naming a nonexistent node pool, `maxWorkers: 8`, `pendingPodDeadline: 2m`, one `unit-test.yml` dispatch (7 GAG-routed jobs) per arm, arms run sequentially on fresh set names (per the gke-dogfood B7 replay rule).
Sampled every 10s from the AGC's mTLS metrics endpoint plus the RunnerSet condition.

**The headline: every §9g prediction held.** Steady-state waste dropped from *N* per window to exactly 1, the condition latched instead of flapping, and the §9e sawtooth is gone.

| | `mode: Off` | `mode: Observe` (latched) |
|---|---|---|
| Wasted claims, first 12.5 min | **21** (3 batches × 7) | **8** (first batch of 7 + 1 probe) |
| Steady-state rate | 7 per window | **1 per window** (probes at 07:25, 07:30, 07:35, 07:40 — reaped 8→9→10→11) |
| Advertisement after a reap | full ceiling (8) — the §9e sawtooth | **pinned at 1** (the probe slot), `withheld{capacity}=7` sustained |
| `WorkerCapacityDeclined` | absent throughout | `True` continuously after first close — alternating `ScaleUpDeclined` (probe stuck) ↔ `AwaitingProbe` (probe reaped) |

The Off control reproduced §9e exactly (21 reaps, 3×7, ~5 min batch period), so the delta is the latch, not a changed environment.

One measured cycle of the latch arm, from the sampler:

```
07:20:48  adv=8  pods=7  cond=False/CapacityAvailable      <- burst assigned whole (batch-granular)
07:21:45  adv=8  pods=7  cond=True/ScaleUpDeclined         <- gate closes: deadline/2 grace (~60s)
07:22:43  adv=7  pods=0  cond=True/AwaitingProbe           <- reap; condition LATCHES, does not clear
07:23:17  adv=1  withheld=7                                <- next poll: probe slot only
07:25:43  adv=1  pods=1  cond=True/AwaitingProbe           <- GitHub assigns 1 (not 7): the probe
07:26:40  adv=1  pods=1  cond=True/ScaleUpDeclined         <- probe sticks; live verdict re-earned
07:27:47  adv=1  pods=0  cond=True/AwaitingProbe           <- probe reaped; latch holds; repeat
```

Two timings worth recording:

* **The practical window is ~5 min, not the 2 m deadline** — reap at 2 m plus ~3 min of GitHub-side re-assignment latency before the probe job lands (the same lag §9e's Off batches showed).
  The steady-state burn rate is therefore ~1 claim / ~5 min at this deadline, and scales with `pendingPodDeadline`.
* **The condition never flapped through `False`.** `WorkersUnschedulable` cleared at each reap (its evidence is the pods) while `WorkerCapacityDeclined` held `True` — the decoupling §9g designed.
  The `AwaitingProbe` message names the reaped evidence and the reopening rule verbatim from the operator doc.

Corroborated in passing: CA's `NotTriggerScaleUp` again landed 0–1 s after pod creation, and the nonexistent-pool shape emitted a two-category body (`1 node(s) had untolerated taint(s), 1 node(s) didn't match Pod's node affinity/selector`) — classified by category, never parsed, per §9c.

**What this hands the Phase 3 decision.** §8's trigger (a) now has its number: residual burn under rate-bounding is ~1 claim per ~(deadline + reassignment) window, only while jobs are actually queued against an unplaceable shape.
For this repo's CI that is acceptable; a GPU/spot operator for whom even that rate is expensive is trigger (a)'s remaining case.
The first batch remains unbounded by design (assignment is batch-granular; the gate cannot close before the first pod sticks), and the classic tier's boolean rung remains unmeasured live.

## 9i. The Karpenter arm of the drift gate, and what it measured (Q479)

Shipped 2026-07-31.
[§9c](#9c-the-live-autoscaler-harness-and-what-it-measured-q474) covered cluster-autoscaler only; this is the arm §9c flagged as needing a live counterpart most.
Karpenter's declination shares kube-scheduler's reason string (`FailedScheduling`), so the matcher's whole Karpenter arm *is* the reporter discrimination — and its failure mode is double-silent: an upstream attribution change makes every Karpenter declination read as scheduler noise, the gate never closes on any Karpenter cluster, and recorded samples carry the old attribution forever.

**The harness.** `make karpenter-cluster` stands up a throwaway kind cluster running a real upstream Karpenter on its [kwok provider](https://github.com/kubernetes-sigs/karpenter/tree/main/kwok).
One structural difference from the CA arm: upstream publishes **no image** for this provider (its own workflow is `ko build` from a checkout), so `scripts/e2e/karpenter-cluster.sh` clones the pinned `KARPENTER_VERSION` tag, builds the binary with the repo's Go toolchain, and reproduces ko's output shape (static binary, empty base — `test/karpenter/Dockerfile`). `make test-karpenter` then drives three cases (`karpenter_verdict_live_test.go`, build tag `karpenter`): the declination read through the discrimination with the scheduler's identically-named event in the same list, a nomination that must leave the gate open and land the pod on a node that did not exist, and the recorder generation.
CI runs it beside the CA arm in `autoscaler-drift.yml`; [`updatecli.d/karpenter.yaml`](../../updatecli.d/karpenter.yaml) moves the pin weekly to the latest upstream release (Q529).

**Measured against Karpenter v1.14.0 / Kubernetes 1.36.1, 2026-07-31.**

* **The reason vocabulary and attribution hold.** `FailedScheduling` and `Nominated`, attributed on *both* fields (`source.component` and `reportingComponent` read `karpenter`) — same double attribution §9c measured for CA, so `reportedByScheduler`'s fallback reads the same answer either way.
* **Only the message prefix is stable.** `Failed to schedule pod, ` held, but the recorded sample's body (`incompatible with nodepool "default", daemonset overhead=…, no instance type satisfied resources`) no longer appears at v1.14.0.
  The measured bodies vary by failure shape: `incompatible requirements, key karpenter.sh/nodepool, …` for a pool-selector mismatch, `no instance type has enough resources, requirements=…` for an oversized pod.
  Both are now unit rows (classified identically — the matcher never parses a body), the live test pins only the prefix, and the runbook's "which one" list was reworded to match.
* **The microsecond-generation premise was wrong.** Karpenter records through the **legacy** recorder — `firstTimestamp`/`lastTimestamp` set, `eventTime` null — the same generation as CA, at the same one-second resolution. §9c and §9d shipped believing Karpenter used the microsecond generation (source inspection, never measured; the operator runbook repeated it as "Karpenter sets eventTime").
  All three claims are now corrected.
  The §9d window loses nothing: it was already justified as making the verdict independent of recorder generation, and the live recorder-generation case is what will notice when either project migrates.
* **Timing.** Karpenter's declination lands 1–2 s after pod creation (its batch idle window is 1 s — no CA-style 10 s scan to wait out), and nomination→new-node→pod-scheduled completed in ~4 s under kwok's fast stages.

**Upstream finding, worked around in the recipe.** The kwok chart at v1.14.0 renders `featureGates.staticCapacity` and `.capacityBuffer` into the `FEATURE_GATES` env var, but its own `values.yaml` omits both keys — the empty string panics the controller at startup (`invalid value of StaticCapacity`).
The harness sets both to their defaults explicitly ([Q531](../STATUS.md#Q531) tracks reporting it upstream).

## 9j. The gap is operator-visible in shipped docs (Q714)

Q714's mechanism was a source read.
It is now measured.
A pod running the exact placeholder the 1.4 DinD templates ship, `example.invalid/build-capable-runner:replace-me`, was applied to a kind cluster and its conditions read directly:

| t | What the pod reports |
|---|---|
| +0s | `Scheduled`, bound to a node, `PodScheduled=True` with an empty reason |
| +1s | `Pulling` |
| +2s | `ErrImagePull`, then `BackOff` and `ImagePullBackOff`; the `Failed` event names the unresolvable host |
| onward | phase stays `Pending` |

`PodScheduled=True` is the confirmation: `podUnschedulable()` requires `PodScheduled=False` with reason `Unschedulable`, so it returns false for the whole window. `WorkersUnschedulable` never trips, and neither `actions_gateway_workers_unschedulable` (v1) nor `actions_gateway_runnerset_workers_unschedulable` (v2) leaves 0.
Both API versions share this, since `RunnerSetReconciler` reuses `evalWorkersUnschedulableForPods` and `PendingPodDeadlineOrDefault`.

The pod is then reaped at `pendingPodDeadline` (default 10m) with a `WorkerPodStuckPending` Warning, which `E2E_AGC_StuckPendingPodReaped` already exercises on the same `.invalid` shape.
Recovery does not loop tightly: the abandoned-run sweeper waits for capacity that an unpullable image never returns and expires after 30m.

**Why this matters beyond intake.** Both 1.4 DinD templates ship that placeholder and require the operator to replace it, so this is the failure a first-time user of the library hits, not a corner case.
[`runner-template-library.md`](../operations/runner-template-library.md) and [`kata-dind-workloads.md`](../operations/kata-dind-workloads.md) now describe the real split: the pod says `ImagePullBackOff` within seconds, while the `RunnerSet` stays quiet until the deadline.
Closing Q714 changes what an operator watches, so it must update both.

## 10. Non-goals

* Predicting schedulability in-process from node allocatable.
  Rejected: it reimplements the scheduler's filter plugins and will drift ([D.8](../design/appendix-d-alternatives-considered.md#d8-gating-intake-on-capacity-which-signals-are-safe-to-gate-on)).
* Pre-warmed placeholder/pause pods.
  That trade (idle compute for correct optimism) is the warm-worker-pool item, [G.12](../design/appendix-g-future-enhancements.md#g12-warm-worker-pool-minidleworkers)/[Q268](../STATUS.md#Q268).
* Cross-RunnerSet coordination on a shared pool (§4 residual).
* A pluggable autoscaler-provider interface, or coverage of every autoscaler.
  Two matchers cover both open-source event vocabularies across ~46 provider implementations, and fail-open covers the rest (§7).
* Auto-detecting cluster elasticity to pick a mode.
  Open question, not scope: a reconcile-time *warning* when a mode's precondition looks violated (an autoscaler is present but the set asked for `SchedulerVerdict`) is cheap and would prevent the one real footgun here.
* Making any mode the default, in any phase. `Off` stays the default until live measurement says otherwise, per the secure/conservative-default stance.
* v1 `RunnerGroup` support.
