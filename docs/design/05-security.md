# 5. Security & Threat Risk Assessment

← [Operational Flows](04-operational-flows.md) | [Back to index](README.md) | Next: [Implementation Phases →](06-implementation-phases.md)

---

The two-tier architecture introduces both stronger isolation guarantees and new attack surfaces. Threats are grouped by which tier they affect.

## 5.1. GMC-Level Threats (Cluster-Scoped)

| Threat Vector | Impact | Mitigation Strategy |
| --- | --- | --- |
| **GMC Privilege Escalation** (Blast Radius: All Tenants) | Critical | The GMC's ClusterRole is tightly scoped: it may read `ActionsGateway` CRs cluster-wide, but write access for Deployments, Roles, RoleBindings, NetworkPolicies, and ResourceQuotas is limited to namespaces where an `ActionsGateway` CR exists. No pod exec or cluster-wide Secret read. **Explicit blast radius if compromised:** a compromised GMC can (a) enumerate every `ActionsGateway` CR in the cluster, learning each tenant's `gitHubAppRef` name and namespace; (b) issue `get` on those specific Secrets to read GitHub App private keys; (c) deploy arbitrary workloads into any namespace that already has an `ActionsGateway` CR (but not into namespaces without one). It CANNOT exec into pods, read Secrets outside the `gitHubAppRef` list, or create new namespaces. GMC pod runs with non-root user, read-only root filesystem, no host mounts, and `seccompProfile: {type: RuntimeDefault}`. Image is digest-pinned. Treat the GMC pod as a Tier-0 workload for monitoring and access. |
| **Admission Webhook Unavailability or Bypass** | Medium | The reserved-namespace validating webhook serves as a safety check, not a security boundary — namespace isolation is enforced by RBAC and NetworkPolicy regardless of webhook state. The webhook uses `failurePolicy: Fail` so requests are rejected when the webhook pod is unhealthy rather than silently bypassed. Serving certificates are managed by cert-manager with automatic rotation; the CA bundle is injected via `caBundle` from the cert-manager-managed Secret. Webhook pod runs `replicas: 2` behind a Service with `podAntiAffinity` to survive single-node loss without stalling tenant onboarding. |
| **Tenant Namespace Escape via Overpermissioned AGC** | Critical | Each AGC's ServiceAccount is bound by a RoleBinding limited to its own namespace. The AGC cannot list or touch resources in any other tenant namespace. |
| **Cross-Tenant GitHub App Credential Leakage** | High | `ActionsGateway` is namespace-scoped, so a tenant's `gitHubAppRef` defaults to their own namespace — another tenant cannot reference it. The GMC mounts credentials into the AGC Pod only; worker pods never have access to the Secret object. Secrets are immutable; rotation creates a new Secret and updates the CR reference, producing a clean Deployment rollout. Old Secrets are not readable by running Pods once the rollout completes. The GMC's ClusterRole grants Secret `get` only on the specific name/namespace from the CR spec, not a blanket read on all Secrets cluster-wide. |
| **`ActionsGateway` CR in Reserved Namespace** | Medium | An admission webhook rejects `ActionsGateway` CRs created in reserved namespaces: the universal `kube-system` and `kube-public`, the GMC's default install namespace `gmc-system`, and the namespace the GMC pod is actually running in (read from the `POD_NAMESPACE` downward-API env var, so custom installs are protected too). Since the CR is namespace-scoped, a tenant can only affect their own namespace — the risk is self-harm or collision with operator-owned resources, not cross-tenant impact. |

---

## 5.2. AGC & Proxy-Level Threats (Namespace-Scoped)

| Threat Vector | Impact | Mitigation Strategy |
| --- | --- | --- |
| **Host Namespace Escape via Malicious Workflow** (Container Breakout) | Critical | Enforced in three layers. (1) The AGC unconditionally sets `hostPID: false`, `hostNetwork: false`, `hostIPC: false`, and `automountServiceAccountToken: false` on every worker pod, overwriting tenant `PodTemplate` values at pod-creation time. (2) The GMC stamps `pod-security.kubernetes.io/enforce` on the tenant namespace at provisioning time, with the level chosen by `ActionsGateway.spec.securityProfile` — see [§5.3](#53-security-profiles-and-the-privileged-opt-in). The default `baseline` blocks privileged containers, hostPath, dangerous capabilities, and host namespaces via the in-tree PodSecurity admission plugin; no external policy engine is required. (3) Sandboxed container runtimes (Kata Containers, gVisor) are supported via `runtimeClassName` in the `PodTemplate` — optional but strongly recommended for tenants who select the `privileged` profile. See [Appendix B](appendix-b-worker-isolation.md) for tradeoffs. |
| **Supply-Chain Compromise of Worker Image** | High | `WorkerImage` SHOULD reference an immutable digest, not a floating tag (see [§3.1](03-api-contracts.md#31-kubernetes-crd-schemas)). Digest pinning eliminates the "update the tag, get a different binary" attack class. Operators are expected to restrict the set of permitted registries via cluster admission policy (e.g. Kyverno, OPA Gatekeeper) — the GMC does not enforce this itself because registry policy is a cluster-wide concern. Image scanning (Trivy, Snyk, etc.) of the worker image in CI is recommended but out of scope for the gateway. `imagePullPolicy: IfNotPresent` (digest) or `Always` (tag) ensures the kubelet does not serve a stale, possibly tampered local copy. |
| **Cross-Job Code Contamination** | High | Enforce absolute 1-Job-Per-Pod isolation. Avoid reusing volumes or host paths between worker pods. Use ephemeral, `emptyDir` volumes for workspace storage. |
| **AGC Token Compromise** | High | The AGC never saves plaintext keys to disk. GitHub App private keys are mounted as read-only volumes with restrictive file permissions (0400). |
| **Eviction-Retry API Misuse** | Medium | The AGC calls `POST /repos/{owner}/{repo}/actions/runs/{run_id}/rerun-failed-jobs` using the tenant's installation access token when a worker pod is evicted. The blast radius is bounded: the installation token is scoped to the GitHub App's installation on a specific organization or repository, so the AGC cannot re-run jobs belonging to other tenants or organizations. The `run_id` is extracted from the job payload delivered by GitHub's broker — the AGC cannot fabricate or substitute a run ID for a run it did not acquire. To prevent abuse of the retry path (e.g. a compromised AGC looping re-runs), `maxEvictionRetries` caps the number of automatic retries per job and is enforced before the API call is made. Operators should monitor `actions_gateway_eviction_retries_exhausted_total` to detect abnormal eviction patterns. |
| **Proxy as Traffic Interception Point** | Medium | The proxy only handles CONNECT tunneling and does not terminate TLS. It cannot inspect or modify the encrypted payload between the AGC/worker and GitHub. Proxy pods run with a read-only root filesystem and no elevated capabilities. |
| **Egress IP Change Mid-Session** | Low–Unknown | GitHub's broker protocol is token-based, not IP-bound. Session IDs and bearer tokens carry no IP affinity, so rotating across proxy pods mid-job is expected to work. The Twirp log stream is naturally sticky (long-lived HTTP/2 connection stays on one proxy pod once open). Impact is unknown because GitHub's abuse detection heuristics are undocumented. **Early mitigation: the [Milestone 1](06-implementation-phases.md#milestone-1-wire-protocol-probe-days-14) wire protocol probe explicitly tests broker API calls routed through a multi-pod proxy pool to confirm GitHub does not reject or flag IP variance across `CreateSession → GetMessage → AcquireJob`.** If the probe surfaces a problem, `ClientIP` session affinity on the proxy Service is the low-effort fallback; explicit per-goroutine proxy assignment is the higher-fidelity option if needed. |
| **Proxy Pool Exhaustion** (DoS via proxy saturation) | Medium | HPA `minReplicas` ensures a floor of available capacity. The `PodDisruptionBudget` prevents draining all replicas simultaneously. `ResourceQuota` caps proxy pod count so a misconfigured `maxReplicas` cannot consume cluster CPU. |
| **Denial of Service via Resource Exhaustion** | Medium | The GMC enforces a `ResourceQuota` per tenant namespace. CPU/Memory limits are defined in the `RunnerGroup` CRD spec. Rogue workflows cannot exceed the tenant quota. |

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

---

← [Operational Flows](04-operational-flows.md) | [Back to index](README.md) | Next: [Implementation Phases →](06-implementation-phases.md)
