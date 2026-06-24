# Dashboard preview harness

A throwaway, fully reproducible stack for **previewing and screenshotting** the
monitoring artifacts in the parent directory — the
[`grafana-dashboard-tenant.json`](../grafana-dashboard-tenant.json) and
[`grafana-dashboard-platform.json`](../grafana-dashboard-platform.json) dashboards
and [`prometheusrule.yaml`](../prometheusrule.yaml) — against a real Prometheus
Operator + Grafana. Re-run it whenever a dashboard or the rules change to get
fresh screenshots that reflect the current artifacts.

This is a **development/verification tool only.** It applies nothing to a real
cluster and is not part of the chart or any install path.

## What it does

[`render.sh`](render.sh) drives the whole flow:

1. Creates a local [kind](https://kind.sigs.k8s.io/) cluster (or reuses one).
2. Installs the public [`kube-prometheus-stack`](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack)
   Helm chart with [`values.yaml`](values.yaml) — Prometheus Operator, Prometheus,
   Grafana (with the image-renderer), and kube-state-metrics.
3. Applies the **real** artifacts: the `PrometheusRule` from `../prometheusrule.yaml`
   and both dashboards (`../grafana-dashboard-tenant.json`,
   `../grafana-dashboard-platform.json`), imported via the Grafana dashboard
   sidecar.
4. Deploys [`workload.yaml`](workload.yaml): a synthetic `actions_gateway_*`
   metrics exporter ([`exporter.py`](exporter.py), stdlib-only — counters and
   histograms grow over time so `rate()` and `histogram_quantile()` behave like a
   live system) plus a dummy `actions-gateway-proxy` Deployment/HPA/ResourceQuota
   so the kube-state-metrics Proxy & Quota panels populate.
5. Renders each dashboard to a PNG via Grafana's `/render` endpoint.

## Usage

```sh
cd deploy/monitoring/preview

./render.sh          # create cluster + stack, apply artifacts, render the PNGs
./render.sh shot     # re-apply artifacts + re-render only (fast iteration)
./render.sh down     # delete the throwaway cluster
```

Writes one PNG per dashboard into `OUT_DIR` (default `.`):
`actions-gateway-tenant.png` and `actions-gateway-platform.png`.

Prerequisites: `docker`, `kind`, `helm`, `kubectl`, `curl` on `PATH`. (On macOS
the script adds Docker Desktop's bundled `kubectl` automatically if it isn't
already on `PATH`.)

Common knobs (environment variables):

| Var | Default | Meaning |
| --- | --- | --- |
| `WAIT` | `180` | Seconds to let metrics accumulate before rendering (rate/histogram windows). |
| `OUT_DIR` | `.` | Directory the PNGs are written to. |
| `WIDTH` / `HEIGHT` | `1500` / `2300` | Render dimensions. |
| `FROM` / `TO` | `now-20m` / `now` | Dashboard time range. |
| `CLUSTER` | `gag-obs` | kind cluster name. |

## Iterating

- Changed the **dashboard JSON or rules**? Run `./render.sh shot` — it re-applies
  the artifacts and re-renders without rebuilding the cluster.
- Changed the **synthetic metrics** (`exporter.py`)? Same: `./render.sh shot`
  rolls the exporter and re-renders.

The synthetic metric names and labels are kept in lockstep with the real
registrations (see the [Full Metrics Reference](../../../docs/operations/observability.md#full-metrics-reference));
if a metric's name or labels change in the controllers, update `exporter.py` to
match so the preview stays faithful.
