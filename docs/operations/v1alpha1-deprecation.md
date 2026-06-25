# `v1alpha1` API deprecation notice

> **Audience:** Platform engineer / tenant operator

The `actions-gateway.github.com/v1alpha1` API group — the monolithic
`ActionsGateway` (with an inline `proxy` and inline `runnerGroups[]`) and the
standalone `RunnerGroup` kind — is **deprecated** in favor of the decomposed
`actions-gateway.com/v2alpha1` API. This page records the deprecation, what stays
working during the transition, and what changes at removal.

## Status

- **`v1alpha1` is deprecated but still served.** Both API groups are served side by
  side. No release has removed v1 capability; existing v1 tenants keep running
  unchanged.
- **New tenants should onboard on v2.** See
  [tenant onboarding](tenant-onboarding.md) for the v2 object set.
- **Existing tenants migrate with the tool** on their own schedule — see
  [migration-v1-to-v2.md](migration-v1-to-v2.md). The migration is a one-shot
  fan-out, not an automatic conversion, because one v1 object becomes several v2
  objects.

## Why v2 (what the decomposition buys)

- **Reusable pod templates.** The large `PodTemplateSpec` moves to a referenced
  `RunnerTemplate`/`ClusterRunnerTemplate`, so one template is shared by many
  `RunnerSet`s instead of being copied into every group.
- **Multiple gateways per namespace.** The v1 one-gateway-per-namespace rule is
  dropped.
- **Standalone / shareable egress proxy.** The inline proxy becomes an `EgressProxy`
  kind any number of `RunnerSet`s can point at.
- **Namespace-scoped Pod Security profile.** `securityProfile` moves off the
  per-gateway spec onto the namespace, matching how Pod Security Admission actually
  works.

Full rationale: [Appendix H](../design/appendix-h-v2-api-decomposition.md).

## The dual-read window (what keeps working during coexistence)

The v2 cutover also aligns two grandfathered, boolean-looking label/annotation values
and moves the project's domain-prefixed keys off `actions-gateway.github.com/` onto
`actions-gateway.com/` (Q147 / the API-group rename). During coexistence every
consumer — the `ValidatingAdmissionPolicy` objects and the GMC validating webhook —
**dual-reads both spellings**:

| Key | Legacy (v1) | Aligned (v2) |
|---|---|---|
| tenant marker | `actions-gateway.github.com/tenant: "true"` | `actions-gateway.com/tenant: managed` |
| PSA profile | `ActionsGateway.spec.securityProfile` | `actions-gateway.com/security-profile` (namespace label) |
| privileged eligibility | `actions-gateway.github.com/privileged-profile: allowed` | `actions-gateway.com/privileged-profile: allowed` |
| downgrade opt-in | `actions-gateway.github.com/allow-profile-downgrade: "true"` | `actions-gateway.com/allow-profile-downgrade: allowed` |
| finalizers | `actions-gateway.github.com/gmc-cleanup`, `…/agentpool-cleanup` | `actions-gateway.com/gmc-cleanup`, `…/agentpool-cleanup` |

The migration tool relabels these in one pass (additively — it adds the v2 keys and
keeps the v1 keys). The dual-read **only widens accepted spelling**: it never relaxes
an invariant. The window **closes exactly when `v1alpha1` is removed**, at which point
the legacy `"true"` arms and the `actions-gateway.github.com/*` keys are dropped from
the policies and the webhook.

## Removal timeline

`v1alpha1` is removed on its own track once v2 adoption is sufficient. Removal will be
announced as a **named release** with at least one release of notice. At removal:

- The `actions-gateway.github.com` CRDs (`ActionsGateway`, `RunnerGroup`) are
  withdrawn; any remaining v1 objects must be migrated first.
- The dual-read window closes — the legacy label/annotation spellings and the v1
  finalizer names are no longer honored. Migrate (which relabels onto the v2 domain)
  before upgrading past the removal release.

Until that named release, no action is forced: run [`gag-migrate`](migration-v1-to-v2.md)
when convenient, validate the v2 path, and decommission v1 at your own pace.
