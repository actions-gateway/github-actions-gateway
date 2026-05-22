# 4. Operational Lifecycle Execution Flows

← [API & Data Contracts](03-api-contracts.md) | [Back to index](README.md) | Next: [Security →](05-security.md)

---

There are two distinct lifecycle flows: tenant provisioning (GMC) and job execution (AGC).

## 4.1. Tenant Provisioning Flow (GMC)

This flow runs once when a tenant creates an `ActionsGateway` resource in their namespace, and re-runs on any spec update.

```
Tenant                 GMC                    K8s API Server         Tenant Namespace
      |                  |                           |                       |
      |-- 1. Apply CR --> |                           |                       |
      |                  |-- 2. Create ServiceAccount, Role, RoleBinding --> |
      |                  |-- 3. Apply NetworkPolicy + ResourceQuota -------> |
      |                  |-- 4. Create Proxy Deployment + Service + HPA ---> |
      |                  |-- 5. Create AGC Deployment + inject App creds --> |
      |                  |-- 6. Bootstrap RunnerGroup CRs -----------------> |
      |                  |                           |                       |
      |                  |<-- 7. Proxy + AGC Deployments Ready ------------- |
      |                  |                           |                       |
      |<- 8. Status: Ready |                          |                       |
```

1. **Declare:** A tenant creates an `ActionsGateway` CR in their own namespace, providing a `gitHubAppRef`, optional `proxy` scaling config, and optional initial `runnerGroups`. No cluster-admin involvement is required.
2. **RBAC:** The GMC creates a `ServiceAccount` for the AGC and a `Role`/`RoleBinding` scoped strictly to the CR's namespace. The AGC receives no cluster-level permissions.
3. **Guardrails:** A `NetworkPolicy` and `ResourceQuota` are applied. The NetworkPolicy permits egress to GitHub's IP ranges only from proxy pods (matched by label); the AGC and worker pods are permitted egress only to the proxy `ClusterIP` Service within the namespace.
4. **Proxy:** The GMC creates the proxy `Deployment` with `podAntiAffinity` spreading replicas across nodes, a `ClusterIP` `Service` in front of it, a `PodDisruptionBudget` with `minAvailable: 1`, and an `HorizontalPodAutoscaler` configured from `spec.proxy`. The HPA scales between `minReplicas` and `maxReplicas` targeting `targetCPUUtilizationPercentage`.
5. **Deploy:** The GMC creates the AGC `Deployment`, injecting the GitHub App credentials from the referenced Secret and setting `HTTP_PROXY`/`HTTPS_PROXY` to the proxy Service address. The worker pod template in the AGC's config also receives these env vars so all job log traffic routes through the proxy pool.
6. **Bootstrap:** Any `RunnerGroup` specs in the `ActionsGateway` CR are created as `RunnerGroup` resources in the same namespace for the AGC to reconcile.
7. **Signal:** The GMC watches both the proxy Deployment's `ReadyReplicas` and the AGC Deployment's `ReadyReplicas`, updating `ActionsGatewayStatus.ProxyReadyReplicas` and the `AGCAvailable` Condition as they become available.
8. **Report:** The `ActionsGateway` status transitions to `Ready` once both the proxy pool has at least `minReplicas` ready and the AGC is healthy.

---

## 4.2. Job Execution Flow (AGC)

This flow runs per-job inside the tenant namespace, entirely managed by the AGC.

```
AGC Goroutine     Proxy Pool    K8s API Server    Worker Pod      GitHub Edge
      |               |                |               |               |
      |--1. GetMessage─>──────────────────────────────────────────────>|
      |<─2. RunnerJobRequest (run_service_url + runner_request_id)────|
      |               |                |               |               |
      |--3. AcquireJob >──────────────────────────────────────────────>|
      |<─4. planId + job instructions ────────────────────────────────|
      |               |                |               |               |
      |==5. RenewJob loop starts (every 60s via proxy) ===============>|
      |               |                |               |               |
      |--6. Create Secret + Pod ──────>|               |               |
      |               |                |─7. Start ────>|               |
      |               |                |               |─8. Logs (via proxy)─>|
      |               |                |               |<─9. Job Done──|
      |==10. RenewJob loop stops =====================================|
      |               |                |<─11. Delete Pod + Secret ─────|
```

1. **Poll:** A dedicated AGC goroutine fires a `GetMessage` request via the proxy pool. GitHub holds the connection for up to 50 seconds; returns `202 Accepted` if no job is queued.
2. **Intercept:** GitHub responds with a `RunnerJobRequest` message containing `run_service_url`, `runner_request_id`, and `billing_owner_id` in the decoded body.
3. **Lock:** The AGC immediately calls `POST {run_service_url}/acquirejob` via the proxy — before creating any Kubernetes resources — to claim the job within the 2-minute delivery window.
4. **Payload:** `acquirejob` returns the full job instructions and `planId`. The AGC decrypts the payload and extracts the single-use `ACTIONS_RUNTIME_TOKEN`.
5. **Renew:** A per-job background goroutine starts calling `POST {run_service_url}/renewjob` every 60 seconds. Each renewal extends the job lock by ~10 minutes. Pod startup time is no longer a race — the lock is already held.
6. **Stage:** The AGC commits a short-lived Kubernetes Secret containing the decrypted job payload to the tenant namespace, then creates the Ephemeral Worker Pod mounted with that Secret and `automountServiceAccountToken: false`.
7. **Handoff:** The worker pod boots, the entrypoint wrapper feeds the payload into Named Pipes, and the .NET `Runner.Worker` engine takes over.
8. **Stream:** The worker pod streams live execution logs to GitHub's Twirp Results Service via the proxy pool.
9. **Complete:** The worker container exits with code `0` on success (non-zero on workflow failure). The AGC's pod watch loop observes the terminal pod phase.
10. **Stop renewing:** The RenewJob goroutine detects pod completion and exits cleanly.
11. **Reclaim:** The AGC deletes the associated job Secret and Kubernetes garbage-collects the completed pod.

---

## 4.3. Failure Paths

The happy-path flows above are sufficient for most operations. The following diagrams cover the four most operationally significant failure modes.

### Provisioning Failure (GMC Cannot Create Resources)

```
Tenant                 GMC                    K8s API Server
      |                  |                           |
      |-- 1. Apply CR --> |                           |
      |                  |-- 2. Create ServiceAccount --> |
      |                  |<-- 3. Error: "forbidden" --|
      |                  |                           |
      |                  | [GMC sets Condition:       |
      |                  |  Ready=False               |
      |                  |  Reason: ProvisionFailed]  |
      |                  |                           |
      |<- 4. Condition: Ready=False (ProvisionFailed) |
      |                  |                           |
      |                  | [Retry with exponential    |
      |                  |  backoff via controller-   |
      |                  |  runtime requeue]          |
```

The GMC reconciler is a standard `controller-runtime` reconciler. Errors are returned from `Reconcile()` and trigger automatic requeue with exponential back-off. The GMC sets a `Ready=False` condition with reason `ProvisionFailed` and a message containing the specific error on each failed attempt.

**What the tenant observes:** The `ActionsGateway` CR exists but has `Ready=False`. No AGC, proxy, or RunnerGroup resources are present. The condition message includes the underlying error.

**Resolution:** See [Troubleshooting — GMC Not Provisioning Tenant Resources](../../docs/operations/troubleshooting.md#gmc-not-provisioning-tenant-resources).

---

### Job Acquisition Failure (Broker Returns Error)

```
AGC Goroutine     Proxy Pool    GitHub Edge
      |               |               |
      |--1. GetMessage─>──────────────>|
      |<─2. RunnerJobRequest          |
      |               |               |
      |--3. AcquireJob >──────────────>|
      |<─4. 409 Conflict (already claimed by another session) |
      |               |               |
      | [Goroutine increments          |
      |  job_acquisition_errors_total  |
      |  {reason="already_claimed"}]  |
      |               |               |
      |--5. GetMessage─>──────────────>| (loop continues)
```

`AcquireJob` can fail with:
- `409 Conflict` — job was claimed by another session (benign race in multi-listener scenarios; the job is executing elsewhere).
- `404 Not Found` — the delivery window expired before `acquirejob` was called; GitHub will redeliver.
- `422 Unprocessable Entity` — job payload is malformed or the runner version is incompatible.

In all cases the goroutine increments `actions_gateway_job_acquisition_errors_total{reason="..."}` and continues polling on the next `GetMessage` loop iteration. A replacement listener goroutine is not spawned (no job was acquired to spawn it for), so the listener count stays the same.

---

### Worker Pod Eviction and Auto-Retry

```
AGC Goroutine     K8s API Server    Worker Pod      GitHub Edge
      |                |               |               |
      |==RenewJob loop running ========================>|
      |                |               |               |
      |                |<─ Evicted (node pressure) ────|
      |                |  pod.status.reason="Evicted"  |
      |                |               |               |
      | [Pod watch observes Evicted state]              |
      |                |               |               |
      | [RenewJob loop STOPS immediately]               |
      |   (GitHub cancels the run as lock lapses)       |
      |                |               |               |
      | [Wait evictionRetryDelay (default 5s)]          |
      |                |               |               |
      | [Check retry budget: retries < maxEvictionRetries?] |
      |                |               |               |
      |──── Yes ──>  POST /repos/.../actions/runs/{run_id}/rerun-failed-jobs ─>|
      |                |               |               |
      |                |               |               |
      |──── No ──> log warning, increment              |
      |            eviction_retries_exhausted_total     |
      |            (manual re-run required)             |
```

The AGC stops renewal immediately on detecting `Evicted` (rather than waiting for lock expiry) so that GitHub cancels the run quickly and the re-queued job enters the delivery queue as soon as possible. The `evictionRetryDelay` default of 5 seconds gives GitHub time to process the cancellation before the rerun API is called.

---

### AGC Crash Mid-Job

```
AGC               K8s               GitHub
  |                |                   |
  |==RenewJob loop running ===========>|
  |                |                   |
  | [AGC OOMKilled / SIGKILL]          |
  |                |                   |
  | [All in-memory state lost:         |
  |  sessions, renew loops, run IDs]   |
  |                |                   |
  |                |<─ Pod restarted   |
  |                |                   |
  | [AGC re-registers sessions ──────>]|
  | [new sessions polling]             |
  |                |                   |
  |                |       (unacquired jobs redelivered within ~2 min)
  |                |                   |
  |  (in-flight jobs whose lock lapsed: GitHub cancels them)
```

**What GitHub observes:** Sessions are dropped (no `DELETE /sessions` sent — the process was killed). GitHub waits for session TTL before redelivering unacquired jobs. For jobs whose `renewjob` lock window expires before the AGC restarts, GitHub cancels the run. These require manual re-run.

**What the AGC does on restart:** Reconnects sessions from scratch. The in-memory retry counter state for evictions is lost; the `maxEvictionRetries` budget resets for all jobs.

**Recovery target:** Sessions restored within ~seconds of pod startup. Unacquired jobs redelivered within ~2 minutes. See the `SessionReacquisition` SLO in [Appendix A](appendix-a-capacity-slos.md).

---

← [API & Data Contracts](03-api-contracts.md) | [Back to index](README.md) | Next: [Security →](05-security.md)
