# Node-Layer P2P Image Distribution — Q540

> **Status (2026-10-13): ❌ Graded, not built.** Two candidates for peer-to-peer (P2P) pod-image distribution at the node layer, graded on kind against one question: what can a pod with no egress policy do at a node's address?
> Dragonfly's node client exposes unauthenticated task control on every node address ([Q539 plan §8.2](q539-dragonfly-mirror-backend.md#82-measured-the-proxy-binds-to-loopback-and-five-ports-stay-on-every-address)); Spegel exposes read access to the node's cached images and nothing else, and runs on the pod network, where NetworkPolicy can narrow even that ([§2](#2-spegel-graded)).
> Recommendation: build Q540's node layer on Spegel ([§4](#4-recommendation)).

[Q540](../queue/Q540.md) is the composed stack: P2P distribution of pod images at the node, the in-cluster pull-through mirrors ([q408-untrusted-pr-egress.md](q408-untrusted-pr-egress.md)) for pulls inside a job.
On dogfood, a Kata worker under the Gateway Manager Controller's (GMC's) workload egress policy reaches no node address on a non-allowlisted port ([Q539 plan §8.1](q539-dragonfly-mirror-backend.md#81-measured-the-worker-policy-holds-and-nothing-else-does)).
Every other pod without an egress policy does, so a node-layer component is graded on what it offers that pod.

## 1. Dragonfly, summarised

Measured on kind with chart 1.8.5 and client v1.5.7 in Q539 plan §8.2: the forward proxy binds to loopback from chart values and containerd pulls through it, but upload gRPC, metrics, health, stats and piece storage stay on `0.0.0.0`.
The upload service includes task download and delete calls with no authentication; that is read from the v1.5.7 source and not probed.
Closing it means peer authentication, such as mutual TLS on the ports nodes must share, which is unbuilt and ungraded.

## 2. Spegel, graded

Spegel `v0.7.4` (image `ghcr.io/spegel-org/spegel@sha256:26c60b05e08a…`, chart `0.7.4` at `sha256:eaf39cc0967a…`, source at commit `79779e2683ef`), installed with chart defaults except the image digest, on a three-node kind cluster running Calico.

**Shape.** A DaemonSet on the pod network, not `hostNetwork`. containerd reaches it through a hostPort: Spegel writes `/etc/containerd/certs.d/_default/hosts.toml` naming `http://<node IP>:30020` and `:30021`.
Peers find each other on a libp2p router at pod port 5001 and fetch from each other at pod port 5000; metrics are on 9090.

**It distributes.** After a node pulled `debian:bookworm-slim`, a second node's pull of the same image counted 4 Spegel cache hits (`spegel_mirror_requests_total{cache="hit"}`), against 6 misses on the first node, whose pull had no peer to serve it.

**It holds the read-only half of the mirror contract.** From a pod with no egress policy, against a node's hostPort, with a canary pod logging every request it receives:

| Probe | Result |
|---|---|
| Canary, fetched directly (control) | 200, canary logged it |
| Cached image's manifest by tag, `?ns=docker.io` | 200 |
| The same tag with `?ns=` naming the canary | 404, canary silent |
| An absent tag, with and without `?ns=` naming the canary | 404, canary silent |
| Absolute-URI `GET` to the canary | 404, canary silent |
| `CONNECT` to the canary | refused (curl `000`) |
| `POST` an upload, `PUT` a manifest, `DELETE` a manifest | 404 each |

That matches the source: the handler mounts only `GET` and `HEAD` under `/v2/` plus two health paths (`pkg/registry/registry.go`), and a miss is fetched from peers at their pod IP and a fixed registry port, never from an upstream registry.

**What it does expose is read access.** Spegel serves any content in the node's store, by tag or digest, with no per-request credential; its only authentication is one optional basic-auth pair for every client.
So a private image one tenant pulled with its own pull secret is readable by any pod that reaches Spegel.
That is read from the source and Spegel's design; the probe above used a public image only.
A rogue libp2p peer could also advertise content, and containerd verifies blobs and digest references but not a tag's resolution, so `resolveTags: true`, the chart default, lets a peer answer a tag lookup.

**NetworkPolicy narrows it, except across nodes.** An ingress policy on the Spegel pods admitting 5000 from node addresses and Spegel pods, and 5001 from Spegel pods alone:

| From a pod with no egress policy | No policy | Policy |
|---|---|---|
| Spegel pod IP, port 5000 | 200 | connect timeout |
| Spegel pod IP, port 5001 | TCP connects | connect timeout |
| Its own node's hostPort 30020 | 200 | connect timeout |
| Another node's hostPort 30020 | 200 | **200** |

The cross-node path stays open because traffic from a pod to another node's address leaves source-NATed to its own node's address, which the policy must admit for containerd.
Peer distribution kept working under the policy: the second node's hits rose from 4 to 12 over a `python:3.12-slim` pull.
Its misses rose from 2 to 25 over the same pull, which these readings do not explain.

**It displaces existing registry config.** On install Spegel moved kind's own `certs.d/127.0.0.1:5000` entry into `certs.d/_backup/`, so a node already carrying per-registry config loses it unless `prependExisting` is set.

## 3. Side by side

| Question | Dragonfly 1.8.5 node client | Spegel 0.7.4 |
|---|---|---|
| Network | `hostNetwork`: no NetworkPolicy can select it | pod network: NetworkPolicy selects it, except the cross-node hostPort path |
| What an unpoliced pod reaches | unauthenticated task download and delete on upload gRPC (source, not probed) | read access to cached images |
| Fetches from a host the request names | proxy, if not bound to loopback (Q539 §2) | no (measured) |
| Accepts writes | proxy forwards pushes, if reachable (Q539 §2) | no (measured) |
| Restarts containerd | the chart does, unless turned off | no; it writes `certs.d`, and kind's containerd used it with no restart |
| Pre-heats ahead of a burst | yes | no: serves only what some node already pulled |

## 4. Recommendation

Build the node layer on Spegel.
Its worst case is read access to cached images, where Dragonfly's is control of a node's download client, and the GMC's workload policy already keeps workers off node addresses (§8.1 of the Q539 plan).
Ship it with the ingress policy of §2 and `resolveTags: false`, and treat the cross-node hostPort path as known exposure for pods outside a GAG tenant.

Three things are unmeasured and gate dogfood:

- Whether GKE's containerd reads `certs.d` per pull, as kind's does; the same question blocked Dragonfly's dfinit.
- What Spegel displaces on a GKE node, and on a node that also runs `kata-deploy`.
- Whether `resolveTags: false` costs the worker image anything: GAG pins worker images by digest, so it should not, but nothing here measured it.

Dragonfly stays the in-guest mirrors' back end (Q539), so "one deployment for both layers" is answered no: the layers want different tools.
