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

## G.9. NetworkPolicy Egress Extensibility (signers, telemetry, job dependencies)

**Current behavior.** The GMC reconciles three default-deny per-tenant
NetworkPolicies (§5.2, [`builder.go`](../../cmd/gmc/internal/controller/builder.go)).
The **AGC** policy permits egress to cluster DNS + the kube API server
(+ the GitHub CIDR allowlist in direct mode), and — once the Vault egress
work landed — a *scoped* AGC→Vault rule when a workload-identity gateway
declares `signer.vault.networkPolicy` (a pod/namespace selector or a CIDR;
the rule targets only that peer, on the Vault API port from the signer
address, and only the AGC pod selects into it). The **workload** policy
permits worker pods egress to cluster DNS + the proxy on 8080 (+ the GitHub
CIDRs in direct mode) and nothing else. The link-local block
`169.254.0.0/16` is permitted on **port 53 only** (NodeLocal DNSCache), not
for anything else.

The Vault work established the reusable pattern for opening a hole in the
default-deny envelope: a peer the **tenant names in its own CR**, that the
**GMC scopes** to a single peer + port, attached to the **AGC policy only**
so untrusted worker pods never inherit it. That peer descriptor is now the
shared [`EgressPeer`](../../api/v2alpha1/actionsgateway_types.go) type
(selector | CIDR + optional explicit port), extracted from the Vault-specific
shape before the v2 freeze so future consumers reuse one descriptor (Q204; see
the pre-v2 analysis below). The entries below are the other egress holes we can
foresee wanting, the governance each needs, and — the load-bearing question —
confirmation that none of them forces a breaking API change before v2.

**Gap — the egress holes we can foresee.**

| Use case | Who needs egress | Governance | Notes |
|---|---|---|---|
| **Cloud KMS signers** (AWS/GCP/Azure KMS as additive `signer.provider` members) | AGC | Tenant-delegated for the signer endpoint (tenant names its own KMS); **admin-governed** for any cloud-metadata leg | The metadata/credential path varies by provider: **GKE Workload Identity** and **EC2 IMDS / EKS Pod Identity** reach a link-local address (`169.254.169.254` / `169.254.170.23`) on :80 — a *new* hole distinct from the existing :53-only `169.254/16` rule; **AWS IRSA** and **Azure Workload Identity** instead call a *public* STS/AAD host (the same opaque-URL problem the Vault rule already solves). |
| **AGC telemetry** (`spec.tracing.endpoint`, and any future metrics/log push) | AGC | Tenant-delegated, AGC-only | A scoped rule is derivable from the *already-existing* `tracing.endpoint`; today that endpoint is not auto-permitted on a policy-enforcing CNI. |
| **Worker job dependencies** (container/package registries, build caches, internal services the CI job legitimately calls) | Worker | **Through the proxy → tenant-delegated** (attribution preserved); **direct → admin-governed** | The single most common operator ask. The attribution-preserving answer is the proxy destination allowlist ([G.1](#g1-proxy-enforced-destination-allowlist)); a *direct* worker egress hole forfeits per-tenant IP attribution and the DNS-exfil containment, so it must stay an explicit admin opt-in, never tenant-openable by default. |
| **Cloud metadata for worker OIDC** (`configure-aws-credentials` etc.) | Worker | **Admin-governed, default-deny** | Metadata-server reach from untrusted job code is a classic credential-theft / SSRF vector; the common OIDC path uses GitHub's token → public STS, which needs no metadata at all. |

**The governance principle (the durable decision).** A tenant may
self-declare egress, in its own CR, **only** to peers it names and controls
that are reached **either** by its AGC control plane **or** through the
attribution-preserving proxy. Egress that originates **directly from worker
pods** (bypassing the proxy) or targets a **shared / sensitive surface**
(cloud metadata, cluster infrastructure, arbitrary internet) stays
**platform-admin-governed** — a GMC flag or cluster policy, never a
tenant-settable default. The rationale is the trust split the whole design
rests on: worker pods run untrusted GitHub Actions job code, the AGC is
trusted control plane, and the proxy is the per-tenant attribution boundary.
The Vault `signer.vault.networkPolicy` is the canonical tenant-delegated case
(AGC-only, tenant-named, GMC-scoped); the worker-direct and metadata cases
are the canonical admin-governed ones.

**Pre-v2 API-compatibility analysis (no breaking change required).** Every
hole above is reachable additively under the frozen v2 shape:

- Cloud KMS signers join as **new `signer.provider` union members**, each
  carrying its *own* optional peer descriptor — exactly the additive
  contract the union was built for; existing members are untouched.
- AGC telemetry and worker-direct egress would be **new optional fields**
  on existing structs (`ActionsGateway` / `RunnerSet` / `RunnerTemplate`)
  or new GMC flags. Optional CRD fields don't prune and old clients ignore
  them, so adding them later is non-breaking.
- The worker-through-proxy case extends the proxy allowlist
  ([G.1](#g1-proxy-enforced-destination-allowlist)), not the CRD egress shape.

The **one** pre-v2 hygiene decision worth settling deliberately was: the Vault
work shipped a per-feature peer descriptor (`VaultNetworkPolicy`: selector |
CIDR, port derived from the address). If KMS, telemetry, and worker-egress had
each grown their own near-duplicate descriptor, v2 would freeze three
inconsistent shapes. **Resolved (Q204):** the descriptor is now the shared
[`EgressPeer`](../../api/v2alpha1/actionsgateway_types.go) type (selector | CIDR
+ an optional explicit port), and `signer.vault.networkPolicy` references it.
The extraction is serialization-compatible — the field keys (`podSelector`,
`namespaceSelector`, `cidr`) are unchanged, so the v2alpha1 shape already
shipped is preserved; only the additive optional `port` is new, with the Vault
builder still deriving the port from the address when `port` is unset. Future
consumers (KMS, telemetry) now reference the same type instead of forking it —
the shape that is far cheaper to settle before the v2alpha1 → v2beta1
graduation ([Q74](../STATUS.md)) than after.

**What would trigger building any of it.** A concrete signer provider beyond
Vault (KMS), a policy-CNI operator who needs their tracing endpoint or a job
dependency reachable, or the graduation review deciding to consolidate the
peer descriptor.

**Related.** [05-security.md](05-security.md) (the per-tenant NetworkPolicy
isolation model); [G.1 Proxy-Enforced Destination Allowlist](#g1-proxy-enforced-destination-allowlist);
[G.8 Optional Egress Proxy](#g8-optional-disable-able-egress-proxy);
[`builder.go`](../../cmd/gmc/internal/controller/builder.go).

---

## G.10. Controller-Discovered apiserver-CIDR Auto-Narrowing

**Current behavior.** The AGC NetworkPolicy's Kubernetes API server egress rule
(443/6443) is **any-destination by default**. An operator whose platform exposes
a stable apiserver CIDR can opt in to scoping it via the GMC's `--apiserver-cidrs`
flag (Helm value `apiServerCIDRs`), which attaches an `ipBlock` peer per CIDR
(Q145). The default is broad because the post-DNAT apiserver IP is provider- and
topology-specific and a wrong `ipBlock` silently severs apiserver access (the PR
#59 trap). This is the [§5.2](05-security.md#52-agc--proxy-level-threats-namespace-scoped)
residual; per-cloud narrowing guidance is in
[security-operations.md § Tightening AGC apiserver egress](../operations/security-operations.md#tightening-agc-apiserver-egress-the-apiserver-cidrs-allowlist).

**Gap — could the controller discover and narrow it automatically?** The GMC
could read the `kubernetes` Service endpoints in the `default` namespace (the
real post-DNAT apiserver IPs) and scope every tenant's AGC policy itself, making
the tightening the default and removing the operator step. The Q183 feasibility
review concluded this **must not be a default**, for three reasons:

1. **Stale-snapshot regression.** On managed control planes (EKS/GKE/AKS) the
   endpoint IPs rotate on scaling, upgrades, and maintenance without notice. A
   one-time discovery tightens to a set that later stops matching and breaks the
   AGC — a silent reachability regression, which secure-by-default forbids.
2. **Self-lockout under a live watch.** Staying correct requires watching
   `endpoints/kubernetes` and re-reconciling every AGC policy on each change.
   There is always a race window where the policy lags a real rotation; during
   it the AGC — and the GMC, which reaches the apiserver over the same path —
   can be locked out of the very apiserver it needs to *repair* the policy. A
   tightening that can strand the controller maintaining it is not a safe
   default.
3. **CNI rewrites.** Some CNIs apply further SNAT/encapsulation, so even the
   discovered endpoint IPs are not guaranteed to be what the policy evaluator
   matches.

**What would trigger building it.** A design that closes the lockout window —
e.g. always-union the freshly-observed endpoints with the prior set and only
*remove* an IP after a confirmed drain, plus a fail-open watchdog that reverts to
any-destination if the GMC loses apiserver contact — and demonstrated robustness
across the managed-CP rotation behaviours above. Until then, narrowing stays the
operator-confirmed, per-cluster `apiServerCIDRs` opt-in.

**Related.** [05-security.md §5.2](05-security.md#52-agc--proxy-level-threats-namespace-scoped);
[security-operations.md § Tightening AGC apiserver egress](../operations/security-operations.md#tightening-agc-apiserver-egress-the-apiserver-cidrs-allowlist);
[network-architecture.md § Policy 2](network-architecture.md#policy-2-actions-gateway-controller--agc--kubernetes-api-server);
[`builder.go`](../../cmd/gmc/internal/controller/builder.go).

---

## G.11. Worker Scale-Up Rate Limiting (anti-stampede)

> **Not the same as [G.2](#g2-proxy-enforced-per-tenant-rate-limiting).** G.2
> rate-limits the *egress proxy* (GitHub API call volume). This is about the
> rate at which **new worker pods are created** when a burst of jobs is
> acquired — a cold-start stampede control, not an egress control.

**Current behavior.** Worker pods are created **reactively, run-to-completion**:
the Actions Gateway Controller (AGC) provisions a pod the moment a job is
acquired from the broker long-poll and releases it on completion
([§2 architecture](02-architecture.md)). There is no warm pool and no
pre-scaled replica count to "ramp" — pod-creation rate tracks GitHub job
arrival directly. The only ceiling today is the **worker quota** (per-RunnerGroup
capacity, surfaced via the `WorkerQuotaPressure`/`WorkerQuotaExceeded` conditions
with `maxQuotaRetries`/`quotaRetryDelay` backoff). That bounds *how many* run at
once; it does not bound *how fast* they start.

**Gap.** A burst of simultaneously-acquired jobs creates a thundering herd of
cold-starting workers. When those workers all hit the same rate-sensitive shared
resource at t=0, the simultaneity — not the steady-state count — is what causes
damage. A creation-rate limit (a ramp / token bucket on pod admission) would let
the herd start in waves instead of all at once.

**When a rate limit is the right tool (and when it is not).** This is the
"when to use it vs. other solutions" decision. Reach for a scale-up rate limit
**only** when simultaneous cold-starts stampede a rate-sensitive shared resource
that the alternatives below do not already cover:

| Symptom at a burst | Right tool | Why |
|---|---|---|
| Every cold node re-pulls the large runner **image** | **Peer-to-peer image mirror** (Spegel/Dragonfly — see the Q211 P2P image-distribution guide) | Makes N concurrent pulls cost ~1 back-to-source pull with **no added latency**. A ramp still pulls N times, just spread out — strictly worse. The storm scales with distinct cold nodes, not pod count. |
| Cold nodes re-pull the **same image** but no P2P | kubelet `maxParallelImagePulls`/`serializeImagePulls`; pre-pull DaemonSet | Per-node pull throttle / pre-warming is the node-layer fix; keeps cold-start off the critical path. |
| Workers stampede a shared **downstream dependency** at startup (artifact registry, license server, internal API, DB, Vault) | **Scale-up rate limit** *(this item)* — or workflow-level `concurrency:` | This is the genuine case for a ramp: the resource isn't the image and isn't cluster-internal. Often pushable to the workflow author via GitHub's native `concurrency:` groups first. |
| Bursting workers saturate a shared **network egress** path (internet uplink, NAT/SNAT gateway, stateful firewall, site-to-site VPN) — e.g. a multi-site network | **Scale-up rate limit** for the *onset* **+ concurrency ceiling** for *sustained* load | Mixed (see [worked example](#worked-example--multi-site-shared-egress-nat--firewall--vpn)): a ramp smooths connection-establishment / conntrack / SNAT-port churn; sustained-bandwidth saturation is a *ceiling* problem a ramp only defers. |
| Mass node provisioning trips **cloud/control-plane API** throttling | Scale-up rate limit *(this item)*; or autoscaler-side limits (Karpenter/CA) | Smoothing pod-admission rate eases the node-scale-up burst; autoscalers also expose their own rate controls. |
| One tenant/workflow drains the **whole shared quota** and starves others | **Worker quota / concurrency ceiling** (already shipped) | A fairness/blast-radius problem — a *ceiling*, not a *rate*. The existing quota model largely covers it. |
| Limited-seat external system (K concurrent consumers) | **Concurrency cap** (ceiling), not a rate | You want a hard in-flight cap, not a ramp. |

In short: **image** pull storms → P2P + node pull controls; **shared-dependency
or control-plane** stampedes → a rate limit (or workflow `concurrency:`);
**fairness / limited seats** → the quota/ceiling GAG already has. A scale-up rate
limit earns its latency cost only in the middle rows.

### Worked example — multi-site shared egress (NAT / firewall / VPN)

A new environment scales runners up and job runs slow down because the
simultaneously starting workers hammer a shared egress path — an internet
uplink, a NAT/SNAT gateway, a stateful firewall's connection-tracking table, or
a site-to-site VPN tunnel (a failure mode also seen with Actions Runner
Controller). This is a real motivator for the ramp, but diagnose *which* limit
binds, because a rate limit and a ceiling fix different halves:

- **Onset / connection-establishment burst** — NAT/SNAT port-allocation spikes,
  firewall conntrack-table churn, TCP slow-start synchronization, VPN/IKE
  renegotiation. Symptom: slowest *right as the burst starts*, recovering once
  it settles. → a **scale-up rate limit (ramp)** helps directly, by spreading
  connection setup over time.
- **Sustained saturation** — uplink bandwidth, total NAT ports in use, conntrack
  table size, VPN tunnel throughput. Symptom: slow for the *whole duration* of
  the concurrent load. → a **concurrency ceiling** is the right tool; a ramp
  only *defers* the cliff, because it bounds the start *rate*, not the running
  *count*. Also raise capacity (more NAT gateways / SNAT IPs) or cut per-job
  egress with a caching pull-through proxy / P2P mirror.

**Diagnostic:** does the slowness peak during scale-up and recover, or stay flat
while many jobs run? Former → ramp; latter → ceiling. Multi-site setups are
often **both**, so the ramp **complements** GAG's existing worker-quota ceiling
rather than replacing it.

**What "added" would look like.** An **opt-in, per-RunnerGroup** creation-rate
limit on the provisioner — a token bucket bounding new worker pods per unit time
(e.g. `scaleUp.maxPerSecond` + `scaleUp.burst`), default **off**. When the bucket
is empty, excess acquired jobs wait in the same admission path the quota backoff
already uses, so it composes with `WorkerQuotaPressure` rather than adding a new
state machine. Metrics: a `worker_scaleup_throttled_total` counter so operators
can see when it bites.

**Why it is default-off.** GAG's core value is zero-idle, immediate provisioning
on job acquisition; a ramp deliberately delays *already-claimed* jobs, directly
trading time-to-pickup. It also overlaps the cluster autoscaler/Karpenter, which
are making their own node-scaling decisions. So it stays an explicit opt-in for
the narrow stampede cases above, never a default.

**Status — promoted to the active backlog (Q223).** The trigger fired: observed
ARC-style scale-up saturating a shared egress path (NAT gateway / firewall / VPN)
in a multi-site network — the worked example above — which the existing quota
ceiling and workflow `concurrency:` do not fully address. The open questions for
the Q223 plan doc are the exact knob surface (`scaleUp.maxPerSecond`/`burst`
naming and scope), how the ramp composes with the `WorkerQuotaPressure` backoff,
and how it coexists with the node autoscaler's own rate controls.

**Related.** The Q211 P2P image-distribution operations guide (image-pull
storms); [G.2](#g2-proxy-enforced-per-tenant-rate-limiting) (proxy egress rate
limiting — distinct); [appendix-e-capacity-planning.md](appendix-e-capacity-planning.md)
(worker quota / capacity model).

---

← [Cost Model](appendix-f-cost-model.md) | [Back to index](README.md)
