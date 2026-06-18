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

✅ **Resolved** — done together, since both touch the listener/multiplexer
logging.

**D — demoted per-session/per-job INFO to Debug** (at thousands of concurrent
sessions these dominate log volume):

- `listener/goroutine.go` — "listener goroutine started" (per spawn), "job
  message received" (per job), idle-shutdown, "healing stale session" (per
  heal), and "job finished; recycling single-use JIT agent" (per job) are now
  Debug.
- `provisioner/provisioner.go` — the three per-pod lines ("job Secret created",
  "worker pod created", "worker pod completed") are now Debug.

Genuinely operator-relevant lifecycle events kept at INFO: concurrency-ceiling
holds, quota-retry, eviction auto-retry, and the credential-rejection recycle
notices in `healSession` (recovery events, not steady-state churn).

**F — added correlation fields so one session→job→pod is traceable:**

- The multiplexer logger now carries `namespace`/`group` (woven on at the
  `NewMultiplexer` call site in `runnergroup_controller.go`); its
  listener-lifecycle lines add `index` beneath.
- The listener's per-goroutine logger now carries `sessionId` on its base
  context (woven on after session creation and rebound on every heal/recycle so
  it always reflects the live session), atop the existing
  `group`/`namespace`/`agentIndex`.
- The provisioner's per-job logger now carries `podName`, atop the
  `namespace`/`runnerGroup` from `logFor`.

Not addressed here (out of the hot-path scope, left as backlog): `jobID`/
`messageId` field-name consistency across handleJob/AcquireJob/renew, and
`pool.go`'s reconcile-path use of the package-global `slog` (drops context, but
fires on the reconcile loop, not the per-session hot path).

---

## Theme E — Debugging blind spots (Q88)

✅ **Resolved** — debug diagnostics added to every previously-silent stuck/failed
path. Operator-facing grep anchors documented in
[observability.md](../operations/observability.md) § Debug diagnostics.

- `provisioner/podwaiter.go` had **zero logs** — a session stuck waiting for pod
  completion (missed informer event, pod never terminal) produced no output;
  this is the single most likely "stuck session" cause. Debug logs now cover
  register/resolve/cancel in `WaitForCompletion` (`pod already terminal at
  registration`, `registered for pod completion`, `pod completion observed`,
  `pod wait cancelled before completion`), each carrying `namespace`/`name`.
- The multiplexer baseline-goroutine **restart/backoff** path
  (`listener/multiplexer.go`) was silent; an operator saw repeated "exited with
  error" with no "restarting" signal. Now emits `restarting after backoff`
  (with `index`/`delay`) and `restart aborted` at debug.
- GMC `reconcileResources` (`actionsgateway_controller.go`) ran ~12
  provisioning steps with no per-step logs; a stalled tenant showed one wrapped
  error, not which step. Now emits a `reconcileResources step` debug marker per
  step (V(1)).
- The GMC validating webhook
  (`internal/webhook/v1alpha1/actionsgateway_webhook.go`) returned excellent
  rejection messages but never **logged** them server-side — no audit trail of
  privileged-container/reserved-namespace attempts. Now logs `ActionsGateway
  admission denied` (operation/namespace/name/reason) on every rejection. This
  one is at **info**, not debug: an audit trail of denials must be visible by
  default, and admission denials add negligible volume.
- Cert generation/renewal in the GMC is no longer silent: `ensureProxyCert` and
  `ensureMetricsCerts` log the (re)issuance and the reason (`secret missing` /
  `unparseable cert` / `near expiry`) at debug, never any key material.

---

## Theme G — CRD-driven per-tenant log level (Q89) — post-1.0

✅ **Resolved** — `spec.logLevel` (enum `info|debug`, default `info`) added to
`ActionsGateway`. The GMC threads it to **both** the AGC and the egress proxy as
the `LOG_LEVEL` environment variable in `builder.go` (`buildAGCDeployment` and
`buildProxyDeployment` via `logLevelOrDefault`), exactly as `spec.securityProfile`
flows as `SECURITY_PROFILE`. The proxy already read `LOG_LEVEL` (Theme A); the AGC
now reads it too (`cmd/agc/main.go` `zapLevelFromEnv`) to set its zap level unless
an explicit `--zap-log-level` flag is passed — the GMC never stamps one, so in
production `LOG_LEVEL` is the sole level source. Because the env lives on the pod
template, flipping `spec.logLevel` is a **rolling restart** of the AGC and proxy
(not a hot reload). Default is `info`, never `debug`, so a CR omitting the field
never silently runs at debug verbosity. `debug` surfaces the Theme D/F
per-session/per-job correlation lines. Tests: envtest defaulting + enum rejection
(`crd_admission_test.go`) and the restart-on-change path
(`log_level_test.go`); builder unit tests for both Deployments
(`builder_test.go`); AGC level-mapping unit test (`main_test.go`). Docs updated:
[03-api-contracts.md](../design/03-api-contracts.md),
[02-architecture.md](../design/02-architecture.md),
[tenant-onboarding.md](../operations/tenant-onboarding.md), and
[observability.md](../operations/observability.md).

---

### Original plan

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
