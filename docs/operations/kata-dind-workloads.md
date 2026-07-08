# Running DinD / image-build workloads under Kata Containers

**Audience:** Platform engineers running a GitHub Actions Gateway (GAG)
cluster. **Goal:** run a Docker-in-Docker (DinD) or in-runner image-build
job under [Kata Containers](https://katacontainers.io/) — a Kernel-based
Virtual Machine (KVM) micro-VM runtime — so the worker pod gets hypervisor
isolation while staying at `securityProfile: baseline` with **no**
`privileged: true` container.

Kata is the only approach in
[In-runner image builds](in-runner-image-builds.md) that gives a real
machine boundary around an *inner Docker daemon*. Rootless BuildKit and
Kaniko avoid the daemon entirely; Sysbox runs the daemon behind a
user-namespace; classic privileged DinD runs it on the host kernel. Kata
runs the whole pod — daemon included — inside a per-pod micro-VM, so a
container escape lands in a throwaway guest kernel, not on the node. This
page is the cluster-side how-to: the node prerequisites, the runtime
install, and the worker `podTemplate` field that selects it.

It is operator-focused. For *why* GAG chose Kata over Sysbox/rootless and
the provider-agnostic design, see
[Kata Containers on GKE](../plan/kata-on-gke.md); for the executable
go/no-go validation steps, see the
[Kata-on-GKE spike runbook](kata-ci-spike-runbook.md).

## Table of Contents

- [Why this matters for GAG](#why-this-matters-for-gag)
- [How it fits together](#how-it-fits-together)
- [Prerequisite — nested-virtualization nodes](#prerequisite--nested-virtualization-nodes)
- [Cluster setup — Kata runtime and RuntimeClass](#cluster-setup--kata-runtime-and-runtimeclass)
- [Configure the worker podTemplate](#configure-the-worker-podtemplate)
- [The security rationale](#the-security-rationale)
- [Caveats and limitations](#caveats-and-limitations)
- [Related](#related)

## Why this matters for GAG

Two GAG-specific constraints rule out privileged DinD and make a micro-VM
boundary the right tool:

- **The untrusted-PR threat model.** GAG targets public open-source
  projects, where any contributor opens a pull request and CI runs their
  code on your infrastructure (the "pwn request" attack class). A
  privileged DinD pod can write the host node's `/proc` and `/sys` and
  reach node-scoped credentials via the cloud metadata server — a direct
  path off the runner. Kata confines that code to a guest VM whose kernel
  is not the node's.
- **The dogfood requirement.** GAG's own end-to-end CI runs `kind` inside a
  runner pod, which needs a Docker daemon. Shipping a product whose value
  is secure multi-tenant isolation while running its *own* CI as privileged
  DinD would contradict the model. Kata lets that CI runner stay
  unprivileged.

Because the isolation is enforced at the hypervisor, the pod keeps a
`baseline` posture: no `privileged: true`, no host namespaces, no relaxed
Pod Security Admission (PSA) level. The
[`privileged` profile](in-runner-image-builds.md#how-the-security-profiles-constrain-a-build)
and its platform-granted namespace label are never needed.

## How it fits together

Kata inserts a micro-VM between the kubelet and the runner container. On a
managed cloud the node is itself a VM, so the node pool must enable
*nested* virtualization for the guest VM to boot:

```text
Node (cloud VM, nested-virt enabled)  ── needs /dev/kvm
  └── kubelet hands the pod to the Kata containerd shim (runtimeClassName)
       └── Kata micro-VM (QEMU)        ── the isolation boundary
            └── runner container        ── securityProfile: baseline, NOT privileged
                 └── dockerd            ── a normal daemon, no special flags
                      └── docker build / kind / nested containers
```

The runner pod never talks to the hypervisor directly — the Kata shim owns
the VM lifecycle outside the pod — so the pod needs no privileged context
of its own. (GKE's nested-virtualization docs mention
`securityContext.privileged: true`; that applies only to pods that launch
their *own* VMs, not to pods that select a Kata `RuntimeClass`.)

## Prerequisite — nested-virtualization nodes

A Kata pod boots a KVM guest, which needs the `/dev/kvm` device on the
node. On bare metal that is present by default. On a managed cloud you must
run the workload on a node pool with nested virtualization enabled and on a
machine family that supports it.

**Google Cloud (GKE).** Nested virtualization is a node-pool setting and is
restricted by machine family:

| Requirement | Detail |
|---|---|
| Cluster mode | **GKE Standard.** Autopilot does **not** allow nested virtualization. |
| Machine family | **N2, N2D, C2, C2D** support nested virtualization and are the families the shipped [`scripts/kata-node-pool.sh`](../../scripts/kata-node-pool.sh) accepts. **N1** also supports nested virtualization on GKE, but the provisioning script rejects it — provision an N1 pool manually if you need it. **E2 does not.** The GPU families (**A2, A3, G2**) do not either — GPU + Kata on cloud needs bare metal or dedicated instances. |
| Node image | **`UBUNTU_CONTAINERD`**, or `COS_CONTAINERD` at `1.28.4-gke.1083000`+. |
| Flag | `--enable-nested-virtualization` (GA). Accepted by *both* `gcloud container node-pools create` and `gcloud container clusters create` — prefer the cluster form so a capacity stockout fails fast instead of wedging the cluster. |
| Workload Identity | `--workload-pool` + `--workload-metadata=GKE_METADATA`. **Required**, not optional — Kata does not block the metadata server. See [the security rationale](#the-security-rationale). |
| Node label | Label the pool with your **own** label (e.g. `gag.dev/kata-ci=true`) and use it to scope the Kata installer. Do **not** reuse `katacontainers.io/kata-runtime` — `kata-deploy` sets that itself once the runtime is installed, and the `RuntimeClass` schedules on it. |

The repo ships a parameterized provisioning script —
[`scripts/kata-node-pool.sh`](../../scripts/kata-node-pool.sh) — that wraps
the `gcloud container node-pools create` call with these flags. Preview it
with `DRY_RUN=1` before spending cloud time; see
[runbook step 1](kata-ci-spike-runbook.md#step-1--create-a-nested-virt-cluster).

**Verify `/dev/kvm` is present** on a labelled node before going further —
if it is missing, nested virtualization is not actually enabled and the
guest VM cannot boot:

```bash
NODE=$(kubectl get nodes -l katacontainers.io/kata-runtime=true \
  -o jsonpath='{.items[0].metadata.name}')
kubectl debug node/"$NODE" -it --image=busybox -- ls -l /host/dev/kvm
```

Expect a character device. Other clouds expose nested virtualization
differently (AWS bare-metal `*.metal` instances; Azure `Dv3`/`Ev3` and
later); on owned hardware no nested virtualization is needed at all.

## Cluster setup — Kata runtime and RuntimeClass

Installing a runtime handler and creating a `RuntimeClass` is a
**cluster-admin** operation. As with the gVisor/Kata sandbox runtimes in
[Appendix B — Worker isolation](../design/appendix-b-worker-isolation.md),
GAG's controllers never install runtime handlers or `RuntimeClass` objects;
the Gateway Manager Controller (GMC) and Actions Gateway Controllers (AGCs)
only *honour* a `runtimeClassName` a tenant sets. Two steps:

1. **Install the Kata runtime on the labelled nodes.** Kata Containers no
   longer ships raw `kata-deploy.yaml` / `kata-rbac.yaml` manifests — those
   release-asset URLs now return 404. The canonical installer is an OCI Helm
   chart. It drops the Kata binaries, the QEMU hypervisor, the guest
   kernel/image, and the containerd runtime handler onto each node it lands on:

   ```bash
   helm install kata-deploy \
     oci://quay.io/kata-containers/kata-deploy-charts/kata-deploy \
     --version 3.32.0 -n kube-system \
     -f deploy/kata-ci/kata-values.yaml --wait
   ```

   > **Do not scope the installer with `katacontainers.io/kata-runtime`.**
   > That is the label `kata-deploy` *sets* on a node once the runtime is in
   > place, and it is what the `RuntimeClass` schedules on. Select the
   > installer with the **node pool's own** label (the shipped values use
   > `gag.dev/kata-ci=true`). Using one label for both creates a race in
   > which a Kata pod can be scheduled onto a node whose runtime does not
   > exist yet.

   The DaemonSet itself runs `privileged: true` — installing a node-level
   container runtime means writing `/opt/kata`, `/usr/local/bin` and
   `/etc/containerd`. That privilege is confined to the trusted,
   operator-run installer; the *workload* pod stays unprivileged. That
   asymmetry is the Kata model.

2. **Register the `RuntimeClass`.** The chart already creates `kata-qemu`
   (with the correct pod `overhead`). The repo ships only a stable `kata`
   alias, so the hypervisor can be retargeted later without editing every
   pod spec — see
   [`deploy/kata-ci/runtimeclass.yaml`](../../deploy/kata-ci/runtimeclass.yaml):

   ```yaml
   apiVersion: node.k8s.io/v1
   kind: RuntimeClass
   metadata:
     name: kata
   handler: kata-qemu            # must match the kata-deploy handler
   overhead:
     podFixed:
       memory: "160Mi"           # the micro-VM's footprint; size nodes for it
       cpu: "250m"
   scheduling:
     nodeSelector:
       katacontainers.io/kata-runtime: "true"   # set BY kata-deploy, post-install
   ```

Confirm the classes exist before configuring workers. Note GKE preinstalls
`gvisor` and `confidential-linked-runner` classes that have nothing to do with
Kata — a non-empty listing does not mean Kata is installed:

```bash
kubectl get runtimeclass kata kata-qemu
kubectl get nodes -l katacontainers.io/kata-runtime=true   # kata-deploy labelled them
```

## Configure the worker podTemplate

Point the worker pods at the runtime by setting `runtimeClassName` on the
runner group's worker `podTemplate` (`spec.runnerGroups[].podTemplate`). No
privileged context, no host namespaces, and no profile escalation are involved:

```yaml
apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: build-gateway
spec:
  securityProfile: baseline            # the default — Kata needs no escalation
  runnerGroups:
    - runnerLabels: ["kata", "self-hosted"]   # first label → derived RunnerGroup name
      podTemplate:                     # worker pod config lives per runner group
        spec:
          runtimeClassName: kata-qemu
          # The runtime label is enforced by the RuntimeClass scheduling
          # rule above; add a matching nodeSelector only if you also want it
          # explicit on the pod.
          containers:
            - name: runner
              # A normal runner image with dockerd inside — no privileged
              # flag, no /var/run/docker.sock host mount.
              securityContext:
                privileged: false
```

The AGC honours a tenant-set `runtimeClassName` and applies no override
that strips it. If `dockerd` fails to start inside the guest, add one
Linux capability at a time to the container's
`securityContext.capabilities.add` and re-test — **never** reach for
`privileged: true`. Record the final minimal capability set for your
runbook.

## What an unprivileged `dockerd` actually needs inside the guest

Dropping `privileged: true` does not come free — six things must be arranged
that a privileged container gets implicitly. All were validated live; the
reference implementation is the `args:` block of
[`deploy/kata-ci/runner-pod.yaml`](../../deploy/kata-ci/runner-pod.yaml).

1. **`/var/lib/docker` must be a raw block volume, not an `emptyDir`.** Kata
   surfaces an `emptyDir` as **virtiofs**, and Docker cannot run `overlay2`
   on it — it silently falls back to `vfs`. `kind` then switches its node
   snapshotter to `fuse-overlayfs`, which needs `/dev/fuse` (absent from the
   guest), and the inner kubelet never becomes healthy. Use a PVC with
   `volumeMode: Block` + `volumeDevices`, then `mkfs.ext4` it inside the
   guest. Beware: the `docker:dind` image declares `VOLUME /var/lib/docker`
   and Kata pre-mounts virtiofs there, so a "is it mounted?" check passes and
   silently skips your ext4 mount — test for the **device**.
2. **`/dev/kmsg` does not exist in the Kata guest**, and a nested kubelet
   requires it. `mknod /dev/kmsg c 1 11` (needs `CAP_MKNOD`).
3. **`/sys/fs/cgroup` is read-only** for a non-privileged container, so `runc`
   cannot create the nested container's cgroup. `mount -o remount,rw
   /sys/fs/cgroup` works with `CAP_SYS_ADMIN`. Under Kata that hierarchy is
   the **guest** kernel's — the remount grants nothing on the host. Under
   plain `runc` the same tree is the host's, which is exactly why classic
   DinD demands `privileged: true`.
4. **`/proc/sys` is read-only** likewise; Docker writes per-veth
   `net.ipv6.conf.<iface>.disable_ipv6`. Same remount, same reasoning.
5. **cgroup v2 nesting.** cgroup v2 forbids a cgroup from holding processes
   *and* delegating controllers to children, so systemd in a nested container
   cannot create `/init.scope` (`Structure needs cleaning`). Move the
   cgroup-namespace root's processes into a leaf, then populate
   `cgroup.subtree_control`. `docker:dind`'s own entrypoint does this — you
   only need it if you override `command:`.
6. **IPv6 is disabled in the guest**, yet `kind` creates its Docker network
   with `--ipv6`. Pre-create an IPv4-only bridge network named `kind`.

The validated capability set — `drop: [ALL]` plus:

```text
CHOWN DAC_OVERRIDE FSETID FOWNER MKNOD NET_RAW SETGID SETUID
SETFCAP SETPCAP NET_BIND_SERVICE SYS_CHROOT KILL AUDIT_WRITE   # Docker's defaults
SYS_ADMIN NET_ADMIN SYS_RESOURCE SYS_PTRACE                    # rootful dockerd + runc
```

Two are easy to miss: **`FOWNER`** (image layer unpack `chmod`s files it does
not own) and **`SYS_CHROOT`** (`runc` `setns()` into the container mount
namespace).

## The security rationale

| Property | Privileged DinD | DinD under Kata |
|---|---|---|
| `securityProfile` required | `privileged` (platform-granted namespace label) | `baseline` (the default) |
| `privileged: true` container | Yes | No |
| Escape blast radius | The host node and, from there, other tenants | A throwaway guest VM kernel |
| Node `/proc`, `/sys`, devices, kernel | Exposed | Behind the VM boundary |
| Node metadata server (`169.254.169.254`) | Exposed | **Still exposed** — see below |

> ### Kata does not close the metadata-server path
>
> Kata isolates the **kernel**, not the **pod network**. Measured from inside a
> Kata micro-VM on GKE: `169.254.169.254` stayed reachable and
> `computeMetadata/v1/instance/service-accounts/default/token` returned
> **HTTP 200** — the node's GCE service-account token. A micro-VM around an
> untrusted CI runner that can still mint node credentials is not a boundary.
>
> **Enable Workload Identity. It is a prerequisite, not an enhancement.**
>
> ```bash
> gcloud container clusters update <cluster> --workload-pool=<project>.svc.id.goog
> gcloud container node-pools update <pool> --cluster=<cluster> \
>   --workload-metadata=GKE_METADATA        # recreates the nodes
> ```
>
> Re-probed with it on, the metadata server serves the workload-pool identity
> (`<project>.svc.id.goog`) instead of the node's service account. Also set
> `automountServiceAccountToken: false` on the runner pod so it carries no
> Kubernetes API token, and consider a NetworkPolicy denying egress to
> `169.254.169.254/32` as defence in depth.
>
> On other clouds the equivalent controls are IMDSv2 with a hop limit of 1 plus
> a restrictive instance profile (AWS), or Azure AD Workload Identity with the
> IMDS endpoint blocked.

Kata satisfies GAG's
[secure-by-default principle](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in):
the workload that historically demanded the least-restrictive profile now
runs at the default profile. Privileged DinD remains documented as a last
resort — and even then it should be *paired* with a sandbox runtime, which
is the same mechanism described here applied on top of `privileged`; see
[In-runner image builds — privileged DinD](in-runner-image-builds.md#approach-4--plain-privileged-dind-avoid-where-possible).

## Caveats and limitations

- **Startup overhead is small — but the `overhead` accounting is not.** Measured
  on `c2-standard-4`: a Kata pod reached `Ready` in ~3 s vs ~1 s for `runc`, and
  `kind create cluster` took 58 s from a cold image cache (43 s warm) against a
  ~6 min ceiling. The bigger planning cost is the `RuntimeClass` `overhead`
  (160Mi / 250m **per pod**), which the scheduler reserves on top of the
  container's own requests — size nodes for it.
- **Nested-virt capacity can be scarce.** During validation, `n2-standard-4` and
  `n2d-standard-4` were both `ZONE_RESOURCE_POOL_EXHAUSTED` in `us-central1-a`
  while CPU quota sat at 0/200 — a stockout, not a quota problem (a plain
  non-nested-virt `n2` failed too, so nested virt itself does not narrow the
  pool). `c2`/`c2d` worked. Check the **per-family** regional quota:
  `C2_CPUS` defaults to 8 on a fresh project.
- **A node-pool stockout wedges the cluster.** A failing `CREATE_NODE_POOL`
  operation holds a cluster-level lock (`Cluster is running incompatible
  operation`) that blocks even deleting the pool, for tens of minutes. Prefer
  creating the nested-virt pool as the cluster's *initial* pool
  (`gcloud container clusters create --enable-nested-virtualization`).
- **`kata-deploy` is a DaemonSet, so it self-heals.** Recreating the node pool
  (for example to flip `--workload-metadata`) reinstalls Kata automatically and
  the `RuntimeClass` survives.
- **Not all kernel features pass through.** Workloads needing host kernel
  modules, specific `/dev` devices, or GPU passthrough need extra Kata
  configuration (and, for GPU, bare-metal or dedicated instances — the
  cloud GPU families lack nested virtualization).
- **Run `dockerd` inside the runner container, not as a regular sidecar.**
  The pattern above keeps the daemon a nested process of the single `runner`
  container, so the pod reaps cleanly when the job ends. If you instead split
  the daemon into a separate sidecar container, declare it as a **native
  sidecar** (`restartPolicy: Always` init container) — a regular sidecar runs
  forever and keeps the worker pod from reaping, stranding the runner slot.
  See [In-runner image builds § Sidecar containers must be native
  sidecars](in-runner-image-builds.md#sidecar-containers-must-be-native-sidecars-q249).
- **Validated, but not yet wired into GAG's own CI.** The unprivileged
  `dockerd` + `kind` path was proven end-to-end on GKE (`1.35.5-gke.1241004`,
  Ubuntu 24.04, `c2-standard-4` nested-virt, Kata 3.32.0/QEMU) — see
  [Kata Containers on GKE](../plan/kata-on-gke.md) for the evidence. Running
  GAG's full `make e2e` inside the runner is still outstanding: it needs a
  runner image bundling `dockerd`, `kind` and the test toolchain, which does
  not exist yet. Confirm the steps on your own cluster before cutting over
  privileged workloads.

## Related

- [In-runner image builds](in-runner-image-builds.md) — pick a build
  approach (BuildKit rootless, Kaniko, Sysbox, Kata, privileged DinD) and
  the `securityProfile` each needs.
- [Kata-on-GKE spike runbook](kata-ci-spike-runbook.md) — executable
  go/no-go steps for the unprivileged `dockerd` + `kind` runner.
- [Kata Containers on GKE](../plan/kata-on-gke.md) — design rationale, the
  options rejected (Sysbox, rootless, kindbox), and the provider-agnostic
  reference architecture.
- [Appendix B — Worker isolation](../design/appendix-b-worker-isolation.md)
  — `runc` vs gVisor vs Kata sandbox-runtime trade-offs.
- [Security § 5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in)
  — the authoritative `securityProfile` model.
</content>
</invoke>
