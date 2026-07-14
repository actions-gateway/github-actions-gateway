# Kata Containers on GKE — Secure CI Reference Architecture

GAG's own e2e CI suite uses `kind create cluster` inside a runner pod (Docker-in-Docker). That means the runner needs a Docker daemon. The obvious solution — privileged DinD — is unacceptable for an OSS project: external contributors open PRs and CI runs their code, making the runner a direct target for "pwn request" attacks. GAG must also dogfood its own isolation model. This plan scopes a spike to validate Kata Containers on GKE as the right path, then build a reusable reference architecture that users with the same requirement (regulated environments, multi-tenant shops, public OSS projects) can follow.

**Status at a glance**

| Phase | Status |
|---|---|
| Spike artifacts — node-pool config, Kata install values, unprivileged runner pod | ✅ Live-validated and corrected against real behaviour |
| Spike — live go/no-go | ✅ **GO** — 5 of 6 acceptance criteria proven end-to-end on a throwaway GKE cluster |
| Reference architecture — [`docs/operations/kata-dind-workloads.md`](../operations/kata-dind-workloads.md) | ✅ Updated with the validated config + constraints |
| AC#5 — `make e2e` inside the runner | ⏸ Deferred: needs a GAG e2e runner image that does not exist yet |
| CI integration — replace privileged DinD in `e2e-reusable.yml` | 🔲 Follow-up (see Queue) — deliberately out of scope here |

**The core claim holds.** On a throwaway GKE cluster (`gag-kata-spike-c2`,
GKE `1.35.5-gke.1241004`, Ubuntu 24.04 / containerd 2.1.5, `c2-standard-4` with
nested virtualization, Kata 3.32.0 / QEMU), `kind create cluster` completed inside
a pod with `runtimeClassName: kata` and **zero** `privileged: true` in its spec.
The full inner control plane (etcd, kube-apiserver, scheduler, controller-manager,
CoreDNS, kindnet) reached `Ready`, `kind load docker-image` worked, and a pod
scheduled and ran in the inner cluster.

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

GAG is a public OSS project. Any contributor can open a PR, and GitHub Actions CI runs
their code on GAG infrastructure. Privileged DinD in a runner pod means:

- The pod can write to `/proc` and `/sys` on the host node.
- Node service account tokens are reachable via the GKE metadata server from inside the
  pod — a direct path to cluster-scoped credentials.
- A compromised runner can pivot to other tenant namespaces if network policy is not
  perfectly airtight.

> **Correction (validated in the spike).** Kata fixes the *first* bullet and not the
> second. Kata isolates the **kernel**, not the **pod network**. Probing from inside
> the micro-VM, `169.254.169.254` remained reachable and
> `computeMetadata/v1/instance/service-accounts/default/token` returned **HTTP 200**,
> i.e. the node's GCE service-account token. The micro-VM alone does **not** close the
> node-credential path.
>
> The control that does close it is **Workload Identity** (`--workload-pool` plus
> `--workload-metadata=GKE_METADATA` on the node pool). Re-probed with it enabled, the
> metadata server serves the workload-pool identity (`<project>.svc.id.goog`) instead
> of the node's GCE service account. Treat Workload Identity as a hard prerequisite of
> this architecture, not an optional extra; add `automountServiceAccountToken: false`
> so the runner carries no Kubernetes API token either. A NetworkPolicy denying egress
> to `169.254.169.254/32` is worthwhile defence in depth.

This is the "pwn request" attack class, actively exploited against OSS projects. GitHub's
mitigations (approval gates for first-time contributors, `pull_request` vs
`pull_request_target` scoping) are process controls, not isolation. They reduce but do not
eliminate the risk.

### 2. GAG must dogfood its own security model

GAG's core value proposition is secure multi-tenant runner isolation. Running GAG's own CI
in privileged DinD would mean:

- The project claims secure isolation but does not use it for its own workloads.
- The privileged-DinD path is implicitly endorsed as acceptable for users who need kind
  inside a runner.

Both undermine the product. GAG's CI runners should use the same isolation model GAG
provides to tenants — or a stricter one.

### 3. Reference architecture for users

Many GAG users have the same requirement: run kind (or Docker builds) inside a self-hosted
runner without `privileged: true`. This includes:

- Regulated environments (FedRAMP, SOC 2, PCI) where privileged containers are
  prohibited or require compensating controls.
- Multi-tenant clusters where operator policy blocks privileged pods cluster-wide.
- Other OSS projects that want to run their own e2e CI through GAG.

A validated, documented reference architecture turns a one-off internal fix into a
reusable deliverable. It also differentiates GAG from ARC: ARC users typically accept
privileged DinD; GAG provides a secure path.

---

## Why not the other options?

**Sysbox** — `nestybox/sysbox#920` (opened March 2025) documents that kind inside Sysbox
breaks for K8s v1.25+ node images. The only kind-specific fix in the Sysbox changelog was
v0.5.0 (March 2022, fixing #415). v0.6.1 (April 2023) added K8s 1.24–1.26 support in the
`sysbox-deploy-k8s` installer but contains no changelog entry for kind with 1.25+ node
images, and issue #920 post-dates it. Claims that a Sysbox v0.7.0 released in June 2026
resolves this were adversarially checked and refuted (no such release found). Docker
acquired Nestybox in May 2022 and development has slowed sharply. Contributing a fix would
take 4–8 weeks of low-level systems work with uncertain upstream acceptance and indefinite
fork-maintenance cost.

**kindbox** — Nestybox's own Sysbox-aware kind replacement is a bash script wrapper
explicitly documented as "a reference example, not a replacement for kind." Last commit:
2021-10-12. No `kind load docker-image` equivalent. Calico CNI (which GAG's e2e uses)
requires Sysbox-EE (enterprise edition), which was archived in May 2022 at
`docker-archive/nestybox.sysbox-ee` after the Docker acquisition and has received no
releases since.

**Rootless Docker + rootless kind** — Requires cgroup v2 on the host node and four
iptables kernel modules pre-loaded: `ip_tables`, `iptable_nat`, `ip6_tables`,
`ip6table_nat`. Doable on GKE COS nodes but requires a privileged DaemonSet to
`modprobe` the modules — the runner pod stays unprivileged but the setup requires node
surgery. Lower isolation gain than Kata (shared kernel vs. micro-VM).

**Kata Containers** — Runs each pod inside a lightweight VM via an OCI-compatible
`RuntimeClass`. The pod itself requires no `privileged: true`; isolation is enforced at
the hypervisor layer. Inside the Kata VM, Docker and kind run natively with no DinD
tricks. This is the strongest *container-escape* boundary available on GKE.

It is not, on its own, a security posture. Kata bounds the kernel, not the pod network:
the node's metadata server stays reachable (measured — see [Motivation](#1-oss-pwn-request-threat)),
so Workload Identity is a prerequisite. The capabilities an unprivileged DinD runner still
needs inside the guest (`SYS_ADMIN`, `NET_ADMIN`, `SYS_PTRACE`, `SYS_CHROOT`, rw `/proc/sys`
and `/sys/fs/cgroup`) approach `privileged` on an ordinary runtime, so the guarantee rests
entirely on the VM boundary rather than on defence in depth. And it trades a large,
well-trodden kernel-CVE surface for a smaller, more exotic hypervisor one — much harder to
exploit, not impossible, and on GKE the hypervisor is itself nested inside a GCE VM.
Q226 verified the boundary exists (guest kernel ≠ node kernel, no host privilege); it did
not attempt a breakout. The full accounting is in
[What Kata does not buy you](../operations/kata-dind-workloads.md#what-kata-does-not-buy-you).

> **Common confusion:** GKE's nested-virtualization documentation mentions
> `securityContext.privileged: true` in some contexts. That requirement applies to pods
> that interact *directly* with the nested hypervisor (e.g. launching their own VMs). A
> pod that uses `runtimeClassName: kata` does not do this — the Kata shim handles VM
> lifecycle outside the pod. The runner pod runs without any privileged context.

---

## Technical approach

GKE nodes are themselves VMs (on GCE). To run VMs inside them (as Kata requires), GKE
must be configured with nested virtualization on the node pool. This is a node-level config
— the runner pod does not need `privileged: true`.

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

**Why kind-in-runner rather than a shared test cluster.** An alternative would run the
e2e suite against a pre-provisioned GKE cluster rather than spinning up kind inside each
runner pod. This eliminates the Docker-in-runner requirement but breaks parallel PR
testing: CRDs, webhooks, and ClusterRoles are cluster-scoped, so concurrent runs collide
unless each gets a fully isolated API server (e.g. via vcluster). kind-in-runner avoids
this entirely — each CI run gets its own cluster, and any number of PRs can run
simultaneously without coordination. For a project developed with multiple concurrent
sessions this parallelism property is load-bearing.

**Why this can't be validated locally on a Mac.** Kata boots a KVM guest per pod, so it
needs `/dev/kvm` — i.e. nested virtualization — on the node. On a Mac, containers already
run inside a Linux VM (Docker Desktop's LinuxKit VM, Colima, …), so Kata would need that
outer VM to expose *nested* virt to its guest and the container tooling to pass `/dev/kvm`
through to the kind node-container. Every link in that chain has to cooperate, and most
Macs break it:

- **Apple Silicon M1 / M2** — no nested virt at the silicon/framework level. Not possible.
- **Apple Silicon M3+ (macOS 15 Sequoia+)** — Apple's `Virtualization.framework` added
  nested virt here, so the host/OS link finally exists, but (a) the container tooling does
  not generally wire `/dev/kvm` through to workloads, and (b) the whole GAG e2e stack is
  x86_64 while the machine is arm64 (emulation, or nothing). Worth re-checking against
  current Docker Desktop / Colima release notes before ruling out a specific M3/M4 box.
- **Intel Macs** — macOS's `Hypervisor.framework` does not expose nested VT-x into the
  Linux guest, so `/dev/kvm` inside the Docker VM does not function.

This is why the local test tier ([`docs/development/testing.md`](../development/testing.md))
routes the *other* sandbox runtime — gVisor — to `minikube` (its systrap platform needs no
KVM), and keeps Kata on GKE nested-virt or bare metal. A genuinely local Kata loop needs a
Linux host with `/dev/kvm` (bare metal, or a cloud VM with nested virt) — not a Mac.

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

AC#5 is deferred, not failed: it requires an image bundling `dockerd` + `kind` + the
Go/test toolchain + the repo checkout, which GAG does not publish today. Everything
that criterion would exercise beneath the suite — Docker, `kind`, image loading, pod
scheduling in the inner cluster — is proven by AC#2/#3/#4. Building that image and
running the suite is part of the CI-integration follow-up.

**Verdict: GO.**

---

## Exact validated configuration

### 1. Node pool

`--enable-nested-virtualization` is a **GA** flag, available on both
`gcloud container clusters create` and `gcloud container node-pools create`. It requires
`UBUNTU_CONTAINERD`, or `COS_CONTAINERD` at `1.28.4-gke.1083000`+. Autopilot cannot do it.

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

Upstream **no longer ships raw `kata-deploy.yaml` / `kata-rbac.yaml`**; those release-asset
URLs now 404. The canonical installer is an OCI Helm chart:

```bash
helm install kata-deploy \
  oci://quay.io/kata-containers/kata-deploy-charts/kata-deploy \
  --version 3.32.0 -n kube-system -f deploy/kata-ci/kata-values.yaml --wait
```

Values live in [`deploy/kata-ci/kata-values.yaml`](../../deploy/kata-ci/kata-values.yaml).
The chart creates the `kata-qemu` RuntimeClass (with `overhead: 160Mi / 250m`); we ship
only the `kata` alias in-repo.

**Do not conflate the two node labels.** `kata-deploy` must be *selected* by the pool's own
label (`gag.dev/kata-ci`), and it *sets* `katacontainers.io/kata-runtime=true` once the
runtime is installed — which is what the RuntimeClass schedules on. Using one label for
both lets a Kata pod land on a node whose runtime does not exist yet.

### 3. The runner pod — six non-obvious requirements

Everything below was discovered by running it. See
[`deploy/kata-ci/runner-pod.yaml`](../../deploy/kata-ci/runner-pod.yaml).

1. **`/var/lib/docker` must be a raw block volume, not an `emptyDir`.** Kata surfaces an
   `emptyDir` as **virtiofs**, on which Docker cannot use `overlay2`; it silently falls
   back to `vfs`. kind then switches its node snapshotter to `fuse-overlayfs`, which needs
   `/dev/fuse` — absent from the guest — and the inner kubelet dies with
   `failed to create kubelet: open /dev/kmsg` / snapshotter errors. Use
   `volumeMode: Block` + `volumeDevices`, then `mkfs.ext4` inside the guest.
   *Gotcha:* `docker:dind` declares `VOLUME /var/lib/docker` and Kata pre-mounts virtiofs
   there, so a naive "is it mounted?" check passes and skips your ext4 mount. Test for the
   **device**, not the mountpoint.
2. **`/dev/kmsg` does not exist in the Kata guest**, and the inner kubelet hard-requires it
   (`open /dev/kmsg: no such file or directory`). `mknod /dev/kmsg c 1 11` (needs
   `CAP_MKNOD`), then bind it into kind's node container via `extraMounts`.
3. **`/sys/fs/cgroup` is mounted read-only** for a non-privileged container, so `runc`
   cannot create the kind node's cgroup. `mount -o remount,rw /sys/fs/cgroup` succeeds with
   `CAP_SYS_ADMIN`. Under Kata this hierarchy belongs to the **guest** kernel, so the
   remount grants nothing on the host — under plain runc the same tree is the host's, which
   is precisely why classic DinD demands `privileged: true`.
4. **`/proc/sys` is read-only** likewise; Docker writes per-veth
   `net.ipv6.conf.<iface>.disable_ipv6`. Same remount, same guest-only reasoning.
   (`net.ipv4.ip_forward` is already `1` in the guest, so Docker never writes it.)
5. **cgroup v2 nesting.** The cgroup-namespace root holds our shell + `dockerd`, and cgroup
   v2 forbids a cgroup from holding processes *and* delegating controllers to children — so
   systemd inside kind's node cannot create `/init.scope`
   (`Failed to create /init.scope control group: Structure needs cleaning`). Evacuate the
   root into a leaf cgroup, then populate `cgroup.subtree_control`. `docker:dind`'s own
   entrypoint does this; overriding `command:` skips it.
6. **IPv6 is disabled in the guest**, but kind unconditionally creates its Docker network
   with `--ipv6`. Pre-create an IPv4-only bridge network named `kind`; kind reuses it.

### 4. Capabilities

`drop: [ALL]`, then add Docker's default set plus four extras. `privileged: true` is never
needed and must never be added.

```
CHOWN DAC_OVERRIDE FSETID FOWNER MKNOD NET_RAW SETGID SETUID
SETFCAP SETPCAP NET_BIND_SERVICE SYS_CHROOT KILL AUDIT_WRITE   # Docker defaults
SYS_ADMIN NET_ADMIN SYS_RESOURCE SYS_PTRACE                    # rootful dockerd + runc
```

Two of these are easy to miss: **`FOWNER`** (image layer unpack `chmod`s files it does not
own — `chmod /run/rpcbind: operation not permitted`) and **`SYS_CHROOT`** (runc `setns()`
into the container mount namespace — `join container mntns: setns: operation not permitted`).

---

## Constraints and gotchas found

- **Capacity, not quota.** `n2-standard-4` and `n2d-standard-4` were both
  `ZONE_RESOURCE_POOL_EXHAUSTED` in `us-central1-a` while `N2_CPUS` quota sat at 0/200.
  A plain non-nested-virt `n2` also failed, so nested virt does **not** narrow the capacity
  pool, and `n2d` is not rejected for lacking AMD SVM — the `n2/n2d/c2/c2d` allowlist in
  [`scripts/kata-node-pool.sh`](../../scripts/kata-node-pool.sh) is correct.
  `c2-standard-4` and `c2d-standard-4` both worked. Watch the per-family regional quota:
  `C2_CPUS` defaulted to **8** on a fresh project.
- **A stockout wedges the cluster.** A failing `CREATE_NODE_POOL` op holds a cluster-level
  lock (`Cluster is running incompatible operation`) and blocks even deleting the pool,
  for tens of minutes. Prefer creating the nested-virt pool as the cluster's *initial* pool
  (`clusters create --enable-nested-virtualization`) so a stockout fails fast.
- **GKE preinstalls `gvisor` and `confidential-linked-runner` RuntimeClasses.** They are
  unrelated to Kata; do not assume a `RuntimeClass` listing means Kata is installed.
- **`kata-deploy` is a DaemonSet, so it self-heals.** Recreating the node pool (e.g. to flip
  `--workload-metadata`) reinstalls Kata automatically; the RuntimeClass survives.
- The chart's post-delete cleanup Job uses `quay.io/kata-containers/kubectl:latest` —
  unpinned. Note it if supply-chain pinning matters to you.
- Kata VM boot overhead is small (~2 s), but the RuntimeClass `overhead`
  (160Mi / 250m per pod) is real and must be included when sizing nodes.
- **Deleting the cluster orphans the Block PVC's Persistent Disk.** A `Delete`
  reclaim policy fires when the *PVC* is deleted, not when the cluster is. The
  100Gi `pd-balanced` survives cluster teardown and keeps billing — delete the PVC
  first, then check `gcloud compute disks list`.
- Kata guest kernel `6.18.35` supports `ext4`, `xfs`, `overlay`, `fuse` and
  `virtiofs`. The `overlay2` failure is a *backing-filesystem* limitation
  (virtiofs), not missing kernel support — which is why a block device fixes it.

---

## Reference architecture deliverable

The spike validates the approach on GKE, but the reference architecture
([`docs/operations/kata-dind-workloads.md`](../operations/kata-dind-workloads.md)) is
provider-agnostic. It covers three tiers:

**Tier 1 — cloud-hosted (GKE, AKS, EKS).** Nested-virtualization node pool + Kata
RuntimeClass. Variant-specific guidance per provider: machine family requirements
(n2/n2d/c2/c2d on GKE), Standard vs. Autopilot trade-offs (Autopilot blocks nested
virt), Kata DaemonSet installer vs. managed add-on. Best fit for teams already
cloud-native.

**Tier 2 — bare metal and on-prem.** Kata on real hardware requires no nested
virtualization — QEMU or Cloud Hypervisor runs directly. No machine-family constraints,
lower overhead, and the correct path for GPU workloads: PCIe passthrough of NVIDIA or AMD
GPUs into the Kata micro-VM works from bare metal. GKE's GPU machine families (a2, a3,
g2) do not support nested virtualization, so GPU + Kata on cloud requires bare-metal or
dedicated instances. This tier is the reference architecture for teams running GPU CI on
owned hardware or cost-sensitive on-prem environments.

**Tier 3 — pragmatic fallback (any provider).** Privileged DinD on a dedicated,
locked-down node pool. Documents compensating controls explicitly: workload-identity
scope-down, metadata-server block, network policy, node taint isolation. For teams where
Kata is not feasible but full privilege is also unacceptable.

Each tier covers: pod security context, RuntimeClass or equivalent, node requirements,
`ActionsGateway` CR configuration to target the right pool, observed startup overhead,
and CI timeout guidance.

---

## CI integration — the follow-up

The spike deliberately stops at the reference architecture. Wiring Kata into the real
dogfood e2e pipeline is separate work, tracked on the Queue. It needs:

1. A **GAG e2e runner image** bundling `dockerd`, `kind`, the Go/test toolchain and the
   repo checkout. This does not exist today and is what blocked AC#5. Its entrypoint must
   perform the six setup steps above (block-device format/mount, `/dev/kmsg`, the two
   remounts, cgroup v2 nesting, the IPv4-only `kind` network) — the spike's
   [`runner-pod.yaml`](../../deploy/kata-ci/runner-pod.yaml) `args:` block is the
   reference implementation.
2. A kind cluster config passing `/dev/kmsg` into the node via `extraMounts`.
3. A permanent nested-virt node pool in the CI project (provisioned once, not per-run) to
   avoid the ~5–10 min pool create/delete overhead — and to dodge the stockout-wedge
   failure mode above.
4. **Workload Identity on that pool** (`--workload-metadata=GKE_METADATA`). This is a
   security prerequisite, not a nicety: without it the runner reaches the node's service
   account token regardless of Kata.
5. Updating [`scripts/dogfood/e2e-setup.sh`](../../scripts/dogfood/e2e-setup.sh) and
   `docs/development/testing.md` for the new runner requirements.

Note that GAG's e2e today runs on GitHub-hosted runners, not the dogfood cluster, so this
is a change of *where* e2e runs as much as *how*.
