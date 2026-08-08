# Secure, Isolated, Multi-Tenant OSS CI

> **Status: goal stated 2026-08-06, deliverables partly shipped.** This is a map
> and a definition of done, not a design. The design for the network half is
> [q408-untrusted-pr-egress.md](q408-untrusted-pr-egress.md) and is not restated
> here. Kernel isolation is shipped and validated. The rest is open.

## Why this is the goal

Multi-tenant CI on shared, expensive nodes is the efficient answer, and almost
nobody runs it. Where managed Kubernetes is cheap and CI fits inside one cloud,
a cluster per tenant in its own network and project is a perfectly good answer,
and a stronger isolation boundary than a shared cluster can offer. GAG exists for
the case where that stops being cheap: cluster overhead is real, the nodes are
big and expensive (GPU, large-memory, on-premises, or reserved capacity already
paid for), and sharing them is the only way the economics work.

Account-per-tenant and cluster-per-tenant have **no cheap on-premises analogue**.
In a cloud an account or project is a free API call that inherits a pre-built
control plane. On bare metal the equivalent boundary is a hardware purchase, or
building the hypervisor, software-defined-networking, and self-service storage
layer that would make accounts possible. Worse, partitioned capacity cannot be
shifted between tenants, so every tenant is provisioned for its own peak and
every trough is stranded. You cannot hand the hardware back at month end.

## What makes it unsolved

The reason teams retreat to a cluster per tenant is not cost. It is that **CI
multi-tenancy has never been secured well enough**, and CI is the hard case on
two axes at once:

- **CI needs root.** Image builds, Docker-in-Docker, `services:` containers,
  package installs. Privileged Docker-in-Docker on a shared node defeats Pod
  Security Admission `restricted` outright.
- **CI needs network isolation, and that is hard on Kubernetes.** NetworkPolicy
  needs a policy-enforcing CNI to mean anything; egress by hostname is
  CNI-specific; and CDN-fronted registries cannot be pinned by CIDR at all.

So the honest framing for this milestone is not "competitor X lacks feature Y".
It is: **nobody has solved this, here is how far we have got, and here is the
boundary of what we currently claim.**

## Threat model: the untrusted contributor

`docs/design/05-security.md` models tenant against tenant, where every tenant is
a team inside the operator's organization. This milestone adds an adversary that
model does not carry: **a contributor who can cause code to run but is not a
user of the platform.** A fork pull request is an arbitrary-code-execution
request that the CI system is designed to honour.

What changes:

- The adversary chooses the workload, so "misconfigured tenant" becomes
  "deliberately hostile job".
- The adversary is anonymous and cheap to become. Rate limiting and abuse
  response matter more than attribution.
- The blast radius that matters is not the tenant's own namespace. It is every
  other tenant on the node, the node's own credentials, and the mirror or cache
  the platform provides.
- GitHub itself warns that self-hosted runners should not be used with public
  repositories, because untrusted workflow code can persist on the runner. GAG's
  answer has to be that nothing persists and nothing reachable is worth having.

## The layer map, with current state

| Layer | Mechanism | State |
|---|---|---|
| Kernel | Kata micro-VM workers, no `privileged: true` | **Shipped and validated.** Default for GAG's own end-to-end CI, a kind cluster built inside an unprivileged worker pod, on nested-virtualization GKE. See [kata-dind-workloads.md](../operations/kata-dind-workloads.md) |
| Pod identity floor | `hostPID`/`hostNetwork`/`hostIPC`/`automountServiceAccountToken` all false, controller-managed ServiceAccount, stamped over any tenant `PodTemplateSpec` | **Shipped** |
| Network egress | Default-deny NetworkPolicy, per-tenant proxy pool, in-cluster pull-through mirror, egress scoped to mirror plus GitHub plus DNS | **Open.** Design and phases in [q408-untrusted-pr-egress.md](q408-untrusted-pr-egress.md); validations are Q539 and Q540 |
| Node metadata | NetworkPolicy denying the link-local metadata address | **Documented, unasserted.** Q226 measured HTTP 200 from inside a Kata guest. Kata bounds the guest kernel, not the pod's network identity. Row: Q716 |
| Cross-job cache | none in-cluster | **This posture removes the cache that works.** `actions/cache` reaches its Azure-blob store through the default egress allowlist today; closing egress to GitHub plus mirror plus DNS takes it away, so an in-cluster cache stops being an optimisation and becomes the only cache an untrusted job can have. Q215 is blocked on that review, which [caching-and-worker-storage.md](caching-and-worker-storage.md) reframes as the design |
| Evidence | Per-tenant egress audit records | **Open.** The proxy emits counters only, so per-tenant egress is reconstructable today only from cluster flow logs. Row: Q564 |
| Transport to the proxy | TLS on the AGC-to-proxy and worker-to-proxy hop | **Open.** The CONNECT target is cleartext on that hop and readable by an eBPF tap, though the tunnelled payload stays TLS to GitHub. Row: Q566 |
| Noisy-neighbour containment | Per-runner-group proxy pool | **Open.** One pool per gateway today, so a bandwidth-heavy group can saturate a co-tenant's. Row: Q567 |

## Definition of done

Broader than Q408's success criteria, which bound the mirror work only. This
milestone is done when all of the following hold, each demonstrated rather than
argued:

1. A job from an untrusted fork pull request runs on a shared cluster with a
   kernel it cannot escape into anything useful, and that is the default posture
   rather than an opt-in.
2. Its egress reaches the mirror, GitHub, and DNS, and an end-to-end run's logs
   name no other host.
3. It cannot reach the node's metadata service, and a test asserts this on a
   policy-enforcing CNI rather than a doc recommending it.
4. It cannot read another tenant's cache, secrets, or job payloads, including
   through anything the platform provides for its convenience.
5. An operator can produce, unprompted, a per-tenant record of which host each
   job reached. Controls without evidence do not satisfy an auditor.
6. The residual channels are enumerated in this document and each one is either
   closed or explicitly accepted with a reason.

## Explicitly out of scope, and residual risk accepted

The most important section. "We run untrusted OSS CI" is indefensible without a
boundary.

- **Compute abuse is contained, not prevented.** A hostile job can consume the
  capacity its tenant's quota allows. The quota, the priority tiers, and
  `maxWorkerLifetime` bound it; nothing detects intent in advance.
- **Secrets a maintainer deliberately grants a fork are out of reach.** If a
  workflow is configured to expose a secret to `pull_request_target`, that is a
  repository configuration decision GAG cannot override and should not pretend
  to.
- **Side channels between co-tenant micro-VMs are not addressed.** Kata bounds
  the kernel. Speculative-execution and shared-cache side channels between guests
  on one node are a hardware and hypervisor concern, not one this milestone
  closes.
- **The mirror is a trusted component.** A compromised pull-through mirror
  reaches every job that pulls through it. The mirror contract in
  [q408 §3.5](q408-untrusted-pr-egress.md#35-the-mirror-role-is-a-contract) is
  what bounds this, and it is a contract rather than an enforcement.
- **This is not a claim about arbitrary hostile input at scale.** The goal is
  that a fork pull request on an adopter's own repositories is safe to run. A
  public, unauthenticated build service is a different product with different
  economics.

## What the site may claim today

Down to earth, current state only:

> Kata micro-VM workers are validated end to end and are the default for GAG's
> own CI, which builds a kind cluster inside an unprivileged worker pod. That
> makes them suitable for **trusted** CI today. Untrusted pull requests need the
> egress work tracked in Q408 before we would recommend them, and that gap is
> stated on the roadmap rather than papered over.

Do not claim untrusted-PR readiness until the definition of done above is met.
The claim is checkable and a wrong one costs more than the feature is worth.

## Deliverables

Shipped: Kata validation, the pod isolation floor, default-deny NetworkPolicy,
per-tenant proxy pools.

Open, with rows: Q408 (egress posture and mirror design), Q539 (Kata plus
Dragonfly as the mirror backend), Q540 (composed node-layer plus guest-layer
stack), Q716 (metadata-server assertion), Q564 (proxy-side audit logging), Q566
(TLS on the proxy hop), Q567 (per-group proxy pool), Q215 (cache backend,
blocked on the cross-tenant isolation review this document scopes).

## Gaps with no row yet

Surfaced by drawing the whole picture, and each needs a decision before it needs
a row:

1. **Secret exposure to fork pull requests.** What GAG can and cannot influence,
   and what it should refuse to run. Interacts with `pull_request_target`
   semantics that belong to the repository, not the platform.
2. **Runner-token scope for an untrusted job.** The job's own credentials are a
   reachable asset. Whether an untrusted class should receive a narrower token
   than a trusted one is unexamined.
3. **What "untrusted" is, as a declared thing.** Today it is a posture an
   operator assembles. A declared class would let the platform apply the whole
   set at once, but the moat review rejected building that surface while five of
   its dependencies are open or blocked, which is the correct order. The decision
   this document owes is what the class would mean, not yet how to express it.
