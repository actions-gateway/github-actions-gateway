# Registry pull-through cache — in-cluster container-image mirror (dogfood)

Five [CNCF Distribution](https://github.com/distribution/distribution) instances in pull-through cache mode, one per upstream registry the dogfood e2e suite pulls from.
Workers reach the mirror; the mirror reaches the registry.
It is a **cache**: a miss is fetched upstream and re-populated, so correctness never depends on the cache surviving.

This is the container-image sibling of [`deploy/athens/`](../athens/README.md) (Go modules), and the concrete artifact behind the recommendation in [`docs/operations/security-operations.md`](../../docs/operations/security-operations.md#prefer-an-in-cluster-caching-mirror-first).
It exists so the Kata e2e variant can carry untrusted-PR CI, and it does: every registry pull rides the mirror, the workers' allow-all egress policy is deleted, and their reachable set is cluster DNS, GitHub, and these five Services.
What is measured, and what is not, is [below](#what-is-proven-and-what-is-not).
The operator-facing recipe is [kata-dind-workloads.md § Untrusted pull requests](../../docs/operations/kata-dind-workloads.md#untrusted-pull-requests--the-tight-egress-posture).

## The instances

Proxy mode takes exactly one upstream per registry, so the upstream set is the instance set.
It is five because the e2e job's fetches were measured rather than assumed ([the Q408 plan's §2.2 and §2.5](../../docs/plan/q408-untrusted-pr-egress.md#22-the-inventory)).

| Service (port 5000) | Upstream | What rides it |
|---|---|---|
| `mirror-docker-io` | `registry-1.docker.io` | buildkit, `registry:2`, the kind node image, curl, Vault |
| `mirror-ghcr-io` | `ghcr.io` | the released `gmc` image, and helm's OCI chart pull |
| `mirror-quay-io` | `quay.io` | cert-manager; Calico on that lane |
| `mirror-registry-k8s-io` | `registry.k8s.io` | metrics-server |
| `mirror-gcr-io` | `gcr.io` | buildkit's `distroless/static` base, a manifest resolution rather than a layer transfer |

Every instance answers `GET /v2/` with 200 once ready, which is also what the readiness probe reads.
Every instance refuses `GET /v2/_catalog` with 403, which is what stops one tenant reading the list of what another pulled ([below](#closing-the-repository-catalog)).

**Image identity is unaffected.** Schema-2/OCI digests are content addresses over bytes that carry no registry hostname, so a digest-pinned pull re-verifies client-side and a cosign signature still checks out.
The mirror is trusted for tag-to-digest resolution, exactly as the upstream registry already was.

## Storage options

The one thing that varies is where each cache lives, a cost against first-pull latency.

| Variant | Render | Cache across e2e windows | Cost at rest |
|---|---|---|---|
| **Ephemeral** (default) | `kubectl apply -k deploy/registry-mirror` | Discarded, cold on the next window | **$0** |
| **Persistent** | `kubectl apply -k deploy/registry-mirror/overlays/persistent` | Survives, warm on the next window | ~$5/mo (5 × 10 GB Balanced PD, billed while the PVCs exist) |

`scripts/dogfood/e2e-start.sh` applies the ephemeral default and selects the persistent overlay via `REGISTRY_MIRROR_PERSISTENT=1`.
`scripts/dogfood/e2e-stop.sh` scales the instances to zero at the end of the window and leaves everything else standing, so a persistent cache stays warm while the pods cost nothing.

10 GB is the GCE persistent-disk floor rather than a sizing judgement.
The docker.io instance is the one that fills, since `kindest/node` alone is about a gigabyte, while the gcr.io instance holds metadata and nothing else.
Rendering the overlay for only the two busy upstreams is a reasonable way to pay less.

### Reclaiming the disks

The disks bill continuously while the PVCs exist; scaling the pods down does **not** stop it.
To stop paying:

```bash
kubectl delete pvc -n gag-registry-mirror -l app=registry-mirror
```

`standard-rwo` uses the GKE default `Delete` reclaim policy, so removing a PVC deletes the backing disk.
Re-render the persistent overlay to recreate fresh, empty caches.

## What the NetworkPolicies do

Two policies, both additive.
A NetworkPolicy can only add an allowed path, so neither weakens the GMC-managed default-deny that governs worker egress.

- **`e2e-mirror-egress`**, in the tenant namespace, lets workload-labelled pods reach mirror pods on 5000 and names no other destination.
- **`registry-mirror-worker-access`**, in the mirror namespace, makes the tenant namespace on port 5000 the mirror pods' entire accepted ingress.

Neither admits an upstream registry, and that is the point.
A rule that let a worker fall back to the upstream on a cache miss would defeat the enforcement this posture exists to deliver.

The mirror pods' **egress** is deliberately unrestricted.
They carry no `actions-gateway/component=workload` label, so the managed default-deny does not select them and they keep the free egress a pull-through cache needs.
Bounding that by network is not available here: the upstreams are CDN-fronted, GKE Dataplane V2 enforces no FQDN policy on this cluster, and a CIDR allowlist rots.
What bounds it instead is `proxy.remoteurl`, one upstream per instance, fixed in the pod spec.
That is content control rather than host control, and it is why the mirror is stronger than allowlisting the five registries would have been: `docker.io` allowlisted is `docker.io/<anyone>/<anything>`, in both directions.

## Closing the repository catalog

`GET /v2/_catalog` names every repository a mirror has cached, and one manifest fetch is enough to add a repository to that list.
Measured on 2026-08-28 against `registry:3.1.1` at the digest [`base/deployment.yaml`](base/deployment.yaml) pins, run as a pull-through cache with the deployed proxy configuration: cold it answered `{"repositories":[]}`, and after a single fetch of `library/alpine:3.20`, `{"repositories":["library/alpine"]}`.
It is a documented registry API endpoint rather than a side channel, so nothing done to cache timing narrows it.

**Distribution offers no setting that closes it.** 3.1.1 registers the route unconditionally, and its one catalog knob is `catalog.maxentries`, which the config parser raises to a default of 1000 whenever it is set to zero or less — so `REGISTRY_CATALOG_MAXENTRIES=0` still answered 200 with the repository listed, measured on a second instance.
Its only request gate is `auth`, and an access controller applies to every route rather than to one: the mirror serves anonymous pulls to four separately-configured clients ([below](#how-the-job-is-wired-to-these-instances)), so credentials would have to reach all four, and a client that missed them would stop pulling rather than lose the catalog.

So each pod fronts its registry with a deny proxy instead.
[`base/catalog-deny.cfg`](base/catalog-deny.cfg) refuses that one path with 403 and forwards everything else; the registry binds `127.0.0.1:5002`, so the proxy is the only way onto it.
The registry container keeps its probes on 5000, which now traverse the proxy, and that is what makes the failure closed: a proxy that does not start leaves the pod NotReady and its Service without an endpoint, rather than a pod that serves the catalog because its deny did not load.
The proxy carries the same probes for the other half of that: a proxy that hangs rather than exits would otherwise restart the registry behind it indefinitely, while the container that is actually broken is never probed.

Measured on 2026-08-29 against both pinned images and the config exactly as `kustomize` renders it — 403 for `/v2/_catalog`, for `?n=100`, for a trailing slash, for `/v2/%5Fcatalog` and `/v2%2F_catalog`, and for the redirect `/v2/./_catalog` lands on; 200 for `GET /v2/`, a real manifest and a tags list; 405 for an upload, unchanged; and a full `docker pull` of `library/alpine:3.20` through the proxy.
The percent-encoded forms are why the rule reads the path through `url_dec`: Go decodes escapes before the route is matched, so an unfronted registry answered `/v2/%5Fcatalog` 200 with the repository listed, and a deny written against the raw path would pass it straight through.

Two checks hold it.
`make registry-mirror-catalog-deny-check` reconciles the six files the posture is spread across — an instance with no deny container, a registry back on the pod network, or a port that stopped agreeing all fail there — and the `catalog` check in [`e2e-mirror-validate.sh`](../../scripts/dogfood/e2e-mirror-validate.sh) grades it on the cluster beside the manifest fetch, so a deny that also broke pulls cannot pass on the catalog reading alone.

## Two topologies: one shared set, or one set per tenant

What this tree renders is **one mirror set serving one tenant**.
`base/networkpolicy.yaml` names the tenant namespace twice: as `e2e-mirror-egress`'s own `metadata.namespace`, and as the mirror-side ingress policy's `kubernetes.io/metadata.name` peer.
On a cluster with one tenant that is the whole story, and the dogfood cluster has one.
On a cluster with several it is a fork, and the platform administrator owns which way to take it, because it trades disk and upstream bandwidth against what one tenant can learn about another.
The decision itself, with the tenant count and the tenant relationship that settle it, is on the operator page: [kata-dind-workloads.md § Choosing a mirror topology](../../docs/operations/kata-dind-workloads.md#choosing-a-mirror-topology).

| Topology | Render | Mirror sets | What one tenant learns about another |
|---|---|---|---|
| **Shared** | `kubectl apply -k deploy/registry-mirror/overlays/shared-tenants` | one, admitting every managed tenant | whether a repository is already warm in the cache, by timing a blob fetch, [below](#what-a-shared-cache-exposes) |
| **Isolated** (what the default renders) | `kubectl apply -k deploy/registry-mirror`, retargeted once per tenant | one per tenant | nothing through the mirror |

Storage is the other axis and composes with either.
The topology itself lives in [`components/shared-tenants/`](components/shared-tenants/kustomization.yaml), so `overlays/shared-tenants` is it over the ephemeral base and [`overlays/shared-tenants-persistent`](overlays/shared-tenants-persistent/kustomization.yaml) is it over the PVC-backed one; the patch is written once and the two cannot drift.
Under the shared topology the persistent cost does not multiply: one set means five PVCs whatever the tenant count.

Only the mirror-side ingress differs between them.
The worker-side `e2e-mirror-egress` policy lives in the tenant's own namespace, so each tenant needs its own copy under both.

### The shared set

[`components/shared-tenants/`](components/shared-tenants/kustomization.yaml) swaps the mirror-side ingress peer for the GMC's managed-tenant marker:

```yaml
- namespaceSelector:
    matchLabels:
      actions-gateway.com/tenant: managed
```

That marker is what the GMC's own confinement ValidatingAdmissionPolicies key on ([`api/v2beta1/shared_types.go`](../../api/v2beta1/shared_types.go)), so the mirror reads the same label a platform admin already sets rather than a list of its own.
It is not the same tenant set as those policies while v1 and v2 coexist: they accept the v2 marker **or** the legacy `actions-gateway.github.com/tenant: "true"`, and this accepts only v2, so the mirror's set is a strict subset of theirs until the window closes.
It also admits tenants created later, automatically: a namespace becomes a mirror client the moment the administrator marks it.
That is the point on a cluster whose tenant set churns and the risk on one where it must not, so the overlay's header carries the `matchExpressions` form that admits a fixed subset instead.
A namespace still on the v1alpha1 marker (`actions-gateway.github.com/tenant: "true"`) matches nothing here and its pulls stop, which is the fail-closed direction; migrate the namespace rather than widening the selector.

Each tenant still needs its own worker-side rule.
Copy the `e2e-mirror-egress` document out of [`base/networkpolicy.yaml`](base/networkpolicy.yaml) once per tenant and change `metadata.namespace`; it names no other tenant-specific value.

### Isolated sets

Render this tree once per tenant, giving each its own mirror namespace and retargeting the two namespace values by hand, as [Adopting this outside the dogfood cluster](#adopting-this-outside-the-dogfood-cluster) sets out.

**Do not reach for kustomize's `namespace:` field to do it.** It renders a broken set at exit 0.
Measured on kubectl 1.36.3 / kustomize v5.8.1, an overlay setting `namespace: gag-registry-mirror-tenant-a` over `base`:

- moves `e2e-mirror-egress` into the *mirror* namespace, where it selects no worker, so that tenant gets no egress rule at all;
- leaves both policies' `kubernetes.io/metadata.name` peers naming `gag-registry-mirror` and `gag-dogfood-e2e`, because a namespace named in a label *value* is not a field any transformer rewrites.

The result applies cleanly and the tenant cannot pull anything.

### What a shared cache exposes

The repository list is closed under both topologies ([above](#closing-the-repository-catalog)).
What a shared set still exposes is timing: whether a repository is already in the cache, which says that some other tenant pulled it.

Measured on 2026-08-28 against `registry:3.1.1` at the digest [`base/deployment.yaml`](base/deployment.yaml) pins, run as a pull-through cache with the same proxy configuration, on a laptop rather than on the cluster:

- **Blob timing separates a hit from a miss by an order of magnitude, on two cold samples.** First fetch of `library/alpine`'s layer took 637 ms; the three after it took 13, 11 and 10 ms. `library/busybox` gave 419 ms, then 61, 44 and 70 ms. The miss arm is those two fetches, against the manifest arm's ten, so read the separation rather than the range.
- **Manifest timing does not.** Ten distinct repositories fetched cold and then warm gave medians near 430 ms and 360 ms, with the distributions overlapping (a warm 445 ms against a cold 407 ms).
  A pull-through cache revalidates a manifest upstream on every request, so a manifest hit still pays a round trip.

That was taken from a laptop over the public internet, not from inside a Kata guest across the bridge NAT, which is where an attacker actually sits, so the figures bound the channel rather than measure it.
[Q1020](../../docs/queue/Q1020.md) holds the guest measurement, and it is now the whole of what a shared set gives away rather than the smaller half of it.

## Adopting this outside the dogfood cluster

Three values are specific to this cluster, all of them in [`base/networkpolicy.yaml`](base/networkpolicy.yaml), [`base/namespace.yaml`](base/namespace.yaml) and the persistent overlay:

- the tenant namespace `gag-dogfood-e2e`, named twice: as the egress policy's namespace, and as the ingress policy's `kubernetes.io/metadata.name` peer;
- the mirror namespace `gag-registry-mirror`, named in every object;
- `standard-rwo`, the GKE storage class, in the PVCs.

The workload label selector is not cluster-specific: `actions-gateway/component=workload` is what the gateway puts on every worker pod.

## How the job is wired to these instances

The clients live in the e2e job, and no two of them read the same configuration, so the wiring is in four places.
The tenant's [`registry-mirror-wiring` ConfigMap](../dogfood-e2e/overlays/kata/mirror-wiring.yaml) holds the endpoint set once, in the two forms those clients can read:

| Client | Reads | Covers |
|---|---|---|
| the inner `dockerd` | `daemon.json`, mounted into the dind sidecar | every Docker Hub pull, digest-pinned ones included, which no ref rewrite can restore a local name for |
| `docker pull` of a non-Hub ref | the `<upstream>=<mirror>` map, via [`scripts/lib/registry-mirror.sh`](../../scripts/lib/registry-mirror.sh) | the cert-manager, metrics-server, Calico and released-GMC pulls |
| helm's OCI client | the same map, rewriting the chart ref and adding `--plain-http` | the released chart the upgrade gate installs |
| buildkit | a generated `buildkitd.toml` off the same map | the `Dockerfile`'s base images, which no cache and no `kind load` covers |

`dockerd` mirrors Docker Hub and only Docker Hub, which is why the other four upstreams need the ref rewrite rather than an entry in `daemon.json`.
The rewrite re-tags each image to the ref the caller asked for, so everything downstream is untouched: `docker save`, `kind load`, and the manifests kubelet resolves.

Adopting the wiring elsewhere means the ConfigMap and the two patches in [the Kata overlay](../dogfood-e2e/overlays/kata/kustomization.yaml) that mount it, on top of the three cluster-specific values above.
`make registry-mirror-wiring-check` reconciles that ConfigMap with the instances here, in both directions.

## What is proven, and what is not

The instances **serve**: 25 of 25 checks on the dogfood cluster on 2026-08-28, five per instance, by [`scripts/dogfood/e2e-mirror-validate.sh`](../../scripts/dogfood/e2e-mirror-validate.sh): Available, `/v2/`, a real upstream manifest, an upload refused with 405, and the bundled image's `:5001` debug listener unbound.
That battery has since gained a sixth check per instance, that `/v2/_catalog` is refused, which has not run on the cluster yet; the deny's own reading is the local one [above](#closing-the-repository-catalog).

The wiring above **rides them** when [`scripts/dogfood/e2e-mirror-hits.sh`](../../scripts/dogfood/e2e-mirror-hits.sh) reports a served content request per instance after a Kata e2e run.
That reading was needed because the tenant still carried an allow-all `e2e-open-egress` policy while the wiring was being proven: a client that ignored its wiring reached its upstream and the run was green either way, and the access log is the one place an unmirrored pull cannot appear.

The posture is **enforced**: on the Kata lane a worker's registry path is these instances or nothing, measured on the dogfood cluster on 2026-08-28.
The tenant's live rules carry zero allow-all, and [`scripts/e2e/egress-negatives.sh`](../../scripts/e2e/egress-negatives.sh) passed all eight of its checks inside a green Kata run: three destinations that answered nothing (a mirrored upstream reached by its own hostname, the plain internet, and the metadata server), each against a control that did answer, so the silence is the policy rather than a worker with no network.
It runs on every run of the lane rather than once, because a policy that stops selecting the worker is invisible in a green suite.
The dind overlay keeps its open-egress policy: it is the explicit trusted-CI fallback, and privileged DinD was never a candidate for untrusted code.
