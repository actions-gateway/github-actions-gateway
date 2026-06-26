# Q205 — Well-known label + metric/span naming audit

**Goal:** before the v2beta1 API freeze, make GAG "feel native" to the
Kubernetes ecosystem by (1) stamping the recommended `app.kubernetes.io/*` labels
on every object GAG creates and (2) aligning metric/span names to the
Prometheus / OpenTelemetry conventions. Names become load-bearing once operators
build selectors and dashboards on them, so renames are cheap now and breaking
later.

## Part 1 — recommended labels

Shared helper: `api/apilabels` (neutral module imported by both GMC and AGC) —
`apilabels.Recommended(name, instance, component, version, managedBy)` and
`apilabels.Merge(dst, …)`. The `app.kubernetes.io/*` set is **additive metadata**;
existing functional selector labels (`app:`, `actions-gateway/component: workload`,
the v2 per-gateway identity labels) are preserved untouched.

Canonical values:

| key | controller objects | proxy objects | worker objects |
|---|---|---|---|
| `name` | `actions-gateway-controller` | `actions-gateway-proxy` | `actions-runner` |
| `instance` | `<ActionsGateway/EgressProxy name>` | `<EgressProxy/ActionsGateway name>` | `<RunnerGroup/RunnerSet name>` |
| `component` | `controller` | `proxy` | `runner` |
| `part-of` | `actions-gateway` | `actions-gateway` | `actions-gateway` |
| `managed-by` | `actions-gateway-gmc` | `actions-gateway-gmc` | `actions-gateway-controller` |
| `version` | *(omitted — no build version plumbed)* | *(omitted)* | runner version (image tag → `names.RunnerVersion`) |

`version` is set only where a stable, meaningful value exists: the runner version
on worker pods and their job Secrets. Versionless infra (RBAC, NetworkPolicies,
Services, TLS Secrets) and control-plane objects (no controller build-version is
plumbed to the GMC/AGC at object-build time; `"dev"` is meaningless) omit it.

Coverage: GMC v1 + v2 builders (SA, RBAC, NetworkPolicy, Service, ServiceMonitor,
Deployment + pod template, Secret, PDB, HPA, RunnerGroup, EgressProxy children) and
the AGC provisioner (worker pod + job Secret).

## Part 2 — metric / span naming

**Metrics** are already largely conformant (every counter ends `_total`; every
histogram carries the `_seconds` base unit; gauges are sensibly named). Audited all
21 `actions_gateway_*` series. One genuine snake_case fix:

| old | new |
|---|---|
| `actions_gateway_renewjob_errors_total` | `actions_gateway_renew_job_errors_total` |

`pod_creation_latency_seconds` was considered for `…_duration_seconds` (sibling of
`job_duration_seconds`) but kept: `latency` is a recognised Prometheus term, the
metric already carries its base unit, and the rename's blast radius (two Grafana
dashboards, recording-rule names that bake in `latency`, the SLO appendix) is large
for a synonym swap.

**Spans/attributes** — span names already describe operations at low cardinality
(kept). Attributes aligned to OTel semconv: k8s-native attributes now use the
`k8s.*` semconv keys, and the remaining custom attributes are consistently
namespaced under `gateway.`:

| old | new |
|---|---|
| `owner.namespace`, `runnergroup.namespace` | `k8s.namespace.name` (semconv) |
| `pod.name` | `k8s.pod.name` (semconv) |
| `owner.name` | `gateway.owner.name` |
| `runnergroup.name` | `gateway.runnergroup.name` |
| `plan.id` | `gateway.plan.id` |
| `active_pods` | `gateway.active_pods` |
| `ceiling.held` | `gateway.ceiling_held` |
| `priority_class` | `gateway.priority_class` |
| `pod.phase` | `gateway.pod.phase` |
| `pod.reason` | `gateway.pod.reason` |
| `duration_seconds` | `gateway.provision.duration_seconds` |

All metric/span renames are documented as breaking observability changes in
`docs/operations/observability.md` with the old→new mapping.

## Testing
- `make check` (gofmt, golangci-lint, shellcheck, STATUS lint, unit tests).
- envtest assertions on created-object labels (label presence is real-apiserver
  observable) in the GMC + AGC integration suites.
