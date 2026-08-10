# GAG dogfood migrate — the `v1alpha1` tenant Q415 migrates for real

A single, throwaway `actions-gateway.github.com/v1alpha1` Docker-in-Docker tenant, used once to validate `gag-migrate` v1→v2 on the live dogfood cluster.
It exists to close the last unverified row in the v2 GA Definition of Done — see [docs/plan/q415-migrate-dogfood-validation.md](../../docs/plan/archive/q415-migrate-dogfood-validation.md) for the goal, the runbook, and the acceptance criteria.

**This is a dogfood/dev config, not a shipped product install, and not a template to copy.** New tenants onboard on v2 ([getting-started](../../docs/getting-started.md)); `v1alpha1` is deprecated and removed at `v2.0.0`.
This tree is deleted at that cut along with the rest of the v1 surface.

## Why it is the legacy shape

The whole point is to have something to migrate.
It is deliberately the monolithic v1 object — one `ActionsGateway` carrying its runner group, its inline proxy, and its `securityProfile` — mirroring the `utils.DinDTenant` fixture that the [Q414 e2e](../../cmd/gmc/test/e2e/migration_v1_to_v2_test.go) migrates on `kind`, so the live run exercises the same fan-out that spec covers.

The privileged DinD worker shape is what makes it representative: it is the case that distinguishes the namespaced `RunnerTemplate` from the cluster-scoped `ClusterRunnerTemplate` on the v2 side, and it is the shape that already hid one real defect (a privileged tenant fanned out to a namespaced template the v2 webhook rejects, leaving the namespace half-migrated).

## How it differs from `deploy/dogfood-e2e/`

| | `dogfood-e2e/` | `dogfood-migrate/` (this tree) |
|---|---|---|
| API version | `actions-gateway.com/v2beta1` (front-door shape) | `actions-gateway.github.com/v1alpha1` (legacy, on purpose) |
| Lifetime | living CI tenant | throwaway; torn down at the end of the run |
| Worker sizing | measured from a real kind-in-DinD e2e run (Q248) | sized for a **smoke** job — roughly a third as large |
| Runner labels | one (`gag-ci-e2e`) because v2beta1 is ScaleSet-only | one (`gag-migrate-v1`) by choice, so `runs-on` survives the migration |

## Deploy

Prerequisites not expressible in kustomize (cluster infra / credentials):

1. **Node pool** — reuses the `e2e` pool from `scripts/dogfood/e2e-setup.sh` (`c2-standard-8` spot, tainted `dedicated=e2e`, autoscales from 0).
   No new pool.
2. **App credential Secret** (not in git — create after the namespace exists):
   ```bash
   kubectl create secret generic github-app-v1 -n gag-dogfood-migrate \
     --from-literal=appId=$APP_ID --from-literal=installationId=$INSTALLATION_ID \
     --from-file=privateKey=app.pem
   ```

Then apply the tenant:

```bash
kubectl apply -k deploy/dogfood-migrate
```

Rendered by kubectl's embedded kustomize — the repo carries no standalone `kustomize` binary, for the reasons in [`../dogfood-e2e/README.md`](../dogfood-e2e/README.md#why-kubectl-apply--k-not-a-standalone-kustomize-binary).

## Teardown

Deleting the namespace is **not** sufficient.
The migration creates a cluster-scoped `ClusterRunnerTemplate` and a ClusterRoleBinding that namespace deletion does not reclaim — which is exactly why `gag-migrate` stamps a provenance label on them.
Full teardown is step 11 of the [runbook](../../docs/plan/archive/q415-migrate-dogfood-validation.md#runbook).

## Trusted-only caveat

The bundled `NetworkPolicy` opens egress additively so the smoke job can pull its base image from Docker Hub, which is CDN-fronted.
That is the same trade-off, with the same trusted-only caveat, as the `dind` e2e overlay: this tenant runs one maintainer-dispatched workflow, never untrusted PR code.
