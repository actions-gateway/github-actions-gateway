# v2 GA graduation plan (`v2beta1` → `v2`)

The last rung of the graduation ladder defined in
[v2-api.md § API maturity & graduation](v2-api.md#api-maturity--graduation-v2alpha1--v2beta1--v2),
and the release that executes the three coupled removals announced by
[release-1.3.md](release-1.3.md).

This plan starts **after `v1.3.0` ships**. It is deliberately unhurried: General
Availability (GA) signs a permanent backward-compatibility contract on a five-kind
API surface, and the contract cannot be walked back.

## Status at a glance

| Phase | Scope | Sz | Status |
|---|---|---|---|
| 0 | Soak criteria + Definition of Done audit recorded (this change) | S | ✅ Done — this change |
| 1 | Beta soak: accumulate the evidence that `v2beta1`'s shape is right | M | ❌ Open ([Q413](../STATUS.md#Q413)) |
| 2 | Add `v2` to each kind, mark it storage, extend conversion coverage | M | ❌ Open ([Q413](../STATUS.md#Q413)) |
| 3 | Storage migration, then drop `v2alpha1`, `v1alpha1`, and classic | M | ❌ Open ([Q273](../STATUS.md#Q273), [Q264](../STATUS.md#Q264)); capability parity cleared — Q417 shipped 2026-07-26 |
| 4 | Operator docs, migration guide, and the `v2.0.0` cut | S | ❌ Open ([Q413](../STATUS.md#Q413)) |

## Why this is gated on a soak, not a date

The graduation ladder in [v2-api.md](v2-api.md#api-maturity--graduation-v2alpha1--v2beta1--v2)
sets the contract at each level:

| Level | Contract |
|---|---|
| `v2alpha1` | may change incompatibly or be dropped without notice |
| `v2beta1` | won't be removed; changes carry a migration path; production-relyable |
| `v2` (GA) | backward-compatible, effectively frozen |

The jump that matters is beta to GA. Beta still permits a shape fix *with* a
migration path; GA does not. So the question this plan answers is not "has enough
time passed" but "do we have evidence the shape is right." Cutting GA early converts
every remaining design mistake into a permanent one.

## Phase 1 — the soak (what "well validated" means)

GA is blocked until **all** of the following hold. These are the evidence bar, and
none of them is a calendar check.

1. **No incompatible `v2beta1` shape change has been needed** across at least two
   minor releases of real use. A field addition is fine. A field whose meaning,
   type, or defaulting had to change is a soak reset, because it is exactly the class
   of change GA forbids.
2. **Every kind has carried real traffic.** All five kinds exercised on the dogfood
   cluster, not just the two the CI path happens to use. An unexercised kind is an
   unvalidated kind.
3. **The conversion webhook has round-tripped every served version under real
   objects**, not only envtest fixtures, including objects created before the
   `v2beta1` graduation.
4. **The v2 GA Definition of Done in
   [v2-api.md](v2-api.md#definition-of-done-v2-ga) audits clean.** That list predates
   this plan and is authoritative; Phase 0 records the audit below rather than
   restating the criteria.

### Definition of Done audit (as of this change)

| DoD item | State |
|---|---|
| M1, M2, M3a, M3b, M5 shipped | ✅ Satisfied — see [v2-api.md](v2-api.md) milestones |
| Graduated `v2alpha1` → `v2beta1` (webhook + storage migration) | ✅ Satisfied — Q74 |
| Graduated `v2beta1` → `v2` | ❌ This plan, Phase 2 |
| `v1alpha1` deprecated **with a named removal release** | ✅ Satisfied: Q412 (2026-07-26) names `v2.0.0` across the operator and design docs, for `v1alpha1`, `v2alpha1`, and classic together. The notice ships with `v1.3.0`, one release ahead. |
| ≥1 representative tenant migrated v1→v2 with the tool for real | ⚠️ **Unverified.** Dogfood runs v2, but whether a v1→v2 `gag-migrate` run was ever exercised end-to-end on a real tenant needs confirming before GA, not asserting. Phase 1 item. |
| Operator docs updated | ❌ Phase 4 |
| Cross-namespace sharing (M4), direct egress | Not GA gates, by the DoD |

## Phase 2 — the graduation hop

Mechanically identical to the `v2alpha1` → `v2beta1` hop, which is the useful part:
the machinery already exists and does not need redesigning. Per
[v2-api.md](v2-api.md#api-maturity--graduation-v2alpha1--v2beta1--v2), each hop is:

1. Add the `v2` version to each of the five kinds and mark it
   `+kubebuilder:storageversion`.
2. Extend the existing `Hub`/`Convertible` conversion webhook to round-trip the new
   served set. The hub moves to `v2`.
3. Storage-migrate stored objects, then drop the superseded served version.

Two project-specific constraints carry over from the last hop and should be read
before starting: shared version-neutral code lives in `api/apiconditions` with
one-line re-exports per version, and `check-v2-api-sync.sh` gates every shared v2
file. Getting this wrong is the most likely way to break the hop.

## Phase 3 — the coupled removals

`v2.0.0` executes all three removals announced by
[release-1.3.md](release-1.3.md):

- `v1alpha1` (the `actions-gateway.github.com` group) — [Q273](../STATUS.md#Q273)
- `v2alpha1` — this plan
- classic acquisition machinery and the transitional `acquisitionProtocol` /
  `maxListeners` fields — [Q264](../STATUS.md#Q264)

They are one bundle because `v2beta1` is already ScaleSet-only: classic acquisition
exists solely to serve `v1alpha1` and `v2alpha1` objects, so removing those versions
removes classic's only consumer. Sequencing within the release still matters, since
the Q147 dual-read window closes exactly when `v1alpha1` is removed. Order:
storage-migrate first, drop served versions second, then strip the dual-read arms
from the `ValidatingAdmissionPolicy` objects and the validating webhook.

### Capability parity is a precondition of the removal

Removing classic must not delete a capability along with it. Dropping a served API
version is a contract change operators can plan for; silently losing a behaviour
they rely on is not, and it is exactly what the deprecation policy exists to
prevent.

| Capability | State | Gate |
|---|---|---|
| Eviction recovery (detect an evicted worker, rerun the job, per-run retry budget) | ✅ **Both tiers.** Q417 ported it: the scale-set assignment's run identity is stamped on the worker pod, the owning reconciler detects `PodFailed`/`Evicted` and claims the pod set-once before calling `rerun-failed-jobs`, and the Q106 per-run budget is shared across tiers. | Cleared. Design: [04-operational-flows.md § On the scale-set tier](../design/04-operational-flows.md#on-the-scale-set-tier-q417). Plan: [scaleset-eviction-recovery.md](scaleset-eviction-recovery.md). |
| Pre-claim quota gate (decline to claim a job the namespace `ResourceQuota` cannot place, leaving it queued for a sibling) | ❌ **Classic only.** `Provisioner.Admit`'s headroom rung is wired from `AdmitFor` and the classic branch of the RunnerSet reconciler; `reconcileScaleSetListener` returns before it. A scale-set set advertises `X-ScaleSetMaxCapacity` from `target.Ceiling` — the Q59 concurrency rung — and consults no quota headroom, so a quota-blocked job is assigned and then retried in place. | **Open — gates the cut.** [Q443](../STATUS.md#Q443). Decision and design: [capacity-aware-intake.md §9a](capacity-aware-intake.md#the-decision-port-the-rung-and-treat-it-as-a-20-gate-q443). |
| Poll-error rate observability (`message_poll_errors_total`) | ⚠️ **Conditions yes, counter no.** `handlePollError` reaches deliberate condition parity (`Degraded`/`Unauthorized` on a rejected refresh, `RateLimited` after a sustained 429 episode) but increments no counter, so there is no rate-able signal — only a binary condition that trips after the episode outlasts `rateLimitAfter`. | **Open — does not gate.** [Q446](../STATUS.md#Q446). Conditions cover the operator-visible states, so this is a fidelity gap, not a lost capability. |

### What this audit checked, and found already covered

Recorded so the cut does not re-derive it. Method: walk the tier seams — the
`ScaleSet` early return in `runnerset_controller.go`, `provision()` versus
`ProvisionScaleSetWorker`, and the `listener/` versus `scalesetlistener/`
packages — then cross-check every capability the README, roadmap, and why-gag
present as a property of the system.

**Confirmed on both tiers** (wired before the protocol route, or ported):
worker-capacity conditions `WorkerQuotaPressure`/`WorkerQuotaExceeded`/
`WorkersUnschedulable` (Q303, explicitly "identical to the classic path"), the
opt-in scale-up rate limit `spec.scaleUp`, the measured sizing recommendation and
`SizingDrift` condition (Q359), the worker-pod reaper including
`orphaned_running` (Q420), and eviction recovery (Q417).

**Correctly absent from the scale-set tier** — artifacts of the many-acquirers
and JIT-agent models that `ScaleSet` removes by construction, so they should
disappear *with* classic rather than be ported:
`jobs_duplicate_delivery_total`, `abandoned_delivery_completions_total`,
`fanout_loser_recycle_deferred_total`, `agent_recycles_total`,
`agent_recycle_errors_total`, `broker_token_propagation_retries_total`, and
`broker_session_leaks_total`. Each measures a race or a repair that only exists
because many sessions acquire against one pool.

**Alerting already has its analog:** `job_acquisition_errors_total` is
classic-only, but [observability-alerting.md](../operations/observability-alerting.md)
ships `actions_gateway:scaleset_provision_success_rate:rate5m` alongside the
classic `job_acquisition_success_rate`, so the shipped rules do not go silent at
the cut. `active_sessions` is likewise classic-only with
`scaleset_jobs_assigned_total` as the documented substitute.

This was a genuine gate, not a nice-to-have: eviction recovery is a headline
capability in [01-executive-summary.md](../design/01-executive-summary.md),
[README.md](../../README.md), and [why-gag.md](../why-gag.md), all of which describe
it as a property of the system rather than of one acquisition tier. Removing classic
before Q417 landed would have made those claims false at the same moment the only tier
that satisfied them disappeared. With the port in, the claims survive the cut.

One residual is worth knowing about but does not gate the removal, because classic
shares it: a worker pod force-deleted with no grace period (or lost with its node)
leaves no `Failed`/`Evicted` object and no chance for the runner to report, so neither
tier recovers it. Q435 measured the adjacent orphan-reclaim question and
[Q438](../STATUS.md#Q438) carries its residual.

Any further capability found to be classic-only before the cut joins this table and
gates the same removal.

## Phase 4 — docs and the cut

Operator-facing surface that must be current before the tag:

- [v1alpha1-deprecation.md](../operations/v1alpha1-deprecation.md) — becomes a
  historical record rather than an active notice.
- [migration-v1-to-v2.md](../operations/migration-v1-to-v2.md) — the pre-upgrade
  migration becomes mandatory rather than at-your-convenience.
- [upgrade.md](../operations/upgrade.md) — the upgrade-past-removal path.
- [roadmap.md](../roadmap.md) — the graduation line stops being forward-looking.

## Guardrails

- **A soak reset is not a failure.** If Phase 1 surfaces a shape problem, fixing it
  in beta with a migration path is the system working. Shipping GA over a known
  shape problem to hold a date is the failure.
- **Deprecation is not removal.** `v2alpha1` stays fully served between `v1.3.0` and
  `v2.0.0`. Nothing is forced on any operator before the major tag.
- **`v2.0.0` is the only breaking release.** Everything between it and `v1.3.0` stays
  backward-compatible, so operators get exactly one migration event.
