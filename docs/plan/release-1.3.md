# Release 1.3 Milestone Definition

The scope and Definition of Done for the `v1.3.0` tag. Queue rows that block this
tag carry the `1.3-gate` label in [docs/STATUS.md](../STATUS.md); this file is what
that label points at, per the "scope the release in a plan doc first, then add the
label" rule in
[maintaining-backlog.md](../development/maintaining-backlog.md#dont-pre-assign-release-versions-to-backlog-items).

Cutting mechanics (pre-flight, tagging, verification, the dogfood release-candidate
gate) live in [operations/release.md](../operations/release.md) and are not repeated
here.

## What 1.3 means

Two things, one of which only a release can deliver.

**The headline is worker right-sizing.** Per-`RunnerSet` usage observability,
recommendations surfaced in `RunnerSet.status`, and opt-in auto-apply sizing
profiles, with the supporting managed-VPA and bring-your-own proxy autoscaler work
alongside it. This is the first capability in the project with no Actions Runner
Controller (ARC) equivalent, so it is the release's positioning story, not just a
changelog entry. Plan:
[runner-sizing-profiles.md](runner-sizing-profiles.md).

**1.3 is the deprecation notice for `v2.0.0`.** The project's stated policy is that
API removals happen "on a named release announced at least one release ahead"
([roadmap.md](../roadmap.md), [v1alpha1-deprecation.md](../operations/v1alpha1-deprecation.md)).
Three removals are coupled and all land at `v2.0.0`:

| Removed at `v2.0.0` | Currently | Why it is coupled |
|---|---|---|
| `v1alpha1` (`actions-gateway.github.com`) | deprecated, served | already on the removal track |
| `v2alpha1` (`actions-gateway.com`) | served, **not yet deprecated** | superseded by `v2beta1` as storage version |
| classic acquisition machinery | served | `v2beta1` is ScaleSet-only, so classic exists *only* to serve the two alpha versions |

The coupling is the load-bearing fact: because `v2beta1` is already ScaleSet-only,
classic acquisition has no consumer other than `v1alpha1` and `v2alpha1` objects.
Removing those versions removes classic's entire reason to exist, so splitting them
across releases would buy nothing and cost operators a second breaking migration.
1.3 announces all three; `v2.0.0` executes all three.

`v2.0.0` itself is gated on the `v2` (General Availability) API being available and
validated. That work is planned separately in [v2-ga.md](v2-ga.md) and is
explicitly **not** part of 1.3.

## Definition of Done

All gating items closed, `make check` green, and the mandatory dogfood
release-candidate validation from
[release.md](../operations/release.md) passing on the latest RC.

### A. Headline feature complete (*satisfied*)

No open gating row: Q359 closed 2026-07-25.

> **The headline feature is fully live-validated, and the dogfood RC gate is
> satisfied on completion rate.** The second dogfood session (2026-07-25) ran the
> ScaleSet-migrated tenant to `sampleCount: 36` and confirmed both previously
> unexercised paths: all three `SizingDrift` states (`SizingWithinRange`, and
> `SizingDriftDetected` for both waste and OOM risk) and `Binpack` actuating at
> Guaranteed QoS with derived `requests == limits`. Detail:
> [runner-sizing-profiles.md](runner-sizing-profiles.md#both-20-sample-paths-confirmed-2026-07-25-second-session).
>
> **Completion rate, measured in the same session.** Before the migration, Classic
> orphaned 81% of the jobs it acquired (85 acquired, 16 worker pods). After it, the
> first 28 GAG jobs ran **28/28 green with zero orphans**. A further 14 jobs ran while the
> tenant was misconfigured mid-session (`maxWorkers` raised past the namespace
> `ResourceQuota`, an operator mistake made during the soak), of which 6 were
> non-green. That window is excluded from the rate and recorded separately in the
> plan doc rather than folded in, in either direction. Queued jobs also survived a 16-minute AGC outage intact
> instead of being burned, which Classic could not have done.
>
> **What the gate still needs at tag time** is a *release-candidate* run per
> [release.md](../operations/release.md) on the actual RC image. This session ran
> `e0acd60`, a pre-release build, so it establishes the tenant and the feature are
> sound; it does not stand in for validating the tagged artifact.

### B. Deprecation notice — *gating*

| Item | Why it gates |
|---|---|
| [Q411](../STATUS.md#Q411) | Deprecate `v2alpha1` in the API itself: `+kubebuilder:deprecatedversion` markers and regenerated Custom Resource Definitions (CRDs). Without the marker the apiserver warns nobody, so the notice reaches only readers of the docs. |

> **Q412 is closed (2026-07-26): `v2.0.0` is named.**
> [v1alpha1-deprecation.md](../operations/v1alpha1-deprecation.md) is now the
> standing notice for all three removals rather than for `v1alpha1` alone: it leads
> with a what-`v2.0.0`-removes table (each row with its replacement and the move),
> states the coupling, and ends with a pre-upgrade checklist. The name is repeated
> wherever an operator forms a plan from the docs (README, roadmap, getting-started,
> install, upgrade, tenant-onboarding, migration-v1-to-v2, migration-from-arc,
> troubleshooting, why-gag) and in the design half (Appendix H, 03-api-contracts).
> Two stale statements were corrected in passing: "you can stay on `v1alpha1`
> indefinitely" (upgrade.md) and "Classic is slated for removal one *minor* release
> out" (tenant-onboarding, troubleshooting), which understated a major-tag removal.
> The `CiliumFQDN`/`CalicoFQDN` enum values still say "a future release (on the
> classic/`v1alpha1` deprecation clock)"; naming a release for them is a separate
> decision, filed as [Q428](../STATUS.md#Q428).

The docs half of the notice already shipped as **Q409**: the ARC migration guide,
getting-started, tenant onboarding, install, and the positioning pages were all
re-routed onto `v2beta1`, leaving `v2alpha1` described only as the `gag-migrate`
on-ramp. That settles which version new tenants onboard on, which was the open
question this release's deprecation decision needed answered.

### C. Release mechanics — *gating*

| Item | Why it gates |
|---|---|
| [Q393](../STATUS.md#Q393) | The docs-site announce bar still reads "v1.2.0 is here". `publish.yml`'s `announce-bar` job fails any stable tag whose banner does not name it, before any image is pushed. A miss costs a re-cut tag. |

### D. Gate integrity — *gating*

Cheap to fix, and undermines the "`main` is green" precondition that
[release.md](../operations/release.md) pre-flight assumes: a gate that never ran
leaves `main` green on evidence it never gathered.

| Item | Why it gates |
|---|---|
| [Q404](../STATUS.md#Q404) | `make check` compiles no build-tagged file, so a tagged-test build break reaches CI rather than the local gate. |

Q400 closed 2026-07-26: `api/**` and `scaleset/**` were added to the
integration, security-scan, and e2e filters, and `api/config/**` to
manifest-validate — a fourth instance of the same gap, found while fixing the
first three, where the workflow validates the five v2 CRDs by name but never
gated on the directory holding them. The residual risk that motivated the gate
is unchanged and not retroactively addressed: the scaleset/api-only changes that
merged since `v1.2.0` were never seen by those tiers, and this fix only stops
new ones from slipping through. The recurrence guard — linting the filters
against `go.work` rather than maintaining them by hand — is
[Q429](../STATUS.md#Q429), deliberately left out of the gate because it is new
tooling rather than a correctness fix.

## Explicitly out of scope

| Deferred | Was | Why out of 1.3 |
|---|---|---|
| Capacity gate `SchedulerVerdict` / `AutoscalerVerdict` modes | [Q405](../STATUS.md#Q405), [Q406](../STATUS.md#Q406) | Only the quota pre-claim rung shipped. Both modes are M-sized and unstarted. Ship the quota rung and describe it as exactly that rather than implying the full ladder. |
| `v1alpha1` + `v2alpha1` + classic **removal** | [Q273](../STATUS.md#Q273), [Q264](../STATUS.md#Q264) | 1.3 is the *notice*. Executing the removal in the same release it is announced would violate the one-release-ahead policy. These land at `v2.0.0`. |
| `v2` GA API version | [v2-ga.md](v2-ga.md) | Gated on a beta soak that has not started. Deliberately slow: GA signs a permanent backward-compatibility contract. |

## Critical path & ordering

1. **[Q404](../STATUS.md#Q404)** touches CI configuration only and is independent of
   everything below it, so it can run in parallel with any of them. (Q400, its
   former partner here, closed 2026-07-26.)
2. **[Q411](../STATUS.md#Q411)** is independent of all of the above and can land at
   any point before the tag. It changes generated CRDs, so it should not race a
   session editing the same API packages. It cannot be dropped on the grounds that
   Q409 aligned the docs and Q412 named the release: the deprecation still has to
   reach the apiserver, or an operator who never reads the docs gets no warning at
   all. (Q412, the other half, is done.)
3. **[Q393](../STATUS.md#Q393) last**, immediately before tagging, so the banner names
   the version actually being cut.

## Guardrails

- Removing a served API group is a breaking change. That is why all three removals
  are pinned to a **major** tag rather than a minor, and why 1.3 must ship the notice
  rather than quietly reserving the right to remove later.
- The deprecation of `v2alpha1` does **not** shorten its served life: it stays served
  until `v2.0.0`, exactly like `v1alpha1`. Deprecation marks intent and emits an
  apiserver warning; it removes nothing.
- Nothing in 1.3 requires a tenant to re-apply anything. The `v2beta1` conversion
  webhook already round-trips every served version.
