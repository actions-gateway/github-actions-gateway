# Q134 — e2e "no session registered" flake: root cause and fix

Intermittent failure of the AGC→fakegithub session-registration e2e specs
(`E2E_AGC_WorkerPodAdmittedWithNonNumericUserImage`, `E2E_AGC_SessionRegistered`,
`E2E_AGC_MultipleJobsQueued`). One spec per run times out (180–240s) on
`fakegithubActiveSessionsForOwner(...)` staying empty — the AGC never registers
a broker session with the shared in-cluster `fakegithub`.

## What it is NOT

- **Not a regression from `5e490d9` (Q134's own "fix", PR #226).** The identical
  failure (`worker_securitycontext_test.go:96`, "no session registered", 180s)
  occurred on `main` at `5bab2d50e` (run 27505615811), *before* the "last green"
  `ac54a78`. The apparent `ac54a78`-green / `5e490d9`-red bisect boundary is an
  artifact of an intermittent flake landing on a green run by luck. It predates
  the commit.
- **Not the gate added by `5e490d9`.** `WaitForRunnerGroupReconciled` waits for
  `RunnerGroup.status.observedGeneration`, which the reconciler sets
  (`runnergroup_controller.go:327`) *synchronously, right after `mux.Start()`
  returns*. `mux.Start()` only *spawns* the listener goroutine — it does not wait
  for `createSession` to succeed. So `observedGeneration` is set ~9s after the
  tenant is applied (CI timeline) while the session is still not registered; the
  gate passes in <1s and the 180s session wait then fails. The gate's premise
  ("observedGeneration ⇒ a broker session is imminent") is incomplete. It is
  harmless but did not address the flake.
- **Not DNS/NetworkPolicy.** `EnsureAgents` (agent registration, an HTTP
  round-trip to fakegithub) *succeeds* at reconcile time — that is what sets
  `observedGeneration` the gate observes. So AGC→fakegithub connectivity is fine
  when the session subsequently fails to register.

## Root cause

The listener's session-establishment broker calls have **no per-call deadline**.
`createSession` (`goroutine.go`) and the OAuth token exchange
(`refreshBrokerToken` → `githubapp.FetchRunnerOAuthToken`) run on the goroutine's
context, which derives from the **long-lived manager context** (cancelled only at
AGC shutdown). The per-agent `broker.Client` is built with `HTTPClient: nil`
(`BrokerConfig.HTTPClient` is never set in `cmd/agc/main.go`), so it falls back to
`http.DefaultClient` — which has no overall/read timeout.

Consequently, if fakegithub *accepts the TCP connection but is slow to respond*
— plausible for a single-replica service under 6-proc parallel CI load — the
goroutine blocks **inside a single `createSession`/token call indefinitely**
rather than failing and retrying. The Multiplexer restarts a baseline listener
every ~1s on a *returned* (retriable) error, but a wedged call never returns, so
no retry happens and the RunnerGroup never registers a session within the test
budget. The missing timeout is what converts transient slowness into a
budget-exhausting hang.

## Fix

Bound the two session-establishment broker calls with a per-call deadline
(`Config.ControlPlaneTimeout`, default 30s). A stalled call now fails fast and
*retriably*, so the Multiplexer restarts the baseline and retries — many attempts
fit inside the 180s budget. The `GetMessage` long-poll is deliberately left
unbounded (it holds the connection open for the broker's poll interval by
design).

- `cmd/agc/internal/listener/goroutine.go`: `Config.ControlPlaneTimeout` +
  `controlPlaneTimeout()` default; wrap `createSession` and `refreshBrokerToken`.
- Test: `TestListener_CreateSessionStallDoesNotWedge` — a broker that never
  responds to `CreateSession` makes `Run` return a *retriable* error well before
  the outer deadline, instead of hanging.

## CI validation and the AcquireJob extension

The first push (createSession + OAuth bound) got the suite **past session
registration** in CI: the next e2e run logged `job 1: picking a live session →
enqueuing onto session-4`, then still timed out at `job_lifecycle_test.go:179`
with `expected >= 1 new worker pods, have 0`. A job was enqueued onto a *live*
session but no worker pod spawned for 240s — the **same unbounded-control-plane
class at the next call site**: `AcquireJob` (the request between job delivery and
pod provisioning) also ran on the manager ctx with `http.DefaultClient`. It is
now bound by the same `ControlPlaneTimeout`; a stalled `AcquireJob` fails fast
and the poll loop re-acquires (its error is already handled as recoverable).

The e2e suite is **multi-flaky**, not single-cause. The same run also failed
`E2E_GMC_TenantProvisioning_ProxyConnectWorks` (`provisioning_test.go:282`, a
curl-through-proxy egress test) — a fast failure unrelated to AGC sessions or
this change. That is a separate flake outside this PR's scope.

## Recurrence after #231, and why diagnostics come next

The bounded-control-plane fix (#231) reduced but did **not** eliminate the flake.
It recurred on a `main` push at `bfd6409` (e2e run 27529447832, 2026-06-15
~07:05): `39 Passed | 1 Failed`, `worker_securitycontext_test.go:103` timed out
at 180s on "no session registered."

The CI timeline rules out AGC *startup* latency as the cause:

| time (UTC) | event |
|---|---|
| 07:02:38.7 | tenant CR applied |
| 07:02:50.5 | AGC Deployment ready **and** `WaitForRunnerGroupReconciled` passes (single poll) |
| 07:02:51.2 | session-wait `Eventually` (180s) starts |
| 07:05:51.2 | timeout — **zero** sessions in fakegithub for the whole 180s |

So the AGC was up within ~12s and the reconcile gate passed immediately; the
listener then failed to register a session for a full 180s. The unanswered
question is **why**, and the spec captured no AGC-side state on timeout — only
the bare fakegithub poll result. Two failure modes fit the evidence equally and
need different fixes:

1. **Recoverable-but-persistently-failing `createSession`** — fakegithub
   (single-replica, shared, under 6-proc parallel load) is slow/erroring, so the
   baseline restarts every `RestartDelay`+`ControlPlaneTimeout` and every attempt
   fails. An AGC code change does not help; the fix is test-side (load/capacity/
   isolation) or the slowness is legitimate.
2. **Non-retriable baseline exit** (`NonRetriableError`: pool-exhausted at startup
   / unauthorized) — `multiplexer.go` sets `permAlive=false` and does **not**
   restart; nothing revives it until a watch event, because `RequeueAfter=reapAfter`
   is 0 with no worker pods. This is exactly **Q137**, a deterministic controller
   bug, and would be the fix.

Without AGC logs the prior root cause (#231) was itself an inference, and it
recurred. Rather than ship a third speculative fix, this change makes the flake
**observable**: a failure-gated `AfterEach` on the three session-registration
specs dumps `utils.DumpAGCSessionDiagnostics` — RunnerGroup status
(`activeSessions`/conditions/`observedGeneration`), AGC pod logs (where the
listener logs broker-call errors), pod/Deployment descriptions, the namespace
event stream, and fakegithub's logs/description. The next CI recurrence then
shows which mode is occurring, and the targeted fix (Q137, or a test-side load
change) follows from evidence instead of a guess. Q134 stays open until that
evidence lands and a real fix produces multiple consecutive green runs.

## First diagnostic capture (PR #240 e2e, run 27533137868)

The diagnostics fired on their very first CI run — but on **Q135**
(`E2E_AGC_MultipleJobsQueued`), not Q134, and the captured state proves mode 2
(non-retriable baseline exit, the Q137 mechanism) is real in the wild. AGC log
timeline (`tenant-job-lifecycle`):

```
08:21:15  listener started   agentIndex=0 sessionId=session-2   (permanent baseline)
08:21:21  job received        agentIndex=0 messageId=2  → worker pod plan-2 created
08:21:21  listener started   agentIndex=1 sessionId=session-4
08:21:21  idle shutdown: 50 empty polls   agentIndex=1            (2nd listener gone)
08:21:25  worker pod completed phase=Failed
08:21:25  job finished; recycling single-use JIT agent  agentIndex=0
08:21:25  WARN listener goroutine exited with error:
          "post-job agent recycle: non-retriable: broker: unauthorized (HTTP 401)"  index=0
```

So the session registered fine; the failure is **downstream**:

1. After the first job, the single-use JIT **agent recycle** (Q114 re-register)
   gets **HTTP 401** from the broker → a `NonRetriableError`.
2. `multiplexer.go` does not restart a non-retriable exit (`permAlive=false`), so
   the permanent baseline (index 0) is gone.
3. Nothing re-reconciles to revive it. The dumped `status.activeSessions: 1` /
   `Ready: True` is **stale** — last written before the exit — exactly the
   **Q137** symptom (`RequeueAfter=reapAfter` is 0 with no live worker pods).
4. The 2nd queued job is never serviced → the 240s `MultipleJobsQueued` timeout.

This is a real, deterministic-looking bug, not load: the flake is the race
between picking up the 2nd job and the recycle-401 killing the baseline. Two
distinct defects sit underneath it:

- **The recycle 401 itself** — why does broker re-registration after a job return
  401 when the installation token is freshly valid (`validFor 1h`)? Likely an
  auth-context bug in the Q114 single-use recycle path (or a fakegithub stub
  gap). This is the *primary* Q135 defect and needs its own investigation.
- **Q137** — even granting a transient recycle error, the controller should
  revive the baseline promptly instead of leaving a dead listener with stale
  `Ready: True` until the 10h resync. Requeue when `ActiveCount < desired`.

Q134 (`worker_securitycontext`, "no session registered") did **not** recur in
this run — that spec passed — so its specific cause (first `createSession` never
succeeding, *before* any job) is still uncaptured and distinct from this
recycle-401 path. The diagnostics are now in place to catch it next time.

## Follow-ups (filed separately)

- **Q136 / Q137** — `runnergroup_controller.go` returns `RequeueAfter=reapAfter`,
  which is 0 when the RunnerGroup has no worker pods. If the permanent baseline
  exits *non-retriably* (pool-exhausted / unauthorized / **recycle 401**), the
  recovery at L317 (`if mux.ActiveCount()==0 { mux.Start() }`) only fires on the
  next watch event — up to the 10h resync. Requeue when `ActiveCount < desired`.
  Now **confirmed** by the PR #240 capture above as the revival half of Q135.
- **Recycle-401 (under Q135)** — the broker returning 401 on post-job single-use
  agent re-registration is the primary Q135 defect; investigate the Q114 recycle
  auth path. Flagged for a separate session — out of scope for this diagnostics PR.
