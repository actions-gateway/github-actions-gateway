# Cross-module logging audit

**Status:** Theme A (F1) ✅ resolved (1.0-gate JSON-unification — see the Fix
below); Themes B–G filed to the [STATUS Queue](../STATUS.md) (Q86–Q89). Theme C
folds into Theme G.

A cross-module audit of every log call site in the repo (`cmd/agc`, `cmd/gmc`,
`cmd/worker`, `cmd/proxy`, `broker/`, `githubapp/`, `cmd/probe`). It found one
1.0-gating defect (mixed log formats on a single stream), a credential-leak
*surface* (no live leak, but uncapped upstream bodies wrapped into errors), and
a set of post-1.0 observability gaps. File:line references are verified against
`main` at audit time; re-verify before editing.

This plan is the detail behind k8s-best-practices §F F1 and STATUS rows
Q86–Q89. Queue Notes stay short; the evidence and fixes live here.

---

## Theme A — Format fragmentation (F1) — `1.0-gate`

✅ **Resolved** — the Fix below has shipped.

**The defect.** `slog.SetDefault` is never called anywhere, so `slog.Default()`
returns the stdlib **TEXT** handler. The controllers configure `zap` (JSON) via
`ctrl.SetLogger`, but the `slog` and `zap` worlds are never bridged, so a single
process emits **mixed zap-JSON + stdlib-text on one stream**. A JSON log
pipeline cannot parse the majority of an AGC pod's lines. This is why F5's
"structured JSON" promise (PR #151) is only half-effective — it is true only for
the minority of lines that go through the manager's `ctrl.Log`.

**Why it gates 1.0.** [release-1.0.md](release-1.0.md) §D says the slog+zap
mismatch is "recommended to resolve, gating only if it breaks log ingestion." It
*does* break ingestion: the busiest AGC code paths emit unparseable text.

**Evidence (verified):**

- AGC — these route through `slog.Default()` (TEXT):
  - `cmd/agc/main.go` passes `nil` for the provisioner logger
    (`NewProvisioner(mgr.GetClient(), m, nil)`) and the pod waiter
    (`NewInformerPodWaiter(mgr.GetCache(), nil)`); both fall back to
    `slog.Default()` (`provisioner.go` `logFor`, `podwaiter.go`
    `NewInformerPodWaiter`).
  - `RunnerGroupReconciler.Log` is never set in `main.go`, so it too falls back
    to `slog.Default()` (`runnergroup_controller.go`).
  - The busiest files log through these: `listener/{goroutine.go,
    multiplexer.go}`, `provisioner/{provisioner.go,podwaiter.go}`,
    `controller/runnergroup_controller.go`, `agentpool/pool.go`.
- GMC — `internal/controller/ipranges.go` logs via `slog.Default()` (TEXT) while
  the manager logs zap JSON. `ActionsGatewayReconciler.Log *slog.Logger` is a
  **dead field** (declared, never set, never read).
- Worker — `cmd/worker/main.go` uses `slog.Info/Warn/Error` package funcs =
  default TEXT handler.
- Proxy — `cmd/proxy/main.go` correctly uses `slog.NewJSONHandler` but with
  `nil` `HandlerOptions` (hard-coded info level, no level source).

**Fix (this PR).** Pick one shape — zap JSON — and route everything through it:

1. After `ctrl.SetLogger(...)` in **both** `cmd/agc/main.go` and
   `cmd/gmc/cmd/main.go`, call
   `slog.SetDefault(slog.New(logr.ToSlogHandler(ctrl.Log)))` so `slog.Default()`
   becomes a logr→zap bridge: a single JSON shape, a single level source
   (`--zap-log-level`). `logr.ToSlogHandler` is available in the vendored
   `go-logr/logr` v1.4.3.
2. Inject named logr-backed loggers into the provisioner, pod waiter, and
   reconciler instead of `nil`, so component lines carry a `logger=` name.
3. Remove the dead `ActionsGatewayReconciler.Log` field.
4. Give the worker a `slog.NewJSONHandler` via `slog.SetDefault` at startup.
5. Give the proxy a non-nil `slog.HandlerOptions` with a level read from the
   environment (`LOG_LEVEL`), so it has a level source like the controllers.

After this, a single `--zap-log-level` / `LOG_LEVEL` governs the whole process
and every line is parseable JSON.

---

## Theme B — Credential-leak surface (Q86) — `security`

✅ **Resolved** — a single shared redactor, `githubapp.SanitizeBody(body, max)`
(`githubapp/sanitize.go`), now caps **and** redacts every upstream body before
it is interpolated into an error or log. It strips GitHub token formats
(`gh[pousr]_`, `github_pat_`), JWTs, the JSON values of sensitive keys
(`access_token`, `refresh_token`, `token`, `encoded_jit_config`,
`client_secret`, `private_key`, `password`, `secret`), and long opaque base64
blobs; redaction runs before capping so a secret straddling the cap boundary
cannot survive in the tail. Applied at all sites: `runner_auth.go` (:248/:259),
`github_registrar.go` (generate-jitconfig + deregister), `broker/client.go`
(via `capBody`), and `cmd/probe/main.go`. `broker` reuses the helper rather than
duplicating redaction (the `broker → githubapp` module edge already existed via
`replace`; both are GitHub-domain libs and there is no standalone broker
binary). Note added to [05-security.md](../design/05-security.md) §5.2.

No **direct** secret logging exists anywhere — tokens, keys, and PEM are never
logged, and that posture must be preserved. But several error paths interpolate
the upstream HTTP **response body** into an error that callers log. Two are
**uncapped** and can carry credential material:

- `githubapp/runner_auth.go:248` — `"runner token endpoint returned %d: %s"`
  (status, **body**) — uncapped. `:259` — `"...missing access_token: %s"`
  (**body**) — uncapped, and this is the **200-OK path**: the OAuth
  token-endpoint body can carry credential material.
- `cmd/agc/internal/agentpool/github_registrar.go:97` — `"generate jit config:
  unexpected status %d: %s"` (status, **respBody**) — uncapped. The success body
  of this endpoint is the runner **JIT registration credential + RSA key**.
- `broker/client.go` (~239/252/385/418) — already capped to 200 bytes via
  `capBody(...)`; lower risk but still an unredacted body. Consider redaction,
  not just capping.
- `cmd/probe/main.go` (~389) — logs a full response body at Info (dev tool;
  lowest concern).

**Fix.** Cap (reuse/extend `broker`'s `capBody`) **and** redact obvious
token-shaped substrings before interpolating any upstream body into an error or
log. Start with the two uncapped sites. Reference and update
[05-security.md](../design/05-security.md). This is defensible as a "should-fix"
hardening item, not a confirmed live leak (these are normally GitHub error
JSON), but recommended before 1.0 given the project's credential-isolation
thesis.

---

## Theme C — No runtime verbosity control — folds into Theme G

`cmd/gmc/internal/controller/builder.go` `buildAGCDeployment` sets **no `Args`**
on the AGC container, so `--zap-log-level` is structurally undeliverable to a
running AGC. The proxy level is hard-coded (`nil` `HandlerOptions`). No
`LOG_LEVEL` env exists anywhere; `AGC_EXTRA_*` is test-only and global. Theme A
fixes the *single level source*; Theme G adds the operator-facing knob that
threads a level to the AGC + proxy. Tracked under Q89.

---

## Theme D + F — Hot-path INFO spam + weak correlation (Q87)

Done together — both touch the listener/multiplexer logging.

**D — demote per-session/per-job INFO to Debug** (at thousands of concurrent
sessions these dominate log volume):

- `listener/goroutine.go` ~131 ("listener goroutine started"), ~231 ("job
  message received"), ~216 (idle shutdown), ~175 (session expired; recreating).
- `provisioner/provisioner.go` ~280/303/314 (3 lines per provisioned pod).

**F — add correlation fields so one session→job→pod is traceable:**

- The multiplexer logger carries only `index` — add namespace/RunnerGroup
  (available at `NewMultiplexer` construction).
- `sessionId` is missing from the listener's base `.With()` context
  (`goroutine.go` ~100) and from the error/warn lines that matter most.
- `jobID`/`messageId` are inconsistent across handleJob/AcquireJob/renew.
- `pool.go` ~170 uses the global `slog` (drops all context).

Add namespace/group/sessionId/podName to the listener + multiplexer base
contexts (builds on Theme A's logger-injection).

---

## Theme E — Debugging blind spots (Q88)

Add targeted Debug logs where a stuck/failed path is currently silent:

- `provisioner/podwaiter.go` has **zero logs** — a session stuck waiting for pod
  completion (missed informer event, pod never terminal) produces no output;
  this is the single most likely "stuck session" cause. Add debug logs around
  register/resolve/WaitForCompletion.
- The multiplexer baseline-goroutine **restart/backoff** path
  (`listener/multiplexer.go` ~142–157) is silent; an operator sees repeated
  "exited with error" with no "restarting" signal — looks like a dead loop.
- GMC `reconcileResources` (`actionsgateway_controller.go`) runs ~12
  provisioning steps with no per-step logs; a stalled tenant shows one wrapped
  error, not which step. Add debug step markers.
- The GMC validating webhook
  (`internal/webhook/v1alpha1/actionsgateway_webhook.go`) returns excellent
  rejection messages but never **logs** them server-side — no audit trail of
  privileged-container/reserved-namespace attempts. Add a server-side log on
  rejection.
- Cert generation/renewal in the GMC is silent.

---

## Theme G — CRD-driven per-tenant log level (Q89) — post-1.0

Add `spec.logLevel` (enum `info|debug`, default `info`) to `ActionsGateway`
(`cmd/gmc/api/v1alpha1/actionsgateway_types.go`), threaded by the GMC into the
AGC + proxy **exactly** the way `spec.securityProfile` already flows:
`securityProfileOrDefault` → `SECURITY_PROFILE` env in `builder.go`
`buildAGCDeployment` (~line 618), read by the AGC in `cmd/agc/main.go`. Mirror
that for a `LOG_LEVEL` env (and an AGC `--zap-log-level` arg or env read).

This gives operators a per-tenant "crank one gateway to debug for a bug repro"
knob via `kubectl edit ag`, no GMC redeploy.

**Caveats:**

- Build **after** Theme A so there is a single level source to thread.
- It is a **rolling restart**, not hot-reload. True hot reload would need a zap
  `AtomicLevel` HTTP endpoint — out of scope.

**Docs to update** (per CLAUDE.md doc rules): `docs/design/03-api-contracts.md`
(new field), `docs/design/02-architecture.md` (prose), and
`docs/operations/tenant-onboarding.md` (operator-facing usage).
