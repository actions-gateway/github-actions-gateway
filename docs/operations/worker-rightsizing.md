# Right-sizing worker resources from measured usage

> **Audience:** SRE, Platform engineer

Every worker pod's CPU/memory `requests`/`limits` start as a guess in the
tenant's `RunnerTemplate`. This guide turns the gateway's built-in usage
metrics into measured values, so the guess is revisited with data instead of
`kubectl top` scraping. Actions Runner Controller (ARC) has no equivalent
feedback loop.

**Scope:** v2 `RunnerSet` worker pods. The Actions Gateway Controller (AGC)
samples usage per RunnerSet × container; v1 `RunnerGroup` workers are not
sampled (v1 is [deprecated](v1alpha1-deprecation.md)).

## How the sampling works

The AGC polls the `metrics.k8s.io` API (metrics-server) every 15 seconds
(`WORKER_USAGE_SAMPLE_INTERVAL`; `0`/`off` disables) and keeps the running
CPU/memory peak per worker pod × container. When the pod finishes, its peaks
are folded into per-`RunnerSet` Prometheus series — one worker pod runs exactly
one job, so a per-pod peak is a per-job peak. See the
[metrics reference](observability-metrics.md#worker-usage--right-sizing-metrics-q359)
for the full series list.

Two caveats to hold in mind when reading the numbers:

- **Short jobs are under-sampled.** metrics-server resolves at ~15s; a job
  shorter than one interval finishes with no sample and is counted in
  `actions_gateway_worker_usage_jobs_unsampled_total` instead. Check the
  unsampled:sampled ratio before trusting the distribution — short jobs are
  rarely the sizing constraint, but a mostly-unsampled RunnerSet has no signal.
- **Peaks are point-in-time reads.** A sub-second CPU burst between reads is
  smoothed by metrics-server's window. Treat CPU figures as sustained-peak, not
  instantaneous-max; memory (a level, not a rate) reads accurately.

## Prerequisites

- **metrics-server** (the `metrics.k8s.io` aggregated API) — present by default
  on GKE/EKS/AKS; on kind/bare clusters install it explicitly. Without it the
  AGC runs normally but emits
  `actions_gateway_worker_usage_poll_errors_total` instead of usage data (see
  [Troubleshooting](#troubleshooting)).
- The AGC tenant role ships the required `pods.metrics.k8s.io` read grant since
  this feature landed; no manual RBAC step.
- A Prometheus scraping the AGC (see
  [metrics access](observability-metrics-access.md)).

## Step 1 — read the distribution

Let jobs run until `actions_gateway_worker_usage_jobs_sampled_total` for the
RunnerSet is meaningful (a few dozen jobs of the workload mix you are sizing
for). Then, per container (the runner container is named `runner`):

```promql
# p95 of per-job CPU peaks (cores), by RunnerSet and container, over 7 days
histogram_quantile(0.95, sum by (le, runner_set, container) (
  rate(actions_gateway_worker_usage_job_cpu_peak_cores_bucket[7d])))

# p95 of per-job memory peaks (bytes)
histogram_quantile(0.95, sum by (le, runner_set, container) (
  rate(actions_gateway_worker_usage_job_memory_peak_bytes_bucket[7d])))

# absolute max peak seen since the AGC last restarted
actions_gateway_worker_usage_cpu_peak_cores
actions_gateway_worker_usage_memory_peak_bytes

# how much of the job population the histograms actually saw
sum by (runner_set) (rate(actions_gateway_worker_usage_jobs_sampled_total[7d]))
  /
sum by (runner_set) (rate(actions_gateway_worker_usage_jobs_unsampled_total[7d]))
```

The histogram quantiles are bucket-interpolated — read them as the bucket
range, not an exact figure, and cross-check against the max-peak gauges.

## Step 2 — derive `requests`/`limits`

Apply the resource-model rules (proven in the
[dogfood right-sizing exercise](../plan/dogfood-runner-rightsizing.md#resource-model-principles)
that this feature automates the measurement half of):

| Field | Set to | Why |
|---|---|---|
| memory `request` | ≈ p95 of per-job memory peaks | Memory is non-compressible; the request is what the scheduler packs by. |
| memory `limit` | ≈ max peak × 1.3–1.4 | OOM headroom. Widen the factor if any job OOMs; exceeding it kills the job. |
| CPU `request` | ≈ p90–p95 of per-job CPU peaks | Drives packing; jobs above it borrow idle node CPU. |
| CPU `limit` | **omit** | CPU is compressible — a limit only throttles bursty build/test steps for no packing benefit. Keep the memory limit for noisy-neighbor safety. |

Update the tenant's `RunnerTemplate.spec.podTemplate` container resources with
the derived values, then validate: watch for `OOMKilled` events in the tenant
namespace over the next few days, and confirm the derived requests still fit
the node shape and namespace `ResourceQuota`
(see [capacity planning](../design/appendix-e-capacity-planning.md)).

**Sizing the trade deliberately:** packing tighter (requests ≈ p95) maximizes
workers per node — the right call on expensive nodes (GPUs, large VMs) where
worker count per node is the cost driver. Sizing up (requests ≈ max) buys
burst headroom so jobs finish faster. The
[sizing-profiles plan](../plan/runner-sizing-profiles.md) tracks automating
this choice; today it is a per-`RunnerTemplate` decision.

## Troubleshooting

**`actions_gateway_worker_usage_poll_errors_total` rising steadily** — the AGC
cannot list `PodMetrics`. Either metrics-server is not installed (the
[install pre-flight](install.md#preflight-the-cluster-required-first-step) warns about this) or the
AGC's RoleBinding predates the `pods.metrics.k8s.io` grant (re-render from the
current chart: the `agc-tenant-role` ClusterRole must contain a
`metrics.k8s.io` rule). The AGC log line `list PodMetrics (is metrics-server
installed?)` carries the underlying error, throttled to roughly one line per
ten minutes.

**All jobs land in `…_jobs_unsampled_total`** — the workload's jobs finish
faster than the sampling interval. There is no per-job signal to size from at
15s resolution; size such a RunnerSet by its node-shape share instead (see the
`NodeShare` idea in the [sizing-profiles plan](../plan/runner-sizing-profiles.md)).

**Gauges reset after an AGC rollout** — the `…_usage_cpu_peak_cores` /
`…_usage_memory_peak_bytes` gauges are peaks *since AGC start* by design.
Use `max_over_time(...[30d])` in Prometheus to bridge restarts; the histograms
and counters are cumulative series that Prometheus rate/window queries already
handle across restarts.
