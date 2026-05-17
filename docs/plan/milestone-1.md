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

### 4.3 Test Fixture Commitment

After a successful live probe run:
1. Pipe stdout to `testdata/job_payload.json`. Redact any sensitive tokens before committing (the `ACTIONS_RUNTIME_TOKEN` field must be replaced with a placeholder value).
2. Add a `testdata/README.md` explaining what each fixture is, how it was captured, and what fields have been redacted.
3. Generate and commit a `testdata/crypto_fixture.json` containing a self-contained key/ciphertext/plaintext triple for use by `crypto_test.go` — this must be synthetically generated (not a real key from GitHub) so the test can run without network access.

---

## 5. Success Criteria Checklist

- [ ] `go build ./cmd/probe/` succeeds with no warnings (run from repo root via workspace).
- [ ] `go test -race ./...` passes with zero failures across both the root module and probe module.
- [ ] `goleak.VerifyNone` passes in all goroutine-spawning tests.
- [ ] Live probe run: job acquired, payload printed, three renewals logged, no GitHub cancellation.
- [ ] `testdata/job_payload.json` committed (with tokens redacted).
- [ ] `testdata/crypto_fixture.json` committed.
- [ ] Investigation A finding documented.
- [ ] Investigation B finding documented.
- [ ] No `TODO(milestone-2+)` items left untracked — each deferred item has a corresponding note in the Milestone 2 plan or a filed issue.

---

## 6. Risks and Mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| `AcknowledgeRunnerRequest` turns out to be required for correct delivery | Medium | Medium | Investigation A resolves this in days 1–2. If required, add to `BrokerClient` and update §3.3 and §4.2 of design docs before proceeding. |
| IP variance causes GitHub to reject sessions or flag abuse | Low–Unknown | High | Investigation B resolves this in days 3–4. If triggered: `sessionAffinity: ClientIP` on the proxy Service is the immediate fallback; per-goroutine proxy assignment is the higher-fidelity fix. Both are implementable before Milestone 4. |
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

*To be filled in before closing Milestone 1.*

### 8.A — `AcknowledgeRunnerRequest`

> TBD — document HTTP status, response body, and observed effect of omitting the call.

### 8.B — Egress IP Variance

> TBD — document call-by-call results across two simulated proxy pods and recommended proxy affinity approach.
