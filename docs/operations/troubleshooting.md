# Troubleshooting Guide

Audience: on-call site reliability engineer (SRE), platform engineer.

Each section below covers a specific failure mode: symptoms, likely cause, diagnostic commands, and resolution steps.

---

## Table of Contents

- [How to Validate a Fresh Deployment](#how-to-validate-a-fresh-deployment)
- [Helm Render Fails: gmc.image Must Be Pinned by Digest](#helm-render-fails-gmcimage-must-be-pinned-by-digest)
- [GMC Not Provisioning Tenant Resources](#gmc-not-provisioning-tenant-resources)
- [Tenant Namespace Missing the Managed-Tenant Marker Label](#tenant-namespace-missing-the-managed-tenant-marker-label)
- [AGC CrashLoopBackOff or Not Acquiring Jobs](#agc-crashloopbackoff-or-not-acquiring-jobs)
- [RunnerGroup ActiveSessions Exceeds maxListeners](#runnergroup-activesessions-exceeds-maxlisteners)
- [Proxy NetworkPolicy Has an Empty GitHub Allowlist](#proxy-networkpolicy-has-an-empty-github-allowlist)
- [Worker Pods Stuck Pending](#worker-pods-stuck-pending)
- [Worker Pod Reaped While Pending (WorkerPodStuckPending)](#worker-pod-reaped-while-pending-workerpodstuckpending)
- [Proxy Pool Not Scaling](#proxy-pool-not-scaling)
- [Proxy Tunnel Closed Mid-Stream — Idle or Lifetime Cap](#proxy-tunnel-closed-mid-stream--idle-or-lifetime-cap)
- [Metrics scrape returns a TLS / connection error](#metrics-scrape-returns-a-tls--connection-error)
- [RateLimited Condition on ActionsGateway](#ratelimited-condition-on-actionsgateway)
- [GitHub App Secret Misconfiguration](#github-app-secret-misconfiguration)
- [Token Refresh Errors Spiking](#token-refresh-errors-spiking)
- [RenewJob Failures Rising](#renewjob-failures-rising)
- [Sessions Stuck in 401/EOF GetMessage Loops (Tenant Throughput Decays to Zero)](#sessions-stuck-in-401eof-getmessage-loops-tenant-throughput-decays-to-zero)
- [Network Connectivity Failures](#network-connectivity-failures)
- [AGC Cannot Reach the Kubernetes API Server (NetworkPolicy + post-DNAT port mismatch)](#agc-cannot-reach-the-kubernetes-api-server-networkpolicy--post-dnat-port-mismatch)
- [Worker Pod Runner.Worker Fails TLS Handshake With UntrustedRoot](#worker-pod-runnerworker-fails-tls-handshake-with-untrustedroot)
- [Evicted Worker Pods Exhausting Retry Budget](#evicted-worker-pods-exhausting-retry-budget)
- [Jobs Failing Due to Namespace ResourceQuota Exhaustion](#jobs-failing-due-to-namespace-resourcequota-exhaustion)
- [Worker Pod Fails to Start After Secure-by-Default SecurityContext](#worker-pod-fails-to-start-after-secure-by-default-securitycontext)
- [securityProfile Downgrade Rejected by Admission Webhook](#securityprofile-downgrade-rejected-by-admission-webhook)
- [Tracing Sampler Rejected by Admission](#tracing-sampler-rejected-by-admission)
- [Worker-Pod Lifecycle Duration Rejected by Admission](#worker-pod-lifecycle-duration-rejected-by-admission)
- [Worker Pod Crashes With configuredSettings ArgumentNullException](#worker-pod-crashes-with-configuredsettings-argumentnullexception)
- [kubectl apply ActionsGateway Times Out On Webhook During GMC Rollout](#kubectl-apply-actionsgateway-times-out-on-webhook-during-gmc-rollout)
- [Worker HTTPS_PROXY Returns connection refused During Proxy Rollout](#worker-https_proxy-returns-connection-refused-during-proxy-rollout)
- [Prometheus Not Scraping Proxy or AGC Metrics](#prometheus-not-scraping-proxy-or-agc-metrics)
- [Proxy Replica Stuck Pending After Enabling HA Defaults](#proxy-replica-stuck-pending-after-enabling-ha-defaults)

## How to Validate a Fresh Deployment

Run these checks immediately after deploying a new tenant gateway or upgrading existing components.

```sh
# 1. Check ActionsGateway status
kubectl get actionsgateway -n <namespace> -o yaml | grep -A 20 status:

# 2. Confirm the AGC pod is running
kubectl get deploy -n <namespace> actions-gateway-controller
kubectl logs -n <namespace> deploy/actions-gateway-controller --tail=50

# 3. Confirm the proxy pool is healthy
kubectl get deploy -n <namespace> actions-gateway-proxy
kubectl get hpa -n <namespace>

# 4. Confirm RunnerGroup resources exist
kubectl get runnergroup -n <namespace>

# 5. Check for condition errors
kubectl get actionsgateway -n <namespace> -o jsonpath='{.status.conditions}' | jq .
```

Expected state after a healthy deployment:

- `ActionsGateway` condition `Ready=True`.
- `ActionsGateway` condition `AGCAvailable=True`.
- `ActionsGateway` condition `ProxyAvailable=True`.
- AGC Deployment: `READY 1/1`.
- Proxy Deployment: `READY` count >= `minReplicas`.
- HPA: `TARGETS` showing a CPU percentage (not `<unknown>`).
- Each RunnerGroup has at least one listener session (`actions_gateway_active_sessions > 0`).

---

## Helm Render Fails: `gmc.image` Must Be Pinned by Digest

**Symptoms.** `helm install`, `helm upgrade`, `helm lint`, or `helm template` of the `actions-gateway` chart fails immediately with:

```
Error: execution error at (actions-gateway/templates/deployment.yaml:...):
gmc.image must be pinned by digest: set gmc.image.digest=sha256:<64 hex digits>
(see docs/operations/install.md, "Pin images by digest").
DEV/TEST ONLY: set allowFloatingImageTags=true to allow a floating tag.
```

**Cause.** `gmc.image.digest` is empty in the release values. The chart enforces digest pinning of the GMC's own controller image at render time (secure by default): nothing at runtime validates the image the GMC itself runs from, so an empty digest must never silently fall back to a mutable `:latest` tag. Common ways to get here:

- A fresh install without `--set gmc.image.digest=sha256:<gmc>`.
- A `helm upgrade` with a values file (or `--reset-values`) that omits the digest. (`--reuse-values` carries the previously pinned digest forward.)
- Offline rendering (`helm template` / `helm lint`) without supplying a digest.

**Resolution.**

- **Production:** pin the digest published for the release you are installing (see [release.md](release.md) for where digests are recorded):

  ```sh
  helm upgrade --install gag charts/actions-gateway \
    --namespace gmc-system \
    --set gmc.image.digest=sha256:<gmc> \
    --set agc.image.digest=sha256:<agc> \
    --set proxy.image.digest=sha256:<proxy>
  ```

- **Dev/test only:** `--set allowFloatingImageTags=true` allows a floating tag for the GMC image *and* disables the GMC's startup digest check on the AGC/proxy images. Never use it in production.
- **Offline rendering:** any well-formed digest satisfies the check, e.g. `--set-string gmc.image.digest=sha256:1111111111111111111111111111111111111111111111111111111111111111`.

Note the contrast with the AGC/proxy images: those are validated by the GMC **at startup** (a floating tag there crash-loops the GMC — see [install.md § Pin images by digest](install.md#pin-images-by-digest)), while the GMC's own image is validated by the chart **at render time**.

---

## GMC Not Provisioning Tenant Resources

**Symptoms.** An `ActionsGateway` CR was applied but nothing has been created in the tenant namespace: no AGC Deployment, no proxy Deployment, no RunnerGroup resources.

**Likely causes.**
- GMC pod is not running or not the leader.
- GMC lacks permission to write to the tenant namespace (RBAC misconfiguration during initial GMC install).
- The `ActionsGateway` CR failed admission validation (check for validation errors in `kubectl apply` output or `Events`).

**Diagnostics.**

```sh
# Check whether the GMC is running and has a leader
kubectl get lease -n gmc-system
kubectl get pods -n gmc-system

# Check GMC logs for reconcile errors
kubectl logs -n gmc-system deploy/gmc-controller-manager --tail=100 | grep -i error

# Check events on the ActionsGateway CR
kubectl describe actionsgateway -n <namespace> <name>

# Check the Ready condition
kubectl get actionsgateway -n <namespace> <name> -o jsonpath='{.status.conditions}' | jq .
```

**Resolution.**
- If the GMC pod is not running, restore it from its Deployment.
- If RBAC is missing, re-run `make install` and `make deploy` from the GMC source.
- If the admission webhook is rejecting the CR, fix the CR spec and re-apply.
- If a reconcile error is logged (e.g. `failed to create Deployment`), check the `actions_gateway_reconcile_errors_total` metric and read the full error from the GMC logs. Fix the underlying permissions or quota issue and the GMC's reconciler will retry.

---

## Tenant Namespace Missing the Managed-Tenant Marker Label

**Symptoms.** An `ActionsGateway` never becomes `Ready`. `kubectl describe` shows a
`Warning` event with reason `NamespaceMarkerMissing`, and the GMC log reports a
`Forbidden` error stamping Pod Security Admission labels, citing the
`namespace-psa-guard` admission policy. This is common immediately after upgrading a
cluster whose tenant namespaces predate the policy (see
[Upgrade — Migration Notes](upgrade.md#migration-notes)).

**Cause.** The GMC's cluster-wide `namespaces:patch` grant is gated by the
`namespace-psa-guard` ValidatingAdmissionPolicy, which denies the GMC any namespace
that is not labelled `actions-gateway.github.com/tenant: "true"`. The label confines
the grant to managed tenants so a compromised GMC cannot relabel `kube-system` PSA
(see [Security §5.1/§5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in)).
The GMC never sets this label itself — a trusted administrator must apply it.

**Diagnostics.**

```sh
# Confirm the warning event
kubectl describe actionsgateway -n <namespace> <name> | grep -A2 NamespaceMarkerMissing

# Check whether the marker label is present
kubectl get namespace <namespace> \
  -o jsonpath='{.metadata.labels.actions-gateway\.github\.com/tenant}'   # want: true

# Confirm the policy and its binding are installed
kubectl get validatingadmissionpolicy gmc-namespace-psa-guard
kubectl get validatingadmissionpolicybinding gmc-namespace-psa-guard-binding
```

**Resolution.** Apply the marker label as an administrator, then the GMC reconciler
retries automatically:

```sh
kubectl label namespace <namespace> actions-gateway.github.com/tenant=true
```

If the GMC's ServiceAccount is installed under a non-default namespace or name, also
confirm the policy's `matchConditions` username
(`system:serviceaccount:gmc-system:gmc-controller-manager`) matches your install.

---

## AGC CrashLoopBackOff or Not Acquiring Jobs

**Symptoms.** The AGC pod is restarting repeatedly, or it is running but `actions_gateway_active_sessions` is zero and `actions_gateway_jobs_acquired_total` is not incrementing even when jobs are queued.

**Likely causes.**
- GitHub App Secret is missing, malformed, or contains an invalid private key.
- GitHub App `installationId` or `appId` is wrong.
- The proxy pool is not reachable from the AGC (network policy or proxy pod not ready).
- The AGC binary was built with an incompatible runner version (GitHub returns 400 on session creation).

**Diagnostics.**

```sh
# Check pod status and restarts
kubectl get pod -n <namespace> -l app=actions-gateway-controller

# Check logs for startup errors
kubectl logs -n <namespace> deploy/actions-gateway-controller

# Check that the referenced Secret exists and has the right keys
kubectl get secret -n <namespace> <gitHubAppRef.name>
kubectl get secret -n <namespace> <gitHubAppRef.name> -o jsonpath='{.data}' | jq 'keys'
# Expected keys: appId, installationId, privateKey

# Test proxy reachability — the AGC image is distroless (no shell, no curl),
# so spawn an ephemeral curl pod in the same namespace and use the same proxy URL.
kubectl run nettest-$$ -n <namespace> --rm -it --restart=Never \
  --image=curlimages/curl:latest \
  --overrides='{"spec":{"automountServiceAccountToken":false,"containers":[{"name":"c","image":"curlimages/curl:latest","command":["sh","-c","curl -x https://actions-gateway-proxy:8080 -sI https://api.github.com"]}]}}'

# Check RunnerGroup conditions
kubectl get runnergroup -n <namespace> -o yaml | grep -A 10 conditions

# Check RunnerGroup events — the AGC emits Warning events for the common failures.
kubectl describe runnergroup -n <namespace> <name>
# Look for:
#   TokenUnavailable          — GitHub App installation token could not be fetched (Secret/appId/installationId).
#   AgentPoolError            — agent Secret provisioning (EnsureAgents) failed.
#   ListenerStartFailed       — listener goroutines could not be (re)started.
#   AgentDeregistrationFailed — agent Secret cleanup on scale-down/delete failed.
#   NoActiveSessions / ListenerActive — Ready condition transitions.
```

**Resolution.**
- If the Secret is missing or has wrong keys, recreate it. See [Getting Started — GitHub App Secret](../getting-started.md#2-create-a-github-app-credential-secret).
- If the private key format is wrong, ensure it is a PEM-encoded RSA key starting with `-----BEGIN RSA PRIVATE KEY-----`. The Secret `stringData.privateKey` must include the full key including header and footer lines.
- If the runner version is outdated, update `workerImage` in the RunnerGroup spec (or the AGC's `--worker-image` flag). Watch for `RunnerGroup` conditions with reason `VersionTooOld`.
- If `appId` or `installationId` are wrong, update the Secret.

---

## RunnerGroup ActiveSessions Exceeds maxListeners

**Symptoms.** `kubectl get runnergroup -n <namespace> -o jsonpath='{.items[*].status.activeSessions}'` reports a value greater than the group's `spec.maxListeners`, typically climbing by one after each broker or network outage. GitHub shows more concurrent runner sessions for the group than the configured ceiling.

**What happened.** On AGC versions without the Q100 fix, a recoverable crash of the permanent baseline listener left the active count at zero for the duration of the restart backoff; a reconcile firing inside that window started a second permanent baseline on top of the pending restart. Permanent listeners are restarted after every recoverable exit and are exempt from the `maxListeners` ceiling, so each repeat of the race ratchets the session count up by one, indefinitely. Fixed versions make the multiplexer start idempotent, so the race cannot stack baselines.

**Resolution.**
- Upgrade the AGC image to a version with the Q100 fix.
- To clear excess listeners immediately on an affected version, restart the AGC Deployment (`kubectl rollout restart deploy/actions-gateway-controller -n <namespace>`). Listener sessions are in-memory; the restarted AGC re-creates exactly one baseline per RunnerGroup.

---

## Proxy NetworkPolicy Has an Empty GitHub Allowlist

**Symptoms.** On a freshly provisioned tenant, all proxy egress to GitHub is silently dropped: `curl` through the proxy times out (no `502`), the AGC cannot acquire jobs, and token refresh fails. The proxy `NetworkPolicy` exists but its `ipBlock` egress peers are empty.

**Likely cause.** The IP Range Reconciler's initial `api.github.com/meta` fetch failed or stalled at GMC startup. The cached ranges seed each proxy `NetworkPolicy`'s `ipBlock` allowlist; until the first fetch lands, the allowlist is empty. The reconciler retries the initial fetch on a capped exponential backoff (1s→30s), so a transient outage normally self-heals within seconds — but a sustained inability to reach `api.github.com` from the GMC pod (egress firewall, DNS, or a long GitHub outage) leaves the allowlist empty until connectivity returns.

**Diagnostics.**

```sh
# Inspect the proxy NetworkPolicy's GitHub ipBlock egress peers — empty means the cache never populated.
kubectl get networkpolicy -n <namespace> actions-gateway-proxy \
  -o jsonpath='{.spec.egress[*].to[*].ipBlock.cidr}'

# Look for retry warnings in the GMC log.
kubectl logs -n gmc-system deploy/gmc-controller-manager \
  | grep -i "GitHub IP-range"
```

**Resolution.**
- Confirm the GMC pod itself can reach `api.github.com` (corporate egress firewall, DNS, or proxy in front of the cluster). The reconciler retries automatically; once connectivity is restored the next successful fetch patches every existing proxy `NetworkPolicy`.
- If the tenant manages its own egress policy (Cilium/Calico FQDN rules), set `spec.proxy.managedNetworkPolicy: false` so the reconciler leaves the policy alone.

---

## Worker Pods Stuck Pending

**Symptoms.** Jobs are acquired (`actions_gateway_jobs_acquired_total` increments) but worker pods remain in `Pending` state for more than 60 seconds. `pod_creation_latency_seconds` p95 exceeds the 15s SLO target.

**Likely causes.**
- Namespace `ResourceQuota` is exhausted — no pod slot, CPU request, or memory request available.
- No node has enough capacity for the pod's requested resources (GPU nodes may be at capacity).
- `PriorityClass` referenced in `priorityTiers` does not exist.
- Image pull is slow due to a large image on a cold node (expected; see SLO targets in [Appendix A](../design/appendix-a-capacity-slos.md)).

**Diagnostics.**

```sh
# Check quota usage
kubectl describe resourcequota -n <namespace>

# Describe a stuck pod to see the scheduling event
kubectl describe pod -n <namespace> <worker-pod-name>
# Look for: "Insufficient cpu", "Insufficient memory", "Insufficient nvidia.com/gpu",
#           "no nodes available to schedule pods", "didn't match PodDisruptionBudget"

# Check whether the PriorityClass exists
kubectl get priorityclass <priorityClassName>

# Check node capacity
kubectl describe nodes | grep -A 5 "Allocated resources"
```

**Resolution.**
- If quota is exhausted: increase `namespaceQuota` in the `ActionsGateway` spec or reduce `maxWorkers` / last-tier threshold.
- If no GPU nodes are available: check node autoscaler status or provision additional nodes.
- If a `PriorityClass` is missing: create it (cluster-admin action) or remove the tier reference.
- If image pull is slow (first job on a cold node): this is expected. If it exceeds the p99 SLO (60s), consider pre-pulling the image via a DaemonSet or enabling image streaming.

**Deadline.** A pod that stays Pending is not held forever: after `pendingPodDeadline` (default 10m, per-RunnerGroup) the AGC deletes it to free the concurrency-ceiling slot it was holding — see the next runbook section. Diagnose a stuck pod (`kubectl describe pod`) *before* the deadline reaps it, or raise `pendingPodDeadline` temporarily while debugging.

---

## Worker Pod Reaped While Pending (WorkerPodStuckPending)

**Symptoms.** A `Warning` Event with reason `WorkerPodStuckPending` appears on the RunnerGroup (`kubectl describe runnergroup -n <namespace>`), `actions_gateway_worker_pods_reaped_total{reason="pending_deadline"}` increments, and the job the pod was created for is cancelled by GitHub (it never started, so its lock lapsed). The worker pod itself is gone.

**What happened.** The pod stayed `Pending` longer than the RunnerGroup's `pendingPodDeadline` (default 10m), so the AGC deleted it. A permanently Pending pod would otherwise hold one of the group's concurrency-ceiling slots forever — the ceiling counts Pending pods. The deadline is a capacity-protection mechanism, not a retry mechanism: the job is **not** re-queued automatically.

**Likely causes.**
- `workerImage` (or the `podTemplate` container image) does not exist or is not pullable from the cluster — `ErrImagePull` / `ImagePullBackOff`.
- `podTemplate` scheduling constraints (nodeSelector, tolerations, GPU resources) that no node satisfies.
- Node autoscaler provisioning slower than the deadline (common for GPU node pools).

**Diagnostics.**

```sh
# The reap event names the deleted pod and the deadline that fired
kubectl get events -n <namespace> --field-selector reason=WorkerPodStuckPending

# Rate of reaps per group
# PromQL: rate(actions_gateway_worker_pods_reaped_total{reason="pending_deadline"}[1h])

# Reproduce the pull/scheduling failure before the next reap:
# trigger a job, then describe the new Pending pod within the deadline window
kubectl get pods -n <namespace> -l actions-gateway/runner-group=<group> -w
kubectl describe pod -n <namespace> <worker-pod-name>
```

**Resolution.**
- Fix the unpullable image or unsatisfiable scheduling constraint — that is the root cause; the reap is the messenger.
- If scheduling is legitimately slow (autoscaled GPU nodes), raise `spec.pendingPodDeadline` on the RunnerGroup (or the matching `runnerGroups[]` entry of the `ActionsGateway` CR) above the worst-case node-provisioning time, e.g. `pendingPodDeadline: "30m"`.
- Re-run the cancelled workflow from the GitHub UI once the cause is fixed.

---

## Proxy Pool Not Scaling

**Symptoms.** The HPA for the proxy pool shows `TARGETS: <unknown>/60%` and the replica count does not increase under load.

**Likely cause.** `resources.requests.cpu` is unset or zero for proxy pods. The Kubernetes Horizontal Pod Autoscaler (HPA) computes CPU utilization as `(current_cpu_usage / requested_cpu)`. If `requests.cpu` is zero, the denominator is undefined and the HPA emits `<unknown>` for the target metric and stops scaling entirely.

**Diagnostics.**

```sh
# Check HPA status
kubectl describe hpa -n <namespace> actions-gateway-proxy

# Check proxy pod resource requests
kubectl get pod -n <namespace> -l app=actions-gateway-proxy -o jsonpath='{.items[0].spec.containers[0].resources}'

# Check metrics-server is running
kubectl get pods -n kube-system -l k8s-app=metrics-server
```

**Resolution.**

Ensure `spec.proxy.resources.requests.cpu` is set to a non-zero value in the `ActionsGateway` spec. The default is `10m`. If you explicitly set `resources` without including `requests.cpu`, the whole `resources` block is replaced and defaults are lost — set all four sub-fields explicitly:

```yaml
proxy:
  resources:
    requests:
      cpu: "10m"
      memory: "32Mi"
    limits:
      cpu: "100m"
      memory: "64Mi"
```

After updating the spec, patch the proxy Deployment or trigger a rollout; the HPA will start computing utilization on the next metrics scrape cycle (~30s).

---

## Proxy Tunnel Closed Mid-Stream — Idle or Lifetime Cap

**Symptoms.** A worker job logs a connection reset, `EOF`, or `broken pipe` from the GitHub SDK / `curl` / `git`, with no proxy `502` response. The proxy pod itself is healthy and serving other tunnels.

**Likely cause.** The proxy enforces two per-tunnel deadlines on the CONNECT relay (M-18, 2026-05-31):

- **Idle timeout** — 5 minutes of no data in either direction. A long-poll against the GitHub API or a stalled SDK call hits this first.
- **Hard lifetime cap** — 6 hours absolute, regardless of activity. A continuous artifact stream or Twirp log relay that exceeds this is torn down even with traffic flowing.

These are not bugs. They bound goroutine and file-descriptor exhaustion from slow or stuck clients. The healthy case (an actively-used GitHub API call) completes in seconds and does not trip either cap.

**Diagnostics.**

The proxy serves `/metrics` over mutual TLS on `:8443` (not `:8081`, which now
carries only the plaintext `/healthz` + `/readyz` probes). Scraping requires the
per-tenant scraper client certificate the GMC publishes — see
[Metrics scrape returns a TLS / connection error](#metrics-scrape-returns-a-tls--connection-error)
for how to fetch the bundle. With the bundle written to `scraper.crt` /
`scraper.key` / `metrics-ca.crt`:

```sh
ns=<namespace>
# Distribution of tunnel lifetimes; a heavy tail near 21600s (6h) or
# a spike at 300s (5m idle) indicates clients hitting the caps.
curl -s --cert scraper.crt --key scraper.key --cacert metrics-ca.crt \
  "https://actions-gateway-proxy.$ns.svc:8443/metrics" | \
  grep actions_gateway_proxy_tunnel_duration_seconds_bucket

# Active vs. total tunnels — healthy ratio is "active << total".
curl -s --cert scraper.crt --key scraper.key --cacert metrics-ca.crt \
  "https://actions-gateway-proxy.$ns.svc:8443/metrics" | \
  grep -E 'actions_gateway_proxy_connections_(active|total)'
```

**Resolution.**

For idle hits: examine the workflow step that stalls. A workflow `sleep`-ing inside a long-running `curl --connect-timeout 0` or a misconfigured webhook receiver are typical causes. The fix is usually in the workflow, not the proxy.

For lifetime-cap hits: split very long-running uploads or streams across multiple HTTP requests. The 6h cap is a safety net for stuck connections; a legitimately-long single stream should be rare.

To change the defaults during an incident, patch the proxy Deployment with environment overrides — note that there is no env-var knob today; defaults are baked into the Server struct and require a code change to adjust. File a Queue item if a tenant repeatedly hits either cap on a legitimate workload.

---

## Metrics scrape returns a TLS / connection error

**Symptoms.** Prometheus (or a manual `curl`) of a per-tenant proxy or AGC
`/metrics` endpoint fails with one of:

- `remote error: tls: certificate required` / `bad certificate` — no client cert, or one signed by the wrong CA.
- `connection refused` on `:8081/metrics` — the metrics endpoint moved to `:8443` (mTLS); `:8081` now serves only `/healthz` + `/readyz`.
- `context deadline exceeded` / no route — the scraper namespace is not labelled `metrics: enabled`, so the NetworkPolicy drops the connection before the handshake.

**Cause.** The proxy and AGC serve `/metrics` over **mutual TLS** on `:8443`
(Q69). A scraper must (1) connect from a namespace labelled `metrics: enabled`
and (2) present a client certificate signed by the per-tenant metrics CA the GMC
issues. Both halves are required.

**Resolution.**

1. Label the monitoring namespace so the NetworkPolicy admits it:
   ```sh
   kubectl label namespace <prometheus-namespace> metrics=enabled
   ```
2. Fetch the scraper client bundle the GMC publishes in each tenant namespace and
   point the scrape at `:8443` with `scheme: https`:
   ```sh
   ns=<tenant-namespace>
   kubectl get secret actions-gateway-metrics-client -n "$ns" -o jsonpath='{.data.tls\.crt}' | base64 -d > scraper.crt
   kubectl get secret actions-gateway-metrics-client -n "$ns" -o jsonpath='{.data.tls\.key}' | base64 -d > scraper.key
   kubectl get secret actions-gateway-metrics-client -n "$ns" -o jsonpath='{.data.ca\.crt}'  | base64 -d > metrics-ca.crt
   curl -s --cert scraper.crt --key scraper.key --cacert metrics-ca.crt \
     "https://actions-gateway-proxy.$ns.svc:8443/metrics" | head
   ```
   Delete the extracted key file when finished. For a `ServiceMonitor`, mount the
   bundle and reference it from `tlsConfig` (`cert`/`keySecret`/`ca`).
3. If the cert is rejected after a CA rotation, the GMC re-issues the whole
   bundle ~30 days before expiry but pods read certs at startup — restart the
   proxy/AGC pods (and re-fetch the client bundle) after a rotation.

---

## RateLimited Condition on ActionsGateway

**Symptoms.** `kubectl get actionsgateway` shows a `RateLimited=True` condition. `actions_gateway_active_sessions` is at or near the per-installation budget.

**Likely cause.** The GitHub App installation's API budget (15,000 `GET /message` requests/hour) is exhausted. This occurs when the sum of `maxListeners` across all RunnerGroups simultaneously bursts to their ceiling for a sustained period.

**SLO threshold.** A `RateLimited` condition lasting more than 1 minute during non-peak hours indicates the installation is over budget. Durations exceeding 10 minutes during business hours should page on-call.

**Diagnostics.**

```sh
# Check the condition
kubectl get actionsgateway -n <namespace> <name> -o jsonpath='{.status.conditions}' | jq .

# Check active sessions vs. budget
# Budget: ~208 sessions (15000/hr ÷ 72 polls/session/hr)
# Metric: actions_gateway_active_sessions{namespace="<namespace>"}

# Check per-RunnerGroup maxListeners sum
kubectl get runnergroup -n <namespace> -o jsonpath='{.items[*].spec.maxListeners}'
```

**Resolution.**
- If a burst is temporary and below 10 minutes: no action required, the condition will clear as the burst subsides.
- If `maxListeners` values are set higher than needed, reduce them.
- If the tenant's RunnerGroup count × `maxListeners` sustainably exceeds the installation budget, shard to a second `ActionsGateway` CR with a new GitHub App installation. See [Appendix E §E.6](../design/appendix-e-capacity-planning.md#e6-when-to-shard-across-installations).

---

## GitHub App Secret Misconfiguration

**Symptoms.** AGC logs show errors like `failed to create installation token`, `private key: RSA key parse error`, or `401 Unauthorized`. The `ActionsGateway` condition `AGCAvailable=False` with reason `CredentialError`.

**Common misconfigurations.**

| Error message | Likely cause |
| --- | --- |
| `private key: RSA key parse error` | PEM key has extra whitespace, missing newline, or wrong format (PKCS#8 instead of RSA PKCS#1). |
| `401 Unauthorized` on token exchange | `appId` or `installationId` is wrong. |
| `404 Not Found` on token exchange | The GitHub App is not installed in the target organization or the `installationId` does not match. |
| `422 Unprocessable Entity` | The App lacks the `Actions: Read` and `Administration: Read` permissions. |

**Diagnostics.**

```sh
# Check Secret keys exist and are non-empty
kubectl get secret -n <namespace> <name> -o jsonpath='{.data.appId}' | base64 -d
kubectl get secret -n <namespace> <name> -o jsonpath='{.data.installationId}' | base64 -d
kubectl get secret -n <namespace> <name> -o jsonpath='{.data.privateKey}' | base64 -d | head -1
# Expected first line: -----BEGIN RSA PRIVATE KEY-----

# Verify the App ID and installation ID match the GitHub App
# GitHub UI: Settings → Developer settings → GitHub Apps → <app> → General (App ID)
# GitHub UI: Settings → Developer settings → GitHub Apps → <app> → Install App (Installation ID in URL)
```

**Resolution.** Re-create the Secret with correct values. To trigger a rolling update on the AGC Deployment after fixing the Secret, change `gitHubAppRef.name` in the `ActionsGateway` spec to reference the new Secret name (the GMC will roll the AGC Deployment automatically) or manually restart the Deployment:

```sh
kubectl rollout restart deploy/actions-gateway-controller -n <namespace>
```

See [Getting Started — Rotating GitHub App Credentials](../getting-started.md#rotating-github-app-credentials) for the full rotation procedure.

---

## Token Refresh Errors Spiking

**Symptoms.** `actions_gateway_token_refresh_errors_total` is increasing. GitHub App installation tokens expire after one hour; if refresh fails, new sessions cannot be established once the token expires.

**Likely causes.**
- GitHub API is temporarily unavailable or returning 5xx errors.
- The GitHub App private key was rotated but the Secret was not updated.
- Network path from AGC to GitHub API is down (proxy pool issue).

**Diagnostics.**

```sh
# Check the error rate
# Metric: rate(actions_gateway_token_refresh_errors_total[5m])

# Check AGC logs for the error detail
kubectl logs -n <namespace> deploy/actions-gateway-controller | grep "token refresh"

# Test connectivity to GitHub via the tenant proxy (AGC is distroless — use an
# ephemeral curl pod in the same namespace; it picks up the same NetworkPolicy egress).
kubectl run nettest-$$ -n <namespace> --rm -it --restart=Never \
  --image=curlimages/curl:latest \
  --overrides='{"spec":{"automountServiceAccountToken":false,"containers":[{"name":"c","image":"curlimages/curl:latest","command":["sh","-c","curl -x https://actions-gateway-proxy:8080 -sI https://api.github.com/app"]}]}}'
```

**Resolution.**
- If GitHub is temporarily unavailable: the AGC's exponential back-off retry (5s → 60s cap) will recover automatically. Monitor until the error rate returns to zero.
- If the private key was rotated: update the Secret. See [Getting Started — Rotating GitHub App Credentials](../getting-started.md#rotating-github-app-credentials).
- If the proxy is unreachable: see [Proxy Pool Not Scaling](#proxy-pool-not-scaling) and the network connectivity section below.

**SLO.** Token refresh errors should stay below 1 per hour per tenant. Above this rate, begin investigating immediately. In-flight sessions will fail at the next reconnection once the token expires (~1 hour).

---

## RenewJob Failures Rising

**Symptoms.** `actions_gateway_renewjob_errors_total` is increasing. Jobs may start being cancelled by GitHub before completion.

**Likely causes.**
- Network connectivity issues between the AGC and GitHub (via proxy).
- GitHub API is temporarily unavailable.
- The runner job lock window expired before the renewer could refresh (AGC was slow or restarting).

**Diagnostics.**

```sh
# Check recent error rate
# Metric: rate(actions_gateway_renewjob_errors_total[5m])

# Check AGC logs for renewal errors and job IDs
kubectl logs -n <namespace> deploy/actions-gateway-controller | grep "renewjob"

# Confirm the proxy pool is healthy
kubectl get pods -n <namespace> -l app=actions-gateway-proxy
```

**Resolution.**
- Transient GitHub API errors: the renewer retries; monitor until the rate returns to zero.
- Proxy pool unhealthy: fix the proxy pool (see [Proxy Pool Not Scaling](#proxy-pool-not-scaling)).
- If the AGC restarted mid-job: jobs whose lock expired will have been cancelled by GitHub. These require manual re-run. Check `actions_gateway_eviction_retries_exhausted_total` for any jobs that were also evicted.

Each `renewjob` error is a warning, not an immediate job failure — GitHub grants ~10 minutes per renewal window. A single transient error on a long-running job is rarely fatal.

---

## Sessions Stuck in 401/EOF GetMessage Loops (Tenant Throughput Decays to Zero)

**Symptoms.** On gateway versions without the Q114 self-heal (≤ the M4 validation build):
- AGC logs fill with repeating `GetMessage error ... decode response: EOF` and later `broker: unauthorized (HTTP 401)` lines for the same session, backing off forever.
- The repo/org runner list (`gh api .../actions/runners`) loses one runner after each completed job, and the registrations never come back.
- `RunnerGroup` `status.activeSessions` decays over time; after roughly `maxListeners` completed jobs, queued workflow jobs wait forever even though the AGC pod is healthy.

**Cause.** GitHub deletes a JIT-registered runner record once it acquires a job (single-use runners). Pre-fix AGC versions keep polling the dead session with the dead agent's credentials instead of re-registering, so every completed job permanently burns one listener slot ([M4 §12, bug 2](../plan/milestone-4.md#12-live-multi-tenant-validation-evidence-2026-06-1112)).

**Diagnostics.**

```sh
# Repeating EOF/401 poll errors
kubectl logs -n <namespace> deploy/actions-gateway-controller | grep -E "decode response: EOF|unauthorized"

# Listener slots remaining
kubectl get runnergroup -n <namespace> -o jsonpath='{.items[*].status.activeSessions}'

# On fixed versions, recycles should be happening instead:
# Metric: rate(actions_gateway_agent_recycles_total[15m])  — roughly tracks job completions
# Metric: actions_gateway_agent_recycle_errors_total       — should stay flat
```

**Resolution.**
- **Upgrade** to a gateway version with the Q114 self-heal. Fixed versions re-register each agent after every job (`actions_gateway_agent_recycles_total{trigger="post_job"}`) and heal stale sessions discovered after a restart (`trigger="stale_session"` / `"startup"`); no per-job operator action is needed.
- **Interim manual recovery on pre-fix versions:** delete the RunnerGroup's agent Secrets and restart the AGC so it registers a fresh pool:

  ```sh
  kubectl delete secret -n <namespace> -l actions-gateway/runner-group=<group>
  kubectl rollout restart deploy/actions-gateway-controller -n <namespace>
  ```

  Expect `409 Already exists` registration errors for any agent that never ran a job — its record survives server-side under an ID the AGC no longer knows. Delete the survivor from GitHub first: find its ID with `gh api '.../actions/runners?name=<group>-<index>'`, then `gh api -X DELETE .../actions/runners/<id>`. Fixed versions resolve this 409 automatically.

**On fixed versions,** a sustained rise of `actions_gateway_agent_recycle_errors_total` means the AGC cannot re-register agents (registration API unreachable, installation token failures, or revoked GitHub App runner-administration permission) — listener capacity shrinks until recycles succeed. Check AGC logs for `recycle` errors and verify the App's runner permissions.

---

## Network Connectivity Failures

**Symptoms.** The AGC cannot reach GitHub through the proxy. Logs show `connection refused`, `dial tcp: i/o timeout`, or `proxy: no response from proxy`.

**Likely causes.**
- The proxy pod is not running or not ready.
- `HTTP_PROXY`/`HTTPS_PROXY` environment variables are incorrect (wrong Service name or port).
- `actions-gateway-workload` NetworkPolicy is blocking the AGC-to-proxy egress path (e.g. proxy ClusterIP changed after a recreate and the rule wasn't reconciled).
- `actions-gateway-proxy` NetworkPolicy is blocking the proxy's egress to GitHub (IP ranges stale or `managedNetworkPolicy: false` with no replacement rule).
- `actions-gateway-controller` NetworkPolicy is missing — AGC can't reach the K8s API server, so token refresh and webhook health checks fail before any GitHub traffic.

**Diagnostics.**

```sh
# Check proxy pod status
kubectl get pods -n <namespace> -l app=actions-gateway-proxy

# Verify the proxy Service exists and has endpoints
kubectl get svc -n <namespace> actions-gateway-proxy
kubectl get endpoints -n <namespace> actions-gateway-proxy

# Check the AGC container's HTTPS_PROXY env var (distroless — inspect spec, not the running process)
kubectl get pod -n <namespace> -l app=actions-gateway-controller \
  -o jsonpath='{range .items[0].spec.containers[?(@.name=="agc")].env[?(@.name=="HTTPS_PROXY")]}{.name}={.value}{"\n"}{end}'

# Test proxy connectivity using an ephemeral curl pod in the same namespace
kubectl run nettest-$$ -n <namespace> --rm -it --restart=Never \
  --image=curlimages/curl:latest \
  --overrides='{"spec":{"automountServiceAccountToken":false,"containers":[{"name":"c","image":"curlimages/curl:latest","command":["sh","-c","curl -v -x https://actions-gateway-proxy:8080 https://api.github.com 2>&1 | head -20"]}]}}'

# Check NetworkPolicy rules — there are three: workload, agc, proxy
kubectl get networkpolicy -n <namespace>
# Expected: actions-gateway-workload, actions-gateway-controller, actions-gateway-proxy
kubectl describe networkpolicy -n <namespace>

# Check the IP range refresh metric
# Metric: actions_gateway_ip_range_updates_total{namespace="<namespace>"}
```

**Resolution.**
- If the proxy pod is down: check its logs and restart if necessary.
- If the `NetworkPolicy` egress rules are stale: trigger a manual refresh by temporarily setting `spec.proxy.managedNetworkPolicy: false` and back to `true`, or wait for the 24-hour automatic refresh cycle. Check the GitHub meta API for current IP ranges: `curl https://api.github.com/meta | jq .actions`.
- If the `NO_PROXY` list is missing the cluster service CIDR: update `spec.proxy.noProxyCIDRs` to include your cluster's service CIDR (see the `noProxyCIDRs` field documentation in [§3.1](../design/03-api-contracts.md#31-kubernetes-crd-schemas)).

---

## AGC Cannot Reach the Kubernetes API Server (NetworkPolicy + post-DNAT port mismatch)

**Symptoms.** AGC logs show `dial tcp 10.96.0.1:443: i/o timeout` (or similar) when calling the K8s API server. The `actions-gateway-controller` NetworkPolicy *appears* to allow port 443, yet the connection is silently dropped. Most often surfaces in kind, but possible on any cluster where the `kubernetes` Service backends listen on a port other than 443.

**Cause.** NetworkPolicy enforcement evaluates packets *after* kube-proxy's DNAT. When a pod connects to `kubernetes.default.svc` (ClusterIP `10.96.0.1:443`), kube-proxy DNATs the destination to the apiserver's actual Endpoints address — in kind, that's `<node-ip>:6443`. The policy controller sees the post-DNAT destination port (6443), and an NP rule that allows only port 443 doesn't match. This is the port-axis equivalent of the `ipBlock: <ClusterIP>/32` trap that bit the proxy NP in PR #59.

**Diagnostics.**

```sh
# 1. Confirm the apiserver Endpoints port. If it's 6443, the AGC NP must allow 6443.
kubectl get endpointslice -n default -l kubernetes.io/service-name=kubernetes \
  -o jsonpath='{.items[0].ports[0].port}{"\n"}'

# 2. Confirm the AGC NetworkPolicy actually allows both 443 and 6443.
kubectl get networkpolicy -n <namespace> actions-gateway-controller -o yaml \
  | yq '.spec.egress[].ports[].port' | sort -u

# 3. If the cluster uses kindnet / kube-network-policies, check the verdict log
#    on the node hosting the AGC pod. Look for lines like:
#      "Pod is not allowed to connect to port" pod="<ns>/<agc-pod>" port=6443
kubectl get pod -n <namespace> -l app=actions-gateway-controller \
  -o jsonpath='{.items[0].spec.nodeName}{"\n"}'
kubectl logs -n kube-system -l app=kindnet --tail=200 --field-selector spec.nodeName=<node-name>
```

**Resolution.** Ensure `buildAGCNetworkPolicy` allows both port 443 (production Service shape) *and* port 6443 (kind / Endpoints-on-6443 clusters). The shipped policy does this. If you see this on a custom build or a hand-edited NP, add the 6443 rule. The diagnosis writeup at [`docs/development/networkpolicy-port-matching.md`](../development/networkpolicy-port-matching.md) has a minimal repro and the reasoning behind allowing both ports.

If you see the same symptom for an *ingress*-type rule or for a different Service whose backend port differs from the Service port, the same fix applies: list both ports, or omit the port restriction on that rule.

---

## Worker Pod Runner.Worker Fails TLS Handshake With UntrustedRoot

**Symptoms.** Worker pod logs (look at the `runner` container) contain repeated lines like:

```
System.Security.Authentication.AuthenticationException: The remote certificate is invalid because of errors in the certificate chain: UntrustedRoot
```

emitted from `JobExtension` connectivity checks, `ResultServer` init, `JobServerQueue` log uploads, the `GitHubActionsService` log-blob fetch, or `RunServer.CompleteJobAsync`. The runner retries for ~3 minutes, then exits 1. The AGC then logs `worker pod completed phase=Failed`, `renewjob` starts returning `401 Not authorized for this job`, and the GitHub workflow concludes `cancelled` even though the actual job steps may have run.

**Cause.** Runner.Worker's .NET HttpClient is validating the egress proxy's TLS cert and the worker pod's trust store does not include the cert-manager-issued self-signed CA that signed it. This is the worker-side mirror of the AGC's proxy-CA pinning ([§5.2](../design/05-security.md) "Cross-Tenant Proxy CA Trust"): the AGC mounts the CA explicitly so its outbound HTTPS works; worker pods must do the same.

The AGC's pod provisioner is supposed to project the per-tenant `actions-gateway-proxy-tls` Secret into every worker pod at `/etc/actions-gateway/proxy-ca/tls.crt` and set `PROXY_CA_CERT_PATH` so the worker entrypoint wrapper builds a combined `SSL_CERT_FILE` bundle before exec'ing `Runner.Worker`. UntrustedRoot means one of those steps did not happen.

**Diagnostics.**

```sh
# 1. Inspect a failed worker pod's spec — the Secret volume must exist.
kubectl get pod -n <namespace> <worker-pod-name> -o yaml \
  | yq '.spec.volumes[] | select(.name=="proxy-ca")'
# Expected: a Secret volume with secretName: actions-gateway-proxy-tls and Items: [{key: tls.crt, path: tls.crt}]
# If empty: the AGC was deployed without PROXY_TLS_SECRET_NAME.

# 2. Confirm the AGC has the PROXY_TLS_SECRET_NAME env wired.
kubectl get pod -n <namespace> -l app=actions-gateway-controller \
  -o jsonpath='{range .items[0].spec.containers[?(@.name=="agc")].env[?(@.name=="PROXY_TLS_SECRET_NAME")]}{.name}={.value}{"\n"}{end}'
# Expected: PROXY_TLS_SECRET_NAME=actions-gateway-proxy-tls
# Empty means the GMC needs to roll the AGC Deployment (likely an upgrade across the 5h boundary).

# 3. Confirm the worker container's PROXY_CA_CERT_PATH env.
kubectl get pod -n <namespace> <worker-pod-name> -o yaml \
  | yq '.spec.containers[] | select(.name=="runner") | .env[] | select(.name=="PROXY_CA_CERT_PATH")'

# 4. Confirm the proxy TLS Secret exists and contains tls.crt.
kubectl get secret -n <namespace> actions-gateway-proxy-tls \
  -o jsonpath='{.data.tls\.crt}' | base64 -d | openssl x509 -noout -subject -issuer -dates
```

**Resolution.**
- If the worker pod has no `proxy-ca` volume: ensure the AGC was started with `PROXY_TLS_SECRET_NAME=actions-gateway-proxy-tls` (the GMC injects this automatically — if it's missing, the GMC needs to roll the AGC Deployment, e.g. by bumping `ag.Spec` or restarting the GMC).
- If the volume is present but the wrapper logs nothing about `proxy CA trust installed`: check that `PROXY_CA_CERT_PATH` is set on the runner container and the mounted file is non-empty. An empty/missing file is tolerated as a no-op, which silently leaves the runner with no proxy trust — the wrapper log line `no proxy CA cert mounted; skipping trust-store install` distinguishes this case from a wrapper that ran the install successfully.
- If the proxy TLS Secret is missing or the cert has expired: the GMC's cert-manager integration ([§2.1](../design/02-architecture.md#21-tier-1--gateway-manager-controller-gmc) "Proxy Deployer") owns rotation; check the GMC's logs for issuer errors. As a fallback, deleting the Secret triggers reissuance.
- If the issue persists after the volume and env are correct: confirm the proxy pod is presenting the cert signed by the CA in the Secret — `kubectl exec` into a curl pod in the same namespace and run `openssl s_client -connect actions-gateway-proxy:8080 -showcerts </dev/null` to inspect what the proxy actually serves.

---

## Evicted Worker Pods Exhausting Retry Budget

**Symptoms.** `actions_gateway_eviction_retries_exhausted_total` is incrementing. Jobs are being cancelled after eviction despite automatic retries.

**Likely causes.**
- Worker pod keeps being evicted on every attempt (persistent node pressure, OOM loop, or scheduling conflict that prevents the pod from completing).
- `maxEvictionRetries` is set too low for a workload that occasionally experiences preemption.

**Diagnostics.**

```sh
# Check eviction retry metrics
# actions_gateway_eviction_retries_total{namespace, runner_group}
# actions_gateway_eviction_retries_exhausted_total{namespace, runner_group}

# Check recent evicted pods
kubectl get pods -n <namespace> --field-selector=status.phase=Failed | grep Evicted

# Describe an evicted pod for the eviction reason
kubectl describe pod -n <namespace> <evicted-pod-name>
# Look for: "The node was low on resource: memory" or "Preempted by another pod"

# Check node events around the eviction time
kubectl get events -n <namespace> --sort-by='.lastTimestamp' | grep -i evict
```

**Resolution.**
- If evictions are due to node memory pressure: increase the worker pod's memory requests to discourage the kubelet from evicting it, or investigate the workload's actual memory usage.
- If evictions are from preemption by higher-priority pods: reduce the priority of competing workloads or adjust `priorityTiers` to give this RunnerGroup a higher floor.
- If the retry budget is too low for a workload that occasionally gets preempted: increase `maxEvictionRetries` on the RunnerGroup spec (default 2, max 10).
- If the workload is consistently failing (OOM crash, not preemption): the auto-retry is not appropriate. Set `maxEvictionRetries: 0` and investigate the underlying workload issue.

---

## Jobs Failing Due to Namespace ResourceQuota Exhaustion

**Symptoms.** `actions_gateway_quota_retries_exhausted_total` is incrementing. Pod creation fails with a `Forbidden` error containing "exceeded quota" in the AGC logs. Jobs are being abandoned before a pod is ever scheduled.

**Likely causes.**
- The namespace ResourceQuota `pods` or `requests.cpu`/`requests.memory` limit is too low for the burst of concurrent jobs arriving.
- A long-running job is holding quota that a new job needs; quota will clear once it completes.
- The quota retry budget (`maxQuotaRetries`, default 5) is exhausting before quota clears.

**Diagnostics.**

```sh
# Check quota retry metrics
# actions_gateway_quota_retries_total{namespace, runner_group}
# actions_gateway_quota_retries_exhausted_total{namespace, runner_group}

# Inspect current quota usage
kubectl describe resourcequota -n <namespace>

# Check AGC logs for quota errors
kubectl logs -n <agc-namespace> deploy/actions-gateway-controller | grep "exceeded quota"
```

**Resolution.**
- If quota is consistently full: increase the namespace `ResourceQuota` limits or reduce `maxWorkers` / `priorityTiers` thresholds so the AGC holds fewer concurrent pods.
- If quota clears quickly but the retry window is too short: increase `maxQuotaRetries` or `quotaRetryDelay` on the RunnerGroup spec (defaults: 5 retries / 30s delay).
- If quota retry is causing unwanted job-lock hold time: set `maxQuotaRetries: 0` to fail immediately on quota exhaustion — the job lock is dropped and GitHub redelivers the job.

---

## Worker Pod Fails to Start After Secure-by-Default SecurityContext

**Symptoms.** A worker pod that previously ran now stays in `CreateContainerConfigError` or `Pending`, or is rejected at admission. `kubectl describe pod` shows one of:
- `Error: container has runAsNonRoot and image has non-numeric user (<name>), cannot verify user is non-root` — the AGC stamped `runAsNonRoot: true` (every profile except `privileged`) and the image declares its user **by name**, which kubelet cannot verify against a numeric UID. The **default** `ghcr.io/actions/actions-runner` image (`USER runner`) is handled automatically — the AGC gap-fills `runAsUser: 1001` so kubelet can verify it (Q115). You hit this only with a **custom/third-party** runner image whose named user is **not** UID 1001, so the auto-stamped 1001 doesn't match what its `USER` resolves to, or whose image has no numeric UID at all.
- `Error: container has runAsNonRoot and image will run as root` — same stamp, but the worker image's default user is `root` (UID 0).
- A PodSecurity admission denial naming `allowPrivilegeEscalation != false` or `unrestricted capabilities` — the namespace is on `securityProfile: restricted` and a tenant container needs `sudo` or extra capabilities.

**Likely causes.**
- A **custom** worker image declares a **named** (non-numeric) user other than the default runner's UID 1001. The AGC's secure-by-default gap-fill stamps `runAsUser: 1001` to match the upstream `actions-runner` image; an image whose user is a different UID still needs its own numeric UID declared. (The default `actions-runner` image needs no action.)
- The worker image runs as root by default (common for custom or third-party runner images). The AGC's secure-by-default `runAsNonRoot: true` then blocks it.
- A job under `restricted` calls `sudo` or installs packages requiring capabilities the PSA-restricted floor drops.

**Diagnostics.**

```sh
# See the exact rejection / config error
kubectl describe pod -n <tenant-namespace> <pod> | sed -n '/Events:/,$p'

# Confirm the namespace's enforced PSA profile
kubectl get ns <tenant-namespace> -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}'; echo

# Confirm the SECURITY_PROFILE the AGC is running with
kubectl get deploy actions-gateway-controller -n <tenant-namespace> -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="SECURITY_PROFILE")].value}'; echo
```

**Resolution.**
- Default `actions-runner` image: **no action needed** — the AGC gap-fills `runAsUser: 1001` automatically (Q115).
- Custom named-user image whose user is **not** UID 1001: declare its actual numeric UID in the RunnerGroup `podTemplate` so kubelet can verify non-root (an explicit `runAsUser` overrides the gap-filled 1001):

  ```yaml
  podTemplate:
    spec:
      securityContext:
        runAsUser: <image-uid>
        runAsGroup: <image-gid>
  ```

  Note: a `podTemplate` change does not reach already-running listeners
  ([Q117](../STATUS.md)) — restart the AGC pod (`kubectl delete pod -n
  <tenant-namespace> -l app=actions-gateway-controller`) after applying it.
- Root-based image that must run as root: the defaults are gap-fill only — set an explicit `securityContext.runAsNonRoot: false` (and `runAsUser`/`runAsGroup` as needed) on the runner container in the RunnerGroup `podTemplate`. No profile escalation is required for `baseline`.
- Job genuinely needs `sudo`/capabilities: move that workload to a `baseline` `ActionsGateway` (the default), which does not stamp the privilege-escalation/capability floor. Reserve `restricted` for workloads that can run without them.
- Workload needs a real privileged container (DinD, kernel modules): set `securityProfile: privileged` on the `ActionsGateway` and pair it with a sandbox runtime — see [§5.3](../design/05-security.md#53-security-profiles-and-the-privileged-opt-in).

---

## securityProfile Downgrade Rejected by Admission Webhook

**Symptoms.** A `kubectl apply` / `kubectl edit` / GitOps sync that changes an existing `ActionsGateway`'s `spec.securityProfile` to a *less restrictive* level is rejected by the GMC validating webhook with:

```
admission webhook "vactionsgateway-v1alpha1.kb.io" denied the request:
securityProfile downgrade from "restricted" to "baseline" is not permitted
without the "actions-gateway.github.com/allow-profile-downgrade" annotation
set to "true"; downgrading relaxes Pod Security Admission isolation and must
be deliberate
```

The profiles rank `privileged` (least restrictive) < `baseline` < `restricted` (most restrictive); any move *down* that ranking is a downgrade — including `baseline → privileged`.

**Likely causes.**
- A deliberate relaxation — e.g. rolling back a `baseline → restricted` hardening attempt that broke the tenant's pods at PSA admission.
- **Unintentional drift:** re-applying an older manifest, or a Helm/Kustomize render that **omits** `securityProfile` (it then re-defaults to `baseline`) while the live object is on `restricted`. An empty/absent value is compared as `baseline`, so an omitted field reads as a downgrade — this is the guard working as intended, catching a silent weakening.

**Diagnostics.**

```sh
# Current (live) profile vs what your manifest sets
kubectl get actionsgateway -n <tenant-namespace> <name> -o jsonpath='{.spec.securityProfile}'; echo
```

**Resolution.**
- **If the downgrade is intended:** add the opt-in annotation, then change the profile (one apply works if both are in the manifest):
  ```sh
  kubectl annotate actionsgateway -n <tenant-namespace> <name> \
    actions-gateway.github.com/allow-profile-downgrade=true --overwrite
  ```
  Remove the annotation afterward if you want future accidental downgrades to keep being blocked. PSA enforce is namespace-scoped, so the new (looser) profile applies to *future* worker pods once the GMC re-stamps the namespace label; pods already running are not re-evaluated.
- **If the downgrade is accidental (drift):** do **not** add the annotation — fix the manifest to match the live profile (set `securityProfile: restricted`, or stop omitting it) so GitOps stops trying to weaken the namespace.

> Note: this guard catches *silent* downgrades; it is not an absolute boundary. Anyone with edit access to the CR can set the annotation, and an operator with direct namespace `patch` rights can change the PSA labels regardless. See [§5.3 — No silent profile downgrades](../design/05-security.md#no-silent-profile-downgrades).

---

## Tracing Sampler Rejected by Admission

**Symptoms.** A `kubectl apply` / `kubectl edit` / GitOps sync that sets `spec.tracing.sampler` on an `ActionsGateway` is rejected at admission with a CRD validation error like:

```
ActionsGateway.actions-gateway.github.com "<name>" is invalid:
spec.tracing.sampler: Unsupported value: "ratio": supported values:
"always_on", "always_off", "traceidratio", "parentbased_always_on",
"parentbased_always_off", "parentbased_traceidratio"
```

**Likely cause.** `spec.tracing.sampler` is a fixed enum mapping to the OpenTelemetry SDK's built-in samplers (it is forwarded verbatim as `OTEL_TRACES_SAMPLER`). A value outside that set — a typo, or one of the SDK's externally-configured samplers (`jaeger_remote`, `xray`) that this field intentionally does not expose — is rejected by the CRD schema before the object is stored.

**Resolution.**
- Pick a supported value. For probabilistic sampling use `parentbased_traceidratio` with `spec.tracing.samplerArg: "0.1"` (10%); for all/no traces use `parentbased_always_on` / `always_off`.
- Leave `sampler` unset to use the SDK default (`parentbased_always_on`).
- To *disable* tracing entirely, remove `spec.tracing.endpoint` (an empty endpoint emits no `OTEL_*` env and the AGC keeps its no-op tracer) — the sampler value is irrelevant when no endpoint is set.

See [observability — enabling tracing on GMC-managed AGCs](observability.md#enabling-tracing-on-gmc-managed-agcs) for the full field list.

---

## Worker-Pod Lifecycle Duration Rejected by Admission

**Symptoms.** Applying a `RunnerGroup` (or an `ActionsGateway` whose `runnerGroups[]` entry sets the field) is rejected at admission with one of:

```
The RunnerGroup "..." is invalid: spec: Invalid value: ...:
completedPodTTL must not be negative
```

```
The RunnerGroup "..." is invalid: spec: Invalid value: ...:
pendingPodDeadline must be at least 1s
```

**Likely cause.** CRD CEL validation on the two worker-pod lifecycle knobs: `completedPodTTL` accepts any non-negative duration (`"0s"` means delete worker pods immediately on completion), while `pendingPodDeadline` must be at least `1s` — a zero deadline would reap every worker pod the instant it was admitted, and there is deliberately no way to disable the deadline (an unbounded Pending pod permanently leaks a concurrency-ceiling slot).

**Resolution.**
- Use a non-negative Go duration string for `completedPodTTL` (`"0s"`, `"5m"`, `"1h"`).
- Use a duration of `1s` or more for `pendingPodDeadline`; to effectively park the deadline while debugging a scheduling issue, set it large (e.g. `"24h"`) rather than zero.
- Omit either field to get the defaults (`5m` retention, `10m` deadline).

---

## Worker Pod Crashes With `configuredSettings` ArgumentNullException

**Symptoms.** Worker pod reaches `Running`, the entrypoint wrapper logs `payload loaded` and starts Runner.Worker, but Runner.Worker exits non-zero almost immediately with a stack trace containing `System.ArgumentNullException: Value cannot be null. (Parameter 'configuredSettings')` originating from `Runner.Common.ConfigurationStore.GetSettings()`. The job is never reported back to GitHub.

**Likely causes.**
- The agent Secret was created before Q5a shipped and is missing the `encodedJITConfig` key; the AGC reconciled forward but the per-job Secret has no `jitconfig` key for the wrapper to materialize.
- A custom registrar (non-GitHub) returns an `AgentCredentials` value without `EncodedJITConfig` populated.
- The runner home directory inside the worker container is not `/home/runner` (custom image), but `RUNNER_HOME_DIR` was not overridden in the pod template — the wrapper writes the files to the wrong location and Runner.Worker reads from `$HOME`.

**Diagnostics.**

```sh
# 1. Inspect the agent Secret. encodedJITConfig must be present and non-empty.
kubectl get secret -n <agc-namespace> -l actions-gateway/runner-group=<group>,actions-gateway/agent-index -o jsonpath='{.items[*].data.encodedJITConfig}' | base64 -d | head -c 32; echo

# 2. Inspect the per-job worker Secret while a job is in flight. The jitconfig
#    key must be present.
kubectl get secret -n <tenant-namespace> -l actions-gateway/runner-group=<group> -o name | grep '^secret/job-' | head -1 | xargs -I{} kubectl get {} -n <tenant-namespace> -o jsonpath='{.data.jitconfig}' | base64 -d | head -c 32; echo

# 3. Confirm the wrapper materialized the files. From a debug sidecar or by
#    exec'ing into a running worker pod, list /home/runner:
kubectl exec -n <tenant-namespace> <pod> -c runner -- ls -la /home/runner/.runner /home/runner/.credentials /home/runner/.credentials_rsaparams
```

**Resolution.**
- If the agent Secret is missing `encodedJITConfig`: scale the agent pool to zero (`maxListeners: 0` on the RunnerGroup), wait for Secrets to be deleted, then scale back up. New agents will be registered via `generate-jitconfig` and carry the blob. An agent whose session is in flight is not torn down mid-job — its Secret is deleted on a later reconcile once the session completes (the controller logs `skipping scale-down delete of in-use agent`), so wait for active jobs to drain before expecting the count to reach zero.
- If the worker image puts `$HOME` elsewhere: set `RUNNER_HOME_DIR` on the runner container env via the RunnerGroup `podTemplate`.
- If a custom registrar is in use: ensure it populates `AgentCredentials.EncodedJITConfig` with the raw blob from GitHub's response (the wrapper only knows how to decode that exact format).

---

## `kubectl apply ActionsGateway` Times Out On Webhook During GMC Rollout

**Symptoms.** Right after a GMC rolling update (image bump, env-var change, leader transition), the next `kubectl apply` of an `ActionsGateway` CR fails with:

```
Internal error occurred: failed calling webhook
"vactionsgateway-v1alpha1.kb.io": failed to call webhook:
Post "https://gmc-webhook-service.gmc-system.svc:443/...?timeout=10s":
context deadline exceeded
```

The webhook recovers seconds later; the same `kubectl apply` succeeds on retry. Common pattern in CI / e2e suites that change GMC env vars then immediately apply a CR.

**Likely causes.**
- Running a GMC image built before the readyz-gates-webhook fix landed (commit `0eaa30e`). The default Kubebuilder scaffold registers `mgr.AddReadyzCheck("readyz", healthz.Ping)`, which returns OK as soon as the manager process is up — *before* the webhook listener on port 9443 is bound. The new pod is briefly added to the `gmc-webhook-service` endpoints in a not-yet-serving state.
- A custom probe override that replaces `/readyz` with a cheap liveness check.

**Diagnostics.**

```sh
# 1. Probe the GMC's /readyz directly. With the fix, output should include
#    "[+]readyz ok" AND "[+]webhook ok". Without the fix, only "[+]readyz ok".
kubectl run dbg-readyz --image=alpine --rm -i --restart=Never --quiet --command -- \
  sh -c "apk add --no-cache curl >/dev/null 2>&1; \
         curl -s http://$(kubectl get pod -n gmc-system -l control-plane=controller-manager -o jsonpath='{.items[0].status.podIP}'):8081/readyz?verbose"

# 2. Confirm the deployment is rolling. If yes, wait for it to settle before
#    retrying apply.
kubectl rollout status deployment/gmc-controller-manager -n gmc-system --timeout=2m
```

**Resolution.**
- Upgrade the GMC image to one built from commit `0eaa30e` or later — the readyz check now waits for the webhook server's `StartedChecker()`.
- Until the upgrade is in place, retry the failing `kubectl apply` after 5–10 seconds.

---

## Worker `HTTPS_PROXY` Returns `connection refused` During Proxy Rollout

**Symptoms.** Worker pods (or `kubectl exec` debug curls from a workload-labeled pod) intermittently fail with `connect: connection refused` against the per-tenant proxy `:8080` immediately after a proxy `Deployment` rollout, scale-up, or HPA event. The proxy pods report `READY 1/1` and `/healthz` returns 200.

**Likely causes.**
- Running a proxy image built before the proxy `/readyz` gate landed. The pre-fix proxy bound the health server on `:8081` in parallel with the CONNECT server on `:8080`. The kubelet observed `/healthz` returning 200 and added the pod IP to the proxy `Service` EndpointSlice before the CONNECT serve goroutine had bound the kernel socket. Worker pods racing the rollout connected to the new pod IP via `Service` DNS and got `ECONNREFUSED`.
- A custom probe override that points the GMC-managed proxy `Deployment`'s readinessProbe at `/healthz` instead of `/readyz` (e.g. an out-of-band `kubectl edit deploy`). The GMC reconciler overwrites the probe back to `/readyz` on the next reconcile, but until then the regression is live.

**Diagnostics.**

```sh
# 1. Confirm the proxy Deployment's readinessProbe path. Should be /readyz.
kubectl get deploy -n <tenant-namespace> actions-gateway-proxy \
  -o jsonpath='{.spec.template.spec.containers[0].readinessProbe.httpGet.path}{"\n"}'

# 2. Probe /readyz directly from a workload-labeled debug pod (the proxy
#    NetworkPolicy denies ingress from unlabeled pods).
kubectl run dbg-readyz --rm -i --restart=Never --quiet \
  --labels='actions-gateway/component=workload' \
  --image=alpine --command -- \
  sh -c "apk add --no-cache curl >/dev/null 2>&1; \
         curl -sv http://actions-gateway-proxy.<tenant-namespace>.svc:8081/readyz"

# 3. From the same debug pod, confirm the CONNECT port accepts TCP. A 200 on
#    /readyz paired with a refused TCP dial would be a Q42 regression.
kubectl run dbg-connect --rm -i --restart=Never --quiet \
  --labels='actions-gateway/component=workload' \
  --image=alpine --command -- \
  sh -c "apk add --no-cache busybox-extras >/dev/null 2>&1; \
         nc -zv actions-gateway-proxy.<tenant-namespace>.svc 8080"
```

**Resolution.**
- Upgrade the proxy image to one built with the `/readyz` gate. The handler returns 503 until both listeners are bound (`cmd/proxy/proxy.go` — `handleReadyz`).
- If a custom override changed the readinessProbe path back to `/healthz`, remove it. GMC re-applies the canonical `Deployment` on its next reconcile, so the regression window closes within a few seconds.

`/healthz` remains the liveness probe (always 200 if the process is up). `/readyz` is the readiness gate — kubelet keeps the pod out of the Service EndpointSlice until both `:8080` and `:8081` are bound.

---

## Prometheus Not Scraping Proxy or AGC Metrics

**Symptom.** The proxy and AGC `/metrics` endpoints (both on `:8081`) return no
data in Prometheus, or scrape targets show as `down` with a connection
timeout/refused — despite the pods being healthy.

**Cause.** Each tenant namespace runs under a default-deny ingress posture. The
GMC's per-tenant NetworkPolicies admit `:8081` ingress *only* from namespaces
labelled `metrics: enabled` (the same convention the GMC's own
`allow-metrics-traffic` NetworkPolicy uses). If the namespace your Prometheus
runs in is not labelled, its scrapes are dropped. Kubelet liveness/readiness
probes are unaffected — they originate from the node, which every supported CNI
exempts from NetworkPolicy enforcement.

```bash
# 1. Confirm the monitoring namespace carries the scrape label.
kubectl get ns <prometheus-namespace> -o jsonpath='{.metadata.labels.metrics}{"\n"}'
# Expected: enabled

# 2. Inspect the per-tenant NP ingress rules — each should list an 8081 rule
#    whose `from` is a namespaceSelector on metrics=enabled.
kubectl get networkpolicy -n <tenant-namespace> \
  actions-gateway-proxy actions-gateway-controller \
  -o jsonpath='{range .items[*]}{.metadata.name}: {.spec.ingress}{"\n"}{end}'

# 3. Reproduce a scrape from a pod in the monitoring namespace (allowed) and an
#    unlabelled namespace (denied) to confirm the policy, not the listener.
kubectl run dbg-scrape --rm -i --restart=Never --quiet \
  -n <prometheus-namespace> --image=curlimages/curl --command -- \
  curl -sS --max-time 5 http://actions-gateway-proxy.<tenant-namespace>.svc:8081/metrics | head
```

**Resolution.**
- Label the namespace your Prometheus runs in: `kubectl label ns <prometheus-namespace> metrics=enabled`.
- The proxy and AGC `/metrics` endpoints are unauthenticated plain HTTP; the
  NetworkPolicy namespace selector is the only access control. Keep the
  `metrics: enabled` label off namespaces that should not see per-tenant
  traffic-volume metrics.

**Same label gates the GMC manager metrics.** Since the §E manifest-defaults
work, the GMC install ships the manager NetworkPolicy enabled by default, which
flips the controller-manager pod to default-deny ingress and admits `:8443`
`/metrics` only from `metrics: enabled` namespaces. If the GMC manager scrape
target is `down`, apply the same label to the Prometheus namespace:
`kubectl label ns <prometheus-namespace> metrics=enabled`. The validating-webhook
port (`9443`) is re-allowed from any source by design (the apiserver caller is
not a labeled pod), so admission is unaffected by this label.

---

## Proxy Replica Stuck Pending After Enabling HA Defaults

**Symptom.** One of the two proxy replicas is `Pending`; `kubectl describe pod`
shows `didn't match pod anti-affinity rules` / `node(s) didn't match pod
anti-affinity`. The proxy Deployment never reaches full availability.

**Cause.** The proxy pool uses **required** pod anti-affinity on
`kubernetes.io/hostname` so replicas land on distinct nodes (a single node
failure must never drop the whole tenant's egress pool, and co-located replicas
defeat the PodDisruptionBudget). With the default `proxy.minReplicas: 2`, the
scheduler needs **at least two schedulable nodes**. On a single-node dev/kind
cluster (e.g. `test/kind-config-1worker.yaml`, where the control-plane is
tainted and only one worker is schedulable) the second replica can never place.

**Resolution.**
- Production: ensure the cluster has at least `proxy.minReplicas` schedulable
  nodes for the proxy pods (the default kind config ships two workers).
- Single-node dev clusters: set `spec.proxy.minReplicas: 1` on the
  `ActionsGateway` to run a single proxy replica.

---

← [Back to Operations](.)
