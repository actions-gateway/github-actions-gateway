# test/

Shared test fixtures and configuration used by integration and end-to-end tests.
Per-module unit tests live alongside the code they cover, not here.

## Contents

| Path | Purpose |
|---|---|
| [fakegithub/](fakegithub/) | Deployable HTTP stub implementing the GitHub App token exchange and the Actions broker v2 protocol. Lets the AGC start and process jobs in fake-GitHub e2e tests without real GitHub credentials. Jobs are injected via a pod-local control API. |
| [kind-config-1worker.yaml](kind-config-1worker.yaml) | `kind` cluster config with one worker node. Default for cluster-only specs. |
| [kind-config-2worker.yaml](kind-config-2worker.yaml) | `kind` cluster config with two worker nodes. Used by specs that need scheduling across nodes. |
| [autoscaler/](autoscaler/) | Manifests for the live-autoscaler drift gate: a real upstream cluster-autoscaler on its kwok cloud provider, plus the fake node groups it scales. Applied by [`scripts/e2e/autoscaler-cluster.sh`](../scripts/e2e/autoscaler-cluster.sh) into a cluster of its own — a live autoscaler adding and removing nodes underneath the e2e suite would perturb every spec in it. |

## Test tiers

| Tier | Where it runs | What it proves | Reference |
|---|---|---|---|
| Unit | `go test` per module | Pure-Go logic, no Kubernetes API | [docs/development/testing.md](../docs/development/testing.md) |
| envtest | Per-controller suites | Reconciler behaviour against a real apiserver + etcd, no kubelet | [docs/development/testing.md](../docs/development/testing.md) |
| cluster-only (kind) | Local `kind` cluster | GMC infrastructure: real CNI, kube-proxy DNAT, kubelet image-pull, NetworkPolicy, TLS-over-tunnel | [docs/design/07-test-plan.md §7.3](../docs/design/07-test-plan.md#73-end-to-end-tests) |
| fake-GitHub (kind + fakegithub) | Local `kind` cluster | AGC lifecycle against the in-cluster `fakegithub/` server — no real GitHub quota burned | [docs/design/07-test-plan.md §7.3](../docs/design/07-test-plan.md#73-end-to-end-tests) |
| live-GitHub | Real cluster + real GitHub App | Real workflow dispatch end-to-end against `actions-gateway-test` | [docs/design/07-test-plan.md §7.3](../docs/design/07-test-plan.md#73-end-to-end-tests) |
| Live autoscaler (kind + kwok) | Its own `kind` cluster | That an **upstream** vocabulary we fail open on is still the one we recognize — the class no recorded sample can observe, because a reword there leaves every test green | [docs/development/testing.md](../docs/development/testing.md#the-live-autoscaler-drift-gate) |

For operational details (Make targets, running a single spec, the `multi-node` label and `SUITE` filter, live-GitHub env vars), see [docs/development/testing.md](../docs/development/testing.md).
For iterating on cluster-only/fake-GitHub locally (image-tag caching, distroless debugging, sub-minute inner loop), see [docs/development/kind-iteration.md](../docs/development/kind-iteration.md).

**Pick the right tier for the bug class.** Unit/envtest can't observe behaviours that emerge from real CNI, DNAT, kubelet, or TLS — when a change crosses one of those boundaries, only cluster-only proves it.
