# Alerting & SLOs

> **Audience:** Platform engineer

Part of the [Observability](observability.md) guide.
The metrics referenced below are catalogued in the [Metrics reference](observability-metrics.md); to scrape them, see [Accessing metrics](observability-metrics-access.md).
For SLO targets, see [Appendix A — Capacity Targets & SLOs](../design/appendix-a-capacity-slos.md).

## Symptom → Metric Mapping

| Symptom | Metric(s) to check | Notes |
| --- | --- | --- |
| Jobs are slow to start | `pod_creation_latency_seconds` p95/p99 | SLO: p95 ≤ 15s, p99 ≤ 60s. Observed when a pod's lifetime ends, so a burst of long jobs delays the signal rather than hiding it |
| Jobs are not being acquired | `scaleset_session_open` (should be 1 per RunnerSet), `message_poll_errors_total` | `0` = the listener holds no session, so GitHub delivers it nothing; a rising poll-error rate = a session whose polls fail |
| Scale-set jobs assigned but not starting | `scaleset_jobs_assigned_total` rising vs `scaleset_jobs_provisioned_total` flat | Provisioning wedged; check `scaleset_provision_errors_total` and worker-pod quota |
| One scale-set job never starts, the rest do | `scaleset_jobs_deferred{reason="name_conflict"}` > 0 | Its runner name will not register; the listener re-offers it on a backoff. Read the `RunnerSet`'s `JobProvisionStalled` condition for the job ids; see the [runbook](troubleshooting.md#scale-set-job-stranded-by-a-stale-runner-record-runner-name-409) |
| Scale-set assignments give up without running | `rate(scaleset_jobs_abandoned_total[15m])` > 0 | GitHub stopped holding jobs the listener was still trying to place, so those runs will not start. Expected in a burst around a mass cancellation or a drain; investigate a sustained rate. See the [runbook](troubleshooting.md#scale-set-assignments-abandoned-assignmentabandoned) |
| Scale-set jobs queue while its workers all run | `scaleset_jobs_deferred{reason="ceiling"}` > 0 | Expected: the set is at the worker ceiling its spec declares, and the held jobs start as workers finish. Alert only on it being sustained; see the [runbook](troubleshooting.md#scale-set-jobs-waiting-at-the-worker-ceiling-workerceilingreached) |
| A gated set has stopped claiming work | `runnerset_worker_capacity_declined` grouped by `reason`; the cost shows as `scaleset_capacity_withheld{reason="capacity"}` | The opt-in capacity gate is refusing intake because the cluster cannot place another worker of this set's shape. Alert on the cost series rather than the gauge: a latched `AwaitingProbe` decline sits `True` indefinitely on an idle set and costs nothing. See the [runbook](troubleshooting.md#runnerset-reports-workercapacitydeclined-the-gateway-stopped-claiming-jobs) |
| Runner credentials are broken | `token_refresh_errors_total` | Spikes indicate Secret or GitHub App issue |
| Evictions causing re-runs | `eviction_retries_total`, `eviction_retries_exhausted_total` | Exhausted budget requires manual intervention |
| Quota rejecting worker pods | `quota_retries_total`, `quota_retries_exhausted_total` | Sustained retries mean tight quota headroom; exhausted budget requires manual intervention |
| Jobs cancelled without ever starting | `worker_pods_reaped_total{reason="pending_deadline"}` | Worker pod stuck Pending past the deadline — fix the image/scheduling cause; see the [runbook](troubleshooting.md#worker-pod-reaped-while-pending-workerpodstuckpending) |
| Jobs ending before their worker can start | `worker_pods_reaped_total{reason="completed_pending"}` | Worker pod still Pending when GitHub reported its job terminal — cancellations outrunning pod startup, or an AGC replaying a queue after a restart; see the [runbook](troubleshooting.md#worker-pod-reaped-while-pending-after-its-job-completed-workerpodcompletedpending) |
| Workers idling after their job is over | `worker_pods_reaped_total{reason="orphaned_running"}` | ScaleSet worker still Running five minutes after GitHub reported its job terminal — it never received the job, or a sidecar outlived the runner; see the [runbook](troubleshooting.md#worker-pod-reaped-while-running-workerpodorphanedrunning) |
| Workers reaped by a gateway teardown | `worker_pods_reaped_total{reason="gateway_deleted"}` | Someone deleted an `ActionsGateway` while its tenant had jobs running; those jobs are lost. Expected during a deliberate tenant teardown, and worth investigating otherwise — drain before deleting a gateway; see the [runbook](troubleshooting.md#worker-pods-reaped-on-gateway-teardown-workerpodsreapedongatewayteardown) |
| Workers killed by the lifetime cap | `worker_pods_reaped_total{reason="lifetime_exceeded"}` | A worker outlived `maxWorkerLifetime` (default 12h) and the kubelet killed it. Either a wedged worker the cap correctly bounded, or a legitimately long job that needs the field raised — tell them apart before changing anything; see the [runbook](troubleshooting.md#worker-killed-by-the-lifetime-cap-workerpodlifetimeexceeded) |
| Jobs running but `ACTIVEJOBS` shows 0 | Check pod phase with `kubectl get pods -l actions-gateway.com/runner-set=<name>` | `ACTIVEJOBS` is updated on pod phase-change events; the column reflects the last reconcile snapshot — not a real-time gauge. A pod that changed phase after the last reconcile will show up after the next event fires. |
| A GHES tenant acquires no jobs | `github_egress_incomplete` | `1` means the pool's `CIDR` allowlist cannot reach the appliance the tenant's gateway names; the connection times out with nothing else naming the cause. See the [runbook](troubleshooting.md#a-ghes-tenants-traffic-never-reaches-the-appliance) |
| Proxy autoscaling not working | HPA TARGETS showing `<unknown>` | `requests.cpu` not set on proxy pods |
| GMC/AGC reconcile broken | `reconcile_errors_total` | Non-zero sustained rate indicates operator issue |

---

## Recommended Alert Rules

> **Apply as code.** These rules — and the [SLO recording rules](#slo-recording-rules) below — ship as a directly-appliable `PrometheusRule` at [`deploy/monitoring/prometheusrule.yaml`](../../deploy/monitoring/prometheusrule.yaml).
> `kubectl apply` it into a namespace your Prometheus selects rules from instead of copying the YAML below by hand (see [`deploy/monitoring/README.md`](../../deploy/monitoring/README.md)).
> The blocks here are the same rules, reproduced for reference.

The following Prometheus alerting rules map to the SLO targets in [Appendix A](../design/appendix-a-capacity-slos.md).
Adjust thresholds to match your environment.

```yaml
groups:
  - name: actions-gateway
    rules:

      # Page: token refresh errors risk job failures within ~1 hour
      - alert: ActionsGatewayTokenRefreshErrors
        expr: |
          rate(actions_gateway_token_refresh_errors_total[5m]) > 0
        for: 5m
        labels:
          severity: critical
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewaytokenrefresherrors"
          summary: "GitHub App token refresh errors in {{ $labels.namespace }}"
          description: "Token refresh has been failing for 5+ minutes. Sessions will fail once the current token expires (~1 hour)."

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
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewaypodcreationlatencyp99"
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
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewaypodcreationlatencyp95"
          summary: "Pod creation p95 latency degraded in {{ $labels.namespace }}"
          description: "p95 pod creation latency exceeds 15s SLO. Investigate quota and scheduling."

      # Ticket: eviction budget exhausted — manual re-run required
      - alert: ActionsGatewayEvictionRetriesExhausted
        expr: |
          increase(actions_gateway_eviction_retries_exhausted_total[5m]) > 0
        labels:
          severity: warning
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewayevictionretriesexhausted"
          summary: "Eviction retry budget exhausted for {{ $labels.runner_group }} in {{ $labels.namespace }}"
          description: "A job's eviction retry budget has been exhausted. Manual re-run required."

      # Ticket: a retry cause's re-runs mostly fail, so the cause is misclassified (Q1245)
      - alert: ActionsGatewayRetryCauseMisclassified
        expr: |
          (
            sum by (namespace, runner_group, cause) (increase(actions_gateway_eviction_rerun_outcomes_total{result="failed"}[7d]))
              /
            sum by (namespace, runner_group, cause) (increase(actions_gateway_eviction_rerun_outcomes_total[7d]))
          ) > 0.5
            and
          sum by (namespace, runner_group, cause) (increase(actions_gateway_eviction_rerun_outcomes_total[7d])) >= 5
        for: 1h
        labels:
          severity: warning
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewayretrycausemisclassified"
          summary: "Most {{ $labels.cause }} re-runs fail for {{ $labels.runner_group }} in {{ $labels.namespace }}"
          description: "More than half of GAG's {{ $labels.cause }} re-runs on this set failed again over 7 days, so re-running is not fixing what that cause names. Investigate the jobs before trusting its automatic retries."

      # Ticket: quota retry budget exhausted — manual re-run required
      - alert: ActionsGatewayQuotaRetriesExhausted
        expr: |
          increase(actions_gateway_quota_retries_exhausted_total[5m]) > 0
        labels:
          severity: warning
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewayquotaretriesexhausted"
          summary: "Quota retry budget exhausted for {{ $labels.runner_group }} in {{ $labels.namespace }}"
          description: "A job was abandoned after its namespace ResourceQuota retry budget was exhausted. Raise the quota or lower maxWorkers, then re-run."

      # Page: the namespace ResourceQuota is rejecting worker pods now (Q82)
      - alert: ActionsGatewayWorkerQuotaExceeded
        expr: |
          actions_gateway_runnerset_worker_quota_exceeded == 1
        for: 5m
        labels:
          severity: critical
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewayworkerquotaexceeded"
          summary: "Worker pods being rejected by ResourceQuota for {{ $labels.runner_set }} in {{ $labels.namespace }}"
          description: "The namespace ResourceQuota cannot admit another worker pod; acquired jobs will fail to schedule. Raise the quota or reduce maxWorkers."

      # Page: the ResourceQuota is rejecting proxy replicas now (Q82)
      - alert: ActionsGatewayProxyQuotaExceeded
        expr: |
          actions_gateway_proxy_quota_exceeded == 1
        for: 5m
        labels:
          severity: critical
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewayproxyquotaexceeded"
          summary: "Proxy replica creation rejected by ResourceQuota for {{ $labels.name }} in {{ $labels.namespace }}"
          description: "The proxy pool is being held below the HPA's target by the namespace ResourceQuota. Raise the quota or lower the EgressProxy's spec.maxReplicas."

      # Ticket: capacity can't reach the configured ceiling within quota headroom (Q82)
      - alert: ActionsGatewayQuotaPressure
        expr: |
          actions_gateway_runnerset_worker_quota_pressure == 1
            or actions_gateway_proxy_quota_pressure == 1
        for: 15m
        labels:
          severity: warning
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewayquotapressure"
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
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewayreconcileerrors"
          summary: "Reconcile errors in {{ $labels.controller }}"
          description: "Reconcile errors at >2/minute for 10+ minutes. Resources may be stale."

      # Page: worker pods can't be scheduled — capacity is not materializing (Q157)
      - alert: ActionsGatewayWorkersUnschedulable
        expr: |
          actions_gateway_runnerset_workers_unschedulable == 1
        for: 10m
        labels:
          severity: critical
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewayworkersunschedulable"
          summary: "Worker pods unschedulable for {{ $labels.runner_set }} in {{ $labels.namespace }}"
          description: "Worker pods are stuck Pending past the scheduling grace because the scheduler can't place them (no matching node / affinity / taints — not quota). Capacity is not materializing; acquired jobs will not start."

      # Page: worker pods bound to a node and the kubelet cannot start them (Q906)
      - alert: ActionsGatewayWorkersNotStarting
        expr: |
          actions_gateway_runnerset_workers_not_starting == 1
        for: 10m
        labels:
          severity: critical
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewayworkersnotstarting"
          summary: "Worker pods will not start for {{ $labels.runner_set }} in {{ $labels.namespace }}"
          description: "Worker pods were placed on a node and the kubelet could not start them — almost always a workerImage that will not pull. The RunnerSet's WorkersNotStarting condition message carries the kubelet's own text, which names the image. Acquired jobs will not start, and without spec.capacityGate nothing throttles intake."

      # Page: the GitHub egress IP-range allowlist has gone stale (Q157)
      - alert: ActionsGatewayEgressRulesStale
        expr: |
          actions_gateway_egress_rules_stale == 1
        for: 15m
        labels:
          severity: critical
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewayegressrulesstale"
          summary: "GitHub egress allowlist stale for {{ $labels.name }} in {{ $labels.namespace }}"
          description: "The gateway's GitHub egress IP-range allowlist has not refreshed within the staleness window; a stalled refresh loop may let the proxy NetworkPolicy drift from GitHub's published ranges, silently dropping new ranges. Reason RefreshNeverSucceeded means no ranges have loaded at all and GitHub egress is already denied."

      # Page: a direct-egress v2 gateway's GitHub allowlist has gone stale (Q1249)
      - alert: ActionsGatewayDirectEgressRulesStale
        expr: |
          actions_gateway_direct_egress_rules_stale == 1
        for: 15m
        labels:
          severity: critical
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewaydirectegressrulesstale"
          summary: "GitHub egress allowlist stale for direct-egress gateway {{ $labels.name }} in {{ $labels.namespace }}"
          description: "The GitHub IP-range allowlist on a direct-egress gateway's AGC and workload NetworkPolicies has not refreshed within the staleness window, so new GitHub ranges may be silently dropped. When the GMC serves no ranges, the condition message says whether each policy still keeps the rules it was last programmed with or carries no GitHub rule, which denies GitHub egress."

      # Ticket: a GHES tenant's egress allowlist can't reach its appliance (Q506, Q537)
      - alert: ActionsGatewayGitHubEgressIncomplete
        expr: |
          actions_gateway_github_egress_incomplete == 1
        for: 15m
        labels:
          severity: warning
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewaygithubegressincomplete"
          summary: "GHES egress incomplete for {{ $labels.name }} in {{ $labels.namespace }}"
          description: "A referring gateway names a GitHub Enterprise Server host the pool's egress allowlist cannot reach, so the tenant's GitHub traffic is denied and it acquires no jobs. The condition's reason names the fix: ApplianceRangesRequired needs the appliance's ranges in spec.destinationCIDRs or an FQDN egress mode, and GitHubHostNotAllowlisted needs the host on the platform --allowed-egress-fqdns allowlist. Neither self-heals."

      # Page: two tenants are driving one scale set at GitHub right now (Q791, Q849)
      - alert: ActionsGatewayScaleSetNameCollision
        expr: |
          actions_gateway_scale_set_name_collision == 1
        for: 5m
        labels:
          severity: critical
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewayscalesetnamecollision"
          summary: "Scale-set name shared across tenants for {{ $labels.name }} in {{ $labels.namespace }}"
          description: "A ScaleSet RunnerSet bound to this gateway claims a scale-set name another RunnerSet already claims in the same GitHub scope, so both tenants' AGCs drive one scale set and each acquires the other's jobs. Admission rejects every new such pair, so this one predates the guard or was applied with the webhook uninstalled; it will not self-heal."

      # Ticket: egress proxy denying CONNECTs to off-allowlist destinations —
      # an SSRF / egress-policy signal, sharper than dial_errors (Q316)
      - alert: ActionsGatewayProxyConnectDenied
        expr: |
          rate(actions_gateway_proxy_connect_denied_total[5m]) > 0.1
        for: 10m
        labels:
          severity: warning
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewayproxyconnectdenied"
          summary: "Egress proxy denying CONNECTs in {{ $labels.namespace }}"
          description: "The egress proxy is refusing CONNECT requests to off-allowlist destinations at >0.1/s for 10m — an SSRF / egress-policy signal (a workload probing blocked destinations, or a misconfigured egress target). Unlike dial errors, every denial here is an explicit allowlist rejection."

      # Page: scale-set tier wedged — jobs are being assigned but none are
      # getting provisioned (Q311). A throughput stall is visible as demand
      # (assigned) flowing while supply (provisioned) is flat.
      # A listener whose poll keeps failing is assigned nothing, which this
      # cannot see; ActionsGatewayMessagePollFailing covers that case, and
      # ActionsGatewayScaleSetNoSession covers a session that never opens.
      - alert: ActionsGatewayScaleSetProvisioningStalled
        expr: |
          (
            sum by (namespace, runner_set) (
              rate(actions_gateway_scaleset_jobs_assigned_total[15m])
            ) > 0
          )
          unless
          (
            sum by (namespace, runner_set) (
              rate(actions_gateway_scaleset_jobs_provisioned_total[15m])
            ) > 0
          )
        for: 10m
        labels:
          severity: critical
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewayscalesetprovisioningstalled"
          summary: "Scale-set provisioning stalled for {{ $labels.runner_set }} in {{ $labels.namespace }}"
          description: "The scale-set acquisition tier is receiving JobAssigned messages but has provisioned no worker pods for 10+ minutes — the tier is wedged. Acquired jobs will not start. Check actions_gateway_scaleset_provision_errors_total, the worker-pod ResourceQuota, and the listener session health."

      # Ticket: the listener's long poll keeps failing, so no job reaches it
      # (Q1275). A set whose poll fails is assigned nothing, so ProvisioningStalled's
      # demand conjunct never holds. A session that never opens counts no poll
      # error; ActionsGatewayScaleSetNoSession covers that case.
      #
      # The threshold is derived from the listener's constants, not from live
      # traffic: a fully black-holed poll times out at 55s (the 50s hold plus
      # 5s slack) and retries after 2s, about 15 errors per 15m per listener,
      # and a fast-failing transport error retries every 2s. Above 10 in 15m
      # is most of one listener's polls failing; a lone blip never reaches it.
      # Grouped by namespace because the series carries no runner_set, so a
      # namespace with several sets reaches the threshold sooner. 401/403
      # and 404/410 are heal paths the counter skips, so a rejected credential
      # shows up as ActionsGatewayTokenRefreshErrors or Degraded=True instead.
      - alert: ActionsGatewayMessagePollFailing
        expr: |
          sum by (namespace) (
            increase(actions_gateway_message_poll_errors_total[15m])
          ) > 10
        for: 10m
        labels:
          severity: warning
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewaymessagepollfailing"
          summary: "Listener long polls failing in {{ $labels.namespace }}"
          description: "GetMessage has failed more than 10 times in 15 minutes, sustained for 10 minutes: most of a listener's polls are failing, so GitHub is not delivering jobs to it and they queue at GitHub. Read the reason label on actions_gateway_message_poll_errors_total: timeout is a black-holed broker connection, rate_limited a 429, other any remaining transport error."

      # Page: a scale-set listener holds no message session, so GitHub delivers it
      # nothing and its jobs queue indefinitely (Q1275): Start failing at any
      # step, or the re-create after a 404 failing inside the poll loop. Neither
      # counts a poll error or assigns a job, so the two rules above cannot see
      # it. The gauge is published by the listener and dropped when the
      # RunnerSet is deleted. `for` is 10m because a rollout's successor waits on its predecessor's session and reads 0 until
      # the old pod exits.
      - alert: ActionsGatewayScaleSetNoSession
        expr: |
          actions_gateway_scaleset_session_open == 0
        for: 10m
        labels:
          severity: critical
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewayscalesetnosession"
          summary: "Scale-set listener has no session for {{ $labels.runner_set }} in {{ $labels.namespace }}"
          description: "The scale-set listener could not open its message session for 10+ minutes, so GitHub delivers it no jobs and they queue indefinitely. The RunnerSet reports Ready=False with reason NoActiveSessions and a ScaleSetListenerStartFailed Event carrying the cause, or the AGC log shows the session re-create failing."

      # Ticket: scale-set provision attempts failing at a sustained rate (Q311)
      - alert: ActionsGatewayScaleSetProvisionErrors
        expr: |
          rate(actions_gateway_scaleset_provision_errors_total[5m]) > 0.1
        for: 10m
        labels:
          severity: warning
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewayscalesetprovisionerrors"
          summary: "Scale-set provision errors for {{ $labels.runner_set }} in {{ $labels.namespace }}"
          description: "The scale-set acquisition tier is failing to provision worker pods (JIT-config mint or pod create) at >0.1/s for 10m. A transient failure retries on a later poll, but a sustained rate means provisioning is degraded — check the run service's generate-jitconfig responses and namespace quota headroom."

      # Ticket: a job's runner name will not register, so nothing can run it (Q551).
      # Scoped to reason="name_conflict": reason="ceiling" is a set running at the
      # concurrency its spec declares, which is not an incident (Q576).
      - alert: ActionsGatewayScaleSetJobsDeferred
        expr: |
          actions_gateway_scaleset_jobs_deferred{reason="name_conflict"} > 0
        for: 15m
        labels:
          severity: warning
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewayscalesetjobsdeferred"
          summary: "Scale-set job cannot register a runner name for {{ $labels.runner_set }} in {{ $labels.namespace }}"
          description: "One or more jobs assigned to the scale set cannot register their runner name (generate-jitconfig 409), so no worker is running them; the listener keeps re-offering them. Each is a workflow run queued at GitHub. Read the RunnerSet's JobProvisionStalled condition for the job ids, then free the conflicting runner records."

      # Ticket: an opted-in capacity gate is holding back job intake (Q695). A
      # job the ladder declines is never assigned, so the withheld gauge is the
      # signal. That gauge alone cannot say the gate is costing anything: an idle set
      # whose worker shape stays unplaceable holds a latched AwaitingProbe
      # decline indefinitely, truthfully and harmlessly (Q512, Q658). Recent
      # assignments are the demand conjunct that tells the two apart — under a
      # full withhold the per-window probe job still lands, so a set with work
      # waiting keeps this counter moving while an idle one does not.
      - alert: ActionsGatewayScaleSetCapacityWithheld
        expr: |
          actions_gateway_scaleset_capacity_withheld{reason="capacity"} > 0
          and on (namespace, runner_set)
          (
            increase(actions_gateway_scaleset_jobs_assigned_total[1h]) > 0
          )
        for: 30m
        labels:
          severity: warning
        annotations:
          runbook_url: "https://actions-gateway.com/operations/runbook/#actionsgatewayscalesetcapacitywithheld"
          summary: "Capacity gate withholding advertised capacity for {{ $labels.runner_set }} in {{ $labels.namespace }}"
          description: "The opt-in capacity gate has held slots back from the set's advertised ceiling for 30+ minutes while GitHub was still assigning it work, so runs are waiting on capacity the set is declining to advertise. Read the reason label on actions_gateway_runnerset_worker_capacity_declined for the evidence, and actions_gateway_scaleset_advertised_capacity for what is left."
```

---

## SLO Recording Rules

These recording rules pre-compute the metrics needed for burn-rate alerting against the SLO targets in [Appendix A](../design/appendix-a-capacity-slos.md).
Apply them alongside the alert rules above.

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

      # Scale-set provision success rate — fraction of provision attempts that
      # succeeded, per namespace: a provision attempt either succeeds
      # (…_jobs_provisioned_total) or fails (…_provision_errors_total).
      - record: actions_gateway:scaleset_provision_success_rate:rate5m
        expr: |
          sum by (namespace) (
            rate(actions_gateway_scaleset_jobs_provisioned_total[5m])
          )
          /
          (
            sum by (namespace) (
              rate(actions_gateway_scaleset_jobs_provisioned_total[5m])
            )
            +
            sum by (namespace) (
              rate(actions_gateway_scaleset_provision_errors_total[5m])
            )
          )
```

---

← Back to [Observability](observability.md)
