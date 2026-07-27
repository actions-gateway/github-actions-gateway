# Q415 — validate `gag-migrate` v1→v2 on the dogfood cluster (plan)

**Status:** ▶ Assets built and rehearsed on `kind` 2026-07-27 (two manifest bugs
fixed, one tool defect filed as [Q463](../STATUS.md#Q463)); the **live dogfood run
has not been performed**, so the GA DoD row stays ⚠️ Unverified.
**Scope:** the last unverified item in the v2 GA Definition of Done —
[v2-ga.md § Definition of Done audit](v2-ga.md#definition-of-done-audit-as-of-this-change)
row *"≥1 representative tenant migrated v1→v2 with the tool for real"*, today
marked ⚠️ **Unverified**. Closing it needs a live migration, not another test.

## Goal (one sentence)

Migrate a real, running `v1alpha1` Docker-in-Docker tenant to v2 on the GKE dogfood
cluster with `gag-migrate`, then have the migrated tenant run a real GitHub Actions
DinD job — so the GA DoD row can move from ⚠️ Unverified to ✅ on evidence.

## Why the existing e2e is not enough

[Q414](../../cmd/gmc/test/e2e/migration_v1_to_v2_test.go) already covers the tool
on a live `kind` cluster, and covers it well: dry-run fan-out, the
`ClusterRunnerTemplate`-vs-`RunnerTemplate` admission split, the namespace patch,
v1/v2 coexistence, and the migrated control plane reconciling to Ready. That spec
says so itself — it names this plan as the follow-up.

Three things it deliberately does not do, each of which is where a real migration
can still fail:

| Not covered by Q414 | Why it matters |
|---|---|
| **No worker pod is ever provisioned** — the spec enqueues no job ("The spec is about the fan-out, the admission verdict, and the reconcile — not about running a build"). | A migrated tenant that reconciles but cannot *run* a job satisfies every assertion in the spec and still fails the DoD. |
| **fakegithub, not GitHub** — no real App installation, no real runner registration, no real scale-set. | Migration rewrites the credential and gateway identity. Whether the migrated gateway re-registers against the real GitHub installation is untested. |
| **kind, not GKE** — no `ResourceQuota`, no per-tenant egress proxy with a real egress IP, no node pools, no taints. | The v2 fan-out extracts the inline proxy into a standalone `EgressProxy`. On kind that is a Deployment becoming Ready; on dogfood it is a real egress path to GitHub. |

## What is being validated, precisely

1. `gag-migrate` dry-run then `--apply` against a **live v1 tenant with real
   credentials**, on the cluster GAG actually dogfoods on.
2. The migrated tenant reconciles into a working per-gateway AGC **and** a working
   `EgressProxy` pool.
3. The migrated tenant **acquires and completes a real GitHub Actions job** whose
   worker is a privileged DinD pod — proving the migration preserved the capability,
   not just the object graph.
4. The **Q231 recreate** is exercised and documented: `gag-migrate` emits `v2alpha1`,
   so the conversion webhook stamps
   `conversion.actions-gateway.com/acquisition-protocol: Classic` on the stored
   `v2beta1` object, and the migrated set keeps running Classic until the `RunnerSet`
   is recreated v2beta1-native. This is the one migration step an operator must be
   told about explicitly, and it is exactly the step this run confirms.

**The runner label is deliberately unchanged across the migration**
(`gag-migrate-v1`). v1 Classic permits many labels and v2beta1 ScaleSet permits
exactly one, so choosing a single-label v1 tenant means the workflow's `runs-on`
survives the migration untouched — which is both the friendlier operator story and a
cleaner assertion (same workflow, same label, before and after).

## Assets built by this change

Neither is a permanent fixture. Both describe a path that **ceases to exist at
`v2.0.0`**, when `v1alpha1` is removed ([v2-ga.md](v2-ga.md) Phase 3), so they are
deliberately small and are deleted with the rest of the v1 surface at that cut
rather than maintained forever.

| Asset | Purpose |
|---|---|
| [`deploy/dogfood-migrate/`](../../deploy/dogfood-migrate/README.md) | The `v1alpha1` DinD tenant, mirroring the `utils.DinDTenant` fixture shape the Queue row names, sized for a smoke job rather than the full e2e suite. |
| [`.github/workflows/dogfood-migrate-validate.yml`](../../.github/workflows/dogfood-migrate-validate.yml) | A `workflow_dispatch`-only DinD smoke job targeting the tenant's runner label. Never runs on push or PR. |

### Why a smoke job, not the full e2e suite

The DoD asks for a representative tenant migrated for real, not for a second copy of
the e2e gate. A `docker build` + `docker run` inside the migrated tenant's privileged
DinD worker proves every link in the chain — job acquisition, worker provisioning,
the native-sidecar dockerd, egress to Docker Hub through the migrated proxy, and
completion reporting — at a fraction of the cost and wall-clock of the
kind-in-DinD suite. Running the full suite would validate the *e2e tenant*, which is
already validated, rather than the *migration*.

### Why no reusable script

The repo's convention is to script live dogfood operations under `scripts/dogfood/`
with stub tests in `make check`. This run is deliberately an exception: it is a
one-time DoD checkbox on a code path that is deleted at `v2.0.0`, so a committed
orchestration script plus its stub test would be dead code within one release. The
runbook below is executed step by step instead, and the findings recorded here.

## Runbook

Every step pins `--project`/`--zone`/`--context` explicitly rather than relying on
ambient context, per
[kind-iteration.md § Target the cluster explicitly](../development/kind-iteration.md).
The dogfood cluster is hard-classified prod in `.claude/prod-guard.json`, so each
mutating command needs `PROD_GUARD_OVERRIDE` naming this plan.

```bash
export PROJECT=actions-gateway-dogfood CLUSTER=gag-dogfood ZONE=us-east1-b
export REPO=actions-gateway/github-actions-gateway
```

1. **Bring the cluster up.** `scripts/dogfood/start.sh` (system pool from 0 to the
   derived running size). The `e2e` node pool autoscales 0→N on job arrival on its
   own.
2. **Create the tenant namespace and its App credential Secret** (out of band — a
   credential, never in git), then apply the v1 tenant:
   `kubectl apply -k deploy/dogfood-migrate`.
3. **Wait for the v1 AGC to be Ready.** Migrating a tenant that never came up would
   prove nothing — establish a live v1 tenant first, exactly as the Q414 spec does.
4. **Baseline (optional but cheap):** dispatch the smoke workflow against the v1
   tenant and confirm green *before* migrating. Without this, a post-migration
   failure cannot be attributed to the migration.
5. **Dry run:** `gag-migrate --namespace gag-dogfood-migrate --context <ctx>`.
   Review the fan-out and both warnings (cluster-scoped object; namespace patch needs
   the downgrade opt-in).
6. **Grant the downgrade opt-in** the dry run asks for
   (`allow-profile-downgrade=allowed` on the namespace). The tool deliberately does
   not write this — opting into a downgrade is the operator's call.
7. **Apply:** `gag-migrate … --apply`. Verify the emitted object set was admitted and
   that the v1 objects survive alongside it.
8. **Wait for the migrated control plane** — the `<gateway>-agc` Deployment and the
   `<gateway>-egress-proxy` pool.
9. **Recreate the `RunnerSet` (Q231)** so it comes back v2beta1-native on ScaleSet
   rather than inheriting `Classic` from the conversion annotation. Confirm the
   annotation is gone and the set registers a scale set at GitHub.
10. **Dispatch the smoke workflow again** — same workflow, same `runs-on` label — and
    confirm it goes green on the migrated tenant.
11. **Tear down:** delete the tenant namespace and the cluster-scoped migration
    outputs (the `ClusterRunnerTemplate` by its
    `MigratedFromNamespaceLabel` provenance label and the matching
    ClusterRoleBinding — namespace deletion does not reclaim them, which is the whole
    reason the tool stamps the label), then `scripts/dogfood/stop.sh`.

## Acceptance criteria

The run closes Q415 only if all of these hold, each recorded with its evidence in
Findings below:

- [ ] `gag-migrate --apply` completes without error against the live v1 tenant.
- [ ] The migrated `<gateway>-agc` and `<gateway>-egress-proxy` Deployments reach
      Ready.
- [ ] The v1 objects survive the migration (coexistence, so rollback stays possible).
- [ ] After the Q231 recreate, the `RunnerSet` is ScaleSet-native (no
      `acquisition-protocol: Classic` annotation) and registers at GitHub.
- [ ] A real GitHub Actions DinD job completes **green** on the migrated tenant.
- [ ] Teardown leaves no orphaned cluster-scoped objects.

Anything that fails is a finding to fix, not a reason to soften the criterion — this
is the run that decides whether the DoD row is honest.

## Cost and blast radius

- **Billable:** the system pool for the run window plus one `c2-standard-8` spot node
  in the `e2e` pool while the smoke job runs. Both return to zero on teardown.
- **Blast radius:** a new, isolated namespace (`gag-dogfood-migrate`). The always-on
  `dogfood`/`dogfoodss` CI tenants and the on-demand `gag-dogfood-e2e` tenant are
  untouched, and no repo variable that routes real CI is changed — the smoke workflow
  is `workflow_dispatch`-only and targets its own label.
- **Not reversible by itself:** the namespace metadata patch and the cluster-scoped
  `ClusterRunnerTemplate`. Both are reclaimed by step 11.

## Findings

### Rehearsal on `kind` (2026-07-27) — assets validated, one tool defect found

Before spending anything on GKE, the tenant manifest was applied to the local `kind`
e2e cluster and `gag-migrate` was run against it in dry-run. This validated the
asset, not the migration — the live run below is still outstanding.

**Validated.** All four objects pass the real v1 CRD schema and the GMC validating
webhook, including `securityProfile: privileged`. The dry-run fans the tenant out to
all four expected v2 kinds (`ActionsGateway`, `EgressProxy`, `ClusterRunnerTemplate`,
`RunnerSet`), puts the privileged worker shape on the **cluster-scoped** kind, and
emits the two expected warnings (cluster-scoped object; namespace patch needs the
downgrade opt-in).

**Two manifest bugs the rehearsal caught**, both of which would have failed only
after the cluster was already up:

1. `v1alpha1` spells the credential fields `gitHubAppRef` / `gitHubURL` — capital H —
   unlike v2's `githubURL`. Both are required, so the wrong casing is a hard reject.
2. The privileged grant was initially written on the **v2** label domain (copied from
   the v2 e2e overlay). It must be the v1 domain for an authentic pre-migration
   tenant — see the defect below for why that distinction is load-bearing.

### Defect found: `gag-migrate`'s grant detection does not dual-read

The v1 validating webhook **dual-reads** the privileged-eligibility grant across both
label domains for the v1/v2 coexistence window — deliberately, per §H.12, so that
relabelling a namespace onto the v2 domain does not strand a still-running v1
gateway ([actionsgateway_webhook.go:279](../../cmd/gmc/internal/webhook/v1alpha1/actionsgateway_webhook.go)).

`gag-migrate` does not. Its carry-forward check reads only the **v1** domain
([migrate.go:387](../../cmd/gmc/internal/migrate/migrate.go)), while the warning it
emits on a miss names the **v2** label:

> namespace … migrates to securityProfile=privileged but holds no
> `actions-gateway.com/privileged-profile=allowed` grant; a platform administrator
> must apply the v2 eligibility label or the profile will be rejected

So a namespace granted eligibility via the v2 label alone — legal, and admitted by
the webhook — is reported as ungranted, and the remedy the warning prescribes is the
label that is *already there*. Re-running after following the advice produces the
identical warning. Observed directly: the first rehearsal namespace carried
`actions-gateway.com/privileged-profile=allowed`, was admitted privileged by the
webhook, and was still warned about.

**Impact is low but the shape is bad.** The end state stays correct — the v2 label is
present either way, so the skipped carry-forward is a no-op — so this is an
unactionable, self-contradicting warning rather than a broken migration. It is
exactly the rough edge Q273 set out to polish, and it surfaces during the one
operation where an operator is least able to judge whether a warning is safe to
ignore. Filed as [Q463](../STATUS.md#Q463); **not** fixed here, because changing the
tool's behaviour in the middle of the run that validates it would invalidate the run.

Working around it is why this plan's tenant grants privileged on the **v1** label
domain: that is both the authentic pre-migration shape and the path that actually
exercises the grant carry-forward.

### Live run

*Not yet performed. Nothing is recorded here ahead of the evidence.*
