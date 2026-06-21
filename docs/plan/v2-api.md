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

## Resolved design decisions

All settled in [§H.16](../design/appendix-h-v2-api-decomposition.md#h16-open-questions--sign-off-needed):

- **Admin policy → keep controller flags** (singleton/class deferred behind triggers, [§H.14](../design/appendix-h-v2-api-decomposition.md#h14-admin-policy-layer--deferred-until-tiering-is-real)).
- **API group → `actions-gateway.com`**; **`gitHubURL` immutable**, `gitHubAppRef.name` mutable; **`maxListeners` default → `10`**; drop `SecretReference.namespace`.
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
- **Exit:** CRDs install and round-trip via the API server alongside `v1alpha1`;
  `make check` green; no reconciler references the new kinds yet.

### M2 — Data kinds (nouns)

- `EgressProxy` reconciler in the GMC: owns its proxy Deployment / Service / HPA /
  PDB ([§H.8](../design/appendix-h-v2-api-decomposition.md#h8-ownership-gc-and-deletion)). **Same-namespace only** at this stage.
- `RunnerTemplate` / `ClusterRunnerTemplate`: pure data; the reserved-pod-field
  rejection webhook moves here from `RunnerGroup` ([§H.4](../design/appendix-h-v2-api-decomposition.md#h4-spec-sketches)).
- **Exit:** a standalone `EgressProxy` reconciles a working proxy pool; a
  `RunnerTemplate` validates and is readable by name; envtest coverage for both.

### M3 — Control kinds (verbs) + multi-gateway *(the core build)*

- `ActionsGateway` + `RunnerSet` reconcilers.
- **Multi-gateway per namespace** ([§H.16 #1](../design/appendix-h-v2-api-decomposition.md#h16-open-questions--sign-off-needed)):
  per-gateway resource naming under the 52-char cap; **AGC scoping** so each AGC
  reconciles only the `RunnerSet`s whose `gatewayRef` targets it; per-gateway
  ownership for clean GC. The `gmc-tenant-resource-guard` VAP is unchanged (it
  keys on the namespace marker, not names).
- **Reference resolution at runtime** ([§H.7](../design/appendix-h-v2-api-decomposition.md#h7-reference-integrity--runtime-conditions-not-admission)):
  resolve `templateRef`/`proxyRef` via watch + enqueue; surface
  `TemplateNotFound`/`ProxyNotFound` conditions; fail-closed (no wiring until refs
  resolve).
- **Optional proxy** ([§H.10](../design/appendix-h-v2-api-decomposition.md#h10-the-egress-proxy-becomes-optional), absorbs Q144):
  unset `proxyRef` ⇒ direct egress with an explicit `proxyMode: Direct` status and
  an `EgressUnattributed` advisory condition; the default-deny egress
  NetworkPolicy stays mandatory; the managed GitHub-IP refresh loop relocates to
  the gateway/runner-set level.
- **Exit:** two `ActionsGateway`s with their own `RunnerSet`s run concurrently in
  one namespace, one proxied and one direct-egress; envtest + a kind e2e prove
  per-gateway isolation and reference-resolution conditions.

### M5 — Migration tool + v1/v2 cutover

- One-shot fan-out migration tool ([§H.11](../design/appendix-h-v2-api-decomposition.md#h11-migration-v2-tool-assisted)):
  reads `v1alpha1` CRs, emits the `v2alpha1` object set (extract inline
  `podTemplate` → `RunnerTemplate`, inline `proxy` → `EgressProxy`, rewrite
  references). Dry-run to manifests by default; `--apply` to apply. Plus tests.
- **Q147 dual-read window** ([§H.12](../design/appendix-h-v2-api-decomposition.md#h12-folding-in-the-grandfathered-label-value-alignment-q147)):
  VAPs + downgrade webhook accept either `"true"` (legacy) or the new keyword;
  the tool rewrites markers/annotations during the pass.
- Operator migration guide; `v1alpha1` deprecation notice.
- **Exit:** the tool migrates a representative v1 namespace to a working v2 object
  set in dry-run and `--apply`; dual-read verified; docs updated.

## Deferred (out of the critical path)

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
- **Credentials discriminated union** — a future `workloadIdentityRef` sibling field is additive; keep the single `gitHubAppRef` now.
- **Webhook → CEL migration** — opportunistic during M1's schema rewrite, not a gate.

## Testing

Each milestone adds to the existing tiers (see [testing.md](../development/testing.md)):
M1 unit + CRD install; M2/M3 envtest integration (real apiserver: defaulting,
CEL, watch-driven conditions); M3 a kind e2e for per-gateway isolation and
direct-vs-proxied egress; M5 migration-tool unit + a round-trip integration test.
