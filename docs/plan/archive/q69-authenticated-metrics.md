# Q69 — Authenticated secure-serving for proxy + AGC metrics

**Status:** ✅ Done — uniform mTLS shipped for the AGC + proxy `/metrics` on `:8443`.
**Size:** M
**Labels:** `security` `infra`
**Queue row:** Q69 (removed)

## What shipped

- GMC issues a per-tenant metrics CA + server cert + scraper client cert
  (`cmd/gmc/internal/controller/metrics_cert.go`), stored in two Secrets
  (`actions-gateway-metrics-tls` server bundle, `actions-gateway-metrics-client`
  scraper bundle) reconciled by `ensureMetricsCerts` and GC'd via owner refs.
- AGC serves `/metrics` mTLS on `:8443` via controller-runtime `TLSOpts`
  (`RequireAndVerifyClientCert` + client CA), no `FilterProvider`, no RBAC change
  (`cmd/agc/metrics.go`).
- Proxy serves `/metrics` mTLS on a dedicated `:8443` listener; `/healthz` +
  `/readyz` stay plaintext on `:8081` for kubelet probes (`cmd/proxy/proxy.go`).
- NetworkPolicy metrics-scrape ingress moved `:8081 → :8443`; the
  `metrics=enabled` namespace gate remains as defense-in-depth.
- Tests: GMC cert-gen + mTLS handshake, proxy mTLS accept/reject/wrong-CA +
  not-on-health-port, AGC options + handshake, builder volume/mount/port + Secret
  builders, and a GMC integration assertion that both Secrets are reconciled.

## Goal

Make the per-tenant proxy and AGC `/metrics` endpoints serve over authenticated
HTTPS, the way the GMC already does — closing the gap where they are
unauthenticated plain HTTP gated only by the L-8 NetworkPolicy namespace
selector.

## Decision (signed off)

**Uniform mTLS** for both the AGC and the proxy: each serves `/metrics` over
HTTPS and requires a client certificate verified against a per-tenant CA issued
by the GMC. Rationale:

- The proxy is a deliberately minimal, stateless CONNECT tunneler with **zero
  Kubernetes dependencies** (no ServiceAccount, no kube client, no kube-API
  egress). The GMC's own auth mechanism — `TokenReview`/`SubjectAccessReview`
  via `filters.WithAuthenticationAndAuthorization` — is made of kube-API calls,
  so adopting it on the proxy would require granting the proxy kube-API
  reachability + client-go + auth-delegator RBAC, **regressing that isolation**.
  mTLS authenticates scrapers cryptographically with no kube-API dependency.
- Doing the AGC the same way (controller-runtime's metrics server accepts raw
  `TLSOpts`, so `ClientAuth: RequireAndVerifyClientCert` + a client CA works
  without a `FilterProvider`) keeps both per-tenant components on one mechanism
  and avoids **all** RBAC changes: no per-tenant `ClusterRoleBinding` to
  `system:auth-delegator`, no GMC RBAC expansion, no cluster-scoped objects to
  garbage-collect.
- This reuses the GMC's existing per-tenant cert machinery (`generateProxyCert`
  / `buildProxyCertSecret` in `cert.go`). The GMC itself keeps its kubebuilder
  default (`TokenReview`) — coherent split: per-tenant components use the GMC's
  per-tenant certs; the singleton GMC uses the cluster's SA-token auth.

One per-tenant **metrics CA** signs a **server cert** (SANs cover the proxy +
AGC Service DNS names) and a **scraper client cert**. The AGC and proxy mount
the server bundle (`ca.crt` + `tls.crt` + `tls.key`); the scraper client bundle
is published in a Secret for the monitoring stack to consume.

## Current state (as of this plan)

### AGC
- `cmd/agc/main.go:70` — `metricsBindAddress = ":8081"`.
- `cmd/agc/main.go:179` — `Metrics: metricsserver.Options{BindAddress: metricsBindAddress}`.
  No `SecureServing`, no `FilterProvider`, no cert → **plain HTTP**.
- AGC NetworkPolicy (`buildAGCNetworkPolicy`, `builder.go:275`) already allows
  egress to the kube API (443 + 6443) and ingress on the metrics port only from
  monitoring namespaces (`metrics=enabled`).
- AGC RBAC: `agc-tenant-role` ClusterRole (`config/agc-tenant-role/agc_tenant_role.yaml`),
  bound per-tenant via a namespaced RoleBinding. **No** tokenreviews/subjectaccessreviews.

### Proxy
- `cmd/proxy/proxy.go:140` — `mux.Handle("/metrics", promhttp.Handler())` on the
  health listener (`:8081`), **plain HTTP**.
- `cmd/proxy/proxy.go:40-43` — `TLSCertFile`/`TLSKeyFile` already exist but apply
  only to the **CONNECT** listener (`:8080`); the health/metrics listener is
  always plaintext (comment at proxy.go:41 makes this explicit).
- The proxy **already has a GMC-issued self-signed TLS cert** mounted at
  `/etc/actions-gateway/proxy-tls/` (`generateProxyCert` in `cert.go`,
  `buildProxyCertSecret` / `buildProxyDeployment`). The cert's SANs already cover
  the proxy Service DNS names — it can serve the metrics port too.
- Proxy `go.mod` has **no** k8s deps; the Deployment sets no `ServiceAccountName`
  (uses the namespace `default` SA).

### L-8 NetworkPolicy gate (shared)
- `metricsScrapeIngressRule()` (`builder.go:144`) admits ingress to the metrics
  port only from namespaces labeled `metrics=enabled`. Applied to both the proxy
  NP and the AGC NP. This stays as defense-in-depth regardless of the auth work.

### GMC reference (the model to extend)
- `cmd/gmc/cmd/main.go:127-156` — `metricsserver.Options{SecureServing: true,
  FilterProvider: filters.WithAuthenticationAndAuthorization, ...}` + optional
  cert-manager cert (`CertDir`); falls back to a controller-runtime self-signed
  serving cert when no cert path is given.
- `config/rbac/metrics_auth_role.yaml` — ClusterRole `metrics-auth-role`:
  `create` on `tokenreviews` + `subjectaccessreviews` (the auth-delegator set).
- `config/rbac/metrics_reader_role.yaml` — ClusterRole `metrics-reader`:
  `get` on `nonResourceURLs: /metrics` (granted to the scraper SA).
- `config/prometheus/monitor.yaml` — ServiceMonitor scrapes `scheme: https`,
  `bearerTokenFile: .../serviceaccount/token`, `insecureSkipVerify: true` by
  default (cert-manager TLS verification is an opt-in patch).
- controller-runtime is **v0.24.1** in both modules.

## Port topology

A new dedicated mTLS metrics port `metricsPort = 8443` on **both** components.
Rationale: the proxy's `:8081` listener serves the kubelet liveness/readiness
probes (`/healthz`, `/readyz`); blanket-mTLS on that port would break probes
(kubelet presents no client cert). A dedicated metrics port keeps probes plain
on `:8081` and metrics fully mTLS-at-handshake on `:8443`. The AGC has no health
probes today, so its metrics simply move `:8081 → :8443` for symmetry.

| Port | AGC | Proxy |
|---|---|---|
| 8080 | — | CONNECT tunnel (existing TLS) |
| 8081 | (freed) | `/healthz` + `/readyz` plain (kubelet probes) |
| 8443 | `/metrics` mTLS | `/metrics` mTLS |

## Plan — GMC cert issuance (`cmd/gmc/internal/controller/`)

1. **`cert.go`** — add `generateMetricsCerts(ag)` returning
   `(caPEM, serverCertPEM, serverKeyPEM, clientCertPEM, clientKeyPEM, error)`:
   - self-signed CA (RSA-2048, `IsCA`, `KeyUsageCertSign`);
   - server leaf signed by the CA — SANs = proxy Service DNS names + AGC Service
     DNS names; `ExtKeyUsageServerAuth`;
   - client leaf signed by the CA — `CN=actions-gateway-metrics-scraper`;
     `ExtKeyUsageClientAuth`.
   The CA private key is **not** persisted; the whole bundle is regenerated on
   expiry/absence, exactly like `generateProxyCert`.
2. **`builder.go`** — two new Secrets:
   - `actions-gateway-metrics-tls` (`ca.crt` + `tls.crt` + `tls.key`, server
     bundle) — mounted RO into AGC + proxy at `/etc/actions-gateway/metrics-tls/`.
   - `actions-gateway-metrics-client` (`ca.crt` + `tls.crt` + `tls.key`, scraper
     bundle) — published for the monitoring stack; not mounted into our pods.
3. **`actionsgateway_controller.go`** — reconcile both Secrets alongside the
   existing proxy-cert reconcile (idempotent: regenerate only on missing/expiring,
   mirroring the proxy-cert path).

## Plan — AGC (`cmd/agc/main.go`)

- Move the metrics bind address `:8081 → :8443`.
- Enable `SecureServing: true`, `CertDir: /etc/actions-gateway/metrics-tls`,
  `CertName: tls.crt`, `KeyName: tls.key`, and a `TLSOpts` func setting
  `ClientAuth = RequireAndVerifyClientCert` + `ClientCAs` loaded from
  `metrics-tls/ca.crt`. **No** `FilterProvider` (no kube auth → no RBAC change).
- Follow the established proxy-cert pattern: secure when the cert files are
  present (GMC always mounts them in production); plain fallback only when absent
  (dev/test), logged loudly. Update the `metricsBindAddress` comment.

## Plan — Proxy (`cmd/proxy/`)

- Keep the health listener (`/healthz`, `/readyz`) plain on `:8081`; **remove**
  `/metrics` from it.
- Add a third listener: `/metrics` over mTLS on `:8443`
  (`tls.Config{Certificates: [server], ClientAuth: RequireAndVerifyClientCert,
  ClientCAs: caPool, MinVersion: TLS1.2, NextProtos: ["http/1.1"]}`). New env
  `PROXY_METRICS_*` for the cert/key/CA file paths (GMC sets them); when unset,
  metrics fall back to plain on the health mux (dev/test), mirroring the existing
  CONNECT-TLS gate.

## Plan — Deployments + NetworkPolicy (`builder.go`)

- AGC Deployment: metrics `containerPort 8081 → 8443`; mount the
  `actions-gateway-metrics-tls` Secret RO (mode `0o440` + `fsGroup 65532`, as the
  existing cert mounts).
- Proxy Deployment: add `containerPort 8443`; mount the same Secret RO.
- `metricsScrapeIngressRule()`: gate `metricsPort (8443)` instead of `8081`
  (one shared function → both AGC NP and proxy NP follow). `:8081` keeps no
  pod-ingress rule (kubelet probes already rely on node-exemption today — no
  change). The `metrics=enabled` namespace selector stays as defense-in-depth.

## Tests

- **GMC cert** (`cert_test.go`): `generateMetricsCerts` produces a CA that
  verifies both leaves; server cert has the expected SANs + `ServerAuth`; client
  cert has `ClientAuth`.
- **Proxy** (`proxy_test.go`): metrics listener rejects a no-client-cert request
  (TLS handshake / 403) and a wrong-CA client cert; accepts a CA-signed client
  cert (200, metrics body). Probes on `:8081` still answer plain.
- **AGC**: unit test on the TLS-config builder (no-cert → handshake fail;
  valid client cert → 200) — extracted into a testable helper so it doesn't need
  a full manager.
- **e2e (deferred / Tier-A)**: in-cluster scrape with the client bundle — folded
  into the scrape-wiring follow-up (open question 3).

## Docs to update

- `docs/design/05-security.md` — record the AGC/proxy metrics auth posture
  (extend the M-5 / L-8 lineage; note the proxy isolation trade-off resolved).
- `docs/plan/security.md` — update the L-8 row / status-at-a-glance.
- `docs/design/02-architecture.md` — metrics table / prose if endpoints change.
- `docs/design/03-api-contracts.md` — any new flags/ports.
- `docs/operations/troubleshooting.md` — runbook for 401 on metrics scrape
  (missing token/cert, missing `metrics-reader`, missing `metrics=enabled` label).
- `docs/STATUS.md` — remove Q69 when done (isolated commit).

## Open questions / deferred

1. **Scrape wiring** — there is no per-tenant ServiceMonitor/PodMonitor or
   metrics-port Service today (the proxy Service exposes only `:8080`; the AGC
   has no Service). Q69 closes the **serving** side (endpoints now require a
   client cert); wiring an actual scrape config — adding a metrics-port Service
   for the proxy, an AGC metrics Service, and ServiceMonitors that present the
   published scraper client bundle — is a follow-up that overlaps Q35
   (observability) / Q51 (metrics reconciliation). The certs ship with the right
   SANs so that wiring is drop-in. → file as a new Queue item.
2. **Cert rotation** — the bundle is regenerated on expiry, but pods read certs
   at startup and won't hot-reload (same limitation as the existing proxy cert;
   operators restart pods). Controller-runtime's metrics server *does* use a
   certwatcher, so the AGC side may hot-reload; the proxy reads once. Acceptable
   for v1, consistent with the proxy-CONNECT-cert behavior.
