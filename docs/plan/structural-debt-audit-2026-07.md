# Structural debt audit — 2026-07

A read-only audit of the whole codebase for technical debt, tangled control flow,
readability, god functions, and oversized files. Run 2026-07-20 against
`main` @ `d6874e2`. This doc holds the *evidence and rationale* for the Queue rows
the audit produced, so each row can stay inside the 250-char cap without dropping
a finding (per [maintaining-backlog.md](../development/maintaining-backlog.md)).

Classification follows [technical-debt.md](../development/technical-debt.md).

> **Status: ⚠️ Partial — filed 2026-07-20; F1 and F2 shipped.** The F1 Secret leak
> was fixed and merged the same day (Q373, #727); F2 (the probe rewrite) shipped as
> Q362. The remaining findings are tracked by Queue rows
> [Q364](../STATUS.md#Q364)–[Q370](../STATUS.md#Q370) and [Q374](../STATUS.md#Q374);
> [Q371](../STATUS.md#Q371) adds the prevention gates; [Q372](../STATUS.md#Q372)
> (Deferred) carries the re-run trigger.
>
> The ID range is not contiguous because concurrent branches allocated IDs while
> this audit was in flight: Q361 went to a CI-latency item (#722) and Q363 to a
> manifest-validate flake (#729), so this audit's F1 and F3 became Q373 and Q374.
> See [§Audit cadence](#audit-cadence) — `Next ID` cannot stay correct under
> parallel work, so open PRs are the authority.

## Headline

The codebase is **healthy overall**. 41,011 non-test, non-generated Go lines
against 54,687 test lines (1.33:1); exactly **one** `TODO` marker repo-wide; only
21 functions over 100 lines and 5 over 200. Comment quality is a genuine asset —
comments overwhelmingly explain *why a bound exists and what breaks without it*,
and the Q-ID cross-references pay off when reading cold.

The debt is **concentrated and structural**, not pervasive erosion. It clusters in
three places:

1. **Version boundaries** — v1/v2 in GMC, classic/scale-set in AGC, v2alpha1/v2beta1
   in `api/`. The forks are mostly deliberate; the *mechanical* copy-paste riding
   along with them is not.
2. **Wiring entry points** — two god `main`/`run` functions that grew past the
   point where the extraction pattern they started with was abandoned.
3. **Test doubles and probes** — four independent hand-rolled implementations of
   one wire protocol.

## Findings

### Fixed as a bug, not flagged as debt

**F1 — ScaleSet worker leaks a JIT-config Secret per job.** ✅ **Shipped** (Q373, #727)

`ProvisionScaleSetWorker` ([provisioner.go:548](../../cmd/agc/internal/provisioner/provisioner.go))
stages `job-ss-<jobID>` at :573, then has three failure exits (:585 ceiling held,
:597 scale-up throttle, :608 pod-create) — none delete it, and neither does the
success path. The classic `provision()` deletes on all five failure paths and on
success (:436, :446, :464, :473, :488, :526).

Its doc comment delegates cleanup: *"Per-job Secret cleanup in steady state is the
caller's responsibility (the reconciler wiring that drives this method)."* The
caller is a one-line closure at
[runnerset_scaleset.go:170](../../cmd/agc/internal/controller/runnerset_scaleset.go)
that does no cleanup. `grep -rn 'job-ss'` returns exactly two hits — the
construction site and one test. `runner_shared.go`'s reaper contains **zero**
Secret references.

Effect: credential-bearing Secrets accumulate one-per-job for the RunnerSet's
lifetime, reclaimed only by cascade-GC on RunnerSet deletion — on what is now the
**default** acquisition protocol. This is a documented-as-handled contract with no
implementer, which is why it is a bug rather than debt.

Remedy: mirror `provision()`'s cleanup on each failure exit, and delete on the
listener's terminal `JobCompleted` (`scalesetlistener/listener.go` `completeJob`,
which today only bumps a metric).

**Shipped in #727** — the reclaim went in at the listener's terminal-completion
seam (`ReclaimJobSecret`), not just on the failure exits, so the steady-state
success path is covered too. The `IsAlreadyExists` replay path deliberately does
**not** delete: a replayed job's pod may still be mounting the Secret. Covered by
an internal provisioner test, a listener-seam test, and an envtest integration
test.

### High

**F2 — `cmd/probe` reimplements the `scaleset` package it exists to validate.** → Q362

`cmd/probe` never imports `scaleset` (verified: no import in any non-test file).
[scaleset.go:187-208](../../cmd/probe/scaleset.go) declares lowercase shadow copies
of five exported library types — `runnerScaleSet`, `runnerScaleSetSession`,
`runnerScaleSetMessage`, `jitRunnerConfig`, `adminConnection` — mirroring
`RunnerScaleSet`, `RunnerScaleSetSession`, `RunnerScaleSetMessage`,
`JITRunnerConfig`, `AdminConnection` in [scaleset/types.go](../../scaleset/types.go).
It hand-builds raw requests rather than calling `scaleset.Client`.

Why it matters beyond duplication: the probe's *purpose* is validating the protocol
the `scaleset` package speaks. Because it speaks its own dialect, a divergence
between library and wire is invisible in exactly the case the probe exists to catch.

Remedy: rewrite against `scaleset.Client`; where the probe needs raw-wire
visibility the client hides, add a response hook to the client rather than keeping
a parallel implementation. Highest-leverage deletion available — most of the 1,048
lines go away.

**Shipped (Q362).** The probe now drives `scaleset.Client` end to end; all five
shadow types and every hand-built modelled request are gone. The remedy's response
hook is `scaleset.ResponseObserver` (`Config.Observer`), which reports status,
headers, and latency for every response — including the 202 polls `GetMessage`
returns as `(nil, nil)`, which is where the probe's rate-limit evidence lives.
Three checks stayed outside the library on purpose, because delegating them would
have made the probe agree with itself: the raw-wire reporting above, the acquire
route/token matrix (the client's construction measured against alternatives it
does not implement, reached through the new `Client.RawServiceCall` escape hatch so
even those comparisons use the client's own auth), and the delivered
`acquireJobUrl` fallback — which now tries the client's static route *first* and
logs a `DIVERGENCE` when the delivered URL succeeds where it failed.

The "most of the 1,048 lines" estimate above was optimistic: the file lands at 847.
The reimplementation itself did go — the five shadow types plus `mintAdminConnection`
/ `registrationToken` / `adminConnection` / `svcCall` are 165 lines deleted outright,
and every remaining modelled call is a one-line client method where it used to build
a request — but three parts of the file were never protocol code and stay: the ~110-line
`PROBE_SCALESET_*` environment contract, the diagnostic matrix above, and a doc comment
that grew to record what the probe still asserts on its own.

**F3 — The v2 conditions sync gate covers 13% of the duplication it appears to.** → [Q374](../STATUS.md#Q374)

Modulo the version string, nine files are byte-identical across `api/v2alpha1` and
`api/v2beta1` — **2,550 lines**:

| File | Lines | Differing |
|---|---|---|
| `conditions.go` | 332 | 0 |
| `shared_types.go` | 246 | 0 |
| `scheduling_types.go` | 177 | 0 |
| `sidecar.go` | 83 | 0 |
| `zz_generated.deepcopy.go` | 879 | 0 |
| `actionsgateway_types.go` | 425 | 7 (storageversion marker) |
| `egressproxy_types.go` | 236 | 1 |
| `runnerset_types.go` | 344 | 101 (genuinely versioned) |
| `conversion.go` | 213 | 224 (genuinely versioned) |

[check-conditions-sync.sh](../../scripts/check-conditions-sync.sh) hardcodes two
paths (`alpha=`, `beta=`) and guards `conditions.go` alone — **332 of 2,550 lines**.
The other ~2,218 drift silently. The sync test concedes the design: *"There is no
generator; whoever edits one mirrors the other."*

Note `conditions.go` is **pure constants plus one helper function — zero kubebuilder
markers, zero API structs**, so it does not require per-version duplication at all.
Same for `sidecar.go` (0 types, 0 markers).

This inverts the project's own *"correct it twice, then automate it"* principle: the
gate automates enforcement of a copy that deletion would remove.

Remedy, cheapest first: (a) generalize the script to a file list covering all nine;
(b) move the non-API identical files into a shared internal package that both
versions type-alias (`type ProxyConfig = apishared.ProxyConfig` — aliases are
deepcopy- and conversion-transparent), leaving only `runnerset_types.go` and
`conversion.go` versioned.

**F4 — `githubCIDREgressRule` exists; the egress-proxy builder open-codes it twice.** → [Q364](../STATUS.md#Q364)

The shared helper at [builder.go:404](../../cmd/gmc/internal/controller/builder.go)
is called from three sites — but not from `egressproxy_builder.go`, which
open-codes the same peer-building loop at
[:470-476 and :487-493](../../cmd/gmc/internal/controller/egressproxy_builder.go).
The helper's own doc comment claims to be the shared one.

Three independent spellings of "which CIDRs may we reach on 443" in a
security-relevant allowlist builder is where a policy bug hides. Same failure class
as the PR #59 `ipBlock` trap the file's own comments warn about.

**F5 — Four hand-rolled implementations of one broker wire protocol.** → [Q368](../STATUS.md#Q368)

`broker/brokertest/server.go` (746) + `test/fakegithub/main.go` (1118) +
`cmd/agc/test/load/broker_stub.go` (373), plus the probe (F2).
`handleSession`, `handleMessage`, `handleAcquireJob`, `handleRenewJob` recur across
all of them — down to identical session-ID minting (`fmt.Sprintf("session-%d", …)`)
and the same bearer-token DELETE fallback. ~1,200 of ~2,240 stub-server lines look
recoverable.

These carry real production cost: `test/fakegithub` is built and published as a
container image, Dependabot-tracked, Trivy-scanned, and Dockerfile-linted — a
shipped product surface at 1,118 lines with its own 661-line test suite.

Related: `cmd/probe/compat/compat.go` is a non-`_test.go` file that imports
`net/http/httptest` and `broker/brokertest` into the module's regular build graph.
Its doc comment asserts it "never ships in a compiled artifact" — true today only
because no `package main` imports it, with nothing enforcing that. A check asserting
no `package main`-reachable file imports `httptest` would make the guarantee real.

### Medium-high

**F6 — Shared foundation lives in v1-named files; blocks the Q273 sunset.** → [Q365](../STATUS.md#Q365)

Most cross-version foundation sits in files named for v1 that v2 imports:

- `builder.go` — the constants block, `ptr`, `securityProfileOrDefault`,
  `hardenedContainerSecurityContext`, `nonrootPodSecurityContext`,
  `copyRecommendedLabels`, `dnsEgressRule`, `githubCIDREgressRule`,
  `buildAGCNetworkPolicyFrom`, `agcWorkloadNames`, `buildAGCDeploymentFrom`,
  `buildNoProxy`.
- `actionsgateway_controller.go` — `provisioningError`, `quotaCheck`,
  `footprintFromResources`, `evalProxyPoolQuota`, `quotaHeadroomViolations`,
  `resourceListEqual`, `quotaHardChangedPredicate`, `labelSafe`.

Same pattern in AGC: the `listener` package is treated as classic-only, but
`provisioner`, `runnerset_target.go`, `runner_shared.go`, and `main.go` all import
it for `listener.Metrics`, `JobHandlerFunc`, `AdmitFunc`, `EventRecorder` — types
with nothing to do with classic acquisition.

Consequence: **deleting v1 today is a multi-hundred-line refactor, not a `git rm`.**
Doing the moves *now*, while both versions are live, is mechanical and
behavior-free, and directly de-risks the already-blocked [Q273](../STATUS.md#Q273).
This is why the row sorts adjacent to Q273 rather than by its own severity.

### Medium

**F7 — ~29 near-identical `CreateOrPatch` wrappers across three GMC reconcilers.** → [Q366](../STATUS.md#Q366)

33 `CreateOrPatch` calls across 29 `apply*` functions
(`actionsgateway_controller.go` 16/13, `actionsgateway_v2_controller.go` 8/7,
`egressproxy_controller.go` 9/9). Each is the same 8–12 lines differing only by Go
type, whether `SetControllerReference` is called, and which `Spec` fields are set.

Collapsing them also surfaces a real latent issue: **v1's owner-reference policy is
inconsistent and undocumented.** Only 4 of 11 v1 helpers call
`SetControllerReference`; the rest rely entirely on the finalizer in
`reconcileDelete`. v2 sets it on every child. Nothing states the v1 policy — you
must read all eleven and notice the absence. A force-removed finalizer on v1 leaks
ServiceAccounts, RoleBindings, NetworkPolicies, and HPAs.

Keep three as bespoke: `applyRoleBinding` (immutable `roleRef`), `applyService`
(preserve server-assigned `ClusterIP`), `applyProxyDeployment` (HPA owns replicas).

**F8 — Two god wiring functions with the linter switched off.** → [Q367](../STATUS.md#Q367)

- [cmd/gmc/cmd/main.go:73](../../cmd/gmc/cmd/main.go) — **669 lines**, prefixed
  `// nolint:gocyclo`, interleaving ~12 concerns: ~25 flag declarations, logging
  bootstrap, HTTP/2 + webhook TLS, cross-flag validation, informer cache scoping,
  manager construction, image-digest policy, v2 CRD detection, 8 controller
  registrations, 6 webhook registrations, health checks. None of it is
  test-reachable.
- [cmd/agc/main.go](../../cmd/agc/main.go) `run()` — **431 lines** with 23 scattered
  inline `os.Getenv` calls, so there is no single place to see the AGC's config
  surface, no validation pass, and no way to unit-test config resolution.

Both already proved the fix works — `validateLeaderElectionTimings`,
`parseAPIServerCIDRs`, `validateImageDigest`, `useImageVolume` were extracted. The
extraction simply stopped.

**F9 — `broker` and `scaleset` duplicate the error taxonomy verbatim.** → [Q369](../STATUS.md#Q369)

`RateLimitError` and `UnauthorizedError` are each declared twice
([broker/client.go:33,47](../../broker/client.go) and
[scaleset/errors.go:90,12](../../scaleset/errors.go)), and `parseRateLimitError`
exists in both ([broker/client.go:647](../../broker/client.go),
[scaleset/client.go:789](../../scaleset/client.go)) with identical bodies differing
only in parameter type (`*http.Response` vs `http.Header`). Callers handling both
protocols must type-switch on two unrelated types with the same name.
`githubapp/httpx/` already exists as the natural home.

**F10 — Script-layer sprawl.** → [Q370](../STATUS.md#Q370)

- `scripts/dogfood/setup.sh` — 688 lines / 32KB, 15 functions spanning cluster
  creation, node pools, credentials, CRD install, Helm install, CA-bundle patching,
  namespace, secrets, quota, an Athens proxy, and CR apply behind one `main()`.
  Larger than any Go file in scope; `scripts/dogfood/lib/` already exists as the
  home for the split.
- `scripts/lib/common.sh` is sourced by **26 of 69** scripts. The largest
  non-adopters re-roll its helpers: `probe-live-run.sh` and
  `probe-investigations-cd.sh` each define their own `step()`/`die()`/`gh_curl()`.
  `repo_root=` is recomputed in 11 scripts; ad-hoc `command -v` guards appear in 12
  despite `common.sh`'s `require_cmd`.
- `scripts/sync-chart-{crds,rbac,webhook}.sh` triplicate an identical
  trap/`render`/`sync`/`check`/`main` skeleton — even the header comments are
  verbatim copies. Extract to `scripts/lib/chart-sync.sh`.

## Prevention: what a linter could and could not have caught

Scored after the fact, to decide whether more gates or more audits are the better
investment. The answer is **both, because they catch disjoint classes** — and the
audit class is the larger one here.

| Finding | Catchable by a gate? |
|---|---|
| F3 sync gate covers 13% | **Yes** — already a script; generalize its file list ([Q374](../STATUS.md#Q374)) |
| F8 god `main`/`run` functions | **Yes** — `funlen` / `gocyclo` ([Q371](../STATUS.md#Q371)) |
| F10 script sprawl | Partly — a line-count check on `scripts/` would flag `setup.sh` |
| F1 Secret leak | No — semantic resource-lifecycle bug |
| F2 probe reimplements `scaleset` | No |
| F4 CIDR rule open-coded twice | No — see below |
| F5 four stub servers | No — cross-module |
| F6 foundation in v1-named files | No — architectural |
| F7 29 `apply*` wrappers | No — see below |
| F9 error taxonomy duplicated | No — cross-module |

**The decisive evidence: `dupl` is already enabled** (threshold 150, see
`.golangci.yml`) and the codebase passes it — so by construction it did not catch
any of the four duplication findings (F4, F5, F7, F9). Those clones are each below
the token threshold, or span files and modules it does not compare. Lowering the
threshold to reach them would drown the signal in table-test and struct-literal
boilerplate, which is exactly why 150 was chosen.

**F4 is the archetype of the irreducible class.** `githubCIDREgressRule` existed,
was documented as the shared helper, and the new code open-coded it anyway. No
linter detects "you reimplemented an abstraction that already exists two files
away" — that requires knowing intent. Roughly 7 of the 10 findings are this shape.

### Suppression discipline is a strength

79 `nolint` directives across 41k first-party lines, and **71 of them are `gosec`**
— whose noisy rule families are already excluded wholesale with written
justification in `.golangci.yml`. That leaves **8 non-`gosec` suppressions in the
entire codebase**. This is unusually disciplined and makes a suppression gate cheap
to adopt rather than a cleanup project.

One concrete payoff waiting: there is exactly one `nolint:gocyclo` in the repo
(`cmd/gmc/cmd/main.go:72`) and **`gocyclo` is not enabled**. The suppression is
inert — someone knew the function was over the line and pre-silenced a gate that
does not exist. `nolintlint` with `allow-unused: false` converts that from
invisible to a build failure.

### The `funlen` threshold curve

Measured 2026-07-20 over non-test, non-generated Go:

| Threshold | Functions over it |
|---|---|
| >200 lines | 5 |
| >150 lines | 11 |
| >120 lines | 15 |
| >100 lines | 21 |
| >80 lines | 31 |

[technical-debt.md](../development/technical-debt.md) currently records
*"Cyclomatic complexity | Skip for now — most long functions are legitimate wiring;
high noise-to-signal."* The curve says that call was too pessimistic: at a high
threshold the gate fires on a handful, not a flood. Sequence the gate **after**
[Q367](../STATUS.md#Q367) clears the two god `main`s, then set the threshold just
above the worst legitimate survivor and ratchet down — the same "gates by not
getting worse" pattern the coverage ratchet already uses, with no allowlist of
shame. **Q371 should update that metrics-table row** when the gate lands; it is
left unchanged until then so the doc keeps describing what is actually enforced.

### Audit cadence

Recurrence is tracked as [Q372](../STATUS.md#Q372) in Deferred, triggered by the
next minor release **or** ≥20% growth over the 41,011-line baseline — whichever
first. Growth is the honest proxy: this audit surfaced ~10 items across 41k lines,
so drift scales with code added rather than calendar time. A time-based schedule
would run the sweep when nothing had changed and miss a burst of growth between
ticks.

The sweep is cheap — this one was three parallel read-only agents plus verification,
with no code touched — which argues for triggering it more readily than a
heavyweight process would justify.

## Deliberate non-findings

Recorded so they are not re-litigated by the next audit.

- **The Makefile is fine.** 574 lines / 147 targets, but six `##@` sections, no
  include sprawl, longest recipe 22 lines, median single-digit. It delegates logic
  to `scripts/` rather than embedding shell — which is *why* `scripts/` sprawled and
  it did not. No action.
- **The GMC v1/v2 reconciler fork is correct.** v2 genuinely diverges (standalone
  `EgressProxy`, no PSA stamping, workload identity, direct egress, per-gateway
  naming, FQDN backends, different status contracts). Merging would produce a worse
  function than either. Only the *mechanical* copy-paste (F4, F6, F7) should go.
- **Done-channel discipline is honored.** `listenerState.done`, `StartRenewLoop`'s
  `renewDone`, `completeSiblingDeliveries`, and `Listener.Start` all return or expose
  `<-chan struct{}` per the repo convention. The AGC concurrency issues are
  concern-mixing, not lifecycle discipline.
- **Comment density is an asset, not debt.** Two spots drift into pure history and
  could move to `docs/design/` with a pointer left behind:
  `listener/config.go:186-208` (dogfood-experiment narrative on `FanoutCompletion`)
  and `listener/job.go:126-141` (re-litigating an abandoned fix). Not worth a Queue
  row on its own.
- **`mulQuantity`'s O(n) addition** is justified by the `Maximum=100` CRD cap, which
  is present on both v1 and v2 (`api/v2alpha1/egressproxy_types.go`). Comment is
  accurate; leave it.

## Unverified residuals

Reported by the audit sweep but **not** independently confirmed end-to-end. Per the
standing rule that source-reading alone has produced wrong conclusions (PR #59),
treat these as leads, not findings — confirm before acting.

- `listener/job.go` `handleJob` (254 lines): correctness may rest on undocumented
  LIFO ordering of four `defer`s, one of which reads a variable written ~60 lines
  later.
- `listener/goroutine.go` `Run` (277 lines): session ownership encoded as a
  `sessionID = ""` sentinel, with the invariant held by two copies of a five-line
  idiom staying in sync.
- The classic listener's empty-poll path may lack a pacing floor;
  `scalesetlistener` added `minPollInterval` for exactly this request-storm hazard
  and it may not have been backported.
- ~230 lines of near-identical reconciler shells between `runnergroup_controller.go`
  and `runnerset_controller.go`, including `drainConditions` — risky because both
  implement the same Q333 retain-until-reflected protocol.
- v1's `reconcileDelete` may not verify its deletes; v2's does (ported via Q328,
  possibly never back-ported).
