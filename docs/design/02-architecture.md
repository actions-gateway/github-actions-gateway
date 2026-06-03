# 2. Core Architectural Components

← [Executive Summary](01-executive-summary.md) | [Back to index](README.md) | Next: [API & Data Contracts →](03-api-contracts.md)

---

The system has four layers. The GMC sits at the cluster level and manages tenant gateway instances. Each tenant's AGC handles the GitHub API control plane. A horizontally autoscaled proxy pool provides isolated, fault-tolerant egress for all GitHub traffic. Ephemeral worker pods form the execution data plane, fully isolated within the tenant's namespace.

The architecture has two flows worth diagramming separately: **provisioning** (how a tenant's gateway comes into existence) and **runtime** (how a job is acquired and executed). The two flows touch overlapping resources but answer different questions.

**Provisioning flow** — what happens when a tenant applies an `ActionsGateway` CR.

```
  Tenant namespace                           System namespace
  ----------------                           ----------------
  +-----------------------+                  +----------------------------+
  |  ActionsGateway CR    |  ──watch (1)──>  |  Gateway Manager Controller|
  |  (namespace-scoped)   |                  |          (GMC)             |
  +-----------------------+                  +----------------------------+
          │                                          │
          │              ┌──── reconciles (2) ───────┘
          ▼              ▼
  +------------------------------------------------------------+
  |  Tenant namespace resources created by GMC                 |
  |  ────────────────────────────────────────────────────────  |
  |  • ServiceAccount + Role + RoleBinding   (RBAC)            |
  |  • NetworkPolicy + ResourceQuota         (guardrails)      |
  |  • Proxy Deployment + Service + HPA + PDB                  |
  |  • AGC Deployment  (replicas: 1, App creds mounted)        |
  |  • RunnerGroup CRs (bootstrap)                             |
  +------------------------------------------------------------+
```

**Runtime flow** — what happens once the gateway is running and a job arrives.

```
                          GitHub Actions Backend
                                  ▲
                                  │ all egress
                                  ▼
                          +---------------------+
                          |  Egress Proxy Pool  |  (HPA-managed)
                          |  proxy-0 ... proxy-N|
                          +---------------------+
                              ▲              ▲
              HTTP(S)_PROXY   │              │   HTTP(S)_PROXY
                              │              │
                  +---------------------+    +----------------------+
                  |  AGC (1 replica)    |    | Ephemeral Worker Pod |
                  |  • session loops    |    | (Runner.Worker)      |
                  |  • token manager   |    +----------------------+
                  |  • renewjob goros  |             ▲
                  +---------------------+             │
                              │                       │ spawned by
                              │ Create Secret + Pod   │ K8s scheduler
                              ▼                       │
                  +---------------------+             │
                  |  Kubernetes API     | ────────────┘
                  |  Server             |
                  +---------------------+
```

All AGC and worker traffic to GitHub flows through the proxy pool; Kubernetes API traffic from the AGC stays in-cluster (excluded via `NO_PROXY`).

---

## 2.1. Tier 1 — Gateway Manager Controller (GMC)

Deployed once by the platform team in a dedicated system namespace. The default install uses `gmc-system`. It holds a ClusterRole that grants it read access to `ActionsGateway` resources across all namespaces, and write access to Deployments, Roles, RoleBindings, NetworkPolicies, and ResourceQuotas within any namespace where an `ActionsGateway` CR exists.

* **Deployment Model:** Runs as a `Deployment` with `replicas: 2` and `controller-runtime` leader election enabled (`leaderElectionID: "actions-gateway-gmc-leader"`). Only the leader pod actively reconciles; the standby is immediately promoted if the leader fails. The GMC's reconciler is fully idempotent, so failover produces no duplicate or conflicting resources.
* **Tenant Provisioner:** On `ActionsGateway` creation, the GMC operates entirely within the CR's own namespace — the namespace already exists because the tenant created the CR there. It creates a scoped `ServiceAccount`, `Role`, and `RoleBinding` granting the AGC permission to manage Pods and Secrets only within that namespace, and applies a `NetworkPolicy` and `ResourceQuota` derived from the `ActionsGateway` spec. The initial `NetworkPolicy` egress rules for the proxy pods are populated by fetching GitHub's current IP ranges from `api.github.com/meta` at provisioning time. The Tenant Provisioner also stamps `pod-security.kubernetes.io/enforce` on the tenant namespace at the level chosen by `spec.securityProfile` (default `baseline`), so the in-tree PodSecurity admission plugin rejects privileged worker pods at admission without requiring an external policy engine. Stamping PSA labels requires cluster-wide `namespaces:patch`; the `namespace-psa-guard` ValidatingAdmissionPolicy confines that grant to namespaces an administrator has marked `actions-gateway.github.com/tenant: "true"` (and to the PSA label keys only), so a compromised GMC cannot relabel system namespaces. See [§5.3](05-security.md#53-security-profiles-and-the-privileged-opt-in) for the profile semantics and the privileged opt-in pattern.
* **Proxy Deployer:** Creates and manages a proxy `Deployment` and `ClusterIP` `Service` within the tenant namespace, along with a `HorizontalPodAutoscaler` that scales the proxy pool based on CPU utilization. Proxy pods are given explicit `resources.requests` and `resources.limits` so the HPA can compute CPU utilization percentages. `podAntiAffinity` rules spread replicas across nodes and a `PodDisruptionBudget` ensures at least one proxy pod survives node drains. The AGC Deployment and all worker pod templates are injected with `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` environment variables — `NO_PROXY` includes `kubernetes.default.svc.cluster.local`, `localhost`, `127.0.0.1`, and the cluster service CIDR so that Kubernetes API calls are never routed through the egress proxy. The proxy's TLS cert is signed by a per-tenant cert-manager-issued self-signed CA stored in the `actions-gateway-proxy-tls` Secret; the Proxy Deployer projects the cert into the AGC pod (cert only, via `Items: [tls.crt]`) and, via the `PROXY_TLS_SECRET_NAME` env on the AGC, instructs the AGC's pod provisioner to project the same cert into every worker pod at `/etc/actions-gateway/proxy-ca/tls.crt`. The private key (`tls.key`) is never projected outside the proxy pod itself.
* **AGC Deployer:** Creates and manages a `Deployment` running the AGC binary with `replicas: 1` inside the CR's namespace, injecting the tenant's GitHub App credentials from the Secret referenced in the `ActionsGateway` spec. The AGC is kept at a single replica to avoid multiple instances independently managing the goroutine session registry; HA is provided at the job level — any unacquired job is redelivered by GitHub.
* **IP Range Reconciler:** Runs a background loop every 24 hours that fetches the current GitHub IP ranges from `api.github.com/meta` and patches any proxy pod `NetworkPolicy` whose egress rules are stale. The cached ranges also seed each proxy `NetworkPolicy`'s `ipBlock` egress allowlist at provisioning time, so the *initial* fetch is on the critical path for proxy egress: until it lands, the allowlist is empty and proxy→GitHub traffic is silently dropped. Because the periodic interval is 24 hours, the reconciler retries the initial fetch on a capped exponential backoff (1s→30s) until it succeeds rather than waiting a full interval after a transient failure or stall; the subsequent patch pass repairs any `NetworkPolicy` created with the empty cache during the retry window. Tenants running Cilium or Calico with FQDN-based egress policies can opt out of this feature via `spec.proxy.managedNetworkPolicy: false`. The fetcher is abstracted behind a `GitHubIPRangeFetcher` interface (default implementation calls `https://api.github.com/meta`) so integration tests can inject a stub without network access:

```go
type GitHubIPRangeFetcher interface {
    FetchIPRanges(ctx context.Context) ([]net.IPNet, error)
}
```

* **Lifecycle Manager:** Propagates spec changes (resource limits, proxy scaling bounds, credential Secret reference changes) down to the tenant's AGC deployment and proxy HPA. When `gitHubAppRef` changes, the GMC rolls the AGC Deployment so the new Pod mounts the new Secret — Secrets are treated as immutable and are never updated in place. On `ActionsGateway` deletion, removes only the GMC-owned resources within the namespace — it does not delete the namespace itself, since the tenant owns it.

---

## 2.2. Tier 2 — Actions Gateway Controller (AGC)

A namespace-scoped operator deployed and managed by the GMC, one instance per tenant. It runs with RBAC permissions limited to its own namespace and manages the lifecycle of `RunnerGroup` Custom Resources within that namespace.

* **Session Multiplexer:** Spawns and manages an adaptive pool of long-polling listener goroutines per `RunnerGroup`. Each RunnerGroup maintains a minimum of one permanent listener goroutine; additional listeners are spawned on demand as jobs arrive and shut themselves down once the queue is idle.

  **Agent pool.** GitHub enforces one active session per registered runner agent (HTTP 409 on duplicate). The AGC therefore maintains a pool of pre-registered runner agents per RunnerGroup — one agent registered per `maxListeners` slot — at RunnerGroup provisioning time. Agent registrations are persistent (created once and deleted when the RunnerGroup is removed). Each listener goroutine is assigned an agent from the pool for the duration of its session; no two goroutines share an agent concurrently.

  **Registration scope.** Agents may be registered at either organization scope (`https://github.com/{org}`) or repository scope (`https://github.com/{owner}/{repo}`); the registrar selects the appropriate REST API endpoints — `/orgs/{org}/actions/runners/...` or `/repos/{owner}/{repo}/actions/runners/...` — from the shape of the configured GitHub URL. Runner groups are an organization-level concept on GitHub's side, so the `group_id` field is included on the register payload only for org-scoped registration and is omitted for repo-scoped registration. Both hosted github.com and GitHub Enterprise Server (GHES) endpoints are supported under the same selection logic.

  The lifecycle of a listener goroutine is as follows. On startup the AGC spawns exactly one listener per RunnerGroup. Each listener claims an agent from the pool, calls `POST /sessions` with that agent's ID to open a broker session, then enters a `GET /message` long-poll loop. When a job message is received, the listener immediately calls `POST /acquirejob` to claim the job (this must happen before pod creation), then spawns two goroutines: a replacement listener (to maintain polling capacity for the next job) and a Job Lock Renewer (to manage the running job's lock). If the total active listener count is below `maxListeners`, the original listener may continue polling rather than exiting — up to `maxListeners` listeners can run concurrently during a burst.

  Idle shutdown: a listener goroutine that receives more than a configurable number of consecutive empty `202` responses (default: 50, matching the GitHub runner client's anomaly threshold) and is not the last listener for its RunnerGroup calls `DELETE /sessions/{id}` to deregister and exits. The RunnerGroup controller ensures at least one listener goroutine is always running, restarting it if it exits unexpectedly.

  This adaptive model means steady-state rate-limit consumption is one session per RunnerGroup (~72 `GET /message` requests/hour), regardless of the configured `maxListeners`. Under burst load the session count climbs toward `maxListeners`, then drains back to one as the queue empties. See [Appendix E](appendix-e-capacity-planning.md) for rate-limit implications and sizing guidance.

  > **Milestone 1 protocol findings** (see [docs/plan/milestone-1.md §8](../plan/milestone-1.md#8-investigation-findings)):
  >
  > *Session reuse confirmed* (Investigation C). A goroutine can call `GET /message` on the same `sessionId` immediately after `acquirejob` returns — the session remains live and continues returning `202` without any protocol error. No delete→create cycle is needed between jobs; the listener goroutine loops as specified.
  >
  > *One active session per registered runner agent enforced* (Investigation D). `POST /sessions` returns `409 Conflict` if the supplied `agentId` already has an active session. This means each concurrent listener goroutine must hold a **distinct pre-registered runner agent** (distinct `agentId`). The Session Multiplexer must therefore maintain a pool of up to `maxListeners` pre-registered agents per RunnerGroup at startup, and assign one agent to each goroutine for the duration of its session. Agent registrations are persistent (created once at RunnerGroup provisioning time and deleted when the RunnerGroup is removed); sessions are ephemeral (created and deleted per listener goroutine lifecycle).
  >
  > *Opportunistic job delivery supported* (inferred from Investigation C timing). A newly dispatched job appeared in `GetMessage` within ~1 second of the `workflow_dispatch` API call landing, strongly suggesting GitHub delivers to any active polling session rather than binding delivery to sessions present at queue time. Direct two-runner proof was not obtained (the second-session test was blocked by the 409 constraint using the same agentId), but the timing evidence is consistent with opportunistic delivery. No warm standby pool is needed.
* **Pod Provisioner:** Intercepts workflow triggers, decrypts incoming payloads, maps runner labels to target pod configurations, and schedules ephemeral worker pods within the tenant namespace. The provisioner extracts and stores the `run_id` from each acquired job payload alongside the pod reference — this is required by the eviction-retry path in the Job Lock Renewer. Before creating a pod, the provisioner enforces whichever concurrency ceiling applies to the RunnerGroup, and handles two failure modes with configurable retry:

  * **`priorityTiers` set:** The provisioner queries the active and pending worker pod count for the group (via a label-selector list against the Kubernetes API) and walks the tier list in ascending threshold order, assigning the `priorityClassName` of the first tier whose threshold the current count has not yet reached. If the count equals or exceeds the last tier's threshold, the pod is held until the count drops — this is the effective `maxConcurrentJobs` ceiling for the group.
  * **`maxWorkers` set (without `priorityTiers`):** The provisioner checks the active and pending pod count against `maxWorkers`. If the count equals or exceeds `maxWorkers`, the pod is held. No `priorityClassName` is set on the pod, so no cluster-scoped `PriorityClass` objects are required.
  * **Neither set:** No `priorityClassName` is set and the namespace `ResourceQuota` is the only active ceiling.

  When building the pod, the provisioner stamps a secure-by-default `SecurityContext` (scaled to the namespace's `securityProfile`) and default resource requests/limits (`500m`/`1Gi`) onto containers that omit them, gap-filling only so explicit tenant `PodTemplate` values always win. See [§5.3](05-security.md#53-security-profiles-and-the-privileged-opt-in) for the per-profile defaults.

  Pod-count queries are per-RunnerGroup, not namespace-wide, so groups with distinct runner labels are correctly accounted for independently. The pod-count read and pod-create are not atomic — a benign race exists where two concurrent job acquisitions each observe count N and both proceed at the ceiling boundary, potentially scheduling one extra pod above the threshold. This is acceptable; the namespace `ResourceQuota` remains the hard enforcement layer and the overshoot self-corrects as the next pod creation re-reads the live count.

  * **Quota rejection:** If the Kubernetes API server rejects a pod create with a `Forbidden/exceeded-quota` error, the provisioner retries in place up to `maxQuotaRetries` times (default 5) with a `quotaRetryDelay` between attempts (default 30s). The job lock is held throughout — quota typically clears as in-flight jobs complete, so retrying in place avoids losing the acquired job. Non-quota creation errors (admission webhook rejection, name conflict) are returned immediately without retry. Setting `maxQuotaRetries: 0` disables this path.
* **Token Manager:** A single background goroutine holds the current GitHub App installation access token in a mutex-protected struct shared across all session goroutines. Installation tokens expire after one hour; the Token Manager proactively refreshes at T-5 minutes before expiry. In-flight long-poll connections are unaffected — the token is only consulted when initiating new connections, not mid-connection. On refresh failure, the manager retries with exponential backoff (5s → 60s cap) and emits `actions_gateway_token_refresh_errors_total`; if the old token expires before refresh succeeds, in-flight session goroutines will start failing on next reconnection and re-register as the new token becomes available.
* **Job Lock Renewer:** After a job is acquired, a per-job background goroutine calls `renewjob` on the run service every 60 seconds to keep the job lock alive (GitHub grants a 10-minute window per renewal). The renewer also watches the worker pod for terminal state changes — event-driven, via a single shared Pod-informer event handler that wakes the per-job goroutine when its pod reaches a terminal phase, rather than each goroutine polling pod state on a timer — and handles two distinct exit paths:

  * **Normal completion:** The worker pod exits with status `Succeeded` or `Failed` (non-eviction). The renewer stops, the AGC garbage-collects the pod and its job-payload Secret, and the job is recorded as complete by GitHub via the Twirp Results Service.

  * **Eviction:** The worker pod enters `Failed` state with `reason: Evicted` — set by the Kubernetes node agent when the pod is preempted or OOM-killed. The renewer immediately stops renewal rather than waiting for the lock window to lapse, allowing GitHub to cancel the job promptly. After a short configurable delay (`evictionRetryDelay`, default 5s — enough for GitHub to process the cancellation), the renewer calls `POST /repos/{owner}/{repo}/actions/runs/{run_id}/rerun-failed-jobs` using the AGC's installation access token, automatically re-queuing the job without user intervention. The `run_id` is extracted from the job payload by the Pod Provisioner at acquisition time and passed to the renewer alongside the pod reference.

  To prevent infinite retry loops, each job tracks a retry count in memory. If a job has already been auto-retried `maxEvictionRetries` times (default 2, configurable per `RunnerGroup`), the renewer logs a warning, emits `actions_gateway_eviction_retries_exhausted_total`, and does not call the rerun API — the job remains cancelled and requires user action. Retries are counted per original `run_id`; a job that succeeds after one retry resets no counters (the retry state is per-job-acquisition, not persistent). The renewer distinguishes eviction from other `Failed` states by checking `pod.status.reason == "Evicted"` — non-eviction failures (OOM without eviction annotation, workflow errors, image pull failures) follow the normal completion path and are not auto-retried.

**Why long-poll, not webhooks.** GitHub's broker protocol is the only mechanism for *claiming* and *executing* runner jobs. `workflow_job` webhooks signal that work has queued, but they do not deliver the job payload or the broker session — only the broker's `GetMessage` long-poll returns a `RunnerJobRequest` that can be acquired. Webhooks are useful as a scaling signal (and could pre-warm goroutines in a future revision), but they cannot replace the polling loop.

**Single replica, job-level HA.** The AGC runs at `replicas: 1` because the session registry and per-job RenewJob goroutines are in-memory state; two replicas would race on session creation and produce duplicate acquires. HA is provided by GitHub's redelivery contract: any job not acquired within the 2-minute delivery window is redelivered to another session. An AGC restart drops all in-flight long polls (which GitHub redelivers) and abandons all per-job RenewJob loops — any job whose renewal window lapses before the AGC recovers will be cancelled by GitHub. The practical blackout budget is therefore `(remaining_lock_time on each in-flight job)`, where each renewal grants ~10 minutes. See [Appendix A](appendix-a-capacity-slos.md) for the target recovery SLO.

**Graceful shutdown.** The AGC's SIGTERM handler iterates the session registry and issues `DELETE /sessions/{id}` for each open session before exiting, so GitHub can re-queue any unacquired work immediately rather than waiting for session TTL. Hard crashes (SIGKILL, OOM, node failure) fall back to GitHub's natural session expiry.

---

## 2.3. Tier 3 — Egress Proxy Pool

A pool of stateless HTTPS `CONNECT` proxy pods deployed per tenant by the GMC, exposed via a `ClusterIP` `Service`. All outbound GitHub traffic from both the AGC and worker pods routes through this pool, giving each tenant a distinct set of egress IPs that are never shared with other tenants.

* **Stateless CONNECT proxy:** Handles only `CONNECT` tunneling — it does not inspect or terminate TLS. This keeps the proxy simple, fast, and horizontally scalable without shared state.
* **HPA-managed scaling:** A `HorizontalPodAutoscaler` targets the proxy `Deployment`, scaling replica count between `proxy.minReplicas` and `proxy.maxReplicas` based on CPU utilization. As job concurrency rises, the proxy pool grows automatically; it scales back down during idle periods. CPU is a coarse proxy for connection load — under bursty, low-CPU workloads (the common case for CONNECT tunneling) the HPA may lag. The v2 upgrade path is a custom `active_connections` metric exposed via prometheus-adapter; CPU is chosen for v1 because it requires no metrics-server extension.
* **Fault tolerance:** `podAntiAffinity` rules distribute replicas across nodes, and a `PodDisruptionBudget` with `minAvailable: 1` ensures at least one proxy pod remains healthy during node drains or rolling updates.

### Design choice: worker egress also routes through the proxy

Both AGC traffic (control plane: token exchange, broker calls, rerun API) and worker traffic (data plane: artifact uploads, log streams, action downloads, image pulls if proxied) traverse the same per-tenant proxy pool. This is a deliberate choice, not a hard requirement of the broker protocol. The alternatives and their tradeoffs:

| Path | Egress IP at GitHub | Throughput cost | Failure surface | Per-tenant kill-switch |
|---|---|---|---|---|
| **Worker via proxy** *(chosen)* | Per-tenant, stable | Proxy pool sized for AGC + worker bandwidth | Proxy outage halts in-flight worker traffic | Drain proxy pool to halt one tenant's egress |
| **Worker direct to GitHub CIDRs** | Node IP (shared across tenants) | None | Proxy outage affects AGC only | Requires per-tenant NetworkPolicy or node-level control |

The chosen path makes the per-tenant egress-IP property hold for *all* GitHub-bound traffic, which is what enables GitHub-side IP allowlisting and per-tenant audit attribution to be coherent claims. The cost is that the proxy pool must be sized to carry worker data-plane bandwidth (multi-GB image pulls and artifact uploads under heavy load); CONNECT-only TCP forwarding without TLS termination keeps the per-byte CPU cost low, and the HPA absorbs burst load.

See [docs/plan/worker-egress-proxy.md](../plan/worker-egress-proxy.md) for the full rationale, capacity-sizing model, and the implementation gap that currently lets workers bypass this path.

---

## 2.4. Tier 4 — Ephemeral Worker Pod

A highly secure, short-lived pod optimized to do exactly one thing: execute a single allocated workflow job. Runs inside the tenant namespace alongside the AGC.

* **Entrypoint Wrapper:** A lightweight utility acting as a dummy parent process. It reads the job payload from a mounted Kubernetes Secret, writes it into local anonymous pipes (inherited file descriptors, not named FIFOs — see [§11.A](../plan/milestone-3.md#11a--named-pipe-protocol) for the protocol details), and initializes the execution engine. Before exec'ing `Runner.Worker`, the wrapper also installs the per-tenant egress-proxy CA cert into a combined trust bundle and exports `SSL_CERT_FILE` so the runner's .NET HttpClient accepts the proxy's TLS handshake — without this, every outbound HTTPS call through `HTTPS_PROXY` fails the outer handshake with `UntrustedRoot` and the runner exits before the workflow can complete. The CA path is signalled to the wrapper via `PROXY_CA_CERT_PATH` set by the AGC's pod provisioner; the wrapper tolerates the env being empty (no per-tenant proxy configured) as a no-op.
* **Runner.Worker Engine:** The native, open-source .NET binary harvested from `actions/runner`. It parses the raw payload from the pipes, executes steps, compiles code, and handles real-time log ingestion back to GitHub via the **Twirp Results Service** — GitHub's protobuf-over-HTTP log and step-summary ingestion endpoint that the worker streams to over a long-lived HTTP/2 connection routed through the egress proxy.
* **Minimal RBAC Surface:** Worker pods are created with `automountServiceAccountToken: false` and a dedicated, minimally-scoped service account. These fields are overwritten by the AGC unconditionally after merging the tenant's `PodTemplate` — workflow code has no reason to call the Kubernetes API, and the token omission removes an unnecessary lateral-movement vector from any compromised workflow step.
* **Full `PodTemplateSpec` with controller-enforced invariants:** The `RunnerGroup` `PodTemplate` field is a standard `corev1.PodTemplateSpec`, giving tenants access to the full range of Kubernetes pod configuration — init containers, sidecars, volumes, scheduling constraints, and so on — using the same schema and tooling they use for any other workload. A small set of fields that the AGC depends on for correct operation (`serviceAccountName`, `automountServiceAccountToken`, `hostPID`, `hostNetwork`, `hostIPC`, and the reserved proxy env vars) are rejected at admission by CRD CEL validation rules and overwritten at pod-creation time. All other security constraints are delegated to the cluster's admission policy engine (e.g. Kyverno, OPA Gatekeeper); the AGC does not duplicate general-purpose policy enforcement.
* **ARC alignment:** ARC's `AutoscalingRunnerSet` exposes the runner container's scheduling and resource surface through `spec.template` (a `corev1.PodTemplateSpec`). The gateway's `RunnerGroup.spec.podTemplate` embeds the same type, so `resources`, `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints`, `runtimeClassName`, `securityContext`, `volumes`, and init/sidecar containers all transfer one-to-one with no schema translation. The field is named `podTemplate` rather than ARC's `template` to keep the underlying Kubernetes type unambiguous; the default `workerImage` is `ghcr.io/actions/actions-runner` to match the ARC `gha-runner-scale-set` chart default.
* **Sandboxed runtime (optional):** Operators concerned about container-escape attacks can set `runtimeClassName` (e.g. `gvisor`, `kata-containers`) in the `PodTemplate`. The system functions correctly on the default `runc` runtime; sandboxed runtimes are a hardening option, not a requirement. See [Appendix B](appendix-b-worker-isolation.md) for tradeoffs.

---

## 2.5. Observability

Both the GMC and AGC expose Prometheus metrics via a `/metrics` endpoint (standard `controller-runtime` metrics server). The following metrics are the minimum required for production operation; additional `controller-runtime` built-ins (reconcile latency, queue depth, etc.) are emitted automatically.

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `actions_gateway_active_sessions` | Gauge | `namespace`, `runner_group` | Currently open long-poll sessions |
| `actions_gateway_jobs_acquired_total` | Counter | `namespace`, `runner_group` | Jobs successfully acquired |
| `actions_gateway_job_acquisition_errors_total` | Counter | `namespace`, `reason` | Acquisition failures (404/409/422/other) |
| `actions_gateway_job_duration_seconds` | Histogram | `namespace`, `runner_group` | Wall time from acquirejob to pod completion |
| `actions_gateway_pod_creation_latency_seconds` | Histogram | `namespace` | Time from acquirejob to pod Scheduled event |
| `actions_gateway_token_refreshes_total` | Counter | `namespace` | Successful installation token refreshes |
| `actions_gateway_token_refresh_errors_total` | Counter | `namespace` | Failed token refreshes |
| `actions_gateway_renewjob_errors_total` | Counter | `namespace` | RenewJob call failures (leading indicator for cancelled jobs) |
| `actions_gateway_eviction_retries_total` | Counter | `namespace`, `runner_group` | Jobs automatically re-queued after worker pod eviction |
| `actions_gateway_eviction_retries_exhausted_total` | Counter | `namespace`, `runner_group` | Evicted jobs where retry budget was exhausted; requires manual re-run |
| `actions_gateway_quota_retries_total` | Counter | `namespace`, `runner_group` | Pod creation attempts retried due to namespace ResourceQuota exhaustion |
| `actions_gateway_quota_retries_exhausted_total` | Counter | `namespace`, `runner_group` | Jobs abandoned after exhausting the quota retry budget |
| `actions_gateway_message_poll_errors_total` | Counter | `namespace` | GetMessage errors (non-empty-poll, non-session-expired) |
| `actions_gateway_reconcile_errors_total` | Counter | `controller`, `resource` | GMC/AGC reconcile errors |
| `actions_gateway_ip_range_updates_total` | Counter | `namespace` | NetworkPolicy egress rule refreshes from GitHub meta API |
| `actions_gateway_managed_gateways` | Gauge | — | Total `ActionsGateway` CRs currently managed by the GMC |

---

## 2.6. Upgrade Strategy

The system has three independently versioned components — GMC binary, AGC binary, worker image. Each upgrades on its own cadence.

* **GMC upgrade:** Standard rolling Deployment update. Because the GMC runs `replicas: 2` with leader election, only one replica reconciles at any moment and the leadership lease transfers seamlessly across the rollout. In-flight tenant reconciliations are idempotent — the new leader re-derives state from the API server and converges without producing duplicate resources.
* **AGC upgrade:** Rolling update of the per-tenant AGC Deployment. Because the AGC is `replicas: 1`, every upgrade incurs the same blackout window described in [§2.2](#22-tier-2--actions-gateway-controller-agc) — in-flight long polls drop and GitHub redelivers within ~2 minutes, while per-job RenewJob loops resume after the new pod starts. Jobs whose lock expires during the window are cancelled by GitHub. Operators should schedule upgrades during low-traffic periods or accept the blackout as a known cost. The SIGTERM session cleanup hook keeps the blackout bounded by the new pod's startup time rather than the full session TTL.
* **Worker image upgrade:** Workers are versioned per `RunnerGroup` via `spec.workerImage`. Bumping the field rolls forward all *future* worker pods on the next job; pods already running on the old image complete their current job and exit normally. Roll-back is symmetrical — revert the field. Because the field is per-RunnerGroup, blue/green or canary worker images can be tested by adding a second RunnerGroup with the new image and a distinct label selector before flipping the default.

GitHub enforces a minimum runner version at session creation; tenants who let `workerImage` drift will start receiving `400 Bad Request` from `POST /sessions`. The session goroutine surfaces this as a `RunnerGroup` condition (see [§7.1](07-test-plan.md#71-unit-tests)) so operators can detect the staleness without scraping pod logs.

---

← [Executive Summary](01-executive-summary.md) | [Back to index](README.md) | Next: [API & Data Contracts →](03-api-contracts.md)
