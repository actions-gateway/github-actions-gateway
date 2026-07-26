# Q414 — Representative DinD tenant fixture, and the migration gap it found

> **Status:** ✅ Complete (2026-07-26). Every deliverable below shipped in one change.

[Q414](../../STATUS.md#Q414) asked for one reusable v1 tenant fixture carrying a
Docker-in-Docker (DinD) worker shape, and for `gag-migrate`'s v1→v2 fan-out to be
covered in e2e rather than only in unit tests. [Q415](../../STATUS.md#Q415) — migrate a
representative tenant on the dogfood cluster for real — is blocked on it, because
the [v2 GA Definition of Done](../v2-api.md#definition-of-done-v2-ga) wants exactly one
real tenant migrated and our own dogfood e2e tenant is the only one that exists.

Building the fixture surfaced a defect that makes both of those impossible today.

## Finding — `gag-migrate` cannot migrate a DinD tenant (measured 2026-07-26)

`FanOut` always emits a **namespaced** `RunnerTemplate` for a v1 `RunnerGroup`'s
`podTemplate` ([migrate.go](../../../cmd/gmc/internal/migrate/migrate.go)). The v2
admission webhook rejects a privileged container on that kind by design — privileged
worker shapes belong on the platform-owned, cluster-scoped `ClusterRunnerTemplate`
([§H.6](../../design/appendix-h-v2-api-decomposition.md), [runnertemplate_webhook.go](../../../cmd/gmc/internal/webhook/v2alpha1/runnertemplate_webhook.go)).
A DinD tenant's worker pod is privileged by construction (the `dockerd` sidecar), so
the two rules collide.

Measured directly by running `FanOut` over a v1 DinD tenant shape and feeding the
emitted template to the real `RunnerTemplateCustomValidator`:

```
templates=1 sets=1 warnings=[]
RunnerTemplate "rt-c44dc9368ef1" ValidateCreate err =
  podTemplate.spec.initContainers["dind"]: privileged containers are not permitted in a
  namespaced RunnerTemplate; use a platform-owned ClusterRunnerTemplate for privileged
  (DinD/sysbox) worker shapes
```

Three things make this worse than a plain rejection:

1. **No dry-run warning.** `FanOut` returns `warnings=[]`, so the reviewed dry-run
   output looks clean. The operator only learns of the problem when `--apply` fails.
2. **`--apply` fails half-done.** `applyResult` creates objects in the order
   `EgressProxy → RunnerTemplates → ActionsGateway → RunnerSets`
   ([main.go](../../../cmd/gmc/migrate/main.go)). The template is the *second* object, so
   the run aborts with the `EgressProxy` already created and no gateway or sets —
   a partially-migrated namespace.
3. **It breaks the tool's own stated invariant.** The runbook promises the fan-out
   "preserves v1 behavior and weakens no security property"
   ([migration-v1-to-v2.md](../../operations/migration-v1-to-v2.md)). For a privileged
   tenant it preserves nothing: v1 ran the workload fine (v1's webhook permits
   privileged containers when the gateway opts into `securityProfile: privileged`
   and the namespace holds the platform grant), and v2 refuses to accept it at all.

The design never covered this case: [§H.11](../../design/appendix-h-v2-api-decomposition.md#h11-migration-v2-tool-assisted)
says only "extracting each inline `podTemplate` into a `RunnerTemplate`".

## Fix — map a privileged v1 shape onto `ClusterRunnerTemplate`

`ClusterRunnerTemplate` exists *precisely* to hold golden privileged DinD/sysbox
templates (§H.6), and `RunnerSet.spec.templateRef.kind` already selects it, so the
mapping needs no new API surface:

- A v1 `RunnerGroup` whose `podTemplate` carries a privileged container (or init
  container) fans out to a **`ClusterRunnerTemplate`** instead of a `RunnerTemplate`,
  and its `RunnerSet` gets `templateRef.kind: ClusterRunnerTemplate`.
- Non-privileged groups are untouched — they keep emitting a namespaced
  `RunnerTemplate` with no `kind` discriminator, so every existing golden file and
  every non-privileged tenant migrates byte-for-byte as before.

**No security property is weakened.** Pod Security Admission, stamped from the
namespace's `security-profile` label, remains the runtime backstop for both template
kinds — the design's own argument for why privileged is permitted on the
cluster-scoped kind. The tool already carries the platform's privileged grant forward
(and warns, never self-grants, when it is missing), so a migrated privileged tenant is
admitted at runtime under exactly the grant its platform admin already gave it.

Two deliberate choices:

- **Names are namespace-qualified** (`crt-<ns>-<hash>`, not the bare content address
  the namespaced kind uses). Cluster-scoped names are cluster-unique, so a pure
  content address would silently *share* one object between two tenants with the same
  worker shape — and a later edit by one tenant's operator would change the other's
  worker pods. Per-namespace names keep the v1 property that each tenant owns its own
  template.
- **The emitted object is labelled with its origin namespace**
  (`actions-gateway.com/migrated-from-namespace`). A namespaced migration now creates
  a cluster-scoped object, which namespace deletion does *not* garbage-collect, so an
  operator needs a way to find and tear down what a migration left behind.

The tool also warns on every emitted cluster template — creating a cluster-scoped
object from a namespace-scoped migration is a blast-radius change the operator must
see in the dry-run before approving `--apply`.

## Deliverables

| # | Deliverable | Status |
|---|---|---|
| 1 | Measure the DinD migration path end to end | ✅ Done — finding above |
| 2 | `FanOut` emits `ClusterRunnerTemplate` for privileged shapes; `RenderManifests` + `applyResult` carry it | ✅ [`migrate.go`](../../../cmd/gmc/internal/migrate/migrate.go) `templateRefFor` |
| 3 | One reusable v1 tenant fixture in `cmd/gmc/test/utils`, replacing the four near-duplicate `ApplyActionsGatewayCR*` builders, with a DinD preset | ✅ [`fixtures.go`](../../../cmd/gmc/test/utils/fixtures.go); 13 call sites converted |
| 4 | e2e spec covering the v1→v2 fan-out on a live cluster, including the DinD tenant | ✅ [`migration_v1_to_v2_test.go`](../../../cmd/gmc/test/e2e/migration_v1_to_v2_test.go) |
| 5 | Runbook + design + conventions docs updated | ✅ [runbook](../../operations/migration-v1-to-v2.md#privileged-worker-shapes-dind-become-cluster-scoped-templates), [§H.11](../../design/appendix-h-v2-api-decomposition.md#h11-migration-v2-tool-assisted), [conventions](../../development/kubernetes-conventions.md) |

## Test coverage

- **Unit** ([`privileged_test.go`](../../../cmd/gmc/internal/migrate/privileged_test.go)) —
  the kind choice, per-group splitting on a mixed tenant, within-namespace reuse
  collapse, cross-namespace non-sharing, name truncation, and a golden manifest for the
  DinD tenant. The decisive one is `TestFanOut_EmittedTemplatesAreAdmissible`: it runs
  the **real** v2 admission validators over everything `FanOut` emits, so the assertion
  is made against the admission rule itself rather than a restatement of it — that is
  the invariant that actually broke.
- **e2e** ([`migration_v1_to_v2_test.go`](../../../cmd/gmc/test/e2e/migration_v1_to_v2_test.go))
  — dry-run writes nothing and warns; `--apply` produces an object set the live
  apiserver admits, with the namespace patch applied and v1 still running beside it;
  and the migrated tenant reconciles into a working per-gateway AGC + proxy pool. The
  last one is what a unit test can never reach: admissible YAML is not the same as a
  tenant that runs.

## Why a fixture, not another one-off builder

`cmd/gmc/test/utils/resources.go` grew four `ApplyActionsGatewayCR*` functions that
differ only in which RunnerGroup knobs they set, each carrying a near-identical copy
of the same 30-line YAML and the same three explanatory comment blocks. Adding a
fifth for DinD would have been the obvious move and the wrong one. The fixture is one
options struct with presets, so a new shape is a few fields rather than another copy
of the whole document.
