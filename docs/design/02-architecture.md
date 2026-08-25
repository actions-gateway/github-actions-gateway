# 2. Core Architectural Components

← [Executive Summary](01-executive-summary.md) | [Back to index](README.md) | Next: [API & Data Contracts →](03-api-contracts.md)

---

The system has four layers.
The GMC sits at the cluster level and manages tenant gateway instances.
Each tenant's AGC handles the GitHub API control plane.
A horizontally autoscaled proxy pool provides isolated, fault-tolerant egress for all GitHub traffic.
Ephemeral worker pods form the execution data plane, fully isolated within the tenant's namespace.

The architecture has two flows worth diagramming separately: **provisioning** (how a tenant's gateway comes into existence) and **runtime** (how a job is acquired and executed).
The two flows touch overlapping resources but answer different questions.

**Provisioning flow** — what happens when a tenant applies an `ActionsGateway` CR.

```
  Tenant namespace                           System namespace
  ════════════════                           ════════════════

  ┌──────────────────────┐                 ┌──────────────────────────────┐
  │  ActionsGateway CR   │─── watch (1) ──▶│  Gateway Manager Controller  │
  │  (namespace-scoped)  │                 │            (GMC)             │
  └──────────────────────┘                 └───────────────┬──────────────┘
                ┌──────────── reconciles (2) ──────────────┘
                ▼
  ┌──────────────────────────────────────────────────────────┐
  │  Tenant namespace resources created by GMC               │
  │  ─────────────────────────────────────────               │
  │    • ServiceAccount + Role + RoleBinding   (RBAC)        │
  │    • NetworkPolicy + ResourceQuota         (guardrails)  │
  │    • Proxy Deployment + Service + HPA + PDB              │
  │    • AGC Deployment   (replicas: 1, App creds mounted)   │
  │    • RunnerGroup CRs  (bootstrap)                        │
  └──────────────────────────────────────────────────────────┘
```

**Runtime flow** — what happens once the gateway is running and a job arrives.

```
                 ┌──────────────────────────┐
                 │  GitHub Actions Backend  │
                 └─────────────┬────────────┘
                               ↕ all egress
        ┌──────────────────────┴─────────────────────┐
        │      Egress Proxy Pool (HPA-managed)       │
        │             proxy-0 … proxy-N              │
        └────────┬──────────────────────────────┬────┘
                 ▲                              ▲
 HTTP(S)_PROXY   │                              │ HTTP(S)_PROXY
    ┌────────────┴────────────┐    ┌────────────┴───────────┐
    │  AGC (1 replica)        │    │  Ephemeral Worker Pod  │
    │  • session loops        │    │    (Runner.Worker)     │
    │  • token manager        │    └────────────┬───────────┘
    │  • renewjob goroutines  │                 ▲
    └────────────┬────────────┘                 │ spawned by
                 │ Create Secret + Pod          │ K8s scheduler
                 │                              │
    ┌────────────▼────────────┐                 │
    │  Kubernetes API Server  ├─────────────────┘
    └─────────────────────────┘
```

All AGC and worker traffic to GitHub flows through the proxy pool; Kubernetes API traffic from the AGC stays in-cluster (excluded via `NO_PROXY`).

---

## 2.1. Tier 1 — Gateway Manager Controller (GMC)

Deployed once by the platform team in a dedicated system namespace.
The default install uses `gmc-system`.
It holds a ClusterRole that grants it read access to `ActionsGateway` resources across all namespaces, and write access (cluster-wide at the RBAC layer) to Deployments, Roles, RoleBindings, and NetworkPolicies.
Per-namespace confinement of those writes is enforced not by RBAC — which cannot express "only namespaces carrying a marker label" — but by the `gmc-tenant-resource-guard` `ValidatingAdmissionPolicy`, which denies the GMC ServiceAccount any `create`/`update`/`delete` of those kinds outside namespaces an administrator has marked `actions-gateway.github.com/tenant: "true"` (Q122).
It does **not** hold `resourcequotas`/`limitranges` write verbs: the namespace `ResourceQuota` is platform-owned and the GMC operates within it without creating or mutating it *(Q130 — dropping that grant is least privilege, partially subsuming Q122)*.

* **Deployment Model:** Runs as a `Deployment` with `replicas: 2` and `controller-runtime` leader election enabled (`leaderElectionID: "actions-gateway-gmc-leader"`).
  Only the leader pod actively reconciles; the standby is immediately promoted if the leader fails.
  The GMC's reconciler is fully idempotent, so failover produces no duplicate or conflicting resources.
* **Tenant Provisioner:** On `ActionsGateway` creation, the GMC operates entirely within the CR's own namespace — the namespace already exists because the tenant created the CR there.
  It creates a scoped `ServiceAccount`, `Role`, and `RoleBinding` granting the AGC permission to manage Pods and Secrets only within that namespace, and applies a `NetworkPolicy` derived from the `ActionsGateway` spec.
  The namespace `ResourceQuota` (and any `LimitRange`) is **platform-owned** — the platform admin provisions it on the namespace and the GMC operates within it without ever creating or mutating it (Q130).
  The initial `NetworkPolicy` egress rules for the proxy pods are populated by fetching GitHub's current IP ranges from `api.github.com/meta` at provisioning time.
  The Tenant Provisioner also stamps `pod-security.kubernetes.io/enforce` on the tenant namespace at the level chosen by `spec.securityProfile` (default `baseline`), so the in-tree PodSecurity admission plugin rejects privileged worker pods at admission without requiring an external policy engine.
  Stamping PSA labels requires cluster-wide `namespaces:patch`; the `namespace-psa-guard` ValidatingAdmissionPolicy confines that grant to namespaces an administrator has marked `actions-gateway.github.com/tenant: "true"` (and to the PSA label keys only), so a compromised GMC cannot relabel system namespaces.
  See [§5.3](05-security.md#53-security-profiles-and-the-privileged-opt-in) for the profile semantics and the privileged opt-in pattern.
  (In the `v2alpha1` (`actions-gateway.com`) API this `spec.securityProfile` field is **removed**: the level is chosen by the namespace label `actions-gateway.com/security-profile` instead — GMC-guarded, namespace-scoped, shared by co-located gateways — see [Appendix H §H.16 #7](appendix-h-v2-api-decomposition.md).
  The stamping mechanism described here is unchanged.)
* **Proxy Deployer:** Creates and manages a proxy `Deployment` and `ClusterIP` `Service` within the tenant namespace, along with a `HorizontalPodAutoscaler` that scales the proxy pool based on CPU utilization.
  Proxy pods are given explicit `resources.requests` and `resources.limits` so the HPA can compute CPU utilization percentages.
  `podAntiAffinity` rules spread replicas across nodes and a `PodDisruptionBudget` ensures at least one proxy pod survives node drains.
  The AGC Deployment and all worker pod templates are injected with `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` environment variables — `NO_PROXY` includes `svc.cluster.local`, `localhost`, and `127.0.0.1`, and — on the AGC, which dials the API server by ClusterIP — `kubernetes.default.svc` plus this cluster's API server ClusterIP, read from `KUBERNETES_SERVICE_HOST` rather than assumed from a Service CIDR (Q465), so that Kubernetes API calls are never routed through the egress proxy.
  The proxy's TLS cert is signed by a per-tenant cert-manager-issued self-signed CA stored in the `actions-gateway-proxy-tls` Secret; the Proxy Deployer projects the cert into the AGC pod (cert only, via `Items: [tls.crt]`) and, via the `PROXY_TLS_SECRET_NAME` env on the AGC, instructs the AGC's pod provisioner to project the same cert into every worker pod at `/etc/actions-gateway/proxy-ca/tls.crt`.
  The private key (`tls.key`) is never projected outside the proxy pod itself.
* **AGC Deployer:** Creates and manages a `Deployment` running the AGC binary with `replicas: 1` inside the CR's namespace, injecting the tenant's GitHub App credentials from the Secret referenced in the `ActionsGateway` spec.
  The AGC is kept at a single replica to avoid multiple instances independently managing the goroutine session registry; HA is provided at the job level — any unacquired job is redelivered by GitHub.
  The Deployer also threads `spec.logLevel` (`info`|`debug`, default `info`) to both the AGC and the proxy as the `LOG_LEVEL` environment variable — the same env-on-pod-template mechanism as `spec.securityProfile`'s `SECURITY_PROFILE` — so an operator can crank one tenant to `debug` for a bug repro via `kubectl edit` with no GMC redeploy.
  Because the level lives on the pod template, flipping it is a rolling restart of the AGC and proxy, not a hot reload.
  Since the AGC is single-replica, its only scaling axis is vertical: on the v2 API `ActionsGateway.spec.agcResources` sizes the container directly, and `spec.agcAutoscaling` optionally hands that sizing to a `VerticalPodAutoscaler` the GMC stamps beside the AGC Deployment — opt-in, recommendation-only by default, composing with `agcResources` rather than overriding it, and degrading to an advisory condition where the `autoscaling.k8s.io` CRDs are absent (Q360; see [Appendix E §E.11](appendix-e-capacity-planning.md#e11-managed-vertical-right-sizing-of-the-control-planes)).
* **IP Range Reconciler:** Runs a background loop every 24 hours that fetches the current GitHub IP ranges from `api.github.com/meta` and patches any proxy pod `NetworkPolicy` whose egress rules are stale.
  The cached ranges also seed each proxy `NetworkPolicy`'s `ipBlock` egress allowlist at provisioning time, so the *initial* fetch is on the critical path for proxy egress: until it lands, the allowlist is empty and proxy→GitHub traffic is silently dropped.
  Because the periodic interval is 24 hours, the reconciler retries the initial fetch on a capped exponential backoff (1s→30s) until it succeeds rather than waiting a full interval after a transient failure or stall; the subsequent patch pass repairs any `NetworkPolicy` created with the empty cache during the retry window.
  Tenants running Cilium or Calico with FQDN-based egress policies can opt out of this feature via `spec.proxy.managedNetworkPolicy: false`.
  The fetcher is abstracted behind a `GitHubIPRangeFetcher` interface (default implementation calls `https://api.github.com/meta`) so integration tests can inject a stub without network access:

```go
type GitHubIPRangeFetcher interface {
    FetchIPRanges(ctx context.Context) ([]net.IPNet, error)
}
```

* **Lifecycle Manager:** Propagates spec changes (resource limits, proxy scaling bounds, credential Secret reference changes) down to the tenant's AGC deployment and proxy HPA.
  When `gitHubAppRef` changes, the GMC rolls the AGC Deployment so the new Pod mounts the new Secret — Secrets are treated as immutable and are never updated in place.
  On `ActionsGateway` deletion, removes only the GMC-owned resources within the namespace — it does not delete the namespace itself, since the tenant owns it.

### Design choice: reconciler writes use `CreateOrPatch`, not Server-Side Apply

The GMC's per-reconcile `apply*` helpers (`applyServiceAccount`/`RoleBinding`/`NetworkPolicy`/`ResourceQuota`/`Deployment`/`Service`/`PDB`/`HPA`/`RunnerGroup`, plus `applyOwnedSecret`) write child resources with `controllerutil.CreateOrPatch` rather than Server-Side Apply (SSA).
Both are strict improvements over the naive `Get → full-object Update`, which carried the object's `resourceVersion` as a precondition (so a racing write produced a spurious `409 Conflict` + requeue) and replaced the whole object, clobbering fields the controller doesn't manage.
`CreateOrPatch` is **not** universally better than SSA; it was the better fit for *this* reconciler, for three reasons:

- **Typed builders vs. applyconfigurations.** The helpers already receive fully-built *typed* objects from the `build*` functions, so `CreateOrPatch` slots in with a small mutate closure.
  Doing SSA *correctly* needs applyconfiguration builders (`appsv1ac.Deployment(...)`): a typed Go struct can't distinguish "field unset" from "field == zero value", so applying a typed object claims ownership of every zero-valued field and fights other managers / strips defaults.
  Clean SSA would mean rewriting all ten builders into a parallel applyconfiguration set — far larger and riskier than warranted.
- **Single-writer sufficiency.** The GMC reconciler is single-writer (`MaxConcurrentReconciles: 1`), so `CreateOrPatch`'s no-precondition merge fully closes the conflict window; SSA's per-field ownership machinery buys little here.
- **One accepted wart.** Because most closures do `obj.Spec = desired.Spec` (omitting server-defaulted fields like a Deployment's rollout `Strategy` or an HPA's `Behavior`), they emit a *non-empty* patch every reconcile that the apiserver then no-op-dedups (re-applies defaults, skips the write, no `resourceVersion` bump).
  SSA-with-applyconfigurations would emit no patch traffic at all.
  So `CreateOrPatch` here deliberately relies on the apiserver's no-op-write detection — verified against a real apiserver by the `apply_nochurn` envtest, since a fake client reproduces neither defaulting nor no-op dedup.

**The cost of losing SSA's field ownership.** Whole-`Spec` replacement is only correct where the GMC is the *only* writer of every field in that spec.
Two children break that assumption and so assign fields selectively instead:

- The proxy **`Service`**: the apiserver assigns `.spec.clusterIP`, so the closure sets only `type`/`selector`/`ports`.
- The proxy **`Deployment`**: it is the `scaleTargetRef` of the pool's HPA, which owns `.spec.replicas` via the `scale` subresource.
  Because both proxy reconcilers `Owns(&appsv1.Deployment{})`, the HPA's own scale write requeues a reconcile — so a whole-`Spec` replacement (whose `Replicas` is the builder's `minReplicas`) reverted every scale-out within milliseconds and capped each tenant's egress capacity at the floor.
  The split is now: **the reconciler owns the replica floor, the HPA owns everything above it.** The reconciler writes `.spec.replicas` only when creating the Deployment, or when it is found at `0` (an HPA refuses to scale a target off zero, so nothing else would revive the pool).
  Scale-*down* below the floor is prevented by the HPA's own `spec.minReplicas`, which the reconciler does keep in sync with the CR.

SSA would have made both cases fall out of per-field ownership for free; with `CreateOrPatch` they are a hand-maintained invariant, enforced by envtest (`hpa_scale_preservation`) because neither the `scale` subresource nor `.spec.replicas` defaulting exists on a fake client.

**Where SSA remains the right tool:** `applyNamespacePSA` keeps SSA (field manager `actionsgateway-controller-psa`) precisely because PSA labels are a security control — SSA records per-field-manager ownership and surfaces a conflict when a human admin changes a field the controller owns, so an out-of-band edit is detected and reported (a `PSALabelsOverridden` Warning Event) rather than silently re-asserted.
See [§5.3](05-security.md#53-security-profiles-and-the-privileged-opt-in).

---

## 2.2. Tier 2 — Actions Gateway Controller (AGC)

A namespace-scoped operator deployed and managed by the GMC, one instance per tenant.
It runs with RBAC permissions limited to its own namespace and manages the lifecycle of `RunnerGroup` Custom Resources within that namespace.

* **Session Multiplexer:** Spawns and manages an adaptive pool of long-polling listener goroutines per `RunnerGroup`.
  Each RunnerGroup maintains a minimum of one permanent listener goroutine; additional listeners are spawned on demand as jobs arrive and shut themselves down once the queue is idle.
  If the permanent baseline exits for a recoverable reason (e.g. a transient broker error), the multiplexer restarts it after a short backoff.
  Starting the multiplexer is idempotent: while a baseline is running or waiting out that backoff, further starts are no-ops, so a reconcile firing during the backoff window — when the active count reads zero — cannot stack a second permanent baseline.
  Stopping the multiplexer also cancels any restart still pending, so a retired multiplexer cannot resurrect a listener.

  **Agent pool.** GitHub enforces one active session per registered runner agent (HTTP 409 on duplicate).
  The AGC therefore maintains a pool of pre-registered runner agents per RunnerGroup — one agent registered per `maxListeners` slot — at RunnerGroup provisioning time.
  Each listener goroutine is assigned an agent from the pool for the duration of its session; no two goroutines share an agent concurrently.
  Agent registrations persist across idle periods and AGC restarts, but **not across jobs**: JIT-registered runners are single-use — GitHub deletes the runner record once it acquires a job (live-confirmed 2026-06-12, [M4 §12](../plan/milestone-4.md#12-live-multi-tenant-validation-evidence-2026-06-1112)) — so the pool re-registers each agent under its stable `<group>-<index>` name after every job (Q114, see the self-heal paragraph below).

  **Registration scope.** Agents may be registered at either organization scope (`https://github.com/{org}`) or repository scope (`https://github.com/{owner}/{repo}`); the registrar selects the appropriate REST API endpoints — `/orgs/{org}/actions/runners/...` or `/repos/{owner}/{repo}/actions/runners/...` — from the shape of the configured GitHub URL.
  Runner groups are an organization-level concept on GitHub's side, so the `group_id` field is included on the register payload only for org-scoped registration and is omitted for repo-scoped registration.
  Both hosted github.com and GitHub Enterprise Server (GHES) endpoints are supported under the same selection logic.

  **One resolver for the API base (Q506).** Which GitHub REST API a gateway addresses is derived from `spec.gitHubURL` in exactly one place — `githubapp.DeriveAPIBaseURL`: `https://api.github.com` for public GitHub, `https://<host>/api/v3` for a GHES appliance.
  Three components had previously each derived it their own way, and the GMC derived it nowhere: it injected `GITHUB_ORG_URL` and left `GITHUB_API_BASE_URL` unset, so the AGC's token exchange defaulted to `api.github.com` and a GHES tenant failed to mint a token before acquiring any job.
  The GMC now injects the derived base on the AGC `Deployment`, and the registrar and scale-set client call the same helper — so registration, token exchange, and the disruption auto-retry cannot drift onto different hosts.
  Egress remains a separate obligation: an FQDN egress mode carries every referrer's GHES host automatically, while the default CIDR mode allows only GitHub's published ranges and needs the appliance's ranges supplied (`GitHubEgressIncomplete` names the gap).

  The lifecycle of a listener goroutine is as follows.
  On startup the AGC spawns exactly one listener per RunnerGroup.
  Each listener claims an agent from the pool, calls `POST /sessions` with that agent's ID to open a broker session, then enters a `GET /message` long-poll loop.
  When a job message is received, the listener first consults the **pre-acquisition admission gate** (Q59): two capacity rungs, both re-read per delivery.
  The first is a live namespace-`ResourceQuota` headroom check for one more worker pod (#784); the second is an in-memory, per-RunnerGroup reservation counter that mirrors the Pod Provisioner's concurrency ceiling.
  If either refuses, the listener **skips `acquirejob` entirely** and continues polling, leaving the job queued at GitHub for redelivery to a sibling session with capacity — rather than claiming a job whose worker pod it cannot place, which would be cancelled when its unrenewed lock lapses.
  Each skipped delivery increments `actions_gateway_jobs_admission_rejected_total` with `reason="ceiling"` or `reason="quota"`.
  The quota rung fails open (an unreadable quota admits) and reserves nothing, so declining on quota leaves the ceiling budget untouched.
  The ceiling reservation is soft state (lost on AGC restart, which is fail-safe — the budget simply resets).
  Neither rung is authoritative: the Pod Provisioner's post-acquire ceiling check and its `maxQuotaRetries` loop remain the backstops for the races a per-delivery read cannot close — a sibling AGC, an eventually-consistent `ResourceQuota.status.used`, the restart window.
  When the gate admits, the listener calls `POST /acquirejob` to claim the job (this must happen before pod creation), holding the reservation until the job completes, then spawns two goroutines: a replacement listener (to maintain polling capacity for the next job) and a Job Lock Renewer (to manage the running job's lock).
  If the total active listener count is below `maxListeners`, the original listener may continue polling rather than exiting — up to `maxListeners` listeners can run concurrently during a burst.

  **Post-job re-registration and session self-heal (Q114).** The job acquisition consumes the agent's single-use JIT runner record server-side, and the session dies with it — left alone, its `GET /message` degrades into `200`-with-empty-body (`decode response: EOF`) and later `401` loops forever, permanently burning one of the group's `maxListeners` slots per completed job.
  The listener therefore self-heals: the moment `acquirejob` succeeds it marks the agent *consumed* on the pool, and once the job handler returns it deletes the dead session, re-registers the agent under its stable name (deregister-then-recreate; a `409 Already exists` from a surviving record is resolved by looking the record's ID up by name, deleting it, and retrying once), rewrites the agent Secret with the fresh credentials, and opens a new session — all without the goroutine exiting, so listener capacity is preserved.
  The same heal ladder backs the poll loop for staleness discovered after the fact (e.g. across an AGC restart, where the consumed mark is lost): a `401/403` poll response first refreshes the broker OAuth token and recreates the session (which also fixes plain token expiry), and escalates to an agent recycle only if fresh credentials are still rejected; a run of three consecutive empty-`200` responses triggers the same path.
  A consumed agent is never returned to the pool's available set un-recycled — if its goroutine exits before healing (error, shutdown), the agent is parked and the next reconcile re-registers it.
  Heal failures exit the goroutine with a retriable error so the multiplexer's existing restart backoff paces retries; recycles are surfaced via `actions_gateway_agent_recycles_total` / `_errors_total`.

  Idle shutdown: a listener goroutine that receives more than a configurable number of consecutive empty `202` responses (default: 50, matching the GitHub runner client's anomaly threshold) and is not the last *poller* for its RunnerGroup calls `DELETE /sessions/{id}` to deregister and exits.
  "Last poller" counts only goroutines currently long-polling for work — a sibling busy executing a job (inside the job handler) does not count, so the group never drains to zero pollers and stops acquiring jobs while a single in-flight job runs (Q152).
  The RunnerGroup controller ensures at least one listener goroutine is always running, restarting it if it exits unexpectedly.
  Empty polls are also floored at 100 ms apart (`broker.MinPollInterval`, Q788), so a backend that declines to hold the long poll (a GHES tenant with a short window, an intermediary that terminates it) cannot spin the loop into a request storm GitHub's rate limiter would answer instead.
  The floor costs nothing against a backend that does hold the poll, and it is applied after the idle accounting so it can neither delay nor suppress an idle shutdown.

  **Reconcile triggers.** Besides reconciling on `RunnerGroup` spec changes, the controller watches the worker Pods its provisioner creates (filtered by the `actions-gateway/runner-group` label, reusing the shared Pod informer).
  A worker-Pod create (job acquired), terminal-phase transition, eviction, or delete re-triggers a reconcile, so `status.activeSessions` and any listener-pushed conditions refresh promptly off the most operationally visible signal — pod churn — rather than going stale until the next spec change or the cache resync.
  The watch is Pods-only by design: a watch on the per-agent Secrets would establish a Secret informer and cache credential material in-process, which the AGC explicitly forbids (see [§5 security](05-security.md), H-2/W3 — Secrets bypass the cache).
  A listener or provisioner goroutine also pushes conditions and Events back to the reconciler over in-process channels; those pushes additionally wake the reconciler directly through a controller-runtime `source.Channel` (Q333), so a condition raised on an otherwise-idle RunnerGroup — one with no pod churn — is drained on the next reconcile rather than lagging up to the cache-resync period.
  A drained condition is retained until the live object is observed to reflect it, so a status write that loses a conflict with a concurrent update does not drop a one-shot listener condition.
  The same wiring backs the v2 RunnerSet reconciler.

  This adaptive model means steady-state rate-limit consumption is one session per RunnerGroup (~72 `GET /message` requests/hour), regardless of the configured `maxListeners`.
  Under burst load the session count climbs toward `maxListeners`, then drains back to one as the queue empties.
  See [Appendix E](appendix-e-capacity-planning.md) for rate-limit implications and sizing guidance.

  > **Milestone 1 protocol findings** (see [docs/plan/milestone-1.md §8](../plan/milestone-1.md#8-investigation-findings)):
  >
  > *Session reuse confirmed* (Investigation C) — **since invalidated for JIT-registered agents.** The M1 probe (a `config.sh`-registered runner) could call `GET /message` on the same `sessionId` immediately after `acquirejob` returned.
  > The M4 live run showed this does **not** hold for the JIT-registered agents the AGC actually uses: GitHub deletes a JIT runner record at job acquisition, killing the session ([M4 §12](../plan/milestone-4.md#12-live-multi-tenant-validation-evidence-2026-06-1112), Q114).
  > The listener re-registers and opens a fresh session after every job — see the self-heal paragraph above.
  >
  > *One active session per registered runner agent enforced* (Investigation D).
  > `POST /sessions` returns `409 Conflict` if the supplied `agentId` already has an active session.
  > This means each concurrent listener goroutine must hold a **distinct pre-registered runner agent** (distinct `agentId`).
  > The Session Multiplexer must therefore maintain a pool of up to `maxListeners` pre-registered agents per RunnerGroup at startup, and assign one agent to each goroutine for the duration of its session.
  > Agent registrations are persistent (created once at RunnerGroup provisioning time and deleted when the RunnerGroup is removed); sessions are ephemeral (created and deleted per listener goroutine lifecycle).
  >
  > *Opportunistic job delivery supported* (inferred from Investigation C timing).
  > A newly dispatched job appeared in `GetMessage` within ~1 second of the `workflow_dispatch` API call landing, strongly suggesting GitHub delivers to any active polling session rather than binding delivery to sessions present at queue time.
  > Direct two-runner proof was not obtained (the second-session test was blocked by the 409 constraint using the same agentId), but the timing evidence is consistent with opportunistic delivery.
  > No warm standby pool is needed.
* **Pod Provisioner:** Intercepts workflow triggers, decrypts incoming payloads, maps runner labels to target pod configurations, and schedules ephemeral worker pods within the tenant namespace.
  The provisioner extracts and stores the `run_id` from each acquired job payload alongside the pod reference — this is required by the eviction-retry path in the Job Lock Renewer.
  Before creating a pod, the provisioner enforces whichever concurrency ceiling applies to the RunnerGroup, and handles two failure modes with configurable retry:

  * **`priorityTiers` set:** The provisioner queries the active and pending worker pod count for the group (via a label-selector list against the Kubernetes API) and walks the tier list in ascending threshold order, assigning the `priorityClassName` of the first tier whose threshold the current count has not yet reached.
    If the count equals or exceeds the last tier's threshold, the pod is held until the count drops — this is the effective `maxConcurrentJobs` ceiling for the group.
  * **`maxWorkers` set (without `priorityTiers`):** The provisioner checks the active and pending pod count against `maxWorkers`.
    If the count equals or exceeds `maxWorkers`, the pod is held.
    No `priorityClassName` is set on the pod, so no cluster-scoped `PriorityClass` objects are required.
  * **Neither set:** No `priorityClassName` is set and the namespace `ResourceQuota` is the only active ceiling.

  When building the pod, the provisioner stamps a secure-by-default `SecurityContext` (scaled to the namespace's `securityProfile`) and default resource requests/limits (`500m`/`1Gi`) onto containers that omit them, gap-filling only so explicit tenant `PodTemplate` values always win.
  See [§5.3](05-security.md#53-security-profiles-and-the-privileged-opt-in) for the per-profile defaults.

  Pod-count queries are per-RunnerGroup, not namespace-wide, so groups with distinct runner labels are correctly accounted for independently.
  The pod-count read and pod-create are not atomic — a benign race exists where two concurrent job acquisitions each observe count N and both proceed at the ceiling boundary, potentially scheduling one extra pod above the threshold.
  This is acceptable; the namespace `ResourceQuota` remains the hard enforcement layer and the overshoot self-corrects as the next pod creation re-reads the live count.

  This pod-creation ceiling check runs *after* `acquirejob` has already claimed the job, so a job held here is dropped with its GitHub lock held.
  The **pre-acquisition admission gate** in the Session Multiplexer (described above, Q59) moves the common case of this decision to *before* the claim, where a rejected job stays redeliverable; the provisioner's check remains as the authoritative backstop for the races the in-memory gate cannot close (a sibling AGC, the restart window, or the read-vs-create race above).
  The two enforce the same ceiling — the gate reads `maxWorkers` / the maximum `priorityTiers` threshold — so they agree in steady state and the provisioner rarely has to hold a post-acquire job once the gate is active.

  **On the scale-set tier the same ceiling has a different pre-check and a different fallback (Q576).** The gate above is classic-only; a ScaleSet set states its capacity as one integer per long-poll instead, and GitHub can still hand it a job the ceiling has no room for — the advertisement bounds jobs assigned, while the ceiling counts worker *pods*, and the two diverge whenever a finished job's pod is still terminating.
  What the listener must not do with such a job is treat it as a transient failure: it acks by advancing a cursor, so "retry later" means holding the cursor, and the long-poll queue then redelivers immediately.
  That is a spin, and an expensive one, because minting a job's JIT config *registers a runner at GitHub* — so every pass created a registration whose name the next pass then had to deregister (704 deregister calls against one job in a 14-minute window on the `v1.3.0-rc.3` dogfood gate).
  Two changes close it.
  The listener asks the provisioner whether the ceiling has room **before** minting, so a job it cannot run costs no registration at all; and a ceiling rejection — from that pre-check, or from the authoritative check when a sibling job took the last slot in between — is routed to the same deferred-job re-offer machinery a runner-name conflict uses (Q551) rather than to the cursor hold.
  Deferred jobs are re-offered every 5s (flat, unlike the conflict path's exponential ladder, because a full ceiling clears the moment any worker finishes and a re-offer now costs no GitHub call), reported as `JobProvisionStalled=True/WorkerCeilingReached` with a Normal Event, and counted by `actions_gateway_scaleset_jobs_deferred{reason="ceiling"}`.
  They are deliberately **not** counted as provision errors: a set at its declared ceiling is working as configured.
  The advertisement itself is unchanged — a deferred job is still assigned, so it still counts against what the set may hold.

  **Holding a job is only safe if something ends the hold (Q553).** The re-offer machinery above had exactly two exits: the job provisions, or the queue delivers a terminal `JobCompleted`.
  The second is the backend's to volunteer, and it does not always arrive for an assignment GitHub has stopped honouring — a deleted run, a job re-queued elsewhere, one the run service concluded on its own.
  A held job with no exit is re-offered forever, and each re-offer that gets through provisions a worker for a job that no longer exists.
  That is what livelocks a drain: `stop.sh` waits on in-flight worker pods, and the listener keeps making them.
  The `v1.3.0-rc.3` dogfood gate recorded the end state — fifteen assignments still being retried against a scale set whose statistics reported zero — on the `ci` tenant, cleared only by hand.
  The missing signal was already on the wire.
  A deferred job is by construction assigned-and-not-complete, so GitHub counts it in `statistics.totalAssignedJobs`; a reading of zero while the listener holds any is a contradiction with one resolution.
  So while it is holding anything, the listener reads that count once a minute and, on a **second** consecutive zero reading, gives up on every job that was already held when the first one was taken.
  Three properties make that safe.
  The reading comes from a session refresh rather than a poll, because a stalled set has nothing to poll for and a 202 carries no statistics.
  Two readings rather than one, because a count is server state a fresh assignment may briefly lead — the second bracket is what keeps a just-deferred job from being dropped while GitHub still waits for it.
  And it acts only on zero: a positive count says how many assignments are live, never which, so the unambiguous case is the only one it can resolve — which is precisely the draining set this exists to unwedge.
  An abandoned job id is remembered, because a re-created session polls from cursor 0 and would otherwise replay the very assignment the check just acted on; abandoning it also releases its queue message for deletion, so the give-up survives the process rather than only the session (Q583) — and the release is followed straight away by the delete that makes it durable, because the two used to be separated by a full round of re-offers (Q603).
  Each loss is a workflow run that will not run, so it is reported as such: an `AssignmentAbandoned` Warning Event and `actions_gateway_scaleset_jobs_abandoned_total`, distinct from the deferred gauge it clears.

  **The same statistics carry the tier's only demand signal (Q720).** Every other scale-set series begins at assignment, so a set that is advertising no capacity reads idle on all of them while jobs pile up at GitHub; `statistics.totalAvailableJobs` is the count of what GitHub holds queued and has assigned to nobody, and `actions_gateway_scaleset_jobs_available` publishes it.
  It is a last-reading gauge rather than a per-poll one, for the reason the abandonment check already turns on: statistics ride on session responses and queue messages, and a 202 carries none.
  The readings are therefore every session open, the first and the re-create after a 404 alike (which is why the listener takes one there, so a set with nothing to deliver still publishes a baseline), every delivered message, and the once-a-minute assignment check while jobs are held.
  A set advertising `0` gets only the first of those, so the series holds that reading for as long as the withholding lasts; closing that gap costs a `RefreshSession` per interval per set and is ranked separately (Q960).

  **Acking is cursor advance plus a delete, and the delete waits (Q583).** The cursor stops a message being redelivered within a session, but it is session-scoped at the backend while the queue log is scale-set-scoped — so a cursor-only ack left every message a listener had ever handled to replay to the *next* session.
  Nothing pruned that queue, so the replay was not a recent window but the scale set's whole history, and a restarted AGC met it with empty `provisioned`/`completed`/`abandoned` guards: it provisioned a worker for every job the set had ever run.
  A dogfood reconnect recorded exactly that on 2026-07-05, briefly building seven workers for a previous pass's jobs.
  The fix issues the second half of the ack, `DeleteMessage`, whose wire shape had been source-derived but never exercised until [Investigation G](../plan/archive/q583-restart-replay.md) measured it answering 204 and confirmed that deleting prunes the log.
  What makes it safe is *when*: a message is deleted only once **every job it names has concluded** — completed with its Secret reclaimed, or abandoned — because replay is the recovery path for everything still in flight.
  A job provisioned but still running, a Q551 deferred job the previous process never provisioned, and a completion whose Secret reclaim failed all hold their message in the queue, so a restart still re-reads them.
  A failed delete is retried on the next poll cycle rather than dropped, since the cursor has already moved past the message and nothing else would bring it back.
  A delete the backend answers 404/410 is a third case: it completes the ack, since a message already gone is nothing left to do, but it removed nothing — and an endpoint that is not served answers the same way.
  The client returns that distinction rather than folding it into success, so the listener logs it and a probe measuring the wire shape cannot read a working delete off one (Q609).

  **The delete has to follow the conclusion closely, and has to survive the process (Q603).** Concluding a job and deleting its message are not one act: `settle` drops the job from every message waiting on it *in memory*, and a later `flushDeletes` issues the `DELETE`.
  A process that stops in between leaves the message in the queue, where the next one reads it with all three guards empty — the same defect, arriving through the fix's own seam.
  Two orderings close the half of it a stop can reach.
  The flush runs immediately after each of the two places a job concludes — the give-up check and the message handler — rather than once at a fixed point in the cycle, so no network work separates a conclusion from its delete; the widest such gap was the give-up's, which sat behind a whole round of deferred-job re-offers.
  And the loop issues whatever deletes are still outstanding on its way out, before the session is torn down and on a context detached from the cancelled one, so a rollout, drain, or eviction landing in the window does not strand the message.
  What that leaves is a hard kill — SIGKILL at grace expiry, OOM, node loss — between the conclusion and a successful `DELETE`, which no ordering can prevent: the conclusion is in this process's memory and the ack is at GitHub, and the two cannot be made atomic from here.
  Closing that is the persisted half, next.

  **The conclusion survives a hard kill by being persisted ahead of the delete (Q606).** The listener writes its concluded-job guards (`completed` and `abandoned`; `provisioned` is deliberately excluded, since replaying a still-running job is the recovery path) to a per-`RunnerSet` ConfigMap, owner-ref'd to the set so it is garbage-collected with it.
  The ordering is write-ahead and enforced: `flushDeletes` refuses to issue *any* `DELETE` while the state has changed and cannot be saved, so once a delete has even been attempted, the conclusion that authorised it is durable.
  A kill between the conclusion and the save's landing leaves the messages in the queue, where the replay re-derives the conclusion (the completion redelivers; the give-up check re-detects), at worst re-counting a completion metric once.
  A fresh listener loads the state before opening its session, and an unreadable store fails `Start` visibly rather than polling with the window silently reopen; a loaded entry is retired like any other guard, by the confirmed delete of the message that assigns it.
  What the confirmed delete structurally cannot retire (a loaded entry whose messages the predecessor already deleted, and a `completed` entry re-added by a completion replayed after its assignment's message was gone) is swept whenever a `202` proves the queue drained: an unacked message redelivers instead of a `202`, so at that moment a guard no held message assigns has nothing left to answer.
  That sweep is the retirement rule that keeps the persisted set bounded by the queue rather than accreting in etcd.
  The one exemption is Q609's: guards kept because a delete removed nothing are marked retained and survive the sweep too, since their message may still be in the log.

  **The same object also records which runs currently have workers, for a reader that is not the listener (Q844).** Beside the guards, the listener persists an `inFlight` entry per job whose worker it built, carrying the run identity `rerun-failed-jobs` addresses a run by.
  It is added on a successful provision and dropped when the job concludes, on the same write-ahead save.
  It is not a replay guard and answers no redelivered assignment; it exists so the owning reconciler can tell, on the way up, that a worker went away while no AGC was watching, which is the one disruption shape the pod itself cannot record because the disruption deletes the pod.
  The reconciler only ever *reads* it, so the poll goroutine remains the single writer and `Save` can keep replacing the whole state rather than merging it.
  A restarted listener does not adopt the entries it loads: the reconciler has already adjudicated them, and a still-running job rebuilds its own entry when its held assignment replays.
  Bounded like the guards are, by the work outstanding rather than by history: every ordinary exit drops its own entry, and a 24-hour age sweep reclaims one whose conclusion never arrives at all.

  **A conclusion the listener never read needs no hard kill to strand its message (Q689).** Q583, Q603, and Q606 all start from a conclusion the listener already holds, and Q606's residual was scoped to a hard kill on that basis.
  There is a third window on the other side of the read: a job concludes at GitHub, its `JobCompleted` is written to the queue, and the process goes away before a poll delivers it.
  Nothing the listener holds says the job is over (`completed` does not name it, so its assignment is legitimately unsettled and legitimately kept), so the exit flush leaves the message, the persisted guards have nothing to persist, and the next process provisions a worker for a job that already ran.
  The poll loop is single-goroutine, so everything it does between two polls widens that window: minting a JIT config and creating a pod, walking the runner-name ladder for a deferred job, spending a session refresh on the give-up check.
  A graceful stop landing there is enough; correlated over 60 stops taken at maximum pressure, all 4 that stopped before reading the completion replayed, and none of the 56 that had read it did.
  The close is symmetric with Q603's: if the loop owes deletes on the way out, it also owes the read that authorises them, so it drains the conclusions the queue is already holding before it flushes.
  The drain settles nothing on its own authority: only `completeJob`, off a terminal `JobCompleted`, on the same path the poll loop uses.
  It neither provisions nor acks an assignment it reads past, whose message therefore still replays to the next session.
  It runs only while some held message is still waiting on a conclusion, walks a cursor local to itself (a cursor ack is session-scoped, so skipping a message costs nothing), and advertises zero capacity, since a process that is leaving must not invite another assignment.
  Its budget is a second, far smaller than the flush's, and not for symmetry: the session cannot be deleted until the drain returns, and a scale set allows one session at a time, so every millisecond here is a millisecond the *next* AGC cannot start acquiring.
  The messages it exists to collect are already queued and come back immediately; the poll that finds nothing left is held for the backend's whole long-poll window and buys nothing.

  **A predecessor's session is a wait, not a fault (Q689).** One session per scale set is a protocol invariant, so every rolling update has a window where the outgoing AGC still holds the session and the incoming one's `CreateSession` answers 409.
  Treated as a start failure that window is far more expensive than it looks: the reconciler requeues on the work queue's exponential backoff, whose ceiling is much longer than the teardown being waited on, so a single collision leaves the set idle long after the session is free: measured as a listener that never acquired again within thirty seconds of the session being released.
  The reconciler now recognises the conflict specifically and re-attempts on a short flat interval instead, with no Warning Event: nothing is wrong, and the thing it is waiting for is bounded by the exit teardown above.
  Every other start failure keeps the loud path.

  **The same delete is what bounds the guards (Q597).** `provisioned`, `completed`, and `abandoned` never shrank: a listener accumulated one entry per job for as long as it ran, unbounded over its lifetime.
  They answer exactly one delivery — a replayed `JobAssigned` — and a replay can only carry a message still in the queue log.
  The cursor never advances past a message the listener is not holding for delete, so what it holds *is* the set of messages it can be handed again: an entry whose job no held message assigns can never be consulted again, and is retired when the message that assigned it is deleted.
  The bound inherits the delete-ack's safety rather than adding its own, because it is keyed on the same event — a message is deleted only once every job it names has concluded, so no guard can be retired for work still in flight, and the sets are bounded by what is in the queue rather than by how long the process has run.
  Two details are load-bearing.
  Retirement keys on the jobs a message **assigned**, not on every job it names, so a completion's delete retires nothing on its own — that job's assignment may still be ahead of the cursor (the Q575 replayed-after-completion case).
  And it keys on what the held messages assign rather than on what they are still *waiting* on, because `settle` empties the waiting set the moment a job concludes while its assignment stays queued behind a sibling job that has not.
  Retirement waits for the wire to confirm the delete *removed* something, which is the distinction Q609 made readable.
  A 404 completes the ack — there is nothing left to delete — but it is also how a backend that does not serve the endpoint answers, and there the message is still queued; so such a message leaves the pending set while its guards stay, and the job keeps its entries.
  That is the pre-Q597 behaviour for that job alone, on a queue already misbehaving, which is the safe direction to degrade in.
  The one shape that remains undetectable is a backend answering 204 while leaving the message in the log: nothing on the wire distinguishes it, Investigation G measured 204-and-pruned on the real queue, and a bound that refused to rest on that measurement would have to be an arbitrary cap — which re-creates the double-provision by construction.

  * **Quota rejection:** If the Kubernetes API server rejects a pod create with a `Forbidden/exceeded-quota` error, the provisioner retries in place up to `maxQuotaRetries` times (default 5) with a `quotaRetryDelay` between attempts (default 30s).
    The job lock is held throughout — quota typically clears as in-flight jobs complete, so retrying in place avoids losing the acquired job.
    Non-quota creation errors (admission webhook rejection, name conflict) are returned immediately without retry.
    Setting `maxQuotaRetries: 0` disables this path.
* **Token Manager:** A single background goroutine holds the current GitHub App installation access token in a mutex-protected struct shared across all session goroutines.
  Installation tokens expire after one hour; the Token Manager proactively refreshes at T-5 minutes before expiry.
  In-flight long-poll connections are unaffected — the token is only consulted when initiating new connections, not mid-connection.
  On refresh failure, the manager retries with exponential backoff (5s → 60s cap) and emits `actions_gateway_token_refresh_errors_total`; if the old token expires before refresh succeeds, in-flight session goroutines will start failing on next reconnection and re-register as the new token becomes available.
* **Job Lock Renewer:** After a job is acquired, a per-job background goroutine calls `renewjob` on the run service every 60 seconds to keep the job lock alive (GitHub grants a 10-minute window per renewal).
  The renewer also watches the worker pod for terminal state changes — event-driven, via a single shared Pod-informer event handler that wakes the per-job goroutine when its pod reaches a terminal phase, rather than each goroutine polling pod state on a timer — and handles two distinct exit paths:

  * **Normal completion:** The worker pod exits with status `Succeeded` or `Failed` (non-eviction).
    The renewer stops, the AGC deletes the job-payload Secret immediately, and the job is recorded as complete by GitHub via the Twirp Results Service.
    The pod itself is retained for `completedPodTTL` (default 5m — a window for operators to inspect a just-failed pod before it disappears; terminal pods consume no compute or quota) and then deleted by the worker-pod reaper.
    Setting `completedPodTTL: 0s` makes the session goroutine delete the pod immediately on completion instead.

  * **Disruption.** Three mechanisms qualify, and they look nothing alike.
    **Node-pressure eviction:** the worker pod enters `Failed` with `reason: Evicted`, set by the kubelet when the node runs out of memory or disk.
    **Scheduler preemption:** kube-scheduler *deletes* the victim to make room for a higher `priorityTiers` tier, stamping a `DisruptionTarget` condition with reason `PreemptionByScheduler` on the way out — it never produces `Evicted`, and its terminal phase is the interrupted container's own exit status, so the condition rather than the phase is what identifies it (Q497).
    **External graceful deletion:** a `kubectl drain` or a bare `kubectl delete pod` of a running worker, identified by the pod's `deletionTimestamp` being set at the moment its `Failed` phase publishes and predating the container's recorded exit — the mark a human cancel and a genuine failure both lack (measured, Q459/Q502), with the AGC's own deletions — the reaper's, and the reclaim of a worker whose job the renewer abandoned (Q501) — excluded by the `deletion-reason` stamp written before each.
    On any of these, the renewer immediately stops renewal rather than waiting for the lock window to lapse, allowing GitHub to cancel the job promptly.
    After a short configurable delay (`evictionRetryDelay`, default 5s), the renewer calls `POST /repos/{owner}/{repo}/actions/runs/{run_id}/rerun-failed-jobs` using the AGC's installation access token — and keeps calling it while GitHub answers `403 This workflow is already running`, the refusal every re-run gets until GitHub concludes the original run, which after an ungraceful kill takes until the job lock's TTL lapses (~10 minutes, measured 9m36-9m45s when the runner's report does not escape — Q503, Q396).
    The retries are paced 30s apart inside a 15-minute window and cost one slot of the retry budget in total, so the job is automatically re-queued without user intervention once GitHub will accept it; a re-run still refused past the window is surfaced via `actions_gateway_eviction_rerun_failures_total` and an `EvictionRerunFailed` Event.
    On the external-deletion mechanism alone, each pass first reads the run's conclusion and stands the recovery down if GitHub concluded it `cancelled`.
    The runbook's remedy for a worker that will not stop after a cancel is to delete its pod, which supplies the very mark that mechanism keys on, and `rerun-failed-jobs` accepts a cancelled run (Q811).
    The `run_id` is extracted from the job payload by the Pod Provisioner at acquisition time and passed to the renewer alongside the pod reference.
    It comes from the payload's serialised `github` context (`contextData.github.run_id`, with `.repository` supplying owner/repo) — the job `variables` carry no run identity, and reading them instead left this whole path inert against real GitHub until Q495: `run_id` resolved to `0`, so every classic eviction logged `pod evicted but run_id unknown` and skipped the re-run.

  To prevent infinite retry loops, each job tracks a retry count in memory.
  If a job has already been auto-retried `maxEvictionRetries` times (default 2, configurable per `RunnerGroup`), the renewer logs a warning, emits `actions_gateway_eviction_retries_exhausted_total`, and does not call the rerun API — the job remains cancelled and requires user action.
  Retries are counted per original `run_id`; a job that succeeds after one retry resets no counters (the retry state is per-job-acquisition, not persistent).
  The renewer distinguishes a disruption from an ordinary outcome by checking `pod.status.reason == "Evicted"`; separately, for a `DisruptionTarget=True` condition whose reason is exactly `PreemptionByScheduler` (matching the condition *type* alone would also match the eviction API's `EvictionByEvictionAPI`); and separately again, for a `Failed` phase that published while the pod carried a `deletionTimestamp` not stamped as the AGC's own and predating the container's recorded exit (Q502 — the mark is measured to be absent on a human cancel, so recovering on it cannot fight one; the ordering excludes both cleanup deletes of already-failed pods and deleted workers that never ran anything).
  Everything else follows the normal completion path and is not auto-retried: workflow errors, image pull failures, an OOM without the eviction annotation, the reaper's own stamped deletions, and any deleted worker with no container exit on record.
  The retry budget is one budget per `run_id` across both tiers and all causes, so no combination can spend it twice.

  The description above is the **classic** acquisition tier, where one goroutine holds the job's identity and watches its own pod.
  The **scale-set** tier reaches the same outcome by a different route, because it has neither: it provisions fire-and-forget, so nothing is watching the pod, and it receives no acquired payload to read `run_id` from (Q417).
  Two substitutions close the gap, both durable rather than process-scoped:

  * **Identity on the pod.** The scale-set assignment message carries `ownerName`, `repositoryName`, and `workflowRunId` (the protocol's `JobMessageBase`, as modelled by the official `actions/scaleset` client).
    The provisioner stamps them as the `actions-gateway.com/run-id` and `actions-gateway.com/repository` annotations at pod creation, alongside an `actions-gateway.com/acquisition-protocol=ScaleSet` label marking the tier.
    An assignment carrying no complete identity still provisions and runs; only automatic recovery degrades, counted by `actions_gateway_eviction_recovery_identity_unknown_total` and surfaced as an `EvictionRecoveryIdentityUnknown` Warning Event.
  * **Detection in the reconciler.** The owning `RunnerSet` reconcile — which already watches worker pods for phase changes and lists them to reap them — scans for disrupted scale-set workers — `Failed`/`Evicted`, the preemption condition, or a `Failed` phase carrying an unstamped deletion mark (Q502) — claims each one by stamping `actions-gateway.com/eviction-handled-at` under an optimistic lock, and calls the same `handleEviction`.
    The claim is taken **before** the GitHub call, which makes recovery at-most-once per evicted pod across reconciles, restarts, and replicas; the scan runs **before** the reaper, so a terminal pod is never deleted before its identity is read.

  Both tiers share one retry budget, keyed by `run_id` alone: `maxEvictionRetries` bounds re-runs per run across the two together, not once per tier.
  The `tier` label on the eviction counters splits the reporting without splitting the budget.

  **The same substitution closes the registration lifecycle** (Q550).
  A scale-set worker's runner record is created by `generatejitconfig` *before* its pod exists, and GitHub auto-removes an ephemeral runner's record only when that runner completes a job — so every worker killed first (reaped while `Pending`, killed by the lifetime cap, failed before the runner connected) leaves the record behind.
  Because the name is `<scaleSet>-<jobID>`, that leftover is precisely what the job's own retry collides with, and a set can wedge against its own debris.
  Nothing in the AGC remembers the name: the listener goroutine that minted it is long gone by the time the reaper runs, possibly in a different process.
  So the name goes on the pod as `actions-gateway.com/runner-name`, for the same reason the run identity and the reap deadline do, and the pod becomes the registry — the reaper deregisters the record it names, and the listener's start-up sweep treats a name stamped on a live pod as claimed.
  That claim check is what makes the sweep safe: a worker still `Pending` has an offline record indistinguishable from a stale one, and the REST runner object carries no timestamp to age records by, so pod-claimed-or-not is the only sound test available.

  A deletion whose worker never ran its container stays out of scope on both tiers: the job never ran to a reportable end, so there is no failed job for `rerun-failed-jobs` to act on.
  For a running worker, Q385's SIGTERM relay hands the signal to the runner, which reports its own outcome — but the relay makes the job *conclude* (as `failure`), not succeed, so the Q502 re-run is the repair rather than a double report.
  Both tiers order the deletion mark against the container's recorded `finishedAt`; that one rule excludes the never-ran worker (a real kubelet publishes a transient `Failed`-with-mark even for a drained still-`Pending` pod) and an operator's cleanup delete of an already-failed pod alike.

* **Worker-Pod Reaper:** A cleanup step at the start of every `RunnerGroup` reconcile that deletes worker pods the group no longer needs: pods in a terminal phase older than `completedPodTTL` (default 5m) and Pending pods older than `pendingPodDeadline` (default 10m).
  The stuck-Pending case is the important one — a pod that can never start (unpullable `workerImage`, unschedulable `podTemplate` constraints) would otherwise hold one of the group's concurrency-ceiling slots forever, since the ceiling counts Pending pods.
  Deleting the pod resolves its waiting session goroutine (the shared Pod-informer handler treats deletion as completion), which releases the listener and the slot, and emits a `WorkerPodStuckPending` Warning Event on the RunnerGroup plus the `actions_gateway_worker_pods_reaped_total` counter.
  The reaper lives in the reconciler rather than the session goroutine so cleanup is restart-safe: it also reaps pods orphaned by an AGC crash — every orphan whose deadline is derivable from durable pod state (terminal phase, `Pending` age, or a completion stamp written before the crash) is reclaimed by a fresh AGC with no operator action, measured in Q435.
  The one orphan the reconcile path cannot reclaim on its own is a `Running` worker whose job ended *while the AGC was down*, because nothing stamped it; that pod is reclaimed if GitHub redelivers its terminal `JobCompleted` to the restarted AGC's new session, which does stamp it (also measured — and GitHub does redeliver across a real multi-hour gap: a live probe observed an unacknowledged `JobCompleted` returned to a session created **13 h** after the scale set's last session was deleted, which outlasts the 12 h `maxWorkerLifetime` past which the kubelet has already killed the worker anyway, so the two mechanisms overlap rather than leaving a window between them — [Q468](../plan/archive/q468-jobcompleted-retention.md), observed 2026-07-29, a lower bound rather than a published contract).
  It is bounded unconditionally by `maxWorkerLifetime` (default 12h), stamped on every worker pod at provision time as its `activeDeadlineSeconds` and enforced by the **kubelet** — the mechanism deliberately does not depend on the AGC, because in the incident that motivated it the AGC was down for the whole 16 hours, so a reaper-side deadline would not have bounded it either.
  A pod the kubelet kills this way lands in `Failed`/`DeadlineExceeded` and is reaped under `reason="lifetime_exceeded"` with a `WorkerPodLifetimeExceeded` Warning Event, rather than under the generic `completed_ttl`, so an operator debugging a killed long job sees the cap as the cause.
  See [Q435](../plan/archive/q435-restart-orphan-reclaim.md) for the measurement and [Q438](../plan/archive/q438-worker-lifetime-deadline.md) for why the cap is a fixed default rather than derived from the job's own `timeout-minutes` (that timeout is not on the wire — measured).
  Time-based expiry between pod events is driven by the reconciler's `RequeueAfter` — the only carrier a reap deadline has, since a pod sitting `Pending` raises no further watch event and controller-runtime drops `RequeueAfter` from any reconcile that also returns an error.
  Both reconcilers therefore cap their work queue's retry backoff at 30s (client-go's default escalates to 1000s), so a run of reconcile errors — an optimistic-lock conflict on the owner's status is the routine one — delays a reap by at most that cap instead of stranding it.
  As a final backstop, every worker pod and job Secret carries a controller `OwnerReference` to its RunnerGroup, so deleting the RunnerGroup — directly or via tenant teardown — cascade-deletes everything the provisioner ever staged.
  **That backstop does not reach v2's gateway teardown** (Q547): a v2 worker pod is owned by its `RunnerSet`, and RunnerSets survive gateway deletion by design (they only *reference* the gateway, so a tenant can re-apply it and resume), which leaves the pods owned by a live object with their only reaper deleted.
  The AGC therefore reaps them itself: on observing a `deletionTimestamp` on its `ActionsGateway` it stops both acquisition tiers and deletes every worker pod under `reason="gateway_deleted"`, with a `WorkerPodsReapedOnGatewayTeardown` Warning Event and `Ready=False`/`GatewayTerminating` on the set.
  The GMC cooperates by holding teardown open until `status.activeJobs`/`pendingJobs` reach zero across the bound sets before it deletes a single child — a SIGTERM-time reap cannot work, because teardown deletes the AGC's RoleBinding and ServiceAccount within milliseconds of its Deployment and the AGC would lose both its authorization and its token before it could act.
  A Running pod is reaped only once its own job is over: the scale-set listener stamps `actions-gateway.com/job-completed-at` on the worker pod when GitHub reports the job terminal, and the reaper deletes a pod still Running more than five minutes later, emitting a `WorkerPodOrphanedRunning` Warning Event and `reason="orphaned_running"`.
  That covers the scale-set tier's fire-and-forget provisioning, where a worker that registers but never receives its job (assignment lapsed, cancelled, or completed elsewhere) would otherwise sit at `Listening for Jobs` holding a concurrency slot and a node forever (Q420) — the classic path cannot reach that state, because `provision()` owns its pod through to a terminal phase.
  A Running pod whose job is still assigned is never reaped: it is bounded by GitHub's job-level timeout and the job-lock renewal contract.
  The same stamp gives a *Pending* pod a much shorter deadline — thirty seconds — under `reason="completed_pending"` with a `WorkerPodCompletedPending` Warning Event: a worker whose job ended before the pod could start has nothing to run and, on the scale-set tier, no longer has the JIT-config Secret it mounts, because the terminal `JobCompleted` reclaims it.
  Reading the stamp only in the Running arm left such a pod to the unrelated `pendingPodDeadline`, holding a slot and a node for ten minutes and then reporting itself as a scheduling stall (Q575).
  The listener avoids creating these pods where it can — it handles a message batch's completions before its assignments, and refuses to provision a job it has already seen completed — so the reap arm covers only the orderings that guard cannot reach, chiefly a completion arriving after the pod already exists.

* **Worker Usage Sampler (Q359):** A background loop (default 15s, `WORKER_USAGE_SAMPLE_INTERVAL`; `0`/`off` disables) that lists `metrics.k8s.io` `PodMetrics` for the worker pods of the v2 `RunnerSet`s this AGC reconciles, keeps the running CPU/memory peak per pod × container, and folds each finished pod's peaks into per-RunnerSet Prometheus series (`actions_gateway_worker_usage_*` — one worker pod is one job, so per-pod peaks are per-job peaks) plus in-memory per-container peak histograms.
  From those histograms the `RunnerSet` reconciler surfaces a measured recommendation in `status.sizingRecommendation` (requests ≈ p95 of per-job peaks, memory limit ≈ observed max × headroom, sample count and window for confidence) and the advisory `SizingDrift` condition when the resolved template's ask deviates materially (≥2× waste, or a memory limit below the observed peak — OOM risk); the status field doubles as the aggregate store the sampler re-seeds from on restart, so the observation window survives rollouts with no separate store (see [Appendix H §H.7](appendix-h-v2-api-decomposition.md#h7-reference-integrity--runtime-conditions-not-admission)).
  This is the measure-and-recommend half of the worker right-sizing loop ([plan](../plan/runner-sizing-profiles.md)); stock VerticalPodAutoscaler cannot fill this role because `RunnerSet` has no `/scale` subresource for its `targetRef` and evict-and-resize actuation is useless on minutes-lived pods — the full alternatives analysis (VPA variants, GitOps recommender, standalone webhook tool) is [Appendix D §D.7](appendix-d-alternatives-considered.md#d7-worker-right-sizing-why-built-in-not-bolted-on).
  Degrades gracefully: without metrics-server the AGC runs normally and counts throttled poll errors.
  Jobs shorter than one sampling interval finish unsampled and are counted separately, so the operator can judge coverage.
  Multi-gateway safe: candidate pods are matched against the (namespace- and gateway-scoped) RunnerSet cache, so co-located AGCs never double-count each other's workers.
  Actuation is opt-in per `RunnerSet` (`spec.sizing.profile`: `Binpack`/`Throughput` from the measured history with whole-pod confidence fallback, `NodeShare` from a declared per-node envelope): the profile transform runs at pod-build time per acquired job, only ever derives the cpu/memory keys (GPUs pass through byte-identical), and reports its state in `status.sizingProfileState` (see [Appendix H §H.7](appendix-h-v2-api-decomposition.md#h7-reference-integrity--runtime-conditions-not-admission)).

**Why long-poll, not webhooks.** GitHub's broker protocol is the only mechanism for *claiming* and *executing* runner jobs.
`workflow_job` webhooks signal that work has queued, but they do not deliver the job payload or the broker session — only the broker's `GetMessage` long-poll returns a `RunnerJobRequest` that can be acquired.
Webhooks are useful as a scaling signal (and could pre-warm goroutines in a future revision), but they cannot replace the polling loop.

**Single replica, job-level HA.** The AGC runs at `replicas: 1` because the session registry and per-job RenewJob goroutines are in-memory state; two replicas would race on session creation and produce duplicate acquires.
HA is provided by GitHub's redelivery contract: any job not acquired within the 2-minute delivery window is redelivered to another session.
An AGC restart drops all in-flight long polls (which GitHub redelivers) and abandons all per-job RenewJob loops — any job whose renewal window lapses before the AGC recovers will be cancelled by GitHub.
The practical blackout budget is therefore `(remaining_lock_time on each in-flight job)`, where each renewal grants ~10 minutes.
See [Appendix A](appendix-a-capacity-slos.md) for the target recovery SLO.

**Graceful shutdown.** The AGC's SIGTERM handler iterates the session registry and issues `DELETE /sessions/{id}` for each open session before exiting, so GitHub can re-queue any unacquired work immediately rather than waiting for session TTL.
Hard crashes (SIGKILL, OOM, node failure) fall back to GitHub's natural session expiry.

The drain is a manager `Runnable`, so it is inside controller-runtime's graceful shutdown: `mgr.Start` does not return — and the process does not exit — until every listener goroutine has run its exit-defer DELETE.
Each delete runs on a context *detached* from the cancelled manager context, retried within a 10-second budget, because that context cancellation is precisely what is happening when the delete is needed.
RunnerGroups drain concurrently, so shutdown is bounded by that budget rather than by listener count.
Without the barrier the process exited out from under goroutines mid-DELETE and leaked a GitHub-side session per in-flight listener on every rollout (Q222).

The barrier covers both acquisition tiers, which it did not originally: scale-set listeners are held in their own map and were never in the drain, so SIGTERM cancelled their poll loops and the process exited without waiting for them.
That tier's exit path carries more than a session DELETE: it reads the conclusions the queue is still holding (Q689) and deletes the messages those conclusions release (Q603), so skipping it left a concluded job's assignment in the queue for the next AGC to build a worker for.
The two tiers drain concurrently, so the wait is the slower tier's rather than their sum (Q689).

---

## 2.3. Tier 3 — Egress Proxy Pool

A pool of stateless HTTPS `CONNECT` proxy pods deployed per tenant by the GMC, exposed via a `ClusterIP` `Service`.
All outbound GitHub traffic from both the AGC and worker pods routes through this pool, giving each tenant a distinct set of egress IPs that are never shared with other tenants.

* **Stateless CONNECT proxy:** Handles only `CONNECT` tunneling — it does not inspect or terminate TLS.
  This keeps the proxy simple, fast, and horizontally scalable without shared state.
* **HPA-managed scaling:** A `HorizontalPodAutoscaler` targets the proxy `Deployment`, scaling replica count between `proxy.minReplicas` and `proxy.maxReplicas` based on CPU utilization.
  As job concurrency rises, the proxy pool grows automatically; it scales back down during idle periods.
  `.spec.replicas` on that Deployment is the HPA's to set — the reconciler seeds it once at create (and restores it if the pool is found at zero, which an HPA cannot recover from) and otherwise leaves it alone; see the `apply*` helper discussion above.
  CPU is a coarse proxy for connection load — under bursty, low-CPU workloads (the common case for CONNECT tunneling) the HPA may lag.
  The v2 upgrade path is a custom `active_connections` metric exposed via prometheus-adapter; CPU is chosen for v1 because it requires no metrics-server extension.
  On the v2 API the managed HPA is also replaceable outright: `EgressProxy.spec.managedAutoscaling: false` makes the GMC provision no HPA at all, so an operator can attach KEDA, VPA, or a custom HPA to the proxy Deployment instead (Q173; see [Appendix H §H.4](appendix-h-v2-api-decomposition.md#h4-spec-sketches)).
* **Fault tolerance:** `podAntiAffinity` rules distribute replicas across nodes, and a `PodDisruptionBudget` with `minAvailable: 1` ensures at least one proxy pod remains healthy during node drains or rolling updates.
* **Graceful shutdown:** SIGTERM is not a signal that traffic has stopped arriving — marking the pod terminating, removing it from EndpointSlices, and each kube-proxy applying that removal are independent control loops.
  Shutdown therefore clears two distinct hazards, both inside one budget.
  First it fails `/readyz`, then deliberately holds the `CONNECT` listener **open** for a bounded *linger*, so connections still being steered here by an unconverged dataplane are served rather than refused (Q386).
  The `/readyz` failure is not what drives endpoint removal, despite being the mechanism upstream intends: measured on Kubernetes 1.35 (Q388), the probe fails from SIGTERM onward but its result never reaches the pod's `Ready` condition, which stays `True` for the entire termination — so removal on the ordinary delete path is driven by the `deletionTimestamp` regardless.
  (The related eviction-path gap, where the probe worker is halted outright, is [kubernetes#124648](https://github.com/kubernetes/kubernetes/issues/124648); the delete-path behaviour is the repo's own Q388 measurement.)
  On the graceful-node-shutdown path neither fires, so the endpoint stays `Ready` until the pod is dead and the linger cannot quiesce; see [node shutdown budgets](../operations/node-shutdown-budgets.md#upstream-state-of-the-art).
  Then it closes the listener and waits for in-flight tunnels; `http.Server.Shutdown` alone is not sufficient — it neither closes nor waits for hijacked connections, and every tunnel is hijacked — so tunnels are tracked separately (Q384).

    Rather than sleeping a fixed interval, the linger **measures**: the proxy is the server, so new-connection arrivals are directly observable, and it exits once none has arrived for a short quiescence interval — measured from the later of (shutdown start, last arrival), which makes that interval both a floor for an idle pod and an extending wait for one still being handed traffic.
    It is capped by `PROXY_SHUTDOWN_LINGER` (default 10s, negative disables).

    The linger is spent **inside** `PROXY_SHUTDOWN_DRAIN_TIMEOUT` (default 45s), not ahead of it: the two waits are for different things and overlap freely, since tunnels opened before SIGTERM keep finishing throughout.
    Worst case is `max(linger, drain)`, not their sum, which is what keeps `terminationGracePeriodSeconds` at 60s (45s drain + 7s force-close/health tail + 8s headroom) and what stops a truncated shutdown window — spot preemption, graceful node shutdown, where the kubelet grants `min(grace period, remaining window)` — from spending its scarce budget idling instead of draining.
    This is why there is no `preStop` sleep, the more common remedy: it is serial with the drain and its cost is unconditional.
    Tunnels still open at the deadline are force-closed and logged, because holding the pod longer only trades a logged cut for a silent SIGKILL.
    Without the linger new connections were refused mid-rollout; without the drain, every rollout severed live CI egress mid-request.

### Design choice: worker egress also routes through the proxy

Both AGC traffic (control plane: token exchange, broker calls, rerun API) and worker traffic (data plane: artifact uploads, log streams, action downloads, image pulls if proxied) traverse the same per-tenant proxy pool.
This is a deliberate choice, not a hard requirement of the broker protocol.
The alternatives and their tradeoffs:

| Path | Egress IP at GitHub | Throughput cost | Failure surface | Per-tenant kill-switch |
|---|---|---|---|---|
| **Worker via proxy** *(chosen)* | Per-tenant, stable | Proxy pool sized for AGC + worker bandwidth | Proxy outage halts in-flight worker traffic | Drain proxy pool to halt one tenant's egress |
| **Worker direct to GitHub CIDRs** | Node IP (shared across tenants) | None | Proxy outage affects AGC only | Requires per-tenant NetworkPolicy or node-level control |

The chosen path makes the per-tenant egress-IP property hold for *all* GitHub-bound traffic, which is what enables GitHub-side IP allowlisting and per-tenant audit attribution to be coherent claims.
The cost is that the proxy pool must be sized to carry worker data-plane bandwidth (multi-GB image pulls and artifact uploads under heavy load); CONNECT-only TCP forwarding without TLS termination keeps the per-byte CPU cost low, and the HPA absorbs burst load.

See [docs/plan/worker-egress-proxy.md](../plan/worker-egress-proxy.md) for the full rationale, capacity-sizing model, and the implementation gap that currently lets workers bypass this path.

---

## 2.4. Tier 4 — Ephemeral Worker Pod

A highly secure, short-lived pod optimized to do exactly one thing: execute a single allocated workflow job.
Runs inside the tenant namespace alongside the AGC.

* **Entrypoint Wrapper:** A lightweight utility acting as a dummy parent process.
  It reads the job payload from a mounted Kubernetes Secret, writes it into local anonymous pipes (inherited file descriptors, not named FIFOs — see [§11.A](../plan/milestone-3.md#11a--named-pipe-protocol) for the protocol details), and initializes the execution engine.
  Before exec'ing `Runner.Worker`, the wrapper also installs the per-tenant egress-proxy CA cert into a combined trust bundle and exports `SSL_CERT_FILE` so the runner's .NET HttpClient accepts the proxy's TLS handshake — without this, every outbound HTTPS call through `HTTPS_PROXY` fails the outer handshake with `UntrustedRoot` and the runner exits before the workflow can complete.
  The CA path is signalled to the wrapper via `PROXY_CA_CERT_PATH` set by the AGC's pod provisioner; the wrapper tolerates the env being empty (no per-tenant proxy configured) as a no-op.
  The wrapper is also PID 1 of the worker container, so it owns the pod's shutdown signal: Kubernetes delivers SIGTERM to PID 1 only, and the wrapper forwards it (and SIGINT) to its child — `Runner.Worker` in the classic mode, `run.sh` in the ScaleSet mode — then waits for the child to exit.
  Without that relay the child never learns the pod is going away and runs until the cgroup SIGKILL, so an evicted, drained, or cancelled job never reports its outcome and GitHub waits out the job lock instead (Q385).
  The handler is registered *before* the child is started, so a SIGTERM arriving in the first instants of the pod's life is held and forwarded to the child once it exists rather than dropped by PID 1's default disposition (Q445).
  The wait is bounded by `WORKER_SHUTDOWN_GRACE` (default 25s, sized to stay inside the pod's `terminationGracePeriodSeconds`); a child still alive at the deadline is SIGKILLed by the wrapper so the overrun is logged rather than silent.
  The child's exit code is still what the wrapper propagates, with a signalled child reported as 128 + signal (SIGTERM → 143).
* **Runner.Worker Engine:** The native, open-source .NET binary harvested from `actions/runner`.
  It parses the raw payload from the pipes, executes steps, compiles code, and handles real-time log ingestion back to GitHub via the **Twirp Results Service** — GitHub's protobuf-over-HTTP log and step-summary ingestion endpoint that the worker streams to over a long-lived HTTP/2 connection routed through the egress proxy.
* **Minimal RBAC Surface:** Worker pods are created with `automountServiceAccountToken: false` and a dedicated, minimally-scoped service account.
  These fields are overwritten by the AGC unconditionally after merging the tenant's `PodTemplate` — workflow code has no reason to call the Kubernetes API, and the token omission removes an unnecessary lateral-movement vector from any compromised workflow step.
* **Full `PodTemplateSpec` with controller-enforced invariants:** The `RunnerGroup` `PodTemplate` field is a standard `corev1.PodTemplateSpec`, giving tenants access to the full range of Kubernetes pod configuration — init containers, sidecars, volumes, scheduling constraints, and so on — using the same schema and tooling they use for any other workload.
  A small set of fields that the AGC depends on for correct operation (`serviceAccountName`, `automountServiceAccountToken`, `hostPID`, `hostNetwork`, `hostIPC`, and the reserved proxy env vars) are rejected at admission by CRD CEL validation rules and overwritten at pod-creation time.
  All other security constraints are delegated to the cluster's admission policy engine (e.g.
  Kyverno, OPA Gatekeeper); the AGC does not duplicate general-purpose policy enforcement.
* **ARC alignment:** ARC's `AutoscalingRunnerSet` exposes the runner container's scheduling and resource surface through `spec.template` (a `corev1.PodTemplateSpec`).
  The gateway's `RunnerGroup.spec.podTemplate` embeds the same type, so `resources`, `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints`, `runtimeClassName`, `securityContext`, `volumes`, and init/sidecar containers all transfer one-to-one with no schema translation.
  The field is named `podTemplate` rather than ARC's `template` to keep the underlying Kubernetes type unambiguous; the default `workerImage` is `ghcr.io/actions/actions-runner` to match the ARC `gha-runner-scale-set` chart default.
* **Sandboxed runtime (optional):** Operators concerned about container-escape attacks can set `runtimeClassName` (e.g.
  `gvisor`, `kata-containers`) in the `PodTemplate`.
  The system functions correctly on the default `runc` runtime; sandboxed runtimes are a hardening option, not a requirement.
  See [Appendix B](appendix-b-worker-isolation.md) for tradeoffs.

---

## 2.5. Observability

Both the GMC and AGC expose Prometheus metrics via a `/metrics` endpoint (standard `controller-runtime` metrics server).
The following metrics are the minimum required for production operation; additional `controller-runtime` built-ins (reconcile latency, queue depth, etc.) are emitted automatically.

The per-tenant proxy and AGC serve metrics over **mutual TLS** on `:8443`: a scraper must present a client certificate signed by the per-tenant metrics CA that the GMC issues, and the GMC publishes the matching scraper bundle in the `actions-gateway-metrics-client` Secret in each tenant namespace.
The AGC uses mTLS rather than the GMC's `TokenReview`-based filter to avoid giving the per-tenant components kube-API auth dependencies; the proxy — which has no kube-API access at all by design — could not use `TokenReview` without regressing that isolation.

The proxy and the AGC both keep their plaintext `/healthz` + `/readyz` probes on `:8081` (the kubelet presents no client cert), so a wedged AGC is restarted by the kubelet rather than running invisibly.
The AGC binds its health listener early in manager start — independently of the initial GitHub App token fetch, which runs as a manager runnable rather than a blocking pre-start wait — so the probes report process health without coupling Deployment readiness to GitHub reachability at startup.
A `startupProbe` gives the informer cache room to sync before liveness takes over.

The metrics-port NetworkPolicy ingress still admits `:8443` only from namespaces labelled `metrics: enabled`, so the mTLS requirement layers on top of the namespace gate as defense-in-depth.
Operators label their Prometheus namespace `metrics: enabled` and configure the scrape with the published client bundle (see [troubleshooting](../operations/troubleshooting.md)).
Kubelet probes are unaffected by that rule: node-sourced traffic is exempt from NetworkPolicy enforcement.

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `actions_gateway_active_sessions` | Gauge | `namespace`, `runner_group` | Currently open long-poll sessions |
| `actions_gateway_jobs_acquired_total` | Counter | `namespace`, `runner_group` | Jobs successfully acquired |
| `actions_gateway_jobs_admission_rejected_total` | Counter | `namespace`, `runner_group`, `reason` | Jobs left queued at GitHub because the pre-acquisition capacity gate refused (acquire skipped for redelivery). `reason="ceiling"`: at the configured worker ceiling. `reason="quota"`: the namespace `ResourceQuota` cannot admit another worker pod |
| `actions_gateway_job_acquisition_errors_total` | Counter | `namespace`, `reason` | Acquisition failures (404/409/422/other) |
| `actions_gateway_job_duration_seconds` | Histogram | `namespace`, `runner_group` | Worker pod wall time, from pod creation to its last container finishing |
| `actions_gateway_pod_creation_latency_seconds` | Histogram | `namespace` | Time from worker pod creation to runner container start (scheduling + image pull) |
| `actions_gateway_token_refreshes_total` | Counter | `namespace` | Successful installation token refreshes |
| `actions_gateway_token_refresh_errors_total` | Counter | `namespace` | Failed token refreshes |
| `actions_gateway_renew_job_errors_total` | Counter | `namespace` | RenewJob call failures (leading indicator for cancelled jobs) |
| `actions_gateway_eviction_retries_total` | Counter | `namespace`, `runner_group`, `tier`, `cause` | Jobs automatically re-queued after a worker pod disruption, by acquisition tier (`classic`, `scaleset`) and cause (`eviction`, `preemption`, `deletion`) |
| `actions_gateway_eviction_retries_exhausted_total` | Counter | `namespace`, `runner_group`, `tier`, `cause` | Disrupted jobs where retry budget was exhausted; requires manual re-run. The budget is shared across both tiers and both causes |
| `actions_gateway_eviction_rerun_withheld_total` | Counter | `namespace`, `runner_group`, `tier`, `cause`, `reason` | Disruption recoveries that deliberately made no re-run call. `reason="run_cancelled"`: the externally deleted worker's run had concluded `cancelled`, so re-running it would undo a human's cancel (Q811). The retry slot is spent, but nothing is wrong and no manual re-run is wanted |
| `actions_gateway_eviction_recovery_identity_unknown_total` | Counter | `namespace`, `runner_group` | Evicted scale-set workers carrying no workflow-run identity, so no automatic re-run was possible |
| `actions_gateway_eviction_recovery_evidence_lost_total` | Counter | `namespace`, `runner_group`, `cause` | Disrupted scale-set workers whose pod was deleted before the recovery could be claimed, so no automatic re-run was attempted and none can be attempted later; the pod is the disruption's only record (Q809). The `preemption` and `deletion` arms are the exposed ones, since both act on a pod that is already terminating |
| `actions_gateway_quota_retries_total` | Counter | `namespace`, `runner_group` | Pod creation attempts retried due to namespace ResourceQuota exhaustion |
| `actions_gateway_quota_retries_exhausted_total` | Counter | `namespace`, `runner_group` | Jobs abandoned after exhausting the quota retry budget |
| `actions_gateway_worker_pods_reaped_total` | Counter | `namespace`, `runner_group`, `runner_set`, `reason` | Worker pods deleted by the reaper (`completed_ttl`, `pending_deadline`, `completed_pending`, `orphaned_running`, `lifetime_exceeded`, or `gateway_deleted`). `runner_group` carries the owning CR's name on both tiers; `runner_set` additionally carries it on scale-set reaps (empty on classic) so the series join the `runner_set`-labelled `scaleset_*` gauges (Q514) |
| `actions_gateway_reap_blocking_sidecar_templates` | Gauge | `namespace`, `runner_set` | Regular (non-native) sidecar containers in a `RunnerSet`'s resolved worker template that may block pod reaping (Q249); pairs with the advisory `PossibleReapBlockingSidecar` condition |
| `actions_gateway_agent_recycles_total` | Counter | `namespace`, `runner_group`, `trigger` | Single-use JIT agents re-registered (`post_job`, `stale_session`, `startup`, `reconcile_repair`) |
| `actions_gateway_agent_recycle_errors_total` | Counter | `namespace`, `runner_group` | Failed attempts to re-register a single-use JIT agent |
| `actions_gateway_broker_session_leaks_total` | Counter | `namespace`, `runner_group` | Broker sessions abandoned after every `DELETE /sessions` attempt failed; each survives until GitHub expires it server-side (Q436) |
| `actions_gateway_message_poll_errors_total` | Counter | `namespace`, `reason` | GetMessage errors on either acquisition tier (non-empty-poll, non-session-expired, non-unauthorized — the last two are heal paths): `rate_limited`, `timeout`, `other` |
| `controller_runtime_reconcile_errors_total` | Counter | `controller` | GMC/AGC reconcile errors (controller-runtime built-in; no `actions_gateway_` prefix) |
| `actions_gateway_ip_range_updates_total` | Counter | `namespace` | NetworkPolicy egress rule refreshes from GitHub meta API |
| `actions_gateway_managed_gateways` | Gauge | — | Total `ActionsGateway` CRs currently managed by the GMC |
| `actions_gateway_worker_usage_job_cpu_peak_cores` | Histogram | `namespace`, `runner_set`, `container` | Per-job CPU usage peak of a v2 `RunnerSet`'s worker pods, sampled from metrics.k8s.io (Q359 right-sizing input) |
| `actions_gateway_worker_usage_job_memory_peak_bytes` | Histogram | `namespace`, `runner_set`, `container` | Per-job memory usage peak (same sampler) |
| `actions_gateway_worker_usage_cpu_peak_cores` / `…_memory_peak_bytes` | Gauge | `namespace`, `runner_set`, `container` | Max per-job peak since AGC start |
| `actions_gateway_worker_usage_jobs_sampled_total` / `…_unsampled_total` | Counter | `namespace`, `runner_set` | Jobs whose peaks were / were not captured (unsampled = shorter than a sampling interval) |
| `actions_gateway_worker_usage_poll_errors_total` | Counter | `namespace` | Failed metrics.k8s.io PodMetrics lists (metrics-server absent or RBAC denied) |

### Kubernetes Events for job-lifecycle transitions

Metrics and status conditions are the steady-state observability surface, but an operator triaging a single incident usually reaches for `kubectl describe` first.
The AGC therefore also records **Kubernetes Events** on the owning `RunnerGroup`/`RunnerSet` for the job-lifecycle transitions that fail terminally (Q170) — the event-based companion to the counters above, surfacing the same incident in `kubectl describe` and event watchers without a Prometheus query.
Each `Reason` mirrors the corresponding metric name so the two correlate:

| Reason | Type | Mirrors metric | Trigger |
|---|---|---|---|
| `JobAcquisitionFailed` | Warning | `actions_gateway_job_acquisition_errors_total` | `acquirejob` failed for a delivered job; it stays queued for redelivery |
| `RunnerVersionTooOld` | Warning | — (also the `RunnerVersionTooOld` condition) | `POST /sessions` rejected: runner version too old (classic protocol only — the scale-set protocol carries no runner version) |
| `WorkerImageBelowMinimum` | Warning | — (also the `RunnerVersionTooOld` condition) | The effective worker image ships a runner below GitHub's enforced minimum, read at reconcile on both tiers. Emitted once per transition, ahead of any GitHub rejection (Q715) |
| `SessionUnauthorized` | Warning | — (also the `Degraded` condition) | A session call rejected as unauthorized (invalid/revoked credentials) — classic `POST /sessions`, or a ScaleSet-path session create/refresh (Q325) |
| `QuotaRetriesExhausted` | Warning | `actions_gateway_quota_retries_exhausted_total` | Pod creation abandoned after the `ResourceQuota` retry budget exhausted |
| `EvictionRetriesExhausted` | Warning | `actions_gateway_eviction_retries_exhausted_total` | Disrupted job's auto-retry budget exhausted; manual re-run required. The event names the cause (eviction, preemption, or deletion) |
| `EvictionRecoveryIdentityUnknown` | Warning | `actions_gateway_eviction_recovery_identity_unknown_total` | An evicted scale-set worker carried no workflow-run identity, so there was no run to re-run; manual re-run required (Q417) |
| `EvictionRecoveryEvidenceLost` | Warning | `actions_gateway_eviction_recovery_evidence_lost_total` | A disrupted scale-set worker's pod was deleted before its recovery could be claimed, so the run cannot be re-run automatically by this or any later reconcile; manual re-run required (Q809) |
| `OrphanedWorkerRecovered` | Warning | `actions_gateway_eviction_retries_total{cause="vanished"}` | A scale-set worker pod was already gone when this AGC started and its job had never concluded, so its run is being re-run; which disruption took the worker was lost with the pod (Q844) |
| `EvictionRerunWithheld` | Normal | `actions_gateway_eviction_rerun_withheld_total` | An externally deleted worker's run had already concluded `cancelled`, so no re-run was requested and the cancel stands; no operator action needed (Q811) |

These follow the established reaper precedent (`WorkerPodStuckPending`): they record on the owner an operator would `kubectl describe`, fire on a transition/terminal outcome (never per reconcile, so no spam), and where a status condition already captures the state the Event complements it rather than duplicating it.
A listener/provisioner goroutine that detects the transition does not hold the live owner object, so it routes the Event back to the reconciler over a buffered channel — mirroring the existing condition-update channel — which records it on the next reconcile.
See [kubernetes-conventions](../development/kubernetes-conventions.md#kubernetes-events-for-lifecycle-transitions) for the convention and [troubleshooting](../operations/troubleshooting.md#job-lifecycle-events-on-a-runnergroup--runnerset) for the operator catalogue.

### Distributed tracing

Beyond metrics, the AGC emits **OpenTelemetry traces** for the reconcile path (`RunnerGroup.Reconcile`) and the job-to-pod path (`Provisioner.provision`, with child spans for secret staging, pod-count, pod creation, and the wait for completion).
Tracing is opt-in and off by default: with no OTLP endpoint configured the global provider stays the no-op default, so the spans cost effectively nothing.

Tracing is enabled — and fully configured (endpoint, sampler, resource attributes) — through the standard OpenTelemetry SDK environment variables, with no bespoke flag surface.
For GMC-managed tenants the operator does not set those env vars directly: they declare `spec.tracing` on the `ActionsGateway` CR and the GMC translates it into the AGC Deployment's `OTEL_*` env.
Auth headers (`OTEL_EXPORTER_OTLP_HEADERS`) are deliberately not exposed on the CR — they can carry credentials, so collector authentication is a network-layer concern.
See [observability](../operations/observability-logging.md#distributed-tracing-agc) for the variables and the CR-driven enablement path.

---

## 2.6. Upgrade Strategy

The system has three independently versioned components — GMC binary, AGC binary, worker image.
Each upgrades on its own cadence.

* **GMC upgrade:** Standard rolling Deployment update.
  Because the GMC runs `replicas: 2` with leader election, only one replica reconciles at any moment.
  The outgoing leader releases its lease on shutdown, so leadership transfers in seconds rather than at a lease timeout.
  In-flight tenant reconciliations are idempotent — the new leader re-derives state from the API server and converges without producing duplicate resources.
  Idempotent is not the same as inert, though: the AGC image and the runner version are GMC-side inputs to the AGC pod template, so an upgrade that changes either re-renders every tenant's AGC and incurs the AGC blackout below fleet-wide and concurrently.
  Only a *restart* of an unchanged GMC is free — it re-renders each template identically and rolls nothing.
* **AGC upgrade:** Rolling update of the per-tenant AGC Deployment.
  Because the AGC is `replicas: 1`, every upgrade incurs the same blackout window described in [§2.2](#22-tier-2--actions-gateway-controller-agc) — in-flight long polls drop and GitHub redelivers within ~2 minutes, while per-job RenewJob loops resume after the new pod starts.
  Jobs whose lock expires during the window are cancelled by GitHub.
  Operators should schedule upgrades during low-traffic periods or accept the blackout as a known cost.
  The SIGTERM session cleanup hook keeps the blackout bounded by the new pod's startup time rather than the full session TTL.
* **Worker image upgrade:** Workers are versioned per `RunnerGroup` via `spec.workerImage`.
  Bumping the field rolls forward all *future* worker pods on the next job; pods already running on the old image complete their current job and exit normally.
  Roll-back is symmetrical — revert the field.
  Because the field is per-RunnerGroup, blue/green or canary worker images can be tested by adding a second RunnerGroup with the new image and a distinct label selector before flipping the default.

GitHub enforces a minimum runner version at session creation; tenants who let `workerImage` drift will start receiving `400 Bad Request` from `POST /sessions`.
The session goroutine surfaces this as a `RunnerGroup` condition (see [§7.1](07-test-plan.md#71-unit-tests)) so operators can detect the staleness without scraping pod logs.

That rejection is classic-only, since the scale-set protocol carries no runner version at session creation, and it arrives only once the version has already stopped working.
So both reconcilers independently read the runner version off the effective worker image reference each reconcile and publish the same `RunnerVersionTooOld` condition from it, with the `WorkerImage*` reasons: `True` below the enforced minimum, `False` at or above it, `Unknown` when the reference names no version and nothing has been checked.
Two versions are in play and they answer different questions.
`agent.version`, sent at session creation, is the AGC's own listener protocol version, and stays pinned to the version GAG implements: the AGC is the registered agent on the classic path, whatever image a worker pod runs.
The worker image's runner version is what will execute the job, and is the one this condition reports.
The injected wrapper logs it from the runner's own dependency manifest at worker startup, which is the only reading that holds for an image whose tag says nothing.

The two producers therefore report different facts through one condition type, keyed by reason, and only the clear is arbitrated.
A healthy image reading does not refute a live session rejection, so it defers to a `True` carrying `VersionTooOld`.
That deference is not reciprocated: a session-sourced `True` writes over an image verdict, because an observed rejection outranks a prediction.
In the other direction the classic listener publishes `False`/`VersionAccepted` as its session-start baseline: `agent.version` is the AGC's own compile-time pin, so the operator's fix for a rejection is a gateway upgrade, and that restarts the process while the condition survives in status with nothing else to clear it.
The reconciler drops that baseline unless the live condition carries one of the listener's own two reasons.
Enumerating the session half rather than the `WorkerImage*` half is what makes a reason added later fail safe: an unrecognized one counts as the reconciler's, so the clear is dropped and the condition merely stays stale.
It drops it at the drain rather than relying on the image reading to overwrite it, and drops a retained copy the image reading has already superseded.
A reconcile that reaches the image reading overwrites a merged clear in memory before writing status, so it never surfaces; the reconcile paths that write status earlier do surface it, and one of those is the unresolved-references branch a tenant reaches by deleting a `RunnerTemplate`, where the clear would replace the image verdict.
The second drop is what makes that bounded: the reconciler's retry for an unpersisted listener push assumes a condition it never re-derives, and this one it re-derives every reconcile, so a push merged before the set's first image reading would otherwise be re-applied for the set's lifetime.

---

← [Executive Summary](01-executive-summary.md) | [Back to index](README.md) | Next: [API & Data Contracts →](03-api-contracts.md)
