# Dashboard preview harness

A throwaway, fully reproducible stack for **previewing and screenshotting** the monitoring artifacts in the parent directory — the [`grafana-dashboard-tenant.json`](../grafana-dashboard-tenant.json) and [`grafana-dashboard-platform.json`](../grafana-dashboard-platform.json) dashboards and [`prometheusrule.yaml`](../prometheusrule.yaml) — against a real Prometheus Operator + Grafana.
Re-run it whenever a dashboard or the rules change to get fresh screenshots that reflect the current artifacts.

This is a **development/verification tool only.** It applies nothing to a real cluster and is not part of the chart or any install path.

## What it does

[`render.sh`](render.sh) drives the whole flow:

1. Creates a local [kind](https://kind.sigs.k8s.io/) cluster (or reuses one).
2. Installs the public [`kube-prometheus-stack`](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack) Helm chart with [`values.yaml`](values.yaml) — Prometheus Operator, Prometheus, Grafana (with the image-renderer), and kube-state-metrics.
3. Applies the **real** artifacts: the `PrometheusRule` from `../prometheusrule.yaml` and both dashboards (`../grafana-dashboard-tenant.json`, `../grafana-dashboard-platform.json`), imported via the Grafana dashboard sidecar.
4. Deploys [`workload.yaml`](workload.yaml): a synthetic `actions_gateway_*` metrics exporter ([`exporter.py`](exporter.py), stdlib-only — counters and histograms grow over time so `rate()` and `histogram_quantile()` behave like a live system) plus a dummy `actions-gateway-proxy` Deployment/HPA/ResourceQuota so the kube-state-metrics Proxy & Quota panels populate.
5. Renders each dashboard to a PNG via Grafana's `/render` endpoint.

## Usage

```sh
cd deploy/monitoring/preview

./render.sh          # create cluster + stack, apply artifacts, render the PNGs
./render.sh shot     # re-apply artifacts + re-render only (fast iteration)
./render.sh down     # delete the throwaway cluster
```

Writes one PNG per dashboard into `OUT_DIR` (default `.`): `actions-gateway-tenant.png` and `actions-gateway-platform.png`.

### Promoting a render into the docs

The rendered PNGs are **gitignored here** — the copies the docs embed live in `docs/assets/`, and nothing copies them for you.
A dashboard change that skips this step leaves the published screenshot showing the old panels, which is how it drifts:

```sh
cp actions-gateway-tenant.png ../../../docs/assets/grafana-dashboard-tenant.png
cp actions-gateway-platform.png ../../../docs/assets/grafana-dashboard-platform.png
```

Copy only the dashboards you actually changed.
The synthetic workload differs run to run, so re-committing an unchanged dashboard's PNG is pure binary churn.

Prerequisites: `docker`, `kind`, `helm`, `kubectl`, `curl` on `PATH`.
(On macOS the script adds Docker Desktop's bundled `kubectl` automatically if it isn't already on `PATH`.) `magick` (ImageMagick) is optional — when present, each PNG is auto-cropped to remove the dead space Grafana leaves below the last panel row; without it the render keeps the full `HEIGHT`.

Common knobs (environment variables):

| Var | Default | Meaning |
| --- | --- | --- |
| `WAIT` | `660` | Seconds to let metrics accumulate before rendering (rate/histogram windows). Keep it `>=` the `FROM` window, or the time-series panels render mostly empty with a spike at the right edge. |
| `OUT_DIR` | `.` | Directory the PNGs are written to. |
| `WIDTH` / `HEIGHT` | `1500` / `2300` | Render dimensions. |
| `FROM` / `TO` | `now-10m` / `now` | Dashboard time range. Matched to `WAIT` so the whole window is backed by data. |
| `CLUSTER` | `gag-obs` | kind cluster name. |

## Iterating

- Changed the **dashboard JSON or rules**?
  Run `./render.sh shot` — it re-applies the artifacts and re-renders without rebuilding the cluster.
- Changed the **synthetic metrics** (`exporter.py`)?
  Same: `./render.sh shot` rolls the exporter and re-renders.

The synthetic metric names and labels are kept in lockstep with the real registrations (see the [Full Metrics Reference](../../../docs/operations/observability-metrics.md#full-metrics-reference)); if a metric's name or labels change in the controllers, update `exporter.py` to match so the preview stays faithful.

### Adding a counter to `exporter.py`

Emit it through `counter_total()`, not a hand-rolled `int(rate * elapsed)`.
A counter only ever lives for `WAIT` seconds and is only ever shown across the `FROM` window, so a rate below `1/WAIT` never reaches its first integer: the series renders as a flat line that looks exactly like a real metric sitting at zero, and a `barchart` of `increase()` over it renders empty. `counter_total()` refuses a rate below `MIN_COUNTER_RATE` (a handful of events per render window) and `render.sh` sees the exporter crash on startup rather than producing a screenshot that looks populated.

A counter that is *meant* to read zero, such as a healthy error counter, stays a literal `0` and skips `counter_total()`.
That zero is a deliberate statement about a healthy system; the floor exists to stop an accidental one from impersonating it.
