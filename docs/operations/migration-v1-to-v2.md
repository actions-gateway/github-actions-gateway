# Migrating a tenant from the v1alpha1 to the v2alpha1 API

> **Audience:** Platform engineer / tenant operator

The v2 API (`actions-gateway.com`) replaces the monolithic `v1alpha1`
`ActionsGateway` + `RunnerGroup` shape with a decomposed set of kinds —
`ActionsGateway`, `EgressProxy`, `RunnerTemplate`/`ClusterRunnerTemplate`, and
`RunnerSet`. The two API groups are **served side by side**: nothing forces a
tenant onto v2, and v1 keeps working until you migrate it. Because one v1 object
fans out into several v2 objects, the move is a tool-assisted **fan-out on
create**, not an automatic conversion — see the
[design rationale](../design/appendix-h-v2-api-decomposition.md#h11-migration-v2-tool-assisted).

This guide covers running the `gag-migrate` tool: dry-run → review → `--apply`, the
coexistence/rollback story, and the post-migration teardown.

## Why upgrade to v2

v2 is **opt-in**. It is served at two versions: **`v2beta1`** (the graduated,
ScaleSet-only storage/hub version) and **`v2alpha1`** (still served for coexistence).
`gag-migrate` lands v1 RunnerGroups on **`v2alpha1`** so a migrated set keeps its
Classic protocol and multi-label matching during the deprecation window; a tenant then
moves to `v2beta1` (a lossless, apiserver-side conversion) once it no longer needs
Classic. v2 is **not a drop-in replacement** (new API group `actions-gateway.com`, one
CR decomposed into several kinds, a tool-assisted fan-out). `v1alpha1` stays fully
supported. Migrate a tenant when one of these is worth that trade-off.

**New capabilities — no v1 equivalent:**

- **Multiple gateways per namespace.** Run several GitHub orgs, or rebalance
  `maxWorkers`/`priorityTiers` across runner sets against a single namespace
  `ResourceQuota`, without spreading across namespaces (v1 is one gateway per namespace).
- **Reusable runner templates.** A `RunnerTemplate` — or cluster-scoped
  `ClusterRunnerTemplate` for platform-owned golden DinD/sysbox templates — is
  referenced by many `RunnerSet`s instead of copied inline into each group. This also
  relieves the etcd per-object size pressure a fat inline template creates
  ([Appendix H §H.1](../design/appendix-h-v2-api-decomposition.md#h1-why-decompose)).
- **Shared or optional egress proxy.** The proxy is a standalone `EgressProxy` several
  runner sets can point at, or omit entirely for direct (still `NetworkPolicy`-restricted)
  egress. v1 couples exactly one proxy to one gateway
  ([§H.10](../design/appendix-h-v2-api-decomposition.md#h10-the-egress-proxy-becomes-optional)).
- **Workload-identity credentials.** `credentials.type: WorkloadIdentity` delegates
  App-JWT signing to an external signer (Vault transit MVP), so the GitHub App private
  key never enters the cluster — the secure-by-default credential model. v1's flat
  credential shape cannot express it
  ([05-security.md §5.7](../design/05-security.md#57-workload-identity-the-no-pem-delegation-model)).
- **Per-gateway control-plane sizing.** `ActionsGateway.spec.agcResources` tunes the AGC
  container CPU/memory per gateway (an additive overlay of the platform default). v1 has
  no equivalent field ([§H.4](../design/appendix-h-v2-api-decomposition.md#h4-spec-sketches)).
- **DNS-aware egress policy.** `EgressProxy.egressPolicyMode` adds an `FQDN` intent
  (default `CIDR`) to allowlist GitHub by hostname; the operator picks the enforcement
  mechanism with the GMC `--fqdn-policy-backend` flag (`none`|`cilium`|`calico`|`gke`).
  The earlier per-CNI `CiliumFQDN` / `CalicoFQDN` values remain accepted-but-deprecated
  (Q245).

**Quality-of-life and hardening — batched into the one schema break:**

- **Higher default concurrency.** `maxListeners` defaults to `10` (v1: `1`), so job
  pickup is not serialized per group. The migration tool pins each set to its v1
  *effective* value, so an existing tenant's ceiling does not jump silently.
- **Better `kubectl` ergonomics.** Additional printer columns (Ready, profile, active
  sessions) and short names.
- **Safer by construction.** `githubURL` is immutable (CEL) — no accidental rebinding of
  a running gateway's org — and secret references are name-only, dropping v1's
  cross-namespace `SecretReference.namespace` footgun.
- **Fewer fail-closed webhooks.** Checks that needed a validating webhook on Kubernetes
  ≤ 1.30 move to structural/CEL, so admission has fewer single points of failure
  ([§H.15](../design/appendix-h-v2-api-decomposition.md#h15-other-breaking-changes-worth-batching)).

## What the tool does

`gag-migrate` reads a tenant's v1 `ActionsGateway` (and the `RunnerGroup` CRs the
AGC serves in its namespace) and emits the equivalent v2 object set plus a namespace
metadata patch:

| v1 source | v2 result |
|---|---|
| `ActionsGateway` identity (`gitHubAppRef.name`, `gitHubURL`, `logLevel`, `tracing`) | v2 `ActionsGateway` (same name) |
| `ActionsGateway.spec.proxy` (inline) | a standalone `EgressProxy` (`<gateway>-egress`), wired as the gateway's `defaultProxyRef`; inherits the v1 gateway-level `logLevel` (which governed both workloads) as its own `spec.logLevel` |
| each `RunnerGroup.spec.podTemplate` + `workerImage` | a `RunnerTemplate` — **identical templates collapse to one** |
| each `RunnerGroup` | a `RunnerSet` (`gatewayRef` + `templateRef`; `proxyRef` left unset so it inherits the gateway's `defaultProxyRef`) |
| `ActionsGateway.spec.securityProfile` | the namespace label `actions-gateway.com/security-profile` |

It also aligns the Q147 / domain-renamed namespace markers — adding the
`actions-gateway.com/tenant=managed` marker, the `actions-gateway.com/security-profile`
label, the domain-migrated `privileged-profile` grant, and the aligned
`allow-profile-downgrade=allowed` annotation. **These are additive:** the legacy
`actions-gateway.github.com/*` keys are kept so v1 keeps working during coexistence
(every admission policy dual-reads both during the window), and are removed only when
`v1alpha1` is finally removed.

### Behavior-preserving guarantees

The fan-out preserves v1 behavior and weakens no security property:

- **Egress stays proxied.** The tool always emits an `EgressProxy` and always sets
  `defaultProxyRef`, so a migrated tenant never silently falls through to direct
  egress (which would lose its per-tenant egress-IP attribution).
- **Concurrency ceiling preserved.** v1 defaulted `maxListeners` to 1; v2 defaults to
  10. The tool pins each `RunnerSet.maxListeners` to the v1 *effective* value, so the
  ceiling does not silently jump.
- **No secret is read.** Only the GitHub App Secret **name** is carried across — from
  v1's `spec.githubAppRef.name` into v2's `spec.credentials.githubApp.name` (Q196) — and
  the tool never reads, prints, or copies the credential Secret's contents.
- **The eligibility grant is never invented.** A tenant migrating to
  `securityProfile: privileged` keeps the *existing* platform grant (domain-migrated);
  if the namespace holds no grant, the tool warns rather than self-granting one.

## Prerequisites

- The **v2 CRDs are installed** and the GMC is serving the v2 reconcilers — install
  the opt-in `actions-gateway-crds-v2` chart and restart the GMC if it was already
  running (see [Getting Started — the v2 CRDs](../getting-started.md#1-deploy-the-gmc)).
- `kubectl` access to the cluster with permission to read the tenant's v1 objects and
  (for `--apply`) create v2 objects and patch the namespace.
- Get the tool. Either **download the signed release binary** for your platform from
  the [GitHub Release](https://github.com/actions-gateway/github-actions-gateway/releases)
  (`gag-migrate-<version>-<os>-<arch>`), or **build from source**: from `cmd/gmc`,
  `make build-migrate` (or `make build-migrate` at the repo root) — both produce
  `.build/gag-migrate`. To verify a downloaded binary (keyless signature over the
  checksum manifest):

  ```bash
  cosign verify-blob --bundle SHA256SUMS.cosign.bundle SHA256SUMS \
    --certificate-identity-regexp 'https://github.com/actions-gateway/github-actions-gateway/.*' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  sha256sum -c SHA256SUMS   # then confirm your binary's checksum matches
  ```

### Pin the target cluster

`gag-migrate` talks to whatever your kubeconfig resolves to. **Pass `--context` to pin
the cluster explicitly** rather than relying on the ambient current-context (a parallel
session can silently repoint it):

```bash
gag-migrate --namespace team-a --context my-cluster
```

Before any write, `--apply` echoes the resolved target context and scope and requires an
explicit `yes`. In automation, pass `--assume-yes` (or set `ASSUME_YES=1`) to skip the
prompt.

## Step 1 — dry-run and review

Dry-run is the default; the tool prints the v2 manifests and applies nothing.

```bash
gag-migrate --namespace team-a            # print to stdout
gag-migrate --namespace team-a --output-dir ./migration   # or write per-namespace files
```

Review the output:

- Confirm one `EgressProxy`, the expected number of `RunnerTemplate`s (fewer than your
  `RunnerGroup` count if templates are shared — that is the object-size win), and one
  `RunnerSet` per group.
- Confirm `defaultProxyRef` is set on the `ActionsGateway`.
- Read the trailing **namespace patch** comment block — it lists the exact
  `kubectl label`/`kubectl annotate` commands the `--apply` path will run.
- Resolve any **warnings** (e.g. a name truncated under the 52-char cap, or a
  privileged profile with no eligibility grant) before applying.

To migrate every tenant at once, use `--all-namespaces` (discovers every namespace
holding a v1 `ActionsGateway`).

## Step 2 — apply

```bash
gag-migrate --namespace team-a --context my-cluster --apply
# About to APPLY the v2 migration.
#   Target context: my-cluster
#   Scope:          namespace "team-a"
# ...
# Proceed? [y/N] y
```

`--apply` first **echoes the resolved target context and scope and waits for a `yes`**
(the wrong-cluster guard) — so `--all-namespaces --apply` can never write cluster-wide
against an unconfirmed context. Pass `--assume-yes` (or `ASSUME_YES=1`) to skip the
prompt in automation.

It then creates the v2 objects (children before referrers) and patches the namespace
additively. It is **idempotent**: an object that already exists is left untouched, so a
re-run never clobbers a hand-edited v2 object. It never deletes v1 objects.

Verify the v2 set reaches steady state:

```bash
kubectl -n team-a get actionsgateways.actions-gateway.com,egressproxies,runnersets
# Each RunnerSet should reach Ready=True once its references resolve; a
# Ready=False/TemplateNotFound or /ProxyNotFound names the missing referent.
```

## Step 3 — coexistence, validation, and rollback

After `--apply`, the v1 and v2 object sets **run side by side**. The v1 gateway keeps
acquiring jobs exactly as before; the v2 gateway provisions its own AGC and runs its
own `RunnerSet`s. Validate the v2 path end to end (trigger a workflow that targets the
v2 runner labels and confirm a worker pod is provisioned and egresses through the
proxy) before removing v1.

**Rollback is "stay on v1."** Nothing about the migration removes v1 capability, so if
the v2 path misbehaves you simply keep using the v1 gateway and delete the v2 objects:

```bash
kubectl -n team-a delete actionsgateways.actions-gateway.com --all
kubectl -n team-a delete runnersets.actions-gateway.com --all
kubectl -n team-a delete egressproxies.actions-gateway.com --all
kubectl -n team-a delete runnertemplates.actions-gateway.com --all
```

The additive namespace labels are harmless to leave in place (the v1 markers are
untouched), but you may remove the v2 keys if you want a clean rollback.

## Step 4 — decommission v1 (when ready)

Once the v2 path is validated and you are committed, tear down the v1 objects. The v1
controllers are still running during coexistence, so deleting the v1 `ActionsGateway`
runs its finalizer and cascades cleanup of its AGC, proxy, and `RunnerGroup` children
normally — nothing is stranded:

```bash
kubectl -n team-a delete actionsgateways.actions-gateway.github.com --all
# Confirm the v1 RunnerGroups and the singleton AGC/proxy children are gone.
```

The legacy `actions-gateway.github.com/*` namespace markers and finalizers are
retired cluster-wide when `v1alpha1` is removed (see the
[deprecation notice](v1alpha1-deprecation.md)); until then the dual-read window keeps
both spellings working.

## Troubleshooting

- **A `RunnerSet` sits `Ready=False`/`TemplateNotFound`.** Its `templateRef` names a
  template that has not been applied (or was hand-deleted). Re-run the dry-run and
  apply the missing `RunnerTemplate`.
- **The namespace patch is rejected.** The `actions-gateway.com/security-profile`
  label is guarded by the `namespace-security-profile-guard` admission policy — a
  downgrade needs the `allow-profile-downgrade=allowed` annotation, and `privileged`
  needs the platform `privileged-profile=allowed` grant. The dry-run warnings call
  these out.
- **`gag-migrate` reports no namespaces.** With `--all-namespaces` it only targets
  namespaces holding a v1 `ActionsGateway`; pass `--namespace` explicitly otherwise.
