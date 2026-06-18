# Appendix H — v2 API Decomposition (Proposal)

← [Optional Future Enhancements](appendix-g-future-enhancements.md) | [Back to index](README.md)

---

> **Status: proposal, not committed.** This appendix describes a proposed
> `v2alpha1` API shape that would replace the current monolithic
> `ActionsGateway` + `RunnerGroup` model. It is recorded here for review.
> Nothing in the shipped `v1alpha1` API changes until this is accepted and
> scheduled. Adopting it is a multi-session effort with a deliberate cutover
> (see [§H.11](#h11-migration-v2-tool-assisted)).

---

## H.1. Why decompose

Three independent pressures all trace back to the same root cause — the
current API is a **monolith that aggregates large pod templates, couples the
egress proxy to the tenant 1:1, and assumes one gateway per namespace.**

1. **Pod templates are growing past comfortable object sizes.** Docker-in-Docker
   and sysbox runner images need large `PodTemplateSpec`s (init containers,
   sidecars, volumes, security context). Today the template is embedded in
   `RunnerGroupSpec`, and a bootstrap copy can also be embedded inline in
   `ActionsGateway.spec.runnerGroups[]`. Several runner groups, each with a fat
   template, marshalled into one CR is the failure mode that approaches etcd's
   ~1.5 MB per-object limit. The limit is **per object**, so the fix is to stop
   the *aggregation* and enable *reuse*, not to shrink fields.

2. **Multiple runner sets should be able to share one egress proxy.** The proxy
   is currently created per `ActionsGateway` and is not independently
   addressable. There is no way to point several runner sets at one proxy pool,
   or to run a single platform-operated pool.

3. **Tenants want everything in one namespace and the freedom to rebalance.**
   The current rule is one `ActionsGateway` per namespace. A tenant running
   several GitHub orgs, or wanting to shuffle `maxWorkers`/`priorityTiers` across
   runner sets against a single namespace `ResourceQuota`, cannot do so without
   spreading across namespaces.

## H.2. Design principles

- **Split *data* objects from *controller* objects.** Two kinds are reconciled
  into running infrastructure (verbs); two are reusable data referenced by name
  (nouns). The large pod template lives only in a data object, so it never
  co-bloats anything that gets aggregated.
- **Reference, don't embed; reference, don't own.** Shared objects are named by
  reference. Referrers never set owner references on referents (that would
  cascade-delete a shared object when any one referrer goes away).
- **GitOps-friendly: no apply-ordering requirements.** Referential integrity is
  a runtime condition, not an admission gate. Applying a whole directory at once
  must converge regardless of object order.
- **Secure by default, optional by layering.** The egress proxy becomes optional
  to lower onboarding cost, but the egress *restriction* it sat on top of stays
  mandatory. Cross-namespace sharing requires explicit provider consent.

## H.3. The CRD set

Two controller kinds (`ActionsGateway`, `RunnerSet`) and two data kinds
(`RunnerTemplate`/`ClusterRunnerTemplate`, `EgressProxy`).

```
                         ┌──────────────────────┐
        GitHub binding   │   ActionsGateway      │  (1..N per namespace)
        + AGC ctrl plane │   - gitHubAppRef      │
                         │   - gitHubURL         │
                         │   - securityProfile   │
                         │   - defaultProxyRef? ──┼──┐ (optional)
                         └──────────┬───────────┘  │
                                    │ gatewayRef    │
                         ┌──────────┴───────────┐  │
   scheduling / quota →  │      RunnerSet        │  │
   (small object)        │   - gatewayRef        │  │
                         │   - templateRef ──────┼──┼──► RunnerTemplate /
                         │   - proxyRef? ────────┼──┤    ClusterRunnerTemplate
                         │   - runnerLabels      │  │     - podTemplate (the big field)
                         │   - maxListeners      │  │     - workerImage
                         │   - maxWorkers        │  │
                         │   - priorityTiers     │  │
                         │   - lifecycle tunables│  │
                         └──────────────────────┘  │
                                                    ▼ (optional)
                                          ┌──────────────────┐
                         egress pool →    │   EgressProxy     │  shared by many
                         (standalone)     │   - min/maxReplicas│ RunnerSets
                                          │   - resources      │
                                          │   - noProxyCIDRs   │
                                          │   - managedNetPol   │
                                          │   - sharing?        │
                                          └──────────────────┘
```

This mirrors the Gateway API pattern (`GatewayClass` → `Gateway` → route
attachment by reference) and ARC's split of scheduling (`AutoscalingRunnerSet`)
from pod shape, rather than introducing a novel structure.

## H.4. Spec sketches

```go
// ActionsGateway — GitHub identity + AGC control plane only.
// Now permitted 1..N per namespace.
type ActionsGatewaySpec struct {
    GitHubAppRef    SecretReference // unchanged
    GitHubURL       string          // unchanged
    SecurityProfile string          // unchanged; still PSA-labels the namespace
    Tracing         TracingConfig   // unchanged

    // DefaultProxyRef names an EgressProxy used for AGC control-plane egress and
    // inherited by RunnerSets that do not set their own proxyRef. Optional:
    // unset means the control plane egresses directly (subject to NetworkPolicy).
    // Same-namespace unless the target EgressProxy grants cross-namespace use.
    DefaultProxyRef *ObjectRef `json:"defaultProxyRef,omitempty"`

    // REMOVED vs v1alpha1: Proxy ProxyConfig         → standalone EgressProxy
    // REMOVED vs v1alpha1: RunnerGroups []RunnerGroupSpec → explicit RunnerSet objects
}

// EgressProxy — standalone, optionally shared proxy pool (was ActionsGateway.spec.proxy).
type EgressProxySpec struct {
    MinReplicas, MaxReplicas       *int32
    TargetCPUUtilizationPercentage *int32
    Resources                      corev1.ResourceRequirements
    NoProxyCIDRs                   []string
    ManagedNetworkPolicy           *bool

    // Sharing controls cross-namespace reference. nil ⇒ same-namespace only
    // (default, secure). Consent lives on the provider (proxy owner) side.
    Sharing *ProxySharing `json:"sharing,omitempty"`
}

type ProxySharing struct {
    // AllowedNamespaces lists namespaces permitted to reference this proxy.
    // Alternatively a NamespaceSelector may be offered.
    AllowedNamespaces []string `json:"allowedNamespaces,omitempty"`
}

// RunnerTemplate (namespaced) and ClusterRunnerTemplate (cluster-scoped, identical
// spec) — the only object permitted to be large. Isolated so it never co-bloats
// a controller object, and reusable across many RunnerSets. The cluster-scoped
// variant lets the platform own golden privileged templates (DinD/sysbox) once.
type RunnerTemplateSpec struct {
    PodTemplate corev1.PodTemplateSpec // the big field
    WorkerImage string

    // Reserved-pod-field rejection (serviceAccountName, hostPID/hostNetwork/hostIPC,
    // automountServiceAccountToken, proxy env vars) moves to THIS object's webhook.
}

// RunnerSet — small scheduling/quota binder (was RunnerGroup; podTemplate removed,
// references added). See H.6 for the rename rationale.
type RunnerSetSpec struct {
    GatewayRef  ObjectRef  // which GitHub connection (was implicit via namespace)
    TemplateRef ObjectRef  // RunnerTemplate | ClusterRunnerTemplate (replaces inline PodTemplate)
    ProxyRef    *ObjectRef // EgressProxy; nil ⇒ gateway.defaultProxyRef; both nil ⇒ direct egress

    RunnerLabels  []string
    MaxListeners  int32
    MaxWorkers    *int32
    PriorityTiers []PriorityTier
    // lifecycle tunables (eviction/quota retries, TTLs, deadlines) — unchanged from RunnerGroup
}
```

## H.5. How each pressure is resolved

- **Object size.** The large `PodTemplateSpec` lives only in
  `RunnerTemplate`/`ClusterRunnerTemplate`. `ActionsGateway` and `RunnerSet`
  become small, fixed-size objects, and nothing embeds a template anymore. Reuse
  means one 40 KB sysbox template exists once and is referenced N times instead
  of copied into N runner sets; a `ClusterRunnerTemplate` lets the platform own
  it once cluster-wide.
- **Shared egress.** Any number of `RunnerSet`s point `proxyRef` at one
  `EgressProxy`; setting `defaultProxyRef` on the gateway makes every runner set
  inherit it — one tenant proxy, many runner sets.
- **One namespace, free rebalancing.** Multiple `ActionsGateway`s and
  `RunnerSet`s are permitted per namespace, all drawing on the single namespace
  `ResourceQuota`. A tenant rebalances by editing small `RunnerSet` objects
  (`maxWorkers`/`priorityTiers`) — no template churn, no new namespaces.
  PriorityClasses are already cluster-shared, so tiers compose across runner sets.

## H.6. Naming and length budgets

The one rename worth insisting on: **`RunnerGroup` → `RunnerSet`.** "Runner
group" is already a first-class GitHub concept (runner groups gate which repos
may use which runners), so the current kind name collides with the domain.
`RunnerSet` also aligns with ARC's `AutoscalingRunnerSet`/`EphemeralRunnerSet`.

| New kind | Short | Scope | Role | Derives | Label value |
|---|---|---|---|---|---|
| `ActionsGateway` | `ag` | ns | GitHub binding + AGC control plane | `<ag>-agc` Deploy/SA/Role | `…/gateway` |
| `RunnerSet` | `rs` | ns | scheduling/quota (was `RunnerGroup`) | worker pod `generateName=<rs>-` | `…/runner-set` |
| `RunnerTemplate` | `rt` | ns | pod shape (the large object) | — (referenced) | `…/runner-template` |
| `ClusterRunnerTemplate` | `crt` | cluster | platform golden templates | — | `…/runner-template` |
| `EgressProxy` | `ep` | ns | proxy pool | `<ep>-proxy` Deploy/Svc/HPA/PDB | `…/egress-proxy` |

**Length constraints that actually bite** (RFC 1123): object names ≤ 253, but
**label values ≤ 63** and **Service names ≤ 63**. These CR names become both
selector label values *and* `<name>-<suffix>` Service names, so the
Service-name path is tightest:

- `EgressProxy` → `<ep>-proxy` Service ⇒ name ≤ **57** (reserve `-proxy`).
- `RunnerSet` → worker pod `generateName` plus the random tail, and the name is
  also a label value ⇒ ≤ **63**, practically ≤ ~57 for headroom.
- **Recommendation:** put an explicit `maxLength` of **52** on every v2 CR name
  (leaves 11 for any derived suffix, stays well under 63 as a label value) and
  document it in the CRD field comment so it is discoverable, not a runtime
  surprise.

Field movement, v1alpha1 → v2alpha1:

| v1alpha1 | v2alpha1 |
|---|---|
| `RunnerGroup` (kind) | `RunnerSet` (kind) |
| `RunnerGroup.spec.podTemplate` + `workerImage` | `RunnerTemplate.spec` (+ `ClusterRunnerTemplate`) |
| `RunnerGroup.spec.{runnerLabels,maxListeners,maxWorkers,priorityTiers, lifecycle}` | `RunnerSet.spec` (unchanged) |
| `ActionsGateway.spec.proxy` | `EgressProxy` (kind) |
| `ActionsGateway.spec.runnerGroups` | removed (explicit `RunnerSet` objects) |
| — | `RunnerSet.spec.{gatewayRef,templateRef,proxyRef}`; `ActionsGateway.spec.defaultProxyRef` |

## H.7. Reference integrity — runtime conditions, not admission

Requiring referents to exist at admission time would force apply ordering, which
breaks GitOps: Argo CD/Flux (and `kubectl apply -f dir/`) submit the whole set
at once and rely on eventual consistency. A webhook that denies a `RunnerSet`
because its `RunnerTemplate` has not synced yet turns a normal reconcile into a
failed sync. So responsibilities split by what admission is actually good at:

- **Webhook keeps (static, order-independent):** structural/shape validation, the
  cross-field rule (`maxWorkers == priorityTiers[last].threshold`),
  reserved-pod-field rejection on `RunnerTemplate`, name `maxLength`, reference
  *well-formedness*, and whether a cross-namespace reference is *permitted by
  operator policy* at all.
- **Runtime condition (existence/referential):** does the template exist, does
  the proxy exist, does the cross-namespace grant exist. The controller watches
  referents and re-reconciles when they appear:

  ```
  RunnerSet status:
    Ready: False
    Reason: TemplateNotFound        # or ProxyNotFound / ProxyShareNotGranted
    Message: "RunnerTemplate 'dind-large' not found in namespace 'team-a'"
  ```

  Wire it with a watch + enqueue mapper (template/proxy/grant → referencing
  `RunnerSet`s) so the set flips to `Ready` the moment the missing object syncs.

This stays **fail-closed**: the controller only creates wiring (worker pods, the
cross-namespace NetworkPolicy egress, the proxy CA mount) once both the reference
*and* any required grant resolve. A `RunnerSet` pointing at a not-yet-granted
cross-namespace proxy simply sits `NotReady` — no traffic is ever permitted in
the gap, so moving the grant check to runtime opens no window.

**Fast feedback without the hard reject:** the admission webhook may return a
non-blocking **warning** (admission responses carry `warnings[]` without denying)
when a reference looks dangling at apply time. The operator sees
`Warning: RunnerTemplate 'dind-large' not found` immediately, the object is still
admitted, and the runtime condition remains authoritative.

## H.8. Ownership, GC, and deletion

Shared objects must not be owner-referenced by their referrers:

- **`EgressProxy`** is standalone and owns its *own* children (the proxy
  Deployment/Service/HPA/PDB, reconciled by the GMC). Nothing owns the
  `EgressProxy`.
- **`RunnerTemplate`** is pure data — no children, nothing owns it.
- **Deletion degrades, it does not block.** Hard-blocking deletion of a
  still-referenced shared object via finalizer would fight GitOps prune the same
  way an ordering webhook does. Instead, allow the delete and flip referrers to
  `Ready=False, Reason=TemplateDeleted` (same fail-closed behavior — no template
  ⇒ no new pods). Keep a finalizer only to record `.status.referencedBy` for
  visibility, not to prevent deletion.

## H.9. Cross-namespace proxy sharing

Default is same-namespace. Cross-namespace sharing uses **provider consent**: the
owner of the `EgressProxy` publishes that it is shareable (via
`spec.sharing.allowedNamespaces` or a namespace selector). Naming a
cross-namespace proxy from the consumer side is not sufficient. The Gateway-API
`ReferenceGrant` object is the more idiomatic alternative and the right choice if
the cluster already uses Gateway API; the inline allowlist is preferred here for
lower onboarding cost.

A shared proxy is a **shared egress identity** — it is for *cooperating* tenants
or a *platform-operated* central pool, not mutually-distrusting tenants, because
sharing surrenders the per-tenant egress attribution the proxy exists to provide.

Cross-namespace sharing forces two mechanisms that same-namespace sharing does
not, and these are the bulk of the implementation cost:

1. **NetworkPolicy on both sides.** The GMC must add egress (consumer workers →
   provider proxy Service) *and* ingress (provider proxy ← consumer namespaces)
   whenever a grant is active. Today both policies assume the proxy is colocated.
2. **Proxy TLS CA distribution.** The proxy CA secret is per-tenant today. A
   cross-namespace consumer needs that CA to validate the tunnel, so the GMC must
   replicate/mount it into consumer namespaces — a secret-distribution mechanism
   that does not exist yet.

## H.10. The egress proxy becomes optional

The proxy earns its keep for **stable per-tenant egress IPs** (GitHub
IP-allowlisting, common with Enterprise Managed Users), **egress attribution /
incident containment**, and **avoiding shared-NAT throttling** when many tenants
reach GitHub from one IP. A small single-tenant cluster whose node egress IPs are
already acceptable needs none of that.

So `proxyRef`/`defaultProxyRef` are both optional; unset ⇒ **direct egress**.
Onboarding collapses to three objects — one `ActionsGateway`, one
`RunnerTemplate`, one `RunnerSet` — with no proxy object at all.

**Secure-by-default guardrail (requires sign-off).** The proxy today bundles two
properties: egress *identity* (IP attribution) and egress *restriction* (traffic
can only reach GitHub). Dropping the proxy may drop *identity*, but it must **not**
drop *restriction*:

- The **NetworkPolicy egress lockdown stays mandatory and on by default** even
  with no proxy — default-deny egress, allow only DNS + GitHub CIDRs (+ kube API
  for the AGC). Direct egress is still IP-restricted egress.
- The **managed GitHub-IP refresh loop**, which today hangs off the proxy, must
  move up to the gateway/runner-set level so the direct-egress NetworkPolicy
  stays current.

With those two in place, defaulting the proxy off loses only per-tenant *IP
identity* — a property a subset of tenants need and can opt into — not the egress
*containment* baseline. Defaulting off the *restriction* would be a security
regression and is out of scope. See the
[secure-by-default principle](05-security.md) for the rule this satisfies.

**Composition bonus.** §H.9 and §H.10 combine: a platform team runs one shared
`EgressProxy` in a central namespace, grants it to the EMU/allowlist tenants who
need stable IPs, and everyone else runs proxy-less.

## H.11. Migration (v2, tool-assisted)

A conversion webhook cannot do this migration. Conversion webhooks convert one
object **in place**; they cannot create sibling objects. Splitting one
`ActionsGateway` (with inline proxy + bootstrap groups) into `ActionsGateway` +
`EgressProxy` + N `RunnerTemplate`s + N `RunnerSet`s is a *fan-out on create*,
which no conversion webhook can express. Therefore:

- Serve `v1alpha1` and `v2alpha1` side by side (no automatic conversion of the
  split fields).
- Ship a one-shot **migration tool** (a `kubectl` plugin or subcommand) that
  reads v1 CRs and emits the v2 object set — extracting each inline `podTemplate`
  into a `RunnerTemplate`, the inline `proxy` into an `EgressProxy`, and rewriting
  references. Dry-run to manifests by default; apply on `--apply`.
- Deprecate `v1alpha1` after a release or two. The cutover is deliberate, not
  silent, because the migration is fan-out-on-create.

## H.12. What adopting this changes

This proposal, if accepted, touches more than the API types. Non-exhaustive
impact list, to be turned into plan-doc scope when scheduled:

- **API:** new `v2alpha1` group with four kinds + generated CRDs/deepcopy/RBAC.
- **GMC:** multi-gateway-per-namespace requires every derived resource to be
  keyed by gateway name (not the namespace-singleton assumption today); the
  `gmc-tenant-resource-guard` policy must still confine writes; new
  `EgressProxy` reconciler; cross-namespace CA distribution.
- **AGC:** `RunnerSet` reconciler resolves `templateRef`/`proxyRef` at runtime
  with watches; reserved-field webhook moves to `RunnerTemplate`.
- **Docs:** [§3.1 CRD schemas](03-api-contracts.md#31-kubernetes-crd-schemas),
  [Appendix E (RunnerGroup design)](appendix-e-capacity-planning.md), and the
  operator-facing docs per the
  [doc-update matrix](../development/doc-update-matrix.md).
- **Migration tool** + its tests.

## H.13. Open questions / sign-off needed

1. **Multi-gateway-per-namespace** resource naming and RBAC rework — biggest GMC
   change; review before committing to the API shape.
2. **Cross-namespace proxy CA distribution** — confirm the secret-replication
   mechanism is acceptable (vs. requiring same-namespace proxies only).
3. **Optional proxy** — confirm the secure-by-default guardrail in §H.10 (egress
   restriction stays mandatory; managed-IP refresh relocates).
4. **Sharing model** — inline `allowedNamespaces` vs. Gateway-API `ReferenceGrant`.
5. **Deletion semantics** — degrade-not-block (§H.8); confirm no operators rely
   on hard deletion protection.
