# Caching and Worker Storage

> **Status: goal stated 2026-08-07, nothing built.** This is a map and a
> definition of done, not a design. The image-pull half already has a design in
> [q408-untrusted-pr-egress.md](q408-untrusted-pr-egress.md) and is not restated
> here. The job-cache and storage halves have no design yet, and the first thing
> this document does is establish that they are three different problems.

## Why this is a goal

Caching is the most common thing a CI platform is measured on that GAG has no
answer for, and it is the one place where the
[secure multi-tenant OSS CI](secure-multi-tenant-oss-ci.md) goal actively makes
the problem worse rather than better. Closing untrusted-PR egress removes the
cache that works today. Nothing in the individual Queue rows says that, which is
why they need an umbrella.

It is also where the competitive position is most often misread in GAG's own
favour and against it at the same time. See
[alternatives.md](../alternatives.md#no-offering-is-perfect-yet): ARC has no
in-cluster cache either, so this is not a reason to prefer ARC. GitLab Runner
and the managed services do have one, so it is a real loss against that lane.

## Three caches, routinely conflated

Treating these as one item is why the roadmap entry reads as though caching does
not work at all. They have different data, different threat models, and
different owners.

| | What it holds | Where it lives today | Row |
|---|---|---|---|
| **Image pulls** | Container images the node pulls to start a worker | Upstream registries, over per-tenant egress | [Q408](../STATUS.md#Q408), [Q539](../STATUS.md#Q539), [Q540](../STATUS.md#Q540) |
| **Job cache** | `actions/cache` entries: dependencies, toolchains, build outputs | **GitHub's Azure-blob store, and it works** | [Q215](../STATUS.md#Q215) |
| **Build layer cache** | Docker layers produced by an image build inside a job | Nowhere. Workers are storage-less | [Q215](../STATUS.md#Q215) |

Alongside them, and not a cache at all:

| | What it is | State | Row |
|---|---|---|---|
| **Shared job storage** | A `ReadWriteMany` volume several jobs mount to pass files | Unvalidated. Also what ARC's `containerMode: kubernetes` needs, so it is a migration blocker | [Q719](../STATUS.md#Q719) |

### `actions/cache` works today, and the roadmap says otherwise

`*.blob.core.windows.net` is in the GMC's default GitHub egress allowlist
(`cmd/gmc/internal/controller/egressproxy_fqdn.go`, the entry commented as the
Azure-blob-backed Actions results / cache / artifact store), asserted by both
`egressproxy_fqdn_test.go` and `egressproxy_builder_test.go`. A tenant's worker
reaches the Actions cache data plane through its own proxy pool like any other
GitHub endpoint.

So the accurate gap is **a cache inside the cluster**, not caching. What a local
cache would buy is egress cost and restore latency, not capability. Stating it
the current way concedes a loss that has not happened, and it hides the loss
that has: no local cache means every restore is a round trip out of the cluster
and back, which on-premises or on metered egress is the expensive path.

## The collision with the security goal

**This is the reason the two goals cannot be planned separately.**

[Q408](../STATUS.md#Q408) Phase 1 closes untrusted-PR egress down to GitHub,
the registry mirror, and DNS. The Actions cache data plane is a **non-GitHub
host**, so closing that egress removes `actions/cache`. This is already the
measured state of GAG's own self-hosted CI lane, where every `actions/cache`
step is skipped because the hardened egress rule does not admit it
([testing.md](../development/testing.md#the-e2e-workflows-kindnet-and-calico), and
[q408 §3](q408-untrusted-pr-egress.md) has the measurement).

The consequences run one way:

1. For a **trusted** tenant, `actions/cache` works and a local cache is an
   optimisation.
2. For an **untrusted-PR** tenant under the Q408 posture, `actions/cache` is
   gone, and an in-cluster cache is the only cache there can be. Q215 stops
   being an optimisation and becomes the thing that makes the security posture
   usable.
3. That in-cluster cache is then reachable by hostile job code by construction,
   which is exactly the review Q215 is blocked on. A cache shared across
   tenants is an exfiltration path; a cache shared across *jobs of one tenant*
   is the useful thing and a narrower risk.

The sequencing that falls out: **the cross-tenant isolation review is not a
prerequisite to be cleared, it is the design.** Q215 should not be scoped as
"add a PVC" and then reviewed. It should be scoped as "what cache can an
untrusted job be given", and the storage follows from the answer.

## Definition of done

Each demonstrated rather than argued:

1. A worker pulls its image from an in-cluster source on a warm node, and an
   end-to-end run's logs name no upstream registry.
2. A job restores a dependency cache without leaving the cluster, and the same
   workflow run on a hardened-egress tenant restores it too.
3. A job cannot read another tenant's cache entries, asserted by a test rather
   than by namespace convention.
4. Cache entries written by an untrusted fork pull request cannot be read by a
   trusted job of the same tenant, or that boundary is explicitly declared
   absent with a reason.
5. A `ReadWriteMany` volume is mounted into a worker in a validated reference
   architecture, and the storage classes it has been exercised against are
   named. Failing that, the stance that workers stay storage-less is written
   down as a decision with its migration consequence for ARC's
   `containerMode: kubernetes`.
6. The cost claim is measured, not asserted: egress bytes and restore latency
   with and without the local cache, on one real workload.

## Explicitly out of scope

- **Building a cache service.** The deliverable is integration and a validated
  reference architecture, not a new storage system. If an existing backend
  (an S3-compatible store, a `ReadWriteMany` class, Dragonfly for images) can
  hold it, that is the answer.
- **Cross-tenant cache sharing, at any scale.** Deduplicating identical entries
  across tenants is the obvious efficiency and the obvious exfiltration path.
  Not now, and not without a threat model that survives the untrusted
  contributor in [secure-multi-tenant-oss-ci.md](secure-multi-tenant-oss-ci.md).
- **Persistent worker state between jobs.** A worker executes one job and is
  deleted. A cache is data a *new* worker can mount, never a worker that
  survives.
- **Competing with the managed lane on build speed.** Those services own
  hardware and a purpose-built cache tier. Local caching here is about egress
  cost and untrusted-PR viability, and
  [go-to-market §1](go-to-market.md) disclaims the speed race outright.

## What the site may claim today

Current state only:

> `actions/cache` works on a GAG tenant today: the Actions cache data plane is
> in the default egress allowlist. What is missing is a cache *inside* the
> cluster, which would cut egress cost and restore latency. ARC has none
> either, so this is not a reason to prefer ARC; GitLab Runner and the managed
> services do have one.

Do not claim a local cache, a shared build cache, or persistent worker storage.
Do not repeat that `actions/cache` has no home.

## Deliverables

Shipped: per-tenant egress that admits the Actions cache data plane; the
registry-mirror contract and the Athens pattern it derives from.

Open, with rows: [Q408](../STATUS.md#Q408) (mirror design and phases),
[Q539](../STATUS.md#Q539) (Dragonfly as the mirror backend),
[Q540](../STATUS.md#Q540) (composed node-layer and guest-layer stack),
[Q215](../STATUS.md#Q215) (job and build-layer cache, blocked on the isolation
review this document reframes as the design),
[Q719](../STATUS.md#Q719) (RWX validation and the reference-architecture
stance), [Q268](../STATUS.md#Q268) (warm worker pool, the competing lever for
the latency half of the same complaint).

## Gaps with no row yet

Each needs a decision before it needs a row:

1. **Whether an untrusted job gets a cache at all.** The honest answer may be
   no, in which case the untrusted lane is permanently slower and that should be
   a stated trade rather than an unmet goal.
2. **Cache entry lifetime and eviction.** GitHub's own cache has a size cap and
   an eviction policy tenants already reason about. A local cache with different
   semantics is a surprise, and matching them is work nobody has scoped.
3. **Who pays for the storage.** A cache inside a tenant's namespace consumes
   the platform-owned `ResourceQuota` that bounds the tenant, and a cache
   outside it is platform capacity a tenant can fill. Neither is obviously
   right, and the quota model has no answer today.
