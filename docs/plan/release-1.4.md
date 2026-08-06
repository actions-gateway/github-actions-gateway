# Release 1.4 Milestone Definition

> **Status: scope decided 2026-08-05. Three gating Queue rows remain**, labelled
> `1.4-gate`: [Q166](../STATUS.md#Q166), [Q554](../STATUS.md#Q554),
> [Q691](../STATUS.md#Q691). Everything else the release contains is already
> merged.

## The minor was forced before anyone chose it

`v1.3.0..main` carries 120 commits, 17 of them `feat`. Six change the shipped
artifact, so a patch release stopped being available the day the first of them
landed:

| Change | Why it is user-facing |
|---|---|
| v1alpha1 apiserver deprecation warnings (Q633) | CRD schema, ships in the chart |
| v2 RunnerSet worker-capacity gauges (Q319) | New metrics, an operator contract |
| `WorkerCapacityDeclined` gauge with its reason label (Q643) | Same |
| Infra PriorityClass allowlist from the watched CR (Q298) | New admission and config behaviour |
| Force-cancel an abandoned job's run (Q683) | Runtime behaviour operators observe |
| Scale-set conclusion guards persisted across a hard kill (Q606) | Durability behaviour change |

The other eleven `feat` commits are `agent`, `ci`, `scripts`, `backlog`, `docs`,
`metrics` and `probe`: development tooling and CI that no released binary or
chart contains. **The classification is by scope, not by conventional-commit
type**, which is why a raw `feat` count answers the wrong question.

Nothing tracked this. There is no `CHANGELOG.md`, no unreleased-changes file, and
the `1.0-gate`/`1.3-gate`/`2.0-gate` labels were carried by **zero** rows when
this scope was decided. The floor was discoverable only by hand-classifying 120
commit subjects, which is why it was discovered at scoping time rather than when
it moved. A semver-floor instrument is being built to close that; until it lands,
the floor is a manual reading.

## What 1.4.0 adds beyond what is merged

**[Q166](../STATUS.md#Q166): v2 API M4, cross-namespace EgressProxy sharing.**
The one item here whose absence is a liability rather than a deferral.
`sharing.allowedNamespaces` is **served in the v2beta1 API with no enforcement**,
so an operator can set a field that nothing honours. That is a shipped defect
wearing a feature label, and every release that ships it that way hardens a
dormant contract further. Demand fired 2026-08-01. Remaining: the M4 consent
check, CA distribution, dual-side NetworkPolicy.

**[Q554](../STATUS.md#Q554): a curated runner template library.** The cheapest
real capability on the list: no new CRD, and it promotes templates CI already
validates (dogfood kata-dind and privileged-dind) into a shipped kustomize base
the e2e overlays patch, plus a plain baseline entry. Packaging rather than new
behaviour, and the constraint that only CI-exercised templates ship is what keeps
it that way.

**[Q691](../STATUS.md#Q691): auto re-run a force-cancelled abandoned run.**
Closes a gap this cycle opened. Q683's cancelled ending accepts
`rerun-failed-jobs`, measured, so operators re-run by hand today. Needs a
capacity-returned trigger and a loop budget, because a re-run re-queues into the
pool that was starved in the first place.

## Deferred to 1.5.0, and why

**The proxy hardening cluster stays together**:
[Q564](../STATUS.md#Q564) audit logging, [Q565](../STATUS.md#Q565) per-tenant rate
limiting, [Q566](../STATUS.md#Q566) TLS on the in-cluster hop, and
[Q567](../STATUS.md#Q567) per-group dedicated pools. Four related items from
[appendix G](../design/appendix-g-future-enhancements.md), the deliberately
non-committal shelf, and **none has demand recorded against it**. Q566 is a real
gap (the CONNECT target host:port is cleartext on the in-cluster hop) and Q567 is
L and wants a plan doc before code. Splitting them across two releases spends
their coherence for nothing; together they are a release theme.

**[Q555](../STATUS.md#Q555), opt-in flaky-job retry,** has an unbuilt
prerequisite. Detection needs a real job outcome, which only the unread exit code
carries.

## Why the scope stops here

1.4.0 already holds six user-facing features before any of the three above. It is
not a thin release, the cut needs a release candidate and a dogfood validation,
and each further item moves that out. The three admitted are the ones where
waiting costs something concrete: a served-unenforced API field, templates CI
already proves, and a manual step this cycle introduced.

## The discipline this cycle could not apply

"Do not let a feature force a minor when the accumulated patches could ship
first" is the right rule, and it binds at the **start** of a cycle. This one was
already past that point when the question was asked. It applies again at 1.4.1
versus 1.5.0, which is exactly when nobody will be thinking about it, since 120
commits accumulated here without a decision. That is the argument for the
semver-floor instrument rather than for a documented rule.
