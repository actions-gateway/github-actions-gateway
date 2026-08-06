#!/usr/bin/env python3
"""Synthetic actions_gateway_* exporter (stdlib only).

Emits the metrics the reference dashboard queries, with counters/histograms
that grow with elapsed time so rate() and histogram_quantile() behave like a
live system. Values are deterministic functions of time + a little jitter.
"""
import math
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

START = time.time()

# (namespace, runner_group) tenants.
TENANTS = [
    ("team-a", "gpu-a100"),
    ("team-a", "cpu-standard"),
    ("team-a", "gpu-2x"),
    ("team-b", "cpu-standard"),
]
NAMESPACES = ["team-a", "team-b"]

# (namespace, runner_set) for the scale-set acquisition tier (Q264 default
# protocol). Labelled by runner_set (not runner_group); a ScaleSet-protocol
# RunnerSet emits none of the classic series above.
SCALESETS = [
    ("team-a", "gpu-a100"),
    ("team-a", "cpu-standard"),
    ("team-b", "cpu-standard"),
]

# The worker-capacity gate is opt-in, and the collector emits its gauge only for a
# RunnerSet that enables one (Q643). Gate a subset so the preview shows the tile
# populated without implying every set carries the condition.
GATED_SCALESETS = {("team-a", "gpu-a100"), ("team-b", "cpu-standard")}

POD_BUCKETS = [0.5, 1, 2.5, 5, 10, 15, 30, 60, 120, 300]   # +Inf appended
JOB_BUCKETS = [1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048]
PROXY_BUCKETS = [0.1, 0.5, 1, 5, 10, 60, 300, 1800, 3600, 21600]
# (namespace, gateway-name) for the GMC-exported fleet condition gauges.
GATEWAYS = [("team-a", "team-a"), ("team-b", "team-b")]
CONTROLLERS = ["actionsgateway", "runnergroup", "runnerset"]


def jitter(seed, amp):
    return amp * math.sin(seed + time.time() / 37.0)


def wavy_total(base_rate, elapsed, seed=0.0, amp=0.4, period=300.0):
    """Monotonic counter total whose *rate* gently undulates over time.

    A plain int(base_rate * elapsed) yields a dead-straight rate() line. This
    integrates a slow sinusoid into the total so the derived rate()/increase()
    panels read like a live system with ebbing and flowing load. Stays
    monotonically non-decreasing (a counter must never go backwards): the
    instantaneous rate is base_rate * (1 + amp*cos(...)), which is > 0 for any
    amp < 1.
    """
    w = (2.0 * math.pi) / period
    total = base_rate * (elapsed + (amp / w) * (math.sin(w * elapsed + seed) - math.sin(seed)))
    return int(max(0.0, total))


def hist_lines(name, labels_prefix, buckets, total_rate, center_idx, elapsed):
    """Emit cumulative *_bucket/_sum/_count for a histogram.

    Counts grow at total_rate/sec; mass concentrates around center_idx so the
    computed quantiles land in a realistic place.
    """
    out = []
    sep = "," if labels_prefix else ""  # avoid a leading comma when unlabelled
    # Per-bucket weights: a rough bell around center_idx.
    weights = [math.exp(-((i - center_idx) ** 2) / 3.0) for i in range(len(buckets))]
    wsum = sum(weights)
    total = total_rate * elapsed
    cumulative = 0.0
    for i, le in enumerate(buckets):
        cumulative += total * weights[i] / wsum
        out.append(f'{name}_bucket{{{labels_prefix}{sep}le="{le}"}} {int(cumulative)}')
    out.append(f'{name}_bucket{{{labels_prefix}{sep}le="+Inf"}} {int(total)}')
    # _sum: approximate as count * representative center value.
    out.append(f'{name}_sum{{{labels_prefix}}} {int(total * buckets[center_idx])}')
    out.append(f'{name}_count{{{labels_prefix}}} {int(total)}')
    return out


def render():
    elapsed = max(1.0, time.time() - START)
    L = []

    L.append("# actions_gateway synthetic metrics")
    for ns, rg in TENANTS:
        sessions = max(1, int(3 + jitter(hash((ns, rg)) % 10, 1.4)))
        L.append(f'actions_gateway_active_sessions{{namespace="{ns}",runner_group="{rg}"}} {sessions}')

    for ns, rg in TENANTS:
        rate = {"gpu-a100": 0.12, "cpu-standard": 0.7, "gpu-2x": 0.05}.get(rg, 0.2)
        acquired = wavy_total(rate, elapsed, seed=hash((ns, rg)) % 7)
        L.append(f'actions_gateway_jobs_acquired_total{{namespace="{ns}",runner_group="{rg}"}} {acquired}')

    for ns in NAMESPACES:
        L.append(f'actions_gateway_job_acquisition_errors_total{{namespace="{ns}",reason="already_claimed"}} {int(0.01 * elapsed)}')
        L.append(f'actions_gateway_job_acquisition_errors_total{{namespace="{ns}",reason="delivery_window_expired"}} {int(0.002 * elapsed)}')
        # GetMessage errors by reason (excludes empty polls / session expiry).
        L.append(f'actions_gateway_message_poll_errors_total{{namespace="{ns}",reason="timeout"}} {int(0.003 * elapsed)}')
        L.append(f'actions_gateway_message_poll_errors_total{{namespace="{ns}",reason="rate_limited"}} {int(0.0005 * elapsed)}')
        # renewjob teardowns: definitive lock loss (Q254).
        L.append(f'actions_gateway_renew_job_teardowns_total{{namespace="{ns}",reason="job_not_found"}} {int(0.0008 * elapsed)}')
        L.append(f'actions_gateway_token_refreshes_total{{namespace="{ns}"}} {int(0.0003 * elapsed) + 1}')
        L.append(f'actions_gateway_token_refresh_errors_total{{namespace="{ns}"}} 0')
        L.append(f'actions_gateway_renew_job_errors_total{{namespace="{ns}"}} 0')
        L.append(f'actions_gateway_ip_range_updates_total{{namespace="{ns}"}} 1')
        # pod creation latency: center ~ bucket index 3-4 (5-10s) -> p95<15, p99~12
        L += hist_lines("actions_gateway_pod_creation_latency_seconds", f'namespace="{ns}"', POD_BUCKETS, 1.2, 3, elapsed)

    for ns, rg in TENANTS:
        # job duration: center idx 6 (~64-128s)
        L += hist_lines("actions_gateway_job_duration_seconds", f'namespace="{ns}",runner_group="{rg}"', JOB_BUCKETS, 0.3, 6, elapsed)
        L.append(f'actions_gateway_eviction_retries_total{{namespace="{ns}",runner_group="{rg}"}} {int(0.002 * elapsed)}')
        L.append(f'actions_gateway_eviction_retries_exhausted_total{{namespace="{ns}",runner_group="{rg}"}} 0')
        L.append(f'actions_gateway_quota_retries_total{{namespace="{ns}",runner_group="{rg}"}} {int(0.001 * elapsed)}')
        L.append(f'actions_gateway_quota_retries_exhausted_total{{namespace="{ns}",runner_group="{rg}"}} 0')
        # single-use JIT agent recycling: routine post-job recycles, no errors
        L.append(f'actions_gateway_agent_recycles_total{{namespace="{ns}",runner_group="{rg}",trigger="post_job"}} {int(0.15 * elapsed)}')
        L.append(f'actions_gateway_agent_recycle_errors_total{{namespace="{ns}",runner_group="{rg}"}} 0')
        # worker-pod reaper: routine completed_ttl cleanup, occasional pending_deadline.
        # The real counter always carries all four labels; runner_set is empty on the
        # classic tier and non-empty only on scale-set reaps (Q514).
        L.append(f'actions_gateway_worker_pods_reaped_total{{namespace="{ns}",runner_group="{rg}",runner_set="",reason="completed_ttl"}} {int(0.14 * elapsed)}')
        L.append(f'actions_gateway_worker_pods_reaped_total{{namespace="{ns}",runner_group="{rg}",runner_set="",reason="pending_deadline"}} {int(0.0002 * elapsed)}')
        # broker OAuth token-propagation retries during recycle churn (Q267)
        L.append(f'actions_gateway_broker_token_propagation_retries_total{{namespace="{ns}",runner_group="{rg}"}} {int(0.004 * elapsed)}')
        # fan-out safety trio (Q260 / Q266): benign steady rates during bursts
        L.append(f'actions_gateway_jobs_duplicate_delivery_total{{namespace="{ns}",runner_group="{rg}"}} {int(0.006 * elapsed)}')
        L.append(f'actions_gateway_abandoned_delivery_completions_total{{namespace="{ns}",runner_group="{rg}",outcome="completed"}} {int(0.006 * elapsed)}')
        L.append(f'actions_gateway_fanout_loser_recycle_deferred_total{{namespace="{ns}",runner_group="{rg}",outcome="winner_concluded"}} {int(0.006 * elapsed)}')
        # tenant health-condition gauges all healthy (0)
        L.append(f'actions_gateway_worker_quota_pressure{{namespace="{ns}",runner_group="{rg}"}} 0')
        L.append(f'actions_gateway_worker_quota_exceeded{{namespace="{ns}",runner_group="{rg}"}} 0')
        L.append(f'actions_gateway_workers_unschedulable{{namespace="{ns}",runner_group="{rg}"}} 0')

    # Scale-set acquisition tier (Q264/Q311): one Listener per ScaleSet-protocol
    # RunnerSet. Assigned drives demand 1:1 (no sibling fan-out); provisioned
    # tracks just below it; a trickle of retried provision errors; completions
    # split mostly-succeeded by result.
    for ns, rs in SCALESETS:
        rate = {"gpu-a100": 0.12, "cpu-standard": 0.7}.get(rs, 0.2)
        assigned = wavy_total(rate, elapsed, seed=hash((ns, rs)) % 7)
        provisioned = int(assigned * 0.98)
        completed = int(provisioned * 0.95)
        L.append(f'actions_gateway_scaleset_jobs_assigned_total{{namespace="{ns}",runner_set="{rs}"}} {assigned}')
        L.append(f'actions_gateway_scaleset_jobs_provisioned_total{{namespace="{ns}",runner_set="{rs}"}} {provisioned}')
        L.append(f'actions_gateway_scaleset_provision_errors_total{{namespace="{ns}",runner_set="{rs}"}} {int(0.002 * elapsed)}')
        L.append(f'actions_gateway_scaleset_jobs_completed_total{{namespace="{ns}",runner_set="{rs}",result="succeeded"}} {int(completed * 0.9)}')
        L.append(f'actions_gateway_scaleset_jobs_completed_total{{namespace="{ns}",runner_set="{rs}",result="failed"}} {int(completed * 0.08)}')
        L.append(f'actions_gateway_scaleset_jobs_completed_total{{namespace="{ns}",runner_set="{rs}",result="canceled"}} {int(completed * 0.02)}')
        # v2 worker-capacity gauges (Q319), all healthy (0). Emitted for every
        # RunnerSet regardless of acquisition protocol, which is why the dashboard's
        # $runner_set variable reads its label values from this family.
        L.append(f'actions_gateway_runnerset_worker_quota_pressure{{namespace="{ns}",runner_set="{rs}"}} 0')
        L.append(f'actions_gateway_runnerset_worker_quota_exceeded{{namespace="{ns}",runner_set="{rs}"}} 0')
        L.append(f'actions_gateway_runnerset_workers_unschedulable{{namespace="{ns}",runner_set="{rs}"}} 0')
        # Scale-set reaps carry the CR name on both runner_group and runner_set, which
        # is what joins them to the runner_set-labelled gauges above (Q514/Q651).
        # orphaned_running is the scale-set-specific reason: a worker that registered
        # but never received its job.
        L.append(f'actions_gateway_worker_pods_reaped_total{{namespace="{ns}",runner_group="{rs}",runner_set="{rs}",reason="completed_ttl"}} {int(0.12 * elapsed)}')
        L.append(f'actions_gateway_worker_pods_reaped_total{{namespace="{ns}",runner_group="{rs}",runner_set="{rs}",reason="orphaned_running"}} {int(0.005 * elapsed)}')
        # Opt-in capacity gate healthy: condition False with the reason that says the
        # gate evaluated and found room, not the latched AwaitingProbe (Q512/Q643).
        if (ns, rs) in GATED_SCALESETS:
            L.append(f'actions_gateway_runnerset_worker_capacity_declined{{namespace="{ns}",runner_set="{rs}",reason="CapacityAvailable"}} 0')

    # Per-tenant egress proxy. The proxy exposes no intrinsic namespace label, but
    # the per-tenant ServiceMonitor stamps one from the scrape target's namespace
    # (Q314) so the tenant dashboard's proxy panels can filter by $namespace. Mirror
    # that here: emit one proxy series per namespace, labelled with it.
    for ns in NAMESPACES:
        L.append(f'actions_gateway_proxy_connections_active{{namespace="{ns}"}} {max(0, int(8 + jitter(3, 4)))}')
        L.append(f'actions_gateway_proxy_connections_total{{namespace="{ns}"}} {int(0.5 * elapsed)}')
        L.append(f'actions_gateway_proxy_dial_errors_total{{namespace="{ns}"}} {int(0.001 * elapsed)}')
        # tunnel duration: center idx 5 (~60s)
        L += hist_lines("actions_gateway_proxy_tunnel_duration_seconds", f'namespace="{ns}"', PROXY_BUCKETS, 0.5, 5, elapsed)

    # GMC fleet rollups.
    L.append("actions_gateway_managed_gateways 4")
    for ns, name in GATEWAYS:
        L.append(f'actions_gateway_runnergroups_degraded{{namespace="{ns}",name="{name}"}} 0')
        L.append(f'actions_gateway_egress_rules_stale{{namespace="{ns}",name="{name}"}} 0')
        L.append(f'actions_gateway_proxy_quota_pressure{{namespace="{ns}",name="{name}"}} 0')
        L.append(f'actions_gateway_proxy_quota_exceeded{{namespace="{ns}",name="{name}"}} 0')
        L.append(f'actions_gateway_github_egress_incomplete{{namespace="{ns}",name="{name}"}} 0')

    # controller-runtime built-ins: healthy reconcile throughput, no errors.
    for c in CONTROLLERS:
        L.append(f'controller_runtime_reconcile_errors_total{{controller="{c}"}} 0')
        L.append(f'controller_runtime_reconcile_total{{controller="{c}",result="success"}} {int(0.2 * elapsed)}')

    return ("\n".join(L) + "\n").encode()


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/metrics"):
            body = render()
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; version=0.0.4")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok\n")

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    HTTPServer(("0.0.0.0", 9100), Handler).serve_forever()
