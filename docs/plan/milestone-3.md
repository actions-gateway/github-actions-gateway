# Milestone 3 Implementation Plan — Worker Pod & Pipe Handoff

← [Milestone 2](milestone-2.md) | [Back to implementation phases](../design/06-implementation-phases.md)

---

## Status at a glance

Last refreshed 2026-05-30. The provisioner, decryption, ceiling
enforcement, eviction retry, GC, and RBAC are all in code. Investigation A
(Named Pipe protocol) is complete. Q5h (worker proxy-CA trust)
shipped 2026-05-30. **Q6 (Tier-C `E2E_GitHub_RealDispatch`)
re-ran successfully on 2026-05-30** against real GitHub
(`actions-gateway/gateway-test`, workflow `test-job.yml`, run
[26685844172](https://github.com/actions-gateway/gateway-test/actions/runs/26685844172)):
both specs (`E2E_GitHub_ActionsGatewayReachesReady` and
`E2E_GitHub_WorkflowCompletesGreen`) pass, GitHub-side run concludes
`success` — the first green checkmark from this system. The re-run
also surfaced an unrelated GMC readiness-probe bug (`/readyz` returned
OK before the webhook server bound, racing every rollout); fixed
separately in `cmd/gmc/cmd/main.go` by gating readyz on
`mgr.GetWebhookServer().StartedChecker()`.

| Success criterion | Status | Notes |
|---|---|---|
| Real workflow job completes with green checkmark | ✅ Done | 2026-05-30 — Tier-C `E2E_GitHub_RealDispatch` passes; run [26685844172](https://github.com/actions-gateway/gateway-test/actions/runs/26685844172) on `actions-gateway/gateway-test` concludes `success`. Required both 5h (worker proxy-CA trust) and the GMC `/readyz` webhook-gating fix. |
| `go test -race ./...` passes across all modules | ✅ Done | Per-module test commands pass |
| Worker container exits 0 on success, non-zero on failure | ✅ Done | Exercised by the 2026-05-30 Tier-C re-run: worker pod exited 0 and the GitHub-side run concluded `success`. |
| Pod and Secret GC'd within 30s of terminal state | ✅ Done in code | `deleteSecret` + pod cleanup in [provisioner.go:166-206](../../cmd/agc/internal/provisioner/provisioner.go) |
| `maxWorkers` ceiling enforced | ✅ Done | `activePodCount` check at [provisioner.go:335](../../cmd/agc/internal/provisioner/provisioner.go) |
| `priorityTiers` ceiling + PriorityClass assignment | ✅ Done | Tier walk in provisioner pod builder |
| Eviction auto-retry up to `maxEvictionRetries` | ✅ Done | `handleEviction` + `rerunFailedJobs` at [provisioner.go:210-276](../../cmd/agc/internal/provisioner/provisioner.go) |
| Retry budget exhausted → no rerun, exhausted metric | ✅ Done | Counter check at line 218; `actions_gateway_eviction_retries_exhausted_total` |
| Message body decryption (AES-CBC w/ session key) | ✅ Done | `aesKey` plumbed through `handleJob` ([goroutine.go:105,177,224](../../cmd/agc/internal/listener/goroutine.go)) |
| Investigation A — Named Pipe protocol documented (§11.A) | ✅ Done | Anonymous pipes + LE int32 type + LE int32 byte-len + UTF-16LE body confirmed from runner source; implementation updated in [worker/main.go](../../cmd/worker/main.go); findings in §11.A |
| Investigation B — Worker image source documented (§11.B) | ⚠️ Partial | Dockerfile pins `ghcr.io/actions/actions-runner:2.327.1` (ARC-aligned base, UID 1001, Runner.Worker at `/home/runner/bin/`); §11.B still says "TBD" |
| RBAC markers regenerated and committed | ✅ Done | Pod + Secret markers in [controller/doc.go](../../cmd/agc/internal/controller/doc.go) |

### Critical path

All M3 criteria are met. Q6 (`E2E_GitHub_RealDispatch`)
passed end-to-end on 2026-05-30 with a green checkmark on the
GitHub-side run, unblocking items 7 (egress-proxy live curl) and
11 (Ed25519 live probe) which both share the live-kind-cluster
dependency.

---

## Overview

**Goal:** Replace the M2 stub job handler with a real pod provisioner, build the worker container image and entrypoint wrapper, and wire the full `AcquireJob` → pod-create → pipe handoff → garbage-collection sequence so that a real GitHub workflow job executes end-to-end inside a Kubernetes pod.

**Duration:** Days 11–16

**Foundation:** All packages from Milestones 1–2 (`broker`, `githubapp`, `cmd/agc/`) are consumed unchanged except for targeted additions. No new module is introduced unless the worker entrypoint binary requires its own Go module (see §2).

**Key constraint from the design doc:** validate the Named Pipe handoff with the static `testdata/job_payload.json` fixture *before* wiring it into the live pod creation path. Named Pipes are the underdocumented interface between the entrypoint wrapper and `Runner.Worker`; debugging them without a live GitHub trigger in the loop is significantly easier.

**Definition of Done:**

- A real workflow job dispatched from GitHub appears in the Actions UI with correct step output, timing, and a green checkmark.
- The worker container exits with code `0` on success and non-zero on workflow failure.
- The pod and its job-payload Secret are garbage-collected by the AGC after the pod reaches a terminal state.
- `maxWorkers` ceiling is enforced when set on the `RunnerGroup`.
- `priorityTiers` ceiling and PriorityClass assignment work when configured.
- Eviction auto-retry fires up to `maxEvictionRetries` times (default 2), then stops.
- All unit tests pass under `go test -race ./...` from the repo root.
- Code is committed to the repository.

---

## 1. Changes to Existing Packages

### 1.1 Message-body decryption (`cmd/agc/internal/listener/goroutine.go`)

`goroutine.go:274` contains:

```go
// TODO(milestone-3): decrypt body using session key before parsing.
```

**What is needed:** `CreateSession` returns an `encryptionKey.value` field — a base64-encoded RSA-encrypted AES-256 session key. The message body returned by `GetMessage` is AES-256-CBC encrypted with that key. `broker.DecryptSessionKey` and `broker.DecryptMessageBody` already implement both steps (`broker/crypto.go`).

**Changes required:**

1. **`broker/client.go` — `CreateSessionResponse`:** Confirm that `CreateSessionResponse` already exposes `EncryptionKey.Value` (base64 RSA-encrypted key). If not, add the field.

2. **`cmd/agc/internal/listener/goroutine.go`:**
   - Change `createSession` to return both the `sessionID` string and the decoded AES session key (`[]byte`). Call `broker.DecryptSessionKey` inside `createSession` using the agent's RSA private key.
   - Pass the session key into `handleJob` and use `broker.DecryptMessageBody(msg.Body, sessionKey)` before unmarshalling `RunnerJobRequestBody`.
   - On session recreation (expired-session path), re-derive the session key from the new `CreateSession` response.

3. **`cmd/agc/internal/listener/goroutine_test.go`:** Add `TestListener_DecryptsMessageBody` — stub `CreateSession` returning a synthetic encrypted key; stub `GetMessage` returning a body encrypted with the matching AES key (use the `testdata/crypto_fixture.json` key material); assert the resulting `RunnerJobRequestBody` fields are correctly parsed.

### 1.2 `TODO(milestone-3): enforce maxWorkers ceiling` (`cmd/agc/internal/controller/runnergroup_controller.go:161`)

This TODO is resolved by the pod provisioner in §3. The reconciler calls the provisioner via the `JobHandlerFunc`; the provisioner itself performs the pod-count check before creating a pod. No separate change is needed in the reconciler beyond wiring the new provisioner (replacing `stubJobHandler`).

### 1.3 RBAC markers (`cmd/agc/internal/controller/runnergroup_controller.go`)

Add kubebuilder RBAC markers for pod and pod/status access:

```go
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods/status,verbs=get
```

Regenerate RBAC manifests with `make manifests` and commit them.

### 1.4 New metrics (`cmd/agc/internal/listener/metrics.go`)

Add the M3-specific metrics defined in [§2.5 of the architecture doc](../design/02-architecture.md#25-observability):

| Metric | Type | Labels |
|---|---|---|
| `actions_gateway_job_duration_seconds` | Histogram | `namespace`, `runner_group` |
| `actions_gateway_pod_creation_latency_seconds` | Histogram | `namespace` |
| `actions_gateway_eviction_retries_total` | Counter | `namespace`, `runner_group` |
| `actions_gateway_eviction_retries_exhausted_total` | Counter | `namespace`, `runner_group` |

---

## 2. Worker Entrypoint Wrapper (`cmd/worker/`)

The entrypoint wrapper is a small Go binary that acts as `ENTRYPOINT` in the worker container image. Its responsibilities:

1. Read the job payload JSON from a mounted Kubernetes Secret (path configurable via `RUNNER_PAYLOAD_PATH` env var; default `/run/secrets/runner/payload`).
2. Write the payload into Named Pipes consumed by `Runner.Worker`.
3. Exec or launch `Runner.Worker` and wait for it to exit.
4. Forward `Runner.Worker`'s exit code.

### 2.1 Module placement

The wrapper is simple enough to live in the root module (`cmd/worker/main.go`) rather than a new Go module, keeping the workspace flat. Add it alongside `cmd/probe/`:

```
github-actions-gateway/
└── cmd/
    ├── probe/       # Milestone 1
    ├── agc/         # Milestone 2
    └── worker/      # Milestone 3 — new
        └── main.go
```

No new `go.mod` is needed; the root `go.mod` already hosts the `broker` and `githubapp` packages and is the right home for a small utility binary.

### 2.2 Named Pipe protocol (Investigation Task A — see §5.A)

`Runner.Worker` reads its job payload through Named Pipes (Linux FIFOs). The exact pipe paths and payload format are not confirmed from the existing codebase. **This is the underdocumented part of the milestone.** Investigation Task §5.A defines how to determine the pipe names and sequencing before writing this binary.

**Provisional implementation** (to be updated after §5.A):

```go
// main.go — skeletal structure, pipe names TBD from Investigation A
func main() {
    payloadPath := envOrDefault("RUNNER_PAYLOAD_PATH", "/run/secrets/runner/payload")
    payload, err := os.ReadFile(payloadPath)
    // ... error handling

    // Write to Named Pipes (names from Investigation A).
    if err := writeToNamedPipes(payload); err != nil {
        // ... fatal
    }

    // Exec Runner.Worker; forward exit code.
    cmd := exec.Command(envOrDefault("RUNNER_WORKER_PATH", "/runner/externals/Runner.Worker"))
    cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
    if err := cmd.Run(); err != nil { ... }
}
```

### 2.3 Static fixture validation

Before integrating the wrapper into live pod creation, validate it end-to-end using the `testdata/job_payload.json` fixture:

```
# Build the wrapper binary locally.
go build -o /tmp/worker-wrapper ./cmd/worker/

# Write the fixture to the expected path.
mkdir -p /tmp/runner-secret
cp testdata/job_payload.json /tmp/runner-secret/payload
RUNNER_PAYLOAD_PATH=/tmp/runner-secret/payload /tmp/worker-wrapper
```

The wrapper should create the Named Pipes, write the payload, and then attempt to exec `Runner.Worker`. Use a stub `Runner.Worker` script (e.g. a shell script that reads from the pipes and prints the payload) for the fixture validation step so the full pipe protocol can be confirmed without a licensed GitHub Actions runner binary.

### 2.4 Dockerfile

```dockerfile
FROM golang:1.24 AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/worker-wrapper ./cmd/worker/

# Runner.Worker is harvested from the official actions/runner release.
# Pin to a specific version digest for reproducibility.
FROM ghcr.io/actions/actions-runner:2.327.1 AS runner-source

FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /bin/worker-wrapper /usr/local/bin/worker-wrapper
COPY --from=runner-source /opt/runner/externals /runner/externals
ENTRYPOINT ["/usr/local/bin/worker-wrapper"]
```

The `DefaultWorkerImage` constant in `cmd/agc/main.go` is updated to reference the new image once it is published.

**Note:** The exact image from which `Runner.Worker` can be harvested and the correct path (`/opt/runner/externals` vs. another directory) must be confirmed as part of Investigation §5.A.

---

## 3. Pod Provisioner (`cmd/agc/internal/provisioner/`)

The pod provisioner replaces `stubJobHandler` in the reconciler. It is a new package implementing `listener.JobHandlerFunc`.

### 3.1 Directory layout

```
cmd/agc/internal/
└── provisioner/
    ├── provisioner.go       # Provisioner struct + Provision method
    ├── provisioner_test.go
    ├── pod_builder.go       # Builds the worker Pod spec from RunnerGroup PodTemplate
    └── pod_builder_test.go
```

### 3.2 Provisioner exported surface

```go
// Config holds the dependencies injected into the provisioner.
type Config struct {
    Client       client.Client     // Kubernetes client
    Namespace    string
    GroupName    string
    WorkerImage  string            // fallback if PodTemplate has no "runner" container
    PodTemplate  corev1.PodTemplateSpec
    MaxWorkers   *int32            // nil = unlimited (only namespace quota applies)
    PriorityTiers []v1alpha1.PriorityTier
    MaxEvictionRetries int32       // default 2
    Metrics      *listener.Metrics
    Log          *slog.Logger
}

// Provisioner creates, monitors, and garbage-collects ephemeral worker pods.
type Provisioner struct{ cfg Config }

// New returns a Provisioner ready to handle job acquisitions.
func New(cfg Config) *Provisioner

// Provision implements listener.JobHandlerFunc. It:
//  1. Decodes the run_id from payload for eviction retry.
//  2. Enforces the pod-count ceiling (maxWorkers / priorityTiers).
//  3. Creates the job-payload Secret.
//  4. Builds and creates the worker Pod.
//  5. Watches the pod to completion, then garbage-collects the Secret and pod.
// It blocks until the pod reaches a terminal state or ctx is cancelled.
func (p *Provisioner) Provision(ctx context.Context, runServiceURL, planID string, payload []byte) error
```

### 3.3 Pod-count ceiling enforcement

Before creating a pod, the provisioner lists pods in the namespace with label `actions-gateway/runner-group: <groupName>` and counts those in phases `Pending` or `Running`:

```go
var podList corev1.PodList
if err := p.cfg.Client.List(ctx, &podList,
    client.InNamespace(p.cfg.Namespace),
    client.MatchingLabels{"actions-gateway/runner-group": p.cfg.GroupName},
); err != nil { ... }

activeCount := int32(0)
for _, pod := range podList.Items {
    if pod.Status.Phase == corev1.PodPending || pod.Status.Phase == corev1.PodRunning {
        activeCount++
    }
}
```

**`maxWorkers` only (no `priorityTiers`):** If `activeCount >= maxWorkers`, log a warning and return `nil` (the job was acquired but not dispatched to a pod — it will timeout and be redelivered). The RenewJob loop continues keeping the lock until the provisioner returns. A tight hold-and-wait loop here would be wrong; instead, return early and let GitHub redeliver when capacity frees up.

> **Note:** The design acknowledges a benign race at the ceiling boundary (§2.2). The namespace ResourceQuota is the hard enforcement layer. The pod-count check here is a best-effort soft ceiling.

**`priorityTiers` set:** Walk the tier list in ascending threshold order. If `activeCount` is below `tier.Threshold`, set the pod's `priorityClassName` to `tier.PriorityClassName`. If `activeCount >= lastTier.Threshold`, hold (return `nil`) as above.

### 3.4 Pod builder (`pod_builder.go`)

```go
// BuildPod constructs the worker Pod spec for a job acquisition.
// It starts from the tenant's PodTemplateSpec, injects the runner container
// if absent, and overwrites all reserved fields.
func BuildPod(
    namespace, groupName, podName, secretName string,
    workerImage string,
    template corev1.PodTemplateSpec,
    priorityClassName string, // "" if no tier applies
    proxy ProxyEnv,           // HTTP_PROXY, HTTPS_PROXY, NO_PROXY values
) *corev1.Pod
```

**Reserved-field overrides applied unconditionally after merging the tenant template:**

| Field | Value set by AGC |
|---|---|
| `spec.serviceAccountName` | `"actions-gateway-worker"` (worker SA, created by GMC in M4; for M3 use the default SA) |
| `spec.automountServiceAccountToken` | `false` |
| `spec.hostPID` | `false` |
| `spec.hostNetwork` | `false` |
| `spec.hostIPC` | `false` |
| `containers[name=runner].env[HTTP_PROXY]` | proxy Service address (injected from AGC env in M4; empty in M3) |
| `containers[name=runner].env[HTTPS_PROXY]` | same |
| `containers[name=runner].env[NO_PROXY]` | cluster-local exclusions |

**Runner container injection:** If no container named `"runner"` exists in the template, prepend one:

```go
corev1.Container{
    Name:  "runner",
    Image: workerImage,
    VolumeMounts: []corev1.VolumeMount{{
        Name:      "runner-payload",
        MountPath: "/run/secrets/runner",
        ReadOnly:  true,
    }},
}
```

**Volume for the payload Secret:**

```go
corev1.Volume{
    Name: "runner-payload",
    VolumeSource: corev1.VolumeSource{
        Secret: &corev1.SecretVolumeSource{SecretName: secretName},
    },
}
```

**Pod labels** (required for pod-count queries and garbage collection):

```go
labels: map[string]string{
    "app.kubernetes.io/managed-by":    "actions-gateway-agc",
    "actions-gateway/runner-group":    groupName,
    "actions-gateway/job-secret-name": secretName,
}
```

### 3.5 Job-payload Secret

Before creating the pod, create a Secret containing the raw `AcquireJob` response bytes:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: job-{shortJobID}   # 8-char hex prefix of SHA-256(payload)
  namespace: team-a
  labels:
    app.kubernetes.io/managed-by: actions-gateway-agc
    actions-gateway/runner-group:  my-runnergroup
type: Opaque
data:
  payload: <base64(rawAcquireJobBytes)>
```

Secret creation must happen before pod creation. If Secret creation fails, return the error without creating a pod. If pod creation fails after Secret creation, delete the Secret before returning the error (best-effort cleanup).

### 3.6 Pod watch and garbage collection

After pod creation, the provisioner enters a watch loop using `controller-runtime`'s typed watch:

```go
watcher, err := p.cfg.Client.Watch(ctx, &corev1.PodList{},
    client.InNamespace(p.cfg.Namespace),
    client.MatchingFields{"metadata.name": pod.Name},
)
```

On each event, check `pod.Status.Phase`:

- `Succeeded` or `Failed` (non-eviction): delete the Secret and the pod. Emit `actions_gateway_job_duration_seconds`. Return `nil`.
- `Failed` with `pod.Status.Reason == "Evicted"`: trigger eviction retry logic (§3.7). Return `nil`.

On context cancellation: stop watching. The RenewJob loop will have already been stopped by the goroutine defer. The pod and Secret are **not** deleted on context cancellation — they may still be running. The AGC will reconcile and garbage-collect orphaned pods on restart via a pod ownership label scan.

### 3.7 Eviction auto-retry

When a pod enters `Failed/Evicted`:

1. Stop the RenewJob loop immediately (call `stopRenewLoop()`) so GitHub can cancel the job promptly.
2. Check the retry counter for this `run_id`. If `retries >= maxEvictionRetries`, emit `actions_gateway_eviction_retries_exhausted_total` and return without re-queuing.
3. Otherwise increment the retry counter, emit `actions_gateway_eviction_retries_total`, wait `evictionRetryDelay` (default 5s), then call:
   ```
   POST /repos/{owner}/{repo}/actions/runs/{run_id}/rerun-failed-jobs
   Authorization: Bearer {installation_token}
   ```
   using the AGC's installation access token from the Token Manager.

**`run_id` extraction:** The `AcquireJob` response body (`job_payload.json`) contains `contextData.github.run_id` (a string). The provisioner extracts it on entry by doing a minimal JSON unmarshal of just that field.

**In-memory retry counter:** A `sync.Map` in the `Provisioner` struct keyed by `run_id` (`string → int32`). This state is lost on AGC restart; GitHub's own retry limits prevent unbounded re-queuing in the crash-loop case.

---

## 4. Job Lock Renewer Enhancements (`cmd/agc/internal/listener/goroutine.go`)

The existing `StartRenewLoop` ticks every 60s and calls `RenewJob`. For M3, two changes:

### 4.1 Stop function returned to provisioner

`StartRenewLoop` already returns a `stop()` function. The provisioner calls it when the pod reaches a terminal state (§3.6) or is evicted (§3.7). No structural change needed — this is already wired correctly in `handleJob`'s `defer stop()`.

### 4.2 Renewer receives pod terminal signal

The current design uses `defer stop()` on the renewer inside `handleJob`. The provisioner blocks until the pod exits (§3.6), at which point `handleJob` returns and `defer stop()` fires. This is correct — no additional change to the renewer is needed in M3. The eviction path calls `stopRenewLoop()` *before* calling the rerun API (§3.7), which requires the provisioner to hold a reference to the stop function and call it early. Pass `stopRenewLoop func()` into `Provision` so the provisioner can call it on eviction detection.

Revised `Provision` signature:

```go
func (p *Provisioner) Provision(ctx context.Context, runServiceURL, planID string, payload []byte) error
```

The provisioner is called from `handleJob`, which calls `defer stop()` after `JobHandler` returns. For early stop on eviction, restructure `handleJob` to pass the `stop` function into the provisioner:

```go
// In handleJob, after starting the renew loop:
if cfg.JobHandler != nil {
    return cfg.JobHandler(ctx, runServiceURL, planID, payload)
}
```

Add a new `JobHandlerFuncWithStop` type if needed, or have the provisioner wrap the stop function internally. Simplest approach: add an `OnEviction func()` callback to `provisioner.Config` that is set to `stop` by the reconciler when constructing the provisioner per-job. This keeps the `listener.JobHandlerFunc` signature unchanged.

---

## 5. Investigation Tasks

### 5.A — Named Pipe Protocol (Runner.Worker handoff)

**Context:** `Runner.Worker` is the .NET binary from `actions/runner`. Its source is open and lives in the repo at `src/Runner.Worker/`. The entrypoint wrapper must feed it the job payload via Named Pipes before `Runner.Worker` can begin executing steps. The exact pipe names, write ordering, and payload format are the unknown.

**How to investigate:**

1. Clone `actions/runner` and search for named-pipe creation in the Worker startup code:
   ```
   grep -r "NamedPipe\|mkfifo\|pipe" src/Runner.Worker/ --include="*.cs"
   ```
2. Look specifically at `src/Runner.Worker/Worker.cs` (entry point) and any `PipeServer` or `MessageReader` class for the pipe initialization sequence.
3. Confirm:
   - The pipe names (likely `<runId>.reader.pipe` / `<runId>.writer.pipe` or similar).
   - Whether pipes are passed as command-line arguments to `Runner.Worker` or are at fixed paths.
   - Whether `Runner.Worker` creates the pipes itself and the wrapper writes to them, or vice versa.
   - The exact bytes written to each pipe (raw job payload JSON? or a framed protocol?).
4. If the source is not conclusive, run `Runner.Worker` locally with `strace -e trace=open,openat,mkfifo,pipe2` to observe pipe creation.

**Expected outcomes:**

- Document the Named Pipe protocol (pipe names, direction, payload format) as a code comment block at the top of `cmd/worker/main.go`.
- Implement `writeToNamedPipes` in the entrypoint wrapper based on confirmed findings.
- Validate the implementation against `testdata/job_payload.json` using a stub `Runner.Worker` script before wiring into live pod creation.

**Document findings:** Add §8.A to the Investigation Findings section at the bottom of this file before closing the milestone.

### 5.B — Worker Image Source

**Context:** The Dockerfile in §2.4 references `ghcr.io/actions/actions-runner:2.327.1` (ARC-aligned base) as a source for `Runner.Worker`. Confirmed via local `docker pull` + `docker run`:

1. `Runner.Worker` is present in the image (no tarball extraction needed).
2. Path: `/home/runner/bin/Runner.Worker`. This directory is **not** on the default `$PATH`, so the worker Dockerfile sets `ENV PATH=/home/runner/bin:$PATH` to keep [cmd/worker/main.go:91](../../cmd/worker/main.go:91)'s `exec.LookPath("Runner.Worker")` resolving correctly.
3. .NET runtime + shared libraries ship inside the image (no host dependency on `ubuntu:24.04`).
4. Image runs as `USER runner` (UID 1001) — tenants need `runAsUser: 1001` in the RunnerGroup `podTemplate` for PSA `restricted` admission. Already documented in [security.md D-2](security.md).

**Document findings:** Add §8.B to the Investigation Findings section.

---

## 6. Reconciler Wiring

The reconciler's `getOrCreateMultiplexer` currently uses `stubJobHandler`. Replace it with a real provisioner instance:

```go
// In getOrCreateMultiplexer, replace:
JobHandler: stubJobHandler,

// With:
prov := provisioner.New(provisioner.Config{
    Client:             r.Client,
    Namespace:          rg.Namespace,
    GroupName:          rg.Name,
    WorkerImage:        rg.Spec.WorkerImage, // falls back to DefaultWorkerImage if ""
    PodTemplate:        rg.Spec.PodTemplate,
    MaxWorkers:         rg.Spec.MaxWorkers,
    PriorityTiers:      rg.Spec.PriorityTiers,
    MaxEvictionRetries: 2,
    Metrics:            r.Metrics,
    Log:                r.Log,
})
JobHandler: prov.Provision,
```

Because the provisioner config is derived from the RunnerGroup spec, it must be rebuilt whenever `maxWorkers`, `priorityTiers`, `podTemplate`, or `workerImage` change. The simplest approach: store the provisioner inside the `Multiplexer` and rebuild it (alongside `SetMaxListeners`) on each reconcile, since `Provision` is called per-job and the config fields are read at call time.

---

## 7. Test Plan

### 7.1 Unit Tests (`go test -race ./...`)

**Message body decryption (`listener/goroutine_test.go`)**

| Test | What it verifies |
|---|---|
| `TestListener_DecryptsMessageBody` | Stub CreateSession returns synthetic encrypted session key; GetMessage returns AES-CBC-encrypted body; assert RunnerJobRequestBody fields parsed correctly. |
| `TestListener_SessionKeyPassedToHandleJob` | Assert the session key from CreateSession is used to decrypt the message body, not a zero/static key. |

**Pod builder (`provisioner/pod_builder_test.go`)**

| Test | What it verifies |
|---|---|
| `TestBuildPod_InjectsRunnerContainer` | Template with no "runner" container → assert runner container prepended with correct image and volume mount. |
| `TestBuildPod_OverwritesReservedFields` | Template sets serviceAccountName, automountServiceAccountToken, hostPID → assert all overwritten to AGC values regardless of input. |
| `TestBuildPod_PriorityClassAssigned` | activeCount=3, priorityTiers=[{threshold:5,class:A},{threshold:20,class:B}] → assert priorityClassName="A". |
| `TestBuildPod_PriorityClassNextTier` | activeCount=7 → assert priorityClassName="B". |
| `TestBuildPod_Labels` | Assert all required labels present on the built pod. |
| `TestBuildPod_PayloadVolumeMount` | Assert Volume and VolumeMount for runner-payload Secret are present. |

**Provisioner (`provisioner/provisioner_test.go`)** — use `controller-runtime/pkg/client/fake` and a real `httptest` server for the rerun API:

| Test | What it verifies |
|---|---|
| `TestProvision_CreatesSecretThenPod` | Assert Secret created before pod; pod has correct labels. |
| `TestProvision_SecretFailure_NoPod` | Fake client returns error on Secret create → assert pod never created. |
| `TestProvision_PodFailure_SecretDeleted` | Secret created; pod create fails → assert Secret deleted. |
| `TestProvision_MaxWorkersHeld` | activeCount >= maxWorkers → assert Provision returns nil without creating pod. |
| `TestProvision_PodSucceeded_GC` | Fake client delivers pod phase=Succeeded watch event → assert Secret and pod deleted. |
| `TestProvision_PodEvicted_Retry` | Pod phase=Failed, reason=Evicted → assert rerun API called; retry counter incremented. |
| `TestProvision_PodEvicted_RetriesExhausted` | retries already at maxEvictionRetries → assert rerun API NOT called; exhausted metric incremented. |
| `TestProvision_PodFailed_NonEviction` | Pod phase=Failed, reason="" → assert GC runs; rerun API NOT called. |
| `TestProvision_run_id_Extracted` | Assert run_id extracted from payload and used in rerun API URL. |

**Worker entrypoint (`cmd/worker/`)**

| Test | What it verifies |
|---|---|
| `TestWrapper_ReadPayloadFromMount` | Write fixture to temp path; assert wrapper reads it without error. |
| `TestWrapper_WritesToNamedPipes` | Assert wrapper creates and writes to expected pipe paths (names from Investigation A). |
| `TestWrapper_MissingPayload` | Payload file absent → assert wrapper exits non-zero. |

### 7.2 Integration Tests (envtest)

| Scenario | Pass Criterion |
|---|---|
| End-to-end job acquisition | Configure stub broker to deliver one job; assert Secret created, pod created with correct labels, provisioner blocks until pod completes; Secret and pod deleted after Succeeded event. |
| maxWorkers ceiling | Two concurrent job acquisitions with maxWorkers=1; assert second acquisition returns nil (held), no second pod created. |
| Eviction retry | Pod events: Running → Failed/Evicted; assert rerun API called once; eviction counter incremented. |
| Retry budget exhausted | Force two evictions then a third → assert third does not call rerun API; exhausted metric incremented. |

### 7.3 Manual End-to-End Verification

After integration tests pass, deploy the updated AGC to a `kind` cluster with a real GitHub App and queue a workflow job:

1. Watch AGC logs: `job message received` → `AcquireJob` → `Secret created` → `Pod created`.
2. Check GitHub Actions UI: job appears running, step output streams correctly.
3. Pod exits with code 0 → job shows green checkmark in GitHub.
4. `kubectl get pod,secret -l actions-gateway/runner-group=<name>` → both gone within 30s.
5. Eviction test: set a very low memory limit on the pod template; confirm eviction triggers a rerun in the GitHub Actions UI.

---

## 8. Success Criteria Checklist

- [ ] Real workflow job completes with green checkmark in GitHub Actions UI.
- [ ] `go test -race ./...` passes with zero failures across all modules.
- [ ] Worker container exits 0 on success, non-zero on workflow failure.
- [ ] Pod and Secret GC'd within 30s of pod reaching terminal state.
- [ ] `maxWorkers` ceiling: no pod created when count at ceiling; next job runs after one completes.
- [ ] `priorityTiers` ceiling: pods assigned correct PriorityClass per active-count tier.
- [ ] Eviction auto-retry: failed job re-queued in GitHub Actions UI after pod eviction.
- [ ] Retry budget: third eviction of same job does NOT trigger rerun; exhausted metric emitted.
- [ ] Message body decryption: listener decrypts AES-CBC body before passing to provisioner.
- [ ] Investigation A (Named Pipe protocol) documented in §9.A.
- [ ] Investigation B (Worker image source) documented in §9.B.
- [ ] New RBAC markers regenerated and CRD/RBAC manifests committed.

---

## 9. Risks and Mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Named Pipe protocol undocumented or changes across runner versions | Medium | High | Investigation §5.A must complete before writing the entrypoint wrapper. Use strace on a local Runner.Worker binary if source inspection is insufficient. Pin runner version in DefaultWorkerImage. |
| `Runner.Worker` requires .NET runtime not present in ubuntu:24.04 | Medium | Medium | Investigation §5.B. If self-contained runtime is not bundled in the runner image, add a multi-stage Dockerfile step to install the .NET runtime. |
| Pod-count ceiling race at boundary | Low | Low | Design-documented benign race. Namespace ResourceQuota is the hard cap. Document the race in provisioner.go. |
| Eviction retry creates duplicate runs on GitHub | Low | High | Confirm `POST /repos/{owner}/{repo}/actions/runs/{run_id}/rerun-failed-jobs` is idempotent for already-running runs. If not, gate the API call on checking run status first. |
| Watch loop in Provision leaks if context cancelled mid-watch | Low | Medium | Ensure `defer watcher.Stop()` is called; test with context cancellation in `TestProvision_ContextCancel`. |
| `job_payload.json` fields differ from live AcquireJob response shape | Low | Medium | The fixture is redacted but structurally accurate. Smoke-test by running the probe against a live GitHub job and comparing top-level field names. |

---

## 10. Deferred to Later Milestones

- **Worker ServiceAccount** (a dedicated minimally-scoped SA) — created by the GMC in Milestone 4. In M3, the pod uses the namespace default SA with `automountServiceAccountToken: false`.
- **`HTTP_PROXY`/`HTTPS_PROXY` injection into worker pods** — Milestone 4 (GMC injects proxy config into the AGC Deployment env; the AGC reads and forwards these to worker pod templates).
- **`NetworkPolicy` restricting worker pod egress to the proxy pool** — Milestone 4.
- **Admission webhook for reserved PodTemplate fields** — Milestone 4 (CRD CEL rules can flag obvious misuse; the webhook gives a better UX error).
- **Hardened pod spec** (read-only root filesystem, dropped capabilities, non-root user) — Milestone 5. M3 focuses on correctness; security hardening is Milestone 5.
- **`gVisor`/Kata `RuntimeClass`** — Milestone 5 (optional hardening).
- **Multi-tenant load test** — Milestone 5.

---

## 11. Investigation Findings

### 11.A — Named Pipe Protocol

**Source:** `src/Runner.Common/ProcessChannel.cs` and `src/Runner.Common/StreamString.cs` in the actions/runner repository (`.context/src/github-actions-runner`).

**Findings:**

The protocol uses **OS anonymous pipes** (not named pipes / FIFOs). The Listener (parent process) creates two `AnonymousPipeServerStream` instances and passes their client handle strings as positional arguments to Runner.Worker:

```
Runner.Worker --startuptype workerprocess <read-fd> <write-fd>
```

On Linux, `.GetClientHandleAsString()` returns the integer file descriptor number as a string. The wrapper creates two anonymous pipes with `os.Pipe()`, passes the read end of pipe-in and the write end of pipe-out via `cmd.ExtraFiles` (which maps to fd 3 and fd 4 in the child), then passes "3" and "4" as the positional args.

**Wire format** (`StreamString.WriteInt32Async` + `WriteStringAsync`):

```
[4 bytes] MessageType as little-endian int32 (1 = NewJobRequest)
[4 bytes] byte-length of body as little-endian int32
[N bytes] body encoded as UTF-16LE  (C# UnicodeEncoding = UTF-16LE)
```

`BitConverter.GetBytes` on x86/x64 Linux is little-endian. The string encoding is `UnicodeEncoding` (UTF-16LE, no BOM). The previous implementation had three errors: it used FIFOs instead of anonymous pipes, big-endian instead of little-endian, and UTF-8 instead of UTF-16LE.

**Process arguments** (corrected during M3/M4 live-cluster dry-run, 2026-05-27):

The initial draft of this section claimed the invocation is
`Runner.Worker --startuptype workerprocess <readFD> <writeFD>` (4 args). That
is wrong. Runner.Worker's `Program.MainAsync` asserts `args.Length == 3` and
`args[0].ToLowerInvariant() == "spawnclient"`
([Program.cs](https://github.com/actions/runner/blob/v2.327.1/src/Runner.Worker/Program.cs)),
so the correct invocation is:

```
Runner.Worker spawnclient <readFD> <writeFD>
```

`args[1]` is `pipeIn` (the wrapper's read-end FD as seen in the child), `args[2]`
is `pipeOut`. The wrapper passes "3" and "4" (Go's `cmd.ExtraFiles[0]` maps to
fd 3 and `[1]` to fd 4 in the child).

**Outstanding gap (resolved 2026-05-27 — Q5a):** Runner.Worker also
reads its runner configuration from `/home/runner/.runner`,
`/home/runner/.credentials`, and `/home/runner/.credentials_rsaparams` (see
`Runner.Common/ConfigurationStore.cs` — `GetSettings()` enforces non-null
settings). Without them Runner.Worker fails at job start with
`ArgumentNullException: configuredSettings`.

The end-to-end plumbing is now:

1. `GithubRegistrar.Register` retains the raw `encoded_jit_config` it
   already parsed and exposes it on `AgentCredentials`
   ([github_registrar.go](../../cmd/agc/internal/agentpool/github_registrar.go)).
2. `Pool.createAgent` writes the blob into the agent Secret under
   `encodedJITConfig`; `secretToAgent` restores it onto the `Agent` struct so
   the AGC reconciler picks it up on restart
   ([pool.go](../../cmd/agc/internal/agentpool/pool.go)).
3. The listener passes `cfg.Agent.EncodedJITConfig` into `JobHandlerFunc`,
   which the provisioner forwards into the worker Secret under the
   `jitconfig` key
   ([goroutine.go](../../cmd/agc/internal/listener/goroutine.go),
   [provisioner.go](../../cmd/agc/internal/provisioner/provisioner.go)).
4. The wrapper reads `<payloadDir>/jitconfig`, base64-decodes the outer blob,
   JSON-unmarshals the file map, base64-decodes each entry, and writes the
   three runner-config files into `$RUNNER_HOME_DIR` (default `/home/runner`)
   with mode 0600 before exec'ing Runner.Worker
   ([worker/main.go](../../cmd/worker/main.go) — `materializeJITConfig`).

Unit tests pin: the agent Secret round-trip
(`TestPool_EnsureAgents_StoresEncodedJITConfig`), the worker Secret hand-off
(`TestProvisioner_ForwardsJITConfigIntoSecret` /
`TestProvisioner_OmitsJITKeyWhenEmpty`), and the wrapper-side
materialization (`TestMaterializeJITConfig_*`). Live-cluster validation
remains gated on Q5c.

**Implementation:** `cmd/worker/main.go` — `writeJobMessage` and `encodeUTF16LE`.

---

### 11.B — Worker Image Source

**Source:** TBD

**Findings:** TBD

---

### 11.C — Worker pod must trust the per-tenant egress proxy CA

**Surfaced by:** 2026-05-29 live kind dry-run of `E2E_GitHub_RealDispatch`
(Tier C `Label("github-real")`) against the `actions-gateway-test` GitHub
App and the `actions-gateway/gateway-test` workflow.

**Symptom:**

- Worker pod is scheduled with
  `HTTPS_PROXY=https://actions-gateway-proxy.<ns>.svc.cluster.local:8080`
  (and matching `HTTP_PROXY`). Volume mounts: only the `job-payload`
  Secret. No mount for the per-tenant `actions-gateway-proxy-tls` Secret.
- Wrapper executes, payload + JIT config are materialized
  (`/home/runner/.runner`, `/home/runner/.credentials`,
  `/home/runner/.credentials_rsaparams`), Runner.Worker 2.327.1 starts and
  reads the job message from the pipe (`Message received` /
  `Job message: ...`).
- Every outbound HTTPS call from Runner.Worker via the proxy fails the
  outer TLS handshake with
  `System.Security.Authentication.AuthenticationException: The remote
  certificate is invalid because of errors in the certificate chain:
  UntrustedRoot`. Affected paths observed in the same run:
    - `JobExtension` connectivity check
    - `ResultServer` init
    - `JobServerQueue` log uploads
    - `GitHubActionsService` log-blob signed-URL fetch
    - `RunServer.CompleteJobAsync` (final completion)
- Runner exits 1 after ~3m of retries. AGC observes
  `worker pod completed phase=Failed reason="" duration=3m50s`.
- RenewJob from the AGC starts returning `401 Not authorized for this job`
  ~60s in (the run-service revokes the lock once the runner abandons).
- GitHub-side run concludes `cancelled`.

**Root cause:**

The .NET HttpClient validates the proxy's TLS certificate before sending
`CONNECT`. The proxy's cert is signed by a cert-manager-issued
self-signed CA (the `actions-gateway-proxy-tls` Secret in the tenant
namespace). The runner image's default OS trust store
(`/etc/ssl/certs/ca-certificates.crt`) does not contain that CA, so the
outer TLS handshake fails before any traffic ever reaches GitHub.

The GMC already mounts this Secret into the AGC pod (cert only, via
`Items: [tls.crt]`) at `/etc/actions-gateway/proxy-tls/tls.crt` — see
`buildAGCDeployment` in
[cmd/gmc/internal/controller/builder.go](../../cmd/gmc/internal/controller/builder.go)
~lines 494-509 — and the AGC code path reads it via the
`appendProxyCAToSystemPool` helper landed under Q5f. Worker
pods need the symmetric treatment, but the AGC provisioner's `BuildPod`
([cmd/agc/internal/provisioner/pod_builder.go](../../cmd/agc/internal/provisioner/pod_builder.go))
never adds that volume.

**Fix sketch (tracked as Q5h):**

1. **AGC provisioner pod builder:** Mount `actions-gateway-proxy-tls`
   (cert only, `Items: [{Key: tls.crt, Path: tls.crt}]`) into the runner
   container at a fixed path (e.g.
   `/etc/actions-gateway/proxy-tls/tls.crt`). Symmetric to the AGC
   mount.
2. **GMC `AGC_EXTRA_*` plumbing:** Thread the proxy-TLS Secret name into
   the AGC deployment as an env var (or hard-code the canonical name —
   the Secret name is deterministic per tenant). AGC reads it when
   building the worker pod spec.
3. **Worker entrypoint wrapper:** After payload + JIT materialization
   and before exec'ing Runner.Worker, read the mounted CA cert and:
    - If the OS trust dir is writable (it should be under
      `runAsUser: 1001` + `fsGroup` in the actions-runner base image),
      append the cert to `/etc/ssl/certs/ca-certificates.crt`
      (or drop into `/usr/local/share/ca-certificates/` and call
      `update-ca-certificates`). .NET on Linux uses OpenSSL's bundle by
      default, so this is sufficient for both the wrapper and
      Runner.Worker.
    - As a fallback, build a combined PEM at a writable path and export
      `SSL_CERT_FILE` + `SSL_CERT_DIR` to both wrapper and child env.
4. **Tests:**
    - `pod_builder_test.go`: assert the runner container has the
      `actions-gateway-proxy-tls` Secret volume + mount, cert-only
      (no `tls.key`).
    - `cmd/worker/worker_test.go`: stage a fake CA at the mount path,
      run the wrapper against a stub `Runner.Worker`, assert the CA was
      appended to the bundle (or `SSL_CERT_FILE` set) before the child
      ran.
5. **Doc updates:**
    - `docs/design/02-architecture.md` — call out the worker proxy-CA
      trust requirement in the egress-proxy section.
    - `docs/design/05-security.md` — note that the proxy CA is
      tenant-scoped and the worker trust-store install is per-pod.
    - `docs/operations/troubleshooting.md` — add a runbook entry keyed
      on the `UntrustedRoot` log line.

**Why this matters for the green-checkmark criterion:**

Without 5h, no real workflow can complete: the runner cannot post step
logs, cannot upload step results, and cannot call `CompleteJob` against
the run-service. The job runs (the `echo` step probably succeeds in
the container) but is invisible to GitHub, which times out the lock and
marks the run `cancelled`. Once Q5h ships, Q6 can be re-run against
the same kind cluster and should reach the green checkmark.

**Resolution (Q5h shipped):**

- AGC pod provisioner gained `Provisioner.ProxyTLSSecretName`
  ([cmd/agc/internal/provisioner/provisioner.go](../../cmd/agc/internal/provisioner/provisioner.go)).
  When non-empty, `buildPod` adds an `Items: [tls.crt]` Secret volume
  + read-only mount at `/etc/actions-gateway/proxy-ca/tls.crt` and
  exports `PROXY_CA_CERT_PATH` on the runner container.
  `tls.key` is never projected, keeping the proxy private key off
  worker pods. Two new unit tests pin the mount shape and the
  empty-secret-name no-op path
  ([provisioner_test.go](../../cmd/agc/internal/provisioner/provisioner_test.go) —
  `TestBuildPod_MountsProxyCASecret`, `TestBuildPod_NoProxyCAWhenSecretNameEmpty`).
- AGC `main.go` reads `PROXY_TLS_SECRET_NAME` and plumbs it into the
  provisioner.
- GMC `buildAGCDeployment` sets `PROXY_TLS_SECRET_NAME=
  actions-gateway-proxy-tls` on the AGC Deployment so each tenant's AGC
  finds the right Secret automatically
  ([cmd/gmc/internal/controller/builder.go](../../cmd/gmc/internal/controller/builder.go));
  `TestBuildAGCDeployment_PlumbsProxyTLSSecretName` guards the env.
- Worker entrypoint wrapper installs the CA into a combined trust
  bundle and sets `SSL_CERT_FILE` on the child Runner.Worker env before
  exec'ing ([cmd/worker/main.go](../../cmd/worker/main.go) —
  `installProxyCATrust` / `readSystemCABundle`). System bundle missing,
  CA file missing, CA file empty, and `PROXY_CA_CERT_PATH=""` are all
  tolerated as no-ops so unit-test and non-proxied deployments keep
  working. Five unit tests cover the helper plus an end-to-end test
  (`TestWrapper_PropagatesProxyTrustEnvToChild`) that asserts the child
  process sees `SSL_CERT_FILE` pointing at the combined bundle.

The next live dry-run of Q6 should reach the green checkmark
without `UntrustedRoot` log lines. M-11b (Ed25519 live probe) and Q7
(egress-proxy live curl validation) are unblocked at the same time.

---

### 11.D — GMC readiness probe did not gate on webhook server start

**Surfaced by:** 2026-05-30 re-run of `E2E_GitHub_RealDispatch` (Queue
Q6). The test's `BeforeAll` does `kubectl set env` on the GMC
Deployment to swap fakegithub→real-GitHub env vars, which triggers a
rolling update. It then waits for `kubectl rollout status` to settle
and immediately applies the tenant `ActionsGateway` CR. Every attempt
failed in `kubectl apply` with:

```
Internal error occurred: failed calling webhook
"vactionsgateway-v1alpha1.kb.io": failed to call webhook:
Post "https://gmc-webhook-service.gmc-system.svc:443/...?timeout=10s":
context deadline exceeded
```

**Root cause:** `cmd/gmc/cmd/main.go` only registered
`mgr.AddReadyzCheck("readyz", healthz.Ping)` — a probe that returns OK
as soon as the manager process is up. controller-runtime starts the
webhook listener on port 9443 in a separate goroutine that races
against the readiness probe. Once readyz returns 200, the kubelet
marks the pod Ready and the EndpointSlice for `gmc-webhook-service`
adds the new pod IP. The kube-apiserver routes admission calls to that
pod — but its webhook listener may not yet be bound, so every
`kubectl apply ActionsGateway` racing a GMC rollout hangs for the
admission `timeout=10s` and fails.

**Fix:** Added a second readyz check gated on
`mgr.GetWebhookServer().StartedChecker()`
([cmd/gmc/cmd/main.go](../../cmd/gmc/cmd/main.go)) — the
controller-runtime helper returns nil only after the webhook listener
is bound *and* a TLS self-dial succeeds. Conditional on
`ENABLE_WEBHOOKS != "false"` so envtest runs that disable webhooks
keep marking themselves Ready.

**Production impact:** This race affected every GMC rolling update in
production, not just the e2e suite. Any concurrent `kubectl apply` of
an `ActionsGateway` CR during a GMC image roll or env-var change had a
1–2s window where it could time out. The fix is a one-line addition
and ships in the same branch as the Q6 re-run.

**Follow-up identified:** The egress proxy
([cmd/proxy/proxy.go](../../cmd/proxy/proxy.go)) has the same class of
bug — its `/healthz` returns OK as soon as the health-port server
binds, but the CONNECT listener on port 8080 is started in a separate
goroutine. The GMC's per-tenant proxy Deployment
([cmd/gmc/internal/controller/builder.go](../../cmd/gmc/internal/controller/builder.go))
uses `/healthz` for both liveness and readiness, so worker pods can
hit `connection refused` on `HTTPS_PROXY` traffic during a proxy
rollout or HPA scale-up. Tracked as a Queue item in
[docs/STATUS.md](../STATUS.md).
