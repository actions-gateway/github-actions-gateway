# Secure, Isolated, Multi-Tenant OSS CI

> **Status: goal stated 2026-08-06, deliverables partly shipped.** This is a map and a definition of done, not a design.
> The design for the network half is [q408-untrusted-pr-egress.md](q408-untrusted-pr-egress.md) and is not restated here.
> Kernel isolation is shipped and validated.
> The rest is open.

## Why this is the goal

Multi-tenant CI on shared, expensive nodes is the efficient answer, and almost nobody runs it.
Where managed Kubernetes is cheap and CI fits inside one cloud, a cluster per tenant in its own network and project is a perfectly good answer, and a stronger isolation boundary than a shared cluster can offer.
GAG exists for the case where that stops being cheap: cluster overhead is real, the nodes are big and expensive (GPU, large-memory, on-premises, or reserved capacity already paid for), and sharing them is the only way the economics work.

Account-per-tenant and cluster-per-tenant have **no cheap on-premises analogue**.
In a cloud an account or project is a free API call that inherits a pre-built control plane.
On bare metal the equivalent boundary is a hardware purchase, or building the hypervisor, software-defined-networking, and self-service storage layer that would make accounts possible.
Worse, partitioned capacity cannot be shifted between tenants, so every tenant is provisioned for its own peak and every trough is stranded.
You cannot hand the hardware back at month end.

## What makes it unsolved

The reason teams retreat to a cluster per tenant is not cost.
It is that **CI multi-tenancy has never been secured well enough**, and CI is the hard case on two axes at once:

- **CI needs root.** Image builds, Docker-in-Docker, `services:` containers, package installs.
  Privileged Docker-in-Docker on a shared node defeats Pod Security Admission `restricted` outright.
- **CI needs network isolation, and that is hard on Kubernetes.** NetworkPolicy needs a policy-enforcing CNI to mean anything; egress by hostname is CNI-specific; and CDN-fronted registries cannot be pinned by CIDR at all.

So the honest framing for this milestone is not "competitor X lacks feature Y".
It is: **nobody has solved this, here is how far we have got, and here is the boundary of what we currently claim.**

## Threat model: the untrusted contributor

`docs/design/05-security.md` models tenant against tenant, where every tenant is a team inside the operator's organization.
This milestone adds an adversary that model does not carry: **a contributor who can cause code to run but is not a user of the platform.** A fork pull request is an arbitrary-code-execution request that the CI system is designed to honour.

What changes:

- The adversary chooses the workload, so "misconfigured tenant" becomes "deliberately hostile job".
- The adversary is anonymous and cheap to become.
  Rate limiting and abuse response matter more than attribution.
- The blast radius that matters is not the tenant's own namespace.
  It is every other tenant on the node, the node's own credentials, and the mirror or cache the platform provides.
- GitHub itself warns that self-hosted runners should not be used with public repositories, because untrusted workflow code can persist on the runner.
  GAG's answer has to be that nothing persists and nothing reachable is worth having.

## The layer map, with current state

| Layer | Mechanism | State |
|---|---|---|
| Kernel | Kata micro-VM workers, no `privileged: true` | **Shipped and validated.** Default for GAG's own end-to-end CI, a kind cluster built inside an unprivileged worker pod, on nested-virtualization GKE. See [kata-dind-workloads.md](../operations/kata-dind-workloads.md) |
| Pod identity floor | `hostPID`/`hostNetwork`/`hostIPC`/`automountServiceAccountToken` all false, controller-managed ServiceAccount, stamped over any tenant `PodTemplateSpec` | **Shipped** |
| Network egress | Default-deny NetworkPolicy, per-tenant proxy pool, in-cluster pull-through mirror, egress scoped to mirror plus GitHub plus DNS | **Shipped and validated (Q408, 2026-08-28).** A Kata worker reaches cluster DNS on 53, GitHub on 443 and the mirrors on 5000 and nothing else, measured on the dogfood cluster by in-job negatives against controls that answered, on every run of the lane. Recipe: [kata-dind-workloads.md § Untrusted pull requests](../operations/kata-dind-workloads.md#untrusted-pull-requests--the-tight-egress-posture); design in [q408-untrusted-pr-egress.md](q408-untrusted-pr-egress.md). [Q539](../queue/Q539.md) and [Q540](../queue/Q540.md) grade two variants against the contract this validated, and are follow-ons rather than gaps |
| Node metadata | The link-local DNS allowance is scoped to port 53, so no rule admits the metadata address on any port it serves | **Shipped and asserted (Q716).** Q226 measured HTTP 200 from inside a Kata guest, because Kata bounds the guest kernel and not the pod's network identity. `TestBuildNetworkPolicy_DeniesCloudMetadataServer` pins the authored policy on every PR; `E2E_V2_DirectEgress_MetadataServerBlocked` proves a real CNI enforces it on the Calico lane. Needs a policy-enforcing CNI: without one, Workload Identity is the only control |
| Cross-job cache | none in-cluster | **This posture removes the cache that works.** `actions/cache` reaches its Azure-blob store through the default egress allowlist today; closing egress to GitHub plus mirror plus DNS takes it away, so an in-cluster cache stops being an optimisation and becomes the only cache an untrusted job can have. Q215 is blocked on that review, which [caching-and-worker-storage.md](caching-and-worker-storage.md) reframes as the design |
| Evidence | Per-tenant egress audit records | **Shipped, opt-in (Q564, Q986).** `EgressProxy.spec.auditLogging: Connections` writes one structured line per accepted CONNECT: namespace, destination host and port, bytes each way, duration. Off by default: the record is data about a tenant's egress, so retaining it is the platform's decision. Definition of Done #5 takes a further opt-in on both sides — `ConnectionsWithSource` adds the client address to that record, and `ActionsGateway.spec.auditLogging: WorkerAddresses` records which job holds each address while its pod lives — because attribution is what turns an egress record into a per-worker movement log, and because a worker pod is deleted with its job so the binding cannot be looked up afterwards. See [observability-logging.md](../operations/observability-logging.md#attributing-a-record-to-a-tenant-and-a-job) and [q986-egress-attribution.md](q986-egress-attribution.md) |
| Transport to the proxy | TLS on the AGC-to-proxy and worker-to-proxy hop | **Open.** The CONNECT target is cleartext on that hop and readable by an eBPF tap, though the tunnelled payload stays TLS to GitHub. Row: Q566 |
| Noisy-neighbour containment | Per-runner-group proxy pool | **Open.** One pool per gateway today, so a bandwidth-heavy group can saturate a co-tenant's. Row: Q567 |

## Definition of done

Broader than Q408's success criteria, which bound the mirror work only and are met.
This milestone is done when all of the following hold, each demonstrated rather than argued:

1. A job from an untrusted fork pull request runs on a shared cluster with a kernel it cannot escape into anything useful, and reaching that state costs one documented step rather than a design of the adopter's own.
   **The default-versus-opt-in question splits, and Q408's Phase 4 is what split it.** The *network* posture is the default and always was: a tenant's egress is default-deny plus GitHub, and what Phase 4 shipped was the **deletion** of an additive allow-all rather than a new object, so weakening the posture is the deliberate act and holding it is the resting state.
   The *pod shape* is not the default and must not become one: a `ClusterRunnerTemplate` marked default applies to every `RunnerSet` naming no template, so shipping the Kata shape as a cluster default would hand a privileged capability set to sets that never asked for one ([runner-template-library.md § Nothing ships as a cluster default](../operations/runner-template-library.md#nothing-ships-as-a-cluster-default)).
   This criterion therefore asks that the secure state be what an operator gets by not acting, and that adopting the kernel boundary on top be one `kubectl apply -k` against a shipped template.
   It does not ask for a default template, and a future reading that revives that demand is reading a criterion this project already declined on a security argument.
2. Its egress reaches the mirror, GitHub, and DNS, and an end-to-end run's logs name no other host.
3. It cannot reach the node's metadata service, and a test asserts this on a policy-enforcing CNI rather than a doc recommending it.
4. It cannot read another tenant's cache, secrets, or job payloads, including through anything the platform provides for its convenience.
5. An operator can produce, unprompted, a per-tenant record of which host each job reached.
   Controls without evidence do not satisfy an auditor.
   **Met as of Q986**, on the two opt-ins above: the join is the proxy record's source address against the AGC's worker-address bindings, and it names the consuming tenant on a shared pool as well as an unshared one.
6. The residual channels are enumerated in this document and each one is either closed or explicitly accepted with a reason.

## Explicitly out of scope, and residual risk accepted

The most important section.
"We run untrusted OSS CI" is indefensible without a boundary.

- **Compute abuse is contained, not prevented.** A hostile job can consume the capacity its tenant's quota allows.
  The quota, the priority tiers, and `maxWorkerLifetime` bound it; nothing detects intent in advance.
- **Secrets a maintainer deliberately grants a fork are out of reach.** If a workflow is configured to expose a secret to `pull_request_target`, that is a repository configuration decision GAG cannot override and should not pretend to.
- **Side channels between co-tenant micro-VMs are not addressed.** Kata bounds the kernel.
  Speculative-execution and shared-cache side channels between guests on one node are a hardware and hypervisor concern, not one this milestone closes.
- **The mirror is a trusted component.** A compromised pull-through mirror reaches every job that pulls through it.
  The mirror contract in [q408 §3.5](q408-untrusted-pr-egress.md#35-the-mirror-role-is-a-contract) is what bounds this, and it is a contract rather than an enforcement.
- **A shared mirror set exposes cache timing to every tenant that can reach it.** Whether a repository is already warm says that some other tenant pulled it.
  The repository *list* is no longer part of this: `GET /v2/_catalog` answered 200 with every cached repository named, `catalog.maxentries=0` did not close it, and every instance is now fronted by a proxy that refuses the path under both topologies ([how, and what it was measured against](../../deploy/registry-mirror/README.md#closing-the-repository-catalog)).
  What is left is timing, measured on a laptop rather than from inside a Kata guest, so the channel is bounded rather than measured where an attacker sits ([Q1020](../queue/Q1020.md)).
  It is an accepted risk only for an adopter who chose the shared topology knowing it, which is what [Choosing a mirror topology](../operations/kata-dind-workloads.md#choosing-a-mirror-topology) exists to make explicit; the isolated topology removes it and costs a mirror set per tenant.
- **This is not a claim about arbitrary hostile input at scale.** The goal is that a fork pull request on an adopter's own repositories is safe to run.
  A public, unauthenticated build service is a different product with different economics.

## What the site may claim today

Down to earth, current state only:

> Kata micro-VM workers are validated end to end and are the default for GAG's own CI, which builds a kind cluster inside an unprivileged worker pod.
> Their egress is now closed to match: an in-cluster registry mirror carries every image pull, the tenant carries no allow-all rule, and a run's own probes confirm that a worker reaches cluster DNS, GitHub and the mirror and nothing else.
> What is not yet demonstrated is the cross-tenant cache boundary and the per-job egress record, so we describe the posture and its measurements rather than declaring untrusted-PR readiness.

Q408 closing moves items 1, 2 and 3 of the definition of done to demonstrated, and item 6 holds as long as the section below stays current, which a layer change is the moment to re-check.
Item 5 closed with Q986.
Item 4 is open on [Q215](../queue/Q215.md), and the part of it the mirror itself introduced now has its basis written down: a shared mirror and a mirror per tenant are both defensible, so the platform admin chooses, and [Choosing a mirror topology](../operations/kata-dind-workloads.md#choosing-a-mirror-topology) is what they choose on.
[Q1020](../queue/Q1020.md) holds only the guest timing measurement that guidance does not rest on.
Do not claim untrusted-PR readiness until all six hold.
The claim is checkable and a wrong one costs more than the feature is worth.

## Deliverables

Shipped: Kata validation, the pod isolation floor, default-deny NetworkPolicy, per-tenant proxy pools, the opt-in per-pool egress audit record (Q564) and the two further opt-ins that make it attributable to a tenant and a job (Q986, which is what satisfies Definition of Done #5: see the Evidence row), and the untrusted-PR egress posture itself (Q408: the in-cluster registry mirror plus the deletion of the tenant's allow-all rule, validated on the dogfood cluster 2026-08-28).

Open, with rows: Q539 (Kata plus Dragonfly as the mirror backend), Q540 (composed node-layer plus guest-layer stack), Q566 (TLS on the proxy hop), Q567 (per-group proxy pool), Q215 (cache backend, blocked on the cross-tenant isolation review this document scopes).

## Gaps with no row yet

Surfaced by drawing the whole picture, and each needs a decision before it needs a row:

1. **Secret exposure to fork pull requests.** What GAG can and cannot influence, and what it should refuse to run.
   Interacts with `pull_request_target` semantics that belong to the repository, not the platform.
2. **Runner-token scope for an untrusted job.** The job's own credentials are a reachable asset.
   Whether an untrusted class should receive a narrower token than a trusted one is unexamined.
3. **What "untrusted" is, as a declared thing.** Today it is a posture an operator assembles.
   A declared class would let the platform apply the whole set at once, but the moat review rejected building that surface while five of its dependencies are open or blocked, which is the correct order.
   The decision this document owes is what the class would mean, not yet how to express it.
