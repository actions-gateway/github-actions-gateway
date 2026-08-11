# Deprecation and removal notice: `v1alpha1`, `v2alpha1`, and Classic

> **Audience:** Platform engineer / tenant operator

!!! warning "Three deprecations, one removal release: `v2.0.0`"
    Onboard new tenants on the **v2 API** at `actions-gateway.com/v2beta1` (see [Getting Started](../getting-started.md#4-create-your-gateway-and-runner-set-v2-recommended)), and author them as single-label `ScaleSet` runner sets.
    Everything named below stays **fully served until `v2.0.0`**, so nothing is forced today.
    Migrate existing tenants with [`gag-migrate`](migration-v1-to-v2.md) at your convenience: the move changes the API objects, not how jobs are acquired.

This page is the project's standing deprecation notice.
It records what `v2.0.0` removes, what keeps working until then, and what an operator has to do before upgrading past it.
One further deprecation runs on its own, later clock — [the `CiliumFQDN` / `CalicoFQDN` egress modes](#a-fourth-deprecation-on-a-different-clock-ciliumfqdn--calicofqdn), removable no earlier than `v3.0.0`.

## What `v2.0.0` removes

| Removed at `v2.0.0` | What it is today | What replaces it | How you move |
|---|---|---|---|
| **`actions-gateway.github.com/v1alpha1`** | the monolithic `ActionsGateway` (inline `proxy` and `runnerGroups[]`) plus the standalone `RunnerGroup` kind | the decomposed `actions-gateway.com` API at `v2beta1` | [`gag-migrate`](migration-v1-to-v2.md), a one-shot fan-out of one v1 object into several v2 objects |
| **`actions-gateway.com/v2alpha1`** | v2's first served version, superseded as storage and hub version by the `v2beta1` graduation | `v2beta1`, the graduated, ScaleSet-only shape | read and re-apply your objects at `v2beta1`; the conversion webhook already round-trips them, so there is no re-author step except for the two `v2alpha1`-only fields below |
| **Classic acquisition** (`RunnerSet.spec.acquisitionProtocol: Classic` and `spec.maxListeners`, both `v2alpha1`-only) | the many-acquirers protocol, and the only protocol `v1alpha1` speaks | `ScaleSet`, the single-acquirer protocol: the default since `v1.1.0`, and the only protocol `v2beta1` serves | create one fresh single-label `ScaleSet` `RunnerSet` per `runs-on` target. `acquisitionProtocol` is immutable, so this is a create-and-delete, not an edit |

`v2beta1` itself is **not** affected.
Beta's contract is that a version will not be removed, and `v2.0.0` adds the GA `v2` version beside it rather than taking it away.

### Why the three are coupled

`v2beta1` is already ScaleSet-only, so classic acquisition exists *only* to serve `v1alpha1` and `v2alpha1` objects.
Removing those two versions removes classic's entire reason to exist.
Splitting the three removals across separate releases would buy nothing and would cost every operator a second breaking migration, so they land together on one major tag.

## A fourth deprecation on a different clock: `CiliumFQDN` / `CalicoFQDN`

The `EgressProxy` field `spec.egressPolicyMode` accepts two deprecated per-Container Network Interface (CNI) values, `CiliumFQDN` and `CalicoFQDN`, superseded by the `FQDN` intent plus the operator's `--fqdn-policy-backend` selector ([security-operations](security-operations.md#expressing-github-egress-by-fqdn-the-egresspolicymode-opt-in)).
They are **not** part of the `v2.0.0` bundle above.

**The earliest release that may remove them is `v3.0.0`.**

Why they cannot ride the `v2.0.0` clock:

1. **They are elements of a beta version, not a version themselves.** The two values exist in `v2alpha1` *and* in `v2beta1`, which is the storage and hub version.
   The `v2alpha1` copy disappears with `v2alpha1` at `v2.0.0`; the `v2beta1` copy does not, because `v2.0.0` keeps serving `v2beta1` — it adds the General Availability (GA) `v2` version beside it.
2. **An API element is removed by incrementing the version, never by deleting it from a served one.** Deleting a value from a version already in the field would reject objects an operator has stored and can still `kubectl apply` — the exact breakage the versioning contract exists to prevent.
   So the values live for as long as `v2beta1` is served.
3. **Beta promises the version will not be removed without a migration path.** Retiring a served version is a breaking change, and this project lands breaking changes on a major tag announced at least one release ahead ([roadmap](../roadmap.md)).
   The next major after `v2.0.0` is `v3.0.0`, so that is the earliest tag that can retire `v2beta1` and, with it, these two values.

Naming `v2.0.0` for them would have been a promise the API contract forbids keeping: the removal would have had to either break `v2beta1` objects or quietly not happen.

Two consequences worth stating plainly:

- **`v3.0.0` is not scheduled and carries no date**, exactly as `v2.0.0` carries none.
  It is gated on `v2beta1`'s retirement, which is gated in turn on the `v2` GA soak ([v2 GA plan](../plan/v2-ga.md)).
  The commitment here is a floor — *not before* `v3.0.0` — and the removal still gets its own one-release-ahead announcement.
- **Whether the GA `v2` version defines the two values at all is a separate, open question**, settled by the graduation hop rather than by this notice. `v2` is a new version, so it is free to omit them; if it does, an operator on `v2` simply cannot set them, while an operator on `v2beta1` still can until `v3.0.0`.
  Either way the removal release above is unchanged.

**What to do now:** nothing is forced, but migrate when convenient — set `egressPolicyMode: FQDN` on the `EgressProxy` and have the platform operator set the matching GMC `--fqdn-policy-backend`.
The admission webhook already warns on every write that still names a deprecated value, and the warning names `v3.0.0`.

## Status

- **All three are deprecated and still fully served.** No release has removed any of them.
  Existing tenants keep running unchanged until they upgrade past `v2.0.0`.
- **The apiserver says so too, on both deprecated versions.** Every `v1alpha1` and `v2alpha1` CRD carries `deprecated: true` plus a `deprecationWarning`, so `kubectl` prints the notice on any read or write of one of those versions — it is no longer something you have to have read this page to know:

    ```text
    Warning: actions-gateway.github.com/v1alpha1 RunnerGroup is deprecated; use actions-gateway.com/v2beta1 RunnerSet. v1alpha1 is served until v2.0.0, which removes it; migrate with gag-migrate.
    ```

    The warning is advisory: it does not fail an `apply`, and both controllers deduplicate it to one log line per process ([observability-logging](observability-logging.md#logging)).
    Classic acquisition carries no apiserver warning of its own — it is a *field value*, not a version, and the versions that admit it are the two that warn above.
- **New tenants should onboard on `v2beta1`.** See [tenant onboarding](tenant-onboarding.md) for the v2 object set.
- **`v2alpha1` stays served as the [`gag-migrate`](migration-v1-to-v2.md) on-ramp.** It carries the `acquisitionProtocol` selector a migrating v1 tenant needs, which a new tenant does not.
  Note that alpha's contract allows an alpha version to be dropped without notice; naming `v2.0.0` for it is a stronger commitment than the maturity level requires.
- **Existing tenants migrate with the tool** on their own schedule: see [migration-v1-to-v2.md](migration-v1-to-v2.md).
  The migration is a one-shot fan-out, not an automatic conversion, because one v1 object becomes several v2 objects.
- **Migrating preserves how your jobs are acquired.** `gag-migrate` maps v1 runner groups to v2 `RunnerSet`s that use the same job-acquisition path: it writes `acquisitionProtocol: Classic` onto every emitted set, so the migration changes the API objects, not the runtime behaviour, and is safe to do ahead of any other change.
  Adopting `ScaleSet` for a migrated group is a distinct, later step (create a fresh single-label set), never a side effect of migrating off `v1alpha1`.

### Two `v1alpha1` fields are inert

Both are in the served schema, both do nothing, and neither gains a behaviour before `v2.0.0` removes the version.
They are called out because the schema alone does not say so: one is rejected at admission despite reading like a supported knob, and the other is a status field the GMC never writes.

| Field | What it actually does | Read instead |
|---|---|---|
| `ActionsGateway.spec.gitHubAppRef.namespace` | Nothing. Any non-empty value is **rejected by the GMC validating webhook** on create and on update. The Secret always resolves in the `ActionsGateway`'s own namespace. | Leave it unset and put the Secret in the gateway's namespace. |
| `ActionsGateway.status.activeSessions` | Nothing. The GMC never sets it, so it is absent from every gateway. It is not a count that happens to be zero. | `status.activeSessions` on each `RunnerGroup`, or the `actions_gateway_active_sessions` metric ([observability-metrics](observability-metrics.md)). |

**An alert or dashboard reading `ActionsGateway.status.activeSessions` is reading a field no controller populates**, so it reports absent (or zero) no matter how busy the gateway is.
Point it at the per-`RunnerGroup` field or the metric instead.

Neither field is carried into v2. `v2beta1` replaces `SecretReference` with the name-only `LocalSecretReference`, which has no `namespace` at all, and `ActionsGatewayStatus` simply omits `activeSessions`.
So migrating with [`gag-migrate`](migration-v1-to-v2.md) needs no action for either: there is nothing to carry across.

They stay in `v1alpha1` rather than being deleted now because [removing a field from a served version is a breaking change](../development/api-review.md#removal-needs-a-version-increment-not-a-promise).
The removal is the version's own, at `v2.0.0`.

## Why v2 (what the decomposition buys)

- **Reusable pod templates.** The large `PodTemplateSpec` moves to a referenced `RunnerTemplate`/`ClusterRunnerTemplate`, so one template is shared by many `RunnerSet`s instead of being copied into every group.
- **Multiple gateways per namespace.** The v1 one-gateway-per-namespace rule is dropped.
- **Standalone / shareable egress proxy.** The inline proxy becomes an `EgressProxy` kind any number of `RunnerSet`s can point at.
- **Namespace-scoped Pod Security profile.** `securityProfile` moves off the per-gateway spec onto the namespace, matching how Pod Security Admission actually works.
- **Single-acquirer job acquisition.** `ScaleSet` holds one listener per runner set, so GitHub never assigns beyond advertised capacity.
  Classic's many-acquirers model can mark a job `in_progress` and then fail to provision a worker for it; measured on this project's own dogfood tenant, that orphaned 81% of acquired jobs ([troubleshooting](troubleshooting.md#concurrent-job-burst-serializes-to-1-worker-duplicate-job-acquisition)).

Full rationale: [Appendix H](../design/appendix-h-v2-api-decomposition.md).

## The dual-read window (what keeps working during coexistence)

The v2 cutover also aligns two grandfathered, boolean-looking label/annotation values and moves the project's domain-prefixed keys off `actions-gateway.github.com/` onto `actions-gateway.com/` (Q147 / the API-group rename).
During coexistence both consumers of these values, the `ValidatingAdmissionPolicy` objects and the GMC validating webhook, **dual-read both spellings**:

| Key | Legacy (v1) | Aligned (v2) |
|---|---|---|
| tenant marker | `actions-gateway.github.com/tenant: "true"` | `actions-gateway.com/tenant: managed` |
| PSA profile | `ActionsGateway.spec.securityProfile` | `actions-gateway.com/security-profile` (namespace label) |
| privileged eligibility | `actions-gateway.github.com/privileged-profile: allowed` | `actions-gateway.com/privileged-profile: allowed` |
| downgrade opt-in | `actions-gateway.github.com/allow-profile-downgrade: "true"` | `actions-gateway.com/allow-profile-downgrade: allowed` |
| finalizers | `actions-gateway.github.com/gmc-cleanup`, `…/agentpool-cleanup` | `actions-gateway.com/gmc-cleanup`, `…/agentpool-cleanup` |

The migration tool relabels these in one pass (additively: it adds the v2 keys and keeps the v1 keys).
The dual-read **only widens accepted spelling**, it never relaxes an invariant.
The window **closes at `v2.0.0`**, when `v1alpha1` is removed and the legacy `"true"` arms and the `actions-gateway.github.com/*` keys are dropped from the policies and the webhook.

## The removal release, and what it is gated on

**`v2.0.0` is the named removal release, announced with `v1.3.0`.** The project's policy is that a removal lands on a named release announced at least one release ahead ([roadmap](../roadmap.md)); `v1.3.0` carries that announcement for all three items above.

`v2.0.0` is a **major** tag because removing a served API version is a breaking change, and it is gated on the **`v2` (GA) API being available and validated**, not on a date and not on an adopter census.
Gating on v2 maturity is what lets the commitment be concrete: you are never asked to move onto an alpha to escape a removal, and there is no census to wait on. `v2beta1` is production-relyable today and converts to `v2` in place.

There is deliberately **no date**. `v1.3.0` fixes *which* release removes these and *what* it removes; the GA soak that gates `v2.0.0` finishes when the evidence says the v2 shape is right.

## Before you upgrade past `v2.0.0`

1. **Migrate off `v1alpha1`.** Run [`gag-migrate`](migration-v1-to-v2.md), which also relabels the namespace markers and annotations onto the `actions-gateway.com/` spellings.
   Do this *before* upgrading: after removal the legacy spellings and the v1 finalizer names are no longer honored, and any remaining `v1alpha1` objects have no served version.
2. **Move `v2alpha1` objects to `v2beta1`.** Re-apply them at `v2beta1` (or let the conversion webhook serve them there and re-record your GitOps manifests at that version) so nothing in Git still names a removed version.
3. **Replace Classic runner sets with `ScaleSet` sets.** Created fresh, since `acquisitionProtocol` is immutable.
   The labels carry across as they are, since a `ScaleSet` set registers every `runnerLabel`, so a multi-label group does not have to be split.
   Pick the *first* one deliberately: it names the scale set at GitHub and must be unique under the gateway.
   Drop `maxListeners`, which `ScaleSet` ignores.
   See [tenant onboarding](tenant-onboarding.md#acquisition-protocol-v2alpha1-only).

Until that release, no action is forced: run [`gag-migrate`](migration-v1-to-v2.md) when convenient, validate the v2 path, and decommission v1 at your own pace.
