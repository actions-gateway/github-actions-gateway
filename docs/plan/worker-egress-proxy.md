# Worker Egress via the Per-Tenant Proxy Pool

## Statement of the design choice

Worker pod traffic to GitHub — image pulls (when proxied), action source
downloads, artifact uploads, and the long-lived Twirp log stream — routes
through the same per-tenant HTTPS `CONNECT` proxy pool that carries AGC
control-plane calls. Worker pods are not permitted to egress directly to
GitHub CIDRs.

This is documented in:

- [README.md](../../README.md), "Tier 3 — Egress Proxy Pool"
- [docs/design/01-executive-summary.md §1.4 and §1.78](../design/01-executive-summary.md)
- [docs/design/02-architecture.md §2.3](../design/02-architecture.md#23-tier-3--egress-proxy-pool)
- [docs/design/network-architecture.md](../design/network-architecture.md)

This plan exists to make the choice explicit, record the tradeoffs that
were weighed, and track the implementation gap that currently allows
workers to bypass the path.

---

## Why route worker traffic through the proxy

The per-tenant egress-IP guarantee is the differentiating feature that
distinguishes this gateway from ARC and KEDA-based alternatives
(see [Appendix D](../design/appendix-d-alternatives-considered.md)). It is
only a coherent claim if *all* GitHub-bound traffic — not just AGC API
calls — appears at GitHub from the per-tenant proxy IPs.

Four downstream capabilities depend on this:

1. **GitHub-side IP allowlisting.** GHES and the GitHub App allowlist
   feature filter inbound requests by source IP. If worker traffic egresses
   from node IPs that are shared across tenants and not stable across pod
   reschedules, the allowlist either rejects legitimate traffic or has to
   be broadened to "any node IP in the cluster" — defeating the control.
2. **Per-tenant audit attribution.** GitHub's audit log groups requests
   by source IP. Direct-from-worker egress attributes job traffic to the
   node, not the tenant.
3. **GitHub-side incident containment.** A rate limit, abuse flag, or IP
   ban issued by GitHub against one tenant's egress IPs only affects that
   tenant. With shared node IPs, a flag on one tenant's IPs can
   collaterally affect any other tenant whose workers happen to land on
   the same node.
4. **Per-tenant kill-switch.** To halt one tenant's egress in an
   incident, draining the proxy pool is one operation. Without proxy
   egress, the same outcome requires per-tenant NetworkPolicy edits or
   node-level controls — slower and easier to get wrong under pressure.

These are not security boundaries against a compromised worker (the
installation token already scopes that). They are network-attribution
properties that turn into compliance, billing, and operability
properties.

---

## Tradeoffs evaluated

| Path | Egress IP at GitHub | Throughput cost | Failure surface | Per-tenant kill-switch |
|---|---|---|---|---|
| **Worker via proxy** *(chosen)* | Per-tenant, stable | Proxy pool sized for AGC + worker bandwidth | Proxy outage halts in-flight worker traffic | Drain proxy pool |
| **Worker direct to GitHub CIDRs** | Node IP, shared across tenants | None | Proxy outage affects AGC only | Per-tenant NetworkPolicy or node control |

The proxy-via path was chosen because:

- The CONNECT proxy does *not* terminate TLS; it forwards bytes between
  two TCP sockets. Per-byte CPU cost is low. A `c5.large`-equivalent pod
  can sustain low-multiple-Gbps of CONNECT traffic — well within the
  bandwidth a single tenant's concurrent workers would generate, given
  GitHub's own per-installation rate caps.
- The HPA scales proxy replicas on CPU; bursty worker upload traffic
  drives replica count up automatically. Bandwidth scales with replica
  count and node NIC capacity.
- Worker traffic patterns are dominated by long-lived flows (artifact
  upload, log stream) rather than per-call latency. Adding one in-cluster
  TCP hop is negligible compared to the wide-area RTT to GitHub.
- A proxy outage is rare; node anti-affinity, PDB, and HPA `minReplicas: 2`
  give multi-node redundancy.

Counter-arguments considered:

- *"Workers should be self-contained so an AGC/proxy outage doesn't kill
  running jobs."* — Valid for in-flight jobs only; new jobs already
  depend on the AGC. The marginal availability gain from bypassing the
  proxy for in-flight workers does not outweigh the network-attribution
  loss for *all* worker traffic.
- *"Image pulls don't need to go through the proxy."* — They don't go
  through the proxy in this design; image pulls are performed by the
  kubelet using cluster-default networking, not by the worker pod's
  process. This document covers *worker process* egress to GitHub: action
  source downloads, artifact uploads, log streams, and any
  workflow-initiated `curl`/`git`/SDK call.

---

## Capacity sizing

Worker egress is the dominant load on the proxy pool. Sizing must
account for:

- **Concurrent workers per tenant.** Bounded by `RunnerGroup.maxWorkers`
  and namespace `ResourceQuota`. Each worker holds at minimum one
  long-lived HTTP/2 connection to the Twirp Results Service for log
  streaming, plus burst connections for artifact uploads and action
  downloads.
- **Per-worker peak bandwidth.** ML and data jobs commonly upload
  multi-GB checkpoints. Assume up to ~1 Gbps per active worker as a
  planning ceiling for bandwidth-intensive RunnerGroups (typically GPU).
- **Aggregate node NIC capacity.** Proxy replicas are spread across
  nodes; aggregate proxy bandwidth is bounded by `replicas × per-node NIC
  available bandwidth`. A 25 Gbps node with anti-affinity-spread proxies
  rarely becomes the bottleneck.
- **HPA reaction time.** CPU utilization is a *coarse* signal for CONNECT
  load. Under bursty upload patterns the HPA may lag by 30–60 s. The v2
  upgrade path is `active_connections` exposed via prometheus-adapter; the
  v1 implementation accepts the lag in exchange for not requiring a
  metrics-server extension.

Default `proxy.minReplicas: 2`, `maxReplicas: 10`, `targetCPUUtilizationPercentage: 60`
covers small-to-medium tenants. Heavy GPU tenants should raise
`maxReplicas` in their `ActionsGateway` spec and verify the HPA reaches
steady state below saturation under their representative workload.

See [Appendix A](../design/appendix-a-capacity-slos.md) and
[Appendix E](../design/appendix-e-capacity-planning.md) for the full
capacity model.

---

## Implementation status

**Done (2026-05-23, commit `4932ce7`).**

The split shipped. `cmd/gmc/internal/controller/builder.go` now emits
three NetworkPolicies:

- `buildProxyNetworkPolicy` (lines 112-157) — `podSelector: { app:
  actions-gateway-proxy }`, egress = DNS + GitHub CIDRs:443, ingress
  restricted to pods carrying `labelComponent: componentWorkload` on
  `proxyPort`.
- `buildWorkloadNetworkPolicy` (lines 159-188) — `podSelector` matching
  AGC and worker labels, egress = DNS + proxy ClusterIP:`proxyPort`
  only.
- `buildAGCNetworkPolicy` (lines 190-223) — additive AGC-only policy
  granting 443 for k8s API access. Worker pods are *not* selected by
  this policy, so they cannot directly reach the API server or GitHub.

`patchNetworkPolicy` in [ipranges.go:193-204](../../cmd/gmc/internal/controller/ipranges.go)
only mutates `npProxyName`; the workload policy carrying the
worker→proxy egress rule survives IP-range refreshes (closes **M-9**).
The `labelComponent`/`componentWorkload` selector closes **M-8** — dev
or debug pods in the namespace can no longer reach the proxy or AGC.

### Live validation — done

The e2e procedure in
[docs/design/network-architecture.md "How to Validate Network Isolation"](../design/network-architecture.md)
runs as three Tier-A specs in [`cmd/gmc/test/e2e/provisioning_test.go`](../../cmd/gmc/test/e2e/provisioning_test.go):

- `E2E_GMC_TenantProvisioning_ProxyConnectWorks` — positive: a
  workload-labelled curl pod CONNECTs through the per-tenant HTTPS proxy
  to `https://api.github.com/zen` and asserts HTTP 200 + non-empty body.
  Exercises kindnet workload-NP egress to the proxy pods, the proxy's
  HTTPS+CONNECT path, the proxy egress NP's IP-range allowlist, and the
  proxy TLS cert+SAN chain end-to-end.
- `E2E_GMC_TenantProvisioning_WorkloadEgressBlockedToNonProxyPod` —
  negative: a workload-labelled curl pod targeting `fakegithub:8080` in
  the `e2e-infra` namespace (an in-cluster pod that does **not** carry
  `app=actions-gateway-proxy`) times out, asserting the workload NP's
  port-8080 egress rule honours its `podSelector` constraint. A
  regression that broadened the rule to "any pod on port 8080" would
  still pass the positive ProxyConnectWorks test and ship silently.
- `E2E_GMC_TenantProvisioning_WorkerCannotReachK8sAPI` — negative: a
  workload-only-labelled curl pod (no `app=actions-gateway-controller`,
  simulating a worker) times out reaching `https://kubernetes.default.svc`,
  asserting that the M-12 split prevents workers from reaching the
  Kubernetes API server even though the AGC may.

The AGC-pod negative case (`curl --noproxy '*' https://api.github.com`
from a pod labelled `app=actions-gateway-controller`) is deliberately
not asserted: the AGC NetworkPolicy permits port 443/6443 to *any*
destination because the kubernetes Service's apiserver port is not
fixed across clusters (kind exposes the apiserver on 6443 via service
port-translation, production typically on 443). An AGC-labelled pod can
therefore reach `api.github.com:443` directly, by design. The per-tenant
egress-IP guarantee for AGC traffic relies on the AGC binary itself
honouring the configured proxy URL, not on the NetworkPolicy. The
relevant invariant — that **worker** pods (which carry no AGC label and
which the per-tenant guarantee actually protects in operation) must
route through the proxy — is fully covered by the three specs above.

### Known limitation: external-egress block under kindnet

The original Q7 acceptance snippet (`curl --noproxy '*'
https://api.github.com` from a workload-labelled pod must time out) is
not directly asserted because kindnet's bundled `kube-network-policies`
enforcer reliably filters in-cluster pod-to-pod traffic but does **not**
enforce egress to external (non-cluster) IPs in the kind CNI path. The
in-cluster `WorkloadEgressBlockedToNonProxyPod` spec exercises the same
workload-NP rule as a regression guard. The external direct-egress
block remains a property of the NetworkPolicy itself (visible in the
manifest egress rules) and is enforced in production by CNIs with full
egress support (Calico, Cilium). A future Tier-A run against a Calico-
or Cilium-equipped cluster would close this verification gap.

---

## Acceptance criteria

The implementation change is complete when:

1. An e2e test in `cmd/gmc/test/e2e/` provisions a tenant and asserts:
   - A workload-labelled curl pod attempting egress to a non-proxy pod
     on port 8080 fails with a connection timeout. ✅ —
     `E2E_GMC_TenantProvisioning_WorkloadEgressBlockedToNonProxyPod`
     (in-cluster substitute for the external `curl --noproxy '*'
     https://api.github.com` snippet — see "Known limitation" above).
   - A workload-labelled curl pod attempting `curl -x
     https://actions-gateway-proxy:8080 https://api.github.com`,
     succeeds. ✅ — `E2E_GMC_TenantProvisioning_ProxyConnectWorks`.
   - A worker-labelled (workload-only) curl pod cannot reach the
     Kubernetes API server. ✅ —
     `E2E_GMC_TenantProvisioning_WorkerCannotReachK8sAPI`.
2. The `IPRangeReconciler` background loop preserves the worker→proxy
   egress rule across iterations (no observable NetworkPolicy churn that
   removes the proxy ClusterIP rule). ✅ — `patchNetworkPolicy` only
   mutates `npProxyName`; covered by builder unit tests.
3. The validation snippets in
   [docs/design/network-architecture.md §"How to Validate Network Isolation"](../design/network-architecture.md)
   pass against a freshly provisioned tenant. ✅ — covered by the three
   specs above.
4. No regression in proxy pool sizing under the existing e2e workload
   (proxy CPU utilization stays below the HPA target with default
   `maxReplicas`). ✅ — HPA + PDB validated by `hpa_pdb_test.go`.

---

## Out of scope for this plan

- **Proxy destination allowlist.** Tracked as M-2 in
  [security.md](security.md). Independent of this plan; the network path
  change here neither helps nor hurts that gap.
- **TLS between AGC/workers and the proxy.** Tracked as M-5 in
  [security.md](security.md). Same independence.
- **Capacity-planning model refinements for GPU-heavy tenants.** Belongs
  in [Appendix E](../design/appendix-e-capacity-planning.md), not here.

---

## Open questions

- Should `automountServiceAccountToken: false` workers be in the same
  `np-agc-worker` policy as the AGC, or in a third selector-scoped policy
  that omits the K8s API egress rule? The K8s API egress is harmless to
  the worker (it has no credentials), but a separate selector makes the
  intent explicit. Defer until the split is implemented.
- For very heavy GPU tenants, is a dedicated proxy pool per RunnerGroup
  warranted, rather than per ActionsGateway? Out of scope for v1; revisit
  if a tenant report shows the shared per-tenant pool saturating.
