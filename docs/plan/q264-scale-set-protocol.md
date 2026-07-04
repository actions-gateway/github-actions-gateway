# Q264 — Migrate AGC acquisition to the runner-scale-set protocol (Option E feasibility spike)

**Status:** design/feasibility spike **plus a live wire probe** (Investigation E,
§2a — `cmd/probe` scenario, run 2026-07-04) — **no production acquisition code
changed**. This is
[Option E in the Q260 design](q260-fanout-completion-reconciliation.md#option-e--single-acquirer-topology--adopt-the-runner-scale-set-protocol-treat-the-cause):
the deferred fallback pursued **only if live re-route #5 rules Option A
(winner fan-out completion) infeasible**. The go/no-go stays with re-route #5;
this doc exists so that fork is not started cold.

**Verdict up front: Option E is viable — and materially cheaper than the Q260 doc
priced it.** The live probe (§2a) then **strengthened** the verdict: GAG's own
GitHub App drove the full protocol end-to-end at both org and repo scope, and
on the current broker-host backend GitHub **auto-assigns** each job to the
scale set (no client acquire call at all, capacity-gated by a header) —
assignment is 1:1 by construction with even less client machinery than the
ARC-era sources document. GitHub has published
[`actions/scaleset`](https://github.com/actions/scaleset) (Public Preview), an
**official standalone Go client + listener package for this exact protocol**,
"so that platform teams, integrators, and infrastructure providers can build their
own custom autoscaling solutions." The protocol is no longer
undocumented-internal-to-ARC: the Go source is a supported reference (though still
no wire-spec document). No security property is forfeited — egress isolation,
the App-token-never-in-the-worker property, and on-demand workers all carry over.
The honest costs: the worker handoff contract must change (forced — see §2.4),
`runs-on` matching collapses to a single label per group, GHES support floor
rises to 3.9, enterprise-scope registration is PAT-only (GAG is App-based), and
each group's acquisition becomes a single session (inherent — it is *why* the
fan-out disappears). Details in §4; load-bearing unknowns a spike cannot settle
from source alone in §5; phased path in §6.

---

## 1. Why this spike exists

Q260 established (live, re-routes #2/#4) that GAG's fan-out race is intrinsic to
its **many-acquirers topology** on GitHub's classic per-runner broker protocol:
concurrency = registered runners = acquirers, so GitHub fans one logical job out
to N sibling sessions, all acquire it (shared planID), and N−1 assignments
dangle on GitHub's books until the unstarted-job timeout cancels the whole job
([Q260 §1–§2](q260-fanout-completion-reconciliation.md#1-the-protocol-from-the-code-and-the-live-evidence)).
Option A reconciles the accounting AGC-side; Option E removes the race **by
construction** by adopting the protocol modern ARC uses: one listener per scale
set acquires each job exactly once via a batch message-queue claim, then a
dedicated ephemeral runner executes it — 1 job : 1 queue entry : 1 acquirer :
1 runner, no sibling deliveries to reconcile.

The pitch is unchanged from the Q260 doc: **"ARC's protocol, GAG's efficiency"**
— a Go listener goroutine per group instead of ARC's ~256 MiB .NET listener pod
per scale set, keeping per-tenant egress isolation and on-demand worker pods.

## 2. The runner-scale-set protocol, documented

Reverse-engineered 2026-07-04 from three mutually consistent public sources
(§8): GitHub's official **`actions/scaleset`** Go client, the ARC client at tag
`gha-runner-scale-set-0.10.1` (`github/actions/client.go` — on `master` this
package is now a stub; the client moved to `actions/scaleset`), and the runner's
C# (`src/Runner.Common/RunnerDotcomServer.cs`, `BrokerServer.cs`). Source-level
confidence is high; wire-level details still need a live probe (§5).

### 2.1 Registration: one scale set + per-job JIT configs, not N agents

Auth bootstraps in two hops, then pivots off the public REST API entirely:

1. **Registration token** — `POST /orgs/{org}/actions/runners/registration-token`
   (or `/repos/{owner}/{repo}/...`, `/enterprises/{ent}/...`) with a PAT or App
   installation token → short-lived `{token, expires_at}`. Same call GAG's
   classic registrar family already uses.
2. **Admin connection** — `POST {github}/actions/runner-registration` with the
   nonstandard header `Authorization: RemoteAuth <registration token>` and body
   `{"url": <configURL>, "runner_event": "register"}` → returns the
   runtime-discovered **Actions Service tenant URL** (e.g.
   `https://pipelines.actions.githubusercontent.com/<tenant>`) and an
   **admin JWT** (~1 h; ARC parses `exp` and refreshes 60 s before expiry —
   `updateTokenIfNeeded`, ARC `client.go:1246`).

Everything else targets `{actionsServiceURL}/_apis/runtime/...` with
`Authorization: Bearer <admin JWT>` and `api-version=6.0-preview`
(Azure-DevOps-style `{count, value}` envelopes):

| Call | Endpoint |
|---|---|
| Create scale set | `POST /_apis/runtime/runnerscalesets` (body `RunnerScaleSet{name, runnerGroupId, labels, runnerSetting{ephemeral,…}}`) |
| Get / update / delete | `GET`/`PATCH`/`DELETE /_apis/runtime/runnerscalesets/{id}` |
| Resolve runner group | `GET /_apis/runtime/runnergroups/?groupName={name}` |
| **Per-job JIT config** | `POST /_apis/runtime/runnerscalesets/{id}/generatejitconfig` (body `{name, workFolder}`) → `{runner{id,name}, encodedJITConfig}` |

Contrast with classic: instead of pre-registering `maxListeners` agents per
group (`generate-jitconfig` per agent,
[`github_registrar.go:89`](../../cmd/agc/internal/agentpool/github_registrar.go))
and recycling each single-use agent after every job (Q114), the controller
registers **one scale-set object per group once**, then mints **one JIT config
per acquired job** for the runner that will execute it. The server
pre-registers the runner and returns a base64 blob bundling `.runner` +
`.credentials` (+ RSA parameters); the runner consumes it with
`run.sh --jitconfig <blob>`. The **scale set's `name` is its single `runs-on`
label** — there is no free-form label list per scale set.

### 2.2 Session + message queue: one authoritative stream per scale set

- `POST /_apis/runtime/runnerscalesets/{id}/sessions` (body
  `{"ownerName": <listener identity>}`) → `RunnerScaleSetSession{sessionId,
  messageQueueUrl, messageQueueAccessToken, statistics}`.
  **One active session per scale set** — a second create conflicts until the
  first is deleted or expires. `PATCH .../sessions/{id}` refreshes the queue
  token (ARC triggers it on a 401 from the queue); `DELETE` on shutdown.
- Long-poll: `GET {messageQueueUrl}?lastMessageId={N}` with
  `Authorization: Bearer <messageQueueAccessToken>` and header
  `X-ScaleSetMaxCapacity: <maxRunners>`. `200` → one `RunnerScaleSetMessage`;
  `202` → empty, poll again (server holds ~50 s).
- A message carries `messageId`, batched typed bodies — `JobAvailable`,
  `JobAssigned`, `JobStarted`, `JobCompleted` — and a
  `RunnerScaleSetStatistic{totalAvailableJobs, totalAcquiredJobs,
  totalAssignedJobs, totalRunningJobs, totalRegisteredRunners, totalBusyRunners,
  totalIdleRunners}` snapshot. The `actions/scaleset` README is explicit: scale
  on `statistics.TotalAssignedJobs`, not by counting individual messages — the
  ARC listener's exact formula is
  `clamp(statistics.TotalAssignedJobs, minRunners, maxRunners)`
  (`autoScalerService.go` `scaleForAssignedJobCount`), i.e. the
  assigned-but-not-completed count is **server-authoritative**, read off every
  envelope rather than reconstructed client-side.
- Ack = advance `lastMessageId` on the next poll **and**
  `DELETE {messageQueueUrl}/{messageId}` (the official listener does both).
  Unacked messages replay after a session re-create — the queue, not the
  listener's memory, is the recovery source of truth.

### 2.3 Batch acquisition — the call that kills the fan-out

> **Live caveat (§2a-3):** on the current broker-host backend the explicit
> `acquirejobs` call below **does not exist** (404) — GitHub auto-assigns jobs
> to the scale set up to the polled `X-ScaleSetMaxCapacity`, delivering
> `JobAssigned` directly. The subsection stands as the documented ARC-era flow
> (and possibly the `pipelines.*` behaviour — U2′); either way acquisition is
> single-stream and 1:1.

```
POST /_apis/runtime/runnerscalesets/{id}/acquirejobs?api-version=6.0-preview
Authorization: Bearer <messageQueueAccessToken>     ← queue token, not admin JWT
Body:     [10234, 10235, 10241]                     ← runnerRequestIds from JobAvailable
Response: {"count": 2, "value": [10234, 10241]}     ← the subset actually acquired
```

One listener claims an arbitrary **batch** in one transactional call; the
response is the authoritative list of wins. `JobAssigned` messages then confirm
each assignment. Because each job is enqueued **once** in the scale set's
**single** serialized queue and claimed by its **single** session, there are no
sibling deliveries, no per-delivery completion fan-out, and nothing to
reconcile — the entire Q260/Q247-completion/Q259-recycle class cannot occur.
The N racing consumers of the classic protocol become 1 listener that
demultiplexes into N runners *after* acquisition.

### 2.4 Job payload and per-job data plane: delivered to the runner, not the listener

**`acquirejobs` returns only ids — the listener never sees the job payload.**
The pipeline job message (`AgentJobRequestMessage`, including the
`SystemVssConnection` endpoint and its job-scoped OAuth token) is delivered to
the **ephemeral runner itself** after it boots with its JIT config and opens its
*own* broker session (`BrokerServer.cs`: `CreateSessionAsync` →
`GetRunnerMessageAsync` → `AcknowledgeRunnerRequestAsync` → `Runner.Worker` →
`DeleteSessionAsync`). The runner renews its own lock and reports its own
completion. The listener is purely a **control-plane accountant** (claim +
count); the data plane is runner ↔ service directly. Security consequence ARC
documents and GAG inherits: the App/PAT token is never passed to the runner pod.

This forces a GAG worker-contract change: today's worker runs `Runner.Worker`
directly via the M3 spawnclient/pipes handoff with the payload the AGC already
acquired ([`cmd/worker/main.go`](../../cmd/worker/main.go)). Under the scale-set
protocol there is no payload to hand off — the worker pod must run the **full
runner** (`run.sh --jitconfig <blob>`, which the default
`ghcr.io/actions/actions-runner` image already supports) and pull its one job
through its own session. The entrypoint wrapper survives for proxy-CA trust
setup; the pipes handoff and the AGC-side renew loop do not.

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

`PROBE_SCALESET_TEST=true` in [`cmd/probe`](../../cmd/probe/scaleset.go) runs the
full chain against real GitHub with only App credentials — registration-token →
RemoteAuth hop → runner-group lookup → throwaway scale set → session → queue
long-poll → acquire-shape probes → `generatejitconfig` → full cleanup.
`PROBE_SCALESET_JOB_TEST=true` additionally waits for a real job (queued by the
dispatch-only [`scaleset-probe.yml`](../../.github/workflows/scaleset-probe.yml)
fixture, `runs-on: gag-probe-scaleset`). Runbook: export `GITHUB_APP_ID`,
`GITHUB_APP_PRIVATE_KEY`, `GITHUB_APP_INSTALLATION_ID`, `GITHUB_ORG_URL`; logs
prefix `INVESTIGATION-E:`; tokens and the JIT blob are never logged.

Findings (all live, `actions-gateway` org / `github-actions-gateway` repo):

1. **U1 resolved — the auth chain works with GAG's App, at org AND repo
   scope.** Registration token (132 chars) → `POST /actions/runner-registration`
   with `Authorization: RemoteAuth …` → 200 with tenant URL + admin JWT
   (~1 KiB). Scale-set create / session / `generatejitconfig` / deletes all
   succeed (200/204). No extra App permissions were needed beyond what the
   dogfood App already has.
2. **The tenant is the broker host, not `pipelines.*`.** The admin connection
   returned `actionsServiceURL = https://broker.actions.githubusercontent.com/rest`
   — a newer backend than the ARC-era sources describe. The session's queue URL
   is `{broker}/scalesets/message` (no query params; the queue token is the
   identity).
3. **The backend AUTO-ASSIGNS jobs — no acquire call exists.** The headline
   deviation from §2.3: `POST …/acquirejobs` returns a router-level
   `404 page not found` at every plausible location (Actions Service path with
   queue token — the official client's exact construction — admin token, and
   the `/scalesets/…` queue-base route), while sibling route
   `GET …/acquirablejobs` exists (204 when empty). With a job queued on the
   scale set's label, the queue delivered **`JobAssigned` directly ~1 s after
   session creation** — no `JobAvailable`, no client claim, `runnerRequestId: 0`,
   a `jobId` UUID, `scaleSetAssignTime` stamped, `statistics.totalAssignedJobs: 1`.
   Admission control is the **`X-ScaleSetMaxCapacity` header** the poll
   advertises (the probe sent `1`). Open sub-question (§5 U2′): whether
   `JobAvailable` + an acquire step reappears when queued jobs exceed the
   advertised capacity, and whether `pipelines.*` tenants still serve the
   ARC-documented explicit-acquire flow.
4. **Delivery is cursor-based, at-least-once.** An unacked message is
   redelivered with the same `messageId` (100000001) on the next
   `lastMessageId=0` poll. Empty long-poll: held ~51 s → `202`, no body.
5. **U4 partial — no rate-limit headers on the queue.** Only
   `X-GitHub-Request-Id`/`X-Github-Backend` observed; no `X-RateLimit-*`,
   no `Retry-After`. Steady-state cost is one ~50 s long-poll per scale set.
6. **Runner-group policy gates org-scoped scale sets — a real operational
   constraint.** With the scale set in the org's `Default` group
   (`allows_public_repositories: false`, the GitHub default), a job from this
   **public** repo was never routed — three-minute windows expired twice with
   `totalAvailableJobs: 0`. Registering the scale set **repo-scoped** (config
   URL = the repo) bypasses runner groups entirely and the pre-queued job
   delivered instantly. GAG must document this per-scope behaviour for tenants
   on public repos (mirrors the classic dogfood setup, which is repo-scoped).
7. **JIT config shape confirmed**: base64 blob (~4 KiB) decoding to top-level
   keys `.runner`, `.credentials`, `.credentials_rsaparams` — the same
   credential family the classic registrar parses today.

**Design consequence for GAG:** the listener tier gets *simpler* than §3
sketched. On this backend the admission gate (Q59) is literally the
`X-ScaleSetMaxCapacity` header — advertise free worker slots and GitHub assigns
at most that many jobs, exactly once each; there is no batch-claim bookkeeping
at all. The `JobAssigned` statistics count is the authoritative
provision-target, matching the ARC `clamp()` model (§2.2).

## 3. Delta from today's classic machinery

What Option E discards, reworks, carries over, and improves — against the
concrete code surface:

### Discarded (the classic-protocol acquisition tier)

| Today | Where | Why it goes |
|---|---|---|
| Classic broker client: `CreateSession`/`GetMessage`/`AcquireJob`/`RenewJob`/`CompleteJob`/`DeleteSession` | [`broker/client.go`](../../broker/client.go) | replaced by the scale-set client — plausibly **vendored `actions/scaleset`** rather than reimplemented (§5 decision) |
| Agent pool: N pre-registered JIT agents, per-agent Secrets, single-use recycle + heal ladder (Q114) | [`cmd/agc/internal/agentpool/`](../../cmd/agc/internal/agentpool/pool.go) | no per-agent registration exists; one scale-set object + per-job JIT configs |
| Multiplexer: `maxListeners` sessions, `SpawnReplacement`, poller accounting (Q152), planID `claimJob` dedup (#512) | [`multiplexer.go`](../../cmd/agc/internal/listener/multiplexer.go) | one session per group; nothing to dedup |
| Listener goroutine: per-delivery `handleJob`, self-heal ladder, `StartRenewLoop` (Q247), `completeAbandonedDelivery` (#513) | [`goroutine.go`](../../cmd/agc/internal/listener/goroutine.go) | acquisition is batch-claim + message dispatch; the runner renews/completes its own job |
| M3 pipes handoff: payload Secret → wrapper → `Runner.Worker spawnclient` | [`cmd/worker/main.go`](../../cmd/worker/main.go), provisioner payload staging | no payload at the listener; worker runs `run.sh --jitconfig` (§2.4) |
| Q260 fan-out accounting model + tests | [`broker/brokertest/server.go`](../../broker/brokertest/server.go) | models a race class that no longer exists; a new scale-set fake replaces it |

### Reworked

- **Admission gate (Q59) — gets *simpler and stronger*.** Today the gate skips
  `acquirejob` per delivery and the provisioner backstops post-acquire. Under
  scale-set, capacity gating is *the batch size*: acquire only as many
  `runnerRequestId`s as there are free worker slots; unacquired jobs stay
  queued at GitHub with zero AGC-side state, and `X-ScaleSetMaxCapacity`
  advertises the ceiling upstream. `priorityTiers` re-expresses as
  which-tier-the-next-pod-gets at provision time — unchanged mechanics, driven
  by `JobAssigned` count instead of per-delivery arrival.
- **Registration/auth:** the `GithubRegistrar` family shrinks to the two-hop
  bootstrap + scale-set CRUD + per-job `generatejitconfig`; the token manager
  gains two new lifecycles (admin JWT, queue token).
- **CRD surface:** `maxListeners` loses its meaning (acquisition concurrency is
  the batch, not a session count); `runnerLabels` collapses to the scale-set
  name (see §4 cost). A per-group protocol selector (or v2-only adoption) is
  the migration lever — §6.
- **Eviction retry:** detection stays pod-phase-based and the rerun-API path
  carries over; the "stop renewal to fast-cancel" half becomes "the runner
  process died with the pod, GitHub's lock lapses on its own" — the fast-cancel
  latency is now GitHub's lock TTL, not GAG's choice (probe item, §5).

### Carried over intact

Provisioner pod-building (pod template merge, security invariants, quota retry,
reaper, `completedPodTTL`/`pendingPodDeadline`), the priority-tier ceilings, the
egress proxy path (the listener's Actions Service traffic and the worker's
session both route through the per-tenant proxy exactly as today), the
`githubapp` token provider, the GMC tier, and the worker image (already the ARC
default `ghcr.io/actions/actions-runner`, which ships `run.sh --jitconfig`).
The JIT-credential-in-worker-Secret surface is **not new** — the provisioner
already stages `encoded_jit_config` into the worker Secret today
([`provisioner.go:150`](../../cmd/agc/internal/provisioner/provisioner.go)).

### Improves

- **The Q260/Q247-completion/Q259 race class is gone by construction** — no
  dedup key, no sibling completion, no recycle-422 seam.
- **Density and rate-limit budget at rest and under burst:** one queue session
  per group (~72 polls/h) at *all* load levels, vs classic's burst climb toward
  `maxListeners` sessions × 72/h. The [§3.5 rate-limit
  ceiling](../design/03-api-contracts.md#35-github-api-rate-limit-budget) stops
  scaling with acquisition concurrency.
- **No agent-recycle churn:** the Q114 recycle (2 REST calls + Secret rewrite +
  session re-create per completed job) disappears; per-job cost becomes one
  `generatejitconfig` call.
- **Recovery model:** unacked queue messages replay on session re-create;
  `statistics` give an authoritative assigned count to reconverge on after an
  AGC restart — strictly better than classic's in-memory session registry.

## 4. Honest cost list (delta vs the Q260 §4E estimate)

The Q260 doc priced Option E as "reverse-engineering and depending on a second
GitHub-internal protocol." **That cost has materially dropped**: the protocol
now has an official, supported Go client (`actions/scaleset`, Public Preview,
MIT-licensed) that may be directly vendorable. The remaining real costs:

1. **Worker handoff rework (forced, was not in the Q260 cost list).** The M3
   pipes/`Runner.Worker` handoff cannot survive — the payload never reaches the
   listener (§2.4). The worker becomes a full runner with a JIT config.
   Consequences: the wrapper keeps only its proxy-CA duty; per-job Secrets
   carry a JIT blob instead of a payload; job-start latency adds one
   session-create + message round-trip from inside the pod (probe item §5).
2. **`runs-on` label regression.** A scale set matches
   `runs-on: <scale-set-name>` only. GAG's `runnerLabels []string`
   (multi-label matching) cannot be expressed; migration needs a
   one-label-per-group story and tenant comms. This is a *user-visible* API
   change, not just internals.
3. **Single acquisition session per group (inherent).** The Q260 doc's SPOF
   concern stands, softened: the AGC is already `replicas: 1`, so process-level
   SPOF is the status quo; what is lost is N independent sessions' redundancy
   *within* the process (Q137 revival per listener). Recovery becomes session
   re-create + queue replay, which is well-defined (§3 Improves). Hot-hot
   listener HA is impossible by protocol design — that constraint *is* the
   fan-out fix.
4. **Auth/registration scope limits.** Org-scope App permissions: Organization
   "Self-hosted runners: RW" (GAG's App model fits). Repo-scope needs
   Repository "Administration: RW" — **broader than today's repo-scope JIT
   registration**; verify per install. **Enterprise scope is PAT-only** (no App
   auth) — GAG is App-based, so enterprise-scope gateways would be unsupported
   or need a PAT credential mode (decision, §5).
5. **GHES floor rises to 3.9** (scale-set support begins there). Classic
   machinery would need to survive for older GHES, or the floor is documented.
6. **Public Preview instability.** GitHub says interfaces "may change." GAG
   would pin a vendored version; wire changes are a live risk shared with ARC
   (mitigation: GitHub now treats third-party listeners as a supported
   audience, which is the opposite of the classic protocol's posture).
7. **Identity shift (unchanged from Q260 §4E).** "Thousands of goroutine-backed
   virtual runners" retires; the story becomes "a lighter-weight ARC listener
   with GAG's isolation + scheduling." Density at rest actually improves; the
   marketing/positioning docs (why-gag, vs-ARC) need a rewrite in the same PR
   that flips the default.

**Security check (secure-by-default):** no property regresses. Egress isolation
holds (all new endpoints are GitHub-hosted; both listener and worker traffic
stay behind the proxy). Workers still never see the App token. The JIT
credential in the worker Secret is today's surface, unchanged. The admission
gate strengthens (§3 Reworked).

## 5. Load-bearing unknowns

Each is marked **probe** (a `cmd/probe` scenario answers it live), **decision**
(the user/design owns it), or **✅ probed** (settled by Investigation E, §2a).

| # | Unknown | Kind |
|---|---|---|
| U1 | Full auth chain with **GAG's GitHub App**, org and repo scope. | **✅ probed** — works at both scopes with the existing App permissions (§2a-1). New constraint found instead: org-scope routing is gated by runner-group policy (`allows_public_repositories`) — repo scope bypasses it (§2a-6). |
| U2 | Wire details: 202 semantics + poll cadence, `X-ScaleSetMaxCapacity` effect, `acquirejobs` responses, message replay. | **✅ probed** — 202 after ~51 s hold; cursor-based at-least-once redelivery; and the headline: the broker-host backend **auto-assigns** (JobAssigned direct, no acquire call — every acquire route 404s), gated by `X-ScaleSetMaxCapacity` (§2a-3/4). |
| U2′ | Does `JobAvailable` + an explicit acquire step reappear when queued jobs exceed the advertised capacity? Do `pipelines.*` tenants still serve the ARC-documented explicit-acquire flow (version skew a GAG client must tolerate)? | **probe** (capacity-0 / multi-job variant of the §2a scenario) |
| U3 | Does an ephemeral runner started with `run.sh --jitconfig` behind the egress proxy (proxy CA via wrapper) receive its job and complete it — and what is the added job-start latency vs the pipes handoff? | **probe** (needs a pod; Tier-A/kind, not a bare probe binary) |
| U4 | Rate limits on the Actions Service tenant. | **partially probed** — no rate-limit headers on the queue at all (§2a-5); sustained-load behaviour unknown until P4. |
| U5 | Eviction fast-cancel: how quickly does GitHub cancel a job whose runner pod died (lock-lapse latency), and does the rerun-API retry path behave as today? | **probe** (live cluster) |
| U6 | Vendor `actions/scaleset` vs reimplement in `broker/`-style? (Vendoring: MIT license, faster, tracks GitHub; reimplementing: fits GAG's client conventions, no Preview-API churn exposure.) **Probe data point:** the official client's static `acquirejobs` construction 404s on the broker-host tenant — vendoring does not exempt GAG from backend skew. | **decision** |
| U7 | Migration surface: new v2-only acquisition (RunnerSet gains a protocol/label field) vs per-group selector on v1alpha1? Interacts with the `runnerLabels` collapse (§4.2). | **decision** |
| U8 | Enterprise-scope (PAT-only) and GHES <3.9: drop, document, or keep classic machinery alive as a legacy mode? Also now: org-scope requires runner-group public-repo policy handling (§2a-6) — document or default to repo-scope registration. | **decision** |

## 6. Phased execution path (no big-bang rewrite)

Mirrors the M1 → M2 pattern that built the classic tier: probe first, then a
parallel implementation behind a flag, then cutover.

- **P0 — this spike.** This doc. No code.
- **P1 — wire probe (S). ✅ DONE 2026-07-04** — the `PROBE_SCALESET_TEST=true`
  scenario in [`cmd/probe`](../../cmd/probe/scaleset.go) plus the
  `scaleset-probe.yml` dispatch fixture. Settled U1/U2 and half of U4 (§2a);
  surfaced the auto-assign model and the runner-group gate. Residual: the U2′
  capacity-overflow variant.
- **P2 — scale-set client package (M).** Decide U6; land the client (vendored
  or new module) + a `scalesettest` fake modelled on
  [`brokertest`](../../broker/brokertest/server.go), encoding the queue/ack/
  batch-acquire semantics P1 confirmed. Unit + envtest coverage; no AGC wiring.
- **P3 — parallel acquisition tier behind a flag (L).** A scale-set listener
  (one goroutine per group: session + poll + batch-acquire + dispatch) and the
  `run.sh --jitconfig` worker path, selected per group (U7), **classic remains
  the default** (secure/stable-by-default). The Q260
  `TestAGC_Q260_FanoutCompletionReconciles` acceptance shape gets a scale-set
  twin: N concurrent jobs, every one concludes `completed`, zero dedup
  involved.
- **P4 — live validation (M).** Dogfood the flagged path on the GKE cluster
  (the Q224 concurrent matrix is the acceptance gate); settles U3/U5.
- **P5 — cutover + retirement (M).** Flip the default, migrate dogfood, retire
  the classic tier per U8, rewrite the positioning docs (§4.7), update
  [03-api-contracts §3.3](../design/03-api-contracts.md) and
  [02-architecture §2.2](../design/02-architecture.md) — the design docs, plus
  the operator docs per the
  [doc-update matrix](../development/doc-update-matrix.md).

Each phase lands independently; P1/P2 are useful even if Option A wins (protocol
knowledge + a fake that documents GitHub's dispatch model).

## 7. Test strategy

- **P1:** live probe log = the evidence artifact, findings folded back into §2
  (the M1 §8 pattern).
- **P2:** the `scalesettest` fake encodes claim-once semantics (an id acquired
  twice must fail the second claim), message replay, token expiry; the client
  is tested against it.
- **P3:** listener unit tests (batch gating vs capacity, assigned-count
  reconciliation, session re-create replay) + envtest (provision on
  `JobAssigned`, JIT Secret staging) + the fan-out-free acceptance twin.
- **P4:** the Q224 concurrent matrix green on the flagged path is the
  go/no-go for P5.

## 8. Sources

Fetched 2026-07-04; all public:

- **`actions/scaleset`** (official Go client, Public Preview): `client.go`,
  `session_client.go`, `types.go`, `config.go`, `common_client.go`,
  `listener/listener.go`.
- **`actions/actions-runner-controller`** @ `gha-runner-scale-set-0.10.1`:
  `github/actions/client.go` (scale-set endpoints, sessions, `AcquireJobs`,
  `GenerateJitRunnerConfig`, token refresh), `types.go`, `multi_client.go`;
  `cmd/ghalistener/`; `controllers/actions.github.com/` (AutoscalingRunnerSet /
  AutoscalingListener / EphemeralRunnerSet / EphemeralRunner);
  `docs/gha-runner-scale-set-controller/README.md`. Independently
  cross-checked against tag `v0.27.6` (`github/actions/{client,types}.go`,
  `cmd/githubrunnerscalesetlistener/{autoScalerService,
  autoScalerMessageListener, sessionrefreshingclient}.go`) — endpoints, token
  split, and message shapes agree. Note for future readers: on ARC `master`
  the `github/actions` client package is a stub — read a
  `gha-runner-scale-set-*`/`v0.27.x` tag or `actions/scaleset`.
- **`actions/runner`** @ `main`: `src/Runner.Common/RunnerDotcomServer.cs`
  (classic registration), `BrokerServer.cs` (the runner-side session that
  receives the job payload), `RunnerServer.cs`.
- **docs.github.com**: ARC "Runner scale sets" concepts, "Authenticate to the
  API" (App permission matrix; enterprise = PAT-only), "Deploying runner scale
  sets with ARC" (GHES ≥ 3.9).

Per the repo rule that source-inspection findings are unverified until
exercised end-to-end, §2's wire-level specifics were treated as unconfirmed
until the P1 probe ran. **Investigation E (§2a) has now live-confirmed the
registration/session/queue/JIT chain — and corrected the acquisition model on
the current backend** (auto-assign, no client acquire). Full probe logs:
`INVESTIGATION-E:` lines from the 2026-07-04 runs.
