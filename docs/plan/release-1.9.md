# Release 1.9 Milestone Definition

> **Status: all three gates landed (Q1085, Q413, then Q1150); not yet tagged.** Scoped 2026-09-17; Q1150 added 2026-09-30.
> The rung exists because Rule #4b requires it rather than because a defect asked for it, and both conditions the shape was held behind are now discharged: 1.8's soak readings came back positive on 2026-09-14, and `v1.8.0` tagged the same day.
> The version is no longer provisional.
> It was held against a negative reading naming a `v2beta1` shape fix that would have landed first; the readings were positive, so nothing displaces this rung.
> Nothing here is a commitment to a date.

## Why this release exists

`v2.0.0` advances the storage version to `v2` and drops `v2beta1`, `v2alpha1`, `v1alpha1` and classic acquisition.
It cannot also be the release that *introduces* `v2`: Kubernetes' [deprecation policy](https://kubernetes.io/docs/reference/using-api/deprecation-policy/) rule #4b says the storage version "may not advance until after a release has been made that supports both the new version and the previous version".

The rule is a convention rather than something the apiserver enforces, so what it buys here is the argument, and it is **rollback**.
Once stored objects are rewritten as `v2`, a cluster cannot return to a release whose CustomResourceDefinitions do not define `v2`.
Without this rung that destination is `v1.8.0`, which would make `v2.0.0` the one upgrade with no way back.
It is already the largest this project asks anyone to make, carrying the `v1`→`v2` migration and four removals.

It is also the only place the `v2beta1` ↔ `v2` conversion edge runs before it is mandatory.
[v2-ga.md](v2-ga.md#phase-1--the-soak-what-well-validated-means)'s Phase 1 soak validates `v2beta1`'s shape and says nothing about a conversion that does not exist yet.

Full reasoning: [release-ladder.md](release-ladder.md#why-19-exists-the-storage-version-cannot-advance-in-the-same-release-that-introduces-v2).

## What this release must NOT do

**It must not mark `v2` the storage version**, and must not migrate stored objects.
That is the whole point of the rung, and it is the one way to ship 1.9 and still not satisfy Rule #4b.
[v2-ga.md](v2-ga.md#phase-2--the-graduation-hop) Phase 2's step 1 carries a `+kubebuilder:storageversion` marker that belongs to Phase 3 and `v2.0.0`; moving it was part of Q413, not a follow-up.

The hub stays at `v2beta1`, though nothing ties it to the storage version: moving it buys nothing before `v2.0.0` leaves one served version, and it would put the alias conversion annotation on every `v2alpha1` and `v2beta1` conversion ([why](v2-ga.md#the-hub-stays-at-v2beta1)).

## Scope ledger

Three gating rows and the candidate validation, in the order they land, then the merged work that rides.

| Q-ID | Item | Gates? | Status |
|---|---|---|---|
| Q1085 | Admission rejects new `CiliumFQDN`/`CalicoFQDN` writes, and the pre-upgrade alias check joins the checklist | `1.9-gate` | ✅ landed first, ahead of Q413: the reject is in the GMC webhook and the check is in the [Pre-Upgrade Validation Checklist](../operations/upgrade.md#before-upgrading-to-v200-no-egressproxy-still-names-a-deprecated-fqdn-alias) |
| Q413 | [v2-ga.md](v2-ga.md#phase-2--the-graduation-hop) Phase 2: add `v2` to all five kinds, serve it beside `v2beta1`, extend conversion coverage. Storage marker withheld | `1.9-gate` | ✅ landed: `v2` served on all six kinds (`PriorityClassAllowlist` included), `v2beta1` the storage version on each, conversion round-trips proven in envtest |
| Q1150 | Retype the five validating webhooks onto `v2`, so the `v2`-typed validators run through this release before `v2.0.0` deletes `api/v2alpha1` | `1.9-gate` | ✅ landed: all five rules name `v2` and the handlers are typed on `api/v2`; the `EgressProxy` alias and the `RunnerSet` Classic protocol are read from their conversion annotations, pinned by envtest writes at `v2alpha1` |
| — | RC validated on dogfood | gates | 🔲 no candidate cut |
| Q1101 | GMC provisions the AGC ServiceMonitor for `v2` ActionsGateways (#1991) | rides | ✅ landed |
| Q1151 | AGC reclaims the scale-set worker whose runner held the job (#2011) | rides | ✅ landed |
| Q1146 | Worker image bumps `actions/runner` to 2.337.0 (#2005) | rides | ✅ landed |

**Q1085 lands before Q413, and the ordering is the whole of what is left of the margin.** Both rows argued for landing the alias reject in 1.8, so that the stored population would already be clean when `v2` first appeared, against landing it here, where "the guard and the hazard arrive together, which works and has no margin".
`v1.8.0` tagged on 2026-09-14 carrying neither of Q1085's remaining halves, so the no-margin case is the one that shipped.
Inside one release the sequencing recovers what it can, which is less than a tag's worth.
The reject and the pre-upgrade check are in the tree before `v2` is served, so `main`, the dogfood cluster and the review order all meet the guard before the hazard.
An operator does not: both halves arrive under the same tag, so for them the clean-population *window* an earlier tag would have given is gone rather than narrowed.

**Three items ride, none of them gating.** `scripts/release/semver-floor.sh v1.8.0` read `FLOOR: MINOR` on 2026-09-30 over the 73 commits since the tag, with five touching a released artifact: the two gates above, and Q1101, Q1151 and Q1146, which merged on their own merits and ship in the tag whether or not the release waits for them.
On 2026-09-17 it read `FLOOR: NONE` over 26 commits, so the riders accumulated during the cycle rather than being scoped into it.
The gates still set the scope, the way 1.8's did: the tag waits for them and not for any rider, unlike 1.6, which nine merged features forced whatever its theme did ([release-ladder.md](release-ladder.md#why-16-exists-rather-than-folding-into-15)).
The riders belong in the release notes, and the ledger takes a row for each further item that merges onto the released surface before the tag.

## Definition of Done

1. **Q413's Phase 2 has landed** with `v2` served on all five kinds and `v2beta1` still the storage version, verified by reading `storage: true` off each shipped CustomResourceDefinition rather than off the markers.
2. **The conversion round-trips both directions** across `v2beta1` ↔ `v2` on real objects, not only in unit tests.
   This is the reading Phase 1's soak could not take, and the reason the rung is worth its cycle.
3. **Q1085's items 2 and 3 have landed**, and landed before Q413.
   1.8 shipped without them, so this is the release that carries them, and after this tag they stop being preventive: `v2` is served, so an unrepresentable object can be requested.
4. **Q1150 has landed**: every `actions-gateway.com` validating webhook rule names `v2` and its handler is typed on `api/v2`.
   This is what gives the `v2`-typed validators a release of real traffic before `v2.0.0` leaves them the only ones, which is the soak Q1150 was pulled forward for.
5. **The published tag serves both versions**, which is the Rule #4b evidence `v2.0.0` depends on.
   Record it here, since `v2.0.0`'s own pre-flight cannot re-derive that a *previous* release served both.

## Pre-flight verdicts

Each verdict names the commit it was measured at, because a verdict covers that commit and nothing later ([release.md](../operations/release.md#1-pre-flight)).
All were taken on 2026-10-02 at `aff5f2268`, the `v1.9.0-rc.1` target.

| Check | Verdict |
|---|---|
| Gating rows | **PASS.** No `1.9-gate` row remains in the store. The empty result was trusted only after the same pattern matched Q1150 as it stood before it closed (`583bbc8f8^`), so it can still match the label. |
| `main` green | **PASS with three path-skipped lanes.** Ten required gates, none not-green. `e2e-calico`, `plan-hygiene` and `status-lint` path-skipped on the target. `ad3106a55`, the Go toolchain bump, ran `e2e-calico` and `status-lint` in full, and `check-artifact-unchanged.sh --lane` exits 0 for each over the one file changed since. `plan-hygiene` last ran in full on `583bbc8f8`, and nothing under `docs/plan/` has changed since. |
| Semver floor | **MINOR**, over 81 commits, set by six touching the released surface: four `feat`s (Q413, Q1150, Q1085, Q1101) and two patches (Q1151, Q1146). The Go 1.27.1 toolchain and Kubernetes 0.37.1 client bumps rebuild every binary but carry the `build` type, so the floor does not count them. |
| API surface | **PASS, ship as-is.** `api-surface-since.sh` lists every `v2` field as added, because `v2` is a new package. Diffing `api/v2` against `api/v2beta1` by hand leaves one difference, the deliberate one: `egressPolicyMode` drops `CiliumFQDN` and `CalicoFQDN` and the `destinationFQDNs` rule requires `FQDN` alone, both per Q452, and the storage marker is withheld per this plan. No existing version lost a field, enum value, default or marker; only comments changed in `api/v2alpha1` and `api/v2beta1`. No new condition reasons, Event reasons, metrics, CLI flags or chart values; metric names are the same 74 set as at `v1.8.0`. |
| `make check` | **PASS**, exit 0, on a branch cut at the target. |

**Reviewed by hand, because the checker missed it:** Q413 publishes a new annotation key, `conversion.actions-gateway.com/egress-policy-mode`, which carries a stored alias into the `v2` view.
`api-surface-since.sh` reported no new annotation keys, and the key is absent from `api/` at `v1.8.0`.
It is written and read by the conversion webhook, and admission rejects a write that sets it to introduce an alias; the notes name it under **API and metric surface**.

**The notes are drafted** in [docs/releases/v1.9.0.md](../releases/v1.9.0.md), interrogated against the tree through `verify-claims`.
That pass corrected four claims in the first draft: the annotation above had been called pre-existing, the Kubernetes bump's starting version was missing, the Go security reading was stated as checked when it was inherited from the bump's commit, and every deprecation notice was credited to `v1.8.0` when the `v1alpha1` and `v2alpha1` ones date from earlier.
The operator-caveat pass ran into the draft: `operator-caveats-since.sh v1.8.0` reports four new `upgrade.md` sections and one new `troubleshooting.md` section, all carried as a `WARNING` (the unpinned read and the alias), a `NOTE` (the webhook rename) and **Upgrading** entries.
The landmine question added the custom-`workerImage` entry: the runner bump changes only the default, and `RunnerVersionTooOld` cannot see GitHub's 30-day window.
`Everything since v1.8.0` reads 81, which `check-release-notes.sh` can verify only once `v1.9.0` is a tag; re-derive it at the cut if anything merges first.
**Validation** reads *pending* until this candidate's run reports.

**Deferred to the stable tag, deliberately.** The marketing reconciliation, the roadmap and `features.md` reconciliation, the announce-bar highlight, and the three prose passes (`readability`, `deslop`, `semantic-remediation`) all bind when the text publishes, and a prerelease deploys no docs and generates rather than curates its Release body.

## What waits for `v2.0.0`

The storage advance and migration ([Q1086](../queue/Q1086.md)) and the four removals ([Q273](../queue/Q273.md), [Q264](../queue/Q264.md), and `v2beta1` itself).
The validating webhook rules cannot silently un-match through those removals: `make webhook-versions-check` (Q1068) fails any rule naming only versions no CRD serves.
[v2-ga.md](v2-ga.md#phase-3--the-storage-advance-and-the-coupled-removals) Phase 3 owns the ordering.
