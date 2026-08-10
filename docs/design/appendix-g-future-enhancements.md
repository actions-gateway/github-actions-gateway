# Appendix G — Optional Future Enhancements

← [Cost Model](appendix-f-cost-model.md) | [Back to index](README.md)

---

This appendix records features that have been considered, deliberately left out of the current design, and may be revisited later.
Each entry states the current behavior, the gap it would close, the cost of adding it, and the trigger that would justify the work.

These are not commitments.
An item appearing here means "we know about it, we chose not to build it for v1, and we know what we'd do if the constraints changed."

---

## G.1. Proxy-Enforced Destination Allowlist

> **✅ Implemented (Q242).** This enhancement was promoted to committed work and shipped — it is the attribution-preserving answer to the most common operator ask (letting CI jobs reach their build dependencies without forfeiting the per-tenant egress model).
> It is retained here as a stub so existing cross-references resolve; the live design and operator docs are authoritative.

A platform admin can allow a v2 `EgressProxy` to forward worker traffic to a small, explicit set of **non-GitHub** destinations — by DNS host suffix (`spec.destinationFQDNs`) and by CIDR (`spec.destinationCIDRs`) — while keeping per-tenant egress-IP attribution and the DNS-exfil containment.
The destinations are gated by a **platform-owned** allowlist (GMC `--allowed-egress-fqdns` / `--allowed-egress-cidrs` + an optional watched ConfigMap) enforced by a validating webhook; **both empty = deny-all-non-GitHub** (the secure default).
They widen one source into two enforcement surfaces — `ipBlock` / `toFQDNs` on the pod-egress policy (the hard gate) and the proxy CONNECT allowlist as defense-in-depth (with DNS-rebinding revalidation on the CIDR path).
The docs **lead with an in-cluster caching mirror** as the recommended path and reserve the allowlist for what a mirror can't proxy.

- Trade-offs and threat model: [§5.2](05-security.md#52-agc--proxy-level-threats-namespace-scoped) (Worker Exfiltration via an Allowlisted Non-GitHub Destination).
- Network shape: [network-architecture.md § Worker egress to allowlisted non-GitHub destinations](network-architecture.md#worker-egress-to-allowlisted-non-github-destinations-opt-in-q242-g1).
- Operator runbook: [security-operations.md § Worker egress destinations](../operations/security-operations.md#worker-egress-destinations-the-egress-allowlist).
- Full design + deliverables: [Q242 plan](../plan/archive/q242-g1-proxy-destination-allowlist.md).

**Related security finding.** [docs/plan/security.md](../plan/security.md) M-2.

---

## G.2. Proxy-Enforced Per-Tenant Rate Limiting

**Current behavior.** The proxy does not rate-limit.
GitHub's own per-installation rate limits are the only ceiling on call volume.

**Gap.** If a tenant's runners go into a tight loop (misconfigured workflow, broken retry policy), the only feedback is GitHub's `429 Too Many Requests` and the Actions Gateway Controller (AGC) exponential backoff.
There is no way to slow a single misbehaving RunnerGroup before it hits the GitHub ceiling.

**What "added" would look like.** A token-bucket per-tenant (and optionally per-RunnerGroup) at the proxy.
Configured via a new field on `ProxyConfig` or `RunnerGroupSpec`.
Bucket state stays in-process on each proxy pod; for accurate global limits a shared backend (Redis, etcd) would be required — out of scope for v1.

**What would trigger building it.** A real incident where one tenant saturates the GitHub rate limit and the platform team needs a sub-tenant kill switch finer than "drain the proxy pool."

---

## G.3. Proxy-Side Audit Logging

**Current behavior.** The proxy emits Prometheus counters (`actions_gateway_proxy_connections_active`, `actions_gateway_proxy_connections_total`, `actions_gateway_proxy_dial_errors_total`) but no per-connection log.

**Gap.** Audit trails for "which tenant talked to which GitHub endpoint at what time" are reconstructable today only via cluster-wide flow logs or GitHub's own audit log.
Operators investigating an incident have no per-tenant per-destination view at the proxy layer.

**What "added" would look like.** A structured log line per accepted CONNECT (tenant namespace, target host, target port, bytes in, bytes out, duration).
Off by default; enabled per-tenant via a `spec.proxy.auditLogging: true` field.
Output to stdout for log collectors to scrape.

**What would trigger building it.** Compliance ask for per-tenant egress audit, or a recurring class of incident where the missing log delays root cause.

---

## G.4. TLS Between AGC/Workers and the Proxy

**Current behavior.** The AGC and worker pods reach the proxy at `http://actions-gateway-proxy.<ns>.svc.cluster.local:8080`.
The CONNECT-tunneled payload is itself TLS to GitHub; only the CONNECT request line is in cleartext on the in-cluster hop.

**Gap.** On clusters with eBPF taps, sidecar meshes, or shared-CNI visibility, the CONNECT target host:port is observable.
The actual payload remains encrypted.
No bearer token is sent on the outer hop today, so credential exposure is not on the table.

**What "added" would look like.** cert-manager-managed cert mounted into proxy pods, AGC and workers use `https://` proxy URL.
Adds one in-cluster handshake per upstream connection.
Adds operational cost of managing per-tenant proxy certs.

**What would trigger building it.** A compliance requirement for in-cluster encryption-in-transit beyond what an mTLS service mesh already provides, or a future change that puts authentication on the outer hop.

---

## G.5. Per-RunnerGroup Dedicated Proxy Pool

**Current behavior.** One proxy pool per `ActionsGateway` (i.e., per tenant).
All RunnerGroups within a tenant share the pool.

**Gap.** A bandwidth-heavy GPU RunnerGroup can saturate the proxy pool and slow a co-tenant's CPU-bound RunnerGroup.
The HPA scales the shared pool, but bursts can outrun reaction time.

**What "added" would look like.** An optional `spec.proxy.dedicated: true` field on `RunnerGroupSpec` that causes the GMC to provision a separate proxy `Deployment` + `Service` + `HPA` for that group.
The AGC's `HTTP_PROXY` env var would need to become per-RunnerGroup rather than per-AGC.

**What would trigger building it.** A tenant report showing the shared proxy pool saturating and the HPA failing to keep up with a single RunnerGroup's bursts.

---

## G.6. X25519 ECDH Session Key Exchange

**Current behavior.** The GitHub Actions broker sends each listener session an AES-256-CBC key encrypted with the agent's RSA public key (RSA-OAEP).
The agent decrypts it and uses it to decrypt subsequent job message bodies.
This requires the agent key to be RSA.
Ed25519 agents cannot participate — they receive job messages without the AES layer and rely on TLS as the sole protection for the session payload.

**Why it was left out.** This is a broker-protocol change, not an AGC-side change.
We do not control the GitHub broker.
The current code exposes Ed25519 as an operator opt-in (`--agent-key-type=ed25519`) for deployments that want faster JWT signing and accept the loss of the AES defense-in-depth layer, but the default is RSA-3072 specifically because of this limitation.

**Gap.** Ed25519 is preferable for JWT signing: smaller keys, faster operations, deterministic signatures, no padding oracle surface.
But making it the secure default requires the broker to support a key- exchange mechanism compatible with Curve25519 keys.
X25519 ECDH (Diffie-Hellman over Curve25519) would allow the broker to establish a shared AES session key with an Ed25519 agent without RSA-OAEP: the broker generates an ephemeral X25519 keypair, the agent uses its Curve25519 key (derived from the Ed25519 private key via standard clamping), and both sides derive the same session secret.
The AES message-encryption layer would be fully preserved for Ed25519 agents.

**What "added" would look like.**

This requires a change to the broker protocol, not to this codebase.
On the GitHub side:

- `CreateSession` response includes an ephemeral X25519 public key and a nonce instead of (or in addition to) the RSA-OAEP-encrypted blob.
- The client performs X25519 key agreement and derives the AES session key via HKDF.

On the AGC side, once the broker supports this:

- `goroutine.go` `createSession` detects the X25519 key-agreement path in the `CreateSession` response and performs ECDH derivation instead of RSA-OAEP decryption.
  (~30 LoC using `golang.org/x/crypto/curve25519`.)
- `cmd/agc/main.go` changes the default from `--agent-key-type=rsa` to `--agent-key-type=ed25519`.
- RSA key support is retained for backward compatibility with existing Secrets.

**What would trigger building it.** GitHub updates the broker protocol to include X25519 ECDH session key exchange, or publishes an API allowing Ed25519 agents to participate in encrypted sessions.
The probe binary (`cmd/probe`, configured entirely through its `GITHUB_RUNNER_*` environment — it takes no command-line flags) is the detection harness: pointed at an Ed25519 agent's credentials, a `session OK` result with a non-empty `EncryptionKey` would indicate the broker is sending a key the agent can use.

**Related finding.** [docs/plan/security.md](../plan/security.md) M-11, D-5.

---

## G.7. ValidatingAdmissionPolicy for direct RunnerGroup PriorityClass enforcement

> **✅ Implemented (Q289).** The `priorityclass-allowlist-guard` `ValidatingAdmissionPolicy` ships with the chart (on by default under `admissionPolicy.enabled`) and in `cmd/gmc/config/admission-policy/`.
> It is retained here as a stub so existing cross-references resolve; the live design and operator docs are authoritative.

The GMC webhooks gate every tenant-facing CR's PriorityClass references (Q132/Q289), but RunnerGroup CRs have no webhook of their own — they are normally authored only by the GMC from an already-validated `ActionsGateway` — so a tenant granted **direct `runnergroups` write RBAC** bypassed the allowlist entirely, and webhooks never re-validate **already-stored** objects.
The policy closes both: it rejects any `runnergroups` create/update (from *any* writer, GMC included) whose `priorityTiers[].priorityClassName` or `podTemplate.spec.priorityClassName` is not in the allowlist supplied via its `paramKind` ConfigMap, and re-validates on every write to an existing object.
A `paramKind` carries the allowlist because CRD CEL `XValidation` cannot read external config — the same reason the webhook, not a CRD rule, enforces it on the other kinds.

- Threat model and enforcement matrix: [05-security.md § Cross-tenant pod preemption via PriorityClass](05-security.md#cross-tenant-pod-preemption-via-priorityclass).
- Operator runbook, param-ConfigMap contract, and failure modes: [security-operations.md § Priority classes](../operations/security-operations.md#priority-classes-the-allowed-priority-classes-allowlist).
- Stored-object upgrade sweep: [security-operations.md § Upgrading](../operations/security-operations.md#upgrading-previously-ungated-priorityclassname-fields-are-now-gated).

**Related finding.** [docs/plan/q132-priorityclass-allowlist.md](../plan/archive/q132-priorityclass-allowlist.md).

---

## G.8. Optional (Disable-able) Egress Proxy

**Current behavior.** The per-tenant egress proxy pool ([§2.3](02-architecture.md#23-tier-3--egress-proxy-pool)) is provisioned unconditionally for every `ActionsGateway`.
The Gateway Manager Controller (GMC) always emits the proxy `Deployment`, `Service`, `HPA`, `PodDisruptionBudget`, and TLS cert `Secret`, and the workload `NetworkPolicy` ([`buildWorkloadNetworkPolicy`](../../cmd/gmc/internal/controller/builder.go)) grants Actions Gateway Controller (AGC) and worker pods egress to *only* cluster DNS and the proxy on port 8080 — so with no proxy, those pods have no network path to GitHub at all. `spec.proxy` is `+optional`, but only its *tuning* (replica counts, resources) is; the pool itself cannot be turned off. `spec.proxy.managedNetworkPolicy: false` is **not** a disable switch — it only drops the GitHub-CIDR rule from the proxy's own NetworkPolicy (for FQDN-based CNI policies); the proxy still runs and still carries all traffic.

**Why it was left out.** Routing *all* GitHub-bound traffic — AGC control-plane and worker data-plane alike — through the per-tenant proxy is what makes the gateway's differentiating claim coherent: stable per-tenant egress IPs at GitHub.
Four downstream capabilities rest on it (see [docs/plan/worker-egress-proxy.md](../plan/worker-egress-proxy.md)): GitHub-side IP allowlisting, per-tenant audit attribution, GitHub-side incident containment (a rate-limit / abuse-flag / IP-ban hits one tenant, not whoever shares the node), and the one-operation per-tenant kill-switch (drain the pool).
These are network-attribution, compliance, and operability properties — **not** a security boundary against a compromised worker, which the installation token already scopes.
Making the proxy optional therefore forfeits attribution and containment without breaching the token boundary.
For the multi-tenant deployments the design targets, that trade is not worth a default, so the proxy stays mandatory.

**Gap.** Niche deployments pay for a property they may not need: cost-sensitive single-tenant or dev clusters that don't require per-tenant egress IPs, and clusters that already attribute egress at the node or cloud layer (per-namespace NAT, a cloud NAT gateway per tenant).
For these, the always-on floor of `minReplicas: 2` proxy pods per tenant is pure overhead.

**What "added" would look like.**

- A new field on `ProxyConfig`, e.g. `mode: Required` (default) `| Disabled` (or `enabled: true` default-true).
- When disabled, the GMC skips the proxy `Deployment` / `Service` / `HPA` / `PDB` / cert `Secret`, and does not inject `HTTP(S)_PROXY` onto the AGC (workers already no-op the proxy-CA mount on an empty `ProxyTLSSecretName`).
- `buildWorkloadNetworkPolicy` swaps its `→ proxy:8080` egress rule for direct egress to the GitHub CIDRs on 443, reusing the IP-range refresh loop that already feeds `buildProxyNetworkPolicy`.
  DNS stays confined to cluster DNS (an independent control — the exfiltration side-channel mitigation in [§5.2](05-security.md) is unaffected).
- `updateStatus` drops `ProxyAvailable` from the `Ready` computation when the proxy is disabled.
- The validating webhook emits a *warning* (not a rejection) on create/update that the attribution / containment / kill-switch properties are forfeited.

Estimated cost: an M-sized change across GMC provisioning, the workload NetworkPolicy, env injection, status, and the webhook, plus CRD docs.

**Secure-by-default constraint.** Disabling the proxy is a security and isolation regression, so per the project's secure-by-default rule it must default to `Required`, be reachable only as an explicit opt-in to the less-secure mode, and carry the forfeited properties prominently in the operator-facing docs.
It must never become the default or a silent behavior.

**What would trigger building it.** A concrete operator ask for a single-tenant / dev / cost-sensitive deployment, or a deployment that already provides per-tenant egress attribution at the node or cloud layer and wants to shed the proxy's always-on cost.

**Related finding.** [docs/plan/worker-egress-proxy.md](../plan/worker-egress-proxy.md) (the four properties forfeited); [02-architecture.md §2.3](02-architecture.md#23-tier-3--egress-proxy-pool).

---

## G.9. NetworkPolicy Egress Extensibility (signers, telemetry, job dependencies)

**Current behavior.** The GMC reconciles three default-deny per-tenant NetworkPolicies (§5.2, [`builder.go`](../../cmd/gmc/internal/controller/builder.go)).
The **AGC** policy permits egress to cluster DNS + the kube API server (+ the GitHub CIDR allowlist in direct mode), and — once the Vault egress work landed — a *scoped* AGC→Vault rule when a workload-identity gateway declares `signer.vault.networkPolicy` (a pod/namespace selector or a CIDR; the rule targets only that peer, on the Vault API port from the signer address, and only the AGC pod selects into it).
The **workload** policy permits worker pods egress to cluster DNS + the proxy on 8080 (+ the GitHub CIDRs in direct mode) and nothing else.
The link-local block `169.254.0.0/16` is permitted on **port 53 only** (NodeLocal DNSCache), not for anything else.

The Vault work established the reusable pattern for opening a hole in the default-deny envelope: a peer the **tenant names in its own CR**, that the **GMC scopes** to a single peer + port, attached to the **AGC policy only** so untrusted worker pods never inherit it.
That peer descriptor is now the shared [`EgressPeer`](../../api/v2alpha1/actionsgateway_types.go) type (selector | CIDR + optional explicit port), extracted from the Vault-specific shape before the v2 freeze so future consumers reuse one descriptor (Q204; see the pre-v2 analysis below).
The entries below are the other egress holes we can foresee wanting, the governance each needs, and — the load-bearing question — confirmation that none of them forces a breaking API change before v2.

**Gap — the egress holes we can foresee.**

| Use case | Who needs egress | Governance | Notes |
|---|---|---|---|
| **Cloud KMS signers** (AWS/GCP/Azure KMS as additive `signer.provider` members) | AGC | Tenant-delegated for the signer endpoint (tenant names its own KMS); **admin-governed** for any cloud-metadata leg | The metadata/credential path varies by provider: **GKE Workload Identity** and **EC2 IMDS / EKS Pod Identity** reach a link-local address (`169.254.169.254` / `169.254.170.23`) on :80 — a *new* hole distinct from the existing :53-only `169.254/16` rule; **AWS IRSA** and **Azure Workload Identity** instead call a *public* STS/AAD host (the same opaque-URL problem the Vault rule already solves). |
| **AGC telemetry** (`spec.tracing.endpoint`, and any future metrics/log push) | AGC | Tenant-delegated, AGC-only | A scoped rule is derivable from the *already-existing* `tracing.endpoint`; today that endpoint is not auto-permitted on a policy-enforcing CNI. |
| **Worker job dependencies** (container/package registries, build caches, internal services the CI job legitimately calls) | Worker | **Through the proxy → tenant-delegated** (attribution preserved); **direct → admin-governed** | The single most common operator ask. The attribution-preserving answer is the proxy destination allowlist ([G.1](#g1-proxy-enforced-destination-allowlist)); a *direct* worker egress hole forfeits per-tenant IP attribution and the DNS-exfil containment, so it must stay an explicit admin opt-in, never tenant-openable by default. |
| **Cloud metadata for worker OIDC** (`configure-aws-credentials` etc.) | Worker | **Admin-governed, default-deny** | Metadata-server reach from untrusted job code is a classic credential-theft / SSRF vector; the common OIDC path uses GitHub's token → public STS, which needs no metadata at all. |

**The governance principle (the durable decision).** A tenant may self-declare egress, in its own CR, **only** to peers it names and controls that are reached **either** by its AGC control plane **or** through the attribution-preserving proxy.
Egress that originates **directly from worker pods** (bypassing the proxy) or targets a **shared / sensitive surface** (cloud metadata, cluster infrastructure, arbitrary internet) stays **platform-admin-governed** — a GMC flag or cluster policy, never a tenant-settable default.
The rationale is the trust split the whole design rests on: worker pods run untrusted GitHub Actions job code, the AGC is trusted control plane, and the proxy is the per-tenant attribution boundary.
The Vault `signer.vault.networkPolicy` is the canonical tenant-delegated case (AGC-only, tenant-named, GMC-scoped); the worker-direct and metadata cases are the canonical admin-governed ones.

**Pre-v2 API-compatibility analysis (no breaking change required).** Every hole above is reachable additively under the frozen v2 shape:

- Cloud KMS signers join as **new `signer.provider` union members**, each carrying its *own* optional peer descriptor — exactly the additive contract the union was built for; existing members are untouched.
- AGC telemetry and worker-direct egress would be **new optional fields** on existing structs (`ActionsGateway` / `RunnerSet` / `RunnerTemplate`) or new GMC flags.
  Optional CRD fields don't prune and old clients ignore them, so adding them later is non-breaking.
- The worker-through-proxy case extends the proxy allowlist ([G.1](#g1-proxy-enforced-destination-allowlist)), not the CRD egress shape.

The **one** pre-v2 hygiene decision worth settling deliberately was: the Vault work shipped a per-feature peer descriptor (`VaultNetworkPolicy`: selector | CIDR, port derived from the address).
If KMS, telemetry, and worker-egress had each grown their own near-duplicate descriptor, v2 would freeze three inconsistent shapes. **Resolved (Q204):** the descriptor is now the shared [`EgressPeer`](../../api/v2alpha1/actionsgateway_types.go) type (selector | CIDR
+ an optional explicit port), and `signer.vault.networkPolicy` references it.
  The extraction is serialization-compatible — the field keys (`podSelector`, `namespaceSelector`, `cidr`) are unchanged, so the v2alpha1 shape already shipped is preserved; only the additive optional `port` is new, with the Vault builder still deriving the port from the address when `port` is unset.
  Future consumers (KMS, telemetry) now reference the same type instead of forking it — the shape that is far cheaper to settle before the v2alpha1 → v2beta1 graduation ([Q74](../STATUS.md)) than after.

**What would trigger building any of it.** A concrete signer provider beyond Vault (KMS), a policy-CNI operator who needs their tracing endpoint or a job dependency reachable, or the graduation review deciding to consolidate the peer descriptor.

**Related.** [05-security.md](05-security.md) (the per-tenant NetworkPolicy isolation model); [G.1 Proxy-Enforced Destination Allowlist](#g1-proxy-enforced-destination-allowlist); [G.8 Optional Egress Proxy](#g8-optional-disable-able-egress-proxy); [`builder.go`](../../cmd/gmc/internal/controller/builder.go).

---

## G.10. Controller-Discovered apiserver-CIDR Auto-Narrowing

**Current behavior.** The AGC NetworkPolicy's Kubernetes API server egress rule (443/6443) is **any-destination by default**.
An operator whose platform exposes a stable apiserver CIDR can opt in to scoping it via the GMC's `--apiserver-cidrs` flag (Helm value `apiServerCIDRs`), which attaches an `ipBlock` peer per CIDR (Q145).
The default is broad because the post-DNAT apiserver IP is provider- and topology-specific and a wrong `ipBlock` silently severs apiserver access (the PR #59 trap).
This is the [§5.2](05-security.md#52-agc--proxy-level-threats-namespace-scoped) residual; per-cloud narrowing guidance is in [security-operations.md § Tightening AGC apiserver egress](../operations/security-operations.md#tightening-agc-apiserver-egress-the-apiserver-cidrs-allowlist).

**Gap — could the controller discover and narrow it automatically?** The GMC could read the `kubernetes` Service endpoints in the `default` namespace (the real post-DNAT apiserver IPs) and scope every tenant's AGC policy itself, making the tightening the default and removing the operator step.
The Q183 feasibility review concluded this **must not be a default**, for three reasons:

1. **Stale-snapshot regression.** On managed control planes (EKS/GKE/AKS) the endpoint IPs rotate on scaling, upgrades, and maintenance without notice.
   A one-time discovery tightens to a set that later stops matching and breaks the AGC — a silent reachability regression, which secure-by-default forbids.
2. **Self-lockout under a live watch.** Staying correct requires watching `endpoints/kubernetes` and re-reconciling every AGC policy on each change.
   There is always a race window where the policy lags a real rotation; during it the AGC — and the GMC, which reaches the apiserver over the same path — can be locked out of the very apiserver it needs to *repair* the policy.
   A tightening that can strand the controller maintaining it is not a safe default.
3. **CNI rewrites.** Some CNIs apply further SNAT/encapsulation, so even the discovered endpoint IPs are not guaranteed to be what the policy evaluator matches.

**What would trigger building it.** A design that closes the lockout window — e.g. always-union the freshly-observed endpoints with the prior set and only *remove* an IP after a confirmed drain, plus a fail-open watchdog that reverts to any-destination if the GMC loses apiserver contact — and evidence that it holds across the managed-CP rotation behaviours above.
Until then, narrowing stays the operator-confirmed, per-cluster `apiServerCIDRs` opt-in.

**Related.** [05-security.md §5.2](05-security.md#52-agc--proxy-level-threats-namespace-scoped); [security-operations.md § Tightening AGC apiserver egress](../operations/security-operations.md#tightening-agc-apiserver-egress-the-apiserver-cidrs-allowlist); [network-architecture.md § Policy 2](network-architecture.md#policy-2-actions-gateway-controller--agc--kubernetes-api-server); [`shared_networkpolicy.go`](../../cmd/gmc/internal/controller/shared_networkpolicy.go).

---

## G.11. Worker Scale-Up Rate Limiting (anti-stampede)

> **Not the same as [G.2](#g2-proxy-enforced-per-tenant-rate-limiting).** G.2 rate-limits the *egress proxy* (GitHub API call volume).
> This is about the rate at which **new worker pods are created** when a burst of jobs is acquired — a cold-start stampede control, not an egress control.

**Current behavior.** Worker pods are created **reactively, run-to-completion**: the Actions Gateway Controller (AGC) provisions a pod the moment a job is acquired from the broker long-poll and releases it on completion ([§2 architecture](02-architecture.md)).
There is no warm pool and no pre-scaled replica count to "ramp" — pod-creation rate tracks GitHub job arrival directly.
The only ceiling today is the **worker quota** (per-RunnerGroup capacity, surfaced via the `WorkerQuotaPressure`/`WorkerQuotaExceeded` conditions with `maxQuotaRetries`/`quotaRetryDelay` backoff).
That bounds *how many* run at once; it does not bound *how fast* they start.

**Gap.** A burst of simultaneously-acquired jobs creates a thundering herd of cold-starting workers.
When those workers all hit the same rate-sensitive shared resource at t=0, the simultaneity — not the steady-state count — is what causes damage.
A creation-rate limit (a ramp / token bucket on pod admission) would let the herd start in waves instead of all at once.

**When a rate limit is the right tool (and when it is not).** This is the "when to use it vs. other solutions" decision.
Reach for a scale-up rate limit **only** when simultaneous cold-starts stampede a rate-sensitive shared resource that the alternatives below do not already cover:

| Symptom at a burst | Right tool | Why |
|---|---|---|
| Every cold node re-pulls the large runner **image** | **Peer-to-peer image mirror** (Spegel/Dragonfly — see the Q211 P2P image-distribution guide) | Makes N concurrent pulls cost ~1 back-to-source pull with **no added latency**. A ramp still pulls N times, just spread out — strictly worse. The storm scales with distinct cold nodes, not pod count. |
| Cold nodes re-pull the **same image** but no P2P | kubelet `maxParallelImagePulls`/`serializeImagePulls`; pre-pull DaemonSet | Per-node pull throttle / pre-warming is the node-layer fix; keeps cold-start off the critical path. |
| Workers stampede a shared **downstream dependency** at startup (artifact registry, license server, internal API, DB, Vault) | **Scale-up rate limit** *(this item)* — or workflow-level `concurrency:` | This is the genuine case for a ramp: the resource isn't the image and isn't cluster-internal. Often pushable to the workflow author via GitHub's native `concurrency:` groups first. |
| Bursting workers saturate a shared **network egress** path (internet uplink, NAT/SNAT gateway, stateful firewall, site-to-site VPN) — e.g. a multi-site network | **Scale-up rate limit** for the *onset* **+ concurrency ceiling** for *sustained* load | Mixed (see [worked example](#worked-example--multi-site-shared-egress-nat--firewall--vpn)): a ramp smooths connection-establishment / conntrack / SNAT-port churn; sustained-bandwidth saturation is a *ceiling* problem a ramp only defers. |
| Mass node provisioning trips **cloud/control-plane API** throttling | Scale-up rate limit *(this item)*; or autoscaler-side limits (Karpenter/CA) | Smoothing pod-admission rate eases the node-scale-up burst; autoscalers also expose their own rate controls. |
| One tenant/workflow drains the **whole shared quota** and starves others | **Worker quota / concurrency ceiling** (already shipped) | A fairness/blast-radius problem — a *ceiling*, not a *rate*. The existing quota model largely covers it. |
| Limited-seat external system (K concurrent consumers) | **Concurrency cap** (ceiling), not a rate | You want a hard in-flight cap, not a ramp. |

In short: **image** pull storms → P2P + node pull controls; **shared-dependency or control-plane** stampedes → a rate limit (or workflow `concurrency:`); **fairness / limited seats** → the quota/ceiling GAG already has.
A scale-up rate limit earns its latency cost only in the middle rows.

### Worked example — multi-site shared egress (NAT / firewall / VPN)

A new environment scales runners up and job runs slow down because the simultaneously starting workers hammer a shared egress path — an internet uplink, a NAT/SNAT gateway, a stateful firewall's connection-tracking table, or a site-to-site VPN tunnel (a failure mode also seen with Actions Runner Controller).
This is a real motivator for the ramp, but diagnose *which* limit binds, because a rate limit and a ceiling fix different halves:

- **Onset / connection-establishment burst** — NAT/SNAT port-allocation spikes, firewall conntrack-table churn, TCP slow-start synchronization, VPN/IKE renegotiation.
  Symptom: slowest *right as the burst starts*, recovering once it settles. → a **scale-up rate limit (ramp)** helps directly, by spreading connection setup over time.
- **Sustained saturation** — uplink bandwidth, total NAT ports in use, conntrack table size, VPN tunnel throughput.
  Symptom: slow for the *whole duration* of the concurrent load. → a **concurrency ceiling** is the right tool; a ramp only *defers* the cliff, because it bounds the start *rate*, not the running *count*.
  Also raise capacity (more NAT gateways / SNAT IPs) or cut per-job egress with a caching pull-through proxy / P2P mirror.

**Diagnostic:** does the slowness peak during scale-up and recover, or stay flat while many jobs run?
Former → ramp; latter → ceiling.
Multi-site setups are often **both**, so the ramp **complements** GAG's existing worker-quota ceiling rather than replacing it.

**What "added" would look like.** An **opt-in, per-RunnerGroup** creation-rate limit on the provisioner — a token bucket bounding new worker pods per unit time (e.g. `scaleUp.maxPerSecond` + `scaleUp.burst`), default **off**.
When the bucket is empty, excess acquired jobs wait in the same admission path the quota backoff already uses, so it composes with `WorkerQuotaPressure` rather than adding a new state machine.
Metrics: a `worker_scaleup_throttled_total` counter so operators can see when it bites.

**Why it is default-off.** GAG's core value is zero-idle, immediate provisioning on job acquisition; a ramp deliberately delays *already-claimed* jobs, directly trading time-to-pickup.
It also overlaps the cluster autoscaler/Karpenter, which are making their own node-scaling decisions.
So it stays an explicit opt-in for the narrow stampede cases above, never a default.

**Status — ✅ implemented (Q223).** Shipped as an opt-in, default-off per-RunnerGroup token bucket on worker-pod creation: `spec.scaleUp.maxPerSecond` (required, ≥1) + `spec.scaleUp.burst` (optional, defaults to `maxPerSecond`), on both the v1 `RunnerGroup` and v2 `RunnerSet`.
The limiter gates pod creation in the AGC provisioner (`golang.org/x/time/rate`), so a throttled job **waits** for a token — holding its GitHub lock, renewed in the background — composing with the `WorkerQuotaPressure` / quota-retry wait rather than adding a new state machine; it coexists with the node autoscaler (bounding pod-admission rate eases the node-scale-up burst, and the autoscaler keeps its own rate controls).
Each throttle increments `actions_gateway_worker_scaleup_throttled_total`.
Operator guidance: [tenant-onboarding.md § worker scale-up rate limit](../operations/tenant-onboarding.md#step-2-create-the-actionsgateway-resource); design record: [archive/q223-worker-scaleup-rate-limit.md](../plan/archive/q223-worker-scaleup-rate-limit.md).

**Related.** The Q211 P2P image-distribution operations guide (image-pull storms); [G.2](#g2-proxy-enforced-per-tenant-rate-limiting) (proxy egress rate limiting — distinct); [appendix-e-capacity-planning.md](appendix-e-capacity-planning.md) (worker quota / capacity model).

---

## G.12. Warm Worker Pool (`minIdleWorkers`)

> **Not the same as node-/workspace-layer warmth.** This is about holding **pre-created worker pods** so a job skips pod scheduling.
> It is *not* image warmth (a peer-to-peer mirror / pre-pull DaemonSet — see [G.11](#g11-worker-scale-up-rate-limiting-anti-stampede) and the Q211 guide) and *not* build-/model-cache warmth (a cache volume mounted in the worker `podTemplate` — a raw PVC works today; a *managed* backend is [Q215](../STATUS.md#Q215)).
> Those cheaper layers should be exhausted first; this item is the residual.

**Current behavior.** Worker pods are created **reactively** the moment a job is acquired from the broker long-poll, with a just-in-time (JIT) runner config bound to that job, and released on completion ([§2 architecture](02-architecture.md)).
There is no pool of pre-created pods.
Job-acquisition latency is therefore bounded by pod scheduling + image pull, **not** runner registration — the goroutine session multiplexer already holds a standing pool of pre-registered virtual sessions at negligible cost ([appendix-d §KEDA](appendix-d-alternatives-considered.md), [appendix-a SLOs](appendix-a-capacity-slos.md)).
So GAG is already "warm" at the layer ARC's `minRunners > 0` exists to warm; only the compute is cold, by design.

**What it would add.** A bounded, per-RunnerGroup pool of **pre-scheduled, image-warm worker pods** kept idle so an acquired job binds to a waiting pod instead of creating one — eliminating the pod-schedule (and, on those nodes, image-pull) latency for the first *N* concurrent jobs.

**Why it is a narrow, last-resort layer.** Decompose the cold start and each layer has a *cheaper* fix that does not hold idle compute:

| Cold-start layer | Cheaper fix (no idle pods) |
|---|---|
| Runner **registration** | Already eliminated — goroutine session pool. |
| **Image** pull | P2P image mirror (Spegel/Dragonfly) / pre-pull DaemonSet. |
| Build cache / **model weights** (the real GPU/ML startup cost) | Cache volume in the `podTemplate` (raw PVC works today; managed backend = [Q215](../STATUS.md#Q215)). |
| **Pod scheduling** | *This item* — the only layer a warm pod pool uniquely addresses. |

Only teams for whom pod-schedule latency is a large fraction of job time (sub-minute, high-frequency, human-waited CPU jobs) *after* exhausting the rows above are candidates.

**Why GPU/ML is explicitly a zero.** Holding idle **GPU** pods is exactly the idle-accelerator cost GAG sells against ([appendix-f cost model §Zero idle](appendix-f-cost-model.md)).
And GPU cold-start is dominated by image + model-weight loading (the cache-volume row), not scheduling.
So the segment that most often *feels* cold is the one this feature should **not** serve — point it at P2P + cache volumes + warm node capacity (Karpenter/CA) instead.

**Design tension.** Today pod creation and JIT binding are coupled to acquisition; a warm pool decouples them — pre-create an **unbound** pod, then inject the JIT config into a waiting one at acquisition.
That partially re-adopts the ARC pre-registered-idle-runner model GAG deliberately replaced with goroutines, and must preserve the single-use invariant (**1 job per pod**, [§5.2 security](05-security.md)): a never-run warm pod may accept a fresh JIT config, but a pod must never be reused across jobs.
Open questions: the knob surface (`minIdleWorkers` scope/naming, per RunnerGroup), how the pool composes with the worker quota ceiling and `WorkerQuotaPressure` backoff, and pod TTL/recycle for idle warm pods.

**Why it would be default-off.** It reintroduces the idle-compute cost GAG's core value removes — a speed-for-idle-cost trade.
Per the [secure-/cost-by-default principle](05-security.md), the zero-idle default stays; this is an explicit per-RunnerGroup opt-in, documented as a trade, and *not* recommended for GPU groups.

**Status — parked (Deferred), demand-pull only.** No Queue row and no priority position.
Trigger: a self-hosted, multi-tenant CPU-CI team hits pod-schedule latency after exhausting P2P/pre-pull + cache volumes.
Rationale for parking: the eligible audience is a single-digit fraction of GAG adopters (every filter above compounds, and GPU — a flagship segment — is excluded), and the cheaper layers absorb most real "GAG feels cold" reports.

**Related.** [G.11](#g11-worker-scale-up-rate-limiting-anti-stampede) (scale-up *rate* limit — a ceiling on how fast pods start, the inverse concern); [appendix-d §KEDA](appendix-d-alternatives-considered.md) (why the goroutine pool replaces pre-registered idle runners); [appendix-f cost model](appendix-f-cost-model.md) (the idle-floor argument this trades against).

---

## G.13. Server-Side Apply for the GMC's Child Resources

**Current behavior.** The GMC's per-reconcile `apply*` helpers write child resources with `controllerutil.CreateOrPatch` and a mutate closure that replaces the whole `Spec` — see the rationale in [§2 architecture](02-architecture.md#design-choice-reconciler-writes-use-createorpatch-not-server-side-apply).
Only `applyNamespacePSA` uses Server-Side Apply (SSA), because PSA labels are a security control whose out-of-band edits must be *detected*, not silently re-asserted.

**The gap it would close — conflict surfacing, not churn.** Whole-`Spec` replacement is correct only where the GMC is the sole writer of every field in that spec.
Two children break that, and today the exceptions are hand-maintained:

| Child | Second writer | How the exception is encoded |
|---|---|---|
| Proxy `Service` | apiserver (`.spec.clusterIP`) | `applyService` assigns `type`/`selector`/`ports` only |
| Proxy `Deployment` | the pool's HPA (`.spec.replicas`) | `assignHPATargetDeploymentSpec` |

The `Deployment` exception was found the hard way (Q283): the reconciler reverted every HPA scale-out within milliseconds, capping each tenant's egress capacity at `minReplicas`, and it did so *silently* for the project's whole lifetime.
Under SSA the same mistake is loud — applying a field another manager owns returns a `409 Conflict` naming the field and the manager, so the reconciler errors on the first pass instead of ping-ponging. **That, not patch-traffic reduction, is the case for migrating.** (The steady-state churn SSA also removes is already a no-op: the apiserver dedups it, verified by the `apply_nochurn` envtest.)

**What it would cost.** SSA cannot be handed a typed `*appsv1.Deployment`.
A typed Go struct cannot distinguish "field unset" from "field == zero value", so applying one claims ownership of every zero-valued field (`strategy: {}`, `resources: {}`) and fights the apiserver's defaulters.
Doing it correctly means rewriting the ten `build*` functions into a parallel set of applyconfiguration builders (`appsv1ac.Deployment(...)`), of which the proxy `Deployment`'s full `PodSpec` — containers, probes, volumes, affinity, security contexts — is by far the largest.

**What it would *not* buy.** SSA gives "never own `.spec.replicas`" for free: omit `WithReplicas(...)` from the apply configuration and the field is never claimed.
Omission *is* un-ownership; no follow-up patch to disown a field is needed, and attempting one is a footgun — **SSA deletes a field that drops out of your apply set unless another manager owns it.** An "apply with `replicas`, then un-own it" sequence would therefore strip `.spec.replicas` on the second reconcile whenever the HPA has not yet written the `scale` subresource (no metrics-server, or simply `ScalingActive=False` on a cold cluster), and the apiserver would re-default the pool to **1 replica**.
Q283's failure mode, re-entered through a different door.

Nor does SSA remove the floor logic.
"Seed `minReplicas` at create" and "restore a pool found at zero" are imperative writes under a separate field manager either way.
SSA relocates the special case; it does not delete it.

**Open question to settle first.** The HPA takes ownership of `.spec.replicas` via an `Update` on the `scale` *subresource*, which records a `managedFields` entry scoped to that subresource.
Whether that entry produces a conflict against an `Apply` on the parent resource is not obvious from the SSA spec and must be confirmed by envtest before the conflict-surfacing argument can be relied on.
If it does **not** conflict, the primary motivation for this item evaporates and the hand-maintained exceptions above are the right answer permanently.

**Status — parked, no Queue row.** Trigger: a *third* child resource acquires a second writer, or the envtest above confirms subresource-scoped ownership does conflict on parent applies — at which point the migration converts a class of silent correctness bugs into loud reconcile errors, and the builder rewrite starts paying for itself.

**Related.** [§2 architecture](02-architecture.md#design-choice-reconciler-writes-use-createorpatch-not-server-side-apply) (why `CreateOrPatch` was chosen, and the two exceptions it forces); [§5.3 security](05-security.md#53-security-profiles-and-the-privileged-opt-in) (`applyNamespacePSA`, the one place SSA is already the right tool).

---

## G.14. Kata e2e untrusted-PR posture — tight egress + in-cluster pull-through mirror

The Kata worker variant (the dogfood e2e default since Q286) hardens the kernel-escape axis, but its egress stays trusted-CI-shaped: the e2e tenant runs an additive open-egress NetworkPolicy because the suite pulls from Docker Hub / quay / registry.k8s.io / get.helm.sh — all CDN-fronted, so a CIDR allowlist would rot, and an FQDN allowlist is not enforceable on GKE Dataplane V2 (Q245).
Until that changes, Kata-isolated runners are still only suitable for *trusted* CI.

**The durable answer** that would let the variant carry untrusted / OSS-PR CI: an in-cluster pull-through registry mirror (so workers need no direct registry egress) plus a tight egress policy scoped to the mirror, GitHub, and DNS.
Operator-facing context: [kata-dind-workloads.md](../operations/kata-dind-workloads.md).

**Status: active — the Demand trigger fired 2026-07-31 (operator ask), moving [Q408](../STATUS.md#Q408) into the Queue with a phased plan: [q408-untrusted-pr-egress.md](../plan/q408-untrusted-pr-egress.md).** Published on the [public roadmap](../roadmap.md#exploring--longer-term) as the named path to untrusted-PR CI.

---

## G.15. Extract the Batch Right-Sizer into a Standalone / Reusable Tool

**Current behavior.** The worker right-sizing loop (Q359) ships **inside** the AGC: `cmd/agc/internal/usage/` samples per-job CPU/memory peaks from `metrics.k8s.io`, aggregates fixed-bucket peak histograms per RunnerSet × container, and the RunnerSet reconciler surfaces derived recommendations in `status.sizingRecommendation` and applies them at pod-build time under the opt-in `spec.sizing` profiles.
The full analysis of why the native design won over external tooling is [Appendix D §D.7](appendix-d-alternatives-considered.md#d7-worker-right-sizing-why-built-in-not-bolted-on); the short version is that actuation must happen at pod creation and the AGC's provisioner already is the pod creator, so native actuation avoids the mutating admission webhook — and its fail-open/fail-closed availability dilemma — that any external tool would need.

**The deferred generality.** Almost none of the measurement/derivation core is GAG-specific.
Peak-per-pod-lifetime sampling, the per-job-peak histograms and quantile interpolation, the p95-requests / max-plus-headroom-limit derivation, and the confidence gating all apply to any one-pod-one-unit-of-work workload: Kubernetes Jobs, Tekton TaskRuns, Argo Workflows, and ARC's ephemeral runners share the shape, and no good open-source batch right-sizer exists (stock VPA is structurally unfit for run-to-completion pods — §D.7).
The implementation deliberately keeps the extraction seams narrow: the `usage` package touches GAG at exactly two points — the worker-pod owner label (`provisioner.LabelRunnerSet`) it selects on, and the `v2alpha1` recommendation status types it emits.

**What extraction would look like.** Two increments, either stoppable:

1. **Library extraction** — lift sampler + histogram + derivation into a neutral module (selector-and-types parameterized).
   GAG consumes it unchanged, keeping its in-process actuation advantage; other controllers embed it the same way.
2. **Standalone tool** — a `PodSizingPolicy`-style CRD (label selector for grouping, profile + clamps mirroring `spec.sizing`), recommendations in the policy status, and a mutating admission webhook as the generic pod-creation actuator for workloads whose controller does not embed the library.
   This is the piece with real new cost: webhook TLS/cert lifecycle, the `failurePolicy` availability trade, its own chart/release/signing pipeline, and a Tier-0 security posture (a cluster-wide pod-mutating webhook).

**Why deferred.** Three reasons, in order: (1) the sizing *model* (p95 of per-job peaks, 1.4× memory headroom, whole-pod confidence gating) is not yet validated against a live workload — freezing an unproven model into a general-purpose tool's public contract would be premature (the Q359 residual tracks live dogfood validation); (2) a standalone tool is a second product with its own maintenance and security budget, which would have delayed and diluted the GAG-native feature it was carved from; (3) GAG's adoption story wants the capability built in — "install one thing" — even though, as a pure-adoption OSS project, a general tool could plausibly reach a wider audience later.

**Triggers to revisit.** Both of: the model is live-validated on dogfood (the Q359 residual closes with the derivation confirmed or corrected), **and** concrete external demand appears (an issue asking to size non-GAG batch workloads, or another controller wanting to embed the recommender).
Start with increment 1; only build increment 2's webhook if a consumer exists that cannot embed the library.

---

## G.16. `ProvisioningRequest` Pre-Acquire Capacity Probe (`check-capacity`)

**Current behavior.** The admission gate refuses to claim a job when the declared worker ceiling is reached (Q59) or the namespace `ResourceQuota` has no headroom (#784).
It has no rung for *placeability*: a tenant whose worker shape cannot be scheduled keeps claiming jobs, each spending a JIT runner record and holding a GitHub lock until `pendingPodDeadline` reaps the pod. `WorkersUnschedulable` (Q157/Q303) reports the scheduler's verdict but never reaches the gate, and [§D.8](appendix-d-alternatives-considered.md#d8-gating-intake-on-capacity-which-signals-are-safe-to-gate-on) records why it cannot be gated on directly wherever a cluster autoscaler is running: the Pending pod *is* the scale-up request.

**The gap this closes.** cluster-autoscaler's `ProvisioningRequest` (`autoscaling.x-k8s.io/v1`) resolves that conflict.
Class `check-capacity.autoscaling.x-k8s.io` is a one-off feasibility question for a pod shape *without creating the pods*; `best-effort-atomic-scale-up.autoscaling.x-k8s.io` provisions the capacity atomically instead.
Either way the answer arrives before the claim, with no GitHub lock ticking, and asking is itself the request for capacity, so the autoscaler trigger is not forfeited.
No CI runner controller appears to use this primitive.

**Sketch.** Two further values of the `spec.capacityGate.mode` enum introduced by the [capacity-aware-intake plan](../plan/capacity-aware-intake.md) (`Probe`, `Provision`), reusing that plan's rung, condition, rejection-metric label, rate-limiting behavior, and fail-open contract unchanged.
A negative answer refuses the claim, leaving the job queued at GitHub for redelivery.
Off by default, per-RunnerSet (which is per-pod-shape, since a set resolves to exactly one `RunnerTemplate`), with a short-TTL cached verdict because a probe per job would be far too chatty.

**Cost, including the parts that are easy to miss.** The plan estimates ~2,400 to 3,400 lines across five or more PRs, dominated by four things:

1. **It cannot be synchronous.** `Admit` runs inline before `AcquireJob`; `ProvisioningRequest` is create-object-then-await-conditions.
   So this needs a background prober plus a verdict cache, not a `Target` method that blocks.
   The quota rung was cheap precisely because it is a cached informer read.
2. **Each `podSet` must reference a real `core/v1` `PodTemplate` object** in the namespace, so the AGC has to create, hash-name, refresh, and garbage-collect one per worker shape, kept consistent with the pod the provisioner would actually build.
   The Q359 sizing profile mutates that shape from status, so the shape is not stable.
3. **`check-capacity` reserves, it does not merely answer.** Upstream v1 is self-contradictory (the `provisioningClassName` field doc says it does not reserve; the class constant says it reserves for a specified time) and ships `BookingExpired`/`CapacityRevoked` conditions plus a `autoscaling.x-k8s.io/consume-provisioning-request` pod annotation.
   A TTL-cached "yes" reused for *N* jobs is therefore either stale feasibility or a single-use booking spent many times, and `Provision` mode additionally has to annotate worker pods to consume what it reserved.
   The condition to read also drifts by version (`CapacityAvailable` in the field docs, absent from the v1 condition constants, which carry `Provisioned`), so a reader must tolerate both.
4. **No test estate answers a probe today, but a local one is plausible.** envtest can install the CRD and no autoscaler stamps conditions, so it needs a fake-CA condition stamper. kind runs no autoscaler *by default*, but upstream ships a [kwok cloud provider](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler/cloudprovider/kwok) whose stated purpose is running real CA against fake nodes, naming a local kind cluster as a use case — and `check-capacity` needs no cloud-provider integration at all, since [it is a scheduling simulation against existing capacity](https://github.com/kubernetes/autoscaler/blob/master/cluster-autoscaler/proposals/provisioning-request.md) that reserves nothing (provider APIs matter only for `best-effort-atomic-scale-up`).
   The negative verdict this rung acts on — *cannot place, do not claim* — therefore needs no scale-up to reproduce. **The kwok harness now exists** — Q474 shipped it as the capacity gate's live drift gate ([plan §9c](../plan/capacity-aware-intake.md#9c-the-live-autoscaler-harness-and-what-it-measured-q474), `make autoscaler-cluster`), so a cluster with a real cluster-autoscaler in it is one `--enable-provisioning-requests` away.
   Still unverified end-to-end: the kwok provider is `v1alpha1` and its docs are silent on ProvisioningRequests, so proving that pairing remains the first step of this item rather than a prerequisite it lacks a place to run.
   Provider support is a separate axis from implementation: `check-capacity` ships in the OSS cluster-autoscaler (1.30.1+, behind `--enable-provisioning-requests`), so any self-managed CA can serve it, and the gap is *managed* control planes.
   GKE serves the `ProvisioningRequest` API (`v1`, from `1.32.2-gke.1652000`) but [documents only its own `queued-provisioning.gke.io` class](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/provisioningrequest) for flex-start: GPU/TPU-oriented, one `podSet` per request, no documented `check-capacity` support as of 2026-07.
   The dogfood cluster therefore cannot validate this class today, which is exactly the provider-support half of the trigger below.
   Validation still dominates code, but a working kind harness would move most of it in-house.

Plus the ordinary surface: a new API dependency (vendor `k8s.io/autoscaler/cluster-autoscaler/apis`, or hand-roll ~120 lines of local types, which is the recommendation), startup CRD detection mirroring `RunnerSetInstalled`, RBAC on both `provisioningrequests` and `podtemplates` in the AGC markers *and* the GMC-granted tenant role, and an operator prerequisite (cluster-autoscaler run with provisioning requests enabled).

**Why deferred.** The cheaper rungs cover most of the harm at a fraction of the cost and are prerequisites rather than detours: gating on the scheduler's verdict where the cluster cannot grow, and on the autoscaler's own declination where it can, both feed the same rung this would later reuse.
Building the `ProvisioningRequest` machinery first would also mean choosing a caching and booking model with no measurement of how much residual claim-burn is actually left after rate-bounding.

**Triggers to revisit.** Any one of: an operator finds the measured residual burn of the latched gate — ~1 claim per deadline window ([plan §9h](../plan/capacity-aware-intake.md#9h-what-the-dogfood-re-run-measured-for-the-latch-q513)) — still unacceptable (the GPU/spot case, where one wasted claim per window is expensive); an operator asks with a cluster already running cluster-autoscaler with provisioning requests enabled; or provider support for `check-capacity` broadens enough that the fail-open path stops being the common one.

---

## G.17. Opt-In Auto-Retry for Flaky Jobs (beyond disruptions)

**Current behavior.** The auto-re-run machinery fires only for infrastructure disruptions — eviction, preemption, drain, hand-delete — and a genuine job failure is deliberately never re-run ([the boundary](../operations/troubleshooting.md#which-disruptions-auto-re-run-a-job-and-which-never-do)), because re-running a legitimate failure masks real breakage.
That leaves the most common CI annoyance untouched: a *flaky* test fails the run for reasons unrelated to the change, and a human clicks re-run.

**The gap this closes.** GitHub Actions has no native job-level auto-retry — the ask sits in open community discussions ([#72195](https://github.com/orgs/community/discussions/72195), [#27121](https://github.com/orgs/community/discussions/27121)) and the workarounds are marketplace step wrappers that require editing every workflow.
GAG can retry at the platform layer with the `rerun-failed-jobs` machinery it already runs for disruptions (Q503's paced retry loop, the per-run budget, the `cause`-labelled counters), keeping the "nothing in your workflows changes" property no in-workflow retry action can offer.
ARC has neither the re-run machinery nor the metrics plumbing, so this extends an existing moat.

**Prior art.** Every major CI system has a variant; none operates at the runner-platform layer for GitHub Actions:

| System | Mechanism | The idea worth farming |
|---|---|---|
| [GitLab `retry:`](https://docs.gitlab.com/ci/yaml/#retry) | per-job max 2, scoped by failure class (`runner_system_failure`, `stuck_or_timeout_failure`, …) | retry scoped to a *failure taxonomy*, not blanket |
| [Buildkite automatic retries](https://buildkite.com/docs/pipelines/configure/retry) | exit status / signal / signal-reason matchers with per-rule limits | spot-instance signals as first-class retry causes |
| [Buildkite Test Engine](https://buildkite.com/docs/test-engine/flaky-test-management) | flags a flake when the same test on the same SHA yields different results; quarantine (mute/skip) via workflows | detection from retry outcomes; quarantine as the *end state*, retry as the evidence collector |
| [CircleCI automatic reruns](https://circleci.com/docs/guides/orchestrate/automatic-reruns/) | `max-auto-reruns` 1–5, re-runs failed jobs only | bounded workflow-level auto-rerun of just the failed subset — the closest shape to what GAG would do |
| [Azure Pipelines](https://learn.microsoft.com/en-us/azure/devops/pipelines/test/flaky-test-management) | `retryCountOnTaskFailure`; VSTest reruns failed tests, marks pass-on-rerun as flaky, can suppress their effect on the build verdict | rerun-then-pass **is** the flake detector; "don't fail the build on a known flake" as a separate switch |
| [Datadog Auto Test Retries](https://docs.datadoghq.com/tests/flaky_tests/auto_test_retries/) | tracer-level retry up to 5, optional *only-known-flakes* mode, org-wide flake states + remediation flows | retry only what history says is flaky — needs a detection corpus first |
| Bazel / Argo / Tekton | `--flaky_test_attempts`, `retryStrategy` | declarative retry budgets at the orchestrator layer |

**Sketch — detection before actuation, two phases.**

1. **Flaky-job detection (no behavior change).** Record re-run-then-pass transitions per job name (the Azure/Buildkite detector) and surface them: a per-set flaky-jobs metric, K8s events, and an advisory condition.
   This is pure observability, zero masking risk, and no CI product offers it per *tenant* on shared runners.
   It also builds the history an only-known-flakes mode needs.
   Its prerequisite is a real per-job outcome, which the AGC does not read today — see the signal note below.
2. **Opt-in retry (`spec.flakyRetry` on the `RunnerSet`).** Bounded per-run budget separate from `maxEvictionRetries`, its own `cause` value on the existing counters, driven by the same paced `rerun-failed-jobs` loop.
   Candidate scoping knobs, cheapest first: max retries per run, per-window cap per set (a stampede guard, like the Q512 latch), and later a known-flakes-only mode fed by phase 1.

**Cost, including the parts that are easy to miss.**

1. **Granularity is the run, not the job.** `rerun-failed-jobs` re-runs *all* failed jobs of a concluded run.
   Test-level retry (Datadog/VSTest style) is out of reach without entering the test framework — explicitly out of scope; this is job-level.
2. **It argues with our own marketing.** The re-run boundary is documented as "a genuine failure is never re-run, re-running would mask real breakage" — the [why-gag box](../why-gag.md) says exactly this.
   The feature must be off by default, per-set opt-in, loudly metered, and framed as "retry *and* surface" — detection metrics keep the flake visible while retry buys throughput.
   Never silent.
3. **Cost attribution.** A retried job spends the tenant's own quota and compute; per-set counters must make the spend visible (the [cost-attribution doc](../operations/cost-attribution.md) would gain a flake-spend line).
4. **Failure classification is thin at the platform layer, but less thin than it looks.** The AGC polls no jobs API; its only outcome signal is the pod phase, used as a proxy (`PodFailed`→failed, else succeeded).
   One layer down there is more: `Runner.Worker` encodes the job's real `TaskResult` as exit code `100 + result`, and the wrapper flattens only 100/101 (the two GitHub concludes as `success`) to 0, passing 102 failed / 103 canceled / 104 skipped through verbatim ([`translateWorkerExitCode`](../../cmd/worker/main.go)).
   Nothing in the AGC reads `ContainerStatuses[].State.Terminated.ExitCode`.
   Reading it is the cheap prerequisite for phase 1's detector; a GitLab-style taxonomy past that still needs log or annotation heuristics.
   Phase 2 ships without matchers first: budget and scope knobs only.

**Signals, and why an explicit one from the job is a separate question.**

The three retry arms all decide from Kubernetes pod facts alone: phase, `reason: Evicted`, the `DisruptionTarget` condition, a `deletionTimestamp`.
Nothing from inside the job reaches the decision.
A tenant-supplied "this failure was flaky, retry it" verdict is a different signal class from the static per-set opt-in phase 2 sketches, and needs its own channel.
Four candidates, and only one is both expressive and safe:

| Channel | Carries | Verdict |
|---|---|---|
| Container exit code | The runner's real `TaskResult` | Free (already propagated, unread), but the runner owns it and no step can set it. Fidelity, not intent. |
| Termination message (`/dev/termination-log` → `State.Terminated.Message`) | Anything the job writes, 4 KiB | The workable one, with one hole: the log is per-container, so a step running in a workflow `container:` job writes to a different filesystem than the kubelet reads. The wrapper must also make the path writable by the job's user. |
| A `runs-on` label (e.g. `gag-flaky-retry`) | Per-job opt-in | Free, since the listener sees job labels at acquisition, but static policy rather than a per-failure verdict. |
| Pod self-annotation | Anything | Rejected. Grants tenant-controlled workflow code patch permission on its own pod. |

Two objections bound the value.
First, the moment a tenant writes a signal into their workflow, the "nothing in your workflows changes" property this enhancement is sold on is gone, and a job that can emit "retry me" can equally wrap the step in a retry loop.
What survives: an in-workflow wrapper retries steps *on the same worker*, while a platform signal re-runs the whole job on a fresh pod — new node, new network, clean runner — which is a flake class a step wrapper structurally cannot fix.
Second, `rerun-failed-jobs` is run-granular and fires only after the run concludes, so a signal from one job re-runs its siblings too; the waiting machinery exists (`rerunUntilAccepted` already holds through `errRunNotConcluded`), the fan-out is the cost.

Whichever channel wins, the writer is tenant-controlled code, often an untrusted PR's workflow.
Gating is not optional: an operator-set `RunnerSet` opt-in (a workflow author's signal must never enable retry on its own), the existing per-run budget plus a per-window cap, and its own `cause` label so the spend is attributable.

**What the isolation shapes do and do not change.** Neither Docker-in-Docker nor Kata interposes on the exit code.
In both shipped shapes `dockerd` is a nested process of the runner container or a native sidecar reachable over localhost ([Kata + DinD](../operations/kata-dind-workloads.md)), so the wrapper is still PID 1 of the runner container and still owns its exit code; what DinD changes is reaping (Q249), which is phase *timing*, not outcome fidelity.
Kata reports exit status through the ordinary `kata-agent` → shim → containerd → kubelet path.

Two things are worth stating plainly before anything is built on this:

- **Nothing has measured that a runner-produced non-zero exit reaches `PodFailed`.** `translateWorkerExitCode`'s 102 passthrough is unit-tested; the e2e sites that assert `PodFailed` are all external-kill shapes (evicted, deleted), not a job that failed on its own merits.
  A green e2e run cannot settle it either, because a *successful* job exits 0 whether or not the runtime propagates codes faithfully.
  The measurement is a single dogfood turn-up on the Kata overlay: fail a step deliberately, then read `.status.containerStatuses[0].state.terminated.exitCode` and `.status.phase`.
- **A workflow `container:` job is the real boundary**, and it splits the two channels.
  The exit code survives it, because the runner execs into the job container, reads the step status, and computes `TaskResult` itself.
  A signal *file* does not: the workspace is bind-mounted across that boundary and `/dev/termination-log` is not, so the obvious implementation vanishes silently for exactly the tenants most likely to want it.

That argues for the wrapper as the translator rather than the job writing a pod-visible file directly.
It sits on the runner-container side of every one of these boundaries, already owns the exit code, and can read a workspace-relative marker the job wrote from wherever it ran.

**Trigger.** Q555 tracks the design decision.
Promote when a design session commits the CRD shape, or an operator asks for it (the demand signal that reorders everything else on the roadmap).

---

← [Cost Model](appendix-f-cost-model.md) | [Back to index](README.md)
