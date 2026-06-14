# Platform-owned ResourceQuota — remove tenant-authored quota from the CR

**Decision (2026-06-13):** the platform admin owns the `Namespace`,
`ResourceQuota`, and `LimitRange`; GAG operates *within* them and never creates
or mutates them. Remove `spec.namespaceQuota` from the `ActionsGateway` CRD and
drop the GMC's `resourcequotas`/`limitranges` write RBAC. A breaking CRD change,
done **pre-1.0 while it is free** (post-1.0 it would need a conversion webhook —
see deferred [Q74](../STATUS.md)).

Tracked as STATUS Queue Q130. **Implemented (2026-06-14):** the
CRD/RBAC/controller/docs edits landed; see the Status section below.

## Why

The website rework surfaced that the implementation contradicts the core value
prop. The site (correctly) says "the platform team caps each tenant," but today:

- `ActionsGateway.spec.namespaceQuota` is **tenant-authored** — a tenant can set
  `requests.cpu: "10000"` in their own CR, so it is not a platform-enforced cap.
  Quotas are a platform resource-allocation control; the tenant must not own them.
- The GMC **creates** the `ResourceQuota` from the spec, which **conflicts with
  existing investments** — platform teams already manage namespaces + quotas via
  GitOps or a tenant operator (Capsule, HNC, vCluster, kiosk). GAG either fights
  them or silently overrides their allocation.
- Owning quotas forces broad GMC RBAC. Dropping it is **least privilege** and
  shrinks the cluster-wide-write surface flagged in [Q122](../STATUS.md).

The corrected model: the platform provisions the namespace + quota (+ optional
`LimitRange`) — it already creates and labels the namespace per
[getting-started](../getting-started.md) — and GAG provisions pods/deployments
*within* them, reading remaining quota but never setting it.

## Scope

### API / CRD
- Remove `NamespaceQuota corev1.ResourceList` from `ActionsGatewaySpec`
  ([actionsgateway_types.go:133](../../cmd/gmc/api/v1alpha1/actionsgateway_types.go:133));
  regenerate with `make manifests generate`.
- Confirm + document that structural-schema pruning drops the field from
  existing CRs silently (no apply rejection) pre-1.0.

### Controller / builder
- Remove `ResourceQuota` construction in
  [builder.go](../../cmd/gmc/internal/controller/builder.go) and its
  reconcile/teardown in the controller; drop it from the owned-resource set.
- **Keep** `maxQuotaRetries` / `quotaRetryDelay` — those are the AGC reacting to
  a *full* quota (operating within it), not owning it.

### RBAC
- Remove `resourcequotas` (and `limitranges` if present) verbs from
  [role.yaml:27](../../cmd/gmc/config/rbac/role.yaml:27) and
  [charts rbac.yaml:33](../../charts/actions-gateway/templates/rbac.yaml:33);
  `make manifests`.
- Partially subsumes [Q122](../STATUS.md): its proposed quota-write *confinement*
  becomes moot once we drop the write entirely.

### Docs (operator-facing + design + website)
- `getting-started.md` step 2: platform creates the namespace **and** a
  `ResourceQuota` (+ optional `LimitRange`); remove `namespaceQuota` from the CR
  example in step 4.
- `02-architecture.md` (Tenant Provisioner, ~line 69): GAG operates within a
  platform-provided quota; no longer derives/creates it.
- `operations/tenant-onboarding.md`: platform provisions namespace + quota;
  tenant supplies only the CR.
- `05-security.md` / `operations/security-operations.md`: reduced GMC RBAC; ties
  [Q121](../STATUS.md)/[Q122](../STATUS.md).
- `appendix-a` / `appendix-e` capacity planning: `priorityTiers` / `maxWorkers`
  soft ceilings pair with the platform-owned quota.
- **Website** CR examples (`index.md` is fine; `why-gag.md` shows the CR) drop
  `namespaceQuota`. This also removes the live inconsistency where the site
  frames "platform caps the tenant" beside a tenant-authored quota — tracked
  under [Q129](../STATUS.md), fix alongside this change.

### Migration (pre-1.0)
- Operators must ensure a platform-managed `ResourceQuota` exists in each tenant
  namespace **before** upgrading; GAG stops managing it.
- A previously GAG-created `ResourceQuota` (ownerRef → the `ActionsGateway`) is
  left orphaned on upgrade. Document adopt/replace with a platform-managed one
  (strip the ownerRef, or delete + recreate under platform management).

## Open question — Roles / RoleBindings ownership

Same principle, one layer over: should GAG keep minting the AGC's
namespace-scoped `Role`/`RoleBinding`, or should the platform grant them? GAG
self-wiring is convenient; platform-granted is stricter "don't touch
platform-owned in-namespace RBAC." Captured here; may split into its own item
after the quota change lands.

## Status

**Implemented (2026-06-14, Q130).** Delivered:

- **API/CRD:** removed `NamespaceQuota` from `ActionsGatewaySpec`
  ([actionsgateway_types.go](../../cmd/gmc/api/v1alpha1/actionsgateway_types.go));
  regenerated deepcopy, the CRD bases, the GMC-bundled chart CRD copy, and RBAC.
  Structural-schema pruning silently drops the now-unknown field from existing CRs
  (no apply rejection) pre-1.0.
- **Controller/builder:** removed `buildResourceQuota`, the reconcile apply path,
  the teardown delete, and the `applyResourceQuota` helper. `maxQuotaRetries` /
  `quotaRetryDelay` (AGC reacting to a *full* quota) are kept — those are operating
  *within* a quota, not owning it.
- **RBAC:** dropped the `resourcequotas` verbs from
  [role.yaml](../../cmd/gmc/config/rbac/role.yaml) and the chart
  [rbac.yaml](../../charts/actions-gateway/templates/rbac.yaml). No `limitranges`
  grant existed. Partially subsumes [Q122](security-audit-2026-06.md#q122--gmc-workload-writes-are-cluster-wide-docs-claim-confinement-high).
- **Tests:** `TestGMC_TenantProvisioning_NoResourceQuotaCreated` (envtest) asserts the
  GMC creates no quota and leaves a pre-existing platform quota untouched; the
  `TestBuildResourceQuota_PassesThrough` unit test was removed.
- **Docs:** getting-started, tenant-onboarding, runbook, troubleshooting,
  security-operations, 01/02/03/04/05/07 design docs, appendix-a/e, why-gag, README,
  and the breaking-change migration note in
  [upgrade.md](../operations/upgrade.md).

The open question below (Role/RoleBinding ownership) is unaddressed and may split
into its own item.
