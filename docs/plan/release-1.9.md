# Release 1.9 Milestone Definition

> **Status: scoped 2026-09-17.** The rung exists because Rule #4b requires it rather than because a defect asked for it, and both conditions the shape was held behind are now discharged: 1.8's soak readings came back positive on 2026-09-14, and `v1.8.0` tagged the same day.
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
[v2-ga.md](v2-ga.md#phase-2--the-graduation-hop) Phase 2's step 1 carries a `+kubebuilder:storageversion` marker that belongs to Phase 3 and `v2.0.0`; moving it is part of [Q413](../queue/Q413.md), not a follow-up.

The hub *may* move to `v2` here: `convertViaHub` routes spoke to hub to spoke, and nothing ties the hub to the storage version.

## Scope ledger

Two gating rows and the candidate validation, in the order they land.

| Q-ID | Item | Gates? | Status |
|---|---|---|---|
| [Q1085](../queue/Q1085.md) | Admission rejects new `CiliumFQDN`/`CalicoFQDN` writes, and the pre-upgrade alias check joins the checklist | `1.9-gate` | 🔲 ready, and first: it is the guard for the hazard Q413 introduces |
| [Q413](../queue/Q413.md) | [v2-ga.md](v2-ga.md#phase-2--the-graduation-hop) Phase 2: add `v2` to all five kinds, serve it beside `v2beta1`, extend conversion coverage. Storage marker withheld | `1.9-gate` | 🔲 ready: the soak read clean 2026-09-14 |
| — | RC validated on dogfood | gates | 🔲 no candidate cut |

**Q1085 lands before Q413, and the ordering is the whole of what is left of the margin.** Both rows argued for landing the alias reject in 1.8, so that the stored population would already be clean when `v2` first appeared, against landing it here, where "the guard and the hazard arrive together, which works and has no margin".
`v1.8.0` tagged on 2026-09-14 carrying neither of Q1085's remaining halves, so the no-margin case is the one that shipped.
Inside one release the sequencing recovers what it can, which is less than a tag's worth.
The reject and the pre-upgrade check are in the tree before `v2` is served, so `main`, the dogfood cluster and the review order all meet the guard before the hazard.
An operator does not: both halves arrive under the same tag, so for them the clean-population *window* an earlier tag would have given is gone rather than narrowed.

**Nothing rides.** `scripts/release/semver-floor.sh v1.8.0` read `FLOOR: NONE` on 2026-09-17 over the 26 commits since the tag: six carry a `feat` or `fix` type and none touches a released artifact, so nothing user-visible has accumulated and a tag today would publish no change.
1.9 is therefore forced entirely by its own content, the way 1.8 was and unlike 1.6, which nine merged features forced whatever its theme did ([release-ladder.md](release-ladder.md#why-16-exists-rather-than-folding-into-15)).
That is a reading rather than a decision: whether anything *should* ride is scope the maintainer sets, and the ledger takes a row for each item that does.

## Definition of Done

1. **[Q413](../queue/Q413.md)'s Phase 2 has landed** with `v2` served on all five kinds and `v2beta1` still the storage version, verified by reading `storage: true` off each shipped CustomResourceDefinition rather than off the markers.
2. **The conversion round-trips both directions** across `v2beta1` ↔ `v2` on real objects, not only in unit tests.
   This is the reading Phase 1's soak could not take, and the reason the rung is worth its cycle.
3. **[Q1085](../queue/Q1085.md)'s items 2 and 3 have landed**, and landed before Q413.
   1.8 shipped without them, so this is the release that carries them, and after this tag they stop being preventive: `v2` is served, so an unrepresentable object can be requested.
4. **The published tag serves both versions**, which is the Rule #4b evidence `v2.0.0` depends on.
   Record it here, since `v2.0.0`'s own pre-flight cannot re-derive that a *previous* release served both.

## What waits for `v2.0.0`

The storage advance and migration ([Q1086](../queue/Q1086.md)), the four removals ([Q273](../queue/Q273.md), [Q264](../queue/Q264.md), and `v2beta1` itself), and the validating webhook rules that un-match when `v2alpha1` goes ([Q1068](../queue/Q1068.md)).
[v2-ga.md](v2-ga.md#phase-3--the-storage-advance-and-the-coupled-removals) Phase 3 owns the ordering.
