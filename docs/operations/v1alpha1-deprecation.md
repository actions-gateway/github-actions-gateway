# Removal notice: `v1alpha1`, `v2alpha1`, `v2beta1`, and Classic

> **Audience:** Platform engineer, Tenant operator

!!! warning "Four removals, one release: `v2.0.0`"
    `v2.0.0` serves `actions-gateway.com/v2` alone.
    It removed `actions-gateway.github.com/v1alpha1`, `actions-gateway.com/v2alpha1`, `actions-gateway.com/v2beta1`, and Classic acquisition.
    A cluster upgrading from 1.x has to move its objects before the upgrade: the steps are [below](#before-you-upgrade-from-1x).

This page records what `v2.0.0` removed and what an operator upgrading from 1.x has to do first.
A fourth removal, [the `CiliumFQDN` / `CalicoFQDN` egress modes](#the-ciliumfqdn--calicofqdn-aliases-ride-the-v200-clock), was announced on a later clock and rode this one.

## What `v2.0.0` removed

| Removed at `v2.0.0` | What it was | What replaces it | How you move |
|---|---|---|---|
| **`actions-gateway.github.com/v1alpha1`** | the monolithic `ActionsGateway` (inline `proxy` and `runnerGroups[]`) plus the standalone `RunnerGroup` kind | the decomposed `actions-gateway.com` API at `v2` | a 1.x [`gag-migrate`](migration-v1-to-v2.md), a one-shot fan-out of one v1 object into several v2 objects |
| **`actions-gateway.com/v2alpha1`** | v2's first served version | `v2` | re-apply your objects at `v2` before the upgrade; the two `v2alpha1`-only fields (`acquisitionProtocol`, `maxListeners`) have no `v2` equivalent |
| **Classic acquisition** (`RunnerSet.spec.acquisitionProtocol: Classic` and `spec.maxListeners`) | the many-acquirers protocol, and the only protocol `v1alpha1` spoke | `ScaleSet`, the single-acquirer protocol: the default since `v1.1.0`, and the only protocol `v2` has | create one fresh `ScaleSet` `RunnerSet` per `runs-on` target |
| **`actions-gateway.com/v2beta1`** | the graduated beta version, the hub since `v1.1.0`, and the storage version from `v1.1.0` until 1.10 | `v2`, the General Availability (GA) version | read and re-apply your objects at `v2` |

`v2.0.0` is a major release, and dropping a served version is a breaking change a major release may make.
The tag before it, 1.10, served `v2beta1` and `v2` together with `v2` as the storage version, so no object was unreachable ([reasoning](../plan/v2beta1-retirement.md)).

**Removing a version also needs every stored object rewritten at `v2` first.** `gag-migrate storage-version` rewrites what is already stored; run it after upgrading to 1.10 and before `v2.0.0`, or the `v2.0.0` CRD apply is rejected ([how](upgrade.md#action-required-before-v200-v2-is-the-storage-version-and-stored-objects-need-rewriting)).

### Why the removals were coupled

`v2beta1` was already ScaleSet-only, so classic acquisition existed only to serve `v1alpha1` and `v2alpha1` objects.
Removing those two versions removed classic's entire reason to exist.
Splitting the removals across releases would have cost every operator a second breaking migration, so they landed together on one major tag.

## The `CiliumFQDN` / `CalicoFQDN` aliases ride the `v2.0.0` clock

The `EgressProxy` field `spec.egressPolicyMode` accepted two deprecated per-Container Network Interface (CNI) values, `CiliumFQDN` and `CalicoFQDN`, superseded by the `FQDN` intent plus the operator's `--fqdn-policy-backend` selector ([security-operations](security-operations.md#expressing-github-egress-by-fqdn-the-egresspolicymode-opt-in)).
Both versions that defined them are removed at `v2.0.0`, and the GA `v2` version does not define them, so `v2.0.0` is where they went.
The `v2` CRD accepts only `CIDR` and `FQDN`.

**What to do before upgrading to `v2.0.0`:** if any `EgressProxy` still names `CiliumFQDN` or `CalicoFQDN`, migrate it.
Set `egressPolicyMode: FQDN` on the `EgressProxy` and have the platform operator set the matching GMC `--fqdn-policy-backend` (`cilium` or `calico`).
The enforced policy is identical either way: each alias pinned its namesake backend, and `FQDN` plus the matching selector resolves to the same emitter, so this is a re-label, not a change in what is enforced.

This is not optional, and the reason is mechanical.
A `v2` read of a 1.x cluster shows a stored alias as `FQDN` and carries the alias in the annotation `conversion.actions-gateway.com/egress-policy-mode`, and a 1.10 GMC, which reads pools at `v2alpha1`, still sees the alias, so the pool keeps its pinned backend.
`v2.0.0` removes every version that can store the alias, so an unmigrated pool is rewritten as plain `FQDN` and enforced by whatever `--fqdn-policy-backend` names, which may not be the backend its alias pinned.
Migrating first makes that choice explicit instead of leaving it to the upgrade.

**Find them with the [pre-upgrade alias check](upgrade.md#before-upgrading-to-v200-no-egressproxy-still-names-a-deprecated-fqdn-alias)** in the Pre-Upgrade Validation Checklist, which prints one line per pool to migrate and nothing when the cluster is clean.

## Why v2 (what the decomposition buys)

- **Reusable pod templates.** The large `PodTemplateSpec` moves to a referenced `RunnerTemplate`/`ClusterRunnerTemplate`, so one template is shared by many `RunnerSet`s instead of being copied into every group.
- **Multiple gateways per namespace.** The v1 one-gateway-per-namespace rule is dropped.
- **Standalone / shareable egress proxy.** The inline proxy becomes an `EgressProxy` kind any number of `RunnerSet`s can point at.
- **Namespace-scoped Pod Security profile.** `securityProfile` moves off the per-gateway spec onto the namespace, matching how Pod Security Admission actually works.
- **Single-acquirer job acquisition.** `ScaleSet` holds one listener per runner set, so GitHub never assigns beyond advertised capacity.
  Classic's many-acquirers model can mark a job `in_progress` and then fail to provision a worker for it; measured on this project's own dogfood tenant, that orphaned 81% of acquired jobs.

Full rationale: [Appendix H](../design/appendix-h-v2-api-decomposition.md).

## The dual-read window closed at `v2.0.0`

The v2 cutover aligned two grandfathered, boolean-looking label/annotation values and moved the project's domain-prefixed keys off `actions-gateway.github.com/` onto `actions-gateway.com/` (Q147 / the API-group rename).
Through 1.x both consumers of these values, the `ValidatingAdmissionPolicy` objects and the GMC validating webhook, dual-read both spellings:

| Key | Legacy (v1) | Aligned (v2) |
|---|---|---|
| tenant marker | `actions-gateway.github.com/tenant: "true"` | `actions-gateway.com/tenant: managed` |
| PSA profile | `ActionsGateway.spec.securityProfile` | `actions-gateway.com/security-profile` (namespace label) |
| privileged eligibility | `actions-gateway.github.com/privileged-profile: allowed` | `actions-gateway.com/privileged-profile: allowed` |
| downgrade opt-in | `actions-gateway.github.com/allow-profile-downgrade: "true"` | `actions-gateway.com/allow-profile-downgrade: allowed` |
| finalizers | `actions-gateway.github.com/gmc-cleanup`, `…/agentpool-cleanup` | `actions-gateway.com/gmc-cleanup`, `…/agentpool-cleanup` |

The 1.x migration tool relabels these in one pass (additively: it adds the v2 keys and keeps the v1 keys).
`v2.0.0` drops the legacy `"true"` arms and the `actions-gateway.github.com/*` keys from the policies and the webhook, so only the aligned spellings are honored.

## Before you upgrade from 1.x

Do these on the 1.x controllers; none of them can be done afterwards.

1. **Migrate off `v1alpha1`.** Run [`gag-migrate`](migration-v1-to-v2.md) from a 1.x release, which also relabels the namespace markers and annotations onto the `actions-gateway.com/` spellings.
   [Decommission v1](migration-v1-to-v2.md#step-4--decommission-v1-when-ready) while the 1.x controllers still run: nothing in `v2.0.0` reconciles a remaining `v1alpha1` object.
   Helm keeps the two `v1alpha1` CRDs on upgrade (`helm.sh/resource-policy: keep`), so a leftover object stays stored and a `RunnerGroup`'s finalizer never clears; [upgrade.md](upgrade.md#before-upgrading-to-v200-no-v1alpha1-object-is-left) has the check and the cleanup.
2. **Move `v2alpha1` and `v2beta1` objects to `v2`.** Re-apply them at `v2` and re-record your GitOps manifests at that version, so nothing in Git still names a removed version.
3. **Replace Classic runner sets with `ScaleSet` sets.** Create them fresh.
   The labels carry across as they are, since a `ScaleSet` set registers every `runnerLabel`, so a multi-label group does not have to be split.
   Pick the *first* one deliberately: it names the scale set at GitHub and must be unique within the GitHub org, enterprise, or repo the gateway binds.
   Drop `maxListeners`, which `v2` does not have.
