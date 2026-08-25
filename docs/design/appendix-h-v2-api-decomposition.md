# Appendix H — v2 API Decomposition

← [Optional Future Enhancements](appendix-g-future-enhancements.md) | [Back to index](README.md)

---

> **Status: shipped — `v2alpha1`, served beside `v1alpha1`.** This appendix is the design source of truth for the `v2alpha1` API (`actions-gateway.com` group) that replaces the monolithic `ActionsGateway` + `RunnerGroup` model.
> **All milestones M1–M5 have landed**: the five kinds, their GMC/AGC reconcilers, multiple gateways per namespace, the namespace-scoped security profile, and the one-shot v1→v2 migration tool are all built.
> `v2alpha1` is an **alpha** API served side by side with `v1alpha1` during the coexistence window — nothing in the shipped `v1alpha1` API changes, and tenants migrate on their own schedule via the migration tool, a deliberate tool-assisted fan-out (see [§H.11](#h11-migration-v2-tool-assisted)).
> Milestone sequencing and the itemized task record are in the [v2 API plan](../plan/v2-api.md).

---

## H.1. Why decompose

Three independent pressures all trace back to the same root cause — the current API is a **monolith that aggregates large pod templates, couples the egress proxy to the tenant 1:1, and assumes one gateway per namespace.**

1. **Pod templates are growing past comfortable object sizes.** Docker-in-Docker and sysbox runner images need large `PodTemplateSpec`s (init containers, sidecars, volumes, security context).
   Today the template is embedded in `RunnerGroupSpec`, and a bootstrap copy can also be embedded inline in `ActionsGateway.spec.runnerGroups[]`.
   Several runner groups, each with a fat template, marshalled into one CR is the failure mode that approaches etcd's ~1.5 MB per-object limit.
   The limit is **per object**, so the fix is to stop the *aggregation* and enable *reuse*, not to shrink fields.

2. **Multiple runner sets should be able to share one egress proxy.** The proxy is currently created per `ActionsGateway` and is not independently addressable.
   There is no way to point several runner sets at one proxy pool, or to run a single platform-operated pool.

3. **Tenants want everything in one namespace and the freedom to rebalance.** The current rule is one `ActionsGateway` per namespace.
   A tenant running several GitHub orgs, or wanting to shuffle `maxWorkers`/`priorityTiers` across runner sets against a single namespace `ResourceQuota`, cannot do so without spreading across namespaces.

### Non-goals

- **Not a behavior change.** v2 re-shapes the API; the runtime semantics (job acquisition, worker provisioning, quota/PSA enforcement, egress restriction) are preserved.
  `v2alpha1` tracks v1 behavior wherever a field is unchanged.
- **Not an in-place conversion.** The v1→v2 split is a fan-out handled by a migration tool ([§H.11](#h11-migration-v2-tool-assisted)), not a conversion webhook.
- **Not the admin policy layer.** Tiered admin policy (singleton/class) is explicitly deferred ([§H.14](#h14-admin-policy-layer--deferred-until-tiering-is-real)).
- **Not cross-namespace sharing on day one.** Same-namespace `EgressProxy` sharing ships first; cross-namespace consent + CA distribution follow on demand ([§H.9](#h9-cross-namespace-proxy-sharing)).

### Risks

- **Two APIs in flight.** Serving `v1alpha1` + `v2alpha1` means dual maintenance until v1 removal — bounded by the coexistence window and the behavior-parity non-goal above.
- **Migration is fan-out-on-create.** Operators run a deliberate one-shot tool, not a silent upgrade — mitigated by dry-run-by-default and a documented runbook.
- **Multi-gateway naming collisions.** Per-gateway derived names under a 52-char cap ([§H.6](#h6-naming-and-length-budgets)); the webhook-enforced `maxLength` makes the limit discoverable, not a runtime surprise.

## H.2. Design principles

- **Split *data* objects from *controller* objects.** Two kinds are reconciled into running infrastructure (verbs); two are reusable data referenced by name (nouns).
  The large pod template lives only in a data object, so it never co-bloats anything that gets aggregated.
- **Reference, don't embed; reference, don't own.** Shared objects are named by reference.
  Referrers never set owner references on referents (that would cascade-delete a shared object when any one referrer goes away).
- **GitOps-friendly: no apply-ordering requirements.** Referential integrity is a runtime condition, not an admission gate.
  Applying a whole directory at once must converge regardless of object order.
- **Secure by default, optional by layering.** The egress proxy becomes optional to lower onboarding cost, but the egress *restriction* it sat on top of stays mandatory.
  Cross-namespace sharing requires explicit provider consent.
- **Simplest shape that solves a problem we have today; forward-compatible for the rest.** Build only the abstraction a *current* pressure forces.
  Where a future need is foreseeable, shape the schema so the abstraction is *additive* when its trigger fires — a new kind, or a new optional field with a default — never a second breaking migration.
  Deferred abstractions are recorded with the concrete trigger that would revive them (the admin policy layer in [§H.14](#h14-admin-policy-layer--deferred-until-tiering-is-real) is the worked example).

## H.3. The CRD set

Two controller kinds (`ActionsGateway`, `RunnerSet`) and two data kinds (`RunnerTemplate`/`ClusterRunnerTemplate`, `EgressProxy`).
Boxes are kinds; arrows are references (a `RunnerSet` points at the objects it uses).
Per-kind fields are in [§H.4](#h4-spec-sketches).

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

This mirrors the Gateway API pattern (`GatewayClass` → `Gateway` → route attachment by reference) and ARC's split of scheduling (`AutoscalingRunnerSet`) from pod shape, rather than introducing a novel structure.

### Runtime view — what the kinds become

The diagram above is the static shape; this is what those kinds reconcile into at runtime.
The GMC (cluster-scoped) provisions the per-tenant control plane; the AGC (one per gateway, in the tenant namespace) provisions worker pods; all GitHub traffic egresses through the proxy pool's stable per-tenant IP.

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

Multiple `ActionsGateway`s may share one namespace; each AGC reconciles only the `RunnerSet`s whose `gatewayRef` targets it.

## H.4. Spec sketches

```go
// ActionsGateway — GitHub identity + AGC control plane only.
// Now permitted 1..N per namespace.
type ActionsGatewaySpec struct {
    Credentials        GitHubCredentials    `json:"credentials"`        // discriminated union: githubApp today, workloadIdentity additive (Q196/Q197, §H.15)
    GitHubURL          string               `json:"githubURL"`          // immutable (CEL oldSelf)

    // GitHubCABundleRef names a ConfigMap in this namespace carrying, under
    // "ca.crt", an extra PEM bundle to trust when reaching githubURL — the private
    // CA fronting a GHES appliance (Q536). Additive to the system roots, never a
    // replacement, and mounted on both the AGC and this gateway's worker pods.
    // Optional; unset ⇒ system roots only. A ConfigMap rather than a Secret because
    // a certificate is public material. Resolved at runtime per §H.7: a missing or
    // unparseable referent degrades the gateway (CABundleNotFound/CABundleInvalid)
    // rather than provisioning an AGC whose pod cannot start.
    GitHubCABundleRef *LocalConfigMapReference `json:"githubCABundleRef,omitempty"`
    DefaultTemplateRef *ObjectRef           `json:"defaultTemplateRef"` // optional (Q172): inherited by RunnerSets with no templateRef
    DefaultRunnerGroup string               `json:"defaultRunnerGroup"` // optional (Q712): GitHub runner group inherited by RunnerSets with no runnerGroup
    Tracing            TracingConfig        `json:"tracing"`            // unchanged

    // REMOVED vs v1alpha1: SecurityProfile string → PSA is namespace-scoped, so it
    // is owned at the namespace (GMC-guarded), not per gateway. See §H.16 #7.
    // (Fallback if co-located differing profiles are ever needed: keep it here and
    // resolve by most-restrictive-wins.)

    // DefaultProxyRef names an EgressProxy used for AGC control-plane egress and
    // inherited by RunnerSets that do not set their own proxyRef. Optional:
    // unset means the control plane egresses directly (subject to NetworkPolicy).
    // Same-namespace unless the target EgressProxy grants cross-namespace use.
    DefaultProxyRef *ObjectRef `json:"defaultProxyRef,omitempty"`

    // AGCResources optionally tunes the per-gateway AGC container CPU/memory
    // requests/limits (Q171). Additive, per-key overlay of the platform default
    // (requests cpu:500m/mem:2Gi, limits cpu:2/mem:4Gi — the Appendix A sizing);
    // unset ⇒ that default unchanged. See §H.4 note below.
    AGCResources *corev1.ResourceRequirements `json:"agcResources,omitempty"`

    // AGCAutoscaling optionally opts this gateway's AGC into managed vertical
    // right-sizing (Q360): the GMC stamps a VerticalPodAutoscaler next to the AGC
    // Deployment. Presence is the opt-in; mode is Off (recommend-only, default),
    // Initial, or Recreate. Composes with agcResources — see §H.4 note below.
    AGCAutoscaling *AGCVerticalAutoscaling `json:"agcAutoscaling,omitempty"`

    // REMOVED vs v1alpha1: Proxy ProxyConfig         → standalone EgressProxy
    // REMOVED vs v1alpha1: RunnerGroups []RunnerGroupSpec → explicit RunnerSet objects
}

// EgressProxy — standalone, optionally shared proxy pool (was ActionsGateway.spec.proxy).
type EgressProxySpec struct {
    MinReplicas, MaxReplicas       *int32
    TargetCPUUtilizationPercentage *int32

    // ManagedAutoscaling (default true) is the bring-your-own-autoscaler opt-out
    // (Q173), mirroring managedNetworkPolicy: false ⇒ the GMC provisions no HPA —
    // only the stable "<name>-proxy" Deployment, whose .spec.replicas the operator's
    // scaler (KEDA, VPA, a custom HPA) then owns outright, an external scale-to-zero
    // included. While false: maxReplicas/targetCPUUtilizationPercentage are inert,
    // minReplicas seeds only the initial replica count, Ready compares ready pods to
    // the Deployment's own desired count, and ProxyQuotaPressure measures headroom
    // to that desired count instead of maxReplicas. Ownership shift only — no
    // security property changes.
    // +kubebuilder:default=true
    ManagedAutoscaling *bool `json:"managedAutoscaling,omitempty"`

    Resources                      corev1.ResourceRequirements
    NoProxyCIDRs                   []string
    ManagedNetworkPolicy           *bool

    // LogLevel is the per-pool verbosity knob (info|debug, default info) — v1
    // parity (Q327). Threaded to the proxy container as LOG_LEVEL; changing it
    // rolls the pool. In v1 the single ActionsGateway.spec.logLevel covered both
    // the AGC and its inline proxy; in v2 the decomposed kinds each carry their
    // own (ActionsGateway.spec.logLevel for the AGC, this field for the pool).
    // +kubebuilder:validation:Enum=info;debug
    // +kubebuilder:default=info
    LogLevel string `json:"logLevel,omitempty"`

    // AuditLogging is the per-pool egress audit record (Q564, appendix G.3):
    // Off (default) writes none; Connections writes one structured line per
    // ACCEPTED CONNECT at tunnel close (namespace, destination host and port,
    // bytes each way, duration), and nothing from the request headers or the
    // tunneled bytes. Off by default is the security requirement, not a
    // convenience: the record is data about a tenant's egress. An enum rather
    // than a bool because what gets recorded is a policy that can grow a kind of
    // record that is not per-connection. Threaded as PROXY_AUDIT_LOGGING plus a
    // downward-API POD_NAMESPACE, injected only when non-Off so an unopted pool's
    // pod template is unchanged; changing it rolls the pool.
    // +kubebuilder:validation:Enum=Off;Connections
    // +kubebuilder:default=Off
    AuditLogging string `json:"auditLogging,omitempty"`

    // EgressPolicyMode is TENANT INTENT for how the GMC expresses the GitHub egress
    // allowlist: CIDR (default; standard NetworkPolicy + 24h IPRangeReconcile, works on
    // every CNI) or FQDN (by hostname, via a CNI-native DNS-aware policy). For FQDN
    // intent the OPERATOR picks the mechanism with the GMC --fqdn-policy-backend flag
    // (none|cilium|calico|gke) — none (default) rejects FQDN intent at admission. The
    // deprecated CiliumFQDN/CalicoFQDN values pin their namesake backend (Q245) and are
    // removable no earlier than v3.0.0, being members of the served beta version (Q428). FQDN
    // modes are fail-closed: the standard NetworkPolicy still default-denies GitHub
    // egress, so an unenforced FQDN policy keeps egress denied rather than opening it
    // (Q208). No effect when ManagedNetworkPolicy is false.
    // +kubebuilder:validation:Enum=CIDR;FQDN;CiliumFQDN;CalicoFQDN
    // +kubebuilder:default=CIDR
    EgressPolicyMode EgressPolicyMode `json:"egressPolicyMode,omitempty"`

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
    TemplateRef *ObjectRef // RunnerTemplate | ClusterRunnerTemplate; optional (Q172): unset ⇒ gateway.defaultTemplateRef ⇒ the single cluster-default ClusterRunnerTemplate ⇒ TemplateNotFound
    ProxyRef    *ObjectRef // EgressProxy; nil ⇒ gateway.defaultProxyRef; both nil ⇒ direct egress

    RunnerLabels  []string
    RunnerGroup   string     // GitHub runner group; "" ⇒ gateway.defaultRunnerGroup; both "" ⇒ GitHub's default group (Q712)
    MaxListeners  int32
    MaxWorkers    *int32
    PriorityTiers []PriorityTier
    // lifecycle tunables (eviction/quota retries, TTLs, deadlines) — unchanged from RunnerGroup
}
```

**Why `templateRef` and `proxyRef` are both optional — but resolve differently.** They look parallel but the *fallback* differs.
An unset `proxyRef` has a well-defined *behavior* — direct egress, still NetworkPolicy-restricted — so the dependency can simply be dropped (Q168, **shipped**): both `proxyRef` and `defaultProxyRef` unset resolves to direct egress, with `proxyMode: Direct` + an `EgressUnattributed` condition in status.
A `RunnerSet` with no template has no such drop-the-dependency fallback — the AGC cannot synthesize a worker pod without a pod shape — so instead of a behavior it resolves a *default template* (Q172, **shipped**).
`templateRef` was required at GA; it has been relaxed to optional-with-a-default — a backward-compatible required → optional change, so a set that sets `templateRef` behaves exactly as before.

The resolution chain for an unset `templateRef` (runtime, fail-closed, §H.7) is:

1. `rs.spec.templateRef` (explicit) — `status.templateSource: TemplateRef`.
2. else `ActionsGateway.spec.defaultTemplateRef` — per-gateway default (may name a `RunnerTemplate` or a `ClusterRunnerTemplate`); `templateSource: GatewayDefault`.
3. else the **single** cluster-default `ClusterRunnerTemplate` — the one marked `actions-gateway.com/is-default-template: "true"` (the `StorageClass` default-class pattern); `templateSource: ClusterDefault`.
4. else `Ready=False`/**`TemplateNotFound`** — fail-closed, no worker wiring, **never a synthesized phantom pod**.

**At most one cluster-default — enforced at runtime, not admission.** The marker lives only on the cluster-scoped `ClusterRunnerTemplate` (platform-authored: a tenant cannot self-elect a namespaced `RunnerTemplate` cluster-wide).
If two are marked, the AGC fails closed `Ready=False`/`AmbiguousDefault` (naming the conflicts) rather than silently picking one — stricter than upstream StorageClass's newest-wins.
The ≤1 invariant is checked at *resolution time* in the AGC reconciler, not at admission, for the same reason all reference integrity is runtime here (§H.7): it is a cross-object invariant single-object CEL cannot express, and an admission reject would break GitOps apply-ordering.
The trade-off — admission would give earlier feedback — is accepted; the condition surfaces the moment a `RunnerSet` actually depends on the ambiguous default, and clears the moment one default is demoted (the `ClusterRunnerTemplate` watch re-enqueues).
A `defaultTemplateRef`/`templateRef` that *names a missing* template still fails closed (`TemplateNotFound`), exactly like a missing proxy fails closed (`ProxyNotFound`); only an entirely-unset reference falls through to the next rung.

**The GitHub-side boundary: `runnerGroup` / `defaultRunnerGroup` (Q712, shipped).** Everything else in this appendix bounds what a tenant's runners *may do*; the GitHub runner group bounds *who may cause them to run*.
It is GitHub's own authorization point for which repositories can target a scale set, and it sits outside the cluster entirely, so no NetworkPolicy, PSA label, or admission rule reaches it.
A scale set left in the installation's default group is targetable by every repository that group admits, typically the whole organization, so a repository outside the tenant can name the set in `runs-on` and route work into the tenant's namespace, quota, and egress IP.
Pod-level isolation is unaffected by this; what is unbounded without a group is the *intake*.

`RunnerSet.spec.runnerGroup` names the group, inheriting `ActionsGateway.spec.defaultRunnerGroup` when unset.
That is the same shape as `templateRef`/`proxyRef`, so a tenant declares its boundary once on the gateway and a set overrides it only to narrow further (a GPU set whose repository access differs, say).
Both unset leaves GitHub's default group, which is today's behaviour and stays the default because a group GAG did not create is not GAG's to assume.

Resolution is runtime and fail-closed, per §H.7: a name the installation has no group for leaves the set `Ready=False`/**`RunnerGroupNotFound`** rather than registering into the default group.
That direction is the whole point, because the convenient fallback silently *widens* the boundary the operator was narrowing, and does it at exactly the moment they typed the name wrong.
Two consequences follow from the group being read once, at scale-set registration: a set adopted from an earlier run is **moved** into its declared group rather than keeping the group it was created in, and changing the declared group restarts the set's listener rather than waiting for an AGC rollout.

What GAG does **not** own: creating runner groups, and configuring which repositories each admits.
Both are the platform admin's, at GitHub.
See [tenant onboarding](../operations/tenant-onboarding.md#bind-a-runner-set-to-a-github-runner-group).

**The scale-set name is a GitHub-side namespace too (Q791, shipped).** The runner group bounds who may *target* a set; this bounds who may *be* it.
A `ScaleSet` set's first `runnerLabel` is its scale-set name, and the AGC adopts a scale set **by name** against the Actions service its gateway's `githubURL` reaches, creating one only when no such name exists.
That name is therefore unique per GitHub org, enterprise, or repo, and a Kubernetes namespace is invisible to it.
Two `RunnerSet`s in different namespaces whose gateways name one org and one first label are two AGCs driving a single scale set, each opening a session on it and acquiring the other tenant's jobs, with that tenant's pods, quota, and egress IP.
It is not only an adversarial shape: [Appendix E.6](appendix-e-capacity-planning.md#e6-when-to-shard-across-installations) shards one org across namespaces and asks for consistent labels, which is exactly the collision.

Admission enforces it cluster-wide, and, like the Q322 `noProxyCIDRs` guard, from both sides, because the pair is assembled from two objects: the label lives on the `RunnerSet`, the scope on its gateway.
A `RunnerSet` write resolves its own gateway's scope and rejects a first label already claimed in it; an `ActionsGateway` write resolves the scope its referrers would land in.
The gateway half is what makes the guard hold rather than merely usually hold: a `RunnerSet` applied before its gateway has no scope to resolve and is admitted (§H.7), so without it the guard is bypassable by apply order alone.
Two sets naming one gateway still collide even before that gateway exists, since they share whatever scope it turns out to bind.

Scopes compare case-insensitively on host and path, matching how GitHub resolves owner names, and the port is dropped; both err toward rejecting, the safe direction for a guard against sharing.
The rejection names the conflicting set only when it sits in the applying tenant's own namespace; a cross-tenant holder is withheld from the API error and written to the GMC log, so the message cannot be used to enumerate other tenants' namespaces and label usage.
`Classic` sets are unaffected throughout: they register no scale-set object, so they claim no name.

**A pair admission never saw is reported at reconcile (Q849, shipped).** A webhook fires on a write, so a collision already stored when the guard shipped is never re-validated: an upgrade from a release before Q791, or a window with the webhook uninstalled.
The GMC's gateway reconciler reads the same inventory each reconcile and reports it on the advisory `ScaleSetNameCollision` condition, a Warning Event on the transition (including the absent→`True` first observation, which is the upgrade case), and the `actions_gateway_scale_set_name_collision` gauge.
It does not gate `Ready` and does not block provisioning: GAG cannot pick which tenant loses the name, and refusing to run the AGC would take down both.
The same non-enumeration rule applies to the condition message, which is tenant-readable; an unreadable inventory leaves the last verdict in place rather than reporting a scope clean on a read that did not happen.
See [§5.2](05-security.md#cross-tenant-job-acquisition-via-a-shared-scale-set-name).

**Per-gateway AGC resources — `agcResources` (Q171, shipped).** The AGC control-plane container is sized by an optional `ActionsGateway.spec.agcResources` of the standard `corev1.ResourceRequirements` shape.
It is an additive, per-key overlay of the platform default — the [Appendix A](appendix-a-capacity-slos.md) sizing (`requests {cpu: 500m, memory: 2Gi}`, `limits {cpu: 2, memory: 4Gi}`): the GMC stamps the default and replaces only the request/limit keys the tenant sets, so an unset field reproduces the default unchanged (non-breaking) and a value that sets one knob keeps the default for the rest.
There is no admission-time floor on the values — sizing guidance and the recommended floor (don't set a memory limit below the working set; don't request more than a node/quota can schedule) are operator-owned in [tenant-onboarding](../operations/tenant-onboarding.md#tuning-agc-control-plane-resources). v1alpha1 has no equivalent field; its AGC carries no GMC-stamped resources (unchanged).

**GHES private-CA trust — `githubCABundleRef` (Q536, shipped).** The AGC and the worker pods it provisions trust the OS system roots plus the per-tenant egress proxy's own CA, so a GitHub Enterprise Server appliance fronted by an internal certificate authority failed the TLS handshake on every call — and no CRD field, Helm value, or GMC flag could extend that trust.
`ActionsGateway.spec.githubCABundleRef` names a `ConfigMap` in the gateway's namespace holding a PEM bundle under `ca.crt`.
A `ConfigMap` rather than a `Secret` because a certificate is public material; the field is the tenant's because it is the same party that supplies `githubURL`, and widening trust reaches only that tenant's own AGC and worker pods.
The bundle is **additive** — `BuildTrustPool` seeds from the system roots and appends every supplied PEM — so a gateway on public GitHub with a bundle set behaves identically, and replacing the roots is not offered.
The GMC mounts it on the AGC `Deployment` and stamps `GITHUB_CA_CONFIGMAP_NAME`; the AGC's pod provisioner projects the same `ConfigMap` into each worker pod, where the wrapper concatenates it with the image's own trust store behind `SSL_CERT_FILE`, so the control plane and the runners trust the same appliance.
Resolution is a runtime condition per §H.7, not admission: a missing or unparseable referent degrades the gateway (`CABundleNotFound`/`CABundleInvalid`) and provisions no AGC, rather than one whose pod would sit at `ContainerCreating`.
The read is uncached and re-polled — the GMC's `ConfigMap` informer is name-pinned to its own namespace so it needs no cluster-wide `ConfigMap` read, which rules out a watch.
It does **not** cover egress reachability; the appliance's address space is still the operator's obligation (`GitHubEgressIncomplete`). v1alpha1 has no equivalent field.

**Per-gateway managed right-sizing — `agcAutoscaling` (Q360, shipped).** An optional `ActionsGateway.spec.agcAutoscaling` block has the GMC stamp a `VerticalPodAutoscaler` (`autoscaling.k8s.io/v1`) next to that gateway's AGC Deployment, owner-referenced like every other child.
The block's presence is the opt-in (no `enabled` flag), and `mode` — `Off` (default, recommendation-only), `Initial`, or `Recreate` — is the autoscaler's `updateMode`; upstream's `Auto` is deliberately not exposed because its actuation mechanism is version-dependent.
It **composes with** `agcResources` rather than overriding it: the autoscaler is pinned to `controlledValues: RequestsOnly` so the stamped limits are never moved, an explicitly set `agcResources.request` becomes `minAllowed`, and the effective limits become `maxAllowed`.
The precedence is resolved at reconcile, not rejected at admission — the combination is coherent (sizing plus bounds), and it is made non-silent by the derived bounds on the stamped object, a Normal Event when the autoscaler first appears, and the `AGCAutoscalingUnavailable` condition.
The `autoscaling.k8s.io` CRDs are an optional add-on, so an opt-in on a cluster without them degrades to that condition (`VPACRDNotInstalled`) plus a Warning Event with a bounded 10-minute re-probe — it never gates `Ready` and never hot-loops.
Full rules in [Appendix E §E.11](appendix-e-capacity-planning.md#e11-managed-vertical-right-sizing-of-the-control-planes); the GMC's own equivalent is the chart's `vpa.enabled`. v1alpha1 has no equivalent field.

### Worked example — minimal proxy-less onboarding (three objects)

```yaml
apiVersion: actions-gateway.com/v2alpha1
kind: ActionsGateway
metadata: { name: acme, namespace: team-a }
spec:
  credentials:                              # discriminated union (Q196, §H.15)
    type: GitHubApp                         # discriminator; workloadIdentity is the additive 2nd member (Q197)
    githubApp: { name: acme-github-app }    # LocalSecretReference, same namespace
  githubURL: https://github.com/acme
  # Pod Security level is owned at the namespace (GMC-guarded), not here — see §H.16 #7.
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
  acquisitionProtocol: Classic     # deprecated; the default is now ScaleSet (Q264 P5), which registers every runnerLabel (Q726)
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

This shows the renamed group, the noun/verb split, and that proxy-less is the minimal path — the `EgressProxy` and `RunnerTemplate` reuse only appear when a tenant actually needs a shared proxy or a second runner shape.

## H.5. How each pressure is resolved

- **Object size.** The large `PodTemplateSpec` lives only in `RunnerTemplate`/`ClusterRunnerTemplate`.
  `ActionsGateway` and `RunnerSet` become small, fixed-size objects, and nothing embeds a template anymore.
  Reuse means one 40 KB sysbox template exists once and is referenced N times instead of copied into N runner sets; a `ClusterRunnerTemplate` lets the platform own it once cluster-wide.
- **Shared egress.** Any number of `RunnerSet`s point `proxyRef` at one `EgressProxy`; setting `defaultProxyRef` on the gateway makes every runner set inherit it — one tenant proxy, many runner sets.
- **One namespace, free rebalancing.** Multiple `ActionsGateway`s and `RunnerSet`s are permitted per namespace, all drawing on the single namespace `ResourceQuota`.
  A tenant rebalances by editing small `RunnerSet` objects (`maxWorkers`/`priorityTiers`) — no template churn, no new namespaces.
  PriorityClasses are already cluster-shared, so tiers compose across runner sets.

## H.6. Naming and length budgets

The one rename worth insisting on: **`RunnerGroup` → `RunnerSet`.** "Runner group" is already a first-class GitHub concept (runner groups gate which repos may use which runners), so the current kind name collides with the domain.
`RunnerSet` also aligns with ARC's `AutoscalingRunnerSet`/`EphemeralRunnerSet`.

| New kind | Short | Scope | Role | Derives | Label value |
|---|---|---|---|---|---|
| `ActionsGateway` | `ag` | ns | GitHub binding + AGC control plane | `<ag>-agc` Deploy/SA/Role | `…/gateway` |
| `RunnerSet` | `rs` | ns | scheduling/quota (was `RunnerGroup`) | worker pod `generateName=<rs>-` | `…/runner-set` |
| `RunnerTemplate` | `rt` | ns | pod shape (the large object) | — (referenced) | `…/runner-template` |
| `ClusterRunnerTemplate` | `crt` | cluster | platform golden templates | — | `…/runner-template` |
| `EgressProxy` | `ep` | ns | proxy pool | `<ep>-proxy` Deploy/Svc/HPA/PDB | `…/egress-proxy` |

**Length constraints that actually bite** (RFC 1123): object names ≤ 253, but **label values ≤ 63** and **Service names ≤ 63**.
These CR names become both selector label values *and* `<name>-<suffix>` Service names, so the Service-name path is tightest:

- `EgressProxy` → `<ep>-proxy` Service ⇒ name ≤ **57** (reserve `-proxy`).
- `RunnerSet` → worker pod `generateName` plus the random tail, and the name is also a label value ⇒ ≤ **63**, practically ≤ ~57 for headroom.
- **Recommendation:** put an explicit `maxLength` of **52** on every v2 CR name (leaves 11 for any derived suffix, stays well under 63 as a label value) and document it in the CRD field comment so it is discoverable, not a runtime surprise.

**Shipped, and the v1 gap this closed.** The 52-char cap is enforced by CEL on every v2 CR name, so v2 derivations are bounded at the input.
`v1alpha1` has no equivalent — an `ActionsGateway` name may be 253 characters — and its derived `<gateway>-<runner-label>` `RunnerGroup` name was never bounded at the output either.
Past 63 characters that name is a legal object name and an **illegal label value**, so the `RunnerGroup` reconciled while every worker pod carrying it as `actions-gateway/runner-group` was rejected: the tenant ran no jobs and GitHub reported only that the runner had lost communication (Q473). v1 therefore applies the bound where the name is derived, through the shared `api/apinames` helpers that also own the worker-pod budget (Q467).
The rule generalises: **budget against the tightest consumer of a name (63), not the limit of the object being created (253)** — see [kubernetes-conventions.md](../development/kubernetes-conventions.md#derive-every-name-through-apiapinames-q467-q473).

### Field naming freezes at GA — do the pass now

JSON field names are part of the API contract and become permanent at `v2`.
Do the naming pass during M1 while names are still cheap to change under `v2alpha1`:

- **Acronym/brand casing — decided.** `github` is one lowercased word; trailing initialisms stay uppercase: **`githubURL`, `githubAppRef`** (k8s-consistent with `clusterIP`, `targetCPUUtilizationPercentage`). v1's `gitHubURL` / `gitHubAppRef` casing is *not* carried over.
  Apply the rule to every field and freeze it.
- **References are uniform.** `gatewayRef` / `templateRef` / `proxyRef` / `githubAppRef` share one `…Ref` suffix and the `ObjectRef` / `LocalSecretReference` shapes.
- **List fields are plural** (`runnerLabels`, `priorityTiers`).

Field movement, v1alpha1 → v2alpha1:

| v1alpha1 | v2alpha1 |
|---|---|
| `RunnerGroup` (kind) | `RunnerSet` (kind) |
| `RunnerGroup.spec.podTemplate` + `workerImage` | `RunnerTemplate.spec` (+ `ClusterRunnerTemplate`) |
| `RunnerGroup.spec.{runnerLabels,maxListeners,maxWorkers,priorityTiers, lifecycle}` | `RunnerSet.spec` (unchanged) |
| `ActionsGateway.spec.proxy` | `EgressProxy` (kind) |
| `ActionsGateway.spec.runnerGroups` | removed (explicit `RunnerSet` objects) |
| — | `RunnerSet.spec.{gatewayRef,templateRef,proxyRef,runnerGroup}`; `ActionsGateway.spec.{defaultProxyRef,defaultTemplateRef,defaultRunnerGroup,agcResources,agcAutoscaling}` |

## H.7. Reference integrity — runtime conditions, not admission

Requiring referents to exist at admission time would force apply ordering, which breaks GitOps: Argo CD/Flux (and `kubectl apply -f dir/`) submit the whole set at once and rely on eventual consistency.
A webhook that denies a `RunnerSet` because its `RunnerTemplate` has not synced yet turns a normal reconcile into a failed sync.
So responsibilities split by what admission is actually good at:

- **Webhook keeps (static, order-independent):** structural/shape validation, the cross-field rule (`maxWorkers == priorityTiers[last].threshold`), reserved-pod-field rejection on `RunnerTemplate`, name `maxLength`, reference *well-formedness*, and whether a cross-namespace reference is *permitted by operator policy* at all.

  *Reserved-pod-field split (M2, implemented).* The scalar pod-level reserved fields (`serviceAccountName`, `host{PID,Network,IPC}`, `automountServiceAccountToken`) are CRD CEL rules (M1).
  The per-container checks that exceed the CEL cost budget — an unbounded containers-array walk — are the GMC-hosted validating webhook (M2): it rejects the AGC-injected egress-proxy env vars (`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`/`PROXY_CA_CERT_PATH`, the variables v1 silently overwrote at pod-build time) on every container of **both** template kinds.
  **Privileged containers** are rejected on the namespaced `RunnerTemplate` (a tenant must not self-author a privileged worker shape) but **allowed** on the cluster-scoped `ClusterRunnerTemplate` — that kind is platform-authored (tenants cannot create cluster-scoped objects) and exists precisely to hold golden privileged templates (DinD/sysbox, §H.6).
  A `RunnerTemplate` carries no `securityProfile`, so a v1-style *profile-aware* privileged decision is impossible at the template layer; Pod Security Admission — which stamps the namespace's enforcement level from the effective `securityProfile` (§H.16 #7) — stays the runtime backstop for both kinds, so allowing privileged on the cluster-scoped kind is no weaker than v1.
- **Runtime condition (existence/referential):** does the template exist, does the proxy exist, does the cross-namespace grant exist.
  The controller watches referents and re-reconciles when they appear:

  ```
  RunnerSet status:
    Ready: False
    Reason: TemplateNotFound        # or ProxyNotFound; ProxyShareNotGranted arrives with M4 (§H.9)
    Message: "RunnerTemplate 'dind-large' not found in namespace 'team-a'"
  ```

  Wire it with a watch + enqueue mapper (template/proxy/grant → referencing `RunnerSet`s) so the set flips to `Ready` the moment the missing object syncs.

This stays **fail-closed**: the controller only creates wiring (worker pods, the cross-namespace NetworkPolicy egress, the proxy CA mount) once both the reference *and* any required grant resolve.
A `RunnerSet` pointing at a not-yet-granted cross-namespace proxy simply sits `NotReady` — no traffic is ever permitted in the gap, so moving the grant check to runtime opens no window.

**Fast feedback without the hard reject:** the admission webhook may return a non-blocking **warning** (admission responses carry `warnings[]` without denying) when a reference looks dangling at apply time.
The operator sees `Warning: RunnerTemplate 'dind-large' not found` immediately, the object is still admitted, and the runtime condition remains authoritative.

**Make `gatewayRef` a CRD field selector.** Under multi-gateway, each AGC must list/watch only the `RunnerSet`s whose `gatewayRef` targets its gateway.
Declaring `spec.gatewayRef.name` a `selectableField` (CRD field selectors, KEP-4358) runs that filter server-side instead of fetching every `RunnerSet` and filtering in-process.
Additive, but the watch should be designed around it from the start.

**Status & condition contract — uniform across all five kinds.** Every kind carries `status.conditions` (`listType=map` keyed on `type`), `status.observedGeneration`, and a `Ready` condition with the same polarity and a shared reason vocabulary; messages name the specific blocker (`RunnerTemplate 'dind-large' not found`), never a generic string.
Pin this as the contract in M1 rather than letting each reconciler invent its own.

Alongside the persisted conditions, each reconciler emits Kubernetes **Events** on the object for the meaningful transitions — provisioning start, success, and failure; credential (mTLS/TLS cert) issuance and rotation; and degraded/recovered — so `kubectl describe` reflects what happened, not just the current condition set (Q305).
Events are transition-gated (emitted only when a condition's status actually changes, or once per object for a lifecycle-unique step like the provisioning-start signal), never on every reconcile, mirroring the v1 controllers' Event discipline.
The operator-facing Event reason vocabulary is tabulated in [troubleshooting.md](../operations/troubleshooting.md).

**Child-health rollup onto the `ActionsGateway` (Q304).** The GMC's `ActionsGateway` reconciler rolls the health of the `RunnerSet`s bound to a gateway up onto a `RunnerSetsDegraded` condition — the v2 counterpart of v1's `RunnerGroupsDegraded`, and the operator's single pane.
Because v2 `RunnerSet`s are not *owned* by the gateway (they only reference it via `spec.gatewayRef`, [§H.8](#h8-ownership-gc-and-deletion)), the binding is resolved by matching `gatewayRef.name` within the namespace — the same scoping the AGC applies server-side via its `spec.gatewayRef.name` field selector, not owner labels.
A set is *impaired* (not serving jobs) on either of two axes: its `Ready` is `False` for a non-transient reason — a reference did not resolve or a provisioning step failed, anything but the benign startup `NoActiveSessions`, which is how v2 surfaces the reference/provisioning failures that had no standalone condition — **or** any of its abnormal-is-True impairing conditions is `True` (`v2alpha1.ImpairingConditionTypes()`: `Degraded`, `CredentialUnavailable`, `RunnerVersionTooOld`, `WorkersUnschedulable`).
The second axis is load-bearing because the shared listener pushes `Degraded`/`RunnerVersionTooOld` onto the `RunnerSet` independently of `Ready` (Q330): a classic set whose sessions are all rejected as unauthorized converges to `Ready=NoActiveSessions` while `Degraded=True` sits on its own condition, so an `Ready`-only rollup would silently miss it.
The advisory conditions (`RateLimited`, the `WorkerQuota` ladder, `EgressUnattributed`, `PossibleReapBlockingSidecar`, `JobProvisionStalled`, `RunnerLabelsIncomplete`) are excluded so the rollup does not flap on normal load.
`RunnerLabelsIncomplete` (Q726) is excluded for a different reason from the rest: it does not flap at all, it simply is not an outage.
The set serves every job targeting the labels that did register, and the ones that did not are a configuration mismatch for the tenant to fix.
Like the v1 rollup it is advisory — it does **not** gate `Ready`, since the gateway's own AGC control plane can be healthy while one tenant's set is impaired.
The GMC watches bound `RunnerSet`s (predicated on a change to a set's impaired signature, dropping high-frequency `activeSessions`/ `pendingJobs` churn) so a child's health change refreshes the parent promptly.

**Observed runner version on the `RunnerSet` (Q792).** Q715 judges the worker image *reference* against GitHub's enforced minimum, so a digest-only or custom tag reports `WorkerImageVersionUnknown`: the reference declares no version and the AGC asks GitHub nothing.
The injected wrapper has always read the real version from the runner's own dependency manifest, and only logged it.
It now hands that back on the pod's termination message, and the reconciler publishes the last one it saw as `status.observedRunnerVersion`.

The harvest hangs off the reap walk, which already lists exactly the set's worker pods and already switches on terminal phases, so it costs no extra `List`.
It fires for every terminal pod the walk sees, reaped that pass or not, so the observation does not depend on reap timing.
Newest wins, because a set whose `workerImage` changed has pods of both versions retained at once; and the field is sticky, because once every terminal pod has aged past `completedPodTTL` the walk sees no reports at all and clearing would flap the field on the reap cycle for an answer that is a property of the image rather than of which pods are retained.

**It is a self-report and deliberately not verdict-bearing**, which is the whole design question rather than a caveat on it.
The runner container runs the tenant's image and the job's own steps run inside it, so anything there can rewrite the report before the container terminates and kubelet reads whatever is last.
Nothing inside the container can do better: a shared `emptyDir`, the manifest file itself, and a `shareProcessNamespace` sidecar reading `/proc/<pid>/root` are all tenant-writable, and an init container cannot see the runner image at all.
So `RunnerVersionTooOld` keeps reporting `WorkerImageVersionUnknown` rather than turning a tenant-controlled value into a verdict, on the same argument that already makes `Unknown` not `False`: reporting "current" for an image nothing has checked would be worse than saying so.
The attestable form reads the image from the registry before the container runs, which also covers the digest-only case and needs no pod; it is [Q988](../queue/Q988.md), and it is a registry-integration feature rather than a status field.

**Advertised capacity on the `RunnerSet` (Q721).** The scale-set tier states its whole admission ladder as one integer per long-poll (`X-ScaleSetMaxCapacity`, [operational flows](04-operational-flows.md#the-ladder-as-an-integer-scale-set-tier-q443)), and until Q721 that number and its per-rung breakdown reached Prometheus alone.
That makes the answer to "why is my intake throttled?" available to whoever can query metrics, which is the platform operator, and not to the tenant who owns the `RunnerSet`: the party whose `maxWorkers` or workflow shape is usually the thing to change.
So the reconciler publishes the same accounting on `status.advertisedCapacity` and `status.withheldCapacity`, readable with `kubectl describe`.

`advertisedCapacity` is a **pointer** because zero is a real advertisement, intake fully withheld, and has to be distinguishable from "nothing advertised yet", which is what a classic-tier set and a listener that has not polled both report.
`withheldCapacity` is a `listType=map` keyed on `reason`, carrying every rung the poll *evaluated* including the ones withholding nothing, so an absent reason means not-evaluated rather than not-binding: the same explicit-zero contract the withheld gauge carries, for the same reason (the two are indistinguishable in an absent series and one of them is a bug).
Its `reason` is deliberately **not** a CRD enum.
The ladder is designed to grow a rung at a time and every rung must land in both acquisition tiers at once, so a closed enum would couple each new rung to a CRD change; the field is controller-written status a tenant cannot author, so validation buys nothing against anybody.

Two properties keep it from costing what high-frequency status usually costs.
The value is recomputed per poll but published per reconcile, on a status write that already happens unconditionally, so it adds no API writes and lags the live number by at most one reconcile.
And the GMC's rollup watch is unaffected: `runnerSetImpairmentChanged` derives its signature from `status.conditions` alone, an allowlist rather than a list of fields to ignore, so an update carrying only a new advertisement is dropped without anyone having to remember to exclude it.

**Measured sizing recommendation on the `RunnerSet` (Q359 Phase 2).** The AGC's usage sampler aggregates per-job CPU/memory peaks per worker container (Phase 1's metrics), and the `RunnerSet` reconciler surfaces the derived per-container recommendation in `status.sizingRecommendation`: recommended `requests` (p95 of per-job peaks), a recommended memory `limit` (observed max × headroom, never a CPU limit), the observed p95/max, a `sampleCount` confidence signal, and the window start.
The field is **advisory and doubly load-bearing as the aggregate store**: the sampler re-seeds its in-memory histograms from it on AGC restart (95% of the persisted mass at the p95, the rest at the max — exactly preserving the two statistics the recommendation uses), so the observation window survives control-plane rollouts with no separate backing store.
The reconciler never overwrites the field with an empty snapshot (a warming-up sampler must not wipe the store).
Alongside it, the advisory abnormal-is-True `SizingDrift` condition fires when the resolved template's effective ask (declared resources, or the provisioner's 500m/1Gi gap-fill defaults when it declares none) deviates materially: a request ≥2× the recommendation (waste) or a memory limit below the observed per-job peak (OOM risk).
It is judged only at ≥20 sampled jobs, never gates `Ready`, and is excluded from the impaired rollup above — a sizing hint, not a health signal.

**Opt-in actuation: `spec.sizing` profiles (Q359 Phase 3).** A `RunnerSet` may opt into having the AGC apply the measured values at pod-build time: `sizing.profile` selects `Static` (default — template authoritative, today's behavior), `Binpack` (requests == limits from the history → Guaranteed QoS, maximum workers per expensive node), `Throughput` (requests from the history, no CPU limit, memory limit = observed peak × `limitHeadroomPercent`), or `NodeShare` (runner-container requests = a *declared* per-node allocatable ÷ `workersPerNode` — declared, because the AGC is deliberately namespace-scoped and never reads Node objects; needs no usage history).
The transform runs in `runnerSetTarget.Resolve` per acquired job — the Q117 no-restart property — and reads its input from the persisted `status.sizingRecommendation`, the same store the sampler re-seeds from, so actuation and status can never disagree.
Rails: only cpu/memory keys are ever written (extended resources byte-identical); `Binpack`/`Throughput` are whole-pod and fall back to `Static` until every template container is drift-confident (`status.sizingProfileState`: `Active`/`AwaitingSamples`); optional `minRequests`/`maxRequests` clamp every derived value.
Because only cpu/memory are ever written, a CEL rule on `nodeShare.allocatable` requires at least one of the two keys (Q484): an envelope carrying neither — empty, or the GPU key alone, which is the realistic mistake since GPUs are what the profile bin-packs *against* — derives nothing while `sizingProfileState` still reports `Active`.
That rule is a **within-object** invariant, which is why it belongs at admission where the cross-object conflicts below do not.
Quota/LimitRange conflicts are deliberately a **runtime** signal, not an admission gate — cross-object admission validation is exactly what this appendix's §H.7 philosophy avoids, and both objects are platform-owned and can change after the `RunnerSet` is written.
A quota conflict surfaces through the existing `WorkerQuota*` conditions and quota retries.

One conflict rejects *nothing* and so needed its own signal (Q489): `Throughput`'s mechanism is the ABSENCE of a CPU limit, and any admission mutation that supplies one — a `LimitRange` `Container` cpu default, a mutating webhook, a policy engine — cancels the profile while the pod is admitted, `sizingProfileState` still reads `Active`, and bursting is gone.
The AGC reports it as the advisory `SizingProfileOverridden` condition, Throughput-only and removed under every other profile (they all set a CPU limit, leaving nothing to inject).

**It reports the effect, not the cause**, and that is the load-bearing choice.
Reading the `LimitRange` — the first design — infers an effect from one policy and is blind to every other injector.
Instead the AGC marks each profile-derived pod (`actions-gateway.com/sizing-profile`) and compares what it built against what the apiserver admitted.
Worker pods are already granted, listed, and watched, so this needs **no new RBAC, no LimitRange informer, and no polling** — and the existing worker-pod watch makes it event-driven for free: the pod event carrying the injected limit is itself what re-reconciles the set.
The trade is that the signal is post-hoc — it arrives with the first pod the profile builds, since a pod is what it reads.
While a profile is `Active`, `SizingDrift` reports `False/SizingProfileActive` (the template ask is not what pods run with).

**Why `profile` bundles two mechanisms deliberately (Q481).** The 1.3 [pre-release API review](../development/api-review.md) asked of `sizing.profile` the question Q470 asked of `capacityGate.mode` — *does this enum answer exactly one question?* — and mechanically it does not.
Two axes run through it: where the request comes from (the template, the usage history, a declared node envelope) and what limits follow (the template's, `requests == limits`, or no CPU limit plus a memory headroom).
The tell the review names is present twice over: `nodeShare` is only meaningful under `NodeShare`, `limitHeadroomPercent` only under `Throughput`.
Two cells of the cross-product have no profile of their own and are plausible wants — a **Guaranteed node share** (the GPU case, where predictable packing is the whole point) and **history-derived requests under hand-set limits**.
It ships bundled anyway, on three grounds:

- **The cost that made Q470 worth a break is absent.** What forced the gate's split was *whose fact it was*: `SchedulerVerdict` asked each tenant to assert a property of infrastructure they may not own, putting the feature's one harmful misconfiguration in the hands of the party least equipped to avoid it.
  Both of `profile`'s axes are the set owner's own choice, so only the two weaker costs (value multiplication, values that read as redundant) remain.
- **The axes are not orthogonal, so splitting them would not remove the CEL rules.** `Throughput`'s memory limit is *observed peak* × headroom, and an observed peak exists only under the usage source.
  A split shape still needs `memoryHeadroomPercent is only meaningful when the source is the usage history` — the same tell, relocated — unless the derivation is redefined off the peak, which means re-deriving a rule live-validated on the dogfood tenant days before this tag (Q449).
- **Both cells are additive, and one of them is already reachable.** Filling either costs one field defaulted to today's behavior (`nodeShare` gaining an opt-in `requests == limits`, say): no conversion shim, no deprecation window, any minor release.
  Q470 had to beat its tag because its fix *removed* enum values.
  This one does not, so the tag freezes nothing a later release cannot add.
  A Guaranteed node share, moreover, is reachable **today** as a side effect of the limit-lift rule: a template limit at or below the derived share is raised to it, so a runner container whose template limits sit under the envelope comes out with `requests == limits` (`TestApplySizingProfileNodeShareLiftedLimitsReachGuaranteed`).
  That is a side effect of a guard against admission rejection, not an expressed intent — which is the argument for eventually adding the explicit field, and the argument against spending a breaking reshape on it now.

**Read `profile` as an intent enum, not a mechanism enum.** All four values name what the operator wants — leave it alone, pack tightly, finish fast, split the node — and the mechanism follows from the intent.
That is also the rule for extending it: a new value must name a *distinct operator intent*; a variation that merely recombines existing mechanisms belongs in a sibling field under the profile it varies.
Nothing is reserved on either axis today, the other contrast with the gate, which had `Probe`/`Provision` waiting on axis 1 and `ProvisioningRequest` availability on axis 2 before Q470 split it.
In-place pod resize — the one extension the plan anticipates — changes *when* actuation happens, not where values come from or what limits follow, so it is a sibling field rather than a fifth profile.
If the intent framing ever fails, `v2.0.0` is already a scheduled break.

**Opt-in intake gating: `spec.capacityGate` (Q405, Q406, Q470).** A `RunnerSet` may opt into the placeability rung of the admission ladder — the AGC refuses to take on jobs whose worker pod the cluster cannot currently place, instead of claiming them and stalling.
`capacityGate.mode` selects `Off` (the default, today's behavior exactly) or `Observe`.

The values name *how the AGC learns* the cluster cannot place a worker, not merely whether the gate is enabled (Q476).
`Observe` decides from evidence an already-stuck pod produced; the reserved `Probe`/`Provision` (Q407) solicit an answer instead.
Every value but `Off` refuses jobs — there is no report-only tier on this axis.

**Two axes, two owners.** The mode is deliberately *not* a choice of signal:

| Object | Field | Owner | Answers |
|---|---|---|---|
| `RunnerSet` | `spec.capacityGate.mode` | The tenant | Should this set refuse work it cannot run? |
| `ActionsGateway` | `spec.clusterCapacity.nodeAutoscaling` | The platform operator | Does anything in this cluster add nodes? |

The second selects the signal the gate may trust: `Absent` ⇒ the scheduler's `PodScheduled=False`/`Unschedulable` verdict (Q405), `Present` (the default) ⇒ the cluster autoscaler's own declination, read as an Event on a stuck worker pod (Q406).

This is the shape Q470 corrected.
The first version encoded the asymmetry as two modes on the `RunnerSet` (`SchedulerVerdict`/`AutoscalerVerdict`), which conflated two independent axes in one enum and — worse — asked each *tenant* to assert a fact about infrastructure they may not own.
In a multi-tenant gateway the person writing the `RunnerSet` is routinely not the person who knows the node contract, and the one genuinely harmful misconfiguration this feature has (scheduler-verdict gating on an elastic cluster) was reachable by exactly that mistake, prevented only by documentation.
Moving the fact to the object the platform owns makes that combination **unrepresentable**: no value a tenant can write produces it.
It also stops the enum growing on an orthogonal axis — `Probe`/`Provision` (Q407) extend the *same* axis by soliciting an answer rather than observing one, and the ProvisioningRequest API's availability is another cluster fact that belongs in `clusterCapacity`.

`Probe`/`Provision` are reserved names on the mode enum and are **rejected at admission** until they ship: an operator who selects a gate expects gating, and silently accepting an unimplemented mode as a no-op is the failure this rung exists to remove.
An unrecognized mode that nonetheless reaches the reconciler — the CRDs ship as their own chart and can be upgraded ahead of the AGC — fails **open** with `WorkerCapacityDeclined=False/GateModeUnsupported` rather than falling through to an implemented mode, because guessing would apply semantics the operator did not ask for.

The cluster fact is an **operator assertion, never an auto-detection**, because the two readings of an unschedulable pod are opposites and only the operator knows which applies.
On a fixed-size cluster nothing is waiting on that pod, so it is pure waste; on an elastic one the pod *is* the request for a node, and gating on it would suppress the very signal that would have rescued the tenant.
A wrong auto-detection starves a tenant, so the AGC does not attempt one ([§D.8](appendix-d-alternatives-considered.md#d8-gating-intake-on-capacity-which-signals-are-safe-to-gate-on)).
Nor does it infer the fact from silence: an autoscaler is legitimately quiet during backoff, during a cooldown after a failed scale-up, or for a pod it filters out of its evaluation, so "no autoscaler events have appeared" is absence of evidence — and the rest of this rung treats absence of evidence as *don't gate* everywhere else.

**`Present` is the default because the two mistakes are not symmetrical.** Under `Present` the gate refuses intake only on an explicit declination, so a missing or wrong value can only ever under-gate, which is today's behavior.
Under `Absent` it gates on the scheduler's verdict alone, so a wrong value there refuses jobs a cluster was about to grow for.
The default therefore has to be the one that fails toward today's behavior, per the secure/conservative-default stance.

**How the elastic-cluster signal is read.** Both open-source autoscaler projects emit their verdicts from shared core code every cloud provider vendors — cluster-autoscaler `NotTriggerScaleUp`/`TriggeredScaleUp`, Karpenter `FailedScheduling`/`Nominated` — so ~46 provider implementations collapse to two event vocabularies and no provider abstraction is warranted.
Three properties make the read safe rather than merely correct:

* **The reporter, not the reason, discriminates `FailedScheduling`.** It is kube-scheduler's own reason as well as Karpenter's, so it counts as a declination only when it came from a reporter that is not the pod's own `spec.schedulerName` (nor `default-scheduler`).
  An unattributable event does not clear that bar.
* **The verdict is the newest relevant event, not merely the existence of a declination.** An autoscaler that declined on one loop and scaled up on the next is rescuing the tenant, and a stale declination must not gate against that.
  Recency is asymmetric, because one loop can emit both verdicts for one pod: a scale-up supersedes a declination immediately, while a declination supersedes a scale-up only from more than a second later — inside that window the pair is one loop's own output, and resolves open.
  Measured, not assumed: cluster-autoscaler v1.36.1 emitted `TriggeredScaleUp` and then `NotTriggerScaleUp` for the same pod 4ms apart ([plan §9c](../plan/capacity-aware-intake.md#9c-the-live-autoscaler-harness-and-what-it-measured-q474)).
  The window is what makes that read a property of the autoscaler's behavior rather than of its recorder's resolution, which is the difference between the two generations in the field.
* **Reads are uncached and doubly scoped** — field-selected to one pod, and only for pods the `WorkersUnschedulable` evaluation already found stuck past the scheduling grace, bounded per reconcile.
  There is no Event informer: Events are the highest-churn object in a cluster, and caching them would tax every AGC to serve a mode most sets never enable.
  A healthy set costs zero reads.

An autoscaler whose vocabulary is not recognized (a commercial optimizer, or none at all) produces no match, which is `declined=false`, which is today's behavior.
That asymmetry is the safety argument: a missed match costs nothing, a wrong match starves a tenant, so the matcher stays deliberately narrow and broad coverage is explicitly not a goal.
If a specific proprietary autoscaler is ever asked for, the extension point is data rather than code — an operator-settable list of extra `(reason, reportingController)` pairs, following the `--allowed-infra-priority-classes` allowlist pattern.

The decision is published as `WorkerCapacityDeclined` and the rung reads that condition back rather than re-deriving the verdict, so `kubectl describe` and the AGC's intake behavior cannot disagree.
Its `reason` names the signal that decided — `PodsUnschedulable`, `ScaleUpDeclined`, `PodsNotStarting` (the kubelet's verdict on a pod that bound and never started, Q714), the latched `AwaitingProbe` (the declined pods were reaped and the decline is retained until a probe resolves it, Q512), or the `False` reasons `CapacityAvailable` and `GateModeUnsupported` — so an operator can tell which rung stopped their jobs and on what evidence.
The first two are selected by `clusterCapacity.nodeAutoscaling`; the third is not selected by anything, because a pod that is already placed is not waiting on an autoscaler and no node changes whether its image pulls.
It is a **separate** condition from `WorkersUnschedulable` even though the fixed-size-cluster signal shares its source: it means something different to an operator ("intake is being refused" versus "pods are stuck"), it stays stable while the signal underneath it changes — the autoscaler-declination source (Q406) is exactly that, a new source under an unchanged condition, rung, and metric label — and `WorkersUnschedulable` is already an impairing rollup input — so `WorkerCapacityDeclined` is deliberately **excluded** from `ImpairingConditionTypes()`, which would otherwise double-count one stall into the gateway's `RunnerSetsDegraded` summary.
A set that did not opt in carries no such condition at all.

Two properties bound what the gate is worth.
It does **not** eliminate the first wasted claim — the signal is derived from a stuck pod, so one has to exist — and it does not remove the need for `pendingPodDeadline` and the reaper.
What it bounds is the *rate*: reaping the pod latches the condition as `AwaitingProbe` rather than clearing it (Q512 — clearing restored the scale-set tier's whole advertisement each window, measured as a no-op), one probe job is admitted, and a still-unplaceable shape trips the live verdict again, so a burst of *N* wasted claims becomes roughly one per deadline window on both acquisition tiers.
Per-pool behavior falls out of per-object keying rather than extra machinery — a verdict is only valid for the pod shape that produced it, and a `RunnerSet` resolves to exactly one worker template (§H.4), so a drained GPU pool gates the GPU sets while CPU sets keep claiming.
Fail-open throughout: an unreadable set, an unresolved template chain, an unreadable pod list all leave intake exactly as it is today.
The gate may under-gate freely; it must never over-gate.

## H.8. Ownership, GC, and deletion

Shared objects must not be owner-referenced by their referrers:

- **`EgressProxy`** is standalone and owns its *own* children (the proxy Deployment/Service/HPA/PDB/NetworkPolicy, a self-signed proxy TLS Secret, and the metrics-mTLS bundle below, reconciled by the GMC).
  Nothing owns the `EgressProxy`.
  Each child is derived as `<ep>-proxy` (the TLS Secret as `<ep>-proxy-tls`) and carries the per-`EgressProxy` identity label `actions-gateway.com/egress-proxy: <name>`; it is the **sole** key of every Deployment / Service / PDB / NetworkPolicy selector and of the pod anti-affinity term.
  This is load-bearing three times over: it keeps multiple proxy pools in one namespace selector-isolated (v1 could assume one proxy per namespace); it keeps a pool clear of a coexisting *v1* pool, which selects on the bare `app: actions-gateway-proxy` a v2 pod deliberately does not carry (Q582 — sharing it put each pool's pods under the other's PDB, wedged both HPAs on `AmbiguousSelector`, and made the pools repel each other off every node); and because each pool is now its own Deployment, proxy metrics carry the proxy identity automatically.
  Same-namespace only at M2; cross-namespace sharing is M4.
  - **Proxy metrics-mTLS + ServiceMonitor (Q324, at classic parity).** The proxy serves `/metrics` over mutual TLS on `:8443`, requiring a scraper client cert signed by this `EgressProxy`'s *own* metrics CA (never shared with the AGC or another tenant).
    The GMC issues a per-`EgressProxy` PKI and writes a server bundle Secret `<ep>-metrics-tls` (mounted into the proxy) and a scraper client bundle `<ep>-metrics-client` (published for monitoring); the NetworkPolicy admits the `:8443` scrape only from `metrics: enabled` namespaces.
    A `<ep>-proxy-metrics` `ServiceMonitor` is provisioned when `--enable-tenant-service-monitors` is on (graceful `ServiceMonitorCRDMissing` handling otherwise).
    This is secure-by-default parity with the classic proxy — without the mounted bundle the proxy binary would fall back to serving `/metrics` plaintext on the health port, so mounting it is what prevents an unauthenticated-metrics regression.
    Both metrics Secrets carry an owner reference and are GC-delegated (the GMC holds no Secret delete verb).
- **`RunnerTemplate`** is pure data — no children, nothing owns it.
- **`ActionsGateway` teardown is fail-closed, not GC-trusting (Q328, ports v1's Q125).** The gateway's namespaced children all carry a controller owner reference, so cascade GC *would* eventually remove them — but a transient GC failure is silent and unretried, and the ClusterRunnerTemplate ClusterRoleBinding (cluster-scoped, cannot be owned by a namespaced CR) has no GC at all.
  So the gateway holds a cleanup finalizer and its delete path deletes every child explicitly, then **verifies each one is gone**: a delete error or a child lingering under a foreign finalizer retains the gateway's finalizer, emits a `TeardownIncomplete` Warning event naming the blocker, and requeues until teardown is verifiably clean.
  The metrics mTLS Secrets are the one GC-delegated exception (the GMC holds no delete verb on Secrets by design).
  Bound `RunnerSet`s are referrers, not children — they degrade per the next bullet.
  The gateway reconciler is also serialized (`MaxConcurrentReconciles: 1`), matching v1's single-writer assumption.
- **Deletion degrades, it does not block — and uses no finalizer at all.** Hard-blocking deletion of a still-referenced shared object via finalizer would fight GitOps prune the same way an ordering webhook does; Kubernetes' own finalizer guidance also warns that finalizers on shared/referenced objects are a common cause of stuck-`Terminating` resources.
  So allow the delete and flip referrers to `Ready=False, Reason=TemplateDeleted` (`ProxyDeleted` for a deleted `EgressProxy`; same fail-closed behavior — no template ⇒ no new pods) via the referent→referrer watch.
  The deletion-specific reason is distinguished from `*NotFound` by the referrer's own status markers (`templateSource` / `proxyMode: Proxied`) under an unchanged spec generation.
  Do **not** keep a finalizer even for bookkeeping: `.status.referencedBy` is computable from the same informer/watch without taking on a finalizer that can block deletion.

## H.9. Cross-namespace proxy sharing

**Shipped (Q166, M4).** The mechanisms below are implemented; two points this section originally left open are recorded at the end.

Default is same-namespace.
Cross-namespace sharing uses **provider consent**: the owner of the `EgressProxy` publishes that it is shareable (via `spec.sharing.allowedNamespaces` or a namespace selector).
Naming a cross-namespace proxy from the consumer side is not sufficient.
This mirrors the consent handshake of Gateway API's `ReferenceGrant` (GA / `v1`), where the grant lives in the *target* (provider) namespace and a consumer-side name alone never authorizes the reference.

**v2 ships the inline allowlist only.** It needs no Gateway API CRDs installed (lower onboarding), and honoring a `ReferenceGrant` when Gateway API *is* present can be added later without a breaking change.
The load-bearing principle taken from the precedent — **consent is always provider-side** — holds for both.

A shared proxy is a **shared egress identity** — it is for *cooperating* tenants or a *platform-operated* central pool, not mutually-distrusting tenants, because sharing surrenders the per-tenant egress attribution the proxy exists to provide.

Cross-namespace sharing forces two mechanisms that same-namespace sharing does not, and these are the bulk of the implementation cost:

1. **NetworkPolicy on both sides.** The GMC must add egress (consumer workers → provider proxy Service) *and* ingress (provider proxy ← consumer namespaces) whenever a grant is active.
   Today both policies assume the proxy is colocated.
2. **Proxy TLS CA distribution — a ConfigMap, not a secret.** A cross-namespace consumer needs the proxy's CA *certificate* (public) to validate the tunnel — never the private key, which stays in the proxy namespace.
   So this is trust distribution, not secret replication: the GMC writes the CA as a **ConfigMap** into only the granted consumer namespaces, scoped by the same grant that authorizes the reference.
   This follows the cert-manager **trust-manager** pattern (selector-scoped bundle sync); if trust-manager is installed, the CA may instead be published as a `Bundle`.
   No new secret-distribution mechanism is required — the earlier framing of this as a "secret" overstated the cost.

### What implementing it settled

**The reference needed a namespace, and it could not go on `ObjectRef`.** Nothing in the v2 API could express a cross-namespace reference at all: `ObjectRef` and `LocalObjectRef` were name-only, and both resolution sites resolved in the referrer's own namespace.
`ObjectRef` also backs `gatewayRef`, `templateRef` and `defaultTemplateRef`, so adding `Namespace` there would have made four references cross-namespace at once, three of them with no consent handshake behind them.
The proxy references therefore moved to their own `ProxyObjectRef` (`name` + optional `namespace`).
Empty `namespace` means the referrer's own, so every existing manifest keeps its meaning.
This dropped the optional `kind` property from `proxyRef`'s schema, whose enum admitted only the two template kinds and which nothing read; structural-schema pruning means a manifest that set it still applies.

**The AGC cannot read a remote `EgressProxy`, so the GMC mediates.** The AGC's cache is pinned to its own tenant namespace precisely so it needs only the per-tenant `Role` the GMC creates rather than a `ClusterRole`, and `RunnerSet.spec.proxyRef` is resolved by the AGC.
Widening that to a `ClusterRole` would let every tenant's AGC read every other tenant's `EgressProxy`, a blast-radius regression, and the secure-by-default rule keeps the narrower option.
A per-grant `Role` plus an uncached read preserves least privilege but loses the watch, since controller-runtime fixes cache namespaces at manager construction.

So the CA ConfigMap this section already mandates carries the decision as well as the trust material: **its presence is the grant**, and its data holds the proxy's Service DNS name and port.
The AGC reads it from its own namespace, resolves nothing across a boundary, and fails closed when it is absent.
Revoking a grant deletes it.
That is one mechanism doing two jobs rather than a second mechanism invented for the purpose.

One consequence worth stating: the GMC reads these projections through its **uncached** API reader.
Its ConfigMap informer is pinned to a single name in its own namespace, so a cached label-selected list would return nothing and the prune that revokes a withdrawn grant would silently find nothing to delete.

## H.10. The egress proxy becomes optional

The proxy earns its keep for **stable per-tenant egress IPs** (GitHub IP-allowlisting, common with Enterprise Managed Users), **egress attribution / incident containment**, and **avoiding shared-NAT throttling** when many tenants reach GitHub from one IP.
A small single-tenant cluster whose node egress IPs are already acceptable needs none of that.

So `proxyRef`/`defaultProxyRef` are both optional; unset ⇒ **direct egress**.
Onboarding collapses to three objects — one `ActionsGateway`, one `RunnerTemplate`, one `RunnerSet` — with no proxy object at all.
This is **shipped** (Q168): a v2 `ActionsGateway`/`RunnerSet` with no proxy reaches Ready with direct egress; the worked example in §H.4 is valid as written.

**Secure-by-default guardrail (signed off).** The proxy bundles two properties: egress *identity* (IP attribution) and egress *restriction* (traffic can only reach GitHub).
Dropping the proxy drops *identity*, but it does **not** drop *restriction* — this trade was raised and signed off, and the shipped behavior holds the line:

- The **NetworkPolicy egress lockdown stays mandatory and on by default** even with no proxy — default-deny egress, allow only DNS + GitHub CIDRs (+ kube API for the AGC).
  Direct egress is still IP-restricted egress; there is no proxy-less mode in which a worker or AGC can reach arbitrary internet.
- The **managed GitHub-IP refresh loop**, which previously hung off the proxy, now runs at the gateway level: the GMC's `IPRangeReconciler` patches each direct-egress gateway's AGC + workload NetworkPolicies (as well as the proxied EgressProxy policies) so the direct-egress allowlist stays current as GitHub rotates ranges.

With those two in place, defaulting the proxy off loses only per-tenant *IP identity* — a property a subset of tenants need and opt into by attaching a proxy — not the egress *containment* baseline.
Defaulting off the *restriction* would be a security regression and is out of scope.
See the [secure-by-default principle](05-security.md) for the rule this satisfies.

**Live enforcement is proven, not just shaped (Q178).** Envtest proves the direct-egress NetworkPolicies carry the right shape but has no CNI, so it cannot prove the lockdown is enforced.
The `E2E_V2_DirectEgress` kind e2e closes that gap: a proxy-less worker pod reaches `api.github.com` directly (positive, both CNI legs) while a connection from the same workload network context to a non-GitHub destination is dropped by the default-deny egress NetworkPolicy (negative, Calico-only — kindnet does not enforce egress drops, so the block self-skips there).
See [§7.3 of the test plan](07-test-plan.md#73-end-to-end-tests).

Two refinements keep direct egress **auditable**, not silently inferred (both shipped):

- **Direct egress is a structurally explicit state.** An unset `proxyRef` resolves to direct egress, and the gateway/runner-set status records `proxyMode: Direct` rather than leaving "no proxy" to be inferred from an absent field.
- **Surface the attribution trade.** The proxy-less gateway and runner set carry an advisory `EgressUnattributed` condition (True), so an operator sees at a glance that this workload has no per-tenant egress identity — the property they opted out of — without grepping specs.
  It is advisory only and never gates Ready.

**Composition bonus.** §H.9 and §H.10 combine: a platform team runs one shared `EgressProxy` in a central namespace, grants it to the EMU/allowlist tenants who need stable IPs, and everyone else runs proxy-less.

## H.11. Migration (v2, tool-assisted)

A conversion webhook cannot do this migration.
Conversion webhooks convert one object **in place**; they cannot create sibling objects.
Splitting one `ActionsGateway` (with inline proxy + bootstrap groups) into `ActionsGateway` + `EgressProxy` + N `RunnerTemplate`s + N `RunnerSet`s is a *fan-out on create*, which no conversion webhook can express.
Therefore:

- Serve `v1alpha1` and `v2alpha1` side by side (no automatic conversion of the split fields).
- Ship a one-shot **migration tool** (a `kubectl` plugin or subcommand) that reads v1 CRs and emits the v2 object set — extracting each inline `podTemplate` into a `RunnerTemplate`, the inline `proxy` into an `EgressProxy`, and rewriting references.
  Dry-run to manifests by default; apply on `--apply`.
- Deprecate `v1alpha1` after a release or two.
  The cutover is deliberate, not silent, because the migration is fan-out-on-create.

**Shipped (M5, Q165).** The tool is `gag-migrate` (core in `cmd/gmc/internal/migrate`, CLI in `cmd/gmc/migrate`).
It resolves the latent ambiguities §H.17 flags: reuse is detected by content-addressing the built `RunnerTemplateSpec` (`podTemplate` **and** `workerImage`), so K identical templates collapse to one object; `maxListeners` is pinned to the v1 effective value (not v2's new default); `defaultProxyRef` is always wired so egress never silently goes direct; standalone `RunnerGroup` CRs win over inline bootstrap entries; and the `securityProfile` relocates onto the namespace label (most-restrictive-wins).
The operator runbook is [migration-v1-to-v2.md](../operations/migration-v1-to-v2.md) and the [`v1alpha1` deprecation notice](../operations/v1alpha1-deprecation.md).

**Privileged worker shapes fan out to the cluster-scoped kind (Q414).** The sketch above says "extracting each inline `podTemplate` into a `RunnerTemplate`", which is under-specified for a privileged (DinD/sysbox) tenant: a *namespaced* `RunnerTemplate` may not declare a privileged container ([§H.7](#h7-reference-integrity--runtime-conditions-not-admission) — a tenant must not self-author one), so that mapping emits an object the apiserver refuses, failing `--apply` after the `EgressProxy` is already created.
The fan-out therefore chooses the kind from the pod shape: a group whose `podTemplate` carries a privileged container or init container becomes a `ClusterRunnerTemplate` — the kind that exists for exactly these golden shapes (§H.6) — and its `RunnerSet` gets `templateRef.kind: ClusterRunnerTemplate`.
The choice is sound because `gag-migrate` is run by a platform administrator, the same role that hand-authors a `ClusterRunnerTemplate`, and it weakens nothing: PSA stamped from the namespace `securityProfile` is the runtime backstop for both kinds, and the `privileged-profile` grant it depends on is carried forward from v1, never invented.
Emitted cluster-template names are namespace-qualified (`crt-<ns>-<hash>`) so two tenants sharing a worker shape do not silently share one cluster-scoped object, and each carries an `actions-gateway.com/migrated-from-namespace` provenance label — the one migration output namespace deletion does not garbage-collect.

**`--apply` rides out a transiently unreachable webhook (Q461).** The fan-out above is a non-atomic sequence of creates followed by the namespace patch, and every create is gated by a v2 validating webhook under `failurePolicy: Fail` that the apiserver does not retry on its own.
A single stalled webhook POST — endpoint mid-rollout, node drain, cold TLS listener — therefore aborted `--apply` partway, leaving the earlier objects created; for a privileged tenant that includes the cluster-scoped `ClusterRunnerTemplate`, the one output namespace deletion does not reclaim.
`--apply` now retries each individual create (and the namespace read-modify-write) for a bounded 90s while the apiserver reports it could not *reach* a webhook, reporting progress on stderr.
The retry is deliberately narrow: a webhook that ran and **denied** the request, and every other error (`AlreadyExists`, RBAC `Forbidden`, a CEL rejection), still fails on the first attempt, so admission verdicts stay fast and loud.
The transport-error signatures are kept identical to the e2e helper's (Q392) so the two cannot drift apart on what counts as transient.

```
       v1alpha1 (one monolith)            one-shot tool         v2alpha1 (fan-out)
  ┌──────────────────────────────┐                       ┌──────────────────────────────┐
  │ ActionsGateway               │                  ┌──► │ ActionsGateway · identity    │
  │   ├ credentials · githubURL  │                  │    └──────────────────────────────┘
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

A conversion webhook can't create those sibling objects — which is exactly why the migration is a tool, not a webhook.

The v1→v2 fan-out is one-time.
Once on `v2alpha1`, the API graduates **in place** `v2alpha1 → v2beta1 → v2` via a conversion webhook (a thing a conversion webhook *can* do, since the kinds no longer change shape) — see the [graduation path](../plan/v2-api.md#api-maturity--graduation-v2alpha1--v2beta1--v2) in the implementation plan for the per-hop mechanics.
The superseded versions do not linger indefinitely: `v2alpha1` is dropped at **`v2.0.0`**, the same release that removes `v1alpha1` and the classic acquisition machinery, announced one release ahead in `v1.3.0`.
That coupling and the operator-facing contract are the [deprecation and removal notice](../operations/v1alpha1-deprecation.md); the release sequencing is [v2-ga.md](../plan/v2-ga.md).

## H.12. Folding in the grandfathered label-value alignment (Q147)

Two shipped keys still carry boolean-looking `"true"` values that predate the [no-boolean label convention](../development/kubernetes-conventions.md) and are grandfathered only because changing them is breaking:

- `actions-gateway.github.com/tenant: "true"` — the managed-tenant marker, matched as `== 'true'` by the `namespace-psa-guard` and `tenant-resource-guard` `ValidatingAdmissionPolicy` objects, the onboarding scripts, and operator runbooks.
- `actions-gateway.github.com/allow-profile-downgrade: "true"` — the downgrade opt-in annotation, matched by the GMC validating webhook.

Aligning them (→ `tenant: managed`, `allow-profile-downgrade: allowed`, following the existing `privileged-profile: allowed` precedent) is a breaking change to deployed clusters: it touches VAP CEL, onboarding, runbooks, and the label/annotation on every live tenant namespace.
The convention doc therefore defers it to "a separate, deliberate migration."
**The v2 cutover is that migration** — it is already breaking, already ships a migration tool ([§H.11](#h11-migration-v2-tool-assisted)), and already reworks the same VAPs and onboarding for multi-gateway-per-namespace.
Folding Q147 in here costs almost nothing extra and avoids a second, standalone breaking migration later.

**The key *prefixes* migrate too, not just the values.** The v2 [API group rename](#h15-other-breaking-changes-worth-batching) moves these keys off the `actions-gateway.github.com/` domain to `actions-gateway.com/` (e.g.
`actions-gateway.com/tenant`), together with the other domain-prefixed identifiers — `privileged-profile`, `agentpool-cleanup`, `gmc-cleanup`, the version label, and the finalizer names.
This is the same class of breaking change as the value alignment and rides the **same dual-read window**: every consumer accepts either domain *and* either value until `v1alpha1` removal, and the migration tool relabels in one pass.
Renaming the API group but leaving the labels on the old domain would be a permanent inconsistency, so the prefixes move *with* the group.

Both keys survive into v2 unchanged in *meaning* — the `tenant` marker still confines the GMC's namespace writes under multi-gateway-per-namespace, and `allow-profile-downgrade` still guards `ActionsGateway` PSA downgrades — so the cutover changes only their *values*, not their role.

**Dual-read window (coincident with the v1/v2 coexistence window).** Q147 needs a dual-read migration so live namespaces are not broken mid-cutover:

1. While `v1alpha1` and `v2alpha1` are served side by side, every consumer of these values — both VAPs and the downgrade webhook — accepts **either** `"true"` (legacy) **or** the new keyword.
   Reads are dual; writes prefer the new keyword.
2. The migration tool ([§H.11](#h11-migration-v2-tool-assisted)) relabels the namespace marker and rewrites the annotation to the new keyword as part of the same one-shot pass that emits the v2 object set, so no separate operator action is required.
3. When `v1alpha1` is removed at `v2.0.0`, drop the `"true"` arm from the VAPs and webhook.
   The dual-read window closes exactly when `v1alpha1` serving does.

This stays **fail-closed** throughout: the CEL/webhook checks already treat any non-sentinel value as "not granted", so accepting a second sentinel during the window never widens a grant — at worst a namespace is briefly un-aligned and the already-applied `"true"` keeps working until the tool relabels it.

**Shipped (M5, Q165).** The dual-read spans all four consumers: the `namespace-psa-guard`, `tenant-resource-guard`, and `namespace-security-profile-guard` `ValidatingAdmissionPolicy` objects (dual-marked in M3a/M3b), and the v1 GMC `ActionsGateway` validating webhook (M5) — whose `validatePrivilegedEligibility` now accepts the grant label on either domain and whose `validateSecurityProfileTransition` accepts the downgrade opt-in on either domain *and* either value keyword.
The migration tool relabels the namespace markers additively (it adds the v2 keys and keeps the v1 keys), so a still-running v1 gateway in a relabeled namespace is never stranded.
The window closes when `v1alpha1` is removed.

**The migration tool is a dual-read consumer too (Q463).** `gag-migrate` decides whether a tenant holds the privileged-eligibility grant, and it must reach the *same* verdict as admission: a namespace granted on the v2 domain alone is legal and is admitted privileged, so a tool reading only v1 reports a live grant as missing and prescribes a label the operator already holds.
The grant read is therefore a single shared function (`gmc/internal/webhook/validation.PrivilegedGrantPresent`) that the v1 webhook and the tool both call, rather than two implementations free to drift — the same single-source-of-truth rule the Q323 audit established for the versioned webhooks.

## H.13. What adopting this changes

This proposal, if accepted, touches more than the API types.
Non-exhaustive impact list, to be turned into plan-doc scope when scheduled:

- **API:** new `v2alpha1` group `actions-gateway.com` with five CRD kinds (`ActionsGateway`, `RunnerSet`, `RunnerTemplate`, `ClusterRunnerTemplate`, `EgressProxy`) + generated CRDs/deepcopy/RBAC.
- **GMC:** multi-gateway-per-namespace requires every derived resource to be keyed by gateway name (not the namespace-singleton assumption today); the `gmc-tenant-resource-guard` policy must still confine writes; new `EgressProxy` reconciler; cross-namespace CA distribution.
- **AGC:** `RunnerSet` reconciler resolves `templateRef`/`proxyRef` at runtime with watches; reserved-field webhook moves to `RunnerTemplate`.
- **Docs:** [§3.1 CRD schemas](03-api-contracts.md#31-kubernetes-crd-schemas), [Appendix E (RunnerGroup design)](appendix-e-capacity-planning.md), and the operator-facing docs per the [doc-update matrix](../development/doc-update-matrix.md).
- **Migration tool** + its tests.
- **Label keys + values (Q147 + group rename):** both the domain *prefix* (`actions-gateway.github.com/*` → `actions-gateway.com/*`, plus finalizer names) and the grandfathered `tenant`/`allow-profile-downgrade` `"true"` *values* migrate during the cutover ([§H.12](#h12-folding-in-the-grandfathered-label-value-alignment-q147)) — VAP CEL, onboarding scripts, runbooks, and the convention doc's "grandfathered" note all update, and one dual-read window covers both, riding the v1alpha1 serving window.

### H.13.1. CRD packaging and conditional GMC startup

The five `actions-gateway.com/v2alpha1` CRDs ship in a **separate, opt-in Helm chart** (`actions-gateway-crds-v2`), not the main `actions-gateway` chart.
The split keeps the main chart's Helm release Secret under Kubernetes' 1 MiB object limit — the five v2 CRDs' OpenAPI schemas are large enough that bundling them would push it over.

So a supported install can have the GMC running without the v2 CRDs present.
The GMC makes the opt-in real on the controller side: at startup it queries the REST mapper for the `ActionsGateway` and `EgressProxy` v2 kinds and only registers the v2 controllers (`ActionsGatewayV2Reconciler`, `EgressProxyReconciler`, `NamespacePSAReconciler`) and the IP-range reconciler's v2 NetworkPolicy refresh passes when both are served.
On a v1-only install it logs one info line and starts v1-only — rather than registering `source.Kind` watches against absent kinds, which would spin a "no matches for kind" retry loop and make the IP-range reconcile log a list error every tick. v1alpha1 reconciliation is unaffected either way.

Detection is a one-shot startup check, so installing the CRD chart into a running v1-only cluster requires a GMC restart to enable the v2 controllers.
The RunnerTemplate validating webhooks are registered unconditionally (they never fire until a `RunnerTemplate`/`ClusterRunnerTemplate` exists, which requires the CRDs), so the webhook path needs no restart.
The operator-facing runbook for the "v2 objects not reconciling after installing the CRD chart" symptom lives in [troubleshooting.md](../operations/troubleshooting.md#v2-objects-not-reconciling-after-installing-the-crd-chart).

### H.13.2. Install/upgrade lifecycle — apply-render, not `helm install` (Q276)

Splitting the CRDs into their own chart (§H.13.1) keeps the **main** chart under the 1 MiB release-Secret limit, but the v2 chart itself is still too large to `helm install`: rendered it is ~2.5 MB, and Helm stores a release as `base64(gzip(json(release)))` — where `json(release)` embeds **both** the rendered manifest **and** a base64-inflated copy of the chart source — inside one Secret the apiserver caps at 1 MiB.
Reconstructing that encoding for the five-CRD chart lands at **~1.10 MiB stored, ~0.1 MiB over the ceiling.** So the chart is **applied from its render, not installed**:

```sh
helm template actions-gateway-crds-v2 <chart-or-oci-ref> --namespace gmc-system \
  | kubectl apply --server-side -f -
```

**Decision: apply-render is the supported, deliberate install/upgrade path — not a stopgap.** Server-side apply is declarative and idempotent, so install and upgrade are the *same* command (re-run it to carry CRD field changes); it also clears kubectl's 256 KB client-side-apply annotation ceiling, which the two ~1.16 MB `RunnerTemplate` CRDs blow past on their own.
This is the mainstream practice for large CRDs (cert- manager, Crossplane, Istio, Gateway API all ship CRDs to be applied out-of-band and warn against Helm-managing them).
The templates carry `helm.sh/resource-policy: keep` so an operator who fronts them with a GitOps tool that *does* build a Helm release keeps the CRDs on prune/uninstall.

To keep the *manual* path a single command with no helm or chart checkout, each release attaches a **pre-rendered, cosign-signed `actions-gateway-crds-v2.yaml`** (rendered for the default `gmc-system` namespace) to its GitHub Release, so a manual install is just `kubectl apply --server-side -f <release-url>/actions-gateway-crds-v2.yaml`.
It is keyless-signed via the same Fulcio/Rekor path as the images and charts (`sign-blob` → a Sigstore bundle verified with `cosign verify-blob --bundle`), which also answers the "release assets are mutable" caveat.
The asset covers the default-namespace case; a GMC in a non-default namespace, or a GitOps render, still uses the `helm template --set … | kubectl apply --server-side` path (the conversion webhook `clientConfig` namespace bakes in at render time).
Argo CD renders the chart itself and never builds a release Secret, so it is unaffected by the 1 MiB limit; Flux `HelmRelease` *does* build one and uses the rendered manifest or a Kustomization instead.

The properties this forgoes — `helm upgrade` revision history, `helm rollback`, `helm uninstall` cleanup — are **low-value for CRDs specifically** and partly anti-patterns: rolling a CRD schema *backward* can strand stored objects (removing a served/storage version or field is a deliberate migration, never a casual rollback), and cascading `helm uninstall` of a CRD deletes every custom resource with it (data loss — exactly what `resource-policy: keep` prevents).
The operator-facing upgrade/rollback/uninstall runbook is [install.md § the v2 API CRDs](../operations/install.md#optional-the-v2-api-crds).

Alternatives, each measured by reconstructing Helm's stored-Secret encoding:

| Option | Stored release Secret | `helm install` | `helm upgrade` | Verdict |
|---|---|---|---|---|
| **Apply-render (chosen)** | n/a — no release created | n/a | re-apply, server-side | **Chosen.** Uniform install≡upgrade; clears both the 1 MiB *and* 256 KB limits; GitOps-friendly; ecosystem norm. |
| Single templated chart (today's chart shape, helm-installed) | ~1.10 MiB | ❌ fails | — | Over the ceiling — this *is* the Q276 problem. |
| `crds/` directory convention | ~0.72 MiB | ✅ installs | ❌ **never** upgrades/deletes | Rejected: Helm's `crds/` dir is create-only by design, so schema changes never propagate on upgrade — trades the size problem for a silent no-upgrade problem on an evolving API. |
| Split large CRDs into per-CRD charts | ~0.50 MiB each large CRD; ~0.10 MiB for the three small ones together | ✅ installs | ✅ | Rejected: technically viable and the `spec.conversion` wiring is per-CRD so it survives the split, but it multiplies one artifact into three published/versioned charts and three releases (an umbrella chart re-aggregates into one Secret and lands back over the limit), buying a Helm CRD lifecycle you should not use (see above) at real packaging + operator-UX cost. |
| Trim rendered size (drop descriptions / `x-kubernetes-preserve-unknown-fields`) | shrinks, but | partial | partial | Rejected: dropping descriptions degrades `kubectl explain`; replacing the embedded `PodTemplateSpec` schema with a preserve-unknown-fields blob drops server-side validation of the pod template — a secure-by-default regression. The structural Pod schema is inherently large regardless. |

**Fold-back into the main chart is gated on v2 reaching a *single served version*, not on `v1alpha1` removal.** Measured against the same encoding: with v2 down to one version (`v2beta1` only — `v2alpha1` and the conversion webhook retired), the folded main chart stores at **~0.65 MiB, ~0.4 MiB under the ceiling.** It fits because a single-version v2 CRD set (~1.3 MB) is about the same footprint as the two v1 CRDs it replaces (~1.3 MB), and today's main chart already carries those and installs cleanly (**~0.62 MiB stored**).
The trap is the coexistence window: while v2 serves *both* `v2beta1` and `v2alpha1` (which is what forces the conversion webhook), that set alone stores at **~1.26 MiB** — over the limit before a single controller is added, which is exactly why the v2 CRDs live in their own chart today.
So the clean fold-back sequence is two removals, both required: retire `v1alpha1`, **and** drop `v2alpha1` (retiring the conversion webhook), leaving one served version — then the now single-version v2 CRDs fold into the main chart with headroom to spare.
Both of those removals land at **`v2.0.0`** ([notice](../operations/v1alpha1-deprecation.md)), which is therefore the earliest the fold-back can be reconsidered, and only once v2 is genuinely down to one served version.
Until then, the opt-in chart plus apply-render stands.

## H.14. Admin policy layer — deferred until tiering is real

The decomposition above mirrors Gateway API's `Gateway → route attachment` but stops one level short: there is no cluster-scoped, **admin-owned** object — no `GatewayClass` equivalent.
Today the admin/tenant boundary is real but lives *outside the API*, scattered across mechanisms that cannot be RBAC'd, audited, or GitOps'd as objects:

| Policy | Where it lives today |
|---|---|
| Which PriorityClasses a tenant may name | `--allowed-priority-classes` GMC flag |
| Whether `privileged` profile is allowed | namespace label `…/privileged-profile=allowed` |
| Default worker image | `--worker-image` GMC flag |
| Reserved namespaces | Go constant + `POD_NAMESPACE` |
| Namespace ResourceQuota | platform-stamped, out-of-band |

Promoting this into a first-class API object turns the boundary into a clean RBAC split (admin writes the policy kind; tenants cannot) and makes "what is this tenant allowed to do?" a `kubectl get` away.
**But it is not a problem we have today, and the abstraction is addable without a second breaking change** — so v2 does *not* ship it.
This section records the capability ladder and the exact trigger, so the decision is captured rather than rediscovered.

### The capability ladder

| Layer | Expresses | Breaks when |
|---|---|---|
| **Flags (today)** | one global policy, cluster-wide | can't vary per tenant at all (except the one bolted-on privileged namespace-label gate) |
| **Singleton policy object** | one global policy, but declarative / auditable / RBAC'd | still *uniform* — every tenant gets the same rules |
| **Singleton + namespace labels** | one global policy plus a *few independent* per-tenant dials ("privileged iff namespace has label X") | the per-tenant variation becomes *multi-dimensional and correlated* |
| **Class** (`ActionsGatewayClass`) | named bundles of correlated policy, tenant-selectable, RBAC-gated on which class may be referenced | — |

A *single* per-tenant escape hatch (like privileged-allowed) does **not** need a class — a namespace label the admin controls handles it, which is already how v1 works.
The singleton + the occasional label dial gets you a long way.

### Promote flags → singleton when

The singleton carries the *same* policy as the flags — one uniform policy, no tiers — so it buys only the rung-2 wins, not tiering.
Promote when **any** of:

- **GitOps** — policy must be managed declaratively / changed without a controller redeploy.
- **RBAC separation** — the people who set policy must be distinct from the people who own the controller Deployment (a platform-policy team vs. the platform engineer who deploys the GMC).
- **Audit/compliance** — "show me, as a cluster object, exactly what tenants are allowed" is an actual requirement.

If none of those bite, flags are simpler and equally forward-compatible.

### The trigger for the class

Introduce `ActionsGatewayClass` only when **both** hold:

1. **≥2 distinct policy *bundles* must coexist** in one cluster — e.g. an "internal/trusted" tier (DinD allowed, broad registries, proxy optional) vs an "external/untrusted" tier (restricted-only, platform registry only, proxy mandatory): multiple policy dimensions that *travel together* as a tier; **and**
2. **either** those tiers are spread across enough namespaces that encoding each as a *combination* of namespace labels becomes an audit/maintenance liability (N namespaces × M labels that should just say "tier = A"), **or** you want tenants to **self-select** a tier with RBAC deciding which they may pick — which labels cannot express, because tenants do not control namespace labels.

Smell signs the trigger has arrived: the onboarding runbook grows a "pick your tier, then apply these K labels" step (that step *is* a class waiting to be born); a request like "team X gets privileged + registries A&B, team Y neither"; or a self-service "request the privileged tier" flow.

### Why deferring costs nothing

**Every rung of the ladder is an additive transition** — none is a breaking migration:

- *flags → singleton*: add the `ActionsGatewayPolicy` kind; the controller prefers it when present and the flags remain as fallback/defaults.
- *singleton → class*: add the `ActionsGatewayClass` kind, and add `ActionsGateway.spec.gatewayClassName` as an *optional* field whose unset value means "the default class / the old singleton"; the singleton simply *becomes* the default class.

So deferring either step buys no future breaking migration.
The one constraint to honor now: **whatever policy lands in v2 — flags or a singleton — must be shaped so a future singleton/class could carry the identical schema field-for-field.** Don't paint the policy into a corner a later rung couldn't inherit.

### v2 decision

**v2 keeps the controller flags.** A singleton/class earns its keep only at the triggers above, none of which is a problem we have today, and every rung is additive — so promoting later costs nothing, while building now would be abstraction ahead of need.
The single obligation v2 carries is to shape the flag-backed policy so a future singleton/class inherits its schema field-for- field.
**Ship neither the singleton nor the class.** Promote to the singleton at the flags→singleton trigger; introduce the class at the two-part class trigger.

## H.15. Other breaking changes worth batching

v2 is the one window where breaking changes are cheap (we are already rewriting the schema and shipping a migration tool).
A few small changes are only *possible* at a major break, or are awkward to add later — batch them in, but only the ones that fix a problem we have today.

**Decided for v2 (today's problem, break-only or break-cheapest):**

- **Drop the `SecretReference.namespace` footgun.** It is reserved-but-validated- empty and reads like a cross-namespace reference that does not exist.
  Replace with a name-only `LocalSecretReference`.
  Removing a field is break-only.
- **Per-field immutability** via CEL `XValidation` (`oldSelf`): **`githubURL` immutable** — rebinding a running gateway's GitHub org is a footgun; **`credentials.githubApp.name` mutable** — it is the credential-rotation path.
  Adding immutability later is itself breaking, so it is fixed at v2.
- **API group rename → `actions-gateway.com`.** The group is `actions-gateway.github.com`, which suffixes a domain the project does not control — against the k8s convention of using a domain you own.
  The project owns `actions-gateway.com`, so v2 renames the group to it.
  Changing the group touches every CRD (and every CR, RBAC rule, VAP, and manifest that names it), so it can only happen at a major break — it rides the v2 cutover and its migration tool.
  The **label/annotation key prefixes, the version label, and finalizer names** carry the same domain and rename with it (`actions-gateway.github.com/*` → `actions-gateway.com/*`), on the Q147 dual-read window so live namespaces are not broken mid-cutover ([§H.12](#h12-folding-in-the-grandfathered-label-value-alignment-q147)).
- **Cheap usability while regenerating:** `additionalPrinterColumns` (Ready, profile, active sessions), resource `categories`, and the short names from [§H.6](#h6-naming-and-length-budgets).
- **`maxListeners` default → `10`** (was `1` in code; matches the design).
  Confirmed against the AGC listener `Multiplexer`/`Run` source: the pool keeps a permanent baseline of **one** poller and demand-spawns extra pollers only as jobs are acquired (a job-holding goroutine is busy, not polling, for the job's whole duration), with non-baseline pollers idle-exiting after 50 empty polls.
  So `maxListeners` is a **concurrency ceiling with a baseline of 1**, not a steady-state count: a higher default costs nothing at idle, while `1` serializes job pickup per group (the busy baseline leaves no poller, and `SpawnReplacement` is a no-op at the ceiling).
  The real resource guards (`maxWorkers` + namespace `ResourceQuota`) still bind, so the higher default regresses no safety property. v2 sets the default to `10`.

**Opportunistic (take if it falls out of the rewrite; not a sign-off item):**

- **Webhook → CEL migration.** v2 targets a newer k8s floor, so checks that are webhook-only today *because* CEL could not express them on k8s ≤1.30 (singleton, GitHub-URL structure, cross-field rules) can become structural/CEL.
  Every check moved out of the fail-closed validating webhook is one fewer thing whose outage blocks all admission — an availability and operability win, best taken during the schema rewrite.

- **Credentials as a discriminated union — _shipped in `v2alpha1` (Q196/Q197)._** A flat `workloadIdentityRef` sibling is *mechanically* additive, but additive *into a permanently worse shape*: once `githubAppRef` is top-level under beta it can never move under a parent without a breaking change + storage migration.
  Since `alpha → beta` is the last free break and workload identity is on-strategy (removes the App key from the cluster — the secure-by-default direction), the credential is nested under an explicit-discriminator `spec.credentials` parent **in `v2alpha1`** — a free reshape while alpha carries no stability contract, so the beta cut inherits the right shape and the conversion webhook (Q74) round-trips it as an identity for the credentials block.
  `spec.credentials` is a discriminated union keyed by `credentials.type` (`+unionDiscriminator`): `githubApp` (a name-only `LocalSecretReference`, the possession model) and `workloadIdentity` (the no-PEM delegation model, Q197).
  The union's "exactly the named member is set" invariant is enforced by a per-member CEL `iff` rule that each new member extends — never an N-way "exactly one of" that grows with the union.
  **`workloadIdentity` (Q197) shipped as the second member**: an external signer signs the App JWT so the App private key never enters the cluster (MVP = HashiCorp Vault transit + Vault Kubernetes auth, behind a `githubapp.Signer` interface so cloud KMS providers add without another breaking change).
  Adding it validated the shape against a real second consumer, the whole point of fixing the shape before the freeze.
  See [05-security.md §5.7](05-security.md#57-workload-identity-the-no-pem-delegation-model) for the trust model.
  Plan + schema sketch: [v2beta1.md](../plan/v2beta1.md).

**Explicitly NOT now (shape for additive later, do not build):**

- **Admin policy class** — [§H.14](#h14-admin-policy-layer--deferred-until-tiering-is-real).
- **Worker-image registry allowlist** — a real security control, but only needed once there are untrusted tenants to restrict.
  It belongs in the admin policy schema and is enforced when that layer arrives; do not add a standalone tenant field for it now.

## H.16. Open questions / sign-off needed

### Recommended (pending ratification)

Each carries a recommendation grounded in precedent (Gateway API, ARC, cert-manager trust-manager, Kubernetes finalizer guidance); ratify or override.

1. **Multi-gateway-per-namespace — naming, AGC scoping, ownership.** Verified against `gmc-tenant-resource-guard` (`cmd/gmc/config/admission-policy/tenant-resource-guard.yaml`): the GMC-confinement VAP keys on the namespace `tenant=true` marker, **not on resource names**, so it already scales to N gateways per namespace and needs no change.
   The real work is three controller-side changes:
   - **(a) Per-gateway naming** — every derived resource becomes `<ag>-<suffix>` (`<ag>-agc`, `<ep>-proxy`, worker `generateName=<rs>-`) under the [§H.6](#h6-naming-and-length-budgets) 52-char cap, so two gateways in one namespace never collide on a fixed name.
   - **(b) Per-gateway AGC scoping** — N gateways ⇒ N AGC Deployments in one namespace, so each AGC must reconcile **only the `RunnerSet`s whose `gatewayRef` targets it** — the one genuinely new controller behavior, without which N controllers fight over the same objects.
   - **(c) Per-gateway ownership** — each `ActionsGateway` owner-refs its own children so deleting one gateway GCs only its resources, not a neighbor's.

   (Optional defense-in-depth: also require a GMC `managed-by` label on writes; not needed for correctness since the VAP already confines by namespace.)
   Precedent: ARC runs multiple scale sets per namespace, names = CR prefix + fixed suffix.
   The core build of v2 — naming + watch-scoping + ownership, not a policy rewrite.

   **Implemented (M3b, Q167).** All three: (a) every AGC child is named `<ag>-<suffix>` (`<ag>-agc`, `<ag>-worker`, `<ag>-workload`, `<ag>-agc-metrics-{tls,client}`) under the §H.6 52-char cap, `<ag>-agc` doubling as the pod `app` label / NetworkPolicy / Service selector so two AGC Deployments never adopt each other's pods; (b) the GMC stamps `GATEWAY_NAME` on each AGC Deployment and the AGC scopes its `RunnerSet` informer with a server-side `spec.gatewayRef.name` field selector (KEP-4358, k8s ≥ 1.31) plus a defense-in-depth reconcile guard; (c) per-gateway names + owner refs GC only the deleted gateway's children.
   The `gmc-tenant-resource-guard` VAP is unchanged.
   Closing the M3a deferral, the AGC also gains least-privilege cluster-scoped read of `ClusterRunnerTemplate` via a per-gateway `ClusterRoleBinding` (shipped `agc-clusterrunnertemplate-reader` ClusterRole; GMC holds only `bind`), deleted explicitly on teardown since a cluster-scoped object cannot own-ref a namespaced gateway.
   Envtest (both suites) + a kind e2e (`E2E_V2_MultiGateway`) prove the scoping isolation and per-gateway GC.
2. **Cross-namespace proxy CA distribution → ConfigMap, not secret.** The CA is a public certificate, so the GMC distributes it as a **ConfigMap** into only the granted consumer namespaces (trust-manager pattern; [§H.9](#h9-cross-namespace-proxy-sharing)).
   No secret-replication subsystem is needed — recommend dropping that as a blocker.
3. **Optional proxy** — ✅ guardrail as in [§H.10](#h10-the-egress-proxy-becomes-optional): egress restriction stays mandatory, managed-IP refresh relocates, plus an explicit `proxyMode: Direct` status and an `EgressUnattributed` advisory condition so the attribution trade is auditable.
4. **Sharing model** — ship the **inline `allowedNamespaces` allowlist only** for v2; `ReferenceGrant` support is additive later.
   Consent stays provider-side either way ([§H.9](#h9-cross-namespace-proxy-sharing)).
5. **Deletion semantics** — degrade-not-block with **no finalizer at all** ([§H.8](#h8-ownership-gc-and-deletion)); `referencedBy` is computed from the watch.
   Confirm no operator relies on hard deletion protection.
6. **Q147 label-value keywords** — ratify `tenant: managed` and `allow-profile-downgrade: allowed` (symmetric with `privileged-profile: allowed`), with the dual-read window closing only at `v1alpha1` removal ([§H.12](#h12-folding-in-the-grandfathered-label-value-alignment-q147)).
7. **Multi-gateway `securityProfile` composition — ✅ move it off the gateway.** The contention is self-inflicted.
   Pod Security Admission is a **namespace-scoped** control in Kubernetes, and v1 hung it on a *sub-namespace* object (`ActionsGateway.spec.securityProfile`) — so under multi-gateway (#1) two `ActionsGateway`s in one namespace fight over the single namespace PSA label.
   The fix **deletes the question instead of answering it: `securityProfile` becomes a namespace-scoped concern, not a per-gateway field.** Drop `SecurityProfile` from `ActionsGatewaySpec` (a cheap follow-up to the just-merged M1 `v2alpha1` types — alpha, no compatibility guarantee) and let the namespace own its Pod Security level, GMC-guarded exactly as today: the downgrade-protection and `privileged`-eligibility machinery (`securityProfileRank`, `validateSecurityProfileTransition`, the `allow-profile-downgrade` keyword) stays, now keyed **once per namespace** instead of per gateway.
   Co-located gateways therefore always share one posture; tenants that need *different* postures use *different* namespaces — the natural PSA isolation boundary anyway.
   Land the field-home change no later than **M3a (Q164)**, where `ActionsGateway` is first reconciled, so M3a reads the profile from its new home rather than building per-gateway logic that M3b would rip out.
   Migration is unaffected (one v1 namespace → one gateway → one profile).

   **Implemented mechanism (M3a, Q175).** The namespace-side selector is the label `actions-gateway.com/security-profile: baseline|restricted|privileged` (`SecurityProfileLabel`); absent on a managed tenant namespace ⇒ `baseline` (secure default).
   Two GMC-side pieces realize the guarantee, and the relocation makes the guard *simpler* than v1, not just relocated:
   - **`NamespacePSAReconciler`** (GMC) watches managed v2 tenant namespaces and stamps the six `pod-security.kubernetes.io/*` labels from the profile label via Server-Side Apply (the v1 `applyNamespacePSA` stamping logic, now keyed once per namespace and decoupled from any gateway's lifecycle).
     The PSA labels exist as soon as the namespace is a managed tenant, with or without a gateway.
   - **`gmc-namespace-security-profile-guard` ValidatingAdmissionPolicy** reproduces the v1 webhook's three guarantees — enum, no-silent-downgrade (requires the `allow-profile-downgrade` annotation), and `privileged` eligibility (requires the platform `privileged-profile=allowed` label) — *none weaker than v1*. v1 needed a Go validating **webhook** because the downgrade/eligibility checks read a *different* object (the namespace) than the one admitted (the `ActionsGateway`).
     Now that the profile lives **on the namespace**, both checks act on the same object the admission is about, so they collapse into a **VAP** — in-process, no webhook-pod availability dependency, fail-closed (`failurePolicy: Fail`, `validationActions: [Deny]`), and consistent with the existing `namespace-psa-guard`/`tenant-resource-guard` pattern.
     The downgrade check is skipped on CREATE (no prior state), so a namespace may be created directly at any eligible profile.
     Both guard policies, plus the two existing ones, dual-read the v1 (`actions-gateway.github.com/tenant=true`) and v2 (`actions-gateway.com/tenant=managed`) markers during coexistence ([§H.12](#h12-folding-in-the-grandfathered-label-value-alignment-q147)), so the GMC can stamp PSA and provision in v2 tenant namespaces.

   **Fallback — only if a concrete need for co-located *differing* profiles emerges:** keep `securityProfile` per-gateway and resolve the namespace label by **most-restrictive-wins** — runtime composition (max `securityProfileRank` across the namespace's gateways), surfaced via a per-gateway `EffectiveSecurityProfile` condition reporting the resolved profile and whether a sibling raised it.
   It is secure-by-default (the label only ever rises) and fits [§H.7](#h7-reference-integrity--runtime-conditions-not-admission)'s "runtime conditions, not admission" stance.
   Reject the other two rules: **all-must-agree / reject-on-conflict** needs cross-object admission (inspect sibling gateways — exactly what §H.7 avoids, and awkward in single-object CEL); **off-label** drops namespace PSA enforcement entirely, a secure-by-default regression.
   Edge for the fallback only: deleting the gateway that forced `restricted` drops the label to the next-highest — a *downgrade-by-deletion* that interacts with `validateSecurityProfileTransition` / `allow-profile-downgrade`; decide whether that auto-downgrade needs the guard or is acceptable (no remaining gateway requested the stricter level).

### Resolved

8. **Admin policy layer** (§H.14) — ✅ **v2 keeps the controller flags.** Neither the singleton nor the class ships in v2; each is deferred behind a documented trigger (flags→singleton, then the two-part class trigger), and every rung is an additive, non-breaking transition. v2's only obligation is to shape the flag-backed policy so a future singleton/class inherits its schema field-for-field.
9. **API group rename** (§H.15) — ✅ **Yes, rename to `actions-gateway.com`.** The project owns the domain; the change rides the v2 cutover and its migration tool.
   Break-only, so it happens here or never.
10. **Per-field immutability** (§H.15) — ✅ **`githubURL` immutable, `credentials.githubApp.name` mutable.**
11. **`maxListeners` default** (§H.15) — ✅ **`10`** (was `1` in code).
    Verified against the AGC listener source: `maxListeners` is a concurrency ceiling with a baseline-of-1 + demand-spawn + idle-shutdown, so a higher default is free at idle and `1` needlessly serializes job pickup; `maxWorkers`/quota remain the binding resource guards.
    (Closed Q162.)

## H.17. Migration correctness — the fan-out's untested invariants

The migration ([§H.11](#h11-migration-v2-tool-assisted)) is the first and only place two invariants this design *asserts* are tested against real v1 data.
Both are stated confidently above; neither has been exercised.
They are acceptance criteria the M5 tool (Q165) must meet — and, because they are pure data-shape questions, they can be validated **before** M5 by a mapping over representative v1 fixtures, surfacing any v2 schema gap at alpha-rewrite cost instead of post-adoption cost.
(The `v2alpha1` types do not exist yet, so this is a fixtures-and-asserted-output exercise, not runnable tool code, until M1 lands.)

### Invariant 1 — "no behavior change" (the non-goal most at risk)

The fan-out changes field *defaults* and *optionality*, so "v2 tracks v1 behavior" is false unless the mapping actively compensates:

- **Proxy must not silently become direct egress.** v1 always routes through the proxy; in v2 an unset `proxyRef` *and* unset `defaultProxyRef` ⇒ direct egress ([§H.10](#h10-the-egress-proxy-becomes-optional)).
  The mapping **must** set `defaultProxyRef` on every migrated gateway, or migration regresses both behavior *and* the secure-by-default egress identity.
  Acceptance: a proxied v1 tenant migrates to a proxied v2 tenant, never to `proxyMode: Direct`.
- **`maxListeners` 1 → 10.** v1 unset = 1; v2 unset = 10 ([§H.15](#h15-other-breaking-changes-worth-batching)).
  The mapping must either pin `maxListeners: 1` to preserve the v1 concurrency ceiling or consciously accept the change — not inherit the new default by omission.
  Decide and encode it.
- **v1 data must be admissible under v2 CEL.** The cross-field rule `maxWorkers == priorityTiers[last].threshold` and the reserved-pod-field rejection (now on `RunnerTemplate`, [§H.7](#h7-reference-integrity--runtime-conditions-not-admission)) must accept every object the mapping emits.
  Real-apiserver defaulting applied to a round-tripped `PodTemplateSpec` can introduce a field the source lacked, so this is an **envtest** check, not a pure-Go transform check.

### Invariant 2 — "reuse" (the object-size justification)

v2's headline benefit is "one template exists once, referenced N times" ([§H.5](#h5-how-each-pressure-is-resolved)).
That benefit is realized **only if the migration detects reuse:** K `RunnerGroup`s sharing an identical `podTemplate` must collapse to **one** `RunnerTemplate`, not K copies.
Template equality is non-trivial (deep-equal over a defaulted `PodTemplateSpec`; whether the separate `workerImage` dedups with the template or independently).
Acceptance: a tenant with K identical-template groups migrates to one `RunnerTemplate` + K `RunnerSet`s.
If the mapping emits K templates, **v2 delivers zero object-size win for migrated tenants** — the benefit evaporates for the exact population it targets.

### Latent v1 ambiguities the fan-out forces to a decision

- **Standalone vs. inline runner groups.** v1 has both a standalone `RunnerGroup` CRD *and* inline `ActionsGateway.spec.runnerGroups[]` bootstrap copies.
  The mapping must define which is authoritative when both name the same group, and whether they merge or collide. v1 never reconciled the two representations; v2 forces the choice.
- **Naming the extracted objects.** The `EgressProxy` pulled from `spec.proxy` and each `RunnerTemplate` pulled from a group need *generated* names under the [§H.6](#h6-naming-and-length-budgets) 52-char cap — a naming scheme distinct from the runtime per-gateway derivation, and one that can collide.

These criteria turn the migration from "does it run" into "does it preserve what the design promised."
The proxy-default regression and the `securityProfile` composition gap ([§H.16 #7](#h16-open-questions--sign-off-needed)) are the two worth finding now, at alpha cost, rather than from an adopter.
