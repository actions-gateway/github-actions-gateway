# test/

Shared test fixtures and configuration used by integration and end-to-end tests. Per-module unit tests live alongside the code they cover, not here.

## Contents

| Path | Purpose |
|---|---|
| [fakegithub/](fakegithub/) | Deployable HTTP stub implementing the GitHub App token exchange and the Actions broker v2 protocol. Lets the AGC start and process jobs in Tier B e2e tests without real GitHub credentials. Jobs are injected via a pod-local control API. |
| [kind-config-1worker.yaml](kind-config-1worker.yaml) | `kind` cluster config with one worker node. Default for Tier A specs. |
| [kind-config-2worker.yaml](kind-config-2worker.yaml) | `kind` cluster config with two worker nodes. Used by specs that need scheduling across nodes. |

## Test tiers

| Tier | Where it runs | What it proves | Reference |
|---|---|---|---|
| Unit | `go test` per module | Pure-Go logic, no Kubernetes API | [docs/development/testing.md](../docs/development/testing.md) |
| envtest | Per-controller suites | Reconciler behaviour against a real apiserver + etcd, no kubelet | [docs/development/testing.md](../docs/development/testing.md) |
| Tier A (kind) | Local `kind` cluster | GMC infrastructure: real CNI, kube-proxy DNAT, kubelet image-pull, NetworkPolicy, TLS-over-tunnel | [docs/design/07-test-plan.md §7.3](../docs/design/07-test-plan.md#73-end-to-end-tests) |
| Tier B (kind + fakegithub) | Local `kind` cluster | AGC lifecycle against the in-cluster `fakegithub/` server — no real GitHub quota burned | [docs/design/07-test-plan.md §7.3](../docs/design/07-test-plan.md#73-end-to-end-tests) |
| Tier C (live) | Real cluster + real GitHub App | Real workflow dispatch end-to-end against `actions-gateway-test` | [docs/design/07-test-plan.md §7.3](../docs/design/07-test-plan.md#73-end-to-end-tests) |

For operational details (Make targets, running a single spec, the `multi-node` label and `SUITE` filter, Tier C env vars), see [docs/development/testing.md](../docs/development/testing.md). For iterating on Tier A/B locally (image-tag caching, distroless debugging, sub-minute inner loop), see [docs/development/kind-iteration.md](../docs/development/kind-iteration.md).

**Pick the right tier for the bug class.** Unit/envtest can't observe behaviours that emerge from real CNI, DNAT, kubelet, or TLS — when a change crosses one of those boundaries, only Tier A proves it.
