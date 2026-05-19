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

* **Session multiplexing.** Even in `RunnerScaleSet` mode, each virtual runner slot corresponds to one registered runner process during the polling phase. Scaling to 1,000 concurrent slots means 1,000 runner processes and 1,000 long-poll connections. The goroutine-based AGC replaces this with ~60 KiB goroutines, a reduction of over 4,000× per session in resident memory.
* **Multi-tenant isolation.** ARC does not provide a self-service multi-tenancy model. Each team typically requires a separate `RunnerDeployment` or `RunnerScaleSet` with its own RBAC, and cluster-admin involvement is required to set up network policies and resource quotas per tenant. There is no equivalent of the `ActionsGateway` CR that lets a team provision an isolated gateway instance within their existing namespace without cluster-admin.
* **Egress IP isolation.** ARC provides no per-tenant egress IP control. All runner traffic exits from shared node IPs unless the operator independently layers a proxy or NAT gateway, which is not part of ARC's feature set. This design's per-tenant `EgressProxyPool` (Tier 3) provides this natively.
* **GPU idle cost.** ARC's `RunnerScaleSet` can scale to zero runners between bursts, which eliminates idle GPU pod allocations during quiet periods. However, the scale-down latency is governed by the autoscaler's reaction time (typically 30–60 seconds after queue depth drops), whereas this design's ephemeral worker pods are garbage-collected immediately on job completion. For GPU workloads where node hours are expensive, the difference in idle time per job cycle accumulates.
* **AGC node placement.** ARC's controller runs on whatever nodes are available and does not distinguish between CPU-only and GPU node pools. The AGC in this design is explicitly designed to run on CPU-only nodes, keeping GPU capacity entirely free for worker pods. This distinction requires intentional `nodeSelector` or `taints/tolerations` configuration in ARC but is enforced structurally in this design.
* **No shared quota across runner sets, and no minimum guarantees within a shared pool.** ARC's `maxRunners` is a per-`RunnerScaleSet` property with no mechanism to express a shared budget across multiple sets. A team with three RunnerScaleSets each capped at 50 can theoretically schedule 150 concurrent runners — exceeding the namespace's actual resource capacity — and there is no native way to say "all sets combined may use at most 100 concurrent jobs" without external tooling or manual coordination of per-set caps. Conversely, lowering per-set caps to constrain the aggregate introduces the opposite problem: a large CPU runner set can be capped low enough to protect namespace headroom, but there is no mechanism to guarantee that GPU runners (which need that headroom) can actually claim it when they need it. This design bounds all worker pods from all `RunnerGroup`s against the same Kubernetes `ResourceQuota` for the shared ceiling, and adds a `priorityTiers` field on each `RunnerGroup` to express minimum guarantees within that pool: the first N pods of a high-priority group are assigned a preempting `PriorityClass` that displaces lower-priority pods when the namespace is contended, while additional pods above the preemption threshold schedule opportunistically. Both the shared ceiling and the per-group priority floors are expressed declaratively in the `ActionsGateway` CR and enforced by the Kubernetes scheduler — no external tooling or manual cap coordination required.
* **Broker protocol opacity.** Because ARC wraps the official runner binary, it inherits any breaking changes GitHub makes to the broker protocol without exposing them as first-class API contracts. This design's explicit broker API documentation ([§3.3](03-api-contracts.md#33-re-implemented-broker-api-endpoints)) makes compatibility requirements visible and testable.

**Verdict:** ARC is the right choice for most teams that need Kubernetes-native self-hosted runners. It is mature, officially supported, and avoids the maintenance burden of re-implementing the broker protocol. This design is the right choice when the requirements include goroutine-level session multiplexing at >250 concurrent sessions per tenant, per-tenant egress IP isolation without additional infrastructure, or self-service multi-tenant onboarding — none of which ARC provides natively.

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
* **GPU idle gap.** Even with KEDA scaling ARC to zero, the scale-down reaction time means GPU allocations are held for up to a full KEDA polling interval after the last job completes. This design's immediate pod garbage-collection eliminates that gap.

**Verdict:** A meaningful improvement over plain ARC for teams where idle runner cost is the primary concern. Does not close the gap on session multiplexing, egress isolation, or multi-tenant self-service provisioning.

---

← [Appendix C](appendix-c-ai-implementation.md) | [Back to index](README.md)
