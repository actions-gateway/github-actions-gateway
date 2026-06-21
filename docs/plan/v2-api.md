# v2 API implementation plan

**Design source of truth:** [Appendix H — v2 API Decomposition](../design/appendix-h-v2-api-decomposition.md).
That appendix holds the *what* and *why* (the CRD set, the resolved decisions in
§H.16, the precedent-grounded recommendations). This doc holds the *sequencing* —
how the work is split into independently shippable milestones and in what order.

**Goal.** Replace the monolithic `v1alpha1` `ActionsGateway` + `RunnerGroup` API
with a decomposed `v2alpha1` API (`actions-gateway.com` group) that enables large
reusable pod templates, multiple gateways per namespace, and an optional/shared
egress proxy — without breaking running `v1alpha1` tenants.

**Approach.** Serve `v1alpha1` and `v2alpha1` side by side (no in-place
conversion — the split is a fan-out, see [§H.11](../design/appendix-h-v2-api-decomposition.md#h11-migration-v2-tool-assisted)).
Build v2 incrementally against the running v1, **nouns before verbs**, with a
one-shot migration tool last. The group rename folds in for free: `v2alpha1`
*is* `actions-gateway.com` from birth; `v1alpha1` keeps the old
`actions-gateway.github.com` group until it is removed.

## Non-goals

- **No behavior change.** v2 re-shapes the API; runtime semantics (job
  acquisition, worker provisioning, quota/PSA enforcement, egress restriction) are
  preserved. `v2alpha1` tracks v1 behavior wherever a field is unchanged.
- **No in-place v1→v2 conversion** — the split is a tooled fan-out (M5), not a
  conversion webhook.
- **Not the admin policy layer, worker-image allowlist, credentials union, or
  cross-namespace sharing** — all deferred (see below and Appendix H §H.14/§H.15).

## Coexistence, rollback & parity

- **Dual-serve.** `v1alpha1` and `v2alpha1` are served simultaneously until v1
  removal; tenants migrate on their own schedule via the M5 tool. v1 bug-fixes are
  ported to v2 throughout coexistence.
- **Rollback = stay on v1.** Nothing forces a tenant onto v2 until they run the
  migration, and no milestone removes v1 capability — so a regressed milestone
  degrades to "keep using `v1alpha1`", not an outage.
- **Parity gate.** `v2alpha1` must reach v1 feature parity (M3a) before
  multi-gateway / optional-proxy features build on it; a per-field/-condition
  parity checklist gates M3a exit.

## Resolved design decisions

All settled in [§H.16](../design/appendix-h-v2-api-decomposition.md#h16-open-questions--sign-off-needed):

- **Admin policy → keep controller flags** (singleton/class deferred behind triggers, [§H.14](../design/appendix-h-v2-api-decomposition.md#h14-admin-policy-layer--deferred-until-tiering-is-real)).
- **API group → `actions-gateway.com`**; **`githubURL` immutable**, `githubAppRef.name` mutable; **`maxListeners` default → `10`**; drop `SecretReference.namespace`. Field casing: `github` lowercased, initialisms uppercase (`githubURL`, `githubAppRef`).
- **Cross-namespace proxy CA → ConfigMap, not secret** (trust-manager pattern).
- **Sharing → inline `allowedNamespaces` only** for v2; `ReferenceGrant` additive later; consent always provider-side.
- **Deletion → degrade-not-block, no finalizer**; `referencedBy` from the watch.
- **Q147 keywords → `tenant: managed`, `allow-profile-downgrade: allowed`**; dual-read window closes at `v1alpha1` removal.

## Milestones

Nouns (data kinds) before verbs (controller kinds); migration last. Each
milestone is independently reviewable and leaves the tree green.

### M1 — API foundation (no controllers)

- New `v2alpha1` API group `actions-gateway.com` with all five kinds:
  `ActionsGateway`, `RunnerSet`, `RunnerTemplate`, `ClusterRunnerTemplate`,
  `EgressProxy` ([§H.3](../design/appendix-h-v2-api-decomposition.md#h3-the-crd-set), [§H.4](../design/appendix-h-v2-api-decomposition.md#h4-spec-sketches)).
- Generated deepcopy, CRD manifests, RBAC scaffolding, chart wiring.
- Structural + CEL validation: per-field immutability transitions, name
  `maxLength` 52 ([§H.6](../design/appendix-h-v2-api-decomposition.md#h6-naming-and-length-budgets)),
  `maxListeners` default `10`, removal of `SecretReference.namespace`,
  `additionalPrinterColumns` + `categories` + short names.
- **Field-naming pass** — freeze acronym/brand casing while still cheap
  (`githubURL`/`githubAppRef`), uniform `…Ref` shapes ([§H.6](../design/appendix-h-v2-api-decomposition.md#h6-naming-and-length-budgets)).
- **Uniform status/condition contract** across all five kinds — `Ready` +
  `observedGeneration`, `listType=map` conditions, specific messages ([§H.7](../design/appendix-h-v2-api-decomposition.md#h7-reference-integrity--runtime-conditions-not-admission)).
- **`selectableFields: spec.gatewayRef`** on `RunnerSet` so M3b's AGC scoping runs
  server-side ([§H.7](../design/appendix-h-v2-api-decomposition.md#h7-reference-integrity--runtime-conditions-not-admission)).
- Labels, annotations, and finalizers use the new `actions-gateway.com/*` domain
  from birth.
- **Exit:** CRDs install and round-trip via the API server alongside `v1alpha1`;
  `make check` green; no reconciler references the new kinds yet.

### M2 — Data kinds (nouns)

- `EgressProxy` reconciler in the GMC: owns its proxy Deployment / Service / HPA /
  PDB ([§H.8](../design/appendix-h-v2-api-decomposition.md#h8-ownership-gc-and-deletion)). **Same-namespace only** at this stage.
- `RunnerTemplate` / `ClusterRunnerTemplate`: pure data; the reserved-pod-field
  rejection webhook moves here from `RunnerGroup` ([§H.4](../design/appendix-h-v2-api-decomposition.md#h4-spec-sketches)).
- **Free observability win:** because each `EgressProxy` Deployment is now
  per-gateway, its proxy metrics carry the gateway label automatically — the
  per-tenant proxy-connection visibility v1's shared-proxy shape could not express.
- **Exit:** a standalone `EgressProxy` reconciles a working proxy pool; a
  `RunnerTemplate` validates and is readable by name; envtest coverage for both.

### M3a — Control kinds (verbs), single-gateway parity *(the core build)*

- `ActionsGateway` + `RunnerSet` reconcilers; **one gateway per namespace** —
  v1 feature parity on the new shape.
- **Reference resolution at runtime** ([§H.7](../design/appendix-h-v2-api-decomposition.md#h7-reference-integrity--runtime-conditions-not-admission)):
  resolve `templateRef`/`proxyRef` via watch + enqueue; surface
  `TemplateNotFound`/`ProxyNotFound` conditions; fail-closed (no wiring until refs
  resolve).
- Proxy **required** (same-namespace `EgressProxy` via `proxyRef`/`defaultProxyRef`),
  matching v1; direct egress is a separate, deferred slice (below).
- **Exit:** a v1-equivalent setup runs end-to-end on `v2alpha1` (job acquired →
  worker pod → proxied egress); the parity checklist passes; envtest coverage.

### M3b — Multi-gateway per namespace

- Per-gateway resource naming under the 52-char cap; **AGC scoping** via the
  `gatewayRef` field selector so each AGC reconciles only its gateway's
  `RunnerSet`s; per-gateway ownership for clean GC. The `gmc-tenant-resource-guard`
  VAP is unchanged — it keys on the namespace marker, not names ([§H.16 #1](../design/appendix-h-v2-api-decomposition.md#h16-open-questions--sign-off-needed)).
- **Exit:** two `ActionsGateway`s with their own `RunnerSet`s run concurrently in
  one namespace; envtest + a kind e2e prove per-gateway isolation.

### M5 — Migration tool + v1/v2 cutover

- One-shot fan-out migration tool ([§H.11](../design/appendix-h-v2-api-decomposition.md#h11-migration-v2-tool-assisted)):
  reads `v1alpha1` CRs, emits the `v2alpha1` object set (extract inline
  `podTemplate` → `RunnerTemplate`, inline `proxy` → `EgressProxy`, rewrite
  references). Dry-run to manifests by default; `--apply` to apply. Plus tests.
- **Dual-read window — group domain + Q147 values** ([§H.12](../design/appendix-h-v2-api-decomposition.md#h12-folding-in-the-grandfathered-label-value-alignment-q147)):
  VAPs + downgrade webhook accept either domain (`actions-gateway.github.com/*` or
  `actions-gateway.com/*`) **and** either value (`"true"` or the new keyword) until
  v1 removal; the tool relabels keys and rewrites values (plus finalizer names) in
  one pass.
- Operator migration guide; `v1alpha1` deprecation notice.
- **Exit:** the tool migrates a representative v1 namespace to a working v2 object
  set in dry-run and `--apply`; dual-read verified; docs updated.

## Itemized tasks

The actionable breakdown per milestone. Each box is a self-contained unit of work;
a milestone is done when every box is checked and its exit criterion holds.

### M1 — API foundation (Q149)

- [x] Scaffold `cmd/agc/api/v2alpha1/` and `cmd/gmc/api/v2alpha1/` (group `actions-gateway.com`, `groupversion_info.go`).
- [x] Define the five kinds + shared subtypes (`ObjectRef`, `LocalSecretReference`, `PriorityTier`, `TracingConfig`).
- [x] Field-naming pass — `githubURL`/`githubAppRef`, uniform `…Ref`, plural list fields.
- [x] CEL/structural validation — name `maxLength` 52; `githubURL` immutable (`oldSelf`); `maxListeners` default 10; `maxWorkers == priorityTiers[last].threshold`; reserved-pod-field rules on `RunnerTemplate`.
- [x] `selectableFields: spec.gatewayRef.name` on `RunnerSet`.
- [x] Uniform status/condition contract across all five kinds (`Ready` + `observedGeneration`, `listType=map`, specific messages).
- [x] `additionalPrinterColumns`, `categories`, short names (`ag`/`rs`/`rt`/`crt`/`ep`).
- [x] Labels/annotations/finalizers on the `actions-gateway.com/*` domain.
- [x] Codegen: deepcopy, CRD manifests, RBAC markers, chart wiring; both groups served beside `v1alpha1`.
- [x] Reserved-pod-field CEL covers the cheap, scalar pod-level fields (`serviceAccountName`, `host{PID,Network,IPC}`, `automountServiceAccountToken`); the per-container checks (privileged containers, proxy env vars) need an unbounded-array walk that exceeds the CEL cost budget, so they stay for the M2 `RunnerTemplate` webhook.

### M2 — Data kinds (Q163)

- [ ] `EgressProxy` reconciler (GMC): own Deployment/Service/HPA/PDB; per-gateway name `<ep>-proxy`; cert/CA wiring; same-namespace only.
- [ ] Per-tenant proxy metrics carry the gateway label (free win from per-gateway proxies).
- [ ] `RunnerTemplate` / `ClusterRunnerTemplate`: data only; move the reserved-pod-field rejection webhook here.
- [ ] envtest for both kinds.

### M3a — Single-gateway parity (Q164)

- [ ] `ActionsGateway` reconciler (GMC): AGC Deployment/SA/Role, namespace PSA labels, credential mount, NetworkPolicy; one gateway/ns.
- [ ] `RunnerSet` reconciler (AGC): port `RunnerGroup` behavior; resolve `templateRef`/`proxyRef` via watch + enqueue; `TemplateNotFound`/`ProxyNotFound` conditions; fail-closed wiring.
- [ ] Proxy required (`proxyRef`/`defaultProxyRef`, same-namespace).
- [ ] Per-field/-condition **parity checklist** vs. `RunnerGroup` (gates exit).
- [ ] envtest + a kind e2e parity run (job → worker pod → proxied egress).

### M3b — Multi-gateway (Q167)

- [ ] Per-gateway derived naming across every GMC-created resource (52-char cap).
- [ ] AGC watch-scoping via the `gatewayRef` field selector (server-side).
- [ ] Per-gateway ownership refs for clean cascade GC.
- [ ] envtest + kind e2e: two gateways with their own runner sets concurrent in one namespace.

### M5 — Migration + cutover (Q165)

- [ ] Fan-out migration tool (subcommand/`kubectl` plugin): v1 → v2 object set; dry-run default, `--apply`.
- [ ] Dual-read window: VAPs + downgrade webhook accept both domains *and* both values; tool relabels keys/values/finalizers in one pass.
- [ ] Operator migration guide; `v1alpha1` deprecation notice + named removal release.
- [ ] Conversion scaffolding (Q74 `Hub`/`Convertible`) staged for the `v2alpha1`→`v2beta1` graduation.
- [ ] Coexistence test (v1 keeps working while v2 served) + migration golden tests.
- [ ] **Behavior-preservation acceptance checks** ([§H.17](../design/appendix-h-v2-api-decomposition.md#h17-migration-correctness--the-fan-outs-untested-invariants)): proxied→proxied (never silent `proxyMode: Direct`); `maxListeners` default decision encoded; emitted objects pass v2 CEL under envtest; K identical templates → one `RunnerTemplate`; standalone-vs-inline group precedence defined. Validatable pre-M5 as a fixtures→asserted-output mapping that fuzzes the M1 schema for completeness/ambiguity.

## API maturity & graduation (`v2alpha1` → `v2beta1` → `v2`)

**Why `v2alpha1` and not `v1alpha1` in the new group?** Nothing technical forces
the version bump: a CRD is identified by **group + kind**, and the version is
orthogonal, so `actionsgateways.actions-gateway.com` would coexist with
`actionsgateways.actions-gateway.github.com` just as cleanly at `v1alpha1`. The
breaking-ness comes entirely from the group rename and the decomposition (the
fan-out the migration tool handles), not from the version number. `v2alpha1` is a
**deliberate communication choice**: this whole effort is named "v2" throughout the
docs, the migration is "v1→v2", and the graduation ladder targets `v2` — so serving
it as `v1alpha1` would leave the API surface contradicting how everyone refers to
it. It also keeps the in-module Go layout unambiguous (`api/v1alpha1` +
`api/v2alpha1`, rather than two `api/v1alpha1` packages). The minor cost — a fresh
group whose first GA is `v2`, skipping `v1` — is accepted. (Confirmed 2026-06-21.)

The milestones above ship `v2alpha1`. Reaching the stable `v2` group involves two
*different* kinds of transition — do not conflate them:

- **`v1alpha1` → `v2alpha1` is a fan-out, done once.** One v1 object becomes
  several v2 objects, which a conversion webhook cannot express — hence the M5
  migration tool ([§H.11](../design/appendix-h-v2-api-decomposition.md#h11-migration-v2-tool-assisted)).
  This is the only expensive transition.
- **`v2alpha1` → `v2beta1` → `v2` are in-place graduations of the same kinds.**
  Field changes are refinements within the same object shape, so a conversion
  webhook round-trips served versions automatically — no tenant re-apply, no
  migration tool.

We graduate **through beta**, not `alpha → GA` directly: GA's contract is
permanent backward compatibility, and `v2`'s large surface (five kinds,
multi-gateway, cross-references) needs a beta soak — where operators can rely on
it in production while shape problems can still be fixed *with* a migration path —
before that contract is signed.

| Level | Contract |
|---|---|
| `v2alpha1` | may change incompatibly or be dropped without notice; early adopters only |
| `v2beta1` | won't be removed; changes carry a migration path; production-relyable |
| `v2` (GA) | backward-compatible, effectively frozen |

Each graduation hop is cheap but not free:

1. Add the new version to each CRD; mark it the **storage version**.
2. **Conversion webhook** round-trips served versions — [Q74](../STATUS.md#deferred)
   (`Hub`/`Convertible` scaffolding) lands at the first graduation, and is
   distinct from the M5 fan-out tool a conversion webhook cannot replace.
3. **Storage migration** — rewrite stored objects to the new version, then drop
   the superseded served version.

`v1alpha1` is deprecated and removed on its own track once v2 adoption is
sufficient — which is also when the Q147 dual-read window closes (M5,
[§H.12](../design/appendix-h-v2-api-decomposition.md#h12-folding-in-the-grandfathered-label-value-alignment-q147)).

## Definition of done (v2 GA)

v2 is GA when **all** hold: M1, M2, M3a, M3b, and M5 have shipped; the API has
graduated `v2alpha1 → v2beta1 → v2` (conversion webhook + storage migration per
hop); at least one representative tenant has migrated v1→v2 with the tool for
real; `v1alpha1` is deprecated with a named removal release; and the
operator-facing docs (onboarding, migration guide, CRD reference) are updated.
Cross-namespace sharing (M4) and direct egress are **not** GA gates.

**Website per capability.** Any milestone that lands a user-facing capability —
notably **M2** (reusable `RunnerTemplate`/`ClusterRunnerTemplate` golden images)
and **M3b** (multiple gateways per namespace) — updates the positioning pages
([why-gag.md](../why-gag.md), [index.md](../index.md)) in the same PR, so the
competitive story vs ARC tracks what actually ships. Per the
[doc-update matrix](../development/doc-update-matrix.md). Internal-only milestones
(M1 types, M5 tooling) need no website change.

## Deferred (out of the critical path)

### Direct egress (optional-proxy behavior)

The `proxyRef`-optional *schema* lands in M1, but the direct-egress *behavior* —
unset ref ⇒ `proxyMode: Direct`, a default-deny egress NetworkPolicy with no
proxy, the managed GitHub-IP refresh relocated to the gateway/runner-set level,
and an `EgressUnattributed` condition — is additive on M3a and **not** required
for GA, since proxy-required is v1 parity ([§H.10](../design/appendix-h-v2-api-decomposition.md#h10-the-egress-proxy-becomes-optional)).
Ship it as a fast-follow when a proxy-less deployment is actually wanted.

### Optional default RunnerTemplate (Q172)

The parallel relaxation for `templateRef`. At GA it is required (v1 parity; a
worker pod needs a pod shape). Relaxing required → optional is non-breaking, so it
waits for onboarding friction: omit `templateRef` and resolve via
`ActionsGateway.defaultTemplateRef` → a default-marked `ClusterRunnerTemplate`
(the `StorageClass` pattern — at most one default, **fail-closed** `TemplateNotFound`
if none resolves, never a flag-synthesized phantom pod). Collapses minimal
onboarding to two objects. See [§H.4](../design/appendix-h-v2-api-decomposition.md#h4-spec-sketches).

### Bring-your-own proxy autoscaler (Q173)

`targetCPUUtilizationPercentage` is the *managed-default* knob, not the ceiling on
flexibility. Mirroring `managedNetworkPolicy`, add `managedAutoscaling` (default
`true`): GMC manages the proxy HPA by default; setting it `false` makes GMC create
only the proxy Deployment (stable name, labels, `scale` subresource) and **no HPA**,
so an operator can target it with KEDA, VPA, or a custom HPA. Additive (`*bool`), so
deferred until an operator needs it — and distinct from improving the *managed*
metric (CPU → connection-based), which is the Q19 proxy-features work.

### Bring-your-own proxy TLS certificate (Q174)

The proxy CA/cert is GMC-generated (self-signed) by default. Same pattern: add an
operator-supplied `certificateSecretRef` on `EgressProxy` — when set, GMC uses that
TLS Secret instead of generating one, letting operators source proxy certs from an
external PKI/Vault (different algorithm, lifetime, or HSM-backed issuance). **Security
invariant:** the referenced Secret must be a same-namespace TLS Secret — no
cross-tenant reuse. Additive optional field, deferred until an operator with managed
PKI asks. Instantiates [design goal 6](../design/01-executive-summary.md#design-goals).

### M4 — Cross-namespace `EgressProxy` sharing

Additive on M3, gated on a **concrete operator ask** for cross-namespace sharing
(same-namespace sharing already works without it). Adds: inline
`spec.sharing.allowedNamespaces` provider consent, ConfigMap CA distribution into
granted namespaces (trust-manager pattern), dual-side NetworkPolicy, and the
managed-IP refresh relocation for remote consumers ([§H.9](../design/appendix-h-v2-api-decomposition.md#h9-cross-namespace-proxy-sharing)).
Tracked in [STATUS.md Deferred](../STATUS.md#deferred).

### Also deferred / opportunistic (per Appendix H)

- **Admin policy singleton/class** — keep flags; promote on the documented triggers ([§H.14](../design/appendix-h-v2-api-decomposition.md#h14-admin-policy-layer--deferred-until-tiering-is-real)).
- **Worker-image registry allowlist** — lands with the admin policy layer, not as a standalone tenant field ([§H.15](../design/appendix-h-v2-api-decomposition.md#h15-other-breaking-changes-worth-batching)).
- **Credentials discriminated union** — a future `workloadIdentityRef` sibling field is additive; keep the single `githubAppRef` now.
- **Webhook → CEL migration** — opportunistic during M1's schema rewrite, not a gate.

**Architecture review (enhancements evaluated).** A full architecture pass found
no new *breaking* holes — the breaking surface is fully covered by the milestones
above. New *additive* enhancements were filed as backlog rather than pulled into
v2: AGC horizontal-scaling/HA (Q169), Kubernetes Events for job lifecycle (Q170),
and tenant-tunable AGC resources (Q171). Deliberately **not** pre-added to the v2
schema: the proxy feature fields (destination allowlist, audit logging, in-cluster
TLS, per-set dedicated pool — Q19). An optional field added later is non-breaking,
so per the simplicity principle they wait for their trigger; pre-adding them now
would be abstraction ahead of need.

## Testing

Each milestone adds to the existing tiers (see [testing.md](../development/testing.md)):
M1 unit + CRD install; M2/M3a envtest integration (real apiserver: defaulting,
CEL, watch-driven conditions); M3b a kind e2e for per-gateway isolation; M5
migration-tool unit + a round-trip integration test.

Two cross-cutting checks: a **coexistence test** asserting `v1alpha1` keeps
working while `v2alpha1` is served (the no-behavior-change non-goal), and — because
the migration tool targets the latest *served* v2 version — its golden output is
regenerated and re-validated at each graduation (alpha→beta→GA).
