# M4: cross-namespace `EgressProxy` sharing (Q166)

> **Status: in progress.** Scope decided 2026-08-08: the full milestone, both the
> gateway control-plane path and the worker path. Design context is
> [appendix H §H.9](../../design/appendix-h-v2-api-decomposition.md#h9-cross-namespace-proxy-sharing);
> this doc records what §H.9 left unspecified and what was measured to settle it.

## The defect

`EgressProxy.spec.sharing.allowedNamespaces` is served by `v2alpha1` and
`v2beta1` and read by nothing. Measured 2026-08-08 at `1e6cbdea`: the field
appears in `api/v2alpha1/shared_types.go`, `api/v2beta1/shared_types.go`, the two
generated deepcopy files, and no other non-vendored Go file. Zero enforcement
sites.

`v2beta1` shipped in `v1.3.0`, so the field is already inside a beta contract and
removing it would be breaking. It has to be implemented, and implemented before
1.4.0 hardens it further.

## What §H.9 did not account for

Two findings from reading the code, both of which change the shape of the work.

### Cross-namespace references are not expressible

No reference type carries a namespace. `ObjectRef` and `LocalObjectRef` are
name-only, and both resolution sites resolve in the referrer's own namespace:
[`actionsgateway_v2_controller.go:221`](../../../cmd/gmc/internal/controller/actionsgateway_v2_controller.go)
for `defaultProxyRef`, [`runnerset_target.go:476`](../../../cmd/agc/internal/controller/runnerset_target.go)
for `proxyRef`.

So `allowedNamespaces` cannot gate anything today: there is no reference for it to
refuse. M4 builds the path and the consent check together. A consent check alone
would gate a reference nobody can write.

The corollary is that the secure default holds by construction rather than by
argument: with no namespace on the reference, and with the proxy's ingress
NetworkPolicy peer carrying a bare `PodSelector` (which means the policy's own
namespace, per [`egressproxy_builder.go:546`](../../../cmd/gmc/internal/controller/egressproxy_builder.go)),
a cross-namespace consumer is refused at both the API and the network layer.
Every change below has to preserve that for an absent or empty
`allowedNamespaces`.

### The AGC cannot read a remote `EgressProxy`

This is the finding that decides the architecture. The AGC's cache is pinned to
its own tenant namespace so that it needs only the per-tenant `Role` the GMC
creates, rather than a `ClusterRole`. See [`cmd/agc/main.go:284-292`](../../../cmd/agc/main.go),
deliberate and documented in place. `RunnerSet.spec.proxyRef` is resolved by the
AGC. So the worker path cannot be built by teaching the AGC to read across
namespaces without either widening its RBAC or adding a projection mechanism.

Three ways out, and why one wins:

| Approach | Verdict |
|---|---|
| Give the AGC a `ClusterRole` for `egressproxies` | **Rejected.** Every tenant's AGC could read every other tenant's `EgressProxy` spec. That is a blast-radius regression, and the secure-by-default rule says the more secure option stays the default. |
| Per-grant `Role`+`RoleBinding` in the provider namespace, plus an uncached read | **Rejected.** Least privilege is preserved, but controller-runtime fixes cache namespaces at manager construction, so the AGC gets no watch and must poll. RBAC churn per grant, for nothing the option below does not already give. |
| **GMC-mediated projection** | **Chosen.** |

The GMC is cluster-scoped and already owns every piece: `defaultProxyRef`
resolution, proxy CA generation, the AGC's CA mount, the AGC NetworkPolicy, and
the proxy NetworkPolicy.

§H.9 already requires the GMC to write the proxy CA as a ConfigMap into each
granted consumer namespace. That ConfigMap is made load-bearing for one more
thing: **its existence is the grant**, and its data carries the proxy's Service
DNS name and port. The AGC then reads everything it needs from its own namespace
with no cross-namespace read, no cache change, no RBAC widening. Revoking a grant
deletes the ConfigMap, and the AGC fails closed.

This is not a new mechanism. It is the mechanism §H.9 already mandates, carrying
the grant decision alongside the trust material that decision authorises.

## API shape

### Why the namespace cannot go on `ObjectRef`

`ObjectRef` backs `gatewayRef`, `templateRef`, `proxyRef`, and
`defaultTemplateRef`. A `Namespace` field on it would make all four
cross-namespace at once, a security regression on three references that have no
consent handshake behind them.

So the proxy references get their own type:

```go
type ProxyObjectRef struct {
    Name      string `json:"name"`
    Namespace string `json:"namespace,omitempty"` // +optional; empty ⇒ same namespace
}
```

used by `RunnerSetSpec.ProxyRef` and `ActionsGatewaySpec.DefaultProxyRef`.

Empty `namespace` means the referrer's own namespace, which is exactly today's
behaviour, so every existing manifest keeps its meaning.

### The one schema subtraction, flagged for review

For `defaultProxyRef` the change is purely additive: `LocalObjectRef` carried
`name` alone, and the new type adds `namespace`.

For `proxyRef` it also **removes** the optional `kind` property, which
`ObjectRef` contributed. This is a subtraction from a served beta version and is
called out rather than assumed:

- `kind`'s enum admits only `RunnerTemplate` and `ClusterRunnerTemplate`, neither
  of which names a proxy, so no manifest can set it meaningfully.
- Nothing reads it on `proxyRef`; the field's own godoc says it is load-bearing
  only on `templateRef`.
- Structural-schema pruning drops an undeclared property silently rather than
  rejecting the apply, so a stale manifest that sets it keeps applying.

Behaviour is therefore identical before and after. The alternative, carrying a
nonsense `kind` on `proxyRef` into the `v2` GA contract to avoid touching a
property nothing can use, costs more than it protects.

## Deliverables

1. **API.** `ProxyObjectRef` in both version packages; `ProxyShareNotGranted`
   reason in `api/apiconditions` re-exported from both; deepcopy, CRD, chart CRD
   and API-reference regeneration. Conversion is free: it is a JSON round-trip
   and both versions carry the same shape.
2. **Consent check.** Both resolution sites. Absent or empty
   `allowedNamespaces` denies. Fail closed with `ProxyShareNotGranted`.
3. **CA distribution.** GMC writes the CA ConfigMap into granted consumer
   namespaces, and deletes it when the grant is revoked.
4. **Dual-side NetworkPolicy.** Provider ingress gains granted namespaces;
   consumer egress gains the remote proxy. The granted-namespace ingress peer
   must carry `NamespaceSelector` and `PodSelector` in **one** peer struct: two
   entries in the `From` list is an OR, which would admit any pod in the granted
   namespace and any workload pod anywhere.
5. **Watches.** Cross-namespace mappers so a grant change re-reconciles remote
   consumers.

## Testing

envtest, in both existing integration suites, because the denial is a controller
decision observable against a real apiserver. The causation claim, that the
consent check is what denies, is settled by deleting the check and requiring the
denial assertion to go red, per
[testing.md](../../development/testing.md#verify-a-causation-claim-by-deleting-the-mechanism).

## Status

Recorded as work lands. Nothing here is written ahead of the evidence.

- 2026-08-08: scope decided (full M4, both paths); architecture decided
  (GMC-mediated projection); API shape decided (`ProxyObjectRef`).
- 2026-08-08: API landed. `ProxyObjectRef` in both version packages,
  `ProxyShareNotGranted` in `apiconditions` and both re-exports, `LocalObjectRef`
  retired as unused. `make v2-api-sync-check` green; CRDs, chart CRDs, deepcopy and
  `docs/reference/api.md` regenerated.
- 2026-08-08: enforcement landed. Consent check on both resolution sites, CA
  projection with prune-on-revoke, dual-side NetworkPolicy, cross-namespace watch
  mappers. The proxy's GitHub-host allowlist now also spans granted remote referrers,
  which same-namespace assembly had missed.
- 2026-08-08: one defect found and fixed during the build. The GMC's ConfigMap
  informer is pinned to a single name in its own namespace, so the cached
  label-selected List that drives the prune would have returned nothing and a
  withdrawn grant would never have had its projection deleted. The consumer treats
  that projection as the grant, so revocation would have been silent. Reads now go
  through the uncached `APIReader`, and the RBAC takes `list` without `watch`.
- 2026-08-08: causation settled per testing.md. Replacing `proxyShareGranted`'s body
  with `return true` turns `TestProxyShareGranted_DeniesWithoutExplicitConsent` and
  `TestProxyShareGranted_MatchesWholeNamespaceOnly` red; restoring it returns them to
  green. The NetworkPolicy and projection tests stay green under that edit, correctly
  because they are built from `allowedNamespaces` directly and are a second, independent
  guard rather than the same one twice.
