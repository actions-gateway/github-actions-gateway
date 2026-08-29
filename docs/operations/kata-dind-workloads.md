# Running DinD / image-build workloads under Kata Containers

**Audience:** Platform engineers running a GitHub Actions Gateway (GAG) cluster.
**Goal:** run a Docker-in-Docker (DinD) or in-runner image-build job under [Kata Containers](https://katacontainers.io/) — a Kernel-based Virtual Machine (KVM) micro-VM runtime — with **no** `privileged: true` container anywhere in the worker pod.

Kata is the only approach in [In-runner image builds](in-runner-image-builds.md) that gives a real machine boundary around an *inner Docker daemon*.
Rootless BuildKit and Kaniko avoid the daemon entirely; Sysbox runs the daemon behind a user-namespace; classic privileged DinD runs it on the host kernel.
Kata runs the whole pod — daemon included — inside a per-pod micro-VM, so a container escape lands in a throwaway guest kernel, not on the node.
This page is the cluster-side how-to: the node prerequisites, the runtime install, and the worker `podTemplate` field that selects it.

**You do not have to transcribe the pod shape from this page.** GAG ships it as `kata-dind` in the [runner template library](runner-template-library.md), applied with a single `kubectl apply -k deploy/templates/kata-dind`.
Read this page for the node and runtime prerequisites the template cannot express, and for why each line of it is the way it is; apply the shipped base rather than retyping it.

It is operator-focused.
For *why* GAG chose Kata over Sysbox/rootless and the provider-agnostic design, see [Kata Containers on GKE](../plan/archive/kata-on-gke.md); for the executable go/no-go validation steps, see the [Kata-on-GKE spike runbook](kata-ci-spike-runbook.md).

## Table of Contents

- [Why this matters for GAG](#why-this-matters-for-gag)
- [How it fits together](#how-it-fits-together)
- [Prerequisite — nested-virtualization nodes](#prerequisite--nested-virtualization-nodes)
- [Cluster setup — Kata runtime and RuntimeClass](#cluster-setup--kata-runtime-and-runtimeclass)
- [Configure the worker podTemplate](#configure-the-worker-podtemplate)
- [The security rationale](#the-security-rationale)
- [Untrusted pull requests — the tight-egress posture](#untrusted-pull-requests--the-tight-egress-posture)
- [Caveats and limitations](#caveats-and-limitations)
- [Related](#related)

## Why this matters for GAG

Two GAG-specific constraints rule out privileged DinD and make a micro-VM boundary the right tool:

- **The untrusted-PR threat model.** GAG targets public open-source projects, where any contributor opens a pull request and CI runs their code on your infrastructure (the "pwn request" attack class).
  A privileged DinD pod can write the host node's `/proc` and `/sys` and reach node-scoped credentials via the cloud metadata server — a direct path off the runner.
  Kata confines that code to a guest VM whose kernel is not the node's.
- **The dogfood requirement.** GAG's own end-to-end CI runs `kind` inside a runner pod, which needs a Docker daemon.
  Shipping a product whose value is secure multi-tenant isolation while running its *own* CI as privileged DinD would contradict the model.
  Kata lets that CI runner stay unprivileged.

Because the isolation is enforced at the hypervisor, the pod itself needs no `privileged: true` container and no host namespaces.
One PSA nuance is easy to get wrong, though: the unprivileged `dockerd` still needs a capability set (`SYS_ADMIN`, `NET_ADMIN`, `SYS_RESOURCE`, `SYS_PTRACE`, `NET_RAW` — [see below](#what-an-unprivileged-dockerd-actually-needs-inside-the-guest)) that exceeds the PSS **baseline** `capabilities.add` allowlist, and Pod Security Admission (PSA) is not Kata-aware — it cannot see that these capabilities act on a guest kernel.
Verified against a real apiserver: `enforce=baseline` rejects this pod shape as Forbidden.
So the worker namespace still needs the [`privileged` profile](in-runner-image-builds.md#how-the-security-profiles-constrain-a-build) (platform-granted label) — or, on a self-managed control plane, a [PodSecurity admission exemption](https://kubernetes.io/docs/concepts/security/pod-security-admission/#exemptions) for the Kata `runtimeClass` (managed offerings like GKE do not expose that config).
What keeps the pod unprivileged is therefore **not** the PSA level but the worker template being platform-owned: author the Kata shape as a cluster-scoped `ClusterRunnerTemplate`, which tenants cannot edit, exactly as for privileged DinD.

## How it fits together

Kata inserts a micro-VM between the kubelet and the runner container.
On a managed cloud the node is itself a VM, so the node pool must enable *nested* virtualization for the guest VM to boot:

```text
Node (cloud VM, nested-virt enabled)  ── needs /dev/kvm
  └── kubelet hands the pod to the Kata containerd shim (runtimeClassName)
       └── Kata micro-VM (QEMU)        ── the isolation boundary
            └── runner container        ── unprivileged (privileged: false)
                 └── dockerd            ── a normal daemon, no special flags
                      └── docker build / kind / nested containers
```

The runner pod never talks to the hypervisor directly — the Kata shim owns the VM lifecycle outside the pod — so the pod needs no privileged context of its own.
(GKE's nested-virtualization docs mention `securityContext.privileged: true`; that applies only to pods that launch their *own* VMs, not to pods that select a Kata `RuntimeClass`.)

## Prerequisite — nested-virtualization nodes

A Kata pod boots a KVM guest, which needs the `/dev/kvm` device on the node.
On bare metal that is present by default.
On a managed cloud you must run the workload on a node pool with nested virtualization enabled and on a machine family that supports it.

**Google Cloud (GKE).** Nested virtualization is a node-pool setting and is restricted by machine family:

| Requirement | Detail |
|---|---|
| Cluster mode | **GKE Standard.** Autopilot does **not** allow nested virtualization. |
| Machine family | Take the list from the API, not from a doc. Measured 2026-08-02, `gcloud` rejected `c2d` and named the families that do take the flag: **A2, A3, C2, C3, C4, C4D, C4N, G2, H3, H4D, N1, N2, N4, N4D, Z3, M4**. **E2, C2D, and N2D are absent.** The shipped [`scripts/dev/kata-node-pool.sh`](../../scripts/dev/kata-node-pool.sh) accepts only `n2`, `n2d`, `c2`, `c2d`, which is a stale subset of that list; provision any other family by hand. **The GPU families A2, A3, and G2 are on the list**, so nested virtualization is *not* what stops GPU workloads running under Kata on GKE (see [what does](../plan/gpu-and-accelerated-ci.md#the-collision-with-the-security-goal)). The newer accelerator families are absent from it: A4X is Arm-based, G4 AMD-based. Google's [restriction rules](https://docs.cloud.google.com/compute/docs/instances/nested-virtualization/overview) (fetched 2026-08-08) exclude E2, memory-optimized, AMD- and Arm-powered, and H4D VMs, which agrees on the GPU families but not in the margins (it excludes H4D, C4D, and N4D, which the API named). Where they disagree, the API's rejection is current. See [Caveats](#caveats-and-limitations). |
| Node image | **`UBUNTU_CONTAINERD`**, or `COS_CONTAINERD` at `1.28.4-gke.1083000`+. |
| Flag | `--enable-nested-virtualization` (GA). Accepted by *both* `gcloud container node-pools create` and `gcloud container clusters create` — prefer the cluster form so a capacity stockout fails fast instead of wedging the cluster. |
| Workload Identity | `--workload-pool` + `--workload-metadata=GKE_METADATA`. **Required**, not optional — Kata does not block the metadata server. See [the security rationale](#the-security-rationale). |
| Node label | Label the pool with your **own** label (e.g. `gag.dev/kata-ci=true`) and use it to scope the Kata installer. Do **not** reuse `katacontainers.io/kata-runtime` as the *installer* scope — `kata-deploy` sets that itself once the runtime is installed, and the `RuntimeClass` schedules on it. |
| Autoscaling from zero | If the pool autoscales 0→N, **also bake `katacontainers.io/kata-runtime=true` into the pool's `--node-labels`** (found live under Q286): the cluster autoscaler simulates scheduling against the pool's *configured* labels only, so a label that kata-deploy applies post-install can never trigger the scale-up and Kata pods stay Pending forever. The window where a pod binds before kata-deploy finishes resolves via kubelet sandbox-create retries. Fixed-size pools should keep the two labels separate. |
| Taints | If the pool is tainted, give the kata-deploy chart a matching `tolerations:` value (the chart ships none) — otherwise the installer can never reach the only nodes it targets (found live under Q286). |

The repo ships a parameterized provisioning script — [`scripts/dev/kata-node-pool.sh`](../../scripts/dev/kata-node-pool.sh) — that wraps the `gcloud container node-pools create` call with these flags.
Preview it with `DRY_RUN=1` before spending cloud time; see [runbook step 1](kata-ci-spike-runbook.md#step-1--create-a-nested-virt-cluster).

**Amazon Web Services (EC2).** Bare metal is no longer the only option, and the unit of configuration is the instance rather than the node pool.
Nested virtualization is a CPU option that is **off by default**: pass `--cpu-options "NestedVirtualization=enabled"` to `run-instances`, or run `aws ec2 modify-instance-cpu-options --nested-virtualization enabled` against a stopped instance.

| Requirement | Detail |
|---|---|
| Instance family | AWS's [nested virtualization guide](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nested-virtualization.html) (fetched 2026-08-12) lists **C8i, M8i, R8i, C8id, R8id, M8id, C8i-flex, R8i-flex, M8i-flex, X8i, C7i, R7i, M7i, C7i-flex, M7i-flex, and I7i**. Every one is Intel; no AMD or Graviton family is on it, and **no P or G family is either**, so on EC2 nested virtualization *is* what stops GPU workloads under Kata, unlike GKE, where A2, A3, and G2 carry it. It shipped in two steps: C8i/M8i/R8i on 2026-02-16, the rest plus GovCloud (US) on 2026-06-18. |
| Hypervisor | KVM and Hyper-V are the supported L1 hypervisors, so Kata's QEMU path is in scope. |
| Performance | AWS recommends bare metal for hardware-virtualization workloads that are performance-sensitive or latency-bound. Kata boots a guest per pod, so treat a virtual family as the cheaper tier rather than a like-for-like substitute, and measure pod-`Ready` latency before committing a build fleet to it. |

Azure exposes nested virtualization on `Dv3`/`Ev3` and later.
On owned hardware none is needed at all.

**Verify `/dev/kvm` is present** on a labelled node before going further — if it is missing, nested virtualization is not actually enabled and the guest VM cannot boot:

```bash
NODE=$(kubectl get nodes -l katacontainers.io/kata-runtime=true \
  -o jsonpath='{.items[0].metadata.name}')
kubectl debug node/"$NODE" -it --image=busybox -- ls -l /host/dev/kvm
```

Expect a character device.

## Cluster setup — Kata runtime and RuntimeClass

Installing a runtime handler and creating a `RuntimeClass` is a **cluster-admin** operation.
As with the gVisor/Kata sandbox runtimes in [Appendix B — Worker isolation](../design/appendix-b-worker-isolation.md), GAG's controllers never install runtime handlers or `RuntimeClass` objects; the Gateway Manager Controller (GMC) and Actions Gateway Controllers (AGCs) only *honour* a `runtimeClassName` a tenant sets.
Two steps:

1. **Install the Kata runtime on the labelled nodes.** Kata Containers no longer ships raw `kata-deploy.yaml` / `kata-rbac.yaml` manifests — those release-asset URLs now return 404.
   The canonical installer is an OCI Helm chart.
   It drops the Kata binaries, the QEMU hypervisor, the guest kernel/image, and the containerd runtime handler onto each node it lands on:

   ```bash
   helm install kata-deploy \
     oci://quay.io/kata-containers/kata-deploy-charts/kata-deploy \
     --version 3.32.0 -n kube-system \
     -f deploy/kata-ci/kata-values.yaml --wait
   ```

   > **Do not scope the installer with `katacontainers.io/kata-runtime`.** That is the label `kata-deploy` *sets* on a node once the runtime is in place, and it is what the `RuntimeClass` schedules on.
   > Select the installer with the **node pool's own** label (the shipped values use `gag.dev/kata-ci=true`).
   > Using one label for both creates a race in which a Kata pod can be scheduled onto a node whose runtime does not exist yet.

   The DaemonSet itself runs `privileged: true` — installing a node-level container runtime means writing `/opt/kata`, `/usr/local/bin` and `/etc/containerd`.
   That privilege is confined to the trusted, operator-run installer; the *workload* pod stays unprivileged.
   That asymmetry is the Kata model.

2. **Register the `RuntimeClass`.** The chart already creates `kata-qemu` (with the correct pod `overhead`).
   The repo ships only a stable `kata` alias, so the hypervisor can be retargeted later without editing every pod spec — see [`deploy/kata-ci/runtimeclass.yaml`](../../deploy/kata-ci/runtimeclass.yaml):

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

Confirm the classes exist before configuring workers.
Note GKE preinstalls `gvisor` and `confidential-linked-runner` classes that have nothing to do with Kata — a non-empty listing does not mean Kata is installed:

```bash
kubectl get runtimeclass kata kata-qemu
kubectl get nodes -l katacontainers.io/kata-runtime=true   # kata-deploy labelled them
```

## Configure the worker podTemplate

The shipped `kata-dind` library entry is this section, complete and CI-exercised:

```bash
kubectl apply -k deploy/templates/kata-dind
```

Then point a `RunnerSet` at it with `templateRef.kind: ClusterRunnerTemplate`.
It runs jobs as applied: the template leaves `spec.workerImage` unset, and the AGC's digest-pinned default runner image already carries a Docker CLI and buildx, which is the client the dind sidecar is the daemon for.
Set `spec.workerImage` only if your jobs need `docker compose` (`ghcr.io/actions-gateway/build-runner`, pinned by digest from the release notes) or a toolchain of your own.
If the image you set will not pull, the worker pod shows `ImagePullBackOff` within seconds and the `RunnerSet` reports `WorkersNotStarting`/`PodsNotStarting` in the same window, gate or no gate; a set that opted into `spec.capacityGate` additionally reports `WorkerCapacityDeclined` and stops claiming jobs.
The [runner template library](runner-template-library.md) covers both, the supported way to patch the base, and [what to watch when the placeholder is left in place](runner-template-library.md#kata-dind-and-privileged-dind).

The rest of this section is what that template contains and why, for reading rather than retyping.

Kata is selected by `runtimeClassName` on the worker `podTemplate`.
It is a platform-owned, cluster-scoped `ClusterRunnerTemplate`: the capability set below exceeds what a tenant should be able to self-author, and platform ownership is what enforces the "no privileged container" property (the namespace PSA level cannot; see [Why this matters for GAG](#why-this-matters-for-gag)).

```yaml
apiVersion: actions-gateway.com/v2beta1
kind: ClusterRunnerTemplate            # platform-owned golden template
metadata:
  name: kata-dind
spec:
  podTemplate:
    spec:
      runtimeClassName: kata           # the alias RuntimeClass above
      automountServiceAccountToken: false
      # dockerd as a NATIVE sidecar (restartPolicy: Always init container);
      # the runner container reaches it via DOCKER_HOST=tcp://localhost:2375.
      initContainers:
        - name: dind
          image: docker:28-dind
          restartPolicy: Always
          # Replace the image entrypoint with the six Kata setup steps —
          # see the next section and the reference implementation below.
          securityContext:
            privileged: false          # THE point of this architecture
            allowPrivilegeEscalation: false
            capabilities:
              drop: [ALL]
              add: [ ... ]             # the validated set — next section
      containers:
        - name: runner
          env:
            - name: DOCKER_HOST
              value: "tcp://localhost:2375"
```

The complete form (the raw block volume for `/var/lib/docker`, the six-step entrypoint, the validated capability set, the dockerd startup probe, and measured resource sizing) is the shipped library entry [`deploy/templates/kata-dind/template.yaml`](../../deploy/templates/kata-dind/template.yaml).
GAG's own e2e tenant consumes that same base and patches only its cluster specifics ([`deploy/dogfood-e2e/overlays/kata/`](../../deploy/dogfood-e2e/README.md)), so what you apply is what CI runs jobs on.
The single-pod (non-GAG) reference the spike validated is [`deploy/kata-ci/runner-pod.yaml`](../../deploy/kata-ci/runner-pod.yaml).

The AGC honours a template-set `runtimeClassName` and applies no override that strips it.
If `dockerd` still fails inside the guest, add one Linux capability at a time to the container's `securityContext.capabilities.add` and re-test — **never** reach for `privileged: true`.
Record the final minimal capability set for your runbook.

## What an unprivileged `dockerd` actually needs inside the guest

Dropping `privileged: true` does not come free — six things must be arranged that a privileged container gets implicitly.
All were validated live; the reference implementation is the `args:` block of [`deploy/kata-ci/runner-pod.yaml`](../../deploy/kata-ci/runner-pod.yaml).

1. **`/var/lib/docker` must be a raw block volume, not an `emptyDir`.** Kata surfaces an `emptyDir` as **virtiofs**, and Docker cannot run `overlay2` on it — it silently falls back to `vfs`.
   `kind` then switches its node snapshotter to `fuse-overlayfs`, which needs `/dev/fuse` (absent from the guest), and the inner kubelet never becomes healthy.
   Use a PVC with `volumeMode: Block` + `volumeDevices`, then `mkfs.ext4` it inside the guest.
   Beware: the `docker:dind` image declares `VOLUME /var/lib/docker` and Kata pre-mounts virtiofs there, so a "is it mounted?" check passes and silently skips your ext4 mount — test for the **device**.
   Also do **not** gate the `mkfs` on `blkid` (root-caused live under Q286): `docker:dind`'s `blkid` is **busybox** blkid, which exits 0 even on a blank device, so `blkid || mkfs` skips the format on every fresh volume and the mount fails `EINVAL`.
   Mount-first and `mkfs` on failure instead — the device is a disposable per-pod cache, so reformatting on any mount failure is safe.
2. **`/dev/kmsg` does not exist in the Kata guest**, and a nested kubelet requires it.
   `mknod /dev/kmsg c 1 11` (needs `CAP_MKNOD`).
3. **`/sys/fs/cgroup` is read-only** for a non-privileged container, so `runc` cannot create the nested container's cgroup.
   `mount -o remount,rw /sys/fs/cgroup` works with `CAP_SYS_ADMIN`.
   Under Kata that hierarchy is the **guest** kernel's — the remount grants nothing on the host.
   Under plain `runc` the same tree is the host's, which is exactly why classic DinD demands `privileged: true`.
4. **`/proc/sys` is read-only** likewise; Docker writes per-veth `net.ipv6.conf.<iface>.disable_ipv6`.
   Same remount, same reasoning.
5. **cgroup v2 nesting.** cgroup v2 forbids a cgroup from holding processes *and* delegating controllers to children, so systemd in a nested container cannot create `/init.scope` (`Structure needs cleaning`).
   Move the cgroup-namespace root's processes into a leaf, then populate `cgroup.subtree_control`.
   `docker:dind`'s own entrypoint does this — you only need it if you override `command:`.
6. **IPv6 is disabled in the guest**, yet `kind` creates its Docker network with `--ipv6`.
   Pre-create an IPv4-only bridge network named `kind`.

The validated capability set — `drop: [ALL]` plus:

```text
CHOWN DAC_OVERRIDE FSETID FOWNER MKNOD NET_RAW SETGID SETUID
SETFCAP SETPCAP NET_BIND_SERVICE SYS_CHROOT KILL AUDIT_WRITE   # Docker's defaults
SYS_ADMIN NET_ADMIN SYS_RESOURCE SYS_PTRACE                    # rootful dockerd + runc
```

One more rule, found live under Q286: a container **inside** the nested kind cluster can only gain capabilities present in this bounding set — nested `runc` fails with `unable to apply caps: operation not permitted` otherwise.
When a CI workload trips this, prefer **tightening the workload** (drop the cap it requests) over widening the set above, so the worker's capability floor stays as small as possible.
GAG's e2e suite hit it once: a dev-mode test Vault requested `IPC_LOCK`, which was dropped from the Vault pod (with `SKIP_SETCAP=true`) rather than added here — a dev-mode Vault never mlocks.
Only widen the bounding set for a workload that genuinely cannot drop the cap.

Two are easy to miss: **`FOWNER`** (image layer unpack `chmod`s files it does not own) and **`SYS_CHROOT`** (`runc` `setns()` into the container mount namespace).

## The security rationale

| Property | Privileged DinD | DinD under Kata |
|---|---|---|
| `securityProfile` required | `privileged` (platform-granted namespace label) | `privileged` label still (PSS baseline forbids the capability adds; PSA is not Kata-aware) — but **no privileged container** behind it |
| `privileged: true` container | Yes | No |
| Escape blast radius | The host node and, from there, other tenants | A throwaway guest VM kernel |
| Node `/proc`, `/sys`, devices, kernel | Exposed | Behind the VM boundary |
| Node metadata server (`169.254.169.254`) | Exposed | **Still exposed** — see below |

> ### Kata does not close the metadata-server path
>
> Kata isolates the **kernel**, not the **pod network**.
> Measured from inside a Kata micro-VM on GKE: `169.254.169.254` stayed reachable and `computeMetadata/v1/instance/service-accounts/default/token` returned **HTTP 200** — the node's GCE service-account token.
> A micro-VM around an untrusted CI runner that can still mint node credentials is not a boundary.
>
> **Enable Workload Identity.
> It is a prerequisite, not an enhancement.**
>
> ```bash
> gcloud container clusters update <cluster> --workload-pool=<project>.svc.id.goog
> gcloud container node-pools update <pool> --cluster=<cluster> \
>   --workload-metadata=GKE_METADATA        # recreates the nodes
> ```
>
> Re-probed with it on, the metadata server serves the workload-pool identity (`<project>.svc.id.goog`) instead of the node's service account.
> Also set `automountServiceAccountToken: false` on the runner pod so it carries no Kubernetes API token, and consider a NetworkPolicy denying egress to `169.254.169.254/32` as defence in depth.
>
> On a cluster whose CNI enforces egress NetworkPolicy, the tenant policies this system generates already close that path, so the defence-in-depth leg is one you inherit rather than one you write.
> It does not work by naming the address: NetworkPolicy is allowlist-only, and the metadata address sits inside the link-local block `169.254.0.0/16` the DNS rule has to admit for NodeLocal DNSCache.
> What keeps it unreachable is that the rule only admits port 53, and the metadata service does not answer there.
> Two consequences for an operator: on a CNI that does **not** enforce egress (kindnet, and any cluster without a policy plugin) you get none of this and Workload Identity is doing all the work, and if you edit the generated policies to widen the link-local rule beyond port 53 you reopen the path.
>
> On other clouds the equivalent controls are IMDSv2 with a hop limit of 1 plus a restrictive instance profile (AWS), or Azure AD Workload Identity with the IMDS endpoint blocked.

Kata advances GAG's [secure-by-default principle](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in) on the axis that matters: the workload that historically demanded a `privileged: true` container now runs without one, converting a kernel escape from node compromise into a discarded guest.
The namespace's PSA *label* stays `privileged` (the capability set exceeds PSS baseline and PSA cannot see the VM boundary), so the platform-owned `ClusterRunnerTemplate` — not the PSA level — is what pins the pod shape.
Privileged DinD remains documented as a last resort — and even then it should be *paired* with a sandbox runtime, which is the same mechanism described here applied on top of `privileged`; see [In-runner image builds — privileged DinD](in-runner-image-builds.md#approach-4--plain-privileged-dind-avoid-where-possible).

### What Kata does not buy you

Kata is one layer, not a security posture.
Four limits worth stating plainly before you conclude "Kata, therefore safe":

- **The capability set is close to privileged-equivalent.** An unprivileged Kata DinD runner still needs `SYS_ADMIN`, `NET_ADMIN`, `SYS_PTRACE`, `SYS_CHROOT`, and read-write remounts of `/sys/fs/cgroup` and `/proc/sys`.
  On an ordinary `runc` container that list is nearly `privileged: true`.
  It is safe here *only* because those capabilities act on the guest kernel.
  There is no defence in depth from the capability set — the entire argument rests on the VM boundary holding.
- **You trade one CVE class for another.** The Linux syscall interface is a large, well-trodden attack surface (`CVE-2019-5736` runc, `CVE-2022-0847` Dirty Pipe, the io_uring family).
  QEMU's device emulation is a much smaller and more exotic one, but it is not empty (`CVE-2019-14378`, the VENOM class).
  Kata makes escape *much harder*, not impossible.
  On GKE the hypervisor is itself running inside a GCE VM (nested virtualisation), so the stack is deeper and less exercised in production than Kata on bare metal.
- **Kata *without* Workload Identity can be worse than privileged DinD *with* compensating controls.** Both configurations let a compromised runner mint the node's service-account token over the pod network.
  The Kata one *feels* safe, so the metadata control is more likely to be skipped.
  Do not deploy this architecture without the Workload Identity step above.
- **GAG has proven the boundary exists, not that it is unbreakable.** Q226 verified the guest kernel differs from the node kernel and that the pod holds no host privilege.
  No breakout was attempted.
  Treat the containment claim as "designed and structurally sound", not "empirically tested against an exploit".

The honest recommendation: **prefer Kata over privileged DinD for untrusted code** — it is the only option here that puts a machine boundary around an inner Docker daemon, and it converts a kernel escape from node compromise into a discarded guest.
But deploy it *with* Workload Identity, `automountServiceAccountToken: false`, and ideally a NetworkPolicy denying egress to `169.254.169.254/32`.
Kata alone is not the control.

## Untrusted pull requests — the tight-egress posture

Kernel isolation is one half of running an external contributor's pull request.
The other half is egress: a micro-VM bounds what the job's code can do to the node, not what it can reach.
This page used to tell you to stop there and treat Kata as protection against a kernel escape rather than a licence to run untrusted code.
That caveat is retired.
The posture below is built, shipped as manifests, and measured on GAG's own dogfood cluster, where it carries every Kata end-to-end run.

**What it does.** A worker's whole reachable set becomes cluster DNS on 53, GitHub on 443, and the in-cluster registry mirrors on 5000.
The public internet, the upstream registries by their own hostnames, and the cloud metadata server all answer nothing.

**Why a mirror rather than an allowlist.** The registries a build pulls from are CDN-fronted, so a CIDR allowlist rots, and fully-qualified-domain-name policy is not enforceable on GKE Dataplane V2.
A host allowlist is also the wrong shape even where it is enforceable: permitting `docker.io` permits `docker.io/<anyone>/<anything>`, in both directions, so the exfiltration surface stays open.
The mirror is content-scoped instead.
Each instance is pinned to exactly one upstream in its pod spec, and refuses uploads, so there is one auditable chokepoint rather than five open hostnames.

### The four parts

| Part | Where it lives | What it does |
|---|---|---|
| The mirrors | [`deploy/registry-mirror/`](../../deploy/registry-mirror/README.md) | One [CNCF Distribution](https://github.com/distribution/distribution) instance in pull-through cache mode per upstream registry your jobs pull from |
| The mirror's policies | [`base/networkpolicy.yaml`](../../deploy/registry-mirror/base/networkpolicy.yaml) | Workers may reach mirror pods on 5000; mirror pods accept ingress from the tenant namespace and nothing else |
| The worker wiring | [`overlays/kata/mirror-wiring.yaml`](../../deploy/dogfood-e2e/overlays/kata/mirror-wiring.yaml) | Points the job's image clients at the mirrors, in the two forms those clients can read |
| The absence of an allow-all | your tenant namespace | No additive egress policy, so the gateway's default-deny is the whole story |

The first three are additive.
The fourth is the one that turns the recipe into enforcement, and it is a deletion rather than an object you apply.

### Choosing a mirror topology

One mirror set can serve every tenant, or each tenant can have its own.
[`deploy/registry-mirror/`](../../deploy/registry-mirror/README.md#two-topologies-one-shared-set-or-one-set-per-tenant) renders both, and the choice is a platform administrator's rather than a default this project can pick for you.
It turns on your tenant count, your disk budget, and whether your tenants are mutually hostile.

**A shared set is one line of difference.** The mirror-side ingress admits any namespace carrying the managed-tenant marker instead of one namespace named literally, and `kubectl apply -k deploy/registry-mirror/overlays/shared-tenants` renders it.
The worker-side egress policy sits in the tenant's own namespace and is per-tenant under either topology, so that half is unchanged.

**An isolated set costs a whole set per tenant.** Read off the shipped manifests, one set is 5 Deployments and 5 Services requesting 125m of CPU and 320Mi of memory, with limits of 2500m and 2560Mi; the persistent overlay adds 5 PVCs holding 50Gi, which the deployment README prices at about $5 a month.
Multiply all of it by tenant count.
The cache also stops being shared, so each tenant pulls `kindest/node` cold on its own and pays that upstream bandwidth separately.

**Where the cost starts to bite.** The ephemeral default is $0 at rest under either topology, so disk enters the arithmetic only if you chose persistence, which composes with either (`overlays/persistent` isolated, `overlays/shared-tenants-persistent` shared). There, isolation is 50Gi and roughly $5 a month per tenant: four tenants is 200Gi and about $20, ten is 500Gi and about $50.
Check the requests before the dollars, because they are the figure that has to fit a node pool you already sized: 125m of CPU and 320Mi of memory per tenant, and 2500m and 2560Mi of limit per tenant if the instances ever run hot together.
The repeated cold pulls are the third cost and the one this project has not measured per tenant, so size it from your own registry egress rather than from a number here.

**Where isolation stops being optional.** If your tenants must not learn what each other build, a shared set cannot give you that, and no amount of tuning makes it.
`GET /v2/_catalog` names every repository in the cache, answers 200 on the same port 5000 the worker policy admits, and needs one manifest fetch to list a repository.
That is a documented registry API endpoint rather than a side channel, so it is not closed by making a cache hit slower, and the one setting that looks as though it would close it does not: measured locally against `registry:3.1.1` at the pinned digest, run as a pull-through cache with the deployed proxy configuration, `catalog.maxentries=0` still answered with the repository listed.
That is the image's behaviour rather than a cluster reading; what has not been exercised anywhere is the join to the policy, that a worker reaches the endpoint on the port `e2e-mirror-egress` admits.
Tenants who are teams inside one organisation, already able to read each other's repositories, lose nothing to that.
Unrelated organisations on one cluster, or tenants held apart by contract or regulation, do.
Isolation also narrows the blast radius of a compromised mirror from every tenant to one.
[The multi-tenant goal](../plan/secure-multi-tenant-oss-ci.md#explicitly-out-of-scope-and-residual-risk-accepted) accepts that risk whole, and it does not arise on the dogfood cluster, which runs one tenant.

**What is still unmeasured, and why it does not hold up the choice.** Whether a cache hit is distinguishable from a miss *from inside a Kata guest*, across the bridge NAT, on this Deployment shape, is not measured.
A laptop measurement against the pinned image put blob hits at 10 to 70 ms against two cold misses of 637 and 419 ms, and left manifest hits and misses overlapping over ten repositories, which bounds that channel rather than measuring it where an attacker sits.
The guidance above does not rest on it: `/v2/_catalog` exposes the same repository list with no timing involved, so a refuted timing channel would not make a shared set private.
[Q1020](../queue/Q1020.md) holds the guest measurement.

### Wiring the job's image clients

No two image clients read the same configuration, so one endpoint set has to reach them four ways.
[`mirror-wiring.yaml`](../../deploy/dogfood-e2e/overlays/kata/mirror-wiring.yaml) holds it once, as a `daemon.json` and as an `<upstream>=<mirror>` map:

| Client | Reads | Covers |
|---|---|---|
| the inner `dockerd` | `daemon.json`, mounted into the DinD sidecar | every Docker Hub pull, digest-pinned ones included |
| `docker pull` of a non-Hub ref | the map, via a rewrite at the one chokepoint those pulls share | everything not on Docker Hub |
| helm's OCI client | the map, rewriting the chart ref and adding `--plain-http` | OCI chart pulls |
| buildkit | a generated `buildkitd.toml` off the map | a `Dockerfile`'s base images, which no build cache covers |

`dockerd`'s `registry-mirrors` setting mirrors Docker Hub and only Docker Hub, which is why the other upstreams need the rewrite rather than another entry in `daemon.json`.
The rewrite re-tags each image to the ref the caller asked for, so everything downstream of the pull is untouched.

Image identity survives the redirection.
Schema-2 and OCI digests are content addresses over bytes that carry no registry hostname, so a digest-pinned pull re-verifies client-side and a cosign signature still checks out.
The mirror is trusted for tag-to-digest resolution, exactly as the upstream registry already was.

### Adopting it

1. **Measure what your jobs actually fetch**, rather than assuming.
   The upstream set is the instance set, because proxy mode takes exactly one upstream per instance.
   GAG's own count went from four to five on a measurement.
2. **Render the mirrors** for your upstreams, retargeting the three cluster-specific values the [README](../../deploy/registry-mirror/README.md#adopting-this-outside-the-dogfood-cluster) names: your tenant namespace, the mirror namespace, and your storage class.
   With more than one tenant, pick a topology first ([above](#choosing-a-mirror-topology)); the retargeting is per tenant under the isolated one, and the per-tenant worker policy is needed under both.
3. **Wire the clients**, which means the ConfigMap above plus the two patches that mount it.
4. **Delete any allow-all egress policy** from the tenant namespace, and confirm it is gone from the live rules rather than only from your manifests.
   `kubectl apply -k` does not prune, so a policy whose manifest you deleted is still standing.

### Confirming it holds

Three readings, and none of them substitutes for another:

- **The mirrors serve.** [`e2e-mirror-validate.sh`](../../scripts/dogfood/e2e-mirror-validate.sh) checks each instance for readiness, `GET /v2/`, a real upstream manifest, an upload refused with 405, and no debug listener.
- **The job's pulls ride them.** [`e2e-mirror-hits.sh`](../../scripts/dogfood/e2e-mirror-hits.sh) reads each mirror's access log, which is the one place a pull that went upstream instead cannot appear.
  Take a baseline first: a count means nothing without one.
- **Nothing else is reachable.** [`egress-negatives.sh`](../../scripts/e2e/egress-negatives.sh) probes from inside the job, on the worker whose posture is being claimed, because a plain pod cannot answer whether policy still binds at the end of a path that leaves a micro-VM guest through a bridge NAT.

**Every negative is paired with a positive.** A battery of nothing-is-reachable checks passes identically when the pod has no network at all, so half of the eight checks are controls that must answer: the mirror over HTTP, GitHub, an upload the mirror refuses, and a `docker pull` through the mirror.
Only then does the silence of the other four mean the policy.
Those four probe three destinations, the upstream twice, by curl and by `docker pull`, because a client can hold a path its shell does not.
Run the negatives on **every** run rather than once: a policy that stops selecting the worker is invisible in a green suite.

### What this posture does not cover

- **The job still reaches GitHub**, which is what a runner is for.
  Data a job can read is data it can push to a repository it has a token for.
- **Cache misses reach upstream from the mirror pod**, which carries no workload label and so keeps the free egress a pull-through cache needs.
  What bounds it is the pinned upstream per instance, not a network rule.
- **The metadata server needs Workload Identity anyway.** The policy closes that path only because the DNS rule admits port 53 alone, and widening it reopens the path.
  Enable Workload Identity, as [the security rationale](#the-security-rationale) sets out.
- **A per-job record of which host each job reached is not this posture's to give**, and where that stands is tracked in the goal's Definition of Done rather than here.
  The other thing it asks for, a cache an untrusted job shares with another tenant, is now a decision rather than a gap: both topologies ship, the shared one exposes its repository list and the isolated one does not, and [Choosing a mirror topology](#choosing-a-mirror-topology) is where an administrator settles it.
  This posture is the network layer, not the whole story: the layer map and what each layer still owes are in [the secure multi-tenant OSS CI goal](../plan/secure-multi-tenant-oss-ci.md#definition-of-done).

**Measured on the dogfood cluster on 2026-08-28**: a green Kata run of 75 specs whose in-job negatives passed all eight checks, with the mirror battery at 25 of 25 and 178 content requests served across the five instances, on a tenant whose live rules carried zero allow-all.

## Caveats and limitations

- **Startup overhead is small — but the `overhead` accounting is not.** Measured on `c2-standard-4`: a Kata pod reached `Ready` in ~3 s vs ~1 s for `runc`, and `kind create cluster` took 58 s from a cold image cache (43 s warm) against a ~6 min ceiling.
  The bigger planning cost is the `RuntimeClass` `overhead` (160Mi / 250m **per pod**), which the scheduler reserves on top of the container's own requests — size nodes *and* the namespace `ResourceQuota` for it.
  The gateway charges it too: the AGC reads the `RuntimeClass` to fold `overhead.podFixed` into the worker footprint behind the `WorkerQuota` conditions and the pre-claim quota gate, so a Kata tenant can report quota pressure a `runc` tenant of the same container shape would not.
  That read needs the cluster-scoped `runtimeclasses` grant the chart ships; it is fail-open, so an AGC without it silently omits the overhead term.
  See [sizing the platform-owned `ResourceQuota`](resourcequota-sizing.md#pod-overhead-needs-a-cluster-scoped-read).
- **The per-worker block device is quota-charged too, and it fails quietly.** The reference shape's `/var/lib/docker` device is a generic ephemeral volume, so Kubernetes creates one real PVC per worker pod — charged against `persistentvolumeclaims`, `requests.storage`, and the `<class>.storageclass.storage.k8s.io/…` keys.
  At `maxWorkers: 4` that is `4` claims and `400Gi`.
  Unlike a CPU or memory shortfall, an exhausted storage quota does **not** reject the pod: the PVC is created after the pod is admitted, so the worker sits `Pending` on an unbound volume holding a job already claimed from GitHub.
  The AGC counts these keys in the worker footprint to refuse before claiming — see [the storage keys](resourcequota-sizing.md#step-3--the-storage-keys).
- **Nested-virt capacity can be scarce.** During validation, `n2-standard-4` and `n2d-standard-4` were both `ZONE_RESOURCE_POOL_EXHAUSTED` in `us-central1-a` while CPU quota sat at 0/200 — a stockout, not a quota problem (a plain non-nested-virt `n2` failed too, so nested virt itself does not narrow the pool).
  `c2`/`c2d` worked.
  Check the **per-family** regional quota: `C2_CPUS` defaults to 8 on a fresh project, which is one node of an 8-vCPU shape; `N2_CPUS` defaults to 200.

  **`c2d` no longer takes `--enable-nested-virtualization`.** As of 2026-08-02 GCP rejects the create and names the families that can, which is the list in [the machine-family row](#prerequisite--nested-virtualization-nodes): no `C2D`, no `N2D`.
  Whether support was withdrawn or the observation above was of a pool created without the flag is unresolved; take the API's rejection as current.
- **A node-pool stockout wedges the cluster.** A failing `CREATE_NODE_POOL` operation holds a cluster-level lock (`Cluster is running incompatible operation`) that blocks even deleting the pool, for tens of minutes.
  Prefer creating the nested-virt pool as the cluster's *initial* pool (`gcloud container clusters create --enable-nested-virtualization`).
- **`kata-deploy` is a DaemonSet, so it self-heals.** Recreating the node pool (for example to flip `--workload-metadata`) reinstalls Kata automatically and the `RuntimeClass` survives.
- **Not all kernel features pass through.** Workloads needing host kernel modules, specific `/dev` devices, or GPU passthrough need extra Kata configuration.
  GPU passthrough in particular still wants bare metal or dedicated instances, but *not* because the cloud GPU families lack nested virtualization (A2, A3, and G2 have it).
  It is because NVIDIA's Kata path needs BIOS-level ACS and IOMMU, no NVIDIA driver bound on the host, and a whole GPU per guest.
  See [GPU and accelerated CI](../plan/gpu-and-accelerated-ci.md#the-collision-with-the-security-goal).
- **Run `dockerd` inside the runner container, not as a regular sidecar.** The pattern above keeps the daemon a nested process of the single `runner` container, so the pod reaps cleanly when the job ends.
  If you instead split the daemon into a separate sidecar container, declare it as a **native sidecar** (`restartPolicy: Always` init container) — a regular sidecar runs forever and keeps the worker pod from reaping, stranding the runner slot.
  See [In-runner image builds § Sidecar containers must be native sidecars](in-runner-image-builds.md#sidecar-containers-must-be-native-sidecars-q249).
- **Validated as an architecture, and GAG's own CI runs on it.** The unprivileged `dockerd` + `kind` path was proven end-to-end on GKE (`1.35.5-gke.1241004`, Ubuntu 24.04, `c2-standard-4` nested-virt, Kata 3.32.0/QEMU) — see [Kata Containers on GKE](../plan/archive/kata-on-gke.md) for the evidence.
  GAG's dogfood e2e ships the Kata worker shape as [`deploy/dogfood-e2e/overlays/kata`](../../deploy/dogfood-e2e/overlays/kata) and selects it **by default**; `E2E_VARIANT=dind scripts/dogfood/e2e-start.sh` is the explicit fallback.
  No bundled all-in-one runner image is needed: the daemon is a stock `docker:28-dind` native sidecar with a six-step entrypoint, and the toolchain rides the regular runner container.
  A full `make e2e` run is green through that overlay.
  Confirm the steps on your own cluster before cutting over privileged workloads.
- **Kata alone is not the untrusted-PR posture, and the rest of it is a deliberate build.** The micro-VM bounds the guest kernel and narrows no egress, so a Kata worker on a permissive egress policy is protected against a kernel escape and nothing more.
  What closes the gap is the in-cluster registry mirror plus an egress policy scoped to it, GitHub, and DNS, which ships as manifests and is measured on GAG's own cluster: [the tight-egress posture](#untrusted-pull-requests--the-tight-egress-posture).
  Adopt that before running an external contributor's pull request on this shape.

## Related

- [`deploy/registry-mirror/`](../../deploy/registry-mirror/README.md): the pull-through cache manifests behind [the tight-egress posture](#untrusted-pull-requests--the-tight-egress-posture), with the storage options and the values to retarget.
- [Runner template library](runner-template-library.md): the shipped `kata-dind` entry this page describes, plus its two siblings and how to fork one.
- [In-runner image builds](in-runner-image-builds.md) — pick a build approach (BuildKit rootless, Kaniko, Sysbox, Kata, privileged DinD) and the `securityProfile` each needs.
- [Kata-on-GKE spike runbook](kata-ci-spike-runbook.md) — executable go/no-go steps for the unprivileged `dockerd` + `kind` runner.
- [Kata Containers on GKE](../plan/archive/kata-on-gke.md) — design rationale, the options rejected (Sysbox, rootless, kindbox), and the provider-agnostic reference architecture.
- [Appendix B — Worker isolation](../design/appendix-b-worker-isolation.md) — `runc` vs gVisor vs Kata sandbox-runtime trade-offs.
- [Security § 5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in) — the authoritative `securityProfile` model.
