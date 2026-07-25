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

### A. Headline feature complete — *gating*

| Item | Why it gates |
|---|---|
| [Q399](../STATUS.md#Q399) | Most dispatched dogfood jobs never finalize. This blocks the mandatory dogfood RC gate *and* it capped the sample count that left Q359's remaining paths unexercised. Critical path for the whole release. |
| [Q359](../STATUS.md#Q359) | The `SizingDrift` verdict and `Binpack` actuating are the two right-sizing paths never exercised live. Headlining a feature with untested actuation is the honesty problem [release-1.0.md](release-1.0.md) §E exists to prevent. |

### B. Deprecation notice — *gating*

| Item | Why it gates |
|---|---|
| [Q411](../STATUS.md#Q411) | Deprecate `v2alpha1` in the API itself: `+kubebuilder:deprecatedversion` markers and regenerated Custom Resource Definitions (CRDs). Without the marker the apiserver warns nobody, so the notice reaches only readers of the docs. |
| [Q412](../STATUS.md#Q412) | Name `v2.0.0` as the removal release for all three items in the table above. This is the "one release ahead" the policy promises; miss it in 1.3 and `v2.0.0` cannot legally remove anything under the project's own rules. Also updates the docs that currently describe `v2alpha1` as merely "still served" to say deprecated, removed at `v2.0.0`. |

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

Both are cheap, and both undermine the "`main` is green" precondition that
[release.md](../operations/release.md) pre-flight assumes.

| Item | Why it gates |
|---|---|
| [Q400](../STATUS.md#Q400) | `scaleset/**` and `api/**` are absent from the integration, security-scan, and e2e path gates. Several scaleset/api-only changes merged since `v1.2.0` were never tested by those tiers. |
| [Q404](../STATUS.md#Q404) | `make check` compiles no build-tagged file, so a tagged-test build break reaches CI rather than the local gate. |

## Explicitly out of scope

| Deferred | Was | Why out of 1.3 |
|---|---|---|
| Capacity gate `SchedulerVerdict` / `AutoscalerVerdict` modes | [Q405](../STATUS.md#Q405), [Q406](../STATUS.md#Q406) | Only the quota pre-claim rung shipped. Both modes are M-sized and unstarted. Ship the quota rung and describe it as exactly that rather than implying the full ladder. |
| `v1alpha1` + `v2alpha1` + classic **removal** | [Q273](../STATUS.md#Q273), [Q264](../STATUS.md#Q264) | 1.3 is the *notice*. Executing the removal in the same release it is announced would violate the one-release-ahead policy. These land at `v2.0.0`. |
| `v2` GA API version | [v2-ga.md](v2-ga.md) | Gated on a beta soak that has not started. Deliberately slow: GA signs a permanent backward-compatibility contract. |

## Critical path & ordering

1. **[Q399](../STATUS.md#Q399) first.** It is the only item that blocks two others
   (the dogfood RC gate and Q359). Root cause is unmeasured today, so it is also the
   least predictable. Starting anywhere else risks discovering late that the release
   gate cannot pass.
2. **[Q359](../STATUS.md#Q359)** follows immediately: once Q399 lifts the sample cap,
   both remaining paths are exercisable via template edits with no soak.
3. **[Q400](../STATUS.md#Q400) and [Q404](../STATUS.md#Q404)** can run in parallel
   with the above by a different session. They touch CI configuration only.
4. **[Q411](../STATUS.md#Q411) and [Q412](../STATUS.md#Q412)** are independent of all
   of the above and can land at any point before the tag. Q411 changes generated
   CRDs, so it should not race a session editing the same API packages. Neither can
   be dropped on the grounds that Q409 already aligned the docs: the deprecation has
   to reach the apiserver (Q411) and name its removal release (Q412), or it is a
   statement of taste rather than a notice operators can plan against.
5. **[Q393](../STATUS.md#Q393) last**, immediately before tagging, so the banner names
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
