# Logging & Tracing

> **Audience:** Platform engineer, Tenant operator

Part of the [Observability](observability.md) guide.
See also: [Metrics reference](observability-metrics.md) · [Accessing metrics](observability-metrics-access.md) · [Alerting & SLOs](observability-alerting.md) · [Grafana dashboards](observability-dashboards.md).

## Logging

All four components — the GMC, the per-tenant AGC, the egress proxy, and the worker wrapper — emit **structured JSON logs at info level by default**, one JSON shape per process stream, ready to ship to a log aggregator (Loki, Elasticsearch, CloudWatch, etc.) without reformatting.
No flag needs to be set in production; the JSON default is what the GMC-provisioned Deployments run with.

The controllers (GMC, AGC) take controller-runtime's standard `zap` flags.
For local development, pass `--zap-devel` to switch to human-readable console logs at debug level, or use the finer-grained `--zap-encoder` / `--zap-log-level` flags (run a controller with `--help` for the full set).
Application code paths that log through the Go standard library's `log/slog` are bridged onto the same `zap` logger, so `--zap-log-level` governs **every** line a controller emits — not just the manager's own — and the whole process shares one JSON schema.

The egress proxy and the worker wrapper are not controllers; they read their level from the `LOG_LEVEL` environment variable (`info` | `debug`, default `info`).

**Per-tenant log level (GMC-managed AGCs).** For a tenant the GMC provisions, you do not set `--zap-log-level` or `LOG_LEVEL` by hand.
Set `spec.logLevel` (`info` | `debug`, default `info`) on the `ActionsGateway` CR and the GMC threads it to **both** the AGC and that tenant's egress proxy as `LOG_LEVEL` (the AGC honors `LOG_LEVEL` unless an explicit `--zap-log-level` flag is passed; the GMC never stamps one).
Flipping it rolls the AGC and proxy Deployments — it is a rolling restart, not a hot reload.
On the v2 (`actions-gateway.com`) API the knob is split per kind: `ActionsGateway.spec.logLevel` covers the AGC, and each proxy pool carries its own `EgressProxy.spec.logLevel` (same values, default, and rolling-restart semantics).
See [tenant onboarding — per-tenant log level](tenant-onboarding.md#per-tenant-log-level).

### Debug diagnostics for otherwise-silent paths

Several paths that can stall a tenant or a session emit **debug**-level diagnostics (suppressed at the default info level).
Raise the component to debug to surface them — for a GMC-managed tenant, set `spec.logLevel: debug` on the `ActionsGateway` (the GMC threads it to the AGC and proxy); for a standalone controller, `--zap-log-level=debug`; for a standalone proxy/worker, `LOG_LEVEL=debug`.
Useful `grep` anchors:

| Path | Component | Log message substring |
|---|---|---|
| Session waiting on a worker pod that never reaches a terminal phase (top "stuck session" cause) | AGC | `pod already terminal at registration`, `registered for pod completion`, `pod completion observed`, `pod wait cancelled before completion` |
| Permanent baseline listener crash/restart backoff (otherwise only the `exited with error` warning is visible) | AGC | `restarting after backoff`, `restart aborted` |
| Which of the ~12 per-tenant provisioning steps a stalled reconcile is on | GMC | `reconcileResources step` |
| Per-tenant TLS cert issuance / renewal | GMC | `issuing proxy TLS cert`, `generating metrics mTLS bundle` |
| Per-session / per-job lifecycle (one line per listener spawn, job pickup, heal, and worker pod) | AGC | `listener goroutine started`, `job message received`, `idle shutdown`, `healing stale session`, `job finished; recycling single-use JIT agent`, `job Secret created`, `worker pod created`, `worker pod completed` |

These per-session/per-job lines are at **debug** by design: at thousands of concurrent sessions they dominate log volume, so the default info stream carries only the operator-relevant lifecycle events (concurrency-ceiling holds, quota and eviction retries, errors).
Raise the AGC to debug — `spec.logLevel: debug` for a GMC-managed tenant, or `--zap-log-level=debug` standalone — to follow an individual job.

**Correlation fields.** AGC log lines carry structured fields that let you follow one session→job→pod through a log pipeline.
Filter on `namespace` and `group` (RunnerGroup name) to scope to a tenant's RunnerGroup; `agentIndex` and `sessionId` identify a single listener goroutine and its current broker session (the `sessionId` is rebound when a session is healed or an agent recycled, so it always names the live session); `podName` appears on the provisioner lines for an acquired job's worker pod.
On the scale-set tier those lines also carry `jobID` and `runID`: one workflow run has many jobs, so the pair is what separates sibling jobs of one run from a redelivered assignment for a single job.

Admission **rejections** (reserved-namespace, cross-namespace `gitHubAppRef`, privileged container, disallowed PriorityClass, silent securityProfile downgrade) are logged server-side at **info** — they need no debug flag — as `ActionsGateway admission denied` with the `operation`, `namespace`, `name`, and `reason` fields, giving an audit trail of denied attempts.

**API server warnings** (e.g. the [`v1alpha1`](upgrade.md#non-breaking-v1alpha1-is-deprecated-and-the-apiserver-now-warns) and [`v2alpha1`](upgrade.md#non-breaking-v2alpha1-is-deprecated-and-the-apiserver-now-warns) deprecation warnings) are logged at info, **deduplicated to one line per unique message per process** in both controllers.
The apiserver attaches such a warning to every read and write of a deprecated API version, so without dedup a set reconciling under churn would drown the log in repeats; expect the first occurrence only, not one per API call.

### Proxy egress audit record

A proxy pool can write one structured line per **accepted** CONNECT, answering "which tenant reached which destination, when, and how much moved" without reconstructing it from cluster flow logs or GitHub's own audit log.

It is **off by default** and opted into per pool:

```yaml
apiVersion: actions-gateway.com/v2beta1
kind: EgressProxy
metadata:
  name: shared
  namespace: team-a
spec:
  auditLogging: Connections   # Off (default) writes no per-connection record
```

Flipping it rolls the proxy pool, since the value is part of the pod template, so the records start once the new pods are up rather than on the write.
See [tenant onboarding: per-pool egress audit record](tenant-onboarding.md#per-pool-egress-audit-record) for when to turn it on and what it costs.

A record looks like this (one JSON object per line, on the same stdout stream as everything else the pool logs):

```json
{
  "time": "2026-08-24T19:58:03.114Z",
  "level": "INFO",
  "msg": "egress audit",
  "namespace": "team-a",
  "event": "connect",
  "host": "api.github.com",
  "port": "443",
  "bytesToDestination": 4182,
  "bytesFromDestination": 91744,
  "durationSeconds": 12.406
}
```

Selecting the audit stream: filter on `msg == "egress audit"`, then on `event` to pick a record kind.
`connect` is the only one today, and a later kind reuses the message rather than inventing a second one.
The fields:

| Field | Meaning |
|---|---|
| `namespace` | The namespace the proxy **pool** runs in, read from the downward API. On a pool no other namespace references this is the consuming tenant; on one shared via `spec.sharing.allowedNamespaces` it names the pool, not the consumer, because a CONNECT carries no namespace. Omitted entirely by a proxy run outside Kubernetes, never emitted empty |
| `event` | `connect`, the record kind |
| `host` / `port` | The CONNECT destination. `host` is capped at 253 **bytes** (the longest legal DNS name) and gains a `…(truncated)` marker if cut, so a capped value never reads as a real destination. The cut lands on a rune boundary, so a raw-UTF-8 internationalized name is never left as a broken character. The same cap applies to every proxy log line carrying the destination (the denial, the dial failure, and the response-write failure), not only to the audit record |
| `bytesToDestination` / `bytesFromDestination` | Bytes relayed each way, final at tunnel close |
| `durationSeconds` | How long the tunnel was open |

Three properties worth knowing before you build on it:

- **Accepted CONNECTs only.** A refused destination and a failed upstream dial are not in this stream; they are the existing `CONNECT destination not allowed` warn and `upstream dial failed` error lines, and the `actions_gateway_proxy_connect_denied_total` / `actions_gateway_proxy_dial_errors_total` counters.
- **Written at info.** An audit record never depends on raising the pool to `debug`, and raising it to `debug` never adds a field to the record.
- **One line per connection.** Under load that is the pool's dominant log volume, which is the cost the opt-in is asking you to accept, so size the pipeline before turning it on.

What the record deliberately does *not* carry, and why, is in the [security design](../design/05-security.md#proxy-egress-audit-record).

### What never appears in logs

Ship AGC and proxy logs to a shared aggregator without a scrubbing pipeline in front: **credential material is redacted before a log line is emitted**, not by the log stack afterward.

Every GitHub response body — broker and runner-scale-set protocol alike — passes through a single redaction path before it can reach a log line or an error message.
It replaces:

- JSON credential fields (`access_token`, `refresh_token`, `token`, `encoded_jit_config`, `client_secret`, `private_key`, `password`, `secret`);
- GitHub token literals by prefix (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_`), JSON Web Tokens, and long base64 blobs anywhere in the body.

Redaction runs **before truncation**, so a shortened body cannot cut a secret loose from the field name that would have matched it, and it is **not level-dependent** — `spec.logLevel: debug` surfaces more lifecycle lines, never unsanitized bodies.

The redaction complements how credentials are handled upstream of logging: GitHub App keys and JIT configs are mounted as files, never environment variables (env leaks into `kubectl describe`, process listings, and child processes), and both controllers bypass their informer caches for Secret bodies so key material is not cache-resident.
See [security §5 — credential handling](../design/05-security.md).

---

## Distributed Tracing (AGC)

The per-tenant AGC emits **OpenTelemetry traces** for its two hottest operational paths:

- **`RunnerGroup.Reconcile`** — one span per reconcile, attributed with `k8s.namespace.name` / `gateway.runnergroup.name`.
  Errors set the span status.
- **`Provisioner.provision`** — one span per acquired job (the job-to-pod path), with child spans `stageJobSecret`, `countActivePods`, `createPod`, and `waitForCompletion`.
  The root span carries `k8s.namespace.name`, `gateway.owner.name`, `gateway.plan.id`, `k8s.pod.name`, `gateway.active_pods`, `gateway.ceiling_held`, `gateway.priority_class`, and the final `gateway.pod.phase` / `gateway.pod.reason` / `gateway.provision.duration_seconds`.
  `waitForCompletion` is usually the long pole, so its child span tells you whether latency is in scheduling/runtime versus the controller.

> Span attribute names follow the [OpenTelemetry semantic conventions](https://opentelemetry.io/docs/specs/semconv/): Kubernetes-native attributes use the `k8s.*` keys (`k8s.namespace.name`, `k8s.pod.name`); project-specific attributes are namespaced under `gateway.`.
> These keys were renamed in the Q205 naming audit — see [Breaking observability changes](observability-metrics.md#breaking-observability-changes-q205) for the old→new mapping.

Each reconcile and each job provision is its own root trace — there is no inbound trace context to continue, and the per-job spans run on the listener goroutines independently of the reconcile that started the pool.

**Tracing is opt-in and off by default.** With no OTLP endpoint configured the AGC installs no exporter and the spans are no-ops (near-zero cost), so production runs without tracing unless you point it at a collector.

### Enabling tracing

The AGC reads the **standard OpenTelemetry SDK environment variables** — there is no bespoke flag.
Tracing turns on as soon as an OTLP endpoint is configured:

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

> **No auth headers via env.** `spec.tracing` deliberately has no field for `OTEL_EXPORTER_OTLP_HEADERS`: those can carry bearer tokens, and this project keeps secrets out of environment variables (they leak into process listings and child processes).
> Authenticate the collector at the **network layer** instead — an in-cluster collector reached over the tenant's egress path, mutual TLS, or a service mesh.
>
> **Testing-only passthrough.** The `AGC_EXTRA_*` mechanism (`--allow-agc-extra-env` on the GMC, then `AGC_EXTRA_OTEL_EXPORTER_OTLP_ENDPOINT=…` in the GMC pod env) still exists but is gated for tests only and not for production use.
> When both are present, `AGC_EXTRA_*` wins (it is appended last).
> Prefer `spec.tracing`.

---

← Back to [Observability](observability.md)
