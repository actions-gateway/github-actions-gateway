# Kata variant (planned)

The Kata Containers isolation variant lands here as a sibling overlay to
[`../dind`](../dind): `kind` in a KVM micro-VM at **`baseline`** security profile,
**no privileged pod**. Tracked as [Q226](../../../../docs/STATUS.md#Q226) /
[Kata-on-GKE plan](../../../../docs/plan/kata-on-gke.md).

It reuses the same [`../../base`](../../base); the delta vs `dind/` is the
worker-isolation mechanism only:

- a nested-virt (N2) node pool + the Kata DaemonSet + a `kata-qemu` `RuntimeClass`
  (cluster infra — set up by the setup script, not kustomize);
- a `ClusterRunnerTemplate` with `runtimeClassName: kata-qemu` and an
  **unprivileged** DinD sidecar (the micro-VM is the isolation boundary);
- the namespace staying at `baseline` (no privileged profile, no eligibility label);
- optionally, a tighter egress policy paired with an in-cluster mirror.

Keeping the two overlays side by side is the point: `diff -r overlays/dind
overlays/kata` (or a diff of the rendered `kustomize build` output) is exactly the
security/complexity tradeoff between them — see [../../README.md](../../README.md).
