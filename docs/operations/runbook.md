# Production Runbook

Audience: on-call SRE. For initial setup steps see [Getting Started](../getting-started.md). For detailed symptom → diagnosis steps see [Troubleshooting](troubleshooting.md).

---

## Day-2 Operations

### Adding a Tenant

1. Ensure the tenant namespace exists: `kubectl get namespace <namespace>`.
2. Have the tenant create the GitHub App Secret in their namespace. See [Getting Started §2](../getting-started.md#2-create-a-github-app-credential-secret).
3. Have the tenant create the `ActionsGateway` CR. See [Getting Started §3](../getting-started.md#3-create-an-actionsgateway-resource).
4. Confirm the GMC has provisioned resources within ~30 seconds:
   ```sh
   kubectl get actionsgateway -n <namespace>
   kubectl get deploy,hpa,networkpolicy,resourcequota -n <namespace>
   ```
5. Confirm the `Ready=True` condition on the `ActionsGateway` CR.

No cluster-admin involvement is required after initial GMC deployment.

---

### Adjusting Tenant Quota

Edit the `ActionsGateway` spec's `namespaceQuota` field:

```sh
kubectl edit actionsgateway -n <namespace> <name>
# Update spec.namespaceQuota values, save and exit
```

The GMC reconciles the change and patches the `ResourceQuota` object within seconds. Running jobs are not interrupted; the new quota takes effect on the next pod creation attempt.

---

### Scaling maxListeners

```sh
kubectl edit actionsgateway -n <namespace> <name>
# Update spec.runnerGroups[N].maxListeners
```

The GMC propagates the change to the `RunnerGroup` CR. The AGC reconciles the new ceiling on its next reconcile cycle (a few seconds). No restart needed.

---

### Rotating GitHub App Credentials

See [Getting Started — Rotating GitHub App Credentials](../getting-started.md#rotating-github-app-credentials) for the full procedure.

In brief: create a new Secret with the new private key, then change `spec.gitHubAppRef.name` in the `ActionsGateway` CR to reference the new Secret. The GMC detects the Secret reference change and rolls the AGC Deployment. Do not update the existing Secret in-place; the GMC does not watch Secret contents, only the reference.

---

## Alerting

Reference the SLO targets in [Appendix A](../design/appendix-a-capacity-slos.md) for threshold derivation.

### Which Metrics to Alert On

| Metric | Recommended threshold | Severity | Notes |
| --- | --- | --- | --- |
| `actions_gateway_token_refresh_errors_total` | rate > 1/hour per namespace | Page | Token expiry causes session failures within ~1 hour |
| `actions_gateway_renewjob_errors_total` | rate > 5/minute per namespace | Page | Sustained failures cancel running jobs |
| `actions_gateway_pod_creation_latency_seconds` p95 | > 15s | Ticket | SLO target from Appendix A |
| `actions_gateway_pod_creation_latency_seconds` p99 | > 60s | Page | Indicates scheduling stall or quota exhaustion |
| `actions_gateway_eviction_retries_exhausted_total` | rate > 0 | Ticket | Each increment requires a manual re-run |
| `actions_gateway_active_sessions` | = 0 for a RunnerGroup | Page | No listener polling; jobs queue indefinitely |
| `actions_gateway_reconcile_errors_total` | rate > 1/5min | Ticket | Persistent reconcile failure; resources may be stale |
| `ActionsGateway` condition `RateLimited=True` | duration > 10 minutes | Page | Installation is over API budget |
| Proxy HPA `TARGETS: <unknown>` | any | Ticket | HPA metric broken; autoscaling not working |
| AGC pod OOMKilled | any | Page | AGC has no active sessions while restarting |

### Page-Worthy vs. Ticket-Worthy

**Page** (requires immediate response, typically < 15 minutes):
- `active_sessions = 0` — no jobs can be acquired until fixed.
- `renewjob_errors_total` rate high — jobs will be cancelled.
- `token_refresh_errors_total` spiking — token will expire within ~1 hour.
- `pod_creation_latency p99 > 60s` — scheduling is stalled.
- `RateLimited` condition > 10 minutes — installation is over budget.
- AGC pod in `OOMKilled` / `CrashLoopBackOff`.

**Ticket** (respond within next business day):
- `pod_creation_latency p95 > 15s` — degraded but jobs are completing.
- `eviction_retries_exhausted_total` incrementing — jobs require manual re-run.
- `reconcile_errors_total` non-zero — investigate before it becomes a page.
- HPA metric unknown — autoscaling broken; proxy may not handle burst load.

---

## SLO Breach Response

### `pod_creation_latency_seconds p95 > 15s`

1. Check for quota exhaustion: `kubectl describe resourcequota -n <namespace>`.
2. Check for pending pods: `kubectl get pods -n <namespace> | grep Pending`.
3. Describe a pending pod for scheduling events: `kubectl describe pod -n <namespace> <pod>`.
4. If quota is exhausted: increase `namespaceQuota` or wait for running pods to complete.
5. If no schedulable nodes: check node autoscaler or provision capacity.
6. If PriorityClass is missing: create it. See [Troubleshooting — Worker Pods Stuck Pending](troubleshooting.md#worker-pods-stuck-pending).

### `active_sessions` Flatlining at Zero

1. Check AGC pod status: `kubectl get pod -n <namespace> -l app=actions-gateway-controller`.
2. Check AGC logs: `kubectl logs -n <namespace> deploy/actions-gateway-controller --tail=100`.
3. Check RunnerGroup conditions: `kubectl get runnergroup -n <namespace> -o yaml`.
4. If pod is `CrashLoopBackOff` or `Error`: see [Troubleshooting — AGC CrashLoopBackOff](troubleshooting.md#agc-crashloopbackoff-or-not-acquiring-jobs).
5. If pod is running but sessions are zero: check for token errors (see [Token Refresh Errors](troubleshooting.md#token-refresh-errors-spiking)) and network connectivity (see [Network Connectivity Failures](troubleshooting.md#network-connectivity-failures)).

### `jobs_acquired_total` Stops Incrementing

1. Verify jobs are actually queued: check the GitHub Actions UI for the repository.
2. Check `active_sessions` — if zero, restore sessions first (see above).
3. Check `RateLimited` condition — if true, reduce session load or wait for the burst to subside.
4. Check `message_poll_errors_total` — persistent poll errors indicate a broken GitHub connection.
5. If sessions are active and no errors, the queue may simply be empty.

---

## Incident Response

### GitHub App Key Compromise

**Immediate steps (< 5 minutes):**

1. Revoke the compromised private key in the GitHub App settings (Settings → Developer settings → GitHub Apps → `<app>` → Private keys → Revoke).
2. The AGC's token refresh will fail within minutes of revocation; sessions will become invalid.

**Restoration steps:**

3. Generate a new private key from the GitHub App settings page and download the `.pem` file.
4. Create a new Secret with the new key:
   ```sh
   kubectl create secret generic <new-secret-name> \
     --from-literal=appId=<appId> \
     --from-literal=installationId=<installationId> \
     --from-file=privateKey=<path-to-new-key.pem> \
     -n <namespace>
   ```
5. Update the `ActionsGateway` CR to reference the new Secret:
   ```sh
   kubectl patch actionsgateway -n <namespace> <name> \
     --type=merge -p '{"spec":{"gitHubAppRef":{"name":"<new-secret-name>"}}}'
   ```
6. Confirm the AGC Deployment has rolled and the new pod is healthy:
   ```sh
   kubectl rollout status deploy/actions-gateway-controller -n <namespace>
   ```
7. Confirm `actions_gateway_token_refresh_errors_total` is no longer incrementing.
8. Delete the old Secret once confirmed healthy.

**Scope assessment.** The compromised key could have been used to acquire installation tokens (scoped to `Actions: Read`, `Administration: Read`). Check GitHub's audit log for unusual API activity from the App installation: Settings → Organizations → `<org>` → Audit log → filter by the App name.

---

### AGC Total Failure

If the AGC pod is destroyed and cannot restart (e.g. node failure without rescheduling, OOM loop):

1. **In-flight jobs** whose `renewjob` loop has lapsed will be cancelled by GitHub. There is no automatic recovery for these — they require manual re-run.
2. **Queued jobs** (not yet acquired) will be redelivered by GitHub to the next healthy session within ~2 minutes of the AGC restarting.
3. **To force restart:** `kubectl rollout restart deploy/actions-gateway-controller -n <namespace>`.
4. Monitor `actions_gateway_active_sessions` — it should reach 1 per RunnerGroup within a few seconds of the pod starting.

**State that persists:** All RunnerGroup CRs, Secrets, and Kubernetes resources are durable. The AGC reconstructs all in-memory state (session registry, per-job renewers) from scratch on restart. The only non-recoverable state is in-flight job locks that expire during the blackout window.

---

### GMC Total Failure

If the GMC pod is unavailable:

1. **Existing tenant gateways continue operating normally.** The GMC is not in the data plane; it only responds to `ActionsGateway` CR changes. Provisioned AGCs, proxies, and RunnerGroups are not affected.
2. **New `ActionsGateway` CRs will not be provisioned** until the GMC recovers.
3. **Spec changes to existing `ActionsGateway` CRs will not be reconciled** until the GMC recovers.
4. To restore: `kubectl rollout restart deploy/gmc-controller-manager -n gmc-system`.
5. On recovery, the GMC reconciles all `ActionsGateway` CRs idempotently — it compares desired vs. actual state and only applies changes. No resources are duplicated or deleted.

---

## On-Call Handoff Checklist

Before handing off to the next on-call:

- [ ] All `ActionsGateway` conditions `Ready=True` across active tenant namespaces.
- [ ] No sustained `RateLimited` conditions.
- [ ] `active_sessions` > 0 for all active RunnerGroups.
- [ ] `token_refresh_errors_total` rate is zero (or below 1/hour).
- [ ] `renewjob_errors_total` rate is zero.
- [ ] No pods in `CrashLoopBackOff` or `OOMKilled` state.
- [ ] No open incidents or unresolved pages.
- [ ] Any `eviction_retries_exhausted_total` increments from the shift are documented and re-runs are queued.

---

## Reference Links

- [Troubleshooting Guide](troubleshooting.md) — symptom → diagnosis → resolution for each failure mode
- [Observability](observability.md) — full metrics reference
- [Getting Started](../getting-started.md) — initial setup and credential rotation
- [Appendix A — Capacity Targets & SLOs](../design/appendix-a-capacity-slos.md)
- [Appendix E — Capacity Planning](../design/appendix-e-capacity-planning.md)
