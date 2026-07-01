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
| Node-pool flag | Create the pool with `--enable-nested-virtualization`. |
| Node label | Label eligible nodes (e.g. `katacontainers.io/kata-runtime=true`) so the Kata install and the worker pods schedule only there. |

The repo ships a parameterized provisioning script —
[`scripts/kata-node-pool.sh`](../../scripts/kata-node-pool.sh) — that wraps
the `gcloud container node-pools create` call with these flags. Preview it
with `DRY_RUN=1` before spending cloud time; see
[spike runbook step 0–1](kata-ci-spike-runbook.md#step-0--preview-the-provisioning-command--offline).

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

1. **Install the Kata runtime on the labelled nodes.** The upstream
   `kata-deploy` DaemonSet drops the Kata binaries, the QEMU hypervisor,
   the guest kernel/image, and the containerd runtime handler onto each
   node it lands on. Scope it to the nested-virt nodes with a node selector
   on `katacontainers.io/kata-runtime=true` so it never touches pools that
   lack `/dev/kvm`.

2. **Register the `RuntimeClass`.** Create a `kata-qemu` `RuntimeClass`
   whose `handler` matches the one `kata-deploy` registered, and pin it to
   the same nodes with `scheduling.nodeSelector` so pods that select it are
   placed where the runtime exists:

   ```yaml
   apiVersion: node.k8s.io/v1
   kind: RuntimeClass
   metadata:
     name: kata-qemu
   handler: kata-qemu            # must match the kata-deploy handler
   scheduling:
     nodeSelector:
       katacontainers.io/kata-runtime: "true"
   ```

The repo's [`deploy/kata-ci/`](../../deploy/kata-ci/) directory carries
ready-to-apply `kata-deploy` and `RuntimeClass` manifests; the
[spike runbook step 2](kata-ci-spike-runbook.md#step-2--install-kata--start-the-unprivileged-runner--live)
walks the apply and rollout. Confirm the class exists before configuring
workers:

```bash
kubectl get runtimeclass kata-qemu
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
    - name: kata-builders
      runnerLabels: ["self-hosted", "kata"]
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

## The security rationale

| Property | Privileged DinD | DinD under Kata |
|---|---|---|
| `securityProfile` required | `privileged` (platform-granted namespace label) | `baseline` (the default) |
| `privileged: true` container | Yes | No |
| Escape blast radius | The host node and, from there, other tenants | A throwaway guest VM kernel |
| Node metadata-server / `/proc` / `/sys` reach | Exposed | Behind the VM boundary |

Kata satisfies GAG's
[secure-by-default principle](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in):
the workload that historically demanded the least-restrictive profile now
runs at the default profile. Privileged DinD remains documented as a last
resort — and even then it should be *paired* with a sandbox runtime, which
is the same mechanism described here applied on top of `privileged`; see
[In-runner image builds — privileged DinD](in-runner-image-builds.md#approach-4--plain-privileged-dind-avoid-where-possible).

## Caveats and limitations

- **Startup overhead.** Booting a micro-VM adds seconds to pod start. GAG's
  spike budgets a `kind create cluster` inside Kata at ≤ 3× the bare
  baseline; measure on your own nodes and set CI timeouts accordingly.
- **Not all kernel features pass through.** Workloads needing host kernel
  modules, specific `/dev` devices, or GPU passthrough need extra Kata
  configuration (and, for GPU, bare-metal or dedicated instances — the
  cloud GPU families lack nested virtualization).
- **Validate before relying on it for production CI.** The end-to-end Kata
  path on GKE is staged through the
  [spike runbook](kata-ci-spike-runbook.md) and has not yet been run live
  against a cluster (status in
  [Kata Containers on GKE](../plan/kata-on-gke.md)). Treat the steps here
  as the configuration contract; confirm them on your cluster before
  cutting over privileged workloads.

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
