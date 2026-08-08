# Sizing the platform-owned `ResourceQuota`

> **Audience:** Platform engineer

Turn a tenant's runner shapes and concurrency ceilings into the numbers you put
in their namespace `ResourceQuota`, so the first install is a calculation rather
than a guess. The quota is platform-owned — the gateway operates inside it but
never creates or mutates it (see
[tenant onboarding Step 1b](tenant-onboarding.md#step-1b-set-the-platform-owned-resourcequota)).

Getting it wrong fails in two directions, both quiet:

- **Too low** — worker pods are rejected or sit `Pending`, and the Actions
  Gateway Controller (AGC) leaves jobs queued at GitHub rather than claiming
  them.
- **Constrained on the wrong key** — a quota that constrains `limits.cpu`
  rejects every pod that declares no CPU limit, which includes the recommended
  Docker-in-Docker (DinD) worker shape. See
  [Only constrain keys every pod declares](#only-constrain-keys-every-pod-declares).

## Table of Contents

- [What the quota actually counts](#what-the-quota-actually-counts)
- [Step 1 — the footprint of one worker pod](#step-1--the-footprint-of-one-worker-pod)
- [Step 2 — multiply by concurrency, add the control plane](#step-2--multiply-by-concurrency-add-the-control-plane)
- [Step 3 — the storage keys](#step-3--the-storage-keys)
- [Only constrain keys every pod declares](#only-constrain-keys-every-pod-declares)
- [Sizing profiles change the ask at pod build](#sizing-profiles-change-the-ask-at-pod-build)
- [Worked example — a DinD tenant at 12 concurrent jobs](#worked-example--a-dind-tenant-at-12-concurrent-jobs)
- [What the gateway's own quota conditions count](#what-the-gateways-own-quota-conditions-count)
- [Related](#related)

## What the quota actually counts

A worker pod's quota footprint is **not** the sum of its
`podTemplate.spec.containers`. Kubernetes composes it from four parts, and two of
them are easy to miss on exactly the worker shapes that cost the most.

| Pod element | Counts toward `requests.*` / `limits.*` | Why it matters here |
|---|---|---|
| Regular containers | **Summed** | The `runner` container. |
| Native sidecars (`restartPolicy: Always` init containers) | **Summed** | The DinD daemon is declared this way, and must be — a regular-container sidecar strands the pod (Q249). Its whole ask counts. |
| Plain init containers | **`max()` against the regular-container sum** | The AGC-injected `gag-wrapper-install` init container declares no resources, so it contributes nothing. A tenant-authored init container only matters if it out-asks the runner. |
| `RuntimeClass` pod overhead (`overhead.podFixed`) | **Added** to requests in full; added to limits **only for a key the pod already limits** with a non-zero value | Kata workers carry `250m` CPU / `160Mi` memory per pod on top of their containers. The asymmetry matters: a shape with no CPU limit gets no overhead on `limits.cpu`. |

Verified on Kubernetes v1.36.1 by applying one pod at a time into a quota'd
namespace and reading the `.status.used` delta. Reproduce it on any cluster:

```sh
kubectl get resourcequota <QUOTA_NAME> -n <NAMESPACE> -o jsonpath='{.status.used}'
```

The practical consequence: **a DinD or Kata worker costs materially more quota
than its runner container's `requests` suggest.** For the reference shapes in
`deploy/dogfood-e2e/overlays/`, the sidecar is 25% of the privileged-DinD pod's
CPU request and 75% of its memory request.

## Step 1 — the footprint of one worker pod

Sum, per pod, across **regular containers plus native sidecars**, then add any
`RuntimeClass` overhead:

```
pod.requests.cpu    = Σ containers req.cpu    + Σ nativeSidecars req.cpu    + overhead.cpu
pod.requests.memory = Σ containers req.memory + Σ nativeSidecars req.memory + overhead.memory
pod.limits.cpu      = Σ containers lim.cpu    + Σ nativeSidecars lim.cpu    [+ overhead.cpu]
pod.limits.memory   = Σ containers lim.memory + Σ nativeSidecars lim.memory [+ overhead.memory]
```

The bracketed overhead terms apply **only if that limit is already non-zero**. A
shape declaring no CPU limit is not charged `overhead.cpu` against `limits.cpu`.

Where the inputs come from:

| Term | Source | Default if unset |
|---|---|---|
| Container `requests`/`limits` | `podTemplate.spec.containers[].resources` on the `RunnerTemplate` (v2) or `runnerGroups[].podTemplate` (v1) | `500m` / `1Gi` as **both** requests and limits — the AGC gap-fills any container declaring neither, so a bare template still costs quota |
| Native sidecar `requests`/`limits` | `podTemplate.spec.initContainers[]` entries with `restartPolicy: Always` | none — the gap-fill deliberately skips init containers, so an undeclared sidecar contributes `0` |
| `overhead.podFixed` | `podTemplate.spec.overhead` when the template declares it, else the `RuntimeClass` named by `podTemplate.spec.runtimeClassName` | none (no `runtimeClassName` and no declared `overhead` ⇒ no overhead) |

A term with no value drops out. A shape that declares requests but no limits
contributes `0` to the `limits.*` rows — which is a constraint on *which quota
keys you may set*, not free capacity. See
[Only constrain keys every pod declares](#only-constrain-keys-every-pod-declares).

## Step 2 — multiply by concurrency, add the control plane

Concurrency is `maxWorkers` per runner group or runner set. It has **no default**
— an unset `maxWorkers` is an unbounded worker pool with no ceiling to size
against, so set it before sizing the quota.

```
pods            = Σ_sets(maxWorkers) + proxy.maxReplicas + 1
requests.cpu    = Σ_sets(maxWorkers × pod.requests.cpu)    + proxy.maxReplicas × proxyReq.cpu    + agcReq.cpu
requests.memory = Σ_sets(maxWorkers × pod.requests.memory) + proxy.maxReplicas × proxyReq.memory + agcReq.memory
limits.cpu      = Σ_sets(maxWorkers × pod.limits.cpu)      + proxy.maxReplicas × proxyLim.cpu    + agcLim.cpu
limits.memory   = Σ_sets(maxWorkers × pod.limits.memory)   + proxy.maxReplicas × proxyLim.memory + agcLim.memory
```

The `+ 1` pod is the AGC control-plane pod. The control-plane terms:

| Term | Default | Set by |
|---|---|---|
| `proxy.maxReplicas` | `10` | `spec.proxy.maxReplicas` (v1) / `EgressProxy.spec.maxReplicas` (v2) |
| `proxyReq.cpu` / `proxyReq.memory` | `10m` / `32Mi` | `spec.proxy.resources.requests` |
| `proxyLim.cpu` / `proxyLim.memory` | `500m` / `64Mi` | `spec.proxy.resources.limits` |
| `agcReq` / `agcLim` (**v2**) | requests `500m` / `2Gi`, limits `2` / `4Gi` | `spec.agcResources`, overlaid per key on these defaults |
| `agcReq` / `agcLim` (**v1**) | **none stamped** | the platform, on the AGC Deployment, or a namespace `LimitRange` |

Two adjustments to make before you write the numbers down:

- **Sum over every runner set, not just the first.** Each has its own
  `maxWorkers` and pod shape; a GPU set with a large per-pod ask can dominate the
  total at a low `maxWorkers`.
- **Round up.** Leave headroom for the next scale-out and for any platform
  daemon pods scheduled into the namespace.

On the v2 API a proxy pool that opts out of the managed autoscaler
(`EgressProxy.spec.managedAutoscaling: false`) makes `maxReplicas` inert — size
the proxy term to your own autoscaler's ceiling instead.

## Step 3 — the storage keys

Two independent storage families apply, and a quota that constrains either
without accounting for workers will block every worker pod.

| Quota key | What consumes it | Per worker |
|---|---|---|
| `requests.ephemeral-storage`, `limits.ephemeral-storage` | the containers' own `ephemeral-storage` asks | summed like CPU/memory |
| `persistentvolumeclaims`, `<class>.storageclass.storage.k8s.io/persistentvolumeclaims` | each **generic ephemeral volume** in the pod (`volumes[].ephemeral`) creates a real PVC named `<pod>-<volume>` | 1 per ephemeral volume — a **count of objects**, unrelated to how much each asks for |
| `requests.storage`, `<class>.storageclass.storage.k8s.io/requests.storage` | that PVC's `volumeClaimTemplate` ask | the declared `storage`, summed across the pod's ephemeral volumes |

The class-scoped keys apply only when the claim template names a
`storageClassName`. An unset one still resolves to the cluster default class and
is charged under *that* class's keys — but the name is not knowable from the
template, so the gateway's own footprint counts only the unscoped
`persistentvolumeclaims` / `requests.storage` for such a volume. If you constrain
a class-scoped key, name the class in the claim template too, or the conditions
will not see that cap.

A pod that mounts a **pre-existing** PVC (`volumes[].persistentVolumeClaim`)
creates nothing and is charged nothing here — that claim was charged when it was
created.

This is what bites Kata tenants. The reference Kata worker shape mounts a
per-pod 100Gi raw block device for `/var/lib/docker`, so a Kata set at
`maxWorkers: 4` needs `persistentvolumeclaims: 4` and `requests.storage: 400Gi`
on top of its CPU and memory. A namespace quota that sets
`persistentvolumeclaims` for other reasons — and does not raise it for workers —
silently caps concurrency at whatever that number is.

### A storage quota fails later, and quieter, than a compute quota

The two families are enforced at different moments, and the difference decides
what you will see when it goes wrong.

A compute key is charged when the **pod** is admitted, so an exhausted
`requests.cpu` rejects the pod outright and the AGC retries. A PVC key is charged
against a **separate object**: the pod is admitted first, and Kubernetes' own
ephemeral-volume controller then creates the PVC behind it. If that create is
quota-rejected, nothing rejects the pod — it sits `Pending` with an unbound
volume, holding a job that has already been claimed from GitHub, until the pod
deadline reaps it.

The gateway counts these keys in its worker footprint precisely so that case is
caught before the job is claimed. Diagnose a suspected one on the PVC, not the
pod:

```sh
kubectl get events -n <NAMESPACE> --field-selector reason=FailedCreate
```

## Only constrain keys every pod declares

A `ResourceQuota` that constrains a compute key makes that declaration
**mandatory** for every pod in the namespace. Kubernetes rejects the pod
outright:

```
pods "runner-abc123" is forbidden: failed quota: tenant-quota: must specify limits.cpu for: runner
```

This is the single most common self-inflicted failure, because the measured DinD
worker shapes deliberately set **no CPU limit** — CPU is compressible, and
throttling a DinD daemon slows every container inside it. Constraining
`limits.cpu` on such a namespace rejects 100% of worker pods.

Before applying a quota, check each key you intend to constrain:

| Key constrained | Every pod must declare it — check |
|---|---|
| `requests.cpu` / `requests.memory` | worker containers (or the AGC gap-fill covers them), native sidecars, the proxy, and the AGC pod |
| `limits.cpu` / `limits.memory` | the same set — and native sidecars often declare only memory limits |
| `pods` only | nothing else to check; no compute cap, but no rejection risk either |

Two ways to satisfy it:

- **Pair the quota with a `LimitRange`** that supplies namespace default requests
  (and limits, if you constrain them). On **v1** this is required rather than
  optional: the GMC stamps no resources on the AGC pod, so a `requests.cpu`-
  constrained namespace rejects the AGC Deployment's pods with
  `must specify requests.cpu`. The v2 AGC carries its own defaults and does not
  need this.
- **Constrain only `pods`**, plus the storage keys if PVCs are in play. You lose
  the compute cap and keep a hard concurrency ceiling.

## Sizing profiles change the ask at pod build

An opt-in [sizing profile](worker-rightsizing.md#sizing-profiles-opt-in-auto-apply)
(`RunnerSet.spec.sizing.profile`, v2 only) rewrites the pod's CPU/memory values
when the pod is built, so **the template's static ask is not what the quota
sees**.

| Profile | Effect on the quota footprint |
|---|---|
| `Static` (default) | None — the template's values are what runs. Size from the template. |
| `NodeShare` | The **runner** container's requests become `allocatable ÷ workersPerNode`. Sidecars keep their template ask. Actuates on the first job — no usage history needed. |
| `Binpack` | Requests `==` limits from measured history, for **every** container. Actuates only once all containers have ≥20 sampled jobs. |
| `Throughput` | Requests from history; **the CPU limit is deleted**, and the memory limit becomes the observed peak × `limitHeadroomPercent` (default 150). Same sample threshold. |

Sizing implications:

- **`Throughput` deletes the CPU limit.** Do not constrain `limits.cpu` on a
  namespace running it.
- **`Binpack` and `Throughput` only actuate after 20 samples per container.**
  Until then pods provision with the template's static values, so size the quota
  for the *template*, which is the larger of the two in the case that matters
  (an over-asking template is what these profiles exist to shrink).
- **Bound the derived values** with `sizing.minRequests` / `sizing.maxRequests` if
  you want a quota number that cannot be invalidated by a shift in measured usage.

Check what a set is actually doing:

```sh
kubectl get runnerset <NAME> -n <NAMESPACE> -o jsonpath='{.status.sizingProfileState}'
```

`Active` means derived values are in effect; `AwaitingSamples` means the template's
static values are.

## Worked example — a DinD tenant at 12 concurrent jobs

**Inputs.** A tenant running Docker-in-Docker CI on the measured reference shape
(the shipped `privileged-dind` entry, `deploy/templates/privileged-dind/template.yaml`;
see [runner template library](runner-template-library.md)), with the default
egress proxy pool, on the v2 API, `sizing.profile: Static`.

| Input | Value |
|---|---|
| Runner container | requests `cpu: 3`, `memory: 1Gi`; limits `memory: 3Gi` (no CPU limit) |
| `dind` native sidecar | requests `cpu: 1`, `memory: 3Gi`; limits `memory: 4Gi` (no CPU limit) |
| `runtimeClassName` | unset — no pod overhead |
| `maxWorkers` | `12` |
| `proxy.maxReplicas` | `10` (default) |
| AGC | v2 defaults: requests `500m` / `2Gi`, limits `2` / `4Gi` |

**Step 1 — one worker pod.** Sum the runner *and* the native sidecar:

| | runner | `dind` sidecar | pod total |
|---|---|---|---|
| `requests.cpu` | 3 | 1 | **4** |
| `requests.memory` | 1Gi | 3Gi | **4Gi** |
| `limits.cpu` | — | — | **none declared** |
| `limits.memory` | 3Gi | 4Gi | **7Gi** |

**Step 2 — multiply and add the control plane.**

| Quota key | Workers (×12) | Proxy (×10) | AGC | Total | Rounded up |
|---|---|---|---|---|---|
| `pods` | 12 | 10 | 1 | 23 | `25` |
| `requests.cpu` | 48 | 100m | 500m | 48.6 | `50` |
| `requests.memory` | 48Gi | 320Mi | 2Gi | ~50.3Gi | `52Gi` |
| `limits.memory` | 84Gi | 640Mi | 4Gi | ~88.6Gi | `90Gi` |

**Step 3 — storage.** This shape declares no `ephemeral-storage` asks and no
ephemeral volumes, so no storage keys are needed — and the gateway charges it
nothing on them, so a `persistentvolumeclaims` cap set for something else in this
namespace will not hold the set back.

The Kata variant is where this bites. Swapping in
`deploy/dogfood-e2e/overlays/kata/`'s worker adds one 100Gi ephemeral volume per
pod on the `standard-rwo` class, so the same 12 workers need four more keys:

| Quota key | Workers (×12) |
|---|---|
| `persistentvolumeclaims` | 12 |
| `requests.storage` | `1200Gi` |
| `standard-rwo.storageclass.storage.k8s.io/persistentvolumeclaims` | 12 |
| `standard-rwo.storageclass.storage.k8s.io/requests.storage` | `1200Gi` |

Set only the keys you intend to constrain — but whichever you set, size it for
all 12. `WorkerQuotaPressure` trips on the tightest of them.

**`limits.cpu` is deliberately absent** from the quota below: neither worker
container declares a CPU limit, so constraining it would reject every worker pod.

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: dind-team-quota
  namespace: dind-team
spec:
  hard:
    pods: "25"
    requests.cpu: "50"
    requests.memory: "52Gi"
    # limits.cpu is intentionally NOT constrained: the DinD runner and sidecar
    # declare no CPU limit, and constraining it would reject every worker pod
    # with "must specify limits.cpu for: runner".
    limits.memory: "90Gi"
```

```sh
kubectl apply -f dind-team-quota.yaml
```

Apply it with an administrator identity — the quota is platform-owned, and the
tenant's gateway has no write access to it.

**Verify** the tenant fits before jobs arrive. Both controllers publish advisory
conditions when the remaining headroom cannot cover the configured ceilings:

```sh
kubectl get actionsgateway <NAME> -n <NAMESPACE> -o jsonpath='{range .status.conditions[?(@.type=="ProxyQuotaPressure")]}{.status}{" "}{.message}{"\n"}{end}'
```

```sh
kubectl get runnerset <NAME> -n <NAMESPACE> -o jsonpath='{range .status.conditions[?(@.type=="WorkerQuotaPressure")]}{.status}{" "}{.message}{"\n"}{end}'
```

`False` on both means the quota admits scaling to `maxReplicas` and `maxWorkers`.

## What the gateway's own quota conditions count

The gateway computes a worker's footprint with the same arithmetic this page uses:
the four-part compute sum (regular containers, native sidecars, the plain-init
`max()` floor, and `RuntimeClass` overhead) **plus** the storage keys from
[Step 3](#step-3--the-storage-keys) — `ephemeral-storage` on both sides, and the
PVC count and storage ask of every generic ephemeral volume, class-scoped keys
included. One calculation feeds all three surfaces, so they cannot disagree:

- the `WorkerQuotaPressure` / `WorkerQuotaExceeded` conditions;
- the pre-claim quota gate on the classic acquisition tier;
- the capacity integer a scale-set runner set advertises to GitHub, which folds
  in live quota headroom on the default tier.

So a `False` `WorkerQuotaPressure` means the quota admits scaling to `maxWorkers`
at the pod's **full** cost — sidecar, overhead, and per-worker PVCs included.

A worker shape that declares no `ephemeral-storage` and no ephemeral volumes is
charged nothing on the storage keys, so a namespace already at its
`persistentvolumeclaims` ceiling for unrelated reasons does not hold such a set
back.

!!! warning "This changed — conditions may now trip where they did not before"

    Before this fix the footprint covered CPU and memory summed over
    `spec.containers` alone. Two things were missing.

    **Init containers and pod overhead.** Dropping all init containers was correct
    for plain ones but **wrong for native sidecars**, which Kubernetes sums;
    `RuntimeClass` overhead was ignored outright:

    | Shape | Former per-pod under-count |
    |---|---|
    | Privileged DinD | `1` CPU / `3Gi` requests; `4Gi` memory limit |
    | Kata | `2` CPU / `6Gi` requests; `4` CPU / `8Gi` limits; plus `250m` / `160Mi` overhead |

    **The storage keys.** `ephemeral-storage`, `persistentvolumeclaims` and
    `requests.storage` were not counted at all, so a shape's entire storage cost
    read as zero. The reference Kata worker was under-counted by its whole
    per-worker PVC — `1` claim and `100Gi` per pod, or `4` claims and `400Gi` at
    `maxWorkers: 4`.

    On upgrade, a tenant whose quota was sized against those under-counted numbers
    can go straight to `WorkerQuotaPressure=True` (or `WorkerQuotaExceeded=True`)
    with no change to the quota or the workload. **Nothing got worse** — such a
    quota could never place those pods; a compute-key shortfall was already
    rejecting them at creation, and a storage-key shortfall was already stranding
    them `Pending` on an unbound volume. The condition just reports it up front now
    instead of leaving it to show up as burnt lock time. A quota already sized with
    the arithmetic on this page is unaffected; re-derive any that were not.

### Pod overhead needs a cluster-scoped read

Overhead lives on the cluster-scoped `RuntimeClass`, not on the pod template, so
the AGC reads it to size a Kata worker. The chart grants this through the
per-gateway `agc-clusterrunnertemplate-reader` `ClusterRole`; a cluster whose AGC
image is upgraded ahead of its chart has no such grant.

The read is **fail-open**: without it the footprint simply omits overhead and the
conditions behave as they did before, so nothing breaks — a Kata worker is just
under-counted by its `overhead.podFixed` again. Confirm the grant landed:

```sh
kubectl auth can-i get runtimeclasses --as=system:serviceaccount:<NAMESPACE>:<AGC_SERVICEACCOUNT>
```

Two cases need no grant at all, because no read happens: a pod template with no
`runtimeClassName`, and one that declares `spec.overhead` itself. A declared
`overhead` always wins over the read, and cannot drift — the `RuntimeClass`
admission controller rejects any pod whose declared overhead differs from its
`RuntimeClass`'s.

## Related

- [Tenant onboarding Step 1b](tenant-onboarding.md#step-1b-set-the-platform-owned-resourcequota) — where the quota is applied in the onboarding flow.
- [Worker right-sizing](worker-rightsizing.md) — derive the per-container `requests`/`limits` this page multiplies, from measured usage.
- [Kata DinD workloads](kata-dind-workloads.md) — the Kata worker shape, its `RuntimeClass`, and the nested-virtualization node prerequisites.
- [Appendix E — capacity planning](../design/appendix-e-capacity-planning.md) — runner group topology, `maxListeners`, and the rate-limit ceiling.
- [Troubleshooting: quota exhaustion](troubleshooting.md#jobs-failing-due-to-namespace-resourcequota-exhaustion) — diagnosing a quota that is already too small.
