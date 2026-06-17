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
- The Gateway Manager Controller (GMC) propagates it to the proxy via an env var or config volume.
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
`429 Too Many Requests` and the Actions Gateway Controller (AGC) exponential backoff. There is no
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

## G.6. X25519 ECDH Session Key Exchange

**Current behavior.** The GitHub Actions broker sends each listener
session an AES-256-CBC key encrypted with the agent's RSA public key
(RSA-OAEP). The agent decrypts it and uses it to decrypt subsequent job
message bodies. This requires the agent key to be RSA. Ed25519 agents
cannot participate — they receive job messages without the AES layer and
rely on TLS as the sole protection for the session payload.

**Why it was left out.** This is a broker-protocol change, not an
AGC-side change. We do not control the GitHub broker. The current code
exposes Ed25519 as an operator opt-in (`--agent-key-type=ed25519`) for
deployments that want faster JWT signing and accept the loss of the AES
defense-in-depth layer, but the default is RSA-3072 specifically because
of this limitation.

**Gap.** Ed25519 is preferable for JWT signing: smaller keys, faster
operations, deterministic signatures, no padding oracle surface. But
making it the secure default requires the broker to support a key-
exchange mechanism compatible with Curve25519 keys. X25519 ECDH
(Diffie-Hellman over Curve25519) would allow the broker to establish a
shared AES session key with an Ed25519 agent without RSA-OAEP: the
broker generates an ephemeral X25519 keypair, the agent uses its
Curve25519 key (derived from the Ed25519 private key via standard
clamping), and both sides derive the same session secret. The AES
message-encryption layer would be fully preserved for Ed25519 agents.

**What "added" would look like.**

This requires a change to the broker protocol, not to this codebase.
On the GitHub side:

- `CreateSession` response includes an ephemeral X25519 public key and
  a nonce instead of (or in addition to) the RSA-OAEP-encrypted blob.
- The client performs X25519 key agreement and derives the AES session
  key via HKDF.

On the AGC side, once the broker supports this:

- `goroutine.go` `createSession` detects the X25519 key-agreement path
  in the `CreateSession` response and performs ECDH derivation instead
  of RSA-OAEP decryption. (~30 LoC using `golang.org/x/crypto/curve25519`.)
- `cmd/agc/main.go` changes the default from `--agent-key-type=rsa` to
  `--agent-key-type=ed25519`.
- RSA key support is retained for backward compatibility with existing
  Secrets.

**What would trigger building it.** GitHub updates the broker protocol
to include X25519 ECDH session key exchange, or publishes an API
allowing Ed25519 agents to participate in encrypted sessions. The probe
binary (`cmd/probe -key-type ed25519`) is the detection mechanism: a
`session OK` result with a non-empty `EncryptionKey` for an Ed25519
agent would indicate the broker is sending a key the agent can use.

**Related finding.** [docs/plan/security.md](../plan/security.md) M-11,
D-5.

---

## G.7. ValidatingAdmissionPolicy for direct RunnerGroup PriorityClass enforcement

**Current behavior.** The platform-owned PriorityClass allowlist (Q132) is
enforced by the GMC validating webhook on the tenant-facing `ActionsGateway` CR:
a `priorityTiers[].priorityClassName` not on `--allowed-priority-classes` is
rejected at admission. RunnerGroup CRs themselves are authored only by the GMC
ServiceAccount (gated to tenant namespaces by the `gmc-tenant-resource-guard`
ValidatingAdmissionPolicy); tenants are not expected to hold direct `runnergroups`
create/update RBAC.

**Gap.** A cluster operator who *does* grant a tenant direct `runnergroups` write
access would bypass the `ActionsGateway` webhook, since that webhook matches only
`actionsgateways`. Such a tenant could create a RunnerGroup naming an
off-allowlist PriorityClass directly, and the AGC would stamp it onto worker pods.

**What "added" would look like.** A `ValidatingAdmissionPolicy` on the
`runnergroups` resource (alongside the existing GMC-SA guards in
`cmd/gmc/config/admission-policy/`) that rejects any `priorityTiers` entry whose
`priorityClassName` is not in an allowlist supplied via a `paramKind` (a ConfigMap
or small custom resource the platform owns — CRD CEL `XValidation` cannot read
external config, which is why the webhook, not a CRD rule, carries the allowlist
today). This applies to *any* creator, including direct tenant writes, closing the
bypass as defense-in-depth.

**What would trigger building it.** A deployment model where tenants are granted
direct RunnerGroup RBAC, or a hardening requirement that the allowlist hold even if
the GMC webhook is bypassed or disabled.

**Related finding.** [docs/plan/q132-priorityclass-allowlist.md](../plan/q132-priorityclass-allowlist.md);
[05-security.md § Cross-Tenant Pod Preemption via PriorityClass](05-security.md#52-agc--proxy-level-threats-namespace-scoped).

---

## G.8. Optional (Disable-able) Egress Proxy

**Current behavior.** The per-tenant egress proxy pool
([§2.3](02-architecture.md#23-tier-3--egress-proxy-pool)) is provisioned
unconditionally for every `ActionsGateway`. The Gateway Manager
Controller (GMC) always emits the proxy `Deployment`, `Service`, `HPA`,
`PodDisruptionBudget`, and TLS cert `Secret`, and the workload
`NetworkPolicy` ([`buildWorkloadNetworkPolicy`](../../cmd/gmc/internal/controller/builder.go))
grants Actions Gateway Controller (AGC) and worker pods egress to
*only* cluster DNS and the proxy on port 8080 — so with no proxy, those
pods have no network path to GitHub at all. `spec.proxy` is `+optional`,
but only its *tuning* (replica counts, resources) is; the pool itself
cannot be turned off. `spec.proxy.managedNetworkPolicy: false` is **not**
a disable switch — it only drops the GitHub-CIDR rule from the proxy's
own NetworkPolicy (for FQDN-based CNI policies); the proxy still runs and
still carries all traffic.

**Why it was left out.** Routing *all* GitHub-bound traffic — AGC
control-plane and worker data-plane alike — through the per-tenant
proxy is what makes the gateway's differentiating claim coherent:
stable per-tenant egress IPs at GitHub. Four downstream capabilities
rest on it (see [docs/plan/worker-egress-proxy.md](../plan/worker-egress-proxy.md)):
GitHub-side IP allowlisting, per-tenant audit attribution, GitHub-side
incident containment (a rate-limit / abuse-flag / IP-ban hits one
tenant, not whoever shares the node), and the one-operation per-tenant
kill-switch (drain the pool). These are network-attribution,
compliance, and operability properties — **not** a security boundary
against a compromised worker, which the installation token already
scopes. Making the proxy optional therefore forfeits attribution and
containment without breaching the token boundary. For the
multi-tenant deployments the design targets, that trade is not worth a
default, so the proxy stays mandatory.

**Gap.** Niche deployments pay for a property they may not need:
cost-sensitive single-tenant or dev clusters that don't require
per-tenant egress IPs, and clusters that already attribute egress at
the node or cloud layer (per-namespace NAT, a cloud NAT gateway per
tenant). For these, the always-on floor of `minReplicas: 2` proxy pods
per tenant is pure overhead.

**What "added" would look like.**

- A new field on `ProxyConfig`, e.g. `mode: Required` (default) `|
  Disabled` (or `enabled: true` default-true).
- When disabled, the GMC skips the proxy `Deployment` / `Service` /
  `HPA` / `PDB` / cert `Secret`, and does not inject `HTTP(S)_PROXY`
  onto the AGC (workers already no-op the proxy-CA mount on an empty
  `ProxyTLSSecretName`).
- `buildWorkloadNetworkPolicy` swaps its `→ proxy:8080` egress rule for
  direct egress to the GitHub CIDRs on 443, reusing the IP-range
  refresh loop that already feeds `buildProxyNetworkPolicy`. DNS stays
  confined to cluster DNS (an independent control — the exfiltration
  side-channel mitigation in [§5.2](05-security.md) is unaffected).
- `updateStatus` drops `ProxyAvailable` from the `Ready` computation
  when the proxy is disabled.
- The validating webhook emits a *warning* (not a rejection) on
  create/update that the attribution / containment / kill-switch
  properties are forfeited.

Estimated cost: an M-sized change across GMC provisioning, the workload
NetworkPolicy, env injection, status, and the webhook, plus CRD docs.

**Secure-by-default constraint.** Disabling the proxy is a security and
isolation regression, so per the project's secure-by-default rule it
must default to `Required`, be reachable only as an explicit opt-in to
the less-secure mode, and carry the forfeited properties prominently in
the operator-facing docs. It must never become the default or a silent
behavior.

**What would trigger building it.** A concrete operator ask for a
single-tenant / dev / cost-sensitive deployment, or a deployment that
already provides per-tenant egress attribution at the node or cloud
layer and wants to shed the proxy's always-on cost.

**Related finding.** [docs/plan/worker-egress-proxy.md](../plan/worker-egress-proxy.md)
(the four properties forfeited);
[02-architecture.md §2.3](02-architecture.md#23-tier-3--egress-proxy-pool).

---

← [Cost Model](appendix-f-cost-model.md) | [Back to index](README.md)
