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

## Follow-ups (filed separately)

- **Q136** — `runnergroup_controller.go` returns `RequeueAfter=reapAfter`, which
  is 0 when the RunnerGroup has no worker pods. If the permanent baseline exits
  *non-retriably* (pool-exhausted / unauthorized), the recovery at L317
  (`if mux.ActiveCount()==0 { mux.Start() }`) only fires on the next watch event —
  up to the 10h resync. Requeue when `ActiveCount < desired`.
