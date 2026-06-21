# Observability

> **Audience:** SRE, Platform engineer

Every component exposes Prometheus metrics from the standard `controller-runtime` metrics server, so built-in metrics (reconcile latency, work queue depth, etc.) are emitted automatically alongside the custom metrics below. The serving posture differs by component:

- **GMC manager** — `:8443/metrics`, served over TLS. How a scrape verifies the cert is controlled by `metrics.tls.certManager.enabled` (see [Verifying the metrics scrape TLS (GMC manager)](#verifying-the-metrics-scrape-tls-gmc-manager)).
- **Per-tenant AGC and proxy** — `:8443/metrics`, served over **mutual TLS**: a scraper must present a client certificate signed by that tenant's metrics CA. The GMC publishes the scraper's client bundle per tenant. See [Scraping per-tenant AGC and proxy metrics (mTLS)](#scraping-per-tenant-agc-and-proxy-metrics-mtls).

For SLO targets associated with these metrics, see [Appendix A — Capacity Targets & SLOs](../design/appendix-a-capacity-slos.md).

---

## Table of Contents

- [Logging](#logging)
- [Distributed Tracing (AGC)](#distributed-tracing-agc)
  - [Enabling tracing](#enabling-tracing)
  - [Enabling tracing on GMC-managed AGCs](#enabling-tracing-on-gmc-managed-agcs)
- [How to Access Metrics](#how-to-access-metrics)
  - [Install-time scraping prerequisites (GMC manager)](#install-time-scraping-prerequisites-gmc-manager)
  - [Verifying the metrics scrape TLS (GMC manager)](#verifying-the-metrics-scrape-tls-gmc-manager)
  - [Scraping per-tenant AGC and proxy metrics (mTLS)](#scraping-per-tenant-agc-and-proxy-metrics-mtls)
- [Full Metrics Reference](#full-metrics-reference)
  - [Proxy metrics](#proxy-metrics)
- [Symptom → Metric Mapping](#symptom--metric-mapping)
- [Recommended Alert Rules](#recommended-alert-rules)
- [SLO Recording Rules](#slo-recording-rules)
- [Grafana Dashboard](#grafana-dashboard)
  - [Suggested Panel Layout](#suggested-panel-layout)
  - [Dashboard Variables](#dashboard-variables)
- [Label Cardinality Warning](#label-cardinality-warning)

## Logging

All four components — the GMC, the per-tenant AGC, the egress proxy, and the worker wrapper — emit **structured JSON logs at info level by default**, one JSON shape per process stream, ready to ship to a log aggregator (Loki, Elasticsearch, CloudWatch, etc.) without reformatting. No flag needs to be set in production; the JSON default is what the GMC-provisioned Deployments run with.

The controllers (GMC, AGC) take controller-runtime's standard `zap` flags. For local development, pass `--zap-devel` to switch to human-readable console logs at debug level, or use the finer-grained `--zap-encoder` / `--zap-log-level` flags (run a controller with `--help` for the full set). Application code paths that log through the Go standard library's `log/slog` are bridged onto the same `zap` logger, so `--zap-log-level` governs **every** line a controller emits — not just the manager's own — and the whole process shares one JSON schema.

The egress proxy and the worker wrapper are not controllers; they read their level from the `LOG_LEVEL` environment variable (`info` | `debug`, default `info`).

**Per-tenant log level (GMC-managed AGCs).** For a tenant the GMC provisions, you do not set `--zap-log-level` or `LOG_LEVEL` by hand. Set `spec.logLevel` (`info` | `debug`, default `info`) on the `ActionsGateway` CR and the GMC threads it to **both** the AGC and that tenant's egress proxy as `LOG_LEVEL` (the AGC honors `LOG_LEVEL` unless an explicit `--zap-log-level` flag is passed; the GMC never stamps one). Flipping it rolls the AGC and proxy Deployments — it is a rolling restart, not a hot reload. See [tenant onboarding — per-tenant log level](tenant-onboarding.md#per-tenant-log-level).

### Debug diagnostics for otherwise-silent paths

Several paths that can stall a tenant or a session emit **debug**-level diagnostics (suppressed at the default info level). Raise the component to debug to surface them — for a GMC-managed tenant, set `spec.logLevel: debug` on the `ActionsGateway` (the GMC threads it to the AGC and proxy); for a standalone controller, `--zap-log-level=debug`; for a standalone proxy/worker, `LOG_LEVEL=debug`. Useful `grep` anchors:

| Path | Component | Log message substring |
|---|---|---|
| Session waiting on a worker pod that never reaches a terminal phase (top "stuck session" cause) | AGC | `pod already terminal at registration`, `registered for pod completion`, `pod completion observed`, `pod wait cancelled before completion` |
| Permanent baseline listener crash/restart backoff (otherwise only the `exited with error` warning is visible) | AGC | `restarting after backoff`, `restart aborted` |
| Which of the ~12 per-tenant provisioning steps a stalled reconcile is on | GMC | `reconcileResources step` |
| Per-tenant TLS cert issuance / renewal | GMC | `issuing proxy TLS cert`, `generating metrics mTLS bundle` |
| Per-session / per-job lifecycle (one line per listener spawn, job pickup, heal, and worker pod) | AGC | `listener goroutine started`, `job message received`, `idle shutdown`, `healing stale session`, `job finished; recycling single-use JIT agent`, `job Secret created`, `worker pod created`, `worker pod completed` |

These per-session/per-job lines are at **debug** by design: at thousands of
concurrent sessions they dominate log volume, so the default info stream carries
only the operator-relevant lifecycle events (concurrency-ceiling holds, quota
and eviction retries, errors). Raise the AGC to debug — `spec.logLevel: debug`
for a GMC-managed tenant, or `--zap-log-level=debug` standalone — to follow an
individual job.

**Correlation fields.** AGC log lines carry structured fields that let you follow
one session→job→pod through a log pipeline. Filter on `namespace` and `group`
(RunnerGroup name) to scope to a tenant's RunnerGroup; `agentIndex` and
`sessionId` identify a single listener goroutine and its current broker session
(the `sessionId` is rebound when a session is healed or an agent recycled, so it
always names the live session); `podName` appears on the provisioner lines for an
acquired job's worker pod.

Admission **rejections** (reserved-namespace, cross-namespace `gitHubAppRef`, privileged container, disallowed PriorityClass, silent securityProfile downgrade) are logged server-side at **info** — they need no debug flag — as `ActionsGateway admission denied` with the `operation`, `namespace`, `name`, and `reason` fields, giving an audit trail of denied attempts.

---

## Distributed Tracing (AGC)

The per-tenant AGC emits **OpenTelemetry traces** for its two hottest operational paths:

- **`RunnerGroup.Reconcile`** — one span per reconcile, attributed with `runnergroup.namespace` / `runnergroup.name`. Errors set the span status.
- **`Provisioner.provision`** — one span per acquired job (the job-to-pod path), with child spans `stageJobSecret`, `countActivePods`, `createPod`, and `waitForCompletion`. The root span carries `runnergroup.*`, `plan.id`, `pod.name`, `active_pods`, `ceiling.held`, `priority_class`, and the final `pod.phase` / `pod.reason` / `duration_seconds`. `waitForCompletion` is usually the long pole, so its child span tells you whether latency is in scheduling/runtime versus the controller.

Each reconcile and each job provision is its own root trace — there is no inbound trace context to continue, and the per-job spans run on the listener goroutines independently of the reconcile that started the pool.

**Tracing is opt-in and off by default.** With no OTLP endpoint configured the AGC installs no exporter and the spans are no-ops (near-zero cost), so production runs without tracing unless you point it at a collector.

### Enabling tracing

The AGC reads the **standard OpenTelemetry SDK environment variables** — there is no bespoke flag. Tracing turns on as soon as an OTLP endpoint is configured:

| Variable | Effect |
|---|---|
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` or `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP/gRPC collector address (e.g. `otel-collector.observability:4317`). Setting either one enables tracing. |
| `OTEL_SDK_DISABLED=true` | Hard kill switch — forces tracing off even when an endpoint is set. |
| `OTEL_SERVICE_NAME` / `OTEL_RESOURCE_ATTRIBUTES` | Override the default `service.name` (`actions-gateway-agc`) and add resource attributes. |
| `OTEL_TRACES_SAMPLER`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_TIMEOUT`, … | All other knobs are the SDK's standard env vars. |

On shutdown the AGC flushes buffered spans (5 s budget) before exiting.

### Enabling tracing on GMC-managed AGCs

The GMC builds the AGC Deployment, so for a GMC-provisioned tenant you do **not** set these env vars by hand — you declare tracing on the `ActionsGateway` CR and the GMC translates `spec.tracing` into the standard `OTEL_*` env on the AGC Deployment:

```yaml
apiVersion: actions-gateway.github.com/v1alpha1
kind: ActionsGateway
metadata:
  name: team-a
  namespace: team-a
spec:
  gitHubAppRef:
    name: team-a-github-app
  gitHubURL: https://github.com/team-a-org
  tracing:
    endpoint: https://otel-collector.observability:4317  # enables tracing
    sampler: parentbased_traceidratio                    # optional
    samplerArg: "0.1"                                     # optional — 10% of traces
    resourceAttributes:                                   # optional
      deployment.environment: prod
    # insecure: true   # only for a plaintext in-cluster collector; TLS is the default
```

| `spec.tracing` field | AGC env it sets | Notes |
|---|---|---|
| `endpoint` | `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | **Setting it is what enables tracing.** Empty → no `OTEL_*` env, tracing stays off. |
| `insecure` | `OTEL_EXPORTER_OTLP_TRACES_INSECURE` | Defaults to `false` (TLS). Set `true` only for a plaintext in-cluster collector. |
| `sampler` | `OTEL_TRACES_SAMPLER` | One of `always_on`, `always_off`, `traceidratio`, `parentbased_always_on`, `parentbased_always_off`, `parentbased_traceidratio` (CRD-enforced enum). |
| `samplerArg` | `OTEL_TRACES_SAMPLER_ARG` | Ratio in `[0,1]` for the ratio-based samplers. |
| `resourceAttributes` | `OTEL_RESOURCE_ATTRIBUTES` | Rendered as a sorted `key=value` list. The AGC's own `service.name`/`service.version` take precedence. |

> **No auth headers via env.** `spec.tracing` deliberately has no field for `OTEL_EXPORTER_OTLP_HEADERS`: those can carry bearer tokens, and this project keeps secrets out of environment variables (they leak into process listings and child processes). Authenticate the collector at the **network layer** instead — an in-cluster collector reached over the tenant's egress path, mutual TLS, or a service mesh.
>
> **Testing-only passthrough.** The `AGC_EXTRA_*` mechanism (`--allow-agc-extra-env` on the GMC, then `AGC_EXTRA_OTEL_EXPORTER_OTLP_ENDPOINT=…` in the GMC pod env) still exists but is gated for tests only and not for production use. When both are present, `AGC_EXTRA_*` wins (it is appended last). Prefer `spec.tracing`.

---

## How to Access Metrics

**Port forward (ad-hoc):** the per-tenant AGC/proxy metrics ports require a client
certificate (mTLS), so a plain `curl` is rejected at the TLS handshake. Present the
published scraper bundle (see [the per-tenant section](#scraping-per-tenant-agc-and-proxy-metrics-mtls)
for the Secret name and an end-to-end `curl --cert/--key/--cacert` example).

**Prometheus operator (production):** the chart wires scraping automatically when
`metrics.serviceMonitor.enabled=true` — both the GMC manager `ServiceMonitor` and a
per-tenant `ServiceMonitor` for each provisioned tenant's AGC/proxy. The metrics port
is named `metrics` (`:8443`) on every Service. You do not hand-author these
`ServiceMonitor`s; the sections below describe what they wire and the prerequisites.

### Install-time scraping prerequisites (GMC manager)

The default GMC install ships the manager NetworkPolicy **enabled by default**
(`networkPolicy.enabled=true`). Selecting the manager pod flips it to default-deny on
ingress, so its `/metrics` endpoint admits traffic only from namespaces carrying
the right label:

- **Scraping the GMC manager metrics:** label your Prometheus namespace
  `metrics: enabled`, or no scrape will reach the manager:
  ```bash
  kubectl label namespace <prometheus-namespace> metrics=enabled
  ```

The validating-webhook port (container `9443`) is intentionally re-allowed from
**any source** — the kube-apiserver that calls it is not a pod in a labeled
namespace, so a source restriction there would silently break every
`ActionsGateway` admission (`failurePolicy: Fail`). The webhook is TLS +
caBundle authenticated, so the sensitive surface stays the `metrics: enabled`
restriction above. No namespace label is required for CR admission.

This applies to the **GMC manager** only. The per-tenant AGC and proxy metrics
are governed by the per-tenant NetworkPolicies the GMC generates (the AGC and
proxy NPs already admit monitoring-namespace scrapes of the metrics port) and use
mutual TLS — see [Scraping per-tenant AGC and proxy metrics (mTLS)](#scraping-per-tenant-agc-and-proxy-metrics-mtls).

> Runtime enforcement of these policies depends on the CNI; kindnet's
> `kube-network-policies` does not drop all egress negatives (see the worker
> egress limitation in [troubleshooting.md](troubleshooting.md)). The manager NP
> is verified by manifest review and is pending a Tier-A runtime check.

The `ServiceMonitor` integration stays **opt-in**, behind the
`metrics.serviceMonitor.enabled` chart value (default `false`): out-of-box
Prometheus Operator scraping. It is left off by default because the
`ServiceMonitor` CRD only exists once the Prometheus Operator is installed, so
rendering it unconditionally would break `helm install` on clusters without it.

### Verifying the metrics scrape TLS (GMC manager)

The GMC metrics endpoint (`:8443`) is served over TLS. How the scrape verifies
that certificate is controlled by `metrics.tls.certManager.enabled`:

- **`true` (default, secure):** cert-manager issues a dedicated metrics serving
  cert — the `metrics-serving-cert` `Certificate`, minted from the same
  `selfsigned-issuer` as the webhook into the `metrics-server-cert` Secret. The
  GMC serves it (`--metrics-cert-path`), and the rendered `ServiceMonitor`
  verifies it against the issuing CA:

  ```yaml
  tlsConfig:
    serverName: <namePrefix>-controller-manager-metrics-service.<namespace>.svc
    ca:
      secret:
        name: metrics-server-cert
        key: ca.crt
  ```

  The scrape is authenticated end-to-end and **not** MITM-able. This path
  requires cert-manager (it reuses the webhook's Issuer) and is automatically
  inert when `certManager.enabled=false`.

- **`false`, or `certManager.enabled=false`:** the GMC falls back to
  controller-runtime's auto-generated self-signed metrics cert, and the
  `ServiceMonitor` scrapes with `tlsConfig.insecureSkipVerify: true`. Prometheus
  cannot verify the server, so an in-cluster attacker who can intercept the
  scrape connection could impersonate the metrics endpoint (the bearer token
  still authenticates the *scraper* to the server, but not the server to the
  scraper). Use this only on clusters without cert-manager, accepting the weaker
  posture.

The metrics-server-cert Secret is read from the **ServiceMonitor's namespace**
(the GMC release namespace), where the chart creates it — no extra copying is
needed. Because verification follows `certManager.enabled` by default, an
install that already uses cert-manager for the webhook (the default) gets
verified metrics scraping automatically once the `ServiceMonitor` is enabled.

### Scraping per-tenant AGC and proxy metrics (mTLS)

Each provisioned tenant runs its own AGC and egress-proxy pods, which serve
`/metrics` over **mutual TLS** on `:8443`. Unlike the GMC manager, the listener
**requires a client certificate** signed by that tenant's metrics CA — there is
no bearer-token or `insecureSkipVerify` fallback. Three things are wired
per tenant so Prometheus can scrape them:

1. **Metrics Services (always created).** The GMC creates a `metrics`-named
   `:8443` port on the proxy `Service` (`actions-gateway-proxy`) and a dedicated
   AGC `Service` (`actions-gateway-controller`), both in the tenant namespace.
   These exist regardless of the scrape toggle.
2. **Per-tenant `ServiceMonitor`s (opt-in).** When `metrics.serviceMonitor.enabled=true`,
   the GMC also creates one `ServiceMonitor` per component in the tenant
   namespace (`actions-gateway-proxy-metrics`, `actions-gateway-controller-metrics`).
   Each selects only its own component's Service via the tenant's owner labels, so
   one tenant's monitor never selects another tenant's pods.
3. **The scraper client bundle (mTLS).** Each `ServiceMonitor` presents the
   per-tenant scraper client bundle from the `actions-gateway-metrics-client`
   Secret in the tenant namespace — `tls.crt`/`tls.key` authenticate the scraper
   to the listener and `ca.crt` verifies the listener's server cert. `serverName`
   is the component's `<service>.<namespace>.svc` DNS name (a SAN on the server
   cert), so the scrape is verified end-to-end and **not** MITM-able:

   ```yaml
   tlsConfig:
     serverName: actions-gateway-proxy.<tenant-namespace>.svc
     ca:        { secret: { name: actions-gateway-metrics-client, key: ca.crt } }
     cert:      { secret: { name: actions-gateway-metrics-client, key: tls.crt } }
     keySecret: { name: actions-gateway-metrics-client, key: tls.key }
   ```

**Prerequisites:**

- **Prometheus Operator** must be installed (the `monitoring.coreos.com`
  `ServiceMonitor` CRD must exist). The toggle is off by default precisely
  because the CRD is not present on every cluster. If the CRD is absent when the
  toggle is on, the GMC logs a warning and emits a `ServiceMonitorCRDMissing`
  Event on the `ActionsGateway` and continues — a missing scrape prerequisite
  never blocks tenant provisioning.
- **Prometheus reads the client bundle from the `ServiceMonitor`'s namespace**
  (the tenant namespace), so the scraping Prometheus must be configured to select
  `ServiceMonitor`s across tenant namespaces (`serviceMonitorNamespaceSelector`)
  and granted read access to the per-tenant `actions-gateway-metrics-client`
  Secret. Each tenant has a distinct CA and client cert, which is why a single
  cluster-wide `ServiceMonitor` cannot scrape them — the wiring is necessarily
  per tenant.
- **NetworkPolicy:** label the Prometheus namespace `metrics: enabled` so the
  per-tenant NetworkPolicy admits the scrape (the AGC and proxy policies admit
  the `:8443` metrics port only from `metrics=enabled` namespaces).

**Ad-hoc verification** (mounting the published bundle locally):

```sh
ns=<tenant-namespace>
kubectl get secret actions-gateway-metrics-client -n "$ns" \
  -o jsonpath='{.data.tls\.crt}' | base64 -d > client.crt
kubectl get secret actions-gateway-metrics-client -n "$ns" \
  -o jsonpath='{.data.tls\.key}' | base64 -d > client.key
kubectl get secret actions-gateway-metrics-client -n "$ns" \
  -o jsonpath='{.data.ca\.crt}'  | base64 -d > ca.crt
kubectl port-forward -n "$ns" svc/actions-gateway-controller 8443:8443 &
curl --cert client.crt --key client.key --cacert ca.crt \
  https://actions-gateway-controller.$ns.svc:8443/metrics --resolve \
  "actions-gateway-controller.$ns.svc:8443:127.0.0.1"
rm -f client.crt client.key ca.crt   # delete the cert material when done
```

(The bundle is a client *certificate*, not a long-lived account credential; still
remove the files when finished.)

---

## Full Metrics Reference

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `actions_gateway_active_sessions` | Gauge | `namespace`, `runner_group` | Currently open long-poll sessions. One per RunnerGroup at steady state; rises toward `maxListeners` during bursts. |
| `actions_gateway_jobs_acquired_total` | Counter | `namespace`, `runner_group` | Jobs successfully acquired from the broker. |
| `actions_gateway_jobs_admission_rejected_total` | Counter | `namespace`, `runner_group` | Delivered jobs the pre-acquisition capacity gate left queued at GitHub (acquire skipped because the group is at its worker ceiling). Expected to rise under sustained saturation; a persistent gap vs. `jobs_acquired_total` means demand exceeds the group's `maxWorkers` / `priorityTiers` ceiling — raise the ceiling or namespace `ResourceQuota`. |
| `actions_gateway_job_acquisition_errors_total` | Counter | `namespace`, `reason` | Acquisition failures. Reason values: `already_claimed` (benign race), `delivery_window_expired` (job redelivered), `version_too_old`, `other`. |
| `actions_gateway_job_duration_seconds` | Histogram | `namespace`, `runner_group` | Wall time from `acquirejob` success to worker pod terminal phase. |
| `actions_gateway_pod_creation_latency_seconds` | Histogram | `namespace` | Time from worker pod creation to the runner container starting (scheduling + image pull). Key SLO metric — see [Appendix A](../design/appendix-a-capacity-slos.md). |
| `actions_gateway_token_refreshes_total` | Counter | `namespace` | Successful GitHub App installation token refreshes. |
| `actions_gateway_token_refresh_errors_total` | Counter | `namespace` | Failed token refresh attempts. See SLO threshold below. |
| `actions_gateway_renewjob_errors_total` | Counter | `namespace` | Failed `renewjob` calls. Leading indicator for cancelled jobs. |
| `actions_gateway_eviction_retries_total` | Counter | `namespace`, `runner_group` | Jobs automatically re-queued after worker pod eviction. |
| `actions_gateway_eviction_retries_exhausted_total` | Counter | `namespace`, `runner_group` | Eviction retries exhausted; job requires manual re-run. |
| `actions_gateway_worker_pods_reaped_total` | Counter | `namespace`, `runner_group`, `reason` | Worker pods deleted by the lifecycle reaper. `reason="completed_ttl"` is routine cleanup after `completedPodTTL`; `reason="pending_deadline"` means a pod was stuck Pending past `pendingPodDeadline` and its job was cancelled — each such reap also emits a `WorkerPodStuckPending` Warning Event on the RunnerGroup. |
| `actions_gateway_message_poll_errors_total` | Counter | `namespace`, `reason` | `GetMessage` errors (excludes empty polls and session expiry — those are normal). `reason="rate_limited"` is a 429; `reason="timeout"` is a black-holed long-poll the broker accepted but never answered, bounded by the client response-header deadline and retried (see [Listener Stalls After a Black-Holed Broker Connection](troubleshooting.md#listener-stalls-for-minutes-after-a-black-holed-broker-connection)); `reason="other"` is any remaining transport/decode error. |
| `actions_gateway_agent_recycles_total` | Counter | `namespace`, `runner_group`, `trigger` | Single-use JIT agents re-registered. `trigger="post_job"` is routine (one per completed job); `stale_session`/`startup` mean a dead agent was detected and healed after the fact; `reconcile_repair` means a parked agent was repaired by the reconciler. |
| `actions_gateway_agent_recycle_errors_total` | Counter | `namespace`, `runner_group` | Failed agent re-registration attempts. Sustained growth shrinks listener capacity — see the [runbook](troubleshooting.md#sessions-stuck-in-401eof-getmessage-loops-tenant-throughput-decays-to-zero). |
| `actions_gateway_worker_quota_pressure` | Gauge | `namespace`, `runner_group` | `1` when `WorkerQuotaPressure=True` (Q82): workers can't scale to the configured ceiling within the namespace `ResourceQuota` headroom. Warning — load-dependent; alert with `for:`, don't page. |
| `actions_gateway_worker_quota_exceeded` | Gauge | `namespace`, `runner_group` | `1` when `WorkerQuotaExceeded=True` (Q82): the `ResourceQuota` can't admit another worker pod — the next acquired job's pod will be rejected. Error — page. |
| `controller_runtime_reconcile_errors_total` | Counter | `controller` | GMC/AGC reconcile errors. Emitted by controller-runtime (no `actions_gateway_` prefix); the `controller` label distinguishes `actionsgateway`, `runnergroup`, etc. Non-zero values deserve investigation. |
| `actions_gateway_ip_range_updates_total` | Counter | `namespace` | `NetworkPolicy` egress rule refreshes from GitHub meta API. |
| `actions_gateway_managed_gateways` | Gauge | — | Total `ActionsGateway` CRs currently managed by the GMC. |
| `actions_gateway_proxy_quota_pressure` | Gauge | `namespace`, `name` | `1` when `ProxyQuotaPressure=True` (Q82): the proxy pool can't scale to `maxReplicas` within the namespace `ResourceQuota` headroom. Warning — alert with `for:`, don't page. |
| `actions_gateway_proxy_quota_exceeded` | Gauge | `namespace`, `name` | `1` when `ProxyQuotaExceeded=True` (Q82): proxy replica creates are being rejected by the `ResourceQuota` now. Error — page. |
| `actions_gateway_runnergroups_degraded` | Gauge | `namespace`, `name` | `1` when `RunnerGroupsDegraded=True` (Q158): one or more of the gateway's owned `RunnerGroup`s report an impairing condition (`CredentialUnavailable`/`Degraded`/`RunnerVersionTooOld`). Rolls child health up to the gateway; the impaired groups are named in the condition message. Advisory — does not gate `Ready`. |

### Proxy metrics

The per-tenant egress proxy exposes its own metrics on its health/metrics
port (`:8081`, restricted by the L-8 NetworkPolicy — see
[security.md L-8](../plan/security.md)). Each proxy is a separate scrape
target; these metrics carry no intrinsic `namespace` label, so attach one
via the `ServiceMonitor`/scrape config if you need per-tenant attribution.

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `actions_gateway_proxy_connections_active` | Gauge | — | Currently open CONNECT tunnels. |
| `actions_gateway_proxy_connections_total` | Counter | — | Total CONNECT tunnels opened. |
| `actions_gateway_proxy_dial_errors_total` | Counter | — | Upstream dial failures (e.g. blocked-destination attempts). |
| `actions_gateway_proxy_tunnel_duration_seconds` | Histogram | — | Tunnel lifetime, observed at close. Buckets reach 21600s (the 6h absolute lifetime cap). |

For abuse/compromise detection built on these metrics (slowloris,
eviction-retry loops, credential-harvesting), see
[security-operations.md](security-operations.md).

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
| Throughput decaying job by job | `agent_recycle_errors_total` rising, `active_sessions` shrinking | Agent re-registration failing; see the [runbook](troubleshooting.md#sessions-stuck-in-401eof-getmessage-loops-tenant-throughput-decays-to-zero) |
| Jobs cancelled without ever starting | `worker_pods_reaped_total{reason="pending_deadline"}` | Worker pod stuck Pending past the deadline — fix the image/scheduling cause; see the [runbook](troubleshooting.md#worker-pod-reaped-while-pending-workerpodstuckpending) |
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

      # Page: the namespace ResourceQuota is rejecting worker pods now (Q82)
      - alert: ActionsGatewayWorkerQuotaExceeded
        expr: |
          actions_gateway_worker_quota_exceeded == 1
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "Worker pods being rejected by ResourceQuota for {{ $labels.runner_group }} in {{ $labels.namespace }}"
          description: "The namespace ResourceQuota cannot admit another worker pod; acquired jobs will fail to schedule. Raise the quota or reduce maxWorkers."

      # Page: the ResourceQuota is rejecting proxy replicas now (Q82)
      - alert: ActionsGatewayProxyQuotaExceeded
        expr: |
          actions_gateway_proxy_quota_exceeded == 1
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "Proxy replica creation rejected by ResourceQuota for {{ $labels.name }} in {{ $labels.namespace }}"
          description: "The proxy pool is being held below the HPA's target by the namespace ResourceQuota. Raise the quota or lower proxy.maxReplicas."

      # Ticket: capacity can't reach the configured ceiling within quota headroom (Q82)
      - alert: ActionsGatewayQuotaPressure
        expr: |
          actions_gateway_worker_quota_pressure == 1 or actions_gateway_proxy_quota_pressure == 1
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "Quota headroom too low to reach configured ceiling in {{ $labels.namespace }}"
          description: "A proxy or worker pool cannot scale to its configured maximum within the namespace ResourceQuota headroom. Plan a quota increase or lower the ceiling before the next load spike."

      # Ticket: reconcile errors need investigation
      - alert: ActionsGatewayReconcileErrors
        expr: |
          rate(controller_runtime_reconcile_errors_total[5m]) > 0.033
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Reconcile errors in {{ $labels.controller }} for {{ $labels.resource }}"
          description: "Reconcile errors at >2/minute for 10+ minutes. Resources may be stale."
```

---

## SLO Recording Rules

These recording rules pre-compute the metrics needed for burn-rate alerting against the SLO targets in [Appendix A](../design/appendix-a-capacity-slos.md). Apply them alongside the alert rules above.

```yaml
groups:
  - name: actions-gateway-slos
    interval: 30s
    rules:

      # Pod creation latency — p95 and p99 per namespace
      - record: actions_gateway:pod_creation_latency_seconds:p95
        expr: |
          histogram_quantile(0.95,
            sum by (namespace, le) (
              rate(actions_gateway_pod_creation_latency_seconds_bucket[5m])
            )
          )

      - record: actions_gateway:pod_creation_latency_seconds:p99
        expr: |
          histogram_quantile(0.99,
            sum by (namespace, le) (
              rate(actions_gateway_pod_creation_latency_seconds_bucket[5m])
            )
          )

      # Job duration — p50, p95, p99 per namespace and runner_group
      - record: actions_gateway:job_duration_seconds:p50
        expr: |
          histogram_quantile(0.50,
            sum by (namespace, runner_group, le) (
              rate(actions_gateway_job_duration_seconds_bucket[5m])
            )
          )

      - record: actions_gateway:job_duration_seconds:p95
        expr: |
          histogram_quantile(0.95,
            sum by (namespace, runner_group, le) (
              rate(actions_gateway_job_duration_seconds_bucket[5m])
            )
          )

      # Token refresh error rate (hourly) — compare against the <1/hr SLO
      - record: actions_gateway:token_refresh_errors:rate1h
        expr: |
          sum by (namespace) (
            increase(actions_gateway_token_refresh_errors_total[1h])
          )

      # Job acquisition success rate — fraction of acquisitions that succeed
      - record: actions_gateway:job_acquisition_success_rate:rate5m
        expr: |
          sum by (namespace, runner_group) (
            rate(actions_gateway_jobs_acquired_total[5m])
          )
          /
          (
            sum by (namespace, runner_group) (
              rate(actions_gateway_jobs_acquired_total[5m])
            )
            +
            sum by (namespace, runner_group) (
              rate(actions_gateway_job_acquisition_errors_total[5m])
            )
          )
```

---

## Grafana Dashboard

The following panels cover the key health and performance signals. Use the recording rules above as data sources where applicable.

### Suggested Panel Layout

**Row 1 — Gateway Health (per namespace)**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Active sessions | `actions_gateway_active_sessions` | Stat / Time series |
| Jobs acquired/min | `rate(actions_gateway_jobs_acquired_total[5m]) * 60` | Time series |
| Token refresh errors | `rate(actions_gateway_token_refresh_errors_total[5m])` | Stat (threshold: >0 = red) |
| RenewJob errors | `rate(actions_gateway_renewjob_errors_total[5m])` | Stat (threshold: >0 = yellow) |

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
| Job duration p50/p95 | `actions_gateway:job_duration_seconds:p50/p95` | Time series |
| Eviction retries | `increase(actions_gateway_eviction_retries_total[1h])` | Bar chart |
| Eviction budget exhausted | `increase(actions_gateway_eviction_retries_exhausted_total[1h])` | Stat (threshold: >0 = red) |

**Row 4 — Proxy and Quota**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Proxy replica count | `kube_deployment_status_replicas_ready{deployment="actions-gateway-proxy"}` | Time series |
| HPA desired vs. current | HPA metrics from `kube_horizontalpodautoscaler_*` | Time series |
| ResourceQuota usage | `kube_resourcequota` filtered by namespace | Bar gauge |

**Row 5 — GMC Overview**

| Panel | Query | Visualization |
|-------|-------|---------------|
| Managed gateways | `actions_gateway_managed_gateways` | Stat |
| Reconcile errors | `rate(controller_runtime_reconcile_errors_total[5m])` | Time series by controller |
| IP range refreshes | `increase(actions_gateway_ip_range_updates_total[24h])` | Stat |

### Dashboard Variables

Add these template variables to make the dashboard multi-tenant:

- `$namespace` — `label_values(actions_gateway_active_sessions, namespace)` — allows filtering to a single tenant
- `$runner_group` — `label_values(actions_gateway_active_sessions{namespace="$namespace"}, runner_group)` — allows filtering to a specific RunnerGroup

---

## Label Cardinality Warning

Metric labels are scoped to `namespace` and `runner_group`. To avoid label cardinality explosion:

- **Do not use dynamically generated `runner_group` names** (e.g. names incorporating PR numbers or commit SHAs). Each unique combination of `namespace` + `runner_group` creates a distinct time series; thousands of unique names will cause memory pressure in Prometheus.
- **Stable, human-meaningful names** like `gpu-2x`, `cpu-standard`, `gpu-a100` are correct. These are configured in the `ActionsGateway` spec and should not change after initial setup.
- If you need per-workflow or per-repo attribution, use Prometheus recording rules or labels from job metadata, not from RunnerGroup names.
