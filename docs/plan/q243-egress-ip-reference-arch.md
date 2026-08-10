# Per-Tenant Egress-IP Reference Architecture (Cloud)

Reference architecture for how GAG delivers **per-tenant egress-IP isolation** — each tenant's GitHub traffic egressing from a distinct, stable set of source IPs — on a production cloud cluster.
It compares the two viable production mechanisms (Cilium Egress Gateway vs per-tenant cloud NAT), documents the single-tenant-direct (dogfood) vs multi-tenant (production) topology and its cost, and defines a live-validation plan for a follow-up.

**Scope of this document: design + written reference architecture, now fully live-validated.** Two live runs on throwaway GKE Dataplane V2 clusters (torn down same-session) proved the claim end-to-end:

- ✅ The **Cloud NAT mechanism is proven** ([campaign, 2026-07-07](q243-q245-q230-live-validation-campaign.md)): two tenants (distinguished by node pool + pod secondary range) egress from **distinct, stable** source IPs, stable across pod reschedule (see [Live-validation results](#live-validation-results-2026-07-07)).
- ✅ **GAG binds a real EgressProxy to its egress IP — proven live** *(2026-07-13; the design was resolved by [Q282](../STATUS.md) and asserted only in envtest until this run)*: a GMC-provisioned `EgressProxy` with `spec.scheduling` lands **both** replicas on one tenant pool → **one** Cloud NAT IP, distinct per tenant and stable across reschedule. `EgressProxy.spec.scheduling` and `ActionsGateway.spec.scheduling` carry `nodeSelector`/`tolerations`/`affinity` through to the proxy pool and AGC pods, and a supplied `podAntiAffinity` **replaces** the builder's required cross-node spread (so a single-node tenant pool no longer strands replicas in `Pending`).
  The 2026-07-07 two-IP spread is gone.
  See [Result — PASS (2026-07-13)](#result--pass-2026-07-13) and [Placement pass-through](#placement-pass-through-q282).
- 📝 **GKE mechanism corrected**: Approach B's "dedicated *subnet* per tenant" is inaccurate for GKE (all node pools share the cluster subnet).
  The working primitive is a **dedicated pod secondary range per tenant node pool + a per-range Cloud NAT + `--disable-default-snat`** (so pod IPs, not the node IP, are the NAT source).
  Corrected in [Approach B](#approach-b--per-tenant-cloud-nat).

Completes Q243 (the [STATUS](../STATUS.md) Queue row is removed on completion — this doc is the durable reference).
Was a v2beta1 (Q74) blocker alongside Q224 and [Q242](archive/q242-g1-proxy-destination-allowlist.md).

---

## Why this exists: the claim vs. the current mechanism

GAG's headline multi-tenant differentiator is per-tenant egress-IP isolation.
The executive summary states it plainly:

> Every tenant's GitHub traffic exits through a dedicated egress IP pool, enabling per-team IP allowlisting on the GitHub side and per-tenant audit attribution. — [01-executive-summary.md](../design/01-executive-summary.md)

And [§2.3](../design/02-architecture.md#23-tier-3--egress-proxy-pool) describes the Tier-3 proxy pool as "giving each tenant a distinct set of egress IPs that are never shared with other tenants."

**The gap this plan closes: the per-tenant egress *proxy pool* is a necessary prerequisite for that claim, but it is not by itself sufficient.** The proxy pool gives each tenant a per-tenant **choke point** — a single, identifiable set of pods through which *all* of that tenant's GitHub traffic (AGC control plane + worker data plane) is funnelled.
What source IP GitHub actually *sees*, however, is decided one layer lower, by how the cluster masquerades (SNATs) pod traffic on the way out of the node:

- On a stock cloud cluster, proxy-pod egress is SNATed to the **node's** external IP (GKE Cloud NAT, an EKS NAT gateway, an AKS outbound rule, or node IP masquerade).
  That node IP is **shared** across every pod on the node — including other tenants' proxy pods — and is **not stable** across a proxy pod rescheduling to a different node (HPA scale, node drain, rolling update).
- So today, absent an egress-IP mechanism, "distinct set of egress IPs per tenant" is **aspirational**: the choke point exists, but the source IP at GitHub is the cluster's shared egress IP set, not a per-tenant one.

This is visible in the existing tradeoff table ([02-architecture.md](../design/02-architecture.md#23-tier-3--egress-proxy-pool), [worker-egress-proxy.md](worker-egress-proxy.md)), which lists the chosen path as "Egress IP at GitHub: Per-tenant, stable."
That column is only true once an **egress-IP mechanism** binds each tenant's choke-point traffic to a distinct, stable source IP. **This plan specifies that mechanism.** It does not change the proxy pool; it adds the source-IP binding underneath it.

```
   What exists today (proxy pool = choke point)      What Q243 adds (source-IP binding)
   ────────────────────────────────────────────      ──────────────────────────────────
   tenant-A workers ─┐                                tenant-A proxy ─▶ egress-IP-A ─▶ GitHub
                     ├─▶ tenant-A proxy ─┐                                 (distinct, stable)
   tenant-A AGC   ───┘                   │
                                         ├─SNAT▶      tenant-B proxy ─▶ egress-IP-B ─▶ GitHub
   tenant-B workers ─┐                   │ node-IP                        (distinct, stable)
                     ├─▶ tenant-B proxy ─┘ (SHARED
   tenant-B AGC   ───┘                     across
                                           tenants,
                                           unstable)
```

---

## Current state (grounded)

What exists in the repo today, so the delta is precise:

| Layer | Current state | Source |
|---|---|---|
| **Per-tenant proxy pool** | GMC provisions a per-tenant `EgressProxy` (Deployment + ClusterIP Service + HPA + PDB + pod anti-affinity) in the tenant namespace. Stateless HTTPS `CONNECT` forwarders; no TLS termination. | [§2.3](../design/02-architecture.md#23-tier-3--egress-proxy-pool), `cmd/gmc/internal/controller/egressproxy_builder.go` |
| **Choke-point enforcement** | Three per-tenant `NetworkPolicy` objects: workload pods egress only to the proxy + DNS; proxy egresses only to GitHub CIDRs + DNS; AGC-only rule for the K8s API. Worker pods cannot egress to GitHub directly. | [network-architecture.md](../design/network-architecture.md#networkpolicy-rules) |
| **Destination allowlist** | [Q242 G.1](archive/q242-g1-proxy-destination-allowlist.md): admin-gated `destinationFQDNs`/`destinationCIDRs` on the `EgressProxy` widen the choke point to a small non-GitHub set without forfeiting attribution. | [q242-g1](archive/q242-g1-proxy-destination-allowlist.md) |
| **FQDN egress (opt-in)** | Q208 `egressPolicyMode: CiliumFQDN`/`CalicoFQDN` express the GitHub allowlist by hostname on DNS-aware CNIs. GKE Dataplane V2's managed Cilium lacks the `CiliumNetworkPolicy` CRD, so `CiliumFQDN` is unusable there (fail-closed). | [network-architecture.md](../design/network-architecture.md#cni-native-fqdn-egress-mode-opt-in-q208-q245) |
| **Source-IP binding (egress IP)** | **None.** No component assigns a per-tenant source IP; proxy egress SNATs to the node's cloud egress IP, shared across tenants and unstable across reschedules. | *(this gap)* |
| **Dogfood posture** | Single-tenant, **direct egress** — no `EgressProxy`, workers egress straight to GitHub behind the default-deny NetworkPolicy. No per-tenant egress isolation is exercised. | [gke-dogfood.md](gke-dogfood.md) |

Because dogfood runs single-tenant-direct, the production multi-tenant isolation claim is currently **un-substantiated by any running system** — the reason Q243 is a v2beta1 blocker.

---

## What "per-tenant egress IP" actually requires

Four downstream properties depend on the egress IP being per-tenant, distinct, and stable (from [worker-egress-proxy.md](worker-egress-proxy.md#why-route-worker-traffic-through-the-proxy)):

1. **GitHub-side IP allowlisting** — GHES / GitHub App IP-allowlist filters inbound by source IP.
   Only works if a tenant's traffic arrives from a *known, stable, tenant-specific* IP set.
2. **Per-tenant audit attribution** — GitHub's audit log groups by source IP; a shared node IP attributes to the cluster, not the tenant.
3. **GitHub-side incident containment** — a rate limit / abuse flag / IP ban against one tenant's IPs must not touch another tenant.
   Shared IPs collapse this.
4. **Per-tenant kill-switch** — halting one tenant's egress must be one operation on that tenant's egress path.

A mechanism satisfies Q243 iff it makes GitHub observe, for tenant *T*, a **distinct** and **stable** source-IP set *S(T)*, disjoint from *S(T′)* for every other tenant *T′*, and stable across proxy-pod reschedules.
Two production mechanisms can do this.
Neither replaces the proxy pool or the NetworkPolicy — they bind a source IP to the choke point the proxy already establishes.

---

## Approach A — Cilium Egress Gateway

Cilium's Egress Gateway SNATs traffic from selected pods, leaving the cluster through a designated **gateway node** with a configured **egress IP** ([Cilium docs](https://docs.cilium.io/en/stable/network/egress-gateway/egress-gateway/)).
Mapping tenants → egress IPs is a direct fit because the policy selects pods by namespace label.

### How it maps tenants → egress IPs

One cluster-scoped `CiliumEgressGatewayPolicy` per tenant selects that tenant's proxy pods (by the `io.kubernetes.pod.namespace` label) and pins their egress to a specific gateway node + egress IP:

```yaml
apiVersion: cilium.io/v2
kind: CiliumEgressGatewayPolicy
metadata:
  name: egress-tenant-a          # cluster-scoped: no namespace field
spec:
  selectors:
    - podSelector:
        matchLabels:
          io.kubernetes.pod.namespace: tenant-a
          app: actions-gateway-proxy      # only the proxy pool egresses externally
  destinationCIDRs:
    - 0.0.0.0/0                   # scope to GitHub CIDRs in practice
  egressGateway:
    nodeSelector:
      matchLabels:
        egress-gateway: "true"
    egressIP: 203.0.113.10        # distinct, stable per-tenant source IP
```

Selecting only `app: actions-gateway-proxy` (not all namespace pods) keeps the egress IP bound to the choke point; worker/AGC pods already reach GitHub only *through* the proxy, so their external identity follows the proxy's.

### Requirements

- **Self-managed Cilium** with the egress-gateway feature: `egressGateway.enabled=true`, `bpf.masquerade=true`, `kubeProxyReplacement=true`, and `identityAllocationMode=crd` ([Cilium docs](https://docs.cilium.io/en/stable/network/egress-gateway/egress-gateway/)).
- One or more **gateway nodes** with the per-tenant egress IPs configured on a node interface (or assigned as cloud secondary/alias IPs the node can source from).
- **Not available on GKE Dataplane V2.** GKE's managed Cilium does not expose `CiliumEgressGatewayPolicy` (it does not even install the `CiliumNetworkPolicy` CRD — the same limitation that makes Q208 `CiliumFQDN` unusable on dogfood, see [network-architecture.md](../design/network-architecture.md#cni-native-fqdn-egress-mode-opt-in-q208-q245)).
  Using this approach on GKE means a **self-managed Cilium** cluster (`--enable-dataplane-v2=false` + BYO Cilium), forfeiting DPv2's managed operations.
  On EKS it means replacing the AWS VPC CNI with self-managed Cilium (unsupported by AWS).
  On AKS it is reachable via Azure CNI Powered by Cilium / BYOCNI ([Isovalent: Cilium Egress Gateway on AKS](https://isovalent.com/blog/post/cilium-egress-gateway-aks/)).

### High-availability & scale caveats (secure-by-default relevant)

- **Single active gateway per endpoint.** Traffic from a matched pod egresses through **one** gateway node, not all — and *"changing the gateway node will break existing egress connections"* with **no automatic failover** in-flight ([Cilium docs](https://docs.cilium.io/en/stable/network/egress-gateway/egress-gateway/)).
  The gateway node is a per-tenant availability chokepoint and a SNAT port-exhaustion ceiling under high concurrent-connection fan-out (relevant given GAG's thousands-of-sessions design).
- **Policy-application delay on pod start** — a brief window where a newly-scheduled proxy pod's traffic may egress **un-SNATed** (node IP) before the egress policy applies.
  This is a *fail-open* window on the isolation property and must be called out: it means the per-tenant IP guarantee is "eventually, within seconds of pod start," not "from the first packet." **(to be validated** — measure the window and whether GitHub calls land in it.)

### Composition with the existing stack

- **Proxy pool:** unchanged.
  The egress-gateway policy binds the source IP of the proxy pods' already-funnelled traffic.
- **NetworkPolicy:** orthogonal and complementary — the three per-tenant `NetworkPolicy` objects still enforce the choke point; the egress-gateway policy only rewrites source IP for traffic that policy already permits.
- **Q242 allowlist:** unchanged.
  Widened destinations still egress from the same per-tenant gateway IP, preserving attribution for non-GitHub destinations too.

---

## Approach B — Per-tenant cloud NAT

Give each tenant a distinct cloud egress IP by routing its proxy pods through a tenant-dedicated NAT path.
On GCP this is **Cloud NAT scoped to a tenant-dedicated subnet/node-pool** with a reserved static IP pool; the equivalent on EKS is a per-tenant subnet + NAT gateway, on AKS a per-node-pool NAT gateway.

### IP-allocation model (GKE reference)

Cloud NAT is a **VPC/subnet-level** construct — it has **no native per-namespace granularity** ([GCP Cloud NAT docs](https://docs.cloud.google.com/nat/docs/ports-and-addresses)).

> **Corrected against live validation (2026-07-07).** On GKE all node pools of a cluster share the **cluster subnet** — you cannot put node pools on separate subnets in one cluster, so "a dedicated *subnet* per tenant" (the diagram below) is not how it works on a single GKE cluster.
> The validated mechanism is a **dedicated pod *secondary range* per tenant node pool** (`--pod-ipv4-range`), a **per-range Cloud NAT** (`--nat-custom-subnet-ip-ranges=SUBNET:RANGE`, one per tenant, all on one Cloud Router), and **`--disable-default-snat`** on the cluster so pods egress with their pod IP (from the tenant range) instead of being masqueraded to the shared node IP — that is what makes the per-range NAT deterministic.
> Confirmed: 3 NATs covering 3 disjoint secondary ranges of one subnet coexist on one router, giving distinct + stable per-tenant IPs ([results](#live-validation-results-2026-07-07)).
> Read "subnet" below as "pod secondary range."
> The EKS/AKS shapes (genuinely per-subnet / per-node-pool NAT) are unaffected.

Per-tenant IPs are achieved by pinning each tenant's proxy pods to a dedicated **node pool** on a dedicated **subnet**, and attaching a Cloud NAT config with a reserved static IP pool to that subnet's ranges:

```
  Tenant A                          Tenant B
  ────────                          ────────
  node pool: pool-tenant-a          node pool: pool-tenant-b
   (taint tenant=a:NoSchedule)       (taint tenant=b:NoSchedule)
  subnet:   subnet-tenant-a         subnet:   subnet-tenant-b
   pods-range :ALL ──┐               pods-range :ALL ──┐
                     ▼                                 ▼
  Cloud NAT: nat-a                  Cloud NAT: nat-b
   --nat-external-ip-pool=IP-A       --nat-external-ip-pool=IP-B
                     │                                 │
                     ▼                                 ▼
                 GitHub sees IP-A                  GitHub sees IP-B
```

- The tenant's `EgressProxy` pods (and only those — via nodeSelector/toleration to the tenant pool) schedule on `pool-tenant-a`; their egress SNATs through `nat-a`'s reserved static IP(s).
- Cloud NAT covers the pod secondary range via the `:ALL` / custom-subnet-range option so pod IPs (not just node IPs) are NATed ([DEV: Static IPs for GKE outbound via Cloud NAT](https://dev.to/lbcristaldo/static-ip-addresses-for-gke-outbound-traffic-a-practical-guide-to-cloud-nat-1ie8)).

### Scaling & cost

- **IP allocation** is per Cloud NAT gateway; multiple static IPs per tenant give SNAT port headroom (Cloud NAT allocates ports per NAT IP).
  Heavy fan-out tenants raise their IP count, not their gateway count.
- **Cost drivers:** reserved static IPs (per-IP hourly), Cloud NAT data processing per GB, and — the real cost — a **dedicated node pool per tenant** (minimum node footprint even at idle, which fights GAG's zero-idle-compute premise).
  This is the crux tradeoff vs. Approach A (see cost model below).
- **Managed HA:** Cloud NAT is a managed, horizontally-redundant service — no single gateway-node SPOF (unlike Approach A), and no in-flight-connection break on scaling.

### Composition with the existing stack

- **Proxy pool / NetworkPolicy / Q242:** all unchanged and orthogonal, same as Approach A. The NAT binds the source IP; the policies still enforce the choke point.
- **Portability:** the pattern (dedicated subnet/node-pool + managed NAT + reserved IP pool) maps cleanly to **EKS** (subnet + NAT gateway per tenant) and **AKS** (NAT gateway per node pool) — a **cloud-portable** pattern that does not depend on a specific CNI.

---

## Comparison

| Dimension | A — Cilium Egress Gateway | B — Per-tenant cloud NAT |
|---|---|---|
| **Isolation guarantee** | Distinct per-tenant egress IP via BPF SNAT; **fail-open window** on pod-start until policy applies **(to be validated)** | Distinct per-tenant egress IP via managed NAT; no policy-application window (subnet-scoped) |
| **Stability across reschedule** | Stable as long as proxy pods stay selected; egress IP fixed on gateway node | Stable — bound to subnet/node-pool, independent of which node the pod lands on |
| **HA** | **Single active gateway node per endpoint; no in-flight failover** — availability + SNAT-port chokepoint | Managed, redundant NAT; no SPOF, no in-flight break |
| **Cost per tenant** | Cheap — no dedicated node pool required; egress IPs on shared gateway node(s). Marginal cost ≈ per-tenant policy + IP | **Higher — dedicated node pool + subnet + NAT + static IPs per tenant.** Idle node floor fights zero-idle-compute |
| **Operational complexity** | Self-managed Cilium (BPF masquerade, kube-proxy replacement, CRD identity mode); GAG emits one cluster-scoped policy per tenant | Cloud-native primitives; per-tenant subnet/node-pool/NAT lifecycle wired to tenant CRUD (Terraform / cloud API), no CNI change |
| **Cloud portability** | GKE: **self-managed only** (not DPv2). EKS: replace VPC CNI (AWS-unsupported). AKS: Azure CNI Powered by Cilium / BYOCNI | **Portable** — GKE Cloud NAT, EKS NAT gateway, AKS NAT gateway; same shape, no CNI dependency |
| **Composes with proxy + NetworkPolicy + Q242** | Yes — orthogonal; binds source IP only | Yes — orthogonal; binds source IP only |
| **Granularity** | Per-namespace (per-tenant) or finer (per-pod-label) | Per-subnet/node-pool (per-tenant); finer-than-tenant needs more subnets |

### Recommendation (secure-by-default)

**No single default yet — the choice is platform-shaped, and the recommended default is the one that does not trade away an isolation or availability property on the target platform.** Concretely:

- **Prefer Approach B (per-tenant cloud NAT) as the portable, managed-HA default** where the per-tenant node-pool cost is acceptable.
  It has no fail-open pod-start window, no gateway-node SPOF, and is cloud-portable without a CNI swap — so it does not regress availability or the from-first-packet isolation property.
  Its cost (a node-pool floor per tenant) is an economic tradeoff, not a security regression.
- **Approach A (Cilium Egress Gateway)** is the better fit for **already-self-managed-Cilium** clusters and cost-sensitive high-tenant-density deployments, **provided** the pod-start fail-open window and single-active-gateway HA characteristics are validated as acceptable for the tenant's threat model.
  Per secure-by-default, A must not become the default on a platform where it silently introduces the fail-open window without operator sign-off.
- **On GKE Dataplane V2 specifically** (GAG's dogfood/reference platform), A is **not available** without abandoning DPv2 — so B is the de-facto GKE path.

The mechanism should be an **operator choice**, surfaced as an explicit, documented opt-in with the tradeoff spelled out — never a silent default that regresses isolation or availability. **The final default is a decision to confirm against the live-validation results, not to fix in this doc.**

---

## Topology: single-tenant-direct (dogfood) vs. production multi-tenant

### Single-tenant-DIRECT — what dogfood does today

```
  gag-dogfood namespace (single tenant)
  ═════════════════════════════════════
   AGC ─────┐
            ├──────────────▶ GitHub          (direct egress; default-deny NP
   worker ──┘   node SNAT / Cloud NAT         allows DNS + GitHub CIDR only)
                (cluster-shared egress IP)
```

- **No `EgressProxy`, no per-tenant egress IP.** Workers egress directly to GitHub behind the default-deny NetworkPolicy (DNS + GitHub CIDR) ([gke-dogfood.md](gke-dogfood.md)).
- **Why acceptable for dogfood:** dogfood is **single-tenant** — there is no second tenant to be isolated *from*, so the per-tenant-IP property is vacuous.
  Direct egress also avoids running a proxy pool on the small `e2-standard-2` system node, which cannot fit a proxy pool alongside GMC + AGC + Athens.
  The isolation claim is a **multi-tenant** property; dogfood exercises the runner lifecycle, not tenant isolation.

### Production MULTI-TENANT — per-tenant egress IP

```
  Cluster (egress-enforcing CNI; egress-IP mechanism A or B)
  ══════════════════════════════════════════════════════════
   tenant-a ns                          tenant-b ns
   ───────────                          ───────────
   AGC-a ──┐                            AGC-b ──┐
           ├─▶ proxy-a ──┐                      ├─▶ proxy-b ──┐
   wkr-a ──┘   (choke     │              wkr-b ─┘   (choke     │
               point)     │                          point)   │
                          ▼ egress-IP mechanism                ▼
                    ┌─────────────┐                     ┌─────────────┐
                    │ A: Cilium EG│                     │ A: Cilium EG│
                    │    node+IP-A│                     │    node+IP-B│
                    │ B: NAT-a/IP-A│                    │ B: NAT-b/IP-B│
                    └──────┬──────┘                     └──────┬──────┘
                           ▼                                   ▼
                   GitHub sees IP-A                    GitHub sees IP-B
                   (distinct, stable,                  (distinct, stable,
                    per-tenant)                         per-tenant)
```

- All of tenant A's GitHub traffic (AGC + workers, via proxy-a) egresses from IP-set A; tenant B's from IP-set B; A ∩ B = ∅.
  This is what makes GitHub-side IP allowlisting, per-tenant audit attribution, incident containment, and the per-tenant kill-switch coherent claims.

### Cost model (per tenant, order-of-magnitude, GKE)

| Item | A — Cilium Egress Gateway | B — Per-tenant cloud NAT |
|---|---|---|
| Dedicated node pool | Not required (shared gateway node[s]) | **Required** — ≥1 node idle floor per tenant |
| Reserved static egress IP(s) | 1+ per tenant (on gateway node) | 1+ per tenant (on Cloud NAT) |
| Managed NAT data processing | n/a (BPF SNAT on gateway node) | per-GB egress processed |
| Gateway-node compute | shared, amortized across tenants | n/a |
| **Dominant cost** | per-tenant IP + policy (low) | **per-tenant node-pool floor** (fights zero-idle-compute) |

The exact figures are cloud- and region-specific and are a **live-validation deliverable** — this table gives the shape (A is cheaper per tenant; B trades cost for managed HA and portability), not billable numbers.

---

## Live-validation results (2026-07-07)

Run on a throwaway GKE Standard / Dataplane V2 cluster (`gag-val`, project deleted after) — full setup in [the campaign plan](q243-q245-q230-live-validation-campaign.md).
The mechanism used was **Approach B on a single cluster**: three pod secondary ranges (`pods-default`/`pods-a`/`pods-b`) on one subnet, one node pool per tenant pinned to its range (`--pod-ipv4-range`), three per-range Cloud NAT gateways on one router, and `--disable-default-snat` so pod IPs are the NAT source.

| Property (from "what the spike must prove") | Result |
|---|---|
| **Distinct egress IPs** — 2 tenants → disjoint source IPs | ✅ tenant-A `34.24.58.224` (nat-a), tenant-B `34.26.87.185` (nat-b), disjoint, consistent over 3 runs, observed at an external IP reflector |
| **Stability across reschedule** — delete/relocate a pod, IP unchanged | ✅ pool-a pod deleted+recreated (pod IP 10.5.0.6→10.5.0.7) → still `34.24.58.224` (the IP binds to the pod *range*, not the pod) |
| **Composition intact** — base NetworkPolicy still enforces the choke point | ✅ validated alongside Q245 (base default-deny NP blocks non-allowlisted egress) |
| **GAG can bind a tenant's proxy to its egress IP** | ✅ *(was ❌; closed by Q282 — see below)* |

**The gap as found (2026-07-07).** `EgressProxy.spec` exposed no `nodeSelector`/`tolerations`/`affinity`, and `buildEgressProxyDeployment` set a **required cross-node pod anti-affinity** that *spreads* proxy pods across nodes.
Live proof: scaling one tenant's `EgressProxy` to 2 replicas landed the pods on **different** pools — `10.5.0.5` (pool-a → nat-a → `34.24.58.224`) *and* `10.6.0.6` (pool-b → nat-b → `34.26.87.185`) — so a single tenant egressed from **two** IPs.
The reference arch's Approach B assumes "the tenant's `EgressProxy` pods … via nodeSelector/toleration to the tenant pool"; that binding did not exist in the API, and the anti-affinity actively worked against it.
GAG delivered the *choke point* but not the *per-tenant IP*.

### Placement pass-through (Q282)

**Closed 2026-07-07.** [`EgressProxy.spec.scheduling`](../../api/v2beta1/egressproxy_types.go) and `ActionsGateway.spec.scheduling` (a shared `PodScheduling`: `nodeSelector`/`tolerations`/`affinity`, served at both `v2alpha1` and `v2beta1`) are plumbed into the proxy pool Deployment and the AGC Deployment ([`egressproxy_builder.go`](../../cmd/gmc/internal/controller/egressproxy_builder.go), [`actionsgateway_v2_builder.go`](../../cmd/gmc/internal/controller/actionsgateway_v2_builder.go)).
The two design points the gap raised were resolved as:

**(a) Anti-affinity composition.** A supplied `nodeAffinity`/`podAffinity` is applied *alongside* the builder's required cross-node anti-affinity, which is preserved (pinning to a pool should not disable HA *within* it).
A supplied `podAntiAffinity` — any non-nil value — **replaces** the built-in term entirely.
An explicit `podAntiAffinity: {}` therefore opts out of spreading, which is what a single-node tenant pool needs; `minReplicas: 1` is the other route.
That the apiserver preserves an empty known object as a non-nil pointer is the load-bearing fact, asserted end-to-end in envtest rather than assumed.

**(b) Governance — reversed from the original framing.** The gap note asserted placement must be "platform-owned, not tenant-set (a tenant must not self-select an egress IP)". **That was wrong**, and Q282 reversed it after review:

- **A distinct egress IP is a feature, not an escape.** Tenants want one so they do not share a rate limit or a block radius.
  Electing one's own egress path is the intended use.
- The capability is **already reachable, but the symmetry is only partial** — worth stating precisely rather than overselling. `RunnerTemplate.spec.podTemplate` is a full `PodTemplateSpec` whose reserved-field guardrail (§H.4) withholds `serviceAccountName`, `automountServiceAccountToken`, `hostPID`, `hostNetwork`, `hostIPC` — and *deliberately not* scheduling.
  In **direct**-egress mode (§H.10) worker placement already selects the egress IP, un-gated.
  In the **proxied default** it does not: NetworkPolicy forces worker traffic through the proxy, so the *proxy pool's* placement selects the IP. `EgressProxy.spec.scheduling` therefore does extend the capability to the default posture.
  Gating it would still leave the direct-egress path open — and, per the next bullet, no validating gate could be sound regardless.
- **A validating allowlist of placement values is unsound.** `affinity.nodeAffinity` supports `NotIn`/`DoesNotExist`, so "any pool except mine" is expressible and no key=value allowlist rejects it in general.
  Pinning `nodeSelector` by **mutation** *is* sound (Kubernetes ANDs it with `nodeAffinity`, and affinity can only narrow) — which is a policy-engine job, not a CRD-webhook job.

What this actually costs is **attribution**, not isolation: a CR author who retargets onto another pool's egress path makes both tenants leave via one IP.
Recorded in [05-security.md §5.2](../design/05-security.md#52-agc--proxy-level-threats-namespace-scoped), with Kyverno + Gatekeeper mutating-pin samples in [admission-policies.md § Governing where GAG pods schedule](../operations/admission-policies.md#governing-where-gag-pods-schedule) for operators who attribute by source IP and do not fully trust tenant CR authors.
The same layer is where a cluster granting tenants `pods: create` must constrain placement anyway — GAG's NetworkPolicies select GAG-labelled pods, so an unlabelled tenant-created pod is selected by none of them.

Operator recipe: [security-operations.md § Pinning a tenant's proxy pool to its egress IP](../operations/security-operations.md#pinning-a-tenants-proxy-pool-to-its-egress-ip-specscheduling).

## Re-runnable live validation

The one gate that Q282 left un-exercised is a **live** end-to-end run: with `spec.scheduling` set, does a real GMC-provisioned `EgressProxy` land *all* its replicas on one tenant node pool — so the whole pool egresses from a *single* Cloud NAT IP — rather than spreading (the 2026-07-07 two-IP result)?
Q282's fix is only asserted in envtest; envtest has no scheduler, so it can prove the emitted pod spec but not the placement + the NAT source IP end-to-end.
That needs a cloud run, and per the repo rule ("treat ✅ findings as unverified until confirmed end-to-end — actually exec the thing") it is the honest close for Q243.

[`scripts/dev/validate-egress-ip.sh`](../../scripts/dev/validate-egress-ip.sh) automates it, teardown-safe, so the run is one authorized command rather than a bespoke sequence (and re-runnable at future egress-path changes — this class of regression is exactly what unit/envtest can miss):

1. Stands up a throwaway GKE **Standard / Dataplane V2** cluster with a system pool plus two tenant node pools, each pinned to its own **pod secondary range** (`--pod-ipv4-range`), behind **per-range Cloud NAT** gateways + reserved static IPs and `--disable-default-snat` (so pod IPs, from the tenant range, are the NAT source — the mechanism already proven on 2026-07-07).
2. Installs the GMC (from `GAG_IMAGE_TAG`, which must carry Q282), then deploys one `EgressProxy` per tenant whose `spec.scheduling` pins it to the tenant pool with `affinity.podAntiAffinity: {}` (opting out of the built-in spread). `minReplicas` defaults to 2, so each pool has two pods to (dis)prove pinning.
3. **Asserts** per tenant: every proxy pod's IP is inside the tenant's pod range (no spill to the other pool), a probe pinned like the proxy egresses from the tenant's reserved static IP, the two tenants' IPs are disjoint, and the IP is unchanged after deleting a proxy pod (stability across reschedule).
4. Tears the infra down on exit (`KEEP=1` to inspect; `--teardown-only` to clean up later).
   The billable steps are gated behind a dogfood-project guard + confirmation.

**PASS** = each tenant's pool → one distinct, stable IP. **FAIL** (a pool spanning ranges / >1 egress IP, colliding IPs, or an IP that moves on reschedule) is a real Q243 regression — record it honestly, do not paper over it.
Cost/teardown/method mirror [the 2026-07-07 campaign](q243-q245-q230-live-validation-campaign.md): a few USD if torn down same-session; prefer a throwaway project deleted wholesale for atomic cost hygiene.

### Result — PASS (2026-07-13)

Ran end-to-end on a throwaway GKE Standard / Dataplane V2 cluster (a scratch project, torn down same-session). **Every assertion passed**, closing the last Q243 gate — the per-tenant egress-IP claim is now substantiated by a running system, not just design:

| Property | Result |
|---|---|
| **Distinct egress IPs** | ✅ tenant-a `35.229.44.8` (nat-a), tenant-b `34.75.214.64` (nat-b) — disjoint |
| **All proxy pods in-range** | ✅ both tenant-a replicas in `10.5.0.0/16`, both tenant-b replicas in `10.6.0.0/16` — no pool spill |
| **Egress IP == reserved NAT IP** | ✅ each tenant's live probe egressed from its own reserved static IP |
| **Stable across reschedule** | ✅ after deleting a tenant-a proxy pod, egress IP unchanged at `35.229.44.8` |

This is the live proof Q282 lacked: with `spec.scheduling` (`nodeSelector` + tolerations + `affinity.podAntiAffinity: {}`), a real GMC-provisioned `EgressProxy` lands **both** replicas on the one tenant node pool, so the pool egresses from a **single** Cloud NAT IP — the 2026-07-07 two-IP spread is gone.

**Per the repo rule ("treat ✅ findings as unverified until confirmed end-to-end"), the first real run mattered:** the harness ([`scripts/dev/validate-egress-ip.sh`](../../scripts/dev/validate-egress-ip.sh)) had never been executed against live GKE, and seven distinct defects surfaced only on contact — each gating a step deeper: private-nodes control-plane access (authorize the operator IP), the `--disable-default-snat`/`--enable-private-nodes` coupling, the GMC Deployment name (`gmc-controller-manager`), the missing conversion-webhook caBundle wiring (Q279), the `tenant=managed` namespace label the `gmc-tenant-resource-guard` ValidatingAdmissionPolicy requires, a Deployment-vs-pod label mismatch in the readiness wait, and `kubectl run -i --rm` polluting the captured egress IP.
All fixed; the script is now green and re-runnable at future egress-path changes — the regression class unit/envtest can't catch.

## Live-validation plan (original spec — retained for the deferred parts)

**The distinct/stable/composition parts above are DONE.** The parts below that were *not* exercised (Approach A's fail-open window, kill-switch, SNAT-port headroom) stay deferred.
Rationale for the original deferral: a live egress spike consumes cloud spend and would contend with / risk the shared dogfood cluster.

### What the spike must prove

1. **Distinct egress IPs.** Two tenants (namespace-a, namespace-b), each with a proxy pool and an egress-IP binding, are observed by an external IP-reflector (e.g. an owned `https://<echo>/ip` endpoint, or GitHub's received-from IP in an audit event) egressing from **disjoint** source-IP sets.
2. **Stability across reschedule.** Delete/relocate a tenant's proxy pod (force a node change); confirm the observed egress IP is unchanged.
3. **Isolation holds under the mechanism's failure mode.** For A: measure the pod-start fail-open window (does any GitHub-bound call egress un-SNATed before the policy applies?) and gateway-node-failover behavior.
   For B: confirm no cross-tenant IP bleed when pods churn.
4. **Composition is intact.** The three per-tenant `NetworkPolicy` objects still enforce the choke point (direct-to-GitHub still blocked), and a Q242 allowlisted non-GitHub destination still egresses from the per-tenant IP.
5. **Kill-switch.** Draining one tenant's egress path halts only that tenant.

### Smallest cluster to prove it

- **Two tenant namespaces** on **one** cluster is sufficient — isolation is a pairwise property; more tenants add no proof.
- **Approach A:** a self-managed-Cilium cluster (e.g.
  GKE with DPv2 disabled + BYO Cilium, or kind/self-managed) with ≥1 gateway node and 2 reserved egress IPs.
  3 small nodes (1 gateway + 2 workload) suffices.
- **Approach B:** a GKE cluster with 2 tenant node pools (1 node each), 2 dedicated subnets, 2 Cloud NAT configs, 2 reserved static IPs.
  Autopilot is unsuitable (narrower NAT/node-pool control); use Standard.
- **Teardown-on-completion** runbook (mirror [gke-dogfood.md](gke-dogfood.md)'s scale-to-zero / delete discipline) so the spike leaves no idle spend.

### Rough cost

Order-of-magnitude, **hours not days** of a 3-node small cluster + a handful of reserved IPs + minimal NAT data processing — target **< a few USD** if torn down same-session.
The follow-up should confirm exact figures as deliverable (1) above; do not commit to a number here.

### Follow-up task — DONE

The live-cloud egress-IP validation that was the actual v2beta1 gate **has run and PASSED** (2026-07-13; see [Result — PASS](#result--pass-2026-07-13)), so the Q243 Queue row is removed.
What remains is genuinely deferred and out of the critical path: Approach A's (Cilium Egress Gateway) fail-open pod-start window, gateway-node failover, kill-switch drain, and SNAT-port headroom under thousands-of-sessions fan-out — none of which apply to the validated Approach-B (per-tenant Cloud NAT) GKE path, and all of which only matter once a self-managed-Cilium deployment is in scope (see the deferred section below).

---

## Open questions (resolve during validation)

- **Default mechanism per platform** — confirm B-on-GKE / A-on-self-managed-Cilium against measured cost + the A fail-open window; encode the default only after.
- **Where the egress-IP config attaches in the API** — a field on `EgressProxy` (e.g. `spec.egressIP` / `spec.egressMechanism`), a platform-owned mapping (tenant → IP) the GMC consumes, or out-of-band infra (Terraform) the operator wires.
  Leaning platform-owned (secure-by-default: tenants must not choose their own egress IP).
  Decide with the API-graduation work (Q74).
- **Per-RunnerSet egress IPs** — whether a heavy runner group can get its own IP (finer than per-tenant).
  Out of scope until per-tenant is proven.
- **SNAT port exhaustion under thousands-of-sessions fan-out** — both mechanisms have a per-IP port ceiling; validate headroom and the IPs-per-tenant knob.

---

## References

- [01-executive-summary.md](../design/01-executive-summary.md) — the per-tenant egress-IP claim
- [02-architecture.md §2.3](../design/02-architecture.md#23-tier-3--egress-proxy-pool) — Tier-3 proxy pool
- [network-architecture.md](../design/network-architecture.md) — NetworkPolicy topology + FQDN modes
- [worker-egress-proxy.md](worker-egress-proxy.md) — why worker traffic routes through the proxy
- [q242-g1-proxy-destination-allowlist.md](archive/q242-g1-proxy-destination-allowlist.md) — destination allowlist
- [gke-dogfood.md](gke-dogfood.md) — single-tenant-direct dogfood posture
- [Cilium Egress Gateway docs](https://docs.cilium.io/en/stable/network/egress-gateway/egress-gateway/)
- [GCP Cloud NAT — IP addresses and ports](https://docs.cloud.google.com/nat/docs/ports-and-addresses)
- [Static IPs for GKE outbound via Cloud NAT](https://dev.to/lbcristaldo/static-ip-addresses-for-gke-outbound-traffic-a-practical-guide-to-cloud-nat-1ie8)
- [Isovalent — Cilium Egress Gateway on AKS](https://isovalent.com/blog/post/cilium-egress-gateway-aks/)
