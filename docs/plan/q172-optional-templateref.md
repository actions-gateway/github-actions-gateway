# Q172 — v2 optional templateRef (default ClusterRunnerTemplate)

## Goal
Relax the v2 `RunnerSet.spec.templateRef` from **required** to **optional**, non-breaking:
an unset `templateRef` resolves a worker pod shape via a fallback chain so minimal
onboarding no longer has to name a template. The templateRef analog of the
optional-proxy work (Q168). Design: appendix-h §H.4 (the "Why `templateRef` is required
but `proxyRef` is optional" note marks this exact relaxation as deferred-but-planned)
and §H.7 (runtime reference resolution, fail-closed).

## Resolution chain (AGC `RunnerSet` reconciler, runtime, fail-closed §H.7)
1. `rs.spec.templateRef` (explicit) — unchanged behavior; source `TemplateRef`.
2. else `gateway.spec.defaultTemplateRef` — per-gateway default; source `GatewayDefault`.
3. else the **single** cluster-default `ClusterRunnerTemplate` (marked by annotation);
   source `ClusterDefault`.
4. else **`TemplateNotFound`** — fail closed, no worker wiring. Never a phantom pod.

Each rung resolves at runtime via the existing watch + enqueue mappers, so a set
flips Ready the moment a missing referent (or default) syncs — no apply ordering.

## Decisions

### Marker for the cluster-default — annotation (StorageClass pattern)
`actions-gateway.com/is-default-template: "true"` on a `ClusterRunnerTemplate`.
Mirrors `storageclass.kubernetes.io/is-default-class: "true"` exactly — the design
explicitly invokes "the StorageClass pattern", and operators already know it. Value
is the literal `"true"` to match that idiom (the key already reads "is-default", so a
keyword value would be redundant). No template-spec CRD change for the marker itself;
the constant lives in `api/v2alpha1/shared_types.go` with the other domain keys.

Only the **cluster-scoped** `ClusterRunnerTemplate` carries the default marker: a
cluster-default must be platform-authored (tenants cannot create cluster-scoped
objects), so a tenant cannot self-elect a namespaced `RunnerTemplate` as the
cluster-wide default. A `gateway.defaultTemplateRef` may still point at either kind.

### ≤1 cluster-default — runtime condition, not admission
Enforced at **resolution time** in the AGC reconciler, not at admission:
- §H.7 already places existence/referential concerns at runtime, not in admission
  (admission breaks GitOps apply-ordering, and CEL cannot express a cross-object
  invariant — it would need a VAP-with-list or a webhook doing a `List`).
- The ambiguity only *matters* when a `RunnerSet` actually falls through to the
  cluster-default rung, so the natural place to surface it is the **consumer**
  (the `RunnerSet`), at the moment it depends on the default.
- When the cluster-default rung lists **≥2** marked templates, fail closed with a new
  `AmbiguousDefault` reason (`Ready=False`) naming the conflicting templates — never
  silently pick one (unlike upstream StorageClass, which picks newest-wins; we are
  stricter per the secure-by-default invariant).

Trade-off: admission rejection would give earlier feedback, but breaks apply-ordering
and is not expressible in single-object CEL. Runtime-with-condition matches §H.7 and
is fail-closed regardless.

### Surfacing which rung resolved
New `RunnerSet.status.templateSource` enum (`TemplateRef` | `GatewayDefault` |
`ClusterDefault`), mirroring the `status.proxyMode` precedent from Q168, plus the rung
named in the `Ready=True` message. Auditable: an operator can see whether a set is on
an explicit ref or a default without inspecting the gateway/cluster.

### Relaxing required → optional
`RunnerSet.spec.templateRef` becomes `*ObjectRef` + `+optional` (was a required value
type), matching `proxyRef`'s shape. Wire-compatible: a set that sets `templateRef`
serializes and validates exactly as today; only the CRD `required` entry drops.

## Files
- `api/v2alpha1/runnerset_types.go` — `TemplateRef *ObjectRef` +optional; add
  `status.TemplateSource`.
- `api/v2alpha1/actionsgateway_types.go` — add `DefaultTemplateRef *ObjectRef`.
- `api/v2alpha1/shared_types.go` — `IsDefaultTemplateAnnotation` / value.
- `api/v2alpha1/conditions.go` — `ReasonAmbiguousDefault`; `TemplateSource*` consts.
- `cmd/agc/internal/controller/runnerset_target.go` — the chain in
  `resolveRunnerSetRefs` / a new `resolveTemplateChain`; `resolvedRefs.templateSource`.
- `cmd/agc/internal/controller/runnerset_controller.go` — set `status.TemplateSource`;
  Ready message names the rung; broaden the template watch mappers so a set on a
  default re-reconciles when the default appears/changes.
- `cmd/gmc/internal/migrate/migrate.go` — `&v2alpha1.ObjectRef{...}` (pointer).
- regen: `make -C api generate`, `make -C cmd/gmc manifests`, `make chart-crds`.

## Tests
- Unit (`runnerset_test.go`, fake client): each rung; fail-closed when none; ≤1-default
  (exactly-one resolves, ≥2 → AmbiguousDefault); namespaced RunnerTemplate is never
  treated as a cluster-default.
- Envtest (`v2_runnerset_test.go`): unset templateRef resolves via gateway
  defaultTemplateRef → Ready; via cluster-default → Ready; none → TemplateNotFound;
  templateSource surfaced.

## Docs
- appendix-h §H.4 — mark shipped; document the chain + the ≤1-default runtime choice.
- `docs/operations/tenant-onboarding.md` — you may omit `templateRef` when a default
  exists; how to mark a cluster-default.
- CRD field comments (godoc) regenerated into the CRDs.
