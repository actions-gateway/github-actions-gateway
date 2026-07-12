# Q72 — Per-tenant metrics scrape wiring (Services + ServiceMonitors)

## Goal

Make Prometheus able to actually scrape the per-tenant mTLS `/metrics`
endpoints that Q69 shipped on `:8443` for the proxy and the AGC.

## Background (verified by source inspection)

- Q69 added an mTLS metrics listener on `metricsPort` (`:8443`) to both the
  proxy and AGC pods, served with a per-tenant server cert and requiring a
  client cert signed by the per-tenant metrics CA (`cmd/gmc/internal/controller/metrics_cert.go`).
- The GMC already publishes a per-tenant scraper **client** bundle Secret
  `actions-gateway-metrics-client` (`ca.crt` + `tls.crt` + `tls.key`) in each
  tenant namespace (`buildMetricsClientSecret`), and a NetworkPolicy ingress
  rule (`metricsScrapeIngressRule`) admits scrapes of `:8443` from any
  `metrics=enabled` namespace.
- BUT nothing scrapes it: `buildProxyService` exposes only the `proxy` port
  (`:8080`); there is **no AGC Service** at all; and no per-tenant
  ServiceMonitor exists. Per-tenant resources are built in Go by the GMC
  reconciler — **not** the Helm chart (the chart only installs the GMC and its
  own manager ServiceMonitor, added by Q104).
- The metrics **server** cert SANs already cover both the proxy and AGC
  Service DNS names (`metricsServerSANs`), so verified scraping is drop-in once
  the Services + ServiceMonitors exist.

## Why per-tenant (not one cross-namespace ServiceMonitor)

Each tenant has its **own** metrics CA / client cert. A single ServiceMonitor
presents exactly one client cert + one CA, so it cannot mutually authenticate
to more than one tenant. Per-tenant ServiceMonitors in each tenant namespace,
each referencing that tenant's `actions-gateway-metrics-client` Secret, are
required. Tenants are created dynamically (one ActionsGateway CR each), so the
GMC reconciler must create them — the chart cannot template unknown namespaces.

## Approach

1. **Services (always created, no new dependency):**
   - Add the `metrics` port (`:8443`) to the proxy Service.
   - Add an AGC metrics Service named `actions-gateway-controller` (matches the
     server-cert SAN) exposing `:8443`.
   - Stamp an `app` label on each metrics Service's metadata so a ServiceMonitor
     can select precisely.
2. **ServiceMonitors (opt-in, built as `unstructured` to avoid adding the
   prometheus-operator API as a Go dependency):**
   - One per component (proxy, AGC), in the tenant namespace, selecting that
     component's Service by managed + `app` labels (tenant-scoped via
     owner-name/owner-ns labels), scraping port `metrics` over HTTPS, presenting
     the per-tenant client bundle (`cert`/`keySecret`/`ca` from
     `actions-gateway-metrics-client`) with `serverName` = `<svc>.<ns>.svc`.
   - Gated behind a new GMC flag `--enable-tenant-service-monitors` (default
     **false**), wired from the chart's existing `metrics.serviceMonitor.enabled`
     value so one operator switch covers both the manager and per-tenant
     monitors. Default-off keeps provisioning working on clusters without the
     Prometheus Operator CRD (secure/safe by default).
   - If the CRD is absent even when opted in, skip gracefully (log + Event) so a
     missing optional scrape resource never breaks tenant provisioning.
   - Owner reference on the ActionsGateway → GC on delete; reconcile also
     deletes them best-effort when the flag is off (flag-flip cleanup).

## Files

- `cmd/gmc/internal/controller/builder.go` — proxy Service metrics port + label,
  `buildAGCService`, `buildMetricsServiceMonitor` (unstructured), DNS-name helper.
- `cmd/gmc/internal/controller/actionsgateway_controller.go` — apply AGC Service;
  apply/delete ServiceMonitors gated on the flag; RBAC marker; teardown delete of
  the AGC Service.
- `cmd/gmc/cmd/main.go` — `--enable-tenant-service-monitors` flag → reconciler.
- `cmd/gmc/config/rbac/role.yaml` + `charts/actions-gateway/files/manager-role-rules.yaml`
  — regenerated (`make manifests` + `make chart-rbac`).
- `charts/actions-gateway/templates/deployment.yaml` + `values.yaml` — pass the
  flag when `metrics.serviceMonitor.enabled`; document the broadened scope.
- Tests: `builder_test.go` (Service ports, AGC Service, ServiceMonitor fields).
- Docs: `docs/operations/observability.md`, `docs/operations/troubleshooting.md`,
  `docs/design/05-security.md`.

## Out of scope (do not absorb)

- Q35 observability/logging-probes work beyond the metrics Service/ServiceMonitor
  wiring.
- Cert hot-reload (Q69 open question 2).
