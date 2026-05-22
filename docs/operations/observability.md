# Observability

Both the GMC and AGC expose Prometheus metrics at `:8080/metrics` (no authentication required by default). The standard `controller-runtime` metrics server is used; additional built-in metrics (reconcile latency, work queue depth, etc.) are emitted automatically alongside the custom metrics below.

For SLO targets associated with these metrics, see [Appendix A — Capacity Targets & SLOs](../design/appendix-a-capacity-slos.md).

---

## How to Access Metrics

**Port forward (ad-hoc):**
```sh
kubectl port-forward -n <namespace> deploy/actions-gateway-controller 8080:8080
curl http://localhost:8080/metrics
```

**Prometheus operator (production):**

Create a `ServiceMonitor` targeting the AGC and GMC services:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: actions-gateway
  namespace: <namespace>
spec:
  selector:
    matchLabels:
      app: actions-gateway-controller
  endpoints:
    - port: metrics
      interval: 30s
```

The metrics port is named `metrics` in the Service spec.

---

## Full Metrics Reference

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `actions_gateway_active_sessions` | Gauge | `namespace`, `runner_group` | Currently open long-poll sessions. One per RunnerGroup at steady state; rises toward `maxListeners` during bursts. |
| `actions_gateway_jobs_acquired_total` | Counter | `namespace`, `runner_group` | Jobs successfully acquired from the broker. |
| `actions_gateway_job_acquisition_errors_total` | Counter | `namespace`, `reason` | Acquisition failures. Reason values: `already_claimed` (benign race), `delivery_window_expired` (job redelivered), `version_too_old`, `other`. |
| `actions_gateway_job_duration_seconds` | Histogram | `namespace`, `runner_group` | Wall time from `acquirejob` success to worker pod terminal phase. |
| `actions_gateway_pod_creation_latency_seconds` | Histogram | `namespace` | Time from `acquirejob` to pod `Scheduled` event. Key SLO metric. |
| `actions_gateway_token_refreshes_total` | Counter | `namespace` | Successful GitHub App installation token refreshes. |
| `actions_gateway_token_refresh_errors_total` | Counter | `namespace` | Failed token refresh attempts. See SLO threshold below. |
| `actions_gateway_renewjob_errors_total` | Counter | `namespace` | Failed `renewjob` calls. Leading indicator for cancelled jobs. |
| `actions_gateway_eviction_retries_total` | Counter | `namespace`, `runner_group` | Jobs automatically re-queued after worker pod eviction. |
| `actions_gateway_eviction_retries_exhausted_total` | Counter | `namespace`, `runner_group` | Eviction retries exhausted; job requires manual re-run. |
| `actions_gateway_message_poll_errors_total` | Counter | `namespace` | `GetMessage` errors (excludes empty polls and session expiry — those are normal). |
| `actions_gateway_reconcile_errors_total` | Counter | `controller`, `resource` | GMC/AGC reconcile errors. Non-zero values deserve investigation. |
| `actions_gateway_ip_range_updates_total` | Counter | `namespace` | `NetworkPolicy` egress rule refreshes from GitHub meta API. |
| `actions_gateway_managed_gateways` | Gauge | — | Total `ActionsGateway` CRs currently managed by the GMC. |

---

## Symptom → Metric Mapping

| Symptom | Metric(s) to check | Notes |
| --- | --- | --- |
| Jobs are slow to start | `pod_creation_latency_seconds` p95/p99 | SLO: p95 ≤ 15s, p99 ≤ 60s |
| Jobs are randomly cancelled | `renewjob_errors_total` | Each sustained error risks a job cancellation |
| Jobs are not being acquired | `active_sessions` (should be ≥ 1 per RunnerGroup), `job_acquisition_errors_total` | Zero sessions = no polling |
| Jobs are queuing but not starting | `active_sessions` (OK) vs `jobs_acquired_total` not incrementing | Check `RateLimited` condition |
| Runner credentials are broken | `token_refresh_errors_total` | Spikes indicate Secret or GitHub App issue |
| Evictions causing re-runs | `eviction_retries_total`, `eviction_retries_exhausted_total` | Exhausted budget requires manual intervention |
| Proxy autoscaling not working | HPA TARGETS showing `<unknown>` | `requests.cpu` not set on proxy pods |
| GMC/AGC reconcile broken | `reconcile_errors_total` | Non-zero sustained rate indicates operator issue |

---

## Recommended Alert Rules

The following Prometheus alerting rules map to the SLO targets in [Appendix A](../design/appendix-a-capacity-slos.md). Adjust thresholds to match your environment.

```yaml
groups:
  - name: actions-gateway
    rules:

      # Page: no sessions means no job acquisition
      - alert: ActionsGatewayNoActiveSessions
        expr: |
          actions_gateway_active_sessions == 0
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "No active listener sessions for {{ $labels.runner_group }} in {{ $labels.namespace }}"
          description: "The AGC has no open long-poll sessions. Jobs queue indefinitely until sessions are restored."

      # Page: token refresh errors risk job failures within ~1 hour
      - alert: ActionsGatewayTokenRefreshErrors
        expr: |
          rate(actions_gateway_token_refresh_errors_total[5m]) > 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "GitHub App token refresh errors in {{ $labels.namespace }}"
          description: "Token refresh has been failing for 5+ minutes. Sessions will fail once the current token expires (~1 hour)."

      # Page: sustained renewjob failures will cancel running jobs
      - alert: ActionsGatewayRenewJobErrors
        expr: |
          rate(actions_gateway_renewjob_errors_total[5m]) > 0.1
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "RenewJob errors in {{ $labels.namespace }}"
          description: "RenewJob is failing at >0.1/s for 5+ minutes. Running jobs may be cancelled."

      # Page: p99 pod creation latency SLO breach
      - alert: ActionsGatewayPodCreationLatencyP99
        expr: |
          histogram_quantile(0.99,
            rate(actions_gateway_pod_creation_latency_seconds_bucket[5m])
          ) > 60
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "Pod creation p99 latency SLO breach in {{ $labels.namespace }}"
          description: "p99 pod creation latency exceeds 60s SLO. Check quota and node capacity."

      # Ticket: p95 pod creation latency SLO breach
      - alert: ActionsGatewayPodCreationLatencyP95
        expr: |
          histogram_quantile(0.95,
            rate(actions_gateway_pod_creation_latency_seconds_bucket[5m])
          ) > 15
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Pod creation p95 latency degraded in {{ $labels.namespace }}"
          description: "p95 pod creation latency exceeds 15s SLO. Investigate quota and scheduling."

      # Ticket: eviction budget exhausted — manual re-run required
      - alert: ActionsGatewayEvictionRetriesExhausted
        expr: |
          increase(actions_gateway_eviction_retries_exhausted_total[5m]) > 0
        labels:
          severity: warning
        annotations:
          summary: "Eviction retry budget exhausted for {{ $labels.runner_group }} in {{ $labels.namespace }}"
          description: "A job's eviction retry budget has been exhausted. Manual re-run required."

      # Ticket: reconcile errors need investigation
      - alert: ActionsGatewayReconcileErrors
        expr: |
          rate(actions_gateway_reconcile_errors_total[5m]) > 0.033
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Reconcile errors in {{ $labels.controller }} for {{ $labels.resource }}"
          description: "Reconcile errors at >2/minute for 10+ minutes. Resources may be stale."
```

---

## Label Cardinality Warning

Metric labels are scoped to `namespace` and `runner_group`. To avoid label cardinality explosion:

- **Do not use dynamically generated `runner_group` names** (e.g. names incorporating PR numbers or commit SHAs). Each unique combination of `namespace` + `runner_group` creates a distinct time series; thousands of unique names will cause memory pressure in Prometheus.
- **Stable, human-meaningful names** like `gpu-2x`, `cpu-standard`, `gpu-a100` are correct. These are configured in the `ActionsGateway` spec and should not change after initial setup.
- If you need per-workflow or per-repo attribution, use Prometheus recording rules or labels from job metadata, not from RunnerGroup names.
