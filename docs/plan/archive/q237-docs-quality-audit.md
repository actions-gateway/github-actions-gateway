# Docs quality audit (Q237): scoring the published docset against the six-goal rubric

> **Status: ✅ Done — audit complete (2026-06-30).** Fan-out audit of the
> published `docs/` set against the six-goal quality rubric in
> [documentation-standards.md](../../development/documentation-standards.md#goals-what-good-looks-like).
> 57 findings filed below. The audit **produced** this ranked list; the fixes
> are follow-on work tracked on the [STATUS Queue](../../STATUS.md) (see
> [Remediation](#remediation)). This is the recurring "docs-vs-code drift audit"
> that documentation-standards.md lists as the highest-value *Proposed* quality
> signal.

## Goal

Score the human-facing documentation against all six quality goals — not just
scannability — and produce a **ranked, evidence-backed findings list** an
operator or maintainer can act on. The rubric, in leverage order: (1) correct &
current, (2) findable, (3) complete enough, (4) fit-for-purpose, (5) usable, (6)
trustworthy in tone (no AI slop).

## Scope

**Audited — the published docset (~62 prose files + directory READMEs):**
`docs/*.md` (top-level), `docs/design/`, `docs/development/`, `docs/operations/`.

**Excluded:**

- `docs/plan/` and `docs/plan/archive/` (69 files) — internal working artifacts
  with their own lint (`plan-index-check`, backlog rules), not held to the
  published-docset rubric.
- Mechanical link/anchor rot — already covered by `make doc-links`; this audit
  targets *semantic* findability (missing-but-needed links, orphans, nav gaps),
  not broken hrefs.

## Headline results

| Goal | Findings | Severity mix | Read |
|---|---|---|---|
| **1 — Correct & current** | **36** | 17 high · 16 med · 3 low | **The dominant problem.** Docs-vs-code drift concentrated in operator copy-paste surfaces (YAML `apiVersion`, field paths, command names, ports, defaults). |
| 2 — Findable | 3 | — | `mkdocs.yml` nav + `operations/README.md` index gaps. |
| 3 — Complete enough | 2 | — | Onboarding / getting-started gaps. |
| 4 — Fit-for-purpose | 2 | — | Altitude/type mismatches. |
| 5 — Usable | 4 | — | Scattered copy-paste hazards. |
| 6 — Trustworthy / no-slop | 10 | mostly low | Acronym expansion + minor promotional tone. |

**Totals:** 57 findings — 20 high, 20 medium, 17 low.

The signal is unambiguous: **goal-1 correctness drift**, not style. 17 of the 36
goal-1 findings are high severity, and most break a copy-paste command or state a
wrong default an operator would rely on. This is exactly what the per-change
doc-update rule misses over time and why a periodic drift audit is the
highest-value quality signal.

## Verification

Findings were produced by source-inspecting agents, so per the repo's
verify-before-trusting rule they are unverified until exec-confirmed. A sample
of **~8 distinct high-severity goal-1 findings was verified directly against the
code** — CRD served version, `ActionsGatewaySpec` fields, worker/AGC
ServiceAccount name constants, the GMC finalizer domain, proxy metrics port, the
proxy CPU-limit default, `ResourceQuota` non-reconciliation, and the shipped CRD
set. **All confirmed; zero false positives in the sample.** Remaining findings
should still be re-confirmed at fix time (a few — e.g. the worker-image override
mechanism, which appears mis-stated in more than one doc — are subtle and worth a
careful check before editing).

## Cross-cutting themes

These patterns recur across files and are the natural fix batches:

1. **API-version / field-path drift in copy-paste YAML & commands** — v1alpha1
   vs `/v1` vs v2 confusion; non-existent fields (`spec.worker.podTemplate`),
   flags, ServiceAccount names, CRD groups. Files: `kata-dind-workloads.md`,
   `air-gapped-install.md`, `velero-backup-restore.md`, `tenant-onboarding.md`,
   `backup-restore.md`, `appendix-e-capacity-planning.md`.
2. **Stale numeric defaults** — `MaxListeners` (10→1), proxy CPU limit (100m→500m,
   wrong in 3 docs), AGC grace period. Files: `03-api-contracts.md`,
   `appendix-a-capacity-slos.md`, `observability.md`, `upgrade.md`.
3. **Worker-image override mechanism mis-documented** — a `--worker-image` flag
   / `WORKER_IMAGE` env var that the GMC does not consume; needs one
   source-of-truth correction. Files: `03-api-contracts.md`, `kind-iteration.md`.
4. **Stale CI/test-gate counts** — image counts (5 vs 6), `make check` gate list,
   trivy/hadolint scope. Files: `testing.md`, `backpressure.md`, `kind-iteration.md`.
5. **Metrics port confusion (8081 vs 8443)** — Files: `observability.md`,
   `troubleshooting.md`.
6. **Acronym expansion & minor tone** (goal 6, mostly low) — DinD/PSP unexpanded,
   a few promotional phrasings.

## Remediation

The fixes are follow-on work, split by leverage so each PR stays scoped:

- **Batch A (high) — operator copy-paste correctness:** the 17 high goal-1
  findings (broken `kubectl apply`/`patch`, wrong CRD group/version/fields/ports).
  Top priority; an operator following these today fails. Themes 1–2, 5.
- **Batch B (medium) — stale defaults, counts & internal drift:** the 16 medium
  goal-1 findings across design + development docs. Themes 2–4.
- **Batch C (low) — usability & tone polish:** the goal-5 (4) and goal-6 (10)
  findings; low reader impact, cheap to sweep in one pass.

Each batch is independent per-file editing and could be dispatched in parallel
(see [parallel-dispatch.md](../../development/parallel-dispatch.md)), but every fix
must re-confirm its finding against the code before editing.

## Full findings

Ranked by goal (leverage order), then severity. `Sev`: **H**igh / **M**edium /
**L**ow. Evidence (line refs + the code symbol each goal-1 finding contradicts)
is preserved in the audit run output; the `Issue`/`Fix` columns are abridged to
fit.

#### Goal 1 — Correct & current (36)

| Sev | File | Issue | Fix |
|---|---|---|---|
| H | `design/03-api-contracts.md` | The MaxListeners field is documented with a default of 10, but the code defaults it to 1. | Change the documented MaxListeners default from 10 to 1 in the §3.1 struct comment (and confirm no downstream sizing prose assumes 10). |
| H | `design/03-api-contracts.md` | The doc tells operators to override the default worker image via a `--worker-image` flag that does not exist; the override is the WORKER_IMAGE environment vari… | Replace "--worker-image flag" with "WORKER_IMAGE environment variable (set by the GMC on the AGC Deployment)". Note the same wrong claim exists in code comment… |
| H | `design/03-api-contracts.md` | The documented proxy-pod CPU limit default (100m) is wrong; the GMC sets a 500m CPU limit, and the code explicitly warns against 100m. | Change the documented proxy CPU limit default from 100m to 500m. |
| H | `design/06-implementation-phases.md` | The Milestone 4 deliverable lists ResourceQuota among the tenant resources the GMC reconciles, but the GMC does not create or mutate ResourceQuota — it is plat… | Remove `ResourceQuota` from the Milestone 4 GMC-reconciled resource list on line 56 (and drop/reword the M4 mermaid-equivalent), matching the Q130 platform-own… |
| H | `design/appendix-a-capacity-slos.md` | The per-proxy-pod CPU request/limit default is documented as '10m / 100m', but the actual GMC-stamped default CPU limit is 500m, not 100m. | Change the proxy CPU request/limit row to '10m / 500m' to match egressProxyResources/proxyResources. Optionally note the 100m→500m rationale (HPA scale-out und… |
| H | `design/appendix-e-capacity-planning.md` | The appendix mixes two incompatible API versions: every worked-example YAML uses the v1alpha1 inline `spec.runnerGroups[].podTemplate` shape, but E.9 explicitl… | Pick one API version and make the whole appendix consistent. If targeting v2, rewrite the worked-example YAMLs as separate `RunnerSet` (+ referenced `RunnerTem… |
| H | `development/code-generation.md` | The AGC regeneration section omits the DeepCopy step that its parallel GMC section correctly flags as mandatory, so following it after changing an AGC CRD type… | Change the AGC section to run `make -C cmd/agc generate` (or list both `generate` for deepcopy and `manifests`), matching the GMC section, so editing an AGC v1… |
| H | `development/dependency-updates.md` | The Go-module row says "9 modules" but the repo has 10 go.mod modules, and the tenth (api/, the shared v2 kinds) is not in .github/dependabot.yml, so it receiv… | Correct the count to 10 and either add a gomod entry for `/api` to .github/dependabot.yml or, if api/ is intentionally excluded, state that exclusion and how a… |
| H | `development/kind-iteration.md` | The doc tells the reader to bump the worker image on the GMC via env var WORKER_IMAGE, but the GMC consumes no WORKER_IMAGE env var — the worker/wrapper image… | Replace WORKER_IMAGE with WRAPPER_IMAGE in the line-149 parenthetical (and confirm the intended image: the runtime worker-wrapper injection env is WRAPPER_IMAG… |
| H | `operations/air-gapped-install.md` | The AGC and worker ServiceAccount names in the step-5 patch commands are wrong, so the copy-paste `kubectl patch serviceaccount` commands target ServiceAccount… | For a v1alpha1 install (the apiVersion used in the doc's step-6 RunnerGroup example), patch the fixed SAs: `kubectl patch serviceaccount actions-gateway-contro… |
| H | `operations/backup-restore.md` | The GMC cleanup finalizer name is given with the wrong domain, so the last-resort `kubectl patch` to remove it would not match the finalizer actually present o… | Change the finalizer to `actions-gateway.github.com/gmc-cleanup` for the v1alpha1 CR this doc describes; if v2 is also in scope, note both forms per v1alpha1-d… |
| H | `operations/kata-dind-workloads.md` | The ActionsGateway CR example declares apiVersion actions-gateway.github.com/v1, but the CRD serves only v1alpha1 — kubectl apply of this manifest fails with '… | Change the apiVersion to `actions-gateway.github.com/v1alpha1`. |
| H | `operations/kata-dind-workloads.md` | The CR example places runtimeClassName under spec.worker.podTemplate, but ActionsGatewaySpec has no `worker` or `podTemplate` field — worker pod config lives u… | Restructure the example so runtimeClassName sits under `spec.runnerGroups[].podTemplate.spec` (with `securityProfile: baseline` remaining at top-level spec, wh… |
| H | `operations/security-operations.md` | The License-attribution inspection command targets a Deployment named `gmc` with no namespace; the GMC Deployment is actually `gmc-controller-manager` in `gmc-… | Use `kubectl exec -n gmc-system deploy/gmc-controller-manager -- cat /licenses/LICENSE` (and note distroless has no shell, so this errors regardless; prefer th… |
| H | `operations/tenant-onboarding.md` | The CRD-installed pre-condition check names CRDs that do not exist, so both kubectl commands return NotFound even on a correctly installed cluster. | Change to `kubectl get crd actionsgateways.actions-gateway.github.com && kubectl get crd runnergroups.actions-gateway.github.com`. |
| H | `operations/troubleshooting.md` | The "Prometheus Not Scraping Proxy or AGC Metrics" runbook says the proxy and AGC /metrics endpoints are both on :8081 as unauthenticated plain HTTP, but in a… | Rewrite this runbook to point scrapes at :8443 over mTLS (matching the "Metrics scrape returns a TLS / connection error" runbook): scrape https on :8443 with t… |
| H | `operations/velero-backup-restore.md` | The doc names the GAG CRD API group as `actions-gateway.github.com` and lists all six CRDs under it, but four of them do not exist under that group. | Correct the group: the current (v2) shipped CRD set — actionsgateways, egressproxies, runnersets, runnertemplates, clusterrunnertemplates — lives under `action… |
| M | `design/03-api-contracts.md` | Four RunnerGroup fields are shown carrying `+kubebuilder:default` markers that do not exist on the actual type; their defaults are applied in Go code, so kubec… | Either drop the `+kubebuilder:default` markers from these four fields in the doc and state the defaults are applied in-code, or add the markers to the real typ… |
| M | `design/06-implementation-phases.md` | The Milestone 5 deliverable describes the Helm chart as shipping "per-tenant ResourceQuotas," implying GAG provisions them, which again contradicts the platfor… | Reword to make clear the ResourceQuota is set by the platform admin out-of-band (or drop it from the chart deliverable), consistent with the Q130 language in 0… |
| M | `design/appendix-e-capacity-planning.md` | The document consistently calls the worker-pool CR a `RunnerGroup` (and the label field `runnerLabels` on it), but under v2 — which the same doc references via… | Under v2, refer to `RunnerSet` (the per-set object) and reserve `RunnerGroup` for v1alpha1 context, or add an explicit version banner stating the appendix targ… |
| M | `design/appendix-g-future-enhancements.md` | G.6 states the probe binary is the Ed25519 detection mechanism, invoked as `cmd/probe -key-type ed25519`. The probe binary has no `-key-type` flag (it register… | Either add the `-key-type`/Ed25519 support to the probe and describe it accurately, or reword the detection note to describe a mechanism that exists (e.g. insp… |
| M | `design/network-architecture.md` | The doc asserts categorically that all GitHub-bound traffic is routed through the per-tenant egress proxy, but v2 direct-egress (proxy-less) mode is shipped, w… | Scope the intro/connection-map to the proxied topology explicitly (note it describes a gateway with an attached EgressProxy), and add a pointer to the v2 direc… |
| M | `development/backpressure.md` | The doc repeatedly enumerates `make check` as only 'gofmt + golangci-lint + STATUS.md lint + unit tests', but the actual Makefile `check` target runs many more… | Update the enumeration to reflect the current `check` prerequisites (or state it is a representative subset), e.g. add plan-index/no-plan-refs drift, go-versio… |
| M | `development/backpressure.md` | The Self-reinforcing assessment cites `claude-workspace-guard` and `claude-branch-guard` as real repo-tracked PreToolUse hooks providing 'real-time backpressur… | Either drop the `claude-` script-name framing (these guards are harness/CLAUDE.md-level, not repo-tracked hook scripts) or cite the two hooks that are actually… |
| M | `development/kind-iteration.md` | The stand-up section says `make e2e-images` builds five images (gmc/agc/proxy/worker/fakegithub) but the default bake target builds six — the `wrapper` image i… | Add `wrapper` to the image list in the line-11 comment (and to the parallel-build description in the `make e2e-images` Makefile help if you want them consisten… |
| M | `development/testing.md` | The Dockerfile-lint section states hadolint runs over "all five Dockerfiles", but CI now lints six. | Change "all five Dockerfiles" to six and add the scripts/dogfood/runner/Dockerfile leg to the description (noting it is a dev/reference image). |
| M | `development/testing.md` | The trivy section undercounts the scanned images ("five") and the blocking set ("four"), and omits the wrapper image entirely. | Say trivy scans six images; the blocking set is five (gmc, agc, proxy, fakegithub, and the FROM-scratch wrapper), with only worker report-only. Describe the wr… |
| M | `development/testing.md` | The `make check` description omits several gates the target actually runs — notably the chart CRD/RBAC/webhook drift gates, plan-index/no-plan-refs checks, and… | Add the chart-drift gates (chart-crds-check/chart-rbac-check/chart-webhook-check), plan-index-check/no-plan-refs-check, and scripts-test to the enumerated list… |
| M | `operations/kata-dind-workloads.md` | The GKE machine-family table lists N1 as supporting nested virtualization, but the repo's provisioning script rejects n1 and only accepts n2/n2d/c2/c2d, so an… | Drop N1 from the supported list (or note the shipped script only provisions n2/n2d/c2/c2d and n1 must be set manually) so the doc and script agree. |
| M | `operations/observability.md` | The 'Proxy metrics' section says the proxy exposes its metrics on its health/metrics port ':8081', but in the production mTLS posture /metrics is served on por… | Change ':8081' to ':8443' (the mTLS metrics port) in the Proxy metrics section intro so it matches lines 8/230; describe :8081 only as the plaintext health por… |
| M | `operations/release.md` | The doc claims both worker-related images are digest-pinned in the chart, but the chart only has a wrapper.image block — there is no worker.image.digest to pin. | Reword to make clear only `wrapper` is chart-pinned: e.g. "The `wrapper` image is digest-pinned in the chart (`wrapper.image.digest`, like `agc`/`proxy`); the… |
| M | `operations/upgrade.md` | The AGC drain step calls 30 seconds "the default" terminationGracePeriodSeconds, but the GMC stamps the AGC Deployment with terminationGracePeriodSeconds: 60. | Change the parenthetical to reflect the real default: e.g. "(the GMC sets 60s by default)" or drop the "(the default)" clause. |
| M | `operations/velero-backup-restore.md` | The CRD list mixes the legacy v1 resource `runnergroups` with the v2 resources (`runnersets`, `runnertemplates`, `clusterrunnertemplates`) as if all six are th… | Pick the generation the doc targets (v2 is the shipped chart) and drop `runnergroups` from the current-CRDs list, or explicitly flag it as the legacy v1 resour… |
| L | `design/appendix-b-worker-isolation.md` | The How-to prose names the field "the RuntimeClassName field on the WorkerPodTemplate", but the actual CRD field an operator edits is podTemplate (RunnerGroupS… | Reword to reference the `podTemplate` field (and `runtimeClassName` under `podTemplate.spec`), matching the YAML below and the CRD, e.g. "set `runtimeClassName… |
| L | `design/network-architecture.md` | The doc references only the v1 field path spec.proxy.managedNetworkPolicy; under the shipped v2 EgressProxy the equivalent knob is spec.managedNetworkPolicy (n… | Where the field is cited, note both forms (v1 `ActionsGateway.spec.proxy.managedNetworkPolicy`, v2 `EgressProxy.spec.managedNetworkPolicy`), or scope the doc t… |
| L | `operations/migration-v1-to-v2.md` | The v1 source field for the GitHub App Secret name is written with wrong casing 'spec.githubAppRef.name'; the actual v1 CRD JSON field is 'spec.gitHubAppRef.na… | Correct the v1 field reference to spec.gitHubAppRef.name (capital H) to match the v1alpha1 CRD. |

#### Goal 2 — Findable (3)

| Sev | File | Issue | Fix |
|---|---|---|---|
| M | `operations/README.md` | The operations index README does not list migration-v1-to-v2.md or v1alpha1-deprecation.md, so a reader browsing the operations directory on GitHub (the primar… | Add two rows to the operations/README.md table for migration-v1-to-v2.md and v1alpha1-deprecation.md (Persona: Platform engineer / tenant operator), ideally gr… |
| M | `mkdocs.yml` | docs/operations/admission-policies.md exists and is cross-linked from several docs but is absent from the mkdocs.yml Operations nav, so a site visitor browsing… | Add `- "Admission policies (Kyverno / Gatekeeper)": operations/admission-policies.md` to the Operations nav in mkdocs.yml (e.g. near security-operations.md). |
| L | `mkdocs.yml` | design/appendix-h-v2-api-decomposition.md is published and indexed in design/README.md but is omitted from the mkdocs Design nav, so site visitors can only rea… | Add `- "Appendix H — v2 API decomposition": design/appendix-h-v2-api-decomposition.md` to the Design nav after Appendix G, or, if the proposal is deliberately… |

#### Goal 3 — Complete enough (2)

| Sev | File | Issue | Fix |
|---|---|---|---|
| H | `getting-started.md` | The build prerequisite states 'Go 1.24+', but every Go module in the repo declares `go 1.26.4`, so a self-builder on Go 1.24 or 1.25 cannot build the images —… | Change getting-started.md:9 to 'Go 1.26+' to match go.work / all go.mod directives and the CONTRIBUTING.md prerequisite. |
| H | `operations/tenant-onboarding.md` | GitHub Enterprise Server (GHES) is documented as a first-class supported gitHubURL value, but no page tells a GHES operator that the managed proxy egress allow… | Add a GHES section to tenant-onboarding.md (and a troubleshooting entry) stating that for a GHES gitHubURL the managed GitHub-CIDR egress allowlist does NOT co… |

#### Goal 4 — Fit-for-purpose (2)

| Sev | File | Issue | Fix |
|---|---|---|---|
| H | `operations/tenant-onboarding.md` | A how-to (onboarding checklist) contains a non-runnable verification command with wrong CRD names, so the reader's copy-pasted "CRDs are installed" check alway… | Replace with the actual names: `kubectl get crd actionsgateways.actions-gateway.github.com runnergroups.actions-gateway.github.com` (matching install.md's Veri… |
| M | `operations/kata-ci-spike-runbook.md` | Altitude/type mismatch: a contributor/maintainer spike go/no-go artifact is filed in the operator docset (docs/operations/) and indexed for a "Platform enginee… | Move this spike runbook out of the published operator docset into docs/plan/ (or docs/development/) alongside kata-on-gke.md, or clearly mark it "internal / no… |

#### Goal 5 — Usable (4)

| Sev | File | Issue | Fix |
|---|---|---|---|
| M | `design/05-security.md` | Several threat-table Mitigation cells are single-paragraph walls of text hundreds of words long, defeating the table's scannability — a reader cannot extract t… | Move the long-form mitigation narrative out of the table into per-threat subsections below it (as §5.3 already does for the privileged opt-in), leaving the tab… |
| L | `development/kind-iteration.md` | The Cleanup paragraph pairs `make e2e-clean` with a claim that `.build/` persists across sessions, but that exact target deletes `.build/` — the juxtaposition… | Clarify: `make e2e-clean` also removes `.build/`; `.build/` otherwise persists across sessions between other targets — remove it manually if you suspect stale… |
| L | `development/networkpolicy-port-matching.md` | A code block uses a shell prompt prefix ($) with command output interleaved, which is a copy-paste hazard. | Split into a runnable command block (`kubectl get endpointslice -n default`, no `$` prefix) and a separate output block, per documentation-standards copy-paste… |
| L | `operations/upgrade.md` | Several `# Metric:` comment lines abbreviate metric names without the `actions_gateway_` prefix used in the code, so a copy-paste into PromQL would not match. | Use the fully-qualified metric names consistently (actions_gateway_* / controller_runtime_reconcile_errors_total) in the abbreviated comment lists. |

#### Goal 6 — Trustworthy / no-slop (10)

| Sev | File | Issue | Fix |
|---|---|---|---|
| L | `design/01-executive-summary.md` | The three leadership-audience subsections lead with bold promotional taglines that read as marketing framing rather than evidence-first statements. | Acceptable for an executive-summary explanation doc; no change strictly required. If tightening, ensure each tagline is immediately substantiated (it is). |
| L | `design/03-api-contracts.md` | GitHub Enterprise Server is abbreviated GHES on first use without a spelled-out expansion in this file. | Optional: no change required if the doc set is read in order (02 expands GHES first). Leave as-is or add a one-word gloss. |
| L | `design/appendix-d-alternatives-considered.md` | The headline memory-efficiency comparison (256 MiB per ARC listener, ~60 KiB per goroutine, "roughly a 4,000x difference", ~2.5 GiB vs ~600 KiB) is asserted re… | Attach a source or mark the figures as estimates (e.g. "order-of-magnitude estimate; not benchmarked"), consistent with the honest "Unverified — treat as a hyp… |
| L | `development/code-generation.md` | AGC and GMC acronyms are used throughout (including as section headings ## AGC, ## GMC) without first-use expansion, contra the documentation standard. | Expand AGC and GMC on first use in the doc body (the same low-severity note applies to building.md, which also uses AGC/GMC unexpanded). |
| L | `development/networkpolicy-port-matching.md` | The acronym DNAT is used throughout (including the title) but never expanded on first use. | Expand on first use, e.g. 'Destination NAT (DNAT)' in the TL;DR, per the CLAUDE.md/documentation-standards acronym-on-first-use convention. |
| L | `development/testing.md` | Several acronyms are used without first-use expansion in contributor-facing prose. | Expand on first use: Quality of Service (QoS), Container Network Interface (CNI), Destination NAT (DNAT), Service Level Objective (SLO), PodDisruptionBudget (P… |
| L | `index.md` | Security acronyms PSA, SBOM, and SLSA appear without first-use expansion, contradicting the docs convention to spell out acronyms on first use. | Expand on first use (e.g. "Pod Security Admission (PSA)", "Software Bill of Materials (SBOM) and SLSA provenance") or, given the tight card format, drop the ba… |
| L | `operations/admission-policies.md` | Two acronyms are used before their first-use expansion: DinD and PSP. | Expand on first use: 'Docker-in-Docker (DinD)' at line 43 and 'PodSecurityPolicy (PSP)-style' at line 190, per the doc's own acronym convention. |
| L | `operations/security-operations.md` | SIEM acronym is expanded on first use in the audit-log section but 'CNI' (Container Network Interface) is used without expansion across the egress sections; mi… | Expand CNI (Container Network Interface) on first use in security-operations.md, or accept it as a well-known term consistent with the glossary. |
| L | `why-gag.md` | Several acronyms are used without being expanded on first use, contrary to the repo's 'spell out acronyms on first use' documentation convention. | Expand on first use: 'Horizontal Pod Autoscaler (HPA)', 'Software Bill of Materials (SBOM)', 'Supply-chain Levels for Software Artifacts (SLSA)', 'Role-Based A… |

## How it was run

A background fan-out workflow (`q237-docs-quality-audit`): 22 per-file finder
agents scored prose files against the per-file goals (1, 5, 6) with correctness
spot-checks against the code; 3 tree-level agents covered the cross-file goals
(2 findable, 3 complete, 4 fit-for-purpose). 26 agents, ~2.4M tokens. Findings
were deduped and ranked into this report.
