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

### Non-goals

- **Not a behavior change.** v2 re-shapes the API; the runtime semantics (job
  acquisition, worker provisioning, quota/PSA enforcement, egress restriction) are
  preserved. `v2alpha1` tracks v1 behavior wherever a field is unchanged.
- **Not an in-place conversion.** The v1→v2 split is a fan-out handled by a
  migration tool ([§H.11](#h11-migration-v2-tool-assisted)), not a conversion webhook.
- **Not the admin policy layer.** Tiered admin policy (singleton/class) is
  explicitly deferred ([§H.14](#h14-admin-policy-layer--deferred-until-tiering-is-real)).
- **Not cross-namespace sharing on day one.** Same-namespace `EgressProxy` sharing
  ships first; cross-namespace consent + CA distribution follow on demand ([§H.9](#h9-cross-namespace-proxy-sharing)).

### Risks

- **Two APIs in flight.** Serving `v1alpha1` + `v2alpha1` means dual maintenance
  until v1 removal — bounded by the coexistence window and the behavior-parity
  non-goal above.
- **Migration is fan-out-on-create.** Operators run a deliberate one-shot tool,
  not a silent upgrade — mitigated by dry-run-by-default and a documented runbook.
- **Multi-gateway naming collisions.** Per-gateway derived names under a 52-char
  cap ([§H.6](#h6-naming-and-length-budgets)); the webhook-enforced `maxLength`
  makes the limit discoverable, not a runtime surprise.

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
- **Simplest shape that solves a problem we have today; forward-compatible for
  the rest.** Build only the abstraction a *current* pressure forces. Where a
  future need is foreseeable, shape the schema so the abstraction is *additive*
  when its trigger fires — a new kind, or a new optional field with a default —
  never a second breaking migration. Deferred abstractions are recorded with the
  concrete trigger that would revive them (the admin policy layer in
  [§H.14](#h14-admin-policy-layer--deferred-until-tiering-is-real) is the worked
  example).

## H.3. The CRD set

Two controller kinds (`ActionsGateway`, `RunnerSet`) and two data kinds
(`RunnerTemplate`/`ClusterRunnerTemplate`, `EgressProxy`). Boxes are kinds;
arrows are references (a `RunnerSet` points at the objects it uses). Per-kind
fields are in [§H.4](#h4-spec-sketches).

```
┌──────────────────────────────┐
│ ActionsGateway               │   GitHub binding + AGC control plane
│ (1..N per namespace)         │
└──────────────▲───────────────┘
               │ gatewayRef
┌──────────────┴───────────────┐
│ RunnerSet                    │   scheduling / quota (small object)
│ scheduling / quota           │
└──────┬────────────────┬──────┘
       │ templateRef    └───────────────────────┐ proxyRef? (optional;
       ▼                                        ▼ else gateway.defaultProxyRef)
┌──────┴───────────────────────┐   ┌────────────┴─────────────────┐
│ RunnerTemplate /             │   │ EgressProxy       (optional) │
│ ClusterRunnerTemplate        │   │ shared egress proxy pool     │
│ pod shape (large); reusable  │   │ sharing? → cross-ns consent  │
└──────────────────────────────┘   └──────────────────────────────┘
```

This mirrors the Gateway API pattern (`GatewayClass` → `Gateway` → route
attachment by reference) and ARC's split of scheduling (`AutoscalingRunnerSet`)
from pod shape, rather than introducing a novel structure.

### Runtime view — what the kinds become

The diagram above is the static shape; this is what those kinds reconcile into at
runtime. The GMC (cluster-scoped) provisions the per-tenant control plane; the AGC
(one per gateway, in the tenant namespace) provisions worker pods; all GitHub
traffic egresses through the proxy pool's stable per-tenant IP.

```
  GMC · cluster controller
    │  reconciles ActionsGateway → AGC Deployment    ┐ creates
    │  reconciles EgressProxy    → Proxy pool        ┘ + owns
    ▼
  ┌──────────────────────┐  reconciles RunnerSet    ┌──────────────────────┐
  │ AGC · per-gateway    │ ──── → creates pods ───► │ Worker pods          │
  │ controller           │       (one per job)      │ ephemeral · per job  │
  └──────────┬───────────┘                          └──────────┬───────────┘
             │ control-plane long-poll                         │ job egress
             └──────────────────────┬──────────────────────────┘
                                    ▼
                  ┌────────────────────────────┐  stable    ┌──────────────────┐
                  │ Proxy pool                 │ per-tenant  │ GitHub           │
                  │ routes all GitHub egress   │ ─── IP ───► │ broker + API     │
                  └────────────────────────────┘            └──────────────────┘
```

Multiple `ActionsGateway`s may share one namespace; each AGC reconciles only the
`RunnerSet`s whose `gatewayRef` targets it.

## H.4. Spec sketches

```go
// ActionsGateway — GitHub identity + AGC control plane only.
// Now permitted 1..N per namespace.
type ActionsGatewaySpec struct {
    GitHubAppRef    LocalSecretReference `json:"githubAppRef"` // was SecretReference; namespace field dropped
    GitHubURL       string               `json:"githubURL"`    // immutable (CEL oldSelf)
    SecurityProfile string               `json:"securityProfile"` // unchanged; still PSA-labels the namespace
    Tracing         TracingConfig         `json:"tracing"`        // unchanged

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

**Why `templateRef` is required but `proxyRef` is optional.** They look parallel
but are not. An unset `proxyRef` has a well-defined *behavior* — direct egress,
still NetworkPolicy-restricted — so the dependency can simply be dropped. A
`RunnerSet` with no template has no such fallback: the AGC cannot synthesize a
worker pod without a pod shape. So `templateRef` stays **required at GA**. It can
later be relaxed to optional-with-a-default without a breaking change (required →
optional is backward-compatible) — see the deferred
[optional default template](../STATUS.md#deferred) item, which resolves an unset
ref via `ActionsGateway.defaultTemplateRef` → a default-marked
`ClusterRunnerTemplate` (the `StorageClass` pattern: at most one default,
fail-closed `TemplateNotFound` if none resolves — never a flag-synthesized phantom
pod). This keeps `templateRef`/`proxyRef` symmetric: both required at GA, each with
a deferred, additive "optional" form.

### Worked example — minimal proxy-less onboarding (three objects)

```yaml
apiVersion: actions-gateway.com/v2alpha1
kind: ActionsGateway
metadata: { name: acme, namespace: team-a }
spec:
  githubAppRef: { name: acme-github-app }   # LocalSecretReference, same namespace
  githubURL: https://github.com/acme
  securityProfile: baseline
---
apiVersion: actions-gateway.com/v2alpha1
kind: RunnerTemplate
metadata: { name: default, namespace: team-a }
spec:
  podTemplate:
    spec:
      containers:
        - name: runner
          resources: { requests: { cpu: "1", memory: 2Gi } }
---
apiVersion: actions-gateway.com/v2alpha1
kind: RunnerSet
metadata: { name: linux, namespace: team-a }
spec:
  gatewayRef:  { name: acme }
  templateRef: { name: default }   # kind defaults to RunnerTemplate; set kind: ClusterRunnerTemplate for a platform golden template
  runnerLabels: [self-hosted, linux]
  maxListeners: 10
  maxWorkers: 50
  # no proxyRef and no ActionsGateway.spec.defaultProxyRef ⇒ direct egress
  # (RunnerSet status reports proxyMode: Direct + an EgressUnattributed condition)
```

Adding a shared egress proxy is one more object plus a `defaultProxyRef`:

```yaml
apiVersion: actions-gateway.com/v2alpha1
kind: EgressProxy
metadata: { name: shared, namespace: team-a }
spec: { minReplicas: 2, maxReplicas: 10 }
# then set on the gateway:  spec.defaultProxyRef: { name: shared }
# every RunnerSet under that gateway inherits it unless it sets its own proxyRef
```

This shows the renamed group, the noun/verb split, and that proxy-less is the
minimal path — the `EgressProxy` and `RunnerTemplate` reuse only appear when a
tenant actually needs a shared proxy or a second runner shape.

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

### Field naming freezes at GA — do the pass now

JSON field names are part of the API contract and become permanent at `v2`. Do
the naming pass during M1 while names are still cheap to change under `v2alpha1`:

- **Acronym/brand casing — decided.** `github` is one lowercased word; trailing
  initialisms stay uppercase: **`githubURL`, `githubAppRef`** (k8s-consistent with
  `clusterIP`, `targetCPUUtilizationPercentage`). v1's `gitHubURL` / `gitHubAppRef`
  casing is *not* carried over. Apply the rule to every field and freeze it.
- **References are uniform.** `gatewayRef` / `templateRef` / `proxyRef` /
  `githubAppRef` share one `…Ref` suffix and the `ObjectRef` /
  `LocalSecretReference` shapes.
- **List fields are plural** (`runnerLabels`, `priorityTiers`).

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

**Make `gatewayRef` a CRD field selector.** Under multi-gateway, each AGC must
list/watch only the `RunnerSet`s whose `gatewayRef` targets its gateway. Declaring
`spec.gatewayRef.name` a `selectableField` (CRD field selectors, KEP-4358) runs
that filter server-side instead of fetching every `RunnerSet` and filtering
in-process. Additive, but the watch should be designed around it from the start.

**Status & condition contract — uniform across all five kinds.** Every kind
carries `status.conditions` (`listType=map` keyed on `type`),
`status.observedGeneration`, and a `Ready` condition with the same polarity and a
shared reason vocabulary; messages name the specific blocker
(`RunnerTemplate 'dind-large' not found`), never a generic string. Pin this as the
contract in M1 rather than letting each reconciler invent its own.

## H.8. Ownership, GC, and deletion

Shared objects must not be owner-referenced by their referrers:

- **`EgressProxy`** is standalone and owns its *own* children (the proxy
  Deployment/Service/HPA/PDB, reconciled by the GMC). Nothing owns the
  `EgressProxy`.
- **`RunnerTemplate`** is pure data — no children, nothing owns it.
- **Deletion degrades, it does not block — and uses no finalizer at all.**
  Hard-blocking deletion of a still-referenced shared object via finalizer would
  fight GitOps prune the same way an ordering webhook does; Kubernetes' own
  finalizer guidance also warns that finalizers on shared/referenced objects are
  a common cause of stuck-`Terminating` resources. So allow the delete and flip
  referrers to `Ready=False, Reason=TemplateDeleted` (same fail-closed behavior —
  no template ⇒ no new pods) via the referent→referrer watch. Do **not** keep a
  finalizer even for bookkeeping: `.status.referencedBy` is computable from the
  same informer/watch without taking on a finalizer that can block deletion.

## H.9. Cross-namespace proxy sharing

Default is same-namespace. Cross-namespace sharing uses **provider consent**: the
owner of the `EgressProxy` publishes that it is shareable (via
`spec.sharing.allowedNamespaces` or a namespace selector). Naming a
cross-namespace proxy from the consumer side is not sufficient. This mirrors the
consent handshake of Gateway API's `ReferenceGrant` (GA / `v1`), where the grant
lives in the *target* (provider) namespace and a consumer-side name alone never
authorizes the reference.

**v2 ships the inline allowlist only.** It needs no Gateway API CRDs installed
(lower onboarding), and honoring a `ReferenceGrant` when Gateway API *is* present
can be added later without a breaking change. The load-bearing principle taken
from the precedent — **consent is always provider-side** — holds for both.

A shared proxy is a **shared egress identity** — it is for *cooperating* tenants
or a *platform-operated* central pool, not mutually-distrusting tenants, because
sharing surrenders the per-tenant egress attribution the proxy exists to provide.

Cross-namespace sharing forces two mechanisms that same-namespace sharing does
not, and these are the bulk of the implementation cost:

1. **NetworkPolicy on both sides.** The GMC must add egress (consumer workers →
   provider proxy Service) *and* ingress (provider proxy ← consumer namespaces)
   whenever a grant is active. Today both policies assume the proxy is colocated.
2. **Proxy TLS CA distribution — a ConfigMap, not a secret.** A cross-namespace
   consumer needs the proxy's CA *certificate* (public) to validate the tunnel —
   never the private key, which stays in the proxy namespace. So this is trust
   distribution, not secret replication: the GMC writes the CA as a **ConfigMap**
   into only the granted consumer namespaces, scoped by the same grant that
   authorizes the reference. This follows the cert-manager **trust-manager**
   pattern (selector-scoped bundle sync); if trust-manager is installed, the CA
   may instead be published as a `Bundle`. No new secret-distribution mechanism is
   required — the earlier framing of this as a "secret" overstated the cost.

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

Two refinements keep direct egress **auditable**, not silently inferred:

- **Direct egress is a structurally explicit state.** An unset `proxyRef`
  resolves to direct egress, and the runner-set status reflects it (e.g.
  `proxyMode: Direct`) rather than leaving "no proxy" to be inferred from an
  absent field.
- **Surface the attribution trade.** Emit an advisory condition (e.g.
  `EgressUnattributed`) on the proxy-less runner set so an operator can see at a
  glance that this workload has no per-tenant egress identity — the property they
  opted out of — without grepping specs.

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

```
       v1alpha1 (one monolith)            one-shot tool         v2alpha1 (fan-out)
  ┌──────────────────────────────┐                       ┌──────────────────────────────┐
  │ ActionsGateway               │                  ┌──► │ ActionsGateway · identity    │
  │   ├ githubAppRef · githubURL │                  │    └──────────────────────────────┘
  │   ├ spec.proxy (inline)      │   ┌───────────┐  │    ┌──────────────────────────────┐
  │   └ spec.runnerGroups[]      │──►│ migration │──┼──► │ EgressProxy                  │
  │       (inline podTemplates)  │   │ tool      │  │    └──────────────────────────────┘
  └──────────────────────────────┘   │ dry-run → │  │    ┌──────────────────────────────┐
            reads v1                  │ --apply   │  ├──► │ RunnerTemplate × N           │
                                      └───────────┘  │    └──────────────────────────────┘
                                     fan-out on create│    ┌──────────────────────────────┐
                                                       └──► │ RunnerSet × N                │
                                                            └──────────────────────────────┘
```

A conversion webhook can't create those sibling objects — which is exactly why the
migration is a tool, not a webhook.

The v1→v2 fan-out is one-time. Once on `v2alpha1`, the API graduates **in place**
`v2alpha1 → v2beta1 → v2` via a conversion webhook (a thing a conversion webhook
*can* do, since the kinds no longer change shape) — see the
[graduation path](../plan/v2-api.md#api-maturity--graduation-v2alpha1--v2beta1--v2)
in the implementation plan for the per-hop mechanics.

## H.12. Folding in the grandfathered label-value alignment (Q147)

Two shipped keys still carry boolean-looking `"true"` values that predate the
[no-boolean label convention](../development/kubernetes-conventions.md) and are
grandfathered only because changing them is breaking:

- `actions-gateway.github.com/tenant: "true"` — the managed-tenant marker, matched
  as `== 'true'` by the `namespace-psa-guard` and `tenant-resource-guard`
  `ValidatingAdmissionPolicy` objects, the onboarding scripts, and operator runbooks.
- `actions-gateway.github.com/allow-profile-downgrade: "true"` — the downgrade
  opt-in annotation, matched by the GMC validating webhook.

Aligning them (→ `tenant: managed`, `allow-profile-downgrade: allowed`, following the
existing `privileged-profile: allowed` precedent) is a breaking change to deployed
clusters: it touches VAP CEL, onboarding, runbooks, and the label/annotation on every
live tenant namespace. The convention doc therefore defers it to "a separate,
deliberate migration." **The v2 cutover is that migration** — it is already breaking,
already ships a migration tool ([§H.11](#h11-migration-v2-tool-assisted)), and already
reworks the same VAPs and onboarding for multi-gateway-per-namespace. Folding Q147 in
here costs almost nothing extra and avoids a second, standalone breaking migration
later.

**The key *prefixes* migrate too, not just the values.** The v2
[API group rename](#h15-other-breaking-changes-worth-batching) moves these keys off
the `actions-gateway.github.com/` domain to `actions-gateway.com/` (e.g.
`actions-gateway.com/tenant`), together with the other domain-prefixed identifiers
— `privileged-profile`, `agentpool-cleanup`, `gmc-cleanup`, the version label, and
the finalizer names. This is the same class of breaking change as the value
alignment and rides the **same dual-read window**: every consumer accepts either
domain *and* either value until `v1alpha1` removal, and the migration tool
relabels in one pass. Renaming the API group but leaving the labels on the old
domain would be a permanent inconsistency, so the prefixes move *with* the group.

Both keys survive into v2 unchanged in *meaning* — the `tenant` marker still confines
the GMC's namespace writes under multi-gateway-per-namespace, and
`allow-profile-downgrade` still guards `ActionsGateway` PSA downgrades — so the cutover
changes only their *values*, not their role.

**Dual-read window (coincident with the v1/v2 coexistence window).** Q147 needs a
dual-read migration so live namespaces are not broken mid-cutover:

1. While `v1alpha1` and `v2alpha1` are served side by side, every consumer of these
   values — both VAPs and the downgrade webhook — accepts **either** `"true"` (legacy)
   **or** the new keyword. Reads are dual; writes prefer the new keyword.
2. The migration tool ([§H.11](#h11-migration-v2-tool-assisted)) relabels the namespace
   marker and rewrites the annotation to the new keyword as part of the same one-shot
   pass that emits the v2 object set, so no separate operator action is required.
3. When `v1alpha1` is deprecated and removed, drop the `"true"` arm from the VAPs and
   webhook. The dual-read window closes exactly when `v1alpha1` serving does.

This stays **fail-closed** throughout: the CEL/webhook checks already treat any
non-sentinel value as "not granted", so accepting a second sentinel during the window
never widens a grant — at worst a namespace is briefly un-aligned and the
already-applied `"true"` keeps working until the tool relabels it.

## H.13. What adopting this changes

This proposal, if accepted, touches more than the API types. Non-exhaustive
impact list, to be turned into plan-doc scope when scheduled:

- **API:** new `v2alpha1` group `actions-gateway.com` with five CRD kinds
  (`ActionsGateway`, `RunnerSet`, `RunnerTemplate`, `ClusterRunnerTemplate`,
  `EgressProxy`) + generated CRDs/deepcopy/RBAC.
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
- **Label keys + values (Q147 + group rename):** both the domain *prefix*
  (`actions-gateway.github.com/*` → `actions-gateway.com/*`, plus finalizer names)
  and the grandfathered `tenant`/`allow-profile-downgrade` `"true"` *values* migrate
  during the cutover ([§H.12](#h12-folding-in-the-grandfathered-label-value-alignment-q147))
  — VAP CEL, onboarding scripts, runbooks, and the convention doc's "grandfathered"
  note all update, and one dual-read window covers both, riding the v1alpha1 serving
  window.

## H.14. Admin policy layer — deferred until tiering is real

The decomposition above mirrors Gateway API's `Gateway → route attachment` but
stops one level short: there is no cluster-scoped, **admin-owned** object — no
`GatewayClass` equivalent. Today the admin/tenant boundary is real but lives
*outside the API*, scattered across mechanisms that cannot be RBAC'd, audited, or
GitOps'd as objects:

| Policy | Where it lives today |
|---|---|
| Which PriorityClasses a tenant may name | `--allowed-priority-classes` GMC flag |
| Whether `privileged` profile is allowed | namespace label `…/privileged-profile=allowed` |
| Default worker image | `--worker-image` GMC flag |
| Reserved namespaces | Go constant + `POD_NAMESPACE` |
| Namespace ResourceQuota | platform-stamped, out-of-band |

Promoting this into a first-class API object turns the boundary into a clean RBAC
split (admin writes the policy kind; tenants cannot) and makes "what is this
tenant allowed to do?" a `kubectl get` away. **But it is not a problem we have
today, and the abstraction is addable without a second breaking change** — so v2
does *not* ship it. This section records the capability ladder and the exact
trigger, so the decision is captured rather than rediscovered.

### The capability ladder

| Layer | Expresses | Breaks when |
|---|---|---|
| **Flags (today)** | one global policy, cluster-wide | can't vary per tenant at all (except the one bolted-on privileged namespace-label gate) |
| **Singleton policy object** | one global policy, but declarative / auditable / RBAC'd | still *uniform* — every tenant gets the same rules |
| **Singleton + namespace labels** | one global policy plus a *few independent* per-tenant dials ("privileged iff namespace has label X") | the per-tenant variation becomes *multi-dimensional and correlated* |
| **Class** (`ActionsGatewayClass`) | named bundles of correlated policy, tenant-selectable, RBAC-gated on which class may be referenced | — |

A *single* per-tenant escape hatch (like privileged-allowed) does **not** need a
class — a namespace label the admin controls handles it, which is already how
v1 works. The singleton + the occasional label dial gets you a long way.

### Promote flags → singleton when

The singleton carries the *same* policy as the flags — one uniform policy, no
tiers — so it buys only the rung-2 wins, not tiering. Promote when **any** of:

- **GitOps** — policy must be managed declaratively / changed without a
  controller redeploy.
- **RBAC separation** — the people who set policy must be distinct from the
  people who own the controller Deployment (a platform-policy team vs. the SRE
  who deploys the GMC).
- **Audit/compliance** — "show me, as a cluster object, exactly what tenants are
  allowed" is an actual requirement.

If none of those bite, flags are simpler and equally forward-compatible.

### The trigger for the class

Introduce `ActionsGatewayClass` only when **both** hold:

1. **≥2 distinct policy *bundles* must coexist** in one cluster — e.g. an
   "internal/trusted" tier (DinD allowed, broad registries, proxy optional) vs an
   "external/untrusted" tier (restricted-only, platform registry only, proxy
   mandatory): multiple policy dimensions that *travel together* as a tier; **and**
2. **either** those tiers are spread across enough namespaces that encoding each
   as a *combination* of namespace labels becomes an audit/maintenance liability
   (N namespaces × M labels that should just say "tier = A"), **or** you want
   tenants to **self-select** a tier with RBAC deciding which they may pick —
   which labels cannot express, because tenants do not control namespace labels.

Smell signs the trigger has arrived: the onboarding runbook grows a "pick your
tier, then apply these K labels" step (that step *is* a class waiting to be
born); a request like "team X gets privileged + registries A&B, team Y neither";
or a self-service "request the privileged tier" flow.

### Why deferring costs nothing

**Every rung of the ladder is an additive transition** — none is a breaking
migration:

- *flags → singleton*: add the `ActionsGatewayPolicy` kind; the controller
  prefers it when present and the flags remain as fallback/defaults.
- *singleton → class*: add the `ActionsGatewayClass` kind, and add
  `ActionsGateway.spec.gatewayClassName` as an *optional* field whose unset value
  means "the default class / the old singleton"; the singleton simply *becomes*
  the default class.

So deferring either step buys no future breaking migration. The one constraint to
honor now: **whatever policy lands in v2 — flags or a singleton — must be shaped
so a future singleton/class could carry the identical schema field-for-field.**
Don't paint the policy into a corner a later rung couldn't inherit.

### v2 decision

**v2 keeps the controller flags.** A singleton/class earns its keep only at the
triggers above, none of which is a problem we have today, and every rung is
additive — so promoting later costs nothing, while building now would be
abstraction ahead of need. The single obligation v2 carries is to shape the
flag-backed policy so a future singleton/class inherits its schema field-for-
field. **Ship neither the singleton nor the class.** Promote to the singleton at
the flags→singleton trigger; introduce the class at the two-part class trigger.

## H.15. Other breaking changes worth batching

v2 is the one window where breaking changes are cheap (we are already rewriting
the schema and shipping a migration tool). A few small changes are only
*possible* at a major break, or are awkward to add later — batch them in, but
only the ones that fix a problem we have today.

**Decided for v2 (today's problem, break-only or break-cheapest):**

- **Drop the `SecretReference.namespace` footgun.** It is reserved-but-validated-
  empty and reads like a cross-namespace reference that does not exist. Replace
  with a name-only `LocalSecretReference`. Removing a field is break-only.
- **Per-field immutability** via CEL `XValidation` (`oldSelf`): **`githubURL`
  immutable** — rebinding a running gateway's GitHub org is a footgun;
  **`githubAppRef.name` mutable** — it is the credential-rotation path. Adding
  immutability later is itself breaking, so it is fixed at v2.
- **API group rename → `actions-gateway.com`.** The group is
  `actions-gateway.github.com`, which suffixes a domain the project does not
  control — against the k8s convention of using a domain you own. The project
  owns `actions-gateway.com`, so v2 renames the group to it. Changing the group
  touches every CRD (and every CR, RBAC rule, VAP, and manifest that names it),
  so it can only happen at a major break — it rides the v2 cutover and its
  migration tool. The **label/annotation key prefixes, the version label, and
  finalizer names** carry the same domain and rename with it
  (`actions-gateway.github.com/*` → `actions-gateway.com/*`), on the Q147 dual-read
  window so live namespaces are not broken mid-cutover ([§H.12](#h12-folding-in-the-grandfathered-label-value-alignment-q147)).
- **Cheap usability while regenerating:** `additionalPrinterColumns` (Ready,
  profile, active sessions), resource `categories`, and the short names from
  [§H.6](#h6-naming-and-length-budgets).
- **`maxListeners` default → `10`** (was `1` in code; matches the design).
  Confirmed against the AGC listener `Multiplexer`/`Run` source: the pool keeps a
  permanent baseline of **one** poller and demand-spawns extra pollers only as
  jobs are acquired (a job-holding goroutine is busy, not polling, for the job's
  whole duration), with non-baseline pollers idle-exiting after 50 empty polls.
  So `maxListeners` is a **concurrency ceiling with a baseline of 1**, not a
  steady-state count: a higher default costs nothing at idle, while `1`
  serializes job pickup per group (the busy baseline leaves no poller, and
  `SpawnReplacement` is a no-op at the ceiling). The real resource guards
  (`maxWorkers` + namespace `ResourceQuota`) still bind, so the higher default
  regresses no safety property. v2 sets the default to `10`.

**Opportunistic (take if it falls out of the rewrite; not a sign-off item):**

- **Webhook → CEL migration.** v2 targets a newer k8s floor, so checks that are
  webhook-only today *because* CEL could not express them on k8s ≤1.30 (singleton,
  GitHub-URL structure, cross-field rules) can
  become structural/CEL. Every check moved out of the fail-closed validating
  webhook is one fewer thing whose outage blocks all admission — an availability
  and operability win, best taken during the schema rewrite.

**Explicitly NOT now (shape for additive later, do not build):**

- **Admin policy class** — [§H.14](#h14-admin-policy-layer--deferred-until-tiering-is-real).
- **Worker-image registry allowlist** — a real security control, but only needed
  once there are untrusted tenants to restrict. It belongs in the admin policy
  schema and is enforced when that layer arrives; do not add a standalone tenant
  field for it now.
- **Credentials as a discriminated union.** OIDC / workload-identity federation
  is the foreseeable successor to long-lived GitHub App keys, but a bare
  `githubAppRef` today does not block it: a future `workloadIdentityRef` sibling
  field (optional, with a CEL "exactly one of") is *additive*, not breaking. So
  do not introduce a tagged union now — keep the single field.

## H.16. Open questions / sign-off needed

### Recommended (pending ratification)

Each carries a recommendation grounded in precedent (Gateway API, ARC,
cert-manager trust-manager, Kubernetes finalizer guidance); ratify or override.

1. **Multi-gateway-per-namespace — naming, AGC scoping, ownership.** Verified
   against `gmc-tenant-resource-guard` (`cmd/gmc/config/admission-policy/tenant-resource-guard.yaml`):
   the GMC-confinement VAP keys on the namespace `tenant=true` marker, **not on
   resource names**, so it already scales to N gateways per namespace and needs no
   change. The real work is three controller-side changes:
   - **(a) Per-gateway naming** — every derived resource becomes `<ag>-<suffix>`
     (`<ag>-agc`, `<ep>-proxy`, worker `generateName=<rs>-`) under the
     [§H.6](#h6-naming-and-length-budgets) 52-char cap, so two gateways in one
     namespace never collide on a fixed name.
   - **(b) Per-gateway AGC scoping** — N gateways ⇒ N AGC Deployments in one
     namespace, so each AGC must reconcile **only the `RunnerSet`s whose
     `gatewayRef` targets it** — the one genuinely new controller behavior, without
     which N controllers fight over the same objects.
   - **(c) Per-gateway ownership** — each `ActionsGateway` owner-refs its own
     children so deleting one gateway GCs only its resources, not a neighbor's.

   (Optional defense-in-depth: also require a GMC `managed-by` label on writes; not
   needed for correctness since the VAP already confines by namespace.) Precedent:
   ARC runs multiple scale sets per namespace, names = CR prefix + fixed suffix.
   The core build of v2 — naming + watch-scoping + ownership, not a policy rewrite.
2. **Cross-namespace proxy CA distribution → ConfigMap, not secret.** The CA is a
   public certificate, so the GMC distributes it as a **ConfigMap** into only the
   granted consumer namespaces (trust-manager pattern; [§H.9](#h9-cross-namespace-proxy-sharing)).
   No secret-replication subsystem is needed — recommend dropping that as a
   blocker.
3. **Optional proxy** — ✅ guardrail as in [§H.10](#h10-the-egress-proxy-becomes-optional):
   egress restriction stays mandatory, managed-IP refresh relocates, plus an
   explicit `proxyMode: Direct` status and an `EgressUnattributed` advisory
   condition so the attribution trade is auditable.
4. **Sharing model** — ship the **inline `allowedNamespaces` allowlist only** for
   v2; `ReferenceGrant` support is additive later. Consent stays provider-side
   either way ([§H.9](#h9-cross-namespace-proxy-sharing)).
5. **Deletion semantics** — degrade-not-block with **no finalizer at all**
   ([§H.8](#h8-ownership-gc-and-deletion)); `referencedBy` is computed from the
   watch. Confirm no operator relies on hard deletion protection.
6. **Q147 label-value keywords** — ratify `tenant: managed` and
   `allow-profile-downgrade: allowed` (symmetric with `privileged-profile:
   allowed`), with the dual-read window closing only at `v1alpha1` removal
   ([§H.12](#h12-folding-in-the-grandfathered-label-value-alignment-q147)).

### Resolved

8. **Admin policy layer** (§H.14) — ✅ **v2 keeps the controller flags.** Neither
   the singleton nor the class ships in v2; each is deferred behind a documented
   trigger (flags→singleton, then the two-part class trigger), and every rung is
   an additive, non-breaking transition. v2's only obligation is to shape the
   flag-backed policy so a future singleton/class inherits its schema
   field-for-field.
9. **API group rename** (§H.15) — ✅ **Yes, rename to `actions-gateway.com`.**
   The project owns the domain; the change rides the v2 cutover and its migration
   tool. Break-only, so it happens here or never.
10. **Per-field immutability** (§H.15) — ✅ **`githubURL` immutable,
    `githubAppRef.name` mutable.**
11. **`maxListeners` default** (§H.15) — ✅ **`10`** (was `1` in code). Verified
    against the AGC listener source: `maxListeners` is a concurrency ceiling with
    a baseline-of-1 + demand-spawn + idle-shutdown, so a higher default is free
    at idle and `1` needlessly serializes job pickup; `maxWorkers`/quota remain
    the binding resource guards. (Closed Q162.)
