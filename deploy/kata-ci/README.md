# Kata Containers CI artifacts (Q226)

Manifests and config for running GitHub Actions self-hosted runners that need Docker-in-runner (for `kind`) **without** `privileged: true`, by isolating each runner pod in a Kata Containers micro-VM.
Motivation, the options analysis, and the measured results live in [docs/plan/archive/kata-on-gke.md](../../docs/plan/archive/kata-on-gke.md).

> **Status: live-validated.** `kind create cluster` was proven to run inside a non-privileged pod with `runtimeClassName: kata` on GKE (`1.35.5-gke.1241004`, Ubuntu 24.04 / containerd 2.1.5, `c2-standard-4` with nested virtualization, Kata 3.32.0 / QEMU).
> Node kernel `6.8.0-1054-gke` vs guest kernel `6.18.35` confirms a real VM boundary. `kind create cluster` took 58 s from a cold image cache.
>
> **Kata does not close the cloud metadata-server path.** Workload Identity (`--workload-metadata=GKE_METADATA`) is a prerequisite of this architecture, not an optional extra.
> See [the security rationale](../../docs/operations/kata-dind-workloads.md#the-security-rationale).

## Contents

| File | Purpose |
|---|---|
| [`../../scripts/dev/kata-node-pool.sh`](../../scripts/dev/kata-node-pool.sh) | Provision (or `DRY_RUN=1` print) a GKE node pool with nested virtualization on a supported machine family (n2/n2d/c2/c2d). |
| [`kata-values.yaml`](kata-values.yaml) | Helm values for upstream's `kata-deploy` **OCI chart** — the canonical installer. (Kata no longer ships raw `kata-deploy.yaml`/`kata-rbac.yaml`; those release-asset URLs 404.) |
| [`runtimeclass.yaml`](runtimeclass.yaml) | The `kata` alias RuntimeClass. The chart owns `kata-qemu`; we do not redeclare it. |
| [`runner-pod.yaml`](runner-pod.yaml) | The unprivileged runner: `runtimeClassName: kata`, `privileged: false`, `drop: [ALL]`, dockerd + kind inside the guest VM. The security crux. Its `args:` block is the reference implementation of the six setup steps an unprivileged dockerd needs. |

## Two node labels, and why they must differ

- `gag.dev/kata-ci=true` — set by **you** on the node pool.
  Scopes *where the installer runs*.
- `katacontainers.io/kata-runtime=true` — set by **kata-deploy**, after the runtime is installed.
  Scopes *where Kata pods may schedule* (the RuntimeClass `scheduling.nodeSelector`).

Using one label for both lets a Kata pod land on a node whose runtime does not exist yet.

## Static validation

- `make manifest-validate` — yamllint over `deploy/kata-ci/` plus kubeconform schema-check of `runtimeclass.yaml` and `runner-pod.yaml`. `kata-values.yaml` is a Helm *values* file, not a manifest, so it is linted but not schema-checked.
- `make shellcheck` (part of `make check`) — lints `scripts/dev/kata-node-pool.sh`.

## Apply order (live — needs a nested-virt cluster)

1. Create the cluster/pool with `--enable-nested-virtualization` **and** `--workload-metadata=GKE_METADATA`.
   Verify `/dev/kvm` on a node.
2. `helm install kata-deploy oci://quay.io/kata-containers/kata-deploy-charts/kata-deploy \` `--version 3.32.0 -n kube-system -f deploy/kata-ci/kata-values.yaml --wait`
3. `kubectl apply -f runtimeclass.yaml` — register the `kata` alias.
4. `kubectl apply -f runner-pod.yaml` — PVC + the unprivileged runner.

[The runbook](../../docs/operations/kata-ci-spike-runbook.md) walks each step with expected output and the verification commands.
