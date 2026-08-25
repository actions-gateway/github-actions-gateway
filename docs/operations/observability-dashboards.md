# Grafana Dashboards

> **Audience:** Platform engineer, Tenant operator, Budget owner

Part of the [Observability](observability.md) guide.
The panels below query the [Metrics reference](observability-metrics.md) and the [SLO recording rules](observability-alerting.md#slo-recording-rules); the scrape wiring they depend on is in [Accessing metrics](observability-metrics-access.md).

> **Import as code.** Three reference dashboards ship under [`deploy/monitoring/`](../../deploy/monitoring/README.md): import them into Grafana (**Dashboards → New → Import**) or provision them, rather than rebuilding the panels by hand.
> The layouts below document what each contains.

| Dashboard | Source scrape | Audience |
| --- | --- | --- |
| [`grafana-dashboard-tenant.json`](../../deploy/monitoring/grafana-dashboard-tenant.json) | a tenant's AGC + egress proxy (per-tenant mTLS) | operator of one tenant's runners |
| [`grafana-dashboard-platform.json`](../../deploy/monitoring/grafana-dashboard-platform.json) | the GMC manager (one cluster-wide TLS scrape) | Platform engineer running the GMC / the fleet |
| [`grafana-dashboard-budget.json`](../../deploy/monitoring/grafana-dashboard-budget.json) | a tenant's AGC (per-tenant mTLS) | Budget owner paying for the fleet |

The first two split along the scrape boundary each reads from, which mirrors how the metrics are exposed (see [Accessing metrics](observability-metrics-access.md#how-to-access-metrics)): a platform operator scrapes the single GMC endpoint and cannot necessarily reach every tenant's mTLS metrics port, so the fleet rollups the GMC exports (`managed_gateways`, `runnergroups_degraded`, `egress_rules_stale`, the proxy-quota gauges) get their own dashboard.

The **budget** dashboard splits on a different axis: it reads the same tenant scrape as the tenant dashboard, and exists because the question is different rather than the data.
A budget owner is asking what each tenant consumed and what it cost, which is one metric read across every namespace at once, not the health of any one of them.

> The screenshots below are rendered against a real Prometheus with synthetic data by the reproducible harness in [`deploy/monitoring/preview/`](../../deploy/monitoring/preview/README.md); regenerate them there whenever a dashboard changes.

## Tenant dashboard

![The per-tenant Grafana dashboard rendered against a live Prometheus: gateway-health, pod-creation-latency SLO, job-throughput, scale-set-acquisition-tier, tenant-health-conditions, egress-proxy, and kube-state-metrics proxy/quota rows.](../assets/grafana-dashboard-tenant.png)

Filtered by the `$namespace`, `$runner_group`, and `$runner_set` template variables.
Uses the [SLO recording rules](observability-alerting.md#slo-recording-rules) as data sources where applicable.

**Row 1 — Gateway Health (per namespace)**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Active sessions | `actions_gateway_active_sessions` | Stat / Time series |
| Jobs acquired/min | `rate(actions_gateway_jobs_acquired_total[5m]) * 60` | Time series |
| Token refresh errors | `rate(actions_gateway_token_refresh_errors_total[5m])` | Stat (threshold: >0 = red) |
| RenewJob errors | `rate(actions_gateway_renew_job_errors_total[5m])` | Stat (threshold: >0 = yellow) |

**Row 2 — Pod Creation Latency SLO**

| Panel | Query | Visualization |
|-------|-------|---------------|
| p95 latency | `actions_gateway:pod_creation_latency_seconds:p95` | Gauge (green <15s, yellow <60s, red >60s) |
| p99 latency | `actions_gateway:pod_creation_latency_seconds:p99` | Gauge |
| Latency heatmap | `rate(actions_gateway_pod_creation_latency_seconds_bucket[5m])` | Heatmap |

**Row 3 — Job Throughput (per runner_group)**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Jobs acquired total | `increase(actions_gateway_jobs_acquired_total[1h])` | Bar chart by runner_group |
| Job duration p50/p95 | `actions_gateway:job_duration_seconds:p50/p95` | Time series, both acquisition tiers. Worker pod lifetime, so it excludes the pre-creation staging a slow `acquirejob` or a `spec.scaleUp` throttle adds |
| AGC / proxy version | `actions_gateway_build_info{component=~"agc\|proxy"}` | Stat showing the series name, not its value (the value is always `1`). Sits under the job-duration panel because it says which span that panel is measuring: the classic tier's was longer before v1.5.0 and nothing was renamed ([upgrade note](upgrade.md#non-breaking-job_duration_seconds-now-measures-worker-pod-lifetime-the-classic-tier-span-shrinks)). The metric carries no `namespace` label, so `$namespace` does not filter it |
| Disruption retries | `sum by (runner_group, tier, cause) (increase(actions_gateway_eviction_retries_total[1h]))` | Bar chart, split by acquisition tier (`classic`, `scaleset`) and cause (`eviction`, `preemption`, `deletion`, `abandoned`, `vanished`). Keep the causes visually distinct: `eviction` rising means node pressure, `preemption` rising means a `priorityTiers` floor is displacing opportunistic work, `abandoned` rising means workers are not being scheduled at all before `pendingPodDeadline` reaps them, `vanished` rising means the AGC itself keeps missing worker teardowns — different investigations entirely |
| Abandoned runs awaiting capacity | `sum by (runner_group, tier, outcome) (increase(actions_gateway_abandoned_run_rerun_waits_total[1h]))` | Stat or bar chart, split by acquisition tier since Q766 ported the recovery to `scaleset`. `expired` is the one to watch: a job whose run was force-cancelled and whose capacity never came back inside the wait window is a job silently lost until someone re-runs it by hand |
| Disruption budget exhausted | `increase(actions_gateway_eviction_retries_exhausted_total[1h])` | Stat (threshold: >0 = red) |
| Quota retries | `increase(actions_gateway_quota_retries_total[1h])` | Bar chart |
| Quota retry budget exhausted | `increase(actions_gateway_quota_retries_exhausted_total[1h])` | Stat (threshold: >0 = red) |

**Row 4 — Scale-set Acquisition Tier (per runner_set)**

The default acquisition protocol (Q264).
These panels are the scale-set analog of the classic Gateway-Health and Job-Throughput rows above: a ScaleSet-protocol RunnerSet never emits `actions_gateway_active_sessions` or `jobs_acquired_total`, so its throughput and health are only visible here.
Labelled by `runner_set` (not `runner_group`), so the `$runner_group` variable does not filter these — the `$runner_set` variable does.

| Panel | Query | Visualization |
|-------|-------|---------------|
| Jobs assigned vs. provisioned/min | `sum by (namespace, runner_set) (rate(actions_gateway_scaleset_jobs_assigned_total[5m])) * 60` and the `…_provisioned_total` counterpart | Time series (a persistent gap = provisioning lagging) |
| Provision success rate | `actions_gateway:scaleset_provision_success_rate:rate5m` | Gauge (green >0.99, yellow <0.99, red <0.9) |
| Provision errors/s | `sum by (namespace, runner_set) (rate(actions_gateway_scaleset_provision_errors_total[5m]))` | Stat (threshold: >0 = yellow) |
| Jobs completed by result (1h) | `sum by (result) (increase(actions_gateway_scaleset_jobs_completed_total[1h]))` | Bar chart by result |
| Worker pods reaped/s (by reason) | `sum by (namespace, runner_set, reason) (rate(actions_gateway_worker_pods_reaped_total{runner_set!=""}[5m]))` | Time series — the reaper counter's per-`RunnerSet` series (Q514), joinable with the capacity gauges above on `(namespace, runner_set)`. The label keys on the owning kind, so a Classic-protocol `RunnerSet` appears here too |

**Row 5 — Tenant Health Conditions**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Worker quota exceeded | `max(actions_gateway_worker_quota_exceeded or actions_gateway_runnerset_worker_quota_exceeded)` | Stat (1 = red) |
| Workers unschedulable | `max(actions_gateway_workers_unschedulable or actions_gateway_runnerset_workers_unschedulable)` | Stat (1 = red) |
| Worker quota pressure | `max(actions_gateway_worker_quota_pressure or actions_gateway_runnerset_worker_quota_pressure)` | Stat (1 = yellow) |
| Worker capacity declined | `max by (reason) (actions_gateway_runnerset_worker_capacity_declined)` | Stat (1 = orange), reason shown beside the value |
| Agent recycle errors | `rate(actions_gateway_agent_recycle_errors_total[5m])` | Time series |

> The first three capacity panels union the v1 `RunnerGroup` family with its `actions_gateway_runnerset_*` v2 twin (Q319).
> The two families key on different labels — `runner_group` and `runner_set` — so `or` unions rather than overlaps them, and a panel that named only the v1 family would read a flat `0` on a v2-only deploy.
> To break either out per owner, replace `max(...)` with `max by (namespace, runner_set) (actions_gateway_runnerset_...)`.

> **Worker capacity declined has no v1 twin, and `No data` is a normal reading** (Q643, Q658).
> The [gauge](observability-metrics.md) is emitted only for a `RunnerSet` that set `spec.capacityGate.mode`, so an empty panel means no set opted in, not that the query is broken.
> It groups by `reason` rather than reducing to a bare `0`/`1` because the value alone cannot separate a live decline from the latched `AwaitingProbe` state, and those call for different actions; exactly one series exists per gated set, so `max by (reason)` cannot double-count.
> The `1` is orange rather than red on purpose: a latched gate is [throttling intake, not failing](troubleshooting.md#runnerset-reports-workercapacitydeclined-the-gateway-stopped-claiming-jobs), and it can sit `True` indefinitely on an idle set whose shape stays unplaceable.

**Row 6 — Egress Proxy (per tenant)**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Active CONNECT tunnels | `actions_gateway_proxy_connections_active` | Time series |
| CONNECT tunnels opened/s | `rate(actions_gateway_proxy_connections_total[5m])` | Time series |
| Proxy dial errors/s | `rate(actions_gateway_proxy_dial_errors_total[5m])` | Time series |
| Denied CONNECTs/s (SSRF signal) | `rate(actions_gateway_proxy_connect_denied_total[5m])` | Time series |
| Tunnel duration p95 | `histogram_quantile(0.95, rate(actions_gateway_proxy_tunnel_duration_seconds_bucket[5m]))` | Time series |

**Row 7 — Proxy & Quota (kube-state-metrics)**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Proxy replicas ready | `kube_deployment_status_replicas_ready{deployment="actions-gateway-proxy"}` | Time series |
| HPA desired vs. current | `kube_horizontalpodautoscaler_status_*_replicas` | Time series |
| ResourceQuota usage | `kube_resourcequota{type="used"}` filtered by namespace | Bar gauge |

**Row 7 — Reliability Signals (Q315)**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Job acquisition errors/s (by reason) | `sum by (namespace, reason) (rate(actions_gateway_job_acquisition_errors_total[5m]))` | Time series |
| Message poll errors/s (by reason) | `sum by (namespace, reason) (rate(actions_gateway_message_poll_errors_total[5m]))` | Time series |
| RenewJob teardowns/s (by reason) | `sum by (namespace, reason) (rate(actions_gateway_renew_job_teardowns_total[5m]))` | Time series |
| Worker pods reaped/s (by reason) | `sum by (runner_group, reason) (rate(actions_gateway_worker_pods_reaped_total[5m]))` | Time series |
| Broker token propagation retries/s | `sum by (runner_group) (rate(actions_gateway_broker_token_propagation_retries_total[5m]))` | Time series |

**Row 8 — Fan-out Safety (Q260 / Q266)**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Duplicate job deliveries/s | `sum by (runner_group) (rate(actions_gateway_jobs_duplicate_delivery_total[5m]))` | Time series |
| Abandoned-delivery completions/s (by outcome) | `sum by (runner_group, outcome) (rate(actions_gateway_abandoned_delivery_completions_total[5m]))` | Time series |
| Fan-out loser recycle deferred/s (by outcome) | `sum by (runner_group, outcome) (rate(actions_gateway_fanout_loser_recycle_deferred_total[5m]))` | Time series |

## Platform dashboard

![The platform/fleet Grafana dashboard rendered against a live Prometheus: fleet-overview stats, GMC control-plane reconcile health, a per-gateway condition state-timeline, cross-tenant throughput, and a build-versions row.](../assets/grafana-dashboard-platform.png)

Fleet-wide; `$namespace` filters the cross-tenant rows.

**Row 1 — Fleet Overview**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Managed gateways | `actions_gateway_managed_gateways` | Stat |
| Degraded gateways | `sum(actions_gateway_runnergroups_degraded)` (v1) / `sum(actions_gateway_runnersets_degraded)` (v2) | Stat (>0 = red) |
| Egress allowlist stale | `sum(actions_gateway_egress_rules_stale)` | Stat (>0 = red) |
| Proxy quota exceeded | `sum(actions_gateway_proxy_quota_exceeded)` | Stat (>0 = red) |

**Row 2 — GMC Control Plane**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Reconcile errors by controller | `rate(controller_runtime_reconcile_errors_total[5m])` | Time series |
| Reconcile rate by controller | `rate(controller_runtime_reconcile_total[5m])` | Time series |
| IP range refreshes (24h) | `sum(increase(actions_gateway_ip_range_updates_total[24h]))` | Stat |

**Row 3 — Fleet Conditions (per gateway)**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Gateway condition rollups | `actions_gateway_runnergroups_degraded` / `_egress_rules_stale` / `_proxy_quota_pressure` / `_proxy_quota_exceeded` (v1); `_runnersets_degraded` / `_agc_available` / `_egress_unattributed` / `_scale_set_name_collision` (v2); `_github_egress_incomplete` (v2 `EgressProxy` only) | State timeline (1 = firing) |

**Row 4 — Cross-tenant Throughput** (requires the per-tenant AGC scrapes)

| Panel | Query | Visualization |
|-------|-------|---------------|
| Active sessions by namespace | `sum by (namespace) (actions_gateway_active_sessions)` | Time series |
| Jobs acquired/min by namespace (classic) | `sum by (namespace) (rate(actions_gateway_jobs_acquired_total[5m])) * 60` | Time series |
| Jobs assigned/min by namespace (scale-set) | `sum by (namespace) (rate(actions_gateway_scaleset_jobs_assigned_total[5m])) * 60` | Time series |

**Row 5 — Build Versions**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Running versions by component | `count by (component, version) (actions_gateway_build_info)` | Stat, one tile per component and version with the instance count |

The fleet's version spread during a staggered upgrade, and the answer to which tenants have crossed a semantics change: `job_duration_seconds` changed span at v1.5.0 without a rename ([upgrade note](upgrade.md#non-breaking-job_duration_seconds-now-measures-worker-pod-lifetime-the-classic-tier-span-shrinks)).
The GMC comes from this dashboard's own scrape; `agc` and `proxy` need the per-tenant scrapes, so a platform-only Prometheus shows one bar.
`actions_gateway_build_info` carries no `namespace` label, so `$namespace` does not filter this row. | Pod creation p99 by namespace | `actions_gateway:pod_creation_latency_seconds:p99` | Time series, both acquisition tiers |

## Budget dashboard

![The budget-owner Grafana dashboard rendered against a live Prometheus: a spend-summary stat row, spend-by-tenant bars and burn rate, pod-hours and job counts by runner shape, and the zero-idle-compute row contrasting worker consumption with the always-on proxy floor.](../assets/grafana-dashboard-budget.png)

For the [budget owner](personas.md#budget-owner), who owns the spend and usually cannot read the cluster at all.
Filtered by `$namespace` and `$runner_group`, and priced by a `$rate` textbox.

Every panel reads one metric: `actions_gateway_job_duration_seconds`.
That series is worker pod wall time (creation to the last container finishing) on **both** acquisition tiers, which is the span [Appendix F §F.1](../design/appendix-f-cost-model.md#worker-pod-dominant-cost) bills against.
A pod that never started a container is not observed, because it occupied no node time.

**The rate is the operator's, not ours.** Appendix F's formula is `(job_duration_seconds / 3600) × hourly_node_rate × resource_fraction`, and `$rate` is that trailing pair collapsed into one number: the effective hourly cost of **one worker slot**. The shipped default, `0.096`, is §F.1's own CPU example: an `m6i.4xlarge` at $0.768/hr with the pod requesting an eighth of it.
Its GPU example works out at $4.10/hr, so the two differ by more than 40×.

That spread is why the shape-level panels stay in pod-hours: one currency figure spanning a tenant's GPU and CPU shapes is an average of two rates that are nothing like each other.
To read one shape's spend, pin `$runner_group` and set `$rate` to that shape's slot rate.
For per-tenant currency computed from a real node price book rather than a typed constant, use [Cost attribution](cost-attribution.md); this dashboard is the GAG-native cross-check for it, not a replacement.

**Row 1 — Spend Summary (selected time range)**

Every panel here uses `$__range`, so the time picker *is* the billing window: switch it to 30 days and the numbers are a month's.

| Panel | Query | Visualization |
|-------|-------|---------------|
| Estimated spend | `sum(increase(actions_gateway_job_duration_seconds_sum[$__range])) / 3600 * $rate` | Stat. Currency only as far as `$rate` is right for the selection |
| Worker pod-hours | `sum(increase(actions_gateway_job_duration_seconds_sum[$__range])) / 3600` | Stat, the measured quantity with no rate applied. This is the number to reconcile against a cost tool's own pod-hours |
| Jobs completed | `sum(increase(actions_gateway_job_duration_seconds_count[$__range]))` | Stat |
| Mean job duration | pod-hours ÷ jobs | Stat. `No data` when the range holds no completed job, since the divisor is zero |

**Row 2 — Spend by Tenant**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Estimated spend by tenant | `sum by (namespace) (increase(actions_gateway_job_duration_seconds_sum[$__range])) / 3600 * $rate` | Bar chart, **instant**. Namespace is tenant, which is why this lines up with an `aggregate=namespace` allocation report with no extra wiring |
| Spend rate per hour by tenant | `sum by (namespace) (rate(actions_gateway_job_duration_seconds_sum[5m])) * $rate` | Time series. Pod-seconds per second is pod-hours per hour, so this is a live burn rate. The histogram attributes a pod's whole lifetime at completion, so the series is a trailing average and is lumpy over windows near the job duration itself |
| Share of fleet worker pod-hours | tenant pod-hours ÷ `scalar(...)` of the unfiltered total | Bar gauge, `percent`. The denominator is deliberately **unfiltered**, so filtering to one tenant shows its share of everything rather than 100% of itself. Rate-free |

**Row 3 — Spend by Runner Shape**

Split by `runner_group`, which on the scale-set tier carries the **`RunnerSet`** name rather than a `RunnerGroup` one: the pod-side reading takes whichever owner label the pod carries, so one label key covers both tiers (Q713).
A shape here is therefore whatever provisioned the worker, on either protocol.

| Panel | Query | Visualization |
|-------|-------|---------------|
| Worker pod-hours by shape | `sum by (namespace, runner_group) (increase(actions_gateway_job_duration_seconds_sum[$__range])) / 3600` | Bar chart. Hours rather than currency, per the rate spread above |
| Jobs completed by shape | the `…_count` counterpart | Bar chart. Beside the pod-hours bars it separates the two ways a shape's spend grows: more jobs, or slower jobs |
| Mean job duration by shape | `rate(…_sum[5m]) / rate(…_count[5m])`, both `sum by (namespace, runner_group)` | Time series. A shape drifting upward buys less for the same money; a large divergence from a cost tool's pod-hours for that shape usually means oversized resource requests ([right-sizing](../design/appendix-f-cost-model.md#worker-resource-right-sizing)) |

**Row 4 — Zero Idle Compute (the always-on floor)**

The row that answers "what am I paying for when nobody is running CI?".

> **The comparison panels run instant queries**, which is where this dashboard departs from its two neighbours.
> A bar chart or bar gauge here reduces the whole range to one number per tenant or per shape, so a range query would plot one bar per scrape and the comparison the panel exists to make would be unreadable.
> The time-series panels stay range queries, because a trend is what they are for.

| Panel | Query | Visualization |
|-------|-------|---------------|
| Worker consumption vs. the always-on floor | `sum by (namespace) (rate(actions_gateway_job_duration_seconds_sum[5m]))` against `kube_deployment_status_replicas_ready{deployment="actions-gateway-proxy"}` | Time series, two series on one panel deliberately. Over a quiet weekend the worker line collapses toward zero while the proxy line stays flat, and that flat line is the entire idle floor. An Actions Runner Controller (ARC) scale set holding `minRunners > 0` would show a worker line that never reaches zero. Needs kube-state-metrics |
| Jobs completed per hour | `sum by (namespace) (rate(actions_gateway_job_duration_seconds_count[5m])) * 3600` | Time series showing the volume that drives the worker line beside it |
| ResourceQuota headroom | `kube_resourcequota{type="used"}` and its `type="hard"` counterpart | Bar gauge, a used bar beside its cap. A tenant pinned at its quota is one whose spend is held down by the cap rather than by demand, which is a budget conversation rather than an incident. Needs kube-state-metrics |

## Dashboard Variables

The dashboards ship with these template variables already wired:

- `$namespace` (`label_values({__name__=~"actions_gateway_active_sessions|actions_gateway_scaleset_jobs_assigned_total"}, namespace)`) filters to a single tenant, on the tenant and platform dashboards.
  The union of the classic and scale-set series is deliberate: a scale-set-only deploy emits no `active_sessions`, so keying the variable on that alone would leave the dashboard blank.
- `$runner_group` (`label_values(actions_gateway_active_sessions{namespace="$namespace"}, runner_group)`) filters to a specific RunnerGroup on the classic-tier panels of the tenant dashboard.
  The `runner_set`-labelled panels are not filtered by it; `$runner_set` is their variable.
- `$runner_set` (`label_values(actions_gateway_runnerset_worker_quota_pressure{namespace="$namespace"}, runner_set)`) filters to a specific `RunnerSet` on the scale-set and v2 capacity panels of the tenant dashboard.
  It reads its label values from the Q319 capacity gauges rather than the `scaleset_*` series on purpose: those gauges are emitted for **every** `RunnerSet`, while `scaleset_*` exists only for `ScaleSet`-protocol sets, so keying on the latter would hide a `Classic` set from the dropdown entirely.

The budget dashboard declares its own set, because it reads a different metric from every panel and its dropdowns have to match that metric exactly:

- `$namespace` (`label_values(actions_gateway_job_duration_seconds_count, namespace)`) is keyed on the series the panels actually read, so the dropdown cannot offer a tenant with no cost data or hide one that has some.
  It needs no classic/scale-set union: the duration series is emitted from the pod informer and covers both tiers already.
- `$runner_group` (`label_values(actions_gateway_job_duration_seconds_count{namespace=~"$namespace"}, runner_group)`) is the runner shape, listing `RunnerGroup` and `RunnerSet` names together for the reason the Row 3 note gives.
- `$rate` is a **textbox**, not a query: the effective hourly cost of one worker slot, which is a fact about your contract and not something the cluster knows.
  Only the currency panels read it, so leaving the default in place still leaves every pod-hour and job count correct.

> **A textbox variable in a panel query needs the PromQL gate to know about it.** `make promql-check` parses every panel expression, and a Grafana variable in syntactic position (`[$__range]`, `* $rate`) is not valid PromQL.
> The checker substitutes from the dashboard's own `templating.list` before parsing, so a variable the dashboard never declares is reported by name rather than passing as a parse error nobody reads.

---

← Back to [Observability](observability.md)
