# Network Architecture

← [Security](05-security.md) | [Back to index](README.md)

---

This document covers the network topology of a deployed gateway: which components initiate which connections, how `NetworkPolicy` rules implement the isolation boundary, and how to validate that isolation is correctly enforced.

---

## Component Connection Map

```
  System namespace (gmc-system)
  ═════════════════════════════
    GMC ──(1)──▶ K8s API Server (in-cluster) ─────────────┐
                                                          │
  Tenant namespace                                        │
  ════════════════                                        │
    AGC ──(2)──▶ K8s API Server (via service CIDR) ───────┘
     │
     └─(3)──▶ Proxy ClusterIP Service ──(4)──▶ GitHub (external)
                             ▲
    Worker Pod ──(5)─────────┘
```

In the proxied topology described here — a gateway with an attached `EgressProxy` — all GitHub-bound traffic from both the AGC and worker pods is routed through the per-tenant egress proxy pool. Kubernetes API traffic from the AGC travels directly in-cluster and bypasses the proxy. (The v2 direct-egress, proxy-less mode routes GitHub traffic straight from the pods instead of through a proxy pool; see [Proxy-less onboarding (direct egress)](../operations/tenant-onboarding.md#proxy-less-onboarding-direct-egress).)

---

## Connection Inventory

| # | Initiator | Destination | Protocol | In-cluster? | Via proxy? |
|---|-----------|-------------|----------|-------------|------------|
| 1 | GMC | K8s API server | HTTPS (443 / 6443) | Yes | No |
| 2 | AGC | K8s API server | HTTPS (443 / 6443) | Yes | No |
| 3 | AGC | Proxy ClusterIP Service | HTTPS CONNECT (8080) | Yes | — |
| 4 | Proxy pod | GitHub API endpoints (see below) | HTTPS (443) | No (egress) | — |
| 5 | Worker pod | Proxy ClusterIP Service | HTTPS CONNECT (8080) | Yes | — |

Connections (3) and (5) to the proxy are HTTPS, not plain HTTP. The GMC generates a per-tenant self-signed cert for the proxy at provisioning time and pins it into the AGC's trust store (W7 / M-5). This protects the AGC↔proxy hop from in-cluster eavesdropping or impersonation by any tenant whose pods can reach the Service ClusterIP.

The GMC also makes one additional outbound call: `GET https://api.github.com/meta` every 24 hours to refresh the GitHub IP ranges used in tenant `NetworkPolicy` egress rules. This call originates from the GMC's own egress path in the system namespace, not through any tenant's proxy pool.

### GitHub Endpoints Reached via Proxy

| Endpoint | Used by | Purpose |
|----------|---------|---------|
| `api.github.com` | AGC | GitHub App token exchange, rerun API |
| `*.actions.githubusercontent.com` | AGC | Broker API (GetMessage, AcquireJob, RenewJob) |
| `pipelines.actions.githubusercontent.com` | Worker pod | Twirp Results Service (live log streaming) |
| `objects.githubusercontent.com` | Worker pod | Action source downloads |

GitHub publishes its current IP ranges at `https://api.github.com/meta` under the `actions` key. The GMC uses this list to populate proxy pod `NetworkPolicy` egress rules and refreshes them every 24 hours. If `spec.proxy.managedNetworkPolicy` is `false`, operators are responsible for keeping egress rules current.

### Per-tenant egress IP: the source-IP mechanism

The per-tenant proxy pool establishes a per-tenant **choke point** — all of a tenant's GitHub-bound traffic (AGC control plane + worker data plane) funnels through one identifiable set of proxy pods. That is a prerequisite for the per-tenant egress-IP claim, but **it is not by itself sufficient**: what source IP GitHub observes is decided one layer lower, by how the cluster masquerades (SNATs) pod egress. On a stock cloud cluster, proxy-pod egress SNATs to the **node's** shared cloud egress IP (GKE Cloud NAT, an EKS NAT gateway, an AKS outbound rule), which is shared across tenants on that node and not stable across proxy-pod reschedules.

Delivering a **distinct, stable, per-tenant** source IP at GitHub — the property that makes GitHub-side IP allowlisting, per-tenant audit attribution, incident containment, and the per-tenant kill-switch coherent — requires an explicit **egress-IP mechanism** underneath the proxy pool: **Cilium Egress Gateway** (per-tenant egress IP via a gateway node) or **per-tenant cloud NAT** (dedicated subnet/node-pool + reserved static IP). The mechanism binds a source IP to the proxy's choke point; it does not change the proxy pool or the `NetworkPolicy` rules below, which stay orthogonal and complementary. The comparison of the two mechanisms, the single-tenant-direct (dogfood) vs production multi-tenant topology, the cost model, and the live-validation results are specified in [the per-tenant egress-IP reference architecture](../plan/q243-egress-ip-reference-arch.md) (Q243). That live validation has now landed: a real GMC-provisioned egress proxy pins to a single **distinct, stable** per-tenant Cloud NAT IP (live PASS 2026-07-13), so the distinct-per-tenant-egress-IP property is **live-substantiated**, not just designed. The dogfood cluster itself runs single-tenant-direct and does not exercise it.

---

## NetworkPolicy Rules

The GMC creates three `NetworkPolicy` objects per tenant in the tenant namespace. The split (over a single combined policy) closes M-12 — worker pods inherit egress to the proxy and DNS only, not the Kubernetes API server. Only the AGC Deployment has API-server egress.

The policies below are the v1 (`actions-gateway.github.com`) shape, whose names and selectors are fixed because v1 permits one gateway and one proxy pool per namespace. **v2 emits the same three policies with the same posture**, per-object rather than per-namespace: the workload and AGC policies are named for the gateway, the proxy policy for the `EgressProxy`, and every selector keys on the owning object's identity label (`actions-gateway.com/gateway` / `actions-gateway.com/egress-proxy`) instead of `app`. Two consequences follow from that, both load-bearing during a [v1→v2 migration](../operations/migration-v1-to-v2.md)'s coexistence window, when a namespace holds both:

- A v2 pool's pods do **not** carry `app: actions-gateway-proxy`, so v1's policy, `PodDisruptionBudget`, `HorizontalPodAutoscaler`, and hostname anti-affinity — all keyed on that one bare label — govern only v1's pool (Q582).
- The v2 workload policy's proxy egress peer selects on the *presence* of `actions-gateway.com/egress-proxy`, not one pool's name: a gateway's `RunnerSet`s may each name their own `proxyRef`, so its workload pods must reach every pool in the namespace. They cannot reach a coexisting v1 pool, which is a tightening over v1's `app`-keyed peer.

### Policy 1: `actions-gateway-workload` — AGC and worker pods → proxy + DNS

Selects all "workload" pods (AGC and worker) by the `actions-gateway/component: workload` label. Allows egress to the proxy pods (port 8080) and DNS only. Denies all ingress.

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: actions-gateway-workload
  namespace: <tenant>
spec:
  podSelector:
    matchLabels:
      actions-gateway/component: workload
  policyTypes:
    - Ingress
    - Egress
  ingress: []  # no ingress permitted
  egress:
    # DNS — needed for resolving the proxy Service name. Confined to cluster DNS,
    # not "any resolver": an open port-53 rule is an unattributed exfiltration
    # side-channel (Q105). Two OR'd peers cover both delivery paths: the kube-dns
    # / CoreDNS pods in kube-system (direct path), and the link-local block
    # 169.254.0.0/16 for NodeLocal DNSCache clusters where pods send DNS to a
    # per-node hostNetwork cache (Q136). Link-local is non-routable, so it does
    # not widen the exfil surface.
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kube-system
          podSelector:
            matchLabels:
              k8s-app: kube-dns
        - ipBlock:
            cidr: 169.254.0.0/16
      ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
    # Proxy pods — selected by PodSelector, NOT the Service ClusterIP. kube-proxy
    # DNATs ClusterIP → PodIP before NetworkPolicy enforcement, so an
    # `ipBlock: <ClusterIP>/32` rule never matches actual packets and silently
    # drops all proxy-bound traffic (the PR #59 trap). Selecting the proxy pods
    # directly matches post-DNAT destinations and survives proxy pod churn from
    # rolling updates and HPA scaling.
    - to:
        - podSelector:
            matchLabels:
              app: actions-gateway-proxy
      ports:
        - port: 8080
          protocol: TCP
```

### Policy 2: `actions-gateway-controller` — AGC → Kubernetes API server

Selects the AGC Deployment pods by `app: actions-gateway-controller`. Adds (additively) egress to the Kubernetes API server on ports 443 *and* 6443. Worker pods do not match this selector and so have no API-server egress.

Both apiserver ports are listed deliberately. NetworkPolicy port matches are evaluated against the **post-DNAT** destination port. Most production clusters expose the apiserver via the `kubernetes` Service at 443 → backends on 443, so a 443-only rule works. Kind (and any cluster where the apiserver Endpoints listen on 6443) translates ClusterIP `10.96.0.1:443` → `<node-ip>:6443`, and the policy evaluator sees 6443 — a 443-only rule silently drops every k8s API call. Allowing both ports keeps the rule precise (only apiserver-style ports) while working in both topologies. See [`docs/development/networkpolicy-port-matching.md`](../development/networkpolicy-port-matching.md) for the diagnosis and a worked repro.

By default this rule has **no destination restriction** (any-dest): the post-DNAT apiserver IP is provider-specific and not predictable at deploy time, so a portable `ipBlock` cannot be hard-coded and any-dest is the secure default (the breadth is the [§5.2](05-security.md#52-agc--proxy-level-threats-namespace-scoped) residual). Operators whose platform exposes a **stable** apiserver CIDR can opt in to scoping it: the GMC's `--apiserver-cidrs` flag (Helm value `apiServerCIDRs`) attaches an `ipBlock` peer per CIDR to this rule (ports unchanged) — an opt-in tightening, validated as CIDRs at GMC startup (Q145). Empty (the default) leaves the rule any-destination. See [security-operations.md § Tightening AGC apiserver egress](../operations/security-operations.md#tightening-agc-apiserver-egress-the-apiserver-cidrs-allowlist).

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: actions-gateway-controller
  namespace: <tenant>
spec:
  podSelector:
    matchLabels:
      app: actions-gateway-controller
  policyTypes:
    - Egress
  egress:
    # DNS — confined to cluster DNS (kube-dns / CoreDNS in kube-system) plus the
    # link-local block for NodeLocal DNSCache; see Q105/Q136.
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kube-system
          podSelector:
            matchLabels:
              k8s-app: kube-dns
        - ipBlock:
            cidr: 169.254.0.0/16
      ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
    # Kubernetes API server — ports 443 and 6443 to any destination.
    # Both ports are needed because NetworkPolicy enforcement evaluates
    # post-DNAT: production clusters typically expose the apiserver on 443,
    # kind translates Service:443 → node:6443. Allowing both works in both.
    - ports:
        - port: 443
          protocol: TCP
        - port: 6443
          protocol: TCP
```

### Policy 3: `actions-gateway-proxy` — Proxy pods → GitHub

Selects proxy pods by `app: actions-gateway-proxy`. Allows ingress only from "workload" pods on port 8080, and egress only to GitHub IP ranges (port 443) and DNS.

A bare `podSelector` peer means *this policy's own namespace*, which is what keeps a v2 `EgressProxy` same-namespace-only unless its owner shares it. When `spec.sharing.allowedNamespaces` names a namespace, the reconciler adds one more ingress rule per granted namespace (Q166, M4): each is a **single** peer carrying both a `namespaceSelector` (on `kubernetes.io/metadata.name`) and the same workload `podSelector`. The two must sit in one peer so they AND; as two entries of `from` they would OR, admitting every pod in the granted namespace *and* every workload pod cluster-wide. The consumer side gets the mirror-image egress peer, and traffic needs both. See [§H.9](appendix-h-v2-api-decomposition.md#h9-cross-namespace-proxy-sharing) and the operator runbook [security-operations.md § Sharing an egress proxy across namespaces](../operations/security-operations.md#sharing-an-egress-proxy-across-namespaces).

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: actions-gateway-proxy
  namespace: <tenant>
spec:
  podSelector:
    matchLabels:
      app: actions-gateway-proxy
  policyTypes:
    - Ingress
    - Egress
  ingress:
    # Only workload pods (AGC and workers) may CONNECT to the proxy
    - from:
        - podSelector:
            matchLabels:
              actions-gateway/component: workload
      ports:
        - port: 8080
          protocol: TCP
  egress:
    # DNS — proxy resolves GitHub hostnames on behalf of clients. Confined to
    # cluster DNS (kube-dns / CoreDNS in kube-system) plus the link-local block
    # for NodeLocal DNSCache; kube-dns recurses upstream so external names still
    # resolve, but the proxy cannot reach an arbitrary resolver — closing the
    # open-DNS exfiltration side-channel (Q105/Q136).
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kube-system
          podSelector:
            matchLabels:
              k8s-app: kube-dns
        - ipBlock:
            cidr: 169.254.0.0/16
      ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
    # GitHub IP ranges — populated from api.github.com/meta, refreshed every 24h
    - to:
        - ipBlock:
            cidr: 192.30.252.0/22
        - ipBlock:
            cidr: 185.199.108.0/22
        - ipBlock:
            cidr: 140.82.112.0/20
        # ... additional ranges from api.github.com/meta .actions
      ports:
        - port: 443
          protocol: TCP
```

The actual IP ranges are fetched at provisioning time and refreshed every 24 hours. The example CIDRs above are illustrative; the authoritative list is at `https://api.github.com/meta`.

If `spec.proxy.managedNetworkPolicy: false` is set, the GMC omits the GitHub-CIDR egress rule from Policy 3 — operators using FQDN-based egress policies (Cilium, Calico) provide their own equivalent rule and the GMC stops fighting them on every IP range refresh.

#### CNI-native FQDN egress mode (opt-in, Q208, Q245)

On a DNS-aware policy CNI an operator can have the GMC express the proxy pool's GitHub allowlist by **hostname** instead of CIDR, removing the dependency on the 24h `api.github.com/meta` feed. The choice is **split across two roles** (Q245) so the tenant API stays stable as CNI/platform FQDN mechanisms proliferate:

- **Tenant intent** — a v2 `EgressProxy` selects `spec.egressPolicyMode`:
  - `CIDR` (default) — the standard NetworkPolicy + 24h IP-range reconcile described above. Works on every CNI, needs no operator backend.
  - `FQDN` — "express my GitHub allowlist by hostname." The tenant does **not** name a CNI; the mechanism is the operator's choice.
  - `CiliumFQDN` / `CalicoFQDN` — **deprecated** aliases that pin their namesake mechanism regardless of the operator backend (retained for backward compatibility; the admission webhook warns). Removable no earlier than `v3.0.0`: they are enum members of the beta version `v2beta1`, which the `v2.0.0` removal bundle keeps serving ([why](../operations/v1alpha1-deprecation.md#a-fourth-deprecation-on-a-different-clock-ciliumfqdn--calicofqdn)).
- **Operator backend** — the GMC `--fqdn-policy-backend` flag (`none` | `cilium` | `calico` | `gke`) resolves an `FQDN` intent to a concrete emitter, once per cluster:
  - `cilium` → a `CiliumNetworkPolicy` (`cilium.io/v2`) with `toFQDNs` rules on TCP/443 plus a DNS-visibility rule so Cilium's DNS proxy learns the resolved IPs.
  - `calico` → a Calico `NetworkPolicy` (`projectcalico.org/v3`) with the GitHub hostnames as destination `domains`.
  - `gke` → a GKE Dataplane V2 `FQDNNetworkPolicy` (`networking.gke.io/v1alpha1`) with `matches` on TCP/443 (no DNS rule — DNS bypasses GKE FQDN enforcement and is carried by the base NetworkPolicy).
  - `none` (default) → **no backend**; an `FQDN`-intent `EgressProxy` is **rejected at admission**, fail-closed and loud, never a silent `Degraded`.

The GitHub hostname set is the same across backends: `api.github.com`, `github.com`, `codeload.github.com`, `objects.githubusercontent.com`, `*.actions.githubusercontent.com`, `*.blob.core.windows.net` — **plus every referring gateway's `gitHubURL` host** (Q506). That addition is what makes an FQDN mode work for GitHub Enterprise Server: the appliance's hostname lives on the referring `ActionsGateway`, not on the `EgressProxy`, so the reconciler resolves the referrer graph (gateways whose `defaultProxyRef` names the proxy, `RunnerSet`s whose `proxyRef` does, contributing their gateway's host) and feeds both the CNI policy and the CONNECT suffix list. A referrer applied after the proxy requeues it through a watch on both kinds. Hosts the public set already covers are dropped and the result is sorted, so a public-GitHub-only pool emits exactly the policy it did before. In any FQDN mode the standard NetworkPolicy keeps its DNS + ingress rules but **drops the GitHub-CIDR egress rule**, and the IP-range reconcile skips the proxy.

> **CIDR mode cannot do the same, and says so.** The default mode programs the ranges `api.github.com/meta` publishes, and a GHES appliance on the customer's own address space appears in none of them — so the NetworkPolicy denies the proxy's traffic to the one host it exists to reach. There is no code answer: the appliance's ranges are knowable only to the operator, and pointing the fetcher at a GHES `/meta` is a design question rather than a substitution (that endpoint describes the appliance's own configuration). A CIDR-mode pool with a GHES referrer and no `destinationCIDRs` therefore reports the advisory `GitHubEgressIncomplete=True` / `ApplianceRangesRequired`, naming the unreachable host and both remedies, instead of failing as an unexplained connect timeout. The GMC exports that condition as the gauge `actions_gateway_github_egress_incomplete` (Q537), so the gap is alertable fleet-wide rather than one `kubectl describe` at a time. Supplying `destinationCIDRs` clears it — the GMC takes the declaration at face value, since it cannot verify the ranges cover the appliance. Note the remedy is platform-gated (`--allowed-egress-cidrs`), so a tenant cannot self-serve it. The CNI-native object is named `<proxy>-proxy-fqdn` and is owned by the `EgressProxy` for cascade GC. The posture stays **fail-closed**: because the standard NetworkPolicy still default-denies GitHub egress, a CNI that cannot enforce the FQDN policy leaves GitHub egress *denied*, never wide-open — so the opt-in cannot silently weaken the default. Backend prerequisites (the matching CRD installed / the platform feature enabled) are the operator's responsibility, documented in [security-operations.md § Expressing GitHub egress by FQDN](../operations/security-operations.md#expressing-github-egress-by-fqdn-the-egresspolicymode-opt-in). FQDN mode is scoped to the v2 `EgressProxy`; the v1 proxy and v2 direct egress stay on the CIDR path.

> **The `gke` backend is additive-allow, not default-deny — the base NetworkPolicy is load-bearing.** A GKE `FQDNNetworkPolicy` composes with any NetworkPolicy on the same pod as a **union** (egress is allowed if it matches *either*), so on its own it denies nothing — it only *adds* an allow for the listed FQDNs. Cilium/Calico FQDN policies, by contrast, are self-default-denying. GAG's fail-closed guarantee for `gke` therefore depends on the base standard NetworkPolicy always being present (it default-denies GitHub egress with the CIDR rule dropped, and carries the DNS-only allow including the Q229 node-local-dns peer): the `FQDNNetworkPolicy` only widens the union to permit GitHub, and if it is absent or unenforced GitHub egress stays denied by the base NP. The `gke` reconcile branch is reached only when the base NP is managed, so a backend that opens egress without a default-deny base is never emitted. `gke`-backend **enforcement is not yet live-validated** on a real GKE cluster (deferred; see the [Q245 plan](../plan/q245-fqdn-intent-backend-split.md)).

> **A managed "Cilium" platform usually does NOT accept the `cilium` backend.** The CRD test is literal: `kubectl get crd ciliumnetworkpolicies.cilium.io` must succeed. **GKE Dataplane V2's managed Cilium does NOT install that CRD** (dropped since GKE 1.21.5-gke.1300) — there, use `--fqdn-policy-backend=gke`, not `cilium`. If a selected backend's CRD is absent the `EgressProxy` goes `Degraded` (`no matches for kind …`) and GitHub egress stays denied (fail-closed). The cluster-scoped backends still deferred (AKS `CiliumClusterwideNetworkPolicy` via Advanced Container Networking Services, EKS DNS-based `ClusterNetworkPolicy` on Auto Mode) and OpenShift OVN `EgressFirewall` `dnsName` need a different ownership/GC model; tracked in the [Q245 plan](../plan/q245-fqdn-intent-backend-split.md). Until a matching backend ships, use an **in-cluster caching mirror** (the recommended path for remote dependencies anyway) or `managedNetworkPolicy: false` and layer your own policy.

#### Worker egress to allowlisted non-GitHub destinations (opt-in, Q242 G.1)

By default Policy 3 permits GitHub and nothing else, so jobs that fetch off-platform build dependencies (a Go module proxy, an internal artifact host, a cloud private-API endpoint) fail on egress. A platform admin can open a small, explicit set of **non-GitHub** destinations on a v2 `EgressProxy` — by DNS host suffix (`spec.destinationFQDNs`, e.g. `proxy.golang.org`) and by CIDR (`spec.destinationCIDRs`, e.g. an internal `10.x` subnet or a Private-Google-Access range) — without forfeiting per-tenant egress-IP attribution or the DNS-exfil containment. The destinations widen two surfaces, derived from the one CR:

- **Pod egress (the hard gate).** CIDRs become native `ipBlock` peers on Policy 3 (port 443), present in **every** `egressPolicyMode` — so a CIDR destination needs no DNS-aware CNI. Host suffixes are appended to the CNI-native `toFQDNs` / `domains` set, so they **require** an FQDN mode (the CRD's CEL rule rejects `destinationFQDNs` in `CIDR` mode, since a standard NetworkPolicy can't express a hostname). GitHub stays implicit; the lists only add.
- **Proxy CONNECT check (defense-in-depth).** The GMC injects the full permitted set — the GitHub hostnames plus the operator's destinations — into the proxy via `PROXY_ALLOWED_HOST_SUFFIXES` / `PROXY_ALLOWED_CIDRS`, but **only when the CR lists at least one extra destination**; with none, the proxy stays transport-only (unchanged). The proxy matches a CONNECT host against the allowed suffixes, and for the CIDR path resolves the host and dials the **validated IP** (closing the DNS-rebinding window). A denied CONNECT returns `403` and increments `actions_gateway_proxy_connect_denied_total`.

Because the `EgressProxy` is tenant-authorable, **what** may be requested is gated by a platform-owned allowlist (`--allowed-egress-fqdns` suffix match / `--allowed-egress-cidrs` subnet containment, optionally augmented by a watched ConfigMap), enforced by an admission webhook — **both empty ⇒ deny-all-non-GitHub** (the secure default). Operators should prefer an **in-cluster caching mirror** for remote third-party dependencies and reserve the allowlist for what a mirror can't proxy; see [security-operations.md § Worker egress destinations](../operations/security-operations.md#worker-egress-destinations-the-egress-allowlist) and the threat row in [§5.2](05-security.md#52-agc--proxy-level-threats-namespace-scoped).

### DNS Resolution

All in-cluster service discovery uses Kubernetes DNS (`kube-dns` / `CoreDNS`). The proxy pool is reachable from the AGC and worker pods via the `ClusterIP` Service name: `actions-gateway-proxy.<namespace>.svc.cluster.local`. The AGC's `NO_PROXY` additionally carries `kubernetes.default.svc` **and this cluster's API server ClusterIP**, so Kubernetes API calls are never routed through the egress proxy. The ClusterIP entry is load-bearing and cannot be a DNS name: client-go's in-cluster config dials the API server by the address in `KUBERNETES_SERVICE_HOST`, never by hostname, so a DNS-only exemption leaves the AGC CONNECTing to the API server through the tenant's proxy — where it cannot verify the proxy CA and crash-loops at startup (Q465, measured on GKE). The GMC reads that ClusterIP from its own pod environment rather than assuming a Service CIDR: the first address of the Service CIDR is `10.96.0.1` on kind/kubeadm, `172.20.0.1` on EKS, `10.0.0.1` on AKS, and provider-assigned on GKE, so any hardcoded range is wrong on most clusters.

External DNS resolution (for GitHub hostnames) is performed by the proxy pods themselves, not by the AGC or worker pods — the AGC and workers connect to the proxy using `CONNECT <hostname>:<port>` and the proxy resolves the hostname on their behalf. This means the proxy pods must have egress access to the cluster's DNS resolver in addition to GitHub's IP ranges.

DNS egress on all three policies is **confined to cluster DNS** rather than left open to any resolver (Q105). An unrestricted port-53 rule (`to: []`) would let any pod smuggle data to an attacker-controlled resolver — an unattributed side-channel that bypasses the per-tenant egress-IP attribution every other egress path enforces. Confining DNS to the in-cluster resolver keeps resolution on the attributable path: `kube-dns` recurses upstream on the pod's behalf, so external GitHub names still resolve while no pod can reach an arbitrary DNS server directly.

Each DNS rule allows two OR'd peers, covering the two ways a pod reaches cluster DNS:

- **Direct path** — the `kube-dns` / `CoreDNS` Service in `kube-system`, matched by `namespaceSelector` on the well-known `kubernetes.io/metadata.name: kube-system` label plus a `podSelector` on the conventional `k8s-app: kube-dns` label.
- **NodeLocal DNSCache path** — the IPv4 link-local block `169.254.0.0/16`, matched by an `ipBlock` (Q136). On clusters running [NodeLocal DNSCache](https://kubernetes.io/docs/tasks/administer-cluster/nodelocaldns/) (`node-local-dns`), pods send DNS to a link-local address (`169.254.20.10` by the kube-standard `__PILLAR__LOCAL__DNS__` convention) served by a per-node `hostNetwork` DNSCache pod, which no pod/namespace selector can match. Allowing the whole link-local block is the simplest correct rule and **preserves Q105's attribution property**: `169.254.0.0/16` is non-routable and node-scoped, so it cannot reach an external resolver — the DNS-exfiltration channel Q105 closed stays closed.

Operators running a DNS service under a non-standard namespace or pod label must adjust the selector accordingly (or supply their own equivalent rule under `spec.proxy.managedNetworkPolicy: false`).

---

## How to Validate Network Isolation

The AGC and proxy container images are distroless (no shell, no curl), so `kubectl exec` against the running pods can only inspect process state, not run probes. Instead, schedule a short-lived `curlimages/curl` pod and apply the same labels as the workload you want to simulate — Kubernetes selects NetworkPolicies by label, so a curl pod with `actions-gateway/component: workload` is governed by the same rules as the AGC and worker pods.

> **The negative checks below only hold on a CNI that enforces egress NetworkPolicy** (Calico, Cilium, …). NetworkPolicy objects are inert without a CNI enforcer, and kind's default kindnet demonstrably does *not* drop egress for these cases — a "blocked" expectation will spuriously succeed there. Production clusters must run an egress-enforcing CNI for the workload isolation described in this document to exist at runtime. The workload-pod negatives below are automated as the cluster-only specs `E2E_GMC_TenantProvisioning_WorkloadEgressBlockedToNonProxyPod` and `E2E_GMC_TenantProvisioning_WorkerCannotReachK8sAPI`, observed enforcing on a Calico kind cluster (`make e2e-cluster KIND_CNI=calico`) on 2026-06-11 — see [the worker-egress-proxy plan](../plan/worker-egress-proxy.md#runtime-negative-case-enforcement-validated-on-calico-q7b-2026-06-11).

### Confirm a workload pod can reach GitHub via the proxy

```sh
kubectl run nettest-workload -n <namespace> --rm -it --restart=Never \
  --image=curlimages/curl:latest \
  --labels='actions-gateway/component=workload' \
  --overrides='{"spec":{"automountServiceAccountToken":false}}' \
  -- curl -x https://actions-gateway-proxy:8080 -sI https://api.github.com
# Expected: HTTP/2 200
```

### Confirm a workload pod cannot reach GitHub directly (bypassing proxy)

```sh
kubectl run nettest-workload -n <namespace> --rm -it --restart=Never \
  --image=curlimages/curl:latest \
  --labels='actions-gateway/component=workload' \
  --overrides='{"spec":{"automountServiceAccountToken":false}}' \
  -- curl --noproxy '*' -sI --connect-timeout 5 https://api.github.com
# Expected: connection timeout (actions-gateway-workload NetworkPolicy blocks direct egress)
```

### Confirm a worker-like pod cannot reach the Kubernetes API server

The `actions-gateway-controller` NetworkPolicy only matches pods labelled `app=actions-gateway-controller`, so worker pods (labelled `actions-gateway/component=workload` but not the AGC `app` label) have no API-server egress.

```sh
kubectl run nettest-worker -n <namespace> --rm -it --restart=Never \
  --image=curlimages/curl:latest \
  --labels='actions-gateway/component=workload' \
  --overrides='{"spec":{"automountServiceAccountToken":false}}' \
  -- curl --noproxy '*' -sI --connect-timeout 5 https://kubernetes.default.svc
# Expected: connection timeout
```

### Confirm nothing can open a connection *to* a worker pod (ingress default-deny)

Worker pods run untrusted job code and accept no inbound by design — the workload NP declares `policyTypes: [Ingress, Egress]` with an empty ingress rule set, so all ingress is denied (Q128). Start a workload-labelled listener, then probe it from an unrelated pod: the connection must fail.

```sh
# Listener: a workload-labelled pod serving on 8000 (simulates a worker pod).
kubectl run nettest-listener -n <namespace> --restart=Never \
  --image=python:3-alpine \
  --labels='actions-gateway/component=workload' \
  --overrides='{"spec":{"automountServiceAccountToken":false}}' \
  -- python3 -m http.server 8000
kubectl wait -n <namespace> --for=condition=Ready pod/nettest-listener

# Probe from an unlabelled pod in the same namespace.
LISTENER_IP=$(kubectl get pod nettest-listener -n <namespace> -o jsonpath='{.status.podIP}')
kubectl run nettest-prober -n <namespace> --rm -it --restart=Never \
  --image=curlimages/curl:latest \
  --overrides='{"spec":{"automountServiceAccountToken":false}}' \
  -- curl --noproxy '*' -sI --connect-timeout 5 "http://${LISTENER_IP}:8000"
# Expected: connection timeout (workload NP denies all ingress to worker pods)
kubectl delete pod nettest-listener -n <namespace>
```

### Confirm a proxy-labelled pod can reach GitHub

```sh
kubectl run nettest-proxy -n <namespace> --rm -it --restart=Never \
  --image=curlimages/curl:latest \
  --labels='app=actions-gateway-proxy' \
  --overrides='{"spec":{"automountServiceAccountToken":false}}' \
  -- curl --noproxy '*' -sI --connect-timeout 5 https://api.github.com
# Expected: HTTP/2 200
```

### Confirm a proxy-labelled pod cannot reach the K8s API server

```sh
kubectl run nettest-proxy -n <namespace> --rm -it --restart=Never \
  --image=curlimages/curl:latest \
  --labels='app=actions-gateway-proxy' \
  --overrides='{"spec":{"automountServiceAccountToken":false}}' \
  -- curl --noproxy '*' -sI --connect-timeout 5 https://kubernetes.default.svc
# Expected: connection timeout (proxy pods have no K8s API egress rule)
```

### Confirm cross-tenant isolation

From tenant A's namespace, confirm a workload-labelled pod cannot reach tenant B's proxy:

```sh
kubectl run nettest-xtenant -n <tenant-a-namespace> --rm -it --restart=Never \
  --image=curlimages/curl:latest \
  --labels='actions-gateway/component=workload' \
  --overrides='{"spec":{"automountServiceAccountToken":false}}' \
  -- curl --noproxy '*' -sI --connect-timeout 5 \
       https://actions-gateway-proxy.<tenant-b-namespace>.svc.cluster.local:8080
# Expected: connection timeout (tenant A's workload NP only allows egress to
# tenant A's own proxy ClusterIP, not arbitrary in-cluster services)
```

### Confirm the GMC manager metrics endpoint is restricted to `metrics: enabled` namespaces

The GMC manager NetworkPolicy (`networkPolicy.enabled=true`, default) admits the manager's `:8443` `/metrics` endpoint **only** from namespaces labelled `metrics: enabled`, while leaving the webhook port `:9443` open so admission keeps working. Unlike the per-tenant policies above this one keys on the *namespace* label (a `namespaceSelector`), so the probe pod's own labels are irrelevant — only the label on its namespace decides. Verified at runtime on a Calico kind cluster on 2026-06-18 (Q83) and codified as the Calico-gated cluster-only spec `Manager NetworkPolicy` (`E2E_GMC_ManagerMetricsNP_*` / `E2E_GMC_ManagerWebhookNP_AdmissionStillWorks`).

```sh
URL="https://gmc-controller-manager-metrics-service.gmc-system.svc.cluster.local:8443/metrics"

# NEGATIVE: scrape from a namespace WITHOUT the label is blocked.
kubectl create namespace np-denied
kubectl run probe -n np-denied --rm -it --restart=Never \
  --image=curlimages/curl:latest \
  -- curl -sk -o /dev/null -w 'HTTP_CODE=%{http_code}\n' --connect-timeout 10 "$URL"
# Expected: curl: (28) ... Timeout; HTTP_CODE=000 (manager NP denies the unlabelled namespace)

# POSITIVE: scrape from a namespace WITH the label reaches the endpoint.
kubectl create namespace np-allowed
kubectl label namespace np-allowed metrics=enabled
kubectl run probe -n np-allowed --rm -it --restart=Never \
  --image=curlimages/curl:latest \
  -- curl -sk -o /dev/null -w 'HTTP_CODE=%{http_code}\n' --connect-timeout 10 "$URL"
# Expected: HTTP_CODE=401 (connection allowed through the NP; 401 is the metrics
# authn layer rejecting the missing bearer token — proof the TCP/TLS path reached
# the server, not the network blocking it)
```

Admission itself proves `:9443` stays open: creating any ActionsGateway (valid or invalid) returns the validating webhook's verdict rather than a `failed calling webhook … context deadline exceeded` transport error.

---

← [Security](05-security.md) | [Back to index](README.md)
