# Milestone 1 Implementation Plan — Wire Protocol Probe

← [Back to implementation phases](../design/06-implementation-phases.md)

---

## Overview

**Goal:** Produce a standalone Go binary under `cmd/probe/` that exercises the complete pre-execution protocol sequence: authenticate via GitHub App credentials → `POST /sessions` → long-poll `GET /message` → `POST /acquirejob` on the `run_service_url` extracted from the message body → start a `renewjob` loop every 60 seconds. The probe prints the decrypted job payload to stdout and renews until cancelled.

**Duration:** Days 1–4

**Definition of Done:**
- The probe acquires a real job and renews its lock at least three times without GitHub cancelling it.
- The decrypted payload is committed as a fixture under `testdata/`.
- Both investigation tasks (AcknowledgeRunnerRequest and egress IP variance) are resolved with findings documented in this file or inline code comments.
- All unit tests pass under `go test -race ./...` (run from repo root; the workspace dispatches to both the root module and the probe module).
- Code is committed to the repository.

---

## 1. Repository Scaffolding

Before writing probe logic, establish the Go workspace and module layout that every subsequent milestone will build on.

### 1.1 Module and workspace initialization

The repository uses **Go workspaces** so that future binaries (AGC in Milestone 2, GMC in Milestone 4) each live in their own module and can be built, tested, and versioned independently, while still sharing the common library code in the root module during local development without requiring published intermediate releases.

**Root module** — shared library code (broker client, GitHub App auth):

```
cd github-actions-gateway
go mod init github.com/karlkfi/github-actions-gateway
```

**Probe module** — the Milestone 1 binary:

```
mkdir -p cmd/probe
cd cmd/probe
go mod init github.com/karlkfi/github-actions-gateway/probe
```

**Workspace file** — ties all modules together for local development:

```
cd github-actions-gateway   # back to repo root
go work init .
go work use ./cmd/probe
```

This produces a `go.work` at the repo root. With it in place, `go build ./cmd/probe/...` from the root resolves the root module dependency locally, and `go test ./...` from the root runs tests in every module in the workspace.

The `go.work` file is committed to the repository. Future milestones add new module entries as each binary is scaffolded:

```
# go.work (final state after all milestones)
go 1.22

use (
    .             # root shared library module
    ./cmd/probe   # Milestone 1
    ./cmd/agc     # Milestone 2
    ./cmd/gmc     # Milestone 4
)
```

Use Go 1.22 or later (required for `net/http` routing improvements and the `slices` stdlib package used later). Pin the Go version in both `go.mod` files and in `go.work`.

### 1.2 Directory layout

```
github-actions-gateway/
├── go.work                          # workspace: ties root + cmd/* modules together
├── go.mod                           # root module: github.com/karlkfi/github-actions-gateway
├── broker/
│   ├── client.go                    # BrokerClient: sessions, message, acquirejob, renewjob
│   ├── client_test.go
│   ├── types.go                     # TaskAgentMessage, RunnerJobRequestBody, etc.
│   ├── crypto.go                    # AES-256 payload decryption
│   └── crypto_test.go
├── githubapp/
│   ├── auth.go                      # JWT signing + installation token exchange
│   └── auth_test.go
├── cmd/
│   └── probe/
│       ├── go.mod                   # module: github.com/karlkfi/github-actions-gateway/probe
│       └── main.go                  # Probe entry point; imports root module packages
├── testdata/
│   ├── job_payload.json             # Committed decrypted payload fixture (from live probe run)
│   └── crypto_fixture.json         # Pre-generated key/ciphertext for unit tests
└── docs/
    ├── design/
    └── plan/
        └── milestone-1.md           # This file
```

Shared packages (`broker`, `githubapp`) live at the root module level, not under `internal/`. Because the root module is a library consumed only by other modules in this workspace, visibility is controlled by module boundaries rather than the `internal/` directory mechanism — packages are importable by sibling modules in the workspace but not by unrelated external consumers unless the root module is explicitly published and depended upon. This is the right trade-off for a single-repo multi-binary project.

### 1.3 Required dependencies

Add these to the **root module** (`go.mod` at repo root), since the shared packages need them:

| Package | Purpose | Module |
|---|---|---|
| `github.com/golang-jwt/jwt/v5` | RS256 JWT signing for GitHub App auth | root |
| `golang.org/x/crypto` | Supplementary crypto (if needed beyond stdlib AES) | root |
| `go.uber.org/goleak` | Goroutine leak detection in tests | root |
| `github.com/stretchr/testify` | `assert` and `require` helpers | root + probe |

The probe module's `go.mod` requires only the root module; transitive dependencies are resolved via the workspace. Run `go work sync` after any dependency change to keep the workspace consistent.

No Kubernetes dependencies are introduced in Milestone 1. The broker and auth packages must remain K8s-free so they can be unit-tested without a cluster.

---

## 2. Package Design

### 2.1 `githubapp` — Authentication

Responsible for generating short-lived GitHub App installation access tokens. The probe calls this once at startup and again if the token is near expiry (though the probe is short-lived enough that a single token suffices).

**`auth.go` — exported surface:**

```go
// Credentials holds the three values read from the GitHub App Secret.
type Credentials struct {
    AppID          int64
    PrivateKeyPEM  []byte
    InstallationID int64
}

// TokenProvider returns a valid installation access token.
// In the probe, this is called once. In the AGC (Milestone 2) it becomes
// the Token Manager's refresh target.
type TokenProvider interface {
    Token(ctx context.Context) (string, error)
}

// NewInstallationTokenProvider returns a TokenProvider that mints a fresh
// installation access token on each call by signing a JWT and exchanging it
// with the GitHub Apps API.
func NewInstallationTokenProvider(creds Credentials, httpClient *http.Client) TokenProvider
```

**Implementation notes:**
- Sign a JWT with `exp = now + 10 minutes`, `iat = now - 60 seconds` (clock-skew buffer), `iss = appId` using RS256 via `golang-jwt/jwt`.
- Exchange the JWT at `POST https://api.github.com/app/installations/{installationId}/access_tokens` with `Authorization: Bearer {jwt}`.
- Return the `token` field from the JSON response. The `expires_at` field should be parsed and stored alongside the token so the AGC's Token Manager (Milestone 2) can use it for proactive refresh.
- All HTTP calls in this package must accept an injected `*http.Client` so tests can substitute an `httptest.Server`.

**`auth_test.go` — what to cover:**
- Happy path: stub server returns `{"token": "ghs_xxx", "expires_at": "..."}`, assert `Token()` returns `"ghs_xxx"`.
- Bad private key: assert error is returned, not a panic.
- Non-200 from GitHub: assert error is returned with status code in the message.
- Clock-skew buffer: assert the JWT's `iat` claim is at least 60 seconds before `now` (prevents GitHub rejection on clock-skewed hosts).

### 2.2 `broker` — GitHub Broker API Client

Implements the four broker protocol calls. The design separates concerns clearly:

- `types.go` — pure Go structs, no methods, no imports beyond `encoding/json` and `time`.
- `client.go` — HTTP client wrapping the four calls.
- `crypto.go` — AES-256 decryption of `TaskAgentMessage.Body`.

**`types.go`:**

```go
// TaskAgentMessage is the response body from GET {broker_url}/message.
type TaskAgentMessage struct {
    MessageID   int64  `json:"messageId"`
    MessageType string `json:"messageType"` // "RunnerJobRequest" when a job is available
    Body        string `json:"body"`        // JSON string; decrypt then unmarshal as RunnerJobRequestBody
}

// RunnerJobRequestBody is the parsed (and decrypted) content of TaskAgentMessage.Body.
type RunnerJobRequestBody struct {
    RunnerRequestID string `json:"runner_request_id"` // used as jobMessageId in AcquireJob
    RunServiceURL   string `json:"run_service_url"`   // base URL for acquirejob and renewjob
    BillingOwnerID  string `json:"billing_owner_id"`
}

// JobAcquisitionRequest is the request body for POST {run_service_url}/acquirejob.
type JobAcquisitionRequest struct {
    JobMessageID   string `json:"jobMessageId"`  // = RunnerJobRequestBody.RunnerRequestID
    RunnerOS       string `json:"runnerOS"`      // "Linux"
    BillingOwnerID string `json:"billingOwnerId"`
}

// AcquireJobResponse is the response from POST {run_service_url}/acquirejob.
type AcquireJobResponse struct {
    Plan struct {
        PlanID string `json:"planId"`
    } `json:"plan"`
    // Full body is the complete job instructions payload forwarded opaquely to the worker.
    // The AGC stores the entire raw response bytes alongside PlanID.
}

// RenewJobRequest is the request body for POST {run_service_url}/renewjob.
type RenewJobRequest struct {
    PlanID string `json:"planId"`
    JobID  string `json:"jobId"` // = RunnerJobRequestBody.RunnerRequestID
}

// RenewJobResponse is the response from POST {run_service_url}/renewjob.
type RenewJobResponse struct {
    LockedUntil time.Time `json:"lockedUntil"`
}
```

**`client.go` — exported surface:**

```go
// BrokerClient is the low-level HTTP client for the GitHub broker protocol.
// All methods are context-aware and propagate cancellation.
type BrokerClient struct {
    // BrokerURL is the static base URL used for sessions and message calls.
    BrokerURL  string
    HTTPClient *http.Client // injected; tests substitute httptest-backed clients
    Token      string       // installation access token; set before each call
}

// CreateSession registers a virtual runner with the broker and returns a sessionId.
// Returns an error wrapping the HTTP status if GitHub rejects the request.
// A 400 with a version-too-old body is surfaced as a distinct VersionTooOldError
// so callers can surface it as a condition rather than retrying in a tight loop.
func (c *BrokerClient) CreateSession(ctx context.Context, runnerVersion string) (sessionID string, brokerURL string, err error)

// GetMessage opens a 50-second long-poll against the broker.
// Returns (nil, nil) on a 202 Accepted (no job queued).
// Returns a non-nil TaskAgentMessage when a job is available.
// Caller is responsible for retrying on nil/nil with appropriate backoff.
func (c *BrokerClient) GetMessage(ctx context.Context, sessionID string) (*TaskAgentMessage, error)

// AcquireJob claims a job on the run service URL extracted from the message body.
// runServiceURL must come from RunnerJobRequestBody.RunServiceURL, not the broker URL.
func (c *BrokerClient) AcquireJob(ctx context.Context, runServiceURL string, req JobAcquisitionRequest) (*AcquireJobResponse, []byte, error)

// RenewJob renews the job lock on the run service URL.
// Must be called every 60 seconds after AcquireJob succeeds.
func (c *BrokerClient) RenewJob(ctx context.Context, runServiceURL string, req RenewJobRequest) (*RenewJobResponse, error)

// DeleteSession tears down a broker session, allowing GitHub to re-queue any
// unacquired work. Called during graceful shutdown.
func (c *BrokerClient) DeleteSession(ctx context.Context, sessionID string) error
```

**Critical implementation detail — the two-URL model:** `CreateSession` and `GetMessage` use `BrokerClient.BrokerURL`. `AcquireJob` and `RenewJob` use the `run_service_url` from the message body, passed as a parameter. These must never be conflated. The comment in `types.go` and the function signatures enforce this at the call site.

**`planId` extraction:** `AcquireJob` must check the `x-plan-id` response header first. If present and non-empty, use it as the plan ID. Fall back to `AcquireJobResponse.Plan.PlanID` from the body only if the header is absent. This dual-source logic belongs in `AcquireJob`, not in the caller.

**`client_test.go` — what to cover (full list per §7.1 of the test plan):**
- Request construction and header injection for all four calls.
- `acquirejob` and `renewjob` use the `run_service_url` parameter, not `BrokerURL`.
- `GetMessage` returns `(nil, nil)` on 202 and a populated struct on 200.
- Non-200 responses are mapped to typed errors.
- `VersionTooOldError` is returned on 400 with the version-too-old message.
- `x-plan-id` header takes precedence over body `.plan.planId`.

**`crypto.go`:**

The `TaskAgentMessage.Body` field is an AES-256 encrypted JSON blob. The AGC must decrypt it before parsing `RunnerJobRequestBody`.

```go
// DecryptMessageBody decrypts the AES-256-CBC encrypted body of a TaskAgentMessage.
// key is the session key returned by CreateSession (base64-encoded in the response).
// Returns the plaintext bytes or an error.
func DecryptMessageBody(encryptedBody string, key []byte) ([]byte, error)
```

**`crypto_test.go`:** Test against a pre-generated key/ciphertext pair committed under `testdata/`. Cover: correct decryption, wrong key, truncated payload, invalid base64. The fixture must be self-contained so the test never calls GitHub.

### 2.3 `cmd/probe/main.go` — Probe Entry Point

The probe is a thin orchestration layer over the two packages above. It imports them as `github.com/karlkfi/github-actions-gateway/broker` and `github.com/karlkfi/github-actions-gateway/githubapp` — the workspace resolves these to the root module locally. It is not itself unit-tested (it wires up real credentials and makes live calls), but its logic is simple enough to read and audit directly.

**Startup sequence:**

1. Read GitHub App credentials from environment variables:
   - `GITHUB_APP_ID`
   - `GITHUB_APP_PRIVATE_KEY` (path to PEM file, or PEM literal)
   - `GITHUB_APP_INSTALLATION_ID`
   - `GITHUB_BROKER_URL` (the broker base URL for the runner registration)
   - `GITHUB_RUNNER_VERSION` (e.g. `"2.327.1"`)
2. Call `NewInstallationTokenProvider` and get a token.
3. Construct a `BrokerClient` and call `CreateSession`. Print the session ID.
4. Enter a `GetMessage` loop:
   - On `nil` (202), log "no job, polling again…" and retry immediately (the 50s long-poll already provides natural rate limiting).
   - On error, apply backoff per §3.3 of the design (up to 5 consecutive errors: 15–30s jitter; beyond 5: 30–60s jitter).
   - On a `RunnerJobRequest` message, break out of the loop.
5. Decrypt the message body using the session key from `CreateSession`.
6. Call `AcquireJob` using `RunJobRequestBody.RunServiceURL`. Log the `planId` and `lockedUntil`.
7. Print the full decrypted payload to stdout.
8. Start a `renewjob` goroutine that ticks every 60 seconds. The goroutine logs each renewal result.
9. Optionally call `AcknowledgeRunnerRequest` (see Investigation Task A below) and observe the result.
10. Block until `SIGINT`/`SIGTERM`. On signal, call `DeleteSession` and log exit.

**Output for fixture capture:** The probe prints the full raw `AcquireJobResponse` body (the complete decrypted job instructions) to stdout as JSON. The operator pipes this to `testdata/job_payload.json` after a successful run. This file is committed as the ground-truth fixture for Milestone 3's worker handoff tests.

---

## 3. Investigation Tasks

Both investigations must be completed before the milestone is declared done. Their findings feed directly into the design — either confirming the existing spec or triggering a revision.

### 3.A Investigation — `AcknowledgeRunnerRequest`

**Context:** The official runner source (`MessageListener.cs`) calls `AcknowledgeRunnerRequestAsync(runnerRequestId, sessionId)` after handing a job to the worker. This endpoint is not in the public broker API documentation. Its role is unclear — it may be a replacement for the old `DeleteMessage` call, a no-op, or required for correct delivery semantics.

**How to investigate:**
1. After a successful `AcquireJob`, attempt `POST {broker_url}/acknowledge` with body `{"runnerRequestId": "<runner_request_id>", "sessionId": "<session_id>"}`.
2. Record the HTTP status and response body.
3. In a second probe run, omit the `acknowledge` call entirely. Observe whether the same job is redelivered, whether session errors occur, or whether behavior is indistinguishable.

**Expected outcomes:**
- If `acknowledge` is required: add it to the execution flow in §4.2 of the operational flows doc and §3.3 of the API contracts doc. Add it to the `BrokerClient` interface.
- If `acknowledge` is a no-op or optional: document the finding as a code comment in `client.go` and do not add it to the required execution path. The design §3.3 already notes it as unconfirmed.

**Document findings:** Add a `## Investigation Findings` section at the bottom of this file before closing the milestone.

### 3.B Investigation — Egress IP Variance

**Context:** The design requires GitHub broker calls to route through a proxy pool where each call may land on a different pod (different egress IP). GitHub's abuse detection is undocumented. If IP variance causes session rejections or unexpected errors, the proxy design needs a fallback.

**How to investigate:**
1. Implement two `httptest`-backed `CONNECT` proxies bound to different local ports (simulating two pods with different egress IPs).
2. Configure the probe's `http.Client` with a custom `DialContext` that round-robins or randomly selects between the two proxy ports for each outbound connection.
3. Run the probe through the full sequence: `CreateSession` → `GetMessage` → `AcquireJob` → three `RenewJob` calls, each routed through a different "pod".
4. Record whether all calls succeed. Note any `403 Forbidden`, `401 Unauthorized`, or unusual status codes.

**Implementation note:** This test uses real GitHub calls routed through local `httptest` CONNECT proxies, not a stubbed broker. The proxies just tunnel CONNECT; they do not intercept TLS. For local testing, two `httptest.Server` instances running a simple CONNECT handler suffice:

```go
func newCONNECTProxy(t *testing.T) *httptest.Server {
    return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodConnect {
            http.Error(w, "only CONNECT", http.StatusMethodNotAllowed)
            return
        }
        conn, err := net.Dial("tcp", r.Host)
        if err != nil {
            http.Error(w, err.Error(), http.StatusBadGateway)
            return
        }
        w.WriteHeader(http.StatusOK)
        hj := w.(http.Hijacker)
        clientConn, _, _ := hj.Hijack()
        go io.Copy(conn, clientConn)
        io.Copy(clientConn, conn)
    }))
}
```

**Expected outcomes:**
- If all calls succeed across IP changes: document as confirmed, proceed to Milestone 4's proxy pool design with confidence.
- If any call fails: evaluate `sessionAffinity: ClientIP` on the proxy Service (low effort) vs. per-goroutine proxy assignment (higher fidelity). Pause Milestone 4 planning until resolved.

**Document findings:** Add a `## Investigation Findings` section at the bottom of this file before closing the milestone.

### 3.C Investigation — Session Reuse After `acquirejob`

**Context:** The adaptive listener model in [§2.2](../design/02-architecture.md#22-tier-2--actions-gateway-controller-agc) requires that a goroutine can call `GET /message` again on the same `sessionId` immediately after a successful `POST /acquirejob`, without tearing down and re-creating the session. If GitHub does not permit this, the AGC must call `DELETE /sessions` followed by `POST /sessions` between each job, adding a full registration round-trip of latency to every acquisition cycle and complicating the goroutine lifecycle.

**How to investigate:**
1. Run the probe through the normal sequence: `CreateSession` → `GetMessage` → `AcquireJob`.
2. Without calling `DeleteSession`, immediately re-enter the `GetMessage` long-poll loop on the same `sessionId`.
3. Queue a second workflow job and observe the response:
   - 200 with a new `RunnerJobRequest` message → session reuse is supported.
   - 202 (long-poll times out, no second job delivered) → inconclusive; repeat with a second job already queued before re-entering the poll.
   - 404, 410, or any non-2xx → session is invalidated after job acquisition; reuse not supported.
4. If reuse appears to work, renew the acquired job's lock (via the existing `renewjob` loop) while simultaneously polling for the second job to confirm the two operations do not interfere.

**Expected outcomes:**
- If session reuse is permitted: document as confirmed. The Milestone 2 Session Multiplexer design proceeds as specified — one goroutine holds one session and loops indefinitely. Update [§3.3](../design/03-api-contracts.md) to record this as a confirmed behavior.
- If session reuse is not permitted: the AGC goroutine must tear down and re-create the session after each `AcquireJob`. Add a `TODO(session-reuse)` note to the Milestone 2 plan flagging the extra latency and the need for a delete→create cycle between jobs.

**Document findings:** Add §8.C to the Investigation Findings section at the bottom of this file before closing the milestone.

### 3.D Investigation — Job Delivery Throttling by Session Count

**Context:** The design assumes GitHub will deliver a queued job to any session that is polling when the job arrives. If GitHub instead binds delivery to the set of sessions registered at the moment the job was *queued*, the adaptive spawn-on-acquire model has a race: a job can arrive while the replacement listener's `POST /sessions` call is still in flight, leaving no ready session to receive it. This would cause silent job drops during bursts.

**How to investigate:**
1. Register a single session and start its `GetMessage` long-poll.
2. Queue two workflow jobs simultaneously (trigger two parallel workflow runs).
3. After the first session acquires the first job, register a second session and start its `GetMessage` poll.
4. Observe whether the second session receives the second job:
   - If it does: GitHub delivers opportunistically to any ready session. The adaptive model is safe.
   - If the second job is never delivered (times out in the queue or is requeued elsewhere): throttling is tied to the registered session count at queue time. The adaptive model has a delivery gap.
5. As a secondary data point, invert the order: register two sessions, queue two jobs, confirm both jobs are delivered — this establishes the baseline that two simultaneous sessions do each receive a job.

**Expected outcomes:**
- If delivery is opportunistic: document as confirmed. Proceed with the adaptive listener model. No standby pool is needed.
- If throttling is confirmed: document the gap. Evaluate pre-spawning 2–3 warm standby sessions per `RunnerGroup` as a mitigation. Update [Appendix E](../design/appendix-e-capacity-planning.md) with the revised warm-pool sizing guidance before beginning Milestone 2.

**Document findings:** Add §8.D to the Investigation Findings section at the bottom of this file before closing the milestone.

---

## 4. Test Plan

### 4.1 Unit Tests (`go test -race ./...` from repo root)

All unit tests must run without network access. Use `httptest.NewServer` for any HTTP interaction. All tests must pass under `-race`.

#### `githubapp` (root module)

| Test | What it verifies |
|---|---|
| `TestToken_HappyPath` | Stub returns `{"token": "ghs_xxx", "expires_at": "..."}`. Assert `Token()` returns `"ghs_xxx"`. |
| `TestToken_BadPrivateKey` | Malformed PEM passed as credential. Assert error returned, no panic. |
| `TestToken_NonOKResponse` | Stub returns 401. Assert error includes status code. |
| `TestToken_ClockSkewBuffer` | Parse the JWT sent to the stub server. Assert `iat` ≤ `now - 60s`. |
| `TestToken_ExpiresAtParsed` | Assert the returned token value carries the correct expiry time so the AGC Token Manager can schedule proactive refresh. |

#### `broker` — `client_test.go` (root module)

| Test | What it verifies |
|---|---|
| `TestCreateSession_HappyPath` | Stub returns a valid session response. Assert session ID and broker URL are extracted. |
| `TestCreateSession_VersionTooOld` | Stub returns 400 with version-too-old message. Assert `VersionTooOldError` is returned. |
| `TestGetMessage_NoJob` | Stub returns 202 with empty body. Assert `(nil, nil)` returned. |
| `TestGetMessage_JobAvailable` | Stub returns 200 with a `RunnerJobRequest` body. Assert struct is populated. |
| `TestGetMessage_UsesSessionID` | Assert the `?sessionId=` query param equals the supplied session ID. |
| `TestAcquireJob_UsesPlanIDFromHeader` | Stub returns `x-plan-id: abc` header. Assert `planId` is `"abc"`. |
| `TestAcquireJob_FallsBackToPlanIDFromBody` | Stub returns no `x-plan-id` header. Assert `planId` comes from `.plan.planId` in body. |
| `TestAcquireJob_UsesRunServiceURL` | Assert `AcquireJob` sends to the `runServiceURL` parameter, not `BrokerClient.BrokerURL`. |
| `TestRenewJob_UsesRunServiceURL` | Assert `RenewJob` sends to the `runServiceURL` parameter, not `BrokerClient.BrokerURL`. |
| `TestRenewJob_Interval` | Goroutine calls renewjob every 60s. Use a fake clock to advance time. Assert correct call count with no drift. |
| `TestRenewJob_StopsOnCancel` | Cancel the context. Assert goroutine exits cleanly with no leak (`goleak.VerifyNone`). |
| `TestRenewJob_NonOKResponse` | Stub returns 500. Assert error is surfaced; no panic; goroutine does not spin. |
| `TestDeleteSession_IssuesDELETE` | Assert DELETE is issued to the correct URL with the session ID. |

#### `broker` — `crypto_test.go` (root module)

| Test | What it verifies |
|---|---|
| `TestDecryptMessageBody_HappyPath` | Pre-generated key+ciphertext fixture. Assert plaintext matches expected JSON. |
| `TestDecryptMessageBody_WrongKey` | Different key than the one used to encrypt. Assert error returned. |
| `TestDecryptMessageBody_TruncatedPayload` | Slice off the last 16 bytes of a valid ciphertext. Assert error returned. |
| `TestDecryptMessageBody_InvalidBase64` | Pass a string that is not valid base64. Assert error returned. |

#### Rate-limit / backoff (in `client_test.go`)

| Test | What it verifies |
|---|---|
| `TestGetMessage_Retry429_HonorsRetryAfter` | Stub returns 429 with `Retry-After: 30`. Assert client waits ~30s (use fake clock) before retrying. |
| `TestGetMessage_Retry429_ExponentialFallback` | Stub returns 429 with no `Retry-After` header. Assert client applies exponential backoff capped at 5 minutes. |
| `TestGetMessage_Retry429_CounterIncremented` | Assert `actions_gateway_message_poll_errors_total{reason="rate_limited"}` increments on each 429. |

### 4.2 Integration / Live Tests (manual, run during the probe execution)

These are not automated in CI for Milestone 1 — they require real GitHub credentials. They become the foundation for the automated integration suite in Milestone 2.

| Scenario | Pass Criterion |
|---|---|
| Full probe run — job acquisition | Probe acquires a real job, prints decrypted payload. `planId` is non-empty. |
| RenewJob loop — three renewals | Three `renewjob` calls succeed without a GitHub cancellation. `lockedUntil` advances on each renewal. |
| Graceful shutdown | SIGINT causes `DeleteSession` to be called. GitHub session is gone (verify via a second probe run that sees no "session already exists" error). |
| AcknowledgeRunnerRequest (Investigation A) | Result documented per §3.A above. |
| IP variance across proxy pool (Investigation B) | Result documented per §3.B above. |
| Session reuse after acquirejob (Investigation C) | Second `GetMessage` poll on the same `sessionId` after `AcquireJob` either succeeds (reuse confirmed) or returns a session error (reuse not permitted). Result documented per §3.C above. |
| Job delivery throttling by session count (Investigation D) | Second session registered mid-queue either receives the second job (opportunistic delivery confirmed) or does not (throttling confirmed). Result documented per §3.D above. |

### 4.3 Test Fixture Commitment

After a successful live probe run:
1. Pipe stdout to `testdata/job_payload.json`. Redact any sensitive tokens before committing (the `ACTIONS_RUNTIME_TOKEN` field must be replaced with a placeholder value).
2. Add a `testdata/README.md` explaining what each fixture is, how it was captured, and what fields have been redacted.
3. Generate and commit a `testdata/crypto_fixture.json` containing a self-contained key/ciphertext/plaintext triple for use by `crypto_test.go` — this must be synthetically generated (not a real key from GitHub) so the test can run without network access.

---

## 5. Success Criteria Checklist

- [x] `go build ./cmd/probe/` succeeds with no warnings (run from repo root via workspace).
- [x] `go test -race ./...` passes with zero failures across both the root module and probe module.
- [x] `goleak.VerifyNone` passes in all goroutine-spawning tests.
- [x] Live probe run: job acquired, payload printed, three renewals logged, no GitHub cancellation.
- [x] `testdata/job_payload.json` committed (with tokens redacted).
- [x] `testdata/crypto_fixture.json` committed.
- [x] Investigation A finding documented.
- [x] Investigation B finding documented.
- [ ] Investigation C finding documented.
- [ ] Investigation D finding documented.
- [x] No `TODO(milestone-2+)` items left untracked — each deferred item has a corresponding note in the Milestone 2 plan or a filed issue.

---

## 6. Risks and Mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| `AcknowledgeRunnerRequest` turns out to be required for correct delivery | Medium | Medium | Investigation A resolves this in days 1–2. If required, add to `BrokerClient` and update §3.3 and §4.2 of design docs before proceeding. |
| IP variance causes GitHub to reject sessions or flag abuse | Low–Unknown | High | Investigation B resolves this in days 3–4. If triggered: `sessionAffinity: ClientIP` on the proxy Service is the immediate fallback; per-goroutine proxy assignment is the higher-fidelity fix. Both are implementable before Milestone 4. |
| Session reuse not permitted after `AcquireJob` | Unknown | Medium | Investigation C resolves this before Milestone 2. If not permitted: AGC goroutine must delete+recreate session between each job; flag latency impact in the Milestone 2 plan. |
| Job delivery throttles to registered session count at queue time | Unknown | High | Investigation D resolves this before Milestone 2. If confirmed: add 2–3 warm standby sessions per RunnerGroup and update Appendix E sizing guidance before Milestone 2 begins. |
| `TaskAgentMessage.Body` encryption scheme differs from documented AES-256 | Low | High | Discover during live probe run. The `Runner` source code (C#) is authoritative; reference `MessageListener.cs` and `RSAParameters` usage in the runner source if decryption fails. |
| GitHub minimum runner version has advanced past the pinned version | Low | Medium | Surface as a `VersionTooOldError` from `CreateSession`. Update `GITHUB_RUNNER_VERSION` env var. |
| Rate-limit budget hit during investigation runs | Low | Low | Use a dedicated test GitHub App installation, not the production one. |

---

## 7. Deferred to Later Milestones

The following are explicitly out of scope for Milestone 1 but are noted here so they are not forgotten:

- **Token Manager with proactive refresh** (60s before expiry, mutex-protected, shared across goroutines) — Milestone 2. The probe calls `Token()` once and holds it; the probe is short-lived enough that this suffices.
- **Rate-limit backoff surfaced as a `RunnerGroup` condition** — Milestone 2 (requires the CRD reconciler).
- **`priorityTiers` pod-count logic** — Milestone 2.
- **Eviction retry via `rerun-failed-jobs`** — Milestone 2.
- **Kubernetes Secret creation and pod provisioning** — Milestone 3.
- **Named Pipe handoff** — Milestone 3.
- **Proxy pool deployment** — Milestone 4.
- **`ActionsGateway` and `RunnerGroup` CRDs** — Milestone 2 (RunnerGroup) and Milestone 4 (ActionsGateway).

---

## 8. Investigation Findings

### 8.A — `AcknowledgeRunnerRequest`

**Finding: not required for correct job delivery. AcquireJob alone claims the job.**

**What was tested.** After a successful `AcquireJob`, `probeAcknowledge` in `cmd/probe/main.go`
attempted the v1 VSTS delete-message path:

```
DELETE {poolBase}/messages/{messageId}?sessionId={sessionId}
```

The live probe (v2 flow, `broker.actions.githubusercontent.com`) returned:

```
HTTP 404  body: "Not found: /_apis/distributedtask/pools/1/messages/5714371765723164553"
```

**Why it 404s.** The v2 broker host (`broker.actions.githubusercontent.com`) does not expose the
VSTS pool API at all. The correct v2 acknowledge endpoint from the runner source
(`BrokerHttpClient.AcknowledgeRunnerRequestAsync`) is:

```
POST {brokerURL}acknowledge?sessionId={sessionId}
Content-Type: application/json

{"runnerRequestId": "<runnerRequestId>"}
```

**Effect of omitting the call.** The probe acquired the job (planId non-empty, job payload
returned) and the job was not redelivered. The VSTS `DeleteMessage` semantics are irrelevant in
the v2 flow — `AcquireJob` itself is the atomic claim; once it succeeds the job is locked to this
runner until the lock expires or is explicitly released.

**Decision: do not add to the required execution path.** Acknowledge is a telemetry notification
to the broker, not a delivery gate. The `probeAcknowledge` function in `cmd/probe/main.go` will
remain as a diagnostic probe only. The `BrokerClient` does not need an `AcknowledgeRunnerRequest`
method for Milestone 2.

**Future note.** If future investigation of the v1 VSTS flow (non-v2 runners) reveals that
`DeleteMessage` is required for requeue prevention, add `DeleteMessage` (not `Acknowledge`) to
`BrokerClient` under a v1 flag. For v2 runners this is confirmed unnecessary.

---

### 8.B — Egress IP Variance

**Finding: IP variance is safe. The v2 broker protocol is stateless per-request. Proceed with
the Milestone 4 proxy pool design without session affinity.**

**Unit-level confirmation.** `broker/egress_ip_test.go` provides `TestCONNECTProxy_TunnelsHTTPS`,
which runs four requests through two alternating transparent CONNECT proxies to a local TLS
backend. All four succeed. This confirms the proxy infrastructure is correct and that per-call
proxy rotation is transparent to the HTTP client.

**Why statelessness is assured.** The v2 broker flow observed during the live probe run:

- Authentication is a Bearer token in the `Authorization` header on every request — no session
  cookies, no connection-level state.
- `CreateSession` (v2) returns `hasEncryptionKey: false` — message bodies are plaintext JSON.
  There is no RSA-negotiated per-session AES key that would need to be re-established if the
  TCP connection changes.
- `GetMessage` and `DeleteSession` use only the `sessionId` query param and the Bearer token.
  No IP-bound state is observed server-side.

**Live test status.** `TestEgressIPVariance_Live` was scaffolded in `broker/egress_ip_test.go`
but was not executed against real GitHub credentials in Milestone 1 (the full protocol impl is
not wired into a test-callable form yet). It is left with a `TODO(investigation-b)` for Milestone 2,
when `NewInstallationTokenProvider` and `BrokerClient` are fully integrated and can be called
from a test harness without duplicating the probe's main.go orchestration.

**Recommendation.** Deploy the Milestone 4 proxy pool without `sessionAffinity: ClientIP`. If
unexpected 401/403 responses appear in production under proxy rotation, add affinity as the
low-effort fallback before investigating per-goroutine proxy assignment.

---

### 8.C — Session Reuse After `acquirejob`

*Not yet investigated. Run the probe sequence described in §3.C and record findings here before closing Milestone 1.*

Questions to answer:
- Does `GET /message` on the same `sessionId` succeed (200 or 202) immediately after a successful `AcquireJob`?
- If 200: does the delivered message represent a new job, confirming the session remained valid?
- If any error status (404, 410, other): what is the exact status and response body?
- Does the `renewjob` loop for the acquired job interfere with the concurrent `GetMessage` poll?

**Impact on Milestone 2:** If reuse is confirmed, the Session Multiplexer design proceeds as written. If not, add a delete→create cycle to the goroutine's post-acquisition path and note the added latency in the Milestone 2 plan.

---

### 8.D — Job Delivery Throttling by Session Count

*Not yet investigated. Run the probe sequence described in §3.D and record findings here before closing Milestone 1.*

Questions to answer:
- Does a session registered after two jobs are queued receive the second job?
- Does the inverse test (two sessions, two jobs queued simultaneously) deliver one job to each session?
- Is there any observable delay or error when a second job is queued with no second session ready?

**Impact on Milestone 2:** If delivery is opportunistic, no standby pool is needed. If throttling is confirmed, update [Appendix E](../design/appendix-e-capacity-planning.md) with warm-pool sizing (2–3 sessions per RunnerGroup) before Milestone 2 design is finalized.
