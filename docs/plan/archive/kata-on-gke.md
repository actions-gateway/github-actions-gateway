# Kata Containers on GKE — Secure CI Reference Architecture

GAG's own e2e CI suite uses `kind create cluster` inside a runner pod (Docker-in-Docker).
That means the runner needs a Docker daemon.
The obvious solution — privileged DinD — is unacceptable for an OSS project: external contributors open PRs and CI runs their code, making the runner a direct target for "pwn request" attacks.
GAG must also dogfood its own isolation model.
This plan scopes a spike to validate Kata Containers on GKE as the right path, then build a reusable reference architecture that users with the same requirement (regulated environments, multi-tenant shops, public OSS projects) can follow.

**Status at a glance**

| Phase | Status |
|---|---|
| Spike artifacts — node-pool config, Kata install values, unprivileged runner pod | ✅ Live-validated and corrected against real behaviour |
| Spike — live go/no-go | ✅ **GO** — 5 of 6 acceptance criteria proven end-to-end on a throwaway GKE cluster |
| Reference architecture — [`docs/operations/kata-dind-workloads.md`](../../operations/kata-dind-workloads.md) | ✅ Updated with the validated config + constraints |
| Q286 wiring — `overlays/kata` worker shape, `E2E_VARIANT` knob, `/dev/kmsg` kind config | ✅ Landed — see [CI integration](#ci-integration--the-follow-up). No bundled runner image needed after all: the daemon is a stock `docker:28-dind` native sidecar. |
| AC#5 — `make e2e` green through the Kata runner on dogfood | ✅ **GREEN** 2026-07-17 (e2e-calico run 29549402471: 56 passed / 0 failed / 2 skipped) after root-causing 7 live defects — [what the live session found](#what-the-live-session-found-2026-07-16) |
| Default flip — `E2E_VARIANT=kata` becomes the dogfood default | ✅ Flipped with the AC#5 close-out; `dind` is the explicit opt-in fallback |

**The core claim holds.** On a throwaway GKE cluster (`gag-kata-spike-c2`, GKE `1.35.5-gke.1241004`, Ubuntu 24.04 / containerd 2.1.5, `c2-standard-4` with nested virtualization, Kata 3.32.0 / QEMU), `kind create cluster` completed inside a pod with `runtimeClassName: kata` and **zero** `privileged: true` in its spec.
The full inner control plane (etcd, kube-apiserver, scheduler, controller-manager, CoreDNS, kindnet) reached `Ready`, `kind load docker-image` worked, and a pod scheduled and ran in the inner cluster.

Evidence the boundary is a real VM, not a runc fallback:

| Probe | Value |
|---|---|
| GKE node kernel | `6.8.0-1054-gke` |
| Kata **guest** kernel | `6.18.35` |
| `/dev/kvm` on the node | `crw-rw---- 10, 232` (+ CPU `vmx` flag) |
| `privileged: true` occurrences in pod spec | **0** |
| `allowPrivilegeEscalation` | `false` |
| `seccompProfile` | `RuntimeDefault` |
| `/dev` entries visible in guest | 16 (a privileged container sees the host's full set) |

Measured cost (`c2-standard-4`):

| Metric | Result | AC#6 ceiling |
|---|---|---|
| `kind create cluster` — cold image cache | **58 s** | ≤ ~6 min |
| `kind create cluster` — warm cache | 43 s | — |
| `kind load docker-image` | 2 s | — |
| Kata VM boot overhead vs runc | ~2 s (3 s vs 1 s) | — |

The spike cluster was torn down; nothing persists.

---

## Motivation

Three independent reasons converge on this work:

### 1. OSS "pwn request" threat

GAG is a public OSS project.
Any contributor can open a PR, and GitHub Actions CI runs their code on GAG infrastructure.
Privileged DinD in a runner pod means:

- The pod can write to `/proc` and `/sys` on the host node.
- Node service account tokens are reachable via the GKE metadata server from inside the pod — a direct path to cluster-scoped credentials.
- A compromised runner can pivot to other tenant namespaces if network policy is not perfectly airtight.

> **Correction (validated in the spike).** Kata fixes the *first* bullet and not the second.
> Kata isolates the **kernel**, not the **pod network**.
> Probing from inside the micro-VM, `169.254.169.254` remained reachable and `computeMetadata/v1/instance/service-accounts/default/token` returned **HTTP 200**, i.e. the node's GCE service-account token.
> The micro-VM alone does **not** close the node-credential path.
>
> The control that does close it is **Workload Identity** (`--workload-pool` plus `--workload-metadata=GKE_METADATA` on the node pool).
> Re-probed with it enabled, the metadata server serves the workload-pool identity (`<project>.svc.id.goog`) instead of the node's GCE service account.
> Treat Workload Identity as a hard prerequisite of this architecture, not an optional extra; add `automountServiceAccountToken: false` so the runner carries no Kubernetes API token either.
> A NetworkPolicy denying egress to `169.254.169.254/32` is worthwhile defence in depth.

This is the "pwn request" attack class, actively exploited against OSS projects.
GitHub's mitigations (approval gates for first-time contributors, `pull_request` vs `pull_request_target` scoping) are process controls, not isolation.
They reduce but do not eliminate the risk.

### 2. GAG must dogfood its own security model

GAG's core value proposition is secure multi-tenant runner isolation.
Running GAG's own CI in privileged DinD would mean:

- The project claims secure isolation but does not use it for its own workloads.
- The privileged-DinD path is implicitly endorsed as acceptable for users who need kind inside a runner.

Both undermine the product.
GAG's CI runners should use the same isolation model GAG provides to tenants — or a stricter one.

### 3. Reference architecture for users

Many GAG users have the same requirement: run kind (or Docker builds) inside a self-hosted runner without `privileged: true`.
This includes:

- Regulated environments (FedRAMP, SOC 2, PCI) where privileged containers are prohibited or require compensating controls.
- Multi-tenant clusters where operator policy blocks privileged pods cluster-wide.
- Other OSS projects that want to run their own e2e CI through GAG.

A validated, documented reference architecture turns a one-off internal fix into a reusable deliverable.
It also differentiates GAG from ARC: ARC users typically accept privileged DinD; GAG provides a secure path.

---

## Why not the other options?

**Sysbox** — `nestybox/sysbox#920` (opened March 2025) documents that kind inside Sysbox breaks for K8s v1.25+ node images.
The only kind-specific fix in the Sysbox changelog was v0.5.0 (March 2022, fixing #415). v0.6.1 (April 2023) added K8s 1.24–1.26 support in the `sysbox-deploy-k8s` installer but contains no changelog entry for kind with 1.25+ node images, and issue #920 post-dates it.
Claims that a Sysbox v0.7.0 released in June 2026 resolves this were adversarially checked and refuted (no such release found).
Docker acquired Nestybox in May 2022 and development has slowed sharply.
Contributing a fix would take 4–8 weeks of low-level systems work with uncertain upstream acceptance and indefinite fork-maintenance cost.

**kindbox** — Nestybox's own Sysbox-aware kind replacement is a bash script wrapper explicitly documented as "a reference example, not a replacement for kind."
Last commit: 2021-10-12.
No `kind load docker-image` equivalent.
Calico CNI (which GAG's e2e uses) requires Sysbox-EE (enterprise edition), which was archived in May 2022 at `docker-archive/nestybox.sysbox-ee` after the Docker acquisition and has received no releases since.

**Rootless Docker + rootless kind** — Requires cgroup v2 on the host node and four iptables kernel modules pre-loaded: `ip_tables`, `iptable_nat`, `ip6_tables`, `ip6table_nat`.
Doable on GKE COS nodes but requires a privileged DaemonSet to `modprobe` the modules — the runner pod stays unprivileged but the setup requires node surgery.
Lower isolation gain than Kata (shared kernel vs. micro-VM).

**Kata Containers** — Runs each pod inside a lightweight VM via an OCI-compatible `RuntimeClass`.
The pod itself requires no `privileged: true`; isolation is enforced at the hypervisor layer.
Inside the Kata VM, Docker and kind run natively with no DinD tricks.
This is the strongest *container-escape* boundary available on GKE.

It is not, on its own, a security posture.
Kata bounds the kernel, not the pod network: the node's metadata server stays reachable (measured — see [Motivation](#1-oss-pwn-request-threat)), so Workload Identity is a prerequisite.
The capabilities an unprivileged DinD runner still needs inside the guest (`SYS_ADMIN`, `NET_ADMIN`, `SYS_PTRACE`, `SYS_CHROOT`, rw `/proc/sys` and `/sys/fs/cgroup`) approach `privileged` on an ordinary runtime, so the guarantee rests entirely on the VM boundary rather than on defence in depth.
And it trades a large, well-trodden kernel-CVE surface for a smaller, more exotic hypervisor one — much harder to exploit, not impossible, and on GKE the hypervisor is itself nested inside a GCE VM.
Q226 verified the boundary exists (guest kernel ≠ node kernel, no host privilege); it did not attempt a breakout.
The full accounting is in [What Kata does not buy you](../../operations/kata-dind-workloads.md#what-kata-does-not-buy-you).

> **Common confusion:** GKE's nested-virtualization documentation mentions `securityContext.privileged: true` in some contexts.
> That requirement applies to pods that interact *directly* with the nested hypervisor (e.g. launching their own VMs).
> A pod that uses `runtimeClassName: kata` does not do this — the Kata shim handles VM lifecycle outside the pod.
> The runner pod runs without any privileged context.

---

## Technical approach

GKE nodes are themselves VMs (on GCE).
To run VMs inside them (as Kata requires), GKE must be configured with nested virtualization on the node pool.
This is a node-level config — the runner pod does not need `privileged: true`.

The setup has three layers:

```
GKE node (GCE VM, nested-virt enabled)
  └── kubelet hands pod to kata-containerd shim
       └── Kata micro-VM (QEMU or Cloud Hypervisor)
            └── runner container
                 └── dockerd (not DinD — no special flags)
                      └── kind cluster (kind node containers)
                           └── GAG e2e tests
```

Key properties:
- Runner pod: `runtimeClassName: kata`, no `privileged`, no `allowPrivilegeEscalation`
- Docker daemon inside the runner: standard `dockerd`, no `--insecure` or `--privileged`
- kind node containers: standard `kindest/node` images, no Sysbox patches needed
- Node-level: nested virtualization enabled in the GKE node pool config

**Why kind-in-runner rather than a shared test cluster.** An alternative would run the e2e suite against a pre-provisioned GKE cluster rather than spinning up kind inside each runner pod.
This eliminates the Docker-in-runner requirement but breaks parallel PR testing: CRDs, webhooks, and ClusterRoles are cluster-scoped, so concurrent runs collide unless each gets a fully isolated API server (e.g. via vcluster). kind-in-runner avoids this entirely — each CI run gets its own cluster, and any number of PRs can run simultaneously without coordination.
For a project developed with multiple concurrent sessions this parallelism property is load-bearing.

**Why this can't be validated locally on a Mac.** Kata boots a KVM guest per pod, so it needs `/dev/kvm` — i.e. nested virtualization — on the node.
On a Mac, containers already run inside a Linux VM (Docker Desktop's LinuxKit VM, Colima, …), so Kata would need that outer VM to expose *nested* virt to its guest and the container tooling to pass `/dev/kvm` through to the kind node-container.
Every link in that chain has to cooperate, and most Macs break it:

- **Apple Silicon M1 / M2** — no nested virt at the silicon/framework level.
  Not possible.
- **Apple Silicon M3+ (macOS 15 Sequoia+)** — Apple's `Virtualization.framework` added nested virt here, so the host/OS link finally exists, but (a) the container tooling does not generally wire `/dev/kvm` through to workloads, and (b) the whole GAG e2e stack is x86_64 while the machine is arm64 (emulation, or nothing).
  Worth re-checking against current Docker Desktop / Colima release notes before ruling out a specific M3/M4 box.
- **Intel Macs** — macOS's `Hypervisor.framework` does not expose nested VT-x into the Linux guest, so `/dev/kvm` inside the Docker VM does not function.

This is why the local test tier ([`docs/development/testing.md`](../../development/testing.md)) routes the *other* sandbox runtime — gVisor — to `minikube` (its systrap platform needs no KVM), and keeps Kata on GKE nested-virt or bare metal.
A genuinely local Kata loop needs a Linux host with `/dev/kvm` (bare metal, or a cloud VM with nested virt) — not a Mac.

---

## Spike acceptance criteria — results

| # | Criterion | Result |
|---|---|---|
| 1 | Nested-virt node pool + Kata installed, documented steps | ✅ `/dev/kvm` present (`10, 232`), CPU `vmx` |
| 2 | `runtimeClassName: kata`, no privileged context, `dockerd` runs inside | ✅ dockerd on `overlay2` |
| 3 | `kind create cluster` (same `kindest/node` digest as CI) completes inside the pod | ✅ inner control plane `Ready` |
| 4 | `kind load docker-image` loads an image | ✅ 2 s |
| 5 | GAG e2e suite (`make e2e`) passes inside the runner pod | ⏸ **not run** — no GAG e2e runner image exists yet |
| 6 | `kind create cluster` ≤ 3× baseline (~2 min → ceiling ~6 min) | ✅ 58 s cold |

AC#5 is deferred, not failed: it requires an image bundling `dockerd` + `kind` + the Go/test toolchain + the repo checkout, which GAG does not publish today.
Everything that criterion would exercise beneath the suite — Docker, `kind`, image loading, pod scheduling in the inner cluster — is proven by AC#2/#3/#4.
Building that image and running the suite is part of the CI-integration follow-up.

**Verdict: GO.**

---

## Exact validated configuration

### 1. Node pool

`--enable-nested-virtualization` is a **GA** flag, available on both `gcloud container clusters create` and `gcloud container node-pools create`.
It requires `UBUNTU_CONTAINERD`, or `COS_CONTAINERD` at `1.28.4-gke.1083000`+.
Autopilot cannot do it.

```bash
gcloud container clusters create <cluster> \
  --zone <zone> --machine-type c2-standard-4 \
  --image-type UBUNTU_CONTAINERD \
  --enable-nested-virtualization \
  --node-labels gag.dev/kata-ci=true \
  --workload-pool <project>.svc.id.goog        # metadata-server hardening; see Motivation
```

Verify before going further — the flag asserts intent, `/dev/kvm` is the proof:

```bash
kubectl debug node/<node> -it --image=busybox -- ls -l /host/dev/kvm   # expect: crw-rw---- 10, 232
```

### 2. Kata install

Upstream **no longer ships raw `kata-deploy.yaml` / `kata-rbac.yaml`**; those release-asset URLs now 404.
The canonical installer is an OCI Helm chart:

```bash
helm install kata-deploy \
  oci://quay.io/kata-containers/kata-deploy-charts/kata-deploy \
  --version 3.32.0 -n kube-system -f deploy/kata-ci/kata-values.yaml --wait
```

Values live in [`deploy/kata-ci/kata-values.yaml`](../../../deploy/kata-ci/kata-values.yaml).
The chart creates the `kata-qemu` RuntimeClass (with `overhead: 160Mi / 250m`); we ship only the `kata` alias in-repo.

**Do not conflate the two node labels.** `kata-deploy` must be *selected* by the pool's own label (`gag.dev/kata-ci`), and it *sets* `katacontainers.io/kata-runtime=true` once the runtime is installed — which is what the RuntimeClass schedules on.
Using one label for both lets a Kata pod land on a node whose runtime does not exist yet.

### 3. The runner pod — six non-obvious requirements

Everything below was discovered by running it.
See [`deploy/kata-ci/runner-pod.yaml`](../../../deploy/kata-ci/runner-pod.yaml).

1. **`/var/lib/docker` must be a raw block volume, not an `emptyDir`.** Kata surfaces an `emptyDir` as **virtiofs**, on which Docker cannot use `overlay2`; it silently falls back to `vfs`. kind then switches its node snapshotter to `fuse-overlayfs`, which needs `/dev/fuse` — absent from the guest — and the inner kubelet dies with `failed to create kubelet: open /dev/kmsg` / snapshotter errors.
   Use `volumeMode: Block` + `volumeDevices`, then `mkfs.ext4` inside the guest. *Gotcha:* `docker:dind` declares `VOLUME /var/lib/docker` and Kata pre-mounts virtiofs there, so a naive "is it mounted?" check passes and skips your ext4 mount.
   Test for the **device**, not the mountpoint.
2. **`/dev/kmsg` does not exist in the Kata guest**, and the inner kubelet hard-requires it (`open /dev/kmsg: no such file or directory`). `mknod /dev/kmsg c 1 11` (needs `CAP_MKNOD`), then bind it into kind's node container via `extraMounts`.
3. **`/sys/fs/cgroup` is mounted read-only** for a non-privileged container, so `runc` cannot create the kind node's cgroup. `mount -o remount,rw /sys/fs/cgroup` succeeds with `CAP_SYS_ADMIN`.
   Under Kata this hierarchy belongs to the **guest** kernel, so the remount grants nothing on the host — under plain runc the same tree is the host's, which is precisely why classic DinD demands `privileged: true`.
4. **`/proc/sys` is read-only** likewise; Docker writes per-veth `net.ipv6.conf.<iface>.disable_ipv6`.
   Same remount, same guest-only reasoning.
   (`net.ipv4.ip_forward` is already `1` in the guest, so Docker never writes it.)
5. **cgroup v2 nesting.** The cgroup-namespace root holds our shell + `dockerd`, and cgroup v2 forbids a cgroup from holding processes *and* delegating controllers to children — so systemd inside kind's node cannot create `/init.scope` (`Failed to create /init.scope control group: Structure needs cleaning`).
   Evacuate the root into a leaf cgroup, then populate `cgroup.subtree_control`. `docker:dind`'s own entrypoint does this; overriding `command:` skips it.
6. **IPv6 is disabled in the guest**, but kind unconditionally creates its Docker network with `--ipv6`.
   Pre-create an IPv4-only bridge network named `kind`; kind reuses it.

### 4. Capabilities

`drop: [ALL]`, then add Docker's default set plus four extras. `privileged: true` is never needed and must never be added.

```
CHOWN DAC_OVERRIDE FSETID FOWNER MKNOD NET_RAW SETGID SETUID
SETFCAP SETPCAP NET_BIND_SERVICE SYS_CHROOT KILL AUDIT_WRITE   # Docker defaults
SYS_ADMIN NET_ADMIN SYS_RESOURCE SYS_PTRACE                    # rootful dockerd + runc
```

Two of these are easy to miss: **`FOWNER`** (image layer unpack `chmod`s files it does not own — `chmod /run/rpcbind: operation not permitted`) and **`SYS_CHROOT`** (runc `setns()` into the container mount namespace — `join container mntns: setns: operation not permitted`).

---

## Constraints and gotchas found

- **Capacity, not quota.** `n2-standard-4` and `n2d-standard-4` were both `ZONE_RESOURCE_POOL_EXHAUSTED` in `us-central1-a` while `N2_CPUS` quota sat at 0/200.
  A plain non-nested-virt `n2` also failed, so nested virt does **not** narrow the capacity pool, and `n2d` is not rejected for lacking AMD SVM — the `n2/n2d/c2/c2d` allowlist in [`scripts/dev/kata-node-pool.sh`](../../../scripts/dev/kata-node-pool.sh) is correct. `c2-standard-4` and `c2d-standard-4` both worked.
  Watch the per-family regional quota: `C2_CPUS` defaulted to **8** on a fresh project.
- **A stockout wedges the cluster.** A failing `CREATE_NODE_POOL` op holds a cluster-level lock (`Cluster is running incompatible operation`) and blocks even deleting the pool, for tens of minutes.
  Prefer creating the nested-virt pool as the cluster's *initial* pool (`clusters create --enable-nested-virtualization`) so a stockout fails fast.
- **GKE preinstalls `gvisor` and `confidential-linked-runner` RuntimeClasses.** They are unrelated to Kata; do not assume a `RuntimeClass` listing means Kata is installed.
- **`kata-deploy` is a DaemonSet, so it self-heals.** Recreating the node pool (e.g. to flip `--workload-metadata`) reinstalls Kata automatically; the RuntimeClass survives.
- The chart's post-delete cleanup Job uses `quay.io/kata-containers/kubectl:latest` — unpinned.
  Note it if supply-chain pinning matters to you.
- Kata VM boot overhead is small (~2 s), but the RuntimeClass `overhead` (160Mi / 250m per pod) is real and must be included when sizing nodes.
- **Deleting the cluster orphans the Block PVC's Persistent Disk.** A `Delete` reclaim policy fires when the *PVC* is deleted, not when the cluster is.
  The 100Gi `pd-balanced` survives cluster teardown and keeps billing — delete the PVC first, then check `gcloud compute disks list`.
- Kata guest kernel `6.18.35` supports `ext4`, `xfs`, `overlay`, `fuse` and `virtiofs`.
  The `overlay2` failure is a *backing-filesystem* limitation (virtiofs), not missing kernel support — which is why a block device fixes it.

---

## Reference architecture deliverable

The spike validates the approach on GKE, but the reference architecture ([`docs/operations/kata-dind-workloads.md`](../../operations/kata-dind-workloads.md)) is provider-agnostic.
It covers three tiers:

**Tier 1 — cloud-hosted (GKE, AKS, EKS).** Nested-virtualization node pool + Kata RuntimeClass.
Variant-specific guidance per provider: machine family requirements (n2/n2d/c2/c2d on GKE), Standard vs. Autopilot trade-offs (Autopilot blocks nested virt), Kata DaemonSet installer vs. managed add-on.
Best fit for teams already cloud-native.

**Tier 2 — bare metal and on-prem.** Kata on real hardware requires no nested virtualization — QEMU or Cloud Hypervisor runs directly.
No machine-family constraints, lower overhead, and the correct path for GPU workloads: PCIe passthrough of NVIDIA or AMD GPUs into the Kata micro-VM works from bare metal.
GKE's GPU machine families (a2, a3, g2) do not support nested virtualization, so GPU + Kata on cloud requires bare-metal or dedicated instances.
<!-- Correction 2026-08-08: a2/a3/g2 DO support nested virtualization; gcloud named all
three as capable on 2026-08-02. The bare-metal conclusion holds for a different reason
(BIOS ACS/IOMMU, no host NVIDIA driver, whole-GPU-per-guest). See
docs/plan/gpu-and-accelerated-ci.md#the-collision-with-the-security-goal. -->
This tier is the reference architecture for teams running GPU CI on owned hardware or cost-sensitive on-prem environments.

**Tier 3 — pragmatic fallback (any provider).** Privileged DinD on a dedicated, locked-down node pool.
Documents compensating controls explicitly: workload-identity scope-down, metadata-server block, network policy, node taint isolation.
For teams where Kata is not feasible but full privilege is also unacceptable.

Each tier covers: pod security context, RuntimeClass or equivalent, node requirements, `ActionsGateway` CR configuration to target the right pool, observed startup overhead, and CI timeout guidance.

---

## CI integration — the follow-up

The spike deliberately stopped at the reference architecture.
The Q286 wiring has since landed in-repo; what follows records what shipped, the one design correction it forced, and the remaining live gate.

### What landed (Q286 wiring)

1. **No bundled runner image after all.** The spike's item "a GAG e2e runner image bundling `dockerd`, `kind`, the Go/test toolchain and the repo checkout" predated the Q231 sidecar split.
   In the dogfood e2e shape the *daemon* is a native-sidecar `docker:28-dind` and the *toolchain* rides the existing `dogfood-e2e-runner` client image (docker CLI + buildx + helm + kubectl + jq); the workflow installs `kind` and Go itself.
   The six Kata setup steps live in the sidecar's entrypoint (`args:` block of [`overlays/kata/resources.yaml`](../../../deploy/dogfood-e2e/overlays/kata/resources.yaml), adapted from the spike's [`runner-pod.yaml`](../../../deploy/kata-ci/runner-pod.yaml)), with two sidecar-specific deltas: `dockerd` listens on tcp for the runner container, and the per-worker block device is a **generic ephemeral volume** (PVC created and deleted with the pod — no orphaned Persistent Disks).
2. **kind config passes `/dev/kmsg`** into every node via `extraMounts` (`test/kind-config-{1,2}worker.yaml`) — harmless where the device already exists, required in the Kata guest.
3. **The variant knob**: `E2E_VARIANT=kata scripts/dogfood/e2e-start.sh` applies `overlays/kata`; `dind` stays the default until the flip.
   [`e2e-setup.sh`](../../../scripts/dogfood/e2e-setup.sh) now owns only cluster infra (pool, Kata install, RuntimeClass, namespace, Secret) — the tenant objects it used to apply directly (an outdated pre-spike Kata RunnerTemplate) are overlay-owned.

### The design correction: the namespace cannot stay PSA-baseline

Earlier revisions of this plan (and the overlay stub) claimed the Kata variant keeps the namespace at `baseline` with no privileged profile.
That is wrong: the unprivileged `dockerd`'s validated capability set (`SYS_ADMIN`, `NET_ADMIN`, `SYS_RESOURCE`, `SYS_PTRACE`, `NET_RAW`) exceeds PSS baseline's fixed `capabilities.add` allowlist, and PSA is not Kata-aware — it cannot credit the VM boundary. **Verified against a real apiserver** (envtest, 2026-07-16): a namespace with `enforce=baseline` rejects the Kata worker pod as Forbidden; `enforce=privileged` admits it.
So the kata overlay carries the same four privileged-namespace gates as dind, and the "no privileged container" property is enforced by the worker shape being a **platform-owned `ClusterRunnerTemplate`** (v2 refuses tenant-authored privileged shapes; tenants cannot edit cluster-scoped templates), not by the PSA level.
A second Kata-specific delta: **CPU limits are load-bearing** in the kata overlay — Kata sizes the guest VM's vCPUs from them — so the dind overlay's "requests-only CPU" idiom does not port.

### Live-validation checklist (the remaining Q286 gate)

**Executed 2026-07-16/17 — all steps complete; AC#5 green** (run 29549402471: 56 passed / 0 failed / 2 skipped, ~18.5 min — dind parity).
Step 2's stale RunnerTemplate turned out not to exist; every other prediction held.
The defects found on the way are in [what the live session found](#what-the-live-session-found-2026-07-16).

1. **Verify the live e2e pool** actually matches what [`e2e-setup.sh`](../../../scripts/dogfood/e2e-setup.sh) now creates (`c2-standard-8`, `UBUNTU_CONTAINERD`, `--enable-nested-virtualization`, `--workload-metadata=GKE_METADATA`, label `gag.dev/kata-ci=true`).
   History says it won't: Q231 provisioned `n2-standard-4`, docs elsewhere say the live dind pool is `e2-standard-8`, and the script skips creation when a pool named `e2e` exists.
   Expect to delete + recreate the pool (fine for dind too — it doesn't care about nested virt).
   Confirm `/dev/kvm` on a node and that kata-deploy labels it `katacontainers.io/kata-runtime=true`.
2. **Delete the stale namespaced RunnerTemplate `kata`** in `gag-dogfood-e2e` if present (applied by the old `e2e-setup.sh apply_cr`; superseded by the ClusterRunnerTemplate).
3. `E2E_VARIANT=kata scripts/dogfood/e2e-start.sh`, then dispatch an e2e run (`gh workflow run e2e-calico.yml` or re-run a PR's e2e) and watch the worker pod: dind sidecar must log `dockerd up on overlay2`, the suite must go green end-to-end.
   Also confirm `automountServiceAccountToken: false` held and the ephemeral PVC is deleted with the pod (`kubectl get pvc -n gag-dogfood-e2e`, then `gcloud compute disks list` after teardown).
4. **Flip the default** on green: `E2E_VARIANT` default `dind` → `kata` in `e2e-start.sh`, demote dind to explicit opt-in in the docs (secure-by-default rule), update [`deploy/dogfood-e2e/README.md`](../../../deploy/dogfood-e2e/README.md) status and this table, and close Q286.

Note that GAG's per-PR e2e still runs on GitHub-hosted runners unless `GAG_E2E_RUNNER` routes it to GAG, so this is a change of *where* e2e runs as much as *how*.

### What the live session found (2026-07-16)

The checklist's step 1 prediction held (the live pool was `e2-standard-8`/COS, no nested virt — deleted and recreated), and five defects stood between the committed wiring and a working Kata worker, each root-caused live and fixed in-repo:

1. **The cluster had no Workload Identity pool** — `--workload-metadata=GKE_METADATA` is rejected with a 400 without cluster-level `--workload-pool` (the Q226 spike ran on a spike cluster that had it).
   Enabled live; the Part A create command in [gke-dogfood.md](../gke-dogfood.md) now carries `--workload-pool` plus a retrofit note.
2. **`helm --set` typed the pool label as a boolean** — `nodeSelector` values must be strings, so kata-deploy's server-side apply failed. `e2e-setup.sh` now uses `--set-string`.
3. **kata-deploy ships no tolerations** — it could never schedule onto the `dedicated=e2e:NoSchedule` pool. `e2e-setup.sh` now passes a matching `tolerations[0]` value.
4. **Autoscale-from-zero never fired**: the `kata` RuntimeClass schedules on `katacontainers.io/kata-runtime=true`, which kata-deploy applies post-install — but the cluster autoscaler simulates against the pool's *configured* labels only, so no Kata pod could ever trigger the 0→N scale-up. `e2e-setup.sh` now bakes the runtime label into the pool's `--node-labels` (the same pattern GKE uses for gVisor sandbox pools); the bind-before-install window resolves via kubelet sandbox-create retries.
5. **`blkid || mkfs` never formatted the fresh PVC** — `docker:dind`'s `blkid` is busybox blkid, which exits 0 on a blank device, so the mkfs was skipped and the ext4 mount failed `EINVAL` on every fresh ephemeral volume.
   The Q226 spike masked this: its *static* PVC had been formatted once by hand, so the gate always found a filesystem.
   Isolated by cloning the AGC-rendered pod and bisecting against a working debug pod.
   The entrypoint (overlay + `deploy/kata-ci/runner-pod.yaml`) now mounts first and reformats on failure.

One consequence fix: while dind crash-looped, the **runner container still took the job** (a native sidecar only gates main-container start on having *started* once) and failed it with "Cannot connect to the Docker daemon".
The dind sidecar now carries a `startupProbe` on :2375 so the runner cannot start — and cannot register — before dockerd is actually up.

With the wiring fixed, the first full suite run surfaced two more Kata-specific defects in the worker shape itself:

6. **The dind-derived resource split starved the guest** (53/2, 25 min vs dind's ~18): under Kata the limits are the guest's whole world — vCPUs from CPU limits, guest RAM including page cache from memory limits — and the entire kind cluster lives inside the dind container's slice, which had 3 of 8 vCPUs and 4Gi. calico-node exec probes timed out (10s) and two specs missed their enforcement/rollout deadlines.
   Rebalanced to an even 4/4 vCPU split with dind at 8Gi — the rerun went 54/1 at dind-parity runtime (~18.5 min).
7. **A nested workload can only gain caps present in the sidecar's bounding set** — the suite's dev-mode test Vault requested `IPC_LOCK`, and nested runc failed with "unable to apply caps: operation not permitted" (invisible under privileged dind, which has every cap).
   Fixed the *right* way in [#658](https://github.com/actions-gateway/github-actions-gateway/pull/658): drop the Vault pod's `IPC_LOCK` request and set `SKIP_SETCAP=true` (a dev-mode Vault holds everything in memory and never mlocks) rather than widen the sidecar's bounding set — keeping the worker's capability floor as tight as possible (secure-by-default).
   The rest of the e2e suite only drops capabilities, so this was the sole nested-cap conflict.

Two operational findings, not blocking: reaped never-started workers leave offline runner records at GitHub that 409-collide re-provisions of the same jobID (Q334), and the single e2-standard-2 system node no longer fits the on-demand e2e AGC alongside the CI AGC + GMC + Athens (Q335; the session ran with the system pool at 2 nodes).

---

## Two variants, one base — and defaulting to Kata

The dogfood e2e deployment and the reference architecture should each carry **both** isolation mechanisms — privileged DinD and Kata — as sibling variants over a shared base, not two forked stacks and not one stack with a runtime toggle.
This is already the committed shape and it answers the "two variants vs. one configurable" question in one move: the variants *are* the configuration surface, selected by a single knob.

### Structure — sibling overlays over a shared base

The dogfood manifests already use this layout ([`deploy/dogfood-e2e/`](../../../deploy/dogfood-e2e/)):

```
deploy/dogfood-e2e/
  base/                 # RunnerSet, namespace, egress policy — mechanism-agnostic
  overlays/dind/        # privileged DinD — explicit opt-in fallback
  overlays/kata/        # Kata micro-VM — the e2e-start.sh default (AC#5 green)
```

The Kata overlay's delta vs. `dind/` is the worker-isolation mechanism *only* (`runtimeClassName: kata`, unprivileged sidecar with the six-step entrypoint, ephemeral block volume).
The namespace gates are identical — see [the design correction](#the-design-correction-the-namespace-cannot-stay-psa-baseline) for why Kata cannot keep the namespace at PSA `baseline`.
Keeping the overlays side by side is deliberate: `diff -r overlays/dind overlays/kata` is exactly the security/complexity tradeoff, reviewable in one place.
Kustomize has no clean conditional, so two thin overlays over one base is both DRY *and* "configurable" — do **not** collapse them into a single overlay with in-manifest toggles.

The selection knob is the overlay name: `E2E_VARIANT=kata|dind` on [`scripts/dogfood/e2e-start.sh`](../../../scripts/dogfood/e2e-start.sh) (default `dind` until the flip).
The cluster-infra half that kustomize can't express (nested-virt node pool, Kata DaemonSet, RuntimeClass) is unconditional in [`scripts/dogfood/e2e-setup.sh`](../../../scripts/dogfood/e2e-setup.sh) — the dind variant simply ignores it.

The reference architecture ([`docs/operations/kata-dind-workloads.md`](../../operations/kata-dind-workloads.md)) mirrors this: it already presents Kata (Tier 1 cloud, Tier 2 bare metal) as primary and privileged DinD (Tier 3) as the documented fallback.
No restructure needed — the dogfood config should track that same ordering.

### The default: Kata, once it reaches parity

Per the project's secure-by-default rule, the more secure option is the default and a regression may only be an explicit opt-in.
So once Kata clears validation, **Kata becomes the dogfood default and privileged DinD becomes the opt-in fallback** — not the reverse.

This flip was *gated*, not immediate.
The gate was one condition — **AC#5, the GAG e2e suite green through the Kata runner** (the [live-validation checklist](#live-validation-checklist-the-remaining-q286-gate) above) — and it **cleared on 2026-07-17**, so the flip is done: `E2E_VARIANT` defaults to `kata` and DinD is the explicit opt-in.

The reasons that keep DinD *available* as a variant after the flip — and keep it the recommended tier for some external users — are environmental, not a knock on Kata:

- **Nested virt is required.** Only `c2/c2d` (and `n2/n2d`) families on GKE, subject to per-family capacity (`ZONE_RESOURCE_POOL_EXHAUSTED`) and the stockout-wedge failure mode; GKE **Autopilot cannot do it at all**.
  Bare metal needs no nested virt but is not everyone's substrate.
- **Per-pod overhead** (RuntimeClass `overhead` 160Mi / 250m) and a small boot cost.
- **Workload Identity is a hard prerequisite** of the secure Kata path (the micro-VM does not close the node-metadata credential path — see [Motivation](#1-oss-pwn-request-threat)).

For GAG's own dogfood, all three are satisfiable, so the default flips to Kata on AC#5 green.
For external users without nested-virt-capable nodes, the reference architecture keeps DinD (Tier 3, with its compensating controls) as the honest fallback — Kata recommended, DinD supported.

**Follow-up work, in order:** (1) ✅ build out `overlays/kata/`; (2) ✅ parameterise `e2e-start.sh` on the variant knob (`E2E_VARIANT`); (3) ✅ run the [live-validation checklist](#live-validation-checklist-the-remaining-q286-gate) — pool recreated, seven defects fixed, `make e2e` green through the Kata runner (AC#5, 2026-07-17); (4) ✅ default flipped to Kata, DinD demoted to explicit opt-in.
Q286 complete.
The residual long-horizon item — the untrusted-PR posture (tight egress + in-cluster pull-through mirror) — is recorded in [appendix G](../../design/appendix-g-future-enhancements.md).
