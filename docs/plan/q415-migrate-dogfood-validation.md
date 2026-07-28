# Q415 — validate `gag-migrate` v1→v2 on the dogfood cluster (plan)

**Status:** ▶ Migration validated live on GKE 2026-07-27; the **DinD job half is
blocked** until the smoke workflow reaches `main` (GitHub only dispatches
`workflow_dispatch` from the default branch), so the GA DoD row stays ⚠️ Unverified
and **Q415 stays open**. Three defects found and filed:
Q463 (since fixed), Q465 (fixed), [Q466](../STATUS.md#Q466).
Full evidence in [Findings](#findings).
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
11. **Tear down, CRs first.** Order is load-bearing and the live run proved it — see
    [the teardown deadlock](#the-teardown-order-is-load-bearing-and-undocumented)
    below. Delete the **CRs while their controllers are still running**, exactly as
    [migration-v1-to-v2.md § Step 4](../operations/migration-v1-to-v2.md) prescribes:

    ```bash
    kubectl -n gag-dogfood-migrate delete actionsgateways.actions-gateway.github.com --all
    kubectl -n gag-dogfood-migrate delete actionsgateways.actions-gateway.com --all
    ```

    Only then delete the namespace, then reclaim the cluster-scoped migration outputs
    (the `ClusterRunnerTemplate` by its `MigratedFromNamespaceLabel` provenance label
    and the matching ClusterRoleBinding — namespace deletion does not reclaim them,
    which is the whole reason the tool stamps the label), then take the system pool
    back to 0.

    Deleting the namespace *first* strands the tenant in `Terminating` forever,
    because the AGCs that clear the `agentpool-cleanup` finalizers live inside the
    namespace being deleted.

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
ignore. Filed as Q463; **not** fixed here, because changing the
tool's behaviour in the middle of the run that validates it would invalidate the run.

**Fixed under Q463 (after this run).** The grant read is now one shared function,
`gmc/internal/webhook/validation.PrivilegedGrantPresent`, called by both the v1 webhook
and the tool, so the two cannot drift again; the warning text names what is actually
missing (a grant on *either* domain) instead of prescribing the v2 label unconditionally.

Working around it is why this plan's tenant grants privileged on the **v1** label
domain: that is both the authentic pre-migration shape and the path that actually
exercises the grant carry-forward.

### Live run on GKE (2026-07-27) — migration validated, job half blocked

Executed against the real dogfood cluster with real GitHub App credentials, in an
isolated `gag-dogfood-migrate` namespace. The always-on `dogfood` CI tenant and the
on-demand `gag-dogfood-e2e` tenant were untouched and verified healthy afterwards;
the cluster was returned to 0 nodes.

**Q415 is NOT closed.** Everything except the job ran and passed. The DoD row stays
⚠️ Unverified until a real DinD job completes on the migrated tenant.

| Acceptance criterion | Result |
|---|---|
| `gag-migrate --apply` completes against the live v1 tenant | ✅ Applied the full object set; no error |
| Migrated AGC + EgressProxy reach Ready | ✅ `dogfood-migrate-agc` 1/1, `dogfood-migrate-egress-proxy` 1/1 |
| v1 objects survive (coexistence) | ✅ Both v1 Deployments still running — but see the coexistence defect below |
| After the Q231 recreate, the set is ScaleSet-native and registers at GitHub | ✅ Annotation flipped `Classic`→`ScaleSet`; `scale-set listener active (scaleSetID 10)` |
| A real DinD job completes green on the migrated tenant | ❌ **Blocked** — see the dispatch constraint below |
| Teardown leaves no orphaned cluster-scoped objects | ✅ After clearing stranded finalizers — see the teardown finding |

**The namespace patch worked exactly as designed**, and this is the part the v1-domain
grant label was chosen to exercise: the platform grant was carried *forward* onto the
v2 domain (`actions-gateway.com/privileged-profile=allowed` added beside the original
v1 label), `security-profile=privileged` was relocated from the gateway to the
namespace, the v2 tenant marker was added, and every v1 marker survived.

`noProxyCIDRs` also carried across onto the emitted `EgressProxy`, which is what let
the migrated AGC start — see the defect immediately below.

### Blocker: the job half cannot run until the workflow is on `main`

`gh workflow run` returns:

> HTTP 404: workflow dogfood-migrate-validate.yml not found on the default branch

GitHub only dispatches a `workflow_dispatch` workflow that exists on the **default
branch**; the `--ref` selects which *version* runs, it does not register the workflow.
So the smoke workflow must be merged to `main` before either the baseline or the
post-migration job can be dispatched.

**Consequence for sequencing:** the live run splits in two. This session validated the
migration itself (independent of the job) as a real-infrastructure rehearsal. The
remaining session runs the full ordered sequence — baseline → migrate → job — once
this PR is merged.

### Defect: the AGC's default `NO_PROXY` only works on kind/kubeadm (Q465)

The most valuable find of the run, and one no existing test could have caught.

A proxied tenant's AGC gets a generated `NO_PROXY` that defaults to
`svc.cluster.local,localhost,127.0.0.1,10.96.0.0/12`
([shared_agc_deployment.go:50](../../cmd/gmc/internal/controller/shared_agc_deployment.go)).
That last entry is the **kind/kubeadm** Service CIDR. This cluster's is
`34.118.224.0/20`, so the AGC dialled the API server *through the egress proxy*, could
not verify the proxy's CA, and `CrashLoopBackOff`ed at startup:

```
detect actions-gateway.com/v2alpha1 RunnerSet CRD: … Get "https://34.118.224.1:443/api":
proxyconnect tcp: tls: failed to verify certificate: x509: certificate signed by unknown authority
```

Adding the real Service CIDR to `noProxyCIDRs` fixed it immediately — root cause
confirmed by measurement, not inferred.

**This is not GKE-specific.** `10.96.0.0/12` covers `10.96.0.0`–`10.111.255.255`, so
the default also misses EKS (`172.20.0.0/16`) and AKS (`10.0.0.0/16`). Every managed
Kubernetes offering breaks; only kind/kubeadm works.

**Why nothing caught it:** the kind e2e runs on a cluster whose Service CIDR *is*
`10.96.0.0/12`, and both dogfood tenants run `Direct` egress, so `NO_PROXY` is never
consulted. It takes a proxied tenant on a managed cluster — precisely what this plan
constructs — to reach the bug. Filed as Q465.

**Fixed (Q465).** The hardcoded range is gone. The GMC now exempts the API server by
the address the cluster reports — its own `KUBERNETES_SERVICE_HOST`, which is the
ClusterIP the AGC pod will dial — falling back to the literal
`$(KUBERNETES_SERVICE_HOST)` for the kubelet to expand when the GMC itself runs
out-of-cluster, and bracketing an IPv6 ClusterIP so Go's `NO_PROXY` parser matches
it. `kubernetes.default.svc` joins the static half for clusters whose cluster domain
is not `cluster.local`. Worker pods lose the range outright and gain nothing: they
hold no kubeconfig and never dial the API server. The net effect is a *narrower*
default on every distribution — one API server address instead of a /12 of
unrelated hosts. Live re-confirmation is outstanding: the workaround
`noProxyCIDRs: [34.118.224.0/20]` has been removed from
[`deploy/dogfood-migrate/resources.yaml`](../../deploy/dogfood-migrate/resources.yaml)
so that a clean AGC startup at the next dogfood sitting is itself the verification.

### Defect: v1 and v2 collide during coexistence ([Q466](../STATUS.md#Q466))

With both control planes live after the migration, the **migrated v2 AGC was clean (0
warnings/errors) while the v1 AGC errored continuously** (14 errors in 3 minutes),
in two distinct ways:

1. **Agent-pool Secret collision.** The v1 `RunnerGroup` and the migrated v2
   `RunnerSet` share a name, so both derive the *same* agent-pool Secret name and both
   controllers try to manage it:
   `agentpool: create agent 1: secrets "agentpool-dogfood-migrate-…-1" already exists`.
   The Secrets carry **no owner references**, so nothing arbitrates — verified
   directly.
2. **v1 AGC lacks RBAC for a v2 kind it watches.** `clusterrunnertemplates… is
   forbidden: User "system:serviceaccount:gag-dogfood-migrate:actions-gateway-controller"
   cannot list…` — the migration grants the *migrated* AGC's ServiceAccount, not the
   v1 one.

This matters because coexistence is load-bearing in the migration story: v1 is left
running specifically so rollback stays possible. In practice the v1 tenant is left in
a broken reconcile loop the moment v2 comes up, which weakens that guarantee. Filed as
[Q466](../STATUS.md#Q466).

### The teardown order is load-bearing and undocumented

Deleting the tenant namespace directly left it in `Terminating` indefinitely on three
finalizers (`actions-gateway.com/agentpool-cleanup`,
`actions-gateway.github.com/agentpool-cleanup`,
`actions-gateway.github.com/gmc-cleanup`). The reason is structural: the AGC
Deployments live *inside* the tenant namespace, so namespace deletion removes the very
controllers that must clear those finalizers.

[migration-v1-to-v2.md § Step 4](../operations/migration-v1-to-v2.md) does prescribe
the correct order — delete the CRs first, while the controllers still run — so this
was a bug in **this plan's runbook**, since corrected in step 11, rather than a
product defect. What is missing is any warning that the obvious alternative deadlocks;
that doc gap is folded into [Q466](../STATUS.md#Q466) rather than filed separately.

Recovery, for the record: all namespace content was already deleted and no
cluster-scoped object was at risk of being orphaned (verified before acting), so the
three finalizers were cleared with a merge patch and the namespace drained
immediately.
