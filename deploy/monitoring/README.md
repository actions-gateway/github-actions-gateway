# Monitoring artifacts

Directly-appliable reference observability artifacts for github-actions-gateway,
so operators can adopt alerts and a dashboard as code instead of rebuilding them
from the prose in [`docs/operations/observability.md`](../../docs/operations/observability.md).

| File | Kind | What it ships |
| --- | --- | --- |
| [`prometheusrule.yaml`](prometheusrule.yaml) | `monitoring.coreos.com/v1` PrometheusRule | The recommended alerting rules and SLO recording rules. |
| [`grafana-dashboard-tenant.json`](grafana-dashboard-tenant.json) | Grafana dashboard | **Per-tenant** view (from a tenant's AGC + egress-proxy mTLS scrape): gateway health, pod-creation-latency SLO, job throughput, tenant health conditions, egress proxy, and kube-state-metrics proxy/quota panels. |
| [`grafana-dashboard-platform.json`](grafana-dashboard-platform.json) | Grafana dashboard | **Platform/fleet** view (from the GMC manager scrape): managed gateways, GMC reconcile health, and the cross-tenant condition rollups (RunnerGroupsDegraded, EgressRulesStale, proxy quota). |

All PromQL references metrics the controllers actually emit — see the [Full
Metrics Reference](../../docs/operations/observability.md#full-metrics-reference).
The recording rules in `prometheusrule.yaml` back several dashboard panels
(`actions_gateway:pod_creation_latency_seconds:p95` / `:p99`,
`actions_gateway:job_duration_seconds:p50` / `:p95`), so apply both together.

## PrometheusRule

Requires the Prometheus Operator (the `monitoring.coreos.com` CRDs). Apply into a
namespace your Prometheus selects rules from, and add whatever label your
Prometheus uses for rule discovery (e.g. `release: kube-prometheus-stack` —
uncomment it in the manifest):

```sh
kubectl apply -n monitoring -f deploy/monitoring/prometheusrule.yaml
```

Each alert maps to an SLO target in
[Appendix A — Capacity Targets & SLOs](../../docs/design/appendix-a-capacity-slos.md);
adjust the thresholds to match your environment.

## Grafana dashboards

Two dashboards, split along the scrape boundary they read from:

- **`grafana-dashboard-tenant.json`** — one tenant's detail, from that tenant's
  AGC + egress-proxy metrics (per-tenant mTLS scrape). Exposes `namespace` and
  `runner_group` template variables for filtering.
- **`grafana-dashboard-platform.json`** — the fleet view, from the GMC manager
  metrics (one cluster-wide TLS scrape). Its Cross-tenant Throughput row also
  draws on the per-tenant scrapes and is empty without them.

Import either via **Dashboards → New → Import** (or provision it through a
dashboard ConfigMap / the Grafana provisioning API). On import, pick the
Prometheus data source that scrapes the gateway.

Some per-tenant Proxy/Quota panels query `kube_deployment_status_replicas_ready`,
`kube_horizontalpodautoscaler_*`, and `kube_resourcequota`, which are emitted by
[kube-state-metrics](https://github.com/kubernetes/kube-state-metrics); those
panels stay empty if it is not installed.

## Previewing / screenshotting

To preview or screenshot these artifacts against a real Prometheus Operator +
Grafana without a production cluster, use the reproducible harness in
[`preview/`](preview/README.md): it stands up a throwaway kind cluster with the
public `kube-prometheus-stack` chart, applies the artifacts above plus a
synthetic `actions_gateway_*` metrics stream, and renders both dashboards to
PNGs. Re-run it whenever a dashboard or the rules change.
