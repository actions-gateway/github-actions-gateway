# Dragonfly node client

A [Dragonfly](https://d7y.io/) `dfdaemon` on every Kata node, which containerd sends its registry pulls through.
The first node to need a worker image fetches it from the registry, and every other node collects its blobs from that node instead.
It registers with the scheduler in [`../dragonfly-core`](../dragonfly-core/README.md), which it shares with the registry mirrors' seed peer, so apply that first.

What it changes on each node, and what it leaves alone, is measured in the [Q540 plan §6](../../docs/plan/q540-node-layer-p2p.md#6-dogfood-validation) on GKE with containerd 2.2.7 and `kata-deploy`.
Why an operator would want it, and what it exposes, is in [Peer-to-peer image distribution](../../docs/operations/p2p-image-distribution.md#gags-dragonfly-node-client-recipe).

| File | What it holds |
|---|---|
| `namespace.yaml` | `gag-dragonfly-node`, at Pod Security `privileged`: the client needs the host network, the host PID namespace and a privileged init container |
| `client.yaml` | The DaemonSet, its `dfdaemon` config and its `dfinit` config, rendered from chart 1.8.5 and pinned by digest |
| `scheduler-from-nodes.yaml` | A NetworkPolicy in `gag-dragonfly` admitting the node subnet to the scheduler's port 8002 |

**Set three things for your cluster before applying it.** The node selector and toleration in `client.yaml` pick the nodes; here they are dogfood's Kata pool.
The CIDR in `scheduler-from-nodes.yaml` is the node subnet; here it is dogfood's `10.142.0.0/20`.
The registries in `dfinit.yaml` are the ones routed through Dragonfly: `docker.io` and `ghcr.io`.

**Applying it restarts containerd on every node it lands on.** containerd reads the `certs.d` directory `dfinit` points it at only on startup, so the restart has to stay on.
Running pods, Kata ones included, kept running through it.

**On GKE it replaces the node's Docker Hub mirror.** GKE configures `mirror.gcr.io` for `docker.io` inline, and `dfinit` replaced that whole `registry` table with `config_path`.
Docker Hub pulls then go to Docker Hub through Dragonfly, once per cluster rather than once per node, and fall back to Docker Hub directly.
Leaving `docker.io` out of `dfinit.yaml` is unlikely to bring `mirror.gcr.io` back, since the table goes with the switch to `config_path` (measured only with `docker.io` listed); a `certs.d/docker.io/hosts.toml` naming it is the untested way back.

**The peer ports listen on every node address.** Upload gRPC (4000), metrics (4002), health (4003), stats (4004) and piece storage (4005 TCP, 4006 UDP) are unauthenticated, and no NetworkPolicy can select a host-network listener.
A Kata worker under the GMC's workload egress policy reached none of them; a pod with no egress policy reached all of the TCP ones, and so did a DinD worker on 4000, because its tenant's allow-all egress policy adds to the GMC's ([measured once](../../docs/operations/p2p-image-distribution.md#on-privileged-dind)).
The proxy (4001) is bound to loopback, so only the node's own containerd reaches it.

`scripts/dogfood/e2e-start.sh` applies it under `E2E_DRAGONFLY_NODES=1`, which the release gate's Dragonfly leg sets, and `e2e-stop.sh` deletes it.
Deleting it leaves `dfinit`'s containerd edits on any node still up: a pull there finds no proxy, and `hosts.toml` names the registry as the fallback (not measured), until the node is replaced.
