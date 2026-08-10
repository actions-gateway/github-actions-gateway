# Q547 — Deleting an ActionsGateway orphans its in-flight worker pods

Deleting a v2 `ActionsGateway` removes the AGC that is the sole reaper of its tenant's worker pods, but nothing removes the pods.
They keep their node-disruption-safety annotations, so consolidation and scale-down leave them alone and a billable node stays pinned.
Observed on the GKE dogfood cluster 2026-07-31, on the teardown `scripts/dogfood/e2e-stop.sh` performs.

## Status

**Complete (2026-08-01).** Both halves shipped with the behaviour promoted into [04-operational-flows.md §4.1.1](../../design/04-operational-flows.md#411-tenant-teardown-and-child-reclamation) and [02-architecture.md](../../design/02-architecture.md); the operator-facing surface (the new reap reason, the two events, the `GatewayTerminating` condition, and the two runbooks) is in [troubleshooting.md](../../operations/troubleshooting.md#worker-pods-reaped-on-gateway-teardown-workerpodsreapedongatewayteardown).
No residual Queue row: §3.4's dead-AGC gap is an accepted trade-off, not deferred work.

| Phase | Scope | Status |
|---|---|---|
| 1 | AGC reaps its worker pods when its `ActionsGateway` is terminating | ✅ Done |
| 2 | GMC gates v2 teardown on the drain, bounded by a deadline | ✅ Done |
| 3 | Tests, docs, operator-facing behaviour | ✅ Done |

## 1. What was measured

All of this is read off the code, not inferred from the symptom.

**Worker pods already carry an owner reference.** Every worker pod and job Secret is stamped with a controller `OwnerReference` to its owning CR — [`pod.go:265`](../../../cmd/agc/internal/provisioner/pod.go), built by `Target.OwnerRef()` ([`runnergroup_target.go:41`](../../../cmd/agc/internal/provisioner/runnergroup_target.go) for v1, the RunnerSet target for v2).
So the Queue row's "add pod ownerRefs" option is already in place; it is the *owner* that survives, not the reference that is missing.

**v1 is not affected.** The v1 gateway's `reconcileDelete` ([`actionsgateway_controller.go:374`](../../../cmd/gmc/internal/controller/actionsgateway_controller.go)) deletes the gateway's `RunnerGroup` CRs first and waits for them to be gone.
RunnerGroups are the gateway's own children (`spec.runnerGroups`), so deleting them cascades to the worker pods through the owner reference, and the RunnerGroup's `agentpool-cleanup` finalizer orders the GitHub-side cleanup.

**v2 is affected, and it is a regression from the v2 API decomposition.** v2 RunnerSets are standalone tenant-authored CRs that only *reference* the gateway via `spec.gatewayRef`. v2 teardown deliberately leaves them alone — [`actionsgateway_v2_controller.go:828`](../../../cmd/gmc/internal/controller/actionsgateway_v2_controller.go): "RunnerSets reference the gateway but are not owned by it, so they are not deleted — they degrade to Ready=False/GatewayNotFound via their own watch."
That is the right call for the RunnerSet (a tenant can recreate the gateway and resume), but it means the worker pods' owner outlives their reaper.

**Nothing else reaps them.** The GMC holds no `pods` RBAC at all — the only `kubebuilder:rbac` marker for pods in the repo is the AGC's ([`cmd/agc/internal/controller/doc.go:10`](../../../cmd/agc/internal/controller/doc.go)).

**The bound is 12h, not "forever".** The Queue row overstates this.
The provisioner stamps `activeDeadlineSeconds` from `maxWorkerLifetime`, which defaults to 12h ([`provisioner.go:207`](../../../cmd/agc/internal/provisioner/provisioner.go)), and the kubelet enforces it with no controller involved (Q438).
An orphaned worker is therefore pinned for up to 12h by default — long enough to be the observed problem, and genuinely unbounded only when the tenant sets `maxWorkerLifetime: 0s` or its own `activeDeadlineSeconds` on the template.

**Namespace deletion is a different failure, already documented.** Deleting the tenant namespace does *not* strand worker pods — the namespace controller deletes them itself.
It deadlocks instead, on the `agentpool-cleanup` finalizers, because the AGC Deployment lives inside the namespace being swept; [migration-v1-to-v2.md § Teardown order is load-bearing](../../operations/migration-v1-to-v2.md#teardown-order-is-load-bearing-never-delete-the-namespace-first) calls it structural and [troubleshooting.md](../../operations/troubleshooting.md#tenant-namespace-stuck-terminating-on-agentpool-cleanup-finalizers) carries the recovery.
Q585 is the open row for the e2e spec that trips it.
Q547 is specific to *delete the gateway, keep the namespace* — the documented **correct** teardown order, and exactly what `e2e-stop.sh` does.

## 2. The ordering finding

The obvious cheap fix — have the AGC reap its workers from its SIGTERM handler — does not work, and the reason is worth recording so it is not re-proposed.

`reconcileDelete` deletes its children in this order:

| Order | Child |
|---|---|
| … | `ClusterRoleBinding` |
| 1 | `Deployment` (the AGC) — [`:863`](../../../cmd/gmc/internal/controller/actionsgateway_v2_controller.go) |
| … | `Service`, two `NetworkPolicy`s |
| 2 | `RoleBinding` (the AGC's) — [`:868`](../../../cmd/gmc/internal/controller/actionsgateway_v2_controller.go) |
| 3 | `ServiceAccount` (the AGC's) — [`:869`](../../../cmd/gmc/internal/controller/actionsgateway_v2_controller.go) |

Steps 1–3 are a handful of API round trips apart — milliseconds — while a SIGTERM reap needs seconds of graceful shutdown.
By the time the AGC could issue a delete, its `RoleBinding` is gone (authorization revoked) and its `ServiceAccount` is gone (its bound token no longer authenticates).
The reap would `403`.

Reordering does not rescue it either: the AGC needs both its RBAC *and* its pod alive, so any teardown that has started deleting children has already lost the race.
The reap has to happen **before** the GMC deletes anything.

## 3. Design

Two halves, settled 2026-08-01.

### 3.1 AGC — reap on `deletionTimestamp`

The RunnerSet reconciler already watches `ActionsGateway` ([`runnerset_controller.go:208`](../../../cmd/agc/internal/controller/runnerset_controller.go)), so setting a deletion timestamp on the gateway already wakes every bound RunnerSet.
On that signal the reconciler:

1. stops both listener tiers (classic multiplexer and scale-set listener), so no further job is acquired;
2. reaps **every** worker pod carrying the set's `LabelRunnerSet`, unconditionally — no TTL, no deadline — under a new `gateway_deleted` reap reason;
3. zeroes `status.activeJobs` / `status.pendingJobs` and sets `Ready=False` / `GatewayTerminating`.

The trigger is a **non-zero `deletionTimestamp` on a gateway that still exists**, deliberately not a `NotFound`. `GatewayNotFound` keeps today's behaviour: an operator who deletes and immediately recreates a gateway must not have live workers reaped in the gap, and an AGC that restarts after teardown finished has no way to tell that gap from a real teardown.

The check runs before reference resolution, not after, so a set whose template or proxy is also missing still reaps.

**Why the counts are a sufficient gate.** The shared reaper skips pods that already carry a `deletionTimestamp` before counting them ([`runner_shared.go:291`](../../../cmd/agc/internal/controller/runner_shared.go)), so the counts fall to zero as soon as the deletes are *issued*.
That is the right moment: once a pod has a deletion timestamp the kubelet finishes it with no controller involved, which is precisely the property Q547 is missing today.

### 3.2 GMC — gate teardown on the drain

`reconcileDelete` requeues **before deleting any child** while any RunnerSet in the gateway's namespace with `gatewayRef.name == ag.Name` reports `activeJobs + pendingJobs > 0`.
The GMC already holds `runnersets: get;list;watch` and already resolves this binding for the `RunnerSetsDegraded` rollup ([`evalRunnerSetHealth`](../../../cmd/gmc/internal/controller/actionsgateway_v2_controller.go)), so the gate needs no new permission and no new watch.

The deadline is derived from `ag.DeletionTimestamp` rather than held in memory, so it survives a GMC restart mid-teardown — the same stateless-deadline shape the reaper uses against `pod.CreationTimestamp`.
On expiry the GMC emits a `WorkerDrainTimeout` Warning naming the sets that never drained and proceeds with teardown.

Its value is **90s**, chosen against the caller rather than the healthy path — which resolves in seconds.
Every e2e teardown deletes its gateway with `kubectl delete --timeout=2m`, and an operator following the documented teardown order does something similar; a wait that reaches the caller's own budget turns the bounded case into a *failed* delete rather than a slow one.
The timeout only ever fires when no AGC is running to reap, and waiting longer in that case buys nothing.

Neither binary gains RBAC.
The AGC still holds its full tenant grant while it reaps, because the GMC has not deleted anything yet.

### 3.3 In-flight jobs are killed

Reaping is immediate: a worker running a job when the gateway is deleted is deleted with it.
This matches the existing contract — draining is the operator's job, and `e2e-stop.sh` already drains before deleting, with `SKIP_E2E_DRAIN=1` to override knowingly ([gke-dogfood.md § F3](../gke-dogfood.md#f3-e2e-operations--on-demand)).
Deleting a gateway means tear down now.

### 3.4 Accepted residual: a dead AGC

If the AGC is already crashed, scaled to zero, or was never healthy, nothing reaps and nothing updates the counts.
The GMC waits out its deadline, emits the Warning, and tears down; the pods are orphaned exactly as they are today, bounded by the 12h `activeDeadlineSeconds`.
This is strictly no worse than the current behaviour and is now legible in an event rather than silent.

Closing that residual would require the GMC to delete pods itself, which means cluster-wide `pods: list;delete` for a controller whose `clusterroles: bind` is deliberately `resourceNames`-scoped to `agc-tenant-role` and `agc-clusterrunnertemplate-reader` and which holds no `roles: create` — i.e. it cannot grant itself anything today.
That is a real posture change and is not taken here.

## 4. Alternatives rejected

| Option | Why not |
|---|---|
| Second owner reference from the pod to a gateway-scoped object | Kubernetes GC deletes a dependent only when **all** of its owners are gone. The RunnerSet owner survives, so nothing would cascade. |
| AGC reaps from its SIGTERM handler | Loses the teardown-ordering race — §2. |
| GMC deletes the pods itself | Needs cluster-wide pod delete for the GMC — §3.4. |
| GMC deletes the RunnerSets | Breaks the deliberate v2 contract that a tenant's RunnerSets survive gateway deletion and resume when it returns. |
| Shorten the default `maxWorkerLifetime` | Bounds the leak without fixing it, and trades away long-job support for every tenant to paper over a teardown bug. |
