# Structural debt audit — 2026-07

A read-only audit of the whole codebase for technical debt, tangled control flow, readability, god functions, and oversized files.
Run 2026-07-20 against `main` @ `d6874e2`.
This doc holds the *evidence and rationale* for the Queue rows the audit produced, so each row can stay inside the 250-char cap without dropping a finding (per [maintaining-backlog.md](../development/maintaining-backlog.md)).

Classification follows [technical-debt.md](../development/technical-debt.md).

> **Status: ✅ Done — filed 2026-07-20; every finding F1–F10 and the prevention gates shipped.** The F1 Secret leak was fixed and merged the same day (Q373, #727); F2 (the probe rewrite) shipped as Q362, F3's share-and-gate split as Q374, F4's CIDR-rule consolidation as Q364, F5's broker-double consolidation as Q368, F7's CreateOrPatch collapse as Q366 (which spun the owner-reference-policy question out to Q394), F8's god-function decomposition as Q367, F9's error-taxonomy unification as Q369, F10's script-sprawl cleanup as Q370, the §Prevention gates (nolintlint + a ratcheted funlen) as Q371, and F6's foundation move as Q365 (which left the interleaved v1/v2 condition collectors to Q403, since shipped: the GMC's v1 collector passes now live in `metrics_v1.go`).
> [Q372](../queue/Q372.md) (Deferred) carries the re-run trigger.
>
> The ID range is not contiguous because concurrent branches allocated IDs while this audit was in flight: Q361 went to a CI-latency item (#722) and Q363 to a manifest-validate flake (#729), so this audit's F1 and F3 became Q373 and Q374.
> See [§Audit cadence](#audit-cadence) — `Next ID` cannot stay correct under parallel work, so open PRs are the authority.

## Headline

The codebase is **healthy overall**.
41,011 non-test, non-generated Go lines against 54,687 test lines (1.33:1); exactly **one** `TODO` marker repo-wide; only 21 functions over 100 lines and 5 over 200.
Comment quality is a genuine asset — comments overwhelmingly explain *why a bound exists and what breaks without it*, and the Q-ID cross-references pay off when reading cold.

The debt is **concentrated and structural**, not pervasive erosion.
It clusters in three places:

1. **Version boundaries** — v1/v2 in GMC, classic/scale-set in AGC, v2alpha1/v2beta1 in `api/`.
   The forks are mostly deliberate; the *mechanical* copy-paste riding along with them is not.
2. **Wiring entry points** — two god `main`/`run` functions that grew past the point where the extraction pattern they started with was abandoned.
3. **Test doubles and probes** — four independent hand-rolled implementations of one wire protocol.

## Findings

### Fixed as a bug, not flagged as debt

**F1 — ScaleSet worker leaks a JIT-config Secret per job.** ✅ **Shipped** (Q373, #727)

`ProvisionScaleSetWorker` ([provisioner.go:548](../../cmd/agc/internal/provisioner/provisioner.go)) stages `job-ss-<jobID>` at :573, then has three failure exits (:585 ceiling held, :597 scale-up throttle, :608 pod-create) — none delete it, and neither does the success path.
The classic `provision()` deletes on all five failure paths and on success (:436, :446, :464, :473, :488, :526).

Its doc comment delegates cleanup: *"Per-job Secret cleanup in steady state is the caller's responsibility (the reconciler wiring that drives this method)."* The caller is a one-line closure at [runnerset_scaleset.go:170](../../cmd/agc/internal/controller/runnerset_scaleset.go) that does no cleanup.
`grep -rn 'job-ss'` returns exactly two hits — the construction site and one test.
`runner_shared.go`'s reaper contains **zero** Secret references.

Effect: credential-bearing Secrets accumulate one-per-job for the RunnerSet's lifetime, reclaimed only by cascade-GC on RunnerSet deletion — on what is now the **default** acquisition protocol.
This is a documented-as-handled contract with no implementer, which is why it is a bug rather than debt.

Remedy: mirror `provision()`'s cleanup on each failure exit, and delete on the listener's terminal `JobCompleted` (`scalesetlistener/listener.go` `completeJob`, which today only bumps a metric).

**Shipped in #727** — the reclaim went in at the listener's terminal-completion seam (`ReclaimJobSecret`), not just on the failure exits, so the steady-state success path is covered too.
The `IsAlreadyExists` replay path deliberately does **not** delete: a replayed job's pod may still be mounting the Secret.
Covered by an internal provisioner test, a listener-seam test, and an envtest integration test.

### High

**F2 — `cmd/probe` reimplements the `scaleset` package it exists to validate.** ✅ **Shipped** (Q362)

`cmd/probe` never imports `scaleset` (verified: no import in any non-test file).
[scaleset.go:187-208](../../cmd/probe/scaleset.go) declares lowercase shadow copies of five exported library types — `runnerScaleSet`, `runnerScaleSetSession`, `runnerScaleSetMessage`, `jitRunnerConfig`, `adminConnection` — mirroring `RunnerScaleSet`, `RunnerScaleSetSession`, `RunnerScaleSetMessage`, `JITRunnerConfig`, `AdminConnection` in [scaleset/types.go](../../scaleset/types.go).
It hand-builds raw requests rather than calling `scaleset.Client`.

Why it matters beyond duplication: the probe's *purpose* is validating the protocol the `scaleset` package speaks.
Because it speaks its own dialect, a divergence between library and wire is invisible in exactly the case the probe exists to catch.

Remedy: rewrite against `scaleset.Client`; where the probe needs raw-wire visibility the client hides, add a response hook to the client rather than keeping a parallel implementation.
Highest-leverage deletion available — most of the 1,048 lines go away.

**Shipped (Q362).** The probe now drives `scaleset.Client` end to end; all five shadow types and every hand-built modelled request are gone.
The remedy's response hook is `scaleset.ResponseObserver` (`Config.Observer`), which reports status, headers, and latency for every response — including the 202 polls `GetMessage` returns as `(nil, nil)`, which is where the probe's rate-limit evidence lives.
Three checks stayed outside the library on purpose, because delegating them would have made the probe agree with itself: the raw-wire reporting above, the acquire route/token matrix (the client's construction measured against alternatives it does not implement, reached through the new `Client.RawServiceCall` escape hatch so even those comparisons use the client's own auth), and the delivered `acquireJobUrl` fallback — which now tries the client's static route *first* and logs a `DIVERGENCE` when the delivered URL succeeds where it failed.

The "most of the 1,048 lines" estimate above was optimistic: the file lands at 847.
The reimplementation itself did go — the five shadow types plus `mintAdminConnection` / `registrationToken` / `adminConnection` / `svcCall` are 165 lines deleted outright, and every remaining modelled call is a one-line client method where it used to build a request — but three parts of the file were never protocol code and stay: the ~110-line `PROBE_SCALESET_*` environment contract, the diagnostic matrix above, and a doc comment that grew to record what the probe still asserts on its own.

**F3 — The v2 conditions sync gate covers 13% of the duplication it appears to.** ✅ **Shipped** (Q374)

Modulo the version string, nine files are byte-identical across `api/v2alpha1` and `api/v2beta1` — **2,550 lines**:

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

`check-conditions-sync.sh` (since replaced) hardcoded two paths (`alpha=`, `beta=`) and guarded `conditions.go` alone — **332 of 2,550 lines**.
The other ~2,218 drifted silently.
The sync test concedes the design: *"There is no generator; whoever edits one mirrors the other."*

Note `conditions.go` is **pure constants plus one helper function — zero kubebuilder markers, zero API structs**, so it does not require per-version duplication at all.
Same for `sidecar.go` (0 types, 0 markers).

This inverts the project's own *"correct it twice, then automate it"* principle: the gate automates enforcement of a copy that deletion would remove.

Remedy, cheapest first: (a) generalize the script to a file list covering all nine; (b) move the non-API identical files into a shared internal package that both versions type-alias (`type ProxyConfig = apishared.ProxyConfig` — aliases are deepcopy- and conversion-transparent), leaving only `runnerset_types.go` and `conversion.go` versioned.

**Shipped in Q374.** Both halves, split by whether the file holds versioned types.

*Shared.* The condition/reason vocabulary moved to `api/apiconditions` and the worker-pod sidecar contract to `api/apisidecar` — neither holds an API struct or a kubebuilder marker, so neither needs per-version duplication at all.
The version packages keep thin re-export blocks (a one-line alias per name; a `RunnerTemplateSpec`-typed wrapper for the heuristic) so all 410 existing `v2alpha1.ConditionX` call sites compile unchanged.
`conditions.go` fell 363 → 97 lines and `sidecar.go` 83 → 28, with the values and the rationale now living in one place where they cannot diverge.
Generated output — the five CRD manifests and both `zz_generated.deepcopy.go` — is byte-identical after `make -C api generate`.

*Gated.* `check-conditions-sync.sh` became `check-v2-api-sync.sh` with the default inverted: every `.go` file present in both packages must match unless named in an `EXEMPT` list with a reason, so a file added to both is covered the day it lands.
Two differences are normalised away — the `package` clause and `+kubebuilder:storageversion` — which is what brings the three near-identical `*_types.go` files (804 lines) under the gate.
Coverage went 363 → 1,397 lines across 8 files; a stale exemption fails the gate so the list cannot rot.

Re-measured at implementation time, the audit's table had already shifted: `conditions.go` was 363 lines (not 332), `zz_generated.deepcopy.go` 1,001 (not 879), and `actionsgateway_types.go`/`egressproxy_types.go`/`runnertemplate_types.go` differed only by a `storageversion` marker (not 7 and 1 lines).
`zz_generated.deepcopy.go` is exempt rather than gated: it is controller-gen output derived from the *versioned* `runnerset_types.go`, so requiring cross-version identity would assert an invariant the generator, not a contributor, decides — and `make generate` drift already guards it.

Two things surfaced on the way: the gate had never run in CI at all (`make check` only, so the drift it guards could reach `main`), and `api/**` was missing from `unit-test.yml`'s `code` paths-filter, so an api-only change skipped its own gofmt, golangci-lint, and unit tests.
Both fixed in the same change.

**F4 — `githubCIDREgressRule` exists; the egress-proxy builder open-codes it twice.** ✅ **Shipped** (Q364)

The shared helper at [builder.go:404](../../cmd/gmc/internal/controller/builder.go) is called from three sites — but not from `egressproxy_builder.go`, which open-codes the same peer-building loop at [:470-476 and :487-493](../../cmd/gmc/internal/controller/egressproxy_builder.go).
The helper's own doc comment claims to be the shared one.

Three independent spellings of "which CIDRs may we reach on 443" in a security-relevant allowlist builder is where a policy bug hides.
Same failure class as the PR #59 `ipBlock` trap the file's own comments warn about.

**Shipped in Q364, but only *one* of the two open-coded loops was a spelling of the GitHub-CIDR rule.** The three-way equivalence check found: the first loop (`githubCIDRs []net.IPNet`, gated `managed && egressUsesCIDR && len>0`) is byte-for-byte the helper's output — consolidated onto `githubCIDREgressRule`, so the GitHub-CIDR 443 allowlist now has one spelling across v1 and v2.
The *second* loop is **not** the GitHub rule: it renders `spec.destinationCIDRs` — CRD-documented as "EXTRA, non-GitHub IP ranges" — a `[]string` operator allowlist gated *without* `egressUsesCIDR` (it applies in every egress mode, while the GitHub rule is CIDR-mode only).
It shares only the port-443 ipBlock shape, not the meaning, so folding it into the GitHub helper would conflate two distinct allowlists; it stays a separate rule with a comment saying why.
A `reflect.DeepEqual` drift test pins the GitHub rule to the helper's output (it passed against both the pre- and post-consolidation builders, proving the rendered policy is unchanged) and locks in that the destinationCIDRs rule stays distinct — including that it survives FQDN mode while the GitHub rule is gated out.

**F5 — Four hand-rolled implementations of one broker wire protocol.** ✅ **Shipped** (Q368)

`broker/brokertest/server.go` (746) + `test/fakegithub/main.go` (1118) + `cmd/agc/test/load/broker_stub.go` (373), plus the probe (F2).
`handleSession`, `handleMessage`, `handleAcquireJob`, `handleRenewJob` recur across all of them — down to identical session-ID minting (`fmt.Sprintf("session-%d", …)`) and the same bearer-token DELETE fallback. ~1,200 of ~2,240 stub-server lines look recoverable.

These carry real production cost: `test/fakegithub` is built and published as a container image, Dependabot-tracked, Trivy-scanned, and Dockerfile-linted — a shipped product surface at 1,118 lines with its own 661-line test suite.

Related: `cmd/probe/compat/compat.go` is a non-`_test.go` file that imports `net/http/httptest` and `broker/brokertest` into the module's regular build graph.
Its doc comment asserts it "never ships in a compiled artifact" — true today only because no `package main` imports it, with nothing enforcing that.
A check asserting no `package main`-reachable file imports `httptest` would make the guarantee real.

**Shipped in Q368.** The session and credential *mechanics* — minting `session-<n>` IDs, resolving a DELETE by `sessionId` query param or bearer token, owner-scoped session listing, the `#POST − #DELETE` active count, connection-reuse-safe JSON framing, and the JWT-issuer extraction — moved to one stdlib-only library, `broker/brokerstub`.
All three doubles build on it.
What stayed per-double is the *policy* that legitimately differs: fan-out job accounting (Q260) in `brokertest`, the single-use JIT lifecycle + opportunistic redelivery + Q154 lease model + registration/control APIs in `fakegithub`, saturated auto-delivery in the load stub.
Folding those divergent `handleMessage`/`handleAcquireJob` bodies into one configurable handler would produce the "worse function than either" the [deliberate non-findings](#deliberate-non-findings) warn against — these are different test scenarios, not reimplementations of the wire protocol, which now exists once.
`broker/brokerstub` is kept dependency-free so `fakegithub` links no third-party code (the `broker` client would pull in githubapp/JWT/Prometheus): the distroless Trivy-scanned image gains only the repo's own module, no new scan surface.

The `httptest` guarantee is now real, not conventional: `TestNoPackageMainReachesHTTPTest` (in `cmd/probe/compat`) walks every `package main` in the workspace and fails if any transitively imports `net/http/httptest` in its compiled build graph — `go list -deps`, so a `_test.go` httptest import (fakegithub's own tests) is correctly ignored.
It runs in `make check`.

### Medium-high

**F6 — Shared foundation lives in v1-named files; blocks the Q273 sunset.** ✅ **Shipped** (Q365)

Most cross-version foundation sits in files named for v1 that v2 imports:

- `builder.go` — the constants block, `ptr`, `securityProfileOrDefault`, `hardenedContainerSecurityContext`, `nonrootPodSecurityContext`, `copyRecommendedLabels`, `dnsEgressRule`, `githubCIDREgressRule`, `buildAGCNetworkPolicyFrom`, `agcWorkloadNames`, `buildAGCDeploymentFrom`, `buildNoProxy`.
- `actionsgateway_controller.go` — `provisioningError`, `quotaCheck`, `footprintFromResources`, `evalProxyPoolQuota`, `quotaHeadroomViolations`, `resourceListEqual`, `quotaHardChangedPredicate`, `labelSafe`.

Same pattern in AGC: the `listener` package is treated as classic-only, but `provisioner`, `runnerset_target.go`, `runner_shared.go`, and `main.go` all import it for `listener.Metrics`, `JobHandlerFunc`, `AdmitFunc`, `EventRecorder` — types with nothing to do with classic acquisition.

Consequence: **deleting v1 today is a multi-hundred-line refactor, not a `git rm`.** Doing the moves *now*, while both versions are live, is mechanical and behavior-free, and directly de-risks the already-blocked [Q273](../queue/Q273.md).
This is why the row sorts adjacent to Q273 rather than by its own severity.

**Shipped in Q365.** Re-measured at implementation time (2026-07-24, after Q360, Q364 and Q366 reshaped the GMC package), the cross-version foundation moved into eight `shared_*.go` files in `cmd/gmc/internal/controller` and the v1-only IP-range pass into `ipranges_v1.go`; `cert.go` and `metrics_cert.go` turned out to hold the same problem (the PKI primitives and mount layout) and were split the same way.
The AGC half became a new leaf package, `cmd/agc/internal/runnercore`, holding `Metrics`, `EventRecorder`, `ConditionUpdater` and `AdmitFunc`.
One correction to the list above: `JobHandlerFunc` is **not** version-neutral — its parameters are the classic `AcquireJob` response and it returns a `broker.TaskResult` from the classic `broker` module — so it stayed in `listener`, where Q264 deletes it wholesale.
The move was proven behaviour-free by an AST diff of every top-level declaration in both packages before and after: byte-identical apart from package qualifiers and the one deliberate `reconcileAll` → `refreshV1ProxyNetworkPolicies` extraction.

### Medium

**F7 — ~29 near-identical `CreateOrPatch` wrappers across three GMC reconcilers.** ✅ **Shipped** (Q366)

33 `CreateOrPatch` calls across 29 `apply*` functions (`actionsgateway_controller.go` 16/13, `actionsgateway_v2_controller.go` 8/7, `egressproxy_controller.go` 9/9).
Each is the same 8–12 lines differing only by Go type, whether `SetControllerReference` is called, and which `Spec` fields are set.

Collapsing them also surfaces a real latent issue: **v1's owner-reference policy is inconsistent and undocumented.** Only 4 of 11 v1 helpers call `SetControllerReference`; the rest rely entirely on the finalizer in `reconcileDelete`. v2 sets it on every child.
Nothing states the v1 policy — you must read all eleven and notice the absence.
A force-removed finalizer on v1 leaks ServiceAccounts, RoleBindings, NetworkPolicies, and HPAs.

Keep three as bespoke: `applyRoleBinding` (immutable `roleRef`), `applyService` (preserve server-assigned `ClusterIP`), `applyProxyDeployment` (HPA owns replicas).

**Shipped in Q366.** Re-measured at implementation time (2026-07-24, after Q360 and Q364 reshaped the package) the counts had shrunk to **26 `CreateOrPatch` calls / 26 `apply*` wrappers** (v1 11, v2 7, EgressProxy 8) — one call per wrapper, no double-call wrappers left.
All 26 now delegate to a single generic `applyManagedChild[T client.Object]` (`apply_helpers.go`): it keys the shell by the desired object's namespace/name, writes the managed labels, runs a per-type `copyManaged` closure, and — **only when the caller passes a non-nil owner** — stamps the controller reference.
The three "bespoke" behaviours did not need separate skeletons: the immutable-`roleRef` recreate is a thin branch *around* the one path (the closure returns `errRoleRefImmutable`; the wrapper does the delete+recreate), and `applyService`/`applyProxyDeployment`/the EgressProxy conditional-scale Deployment are just different `copyManaged` closures.
So the 8–12-line skeleton collapsed 26→1 while every wrapper kept its exact type/fields/owner behaviour.

The refactor was strictly behaviour-preserving: the per-call-site owner-reference decision is passed through unchanged (v1 stays 4-of-11 owned, v2 all-but-the- cluster-scoped-binding, EgressProxy all-owned).
New table tests (`apply_helpers_ownerref_test.go`) pin that contract per helper — including an explicit `4 of 11` assertion for v1 — so the collapse cannot silently add or drop an owner.

The ownerRef-policy inconsistency itself was **deliberately not resolved here** — a force-removed-finalizer leak is a security-relevant behaviour question that must be decided on its own, not smuggled into a cleanup.
It was filed as Q394 for a separate, signed-off decision.

**Q394 resolved it (2026-07-26): defence in depth, not either/or.** All 11 v1 children are namespaced siblings of the CR, so all 11 now stamp a controller owner reference — matching v2 and the EgressProxy reconciler — while `reconcileDelete` stays exactly as it was.
The finalizer remains the primary path (ordered: drain `RunnerGroup`s first; fail-closed: no finalizer removal until every delete is confirmed; and the only thing that reaches legacy objects the reconciler no longer applies), and cascade GC is purely a backstop for a force-removed finalizer.
The ordered drain is unaffected because the CR does not leave etcd — and so GC does not fire — until the finalizer clears.
The single remaining un-owned child is v2's cluster-scoped `ClusterRoleBinding`, which a namespaced gateway *cannot* legally own.
The `4 of 11` assertion became `11 of 11`, and an envtest sweep (`TestGMC_V1Provisioning_EveryManagedChildIsOwnerReferenced`) now drives the assertion from what is on the cluster, so a twelfth child added through a new code path cannot ship un-owned without appearing in any table.
Policy documented at [design §4.1.1](../design/04-operational-flows.md#411-tenant-teardown-and-child-reclamation).

**F8 — Two god wiring functions with the linter switched off.** ✅ **Shipped** (Q367)

- [cmd/gmc/cmd/main.go:73](../../cmd/gmc/cmd/main.go) — **669 lines**, prefixed `// nolint:gocyclo`, interleaving ~12 concerns: ~25 flag declarations, logging bootstrap, HTTP/2 + webhook TLS, cross-flag validation, informer cache scoping, manager construction, image-digest policy, v2 CRD detection, 8 controller registrations, 6 webhook registrations, health checks.
  None of it is test-reachable.
- [cmd/agc/main.go](../../cmd/agc/main.go) `run()` — **431 lines** (grown to 462 by the time Q367 ran) with 23 (measured: 24) scattered inline `os.Getenv` calls, so there is no single place to see the AGC's config surface, no validation pass, and no way to unit-test config resolution.

Both already proved the fix works — `validateLeaderElectionTimings`, `parseAPIServerCIDRs`, `validateImageDigest`, `useImageVolume` were extracted.
The extraction simply stopped.

**Shipped in Q367.** Pure refactor — same flags, env vars, defaults, and startup ordering (the argument: `os.Getenv` is side-effect-free and the environment is immutable during startup, so hoisting all reads to a snapshot cannot change behavior; every erroring/side-effecting step kept its original sequence position).

GMC `main()` fell **669 → 83 lines**: flag declarations moved to `addFlags` (`flags.go`, bound to a passed `flag.FlagSet` so defaults are testable); cross-flag validation to `resolveConfig`/`buildCacheOptions` and image/digest policy to `resolveImages` (`config.go`); the option builders, `newManager`, and the controller/webhook/health registration to `config.go`/`wiring.go`.
The inert `// nolint:gocyclo` is gone.
AGC `run()` fell **462 → 329 lines**: the 24 env reads became a single `loadConfig` snapshot, with `buildRegistrar`, `buildBrokerConfig`, `buildScheme`, `configureProxyTrust`, `setupProvisioner`, and `setupUsageSampler` carved out.
New unit tests cover the now-reachable config helpers (`loadConfig`, `buildRegistrar`, `buildBrokerConfig`, `buildScheme`, `addFlags`, `resolveConfig`, `buildCacheOptions`, `resolveImages`).
The `funlen`/`nolintlint` gates that would lock this in stay with Q371.

**F9 — `broker` and `scaleset` duplicate the error taxonomy verbatim.** ✅ **Shipped** (Q369)

`RateLimitError` and `UnauthorizedError` are each declared twice ([broker/client.go:33,47](../../broker/client.go) and [scaleset/errors.go:90,12](../../scaleset/errors.go)), and `parseRateLimitError` exists in both ([broker/client.go:647](../../broker/client.go), [scaleset/client.go:789](../../scaleset/client.go)) with identical bodies differing only in parameter type (`*http.Response` vs `http.Header`).
Callers handling both protocols must type-switch on two unrelated types with the same name.
`githubapp/httpx/` already exists as the natural home.

**Shipped in Q369, premise confirmed on re-measurement.** The two pairs were semantically identical — same single field (`RetryAfter time.Duration` with the same -1 "no header, back off exponentially" sentinel; `StatusCode int`), no `Unwrap`/`Is` method on either, and `Error()` text differing only in the `broker: ` / `scaleset: ` prefix — so unifying them changed no protocol's behavior.
Both now alias one declaration in [githubapp/httpx/errors.go](../../githubapp/httpx/errors.go), which carries a `Source` field (`SourceBroker`/`SourceScaleSet`) purely to keep those message prefixes; it never gates matching.
The two `parseRateLimitError` bodies collapsed into the exported `httpx.ParseRateLimitError(source, header)` — the `*http.Response` caller passes `resp.Header`.

Aliases (not renames) were deliberate: `broker.RateLimitError` and `scaleset.UnauthorizedError` are referenced from `cmd/agc`, `cmd/probe`, and the compatibility matrix, and an alias keeps every one of those call sites valid *while* making the two names the same type — which is the actual fix, since an `errors.As` against either now matches an error from either protocol.
The regression guard the compiler cannot give is `errors_q369_test.go` in both listener packages: `scalesetlistener`'s drives real 429/401 responses through *both* protocol clients and asserts that listener's own matchers (`isRateLimited`, `isUnauthorized`) fire for each, plus a cross-package assignment that fails to *compile* if the types are ever re-split; `listener`'s does the same for the classic matchers.
Both halves were verified to fail by temporarily re-declaring a distinct `scaleset` type — the matcher assertions returned false and the assignment stopped compiling, as intended.

**F10 — Script-layer sprawl.** ✅ **Shipped** (Q370)

- `scripts/dogfood/setup.sh` — 688 lines / 32KB, 15 functions spanning cluster creation, node pools, credentials, CRD install, Helm install, CA-bundle patching, namespace, secrets, quota, an Athens proxy, and CR apply behind one `main()`.
  Larger than any Go file in scope; `scripts/dogfood/lib/` already exists as the home for the split.
- `scripts/lib/common.sh` is sourced by **26 of 69** scripts.
  The largest non-adopters re-roll its helpers: `probe-live-run.sh` and `probe-investigations-cd.sh` each define their own `step()`/`die()`/`gh_curl()`.
  `repo_root=` is recomputed in 11 scripts; ad-hoc `command -v` guards appear in 12 despite `common.sh`'s `require_cmd`.
- `scripts/sync-chart-{crds,rbac,webhook}.sh` triplicate an identical trap/`render`/`sync`/`check`/`main` skeleton — even the header comments are verbatim copies.
  Extract to `scripts/lib/chart-sync.sh`.

**Shipped in Q370, two of the three threads — the other two premises were stale on re-measurement.** The `sync-chart-*.sh` triplication is gone: the temp-file registry, EXIT-trap cleanup, and `--check`/write/usage dispatch moved to the new `scripts/lib/chart-sync.sh`, which the three now source (each keeps only its genuinely-different `render`/`sync`/`check`).
The two named `common.sh` non-adopters now source it: the byte-identical `step`/`die`/`gh_curl` were promoted into `common.sh` and the local copies deleted (`info`/`warn` stayed local to `probe-investigations-cd.sh` — single-script, not duplicated).
`docs-preview.sh`'s `die` was left alone: it prints a script-name-prefixed message, so it is not the same contract.
`setup.sh` needed no work — re-measured at 730 lines but **already decomposed** into 16 concern-scoped functions (`create_cluster`, `install_gag`, `apply_cr`, …) behind a clean orchestrating `main()`; the length is inherent step complexity, not a god function, and moving single-use bootstrap functions into `dogfood/lib/` would split the from-zero flow across files for no reuse gain while risking the Q380 bootstrap.
The `command -v`/`repo_root` counts were also stale: `repo_root="$(git rev-parse …)"` is the required pre-source idiom (`common.sh`'s own header mandates it, not sprawl), and the remaining `command -v` guards are graceful-detection sites where `require_cmd`'s fail-fast `exit 1` would be wrong (the tool-checker itself, the `PreToolUse` hooks, `local-throttle.sh`'s optional probes) — no clean swap exists, so none was forced.

## Prevention: what a linter could and could not have caught

Scored after the fact, to decide whether more gates or more audits are the better investment.
The answer is **both, because they catch disjoint classes** — and the audit class is the larger one here.

| Finding | Catchable by a gate? |
|---|---|
| F3 sync gate covers 13% | **Yes** — already a script; generalize its file list (Q374, shipped) |
| F8 god `main`/`run` functions | **Yes** — `funlen` / `gocyclo` (Q371, shipped) |
| F10 script sprawl | Partly — a line-count check on `scripts/` would flag `setup.sh` |
| F1 Secret leak | No — semantic resource-lifecycle bug |
| F2 probe reimplements `scaleset` | No |
| F4 CIDR rule open-coded twice | No — see below |
| F5 four stub servers | No — cross-module |
| F6 foundation in v1-named files | No — architectural |
| F7 29 `apply*` wrappers | No — see below |
| F9 error taxonomy duplicated | No — cross-module |

**The decisive evidence: `dupl` is already enabled** (threshold 150, see `.golangci.yml`) and the codebase passes it — so by construction it did not catch any of the four duplication findings (F4, F5, F7, F9).
Those clones are each below the token threshold, or span files and modules it does not compare.
Lowering the threshold to reach them would drown the signal in table-test and struct-literal boilerplate, which is exactly why 150 was chosen.

**F4 is the archetype of the irreducible class.** `githubCIDREgressRule` existed, was documented as the shared helper, and the new code open-coded it anyway.
No linter detects "you reimplemented an abstraction that already exists two files away" — that requires knowing intent.
Roughly 7 of the 10 findings are this shape.

### Suppression discipline is a strength

79 `nolint` directives across 41k first-party lines, and **71 of them are `gosec`** — whose noisy rule families are already excluded wholesale with written justification in `.golangci.yml`.
That leaves **8 non-`gosec` suppressions in the entire codebase**.
This is unusually disciplined and makes a suppression gate cheap to adopt rather than a cleanup project.

One concrete payoff has since been realized: the repo's only `nolint:gocyclo` (`cmd/gmc/cmd/main.go`) was inert — someone knew the function was over the line and pre-silenced a gate that does not exist (`gocyclo` is not enabled).
Q367 removed it when the function shrank to 83 lines; `nolintlint` with `allow-unused: false` (Q371) would have converted such an inert suppression from invisible to a build failure.

### The `funlen` threshold curve

Measured 2026-07-20 over non-test, non-generated Go:

| Threshold | Functions over it |
|---|---|
| >200 lines | 5 |
| >150 lines | 11 |
| >120 lines | 15 |
| >100 lines | 21 |
| >80 lines | 31 |

[technical-debt.md](../development/technical-debt.md) previously recorded *"Cyclomatic complexity | Skip for now — most long functions are legitimate wiring; high noise-to-signal."* The curve showed that call was too pessimistic: at a high threshold the gate fires on a handful, not a flood.
With Q367 clearing the two god `main`s, Q371 landed the gate at `lines: 200` — just above the worst legitimate survivor (`listener.Run`, 197 lines, measured 2026-07-23 after Q367 shrank the two `main`s to 178/191 and below) — set to ratchet down, the same "gates by not getting worse" pattern the coverage ratchet already uses, with no allowlist of shame.
Q371 updated that metrics-table row to match.

### Audit cadence

Recurrence is tracked as [Q372](../queue/Q372.md) in Deferred, triggered by the next minor release **or** ≥20% growth over the 41,011-line baseline — whichever first.
Growth is the honest proxy: this audit surfaced ~10 items across 41k lines, so drift scales with code added rather than calendar time.
A time-based schedule would run the sweep when nothing had changed and miss a burst of growth between ticks.

The sweep is cheap — this one was three parallel read-only agents plus verification, with no code touched — which argues for triggering it more readily than a heavyweight process would justify.

## Deliberate non-findings

Recorded so they are not re-litigated by the next audit.

- **The Makefile is fine.** 574 lines / 147 targets, but six `##@` sections, no include sprawl, longest recipe 22 lines, median single-digit.
  It delegates logic to `scripts/` rather than embedding shell — which is *why* `scripts/` sprawled and it did not.
  No action.
- **The GMC v1/v2 reconciler fork is correct.** v2 genuinely diverges (standalone `EgressProxy`, no PSA stamping, workload identity, direct egress, per-gateway naming, FQDN backends, different status contracts).
  Merging would produce a worse function than either.
  Only the *mechanical* copy-paste (F4, F6, F7) should go.
- **Done-channel discipline is honored.** `listenerState.done`, `StartRenewLoop`'s `renewDone`, `completeSiblingDeliveries`, and `Listener.Start` all return or expose `<-chan struct{}` per the repo convention.
  The AGC concurrency issues are concern-mixing, not lifecycle discipline.
- **Comment density is an asset, not debt.** Two spots drift into pure history and could move to `docs/design/` with a pointer left behind: `listener/config.go:186-208` (dogfood-experiment narrative on `FanoutCompletion`) and `listener/job.go:126-141` (re-litigating an abandoned fix).
  Not worth a Queue row on its own.
- **`mulQuantity`'s O(n) addition** is justified by the `Maximum=100` CRD cap, which is present on both v1 and v2 (`api/v2alpha1/egressproxy_types.go`).
  Comment is accurate; leave it.

## Unverified residuals

Reported by the audit sweep but **not** independently confirmed end-to-end.
Per the standing rule that source-reading alone has produced wrong conclusions (PR #59), treat these as leads, not findings — confirm before acting.

- `listener/job.go` `handleJob` (254 lines): correctness may rest on undocumented LIFO ordering of four `defer`s, one of which reads a variable written ~60 lines later.
- `listener/goroutine.go` `Run` (277 lines): session ownership encoded as a `sessionID = ""` sentinel, with the invariant held by two copies of a five-line idiom staying in sync.
- The classic listener's empty-poll path may lack a pacing floor; `scalesetlistener` added `minPollInterval` for exactly this request-storm hazard and it may not have been backported.
- ~230 lines of near-identical reconciler shells between `runnergroup_controller.go` and `runnerset_controller.go`, including `drainConditions` — risky because both implement the same Q333 retain-until-reflected protocol.
- v1's `reconcileDelete` may not verify its deletes; v2's does (ported via Q328, possibly never back-ported).
