# Q415 — validate `gag-migrate` v1→v2 on the dogfood cluster (plan)

**Status:** ✅ **COMPLETE — validated end to end on the live GKE dogfood cluster 2026-07-28.** Every acceptance criterion passed, including a real GitHub Actions DinD job green on the migrated tenant, on the scale-set path, with a green baseline before the migration for attribution.
The v2 GA Definition of Done row *"≥1 representative tenant migrated v1→v2 with the tool for real"* moves from ⚠️ Unverified to ✅.
All four defects found — Q463, Q465, Q466 and Q467 — have since been **fixed** by parallel sessions (#911, #912, #915, and the Q467 fix).
Full evidence in [Findings](#findings).

> That 2026-07-28 run confirmed none of those fixes — it ran against an `agc` image predating all of them, with `noProxyCIDRs` pinned and a deliberately short gateway name as workarounds.
> **All three are now confirmed live** on a control plane built from `2715e7f8`, with both workarounds removed: [Re-validation (Q472)](#re-validation-on-a-fixed-control-plane-2026-07-31-q472).
> That run found one new defect of its own, [Q535](#defect-a-v2-agc-also-reconciles-the-v1-runnergroups-in-its-namespace-q535), since fixed.
> **Scope:** the last unverified item in the v2 GA Definition of Done — [v2-ga.md § Definition of Done audit](../v2-ga.md#definition-of-done-audit-as-of-this-change) row *"≥1 representative tenant migrated v1→v2 with the tool for real"*, today marked ⚠️ **Unverified**.
> Closing it needs a live migration, not another test.

## Goal (one sentence)

Migrate a real, running `v1alpha1` Docker-in-Docker tenant to v2 on the GKE dogfood cluster with `gag-migrate`, then have the migrated tenant run a real GitHub Actions DinD job — so the GA DoD row can move from ⚠️ Unverified to ✅ on evidence.

## Why the existing e2e is not enough

[Q414](../../../cmd/gmc/test/e2e/migration_v1_to_v2_test.go) already covers the tool on a live `kind` cluster, and covers it well: dry-run fan-out, the `ClusterRunnerTemplate`-vs-`RunnerTemplate` admission split, the namespace patch, v1/v2 coexistence, and the migrated control plane reconciling to Ready.
That spec says so itself — it names this plan as the follow-up.

Three things it deliberately does not do, each of which is where a real migration can still fail:

| Not covered by Q414 | Why it matters |
|---|---|
| **No worker pod is ever provisioned** — the spec enqueues no job ("The spec is about the fan-out, the admission verdict, and the reconcile — not about running a build"). | A migrated tenant that reconciles but cannot *run* a job satisfies every assertion in the spec and still fails the DoD. |
| **fakegithub, not GitHub** — no real App installation, no real runner registration, no real scale-set. | Migration rewrites the credential and gateway identity. Whether the migrated gateway re-registers against the real GitHub installation is untested. |
| **kind, not GKE** — no `ResourceQuota`, no per-tenant egress proxy with a real egress IP, no node pools, no taints. | The v2 fan-out extracts the inline proxy into a standalone `EgressProxy`. On kind that is a Deployment becoming Ready; on dogfood it is a real egress path to GitHub. |

## What is being validated, precisely

1. `gag-migrate` dry-run then `--apply` against a **live v1 tenant with real credentials**, on the cluster GAG actually dogfoods on.
2. The migrated tenant reconciles into a working per-gateway AGC **and** a working `EgressProxy` pool.
3. The migrated tenant **acquires and completes a real GitHub Actions job** whose worker is a privileged DinD pod — proving the migration preserved the capability, not just the object graph.
4. The **Q231 recreate** is exercised and documented: `gag-migrate` emits `v2alpha1`, so the conversion webhook stamps `conversion.actions-gateway.com/acquisition-protocol: Classic` on the stored `v2beta1` object, and the migrated set keeps running Classic until the `RunnerSet` is recreated v2beta1-native.
   This is the one migration step an operator must be told about explicitly, and it is exactly the step this run confirms.

**The runner label is deliberately unchanged across the migration** (`gag-migrate-v1`). v1 Classic permits many labels and v2beta1 ScaleSet permits exactly one, so choosing a single-label v1 tenant means the workflow's `runs-on` survives the migration untouched — which is both the friendlier operator story and a cleaner assertion (same workflow, same label, before and after).

## Assets built by this change

Neither is a permanent fixture.
Both describe a path that **ceases to exist at `v2.0.0`**, when `v1alpha1` is removed ([v2-ga.md](../v2-ga.md) Phase 3), so they are deliberately small and are deleted with the rest of the v1 surface at that cut rather than maintained forever.

| Asset | Purpose |
|---|---|
| [`deploy/dogfood-migrate/`](../../../deploy/dogfood-migrate/README.md) | The `v1alpha1` DinD tenant, mirroring the `utils.DinDTenant` fixture shape the Queue row names, sized for a smoke job rather than the full e2e suite. |
| [`.github/workflows/dogfood-migrate-validate.yml`](../../../.github/workflows/dogfood-migrate-validate.yml) | A `workflow_dispatch`-only DinD smoke job targeting the tenant's runner label. Never runs on push or PR. |

### Why a smoke job, not the full e2e suite

The DoD asks for a representative tenant migrated for real, not for a second copy of the e2e gate.
A `docker build` + `docker run` inside the migrated tenant's privileged DinD worker proves every link in the chain — job acquisition, worker provisioning, the native-sidecar dockerd, egress to Docker Hub through the migrated proxy, and completion reporting — at a fraction of the cost and wall-clock of the kind-in-DinD suite.
Running the full suite would validate the *e2e tenant*, which is already validated, rather than the *migration*.

### Why no reusable script

The repo's convention is to script live dogfood operations under `scripts/dogfood/` with stub tests in `make check`.
This run is deliberately an exception: it is a one-time DoD checkbox on a code path that is deleted at `v2.0.0`, so a committed orchestration script plus its stub test would be dead code within one release.
The runbook below is executed step by step instead, and the findings recorded here.

## Runbook

Every step pins `--project`/`--zone`/`--context` explicitly rather than relying on ambient context, per [kind-iteration.md § Target the cluster explicitly](../../development/kind-iteration.md).
The dogfood cluster is hard-classified prod in `.claude/prod-guard.json`, so each mutating command needs `PROD_GUARD_OVERRIDE` naming this plan.

```bash
export PROJECT=actions-gateway-dogfood CLUSTER=gag-dogfood ZONE=us-east1-b
export REPO=actions-gateway/github-actions-gateway
```

1. **Bring the cluster up.** `scripts/dogfood/start.sh` (system pool from 0 to the derived running size).
   The `e2e` node pool autoscales 0→N on job arrival on its own.
2. **Create the tenant namespace and its App credential Secret** (out of band — a credential, never in git), then apply the v1 tenant: `kubectl apply -k deploy/dogfood-migrate`.
3. **Wait for the v1 AGC to be Ready.** Migrating a tenant that never came up would prove nothing — establish a live v1 tenant first, exactly as the Q414 spec does.
4. **Baseline (optional but cheap):** dispatch the smoke workflow against the v1 tenant and confirm green *before* migrating.
   Without this, a post-migration failure cannot be attributed to the migration.
5. **Dry run:** `gag-migrate --namespace gag-dogfood-migrate --context <ctx>`.
   Review the fan-out and both warnings (cluster-scoped object; namespace patch needs the downgrade opt-in).
6. **Grant the downgrade opt-in** the dry run asks for (`allow-profile-downgrade=allowed` on the namespace).
   The tool deliberately does not write this — opting into a downgrade is the operator's call.
7. **Apply:** `gag-migrate … --apply`.
   Verify the emitted object set was admitted and that the v1 objects survive alongside it.
8. **Wait for the migrated control plane** — the `<gateway>-agc` Deployment and the `<gateway>-egress-proxy` pool.
9. **Recreate the `RunnerSet` (Q231)** so it comes back v2beta1-native on ScaleSet rather than inheriting `Classic` from the conversion annotation.
   Confirm the annotation is gone and the set registers a scale set at GitHub.
10. **Dispatch the smoke workflow again** — same workflow, same `runs-on` label — and confirm it goes green on the migrated tenant.
11. **Tear down, CRs first.** Order is load-bearing and the live run proved it — see [the teardown deadlock](#the-teardown-order-is-load-bearing-and-undocumented) below.
    Delete the **CRs while their controllers are still running**, exactly as [migration-v1-to-v2.md § Step 4](../../operations/migration-v1-to-v2.md) prescribes:

    **RunnerSets before gateways** — the v2 `RunnerSet` is not gateway-owned, so deleting the gateway first cascades the AGC away and strands the set's finalizer:

    ```bash
    kubectl -n gag-dogfood-migrate delete runnersets.actions-gateway.com --all
    kubectl -n gag-dogfood-migrate delete actionsgateways.actions-gateway.com --all
    kubectl -n gag-dogfood-migrate delete actionsgateways.actions-gateway.github.com --all
    ```

    Only then delete the namespace, then reclaim the cluster-scoped migration outputs (the `ClusterRunnerTemplate` by its `MigratedFromNamespaceLabel` provenance label and the matching ClusterRoleBinding — namespace deletion does not reclaim them, which is the whole reason the tool stamps the label), then take the system pool back to 0.

    Both orderings were learned the hard way here — deleting the namespace first, and deleting the gateway before its RunnerSet, each deadlock in their own way.
    See [the teardown findings](#the-teardown-order-is-load-bearing-and-undocumented).

## Acceptance criteria

The run closes Q415 only if all of these hold, each recorded with its evidence in Findings below.
**All met 2026-07-28.**

- [x] `gag-migrate --apply` completes without error against the live v1 tenant.
- [x] The migrated `<gateway>-agc` and `<gateway>-egress-proxy` Deployments reach Ready.
- [x] The v1 objects survive the migration (coexistence, so rollback stays possible).
- [x] After the Q231 recreate, the `RunnerSet` is ScaleSet-native (no `acquisition-protocol: Classic` annotation) and registers at GitHub.
- [x] A real GitHub Actions DinD job completes **green** on the migrated tenant — confirmed on the scale-set path from the AGC's own provisioning log, with a green baseline beforehand for attribution.
- [x] Teardown leaves no orphaned cluster-scoped objects.

Anything that fails is a finding to fix, not a reason to soften the criterion — this is the run that decides whether the DoD row is honest.

## Cost and blast radius

- **Billable:** the system pool for the run window plus one `c2-standard-8` spot node in the `e2e` pool while the smoke job runs.
  Both return to zero on teardown.
- **Blast radius:** a new, isolated namespace (`gag-dogfood-migrate`).
  The always-on `dogfood`/`dogfoodss` CI tenants and the on-demand `gag-dogfood-e2e` tenant are untouched, and no repo variable that routes real CI is changed — the smoke workflow is `workflow_dispatch`-only and targets its own label.
- **Not reversible by itself:** the namespace metadata patch and the cluster-scoped `ClusterRunnerTemplate`.
  Both are reclaimed by step 11.

## Findings

### Rehearsal on `kind` (2026-07-27) — assets validated, one tool defect found

Before spending anything on GKE, the tenant manifest was applied to the local `kind` e2e cluster and `gag-migrate` was run against it in dry-run.
This validated the asset, not the migration — the live run below is still outstanding.

**Validated.** All four objects pass the real v1 CRD schema and the GMC validating webhook, including `securityProfile: privileged`.
The dry-run fans the tenant out to all four expected v2 kinds (`ActionsGateway`, `EgressProxy`, `ClusterRunnerTemplate`, `RunnerSet`), puts the privileged worker shape on the **cluster-scoped** kind, and emits the two expected warnings (cluster-scoped object; namespace patch needs the downgrade opt-in).

**Two manifest bugs the rehearsal caught**, both of which would have failed only after the cluster was already up:

1. `v1alpha1` spells the credential fields `gitHubAppRef` / `gitHubURL` — capital H — unlike v2's `githubURL`.
   Both are required, so the wrong casing is a hard reject.
2. The privileged grant was initially written on the **v2** label domain (copied from the v2 e2e overlay).
   It must be the v1 domain for an authentic pre-migration tenant — see the defect below for why that distinction is load-bearing.

### Defect found: `gag-migrate`'s grant detection does not dual-read

The v1 validating webhook **dual-reads** the privileged-eligibility grant across both label domains for the v1/v2 coexistence window — deliberately, per §H.12, so that relabelling a namespace onto the v2 domain does not strand a still-running v1 gateway ([actionsgateway_webhook.go:279](../../../cmd/gmc/internal/webhook/v1alpha1/actionsgateway_webhook.go)).

`gag-migrate` does not.
Its carry-forward check reads only the **v1** domain ([migrate.go:387](../../../cmd/gmc/internal/migrate/migrate.go)), while the warning it emits on a miss names the **v2** label:

> namespace … migrates to securityProfile=privileged but holds no `actions-gateway.com/privileged-profile=allowed` grant; a platform administrator must apply the v2 eligibility label or the profile will be rejected

So a namespace granted eligibility via the v2 label alone — legal, and admitted by the webhook — is reported as ungranted, and the remedy the warning prescribes is the label that is *already there*.
Re-running after following the advice produces the identical warning.
Observed directly: the first rehearsal namespace carried `actions-gateway.com/privileged-profile=allowed`, was admitted privileged by the webhook, and was still warned about.

**Impact is low but the shape is bad.** The end state stays correct — the v2 label is present either way, so the skipped carry-forward is a no-op — so this is an unactionable, self-contradicting warning rather than a broken migration.
It is exactly the rough edge Q273 set out to polish, and it surfaces during the one operation where an operator is least able to judge whether a warning is safe to ignore.
Filed as Q463; **not** fixed here, because changing the tool's behaviour in the middle of the run that validates it would invalidate the run.

**Fixed under Q463 (after this run).** The grant read is now one shared function, `gmc/internal/webhook/validation.PrivilegedGrantPresent`, called by both the v1 webhook and the tool, so the two cannot drift again; the warning text names what is actually missing (a grant on *either* domain) instead of prescribing the v2 label unconditionally.

Working around it is why this plan's tenant grants privileged on the **v1** label domain: that is both the authentic pre-migration shape and the path that actually exercises the grant carry-forward.

### Live run on GKE (2026-07-27) — migration validated, job half blocked

Executed against the real dogfood cluster with real GitHub App credentials, in an isolated `gag-dogfood-migrate` namespace.
The always-on `dogfood` CI tenant and the on-demand `gag-dogfood-e2e` tenant were untouched and verified healthy afterwards; the cluster was returned to 0 nodes.

**Q415 is NOT closed.** Everything except the job ran and passed.
The DoD row stays ⚠️ Unverified until a real DinD job completes on the migrated tenant.

| Acceptance criterion | Result |
|---|---|
| `gag-migrate --apply` completes against the live v1 tenant | ✅ Applied the full object set; no error |
| Migrated AGC + EgressProxy reach Ready | ✅ `dogfood-migrate-agc` 1/1, `dogfood-migrate-egress-proxy` 1/1 |
| v1 objects survive (coexistence) | ✅ Both v1 Deployments still running — but see the coexistence defect below |
| After the Q231 recreate, the set is ScaleSet-native and registers at GitHub | ✅ Annotation flipped `Classic`→`ScaleSet`; `scale-set listener active (scaleSetID 10)` |
| A real DinD job completes green on the migrated tenant | ❌ **Blocked** — see the dispatch constraint below |
| Teardown leaves no orphaned cluster-scoped objects | ✅ After clearing stranded finalizers — see the teardown finding |

**The namespace patch worked exactly as designed**, and this is the part the v1-domain grant label was chosen to exercise: the platform grant was carried *forward* onto the v2 domain (`actions-gateway.com/privileged-profile=allowed` added beside the original v1 label), `security-profile=privileged` was relocated from the gateway to the namespace, the v2 tenant marker was added, and every v1 marker survived.

`noProxyCIDRs` also carried across onto the emitted `EgressProxy`, which is what let the migrated AGC start — see the defect immediately below.

### Part 2 — the job half, run after the workflow reached `main` (2026-07-28)

With the smoke workflow on the default branch the dispatch blocker cleared and the full ordered sequence ran.
**All acceptance criteria pass.**

| Criterion | Result |
|---|---|
| Baseline DinD job green *before* migrating | ✅ [run 30324549361](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30324549361), ~200 s |
| `gag-migrate --apply` on the live v1 tenant | ✅ two expected warnings, four v2 kinds emitted |
| Migrated AGC + EgressProxy reach Ready | ✅ `dfmigrate-agc`, `dfmigrate-egress-proxy` |
| v1 survives `--apply` (coexistence) | ✅ |
| Q231 recreate → ScaleSet-native | ✅ annotation `Classic` → `ScaleSet` |
| **Real DinD job green on the migrated tenant** | ✅ [run 30326208300](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30326208300), ~220 s |
| Teardown leaves nothing orphaned | ✅ after clearing a stranded finalizer — second trap, below |

**The job is confirmed to have run on the migrated tenant's scale-set path**, not inferred from the green tick.
The migrated AGC logged it at `provisioner.go:646`:

```
"scale-set worker pod created" owner=dfmigrate-gag-migrate-v1-18c32e1
  podName=runner-dfmigrate-gag-migrate-v1-18c32e1-8b9a78b-9e56291d-225d-5
  jobID=9e56291d-225d-52b2-9e97-219aa94380ce
```

The worker pod was observed `Pending` → `2/2 Running` → `Completed`, i.e. the runner container plus the privileged DinD native sidecar, which is the shape the whole exercise is about.

**Method note — v1 was decommissioned before the post-migration dispatch.** Both tenants advertise the same `gag-migrate-v1` label, so with both live GitHub could route the job to either and a green result would not have proven the *migrated* tenant ran it.
Coexistence was already verified independently at the `--apply` step, so v1 was removed first — which is [migration-v1-to-v2.md § Step 4](../../operations/migration-v1-to-v2.md) anyway — making the final job unambiguous.
Deleting the v1 gateway cascaded its AGC and proxy away cleanly, confirming that guide's "nothing is stranded" claim for v1.

### Blocker (part 1, since cleared): the job half could not run until the workflow was on `main`

`gh workflow run` returns:

> HTTP 404: workflow dogfood-migrate-validate.yml not found on the default branch

GitHub only dispatches a `workflow_dispatch` workflow that exists on the **default branch**; the `--ref` selects which *version* runs, it does not register the workflow.
So the smoke workflow must be merged to `main` before either the baseline or the post-migration job can be dispatched.

**Consequence for sequencing:** the live run splits in two.
This session validated the migration itself (independent of the job) as a real-infrastructure rehearsal.
The remaining session runs the full ordered sequence — baseline → migrate → job — once this PR is merged.

### Defect: the AGC's default `NO_PROXY` only works on kind/kubeadm (Q465)

The most valuable find of the run, and one no existing test could have caught.

A proxied tenant's AGC gets a generated `NO_PROXY` that defaults to `svc.cluster.local,localhost,127.0.0.1,10.96.0.0/12` ([shared_agc_deployment.go:50](../../../cmd/gmc/internal/controller/shared_agc_deployment.go)).
That last entry is the **kind/kubeadm** Service CIDR.
This cluster's is `34.118.224.0/20`, so the AGC dialled the API server *through the egress proxy*, could not verify the proxy's CA, and `CrashLoopBackOff`ed at startup:

```
detect actions-gateway.com/v2alpha1 RunnerSet CRD: … Get "https://34.118.224.1:443/api":
proxyconnect tcp: tls: failed to verify certificate: x509: certificate signed by unknown authority
```

Adding the real Service CIDR to `noProxyCIDRs` fixed it immediately — root cause confirmed by measurement, not inferred.

**This is not GKE-specific.** `10.96.0.0/12` covers `10.96.0.0`–`10.111.255.255`, so the default also misses EKS (`172.20.0.0/16`) and AKS (`10.0.0.0/16`).
Every managed Kubernetes offering breaks; only kind/kubeadm works.

**Why nothing caught it:** the kind e2e runs on a cluster whose Service CIDR *is* `10.96.0.0/12`, and both dogfood tenants run `Direct` egress, so `NO_PROXY` is never consulted.
It takes a proxied tenant on a managed cluster — precisely what this plan constructs — to reach the bug.
Filed as Q465.

**Fixed (Q465).** The hardcoded range is gone.
The GMC now exempts the API server by the address the cluster reports — its own `KUBERNETES_SERVICE_HOST`, which is the ClusterIP the AGC pod will dial — falling back to the literal `$(KUBERNETES_SERVICE_HOST)` for the kubelet to expand when the GMC itself runs out-of-cluster, and bracketing an IPv6 ClusterIP so Go's `NO_PROXY` parser matches it.
`kubernetes.default.svc` joins the static half for clusters whose cluster domain is not `cluster.local`.
Worker pods lose the range outright and gain nothing: they hold no kubeconfig and never dial the API server.
The net effect is a *narrower* default on every distribution — one API server address instead of a /12 of unrelated hosts.
The workaround `noProxyCIDRs: [34.118.224.0/20]` was removed from [`deploy/dogfood-migrate/resources.yaml`](../../../deploy/dogfood-migrate/resources.yaml) so that a clean AGC startup would itself be the verification — [confirmed live 2026-07-31](#re-validation-on-a-fixed-control-plane-2026-07-31-q472).

### Defect: v1 and v2 collide during coexistence (Q466)

With both control planes live after the migration, the **migrated v2 AGC was clean (0 warnings/errors) while the v1 AGC errored continuously** (14 errors in 3 minutes), in two distinct ways:

1. **Agent-pool Secret collision.** The v1 `RunnerGroup` and the migrated v2 `RunnerSet` share a name, so both derive the *same* agent-pool Secret name and both controllers try to manage it: `agentpool: create agent 1: secrets "agentpool-dogfood-migrate-…-1" already exists`.
   The Secrets carry **no owner references**, so nothing arbitrates — verified directly.
2. **v1 AGC lacks RBAC for a v2 kind it watches.** `clusterrunnertemplates… is forbidden: User "system:serviceaccount:gag-dogfood-migrate:actions-gateway-controller" cannot list…` — the migration grants the *migrated* AGC's ServiceAccount, not the v1 one.

This matters because coexistence is load-bearing in the migration story: v1 is left running specifically so rollback stays possible.
In practice the v1 tenant is left in a broken reconcile loop the moment v2 comes up, which weakens that guarantee.
Filed as Q466.

**Fixed.** The agent pool now derives its Secret name, selector label, and GitHub runner name from the owner's *kind* as well as its name, so the two pools are disjoint; each agent Secret carries an owner reference (back-filled onto existing ones); and an existing v2 install's Secrets are moved onto the new names on first reconcile rather than orphaned.
Splitting the Kubernetes name alone would not have been enough — GitHub runner names are unique per registration scope, so the two pools would have gone on deregistering each other's live records through the 409-conflict path.

The second symptom had a different root cause than the row's framing implies, found while fixing it: the v1 singleton AGC reconciles **every** `RunnerSet` (its `GATEWAY_NAME` is empty, which disables gateway scoping), so it was resolving the migrated set's `templateRef` and reaching for a cluster-scoped kind it is not bound to.
The fix is to stop the read rather than widen the grant — a `RunnerSet` belongs to the AGC of the gateway its `gatewayRef` names, so an AGC with no `GATEWAY_NAME` no longer registers the reconciler at all.
That also closes an exposure the RBAC error had been masking: with the grant added and the scoping left alone, the v1 AGC would have run a second listener pool and a second set of GitHub registrations for a set the migrated gateway's AGC was already serving.
Convention written up in [kubernetes-conventions.md § Derive a per-owner name from the owner's kind](../../development/kubernetes-conventions.md#derive-a-per-owner-name-from-the-owners-kind-not-just-its-name-q466).

[Confirmed live 2026-07-31](#re-validation-on-a-fixed-control-plane-2026-07-31-q472): both symptoms absent, the two pools' Secrets disjoint and owner-referenced.
That run also found the symmetric case this fix left open — [Q535](#defect-a-v2-agc-also-reconciles-the-v1-runnergroups-in-its-namespace-q535), fixed by the same shape of gate.

### Defect: a truncated worker pod name can be invalid, and then no job ever runs (Q467)

The most serious find of the exercise, and the one that made the first baseline job fail.
The provisioner derives the worker pod name and truncates it to the 63-char DNS label limit **without trimming a trailing hyphen** ([provisioner.go:391](../../../cmd/agc/internal/provisioner/provisioner.go)):

```go
podName := fmt.Sprintf("runner-%s-%s", safeName(key.Name), safePlanID)
if len(podName) > 63 {
    podName = podName[:63]   // may land on '-'
}
```

The plan ID is a UUID, whose hyphens sit at indices 8, 13, 18 and 23.
If the cut lands on one, the apiserver rejects **every** worker pod:

```
provisioner: create Pod runner-dogfood-migrate-gag-migrate-v1-18c32e1-d4766d0-a20852f8-:
  metadata.name: Invalid value: "…-": a lowercase RFC 1123 subdomain must … end with
  an alphanumeric character
```

Three things make this worse than a naming nit:

1. **Deterministic, not flaky.** Whether a tenant can run jobs at all is fixed by the character lengths of its gateway name and runner label.
   The original tenant landed exactly on index 8 and could never have run a single job.
2. **Both tiers.** `scaleSetPodName` ([provisioner.go:718](../../../cmd/agc/internal/provisioner/provisioner.go)) does the identical naive truncation, so this does **not** disappear when classic is removed at `v2.0.0`.
3. **The symptom points the wrong way.** No worker pod is ever created, and GitHub reports *"The self-hosted runner lost communication with the server… verify the machine is running and has a healthy network connection."* An operator debugs networking, not name validation.

**Proof by controlled change.** The only variable altered was the gateway name — `dogfood-migrate` (cut at index 8, a hyphen) → `dfmigrate` (cut at index 14, a hex digit).
Same tenant, same workflow, same label: the job went from *no pod, runner lost communication* to green, with a valid 63-char pod name ending in `4`.

Truncation is effectively unavoidable — `runner-` + key + `-` + a 36-char UUID exceeds 63 for any realistic tenant name — so every tenant is rolling against the hyphen positions.
The kind e2e tenants and the dogfood CI tenant happen to land safely, which is why nothing caught it.
Filed as Q467; the fix belongs in `provisioner.go` in its own PR, and cannot be validated here anyway because dogfood runs the released `agc` image, not a branch build.

The tenant manifest pins a deliberately short gateway name as the workaround, with the arithmetic inline.

**Fixed.** The provisioner now splits the 63-char budget across the name's segments before joining them and replaces each truncated tail with a hash of that whole segment, so a derived name is always a valid DNS label *and* still unique per job — trimming the trailing hyphen alone would have traded a rejected pod for a colliding one.
Both tiers share the one derivation.
An envtest at the boundary length pins it against a real API server, and a `WorkerPodCreateFailed` Warning Event now carries the API server's message to the owner object, so the next rejection of any kind is diagnosable from `kubectl describe` instead of a live debugging session ([convention](../../development/kubernetes-conventions.md#derive-every-name-through-apiapinames-q467-q473) · [runbook](../../operations/troubleshooting.md#runner-lost-communication-and-no-worker-pod-was-ever-created)).
The dogfood tenant's natural name was restored once the cluster ran a build carrying the fix, and both tiers then produced valid 63-char names — [confirmed live 2026-07-31](#re-validation-on-a-fixed-control-plane-2026-07-31-q472).

### Re-validation on a fixed control plane (2026-07-31, Q472)

The run the caveat above asks for.
Dogfood now pins `2715e7f8` for both GMC and AGC, which carries all three fixes, so each is live-verifiable for the first time.
The tenant ran with `noProxyCIDRs` unset **and** its natural gateway name `dogfood-migrate` restored — both workarounds removed, so a clean run is itself the evidence.

| Fix | Verdict | Evidence |
|---|---|---|
| Q465 — `NO_PROXY` default | ✅ confirmed | Generated `NO_PROXY` is `kubernetes.default.svc,svc.cluster.local,localhost,127.0.0.1,34.118.224.1`. The hardcoded `10.96.0.0/12` is gone; `34.118.224.1` equals this cluster's live `kubernetes` Service ClusterIP. `HTTPS_PROXY` is set, so this is the proxied path that used to crashloop — the AGC reached Ready in 37 s with 0 restarts. |
| Q466 — pool disjointness | ✅ confirmed | Both symptoms absent across the coexistence window: 0 × `already exists`, 0 × `is forbidden` on either AGC. Agent Secrets are kind-disjoint and owner-referenced: `agentpool-…-{0,1}` owned by `RunnerGroup`, `agentpool-rs-…-{0,1}` owned by `RunnerSet`. Pre-fix these collided and carried no owner reference. |
| Q467 — worker pod name | ✅ confirmed, both tiers | Classic: `runner-dogfood-migrate-gag-3994471-78a9ae58-cfa8-44a4-a-4dcd6f9`. ScaleSet: `runner-dogfood-migrate-gag-3994471-1085497c-3aa7-5385-b-e51b992`. Both exactly 63 chars, both ending alphanumeric, each truncated segment ending in a hash rather than a bare cut. |

Both smoke jobs went green with the natural name — baseline [run 30647304201](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30647304201) on the v1 classic path (runner `dogfood-migrate-gag-migrate-v1-18c32e1-0`) and [run 30647976704](https://github.com/actions-gateway/github-actions-gateway/actions/runs/30647976704) after the migration on the scale-set path (runner `gag-migrate-v1-1085497c-…`, provisioned via `scaleSetPodName`).
The Q231 recreate flipped the stored annotation `Classic` → `ScaleSet` as before.
Teardown followed the documented order and deadlocked nowhere: RunnerSets, gateway, namespace and the cluster-scoped `ClusterRunnerTemplate` all cleared without a stranded finalizer, leaving no orphan.
Both workarounds are now removed from [`resources.yaml`](../../../deploy/dogfood-migrate/resources.yaml) permanently.

**The workarounds were load-bearing, and removing them proved it.** The natural name is the case Q467 originally failed on: the old derivation cut `runner-dogfood-migrate-gag-migrate-v1-18c32e1-<planID>` at index 63, landing on the UUID's hyphen at index 8, and every worker pod was rejected.

### Defect: a v2 AGC also reconciles the v1 RunnerGroups in its namespace (Q535)

Found by this run, in the window Q466 was being re-checked.
It is **not** a Q466 regression — the registration below is unconditional at `#915`'s parent too — but it is the mirror image of the case Q466 closed, and Q466's fix did not close it.

The v1 `RunnerGroupReconciler` is registered unconditionally on every AGC ([main.go:433](../../../cmd/agc/main.go)), and gateway scoping is a field selector on `RunnerSet` alone ([main.go:294](../../../cmd/agc/main.go)) — nothing scopes the RunnerGroup informer.
So during coexistence the migrated per-gateway AGC serves the v1 tenant's `RunnerGroup` *as well as* the v1 AGC that owns it.
Measured, both live at once:

- **Duplicate listener pools on one group.** The v2 AGC started its own listener at `agentIndex: 0` for `dogfood-migrate-gag-migrate-v1-18c32e1` — the index the v1 AGC was already running.
  Same index means the same agent Secret and the same GitHub runner name, which is exactly the hazard the Q466 fix text describes ("two controllers on one object, which no amount of naming can separate") in the direction it did not fix.
- **A hot retry loop against GitHub.** The duplicate listener took `409` from the broker on every `CreateSession` — **153 in ~2.5 minutes**, roughly 1/s, listener index climbing monotonically, no backoff.
- **Continuous status contention.** Both AGCs' RunnerGroup reconcilers wrote the same object: ~35 `the object has been modified` errors per minute *each*, not settling.

**Proof by controlled change.** Deleting the v1 gateway — the only variable altered — removed the v1 `RunnerGroup` and the 409s stopped at that instant (last one 16:37:29), leaving the v2 AGC quiet.

Why the first run missed it: the v1 AGC's own Secret-collision and RBAC errors dominated that window, and the v2 AGC was checked for *its* kind rather than for the v1 kind it had also picked up.

**Fixed.** The AGC registers the v1 `RunnerGroup` reconciler only when `GATEWAY_NAME` is empty — the exact mirror of Q466's gate, and, as predicted, a decline rather than a widening.
Since a `RunnerGroup` names no gateway, "the RunnerGroups that gateway does not own" is all of them, so there is no informer to scope: a gateway-scoped AGC simply does not serve the kind.
Net effect, now stated in [05-security.md](../../design/05-security.md): **each AGC process serves exactly one API**.

Two consequences fell out of the change rather than being sought:

- **No AGC serves both APIs any more.** The two gates are complementary, so the process shape the Q466 coexistence suite modelled — one manager running both reconcilers — no longer exists.
  That suite still earns its keep (its assertions are about the *objects*: disjoint Secret names, labels, runner names and owner refs keep the pools separable however they are deployed) and now runs a strictly harsher configuration than production does.
- **A gateway-scoped AGC on a v1-only install now has no reconciler at all**, so it fails fast at startup instead of passing its probes and reconciling nothing.
  Reachable only by uninstalling the v2 CRD chart under live v2 gateways; [troubleshooting.md](../../operations/troubleshooting.md#agc-exits-at-startup-gateway_name-set-but-the-v2-runnerset-crd-is-missing) covers it.

Regression cover is `TestQ535_GatewayScopedAGCDeclinesV1RunnerGroups` (`cmd/agc/internal/controller/integration/`), which runs the two AGCs a mid-migration namespace actually has and asserts the v1 group keeps exactly one listener, still on the session the v1 AGC opened.
It shares `controller.ServesRunnerGroups` with `main.go`, and was confirmed to fail on that assertion with the gate reverted.

**One finding the test surfaced about the fake.** A broker session's `ownerName` is `<CR name>-<agentIndex>` for *both* kinds — the kind disambiguation Q466 added to agent Secret names and GitHub runner names never reached it.
That is cosmetic in the protocol (GitHub keys session conflicts on the agent, not the owner string), but it means `brokertest.Server.ActiveSessionsForOwner` cannot separate a same-named RunnerGroup and RunnerSet, and its doc comment still claims the owner identifies a RunnerGroup.
Filed as Q538; the test works around it by naming its RunnerSet distinctly.

Q538 later measured the production client and refuted the "cosmetic" reading above: the listener sends that same string as `agent.name`, so for a RunnerSet it names no runner GitHub has registered.
The fake's own name scoping and doc were fixed there; the wire-name divergence carried to [Q677](../../STATUS.md#Q677).

### The teardown order is load-bearing and undocumented

Deleting the tenant namespace directly left it in `Terminating` indefinitely on three finalizers (`actions-gateway.com/agentpool-cleanup`, `actions-gateway.github.com/agentpool-cleanup`, `actions-gateway.github.com/gmc-cleanup`).
The reason is structural: the AGC Deployments live *inside* the tenant namespace, so namespace deletion removes the very controllers that must clear those finalizers.

[migration-v1-to-v2.md § Step 4](../../operations/migration-v1-to-v2.md) does prescribe the correct order — delete the CRs first, while the controllers still run — so this was a bug in **this plan's runbook**, since corrected in step 11, rather than a product defect.
What is missing is any warning that the obvious alternative deadlocks; that doc gap is folded into Q466 rather than filed separately.

**Fixed.** [migration-v1-to-v2.md § Teardown order is load-bearing](../../operations/migration-v1-to-v2.md#teardown-order-is-load-bearing-never-delete-the-namespace-first) now states the order, names the three finalizers, explains why the deadlock is structural rather than a slow reconcile, and gives the recovery — including what the recovery skips (the GitHub-side deregistration).
A [troubleshooting entry](../../operations/troubleshooting.md#tenant-namespace-stuck-terminating-on-agentpool-cleanup-finalizers) points there from the symptom.
The rollback snippet in Step 3 was also reordered: it deleted the v2 `ActionsGateway` before its `RunnerSet`s, which cascades away the AGC whose finalizer they wait on — the same deadlock, in the doc that prescribes it.

Recovery, for the record: all namespace content was already deleted and no cluster-scoped object was at risk of being orphaned (verified before acting), so the three finalizers were cleared with a merge patch and the namespace drained immediately.

**A second, different ordering trap on the v2 side (part 2).** Following the corrected "CRs first" rule was still not enough, because *which* CR comes first matters:

```bash
kubectl delete actionsgateways.actions-gateway.com --all   # cascades away the AGC…
kubectl delete runnersets.actions-gateway.com --all        # …now this hangs forever
```

Deleting the `ActionsGateway` cascades its AGC Deployment away, but a **v2 `RunnerSet` is not owned by the gateway** — [backup-restore.md](../../operations/backup-restore.md) says so explicitly ("they are never deleted by gateway teardown").
So the RunnerSet survives the cascade and is then left holding `actions-gateway.com/agentpool-cleanup` with no controller alive to clear it.
Observed: the set sat `Terminating` from 03:40:22 with `dfmigrate-agc` already gone.

This is not the same bug as the namespace deadlock, and the existing docs do not cover it.
[migration-v1-to-v2.md § Step 4](../../operations/migration-v1-to-v2.md) says `delete actionsgateways --all` and states children cascade — **true for v1**, where `RunnerGroup`s *are* gateway-owned (confirmed here: deleting the v1 gateway removed its AGC, proxy and RunnerGroup cleanly), and **false for v2**.

**The correct v2 order is RunnerSets first, then the ActionsGateway, then the namespace, then the cluster-scoped outputs.** Both deadlocks are now documented for operators — the Q466 fix (#915) added [Teardown order is load-bearing: never delete the namespace first](../../operations/migration-v1-to-v2.md#teardown-order-is-load-bearing-never-delete-the-namespace-first) to the migration guide, covering the namespace-first case *and* the gateway-before-RunnerSet case this run found.
Nothing further outstanding here.
