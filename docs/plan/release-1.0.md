# Release 1.0 Milestone Definition

← [Milestone 5](milestone-5.md) | [Back to implementation phases](../design/06-implementation-phases.md) | [STATUS](../STATUS.md)

---

## What 1.0 means

1.0 is the first release an operator can **install, run multi-tenant in
production, and trust the security and isolation claims of** — without
reading the source to know whether a guardrail actually holds.

It is *not* the release that proves the headline capacity claim
(thousands of virtual sessions per AGC) at scale, nor the release that
validates sandboxed runtimes. Those are real goals, but expensive to
validate and not prerequisites for a correct, deployable, secure
single-to-modest-tenant system. They are explicitly deferred to
[post-1.0](#explicitly-out-of-scope-post-10) and the docs are required
to say so plainly.

Phrased as one sentence: **an operator can `helm install` the chart,
onboard two isolated tenants that each run a real GitHub job through the
egress proxy, and every security control we document is one we have
either observed enforcing or whose validation limits the docs state
plainly.**

---

## Definition of Done

1.0 ships when every **gating** item below is ✅. **Recommended** items
strengthen the release but do not block the tag; any recommended item
not done at tag time moves to the post-1.0 Queue with its rationale. A
third class — bucket [G, **must-resolve**](#g-must-resolve-before-tag--fix-or-fold-into-the-q99-honesty-pass) —
also blocks the tag, but is satisfied by *either* a fix *or* an honest
docs caveat (folded into the [Q99](../STATUS.md) honesty pass), not only
by a ✅.

### A. Functional completeness & live proof — *gating*

~~The code is done; what's missing is proof against a real cluster and
real GitHub.~~ **All four items proven live on 2026-06-12** — full
session record in
[milestone-4.md §12](milestone-4.md#12-live-multi-tenant-validation-evidence-2026-06-1112).
The session surfaced four product bugs (Q114–Q117); Q114 (JIT agents
are single-use, no AGC self-healing) and Q115 (default worker
SecurityContext breaks the runner image) are new `1.0-gate` rows.

- [x] **Two `ActionsGateway` CRs → two isolated tenants**, validated
  live on `kind` (not source-read): each tenant gets its own namespace,
  AGC, and proxy pool with ≥ `minReplicas` Ready pods. *(M4 DoD row 1)*
  — proven 2026-06-12, [evidence](milestone-4.md#12-live-multi-tenant-validation-evidence-2026-06-1112).
- [x] **Deleting one CR tears down only that tenant**; the other
  tenant's namespace and workloads are unaffected. *(M4 DoD row 2)*
  — proven 2026-06-12 (the surviving tenant also ran a green job
  afterwards).
- [x] **End-to-end job through the proxy**: a job dispatched from
  GitHub routes via `HTTPS_PROXY`, runs in a worker pod, and completes
  green. The Tier-A `E2E_GMC_TenantProvisioning_ProxyConnectWorks` spec
  already exists and runs in CI; the real-GitHub path was proven live
  2026-06-12 (runs 27386891757 / 27395702908, both `success`).
  **Caveat:** the green path needed a per-tenant `runAsUser: 1001`
  podTemplate workaround — the *default-path* worker pod is broken
  until [Q115](../STATUS.md) lands, so Q115 is itself a `1.0-gate`.
- [x] **Runner version contract confirmed** ([Q71](../STATUS.md), now
  closed): live session creation with real App creds succeeds. Nuance:
  the GMC-provisioned AGC sends an **empty** `agent.version` (it never
  sets `GITHUB_RUNNER_VERSION`), which GitHub accepts — the 2.334.x pin
  lives only in the worker image. Follow-up tracked as
  [Q118](../STATUS.md) — now itself a `1.0-gate` (see below).

Two follow-on gates were identified after the live run (same pattern as
the Q114/Q115 discoveries above) and must close before tag:

- [x] **Runner-version contract not regressed** ([Q118](../STATUS.md),
  closed): the runner version is now a single source of truth
  (`RunnerVersion` in `cmd/agc/names`) that drives `DefaultWorkerImage`
  (now digest-pinned to 2.335.1, matching the worker Dockerfile) and the
  `GITHUB_RUNNER_VERSION` the GMC injects into the AGC — so `CreateSession`
  sends a non-empty `agent.version` equal to the pinned version. A lockstep
  unit test (`cmd/agc/names/runner_version_test.go`) fails CI if the
  Dockerfile `FROM` tag/digest and the constant drift, so a future
  Dependabot bump can't silently regress the contract bucket-A row 4
  ([Q71](../STATUS.md)) gated on.
- [ ] **Teardown does not fail open** ([Q125](../STATUS.md), *gating*):
  `deleteIfExists` (`actionsgateway_controller.go:274`) swallows
  non-NotFound delete errors and `reconcileDelete` removes the finalizer
  anyway, so a transient API failure orphans a live **credentialed** AGC
  Deployment with no retry. This breaks the error path of the
  delete-one-tenant isolation behavior gated by row 2 — collect errors
  and requeue until every delete succeeds or is NotFound.

### B. Security & isolation proof — *gating + recommended*

The security *control* (the egress NetworkPolicy) must ship
enforced-by-default; *observing* the negative path actually drop traffic
is recommended, not gating, because it needs a special-CNI cluster — the
same expense class as the deferred gVisor/load-test work. Q7b ran
2026-06-11, so the bucket-E egress caveat is lifted (replaced by the
production CNI requirement statement).

- [ ] **No secure-by-default regression in shipped manifests**
  (the security half of [Q34](../STATUS.md), *gating*): the install
  artifact must not ship with NetworkPolicy / ServiceMonitor commented
  out. Anything off-by-default that weakens a security property is a
  documented, explicit opt-*out*, never a silent default
  (secure-by-default principle; see
  [security design](../design/05-security.md)).
- [ ] **Documented RBAC scope matches the install artifact**
  ([Q121](../STATUS.md) + [Q122](../STATUS.md), *gating*):
  `05-security.md` / `02-architecture.md` claim GMC Secret access is
  name-scoped (metadata-only list) and that workload writes are confined
  to namespaces holding an `ActionsGateway` CR — but the shipped
  ClusterRole grants cluster-wide Secret read/write and all-namespace
  workload writes (deployments/rolebindings/NPs/SAs/quotas). Two
  documented security controls that are **false in the install
  artifact**. Resolve each = the confining ValidatingAdmissionPolicy(s)
  **or** correct the docs — it gates either way.
- [x] **Worker egress negatives observed enforcing** (Q7b,
  *recommended*, ran 2026-06-11): `WorkloadEgressBlockedToNonProxyPod` +
  `WorkerCannotReachK8sAPI` observed dropping traffic on a Calico
  v3.31.5 kind cluster (`make e2e-cluster KIND_CNI=calico`), alongside
  the green positive `ProxyConnectWorks` on the same cluster (14/14
  provisioning specs). Evidence and reproduction:
  [worker-egress-proxy.md](worker-egress-proxy.md#runtime-negative-case-enforcement-validated-on-calico-q7b-2026-06-11).
  A CI leg for the Calico profile is tracked as Q119.

### C. Packaging & supply chain — *gating + recommended*

This is the keystone: nothing downstream (posture scan, signing, audit
policy) can exist until there is an artifact to install and scan.

- [x] **Reproducible install artifact** ([Q12](../STATUS.md), now
  closed — chart shipped + lint/template/kubeconform-validated offline,
  and live-proven 2026-06-12 (digest-pinned `helm install` → working
  tenant → green job; CRD `resource-policy: keep` observed on
  uninstall). Evidence:
  [q12-helm-chart.md](q12-helm-chart.md#live-validation-track-a--2026-06-12).
  Publishing pipeline remains [Q98](../STATUS.md). Original gate text:
  a Helm chart under `charts/actions-gateway/` (Helm decided over
  Kustomize per [D-M5-1](milestone-5.md#11-install-vehicle--decided-helm-chart))
  that produces a working tenant from a single `helm install`. Contents
  per [milestone-5.md §1.2](milestone-5.md): both CRDs, GMC
  Deployment + RBAC + webhook config, IP-range schedule, proxy image
  refs, an opt-in sample CR, **all images pinned by digest**. Two
  Helm-specific sub-gates from §1.1:
    - [x] **cert-manager is optional**: `certManager.enabled` value with
      a self-signed-cert hook fallback, so the webhook installs without
      cert-manager present.
    - [x] **CRDs upgrade**: shipped in `templates/` with
      `helm.sh/resource-policy: keep`, not `crds/` (Helm never upgrades
      `crds/`), so day-2 `helm upgrade` carries CRD field changes.
- [ ] **Posture scan clean** ([Q14](../STATUS.md), *gating*): `polaris`
  audit against the rendered install artifact returns zero "danger"
  findings; intentional deviations live in a `polaris.yaml` suppression
  list with a per-entry design-doc citation.
- [ ] **Images signed + SBOM** ([Q28](../STATUS.md), *recommended*):
  cosign signatures + SBOM attached to published images. Strong supply
  chain story; not strictly required to run.
- [ ] **API-server audit policy sample** ([Q29](../STATUS.md),
  *recommended*): ship a sample policy that surfaces a compromised
  GMC's Secret `get` calls.

### D. Production operability — *gating + recommended*

Things an operator hits on day one that source-level unit tests don't
catch.

- [ ] **AGC production logging + health probes** (part of
  [Q35](../STATUS.md), *gating*): AGC must not hard-code
  `zap.UseDevMode(true)` in production and must expose liveness/readiness
  probes. The two-logger-library JSON mismatch (`slog`+`zap`) is
  *recommended* to resolve, gating only if it breaks log ingestion.
- [ ] **HA manifest defaults** (HA half of [Q34](../STATUS.md),
  *recommended*): PDB, PriorityClass, `startupProbe`,
  `terminationGracePeriodSeconds` on the GMC. Single-replica is
  acceptable for a 1.0 default if documented.
- [ ] **Metrics are scrapeable and documented accurately**
  ([Q72](../STATUS.md) + [Q51](../STATUS.md), *recommended*): per-tenant
  metrics Services/ServiceMonitors exist, and every metric named in the
  docs is either registered in code or marked `(planned)`.

### E. Documentation honesty — *gating*

Because scale and sandboxing are deferred, the docs must not imply they
are validated.

- [x] **Capacity claim reframed** (Q99, 2026-06-15): the
  "thousands of sessions per AGC" figure is now labelled a **design
  target, not yet validated at scale** (1.0 ships without the
  [Q13](../STATUS.md) load run) at [README](../../README.md) Tier 2 and
  [executive-summary](../design/01-executive-summary.md), anchored to
  the validation-status note added to
  [Appendix A](../design/appendix-a-capacity-slos.md). The ~60 KiB
  per-goroutine cost is now stated as a design estimate, not a measured
  result.
- [x] **Egress enforcement scope stated honestly** (lifted 2026-06-11
  by Q7b): the negative path (non-proxy egress blocked) is now verified
  on the Calico kind profile, so the "unverified" caveat is gone. What
  the docs must still state — and now do
  ([tenant-onboarding](../operations/tenant-onboarding.md),
  [security-operations](../operations/security-operations.md),
  [network-architecture](../design/network-architecture.md)) — is that
  enforcement is the cluster CNI's job: production operators must run a
  CNI that enforces egress NetworkPolicy (Calico/Cilium); kindnet does
  not.
- [x] **Sandboxed runtime stated as untested opt-in** (Q99,
  2026-06-15): [Appendix B](../design/appendix-b-worker-isolation.md)
  now carries a validation-status note stating the gVisor/Kata path is
  *documented and supported in spec but not exercised on a real cluster
  as of 1.0* (validation deferred post-1.0, Q15).
- [x] **SLSA-L3 claim dropped to an honest qualifier** (Q99,
  2026-06-15): the provenance predicate ([Q103](../STATUS.md)) is **not**
  yet implemented, so the four Dockerfiles no longer assert "(SLSA-L3)" —
  they now state the build is a reproducible-build input for SLSA-L3 with
  provenance attestation not yet emitted (pointing at Q103). The strong
  parts of the supply-chain story that *are* true (SHA-pinned actions +
  tags-only keyless cosign signing + per-arch SBOM, Q123/Q124; vendor-vs-
  `go.sum` gating + cosign checksum, Q126/Q127) remain described in
  [05-security.md](../design/05-security.md), which already used the
  honest "SLSA-L3-friendly" wording.
- [x] **`docs/operations/` reflects the install artifact** (Q99,
  2026-06-15): onboarding/runbook/upgrade docs describe the real `helm
  install` / `helm upgrade` flow (cert-manager toggle + CRD-upgrade-via-
  templates note verified honest and current), not the per-binary
  `cmd/*/config/` bases. **Q98 carve-out remains:** the `oci://` chart-
  pull references depend on Q98's first live chart publish — until then
  [install.md](../operations/install.md) install-from-source-checkout is
  the honest path and an HTML `<!-- Q98: … -->` marker flags where the
  registry instructions slot in once the first `v*` tag publishes.

### F. Engineering quality gates — *gating*

CI-enforced quality measurements that keep an AI-developed, multi-session
codebase from regressing silently between sessions. Most are cheap to wire;
where a threshold applies, the deliverable is as much the **agreed bar** as
the plumbing, and those gate by *not getting worse* to avoid manufacturing
low-value tests or churn.

- [x] **Test coverage measured + gated** ([Q77](../STATUS.md), *gating*):
  a `coverage` job in `unit-test.yml` measures per-module `go test` coverage
  and gates it with a *no-regression ratchet* rather than an arbitrary absolute
  percentage. [`scripts/coverage.sh`](../../scripts/coverage.sh) is the single
  source of truth (`make cover`/`cover-check`/`cover-update`); per module it
  takes `go test -coverprofile` and computes the aggregate with `go tool cover
  -func` over a profile **filtered** of mechanically-generated code
  (`zz_generated*`, `groupversion_info.go`), so a CRD-field add can't trip the
  gate without a real test change. `main.go` is **not** excluded (departing
  from the original "exclude thin main/wiring" sketch): cmd/worker and cmd/proxy
  keep unit-tested logic in `package main`, so a blanket exclusion would hide
  tested logic and leave them ungated; the thin agc/gmc entrypoints just yield a
  lower-but-defended floor, which costs a ratchet nothing. Floors live in
  [`coverage-baseline.txt`](../../coverage-baseline.txt); `cover-check` fails
  only if a module drops **>0.5pp** below its floor (small tolerance absorbing
  benign denominator drift — coverage is deterministic, so it's not for flake).
  **Recorded baseline:** broker 48.3%, cmd/agc 77.4%, cmd/gmc 48.2%, cmd/proxy
  72.0%, cmd/worker 72.0%, githubapp 81.8%; cmd/probe and test/fakegithub 0.0%
  (no tests yet). Kept out of `make check` (like `test-race`/`vulncheck`) so the
  fast loop doesn't double-run the suite. Documented in
  [`docs/development/testing.md`](../development/testing.md).
- [x] **Code-duplication check** ([Q78](../STATUS.md), *gating*): enabled
  `dupl` in the root `.golangci.yml` at the conventional 150-token threshold,
  so it runs per-module the same way CI lints. Triage of the initial run found
  the only clones above threshold were table-style test functions (the
  cmd/agc ScaleUp/ScaleDown and VersionTooOld/RateLimited condition tests),
  which read more clearly kept separate; these are suppressed by a single
  `dupl`-on-`_test.go` exclusion. The exclusion is scoped to test files rather
  than all of `internal/*` (as `cmd/gmc/.golangci.yml` does) so dupl stays
  active on production code — including the builder.go `SecurityContext`
  copy-paste this check is here to catch. Production code is clean at 150.
- [x] **Unit tests run under `-race`** ([Q79](../STATUS.md), *gating*):
  the `unit-test.yml` job now runs the per-module unit tests under the race
  detector, not just integration. The multiplexing core (agentpool,
  listener/mux, broker, token) is where data races hide and was previously
  never race-checked in the gate — the class the [Q76](../STATUS.md) pool
  race belongs to. No threshold: `-race` is pass/fail. **Design decision on
  the fast-job-vs-separate-job call** (it roughly doubles unit runtime): a
  dedicated `make test-race` target carries the race flags and the same
  local throttle/parallelism cap as `make test`, and CI invokes it; `make
  test`/`make check` stay plain so the dev loop isn't silently turned into
  an unthrottled `-race` run (which is the single command most likely to
  trip the macOS WindowServer watchdog). The race gate is reproduced locally
  with `make test-race`, documented alongside `make vulncheck` as a heavier
  opt-in gate in [`docs/development/testing.md`](../development/testing.md).
- [x] **`gosec` security linting** ([Q80](../STATUS.md), *gating*): enabled
  gosec in the root `.golangci.yml`, so it runs per-module the same way CI
  lints. Noisy/redundant rule families are excluded wholesale with a
  per-family justification in the config (G104 redundant with errcheck;
  G109/G115 integer-overflow on bounded conversions; G304 trusted-path file
  reads; G703/G704/G706 experimental taint analysis vs. the forward proxy's
  by-design dialing). Every remaining accept carries a targeted
  `//nolint:gosec // Gxxx: reason`. The pre-existing dead markers
  (`broker/crypto.go` SHA-1 G401/G505, listener jitter G404) now actively
  suppress their findings — verified by strip-and-restore.
- [x] **`errcheck` across all modules** ([Q81](../STATUS.md), *gating*):
  promoted errcheck from GMC-only (`cmd/gmc/.golangci.yml`) to the root
  `.golangci.yml`, so unchecked errors are caught in
  agc/broker/proxy/worker/githubapp/probe too. The batch it surfaced
  (~80 sites, mostly `resp.Body.Close()`/conn closes and test-helper
  `fmt.Fprint`/`io.Copy`) was fixed with real error checks or `_ =`
  ignores; no blanket suppression.
- [x] **Shell linting** ([Q84](../STATUS.md), *gating*): `shellcheck` over the
  standalone helper scripts in a dedicated `shellcheck` job in `unit-test.yml`
  and a `make shellcheck` target wired into `make check`, so the local gate
  matches CI. The glob is the git pathspec `scripts/*.sh` resolved through
  `git ls-files` — tracked-only and recursive (git's default `*` spans `/`, so a
  future `scripts/<subdir>/*.sh` is covered automatically without re-touching the
  gate). The CI job **pins shellcheck (`v0.11.0`)** instead of the runner image's
  drifting preinstalled copy, because its SC2015 heuristics differ between
  releases — an unpinned gate gave a different verdict locally vs. CI. Previously
  only inline workflow `run:` blocks were checked (via `actionlint`); the
  standalone scripts (`setup.sh`, `kind-with-registry.sh`, `start-registry.sh`, …)
  shipped unlinted — a quoting/`set -e` defect there breaks the install or e2e
  flow this product gates on. Findings fixed across `probe-investigations-cd.sh`
  and `probe-live-run.sh`: the `gh`-CLI-dispatch `A && B || C` (SC2015) became a
  real `if/else`, and the best-effort `(cd … && remove … || true)` cleanups were
  rewritten as `(cd … && remove …) || true` to drop the SC2015 pattern; the two
  intentional dynamic-name `read`/`export` warnings (SC2229/SC2163) carry targeted
  `# shellcheck disable` directives with a justifying comment. Now green, so the
  shared image-pull-retry helper deferred from #150 can be extracted with lint
  coverage intact.
- [x] **Install artifact validates** ([Q66](../STATUS.md), *gating*): the
  `manifest-validate.yml` workflow runs `yamllint` over the static manifests +
  chart metadata and `kubeconform` (at the chart's 1.30.0 `kubeVersion` floor)
  over the kustomize-rendered default overlay, the standalone opt-in manifests,
  and the `helm template` output in both default and all-features form, plus
  `helm lint`. Reproduced locally by `make manifest-validate`. yamllint is
  tuned (in `.yamllint.yaml`) to catch real defects while relaxing cosmetic
  rules that only fire on generated CRD/scaffold style; kubeconform's
  `-ignore-missing-schemas` is scoped to third-party/custom kinds (cert-manager,
  Prometheus Operator, our own CRs), with the CRDs that define them still
  validated. A malformed RBAC/CRD/policy file — a release defect for an
  install-artifact product — now fails CI. (This gate schema-validates each CRD
  independently; the GMC-bundled-vs-AGC-source RunnerGroup CRD *drift* check
  stays tracked separately as [Q73](../STATUS.md), since kubeconform does not
  compare the two copies for field skew.)

### G. Must-resolve before tag — fix or fold into the Q99 honesty pass

These are **not** separate `1.0-gate` rows, but 1.0 may not tag until each
is **either fixed or explicitly caveated** in the docs-honesty pass
([Q99](../STATUS.md)). Each leaves a documented security or
release-integrity claim false if shipped silently — the exact failure the
1.0 bar exists to prevent — so "resolved" means a real fix *or* an honest
docs caveat, not omission.

- ~~**Unrestricted port-53 egress** ([Q105](../STATUS.md)): `builder.go`
  emits a port-53 egress rule with no `To` peer, so workers/proxy can
  resolve via any DNS server — a DNS-exfil channel that undercuts the
  per-tenant egress-IP isolation claim.~~ **Resolved:** all three per-tenant
  NetworkPolicies (workload, AGC, proxy) now confine port-53 egress to the
  cluster DNS service (`k8s-app: kube-dns` in `kube-system`); guarded by the
  authoring test `TestBuildNetworkPolicy_DNSEgressRestrictedToKubeDNS`. See
  [05-security.md](../design/05-security.md) § DNS Exfiltration Side-Channel.
- **Release-integrity siblings of [Q123](../STATUS.md)/[Q124](../STATUS.md)** —
  vendored deps are never hash-verified against `go.sum`
  ([Q126](../STATUS.md)), and the cosign binary in the signing pipeline is
  downloaded without a checksum (the cosign item in [Q127](../STATUS.md)).
  Both admit an unverified artifact into a signed release. Fix
  (re-vendor + `git diff --exit-code` CI gate; checksum-pin cosign as the
  `KIND_BINARY_SHA256` pattern does) or caveat.
- ~~**Library `generateKey` empty-keyType default is Ed25519**
  ([Q109](../STATUS.md)): an empty `keyType` yields Ed25519 — the
  secure-by-default regression `CLAUDE.md` explicitly forbids (Ed25519
  agents can't decrypt RSA-OAEP session keys). Only the CLI default holds
  RSA; the **library** default must be RSA too.~~ **Resolved (Q109):**
  `generateKey` now defaults an empty/unrecognised `keyType` to RSA-3072,
  matching the CLI; Ed25519 requires the explicit `KeyTypeEd25519` opt-in.

---

## Explicitly out of scope (post-1.0)

Deferred deliberately because they are expensive to validate relative to
their gate value, and their absence is a *documented limitation*, not a
*defect*:

| Deferred | Was | Why out of 1.0 | Revive trigger |
|---|---|---|---|
| **Load test harness + 1,000-session run** | [Q13](../STATUS.md), [M5 §2](milestone-5.md) | Building `test/load/` and standing up a cluster big enough to sustain 1,000 concurrent sessions is multi-session L work; the capacity number is reframed as a design target instead. | First external interest in capacity SLOs, or a perf regression report. |
| **gVisor / Kata RuntimeClass validation** | [Q15](../STATUS.md), [M5 §4](milestone-5.md) | Requires a cluster with the runtime installed (nested-virt / special nodes); operator concern, low gate value for a first release. The opt-in is documented. | A tenant requires sandboxed isolation, or staging gains a gVisor node pool. |

Both retain their Queue/Deferred rows in [STATUS.md](../STATUS.md);
this doc only states they are not 1.0 gates.

---

## Critical path & ordering

Two independent tracks land in parallel; packaging is the long pole.

```
Track 1 (live validation — needs real GitHub App creds + kind):
  A (M4 multi-tenant + e2e job + Q71 runner version)
        └─ B-egress (Q7b — DONE 2026-06-11 on the Calico kind profile)

Track 2 (packaging — the keystone):
  C-Q12 (install artifact)
        ├─ C-Q14 (posture scan)        ┐ all block on an artifact
        ├─ C-Q28 (sign + SBOM)         │ existing to install/scan
        └─ C-Q29 (audit policy sample) ┘

Independent (any time):
  D (Q35 AGC logging/probes; Q34 manifests; Q51/Q72 metrics)
  F (Q77 coverage; Q78 dup-check; Q79 -race; Q80 gosec; Q81 errcheck;
     Q84 shellcheck; Q66 manifest validation) — CI-only, no cluster needed
  E (docs honesty pass) — finish last, once A–D outcomes are known
```

Suggested sequence:

1. **Q12 install artifact** — unblocks the entire C track; start first.
2. **A: M4 live validation + Q71** — one focused session with real
   creds against `kind`; flips four ⚠️/❌ DoD rows at once.
3. **B: Q7b egress negatives** (*recommended, not gating*) — ran
   2026-06-11 on the `make e2e-cluster KIND_CNI=calico` profile; the
   bucket-E egress caveat is lifted (the production CNI requirement
   statement remains in the operator docs).
4. **C: Q14 posture scan** (then optionally Q28/Q29) once Q12 lands.
5. **D: Q35/Q34 operability fixes** and **F: Q77/Q78 CI quality gates** —
   parallelizable, no cluster needed.
6. **E: docs honesty pass** — last, so the scale/sandbox caveats and the
   new install flow are written against what actually shipped.

---

## Gate summary

| Bucket | Gating items | Recommended items |
|---|---|---|
| A. Functional + live proof | M4 multi-tenant, delete-isolation, e2e proxy job, Q71, Q118 (runner-version contract), Q125 (teardown not fail-open) | — |
| B. Security/isolation | Q34 secure-by-default, Q121 + Q122 (documented RBAC scope vs. install artifact) | Q7b egress negatives (✅ ran 2026-06-11) |
| C. Packaging/supply chain | Q12, Q14 | Q28, Q29 |
| D. Operability | Q35 (logging+probes) | Q34 HA, Q51, Q72, Q35 logger unify |
| E. Docs honesty | capacity reframe, egress + sandbox caveats, ops install flow, Q103 SLSA-L3 claim | — |
| F. Engineering quality | Q77 coverage, Q78 dup-check, Q79 `-race` unit, Q80 gosec, Q81 errcheck, Q84 shellcheck, Q66 install-artifact validation | — |
| G. Must-resolve (fix **or** caveat → Q99) | Q105 port-53 egress, Q126 + Q127-cosign release integrity, ~~Q109 Ed25519 library default~~ (fixed) | — |

**1.0 = all gating boxes ticked**, and every bucket-G item either fixed or
honestly caveated. Recommended items that slip become ordinary post-1.0
Queue entries.
