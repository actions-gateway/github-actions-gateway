# Integration Test Plan

This document covers the integration-test layer described in the design (§7.2). It translates each scenario into concrete test cases, specifies the infrastructure decisions required to make them run, and maps each test to a file location. Unit tests (fake-client, httptest-only) are out of scope; end-to-end tests against a real GitHub cluster are out of scope (§7.3).

---

## 1. Scope and Speed Contract

**Scope:** Multiple components interacting with a real Kubernetes API server (via `envtest`) and a stubbed GitHub broker (`httptest.Server`). No actual GitHub network calls. No container image builds. No real Kubernetes scheduling — pods are created in the API server but never actually scheduled.

**Speed contract:** Full suite in under 5 minutes. Each test must complete in under 30 seconds. Tests that sleep beyond 5 seconds are a red flag and should be rewritten with `gomega.Eventually` + short polling intervals.

**Build tag:** `//go:build integration` on all test files. This keeps integration tests out of the unit-test run (`go test ./...`) and requires an explicit `-tags integration` flag. CI runs them as a separate job after unit tests pass.

---

## 2. Infrastructure Decisions

### 2.1 Kubernetes API Server — `envtest`

`controller-runtime/pkg/envtest` spins up a real `kube-apiserver` and `etcd` binary locally, without any kubelet or scheduler. It provides a `client.Client` and a REST config that reconcilers can use exactly as they would in production.

**Why envtest instead of fake-client for these tests:** The fake-client cannot enforce CRD admission validation (CEL rules, `x-kubernetes-validations`), does not handle ownership references and garbage collection, and cannot test webhook behavior. envtest runs a real API server so CRD schemas, admission webhooks, and status subresources all work as in production.

**Binary management:** `controller-runtime`'s `setup-envtest` CLI downloads the required binaries into a local cache. The integration test `TestMain` or `BeforeSuite` reads `KUBEBUILDER_ASSETS` (set by `setup-envtest use --print env`) to locate them. The CI workflow sets this environment variable before running integration tests.

```bash
# One-time setup (add to CI job):
go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use --print env
# Output: export KUBEBUILDER_ASSETS="/home/runner/.local/share/kubebuilder-envtest/k8s/1.31-linux-amd64"
```

**CRD installation:** The `envtest.Environment` installs CRDs from path(s) specified in `CRDDirectoryPaths`. Integration tests load both CRDs:
- `cmd/agc/config/crd/actions-gateway.github.com_runnergroups.yaml`
- `cmd/gmc/config/crd/bases/actions-gateway.github.com_actionsgateways.yaml`

### 2.2 GitHub Broker Fake — `httptest.Server`

A shared `httptest.Server` implements the broker protocol with real stateful behaviour. Unlike the unit test stubs (which serve static canned responses), the integration broker fake tracks session registrations, delivers job messages on demand, and records which sessions issued `DELETE /sessions/{id}` on shutdown.

The fake is implemented in a shared `internal/brokertest` package (see §3.1 for its interface). Tests control it by calling methods like `server.EnqueueJob(sessionID, payload)` and `server.WaitForSessionDelete(sessionID, timeout)`.

### 2.3 Test Framework

Use standard `testing` + `testify` + `gomega.Eventually` — the same pattern as the existing unit tests in this repo. Do not switch to Ginkgo/Gomega for the integration layer; the existing kubebuilder-generated Ginkgo scaffolding in `suite_test.go` is currently empty and was never populated, so starting fresh with the repo's established style is cleaner.

The one exception is `gomega.Eventually` (from `github.com/onsi/gomega`), which is already used in `cmd/agc/internal/controller/runnergroup_controller_test.go` and is the right tool for asserting eventually-consistent API server state.

---

## 3. File Layout

```
cmd/
├── agc/
│   └── internal/
│       └── controller/
│           └── integration/
│               ├── suite_integration_test.go   # TestMain, envtest setup/teardown
│               ├── reconciler_test.go          # AGC reconciler lifecycle tests
│               ├── secret_lifecycle_test.go    # Secret create/delete per job
│               ├── pod_provisioning_test.go    # Worker pod spec assertions
│               ├── failure_recovery_test.go    # Pod crash without Secret leak
│               └── sigterm_test.go             # Graceful session cleanup
└── gmc/
    └── internal/
        └── controller/
            └── integration/
                ├── suite_integration_test.go   # TestMain, envtest setup/teardown
                ├── provisioning_test.go        # Tenant resource creation
                ├── teardown_test.go            # Tenant resource deletion
                ├── hpa_update_test.go          # Proxy bounds propagation
                ├── network_policy_test.go      # Egress rule enforcement
                └── rbac_scope_test.go          # Cross-namespace deny
```

Both `suite_integration_test.go` files declare the same `//go:build integration` tag and contain:
1. A `TestMain` that starts/stops `envtest`
2. A package-level `k8sClient client.Client` initialized from the envtest REST config
3. A `startReconciler(t)` helper that runs the reconciler under test in a background goroutine, cancelled by `t.Cleanup`

### 3.1 Shared Broker Fake

```
internal/
└── brokertest/
    ├── server.go        # Fake broker server and control interface
    └── server_test.go   # Self-tests for the fake
```

The fake provides:

```go
package brokertest

// Server is a controllable in-process broker fake.
type Server struct {
    URL string // base URL of the httptest.Server
    // ...
}

// New starts a new fake broker. Call Close when done.
func New() *Server

// RegisteredSessions returns the IDs of all sessions that have been created.
// Called by the fake's POST /session handler automatically.
func (s *Server) RegisteredSessions() []string

// EnqueueJob causes the fake's GET /message long-poll to return
// a job message to the next caller polling on sessionID.
func (s *Server) EnqueueJob(sessionID string, payload broker.RunnerJobRequestBody)

// WaitForSessionDelete blocks until the given sessionID is deleted
// via DELETE /session, or until timeout expires.
func (s *Server) WaitForSessionDelete(sessionID string, timeout time.Duration) bool

// AcquireJobCalls returns the number of POST /acquirejob calls received.
func (s *Server) AcquireJobCalls() int

// Close shuts down the httptest.Server.
func (s *Server) Close()
```

The fake broker lives in a test-only package under `internal/brokertest/` within the root module. Because the `cmd/agc` module imports the root module's `broker` package, `brokertest` can live alongside it and be imported by AGC integration tests via the workspace replace directive.

---

## 4. GMC Integration Tests (`cmd/gmc/internal/controller/integration/`)

These tests start the `ActionsGatewayReconciler` against an envtest API server and assert on the resources it creates.

### 4.1 Suite Setup (`suite_integration_test.go`)

```go
//go:build integration

func TestMain(m *testing.M) {
    env := &envtest.Environment{
        CRDDirectoryPaths: []string{
            "../../../../config/crd/bases",         // ActionsGateway CRD
            "../../../../../agc/config/crd",        // RunnerGroup CRD
        },
        ErrorIfCRDPathMissing: true,
    }
    cfg, err := env.Start()
    // ... register schemes, create k8sClient ...
    // start IPRangeReconciler with a stub fetcher returning fixed CIDRs
    exitCode := m.Run()
    _ = env.Stop()
    os.Exit(exitCode)
}
```

The `ActionsGatewayReconciler` is started in each test via a helper that creates a `ctrl.Manager` with the envtest REST config, registers the reconciler, and runs the manager in a goroutine cancelled by `t.Cleanup`. This gives each test its own isolated reconciler instance with no shared state.

### 4.2 Tenant Provisioning (`provisioning_test.go`)

**TestGMC_TenantProvisioning_AllResourcesCreated**

Setup: create a namespace `team-a` and apply an `ActionsGateway` CR with a stub `gitHubAppRef` Secret.

Exercise: wait for one reconcile cycle.

Assert:
- `ServiceAccount` named `actions-gateway-agc` exists in `team-a`
- `ServiceAccount` named `actions-gateway-worker` exists in `team-a`
- `Role` exists in `team-a` with the expected rules (pod, secret, runnergroup permissions)
- `RoleBinding` exists in `team-a` binding `actions-gateway-agc` SA to the Role
- `NetworkPolicy` exists in `team-a`
- `ResourceQuota` exists in `team-a` (when `spec.namespaceQuota` is set in the CR)
- `Deployment` named `actions-gateway-proxy` exists in `team-a` with `spec.replicas >= 1`, `resources.requests.cpu` set
- `Service` named `actions-gateway-proxy` exists in `team-a` with port 8080
- `PodDisruptionBudget` exists in `team-a` with `minAvailable: 1`
- `HorizontalPodAutoscaler` exists in `team-a` with correct `minReplicas`, `maxReplicas`, `targetCPUUtilizationPercentage`
- `Deployment` named `actions-gateway-agc` exists in `team-a` with `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` env vars set; `GITHUB_APP_ID` mapped to the correct `secretKeyRef`
- AGC Deployment's `NO_PROXY` contains `kubernetes.default.svc.cluster.local`
- Namespace `team-a` still exists (GMC did not delete or modify it)
- `ActionsGatewayStatus.Conditions` includes `ProxyAvailable` and `AGCAvailable` conditions (regardless of `Ready` status, since envtest does not schedule pods)

Use `gomega.Eventually` with a 15-second timeout to poll for each resource, since reconciles are asynchronous.

---

**TestGMC_TenantProvisioning_BootstrapRunnerGroups**

Setup: apply an `ActionsGateway` CR with two entries in `spec.runnerGroups`.

Assert after reconcile:
- Two `RunnerGroup` CRs exist in the namespace, named `{gateway-name}-{runnergroup.name}`
- Each `RunnerGroup` has the spec fields from the `ActionsGateway` CR passed through unchanged
- Each `RunnerGroup` has the managed label `actions-gateway/owner-name: {gateway-name}`

---

**TestGMC_TenantProvisioning_NoProxyMergesDefaults**

Setup: apply an `ActionsGateway` CR with `spec.proxy.noProxyCIDRs: ["192.168.1.0/24"]`.

Assert: the AGC Deployment's `NO_PROXY` env contains both `192.168.1.0/24` and `kubernetes.default.svc.cluster.local` (verifies the `buildNoProxy` merge, not the old replace-only bug).

---

**TestGMC_TenantProvisioning_GitHubAppRefDefaultsToOwnNamespace**

Setup: apply an `ActionsGateway` CR with `spec.gitHubAppRef.name: "my-secret"` and `namespace` omitted.

Assert: the AGC Deployment's `GITHUB_APP_ID` env `secretKeyRef.name` is `"my-secret"` and its namespace is implicitly `team-a` (the CR's own namespace) — i.e., the `secretKeyRef` in the volume mount references the CR namespace, not a cluster-level secret.

---

**TestGMC_TenantProvisioning_CredentialRotation**

Setup: create initial `ActionsGateway` CR referencing `secret-v1`. Let it reconcile. Then update `spec.gitHubAppRef.name` to `secret-v2`.

Assert after the update reconcile:
- AGC Deployment's `GITHUB_APP_ID` env `secretKeyRef.name` is `"secret-v2"` (rollout triggered)
- The old Secret (`secret-v1`) is not deleted by the GMC (tenant manages rotation)

### 4.3 Tenant Teardown (`teardown_test.go`)

**TestGMC_TenantTeardown_RemovesOnlyOwnedResources**

Setup: apply two `ActionsGateway` CRs in two separate namespaces (`team-a`, `team-b`). Wait for both to reconcile. Then delete the `team-a` CR.

Assert after the delete reconcile:
- `ServiceAccount`, `Role`, `RoleBinding`, `NetworkPolicy`, `Deployment` (proxy + AGC), `Service`, `PodDisruptionBudget`, `HPA` all removed from `team-a`
- `RunnerGroup` CRs in `team-a` are removed
- Namespace `team-a` still exists (GMC does not delete it)
- All corresponding resources in `team-b` are untouched

Use `gomega.Eventually` to wait for the finalizer to be removed from the `ActionsGateway` CR before asserting resource absence.

---

**TestGMC_TenantTeardown_ReapplyAfterDelete**

Setup: apply an `ActionsGateway` CR, wait for reconcile, delete it, wait for cleanup. Then re-apply the same CR.

Assert: all resources are re-created cleanly (no stale state, no `AlreadyExists` errors in the reconciler).

### 4.4 HPA Bounds Update (`hpa_update_test.go`)

**TestGMC_HPABoundsUpdate**

Setup: apply an `ActionsGateway` CR with `spec.proxy.maxReplicas: 5`. Wait for initial reconcile.

Exercise: update `spec.proxy.maxReplicas: 10`.

Assert within one reconcile cycle: the `HorizontalPodAutoscaler` in the tenant namespace has `spec.maxReplicas == 10`.

---

**TestGMC_HPABoundsUpdate_MinReplicasClamped**

Setup: apply an `ActionsGateway` CR with `spec.proxy.minReplicas: 3, spec.proxy.maxReplicas: 5`.

Assert: HPA has `minReplicas == 3`, `maxReplicas == 5`. GMC does not flip or invert the bounds.

### 4.5 NetworkPolicy Enforcement (`network_policy_test.go`)

envtest does not run a CNI plugin, so the `NetworkPolicy` cannot be "enforced" in the networking sense. What these tests verify is the *content* of the generated policy, which is correctness of a different kind than the unit tests (which test builder functions in isolation). Integration tests verify the policy is actually created in the API server with the right spec, and that updates propagate correctly.

**TestGMC_NetworkPolicy_ProxyEgressContainsGitHubCIDRs**

Setup: start the GMC with a stub `GitHubIPRangeFetcher` that returns `["140.82.112.0/20", "192.30.252.0/22"]`. Apply an `ActionsGateway` CR.

Assert: the `NetworkPolicy` in the tenant namespace has an egress rule whose `ipBlock.cidr` set includes both `140.82.112.0/20` and `192.30.252.0/22`, scoped to the pod selector `app: actions-gateway-proxy`.

---

**TestGMC_NetworkPolicy_AGCWorkerEgressToProxy**

Assert (same setup as above): the `NetworkPolicy` contains an egress rule from `app: actions-gateway-agc` to the proxy ClusterIP on port 8080 and an egress rule from `app: actions-gateway-worker` to the proxy ClusterIP on port 8080.

---

**TestGMC_NetworkPolicy_ManagedFalse_NoGitHubCIDRs**

Setup: apply an `ActionsGateway` CR with `spec.proxy.managedNetworkPolicy: false`.

Assert: the `NetworkPolicy` has no egress rules containing GitHub IP CIDRs. The within-namespace and DNS egress rules still exist.

---

**TestGMC_NetworkPolicy_IPRangeReconciler_UpdatesExistingPolicy**

Setup: start the GMC with a stub fetcher returning `["140.82.112.0/20"]`. Apply an `ActionsGateway` CR. Wait for initial reconcile.

Exercise: update the stub fetcher to return `["140.82.112.0/20", "1.2.3.0/24"]`. Trigger a manual call to `IPRangeReconciler.reconcileAll`.

Assert: the `NetworkPolicy` egress rules now include `1.2.3.0/24`. `actions_gateway_ip_range_updates_total` counter incremented by 1.

### 4.6 AGC RBAC Scope Enforcement (`rbac_scope_test.go`)

**TestGMC_AGCRBACScopeEnforcement_CrossNamespaceDenied**

Setup: provision a `team-a` `ActionsGateway` CR (so GMC creates the `actions-gateway-agc` SA and its Role/RoleBinding in `team-a`). Also provision `team-b`.

Exercise: use the `actions-gateway-agc` ServiceAccount's identity (obtained from the envtest REST config with impersonation) to list `Pods` in `team-b`.

Assert: the API server returns a `403 Forbidden` response.

Implementation note: use `rest.ImpersonateConfig` with `UserName: "system:serviceaccount:team-a:actions-gateway-agc"` on the REST config to simulate the AGC's identity. The envtest API server enforces RBAC.

---

**TestGMC_AGCRole_PermitsOwnNamespace**

Exercise: use the same impersonation to list `Pods` in `team-a`.

Assert: the API server returns `200 OK` (no error), since the Role grants pod access within `team-a`.

---

## 5. AGC Integration Tests (`cmd/agc/internal/controller/integration/`)

These tests start the `RunnerGroupReconciler` against an envtest API server with a fake broker, exercising the goroutine lifecycle, Secret management, and pod creation (verified via the API server, not actual scheduling).

### 5.1 Suite Setup (`suite_integration_test.go`)

```go
//go:build integration

var (
    k8sClient   client.Client
    brokerServer *brokertest.Server
    testEnv     *envtest.Environment
)

func TestMain(m *testing.M) {
    brokerServer = brokertest.New()
    defer brokerServer.Close()

    testEnv = &envtest.Environment{
        CRDDirectoryPaths:     []string{"../../../../config/crd"},
        ErrorIfCRDPathMissing: true,
    }
    cfg, _ := testEnv.Start()
    defer testEnv.Stop()

    // build scheme, create k8sClient from cfg
    os.Exit(m.Run())
}

// startAGCReconciler starts a RunnerGroupReconciler using the envtest
// REST config and the fake broker's URL. Cancelled by t.Cleanup.
func startAGCReconciler(t *testing.T) *controller.RunnerGroupReconciler
```

The fake broker URL is injected into the `BrokerConfig` so listener goroutines talk to the fake rather than real GitHub.

### 5.2 Reconciler Lifecycle (`reconciler_test.go`)

**TestAGC_Reconciler_CreateStartsOneListener**

Setup: apply a `RunnerGroup` CR with `maxListeners: 3`. Reconcile twice (once for finalizer, once for provisioning).

Assert: exactly 1 listener goroutine is running (the permanent baseline). The fake broker records 1 active session.

---

**TestAGC_Reconciler_BurstSpawnsAdditionalListeners**

Setup: apply a `RunnerGroup` with `maxListeners: 3`. Start the reconciler. Enqueue 3 jobs via `brokerServer.EnqueueJob`.

Assert via `gomega.Eventually`: the active session count reaches 3 (one per enqueued job), then drains back to 1 as the queue empties (simulated by the fake returning 202s after the jobs are delivered). Use `brokerServer.RegisteredSessions()` to count.

---

**TestAGC_Reconciler_ScaleMaxListeners**

Setup: `RunnerGroup` with `maxListeners: 2`. Reconcile. Then update `maxListeners: 5`. Reconcile again.

Assert: the multiplexer's `maxListeners` is now 5 (observable via `RunnerGroup.Status.ActiveSessions` ceiling or via `r.SetConditionForTest`). No in-flight goroutines were restarted.

---

**TestAGC_Reconciler_Delete_AllGoroutinesExit**

Setup: `RunnerGroup` with `maxListeners: 2`. Reconcile to provision. Then trigger deletion (set `DeletionTimestamp`, reconcile).

Assert via `gomega.Eventually`:
- All agent Secrets are deleted from the namespace
- The finalizer is removed from the `RunnerGroup`
- No listener goroutines remain (observable via `RunnerGroup.Status.ActiveSessions == 0` after a settled reconcile)

### 5.3 Secret Lifecycle (`secret_lifecycle_test.go`)

**TestAGC_SecretLifecycle_CreatedOnJobAcquire**

Setup: apply a `RunnerGroup`. Wait for the listener goroutine to start. Call `brokerServer.EnqueueJob` with a synthetic payload.

Assert via `gomega.Eventually`:
- A `Secret` with label `actions-gateway/runner-group: {rg-name}` is created in the namespace
- The Secret's `data["payload"]` is non-empty (the provisioner staged the job payload)
- The Secret's `data["plan-id"]` is set (extracted from the fake acquirejob response)

---

**TestAGC_SecretLifecycle_DeletedAfterPodCompletes**

Continuing from the previous test (or a fresh setup): simulate pod completion by creating the worker pod in the API server (in `Succeeded` phase) so the provisioner's watch loop observes it.

Assert via `gomega.Eventually`:
- The job Secret is deleted from the namespace
- No orphaned Secrets remain (list all Secrets with the runner-group label; expect 0)

Implementation note: in envtest there is no kubelet, so pods do not advance phases automatically. The test must create the pod in `Succeeded` phase directly (use `k8sClient.Create` with the pod already in `Succeeded` status, or update the pod's status after creation via the status subresource client).

### 5.4 Pod Provisioning (`pod_provisioning_test.go`)

**TestAGC_PodProvisioning_CorrectSpec**

Setup: apply a `RunnerGroup` with a `podTemplate` specifying a custom image tag and resource limits. Enqueue a job.

Assert via `gomega.Eventually` (after the provisioner creates the Pod):
- Pod exists in the namespace with label `actions-gateway/runner-group: {rg-name}`
- Pod spec matches the `RunnerGroup.Spec.PodTemplate` for user-supplied fields (custom image, resource limits)
- Pod `spec.automountServiceAccountToken` is `false` (controller-enforced invariant)
- Pod `spec.serviceAccountName` is `actions-gateway-worker` (controller-enforced)
- Pod runner container has `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` env vars set to the provisioner's configured values
- Pod runner container does NOT have `spec.hostPID`, `spec.hostNetwork`, or `spec.hostIPC` set to `true`

---

**TestAGC_PodProvisioning_PriorityTiers**

Setup: `RunnerGroup` with `priorityTiers: [{priorityClassName: "critical", threshold: 2}, {priorityClassName: "standard", threshold: 5}]`. Enqueue 3 jobs simultaneously.

Assert (after pods are created):
- First 2 pods have `spec.priorityClassName: "critical"`
- Third pod has `spec.priorityClassName: "standard"`

---

**TestAGC_PodProvisioning_MaxWorkersCeiling**

Setup: `RunnerGroup` with `maxWorkers: 2`. Enqueue 3 jobs.

Assert:
- Exactly 2 pods are created
- The third job is held (pod not created) while 2 pods are active
- After one pod transitions to `Succeeded`, the held job's pod is created

### 5.5 Failure Recovery (`failure_recovery_test.go`)

**TestAGC_FailureRecovery_PodCrash_NoSecretLeak**

Setup: provision a `RunnerGroup`. Enqueue a job. Wait for the Secret and Pod to be created.

Exercise: simulate a non-eviction pod failure by updating the pod status to `Failed` (reason: not "Evicted") via the envtest status subresource client.

Assert via `gomega.Eventually`:
- The job Secret is deleted (provisioner's watch loop cleaned it up)
- No Secrets with `actions-gateway/plan-id` label remain in the namespace
- `RunnerGroup.Status.ActiveSessions` has not increased (no infinite retry on non-eviction failure)

---

**TestAGC_FailureRecovery_EvictionTriggersRequeue**

Setup: provision a `RunnerGroup` with `maxEvictionRetries: 1`. Enqueue a job. Wait for pod creation.

Exercise: simulate an eviction by updating the pod status to `Failed` with `reason: "Evicted"`.

Assert via `gomega.Eventually`:
- The fake broker records a `POST /rerun-failed-jobs` call for the job's `run_id` (the provisioner called the rerun API)
- The job Secret is deleted
- `actions_gateway_eviction_retries_total` counter is 1

---

**TestAGC_FailureRecovery_EvictionBudgetExhausted**

Setup: same as above but configure `maxEvictionRetries: 0`.

Assert: the fake broker records NO `POST /rerun-failed-jobs` call. `actions_gateway_eviction_retries_exhausted_total` counter is 1.

### 5.6 SIGTERM Session Cleanup (`sigterm_test.go`)

**TestAGC_SIGTERM_DeletesAllSessions**

Setup: apply a `RunnerGroup` with `maxListeners: 3`. Wait for 3 sessions to register with the fake broker (via `brokerServer.EnqueueJob` to burst the listener count to 3). Confirm `brokerServer.RegisteredSessions()` length is 3.

Exercise: cancel the reconciler's context (simulating SIGTERM) and call the AGC's graceful shutdown path.

Assert within 5 seconds:
- `brokerServer.WaitForSessionDelete` returns true for each of the 3 registered session IDs
- No goroutine leak (verified via `goleak.VerifyNone(t)` in a `t.Cleanup` hook)

Implementation note: the SIGTERM handler is the multiplexer's `Stop` method (called when the reconciler's context is cancelled). The test cancels the context and then polls the fake until all session deletes are confirmed.

---

## 6. CRD Admission Tests (shared setup)

These tests live in the GMC integration suite because CRD admission (webhooks, CEL rules) is a GMC concern.

**TestCRD_ValidActionsGateway_Accepted**

Apply a fully valid `ActionsGateway` CR. Assert no admission error.

---

**TestCRD_ActionsGateway_WebhookRejectsKubeSystem**

Apply an `ActionsGateway` CR in `kube-system`. Assert admission returns a `403` or field validation error containing `"reserved"`.

Implementation note: in envtest, the admission webhook must be configured as a `ValidatingWebhookConfiguration` and the envtest server must be started with `WebhookInstallOptions`. Alternatively, call the webhook handler directly without TLS (see [controller-runtime docs](https://book.kubebuilder.io/cronjob-tutorial/writing-tests#writing-integration-tests-for-your-controller)). The latter is simpler and avoids cert-manager setup in CI.

---

**TestCRD_RunnerGroup_CELValidation_PriorityTierOrder**

Apply a `RunnerGroup` CR with `priorityTiers` in descending threshold order (invalid). Assert the API server returns a validation error containing `"ascending threshold order"`.

---

**TestCRD_RunnerGroup_CELValidation_MaxWorkersConflict**

Apply a `RunnerGroup` with `maxWorkers: 10` and a `priorityTiers` list whose last tier has `threshold: 5`. Assert the API server rejects it with the mismatch error.

---

## 7. CI Integration

Add a new workflow job in `.github/workflows/unit-test.yml` (or a new `integration-test.yml`):

```yaml
integration-test:
  runs-on: ubuntu-latest
  needs: unit-test
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with: { go-version-file: go.work }

    - name: Install envtest binaries
      run: |
        go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use \
          --bin-dir /tmp/envtest-bins --print env > /tmp/envtest.env
        cat /tmp/envtest.env >> "$GITHUB_ENV"

    - name: Run GMC integration tests
      run: go test -tags integration -timeout 5m -count=1 ./cmd/gmc/internal/controller/integration/...
      working-directory: cmd/gmc

    - name: Run AGC integration tests
      run: go test -tags integration -timeout 5m -count=1 ./cmd/agc/internal/controller/integration/...
      working-directory: cmd/agc
```

The `setup-envtest` output sets `KUBEBUILDER_ASSETS`, which `envtest.Environment.Start()` reads automatically.

---

## 8. What Is NOT Covered Here

These scenarios are deliberately out of scope for the integration layer and belong in the E2E suite (§7.3):

| Scenario | Reason deferred to E2E |
|---|---|
| Real GitHub job dispatch, execution, and log streaming | Requires live GitHub credentials and network |
| Proxy egress IP isolation | Requires network observation tools |
| HPA scaling under real load | Requires a running metrics-server and scheduler |
| Worker pod actually running the runner binary | Requires image builds and a real kubelet |
| GMC restart resilience under live traffic | Requires real job flow |
| RenewJob keeping a 15-minute job alive | Requires a live GitHub job to stay active |
| Rolling AGC upgrade under job load | Requires live traffic and real pod scheduling |
| GitHub IP range live fetch and NetworkPolicy update | Requires network access to api.github.com |

---

## 9. Test Count Summary

✅ = implemented, ☐ = not yet implemented.

| Suite | Test | Status |
|---|---|---|
| GMC provisioning | `TestGMC_TenantProvisioning_AllResourcesCreated` | ✅ |
| GMC provisioning | `TestGMC_TenantProvisioning_BootstrapRunnerGroups` | ✅ |
| GMC provisioning | `TestGMC_TenantProvisioning_NoProxyMergesDefaults` | ✅ |
| GMC provisioning | `TestGMC_TenantProvisioning_GitHubAppRefDefaultsToOwnNamespace` | ✅ |
| GMC provisioning | `TestGMC_TenantProvisioning_CredentialRotation` | ✅ |
| GMC teardown | `TestGMC_TenantTeardown_RemovesOnlyOwnedResources` | ✅ |
| GMC teardown | `TestGMC_TenantTeardown_ReapplyAfterDelete` | ✅ |
| GMC HPA update | `TestGMC_HPABoundsUpdate` | ✅ |
| GMC HPA update | `TestGMC_HPABoundsUpdate_MinReplicasClamped` | ✅ |
| GMC NetworkPolicy | `TestGMC_NetworkPolicy_ProxyEgressContainsGitHubCIDRs` | ✅ |
| GMC NetworkPolicy | `TestGMC_NetworkPolicy_AGCWorkerEgressToProxy` | ✅ |
| GMC NetworkPolicy | `TestGMC_NetworkPolicy_ManagedFalse_NoGitHubCIDRs` | ✅ |
| GMC NetworkPolicy | `TestGMC_NetworkPolicy_IPRangeReconciler_UpdatesExistingPolicy` | ✅ |
| GMC RBAC scope | `TestGMC_AGCRBACScopeEnforcement_CrossNamespaceDenied` | ✅ |
| GMC RBAC scope | `TestGMC_AGCRole_PermitsOwnNamespace` | ✅ |
| CRD admission | `TestCRD_ValidActionsGateway_Accepted` | ✅ |
| CRD admission | `TestCRD_ActionsGateway_WebhookRejectsKubeSystem` | ✅ |
| CRD admission | `TestCRD_RunnerGroup_CELValidation_PriorityTierOrder` | ✅ |
| CRD admission | `TestCRD_RunnerGroup_CELValidation_MaxWorkersConflict` | ✅ |
| AGC reconciler lifecycle | `TestAGC_Reconciler_CreateStartsOneListener` | ✅ |
| AGC reconciler lifecycle | `TestAGC_Reconciler_BurstSpawnsAdditionalListeners` | ✅ |
| AGC reconciler lifecycle | `TestAGC_Reconciler_ScaleMaxListeners` | ✅ |
| AGC reconciler lifecycle | `TestAGC_Reconciler_Delete_AllGoroutinesExit` | ✅ |
| AGC Secret lifecycle | `TestAGC_SecretLifecycle_CreatedOnJobAcquire` | ✅ |
| AGC Secret lifecycle | `TestAGC_SecretLifecycle_DeletedAfterPodCompletes` | ✅ |
| AGC pod provisioning | `TestAGC_PodProvisioning_CorrectSpec` | ✅ |
| AGC pod provisioning | `TestAGC_PodProvisioning_PriorityTiers` | ✅ |
| AGC pod provisioning | `TestAGC_PodProvisioning_MaxWorkersCeiling` | ✅ |
| AGC failure recovery | `TestAGC_FailureRecovery_PodCrash_NoSecretLeak` | ✅ |
| AGC failure recovery | `TestAGC_FailureRecovery_EvictionTriggersRequeue` | ✅ |
| AGC failure recovery | `TestAGC_FailureRecovery_EvictionBudgetExhausted` | ✅ |
| AGC SIGTERM cleanup | `TestAGC_SIGTERM_DeletesAllSessions` | ✅ |
| **Total** | **32 / 32 implemented** | |
