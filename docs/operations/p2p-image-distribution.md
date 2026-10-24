# Peer-to-peer image distribution: surviving worker image-pull storms at scale

> **Audience:** Platform engineer

**Goal:** for a platform team running GitHub Actions Gateway (GAG) at scale, decide whether to add a peer-to-peer (P2P) container-image mirror — [Spegel](https://github.com/spegel-org/spegel) or [Dragonfly](https://d7y.io/) — to keep the runner image-pull path fast and resilient when many ephemeral worker pods start at once.

> **This is a recommended companion, not a GAG component.** GAG's chart does not install, require, or configure either tool.
> The repository carries one tested recipe, [Dragonfly's node client](#gags-dragonfly-node-client-recipe), which the release gate runs; you apply it yourself.
> A P2P mirror is a cluster-wide, node-level concern that lives below GAG, at the container runtime.
> This page explains *why* it pairs well with GAG's worker model and *how* the two interact; the tools' own documentation is authoritative for installation and tuning.

## Why ephemeral workers cause pull storms

GAG provisions a worker pod **per job, run-to-completion** — the pod is created when a job is acquired and deleted the moment it finishes (see [Architecture § worker lifecycle](../design/02-architecture.md)).
There is no warm pool of long-lived runners holding a cached image.
At scale this produces a distinctive load pattern on whatever registry serves the worker image (by default the upstream `ghcr.io/actions/actions-runner`, which is several hundred megabytes):

- **Every cold node pulls the full image.** The kubelet only has the layers a node has previously pulled.
  A burst of jobs that lands worker pods on nodes without the image cached — a CI rush hour, a fresh node group from the cluster autoscaler, a node-image roll — triggers one registry pull per cold node, concurrently.
- **The registry becomes a bottleneck and a single point of failure (SPOF).** Hundreds of simultaneous pulls of a large image saturate registry bandwidth and connection limits, slow every worker's cold start (delaying job pickup), and can trip registry **rate limits** (GHCR and Docker Hub both throttle).
  If the registry is briefly unreachable, *no* cold node can start a worker until it recovers.
- **Egress cost and air-gap pressure compound it.** Each duplicate pull is duplicate egress (or duplicate load on the private mirror in an [air-gapped install](air-gapped-install.md)).

A P2P mirror breaks this pattern: the **first** node to need an image pulls it from the registry ("back-to-source"); **every other** node pulls the layers from a peer that already has them, over the cluster network.
The registry is hit roughly once per image per cluster instead of once per cold node — it stops being the bottleneck or the SPOF for pull storms.

## Spegel vs Dragonfly — what each is and when to choose it

Both turn the cluster into a P2P content-addressed mirror, but they sit at different points on the simplicity-vs-capability curve.

### Spegel — stateless, uses what containerd already has

[Spegel](https://github.com/spegel-org/spegel) is a **stateless** DaemonSet that exposes each node's **existing containerd content store** as a registry mirror to its peers.
It adds **no separate storage** and never mutates or re-packages images: it advertises the layers a node has already pulled and serves them, byte for byte, to peers that ask for the same digest.

- **How it plugs in:** Spegel registers itself as a containerd **registry mirror** (via `hosts.toml`).
  When the kubelet asks containerd to pull, the request is routed first to the local Spegel, which finds a peer holding the content (discovery is over a libp2p distributed hash table) and streams it from there; only if no peer has it does the pull fall back to the upstream registry.
- **What it does *not* do:** Spegel is **lazy** — it can only serve images some node in the cluster has *already* pulled.
  It does not pre-fetch or pre-heat, and it distributes container images only.
- **Choose Spegel when:** you want a low-operational-overhead, drop-in mirror that smooths pull storms with no new stateful component.
  It is the lighter of the two, though GAG ships and tests a recipe for Dragonfly only; on a cluster shared by tenants with private images, read [what it exposes](#operational-notes) first.
  It requires a containerd-based runtime configured to keep unpacked layers (`discard_unpacked_layers = false`) and to honor mirror config — check Spegel's [compatibility requirements](https://spegel.dev/docs/getting-started/) against your node image first.

### Dragonfly — full P2P distribution with pre-heating

[Dragonfly](https://d7y.io/) (a CNCF project) is a **full P2P file- and image-distribution system**: per-node `dfdaemon` peers, a `manager`, a `scheduler`, and optional `seed peers` that hold a durable cache.
It is a heavier, stateful deployment with more moving parts, and in return offers capabilities Spegel does not.

- **Pre-heating / pre-fetch:** you can warm the worker image across the fleet *before* a known burst, so even the first wave of jobs on cold nodes pulls from peers rather than the registry — directly attacking cold-start latency for a CI rush you can predict.
- **Seed peers and back-to-source control:** a durable seed cache and configurable back-to-source behavior reduce registry hits further and span scenarios Spegel's lazy model cannot.
- **Beyond container images:** Dragonfly also distributes arbitrary files and large artifacts (model weights, datasets), and supports multi-cluster topologies.
- **Choose Dragonfly when:** you operate a very large or multi-cluster fleet, need to **pre-heat** images ahead of predictable bursts, want a durable seed cache, or already use it to distribute other large artifacts.
  Accept the extra operational surface in exchange.

| | **Spegel** | **Dragonfly** |
|---|---|---|
| Model | Stateless mirror over containerd's store | Full P2P with daemons + manager/scheduler |
| Storage | None (reuses containerd content) | Optional durable seed cache |
| Pre-heating | No (lazy — serves what's pulled) | Yes |
| Scope | Container images | Images + arbitrary files |
| Ops overhead | Low (single DaemonSet) | Higher (several components) |
| Best for | Most GAG fleets; drop-in pull-storm relief | Huge / multi-cluster fleets; predictable bursts; artifact distribution |

## How a P2P mirror interacts with GAG's worker image

The interplay is deliberately thin: a P2P mirror operates at the **container runtime / node** layer, transparently to the pod spec.
GAG does not have to change anything about how it stamps the worker image — but two GAG behaviors matter for getting it right.

### `imagePullPolicy`: leave it at the digest default

GAG does **not** set an explicit `imagePullPolicy` on worker containers, so the kubelet applies its default, which is keyed off the image reference:

- The default worker image is **fully digest-pinned** (`ghcr.io/actions/actions-runner:<version>@sha256:…`).
  For a digest (or any non-`:latest` tag) the kubelet defaults to **`IfNotPresent`**.
- `IfNotPresent` is exactly what you want with P2P: the kubelet skips the pull entirely when a node already has the layers, and on a cold node the pull it *does* issue is routed through the containerd mirror — where the P2P layer intercepts it and serves from a peer.
  No pod-spec change, no GAG-side configuration, is needed to benefit.
- **Avoid forcing `Always`.** A floating-tag worker image defaults to `imagePullPolicy: Always`, which makes the kubelet re-check the registry on every pod start.
  P2P still routes that check through the mirror so it does not melt the registry, but `Always` partially defeats local caching and adds latency to every cold start.
  Digest pinning (the secure default) keeps you on `IfNotPresent` — keep it there.

Because P2P intercepts at the mirror, it works the same whether the worker image is the built-in default or a per-`RunnerGroup` `workerImage` override, and whether it is served by GHCR or a private registry.

### Digest pinning and P2P are complementary

GAG's worker images are digest-pinned by design (a bare tag is mutable; see [Security § Supply-Chain Compromise of Worker Image](../design/05-security.md)).
P2P distribution is **content-addressed**, so it reinforces this rather than fighting it:

- A peer serves layers and manifests keyed by their `sha256:` digest.
  Spegel advertises exactly the digest in containerd's content store, so what a peer delivers is **byte-identical** to what the registry would have served for that pinned reference — the digest is the integrity check, and it must match on both sides or the kubelet rejects the content.
- **Confirm the mirror serves digest references, not just tags.** Worker pods request the image *by digest*, so the mirror must resolve manifest-by-digest requests, not only tag lookups.
  Spegel does this natively (it keys on digests); for Dragonfly, verify your registry/mirror configuration handles digest-pinned pulls.
  A mirror that only understands tags will silently miss every GAG worker pull and fall back to the registry — defeating the point.
- Pinning is unaffected by relocation: as in the [air-gapped install](air-gapped-install.md), content-addressed copies keep the same digest, so a P2P mirror in front of a private registry preserves the pin end to end.

### Removing the registry as a bottleneck and SPOF

Putting the two together: with the digest-pinned default (`IfNotPresent`) and a P2P mirror, a pull storm of N cold nodes generates roughly **one** back-to-source registry pull instead of N. The remaining N−1 nodes fetch the pinned digest from peers over the cluster network.
That:

- **Caps registry load** so a CI rush hour or an autoscaler scale-up no longer scales registry egress linearly with the fleet — and keeps you under GHCR / Docker Hub rate limits.
- **Removes the registry as a pull-storm SPOF:** once any node has the layers, a transient registry outage no longer blocks cold-node workers from starting.
  (The registry is still the source of truth for the *first* pull and for new image versions — P2P reduces, not eliminates, the dependency.)
- **Speeds cold starts**, shrinking the gap between job acquisition and the runner coming online.

This composes with an [air-gapped install](air-gapped-install.md): the private mirror remains the back-to-source registry, and P2P keeps load off it the same way it keeps load off GHCR.

## GAG's Dragonfly node-client recipe

[`deploy/dragonfly-node/`](../../deploy/dragonfly-node/README.md) puts a Dragonfly `dfdaemon` on each Kata node, and containerd pulls worker images through it.
It shares the scheduler in [`deploy/dragonfly-core/`](../../deploy/dragonfly-core/README.md) with the registry mirrors' seed peer, so it needs that applied first.
It is validated on GKE with containerd 2.2.7 and Kata from `kata-deploy`, and the release gate's Dragonfly leg runs the e2e matrix with it installed.

**What changes on the node:**

- **containerd restarts once on each node.** `dfinit` switches containerd to per-registry `certs.d` files, which containerd reads only on startup.
  Running pods, Kata ones included, kept running through the restart.
- **`docker.io` and `ghcr.io` pulls go through Dragonfly.** Only blob requests take the peer path; tags and manifests go to the registry through it directly.
- **On GKE, the node's `mirror.gcr.io` mirror for Docker Hub is gone.** Docker Hub pulls reach Docker Hub through Dragonfly, once per cluster rather than once per node, so watch Docker Hub's rate limits if your worker images live there.
- **`kata-deploy`'s containerd config survives.** `dfinit` kept its `imports` line on the nodes tested.

**What it exposes, and to whom:**

- **The peer ports listen on every node address, unauthenticated.** That is upload gRPC on 4000, then metrics, health, stats and piece storage on 4002 to 4006.
  A worker under GAG's workload egress policy reached none of them; a pod with no egress policy of its own reached every TCP one, and so did a DinD worker whose tenant adds an allow-all egress policy ([below](#on-privileged-dind)).
  So on a cluster that also runs untrusted pods outside GAG, give those pods an egress policy before applying this.
- **The proxy is bound to loopback**, so only the node's own containerd can use it, and it never reaches a worker.

**Set three values before applying it:** the node selector and toleration in `client.yaml` (your worker nodes), the CIDR in `scheduler-from-nodes.yaml` (your node subnet), and the registries in the `dfinit` config.

**Not yet measured:** UDP 4006, and more than one fresh-node startup order, where `dfinit` and `kata-deploy` race to edit containerd's config on a new node.

### On privileged DinD

The same recipe runs under the privileged-DinD worker, the fallback for jobs Kata cannot run, such as GPU jobs on hardware Kata does not support.
It was validated once rather than on every release: one dogfood e2e run on 2026-10-24 (UTC), with `E2E_VARIANT=dind E2E_MIRROR_BACKEND=dragonfly E2E_DRAGONFLY_NODES=1`, was green.

What the readings from that run showed:

- **The job's pulls rode the mirrors.** All five instances served it: 161 content requests across 15 repositories, from the inner `dockerd`, the rewritten refs, helm and buildkit.
- **The mirrors fetched through Dragonfly.** The seed peer sent 123 blob requests by its peer-to-peer rule and 374 direct.
- **The node client served the worker pod's own image.** containerd sent 34 requests through it, the runner image's blobs among them, and 17 tasks were served by peers and 14 from the registry.
- **A DinD worker reached the node client's upload port, 4000.** The tenant's allow-all egress policy adds to GAG's workload policy, so it admits node addresses too.
  The seed peer's proxy held: the same worker timed out on it, because the seed's own ingress policy refuses it.

So DinD with this recipe gives every job a path to an unauthenticated gRPC port on its node.
That fits the trusted-CI use DinD is already limited to, and it is one more reason never to run untrusted code there.

## Operational notes

- **Pull Secrets still matter for the first pull.** The node performing the back-to-source pull authenticates to the registry as usual (the worker's `imagePullSecrets`; see [air-gapped install](air-gapped-install.md) for the secure pull-Secret pattern).
  Peer-to-peer transfers happen inside the cluster network; they do not need registry credentials, which is part of why P2P reduces registry-side auth load too.
- **NetworkPolicy.** P2P daemons talk node-to-node on their own ports.
  GAG's isolation boundary is the *tenant* NetworkPolicy on worker and proxy pods, not host-level node traffic, so the two generally do not collide — but if you run a default-deny policy at the node/host layer, allow the P2P tool's ports per its docs.
- **Keep worker pods off Dragonfly's proxy port.** dfdaemon's proxy (port 4001 by default) forwards requests to any host, including pushes, and no setting restricts it, so a worker that can reach it has unrestricted egress (measured on client v1.5.7).
  Admit only the clients you mean to serve, by NetworkPolicy on a pod-network deployment; a `hostNetwork` dfdaemon listens on every node address, where pod-level policy cannot select it.
  GAG's worker egress policy does block a worker from that port on every node, its own included (measured with a Kata worker on GKE Dataplane V2), but any pod without its own egress policy reaches it.
- **Binding dfdaemon's proxy to loopback is not enough on its own.** `proxy.server.ip: 127.0.0.1` keeps the proxy off node addresses, and containerd still pulls through it (measured on kind with chart 1.8.5).
  Upload gRPC, metrics, health, stats and piece storage stay on every node address, and the upload service accepts task download and delete calls with no authentication unless you configure peer TLS (read from the client v1.5.7 source).
  Treat those ports as reachable by any pod without an egress policy.
- **Spegel serves cached images to anything that reaches it.** It serves `GET` and `HEAD` only and never fetches from a host a request names (measured on v0.7.4), but it checks no per-request credential, so an image one tenant pulled with its own pull Secret is readable by any pod that reaches Spegel (read from the source; only public images were probed).
  Spegel runs on the pod network, so an ingress NetworkPolicy can admit only node addresses and other Spegel pods.
  On kind with Calico that closed the pod ports and a pod's path to its own node's hostPort, but not to another node's hostPort, which arrives from a node address.
  Set `resolveTags: false` so a peer cannot answer a tag lookup; containerd verifies digests but not tag resolution.
  Spegel also moves existing per-registry `certs.d` entries to `_backup/` unless `prependExisting` is set.
- **Verify with the upstream guides.** Installation, runtime compatibility (containerd settings, mirror configuration), and tuning are owned by the tools.
  Start at Spegel's [Getting Started](https://spegel.dev/docs/getting-started/) or Dragonfly's [documentation](https://d7y.io/docs/).

## Related

- [Reference architectures](reference-architectures.md): which P2P pairings are supported profiles, and how each was tested.
- [Air-gapped install](air-gapped-install.md) — relocating images to a private registry with digests preserved; P2P sits in front of that registry.
- [In-runner image builds](in-runner-image-builds.md) — the sibling scale/PSA companion for build workloads inside workers.
- [Security § Supply-Chain Compromise of Worker Image](../design/05-security.md) — why worker images are digest-pinned and the `imagePullPolicy` rationale.
- [Tenant onboarding](tenant-onboarding.md) — setting a per-`RunnerGroup` `workerImage` and its pull Secret.
- Upstream: [Spegel](https://github.com/spegel-org/spegel) · [Dragonfly](https://d7y.io/).
