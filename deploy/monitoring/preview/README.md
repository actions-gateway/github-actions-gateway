# Dashboard preview harness

A throwaway, fully reproducible stack for **previewing and screenshotting** the monitoring artifacts in the parent directory — every `../grafana-dashboard-*.json` dashboard and [`prometheusrule.yaml`](../prometheusrule.yaml) — against a real Prometheus Operator + Grafana.
Re-run it whenever a dashboard or the rules change to get fresh screenshots that reflect the current artifacts.

This is a **development/verification tool only.** It applies nothing to a real cluster and is not part of the chart or any install path.

## What it does

[`render.sh`](render.sh) drives the whole flow:

1. Creates a local [kind](https://kind.sigs.k8s.io/) cluster (or reuses one).
2. Installs the public [`kube-prometheus-stack`](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack) Helm chart with [`values.yaml`](values.yaml) — Prometheus Operator, Prometheus, Grafana (with the image-renderer), and kube-state-metrics.
3. Applies the **real** artifacts: the `PrometheusRule` from `../prometheusrule.yaml` and every `../grafana-dashboard-*.json`, imported via the Grafana dashboard sidecar.
4. Deploys [`workload.yaml`](workload.yaml): a synthetic `actions_gateway_*` metrics exporter ([`exporter.py`](exporter.py), stdlib-only — counters and histograms grow over time so `rate()` and `histogram_quantile()` behave like a live system) plus a dummy `actions-gateway-proxy` Deployment/HPA/ResourceQuota so the kube-state-metrics Proxy & Quota panels populate.
5. Renders each dashboard to a PNG via Grafana's `/render` endpoint.

## Usage

```sh
cd deploy/monitoring/preview

./render.sh          # create cluster + stack, apply artifacts, render the PNGs
./render.sh shot     # re-apply artifacts + re-render only (fast iteration)
./render.sh down     # delete the throwaway cluster
```

Writes one PNG per dashboard into `OUT_DIR` (default `.`): `actions-gateway-tenant.png`, `actions-gateway-platform.png`, `actions-gateway-budget.png`, and `actions-gateway-security.png`.

The apply step globs `../grafana-dashboard-*.json`, but the render step walks the `DASH_UIDS` array in [`render.sh`](render.sh).
A new dashboard therefore needs its uid added there, or it is imported and never shot, and the screenshot gate then fails on a PNG the harness was never asked to produce.

### Promoting a render into the docs

The rendered PNGs are **gitignored here** — the copies the docs embed live in `docs/assets/`, and nothing copies them for you.
A dashboard change that skips this step leaves the published screenshot showing the old panels, which is how it drifts:

```sh
cp actions-gateway-tenant.png ../../../docs/assets/grafana-dashboard-tenant.png
cp actions-gateway-platform.png ../../../docs/assets/grafana-dashboard-platform.png
cp actions-gateway-budget.png ../../../docs/assets/grafana-dashboard-budget.png
cp actions-gateway-security.png ../../../docs/assets/grafana-dashboard-security.png
```

Copy only the dashboards you actually changed.
The synthetic workload differs run to run, so re-committing an unchanged dashboard's PNG is pure binary churn.

`make dashboard-render-check` enforces this step, in `make check` and on every PR (Q868).
It reads the branch's whole diff against the merge base with `origin/main`, so landing the JSON in one commit and the PNG in another is fine.
A panel `description` is exempt: it renders as an info-icon tooltip that no screenshot carries, so rewording one asks for no render.
Anything else that changed in the JSON does.

Skipping the render is how the published screenshot drifts, and the drift is silent: the page keeps showing a plausible dashboard, and only somebody holding the JSON open can tell it is a release behind.
That is what [#1526](https://github.com/actions-gateway/github-actions-gateway/pull/1526) shipped, adding a series to the platform dashboard's fleet-conditions panel while the screenshot kept the old five.

For a JSON change that provably cannot alter the render, name the file to skip:

```sh
DASHBOARD_ALLOW_STALE_RENDER=grafana-dashboard-platform.json make dashboard-render-check
```

It reports what it excused rather than passing in silence.

Prerequisites: `docker`, `kind`, `helm`, `kubectl`, `curl` on `PATH`.
(On macOS the script adds Docker Desktop's bundled `kubectl` automatically if it isn't already on `PATH`.)
`magick` (ImageMagick) is optional — when present, each PNG is auto-cropped to remove the dead space Grafana leaves below the last panel row; without it the render keeps the full `HEIGHT`.

Common knobs (environment variables):

| Var | Default | Meaning |
| --- | --- | --- |
| `WAIT` | `660` | Seconds to let metrics accumulate before rendering (rate/histogram windows). Keep it `>=` the `FROM` window, or the time-series panels render mostly empty with a spike at the right edge. |
| `OUT_DIR` | `.` | Directory the PNGs are written to. |
| `WIDTH` / `HEIGHT` | `1500` / `2300` | Render dimensions. |
| `FROM` / `TO` | `now-10m` / `now` | Dashboard time range. Matched to `WAIT` so the whole window is backed by data. |
| `CLUSTER` | `gag-obs` | kind cluster name. Runs sharing it are serialized against each other; set your own to opt out. See [Running two of these at once](#running-two-of-these-at-once). |

### Running two of these at once

`render.sh` holds an exclusive lock on `$CLUSTER` for the whole of `up`, `shot`, and `down`, so a second session waits rather than entering a cluster someone is already rendering.
Expect it to sit there.
A run holds the lock for `WAIT` plus the install, so a queued run reports every 30 s rather than looking hung:

```
==> waiting for the gag-obs preview cluster (another session is rendering, queued 30s)...
```

The lock is there because an unserialized overlap corrupts the screenshot silently (Q1072).
Both runs apply `../grafana-dashboard-*.json` from **their own** worktree into the one Grafana, and the whole `WAIT` window sits between that apply and the render, so the damage lands on whichever session applied **first**.
It renders the other branch's JSON and gets a PNG that looks entirely plausible, while the session that applied last finishes normally and has no way to tell it just overwrote a peer's run.
That is the same drift `make dashboard-render-check` exists to catch, arriving through the harness rather than around it.
The Helm collision an overlap also produces (`another operation (install/upgrade/rollback) is in progress`) is the loud half, and the cheap one: it costs a run, not a wrong screenshot.

Don't wait if you don't want to.
The lock is keyed on the cluster name, so a cluster of your own never queues:

```sh
CLUSTER=gag-obs-$USER ./render.sh
```

That buys wall-clock at the cost of a second full `kube-prometheus-stack` install, which is the trade this box usually loses.
The lock file lives outside the repo (`~/Library/Caches/github-actions-gateway/` on macOS, `$XDG_CACHE_HOME` on Linux) because the sessions contending for the cluster are in different worktrees.
It is an advisory `flock`, so killing a render releases it: there is no stale lock to clear.

The case to know about is a **wedged** run, because `down` is what you reach for and `down` queues behind the wedge like anything else.
Kill the wedged render first, which releases the lock, and then `down` proceeds.
Kill it with Ctrl-C, or by signalling the process group; killing the launcher process alone leaves the render running without its lock.

### Reading a render

Open the PNG.
The harness cannot tell you whether it is right: `render.sh` checks Grafana's HTTP 200 and prints a byte count, and neither separates a populated dashboard from one where every panel reads "No data" (Q1074).

Two traps cost a render each, both found adding a series to the security dashboard's webhook panel:

- **A solo render cannot answer whether a legend is clipped.** `/render/d-solo/<uid>/<uid>?panelId=N&width=…&height=…` grows the returned image to fit the legend whatever height you ask for, so a panel whose legend is cut off in the dashboard shows every entry when rendered alone, at any size.
  Only the full-dashboard render is faithful to the grid cell.
  Use the solo render to read a panel closely, never to judge whether it fits.
- **Legend capacity is not a row count you can derive, and panel height does not buy you rows.** Three renders of the same `w=8, h=7` cell: six labels at full path length clipped three; six short labels (`actionsgateway requests`) fitted in three rows with nothing cut; seven medium labels (`clusterrunnertemplate-v2alpha1 denied`) showed only four.
  Raising that panel to `h=10` still clipped, and shifted the ten panels below it.
  The middle case is the committed artifact, so six entries demonstrably fit: capacity moves with label width in a way these three points do not pin down.
  What they do support is the response: once a legend needs more than about two rows, reduce the number of series rather than the length of their names, and render to confirm.

`label_replace(…, "kind", "$1", …)` is the obvious way to shorten them and **does not work here**: `make promql-check` rejects a `$1` in a dashboard expression, because Grafana's own `$var` interpolation reaches it. The working shape is a `renameByRegex` transformation on the panel, where the `$1` belongs to Grafana's rename machinery rather than to the query.

**A shortening regex has to be injective, and one that parses the name's shape usually is not.** Deriving a label from a webhook path with `.*-([a-z]+)` reads the resource off the end and drops the API version, so `…-github-com-v1alpha1-actionsgateway` and `…-com-v2alpha1-actionsgateway` both render `actionsgateway` and two bands become indistinguishable: exactly the defect the panel was being fixed for.
Prefer stripping a constant prefix and suffix (`v(.*)\.kb\.io`), which cannot collide whatever names are added later, and check the rename against the full set the chart ships rather than the subset the exporter fakes.

## Iterating

- Changed the **dashboard JSON or rules**?
  Run `./render.sh shot` — it re-applies the artifacts and re-renders without rebuilding the cluster.
- Changed the **synthetic metrics** (`exporter.py`)?
  Same: `./render.sh shot` rolls the exporter and re-renders.

The synthetic metric names and labels are kept in lockstep with the real registrations (see the [Full Metrics Reference](../../../docs/operations/observability-metrics.md#full-metrics-reference)); if a metric's name or labels change in the controllers, update `exporter.py` to match so the preview stays faithful.

### Adding a counter to `exporter.py`

Emit it through `counter_total()`, not a hand-rolled `int(rate * elapsed)`.
A counter only ever lives for `WAIT` seconds and is only ever shown across the `FROM` window, so a rate below `1/WAIT` never reaches its first integer: the series renders as a flat line that looks exactly like a real metric sitting at zero, and a `barchart` of `increase()` over it renders empty.
`counter_total()` refuses a rate below `MIN_COUNTER_RATE` (a handful of events per render window) and `render.sh` sees the exporter crash on startup rather than producing a screenshot that looks populated.

A counter that is *meant* to read zero, such as a healthy error counter, stays a literal `0` and skips `counter_total()`.
That zero is a deliberate statement about a healthy system; the floor exists to stop an accidental one from impersonating it.
