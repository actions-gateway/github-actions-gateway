# Q242 — G.1 Proxy-Enforced Destination Allowlist (worker dependency egress)

> **Status: DESIGN — awaiting sign-off. No implementation has landed.** This
> promotes [Appendix G.1](../design/appendix-g-future-enhancements.md#g1-proxy-enforced-destination-allowlist)
> (tracked under the non-committed Q19 bundle) to committed work, because it is
> the attribution-preserving answer to the single most common operator ask:
> letting CI jobs reach their build dependencies (package registries, module
> proxies, test-asset hosts) without forfeiting the per-tenant egress model.

## Goal

Let an operator allow a **per-tenant egress proxy** to forward worker traffic to
a small, explicit set of **non-GitHub** destinations — by **DNS host suffix**
(e.g. `proxy.golang.org`) and by **CIDR** (an internal subnet with no DNS, or a
cloud private-API range) — so jobs that fetch dependencies work, while keeping
every property the GitHub-only egress posture provides: per-tenant egress-IP
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

**Two destination forms** — host suffixes (DNS names) **and** CIDR blocks — because
real CI dependencies need both: package/module hosts resolve by DNS
(`proxy.golang.org`), but internal services are often reachable only by **IP with
no internal DNS**, and cloud private API endpoints resolve into known **CIDR
ranges** on a private network (e.g. GCP Private Google Access →
`restricted.googleapis.com` lands in `199.36.153.8/30`). An FQDN-only allowlist
would force operators to invent DNS or give up; a CIDR-only one can't express a
moving package host. So:

```go
// DestinationFQDNs lists EXTRA, non-GitHub DNS host suffixes this proxy may
// forward worker CONNECT traffic to (e.g. proxy.golang.org). Host-suffix entries
// REQUIRE an FQDN egressPolicyMode, since the pod-egress layer expresses them as
// toFQDNs rules. +optional +kubebuilder:validation:MaxItems=64
DestinationFQDNs []string `json:"destinationFQDNs,omitempty"`

// DestinationCIDRs lists EXTRA, non-GitHub IP ranges this proxy may forward to
// (e.g. an internal 10.x subnet with no DNS, or a cloud private-API CIDR like
// 199.36.153.8/30). CIDR entries work in ANY egressPolicyMode — they become
// ipBlock egress peers (CIDR mode) or toCIDR peers (FQDN mode). +optional
// +kubebuilder:validation:MaxItems=64
DestinationCIDRs []string `json:"destinationCIDRs,omitempty"`
```

Both default empty (GitHub-only — today's behavior). GitHub is always allowed;
the lists only add.

**Who governs the destinations — the critical point.** The `EgressProxy` is a
**namespace-scoped, tenant-authorable** CR (only the cluster-scoped
`ClusterRunnerTemplate` is platform-only — appendix-h §H). So these fields cannot
be trusted as "admin-only" just because they sit on the `EgressProxy`: a tenant
with write access to their namespace could otherwise open their own egress to
anywhere, which the egress sign-off (Q168) and Appendix G forbid ("never
tenant-openable by default"; "no mode in which a worker can reach arbitrary
internet"). The fields are therefore **gated by a platform-owned allowlist**,
exactly mirroring the `--allowed-priority-classes` model (Q132/Q188) for the
other security-relevant tenant-settable field:

- **Two allowlist surfaces, mirroring the two CR fields** — because the two forms
  match differently and a single flat list would have to type-detect each entry:
  - `--allowed-egress-fqdns` — permitted FQDN suffixes. A request matches if it is
    equal to, or a subdomain of, a permitted entry (so allowing `golang.org`
    permits `proxy.golang.org`); `destinationFQDNs` is validated against this.
  - `--allowed-egress-cidrs` — permitted IP ranges. A request matches by
    **subnet containment** (allowing `10.0.0.0/8` permits a requested
    `10.1.0.0/16`); `destinationCIDRs` is validated against this.
  Both may also be sourced from a watched ConfigMap (keys `fqdns:` / `cidrs:`),
  additive + fail-safe, exactly like `--priority-class-allowlist-configmap`.
- The GMC validating webhook **rejects** any `destinationFQDNs` entry not covered
  by `--allowed-egress-fqdns`, and any `destinationCIDRs` entry not contained in
  `--allowed-egress-cidrs`.
- **Both empty forbids every non-GitHub destination** (secure default) — out of
  the box no tenant can open egress at all, identical to how an empty
  `--allowed-priority-classes` forbids every `priorityClassName`.

  *(Alternative considered: one flat `--allowed-egress-destinations` that
  parse-detects CIDR-vs-FQDN per entry, as the `noProxyCIDRs` field already does.
  Rejected for the CRD-symmetry + unambiguous-semantics reasons above, but it is a
  viable simpler variant if a single flag is preferred — a sub-decision for review.)*

A tenant can thus *request* a destination on their `EgressProxy` for GitOps
ergonomics, but only the platform decides what is actually permitted; a request
outside the allowlist fails admission.

Two enforcement surfaces, one source of truth:

1. **Pod-egress layer (the hard gate).** The GMC adds the destinations to the
   proxy pod's egress policy:
   - **Host suffixes** → `toFQDNs` / destination-domains, which **requires** a
     CNI-native FQDN `egressPolicyMode` (`CiliumFQDN` / `CalicoFQDN`, already built
     for Q208 — [`egressproxy_fqdn.go`](../../cmd/gmc/internal/controller/egressproxy_fqdn.go)).
   - **CIDRs** → native `ipBlock` peers on the standard NetworkPolicy
     ([`buildEgressProxyNetworkPolicy`](../../cmd/gmc/internal/controller/egressproxy_builder.go),
     same shape as the GitHub-CIDR rule) or Cilium `toCIDR` in FQDN mode — so CIDRs
     work in **every** mode, no DNS-aware CNI needed.
   If the CNI can't enforce, egress stays denied (fail-closed, same Q208 guarantee).
2. **Proxy CONNECT check (defense-in-depth).** The GMC passes both lists to the
   proxy via env (read in [`cmd/proxy/main.go`](../../cmd/proxy/main.go) into new
   `Server` fields). `handleConnect` (proxy.go:309), before the dial at proxy.320:
   - matches the CONNECT host (normalized, port-stripped) against the GitHub set +
     `DestinationFQDNs` suffixes; and
   - for `DestinationCIDRs`, **resolves the host and checks the resolved IP is in
     range** (a literal-IP target is checked directly). Dialing that resolved IP
     (rather than re-resolving by name) also closes the DNS-rebinding window — so
     the resolve-and-revalidate hardening is *built in* for the CIDR path, not a
     deferred extra. Anything matching neither is rejected `403`.

Worker traffic already flows through the proxy for proxied tenants — the AGC
provisioner injects `HTTPS_PROXY` into worker containers
([`provisioner.go:1143`](../../cmd/agc/internal/provisioner/provisioner.go)) — so
no worker-side change is needed; this only widens what that proxy will carry.

## Security posture (secure-by-default)

- **Empty default = no change.** `destinationFQDNs: []` is GitHub-only,
  byte-for-byte today's behavior. The feature is purely additive opt-in.
- **Platform-governed, not tenant-openable — via an allowlist, not ownership.**
  The `EgressProxy` is tenant-authorable, so governance comes from the GMC
  `--allowed-egress-fqdns` / `--allowed-egress-cidrs` allowlists +
  validating-webhook rejection (see above), not from the CR's ownership. Both
  empty = deny-all-non-GitHub.
  This is the same platform-ownership model the design already uses for the only
  other security-relevant tenant-settable field — `priorityClassName` /
  `--allowed-priority-classes` (Q132/Q188) — and satisfies Appendix G's "never
  tenant-openable by default" without pretending the CR is admin-only.
- **Attribution + containment preserved.** Traffic still exits the per-tenant
  proxy IPs (attribution intact) and DNS still resolves on the in-cluster path
  (no new exfil channel) — the two things a direct worker-egress hole would
  destroy.
- **Fail-closed mode coupling.** A host-suffix entry without an FQDN
  `egressPolicyMode` is rejected by admission (CEL): the standard NetworkPolicy
  can't express a hostname, so we refuse rather than silently no-op. CIDR entries
  carry no such constraint — they're native `ipBlock`/`toCIDR` peers in any mode.
- **CIDRs are precise, not rot-prone.** Operators allowlist *internal* or
  *cloud-private* ranges (a `10.x` subnet, a Private-Google-Access block) that are
  stable and operator-owned — unlike trying to hand-maintain a public CDN's
  (Fastly/Google) shifting CIDRs, which is why the *public* package hosts use the
  FQDN form instead.
- **GitHub stays implicit.** GitHub is always allowed; the lists only add
  destinations, so they can never *remove* GitHub access.

## Deliverables (when approved)

1. `api/v2alpha1/egressproxy_types.go` — `DestinationFQDNs` (host suffixes) +
   `DestinationCIDRs` (IP ranges) fields + CEL `XValidation` (host-suffix entries
   require an FQDN mode; entries are valid host suffixes / CIDRs respectively);
   regenerate deepcopy + CRD.
2. `cmd/proxy/proxy.go` + `cmd/proxy/main.go` — `Server.DestinationFQDNs` +
   `Server.DestinationCIDRs`, env wiring, the CONNECT check (host-suffix match +
   resolve-and-check-IP-in-CIDR, dialing the validated IP) + a
   `actions_gateway_proxy_connect_denied_total` counter; unit tests.
3. `cmd/gmc/internal/controller/egressproxy_builder.go` + `egressproxy_fqdn.go` —
   pass both lists to the proxy Deployment; append host suffixes to the FQDN
   policy and CIDRs as `ipBlock`/`toCIDR` peers.
4. **Platform allowlist (the governance gate).** GMC `--allowed-egress-fqdns` +
   `--allowed-egress-cidrs` flags + optional watched ConfigMap (keys `fqdns:`/
   `cidrs:`; additive, fail-safe — mirror `--priority-class-allowlist-configmap`);
   GMC validating webhook rejects any `destinationFQDNs` not covered by the FQDN
   allowlist (suffix match), any `destinationCIDRs` not contained in the CIDR
   allowlist (subnet containment), and any host-suffix entry without an FQDN mode.
   Both empty = deny-all-non-GitHub.
5. Docs: [`05-security.md`](../design/05-security.md) (threat-model row +
   move G.1 out of Appendix G), [`network-architecture.md`](../design/network-architecture.md),
   [`security-operations.md`](../operations/security-operations.md) (operator
   how-to, incl. the allowlist flag/ConfigMap), CRD reference; flip the Appendix
   G.1 entry to "implemented."
6. Tests: unit (host-suffix + CIDR allow/deny, resolve-check, GitHub-implicit,
   normalization) + GMC envtest (env propagation, FQDN policy carries the hosts,
   NetworkPolicy carries the CIDRs, admission **rejects an off-allowlist entry**
   and a host-suffix-without-FQDN-mode, empty allowlist denies all non-GitHub).
7. **Dogfood application (closes Q224):** set the GMC
   `--allowed-egress-fqdns` to `proxy.golang.org,sum.golang.org`, attach an
   `EgressProxy` with `egressPolicyMode: CiliumFQDN` (GKE Dataplane V2 is Cilium)
   and `destinationFQDNs: [proxy.golang.org, sum.golang.org]`, set the gateway's
   `defaultProxyRef`, and confirm `vendor-check` / `tidy-check` go green on
   `gag-ci`.

## Open questions for sign-off

1. **Approve the platform-allowlist governance model** (GMC
   `--allowed-egress-fqdns` + `--allowed-egress-cidrs`, both empty =
   deny-all-non-GitHub, tenant requests gated by admission — the
   `--allowed-priority-classes` pattern)? This is the core trade-off and the
   secure-by-default question; the rest follows from it. **Sub-decision:** two
   typed flags (recommended, CRD-symmetric) vs one flat parse-detecting flag.
2. ~~FQDN-only vs CIDR variant~~ **Resolved: support both** (host-suffix +
   `destinationCIDRs`), per review — internal IP-only hosts and cloud private-API
   CIDRs need the IP form; public package hosts need the FQDN form.
3. **envtest assets.** `unit-test`/integration may need kube-apiserver/etcd from
   `storage.googleapis.com` / `dl.k8s.io`. Bake the version-pinned binaries into
   the runner image (reproducible, zero egress — recommended) or add those hosts
   to the allowlist? (To confirm by running `unit-test` on the Q239 image.)
4. ~~DNS-rebinding revalidation in v1 or defer?~~ **Resolved: in v1** — it falls
   out of the `destinationCIDRs` resolve-and-dial-the-validated-IP path.
5. **Scope:** EgressProxy only (v2), matching Q208? v1 / v2-direct-egress out of
   scope.

## Testing

`make check` (gofmt + lint + unit, incl. the new proxy + builder unit tests) +
the GMC envtest integration suite (env propagation, FQDN-policy emission,
admission rejection). Final end-to-end proof on the GKE dogfood per deliverable 6.
