# 5. Security & Threat Risk Assessment

← [Operational Flows](04-operational-flows.md) | [Back to index](README.md) | Next: [Implementation Phases →](06-implementation-phases.md)

---

The two-tier architecture introduces both stronger isolation guarantees and new attack surfaces.
Threats are grouped by which tier they affect.

## 5.1. GMC-Level Threats (Cluster-Scoped)

| Threat Vector | Impact | Mitigation Strategy |
| --- | --- | --- |
| **GMC Privilege Escalation** (Blast Radius: All Tenants) | Critical | Cluster-wide provisioning writes are confined to marked tenant namespaces by two `ValidatingAdmissionPolicy` objects (`namespace-psa-guard`, `gmc-tenant-resource-guard`); the honest residual is cluster-wide Secret *reads*, offset by metadata-only caching, uncached direct reads, and the Q29 audit policy. Full blast radius and compensating controls: [see below](#gmc-privilege-escalation-blast-radius-and-compensating-controls). |
| **Admission Webhook Unavailability or Bypass** | Medium | The reserved-namespace validating webhook serves as a safety check, not a security boundary — namespace isolation is enforced by RBAC and NetworkPolicy regardless of webhook state. The webhook uses `failurePolicy: Fail` so requests are rejected when the webhook pod is unhealthy rather than silently bypassed. Serving certificates are managed by cert-manager with automatic rotation; the CA bundle is injected via `caBundle` from the cert-manager-managed Secret. Webhook pod runs `replicas: 2` behind a Service with `podAntiAffinity` to survive single-node loss without stalling tenant onboarding. |
| **Metrics Scrape Interception (MITM)** | Low | The `:8443` metrics endpoint is TLS + TokenReview/SAR-authorized, served with a cert-manager metrics cert the `ServiceMonitor` verifies (secure default), with a documented `insecureSkipVerify` fallback only when cert-manager is absent; the manager NetworkPolicy admits `:8443` **only** from `metrics: enabled` namespaces. Full TLS/network posture: [see below](#metrics-scrape-interception-and-the-mtls-posture). |
| **Tenant Namespace Escape via Overpermissioned AGC** | Critical | Each AGC's ServiceAccount is bound by a RoleBinding limited to its own namespace. The AGC cannot list or touch resources in any other tenant namespace. |
| **Cross-Tenant GitHub App Credential Leakage** | High | `ActionsGateway` is namespace-scoped so a `gitHubAppRef` cannot cross namespaces, and the credential Secret is mounted into the AGC pod only (never worker pods). Secret **writes** are confined by `gmc-tenant-resource-guard`; cluster-wide *reads* are offset by two-layer cache isolation (metadata-only informer + `DisableFor` uncached `Get`). Full detail: [see below](#cross-tenant-github-app-credential-leakage-and-cache-isolation). |
| **`ActionsGateway` CR in Reserved Namespace** | Medium | An admission webhook rejects `ActionsGateway` CRs created in reserved namespaces: the universal `kube-system` and `kube-public`, the GMC's default install namespace `gmc-system`, and the namespace the GMC pod is actually running in (read from the `POD_NAMESPACE` downward-API env var, so custom installs are protected too). The same guard applies to the v2 kinds that make the GMC provision workloads into their namespace — the v2 `ActionsGateway` (AGC control plane) and `EgressProxy` (proxy Deployment + NetworkPolicies) — via the shared `validation` package (Q323); a v2 `RunnerSet`/`RunnerTemplate` needs no guard of its own because it is inert without a same-namespace gateway. Since the CR is namespace-scoped, a tenant can only affect their own namespace — the risk is self-harm or collision with operator-owned resources, not cross-tenant impact. |

### GMC privilege-escalation blast radius and compensating controls

The GMC's ClusterRole grants cluster-wide *write* (`create`/`update`/`delete`) on the tenant provisioning kinds — Deployments, Services, ServiceAccounts, RoleBindings, Roles, NetworkPolicies, HPAs, VerticalPodAutoscalers (Q360; the grant is inert on a cluster without the optional `autoscaling.k8s.io` CRDs, and a VPA only ever resizes the workload it targets), PodDisruptionBudgets, RunnerGroups, and Secret `create`/`update` — because RBAC cannot express "only namespaces that carry a marker label".

Two `ValidatingAdmissionPolicy` objects close that gap by confining the GMC ServiceAccount at admission: `namespace-psa-guard` confines `namespaces:patch` to the six `pod-security.kubernetes.io/*` keys on marked namespaces, and **`gmc-tenant-resource-guard` confines every `create`/`update`/`delete` of the kinds above to namespaces an administrator has marked `actions-gateway.github.com/tenant: "true"`** (Q121 write path / Q122).
So a compromised GMC cannot create a Deployment or RoleBinding in `kube-system`, write a Secret into an arbitrary namespace, or relabel `kube-system` PSA to `privileged` (see [§5.3](#53-security-profiles-and-the-privileged-opt-in)).

The grant RBAC and admission cannot confine is Secret **reads**: admission never runs on `get`/`list`/`watch`, `resourceNames` cannot scope `list`/`watch`, and tenant Secret names are dynamic so `get` cannot be name-scoped either — therefore the ClusterRole grants cluster-wide Secret `get`/`list`/`watch`, and this is an honestly-stated residual, not a confined property (Q121; compensating controls below).

**Explicit blast radius if compromised.** A compromised GMC can:

* Enumerate every `ActionsGateway` CR in the cluster, learning each tenant's `gitHubAppRef` name and namespace.
* `list`/`watch` and `get` the full `.data` of **any** Secret in the cluster, including GitHub App private keys (the metadata-only informer and uncached reads below are client-side hygiene, not authorization).
* Create/update/delete workloads and Secrets **only in marked tenant namespaces** (the `gmc-tenant-resource-guard` VAP denies writes elsewhere).

It CANNOT exec into pods, create new namespaces, or write resources into unmarked namespaces such as `kube-system`.

**Secret-read compensating controls.** Four, three preventive and one detective:

* `WatchesMetadata` keeps the informer cache to Secret ObjectMeta only (no `.data`).
* `client.Cache.DisableFor[*corev1.Secret]` forces `r.Get()` to hit the API server directly, so key material is never cache-resident.
* In practice the GMC only `get`s the named credential Secret in the CR's own namespace.
* The Q29 audit policy ([sample](../operations/examples/apiserver-audit-policy.yaml), [runbook](../operations/security-operations.md#api-server-audit-policy-sample)) surfaces any out-of-pattern Secret read.

GMC pod runs with non-root user, read-only root filesystem, no host mounts, and `seccompProfile: {type: RuntimeDefault}`.
Image is digest-pinned, enforced at chart render time — the Helm chart refuses to render an unpinned `gmc.image`.
Treat the GMC pod as a Tier-0 workload for monitoring and access.

### Metrics scrape interception and the mTLS posture

The GMC metrics endpoint (`:8443`) is served over TLS and authorized by TokenReview/SubjectAccessReview.
By default the chart issues a dedicated metrics serving cert via cert-manager (`metrics-serving-cert` → `metrics-server-cert` Secret, minted from the same `selfsigned-issuer` as the webhook), the GMC serves it with `--metrics-cert-path`, and the rendered `ServiceMonitor` verifies it against the issuing CA (`tlsConfig.ca` + `serverName`) — so an in-cluster attacker cannot impersonate the metrics endpoint.
This is the secure default (`metrics.tls.certManager.enabled=true`), gated on `certManager.enabled=true` since it reuses the webhook Issuer.

When cert-manager is absent (`certManager.enabled=false`) or the toggle is off, the GMC falls back to controller-runtime's auto-generated self-signed cert and the `ServiceMonitor` scrapes with `tlsConfig.insecureSkipVerify: true` — an explicit, documented opt-out that loses server verification (the bearer token still authenticates the scraper to the server, but not the server to the scraper).

As network-layer defence in depth, the manager NetworkPolicy (`networkPolicy.enabled=true`, default; Q34/E5) flips the controller-manager pod to default-deny ingress and re-admits `:8443` **only** from namespaces labelled `metrics: enabled`, while leaving the webhook port `:9443` open (no source restriction) so admission keeps working under `failurePolicy: Fail`.

This deny-unlabelled / allow-labelled / admission-still-works posture was verified end-to-end at runtime on a Calico kind cluster on 2026-06-18 (Q83): a scrape from an unlabelled namespace timed out (no route to `:8443`), a scrape from a `metrics: enabled` namespace reached the endpoint (HTTP 401 — TLS/auth layer, not network), and the validating webhook on `:9443` returned its verdict (admission unaffected).
The check is codified as a Calico-gated cluster-only e2e spec (`Manager NetworkPolicy`).
See [observability.md § Verifying the metrics scrape TLS](../operations/observability-metrics-access.md#verifying-the-metrics-scrape-tls-gmc-manager).

Both TLS servers the manager exposes — the `:8443` metrics endpoint and the `:9443` admission webhook — are configured with HTTP/2 **disabled** (`TLSOpts` force `NextProtos: ["http/1.1"]`).
This closes the HTTP/2 Rapid Reset denial-of-service class (CVE-2023-44487 / CVE-2023-39325), where a client cheaply cancels a flood of streams to exhaust server resources.
The endpoints carry low request volume, so losing HTTP/2 multiplexing costs nothing; keeping it would expose a DoS surface for no benefit.
This matches the controller-runtime scaffold's own secure default.

### Cross-tenant GitHub App credential leakage and cache isolation

`ActionsGateway` is namespace-scoped, so a tenant's `gitHubAppRef` defaults to their own namespace — another tenant cannot reference it.
The GMC mounts credentials into the AGC Pod only; worker pods never have access to the Secret object.
Secrets are immutable; rotation creates a new Secret and updates the CR reference, producing a clean Deployment rollout.
Old Secrets are not readable by running Pods once the rollout completes.

The GMC's ClusterRole grants Secret `get`/`list`/`watch` cluster-wide (not name-scoped — `resourceNames` cannot scope `list`/`watch`, and tenant Secret names are dynamic so it cannot scope `get` either); Secret **writes** (`create`/`update`) are confined to marked tenant namespaces by the `gmc-tenant-resource-guard` `ValidatingAdmissionPolicy`, but reads cannot be confined at the authorization layer (admission does not run on read verbs — Q121).

**Two-layer cache isolation** (the compensating control for reads): the GMC uses `WatchesMetadata` (not a full Secret watch) so the in-process informer cache holds only Secret ObjectMeta (name, namespace, resourceVersion — no `.data`); and `client.Cache.DisableFor[*corev1.Secret]` ensures `r.Get()` calls bypass the cache entirely and hit the API server directly, so actual key material is never resident in memory beyond the duration of a single reconcile call.

---

## 5.2. AGC & Proxy-Level Threats (Namespace-Scoped)

Several mitigations below rest on the per-tenant NetworkPolicies the GMC reconciles (workload egress restricted to DNS + proxy, with **DNS itself confined to the cluster DNS service** rather than any resolver — Q105; only AGC-labelled pods get apiserver egress; **workload pods default-deny all ingress** — Q128).

The ingress default-deny matters because worker pods run untrusted GitHub Actions job code and are outbound-only by design (they long-poll/dial out to GitHub via the proxy and to the AGC); nothing legitimately initiates a connection *to* a worker, so the workload NP declares `policyTypes: [Ingress, Egress]` with an empty ingress rule set.
Without it, worker pods were default-allow ingress and any pod in the cluster could open connections to untrusted job code — a lateral-movement / cross-tenant channel.

NetworkPolicy objects are inert unless the cluster CNI enforces them — kind's default kindnet does **not** drop traffic, so production clusters must run a policy-enforcing CNI (Calico, Cilium, or equivalent).
Runtime enforcement of the egress negatives was observed on a Calico kind cluster on 2026-06-11 (Q7b; see [network-architecture.md § How to Validate Network Isolation](network-architecture.md#how-to-validate-network-isolation)).

**Secure-by-default trade for the optional egress proxy (Q168, signed off).** In v2 the egress proxy is optional ([appendix-h §H.10](appendix-h-v2-api-decomposition.md#h10-the-egress-proxy-becomes-optional)): a gateway/runner set with no `defaultProxyRef`/`proxyRef` egresses **directly** to GitHub.
This is a deliberate, reviewed trade and it splits the two properties the proxy used to bundle:

* It drops egress **identity** (per-tenant IP attribution): direct egress leaves a workload with no stable per-tenant source IP, surfaced explicitly as `proxyMode: Direct` plus an advisory `EgressUnattributed` condition so the operator sees the property they opted out of.
* It does **not** drop egress **restriction**: the GMC still provisions the mandatory, on-by-default default-deny egress NetworkPolicy, now allowing **DNS (cluster DNS only) + the GitHub CIDR allowlist** for workers, plus the **kube API server** for the AGC — there is no proxy-less mode in which a worker or AGC can reach arbitrary internet, and the GMC's `IPRangeReconciler` keeps the direct-egress GitHub allowlist current as GitHub rotates ranges.

Restriction stays mandatory and default-on; only **identity is the opt-in** (attach an `EgressProxy`).
Defaulting off the *restriction* would be a security regression and is out of scope.
The DNS-exfiltration containment below (port-53 confined to cluster DNS) is unchanged in direct mode; what direct egress removes is only the per-tenant *attribution* of the GitHub-bound leg, which is exactly the `EgressUnattributed`-flagged property.

> **CNI-native FQDN egress is an opt-in, fail-closed, never a default (Q208 / Q245).** How the GitHub allowlist is *expressed* is operator-selectable on a v2 `EgressProxy`, split across two roles (Q245): the **tenant** expresses intent via `spec.egressPolicyMode` (`FQDN` = "by hostname"; default `CIDR` = the standard NetworkPolicy + 24h `IPRangeReconciler` above, works on every CNI), and the **operator** picks the enforcement mechanism via the GMC `--fqdn-policy-backend` flag (`none` | `cilium` | `calico` | `gke`).
> The GMC then emits a CNI-native DNS-aware policy (`CiliumNetworkPolicy` `toFQDNs` / Calico `NetworkPolicy` `domains` / GKE `FQDNNetworkPolicy` `matches`) scoped to the GitHub hostnames, dropping the GitHub-CIDR rule from the standard NetworkPolicy and skipping the IP-range reconcile for that proxy.
> The deprecated `CiliumFQDN` / `CalicoFQDN` enum values still work (each pins its namesake backend) but the webhook warns and steers to `FQDN` + a backend; being enum members of the beta version `v2beta1`, which `v2.0.0` keeps serving, they are removable no earlier than `v3.0.0` ([why](../operations/v1alpha1-deprecation.md#a-fourth-deprecation-on-a-different-clock-ciliumfqdn--calicofqdn)).
> This **does not** relax restriction: the standard NetworkPolicy still default-denies GitHub egress, so if the chosen backend cannot enforce the FQDN policy (wrong CNI / CRD absent) the apply fails loudly (`EgressProxy` → `Degraded`) and GitHub egress stays **denied** rather than opening wide — selecting an FQDN mode can only ever keep or tighten the posture, never weaken it.
> The secure default `--fqdn-policy-backend=none` **rejects** an `FQDN`-intent `EgressProxy` at admission rather than guessing a mechanism.
>
> **The `gke` backend is additive-allow, so the base default-deny NetworkPolicy is a required invariant.** Unlike Cilium/Calico FQDN policies (self-default-denying), a GKE `FQDNNetworkPolicy` composes with a NetworkPolicy on the same pod as a **union** — it only *adds* an allow, it denies nothing on its own.
> GAG's fail-closed guarantee for `gke` therefore depends on the base standard NetworkPolicy (always emitted; GitHub-CIDR rule dropped, DNS-only allow kept) staying present alongside it: the `FQDNNetworkPolicy` only widens the union to permit GitHub, and if it is absent or unenforced GitHub egress stays denied by the base NP.
> A future refactor must not drop the base NP for the `gke` backend, or it opens egress.
> (`gke`-backend enforcement is not yet live-validated; see the [Q245 plan](../plan/q245-fqdn-intent-backend-split.md).)
> See [network-architecture.md § CNI-native FQDN egress mode](network-architecture.md#cni-native-fqdn-egress-mode-opt-in-q208-q245) and the operator runbook [security-operations.md § Expressing GitHub egress by FQDN](../operations/security-operations.md#expressing-github-egress-by-fqdn-the-egresspolicymode-opt-in).

> **Pod placement is CR-author-settable, so per-tenant egress-IP *attribution* is a property of the cluster's policy layer, not of GAG alone (Q282).** A per-tenant egress IP is realized by pinning a tenant's pods to a node pool whose egress path (cloud NAT gateway, dedicated subnet) owns that IP — so the pod's **node** decides which IP its traffic leaves by.
> GAG passes placement through to the CR author in three places: `RunnerTemplate.spec.podTemplate` (a full `PodTemplateSpec`, always has), and — since Q282 — `EgressProxy.spec.scheduling` and `ActionsGateway.spec.scheduling`, each carrying `nodeSelector`/`tolerations`/`affinity`.
> This is **deliberate and not admin-gated**: choosing an egress path is a *feature* (tenants should not share a rate limit or a block radius).
> Stated precisely, since the symmetry with worker pods is only partial — in `proxyMode: Direct` worker placement already selects the egress IP with no gate, but in the **proxied default** it does not (NetworkPolicy forces worker traffic through the proxy, so the *proxy pool's* placement selects the IP). `EgressProxy.spec.scheduling` therefore *does* extend the capability to the default posture; gating it would still leave the direct-egress path open, and — see below — no *validating* gate on these fields can be sound anyway.
> What this means precisely: a CR author who retargets its pods onto another pool's egress path makes both tenants' traffic leave via one IP, so **attribution** no longer holds. **Isolation, RBAC, and the egress choke point are unchanged** — the pods still traverse the same default-deny NetworkPolicy and the same proxy.
> Operators who attribute traffic by source IP (a downstream firewall allowlist, an audit trail) and do not fully trust their tenant CR authors must constrain placement in the **pod admission layer**.
> Note that a validating *allowlist* of `nodeSelector` values is unsound — `affinity.nodeAffinity` supports `NotIn`/`DoesNotExist`, so "any pool but mine" is expressible — whereas pinning `nodeSelector` by **mutation** is sound, since Kubernetes ANDs it with `nodeAffinity` and affinity can only narrow.
> Ready-to-apply Kyverno and Gatekeeper samples: [admission-policies.md § Governing where GAG pods schedule](../operations/admission-policies.md#governing-where-gag-pods-schedule).
> The same layer is where a cluster that grants tenants `pods: create` must constrain placement anyway — GAG's NetworkPolicies select GAG-labelled pods, so an unlabelled tenant-created pod is selected by none of them.

> **Worker egress beyond GitHub is a platform-gated, deny-by-default opt-in (Q242 G.1).** By default the proxy carries GitHub only.
> A platform admin can allow a small, explicit set of **non-GitHub** destinations a v2 `EgressProxy` may forward to — by DNS host suffix (`spec.destinationFQDNs`) and CIDR (`spec.destinationCIDRs`) — so CI jobs reach their build dependencies without forfeiting per-tenant egress-IP attribution or the DNS-exfil containment.
> Because the `EgressProxy` is tenant-authorable, the destinations are **governed by a platform-owned allowlist**, not the CR's ownership: the GMC `--allowed-egress-fqdns` (suffix match) / `--allowed-egress-cidrs` (subnet containment) flags — optionally augmented by a watched ConfigMap — plus a validating webhook that **rejects** any destination not on the allowlist. **Both empty ⇒ deny-all-non-GitHub**, identical in spirit to the empty `--allowed-priority-classes` default (Q132/Q188).
> The destinations widen one source into two enforcement surfaces (CIDR `ipBlock` / FQDN `toFQDNs` on the pod-egress policy, and the proxy CONNECT allowlist as defense-in-depth with DNS-rebinding revalidation on the CIDR path); GitHub stays implicit.
> This is a **deliberate, bounded relaxation** of the GitHub-only posture, accepted with the trade-offs recorded — the residual is that a compromised worker can exfiltrate to an allowlisted destination, which the allowlist bounds but does not eliminate; the docs therefore steer operators to an **in-cluster caching mirror** first and reserve the allowlist for what a mirror can't proxy.
> See the threat row below, [network-architecture.md § Worker egress to allowlisted non-GitHub destinations](network-architecture.md#worker-egress-to-allowlisted-non-github-destinations-opt-in-q242-g1), the operator runbook [security-operations.md § Worker egress destinations](../operations/security-operations.md#worker-egress-destinations-the-egress-allowlist), and the design [Q242 plan](../plan/archive/q242-g1-proxy-destination-allowlist.md).

> **A `noProxyCIDRs` entry must never route GitHub around the proxy.** `noProxyCIDRs` (v1 `ActionsGateway.spec.proxy`, v2 `EgressProxy.spec`) is threaded verbatim into the AGC/worker `NO_PROXY` env var, where a hostname entry is a domain-suffix match — so a tenant-authored entry like `github.com` (or an over-broad `.com`) would silently exclude GitHub from the proxy and defeat per-tenant egress-IP attribution.
> Both GMC validating webhooks reject any entry that NO_PROXY-matches a protected GitHub host (shared guard: `gmc/internal/webhook/noproxy`), while still admitting the legitimate internal-destination entries (CIDRs, bare IPs, non-GitHub domain suffixes such as `svc.cluster.local`).
> On the v2 `EgressProxy` — which carries no `gitHubURL` of its own — the protected set is the public GitHub hosts **plus the `gitHubURL` host (a GitHub Enterprise Server host included) of every referrer**, resolved through the uncached API reader from both directions (Q322): the `EgressProxy` write checks the gateways/RunnerSets that reference it, and the `ActionsGateway`/`RunnerSet` write checks the referenced proxy's `noProxyCIDRs`, so the bypass pair is rejected regardless of creation order (`gmc/internal/webhook/v2alpha1/noproxy_referrers.go`; unresolvable reads fail closed).
> One accepted residual: a **CIDR/IP entry covering GitHub's rotating published ranges** is not detected on either version (an in-tree IP blocklist would rot; operator responsibility).
> The runbook for the rejection: [troubleshooting.md § `proxy.noProxyCIDRs` rejected](../operations/troubleshooting.md#proxynoproxycidrs-rejected-entry-would-bypass-the-proxy-for-github).

> **The GMC's own `NO_PROXY` defaults exempt the API server, not a network range (Q465).** The same `NO_PROXY` var carries a platform-authored half that the GMC appends to every tenant's entries, and it is subject to the identical rule: whatever it lists escapes the tenant's egress proxy and so escapes egress-IP attribution.
> The mandatory set is therefore **DNS names plus one address**: `svc.cluster.local` and `kubernetes.default.svc` (in-cluster names, not publicly resolvable), `localhost`/`127.0.0.1` (the pod's own loopback), and — for the AGC only — **this cluster's API server ClusterIP**, read from the GMC's own `KUBERNETES_SERVICE_HOST` and emitted as a single bare address.
> The API server entry cannot be dropped: client-go's in-cluster config dials by IP, so without it a proxied AGC CONNECTs to the API server through the tenant's proxy, fails to verify the proxy CA, and crash-loops.
> It also cannot be a hardcoded range — the ClusterIP differs on every distribution.
> It was previously written as the kubeadm Service CIDR `10.96.0.0/12`, which was **both a correctness bug and a widening**: wrong (so broken) on every managed distribution, and on a cluster whose pod or node addresses fall inside that range it exempted arbitrary unrelated traffic from the proxy.
> Worker pods get a strictly smaller set (no API server entry at all — they hold no kubeconfig and run with `automountServiceAccountToken: false`). `TestBuildNoProxy_DefaultsAreMinimal` pins the exact set, so widening it is a deliberate edit rather than a drift.

> **Cross-namespace proxy sharing is provider-consented and deny-by-default (Q166, M4).** A v2 `proxyRef`/`defaultProxyRef` may name an `EgressProxy` in another namespace, but naming it authorizes nothing.
> Consent is **provider-side**: the proxy owner lists consumer namespaces in `spec.sharing.allowedNamespaces`, mirroring Gateway API's `ReferenceGrant`, where the grant lives in the target namespace. **Absent or empty `sharing` denies**, so a namespace never gains reach because a field was left unset, and every reference that predates the feature keeps resolving in its own namespace exactly as before.
> An unconsented reference fails closed with `ProxyShareNotGranted` and no wiring at all: no NetworkPolicy peer, no CA, no worker pod.
> Three surfaces enforce it rather than one: the controller's consent check gates provisioning; the **provider's** ingress admits a granted namespace through a single peer carrying both a `NamespaceSelector` and a `PodSelector`, which AND (two separate peers would OR and admit every pod in that namespace *and* every workload pod cluster-wide); and the consumer's egress peer is emitted only for a consented reference.
> Traffic needs both halves, so neither side alone opens a path.
> What sharing *does* surrender is per-tenant egress **attribution**, since a shared proxy is a shared egress identity, which is why it suits cooperating tenants or a platform-operated central pool and not mutually-distrusting ones.
> Trust distribution is a **ConfigMap, not a Secret**: only the proxy's public certificate crosses the namespace boundary, and the private key never leaves the provider.
> Withdrawing a grant deletes that ConfigMap and removes the ingress peer, so revocation is real rather than a stopped refresh.
> See [appendix-h §H.9](appendix-h-v2-api-decomposition.md#h9-cross-namespace-proxy-sharing) and the operator runbook [security-operations.md § Sharing an egress proxy across namespaces](../operations/security-operations.md#sharing-an-egress-proxy-across-namespaces).

| Threat Vector | Impact | Mitigation Strategy |
| --- | --- | --- |
| **Unauthorized Cross-Namespace Proxy Use** (egress through another tenant's pool) | High | A cross-namespace `proxyRef` resolves only with **provider consent** (`EgressProxy.spec.sharing.allowedNamespaces`); absent or empty sharing denies, so the pre-M4 same-namespace-only posture is both the default and the unset case. A consumer-side name alone never authorizes the reference: it fails closed with `ProxyShareNotGranted` and the controller wires nothing. Enforcement is dual-side and independent: the provider's NetworkPolicy ingress admits only workload pods in granted namespaces (both selectors in **one** peer, so they AND), and the consumer's egress peer is emitted only for a consented reference; traffic needs both. The proxy's **public certificate only** is distributed, as a ConfigMap, into granted namespaces. The private key never leaves the provider namespace, so a shared proxy cannot be impersonated by a consumer. Revoking a grant deletes the projection and drops the ingress peer. Residual, and inherent to the feature: sharing surrenders per-tenant egress-IP **attribution** between the cooperating namespaces, which is why the field is documented for cooperating tenants and platform pools rather than mutually-distrusting ones. |
| **Host Namespace Escape via Malicious Workflow** (Container Breakout) | Critical | Enforced in three layers. (1) The AGC unconditionally sets `hostPID: false`, `hostNetwork: false`, `hostIPC: false`, and `automountServiceAccountToken: false` on every worker pod, overwriting tenant `PodTemplate` values at pod-creation time. (2) The GMC stamps `pod-security.kubernetes.io/enforce` on the tenant namespace at provisioning time, with the level chosen by `ActionsGateway.spec.securityProfile` — see [§5.3](#53-security-profiles-and-the-privileged-opt-in). The default `baseline` blocks privileged containers, hostPath, dangerous capabilities, and host namespaces via the in-tree PodSecurity admission plugin; no external policy engine is required. (3) Sandboxed container runtimes (Kata Containers, gVisor) are supported via `runtimeClassName` in the `PodTemplate` — optional but strongly recommended for tenants who select the `privileged` profile. See [Appendix B](appendix-b-worker-isolation.md) for tradeoffs. |
| **Supply-Chain Compromise of Worker Image** | High | `WorkerImage` should be digest-pinned; the GMC-injected AGC/proxy images are digest-pinning-*enforced* at startup and the GMC's own image at chart-render time. CI runs `govulncheck` + `trivy` on every PR; release images are multi-arch, keyless-`cosign`-signed with per-architecture SBOM and SLSA build-provenance attestations. Full scanning / signing / provenance detail: [see below](#supply-chain-compromise-of-the-worker-image). |
| **Cross-Job Code Contamination** | High | Enforce absolute 1-Job-Per-Pod isolation. Avoid reusing volumes or host paths between worker pods. Use ephemeral, `emptyDir` volumes for workspace storage. |
| **AGC Token Compromise** | High | The AGC never saves plaintext keys to disk. GitHub App private keys are mounted as read-only volumes with restrictive file permissions (0400). |
| **Token Exchange over a Plaintext Channel** | High | Minting a GitHub App installation access token POSTs an App-JWT (signed with the tenant's RSA/Ed25519 private key) to the token-exchange endpoint and receives a short-lived installation token in the response — both are credential material that a non-HTTPS channel would expose to any on-path observer. The endpoint host is `GITHUB_API_BASE_URL`, which the GMC derives from the gateway's `spec.gitHubURL` and injects on the AGC `Deployment` (Q506); the AGC's own default when it is unset is `https://api.github.com`. Because `gitHubURL` is validated `^https://` at admission, the derived base is always HTTPS. Historically *any* value was accepted, including a plaintext `http://` URL. The token provider now **rejects a non-HTTPS `GITHUB_API_BASE_URL` by default** (validated at AGC/probe startup, so a misconfiguration fails fast rather than leaking on first mint) — TLS for this leg is secure-by-default, not opt-in. The legitimate dev/test case (the e2e suite points the AGC at an in-cluster `fakegithub` over plaintext) is preserved via an **explicit opt-in**: the AGC permits a plaintext base URL only when the stub env (`STUB_AUTH_URL`) is set — a signal a production AGC never carries (it reaches a GMC-provisioned AGC only via `AGC_EXTRA_*` under the testing-only `--allow-agc-extra-env` flag) — and logs the relaxation at startup. The probe and the live egress-IP test pass no opt-in, so they always require HTTPS. The error names the offending URL but never any token/JWT material. See [security-operations.md § GitHub API base URL must be HTTPS](../operations/security-operations.md#github-api-base-url-must-be-https). |
| **GitHub-App-Key Exfiltration via AGC apiserver Egress** | Medium | The AGC apiserver egress rule allows ports 443/6443 with **no `to:` restriction** because the post-DNAT apiserver IP is unpredictable — any-destination is the secure default. Blast radius is bounded (read-only 0440 key mount, no worker apiserver egress, digest-pinned non-root AGC); operators with a stable apiserver CIDR can opt into `--apiserver-cidrs` scoping. Rationale, per-cloud guidance, and the auto-narrowing verdict: [see below](#github-app-key-exfiltration-via-agc-apiserver-egress). |
| **Credential Leak via Logged Error Bodies** | Medium | The AGC, broker client, and probe interpolate upstream GitHub HTTP response bodies into errors that callers log. Some of these bodies carry credential material — the runner-token endpoint's 200 body holds an access token, and `generate-jitconfig`'s body holds the runner JIT registration credential plus RSA key. Before any upstream body is placed into an error or log line it passes through a single shared redactor (`githubapp.SanitizeBody`) that strips credential-shaped substrings (GitHub `gh*_`/`github_pat_` tokens, JWTs, `access_token`/`encoded_jit_config`/`private_key`/`secret` JSON values, and long opaque base64 blobs) and caps the result. Redaction runs before capping so a secret straddling the cap boundary cannot survive in the truncated tail. No secret is ever logged directly; this control hardens the indirect path. |
| **Eviction-Retry API Misuse** | Medium | The AGC calls `POST /repos/{owner}/{repo}/actions/runs/{run_id}/rerun-failed-jobs` using the tenant's installation access token when a worker pod is evicted. The blast radius is bounded: the installation token is scoped to the GitHub App's installation on a specific organization or repository, so the AGC cannot re-run jobs belonging to other tenants or organizations. The `run_id` originates with GitHub in both acquisition tiers — from the job payload delivered by the broker (classic), or from the `workflowRunId` on the scale-set assignment message, which the provisioner records on the worker pod at creation and reads back on eviction (scale-set, Q417). Neither path lets the AGC fabricate or substitute a run ID for a run it did not acquire; the pod annotation is controller-written and controller-read, and an absent or malformed identity is refused rather than defaulted (no re-run is attempted, and `actions_gateway_eviction_recovery_identity_unknown_total` increments). To prevent abuse of the retry path (e.g. a compromised AGC looping re-runs), `maxEvictionRetries` caps the number of automatic retries per run — keyed by run ID alone, so the cap is shared across both tiers rather than doubled — and is enforced before the API call is made. The scale-set path additionally claims each evicted pod under an optimistic lock before calling GitHub, so a re-run is attempted at most once per evicted pod even across AGC replicas. Operators should monitor `actions_gateway_eviction_retries_exhausted_total` to detect abnormal eviction patterns. |
| **DNS Exfiltration Side-Channel** (Unattributed Egress) | Medium | All three per-tenant NetworkPolicies confine port-53 egress to **cluster DNS only** — three OR'd peers (kube-dns, node-local-dns, and the `169.254.0.0/16` link-local block) — closing the unattributed any-resolver side-channel while preserving legitimate resolution. Enforced only by a policy-aware CNI. Full peer breakdown and Q229 verification: [see below](#dns-exfiltration-side-channel-containment). |
| **Proxy as Traffic Interception Point** | Medium | The proxy only handles CONNECT tunneling and does not terminate TLS. It cannot inspect or modify the encrypted payload between the AGC/worker and GitHub. Proxy pods run with a read-only root filesystem and no elevated capabilities. |
| **Worker Exfiltration via an Allowlisted Non-GitHub Destination** (deliberate, bounded relaxation — Q242 G.1) | Medium | Exists **only** when a platform admin opts in to non-GitHub worker egress; the tenant-authorable destinations are gated by a **platform-owned** allowlist (both empty = deny-all-non-GitHub) enforced by webhook plus dual-surface NetworkPolicy/CONNECT gates, with per-tenant attribution and in-cluster DNS containment preserved. Full trade-off record: [see below](#worker-exfiltration-via-an-allowlisted-non-github-destination). |
| **Cross-Tenant Proxy CA Trust** | Medium | The egress proxy's TLS cert is signed by a cert-manager-issued self-signed CA stored in the per-tenant `actions-gateway-proxy-tls` Secret. The AGC pins this CA explicitly (via its trust pool) rather than trusting the cluster's root store, and worker pods install the same CA into a combined `SSL_CERT_FILE` bundle so Runner.Worker's .NET HttpClient accepts the proxy handshake. The cert (`tls.crt`) is projected into both AGC and worker pods via an `Items: [tls.crt]` Secret volume; the private key (`tls.key`) is mounted *only* into the proxy pod itself, so a runner compromise does not yield the ability to forge a proxy cert. Trust is tenant-scoped: each tenant's CA is independent, so a compromised CA in one namespace cannot mint a cert trusted by another tenant's AGC or workers. The proxy's CONNECT listener pins a **TLS 1.2 minimum** explicitly (matching the metrics listener) rather than inheriting the Go default — the worker→proxy leg carries every tenant's GitHub-bound traffic, so its TLS floor is a tenant-isolation boundary and is stated in code, not left implicit (Q127). A gateway may **add** one more root — the private CA fronting a GHES appliance, via `spec.githubCABundleRef` (Q536) — and only ever add: the pool is seeded from the system roots and extended, never replaced, so no supplied bundle can narrow what the AGC will validate. That widening is tenant-scoped in the same way as the proxy CA (it reaches that gateway's own AGC pod and worker pods and nothing else), and it is authored by the same party that already chose `githubURL`, which is the endpoint the trust applies to. |
| **Per-Tenant Metrics Scrape Interception / Unauthorized Read** | Low | The per-tenant AGC and proxy serve `/metrics` over **mutual TLS** on `:8443` (Q69): the listener requires a client certificate signed by that tenant's metrics CA, and there is no bearer-token or `insecureSkipVerify` fallback. The GMC generates a per-tenant metrics PKI (CA → server leaf with the proxy/AGC Service DNS names as SANs, client leaf for the scraper) and writes two Secrets — the server bundle (`actions-gateway-metrics-tls`, mounted into the AGC/proxy pods) and the scraper client bundle (`actions-gateway-metrics-client`, published for the monitoring stack, never mounted into the workload pods so a runner compromise cannot read it). Each tenant's CA is independent, so a client cert from one tenant cannot scrape another's endpoints, and an in-cluster attacker without the client cert cannot read a tenant's metrics or impersonate the endpoint. The metrics port is reachable on ingress only from namespaces labelled `metrics: enabled` (the per-tenant NetworkPolicy metrics-scrape rule, L-8). Scrape wiring is **opt-in** (`metrics.serviceMonitor.enabled`, default off): when on, the GMC creates a per-tenant `ServiceMonitor` for the proxy and AGC presenting the client bundle and verifying the server cert via `serverName` + `ca` — no `insecureSkipVerify`. The metrics Services exist regardless of the toggle; only the scrape config is gated. See [observability.md § Scraping per-tenant AGC and proxy metrics (mTLS)](../operations/observability-metrics-access.md#scraping-per-tenant-agc-and-proxy-metrics-mtls). |
| **Egress IP Change Mid-Session** | Low–Unknown | GitHub's broker protocol is token-based, not IP-bound. Session IDs and bearer tokens carry no IP affinity, so rotating across proxy pods mid-job is expected to work. The Twirp log stream is naturally sticky (long-lived HTTP/2 connection stays on one proxy pod once open). Impact is unknown because GitHub's abuse detection heuristics are undocumented. **Early mitigation: the [Milestone 1](06-implementation-phases.md#milestone-1-wire-protocol-probe-days-14) wire protocol probe explicitly tests broker API calls routed through a multi-pod proxy pool to confirm GitHub does not reject or flag IP variance across `CreateSession → GetMessage → AcquireJob`.** If the probe surfaces a problem, `ClientIP` session affinity on the proxy Service is the low-effort fallback; explicit per-goroutine proxy assignment is the higher-fidelity option if needed. |
| **Proxy Pool Exhaustion** (DoS via proxy saturation) | Medium | HPA `minReplicas` ensures a floor of available capacity. The `PodDisruptionBudget` prevents draining all replicas simultaneously. The platform-owned namespace `ResourceQuota` caps proxy pod count so a misconfigured `maxReplicas` cannot consume cluster CPU. The GMC also surfaces two non-blocking status conditions (Q82): **`ProxyQuotaPressure`** (warning — the pool cannot scale to `maxReplicas` within the quota's remaining headroom) and **`ProxyQuotaExceeded`** (error — replica creates are being rejected by the quota now). They turn a silent ceiling (the HPA wedging at `FailedCreate` under load) into operator-visible signals — each also exported as a gauge for alerting — without rejecting the config (the quota stays the enforcement point). |
| **Denial of Service via Resource Exhaustion** | Medium | The namespace `ResourceQuota` is **platform-owned** — the platform admin sets it on the tenant namespace and GAG operates within it without ever creating or mutating it (Q130). It is the hard cap: a tenant-authored quota was removed pre-1.0 because the tenant could simply raise it. CPU/Memory limits are also defined in the `RunnerGroup` CRD spec. Rogue workflows cannot exceed the tenant quota. Both the GMC and the AGC hold **read-only** (`get`/`list`/`watch`) access to `resourcequotas` so they can compute the proxy and worker quota conditions respectively (Q82); both deliberately hold **no** quota-write verb, keeping least privilege intact (partially subsumes Q122) — they observe the quota but can never create or relax it. The AGC additionally surfaces **`WorkerQuotaPressure`** (warning — workers can't scale to `maxWorkers` within headroom) and **`WorkerQuotaExceeded`** (error — the quota can't admit another worker pod) on each `RunnerGroup`, complementing Q59's configured-ceiling admission gate. The worker footprint behind both conditions — and behind the pre-claim quota gate and the scale-set capacity integer, which share one calculation — matches Kubernetes' own effective-request arithmetic: regular containers and **native sidecars** summed, plain init containers as a `max()` floor, plus `RuntimeClass` pod overhead (Q450). Counting containers alone under-counted a DinD or Kata worker by its sidecar's entire ask, so the gate admitted jobs whose pods the quota then rejected. The footprint also spans the **storage** keys Kubernetes charges through its PVC evaluator (Q453) — `ephemeral-storage`, and the `persistentvolumeclaims` / `requests.storage` (plus per-`StorageClass`) charge of every generic ephemeral volume. Those fail *later* than compute keys, since the PVC is created after the pod is admitted: an uncounted one left a Kata worker `Pending` on an unbound volume holding an already-claimed job. A shape with no such volumes is charged nothing on those keys, so an unrelated `persistentvolumeclaims` ceiling cannot starve it. |
| **Cross-Tenant Pod Preemption via PriorityClass** | High | `priorityClassName` stamps a **cluster-scoped** `PriorityClass`, so the platform owns which classes a tenant may reference: the GMC webhook rejects any name not on `--allowed-priority-classes` (empty = forbid all, the secure default; optional additive, fail-safe ConfigMap source). Both tenant-reachable routes are gated on every tenant-authored kind — `priorityTiers[]` (v1 `ActionsGateway`, v2 `RunnerSet`) and `podTemplate.spec` (v1 `ActionsGateway`, v2 `RunnerTemplate`) — the latter otherwise admitting `system-cluster-critical` from any namespace. Platform should create allowlisted classes `preemptionPolicy: Never` unless cross-tenant preemption is intended. Infra pods (`EgressProxy`/`ActionsGateway` `spec.scheduling.priorityClassName`) are gated by a **separate, disjoint** allowlist `--allowed-infra-priority-classes` (Q284), which takes the same watched-CR augmentation (Q298) behind a four-point disjointness check. **Closed (Q132 v1 tiers, Q289 podTemplate + v2 RunnerSet tiers; Q284 infra allowlist); watched-CR source Q188/Q492, infra parity Q298.** Full detail: [see below](#cross-tenant-pod-preemption-via-priorityclass). |
| **Cross-Tenant Job Acquisition via a Shared Scale-Set Name** | High | A `ScaleSet` set's first `runnerLabel` is its scale-set *name*, and the AGC adopts a scale set by that name against the Actions service its gateway's `githubURL` reaches, so the name is unique per GitHub org/enterprise/repo and **not** per namespace. Two `RunnerSet`s under one org sharing a first label are two AGCs driving one scale set, each acquiring the other tenant's jobs into its own namespace, quota, and egress IPs. The uniqueness guard was namespace-scoped and could not see it; it is now keyed on the gateway's GitHub binding and enforced cluster-wide, from **both** admission paths (the label is on the `RunnerSet`, the scope on its gateway, so either object can complete the pair, the same shape as the Q322 `noProxyCIDRs` guard). Unverifiable reads fail closed, scope comparison is lossy toward rejecting, and a cross-tenant conflict is withheld from the rejection message (logged for the admin) so it cannot be used to enumerate other tenants. `Classic` sets register no scale set and are unaffected. Admission only fires on a write, so the GMC's gateway reconciler re-checks the same inventory every reconcile and reports a pair that predates the guard on the advisory `ScaleSetNameCollision` condition, a Warning Event, and the `actions_gateway_scale_set_name_collision` gauge (Q849). **Closed (Q791 admission, Q849 reconcile).** Full detail: [see below](#cross-tenant-job-acquisition-via-a-shared-scale-set-name). |

### The AGC's cluster-scoped read surface

The AGC's cache and RBAC are namespace-scoped by design: the GMC binds the `agc-tenant-role` `ClusterRole` with a per-tenant **`RoleBinding`**, so every grant in it is confined to the tenant namespace.
Two kinds the AGC genuinely depends on are cluster-scoped, and a `RoleBinding` cannot authorize those — they are bound separately, and cluster-wide, by the per-gateway `agc-clusterrunnertemplate-reader` `ClusterRoleBinding`:

| Kind | Verbs | Why it cannot be namespaced |
|---|---|---|
| `clusterrunnertemplates` (`actions-gateway.com`) | `get`/`list`/`watch` | The platform-authored worker template a tenant `RunnerSet` resolves through `templateRef`. |
| `runtimeclasses` (`node.k8s.io`) | `get`/`list`/`watch` | A worker pod's `ResourceQuota` charge includes its `RuntimeClass` `overhead.podFixed` (`250m`/`160Mi` on the reference Kata shape), and that value exists only on the cluster-scoped object — not on the pod template (Q450). |

This list is the whole of the AGC's cluster-wide authority, and the bar for adding to it is high.
Three properties keep the surface acceptable:

- **Read-only, always.** Neither kind carries a write verb.
  The AGC cannot create, mutate, or delete a `ClusterRunnerTemplate` or a `RuntimeClass`; both are platform-owned (a `RuntimeClass` is typically installed by the runtime's own operator, e.g. kata-deploy).
- **Neither kind carries tenant data.** A `RuntimeClass` holds a handler name, pod overhead, and a scheduling `nodeSelector` — cluster topology, not secrets and not another tenant's configuration.
  A compromised AGC reading them learns which sandbox runtimes the cluster offers, which it can already infer from the templates it is entitled to resolve.
- **Every read fails open.** An AGC whose `ClusterRoleBinding` is absent or stale (an image upgraded ahead of its chart) degrades rather than breaking: the overhead term is omitted from the worker footprint and the quota conditions behave as they did before Q450.
  The grant buys accuracy, so losing it costs accuracy — never availability.

**Only a gateway-scoped AGC holds this binding, and only it needs one.** The `ClusterRoleBinding` is per-gateway, created by the v2 `ActionsGateway` reconciler for the AGC it provisions.
The v1 singleton AGC gets no such binding, and gets by without one because it does not reconcile `RunnerSet`s at all: a `RunnerSet` is served by the AGC of the gateway its `gatewayRef` names, which is the one carrying `GATEWAY_NAME`.

During a v1→v2 migration the tenant namespace holds both AGCs, and an unscoped v1 AGC that reconciled the migrated set anyway would resolve its `templateRef` — reaching for a cluster-scoped kind it holds no grant for, and error-looping on `clusterrunnertemplates … is forbidden` for the whole coexistence window, as measured live on the dogfood cluster (Q466).
The fix was to stop the read, not to widen the grant: declining work that belongs to another controller is what keeps this table as short as it is, and it also stops the two AGCs from running competing listener pools for one `RunnerSet`.

**The partition runs both ways.** A gateway-scoped AGC likewise declines `RunnerGroup`s: a `RunnerGroup` names no gateway — v1 is a namespace singleton — so the AGC that serves it is the one the v1 GMC provisioned, which stamps no `GATEWAY_NAME`.
While that reconciler was registered on every AGC, the migrated gateway's AGC ran a second listener on the v1 group at the *same* `agentIndex` as the v1 AGC's, which means the same agent Secret and the same GitHub runner name — the hazard the disjoint-naming fix above cannot reach, because it separates kinds and not controllers.

Measured live: 409 on every `CreateSession` (153 in ~2.5 minutes, no backoff) and both reconcilers writing the same `RunnerGroup` status (Q535).
Nothing can scope a `RunnerGroup` informer the way `spec.gatewayRef.name` scopes the `RunnerSet` one, so declining the kind is the whole fix.
Net effect: **each AGC process serves exactly one API**, and the two never contend for an object.

The table above is enforced, not just documented.
The shipped rules (`charts/actions-gateway/files/agc-clusterrunnertemplate-reader-rules.yaml`) and the `+kubebuilder:rbac` markers that declare the AGC's needs (`cmd/agc/internal/controller/doc.go`) are hand-synced, and a unit test (`rbac_chart_drift_test.go`, Q454) fails on any divergence: a cluster-wide rule the markers never asked for, a marker verb the chart does not grant, and — the read-only property above — any write verb at all.
A second case asserts the reverse direction for both AGC roles, so a marker added without its chart rule fails the build instead of 403-ing in a real install.

The corresponding operator-visible consequence — a `WorkerQuotaPressure` that can newly trip on a Kata tenant, and how to confirm the grant landed — is in [resourcequota-sizing.md](../operations/resourcequota-sizing.md#pod-overhead-needs-a-cluster-scoped-read) and the [upgrade note](../operations/upgrade.md#worker-quota-accounting-now-counts-native-sidecars-runtimeclass-overhead-and-storage).

### Supply-chain compromise of the worker image

`WorkerImage` SHOULD reference an immutable digest, not a floating tag (see [§3.1](03-api-contracts.md#31-kubernetes-crd-schemas)).
Digest pinning eliminates the "update the tag, get a different binary" attack class.
Operators are expected to restrict the set of permitted registries via cluster admission policy (e.g.
Kyverno, OPA Gatekeeper) — the GMC does not enforce this itself because registry policy is a cluster-wide concern.

#### CI scans every image on every PR

The gateway's own CI runs two supply-chain gates on every PR (`security-scan.yml`): `govulncheck` across all Go modules and `trivy` image scans of all five built images — see [testing.md § Security scanning](../development/testing.md#security-scanning).
The four images built from a minimal/distroless base block on fixable HIGH/CRITICAL findings; the default worker image (built `FROM` the upstream actions-runner) is scanned report-only because its CVEs live in upstream components, with base bumps automated via dependabot.
Tenants supplying their own `WorkerImage` are still expected to scan it themselves. `imagePullPolicy: IfNotPresent` (digest) or `Always` (tag) ensures the kubelet does not serve a stale, possibly tampered local copy.

#### First-party digests are enforced at chart render time

For the four first-party images the chart resolves (`gmc`, and the `AGC_IMAGE`/`PROXY_IMAGE`/`WRAPPER_IMAGE` refs the GMC injects), digest pinning is *enforced*, not advisory, and it is enforced at **chart render time**: the Helm chart fails to render — naming the offending image — while any of the four digests is empty (the `allowFloatingImageTags` value is the shared dev/test opt-out), and the CI `manifest-validate` gate asserts each image's render is rejected, so the check cannot regress to fail-open (Q307).

Render-time enforcement is the primary layer because the GMC's own image has no runtime guard at all (nothing validates the image the GMC runs from), and because catching an unpinned AGC/proxy/wrapper ref at render surfaces it as one clear error rather than a later GMC crash-loop.
The AGC/proxy/wrapper images are *additionally* re-checked by the GMC at startup — it rejects any injected reference not in `image@sha256:<digest>` form (the same `--allow-floating-image-tags` opt-out) — a second layer that also covers deployments that bypass the chart.

#### Released images are signed, SBOM'd, and provenance-attested

All five first-party images additionally carry `org.opencontainers.image.*` provenance labels (`source`/`revision`/`version`/`title`/`description`, with `revision`/`version` stamped from the build's git SHA via `docker-bake.hcl`) so SBOM scanners can trace an image back to its commit, and their Go binaries are compiled with `-trimpath -ldflags=-buildid=` for path-free, reproducible output (a reproducible-build input).

On a `v*` release tag, `publish.yml` pushes those five images to GHCR as **multi-arch OCI indexes** (`linux/amd64` + `linux/arm64`; the Go builder stages cross-compile on `$BUILDPLATFORM`, and the digest operators pin is the index digest) and **signs each one keyless with `cosign`** (sigstore/Fulcio via GitHub Actions OIDC — no long-lived signing key, no stored secret), recursively over the index and every per-arch manifest, and attaches an SPDX-JSON SBOM **per architecture** as a cosign attestation on that architecture's manifest, so a downstream operator can `cosign verify` the publish-workflow identity before deploying and enforce it cluster-wide via an admission policy.

Each image also carries a signed **SLSA build-provenance attestation** (`actions/attest-build-provenance`, Q103) on the index digest via the same keyless Fulcio/Rekor path — authenticated provenance recording the workflow, repo, commit, and trigger (**SLSA Build L2**; buildx's unsigned default provenance is disabled in favour of it, and full Build L3 would require an isolated reusable-workflow builder).
The PR-time `security-scan.yml` already generates each SBOM as a build artifact so that path can't silently break.

#### The publish pipeline itself is hardened

The publish pipeline itself is hardened against upstream-tag hijack: every `uses:` across `.github/workflows/` is pinned to a full commit SHA (the `publish` job holds `id-token: write`, so a mutable action tag repointed at malicious code could otherwise keyless-sign images as the release identity), runtime tool downloads are version-pinned (`cosign` via `cosign-installer`, `syft` via `syft-version`), and Dependabot's `github-actions` ecosystem bumps the SHA pins so they don't rot (Q123).

The keyless signing identity is **tags-only**: `publish.yml` refuses to run from a non-tag ref and `make verify-release` anchors the cosign `--certificate-identity-regexp` to `…/publish.yml@refs/tags/v.*$`, so a signature minted from a branch (e.g. a `workflow_dispatch` from a scratch branch overwriting a released tag) is both prevented and rejected (Q124).
See [security-operations.md § Image provenance](../operations/security-operations.md#image-provenance-signature--sbom-verification) for the operator verification runbook and [release.md § Supply-chain integrity of the pipeline](../operations/release.md#supply-chain-integrity-of-the-pipeline-itself) for the SHA-pin / signing-identity policy.

### GitHub-App-key exfiltration via AGC apiserver egress

The AGC pod holds the tenant's GitHub App private key, and its NetworkPolicy (`buildAGCNetworkPolicy`) admits egress on the Kubernetes API server ports (443 and 6443) with **no `to:` destination restriction** — so a compromised AGC could in principle exfiltrate the key over port 443 to an arbitrary external HTTPS endpoint, not just the apiserver.

This breadth is the **default**, not an oversight: the `kubernetes` Service ClusterIP is DNAT'd by kube-proxy to provider-specific node/apiserver IPs *before* NetworkPolicy is evaluated, so a precise `ipBlock` is not portable and a wrong one silently severs the AGC's apiserver access (the post-DNAT trap that bit the proxy NP in PR #59) — so any-destination must stay the secure default where the post-DNAT apiserver IP is unpredictable.

The blast radius is bounded by compensating controls: the App key is file-mounted read-only at 0440 (never an env var), worker pods carry **no** apiserver egress at all (only DNS + proxy), the AGC image is digest-pinned and the pod runs non-root with a read-only root filesystem, and all *GitHub-bound* AGC traffic still routes through the per-tenant proxy (the apiserver egress rule is scoped to apiserver ports, not GitHub).

**Operators whose platform exposes a stable apiserver endpoint CIDR can now close this residual with a first-class opt-in** (Q145): set the GMC's `--apiserver-cidrs` flag (Helm value `apiServerCIDRs`) to that CIDR set and the AGC NetworkPolicy's 443/6443 rule is scoped to those CIDRs via `ipBlock` — a strict tightening, validated as CIDRs at GMC startup, ports unchanged.
Leaving it empty preserves the any-destination default for clusters where the apiserver IP is unpredictable.

**Why this stays an opt-in and not an auto-tightened default (Q183 feasibility verdict):** the GMC could in principle read the `kubernetes` Service endpoints and scope the rule itself, but those post-DNAT IPs rotate on managed control planes without notice — a snapshot goes stale and breaks the AGC; keeping it live needs an endpoint watch with an unavoidable race window during which the AGC (and the GMC, on the same path) can be locked out of the apiserver it needs to repair the policy; and some CNIs rewrite the target again.
So automatic default-on tightening is not safe or portable, and narrowing stays operator-confirmed and per-cluster.
See [security-operations.md § Tightening AGC apiserver egress](../operations/security-operations.md#tightening-agc-apiserver-egress-the-apiserver-cidrs-allowlist) for per-cloud (EKS/GKE/AKS/kubeadm/kind) guidance and the full verdict, and [appendix-g §G.10](appendix-g-future-enhancements.md#g10-controller-discovered-apiserver-cidr-auto-narrowing) for the deferred auto-narrowing enhancement.

### DNS exfiltration side-channel containment

The per-tenant egress-IP attribution that isolates tenants rests on *all* real egress traversing the tenant proxy, whose source IPs are attributable.
An unrestricted port-53 egress rule (`to: []` ≡ any resolver) would defeat that: any pod — including untrusted worker job code — could smuggle data out by encoding it into DNS queries aimed at an attacker-controlled authoritative server, an unattributed side-channel that never touches the proxy.

All three per-tenant NetworkPolicies (workload, AGC, proxy) therefore confine port-53 egress to **cluster DNS only**, via three OR'd peers:

1. The `kube-dns` / `CoreDNS` Service in `kube-system` (matched by `namespaceSelector` on `kubernetes.io/metadata.name: kube-system` plus `podSelector` on `k8s-app: kube-dns`).
2. The NodeLocal DNSCache pods `k8s-app: node-local-dns` in `kube-system` (same namespace+pod AND-selector), the **redirect backend** on CNIs that rewrite the kube-dns ClusterIP to a per-node cache pod.
   GKE Dataplane V2 (Cilium) does this via a `RedirectService` / Cilium Local Redirect, and enforces egress against that backend's identity (`node-local-dns`, not `kube-dns`), so without this peer DNS is silently dropped and the tenant's AGC crash-loops on its first GitHub token fetch (Q229).
3. For clusters running NodeLocal DNSCache in the classic link-local mode, where pods send DNS to a per-node `hostNetwork` cache at a link-local address no selector can match, the link-local block `169.254.0.0/16` (`ipBlock`, Q136).

All three peers preserve the attribution property: `kube-dns`/`node-local-dns` recurse upstream on the pod's behalf so legitimate resolution (including the proxy's own GitHub-hostname lookups) is unaffected, each is port-53 cluster DNS — never an arbitrary external resolver — and `169.254.0.0/16` is **non-routable and node-scoped**, so the exfiltration channel stays closed while the "any resolver" breadth remains removed (Q105/Q136/Q229).

Like the other egress negatives, this is enforced only by a policy-aware CNI; the reliable CI guard is the authoring-level test `TestBuildNetworkPolicy_DNSEgressRestrictedToKubeDNS`, which asserts every policy's DNS rule selects kube-dns, node-local-dns, **and** the link-local block and is never open.
The node-local-dns peer's runtime effect was verified end-to-end on a live GKE Dataplane V2 cluster (Q229).

### Worker exfiltration via an allowlisted non-GitHub destination

By default the proxy carries GitHub only, so this risk does not exist; it appears **only** when a platform admin opts in to widening worker egress to specific non-GitHub destinations (`EgressProxy.spec.destinationFQDNs` / `destinationCIDRs`).
The residual is honestly stated: a compromised worker (malicious dependency, job RCE) can exfiltrate to any **allowlisted** destination — the allowlist bounds the hole, it does not eliminate it; this is the core trade the feature accepts.

**It is never tenant-openable:** the `EgressProxy` is tenant-authorable, so the destinations are gated by a **platform-owned** allowlist (GMC `--allowed-egress-fqdns` suffix match / `--allowed-egress-cidrs` subnet containment, optionally a watched ConfigMap) enforced by a validating webhook that rejects any off-allowlist entry — **both empty = deny-all-non-GitHub**, the secure default (mirrors `--allowed-priority-classes`, Q132/Q188).

The properties the cheap alternatives throw away are **preserved**: traffic still exits the per-tenant proxy IPs (attribution intact) and DNS still resolves on the in-cluster path (no new exfil channel).
Enforcement is dual-surface from one source of truth — CIDR `ipBlock` / FQDN `toFQDNs` on the pod-egress policy (the hard gate; fail-closed if the CNI can't enforce an FQDN rule, same as Q208) **and** the proxy CONNECT allowlist as defense-in-depth (with resolve-and-dial-the-validated-IP closing the DNS-rebinding window on the CIDR path; denials counted on `actions_gateway_proxy_connect_denied_total`).

The admin footgun (a too-broad suffix/CIDR — `*.googleapis.com`, a CDN, the IMDS endpoint) is bounded by guidance, not code: the docs **lead with an in-cluster caching mirror** as the recommended path and reserve the allowlist for what a mirror genuinely can't proxy.
See [network-architecture.md § Worker egress to allowlisted non-GitHub destinations](network-architecture.md#worker-egress-to-allowlisted-non-github-destinations-opt-in-q242-g1), [security-operations.md § Worker egress destinations](../operations/security-operations.md#worker-egress-destinations-the-egress-allowlist), and the [Q242 plan](../plan/archive/q242-g1-proxy-destination-allowlist.md) for the full trade-off record.

### Cross-tenant pod preemption via PriorityClass

A **cluster-scoped** `PriorityClass` carries a priority value and a `preemptionPolicy` (Kubernetes default `PreemptLowerPriority`), so an unvalidated tenant-chosen class would let a tenant name a high-priority, preempting class and have the scheduler **evict other tenants' running worker pods** — and their egress proxies — to schedule its own, defeating per-tenant isolation.

A tenant can reach a worker pod's `priorityClassName` by **two** routes, and the allowlist gates both — on every tenant-authored kind that carries them:

| Route | Kind | Gated by |
|---|---|---|
| `priorityTiers[].priorityClassName` | v1 `ActionsGateway` / `RunnerGroup` (Q132); v2 `RunnerSet` (Q289) | v1 `ActionsGateway` webhook; v2 `RunnerSet` webhook |
| `podTemplate.spec.priorityClassName` | v1 `ActionsGateway.runnerGroups[]`, v2 `RunnerTemplate` (Q289) | v1 `ActionsGateway` webhook; v2 `RunnerTemplate` webhook |

The `podTemplate` route existed because it is a full `corev1.PodTemplateSpec` that the AGC copies verbatim into the worker pod, overriding `priorityClassName` only when a `priorityTiers` tier matches.
Gating only the v1 tiers therefore left the guarantee unenforced: Kubernetes ships `system-cluster-critical` (value `2000000000`, `preemptionPolicy: PreemptLowerPriority`) in **every** cluster and does **not** restrict it to `kube-system` — verified against a real apiserver — so a tenant could preempt every other tenant with no setup at all.
The same applied to the v2 `RunnerSet`'s own `priorityTiers`: the `RunnerSet` is tenant-authored (it is the v2 front door) and its matched tier is stamped over the template, so its tiers are gated in the `RunnerSet` webhook exactly as the v1 tiers are in the `ActionsGateway` webhook.
All routes are now rejected at admission unless the class is on the platform allowlist.

#### The platform owns the allowlist, and an empty one forbids every class

The platform owns *which* classes a tenant may reference: the platform admin pre-creates the `PriorityClass` objects (the GMC never creates cluster-scoped objects — consistent with the Q121/Q122/Q130 platform-ownership model) and lists their names in the GMC `--allowed-priority-classes` flag; the GMC validating webhook rejects any `priorityClassName` not on the allowlist.

The allowlist is **additionally** sourced from a watched cluster-scoped `PriorityClassAllowlist` CR (`--priority-class-allowlist-name`, Q188; a ConfigMap until Q492) so an admin can add a class without a flag edit + restart — the CR is **additive** to the flag and **fail-safe** (a missing/malformed object falls back to the static flag allowlist and never silently widens it), so the guardrail is preserved.
An **empty allowlist forbids every named class** (secure default), so out of the box no tenant can set a `PriorityClass` at all; an *unset* `podTemplate.spec.priorityClassName` stays admissible, since the secure default must not forbid ordinary unprioritized workers.

The cluster-scoped `ClusterRunnerTemplate` is exempt: a tenant cannot create cluster-scoped objects, so gating it would only bind the platform against itself — the same reasoning that lets it declare privileged containers (§H.4/§H.6).

Because `PriorityClass` is global, the platform should create allowlisted classes with `preemptionPolicy: Never` unless cross-tenant preemption is genuinely intended for that tier — see [security-operations.md § Priority classes](../operations/security-operations.md).
The dead tenant-settable per-tier `preemptionPolicy` field was removed pre-1.0 (it was never wired to pods and was a tenant-controlled preemption lever the platform must own). **Closed (Q132 v1 tiers, Q289 podTemplate + v2 `RunnerSet` tiers); watched-object source added Q188.**

#### A ValidatingAdmissionPolicy closes the webhook residual

The webhook residual — `RunnerGroup` has no webhook of its own, so direct `runnergroups` RBAC bypassed the allowlist, and webhooks never re-validate stored objects — is closed by the `priorityclass-allowlist-guard` `ValidatingAdmissionPolicy` ([G.7](appendix-g-future-enhancements.md#g7-validatingadmissionpolicy-for-direct-runnergroup-priorityclass-enforcement), Q289): it gates every `runnergroups` write from any principal against a `paramKind` allowlist object and re-validates existing objects on update.

The policy also matches the v2 carriers of the same two routes — `runnersets` (`priorityTiers`) and `runnertemplates` (`podTemplate.spec.priorityClassName`), both v2alpha1 and v2beta1 (Q323): those kinds have `failurePolicy: Fail` webhooks, so for them the policy is the webhook-outage/bypass backstop plus stored-object re-validation, restoring the defense-in-depth v1 had (`ClusterRunnerTemplate` stays exempt as platform-authored, matching its webhook exemptions).
What remains outside any admission gate is a stored off-allowlist object that is never written again — the [upgrade sweep](../operations/security-operations.md#upgrading-previously-ungated-priorityclassname-fields-are-now-gated) finds those.

#### The policy's `paramKind` must be a CRD, never a core type

The `paramKind` that makes the allowlist readable at admission time is a **CRD** — the cluster-scoped `PriorityClassAllowlist`, which is also the object the GMC watches — and that choice is a security property, not a convenience.
With a *core*-type `paramKind` (a ConfigMap, as shipped before **Q492**) param resolution could stop working permanently: every matched write was then denied with `no params found for policy binding` even though the ConfigMap was present at the referenced name (**Q444**; observed on Kubernetes 1.35.5 and 1.36.1).

Because `parameterNotFoundAction: Deny` resolves params before per-object matching, that denied *every* matched write cluster-wide, recoverable only by a kube-apiserver restart — unavailable on a managed control plane.
It could also fail **open**, silently enforcing a stale allowlist from the torn-down informer's frozen cache, which is the more dangerous half.
The trigger is deleting the policy's *binding* (what `helm uninstall` does): the apiserver keys one shared param informer per `paramKind`, tears it down once no binding names that kind, and never restarts it.

A CRD `paramKind` gets a fresh dynamic informer per context and survives the same transition — measured against a ConfigMap on one apiserver by [`scripts/e2e/vap-param-informer-check.sh`](../../scripts/e2e/vap-param-informer-check.sh), and guarded end to end by `scripts/e2e/chart-reinstall-check.sh` in CI.
Mechanism: [`../plan/archive/q444-vap-param-resolution.md`](../plan/archive/q444-vap-param-resolution.md).

#### Deletion-only updates are exempt from re-validation (Q518)

Stored-object re-validation has one legitimate write it must not deny: the teardown write.
Once a class is removed from the allowlist while stored objects still name it, *every* later write to those objects is denied — including the AGC's finalizer-removal update — so a tenant deleted in that state wedged permanently: the finalizer could never clear and the namespace hung in `Terminating` (Q499).
Both admission layers therefore exempt **deletion-only updates**: the guard policy via the `exclude-deletion-only-updates` `matchCondition`, and every GMC webhook's `ValidateUpdate` via the shared `validation.DeletionOnlyUpdate` helper (`cmd/gmc/internal/webhook/validation`), so the layers cannot disagree.

The exemption is deliberately **narrower than "the object is being deleted"**.
A write is exempt only when *both* hold:

- **`metadata.deletionTimestamp` is set.** It is written only by the API server in response to a delete, is immutable once set, and cannot be smuggled in by a client (a client-supplied value on create/update is ignored), so "deleting" is not a state a tenant can enter to dodge validation — and it is a one-way state: the object can only proceed to removal.
- **The incoming spec is identical to the stored spec** (CEL `object.spec == oldObject.spec`; `apiequality.Semantic.DeepEqual` in the webhooks).
  This is what makes the exemption provably unable to widen anything: the admitted object can reference exactly what the already-stored object references, nothing more.
  The rare-but-legal *spec change on a deleting object* stays fully validated, so even a deleting object can never acquire a new class name (or any other newly-forbidden spec state) through the exemption — no reasoning about controller behavior on deleting objects is needed.

The simple `deletionTimestamp`-only form was considered and rejected: it would admit spec changes on deleting objects, whose safety rests on the claim that no controller provisions from a deleting object — true of ours today, but a behavioral invariant rather than an admission invariant, and the narrow form costs one extra CEL disjunct.
The exemption applies uniformly at the top of each `ValidateUpdate` (not just to the PriorityClass check): with an unchanged spec, every spec-derived check reproduces its stored verdict, and the checks that consult *external* state — an allowlist, an eligibility label, a sibling list — are exactly the ones that can wedge teardown the same way. **Closed (Q518).** Operator-facing detail: [security-operations.md § Narrowing the allowlist](../operations/security-operations.md#narrowing-the-allowlist-drain-stored-references-first).

#### The two PriorityClass allowlists must stay disjoint (worker vs. infra, Q284)

`priorityClassName` is reachable from a **second** family of surfaces: `spec.scheduling.priorityClassName` on the tenant-authorable `EgressProxy` and v2 `ActionsGateway`, which prioritizes the **infra** pods (the per-tenant egress proxy pool and the AGC control plane).
This is gated too — but by a **separate** allowlist, `--allowed-infra-priority-classes`, and the separation is a durable governance invariant, not an implementation detail:

- **The two allowlists must be disjoint, and disjointness is enforced wherever an overlap can be introduced.** Reusing the worker allowlist is the non-obvious trap.
  Infra pods must sit *above* workers — that is the point of prioritizing them — so an operator would add a high, preempting class to let an `EgressProxy` name it.
  An allowlist has one namespace of meaning, so that class would immediately be nameable from a **worker** pod (`RunnerTemplate.podTemplate.spec.priorityClassName`) as well.
  Any tenant could then lift its *workers* to infra priority and preempt *other tenants'* proxy pods: the gate meant to protect the proxy becomes the mechanism for evicting it, and the priority ordering inverts.
  Two disjoint allowlists make an infra class unreachable from any worker surface by construction.
- **`priorityClassName` is the one `PodScheduling` field behind a gate**, and the asymmetry is principled.
  The rest of the block (`nodeSelector`/`tolerations`/`affinity`/`topologySpreadConstraints`) is tenant-settable and ungated because it is a choice about the tenant's *own* traffic — it weakens *attribution* (two tenants may egress via one IP), not *isolation*, and cannot preempt another tenant.
  Priority is a cluster-wide, cross-tenant preemption lever: distinct in kind, so gated where placement is not.
  And because `system-*` classes are nameable from any namespace (above), an ungated infra `priorityClassName` would be the same cluster-wide preemption escape the worker gate closes — reopened on a new path. **Any future `PodTemplateSpec`/priority passthrough on a tenant-writable CR must be audited for this.**

- **A boot-time check is not sufficient once either set is editable at runtime.** Both allowlists take an additive, fail-safe dynamic half from the watched `PriorityClassAllowlist` CR (Q188 worker, Q298 infra), so an edit is a route to an overlap the startup check never sees.
  Disjointness therefore holds at four points, each covering what the others structurally cannot: the **startup check** on the two flags; a **CRD CEL rule** rejecting a write that puts one class on both of the object's lists; the **GMC reconciler**, which refuses a CR whose list collides with the *other surface's flag* — invisible to CEL, which cannot read a controller flag — by dropping **both** dynamic sets back to the flags rather than applying half a pair; and the **admission read path**, where the two allowlist instances are paired so a name reaching both is admitted on neither surface.
  The last is the backstop that makes the invariant structural rather than procedural: no ordering window or future second writer of the dynamic sets can convert an overlap into an admitted pod.
- **Wholesale refusal is the point, not collateral damage.** A partially applied pair is exactly how the overlap becomes real, so a bad CR withholds both lists — the observable cost is that recently self-serviced classes stop being accepted together, never that an unintended class becomes allowed.

The infra webhook carries the same residual as the worker one (G.7): direct RBAC on the underlying resource bypasses the webhook, and stored objects are never re-validated.
A `ValidatingAdmissionPolicy` backstop for the infra allowlist, following the G.7 `paramKind` pattern, is the planned closure; the worker-facing `priorityclass-allowlist-guard` policy reads only `allowedPriorityClasses`, since the kind it exists for (`runnergroups`, which has no webhook) carries no infra scheduling block. **Closed (Q284): flag, disjointness check, and both webhooks (`EgressProxy` extended; a new v2 `ActionsGateway` webhook stood up, since none existed).
Dynamic augmentation Q298.**

### Unbounded job intake via the installation's default runner group

Every other control in this section bounds what a tenant's worker may *do* once it is running.
This one bounds *who may cause it to run at all*, and it is the only control in the threat model that lives at GitHub rather than in the cluster: no NetworkPolicy, PSA label, admission policy, or `ResourceQuota` can reach it.

A GitHub **runner group** carries a repository-access policy.
A scale set registered into the installation's *default* group is targetable by every repository that group admits, which in a typical organization is all of them.
The consequence on a shared cluster: a repository outside the tenant puts the tenant's runner label in `runs-on`, and GitHub assigns the job to the tenant's scale set.
The AGC provisions a worker in the tenant's namespace, against the tenant's `ResourceQuota`, egressing from the tenant's proxy IPs.

Nothing about pod-level isolation fails here, which is what makes it easy to miss.
The worker still runs under the namespace's PSA level, its own `NetworkPolicy`, and the tenant's quota.
What is unbounded is *intake*: whose workflow content executes there, whose logs and job metadata land in the tenant's namespace, and whose traffic leaves on the IPs an allowlist elsewhere may trust as that tenant's.

`RunnerSet.spec.runnerGroup`, inheriting `ActionsGateway.spec.defaultRunnerGroup`, binds the set to a named group instead (Q712, §H.4).
Two properties make it a boundary rather than a hint:

- **Unresolvable fails closed.** A name the installation has no group for leaves the set `Ready=False`/`RunnerGroupNotFound` and registers no scale set.
  The obvious alternative, falling back to the default group, is a silent *widening* triggered by a typo, which is the worst possible direction for a security default.
- **An adopted scale set is reconciled, not left.** A set registered by an earlier run keeps the group it was created in unless the declared group moves it, so the field means the same thing on an existing set as on a new one.

**What stays outside GAG's control, and must stay documented.** GAG never creates a runner group and never edits which repositories one admits; both are the platform admin's, at GitHub.
So the guarantee is exactly "the tenant's runners live in the group you named" and never "only your repositories can reach them" — a named group whose access policy is *All repositories* is the default group with extra steps.
See [tenant onboarding](../operations/tenant-onboarding.md#bind-a-runner-set-to-a-github-runner-group).

The field is tenant-authored, and the [whose-fact](../development/api-review.md#ask-whose-fact-it-is) question resolves benignly: a tenant naming a group that is not theirs volunteers *its own* runners to that group's repositories.
It costs the tenant that lied and escalates nothing across tenants, which is why it is not gated by a platform allowlist the way `priorityClassName` is. **Closed (Q712).**

### Cross-tenant job acquisition via a shared scale-set name

The second forge-side namespace, and the one where the whose-fact question does *not* resolve benignly.
A `ScaleSet` set's first `runnerLabel` is its scale-set name, and the AGC adopts a scale set **by name** against the Actions service its gateway's `githubURL` reaches, creating one only when the name is free.
That name is unique per GitHub org, enterprise, or repo; the Kubernetes namespace is invisible to it.

Two `RunnerSet`s in different namespaces whose gateways name one org and one first label are therefore two AGCs driving a **single** scale set.
Both open a session on it, and GitHub distributes that scale set's jobs across both: each tenant acquires jobs belonging to the other, executing another organization's workflow content in its own namespace, against its own quota, egressing from its own attributed IPs.
It is the runner-group threat's mirror image.
That one lets an outside repository reach *in* to a tenant's runners; this one lets a tenant reach *out* and take another tenant's jobs, and unlike the runner group it costs the *other* tenant rather than the one that misconfigured.

The uniqueness guard predated this and was **namespace-scoped**, so it did not see the case at all.
It is now scoped to the GitHub binding and enforced cluster-wide, from both admission paths (Q791, §H.4): the label lives on the `RunnerSet` and the scope on its gateway, so exactly like the `noProxyCIDRs` bypass pair (Q322), either object can complete the collision and each is checked from its own side.
The gateway half is load-bearing rather than defensive: a `RunnerSet` applied before its gateway has no resolvable scope and is admitted (§H.7), so a RunnerSet-only guard would be bypassable by apply order alone.

Two properties matter for the guard being a boundary:

- **Unverifiable fails closed.** If the cluster-wide read errors, the write is rejected rather than admitted on faith, and the scope comparison is deliberately lossy in the *rejecting* direction (case-insensitive host and path, port dropped), so a near-miss encoding of one org cannot slip between two keys.
- **The rejection does not enumerate tenants.** A conflicting set in the applying tenant's own namespace is named; a cross-tenant one is withheld from the API error and written to the GMC log instead.
  The `RunnerSet` is tenant-authored, so a message naming the holder would let anyone able to create one in their own namespace probe another tenant's namespaces and label usage.

**Admission cannot see a pair that already exists, so the reconciler reports one (Q849).** A validating webhook fires on a write, and both halves of the guard are complete against validated writes in every apply order, which is exactly why the remaining exposure is the state no write produced.
Two ways in: an upgrade from a release before Q791, where the colliding pair is already stored and nothing re-validates it, and a window with the webhook uninstalled.
In both, two AGCs drive one scale set and each tenant acquires the other's jobs until somebody happens to re-apply an object.

The GMC's `ActionsGateway` reconciler reads the same cluster-wide claim inventory each reconcile and reports what it finds on the gateway:

- the advisory **`ScaleSetNameCollision`** condition (`ScaleSetNameShared` / `ScaleSetNamesUnique`),
- a Warning **Event** on the transition into it, including the absent→`True` first observation, which is the upgrade case,
- the `actions_gateway_scale_set_name_collision` gauge, so it is alertable ([observability](../operations/observability-metrics.md#full-metrics-reference)).

It reports rather than enforces, and does not gate `Ready`.
GAG cannot pick which of two tenants loses the name, and refusing to provision the AGC would take down both tenants rather than the one that is misconfigured, so the operator gets the signal and makes the call ([troubleshooting](../operations/troubleshooting.md#actionsgateway-reports-scalesetnamecollision)).
Both boundary properties carry over: the condition names only the applying gateway's own `RunnerSet`s (a cross-namespace holder goes to the GMC log, since the gateway's status is tenant-readable), and an unreadable inventory leaves the last verdict standing rather than writing `False` from a read that did not happen.
The reconciler's read is cached, unlike admission's: it reports a persisted state rather than arbitrating a write, so the stale-cache race the webhook guards against does not apply.

**What stays outside GAG's control.** A scale set registered at GitHub by something *other* than GAG (ARC, or a hand-registered set) is not in the cluster and cannot be seen at admission or at reconcile.
The AGC adopts such a set by name rather than failing, which is the intended behaviour for migration ([migrating from ARC](../operations/migration-from-arc.md)) and the reason ARC and GAG must not run one scale-set name concurrently. **Closed (Q791 admission, Q849 reconcile).**

---

## 5.3. Security Profiles and the Privileged Opt-In

Worker pod security is defense-in-depth: PSA enforcement at the API server, AGC-enforced invariants on the PodSpec, and an optional sandbox runtime layer.
The default posture is secure; tenants opt into looser policy explicitly.

### The three profiles

`ActionsGateway.spec.securityProfile` is one of three values; the GMC stamps the corresponding label on the tenant namespace.

| Profile | PSA label | Container escape risk | Typical use |
|---|---|---|---|
| `baseline` *(default)* | `pod-security.kubernetes.io/enforce: baseline` | Low — privileged/host namespaces/hostPath/dangerous caps all blocked | Normal CI: builds, tests, integration runs |
| `restricted` | `pod-security.kubernetes.io/enforce: restricted` | Very low — adds runAsNonRoot, drop ALL caps, seccomp RuntimeDefault | High-isolation tenants; compliance workloads |
| `privileged` | `pod-security.kubernetes.io/enforce: privileged` | High — admission imposes no restrictions | DinD, Buildah-without-sandbox, kernel-module workflows |

The default is `baseline`.
A tenant must explicitly set `securityProfile: privileged` on the `ActionsGateway` to allow privileged worker pods — there is no silent path to it.
And setting it is necessary but **not sufficient**: `privileged` is only *eligible* in a namespace a platform administrator has opted in — see [Privileged eligibility is a platform decision](#privileged-eligibility-is-a-platform-decision) below.

In-runner image builds (`docker build`, the most common heavyweight runner workload) are where this profile choice bites hardest.
The [In-runner image builds](../operations/in-runner-image-builds.md) operator guide maps each build approach — BuildKit-rootless, Kaniko, Sysbox, and privileged Docker-in-Docker (DinD) — to the profile it needs, so most builds land on `baseline` rather than `privileged`.

### Privileged eligibility is a platform decision

The tenant owns the `ActionsGateway` CR, so a tenant can *self-select* `securityProfile: privileged` at create — only *downgrades* of an existing CR are otherwise gated (see [No silent profile downgrades](#no-silent-profile-downgrades) below).
That left a self-granted-escalation gap: `privileged` makes the GMC stamp the namespace PSA to `privileged`, the cluster's least-restrictive pod-security posture, so a tenant who could create an `ActionsGateway` could grant their own namespace that posture at will (Q133).

Whether a namespace may run privileged workers is a **platform** decision, not a tenant one.
The GMC validating webhook therefore rejects any `ActionsGateway` requesting `securityProfile: privileged` — at **create or update** — unless its namespace carries the label

```
actions-gateway.github.com/privileged-profile: allowed
```

applied by a platform administrator.
This is the same trust model as the `actions-gateway.github.com/tenant` marker (see [Constraining the GMC's PSA-stamping privilege](#constraining-the-gmcs-psa-stamping-privilege)): a label on the *namespace*, an object the tenant does not own and cannot edit, set by a trusted identity.
The GMC never sets it itself.

The granting value is the enum keyword `allowed`, **not** `true`, deliberately: a boolean-looking label value invites the YAML coercion footgun (`privileged-profile: true` parses as a boolean, which a string label value then rejects or mishandles — and YAML 1.1 coerces `yes`/`no`/`on`/`off` too), so a non-boolean keyword is both safer to author and self-documenting.
The value is matched exactly.
This is the project-wide convention for new operator-set labels and annotations — see [Kubernetes API conventions](../development/kubernetes-conventions.md#label--annotation-value-conventions).

The gate is **fail-closed**: an absent label, any value other than `allowed`, or a namespace the webhook cannot read all leave privileged **ineligible** and the request is rejected.
Non-privileged profiles (`baseline`, `restricted`, and the empty default) never consult the label and are unaffected.

The two privileged gates are independent and both must pass: the namespace must be labelled eligible (this check), *and* on an update the rank-downgrade guard still requires the `actions-gateway.github.com/allow-profile-downgrade` annotation (anything → `privileged` is a downgrade in rank).
The label is the platform's eligibility decision; the annotation is the tenant's deliberate, auditable act of relaxing their own profile.

This is a webhook check rather than a CRD CEL `XValidation` rule because the decision depends on a label of a *different* object (the namespace), which a spec-scoped CEL rule cannot read.
The webhook reads the namespace's current label through the uncached API reader, so a tenant cannot smuggle eligibility in through the CR.
Operators grant eligibility per [Tenant Onboarding](../operations/tenant-onboarding.md); a tenant who hits the rejection is pointed at [troubleshooting: privileged profile rejected](../operations/troubleshooting.md#privileged-securityprofile-rejected-namespace-not-eligible).

### Constraining the GMC's PSA-stamping privilege

Stamping a PSA label on a namespace requires `patch` on `namespaces`, which is cluster-scoped — RBAC cannot express "only namespaces the GMC manages".
Left unconstrained, a compromised GMC pod could relabel `kube-system` (or any namespace) to `privileged`.
Two controls confine the grant:

- **A trusted marker label.** A namespace is eligible for GMC management only if an administrator has labelled it `actions-gateway.github.com/tenant: "true"`.
  This is a tenant onboarding pre-condition (see [Tenant Onboarding](../operations/tenant-onboarding.md)).
  The GMC never sets this label itself — doing so would defeat the control.
- **The `namespace-psa-guard` ValidatingAdmissionPolicy.** Scoped to the GMC ServiceAccount only, it denies any namespace `UPDATE` unless the *existing* namespace already carries the marker (read from `oldObject`, which the requester cannot forge), and denies any change to a namespace label other than the six `pod-security.kubernetes.io/*` keys or to any annotation.
  It ships in the Helm chart (`charts/actions-gateway/templates/namespace-psa-guard.yaml`, gated on `admissionPolicy.enabled`).

The policy deliberately does **not** ban writing `privileged` outright, because `securityProfile: privileged` is a supported per-tenant opt-in and the GMC legitimately stamps it on those tenants' namespaces.
The marker scope confines the blast radius to GMC-managed tenant namespaces.

### No silent profile downgrades

Separately from the GMC's stamping privilege, the GMC validating webhook (`ValidateUpdate`) prevents a tenant's profile from being *silently* weakened.
The profiles are ranked `privileged(0) < baseline(1) < restricted(2)`; on update the webhook compares the old and new rank:

- An **upgrade** (`baseline → restricted`) — hardening — is always allowed.
  So is a no-op change.
- A **downgrade** (`restricted → baseline`, or anything → `privileged`) is **rejected** unless the object carries the annotation `actions-gateway.github.com/allow-profile-downgrade: "true"`.

This closes the accidental path without trapping operators.
A stray `kubectl apply` of an older manifest — or one that *drops* the field and lets it re-default to `baseline` — does not carry the annotation, so it is refused rather than quietly relaxing isolation (an empty value is treated as its `baseline` default for the comparison, so dropping the field cannot sneak a downgrade through).
But a *deliberate* relaxation — for example rolling back a `baseline → restricted` hardening attempt that turned out to break the tenant's pods at admission — needs only a two-field edit (set the annotation, set the profile), not a destructive recreate of the whole `ActionsGateway`.
Requiring the explicit annotation keeps the relaxation auditable while keeping the safe direction (hardening) cheap and the unsafe direction (relaxing) intentional.

The check is a webhook rather than a CRD CEL `XValidation` rule because the decision depends on `metadata.annotations`, which a spec-scoped CEL rule cannot read.
(`gitHubAppRef.name` is deliberately left mutable: changing it is the supported credential-rotation mechanism — see §3.2.)

This is a guard against *accidental/silent* downgrade, not an absolute boundary: an operator who holds the `allow-profile-downgrade` annotation write (i.e. edit access to the CR) is trusted to relax the profile on purpose, and one with direct namespace `patch` rights could edit the PSA labels regardless.
A *compromised GMC* relabelling namespaces is a separate threat, constrained by the `namespace-psa-guard` ValidatingAdmissionPolicy above, not by this rule.

### Mixing privileged and non-privileged workloads

PSA enforcement is namespace-scoped: every pod in a namespace is evaluated against the same profile.
A tenant that needs both privileged and non-privileged workloads deploys **two `ActionsGateway` CRs in two namespaces** — for example, `myteam-builds` with `securityProfile: privileged` for DinD jobs and `myteam-tests` with the default `baseline` for everything else.
Workflows route to the appropriate gateway via `runs-on:` labels matching RunnerGroups in each.

This is the same separation operators already use to assign different quotas, priority tiers, and node selectors to different workload classes — the security profile rides on the existing namespace boundary rather than introducing a new sub-namespace concept.

If finer granularity (per-RunnerGroup profile within one `ActionsGateway`) becomes necessary, the path forward is documented in [Appendix G](appendix-g-future-enhancements.md) as a future enhancement.

### Pairing `privileged` with a sandbox runtime

Selecting `privileged` removes the API-server-side admission guard but does not remove the option of sandbox-based isolation.
For tenants who need privileged *semantics* (a real Docker daemon, full syscall surface) but don't trust the workload code, the recommended pattern is:

```yaml
apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: builds
  namespace: myteam-builds
spec:
  securityProfile: privileged
  runnerGroups:
    - runnerLabels: [self-hosted, dind]
      podTemplate:
        spec:
          runtimeClassName: kata-containers   # or gvisor
          containers:
            - name: runner
              securityContext:
                privileged: true
```

`runtimeClassName: kata-containers` runs the worker pod inside a lightweight VM.
Privileged-inside-Kata grants the workload full control of a microVM kernel, not the host kernel — a container escape lands in a throwaway guest kernel rather than on the node.
See [Appendix B](appendix-b-worker-isolation.md) for the full tradeoff between `runc`, `gvisor`, and `kata-containers`.
(The `RuntimeClass` name is whatever the platform admin registered; the Q226 reference architecture registers `kata` → handler `kata-qemu`.)

**Kata bounds the kernel, not the pod network.** A micro-VM does not isolate the cloud metadata server: Q226 measured the node's GCE service-account token endpoint returning `HTTP 200` from *inside* a Kata guest on GKE.
Escaping the container is no longer a path to the node, but minting node credentials over the pod network still is.
Kata must therefore be paired with a metadata control (GKE Workload Identity, AWS IMDSv2 hop-limit 1, or a NetworkPolicy denying `169.254.169.254/32`) and `automountServiceAccountToken: false` — which the AGC already sets unconditionally on worker pods.
Kata is one layer, not a posture; the capabilities a DinD workload still needs inside the guest approach `privileged`, so the whole guarantee rests on the VM boundary.
See [Running DinD workloads under Kata § What Kata does not buy you](../operations/kata-dind-workloads.md#what-kata-does-not-buy-you).

**The platform can enforce this pairing, and no longer only recommend it.** `runtimeClassName` lives in the `PodTemplate`, but in v2 the `PodTemplate` need not be tenant-owned: a cluster-scoped `ClusterRunnerTemplate` is platform-authored, and the Q172 resolution ladder (`templateRef` → `ActionsGateway.spec.defaultTemplateRef` → the annotated cluster-default `ClusterRunnerTemplate`, then fail closed) means a `RunnerSet` that names no template of its own resolves to one the platform wrote.
Marking a sandbox-runtime template as the cluster default makes the pairing the outcome a tenant gets by default rather than one they have to choose.
GAG ships the pod shape for this as `kata-dind` in the [runner template library](../operations/runner-template-library.md), which deliberately does **not** ship marked as a default: which template a cluster defaults to is the platform team's decision to make, not the project's.

What remains tenant-owned is a `RunnerSet` that authors its own namespaced `RunnerTemplate` and names it explicitly.
That template cannot declare a privileged container at all (rejected at admission), so the escape hatch it offers is a weaker pod shape, not a more privileged one.

### Privileged worker containers are admitted only under the privileged profile

The GMC validating webhook (`validateRunnerGroups`) rejects a `securityContext.privileged: true` container or init-container in any RunnerGroup's `PodTemplate` — **except** when the `ActionsGateway` has explicitly set `securityProfile: privileged`.
Under `baseline` (the default) and `restricted`, a privileged container is refused at admission, secure by default with no silent opt-in.
The check is keyed on the *effective* profile, so an omitted/empty `securityProfile` is treated as `baseline` and still rejects.

This makes the webhook coherent with the rest of the profile model: the `privileged` profile stamps the namespace PSA to `privileged` so the pod is actually admittable, and the documented Kata/DinD pattern above (a `privileged: true` runner under `securityProfile: privileged`) is now accepted end to end rather than being blocked by the webhook while PSA would have allowed it.
The webhook covers only the GMC-managed `ActionsGateway` path; a directly-applied `RunnerGroup` CR bypasses the webhook entirely, so **Pod Security Admission — stamped per the namespace's profile — is the real enforcement backstop for both paths**.
Operators who hit the rejection are pointed at [troubleshooting: privileged worker container rejected](../operations/troubleshooting.md#privileged-worker-container-rejected-by-admission).

### Floor invariants apply at every profile

The AGC enforces the following on every worker pod regardless of profile, by overwriting the merged PodSpec before submission:

- `Spec.HostPID = false`
- `Spec.HostNetwork = false`
- `Spec.HostIPC = false`
- `Spec.AutomountServiceAccountToken = false`
- `Spec.ServiceAccountName = <worker SA>` (no K8s API credentials projected)
- Reserved env vars (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, the payload mount path) are stamped by the controller

A tenant who sets `securityProfile: privileged` still cannot enable host namespace sharing or expose Kubernetes API credentials inside the worker pod.
These invariants are non-negotiable across all profiles.

### Secure-by-default pod SecurityContext and resources

Floor invariants above are non-negotiable. *On top of them*, the AGC stamps a secure-by-default `SecurityContext` and resource requests/limits onto every worker pod.
These defaults **gap-fill only** — an explicit value in the tenant `PodTemplate` always wins, so a tenant can opt out of any individual default (e.g. `runAsNonRoot: false` for a root-based image) without escalating to the `privileged` profile.

The hardening scales to the namespace's PSA profile (propagated to the AGC via the `SECURITY_PROFILE` env var the GMC sets on the AGC Deployment):

| Profile | SecurityContext defaults stamped |
|---|---|
| `baseline` *(default)* | Pod-level `runAsNonRoot: true` + `runAsUser: 1001` + `seccompProfile: RuntimeDefault`. Deliberately **not** `allowPrivilegeEscalation: false` or capability drop — `baseline` PSA permits in-job privilege escalation (`sudo`) and many CI jobs rely on it. |
| `restricted` | The above, plus the per-container PSA-restricted floor: `allowPrivilegeEscalation: false` and `capabilities.drop: [ALL]`. Without these the namespace's PodSecurity admission would reject the pod, so the AGC stamps them to make the profile usable rather than self-blocking. |
| `privileged` | None — this profile exists precisely so DinD / host-capability workloads can opt in via their own `PodTemplate`. |

**Why `runAsUser: 1001` accompanies `runAsNonRoot: true`.** kubelet's `runAsNonRoot` enforcement can only *prove* a container is non-root against a **numeric** UID.
The default worker image (`ghcr.io/actions/actions-runner`, and the `cmd/worker` image built from it) declares its user **by name** — `USER runner` — which kubelet cannot resolve to a UID at admission.
With `runAsNonRoot: true` but no numeric UID, kubelet rejects the pod outright (`CreateContainerConfigError: container has runAsNonRoot and image has non-numeric user`), so an unmodified RunnerGroup would fail *every* job.
The AGC therefore gap-fills the runner image's own UID (1001) whenever non-root is being enforced, letting kubelet verify non-root without changing which user the runner actually runs as.
The gap-fill is skipped when a tenant sets `runAsNonRoot: false` (a root-based image), so it never contradicts an explicit opt-out, and an explicit `runAsUser` always wins.
(Q115)

`readOnlyRootFilesystem` is **not** defaulted on any profile: the GitHub Actions runner writes to its work, diagnostics, and home directories at runtime, so a read-only root would break essentially every job, and it is not part of the PSA `restricted` floor.
Tenants who can run with a read-only root may set it (plus the writable `emptyDir` mounts the runner needs) explicitly in their `PodTemplate`.

Resource requests **and** limits default to `500m` CPU / `1Gi` memory on every profile when the tenant container declares neither.
This moves a worker pod off Best-Effort QoS — the first thing the kubelet evicts under node pressure, which otherwise burns the eviction-retry budget fast.
A single-container worker pod with the defaults is Guaranteed QoS.
The eviction-retry budget backs it up on both acquisition tiers as of Q417 ([§4](04-operational-flows.md#on-the-scale-set-tier-q417)), so the QoS default is a first line of defence rather than the only one — on either tier.

## 5.7. Workload identity: the no-PEM delegation model

A gateway authenticates to GitHub as a GitHub App by signing a short-lived App JWT and exchanging it for an installation token.
There are two trust models for *where the App private key lives*, expressed as the two members of the `spec.credentials` discriminated union ([appendix-h §H.15](appendix-h-v2-api-decomposition.md#h15-other-breaking-changes-worth-batching)):

- **`githubApp` — the possession model (default, Q196).** The App's RSA private key lives at rest in a namespace `Secret`, mounted into the AGC, which signs the JWT in-process.
  This is the secure-by-default option: the key is confined to the tenant namespace under the GMC's Secret-read controls, and admission/RBAC are unchanged from v1.
- **`workloadIdentity` — the delegation model (Q197).** **No App private key is ever in the cluster.** The AGC holds only the non-secret App identity (`appId`/`installationId`) and a reference to an *external signer*.
  It proves its own pod identity to that signer's trust anchor and the anchor signs the App JWT on its behalf.
  The AGC never reads, holds, mounts, or logs the App key — there is no `privateKey` field on this member by construction.

`workloadIdentity` is on the strict-upgrade direction of the secure-by-default principle (it *removes* a stored credential), so it is offered as an explicit opt-in member; the in-cluster-PEM `githubApp` stays the default.
Choosing `workloadIdentity` regresses no property: the GitHub token-exchange channel still requires HTTPS, the App JWT still carries a `jti` replay defense and a 10-minute expiry, and the union CEL still enforces exactly one credential member.

**MVP signer — HashiCorp Vault transit + Vault Kubernetes auth.** The first external signer is Vault transit, chosen because it is kind-validatable end to end without a cloud account.
The flow holds no long-lived secret in the cluster:

1. The AGC reads its **kubelet-projected ServiceAccount token** fresh from disk — a short-lived token minted by the kubelet, not a stored `Secret`.
2. It presents that token to **Vault Kubernetes auth** (`POST auth/<mount>/login` with `{role, jwt}`); Vault verifies it via the cluster `TokenReview` API and returns a short-lived Vault **client token**, cached only in memory and re-fetched on lease expiry.
3. It asks **Vault transit** (`POST <transit>/sign/<key>`) to sign the App JWT's `header.payload` with an RSA key Vault holds — RSASSA-PKCS1-v1_5 over SHA-256, i.e. JWS `RS256`.
   The private key never leaves Vault.

The signing material's location is abstracted behind a `githubapp.Signer` interface (`JWTAlg`/`Sign`), so cloud KMS/HSM signers (AWS/GCP/Azure) add as new implementations + new `signer.provider` union members without another breaking change. **No code path logs, returns in an error, or env-passes the projected ServiceAccount token, the Vault client token, or the produced signature** — Vault error responses surface only operational messages (`permission denied`) and HTTP status.
The Vault address is HTTPS-by-default (the ServiceAccount token transits it at login); a plaintext address is permitted only under an explicit dev/test opt-in, mirroring the GitHub-API-base-URL rule above.

Operator configuration is in [tenant-onboarding.md § Workload-identity credentials](../operations/tenant-onboarding.md#workload-identity-credentials-external-signer).
The GMC provisions a workload-identity AGC end to end (Q201): it stamps the signer config env, projects a Vault-audience-scoped ServiceAccount token volume (mounted read-only; never an env var), and runs the AGC under its per-gateway ServiceAccount — the in-cluster identity the operator binds to a Vault Kubernetes-auth role out of band.
The AGC's `vaultsigner` provider then logs in to Vault with that projected token and delegates the App-JWT sign to Vault transit, so the gateway authenticates to GitHub with no App key in the cluster.
The live no-PEM round-trip is covered by an in-cluster (dev-mode) Vault kind e2e.

> **NetworkPolicy egress to Vault (Q202).** The per-tenant AGC NetworkPolicy default-denies egress except DNS + GitHub + the kube API.
> Vault's `address` is an opaque URL, not a selectable peer, so the GMC takes the peer from `signer.vault.networkPolicy`: a pod/namespace selector (in-cluster Vault) or a CIDR (external Vault).
> When set, the GMC adds a **scoped** AGC→Vault egress rule — that one peer on the Vault API port parsed from `address` — to the per-gateway AGC NetworkPolicy only (worker pods never get it).
> It is emitted **only** for `credentials.type: WorkloadIdentity` with a Vault signer; a possession-model (`githubApp`) gateway keeps strict default-deny and the rule is a strict tightening that is only ever added, never a broaden-to-all-egress.
> Leave `networkPolicy` unset on a non-enforcing CNI (kindnet) or to manage the rule out of band — the egress posture is unchanged in that case.
> As with every egress negative this is enforced only by a policy-aware CNI.
>
> `signer.vault.networkPolicy` is a shared `EgressPeer` (selector | CIDR + optional explicit `port`) — the one descriptor future tenant-delegated egress holes (cloud KMS signers, telemetry endpoints) reuse so the v2 API freezes one consistent shape (Q204).
> For Vault the `port` is left unset and derived from `address`; the secure default is unchanged — still a single scoped peer, never a broaden-to-all-egress.
> See [appendix-g §G.9](appendix-g-future-enhancements.md#g9-networkpolicy-egress-extensibility-signers-telemetry-job-dependencies).

---

← [Operational Flows](04-operational-flows.md) | [Back to index](README.md) | Next: [Implementation Phases →](06-implementation-phases.md)
