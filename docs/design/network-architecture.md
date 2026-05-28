# Network Architecture

← [Security](05-security.md) | [Back to index](README.md)

---

This document covers the network topology of a deployed gateway: which components initiate which connections, how `NetworkPolicy` rules implement the isolation boundary, and how to validate that isolation is correctly enforced.

---

## Component Connection Map

```
  System namespace (gmc-system)
  ─────────────────────────────────────────
  GMC ──(1)──── K8s API Server (in-cluster) ──────────────────────
                                                                   │
  Tenant namespace                                                  │
  ─────────────────────────────────────────                         │
                                                                   │
  AGC ──(2)──── K8s API Server (in-cluster, via service CIDR) ────┘
   │
   └──(3)──── Proxy ClusterIP Service ──(4)──── GitHub (external)
                      │
  Worker Pod ──(5)───┘
```

All GitHub-bound traffic — from both the AGC and worker pods — is routed through the per-tenant egress proxy pool. Kubernetes API traffic from the AGC travels directly in-cluster and bypasses the proxy.

---

## Connection Inventory

| # | Initiator | Destination | Protocol | In-cluster? | Via proxy? |
|---|-----------|-------------|----------|-------------|------------|
| 1 | GMC | K8s API server | HTTPS (443) | Yes | No |
| 2 | AGC | K8s API server | HTTPS (443) | Yes | No |
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

---

## NetworkPolicy Rules

The GMC creates three `NetworkPolicy` objects per tenant in the tenant namespace. The split (over a single combined policy) closes M-12 — worker pods are restricted to proxy + DNS egress, not the Kubernetes API server. Only the AGC Deployment has API-server egress.

Each pod is governed by **exactly one** of the workload or AGC policies — not both. The AGC carries `app: actions-gateway-controller` and is governed by Policy 2 only; worker pods carry `actions-gateway/component: workload` and are governed by Policy 1 only. Background: during PR #59's live-cluster kind dry-run, an AGC pod (then labelled with both selectors) lost its 443 egress to the kube-apiserver; removing the workload label restored access. The Kubernetes NetworkPolicy spec defines multi-NP semantics as additive and kindnet's NP implementation (kube-network-policies) appears to honour that, so the root cause is probably elsewhere — a diagnosis task is queued. In the meantime, the single-NP shape sidesteps whatever was actually breaking and is also simpler than the two-NP layout.

### Policy 1: `actions-gateway-workload` — Worker pods → proxy + DNS

Selects worker pods by the `actions-gateway/component: workload` label. Allows egress to the proxy pods (port 8080) and DNS only. AGC pods do not carry this label.

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
    - Egress
  egress:
    # DNS — needed for resolving the proxy Service name
    - ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
    # Proxy — selected by pod label, NOT by Service ClusterIP. kube-proxy DNATs
    # ClusterIP → PodIP before NetworkPolicy enforcement, so an ipBlock rule on
    # the Service IP would silently never match.
    - to:
        - podSelector:
            matchLabels:
              app: actions-gateway-proxy
      ports:
        - port: 8080
          protocol: TCP
```

### Policy 2: `actions-gateway-controller` — AGC → proxy + DNS + Kubernetes API server

Selects the AGC Deployment pods by `app: actions-gateway-controller`. Allows DNS, proxy egress (port 8080), and Kubernetes API server egress (port 443). The AGC NP intentionally includes the proxy + DNS rules that workers get from Policy 1 — AGC is not selected by Policy 1, so it must get everything it needs from this single policy.

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
    # DNS
    - ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
    # Proxy — same PodSelector form as Policy 1, for the same kube-proxy DNAT reason.
    - to:
        - podSelector:
            matchLabels:
              app: actions-gateway-proxy
      ports:
        - port: 8080
          protocol: TCP
    # Kubernetes API server — port 443 to any destination. The exact CIDR
    # depends on the cluster's apiserver placement (control-plane node IPs
    # or kubernetes Service ClusterIP); the policy allows port 443 broadly
    # rather than tracking a moving target.
    - ports:
        - port: 443
          protocol: TCP
```

### Policy 3: `actions-gateway-proxy` — Proxy pods → GitHub

Selects proxy pods by `app: actions-gateway-proxy`. Allows ingress from worker pods and from the AGC on port 8080, and egress only to GitHub IP ranges (port 443) and DNS. Because workers and the AGC carry different labels (see above), the ingress rule lists both peers explicitly.

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
    # Workers and the AGC may CONNECT to the proxy.
    - from:
        - podSelector:
            matchLabels:
              actions-gateway/component: workload
        - podSelector:
            matchLabels:
              app: actions-gateway-controller
      ports:
        - port: 8080
          protocol: TCP
  egress:
    # DNS — proxy resolves GitHub hostnames on behalf of clients
    - ports:
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

### DNS Resolution

All in-cluster service discovery uses Kubernetes DNS (`kube-dns` / `CoreDNS`). The proxy pool is reachable from the AGC and worker pods via the `ClusterIP` Service name: `actions-gateway-proxy.<namespace>.svc.cluster.local`. The `NO_PROXY` env var includes `kubernetes.default.svc.cluster.local` and the cluster service CIDR so that Kubernetes API calls are never routed through the egress proxy.

External DNS resolution (for GitHub hostnames) is performed by the proxy pods themselves, not by the AGC or worker pods — the AGC and workers connect to the proxy using `CONNECT <hostname>:<port>` and the proxy resolves the hostname on their behalf. This means the proxy pods must have egress access to the cluster's DNS resolver in addition to GitHub's IP ranges. In practice, DNS egress is typically covered by the cluster's default network policy or a separate allow-all DNS rule.

---

## How to Validate Network Isolation

The AGC and proxy container images are distroless (no shell, no curl), so `kubectl exec` against the running pods can only inspect process state, not run probes. Instead, schedule a short-lived `curlimages/curl` pod and apply the same labels as the workload you want to simulate — Kubernetes selects NetworkPolicies by label, so a curl pod with `actions-gateway/component: workload` is governed by the same rules as the AGC and worker pods.

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

---

← [Security](05-security.md) | [Back to index](README.md)
