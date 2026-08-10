# Observability

> **Audience:** Platform engineer, Tenant operator

Every component exposes Prometheus metrics from the standard `controller-runtime` metrics server, so built-in metrics (reconcile latency, work queue depth, etc.) are emitted automatically alongside the custom metrics documented in this guide.
The serving posture differs by component:

- **GMC manager** — `:8443/metrics`, served over TLS.
  How a scrape verifies the cert is controlled by `metrics.tls.certManager.enabled` (see [Verifying the metrics scrape TLS (GMC manager)](observability-metrics-access.md#verifying-the-metrics-scrape-tls-gmc-manager)).
- **Per-tenant AGC and proxy** — `:8443/metrics`, served over **mutual TLS**: a scraper must present a client certificate signed by that tenant's metrics CA.
  The GMC publishes the scraper's client bundle per tenant.
  See [Scraping per-tenant AGC and proxy metrics (mTLS)](observability-metrics-access.md#scraping-per-tenant-agc-and-proxy-metrics-mtls).

For SLO targets associated with these metrics, see [Appendix A — Capacity Targets & SLOs](../design/appendix-a-capacity-slos.md).

---

## This guide

Observability is split by surface so you can read just the part you need without loading the whole thing:

| Page | What's here |
| --- | --- |
| [Metrics reference](observability-metrics.md) | Every metric the GMC, AGC, and proxy emit — the full catalogue, the scale-set acquisition tier, proxy metrics, the `kubectl` status columns and owner-label selectors, the label-cardinality guardrail, and the Q205 metric/span renames. |
| [Accessing metrics (scraping setup)](observability-metrics-access.md) | How to scrape: ad-hoc port-forward, the Prometheus Operator wiring, the install-time NetworkPolicy prerequisites, TLS verification for the GMC manager, and per-tenant AGC/proxy mTLS. |
| [Alerting & SLOs](observability-alerting.md) | The symptom → metric map, the ready-to-apply `PrometheusRule` alert rules, and the SLO recording rules. |
| [Grafana dashboards](observability-dashboards.md) | The bundled tenant and platform dashboards, panel by panel, and their template variables. |
| [Logging & tracing](observability-logging.md) | Structured JSON logging, per-tenant log levels, debug diagnostics for otherwise-silent paths, and OpenTelemetry tracing (AGC). |

The alert rules and both dashboards also ship as code under [`deploy/monitoring/`](../../deploy/monitoring/README.md) — a `PrometheusRule` plus two Grafana dashboards — so prefer applying those over hand-copying the YAML.
