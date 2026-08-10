# Live-GKE Egress-Validation Campaign — Q243 + Q245 + Q230

> **Status (2026-07-07): COMPLETE — cluster torn down.** One coordinated live-GKE campaign on ONE throwaway Dataplane V2 cluster, batching three deferred live validations that share a platform and a theme (egress on GKE DPv2). **Results: Q245 PASS, Q230 PASS, Q243 mechanism PASS + API-pinning gap found (follow-up filed).** Evidence below; folded back into each item's own plan doc + STATUS.
> The throwaway project `gag-egress-val-260707` was deleted same-session (atomic teardown — cluster, node pools, subnets, Cloud NATs, static IPs, router).

## Cluster as built (for reproducibility)

- Project `gag-egress-val-260707` (throwaway, deleted); cluster `gag-val`, zone `us-east1-b`, GKE `1.35.5-gke.1241004`, Standard, Dataplane V2.
- Flags: `--enable-dataplane-v2 --enable-fqdn-network-policy --addons=...,NodeLocalDNS --enable-private-nodes --disable-default-snat` (private nodes + `--disable-default-snat` so **pod IPs** are the Cloud NAT source, enabling per-pod-range → per-IP mapping; Private Google Access on the subnet so nodes bootstrap before NAT exists).
- Custom VPC `gag-val-net` / subnet `gag-val-subnet` (10.0.0.0/22) with **three pod secondary ranges**: `pods-default` (10.4/16), `pods-a` (10.5/16), `pods-b` (10.6/16), plus `svc`.
  Node pools: `default-pool` (GMC), `pool-a` (`--pod-ipv4-range=pods-a`), `pool-b` (`--pod-ipv4-range=pods-b`).
- One Cloud Router + **three Cloud NAT gateways** on it, each scoped to a disjoint range (`nat-default` = primary+pods-default → 34.23.242.48; `nat-a` = pods-a → 34.24.58.224; `nat-b` = pods-b → 34.26.87.185). **Finding:** three NATs on one router covering disjoint secondary ranges of one subnet is accepted by GCP — this is the single-cluster per-tenant-IP mechanism (the reference arch's "dedicated subnet per tenant" is inaccurate for GKE, which puts all node pools on the cluster subnet; the accurate primitive is per-pod-secondary-range NAT + `--disable-default-snat`).
- GMC built from HEAD (`daf977b`, has #576 gke backend + Q229 DNS fix), pushed to `ghcr.io/actions-gateway/gmc`, installed `--fqdn-policy-backend=gke`.
  Proxy image reused `proxy:0ef4c6fa`.
  No AGC/RunnerSet/workers/GitHub-App needed.

Batches the deferred live-validation residuals of:

- **[Q243](q243-egress-ip-reference-arch.md)** — per-tenant egress-IP reference arch; residual = live proof that 2 tenants egress from **distinct, stable** source IPs.
- **[Q245](q245-fqdn-intent-backend-split.md)** — the `gke` `--fqdn-policy-backend` shipped (impl+envtest, #576); residual = live GKE `FQDNNetworkPolicy` **enforcement** validation.
- **[Q230](../operations/troubleshooting.md#dns-times-out-under-the-egress-networkpolicy-gke-dataplane-v2--nodelocal-dnscache)** — DNS-resolves-under-egress-NP on DPv2 + NodeLocal DNSCache (the Q229 fix); currently Deferred, its trigger fired by greenlighting this campaign.

## Why one cluster

Q245 and Q230 both **require** GKE Dataplane V2 (managed Cilium) with `--enable-fqdn-network-policy` and NodeLocal DNSCache.
Q243 needs an egress-IP mechanism; the [reference arch](q243-egress-ip-reference-arch.md) offers two:

- **Approach B — per-tenant cloud NAT** — works on managed/DPv2 GKE.
  Chosen, so **one DPv2 cluster serves all three validations.**
- **Approach A — Cilium Egress Gateway** — needs self-managed Cilium, i.e. DPv2 **disabled** → a *second* cluster.
  Rejected for this campaign to bound cost: Approach B demonstrates the reference arch's headline claim (distinct, stable per-tenant egress IP) on the same DPv2 cluster the other two need.
  Approach A's distinct properties (fail-open pod-start window, single-gateway HA) are already documented as tradeoffs in the reference arch and do not need a live spike to confirm the *per-tenant-IP* claim.
  If a future task wants Approach A validated live, it is a separate self-managed-Cilium spike (documented two-cluster cost).

## Topology

One GKE **Standard zonal** cluster, Dataplane V2, `us-east1-b` (same region as dogfood — known-good quota), in a **dedicated throwaway GCP project** deleted wholesale at teardown (atomic cost hygiene — no orphaned static IPs / NAT / subnets). **NOT** the dogfood cluster/project (`gag-dogfood` / `actions-gateway-dogfood`, prod-classified by `.claude/prod-guard.json`).

```
  validation cluster (GKE Standard, Dataplane V2 + FQDN NP + NodeLocal DNSCache)
  ═════════════════════════════════════════════════════════════════════════════
   default-pool (system): GMC control plane + CRDs
   pool-tenant-a  ── subnet-a ── Cloud NAT nat-a ── static IP-A ─▶ GitHub/echo
   pool-tenant-b  ── subnet-b ── Cloud NAT nat-b ── static IP-B ─▶ GitHub/echo
```

- Cluster flags: `--enable-dataplane-v2 --enable-fqdn-network-policy --addons=NodeLocalDNS --enable-ip-alias` (Standard, not Autopilot — Approach B needs node-pool/subnet/NAT control).
- Small: `e2-standard-2`, 1 node system pool; 1 node each tenant pool.

## Prerequisites — what to build, what to skip

**Only the GMC control plane is needed** — no AGC, RunnerSet, workers, GitHub App, Athens, or job execution.
The validations probe NetworkPolicy / FQDNNetworkPolicy enforcement and NAT source-IP with **labeled test pods**, not runners.

- **GMC image**: the dogfood image (`0ef4c6fa`, at #529 = Q245 *design*) **predates #576** (the `gke` backend impl), so build+push a GMC image from **HEAD** (has #576 + the Q229 DNS fix): `docker buildx bake gmc --set '*.platform=linux/amd64' --set gmc.tags=ghcr.io/actions-gateway/gmc:<sha>`, then `gh auth refresh -s write:packages` + push.
  The `gmc` package already exists public, so a new tag stays public (no visibility flip needed).
- **CRDs**: from HEAD (matches the GMC image), applied `helm template | kubectl apply --server-side` (v2 CRD chart >1 MiB — Q276/Q277).
- **proxy image** (only for Q243 real proxy pods): reuse the already-public `ghcr.io/actions-gateway/proxy:0ef4c6fa` (proxy behavior unchanged for this purpose).

## Feasibility findings that shape the method (discovered pre-flight)

- **`EgressProxySpec` has no `nodeSelector`/`tolerations`/`affinity`**, and the builder sets a **required cross-node pod anti-affinity that *spreads* proxy pods across nodes** (`egressproxy_builder.go`).
  So the CR **cannot** be pinned to a tenant's node pool/subnet — and its anti-affinity actively spreads it across pools, making its NAT egress IP non-deterministic.
  The [reference arch Approach B](q243-egress-ip-reference-arch.md#approach-b--per-tenant-cloud-nat) *assumes* "the tenant's EgressProxy pods … via nodeSelector/toleration to the tenant pool" — **the API can't express that today.** → **Q243 method:** validate the cloud-NAT *mechanism* (distinct + stable IP per subnet) with a pinned representative pod, AND record the API-integration gap as a follow-up (add scheduling constraints to `EgressProxy`, or a platform-owned tenant→pool mapping).
  Also run a real GMC-provisioned `EgressProxy` to show its pods land arbitrarily (concretely demonstrating the gap).

## Sequencing (on the one cluster)

1. **Stand up** — throwaway project + billing + APIs; create cluster (DPv2 + FQDN NP + NodeLocal DNSCache); build+push GMC image; install CRDs + GMC (`--fqdn-policy-backend=gke`); create tenant namespaces.
2. **Q230** first (cheapest, no NAT): base egress NP + labeled test pod; confirm DNS resolves under the NP on DPv2 + NodeLocal DNSCache (Q229 fix holds live).
3. **Q245**: EgressProxy `egressPolicyMode: FQDN`; confirm `FQDNNetworkPolicy` emitted + **enforces** (GitHub reachable by name; base default-deny NP still blocks all other egress — additive-allow invariant); confirm fail-closed if the feature/CRD absent.
4. **Q243** last (most infra): 2 subnets + 2 NATs + 2 static IPs + 2 tenant pools; observe distinct + stable egress IP per tenant at an echo endpoint, stable across pod reschedule.
5. **Record + tear down + STATUS.**

## Cost estimate

Order-of-magnitude, **hours not days**: 2nd zonal cluster mgmt fee (~$0.10/hr), ~3 `e2-standard-2` nodes (~$0.20/hr), 2 Cloud NAT gateways (~$0.09/hr) + trivial data, 2 in-use static IPs (~negligible). **≈ $0.40–0.60/hr → a few USD** for a same-session run.
Left running: ~$10–14/day — hence same-session teardown.

## Teardown

`gcloud projects delete <throwaway-project>` — atomic; removes cluster, pools, subnets, NATs, and reserved IPs in one step.
Confirm the project ID is the throwaway (NOT `actions-gateway-dogfood`) before deleting.

## Validation methods & pass/fail

### Q230 — DNS under egress NP (DPv2 + NodeLocal DNSCache)

- **Setup:** tenant ns with a GMC-managed base egress NP (via an EgressProxy or the tenant default); a test pod labeled as a workload (matched by the NP podSelector).
- **Assert:** confirm `node-local-dns` pods exist (`k8s-app=node-local-dns`, `kube-system`); the NP's DNS egress rule carries the 3rd `node-local-dns` peer; DNS resolves from the governed pod (`nslookup github.com`) while a non-allowlisted **non-DNS** egress is denied.
- **PASS** = DNS resolves under the NP; **FAIL** = times out (Q229 regression).
- **Decision to record:** is an automated GKE-DPv2 CI lane worth building vs staying deferred (do NOT build a full GKE-in-CI lane here unless cheap).

### Q245 — gke `FQDNNetworkPolicy` enforcement

- **Setup:** EgressProxy `egressPolicyMode: FQDN`, GMC `--fqdn-policy-backend=gke`.
- **Assert:** `FQDNNetworkPolicy` (`networking.gke.io/v1alpha1`) emitted + owned; from a governed pod, GitHub host (e.g. `api.github.com:443`) reachable; a **non-allowlisted** host blocked by the base default-deny NP (additive-allow invariant: FQDN policy widens, never replaces default-deny).
  Confirm fail-closed if `--enable-fqdn-network-policy`/CRD absent (egress stays denied).
- **PASS** = GitHub allowed, everything else denied, base NP still fail-closed; **FAIL** = non-GitHub egress leaks, or GitHub blocked, or not fail-closed.

### Q243 — per-tenant egress IP (cloud NAT)

- **Setup:** 2 tenant pools on 2 subnets, each with its own Cloud NAT + reserved static IP; a pinned representative egress pod per tenant (nodeSelector to its pool).
  Echo endpoint reflecting source IP (e.g. an owned `/ip` or a public reflector).
- **Assert:** tenant-A egresses from IP-A, tenant-B from IP-B, A ≠ B (distinct); delete/relocate a pod → IP unchanged (stable).
  Also apply a real GMC-provisioned EgressProxy and record where its (unpinnable) pods land.
- **PASS** = distinct + stable per-tenant IPs via NAT + documented API-pinning gap; **FAIL** = shared or unstable IPs.

**If any validation fails, that is a real security/correctness finding — record it honestly here and file a follow-up; do not paper over it (secure-by-default).**

## Findings log

| Validation | Result | Evidence |
|---|---|---|
| **Q230** DNS under egress NP | ✅ **PASS** | `node-local-dns` pods running (`k8s-app=node-local-dns`, kube-system); emitted base NP's DNS egress rule carries all **three** peers (`kube-dns`, `node-local-dns`, `169.254.0.0/16`); from a pod governed by the NP, `nslookup github.com` → `140.82.114.4` (via `10.0.32.10`). Q229 fix holds live on DPv2 + NodeLocal DNSCache. |
| **Q245** gke FQDNNetworkPolicy enforcement | ✅ **PASS** | GMC emitted `networking.gke.io/v1alpha1 FQDNNetworkPolicy` (GitHub FQDNs + `*.blob.core.windows.net` on 443) owned by the EgressProxy. From a governed pod: `api.github.com` + `github.com` → **HTTP 200** (allowed); `example.com` resolves but TCP:443 **times out** (blocked by base default-deny — additive-allow invariant). **Fail-closed confirmed:** with the FQDNNetworkPolicy deleted (GMC scaled to 0), `api.github.com` → HTTP 000 timeout — the FQDN policy is the *sole* opener; absent it the base NP denies GitHub too. |
| **Q243** per-tenant egress IP | ⚠️ **mechanism PASS + API gap** | Two pods pinned to `pool-a`/`pool-b` egress from **distinct** IPs — tenant-A `34.24.58.224` (nat-a), tenant-B `34.26.87.185` (nat-b), consistent over 3 runs. **Stable across reschedule:** delete+recreate the pool-a pod (pod IP 10.5.0.6→10.5.0.7) → still `34.24.58.224`. **BUT** `EgressProxy.spec` has no `nodeSelector`/`tolerations`/`affinity`, and the builder's required cross-node anti-affinity **spreads** the pool: scaling one tenant's proxy to 2 replicas landed them on pool-a (`10.5.0.5`→nat-a) *and* pool-b (`10.6.0.6`→nat-b) — one tenant, **two** egress IPs. The NAT mechanism is proven; the API can't yet bind a tenant's proxy to one egress IP. → follow-up Queue item. |

## Decisions recorded

- **Q230 automated GKE-DPv2 CI lane → stay DEFERRED (do not build).** A per-PR GKE lane is expensive (real cluster provisioning + cloud spend per run) and redundant for the actual regression vector — the GMC dropping the `node-local-dns` DNS peer — which is already locked in by a unit assertion (`cmd/gmc/internal/controller/builder_test.go`, Q136/Q229).
  Guard = that unit test; re-run *this* manual live validation at major egress-path changes.
- **Q243 is NOT fully closed.** The reference-arch mechanism claim (distinct + stable per-tenant egress IP via Cloud NAT) is validated, and the EgressProxy API change needed to bind a tenant's proxy pods to one egress path has since landed (**Q282** — `spec.scheduling` pass-through on the `EgressProxy` + `ActionsGateway` CRs; see [Placement pass-through](q243-egress-ip-reference-arch.md#placement-pass-through-q282)).
  What remains for Q243 is a live end-to-end validation that a pinned proxy pool egresses from exactly one IP.
  Note Q282 reversed this campaign's original "platform-governed placement" design point — placement is CR-author-settable, and constraining it is a policy-engine concern; the reasoning is recorded in the Q243 plan.
