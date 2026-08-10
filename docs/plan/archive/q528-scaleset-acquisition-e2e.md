# Q528: A kind e2e for the scale-set acquisition half

The scale-set tier's *recovery* half runs on kind under the chart's real RBAC ([`worker_scaleset_recovery_test.go`](../../../cmd/gmc/test/e2e/worker_scaleset_recovery_test.go), Q519).
Its *acquisition* half — open a message-queue session, take an assignment off the queue, mint a JIT runner config, provision a worker pod from it — has never run on a cluster.
The reason is stated in that spec's own header: the e2e fakegithub speaks only the classic broker protocol, so the scale-set listener's session can never open there and the spec has to stage its worker pod by hand.

This plan closes that: teach the deployed fake-GitHub image the scale-set protocol, then assert the acquisition chain end-to-end on kind.

## What is missing, precisely

Everything below is covered by envtest and by the `scalesettest` unit stub, and by nothing that runs against a real apiserver + kubelet + chart RBAC:

| Step | Client call | Covered on kind today |
|---|---|---|
| Resolve/create the scale set | `GetRunnerScaleSetByName`, `ResolveRunnerGroup`, `CreateRunnerScaleSet` | no |
| Open the message-queue session | `CreateSession` | no |
| Poll, advertising capacity | `GetMessage` (`X-ScaleSetMaxCapacity`) | no |
| Claim an offered job (GHES flow) | `AcquireJobs` | no |
| Mint the runner config | `GenerateJITConfig` | no |
| Provision the worker from the assignment | `ProvisionScaleSetWorker` | no |
| Ack the message | `DeleteMessage` | no |
| Release the session on shutdown | `DeleteSession` | no |

The recovery spec covers the tier's *other* half and deliberately says so.

## Approach

### 1. Split the protocol model out of `scalesettest`

`scaleset/scalesettest` already models the whole protocol — sessions, the cursor-based queue log, auto-assign vs JobAvailable→acquire, claim-once acquisition, JIT minting, the long poll.
It is the model to reuse, not to re-derive; a second hand-written copy inside fakegithub would be ~1,400 lines that drift.

It cannot be imported as-is: it builds an `httptest.Server`, and `TestNoPackageMainReachesHTTPTest` forbids any `package main` in the workspace from linking `net/http/httptest`. fakegithub is `package main`.

So the model moves to **`scaleset/scalesetstub`** — transport-free, exposing an `http.Handler` — and `scalesettest.Server` becomes a thin `httptest` wrapper that embeds it, keeping its exported API intact for the unit and envtest suites.
This is the same split `broker/brokerstub` already makes for the classic protocol, and the compat gate's own error message names it as the pattern.

The one transport-coupled detail is the self-referential URLs the protocol hands back — the admin connection's tenant URL, the session's `messageQueueUrl`, a `JobAvailable`'s `acquireJobUrl`.
In `scalesettest` those are the fixed `httptest` base; in a deployed pod they must follow the request's `Host`.
The stub therefore resolves its base per request through a `BaseURL func(*http.Request) string`, which is what fakegithub's existing `externalBase` already does for the classic protocol.

### 2. Mount it in fakegithub

fakegithub serves the stub's handler at the paths the client uses:

- `/_apis/runtime/…` and `/queue/…` — scale-set only, no collision with the classic routes.
- `/api/v3/actions/runner-registration` and `/api/v3/{orgs,repos}/…/actions/runners/registration-token` — the two REST hops of the bootstrap, prefix-stripped onto the same handler.

The classic `/api/v3/…/actions/runners` routes (generate-jitconfig, list, delete) stay on fakegithub's own runner registry.
The scale-set happy path never touches them — it mints runners through `generatejitconfig` on the Actions Service — so the two registries do not have to agree. `DeregisterRunnerByName`, which does use the REST routes, is only reached on a runner-name 409, which this venue does not produce.

Control endpoints, alongside the existing classic ones:

- `POST /control/scaleset/enqueue?name=<scale set>` — queue a job.
- `POST /control/scaleset/acquireflow?ghes=true|false` — pick auto-assign (dotcom) or JobAvailable→acquire (GHES).
- `GET  /control/scaleset/state?name=<scale set>` — session liveness, assigned count, and the call log, so a spec asserts on what the server saw rather than inferring it from the AGC's logs.

### 3. Point the AGC's scale-set client at it

`buildScaleSetClient` derives both `ConfigURL` and `APIBase` from `gw.Spec.GitHubURL`, and that field is pinned to `^https://` by the CRD pattern *and* by the webhook — so it cannot name plaintext fakegithub.
(This is exactly why the recovery spec's listener never starts: its bootstrap dies on a TLS handshake against the plaintext port, which that spec wants.)

The codebase already solves this for the classic tier: `buildRegistrar` prefers a `StubRegistrar` when `STUB_AUTH_URL` **and** `STUB_BROKER_URL` are both set, a pair that reaches a GMC-provisioned AGC only through `AGC_EXTRA_*` under the testing-only `--allow-agc-extra-env` flag.
The scale-set client follows the same rule and the same signal: with the stub env set, `ConfigURL`/`APIBase` are derived from `STUB_BROKER_URL` instead of the gateway's `githubURL`.
Production AGCs never carry the pair and are unaffected.

No new env var, no new opt-in surface, and the security posture is the one already reviewed for the classic path.

### 4. The spec

`E2E_AGC_ScaleSetAcquisition`, one `Ordered` container, both acquire flows:

1. Apply a v2 object set whose RunnerSet is `acquisitionProtocol: ScaleSet`.
2. Wait for the listener to reach the queue — the RunnerSet's Ready condition, corroborated by fakegithub reporting a live session for the scale set.
3. Enqueue a job; assert a worker pod appears carrying the run identity the assignment delivered, and that the pod is the shape `ProvisionScaleSetWorker` stamps (owner label, acquisition-protocol label, run-id/repository annotations, the JIT config env var).
4. Flip fakegithub to the GHES acquire flow, enqueue again, and assert the claim landed server-side (`acquirejobs` in the call log) before the second worker appears — the rung auto-assign skips entirely.
5. Delete the RunnerSet and assert the session is released rather than leaked.

## Result: the acquisition chain, run on kind 2026-07-31

`E2E_AGC_ScaleSetAcquisition` — 4 specs, green on three consecutive runs against the local kind cluster (`e2e-1e54fbf4` images).

The load-bearing evidence is fakegithub's own ordered call log, attached to the report by the GHES spec:

```
registration-token /orgs/ssacqorg/actions/runners/registration-token
runner-registration
get-scaleset name=e2e-ssacq
create-scaleset name=e2e-ssacq group=1
create-session id=1
poll id=1 cap=10 last=0   (×3)
generatejitconfig id=1
poll id=1 cap=10 last=1
acquirejobs id=1 auth=queue ids=[1002]
poll id=1 cap=10 last=2
generatejitconfig id=1
poll id=1 cap=10 last=3
```

Every rung of the table above, from the deployed AGC, under the chart's real `agc-tenant-role`: both bootstrap hops, scale-set resolve-then-create, the session, capacity-advertising long polls (`cap=10`, the `defaultScaleSetMaxCapacity`), an auto-assigned job provisioned through `generatejitconfig`, then — after the flow switch — a `JobAvailable` claimed with `acquirejobs` **authorized by the queue token, not the admin JWT**, and only then its own JIT mint.
Two worker pods, each carrying the run identity its assignment delivered.

**Why the call log is in the spec and not just in this doc.** The whole chain runs in-cluster against a stub whose long polls wake the instant a job is enqueued, so the GHES spec completes in ~34 ms end to end.
At that resolution a genuine pass and a vacuous one — assertions satisfied by state that predates the enqueue — are indistinguishable from spec timings, which is exactly what the first green run looked like before the log was captured.
The log is diagnostics, not a gate; the assertions remain the gate.

**Two things the run did not exercise**, recorded so neither is read into the result. `ResolveRunnerGroup` is absent from the log — the listener carried no group name, so the scale set was created against group 1 directly.
And no runner ever starts: fakegithub's JIT config is a syntactic placeholder, so the spec's terminal assertion is the staged JIT-config Secret the worker pod mounts, not a pod phase.
A real runner executing a real job stays with the live-GitHub tier.

## Status

**Done.** fakegithub serves the scale-set protocol from the shared `scaleset/scalesetstub` model, the AGC's bootstrap is re-pointed at it under the existing `STUB_AUTH_URL`/`STUB_BROKER_URL` pair, and the acquisition chain is gated on kind by `E2E_AGC_ScaleSetAcquisition`.
