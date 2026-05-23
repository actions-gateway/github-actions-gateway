# Appendix G — Optional Future Enhancements

← [Cost Model](appendix-f-cost-model.md) | [Back to index](README.md)

---

This appendix records features that have been considered, deliberately
left out of the current design, and may be revisited later. Each entry
states the current behavior, the gap it would close, the cost of adding
it, and the trigger that would justify the work.

These are not commitments. An item appearing here means "we know about
it, we chose not to build it for v1, and we know what we'd do if the
constraints changed."

---

## G.1. Proxy-Enforced Destination Allowlist

**Current behavior.** The egress proxy ([§2.3](02-architecture.md#23-tier-3--egress-proxy-pool))
is a pure HTTPS `CONNECT` tunneler. It accepts any `r.Host` and dials
it. Destination policy lives entirely in the `NetworkPolicy` attached
to the proxy pods, which restricts outbound TCP to GitHub IP ranges
plus DNS.

**Why it was left out.** The proxy is intentionally a transport
component, not a policy enforcement point. Two reasons drove that
choice:

1. **Single source of truth.** NetworkPolicy already restricts proxy
   egress to GitHub CIDRs. Adding a host-suffix allowlist in the proxy
   would create two overlapping policy surfaces that have to agree.
2. **Operational simplicity.** A stateless byte-forwarding proxy with
   no policy code is trivial to reason about, version, and replace.
   Every conditional in the data path is a future bug.

**Gap.** Defense-in-depth. NetworkPolicy enforcement is a function of
the cluster's CNI and can be disabled per-tenant via
`spec.proxy.managedNetworkPolicy: false`. If a tenant opts out, or if
the CNI doesn't enforce egress policy, or if GitHub's published IP
ranges expand to cover broader public-cloud blocks, the proxy has no
fallback restriction of its own.

**What "added" would look like.**

- A new optional field on `ProxyConfig`, e.g.
  `destinationAllowlist: [github.com, githubusercontent.com, ghe.io]`.
- The GMC propagates it to the proxy via an env var or config volume.
- The proxy parses the CONNECT target hostname, normalizes it
  (lowercase, strip port), and rejects any host not matching one of
  the configured suffixes.
- Optional refinement: post-DNS check to defeat DNS rebinding —
  resolve the host inside the proxy and re-validate the resolved IP
  against the GitHub CIDRs that NetworkPolicy uses.

Estimated cost: ~150 lines of Go + tests; a new CRD field with CEL
validation; one extra round-trip in the connect path (negligible
relative to upstream RTT).

**What would trigger building it.**

- A tenant or operator asks to disable `managedNetworkPolicy` and the
  platform team wants a guardrail layer that doesn't depend on
  per-tenant NetworkPolicy.
- A CNI in use by the deployment doesn't enforce egress policy
  reliably (some CNIs default to policy-disabled or have known
  enforcement edge cases).
- An incident shows that GitHub's `/meta` IP ranges expanded in a way
  that made the NetworkPolicy effectively too broad to function as a
  policy gate.

Until one of those triggers fires, the operational simplicity of
"transport-only proxy + NetworkPolicy as the single policy surface" is
worth more than the marginal defense-in-depth.

**Related security finding.** [docs/plan/security.md](../plan/security.md)
M-2.

---

## G.2. Proxy-Enforced Per-Tenant Rate Limiting

**Current behavior.** The proxy does not rate-limit. GitHub's own
per-installation rate limits are the only ceiling on call volume.

**Gap.** If a tenant's runners go into a tight loop (misconfigured
workflow, broken retry policy), the only feedback is GitHub's
`429 Too Many Requests` and the AGC's exponential backoff. There is no
way to slow a single misbehaving RunnerGroup before it hits the GitHub
ceiling.

**What "added" would look like.** A token-bucket per-tenant (and
optionally per-RunnerGroup) at the proxy. Configured via a new field
on `ProxyConfig` or `RunnerGroupSpec`. Bucket state stays in-process
on each proxy pod; for accurate global limits a shared backend (Redis,
etcd) would be required — out of scope for v1.

**What would trigger building it.** A real incident where one tenant
saturates the GitHub rate limit and the platform team needs a
sub-tenant kill switch finer than "drain the proxy pool."

---

## G.3. Proxy-Side Audit Logging

**Current behavior.** The proxy emits Prometheus counters
(`actions_gateway_proxy_connections_active`,
`actions_gateway_proxy_connections_total`,
`actions_gateway_proxy_dial_errors_total`) but no per-connection log.

**Gap.** Audit trails for "which tenant talked to which GitHub
endpoint at what time" are reconstructable today only via cluster-wide
flow logs or GitHub's own audit log. Operators investigating an
incident have no per-tenant per-destination view at the proxy layer.

**What "added" would look like.** A structured log line per accepted
CONNECT (tenant namespace, target host, target port, bytes in, bytes
out, duration). Off by default; enabled per-tenant via a
`spec.proxy.auditLogging: true` field. Output to stdout for log
collectors to scrape.

**What would trigger building it.** Compliance ask for per-tenant
egress audit, or a recurring class of incident where the missing log
delays root cause.

---

## G.4. TLS Between AGC/Workers and the Proxy

**Current behavior.** The AGC and worker pods reach the proxy at
`http://actions-gateway-proxy.<ns>.svc.cluster.local:8080`. The
CONNECT-tunneled payload is itself TLS to GitHub; only the CONNECT
request line is in cleartext on the in-cluster hop.

**Gap.** On clusters with eBPF taps, sidecar meshes, or shared-CNI
visibility, the CONNECT target host:port is observable. The actual
payload remains encrypted. No bearer token is sent on the outer hop
today, so credential exposure is not on the table.

**What "added" would look like.** cert-manager-managed cert mounted
into proxy pods, AGC and workers use `https://` proxy URL. Adds one
in-cluster handshake per upstream connection. Adds operational cost of
managing per-tenant proxy certs.

**What would trigger building it.** A compliance requirement for
in-cluster encryption-in-transit beyond what an mTLS service mesh
already provides, or a future change that puts authentication on the
outer hop.

---

## G.5. Per-RunnerGroup Dedicated Proxy Pool

**Current behavior.** One proxy pool per `ActionsGateway` (i.e., per
tenant). All RunnerGroups within a tenant share the pool.

**Gap.** A bandwidth-heavy GPU RunnerGroup can saturate the proxy
pool and slow a co-tenant's CPU-bound RunnerGroup. The HPA scales the
shared pool, but bursts can outrun reaction time.

**What "added" would look like.** An optional
`spec.proxy.dedicated: true` field on `RunnerGroupSpec` that causes
the GMC to provision a separate proxy `Deployment` + `Service` + `HPA`
for that group. The AGC's `HTTP_PROXY` env var would need to become
per-RunnerGroup rather than per-AGC.

**What would trigger building it.** A tenant report showing the
shared proxy pool saturating and the HPA failing to keep up with a
single RunnerGroup's bursts.

---

← [Cost Model](appendix-f-cost-model.md) | [Back to index](README.md)
