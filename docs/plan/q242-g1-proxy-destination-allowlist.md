# Q242 — G.1 Proxy-Enforced Destination Allowlist (worker dependency egress)

> **Status: APPROVED — planned as v2beta1 blocker [Q242](../STATUS.md). No
> implementation has landed yet.** This promotes [Appendix G.1](../design/appendix-g-future-enhancements.md#g1-proxy-enforced-destination-allowlist)
> (tracked under the non-committed Q19 bundle) to committed work, because it is
> the attribution-preserving answer to the single most common operator ask:
> letting CI jobs reach their build dependencies (package registries, module
> proxies, test-asset hosts) without forfeiting the per-tenant egress model. The
> core go/no-go is **decided (accept)** with the [Trade-offs](#trade-offs-why-this-needs-sign-off)
> recorded, a [bounded scope](#scope-we-accept--and-the-scope-creep-we-decline),
> and the [in-cluster mirror recommended first](#recommended-path-in-cluster-caching-mirror-first);
> what remains is review of the design details (the flag-shape sub-decision and
> the envtest-assets question).

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
`private.googleapis.com` is `199.36.153.8/30`, `restricted.googleapis.com`
`199.36.153.4/30`). An FQDN-only allowlist would force operators to invent DNS or
give up; a CIDR-only one can't express a moving package host. So:

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
  Both may also be sourced from a **single** watched ConfigMap with two keys
  (`fqdns:` / `cidrs:`) — named by one `--egress-destination-allowlist-configmap`
  flag, one object / one watch / one RBAC grant, **not** two ConfigMaps —
  additive + fail-safe, exactly like the single `--priority-class-allowlist-configmap`.
- The GMC validating webhook **rejects** any `destinationFQDNs` entry not covered
  by `--allowed-egress-fqdns`, and any `destinationCIDRs` entry not contained in
  `--allowed-egress-cidrs`.
- **Both empty forbids every non-GitHub destination** (secure default) — out of
  the box no tenant can open egress at all, identical to how an empty
  `--allowed-priority-classes` forbids every `priorityClassName`.

  *(Alternative rejected: one flat `--allowed-egress-destinations` that
  parse-detects CIDR-vs-FQDN per entry, as the `noProxyCIDRs` field already does.
  Simpler — one flag — but it loses CRD symmetry and conflates two different match
  rules; we go with the two typed flags above.)*

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
2. **Proxy CONNECT check (defense-in-depth).** The GMC passes the proxy the
   **full** permitted set via two env vars — `PROXY_ALLOWED_HOST_SUFFIXES` and
   `PROXY_ALLOWED_CIDRS` (read in [`cmd/proxy/main.go`](../../cmd/proxy/main.go)
   into `Server.AllowedHostSuffixes` / `Server.AllowedCIDRs`). The full set is the
   implicit GitHub hostnames **plus** the operator's `destinationFQDNs`
   (host-suffix env) and `destinationCIDRs` (CIDR env); GitHub is carried as host
   suffixes regardless of `egressPolicyMode` since workers always reach it by name,
   and the bulky GitHub CIDR feed is *not* injected. The GMC injects this env **only
   when the EgressProxy lists at least one extra destination** — with none, both env
   vars are absent and the proxy stays transport-only (byte-for-byte today's
   behavior; the NetworkPolicy is the sole gate). `checkDestination` (proxy.go),
   before the dial in `handleConnect`:
   - with no allowlist env, permits every destination (transport-only);
   - otherwise matches the CONNECT host (normalized, port-stripped) against the
     allowed host suffixes (GitHub + `destinationFQDNs`); and
   - for the allowed CIDRs (`destinationCIDRs`), **resolves the host and checks the
     resolved IP is in range** (a literal-IP target is checked directly), then dials
     **that validated IP** rather than re-resolving by name — closing the
     DNS-rebinding window, so resolve-and-revalidate is *built in* for the CIDR path.
     Anything matching neither is rejected `403` and counted on
     `actions_gateway_proxy_connect_denied_total`.

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
- **Allowlisting opens the *policy*, not the *route*.** A `destinationCIDRs` entry
  lifts the egress-policy block; it does not make the destination reachable.
  The Private-Google-Access VIPs (`199.36.153.x/30`) in particular are **not
  reachable by default** — they require subnet-level Private Google Access enabled
  plus Cloud DNS + a route, and they exist for nodes *without* external IPs;
  GKE nodes that have an external IP reach Google APIs over public IPs instead.
  So the operator's cluster networking (PGA/DNS/routes) is a prerequisite G.1 does
  not configure. (Not relevant to the dogfood: its nodes have external IPs and it
  allowlists only `proxy.golang.org`/`sum.golang.org`, no Google-API access.)

## Trade-offs (why this needs sign-off)

Accepting this feature is accepting a **deliberate, bounded relaxation** of the
GitHub-only egress posture. It is worth it because the alternative is forcing
every operator who needs a dependency off-platform or off-isolation — but the
costs are real and permanent, so they are recorded here.

**What we gain**

- Solves the single most common operator ask (worker dependency egress) while
  keeping per-tenant egress-IP **attribution** and DNS-exfil **containment** —
  the properties the cheap alternatives (punt to GitHub-hosted, widen worker
  CIDRs, `managedNetworkPolicy: false`) each throw away.
- Secure-by-default: empty allowlist = today's behavior, byte-for-byte.
- Reuses the already-signed-off `--allowed-priority-classes` governance shape — no
  new paradigm.
- Defense-in-depth (proxy CONNECT check + CNI/NetworkPolicy from one field; the
  CIDR path folds in DNS-rebinding revalidation).

**What we pay**

- **A real hole, however narrow.** A compromised worker (malicious dependency,
  job RCE) can now exfiltrate to any allowlisted destination. The allowlist
  bounds it; it does not eliminate it. This is the core trade.
- **The proxy is no longer purely transport-only.** Appendix G kept it
  byte-forwarding on purpose ("every conditional in the data path is a future
  bug"). We add host parsing, allowlist matching, and a **DNS resolve per CONNECT**
  for CIDR entries — more code, more latency, more CVE surface in a
  security-critical hot path.
- **DNS-based allowlisting is inherently leaky.** `toFQDNs` depends on DNS
  interception + CNI enforcement, a resolve/connect TOCTOU window, and blurs on
  CDN/shared-IP hosts. It looks more precise than it is.
- **A new admin footgun.** A too-broad entry (`*.googleapis.com`, a wide CIDR, a
  CDN that hosts arbitrary content) silently reopens broad egress. The guardrail
  is only as good as the entries.
- **Maintenance cost** of the proxy data-path code and the webhook, for an
  opt-in feature, on an adoption-only project's finite maintainer budget.

## Recommended path: in-cluster caching mirror first

We do **not** force operators onto an in-cluster mirror — G.1 exists so they
don't have to — but the docs must **lead with the mirror as the recommended,
more-secure, lower-maintenance option** for *remote third-party dependencies*
(Go modules, npm, PyPI, crates, container layers), and present the destination
allowlist as the escape hatch for what a mirror can't cover. The reasoning,
which the operator guide must state plainly:

- **More secure.** Workers egress only to a stable **in-cluster** pod; the
  worker NetworkPolicy never has to name an external destination at all. The
  mirror — not every worker — holds the (narrow, auditable) outbound path, and it
  can be pre-populated and run air-gapped.
- **Lower-maintenance / less breakage.** Remote dependency IPs and even hostnames
  churn; a CIDR/FQDN allowlist of *public* hosts rots and breaks builds when a
  registry shifts ranges. A mirror's address is stable forever, so the egress
  policy never changes when upstreams move.
- **Better behavior.** Caching cuts repeat fetches, survives upstream outages, and
  gives a single audit point.

So the allowlist is **best reserved for what a mirror genuinely cannot proxy**:
a *specific* live cloud-provider API (a single host like `kms.<region>.amazonaws.com`,
or a Private-Google-Access CIDR like `199.36.153.8/30` — **never** a wildcard like
`*.googleapis.com`, which would cover `storage.googleapis.com/<any-bucket>` and
reopen broad exfil; and **not** the metadata/IMDS endpoint, which workers should
stay blocked from), internal services reachable only by IP, and one-off stable
endpoints. Public package
ecosystems should be steered to a mirror (Athens for Go, Verdaccio for npm, a
registry pull-through for images) — and the dogfood itself should ultimately
demonstrate *both* (the allowlist for the immediate fix, a Go module proxy as the
"do it the durable way" follow-up). This guidance is a **required** part of the
docs deliverable, not a footnote.

## Scope we accept — and the scope-creep we decline

Accepting G.1 is a **bounded** commitment. To keep the proxy auditable and the
maintenance load finite, the following are explicitly **in scope** now, and the
rest are **declined by default** (each with the trigger that would reopen it).

**In scope (this work):** the two typed destination fields; the platform
allowlist (two flags + one ConfigMap); admission gating; FQDN-suffix + CIDR-
containment matching; the CONNECT check with CIDR resolve-and-revalidate; a
`*_connect_denied_total` counter; the mirror-first operator guidance.

**Declined by default** (do *not* build as part of accepting G.1; revisit only on
the stated trigger):

| Tempting extension | Why it's declined | Trigger to revisit |
|---|---|---|
| Per-`RunnerSet` (tenant-self-service) destinations | Re-opens the tenant-openable hole the allowlist exists to close; governance must stay platform-level | A concrete multi-team ask *and* a per-set admission story |
| Wildcard / regex destinations beyond suffix + CIDR | Powerful footgun; `*.evil-cdn.com`-class mistakes; harder to audit | A real destination set that suffix+CIDR genuinely can't express |
| Per-destination **rate limiting / quotas** | Belongs at a mirror or a mesh, not the transport proxy; adds state to a stateless hot path | Abuse observed that the mirror can't absorb |
| HTTP **path/method-level** rules | The proxy is CONNECT-only (no TLS termination); inspecting paths means MITM — a non-goal | Never, without a separate explicit TLS-terminating design |
| Upstream **auth / credential injection** | Turns the proxy into a secrets handler; large new attack surface | A separate design with its own threat model |
| **Audit log** of every *allowed* connection | The denied-counter covers the security signal; full connection logging is a SIEM/mesh concern | An operator compliance requirement |

The principle: G.1 adds **destination allowlisting and nothing else** to the
proxy. Anything that wants inspection, mutation, statefulness, or tenant-level
egress authority is out of band and routes to a mirror, a mesh, or its own design.

## Deliverables (when approved)

1. `api/v2alpha1/egressproxy_types.go` — `DestinationFQDNs` (host suffixes) +
   `DestinationCIDRs` (IP ranges) fields + CEL `XValidation` (host-suffix entries
   require an FQDN mode; entries are valid host suffixes / CIDRs respectively);
   regenerate deepcopy + CRD.
2. `cmd/proxy/proxy.go` + `cmd/proxy/main.go` — `Server.AllowedHostSuffixes` +
   `Server.AllowedCIDRs`, fed from the `PROXY_ALLOWED_HOST_SUFFIXES` /
   `PROXY_ALLOWED_CIDRS` env vars. These hold the **full** GMC-injected allowed set
   (GitHub hosts + the operator's extras), not just the extras; **both empty ⇒
   transport-only** (any CONNECT tunneled, NetworkPolicy is the gate). The CONNECT
   check (`checkDestination`: host-suffix match + resolve-and-check-IP-in-CIDR,
   dialing the validated IP) + a `actions_gateway_proxy_connect_denied_total`
   counter; unit tests. **(Merged: #461.)**
3. `cmd/gmc/internal/controller/egressproxy_builder.go` + `egressproxy_fqdn.go` —
   inject `PROXY_ALLOWED_HOST_SUFFIXES` (GitHub hostnames + `destinationFQDNs`) and
   `PROXY_ALLOWED_CIDRS` (`destinationCIDRs`) into the proxy Deployment **only when
   the EgressProxy lists ≥1 extra destination** (else no env ⇒ transport-only,
   backward-compatible); append `destinationFQDNs` to the FQDN policy's
   `toFQDNs`/domains and `destinationCIDRs` as `ipBlock` peers on the standard
   NetworkPolicy (applied in every mode, so CIDRs need no DNS-aware CNI). The bulky
   GitHub CIDR feed is not injected into the proxy env — GitHub is reachable by
   hostname via the suffix list. **(Merged: #460 for the CRD fields; this PR for the
   GMC plumbing.)**
4. **Platform allowlist (the governance gate).** GMC `--allowed-egress-fqdns` +
   `--allowed-egress-cidrs` flags + one optional watched ConfigMap with two keys
   (`fqdns:`/`cidrs:`), named by `--egress-destination-allowlist-configmap`
   (additive, fail-safe — mirror `--priority-class-allowlist-configmap`);
   GMC validating webhook rejects any `destinationFQDNs` not covered by the FQDN
   allowlist (suffix match), any `destinationCIDRs` not contained in the CIDR
   allowlist (subnet containment). The host-suffix-without-FQDN-mode rejection is
   handled by the CRD CEL rule (#460). Both empty = deny-all-non-GitHub.
   **(Done:** `allowlist.EgressDestinationAllowlist` + `EgressDestinationAllowlistReconciler`
   + `EgressProxyCustomValidator`; chart flags/ConfigMap/Role wiring; envtest admission
   tests. Operator how-to in `security-operations.md` shipped with this deliverable.**)**
5. Docs: [`05-security.md`](../design/05-security.md) (threat-model row + the
   trade-offs above + move G.1 out of Appendix G),
   [`network-architecture.md`](../design/network-architecture.md),
   [`security-operations.md`](../operations/security-operations.md) (operator
   how-to for the allowlist flags/ConfigMap **that leads with the in-cluster-mirror
   recommendation** and presents the allowlist as the escape hatch — required, not
   a footnote; include per-ecosystem mirror pointers: Athens/Verdaccio/registry
   pull-through), CRD reference; flip the Appendix G.1 entry to "implemented."
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

1. ~~Approve the feature / platform-allowlist governance model at all?~~
   **Decided: accept**, with the Trade-offs recorded above, the bounded scope
   below, and the in-cluster-mirror recommended first. We will not force operators
   onto a mirror, but the docs steer them there for remote third-party deps and
   reserve the allowlist for what a mirror can't proxy. Governance is the
   `--allowed-priority-classes` shape (two flags + one ConfigMap, both empty =
   deny-all-non-GitHub, admission-gated). **Sub-decision resolved: two typed flags**
   (`--allowed-egress-fqdns` + `--allowed-egress-cidrs`) — CRD-symmetric and
   unambiguous, over the one-flat-flag variant.
2. ~~FQDN-only vs CIDR variant~~ **Resolved: support both** (host-suffix +
   `destinationCIDRs`), per review — internal IP-only hosts and cloud private-API
   CIDRs need the IP form; public package hosts need the FQDN form.
3. ~~envtest assets — does `unit-test` need an egress hole for them?~~ **Resolved:
   no, not for the four offline jobs.** `cmd/agc/Makefile` is explicit —
   `test: ## Run unit tests (no envtest required)`; only `test-integration` uses
   envtest, via `setup-envtest use` (which *downloads* the kube-apiserver/etcd
   binaries — the `setup-envtest` *tool* is vendored, the binaries are not). They
   can't be "vendored as source": they're prebuilt binaries of external projects
   (`k8s.io/kubernetes`' apiserver isn't buildable as a library dep; etcd is its
   own binary), and the suites *deliberately* use a real apiserver — a vendored
   fake client can't reproduce the defaulting/no-op-dedup/CEL/`IsConflict`
   semantics the integration tier exists to test. So envtest is irrelevant to
   lint/shellcheck/unit-test/coverage. **When the heavier
   `integration-test` job is later migrated**, bake the version-pinned binaries
   into the runner image at build time (run `setup-envtest use` during
   `docker build`, set `KUBEBUILDER_ASSETS`) — reproducible, zero runtime egress —
   rather than allowlisting the download host. **Caveat:** envtest locates the
   binaries via the `KUBEBUILDER_ASSETS` *directory* env var, not `$PATH`, and
   `make test-integration` recomputes it by running `setup-envtest use` against a
   repo-relative `--bin-dir` that is empty on a fresh checkout — so a plain `COPY`
   into the image isn't enough; the integration target must be pointed at the
   baked dir (pre-set `KUBEBUILDER_ASSETS`, or seed `setup-envtest`'s store /
   `--bin-dir` there) or it re-downloads. Tracked with that migration, not this
   feature.
4. ~~DNS-rebinding revalidation in v1 or defer?~~ **Resolved: in v1** — it falls
   out of the `destinationCIDRs` resolve-and-dial-the-validated-IP path.
5. **Scope:** EgressProxy only (v2), matching Q208? v1 / v2-direct-egress out of
   scope.

## Testing

`make check` (gofmt + lint + unit, incl. the new proxy + builder unit tests) +
the GMC envtest integration suite (env propagation, FQDN-policy emission,
admission rejection). Final end-to-end proof on the GKE dogfood per deliverable 6.
