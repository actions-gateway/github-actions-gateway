# v2 GA graduation plan (`v2beta1` → `v2`)

The last rung of the graduation ladder defined in [v2-api.md § API maturity & graduation](v2-api.md#api-maturity--graduation-v2alpha1--v2beta1--v2), and the release that executes the three coupled removals announced by [release-1.3.md](release-1.3.md).

This plan starts **after `v1.3.0` ships**.
It is deliberately unhurried: General Availability (GA) signs a permanent backward-compatibility contract on a five-kind API surface, and the contract cannot be walked back.

> **Status: parked — no active work, every phase carried by a Deferred trigger.** Phases 1, 2 and 4 wait on [Q413](../STATUS.md#Q413) (**Event:** `v1.3.0` ships, starting the Phase 1 soak); Phase 3's coupled removals wait on [Q273](../STATUS.md#Q273) and [Q264](../STATUS.md#Q264); the Phase 2 alias decision is [Q452](../STATUS.md#Q452).
> The `✅` on this plan's [Progress](../STATUS.md#progress) row means *no open Queue row remains*, not that the graduation has happened — deferred residuals [don't count](../development/maintaining-backlog.md#-means-an-open-queue-row-remains--deferred-residuals-dont-count).
> The phase table below is the real state.

## Status at a glance

| Phase | Scope | Sz | Status |
|---|---|---|---|
| 0 | Soak criteria + Definition of Done audit recorded (this change) | S | ✅ Done — this change |
| 1 | Beta soak: accumulate the evidence that `v2beta1`'s shape is right | M | ❌ Open ([Q413](../STATUS.md#Q413)) |
| 2 | Add `v2` to each kind, mark it storage, extend conversion coverage | M | ❌ Open ([Q413](../STATUS.md#Q413)) |
| 3 | Storage migration, then drop `v2alpha1`, `v1alpha1`, and classic | M | ❌ Open ([Q273](../STATUS.md#Q273), [Q264](../STATUS.md#Q264)); capability parity **cleared**: Q417/Q443/Q446 cleared the audit's three rows (2026-07-26), Q766 closed the abandoned-run asymmetry inside 1.4, and Q713 put the duration and latency series on both tiers (2026-08-11). See the [parity table](#capability-parity-is-a-precondition-of-the-removal) |
| 4 | Operator docs, migration guide, and the `v2.0.0` cut | S | ❌ Open ([Q413](../STATUS.md#Q413)) |

## Why this is gated on a soak, not a date

The graduation ladder in [v2-api.md](v2-api.md#api-maturity--graduation-v2alpha1--v2beta1--v2) sets the contract at each level:

| Level | Contract |
|---|---|
| `v2alpha1` | may change incompatibly or be dropped without notice |
| `v2beta1` | won't be removed; changes carry a migration path; production-relyable |
| `v2` (GA) | backward-compatible, effectively frozen |

The jump that matters is beta to GA.
Beta still permits a shape fix *with* a migration path; GA does not.
So the question this plan answers is not "has enough time passed" but "do we have evidence the shape is right."
Cutting GA early converts every remaining design mistake into a permanent one.

## Phase 1 — the soak (what "well validated" means)

GA is blocked until **all** of the following hold.
These are the evidence bar, and none of them is a calendar check.

1. **No incompatible `v2beta1` shape change has been needed** across at least two minor releases of real use.
   A field addition is fine.
   A field whose meaning, type, or defaulting had to change is a soak reset, because it is exactly the class of change GA forbids.
2. **Every kind has carried real traffic.** All five kinds exercised on the dogfood cluster, not just the two the CI path happens to use.
   An unexercised kind is an unvalidated kind.
3. **The conversion webhook has round-tripped every served version under real objects**, not only envtest fixtures, including objects created before the `v2beta1` graduation.
4. **The v2 GA Definition of Done in [v2-api.md](v2-api.md#definition-of-done-v2-ga) audits clean.** That list predates this plan and is authoritative; Phase 0 records the audit below rather than restating the criteria.

### Definition of Done audit (as of this change)

| DoD item | State |
|---|---|
| M1, M2, M3a, M3b, M5 shipped | ✅ Satisfied — see [v2-api.md](v2-api.md) milestones |
| Graduated `v2alpha1` → `v2beta1` (webhook + storage migration) | ✅ Satisfied — Q74 |
| Graduated `v2beta1` → `v2` | ❌ This plan, Phase 2 |
| `v1alpha1` deprecated **with a named removal release** | ✅ Satisfied: Q412 (2026-07-26) names `v2.0.0` across the operator and design docs, for `v1alpha1`, `v2alpha1`, and classic together. The notice ships with `v1.3.0`, one release ahead. |
| ≥1 representative tenant migrated v1→v2 with the tool for real | ✅ **Satisfied: Q415 (2026-07-28).** A live `v1alpha1` privileged-DinD tenant on the GKE dogfood cluster was migrated with `gag-migrate --apply` and then ran a real GitHub Actions DinD job green on the scale-set path, with a green baseline before the migration for attribution. Evidence: [q415-migrate-dogfood-validation.md](archive/q415-migrate-dogfood-validation.md#part-2--the-job-half-run-after-the-workflow-reached-main-2026-07-28). Found four defects — Q463, Q465, Q466, Q467 — none of which gate this row. |
| Operator docs updated | ❌ Phase 4 |
| Cross-namespace sharing (M4), direct egress | Not GA gates, by the DoD |

## Phase 2 — the graduation hop

Mechanically identical to the `v2alpha1` → `v2beta1` hop, which is the useful part: the machinery already exists and does not need redesigning.
Per [v2-api.md](v2-api.md#api-maturity--graduation-v2alpha1--v2beta1--v2), each hop is:

1. Add the `v2` version to each of the five kinds and mark it `+kubebuilder:storageversion`.
2. Extend the existing `Hub`/`Convertible` conversion webhook to round-trip the new served set.
   The hub moves to `v2`.
3. Storage-migrate stored objects, then drop the superseded served version.

Two project-specific constraints carry over from the last hop and should be read before starting: shared version-neutral code lives in `api/apiconditions` with one-line re-exports per version, and `check-v2-api-sync.sh` gates every shared v2 file.
Getting this wrong is the most likely way to break the hop.

**One shape decision the hop must make: does `v2` define `CiliumFQDN`/`CalicoFQDN`?** The two deprecated `EgressProxy.spec.egressPolicyMode` aliases cannot be removed at `v2.0.0` — they are members of the served beta version `v2beta1`, so their floor is `v3.0.0` ([Q428](../operations/v1alpha1-deprecation.md#a-fourth-deprecation-on-a-different-clock-ciliumfqdn--calicofqdn)).
That leaves a live choice for `v2`: omit them (a clean GA surface, but the hub moves to `v2` and a `v2beta1` object naming an alias then needs a lossless carrier, since the conversion deliberately rides the values across verbatim rather than collapsing them to `FQDN`) or carry them deprecated (trivially lossless, but GA is effectively frozen and would be born owning two aliases).
Either way the removal release is unchanged.
Decide it here, not at the tag: [Q452](../STATUS.md#Q452).

## Phase 3 — the coupled removals

`v2.0.0` executes all three removals announced by [release-1.3.md](release-1.3.md):

- `v1alpha1` (the `actions-gateway.github.com` group) — [Q273](../STATUS.md#Q273)
- `v2alpha1` — this plan
- classic acquisition machinery and the transitional `acquisitionProtocol` / `maxListeners` fields — [Q264](../STATUS.md#Q264)

They are one bundle because `v2beta1` is already ScaleSet-only: classic acquisition exists solely to serve `v1alpha1` and `v2alpha1` objects, so removing those versions removes classic's only consumer.
Sequencing within the release still matters, since the Q147 dual-read window closes exactly when `v1alpha1` is removed.
Order: storage-migrate first, drop served versions second, then strip the dual-read arms from the `ValidatingAdmissionPolicy` objects and the validating webhook.

### Capability parity is a precondition of the removal

Removing classic must not delete a capability along with it.
Dropping a served API version is a contract change operators can plan for; silently losing a behaviour they rely on is not, and it is exactly what the deprecation policy exists to prevent.

| Capability | State | Gate |
|---|---|---|
| Eviction recovery (detect an evicted worker, rerun the job, per-run retry budget) | ✅ **Both tiers.** Q417 ported it: the scale-set assignment's run identity is stamped on the worker pod, the owning reconciler detects `PodFailed`/`Evicted` and claims the pod set-once before calling `rerun-failed-jobs`, and the Q106 per-run budget is shared across tiers. | Cleared. Design: [04-operational-flows.md § On the scale-set tier](../design/04-operational-flows.md#on-the-scale-set-tier-q417). Plan: [scaleset-eviction-recovery.md](scaleset-eviction-recovery.md). |
| Pre-claim quota gate (refuse work the namespace `ResourceQuota` cannot place, rather than claim it and stall) | ✅ **Both tiers.** Q443 ported it: the ladder `Provisioner.Admit` walks per delivered job is also expressed as an integer (`AdvertiseCapacity`), and the scale-set tier advertises `min(ceiling, own in-flight pods + quota headroom)` as `X-ScaleSetMaxCapacity` — so a quota-blocked job is never assigned at all. | Cleared. Design: [04-operational-flows.md § The ladder as an integer](../design/04-operational-flows.md#the-ladder-as-an-integer-scale-set-tier-q443). Plan: [capacity-aware-intake.md §9a](capacity-aware-intake.md#9a-the-shipped-quota-rung-was-classic-only-q443). |
| Abandoned-run force-cancel and automatic re-run (a worker removed before it ran) | ✅ **Both tiers.** Q683 and Q691 shipped classic-only inside 1.4 and Q766 ported both in the same release, before any tag published the asymmetry. The scale-set detection reads the run identity from the worker pod's annotations where classic reads it from the payload it holds, which is what the `tier` label on `abandoned_run_force_cancels_total` splits. | Cleared. Design: [04-operational-flows.md § On the scale-set tier](../design/04-operational-flows.md#on-the-scale-set-tier-q766). Plan: [release-1.4.md](release-1.4.md). |
| Job duration and pod-creation latency (`job_duration_seconds`, `pod_creation_latency_seconds`) | ✅ **Both tiers.** Both are observed off the shared pod informer, which sees a scale-set worker pod and a classic one identically, so neither depends on the waiter a fire-and-forget provision never registers. `job_duration_seconds` is worker pod lifetime on both tiers, one span rather than one per tier. | **Cleared** by Q713 (2026-08-11). |
| Restart-safe disruption recovery (a preempted or drained worker whose AGC was down for the teardown) | ✅ **Both tiers.** Q844 ported it: the classic provisioning goroutine reads the disruption off the resolving event it already watches, while a scale-set worker is readable only while it terminates, so an AGC down for that window listed no pod and issued no re-run. The listener now persists the run identity behind every worker it builds in the per-`RunnerSet` guard ConfigMap, and the reconciler re-runs any whose pod is gone at startup, reported as `eviction_retries_total{cause="vanished"}` because which disruption took the worker went with the pod. | Cleared. Design: [04-operational-flows.md](../design/04-operational-flows.md#why-preemption-deletes-rather-than-evicts-and-what-that-costs-us). Plan: [archive/q844-owed-rerun-tombstone.md](archive/q844-owed-rerun-tombstone.md). |
| Poll-error rate observability (`message_poll_errors_total`) | ✅ **Both tiers.** Q446 closed the counter half: `handlePollError` increments the same `actions_gateway_message_poll_errors_total{namespace, reason}` series the classic listener writes, under the same reason vocabulary (`rate_limited`, `timeout`, `other`), with the 401/403 and 404/410 heal branches counting nothing exactly as classic does — so an existing dashboard or alert keeps its meaning. The conditions (`Degraded`/`Unauthorized`, `RateLimited` after a sustained episode) stay as the state half. | Cleared. Metric: [observability-metrics.md](../operations/observability-metrics.md). |

### What this audit checked, and found already covered

Recorded so the cut does not re-derive it.
Method: walk the tier seams — the `ScaleSet` early return in `runnerset_controller.go`, `provision()` versus `ProvisionScaleSetWorker`, and the `listener/` versus `scalesetlistener/` packages — then cross-check every capability the README, roadmap, and why-gag present as a property of the system.

**Confirmed on both tiers** (wired before the protocol route, or ported): worker-capacity conditions `WorkerQuotaPressure`/`WorkerQuotaExceeded`/ `WorkersUnschedulable` (Q303, explicitly "identical to the classic path"), the opt-in scale-up rate limit `spec.scaleUp`, the measured sizing recommendation and `SizingDrift` condition (Q359), the worker-pod reaper including `orphaned_running` (Q420), and eviction recovery (Q417).

**Correctly absent from the scale-set tier** — artifacts of the many-acquirers and JIT-agent models that `ScaleSet` removes by construction, so they should disappear *with* classic rather than be ported: `jobs_duplicate_delivery_total`, `abandoned_delivery_completions_total`, `fanout_loser_recycle_deferred_total`, `agent_recycles_total`, `agent_recycle_errors_total`, `broker_token_propagation_retries_total`, `broker_session_leaks_total`, `renew_job_errors_total`, and `renew_job_teardowns_total`.
Each measures a race or a repair that only exists because many sessions acquire against one pool, or a call the AGC only makes on the classic tier: a scale-set runner renews and completes its own job, so there is no `renewjob` for the gateway to fail at.

The last two joined the list from the Q776 re-walk below.
That list is prose and this section is archived at the cut, so the machine-readable form is the [acquisition-tier ledger](../operations/observability-metrics.md#acquisition-tier-reach), which carries every `actions_gateway_*` series rather than only the omissions, and which `make metric-tiers-check` holds to the source.
The two are reconciled by that gate in both directions, so a name here that the ledger does not call classic-only fails.

**Alerting already has its analog:** `job_acquisition_errors_total` is classic-only, but [observability-alerting.md](../operations/observability-alerting.md) ships `actions_gateway:scaleset_provision_success_rate:rate5m` alongside the classic `job_acquisition_success_rate`, so the shipped rules do not go silent at the cut. `active_sessions` is likewise classic-only with `scaleset_jobs_assigned_total` as the documented substitute.

Both were genuine gates, not nice-to-haves: eviction recovery and the pre-claim quota gate are headline capabilities in [01-executive-summary.md](../design/01-executive-summary.md), [README.md](../../README.md), and [why-gag.md](../why-gag.md), all of which describe them as properties of the system rather than of one acquisition tier.
Removing classic before Q417 and Q443 landed would have made those claims false at the same moment the only tier that satisfied them disappeared.
With both ports in, the claims survive the cut.

The two were found the same way and a day apart, which is the reusable lesson: a capability wired into a call site the default tier never reaches reads as shipped from every angle except the one that matters — the tier tenants actually run.
A claim about what GAG does is not verified until it names the acquisition tier it was verified on.

One residual is worth knowing about but does not gate the removal, because classic shares it: a worker pod force-deleted with no grace period (or lost with its node) leaves no `Failed`/`Evicted` object and no chance for the runner to report, so neither tier recovers it.
Q435 measured the adjacent orphan-reclaim question and [Q438](archive/q438-worker-lifetime-deadline.md) closed its residual with a provision-time worker lifetime cap.

Any further capability found to be classic-only before the cut joins this table and gates the same removal.

**Four joined it after the audit, which is the rule working rather than failing.** The abandoned-run recovery opened and closed inside 1.4: Q683 and Q691 shipped classic-only, Q766 ported them, and no tag ever published the asymmetry.
Q713 was the third and the longest-lived: the duration and latency series were the one capability with no scale-set analog to substitute, so it took a port rather than a caveat, and it closed 2026-08-11 by moving both observations onto the shared pod informer.
Q844 was the fourth, on 2026-08-14: restart-safe disruption recovery was classic-only because a scale-set worker is readable only while it terminates, and the ScaleSet tier now persists the run behind every worker it builds.
Read the table, not this section's history, for the state of the gate.

The audit's method has a known blind spot both instances share.
It walked the tier seams once, in July, and a capability added to `provision()` afterwards is classic-only from birth without anything re-walking them.
Q683, Q691 and Q713 all arrived that way, and Q844 made four on 2026-08-14, found by hand rather than by any gate.

**The metric surface now re-walks itself.** Q776 closed that half: every `actions_gateway_*` series the AGC defines carries a tier in the [acquisition-tier ledger](../operations/observability-metrics.md#acquisition-tier-reach), and `make metric-tiers-check` fails a series added without one, a ledger row the source refutes, and a name on the absent-by-design list above that the ledger does not call classic-only.
The re-walk it required found two: `renew_job_errors_total` and `renew_job_teardowns_total` were classic-only by construction and on no list, and `eviction_recovery_evidence_lost_total` reached no operator doc at all.

That gate covers metrics, not capabilities, so the manual step survives for anything with no series behind it: Q844 had one only because the recovery it ported reports through a counter that already spanned both tiers.
Until [Q774](../STATUS.md#Q774) gates scope statements mechanically, adding a row to the table above is still a manual step in the change that creates the asymmetry, and the [doc-update matrix](../development/doc-update-matrix.md) is where that obligation is written down.

## Phase 4 — docs and the cut

Operator-facing surface that must be current before the tag:

- [v1alpha1-deprecation.md](../operations/v1alpha1-deprecation.md) — becomes a historical record rather than an active notice.
- [migration-v1-to-v2.md](../operations/migration-v1-to-v2.md) — the pre-upgrade migration becomes mandatory rather than at-your-convenience.
- [upgrade.md](../operations/upgrade.md) — the upgrade-past-removal path.
- [roadmap.md](../roadmap.md) — the graduation line stops being forward-looking.

## Guardrails

- **A soak reset is not a failure.** If Phase 1 surfaces a shape problem, fixing it in beta with a migration path is the system working.
  Shipping GA over a known shape problem to hold a date is the failure.
- **Deprecation is not removal.** `v2alpha1` stays fully served between `v1.3.0` and `v2.0.0`.
  Nothing is forced on any operator before the major tag.
- **`v2.0.0` is the only breaking release.** Everything between it and `v1.3.0` stays backward-compatible, so operators get exactly one migration event.
