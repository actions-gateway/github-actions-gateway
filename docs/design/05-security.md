# 5. Security & Threat Risk Assessment

← [Operational Flows](04-operational-flows.md) | [Back to index](README.md) | Next: [Implementation Phases →](06-implementation-phases.md)

---

The two-tier architecture introduces both stronger isolation guarantees and new attack surfaces. Threats are grouped by which tier they affect.

## 5.1. GMC-Level Threats (Cluster-Scoped)

| Threat Vector | Impact | Mitigation Strategy |
| --- | --- | --- |
| **GMC Privilege Escalation** (Blast Radius: All Tenants) | Critical | The GMC's ClusterRole grants cluster-wide *write* (`create`/`update`/`delete`) on the tenant provisioning kinds — Deployments, Services, ServiceAccounts, RoleBindings, Roles, NetworkPolicies, HPAs, PodDisruptionBudgets, RunnerGroups, and Secret `create`/`update` — because RBAC cannot express "only namespaces that carry a marker label". Two `ValidatingAdmissionPolicy` objects close that gap by confining the GMC ServiceAccount at admission: `namespace-psa-guard` confines `namespaces:patch` to the six `pod-security.kubernetes.io/*` keys on marked namespaces, and **`gmc-tenant-resource-guard` confines every `create`/`update`/`delete` of the kinds above to namespaces an administrator has marked `actions-gateway.github.com/tenant: "true"`** (Q121 write path / Q122). So a compromised GMC cannot create a Deployment or RoleBinding in `kube-system`, write a Secret into an arbitrary namespace, or relabel `kube-system` PSA to `privileged` (see [§5.3](#53-security-profiles-and-the-privileged-opt-in)). The grant RBAC and admission cannot confine is Secret **reads**: admission never runs on `get`/`list`/`watch`, `resourceNames` cannot scope `list`/`watch`, and tenant Secret names are dynamic so `get` cannot be name-scoped either — therefore the ClusterRole grants cluster-wide Secret `get`/`list`/`watch`, and this is an honestly-stated residual, not a confined property (Q121; compensating controls below). **Explicit blast radius if compromised:** a compromised GMC can (a) enumerate every `ActionsGateway` CR in the cluster, learning each tenant's `gitHubAppRef` name and namespace; (b) `list`/`watch` and `get` the full `.data` of **any** Secret in the cluster, including GitHub App private keys (the metadata-only informer and uncached reads below are client-side hygiene, not authorization); (c) create/update/delete workloads and Secrets **only in marked tenant namespaces** (the `gmc-tenant-resource-guard` VAP denies writes elsewhere). It CANNOT exec into pods, create new namespaces, or write resources into unmarked namespaces such as `kube-system`. **Secret-read compensating controls:** the GMC uses `WatchesMetadata` so the informer cache holds only Secret ObjectMeta (no `.data`), `client.Cache.DisableFor[*corev1.Secret]` forces `r.Get()` to hit the API server directly (key material is never cache-resident), in practice the GMC only `get`s the named credential Secret in the CR's own namespace, and the [Q29 audit policy](../plan/security-audit-2026-06.md) is the detective complement that surfaces any out-of-pattern Secret read. GMC pod runs with non-root user, read-only root filesystem, no host mounts, and `seccompProfile: {type: RuntimeDefault}`. Image is digest-pinned, enforced at chart render time — the Helm chart refuses to render an unpinned `gmc.image`. Treat the GMC pod as a Tier-0 workload for monitoring and access. |
| **Admission Webhook Unavailability or Bypass** | Medium | The reserved-namespace validating webhook serves as a safety check, not a security boundary — namespace isolation is enforced by RBAC and NetworkPolicy regardless of webhook state. The webhook uses `failurePolicy: Fail` so requests are rejected when the webhook pod is unhealthy rather than silently bypassed. Serving certificates are managed by cert-manager with automatic rotation; the CA bundle is injected via `caBundle` from the cert-manager-managed Secret. Webhook pod runs `replicas: 2` behind a Service with `podAntiAffinity` to survive single-node loss without stalling tenant onboarding. |
| **Tenant Namespace Escape via Overpermissioned AGC** | Critical | Each AGC's ServiceAccount is bound by a RoleBinding limited to its own namespace. The AGC cannot list or touch resources in any other tenant namespace. |
| **Cross-Tenant GitHub App Credential Leakage** | High | `ActionsGateway` is namespace-scoped, so a tenant's `gitHubAppRef` defaults to their own namespace — another tenant cannot reference it. The GMC mounts credentials into the AGC Pod only; worker pods never have access to the Secret object. Secrets are immutable; rotation creates a new Secret and updates the CR reference, producing a clean Deployment rollout. Old Secrets are not readable by running Pods once the rollout completes. The GMC's ClusterRole grants Secret `get`/`list`/`watch` cluster-wide (not name-scoped — `resourceNames` cannot scope `list`/`watch`, and tenant Secret names are dynamic so it cannot scope `get` either); Secret **writes** (`create`/`update`) are confined to marked tenant namespaces by the `gmc-tenant-resource-guard` `ValidatingAdmissionPolicy`, but reads cannot be confined at the authorization layer (admission does not run on read verbs — Q121). **Two-layer cache isolation** (the compensating control for reads): the GMC uses `WatchesMetadata` (not a full Secret watch) so the in-process informer cache holds only Secret ObjectMeta (name, namespace, resourceVersion — no `.data`); and `client.Cache.DisableFor[*corev1.Secret]` ensures `r.Get()` calls bypass the cache entirely and hit the API server directly, so actual key material is never resident in memory beyond the duration of a single reconcile call. |
| **`ActionsGateway` CR in Reserved Namespace** | Medium | An admission webhook rejects `ActionsGateway` CRs created in reserved namespaces: the universal `kube-system` and `kube-public`, the GMC's default install namespace `gmc-system`, and the namespace the GMC pod is actually running in (read from the `POD_NAMESPACE` downward-API env var, so custom installs are protected too). Since the CR is namespace-scoped, a tenant can only affect their own namespace — the risk is self-harm or collision with operator-owned resources, not cross-tenant impact. |

---

## 5.2. AGC & Proxy-Level Threats (Namespace-Scoped)

Several mitigations below rest on the per-tenant NetworkPolicies the GMC reconciles (workload egress restricted to DNS + proxy, with **DNS itself confined to the cluster DNS service** rather than any resolver — Q105; only AGC-labelled pods get apiserver egress; **workload pods default-deny all ingress** — Q128). The ingress default-deny matters because worker pods run untrusted GitHub Actions job code and are outbound-only by design (they long-poll/dial out to GitHub via the proxy and to the AGC); nothing legitimately initiates a connection *to* a worker, so the workload NP declares `policyTypes: [Ingress, Egress]` with an empty ingress rule set. Without it, worker pods were default-allow ingress and any pod in the cluster could open connections to untrusted job code — a lateral-movement / cross-tenant channel. NetworkPolicy objects are inert unless the cluster CNI enforces them — kind's default kindnet does **not** drop traffic, so production clusters must run a policy-enforcing CNI (Calico, Cilium, or equivalent). Runtime enforcement of the egress negatives was observed on a Calico kind cluster on 2026-06-11 (Q7b; see [network-architecture.md § How to Validate Network Isolation](network-architecture.md#how-to-validate-network-isolation)).

| Threat Vector | Impact | Mitigation Strategy |
| --- | --- | --- |
| **Host Namespace Escape via Malicious Workflow** (Container Breakout) | Critical | Enforced in three layers. (1) The AGC unconditionally sets `hostPID: false`, `hostNetwork: false`, `hostIPC: false`, and `automountServiceAccountToken: false` on every worker pod, overwriting tenant `PodTemplate` values at pod-creation time. (2) The GMC stamps `pod-security.kubernetes.io/enforce` on the tenant namespace at provisioning time, with the level chosen by `ActionsGateway.spec.securityProfile` — see [§5.3](#53-security-profiles-and-the-privileged-opt-in). The default `baseline` blocks privileged containers, hostPath, dangerous capabilities, and host namespaces via the in-tree PodSecurity admission plugin; no external policy engine is required. (3) Sandboxed container runtimes (Kata Containers, gVisor) are supported via `runtimeClassName` in the `PodTemplate` — optional but strongly recommended for tenants who select the `privileged` profile. See [Appendix B](appendix-b-worker-isolation.md) for tradeoffs. |
| **Supply-Chain Compromise of Worker Image** | High | `WorkerImage` SHOULD reference an immutable digest, not a floating tag (see [§3.1](03-api-contracts.md#31-kubernetes-crd-schemas)). Digest pinning eliminates the "update the tag, get a different binary" attack class. Operators are expected to restrict the set of permitted registries via cluster admission policy (e.g. Kyverno, OPA Gatekeeper) — the GMC does not enforce this itself because registry policy is a cluster-wide concern. The gateway's own CI runs two supply-chain gates on every PR (`security-scan.yml`): `govulncheck` across all Go modules and `trivy` image scans of all five built images — see [testing.md § Security scanning](../development/testing.md#security-scanning). The four images built from a minimal/distroless base block on fixable HIGH/CRITICAL findings; the default worker image (built `FROM` the upstream actions-runner) is scanned report-only because its CVEs live in upstream components, with base bumps automated via dependabot. Tenants supplying their own `WorkerImage` are still expected to scan it themselves. `imagePullPolicy: IfNotPresent` (digest) or `Always` (tag) ensures the kubelet does not serve a stale, possibly tampered local copy. For the AGC and proxy images the GMC itself injects (`AGC_IMAGE`/`PROXY_IMAGE`), digest pinning is *enforced*, not advisory: the GMC rejects any reference not in `image@sha256:<digest>` form at startup, with a `--allow-floating-image-tags` opt-out reserved for dev/test. The GMC's *own* image is enforced one layer earlier, at chart render time — nothing at runtime validates the image the GMC runs from, so the Helm chart fails to render while `gmc.image.digest` is empty (the same `allowFloatingImageTags` value is the dev/test opt-out), and the CI `manifest-validate` gate asserts that a default-values render is rejected, so the check cannot regress to fail-open. All four first-party images additionally carry `org.opencontainers.image.*` provenance labels (`source`/`revision`/`version`/`title`/`description`, with `revision`/`version` stamped from the build's git SHA via `docker-bake.hcl`) so SBOM scanners can trace an image back to its commit, and their Go binaries are compiled with `-trimpath -ldflags=-buildid=` for path-free, reproducible output (SLSA-L3-friendly). On a `v*` release tag, `publish.yml` pushes those four images to GHCR as **multi-arch OCI indexes** (`linux/amd64` + `linux/arm64`; the Go builder stages cross-compile on `$BUILDPLATFORM`, and the digest operators pin is the index digest) and **signs each one keyless with `cosign`** (sigstore/Fulcio via GitHub Actions OIDC — no long-lived signing key, no stored secret), recursively over the index and every per-arch manifest, and attaches an SPDX-JSON SBOM **per architecture** as a cosign attestation on that architecture's manifest, so a downstream operator can `cosign verify` the publish-workflow identity before deploying and enforce it cluster-wide via an admission policy. The PR-time `security-scan.yml` already generates each SBOM as a build artifact so that path can't silently break. The publish pipeline itself is hardened against upstream-tag hijack: every `uses:` across `.github/workflows/` is pinned to a full commit SHA (the `publish` job holds `id-token: write`, so a mutable action tag repointed at malicious code could otherwise keyless-sign images as the release identity), runtime tool downloads are version-pinned (`cosign` via `cosign-installer`, `syft` via `syft-version`), and Dependabot's `github-actions` ecosystem bumps the SHA pins so they don't rot (Q123). The keyless signing identity is **tags-only**: `publish.yml` refuses to run from a non-tag ref and `make verify-release` anchors the cosign `--certificate-identity-regexp` to `…/publish.yml@refs/tags/v.*$`, so a signature minted from a branch (e.g. a `workflow_dispatch` from a scratch branch overwriting a released tag) is both prevented and rejected (Q124). See [security-operations.md § Image provenance](../operations/security-operations.md#image-provenance-signature--sbom-verification) for the operator verification runbook and [release.md § Supply-chain integrity of the pipeline](../operations/release.md#supply-chain-integrity-of-the-pipeline-itself) for the SHA-pin / signing-identity policy. |
| **Cross-Job Code Contamination** | High | Enforce absolute 1-Job-Per-Pod isolation. Avoid reusing volumes or host paths between worker pods. Use ephemeral, `emptyDir` volumes for workspace storage. |
| **AGC Token Compromise** | High | The AGC never saves plaintext keys to disk. GitHub App private keys are mounted as read-only volumes with restrictive file permissions (0400). |
| **Credential Leak via Logged Error Bodies** | Medium | The AGC, broker client, and probe interpolate upstream GitHub HTTP response bodies into errors that callers log. Some of these bodies carry credential material — the runner-token endpoint's 200 body holds an access token, and `generate-jitconfig`'s body holds the runner JIT registration credential plus RSA key. Before any upstream body is placed into an error or log line it passes through a single shared redactor (`githubapp.SanitizeBody`) that strips credential-shaped substrings (GitHub `gh*_`/`github_pat_` tokens, JWTs, `access_token`/`encoded_jit_config`/`private_key`/`secret` JSON values, and long opaque base64 blobs) and caps the result. Redaction runs before capping so a secret straddling the cap boundary cannot survive in the truncated tail. No secret is ever logged directly; this control hardens the indirect path. |
| **Eviction-Retry API Misuse** | Medium | The AGC calls `POST /repos/{owner}/{repo}/actions/runs/{run_id}/rerun-failed-jobs` using the tenant's installation access token when a worker pod is evicted. The blast radius is bounded: the installation token is scoped to the GitHub App's installation on a specific organization or repository, so the AGC cannot re-run jobs belonging to other tenants or organizations. The `run_id` is extracted from the job payload delivered by GitHub's broker — the AGC cannot fabricate or substitute a run ID for a run it did not acquire. To prevent abuse of the retry path (e.g. a compromised AGC looping re-runs), `maxEvictionRetries` caps the number of automatic retries per job and is enforced before the API call is made. Operators should monitor `actions_gateway_eviction_retries_exhausted_total` to detect abnormal eviction patterns. |
| **DNS Exfiltration Side-Channel** (Unattributed Egress) | Medium | The per-tenant egress-IP attribution that isolates tenants rests on *all* real egress traversing the tenant proxy, whose source IPs are attributable. An unrestricted port-53 egress rule (`to: []` ≡ any resolver) would defeat that: any pod — including untrusted worker job code — could smuggle data out by encoding it into DNS queries aimed at an attacker-controlled authoritative server, an unattributed side-channel that never touches the proxy. All three per-tenant NetworkPolicies (workload, AGC, proxy) therefore confine port-53 egress to the **cluster DNS service only** (`kube-dns` / `CoreDNS` in `kube-system`, matched by `namespaceSelector` on `kubernetes.io/metadata.name: kube-system` plus `podSelector` on `k8s-app: kube-dns`). `kube-dns` recurses upstream on the pod's behalf, so legitimate resolution (including the proxy's own GitHub-hostname lookups) is unaffected — only the "any resolver" breadth is removed (Q105). Like the other egress negatives, this is enforced only by a policy-aware CNI; the reliable CI guard is the authoring-level test `TestBuildNetworkPolicy_DNSEgressRestrictedToKubeDNS`, which asserts every policy's DNS rule selects kube-dns and is never open. |
| **Proxy as Traffic Interception Point** | Medium | The proxy only handles CONNECT tunneling and does not terminate TLS. It cannot inspect or modify the encrypted payload between the AGC/worker and GitHub. Proxy pods run with a read-only root filesystem and no elevated capabilities. |
| **Cross-Tenant Proxy CA Trust** | Medium | The egress proxy's TLS cert is signed by a cert-manager-issued self-signed CA stored in the per-tenant `actions-gateway-proxy-tls` Secret. The AGC pins this CA explicitly (via its trust pool) rather than trusting the cluster's root store, and worker pods install the same CA into a combined `SSL_CERT_FILE` bundle so Runner.Worker's .NET HttpClient accepts the proxy handshake. The cert (`tls.crt`) is projected into both AGC and worker pods via an `Items: [tls.crt]` Secret volume; the private key (`tls.key`) is mounted *only* into the proxy pod itself, so a runner compromise does not yield the ability to forge a proxy cert. Trust is tenant-scoped: each tenant's CA is independent, so a compromised CA in one namespace cannot mint a cert trusted by another tenant's AGC or workers. |
| **Egress IP Change Mid-Session** | Low–Unknown | GitHub's broker protocol is token-based, not IP-bound. Session IDs and bearer tokens carry no IP affinity, so rotating across proxy pods mid-job is expected to work. The Twirp log stream is naturally sticky (long-lived HTTP/2 connection stays on one proxy pod once open). Impact is unknown because GitHub's abuse detection heuristics are undocumented. **Early mitigation: the [Milestone 1](06-implementation-phases.md#milestone-1-wire-protocol-probe-days-14) wire protocol probe explicitly tests broker API calls routed through a multi-pod proxy pool to confirm GitHub does not reject or flag IP variance across `CreateSession → GetMessage → AcquireJob`.** If the probe surfaces a problem, `ClientIP` session affinity on the proxy Service is the low-effort fallback; explicit per-goroutine proxy assignment is the higher-fidelity option if needed. |
| **Proxy Pool Exhaustion** (DoS via proxy saturation) | Medium | HPA `minReplicas` ensures a floor of available capacity. The `PodDisruptionBudget` prevents draining all replicas simultaneously. The platform-owned namespace `ResourceQuota` caps proxy pod count so a misconfigured `maxReplicas` cannot consume cluster CPU. |
| **Denial of Service via Resource Exhaustion** | Medium | The namespace `ResourceQuota` is **platform-owned** — the platform admin sets it on the tenant namespace and GAG operates within it without ever creating or mutating it (Q130). It is the hard cap: a tenant-authored quota was removed pre-1.0 because the tenant could simply raise it. CPU/Memory limits are also defined in the `RunnerGroup` CRD spec. Rogue workflows cannot exceed the tenant quota. Dropping the GMC's quota-write RBAC is least privilege (partially subsumes Q122). |
| **Cross-Tenant Pod Preemption via PriorityClass** | High | `priorityTiers[].priorityClassName` stamps a **cluster-scoped** `PriorityClass` onto worker pods. A `PriorityClass` carries a priority value and a `preemptionPolicy` (Kubernetes default `PreemptLowerPriority`), so an unvalidated tenant-chosen class would let a tenant name a high-priority, preempting class and have the scheduler **evict other tenants' running worker pods** to schedule its own — defeating per-tenant isolation. The platform owns *which* classes a tenant may reference (Q132): the platform admin pre-creates the `PriorityClass` objects (the GMC never creates cluster-scoped objects — consistent with the Q121/Q122/Q130 platform-ownership model) and lists their names in the GMC `--allowed-priority-classes` flag; the GMC validating webhook rejects any `priorityClassName` not on the allowlist. An **empty allowlist forbids every reference** (secure default), so out of the box no tenant can set a `PriorityClass` at all. Because `PriorityClass` is global, the platform should create allowlisted classes with `preemptionPolicy: Never` unless cross-tenant preemption is genuinely intended for that tier — see [security-operations.md § Priority classes](../operations/security-operations.md). The dead tenant-settable per-tier `preemptionPolicy` field was removed pre-1.0 (it was never wired to pods and was a tenant-controlled preemption lever the platform must own). **Closed (Q132).** |

---

## 5.3. Security Profiles and the Privileged Opt-In

Worker pod security is defense-in-depth: PSA enforcement at the API
server, AGC-enforced invariants on the PodSpec, and an optional
sandbox runtime layer. The default posture is secure; tenants opt
into looser policy explicitly.

### The three profiles

`ActionsGateway.spec.securityProfile` is one of three values; the GMC
stamps the corresponding label on the tenant namespace.

| Profile | PSA label | Container escape risk | Typical use |
|---|---|---|---|
| `baseline` *(default)* | `pod-security.kubernetes.io/enforce: baseline` | Low — privileged/host namespaces/hostPath/dangerous caps all blocked | Normal CI: builds, tests, integration runs |
| `restricted` | `pod-security.kubernetes.io/enforce: restricted` | Very low — adds runAsNonRoot, drop ALL caps, seccomp RuntimeDefault | High-isolation tenants; compliance workloads |
| `privileged` | `pod-security.kubernetes.io/enforce: privileged` | High — admission imposes no restrictions | DinD, Buildah-without-sandbox, kernel-module workflows |

The default is `baseline`. A tenant must explicitly set
`securityProfile: privileged` on the `ActionsGateway` to allow
privileged worker pods — there is no silent path to it.

### Constraining the GMC's PSA-stamping privilege

Stamping a PSA label on a namespace requires `patch` on `namespaces`,
which is cluster-scoped — RBAC cannot express "only namespaces the GMC
manages". Left unconstrained, a compromised GMC pod could relabel
`kube-system` (or any namespace) to `privileged`. Two controls confine
the grant:

- **A trusted marker label.** A namespace is eligible for GMC
  management only if an administrator has labelled it
  `actions-gateway.github.com/tenant: "true"`. This is a tenant
  onboarding pre-condition (see
  [Tenant Onboarding](../operations/tenant-onboarding.md)). The GMC
  never sets this label itself — doing so would defeat the control.
- **The `namespace-psa-guard` ValidatingAdmissionPolicy.** Scoped to
  the GMC ServiceAccount only, it denies any namespace `UPDATE` unless
  the *existing* namespace already carries the marker (read from
  `oldObject`, which the requester cannot forge), and denies any change
  to a namespace label other than the six `pod-security.kubernetes.io/*`
  keys or to any annotation. It ships in
  `cmd/gmc/config/admission-policy/namespace-psa-guard.yaml` and is
  applied by `make deploy`.

The policy deliberately does **not** ban writing `privileged` outright,
because `securityProfile: privileged` is a supported per-tenant opt-in
and the GMC legitimately stamps it on those tenants' namespaces. The
marker scope confines the blast radius to GMC-managed tenant
namespaces.

### No silent profile downgrades

Separately from the GMC's stamping privilege, the GMC validating
webhook (`ValidateUpdate`) prevents a tenant's profile from being
*silently* weakened. The profiles are ranked
`privileged(0) < baseline(1) < restricted(2)`; on update the webhook
compares the old and new rank:

- An **upgrade** (`baseline → restricted`) — hardening — is always
  allowed. So is a no-op change.
- A **downgrade** (`restricted → baseline`, or anything → `privileged`)
  is **rejected** unless the object carries the annotation
  `actions-gateway.github.com/allow-profile-downgrade: "true"`.

This closes the accidental path without trapping operators. A stray
`kubectl apply` of an older manifest — or one that *drops* the field and
lets it re-default to `baseline` — does not carry the annotation, so it
is refused rather than quietly relaxing isolation (an empty value is
treated as its `baseline` default for the comparison, so dropping the
field cannot sneak a downgrade through). But a *deliberate* relaxation —
for example rolling back a `baseline → restricted` hardening attempt that
turned out to break the tenant's pods at admission — needs only a
two-field edit (set the annotation, set the profile), not a destructive
recreate of the whole `ActionsGateway`. Requiring the explicit annotation
keeps the relaxation auditable while keeping the safe direction
(hardening) cheap and the unsafe direction (relaxing) intentional.

The check is a webhook rather than a CRD CEL `XValidation` rule because
the decision depends on `metadata.annotations`, which a spec-scoped CEL
rule cannot read. (`gitHubAppRef.name` is deliberately left mutable:
changing it is the supported credential-rotation mechanism — see §3.2.)

This is a guard against *accidental/silent* downgrade, not an absolute
boundary: an operator who holds the `allow-profile-downgrade` annotation
write (i.e. edit access to the CR) is trusted to relax the profile on
purpose, and one with direct namespace `patch` rights could edit the PSA
labels regardless. A *compromised GMC* relabelling namespaces is a
separate threat, constrained by the `namespace-psa-guard`
ValidatingAdmissionPolicy above, not by this rule.

### Mixing privileged and non-privileged workloads

PSA enforcement is namespace-scoped: every pod in a namespace is
evaluated against the same profile. A tenant that needs both
privileged and non-privileged workloads deploys **two
`ActionsGateway` CRs in two namespaces** — for example,
`myteam-builds` with `securityProfile: privileged` for DinD jobs and
`myteam-tests` with the default `baseline` for everything else.
Workflows route to the appropriate gateway via `runs-on:` labels
matching RunnerGroups in each.

This is the same separation operators already use to assign different
quotas, priority tiers, and node selectors to different workload
classes — the security profile rides on the existing namespace
boundary rather than introducing a new sub-namespace concept.

If finer granularity (per-RunnerGroup profile within one
`ActionsGateway`) becomes necessary, the path forward is documented
in [Appendix G](appendix-g-future-enhancements.md) as a future
enhancement.

### Pairing `privileged` with a sandbox runtime

Selecting `privileged` removes the API-server-side admission guard
but does not remove the option of sandbox-based isolation. For
tenants who need privileged *semantics* (a real Docker daemon, full
syscall surface) but don't trust the workload code, the recommended
pattern is:

```yaml
apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: builds
  namespace: myteam-builds
spec:
  securityProfile: privileged
  runnerGroups:
    - runnerLabels: [self-hosted, dind]
      podTemplate:
        spec:
          runtimeClassName: kata-containers   # or gvisor
          containers:
            - name: runner
              securityContext:
                privileged: true
```

`runtimeClassName: kata-containers` runs the worker pod inside a
lightweight VM. Privileged-inside-Kata grants the workload full
control of a microVM kernel, not the host kernel — container escape
within the VM has nowhere to escape to. See
[Appendix B](appendix-b-worker-isolation.md) for the full tradeoff
between `runc`, `gvisor`, and `kata-containers`.

This pairing is a tenant-level decision: the platform team can
recommend it via policy and documentation, but cannot enforce it
from the GMC alone (the `runtimeClassName` field lives in the
`PodTemplate`, owned by the tenant).

### Floor invariants apply at every profile

The AGC enforces the following on every worker pod regardless of
profile, by overwriting the merged PodSpec before submission:

- `Spec.HostPID = false`
- `Spec.HostNetwork = false`
- `Spec.HostIPC = false`
- `Spec.AutomountServiceAccountToken = false`
- `Spec.ServiceAccountName = <worker SA>` (no K8s API credentials projected)
- Reserved env vars (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, the
  payload mount path) are stamped by the controller

A tenant who sets `securityProfile: privileged` still cannot enable
host namespace sharing or expose Kubernetes API credentials inside
the worker pod. These invariants are non-negotiable across all
profiles.

### Secure-by-default pod SecurityContext and resources

Floor invariants above are non-negotiable. *On top of them*, the AGC
stamps a secure-by-default `SecurityContext` and resource requests/limits
onto every worker pod. These defaults **gap-fill only** — an explicit
value in the tenant `PodTemplate` always wins, so a tenant can opt out of
any individual default (e.g. `runAsNonRoot: false` for a root-based image)
without escalating to the `privileged` profile.

The hardening scales to the namespace's PSA profile (propagated to the AGC
via the `SECURITY_PROFILE` env var the GMC sets on the AGC Deployment):

| Profile | SecurityContext defaults stamped |
|---|---|
| `baseline` *(default)* | Pod-level `runAsNonRoot: true` + `runAsUser: 1001` + `seccompProfile: RuntimeDefault`. Deliberately **not** `allowPrivilegeEscalation: false` or capability drop — `baseline` PSA permits in-job privilege escalation (`sudo`) and many CI jobs rely on it. |
| `restricted` | The above, plus the per-container PSA-restricted floor: `allowPrivilegeEscalation: false` and `capabilities.drop: [ALL]`. Without these the namespace's PodSecurity admission would reject the pod, so the AGC stamps them to make the profile usable rather than self-blocking. |
| `privileged` | None — this profile exists precisely so DinD / host-capability workloads can opt in via their own `PodTemplate`. |

**Why `runAsUser: 1001` accompanies `runAsNonRoot: true`.** kubelet's
`runAsNonRoot` enforcement can only *prove* a container is non-root against a
**numeric** UID. The default worker image
(`ghcr.io/actions/actions-runner`, and the `cmd/worker` image built from it)
declares its user **by name** — `USER runner` — which kubelet cannot resolve to
a UID at admission. With `runAsNonRoot: true` but no numeric UID, kubelet
rejects the pod outright (`CreateContainerConfigError: container has
runAsNonRoot and image has non-numeric user`), so an unmodified RunnerGroup
would fail *every* job. The AGC therefore gap-fills the runner image's own UID
(1001) whenever non-root is being enforced, letting kubelet verify non-root
without changing which user the runner actually runs as. The gap-fill is skipped
when a tenant sets `runAsNonRoot: false` (a root-based image), so it never
contradicts an explicit opt-out, and an explicit `runAsUser` always wins. (Q115)

`readOnlyRootFilesystem` is **not** defaulted on any profile: the GitHub
Actions runner writes to its work, diagnostics, and home directories at
runtime, so a read-only root would break essentially every job, and it is
not part of the PSA `restricted` floor. Tenants who can run with a
read-only root may set it (plus the writable `emptyDir` mounts the runner
needs) explicitly in their `PodTemplate`.

Resource requests **and** limits default to `500m` CPU / `1Gi` memory on
every profile when the tenant container declares neither. This moves a
worker pod off Best-Effort QoS — the first thing the kubelet evicts under
node pressure, which otherwise burns the eviction-retry budget fast. A
single-container worker pod with the defaults is Guaranteed QoS.

---

← [Operational Flows](04-operational-flows.md) | [Back to index](README.md) | Next: [Implementation Phases →](06-implementation-phases.md)
