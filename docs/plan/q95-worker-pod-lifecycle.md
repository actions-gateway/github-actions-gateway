# Q95 — Worker pod lifecycle: cleanup of completed and stuck pods

**Goal:** Worker pods must never accumulate: completed pods are deleted after a
bounded retention window, stuck-Pending pods are reaped after a deadline (freeing
their concurrency-ceiling slot), and tenant/CR deletion garbage-collects all
worker pods via OwnerReferences.

**Bug being fixed:** the provisioner creates worker pods with no OwnerReference
and no deadline, and the post-job cleanup path deletes only the per-pod job
Secret — never the pod. Completed pods accumulate indefinitely (the design docs
wrongly claimed they were garbage-collected), and a stuck-Pending pod (image
pull failure, unschedulable) holds a ceiling slot forever because
`activePodCount` counts Pending pods and `waitForCompletion` never returns.

## Design decisions

### 1. OwnerReference → RunnerGroup (pod + job Secret)

Both the worker pod and its job-payload Secret get a controller OwnerReference
to the owning RunnerGroup (same namespace, so a namespaced ownerRef is legal).
Deleting the RunnerGroup — directly, via `ActionsGateway` teardown, or via
namespace deletion — lets the Kubernetes GC controller cascade-delete every
worker pod and staged Secret, including ones orphaned by an AGC crash.

`blockOwnerDeletion` is left unset: the RunnerGroup already has a finalizer for
agent deregistration, and setting it would require the `finalizers` permission
under the `OwnerReferencesPermissionEnforcement` admission plugin.

### 2. Completed-pod TTL — `spec.completedPodTTL` (default 5m)

New optional `RunnerGroup` field `completedPodTTL` (`metav1.Duration`):
how long a worker pod in a terminal phase (Succeeded/Failed/Unknown) is
retained before the AGC deletes it.

- **Default (field omitted): 5m.** A short retention window so an operator can
  `kubectl logs`/`describe` a just-failed pod (job logs stream to GitHub, but a
  pod that crashes before the runner starts leaves its only evidence in the pod),
  while keeping accumulation bounded at (job rate × TTL). Terminal pods consume
  no compute and no ResourceQuota.
- **`0s` = delete immediately on completion**: the provision goroutine deletes
  the pod in its cleanup step, right after recording phase/duration/eviction.
- CEL: must be ≥ `0s` (rejects negative durations).

Enforcement is reconcile-driven (see §4), so it also covers pods orphaned by an
AGC restart — the goroutine-side delete is only the `0s` fast path.

### 3. Stuck-Pending deadline — `spec.pendingPodDeadline` (default 10m)

New optional `RunnerGroup` field `pendingPodDeadline` (`metav1.Duration`): the
maximum time a worker pod may remain Pending (measured from creation) before
the AGC deletes it. Deletion resolves the waiting session goroutine (the
`InformerPodWaiter` treats pod deletion as completion), which releases the
listener and the ceiling slot; the job Secret is deleted by that goroutine's
normal cleanup path.

- **Default (field omitted): 10m** — generous for image pulls, far below
  "forever". Clusters with slow autoscaling (e.g. GPU node provisioning) should
  raise it.
- CEL: must be ≥ `1s` (a 0 deadline would reap every pod at admission age 0;
  there is deliberately no way to disable the deadline — an unbounded Pending
  pod is a capacity leak, so the guard is secure/robust by default).
- `pod.spec.activeDeadlineSeconds` is **not** usable here: it is enforced by
  the kubelet, and an unscheduled Pending pod has no kubelet.

Each reap emits a Warning Event (`WorkerPodStuckPending`) on the RunnerGroup
and increments the new counter metric.

### 4. Enforcement: reaper in the RunnerGroup reconciler

A `reapWorkerPods` step runs early in `Reconcile` (before the GitHub token
fetch, so reaping keeps working during GitHub outages). It lists the group's
worker pods from the shared informer cache (label
`actions-gateway/runner-group`), deletes any terminal pod older than the TTL
and any Pending pod older than the deadline, and returns the time until the
next pod becomes due, which the reconciler propagates as `RequeueAfter`.

Why the reconciler and not the provision goroutine: the goroutine dies with the
AGC process, so it can never be the mechanism of record — a reconcile-driven
reaper is restart-safe and also covers pods whose goroutine is gone. The
existing Pod watch already re-triggers reconcile on phase transitions; the
`RequeueAfter` covers the purely time-based expiries in between.

Terminal-pod age is measured from the latest container `terminated.finishedAt`
(set by the kubelet), falling back to the pod CreationTimestamp when absent.

### 5. Metric

`actions_gateway_worker_pods_reaped_total{namespace, runner_group, reason}`
with `reason` ∈ `completed_ttl` | `pending_deadline`. Goroutine-side immediate
deletes (TTL `0s`) are not counted — the metric tracks reaper interventions an
operator may want to alert on (especially `pending_deadline`).

### Out of scope

- **Stuck-Running pods**: a Running pod is bounded by GitHub's job-level
  timeout and the job-lock renewal contract (lock lapse → GitHub cancels →
  runner exits). No AGC-side deadline is added for Running pods.
- **Reaping pods of deleted RunnerGroups**: covered by ownerRef GC. Pods
  created by pre-Q95 AGC versions whose RunnerGroup was deleted have neither an
  ownerRef nor a reconcile trigger; the upgrade note tells operators to clean
  these up once by label.
- **RunnerGroup spec updates not reaching cached multiplexer job-handler
  closures** (e.g. `workerImage` changes need an AGC restart) — pre-existing
  behaviour, flagged separately if confirmed.

## Test plan

| Tier | What it proves |
|---|---|
| Unit (fake client) | ownerRef stamped on pod + Secret; `0s` TTL deletes pod in goroutine cleanup; >0 TTL retains; reaper deletes terminal-past-TTL and Pending-past-deadline pods, keeps fresh ones, computes RequeueAfter |
| envtest (`cmd/agc/internal/controller/integration/`) | against a real apiserver with the manager + reconciler running: a pod set to Succeeded is deleted after `completedPodTTL`; a never-scheduled Pending pod (envtest has no scheduler) is deleted after `pendingPodDeadline` |
| Tier-A kind e2e (`cmd/gmc/test/e2e/`) | end-to-end through GMC→AGC→fakegithub: worker pod carries the RunnerGroup ownerRef; a completed worker pod is deleted after the TTL; a stuck-Pending pod (unpullable image) is reaped after the deadline and the Secret is gone |

envtest cannot prove the ownerRef cascade (no kube-controller-manager / GC
controller in envtest), hence the ownerRef assertion at Tier A.

## Doc updates

- `docs/design/02-architecture.md` — fix the false "AGC garbage-collects the
  pod" claim (§2.2 job-completion path), the Pod-watch prose, metrics table.
- `docs/design/03-api-contracts.md` — the two new fields.
- `docs/design/04-operational-flows.md` — fix step 11 "Reclaim" prose; add a
  stuck-Pending failure flow.
- `docs/design/07-test-plan.md` — integration/e2e criteria for cleanup.
- `docs/operations/troubleshooting.md` — runbook: worker pod stuck Pending /
  reaped (symptom: Warning event + metric), CEL rejection of bad durations,
  one-time cleanup of pre-Q95 accumulated pods.
- `docs/operations/tenant-onboarding.md` — document the two knobs + defaults.
- `docs/operations/observability.md` — new metric + event.
- `docs/operations/upgrade.md` — one-time cleanup note for pre-existing pods.

## Progress

- [x] Plan committed
- [ ] API fields + CEL + deepcopy + CRD regen (agc + gmc) + chart CRD sync
- [ ] Provisioner: ownerRefs, `0s`-TTL delete-on-completion
- [ ] Reaper in RunnerGroup reconciler + metric + events
- [ ] Unit tests (provisioner + reaper)
- [ ] envtest integration tests
- [ ] Tier-A e2e test
- [ ] Doc updates
- [ ] `make check` green; STATUS row removed
