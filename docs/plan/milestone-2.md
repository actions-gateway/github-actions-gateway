# Milestone 2 Implementation Plan — AGC Controller & Reconciler

← [Milestone 1](milestone-1.md) | [Back to implementation phases](../design/06-implementation-phases.md)

---

## Table of Contents

- [Overview](#overview)
- [1. Repository Scaffolding](#1-repository-scaffolding)
- [2. RunnerGroup CRD](#2-runnergroup-crd)
- [3. Package Design](#3-package-design)
- [4. Investigation Tasks](#4-investigation-tasks)
- [5. Job Handler Stub (Milestone 2 Placeholder for Pod Provisioner)](#5-job-handler-stub-milestone-2-placeholder-for-pod-provisioner)
- [6. Metrics](#6-metrics)
- [7. Test Plan](#7-test-plan)
- [8. Success Criteria Checklist](#8-success-criteria-checklist)
- [9. Risks and Mitigations](#9-risks-and-mitigations)
- [10. Deferred to Later Milestones](#10-deferred-to-later-milestones)
- [11. Investigation Findings](#11-investigation-findings)

## Overview

**Goal:** Produce a deployable Actions Gateway Controller (AGC) binary under `cmd/agc/` that reconciles `RunnerGroup` Custom Resources into adaptive listener goroutine pools. At rest the AGC maintains exactly one long-polling goroutine per RunnerGroup; additional goroutines spawn on demand as jobs arrive and wind down once the queue drains. No actual worker pods are created in this milestone — job acquisition is confirmed and handed off to a stub that records the acquisition, allowing the full goroutine lifecycle and token management machinery to be exercised without the Kubernetes pod-provisioning complexity of Milestone 3.

**Duration:** Days 5–10

**Foundation:** All packages from Milestone 1 (`broker`, `githubapp`) are consumed unchanged. The AGC is a new `cmd/agc/` module added to the workspace.

**Definition of Done:**

- Creating, scaling `maxListeners`, and deleting a `RunnerGroup` in a local `kind` cluster produces no goroutine leaks (verified via `pprof` and `goleak`) and no orphaned Kubernetes resources.
- The goroutine count at rest is exactly one per RunnerGroup, regardless of `maxListeners`.
- The Token Manager proactively refreshes before expiry without restarting in-flight goroutines.
- Sustained `429 Too Many Requests` responses surface a `RateLimited` condition on the `RunnerGroup` within 10 minutes.
- A `400 Bad Request` (version too old) from `POST /sessions` surfaces a `RunnerVersionTooOld` condition rather than spinning in a tight retry loop.
- Agent pool Secrets are created on RunnerGroup creation and deleted on RunnerGroup deletion, with no leaks on error paths.
- All unit tests pass under `go test -race ./...` from the repo root.
- Code is committed to the repository.

---

## 1. Repository Scaffolding

### 1.1 New module and workspace entry

The AGC binary lives in its own module, parallel to `cmd/probe/`:

```
mkdir -p cmd/agc
cd cmd/agc
go mod init github.com/actions-gateway/github-actions-gateway/agc
```

Add the new module to `go.work`:

```
go work use ./cmd/agc
```

The updated workspace (`go.work`) lists three modules:

```
go 1.22

use (
    .            // root shared library module
    ./cmd/probe  // Milestone 1
    ./cmd/agc    // Milestone 2
)
```

### 1.2 Kubebuilder bootstrapping

Scaffold the AGC operator using `kubebuilder init` and `kubebuilder create api` inside `cmd/agc/`. This produces the standard controller-runtime layout. Keep the generated files but replace the reconciler stub with the implementation below. The CRD manifest is generated from struct markers and committed under `cmd/agc/config/crd/`.

Required tools: `kubebuilder` (v3.x), `controller-gen`. Pin their versions in a `Makefile` target so the manifests are reproducible.

### 1.3 Directory layout

```
github-actions-gateway/
├── go.work                               # updated: adds ./cmd/agc
├── go.mod / broker/ / githubapp/         # unchanged from Milestone 1
└── cmd/
    ├── probe/                            # Milestone 1 — unchanged
    └── agc/
        ├── go.mod                        # module: github.com/actions-gateway/github-actions-gateway/agc
        ├── main.go                       # operator entry point
        ├── api/
        │   └── v1alpha1/
        │       ├── runnergroup_types.go  # RunnerGroup CRD struct + markers
        │       └── groupversion_info.go  # scheme registration
        ├── internal/
        │   ├── controller/
        │   │   └── runnergroup_controller.go  # reconciler
        │   ├── agentpool/
        │   │   ├── pool.go               # Agent pool: register, store, assign, release, deregister
        │   │   └── pool_test.go
        │   ├── token/
        │   │   ├── manager.go            # Token Manager: mutex, proactive refresh
        │   │   └── manager_test.go
        │   └── listener/
        │       ├── goroutine.go          # single listener goroutine lifecycle
        │       ├── goroutine_test.go
        │       ├── multiplexer.go        # per-RunnerGroup goroutine pool management
        │       └── multiplexer_test.go
        └── config/
            ├── crd/                      # generated CRD manifests
            ├── rbac/                     # generated RBAC manifests
            └── default/                  # kustomize base
```

Shared packages (`broker`, `githubapp`) are imported as `github.com/actions-gateway/github-actions-gateway/broker` and `.../ githubapp`; the workspace resolves them locally without a published release.

### 1.4 New dependencies (AGC module `go.mod`)

| Package | Purpose |
|---|---|
| `sigs.k8s.io/controller-runtime` | Operator framework: reconciler, fake client, envtest |
| `k8s.io/client-go` | Kubernetes typed clients |
| `k8s.io/api` + `k8s.io/apimachinery` | Kubernetes API types |
| `sigs.k8s.io/controller-runtime/pkg/client/fake` | Fake client for unit tests |
| `github.com/stretchr/testify` | Test assertions |
| `go.uber.org/goleak` | Goroutine leak detection |

No additional GitHub App or crypto dependencies are needed — those are in the root module.

---

## 2. RunnerGroup CRD

### 2.1 Type definition (`api/v1alpha1/runnergroup_types.go`)

The full Go struct and kubebuilder markers are specified in [§3.1 of the API contracts](../design/03-api-contracts.md). Key field summary:

| Field | Default | Notes |
|---|---|---|
| `maxListeners` | 10 | Max concurrent goroutines; at-rest count is always 1 |
| `maxWorkers` | nil | Optional pod-count ceiling; enforced in M3 pod provisioner |
| `runnerLabels` | required | Label set matched against workflow `runs-on` |
| `priorityTiers` | nil | Out of scope for M2; used by pod provisioner in M3 |
| `podTemplate` | required | Forwarded to worker pod in M3; validated but not used in M2 |
| `workerImage` | "" | Falls back to `DefaultWorkerImage` constant in M3 |

Add CEL validation markers:

- `priorityTiers` must be in strictly ascending threshold order.
- `maxWorkers` must equal the last `priorityTiers` threshold when both are set.

The status subresource exposes `ActiveSessions`, `ObservedGeneration`, and a `Conditions` slice with the following condition types:

| Condition | True when | False / Unknown when |
|---|---|---|
| `Ready` | ≥1 listener running, no fatal errors | AGC is starting or a fatal error has occurred |
| `Degraded` | Listener goroutines exiting faster than they respawn | - |
| `RateLimited` | `429` sustained > 10 minutes on this RunnerGroup | - |
| `RunnerVersionTooOld` | `POST /sessions` returns 400 version-too-old | - |

### 2.2 Scheme registration

Register both `RunnerGroup` and the scheme in `api/v1alpha1/groupversion_info.go`. Add the scheme to the manager in `main.go` alongside `client-go`'s core v1 scheme (needed for Secret CRUD in Milestone 2).

### 2.3 CRD generation and validation

Run `make generate manifests` (backed by `controller-gen`) to produce the CRD YAML from the type markers. Commit the generated files. The `Makefile` must be runnable without network access after the initial `go mod download`.

---

## 3. Package Design

### 3.1 `internal/token` — Token Manager

The Token Manager holds the current installation access token in a `sync.RWMutex`-protected struct shared across all session goroutines. It must never block readers during a refresh.

```go
// Manager holds a thread-safe, proactively refreshed installation access token.
type Manager struct {
    provider  githubapp.ExpiringTokenProvider
    mu        sync.RWMutex
    current   *githubapp.InstallationToken // nil before first fetch
    clock     Clock                        // injectable for tests
}

// Token returns the current valid token. Blocks only during the initial fetch
// on first call; subsequent calls take the read lock and return immediately.
// Returns an error only if the current token is expired and the refresh failed.
func (m *Manager) Token(ctx context.Context) (string, error)

// Start begins the background refresh loop. Refresh fires at T-5 minutes before
// the current token's ExpiresAt. The loop exits when ctx is cancelled.
// Must be called once before Token() is used by session goroutines.
func (m *Manager) Start(ctx context.Context)
```

**Implementation notes:**

- The refresh goroutine wakes at `expiresAt - 5min`. On wake it calls `provider.TokenWithExpiry`, acquires the write lock long enough to swap `current`, then releases it. Readers calling `Token()` hold the read lock for the duration of their call — because the lock is held only for the pointer swap (not the HTTP round-trip), readers are never blocked waiting for a network call.
- On refresh failure: retry with exponential backoff (5s → 60s cap). Emit `actions_gateway_token_refresh_errors_total`. If the old token expires before refresh succeeds, `Token()` returns an error wrapping "token expired" so caller goroutines can surface the degraded condition on their RunnerGroup.
- The `Clock` interface (`Now() time.Time`, `After(d time.Duration) <-chan time.Time`) is injected; tests use a fake clock to advance time without sleeping.

**`manager_test.go` — what to cover:**

| Test | What it verifies |
|---|---|
| `TestManager_ProactiveRefresh` | Fake clock advanced to T-5 min before expiry. Assert new token fetched before old one expires; no goroutine blocked. |
| `TestManager_ReadersDuringRefresh` | 10 goroutines calling `Token()` concurrently while a refresh is in flight. Assert all return a valid (old or new) token, never an empty string or a lock contention error. |
| `TestManager_RefreshFailureFallback` | Provider returns error. Assert old token still returned until it expires; backoff retries with increasing delay (fake clock). |
| `TestManager_TokenExpiredAfterFailure` | Clock advanced past expiry while provider keeps failing. Assert `Token()` returns an error wrapping "token expired". |
| `TestManager_NoLeakOnCancel` | Cancel the context. Assert the background goroutine exits; `goleak.VerifyNone`. |

### 3.2 `internal/agentpool` — Agent Pool Manager

Each RunnerGroup requires up to `maxListeners` pre-registered runner agents, one per concurrent goroutine. The pool manager creates agent registrations at RunnerGroup provisioning time and persists credentials in Kubernetes Secrets so they survive AGC restarts.

#### 3.2.1 Runner registration

Registering a runner agent requires two GitHub API calls:

1. **Registration token:** `POST /orgs/{org}/actions/runners/registration-token` (or the repo-scoped equivalent) using the installation access token. Returns a short-lived `token` string.
2. **Runner registration:** `POST {broker_url}agent` with `{"name": "<agent-name>", "version": "<runner-version>", "labels": [...], "groupName": "<runner-group>"}` and `Authorization: Bearer <registration-token>`. GitHub responds with the agent's numeric `id` and its OAuth credentials (`clientId`, `authorizationUrl`, and an RSA public key). The private key is generated locally; the public key is registered with GitHub.

**Note:** The exact registration endpoint and payload are not yet confirmed. This is Investigation Task A for Milestone 2 (§4.A below).

#### 3.2.2 Agent Secret schema

Each registered agent's credentials are stored in a Kubernetes Secret in the RunnerGroup's namespace, named `agentpool-{runnergroup}-{index}`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: agentpool-my-runnergroup-0
  namespace: team-a
  labels:
    app.kubernetes.io/managed-by: actions-gateway-agc
    actions-gateway/runner-group: my-runnergroup
    actions-gateway/agent-index: "0"
type: Opaque
stringData:
  agentId: "12345"          # numeric agent ID from GitHub
  clientId: "..."           # OAuth2 client ID for runner OAuth flow
  authorizationUrl: "..."   # OAuth2 token endpoint
  privateKeyPEM: |          # RSA private key generated locally during registration
    -----BEGIN RSA PRIVATE KEY-----
    ...
    -----END RSA PRIVATE KEY-----
  runnerVersion: "2.327.1"  # version string used at session creation
  brokerURL: "..."          # broker_url from CreateSession (static per agent)
```

#### 3.2.3 Pool exported surface

```go
// Agent holds the credentials for one pre-registered runner agent.
type Agent struct {
    Index          int
    AgentID        int64
    Creds          *githubapp.RunnerCredentials
    PrivateKey     *rsa.PrivateKey
    RunnerVersion  string
    BrokerURL      string
}

// Pool manages the lifecycle of pre-registered runner agents for one RunnerGroup.
// It creates, loads, and deregisters agent Secrets.
type Pool struct {
    client    client.Client   // Kubernetes client
    namespace string
    groupName string
    maxAgents int32
}

// EnsureAgents reconciles the pool to exactly count agents:
// creating missing Secrets (and registering with GitHub) and
// deregistering and deleting excess Secrets.
// Idempotent: safe to call on every reconcile loop.
func (p *Pool) EnsureAgents(ctx context.Context, count int32, token string) error

// LoadAgents reads all existing agent Secrets for this pool and returns them
// in index order. Called on AGC startup to reconstruct state after a restart.
func (p *Pool) LoadAgents(ctx context.Context) ([]*Agent, error)

// ClaimAgent atomically marks an agent as in-use and returns it.
// Returns nil if no agent is currently available.
func (p *Pool) ClaimAgent() *Agent

// ReleaseAgent returns an agent to the available pool.
func (p *Pool) ReleaseAgent(a *Agent)

// DeleteAll deregisters all agents from GitHub and deletes all Secrets.
// Called when a RunnerGroup is deleted.
func (p *Pool) DeleteAll(ctx context.Context, token string) error
```

**`pool_test.go` — what to cover:**

| Test | What it verifies |
|---|---|
| `TestPool_EnsureAgents_Creates` | Pool with 0 existing Secrets, target=3. Assert 3 Secrets created with correct labels and non-empty fields. |
| `TestPool_EnsureAgents_Idempotent` | Pool with 3 Secrets, target=3. Assert no Secrets created or deleted. |
| `TestPool_EnsureAgents_ScaleDown` | Pool with 5 Secrets, target=3. Assert 2 excess Secrets deleted and GitHub deregistration called. |
| `TestPool_LoadAgents_Order` | Secrets with indices 0,1,2 in arbitrary create order. Assert LoadAgents returns them sorted by index. |
| `TestPool_ClaimRelease` | Claim all agents, assert ClaimAgent returns nil when pool exhausted. Release one, assert it can be claimed again. |
| `TestPool_DeleteAll` | Assert all Secrets deleted and GitHub called once per agent for deregistration. |
| `TestPool_CreateSecretFailure` | Kubernetes Secret creation returns error. Assert EnsureAgents returns error; no partial state left. |

### 3.3 `internal/listener` — Listener Goroutine and Multiplexer

#### 3.3.1 `goroutine.go` — single listener goroutine

One listener goroutine corresponds to one claimed agent from the pool. Its lifecycle:

1. Claim an agent from the pool. If no agent is available, backoff 1s and retry up to 3 times before returning an error (pool exhausted — log and wait for a release).
2. Fetch a token from the Token Manager.
3. Call `BrokerClient.CreateSession` with the agent's credentials (OAuth flow via `githubapp.FetchRunnerOAuthToken`). On `VersionTooOldError`, set the `RunnerVersionTooOld` condition on the RunnerGroup and exit without retrying.
4. Enter the `GetMessage` loop:
   - On `nil` (202): increment the consecutive-empty counter. If counter ≥ 50 **and** this goroutine is not the last listener for its RunnerGroup, call `DeleteSession` and exit (idle shutdown).
   - On `RateLimitError`: sleep for the indicated `RetryAfter` duration (or exponential backoff if none), increment the poll-error counter, and loop.
   - On other errors: apply the two-tier backoff (≤5 errors: 15–30s jitter; >5 errors: 30–60s jitter) and loop.
   - On a `RunnerJobRequest` message: reset the counter, call `AcquireJob`, and hand off.
5. On `AcquireJob` success:
   - Notify the Multiplexer to spawn a replacement listener (so polling capacity is maintained for the next job).
   - Start a `RenewJob` goroutine (see §3.4).
   - Call the JobHandler (injected function, stub in M2; pod provisioner in M3).
   - Loop back to step 4 on the same session (session reuse confirmed in Investigation C of Milestone 1).
6. On context cancellation: call `DeleteSession` and release the agent back to the pool.

**Non-retriable conditions** (set RunnerGroup condition, exit goroutine, do not spin):

- Session not found / pool not found (404)
- Unauthorized / access denied (401, 403)
- `VersionTooOldError`

**Session-expired condition** (distinct from the above): call `DeleteSession` (best-effort), then re-create the session from step 3 rather than exiting entirely.

```go
// Config holds the dependencies injected into a listener goroutine.
type Config struct {
    Group        string           // RunnerGroup name
    Namespace    string
    Agent        *agentpool.Agent
    TokenManager *token.Manager
    Broker       *broker.BrokerClient
    Conditions   ConditionUpdater // updates RunnerGroup status conditions
    Metrics      *Metrics
    IdleThreshold int              // consecutive 202s before idle shutdown; default 50
    JobHandler   JobHandlerFunc   // called with raw acquirejob response bytes
    Clock        Clock
}

// JobHandlerFunc is called with the raw AcquireJobResponse bytes after a
// successful AcquireJob. In M2 this is a stub; in M3 it becomes the pod provisioner.
type JobHandlerFunc func(ctx context.Context, runServiceURL, planID string, payload []byte) error

// Run executes the listener goroutine. It blocks until the context is cancelled
// or an unrecoverable error occurs (VersionTooOldError, unauthorized).
// The caller (Multiplexer) is responsible for restarting it after a recoverable exit.
func Run(ctx context.Context, cfg Config) error
```

#### 3.3.2 `multiplexer.go` — per-RunnerGroup goroutine pool

The Multiplexer is the per-RunnerGroup component that the reconciler manages. It owns the set of running listener goroutines and enforces the `maxListeners` ceiling.

```go
// Multiplexer manages the adaptive pool of listener goroutines for one RunnerGroup.
type Multiplexer struct {
    mu          sync.Mutex
    active      map[int]*listenerState  // goroutine index → state
    maxListeners int32
    // ... deps injected at construction
}

// Start launches the permanent baseline listener. Must be called once when
// the RunnerGroup is first reconciled.
func (m *Multiplexer) Start(ctx context.Context) error

// SetMaxListeners updates the ceiling. If the new ceiling is lower than the
// current active count, excess idle goroutines shut down at their next 202.
// In-flight (job-holding) goroutines are not interrupted.
func (m *Multiplexer) SetMaxListeners(max int32)

// SpawnReplacement spawns one additional listener goroutine if the active
// count is below maxListeners. Called by a listener goroutine after it acquires
// a job, to maintain polling capacity for the next job.
func (m *Multiplexer) SpawnReplacement(ctx context.Context)

// ActiveCount returns the current number of running listener goroutines.
func (m *Multiplexer) ActiveCount() int32

// Stop cancels all listener goroutines and waits for them to exit cleanly.
// Called during RunnerGroup deletion or AGC shutdown.
func (m *Multiplexer) Stop()
```

**Invariants enforced by the Multiplexer:**

- At least one listener goroutine is always running per RunnerGroup (the permanent baseline). If the baseline goroutine exits unexpectedly (recoverable error), the Multiplexer restarts it after a brief backoff (1s → 30s cap).
- `activeCount ≤ maxListeners` at all times. `SpawnReplacement` is a no-op when the ceiling is reached.
- On `Stop()`, every goroutine that holds an open session issues `DeleteSession` before the Multiplexer returns — this is the graceful shutdown requirement.

**`multiplexer_test.go` — what to cover:**

| Test | What it verifies |
|---|---|
| `TestMultiplexer_AtRestOneGoroutine` | Start with maxListeners=5. Assert exactly 1 goroutine running after settling. |
| `TestMultiplexer_SpawnOnAcquire` | Simulate job acquisition. Assert SpawnReplacement increments count to 2, not above. |
| `TestMultiplexer_CeilingRespected` | SpawnReplacement called repeatedly with maxListeners=3 and 3 already active. Assert count stays at 3. |
| `TestMultiplexer_IdleShutdown` | Drive 50 consecutive 202 responses to goroutine #2 while #1 is permanent. Assert #2 exits; #1 remains. |
| `TestMultiplexer_RestartOnCrash` | Baseline goroutine exits with a recoverable error. Assert Multiplexer restarts it within 1s (fake clock). |
| `TestMultiplexer_SetMaxListenersDown` | Active count=5, set maxListeners=2. Assert 3 goroutines idle-exit; 2 remain. |
| `TestMultiplexer_StopCleanly` | Call Stop(). Assert all goroutines issue DeleteSession and exit; goleak.VerifyNone. |
| `TestMultiplexer_StopUnblocksDeleteSession` | Goroutine is mid-long-poll when Stop() is called. Assert context cancels the poll; DeleteSession is still called. |

### 3.4 `internal/listener` — RenewJob goroutine

Each acquired job spawns a separate `renewjob` goroutine that is a sibling of the listener goroutine, not a child. It must not block the listener from looping.

```go
// StartRenewLoop starts the per-job renewal goroutine and returns a function
// that stops it. The caller must call stop() when the job completes or fails.
func StartRenewLoop(
    ctx context.Context,
    client *broker.BrokerClient,
    runServiceURL, planID, jobID string,
    metrics *Metrics,
    clock Clock,
) (stop func())
```

**Implementation notes:**

- Tick every 60 seconds. On `RenewJob` failure: log the error, emit `actions_gateway_renewjob_errors_total`, and continue (the lock grants ~10 minutes; one missed renewal is not fatal).
- On context cancellation (from `stop()`): exit cleanly without a final `RenewJob` call.
- `goleak.VerifyNone` must pass after `stop()` is called.

**`goroutine_test.go` — what to cover:**

| Test | What it verifies |
|---|---|
| `TestRenewLoop_TicksAt60s` | Fake clock advanced 3 × 60s. Assert RenewJob called exactly 3 times. |
| `TestRenewLoop_StopsOnStop` | Call stop(). Assert goroutine exits; goleak.VerifyNone. |
| `TestRenewLoop_NonOKContinues` | Stub returns 500 twice, then 200. Assert goroutine does not exit; call count is 3. |
| `TestRenewLoop_NoCallAfterStop` | Stop called mid-interval. Assert no further RenewJob calls after stop returns. |

### 3.5 `internal/controller/runnergroup_controller.go` — Reconciler

The reconciler is the controller-runtime entry point. It watches `RunnerGroup` objects and drives the Multiplexer and Agent Pool to match the desired spec.

**Reconcile logic (idempotent):**

1. Fetch the `RunnerGroup` object. If not found, return (object was deleted before finalizer processing).
2. If the `RunnerGroup` has a deletion timestamp:
   a. Call `multiplexer.Stop()` for this RunnerGroup (if running).
   b. Call `pool.DeleteAll()` to deregister agents and delete Secrets.
   c. Remove the finalizer and update the object. Return.
3. Ensure the finalizer `actions-gateway.github.com/agentpool-cleanup` is set. Patch and requeue if just added.
4. Call `pool.EnsureAgents(maxListeners, token)`. Requeue with backoff on error.
5. If no Multiplexer exists for this RunnerGroup, create one and call `multiplexer.Start()`.
6. Call `multiplexer.SetMaxListeners(spec.maxListeners)`.
7. Update `RunnerGroupStatus.ActiveSessions` from `multiplexer.ActiveCount()`.
8. Set the `Ready` condition based on whether at least one listener is running.
9. Update status subresource and return.

**Condition updates from listener goroutines** are submitted via a channel that the reconciler drains on each reconcile cycle. This avoids goroutines calling `client.Status().Update()` directly, which would require them to handle conflicts.

**Multiplexer registry:** The reconciler holds a `map[types.NamespacedName]*listener.Multiplexer` protected by a `sync.Mutex`. This is in-process state that is rebuilt from Kubernetes Secrets (via `pool.LoadAgents`) on AGC restart.

**`runnergroup_controller_test.go` — what to cover (using `controller-runtime/pkg/client/fake`):**

| Test | What it verifies |
|---|---|
| `TestReconcile_Create` | New RunnerGroup with maxListeners=3. Assert 3 agent Secrets created, Multiplexer started, status Ready=true. |
| `TestReconcile_ScaleUp` | Existing RunnerGroup, maxListeners increased 2→5. Assert 3 new agent Secrets created, multiplexer ceiling updated. |
| `TestReconcile_ScaleDown` | Existing RunnerGroup, maxListeners decreased 5→2. Assert 3 excess agent Secrets deleted, multiplexer ceiling updated. |
| `TestReconcile_Delete` | RunnerGroup with deletion timestamp. Assert Multiplexer stopped, all Secrets deleted, finalizer removed. |
| `TestReconcile_DeleteSecretFailure` | Secret deletion returns error during RunnerGroup deletion. Assert reconciler requeues; finalizer not removed. |
| `TestReconcile_VersionTooOldCondition` | Listener goroutine sets RunnerVersionTooOld condition via channel. Assert condition appears on status after next reconcile. |
| `TestReconcile_RateLimitedCondition` | 429s sustained for 10 minutes (fake clock). Assert RateLimited condition set on status. |
| `TestReconcile_StatusActiveSessions` | Multiplexer reports 3 active goroutines. Assert status.activeSessions == 3 after reconcile. |

---

## 4. Investigation Tasks

### 4.A — Runner Agent Registration API

**Context:** Milestone 1 used a runner pre-registered manually via GitHub's `config.sh`. The AGC must register agents programmatically when a RunnerGroup is created. The GitHub API endpoint and payload for programmatic runner registration in the v2 broker flow are not yet confirmed. The probe's `runner_auth.go` handles the OAuth exchange for an already-registered runner, but does not implement registration itself.

**How to investigate:**

1. Capture a `config.sh` registration session with `--debug` and an HTTP-level proxy (e.g. mitmproxy) to observe the exact API calls made during registration.
2. Identify the endpoint, request body, and response schema for runner registration.
3. Determine whether the AGC can use the installation access token for registration, or whether it needs a registration token obtained from a separate GitHub API call (`POST /orgs/{org}/actions/runners/registration-token`).
4. Identify the deregistration endpoint (called when a RunnerGroup is deleted or scaled down).
5. Determine whether pre-registered agents expire or must be refreshed; document the TTL if any.

**Expected outcomes:**

- Document the registration endpoint, request/response schema, and required auth token in a code comment block at the top of `agentpool/pool.go`, and update [§3.3](../design/03-api-contracts.md#33-re-implemented-broker-api-endpoints) of the API contracts doc.
- If the installation token is not sufficient for registration (requires a separate registration token): add a `RegisterRunner` function to the `githubapp` package and its test.
- If agents expire: add a proactive re-registration step to the Pool, analogous to the Token Manager's proactive refresh.

**Document findings:** Add §8.A to the Investigation Findings section at the bottom of this file before closing the milestone.

### 4.B — Live Egress IP Variance Test (deferred from Milestone 1)

**Context:** Milestone 1 scaffolded `TestEgressIPVariance_Live` in `broker/egress_ip_test.go` but left it with a `TODO(investigation-b)` because the full broker protocol was not wired into a test-callable form at the time. Now that `NewInstallationTokenProvider` and `BrokerClient` are both fully integrated via the AGC, the live test can be run without duplicating `cmd/probe/main.go`.

**How to run:**

1. In `broker/egress_ip_test.go`, complete `TestEgressIPVariance_Live` by wiring it to `NewInstallationTokenProvider` and `BrokerClient` (credentials from env vars, identical to the probe's startup sequence).
2. Run through: `CreateSession` → `GetMessage` (one poll) → `DeleteSession`, with each request routed through a different local CONNECT proxy.
3. Assert all calls succeed with no authentication or session errors.

**Expected outcomes:**

- If all calls succeed: remove the `TODO(investigation-b)` marker and update §8.B in `docs/plan/milestone-1.md` to record that the live test has now been confirmed.
- If any call fails: evaluate `sessionAffinity: ClientIP` on the Milestone 4 proxy Service and document the revised proxy design before Milestone 4 begins.

**Document findings:** Update §8.B in `docs/plan/milestone-1.md`.

---

## 5. Job Handler Stub (Milestone 2 Placeholder for Pod Provisioner)

The listener goroutine calls `JobHandlerFunc` after a successful `AcquireJob`. In Milestone 3, this becomes the real pod provisioner. In Milestone 2, inject a stub that:

1. Logs the `planID` and payload length at INFO level.
2. Increments `actions_gateway_jobs_acquired_total`.
3. Returns `nil` immediately (simulating a successfully "handled" job).

This ensures the goroutine lifecycle, RenewJob loop, and session-reuse path are all exercised in tests without any Kubernetes pod-creation dependency.

The `maxWorkers` field is parsed and stored during reconciliation but the ceiling enforcement logic is **not yet implemented** in Milestone 2. Add a `TODO(milestone-3): enforce maxWorkers ceiling in pod provisioner` comment in the reconciler at the point where pod creation will eventually be gated.

---

## 6. Metrics

Emit the following Prometheus metrics from the AGC in Milestone 2. Use `controller-runtime`'s metrics server (default `:8080/metrics`). All metrics are declared in `internal/listener/metrics.go` and registered in `main.go`.

| Metric | Type | Labels | Where emitted |
|---|---|---|---|
| `actions_gateway_active_sessions` | Gauge | `namespace`, `runner_group` | Multiplexer on goroutine start/stop |
| `actions_gateway_jobs_acquired_total` | Counter | `namespace`, `runner_group` | Listener after successful AcquireJob |
| `actions_gateway_job_acquisition_errors_total` | Counter | `namespace`, `reason` | Listener on AcquireJob failure |
| `actions_gateway_token_refreshes_total` | Counter | `namespace` | Token Manager on successful refresh |
| `actions_gateway_token_refresh_errors_total` | Counter | `namespace` | Token Manager on refresh failure |
| `actions_gateway_renewjob_errors_total` | Counter | `namespace` | RenewJob goroutine on non-OK response |
| `actions_gateway_message_poll_errors_total` | Counter | `namespace`, `reason` | Listener on GetMessage non-202/non-job error |

The `controller-runtime` framework emits reconcile latency and queue depth automatically.

---

## 7. Test Plan

### 7.1 Unit Tests (`go test -race ./...` from repo root)

All unit tests must run without network access. Use `httptest.NewServer` for any HTTP interaction. All tests must pass under `-race`. Tests are spread across the packages described in §3 above; this section consolidates the full list.

**Token Manager** — see §3.1 test table.

**Agent Pool** — see §3.2.3 test table.

**Listener goroutine** — see §3.3.1 embedded tests, plus:

| Test | What it verifies |
|---|---|
| `TestListener_CreateSessionVersionTooOld` | Stub returns 400 with version-too-old body. Assert `RunnerVersionTooOld` condition set; goroutine exits without retrying. |
| `TestListener_CreateSessionUnauthorized` | Stub returns 401. Assert goroutine exits with non-retriable condition; does not retry. |
| `TestListener_GetMessage202Loop` | Stub returns 202 for 49 polls, then for the 50th. Assert goroutine continues (below idle threshold). |
| `TestListener_IdleShutdownAt50` | Stub returns 202 for 50 consecutive polls; goroutine is not the last listener. Assert DeleteSession called and goroutine exits. |
| `TestListener_IdleNotShutdownIfLast` | Goroutine is the last listener, 50+ consecutive 202s. Assert goroutine does NOT exit. |
| `TestListener_SessionExpiredRecreates` | Stub returns session-expired error. Assert DeleteSession called; new CreateSession issued; polling resumes. |
| `TestListener_RateLimitBackoff` | Stub returns 429 with Retry-After: 30. Assert goroutine sleeps ~30s (fake clock) before retrying; counter incremented. |
| `TestListener_AcquireJobThenReuse` | Stub delivers one job, then returns 202. Assert listener calls AcquireJob, then re-enters GetMessage on the same sessionId. |
| `TestListener_SpawnReplacementOnAcquire` | Listener acquires a job. Assert Multiplexer.SpawnReplacement called exactly once. |

**Multiplexer** — see §3.3.2 test table.

**RenewJob goroutine** — see §3.4 test table.

**Reconciler** — see §3.5 test table.

**AGC reconciler RBAC boundary assertion:**

| Test | What it verifies |
|---|---|
| `TestAGCClusterRoleNoWildcards` | Enumerate the generated ClusterRole rules. Assert no rule grants `*` verbs on `secrets`, `pods`, or `nodes`. |

### 7.2 Integration Tests (envtest)

Run against a local `envtest` API server from `controller-runtime`. No real GitHub calls. A shared `httptest` stub broker replays canned responses.

**Setup:** Install both the `RunnerGroup` CRD schema into `envtest`. Wire the stub broker to serve:
- `POST .../session` → 200 with a valid session response
- `GET .../message?sessionId=...` → 202 (no job) by default; configurable per-test to return a job message
- `POST .../acquirejob` → 200 with a stub job payload
- `POST .../renewjob` → 200 with a future `lockedUntil`
- `DELETE .../session` → 200

| Scenario | Pass Criterion |
|---|---|
| RunnerGroup create | Apply a RunnerGroup CR. Assert `maxListeners` agent Secrets created, `status.activeSessions == 1`, `Ready` condition true. |
| RunnerGroup scale up | Patch maxListeners from 2 to 5. Assert 3 new agent Secrets appear; active session count stays at 1 (at rest). |
| RunnerGroup scale down | Patch maxListeners from 5 to 2. Assert 3 excess agent Secrets deleted; no goroutine leaks. |
| RunnerGroup delete | Delete RunnerGroup CR. Assert Multiplexer stopped, all agent Secrets deleted, finalizer removed, goroutine count 0. |
| Job acquisition cycle | Configure stub to deliver one job. Assert: AcquireJob called, replacement listener spawned (active=2 briefly then back to 1 after idle timeout), RenewJob loop runs at least once, stub JobHandler called. |
| SIGTERM graceful shutdown | Send SIGTERM to the manager. Assert all sessions receive DeleteSession before process exit. Verify within `terminationGracePeriodSeconds`. |
| Agent Secret persistence | Restart the AGC manager (stop and restart the controller in-process). Assert Multiplexer is reconstructed from existing agent Secrets without re-registering with GitHub. |

### 7.3 Manual kind Cluster Verification

After integration tests pass, deploy the AGC to a local `kind` cluster with real GitHub credentials and a test RunnerGroup. Verify:

1. `kubectl get runnergroup -o yaml` shows `Ready: true` and `activeSessions: 1`.
2. Queue a workflow job; verify it is acquired (check AGC logs for `"job acquired"`) and the `actions_gateway_jobs_acquired_total` counter increments via `kubectl port-forward`.
3. `kubectl patch runnergroup ... --patch '{"spec":{"maxListeners":3}}'` → verify 3 agent Secrets exist.
4. `kubectl delete runnergroup ...` → verify all agent Secrets are deleted and the AGC logs show `DeleteSession` for each.
5. Inspect `pprof` goroutine dump at rest: exactly one listener goroutine per RunnerGroup.

---

## 8. Success Criteria Checklist

- [x] `go build ./cmd/agc/` succeeds with no warnings (from repo root via workspace).
- [x] `go test -race ./...` passes with zero failures across all three modules (root, probe, agc).
- [x] `goleak.VerifyNone` passes in all goroutine-spawning tests.
- [x] CRD YAML generated and committed under `cmd/agc/config/crd/`.
- [x] `RunnerGroup` create/scale/delete lifecycle produces no goroutine leaks in integration tests.
- [ ] `status.activeSessions` is exactly 1 at rest per RunnerGroup in the kind cluster.
- [x] Token Manager proactive refresh fires before expiry (verified via unit test with fake clock).
- [x] `RateLimited` condition surfaces after 10 minutes of sustained 429s (verified via unit test).
- [x] `RunnerVersionTooOld` condition surfaces on 400 from `POST /sessions` without a retry loop.
- [x] Agent Secrets created on RunnerGroup creation, deleted on deletion, with no leaks on error paths.
- [x] Investigation A finding documented (runner registration API).
- [x] Investigation B live test implemented; finding documented in §11.B.
- [x] No `TODO(milestone-3+)` items left untracked — each deferred item has a note in the Milestone 3 plan or a filed comment.

---

## 9. Risks and Mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Runner registration API is undocumented and changes between runner versions | Medium | High | Investigation A (§4.A) must be completed in day 5–6. If the API is opaque, use mitmproxy against `config.sh` to capture the exact wire format. Pin the runner version used for registration to match `WorkerImage` in the RunnerGroup spec. |
| Agent registrations expire silently (TTL shorter than expected) | Unknown | Medium | Monitor for 401/403 on `CreateSession` in the kind cluster run. If agents expire, add proactive re-registration to the Pool (same pattern as Token Manager). |
| Goroutine leak in the Multiplexer under rapid create/delete cycling | Low | Medium | `goleak.VerifyNone` in every reconciler test. Run a stress test (create + delete a RunnerGroup 100 times) in the integration suite before marking the milestone done. |
| `controller-runtime` fake client does not surface all Kubernetes error cases | Low | Low | Where fake client gaps appear, use envtest instead for that specific test scenario. |
| Token Manager write-lock contention under high goroutine counts | Low | Low | The lock is held only for the pointer swap, not the HTTP call. Benchmark with 100 concurrent `Token()` callers if contention appears in profiling. |
| Session-expired and unauthorized errors conflated, causing incorrect non-retriable handling | Medium | Medium | Explicitly match the error message text or HTTP status in the listener goroutine. Add a test for each branch with a distinct stub response. |

---

## 10. Deferred to Later Milestones

- **Worker pod creation and Secret staging** — Milestone 3. The `JobHandlerFunc` stub in M2 records the acquisition but creates no Kubernetes resources.
- **`maxWorkers` and `priorityTiers` enforcement** — Milestone 3. The ceiling logic belongs in the pod provisioner.
- **Named Pipe handoff** — Milestone 3.
- **`ActionsGateway` CRD and GMC** — Milestone 4.
- **Egress proxy pool** — Milestone 4. AGC in M2 makes direct outbound calls (suitable for testing in a kind cluster with direct internet access).
- **`HTTP_PROXY`/`HTTPS_PROXY` env var injection** — Milestone 4 (GMC injects them into the AGC Deployment).
- **Admission webhook for reserved namespaces** — Milestone 4 (part of GMC).
- **`spec.proxy.managedNetworkPolicy` IP range reconciler** — Milestone 4.
- **Multi-tenant isolation and load testing** — Milestone 5.

---

## 11. Investigation Findings

### 11.A — Runner Agent Registration API

**Source:** `github.com/actions/runner` open-source repository,
`src/Runner.Common/RunnerDotcomServer.cs` and `src/Sdk/DTWebApi/WebApi/Runner.cs`.

**Registration flow (confirmed from source):**

1. Obtain a short-lived registration token:
   ```
   POST https://api.github.com/orgs/{org}/actions/runners/registration-token
   Authorization: Bearer {installationAccessToken}
   → {"token": "...", "expires_at": "..."}
   ```

2. Register the runner agent:
   ```
   POST https://api.github.com/actions/runners/register
   Authorization: RemoteAuth {registrationToken}
   Content-Type: application/json
   {
     "url": "{orgURL}",
     "group_id": {groupID},
     "name": "{name}",
     "version": "{version}",
     "updates_disabled": false,
     "ephemeral": false,
     "labels": [],
     "public_key": "{base64(DER(SubjectPublicKeyInfo))}"
   }
   → {
     "id": 12345,
     "authorization": {
       "authorization_url": "...",
       "server_url": "...",
       "client_id": "..."
     }
   }
   ```
   The `public_key` field is `base64.StdEncoding.EncodeToString(x509.MarshalPKIXPublicKey(...))`.
   Authentication uses `Authorization: RemoteAuth {token}` (not `Bearer`).

3. Deregister:
   ```
   DELETE https://api.github.com/orgs/{org}/actions/runners/{id}
   Authorization: Bearer {installationAccessToken}
   ```

**Implementation:** `GithubRegistrar` in `cmd/agc/internal/agentpool/github_registrar.go` implements
this flow. `StubRegistrar` remains wired in `main.go` until validated against live GitHub credentials.

**TODO(investigation-a):** Confirm exact request/response schema against a live `config.sh --debug`
capture before replacing `StubRegistrar` in production `main.go`. The schema above is sourced from
the open-source runner code and may differ for enterprise GitHub instances or future runner versions.

---

### 11.B — Live Egress IP Variance (deferred completion from Milestone 1)

**Finding:** The GitHub Actions v2 broker is stateless with respect to the client IP address. Sessions
are identified by `sessionId` (a GUID), not by IP or TCP connection. Every API call
(CreateSession, GetMessage, AcquireJob, RenewJob, DeleteSession) carries a Bearer token and the
session ID in the URL — there is no server-side session affinity by IP.

**Evidence:**
- `TestCONNECTProxy_TunnelsHTTPS` (unit test) confirms that the CONNECT-proxy infrastructure
  correctly tunnels TLS without termination; two proxy instances alternate across four requests
  without errors.
- `TestEgressIPVariance_Live` (integration test, skipped unless `GITHUB_*` env vars are set)
  runs the full broker protocol sequence (CreateSession → GetMessage → DeleteSession) with each
  outbound connection routed through an alternating pair of CONNECT proxies. Run manually with
  real GitHub credentials to confirm on live endpoints:
  ```
  GITHUB_APP_ID=... GITHUB_APP_PRIVATE_KEY=... GITHUB_APP_INSTALLATION_ID=... \
  GITHUB_BROKER_URL=... GITHUB_RUNNER_VERSION=... GITHUB_AGENT_ID=... \
  go test -v -run TestEgressIPVariance_Live ./broker/
  ```
- See also `docs/plan/milestone-1.md §8.B` for the original M1 finding (stateless broker design).

**Conclusion:** No proxy affinity is required. The Milestone 4 egress proxy pool can use round-robin
or any stateless load-balancing strategy across proxy pods without risk of session disruption.
