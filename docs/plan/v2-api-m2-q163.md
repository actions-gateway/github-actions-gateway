# v2 API M2 (Q163) — EgressProxy reconciler + RunnerTemplate webhook

Session-scoped implementation plan for **Q163 — v2 API M2: data-kind ("noun")
reconcilers**. Parent plan: [v2-api.md](v2-api.md) § "M2 — Data kinds (nouns)".
Design source of truth: [appendix-h](../design/appendix-h-v2-api-decomposition.md)
§H.4, §H.7, §H.8.

**Scope:** M2 nouns only — same-namespace, no control kinds (`ActionsGateway` /
`RunnerSet` reconcilers are M3a). Builds on M1 (Q149, #352): the `v2alpha1`
(`actions-gateway.com`) types, CRDs, CEL, and status contract already exist.

## Deliverables

1. **`EgressProxy` reconciler (GMC).** Reconciles a standalone `EgressProxy` into
   a working proxy pool, owning each child via a controller owner-reference on the
   `EgressProxy` (clean cascade GC, §H.8). Same-namespace only.
   - Owned children, all named `<ep>-proxy`:
     - **Deployment** — mirrors v1 `buildProxyDeployment` (hardened SecurityContext,
       required pod anti-affinity, proxy TLS cert mount, proxy container, probes).
     - **Service** — ClusterIP, proxy port.
     - **HPA** — min/max/targetCPU from spec.
     - **PDB** — `minAvailable: 1`.
     - **NetworkPolicy** (when `managedNetworkPolicy`, default true) — egress
       lockdown to DNS + GitHub CIDRs; ingress from workload pods. Secure-by-default;
       no regression vs v1's proxy NetworkPolicy. Needs the shared `IPCache`.
     - **Proxy TLS cert Secret** `<ep>-proxy-tls` — self-signed, SANs for the
       `<ep>-proxy` Service DNS names ("cert/CA wiring").
   - **Per-EgressProxy identity label** `actions-gateway.com/egress-proxy: <name>`
     on every child + the pod template. This is load-bearing two ways:
     1. **Selector isolation** — multiple `EgressProxy`s per namespace must not
        collide on a shared `app: proxy` selector (v1 could assume one proxy/ns).
        Every Deployment/Service/PDB/HPA/NetworkPolicy selector and the pod
        anti-affinity key on this label.
     2. **Free observability win** (M2 deliverable, §H.8) — per-`EgressProxy`
        Deployment ⇒ proxy metrics carry the proxy/gateway identity automatically.
   - **Status** — uniform contract (§H.7): `Ready` + `observedGeneration` +
     `readyReplicas`, using the M1 `conditions.go` vocabulary.

2. **`RunnerTemplate` / `ClusterRunnerTemplate` validating webhook (GMC).** Pure
   data kinds; the reserved-pod-field rejection that v1 did by *silent override* at
   pod-build time (`provisioner.go`) becomes *author-time rejection* (§H.4, §H.7).
   M1 CEL already rejects the scalar pod-level fields (`serviceAccountName`,
   `host{PID,Network,IPC}`, `automountServiceAccountToken`). The webhook adds the
   per-container checks that exceed the CEL cost budget:
   - **Reserved proxy env vars** — reject `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`
     (case-insensitive) and `PROXY_CA_CERT_PATH` on any container/initContainer of
     **both** kinds. These are unconditionally controller-injected on the worker
     container in v1; a template that sets them would be silently overwritten.
   - **Privileged containers** — reject on the **namespaced `RunnerTemplate`**
     (a tenant must not self-author a privileged worker shape); **allow on
     `ClusterRunnerTemplate`** (cluster-scoped ⇒ platform-authored golden
     privileged templates — DinD/sysbox — the documented purpose of the
     cluster-scoped kind, §H.4/§H.6).

## Key scope decision — privileged containers (FLAG for review)

There is a documented tension the dispatcher should confirm:

- **§H.4's reserved-field list omits `privileged`** (lists serviceAccountName,
  host*, automountServiceAccountToken, proxy env vars only).
- **M1's own note** ([v2-api.md](v2-api.md) M1 task list) says the M2 webhook
  rejects "privileged containers and proxy env vars".

These conflict. Resolution chosen here, erring toward the **secure-by-default**
side (a missing security control is worse than a present one):

> Reject privileged on the **namespaced** `RunnerTemplate`; **allow** on
> `ClusterRunnerTemplate`. Reject proxy env vars on **both**.

Rationale:
- A template webhook **cannot** make v1's *profile-aware* privileged decision: the
  `RunnerTemplate` references no `ActionsGateway`, so it cannot read
  `securityProfile`. An *unconditional* privileged rejection would break the
  cluster-scoped kind's documented golden-privileged-template purpose.
- The kind split is decidable at the template layer (no profile needed), mirrors
  the existing "privileged is a platform decision, not tenant-settable" stance
  (`validatePrivilegedEligibility`), and is **no weaker than v1**: Pod Security
  Admission (stamped per the gateway's `securityProfile`, unchanged in v2) remains
  the runtime enforcement backstop for both kinds, exactly as v1's webhook comment
  already calls it.

If the dispatcher prefers PSA-only (no webhook privileged check, matching §H.4
literally), removing the namespaced-kind check is a one-line deletion.

## Metrics mTLS — deferred to M3a (not a regression)

v1's proxy Deployment also mounts a per-tenant metrics-mTLS bundle and a
ServiceMonitor, under a metrics CA **jointly owned with the AGC**. That joint CA
arrives with the `ActionsGateway` reconciler in M3a. For the standalone M2
`EgressProxy`, exposing a metrics listener with no cert would either serve
plaintext (a security regression) or nothing — so M2 omits the metrics port/listener
entirely. The proxy still tunnels; the identity label (already stamped) makes the
series carry the proxy identity once the M3a listener is wired. The proxy binary
boots fine without the metrics-mTLS env (`cmd/proxy/main.go` treats it as optional).

## Where the code lives — GMC, not AGC

The webhook and the EgressProxy reconciler both live in the **GMC** (the
cluster-singleton). The AGC is deployed per-tenant, so it cannot host a
cluster-wide admission webhook; the GMC already serves the v1 ActionsGateway
webhook and already imports the AGC api module. The GMC imports
`agc/api/v2alpha1` for the `RunnerTemplate` types.

## Task checklist

- [ ] Register `agcv2alpha1` in the GMC scheme (`main.go` + integration suite).
- [ ] `egressproxy_builder.go` — child builders (Deployment/Service/HPA/PDB/NP/cert).
- [ ] `egressproxy_cert.go` — self-signed proxy cert for the `<ep>-proxy` SANs.
- [ ] `egressproxy_controller.go` — reconcile + apply* (owner-refs) + status + SetupWithManager (Owns children, IPCache watch).
- [ ] RBAC markers for `egressproxies` (+ `/status`); regenerate `config/rbac`.
- [ ] `webhook/v2alpha1/runnertemplate_webhook.go` — two validators + webhook markers; regenerate `config/webhook`.
- [ ] Wire both in `cmd/gmc/cmd/main.go` (reconciler + webhook + IPCache).
- [ ] Chart sync: `make chart-crds chart-rbac chart-webhook`.
- [ ] envtest: EgressProxy reconcile (children created+owned, defaulting, owner-ref GC, status); RunnerTemplate/ClusterRunnerTemplate webhook (reject proxy env + privileged split, admit clean).
- [ ] Docs: design (appendix-h M2 status), operations (new owned resources / webhook rejections), website per-capability, STATUS (isolated commit, remove Q163 row).
- [ ] `make check` green.
