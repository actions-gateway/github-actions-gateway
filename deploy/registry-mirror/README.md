# Registry pull-through cache — in-cluster container-image mirror (dogfood)

Five [CNCF Distribution](https://github.com/distribution/distribution) instances in pull-through cache mode, one per upstream registry the dogfood e2e suite pulls from.
Workers reach the mirror; the mirror reaches the registry.
It is a **cache**: a miss is fetched upstream and re-populated, so correctness never depends on the cache surviving.

This is the container-image sibling of [`deploy/athens/`](../athens/README.md) (Go modules), and the concrete artifact behind the recommendation in [`docs/operations/security-operations.md`](../../docs/operations/security-operations.md#prefer-an-in-cluster-caching-mirror-first).
It exists so the Kata e2e variant can carry untrusted-PR CI: with every registry pull riding the mirror, the workers' allow-all egress policy can be deleted and their reachable set becomes cluster DNS, GitHub, and these five Services.

**It is not yet load-bearing.** Nothing is wired to it, and the workers' allow-all egress policy is still in place.
Both of those land in later phases of Q408; see [what is not wired yet](#what-is-not-wired-yet).

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

## Adopting this outside the dogfood cluster

Three values are specific to this cluster, all of them in [`base/networkpolicy.yaml`](base/networkpolicy.yaml), [`base/namespace.yaml`](base/namespace.yaml) and the persistent overlay:

- the tenant namespace `gag-dogfood-e2e`, named twice: as the egress policy's namespace, and as the ingress policy's `kubernetes.io/metadata.name` peer;
- the mirror namespace `gag-registry-mirror`, named in every object;
- `standard-rwo`, the GKE storage class, in the PVCs.

The workload label selector is not cluster-specific: `actions-gateway/component=workload` is what the gateway puts on every worker pod.

## What is not wired yet

Nothing pulls through these instances today.
The remaining work is tracked as Q408:

- **Wiring.** `dockerd --registry-mirror` for Docker Hub, rewritten refs for the non-Hub pulls, buildkit pointed at the gcr.io instance, and helm's OCI ref pointed at the ghcr.io instance.
  Neither `dockerd --registry-mirror` nor containerd's mirror config redirects helm, which speaks the registry protocol itself.
- **Enforcement.** Deleting the e2e tenant's allow-all `e2e-open-egress` policy, once the wiring is proven from the mirror's access logs.

The manifests here have not been applied to a cluster.
The instance configuration was measured against `registry:3.1.1` locally.
The Kubernetes half (scheduling, probes, the policies, the volume permissions) needs a booked dogfood session, and has to pass before the next phase's wiring can be proven to ride it.
