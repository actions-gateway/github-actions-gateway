# Q414 — Representative DinD tenant fixture, and the migration gap it found

> **Status:** ✅ Complete (2026-07-26).
> Every deliverable below shipped in one change.

Q414 asked for one reusable v1 tenant fixture carrying a Docker-in-Docker (DinD) worker shape, and for `gag-migrate`'s v1→v2 fan-out to be covered in e2e rather than only in unit tests.
[Q415](q415-migrate-dogfood-validation.md) — migrate a representative tenant on the dogfood cluster for real — was blocked on it, because the [v2 GA Definition of Done](../v2-api.md#definition-of-done-v2-ga) wants exactly one real tenant migrated and our own dogfood e2e tenant is the only one that exists.

Building the fixture surfaced a defect that makes both of those impossible today.

## Finding — `gag-migrate` cannot migrate a DinD tenant (measured 2026-07-26)

`FanOut` always emits a **namespaced** `RunnerTemplate` for a v1 `RunnerGroup`'s `podTemplate` ([migrate.go](../../../cmd/gmc/internal/migrate/migrate.go)).
The v2 admission webhook rejects a privileged container on that kind by design — privileged worker shapes belong on the platform-owned, cluster-scoped `ClusterRunnerTemplate` ([§H.6](../../design/appendix-h-v2-api-decomposition.md), [runnertemplate_webhook.go](../../../cmd/gmc/internal/webhook/v2alpha1/runnertemplate_webhook.go)).
A DinD tenant's worker pod is privileged by construction (the `dockerd` sidecar), so the two rules collide.

Measured directly by running `FanOut` over a v1 DinD tenant shape and feeding the emitted template to the real `RunnerTemplateCustomValidator`:

```
templates=1 sets=1 warnings=[]
RunnerTemplate "rt-c44dc9368ef1" ValidateCreate err =
  podTemplate.spec.initContainers["dind"]: privileged containers are not permitted in a
  namespaced RunnerTemplate; use a platform-owned ClusterRunnerTemplate for privileged
  (DinD/sysbox) worker shapes
```

Three things make this worse than a plain rejection:

1. **No dry-run warning.** `FanOut` returns `warnings=[]`, so the reviewed dry-run output looks clean.
   The operator only learns of the problem when `--apply` fails.
2. **`--apply` fails half-done.** `applyResult` creates objects in the order `EgressProxy → RunnerTemplates → ActionsGateway → RunnerSets` ([main.go](../../../cmd/gmc/migrate/main.go)).
   The template is the *second* object, so the run aborts with the `EgressProxy` already created and no gateway or sets — a partially-migrated namespace.
3. **It breaks the tool's own stated invariant.** The runbook promises the fan-out "preserves v1 behavior and weakens no security property" ([migration-v1-to-v2.md](../../operations/migration-v1-to-v2.md)).
   For a privileged tenant it preserves nothing: v1 ran the workload fine (v1's webhook permits privileged containers when the gateway opts into `securityProfile: privileged` and the namespace holds the platform grant), and v2 refuses to accept it at all.

The design never covered this case: [§H.11](../../design/appendix-h-v2-api-decomposition.md#h11-migration-v2-tool-assisted) says only "extracting each inline `podTemplate` into a `RunnerTemplate`".

## Two more defects, found only by running it

The template fix was necessary but not sufficient.
Standing the e2e up against a live cluster found two further blockers on the same path — neither reachable by a unit test, because each depends on a real apiserver evaluating a real admission policy.

### Finding 2 — a lone privileged tenant was silently downgraded to `baseline`

`MostRestrictiveProfile` seeded its accumulator with `baseline` unconditionally.
But `privileged` **ranks below** `baseline` (it is the least restrictive level, rank 0), so a single privileged gateway could never beat the seed: `MostRestrictiveProfile("privileged")` returned `"baseline"`.
The CLI's namespace-wide override in `migrateNamespace` then clobbered the correct `privileged` value that `buildNamespacePatch` had already computed.

Direction-safe, outcome-wrong: the migrated tenant's namespace enforced `baseline`, so Pod Security Admission would refuse the very DinD worker pods the tenant migrated in order to keep running.
Fixed by seeding from the first input profile instead — every genuine multi-gateway comparison is unchanged (strictest still wins), and a single gateway now migrates to exactly its own profile.

### Finding 3 — the downgrade guard rejects every privileged relocation

Fixing finding 2 exposed the next one.
`namespace-security-profile-guard` compares the incoming `security-profile` label against the current one, and an **absent** label reads as `baseline`.
A tenant coming from v1 has never carried the v2 label, so relocating `privileged` always presents as `baseline` → `privileged` — a downgrade — and is denied:

```
namespaces "tenant-migrate-dind" is forbidden: ValidatingAdmissionPolicy
'gmc-namespace-security-profile-guard' denied request: security profile downgrade from
baseline to privileged is not permitted without the
actions-gateway.com/allow-profile-downgrade=allowed annotation
```

The downgrade is only apparent — the namespace's PSA level is already `privileged` under v1 — but the policy cannot see that, and it must not: it is what stops a stray re-apply from silently relaxing a tenant's isolation.

Resolved by **warning, not self-granting**.
Writing the opt-in annotation would be the tool inventing a security decision on the operator's behalf, the same thing the privileged-eligibility grant is deliberately never invented for.
The dry-run now prints the exact remediation, and the runbook documents the annotate → migrate → un-annotate sequence.
Without the warning the operator discovers this only when `--apply` fails at its *last* step, with every v2 object already created.

### Why the order matters

Each defect masked the next.
Only the template fix let the apply reach the RunnerSet; only the profile fix let it reach the namespace patch; only then did the downgrade guard speak up.
A test tier that stops at "the right Go structs were produced" could not have found any of the three.

## Fix — map a privileged v1 shape onto `ClusterRunnerTemplate`

`ClusterRunnerTemplate` exists *precisely* to hold golden privileged DinD/sysbox templates (§H.6), and `RunnerSet.spec.templateRef.kind` already selects it, so the mapping needs no new API surface:

- A v1 `RunnerGroup` whose `podTemplate` carries a privileged container (or init container) fans out to a **`ClusterRunnerTemplate`** instead of a `RunnerTemplate`, and its `RunnerSet` gets `templateRef.kind: ClusterRunnerTemplate`.
- Non-privileged groups are untouched — they keep emitting a namespaced `RunnerTemplate` with no `kind` discriminator, so every existing golden file and every non-privileged tenant migrates byte-for-byte as before.

**No security property is weakened.** Pod Security Admission, stamped from the namespace's `security-profile` label, remains the runtime backstop for both template kinds — the design's own argument for why privileged is permitted on the cluster-scoped kind.
The tool already carries the platform's privileged grant forward (and warns, never self-grants, when it is missing), so a migrated privileged tenant is admitted at runtime under exactly the grant its platform admin already gave it.

Two deliberate choices:

- **Names are namespace-qualified** (`crt-<ns>-<hash>`, not the bare content address the namespaced kind uses).
  Cluster-scoped names are cluster-unique, so a pure content address would silently *share* one object between two tenants with the same worker shape — and a later edit by one tenant's operator would change the other's worker pods.
  Per-namespace names keep the v1 property that each tenant owns its own template.
- **The emitted object is labelled with its origin namespace** (`actions-gateway.com/migrated-from-namespace`).
  A namespaced migration now creates a cluster-scoped object, which namespace deletion does *not* garbage-collect, so an operator needs a way to find and tear down what a migration left behind.

The tool also warns on every emitted cluster template — creating a cluster-scoped object from a namespace-scoped migration is a blast-radius change the operator must see in the dry-run before approving `--apply`.

## Deliverables

| # | Deliverable | Status |
|---|---|---|
| 1 | Measure the DinD migration path end to end | ✅ Done — finding above |
| 2 | `FanOut` emits `ClusterRunnerTemplate` for privileged shapes; `RenderManifests` + `applyResult` carry it | ✅ [`migrate.go`](../../../cmd/gmc/internal/migrate/migrate.go) `templateRefFor` |
| 2b | A lone privileged tenant keeps its profile (finding 2) | ✅ `MostRestrictiveProfile` seeds from the input |
| 2c | The downgrade guard's rejection is warned about, not self-granted (finding 3) | ✅ `warnIfDowngradeGuardWillReject` |
| 3 | One reusable v1 tenant fixture in `cmd/gmc/test/utils`, replacing the four near-duplicate `ApplyActionsGatewayCR*` builders, with a DinD preset | ✅ [`fixtures.go`](../../../cmd/gmc/test/utils/fixtures.go); 13 call sites converted |
| 4 | e2e spec covering the v1→v2 fan-out on a live cluster, including the DinD tenant | ✅ [`migration_v1_to_v2_test.go`](../../../cmd/gmc/test/e2e/migration_v1_to_v2_test.go) |
| 5 | Runbook + design + conventions docs updated | ✅ [runbook](../../operations/migration-v1-to-v2.md#privileged-worker-shapes-dind-become-cluster-scoped-templates), [§H.11](../../design/appendix-h-v2-api-decomposition.md#h11-migration-v2-tool-assisted), [conventions](../../development/kubernetes-conventions.md) |

## Test coverage

- **Unit** ([`privileged_test.go`](../../../cmd/gmc/internal/migrate/privileged_test.go)) — the kind choice, per-group splitting on a mixed tenant, within-namespace reuse collapse, cross-namespace non-sharing, name truncation, and a golden manifest for the DinD tenant.
  The decisive one is `TestFanOut_EmittedTemplatesAreAdmissible`: it runs the **real** v2 admission validators over everything `FanOut` emits, so the assertion is made against the admission rule itself rather than a restatement of it — that is the invariant that actually broke.
- **e2e** ([`migration_v1_to_v2_test.go`](../../../cmd/gmc/test/e2e/migration_v1_to_v2_test.go)) — dry-run writes nothing and warns; `--apply` produces an object set the live apiserver admits, with the namespace patch applied and v1 still running beside it; and the migrated tenant reconciles into a working per-gateway AGC + proxy pool.
  The last one is what a unit test can never reach: admissible YAML is not the same as a tenant that runs.
  Findings 2 and 3 above were both caught here and nowhere else.

**Verified live**, 3/3 specs green on a from-scratch kind cluster pinned to the CI Kubernetes version (`kindest/node:v1.35.5`), 2026-07-26.

## Local-loop notes (not product defects)

Two re-run artifacts cost time here and are worth knowing before iterating on this spec:

- **Re-running the suite over `E2E_SKIP_TEARDOWN=true` strands the tenant namespace.** The spec's `AfterAll` deletes the namespace, which takes the tenant AGC with it, and the `agentpool-cleanup` finalizers on its RunnerGroup/RunnerSet then have nothing left to clear them — the Q301 shape `drainTenantNamespaces` exists to prevent, bypassed by the skip flag.
  Clear the finalizers by hand, or don't skip teardown.
- **`helm uninstall` → reinstall leaves the PriorityClass allowlist VAP unable to resolve its params**, so every `runnergroups`/`runnersets` write is denied with `no params found for policy binding` even though the param ConfigMap is present at exactly the referenced name and namespace (verified directly on Kubernetes 1.36.1; the same denial appeared on 1.35.5 after a reinstall).
  A from-scratch cluster is unaffected, which is why CI never sees it.
  Filed separately — it is an operator-facing chart-reinstall hazard, not something this change introduced.

## Why a fixture, not another one-off builder

`cmd/gmc/test/utils/resources.go` grew four `ApplyActionsGatewayCR*` functions that differ only in which RunnerGroup knobs they set, each carrying a near-identical copy of the same 30-line YAML and the same three explanatory comment blocks.
Adding a fifth for DinD would have been the obvious move and the wrong one.
The fixture is one options struct with presets, so a new shape is a few fields rather than another copy of the whole document.
