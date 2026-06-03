# Q56 — Gate GMC cluster-wide `namespaces: patch` (k8s audit §B B2)

**Status:** ✅ Done — shipped via the `namespace-psa-guard` VAP + marker-label contract; covered by `TestGMC_NamespacePSAGuard_EnforcesMarkerAndFieldScope`.
**Queue:** [Q56](../STATUS.md) · **Finding:** [k8s-best-practices.md §B B2](k8s-best-practices.md#b-rbac--cluster-wide-privilege-)

## Goal

A compromised GMC pod must not be able to relabel a non-tenant namespace (notably
`kube-system`) Pod Security Admission (PSA) profile. Today the GMC holds cluster-wide
`namespaces: patch/update` so it can stamp PSA labels on tenant namespaces — but that
same grant lets a compromised pod relabel `kube-system` to `privileged`.

## Approach (chosen)

A `ValidatingAdmissionPolicy` (VAP) scoped to the GMC ServiceAccount, gating namespace
`UPDATE`s. VAP is in-API-server (no webhook server, cert, or availability risk) and is
GA on the project's envtest k8s version (1.35).

RBAC stays cluster-wide: RBAC cannot express "namespaces carrying label X", so the VAP
— not the Role — is the gate. This is exactly the fix the finding describes.

### Two constraints

1. **Marker-label scope (primary).** Deny a namespace `UPDATE` by the GMC SA unless the
   *existing* object (`oldObject`) carries `actions-gateway.github.com/tenant: "true"`.
   The marker is read from `oldObject`, which a compromised GMC cannot forge in the
   request, and is applied by a *trusted* actor (the admin, at namespace creation) — the
   GMC must never be able to set it, or a compromised GMC could mark `kube-system` then
   patch it. `kube-system` and every other non-tenant namespace lack the marker → the
   GMC SA cannot touch them. This alone fully closes B2.

2. **PSA-only field guard (value guard, defense-in-depth).** Deny if the GMC's update
   changes any namespace label *other than* the six `pod-security.kubernetes.io/*` keys,
   or changes any annotation. The GMC's legitimate SSA apply (field manager
   `actionsgateway-controller-psa`) only ever declares those six labels, so this is
   non-breaking, but it stops a compromised GMC from doing label/annotation mischief on
   the tenant namespaces it *can* reach (e.g. stripping a NetworkPolicy selector label).

### Why no blanket "GMC may never write `privileged`"

`securityProfile: privileged` is a supported onboarding path (DinD / kernel-module
tenants — see [05-security.md §5.3](../design/05-security.md), `securityProfile` enum
`baseline;restricted;privileged`). The GMC legitimately stamps PSA `privileged` on such
a tenant's namespace, so a value ban on `privileged` would break supported tenants. The
marker scope already bounds the blast radius to GMC-managed tenant namespaces.

### Residual (documented, out of scope for Q56)

A compromised GMC can still flip the PSA profile *within a tenant namespace it already
manages* (e.g. `baseline` → `privileged` on that one tenant). That blast radius is
inherent — the GMC's whole job is managing that tenant's PSA. The complementary control
is CEL immutability / downgrade-audit on `securityProfile` itself, tracked as
[Q33](../STATUS.md) (k8s audit §D D5). Q56 is strictly about non-tenant namespaces.

## Deliverables

### Manifests
- `cmd/gmc/config/admission-policy/policy.yaml` — `ValidatingAdmissionPolicy` +
  `ValidatingAdmissionPolicyBinding`.
- `cmd/gmc/config/admission-policy/kustomization.yaml`.
- Wire `- ../admission-policy` into `cmd/gmc/config/default/kustomization.yaml`
  (uncommented = secure by default).

### VAP shape
- `matchConstraints`: `""/v1 namespaces`, `operations: [UPDATE]` (GMC has no `create` on
  namespaces — verbs are get/list/patch/update/watch — so CREATE need not be matched).
- `matchConditions`: `request.userInfo.username == 'system:serviceaccount:gmc-system:gmc-controller-manager'`
  (the default install identity; documented as the one string to change if the install
  namespace/name is customized).
- `variables` + `validations` implementing constraints 1 and 2.
- Binding `validationActions: [Deny]` (secure default). Migration path for existing
  clusters documented (label namespaces first, or temporarily flip to `Audit`).

### Code
- `applyNamespacePSA`: when the SSA apply is rejected with `IsForbidden` (VAP denial,
  typically a new tenant whose namespace was not labeled), emit a `Warning` event
  (`NamespaceMarkerMissing`) guiding the operator to apply the marker label, instead of
  surfacing only an opaque reconcile error.

### Tests (envtest, `cmd/gmc/internal/controller`)
- Install the VAP + binding objects; using an **impersonating** client
  (`rest.Config.Impersonate` = the GMC SA), assert via `Eventually` (VAP enforcement is
  not instantaneous after creation):
  - UPDATE of an **unmarked** namespace → Forbidden.
  - UPDATE of a **marked** namespace setting PSA labels → allowed.
  - UPDATE of a marked namespace changing a **non-PSA** label → Forbidden.
  - (Control) a cluster-admin client is unaffected by the policy.

### Docs
- `docs/design/05-security.md` — new control + the marker-label contract.
- `docs/operations/tenant-onboarding.md` — add the marker label to the namespace
  pre-condition and the create-namespace step.
- `docs/getting-started.md` — add the label to the namespace it creates.
- `docs/operations/upgrade.md` — migration: label existing tenant namespaces before
  upgrading (or run the binding in `Audit` during transition).
- `docs/operations/troubleshooting.md` — runbook: "PSA reconcile denied / namespace
  marker missing".
- `docs/design/02-architecture.md` — one line in the RBAC/security prose.
- `docs/plan/k8s-best-practices.md` — mark B2 ✅ with the implemented approach.
- `docs/STATUS.md` — remove Q56 row (isolated commit).
