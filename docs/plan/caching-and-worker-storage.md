# Caching and Worker Storage

> **Status: goal stated 2026-08-07; one of its seven definition-of-done items closed 2026-08-24.** This is a map and a definition of done, not a design.
> The storage half is the one that closed (Q719, item 5): see [worker-shared-storage.md](../operations/worker-shared-storage.md).
> Every cache half is still unbuilt.
> The image-pull half already has a design in [q408-untrusted-pr-egress.md](q408-untrusted-pr-egress.md) and is not restated here.
> The job-cache and storage halves have no design yet, and the first thing this document does is establish that they are three different problems.

## Why this is a goal

Caching is the most common thing a CI platform is measured on that GAG has no answer for, and it is the one place where the [secure multi-tenant OSS CI](secure-multi-tenant-oss-ci.md) goal actively makes the problem worse rather than better.
Closing untrusted-PR egress removes the cache that works today.
Nothing in the individual Queue rows says that, which is why they need an umbrella.

It is also where the competitive position is most often misread in GAG's own favour and against it at the same time.
See [alternatives.md](../alternatives.md#no-offering-is-perfect-yet): ARC has no in-cluster cache either, so this is not a reason to prefer ARC.
GitLab Runner and the managed services do have one, so it is a real loss against that lane.

## Three caches, routinely conflated

Treating these as one item is why the roadmap entry reads as though caching does not work at all.
They have different data, different threat models, and different owners.

**This is a split by problem, not by tool.** The content classes are genuinely distinct; the backends that serve them overlap heavily, and the section after next is about that overlap.
Do not read this table as three deployments.

| | What it holds | Where it lives today | Row |
|---|---|---|---|
| **Image pulls** | Container images the node pulls to start a worker | Upstream registries, over per-tenant egress | [Q408](../queue/Q408.md), [Q539](../queue/Q539.md), [Q540](../queue/Q540.md) |
| **Job cache** | `actions/cache` entries: dependencies, toolchains, build outputs | **GitHub's Azure-blob store, and it works** | [Q215](../queue/Q215.md) |
| **Build layer cache** | Docker layers produced by an image build inside a job | Nowhere. Workers are storage-less | [Q215](../queue/Q215.md) |

Alongside them, and not a cache at all:

| | What it is | State | Row |
|---|---|---|---|
| **Shared job storage** | A `ReadWriteMany` volume several jobs mount to pass files | Validated 2026-08-24 and documented as a reference architecture: [worker-shared-storage.md](../operations/worker-shared-storage.md). Also what ARC's `containerMode: kubernetes` needs | Closed (Q719) |

### `actions/cache` works today, and the roadmap says otherwise

`*.blob.core.windows.net` is in the GMC's default GitHub egress allowlist (`cmd/gmc/internal/controller/egressproxy_fqdn.go`, the entry commented as the Azure-blob-backed Actions results / cache / artifact store), asserted by both `egressproxy_fqdn_test.go` and `egressproxy_builder_test.go`.
A tenant's worker reaches the Actions cache data plane through its own proxy pool like any other GitHub endpoint.

### One backend can serve several classes

Distinct problems do not imply distinct deployments, and scoping them as three projects is how this becomes three times the work it needs to be.

Dragonfly is the concrete case, and it is already the scheduled mirror candidate in [Q539](../queue/Q539.md).
It is a **general file-distribution system** whose registry-mirror use is one application, not its definition.
Measured from the upstream docs on 2026-08-07:

- `dfget` downloads arbitrary files and directories, with native backends for HTTP/HTTPS, **S3, GCS, Azure Blob Storage, OSS, OBS, COS, HDFS**, plus Hugging Face and ModelScope.
- `dfdaemon` exposes a configurable HTTP proxy whose rules are content-agnostic, rather than an image-specific shim.
- **Git LFS is a documented integration**, which is the existing proof that a non-image artifact class rides the same P2P mesh.

The Azure Blob Storage backend is the one to notice: `actions/cache` stores to `*.blob.core.windows.net`, which is exactly the host the hardened posture in the next section removes.
Whether a Dragonfly deployment can front the Actions cache API specifically is **unverified** and not a small question, since that API is authenticated and signed-URL based rather than a plain blob `GET`.
Treat this as the thing Q539 and Q540 should be scoped to answer, not as a capability GAG has.

### The mirror contract is read-only, and a job cache is not

The obstacle is not the backend.
It is the contract.

[q408 §3.5](q408-untrusted-pr-egress.md#35-the-mirror-role-is-a-contract) defines the mirror role as four properties, one of which is that **non-GET operations are refused, not forwarded**.
That property is load-bearing: it is most of why an untrusted job can be allowed to talk to the mirror at all.

A job cache is read-write by definition.
`actions/cache` saves entries as well as restoring them, and a cache nothing writes to is empty.
So the artifact cache **cannot simply reuse the mirror role**, however well the same software serves both.
It needs either a second role with its own contract, or a writable variant whose isolation argument is made separately, and that argument is the hard part: a write path reachable by an untrusted fork pull request is a way to plant content that a later trusted job restores and executes.

That is the sharpest version of the review [Q215](../queue/Q215.md) is blocked on, and it is a contract question rather than a storage question.

### The accurate gap

The gap is **a cache inside the cluster**, not caching.
What a local cache would buy is egress cost and restore latency, not capability.
Stating it the current way concedes a loss that has not happened, and it hides the loss that has: no local cache means every restore is a round trip out of the cluster and back, which on-premises or on metered egress is the expensive path.

## The collision with the security goal

**This is the reason the two goals cannot be planned separately.**

[Q408](../queue/Q408.md) Phase 1 closes untrusted-PR egress down to GitHub, the registry mirror, and DNS.
The Actions cache data plane is a **non-GitHub host**, so closing that egress removes `actions/cache`.
This is already the measured state of GAG's own self-hosted CI lane, where every `actions/cache` step is skipped because the hardened egress rule does not admit it ([testing.md](../development/testing.md#the-e2e-workflows-kindnet-and-calico), and [q408 §3](q408-untrusted-pr-egress.md) has the measurement).

The consequences run one way:

1. For a **trusted** tenant, `actions/cache` works and a local cache is an optimisation.
2. For an **untrusted-PR** tenant under the Q408 posture, `actions/cache` is gone, and an in-cluster cache is the only cache there can be.
   Q215 stops being an optimisation and becomes the thing that makes the security posture usable.
3. That in-cluster cache is then reachable by hostile job code by construction, which is exactly the review Q215 is blocked on.
   A cache shared across tenants is an exfiltration path; a cache shared across *jobs of one tenant* is the useful thing and a narrower risk.
4. And it must be **writable** by that job, which is the property the mirror role deliberately refuses.
   The two roles cannot be the same endpoint even when they are the same software.

The sequencing that falls out: **the cross-tenant isolation review is not a prerequisite to be cleared, it is the design.** Q215 should not be scoped as "add a PVC" and then reviewed.
It should be scoped as "what cache can an untrusted job be given", and the storage follows from the answer.

## Definition of done

Each demonstrated rather than argued:

1. A worker pulls its image from an in-cluster source on a warm node, and an end-to-end run's logs name no upstream registry.
2. A job restores a dependency cache without leaving the cluster, and the same workflow run on a hardened-egress tenant restores it too.
3. A job cannot read another tenant's cache entries, asserted by a test rather than by namespace convention.
4. Cache entries written by an untrusted fork pull request cannot be read by a trusted job of the same tenant, or that boundary is explicitly declared absent with a reason.
5. The write path is contracted, not incidental: whatever endpoint accepts a cache save states what it accepts, from whom, and what it refuses, in the same form [q408 §3.5](q408-untrusted-pr-egress.md#35-the-mirror-role-is-a-contract) states the read path.
   Four properties for the mirror, its own set for this.
6. ✅ **Closed 2026-08-24 (Q719).** A `ReadWriteMany` volume is mounted into a worker in a validated reference architecture, and the storage classes it has been exercised against are named: [worker-shared-storage.md](../operations/worker-shared-storage.md).
   Both halves landed rather than one instead of the other.
   The stance is written down (workers stay storage-less; a shared volume is a tenant `podTemplate` concern GAG never provisions), with its migration consequence for ARC's `containerMode: kubernetes`, and `make test-rwx-storage` is what keeps the reference architecture a measurement.
   One class has been exercised, `gag-rwx-nfs` over csi-driver-nfs v4.13.4; every cloud filesystem is explicitly unvalidated, and the harness takes the class to test in `RWX_STORAGE_CLASS` so an operator can close that on their own.
7. The cost claim is measured, not asserted: egress bytes and restore latency with and without the local cache, on one real workload.

## Explicitly out of scope

- **Building a cache service.** The deliverable is integration and a validated reference architecture, not a new storage system.
  If an existing backend can hold it, that is the answer, and preferring one backend across content classes over a best-of-breed per class is deliberate: each additional deployment is another NetworkPolicy scope, another upstream allowlist, and another thing an untrusted job can reach.
- **Cross-tenant cache sharing, at any scale.** Deduplicating identical entries across tenants is the obvious efficiency and the obvious exfiltration path.
  Not now, and not without a threat model that survives the untrusted contributor in [secure-multi-tenant-oss-ci.md](secure-multi-tenant-oss-ci.md).
- **Persistent worker state between jobs.** A worker executes one job and is deleted.
  A cache is data a *new* worker can mount, never a worker that survives.
- **Competing with the managed lane on build speed.** Those services own hardware and a purpose-built cache tier.
  Local caching here is about egress cost and untrusted-PR viability, and [go-to-market §1](go-to-market.md) disclaims the speed race outright.

## What the site may claim today

Current state only:

> `actions/cache` works on a GAG tenant today: the Actions cache data plane is in the default egress allowlist.
> What is missing is a cache *inside* the cluster, which would cut egress cost and restore latency.
> ARC has none either, so this is not a reason to prefer ARC; GitLab Runner and the managed services do have one.

Do not claim a local cache, a shared build cache, or persistent worker storage.
Do not repeat that `actions/cache` has no home.

## Deliverables

Shipped: per-tenant egress that admits the Actions cache data plane; the registry-mirror contract and the Athens pattern it derives from; the RWX validation and the reference-architecture stance (Q719, [worker-shared-storage.md](../operations/worker-shared-storage.md)).

Open, with rows: [Q408](../queue/Q408.md) (mirror design and phases), [Q539](../queue/Q539.md) (Dragonfly as the mirror backend), [Q540](../queue/Q540.md) (composed node-layer and guest-layer stack), [Q215](../queue/Q215.md) (job and build-layer cache, blocked on the isolation review this document reframes as the design), [Q268](../queue/Q268.md) (warm worker pool, the competing lever for the latency half of the same complaint).

**Scope note for Q539 and Q540.** Both are currently written as image-only validations.
Because the candidate backend is a general file distributor, both should also answer whether *one* deployment serves the artifact class as well, and at what cost to the §3.5 contract.
Answering it there is nearly free while the deployment is already stood up, and expensive later.
Grade the four contract properties **per content class**, not once: a backend that refuses non-GET for images has said nothing about what it does for a cache save.

## Gaps with no row yet

Each needs a decision before it needs a row:

1. **Whether an untrusted job gets a cache at all.** The honest answer may be no, in which case the untrusted lane is permanently slower and that should be a stated trade rather than an unmet goal.
2. **Cache entry lifetime and eviction.** GitHub's own cache has a size cap and an eviction policy tenants already reason about.
   A local cache with different semantics is a surprise, and matching them is work nobody has scoped.
3. **Who pays for the storage.** A cache inside a tenant's namespace consumes the platform-owned `ResourceQuota` that bounds the tenant, and a cache outside it is platform capacity a tenant can fill.
   Neither is obviously right, and the quota model has no answer today.
