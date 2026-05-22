# Troubleshooting Guide

Audience: on-call site reliability engineer (SRE), platform engineer.

Each section below covers a specific failure mode: symptoms, likely cause, diagnostic commands, and resolution steps.

---

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

## GMC Not Provisioning Tenant Resources

**Symptoms.** An `ActionsGateway` CR was applied but nothing has been created in the tenant namespace: no AGC Deployment, no proxy Deployment, no RunnerGroup resources.

**Likely causes.**
- GMC pod is not running or not the leader.
- GMC lacks permission to write to the tenant namespace (RBAC misconfiguration during initial GMC install).
- The `ActionsGateway` CR failed admission validation (check for validation errors in `kubectl apply` output or `Events`).

**Diagnostics.**

```sh
# Check whether the GMC is running and has a leader
kubectl get lease -n actions-gateway-system
kubectl get pods -n actions-gateway-system

# Check GMC logs for reconcile errors
kubectl logs -n actions-gateway-system deploy/gateway-manager-controller --tail=100 | grep -i error

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

# Test proxy reachability from inside the AGC pod
kubectl exec -n <namespace> deploy/actions-gateway-controller -- \
  curl -x $HTTPS_PROXY -sI https://api.github.com

# Check RunnerGroup conditions
kubectl get runnergroup -n <namespace> -o yaml | grep -A 10 conditions
```

**Resolution.**
- If the Secret is missing or has wrong keys, recreate it. See [Getting Started — GitHub App Secret](../getting-started.md#2-create-a-github-app-credential-secret).
- If the private key format is wrong, ensure it is a PEM-encoded RSA key starting with `-----BEGIN RSA PRIVATE KEY-----`. The Secret `stringData.privateKey` must include the full key including header and footer lines.
- If the runner version is outdated, update `workerImage` in the RunnerGroup spec (or the AGC's `--worker-image` flag). Watch for `RunnerGroup` conditions with reason `VersionTooOld`.
- If `appId` or `installationId` are wrong, update the Secret.

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

# Test connectivity to GitHub from the AGC
kubectl exec -n <namespace> deploy/actions-gateway-controller -- \
  curl -x $HTTPS_PROXY -sI https://api.github.com/app
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

## Network Connectivity Failures

**Symptoms.** The AGC cannot reach GitHub through the proxy. Logs show `connection refused`, `dial tcp: i/o timeout`, or `proxy: no response from proxy`.

**Likely causes.**
- The proxy pod is not running or not ready.
- `HTTP_PROXY`/`HTTPS_PROXY` environment variables are incorrect (wrong Service name or port).
- `NetworkPolicy` is blocking the AGC-to-proxy egress path.
- The proxy pod's egress to GitHub is blocked (`NetworkPolicy` IP ranges are stale).

**Diagnostics.**

```sh
# Check proxy pod status
kubectl get pods -n <namespace> -l app=actions-gateway-proxy

# Verify the proxy Service exists and has endpoints
kubectl get svc -n <namespace> actions-gateway-proxy
kubectl get endpoints -n <namespace> actions-gateway-proxy

# Check the AGC's HTTPS_PROXY env var
kubectl exec -n <namespace> deploy/actions-gateway-controller -- env | grep PROXY

# Test proxy connectivity from the AGC
kubectl exec -n <namespace> deploy/actions-gateway-controller -- \
  curl -v -x $HTTPS_PROXY https://api.github.com 2>&1 | head -20

# Check NetworkPolicy rules
kubectl get networkpolicy -n <namespace>
kubectl describe networkpolicy -n <namespace>

# Check the IP range refresh metric
# Metric: actions_gateway_ip_range_updates_total{namespace="<namespace>"}
```

**Resolution.**
- If the proxy pod is down: check its logs and restart if necessary.
- If the `NetworkPolicy` egress rules are stale: trigger a manual refresh by temporarily setting `spec.proxy.managedNetworkPolicy: false` and back to `true`, or wait for the 24-hour automatic refresh cycle. Check the GitHub meta API for current IP ranges: `curl https://api.github.com/meta | jq .actions`.
- If the `NO_PROXY` list is missing the cluster service CIDR: update `spec.proxy.noProxyCIDRs` to include your cluster's service CIDR (see the `noProxyCIDRs` field documentation in [§3.1](../design/03-api-contracts.md#31-kubernetes-crd-schemas)).

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

← [Back to Operations](.)
