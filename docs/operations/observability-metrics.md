# Metrics Reference

> **Audience:** SRE, Platform engineer

Part of the [Observability](observability.md) guide. To scrape these metrics, see [Accessing metrics (scraping setup)](observability-metrics-access.md); to alert on them, see [Alerting & SLOs](observability-alerting.md). For SLO targets, see [Appendix A — Capacity Targets & SLOs](../design/appendix-a-capacity-slos.md).

## Full Metrics Reference

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `actions_gateway_active_sessions` | Gauge | `namespace`, `runner_group` | Currently open long-poll sessions. One per RunnerGroup at steady state; rises toward `maxListeners` during bursts. |
| `actions_gateway_jobs_acquired_total` | Counter | `namespace`, `runner_group` | Jobs successfully acquired from the broker. |
| `actions_gateway_jobs_admission_rejected_total` | Counter | `namespace`, `runner_group`, `reason` | Delivered jobs the pre-acquisition capacity gate left queued at GitHub (acquire skipped). Expected to rise under sustained saturation; a persistent gap vs. `jobs_acquired_total` means demand exceeds available capacity. The `reason` label says which limit bound and therefore what to raise: `reason="ceiling"` — the owner is at its configured `maxWorkers` / max `priorityTiers` threshold (Q59); `reason="quota"` — the namespace `ResourceQuota` has no headroom for another worker pod, so the AGC declined to claim rather than claim-and-stall (#784). Read the owner's `WorkerQuotaExceeded` condition for the binding resource; `reason="capacity"` — the owner opted into `spec.capacityGate` and the cluster cannot currently *place* another worker pod of its shape, so the AGC declined rather than spend a JIT runner record on a pod that would be reaped (Q405, Q406). Read the set's `WorkerCapacityDeclined` condition for which signal said so: `reason: ScaleUpDeclined` is the cluster autoscaler's own declination (the default, where the gateway reports `clusterCapacity.nodeAutoscaling: Present`), `reason: PodsUnschedulable` is the scheduler's verdict (`Absent`), and `reason: AwaitingProbe` is the latched state — the declined pods were reaped and intake is limited to one probe job per `pendingPodDeadline` window until a worker pod schedules (Q512). One label value covers both on purpose — the rung, the refusal, and the operator's remedy are the same; only the evidence differs, and the condition carries that. Off by default: this series is absent until a `RunnerSet` sets `spec.capacityGate.mode`. **Classic acquisition only** — this is the *per-delivered-job* form of the gate, so both `reason` series read a flat zero on a `ScaleSet` set. That set enforces the same ladder as a capacity integer instead, and its equivalents are [`scaleset_advertised_capacity` / `scaleset_capacity_withheld`](#scale-set-acquisition-tier-q264) (Q443). |
| `actions_gateway_jobs_duplicate_delivery_total` | Counter | `namespace`, `runner_group` | Duplicate job deliveries deduplicated (Q260): the broker delivered the same job (same `planID`, distinct `RunnerRequestID`) to more than one sibling session and this one skipped provisioning — recycling its runner instead — because the `planID` was already claimed in this AGC. The dedup keys on `planID` (only known post-`acquirejob`), so a deduplicated delivery still ran `acquirejob`; the win is that it does not collide on the shared `job-<planID>` worker Secret or the winner's `runner-…-<planID>` pod. Two cases both count here: a **concurrent burst** (a sibling is provisioning the `planID` right now) and a **late redelivery** (the winner already completed, but its terminal worker pod has not yet been reaped, so the claim is retained for `completedPodTTL` past completion to keep deduping — otherwise the redelivery would re-provision and hit `create Pod … already exists`). A steady low rate during bursts is normal and benign — the gate is protecting runner slots. A sudden spike proportional to a stalled matrix indicates heavy fan-out; correlate with `jobs_acquired_total` (which should keep climbing) to confirm work is still being provisioned. |
| `actions_gateway_abandoned_delivery_completions_total` | Counter | `namespace`, `runner_group`, `outcome` | A `completejob` the AGC issues on a deduped sibling delivery of a fanned-out job — released by the winner on completion or on a late redelivery within the linger window (Q260 Option A) — so GitHub does not cancel the whole job at its ~15-minute unstarted-job timeout. A session's **own** delivery whose worker was removed before any container started is deliberately *not* completed and does not count here: every accepted `completejob` value concludes the run as `success`, a false green for a job that never ran (measured, Q645/Q676). `outcome="completed"` resolved the assignment (or found it already gone server-side); `outcome="error"` failed and the job may still be cancelled. **On by default** (opt out with `AGC_FANOUT_COMPLETION=false`), so a steady `completed` rate under concurrent bursts is normal. A rising `error` rate warrants investigating the run service's `completejob` responses. |
| `actions_gateway_fanout_loser_recycle_deferred_total` | Counter | `namespace`, `runner_group`, `outcome` | A deduped fan-out loser **deferring its slot recycle until its winner concluded** (Q266). A loser ran `acquirejob`, so GitHub holds its runner as assigned to the job; its recycle would `422` ("runner is currently running a job and cannot be deleted") for the winner's whole runtime — past the bounded recycle backoff — so recycling eagerly would exit the listener and, under sustained burst, collapse the pool. Instead the loser holds its slot until the winner concludes (fanning `completejob` out to this delivery clears the `422`), then recycles in place. `outcome="winner_concluded"` is the normal path; `outcome="fallback_timeout"` means the winner never concluded within the bound and the loser recycled anyway (GitHub's unstarted-job timeout should have released the assignment by then) — a sustained rate here is **alert-worthy** (a class of stuck winners); `outcome="context_cancelled"` is AGC shutdown. Only emitted when fan-out completion (Q260 Option A) is enabled. |
| `actions_gateway_job_acquisition_errors_total` | Counter | `namespace`, `reason` | Acquisition failures. Reason values: `already_claimed` (benign race), `delivery_window_expired` (job redelivered), `version_too_old`, `other`. An `acquirejob` failure also emits a `JobAcquisitionFailed` Warning Event on the owning `RunnerGroup`/`RunnerSet` (Q170). |
| `actions_gateway_job_duration_seconds` | Histogram | `namespace`, `runner_group` | Wall time from `acquirejob` success to worker pod terminal phase. |
| `actions_gateway_pod_creation_latency_seconds` | Histogram | `namespace` | Time from worker pod creation to the runner container starting (scheduling + image pull). Key SLO metric — see [Appendix A](../design/appendix-a-capacity-slos.md). |
| `actions_gateway_token_refreshes_total` | Counter | `namespace` | Successful GitHub App installation token refreshes. |
| `actions_gateway_token_refresh_errors_total` | Counter | `namespace` | Failed token refresh attempts. See SLO threshold below. |
| `actions_gateway_renew_job_errors_total` | Counter | `namespace` | Failed `renewjob` calls. Leading indicator for cancelled jobs. (Renamed from `…_renewjob_errors_total` in Q205 — see [Breaking observability changes](#breaking-observability-changes-q205).) |
| `actions_gateway_renew_job_teardowns_total` | Counter | `namespace`, `reason` | Workers self-cancelled because the job's lock was definitively lost (Q254), avoiding an orphan pod — each also deletes the worker pod and increments `worker_pods_reaped_total{reason="job_abandoned"}` (Q501). `reason="job_not_found"` is a definitive 404/410 from the run service (job recycled/reassigned); `reason="consecutive_failures"` is 5 consecutive renewal failures (~5 min). See the [runbook](troubleshooting.md#renewjob-failures-rising). |
| `actions_gateway_eviction_retries_total` | Counter | `namespace`, `runner_group`, `tier`, `cause` | Recoveries **started** after a worker pod disruption — one per reserved retry-budget slot, incremented before the API calls. A recovery now outlasts GitHub's refusal window: the re-run is retried while GitHub answers `403 This workflow is already running` (which after an ungraceful eviction lasts until the job lock's TTL lapses, ~10 minutes — Q503), so in steady state each increment corresponds to a re-run that eventually lands. The recoveries that instead never landed are counted by `actions_gateway_eviction_rerun_failures_total` below — read the two together: `retries_total − rerun_failures_total` is the honest recovery count. `tier="classic"` is the classic acquisition path, where the goroutine that acquired the job watches its own worker pod; `tier="scaleset"` is the scale-set path, where the owning reconciler detects the disruption from the worker pod itself (Q417). `cause="eviction"` is the kubelet's node-pressure eviction; `cause="preemption"` is kube-scheduler displacing the worker for a higher `priorityTiers` tier (Q497); `cause="deletion"` is an external graceful deletion — a drain or a `kubectl delete pod` — whose worker published a terminal phase carrying the deletion mark (Q502; graceful too, with a measured 15–26s conclusion latency, so the retry loop lands it within a few paced attempts). The retry budget is **shared** — it is keyed by run ID alone, so `maxEvictionRetries` bounds re-runs per run across both tiers **and all causes** together, not once per combination. Labels added in Q417 (`tier`) and Q497 (`cause`); see [Breaking observability changes](#breaking-observability-changes-q417). |
| `actions_gateway_eviction_retries_exhausted_total` | Counter | `namespace`, `runner_group`, `tier`, `cause` | Disruption retries exhausted; job requires manual re-run. Each occurrence also emits an `EvictionRetriesExhausted` Warning Event on the owning `RunnerGroup`/`RunnerSet` (Q170). `tier` and `cause` as above. |
| `actions_gateway_eviction_rerun_failures_total` | Counter | `namespace`, `runner_group`, `tier`, `cause`, `reason` | Disruption recoveries whose re-run was **never accepted** by GitHub, so the budget slot is spent but the job was not re-run and needs a manual `gh run rerun` (Q503). `reason="run_never_concluded"`: GitHub was still answering `403 This workflow is already running` when the 15-minute re-run window closed — the original run outlived the job lock's ~10-minute TTL bound, which is itself worth investigating. `reason="api_error"`: a terminal API failure (a non-403 error, or a 403 that is a permissions problem rather than the still-running refusal). Each occurrence also emits an `EvictionRerunFailed` Warning Event naming the run. **Expected to be zero**; see the [runbook](troubleshooting.md#evicted-worker-pods-exhausting-retry-budget). |
| `actions_gateway_eviction_recovery_identity_unknown_total` | Counter | `namespace`, `runner_group`, `cause` | Disrupted **scale-set** worker pods that carried no workflow-run identity, so no automatic re-run could be attempted and the job stays failed until a human re-runs it (Q417). Each occurrence also emits an `EvictionRecoveryIdentityUnknown` Warning Event on the owning `RunnerSet`. This is the one failure mode that makes scale-set eviction recovery silently inert, which is why it is counted separately from an exhausted budget: an exhausted budget means a tenant is evicting more than `maxEvictionRetries` allows, while this means GitHub did not send the assignment fields (`ownerName`, `repositoryName`, `workflowRunId`) the mechanism reads. **Expected to be zero** — the assignment fields were confirmed present on live GitHub on 2026-07-26, so a sustained rate is a protocol-level regression, not a capacity problem — see the [runbook](troubleshooting.md#evicted-scale-set-jobs-are-not-re-run-automatically). |
| `actions_gateway_quota_retries_total` | Counter | `namespace`, `runner_group` | Pod creation attempts retried after the namespace `ResourceQuota` rejected the worker pod. A brief non-zero rate under burst is normal (the listener backs off and retries); a sustained rate means quota headroom is tight — raise the quota or lower `maxWorkers`. |
| `actions_gateway_quota_retries_exhausted_total` | Counter | `namespace`, `runner_group` | Quota retries exhausted; the job was abandoned after the quota retry budget ran out and requires a manual re-run. |
| `actions_gateway_worker_pods_reaped_total` | Counter | `namespace`, `runner_group`, `runner_set`, `reason` | Worker pods the AGC deleted — the lifecycle reaper's five reasons, plus the job-abandoned reclaim. `runner_group` carries the owning CR's name on **both** acquisition tiers (a `RunnerSet`'s reaps land there too, unchanged, so existing `runner_group`-keyed queries keep working); `runner_set` additionally carries the set name on scale-set reaps and is empty on classic ones (added in Q514), so the reap series join the [`runner_set`-labelled `scaleset_*` gauges](#scale-set-acquisition-tier-q264) on `(namespace, runner_set)` — filter with `{runner_set!=""}` for the scale-set-only view. `reason="completed_ttl"` is routine cleanup after `completedPodTTL`; `reason="pending_deadline"` means a pod was stuck Pending past `pendingPodDeadline` and its job was cancelled — each such reap also emits a `WorkerPodStuckPending` Warning Event on the RunnerGroup. `reason="completed_pending"` means a pod was still Pending thirty seconds after GitHub reported its job terminal — the job ended before the pod could start, so the pod had nothing to run and (on the scale-set tier) no longer has the JIT-config Secret it mounts — and emits a `WorkerPodCompletedPending` Warning Event; it is deliberately distinct from `pending_deadline`, which would send you after a scheduling problem that does not exist; see the [runbook](troubleshooting.md#worker-pod-reaped-while-pending-after-its-job-completed-workerpodcompletedpending). `reason="orphaned_running"` means a pod was still Running five minutes after GitHub reported its job terminal — a ScaleSet worker that registered but never received its job, or a pod held open by a container that outlived the runner — and emits a `WorkerPodOrphanedRunning` Warning Event; see the [runbook](troubleshooting.md#worker-pod-reaped-while-running-workerpodorphanedrunning). `reason="lifetime_exceeded"` means the kubelet killed the pod for outliving `maxWorkerLifetime` (default 12h, the pod's `activeDeadlineSeconds`) and emits a `WorkerPodLifetimeExceeded` Warning Event — the unconditional backstop for a worker orphaned while the AGC was down, and the one reap reason that fires with no AGC running; see the [runbook](troubleshooting.md#worker-killed-by-the-lifetime-cap-workerpodlifetimeexceeded). `reason="gateway_deleted"` means the pod's `ActionsGateway` is being deleted: the AGC is the pods' only reaper and is torn down with the gateway, so it stops acquiring and deletes them itself rather than strand them — any job they were running is lost, and a single `WorkerPodsReapedOnGatewayTeardown` Warning Event names the count; see the [runbook](troubleshooting.md#worker-pods-reaped-on-gateway-teardown-workerpodsreapedongatewayteardown). `reason="job_abandoned"` is the one non-reaper source: the classic-tier provisioner reclaiming the worker of a job whose lock was definitively lost, immediately after the matching `renew_job_teardowns_total` increment — it should track that counter, and see the [runbook](troubleshooting.md#renewjob-failures-rising). |
| `actions_gateway_worker_scaleup_throttled_total` | Counter | `namespace`, `runner_group` | Worker-pod creations delayed by the opt-in per-RunnerGroup scale-up rate limit (`spec.scaleUp`): the token bucket was empty so the acquired job waited for a token before its pod was created (Q223). **Zero unless a group sets `scaleUp`** — it is default-off. A sustained rate means the ramp is actively smoothing a cold-start burst on a shared egress path (NAT/firewall/VPN); that is the knob doing its job, not an error. If it is *persistently* high, the ramp may be holding already-claimed jobs too long — raise `maxPerSecond`/`burst`, or confirm a rate limit is the right tool for the burst (see [tenant-onboarding: worker scale-up rate limit](tenant-onboarding.md#step-2-create-the-actionsgateway-resource)). |
| `actions_gateway_message_poll_errors_total` | Counter | `namespace`, `reason` | `GetMessage` errors (excludes empty polls and session expiry — those are normal). `reason="rate_limited"` is a 429; `reason="timeout"` is a black-holed long-poll the broker accepted but never answered, bounded by the client response-header deadline and retried (see [Listener Stalls After a Black-Holed Broker Connection](troubleshooting.md#listener-stalls-for-minutes-after-a-black-holed-broker-connection)); `reason="other"` is any remaining transport/decode error. **Both acquisition tiers** — a `ScaleSet` set writes the same `namespace`/`reason` series as a Classic group (Q446), so one query covers a mixed fleet and keeps working after Classic is removed. Credential rejection (401/403) and session expiry (404/410) are *not* counted on either tier: both are heal paths, not poll failures. On a `ScaleSet` set the counter is the rate-able half of a picture the conditions complete — `Degraded`/`Unauthorized` on a rejected session refresh, and `RateLimited` once a 429 episode outlasts ten minutes — so a stream of brief episodes shows up here even though no condition trips. |
| `actions_gateway_agent_recycles_total` | Counter | `namespace`, `runner_group`, `trigger` | Single-use JIT agents re-registered. `trigger="post_job"` is routine (one per completed job); `stale_session`/`startup` mean a dead agent was detected and healed after the fact; `reconcile_repair` means a parked agent was repaired by the reconciler. |
| `actions_gateway_agent_recycle_errors_total` | Counter | `namespace`, `runner_group` | Failed agent re-registration attempts. Sustained growth shrinks listener capacity — see the [runbook](troubleshooting.md#sessions-stuck-in-401eof-getmessage-loops-tenant-throughput-decays-to-zero). |
| `actions_gateway_broker_session_leaks_total` | Counter | `namespace`, `runner_group` | Broker sessions the AGC gave up deleting: every `DELETE /sessions` attempt failed (3 tries inside a 10 s budget), so the session stays registered at GitHub until it expires server-side (Q436). The listener recovers either way — it opens a fresh session and keeps its polling slot — so this is **not** an availability alarm on its own. A one-off during a GitHub blip or a fleet-wide teardown is expected. A **sustained** rate means the tenant is accumulating server-side sessions nobody polls, and points at a slow or unreachable broker on the control-plane path: check `actions_gateway_message_poll_errors_total{reason="timeout"}` and the egress proxy for the same tenant. |
| `actions_gateway_broker_token_propagation_retries_total` | Counter | `namespace`, `runner_group` | Broker OAuth token-exchange retries a freshly recycled agent made while GitHub's token endpoint still returned a transient `400 "Registration … was not found"` for its just-created runner record (the `generate-jitconfig` → OAuth-service propagation window, Q267). The listener rides these out with a bounded, jittered backoff instead of exiting and churning a new record. A brief non-zero blip during a burst is normal; a **sustained** rate means wide-pool recycle churn is repeatedly hitting the propagation seam — see the [runbook](troubleshooting.md#concurrent-job-burst-serializes-to-1-worker-recycle-blocked-on-a-still-running-runner). |
| `actions_gateway_worker_quota_pressure` | Gauge | `namespace`, `runner_group` | `1` when `WorkerQuotaPressure=True` (Q82): workers can't scale to the configured ceiling within the namespace `ResourceQuota` headroom. Warning — load-dependent; alert with `for:`, don't page. |
| `actions_gateway_worker_quota_exceeded` | Gauge | `namespace`, `runner_group` | `1` when `WorkerQuotaExceeded=True` (Q82): the `ResourceQuota` can't admit another worker pod — the next acquired job's pod will be rejected. Error — page. |
| `actions_gateway_workers_unschedulable` | Gauge | `namespace`, `runner_group` | `1` when `WorkersUnschedulable=True` (Q157): worker pods are stuck Pending past the scheduling grace because the scheduler can't place them (no matching node / affinity / taints — **not** quota, which `WorkerQuotaExceeded` covers). Capacity is not materializing — page if sustained. The stuck pods and the scheduler verdict are named in the condition message. |
| `actions_gateway_runnerset_worker_quota_pressure` | Gauge | `namespace`, `runner_set` | `1` when `WorkerQuotaPressure=True` on a v2 `RunnerSet` (Q303, exported in Q319) — the per-set twin of `actions_gateway_worker_quota_pressure`, same semantics and same warning grade. Alert with `for:`, don't page. |
| `actions_gateway_runnerset_worker_quota_exceeded` | Gauge | `namespace`, `runner_set` | `1` when `WorkerQuotaExceeded=True` on a v2 `RunnerSet` — the per-set twin of `actions_gateway_worker_quota_exceeded`: the `ResourceQuota` can't admit another worker pod. Error — page. On a `ScaleSet` set the same headroom also drives `scaleset_capacity_withheld{reason="quota"}`, but the two are not equivalent: that gauge counts the slots quota removed from the ceiling, while this one trips only once there is no room for even one more worker pod. |
| `actions_gateway_runnerset_workers_unschedulable` | Gauge | `namespace`, `runner_set` | `1` when `WorkersUnschedulable=True` on a v2 `RunnerSet` — the per-set twin of `actions_gateway_workers_unschedulable`: worker pods are stuck Pending past the scheduling grace because the scheduler can't place them (**not** quota). Page if sustained; the stuck pods and the scheduler verdict are named in the condition message. |
| `actions_gateway_runnerset_worker_capacity_declined` | Gauge | `namespace`, `runner_set`, `reason` | `1` when `WorkerCapacityDeclined=True` on a v2 `RunnerSet` (Q405, Q406; gauged in Q643): the opt-in capacity gate is refusing job intake because the cluster cannot place another worker pod of this set's shape. **Emitted only for a set that set `spec.capacityGate.mode`** — a set with no gate carries no condition, and an absent series is what says so; a `0` would read as "gate evaluated, capacity available". `reason` is the condition's current reason and is the label to alert and group on: `PodsUnschedulable` is the scheduler's verdict (a fixed-size cluster, `clusterCapacity.nodeAutoscaling: Absent`), `ScaleUpDeclined` is the cluster autoscaler's own declination (the default, `Present`), and `AwaitingProbe` is the **latched** state (Q512) — the declined pods were reaped, so intake is throttled to one probe job per `pendingPodDeadline` window until a worker pod schedules. `CapacityAvailable` and `GateModeUnsupported` are the two `False` reasons and read `0`. Exactly one series exists per gated set: a reason change replaces it rather than adding one, so `max by (reason) (…) == 1` is safe. **The `AwaitingProbe` row is the one that needs the label.** It outlives the stuck pod that produced it, so `actions_gateway_runnerset_workers_unschedulable` has already fallen back to `0` while this reads `1` — an operator watching only the scheduler signal sees a recovered set whose intake is still throttled. |
| `actions_gateway_reap_blocking_sidecar_templates` | Gauge | `namespace`, `runner_set` | Number of regular (non-native) sidecar containers in a `RunnerSet`'s resolved worker template that may keep the worker pod alive after the runner container exits, stranding the runner slot against `maxWorkers` (Q249). `> 0` also sets the advisory `PossibleReapBlockingSidecar=True` condition on the `RunnerSet` naming the offending containers. Config warning, not load — fix the template: declare the sidecar as a native sidecar (`restartPolicy: Always` init container, Kubernetes ≥ 1.29) so the pod terminates when the runner exits, or, if it exits cleanly on its own, acknowledge it in the `actions-gateway.com/self-exiting-sidecars` annotation. Advisory — does not gate `Ready`. |
| `controller_runtime_reconcile_errors_total` | Counter | `controller` | GMC/AGC reconcile errors. Emitted by controller-runtime (no `actions_gateway_` prefix); the `controller` label distinguishes `actionsgateway`, `runnergroup`, etc. Non-zero values deserve investigation. |
| `actions_gateway_ip_range_updates_total` | Counter | `namespace` | `NetworkPolicy` egress rule refreshes from GitHub meta API. |
| `actions_gateway_managed_gateways` | Gauge | — | Total `ActionsGateway` CRs (v1 **and** v2) currently managed by the GMC (Q320). |
| `actions_gateway_proxy_quota_pressure` | Gauge | `namespace`, `name` | `1` when `ProxyQuotaPressure=True` (Q82): the proxy pool can't scale to `maxReplicas` within the namespace `ResourceQuota` headroom. Warning — alert with `for:`, don't page. `name` is the v1 `ActionsGateway` or, on a v2 deploy, the `EgressProxy` owning the pool (Q320). |
| `actions_gateway_proxy_quota_exceeded` | Gauge | `namespace`, `name` | `1` when `ProxyQuotaExceeded=True` (Q82): proxy replica creates are being rejected by the `ResourceQuota` now. Error — page. `name` is the v1 `ActionsGateway` or, on a v2 deploy, the `EgressProxy` owning the pool (Q320). |
| `actions_gateway_runnergroups_degraded` | Gauge | `namespace`, `name` | `1` when `RunnerGroupsDegraded=True` (Q158): one or more of the gateway's owned `RunnerGroup`s report an impairing condition (`CredentialUnavailable`/`Degraded`/`RunnerVersionTooOld`/`WorkersUnschedulable`). Rolls child health up to the gateway; the impaired groups are named in the condition message. Advisory — does not gate `Ready`. v1 only — the v2 twin is `actions_gateway_runnersets_degraded` below. |
| `actions_gateway_egress_rules_stale` | Gauge | `namespace`, `name` | `1` when `EgressRulesStale=True` (Q157): the GitHub egress IP-range allowlist has not been refreshed within the staleness window (just over two of the ~24h refresh cycles), so a stalled refresh loop may have let the proxy `NetworkPolicy` drift from GitHub's published ranges. Advisory — does not gate `Ready`; page if sustained, as new GitHub ranges will be silently dropped. `name` is the v1 `ActionsGateway` or, on a v2 deploy, the CIDR-mode `EgressProxy` carrying the condition (an FQDN-mode proxy carries no refreshed CIDR rule, so it never trips) (Q320). |
| `actions_gateway_github_egress_incomplete` | Gauge | `namespace`, `name` | `1` when an `EgressProxy`'s `GitHubEgressIncomplete=True` (Q506): a referring gateway names a GitHub Enterprise Server host, and the pool's `CIDR`-mode allowlist carries only the ranges `api.github.com/meta` publishes — which never contain a customer appliance, so the tenant's GitHub traffic is denied. Advisory — does not gate `Ready`, but the affected tenant acquires no jobs; ticket it, as the fix is an operator config change (`spec.destinationCIDRs` or an FQDN egress mode) that will not self-heal. See [A GHES Tenant's Traffic Never Reaches the Appliance](troubleshooting.md#a-ghes-tenants-traffic-never-reaches-the-appliance). v2 only, emitted only on a v2 install (Q537). |
| `actions_gateway_runnersets_degraded` | Gauge | `namespace`, `name` | `1` when a v2 `ActionsGateway`'s `RunnerSetsDegraded=True` (Q304): one or more of the `RunnerSet`s bound to the gateway (`spec.gatewayRef`) report an impairing condition. The v2 twin of `actions_gateway_runnergroups_degraded`; rolls child health up to the gateway, naming the impaired sets in the condition message. Advisory — does not gate `Ready`. v2 only, emitted only on a v2 install (Q321). |
| `actions_gateway_agc_available` | Gauge | `namespace`, `name` | `1` when a v2 `ActionsGateway`'s `AGCAvailable=True`: the tenant's AGC `Deployment` has a ready replica (the gateway's control plane is up). Drops to `0` while the AGC is rolling out or unavailable — correlate with `Ready`. v2 only, emitted only on a v2 install (Q321). |
| `actions_gateway_egress_unattributed` | Gauge | `namespace`, `name` | `1` when a v2 `ActionsGateway`'s `EgressUnattributed=True` (§H.10): the gateway runs in **direct** egress mode, so its GitHub traffic is not attributed to a per-tenant egress proxy. Advisory — expected and `0` on a proxied deploy; a `1` on a deploy meant to be proxied flags a misconfiguration. v2 only, emitted only on a v2 install (Q321). |
| `actions_gateway_agc_autoscaling_unavailable` | Gauge | `namespace`, `name` | `1` when a v2 `ActionsGateway`'s `AGCAutoscalingUnavailable=True` (Q360, §E.11): the gateway opted into managed AGC right-sizing (`agcAutoscaling`) but it cannot be satisfied — the `VerticalPodAutoscaler` CRDs are not installed (`VPACRDNotInstalled`) or a precedence conflict blocks the managed VPA. Advisory — the AGC still runs on its stamped `agcResources` sizing and `Ready` is unaffected; the opt-in is simply inert until the blocker clears. `0` when satisfied or not opted in. Without this gauge the unsatisfiable opt-in is visible only via `kubectl describe` (Q390). v2 only, emitted only on a v2 install (Q321). |
| `actions_gateway_build_info` | Gauge | `component`, `version` | Constant `1` per running control-plane binary, following the Prometheus `*_build_info` convention (Q318). Emitted by the GMC, AGC, and proxy — `component` is `gmc`/`agc`/`proxy` and `version` is the build tag stamped into the binary (`dev` for un-stamped local builds). Not load-bearing for alerting; join it into other series to correlate the running version during an incident (worker pods carry `app.kubernetes.io/version`, but the control plane otherwise does not expose its version in metrics). |

> **Reading the eviction metrics across tiers and causes.** Both `eviction_retries_total`
> and `eviction_retries_exhausted_total` are emitted on **both** acquisition tiers, split
> by the `tier` label (Q417), and for the **three** recovered disruptions, split by the
> `cause` label (Q497, Q502). Detection differs across every combination — an inline pod
> wait on classic, the owning reconciler's recovery pass on scale-set; a `PodFailed`/
> `Evicted` phase for an eviction, a `DisruptionTarget` condition for a preemption, the
> pod's `deletionTimestamp` at terminal publish for an external deletion — but they
> share one budget, keyed by workflow run alone, so `maxEvictionRetries` caps re-runs
> per run across the whole set rather than once per combination.
>
> **The `cause` split is a diagnosis, not decoration.** A climbing `cause="eviction"`
> rate means node pressure: memory or disk exhaustion on the nodes, and the fix is
> capacity or worker sizing. A climbing `cause="preemption"` rate means a `priorityTiers`
> floor is displacing more opportunistic work than the tenant sized for, and the fix is
> tier thresholds or where the work is placed. A climbing `cause="deletion"` rate means
> something outside the gateway is deleting live workers — node drains from upgrades or
> autoscaler consolidation, a descheduler, or hand-run deletes — and the fix is finding
> the deleter. Reading one as another sends an operator hunting in entirely the wrong
> place.
>
> A flat zero on `tier="scaleset"` while workers are visibly being disrupted means the
> recovery is not firing, not that nothing happened. Check
> `kubectl get pods --field-selector=status.phase=Failed` for `Evicted` pods, then
> `actions_gateway_eviction_recovery_identity_unknown_total` and the
> [runbook](troubleshooting.md#evicted-scale-set-jobs-are-not-re-run-automatically).
> For preemption specifically, see
> [A Preempted Worker's Job Is Not Re-Run](troubleshooting.md#a-preempted-workers-job-is-not-re-run)
> — its scale-set path has a time limit the eviction path does not.

> **Proxy conditions on a v2 deploy.** On a v2 install (the opt-in
> `actions-gateway-crds-v2` CRDs), the GMC also counts v2 `ActionsGateway`s in
> `managed_gateways` and reflects each `EgressProxy`'s proxy conditions in
> `proxy_quota_pressure`, `proxy_quota_exceeded`, and `egress_rules_stale` — the
> EgressProxy reconciler sets those conditions with the same semantics as the v1
> `ActionsGateway` (a namespace-`ResourceQuota`-bounded, HPA-scaled pool whose default
> CIDR-mode `NetworkPolicy` is refreshed from the shared GitHub IP-range cache) (Q320).
> The v1 and v2 series share one metric family; the `name` label distinguishes them by
> object — both kinds carry the same `namespace`/`name` labels, so nothing about the v1
> series changes. The worker-capacity gauges below take the other route, and the note on
> them says why.
>
> `github_egress_incomplete` is the exception among the proxy gauges: its condition
> exists only on the `EgressProxy` — v1 has no twin — so the family is emitted only on
> a v2 install (Q537).
>
> **Worker-capacity conditions on v2 `RunnerSet`s.** The `WorkerQuotaPressure`,
> `WorkerQuotaExceeded`, and `WorkersUnschedulable` conditions are also set on a v2
> `RunnerSet` (Q303) with the same semantics as the v1 `RunnerGroup`, so a stalled
> set surfaces the capacity blocker in `.status.conditions` instead of only a rising
> `pendingJobs` with `Ready=True`. Each has its own gauge — the
> `actions_gateway_runnerset_*` triplet above (Q319) — so a `RunnerSet` is alertable
> without scraping CRD conditions through kube-state-metrics.
>
> **`WorkerCapacityDeclined` is gauged too, and differs in two ways** (Q643). It carries a
> `reason` label, because the value alone cannot separate a live decline from the latched
> `AwaitingProbe` state, and those call for different actions — one has stuck pods to
> inspect, the other has none and means intake is throttled to one probe job per
> `pendingPodDeadline` window. And it is emitted only for a set whose gate is on, because
> the reconciler *removes* the condition for an ungated set rather than publishing it
> `False`; the family follows the condition, so absence means "no gate here". The
> per-consequence series stay as they are: `jobs_admission_rejected_total{reason="capacity"}`
> counts jobs the classic tier left queued and `scaleset_capacity_withheld{reason="capacity"}`
> counts slots the scale-set tier withheld. Those answer "how much did the gate cost this
> tenant"; this gauge answers "is the gate closed right now, and on what evidence".
>
> **Why separate families instead of a `runner_set` label on the v1 gauges.** Unlike the
> proxy conditions above, the two objects do not share a label set: the v1 series key on
> `runner_group`, which a `RunnerSet` has none of. Folding both into one family would
> leave every set at `runner_group=""`, which silently breaks the
> `sum by (namespace, runner_group)` groupings the v1 series promise — every set would
> collapse into a single unnamed bucket — and would add an always-empty `runner_set`
> label to every existing v1 series. Separate names cost existing queries nothing, and a
> v2 dashboard selects on `runner_set` directly rather than filtering `{runner_set!=""}`
> on every query. The v1 families are unchanged and stay `RunnerGroup`-only.

> **`RunnerSetsDegraded` on a v2 `ActionsGateway`.** The v2 `ActionsGateway` carries a
> `RunnerSetsDegraded` condition (Q304) — the child-health rollup counterpart of the v1
> `RunnerGroupsDegraded` above. It is `True` when one or more of the `RunnerSet`s bound
> to the gateway (`spec.gatewayRef`) are impaired — not serving jobs: a non-transient
> `Ready=False` (a reference did not resolve or a provisioning step failed) **or** any
> abnormal-is-True impairing condition — `Degraded` (revoked/invalid credentials, pushed
> by the listener independently of `Ready`, Q330), `CredentialUnavailable`,
> `RunnerVersionTooOld`, or `WorkersUnschedulable`. The advisory conditions (`RateLimited`,
> the `WorkerQuota` ladder, `EgressUnattributed`, `PossibleReapBlockingSidecar`,
> `JobProvisionStalled`) are excluded so the rollup does not flap on normal load. The condition message names the impaired sets and their
> tripped signals, giving the operator a single pane without inspecting each child.
> Advisory — like the v1 rollup it does **not** gate `Ready`, since the gateway's own
> AGC control plane can be healthy while a tenant's set is impaired. It is exported as the `actions_gateway_runnersets_degraded`
> gauge (Q321), alongside `actions_gateway_agc_available`,
> `actions_gateway_egress_unattributed`, and `actions_gateway_agc_autoscaling_unavailable`
> for the gateway's `AGCAvailable`, `EgressUnattributed`, and `AGCAutoscalingUnavailable`
> conditions — the v2 twins of the v1 `ActionsGateway` condition gauges. Every v2
> gateway condition thus has a metric twin, so the advisory `agcAutoscaling` opt-in
> (Q360/Q390) is alertable rather than only visible via `kubectl describe`. All are
> emitted only on a v2 install and labelled per gateway (`namespace`, `name`).

### Scale-set acquisition tier (Q264)

These series are emitted **only** by a `RunnerSet` with `spec.acquisitionProtocol: ScaleSet` (Q264 Option E, the default since P5), which drives one runner-scale-set session per set — one job : one queue entry : one acquirer : one runner — instead of the classic many-acquirers pool. A `Classic` (deprecated) `RunnerSet` never touches them, so they read zero on a Classic-only deployment; the classic `actions_gateway_jobs_*` series above are what a Classic set emits. All are labelled per `RunnerSet` (`namespace`, `runner_set`). During the P4 dogfood validation (the Q224 fan-out acceptance gate) the counters are the primary signal that a scale-set set is assigning and provisioning jobs 1:1 with no fan-out.

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `actions_gateway_scaleset_jobs_assigned_total` | Counter | `namespace`, `runner_set` | Jobs the scale set's queue delivered as `JobAssigned` to the listener. Because the scale-set protocol assigns each job exactly once (no sibling fan-out), this tracks demand 1:1 — unlike the classic `jobs_acquired_total`, there is no duplicate-delivery series to correlate against. |
| `actions_gateway_scaleset_jobs_provisioned_total` | Counter | `namespace`, `runner_set` | Worker pods successfully provisioned, one per assigned job. A steady gap below `…_jobs_assigned_total` means provisioning is lagging or failing — correlate with `…_provision_errors_total` and the worker-pod `ResourceQuota` gauges. |
| `actions_gateway_scaleset_provision_errors_total` | Counter | `namespace`, `runner_set` | Failed provision attempts (JIT-config mint or worker pod create). A transient failure leaves the job un-provisioned to retry on a later poll. A `generate-jitconfig` **runner-name conflict** (HTTP 409) instead retries under a fresh runner name; if it still conflicts after a bounded number of tries the job is **deferred** (counted here once per round) so it cannot wedge the queue cursor behind it (Q270), and re-offered on a backoff until it runs — see `…_jobs_deferred` below. A job held because the set is at its **worker ceiling** is *not* counted here (Q576): it is backpressure rather than a failure, and `…_jobs_deferred{reason="ceiling"}` carries it. A sustained rate warrants checking the run service's `generate-jitconfig` responses and namespace quota headroom. |
| `actions_gateway_scaleset_jobs_completed_total` | Counter | `namespace`, `runner_set`, `result` | Terminal `JobCompleted` messages the queue delivered, by GitHub-reported `result` (e.g. `succeeded`, `failed`, `canceled`). This is the completion signal the classic many-acquirers protocol never delivered, so it is unique to the scale-set tier. Counted at most once per job even if a re-created session replays the message. |
| `actions_gateway_scaleset_jobs_deferred` | Gauge | `namespace`, `runner_set`, `reason` | Assigned jobs the listener is holding for a later re-offer, by why. Each one is a workflow run queued at GitHub with no worker running it, and it is the metric twin of the `RunnerSet`'s advisory `JobProvisionStalled` condition, whose message names the job ids. **Alert on the reason, not the total** — the two mean different things. `reason="name_conflict"`: the runner name will not register — a `generate-jitconfig` 409 that neither deleting the stale record nor a fresh suffixed name could clear (Q551). An anomaly; **any non-zero value is worth alerting on**, fix per [Scale-Set Job Stranded by a Stale Runner Record](troubleshooting.md#scale-set-job-stranded-by-a-stale-runner-record-runner-name-409). `reason="ceiling"`: the set is already running as many workers as its spec allows (Q576) — expected backpressure that clears as workers finish, so alert only on it being *sustained*, if at all; see [Scale-Set Jobs Waiting at the Worker Ceiling](troubleshooting.md#scale-set-jobs-waiting-at-the-worker-ceiling-workerceilingreached). Both reasons are published on every update, zero included, so a series never freezes at its last non-zero reading. Dropped when the `RunnerSet` is deleted. |
| `actions_gateway_scaleset_jobs_abandoned_total` | Counter | `namespace`, `runner_set` | Assigned jobs the listener gave up on because the scale set stopped counting them as assigned — GitHub is no longer holding the job and never reported it complete (Q553). Each is a **workflow run that will not run**, so unlike `…_jobs_deferred` this is a loss, not backpressure: it is what distinguishes a deferred set that cleared because its jobs ran from one that cleared because they evaporated. **Any non-zero value is worth alerting on**; see [Scale-Set Assignments Abandoned](troubleshooting.md#scale-set-assignments-abandoned-assignmentabandoned). It is expected to stay flat in steady state — a small burst around a mass run cancellation or a `stop.sh` drain is the designed behaviour, a sustained rate is not. |
| `actions_gateway_scaleset_advertised_capacity` | Gauge | `namespace`, `runner_set` | The `X-ScaleSetMaxCapacity` most recently advertised for the set: the total jobs GitHub may keep assigned to it at once. This is the scale-set tier's whole admission decision — the minimum of the declared worker ceiling, live namespace-`ResourceQuota` headroom (Q443), and — when the set opts in — its capacity gate (Q405). A value below the set's `maxWorkers` means a rung is binding; **`0` means GitHub will assign nothing at all** until it recovers. Dropped when the `RunnerSet` is deleted, so a stale series does not outlive the set. |
| `actions_gateway_scaleset_capacity_withheld` | Gauge | `namespace`, `runner_set`, `reason` | Slots the named rung removed from the declared ceiling on that same poll — `advertised_capacity` plus the sum of these equals the ceiling. `reason="quota"` is namespace-`ResourceQuota` headroom; `reason="capacity"` is the opt-in capacity gate, which bounds the total at the set's own in-flight workers while the cluster cannot place another (Q405) — plus one probe slot per `pendingPodDeadline` window while the gate is latched (`AwaitingProbe`, Q512), so under a sustained decline expect this series to *hold* near the ceiling across reap cycles rather than sawtooth back to `0`. Every evaluated rung publishes a value each poll, **including an explicit `0`**, so a series never sits frozen at its last non-zero reading — the capacity rung publishes its zero even with the gate `Off`, because the gate is per-set spec rather than a rung the AGC skips. Nothing is published for a rung an operator has turned off AGC-wide (`AGC_QUOTA_ADMISSION=false`). |

> **Why gauges and not a rejection counter.** On the classic tier a declined job is a
> delivered job, counted by `actions_gateway_jobs_admission_rejected_total{reason}`.
> On the scale-set tier the equivalent job is never assigned in the first place, so
> there is nothing to count — which is why that counter reads a flat zero here, and why
> these two gauges are its counterpart. Pair them with the set's
> `WorkerQuotaPressure`/`WorkerQuotaExceeded` conditions, which name the binding
> resource in their message.

### Worker usage / right-sizing metrics (Q359)

The AGC samples worker pod CPU/memory usage from the `metrics.k8s.io` API
(metrics-server) every 15s (`WORKER_USAGE_SAMPLE_INTERVAL` on the AGC
Deployment; `0`/`off` disables) and folds each finished pod's peak into these
series. One worker pod runs exactly one job, so a per-pod peak is a **per-job
peak**. Emitted for v2 `RunnerSet` workers only, labelled per RunnerSet and
container (bounded cardinality: one series per RunnerSet × container name).
These are the input to the [worker right-sizing recipe](worker-rightsizing.md);
without metrics-server they stay empty and `…_poll_errors_total` counts instead.

The same sampled history also drives two status surfaces on the v2 `RunnerSet`
(Q359 Phase 2): `status.sizingRecommendation` (per-container recommended
`requests`/`limits` with observed p95/max, sample count, and window) and the
advisory `SizingDrift` condition — `True` when, after ≥20 sampled jobs, the
template's ask is ≥2× the recommendation (waste) or a memory limit is below the
observed per-job peak (OOM risk). Advisory only; never gates `Ready`. A set
that opts into a sizing profile (`spec.sizing.profile`) additionally reports
`status.sizingProfileState` (`Active`/`AwaitingSamples`), and `SizingDrift`
reads `False/SizingProfileActive` while the profile actuates. A set on the
`Throughput` profile also carries the advisory `SizingProfileOverridden`
condition — `True` when a worker pod the profile built *without* a CPU limit was
admitted *with* one (a `LimitRange` cpu default, a mutating webhook, a policy
engine), which cancels the profile while rejecting nothing
([detail](worker-rightsizing.md#when-something-re-injects-the-cpu-limit-throughput-removes)).
See the
[right-sizing recipe](worker-rightsizing.md#step-0--read-the-built-in-recommendation-first).

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `actions_gateway_worker_usage_job_cpu_peak_cores` | Histogram | `namespace`, `runner_set`, `container` | Per-job CPU peak (cores), one observation per sampled job. `histogram_quantile` over a chosen window gives the p50/p95 the right-sizing derivation needs. |
| `actions_gateway_worker_usage_job_memory_peak_bytes` | Histogram | `namespace`, `runner_set`, `container` | Per-job memory peak (bytes), one observation per sampled job. |
| `actions_gateway_worker_usage_cpu_peak_cores` | Gauge | `namespace`, `runner_set`, `container` | Highest per-job CPU peak seen since AGC start — the absolute-max cross-check for the interpolated histogram quantiles. Resets on AGC restart (bridge with `max_over_time`). |
| `actions_gateway_worker_usage_memory_peak_bytes` | Gauge | `namespace`, `runner_set`, `container` | Highest per-job memory peak seen since AGC start. |
| `actions_gateway_worker_usage_jobs_sampled_total` | Counter | `namespace`, `runner_set` | Jobs that finished with at least one usage sample in the histograms. |
| `actions_gateway_worker_usage_jobs_unsampled_total` | Counter | `namespace`, `runner_set` | Jobs that finished before any sample landed (shorter than ~one sampling interval). A high ratio vs `…_jobs_sampled_total` means the histograms under-represent the workload. |
| `actions_gateway_worker_usage_poll_errors_total` | Counter | `namespace` | Failed `PodMetrics` list calls. A constant rate means usage is not being sampled at all — metrics-server missing or the RBAC grant absent; see [troubleshooting](worker-rightsizing.md#troubleshooting). |

### Proxy metrics

The per-tenant egress proxy exposes its own metrics on `:8443` over **mutual
TLS** — the same posture as the AGC (see [Scraping per-tenant AGC and proxy
metrics (mTLS)](observability-metrics-access.md#scraping-per-tenant-agc-and-proxy-metrics-mtls)), and
restricted by the L-8 NetworkPolicy (see [security.md L-8](../plan/security.md)).
The proxy's `:8081` port serves only the plaintext health probes (`/healthz`,
`/readyz`), not metrics. Each proxy is a separate scrape target; these metrics
carry no intrinsic `namespace` label. The GMC-generated per-tenant proxy
`ServiceMonitor` stamps one via a relabeling (`namespace` ← the scrape target's
namespace, which is the tenant's namespace), so the tenant Grafana dashboard's
proxy panels filter by `$namespace` for per-tenant attribution. If you scrape the
proxy with a hand-written scrape config instead of the generated `ServiceMonitor`,
add the equivalent relabeling to get the `namespace` label.

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `actions_gateway_proxy_connections_active` | Gauge | `namespace`¹ | Currently open CONNECT tunnels. |
| `actions_gateway_proxy_connections_total` | Counter | `namespace`¹ | Total CONNECT tunnels opened. |
| `actions_gateway_proxy_dial_errors_total` | Counter | `namespace`¹ | Upstream dial failures (e.g. transient network errors reaching an allowed destination). |
| `actions_gateway_proxy_connect_denied_total` | Counter | `namespace`¹ | CONNECT requests refused because the destination is not on the egress allowlist. A precise Server-Side Request Forgery (SSRF) / egress-policy signal: unlike `…_dial_errors_total` (which also counts transient dial failures to *allowed* hosts), every increment here is an explicit allowlist denial — a workload attempting to reach a blocked destination. A sustained rate is alert-worthy; see [security-operations.md § Threat → signal map](security-operations.md#threat--signal-map). |
| `actions_gateway_proxy_tunnel_duration_seconds` | Histogram | `namespace`¹ | Tunnel lifetime, observed at close. Buckets reach 21600s (the 6h absolute lifetime cap). |

¹ Not exposed by the proxy itself — added by the per-tenant `ServiceMonitor`
relabeling described above. Absent if you scrape without that relabeling.

For abuse/compromise detection built on these metrics (slowloris,
eviction-retry loops, credential-harvesting), see
[security-operations.md](security-operations.md).

---

## CRD Status Fields (kubectl columns)

`kubectl get runnergroup` and `kubectl get runnerset` print a subset of each CR's
`.status` as additional columns. These give an at-a-glance view of live job state
without opening Grafana:

| Column | Field | RunnerGroup | RunnerSet | Description |
| --- | --- | --- | --- | --- |
| `ACTIVESESSIONS` | `.status.activeSessions` | ✓ | ✓ | Currently open long-poll sessions. Rises toward `maxListeners` during bursts; `0` means the group is not polling for work. |
| `ACTIVEJOBS` | `.status.activeJobs` | ✓ | ✓ | Worker pods in Running phase — jobs actively executing. Updated each reconcile (driven by pod phase-change events). |
| `PENDINGJOBS` | `.status.pendingJobs` | ✓ | ✓ | Worker pods in Pending phase — jobs acquired, pod spawned but not yet running. A sustained non-zero value signals scheduling pressure; check `WorkersUnschedulable`, `kubectl describe pod`, and node capacity. Pods past `pendingPodDeadline` are automatically reaped (and counted in `worker_pods_reaped_total{reason="pending_deadline"}`). |
| `READY` | `.status.conditions[Ready].status` | ✓ | ✓ | `True` when at least one listener goroutine is running. |
| `EGRESS` | `.status.proxyMode` | — | ✓ | `Proxied` or `Direct`. |

> **Note:** `ACTIVEJOBS` and `PENDINGJOBS` are pod-phase counts derived at reconcile
> time. They reflect a snapshot of the last reconcile cycle (re-triggered on every
> pod phase-change event) — not a real-time counter. A pod that was just reaped in
> the same reconcile cycle appears in `PENDINGJOBS` until the pod-deletion event
> triggers the next reconcile (typically sub-second).

### Drilling down to individual runner pods

The count columns tell you *how many* jobs are running; to see *which* pods back them, filter by the owner label:

```bash
# RunnerGroup (v1alpha1)
kubectl get pods -n <namespace> -l actions-gateway/runner-group=<name>

# RunnerSet (v2alpha1)
kubectl get pods -n <namespace> -l actions-gateway.com/runner-set=<name>
```

Add `-o wide` for node placement or `-w` to watch phase transitions live.

**Correlating a pod with its GitHub Actions job:** the AGC stamps these
annotations on every worker pod at creation time:

| Annotation | Example | Notes |
| --- | --- | --- |
| `actions-gateway.com/run-id` | `12345678` | GitHub workflow run ID |
| `actions-gateway.com/repository` | `myorg/myrepo` | Repository the job belongs to |
| `actions-gateway.com/job-name` | `build` | Job name as defined in the workflow YAML |
| `actions-gateway.com/workflow` | `CI` | Workflow name. Classic only — the scale-set protocol delivers no workflow name |

On the scale-set tier these are more than diagnostics: `run-id` and `repository`
are the **only** record of which workflow run a worker was serving, because that
tier provisions fire-and-forget with no in-process job state. Eviction recovery
reads them back off the pod to name the run to re-run (Q417), so a worker missing
them cannot be recovered automatically — that case is counted by
`actions_gateway_eviction_recovery_identity_unknown_total`. Do not remove or
overwrite them.

Scale-set worker pods additionally carry:

| Metadata | Example | Notes |
| --- | --- | --- |
| `actions-gateway.com/acquisition-protocol` (label) | `ScaleSet` | Marks the pod as provisioned by the scale-set tier. Present only on that tier, so `-l actions-gateway.com/acquisition-protocol=ScaleSet` selects exactly the scale-set workers |
| `actions-gateway.com/runner-name` (annotation) | `gag-ci-e2e-8f3c…` | The name this pod's runner is registered under at GitHub. The AGC deregisters that record when it reaps the pod, and treats a name stamped here as in-use when it sweeps stale records (Q550) |
| `actions-gateway.com/job-completed-at` (annotation) | `2026-07-26T12:00:00Z` | When GitHub reported the pod's job terminal. Gives a still-Running worker a reap deadline (Q420) |
| `actions-gateway.com/eviction-handled-at` (annotation) | `2026-07-26T12:04:00Z` | When the AGC adjudicated this pod's eviction. Its presence is what makes automatic recovery at-most-once per evicted pod across reconciles, restarts, and replicas (Q417) |

All four are controller-set: never set them by hand. Editing or removing
`runner-name` in particular makes the pod's runner record uncollectable, which is
what leaves stale registrations behind — see
[Scale-Set Job Stranded by a Stale Runner Record](troubleshooting.md#scale-set-job-stranded-by-a-stale-runner-record-runner-name-409).

Worker pods on **either** tier gain one more annotation at end of life:
`actions-gateway.com/deletion-reason`, stamped with the reap reason (e.g.
`completed_ttl`, `pending_deadline`) immediately before the AGC's reaper deletes the
pod (Q502). It marks the deletion as the AGC's own, which is what excludes reaper
cleanup from the graceful-deletion recovery that a drain or a manual delete triggers.
Controller-set; never set it by hand — a hand-set stamp suppresses automatic recovery
for that pod.

To see them in a table:

```bash
kubectl get pods -n <namespace> -l actions-gateway/runner-group=<name> \
  -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,RUN:.metadata.annotations.actions-gateway\.com/run-id,JOB:.metadata.annotations.actions-gateway\.com/job-name,WORKFLOW:.metadata.annotations.actions-gateway\.com/workflow'
```

Or inspect a single pod in full:

```bash
kubectl describe pod <pod-name> -n <namespace>
```

The annotations are absent if the job's identity did not reach the AGC: on the
classic tier, an AcquireJob payload without the corresponding `system.github.*`
variables (older GitHub runners or stub/test jobs); on the scale-set tier, an
assignment message without `ownerName`/`repositoryName`/`workflowRunId`.

### Selecting GAG objects with the recommended labels

Every object GAG creates — AGC/proxy/worker pods, Deployments, Services,
NetworkPolicies, ServiceAccounts, RBAC, Secrets, PDBs, HPAs, and the per-tenant CRs
— carries the Kubernetes [recommended (`app.kubernetes.io/*`) labels](https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/),
so Lens / k9s / Argo CD grouping, Prometheus relabel rules, and OpenCost/Kubecost
cost attribution work without learning the project-specific keys. They are
**additive metadata** — the functional selectors the controllers rely on (`app:`,
`actions-gateway/component: workload`, the per-gateway/runner-set identity labels)
are untouched, so never build a controller's pod selector on the `app.kubernetes.io/*`
labels.

> For live per-tenant **cost** attribution with OpenCost/Kubecost — mapping these
> labels and the per-tenant namespaces to allocation queries — see
> [Live per-tenant cost attribution](cost-attribution.md).

| Label | Values |
| --- | --- |
| `app.kubernetes.io/name` | `actions-gateway-controller` · `actions-gateway-proxy` · `actions-runner` |
| `app.kubernetes.io/instance` | the owning `ActionsGateway` / `EgressProxy` / `RunnerGroup` / `RunnerSet` name |
| `app.kubernetes.io/component` | `controller` · `proxy` · `runner` |
| `app.kubernetes.io/part-of` | `actions-gateway` (every GAG object) |
| `app.kubernetes.io/managed-by` | `actions-gateway-gmc` (control-plane children) · `actions-gateway-controller` (worker pods + job Secrets, created by the AGC) |
| `app.kubernetes.io/version` | the runner version on worker pods and their job Secrets; omitted on versionless infra (RBAC, NetworkPolicies, Services, TLS Secrets) and control-plane objects |

```bash
# Everything GAG owns, across tenants:
kubectl get all,networkpolicy,secret -A -l app.kubernetes.io/part-of=actions-gateway

# One tenant's proxy pool:
kubectl get all -n <namespace> \
  -l app.kubernetes.io/instance=<gateway>,app.kubernetes.io/component=proxy
```

### Node-disruption-safety annotations

A worker pod runs exactly one CI job and has no replica or controller behind it: evict it mid-job and the job is stranded with no replacement. So the AGC also stamps every worker pod with the markers the common node autoscalers and the descheduler honor to leave a running pod alone:

| Annotation | Value | Honored by |
| --- | --- | --- |
| `karpenter.sh/do-not-disrupt` | `true` | Karpenter — skips the pod's node for consolidation/drift disruption |
| `cluster-autoscaler.kubernetes.io/safe-to-evict` | `false` | Cluster Autoscaler — won't scale down a node running the pod |
| `descheduler.alpha.kubernetes.io/prefer-no-eviction` | `true` | Descheduler — skips the pod (current well-known key; the older `descheduler.alpha.kubernetes.io/evict` is opt-*in* only and its value is ignored) |

These markers ride on the worker pod itself, so they are removed the moment the pod is torn down on job completion (immediately when `completedPodTTL: 0s`, otherwise by the reaper once the TTL elapses) — they never pin a node for a pod that is no longer running.

**Overriding.** The markers are gap-fill defaults: set any of these keys in the runner's `podTemplate.metadata.annotations` and your explicit value wins. For example, a job you know is safe to interrupt can opt back into eviction with `cluster-autoscaler.kubernetes.io/safe-to-evict: "true"`. Only these three keys are honored from the template; other `podTemplate` annotations are not copied onto worker pods. Prefer a [PodDisruptionBudget](https://kubernetes.io/docs/tasks/run-application/configure-pdb/) if you need finer voluntary-disruption control.

---

## Label Cardinality Warning

Metric labels are scoped to `namespace` and `runner_group`. To avoid label cardinality explosion:

- **Do not use dynamically generated `runner_group` names** (e.g. names incorporating PR numbers or commit SHAs). Each unique combination of `namespace` + `runner_group` creates a distinct time series; thousands of unique names will cause memory pressure in Prometheus.
- **Stable, human-meaningful names** like `gpu-2x`, `cpu-standard`, `gpu-a100` are correct. These are configured in the `ActionsGateway` spec and should not change after initial setup.
- If you need per-workflow or per-repo attribution, use Prometheus recording rules or labels from job metadata, not from RunnerGroup names.

## Breaking observability changes (Q417)

Q417 ported eviction recovery to the scale-set acquisition tier and added a `tier`
label to the two eviction counters so the two tiers' recoveries are distinguishable.
Q497 extended recovery to scheduler preemption and added a `cause` label, on those two
counters and on the identity counter, for the same reason — the two disruptions demand
different operator responses:

| Metric | Labels before | After Q417 | After Q497 |
| --- | --- | --- | --- |
| `actions_gateway_eviction_retries_total` | `namespace`, `runner_group` | + `tier` | + `cause` |
| `actions_gateway_eviction_retries_exhausted_total` | `namespace`, `runner_group` | + `tier` | + `cause` |
| `actions_gateway_eviction_recovery_identity_unknown_total` | `namespace`, `runner_group` | — | + `cause` |

**What breaks.** Only queries that match the full label set exactly, or that render
one series per metric and now render more. Aggregations are unaffected: `sum(...)`,
`increase(...) > 0`, and `sum by (namespace, runner_group) (...)` keep working
unchanged, which covers the shipped dashboards and alert rules. Add `tier` or `cause`
to a `by (...)` clause where you want the split.

**One reading does change meaning even though no query breaks.** Before Q497,
`actions_gateway_eviction_retries_total` counted node-pressure evictions only, so a
dashboard titled "evictions" was accurate. It now also counts preemptions, which are a
routine consequence of running a `priorityTiers` floor rather than a sign of node
trouble. An alert that pages on this counter rising should filter to
`{cause="eviction"}` unless it genuinely wants both.

**Continuity.** Both counters keep their names, so history is preserved; series
recorded before the upgrade simply carry no `tier` label.

## Breaking observability changes (Q205)

The Q205 naming audit aligned metric and span/attribute names to the Prometheus and
OpenTelemetry conventions before the v2beta1 freeze. These are **breaking** for any
dashboard, alert, recording rule, or trace query that references the old names —
update them when you adopt a release that includes Q205.

**Metric renames**

| Old | New |
| --- | --- |
| `actions_gateway_renewjob_errors_total` | `actions_gateway_renew_job_errors_total` |

All other metric names were audited and kept: every counter already ends in `_total`,
every histogram already carries the `_seconds` base unit, and the gauge names are
already conventional. (`pod_creation_latency_seconds` was considered for a
`…_duration_seconds` rename but kept — `latency` is a recognised Prometheus term and
the rename's blast radius across dashboards and recording-rule names outweighed the
stylistic gain.)

**Span attribute renames** (the span names themselves — `RunnerGroup.Reconcile`,
`Provisioner.provision`, and the child spans — are unchanged):

| Old | New |
| --- | --- |
| `owner.namespace`, `runnergroup.namespace` | `k8s.namespace.name` |
| `pod.name` | `k8s.pod.name` |
| `owner.name` | `gateway.owner.name` |
| `runnergroup.name` | `gateway.runnergroup.name` |
| `plan.id` | `gateway.plan.id` |
| `active_pods` | `gateway.active_pods` |
| `ceiling.held` | `gateway.ceiling_held` |
| `priority_class` | `gateway.priority_class` |
| `pod.phase` | `gateway.pod.phase` |
| `pod.reason` | `gateway.pod.reason` |
| `duration_seconds` | `gateway.provision.duration_seconds` |

---

← Back to [Observability](observability.md)
