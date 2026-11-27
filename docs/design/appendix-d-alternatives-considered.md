# Appendix D — Alternatives Considered

← [Appendix C](appendix-c-ai-implementation.md) | [Back to index](README.md) | Next: [Appendix E — Capacity Planning →](appendix-e-capacity-planning.md)

---

This appendix documents the self-hosted runner approaches that were evaluated before settling on the four-tier gateway design.
Each alternative is a legitimate solution for some deployment contexts; the goal here is to be explicit about the trade-offs that make them insufficient for the specific requirements of high-scale, multi-tenant, GPU-capable Kubernetes clusters.

The requirements driving this evaluation were: goroutine-level session multiplexing to eliminate idle pod overhead, per-tenant egress IP isolation, zero-idle compute between jobs (scale-to-zero), and self-service tenant onboarding without cluster-admin involvement per team.

---

## D.1. GitHub-Hosted Runners

GitHub's documentation carries no version, so claims from it are dated by when they were read.

**Advantages**

* No cluster of your own is needed: GitHub hosts each runner and takes care of machine maintenance and upgrades ([GitHub-hosted runners](https://docs.github.com/en/actions/concepts/runners/github-hosted-runners), read 2026-11-26).
* Standard runners are free in public repositories, and plans include a compute allowance expressed as money rather than minutes ([Actions billing](https://docs.github.com/en/billing/concepts/product-billing/github-actions), read 2026-11-26).
* Ubuntu Linux, Windows, and macOS are offered, with arm64 rates listed for Linux and Windows (same two pages, read 2026-11-26).

**Disadvantages**

* Reaching a private network takes setup: larger runners offer static IP ranges and Azure private networking, on Linux and Windows only ([larger runners](https://docs.github.com/en/actions/concepts/runners/larger-runners), read 2026-11-26).
  Those narrow the gap, and larger runners can be GPU-powered (same read), so the gaps here are about hardware you own rather than about reachability.
* The hardware is GitHub's (first page above, read 2026-11-26), so GPUs already bought, reserved capacity, and on-premises or air-gapped machines do not run these jobs.
* Custom images work only with larger runners ([larger runners](https://docs.github.com/en/actions/concepts/runners/larger-runners), read 2026-11-26).
* Larger runners are billed per minute, in public repositories too (same read).
  Whether that costs more than self-managed compute at a given job volume is unverified here.
* A runner group restricts which organizations and repositories can use runners and can set concurrency limits ([runner groups](https://docs.github.com/en/actions/concepts/runners/runner-groups), read 2026-11-26).
  Whether those limits can serve as a cap a platform team sets and a tenant cannot raise is unverified.

**Verdict:** Appropriate for teams whose jobs can leave the clusters they operate and who do not need a boundary between a platform team and its tenants.
Does not satisfy the owned-hardware, multi-tenant cluster requirements driving this design.

---

## D.2. Naive Self-Hosted Runners (No Controller)

The baseline approach: register runner processes directly with GitHub, either as static pods in Kubernetes or on dedicated VMs, with no automation layer managing lifecycle.

**Advantages**

* No operator or Custom Resource Definition (CRD) to run: the runner application connects to GitHub for job assignments by itself ([self-hosted runners reference](https://docs.github.com/en/actions/reference/runners/self-hosted-runners), read 2026-11-26, when the latest runner was v2.338.2).
* The runner updates its own software by default (same read).

**Disadvantages**

* A static runner has to be running before it can receive a job, so each slot holds its pod or VM whether or not jobs are queued (inferred from the model above, unmeasured).
  That one-process-per-slot shape is the core problem this design solves.
* Adding and removing capacity is yours to script, and idle capacity stays until you remove it (inferred from the baseline having no controller, unverified).
* A registration token expires after one hour ([REST API](https://docs.github.com/en/rest/actions/self-hosted-runners), read 2026-11-26), so a later registration needs a fresh one.
* GitHub removes a runner that has not connected for more than 14 days, or 1 day for an ephemeral one ([removing runners](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/remove-runners), read 2026-11-26).
* Runner groups restrict which repositories can use a runner ([runner groups](https://docs.github.com/en/actions/concepts/runners/runner-groups), read 2026-11-26).
  Compute isolation between teams is whatever the host provides (unverified).
* Nothing in this baseline assigns an egress identity, so runners share whatever source IPs their hosts use (inferred, unverified).
* A static runner on a GPU node keeps its GPU allocated between jobs (inferred, unmeasured).

**Verdict:** Can serve a small team with a handful of runners.
It does not meet the scale-to-zero requirement, and it brings no tenant provisioning or egress isolation of its own (inferred, as above).

---

## D.3. Actions Runner Controller (ARC)

[ARC](https://github.com/actions/actions-runner-controller) is a Kubernetes operator for self-hosted runners that GitHub documents and supports ([support for ARC](https://docs.github.com/en/actions/concepts/runners/support-for-arc), read 2026-11-26).
It is the most relevant comparison for this design.

**Advantages**

* GitHub Support covers the latest Autoscaling Runner Sets version of ARC, on GitHub Enterprise Server (GHES) 3.9 and later ([support for ARC](https://docs.github.com/en/actions/concepts/runners/support-for-arc), read 2026-11-26).
  Orchestration, networking and policy application are among the things it leaves out of scope (same read).
* The same protocol dependency, since `v2.0.0`.
  Both acquire jobs through the official `actions/scaleset` client library ([ARC 0.15.1 `go.mod`](https://github.com/actions/actions-runner-controller/blob/gha-runner-scale-set-0.15.1/go.mod), read 2026-11-26).
  Until `v2.0.0` this design also carried a classic tier that re-implemented part of the broker API ([§3.3](03-api-contracts.md#33-re-implemented-broker-api-endpoints)); that tier was [removed at `v2.0.0`](03-api-contracts.md#31-kubernetes-crd-schemas), so the maintenance risk it carried is no longer a difference between the two.
* Scale-set mode runs ephemeral runner pods on demand and none at rest ([comparison table](../why-gag.md#gag-vs-arc-scale-set-mode), ARC 0.15.0 read 2026-10-04).
* **`containerMode: kubernetes`** runs a job's `container:` and `services:` steps as separate unprivileged pods sharing a `ReadWriteMany` volume (ARC `gha-runner-scale-set` 0.14.2, measured 2026-08-06, [D.15](#d15-pod-per-step-container-execution-arcs-containermode-kubernetes)).
  GAG has no equivalent and will not adopt this mechanism: it requires a namespace-wide pod-`create` grant in a namespace that holds other runner sets' registration credentials (same reading).
  The supported GAG answer is Docker-in-Docker under Kata, and the reasoning, the alternatives, and the population that answer does not serve are in [D.15](#d15-pod-per-step-container-execution-arcs-containermode-kubernetes).

**Disadvantages**

* **Listener packaging.** In `RunnerScaleSet` mode, ARC uses one listener per scale set, not one per slot ([comparison table](../why-gag.md#gag-vs-arc-scale-set-mode), ARC 0.15.0 read 2026-10-04), so its steady-state long-poll connection count is similar to this design's adaptive listener model.
  The difference is packaging: ARC deploys each listener as its own always-on pod holding a pod IP (same read), while the Actions Gateway Controller (AGC) runs every listener as a goroutine in one shared pod, at a measured ~7.7 KiB of AGC state per scale set ([Appendix A](appendix-a-capacity-slos.md)).
  At a tenant operating 10 RunnerScaleSets, that is 10 always-on listener pods and 10 pod IPs for ARC versus 1 AGC pod and 1 pod IP here (same reading).
  (No per-listener memory ratio is published: there is no measured ARC-side figure to compare against, and [Appendix A](appendix-a-capacity-slos.md) records why the earlier ~4,000× claim was retired.)
* **Multi-tenant isolation.** ARC offers no self-service multi-tenancy model: network policies and resource quotas per tenant take cluster-admin work (unverified).
  This design's `ActionsGateway` CR lets a team provision an isolated gateway instance within their existing namespace without cluster-admin.
* **Egress IP isolation.** ARC points at a proxy you already run and provisions no per-tenant egress addresses ([comparison table](../why-gag.md#gag-vs-arc-scale-set-mode), ARC 0.15.0 read 2026-10-04).
  This design's per-tenant `EgressProxyPool` (Tier 3) provides this natively.
* **GPU idle cost is not a difference in scale-set mode.** Both delete the runner pod when its job completes and run no worker at rest ([comparison row](../why-gag.md#gag-vs-arc-scale-set-mode), ARC 0.15.0 read 2026-10-04).
  Where the two part is what happens when the cluster takes a running GPU worker away: this design re-runs the job, and ARC gives it up (same table and read).
* **No shared ceiling across runner sets, and no reserved floor within one.** ARC does express scheduling *ordering* natively: `AutoscalingRunnerSetSpec.Template` is a full `corev1.PodTemplateSpec`, the `gha-runner-scale-set` chart copies every `template.spec` key it does not reserve for itself (`containers`, `initContainers`, `volumes`, `serviceAccountName`), and the `EphemeralRunner` controller assigns that `PodSpec` to the runner pod wholesale (`newPod.Spec = runner.Spec.Spec`), so a `priorityClassName` set on a scale set reaches its runner pods and preempts lower-priority ones (measured against ARC `master`, 2026-08-12).
  What ARC cannot express is a *bounded* reservation, in either direction ([comparison table](../why-gag.md#gag-vs-arc-scale-set-mode), ARC 0.15.0 read 2026-10-04).
  The floor: priority is a property of the scale set's one pod template, so every runner in the set carries the same class (same read).
  "The first 5 GPU runners preempt, the rest schedule opportunistically" requires the class to vary by pod index within a single set, and a set whose whole template preempts starves the sets beneath it rather than reserving a slice of the pool (inferred from the same read).
  The ceiling: `maxRunners` is a per-`AutoscalingRunnerSet` property with no mechanism to express a shared budget across multiple sets (same read).
  A team with three sets each capped at 50 can theoretically schedule 150 concurrent runners, exceeding the namespace's actual resource capacity, so "all sets combined may use at most 100 concurrent jobs" takes a `ResourceQuota`, external tooling, or hand-coordinated caps (inferred from the same read).
  This design bounds all worker pods from all `RunnerGroup`s against the same Kubernetes `ResourceQuota` for the shared ceiling, and adds a `priorityTiers` field on each `RunnerGroup` for the floor: the first N pods of a high-priority group are assigned a preempting `PriorityClass` that displaces lower-priority pods when the namespace is contended, while additional pods above the preemption threshold schedule opportunistically.
  Both are expressed declaratively in the `ActionsGateway` CR and enforced by the Kubernetes scheduler, with no external tooling or manual cap coordination.
* **The protocol dependency is shared.** Both reach the runner-scale-set protocol through the same client library ([ARC 0.15.1 `go.mod`](https://github.com/actions/actions-runner-controller/blob/gha-runner-scale-set-0.15.1/go.mod), read 2026-11-26), so a breaking change there reaches both.
  This design records each GitHub-facing protocol, its source of truth and a stability tier in the [protocol dependency register](github-protocol-dependencies.md); a search of ARC's repository for "stability tier" and "protocol dependency" found no equivalent (0 code hits and one unrelated closed issue, 2026-11-26).

**Verdict:** ARC is the right choice where its GitHub Support entitlement or `containerMode: kubernetes` (both dated above), GHES tested on a real appliance, or install base (both unverified) decides the evaluation.
This design has none of those four, and a capability list does not offset them.
This design is the right choice for a cluster several teams share: a platform-owned quota the tenant controller has no verb to write, intake that reads that quota before claiming, a `priorityTiers` floor under the shared ceiling, re-run of a job the cluster disrupts, isolation reconciled per tenant, per-tenant egress identity, and self-service from namespaced objects, which ARC does not provide natively (per the reads dated above; the self-service point is unverified).
A single team on its own cluster gets the same behaviour, and the cost of running this design for one team against ARC is unmeasured.

---

## D.4. ARC with KEDA Autoscaling

This section weighs layering [KEDA](https://keda.sh/), an event-driven autoscaler, on top of ARC with a `ScaledObject` driving the runner count.
That layering does not fit scale-set mode:

* ARC's listener already sets the runner count itself, by patching the `EphemeralRunnerSet` ([ARC concepts](https://docs.github.com/en/actions/concepts/runners/actions-runner-controller), read 2026-11-26).
* A `ScaledObject` can target a custom resource only if it defines a `/scale` subresource ([KEDA 2.21 `ScaledObject` spec](https://keda.sh/docs/latest/reference/scaledobject-spec/), read 2026-11-26).
* The `AutoscalingRunnerSet` CRD declares a `status` subresource and no `scale` one ([ARC 0.15.1 CRD](https://github.com/actions/actions-runner-controller/blob/gha-runner-scale-set-0.15.1/charts/gha-runner-scale-set-controller/crds/actions.github.com_autoscalingrunnersets.yaml), read 2026-11-26).
* KEDA's `github-runner` scaler reads queued jobs from the GitHub API, and its page does not mention ARC ([KEDA 2.21 scaler](https://keda.sh/docs/latest/scalers/github-runner/), read 2026-11-26).
  Autoscaling runners with it and without ARC is a different setup, which this appendix has not evaluated.
* Whether KEDA is useful with ARC's legacy mode is unverified.
  GitHub leaves that mode to community support ([support for ARC](https://docs.github.com/en/actions/concepts/runners/support-for-arc), read 2026-11-26).

**Verdict:** Not a separate alternative to scale-set ARC, so D.3's comparison applies.

---

Sections D.1–D.4 cover ways of running the runners themselves.
The two sections below cover *adjacent* Kubernetes tooling that is frequently raised alongside this design — a job-queue / quota manager and an infrastructure cost optimizer.
Neither is a self-hosted runner controller, so neither is a drop-in substitute; both are included because each overlaps part of the problem space (priority/quota arbitration; GPU/compute cost) and the boundary between "what this design does" and "what these tools do" is a common point of confusion.

## D.5. Kueue and Kubernetes Job-Queue / Quota Managers

[Kueue](https://kueue.sigs.k8s.io/) is the Kubernetes-native job queueing and quota manager maintained under the Kubernetes Special Interest Group (SIG) for scheduling.
It is the natural off-the-shelf tool to reach for when someone asks "why not just put a priority queue in front of the runners?", so the boundary between it and this design is worth stating explicitly.

**What it does.** Kueue arbitrates workloads against declarative quota.
Its core objects are `ClusterQueue` and `LocalQueue` (the quota and submission surfaces), `ResourceFlavor` (heterogeneous resource pools, e.g. GPU vs CPU), `Cohort` (quota borrowing between queues), and `WorkloadPriorityClass` (priority-ordered preemption).
Per its own documentation, Kueue "decides when a job should wait, when a job should be admitted to start (as in pods can be created), and when a job should be preempted."
It installs cluster-scoped objects, so deploying it needs cluster-scoped permissions.
Measured on the v0.18.8 Helm chart, 2026-09-10: it ships 11 Custom Resource Definitions (CRDs), ClusterRoles and their bindings, a `MutatingWebhookConfiguration` and a `ValidatingWebhookConfiguration`, and two `APIService` registrations for its visibility API.

**Where it overlaps.** Kueue's quota-and-priority model overlaps the same need this design addresses with a shared `ResourceQuota` ceiling plus per-`RunnerGroup` `priorityTiers`: keeping a high-priority runner type from being starved by a flood of lower-priority work, and expressing a shared budget across heterogeneous pools.
A cluster that already runs Kueue has a credible answer to the priority/quota half of the problem at the pod layer.

**The differentiator.** Kueue gates the *pod* layer; this design's admission decision has to happen one layer above it, at the GitHub broker.
A worker pod only exists *after* the Actions Gateway Controller (AGC) has already claimed the job from GitHub (`acquirejob`), at which point GitHub considers the job owned by that session and the job lock is ticking.
Kueue has no visibility into the broker and cannot defer a job that is not yet a Kubernetes workload; if it defers the *pod* after the claim, the work is queued while the lock the design must renew counts down — the exact failure the broker-layer admission gate exists to prevent.
So Kueue **augments** rather than **replaces** the design's gate: in a cluster that already runs Kueue, this design's worker pods can still participate in a `ClusterQueue` for cluster-wide quota and preemption at the pod layer, while the broker-layer decision of *whether to claim the job at all* stays upstream of anything Kueue can act on.
Kueue also needs those cluster-scoped permissions to install, which is in tension with this design's self-service-without-cluster-admin requirement, so making it a hard dependency would regress that goal.

The full argument — why admission is gated before `acquirejob` rather than delegated to an in-cluster queue, and why a durable internal queue was also rejected — is developed in the pre-acquisition admission-control plan (Q59; see [Relationship to Kueue](../plan/archive/acquire-admission-control.md#relationship-to-kueue-why-an-off-the-shelf-k8s-queue-isnt-the-admission-layer)) and is not duplicated here.

**Verdict:** Kueue is a strong fit for cluster-wide batch quota and priority arbitration at the pod layer, and composes with this design rather than competing with it.
It is not a substitute for runner-control-plane admission, because the decision that matters for GitHub Actions jobs — whether to claim a job from the broker — happens before any Kubernetes workload exists for Kueue to manage.

---

## D.6. Exostellar and Infrastructure / GPU Cost Optimizers

[Exostellar](https://exostellar.io/) is representative of a class of infrastructure cost-optimization tooling that is sometimes mentioned in the same breath as runner autoscaling because it targets the cost of expensive (especially GPU) compute.
It is included here to draw the layer boundary, not because it manages runners.

**What it does.** Per Exostellar's public materials, it offers two main capabilities.
The Exostellar Infrastructure Optimizer runs workloads inside virtual machines (VMs) on cloud instances and predicts spot-instance reclamation, live-migrating a VM to another spot or on-demand instance to keep the workload alive while capturing spot pricing.
Its Software Defined GPU offering provides vendor-agnostic, fractional GPU slicing through Kubernetes Dynamic Resource Allocation (DRA), partitioning GPUs beyond fixed Multi-Instance GPU (MIG) boundaries to raise utilization.
Both are aimed at lowering the unit cost of the underlying compute.

**Where it overlaps.** Only at the framing level of "make expensive GPU compute cheaper."
This design reduces GPU cost by holding *zero* idle GPU allocation between jobs — worker pods are provisioned when a job is acquired and release their compute on completion — so the comparison is real for anyone evaluating "how do I stop paying for idle GPUs."

**The differentiator.** Exostellar operates at the node / VM / GPU *infrastructure* layer: it optimizes the cost and packing of compute that has already been requested.
This design operates at the runner *control-plane* layer: it decides whether a worker pod needs to exist at all (goroutine-multiplexed virtual sessions with no per-runner pod at rest), provides per-tenant egress IP isolation, and offers multi-tenant self-service provisioning — none of which an infrastructure optimizer addresses.
The two are orthogonal and could compose: an infrastructure optimizer could pack the nodes that this design's ephemeral worker pods land on.

> **Unverified — treat as a hypothesis, not a claim.** Working notes for this analysis speculated that vendors such as Exostellar layer a queue manager (e.g.
> Kueue) beneath ARC for GPU/quota management.
> Public materials reviewed for this appendix describe Exostellar as an infrastructure / GPU optimizer (spot-VM migration and GPU slicing) and do **not** describe a GitHub Actions runner, ARC integration, or a runner-queue product.
> The "layered under ARC" pattern is therefore not asserted here.
> What can be stated with confidence is the layer distinction above: infrastructure optimizers and this design address different layers and are not substitutes.

**Verdict:** Infrastructure and GPU cost optimizers are complementary to this design, not alternatives.
They lower the cost of compute that is running; this design lowers cost primarily by ensuring compute is not running when no job needs it, and adds the runner-control-plane properties (multiplexing, egress isolation, multi-tenant self-service) that sit entirely outside an infrastructure optimizer's scope.

---

## D.7. Worker Right-Sizing: Why Built In, Not Bolted On

The worker right-sizing loop (Q359: per-job usage sampling → measured recommendations in `RunnerSet` status → opt-in sizing profiles applied at pod-build time; see [§2.2](02-architecture.md#22-tier-2--actions-gateway-controller-agc) and [Appendix H §H.7](appendix-h-v2-api-decomposition.md#h7-reference-integrity--runtime-conditions-not-admission)) is implemented inside the AGC rather than delegated to external sizing tooling.
That was a deliberate choice among four alternatives, and the deciding argument is structural, not preference — it is worth recording because "why didn't you just use VPA?" is the obvious question, and because the same structural facts explain why this capability is hard for other runner controllers to retrofit.

**The workload shape that breaks generic tooling.** A worker pod runs exactly one CI job and lives minutes.
Three consequences follow: pods can only be sized *at creation* (there is no steady state to converge on later); the sizing statistic that matters is the **per-job peak envelope** (p95/max of per-job peaks), not a time-weighted usage distribution; and there is no long-lived controller object with `/scale` semantics to group pods under (`replicas` is meaningless in a scale-to-zero design).

**Alternative 1 — stock Vertical Pod Autoscaler (VPA).** Fails on all three facts.
Its `targetRef` requires a `/scale` subresource `RunnerSet` cannot meaningfully offer; its actuation is evict-and-resize, which on a one-job pod means killing the CI job to resize it; and its recommender models long-running services (usage distributions with half-life decay, OOM-event feedback), the wrong statistic for run-to-completion work.
Recommendation dashboards layered on VPA (Goldilocks, Kubecost's request right-sizing) inherit the same foundation.
**Verdict:** structurally unfit, not merely inconvenient.

**Alternative 2 — VPA with a custom recommender.** VPA's recommender is pluggable, so a batch-aware recommender could in principle replace the stock model.
But the actuation and grouping problems remain (a webhook applying "Initial"-style values still needs a pod-grouping convention VPA does not have for `/scale`-less owners), so this path amounts to writing the hard parts from scratch *inside someone else's framing* — more total machinery than the native implementation, spread across more failure domains.

**Alternative 3 — an external recommender closing the loop through GitOps.** A cron or pipeline queries the Phase 1 Prometheus series, derives values, and opens pull requests against the tenant's `RunnerTemplate`.
This is a legitimate design — auditable in git, zero new API surface — and nothing in GAG prevents an operator from running exactly this today on top of the exported metrics.
It was rejected as *the* design because it requires a Prometheus (the Phase 1 decision deliberately avoided a hard external dependency), closes the loop on a days-scale cadence, cannot express per-container confidence gating at pod-build time, and pushes per-tenant automation onto every adopter — the opposite of the batteries-included posture that motivates the feature.

**Alternative 4 — a from-scratch, general-purpose batch right-sizer.** The serious contender.
Designed fresh for run-to-completion pods it would be: a `PodSizingPolicy`-style CRD with a **label selector** (solving the grouping problem correctly), the same peak-per-pod-lifetime sampler and p95-of-peaks-plus-headroom recommender, and a **mutating admission webhook** as the only generic pod-creation actuation hook.
This tool would genuinely generalize — Kubernetes Jobs, Tekton TaskRuns, Argo Workflows, and ARC's ephemeral runners all share the one-pod-one-unit-of-work shape — and no good open-source implementation of it exists.
It was not chosen because the webhook is an entire failure domain the native design simply does not have: `failurePolicy: Fail` makes a sick *optimization* component block every matching pod (the CI fleet stops launching), `Ignore` makes sizing silently stop; either way the tool needs its own certs, chart, Tier-0 security review, and install step — and GAG's headline feature would begin "first install this other thing."
The AGC's provisioner already *is* the pod creator, so native actuation is an in-process function call with direct access to confidence state: strictly less machinery, no availability coupling, and the recommendation surfaces on the tenant's own `RunnerSet` under their existing RBAC.

**What the coupling costs, honestly.** The sizing API (`spec.sizing`, `status.sizingRecommendation`) is permanent GAG surface carried into the v2beta1 graduation, and a built-in feature cannot serve other batch systems the way Alternative 4 could.
That deferred generality is deliberately kept cheap to revisit: the sampler/histogram/derivation core touches GAG at exactly two narrow seams (the worker-pod owner label and the v2 status types), and [Appendix G §G.15](appendix-g-future-enhancements.md#g15-extract-the-batch-right-sizer-into-a-standalone--reusable-tool) records the extraction path and its triggers.

**The competitive corollary.** The same three structural facts apply to ARC: its ephemeral runner pods are `/scale`-less, one-job, minutes-lived — so stock VPA cannot size them either, and only ARC's own controller (or an Alternative-4-style webhook tool that does not exist) could actuate at pod creation.
A measured sizing loop is therefore a capability that effectively must live *inside* a runner controller, which is why "measure → recommend → apply, built in" is a durable differentiator rather than a feature gap ARC closes by adding a sidecar tool.

**Verdict:** For this workload shape, the sizing loop belongs in the controller that builds the pods.
Generic tooling fails structurally (D.7 alternatives 1–2), the GitOps loop remains available to operators who prefer it (alternative 3), and the general-purpose tool (alternative 4) is a valid *future extraction* of the shipped core, not a better first implementation.

---

## D.8. Gating Intake on Capacity: Which Signals Are Safe to Gate On

The pre-acquisition admission gate refuses to claim a job from GitHub when the worker pod that job needs cannot be provisioned, because a claimed job holds a single-use JIT runner record and a ticking job lock.
Two rungs are implemented: the owner's declared worker ceiling (Q59) and observed namespace-`ResourceQuota` headroom (#784).
A third, obvious-looking rung is deliberately **not** implemented on the same terms: the scheduler's own `Unschedulable` verdict, which the `WorkersUnschedulable` condition already publishes as observability.

That looks inconsistent, and the reason it is not is worth recording, because it does not appear to be written down anywhere in the ecosystem and it explains why every other runner controller settled for timeouts instead.

**The principle.** A capacity signal is safe to gate intake on if, and only if, **no other actor is waiting on that signal to make capacity appear.** Gating suppresses the signal; suppressing a signal that something else acts on destroys the rescue.
It is not specific to runners or to GitHub: it generalises to any admission controller sitting above an autoscaler, which is why [why-gag.md](../why-gag.md#rows-with-fine-print) carries the reader-facing half and this section keeps the derivation.

Applied to the four signals:

| Signal | Is it an input to another actor? | Safe to gate on |
|---|---|---|
| `ResourceQuota` headroom | No. No autoscaler adds a node because a namespace quota is full. Self-clearing as in-flight jobs finish. | Yes, unconditionally (#784) |
| Scheduler `Unschedulable` verdict | **Only if a cluster autoscaler is running.** A Pending unschedulable pod *is* the request for a node. | Conditionally: yes on a cluster that cannot grow, no otherwise |
| Autoscaler declination (`NotTriggerScaleUp`, Karpenter `FailedScheduling`) | No. The actor already evaluated this pod and declined, so nothing further is pending on it. | Yes |
| `ProvisioningRequest` `check-capacity` answer | No. Asking *is* the request for capacity, so the trigger is not forfeited. | Yes (Appendix [G.16](appendix-g-future-enhancements.md#g16-provisioningrequest-pre-acquire-capacity-probe-check-capacity)) |

Three consequences follow.

**Elasticity is a property of the cluster, not of the signal.** The scheduler's verdict is the *same* fact on every cluster; only the presence of an autoscaler changes whether acting on it is safe.
So the choice cannot be made once in code.
It is an operator input, and on a fixed-size cluster (on-premises, a contracted node count) the cheapest rung is also the correct one, because no rescue was ever coming and every wasted claim is pure loss.

**Even where gating is safe, it should rate-limit rather than hard-stop.** A gate derived from the existence of a stuck pod is self-clearing: intake stops, the pod is reaped at `pendingPodDeadline`, the condition clears, one job is claimed, and the cycle repeats.
A burst of *N* wasted claims becomes roughly one per deadline window while a Pending pod remains present for much of it, which keeps any autoscaler being asked and keeps the tenant discovering recovery.
Rate-bounding, not elimination, is the achievable property.

**Predicting placement in-process is not a fourth option.** Reimplementing the scheduler's filter plugins (taints, affinity, topology spread, DRA, extended resources) is a large surface that will drift from the scheduler.
`WorkersUnschedulable` deliberately reads the scheduler's verdict rather than guessing, and any capacity rung should delegate for the same reason.

**Verdict:** the quota rung is unconditionally safe and is implemented; the scheduler-verdict rung is safe exactly where nothing will act on the pod, so it belongs behind an explicit, off-by-default operator choice; the autoscaler's own declination and a solicited `ProvisioningRequest` answer are both safe and differ only in cost.
The sequencing of those rungs is planned in [capacity-aware-intake.md](../plan/capacity-aware-intake.md).

---

Sections D.1–D.8 cover the alternatives that were weighed while designing this system.
The sections below cover *other runner control planes and CI systems* an adopter may already be running or actively evaluating.
They were added after a competitive review on 2026-08-06; each records what the alternative does, where it overlaps, the differentiator, and a verdict, on the same terms as the sections above.
**Every claim about a third-party system is dated, because they move.**

## D.9. ForgeMT and Cloud-Identity Tenant Boundaries

[ForgeMT](https://github.com/cisco-open/forge) (Cisco, Apache-2.0; measured 2026-08-12: 211 stars, created 2025-05-12, last pushed the same day) is the closest *positional* competitor found: an explicitly multi-tenant, self-hosted GitHub Actions runner platform aimed at platform teams serving many internal tenants.

**What it does.** Terraform and Terragrunt deliver a runner control plane and an operating model across an AWS organization.
It runs two execution backends in parallel: ephemeral EC2 runners, and ARC runner scale sets on EKS.
Tenant onboarding is a reviewed infrastructure-as-code change.
Per its README it enforces tenant boundaries for labels, IAM and OIDC role access, networks, images, runner specs, and GitHub App scope.

**Where it overlaps.** The problem statement is nearly identical to this design's: many tenants, one platform team, self-hosted runners, governance and onboarding automation as first-class concerns.
It also ships cost-attribution and observability integrations, which this design addresses through [cost attribution](../operations/cost-attribution.md).

**The differentiator, and it is a genuine architectural disagreement.** ForgeMT draws the tenant boundary in cloud identity and forge scope rather than in the cluster (its docs, read 2026-08-12).
Its own vocabulary defines a tenant as the isolation and configuration boundary for a team: labels, runner settings, GitHub App scope, and AWS access, under a stated model of shared platform, isolated execution (`docs/architecture.md`).
The AWS account layout is an operator input, not a product decision: onboarding intake asks for the account IDs the operator already runs, and tenants are Terragrunt directories under one shared environment, region, and VPC root (`docs/operations/tenant-onboarding.md`).
Where a job needs a harder boundary than a pod, the answer is the EC2 lane, a full VM per job from a platform-owned AMI.
On the ARC lane tenants share an EKS cluster selected by `arc_cluster_name`, and whether they also share nodes is one of the decisions the security guide hands to the operator, alongside namespaces, service accounts, pod identity, and network policy (`docs/security.md`).
Two things follow, and both are trade-offs rather than refutations:

* **Every enforcement point is an AWS API, so the boundary does not port.** IAM and OIDC role scope, VPC and subnet placement, AMIs, SSM for the GitHub App key, and ECR for images are what hold one ForgeMT tenant apart from the next.
  Off AWS there is no substitute for any of them, and compliance, data residency, and reserved GPU capacity are precisely the constraints that put an adopter on-premises, so for that adopter the platform does not apply at all.
  This design's enforcement points are Kubernetes API objects, so they land wherever a conformant cluster does, including air-gapped.
* **The boundary is applied rather than reconciled, and it stops at the cluster edge.** A ForgeMT tenant is correct as of the last `terragrunt apply`, which buys a reviewable diff per onboarding that a single `ActionsGateway` CR does not match.
  What it does not buy is a floor that re-asserts itself, and on the ARC lane the in-cluster half is a checklist rather than a default: namespaces, network policy, pod identity, privileged containers, and node sharing are all left to the operator to decide and review.
  This design reconciles that floor instead ([Pod Security Admission, default-deny NetworkPolicy, per-tenant egress](05-security.md)), keeps the cap in a platform-owned `ResourceQuota` the tenant controller has no verb to write, and makes kernel isolation a per-workload choice inside the shared pool ([validated](../operations/kata-dind-workloads.md)) rather than a second lane provisioned per job.
  See [appendix B](appendix-b-worker-isolation.md) for the full worker-isolation analysis.

**Verdict:** ForgeMT is a strong fit for an AWS-native organization that wants its tenant boundary in cloud identity and its hard-isolation lane in VMs.
This design is the right choice when the boundary has to hold inside a cluster, when the compute is expensive enough that sharing it beats provisioning a VM per job, or when the cluster is not in a cloud at all.

## D.10. Prow, and the Prior Art for Automatic Re-Run

[Prow](https://github.com/kubernetes-sigs/prow) is Kubernetes' own CI system and the most relevant prior art for this design's disruption-recovery behaviour.

**What it does.** Prow owns its own job queue and reconciles `ProwJob` objects into pods.
Verified in source 2026-08-06: when a job pod is evicted, its node becomes unreachable, it is OOM-killed, or it stops unexpectedly, `plank` increments `PodRevivalCount`, deletes the pod, and recreates it on the next sync, up to `Plank.MaxRevivals` (**default 3**).
The per-job opt-*out* is `error_on_eviction: true`.
The field's own documentation is unambiguous: if it is unspecified or false, a new pod replaces the evicted one.

**Where it overlaps.** This is the same outcome this design provides for disrupted jobs, shipped by default, in a system running CI for thousands of repositories.
Any claim that automatic re-run after disruption is *novel* is wrong, and should not be made.

**The differentiator.** Prow has no forge-side claim to protect.
It reads its own queue, so "do not claim it" is not a problem it has, and its recovery is a pod-level restart of work it scheduled itself.
A GitHub Actions runner control plane acquires a job *from GitHub*, which makes both halves harder: the job must be concluded at GitHub before it can be re-run, and the re-run itself is a call to a public REST endpoint outside the runner-scale-set protocol, requiring a credential scope a runner controller does not otherwise need.

The honest scoping is therefore architectural rather than competitive: automatic disruption re-run is rare **among control planes that claim work from an external forge**, not rare in general.
GitLab Runner, which faces exactly that problem, [detects the condition precisely and classifies it terminal](#d12-gitlab-runners-kubernetes-executor).

**Verdict:** Prow is not an alternative for GitHub Actions workloads, and is included because it is the strongest counter-example to an over-broad claim.
The capability is real; the scoping matters.

## D.11. Self-Hosted GitHub Actions Without Kubernetes

A large part of the self-hosted runner market runs on virtual machines rather than Kubernetes.
The three most prominent (measured 2026-08-06):

* **[RunsOn](https://runs-on.com)** installs into the adopter's own AWS account and runs ephemeral EC2 instances.
  Code and secrets stay in the customer's account.
* **[github-aws-runners/terraform-aws-github-runner](https://github.com/github-aws-runners/terraform-aws-github-runner)** (formerly philips-labs) is the established open-source Terraform and Lambda pattern for ephemeral EC2 runners.
* **[Actuated](https://actuated.com)** is a hybrid: a vendor-hosted scheduler drives Firecracker micro-VMs on hardware the customer owns, which is a genuine answer for bare metal but places a vendor in the control path.

**Where they overlap.** All three satisfy "we must self-host", often with less operational surface than a Kubernetes control plane.
For an adopter whose only requirement is that jobs not run on a vendor's shared infrastructure, they are strong and this design is heavier than necessary.

**The differentiator.** A VM per job gives isolation without a namespace argument, but the unit of allocation is an instance rather than a pod, so bin-packing several tenants onto one large machine is not the model.
There is no `ResourceQuota` to arbitrate, no per-tenant capacity floor, and no shared-cluster utilization argument to make.
The two AWS-based options are also cloud-locked, which rules them out for on-premises and reserved-hardware adopters.
Disruption recovery is not the differentiator here: RunsOn retries a job interrupted mid-run once on an on-demand instance by default (its site, read 2026-11-07), a comparable outcome to this design's auto re-run on a pool, so the comparison is about who owns the pool and not about who re-runs the job.

**Verdict:** the right choice when the constraint is "not on a vendor's infrastructure" and the compute is elastic cloud capacity.
This design targets the case where the compute is a fixed, expensive pool that several teams must share.

## D.12. GitLab Runner's Kubernetes Executor

The most mature multi-tenant CI-on-Kubernetes system in wide production use, and the closest architectural sibling to this design outside the GitHub ecosystem.
Included because it faces the *same* structural problem: it claims a job from a hosted forge, then has to place a pod for it.

**Where it overlaps.** A job becomes a pod; `[runners.kubernetes]` configures the pod shape; namespaces separate projects.
Verified 2026-08-06: it ships `pod_disruption_budget` for voluntary drains, `priority_class_name` for scheduling priority, and `retry_limits` for retrying request errors.

**The differentiator.** Two decisions differ, and both are the decisions this design exists to make differently:

* **Capacity is discovered after the claim.** Its intake gates (`concurrent`, `limit`, `request_concurrency`) are counters, not cluster state.
  The documentation's answer to over-subscription is `poll_timeout`, described in GitLab's own reference as being for "queueing more builds than the cluster can handle at a time".
  That is an already-claimed job waiting out a timeout, the failure mode the [pre-acquisition gate](#d8-gating-intake-on-capacity-which-signals-are-safe-to-gate-on) exists to prevent.
* **Disruption is detected precisely, then treated as terminal.** Its pod watcher reads the `DisruptionTarget` condition, which is exactly the signal Kubernetes sets on eviction, preemption, and graceful node shutdown.
  The function consuming it is documented as handling errors the system cannot recover from, and the build fails.
  Recovery is the workflow author's `retry:` keyword, which defaults to none.

`priority_class_name` is also a single class for a runner configuration, not a reserved floor per runner class, so it cannot express "GPU always keeps N slots".

**Verdict:** not an alternative for GitHub Actions workloads, but the most instructive comparison in this appendix.
It demonstrates that detecting a disruption is the easy half, and that the placement of the admission decision relative to the claim is a deliberate architectural choice rather than an oversight.

## D.13. Buildkite Agent Stack for Kubernetes

[agent-stack-k8s](https://github.com/buildkite/agent-stack-k8s) runs Buildkite jobs as Kubernetes pods, with the control plane hosted by Buildkite and the agents self-hosted.

**Where it overlaps.** The split resembles this design's: a long-lived component watching a hosted queue, and one pod per unit of work.
Its scheduler chain reserves work before a max-in-flight limiter and long before a pod exists, so like GitLab Runner it takes the claim before knowing the pod is placeable.
Verified 2026-08-06: it sets `BackoffLimit: 0` and `RestartPolicy: Never`, so a disrupted pod is a failed job, and its limiter *orders* a pending queue by priority without reserving capacity for any class.

**The differentiator.** The control plane is a hosted service, so the compliance-driven adopter this design targets is out of scope by construction, and the comparison is only architectural.
No per-tenant egress identity or `ResourceQuota`-aware intake appears in its configuration surface.

**Verdict:** a different ecosystem, and relevant mainly as evidence that the claim-then-place ordering is the industry norm rather than an ARC peculiarity.

## D.14. Managed Runner Services: An Explicit Non-Competitor

Blacksmith, Namespace, Depot, WarpBuild, Cirrus Runners, Ubicloud and others sell faster GitHub Actions runners on infrastructure they operate.
Two of those read on 2026-11-07 also run the compute in the customer's own cloud account under the vendor's control plane: Depot in AWS, with VPC peering as an option, and WarpBuild in AWS, GCP or Azure, with static IPs on that mode.
Depot lists GPU-enabled runners on its Business plan; none of the five pages read describes running on a Kubernetes cluster the customer operates, or a boundary between a platform team and its tenants.

**Why they are not compared feature-by-feature.** They compete on build speed and price per minute, and this design has measured neither, so a row on that axis would carry a verdict nobody took.
What this design competes on is governance and isolation inside a cluster the adopter operates, and no vendor page read describes it.
A feature this design ships can be moot against a vendor: automatic re-run matters where a job runs on a pool the adopter owns and the cluster can take a worker away, and on a vendor's pool that problem is the vendor's.

**The routing question is where the jobs run, not whose compute it is.** Bring-your-own-cloud modes made *must the compute be yours* stop sorting the field, since a vendor can now answer yes.
What still sorts it: must the jobs run on a Kubernetes cluster you operate, because the hardware is on-premises or air-gapped, the GPUs are already bought, or CI has to sit beside cluster-internal services?
If no, a managed or bring-your-own-cloud service is in the lane, and what this design offers against it is the tenancy model, not the build time.
If yes, every vendor read is unavailable regardless of its merits, and the question becomes how many teams share that cluster and how safely.

**Verdict:** a different axis, overlapping where a vendor runs in the adopter's cloud.
Comparisons that place them on one speed-and-price axis mislead in both directions.

## D.15. Pod-per-Step Container Execution (ARC's `containerMode: kubernetes`)

The one ARC capability with no GAG equivalent, declined on 2026-08-25 under [Q727](../plan/q727-container-steps.md) rather than left as unbuilt work.
This section records why the mechanism cannot be adopted; the costing of the alternatives is in [q727-container-steps.md](../plan/q727-container-steps.md).

**How ARC does it.** The runner pod is issued a Kubernetes service-account token carrying pod `create`, `exec`, and `log` rights in its own namespace, and `ACTIONS_RUNNER_CONTAINER_HOOKS` points the runner at [`runner-container-hooks`](https://github.com/actions/runner-container-hooks).
The runner invokes that hook at job boundaries and at each container step; the hook creates one pod per job container, service container, and container action, all sharing a `ReadWriteMany` volume mounted at the workspace path.
No pod needs `privileged: true`, which is the property that makes it worth wanting.
The capability is measured against ARC `gha-runner-scale-set` 0.14.2 on 2026-08-06 ([arc-parity.md](../plan/arc-parity.md)); the hook's verb set is read from the upstream repository and has not been exercised against a running ARC here.

**The grant is namespace-wide, and Kubernetes offers no narrower one.** RBAC cannot scope `create` below a namespace: `resourceNames` does not restrict it, because the authorizer has no object name to match against at admission time.
So "pod-`create` in the runner's own namespace" necessarily means "pod-`create` over everything in that namespace".
ARC did not choose a loose grant; it is the only shape the primitive offers, which is why this section treats the property as structural rather than as a defect in ARC's implementation.

**The assumption that makes it safe is one ordinary usage breaks.** The grant is defensible while a namespace holds a single trust domain.
In practice a team runs several scale sets in one namespace because different CI checks need differently shaped runners, and GAG's own [migration table](../operations/migration-from-arc.md) records ARC's shape as many scale sets per namespace.
Every runner pod in such a namespace can then create pods alongside the other sets' runner pods and their JIT registration Secrets, so a job on one shape can reach the registration credential of a shape serving a different repository.
No configuration closes that, short of a namespace per runner shape, which removes the reason the sets were grouped.

**ARC's own mitigation does not transfer.** Under `containerMode: kubernetes` the job's steps run in the step pods the hook creates rather than in the runner pod, so the token does not sit beside ordinary `run:` execution.
That is a real property of the mode and worth stating plainly.
GAG has no job-container split to inherit it from: one worker pod per job *is* where the steps run, so a token placed there would sit with the job's own code.
Building the split is the deferred design ([Q998](../queue/Q998.md)), not something adopting the hooks would come with.

**GAG's layout makes the same grant sharper.** GAG stages one Secret per job in the tenant's namespace, holding that job's `jitconfig`, its runner registration credential, and its acquired payload, which carries the job-scoped auth token.
It also institutionalizes many shapes per namespace: one `RunnerTemplate` is referenced by many `RunnerSet`s, all in the tenant namespace.
The condition ARC's model needs is therefore false by construction here, and the blast radius is every concurrent job of the same tenant rather than the tenant boundary namespace separation already defends.

**Which is why the invariant holds.** GAG sets `automountServiceAccountToken: false` on every worker pod and enforces it in two places: `RunnerTemplate` admission rejects `automountServiceAccountToken` and `serviceAccountName` as reserved fields, and the provisioner overwrites both after the tenant `PodTemplate` merges ([`pod.go`](../../cmd/agc/internal/provisioner/pod.go), *Overwrite reserved fields (controller-enforced invariants)*).
Both layers are pinned by tests, and [§5](05-security.md) rates the control Critical: worker pods hold "no API server entry at all".
Adopting ARC's mechanism means reversing that Critical control for every worker pod in the system, not relaxing it for the jobs that ask.

**Translating the job's containers at provisioning time is not a substitute.** GitHub sends `jobContainer` and `jobServiceContainers` as top-level fields of the `AcquireJob` response, visible in the committed capture at [`testdata/job_payload.json`](../../testdata/job_payload.json) and null there only because the probe workflow declares neither, so on the classic acquisition path the AGC does hold the job's container definitions before it builds the pod, and could fold them into the pod spec as a container and sidecars.
That path is not the one this gap is about.
On the scale-set path, which is the ARC-migration path because an ARC scale set becomes a `RunnerSet`, `ProvisionScaleSetWorker` stages only the JIT config: there is no acquired payload, because the runner pulls its own job after the pod is running.
The pod is therefore built before its job is known, and no provisioning-time rewrite can reach it.

**What stays buildable, and what it would cost.** A pod-per-step path that preserves the invariant is possible: ship a GAG implementation of the container-hooks protocol in the worker, and have it request step pods from the AGC instead of from the Kubernetes API, so the AGC remains the only component that creates pods and applies the same invariants to a step pod as to a worker.
That is coherent with the architecture rather than a bolt-on, and it is not cheap.
It needs an authenticated worker-to-AGC endpoint that does not exist today, reachable from untrusted job code through a NetworkPolicy that currently restricts a workload pod's egress to its per-tenant proxy alone, plus step-pod lifecycle, quota accounting, and `fsGroup` propagation to every created pod.
It trades a token in the pod for a control-plane endpoint the job can call, which is a narrower grant than ARC's and not a free one.

**The supported answer is Docker-in-Docker, unprivileged under Kata.** A job that needs `container:`, `services:`, or a container action runs an inner Docker daemon inside a Kata micro-VM, where the capabilities it needs act on a guest kernel rather than the node's ([kata-dind-workloads.md](../operations/kata-dind-workloads.md)).
GAG's own CI runs this way, building a `kind` cluster inside a worker pod with zero `privileged: true`.

**Who that answer does not serve.** Kata boots a KVM guest, so it needs nested virtualization, and that is a hardware prerequisite ARC does not impose.
Measured 2026-08-02 against the GCP API and 2026-08-12 against AWS's guide: GKE Autopilot does not allow nested virtualization at all, GKE Standard excludes E2, C2D, and N2D, and on EC2 every supporting family is Intel, with no AMD, no Graviton, and no P or G GPU family, so those need `.metal` or nothing.
A team on Autopilot, on AMD or Arm nodes, or on most AWS fleets has no Kata available, and for them the answer is a platform-granted privileged `ClusterRunnerTemplate`.
State that as the trade it is rather than as a straight loss.
ARC needs no *pod* privilege for this workload because it holds a namespace-wide API grant instead, and the two fail differently: the grant is standing authorized access that a job simply uses, while a privileged container has to be escaped first.
Severity runs the other way, since an escaped privileged container on plain `runc` reaches the node rather than one namespace, which is why Kata is the recommendation wherever it is available and why the honest summary is lower-probability-higher-severity against no-exploit-required.
That population is the honest cost of this decline and is named on the comparison surfaces rather than left for a reader to discover.

**What would reopen it.** Demand from an adopter who cannot run Kata, recorded against the deferred row that carries the broker-mediated design.
The decline is permanent for ARC's mechanism, which no future release can adopt without reversing the token invariant; it is not permanent for pod-per-step execution as a capability.

---

← [Appendix C](appendix-c-ai-implementation.md) | [Back to index](README.md)
