# Migrating a tenant from the v1alpha1 to the v2alpha1 API

> **Audience:** Platform engineer / tenant operator

The v2 API (`actions-gateway.com`) replaces the monolithic `v1alpha1` `ActionsGateway` + `RunnerGroup` shape with a decomposed set of kinds — `ActionsGateway`, `EgressProxy`, `RunnerTemplate`/`ClusterRunnerTemplate`, and `RunnerSet`.
The two API groups are **served side by side**: nothing forces a tenant onto v2, and v1 keeps working until you migrate it.
Because one v1 object fans out into several v2 objects, the move is a tool-assisted **fan-out on create**, not an automatic conversion — see the [design rationale](../design/appendix-h-v2-api-decomposition.md#h11-migration-v2-tool-assisted).

This guide covers running the `gag-migrate` tool: dry-run → review → `--apply`, the coexistence/rollback story, and the post-migration teardown.

## Why upgrade to v2

v2 is **opt-in**.
It is served at two versions: **`v2beta1`** (the graduated, ScaleSet-only storage/hub version) and **`v2alpha1`** (deprecated, served as this tool's on-ramp).
`gag-migrate` lands v1 RunnerGroups on **`v2alpha1`** so a migrated set keeps the Classic protocol it was registered under during the deprecation window; a tenant then moves to `v2beta1` (a lossless, apiserver-side conversion) once it no longer needs Classic.
Multi-label matching is not a reason to stay: `v2beta1` registers every `runnerLabel` on the scale set.
Because `v2alpha1` is deprecated, the apiserver warns on every `v2alpha1` read or write, including the objects `gag-migrate` applies.
The warning is advisory and blocks nothing; it clears when the tenant moves to `v2beta1`.
[`v1alpha1` warns the same way](upgrade.md#non-breaking-v1alpha1-is-deprecated-and-the-apiserver-now-warns), so a tenant mid-migration sees a notice on both sides until v1 is decommissioned.
Detail: [upgrade.md](upgrade.md#non-breaking-v2alpha1-is-deprecated-and-the-apiserver-now-warns). v2 is **not a drop-in replacement** (new API group `actions-gateway.com`, one CR decomposed into several kinds, a tool-assisted fan-out).
`v1alpha1` stays fully supported until it is [removed at `v2.0.0`](v1alpha1-deprecation.md), which also removes `v2alpha1` and Classic, so treat the `v2alpha1` landing spot as a way station and `v2beta1` as the destination.
Migrate a tenant when one of these is worth that trade-off.

**New capabilities — no v1 equivalent:**

- **Multiple gateways per namespace.** Run several GitHub orgs, or rebalance `maxWorkers`/`priorityTiers` across runner sets against a single namespace `ResourceQuota`, without spreading across namespaces (v1 is one gateway per namespace).
- **Reusable runner templates.** A `RunnerTemplate` — or cluster-scoped `ClusterRunnerTemplate` for platform-owned golden DinD/sysbox templates — is referenced by many `RunnerSet`s instead of copied inline into each group.
  This also relieves the etcd per-object size pressure a fat inline template creates ([Appendix H §H.1](../design/appendix-h-v2-api-decomposition.md#h1-why-decompose)).
- **Shared or optional egress proxy.** The proxy is a standalone `EgressProxy` several runner sets can point at, or omit entirely for direct (still `NetworkPolicy`-restricted) egress. v1 couples exactly one proxy to one gateway ([§H.10](../design/appendix-h-v2-api-decomposition.md#h10-the-egress-proxy-becomes-optional)).
- **Workload-identity credentials.** `credentials.type: WorkloadIdentity` delegates App-JWT signing to an external signer (Vault transit MVP), so the GitHub App private key never enters the cluster — the secure-by-default credential model. v1's flat credential shape cannot express it ([05-security.md §5.7](../design/05-security.md#57-workload-identity-the-no-pem-delegation-model)).
- **Per-gateway control-plane sizing.** `ActionsGateway.spec.agcResources` tunes the AGC container CPU/memory per gateway (an additive overlay of the platform default). v1 has no equivalent field ([§H.4](../design/appendix-h-v2-api-decomposition.md#h4-spec-sketches)).
- **Per-gateway managed right-sizing.** `ActionsGateway.spec.agcAutoscaling` has the GMC stamp a `VerticalPodAutoscaler` next to the AGC `Deployment` so an autoscaler sizes its requests instead of you tuning `agcResources` by hand.
  Opt-in, recommendation-only by default, and it composes with `agcResources` rather than overriding it. v1 has no equivalent field ([tenant-onboarding](tenant-onboarding.md#letting-an-autoscaler-size-the-agc-agcautoscaling)).
- **DNS-aware egress policy.** `EgressProxy.egressPolicyMode` adds an `FQDN` intent (default `CIDR`) to allowlist GitHub by hostname; the operator picks the enforcement mechanism with the GMC `--fqdn-policy-backend` flag (`none`|`cilium`|`calico`|`gke`).
  The earlier per-CNI `CiliumFQDN` / `CalicoFQDN` values remain accepted-but-deprecated (Q245) and are **not** removed by `v2.0.0` — they are enum members of the beta version `v2beta1`, which `v2.0.0` keeps serving, so `v3.0.0` is the earliest release that may remove them ([why](v1alpha1-deprecation.md#a-fourth-deprecation-on-a-different-clock-ciliumfqdn--calicofqdn)).

**Quality-of-life and hardening — batched into the one schema break:**

- **Higher default concurrency.** `maxListeners` defaults to `10` (v1: `1`), so job pickup is not serialized per group.
  The migration tool pins each set to its v1 *effective* value, so an existing tenant's ceiling does not jump silently.
- **Better `kubectl` ergonomics.** Additional printer columns (Ready, profile, active sessions) and short names.
- **Safer by construction.** `githubURL` is immutable (CEL) — no accidental rebinding of a running gateway's org — and secret references are name-only, dropping v1's cross-namespace `SecretReference.namespace` footgun.
- **Fewer fail-closed webhooks.** Checks that needed a validating webhook on Kubernetes ≤ 1.30 move to structural/CEL, so admission has fewer single points of failure ([§H.15](../design/appendix-h-v2-api-decomposition.md#h15-other-breaking-changes-worth-batching)).

## What the tool does

`gag-migrate` reads a tenant's v1 `ActionsGateway` (and the `RunnerGroup` CRs the AGC serves in its namespace) and emits the equivalent v2 object set plus a namespace metadata patch:

| v1 source | v2 result |
|---|---|
| `ActionsGateway` identity (`gitHubAppRef.name`, `gitHubURL`, `logLevel`, `tracing`) | v2 `ActionsGateway` (same name) |
| `ActionsGateway.spec.proxy` (inline) | a standalone `EgressProxy` (`<gateway>-egress`), wired as the gateway's `defaultProxyRef`; inherits the v1 gateway-level `logLevel` (which governed both workloads) as its own `spec.logLevel` |
| each `RunnerGroup.spec.podTemplate` + `workerImage` | a `RunnerTemplate` — **identical templates collapse to one** |
| a `RunnerGroup.spec.podTemplate` with a **privileged** container (Docker-in-Docker, sysbox) | a cluster-scoped **`ClusterRunnerTemplate`** instead, referenced as `templateRef.kind: ClusterRunnerTemplate` — see [privileged worker shapes](#privileged-worker-shapes-dind-become-cluster-scoped-templates) |
| each `RunnerGroup` | a `RunnerSet` (`gatewayRef` + `templateRef`; `proxyRef` left unset so it inherits the gateway's `defaultProxyRef`) |
| `ActionsGateway.spec.securityProfile` | the namespace label `actions-gateway.com/security-profile` |

It also aligns the Q147 / domain-renamed namespace markers — adding the `actions-gateway.com/tenant=managed` marker, the `actions-gateway.com/security-profile` label, the domain-migrated `privileged-profile` grant, and the aligned `allow-profile-downgrade=allowed` annotation.
**These are additive:** the legacy `actions-gateway.github.com/*` keys are kept so v1 keeps working during coexistence (every admission policy dual-reads both during the window), and are removed only when `v1alpha1` is finally removed.

### Behavior-preserving guarantees

The fan-out preserves v1 behavior and weakens no security property:

- **Egress stays proxied.** The tool always emits an `EgressProxy` and always sets `defaultProxyRef`, so a migrated tenant never silently falls through to direct egress (which would lose its per-tenant egress-IP attribution).
- **Concurrency ceiling preserved.** v1 defaulted `maxListeners` to 1; v2 defaults to 10.
  The tool pins each `RunnerSet.maxListeners` to the v1 *effective* value, so the ceiling does not silently jump.
- **No secret is read.** Only the GitHub App Secret **name** is carried across — from v1's `spec.githubAppRef.name` into v2's `spec.credentials.githubApp.name` (Q196) — and the tool never reads, prints, or copies the credential Secret's contents.
- **The eligibility grant is never invented.** A tenant migrating to `securityProfile: privileged` keeps the *existing* platform grant (domain-migrated); if the namespace holds no grant, the tool warns rather than self-granting one.
  The grant is recognized on **either label domain** — `actions-gateway.github.com/` or `actions-gateway.com/`, both valued `allowed` — matching what the v1 admission webhook accepts during coexistence, so a namespace already relabelled onto the v2 domain is never reported as ungranted.

### Privileged worker shapes (DinD) become cluster-scoped templates

v2 refuses a privileged container in a **namespaced** `RunnerTemplate` — a tenant must not be able to self-author a privileged worker shape.
Privileged shapes live on the platform-owned, cluster-scoped `ClusterRunnerTemplate`, which exists precisely to hold golden Docker-in-Docker / sysbox templates ([§H.6](../design/appendix-h-v2-api-decomposition.md)).

So when a v1 `RunnerGroup`'s `podTemplate` declares a privileged container or init container, `gag-migrate` emits a `ClusterRunnerTemplate` rather than a `RunnerTemplate`, and sets the set's `templateRef.kind: ClusterRunnerTemplate`.
Non-privileged groups are unaffected.
**This changes what a migration touches**, so it is called out in the dry-run with a warning, and there are three things to know:

- **It needs cluster-scoped create permission.** `--apply` already requires permission to patch the tenant namespace; this adds `create` on `clusterrunnertemplates.actions-gateway.com`.
- **Deleting the tenant namespace does not reclaim it.** Cluster-scoped objects have no owning namespace.
  Every emitted template is labelled with the namespace it came from, so you can always find them again:

  ```bash
  kubectl get clusterrunnertemplates -l actions-gateway.com/migrated-from-namespace=team-a
  ```

- **Templates are not shared between tenants.** The emitted name is namespace-qualified (`crt-<namespace>-<hash>`), so two tenants with a byte-identical DinD worker shape still get their own object — editing one never changes the other's worker pods.
  Within one namespace, identical groups still collapse to a single template as usual.

Pod Security Admission remains the runtime enforcement backstop, exactly as it is for the namespaced kind: a privileged worker pod is only admitted in a namespace whose `actions-gateway.com/security-profile` is `privileged`, which in turn requires the platform `privileged-profile=allowed` grant the migration carries forward and never invents — held on either label domain.
A privileged tenant therefore migrates under exactly the grant it already had.

#### A privileged tenant needs the downgrade opt-in for the duration of the migration

Relocating `securityProfile: privileged` onto the namespace is **rejected by default**, and every privileged tenant hits this.
The `namespace-security-profile-guard` policy compares the incoming `actions-gateway.com/security-profile` label against the current one, and an **absent** label reads as the `baseline` default.
A tenant coming from v1 has never carried the v2 label, and `privileged` is the *least* restrictive level — so the relocation always presents as `baseline` → `privileged`, i.e. a downgrade, and is denied without the opt-in annotation.

The downgrade is only apparent: the namespace's PSA enforcement is *already* `privileged` under v1, stamped there by the GMC from the gateway's spec.
But the policy cannot see that, and it must not — it is the control that stops a stray re-apply from silently relaxing a tenant's isolation.
`gag-migrate` therefore warns in the dry-run instead of writing the annotation itself, for the same reason it never invents the eligibility grant: opting into a downgrade is the operator's decision.

Add it before `--apply`, and remove it once the migration is verified:

```bash
kubectl annotate namespace team-a actions-gateway.com/allow-profile-downgrade=allowed
gag-migrate --namespace team-a --context my-cluster --apply
kubectl annotate namespace team-a actions-gateway.com/allow-profile-downgrade-
```

Tenants migrating to `baseline` or `restricted` need none of this.

## Prerequisites

- The **v2 CRDs are installed** and the GMC is serving the v2 reconcilers — install the opt-in `actions-gateway-crds-v2` chart and restart the GMC if it was already running (see [Getting Started — the v2 CRDs](../getting-started.md#1-deploy-the-gmc)).
- `kubectl` access to the cluster with permission to read the tenant's v1 objects and (for `--apply`) create v2 objects and patch the namespace.
  A tenant with a privileged (DinD/sysbox) worker shape additionally needs `create` on cluster-scoped `clusterrunnertemplates.actions-gateway.com` — see [privileged worker shapes](#privileged-worker-shapes-dind-become-cluster-scoped-templates).
- Get the tool.
  Either **download the signed release binary** for your platform from the [GitHub Release](https://github.com/actions-gateway/github-actions-gateway/releases) (`gag-migrate-<version>-<os>-<arch>`), or **build from source**: from `cmd/gmc`, `make build-migrate` (or `make build-migrate` at the repo root) — both produce `.build/gag-migrate`.
  To verify a downloaded binary (keyless signature over the checksum manifest):

  ```bash
  cosign verify-blob --bundle SHA256SUMS.cosign.bundle SHA256SUMS \
    --certificate-identity-regexp 'https://github.com/actions-gateway/github-actions-gateway/.*' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  sha256sum -c SHA256SUMS   # then confirm your binary's checksum matches
  ```

### Pin the target cluster

`gag-migrate` talks to whatever your kubeconfig resolves to.
**Pass `--context` to pin the cluster explicitly** rather than relying on the ambient current-context (a parallel session can silently repoint it):

```bash
gag-migrate --namespace team-a --context my-cluster
```

Before any write, `--apply` echoes the resolved target context and scope and requires an explicit `yes`.
In automation, pass `--assume-yes` (or set `ASSUME_YES=1`) to skip the prompt.

## Step 1 — dry-run and review

Dry-run is the default; the tool prints the v2 manifests and applies nothing.

```bash
gag-migrate --namespace team-a            # print to stdout
gag-migrate --namespace team-a --output-dir ./migration   # or write per-namespace files
```

Review the output:

- Confirm one `EgressProxy`, the expected number of `RunnerTemplate`s (fewer than your `RunnerGroup` count if templates are shared — that is the object-size win), and one `RunnerSet` per group.
- Confirm `defaultProxyRef` is set on the `ActionsGateway`.
- If any `ClusterRunnerTemplate` appears, confirm you expected a privileged worker shape there — that object is cluster-scoped and outlives the tenant namespace ([detail](#privileged-worker-shapes-dind-become-cluster-scoped-templates)).
- Read the trailing **namespace patch** comment block — it lists the exact `kubectl label`/`kubectl annotate` commands the `--apply` path will run.
- Resolve any **warnings** (e.g. a name truncated under the 52-char cap, or a privileged profile with no eligibility grant) before applying.

To migrate every tenant at once, use `--all-namespaces` (discovers every namespace holding a v1 `ActionsGateway`).

## Step 2 — apply

```bash
gag-migrate --namespace team-a --context my-cluster --apply
# About to APPLY the v2 migration.
#   Target context: my-cluster
#   Scope:          namespace "team-a"
# ...
# Proceed? [y/N] y
```

`--apply` first **echoes the resolved target context and scope and waits for a `yes`** (the wrong-cluster guard) — so `--all-namespaces --apply` can never write cluster-wide against an unconfirmed context.
Pass `--assume-yes` (or `ASSUME_YES=1`) to skip the prompt in automation.

It then creates the v2 objects (children before referrers) and patches the namespace additively.
It is **idempotent**: an object that already exists is left untouched, so a re-run never clobbers a hand-edited v2 object.
It never deletes v1 objects.

Every create passes through a v2 validating webhook served by the GMC, so `--apply` **rides out a transiently unreachable webhook** rather than aborting partway through the fan-out.
If the webhook endpoint is mid-rollout (or the apiserver's call to it times out), you will see:

```text
admission webhook unreachable applying EgressProxy/team-a-egress: Internal error occurred: failed calling webhook ...
this is usually transient (webhook endpoint rolling out); retrying for up to 1m30s
```

This is informational — the apply continues on its own once the webhook answers, and gives up with the last error after **90 seconds** if it never does.
Only a webhook the apiserver could not *reach* is retried; a webhook that ran and **rejected** the object fails immediately with its own reason, so a real admission problem is never hidden behind a wait.

Verify the v2 set reaches steady state:

```bash
kubectl -n team-a get actionsgateways.actions-gateway.com,egressproxies,runnersets
# Each RunnerSet should reach Ready=True once its references resolve; a
# Ready=False/TemplateNotFound or /ProxyNotFound names the missing referent.
```

## Step 3 — coexistence, validation, and rollback

After `--apply`, the v1 and v2 object sets **run side by side**.
The v1 gateway keeps acquiring jobs exactly as before; the v2 gateway provisions its own AGC and runs its own `RunnerSet`s.
Validate the v2 path end to end (trigger a workflow that targets the v2 runner labels and confirm a worker pod is provisioned and egresses through the proxy) before removing v1.

### The two tenants keep separate runners

The tool keeps each tenant's name, so a namespace normally holds a `RunnerGroup` and a `RunnerSet` **called the same thing**.
They are still two independent tenants, and each provisions its own pool of pre-registered runners:

| | v1 `RunnerGroup` `web` | v2 `RunnerSet` `web` |
|---|---|---|
| Agent Secret | `agentpool-web-<index>` | `agentpool-rs-web-<index>` |
| Secret label | `actions-gateway/runner-group=web` | `actions-gateway.com/runner-set=web` |
| GitHub runner name | `web-<index>` | `rs-web-<index>` |

So during coexistence you will see **two sets of runners** registered with GitHub for one tenant — expected, and the reason both can run at once.
Each agent Secret also carries an `ownerReference` to the `RunnerGroup` or `RunnerSet` it belongs to, so `kubectl -n team-a get secret agentpool-web-0 -o jsonpath='{.metadata.ownerReferences}'` answers "which tenant owns this?" without guessing from the name.

### Each AGC serves exactly one API

The namespace runs two AGC Deployments during coexistence, and they divide the work by kind rather than sharing it:

| AGC | `GATEWAY_NAME` | Reconciles |
|---|---|---|
| the v1 singleton (`agc`) | unset | `RunnerGroup`s only |
| each migrated gateway's (`<gateway>-agc`) | the gateway's name | that gateway's `RunnerSet`s only |

So a tenant is served by exactly one controller, and the AGC whose logs answer a question about a `RunnerGroup` is always the v1 one.
Confirm which pod is which with:

```bash
kubectl -n team-a get deploy -l app.kubernetes.io/managed-by=gateway-manager-controller -o custom-columns='NAME:.metadata.name,GATEWAY:.spec.template.spec.containers[?(@.name=="agc")].env[?(@.name=="GATEWAY_NAME")].value'
```

A gateway-scoped AGC logs `v1alpha1 RunnerGroup reconciler disabled` at startup — that line is the division working, not a problem.

Upgrading an install that already ran v2 **moves** its existing agent Secrets onto the `agentpool-rs-` names on the first reconcile, carrying each agent's GitHub registration with it.
Nothing re-registers, and no Secret is left behind — but if you have scripts or dashboards that match `agentpool-<set>-` for a `RunnerSet`, update them to `agentpool-rs-<set>-`.

### The two proxy pools stay isolated

The namespace also runs two egress proxy pools during coexistence — v1's inline `actions-gateway-proxy` and the extracted `EgressProxy`'s `<proxy>-proxy` — and they are independent: separate `Deployment`s, `Service`s, `HorizontalPodAutoscaler`s, `PodDisruptionBudget`s, and `NetworkPolicy`s, each governing only its own pods.
Each pool's replicas spread across nodes among *themselves*, so two coexisting pools need `max(v1, v2)` worker nodes, not v1+v2.

What keeps them apart is the label each pool's selectors key on.
Select them separately:

| | v1 inline pool | v2 `EgressProxy` pool |
|---|---|---|
| Workload name | `actions-gateway-proxy` | `<proxy>-proxy` |
| Selector label | `app=actions-gateway-proxy` | `actions-gateway.com/egress-proxy=<proxy>` |
| Reached by | v1 AGC and worker pods | that gateway's AGC and worker pods |

```bash
kubectl -n team-a get pods -l app=actions-gateway-proxy                  # v1 pool only
kubectl -n team-a get pods -l actions-gateway.com/egress-proxy           # every v2 pool
```

Both pools' pods also carry the recommended `app.kubernetes.io/name=actions-gateway-proxy` label, which is the version-agnostic way to list every proxy pod in the namespace.
It is metadata for humans and tooling — do not build a `NetworkPolicy` or PDB selector on it.

Before `v1.3.0` the v2 pool also stamped `app: actions-gateway-proxy`, which put each pool's pods under the other's `PodDisruptionBudget`, wedged **both** pools' autoscaling on `AmbiguousSelector`, and made the two pools repel each other off every node.
If a namespace you migrated on an earlier release shows either symptom, upgrading the GMC clears it — and recreates that `EgressProxy`'s pool once in the process.
See [the upgrade note](upgrade.md#non-breaking-an-egressproxy-pools-pods-drop-the-app-actions-gateway-proxy-label-its-pool-is-recreated-once).

**Rollback is "stay on v1."** Nothing about the migration removes v1 capability, so if the v2 path misbehaves you simply keep using the v1 gateway and delete the v2 objects:

Order matters — the `RunnerSet`s go **before** the `ActionsGateway` that owns their AGC, because deleting the gateway first removes the controller whose finalizer they are waiting on (see [Teardown order is load-bearing](#teardown-order-is-load-bearing-never-delete-the-namespace-first)):

```bash
kubectl -n team-a delete runnersets.actions-gateway.com --all
kubectl -n team-a delete actionsgateways.actions-gateway.com --all
kubectl -n team-a delete egressproxies.actions-gateway.com --all
kubectl -n team-a delete runnertemplates.actions-gateway.com --all
# Cluster-scoped, so NOT covered by the -n team-a deletes above:
kubectl delete clusterrunnertemplates -l actions-gateway.com/migrated-from-namespace=team-a
```

The v1 tenant is unaffected by any of this: its agent Secrets and GitHub runner registrations are derived separately from the v2 set's ([detail](#the-two-tenants-keep-separate-runners)), so removing the v2 objects leaves the `RunnerGroup` exactly as it was.

The additive namespace labels are harmless to leave in place (the v1 markers are untouched), but you may remove the v2 keys if you want a clean rollback.

## Step 4 — decommission v1 (when ready)

Once the v2 path is validated and you are committed, tear down the v1 objects.
The v1 controllers are still running during coexistence, so deleting the v1 `ActionsGateway` runs its finalizer and cascades cleanup of its AGC, proxy, and `RunnerGroup` children normally — nothing is stranded:

```bash
kubectl -n team-a delete actionsgateways.actions-gateway.github.com --all
# Confirm the v1 RunnerGroups and the singleton AGC/proxy children are gone.
```

### Teardown order is load-bearing: never delete the namespace first

Whether you are rolling back, decommissioning v1, or retiring the tenant outright, **delete the custom resources while their controllers are still running, and delete the namespace last.**

```bash
# 1. The CRs first — their finalizers run while the AGC is still up.
kubectl -n team-a delete runnersets.actions-gateway.com --all
kubectl -n team-a delete runnergroups.actions-gateway.github.com --all
# 2. Then the gateways, which cascade their AGC, proxy, and RBAC children.
kubectl -n team-a delete actionsgateways.actions-gateway.com --all
kubectl -n team-a delete actionsgateways.actions-gateway.github.com --all
# 3. Only now, if you are retiring the tenant, the namespace.
kubectl delete namespace team-a
```

Step 2 blocks briefly while the AGC reaps the tenant's worker pods — deleting a v2 gateway kills any job still running on it, because the AGC is those pods' only reaper and is torn down with the gateway.
Drain first if you need in-flight jobs to finish: see [Worker Pods Reaped on Gateway Teardown](troubleshooting.md#worker-pods-reaped-on-gateway-teardown-workerpodsreapedongatewayteardown).

Deleting the namespace first **deadlocks**, and it is structural rather than a timing race: the AGC Deployments live *inside* the tenant namespace, so namespace deletion removes the very controllers whose finalizers must clear.
The namespace then sits in `Terminating` indefinitely on

- `actions-gateway.com/agentpool-cleanup` (v2 `RunnerSet`),
- `actions-gateway.github.com/agentpool-cleanup` (v1 `RunnerGroup`),
- `actions-gateway.github.com/gmc-cleanup` (v1 `ActionsGateway`).

Nothing will clear them on its own.
Recovery, once you have confirmed the namespace is otherwise empty and no cluster-scoped object depends on it, is to drop the finalizers by hand — which **skips the GitHub-side deregistration those finalizers exist to perform**, so the tenant's runner records are left registered and must be removed from the GitHub runners page afterwards:

```bash
kubectl -n team-a patch runnersets.actions-gateway.com <name> \
  --type merge -p '{"metadata":{"finalizers":[]}}'
```

Repeat per stuck kind.
Prefer the ordered teardown above; this is a recovery path, not an alternative.

If you later retire the tenant entirely, remember that any `ClusterRunnerTemplate` the migration emitted is cluster-scoped and survives deleting the namespace — reclaim it by its provenance label ([detail](#privileged-worker-shapes-dind-become-cluster-scoped-templates)).

The legacy `actions-gateway.github.com/*` namespace markers and finalizers are retired cluster-wide when `v1alpha1` is removed at **`v2.0.0`** (see the [deprecation and removal notice](v1alpha1-deprecation.md)); until then the dual-read window keeps both spellings working.

## Troubleshooting

- **A `RunnerSet` sits `Ready=False`/`TemplateNotFound`.** Its `templateRef` names a template that has not been applied (or was hand-deleted).
  Re-run the dry-run and apply the missing `RunnerTemplate`.
  For a privileged set, check `templateRef.kind` is `ClusterRunnerTemplate` — with no explicit kind the referent resolves as a namespaced `RunnerTemplate`, which will never be found.
- **`--apply` fails creating a `ClusterRunnerTemplate` with a permissions error.** The tenant has a privileged worker shape, which migrates to a cluster-scoped object; the account running the tool needs `create` on `clusterrunnertemplates.actions-gateway.com` ([detail](#privileged-worker-shapes-dind-become-cluster-scoped-templates)).
- **The namespace patch is rejected.** The `actions-gateway.com/security-profile` label is guarded by the `namespace-security-profile-guard` admission policy — a downgrade needs the `allow-profile-downgrade=allowed` annotation, and `privileged` needs the platform `privileged-profile=allowed` grant.
  The dry-run warnings call these out.
  Migrating a **privileged** tenant always trips the downgrade rule; see [the downgrade opt-in](#a-privileged-tenant-needs-the-downgrade-opt-in-for-the-duration-of-the-migration).
  Note the ordering cost of discovering this at `--apply` time rather than in the dry-run: the namespace patch is the **last** step, so the v2 objects are already created when it fails.
  Re-running after adding the annotation is safe — apply is idempotent and skips what exists.
- **`--apply` fails with `failed calling webhook … context deadline exceeded`.** The apiserver could not reach the GMC validating webhook.
  `--apply` already retries this for 90 seconds, so reaching this error means the webhook was down for longer than that — check the GMC deployment is `Running` with programmed endpoints (`kubectl -n gag-system get deploy,endpoints`) before re-running.
  Re-running is safe: apply is idempotent and skips whatever the aborted run already created.
- **The dry-run warns the namespace "holds no privileged-eligibility grant on either label domain".** The tenant migrates to `securityProfile: privileged` but carries the grant label on neither domain, or carries it with a value other than `allowed` (the match is exact — the legacy `"true"` is *not* a grant here).
  Apply the v2 label the warning names, as a platform administrator:

  ```bash
  kubectl label namespace team-a actions-gateway.com/privileged-profile=allowed
  ```

  The warning fires only when the grant is genuinely absent from both domains: a namespace holding it on either one is recognized, and the tool carries it forward onto the v2 domain without complaint.
- **`gag-migrate` reports no namespaces.** With `--all-namespaces` it only targets namespaces holding a v1 `ActionsGateway`; pass `--namespace` explicitly otherwise.
