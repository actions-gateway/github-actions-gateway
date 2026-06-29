# Q242 — G.1 Proxy-Enforced Destination Allowlist (worker dependency egress)

> **Status: DESIGN — awaiting sign-off. No implementation has landed.** This
> promotes [Appendix G.1](../design/appendix-g-future-enhancements.md#g1-proxy-enforced-destination-allowlist)
> (tracked under the non-committed Q19 bundle) to committed work, because it is
> the attribution-preserving answer to the single most common operator ask:
> letting CI jobs reach their build dependencies (package registries, module
> proxies, test-asset hosts) without forfeiting the per-tenant egress model.

## Goal

Let an operator allow a **per-tenant egress proxy** to forward worker traffic to
a small, explicit set of **non-GitHub** hosts (e.g. `proxy.golang.org`,
`sum.golang.org`) — so jobs that fetch dependencies work — while keeping every
property the GitHub-only egress posture provides: per-tenant egress-IP
attribution, the DNS-exfil containment, and "no direct worker egress hole."

**Concrete driver (Q224 / Q239).** On the GKE dogfood the offline-capable jobs
(`lint`, `shellcheck`, `unit-test`, `coverage`) now run green on the Q239
build-capable image, but `vendor-check` and `tidy-check` re-fetch modules from
`proxy.golang.org` on a cold cache. The worker egress `NetworkPolicy` allows TCP
443 only to the ~7400 GitHub CIDRs, so those two jobs fail — not on toolchain, on
egress. This feature is what makes them green the *right* way (rather than
punting them to `ubuntu-latest`, which forfeits the isolation the dogfood exists
to demonstrate, or widening the CIDR allowlist, which erodes it).

## Background — why this isn't built yet

The egress proxy ([`cmd/proxy/proxy.go`](../../cmd/proxy/proxy.go)) is a
deliberately **transport-only** HTTPS `CONNECT` tunneler: `handleConnect`
(proxy.go:309) dials `r.Host` (proxy.go:320) with no inspection. Destination
policy lives entirely in the proxy pod's egress `NetworkPolicy`
(`buildEgressProxyNetworkPolicy`,
[`egressproxy_builder.go:308`](../../cmd/gmc/internal/controller/egressproxy_builder.go)),
which permits 443 only to the GitHub CIDR feed (or, under Q208, the GitHub
FQDNs). Appendix G left a proxy-side allowlist out for two reasons: a single
policy surface (NetworkPolicy) is simpler to reason about, and a byte-forwarding
proxy with no policy code is trivial to audit.

This design keeps that spirit by deriving **both** surfaces from **one** field
(see Approach), so there are not two overlapping policies to keep in sync.

## Approach

Add one field to `EgressProxySpec`
([`api/v2alpha1/egressproxy_types.go`](../../api/v2alpha1/egressproxy_types.go)),
alongside the existing `egressPolicyMode` / `managedNetworkPolicy` / `noProxyCIDRs`:

```go
// DestinationAllowlist lists EXTRA, non-GitHub host suffixes this proxy may
// forward worker CONNECT traffic to (e.g. proxy.golang.org, sum.golang.org).
// GitHub hosts are always allowed and need not be listed. Empty (the default)
// means GitHub-only — identical to today's behavior. Opening egress beyond
// GitHub is an ADMIN decision: it lives on the operator-owned EgressProxy, is
// never tenant-settable, and (for non-GitHub hosts) requires an FQDN
// egressPolicyMode so the pod-level egress can be expressed by hostname.
// +optional
// +kubebuilder:validation:MaxItems=64
DestinationAllowlist []string `json:"destinationAllowlist,omitempty"`
```

Two enforcement surfaces, one source of truth:

1. **Pod-egress layer (the hard gate).** The allowlisted hosts are added to the
   proxy pod's egress policy. Because they are hostnames, this requires an
   **FQDN `egressPolicyMode`** (`CiliumFQDN` / `CalicoFQDN`, already built for
   Q208 — [`egressproxy_fqdn.go`](../../cmd/gmc/internal/controller/egressproxy_fqdn.go)):
   the GMC appends them to the `toFQDNs` / destination-domains set. This is the
   real boundary — if the CNI can't enforce it, egress stays denied (fail-closed,
   same guarantee Q208 already documents).
2. **Proxy CONNECT check (defense-in-depth).** The GMC passes the same list to
   the proxy as an env var (`PROXY_DESTINATION_ALLOWLIST`, read in
   [`cmd/proxy/main.go`](../../cmd/proxy/main.go) into a new `Server` field).
   `handleConnect` parses the CONNECT target host, normalizes it (lowercase,
   strip port), and rejects (`403`) any host that is neither a GitHub host nor a
   configured suffix — before the dial at proxy.320. Optional hardening
   (deferred unless wanted): resolve-and-revalidate the dialed IP to defeat DNS
   rebinding.

Worker traffic already flows through the proxy for proxied tenants — the AGC
provisioner injects `HTTPS_PROXY` into worker containers
([`provisioner.go:1143`](../../cmd/agc/internal/provisioner/provisioner.go)) — so
no worker-side change is needed; this only widens what that proxy will carry.

## Security posture (secure-by-default)

- **Empty default = no change.** `destinationAllowlist: []` is GitHub-only,
  byte-for-byte today's behavior. The feature is purely additive opt-in.
- **Admin-governed, never tenant-openable.** The field is on the operator-owned
  `EgressProxy` (referenced by `proxyRef` / `defaultProxyRef`). A tenant editing
  a RunnerSet cannot open egress. This mirrors Appendix G line 377 ("must stay an
  explicit admin opt-in, never tenant-openable by default") and the existing
  `noProxyCIDRs` "rejected by the GMC admission path" treatment.
- **Attribution + containment preserved.** Traffic still exits the per-tenant
  proxy IPs (attribution intact) and DNS still resolves on the in-cluster path
  (no new exfil channel) — the two things a direct worker-egress hole would
  destroy.
- **Fail-closed for non-GitHub hosts.** Allowing a non-GitHub host without an
  FQDN `egressPolicyMode` is rejected by admission (CEL): in CIDR mode the
  pod-egress layer cannot express a hostname, so we refuse rather than silently
  doing nothing or forcing the operator to hand-maintain Fastly/Google CIDRs.
- **GitHub stays implicit.** GitHub hosts are always allowed; the list only adds
  destinations, so it can never *remove* GitHub access.

## Deliverables (when approved)

1. `api/v2alpha1/egressproxy_types.go` — `DestinationAllowlist` field + CEL
   `XValidation` (non-GitHub entries require an FQDN mode; entries are valid host
   suffixes); regenerate deepcopy + CRD.
2. `cmd/proxy/proxy.go` + `cmd/proxy/main.go` — `Server.DestinationAllowlist`,
   `PROXY_DESTINATION_ALLOWLIST` env wiring, the CONNECT host-suffix check + a
   `actions_gateway_proxy_connect_denied_total` counter; unit tests.
3. `cmd/gmc/internal/controller/egressproxy_builder.go` + `egressproxy_fqdn.go` —
   pass the env to the proxy Deployment; append the hosts to the FQDN egress
   policy; GMC admission rejecting non-GitHub-host-without-FQDN-mode.
4. Docs: [`05-security.md`](../design/05-security.md) (threat-model row +
   move G.1 out of Appendix G), [`network-architecture.md`](../design/network-architecture.md),
   [`security-operations.md`](../operations/security-operations.md) (operator
   how-to), CRD reference; flip the Appendix G.1 entry to "implemented."
5. Tests: unit (host-match allow/deny, GitHub-implicit, normalization) + GMC
   envtest (env propagation, FQDN policy carries the hosts, admission rejects
   CIDR-mode + non-GitHub host).
6. **Dogfood application (closes Q224):** attach an `EgressProxy` with
   `egressPolicyMode: CiliumFQDN` (GKE Dataplane V2 is Cilium) and
   `destinationAllowlist: [proxy.golang.org, sum.golang.org]`, set the gateway's
   `defaultProxyRef`, and confirm `vendor-check` / `tidy-check` go green on
   `gag-ci`.

## Open questions for sign-off

1. **Is opening proxy egress beyond GitHub acceptable as an admin opt-in at all?**
   This is the core trade-off (secure-by-default rule → needs explicit sign-off).
2. **FQDN-mode-only, or also a CIDR-allowlist variant?** Recommendation:
   FQDN-mode-only for non-GitHub hosts (clean, no CIDR rot); reconsider only if a
   non-DNS-aware CNI must be supported.
3. **envtest assets.** `unit-test`/integration may need kube-apiserver/etcd from
   `storage.googleapis.com` / `dl.k8s.io`. Bake the version-pinned binaries into
   the runner image (reproducible, zero egress — recommended) or add those hosts
   to the allowlist? (To confirm by running `unit-test` on the Q239 image.)
4. **DNS-rebinding revalidation** in v1, or defer? (Appendix G lists it optional.)
5. **Scope:** EgressProxy only (v2), matching Q208? v1 / v2-direct-egress out of
   scope.

## Testing

`make check` (gofmt + lint + unit, incl. the new proxy + builder unit tests) +
the GMC envtest integration suite (env propagation, FQDN-policy emission,
admission rejection). Final end-to-end proof on the GKE dogfood per deliverable 6.
