# Reference architectures: supported profiles

A GitHub Actions Gateway (GAG) deployment is a choice on six axes: worker isolation, egress posture, mirror topology, mirror back end, node-layer peer-to-peer (P2P) image distribution, and the cluster's network plugin (CNI).
Most combinations are nobody's real choice, so this page names the handful of **profiles** this project supports and says how each one was tested.

Every profile is in one of two tiers:

- **Gated every release.** The release gate runs it on the dogfood cluster for every release candidate, and a red e2e matrix stops the release.
  Readings a leg records beside the matrix are not gated; the profile says which.
- **Validated once.** It ran once on the dogfood cluster, and this page records the date, the run and the readings.
  It is re-run on demand, or when something it depends on changes.

The validated-once tier is a compromise on cost: some evidence for a profile without making every release pay for it.
The dogfood cluster costs nothing at rest, since GKE waives the fee for one zonal cluster and its node pools scale to zero between runs, so what it costs is node time, and every gated leg adds node time to every release.
With more cluster budget, from a sponsor for instance, more profiles could join the gate and run in parallel.

A combination in neither tier may well work, and [the last section](#combinations-not-validated) lists what is known about the common ones.

## Gated every release

All three run on the dogfood cluster: Google Kubernetes Engine (GKE) with Dataplane V2, its Cilium-based CNI.
Each is a leg of the release gate, which dispatches the e2e matrix onto GAG runners and fails the release on a red run ([the legs the gate runs](release.md#the-legs-the-gate-runs)).

| Profile | Worker isolation | Egress posture | Mirror topology | Mirror back end | Node-layer P2P | Gate leg |
|---|---|---|---|---|---|---|
| **Untrusted pull requests** | Kata micro-VM | Tight: mirrors, GitHub and DNS only | Per tenant, ephemeral | Distribution | None | `kata` |
| **Untrusted pull requests at scale** | Kata micro-VM | Tight | Per tenant, ephemeral | Dragonfly seed peer | Dragonfly node client, from the next candidate | `dragonfly`, isolation readings not gated |
| **Trusted CI fallback** | Privileged Docker-in-Docker (DinD) | Open | None on the job's path | — | None | `dind` |

### Untrusted pull requests

The posture for running an external contributor's code: the job runs in a Kata micro-VM, and its only network paths are cluster DNS, GitHub, and the in-cluster registry mirrors.
Setup and the reasoning are in [kata-dind-workloads.md § Untrusted pull requests](kata-dind-workloads.md#untrusted-pull-requests--the-tight-egress-posture).

Beyond a green matrix, every run probes the egress policy from inside the job: three destinations must answer nothing while four controls answer ([confirming it holds](kata-dind-workloads.md#confirming-it-holds)).
Nodes need nested virtualization.

### Untrusted pull requests at scale

The same posture with Dragonfly, a P2P image distribution system, in two places: the registry mirrors fetch layer blobs through a Dragonfly seed peer, and a Dragonfly client on each node serves the kubelet's pulls of the worker image.
The mirror half is in [`deploy/registry-mirror/` § Dragonfly back end](../../deploy/registry-mirror/README.md#dragonfly-back-end); the node half is [GAG's Dragonfly node-client recipe](p2p-image-distribution.md#gags-dragonfly-node-client-recipe).

The node client joined the leg on 2026-10-23, after the last release candidate, so no candidate has run it yet: `v1.9.0`'s gate ran the Dragonfly back end without it.
The next candidate's gate is the first to run the profile as the table shows it.

A green matrix proves little here, because the mirrors serve whether or not Dragonfly does.
The leg therefore also records six readings, including whether any blob actually came from a peer and whether a worker can reach the seed peer's open proxy or the node client's upload port.
Those readings are recorded, not gated: a `finding` does not fail the release, and is read before promoting ([the readings](release.md#the-legs-the-gate-runs)).

### Trusted CI fallback

Privileged DinD with open egress, for clusters without nested virtualization and for jobs Kata cannot run.
It is never a candidate for untrusted code.
The registry mirrors are deployed beside it, but the DinD worker carries no mirror wiring, so its pulls go to the upstream registries directly.
The worker shape is the `privileged-dind` entry in the [runner template library](runner-template-library.md).

## Validated once

None yet.

The first candidate is DinD with the Dragonfly back end and node client, for GPU fleets that land on DinD because AMD GPUs do not run under Kata today, which needs the DinD overlay wired to the mirrors before it can run ([Q1193](../queue/Q1193.md)).

An entry here records the profile's six axes, the date, the dogfood run, the readings it took, and what would prompt a re-run.

## Combinations not validated

These compose on paper and have not run end to end as a profile.
Each line says how far the evidence goes, read from the scripts and manifests on 2026-10-23.

- **A persistent mirror cache**, under either topology.
  `overlays/persistent` and `overlays/shared-tenants-persistent` are render-checked on every change to the mirror tree and have not run on the dogfood cluster ([what is proven](../../deploy/registry-mirror/README.md#what-is-proven-and-what-is-not)).
- **One mirror set shared by every tenant.** Its NetworkPolicy peer is driven on every merge that touches the mirror tree, by the Calico e2e lane on a kind cluster.
  Whether real workers on GKE carry the label that peer admits has not yet produced a graded reading ([Q1048](../queue/Q1048.md)).
  The dogfood cluster runs one tenant, so the gated profiles use one set per tenant.
- **The Dragonfly back end with a persistent cache, or with a shared set.** No overlay renders either, and `scripts/dogfood/e2e-start.sh` refuses `E2E_MIRROR_BACKEND=dragonfly` together with `REGISTRY_MIRROR_PERSISTENT=1`.
- **Spegel as the node layer.** Graded on kind against Dragonfly, and not yet a supported recipe ([Q1192](../queue/Q1192.md)).
- **A CNI other than GKE Dataplane V2.** The kindnet and Calico e2e lanes run the controller suite on merges whose diff touches the paths each lane covers.
  They run on GitHub-hosted runners, so they test the controllers rather than any profile's worker or mirror wiring.

**A combination you need that is not on this page** goes on the backlog as a validated-once candidate.
New legs join the release gate only on demand, because each one adds node time to every release.
