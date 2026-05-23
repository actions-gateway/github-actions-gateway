# End-to-End Test Plan — kind cluster

This document covers the end-to-end test layer described in the design (§7.3). It translates each scenario into concrete test cases, defines the infrastructure required to run them on a local kind cluster, and maps each test to a file location.

The integration tests (§7.2, detailed in [integration-tests.md](integration-tests.md)) run against an in-process `envtest` API server — fast, but without real pod scheduling, real NetworkPolicy enforcement, real HPA scaling, or real container images running. End-to-end tests on kind fill that gap.

---

## 1. Scope and Speed Contract

**Scope:** The full system deployed into a local kind Kubernetes cluster. GMC and AGC binaries run as real Pods. Proxy pods are scheduled and connected. Cert-manager issues TLS certificates for the admission webhook. NetworkPolicy is enforced by kindnet. HPA scaling is driven by metrics-server.

Two test tiers are defined here:

| Tier | What it tests | GitHub required? | Speed |
|---|---|---|---|
| **A — Infrastructure** | GMC provisioning, proxy scheduling, HPA, PDB, RBAC, teardown, GMC restart | No | ~10 min |
| **B — Lifecycle (fake broker)** | AGC session polling, job acquisition, pod creation, eviction retry, SIGTERM cleanup | No | ~20 min |
| **C — Real GitHub** | Actual workflow dispatch, log streaming, RenewJob, proxy egress routing | Yes | ~45 min |

Tier A + B run in CI on every merge to `main`. Tier C runs nightly against a test GitHub repository; it requires `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`, and `GITHUB_APP_PRIVATE_KEY` to be available as GitHub Actions secrets.

**Speed contract:** Each individual test must complete within 3 minutes. Cluster setup is a one-time `BeforeSuite` cost, not counted against individual test budgets.

**Build tag:** `//go:build e2e` on all files. Tests are excluded from `go test ./...` and from the integration test run. CI runs them as a separate job: `go test -tags e2e`.

---

## 2. What kind Adds Over envtest Integration Tests

| Capability | envtest | kind (e2e) |
|---|---|---|
| CRD admission + CEL validation | ✅ | ✅ |
| Admission webhook (cert-manager TLS) | ⚠️ requires manual cert workaround | ✅ |
| Real pod scheduling (kubelet present) | ❌ | ✅ |
| Container images actually pulled and run | ❌ | ✅ |
| NetworkPolicy enforcement (kindnet CNI) | ❌ | ✅ |
| HPA scaling (metrics-server required) | ❌ | ✅ |
| PDB enforcement during node drain | ❌ | ✅ |
| Proxy CONNECT tunnel actually relays bytes | ❌ | ✅ |
| GMC/AGC/proxy binaries run as real Pods | ❌ | ✅ |
| Deployment rollout behavior | ❌ | ✅ |

---

## 3. Infrastructure Decisions

### 3.1 kind Cluster Configuration

A 3-node cluster (1 control-plane + 2 workers) is required for pod anti-affinity and PDB tests to be meaningful.

```yaml
# test/kind-config.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
- role: worker
- role: worker
networking:
  podSubnet: "10.244.0.0/16"
  serviceSubnet: "10.96.0.0/12"
```

kindnet (the default CNI shipped with kind ≥ 0.18) supports `NetworkPolicy` natively. No additional CNI is required for policy enforcement.

The cluster name defaults to `actions-gateway-e2e`. Tests read `E2E_CLUSTER_NAME` (default: `actions-gateway-e2e`) and `KIND` (default: `kind`) from the environment, matching the pattern in `test/utils/utils.go`.

### 3.2 Required Cluster Addons

**cert-manager** is already installed by the existing `BeforeSuite` in `e2e_suite_test.go` via `utils.InstallCertManager()`. No changes needed.

**metrics-server** is required for HPA autoscaling tests (Tier A, §4.5). The `BeforeSuite` installs it if not present:

```go
By("installing metrics-server for HPA tests")
cmd := exec.Command("kubectl", "apply", "-f",
    "https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml")
_, err := utils.Run(cmd)
Expect(err).NotTo(HaveOccurred())

// Patch to allow insecure kubelet TLS (required for kind)
cmd = exec.Command("kubectl", "patch", "-n", "kube-system", "deployment", "metrics-server",
    "--type=json",
    `--patch=[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]`)
_, err = utils.Run(cmd)
Expect(err).NotTo(HaveOccurred())

By("waiting for metrics-server to be ready")
cmd = exec.Command("kubectl", "wait", "deployment/metrics-server", "-n", "kube-system",
    "--for=condition=Available", "--timeout=3m")
_, err = utils.Run(cmd)
Expect(err).NotTo(HaveOccurred())
```

### 3.3 Container Image Strategy

Four images are required. The `BeforeSuite` builds all images and loads them into the kind cluster before any test runs.

| Image tag | Source | Build command |
|---|---|---|
| `gmc:e2e` | `cmd/gmc/` | `make -C cmd/gmc docker-build IMG=gmc:e2e` |
| `agc:e2e` | `cmd/agc/` | `docker build -t agc:e2e -f cmd/agc/Dockerfile .` |
| `proxy:e2e` | `cmd/proxy/` | `docker build -t proxy:e2e -f cmd/proxy/Dockerfile .` |
| `fakegithub:e2e` | `test/fakegithub/` | `docker build -t fakegithub:e2e test/fakegithub/` |

`fakegithub:e2e` is only loaded for Tier B tests. All images use `:e2e` tags to avoid confusion with release images.

Loading images into kind:

```go
for _, img := range []string{"gmc:e2e", "agc:e2e", "proxy:e2e", "fakegithub:e2e"} {
    err = utils.LoadImageToKindClusterWithName(img)
    Expect(err).NotTo(HaveOccurred())
}
```

The `imagePullPolicy: Never` flag is set on all container specs deployed in e2e tests, since images are pre-loaded rather than pulled from a registry.

### 3.4 Fake GitHub Server (Tier B)

`test/fakegithub/` contains a standalone HTTP server that emulates the subset of GitHub APIs and the Actions broker protocol that the AGC calls. It replaces the in-process `brokertest.Server` used in integration tests, functioning instead as a Kubernetes `Deployment` + `Service` accessible to the AGC at a stable in-cluster address.

**Protocol surface:**

| Endpoint | Purpose |
|---|---|
| `POST /app/installations/{id}/access_tokens` | GitHub App token exchange — returns `{"token": "fake-token-xxx", "expires_at": "<+1hr>"}` |
| `POST /actions/runners/registration-token` | Returns the broker base URL pointing to the fake itself |
| `POST /sessions` | Opens a broker session; records session ID |
| `GET /message` | Long-polls for job messages; blocks until a job is enqueued via the control API |
| `POST /acquirejob` | Records acquire call; returns fake `run_service_url` |
| `POST /renewjob` | Records renewal calls |
| `DELETE /sessions/{id}` | Deregisters a session |
| `POST /repos/{owner}/{repo}/actions/runs/{run_id}/rerun-failed-jobs` | Records rerun call |

**Control API** (accessible from tests via `kubectl port-forward`):

| Endpoint | Purpose |
|---|---|
| `POST /control/enqueue-job` | Causes the next `GET /message` from `sessionID` to return a job payload |
| `GET /control/registered-sessions` | Lists all session IDs currently registered |
| `GET /control/acquire-calls` | Returns total `POST /acquirejob` call count |
| `GET /control/rerun-calls` | Returns all `POST /rerun-failed-jobs` calls recorded |
| `POST /control/reset` | Clears all state (used between tests) |

The fake is deployed into `fakegithub-system` namespace. Tests call `utils.FakeGitHubControlURL()` to get the port-forwarded address of the control API.

**AGC configuration:** When the `ActionsGateway` CR is applied in Tier B tests, the AGC Deployment is patched after GMC provisioning to set `GITHUB_API_URL` to the in-cluster address of the fake service (`http://fakegithub.fakegithub-system.svc.cluster.local:8080`). The `gitHubAppRef` Secret contains dummy values that the fake accepts without validation.

---

## 4. File Layout

The existing kubebuilder-generated scaffold under `cmd/gmc/test/` is retained and extended:

```
cmd/gmc/test/
├── e2e/
│   ├── e2e_suite_test.go          # BeforeSuite/AfterSuite — kind cluster setup
│   ├── e2e_test.go                # (existing) GMC pod, metrics, cert/webhook checks
│   ├── provisioning_test.go       # Tier A: all 12 resource types created
│   ├── isolation_test.go          # Tier A: two tenants — no resource crossover
│   ├── teardown_test.go           # Tier A: CR deletion removes only owned resources
│   ├── hpa_pdb_test.go            # Tier A: HPA scaling + PDB enforcement
│   ├── rbac_e2e_test.go           # Tier A: cross-namespace RBAC deny
│   ├── resilience_test.go         # Tier A: GMC pod restart with active tenants
│   ├── job_lifecycle_test.go      # Tier B: AGC + fake broker full flow
│   └── github_e2e_test.go         # Tier C: real GitHub job dispatch (opt-in)
└── utils/
    ├── utils.go                   # (existing)
    ├── cluster.go                 # kind cluster create/delete helpers
    ├── resources.go               # kubectl wrapper helpers (create NS, apply CR, etc.)
    └── fakegithub.go              # port-forward + control API client for fake broker
test/
├── kind-config.yaml               # 3-node kind cluster spec
└── fakegithub/
    ├── main.go                    # fake GitHub + broker HTTP server
    ├── server.go                  # handler implementations
    ├── control.go                 # control API handlers
    ├── server_test.go             # unit tests for the fake itself
    └── Dockerfile                 # distroless image for kind loading
testdata/
└── e2e/
    ├── actionsgateway.yaml        # sample ActionsGateway CR for tests
    └── github-app-secret.yaml     # dummy GitHub App Secret (fake creds)
```

`e2e_suite_test.go` is rewritten to replace the generated boilerplate with the full `BeforeSuite` described in §3.

---

## 5. Tier A — GMC Infrastructure Tests

### 5.1 Extend Existing `e2e_test.go`

The existing tests cover GMC pod running, metrics endpoint, and cert-manager CA injection. No changes needed, but the `BeforeSuite` must be updated to load all four images and install metrics-server before the existing tests run.

### 5.2 Tenant Provisioning (`provisioning_test.go`)

**`E2E_GMC_TenantProvisioning_AllResourcesCreated`**

Setup: create namespace `e2e-tenant-a`. Apply the sample `ActionsGateway` CR referencing the dummy Secret. Wait up to 60 seconds for `ActionsGateway.status.conditions[ProxyAvailable].status == "True"` — this requires the proxy Deployment to have at least one Ready pod, which only happens in kind because a real kubelet schedules and starts the container.

Assert:
- All 12 resource types exist in `e2e-tenant-a` (same list as the integration test)
- Proxy Deployment has `status.readyReplicas >= 1` (real pods are running — stronger than the integration test which stays at 0)
- `ActionsGatewayStatus.Conditions` has `ProxyAvailable=True` and `AGCAvailable=True`
- `ActionsGateway.status.conditions[Ready].status == "True"` — the condition that never fires in envtest does fire here

**`E2E_GMC_TenantProvisioning_ProxyPodsSpreadAcrossNodes`**

Setup: same as above. Assert after proxy pods are Ready:
- Proxy pods are scheduled on distinct nodes (`spec.nodeName` differs across replicas, if `minReplicas >= 2`)

This verifies the `podAntiAffinity` rule in the proxy Deployment actually takes effect under a real scheduler.

**`E2E_GMC_TenantProvisioning_ProxyConnectWorks`**

Assert that the proxy pod is functional, not just scheduled. Run a `curl` pod in `e2e-tenant-a` with `HTTP_PROXY=http://actions-gateway-proxy.e2e-tenant-a.svc.cluster.local:8080` and `HTTPS_CONNECT` method to a reachable target (e.g., `httpbin.org:443` if outbound is available, or an in-cluster echo server). Assert the CONNECT handshake returns `200 Connection established`.

This verifies the proxy binary is running and routing correctly — not testable in envtest.

### 5.3 Multi-Tenant Isolation (`isolation_test.go`)

**`E2E_GMC_Isolation_TwoTenantsGetIndependentResources`**

Setup: apply `ActionsGateway` CRs in `e2e-tenant-a` and `e2e-tenant-b`. Wait for both to reach `ProxyAvailable=True`.

Assert:
- `Deployment/actions-gateway-proxy` exists in both namespaces
- The proxy Service ClusterIP in `tenant-a` differs from the one in `tenant-b`
- No resource in `tenant-a` has an owner reference pointing to the CR in `tenant-b`, and vice versa

**`E2E_GMC_Isolation_OneTenantsProxyDoesNotRouteOtherTenantTraffic`**

Deploy a `curl` pod in `e2e-tenant-a` with `HTTP_PROXY` pointing at `tenant-b`'s proxy service. Assert the curl pod cannot reach it (NetworkPolicy denies traffic from `tenant-a` pods to `tenant-b`'s proxy).

This is the only test in the suite that verifies NetworkPolicy enforcement — not possible in envtest without a CNI.

### 5.4 Tenant Teardown (`teardown_test.go`)

**`E2E_GMC_Teardown_RemovesOnlyOwnedResources`**

Setup: apply two `ActionsGateway` CRs (`tenant-a`, `tenant-b`). Wait for both to reach `ProxyAvailable=True`. Delete the `tenant-a` CR.

Assert via `Eventually`:
- All GMC-owned resources are gone from `tenant-a` (Deployment, Service, HPA, PDB, RBAC, NetworkPolicy, ResourceQuota)
- Namespace `tenant-a` itself is still present
- All `tenant-b` resources are untouched
- `tenant-b`'s proxy pods remain `Ready` throughout (no spurious restart)

**`E2E_GMC_Teardown_ReapplyAfterDelete`**

After teardown, re-apply the same `ActionsGateway` CR. Assert the proxy pods reach `Ready` again and `ProxyAvailable=True` is set without any `AlreadyExists` errors in the GMC logs.

### 5.5 HPA and PDB (`hpa_pdb_test.go`)

> **CI note:** `E2E_GMC_HPA_ScalesUpUnderLoad` and `E2E_GMC_PDB_PreventsEvictionBelowMinAvailable` are marked `Label("local-only")` and excluded from the CI workflow (`--label-filter '!local-only'`). `HPA_ScalesUpUnderLoad` requires generating enough CPU load to trigger autoscaling, which is unreliable on 2-vCPU GitHub Actions runners where the proxy pods and the load-generator pod compete for the same cores. `PDB_PreventsEvictionBelowMinAvailable` runs `kubectl drain`, which depends on Kubernetes eviction timing that becomes flaky under CPU contention. Both tests run correctly on local machines with more cores. `HPA_BoundsUpdate_Propagates` — a pure API-server assertion with no load — runs in CI without restriction.

**`E2E_GMC_HPA_ScalesUpUnderLoad`** *(local-only)*

Setup: apply an `ActionsGateway` CR with `minReplicas: 1`, `maxReplicas: 4`. Wait for the proxy Deployment to have 1 Ready pod.

Exercise: generate CPU load against the proxy pods using a burst of concurrent CONNECT requests from a load-generator pod inside the cluster.

Assert via `Eventually` with 5-minute timeout:
- `HorizontalPodAutoscaler.status.currentReplicas > 1` (HPA triggered scale-up)

Assert after load subsides (Eventually with 5-minute timeout):
- Replica count returns to `1` (HPA scales back down)

Note: HPA reactivity depends on the 15-second scrape interval from metrics-server and a 5-minute stabilization window. The assertions use long timeouts deliberately.

**`E2E_GMC_PDB_PreventsEvictionBelowMinAvailable`** *(local-only)*

Setup: apply an `ActionsGateway` CR with `minReplicas: 2`. Wait for 2 proxy pods to be Ready on separate nodes.

Exercise: cordon one worker node (simulating maintenance) and attempt to drain it (`kubectl drain --ignore-daemonsets --delete-emptydir-data`).

Assert: the drain command eventually fails or is blocked for the proxy pod on that node, because the PDB (`minAvailable: 1`) prevents bringing the replica count below 1 while the other pod is still draining.

After the drain attempt, uncordon the node. Assert both proxy pods return to `Ready`.

**`E2E_GMC_HPA_BoundsUpdate_Propagates`**

Apply an `ActionsGateway` CR with `maxReplicas: 3`. Wait for initial reconcile. Update `spec.proxy.maxReplicas: 6` via `kubectl patch`. Assert the HPA in the namespace shows `spec.maxReplicas == 6` within one reconcile cycle (≤ 15 seconds).

### 5.6 RBAC Enforcement (`rbac_e2e_test.go`)

**`E2E_GMC_RBAC_AGCCannotReadOtherTenantNamespace`**

Setup: provision `tenant-a` and `tenant-b`. Identify the `actions-gateway-agc` ServiceAccount in `tenant-a`.

Exercise: create a pod in `tenant-a` using the `actions-gateway-agc` ServiceAccount and execute `kubectl get pods -n tenant-b` from within it.

Assert: the command fails with `403 Forbidden`. The API server enforces the namespace-scoped Role.

This test is stronger than the envtest RBAC test because it exercises the real RBAC enforcer with a real Pod identity, not an impersonated REST config.

### 5.7 GMC Restart Resilience (`resilience_test.go`)

**`E2E_GMC_Restart_ResourcesPersistAfterGMCPodDeletion`**

Setup: provision `tenant-a`. Wait for `ProxyAvailable=True` and proxy pods `Ready`.

Exercise: delete the GMC pod (`kubectl delete pod -l control-plane=controller-manager -n gmc-system`). Wait for the new GMC pod to become `Ready` (leader election promoted the standby, or the Deployment controller created a replacement).

Assert:
- Proxy Deployment in `tenant-a` still exists with unchanged spec
- Proxy pods remain `Ready` throughout (no disruption)
- HPA and PDB remain intact
- `ActionsGateway.status.conditions[Ready].status` still `True` after the new GMC pod starts and re-derives state

**`E2E_GMC_Restart_NoDuplicateResourcesAfterRestart`**

Following the restart, trigger a synthetic change to the `ActionsGateway` CR (e.g., update a label on the metadata). Assert that the GMC reconciler runs exactly one reconcile cycle (observable via `actions_gateway_reconcile_errors_total` not incrementing) and does not create duplicate resources.

---

## 6. Tier B — AGC Job Lifecycle (Fake Broker)

### 6.1 Fake GitHub Server Setup

The `BeforeSuite` for Tier B (controlled by a build-time flag or a dedicated `BeforeSuite` in `job_lifecycle_test.go`) deploys the fake GitHub server:

```go
By("deploying fake GitHub server")
cmd := exec.Command("kubectl", "apply", "-f", "testdata/e2e/fakegithub-deployment.yaml")
_, err := utils.Run(cmd)
Expect(err).NotTo(HaveOccurred())

By("waiting for fake GitHub server to be ready")
cmd = exec.Command("kubectl", "wait", "deployment/fakegithub",
    "-n", "fakegithub-system", "--for=condition=Available", "--timeout=2m")
_, err = utils.Run(cmd)
Expect(err).NotTo(HaveOccurred())
```

The fake GitHub server's control API is accessed via `kubectl port-forward` in a background goroutine started in `BeforeEach`:

```go
controlURL, cancel := utils.StartFakeGitHubPortForward(GinkgoT())
DeferCleanup(cancel)
```

### 6.2 Patching the AGC to Use the Fake

After GMC provisions an `ActionsGateway` CR, the AGC Deployment is patched:

```go
By("patching AGC Deployment to point at fake GitHub")
fakeURL := "http://fakegithub.fakegithub-system.svc.cluster.local:8080"
patch := fmt.Sprintf(`[{"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{"name":"GITHUB_API_URL","value":%q}}]`, fakeURL)
cmd := exec.Command("kubectl", "patch", "deployment", "actions-gateway-agc",
    "-n", namespace, "--type=json", "--patch", patch)
_, err := utils.Run(cmd)
Expect(err).NotTo(HaveOccurred())

By("waiting for AGC pod to be ready after patch")
cmd = exec.Command("kubectl", "rollout", "status", "deployment/actions-gateway-agc",
    "-n", namespace, "--timeout=2m")
_, err = utils.Run(cmd)
Expect(err).NotTo(HaveOccurred())
```

The `gitHubAppRef` Secret uses dummy values that the fake accepts without validation (since the fake returns a fake token regardless of input).

### 6.3 Job Lifecycle Tests (`job_lifecycle_test.go`)

**`E2E_AGC_JobLifecycle_SessionRegistered`**

Setup: apply `ActionsGateway` + patch AGC to use fake. Wait for AGC pod `Ready`.

Assert via `Eventually` (polling the control API):
- `GET /control/registered-sessions` returns at least one session ID (AGC connected to fake and opened a session)

**`E2E_AGC_JobLifecycle_JobDispatchCreatesSecretAndPod`**

Continuing from the previous setup. Enqueue a job via `POST /control/enqueue-job` with a synthetic job payload.

Assert via `Eventually`:
- A `Secret` with label `actions-gateway/runner-group` exists in the tenant namespace
- A `Pod` with label `actions-gateway/runner-group` exists in the tenant namespace with the correct spec (proxy env vars set, `automountServiceAccountToken: false`)
- `GET /control/acquire-calls` returns `>= 1` (AGC called acquirejob)

**`E2E_AGC_JobLifecycle_SecretDeletedAfterPodCompletes`**

Continuing from the previous setup. Wait for the pod to exist. Update the pod's status to `Succeeded` via `kubectl patch pod ... --subresource=status`.

Assert via `Eventually`:
- The job Secret is deleted
- No pods with `actions-gateway/runner-group` label remain in the namespace

**`E2E_AGC_JobLifecycle_EvictionTriggersRequeue`**

Setup: enqueue a job and wait for a pod to be created. Simulate eviction by patching the pod status: `phase: Failed, reason: Evicted`.

Assert via `Eventually`:
- `GET /control/rerun-calls` returns a record for the job's `run_id`
- The job Secret is deleted
- `actions_gateway_eviction_retries_total{namespace="..."}` counter is `1` (scraped from the AGC's metrics endpoint)

**`E2E_AGC_SIGTERM_DeletesAllSessions`**

Setup: apply a `RunnerGroup` with `maxListeners: 3`. Enqueue 2 jobs to bring the session count to 3.

Assert: `GET /control/registered-sessions` returns 3 sessions.

Exercise: delete the AGC pod (`kubectl delete pod`). The Deployment controller will restart it; the SIGTERM handler runs during the deletion.

Assert within 30 seconds:
- The fake broker records `DELETE /sessions/{id}` for each of the 3 session IDs before the pod terminates
- (Observable via `GET /control/registered-sessions` returning empty after the pod terminates and before the new pod reconnects)

---

## 7. Tier C — Real GitHub Tests (Opt-in)

These tests require a test GitHub repository with pre-committed workflow files and a GitHub App with the correct permissions.

### 7.1 Required Environment Variables

All Tier C tests skip at runtime if any of these variables are absent:

| Variable | Description |
|---|---|
| `E2E_GITHUB_APP_ID` | GitHub App ID |
| `E2E_GITHUB_APP_INSTALLATION_ID` | Installation ID for the test org/repo |
| `E2E_GITHUB_APP_PRIVATE_KEY` | PEM-encoded private key (base64-encoded in CI) |
| `E2E_GITHUB_ORG` | GitHub org or user owning the test repository |
| `E2E_GITHUB_REPO` | Repository name containing test workflow files |

Tests use `skipIfNoGitHubCreds(GinkgoT())` — a helper that calls `Skip()` if any variable is missing. This makes all Tier C tests no-ops in environments without credentials, so they can coexist in the same build tag without breaking CI runners that lack secrets.

### 7.2 Test GitHub Repository Setup

A dedicated repository (e.g., `{org}/actions-gateway-e2e-workflows`) contains:

| Workflow file | Purpose |
|---|---|
| `smoke.yml` | `workflow_dispatch`; single job: `echo "hello"` |
| `parallel.yml` | `workflow_dispatch` with `matrix` strategy (10 variants), each running `echo "job ${{ matrix.i }}"` |
| `slow.yml` | `workflow_dispatch`; single job that sleeps 15 minutes then exits 0 |
| `failing.yml` | `workflow_dispatch`; single job that exits 1 |

Workflows use `runs-on: [self-hosted, e2e-test]` — a label set on the `RunnerGroup` in the test `ActionsGateway` CR.

### 7.3 Real GitHub Tests (`github_e2e_test.go`)

**`E2E_GitHub_Smoke_SingleJobCompletesGreen`**

Setup: apply `ActionsGateway` CR with the real GitHub App credentials. Wait for `ProxyAvailable=True` and AGC pod `Ready`.

Exercise: trigger a `workflow_dispatch` for `smoke.yml` via the GitHub REST API.

Assert via polling the GitHub REST API (up to 5 minutes):
- The workflow run transitions from `queued` → `in_progress` → `completed`
- `conclusion == "success"`
- Job steps include the expected log output (`"hello"`)

This is the merge-gate smoke test: it runs on every merge to `main` when credentials are present.

**`E2E_GitHub_Parallel_TenJobsAllComplete`**

Exercise: dispatch `parallel.yml`.

Assert via polling (up to 10 minutes):
- All 10 matrix jobs complete with `conclusion == "success"`
- No jobs are stuck in `queued` after the burst (verifies the session multiplexer handles concurrent polling without message collisions)

**`E2E_GitHub_ProxyEgress_TrafficExitsThroughTenantProxy`**

Exercise: dispatch `smoke.yml`. While the job is running, scrape the AGC's metrics endpoint.

Assert:
- `actions_gateway_proxy_connections_total` on the proxy pod metrics endpoint is > 0 (proxy handled at least one connection)
- No `curl` connection to a GitHub IP directly from the AGC pod succeeds when `HTTPS_PROXY` is set and the NetworkPolicy blocks direct egress (verify via `kubectl exec` against the running AGC pod)

**`E2E_GitHub_JobFailurePropagates`**

Exercise: dispatch `failing.yml`.

Assert:
- Workflow run completes with `conclusion == "failure"`
- Worker pod in the tenant namespace exits and is cleaned up (no pod or Secret leak after run ends)

**`E2E_GitHub_RenewJobKeeps15MinuteJobAlive`**

Exercise: dispatch `slow.yml` (sleeps 15 minutes).

Assert:
- Workflow run completes with `conclusion == "success"` — GitHub did not cancel the job for lock expiry
- Confirm the RenewJob loop fired by checking `actions_gateway_renewjob_errors_total` remains at `0` throughout

This test has a 20-minute timeout.

**`E2E_GitHub_TenantProvisioningAndDeprovisioning_WithRealJobs`**

Exercise: apply CR, run `smoke.yml` to completion, delete the CR, re-apply the CR, run `smoke.yml` again.

Assert:
- Both jobs complete successfully
- After the delete+reapply cycle, the new AGC session is independent of the old one (no duplicate session registration)

---

## 8. CI Workflow

Add a new workflow file `.github/workflows/e2e-test.yml`:

```yaml
name: e2e-tests

on:
  push:
    branches: [main]
  schedule:
    - cron: '0 3 * * *'  # nightly at 03:00 UTC
  workflow_dispatch:

jobs:
  e2e-tier-ab:
    name: kind e2e (Tier A + B)
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.work
      - name: Install kind
        run: |
          curl -Lo /usr/local/bin/kind https://kind.sigs.k8s.io/dl/v0.26.0/kind-linux-amd64
          chmod +x /usr/local/bin/kind

      - name: Create kind cluster
        run: kind create cluster --name actions-gateway-e2e --config test/kind-config.yaml

      - name: Build and load images
        run: |
          docker build -t gmc:e2e -f cmd/gmc/Dockerfile .
          docker build -t agc:e2e -f cmd/agc/Dockerfile .
          docker build -t proxy:e2e -f cmd/proxy/Dockerfile .
          docker build -t fakegithub:e2e test/fakegithub/
          for img in gmc:e2e agc:e2e proxy:e2e fakegithub:e2e; do
            kind load docker-image "$img" --name actions-gateway-e2e
          done

      - name: Run Tier A + B e2e tests
        run: go test -tags e2e -v -timeout 40m -count=1 --label-filter '!local-only' ./test/e2e/...
        working-directory: cmd/gmc
        env:
          KIND_CLUSTER: actions-gateway-e2e

      - name: Collect debug info on failure
        if: failure()
        run: |
          kubectl get all -A
          kubectl describe nodes
          kubectl logs -n gmc-system -l control-plane=controller-manager --tail=200

      - name: Delete kind cluster
        if: always()
        run: kind delete cluster --name actions-gateway-e2e

  e2e-tier-c:
    name: kind e2e (Tier C — real GitHub)
    runs-on: ubuntu-latest
    if: github.event_name == 'schedule' || github.event_name == 'workflow_dispatch'
    needs: e2e-tier-ab
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.work
      - name: Install kind
        run: |
          curl -Lo /usr/local/bin/kind https://kind.sigs.k8s.io/dl/v0.26.0/kind-linux-amd64
          chmod +x /usr/local/bin/kind

      - name: Create kind cluster
        run: kind create cluster --name actions-gateway-e2e --config test/kind-config.yaml

      - name: Build and load images
        run: |
          docker build -t gmc:e2e -f cmd/gmc/Dockerfile .
          docker build -t agc:e2e -f cmd/agc/Dockerfile .
          docker build -t proxy:e2e -f cmd/proxy/Dockerfile .
          for img in gmc:e2e agc:e2e proxy:e2e; do
            kind load docker-image "$img" --name actions-gateway-e2e
          done

      - name: Run Tier C e2e tests
        run: go test -tags e2e -v -timeout 90m -count=1 ./test/e2e/...
        working-directory: cmd/gmc
        env:
          KIND_CLUSTER: actions-gateway-e2e
          E2E_GITHUB_APP_ID: ${{ secrets.E2E_GITHUB_APP_ID }}
          E2E_GITHUB_APP_INSTALLATION_ID: ${{ secrets.E2E_GITHUB_APP_INSTALLATION_ID }}
          E2E_GITHUB_APP_PRIVATE_KEY: ${{ secrets.E2E_GITHUB_APP_PRIVATE_KEY }}
          E2E_GITHUB_ORG: ${{ secrets.E2E_GITHUB_ORG }}
          E2E_GITHUB_REPO: ${{ secrets.E2E_GITHUB_REPO }}

      - name: Delete kind cluster
        if: always()
        run: kind delete cluster --name actions-gateway-e2e
```

The smoke test (`E2E_GitHub_Smoke_SingleJobCompletesGreen`) also runs in the main `e2e-tier-ab` job on every merge to `main` when `E2E_GITHUB_APP_ID` is set — it is gated by `skipIfNoGitHubCreds` so it becomes a no-op when credentials are absent.

---

## 9. Makefile Targets

Add to the root `Makefile`:

```makefile
# Create a local kind cluster for e2e tests
e2e-cluster:
	kind create cluster --name actions-gateway-e2e --config test/kind-config.yaml

# Build and load all images into the e2e kind cluster
e2e-images:
	docker build -t gmc:e2e -f cmd/gmc/Dockerfile .
	docker build -t agc:e2e -f cmd/agc/Dockerfile .
	docker build -t proxy:e2e -f cmd/proxy/Dockerfile .
	docker build -t fakegithub:e2e test/fakegithub/
	for img in gmc:e2e agc:e2e proxy:e2e fakegithub:e2e; do \
		kind load docker-image $$img --name actions-gateway-e2e; \
	done

# Run Tier A + B e2e tests against the local kind cluster
e2e:
	KIND_CLUSTER=actions-gateway-e2e \
	go test -C cmd/gmc -tags e2e -v -timeout 40m -count=1 ./test/e2e/...

# Full local setup + run (cluster + images + tests)
e2e-all: e2e-cluster e2e-images e2e

# Delete the e2e kind cluster
e2e-clean:
	kind delete cluster --name actions-gateway-e2e
```

Typical local usage:

```bash
make e2e-up         # create cluster (if missing), build+load images, run tests
make e2e-clean      # when done
```

Or step-by-step if you want finer control:

```bash
make e2e-cluster        # once per session
make e2e-load-images    # builds images then loads them; re-run after a code change
make e2e                # run tests
make e2e-clean          # when done
```

---

## 10. What Is NOT Covered Here

These scenarios are deferred or explicitly out of scope for the kind e2e layer:

| Scenario | Reason |
|---|---|
| `RenewJob under 15-minute job` (Tier C) | Requires real GitHub job actively running; nightly only |
| Rolling AGC upgrade under live traffic | Requires sustained real job flow; not reproducible in kind without real GitHub |
| GitHub IP range live fetch and NetworkPolicy update | Requires network access to `api.github.com`; integration test covers the logic with a stub |
| Proxy HPA scaling under real GitHub job load | Real load pattern requires concurrent GitHub jobs dispatched from the same runner |
| Worker pod running the actual `.NET` runner binary | Requires the runner image, GitHub Actions token, and real GitHub backend |
| Stress test: 50 sequential jobs, 5 tenants | Load test harness deferred to Milestone 5 |
| gVisor/Kata RuntimeClass | Optional worker isolation; no requirement on the kind cluster |

---

## 11. Test Count Summary

☐ = not yet implemented.

| Suite | Test | Tier |
|---|---|---|
| Existing e2e | GMC pod running | A |
| Existing e2e | Metrics endpoint | A |
| Existing e2e | Cert-manager CA injection | A |
| provisioning | `E2E_GMC_TenantProvisioning_AllResourcesCreated` | A |
| provisioning | `E2E_GMC_TenantProvisioning_ProxyPodsSpreadAcrossNodes` | A |
| provisioning | `E2E_GMC_TenantProvisioning_ProxyConnectWorks` | A |
| isolation | `E2E_GMC_Isolation_TwoTenantsGetIndependentResources` | A |
| isolation | `E2E_GMC_Isolation_OneTenantsProxyDoesNotRouteOtherTenantTraffic` | A |
| teardown | `E2E_GMC_Teardown_RemovesOnlyOwnedResources` | A |
| teardown | `E2E_GMC_Teardown_ReapplyAfterDelete` | A |
| hpa_pdb | `E2E_GMC_HPA_ScalesUpUnderLoad` *(local-only)* | A |
| hpa_pdb | `E2E_GMC_PDB_PreventsEvictionBelowMinAvailable` *(local-only)* | A |
| hpa_pdb | `E2E_GMC_HPA_BoundsUpdate_Propagates` | A |
| rbac_e2e | `E2E_GMC_RBAC_AGCCannotReadOtherTenantNamespace` | A |
| resilience | `E2E_GMC_Restart_ResourcesPersistAfterGMCPodDeletion` | A |
| resilience | `E2E_GMC_Restart_NoDuplicateResourcesAfterRestart` | A |
| job_lifecycle | `E2E_AGC_JobLifecycle_SessionRegistered` | B |
| job_lifecycle | `E2E_AGC_JobLifecycle_JobDispatchCreatesSecretAndPod` | B |
| job_lifecycle | `E2E_AGC_JobLifecycle_SecretDeletedAfterPodCompletes` | B |
| job_lifecycle | `E2E_AGC_JobLifecycle_EvictionTriggersRequeue` | B |
| job_lifecycle | `E2E_AGC_SIGTERM_DeletesAllSessions` | B |
| github_e2e | `E2E_GitHub_Smoke_SingleJobCompletesGreen` | C |
| github_e2e | `E2E_GitHub_Parallel_TenJobsAllComplete` | C |
| github_e2e | `E2E_GitHub_ProxyEgress_TrafficExitsThroughTenantProxy` | C |
| github_e2e | `E2E_GitHub_JobFailurePropagates` | C |
| github_e2e | `E2E_GitHub_RenewJobKeeps15MinuteJobAlive` | C |
| github_e2e | `E2E_GitHub_TenantProvisioningAndDeprovisioning_WithRealJobs` | C |
| **Total** | **27 tests** (3 existing + 13 Tier A new + 5 Tier B + 6 Tier C) | |
