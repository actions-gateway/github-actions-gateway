# Release 1.3 Milestone Definition

> **Status: one gating row is OPEN — Q551.** Two were filed 2026-07-31 from the
> `v1.3.0-rc.2` validation window, where the RC gate's dispatched e2e job was
> wedged by the AGC itself: provisioning leaked runner registrations at GitHub
> (reap never deregistered them, and names derive from the job ID, so retries
> 409 against their own leftovers — Q550), and after four attempts the listener
> dropped the job permanently with no retry, condition, or Event (Q551). Both
> are availability bugs in the scale-set listener an ordinary tenant can hit —
> any burst of provisioning failures (quota, stockout, admission) starts the
> same cycle — so they gate the tag rather than ride the backlog.
>
> **Q550 shipped 2026-07-31**
> ([plan](archive/q550-runner-registration-leak.md)): the worker pod now carries
> the name its runner is registered under, the reaper deregisters that record
> before deleting the pod, and a listener start sweeps records no live pod
> claims. That removes most of what *causes* the collisions but does not change
> what the listener does once they persist, so **Q551 still gates the tag**. The
> paragraph below records the pre-2026-07-31 history.
>
> **Previously: no gating Queue row remained.** The original six closed 2026-07-26
> (Q359, Q400, Q404, Q411, Q412, Q393), and all four rows the
> [API review](#e-api-review-satisfied) opened closed 2026-07-28:
> Q485 with the `windowStartTime` rename shipped, Q484 with a CEL rule requiring
> `nodeShare.allocatable` to declare cpu, memory, or both, and Q481 (**ship
> `spec.sizing` as-is, deliberately**) and Q486 (**the two managed-autoscaler
> opt-ins keep their different shapes, deliberately**) with no API change at all.
>
> **The last gates to clear were all API-shape questions**, which is the pattern
> worth noticing: once the capability work landed, this release's residual risk
> was not anything unfinished but surface about to be frozen — cheap to fix until
> the tag, a conversion shim or a version bump afterwards.
>
> **The tag is still not cut.** The Definition of Done also
> requires the release-candidate dogfood validation in
> [operations/release.md](../operations/release.md), which can only run against
> the actual RC image and is deliberately not tracked as a Queue row. Residuals
> deferred out of 1.3 are under
> [Explicitly out of scope](#explicitly-out-of-scope).
>
> **`v1.3.0-rc.1` is published and verified; its dogfood validation is still
> owed.** The RC was tagged 2026-07-31 off `2d85b4c6`; `publish.yml` ran green
> and every artifact verification passed (`make verify-release`, the v2 CRD
> blob signature, an SBOM attestation spot-check, SLSA provenance, both
> arches). The first validation attempt the same day **aborted without a
> verdict**: the gate's then-repo-wide e2e routing window caught concurrent
> sessions' CI (two PRs and a merge landed mid-window), the teardown deleted
> the e2e AGC under a caught job, and the stranded queued runs wedged `main`'s
> e2e concurrency group until they were cancelled. The gate now routes via a
> run-scoped `workflow_dispatch` input and `e2e-stop.sh` drains before deleting
> the AGC; re-run `validate-release.sh v1.3.0-rc.1` for the verdict. The
> orphaned-worker-pod product defect the incident exposed is Queue-tracked
> (GMC cascade reap).

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
> The `CiliumFQDN`/`CalicoFQDN` enum values were left saying "a future release (on the
> classic/`v1alpha1` deprecation clock)"; naming a release for them was filed as
> Q428 and is now **settled: `v3.0.0` at the earliest**, not
> `v2.0.0`. They are enum members of the beta version `v2beta1`, which `v2.0.0` keeps
> serving, and an API element is removable only by incrementing the version — so they
> outlive this release's bundle by a major tag. Stated for operators in
> [v1alpha1-deprecation.md](../operations/v1alpha1-deprecation.md#a-fourth-deprecation-on-a-different-clock-ciliumfqdn--calicofqdn).

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
itself to the tagged trees, which needed its own triage pass and landed
separately as Q430 (closed 2026-07-27) — `run.build-tags` now covers the same
102 files, and the 21 findings estimated here turned out to be 100 once
golangci-lint's default `max-same-issues: 3` cap was lifted. Detail:
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

### E. API review (*satisfied*)

1.3 is the first release to run the
[pre-release API review](../development/api-review.md), and it is also the
release that motivated it: Q476 renamed `capacityGate.mode: On` to `Observe`
days before this tag would have published it, caught by an unrelated
conversation rather than by any step.

**Reviewed:** the surface `scripts/api-surface-since.sh v1.2.0` reports — the
`RunnerSet` additions (`spec.sizing`, `spec.capacityGate`,
`spec.maxWorkerLifetime`, `status.sizingRecommendation`,
`status.sizingProfileState`), the `ActionsGateway` additions
(`spec.agcAutoscaling`, `spec.clusterCapacity`), `EgressProxy`'s
`spec.managedAutoscaling`, thirteen new condition types/reasons, and the
`actions-gateway.com/migrated-from-namespace` label. None of these has appeared
in a tagged release, so all of them are in the cheap window until this tag.

**Found and fixed before the tag:** `capacityGate.mode: On` → `Observe` (Q476) —
the value named *that* the gate was on rather than *how* it decides, which stops
distinguishing anything once Q407's reserved `Probe`/`Provision` join the same
axis. And `status.sizingRecommendation[].windowStart` → `windowStartTime`
(Q485, closed 2026-07-28) — upstream spells timestamp fields `somethingTime`
(`startTime`, `lastTransitionTime`), and this is the API's only project-defined
`metav1.Time`, so it sets the precedent every later one is read against. The
rename had to land in **both** v2alpha1 and v2beta1: the spoke↔hub conversion is
a JSON round-trip (`api/v2alpha1/conversion.go`), so a tag renamed on one side
only would have silently dropped the field on conversion rather than failing to
compile.

**Found and shipped as-is, deliberately:** Q481 — `sizing.profile` carries two
axes (where the request comes from; what limits follow) the same way
`capacityGate.mode` did before Q470, leaving a Guaranteed node share and
history-derived-requests-under-hand-set-limits without a profile of their own.
Gating because
the tag freezes the shape either way. **Closed 2026-07-28 without an API
change**, on three grounds: the cost that made Q470 worth a break is absent
here — both axes are the set owner's own choice, so nothing asks a tenant to
assert a fact they do not own; the axes are not orthogonal, so a split shape
still needs a `only meaningful when …` CEL rule for the headroom percent (which
is defined off the *observed peak*, and a peak exists only under the usage
source); and both cells are reachable **additively** in any later minor, which is
the difference that matters — Q470 had to beat its tag because its fix removed
enum values, and this one does not. The review also found the Guaranteed node
share is reachable *today*, as a side effect of the limit-lift guard rather than
by design — verified, pinned by
`TestApplySizingProfileNodeShareLiftedLimitsReachGuaranteed`, and written up for
operators in
[worker-rightsizing.md](../operations/worker-rightsizing.md#getting-guaranteed-qos-out-of-nodeshare).
Full rationale, and the rule for
extending the enum (`profile` is an intent enum: new values name a distinct
operator intent, mechanism recombinations go in a sibling field):
[appendix-h §H.7](../design/appendix-h-v2-api-decomposition.md#h7-reference-integrity--runtime-conditions-not-admission).

> The premise the row was filed under — "the sizing model is unvalidated" —
> **expired before the close, and in the direction that made shipping as-is
> easier, not harder.** `Binpack` was live-validated 2026-07-25 and `Throughput`
> on the dogfood `ci` tenant days later (Q449), which turns the derivation the
> split would have had to redefine from an unproven rule into a measured one.
> `NodeShare` is still envtest-only (Q448) — and it is the profile whose missing
> cell was the complaint, so the case for reshaping around it is weaker still.

**Also shipped as-is, deliberately:** Q486 — 1.3 publishes two managed-autoscaler
opt-ins with **different shapes**:
`EgressProxy.spec.managedAutoscaling` is a `*bool` defaulting `true` (an
*opt-out* of a managed HorizontalPodAutoscaler, Q173), while
`ActionsGateway.spec.agcAutoscaling` is a block whose *presence* is the opt-in
into a managed VerticalPodAutoscaler and which carries its own `mode` enum
(Q360). An operator meeting both in one changelog can fairly ask why "managed
autoscaling" is spelled two ways. **Closed 2026-07-28 without an API change**, on
three grounds, each of which decides its own side independently:

1. **The direction is not a style choice — it follows what already ships.** The
   proxy pool's HPA was managed *before* 1.3; Q173 adds only the ability to stop
   managing it, so the field must default to today's behaviour and can only be an
   opt-out. The AGC VerticalPodAutoscaler is new, with no behaviour to preserve,
   so it defaults off. Reversing either is the actual defect: an opt-in
   `managedAutoscaling` deletes the HPA of every pool that upgrades, and a
   default-on `agcAutoscaling` hands the single AGC pod's requests to an
   autoscaler nobody asked for. Making them symmetric would mean breaking one of
   those two, so this difference survives any redesign.
2. **The container follows whether the opt-in carries knobs.**
   `managedAutoscaling` is a pure ownership toggle — the HPA's knobs
   (`minReplicas`, `maxReplicas`, `targetCPUUtilizationPercentage`) already exist
   as siblings and predate it, so a block would have to either move them (a wire
   break) or sit next to them holding nothing. `agcAutoscaling` carries `mode`,
   which is meaningful only when opted in; block presence gives that knob a home
   *and* is the switch, so there is no `enabled: true` + `mode:` pair to keep
   consistent — which is exactly the "sibling fields gated by one value" tell
   under [one field answers one question](../development/api-review.md#one-field-answers-one-question).
3. **Consistency is owed to the field's neighbours, not to the other CRD.**
   `managedAutoscaling` sits beside `managedNetworkPolicy` on the same
   `EgressProxySpec` — same `*bool`, same `+kubebuilder:default=true`, same "the
   GMC owns this object unless you say otherwise" meaning — and
   `managedNetworkPolicy` shipped in `v1.1.0`, so that pattern is already
   published and already learned. Reshaping `managedAutoscaling` to match a field
   on a different CRD would make it the odd one out in the object an operator
   actually reads it in.

> **The `*bool` was checked against [prefer a string enum](../development/api-review.md#prefer-a-string-enum-to-a-bool) rather than grandfathered.**
> It passes because the axis it names is genuinely two-valued: it answers "does
> the GMC own the `<name>-proxy` HPA?", and *who else* owns scaling is
> deliberately not our question — an external KEDA, VPA, or custom HPA targets
> the stable Deployment name without telling us. If we ever manage a second
> autoscaler flavour, that is a new sibling naming the mechanism, not a third
> value here — additively, in any later minor.

The accept is a real freeze: `managedAutoscaling` could not become a block later
without a wire break. What is bought for that is a field that matches its own
object and preserves upgrade behaviour; what is paid is one cross-CRD asymmetry,
mitigated by both shapes being documented where operators meet them
([tenant-onboarding.md](../operations/tenant-onboarding.md#letting-an-autoscaler-size-the-agc-agcautoscaling))
and by the rule now generalised in
[api-review.md § Let the opt-in's direction follow what already ships](../development/api-review.md#let-the-opt-ins-direction-follow-what-already-ships).

**Found by the second pass, and now closed too:** the three further gating rows
that second pass filed were Q485 and Q486 above, and one last one:

- **Q484 — fixed 2026-07-28.** A `nodeShare.allocatable`
  carrying neither cpu nor memory was admitted, and `sizingProfileState` then
  reported `Active` while nothing was derived. Fixed with the CEL rule the row
  scoped — `'cpu' in self || 'memory' in self`, on the `allocatable` field
  itself rather than on `sizing`, so ratcheting suppresses it on every write
  that does not touch the envelope. Declaring one of the two stays valid: the
  other resource keeps the template's ask. Rationale in
  [appendix-h §H.7](../design/appendix-h-v2-api-decomposition.md#h7-reference-integrity--runtime-conditions-not-admission);
  the rejection has an
  [operator runbook](../operations/troubleshooting.md#runnerset-rejected-nodeshareallocatable-declares-neither-cpu-nor-memory).
  This was a **validation tightening**, which is wire-breaking after a tag — the
  reason the row was 1.3-gating rather than ordinary backlog.

> **Q481 and Q484 both concerned `NodeShare`, and did not overlap.** Q481 asked
> whether the *shape* is right and answered yes; Q484 was a missing validation on
> a field that shape already has. Closing Q481 as ship-as-is neither fixed nor
> blocked it — worth stating because "the sizing shape was reviewed and accepted"
> is easy to misread as covering both.

**Accepted without change:** the bare-`string` enum fields
(`capacityGate.mode`, `sizing.profile`, `clusterCapacity.nodeAutoscaling`,
`status.sizingProfileState`) versus the named types used by `VPAUpdateMode` and
`EgressPolicyMode`. Wire-identical either way, so it is a Go-API break for `api`
module consumers only and does not need to beat this tag.

## Explicitly out of scope

| Deferred | Was | Why out of 1.3 |
|---|---|---|
| Capacity gate `AutoscalerVerdict` mode | Q406 | The quota pre-claim rung and `SchedulerVerdict` (Q405) shipped; `AutoscalerVerdict` was M-sized and unstarted at the cut. Describe what shipped as exactly that rather than implying the full ladder. (It shipped after 1.3, on 2026-07-27.) |
| `v1alpha1` + `v2alpha1` + classic **removal** | [Q273](../STATUS.md#Q273), [Q264](../STATUS.md#Q264) | 1.3 is the *notice*. Executing the removal in the same release it is announced would violate the one-release-ahead policy. These land at `v2.0.0`. |
| `v2` GA API version | [v2-ga.md](v2-ga.md) | Gated on a beta soak that has not started. Deliberately slow: GA signs a permanent backward-compatibility contract. |

## Critical path & ordering

**Nothing is left to order.** Six gating items closed
2026-07-26: both gate-integrity items (Q400, Q404), both halves of the
deprecation notice (Q411, Q412), and the announce bar (Q393); the API review's
Q481 closed 2026-07-28 without an API change
([§ E](#e-api-review-satisfied)), as did Q486, and neither
needed ordering because a no-op decision has no dependents; Q485 closed the same
day with the `windowStartTime` rename shipped, and Q484 with a CEL rule on
`nodeShare.allocatable`. Each of the four was a self-contained change to one
field or none, independent of everything else here. Neither half
Q412 named `v2.0.0` where operators plan from the docs, and Q411 put the same
release into the apiserver warning, so an operator who never reads the docs still
gets told.

The announce bar used to sit at the end of this list, to be landed "last,
immediately before tagging" so it named the version being cut. Q393 made its
version derive from the tag list at build time, so it no longer needs a place in
the ordering at all.

One thing that remains is not a Queue item at all: the release-candidate dogfood
validation in [§ A](#a-headline-feature-complete-satisfied), which can only be run
against the actual RC image. It is last regardless — it validates the RC the
other rows have already shaped.

## Guardrails

- Removing a served API group is a breaking change. That is why all three removals
  are pinned to a **major** tag rather than a minor, and why 1.3 must ship the notice
  rather than quietly reserving the right to remove later.
- The deprecation of `v2alpha1` does **not** shorten its served life: it stays served
  until `v2.0.0`, exactly like `v1alpha1`. Deprecation marks intent and emits an
  apiserver warning; it removes nothing.
- Nothing in 1.3 requires a tenant to re-apply anything. The `v2beta1` conversion
  webhook already round-trips every served version.
