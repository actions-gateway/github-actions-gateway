# Production Runbook

> **Audience:** Platform engineer

For initial setup steps see [Getting Started](../getting-started.md).
For detailed symptom → diagnosis steps see [Troubleshooting](troubleshooting.md).

---

## Day-2 Operations

### Adding a Tenant

1. Ensure the tenant namespace exists: `kubectl get namespace <namespace>`.
2. Have the tenant create the GitHub App Secret in their namespace.
   See [Getting Started §3](../getting-started.md#3-create-a-github-app-credential-secret).
3. Have the tenant create the gateway CR(s) — the recommended v2 `ActionsGateway` + `RunnerSet`, or the legacy v1 `ActionsGateway`.
   See [Getting Started §4](../getting-started.md#4-create-your-gateway-and-runner-set-v2-recommended).
4. Confirm the GMC has provisioned resources within ~30 seconds:
   ```sh
   kubectl get actionsgateway -n <namespace>
   kubectl get deploy,hpa,networkpolicy,resourcequota -n <namespace>
   ```
5. Confirm the `Ready=True` condition on the `ActionsGateway` CR.

No cluster-admin involvement is required after initial GMC deployment.

---

### Adjusting Tenant Quota

The namespace `ResourceQuota` is **platform-owned** — it is not a field on the `ActionsGateway` CR.
Edit the `ResourceQuota` object on the tenant namespace directly (or through your GitOps / tenant-operator stack, if that is what manages it):

```sh
kubectl edit resourcequota -n <namespace> <quota-name>
# Update spec.hard values, save and exit
```

The change takes effect immediately.
Running jobs are not interrupted; the new quota applies on the next pod creation attempt.
The gateway reads remaining quota and reacts to exhaustion but never writes the quota itself.
On the **classic** acquisition tier it declines to claim a job the quota can't place, leaving it queued at GitHub for a sibling with capacity; if headroom is lost after the claim, the pod create is retried in place (`maxQuotaRetries` × `quotaRetryDelay`) and the job is abandoned if the budget runs out.
On a `ScaleSet` set — the default — there is no pre-claim quota rung: the set advertises its configured worker ceiling to GitHub, so a quota-blocked job is assigned and goes straight to that same in-place retry.
Either way, watch `actions_gateway_quota_retries_exhausted_total`.

---

### Scaling maxListeners

```sh
kubectl edit actionsgateway -n <namespace> <name>
# Update spec.runnerGroups[N].maxListeners
```

The GMC propagates the change to the `RunnerGroup` CR.
The AGC reconciles the new ceiling on its next reconcile cycle (a few seconds).
No restart needed.

---

### Rotating GitHub App Credentials

See [Getting Started — Rotating GitHub App Credentials](../getting-started.md#rotating-github-app-credentials) for the full procedure.

In brief: create a new Secret with the new private key, then change `spec.gitHubAppRef.name` in the `ActionsGateway` CR to reference the new Secret.
The GMC detects the Secret reference change and rolls the AGC Deployment.
Do not update the existing Secret in-place; the GMC does not watch Secret contents, only the reference.

---

## Alerting

Reference the SLO targets in [Appendix A](../design/appendix-a-capacity-slos.md) for threshold derivation.

The alerts below cover availability and SLO breaches.
For **abuse and compromise** detection (eviction-retry loops, proxy slowloris, credential harvesting), see [security-operations.md](security-operations.md).

### Which Metrics to Alert On

| Metric | Recommended threshold | Severity | Notes |
| --- | --- | --- | --- |
| `actions_gateway_token_refresh_errors_total` | rate > 1/hour per namespace | Page | Token expiry causes session failures within ~1 hour |
| `actions_gateway_renew_job_errors_total` | rate > 5/minute per namespace | Page | Sustained failures cancel running jobs |
| `actions_gateway_pod_creation_latency_seconds` p95 | > 15s | Ticket | SLO target from Appendix A |
| `actions_gateway_pod_creation_latency_seconds` p99 | > 60s | Page | Indicates scheduling stall or quota exhaustion |
| `actions_gateway_eviction_retries_exhausted_total` | rate > 0 | Ticket | Each increment requires a manual re-run |
| `actions_gateway_active_sessions` | = 0 for a RunnerGroup | Page | No listener polling; jobs queue indefinitely |
| `controller_runtime_reconcile_errors_total` | rate > 1/5min | Ticket | Persistent reconcile failure; resources may be stale |
| `ActionsGateway` condition `RateLimited=True` | duration > 10 minutes | Page | Installation is over API budget |
| Proxy HPA `TARGETS: <unknown>` | any | Ticket | HPA metric broken; autoscaling not working |
| AGC pod OOMKilled | any | Page | AGC has no active sessions while restarting |

### Page-Worthy vs. Ticket-Worthy

**Page** (requires immediate response, typically < 15 minutes):
- `active_sessions = 0` — no jobs can be acquired until fixed.
- `renew_job_errors_total` rate high — jobs will be cancelled.
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

## Alert Rule Reference

Every alert shipped in the reference [`PrometheusRule`](../../deploy/monitoring/prometheusrule.yaml) (reproduced in [Recommended Alert Rules](observability-alerting.md#recommended-alert-rules)) carries a `runbook_url` annotation that resolves to the matching entry below, so on-call lands on a response procedure rather than just the alert's `summary`/`description`.
Severity classes follow [Page-Worthy vs. Ticket-Worthy](#page-worthy-vs-ticket-worthy).

### ActionsGatewayNoActiveSessions

**Page.** The AGC has no open long-poll sessions, so no jobs are acquired and the queue backs up indefinitely.
Restore sessions with [`active_sessions` Flatlining at Zero](#active_sessions-flatlining-at-zero).

### ActionsGatewayTokenRefreshErrors

**Page.** GitHub App token refresh has been failing; sessions will fail once the current token expires (~1 hour).
See [Token Refresh Errors Spiking](troubleshooting.md#token-refresh-errors-spiking).

### ActionsGatewayRenewJobErrors

**Page.** RenewJob is failing at a sustained rate, so running jobs may be cancelled by GitHub.
See [RenewJob Failures Rising](troubleshooting.md#renewjob-failures-rising).

### ActionsGatewayPodCreationLatencyP99

**Page.** p99 pod-creation latency has breached the 60s SLO, indicating a scheduling stall or quota exhaustion.
Triage with [`pod_creation_latency_seconds p95 > 15s`](#pod_creation_latency_seconds-p95--15s).

### ActionsGatewayPodCreationLatencyP95

**Ticket.** p95 pod-creation latency has breached the 15s SLO — degraded but jobs still complete.
Triage with [`pod_creation_latency_seconds p95 > 15s`](#pod_creation_latency_seconds-p95--15s).

### ActionsGatewayEvictionRetriesExhausted

**Ticket.** A job's eviction-retry budget is exhausted and the job requires a manual re-run.
See [Evicted Worker Pods Exhausting Retry Budget](troubleshooting.md#evicted-worker-pods-exhausting-retry-budget).

### ActionsGatewayQuotaRetriesExhausted

**Ticket.** A job's quota-retry budget is exhausted — worker pod creation kept being rejected by the namespace `ResourceQuota` — and the job was abandoned and requires a manual re-run.
Raise the quota or lower `maxWorkers` — see [Jobs Failing Due to Namespace ResourceQuota Exhaustion](troubleshooting.md#jobs-failing-due-to-namespace-resourcequota-exhaustion) and [Adjusting Tenant Quota](#adjusting-tenant-quota).

### ActionsGatewayWorkerQuotaExceeded

**Page.** The namespace `ResourceQuota` is rejecting worker pods, so acquired jobs cannot schedule.
Raise the quota or lower `maxWorkers` — see [Jobs Failing Due to Namespace ResourceQuota Exhaustion](troubleshooting.md#jobs-failing-due-to-namespace-resourcequota-exhaustion) and [Adjusting Tenant Quota](#adjusting-tenant-quota).

### ActionsGatewayProxyQuotaExceeded

**Page.** The `ResourceQuota` is holding the egress proxy pool below the HPA's target.
Raise the quota or lower `proxy.maxReplicas` — see [Proxy Pool Not Scaling](troubleshooting.md#proxy-pool-not-scaling).

### ActionsGatewayQuotaPressure

**Ticket.** A proxy or worker pool cannot reach its configured ceiling within the namespace `ResourceQuota` headroom.
Plan a quota increase before the next load spike — see [Adjusting Tenant Quota](#adjusting-tenant-quota) and [Jobs Failing Due to Namespace ResourceQuota Exhaustion](troubleshooting.md#jobs-failing-due-to-namespace-resourcequota-exhaustion).

### ActionsGatewayReconcileErrors

**Ticket.** A controller is logging sustained reconcile errors and owned resources may be stale.
See [GMC Not Provisioning Tenant Resources](troubleshooting.md#gmc-not-provisioning-tenant-resources).

### ActionsGatewayWorkersUnschedulable

**Page.** Worker pods are stuck `Pending` past the scheduling grace because the scheduler cannot place them (no matching node / affinity / taints — not quota); capacity is not materializing.
See [RunnerGroup Reports WorkersUnschedulable](troubleshooting.md#runnergroup-reports-workersunschedulable) and [Worker Pods Stuck Pending](troubleshooting.md#worker-pods-stuck-pending).

### ActionsGatewayEgressRulesStale

**Page.** The gateway's GitHub egress IP-range allowlist has not refreshed within the staleness window; the proxy `NetworkPolicy` may drift from GitHub's published ranges.
See [ActionsGateway Reports EgressRulesStale](troubleshooting.md#actionsgateway-reports-egressrulesstale).

### ActionsGatewayGitHubEgressIncomplete

**Ticket.** A referring gateway names a GitHub Enterprise Server host the `EgressProxy` pool's `CIDR`-mode allowlist cannot reach, so that tenant's GitHub traffic is denied and it acquires no jobs.
The fix is yours to make — the appliance's ranges are knowable only to you, so this will not self-heal: supply them in `spec.destinationCIDRs` (a platform admin must allowlist them first) or switch the pool to an FQDN egress mode.
See [A GHES Tenant's Traffic Never Reaches the Appliance](troubleshooting.md#a-ghes-tenants-traffic-never-reaches-the-appliance).

### ActionsGatewayScaleSetNameCollision

**Page.** Two tenants are driving one scale set at GitHub right now, and each is acquiring the other's jobs: another organization's workflow content runs in the wrong namespace, against the wrong quota, egressing from the wrong attributed IPs.
Admission rejects every new such pair, so this one predates the guard (an upgrade from before `v1.5.0`) or was applied while the validating webhook was uninstalled.
It will not self-heal, and the condition names only the alerting gateway's own runner sets: the other holder is in the GMC log, because gateway status is tenant-readable.
See [`ActionsGateway` Reports `ScaleSetNameCollision`](troubleshooting.md#actionsgateway-reports-scalesetnamecollision).

### ActionsGatewayAgentRecycleErrors

**Ticket.** Single-use JIT agent re-registration is failing; sustained growth shrinks listener capacity and decays tenant throughput job by job.
See [Concurrent Job Burst Serializes to ~1 Worker (Recycle Blocked on a Still-Running Runner)](troubleshooting.md#concurrent-job-burst-serializes-to-1-worker-recycle-blocked-on-a-still-running-runner).

### ActionsGatewayFanoutFallbackTimeout

**Ticket.** Deduped fan-out losers are recycling on the fallback timeout because their winner never concluded within the bound — a class of stuck winners.
Investigate long-running or wedged winning jobs; see [Concurrent Job Burst Serializes to ~1 Worker (Duplicate Job Acquisition)](troubleshooting.md#concurrent-job-burst-serializes-to-1-worker-duplicate-job-acquisition).

### ActionsGatewayAbandonedDeliveryErrors

**Ticket.** The winner of a fanned-out job is failing to issue `completejob` on a deduped sibling delivery; affected jobs may be cancelled at GitHub's ~15-minute unstarted-job timeout.
See [Concurrent Job Burst Serializes to ~1 Worker (Duplicate Job Acquisition)](troubleshooting.md#concurrent-job-burst-serializes-to-1-worker-duplicate-job-acquisition).

### ActionsGatewayScaleSetProvisioningStalled

**Page.** The scale-set acquisition tier (the default protocol) is receiving `JobAssigned` messages but has provisioned no worker pods — the tier is wedged and acquired jobs will not start.
A ScaleSet-protocol RunnerSet emits no `actions_gateway_active_sessions`, so this demand-vs-supply signal is the scale-set analog of [`active_sessions` Flatlining at Zero](#active_sessions-flatlining-at-zero).
Respond with [Scale-set provisioning stalled](#scale-set-provisioning-stalled).

### ActionsGatewayScaleSetProvisionErrors

**Ticket.** The scale-set tier is failing to provision worker pods (JIT-config mint or pod create) at a sustained rate.
A transient failure retries on a later poll; a sustained rate means provisioning is degraded.
Check the run service's `generate-jitconfig` responses and namespace quota headroom, then triage as for [Scale-set provisioning stalled](#scale-set-provisioning-stalled).

### ActionsGatewayScaleSetJobsDeferred

**Ticket.** One or more jobs assigned to a scale set cannot register their runner name (`generate-jitconfig` 409 that neither deleting the stale record nor a fresh suffixed name cleared), so no worker is running them.
The listener holds each one and re-offers it on a backoff, so the run starts by itself as soon as the name is free — but until then it sits queued at GitHub.
Read the job ids off the `RunnerSet`'s `JobProvisionStalled` condition, then free the conflicting runner records per [Scale-Set Job Stranded by a Stale Runner Record](troubleshooting.md#scale-set-job-stranded-by-a-stale-runner-record-runner-name-409).

The alert is scoped to `reason="name_conflict"`.
The other reason the same gauge carries, `reason="ceiling"`, is a set running at the worker concurrency its spec declares — expected backpressure with no action to take, covered by [Scale-Set Jobs Waiting at the Worker Ceiling](troubleshooting.md#scale-set-jobs-waiting-at-the-worker-ceiling-workerceilingreached).

### ActionsGatewayProxyConnectDenied

**Ticket.** The egress proxy is refusing CONNECT requests to destinations off the egress allowlist at a sustained rate — a Server-Side Request Forgery (SSRF) / egress-policy signal.
Every increment is an explicit allowlist denial (sharper than `dial_errors`, which also counts transient failures to *allowed* hosts).

1. Identify the denied destinations from the proxy's `CONNECT destination not allowed` warning logs, which record the rejected `host`: `kubectl logs -n <namespace> deploy/<proxy-deployment> | grep "destination not allowed"`.
2. If the destinations are unexpected, treat it as SSRF probing or a compromised workload — inspect the workflow acquiring the affected runner and correlate with `proxy_dial_errors_total`.
3. If a legitimate egress target is missing, add it to the platform-owned egress allowlist rather than to the workflow — see [security-operations.md § Worker egress destinations](security-operations.md#worker-egress-destinations-the-egress-allowlist).

### ActionsGatewayCapacityGateRejectingJobs

**Ticket.** A runner set opted into [`spec.capacityGate`](troubleshooting.md#runnerset-reports-workercapacitydeclined-the-gateway-stopped-claiming-jobs) and the gate has been leaving delivered jobs queued at GitHub for half an hour, because the cluster cannot place another worker pod of that set's shape.
This is the **classic** acquisition tier, where every increment is one job GitHub delivered and the AGC declined to claim.

Refusing is the gate working: claiming a job the cluster cannot run spends a single-use JIT runner record and ends in a cancelled workflow run.
What the alert reports is that the refusal is not clearing.
The gate throttles rather than seals, so intake continues at roughly one claim per `pendingPodDeadline` window; over 30 minutes that is at least three probes that failed to find capacity.

1. Read the gate's evidence: `max by (namespace, runner_set, reason) (actions_gateway_runnerset_worker_capacity_declined == 1)`. `ScaleUpDeclined` is the cluster autoscaler's own refusal, `PodsUnschedulable` is the scheduler's verdict, and `AwaitingProbe` is the latched state that outlives the pod which produced it.
   The gauge keys on `runner_set` while this alert's series keys on `runner_group`; both carry the same set's name, because the rejection counter is shared with v1 `RunnerGroup` owners.
2. Work the reason with [RunnerSet Reports WorkerCapacityDeclined](troubleshooting.md#runnerset-reports-workercapacitydeclined-the-gateway-stopped-claiming-jobs), which maps each one to the node, taint, or quota fix that restores placement.
3. The one-line rollback, if the gate is wrong about the cluster, is `spec.capacityGate.mode: Off`; the same section covers when that is the right call.

### ActionsGatewayScaleSetCapacityWithheld

**Ticket.** The same gate on the **scale-set** (default) tier, where a declined job is never assigned at all, so there is no rejected delivery to count.
The gate shows up instead as slots removed from the ceiling the set advertises to GitHub, and runs wait in the queue for capacity the set is declining to offer.

The alert requires the set to have been assigned work in the last hour, which is what separates a gate doing its job from one worth a ticket.
An idle gated set whose worker shape stays unplaceable holds a latched `AwaitingProbe` decline indefinitely; that reading is truthful and costs nothing, so on its own it must not raise a ticket.
Under a real withhold the per-window probe job is still assigned, so a set with work waiting keeps `actions_gateway_scaleset_jobs_assigned_total` moving while an idle one does not.

1. Read `actions_gateway_scaleset_advertised_capacity` for what the set is still offering and `actions_gateway_scaleset_capacity_withheld` by `reason` for who took the rest; a `quota` share alongside the `capacity` one means the [ResourceQuota](troubleshooting.md#jobs-failing-due-to-namespace-resourcequota-exhaustion) is binding too.
2. Then follow the same evidence and remediation path as [ActionsGatewayCapacityGateRejectingJobs](#actionsgatewaycapacitygaterejectingjobs) above.

---

## SLO Breach Response

### `pod_creation_latency_seconds p95 > 15s`

1. Check for quota exhaustion: `kubectl describe resourcequota -n <namespace>`.
2. Check for pending pods: `kubectl get pods -n <namespace> | grep Pending`.
3. Describe a pending pod for scheduling events: `kubectl describe pod -n <namespace> <pod>`.
4. If quota is exhausted: raise the platform-owned `ResourceQuota` on the namespace (`kubectl edit resourcequota -n <namespace> <quota-name>`) or wait for running pods to complete.
5. If no schedulable nodes: check node autoscaler or provision capacity.
6. If PriorityClass is missing: create it.
   See [Troubleshooting — Worker Pods Stuck Pending](troubleshooting.md#worker-pods-stuck-pending).

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

### Scale-set provisioning stalled

The scale-set tier is the default protocol; it emits no `active_sessions` gauge, so a wedge shows up as `scaleset_jobs_assigned_total` climbing while `scaleset_jobs_provisioned_total` stays flat.

1. Confirm the wedge: `scaleset_jobs_assigned_total` is rising but `scaleset_jobs_provisioned_total` is not, for the affected `namespace`/`runner_set`.
2. Check the provision-error rate: `rate(actions_gateway_scaleset_provision_errors_total[5m])`.
   A non-zero rate points at JIT-config mint or pod-create failures — inspect the AGC logs (`kubectl logs -n <namespace> deploy/actions-gateway-controller --tail=100`) for `generate-jitconfig` errors and worker-pod create rejections.
3. Check the worker-pod `ResourceQuota` (see [Adjusting Tenant Quota](#adjusting-tenant-quota)) and the `WorkerQuotaExceeded` / `WorkersUnschedulable` conditions — a full quota or an unschedulable pod stalls provisioning with no provision *error*.
4. If provision errors are zero and quota is healthy, check the listener session itself: the RunnerSet's `Ready`/`RateLimited`/`Degraded` conditions (`kubectl get runnerset -n <namespace> -o yaml`) surface a rate-limited or unauthorized scale-set session (Q325).
   The conditions are a *state* signal that only trips once an episode persists (`RateLimited` after ten minutes), so pair them with the *rate* signal `rate(actions_gateway_message_poll_errors_total[5m])` for the same namespace — a stream of brief 429 or transport episodes throttles polling without ever setting a condition (Q446).
5. If the queue is genuinely empty, `scaleset_jobs_assigned_total` will also be flat — the alert only fires when assignment is *active*, so a flat-both state is benign.

---

## Incident Response

For restoring a deleted or corrupted `ActionsGateway` CR, a lost tenant namespace, or a full cluster, see the dedicated [Backup, Restore, and Disaster Recovery](backup-restore.md) guide.
The CR is the source of truth; deleting it cascades to the resources the GMC owns, and re-applying it reconciles them back.

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
8. Delete the old Secret once confirmed healthy, and delete the downloaded `.pem` file from disk (`shred -u <path>` on Linux, `rm -P <path>` on macOS) — the key now lives only in the Kubernetes Secret.

**Scope assessment.** The compromised key could have been used to acquire installation tokens (scoped to `Actions: Read`, `Administration: Read`).
Check GitHub's audit log for unusual API activity from the App installation: Settings → Organizations → `<org>` → Audit log → filter by the App name.

---

### AGC Total Failure

If the AGC pod is destroyed and cannot restart (e.g. node failure without rescheduling, OOM loop):

1. **In-flight jobs** whose `renewjob` loop has lapsed will be cancelled by GitHub.
   There is no automatic recovery for these — they require manual re-run.
2. **Queued jobs** (not yet acquired) will be redelivered by GitHub to the next healthy session within ~2 minutes of the AGC restarting.
3. **To force restart:** `kubectl rollout restart deploy/actions-gateway-controller -n <namespace>`.
4. Monitor `actions_gateway_active_sessions` — it should reach 1 per RunnerGroup within a few seconds of the pod starting.

**State that persists:** All RunnerGroup CRs, Secrets, and Kubernetes resources are durable.
The AGC reconstructs all in-memory state (session registry, per-job renewers) from scratch on restart.
The only non-recoverable state is in-flight job locks that expire during the blackout window.

---

### GMC Total Failure

If the GMC pod is unavailable:

1. **Existing tenant gateways continue operating normally.** The GMC is not in the data plane; it only responds to `ActionsGateway` CR changes.
   Provisioned AGCs, proxies, and RunnerGroups are not affected.
2. **New `ActionsGateway` CRs will not be provisioned** until the GMC recovers.
3. **Spec changes to existing `ActionsGateway` CRs will not be reconciled** until the GMC recovers.
4. To restore: `kubectl rollout restart deploy/gmc-controller-manager -n gmc-system`.
5. On recovery, the GMC reconciles all `ActionsGateway` CRs idempotently — it compares desired vs. actual state and only applies changes.
   No resources are duplicated or deleted.

---

## On-Call Handoff Checklist

Before handing off to the next on-call:

- [ ] All `ActionsGateway` conditions `Ready=True` across active tenant namespaces.
- [ ] No sustained `RateLimited` conditions.
- [ ] `active_sessions` > 0 for all active RunnerGroups.
- [ ] `token_refresh_errors_total` rate is zero (or below 1/hour).
- [ ] `renew_job_errors_total` rate is zero.
- [ ] No pods in `CrashLoopBackOff` or `OOMKilled` state.
- [ ] No open incidents or unresolved pages.
- [ ] Any `eviction_retries_exhausted_total` increments from the shift are documented and re-runs are queued.

---

## Reference Links

- [Backup, Restore, and Disaster Recovery](backup-restore.md) — backup posture and recovery procedures for a deleted or corrupted CR
- [Troubleshooting Guide](troubleshooting.md) — symptom → diagnosis → resolution for each failure mode
- [Security Operations](security-operations.md) — abuse-detection alerts and compromise-response playbooks
- [Observability: metrics reference](observability-metrics.md) — full metrics reference
- [Getting Started](../getting-started.md) — initial setup and credential rotation
- [Appendix A — Capacity Targets & SLOs](../design/appendix-a-capacity-slos.md)
- [Appendix E — Capacity Planning](../design/appendix-e-capacity-planning.md)
