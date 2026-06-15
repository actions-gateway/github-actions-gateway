# Appendix B — Worker Isolation Runtime (Optional)

← [Appendix A](appendix-a-capacity-slos.md) | [Back to index](README.md) | Next: [Appendix C — AI-Assisted Implementation →](appendix-c-ai-implementation.md)

---

Worker pods execute arbitrary workflow code, which is untrusted by definition. The system functions correctly on the default `runc` container runtime, but operators concerned about kernel-level container-escape attacks have the option of running worker pods under a sandboxed runtime by setting a `RuntimeClass` on the worker `PodTemplate`.

**This is optional.** Sandboxed runtimes add operational complexity (additional node configuration, larger pod startup latency, occasional kernel-feature incompatibilities) that may not be justified for every deployment. Use this appendix to decide whether to opt in.

> **Validation status (as of 1.0).** This path is **documented and supported in the spec** — the AGC honours a `runtimeClassName` set on the worker `PodTemplate` and applies no override that would strip it — but it has **not been exercised on a real cluster** with gVisor or Kata installed. Validating it requires a cluster with the runtime handler and a nested-virt-capable / runsc-enabled node pool, which is deferred post-1.0 (Q15). Operators enabling it should validate the full job path on their own cluster before relying on it for isolation.

---

## B.1. Threat Coverage

The escape vectors covered by sandboxed runtimes are kernel-level: shared-kernel exploits (e.g., `dirtyc0w`-class vulnerabilities), syscall-table abuse, and privilege escalation through container-runtime bugs. They do **not** cover the threats that ordinary Pod Security Standards already mitigate: dropped capabilities, non-root user, read-only root filesystem, seccomp profiles. Those should be enforced *regardless* of the runtime choice.

| Threat | `runc` (default) | gVisor | Kata Containers |
| --- | --- | --- | --- |
| Container-to-host kernel exploit | Direct kernel surface | Sandboxed user-space kernel (Sentry) | Hardware-virtualized guest kernel |
| Syscall surface exposed to workload | Full host kernel | ~250 syscalls, intercepted | Full guest kernel (isolated VM) |
| Cross-pod kernel-level interference | Shared kernel | Per-pod Sentry | Per-pod VM |
| Pod startup latency overhead | Baseline | + 50–200ms | + 1–3s (VM boot) |
| Compatible workflows | All | Most (some syscalls unimplemented) | All |
| GPU / device passthrough | Native | Limited | Possible but complex |

---

## B.2. Operational Cost

| Concern | gVisor | Kata Containers |
| --- | --- | --- |
| Node-level installation | runsc binary + containerd plugin | Kata runtime + nested-virt-capable kernel |
| Cloud compatibility | Most clouds support runsc on standard nodes | Requires nested-virt or bare-metal nodes (e.g. AWS bare-metal, GCP nested-virt families) |
| Per-pod memory overhead | ~10–30 MiB (Sentry process) | ~50–150 MiB (guest kernel + agent) |
| Per-pod CPU overhead | ~3–10% syscall-heavy, near 0% compute-heavy | ~1–5% in steady state, larger startup cost |
| Debugging | `kubectl exec` works; some debugger tools incompatible | `kubectl exec` works through Kata agent; kernel-debug tools constrained |

---

## B.3. When to Opt In

**Strong reasons to enable a sandboxed runtime:**

* The cluster hosts both first-party and third-party workflow code (e.g. PRs from external contributors).
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
  - name: untrusted-prs
    runnerLabels: [self-hosted, sandboxed]
    podTemplate:
      spec:
        runtimeClassName: gvisor   # or kata-containers
        containers:
        - name: runner
          resources:
            requests: { cpu: "1", memory: "2Gi" }
```

The cluster must have the corresponding `RuntimeClass` object installed and at least one node carrying the appropriate handler. The Gateway Manager Controller (GMC) does not install RuntimeClasses or runtime handlers — that is a cluster-admin operation.

---

← [Appendix A](appendix-a-capacity-slos.md) | [Back to index](README.md) | Next: [Appendix C — AI-Assisted Implementation →](appendix-c-ai-implementation.md)
