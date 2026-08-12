# Appendix B — Worker Isolation Runtime (Optional)

← [Appendix A](appendix-a-capacity-slos.md) | [Back to index](README.md) | Next: [Appendix C — AI-Assisted Implementation →](appendix-c-ai-implementation.md)

---

Worker pods execute arbitrary workflow code, which is untrusted by definition.
The system functions correctly on the default `runc` container runtime, but operators concerned about kernel-level container-escape attacks have the option of running worker pods under a sandboxed runtime by setting a `RuntimeClass` on the worker `PodTemplate`.

**This is optional.** Sandboxed runtimes add operational complexity (additional node configuration, larger pod startup latency, occasional kernel-feature incompatibilities) that may not be justified for every deployment.
Use this appendix to decide whether to opt in.

> **Validation status.** The AGC honours a `runtimeClassName` set on the worker `PodTemplate` and applies no override that would strip it.
>
> **Kata Containers is live-validated (Q226/Q286).** GAG's own end-to-end CI creates a `kind` cluster inside a worker pod under `runtimeClassName: kata` with zero `privileged: true`, on a nested-virtualization GKE node pool, and that is the dogfood default.
> The validated node prerequisites, the capability set an unprivileged `dockerd` needs, and the boundary's real limits are in [Running DinD workloads under Kata](../operations/kata-dind-workloads.md).
>
> **gVisor has not been exercised on a real cluster (Q15).** Validating it needs a runsc-enabled node pool; operators selecting `runsc` should validate the full job path on their own cluster before relying on it for isolation.

---

## B.1. Threat Coverage

The escape vectors covered by sandboxed runtimes are kernel-level: shared-kernel exploits (e.g., `dirtyc0w`-class vulnerabilities), syscall-table abuse, and privilege escalation through container-runtime bugs.
They do **not** cover the threats that ordinary Pod Security Standards already mitigate: dropped capabilities, non-root user, read-only root filesystem, seccomp profiles.
Those should be enforced *regardless* of the runtime choice.

| Threat | `runc` (default) | gVisor | Kata Containers |
| --- | --- | --- | --- |
| Container-to-host kernel exploit | Direct kernel surface | Sandboxed user-space kernel (Sentry) | Hardware-virtualized guest kernel |
| Syscall surface exposed to workload | Full host kernel | ~250 syscalls, intercepted | Full guest kernel (isolated VM) |
| Cross-pod kernel-level interference | Shared kernel | Per-pod Sentry | Per-pod VM |
| Pod startup latency overhead | Baseline | Sentry process start | Guest kernel boot (see [B.2](#b2-operational-cost)) |
| Compatible workflows | All | Most (some syscalls unimplemented) | All |
| GPU / device passthrough | Native | Limited | Possible but complex |

---

## B.2. Operational Cost

| Concern | gVisor | Kata Containers |
| --- | --- | --- |
| Node-level installation | runsc binary + containerd plugin | Kata runtime + nested-virt-capable kernel |
| Cloud compatibility | Most clouds support runsc on standard nodes | Requires nested-virt or bare-metal nodes (e.g. AWS bare-metal, GCP nested-virt families) |
| Per-pod memory overhead | Sentry process | Guest kernel + agent; the shipped `kata` RuntimeClass reserves `160Mi` |
| Per-pod CPU overhead | Syscall interception, so syscall-heavy jobs pay most and compute-bound jobs pay little | Near-native in steady state; the RuntimeClass reserves `250m`, and the real cost is at boot and on I/O crossing the virtio boundary |
| Debugging | `kubectl exec` works; some debugger tools incompatible | `kubectl exec` works through Kata agent; kernel-debug tools constrained |

**How to read these tables.** Neither is a benchmark of this system.
No rigorous, current head-to-head of `runc`, gVisor and Kata is cited here, and the figures that circulate are taken on some other kernel, hypervisor and storage path than the one an operator will run.
Nothing in the gVisor column has been exercised on a cluster here at all (Q15, above).
Read both tables as mechanism, and measure your own configuration before budgeting against [Appendix A](appendix-a-capacity-slos.md)'s startup SLO.

One figure is ours.
On the dogfood configuration (GKE `1.35.5-gke.1241004`, Ubuntu 24.04, `c2-standard-4` nested-virt, Kata 3.32.0 with QEMU) the Q226 spike measured the micro-VM boot at roughly 2s over `runc`, with `kind create cluster` at 58s cold and 43s warm inside the guest ([`deploy/kata-ci/runner-pod.yaml`](../../deploy/kata-ci/runner-pod.yaml)).
That is a QEMU number, not a property of Kata: the hypervisor is pluggable, and the lighter ones (Cloud Hypervisor, Firecracker) exist because QEMU's device model is the expensive part of the boot.
The repo ships a stable `kata` alias onto the `kata-qemu` handler so the hypervisor can be retargeted without editing pod specs, and that object carries the pod overhead the scheduler reserves ([`deploy/kata-ci/runtimeclass.yaml`](../../deploy/kata-ci/runtimeclass.yaml)).

For a CI workload the runtime is rarely the dominant cost anyway.
How the sandbox surfaces the worker's writable storage swings image pull and build time more than the boot does, and the same decision governs whether the workload runs at all: Kata surfaces an `emptyDir` as virtiofs, on which Docker cannot use `overlay2` and silently falls back to `vfs`, while a raw block device passed into the guest restores native `overlay2` ([Running DinD workloads under Kata](../operations/kata-dind-workloads.md)).
Settle the storage path first; the runtime's boot cost is the smaller question.

---

## B.3. When to Opt In

**Strong reasons to enable a sandboxed runtime:**

* The cluster hosts both first-party and third-party workflow code (e.g.
  PRs from external contributors).
* The compliance posture requires hardware or hypervisor-level workload isolation.
* A previous incident or pen-test surfaced a kernel-level concern.

**Reasonable reasons to stay on `runc`:**

* The cluster runs only first-party code from trusted contributors.
* The cluster has no nested-virt support and the operational cost of installing gVisor outweighs the benefit.
* Pod-startup latency is at the SLO ceiling already (see [Appendix A](appendix-a-capacity-slos.md)).

---

## B.4. How to Enable

Per-RunnerGroup, set the `RuntimeClassName` field on the `WorkerPodTemplate`:

```yaml
spec:
  runnerGroups:
  - runnerLabels: [sandboxed, self-hosted]   # first label → derived RunnerGroup name
    podTemplate:
      spec:
        runtimeClassName: gvisor   # or kata-containers
        containers:
        - name: runner
          resources:
            requests: { cpu: "1", memory: "2Gi" }
```

The cluster must have the corresponding `RuntimeClass` object installed and at least one node carrying the appropriate handler.
The Gateway Manager Controller (GMC) does not install RuntimeClasses or runtime handlers — that is a cluster-admin operation.

## B.5. Sidecar containers and pod reaping (Q249)

A worker pod is one runner container plus, optionally, sidecars.
A Kubernetes pod terminates only when **every** regular `spec.containers[]` entry has exited, so a sidecar that runs for the life of the job — a `docker:dind` daemon, a rootless BuildKit sidecar, a metrics agent — keeps the pod alive after the runner container finishes if it is declared as a **regular container**.
The pod lingers, and because GAG counts a pod as an active session until it reaps, the runner slot stays charged against the `RunnerSet`'s `maxWorkers` — the same stranding class as [Q247](../STATUS.md) (a pod left behind after its job is gone).
Under a concurrent matrix the pool collapses to the pods that happened to reap.

GAG does **not** solve this with a bespoke reaper — it relies on the upstream mechanism.
A **native sidecar** ([KEP-753](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/753-sidecar-containers): a `restartPolicy: Always` init container, beta/on-by-default in Kubernetes 1.29, GA in 1.33) is torn down by the kubelet when the main container exits, so the pod completes on its own.
Operators declare long-running build sidecars that way.

Because nothing in a pod spec declares that a container "runs forever" (`dockerd` never exits; `busybox true` exits at once), the detection is necessarily a **heuristic** — "a regular, non-runner container may block reaping."
That is why every outlet is a **non-blocking warning**, never a rejection: an admission `Warning:`, the advisory `PossibleReapBlockingSidecar` condition on the `RunnerSet`, and the `actions_gateway_reap_blocking_sidecar_templates` gauge.
A per-template `actions-gateway.com/self-exiting-sidecars` name-list annotation acknowledges sidecars the operator asserts exit cleanly, silencing all three for the named containers only (a name-list, not a boolean, so a newly added footgun still warns).
The operator-facing how-to lives in [in-runner image builds § Sidecar containers must be native sidecars](../operations/in-runner-image-builds.md#sidecar-containers-must-be-native-sidecars-q249).

---

← [Appendix A](appendix-a-capacity-slos.md) | [Back to index](README.md) | Next: [Appendix C — AI-Assisted Implementation →](appendix-c-ai-implementation.md)
