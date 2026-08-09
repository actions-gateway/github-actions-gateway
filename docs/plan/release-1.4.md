# Release 1.4 Milestone Definition

> **Status: scope decided 2026-08-05. No gating Queue rows remain.** All three
> `1.4-gate` items shipped 2026-08-08: Q691, Q554 (its
> [plan](archive/runner-template-library.md) archived), and Q166. Everything else
> the release contains is already merged. `v1.4.0-rc.1` was cut from `162d97a7`
> on 2026-08-09 and its dogfood validation **PASSED on the first attempt**.
> Work landed after that commit (the Q766 ScaleSet port and the docs sweep), so
> rc.1 is no longer the candidate: **an rc.2 follows once those merge**, and it
> is the one that has to validate before the stable tag.

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

**Q554: a curated runner template library. Shipped 2026-08-08.** The cheapest
real capability on the list: no new CRD, and it promotes templates CI already
validates (dogfood kata-dind and privileged-dind) into a shipped kustomize base
the e2e overlays patch, plus a plain baseline entry. Packaging rather than new
behaviour, and the constraint that only CI-exercised templates ship is what keeps
it that way. That constraint is now a gate rather than a convention:
`make template-library-check` reconciles the shipped and exercised sets both
ways, and every entry is admitted by a real apiserver on each integration run.
The operator surface is [runner-template-library.md](../operations/runner-template-library.md);
the implementation findings are in the [archived plan](archive/runner-template-library.md).

**Q691: auto re-run a force-cancelled abandoned run.**
Closes a gap this cycle opened. Q683's cancelled ending accepts
`rerun-failed-jobs`, measured, so operators re-ran by hand. **Shipped
2026-08-08:** the run is re-run when a worker pod binds for the owner again, and
the loop a re-run into a starved pool would otherwise cause is bounded by the
existing per-run retry budget, with exhaustion on
`eviction_retries_exhausted_total{cause="abandoned"}` and expiry on
`abandoned_run_rerun_waits_total{outcome="expired"}`.

## Deferred out of 1.4, and why

> **Corrected 2026-08-09.** This section was headed "Deferred to 1.5.0", which
> promised a release none of these rows was ever labelled for. The proxy cluster
> in particular is demand-gated, as the paragraph below says in its own words, so
> it is parked rather than scheduled. The ladder is
> [release-ladder.md](release-ladder.md); the reshape is
> [Q772](../STATUS.md#Q772).

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
  scale sets, which GAG does not have. **Corrected 2026-08-08.**
- `why-gag.md` states ARC ships no bundled dashboard; it ships a per-scale-set
  Grafana sample. The defensible claim is that nothing aggregates across scale
  sets or per tenant. **Corrected 2026-08-08.**

**`docs/features.md` is the inventory and needs a sweep.** It was created
2026-08-01 and is close to complete, but 1.4 adds six user-facing features ahead
of the three gating rows, and a feature that never reaches the inventory never
reaches the curated surfaces either.

**One under-claim is worth pulling up now** rather than waiting for 1.5's larger
pass: no-PEM workload identity gets a single line in `features.md` and a
nine-word aside inside a YAML footnote, while the weaker claim ("App keys never
in env") occupies a security pillar. ARC reads the App private key from a
Secret, so "the key never enters the cluster" is a row that writes itself.
**Shipped 2026-08-09:** `why-gag.md` gains a credential row in the ARC table,
and the security pillar carries the stronger claim under the weaker one.

The row is narrower than the paragraph above assumed, and the difference is the
whole point of measuring before writing a comparative claim. ARC's *default*
does read the key from a Secret, and copies it into the listener config Secret
the controller generates, so the PEM is at rest twice. But 0.14.2 also ships an
opt-in Azure Key Vault path in the released chart, and there nothing holds the
key in etcd: the listener fetches the value itself at runtime, against an Azure
client certificate read from a path in its pod. So the defensible line is that
under `workloadIdentity` no App key exists in the cluster in any form, not that
ARC cannot keep one out of etcd. Measured 2026-08-09 against the 0.14.2 chart
(`values.yaml`, `templates/autoscalingrunnerset.yaml`, the `AutoscalingRunnerSet`
types) and `master` (`vault/vault.go`, `vault/azurekeyvault/`,
`appconfig.FromSecret`, `ResourceBuilder.newScaleSetListenerConfig`).

The rest of the reconciliation, including whether the comparison table keeps its
verdict-table shape, is [1.5 scope](release-1.5.md#in-scope-reconcile-the-marketing-surfaces).
The recurring form is [release.md § Pre-flight](../operations/release.md#1-pre-flight).

## Sweep verdict: the docs and marketing surfaces, 2026-08-09

The pre-flight sweep ran against `v1.3.0..main` (181 commits). Every mechanical
gate was already green, including `doc-links`, `roadmap-check`, the nav-coverage
gate and `release-pins-check`, and pre-flight question 3 is satisfied: after
#1329 and #1362 every ARC claim on `why-gag.md` and `alternatives.md` carries a
version and a measurement date. Three content gaps remained, all created this
cycle, and all are now closed.

**The auto-re-run docs did not agree with each other about Q683 and Q691, and
none of them carried the tier scope.** `04-operational-flows.md` records the
scope correctly ("Classic tier only, matching the force-cancel it recovers");
no operator-facing or marketing surface repeated it. Measured from source:
`forceCancelAbandonedRun` has one caller, inside the classic `provision()`
handler, and the ScaleSet tier's `disruptionAwaitingRecovery` has three arms
(eviction, preemption, deletion) with no abandoned case, deliberately. So on the
ScaleSet tier, which is the v2 default every new tenant runs, a worker reaped
while Pending still concludes at GitHub's ~15-minute unstarted-job timeout and
needs a manual re-run. `features.md` had been advertising the capability to those
tenants unscoped, and it also said the run "accepts a re-run afterwards", which
describes the pre-Q691 manual state. The consolidated matrix in
`troubleshooting.md` said the opposite, excluding a never-started worker "by
design" with no note that the classic tier now recovers it. All three are
corrected, and the matrix preamble names `abandoned` as the fifth, out-of-table
cause. This was a propagation failure rather than a code gap: the design doc had
the answer the whole time.

**Q166 and Q554 had reached `features.md` and stopped there.** Neither appeared
on `index.md`, `README.md` or `why-gag.md`, which is exactly the rot pre-flight
question 1 exists to catch. Both are now on all three. They were deliberately
*not* added to the ARC comparison table: a new row there needs a measured ARC
column, and this sweep measured no new ARC behaviour, so adding one would have
created precisely the undated claim the 2026-08-06 review was called to remove.

**The announce bar still carried 1.3's highlight.** `highlight_for` named
`v1.3.0`, so a `v1.4.0` build degraded to the plain release-notes link (verified
by building at `GAG_DOCS_RELEASE=v1.4.0`). It now names `v1.4.0` and leads with
cross-namespace proxy sharing, the runner template library, and the v2
worker-capacity gauges. The abandoned-run re-run was considered and rejected for
the bar: it is classic-tier only, and a banner is the wrong place for a scope
qualifier.

### The tier caveat that no longer applies, and why it is recorded anyway

The sweep found Q683 and Q691 wired into the classic path only, with the scope
recorded in [04-operational-flows.md](../design/04-operational-flows.md) and on
no operator surface, so a `v2beta1` tenant reading this release would have
expected a one-second cancel and an automatic re-run their tier did not perform.
That was drafted as a caveat for the curated notes.

**It is moot: Q766 ported both to the ScaleSet tier inside this release**, so 1.4
ships them on both tiers and there is no caveat to carry. The `upgrade.md`
migration note says so directly.

Worth keeping the trail. The gap existed for a few days and no operator surface
named it, which is the failure the pre-flight sweep exists to catch, and it was
caught by reading the code rather than the plan doc. The `upgrade.md` placement
also turned out to be the load-bearing part: `operator-caveats-since.sh` builds
the curated notes from the `docs/operations/` diff, so a tier scope documented
only in a design doc never reaches them either way.

## Pre-flight: the API surface this tag publishes

Recorded 2026-08-09 from `scripts/release/api-surface-since.sh` over
`v1.3.0..162d97a7`, the commit `v1.4.0-rc.1` was cut from, per
[release.md § Pre-flight](../operations/release.md#1-pre-flight). **Verdict:
ship as-is.** Four wire fields and one condition reason are published for the
first time; no enum constraint and no default changed.

| Addition | Carried on | Why the shape is right |
|---|---|---|
| `allowedInfraPriorityClasses` (Q298) | `PriorityClassAllowlist` | Unset or empty forbids every named class, so the secure posture is the unset case. Name and validation mirror the sibling `allowedPriorityClasses`. |
| `proxyRef` (Q166) | `RunnerSet` | Optional because direct egress is a defined behaviour rather than a failure, which is what separates it from the required `templateRef` (§H.4, §H.10). |
| `defaultProxyRef` (Q166) | `ActionsGateway` | Inherited only by RunnerSets that set no `proxyRef`, so the narrower field always wins. |
| `namespace` (Q166) | `ProxyObjectRef` | Empty means the referrer's own namespace, so the pre-M4 same-namespace posture is the default and the unset case. |
| `ProxyShareNotGranted` | condition reason | Names the fail-closed outcome when a cross-namespace reference lacks provider consent. |

**The one thing that looked like a gap is house style, checked rather than
assumed.** `ProxyObjectRef` bounds both its fields by length and neither by
pattern, while the allowlist fields next to it pattern every item. No object ref
in `v2beta1` patterns a name: `ProxyObjectRef.Name` carries the same
`MinLength=1`/`MaxLength=52` pair as `ObjectRef.Name`, and `LocalSecretReference`
and `LocalConfigMapReference` are length-only too. Adding a pattern narrows a
published field, so the cheap window for it closes at the stable tag and this is
the pass that had to settle it. An unresolvable namespace string is rejected at
resolution rather than at admission, and fails closed.

## The rc.1 validation verdict, 2026-08-09

`validate-release.sh v1.4.0-rc.1` **PASSED**, exit 0, in 25m05s end to end, and
the cluster was confirmed back at 0 nodes by polling `gcloud` rather than by
reading the gate's own teardown line. These are the receipts the notes'
Validation section draws on.

| Leg | Result |
|---|---|
| deploy | RC deployed and CI routed to GAG in 2m43s |
| e2e matrix | **74/74 specs, 62 ok, 0 failed, 12 skipped** ([run 31330763470](https://github.com/actions-gateway/github-actions-gateway/actions/runs/31330763470)) |
| sizing, `NodeShare` | `sizingProfileState=Active`; worker CPU request derived to **1500m** where the templates ask 2 and 3 |
| sizing, `Throughput` | `Active`, `sampleCounts=[166]` |
| CRD smoke | blob signature `Verified OK`; all five v2 CRDs server-side applied and registered |

Artifact verification cleared separately and before the gate: 7/7 OCI signatures
(five images, both charts), the `verify-blob` signature on the v2 CRD manifest,
and a build-provenance attestation whose `buildSignerURI` ends
`publish.yml@refs/tags/v1.4.0-rc.1` with a `sourceRepositoryDigest` equal to the
tagged commit. Each green check was re-run against a deliberately wrong identity
(a `refs/heads/` regexp, and `unit-test.yml` as signer) and each failed, so the
passes discriminate rather than merely exiting 0.

### No candidate had cleared this gate on the first attempt before

Worth stating in the notes because it is checkable and because it is the return
on this cycle's release-tooling work, not a lucky run. The 1.3 line needed four
candidates to produce any verdict at all: rc.1 aborted when the gate's then
repo-wide e2e routing caught concurrent sessions' CI, rc.2 returned Q550 and
Q551 instead of a result, rc.3 aborted at `start.sh`'s AGC wait, and rc.4 was
"the first verdict any RC in this line has produced"
([release-1.3.md](release-1.3.md)).

**Scope the claim to what the record supports.** `validate-release.sh` landed
2026-07-12 (Q294, #619), the day `v1.1.0` was tagged, so `v1.0.0` and `v1.1.0`
predate it entirely. `v1.2.0` had a single RC and the gate did exist by then,
but no plan doc records a validation run for it, and no record is not the same
as no run. So the defensible sentence is that 1.3 is the only prior line with a
recorded validation history and rc.1 here is the first candidate to pass first
time, not that this has never happened.

The three failure modes that cost the 1.3 line its early candidates were each
fixed since: run-scoped dispatch replaced repo-wide routing (repo-wide is now an
explicit `E2E_ROUTE_VAR=1` opt-in), the gate settles the e2e lane before it
scales a node, and Q629, Q640 and Q630 bounded the run watch, reclaimed orphaned
leases, and reconciled sentinel silence against run status. Q630 earned itself
on this run: the one stall event it raised came only after the run was no longer
live, which is the reconciliation behaving as designed rather than a false
positive on a quiet leg.

### `Throughput` actuated, which the runbook does not expect

[release.md](../operations/release.md#validate-the-release-candidate-on-dogfood)
records `Throughput` as reported-but-never-fatal because it needs at least 20
samples per template container and the gate's own ~7-job matrix cannot supply
them, making `NOT VALIDATED THIS RUN` the documented normal outcome. It came back
`Active` with 166 samples. Nothing anomalous happened: the sampler tracks every
worker pod regardless of `spec.sizing` and the aggregate re-seeds from the
persisted `status.sizingRecommendation`, so the dogfood cluster's ordinary CI
traffic since 1.3.0 had already earned the history. The consequence for the notes
is a stronger claim than a passing gate alone, that this RC ran its own CI on
derived sizing. The runbook's expectation is now stale for this cluster and is
worth correcting separately.

## The discipline this cycle could not apply

"Do not let a feature force a minor when the accumulated patches could ship
first" is the right rule, and it binds at the **start** of a cycle. This one was
already past that point when the question was asked. It applies again at 1.4.1
versus 1.5.0, which is exactly when nobody will be thinking about it, since 120
commits accumulated here without a decision. That is the argument for the
semver-floor instrument rather than for a documented rule.
