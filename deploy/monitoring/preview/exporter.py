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

POD_BUCKETS = [0.5, 1, 2.5, 5, 10, 15, 30, 60, 120, 300]   # +Inf appended
JOB_BUCKETS = [1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048]
PROXY_BUCKETS = [0.1, 0.5, 1, 5, 10, 60, 300, 1800, 3600, 21600]
# (namespace, gateway-name) for the GMC-exported fleet condition gauges.
GATEWAYS = [("team-a", "team-a"), ("team-b", "team-b")]
CONTROLLERS = ["actionsgateway", "runnergroup", "runnerset"]


def jitter(seed, amp):
    return amp * math.sin(seed + time.time() / 37.0)


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
        L.append(f'actions_gateway_jobs_acquired_total{{namespace="{ns}",runner_group="{rg}"}} {int(rate * elapsed)}')

    for ns in NAMESPACES:
        L.append(f'actions_gateway_job_acquisition_errors_total{{namespace="{ns}",reason="already_claimed"}} {int(0.01 * elapsed)}')
        L.append(f'actions_gateway_token_refreshes_total{{namespace="{ns}"}} {int(0.0003 * elapsed) + 1}')
        L.append(f'actions_gateway_token_refresh_errors_total{{namespace="{ns}"}} 0')
        L.append(f'actions_gateway_renewjob_errors_total{{namespace="{ns}"}} 0')
        L.append(f'actions_gateway_ip_range_updates_total{{namespace="{ns}"}} 1')
        # pod creation latency: center ~ bucket index 3-4 (5-10s) -> p95<15, p99~12
        L += hist_lines("actions_gateway_pod_creation_latency_seconds", f'namespace="{ns}"', POD_BUCKETS, 1.2, 3, elapsed)

    for ns, rg in TENANTS:
        # job duration: center idx 6 (~64-128s)
        L += hist_lines("actions_gateway_job_duration_seconds", f'namespace="{ns}",runner_group="{rg}"', JOB_BUCKETS, 0.3, 6, elapsed)
        L.append(f'actions_gateway_eviction_retries_total{{namespace="{ns}",runner_group="{rg}"}} {int(0.002 * elapsed)}')
        L.append(f'actions_gateway_eviction_retries_exhausted_total{{namespace="{ns}",runner_group="{rg}"}} 0')
        # single-use JIT agent recycling: routine post-job recycles, no errors
        L.append(f'actions_gateway_agent_recycles_total{{namespace="{ns}",runner_group="{rg}",trigger="post_job"}} {int(0.15 * elapsed)}')
        L.append(f'actions_gateway_agent_recycle_errors_total{{namespace="{ns}",runner_group="{rg}"}} 0')
        # tenant health-condition gauges all healthy (0)
        L.append(f'actions_gateway_worker_quota_pressure{{namespace="{ns}",runner_group="{rg}"}} 0')
        L.append(f'actions_gateway_worker_quota_exceeded{{namespace="{ns}",runner_group="{rg}"}} 0')
        L.append(f'actions_gateway_workers_unschedulable{{namespace="{ns}",runner_group="{rg}"}} 0')

    # Per-tenant egress proxy (no intrinsic namespace label — one target/tenant).
    L.append(f'actions_gateway_proxy_connections_active {max(0, int(8 + jitter(3, 4)))}')
    L.append(f'actions_gateway_proxy_connections_total {int(0.5 * elapsed)}')
    L.append(f'actions_gateway_proxy_dial_errors_total {int(0.001 * elapsed)}')
    # tunnel duration: center idx 5 (~60s)
    L += hist_lines("actions_gateway_proxy_tunnel_duration_seconds", "", PROXY_BUCKETS, 0.5, 5, elapsed)

    # GMC fleet rollups.
    L.append("actions_gateway_managed_gateways 4")
    for ns, name in GATEWAYS:
        L.append(f'actions_gateway_runnergroups_degraded{{namespace="{ns}",name="{name}"}} 0')
        L.append(f'actions_gateway_egress_rules_stale{{namespace="{ns}",name="{name}"}} 0')
        L.append(f'actions_gateway_proxy_quota_pressure{{namespace="{ns}",name="{name}"}} 0')
        L.append(f'actions_gateway_proxy_quota_exceeded{{namespace="{ns}",name="{name}"}} 0')

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
