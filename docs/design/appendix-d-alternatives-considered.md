# Appendix D — Alternatives Considered

← [Appendix C](appendix-c-ai-implementation.md) | [Back to index](README.md) | Next: [Appendix E — Capacity Planning →](appendix-e-capacity-planning.md)

---

This appendix documents the self-hosted runner approaches that were evaluated before settling on the four-tier gateway design. Each alternative is a legitimate solution for some deployment contexts; the goal here is to be explicit about the trade-offs that make them insufficient for the specific requirements of high-scale, multi-tenant, GPU-capable Kubernetes clusters.

The requirements driving this evaluation were: goroutine-level session multiplexing to eliminate idle pod overhead, per-tenant egress IP isolation, zero-idle compute between jobs (scale-to-zero), and self-service tenant onboarding without cluster-admin involvement per team.

---

## D.1. GitHub-Hosted Runners

GitHub provides managed compute for running workflow jobs, with no cluster infrastructure required.

**Advantages**

* Zero operational overhead — no runner infrastructure to deploy, upgrade, or monitor.
* Automatically scaled by GitHub; no capacity planning required.
* Free for public repositories; included minutes for most GitHub plans.
* Broad OS and architecture matrix available (Linux, macOS, Windows; x64, ARM).

**Disadvantages**

* No access to private network resources (internal APIs, private registries, on-premises databases) without additional tunneling infrastructure, which reintroduces operational complexity.
* No GPU support on standard plans. GitHub's larger runners offer some GPU options, but availability is limited, the hardware selection is fixed, and cost per minute is significantly higher than self-managed GPU nodes.
* Cannot use custom base images or pre-warmed dependency caches without workarounds (artifact caching, container layer caching), which add latency and complexity.
* Per-minute billing at scale makes GitHub-hosted runners substantially more expensive than self-managed compute for teams with high job volume or long-running build pipelines.
* No control over egress IPs, making IP-based allowlisting on internal services or GitHub App integrations impractical.

**Verdict:** Appropriate for teams without private network requirements, GPU workloads, or strict egress control needs. Does not satisfy the multi-tenant, GPU-capable cluster requirements driving this design.

---

## D.2. Naive Self-Hosted Runners (No Controller)

The baseline approach: register runner processes directly with GitHub, either as static pods in Kubernetes or on dedicated VMs, with no automation layer managing lifecycle.

**Advantages**

* Minimal setup — the `actions/runner` binary is well-documented and requires no Kubernetes-specific tooling.
* No operator or CRD complexity; the runner process handles its own registration and job polling.
* Straightforward to debug: one process, one log stream, one GitHub registration entry.

**Disadvantages**

* The 1:1 pod-to-connection model is the core problem this design solves. Every runner slot requires a running pod holding memory, a cluster IP, and a long-poll connection — regardless of whether any jobs are queued.
* No lifecycle automation: scaling up or down requires manual intervention or custom scripts. Idle capacity is permanent unless explicitly removed.
* No multi-tenancy. Runners registered at the organization or repository level are shared across all teams, with no resource isolation between tenants.
* No egress IP isolation. All runner traffic exits from shared node IPs.
* GPU nodes must be allocated to runner pods continuously, even between jobs. A team running ten GPU runner slots holds ten GPU allocations idle, paying for capacity that is delivering no value during quiet periods.
* Runner registration tokens expire; re-registration is a manual or scripted process with no automated recovery.

**Verdict:** Viable for small teams with a handful of runners. Fails at scale due to idle resource accumulation, and provides no multi-tenancy or egress isolation primitives.

---

## D.3. Actions Runner Controller (ARC)

[ARC](https://github.com/actions/actions-runner-controller) is the official GitHub-maintained Kubernetes operator for self-hosted runners. It is the most mature and widely-deployed alternative and the most relevant comparison for this design.

**Advantages**

* Official GitHub support: ARC is maintained by GitHub, has a large community, and is well-documented. API compatibility with the GitHub broker protocol is kept current by the maintainers.
* No broker protocol re-implementation required. ARC uses the official `actions/runner` binary and registration flow; this design re-implements a significant portion of the broker API (see [§3.3](03-api-contracts.md#33-re-implemented-broker-api-endpoints)), which carries ongoing maintenance risk.
* `RunnerScaleSet` mode (introduced in ARC v0.5+) supports ephemeral runners that are provisioned on-demand and terminated after each job, eliminating the idle-pod problem for teams that adopt this mode.
* Integrates with Kubernetes-native autoscaling. The `RunnerScaleSet` controller publishes a custom metric that KEDA or the built-in autoscaler can act on.
* Broad adoption means community-tested Helm charts, pre-built container images, and an established set of known operational issues.

**Disadvantages**

* **Session multiplexing.** In `RunnerScaleSet` mode, ARC uses one `Runner.Listener` process per scale set (not one per slot), so its steady-state long-poll connection count is similar to this design's adaptive listener model. However, each `Runner.Listener` process carries a ~256 MiB resident memory footprint (the full .NET runner runtime), versus ~60 KiB per goroutine in the Actions Gateway Controller (AGC) — roughly a 4,000× difference per active session. At a tenant operating 10 RunnerScaleSets, that is ~2.5 GiB for ARC's listeners versus ~600 KiB for the AGC at rest.
* **Multi-tenant isolation.** ARC does not provide a self-service multi-tenancy model. Each team typically requires a separate `RunnerDeployment` or `RunnerScaleSet` with its own RBAC, and cluster-admin involvement is required to set up network policies and resource quotas per tenant. There is no equivalent of the `ActionsGateway` CR that lets a team provision an isolated gateway instance within their existing namespace without cluster-admin.
* **Egress IP isolation.** ARC provides no per-tenant egress IP control. All runner traffic exits from shared node IPs unless the operator independently layers a proxy or NAT gateway, which is not part of ARC's feature set. This design's per-tenant `EgressProxyPool` (Tier 3) provides this natively.
* **GPU idle cost.** ARC's `RunnerScaleSet` can scale to zero runners between bursts, which eliminates idle GPU pod allocations during quiet periods. However, the scale-down latency is governed by the autoscaler's reaction time (typically 30–60 seconds after queue depth drops), whereas this design's ephemeral worker pods release their compute immediately on job completion. For GPU workloads where node hours are expensive, the difference in idle time per job cycle accumulates.
* **AGC node placement.** ARC's controller runs on whatever nodes are available and does not distinguish between CPU-only and GPU node pools. The AGC in this design is explicitly designed to run on CPU-only nodes, keeping GPU capacity entirely free for worker pods. This distinction requires intentional `nodeSelector` or `taints/tolerations` configuration in ARC but is enforced structurally in this design.
* **No shared quota across runner sets, and no minimum guarantees within a shared pool.** ARC's `maxRunners` is a per-`RunnerScaleSet` property with no mechanism to express a shared budget across multiple sets. A team with three RunnerScaleSets each capped at 50 can theoretically schedule 150 concurrent runners — exceeding the namespace's actual resource capacity — and there is no native way to say "all sets combined may use at most 100 concurrent jobs" without external tooling or manual coordination of per-set caps. Conversely, lowering per-set caps to constrain the aggregate introduces the opposite problem: a large CPU runner set can be capped low enough to protect namespace headroom, but there is no mechanism to guarantee that GPU runners (which need that headroom) can actually claim it when they need it. This design bounds all worker pods from all `RunnerGroup`s against the same Kubernetes `ResourceQuota` for the shared ceiling, and adds a `priorityTiers` field on each `RunnerGroup` to express minimum guarantees within that pool: the first N pods of a high-priority group are assigned a preempting `PriorityClass` that displaces lower-priority pods when the namespace is contended, while additional pods above the preemption threshold schedule opportunistically. Both the shared ceiling and the per-group priority floors are expressed declaratively in the `ActionsGateway` CR and enforced by the Kubernetes scheduler — no external tooling or manual cap coordination required.
* **Broker protocol opacity.** Because ARC wraps the official runner binary, it inherits any breaking changes GitHub makes to the broker protocol without exposing them as first-class API contracts. This design's explicit broker API documentation ([§3.3](03-api-contracts.md#33-re-implemented-broker-api-endpoints)) makes compatibility requirements visible and testable.

**Verdict:** ARC is the right choice for most teams that need Kubernetes-native self-hosted runners. It is mature, officially supported, and avoids the maintenance burden of re-implementing the broker protocol. This design is the right choice when the requirements include goroutine-level memory efficiency at scale (4,000× less resident memory per listener than ARC's .NET runner process), per-tenant egress IP isolation without additional infrastructure, or self-service multi-tenant onboarding with per-tenant namespace isolation, `priorityTiers` preemption control, and declarative `ActionsGateway` provisioning — none of which ARC provides natively.

---

## D.4. ARC with KEDA Autoscaling

A common production pattern layers [KEDA](https://keda.sh/) on top of ARC, using a `ScaledObject` targeting ARC's queue-depth metric to drive runner replica count. This addresses ARC's baseline idle-runner problem more aggressively than ARC's built-in autoscaler alone.

**Advantages**

* Eliminates idle runners during sustained quiet periods: KEDA can scale the `RunnerScaleSet` to zero replicas when the queue is empty and scale up in response to queued jobs.
* Uses standard, widely-adopted tooling. KEDA is a CNCF project with broad ecosystem support.
* Requires no changes to ARC's runner binary or broker integration.

**Disadvantages**

* **Scale-up latency.** KEDA reacts to metric changes on a configurable polling interval (default 30 seconds). During a burst, new runner pods must be scheduled, image-pulled, and registered with GitHub before they can accept work. This design's goroutine model maintains a standing pool of pre-registered virtual sessions at negligible cost, so job acquisition latency is bounded by pod scheduling and image pull time rather than runner registration time.
* **Adds operational dependency.** KEDA introduces another component to install, upgrade, and monitor. Failure modes compound: a KEDA controller outage or metric source failure stalls autoscaling.
* **Does not solve multiplexing or egress isolation.** KEDA addresses scale-to-zero but leaves the per-pod session overhead and shared-egress-IP problems untouched.
* **GPU idle gap.** Even with KEDA scaling ARC to zero, the scale-down reaction time means GPU allocations are held for up to a full KEDA polling interval after the last job completes. This design's immediate compute release on pod completion eliminates that gap.

**Verdict:** A meaningful improvement over plain ARC for teams where idle runner cost is the primary concern. Does not close the gap on session multiplexing, egress isolation, or multi-tenant self-service provisioning.

---

Sections D.1–D.4 cover ways of running the runners themselves. The two sections below cover *adjacent* Kubernetes tooling that is frequently raised alongside this design — a job-queue / quota manager and an infrastructure cost optimizer. Neither is a self-hosted runner controller, so neither is a drop-in substitute; both are included because each overlaps part of the problem space (priority/quota arbitration; GPU/compute cost) and the boundary between "what this design does" and "what these tools do" is a common point of confusion.

## D.5. Kueue and Kubernetes Job-Queue / Quota Managers

[Kueue](https://kueue.sigs.k8s.io/) is the Kubernetes-native job queueing and quota manager maintained under the Kubernetes Special Interest Group (SIG) for scheduling. It is the natural off-the-shelf tool to reach for when someone asks "why not just put a priority queue in front of the runners?", so the boundary between it and this design is worth stating explicitly.

**What it does.** Kueue arbitrates workloads against declarative quota. Its core objects are `ClusterQueue` and `LocalQueue` (the quota and submission surfaces), `ResourceFlavor` (heterogeneous resource pools, e.g. GPU vs CPU), `Cohort` (quota borrowing between queues), and `WorkloadPriorityClass` (priority-ordered preemption). Per its own documentation, Kueue "decides when a job should wait, when a job should be admitted to start (as in pods can be created), and when a job should be preempted." It installs as Custom Resource Definitions (CRDs), a cluster-wide controller, and admission webhooks, and therefore requires cluster-admin to deploy.

**Where it overlaps.** Kueue's quota-and-priority model overlaps the same need this design addresses with a shared `ResourceQuota` ceiling plus per-`RunnerGroup` `priorityTiers`: keeping a high-priority runner type from being starved by a flood of lower-priority work, and expressing a shared budget across heterogeneous pools. A cluster that already runs Kueue has a credible answer to the priority/quota half of the problem at the pod layer.

**The differentiator.** Kueue gates the *pod* layer; this design's admission decision has to happen one layer above it, at the GitHub broker. A worker pod only exists *after* the Actions Gateway Controller (AGC) has already claimed the job from GitHub (`acquirejob`), at which point GitHub considers the job owned by that session and the job lock is ticking. Kueue has no visibility into the broker and cannot defer a job that is not yet a Kubernetes workload; if it defers the *pod* after the claim, the work is queued while the lock the design must renew counts down — the exact failure the broker-layer admission gate exists to prevent. So Kueue **augments** rather than **replaces** the design's gate: in a cluster that already runs Kueue, this design's worker pods can still participate in a `ClusterQueue` for cluster-wide quota and preemption at the pod layer, while the broker-layer decision of *whether to claim the job at all* stays upstream of anything Kueue can act on. Kueue also requires cluster-admin to install, which is in tension with this design's self-service-without-cluster-admin requirement, so making it a hard dependency would regress that goal.

The full argument — why admission is gated before `acquirejob` rather than delegated to an in-cluster queue, and why a durable internal queue was also rejected — is developed in the pre-acquisition admission-control plan ([Q59](../STATUS.md#Q59); see [Relationship to Kueue](../plan/acquire-admission-control.md#relationship-to-kueue-why-an-off-the-shelf-k8s-queue-isnt-the-admission-layer)) and is not duplicated here.

**Verdict:** Kueue is a strong fit for cluster-wide batch quota and priority arbitration at the pod layer, and composes with this design rather than competing with it. It is not a substitute for runner-control-plane admission, because the decision that matters for GitHub Actions jobs — whether to claim a job from the broker — happens before any Kubernetes workload exists for Kueue to manage.

---

## D.6. Exostellar and Infrastructure / GPU Cost Optimizers

[Exostellar](https://exostellar.io/) is representative of a class of infrastructure cost-optimization tooling that is sometimes mentioned in the same breath as runner autoscaling because it targets the cost of expensive (especially GPU) compute. It is included here to draw the layer boundary, not because it manages runners.

**What it does.** Per Exostellar's public materials, it offers two main capabilities. The Exostellar Infrastructure Optimizer runs workloads inside virtual machines (VMs) on cloud instances and predicts spot-instance reclamation, live-migrating a VM to another spot or on-demand instance to keep the workload alive while capturing spot pricing. Its Software Defined GPU offering provides vendor-agnostic, fractional GPU slicing through Kubernetes Dynamic Resource Allocation (DRA), partitioning GPUs beyond fixed Multi-Instance GPU (MIG) boundaries to raise utilization. Both are aimed at lowering the unit cost of the underlying compute.

**Where it overlaps.** Only at the framing level of "make expensive GPU compute cheaper." This design reduces GPU cost by holding *zero* idle GPU allocation between jobs — worker pods are provisioned when a job is acquired and release their compute on completion — so the comparison is real for anyone evaluating "how do I stop paying for idle GPUs."

**The differentiator.** Exostellar operates at the node / VM / GPU *infrastructure* layer: it optimizes the cost and packing of compute that has already been requested. This design operates at the runner *control-plane* layer: it decides whether a worker pod needs to exist at all (goroutine-multiplexed virtual sessions with no per-runner pod at rest), provides per-tenant egress IP isolation, and offers multi-tenant self-service provisioning — none of which an infrastructure optimizer addresses. The two are orthogonal and could compose: an infrastructure optimizer could pack the nodes that this design's ephemeral worker pods land on.

> **Unverified — treat as a hypothesis, not a claim.** Working notes for this analysis speculated that vendors such as Exostellar layer a queue manager (e.g. Kueue) beneath ARC for GPU/quota management. Public materials reviewed for this appendix describe Exostellar as an infrastructure / GPU optimizer (spot-VM migration and GPU slicing) and do **not** describe a GitHub Actions runner, ARC integration, or a runner-queue product. The "layered under ARC" pattern is therefore not asserted here. What can be stated with confidence is the layer distinction above: infrastructure optimizers and this design address different layers and are not substitutes.

**Verdict:** Infrastructure and GPU cost optimizers are complementary to this design, not alternatives. They lower the cost of compute that is running; this design lowers cost primarily by ensuring compute is not running when no job needs it, and adds the runner-control-plane properties (multiplexing, egress isolation, multi-tenant self-service) that sit entirely outside an infrastructure optimizer's scope.

---

← [Appendix C](appendix-c-ai-implementation.md) | [Back to index](README.md)
