# Q264 — Migrate AGC acquisition to the runner-scale-set protocol (Option E feasibility spike)

> **Archived phase narrative (P0–P5).** This is the full spike/probe/decision/phase record for the scale-set migration, kept for its rationale.
> The migration is **shipped** — ScaleSet is the default acquisition protocol since v1.1.0 and Classic is deprecated.
> The one remaining **open residual** — the terminal PR that removes the classic acquisition machinery and the transitional API fields — is tracked in the live doc [q264-scale-set-protocol.md](../q264-scale-set-protocol.md), not here.

**Status:** SHIPPED (P0–P5), archived.
The Option E rewrite is committed.
Phases **P0 (spike)**, **P1 (live wire probes**, Investigations E and E2, §2a/§2b — `cmd/probe` scenarios, run 2026-07-04**)**, **P2 (the `scaleset/` client package + its `scalesettest` fake, §6)**, **P3 (AGC wiring behind the `acquisitionProtocol` field)**, and **P4 (live dogfood validation — clean-green achieved 2026-07-05, Q224 CLOSED) are DONE** — sub-PRs (a) (API field + CEL + GMC webhook), (b) (the standalone `scalesetlistener` engine + the fan-out-free acceptance twin), (c) (the worker `run.sh --jitconfig` mode + provisioner JIT staging), and (d) (the RunnerSet-controller wiring that makes `ScaleSet` live behind the field + its lifecycle envtest) are landed, **all default `Classic`** so nothing changes for existing users until P5.
**P4 (live dogfood validation) is DONE — the Q224 fan-out distinct-delivery starvation is ELIMINATED by construction and, on the 2026-07-05 clean-green re-run, the whole CI matrix went ACTUALLY all-green.
The single-acquirer listener assigned, ran, and terminally concluded ALL 7 distinct jobs (vs classic's 2/7 with 5 starved forever), zero dedup/collision.
First P4 pass (2026-07-05, sha `4ea41f6`) left 4 of 7 CI jobs non-green for reasons ORTHOGONAL to acquisition — a self-referential `WORKER_MODE` test-env leak (Q269) and node CPU capacity; both were fixed and the RE-RUN on `main`@`2025557` (Q269 #542 + Q270 #544) landed **all 7 GAG jobs GREEN** on a fresh scale set (`gag-scaleset3`, scaleSetID 5) with 0 dedup/wedge, `unit-test`+`coverage` green (WORKER_MODE fix holds) and `lint`/`integration-test` green on 6-node CPU headroom.
Q224 is CLOSED.
**P5 default-flip is DONE (2026-07-06): the v2alpha1 `acquisitionProtocol` default is now `ScaleSet` and `Classic` is deprecated; the one-minor-release deprecation window + the classic-machinery removal are the remaining P5 residual (§6-P5).** Full §6-P4.** A Classic RunnerSet's acquisition path is **byte-for-byte unchanged**.
Every protocol-level unknown is probed; the residuals are integration-level (P4).
This is [Option E in the Q260 design](q260-fanout-completion-reconciliation.md#option-e--single-acquirer-topology--adopt-the-runner-scale-set-protocol-treat-the-cause): the deferred fallback pursued **only if live re-route #5 rules Option A (winner fan-out completion) infeasible**.
The go/no-go stays with re-route #5; this doc exists so that fork is not started cold.

**Verdict up front: Option E is viable — and materially cheaper than the Q260 doc priced it.** The live probes (§2a/§2b) then **strengthened** the verdict: GAG's own GitHub App drove the full protocol end-to-end at both org and repo scope; on the current broker-host backend GitHub **auto-assigns** each job to the scale set (no client acquire call at all — strictly and dynamically gated by the `X-ScaleSetMaxCapacity` poll header), assignment is 1:1 by construction; the message stream replays to a recreated session (restart-safe recovery with no local state); and a real `actions-runner` container started from a probe-minted JIT config picked its job up in ~2 s and completed it, with the terminal `JobCompleted{result}` delivered back to the listener — a signal the classic protocol never gives the AGC.
GitHub has published [`actions/scaleset`](https://github.com/actions/scaleset) (Public Preview), an **official standalone Go client + listener package for this exact protocol**, "so that platform teams, integrators, and infrastructure providers can build their own custom autoscaling solutions."
The protocol is no longer undocumented-internal-to-ARC: the Go source is a supported reference (though still no wire-spec document).
No security property is forfeited — egress isolation, the App-token-never-in-the-worker property, and on-demand workers all carry over.
The honest costs: the worker handoff contract must change (forced — see §2.4), `runs-on` matching collapses to a single label per group, GHES support floor rises to 3.9, enterprise-scope registration is PAT-only (GAG is App-based), and each group's acquisition becomes a single session (inherent — it is *why* the fan-out disappears).
Details in §4; load-bearing unknowns a spike cannot settle from source alone in §5; phased path in §6.

---

## 1. Why this spike exists

Q260 established (live, re-routes #2/#4) that GAG's fan-out race is intrinsic to its **many-acquirers topology** on GitHub's classic per-runner broker protocol: concurrency = registered runners = acquirers, so GitHub fans one logical job out to N sibling sessions, all acquire it (shared planID), and N−1 assignments dangle on GitHub's books until the unstarted-job timeout cancels the whole job ([Q260 §1–§2](q260-fanout-completion-reconciliation.md#1-the-protocol-from-the-code-and-the-live-evidence)).
Option A reconciles the accounting AGC-side; Option E removes the race **by construction** by adopting the protocol modern ARC uses: one listener per scale set acquires each job exactly once via a batch message-queue claim, then a dedicated ephemeral runner executes it — 1 job : 1 queue entry : 1 acquirer : 1 runner, no sibling deliveries to reconcile.

The pitch is unchanged from the Q260 doc: **"ARC's protocol, GAG's efficiency"** — a Go listener goroutine per group instead of ARC's ~256 MiB .NET listener pod per scale set, keeping per-tenant egress isolation and on-demand worker pods.

## 2. The runner-scale-set protocol, documented

Reverse-engineered 2026-07-04 from three mutually consistent public sources (§8): GitHub's official **`actions/scaleset`** Go client, the ARC client at tag `gha-runner-scale-set-0.10.1` (`github/actions/client.go` — on `master` this package is now a stub; the client moved to `actions/scaleset`), and the runner's C# (`src/Runner.Common/RunnerDotcomServer.cs`, `BrokerServer.cs`).
Source-level confidence is high; wire-level details still need a live probe (§5).

### 2.1 Registration: one scale set + per-job JIT configs, not N agents

Auth bootstraps in two hops, then pivots off the public REST API entirely:

1. **Registration token** — `POST /orgs/{org}/actions/runners/registration-token` (or `/repos/{owner}/{repo}/...`, `/enterprises/{ent}/...`) with a PAT or App installation token → short-lived `{token, expires_at}`.
   Same call GAG's classic registrar family already uses.
2. **Admin connection** — `POST {github}/actions/runner-registration` with the nonstandard header `Authorization: RemoteAuth <registration token>` and body `{"url": <configURL>, "runner_event": "register"}` → returns the runtime-discovered **Actions Service tenant URL** (e.g.
   `https://pipelines.actions.githubusercontent.com/<tenant>`) and an **admin JWT** (~1 h; ARC parses `exp` and refreshes 60 s before expiry — `updateTokenIfNeeded`, ARC `client.go:1246`).

Everything else targets `{actionsServiceURL}/_apis/runtime/...` with `Authorization: Bearer <admin JWT>` and `api-version=6.0-preview` (Azure-DevOps-style `{count, value}` envelopes):

| Call | Endpoint |
|---|---|
| Create scale set | `POST /_apis/runtime/runnerscalesets` (body `RunnerScaleSet{name, runnerGroupId, labels, runnerSetting{ephemeral,…}}`) |
| Get / update / delete | `GET`/`PATCH`/`DELETE /_apis/runtime/runnerscalesets/{id}` |
| Resolve runner group | `GET /_apis/runtime/runnergroups/?groupName={name}` |
| **Per-job JIT config** | `POST /_apis/runtime/runnerscalesets/{id}/generatejitconfig` (body `{name, workFolder}`) → `{runner{id,name}, encodedJITConfig}` |

Contrast with classic: instead of pre-registering `maxListeners` agents per group (`generate-jitconfig` per agent, [`github_registrar.go:89`](../../../cmd/agc/internal/agentpool/github_registrar.go)) and recycling each single-use agent after every job (Q114), the controller registers **one scale-set object per group once**, then mints **one JIT config per acquired job** for the runner that will execute it.
The server pre-registers the runner and returns a base64 blob bundling `.runner` + `.credentials` (+ RSA parameters); the runner consumes it with `run.sh --jitconfig <blob>`.
The **scale set's `name` is its single `runs-on` label** — there is no free-form label list per scale set.

### 2.2 Session + message queue: one authoritative stream per scale set

- `POST /_apis/runtime/runnerscalesets/{id}/sessions` (body `{"ownerName": <listener identity>}`) → `RunnerScaleSetSession{sessionId, messageQueueUrl, messageQueueAccessToken, statistics}`.
  **One active session per scale set** — a second create conflicts until the first is deleted or expires.
  `PATCH .../sessions/{id}` refreshes the queue token (ARC triggers it on a 401 from the queue); `DELETE` on shutdown.
- Long-poll: `GET {messageQueueUrl}?lastMessageId={N}` with `Authorization: Bearer <messageQueueAccessToken>` and header `X-ScaleSetMaxCapacity: <maxRunners>`.
  `200` → one `RunnerScaleSetMessage`; `202` → empty, poll again (server holds ~50 s).
- A message carries `messageId`, batched typed bodies — `JobAvailable`, `JobAssigned`, `JobStarted`, `JobCompleted` — and a `RunnerScaleSetStatistic{totalAvailableJobs, totalAcquiredJobs, totalAssignedJobs, totalRunningJobs, totalRegisteredRunners, totalBusyRunners, totalIdleRunners}` snapshot.
  The `actions/scaleset` README is explicit: scale on `statistics.TotalAssignedJobs`, not by counting individual messages — the ARC listener's exact formula is `clamp(statistics.TotalAssignedJobs, minRunners, maxRunners)` (`autoScalerService.go` `scaleForAssignedJobCount`), i.e. the assigned-but-not-completed count is **server-authoritative**, read off every envelope rather than reconstructed client-side.
- Ack = advance `lastMessageId` on the next poll **and** `DELETE {messageQueueUrl}/{messageId}` (the official listener does both).
  Unacked messages replay after a session re-create — the queue, not the listener's memory, is the recovery source of truth.

### 2.3 Batch acquisition — the call that kills the fan-out

> **Live caveat (§2a-3):** on the current broker-host backend the explicit `acquirejobs` call below **does not exist** (404) — GitHub auto-assigns jobs to the scale set up to the polled `X-ScaleSetMaxCapacity`, delivering `JobAssigned` directly.
> The subsection stands as the documented ARC-era flow (and possibly the `pipelines.*` behaviour — U2′); either way acquisition is single-stream and 1:1.

```
POST /_apis/runtime/runnerscalesets/{id}/acquirejobs?api-version=6.0-preview
Authorization: Bearer <messageQueueAccessToken>     ← queue token, not admin JWT
Body:     [10234, 10235, 10241]                     ← runnerRequestIds from JobAvailable
Response: {"count": 2, "value": [10234, 10241]}     ← the subset actually acquired
```

One listener claims an arbitrary **batch** in one transactional call; the response is the authoritative list of wins.
`JobAssigned` messages then confirm each assignment.
Because each job is enqueued **once** in the scale set's **single** serialized queue and claimed by its **single** session, there are no sibling deliveries, no per-delivery completion fan-out, and nothing to reconcile — the entire Q260/Q247-completion/Q259-recycle class cannot occur.
The N racing consumers of the classic protocol become 1 listener that demultiplexes into N runners *after* acquisition.

### 2.4 Job payload and per-job data plane: delivered to the runner, not the listener

**`acquirejobs` returns only ids — the listener never sees the job payload.** The pipeline job message (`AgentJobRequestMessage`, including the `SystemVssConnection` endpoint and its job-scoped OAuth token) is delivered to the **ephemeral runner itself** after it boots with its JIT config and opens its *own* broker session (`BrokerServer.cs`: `CreateSessionAsync` → `GetRunnerMessageAsync` → `AcknowledgeRunnerRequestAsync` → `Runner.Worker` → `DeleteSessionAsync`).
The runner renews its own lock and reports its own completion.
The listener is purely a **control-plane accountant** (claim + count); the data plane is runner ↔ service directly.
Security consequence ARC documents and GAG inherits: the App/PAT token is never passed to the runner pod.

This forces a GAG worker-contract change: today's worker runs `Runner.Worker` directly via the M3 spawnclient/pipes handoff with the payload the AGC already acquired ([`cmd/worker/main.go`](../../../cmd/worker/main.go)).
Under the scale-set protocol there is no payload to hand off — the worker pod must run the **full runner** (`run.sh --jitconfig <blob>`, which the default `ghcr.io/actions/actions-runner` image already supports) and pull its one job through its own session.
The entrypoint wrapper survives for proxy-CA trust setup; the pipes handoff and the AGC-side renew loop do not.

### 2.5 Token matrix

| Token | Minted by | Authorizes | Lifecycle GAG must manage |
|---|---|---|---|
| App installation token | existing `githubapp` package | registration-token endpoint | already built |
| Registration token | REST registration-token call | the `runner-registration` RemoteAuth hop | short-lived, per bootstrap |
| **Admin JWT** | `runner-registration` response | scale-set CRUD, sessions, `generatejitconfig` | ~1 h; refresh pre-expiry (new) |
| **Queue access token** | session create/refresh | queue long-poll **and `acquirejobs`** | refresh via session PATCH on 401 (new) |
| JIT config | `generatejitconfig` | the runner pod's own session | one-shot, per job (replaces per-agent creds) |
| Per-job SystemVssConnection token | inside the job message **to the runner** | renew/complete, logs, artifacts | moves out of the AGC entirely |

## 2a. Investigation E — live wire probe (2026-07-04)

`PROBE_SCALESET_TEST=true` in [`cmd/probe`](../../../cmd/probe/scaleset.go) runs the full chain against real GitHub with only App credentials — registration-token → RemoteAuth hop → runner-group lookup → throwaway scale set → session → queue long-poll → acquire-shape probes → `generatejitconfig` → full cleanup.
`PROBE_SCALESET_JOB_TEST=true` additionally waits for a real job (queued by the dispatch-only [`scaleset-probe.yml`](../../../.github/workflows/scaleset-probe.yml) fixture, `runs-on: gag-probe-scaleset`).
Runbook: export `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_APP_INSTALLATION_ID`, `GITHUB_ORG_URL`; logs prefix `INVESTIGATION-E:`; tokens and the JIT blob are never logged.

Findings (all live, `actions-gateway` org / `github-actions-gateway` repo):

1. **U1 resolved — the auth chain works with GAG's App, at org AND repo scope.** Registration token (132 chars) → `POST /actions/runner-registration` with `Authorization: RemoteAuth …` → 200 with tenant URL + admin JWT (~1 KiB).
   Scale-set create / session / `generatejitconfig` / deletes all succeed (200/204).
   No extra App permissions were needed beyond what the dogfood App already has.
2. **The tenant is the broker host, not `pipelines.*`.** The admin connection returned `actionsServiceURL = https://broker.actions.githubusercontent.com/rest` — a newer backend than the ARC-era sources describe.
   The session's queue URL is `{broker}/scalesets/message` (no query params; the queue token is the identity).
3. **The backend AUTO-ASSIGNS jobs — no acquire call exists.** The headline deviation from §2.3: `POST …/acquirejobs` returns a router-level `404 page not found` at every plausible location (Actions Service path with queue token — the official client's exact construction — admin token, and the `/scalesets/…` queue-base route), while sibling route `GET …/acquirablejobs` exists (204 when empty).
   With a job queued on the scale set's label, the queue delivered **`JobAssigned` directly ~1 s after session creation** — no `JobAvailable`, no client claim, `runnerRequestId: 0`, a `jobId` UUID, `scaleSetAssignTime` stamped, `statistics.totalAssignedJobs: 1`.
   Admission control is the **`X-ScaleSetMaxCapacity` header** the poll advertises (the probe sent `1`).
   Open sub-question (§5 U2′): whether `JobAvailable` + an acquire step reappears when queued jobs exceed the advertised capacity, and whether `pipelines.*` tenants still serve the ARC-documented explicit-acquire flow.
4. **Delivery is cursor-based, at-least-once.** An unacked message is redelivered with the same `messageId` (100000001) on the next `lastMessageId=0` poll.
   Empty long-poll: held ~51 s → `202`, no body.
5. **U4 partial — no rate-limit headers on the queue.** Only `X-GitHub-Request-Id`/`X-Github-Backend` observed; no `X-RateLimit-*`, no `Retry-After`.
   Steady-state cost is one ~50 s long-poll per scale set.
6. **Runner-group policy gates org-scoped scale sets — a real operational constraint.** With the scale set in the org's `Default` group (`allows_public_repositories: false`, the GitHub default), a job from this **public** repo was never routed — three-minute windows expired twice with `totalAvailableJobs: 0`.
   Registering the scale set **repo-scoped** (config URL = the repo) bypasses runner groups entirely and the pre-queued job delivered instantly.
   GAG must document this per-scope behaviour for tenants on public repos (mirrors the classic dogfood setup, which is repo-scoped).
7. **JIT config shape confirmed**: base64 blob (~4 KiB) decoding to top-level keys `.runner`, `.credentials`, `.credentials_rsaparams` — the same credential family the classic registrar parses today.

**Design consequence for GAG:** the listener tier gets *simpler* than §3 sketched.
On this backend the admission gate (Q59) is literally the `X-ScaleSetMaxCapacity` header — advertise free worker slots and GitHub assigns at most that many jobs, exactly once each; there is no batch-claim bookkeeping at all.
The `JobAssigned` statistics count is the authoritative provision-target, matching the ARC `clamp()` model (§2.2).

## 2b. Investigation E2 — capacity gating, recovery, and a real runner (2026-07-04)

Second live round (`PROBE_SCALESET_CAPACITY_TEST=true` + two pre-queued jobs from the fixture, then two locally docker-run `ghcr.io/actions/actions-runner` containers registered with probe-minted JIT configs; `PROBE_SCALESET_JITCONFIG_FILES` + `PROBE_SCALESET_HOLD_SECONDS` keep the scale set alive while they work).
Logs prefix `INVESTIGATION-E2:`.

1. **U2′ resolved — assignment is strictly, dynamically capacity-gated, and there is no overflow `JobAvailable`/acquire flow on this backend.** With two jobs queued: a capacity-**0** poll long-held then returned **202** — jobs simply wait server-side, nothing is offered.
   The next poll at capacity-**1** delivered exactly **one** `JobAssigned` (`totalAssignedJobs: 1`); the follow-up at capacity-**2** delivered the second (`totalAssignedJobs: 2`).
   Capacity is re-evaluated **per poll**, so a GAG listener can widen/narrow its advertised capacity every poll cycle — the Q59 gate maps 1:1 with per-cycle granularity.
2. **Session token refresh works as documented**: `PATCH …/sessions/{id}` → 200, same `sessionId`, **new** `messageQueueAccessToken`.
3. **Recovery-by-recreate confirmed (the §3 claim)**: after `DELETE` + re-`POST` of the session, a `lastMessageId=0` poll on the **fresh** session **replayed** the earlier `JobAssigned` message (same `messageId`) — the message stream is scale-set-scoped, not session-scoped.
   An AGC restart re-reads assigned-but-unprovisioned jobs from the queue; no in-memory registry to lose.
4. **U3 core resolved — a real ephemeral runner works against a probe-minted JIT config.** Two `actions-runner` containers started with `run.sh --jitconfig <blob>`: both connected and picked up a job **~2 s** after start; the fast job ran to completion and its runner **exited 0 and deregistered itself** (single-use lifecycle end-to-end).
   The scale-set queue meanwhile streamed the lifecycle telemetry: `JobStarted` messages with `runnerName`, and statistics transitions (`totalRegisteredRunners: 2`, `totalRunningJobs: 1`, `totalAssignedJobs` 2→1 as the fast job concluded) — completion accounting is fully observable from the listener session.
   *Residual for P4:* the same flow behind the per-tenant egress proxy with the proxy-CA trust bundle, and job-start latency under pod (not local-docker) conditions.
5. **U5 core measured — killed-runner cancel latency ≈ 9.5 minutes.** `docker kill` (SIGKILL) on the runner mid-job at 16:19:06Z; GitHub concluded the job `failure` at 16:28:40Z (annotation: *"The self-hosted runner lost communication with the server"* — coinciding with the job's 10-minute `timeout-minutes` boundary).
   Same order as the classic protocol's ~10-minute lock-TTL lapse: Option E neither gains nor loses on dead-runner detection, and GAG's AGC-side pod-death → rerun-API fast path remains the differentiator to port.
6. **Terminal results are delivered to the listener.** The hold loop received `JobCompleted` with `result: "failed"`, the `runnerId`/`runnerName` that held the job, and `finishTime`, statistics zeroing in the same message — the authoritative signal the ported eviction-retry (and job-duration metrics) can key off, something the classic AGC never gets (§1 of the Q260 doc: the JobHandler never learns the real result).
7. **Admin JWT TTL is short — observed expiry within ~17 minutes.** The connection minted at run start 401-ed (`InvalidTokenException`) the cleanup deletes after the 15-minute hold.
   ARC's parse-`exp`-and-refresh (60 s pre-expiry) is **mandatory** client behaviour, not defensive polish.
   The probe now re-mints the connection after holds, and a `PROBE_SCALESET_CLEANUP=true` mode deletes a leaked scale set by name.

**Net effect on §4 costs:** none added — E2 only removed unknowns.
The capacity-header admission model (1) plus queue replay (3) simplify the target listener design further: no acquire plumbing, no persistent local state — and (6) actually *removes* a classic-protocol limitation.

## 3. Delta from today's classic machinery

What Option E discards, reworks, carries over, and improves — against the concrete code surface:

### Discarded (the classic-protocol acquisition tier)

| Today | Where | Why it goes |
|---|---|---|
| Classic broker client: `CreateSession`/`GetMessage`/`AcquireJob`/`RenewJob`/`CompleteJob`/`DeleteSession` | [`broker/client.go`](../../../broker/client.go) | replaced by the scale-set client — plausibly **vendored `actions/scaleset`** rather than reimplemented (§5 decision) |
| Agent pool: N pre-registered JIT agents, per-agent Secrets, single-use recycle + heal ladder (Q114) | [`cmd/agc/internal/agentpool/`](../../../cmd/agc/internal/agentpool/pool.go) | no per-agent registration exists; one scale-set object + per-job JIT configs |
| Multiplexer: `maxListeners` sessions, `SpawnReplacement`, poller accounting (Q152), planID `claimJob` dedup (#512) | [`multiplexer.go`](../../../cmd/agc/internal/listener/multiplexer.go) | one session per group; nothing to dedup |
| Listener goroutine: per-delivery `handleJob`, self-heal ladder, `StartRenewLoop` (Q247), `completeAbandonedDelivery` (#513) | [`goroutine.go`](../../../cmd/agc/internal/listener/goroutine.go) | acquisition is batch-claim + message dispatch; the runner renews/completes its own job |
| M3 pipes handoff: payload Secret → wrapper → `Runner.Worker spawnclient` | [`cmd/worker/main.go`](../../../cmd/worker/main.go), provisioner payload staging | no payload at the listener; worker runs `run.sh --jitconfig` (§2.4) |
| Q260 fan-out accounting model + tests | [`broker/brokertest/server.go`](../../../broker/brokertest/server.go) | models a race class that no longer exists; a new scale-set fake replaces it |

### Reworked

- **Admission gate (Q59) — gets *simpler and stronger*.** Today the gate skips `acquirejob` per delivery and the provisioner backstops post-acquire.
  Under scale-set, capacity gating is *the batch size*: acquire only as many `runnerRequestId`s as there are free worker slots; unacquired jobs stay queued at GitHub with zero AGC-side state, and `X-ScaleSetMaxCapacity` advertises the ceiling upstream.
  `priorityTiers` re-expresses as which-tier-the-next-pod-gets at provision time — unchanged mechanics, driven by `JobAssigned` count instead of per-delivery arrival.
- **Registration/auth:** the `GithubRegistrar` family shrinks to the two-hop bootstrap + scale-set CRUD + per-job `generatejitconfig`; the token manager gains two new lifecycles (admin JWT, queue token).
- **CRD surface:** `maxListeners` loses its meaning (acquisition concurrency is the batch, not a session count); `runnerLabels` collapses to the scale-set name (see §4 cost).
  A per-group protocol selector (or v2-only adoption) is the migration lever — §6.
- **Eviction retry:** detection stays pod-phase-based and the rerun-API path carries over; the "stop renewal to fast-cancel" half becomes "the runner process died with the pod, GitHub's lock lapses on its own" — the fast-cancel latency is now GitHub's lock TTL, not GAG's choice (probe item, §5).

### Carried over intact

Provisioner pod-building (pod template merge, security invariants, quota retry, reaper, `completedPodTTL`/`pendingPodDeadline`), the priority-tier ceilings, the egress proxy path (the listener's Actions Service traffic and the worker's session both route through the per-tenant proxy exactly as today), the `githubapp` token provider, the GMC tier, and the worker image (already the ARC default `ghcr.io/actions/actions-runner`, which ships `run.sh --jitconfig`).
The JIT-credential-in-worker-Secret surface is **not new** — the provisioner already stages `encoded_jit_config` into the worker Secret today ([`provisioner.go:150`](../../../cmd/agc/internal/provisioner/provisioner.go)).

### Improves

- **The Q260/Q247-completion/Q259 race class is gone by construction** — no dedup key, no sibling completion, no recycle-422 seam.
- **Density and rate-limit budget at rest and under burst:** one queue session per group (~72 polls/h) at *all* load levels, vs classic's burst climb toward `maxListeners` sessions × 72/h.
  The [§3.5 rate-limit ceiling](../../design/03-api-contracts.md#35-github-api-rate-limit-budget) stops scaling with acquisition concurrency.
- **No agent-recycle churn:** the Q114 recycle (2 REST calls + Secret rewrite + session re-create per completed job) disappears; per-job cost becomes one `generatejitconfig` call.
- **Recovery model:** unacked queue messages replay on session re-create; `statistics` give an authoritative assigned count to reconverge on after an AGC restart — strictly better than classic's in-memory session registry.

## 4. Honest cost list (delta vs the Q260 §4E estimate)

The Q260 doc priced Option E as "reverse-engineering and depending on a second GitHub-internal protocol."
**That cost has materially dropped**: the protocol now has an official, supported Go client (`actions/scaleset`, Public Preview, MIT-licensed) that may be directly vendorable.
The remaining real costs:

1. **Worker handoff rework (forced, was not in the Q260 cost list).** The M3 pipes/`Runner.Worker` handoff cannot survive — the payload never reaches the listener (§2.4).
   The worker becomes a full runner with a JIT config.
   Consequences: the wrapper keeps only its proxy-CA duty; per-job Secrets carry a JIT blob instead of a payload; job-start latency adds one session-create + message round-trip from inside the pod (probe item §5).
2. **`runs-on` label regression.** A scale set matches `runs-on: <scale-set-name>` only.
   GAG's `runnerLabels []string` (multi-label matching) cannot be expressed; migration needs a one-label-per-group story and tenant comms.
   This is a *user-visible* API change, not just internals.
3. **Single acquisition session per group (inherent).** The Q260 doc's SPOF concern stands, softened: the AGC is already `replicas: 1`, so process-level SPOF is the status quo; what is lost is N independent sessions' redundancy *within* the process (Q137 revival per listener).
   Recovery becomes session re-create + queue replay, which is well-defined (§3 Improves).
   Hot-hot listener HA is impossible by protocol design — that constraint *is* the fan-out fix.
4. **Auth/registration scope limits.** Org-scope App permissions: Organization "Self-hosted runners: RW" (GAG's App model fits).
   Repo-scope needs Repository "Administration: RW" — **broader than today's repo-scope JIT registration**; verify per install.
   **Enterprise scope is PAT-only** (no App auth) — GAG is App-based, so enterprise-scope gateways would be unsupported or need a PAT credential mode (decision, §5).
5. **GHES floor rises to 3.9** (scale-set support begins there).
   Classic machinery would need to survive for older GHES, or the floor is documented.
6. **Public Preview instability.** GitHub says interfaces "may change."
   GAG would pin a vendored version; wire changes are a live risk shared with ARC (mitigation: GitHub now treats third-party listeners as a supported audience, which is the opposite of the classic protocol's posture).
7. **Identity shift (unchanged from Q260 §4E).** "Thousands of goroutine-backed virtual runners" retires; the story becomes "a lighter-weight ARC listener with GAG's isolation + scheduling."
   Density at rest actually improves; the marketing/positioning docs (why-gag, vs-ARC) need a rewrite in the same PR that flips the default.

**Security check (secure-by-default):** no property regresses.
Egress isolation holds (all new endpoints are GitHub-hosted; both listener and worker traffic stay behind the proxy).
Workers still never see the App token.
The JIT credential in the worker Secret is today's surface, unchanged.
The admission gate strengthens (§3 Reworked).

## 5. Load-bearing unknowns

> **AGC-side escape-hatch checked — none found (2026-07-05).** Before committing to this rewrite, the two proposed classic-protocol levers for re-route #8's fan-out distinct-delivery starvation were tested against the mechanism and the live evidence in a dedicated spike ([`q224-fanout-dispatch-lever-spike.md`](../q224-fanout-dispatch-lever-spike.md)).
> Verdict: **no reliable AGC-side lever.** Unique/ephemeral runner names are a non-lever (they add no distinct idle sessions and the #8 orphaning is runner-id churn, not name reuse); a warm idle **listener** baseline (distinct from Q261's warm *worker* pods) is at best a probabilistic green-*rate* stopgap whose very efficacy is unconfirmed and whose favourable case converges on reimplementing this scale-set capacity model on the fan-out-prone protocol.
> **Option E remains the only structural fix** — the spike *strengthens* the case here without force-triggering it.
> A live classic-dispatch probe that would settle the one residual GitHub-side unknown (warm-wide classic: spread vs fan-out) is designed and ready in that doc's §5 but was not run, as its outcome does not move this go/no-go.

Each is marked **probe** (a `cmd/probe` scenario answers it live), **decision** (the user/design owns it), or **✅ probed** (settled by Investigation E, §2a).

| # | Unknown | Kind |
|---|---|---|
| U1 | Full auth chain with **GAG's GitHub App**, org and repo scope. | **✅ probed** — works at both scopes with the existing App permissions (§2a-1). New constraint found instead: org-scope routing is gated by runner-group policy (`allows_public_repositories`) — repo scope bypasses it (§2a-6). |
| U2 | Wire details: 202 semantics + poll cadence, `X-ScaleSetMaxCapacity` effect, `acquirejobs` responses, message replay. | **✅ probed** — 202 after ~51 s hold; cursor-based at-least-once redelivery; and the headline: the broker-host backend **auto-assigns** (JobAssigned direct, no acquire call — every acquire route 404s), gated by `X-ScaleSetMaxCapacity` (§2a-3/4). |
| U2′ | Does `JobAvailable` + an explicit acquire step reappear when queued jobs exceed the advertised capacity? | **✅ probed** — no: jobs above capacity are simply **held server-side** (capacity-0 poll → 202 with two jobs queued); each capacity increment releases exactly one `JobAssigned`, re-evaluated per poll (§2b-1). Session refresh (PATCH) and delete/recreate replay also confirmed (§2b-2/3). Residual: whether `pipelines.*` tenants still serve the ARC-era explicit-acquire flow — version skew a GAG client should tolerate by preferring message-delivered URLs. |
| U3 | Does an ephemeral runner started with `run.sh --jitconfig` receive its job and complete it? | **✅ core probed** — yes: two real `actions-runner` containers registered via probe-minted JIT configs, picked up jobs ~2 s after start; the fast job completed `success` in 5 s and its runner exited 0 (§2b-4). **P4 (2026-07-05): pod-conditions core SETTLED** — a real `run.sh --jitconfig` worker pod pulled its job, ran it, and its runner reported its own true terminal result (§6-P4). **Residual:** the egress-proxy (proxy-CA trust) sub-part, not exercised (P4 tenant was direct-egress). |
| U4 | Rate limits on the Actions Service tenant. | **partially probed** — no rate-limit headers on the queue at all (§2a-5); sustained-load behaviour unknown until P4. |
| U5 | Eviction fast-cancel: how quickly does GitHub conclude a job whose runner died? | **✅ core probed** — ≈9.5 min to `failure` ("runner lost communication"), same order as the classic lock-TTL lapse (§2b-5). The listener receives the terminal `JobCompleted{result, runnerName}` on its queue (§2b-6), so the ported eviction-retry keys off that signal; the rerun-API call itself is unchanged AGC code. **P4 (2026-07-05): NOT tested** — no mid-job pod eviction was performed (§6-P4); under-pod-eviction stays a follow-up. |
| U6 | Vendor `actions/scaleset` vs reimplement in `broker/`-style? | **✅ decided (§5a)** — GAG-owned client in a new leaf `scaleset/` module, tracking upstream as the reference spec; revisit at upstream GA. |
| U7 | Migration surface for the protocol selector. | **✅ decided (§5a)** — v2alpha1 `RunnerSet.spec.acquisitionProtocol: Classic\|ScaleSet`, default `Classic`, immutable, CEL `ScaleSet ⇒ one runnerLabel`; v1alpha1 stays classic-only. |
| U8 | Enterprise scope, GHES floor, org-scope group policy, classic retirement. | **✅ decided (§5a)** — enterprise: out of scope (non-regression); GHES floor moot but keep the acquire-on-`JobAvailable` path (GHES requires it); prefer repo scope + document org-scope group policy; classic: coexist P3–P4 → default flip P5 → one-minor-release deprecation → remove. |

## 5a. The three decisions — analysis and recommendations (2026-07-04)

Researched exhaustively (upstream `actions/scaleset` inspected at HEAD `1b6da87`; ARC master `go.mod`; GHES release lifecycle; GAG's v2 API, scope support, and dependency conventions).
Each subsection: options, the facts that discriminate, a recommendation.
**All three recommendations were signed off 2026-07-04 — these are now the decisions of record.**

### U6 — wire client: vendor `actions/scaleset` vs GAG-owned implementation

**Upstream facts** (`github.com/actions/scaleset`, MIT, Public Preview):

- Lean: the library packages import only stdlib + `golang-jwt/jwt/v4` + `go-retryablehttp` + `google/uuid` (~2.3 k non-test lines; the scary dependency block is tool-directive/example-only and never vendors).
  Go floor 1.26.3 — GAG is on 1.26.4, compatible.
- ARC dogfoods it (ARC master requires `scaleset v0.4.0`); maintained by the ARC maintainers; 4 releases (v0.1.0–v0.4.0, Feb–May 2026), all v0.x with the README caveat "interfaces … may change."
- Exposes exactly the raw primitives GAG needs (session, `GetMessage`, `DeleteMessage`, `AcquireJobs`, `GenerateJitRunnerConfig`, `RemoveRunner`); the `listener/` package is a clean optional library (a `Scaler` interface — `HandleJobStarted/JobCompleted/HandleDesiredRunnerCount`), not ARC-wired.
- Handles the auth chain internally, **including the admin-JWT `exp`-refresh our probe proved mandatory (§2b-7)** and the session refresh-once-on-401.
- **Auto-assign compatible**: its listener calls `AcquireJobs` only when `JobAvailable` messages arrive — zero on the auto-assign backend — and handles `JobStarted`/`JobCompleted` with no prior-acquire bookkeeping.
  But the auto-assign contract is *undocumented*: upstream issue #107 reports exactly our §2a-3 observation and has had **no maintainer response in a month**.
- Frictions: a single mutex serializes every `Client` call (#104 — mitigable with one `Client` per RunnerSet); transport injection type-asserts `*http.Transport` (GAG's proxy-patched `http.DefaultTransport` *is* one, so compatible, but it forbids non-standard RoundTrippers); `go-retryablehttp` adds a retry layer GAG normally owns explicitly; a shipped API typo (`WithRetryableHTTPClint`) signals pre-1.0 roughness.
- **Breakage precedent**: upstream removed the job-acquire flow, which **silently broke GHES** (issue #75), restored in v0.3.0 (PR #90).
  One real breaking misstep in four releases — and on precisely the GHES path GAG supports.

**Options:**

| | Option | Assessment |
|---|---|---|
| A | Vendor; use client **and** `listener/` directly | Maximum reuse, but the listener's at-most-once delete-before-handle and abort-on-any-error semantics don't match GAG's multiplexer/backoff conventions; GAG would fight the loop it adopted. |
| B | Vendor the client; GAG-owned loop behind a wrapper interface | Real value: maintained JWT/session refresh, GHES URL derivation, typed errors. Cost: every v0.x bump may break the wrapper (precedent exists); auth duplicates `githubapp`; the backend GAG actually talks to is where upstream is silent. |
| C | **GAG-owned client in a new leaf module (`scaleset/`, `broker/`-style), mirroring upstream types for wire parity** | The probe already implements and live-validates the full surface (~700 lines incl. the E2 flows); GAG idioms exactly (httpx bounded clients, typed errors, metrics recorder, `scalesettest` fake); zero Preview coupling; matches GAG's protocol-transparency positioning ([appendix D](../../design/appendix-d-alternatives-considered.md) critiques ARC's protocol opacity). Cost: GAG re-owns JWT/session refresh (subtleties now probed and documented, §2b-7) and tracks upstream wire changes manually. |
| D | Fork / vendor-and-patch | Worst of both; fallback only. |

**Decision (signed off 2026-07-04): C — GAG-owned client, tracking `actions/scaleset` as the reference spec.** The deciding argument: **whichever option is chosen, GAG must own the auto-assign semantics** — the fake, the tests, and the invariants must encode what §2a/§2b probed, because upstream neither documents nor answers for them (#107).
Once GAG owns the semantics, the fake, and a wrapper interface, the marginal cost of owning the ~700-line wire client is small — and it removes a v0.x dependency whose one demonstrated failure mode (dropping the acquire flow) would have broken GAG's GHES path silently.
Revisit trigger: upstream reaching GA/v1.0, or GHES-specific divergence proving expensive to replicate.

### U7 — where the protocol selector lives

**GAG facts:** the v1alpha1 surface is frozen (migration is the `gag-migrate` fan-out tool, not conversion); `v2alpha1` is **alpha — adding a spec field now is a free reshape**, whereas after the v2beta1 freeze it needs the Q74 conversion webhook (graduation is not imminent: blocked on Q191/Q196/Q197/Q224/Q242/Q243).
A per-set field reaches the AGC directly through its RunnerSet informer — no GMC env threading needed (that mechanism is for gateway-level config like `GATEWAY_NAME`).

**Options:**

| | Option | Assessment |
|---|---|---|
| a | v1alpha1 `RunnerGroup` field | Violates the v1 freeze; doubles the surface; complicates `gag-migrate`. Reject. |
| b | **v2alpha1 `RunnerSet.spec.acquisitionProtocol: Classic \| ScaleSet` (default `Classic`)** | Per-set granularity → tenants migrate set-by-set; free while alpha; CEL-expressible constraint. |
| c | `ActionsGateway`-level selector | Whole tenant flips at once — kills incremental migration. Reject. |
| d | AGC env flag only | Not tenant-declarative; invisible in the API. Reject (fine as an additional operator kill-switch during P3/P4, not as the surface). |

**Decision (signed off 2026-07-04): b**, with these specifics:

- **Field:** `acquisitionProtocol`, enum `Classic|ScaleSet`, default `Classic` (stability-by-default; neither value relaxes a security property).
  Naming follows [appendix H §H.6](../../design/appendix-h-v2-api-decomposition.md).
- **Immutable** via CEL `oldSelf` (H.15 pattern): switching a live set is a re-registration storm; start immutable — *relaxing* immutability later is compatible, adding it later is breaking.
- **Admission (CEL):** `ScaleSet` ⇒ `size(runnerLabels) == 1` — the scale set's name **is** the single label (the tenant's `runs-on` contract), not the RunnerSet object name.
  Webhook-level check: no two `ScaleSet`-protocol RunnerSets under one gateway may share that label (two scale sets with one name would collide at GitHub).
- **v2-exclusive by design** (signed off with explicit rationale 2026-07-04): the field never appears on v1alpha1 — zero changes to the frozen v1 surface, and the fan-out-free acquisition model becomes a concrete, user-visible reason to migrate to v2, alongside the decomposed kinds.
  `gag-migrate` maps v1 groups to `Classic` RunnerSets unchanged; a tenant opts into `ScaleSet` by editing the migrated RunnerSet (a new object, so the immutability rule never blocks the migration itself).
- **The field is transitional — v2beta1 is ScaleSet-only** (signed off 2026-07-04): `acquisitionProtocol` exists only on v2alpha1, as the per-set canary/rollback lever P3–P4 need (the Q260 lesson: live validation wants per-set opt-in, and rollback must be a field edit, not an AGC image downgrade across every v2 tenant).
  **v2beta1 never serves `Classic`**: the Q74 graduation conversion strips the field, and `maxListeners` — meaningless under ScaleSet (one session per set; concurrency is governed by `maxWorkers`/`priorityTiers` via the capacity header) — is removed from RunnerSet at the same graduation (documented as ignored for `ScaleSet` sets in the interim).
  End state: the protocol is an API-version property — v1 = classic (deprecated), v2 = scale-set — with no enum surviving.

### U8 — support-matrix policy

**Enterprise scope — non-regression; declare out of scope.** GAG supports org- and repo-scope registration only (URL-shape selection in [`github_registrar.go`](../../../cmd/agc/internal/agentpool/github_registrar.go); the three documented URL forms) — it has never supported enterprise-scope runners.
The scale-set PAT-only limitation at enterprise scope is a GitHub platform constraint that binds ARC identically ([ARC auth docs](https://docs.github.com/en/actions/tutorials/use-actions-runner-controller/authenticate-to-the-api)).
No PAT credential mode; document enterprise scope as out of scope (it already is).

**GHES — the ≥3.9 floor excludes nothing; but GHES keeps the acquire flow.** As of 2026-07 the supported GHES window is ~3.17–3.20 ([release lifecycle](https://docs.github.com/en/enterprise-server@3.17/admin/all-releases)); GHES 3.9 has been EOL for years, so the floor costs zero vendor-supported deployments.
The real GHES fact is upstream's #75/#90: **GHES's scale-set backend requires the explicit `JobAvailable` → `acquirejobs` flow** (removing it broke GHES), while dotcom's broker-host auto-assigns (§2a-3).
The GAG client therefore implements **both** paths with one rule — acquire exactly the ids that arrive as `JobAvailable`, and treat `JobAssigned` as authoritative regardless of origin (ARC's listener logic).
This also settles the U2′ residual: dotcom-vs-GHES skew is a handled case, not a fork.

**Org-scope public repos — document + prefer repo scope.** GAG's existing URL-shape scope selection carries over: repo-shaped `githubURL` → repo-scoped scale set (bypasses runner groups; the dogfood model).
Operator docs (P3/P5) gain: org-scoped scale sets serving **public** repos require a runner group with `allows_public_repositories: true` (§2a-6), plus a troubleshooting entry (symptom: jobs queue forever, `totalAvailableJobs: 0`).
Optional later enhancement, not now: an advisory condition when an org-scoped set assigns nothing while jobs queue.

**Classic retirement — deprecation window, then remove.** Keeping classic forever re-imports the dual-protocol maintenance burden Option E exists to end; retiring it at cutover strands nobody (scale sets cover dotcom + all vendor-supported GHES).
Recommended sequence: flagged coexistence through P3–P4 → default flips to `ScaleSet` at P5 (with the positioning-doc rewrite, §4.7) → classic deprecated for **one minor release** → classic machinery (agent pool + Q114 recycle, multiplexer, Q260 dedup, classic broker client) removed in an isolated PR, **aligned with the Q74 v2beta1 graduation** — the same hop that strips the transitional `acquisitionProtocol` field and `maxListeners` (U7): v2beta1 never serves `Classic`, so graduation is the natural removal milestone for the classic machinery too.

**Consequence of ScaleSet being v2-exclusive:** classic is v1alpha1's *only* acquisition path, so removing the classic machinery necessarily ends v1alpha1's ability to acquire jobs — the classic deprecation window **is** the v1alpha1 migration window.
The removal PR must therefore be sequenced after v1alpha1 is itself deprecated (tenants moved via `gag-migrate`); announcing the two deprecations together is honest and turns the fan-out-free model into the concrete incentive to complete the v1→v2 migration.

**Ladder (updated 2026-07-13).** P5 default flip **shipped in v1.1.0** (Classic deprecated in the release notes — the one-minor deprecation window starts here). v2beta1 graduation has **also shipped** (v2beta1 is the served+storage/hub version) — but it graduated **without** carrying the classic-machinery removal the §5a alignment above anticipated.
So the removal is no longer bundled into the graduation hop; it is now the terminal step, gated on two independent conditions:

1. the one-minor classic deprecation window from v1.1.0 elapses (i.e. v1.2.0 ships), and
2. v1alpha1 tenants have migrated off via `gag-migrate` (Q273) — since Classic is v1alpha1's only acquisition path, its removal ends v1alpha1's ability to acquire.

When both hold, one isolated PR removes the classic machinery (agent pool + Q114 recycle, multiplexer, Q260 dedup, classic broker client) and the transitional `acquisitionProtocol` / `maxListeners` fields.

## 6. Phased execution path (no big-bang rewrite)

Mirrors the M1 → M2 pattern that built the classic tier: probe first, then a parallel implementation behind a flag, then cutover.

- **P0 — this spike.** This doc.
  No code.
- **P1 — wire probe (S). ✅ DONE 2026-07-04, both rounds** — the `PROBE_SCALESET_TEST=true` scenario in [`cmd/probe`](../../../cmd/probe/scaleset.go) plus the `scaleset-probe.yml` fixture.
  Round 1 (§2a) settled U1/U2 and half of U4; round 2 (§2b, `PROBE_SCALESET_CAPACITY_TEST` + real docker-run runners) settled U2′ and the cores of U3/U5.
  Every protocol-level unknown is now probed; what remains for P4 is integration-level only (egress proxy, pod conditions, sustained load).
- **P2 — scale-set client package (M). ✅ DONE** — the new leaf module [`scaleset/`](../../../scaleset/) (`broker/`-style — httpx bounded clients that clone the proxy-patched `http.DefaultTransport`, typed errors, a nil-safe `MetricsRecorder`), promoting the probe's live-validated flows with types mirroring `actions/scaleset` for wire parity (not vendored — U6-C).
  Covers: the two-hop auth bootstrap; lazy admin-JWT refresh ~60s pre-expiry (§2b-7); scale-set CRUD + runner-group resolve + per-job `generatejitconfig`; session create/refresh(PATCH)/delete; the capacity-gated queue long-poll; both acquisition paths as one rule (`AvailableJobIDs`→`AcquireJobs` on GHES, `AssignedJobs` authoritative regardless of origin — §5a-U8); and the token matrix (queue token, not admin JWT, on `acquirejobs` + poll — §2.5).
  Plus the [`scalesettest`](../../../scaleset/scalesettest/server.go) fake (modelled on [`brokertest`](../../../broker/brokertest/server.go)) encoding the §2a/§2b semantics: auto-assign under advertised capacity **and** the GHES-style `JobAvailable`→acquire flow, cursor-based at-least-once replay to a re-created session, admin- and queue-token expiry, and claim-once acquisition.
  Unit + client-vs-fake coverage (81.6%, race-clean); no envtest (a pure network-client package — coverage is entirely client-vs-fake); **no AGC wiring** (P3).
  **P2-surfaced unknown for P4:** the message-DELETE ack endpoint shape (`DELETE {messageQueueUrl}/{messageId}`, §2.2) is source-derived from the official listener but was **not** exercised by the live probe (which acked by cursor only); `Client.DeleteMessage` implements it and the fake tests the construction, but P4 must confirm the URL shape and status semantics on a live tenant before the P3 listener relies on delete-acking over pure cursor advance.
  **P3 now builds on:** `scaleset.Client` (one per RunnerSet) + the `scalesettest` fake for the listener's acceptance twin.
- **P3 — parallel acquisition tier behind the API field (L). ✅ DONE.** Per U7 (§5a): `RunnerSet.spec.acquisitionProtocol` (v2alpha1, default `Classic`, immutable, CEL-validated single label) selects a scale-set listener per set (one goroutine: session + capacity-gated poll + dispatch) and the `run.sh --jitconfig` worker path.
  The Q260 `TestAGC_Q260_FanoutCompletionReconciles` acceptance shape gets a scale-set twin: N concurrent jobs, every one concludes `completed`, zero dedup involved.
  Landing as a sequence of PRs, all under Q264 P3 and **all behind default `Classic`** (nothing changes for existing users until P5):
  - **(a) API field + codegen + CEL/webhook — ✅ DONE.** `acquisitionProtocol` added to `api/v2alpha1` RunnerSet: enum `Classic|ScaleSet`, default `Classic`, immutable (CEL `self == oldSelf`), spec-level CEL `ScaleSet ⇒ size(runnerLabels) == 1`, and a GMC validating webhook (`vrunnerset-v2alpha1.kb.io`) rejecting two `ScaleSet` sets that share their single label under one gateway (the scale-set-name collision a spec-scoped CEL rule cannot see).
    CRD + chart + webhook manifests regenerated.
    Coverage: webhook unit tests (fake client), a cmd/agc CRD envtest (defaulting/immutability/single-label CEL), and a cmd/gmc admission-through-apiserver envtest for the sibling-uniqueness webhook. v1alpha1 untouched.
  - **(b) scale-set listener — ✅ DONE.** New standalone package [`scalesetlistener`](../../../cmd/agc/internal/scalesetlistener/): one `Listener` per (future) ScaleSet set holds a single session, long-polls advertising free worker slots as `X-ScaleSetMaxCapacity`, mints a per-job JIT config and provisions one worker per `JobAssigned` (GHES `JobAvailable`→`AcquireJobs` handled as the one rule), and reconciles against the server-authoritative `statistics.totalAssignedJobs`.
    Acking is **cursor-only** (the live-proven semantics, §2b-4) — it deliberately does not rely on the unproven `DeleteMessage` (P4); recovery is queue replay to a re-created session, made idempotent by process-scoped provisioned/completed sets.
    The engine is **not yet wired into the RunnerSet reconciler** (default Classic trivially preserved); that + the real provisioner/worker path is (c).
    Tests (race-clean) against the `scalesettest` fake: one-worker-per-job/zero-dedup, capacity gating, assigned-count reconciliation, session-recreate replay without double-provision, the GHES acquire path, and **the fan-out-free acceptance twin** — the scale-set analog of `TestAGC_Q260_FanoutCompletionReconciles`: N concurrent jobs, every one concludes `completed`, zero dedup.
  - **(c) worker path + provisioner staging — ✅ DONE.** The entrypoint wrapper ([`cmd/worker`](../../../cmd/worker/main.go)) gains a `WORKER_MODE=scaleset` mode: no payload handoff — the pod runs the full runner via `run.sh --jitconfig`, which pulls and completes its own job (§2.4); the wrapper keeps only its proxy-CA trust duty (the pipes handoff + Runner.Worker spawn do not run).
    `Provisioner.ProvisionScaleSetWorker` stages a JIT-config Secret (the blob, no acquired payload) and creates a scale-set-mode worker pod, fire-and-forget and idempotent per jobID.
    Not yet called by any reconciler (default Classic unchanged).
    Unit tests: the wrapper's run.sh exec + proxy-CA trust, and the provisioner's JIT Secret staging + scale-set pod mode.
  - **(d) RunnerSet-controller wiring + envtest — ✅ DONE.** The AGC RunnerSet reconciler now branches on `spec.acquisitionProtocol == ScaleSet` ([`runnerset_scaleset.go`](../../../cmd/agc/internal/controller/runnerset_scaleset.go)): once references resolve it starts exactly one `scalesetlistener` per set (session + capacity-gated poll + provision-on-`JobAssigned`) via a per-set `scaleset.Client` (config URL/API base from the resolved gateway's `githubURL`; a `ScaleSetClientFactory` seam points tests at the `scalesettest` fake), with Provision → `Provisioner.ProvisionScaleSetWorker` and Capacity → the maxWorkers/priorityTiers ceiling advertised as `X-ScaleSetMaxCapacity` (default 10 when neither is set).
    Idempotent across reconciles (one session per set, §2.2); the listener's context derives from the manager-scoped reconcile ctx so it stops on RunnerSet delete (`stopScaleSetListener` cancels and waits for the loop to delete the session) or manager shutdown — no leaked session/goroutine.
    A Classic set is untouched (never builds a client, never registers a scale set).
    The listener's Actions Service traffic routes through the per-tenant egress proxy exactly like classic (the client clones the proxy-patched transport); the App token never reaches the worker (§4).
    Envtest ([`v2_runnerset_scaleset_test.go`](../../../cmd/agc/internal/controller/integration/v2_runnerset_scaleset_test.go)): a ScaleSet set registers one scale set + session, a `JobAssigned` from the fake provisions one `WORKER_MODE=scaleset` worker pod with the JIT-config Secret staged (no payload), and deleting the set stops the listener + deletes the session; a Classic set drives the classic path and never reaches the fake.
    This is the step that makes `ScaleSet` live behind the field — **P3 is complete**.
- **P4 — live validation (M). ✅ DONE — CLEAN-GREEN ACHIEVED 2026-07-05 (re-run).
  The whole CI matrix went actually all-green on the ScaleSet path.** Two passes.
  The **clean-green re-run** (below) is the one that CLOSES Q224; the first pass (immediately after) proved the acquisition claim but left 4 CI jobs non-green for reasons orthogonal to acquisition, both since fixed.

  **Clean-green re-run — 2026-07-05, `main`@`2025557` (Q269 #542 + Q270 #544 in).** Rebuilt AGC + wrapper off current `main` — `agc:e2e-2025557` (index `sha256:cef2a16b…`, amd64 `sha256:229435a5…`) and `wrapper:e2e-2025557` (`sha256:7974a83b…`) — deployed via the GMC `AGC_IMAGE`/`WRAPPER_IMAGE` patch.
  The stale P4 `dogfoodss` tenant had been torn down to a bare orphaned `ciss`; the gateway + template were recreated, and `ciss` was reset to a **fresh scale-set label `gag-scaleset3`** (scaleSetID 5) — necessary because reconnecting to P4's `gag-scaleset2` (scaleSetID 4) **replayed** the old sha-`4ea41f6` `JobAssigned` messages from the scale-set-scoped queue (§2b-3), which would have re-run the pre-Q269 code; a new label = a new scale-set object = an empty queue.
  Capacity: `workers-od` `e2-standard-4 ×6` `pd-standard` (bumped 4→6 for CPU headroom vs the first pass's saturation), `maxWorkers: 8`, worker CPU req 1.
  Fired the same 7-job matrix via Q271 opt-in routing (`workflow_dispatch` + `target_gag=true`, `GAG_RUNNER → "gag-scaleset3"`): unit-test.yml `28759754797` (6 GAG jobs) + integration-test.yml `28759755655` (1 GAG job).
  - **Q224 GATE — MET (again, clean).** 7 distinct `JobAssigned` → **7 distinct worker pods in ~2 s** (00:11:30–32Z), one per job, **0 dedup / 0 `already exists` / 0 jitconfig conflict / 0 cursor wedge** (Q270 fix holds — the AGC log shows zero conflict/wedge lines across the run).
  - **ALL 7 GAG jobs concluded GREEN** on `gag-scaleset3-<jobUUID>` runners (1:1 with the provisioned pods): `unit-test` ✅ + `coverage` ✅ (**Q269 WORKER_MODE fix HOLDS** — the two jobs that failed the first pass are now green), `shellcheck`/`tidy-check`/`vendor-check` ✅, `lint` ✅ (no `timeout-minutes: 10` lapse — the first pass's timeout was CPU saturation, gone with 6-node headroom), `integration-test` ✅ (no envtest `context canceled` — the first pass's capacity confound, [Q248](../dogfood-runner-rightsizing.md), resolved by the node bump).
    Both runs concluded `success`.
    Worker pods reaped `phase: Succeeded` (runners exited 0).
  - **Verdict.** ScaleSet **eliminates the Q224 fan-out starvation by construction AND runs the real CI matrix pristine-green** — Option E is fully validated live.
    **Q224 is CLOSED; P5 (default flip / classic retirement) is UNBLOCKED**; [Q242](q242-g1-proxy-destination-allowlist.md) concurrent-green is achieved.
    Evidence: AGC debug logs (`agc:e2e-2025557`, scaleSetID 5), runs `28759754797`/`28759755655` (burst `00:11:30Z`, sha `2025557`).

  **First pass — 2026-07-05, `main`@`8a29b75` (the acquisition proof).** Deployed a fresh AGC built off `main`@`8a29b75` (=#537, all P3 in) — `ghcr.io/actions-gateway/agc:e2e-8a29b75` (index `sha256:4c88631d…`, amd64 `sha256:91dd52ad…`) — via the GMC `AGC_IMAGE` patch, **plus the matching P3c wrapper** `ghcr.io/actions-gateway/wrapper:e2e-8a29b75` (index `sha256:0040ae1e…`, via the GMC `WRAPPER_IMAGE` patch — see finding 3 below).
  Fresh **ScaleSet tenant** on `gag-dogfood`: `ActionsGateway/dogfoodss` (repo-scoped per §2a-6, **direct-egress** — matching the classic re-route baseline — `logLevel: debug`), `RunnerTemplate/default-ss` (build-capable `dogfood-runner:2.335.1` + Athens), `RunnerSet/ciss` (`acquisitionProtocol: ScaleSet`, single `runnerLabels: [gag-scaleset2]`, `maxWorkers: 8`).
  Capacity: non-preemptible `workers-od` `e2-standard-4 ×4` `pd-standard` ([Q248](../dogfood-runner-rightsizing.md)) + `default-pool` 2, worker CPU req 1.
  The v2 CRD chart had to be upgraded to HEAD first (the pinned rc.6 CRD predated the `acquisitionProtocol` field).
  Fired the same ~7-job matrix as classic re-routes #5–#8 — push-event reruns of unit-test.yml `28752455482` (6 jobs) + integration-test.yml `28752455509` (1 job) on sha `4ea41f6`, routed via `GAG_RUNNER → "gag-scaleset2"`.
  - **Q224 GATE — MET.** The single-acquirer listener received **7 distinct `JobAssigned` messages** and provisioned **7 distinct worker pods in 3 seconds** (one per job) — **zero dedup, zero `create Secret … already exists`, zero `create Pod … already exists`**.
    All 7 jobs **ran** and all 7 reached a **terminal conclusion**; **none wedged `in_progress` indefinitely** — the decisive contrast with classic (re-routes #5/#8: **2/7**, 5 jobs starved forever because their distinct planIDs never arrived).
    One acquirer, one authoritative queue, no sibling deliveries → the starvation class cannot occur, confirmed live.
    `maxWorkers=8` advertised as `X-ScaleSetMaxCapacity`; GitHub assigned exactly the 7 queued jobs (≤ capacity), one worker each.
  - **Terminal conclusions: 3 clean green, 4 non-green — every non-green ORTHOGONAL to acquisition.** `shellcheck`/`tidy-check`/`vendor-check` ✅ (the last confirms Athens under the scale-set worker).
    `unit-test` ❌ + `coverage` ❌: **a self-referential dogfooding artifact** — the provisioner sets `WORKER_MODE=scaleset` on the runner *container* (§2.4), so the job's `go test` inherits it and the `cmd/worker` tests take the jitconfig branch and fail their classic-payload assertions (`TestRun_ReadPayloadErrorIsWrapped`, …); `cover-check` runs the same suite.
    Only bites because GAG dogfoods its **own** CI on its **own** scale-set worker — a normal tenant is unaffected.
    **Fix = `cmd/worker` tests must pin `WORKER_MODE` (Queue item); it must land on `main` before a clean-green re-run is possible.** `integration-test` ❌: envtest `context canceled` under node CPU saturation (98–101%) — capacity confound ([Q248](../dogfood-runner-rightsizing.md)).
    `lint` (cancelled): hit its own `timeout-minutes: 10` (golangci-lint pathologically slow on the saturated node) — a mundane timeout, **not** a lock lapse.
  - **U3 residual — CORE SETTLED under pod conditions (direct-egress):** the `run.sh --jitconfig` worker ran in a real pod, connected, pulled its job, executed, and **its runner reported its own true terminal result** (`Job … completed with result: Failed`).
    **Residual:** the same flow behind the per-tenant **egress proxy** (proxy-CA trust bundle) was not exercised (direct-egress tenant) — stays a follow-up.
  - **U5 residual — NOT tested this run:** no mid-job eviction performed (the two ~10-min durations were natural runtime / a job timeout, not lock lapses).
    U5-core was offline-probed (§2b-5); U5-under-pod-eviction stays a follow-up.
  - **Minor code findings (Queue):** (1) ✅ **FIXED (Q270).** `scaleset.statusError` mapped **every** 409 to `SessionConflictError` — a `generatejitconfig` runner-name conflict was mislabeled.
    Now `statusError` yields a neutral `ConflictError` and each call translates it: `CreateSession`→`SessionConflictError`, `GenerateJITConfig`→a new `RunnerNameConflictError` ([`client.go`](../../../scaleset/client.go), [`errors.go`](../../../scaleset/errors.go)).
    (2) ✅ **FIXED (Q270).** The listener retried `generatejitconfig` with **no backoff** and never advanced the cursor on a persistent 409 → a replay loop that wedged the batch on a stale registered runner-name.
    Now a runner-name conflict retries under a *fresh* runner name (bounded by `maxJITNameConflictRetries`, backed off), and a persistently-conflicting job is **skipped** so the cursor advances past it and the other jobs still provision ([`listener.go`](../../../cmd/agc/internal/scalesetlistener/listener.go)).
    (3) **Deploy-coupling:** the scale-set worker **requires** the P3c wrapper; a stale wrapper silently runs the classic payload path and every worker errors `open …/job-payload/payload` (P5 rollout note — bump wrapper in lockstep with AGC).
    (4) cosmetic post-completion `[RUNNER ERR BrokerServer] HttpClient` after the result is already reported (harmless).
  - **First-pass verdict (superseded by the clean-green re-run above).** ScaleSet **eliminates the Q224 fan-out starvation by construction — proven live (7/7 assigned+ran+concluded vs classic 2/7)**; the Option E structural claim is CONFIRMED.
    At the time this pass left Q224 open pending the `WORKER_MODE` fix on `main` + a clean re-run on adequate CPU — **both delivered on the 2026-07-05 re-run (all 7 green), so Q224 is now CLOSED and P5 is unblocked.** First-pass evidence: AGC debug logs (`agc:e2e-8a29b75`, scaleSetID 4), reruns `28752455482`/`28752455509` (burst `19:55:54Z`, sha `4ea41f6`).

  *Observability prep (done, #538):* the tier emits per-`RunnerSet` Prometheus counters (`actions_gateway_scaleset_jobs_assigned_total` / `…_jobs_provisioned_total` / `…_provision_errors_total` / `…_jobs_completed_total{result}`) via a `scalesetlistener.Metrics` recorder wired into the listener `Config`.
  Documented in [observability.md](../../operations/observability-metrics.md#scale-set-acquisition-tier-q264).
  *Prerequisite fixed (Q269):* `cmd/worker`'s tests now clear the ambient `WORKER_MODE` in `TestMain`, so the classic-path unit tests stay deterministic when GAG's own CI runs on a `WORKER_MODE=scaleset` runner pod — otherwise the P4 dogfood CI (which runs `main`'s code) fails unit-test+coverage before the Q224 matrix can even run.
- **P5 — cutover + retirement (M). ▶ default flip DONE 2026-07-06; deprecation window + classic removal are the remaining residual.** The default of `RunnerSet.spec.acquisitionProtocol` (v2alpha1) is flipped `Classic → ScaleSet` (`+kubebuilder:default=ScaleSet`, CRD + both chart copies regenerated); `Classic` is marked **deprecated** (godoc `Deprecated:` + the field doc + operator docs).
  Secure-by-default holds — the flip relaxes no security property (single-acquirer topology, same egress/isolation/NetworkPolicy; §5a): Classic was default only for stability-by-default, now satisfied by P4 clean-green.
  - **Consequences of the flip, handled in this step:**
    - A bare (protocol-omitted) `RunnerSet` now defaults to `ScaleSet`, which
      requires exactly one `runnerLabel`; a **multi-label** set must set
      `acquisitionProtocol: Classic` explicitly. Documented in tenant-onboarding +
      a troubleshooting entry.
    - `gag-migrate` now emits `acquisitionProtocol: Classic` on every generated
      set (was relying on the old default) — a migrated multi-label v1 group would
      otherwise default to `ScaleSet` and be rejected on apply. Migrate golden +
      unit assertion updated.
    - Existing sets are unaffected: the value is persisted at admission, so any set
      created while the default was `Classic` keeps `Classic`.
  - Tests: envtest defaulting flipped to assert `ScaleSet`; a new envtest asserts a bare multi-label set is now rejected and the same labels are accepted under explicit `Classic`; shared v2 fixtures pin `Classic` (they exercise the classic path). agc + gmc integration suites green.
  - **Remaining P5 residual (separate later PRs, NOT this step):** run the U8 retirement sequence (§5a: one-minor-release deprecation → **remove** the classic machinery in an isolated PR, aligned with the Q74 v2beta1 graduation).
    Optional light dogfood re-confirm (P4 already validated ScaleSet clean-green).
  - **Doc-ownership handoff:** the §4.7 positioning rewrite (why-gag / index / website / the v1→v2 migration narrative) and the Option E design-narrative updates to [03-api-contracts §3.3](../../design/03-api-contracts.md) / [02-architecture §2.2](../../design/02-architecture.md) are **Q273's** ("make v2 the front door") — this step deliberately does not touch those to avoid a collision.
    This step owns only the field default + the deprecation notice + the operator docs (tenant-onboarding, troubleshooting, observability, v1alpha1-deprecation) per the [doc-update matrix](../../development/doc-update-matrix.md).

Each phase lands independently; P1/P2 are useful even if Option A wins (protocol knowledge + a fake that documents GitHub's dispatch model).

## 7. Test strategy

- **P1:** live probe log = the evidence artifact, findings folded back into §2 (the M1 §8 pattern).
- **P2:** the `scalesettest` fake encodes claim-once semantics (an id acquired twice must fail the second claim), message replay, token expiry; the client is tested against it.
- **P3:** listener unit tests (batch gating vs capacity, assigned-count reconciliation, session re-create replay) + envtest (provision on `JobAssigned`, JIT Secret staging) + the fan-out-free acceptance twin.
- **P4:** the Q224 concurrent matrix green on the flagged path is the go/no-go for P5.

## 8. Sources

Fetched 2026-07-04; all public:

- **`actions/scaleset`** (official Go client, Public Preview): `client.go`, `session_client.go`, `types.go`, `config.go`, `common_client.go`, `listener/listener.go`.
- **`actions/actions-runner-controller`** @ `gha-runner-scale-set-0.10.1`: `github/actions/client.go` (scale-set endpoints, sessions, `AcquireJobs`, `GenerateJitRunnerConfig`, token refresh), `types.go`, `multi_client.go`; `cmd/ghalistener/`; `controllers/actions.github.com/` (AutoscalingRunnerSet / AutoscalingListener / EphemeralRunnerSet / EphemeralRunner); `docs/gha-runner-scale-set-controller/README.md`.
  Independently cross-checked against tag `v0.27.6` (`github/actions/{client,types}.go`, `cmd/githubrunnerscalesetlistener/{autoScalerService, autoScalerMessageListener, sessionrefreshingclient}.go`) — endpoints, token split, and message shapes agree.
  Note for future readers: on ARC `master` the `github/actions` client package is a stub — read a `gha-runner-scale-set-*`/`v0.27.x` tag or `actions/scaleset`.
- **`actions/runner`** @ `main`: `src/Runner.Common/RunnerDotcomServer.cs` (classic registration), `BrokerServer.cs` (the runner-side session that receives the job payload), `RunnerServer.cs`.
- **docs.github.com**: ARC "Runner scale sets" concepts, "Authenticate to the API" (App permission matrix; enterprise = PAT-only), "Deploying runner scale sets with ARC" (GHES ≥ 3.9).
- **§5a decision research (2026-07-04)**: `actions/scaleset` @ HEAD `1b6da87` — `go.mod`/LICENSE, `client.go`/`session_client.go`/`common_client.go`/ `errors.go`/`config.go`, `listener/listener.go`, releases v0.1.0–v0.4.0, issues #75/#90 (GHES acquire-flow removal + restore), #104 (client mutex), #107 (auto-assign, unanswered); ARC master `go.mod` (requires `scaleset v0.4.0`); GHES release lifecycle (supported window ~3.17–3.20 as of 2026-07); ARC authentication docs (enterprise scope PAT-only).

Per the repo rule that source-inspection findings are unverified until exercised end-to-end, §2's wire-level specifics were treated as unconfirmed until the P1 probe ran.
**Investigation E (§2a) has now live-confirmed the registration/session/queue/JIT chain — and corrected the acquisition model on the current backend** (auto-assign, no client acquire).
Full probe logs: `INVESTIGATION-E:` lines from the 2026-07-04 runs.
