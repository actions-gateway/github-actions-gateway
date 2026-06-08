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
not done at tag time moves to the post-1.0 Queue with its rationale.

### A. Functional completeness & live proof — *gating*

The code is done; what's missing is proof against a real cluster and
real GitHub. These close out [Milestone 4](milestone-4.md)'s unverified
DoD rows.

- [ ] **Two `ActionsGateway` CRs → two isolated tenants**, validated
  live on `kind` (not source-read): each tenant gets its own namespace,
  AGC, and proxy pool with ≥ `minReplicas` Ready pods. *(M4 DoD row 1)*
- [ ] **Deleting one CR tears down only that tenant**; the other
  tenant's namespace and workloads are unaffected. *(M4 DoD row 2)*
- [ ] **End-to-end job through the proxy**: a job dispatched from
  GitHub routes via `HTTPS_PROXY`, runs in a worker pod, and completes
  green. Lands as the planned Tier-A test
  `E2E_GMC_TenantProvisioning_ProxyConnectWorks`. *(M4 DoD row 5)*
- [ ] **Runner version contract confirmed** ([Q71](../STATUS.md)): run
  `E2E_GMC_TenantProvisioning` with real GitHub App creds and confirm
  GitHub accepts the pinned runner `2.334.0` at session creation.

### B. Security & isolation proof — *gating + recommended*

The security *control* (the egress NetworkPolicy) must ship
enforced-by-default; *observing* the negative path actually drop traffic
is recommended, not gating, because it needs a special-CNI cluster — the
same expense class as the deferred gVisor/load-test work. The honesty
caveat in bucket E covers the gap until Q7b runs.

- [ ] **No secure-by-default regression in shipped manifests**
  (the security half of [Q34](../STATUS.md), *gating*): the install
  artifact must not ship with NetworkPolicy / ServiceMonitor commented
  out. Anything off-by-default that weakens a security property is a
  documented, explicit opt-*out*, never a silent default
  (secure-by-default principle; see
  [security design](../design/05-security.md)).
- [ ] **Worker egress negatives observed enforcing** ([Q7b](../STATUS.md),
  *recommended*): re-run `WorkloadEgressBlockedToNonProxyPod` +
  `WorkerCannotReachK8sAPI` on a `kind` cluster with Calico or Cilium
  (kindnet does not enforce egress, so today's green is positive-case
  only). Deferred from gating per its special-cluster cost; the egress
  claim is caveated in the docs (bucket E) until it runs.

### C. Packaging & supply chain — *gating + recommended*

This is the keystone: nothing downstream (posture scan, signing, audit
policy) can exist until there is an artifact to install and scan.

- [ ] **Reproducible install artifact** ([Q12](../STATUS.md), *gating*):
  a Helm chart under `charts/actions-gateway/` (Helm decided over
  Kustomize per [D-M5-1](milestone-5.md#11-install-vehicle--decided-helm-chart))
  that produces a working tenant from a single `helm install`. Contents
  per [milestone-5.md §1.2](milestone-5.md): both CRDs, GMC
  Deployment + RBAC + webhook config, IP-range schedule, proxy image
  refs, an opt-in sample CR, **all images pinned by digest**. Two
  Helm-specific sub-gates from §1.1:
    - [ ] **cert-manager is optional**: `certManager.enabled` value with
      a self-signed-cert hook fallback, so the webhook installs without
      cert-manager present.
    - [ ] **CRDs upgrade**: shipped in `templates/` with
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

- [ ] **Capacity claim reframed**: anywhere the docs state the
  "thousands of sessions per AGC" figure, it is labelled a **design
  target, not yet validated at scale** (1.0 ships without the
  [Q13](../STATUS.md) load run). Remove or qualify any phrasing that
  reads as a measured result.
- [ ] **Egress enforcement scope stated honestly**: docs state that
  worker egress isolation is validated positive-case (job traffic flows
  through the proxy) but the negative path (non-proxy egress blocked) is
  enforced by the cluster CNI and is **unverified under the default
  kindnet test setup** — production operators must run a CNI that
  enforces egress NetworkPolicy (Calico/Cilium). Lifts when
  [Q7b](../STATUS.md) runs.
- [ ] **Sandboxed runtime stated as untested opt-in**: gVisor/Kata
  docs ([Appendix B](../design/appendix-b-worker-isolation.md)) say the
  path is *documented and supported in spec but not exercised on a real
  cluster as of 1.0*.
- [ ] **`docs/operations/` reflects the install artifact**:
  onboarding/runbook/upgrade docs describe the real `helm install` /
  `helm upgrade` flow (including the cert-manager toggle and the CRD
  upgrade note), not the per-binary `cmd/*/config/` bases.

### F. Engineering quality gates — *gating*

CI-enforced quality measurements that keep an AI-developed, multi-session
codebase from regressing silently between sessions. Most are cheap to wire;
where a threshold applies, the deliverable is as much the **agreed bar** as
the plumbing, and those gate by *not getting worse* to avoid manufacturing
low-value tests or churn.

- [ ] **Test coverage measured + gated** ([Q77](../STATUS.md), *gating*):
  `unit-test.yml` reports per-module `go test` coverage. **Threshold
  decision:** start report-only for one cycle to establish a baseline, then
  enforce a *no-regression ratchet* — the build fails if a module's coverage
  drops below its recorded floor. Prefer the ratchet over an arbitrary
  absolute percentage. Generated code (`zz_generated*`,
  `api/v1alpha1/groupversion_info.go`) and thin `main`/wiring packages are
  excluded from the floor so the number reflects logic, not boilerplate.
- [ ] **Code-duplication check** ([Q78](../STATUS.md), *gating*): `dupl`
  enabled in `.golangci.yml`. **Threshold decision:** start at the
  conventional 150-token threshold, then tune up only as far as needed to
  suppress false positives on table-driven tests and the divergent pod-spec
  builders. Catches the copy-paste-drift class that bit the AGC/proxy
  `SecurityContext` blocks (extracted in the builder.go helper refactor).
- [ ] **Unit tests run under `-race`** ([Q79](../STATUS.md), *gating*):
  `make test` and `unit-test.yml` add the race detector to the *unit* path,
  not just integration. The multiplexing core (agentpool, listener/mux,
  broker, token) is where data races hide and is currently never
  race-checked in the gate — the class the [Q76](../STATUS.md) pool race
  belongs to. No threshold: `-race` is pass/fail; the only call is
  fast-job-vs-separate-job since it roughly doubles unit runtime.
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
- [ ] **Shell linting** ([Q84](../STATUS.md), *gating*): `shellcheck` over
  `scripts/**` in `unit-test.yml`/`make check`. Today only inline workflow
  `run:` blocks are checked (via `actionlint`); the standalone helper scripts
  (`setup.sh`, `kind-with-registry.sh`, `start-registry.sh`, …) and CI install
  paths ship unlinted — a quoting/`set -e` defect there breaks the install or
  e2e flow this product gates on. Fix or justify the batch it surfaces; once
  green, the shared image-pull-retry helper deferred from #150 can be extracted
  with lint coverage intact.
- [ ] **Install artifact validates** ([Q66](../STATUS.md), *gating*; folds
  [Q73](../STATUS.md) CRD drift): `yamllint` + `kubeconform` on the
  manifests, and once the Helm chart ([Q12](../STATUS.md)) exists,
  `helm lint` + kubeconform on the rendered output. A malformed
  RBAC/CRD/policy file is a release defect for an install-artifact product.

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
        └─ B-egress (Q7b — RECOMMENDED, needs Calico/Cilium kind cluster)

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
3. **B: Q7b egress negatives** (*recommended, not gating*) — needs a
   Calico/Cilium cluster; piggyback on the same live session if the CNI
   is swapped in. If it doesn't run before tag, the bucket-E egress
   caveat ships in its place.
4. **C: Q14 posture scan** (then optionally Q28/Q29) once Q12 lands.
5. **D: Q35/Q34 operability fixes** and **F: Q77/Q78 CI quality gates** —
   parallelizable, no cluster needed.
6. **E: docs honesty pass** — last, so the scale/sandbox caveats and the
   new install flow are written against what actually shipped.

---

## Gate summary

| Bucket | Gating items | Recommended items |
|---|---|---|
| A. Functional + live proof | M4 multi-tenant, delete-isolation, e2e proxy job, Q71 | — |
| B. Security/isolation | Q34 secure-by-default | Q7b egress negatives |
| C. Packaging/supply chain | Q12, Q14 | Q28, Q29 |
| D. Operability | Q35 (logging+probes) | Q34 HA, Q51, Q72, Q35 logger unify |
| E. Docs honesty | capacity reframe, egress + sandbox caveats, ops install flow | — |
| F. Engineering quality | Q77 coverage, Q78 dup-check, Q79 `-race` unit, Q80 gosec, Q81 errcheck, Q84 shellcheck, Q66 install-artifact validation | — |

**1.0 = all gating boxes ticked.** Recommended items that slip become
ordinary post-1.0 Queue entries.
