# Release 1.3 Milestone Definition

> **Status: every gating Queue row is closed (2026-07-26).** Q359, Q400, Q404,
> Q411, Q412, and Q393 all landed, so no `1.3-gate` row remains in
> [docs/STATUS.md](../STATUS.md). **The tag is not cut**: the Definition of Done
> also requires the release-candidate dogfood validation in
> [operations/release.md](../operations/release.md), which can only run against
> the actual RC image and is not tracked as a Queue row. Residuals deliberately
> deferred out of 1.3 are listed under
> [Explicitly out of scope](#explicitly-out-of-scope).

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
| `v2alpha1` (`actions-gateway.com`) | deprecated, served | superseded by `v2beta1` as storage version |
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

### B. Deprecation notice (*satisfied*)

No open gating row: Q411 and Q412 both closed 2026-07-26. The notice now exists in
both halves the policy needs, the API surface and the docs.

> **Q411 is closed (2026-07-26): the deprecation reaches the apiserver.** All five
> `actions-gateway.com` kinds carry `+kubebuilder:deprecatedversion` on `v2alpha1`, so
> the regenerated CRDs (and their chart copies) set `deprecated: true` plus a
> `deprecationWarning` naming `v2beta1` as the replacement and `v2.0.0` as the removal
> release. Verified against a real apiserver, not just the generated YAML: on a kind
> cluster carrying `api/config/crd`, a `v2alpha1` read *and* write each emit
> `Warning: actions-gateway.com/v2alpha1 RunnerTemplate is deprecated; use
> actions-gateway.com/v2beta1. v2alpha1 is served until v2.0.0, which removes it.`,
> the write still succeeds, and the same object read at `v2beta1` emits nothing. The
> warning names `v2.0.0` itself, so the API surface and the docs state one removal
> release rather than two half-answers. `check-v2-api-sync.sh` now normalises the
> marker as an entitled per-version difference, alongside `+kubebuilder:storageversion`.

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

### C. Release mechanics (*satisfied*)

No open gating row: Q393 closed 2026-07-26.

> The docs-site announce bar's version is now derived from the git tags at build
> time rather than hand-edited per release, so `v1.3.0` names itself with no
> pre-flight step and cannot ship the stale banner every prior stable tag did.
> `publish.yml`'s `announce-bar` job still gates the release, but now by building
> the site at the tag and asserting the *rendered* banner names it. Details:
> [website.md § The announce bar](../development/website.md#the-announce-bar).

### D. Gate integrity (*satisfied*)

No open gating row: Q400 and Q404 both closed 2026-07-26.

Both mattered for the same reason, which is why they were scoped together: a gate
that never ran leaves `main` green on evidence it never gathered, and that
undermines the "`main` is green" precondition that
[release.md](../operations/release.md) pre-flight assumes.

Q404 closed 2026-07-26: `make check` compiled no build-tagged file, so a compile
break in an `integration`/`e2e`/`load` package reached only CI's path-gated heavy
tiers, which may not even run on the PR that introduced it. `make
build-tags-check` now vets the workspace with every first-party tag enabled, in
both the local gate and CI's `lint` job, and a coverage assertion fails the gate
if a *new* build tag appears that its list does not cover, so the hole cannot
reopen in a new shape. Deliberately out of the fix: widening `golangci-lint`
itself to the tagged trees, a one-line change that surfaces 21 pre-existing
findings needing individual triage, filed as [Q430](../STATUS.md#Q430). Detail:
[testing.md § The build-tag gate](../development/testing.md#the-build-tag-gate).

Q400 closed 2026-07-26: `api/**` and `scaleset/**` were added to the
integration, security-scan, and e2e filters, and `api/config/**` to
manifest-validate — a fourth instance of the same gap, found while fixing the
first three, where the workflow validates the five v2 CRDs by name but never
gated on the directory holding them. The residual risk that motivated the gate
is unchanged and not retroactively addressed: the scaleset/api-only changes that
merged since `v1.2.0` were never seen by those tiers, and this fix only stops
new ones from slipping through. The recurrence guard — linting the filters
against `go.work` rather than maintaining them by hand — was Q429, deliberately
left out of the gate because it is new tooling rather than a correctness fix.

Q429 closed 2026-07-26 anyway, inside the release: `make path-filters-check`
(`scripts/check-path-filters.sh`, also CI's `path-filters` job) now fails when a
`go.work` module is missing from a filter whose jobs exercise the whole
workspace, when a `filters:` block declares a filter the gate has not been told
to treat as workspace-covering or narrow-by-design, or when a pattern names a
path that no longer exists. It reproduces the Q400 gap end-to-end: dropping
`scaleset/**` from `integration-test.yml` fails the gate naming the module and
the pattern to add. What it deliberately does NOT decide is whether a narrow
filter should have been widened — that judgement is still the reviewer's, and
the Q400 residual risk above is unaffected. Detail:
[testing.md § The path-filter gate](../development/testing.md#the-path-filter-gate).

## Explicitly out of scope

| Deferred | Was | Why out of 1.3 |
|---|---|---|
| Capacity gate `SchedulerVerdict` / `AutoscalerVerdict` modes | [Q405](../STATUS.md#Q405), [Q406](../STATUS.md#Q406) | Only the quota pre-claim rung shipped. Both modes are M-sized and unstarted. Ship the quota rung and describe it as exactly that rather than implying the full ladder. |
| `v1alpha1` + `v2alpha1` + classic **removal** | [Q273](../STATUS.md#Q273), [Q264](../STATUS.md#Q264) | 1.3 is the *notice*. Executing the removal in the same release it is announced would violate the one-release-ahead policy. These land at `v2.0.0`. |
| `v2` GA API version | [v2-ga.md](v2-ga.md) | Gated on a beta soak that has not started. Deliberately slow: GA signs a permanent backward-compatibility contract. |

## Critical path & ordering

**Nothing is left to order.** Every gating item closed 2026-07-26: both
gate-integrity items (Q400, Q404), both halves of the deprecation notice (Q411,
Q412), and the announce bar (Q393). Neither half of the notice could stand alone:
Q412 named `v2.0.0` where operators plan from the docs, and Q411 put the same
release into the apiserver warning, so an operator who never reads the docs still
gets told.

The announce bar used to sit at the end of this list, to be landed "last,
immediately before tagging" so it named the version being cut. Q393 made its
version derive from the tag list at build time, so it no longer needs a place in
the ordering at all.

What remains before the tag is not a Queue item: the release-candidate dogfood
validation in [§ A](#a-headline-feature-complete-satisfied), which can only be run
against the actual RC image.

## Guardrails

- Removing a served API group is a breaking change. That is why all three removals
  are pinned to a **major** tag rather than a minor, and why 1.3 must ship the notice
  rather than quietly reserving the right to remove later.
- The deprecation of `v2alpha1` does **not** shorten its served life: it stays served
  until `v2.0.0`, exactly like `v1alpha1`. Deprecation marks intent and emits an
  apiserver warning; it removes nothing.
- Nothing in 1.3 requires a tenant to re-apply anything. The `v2beta1` conversion
  webhook already round-trips every served version.
