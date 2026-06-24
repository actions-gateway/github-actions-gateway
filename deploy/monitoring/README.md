# Monitoring artifacts

Directly-appliable reference observability artifacts for github-actions-gateway,
so operators can adopt alerts and a dashboard as code instead of rebuilding them
from the prose in [`docs/operations/observability.md`](../../docs/operations/observability.md).

| File | Kind | What it ships |
| --- | --- | --- |
| [`prometheusrule.yaml`](prometheusrule.yaml) | `monitoring.coreos.com/v1` PrometheusRule | The recommended alerting rules and SLO recording rules. |
| [`grafana-dashboard.json`](grafana-dashboard.json) | Grafana dashboard | Gateway health, pod-creation-latency SLO, job throughput, proxy/quota, and GMC overview panels. |

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

## Grafana dashboard

Import `grafana-dashboard.json` via **Dashboards → New → Import** (or provision it
through a dashboard ConfigMap / the Grafana provisioning API). On import, pick the
Prometheus data source that scrapes the gateway. The dashboard exposes `namespace`
and `runner_group` template variables for multi-tenant filtering.

Some Proxy/Quota panels query `kube_deployment_status_replicas_ready`,
`kube_horizontalpodautoscaler_*`, and `kube_resourcequota`, which are emitted by
[kube-state-metrics](https://github.com/kubernetes/kube-state-metrics); those
panels stay empty if it is not installed.
