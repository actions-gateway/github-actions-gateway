# Q121 / Q122 / Q125 — GMC RBAC confinement + fail-closed teardown

Three `1.0-gate` findings from [security-audit-2026-06](security-audit-2026-06.md),
sharing the GMC RBAC + reconcile/teardown surface. Implemented together.

## Q121 — GMC Secret RBAC: cluster-wide vs. name-scoped claim

`role.yaml` grants `secrets: get/list/create/update/watch` cluster-wide.
`05-security.md` claimed "no cluster-wide Secret read", metadata-only list,
"`get` on specific names only" — all false.

**What is tightenable, what is not:**

- **Writes** (`create`/`update`) go through admission, so a
  `ValidatingAdmissionPolicy` *can* confine them to tenant-marked
  namespaces. → enforced by the new `gmc-tenant-resource-guard` VAP.
- **Reads** (`get`/`list`/`watch`) do **not** go through admission — VAP
  never sees read verbs. RBAC `resourceNames` cannot scope `list`/`watch`,
  and tenant Secret names are dynamic so it cannot scope `get` either. The
  metadata informer additionally needs cluster-wide `list`/`watch` because
  the GMC discovers tenant namespaces at runtime. **Full read-confinement
  is therefore not achievable via VAP/RBAC alone.**

**Resolution (claim == reality):** tighten writes via the VAP; correct
`05-security.md` to state the honest read scope (cluster-wide get + metadata
list/watch) and the compensating controls: metadata-only informer (no
`.data` cached), uncached direct `get` of only the named credential Secret
in the CR namespace, the write-confinement VAP, and the Q29 detective audit
policy. No more "no cluster-wide read" / "specific names only" claims.

## Q122 — GMC workload writes: cluster-wide vs. CR-namespace claim

deployments / rolebindings / roles / networkpolicies / services /
serviceaccounts / HPAs / PDBs / runnergroups — all writes, all namespaces.
These are all `CREATE`/`UPDATE`/`DELETE`, i.e. fully admission-gated, so the
same VAP confines **all of them** to tenant-marked namespaces. Full
tightening; no doc-downgrade needed. (resourcequotas already dropped by Q130.)

## Shared mechanism — one new VAP

`cmd/gmc/config/admission-policy/tenant-resource-guard.yaml` (sibling to
`namespace-psa-guard.yaml`; kept out of the namePrefix kustomize tree, same
as the PSA guard). Confines the GMC SA's `CREATE`/`UPDATE`/`DELETE` of the
provisioning kinds above to namespaces whose `namespaceObject.metadata.labels`
carry the existing marker `actions-gateway.github.com/tenant=true` — the
same marker the PSA guard already keys on (reused, not reinvented). A
separate policy (not folded into the PSA guard) because VAP `validations`
apply to *every* matched resource and the PSA guard's label/annotation rules
are namespace-UPDATE-specific.

Why it doesn't break normal ops: every GMC write targets `ag.Namespace`,
which must already carry the marker (the PSA guard denies the GMC even the
step-0 namespace patch otherwise). In envtest the reconciler runs as the
admin user, not the GMC SA, so the policy is inert for the provisioning
tests; the VAP tests use an impersonated GMC SA client.

Shipped by the Helm chart (`templates/tenant-resource-guard.yaml`, gated by
the existing `admissionPolicy.enabled`) and applied by `make deploy`.

## Q125 — teardown fail-open

`deleteIfExists` swallowed non-NotFound delete errors and `reconcileDelete`
removed the finalizer unconditionally → a transient API failure orphaned a
live credentialed AGC Deployment + RoleBinding with no retry.

**Fix:** `deleteIfExists` returns its error (NotFound → nil = success).
`reconcileDelete` collects errors across all deletes; if any are non-nil it
emits a `TeardownIncomplete` Warning event and returns the joined error to
requeue **without removing the finalizer**. The finalizer is removed only
once every delete is confirmed gone. Idempotent: repeated passes re-list
RunnerGroups (gone → skip) and re-issue deletes (NotFound → success), so it
converges.

## Tests

- Q121/Q122: `tenant_resource_guard_test.go` (integration) — load the real
  shipped VAP, impersonate the GMC SA, assert writes denied in an unmarked
  namespace and allowed in a marked one, across Secret + a representative
  workload kind; admin not subject.
- Q125: controller unit tests (`fake` + `interceptor.Funcs` forcing a
  Delete error) — finalizer retained + error returned on failed delete;
  finalizer removed on clean teardown.

## Docs

- `05-security.md` — rewrite the GMC-privilege-escalation + credential-leak
  rows so claim == reality (strike-throughs replaced by accurate prose).
- `02-architecture.md` — fix the "writes limited to CR namespaces" claim.
- `security-operations.md` — document the second VAP an operator should see.
- `troubleshooting.md` — Q125: stuck teardown symptom (finalizer retained,
  requeue, `TeardownIncomplete` event).
- mark Q121/Q122/Q125 resolved in `security-audit-2026-06.md`.
- delete Q121/Q122/Q125 rows from `STATUS.md` (isolated commit).
