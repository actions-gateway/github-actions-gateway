# Node-Layer P2P Image Distribution — Q540

> **Status (2026-10-19): ⚠️ Validated on dogfood, not shipped.** Two candidates for peer-to-peer (P2P) pod-image distribution at the node layer, graded on kind.
> Against the goal Q540 serves, running untrusted pull-request checks on GAG's Kata workers, neither candidate is on the worker's path, so security does not choose between them ([§4](#4-against-the-goal)).
> Chosen 2026-10-19: Dragonfly, for its features and because it is already deployed as the mirrors' back end ([§5](#5-choosing)); validated on dogfood the same day with a Kata worker's reachable set unchanged ([§6](#6-dogfood-validation)).

[Q540](../queue/Q540.md) is the composed stack: P2P distribution of pod images at the node, the in-cluster pull-through mirrors ([q408-untrusted-pr-egress.md](q408-untrusted-pr-egress.md)) for pulls inside a job.
The attacker Q408 designs against is the job: code from an untrusted pull request, running in a Kata worker whose reachable set is cluster DNS, GitHub and the mirrors.
So the security question for a node layer is whether it widens that set.
On dogfood, a Kata worker under the Gateway Manager Controller's (GMC's) workload egress policy reaches no node address on a non-allowlisted port ([Q539 plan §8.1](q539-dragonfly-mirror-backend.md#81-measured-the-worker-policy-holds-and-nothing-else-does)).

§1 and §2 also record what each candidate offers a pod with *no* egress policy.
That is outside this goal, and it matters only to an operator who runs untrusted pods other than GAG workers on the same cluster; the operator guidance is [p2p-image-distribution.md](../operations/p2p-image-distribution.md#operational-notes).

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

## 3. Side by side, for a pod with no egress policy

| Question | Dragonfly 1.8.5 node client | Spegel 0.7.4 |
|---|---|---|
| Network | `hostNetwork`: no NetworkPolicy can select it | pod network: NetworkPolicy selects it, except the cross-node hostPort path |
| What an unpoliced pod reaches | unauthenticated task download and delete on upload gRPC (source, not probed) | read access to cached images |
| Fetches from a host the request names | proxy, if not bound to loopback (Q539 §2) | no (measured) |
| Accepts writes | proxy forwards pushes, if reachable (Q539 §2) | no (measured) |
| Restarts containerd | the chart does, unless turned off | no; it writes `certs.d`, and kind's containerd used it with no restart |
| Pre-heats ahead of a burst | yes | no: serves only what some node already pulled |

## 4. Against the goal

**Neither candidate is reachable from a worker.** The workload policy admits DNS, the egress proxy, GitHub's ranges on 443 and the mirrors; it admits no node address and no Spegel pod IP, so both Dragonfly's node ports and Spegel's ports in §2 are off the worker's path.
That is inferred from the policy for Spegel's pod IPs and measured for node addresses, with a stand-in listener rather than either real component.

**A pull request does not choose what the node layer serves.** The kubelet pulls the worker pod's images, which the operator sets on the RunnerGroup; a job's own containers are pulled inside the guest, through the mirrors.
So tag poisoning by a rogue Spegel peer, or a private image read through Spegel, needs a pod that can reach Spegel, which a worker is not.

**What remains to measure for this goal** is the same reading with the real component installed: a worker's reachable set unchanged, on dogfood, under Kata.

## 5. Choosing

With security neutral against the goal, the choice is operational:

| | Dragonfly | Spegel |
|---|---|---|
| Already deployed here | yes, as the mirrors' back end (Q539); the node client is not | no |
| Warms worker images on nodes ahead of a burst | yes | no: serves only what some node already pulled |
| Distributes more than images | yes, arbitrary files | no |
| Footprint | node DaemonSet plus a scheduler | one DaemonSet |
| containerd restart on install | the chart's default, can be turned off | none on kind |

**Chosen: Dragonfly.** Pre-heating and its wider scope matter most for the workloads with the largest pulls: GPU CI, whose images typically run to several gigabytes (not measured here), is where a cold node costs most and where warming nodes ahead of a burst pays.
It also keeps one P2P system in the cluster rather than two, with one scheduler serving both the node clients and the mirrors' seed peer if that sharing holds up on dogfood.

Model weights and datasets are a different path: a job downloads them inside the guest, where the Q408 posture admits only DNS, GitHub and the mirrors.
Dragonfly reaches them only if they are published as OCI artifacts the mirrors serve, which the mirrors' content-class grading in the [Q539 plan §5](q539-dragonfly-mirror-backend.md#5-per-content-class) does not cover yet.

Spegel stays a candidate for a simpler reference architecture, tracked as its own row.

The three readings that gated dogfood are answered in [§6](#6-dogfood-validation).

## 6. Dogfood validation

Run on 2026-10-19 on the dogfood cluster (GKE `v1.35.8-gke.1626001`, Dataplane V2, Ubuntu nodes with containerd `2.2.7`, Kata from `kata-deploy`), with the Kata e2e tenant up under the Q408 posture.
Chart 1.8.5's node client only (`client:v1.5.7` and `dfinit:v1.5.7`, both by digest), on the e2e pool, proxy bound to `127.0.0.1`, dfinit routing docker.io and ghcr.io, against the Q539 scheduler in `gag-dragonfly`.
Everything was removed afterwards and the e2e nodes scaled away, taking dfinit's edits with them.

**GKE's containerd does not read `certs.d` until restarted.** A fresh e2e node had no `certs.d` and an inline `registry.mirrors."docker.io"` naming `mirror.gcr.io` then Docker Hub. dfinit logged "config_path not set" and rewrote `config.toml` to `config_path = "/etc/containerd/certs.d"`; with its restart off, a pull of `debian:bookworm-slim` went straight to the registry and the node client saw zero requests.
So dfinit's containerd restart has to stay on, its chart default.

**dfinit drops GKE's Docker Hub mirror.** Its rewrite removed the `mirror.gcr.io` endpoint, so Docker Hub pulls fall back to Docker Hub itself, through Dragonfly, rather than to Google's mirror.

**It coexists with `kata-deploy`.** `kata-deploy` adds an `imports` line naming `/opt/kata/containerd/config.d/kata-deploy.toml`, and dfinit's rewrite kept it, on the node that had Kata first and on a fresh node where the two started together.
That fresh node is one sample of the startup order, not a proof that every order works.

**The restart did not disturb running pods.** dfinit's `systemctl restart containerd.service` left every pod on the node, two running Kata pods among them, with its start time unchanged and zero restarts, and a running Kata worker still answered `exec` from its guest kernel.
A Kata pod started afterwards on `ghcr.io/actions-gateway/dogfood-e2e-runner:2.337.0-1` (723 MB) pulled it in 28.5 s through the node client: 15 blob requests by the P2P rule, 5 direct, all to ghcr.io.

**Pod images move between nodes.** The same image on a second e2e node pulled in 36.5 s, and that node's client collected 11 blobs from the first node (11 log lines naming it), with 4 small ones fetched from source.
The second pull was slower than the first; one pair of timings does not say why.

**One scheduler serves both layers.** The host-network clients registered with the Q539 scheduler once an additive policy admitted the node subnet (`10.142.0.0/20`) to its port 8002: on Dataplane V2 an `ipBlock` on the node subnet does match those clients.

**A worker's reachable set is unchanged.** A TCP connect to ports 4000 to 4005 on both e2e nodes' InternalIPs and on `169.254.4.6`, from three pods on the first node:

| Port | runc pod, no egress policy | Kata pod, no `workload` label | Kata worker |
|---|---|---|---|
| 4000, 4002 to 4005 | connects, all three addresses | connects, all three addresses | no connect |
| 4001 (bound to loopback) | no connect | no connect | no connect |
| `mirror-docker-io:5000` (control) | refused by the mirror's ingress policy | 200 | 200 |

UDP 4006 was not tested: a TCP connect cannot test it, and the worker policy admits no UDP beyond DNS.
No full e2e workflow ran with the node client installed; the Kata pods above are the evidence that Kata workloads start and pull through it.
