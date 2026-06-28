# Kata Containers CI artifacts (Q226 spike)

Manifests and config for running GitHub Actions self-hosted runners that need
Docker-in-runner (for `kind`) **without** `privileged: true`, by isolating each
runner pod in a Kata Containers micro-VM. Motivation, options analysis, and the
six spike acceptance criteria live in
[docs/plan/kata-on-gke.md](../../docs/plan/kata-on-gke.md).

> **Status: offline-authored, not yet live-validated.** These artifacts are
> statically validated (yamllint + kubeconform + shellcheck) but the end-to-end
> go/no-go is a separate human step that needs a GKE cluster with nested
> virtualization. Follow
> [docs/operations/kata-ci-spike-runbook.md](../../docs/operations/kata-ci-spike-runbook.md)
> for that. Items marked **VERIFY AT SPIKE TIME** in the files below must be
> confirmed against live behaviour and current upstream/cloud docs.

## Contents

| File | Purpose |
|---|---|
| [`../../scripts/kata-node-pool.sh`](../../scripts/kata-node-pool.sh) | Provision (or `DRY_RUN=1` print) a GKE Standard node pool with nested virtualization on a nested-virt-capable machine family (n2/n2d/c2/c2d). |
| [`kata-deploy.yaml`](kata-deploy.yaml) | The kata-deploy installer DaemonSet + scoped RBAC, pinned by image tag. Installs the Kata runtime onto the labelled node pool. Privileged by necessity — it is the trusted, operator-run installer, not a workload. |
| [`runtimeclass.yaml`](runtimeclass.yaml) | The `kata` and `kata-qemu` RuntimeClass objects pods opt into. |
| [`runner-pod.yaml`](runner-pod.yaml) | The unprivileged runner pod: `runtimeClassName: kata`, `privileged: false`, dropped capabilities, runs dockerd + kind inside the guest VM. The security crux of the spike. |

## Static validation

These manifests and the provisioning script are wired into the repo's existing
gates — no live cluster required:

- `make manifest-validate` — yamllint + kubeconform schema-check of the manifests.
- `make shellcheck` (part of `make check`) — lints `scripts/kata-node-pool.sh`.

## Apply order (live — needs a cluster + GCP credentials)

1. `scripts/kata-node-pool.sh` — create the nested-virt node pool.
2. `kubectl apply -f kata-deploy.yaml` — install the Kata runtime; wait for the
   DaemonSet to label the nodes.
3. `kubectl apply -f runtimeclass.yaml` — register the RuntimeClasses.
4. `kubectl apply -f runner-pod.yaml` — schedule the unprivileged runner.

The runbook covers each step with expected output and the acceptance criteria.
