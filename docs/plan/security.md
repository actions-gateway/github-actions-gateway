# Security Review Findings

A code-level security review of the GitHub Actions Gateway as of 2026-05-23.

Scope: `broker/`, `githubapp/`, `cmd/agc/`, `cmd/gmc/`, `cmd/proxy/`, `cmd/worker/`,
`cmd/probe/`. Each finding lists location, an OWASP-style category, severity,
description, and mitigation.

Severity rubric — based on *actual exploitability in this codebase as deployed*,
not on the worst-case of the vulnerability class:

- **Critical** — the documented security boundary is broken; cross-tenant or
  cluster-scope impact is reachable from a single compromised tenant or worker.
- **High** — single-tenant impact requiring deliberate fix; the documented
  control exists but is weaker than the design claims.
- **Medium** — defense-in-depth gap; another control covers it today but the
  layer is missing, weak, or operator-disableable. Or: a documented
  architectural contract is violated without granting new capability.
- **Low** — informational, latent risk, or hygiene.

The findings below complement [`docs/design/05-security.md`](../design/05-security.md);
the design doc describes the *intended* control surface, this file records
where the implementation diverges from that design, or where additional
controls are warranted.

---

## Status at a glance

Last refreshed 2026-05-25. Authoritative state for each finding lives in
its own section below — this table is the index. Status legend:
✅ done, ⚠️ partial (residual accepted), ❌ open, ⓘ informational /
by-design / accepted.

| ID | Finding | Severity | Workstream | Status | Notes |
|---|---|---|---|---|---|
| **C-1** | RunnerGroup PodTemplate not validated | Critical | W2 | ✅ Done 2026-05-23 | PSA labels + CEL `XValidation` rejecting `privileged: true` |
| **H-1** | GitHub App key in env var | High | W3 | ✅ Done 2026-05-23 | File mount at `/etc/actions-gateway/github-app/` mode `0o400` |
| **H-2** | AGC Role grants broad Secret access | High | W4 | ⚠️ Partial | `list` retained (needed by agentpool); `watch` over-declared (no informer registered); Secret cache disabled; residual accepted per D-3. See §GMC-1 for GMC's separate credential watch design. |
| **M-1** | Workers can bypass proxy to GitHub | Medium | W1 | ✅ Done 2026-05-23 | Three split NetworkPolicies (`buildProxyNetworkPolicy`, `buildWorkloadNetworkPolicy`, `buildAGCNetworkPolicy`) |
| **M-2** | Proxy has no destination allowlist | Informational | — | ⓘ By design | Appendix G §G.1 records revisit conditions |
| **M-3** | AES-CBC padding-oracle-shaped errors | Medium | W6 | ✅ Done 2026-05-23 | Single `errInvalidPadding` sentinel + `crypto/subtle` constant-time |
| **M-4** | `rerun-failed-jobs` URL injection | Medium | W6 | ✅ Done 2026-05-23 | `repoSegmentRE` + `neturl.PathEscape` |
| **M-5** | AGC→proxy plaintext HTTP | Medium | W7 | ✅ Done 2026-05-23 | GMC self-signed cert + AGC pinning |
| **M-6** | Broker URLs built via string concat | Medium | W6 | ✅ Done 2026-05-23 | `url.Parse` + `url.Values` in `GetMessage`/`DeleteSession` |
| **M-7** | AGC Deployment lacks SecurityContext | Medium | W8 | ✅ Done 2026-05-23 | `RunAsNonRoot`/`ReadOnlyRoot`/`Caps Drop ALL`/`SeccompRuntimeDefault` on AGC + proxy |
| **M-8** | NetworkPolicy ingress too permissive | Medium | W1 | ✅ Done 2026-05-23 | Proxy ingress restricted to `labelComponent: componentWorkload` |
| **M-9** | IPRangeReconciler drops proxy egress rule | Medium | W1 | ✅ Done 2026-05-23 | `patchNetworkPolicy` only touches `npProxyName` |
| **M-10** | Webhook only validates namespace | Medium | — | ✅ Done 2026-05-25 | CEL for `proxy.{min,max}Replicas`; webhook for `gitHubAppRef.namespace` |
| **M-11a** | Agent RSA-2048 keys | Medium | W9 | ✅ Done 2026-05-23 | EdDSA/RSA signing branches |
| **M-11b** | Real-GitHub Ed25519 compatibility probe | Medium | W9 | ❌ Open | Needs probe flag extensions + manual run with real credentials |
| **M-11c** | Default to RSA-3072 | Medium | W9 | ✅ Done 2026-05-23 | RSA-3072 default; Ed25519 opt-in via `--agent-key-type=ed25519` |
| **M-12** | Proxy 502 leaks dial errors | Medium | — | ✅ Done | Generic `"upstream unavailable"`; dial error logged server-side |
| **M-13** | Verbose broker error bodies | Medium | — | ✅ Done | `capBody(b, 200)` throughout broker client |
| **M-14** | GMC forwards `AGC_EXTRA_*` cluster-wide | Medium | W5 | ✅ Done 2026-05-23 | `--allow-agc-extra-env`, default `false` (Option A, D-1 resolved) |
| **M-15** | `MaxWorkers` TOCTOU race | Medium | — | ⓘ Accepted | Soft ceiling per D-6; ResourceQuota is the hard cap |
| **M-16** | `safeName` collision risk | Medium | — | ✅ Done 2026-05-25 | Hash suffix in both `safeName` and `labelSafe` |
| **L-1** | JWT `iat` without `jti` | Informational | — | ✅ Done | `ID: newUUID()` sets `jti` |
| **L-2** | `http.DefaultClient` has no timeout | Low | — | ✅ Done | 60s timeout client injected into broker, registrar, IP-range fetcher |
| **L-3** | `math/rand` for jitter | Informational | — | ✅ Done | `//nolint:gosec // jitter, not crypto` |
| **L-4** | `isUnauthorized` substring match | Low | — | ✅ Done 2026-05-24 | Typed `*UnauthorizedError`; substring fallbacks removed |
| **L-5** | PEM parser asymmetry | Informational | — | ✅ Done 2026-05-23 | Unified PKCS#1+PKCS#8 parsing (via W9) |
| **L-6** | `mustEnv` calls `os.Exit` | Informational | — | ✅ Done | All three return errors |
| **L-7** | Stub URLs default to `stub.example.com` | Informational | — | ✅ Done | Both stub URLs required together when `GITHUB_ORG_URL` unset |

### Open work

- **M-11b** — Live Ed25519 probe. Does not affect the RSA-3072 default;
  gates only operator documentation for the `--agent-key-type=ed25519`
  opt-in.
- **Phase 1 live validation** — `kind` end-to-end checks (worker pod
  can't reach GitHub directly; AGC has no `GITHUB_APP_*` env;
  SA-scoped Secret access enumeration). Tracked under Milestone 4
  manual verification.

### Out of scope, flagged separately

- Image-digest pinning for `AGC_IMAGE` / `PROXY_IMAGE`.
- Explicit `imagePullPolicy` on worker pods.

---

## Critical

### C-1. RunnerGroup `PodTemplate` is not validated; tenant can ship privileged worker pods

- **Status (2026-05-23): Done.** Closed by W2. `SecurityProfile` enum on
  `ActionsGatewaySpec` (default `baseline`); `applyNamespacePSA` in
  [actionsgateway_controller.go:509](../../cmd/gmc/internal/controller/actionsgateway_controller.go)
  stamps `pod-security.kubernetes.io/{enforce,warn,audit}` labels on the
  tenant namespace at every reconcile. The in-tree PodSecurity admission
  plugin rejects any worker pod that violates the profile, including pods
  built by the AGC from a tenant's PodTemplate. A CEL `XValidation` on
  `RunnerGroupSpec.PodTemplate` also rejects `privileged: true` at
  `kubectl apply` time for the kinder failure path (per D-7).
- **Location:**
  [cmd/agc/api/v1alpha1/runnergroup_types.go:30-60](../../cmd/agc/api/v1alpha1/runnergroup_types.go),
  [cmd/agc/internal/provisioner/provisioner.go:349-439](../../cmd/agc/internal/provisioner/provisioner.go),
  [cmd/gmc/internal/controller/actionsgateway_controller.go](../../cmd/gmc/internal/controller/actionsgateway_controller.go)
- **Category:** OWASP A04:2025 — Insecure Design / OWASP A01:2025 — Broken Access Control
- **Why Critical:** Container breakout via privileged PodSpec fields is a
  direct path from "tenant author" to "root on the node". On any cluster that
  co-locates tenant workers on shared nodes (the common case for GPU
  capacity), `privileged: true` is cross-tenant impact. Today the code has
  no admission-time guard; the AGC's per-PodSpec overrides cover only
  `hostPID`/`hostNetwork`/`hostIPC`/`automountServiceAccountToken` and leave
  everything else (privileged, capabilities, hostPath, etc.) to whichever
  cluster-wide policy engine the operator happens to install. That dependency
  is invisible to the GMC and silent to the tenant.
- **Description:** The CRD field `RunnerGroupSpec.PodTemplate` is a verbatim
  `corev1.PodTemplateSpec`. No CEL `XValidation` rules constrain it.
  `Provisioner.buildPod` overrides `HostPID`/`HostNetwork`/`HostIPC` and
  forces `AutomountServiceAccountToken: false`, but it does *not* touch:
  - `securityContext.privileged`, `runAsUser=0`,
    `allowPrivilegeEscalation`, `capabilities.add`
  - `hostPath` volumes
  - `PodSecurityContext.Sysctls`
  - `containers[*].securityContext` for non-`runner` containers (e.g. tenant
    sidecars)
- **Mitigation (secure by default, explicit opt-in for privileged):**
  Adopt a tiered security profile model driven by the in-tree
  [PodSecurity admission plugin](https://kubernetes.io/docs/concepts/security/pod-security-admission/)
  (GA since Kubernetes 1.25). PSA is built into the kube-apiserver — no
  Kyverno, OPA, or other operator-installed component required.

  1. **Add `securityProfile` to `ActionsGatewaySpec`** with enum
     `baseline | restricted | privileged`, default `baseline`. See the
     CRD spec in
     [docs/design/03-api-contracts.md §3.1](../design/03-api-contracts.md#31-kubernetes-crd-schemas).
  2. **GMC stamps the matching PSA labels on the tenant namespace** at
     `ActionsGateway` reconcile time:
     ```yaml
     pod-security.kubernetes.io/enforce:         <securityProfile>
     pod-security.kubernetes.io/enforce-version: latest
     pod-security.kubernetes.io/warn:            <securityProfile>
     pod-security.kubernetes.io/audit:           <securityProfile>
     ```
     The API server then rejects any worker pod that violates the
     profile — including pods the AGC creates from a tenant's
     `PodTemplate`.
  3. **AGC PodSpec invariants remain unconditional.** Regardless of the
     selected profile, `provisioner.buildPod` continues to force
     `HostPID=false`, `HostNetwork=false`, `HostIPC=false`,
     `AutomountServiceAccountToken=false`, and the worker
     `ServiceAccountName`. PSA is the safety net at the namespace
     boundary; the AGC's overrides are the floor at pod construction.
     This ensures a `privileged`-profile tenant still cannot project
     Kubernetes API credentials into a worker pod or share host
     namespaces.
  4. **Tenants who need both privileged and non-privileged workloads**
     deploy two `ActionsGateway` CRs in two namespaces (e.g.
     `myteam-builds` with `privileged` for DinD, `myteam-tests` with
     `baseline` for everything else). Per-`ActionsGateway` is the
     granularity at which the profile is chosen; per-`RunnerGroup`
     within one namespace is not supported in v1 (PSA is
     namespace-scoped).
     [Appendix G](../design/appendix-g-future-enhancements.md) records
     finer granularity as a candidate future enhancement.
  5. **`privileged` tenants are strongly encouraged to pair the profile
     with a sandbox runtime** (`runtimeClassName: kata-containers` or
     `gvisor` in the `PodTemplate`). Privileged-inside-Kata grants
     control over a microVM kernel, not the host kernel — escape from
     the container ends inside the VM. See
     [Appendix B](../design/appendix-b-worker-isolation.md). This
     pairing cannot be enforced by the GMC (the field lives in the
     tenant-owned `PodTemplate`); it is documented as the recommended
     pattern.

  This mitigation closes C-1 without an external dependency. Kyverno or
  OPA Gatekeeper remain useful for *custom* rules (image registry
  allowlists, project-specific schemas) but are not required for the
  baseline "no container escape" guarantee.

  Optional belt-and-suspenders that can land in the same change:

  - CEL `XValidation` on `RunnerGroupSpec.PodTemplate` forbidding
    explicit `privileged: true` regardless of the namespace profile —
    so a misconfigured CR fails at `kubectl apply` time with a clear
    message, rather than at pod-creation time with a PSA rejection.
  - Extend `provisioner.buildPod` to zero out
    `Spec.SecurityContext.Sysctls` and reject any
    `Spec.Volumes[i].HostPath != nil` outside the `privileged`
    profile, as a defense-in-depth check at pod construction.

---

## High

### H-1. GitHub App private key injected into AGC pod as an environment variable

- **Status (2026-05-23): Done.** Closed by W3. `buildAGCDeployment` mounts
  the `gitHubAppRef` Secret as a volume at `/etc/actions-gateway/github-app/`
  with mode `0o400`
  ([builder.go:33,450,481](../../cmd/gmc/internal/controller/builder.go)).
  `cmd/agc/main.go:91-111` reads `appId`, `installationId`, and `privateKey`
  from files. The three `GITHUB_APP_*` env-var entries are gone; `kubectl
  describe pod` no longer shows the key bytes.
- **Location:**
  [cmd/gmc/internal/controller/builder.go:291-321](../../cmd/gmc/internal/controller/builder.go),
  [cmd/agc/main.go:42-55](../../cmd/agc/main.go)
- **Category:** OWASP A02:2025 — Cryptographic Failures / A05:2025 — Misconfiguration
- **Why High:** The control documented in the design *does not exist* in the
  code, and the actual control (env-var Secret reference) is weaker. Anyone
  with `get pod` in the tenant namespace — or any sidecar, debug exec, or
  crash dump that captures `os.Environ()` — sees the private key. Not
  cross-tenant, but a much wider blast radius within the tenant than the
  design claims.
- **Description:** `buildAGCDeployment` exposes the GitHub App private key
  via `env: GITHUB_APP_PRIVATE_KEY` sourced from a `secretKeyRef`. The
  documented mitigation in
  [docs/design/05-security.md](../design/05-security.md) §5.2 ("AGC Token
  Compromise") is *"The AGC never saves plaintext keys to disk. GitHub App
  private keys are mounted as read-only volumes with restrictive file
  permissions (0400)."* That is *not* what the code does. Env vars are
  visible to:
  - `kubectl describe pod` (any user with `get pod` on the namespace).
  - The container process environment via `/proc/<pid>/environ` (any
    sidecar in the pod, any in-pod debug probe, any `kubectl exec`).
  - Crash dumps and logs that echo `os.Environ()`.
  - Child processes spawned by the AGC.

  The `gitHubAppRef` Secret schema is already correct (keys `appId`,
  `installationId`, `privateKey`) and test fixtures already create real
  Secrets via [`CreateGitHubAppSecret`](../../cmd/gmc/test/utils/resources.go).
  Only the AGC's *consumption* of the Secret needs to change.
- **Mitigation:** Project the `gitHubAppRef` Secret as a file volume on
  the AGC pod at `/etc/actions-gateway/github-app/` with `defaultMode: 0o400`.
  Update `cmd/agc/main.go` to read `appId`, `installationId`, and
  `privateKey` from files. `loadPEM` already supports the file-path
  form. Delete the three `GITHUB_APP_*` env-var entries from
  `buildAGCDeployment`.

  Scope is limited to credential material. Non-secret configuration
  (proxy address, runner version, broker URL overrides for tests) stays
  in env vars — those are not secrets and don't belong in a Secret
  volume just for symmetry. The cluster-wide env-var injection
  mechanism that *does* affect non-secret config is a separate issue
  tracked as M-14.

  Invariant after this change: **private key material never appears in
  any process environment.** `kubectl describe pod` shows the volume
  mount, not the key bytes.

### H-2. AGC namespaced Role grants broad Secret read/list/watch + create/delete

- **Location:** [cmd/gmc/internal/controller/builder.go:81-92](../../cmd/gmc/internal/controller/builder.go)
- **Category:** OWASP A01:2025 — Broken Access Control
- **Why High:** A compromised AGC has more access than the design implies.
  Not cross-tenant (it's still namespace-scoped), but the AGC can enumerate
  every Secret the tenant owns — including user-managed ones unrelated to
  GitHub Actions.
- **Description:** The Role bound to the AGC ServiceAccount grants
  `secrets: [get, list, watch, create, delete]` on the entire tenant
  namespace. The AGC only needs:
  - `get` on its agent-pool Secrets (selector `actions-gateway/runner-group=*`)
  - `create`/`delete` on those same Secrets
  - `get` on per-job payload Secrets that it creates (label
    `app.kubernetes.io/managed-by=actions-gateway-agc`)

  The broad `list/watch` lets a compromised AGC enumerate any user-managed
  Secret in the tenant namespace (e.g. a developer's `ghcr-pull-token` or
  `slack-webhook`). The broad `create` would let it stage a Secret that another
  workload mounts.
- **Mitigation (implemented, partially):** Three controls landed:

  1. **`list`/`watch` were initially removed** (commit where W4 first
     shipped), but this broke `agentpool.Pool.listSecrets` — the agent
     pool must enumerate its own per-runner Secrets to reconcile state
     (`EnsureAgents`). `list` was restored (dc80293).

  2. **Secret cache disabled on the AGC manager** (`Client.Cache.DisableFor
     [*corev1.Secret]`, commit 8ea6f5f) — all Secret reads (`Get` and
     `List`) bypass the controller-runtime in-process cache and go direct
     to the API server. A compromised AGC cannot silently drain Secret
     bodies from memory; any exfiltration requires live API server calls
     that appear in the audit log.

  3. **No Secret informer registered** —
     `RunnerGroupReconciler.SetupWithManager` registers only
     `For(&v1alpha1.RunnerGroup{})`. There is no `Watches` or
     `WatchesMetadata` call for Secrets. The `watch` verb in the AGC Role
     is therefore **over-declared**: the RBAC grants it, but the AGC's
     controller machinery never establishes a Secret informer (full or
     metadata-only), so no Secret data or metadata buffers in-process. A
     compromised AGC binary *could* exercise `watch` out-of-band, but the
     legitimate controller never does.

- **Residual risk (accepted):** The AGC Role grants `list` on all Secrets
  in the tenant namespace. A compromised AGC can enumerate user-managed
  Secrets (e.g. `ghcr-pull-token`, `slack-webhook`). The `watch` verb is
  over-declared and unused by the controller. Two paths to closing `list`
  were evaluated and rejected for v1:

  - **`resourceNames` restriction** — not viable: RBAC doesn't support
    glob patterns and agent Secret names are dynamic (created per
    acquisition). Static enumeration would require the GMC to update the
    Role on every agent-pool resize.

  - **Label-selector-restricted RBAC (KEP-4601)** — KEP-4601 ("Authorize
    with Field and Label Selectors") reached GA in Kubernetes 1.34 and
    is enabled by default in 1.35 (the cluster version used here), but it
    extends the *authorizer* layer, not the `rbacv1.PolicyRule` API.
    `PolicyRule` has no `LabelSelector` field even in k8s.io/api v0.35.
    The feature benefits webhook authorizers (OPA, Kyverno) but does not
    let standard RBAC roles scope `list` to label-filtered resources.

  - **Split ServiceAccount** — a second SA with `list`/`watch`/`get` for
    the agent pool; the main AGC SA keeps `create`/`delete`. Viable but
    ~100 LoC, and a compromised AGC already controls the credentials it
    minted (the accepted D-3 reasoning extends here).

  **Decision:** accept `list` in combination with the cache-disable
  control and the no-informer property. Revisit if a real exploit path
  surfaces or if Kubernetes adds label-selector support to `PolicyRule`.
  The over-declared `watch` verb can be removed from the AGC Role once
  confirmed no informer path needs it — low priority, zero functional
  impact.

---

### GMC-1. GMC credential Secret watch design

- **Status: Done 2026-05-26.** Two-layer isolation in place.
- **Context:** The GMC reconciler must detect credential Secret
  creation/deletion to set `CredentialUnavailable` promptly. The naive
  implementation — `Watches(&corev1.Secret{}, ...)` — establishes a full
  Secret informer, buffering all Secret `.data` (GitHub App private keys)
  in the controller's in-process cache indefinitely. This was briefly
  shipped and then reverted when the security intent was confirmed.
- **Implementation:**
  1. **`WatchesMetadata(&corev1.Secret{}, ...)`** in
     [actionsgateway_controller.go](../../cmd/gmc/internal/controller/actionsgateway_controller.go)
     — registers a metadata-only informer. The cache holds only
     `ObjectMeta` (name, namespace, resourceVersion, deletionTimestamp);
     `.data` is never buffered. The controller gets event-driven
     reconcilation on Secret create/delete. The GMC ClusterRole retains
     `list`/`watch` on Secrets (required for the metadata informer).
  2. **`client.Cache.DisableFor[*corev1.Secret]`** in
     [cmd/gmc/cmd/main.go](../../cmd/gmc/cmd/main.go)
     — ensures all `r.Get()` calls on Secrets bypass the cache and hit the
     API server directly. Secret key material is never resident in the
     in-process cache; it exists only on the call stack during a reconcile
     and is GC'd immediately after.
  3. `setCredentialUnavailable` also returns `RequeueAfter: 30s` as a
     fallback for cases where a watch event is missed (e.g. controller
     restart during Secret deletion window).
- **Residual risk:** A compromised GMC can `list`/`watch` Secret
  *metadata* cluster-wide (names, namespaces), which leaks tenant
  `gitHubAppRef` names even without a matching `ActionsGateway` CR. This
  is the same information already exposed by the CR spec itself, so it
  adds no new attack surface. Accepted.

---

## Medium

### M-1. NetworkPolicy permits worker pods to egress directly to GitHub, bypassing the proxy

- **Status (2026-05-23): Done.** Closed by W1. The single empty-selector
  NetworkPolicy was replaced with three `podSelector`-scoped policies:
  `buildProxyNetworkPolicy` (proxy pods → DNS + GitHub CIDRs:443),
  `buildWorkloadNetworkPolicy` (AGC + worker pods → DNS + proxy ClusterIP:proxyPort only),
  and `buildAGCNetworkPolicy` (AGC additive: also reaches k8s API on 443).
  Workers are now selected only by the workload policy and cannot egress
  directly to GitHub
  ([builder.go:112-223](../../cmd/gmc/internal/controller/builder.go)).
- **Location:** [cmd/gmc/internal/controller/builder.go:103-151](../../cmd/gmc/internal/controller/builder.go)
- **Category:** OWASP A05:2025 — Security Misconfiguration
- **Why Medium:** Listed here as a security finding because the gap
  weakens GitHub-side incident containment (a flag against a node IP
  affects every tenant whose workers share that node), but the underlying
  issue is an implementation deviation from a documented design choice,
  not a privilege boundary failure. The design choice (route worker
  traffic through the proxy) and its rationale, tradeoffs, and
  acceptance criteria are tracked in
  [docs/plan/worker-egress-proxy.md](worker-egress-proxy.md).
  Bypassing the proxy does not grant a compromised worker any new
  capability — same OAuth token, same set of reachable endpoints
  (GitHub CIDRs), no cross-tenant impersonation.
- **Description:** `buildNetworkPolicy` creates a single `NetworkPolicy` with
  an empty `PodSelector` (the default value), which selects every pod in the
  namespace. The egress rules are the *union* of `proxyEgress` (DNS + GitHub
  CIDRs:443) and `agcWorkerEgress` (proxy ClusterIP:8080). Because both rule
  sets apply to every pod, worker pods are allowed to talk directly to GitHub
  on :443 — bypassing the egress proxy entirely. The
  [network-architecture.md](../design/network-architecture.md) design
  document already shows the intended two-policy structure; the
  implementation never converged on it.
- **Mitigation:** Emit two NetworkPolicy objects (or one with multiple
  `podSelector`-scoped rules) as described in
  [docs/plan/worker-egress-proxy.md "Implementation status"](worker-egress-proxy.md#implementation-status):
  - `np-proxy`: `podSelector: { app: actions-gateway-proxy }`,
    egress = DNS + GitHub CIDRs on 443.
  - `np-agc-worker`: `podSelector` matching the AGC and worker labels,
    egress = DNS + proxy ClusterIP on 8080 only.

  Add an `ingress` rule on `np-agc-worker` denying inbound except from the
  proxy/AGC selectors. Fixes for M-8 and M-9 land naturally in the same
  change.

### M-2. Proxy has no destination allowlist — designed omission, tracked as future enhancement

- **Location:** [cmd/proxy/proxy.go:91-138](../../cmd/proxy/proxy.go)
- **Category:** OWASP A10:2025 — Server-Side Request Forgery (SSRF) — *informational*
- **Status:** Not a finding; **by design**. Logged here so security
  reviewers reading the codebase don't re-discover it as a gap.
- **Why:** The proxy is intentionally a transport-only CONNECT tunneler.
  Egress policy lives in the proxy pod's `NetworkPolicy`, which restricts
  outbound TCP to GitHub CIDRs + DNS. Putting policy in the proxy code
  too would create two overlapping policy surfaces that must agree, and
  every conditional in the data path is a future bug. The full rationale,
  the conditions under which this choice would be revisited, and a sketch
  of the implementation are in
  [Appendix G §G.1](../design/appendix-g-future-enhancements.md#g1-proxy-enforced-destination-allowlist).
- **When this becomes a real finding:** if `proxy.managedNetworkPolicy: false`
  is set and the operator does not provide an equivalent egress policy by
  another means, the network-layer guard disappears and the proxy becomes
  effectively open. The mitigation in that case is operator policy, not
  proxy code — but Appendix G §G.1 is the path forward if the project
  decides to provide an in-proxy fallback.

### M-3. AES-CBC unpadding has padding-oracle-shaped error returns

- **Status (2026-05-23): Done.** Closed by W6. `pkcs7Unpad` in
  [broker/crypto.go:84](../../broker/crypto.go) now returns a single
  `errInvalidPadding` sentinel for every malformed input (empty, bad
  length, wrong byte). The byte comparison runs in constant time via
  `crypto/subtle`. The oracle shape is eliminated regardless of how the
  error is surfaced.
- **Location:** [broker/crypto.go:75-89](../../broker/crypto.go)
- **Category:** OWASP A02:2025 — Cryptographic Failures
- **Why Medium:** The vulnerability *class* is critical, but the only
  "attacker" in this code path is GitHub's broker (an authenticated trust
  boundary), and the decryption result is consumed locally — the unpadding
  error never reaches an untrusted observer. Recorded here because the code
  itself is oracle-shaped and would become exploitable if reused or if these
  errors are ever reflected in a response/log surface that an attacker can
  probe.
- **Description:** `pkcs7Unpad` returns three distinguishable errors:
  `"empty plaintext"`, `"invalid padding length %d"`, `"invalid padding byte
  at position %d"`. Combined with byte-by-byte comparison that short-circuits,
  this is the textbook Vaudenay padding oracle pattern. AES-CBC + PKCS#7 +
  no MAC = padding oracle if an attacker can submit chosen ciphertexts and
  observe oracle output.
- **Mitigation:**
  1. Replace the error returns with a single sentinel error
     `errInvalidPadding` returned for *all* unpadding failures (empty, bad
     length, wrong byte).
  2. Make the comparison constant-time: collect a single `ok` bit across
     all bytes (e.g. `mask |= data[i] ^ byte(padLen)`) and return after the
     loop.
  3. Add a Note that this decrypt routine MUST NOT be exposed as a
     network-callable handler.
  4. Long-term: ask GitHub's broker to switch to AES-GCM or otherwise
     AEAD-protect the message body; the AES-CBC choice is dictated by
     compatibility with the .NET runner, not by us.

### M-4. `rerun-failed-jobs` URL built from job payload values without validation

- **Status (2026-05-23): Done.** Closed by W6. `rerunFailedJobs` at
  [provisioner.go:250-276](../../cmd/agc/internal/provisioner/provisioner.go)
  rejects any `owner`/`repo` not matching `repoSegmentRE` (line 526:
  `^[A-Za-z0-9][A-Za-z0-9._-]*$`) before building the URL, and wraps each
  segment in `neturl.PathEscape`. A unit test feeds adversarial
  `system.github.repository` values and asserts the call is rejected
  pre-HTTP.
- **Location:** [cmd/agc/internal/provisioner/provisioner.go:248-293](../../cmd/agc/internal/provisioner/provisioner.go)
- **Category:** OWASP A03:2025 — Injection (URL/path injection)
- **Why Medium:** The `owner`/`repo` inputs come from the AcquireJob response
  over a TLS-authenticated channel to GitHub. An attacker would need to MitM
  that channel or compromise the broker. The class is injection, but the
  input is server-derived. Worth fixing because the cost is low and the
  contract from GitHub is not formally guaranteed.
- **Description:** `rerunFailedJobs` builds
  `%s/repos/%s/%s/actions/runs/%s/rerun-failed-jobs` from `owner`, `repo`, and
  `runID`. `owner` and `repo` are extracted from
  `acquirePayload.Variables["system.github.repository"]` via
  `strings.SplitN("/", 2)`. `runID` is parsed as `%d` (safe).
  There is no validation that `owner`/`repo` contain only `[A-Za-z0-9_.-]`.
- **Mitigation:**
  1. Reject any `owner`/`repo` that contains a character outside
     `[A-Za-z0-9._-]` before building the URL.
  2. Use `url.PathEscape` on each segment.
  3. Add a unit test that feeds adversarial `system.github.repository`
     values and asserts the call is rejected before HTTP.

### M-5. AGC→proxy hop uses plaintext HTTP — **Fixed (W7, 2026-05-23)**

- **Location:** [cmd/gmc/internal/controller/builder.go:243-245](../../cmd/gmc/internal/controller/builder.go),
  [cmd/gmc/internal/controller/builder.go:298-300](../../cmd/gmc/internal/controller/builder.go)
- **Category:** OWASP A02:2025 — Cryptographic Failures (Transport)
- **Why Medium:** The CONNECT-tunneled payload is itself TLS to GitHub, so
  the actual job traffic remains end-to-end encrypted. Only the CONNECT
  *request line* (target hostname:port) is observable on-wire. No bearer
  token is currently sent on the outer hop because the proxy is unauthenticated.
  Risk grows if proxy auth is ever added without TLS.
- **Description:** `buildProxyServiceAddr` returns
  `http://actions-gateway-proxy.<ns>.svc.cluster.local:8080`. On a cluster
  with eBPF or shared CNI visibility (sidecar mesh, host-based intrusion
  detection), the CONNECT host:port is observable.
- **Mitigation (defense in depth):**
  1. Terminate TLS at the proxy with a per-tenant cert from cert-manager
     and use `https://` in the proxy address. The proxy already mounts no
     filesystem; adding a Secret volume with cert/key is straightforward.
  2. Alternatively, deploy the cluster with a mTLS mesh (Cilium, Istio)
     and rely on transparent encryption for in-namespace traffic.

### M-6. Broker URLs built with raw string concatenation

- **Status (2026-05-23): Done.** Closed by W6. Both `GetMessage`
  ([broker/client.go:293](../../broker/client.go)) and `DeleteSession`
  (line 313) build URLs through `net/url` — `url.Parse` for the base,
  `url.Values` for query parameters, `RawQuery = q.Encode()` to attach
  them. A future operator-controlled value for `RunnerVersion`/etc.
  cannot smuggle a query parameter.
- **Location:** [broker/client.go:262-313](../../broker/client.go) (`GetMessage`),
  [broker/client.go:417-444](../../broker/client.go) (`DeleteSession`)
- **Category:** OWASP A03:2025 — Injection (URL injection)
- **Why Medium:** Both `sessionID` (server-derived) and the v2 query
  parameters (`RunnerVersion`/`RunnerOS`/`RunnerArch`, operator-set env vars)
  come from non-adversarial sources today. The pattern is fragile though: a
  future refactor that lets a tenant influence one of these values would turn
  it into a real injection.
- **Description:** Values are concatenated into URL strings without
  `url.QueryEscape` / `url.PathEscape`. An operator setting
  `GITHUB_RUNNER_OS="linux&secret=stolen"` would smuggle a query param.
- **Mitigation:** Build URLs with `net/url`:
  ```go
  u, _ := url.Parse(strings.TrimRight(c.BrokerURL, "/") + "/message")
  q := u.Query()
  q.Set("sessionId", sessionID)
  q.Set("status", "online")
  q.Set("runnerVersion", c.RunnerVersion)
  u.RawQuery = q.Encode()
  ```
  Apply the same change to `DeleteSession` (session ID interpolated into
  path) and any future broker URL builder.

### M-7. AGC Deployment lacks SecurityContext

- **Status (2026-05-23): Done.** Closed by W8. `buildAGCDeployment` now
  sets `RunAsNonRoot: true`, `ReadOnlyRootFilesystem: true`,
  `AllowPrivilegeEscalation: false`, `Capabilities.Drop: ALL`, and
  `SeccompProfile: RuntimeDefault` on the AGC container
  ([builder.go:492-497](../../cmd/gmc/internal/controller/builder.go)).
  The same hardening was applied to the proxy container (line 323-327)
  alongside `Capabilities`/`SeccompProfile` that were missing there.
- **Location:** [cmd/gmc/internal/controller/builder.go:291-321](../../cmd/gmc/internal/controller/builder.go)
- **Category:** OWASP A05:2025 — Security Misconfiguration
- **Description:** `buildAGCDeployment` sets no `securityContext` on the AGC
  pod or container. The proxy Deployment (lines 220-225) correctly sets
  `RunAsNonRoot: true`, `ReadOnlyRootFilesystem: true`,
  `AllowPrivilegeEscalation: false`. The AGC, which holds the GitHub App
  private key in memory, should have at minimum the same.
- **Mitigation:** Add to the AGC container:
  ```go
  SecurityContext: &corev1.SecurityContext{
      RunAsNonRoot:             ptr(true),
      ReadOnlyRootFilesystem:   ptr(true),
      AllowPrivilegeEscalation: ptr(false),
      Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
      SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
  }
  ```
  Apply the same hardening to the proxy Deployment for the `Capabilities`
  and `SeccompProfile` fields, which are also missing there.

### M-8. NetworkPolicy ingress to proxy and AGC is overly permissive

- **Status (2026-05-23): Done.** Closed by W1. The proxy NetworkPolicy now
  selects only proxy pods, and its single ingress rule restricts inbound to
  pods carrying `labelComponent: componentWorkload` on `proxyPort`
  ([builder.go:138-146](../../cmd/gmc/internal/controller/builder.go)).
  Dev/debug pods in the namespace can no longer reach the proxy or AGC.
- **Location:** [cmd/gmc/internal/controller/builder.go:137-139](../../cmd/gmc/internal/controller/builder.go)
- **Category:** OWASP A05:2025 — Security Misconfiguration
- **Description:** `proxyIngress` allows traffic from any pod in the
  namespace via an empty `PodSelector`. Combined with M-1, every pod in the
  tenant namespace can hit the proxy and the AGC, including any
  user-created pod (a dev's debugging container).
- **Mitigation:** Narrow ingress to selectors matching the AGC pod and the
  pods carrying `actions-gateway/runner-group=*` (workers). For the AGC's
  own ingress, allow only the proxy/health probe namespaces or
  Prometheus scrape source.

### M-9. `IPRangeReconciler` drops worker→proxy egress rule on every refresh

- **Status (2026-05-23): Done.** Closed by W1. `patchNetworkPolicy` now
  only refreshes the proxy NetworkPolicy (`npProxyName`); the workload
  policy carrying the worker→proxy egress rule is left untouched
  ([ipranges.go:193-204](../../cmd/gmc/internal/controller/ipranges.go)).
  The separation of policies makes the previous code-path conflation
  impossible by construction.
- **Location:** [cmd/gmc/internal/controller/ipranges.go:162-173](../../cmd/gmc/internal/controller/ipranges.go)
- **Category:** OWASP A04:2025 — Insecure Design
- **Description:** `patchNetworkPolicy` calls
  `buildNetworkPolicy(ag, "", cidrs)` — passing `""` for `proxyClusterIP`.
  In `buildNetworkPolicy`, the AGC-and-worker egress rule is only emitted
  when `proxyClusterIP != ""`. The IP-range refresh therefore overwrites
  the NetworkPolicy with a version that drops the proxy egress rule until
  the main `ActionsGatewayReconciler` re-runs. Workers cannot reach the
  proxy in that interval — a self-inflicted outage. While not strictly a
  security finding, the same code path could be inverted (open *more* egress
  by accident) by a future refactor.
- **Mitigation:** `patchNetworkPolicy` should re-look-up the proxy Service
  ClusterIP and pass it through, *or* only update `.Egress[0..n]` slots that
  contain CIDR peers, leaving the proxy egress rule untouched.

### M-10. Untyped admission webhook: only namespace check, no spec validation

- **Location:** [cmd/gmc/internal/webhook/v1alpha1/actionsgateway_webhook.go:50-65](../../cmd/gmc/internal/webhook/v1alpha1/actionsgateway_webhook.go)
- **Category:** OWASP A04:2025 — Insecure Design
- **Description:** The validating webhook checks only the reserved-namespace
  list. It does not reject:
  - `gitHubAppRef.namespace` ≠ the CR's own namespace (the field is
    silently ignored by `secretKeyRef` anyway, but a tenant who reads the
    CRD will think they can cross namespaces — a confused-deputy footgun).
  - PodTemplate-level forbidden fields (see C-1).
  - `proxy.maxReplicas` higher than a configured per-cluster cap.
  - `namespaceQuota` negative or absurdly large.
- **Mitigation:** Either expand the webhook to validate these, or — preferred
  — move them to CRD `XValidation` CEL rules so the failure mode is
  declarative and visible in `kubectl explain`.

### M-11. `agentpool` keeps generated RSA private keys at 2048 bits

- **Location:** [cmd/agc/internal/agentpool/pool.go:149-155](../../cmd/agc/internal/agentpool/pool.go)
- **Category:** OWASP A02:2025 — Cryptographic Failures
- **Description:** `rsa.GenerateKey(rand.Reader, 2048)`. NIST SP 800-57
  considers 2048-bit RSA acceptable through 2030; 3072-bit is the
  forward-looking default. Since these keys can outlive the runner group
  in backup tape and there is no online rotation path, the longer key is
  cheap insurance.
- **Mitigation:** Upgrade to RSA-3072 (the security fix). Ed25519 is a
  **performance optimization**, not a security improvement in this
  protocol: Ed25519 agents cannot decrypt the RSA-OAEP-encrypted session
  key the broker sends, so job message bodies traverse the broker session
  without the AES-256-CBC layer (TLS is the primary protection; the AES
  layer is defense-in-depth). Ed25519 is offered as an explicit operator
  opt-in via `--agent-key-type=ed25519` for deployments that prioritize
  signing performance over defense-in-depth. RSA-3072 is and must remain
  the secure default. See D-5 for decision rationale and
  [Appendix G §G.6](../design/appendix-g-future-enhancements.md#g6-x25519-ecdh-session-key-exchange)
  for the broker-protocol change that would let Ed25519 become the default
  without any security regression.
- **Status (2026-05-23):** RSA-3072 is now the default
  (`--agent-key-type=rsa`). Ed25519 available as opt-in via
  `--agent-key-type=ed25519`. M-11a/c implemented; M-11b (real-GitHub
  compatibility probe) still open.

### M-12. Proxy error response leaks upstream dial errors verbatim

- **Location:** [cmd/proxy/proxy.go:103-106](../../cmd/proxy/proxy.go)
- **Category:** OWASP A09:2025 — Security Logging and Monitoring Failures (info leak)
- **Description:** `http.Error(w, "upstream dial: "+err.Error(), 502)` returns
  the system DNS error or TCP error to the caller. On many resolvers that
  includes the resolved IP — useful to an attacker probing internal
  addresses (combined with M-2).
- **Mitigation:** Return a generic `"upstream unavailable"` message and log
  the detailed error server-side.

### M-13. Verbose error bodies in broker client

- **Location:** [broker/client.go:222-226](../../broker/client.go) (`CreateSession`),
  [broker/client.go:341-343](../../broker/client.go) (`AcquireJob`),
  [broker/client.go:373-376](../../broker/client.go) (`RenewJob`),
  [cmd/agc/internal/agentpool/github_registrar.go:108-110](../../cmd/agc/internal/agentpool/github_registrar.go)
- **Category:** OWASP A09:2025 — Logging Failures (verbose error)
- **Description:** Errors include the full response body. GitHub may return
  request IDs and partial token echoes in error bodies — these end up in
  AGC logs that are aggregated cluster-wide and often retained.
- **Mitigation:** Cap body output (e.g. `body[:200]`) and add a
  `log.Debug` path with the full body gated by an env-var debug switch.

### M-14. GMC forwards `AGC_EXTRA_*` env vars to every tenant AGC

- **Status (2026-05-23): Done (Option A).** Closed by W5. The forwarding
  loop is now gated by `--allow-agc-extra-env`, default `false`
  ([cmd/gmc/cmd/main.go:75-77,199-217](../../cmd/gmc/cmd/main.go)).
  Production GMC deployments omit the flag, so a misconfigured
  `AGC_EXTRA_*` env var on the GMC pod is silently dropped. Test rigs
  patch the flag on (`e2e_suite_test.go:155-177`). Option B (typed
  per-tenant CR field) deferred to a future change if a real production
  use case for per-tenant endpoint overrides surfaces; D-1 resolved
  in favor of Option A for v1.
- **Location:** [cmd/gmc/cmd/main.go:191-206](../../cmd/gmc/cmd/main.go)
- **Category:** OWASP A05:2025 — Security Misconfiguration
- **Description:** Any env var prefixed `AGC_EXTRA_` on the GMC pod is
  injected verbatim into *every* AGC Deployment. The mechanism is
  currently used by e2e tests
  ([e2e_suite_test.go:98-109](../../cmd/gmc/test/e2e/e2e_suite_test.go))
  to override `GITHUB_API_BASE_URL` / `GITHUB_BROKER_URL` / `STUB_AUTH_URL`
  / `STUB_BROKER_URL` so AGC pods point at the fakegithub server. None of
  these are credentials; the issue is that the forwarding is **cluster-wide
  and untyped** — any value a cluster-admin puts on the GMC pod becomes
  configuration for every tenant's AGC. A misconfigured
  `AGC_EXTRA_GITHUB_API_BASE_URL=https://attacker.tld` redirects every
  tenant's GitHub API traffic without touching any tenant's CR.
- **Mitigation:** Two reasonable options; pick one based on whether
  per-tenant test overrides will ever be useful.

  **Option A — gate behind a flag (lowest churn).** Add
  `--allow-agc-extra-env` to the GMC, defaulting `false`. The forwarding
  loop only runs when the flag is set. Production deployments omit the
  flag; test rigs pass it. The mechanism stays cluster-wide but is no
  longer silently active in production.

  **Option B — replace with a per-tenant typed field.** Add an optional
  `proxy.endpointOverrides` (or similarly named) block to
  `ActionsGatewaySpec` with strongly-typed fields:
  `apiBaseURL`, `brokerURL`, `stubAuthURL`, `stubBrokerURL`. The GMC
  reads these from each CR and stamps them onto that tenant's AGC
  Deployment as ordinary env vars. CRD CEL validation rejects values
  that aren't `https://` URLs (or `http://` against
  `*.svc.cluster.local` for the test case). The GMC pod's environment
  has no special privileged role anymore — every override is visible in
  the CR. `AGC_EXTRA_*` is deleted.

  Option B is the cleaner outcome (typed, per-tenant, no cluster-wide
  side channel) but adds a CRD field whose only legitimate production
  use case is "I want to point one tenant at a private GitHub
  Enterprise endpoint" — which is plausible but not a current
  requirement. Option A is one flag + one `if` and removes the
  silent-default-on misconfiguration risk.

  These values are non-credentials and stay in env vars in either
  option; this finding is about the *forwarding mechanism*, not about
  whether env vars are an acceptable transport for non-secret config.
  Credential material is handled separately by H-1.

### M-15. `provisioner.activePodCount` reads from controller-runtime cache; race with quick pod churn

- **Location:** [cmd/agc/internal/provisioner/provisioner.go:296-312](../../cmd/agc/internal/provisioner/provisioner.go)
- **Category:** OWASP A04:2025 — Insecure Design (TOCTOU)
- **Description:** The check `count >= *rg.Spec.MaxWorkers` is followed by
  `Client.Create(ctx, pod)`. Between the two, the cache may be stale by
  multiple seconds; the AGC can exceed `MaxWorkers` under burst arrivals.
  Not a direct security boundary, but `MaxWorkers` is part of the
  "Denial of Service via Resource Exhaustion" mitigation in
  [docs/design/05-security.md](../design/05-security.md) §5.2. A tenant
  that intentionally crafts a flurry of jobs gets more workers than the
  contract allows; downstream the `ResourceQuota` still caps actual
  resources, so the real-world impact is bounded.
- **Mitigation:** Use a per-RunnerGroup in-process counter incremented
  before `Create` and decremented on pod completion / Create failure.
  Alternatively, rely solely on `ResourceQuota` and document that
  `MaxWorkers` is a soft ceiling.

### M-16. `safeName` collapses arbitrary characters; collisions possible

- **Location:**
  [cmd/agc/internal/provisioner/provisioner.go:508-526](../../cmd/agc/internal/provisioner/provisioner.go),
  [cmd/gmc/internal/controller/builder.go:454-466](../../cmd/gmc/internal/controller/builder.go)
- **Category:** OWASP A04:2025 — Insecure Design
- **Description:** Both `safeName` variants replace any non-DNS character
  with `-`. Two distinct inputs (e.g. `gpu/a100` and `gpu_a100`) yield the
  same Kubernetes object name, causing a silent overwrite. For job
  payload Secrets keyed on planID, this is theoretically a cross-job leak:
  if planIDs from two different acquisitions normalize to the same name,
  the Secret content is overwritten. GitHub planIDs are GUIDs in practice,
  but the contract is not declared.
- **Mitigation:** When collapsing, append a hash suffix of the original
  input (`-<8 hex bytes>`) so distinct inputs produce distinct names.

---

## Low

### L-1. JWT signing uses `time.Now()` directly; no monotonic protection

- **Location:** [githubapp/auth.go:97-111](../../githubapp/auth.go)
- **Category:** Informational
- **Description:** `iat` is `time.Now().Add(-60 * time.Second)` to absorb
  clock skew. If the system clock jumps backward (NTP correction, VM resume),
  the same JWT might be re-issued with the same `iat`, but `jti` is not
  set on the App-level JWT (only on the runner OAuth JWT). GitHub accepts
  this; documenting as low-risk.
- **Mitigation:** None required; consider adding `jti` for parity.

### L-2. `http.DefaultClient` has no timeout

- **Location:** [githubapp/auth.go:127](../../githubapp/auth.go), [broker/client.go:138-143](../../broker/client.go),
  [cmd/agc/internal/agentpool/github_registrar.go:57-62](../../cmd/agc/internal/agentpool/github_registrar.go),
  [cmd/gmc/internal/controller/ipranges.go:57-59](../../cmd/gmc/internal/controller/ipranges.go)
- **Category:** OWASP A04:2025 — Insecure Design (no timeout)
- **Description:** `http.DefaultClient` has no timeout. A hostile or
  unhealthy GitHub endpoint can hang an AGC goroutine indefinitely. Per-call
  contexts mitigate this *only when the caller passes a deadline* — the
  long-poll `GetMessage` deliberately does not, and others (token fetch,
  registration, IP-range refresh) inherit the parent context.
- **Mitigation:** In each binary's `main`, construct an `http.Client` with
  `Timeout: 60 * time.Second` (or per-call deadlines for the long-poll) and
  inject it into the broker client, the registrar, and the IP-range fetcher.

### L-3. `BackoffDelay` uses `math/rand` for jitter

- **Location:** [cmd/agc/internal/listener/goroutine.go:445-450](../../cmd/agc/internal/listener/goroutine.go)
- **Category:** Informational
- **Description:** `rand.Int63n` is non-CSPRNG, but jitter does not require
  unpredictability. Recorded for completeness — gosec will flag this.
- **Mitigation:** Add `//nolint:gosec // jitter, not crypto` or switch to
  `math/rand/v2`.

### L-4. `isUnauthorized` / `isSessionExpired` use substring matches

- **Location:** [cmd/agc/internal/listener/goroutine.go:423-441](../../cmd/agc/internal/listener/goroutine.go)
- **Category:** OWASP A04:2025 — Insecure Design
- **Description:** Both functions look for `"401"`, `"403"`, `"404"`, `"410"`
  as substrings of the error message. Fragile — a response body containing
  those digits in an unrelated context can match.
- **Mitigation:** Return typed errors from `broker.Client` (e.g.
  `*UnauthorizedError`, `*SessionExpiredError`) and use `errors.As`.

### L-5. PEM key parsing accepts only PKCS#1 in `agentpool/crypto.go` (asymmetric with `githubapp/auth.go`)

- **Location:** [cmd/agc/internal/agentpool/crypto.go:17-30](../../cmd/agc/internal/agentpool/crypto.go)
  vs [githubapp/auth.go:147-169](../../githubapp/auth.go)
- **Category:** Informational
- **Description:** The agent-pool key parser accepts only `RSA PRIVATE KEY`
  blocks; the App key parser accepts both PKCS#1 and PKCS#8. Since the
  agent-pool keys are *generated* by this code (always PKCS#1), this is
  fine today, but a future serialization change will silently fail to
  parse old Secrets.
- **Mitigation:** Use a single shared parser that handles both forms.

### L-6. `mustEnv` calls `os.Exit` from package main inside helper functions

- **Location:** [cmd/agc/main.go:179-186](../../cmd/agc/main.go),
  [cmd/probe/main.go:46-53](../../cmd/probe/main.go),
  [cmd/gmc/cmd/main.go:254-261](../../cmd/gmc/cmd/main.go)
- **Category:** Informational (operability, not security)
- **Description:** Tests cannot exercise the failure path.
- **Mitigation:** Return errors and let the caller handle exit.

### L-7. Stub registrar default URLs use `stub.example.com`

- **Location:** [cmd/agc/main.go:142-154](../../cmd/agc/main.go)
- **Category:** Informational
- **Description:** If neither `GITHUB_ORG_URL`, `STUB_AUTH_URL`, nor
  `STUB_BROKER_URL` are set the AGC errors out cleanly. But if only one
  of the stub URLs is set the other defaults to `https://stub.example.com/...`
  — a real domain on the public Internet. If the AGC starts up in production
  with a half-configured stub, it will send authentication tokens to a
  third-party endpoint.
- **Mitigation:** When `GITHUB_ORG_URL` is unset, *require* both stub URLs
  to be set explicitly; do not silently default either to a public domain.

---

## Out-of-scope but worth noting

- **No image-digest pinning in `buildAGCDeployment` / `buildProxyDeployment`.**
  The design says digest-pinning SHOULD be used; the GMC accepts whatever
  `AGC_IMAGE` / `PROXY_IMAGE` env var holds. Recommend enforcing
  `image@sha256:…` syntax at GMC startup.
- **No `imagePullPolicy` set on tenant worker pods.** With digest-pinned
  worker images this is moot, but in the floating-tag case Kubernetes
  defaults to `Always` (good) only for `:latest`; other tags use
  `IfNotPresent` (cache-poisoning surface).
- **No `pod-security.kubernetes.io/enforce` namespace label.** Hooking up
  the in-tree PodSecurity admission plugin is the cheapest catch-all for
  C-1.

---

## Implementation Plan

Findings are bundled into workstreams that share files, tests, or
review surface. Workstreams within a phase can land in any order
unless a dependency is noted; phases ship sequentially.

### Decisions required before implementation begins

Items below are not bugs to fix — they are choices that determine the
shape of the fix. The status column distinguishes blockers (work cannot
start without the call) from defaults (work can start with the listed
assumption but should be confirmed).

| ID | Decision | Affects | Status | Default if undecided |
|---|---|---|---|---|
| D-1 | M-14: Option A (flag-gate `AGC_EXTRA_*`) vs Option B (typed `endpointOverrides` CR field) | W5 | **Resolved (2026-05-23)** | Option A shipped — `--allow-agc-extra-env`, default `false`. Option B deferred until a real production use case appears. |
| D-2 | Does `restricted` profile actually work with `ghcr.io/actions/runner`? Verify, or drop `restricted` from v1 | W2 | **Resolved (2026-05-23)** | `restricted` confirmed compatible; keep enum value; require `runAsUser: 1001` in PodTemplate |
| D-3 | W4 residual broad `secrets.create` — accept, or invest in per-job Secret alternative | W4 | Default if undecided | Accept with a code comment; revisit when a real exploit path is found |
| D-4 | W7 cert source: cert-manager required, cert-manager opt-out, or GMC-managed self-signed cert with AGC pinning | W7 | Default if undecided | **GMC self-signed cert + AGC pinning** (no cert-manager dependency, secure by default) |
| D-5 | M-11: RSA-3072 as secure default; Ed25519 as performance opt-in | M-11 | **Decided** | **RSA-3072 default; Ed25519 opt-in via `--agent-key-type=ed25519`. Ed25519 loses AES session-key layer; X25519 ECDH (Appendix G §G.6) is the path to making Ed25519 the secure default.** |
| D-6 | M-15: hard `MaxWorkers` (in-process counter) vs soft (document, rely on ResourceQuota) | M-15 (Phase 3) | Default if undecided | Soft ceiling; document and rely on ResourceQuota |
| D-7 | C-1 belt-and-suspenders: ship CEL `XValidation` + `buildPod` zero-out, or rely on PSA alone | W2 | Default if undecided | Ship the CEL rule (cheap, better failure mode); skip `buildPod` zero-out (PSA already covers it) |
| D-8 | Phase 1 parallelism — single contributor sequential, or parallel work streams | All Phase 1 | Default if undecided | Assume single contributor; sequence W3 → W4 → W1 → W2 → W5 (smallest blast radius first) |

**Detail on each:**

- **D-1 (resolved 2026-05-23):** Option A shipped — `--allow-agc-extra-env`
  flag on the GMC, default `false`. The forwarding loop only runs when
  the flag is set, so production deployments cannot accidentally redirect
  tenant AGC traffic via a single misconfigured env var on the GMC pod.
  Option B (typed `endpointOverrides` CR field) remains available as a
  future change if per-tenant endpoint overrides become a real
  requirement; it would replace the flag-gated mechanism rather than
  extend it.

- **D-2 (resolved 2026-05-23):** Probe run against
  `ghcr.io/actions/actions-runner:latest` (v2.334.0) on a
  kind/k8s-1.35 cluster with `restricted` PSA enforcement. Pod
  admitted; runner binary executed and exited 0. One caveat: the image
  declares USER as the non-numeric name `runner` (UID 1001). Kubernetes
  requires a numeric `runAsUser` to verify non-root under `restricted`,
  so tenants must set `runAsUser: 1001` (and `runAsGroup: 1001`)
  explicitly in their `RunnerGroup.spec.podTemplate`. Without it the
  pod is rejected at admission with "cannot verify user is non-root".
  The enum value is kept; `restricted` is a valid production choice.

- **D-3 (W4):** RBAC's `resourceNames` doesn't support globs. Tightening
  agent-pool Secret access is straightforward (names are deterministic
  and enumerable), but per-job payload Secrets are created per
  acquisition — the AGC needs `create` on `secrets` more broadly than
  `resourceNames` allows. Alternatives: (a) accept the broad `create`,
  (b) replace the per-job Secret with a different resource the AGC
  creates under namespace-restricted RBAC (a `ConfigMap` is wrong — the
  payload is sensitive; a CRD is heavy), (c) introduce a separate
  `job-secret-creator` SA whose tokens are projected only when needed.
  (a) is the path of least resistance and matches the threat model
  (AGC compromise already implies access to credentials it minted).

- **D-4 (W7):** The AGC↔proxy hop needs a server cert the AGC trusts —
  but it does not need a CA hierarchy, ACME, OCSP, CRLs, or any of
  cert-manager's actual complexity. The single consumer is the AGC in
  the same namespace; the cert is never seen by the public internet.
  Three options:

  1. **cert-manager hard prerequisite** — operators must run
     cert-manager; GMC provisions per-tenant `Certificate` objects.
     Smallest LoC (~50) but adds an install dependency.
  2. **cert-manager opt-out** — same as (1) by default, with a
     `--disable-proxy-tls` flag. Still requires cert-manager unless
     operators opt out, leaving plaintext.
  3. **GMC self-signed cert + AGC pinning** — GMC generates a
     self-signed cert at provisioning, stores in a Secret, mounts the
     cert+key to the proxy and the cert (public part) to the AGC. AGC
     uses `tls.Config{RootCAs: poolOfThisOneCert}` — pinning, not
     CA-chain trust. ~150 LoC, no external dependency. Composes with
     cert-manager: an operator can replace the Secret with a
     cert-manager-issued one, and the GMC can detect and stop
     regenerating (optional v1.1).

  Trust model under option 3 is *stronger* than under (1) or (2): a
  pinned cert prevents a compromised cluster CA from issuing a
  usurper. cert-manager's complexity (ACME, multi-issuer, OCSP) is
  irrelevant for an internal hop with one consumer — using it here is
  overkill, not a feature.

  Option 3 is recommended: secure by default, zero install
  dependency, stronger trust model than cert-manager would give us,
  and ~100 LoC cost over Option 1. We are not reimplementing
  cert-manager — we are calling `x509.CreateCertificate` once per
  tenant per rotation cycle.

- **D-5 (M-11):** RSA-3072 is the correct secure default because agent
  keys serve two purposes in the protocol: JWT signing (for OAuth token
  requests) and RSA-OAEP session key decryption (the broker encrypts an
  AES-256-CBC key with the agent's public RSA key). Ed25519 can sign
  JWTs but cannot participate in RSA-OAEP key exchange — Ed25519 agents
  receive job messages without the AES encryption layer.

  Ed25519 is offered as an operator opt-in (`--agent-key-type=ed25519`)
  as a **performance optimization**: smaller keys, faster JWT signing,
  deterministic signatures, no padding oracle surface. Operators who are
  confident in their TLS stack and want to reduce signing overhead can
  opt in, accepting the loss of the AES defense-in-depth layer.

  **Ed25519 as a secure default requires broker-side changes.** If the
  GitHub broker replaced RSA-OAEP session key delivery with
  X25519 ECDH key exchange, Ed25519 agents could participate in session
  encryption and Ed25519 would become the right default — a performance
  win with no security regression. That is tracked as a future
  enhancement in
  [Appendix G §G.6](../design/appendix-g-future-enhancements.md#g6-x25519-ecdh-session-key-exchange).

  Implementation status:
  - **M-11a (done):** Ed25519/EdDSA branches in `githubapp/runner_auth.go`
    and PKCS#8 marshal/parse in `agentpool/crypto.go`.
  - **M-11b (open, deferred):** real-GitHub compatibility probe —
    verifies that the broker accepts Ed25519 SPKI registration and EdDSA
    JWT assertions so that operators using `--agent-key-type=ed25519` get
    working agents. Blocked on probe-extension code (`-key-type` and
    `-register-test-runner` flags do not exist yet) plus a manual run
    with real credentials. Does not gate the default; gates operator
    documentation.
  - **M-11c (done):** `agentpool.createAgent` uses RSA-3072 by default;
    key type is configurable via `--agent-key-type`.

- **D-6 (M-15):** Soft ceiling is simpler and matches the
  `ResourceQuota`-as-real-limit story already documented. Hard
  ceiling closes the TOCTOU window but adds in-process state that
  must be reconciled with reality on AGC restart. Soft is the right
  default unless a tenant report shows the TOCTOU window mattering.

- **D-7 (W2):** PSA alone closes the C-1 threat. The CEL
  `XValidation` adds a kinder error path (rejected at `kubectl
  apply`, not at pod creation) for the common misconfiguration. The
  `buildPod` zero-out adds runtime defense if PSA is somehow
  disabled. Recommended: ship the CEL rule (5 lines of CRD
  annotation, no maintenance cost); skip the `buildPod` zero-out
  unless PSA-bypass becomes a real concern.

- **D-8 (sequencing):** If one contributor does Phase 1, suggested
  order is W3 (file mount, smallest, no integration test churn) →
  W4 (RBAC, only test surface is `rbac_test.go`) → W1 (NetworkPolicy
  split, touches e2e) → W2 (PSA, also touches e2e and overlaps with
  W1 in `reconcileResources`) → W5 (depends on D-1 and may be
  Option-B-shaped). If multiple contributors, W1/W2 sequencing is the
  only constraint.

### Phase 1 — Required for next release — **Done (2026-05-23)**

Closes the only privilege-escalation surface (C-1), the two single-tenant
credential-exposure weakenings (H-1, H-2 — H-2 accepted with compensating
controls), and the documented-contract gap that the worker-egress plan
covers (M-1, M-8, M-9). W4 is the only Phase 1 workstream still carrying
residual risk; see the inline status block below.

#### W1 — NetworkPolicy split (closes M-1, M-8, M-9) — **Done (2026-05-23, commit `4932ce7`)**

See [docs/plan/worker-egress-proxy.md](worker-egress-proxy.md) for the
full rationale.

- **What shipped:**
  - `cmd/gmc/internal/controller/builder.go` — replaced the single
    empty-selector `buildNetworkPolicy` with three policies:
    `buildProxyNetworkPolicy` (proxy → DNS + GitHub CIDRs:443; ingress
    restricted to workload-labelled pods),
    `buildWorkloadNetworkPolicy` (AGC + workers → DNS + proxy:proxyPort),
    `buildAGCNetworkPolicy` (additive: AGC also gets 443 for k8s API).
  - `cmd/gmc/internal/controller/ipranges.go` `patchNetworkPolicy` now
    only mutates `npProxyName`; the workload policy carrying the
    worker→proxy egress rule is left untouched across IP-range refreshes.
  - `cmd/gmc/internal/controller/actionsgateway_controller.go` —
    `reconcileResources` and `reconcileDelete` apply/remove all three
    policies.
- **Tests:** `network_policy_test.go` expectations updated; integration
  test asserts the IPRange refresh preserves the workload policy.
- **Deferred:** end-to-end `curl` validation from a debug pod (the
  acceptance snippets in
  [docs/design/network-architecture.md](../design/network-architecture.md))
  has not been exercised against a freshly provisioned tenant. Tracked
  as a Milestone 4 manual-verification item.

#### W2 — Security profiles via PSA (closes C-1) — **Done (2026-05-23, commit `1155d6f`)**

- **What shipped:**
  - `cmd/gmc/api/v1alpha1/actionsgateway_types.go` — `SecurityProfile`
    enum field (`baseline | restricted | privileged`, default `baseline`)
    with CEL `enum` validation. `zz_generated.deepcopy.go` regenerated.
  - `cmd/gmc/internal/controller/actionsgateway_controller.go` —
    `applyNamespacePSA` (line 509) stamps
    `pod-security.kubernetes.io/{enforce,warn,audit}` and matching
    `*-version` labels on the tenant namespace at every reconcile.
    Idempotent label merge; called from `reconcileResources`.
  - `cmd/agc/api/v1alpha1/runnergroup_types.go` — CEL `XValidation`
    rejects `privileged: true` in `RunnerGroupSpec.PodTemplate` at
    `kubectl apply` time (per D-7).
- **Tests:** integration coverage for namespace label stamping; e2e
  fixtures that need privileged set `securityProfile: privileged` on
  their CRs.
- **Skipped (per D-7):** the `buildPod` zero-out of `Sysctls` /
  `HostPath` — PSA covers it at the namespace boundary and the CEL rule
  catches the common misconfiguration earlier.

#### W3 — Credentials via file mount (closes H-1) — **Done (2026-05-23, commit `1155d6f`)**

- **What shipped:**
  - `cmd/gmc/internal/controller/builder.go` — `buildAGCDeployment`
    mounts the `gitHubAppRef` Secret at `/etc/actions-gateway/github-app/`
    mode `0o400` via `agcCredsVolumeName`/`agcCredsMountPath`
    (lines 33, 450, 481). The three `GITHUB_APP_*` env entries are gone.
  - `cmd/agc/main.go:91-111` — reads `appId`, `installationId`,
    `privateKey` from the mounted directory; `loadPEM` handles the key
    path. Env-var read path deleted.
  - `cmd/probe/main.go` — same file-read pattern, kept in sync.
- **Tests:** `builder_test.go` asserts volume mount, Secret name, and
  `defaultMode: 0o400`. Integration `provisioning_test.go` checks the
  volume instead of env vars.
- **Verified by invariant:** `kubectl exec <agc> -- env | grep GITHUB_APP`
  returns nothing; only the volume contents are visible.

#### W4 — Tighten AGC RBAC (closes H-2)

- **Status (2026-05-24): Partially implemented — residual risk accepted.**

  Two controls implemented:

  - **Secret cache disabled** (`cmd/agc/main.go` — `Client.Cache.DisableFor
    [*corev1.Secret]`): Secret reads bypass the controller-runtime
    in-process cache; no Secret bodies are silently accumulated in the
    AGC process. Exfiltration requires live API calls visible in the
    audit log.

  - **Role comment updated** (`cmd/gmc/internal/controller/builder.go`
    `buildAGCRole`): documents why `list`/`watch` are required and points
    to the cache-disable as the substitute control.

  `list`/`watch` on all Secrets in the tenant namespace remain in the
  Role. `resourceNames` restriction is not viable (dynamic names, no glob
  support). KEP-4601 (GA in k8s 1.34) does not extend `rbacv1.PolicyRule`
  — it helps webhook authorizers, not standard RBAC. Split-SA was
  evaluated and declined (D-3 reasoning: AGC compromise already implies
  access to credentials it minted). The residual enumeration risk is
  accepted for v1 and documented in the H-2 finding above.

#### W5 — `AGC_EXTRA_*` mechanism (closes M-14) — **Done (2026-05-23, commit `1155d6f`)**

D-1 resolved in favor of **Option A** (flag-gated). Option B (typed
per-tenant CR field) deferred until a production use case for endpoint
overrides surfaces.

- **What shipped:**
  - `cmd/gmc/cmd/main.go:75-77` — `--allow-agc-extra-env` flag, default
    `false`. The forwarding loop at lines 199-217 only runs inside
    `if allowAgcExtraEnv`.
  - `cmd/gmc/test/e2e/e2e_suite_test.go:155-177` — e2e suite patches the
    GMC Deployment args to enable the flag before injecting
    `AGC_EXTRA_*` overrides for fakegithub.
- **Production posture:** GMC pods deploy without the flag, so any
  `AGC_EXTRA_*` env var on the GMC pod is silently dropped — no
  cluster-wide redirect of tenant AGC traffic from a single misconfigured
  env var.

### Phase 2 — Hardening — **Done (2026-05-23)**

W6, W7, W8, and W9 all landed on 2026-05-23 (commits `1155d6f`,
`bc15064`, `22f04bb`, `33af439`). Originally framed as the next
milestone after Phase 1; shipped in the same push.

#### W6 — Crypto and injection hygiene (closes M-3, M-4, M-6) — **Done (2026-05-23, commit `1155d6f`)**

- **What shipped:**
  - `broker/crypto.go:19,84` — `errInvalidPadding` is the single sentinel
    returned by `pkcs7Unpad` for every malformed input. Byte comparison
    runs through `crypto/subtle` in constant time.
  - `cmd/agc/internal/provisioner/provisioner.go` — `repoSegmentRE`
    (line 526) restricts `owner`/`repo` to
    `^[A-Za-z0-9][A-Za-z0-9._-]*$`. `rerunFailedJobs` (lines 250-276)
    rejects non-matching values pre-HTTP and wraps each path segment in
    `neturl.PathEscape`.
  - `broker/client.go` — `GetMessage` (line 293) and `DeleteSession`
    (line 313) build URLs through `url.Parse` + `url.Values` with
    `RawQuery = q.Encode()`. No raw string concatenation in the URL path.
- **Tests:** unit tests with adversarial `system.github.repository`
  inputs cover the M-4 reject path; crypto round-trip tests exercise
  the M-3 sentinel; broker tests assert correct URL composition.

#### W7 — Transport hardening: GMC-managed self-signed cert with AGC pinning (closes M-5) — **Done 2026-05-23**

No cert-manager dependency. The GMC generates a self-signed cert per
tenant at provisioning time and rotates it on a schedule. The AGC's
HTTPS client pins this specific cert (not a CA chain). See D-4 for
the rationale.

- **Files:**
  - `cmd/gmc/internal/controller/builder.go` — new helper
    `buildProxyCertSecret(ag)` generates a 2048-bit RSA keypair and a
    self-signed cert via `crypto/x509.CreateCertificate` with the
    proxy ClusterIP/DNS name as SAN, ~1-year lifetime. Store the
    cert+key in a Secret (e.g. `actions-gateway-proxy-tls`). The
    proxy Deployment mounts the Secret at
    `/etc/actions-gateway/proxy-tls/` mode `0400`; the AGC Deployment
    mounts only the cert (public part) at
    `/etc/actions-gateway/proxy-ca/`. `buildProxyServiceAddr` returns
    `https://`.
  - `cmd/gmc/internal/controller/actionsgateway_controller.go` —
    `reconcileResources` ensures the Secret exists and re-issues when
    expiry is within (say) 30 days. Reconciler-driven rotation; no
    external controller needed.
  - `cmd/proxy/proxy.go` and `cmd/proxy/main.go` — accept
    `PROXY_TLS_CERT_PATH` and `PROXY_TLS_KEY_PATH`. When set, the
    proxy listens with `crypto/tls`; otherwise stays plaintext for
    backward compat during rollout.
  - `cmd/agc/main.go` — read the pinned cert from
    `/etc/actions-gateway/proxy-ca/tls.crt`, build a `*x509.CertPool`
    containing only that cert, install on the `http.Transport` used
    for proxy CONNECT. No `InsecureSkipVerify` anywhere.
- **Tests:**
  - Unit test in the GMC reconciler that the generated cert is valid,
    has the proxy DNS name as SAN, and is signed by the embedded key.
  - Integration test that the AGC successfully CONNECTs through the
    proxy over HTTPS, and rejects a connection to a proxy presenting
    a different cert.
  - e2e: assert `kubectl exec <agc> -- curl -x https://actions-gateway-proxy:8080 https://api.github.com`
    succeeds.
- **Done when:** the AGC↔proxy hop is TLS by default with no operator
  action, and pinning rejects any cert other than the one in the
  Secret.
- **Composes with cert-manager (optional v1.1):** if an operator
  wants cert-manager-issued certs instead, the reconciler can detect
  an externally-managed `Certificate` referencing the same Secret
  name and skip the self-issued path. Not required for v1.

#### W8 — AGC SecurityContext (closes M-7) — **Done (2026-05-23, commit `1155d6f`)**

- **What shipped:**
  - `cmd/gmc/internal/controller/builder.go:492-497` — `buildAGCDeployment`
    container now sets `RunAsNonRoot: true`, `ReadOnlyRootFilesystem: true`,
    `AllowPrivilegeEscalation: false`, `Capabilities.Drop: [ALL]`, and
    `SeccompProfile: RuntimeDefault`.
  - Same SecurityContext block applied to the proxy container at
    lines 323-327 (it previously had only `RunAsNonRoot`/`ReadOnly`/
    `AllowPrivilegeEscalation`; capabilities and seccomp were added by
    W8).
- **Tests:** `builder_test.go` asserts the full SecurityContext block on
  both AGC and proxy containers.

#### W9 — Ed25519 opt-in and RSA-3072 default (closes M-11a/c; sets up M-11b) — **Done 2026-05-23**

Ed25519 is implemented as an operator opt-in performance optimization.
RSA-3072 is the secure default. The infrastructure code supports both
key types; the real-GitHub compatibility probe (M-11b) remains open.

- **Files changed:**
  - `githubapp/runner_auth.go` — EdDSA signing branch (`SigningMethodEdDSA`
    for Ed25519 keys, `SigningMethodRS256` for RSA). Key type dispatched
    via type assertion on `crypto.Signer`.
  - `cmd/agc/internal/agentpool/crypto.go` — `KeyType` type, PKCS#8
    marshal/parse for both Ed25519 and RSA. Legacy PKCS#1 RSA parse
    retained for backward compatibility with existing Secrets.
  - `cmd/agc/internal/agentpool/pool.go` — `Agent.PrivateKey` typed as
    `crypto.Signer`; `NewPool` accepts `KeyType`; `createAgent` uses RSA-3072
    by default.
  - `cmd/agc/main.go` — `--agent-key-type` flag, default `rsa`.
- **Tests:** EdDSA signing path and PKCS#8 round-trip covered in
  `githubapp/runner_auth_test.go`.
- **M-11b (still open, deferred):** real-GitHub probe to verify the
  broker accepts Ed25519 SPKI and EdDSA JWT assertions. Blocked on two
  things: (1) extending `cmd/probe` with `-key-type` and
  `-register-test-runner` flags, and (2) a manual run with real GitHub
  App credentials. Does not affect the RSA-3072 default. Details under
  Manual verifications below.

### Phase 3 — Hardening backlog

Independent items, scheduled opportunistically.

| Finding | Workstream | Notes |
|---|---|---|
| M-10 | ~~Expand validating webhook *or* move to CRD CEL~~ | **Done (2026-05-25).** `proxy.maxReplicas ≤ 100`, `minReplicas ≤ maxReplicas` → CEL `x-kubernetes-validations`. `gitHubAppRef.namespace` confused-deputy check → webhook (`ValidateCreate`/`ValidateUpdate`), because k8s 1.30 CEL does not expose `self.metadata.namespace` at the resource root. |
| M-11b | Extend `cmd/probe` (add `-key-type`/`-register-test-runner` flags) then run against real GitHub App | Verifies broker accepts Ed25519 SPKI + EdDSA JWTs so `--agent-key-type=ed25519` opt-in is documented as working. Does not affect the RSA-3072 default. |
| M-11c | ~~Migrate `agentpool.createAgent`~~ | **Done (2026-05-23).** RSA-3072 is the default; Ed25519 is opt-in via `--agent-key-type=ed25519`. |
| M-12 | ~~Generic 502 in `cmd/proxy/proxy.go:103-106`~~ | **Done.** Dial error logged server-side; response body is generic `"upstream unavailable"`. |
| M-13 | ~~Cap broker error bodies (`body[:200]`)~~ | **Done.** `capBody(rawBody, 200)` used throughout broker client; `capBody` helper added. |
| M-15 | ~~In-process counter in `provisioner` for MaxWorkers~~ | **Accepted (D-6).** Soft ceiling; ResourceQuota is the hard limit. No code change needed. |
| M-16 | ~~Hash suffix in `safeName`~~ | **Done (2026-05-25).** `provisioner.go:safeName` already had the suffix. Added hash suffix to `actionsgateway_controller.go:labelSafe` (used for RunnerGroup names); 6 unit tests added. |
| L-1 | ~~Add `jti` to App-level JWT~~ | **Done.** `ID: newUUID()` sets the `jti` claim. |
| L-2 | ~~`http.Client{Timeout}` in each binary `main`~~ | **Done.** 60 s timeout client injected into broker client, registrar, and IP-range fetcher. |
| L-3 | ~~`//nolint:gosec` or migrate to `math/rand/v2`~~ | **Done.** `//nolint:gosec // jitter, not crypto` on both `rand.Int63n` calls. |
| L-4 | ~~Typed errors from `broker.Client`~~ | **Done (2026-05-24).** `CreateSession` now returns `*UnauthorizedError` for 401/403. Substring fallbacks removed from `isUnauthorized`/`isSessionExpired`. |
| L-5 | ~~Unified PEM parser shared by `agentpool/crypto.go` and `githubapp/auth.go`~~ | **Done (W9, 2026-05-23).** Both parsers already handle PKCS#8 and PKCS#1; the asymmetry no longer exists. |
| L-6 | ~~Return errors instead of `os.Exit` in `mustEnv`~~ | **Done.** All three `mustEnv` helpers (`cmd/agc`, `cmd/probe`, `cmd/gmc`) return errors; callers handle exit. |
| L-7 | ~~Require both stub URLs together~~ | **Done.** When `GITHUB_ORG_URL` is unset, `cmd/agc/main.go` requires both stub URLs; partial config returns an error. |

### Out of scope (flagged separately)

- **Image-digest pinning** of `AGC_IMAGE` and `PROXY_IMAGE` at GMC
  startup. Recommend rejecting any image reference not in
  `image@sha256:…` form.
- **`imagePullPolicy`** explicitly set on worker pods. With
  digest-pinned images this is moot, but in the floating-tag case
  Kubernetes defaults to `IfNotPresent` for non-`:latest` tags,
  opening a cache-poisoning surface.

### Informational (not in the queue)

- **M-2** — proxy destination allowlist is a designed omission;
  [Appendix G §G.1](../design/appendix-g-future-enhancements.md#g1-proxy-enforced-destination-allowlist)
  records the trigger conditions that would justify revisiting.

### Dependencies and ordering notes

- **W1 ↔ W2** — both touch the GMC reconciler but distinct files; can
  land in parallel. If they overlap on `reconcileResources`, sequence
  W1 first (smaller surface).
- **W3 ↔ W5** — independent. W3 changes credential transport; W5
  changes config-override transport. The earlier "everything in the
  Secret" framing is *not* part of this plan; non-secrets stay in env
  vars per H-1's revised scope.
- **W5 (Option B)** — if chosen, can ship before or after W3; no code
  overlap.
- **W7** — no external dependency under the GMC-self-signed model.
  Touches the GMC reconciler (cert generation + Secret), the proxy
  (TLS listener), and the AGC (pinned trust pool). Can land in Phase
  2 without operator-side prerequisites.
- **W9 (M-11a)** — independent of all other workstreams. Touches
  `cmd/probe`, `githubapp/runner_auth.go`, and
  `agentpool/crypto.go`. Lands in Phase 2 as a stepping stone for
  M-11b/c, which are sequenced after the probe runs.

### Acceptance criteria for "Phase 1 complete" — **Met in code (2026-05-23)**

The Phase 1 workstreams are implemented; the residual gap is end-to-end
validation against a freshly provisioned tenant on `kind`. The criteria
below remain the definition of "complete in the field." Status reflects
whether the *implementation* is in place; live validation is the final
acceptance step.

1. **W1, M-1** — Worker pod cannot reach GitHub except via the proxy.
   Code: ✅ split NetworkPolicies. Live validation: ⏳ pending (debug-pod
   `curl` against [docs/design/network-architecture.md](../design/network-architecture.md)
   procedure).
2. **W2, C-1** — A `RunnerGroup` with `privileged: true` in its
   PodTemplate is rejected at pod creation under default
   `securityProfile`. Code: ✅ PSA labels + CEL `XValidation`.
3. **W3, H-1** — The AGC pod has no `GITHUB_APP_*` env vars;
   credentials are mounted at `/etc/actions-gateway/github-app/`
   mode `0o400`. Code: ✅ volume mount in `buildAGCDeployment`; AGC
   reads from files.
4. **W4, H-2** — A pod running with the AGC's SA cannot list or get
   an unrelated Secret in the tenant namespace. Code: ⚠ partial; broad
   `list`/`watch` retained with Secret cache disabled as compensating
   control. Residual risk accepted per D-3.
5. **W5, M-14** — The `AGC_EXTRA_*` mechanism is flag-gated and off in
   production. Code: ✅ `--allow-agc-extra-env`, default `false`.

### Verification & test plan

Tests live at the cheapest level that gives real signal. If a unit test
can verify the invariant, don't write a kind e2e for it. If an
integration test catches it, don't wait for kind.

| Level | Tooling | CI |
|---|---|---|
| **Unit** | `go test`, no cluster | ✓ per PR |
| **Integration** | `envtest` (real API server, no CNI) | ✓ per PR |
| **E2e** | kind + fakegithub | ✓ per PR |
| **Manual** | Real GitHub App credentials or CNI cluster | ✗ on demand |

#### Unit tests

Assert desired-state objects (structs the controller builds) and
pure-logic properties. No cluster required.

| Finding | Test file | Assertion | Status |
|---|---|---|---|
| W1 — NP shapes | `cmd/gmc/internal/controller/builder_test.go` | `buildProxyNetworkPolicy`, `buildWorkloadNetworkPolicy`, `buildAGCNetworkPolicy`: correct `PodSelector`, PolicyTypes, and egress ports (proxy: DNS+8080; workload: DNS+proxy; AGC: DNS+443). DNS egress present in all three. | ✓ done |
| W2 — PSA labels | `cmd/gmc/internal/controller/builder_test.go` | Tenant namespace gets `pod-security.kubernetes.io/enforce: <profile>` + companion labels; `privileged` profile produces `privileged` label. | ✓ done |
| W3 — creds as files | `cmd/gmc/internal/controller/builder_test.go` | AGC Deployment has no `GITHUB_APP_*` env entries; Secret volume mount at `/etc/actions-gateway/github-app/` with `defaultMode: 0o400`. | ✓ done |
| W4 — RBAC verbs | `cmd/gmc/internal/controller/rbac_test.go` | GMC ClusterRole has no wildcard verbs and no wildcard on sensitive resources. AGC Role includes `list`/`watch` on secrets (required by agent pool; see H-2 residual risk note). | ✓ done |
| W5 — extra env | `cmd/gmc/internal/controller/builder_test.go` | `buildAGCDeployment` with nil `extraEnv` produces no `AGC_EXTRA_*` env entries. | ✓ done |
| W7 — cert mount | `cmd/gmc/internal/controller/builder_test.go` | Self-signed cert has proxy DNS SAN and 1-year validity; AGC Deployment mounts only the cert; proxy Deployment mounts cert + key. | ✓ done |
| W8 — SecurityContext | `cmd/gmc/internal/controller/builder_test.go` | AGC and proxy containers have `RunAsNonRoot`, `ReadOnlyRootFilesystem`, `AllowPrivilegeEscalation: false`, `Capabilities.Drop: [ALL]`, `SeccompProfile: RuntimeDefault`. | ✓ done |
| M-3 — PKCS#7 | `broker/crypto_test.go` | `pkcs7Unpad` returns the same sentinel error for empty, wrong-length, and wrong-byte padding; no panic on adversarial input. | ✓ done |
| M-4 — rerun URL path traversal | `cmd/agc/internal/provisioner/provisioner_test.go` | Adversarial `system.github.repository` values (`../`, `;`, spaces) are rejected before the rerun URL is built. Also fixed `repoSegmentRE` to require alphanumeric-first (previously `..` passed the old `^[A-Za-z0-9._-]+$` regex). | ✓ done |
| M-6 — RunnerOS injection | `broker/client_test.go` | `GetMessage` with adversarial `RunnerOS` (`"linux&admin=true"`) encodes to a single `os` value and does not smuggle additional parameters. | ✓ done |
| W9, M-11a — EdDSA | `githubapp/runner_auth_test.go` | EdDSA signing branch produces a JWT verifiable with the matching Ed25519 public key; PKCS#8 round-trip works for RSA and Ed25519. | ✓ done |
| L-1 — jti | existing JWT tests | `jti` claim is unique per-request and included in the signed assertion. | ✓ done |
| L-4 — typed errors | `broker/client_test.go`, `cmd/agc/internal/listener/goroutine_test.go` | `CreateSession` returns `*UnauthorizedError` for 401/403; `isUnauthorized`/`isSessionExpired` use `errors.As` only (no substring fallback). | ✓ done |

**W4 residual risk (H-2 accepted):** The AGC Role grants `get`, `list`,
`watch`, `create`, and `delete` on all Secrets in the tenant namespace.
`list`/`watch` are required by `agentpool.Pool.listSecrets`; narrowing them
by label is not possible through standard `rbacv1.PolicyRule` (no
`LabelSelector` field even in k8s.io/api v0.35 / KEP-4601 GA). The
substitute control is `Client.Cache.DisableFor[*corev1.Secret]` in the AGC
manager — Secret bodies are never held in-process. Do not write a test
asserting that `list` or `get` on an unrelated Secret is denied; both are
currently allowed and the design knowingly accepts this. See H-2 for full
rationale.

#### Integration tests

Use `envtest` (real Kubernetes API server, no CNI). Verify that the
controller correctly creates and updates objects in a live API.

| Finding | Test file | Assertion | Status |
|---|---|---|---|
| W1 — AGC NP existence | `cmd/gmc/internal/controller/integration/network_policy_test.go` | `actions-gateway-agc` NetworkPolicy exists with `app: actions-gateway-agc` selector and a port-443 egress rule after reconcile. | ✓ done |
| W1 — IPRange refresh | `cmd/gmc/internal/controller/integration/network_policy_test.go` | After `ipRangeReconciler.ReconcileNow(ctx)`, the workload NetworkPolicy's port-8080 proxy egress rule survives. | ✓ done |
| W2 — CEL admission (D-7) | `cmd/gmc/internal/controller/integration/crd_admission_test.go` | `envtest` rejects a CR with `privileged: true` in PodTemplate when `securityProfile: baseline`. | ✓ done |
| W4 — cross-namespace RBAC | `cmd/gmc/internal/controller/integration/rbac_scope_test.go` | AGC SA cannot perform actions outside its own namespace. | ✓ done |

#### E2e tests

Run the full GMC + fakegithub stack on a kind cluster. Verify live
cluster behavior — resource creation, controller reactions, pod state.

| Finding | Test file | Assertion | Status |
|---|---|---|---|
| W1 — NP exists in each tenant ns | `cmd/gmc/test/e2e/isolation_test.go` | `actions-gateway-proxy`, `actions-gateway-workload`, and `actions-gateway-agc` NetworkPolicies present in each tenant namespace (`E2E_GMC_NetworkPolicyScopedToNamespace`). | ✓ done |
| W2 — PSA label on namespace | `cmd/gmc/test/e2e/security_profile_test.go` | Namespace gets `pod-security.kubernetes.io/enforce=baseline` (and warn/audit) after reconcile; patching to `privileged` profile updates the label. | ✓ done |
| W3 — no env creds in AGC pod | `cmd/gmc/test/e2e/provisioning_test.go` | `kubectl exec` into AGC (`E2E_GMC_AGCNoCredentialEnvVars`): `env \| grep GITHUB_APP` returns nothing; `/etc/actions-gateway/github-app/` lists `appId`, `installationId`, `privateKey`. | ✓ done |
| W4 — RBAC provisioned | `cmd/gmc/test/e2e/provisioning_test.go` | `E2E_GMC_ServiceAccountAndRBACCreated`: ServiceAccount and RoleBinding present in tenant namespace. | ✓ done |
| W5 — no extra env without flag | unit test covers the builder invariant; e2e is skipped because the suite itself injects `AGC_EXTRA_*` to redirect AGC to fakegithub — testing "no flag" requires a separate GMC deployment. | n/a — covered by unit test | ✓ done (unit) |

> **⚠️ W1 traffic-enforcement tests — not CI-safe (requires CNI enforcement)**
>
> The kind cluster in CI runs `kindnet`, which stores NetworkPolicies in
> etcd but does **not** enforce them. Any test asserting "a curl from a
> worker pod to GitHub times out" passes vacuously — making it worthless
> as a correctness signal.
>
> These tests require a cluster with Calico or Cilium. They are not wired
> into the standard CI pipeline. To run locally against a CNI-enforced
> cluster:
>
> ```sh
> # Create a kind cluster with Calico (not kindnet), then:
> make e2e-netpol   # TODO: add this Makefile target
> ```
>
> The NP *shape* tests (unit + integration) run in CI and catch
> misconfiguration. The traffic-enforcement tests are a belt-and-suspenders
> check for CNI-enforced clusters — run them before deploying to production.

#### Manual verifications

Two verifications require real credentials and cannot run in CI.

**M-11b — Ed25519 broker compatibility probe (opt-in verification)**

**Status (2026-05-23): deferred — not blocked on attention, blocked on a
probe extension plus real GitHub App credentials.** Two pieces of work
have to happen before a verdict can be recorded:

1. **Probe extension (code, not yet written).** `cmd/probe/main.go`
   currently consumes `GITHUB_AGENT_ID`/`GITHUB_AGENT_NAME` from an
   already-registered runner and signs with the App's RSA private key. To
   verify Ed25519 specifically, the probe needs:
   - a `-key-type {rsa,ed25519}` flag selecting the key generated for the
     synthetic runner agent;
   - a `-register-test-runner` mode that POSTs to GitHub's runner-
     registration API with the generated SPKI public key, captures the
     returned `agentId`/`agentName`, signs the next assertion with the
     matching private key, exchanges it for a session, then cleans up the
     registration on exit.
2. **Manual run with real credentials.** Once the probe supports the flag,
   someone with a test GitHub App installation runs it and records the
   verdict here.

RSA-3072 is the secure default and is not gated on this probe — it stays
the default regardless of the outcome. M-11b only documents whether
`--agent-key-type=ed25519` is a working opt-in. See
[Appendix G §G.6](../design/appendix-g-future-enhancements.md#g6-x25519-ecdh-session-key-exchange)
for the broker-side change that would make Ed25519 the secure default.

**Intended procedure (once the probe is extended):**

Prerequisites:
- GitHub App with installation on a test org/repo
- App ID, installation ID, private key PEM
- Broker URL and runner version from a real `.runner` config

```sh
export GITHUB_APP_ID=<id>
export GITHUB_APP_PRIVATE_KEY=/path/to/key.pem
export GITHUB_APP_INSTALLATION_ID=<id>
export GITHUB_BROKER_URL=https://...
export GITHUB_RUNNER_VERSION=2.327.1
./.build/probe -key-type ed25519 -register-test-runner
```

Outcomes:

| Output | Verdict | Next step |
|---|---|---|
| `register OK / assertion OK / session OK` | Ed25519 opt-in fully working | Document as supported; note session messages arrive without AES layer. |
| `register OK / assertion 400 …` | Broker rejects EdDSA JWTs | Document `--agent-key-type=ed25519` as unsupported; leave flag but warn in help text. |
| `register 400 "...key type..."` | Broker rejects Ed25519 SPKI | Same as above. |
| Any other failure | Network or App-config issue | Retry; not a key-type signal. |

Owner: whoever picks up the probe extension. Record the outcome,
GitHub-reported runner version, and date as a paragraph here. The probe
code stays in the tree; re-run if GitHub's broker is updated to accept
EdDSA.

**D-2 — `restricted` profile against the runner image (resolved 2026-05-23)**

Probe run on kind/k8s-1.35 with `restricted` PSA enforcement.
Image: `ghcr.io/actions/actions-runner:latest` (v2.334.0, UID 1001).

```sh
kind create cluster --name d2-probe --config test/kind-config-ci.yaml
kubectl --context kind-d2-probe create ns runner-restricted-probe
kubectl --context kind-d2-probe label ns runner-restricted-probe \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/enforce-version=latest \
  pod-security.kubernetes.io/warn=restricted \
  pod-security.kubernetes.io/warn-version=latest \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/audit-version=latest

# runAsUser must be numeric; image USER "runner" = UID 1001
kubectl --context kind-d2-probe apply -f - <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: runner-probe
  namespace: runner-restricted-probe
spec:
  containers:
    - name: runner
      image: ghcr.io/actions/actions-runner:latest
      imagePullPolicy: Never
      command: ["sleep", "300"]
      securityContext:
        runAsNonRoot: true
        runAsUser: 1001
        runAsGroup: 1001
        allowPrivilegeEscalation: false
        capabilities: { drop: [ALL] }
        seccompProfile: { type: RuntimeDefault }
YAML

kubectl --context kind-d2-probe wait --for=condition=Ready \
  pod/runner-probe -n runner-restricted-probe --timeout=60s
kubectl --context kind-d2-probe exec -n runner-restricted-probe runner-probe \
  -- /home/runner/bin/Runner.Listener --version
# → 2.334.0, exit 0
```

**Result:** pod admitted and runner binary executed successfully.
`restricted` is a viable production profile. Tenants must set
`runAsUser: 1001` and `runAsGroup: 1001` in their
`RunnerGroup.spec.podTemplate`; without a numeric UID Kubernetes
rejects the pod with "cannot verify user is non-root".

#### CI gating

- **Per-PR:** unit tests + integration tests (envtest) + kind e2e (fakegithub).
- **On-demand (CNI cluster):** W1 traffic-enforcement tests (`make e2e-netpol`).
- **Manual (credentials required):** M-11b Ed25519 probe.
