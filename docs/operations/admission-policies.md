# Admission policies: Kyverno / Gatekeeper compatibility

**Audience:** Platform engineer, Security
**Goal:** know — *before* you install — whether your cluster's admission
policies will let github-actions-gateway (GAG) pods schedule, and apply
complementary sample policies that lock GAG's own hardened posture in place.

Many clusters run a policy engine — [Kyverno](https://kyverno.io) or
[OPA Gatekeeper](https://open-policy-agent.github.io/gatekeeper/) — that
**rejects pods at admission** when they violate a cluster rule: require
`runAsNonRoot`, drop all Linux capabilities, block the `:latest` image tag,
allow only certain registries, require resource limits, require a seccomp
profile. If one of those rules rejects a GAG worker, egress-proxy, Actions
Gateway Controller (AGC), or Gateway Manager Controller (GMC) pod, the pod
**never schedules** — a confusing, silent failure that surfaces only as "no
runners ever come online" or a `Deployment` stuck at zero ready replicas.

This page does two things:

1. A [**compatibility matrix**](#compatibility-matrix) — for each common policy
   class, whether each GAG pod already complies (and why), or what you must
   allow. Every row is grounded in the real pod specs GAG produces.
2. [**Sample policies**](#sample-policies) you can apply to *enforce* GAG's own
   security posture, plus *exception* snippets for where a strict cluster policy
   would otherwise block GAG.

> GAG uses in-tree [Pod Security Admission](https://kubernetes.io/docs/concepts/security/pod-security-admission/)
> (PSA) as its **floor**, not as a replacement for your policy engine — the GMC
> stamps a `pod-security.kubernetes.io/enforce` level on every tenant namespace
> ([§5.3 of the security design](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in)).
> A Kyverno/Gatekeeper layer on top is fully supported and encouraged; this page
> is how to make the two layers agree.

## GAG pod classes

The matrix distinguishes seven pod classes because their security posture
differs by design:

| Pod class | Created by | Namespace | Lifecycle |
|---|---|---|---|
| **Worker — `baseline`** *(default)* | AGC | tenant | Ephemeral, per-job, run-to-completion |
| **Worker — `restricted`** | AGC | tenant | As above, hardened tenant opt-in |
| **Worker — `privileged`** | AGC | tenant | As above, escape-hatch tenant opt-in (Docker-in-Docker (DinD), host caps) |
| **Egress proxy** | GMC | tenant | Long-running per-tenant `Deployment` |
| **AGC (v1alpha1)** | GMC | tenant | Long-running per-tenant control plane |
| **AGC (v2alpha1)** | GMC | tenant | As above; adds resource defaults |
| **GMC manager** | Helm chart | install ns | Long-running cluster control plane |

The worker profile is the tenant's `ActionsGateway.spec.securityProfile`
(`baseline` default, `restricted`, or `privileged`); the GMC mirrors it onto the
worker pods and the namespace PSA level. The proxy, AGC, and GMC pods are
**uniformly hardened** and not tenant-tunable.

## Compatibility matrix

Legend: ✅ already complies · ⚠️ complies by default but a tenant override or
profile can change it · ❌ does **not** comply — you must allow it (exempt the
pod, or pick a stricter worker profile).

| Policy class | Worker `baseline` | Worker `restricted` | Worker `privileged` | Egress proxy | AGC v1 | AGC v2 | GMC |
|---|:--:|:--:|:--:|:--:|:--:|:--:|:--:|
| **`runAsNonRoot: true`** | ✅ | ✅ | ❌ | ✅ | ✅ | ✅ | ✅ |
| **`seccompProfile: RuntimeDefault`** | ✅ | ✅ | ❌ | ✅ | ✅ | ✅ | ✅ |
| **Drop ALL capabilities** | ❌ | ✅ | ❌ | ✅ | ✅ | ✅ | ✅ |
| **`allowPrivilegeEscalation: false`** | ❌ | ✅ | ❌ | ✅ | ✅ | ✅ | ✅ |
| **`readOnlyRootFilesystem: true`** | ❌ | ❌ | ❌ | ✅ | ✅ | ✅ | ✅ |
| **No privileged containers** | ✅ | ✅ | ❌ | ✅ | ✅ | ✅ | ✅ |
| **No host namespaces** (`hostPID/Network/IPC`) | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Block `:latest` / require digest pin** | ⚠️ | ⚠️ | ⚠️ | ✅ | ✅ | ✅ | ✅ |
| **Registry allowlist** | ⚠️ | ⚠️ | ⚠️ | ⚠️ | ⚠️ | ⚠️ | ⚠️ |
| **Require CPU/memory requests + limits** | ✅ | ✅ | ✅ | ✅ | ❌ | ✅ | ✅ |

### Row notes (the *why*, grounded in the pod specs)

**`runAsNonRoot: true`** — On `baseline`/`restricted` the AGC gap-fills
pod-level `runAsNonRoot: true` *and* a numeric `runAsUser: 1001`
([`provisioner.go`](../../cmd/agc/internal/provisioner/provisioner.go)
`applySecurityDefaults`). The numeric UID matters: kubelet cannot verify
`runAsNonRoot` against the runner image's non-numeric `USER runner`, so without
it the pod is rejected with `CreateContainerConfigError` (Q115). The
`privileged` profile stamps **no** securityContext defaults — that is its whole
purpose (DinD, root-requiring builds) — so a `runAsNonRoot` policy will reject
it; that is correct, and those tenants need an explicit exception. Proxy, AGC,
and GMC containers set `runAsNonRoot: true` directly.

**`seccompProfile: RuntimeDefault`** — Set on every `baseline`/`restricted`
worker (pod-level) and on every proxy/AGC/GMC container. `privileged` workers
get no default.

**Drop ALL capabilities** — Here is the most common surprise. The `baseline`
worker profile **deliberately does not** drop capabilities or set
`allowPrivilegeEscalation: false`, because baseline PSA permits in-job
privilege escalation (`sudo`) and a large fraction of real CI jobs rely on it.
A cluster policy that requires `drop: [ALL]` on *all* pods will therefore
**reject default workers**. Your options, in order of preference:

- Tell tenants to set `securityProfile: restricted` — that profile gap-fills
  `allowPrivilegeEscalation: false` + `drop: [ALL]` on every worker container,
  satisfying the policy with no exception needed.
- Or scope the cluster policy to exclude tenant namespaces (see the
  [exception samples](#sample-policies)) so `baseline` keeps working.

`restricted`, proxy, AGC, and GMC pods already drop all capabilities.

**`allowPrivilegeEscalation: false`** — Same story as capabilities: only
`restricted` (and the proxy/AGC/GMC pods) set it. `baseline` leaves it unset on
purpose.

**`readOnlyRootFilesystem: true`** — **No** worker profile sets this, including
`restricted`. The runner writes to its working tree (`_work`), tool cache, and
temp dirs on the root filesystem; a read-only root would break virtually every
job. If your cluster requires `readOnlyRootFilesystem`, you **must** exempt
worker pods. The proxy, AGC, and GMC containers all run with
`readOnlyRootFilesystem: true`.

**No privileged containers / no host namespaces** — The AGC unconditionally
forces `hostPID/hostNetwork/hostIPC: false` and `automountServiceAccountToken:
false` on every worker pod (all three profiles), overwriting any tenant
`PodTemplate` value. `baseline`/`restricted` also forbid privileged containers
via PSA. Only the `privileged` profile permits a privileged container — by
design, and gated behind an admission webhook that requires an explicit
per-tenant opt-in
([§5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in)).

**Block `:latest` / require digest pin** — All GAG-managed images are pinned by
`@sha256:` digest: the GMC, AGC, and proxy images are digest-pinned and the
Helm chart **refuses to render** without a digest (`validateImageDigest`); the
default worker image is also digest-pinned
([`names.go`](../../cmd/agc/names/names.go)). The ⚠️ is only because a tenant
*may* override `spec.workerImage` (or a `PodTemplate` container image) with a
floating tag — a tenant choice, not a GAG default. A "require digest" policy
scoped to tenant namespaces is a good way to force tenants to pin too.

**Registry allowlist** — ⚠️ for every class because you must add GAG's
registries to your allowlist:

- Control-plane images (`GMC`, `AGC`, egress proxy): `ghcr.io/actions-gateway/*`.
- Default worker image: `ghcr.io/actions/actions-runner`.
- Any registry your tenants set via `spec.workerImage` / `PodTemplate`.

**Require CPU/memory requests + limits** — Workers are gap-filled with
`500m` CPU / `1Gi` memory as *both* requests and limits when a container sets
neither. The proxy sets requests+limits. **AGC v1alpha1 stamps no resources** —
a "require limits" policy will reject AGC v1 pods; either move the tenant to a
v2alpha1 `ActionsGateway` (which gap-fills `500m`/`2Gi` requests, `2`/`4Gi`
limits) or exempt AGC pods. The GMC's resources come from the chart's
`resources` value, which ships with a sane default (`10m`/`64Mi` request,
`500m`/`128Mi` limit).

## Sample policies

Ready-to-apply samples live in
[`examples/policies/`](examples/policies/). Two complementary kinds, for both
engines:

| File | Engine | Purpose |
|---|---|---|
| [`kyverno/enforce-gag-worker-hardening.yaml`](examples/policies/kyverno/enforce-gag-worker-hardening.yaml) | Kyverno | **Enforce** GAG's posture: in tenant namespaces require `runAsNonRoot`, seccomp `RuntimeDefault`, no host namespaces, no privileged container, and digest-pinned images. |
| [`kyverno/policyexception-gag.yaml`](examples/policies/kyverno/policyexception-gag.yaml) | Kyverno | **Allow** GAG where a strict cluster policy would block it (read-only-rootfs, drop-ALL-caps) — scoped to tenant namespaces and the GMC install namespace. |
| [`gatekeeper/enforce-gag-worker-hardening.yaml`](examples/policies/gatekeeper/enforce-gag-worker-hardening.yaml) | Gatekeeper | Same **enforce** intent as the Kyverno policy, as a `ConstraintTemplate` + `Constraint`. |
| [`gatekeeper/exclude-gag-namespaces.yaml`](examples/policies/gatekeeper/exclude-gag-namespaces.yaml) | Gatekeeper | **Exclude** GAG namespaces from strict cluster `Constraint`s, two ways: `excludedNamespaces` and a namespace-marker `labelSelector`. |

All samples key off two stable signals so they keep working as tenants come and
go:

- The **tenant-namespace marker** `actions-gateway.github.com/tenant: "true"`,
  which the GMC requires on every namespace it provisions into (worker, proxy,
  and AGC pods all live in marked namespaces).
- The **GMC install namespace**, where only the GMC manager pod runs.

### Apply the enforce policies

```bash
# Kyverno (cluster-scoped ClusterPolicy)
kubectl apply -f docs/operations/examples/policies/kyverno/enforce-gag-worker-hardening.yaml

# Gatekeeper (ConstraintTemplate must exist before its Constraint)
kubectl apply -f docs/operations/examples/policies/gatekeeper/enforce-gag-worker-hardening.yaml
```

Start with `validationFailureAction: Audit` (Kyverno) or
`enforcementAction: dryrun` (Gatekeeper) — both samples ship in that mode — and
promote to enforce once you confirm no false positives against your real
workloads.

### Apply the exceptions

If you already run a strict cluster baseline (e.g. the Kyverno
`require-ro-rootfs` / `disallow-capabilities-strict` policies, or a Gatekeeper
PodSecurityPolicy (PSP)-style constraint), apply the matching exception so GAG pods are not caught:

```bash
# Kyverno PolicyException — requires Kyverno's PolicyExceptions feature enabled.
kubectl apply -f docs/operations/examples/policies/kyverno/policyexception-gag.yaml

# Gatekeeper — exclude GAG namespaces from your strict Constraints.
kubectl apply -f docs/operations/examples/policies/gatekeeper/exclude-gag-namespaces.yaml
```

Read the comments in each file: the exceptions are deliberately **narrow** (only
the properties GAG legitimately cannot satisfy, only in GAG namespaces) so they
do not widen your cluster posture more than necessary.

## See also

- [Security design §5.3 — security profiles and the privileged opt-in](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in)
- [Security operations — posture scanning, abuse response](security-operations.md)
- [Install — pin images by digest](install.md)
- [Tenant onboarding — choosing a security profile](tenant-onboarding.md)
