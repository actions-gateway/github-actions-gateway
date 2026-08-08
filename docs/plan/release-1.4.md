# Release 1.4 Milestone Definition

> **Status: scope decided 2026-08-05. One gating Queue row remains**, labelled
> `1.4-gate`: [Q554](../STATUS.md#Q554). Q166 and Q691 both shipped 2026-08-08.
> Everything else the release contains is already merged.

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

**Q166: v2 API M4, cross-namespace EgressProxy sharing. — shipped.**
The one item here whose absence was a liability rather than a deferral.
`sharing.allowedNamespaces` was **served in the v2beta1 API with no enforcement**,
so an operator could set a field that nothing honoured. That is a shipped defect
wearing a feature label, and every release that shipped it that way hardened a
dormant contract further. Demand fired 2026-08-01.

Delivered whole: the consent check, CA distribution, and dual-side NetworkPolicy.
Two things the plan had not accounted for turned up in the code and are recorded in
[§H.9](../design/appendix-h-v2-api-decomposition.md#h9-cross-namespace-proxy-sharing)
— a cross-namespace reference was not expressible at all (so M4 had to build the
path, not just guard it), and the AGC cannot read a remote `EgressProxy` without an
RBAC widening nobody wanted, so the GMC mediates. Absent or empty `sharing` denies,
which keeps the pre-M4 posture as the default and the unset case.

**[Q554](../STATUS.md#Q554): a curated runner template library.** The cheapest
real capability on the list: no new CRD, and it promotes templates CI already
validates (dogfood kata-dind and privileged-dind) into a shipped kustomize base
the e2e overlays patch, plus a plain baseline entry. Packaging rather than new
behaviour, and the constraint that only CI-exercised templates ship is what keeps
it that way.

**Q691: auto re-run a force-cancelled abandoned run.**
Closes a gap this cycle opened. Q683's cancelled ending accepts
`rerun-failed-jobs`, measured, so operators re-ran by hand. **Shipped
2026-08-08:** the run is re-run when a worker pod binds for the owner again, and
the loop a re-run into a starved pool would otherwise cause is bounded by the
existing per-run retry budget, with exhaustion on
`eviction_retries_exhausted_total{cause="abandoned"}` and expiry on
`abandoned_run_rerun_waits_total{outcome="expired"}`.

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

## Also in scope: the feature list and the marketing surfaces

Added 2026-08-06, after a competitive review found both halves of the marketing
rotting at once. This is docs-only, touches no shipped artifact, and does not
need a gate row; it does need to happen before the tag.

**The inaccurate claims land here, not in 1.5.** They are wrong now, and every
day they stay up is a day a prospect can check one and stop trusting the page:

- The executive summary promised OOM-killed and node-lost jobs re-run
  automatically, in three places. The provisioner recovers eviction, preemption,
  and external graceful deletion; an OOM-killed container is explicitly excluded
  as "the job failed on its own merits". **Corrected 2026-08-06.**
- The ~10 minute recovery figure was the worst case quoted as the case. A
  preemption or drain concludes in a measured 15-26s. **Corrected 2026-08-06.**
- GitHub's queue timeout is 24 hours; "up to 48 h" came from a GHES page that
  has since been rewritten. **Corrected 2026-08-06.**
- Two ARC-side comparison rows went false at datable upstream releases: 0.13.1
  made quota-blocked pod creation self-healing, and 0.14.0 added multi-label
  scale sets, which GAG does not have. **Open.**
- `why-gag.md` states ARC ships no bundled dashboard; it ships a per-scale-set
  Grafana sample. The defensible claim is that nothing aggregates across scale
  sets or per tenant. **Open.**

**`docs/features.md` is the inventory and needs a sweep.** It was created
2026-08-01 and is close to complete, but 1.4 adds six user-facing features ahead
of the three gating rows, and a feature that never reaches the inventory never
reaches the curated surfaces either.

**One under-claim is worth pulling up now** rather than waiting for 1.5's larger
pass: no-PEM workload identity gets a single line in `features.md` and a
nine-word aside inside a YAML footnote, while the weaker claim ("App keys never
in env") occupies a security pillar. ARC reads the App private key from a
Secret, so "the key never enters the cluster" is a row that writes itself.

The rest of the reconciliation, including whether the comparison table keeps its
verdict-table shape, is [1.5 scope](release-1.5.md#in-scope-reconcile-the-marketing-surfaces).
The recurring form is [release.md § Pre-flight](../operations/release.md#1-pre-flight).

## The discipline this cycle could not apply

"Do not let a feature force a minor when the accumulated patches could ship
first" is the right rule, and it binds at the **start** of a cycle. This one was
already past that point when the question was asked. It applies again at 1.4.1
versus 1.5.0, which is exactly when nobody will be thinking about it, since 120
commits accumulated here without a decision. That is the argument for the
semver-floor instrument rather than for a documented rule.
