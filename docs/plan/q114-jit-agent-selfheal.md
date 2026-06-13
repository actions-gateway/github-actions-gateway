# Q114 — AGC self-healing for single-use JIT agents

**Status: in progress.**

Primary source: [milestone-4.md §12](milestone-4.md#12-live-multi-tenant-validation-evidence-2026-06-1112),
bug 2 — live-found 2026-06-12 during the M4 validation run.

## Problem

GitHub deletes a JIT (just-in-time) runner record after it completes (or has
acquired a then-cancelled) job. The AGC assumes agents are long-lived: after a
job, the listener goroutine keeps long-polling `GetMessage` on the same
session with the same agent. Observed live:

- `GetMessage` degrades into a `200`-with-empty-body loop
  (`decode response: EOF`), later into `401 unauthorized` — forever. Neither
  is classified today: both fall into the generic `pollErrors` backoff loop in
  `listener.Run`, which never exits and never re-registers.
- The agent Secret still holds the dead agent's credentials, so an AGC
  restart reloads the stale agent and the baseline listener loops on
  401 at `CreateSession` (a `NonRetriableError` → baseline not restarted →
  reconcile restarts it → fails again).
- Manual recovery (delete agentpool Secrets + restart) then hit
  `409 Already exists` re-registering a *surviving* name (an agent that never
  ran a job is **not** deleted server-side, and its record's agent ID was lost
  with the Secret).

Net effect: every completed job permanently burns one agent; a tenant decays
to zero throughput after ~`maxListeners` jobs.

## Design

### Decision 1 — where the post-job re-register hooks in: the listener goroutine, with the pool owning the mechanics

After `handleJob` returns **and the job was actually acquired** (AcquireJob
succeeded — acquisition, not pod success, is what consumes a JIT runner), the
goroutine knows definitively its agent is spent. It self-heals inline:

1. Best-effort `DeleteSession` on the old session (usually already dead).
2. `cfg.RecycleAgent(ctx)` → the pool deregisters + re-registers the agent
   under the same name, rewrites the Secret, and returns a fresh `*Agent`.
3. Fetch a new broker OAuth token with the fresh credentials, `createSession`,
   reset poll counters, continue polling.

**Implementation note (revised during validation).** The controller factory
wires `RecycleAgent`/`MarkAgentConsumed` **unconditionally**, for every
registrar — not nil-for-stubs as an earlier draft of this plan implied. It has
to: the Tier B self-heal e2e exercises the recycle path through the
`StubRegistrar` (e2e never has real GitHub credentials), so gating it on
registrar type would leave the e2e unable to heal. It is also production-correct
— a JIT session always serves exactly one job, so re-registering after every
job is never wrong. The consequence is that **a session serves one job in all
configurations**: the pre-existing `E2E_AGC_MultipleJobsQueued` test, which
captured one session and enqueued two jobs onto it, was updated to deliver each
job onto a freshly-queried session (the captured session is recycled away after
the first). The `Config.RecycleAgent` field stays documented as nil-able (the
listener falls back to plain session reuse when it is nil — the path the
`TestListener_AcquireJobThenReuse` unit test still covers), it is just always
provided in the live wiring.

Why here and not the multiplexer or provisioner:

- The goroutine never exits, so **maxListeners accounting is untouched** — the
  slot stays occupied by a live poller, `ActiveCount` is stable, and none of
  the Q100 restart/permAlive logic is involved.
- The provisioner completion path doesn't know about agents or sessions; the
  multiplexer doesn't know about jobs. The goroutine is the only place that
  has both the "job acquired" event and the session lifecycle.
- The recycle *mechanics* (registrar calls, Secret rewrite, in-memory swap)
  belong to `agentpool.Pool`, which already owns registration and Secret
  lifecycle. The listener only sees a `RecycleAgent func(ctx) (*Agent, error)`
  closure, built in the controller factory (which has the `TokenManager` for
  the installation token), parallel to the existing `ReleaseAgent` closure.

### Decision 2 — 409 on a surviving name: delete-then-recreate, stable names

Names stay `<group>-<index>` (recomputed in both `pool.createAgent` and
`listener.createSession`; a name suffix would force storing per-agent name
state in the Secret and accumulate garbage entries in the GitHub runner UI).

`Pool.Recycle(ctx, agent, token)`:

1. `Deregister(storedAgentID)` — best-effort; the record is usually already
   gone (404 ignored).
2. `Register(name)` — on a new typed `*agentpool.NameConflictError` (HTTP 409
   from generate-jitconfig), the surviving record's ID is unknown (the
   partial-failure case: a previous Register succeeded but the Secret write
   crashed, or the Secret was manually deleted). Resolve it with a new
   registrar method `ResolveAgentID(ctx, token, name)` (GitHub:
   `GET …/actions/runners?name={name}`), `Deregister` the resolved ID, retry
   `Register` once.
3. Rewrite the agent Secret in place (same name/labels, fresh
   agentId/clientId/key/JIT blob) and swap the in-memory `byIndex`/`agents`
   entry under the pool lock, preserving the claim — the calling goroutine
   still holds the slot.

Registrar interface grows `ResolveAgentID`; `StubRegistrar` gets a
name-tracking implementation plus a test hook to inject a conflict.

### Decision 3 — classifying 401/EOF on GetMessage as "stale agent" vs outage

The poll loop gains a self-heal ladder that is deliberately *not* a new retry
loop — each rung falls back to the existing backoff/restart machinery:

- **401/403 (`UnauthorizedError`) from GetMessage** — first assume an expired
  broker OAuth token (~50 min lifetime; today this also loops forever):
  refresh the token and recreate the session. Only if `CreateSession` itself
  comes back unauthorized — proof the *agent* is dead, not the token — recycle
  the agent and recreate. Triggers on the first 401: the refresh+recreate
  attempt is cheap and correct for both causes.
- **200-with-EOF (decode error wrapping `io.EOF`/`io.ErrUnexpectedEOF`)** —
  GitHub's observed "your runner is gone" signature, but a one-off could be a
  network blip. After **3 consecutive** EOFs, treat as a stale session: same
  recreate → (recycle if unauthorized) path. Below the threshold, the existing
  generic backoff applies. No registration traffic from transient noise.
- **Real outage (5xx, transport errors, 429)** — untouched: existing generic
  backoff and rate-limit handling.
- **Heal failure** (recycle or recreate fails, e.g. GitHub down): the
  goroutine returns a *retriable* error and exits. The multiplexer's existing
  RestartDelay/backoff paces the retry (Q100's idempotent `Start` and
  `permAlive` logic apply unchanged — we add no competing restart loop).
- **Startup unauthorized** (covers "AGC restarted while Secrets hold consumed
  agents", where the in-memory consumed set is lost): if the initial broker
  token fetch or `CreateSession` fails unauthorized, attempt **one** recycle,
  then retry once; only then surface the existing `NonRetriableError` +
  `Degraded` condition. Requires a typed `githubapp.TokenExchangeError{StatusCode}`
  from `FetchRunnerOAuthToken` (today: untyped `fmt.Errorf`) so a 401 from the
  OAuth exchange is distinguishable from a transport failure.

Recycle attempts are bounded: at most one per heal attempt, and heal attempts
are paced by the existing backoff/RestartDelay — no tight registration loops
against GitHub during an outage that happens to return 401s.

### Decision 4 — maxListeners/pool accounting: a consumed agent never re-enters `available` un-recycled

The happy path (Decision 1) keeps the slot occupied, so nothing changes. The
risk is the *unhappy* paths: a goroutine that exits (error, Stop, cancel)
after consuming its agent currently `ReleaseAgent`s it straight back into
`available`, poisoning the pool — the next claimant inherits a dead agent.

- `Pool` gains a `consumed map[int]bool` (guarded by `p.mu`, preserved across
  `reload()` like `claimed`, in-memory only — restarts are covered by the
  startup heal above).
- The listener marks consumption via a `cfg.MarkAgentConsumed()` closure
  immediately after AcquireJob succeeds; a successful `Recycle` clears it.
- `ReleaseAgent` **parks** a consumed agent (clears the claim, does not
  re-add to `available`).
- `EnsureAgents` — which runs on every reconcile and already holds an
  installation token — recycles parked agents and returns them to
  `available`. Worker-pod phase changes trigger reconciles, so repair is
  prompt, and the slot the exited goroutine freed becomes claimable again.

## Code changes

| Where | What |
|---|---|
| `cmd/agc/internal/agentpool/pool.go` | `Recycle` method, `consumed` set, `MarkConsumed`, park-on-release, repair pass in `EnsureAgents` |
| `cmd/agc/internal/agentpool/github_registrar.go` | `NameConflictError` on 409, `ResolveAgentID` (list-runners-by-name) |
| `cmd/agc/internal/agentpool/stub_registrar.go` | name tracking, `ResolveAgentID`, conflict-injection hook |
| `cmd/agc/internal/listener/goroutine.go` | post-job recycle, poll-loop heal ladder, startup heal, `Config.RecycleAgent`/`Config.MarkAgentConsumed` |
| `cmd/agc/internal/listener/metrics.go` | `actions_gateway_agent_recycles_total{namespace, runner_group, trigger}` (trigger ∈ `post_job`, `stale_session`, `startup`, `reconcile_repair`) and `actions_gateway_agent_recycle_errors_total{namespace, runner_group}` |
| `cmd/agc/internal/controller/runnergroup_controller.go` | factory wires `RecycleAgent`/`MarkAgentConsumed` closures (pool + TokenManager) |
| `githubapp/runner_auth.go` | typed `TokenExchangeError{StatusCode}` |
| `broker/client.go` | no behavior change; possibly a small helper/typed wrap for decode-EOF detection if `errors.Is` proves awkward |
| `test/fakegithub/main.go` | single-use simulation (below) |

No new long-lived goroutines are introduced (recycle is inline in the
listener goroutine; repair is inline in reconcile), so no new done-channel
surface is needed; if that changes, the repo's done-channel convention
applies.

## fakegithub: single-use simulation

So the death-spiral and the fix are reproducible at Tier A/B without real
GitHub:

- **Runner registry + registration API** at GHES-style paths
  (`/api/v3/orgs/{org}/...` and `/api/v3/repos/{owner}/{repo}/...`, since
  `GithubRegistrar.apiBase()` derives `{host}/api/v3` for non-github.com
  hosts):
  - `POST .../actions/runners/generate-jitconfig` — mints an agent ID and an
    RSA-2048 key, returns a real JIT blob (`.runner` with `serverUrlV2` →
    fakegithub base URL, `.credentials` → `/token`, `.credentials_rsaparams`);
    **409** when the name already has a live record.
  - `DELETE .../actions/runners/{id}`; `GET .../actions/runners?name={name}`.
- **Sessions bind to agents**: `POST /session` parses `agent.id`. Unknown IDs
  are implicitly registered, so existing StubRegistrar-based Tier B/C flows
  keep working unchanged.
- **Single-use enforcement (opt-in)**: `/acquirejob` is linked back to the
  delivering session via the message's `runnerRequestId` (injected into the
  enqueued body by `/control/enqueue` when the test omits it); on acquisition
  the session's agent is consumed — the runner record is deleted, the old
  session's next `GET /message` returns **200 with an empty body** (the EOF
  signature) and **401** from then on, and `POST /session` for the dead agent
  ID returns 401. Enabled via `SINGLE_USE_RUNNERS=true` or
  `POST /control/singleuse`.

Enforcement is **default OFF**: fakegithub's job queues are per-session,
while real GitHub re-queues an unacquired job pool-wide — existing Tier B
tests that enqueue two jobs onto one session (`job_lifecycle_test.go`) would
lose the second message when the first acquisition kills the session. A
dedicated Tier B test toggles enforcement on, runs a job to completion, and
asserts a fresh session replaces the consumed one and a second job still
completes — proving the self-heal loop on a real cluster. fakegithub also
gets its own module-level HTTP test covering register → consume → EOF/401 →
409-on-re-register.

## Tests

Unit (listener, httptest pattern as in `goroutine_test.go`; all listener +
agentpool packages run under `make test-race`):

1. Post-job recycle: job completes → registrar `Register` called again, old
   session deleted, new session created, polling continues on the same
   goroutine (no exit), second job served.
2. GetMessage 401 with a *live* agent (expired-token case): token refreshed,
   session recreated, **no** recycle.
3. GetMessage 401 with a dead agent: recreate 401s → recycle → fresh session.
4. EOF threshold: 2 consecutive EOFs then recovery → no heal; 3 → heal.
5. Heal failure: registrar down → goroutine exits retriable; consumed agent is
   parked, not returned to `available`.
6. Startup heal: stale credentials at startup → one recycle → success; and
   recycle failure → `NonRetriableError` as today.

Unit (agentpool):

7. `Recycle` happy path: Secret rewritten, in-memory swap, claim preserved.
8. 409 path: `NameConflictError` → `ResolveAgentID` → deregister → retry OK.
9. `ReleaseAgent` parks consumed agents; `EnsureAgents` repairs them;
   `reload()` preserves the consumed set.
10. `GithubRegistrar`: 409 → typed error; `ResolveAgentID` request/parse.

Tier B (kind): a dedicated e2e test toggles `/control/singleuse` on, runs a
job to completion, and asserts the consumed session is replaced by a fresh
one and a second job completes (the pre-fix AGC would stall forever).

## Docs

- `docs/design/02-architecture.md` — session lifecycle prose (post-job
  re-registration, heal ladder) + metrics table rows.
- `docs/design/04-operational-flows.md` — job flow gains the post-job
  re-register step; new self-heal flow.
- `docs/design/07-test-plan.md` — single-use simulation in the Tier B
  criteria.
- `docs/operations/troubleshooting.md` — runbook: symptom on pre-fix versions
  (sessions stuck in `decode response: EOF` / 401 GetMessage loops, runner
  list emptying, tenant throughput decaying to zero after ~maxListeners
  jobs), cause, remediation (upgrade; interim: delete agentpool Secrets +
  restart AGC pod, expect 409s for surviving names).
- `docs/operations/observability.md` — new metrics.
- `docs/plan/milestone-4.md` §12 — note the fix.
- `docs/STATUS.md` — drop Q114 row (isolated commit).

## Progress

- [x] Plan written
- [x] agentpool: Recycle + consumed set + registrar additions
- [x] listener: post-job recycle + heal ladder + startup heal
- [x] controller factory wiring
- [x] githubapp: typed token-exchange error
- [x] fakegithub single-use simulation (+ owner-scoped toggle, session owner filter)
- [x] unit tests (race-clean) + Tier B spec `E2E_AGC_SingleUseSelfHeal`
- [x] docs
- [ ] make check green, Tier B run, PR
