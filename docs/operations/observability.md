# Observability

Both the GMC and AGC expose Prometheus metrics at `/metrics`.

## Key metrics

| Metric | Description |
| --- | --- |
| `actions_gateway_active_sessions` | Currently open long-poll sessions per runner group |
| `actions_gateway_jobs_acquired_total` | Jobs successfully acquired |
| `actions_gateway_job_duration_seconds` | Wall time from acquire to pod completion |
| `actions_gateway_pod_creation_latency_seconds` | Time from acquire to pod scheduled |
| `actions_gateway_eviction_retries_total` | Jobs automatically re-queued after eviction |
| `actions_gateway_eviction_retries_exhausted_total` | Evicted jobs where retry budget was exhausted |
| `actions_gateway_token_refresh_errors_total` | Failed GitHub App token refreshes |
| `actions_gateway_renewjob_errors_total` | RenewJob failures (leading indicator for cancelled jobs) |

For the SLO targets associated with these metrics, see [Appendix A — Capacity Targets & SLOs](../design/appendix-a-capacity-slos.md).
