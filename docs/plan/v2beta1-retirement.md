# Should `v2beta1` retire at `v2.0.0`?

> **Status: decided 2026-09-08, yes, Path 2.** `v2beta1` is no longer served past `v2.0.0`.
> The reasoning given: `v2.0.0` is a major release and a major release may make breaking changes.
> The analysis below is kept as taken, before the decision, so the cost that was accepted stays legible.

**What the decision settles.** [Q452](v2-ga.md#decided-v2-omits-ciliumfqdncalicofqdn) flips to *`v2` omits the aliases*, GA is born clean, and the aliases stop being on their own clock: they ride `v2.0.0` with `v1alpha1`, `v2alpha1` and classic.
The published `v3.0.0` floor is walked back to `v2.0.0` across the operator docs, the enum godoc and the admission warning.

**What it does not settle, and what it therefore obliges.** Rule #4b is unaffected by semver: storage cannot advance to `v2` until a release has shipped serving both `v2beta1` and `v2`, so the overlap release below is still required and the deprecation notice still has to precede it.
That package is [Q1085](../queue/Q1085.md): the notice, the admission change from warn to reject, and the pre-upgrade alias check.
It is now on the critical path rather than optional.
The check is load-bearing for a mechanical reason given in [v2-ga.md](v2-ga.md#decided-v2-omits-ciliumfqdncalicofqdn): one stored object naming an alias fails the whole conversion request that carries it, so it breaks `kubectl get egressproxies` at `v2` for the cluster rather than for itself.

---

Q452 asked whether GA `v2` defines the deprecated `CiliumFQDN`/`CalicoFQDN` aliases.
It was first decided yes, on the reasoning that `v2beta1` keeps serving them until `v3.0.0` and every served version must be able to represent every stored object.
That reasoning is sound and rests on a premise nobody argued: **that `v2.0.0` keeps serving `v2beta1`.** If `v2beta1` retires at `v2.0.0` instead, the aliases go with it, `v2` never defines them, and Q452's answer flips.

## The premise is asserted in three places and contradicted in two

| Says `v2beta1` survives `v2.0.0` | Says the hop drops the superseded version |
|---|---|
| [v1alpha1-deprecation.md](../operations/v1alpha1-deprecation.md) — operator-facing, published | [v2-api.md](v2-api.md) graduation ladder, step 3 |
| [security-operations.md](../operations/security-operations.md) — operator-facing, published | [v2-ga.md](v2-ga.md) Phase 2, step 3 |
| [api-review.md](../development/api-review.md) | |

Both say "drop the superseded served version".
For the `v2beta1` → `v2` hop the superseded served version *is* `v2beta1`.
The two operator-facing pages are already live on the site, which is the asymmetry that matters: one side is a published promise, the other internal plan text.

The likely resolution is that step 3 is generic hop text, correct for `v2alpha1` → `v2beta1` where alpha carries no promise, and wrong for beta → GA.
That is an inference, not a reading, and it is what this document asks someone to settle.

## What upstream actually requires

Measured 2026-09-07 against the [Kubernetes deprecation policy](https://kubernetes.io/docs/reference/using-api/deprecation-policy/):

- **Beta versions are on a clock from birth**, not indefinite: deprecated no more than 9 months or 3 minor releases after introduction, and no longer served 9 months or 3 minor releases after deprecation, whichever is longer in each case.
- **Removing a beta needs no major version.** Rule #3 says GA versions can replace beta ones.
  The `v3.0.0` floor is this project's own rule that breaking changes land on a major tag, not an upstream requirement.
- **Rule #4b forces an overlap release.** The storage version "may not advance until after a release has been made that supports both the new version and the previous version".

`v2beta1` was introduced in `v1.1.0` (2026-07-12), so its upstream deprecation deadline is 2027-04-12 and, if deprecated today, its earliest upstream removal is 2027-06-07.

## Rule #4b applies to both paths, which is what makes them close

Neither path can add `v2` as storage and retire `v2beta1` in one release.
Both need the same three-release shape, so the overlap release, the storage migration and the `v2` addition are **common cost**, not a cost of retiring.

The paths differ only in what happens at the third release and what `v2` carries.

## The two paths

### Path 1 — `v2beta1` survives `v2.0.0` (the current plan, and Q452 as decided)

`v2.0.0` adds `v2` beside `v2beta1` and removes `v1alpha1`, `v2alpha1` and classic.
`v2` defines both aliases, deprecated, because a stored object naming one must be representable in `v2`.

- Keeps the published `v3.0.0` floor exactly as operators can read it today.
- The aliases enter a frozen GA surface, so they outlive `v3.0.0` and are removable only by retiring `v2` itself.
- Q1076, filed on #1867 and not yet merged, is the only lever that bounds this, and its window closes at the tag.

### Path 2 — `v2beta1` retires at `v2.0.0`

`v2` omits the aliases; `v2beta1` is deprecated now and removed at `v2.0.0` with the other three removals.

| Release | Ships | Operator action |
|---|---|---|
| A (1.8) | `v2beta1` deprecated and the removal named for `v2.0.0`, in the operator docs, the enum godoc and the admission warning. | None. Migrate at your convenience: set `egressPolicyMode: FQDN` and have the platform operator set `--fqdn-policy-backend`. |
| B (1.9) | The Rule #4b overlap: `v2` served beside `v2beta1`, `v2beta1` still storage, `v2` without the aliases. Admission **rejects** new alias use, where A only warns, and the alias check joins the [Pre-Upgrade Validation Checklist](../operations/upgrade.md#pre-upgrade-validation-checklist). | Run the check. It flags any `EgressProxy` still naming an alias, which must be migrated before `v2` is requested for it. |
| C (`v2.0.0`) | Storage advances to `v2`, stored objects migrated, then `v2beta1`, `v2alpha1`, `v1alpha1` and classic removed together. | The `v1`→`v2` migration already planned. |

- GA is clean permanently, and Q1076 becomes moot.
- Costs a walkback of the published `v3.0.0` floor, in the direction that disfavours an operator: sooner than promised.
- **Release B's alias check is load-bearing, and it ships in the same release as the failure mode it guards.** B is where `v2` starts being served, so B is the first release where a conversion that cannot represent the alias can be asked for.
  `ConversionRequest.Objects` is a list, so one such object breaks `kubectl get egressproxies` at `v2` for the cluster.
  Landing the check and the reject in A instead is strictly safer, and is worth doing if 1.8 has room; what makes B the deadline rather than the target is that after B they are no longer preventive.

## What the paths actually differ on

| | Path 1 | Path 2 |
|---|---|---|
| Aliases in GA `v2` | Yes, deprecated, effectively permanent | No |
| Published `v3.0.0` floor | Kept | Walked back to `v2.0.0` |
| Marginal work over the common cost | None | Deprecation notice, admission rejection, alias preflight |
| Q1076 (on #1867) | Live, window closes at the tag | Moot |
| Risk to an operator using an alias | None | Breaks at Release B if they ignore Release A |

## Work both paths need regardless

- **The storage migration is unbuilt.** `storedVersions` and `StorageVersionMigration` appear nowhere outside `vendor/`, and Phase 2 and Phase 3 both assume it.
- **The Rule #4b overlap release**, per above.

## Recommendation

> Accepted 2026-09-08 on a different and simpler argument than the one below: a major release is allowed to break.
> The cost asymmetry stands as a second reason rather than the deciding one.

**Path 2**, weakly, and the reasoning is the cost asymmetry rather than tidiness.
Path 1's cost is permanent and unbounded; Path 2's costs are one-time and bounded, and most of what looks expensive about it is common cost the graduation owes anyway.
The marginal work is a notice, an admission change, and a preflight.

The measured facts that support it: no chart, overlay or e2e manifest in the tree sets an alias, and [Q245](q245-fqdn-intent-backend-split.md#migration--compatibility) recorded the only known consumers as tests and docs, so the migration is very likely a no-op on every real cluster.
The floor being walked back is 5 weeks old, announced with `v1.3.0` on 2026-08-03.

**Deciding sooner strictly widens the options.** The notice starts a clock, does not commit us to removing anything, and every release it is delayed pushes the earliest clean-GA date out.

## Open premises this decision rests on

- **Which side of the doc conflict governs** is the thing being decided, not a fact to look up.
- **Whether any external adopter uses an alias** is unknowable for a public project.
  "Known consumers are tests and docs" is a floor, not a rate.
- **What notice period this project actually owes** is unsettled: the stated policy is "at least one release ahead", which at the measured cadence of `v1.1.0` → `v1.7.0` (6 minors in 49 days) is about 8 days of wall-clock.
  Q1083 proposed pairing that release count with a wall-clock floor, on upstream's "9 months or 3 minor releases, whichever is longer" shape.
  Declined 2026-09-09: the project takes no wall-clock commitment, so the policy stays release-count only.
  Either way it is independent of this decision.
