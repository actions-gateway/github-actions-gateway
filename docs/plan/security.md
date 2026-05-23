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

## Critical

### C-1. RunnerGroup `PodTemplate` is not validated; tenant can ship privileged worker pods

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
- **Mitigation:** Split into two narrower rules with `resourceNames` listing
  the agent-pool Secret names *or* a label-selector-restricted Role. The
  path-of-least-resistance fix is `resourceNames` since the secret names are
  deterministic: `agentpool-<group>-<index>` and `job-<safePlanID>`. Use two
  Role rules with `resourceNames` patterns, or restrict to label selector
  via `controller-runtime`'s cache filter (the in-process AGC cache already
  filters; the *RBAC* surface does not).

---

## Medium

### M-1. NetworkPolicy permits worker pods to egress directly to GitHub, bypassing the proxy

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
| D-1 | M-14: Option A (flag-gate `AGC_EXTRA_*`) vs Option B (typed `endpointOverrides` CR field) | W5 | **Blocks W5** | — must pick |
| D-2 | Does `restricted` profile actually work with `ghcr.io/actions/runner`? Verify, or drop `restricted` from v1 | W2 | **Resolved (2026-05-23)** | `restricted` confirmed compatible; keep enum value; require `runAsUser: 1001` in PodTemplate |
| D-3 | W4 residual broad `secrets.create` — accept, or invest in per-job Secret alternative | W4 | Default if undecided | Accept with a code comment; revisit when a real exploit path is found |
| D-4 | W7 cert source: cert-manager required, cert-manager opt-out, or GMC-managed self-signed cert with AGC pinning | W7 | Default if undecided | **GMC self-signed cert + AGC pinning** (no cert-manager dependency, secure by default) |
| D-5 | M-11: RSA-3072 as secure default; Ed25519 as performance opt-in | M-11 | **Decided** | **RSA-3072 default; Ed25519 opt-in via `--agent-key-type=ed25519`. Ed25519 loses AES session-key layer; X25519 ECDH (Appendix G §G.6) is the path to making Ed25519 the secure default.** |
| D-6 | M-15: hard `MaxWorkers` (in-process counter) vs soft (document, rely on ResourceQuota) | M-15 (Phase 3) | Default if undecided | Soft ceiling; document and rely on ResourceQuota |
| D-7 | C-1 belt-and-suspenders: ship CEL `XValidation` + `buildPod` zero-out, or rely on PSA alone | W2 | Default if undecided | Ship the CEL rule (cheap, better failure mode); skip `buildPod` zero-out (PSA already covers it) |
| D-8 | Phase 1 parallelism — single contributor sequential, or parallel work streams | All Phase 1 | Default if undecided | Assume single contributor; sequence W3 → W4 → W1 → W2 → W5 (smallest blast radius first) |

**Detail on each:**

- **D-1 (blocks W5):** Option A is ~10 LoC + a flag. Option B is a CRD
  field + reconciler change + e2e fixture migration. Option B is the
  cleaner long-term shape (typed, per-tenant, visible in the CR);
  Option A is the lowest-churn fix for the immediate misconfiguration
  risk.

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
  - **M-11b (open):** real-GitHub compatibility probe — verifies that
    the broker accepts Ed25519 SPKI registration and EdDSA JWT assertions
    so that operators using `--agent-key-type=ed25519` get working agents.
    Does not gate the default; gates operator documentation.
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

### Phase 1 — Required for next release

Closes the only privilege-escalation surface (C-1) and the two
single-tenant credential-exposure weakenings (H-1, H-2), plus the
documented-contract gap that the worker-egress plan covers (M-1, M-8,
M-9).

#### W1 — NetworkPolicy split (closes M-1, M-8, M-9)

See [docs/plan/worker-egress-proxy.md](worker-egress-proxy.md) for the
full rationale and acceptance criteria.

- **Files:**
  - `cmd/gmc/internal/controller/builder.go` — replace
    `buildNetworkPolicy` with `buildProxyNetworkPolicy` + `buildAGCWorkerNetworkPolicy`,
    each `podSelector`-scoped.
  - `cmd/gmc/internal/controller/ipranges.go` — `patchNetworkPolicy`
    must look up the proxy ClusterIP (or only modify the
    GitHub-CIDR-bearing rule) so the AGC/worker→proxy egress rule is
    preserved across refreshes.
  - `cmd/gmc/internal/controller/actionsgateway_controller.go` —
    `reconcileResources` and `reconcileDelete` call two apply/delete
    helpers instead of one.
- **Tests:**
  - New e2e in `cmd/gmc/test/e2e/`: spawn a debug pod with worker
    labels, assert `curl --noproxy '*' https://api.github.com` times
    out and `curl -x http://actions-gateway-proxy:8080 …` succeeds.
  - Integration test in `cmd/gmc/internal/controller/integration/`
    asserting the IPRange refresh preserves the proxy-egress rule.
  - Update `network_policy_test.go` expectations.
- **Done when:** the validation snippets in
  [docs/design/network-architecture.md "How to Validate Network Isolation"](../design/network-architecture.md)
  pass against a freshly provisioned tenant.

#### W2 — Security profiles via PSA (closes C-1)

- **Files:**
  - `cmd/gmc/api/v1alpha1/actionsgateway_types.go` — add
    `SecurityProfile string` field, `enum` validation, default
    `baseline`. Regenerate zz_generated.deepcopy.go.
  - `cmd/gmc/internal/controller/actionsgateway_controller.go` — in
    `reconcileResources`, ensure the tenant namespace carries
    `pod-security.kubernetes.io/enforce`, `enforce-version`, `warn`,
    `audit` labels matching the profile. Idempotent label merge.
  - Optional belt-and-suspenders in
    `cmd/agc/internal/provisioner/provisioner.go` `buildPod`: zero
    out `Spec.SecurityContext.Sysctls`; reject `Spec.Volumes[i].HostPath`
    outside `privileged`. Add a CEL `XValidation` on
    `RunnerGroupSpec.PodTemplate` forbidding `privileged: true` in
    `baseline`/`restricted` profiles so the failure mode is at
    `kubectl apply`, not at pod creation.
- **Tests:**
  - Integration: apply ActionsGateway with each profile, assert the
    namespace labels match.
  - e2e: apply a `privileged: true` PodTemplate under each profile;
    assert pod creation is rejected by PSA for `baseline`/`restricted`
    and accepted for `privileged`.
  - Update existing e2e fixtures so any test PodSpec that needs
    privileged sets `securityProfile: privileged` on its CR.
- **Done when:** a tenant CR with default `securityProfile` rejects
  any pod the AGC tries to create with `privileged: true`, with no
  Kyverno/OPA installed on the test cluster.

#### W3 — Credentials via file mount (closes H-1)

- **Files:**
  - `cmd/gmc/internal/controller/builder.go` `buildAGCDeployment` —
    replace the three `GITHUB_APP_*` env entries with a Secret volume
    mount at `/etc/actions-gateway/github-app/` mode `0400`. Keep
    non-secret env vars (HTTP_PROXY, etc.) as env vars.
  - `cmd/agc/main.go` — read `appId`/`installationId` from files,
    pass the `privateKey` file path through `loadPEM`. Delete the
    env-var read path.
  - `cmd/probe/main.go` — same file-read path so the probe doesn't
    diverge from the AGC pattern.
- **Tests:**
  - `cmd/gmc/internal/controller/builder_test.go` — replace env-var
    assertions with volume-mount assertions (Secret name, mount path,
    `defaultMode: 0o400`).
  - `cmd/gmc/internal/controller/integration/provisioning_test.go` —
    update the GITHUB_APP_ID env probe to check the volume instead.
  - e2e tests already create the Secret correctly; no fixture change
    needed beyond updating any assertions on AGC pod env vars.
- **Done when:** `kubectl exec -it <agc-pod> -- env | grep GITHUB_APP`
  returns nothing; `kubectl exec -it <agc-pod> -- ls /etc/actions-gateway/github-app/`
  lists `appId`, `installationId`, `privateKey`.

#### W4 — Tighten AGC RBAC (closes H-2)

- **Files:**
  - `cmd/gmc/internal/controller/builder.go` `buildAGCRole` — split
    the `secrets` rule into two `resourceNames`-restricted rules:
    - `verbs: [get,create,delete]`, `resourceNames: [agentpool-*]`
      — *but* RBAC doesn't support glob `resourceNames`, so this
      requires either listing exact names (driven by reconciler) or
      using a label-selector cache filter and accepting that the RBAC
      surface is broader than the in-process filter.
  - In practice the cleanest fix is two rules: one with
    `resourceNames` explicitly enumerated for the agent-pool Secrets
    (the GMC knows the names) and a separate cache filter in the AGC.
    Job-payload Secrets are short-lived (created/deleted per job) so
    they need `create`/`delete` without `resourceNames` — accept that
    `create` on `secrets` remains broad, but constrain it via OPA/
    Kyverno only if available (not required for v1).
  - Document the residual broadness as a comment so future readers
    don't think the loose `create` is a bug.
- **Tests:**
  - `cmd/gmc/internal/controller/rbac_test.go` — assert the new rule
    shape.
  - `cmd/gmc/internal/controller/integration/rbac_scope_test.go` —
    assert the AGC's SA cannot `list secrets` cluster-wide and cannot
    `get` an unmanaged Secret in the tenant namespace.
- **Done when:** an attacker-controlled pod with the AGC SA cannot
  read a user-created Secret (e.g. `kubectl create secret generic
  developer-secret`) in the same namespace.

#### W5 — `AGC_EXTRA_*` mechanism (closes M-14)

Blocked by D-1.

- **If Option A (flag-gated):**
  - `cmd/gmc/cmd/main.go` — add `--allow-agc-extra-env` flag, default
    `false`. The forwarding loop runs only when the flag is set.
  - Test rigs in `cmd/gmc/test/e2e/e2e_suite_test.go` pass the flag.
- **If Option B (typed CR field):**
  - `cmd/gmc/api/v1alpha1/actionsgateway_types.go` — add
    `EndpointOverrides` struct (apiBaseURL, brokerURL, stubAuthURL,
    stubBrokerURL), CEL-validated.
  - `cmd/gmc/internal/controller/builder.go` `buildAGCDeployment` —
    stamp the override env vars per-tenant from the CR field.
  - Delete the `AGC_EXTRA_*` forwarding loop and `AGCExtraEnv` field.
  - Migrate e2e fixtures (`e2e_suite_test.go`, `github_e2e_test.go`)
    to set the new CR field instead of GMC pod env vars.

### Phase 2 — Next milestone

#### W6 — Crypto and injection hygiene (closes M-3, M-4, M-6)

- **Files:**
  - `broker/crypto.go` — single sentinel error from `pkcs7Unpad`,
    constant-time padding check.
  - `cmd/agc/internal/provisioner/provisioner.go` — validate `owner`/`repo`
    against `[A-Za-z0-9._-]`, `url.PathEscape` each segment before
    building the rerun URL.
  - `broker/client.go` — build `GetMessage` and `DeleteSession` URLs
    with `net/url` instead of string concatenation.
- **Tests:** unit tests with adversarial inputs in each affected file.

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

#### W8 — AGC SecurityContext (closes M-7)

- **Files:** `cmd/gmc/internal/controller/builder.go` —
  `buildAGCDeployment` gets the same `SecurityContext` block as the
  proxy plus `Capabilities.Drop: ALL` and
  `SeccompProfile: RuntimeDefault`. Apply the same caps/seccomp
  hardening to the proxy.
- **Tests:** assert on container `SecurityContext` in
  `builder_test.go`.

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
- **M-11b (still open):** real-GitHub probe to verify the broker accepts
  Ed25519 SPKI and EdDSA JWT assertions — documents that the opt-in path
  works end-to-end. Does not affect the default.

### Phase 3 — Hardening backlog

Independent items, scheduled opportunistically.

| Finding | Workstream | Notes |
|---|---|---|
| M-10 | Expand validating webhook *or* move to CRD CEL | Prefer CEL where possible — visible in `kubectl explain`. |
| M-11b | Run probe against real GitHub App | Verifies broker accepts Ed25519 SPKI + EdDSA JWTs so `--agent-key-type=ed25519` opt-in is documented as working. Does not affect the RSA-3072 default. |
| M-11c | ~~Migrate `agentpool.createAgent`~~ | **Done (2026-05-23).** RSA-3072 is the default; Ed25519 is opt-in via `--agent-key-type=ed25519`. |
| M-12 | Generic 502 in `cmd/proxy/proxy.go:103-106` | Log detail server-side. |
| M-13 | Cap broker error bodies (`body[:200]`) | Add debug env var to log full body. |
| M-15 | In-process counter in `provisioner` for MaxWorkers | Or document MaxWorkers as a soft ceiling and rely on ResourceQuota. |
| M-16 | Hash suffix in `safeName` | Both `provisioner.go` and `builder.go` variants. |
| L-1 | Add `jti` to App-level JWT | Optional. |
| L-2 | `http.Client{Timeout}` in each binary `main` | Inject into broker client, registrar, IPRange fetcher. |
| L-3 | `//nolint:gosec` or migrate to `math/rand/v2` | Trivial. |
| L-4 | Typed errors from `broker.Client` | `*UnauthorizedError`, `*SessionExpiredError`; use `errors.As` instead of substring match. |
| L-5 | Unified PEM parser shared by `agentpool/crypto.go` and `githubapp/auth.go` | Currently asymmetric. |
| L-6 | Return errors instead of `os.Exit` in `mustEnv` | Operability, not security. |
| L-7 | Require both stub URLs together | Reject half-configured stub state in `cmd/agc/main.go`. |

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

### Acceptance criteria for "Phase 1 complete"

A freshly provisioned tenant on a vanilla Kubernetes cluster (no
Kyverno, no OPA, no service mesh) satisfies all of the following:

1. Worker pod cannot reach GitHub except via the proxy
   (verifies W1, M-1).
2. A `RunnerGroup` with `privileged: true` in its PodTemplate is
   rejected at pod creation under default `securityProfile`
   (verifies W2, C-1).
3. The AGC pod has no `GITHUB_APP_*` env vars; credentials are mounted
   at `/etc/actions-gateway/github-app/` mode `0400`
   (verifies W3, H-1).
4. A pod running with the AGC's SA cannot list or get an unrelated
   Secret in the tenant namespace
   (verifies W4, H-2).
5. The `AGC_EXTRA_*` mechanism is either flag-gated and off in
   production, or removed entirely
   (verifies W5, M-14).

### Verification & test plan

Each finding lands in one of three verification venues. The table below
maps the workstream/finding to its venue so reviewers and implementers
can find the gating check without re-reading the whole document.

| Venue | What goes here | When it runs |
|---|---|---|
| **Static checks** — `go test ./...` and `envtest`-backed integration | Anything verifiable from the in-process Kubernetes API or pure unit logic | Per PR, in CI |
| **Kind-based e2e** — `cmd/gmc/test/e2e/` against `fakegithub` | Anything requiring a running cluster + the provisioner reacting | Per PR, in CI |
| **Real-GitHub manual** — `cmd/gmc/test/e2e/github_e2e_test.go` or `cmd/probe` | Anything requiring real GitHub responses (broker key-type acceptance, registration token issuance) | Manual, gated on credentials |

#### Static checks

These run as Go tests with no cluster. Each is asserted on the *desired
state objects* the GMC produces, not by observing a live cluster.

| Workstream / finding | Test location | Assertion |
|---|---|---|
| W1, M-1, M-8 | `cmd/gmc/internal/controller/network_policy_test.go` | Two `NetworkPolicy` objects emitted; each has a non-empty `PodSelector`; egress rules match the expected per-selector shape. |
| W2, C-1 | `cmd/gmc/internal/controller/builder_test.go` | Tenant namespace gets `pod-security.kubernetes.io/enforce: <profile>` plus the three companion labels; `securityProfile: privileged` produces `privileged` label. |
| W2 belt-and-suspenders (D-7) | `cmd/gmc/internal/controller/integration/crd_admission_test.go` | `envtest` rejects a CR with `privileged: true` in PodTemplate when `securityProfile: baseline`. |
| W3, H-1 | `cmd/gmc/internal/controller/builder_test.go` | AGC Deployment has no `GITHUB_APP_*` env entries; has a Secret volume mount at `/etc/actions-gateway/github-app/` with `defaultMode: 0o400`. |
| W4, H-2 | `cmd/gmc/internal/controller/rbac_test.go` and `cmd/gmc/internal/controller/integration/rbac_scope_test.go` | AGC Role has `resourceNames` patterns on the agent-pool Secret rule; SAR check confirms the AGC SA cannot list cluster-wide and cannot `get` an unrelated Secret in-namespace. |
| W7, M-5 | `cmd/gmc/internal/controller/builder_test.go` | Self-signed cert generated has the proxy DNS name as SAN and 1-year validity; AGC Deployment mounts only the cert (public part); proxy Deployment mounts cert + key. |
| W8, M-7 | `cmd/gmc/internal/controller/builder_test.go` | AGC and proxy container `SecurityContext` has `RunAsNonRoot: true`, `ReadOnlyRootFilesystem: true`, `AllowPrivilegeEscalation: false`, `Capabilities.Drop: [ALL]`, `SeccompProfile: RuntimeDefault`. |
| M-3 | `broker/crypto_test.go` | `pkcs7Unpad` returns the same sentinel error for empty, wrong-length, and wrong-byte padding inputs. Adversarial inputs do not panic. |
| M-4 | `cmd/agc/internal/provisioner/provisioner_test.go` | Adversarial `system.github.repository` values (`../`, `;`, spaces) are rejected before the rerun URL is built. |
| M-6 | `broker/client_test.go` | `GetMessage` URL with an adversarial `RunnerOS` value escapes the query parameter and does not smuggle additional parameters. |
| W9, M-11a | `cmd/probe/main_test.go` (new), `githubapp/runner_auth_test.go` | EdDSA signing branch produces a valid JWT verifiable with the matching public key; PKCS#8 round-trip works for both RSA and Ed25519 keys. |

#### Kind-based e2e

These add Ginkgo `It` blocks to the existing e2e suite. The kind
cluster, image builds, and `fakegithub` harness are reused. Each
acceptance-criterion item above maps to one or more `It` blocks.

| Workstream / finding | Test file | Assertion |
|---|---|---|
| W1 (acceptance criterion 1) | new `cmd/gmc/test/e2e/network_isolation_test.go` | Debug pod with worker labels: direct `curl https://api.github.com` times out; `curl -x http://actions-gateway-proxy:8080 …` succeeds. |
| W1 (regression for M-9) | same file | Trigger the IPRange reconciler refresh; verify the worker→proxy egress rule survives. |
| W2 (acceptance criterion 2) | new `cmd/gmc/test/e2e/security_profile_test.go` | Apply RunnerGroup with `privileged: true` PodTemplate under each profile; assert pod creation is rejected by PSA under `baseline`, accepted under `privileged`. Observed via Events containing `violates PodSecurity`. |
| W3 (acceptance criterion 3) | extend `cmd/gmc/test/e2e/provisioning_test.go` | `kubectl exec` into AGC: `env | grep GITHUB_APP` returns nothing; `ls /etc/actions-gateway/github-app/` lists `appId`, `installationId`, `privateKey`. |
| W4 (acceptance criterion 4) | new `cmd/gmc/test/e2e/rbac_isolation_test.go` | Create a user-managed Secret; spawn a pod with the AGC SA; `kubectl get secret <name>` fails with `forbidden`. |
| W5 (acceptance criterion 5) | extend `cmd/gmc/test/e2e/e2e_suite_test.go` | If D-1 = Option A: assert `AGC_EXTRA_*` env vars on the GMC pod do not propagate without `--allow-agc-extra-env`. If D-1 = Option B: assert `AGC_EXTRA_*` is removed and `spec.endpointOverrides` populates the AGC env. |
| W7 cert pinning | new `cmd/gmc/test/e2e/proxy_tls_test.go` | AGC reaches proxy over `https://`; rotating the proxy cert to an unrelated self-signed cert causes the AGC's CONNECT to fail until the AGC's trust file is updated. |

#### Real-GitHub manual verifications

Two verifications cannot be done without real GitHub credentials. Each
records its outcome as a one-paragraph note in this document.

**M-11b — Ed25519 broker compatibility probe (opt-in verification)**

**Purpose:** RSA-3072 is the secure default and is not gated on this
probe. The probe verifies that operators using `--agent-key-type=ed25519`
get a working agent end-to-end — that the broker accepts Ed25519 SPKI
at registration and EdDSA-signed JWT assertions at token issuance. This
documents the opt-in path rather than driving a default change.

Prerequisites:
- GitHub App with installation on a test org/repo
- App ID, installation ID, private key PEM
- Broker URL and runner version from a real `.runner` config

Procedure:

```sh
export GITHUB_APP_ID=<id>
export GITHUB_APP_PRIVATE_KEY=/path/to/key.pem
export GITHUB_APP_INSTALLATION_ID=<id>
export GITHUB_BROKER_URL=https://...
export GITHUB_RUNNER_VERSION=2.327.1
./cmd/probe -key-type ed25519 -register-test-runner
```

Outcomes:

| Output | Verdict | Next step |
|---|---|---|
| `register OK / assertion OK / session OK` | Ed25519 opt-in fully working | Document as supported; note session messages arrive without AES layer. |
| `register OK / assertion 400 …` | Broker rejects EdDSA JWTs | Document `--agent-key-type=ed25519` as unsupported; leave flag but warn in help text. |
| `register 400 "...key type..."` | Broker rejects Ed25519 SPKI | Same as above. |
| Any other failure | Network or App-config issue | Retry; not a key-type signal. |

Owner: whoever has the GitHub App credentials. Record the outcome,
GitHub-reported runner version, and date as a paragraph here. The probe
code stays in the tree; re-run if GitHub's broker is updated to accept
EdDSA. See also
[Appendix G §G.6](../design/appendix-g-future-enhancements.md#g6-x25519-ecdh-session-key-exchange)
for the broker change that would make Ed25519 the secure default.

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

- **PR pipeline:** runs static checks and kind-based e2e against
  `fakegithub`. Phase 1 acceptance criteria become required checks.
- **Pre-release (manual or scheduled):** runs
  `cmd/gmc/test/e2e/github_e2e_test.go` against real GitHub to verify
  the full broker protocol still works with all of Phase 1's changes
  applied.
- **One-shot, gated on credentials:** M-11b probe.
