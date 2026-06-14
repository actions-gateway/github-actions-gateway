# Security Audit 2 — June 2026

Second full security audit, performed 2026-06-12. Four parallel review
tracks — GMC tenant isolation & provisioning, proxy/broker/probe network
trust boundaries, CI/CD supply chain, and AGC credential/crypto handling —
plus `make vulncheck` (govulncheck: clean across all 8 modules). The first
audit (2026-05) and its W/C/H/M/L workstreams live in
[security.md](security.md); this doc records only the second audit's
findings and their disposition.

**Method note:** each track read the claimed security properties in
`docs/design/05-security.md` / `02-architecture.md` /
`network-architecture.md` first, then verified the code implements them.
The recurring theme in the new findings is *claim-vs-code mismatch*:
documented isolation properties that are client-side hygiene or
unimplemented at the authorization layer.

## Table of Contents

- [Disposition of all findings](#disposition-of-all-findings)
- [Queued findings (detail)](#queued-findings-detail)
- [Q127 hardening-batch items](#q127-hardening-batch-items)
- [Verified strengths](#verified-strengths)
- [Coverage notes](#coverage-notes)

## Disposition of all findings

| Finding | Severity | Disposition |
|---|---|---|
| GMC ClusterRole grants cluster-wide Secret read/write; docs claim name-scoped | High | **New → [Q121](../STATUS.md#Q121)** (Q29 audit policy is the detective half) |
| GMC workload writes cluster-wide; docs claim CR-namespace confinement | High | **New → [Q122](../STATUS.md#Q122)** |
| Worker pods have no ingress NetworkPolicy (default-allow ingress) | High | **Resolved (Q128)** — workload NP now declares `policyTypes: [Ingress, Egress]` with an empty ingress rule set (default-deny) |
| No GitHub Actions SHA-pinned; publish.yml runs tag-pinned actions with `id-token: write` | High | **Resolved (Q123)** — every `uses:` SHA-pinned, syft version-pinned, Dependabot `github-actions` ecosystem bumps the pins |
| `make verify-release` accepts `refs/heads/.*` signing identities | Medium | **Resolved (Q124)** — identity regexp anchored tags-only (`@refs/tags/v.*$`); publish.yml refuses non-tag refs; regexp guarded by `scripts/verify-release-test.sh` |
| GMC teardown fail-open (`deleteIfExists` swallows errors, finalizer removed) | Medium | **New → [Q125](../STATUS.md#Q125)** |
| Vendored deps never integrity-checked against go.sum in CI | Medium | **New → [Q126](../STATUS.md#Q126)** |
| 8 smaller hardening items (see below) | Low | **New → [Q127](../STATUS.md#Q127)** (batch) |
| DNS egress allows port 53 to any destination | Medium | Known — [Q105](../STATUS.md#Q105) |
| Proxy has no app-layer destination allowlist / connection cap | Medium | Accepted by design — security.md M-2, Appendix G §G.1, [Q19](../STATUS.md#Q19). This audit adds the metadata-service/SSRF framing as a revisit argument |
| ResourceQuota is optional and tenant-controlled | Medium | **Resolved (Q130, 2026-06-14):** the tenant-authored `spec.namespaceQuota` was removed; the `ResourceQuota` is now platform-owned (the platform admin must provision it on the namespace), so it is no longer tenant-controlled. Remaining per-cluster proxy HPA-max guard work stays in [Q82](../STATUS.md#Q82). |
| No SLSA provenance attestation | Info | Known — [Q103](../STATUS.md#Q103) |
| Worker trivy leg report-only | Info | Known — [Q70](../STATUS.md#Q70) |
| ServiceMonitor `insecureSkipVerify` | Low | Known — [Q104](../STATUS.md#Q104) |
| Library agent-key-type default Ed25519 | Low | Known — [Q109](../STATUS.md#Q109) |
| Docs claim CRD CEL rejects reserved podTemplate fields; no such rules (runtime overwrite layer does exist and holds) | Info | Docs-honesty — fold into [Q99](../STATUS.md#Q99) |
| `GITHUB_API_BASE_URL` env override of token exchange | Low | Residual of M-14 fix (`--allow-agc-extra-env` default-off); guard option listed under Q127 |
| Missing explicit `permissions:` on read-only workflows | Info | Repo default verified read-only; declare-in-file is a nice-to-have, not queued |
| GHCR version tags mutable + re-dispatchable | Info | Mitigated by digest-pin consumption + Q124 fix; revisit if GHCR ships tag immutability |

## Queued findings (detail)

### Q128 — Worker pods lack ingress NetworkPolicy (High) — RESOLVED

`buildWorkloadNetworkPolicy` (`cmd/gmc/internal/controller/builder.go`)
set `PolicyTypes: [Egress]` only; the AGC and proxy policies declared
ingress but nothing selected worker pods (`actions-gateway/component:
workload`) for ingress, so they were default-allow. Any pod in the cluster
could connect to a worker running untrusted job code; combined with the
unrestricted port-53 egress (Q105), two malicious workflows in different
tenants could form a cross-tenant channel. Contradicted the "fully isolated
within the tenant's namespace" claim (`02-architecture.md:7`).

**Resolution:** the workload NP now declares
`policyTypes: [Ingress, Egress]` with an empty ingress rule set
(default-deny — workers accept no inbound by design; they are outbound-only,
dialing the proxy and AGC). The AGC pod is also selected by the workload NP
but its own `buildAGCNetworkPolicy` additively re-admits the monitoring
metrics scrape, so default-deny costs it nothing. kubelet liveness/readiness
probes originate from the node and are not subject to NetworkPolicy, so
health checks are unaffected. Guarded by the spec-level authoring test
`TestBuildWorkloadNetworkPolicy_DefaultDenyIngress` (the reliable CI gate —
kindnet does not enforce ingress policy) and the e2e `WorkloadNPSpec`
assertion; the [network-architecture.md validation
section](../design/network-architecture.md#how-to-validate-network-isolation)
adds a manual runtime probe for policy-enforcing CNIs.

### Q121 — GMC Secret RBAC is cluster-wide; docs claim name-scoped (High)

`cmd/gmc/config/rbac/role.yaml:38-47` grants `secrets:
create,get,list,update,watch` with no `resourceNames`, bound by a
ClusterRoleBinding. `05-security.md:13-16` claims "no cluster-wide Secret
read", metadata-only list, and "`get` on specific names only". The
metadata-only watch (`WatchesMetadata`) and cache bypass
(`DisableFor[*corev1.Secret]`) are client-side hygiene — RBAC lets a
compromised GMC `get`/`list` full `.data` of every Secret in the cluster
and `create`/`update` Secrets anywhere. The first audit's §GMC-1 residual
("metadata only") understated this (corrected 2026-06-12).
**Fix:** ValidatingAdmissionPolicy confining GMC Secret reads/writes to
tenant-marked namespaces (the `namespace-psa-guard` pattern), per-namespace
Roles at onboarding, or correct the docs so operators don't rely on a
non-existent property. Q29's audit-policy sample is the detective
complement either way. *Interim (2026-06-12): the inaccurate claims in
`05-security.md` are struck through with corrections in place; restoring
clean prose there is part of resolving this item.*

### Q122 — GMC workload writes are cluster-wide; docs claim confinement (High)

`role.yaml:84-158`: deployments, rolebindings, networkpolicies, services,
serviceaccounts, resourcequotas, HPAs, PDBs — all verbs, all namespaces.
`05-security.md:13` and `02-architecture.md:66` claim writes are "limited
to namespaces where an `ActionsGateway` CR exists"; nothing enforces that.
A compromised GMC can create a Deployment or RoleBinding in `kube-system`.
**Fix:** extend the `namespace-psa-guard` VAP approach — deny GMC-SA
writes of these kinds in namespaces lacking the tenant marker label —
and/or update the docs. *Interim (2026-06-12): the claims in
`05-security.md` and `02-architecture.md` are struck through with
corrections in place pending resolution.* **Partially resolved (Q130,
2026-06-14): the `resourcequotas` write verb has been dropped entirely —
the namespace `ResourceQuota` is now platform-owned and the GMC no longer
creates or mutates it, shrinking the cluster-wide-write surface by one
kind. The remaining kinds (deployments, rolebindings, networkpolicies,
services, serviceaccounts, HPAs, PDBs) are still unconfined — Q122 stays
open for the VAP-confinement work.**

### Q123 — SHA-pin GitHub Actions; publish.yml first (High) — RESOLVED

Every `uses:` in `.github/workflows/` was tag-pinned;
`anchore/sbom-action/download-syft@v0` floated a whole major. The publish
job holds `packages: write` + `id-token: write`: a hijacked action tag
executes inside the job whose ambient OIDC identity *is* the release trust
root — it could push and **keyless-sign malicious images as the legitimate
publish identity**, which `make verify-release` would accept.

**Resolution:** every `uses:` across all ten workflow files is pinned to a
full 40-char commit SHA with a trailing `# vX.Y.Z` comment (publish.yml
first). Runtime tool downloads in the publish path are version-pinned too:
`cosign` already via `cosign-installer` `cosign-release`, and `syft` now via
an explicit `syft-version` input on `download-syft` (the action is
SHA-pinned, but syft is a runtime download). Dependabot already declared the
`github-actions` ecosystem (`.github/dependabot.yml`); Dependabot natively
bumps SHA pins and their `# vX.Y.Z` comments, so the pins don't rot. Policy
+ bump procedure documented in
[release.md § Supply-chain integrity of the pipeline](../operations/release.md#supply-chain-integrity-of-the-pipeline-itself);
`actionlint` (CI `lint`) keeps SHA-pinned `uses:` lint-clean.

### Q124 — `make verify-release` accepts branch identities (Medium) — RESOLVED

`scripts/verify-release.sh` (post-#209 home of the recipe; was `Makefile:511`)
matched `publish\.yml@refs/(tags|heads)/.*$`; the documented identity
(`security-operations.md`, `release.md`) is tags-only. `publish.yml` is
`workflow_dispatch`-able from any ref with an arbitrary `tag` input and runs
the workflow file *from that ref*: repo-write could dispatch from a scratch
branch, overwrite a released GHCR version tag, and still pass verification.

**Resolution:** the identity regexp is anchored tags-only —
`@refs/tags/v.*$` — and factored into `release_identity_regexp` in
`scripts/lib/common.sh` so it has a single source of truth. Defense in
depth: both `publish.yml` jobs' "Resolve publish tag" step now refuse any
`GITHUB_REF` that isn't `refs/tags/…`, so a branch `workflow_dispatch` can't
even reach the sign step. `scripts/verify-release-test.sh` (run by `make
check` and the CI shellcheck job) asserts the regexp accepts
`refs/tags/vX.Y.Z` and rejects `refs/heads/…`.

### Q125 — GMC teardown fail-open (Medium)

`reconcileDelete` (`actionsgateway_controller.go:222-272`) error-checks
RunnerGroup deletion, but the AGC Deployment, proxy resources,
NetworkPolicies, RoleBinding, and ServiceAccounts go through
`deleteIfExists` (`:274-280`), which logs and discards non-NotFound
errors; the finalizer is then removed unconditionally. A transient API
failure orphans a live credentialed AGC Deployment + RoleBinding after
tenant offboarding, with no retry and only a log line.
**Fix:** collect `deleteIfExists` errors and requeue without removing the
finalizer until every delete succeeds or is NotFound.

### Q126 — CI gate: vendor contents vs go.sum (Medium)

With `-mod=vendor`, the Go toolchain checks only `vendor/modules.txt`
consistency — never the hashes in `go.sum`. No CI step re-vendors and
diffs, so a malicious edit inside a large vendor diff compiles into the
signed release binaries unchecked (the license-notices job checks license
drift only). Sibling of Q94 (go.sum tidiness) and Q111 (Dependabot vendor
sync), which don't cover integrity.
**Fix:** CI job running `go work vendor` (re-fetches with go.sum
verification) + `git diff --exit-code vendor/ tools/vendor/`.

## Q127 hardening-batch items

Small items, one Queue row; fix opportunistically or as one PR:

1. **`namespace-psa-guard` hardcodes the GMC SA username**
   (`cmd/gmc/config/admission-policy/namespace-psa-guard.yaml:44`). A GMC
   installed under any other namespace/name is silently not subject to the
   policy. Parameterize via deploy tooling; add an e2e assertion.
2. **No ActionsGateway-singleton guard per namespace.** Two CRs fight over
   fixed-name resources; with different `securityProfile`s the namespace
   PSA labels flap (intermittently admitting privileged pods), and
   deleting either CR tears down the survivor's infrastructure. Reject a
   second CR in `ValidateCreate`.
3. **`proxy.noProxyCIDRs` unvalidated** (`actionsgateway_types.go:47` →
   `builder.go:819`). `["github.com"]` silently bypasses the egress proxy
   for GitHub traffic, defeating egress-IP attribution. CEL-validate as
   CIDRs; reject/flag values matching GitHub endpoints.
4. **CONNECT listener lacks explicit `MinVersion`**
   (`cmd/proxy/proxy.go:219`) — metrics listener pins TLS 1.2, the CONNECT
   listener inherits the Go default. Pin explicitly.
5. **Tool downloads not checksum-verified** (cosign `Makefile:548`,
   kubeconform/kustomize `manifest-validate.yml`, shellcheck, polaris).
   GitHub release assets are replaceable for an existing tag; cosign is
   the sharpest case (it's the release verifier). Mirror the
   `KIND_BINARY_SHA256` pattern.
6. **Release images assemble from GHA BuildKit cache**
   (`publish.yml:129` `cache-from`). Layers of signed releases may come
   from cache, not the tagged source. Drop `cache-from` (or `no-cache:
   true`) on tag builds. Related: `moby/buildkit:buildx-stable-1` builder
   tag is floating — digest-pin it.
7. **AGC egress allows any destination on 443/6443**
   (`builder.go:327-332`, rationale in the function comment). The pod
   holding the GitHub App key has effectively unrestricted 443 egress.
   Scope to apiserver endpoints where the platform allows, else document
   the residual in `05-security.md` §5.2.
8. **Privileged-profile webhook incoherence.** The webhook rejects
   `privileged: true` on the GMC path unconditionally
   (`actionsgateway_webhook.go:126`), so the documented Kata/DinD
   privileged pattern (`05-security.md:161-178`) is rejected even under
   `securityProfile: privileged`; meanwhile direct RunnerGroup CRs skip
   the check entirely (PSA is the real backstop). Make the check
   profile-aware; document PSA as the enforcement layer for the direct
   path. Optional hardening from the same track: refuse non-HTTPS
   `GITHUB_API_BASE_URL` overrides outside dev mode
   (`githubapp/auth.go:129`).

## Verified strengths

Recorded so the next audit can diff against them:

- Credentials file-mounted, never env vars (GitHub App key 0440 + fsGroup;
  JIT config via per-job Secret volume, deleted on cleanup);
  `InsecureSkipVerify` in tests only; credential-bearing bodies routed
  through `githubapp.SanitizeBody`.
- Broker AES-CBC unpadding constant-time with a single error sentinel
  (M-3 fix holding); no untrusted deserialization beyond JSON-to-struct.
- Proxy: CONNECT passthrough (no TLS termination), slowloris/idle/lifetime
  caps (M-17/M-18 holding), h2-smuggling avoided (`http/1.1` forced),
  mTLS metrics with fail-fast cert loading, `readyz` gating.
- GMC: two-layer Secret cache isolation as designed (client-side);
  `agc-tenant-role` bind-not-escalate pattern; `namespace-psa-guard` VAP
  reads the marker from `oldObject` with `failurePolicy: Fail`;
  PSA-stamping is step 0 and fail-closed; securityProfile downgrade guard;
  cross-namespace `gitHubAppRef` rejected; image digest pinning enforced
  at startup; worker pod labels built fresh (tenants can't spoof NP
  selectors); floor invariants overwritten post-merge; collision-safe name
  truncation.
- Supply chain: no `pull_request_target`, no script injection, privileged
  jobs least-privilege, base images digest-pinned, distroless/nonroot
  COPY-only runtimes, offline vendored builds, keyless signing over
  digests with per-arch SBOM attestation, chart fail-closed on missing
  digests, `replace` directives point only to in-repo siblings, no
  curl-pipe-shell anywhere.

## Coverage notes

- The AGC-internal track's agent report was lost; its critical ground
  (TLS verification, JIT credential flow, token log hygiene, base-URL
  override) was re-covered directly, but **goroutine-level session-state
  race analysis in `cmd/agc` got lighter coverage** than other areas.
  A targeted `-race` pass or follow-up review would close it.
- `trivy` was not run locally (not installed); CI `security-scan.yml`
  covers it (worker leg report-only — Q70).
