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
| 3 | Storage migration, then drop `v2alpha1`, `v1alpha1`, and classic | M | ❌ Open ([Q273](../STATUS.md#Q273), [Q264](../STATUS.md#Q264)); capability parity gated on [Q417](../STATUS.md#Q417) |
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
| Eviction recovery (detect an evicted worker, rerun the job, per-run retry budget) | Classic only. `handleEviction` is reached from one call site inside classic `provision()`; `ProvisionScaleSetWorker` is fire-and-forget and never observes `PodFailed`/`Evicted`, so no scale-set eviction is detected and no rerun fires. | [Q417](../STATUS.md#Q417) must land before the classic removal, or `v2.0.0` ships without automatic eviction recovery. Evidence and plan: [scaleset-eviction-recovery.md](scaleset-eviction-recovery.md). |

This is a genuine gate, not a nice-to-have: eviction recovery is a headline
capability in [01-executive-summary.md](../design/01-executive-summary.md),
[README.md](../../README.md), and [why-gag.md](../why-gag.md), all of which describe
it as a property of the system rather than of one acquisition tier. Removing classic
without [Q417](../STATUS.md#Q417) would make those claims false at the same moment
the only tier that satisfied them disappears.

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
