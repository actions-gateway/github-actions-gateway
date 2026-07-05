# FQDN Egress: Split Tenant Intent from the CNI/Platform Backend

Design for decoupling the **tenant-facing egress intent** (`CIDR` vs `FQDN`)
from the **platform-chosen FQDN enforcement backend** (Cilium, Calico, GKE
Dataplane V2, …), and for adding a `gke` backend that emits GKE's managed
`networking.gke.io FQDNNetworkPolicy`. It closes the fragmentation the Q242
turn-on surfaced: today `egressPolicyMode` bakes a **per-CNI object kind**
directly into the tenant API, so the same intent ("allow GitHub egress by
hostname") is spelled differently on every CNI and cannot scale past the
enum.

**Scope of this document / this plan's PR: design + phased implementation plan
only.** No cluster is stood up and no live GKE enforcement is validated here —
the `gke` backend interacts with GKE Dataplane V2's in-kernel FQDN enforcement,
which can only be confirmed on a real GKE cluster; that is deliberately deferred
to a follow-up (see [Live-validation plan](#live-validation-plan-deferred-follow-up))
to keep it off the cloud bill. Every `gke`-backend guarantee that depends on the
deferred validation is marked **(to be validated)** rather than asserted.

Tracks [Q245](../STATUS.md#Q245). A v2beta1 ([Q74](../STATUS.md#Q74)) input
alongside [Q242](../STATUS.md#Q242) / [Q243](../STATUS.md#Q243), though — see
[Migration](#migration--compatibility) — the reshape is **free to land in
`v2alpha1` now** and does not block on the beta cut. Promoted from the
[Q242 plan § Provider FQDN-egress fragmentation](q242-g1-proxy-destination-allowlist.md#provider-fqdn-egress-fragmentation-post-implementation-finding).

---

## The problem: the enum conflates intent with mechanism

`EgressProxy.spec.egressPolicyMode` today is a three-value enum
([`api/v2alpha1/egressproxy_types.go`](../../api/v2alpha1/egressproxy_types.go)):

```go
// +kubebuilder:validation:Enum=CIDR;CiliumFQDN;CalicoFQDN
type EgressPolicyMode string
```

- `CIDR` (default) — a standard `NetworkPolicy` whose egress allowlist is the
  GitHub IP ranges, refreshed every 24h by the GMC's `IPRangeReconciler`. Works
  on every NetworkPolicy-enforcing CNI.
- `CiliumFQDN` — the GMC emits a `cilium.io/v2 CiliumNetworkPolicy` with
  `toFQDNs` rules.
- `CalicoFQDN` — the GMC emits a `projectcalico.org/v3 NetworkPolicy` with
  destination `domains`.

The GMC picks the emitter by switching on the enum value directly
([`egressproxy_controller.go:146`](../../cmd/gmc/internal/controller/egressproxy_controller.go)):

```go
wantCilium := managed && mode == gmcv2alpha1.EgressPolicyModeCiliumFQDN
wantCalico := managed && mode == gmcv2alpha1.EgressPolicyModeCalicoFQDN
```

Three problems, all surfaced by the Q242 dogfood turn-on
([Q242 finding](q242-g1-proxy-destination-allowlist.md#provider-fqdn-egress-fragmentation-post-implementation-finding)):

1. **The per-CNI enum doesn't scale, and it leaks platform detail into the
   tenant API.** A tenant authors `EgressProxy` — a tenant should not have to
   know whether the cluster runs Cilium, Calico, or GKE Dataplane V2. Adding GKE
   means a 4th enum value, AKS a 5th, EKS a 6th, OVN a 7th. Each new value is a
   tenant-facing API change, a new CEL branch, and a new admission case. The
   space also moves fast: EKS DNS-based `ClusterNetworkPolicy` shipped Dec 2025;
   GKE's managed `FQDNNetworkPolicy` is still `v1alpha1`.
2. **A managed offering branded "Cilium" does not accept the upstream CRD.**
   GKE Dataplane V2 *is* Cilium under the hood but does **not** install the
   `cilium.io/v2 CiliumNetworkPolicy` CRD (dropped since GKE 1.21.5-gke.1300).
   So `CiliumFQDN` — an intent the tenant expressed — silently maps to a
   mechanism that platform can't honor, and the `EgressProxy` goes `Degraded`
   (`no matches for kind "CiliumNetworkPolicy"`, verified 2026-06-29). The
   tenant asked for the *right* thing; the API forced them to name the *wrong*
   mechanism.
3. **The intent is stable; the mechanism is not.** "Allow GitHub egress by
   hostname" is a durable tenant intent. Which CRD kind expresses it is a
   per-cluster, per-CNI, per-platform-version implementation detail. Coupling
   them means every mechanism change is an API change.

### The CNI / managed-platform FQDN-egress matrix

Researched 2026-06-29, schema details reconfirmed 2026-07-05:

| Environment | In-cluster FQDN-egress mechanism | API kind | Enable | Scope |
|---|---|---|---|---|
| Self-managed Cilium | `toFQDNs` | `cilium.io/v2` `CiliumNetworkPolicy` | install Cilium | **namespaced** (GAG emits — `cilium` backend) |
| Self-managed Calico (DNS policy) | destination `domains` | `projectcalico.org/v3` `NetworkPolicy` | enable DNS policy | **namespaced** (GAG emits — `calico` backend) |
| **GKE Dataplane V2 (managed)** | `FQDNNetworkPolicy` | `networking.gke.io/v1alpha1` `FQDNNetworkPolicy` | `--enable-fqdn-network-policy` | **namespaced** (alpha); **no `CiliumNetworkPolicy` CRD** — `gke` backend, this plan |
| AKS (Azure CNI Powered by Cilium) | FQDN filtering | `cilium.io/v2` `CiliumClusterwideNetworkPolicy` | Advanced Container Networking Services + k8s ≥1.29 | **cluster-wide**; partial wildcard |
| EKS (VPC CNI, GA Dec 2025) | DNS-based network policy | EKS `ClusterNetworkPolicy` | VPC CNI ≥1.21.1 + EKS Auto Mode + k8s ≥1.29 | **cluster-wide**; new |
| OpenShift (OVN-Kubernetes) | `EgressFirewall` `dnsName` (+ `DNSNameResolver`) | `k8s.ovn.org` `EgressFirewall` | built-in | namespaced; 30-min TTL TOCTOU |

Two structural axes fall out of this table, and they are what the design
separates:

- **Namespaced vs cluster-scoped.** Cilium, Calico, and GKE-managed are all
  **namespaced** — they slot into the GMC's existing per-`EgressProxy`
  ownership + cascade-GC model unchanged. AKS (`CiliumClusterwideNetworkPolicy`)
  and EKS (`ClusterNetworkPolicy`) are **cluster-scoped**, which the namespaced
  per-`EgressProxy` ownership cannot express — those need the GMC to own/merge a
  cluster-scoped object (a different ownership/GC model). **This is why `gke` is
  the right first backend to add and AKS/EKS are deferred** (see
  [Backend matrix](#backend-support-matrix)).
- **Default-deny vs additive-allow composition.** This is the security crux and
  gets its own section ([Secure by default](#secure-by-default-the-composition-crux)).

## The design: two independent axes

Split the one enum into two orthogonal settings — one the **tenant** owns, one
the **operator** owns.

### Axis 1 — Tenant intent (`EgressProxy.spec.egressPolicyMode`)

Collapse the tenant field to intent only:

```go
// +kubebuilder:validation:Enum=CIDR;FQDN
type EgressPolicyMode string

const (
    EgressPolicyModeCIDR EgressPolicyMode = "CIDR" // default; standard NetworkPolicy + 24h IP-range reconcile
    EgressPolicyModeFQDN EgressPolicyMode = "FQDN" // CNI-native DNS-aware policy; mechanism chosen by the operator backend
)
```

- `CIDR` (unchanged default) — standard `NetworkPolicy` + IP-range reconcile.
  Works on every CNI, needs no operator backend, secure default.
- `FQDN` — "express my GitHub (+ Q242 `destinationFQDNs`) egress allowlist by
  hostname." The tenant says *what*; the operator's backend decides *how*.

The tenant no longer names Cilium/Calico/GKE. `destinationFQDNs` (Q242) now
requires intent `FQDN` (not the specific `CiliumFQDN`/`CalicoFQDN` values), so
the Q242 host-suffix allowlist composes with **whatever** backend the cluster
runs.

### Axis 2 — Operator backend (`GMC --fqdn-policy-backend`)

A GMC install-level flag, chosen once per cluster by the platform operator,
alongside the existing platform-governance egress flags
([`cmd/gmc/cmd/main.go:143`](../../cmd/gmc/cmd/main.go) — `--allowed-egress-fqdns`
/ `--allowed-egress-cidrs` / `--egress-destination-allowlist-configmap`):

```
--fqdn-policy-backend=none|cilium|calico|gke   (default: none)
```

- `none` (default) — the cluster declares **no** FQDN backend. A tenant that
  requests intent `FQDN` is **rejected by admission** with a clear message
  ("this cluster has no FQDN egress backend configured; ask the platform
  operator, or use egressPolicyMode: CIDR"). Fail-closed **and loud** — never a
  silent runtime `Degraded`.
- `cilium` — emit `cilium.io/v2 CiliumNetworkPolicy` (today's `CiliumFQDN`
  emitter, unchanged).
- `calico` — emit `projectcalico.org/v3 NetworkPolicy` (today's `CalicoFQDN`
  emitter, unchanged).
- `gke` — emit `networking.gke.io/v1alpha1 FQDNNetworkPolicy` (this plan, [below](#the-gke-backend)).

The flag mirrors the governance shape already established for
`--allowed-egress-fqdns`: platform-set, not tenant-set; `none`/empty is the
secure default that denies rather than guesses.

### Backend resolution

A single resolver maps `(intent, backend)` → concrete emitter, replacing the
direct enum switch:

| `spec.egressPolicyMode` | `--fqdn-policy-backend` | Emits | Admission |
|---|---|---|---|
| `CIDR` | *(any)* | standard `NetworkPolicy` only | allowed |
| `FQDN` | `none` | *(nothing)* | **rejected** — no backend |
| `FQDN` | `cilium` | `CiliumNetworkPolicy` | allowed |
| `FQDN` | `calico` | Calico `NetworkPolicy` | allowed |
| `FQDN` | `gke` | `FQDNNetworkPolicy` | allowed |

Adding a provider becomes: **one new emitter function + one new `--fqdn-policy-backend`
value + one namespaced (or, for AKS/EKS, cluster-scoped) reconcile branch.** No
tenant-facing enum churn, no CEL change, no per-provider admission case. The
tenant API stops moving when the mechanism moves.

## Secure by default: the composition crux

The backends differ in a security-critical way that the design **must** encode,
or a `gke` cluster silently loses egress isolation.

**Cilium / Calico FQDN policies are self-default-denying.** A
`CiliumNetworkPolicy` (or Calico `NetworkPolicy` with `types: [Egress]`) makes
its selected pods **default-deny** for egress — the policy *is* the deny, and it
allow-lists DNS + GitHub on top. So the emitted CNI object alone is fail-closed.

**GKE `FQDNNetworkPolicy` is additive-allow (union), NOT default-deny.** Per the
[GKE docs](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/fqdn-network-policies):
"When both a `FQDNNetworkPolicy` and a `NetworkPolicy` apply to the same Pod,
egress traffic is allowed as long as it matches **one** of the policies" — a
**union**, with no hierarchy. An `FQDNNetworkPolicy` on its own does **not**
deny anything; it only *adds* an allow for the listed FQDNs.

This inverts the fail-closed reasoning, and getting it wrong is a wide-open
egress hole. The design resolves it by leaning on an invariant GAG already
maintains:

> **The standard `NetworkPolicy` is always emitted, and in an FQDN intent it
> default-denies GitHub egress** (it drops the GitHub-CIDR rule but keeps
> `policyTypes: [Egress]` + the DNS-only allow). See
> [`egressproxy_controller.go:126`](../../cmd/gmc/internal/controller/egressproxy_controller.go)
> — `applyNetworkPolicy` runs on every path.

So the composition is:

- **base `NetworkPolicy`** = default-deny egress, allow DNS only (already true
  in FQDN intent).
- **`gke` `FQDNNetworkPolicy`** = allow GitHub (+ Q242) FQDNs on 443.
- **union** = DNS + GitHub allowed, **everything else denied**. Fail-closed. ✅

The fail-closed guarantee for `gke` therefore **depends on the base
`NetworkPolicy` being present**. The design makes that explicit and testable:
the `gke` reconcile branch is only ever reached when `managedNetworkPolicy` is
true (the base NP is managed); a `gke` backend is **never** offered in a
configuration where the base NP is absent. If Dataplane V2's FQDN enforcement is
disabled or the `FQDNNetworkPolicy` fails to apply, GitHub egress stays *denied*
by the base NP — never opened. This matches the existing secure-by-default
contract for Cilium/Calico ([05-security.md](../design/05-security.md),
[network-architecture.md § CNI-native FQDN egress mode](../design/network-architecture.md)).

**Secure-default checklist (CLAUDE.md § Security principles):**

- The default `--fqdn-policy-backend=none` **denies** FQDN intent rather than
  guessing a mechanism — no auto-detect, no silent fallback.
- No backend value makes egress **less** isolated than today's `CIDR` default.
  `CIDR` remains the CRD default; nothing about this change alters the default
  posture of an `EgressProxy` that never sets `egressPolicyMode`.
- `FQDN` + `none` is an **admission rejection**, not a runtime `Degraded` — the
  operator learns at apply time, not from a stranded proxy pool.
- The `gke` union semantics are called out as a **required invariant** (base NP
  present), not an incidental one, so a future refactor can't quietly drop the
  base NP and open egress.

## The `gke` backend

Target the **managed Dataplane V2 built-in** `FQDNNetworkPolicy`
(`networking.gke.io/v1alpha1`, enabled by `--enable-fqdn-network-policy`) — the
one GKE enforces in-kernel with **no self-deployed controller**. (Note: there is
also a separate open-source add-on controller,
`GoogleCloudPlatform/gke-fqdnnetworkpolicies-golang`, at `networking.gke.io/v1alpha3`
with a *different* schema — `egress[].to[].fqdns[]` — which requires you to run
its controller. The `gke` backend targets the **managed built-in**, not the
add-on. An operator running the add-on instead would want a distinct backend
value, e.g. `gke-oss`, deferred.)

### Emitted object (managed built-in schema)

```yaml
apiVersion: networking.gke.io/v1alpha1
kind: FQDNNetworkPolicy
metadata:
  name: <ep>-proxy-fqdn          # same suffix convention as cilium/calico
  namespace: <ep-namespace>
  labels: { <egressProxyLabels> }
  ownerReferences: [ <controller ref to the EgressProxy> ]   # namespaced ⇒ cascade GC
spec:
  podSelector:
    matchLabels: { <egressProxyPodSelector> }   # this pool's proxy pods only
  egress:
  - matches:
    - name: api.github.com
    - name: github.com
    - name: codeload.github.com
    - name: objects.githubusercontent.com
    - pattern: "*.actions.githubusercontent.com"
    - pattern: "*.blob.core.windows.net"
    # + operator destinationFQDNs (Q242), name/pattern by wildcard detection
    ports:
    - protocol: TCP
      port: 443
```

Emitter shape mirrors `buildEgressProxyCiliumNetworkPolicy` /
`buildEgressProxyCalicoNetworkPolicy`
([`egressproxy_fqdn.go`](../../cmd/gmc/internal/controller/egressproxy_fqdn.go)):
build the FQDN list via the existing `egressFQDNs(ep)` (GitHub implicit set +
`destinationFQDNs`), split each entry into `{name: …}` (exact) or `{pattern: …}`
(contains `*`), as an `unstructured.Unstructured` so the GMC takes **no
compile-time dependency** on any GKE API module and a non-GKE cluster
`NoMatch`-errors loudly rather than forcing the dependency everywhere (identical
to how the Cilium/Calico objects are handled). Reconcile/GC reuse
`applyCNIPolicy` / `deleteCNIPolicy` unchanged.

### DNS handling — simpler than Cilium (Q229 interaction)

Cilium's backend needs a **DNS-visibility** rule so its DNS proxy learns the
resolved IPs, plus (Q229 — see
[network-architecture.md](../design/network-architecture.md)) a `node-local-dns`
DNS peer on GKE Dataplane V2, where NodeLocal DNSCache redirects the kube-dns
ClusterIP to the per-node `node-local-dns` pod.

The `gke` `FQDNNetworkPolicy` needs **neither** inside the FQDN object: the GKE
docs state an active `FQDNNetworkPolicy` "does not affect the ability of
workloads to make DNS requests" — DNS bypasses FQDN enforcement, and GKE
resolves the names itself. **But** DNS still has to be allowed by the **base
`NetworkPolicy`**, and that base NP already carries the Q229 `node-local-dns`
peer (`dnsEgressRule`, shared with the Cilium/Calico builders). So the Q229
DPv2-DNS fix is inherited for free by the `gke` backend — no new DNS rule, and
no risk of the Q229 regression, precisely because DNS lives in the base NP that
`gke` composes with (see [Secure by default](#secure-by-default-the-composition-crux)).

### Composition with the Q242 allowlist

Q242's `destinationFQDNs` (host suffixes) and `destinationCIDRs` (IP ranges)
compose unchanged:

- `destinationFQDNs` → appended to the `FQDNNetworkPolicy` `matches` (as
  `name`/`pattern`), exactly as they're appended to Cilium `toFQDNs` / Calico
  `domains` today. Governed by `--allowed-egress-fqdns` at admission.
- `destinationCIDRs` → remain `ipBlock` peers on the **base `NetworkPolicy`**
  (they already work in every mode / backend — CIDRs never needed the FQDN
  object). Governed by `--allowed-egress-cidrs`.

No Q242 code changes; the split only renames the intent the CEL rule keys on
(`FQDN` instead of `CiliumFQDN||CalicoFQDN`).

### `gke` caveats (to document in operator docs)

- **50-IP resolution ceiling per FQDN, 100-IP quota per hostname.** A wildcard
  like `*.blob.core.windows.net` (Azure-blob-backed Actions cache/artifacts) can
  resolve to many IPs; if enforcement caps at 50 resolved IPs, some blob egress
  may be dropped intermittently. This is the same "DNS-based allowlisting is
  inherently leaky" fragility Q242 documented — reinforce the **in-cluster
  caching mirror** as the robust path and reserve FQDN egress for what a mirror
  can't proxy. **(to be validated on a real GKE cluster.)**
- **`v1alpha1`, managed, GKE-version-gated** (1.26.4-gke.500+ / 1.27.1-gke.400+,
  Dataplane V2, `--enable-fqdn-network-policy`, kube-dns or Cloud DNS). Absence
  → `NoMatch`, which the reconcile tolerates on delete and surfaces as
  `Degraded` on apply (fail-closed by the base NP).

## Backend support matrix

| Backend | Object | Scope | Ownership/GC model | Status |
|---|---|---|---|---|
| `cilium` | `cilium.io/v2 CiliumNetworkPolicy` | namespaced | per-`EgressProxy` owner ref (today) | **shipped** (was `CiliumFQDN`) |
| `calico` | `projectcalico.org/v3 NetworkPolicy` | namespaced | per-`EgressProxy` owner ref (today) | **shipped** (was `CalicoFQDN`) |
| `gke` | `networking.gke.io/v1alpha1 FQDNNetworkPolicy` | namespaced | per-`EgressProxy` owner ref (reuse) | **this plan** (Phase 2; enforcement to-be-validated) |
| `aks` | `cilium.io/v2 CiliumClusterwideNetworkPolicy` | **cluster** | needs cluster-scoped own/merge — different GC | **deferred** (Phase 3) |
| `eks` | EKS `ClusterNetworkPolicy` | **cluster** | needs cluster-scoped own/merge — different GC | **deferred** (Phase 3) |
| `ovn` | `k8s.ovn.org EgressFirewall` `dnsName` | namespaced | per-`EgressProxy` owner ref | **deferred** (30-min TTL TOCTOU caveat) |

The split is what makes `aks`/`eks` *addable at all* without another
tenant-facing enum explosion — but their **cluster-scoped** objects need a new
ownership/GC design (the GMC owning/merging one cluster-scoped policy across many
namespaced `EgressProxy` pools, with reference-counted GC). That is a
self-contained follow-up, out of scope here; the security model is unaffected
(the allowlist is platform-governed regardless of where the object lives).

## Migration / compatibility

**The reshape is free to land in `v2alpha1` now — it does not block on the
v2beta1 cut.** Per [v2beta1.md](v2beta1.md) (Q196 credentials precedent):
"alpha carries no stability contract, so the reshape is free now, and the beta
cut then inherits the correct shape." `egressPolicyMode` is `v2alpha1`, and the
only consumers of `CiliumFQDN`/`CalicoFQDN` are tests and docs — the dogfood is
direct-egress and never exercised an FQDN mode (Q242 finding). So the clean
break (drop `CiliumFQDN`/`CalicoFQDN`, add `FQDN` + the operator flag) can be
made directly, with **no additive-alias deprecation dance**.

Tie-in with **Q74** (v2alpha1→v2beta1 conversion webhook): because the reshape
lands in `v2alpha1`, the beta cut inherits the already-correct
`egressPolicyMode: CIDR|FQDN` shape, and the conversion webhook maps
`egressPolicyMode` as an **identity** (like it does for the Q196 credentials
block) — no reshape in the webhook. The operator `--fqdn-policy-backend` flag is
**not** a CRD field and never enters conversion at all; it is install
configuration, versionless by construction. This is strictly simpler for Q74
than leaving the fragmented enum to be reshaped *during* the cut.

If, contrary to the above, a real consumer of `CiliumFQDN`/`CalicoFQDN` is found
before this lands, the fallback is a one-release additive alias: keep
`CiliumFQDN`/`CalicoFQDN` in the enum as deprecated, treat `CiliumFQDN` ≡ `FQDN`
+ forced `cilium` backend (ignoring the flag) and likewise for Calico, then drop
them at the beta cut via the conversion webhook. **Recommended: skip the alias**
— there are no consumers, and the alias is pure carrying-cost.

## Implementation plan (phased)

Sized L overall; phased so each phase is independently shippable, reviewable,
and (except the live GKE pass) offline-testable.

### Phase 1 — API split + operator flag + backend resolver *(offline)*

1. `api/v2alpha1/egressproxy_types.go` — `EgressPolicyMode` enum `CIDR;FQDN`;
   update the `destinationFQDNs` CEL rule to key on `egressPolicyMode == 'FQDN'`;
   update godoc. Regenerate CRDs/deepcopy per
   [code-generation.md](../development/code-generation.md) (`make generate manifests`).
2. `cmd/gmc/cmd/main.go` — register `--fqdn-policy-backend` (default `none`,
   validated enum), thread it into `EgressProxyReconciler` (same wiring path as
   the egress allowlist).
3. `egressproxy_fqdn.go` / `egressproxy_controller.go` — replace the
   `mode == CiliumFQDN` / `== CalicoFQDN` switch with a `(intent, backend)`
   resolver; retarget the existing Cilium/Calico builders through it (behavior
   identical for `cilium`/`calico`).
4. Admission ([`egressproxy_webhook.go`](../../cmd/gmc/internal/webhook/v2alpha1/egressproxy_webhook.go)):
   reject intent `FQDN` when backend is `none`.
5. Tests: unit (resolver table, admission), envtest (existing Cilium/Calico
   emission/GC via the new resolver; the `cni-crds` testdata already exists).
   Update the tests/docs that name `CiliumFQDN`/`CalicoFQDN`.

### Phase 2 — `gke` backend *(offline build + deferred live validation)*

1. `egressproxy_fqdn.go` — `buildEgressProxyGKEFQDNNetworkPolicy` (managed
   `v1alpha1` schema above); add `gke` to the resolver + reconcile/GC branch
   (reuse `applyCNIPolicy`/`deleteCNIPolicy`).
2. Add a `networking.gke.io FQDNNetworkPolicy` CRD to
   `cmd/gmc/internal/controller/integration/testdata/cni-crds/` for envtest
   emission/GC coverage (structural).
3. Unit golden test of the emitted unstructured object; envtest apply/GC.
4. **Deferred: live GKE validation** (see below) before flipping any docs from
   "(to be validated)" to asserted.

### Phase 3 — cluster-scoped backends `aks` / `eks` *(deferred, separate design)*

Needs the cluster-scoped ownership/GC design. File as its own Queue item on
completion of Phase 1 (it's unblocked by the split, but is a distinct chunk).

## Live-validation plan (deferred follow-up)

The `gke` backend's actual **enforcement** — that Dataplane V2 drops non-GitHub
egress while the base NP + `FQDNNetworkPolicy` union permits GitHub, that the
50-IP ceiling behavior on `*.blob.core.windows.net` is tolerable, that DNS
resolution is unaffected — can only be confirmed on a real GKE cluster with
`--enable-fqdn-network-policy`. Deferred to keep it off the cloud bill and
because CLAUDE.md requires end-to-end confirmation before asserting a finding.
The validation steps (run on the existing `gag-dogfood` GKE cluster or a
throwaway):

1. Enable `--enable-fqdn-network-policy` on the cluster (Dataplane V2 already on).
2. Apply an `EgressProxy` with `egressPolicyMode: FQDN`, GMC
   `--fqdn-policy-backend=gke`.
3. Confirm the `FQDNNetworkPolicy` is emitted, owned, and enforced: GitHub
   CONNECT succeeds; a non-allowlisted host is dropped; DNS still resolves.
4. Stress the wildcard-blob 50-IP ceiling under real Actions cache/artifact
   load; record whether blob egress is intermittently dropped.

Until then, `gke`-backend enforcement guarantees are marked **(to be validated)**.

## Docs to update when this lands

- **Design:** [network-architecture.md § CNI-native FQDN egress mode](../design/network-architecture.md)
  (intent/backend split; drop the "not yet emit" note for GKE),
  [appendix-h-v2-api-decomposition.md](../design/appendix-h-v2-api-decomposition.md)
  (`EgressPolicyMode` enum `CIDR;FQDN`), [05-security.md](../design/05-security.md)
  (the union-vs-default-deny composition invariant).
- **Operator:** [security-operations.md § Expressing GitHub egress by FQDN](../operations/security-operations.md#expressing-github-egress-by-fqdn-the-egresspolicymode-opt-in)
  (tenant sets `FQDN`, operator sets `--fqdn-policy-backend`; the `gke` caveats),
  [migration-v1-to-v2.md](../operations/migration-v1-to-v2.md) (enum change).
- **Backlog:** remove the Q245 Queue row; if Phase 3 (aks/eks) is wanted, file it.
