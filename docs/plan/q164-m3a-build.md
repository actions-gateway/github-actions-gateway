# Q164 — M3a build plan (single-gateway parity)

Working plan for the M3a core build. Tracks the two reconcilers and the
provisioner owner-ref seam. Companion to the milestone plan in
[v2-api.md](v2-api.md) (§ M3a) — that doc holds the parity checklist this build
must close; this doc holds the implementation decisions.

## Scope (this PR)

- GMC `ActionsGateway` (v2) reconciler — provisions the per-gateway AGC control
  plane; **does not** provision the proxy pool (that is the M2 `EgressProxy`
  reconciler) and **does not** stamp PSA labels (that is Q175's
  `NamespacePSAReconciler`).
- AGC `RunnerSet` reconciler — runtime ref resolution (gatewayRef/templateRef/
  proxyRef) + fail-closed conditions + worker provisioning.
- Provisioner owner-ref seam — parameterize the provisioner over an owner
  abstraction so it can own-ref a real `RunnerSet`, with v1 `RunnerGroup`
  untouched.
- envtest coverage in both `internal/controller/integration/` suites.
- Single-gateway only (no gatewayRef field-selector scoping — that is M3b/Q167).
- Proxy required (no `proxyMode: Direct` — Q168 deferred).

## Design decisions

### Provisioner owner-ref seam (AGC concurrency core)

The provisioner is pervasively typed to `*v1alpha1.RunnerGroup`. v2 differs in
two ways that reshape the seam, not just the owner identity:

1. **Pod shape source.** v1 reads `PodTemplate`/`WorkerImage` off `rg.Spec`. v2
   `RunnerSet` carries neither — they come from the resolved `RunnerTemplate`/
   `ClusterRunnerTemplate` (templateRef).
2. **Proxy source.** v1 uses process-wide `Provisioner.HTTP(S)Proxy`/
   `ProxyTLSSecretName` (one proxy per AGC, from AGC env). v2 wires workers to
   the per-`RunnerSet` resolved `EgressProxy` (proxyRef → else gateway
   defaultProxyRef).

**Approach: a `Target` interface + a `ResolvedSpec` value.**

- `Target` (provisioner package): owner identity for the OwnerReference and pod
  labels/naming/admission-key, plus `Resolve(ctx) (*ResolvedSpec, error)` which
  re-reads the fresh provisioning inputs on every acquired job (preserves Q117
  live-edit semantics). A non-nil error ⇒ **fail-closed**: the job fails, no
  worker pod is created.
- `ResolvedSpec`: the fully-resolved, already-defaulted per-job inputs — pod
  template, worker image, eviction/quota/TTL tunables, ceilings (PriorityTiers/
  MaxWorkers), proxy wiring (HTTP/HTTPS/NoProxy + proxy CA secret), and the
  PSA-scaled security profile. v1's process-wide proxy/security and the
  `settingsFor` defaulting move into the v1 adapter that builds a `ResolvedSpec`;
  v2's adapter builds it from RunnerSet + RunnerTemplate + EgressProxy.

`buildPod`/`buildSecret`/`provision`/`ceilingCheck`/`handleEviction`/admission
consume `Target`+`ResolvedSpec` instead of `*v1alpha1.RunnerGroup`. The
multiplexer/listener already hold no `RunnerGroup` reference (only `Group`/
`Namespace` strings + factory closures), so they are untouched.

`HandlerFor(rg)`/`AdmitFor(rg)` keep their v1 signatures (wrap a v1 adapter), so
the v1 controller and tests are byte-for-byte unchanged. New
`HandlerForTarget`/`AdmitForTarget` (or `Handle`/`Admit` taking `Target`) serve
v2.

**Worker pod owner label.** v1 worker pods carry `actions-gateway/runner-group:
<name>`. v2 worker pods carry `actions-gateway.com/runner-set: <name>` (new
domain) so the two controllers' Pod watches/reapers never cross-wire. The
`Target` supplies the owner label set; `activePodCount`/reaper filter on it. The
shared `actions-gateway/component: workload` label stays (workload NetworkPolicy
selector) on both.

### GMC ActionsGateway (v2) reconciler

Mirrors v1 provisioning **minus** the proxy pool (now `EgressProxy`) and PSA
(now `NamespacePSAReconciler`). Owns (controller owner-refs for GC, §H.8):

- AGC ServiceAccount + worker ServiceAccount
- AGC RoleBinding → `agc-tenant-role` ClusterRole
- metrics mTLS certs (server + client Secrets) — jointly owned w/ AGC
- AGC NetworkPolicy (egress: DNS + k8s API) and workload NetworkPolicy
  (egress: DNS + proxy:8080; default-deny ingress)
- AGC Service (metrics)
- AGC Deployment — credential file mount (never env), proxy egress env
  (`HTTP(S)_PROXY` → resolved EgressProxy Service, `NO_PROXY`), proxy CA mount
  (from `<ep>-proxy-tls`), metrics TLS mount, `SECURITY_PROFILE` from the
  namespace `security-profile` label (read-only; not stamped), `LOG_LEVEL`,
  tracing env, `GITHUB_ORG_URL`, `WORKER_SERVICE_ACCOUNT`, `PROXY_TLS_SECRET_NAME`.

**Proxy resolution.** Resolve `spec.defaultProxyRef` → same-namespace
`EgressProxy`. Proxy is **required** in M3a: defaultProxyRef unset or EgressProxy
absent ⇒ fail-closed (`Ready=False`, a Proxy* reason), AGC not wired. Service
addr `https://<ep>-proxy.<ns>.svc.cluster.local:8080`, CA secret `<ep>-proxy-tls`
(reuse `proxyResourceName`/`egressProxyTLSSecretName`).

**Status.** `Ready` + `observedGeneration`; `AGCAvailable`,
`CredentialUnavailable`, `Degraded` per the uniform contract (conditions.go).

**Finalizer.** `actions-gateway.com/gmc-cleanup` (already defined). Cleanup =
delete owned children (most are owner-ref GC; explicit best-effort delete for
parity), then remove finalizer.

### AGC RunnerSet reconciler

- Watches `RunnerSet` + (enqueue mappers) `ActionsGateway`, `EgressProxy`,
  `RunnerTemplate`, `ClusterRunnerTemplate`, worker `Pod`s, `ResourceQuota`.
- Resolve `gatewayRef` (same-ns ActionsGateway), `templateRef` (RunnerTemplate or
  ClusterRunnerTemplate by `kind`), `proxyRef` (EgressProxy; else
  gateway.defaultProxyRef). Missing ⇒ `Ready=False` /
  `GatewayNotFound`/`TemplateNotFound`/`ProxyNotFound`, fail-closed (no listeners
  wired until refs resolve).
- Once resolved, start/maintain the multiplexer/agent-pool exactly like the
  RunnerGroup controller, with the provisioner `Target` carrying the resolved
  template + proxy.
- Reaper / unschedulable / quota mirror the RunnerGroup helpers, filtering on the
  v2 worker-set label, reading tunables off RunnerSet.

## Status / parity checklist
Tracked in [v2-api.md](v2-api.md) § "Per-field / -condition parity checklist".
Close each row as implemented + tested.

## Testing
- envtest (gmc): v2 ActionsGateway → AGC Deployment + SA + RoleBinding + NPs +
  metrics Secrets + owner-refs; proxy wiring from defaultProxyRef; fail-closed
  when proxy/credential missing.
- envtest (agc): RunnerSet ref resolution (each NotFound condition), fail-closed,
  worker provisioning + owner-ref to the RunnerSet.
- `make test-race` (touches the provisioner/multiplexer core).
- kind e2e (job→pod→proxy→GitHub): per the task, M3a minimum is envtest; e2e may
  land with M3b — note the deferral in the PR if so.
