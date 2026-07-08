# Live-GKE Egress-Validation Campaign — Q243 + Q245 + Q230

> **Status (2026-07-07): IN PROGRESS.** One coordinated live-GKE campaign on ONE
> throwaway Dataplane V2 cluster, batching three deferred live validations that
> share a platform and a theme (egress on GKE DPv2). Each validation's result is
> recorded here and folded back into its own plan doc; the cluster is torn down
> same-session. This doc is kept current as the campaign runs.

Batches the deferred live-validation residuals of:

- **[Q243](q243-egress-ip-reference-arch.md)** — per-tenant egress-IP reference
  arch; residual = live proof that 2 tenants egress from **distinct, stable**
  source IPs.
- **[Q245](q245-fqdn-intent-backend-split.md)** — the `gke` `--fqdn-policy-backend`
  shipped (impl+envtest, #576); residual = live GKE `FQDNNetworkPolicy`
  **enforcement** validation.
- **[Q230](../operations/troubleshooting.md#dns-times-out-under-the-egress-networkpolicy-gke-dataplane-v2--nodelocal-dnscache)**
  — DNS-resolves-under-egress-NP on DPv2 + NodeLocal DNSCache (the Q229 fix);
  currently Deferred, its trigger fired by greenlighting this campaign.

## Why one cluster

Q245 and Q230 both **require** GKE Dataplane V2 (managed Cilium) with
`--enable-fqdn-network-policy` and NodeLocal DNSCache. Q243 needs an egress-IP
mechanism; the [reference arch](q243-egress-ip-reference-arch.md) offers two:

- **Approach B — per-tenant cloud NAT** — works on managed/DPv2 GKE. Chosen, so
  **one DPv2 cluster serves all three validations.**
- **Approach A — Cilium Egress Gateway** — needs self-managed Cilium, i.e. DPv2
  **disabled** → a *second* cluster. Rejected for this campaign to bound cost:
  Approach B demonstrates the reference arch's headline claim (distinct, stable
  per-tenant egress IP) on the same DPv2 cluster the other two need. Approach A's
  distinct properties (fail-open pod-start window, single-gateway HA) are already
  documented as tradeoffs in the reference arch and do not need a live spike to
  confirm the *per-tenant-IP* claim. If a future task wants Approach A validated
  live, it is a separate self-managed-Cilium spike (documented two-cluster cost).

## Topology

One GKE **Standard zonal** cluster, Dataplane V2, `us-east1-b` (same region as
dogfood — known-good quota), in a **dedicated throwaway GCP project** deleted
wholesale at teardown (atomic cost hygiene — no orphaned static IPs / NAT /
subnets). **NOT** the dogfood cluster/project (`gag-dogfood` /
`actions-gateway-dogfood`, prod-classified by `.claude/prod-guard.json`).

```
  validation cluster (GKE Standard, Dataplane V2 + FQDN NP + NodeLocal DNSCache)
  ═════════════════════════════════════════════════════════════════════════════
   default-pool (system): GMC control plane + CRDs
   pool-tenant-a  ── subnet-a ── Cloud NAT nat-a ── static IP-A ─▶ GitHub/echo
   pool-tenant-b  ── subnet-b ── Cloud NAT nat-b ── static IP-B ─▶ GitHub/echo
```

- Cluster flags: `--enable-dataplane-v2 --enable-fqdn-network-policy
  --addons=NodeLocalDNS --enable-ip-alias` (Standard, not Autopilot — Approach B
  needs node-pool/subnet/NAT control).
- Small: `e2-standard-2`, 1 node system pool; 1 node each tenant pool.

## Prerequisites — what to build, what to skip

**Only the GMC control plane is needed** — no AGC, RunnerSet, workers, GitHub
App, Athens, or job execution. The validations probe NetworkPolicy /
FQDNNetworkPolicy enforcement and NAT source-IP with **labeled test pods**, not
runners.

- **GMC image**: the dogfood image (`0ef4c6fa`, at #529 = Q245 *design*)
  **predates #576** (the `gke` backend impl), so build+push a GMC image from
  **HEAD** (has #576 + the Q229 DNS fix): `docker buildx bake gmc --set
  '*.platform=linux/amd64' --set gmc.tags=ghcr.io/actions-gateway/gmc:<sha>`,
  then `gh auth refresh -s write:packages` + push. The `gmc` package already
  exists public, so a new tag stays public (no visibility flip needed).
- **CRDs**: from HEAD (matches the GMC image), applied `helm template | kubectl
  apply --server-side` (v2 CRD chart >1 MiB — Q276/Q277).
- **proxy image** (only for Q243 real proxy pods): reuse the already-public
  `ghcr.io/actions-gateway/proxy:0ef4c6fa` (proxy behavior unchanged for this
  purpose).

## Feasibility findings that shape the method (discovered pre-flight)

- **`EgressProxySpec` has no `nodeSelector`/`tolerations`/`affinity`**, and the
  builder sets a **required cross-node pod anti-affinity that *spreads* proxy
  pods across nodes** (`egressproxy_builder.go`). So the CR **cannot** be pinned
  to a tenant's node pool/subnet — and its anti-affinity actively spreads it
  across pools, making its NAT egress IP non-deterministic. The
  [reference arch Approach B](q243-egress-ip-reference-arch.md#approach-b--per-tenant-cloud-nat)
  *assumes* "the tenant's EgressProxy pods … via nodeSelector/toleration to the
  tenant pool" — **the API can't express that today.** → **Q243 method:**
  validate the cloud-NAT *mechanism* (distinct + stable IP per subnet) with a
  pinned representative pod, AND record the API-integration gap as a follow-up
  (add scheduling constraints to `EgressProxy`, or a platform-owned tenant→pool
  mapping). Also run a real GMC-provisioned `EgressProxy` to show its pods land
  arbitrarily (concretely demonstrating the gap).

## Sequencing (on the one cluster)

1. **Stand up** — throwaway project + billing + APIs; create cluster (DPv2 + FQDN
   NP + NodeLocal DNSCache); build+push GMC image; install CRDs + GMC
   (`--fqdn-policy-backend=gke`); create tenant namespaces.
2. **Q230** first (cheapest, no NAT): base egress NP + labeled test pod; confirm
   DNS resolves under the NP on DPv2 + NodeLocal DNSCache (Q229 fix holds live).
3. **Q245**: EgressProxy `egressPolicyMode: FQDN`; confirm `FQDNNetworkPolicy`
   emitted + **enforces** (GitHub reachable by name; base default-deny NP still
   blocks all other egress — additive-allow invariant); confirm fail-closed if
   the feature/CRD absent.
4. **Q243** last (most infra): 2 subnets + 2 NATs + 2 static IPs + 2 tenant
   pools; observe distinct + stable egress IP per tenant at an echo endpoint,
   stable across pod reschedule.
5. **Record + tear down + STATUS.**

## Cost estimate

Order-of-magnitude, **hours not days**: 2nd zonal cluster mgmt fee (~$0.10/hr),
~3 `e2-standard-2` nodes (~$0.20/hr), 2 Cloud NAT gateways (~$0.09/hr) + trivial
data, 2 in-use static IPs (~negligible). **≈ $0.40–0.60/hr → a few USD** for a
same-session run. Left running: ~$10–14/day — hence same-session teardown.

## Teardown

`gcloud projects delete <throwaway-project>` — atomic; removes cluster, pools,
subnets, NATs, and reserved IPs in one step. Confirm the project ID is the
throwaway (NOT `actions-gateway-dogfood`) before deleting.

## Validation methods & pass/fail

### Q230 — DNS under egress NP (DPv2 + NodeLocal DNSCache)

- **Setup:** tenant ns with a GMC-managed base egress NP (via an EgressProxy or
  the tenant default); a test pod labeled as a workload (matched by the NP
  podSelector).
- **Assert:** confirm `node-local-dns` pods exist (`k8s-app=node-local-dns`,
  `kube-system`); the NP's DNS egress rule carries the 3rd `node-local-dns` peer;
  DNS resolves from the governed pod (`nslookup github.com`) while a
  non-allowlisted **non-DNS** egress is denied.
- **PASS** = DNS resolves under the NP; **FAIL** = times out (Q229 regression).
- **Decision to record:** is an automated GKE-DPv2 CI lane worth building vs
  staying deferred (do NOT build a full GKE-in-CI lane here unless cheap).

### Q245 — gke `FQDNNetworkPolicy` enforcement

- **Setup:** EgressProxy `egressPolicyMode: FQDN`, GMC `--fqdn-policy-backend=gke`.
- **Assert:** `FQDNNetworkPolicy` (`networking.gke.io/v1alpha1`) emitted + owned;
  from a governed pod, GitHub host (e.g. `api.github.com:443`) reachable; a
  **non-allowlisted** host blocked by the base default-deny NP (additive-allow
  invariant: FQDN policy widens, never replaces default-deny). Confirm
  fail-closed if `--enable-fqdn-network-policy`/CRD absent (egress stays denied).
- **PASS** = GitHub allowed, everything else denied, base NP still fail-closed;
  **FAIL** = non-GitHub egress leaks, or GitHub blocked, or not fail-closed.

### Q243 — per-tenant egress IP (cloud NAT)

- **Setup:** 2 tenant pools on 2 subnets, each with its own Cloud NAT + reserved
  static IP; a pinned representative egress pod per tenant (nodeSelector to its
  pool). Echo endpoint reflecting source IP (e.g. an owned `/ip` or a public
  reflector).
- **Assert:** tenant-A egresses from IP-A, tenant-B from IP-B, A ≠ B (distinct);
  delete/relocate a pod → IP unchanged (stable). Also apply a real
  GMC-provisioned EgressProxy and record where its (unpinnable) pods land.
- **PASS** = distinct + stable per-tenant IPs via NAT + documented API-pinning
  gap; **FAIL** = shared or unstable IPs.

**If any validation fails, that is a real security/correctness finding — record
it honestly here and file a follow-up; do not paper over it (secure-by-default).**

## Findings log

_(filled in as the campaign runs)_

| Validation | Result | Evidence |
|---|---|---|
| Q230 DNS under NP | ⏳ | |
| Q245 FQDN enforcement | ⏳ | |
| Q243 per-tenant egress IP | ⏳ | |
