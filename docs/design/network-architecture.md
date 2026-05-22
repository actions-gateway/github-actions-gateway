# Network Architecture

← [Security](05-security.md) | [Back to index](README.md)

---

This document covers the network topology of a deployed gateway: which components initiate which connections, how `NetworkPolicy` rules implement the isolation boundary, and how to validate that isolation is correctly enforced.

---

## Component Connection Map

```
  System namespace (actions-gateway-system)
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
| 3 | AGC | Proxy ClusterIP Service | HTTP CONNECT | Yes | — |
| 4 | Proxy pod | GitHub API endpoints (see below) | HTTPS (443) | No (egress) | — |
| 5 | Worker pod | Proxy ClusterIP Service | HTTP CONNECT | Yes | — |

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

The GMC creates two `NetworkPolicy` objects per tenant in the tenant namespace.

### Policy 1: Default Deny + Selective Egress for AGC and Worker Pods

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: actions-gateway-workload
  namespace: <tenant>
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/managed-by: actions-gateway
  policyTypes:
    - Ingress
    - Egress
  ingress: []  # no ingress permitted
  egress:
    # Allow AGC and worker pods to reach the proxy pool
    - to:
        - podSelector:
            matchLabels:
              app: actions-gateway-proxy
      ports:
        - port: 3128
          protocol: TCP
    # Allow AGC to reach the Kubernetes API server
    # (NO_PROXY routes these calls directly; NetworkPolicy must also permit them)
    - to:
        - namespaceSelector: {}
          podSelector:
            matchLabels:
              component: kube-apiserver
      ports:
        - port: 443
          protocol: TCP
    # Fallback for clusters where the API server is a ClusterIP Service
    # rather than a pod (e.g. managed Kubernetes, kubeadm with service proxy)
    - to:
        - ipBlock:
            cidr: <cluster-service-cidr>  # e.g. 10.96.0.0/12 (kubeadm default)
      ports:
        - port: 443
          protocol: TCP
```

The `podSelector` matches all pods created by the AGC (both the AGC Deployment and ephemeral worker pods share the `app.kubernetes.io/managed-by: actions-gateway` label). Worker pods do not call the Kubernetes API (`automountServiceAccountToken: false`), but they share the policy — the K8s API egress rule has no effect on them because they hold no credentials.

### Policy 2: Proxy Pod Egress to GitHub

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: actions-gateway-proxy-egress
  namespace: <tenant>
spec:
  podSelector:
    matchLabels:
      app: actions-gateway-proxy
  policyTypes:
    - Egress
  egress:
    # GitHub IP ranges — populated from api.github.com/meta, refreshed every 24h
    - to:
        - ipBlock:
            cidr: 192.30.252.0/22
        - ipBlock:
            cidr: 185.199.108.0/22
        - ipBlock:
            cidr: 140.82.112.0/20
        - ipBlock:
            cidr: 143.55.64.0/20
        # ... additional ranges from api.github.com/meta .actions
      ports:
        - port: 443
          protocol: TCP
```

The actual IP ranges are fetched at provisioning time and refreshed every 24 hours. The example CIDRs above are illustrative; the authoritative list is at `https://api.github.com/meta`.

### DNS Resolution

All in-cluster service discovery uses Kubernetes DNS (`kube-dns` / `CoreDNS`). The proxy pool is reachable from the AGC and worker pods via the `ClusterIP` Service name: `actions-gateway-proxy.<namespace>.svc.cluster.local`. The `NO_PROXY` env var includes `kubernetes.default.svc.cluster.local` and the cluster service CIDR so that Kubernetes API calls are never routed through the egress proxy.

External DNS resolution (for GitHub hostnames) is performed by the proxy pods themselves, not by the AGC or worker pods — the AGC and workers connect to the proxy using `CONNECT <hostname>:<port>` and the proxy resolves the hostname on their behalf. This means the proxy pods must have egress access to the cluster's DNS resolver in addition to GitHub's IP ranges. In practice, DNS egress is typically covered by the cluster's default network policy or a separate allow-all DNS rule.

---

## How to Validate Network Isolation

Run these commands from within the tenant namespace to confirm that isolation is enforced as expected.

### Confirm AGC Can Reach GitHub via Proxy

```sh
kubectl exec -n <namespace> deploy/actions-gateway-controller -- \
  curl -x $HTTPS_PROXY -sI https://api.github.com
# Expected: HTTP/2 200
```

### Confirm AGC Cannot Reach GitHub Directly (Bypassing Proxy)

```sh
kubectl exec -n <namespace> deploy/actions-gateway-controller -- \
  curl --noproxy '*' -sI --connect-timeout 5 https://api.github.com
# Expected: connection timeout or connection refused (NetworkPolicy blocks direct egress)
```

### Confirm Worker Pod Cannot Reach External Hosts Directly

```sh
# Spawn a debug pod with the worker SA (automountServiceAccountToken: false)
kubectl run nettest --image=curlimages/curl --rm -it \
  --overrides='{"spec":{"automountServiceAccountToken":false}}' \
  -n <namespace> -- \
  curl --noproxy '*' -sI --connect-timeout 5 https://api.github.com
# Expected: connection timeout (NetworkPolicy blocks direct egress from non-proxy pods)
```

### Confirm Proxy Pod Can Reach GitHub

```sh
kubectl exec -n <namespace> \
  $(kubectl get pod -n <namespace> -l app=actions-gateway-proxy -o jsonpath='{.items[0].metadata.name}') -- \
  curl -sI --connect-timeout 5 https://api.github.com
# Expected: HTTP/2 200
```

### Confirm Proxy Pod Cannot Reach K8s API Server

```sh
kubectl exec -n <namespace> \
  $(kubectl get pod -n <namespace> -l app=actions-gateway-proxy -o jsonpath='{.items[0].metadata.name}') -- \
  curl -sI --connect-timeout 5 https://kubernetes.default.svc.cluster.local
# Expected: connection timeout or no route (proxy pods have no K8s API egress rule)
```

### Confirm Cross-Tenant Isolation

From one tenant's AGC, confirm it cannot reach another tenant's proxy:

```sh
kubectl exec -n <tenant-a-namespace> deploy/actions-gateway-controller -- \
  curl -sI --connect-timeout 5 \
  http://actions-gateway-proxy.<tenant-b-namespace>.svc.cluster.local:3128
# Expected: connection refused or timeout (namespace NetworkPolicy blocks cross-tenant egress)
```

---

← [Security](05-security.md) | [Back to index](README.md)
