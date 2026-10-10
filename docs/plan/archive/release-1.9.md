# Release 1.9 Milestone Definition

> **Status: shipped as `v1.9.0` on 2026-10-05, validated on `rc.2` in CI ([gate run 37380198658](https://github.com/actions-gateway/github-actions-gateway/actions/runs/37380198658)); `v1.9.0` was the first stable tag `publish.yml` refused to publish without the gate's own evidence (Q880), and it published (run 37395739048).** `rc.1` validated on 2026-10-03 through the local gate and was held for Q880, Q1158 and Q1160.
> Scoped 2026-09-17; Q1150 added 2026-09-30.
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
[v2-ga.md](../v2-ga.md#phase-1--the-soak-what-well-validated-means)'s Phase 1 soak validates `v2beta1`'s shape and says nothing about a conversion that does not exist yet.

Full reasoning: [release-ladder.md](../release-ladder.md#why-19-exists-the-storage-version-cannot-advance-in-the-same-release-that-introduces-v2).

## What this release must NOT do

**It must not mark `v2` the storage version**, and must not migrate stored objects.
That is the whole point of the rung, and it is the one way to ship 1.9 and still not satisfy Rule #4b.
[v2-ga.md](../v2-ga.md#phase-2--the-graduation-hop) Phase 2's step 1 carries a `+kubebuilder:storageversion` marker that belongs to Phase 3 and `v2.0.0`; moving it was part of Q413, not a follow-up.

The hub stays at `v2beta1`, though nothing ties it to the storage version: moving it buys nothing before `v2.0.0` leaves one served version, and it would put the alias conversion annotation on every `v2alpha1` and `v2beta1` conversion ([why](../v2-ga.md#the-hub-stays-at-v2beta1)).

## Scope ledger

Three gating rows and the candidate validation, in the order they land, then the merged work that rides.

| Q-ID | Item | Gates? | Status |
|---|---|---|---|
| Q1085 | Admission rejects new `CiliumFQDN`/`CalicoFQDN` writes, and the pre-upgrade alias check joins the checklist | `1.9-gate` | ✅ landed first, ahead of Q413: the reject is in the GMC webhook and the check is in the [Pre-Upgrade Validation Checklist](../../operations/upgrade.md#before-upgrading-to-v200-no-egressproxy-still-names-a-deprecated-fqdn-alias) |
| Q413 | [v2-ga.md](../v2-ga.md#phase-2--the-graduation-hop) Phase 2: add `v2` to all five kinds, serve it beside `v2beta1`, extend conversion coverage. Storage marker withheld | `1.9-gate` | ✅ landed: `v2` served on all six kinds (`PriorityClassAllowlist` included), `v2beta1` the storage version on each, conversion round-trips proven in envtest |
| Q1150 | Retype the five validating webhooks onto `v2`, so the `v2`-typed validators run through this release before `v2.0.0` deletes `api/v2alpha1` | `1.9-gate` | ✅ landed: all five rules name `v2` and the handlers are typed on `api/v2`; the `EgressProxy` alias and the `RunnerSet` Classic protocol are read from their conversion annotations, pinned by envtest writes at `v2alpha1` |
| Q880 | Validate release candidates in CI: keyless identity, the gate on a Linux runner, a workflow on each `v*-rc.*` tag, and `publish.yml` reading its verdict | `1.9-gate` | ✅ landed: milestones 1–3 in #2029, milestone 4 in #2043; `rc.2`'s evidence passes `check-validated-candidate.sh v1.9.0` |
| Q1155 | Gate teardown keeps its lease when a stop script refuses, so `--reclaim` finds stranded nodes | gates, with Q880 | ✅ landed (#2026) |
| Q1159 | The gate runs the e2e matrix a second time on the shipped `privileged-dind` template, after the Kata legs | gates | ✅ landed (#2031) |
| Q1158 | The gate's lease is visible across hosts, so a CI run and a local run cannot overlap on the cluster | gates `rc.2` | ✅ landed (#2036) |
| Q1160 | A third e2e leg, Kata with the Dragonfly mirror back end, and the readings that show Dragonfly carried the pulls | gates `rc.2` | ✅ landed (#2037); its manifest fix landed with Q539 (#2042) |
| — | RC validated on dogfood | gates | `rc.1` ✅ through the local gate; `rc.2` ✅ in CI, gate run 37380198658 |
| Q1101 | GMC provisions the AGC ServiceMonitor for `v2` ActionsGateways (#1991) | rides | ✅ landed |
| Q1151 | AGC reclaims the scale-set worker whose runner held the job (#2011) | rides | ✅ landed |
| Q1146 | Worker image bumps `actions/runner` to 2.337.0 (#2005) | rides | ✅ landed |

**Q880 was added on 2026-10-04, after `rc.1` validated.** It touches no released file, so `rc.2` ships the same artifacts `rc.1` passed with; the hold buys where the verdict is recorded, a job result on the tag rather than a ref anyone who can push a tag can also push.
Q1155 gates with it because a CI run is unattended: on a laptop the stranded nodes of a failed teardown were noticed and stopped by hand, and in CI nobody would notice.
The stable-tag passes below stand, except that the notes' **Validation** section and the commit count are re-derived for `rc.2`.

**Q1159 was added the same day, because 1.9 changed a template no gate ran.** #2004 moved `privileged-dind`'s daemon from `docker:27-dind` to `docker:28-dind`, and the gate ran only the Kata variant, so `rc.1` validated without a job ever running on the shipped DinD template.

**`rc.2` waits for Q1158 and Q1160 as well, decided on 2026-10-04.** Q539's Dragonfly back end merged after `rc.1` (#2027) and its operator page publishes with 1.9.0, but no gate run had used it, since nothing set `E2E_MIRROR_BACKEND`; Q1160 adds the leg that does.
Q1158 closes the overlap Q880 opened: a CI gate and a local gate each keep their lease on their own host, so neither sees the other.
Neither changes a released file; both change what the candidate's validation covers.

**Q1085 lands before Q413, and the ordering is the whole of what is left of the margin.** Both rows argued for landing the alias reject in 1.8, so that the stored population would already be clean when `v2` first appeared, against landing it here, where "the guard and the hazard arrive together, which works and has no margin".
`v1.8.0` tagged on 2026-09-14 carrying neither of Q1085's remaining halves, so the no-margin case is the one that shipped.
Inside one release the sequencing recovers what it can, which is less than a tag's worth.
The reject and the pre-upgrade check are in the tree before `v2` is served, so `main`, the dogfood cluster and the review order all meet the guard before the hazard.
An operator does not: both halves arrive under the same tag, so for them the clean-population *window* an earlier tag would have given is gone rather than narrowed.

**Three items ride, none of them gating.** `scripts/release/semver-floor.sh v1.8.0` read `FLOOR: MINOR` on 2026-09-30 over the 73 commits since the tag, with five touching a released artifact: the two gates above, and Q1101, Q1151 and Q1146, which merged on their own merits and ship in the tag whether or not the release waits for them.
On 2026-09-17 it read `FLOOR: NONE` over 26 commits, so the riders accumulated during the cycle rather than being scoped into it.
The gates still set the scope, the way 1.8's did: the tag waits for them and not for any rider, unlike 1.6, which nine merged features forced whatever its theme did ([release-ladder.md](../release-ladder.md#why-16-exists-rather-than-folding-into-15)).
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

Each verdict names the commit it was measured at, because a verdict covers that commit and nothing later ([release.md](../../operations/release.md#1-pre-flight)).
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

**The notes are drafted** in [docs/releases/v1.9.0.md](../../releases/v1.9.0.md), interrogated against the tree through `verify-claims`.
That pass corrected four claims in the first draft: the annotation above had been called pre-existing, the Kubernetes bump's starting version was missing, the Go security reading was stated as checked when it was inherited from the bump's commit, and every deprecation notice was credited to `v1.8.0` when the `v1alpha1` and `v2alpha1` ones date from earlier.
The operator-caveat pass ran into the draft: `operator-caveats-since.sh v1.8.0` reports four new `upgrade.md` sections and one new `troubleshooting.md` section, all carried as a `WARNING` (the unpinned read and the alias), a `NOTE` (the webhook rename) and **Upgrading** entries.
The landmine question added the custom-`workerImage` entry: the runner bump changes only the default, and `RunnerVersionTooOld` cannot see GitHub's 30-day window.
`Everything since v1.8.0` reads 81, which `check-release-notes.sh` can verify only once `v1.9.0` is a tag; re-derive it at the cut if anything merges first.
**Validation** reads *pending* until this candidate's run reports.

**Deferred to the stable tag, deliberately.** The marketing reconciliation, the roadmap and `features.md` reconciliation, the announce-bar highlight, and the three prose passes (`readability`, `deslop`, `semantic-remediation`) all bind when the text publishes, and a prerelease deploys no docs and generates rather than curates its Release body.

## Stable-tag verdicts

Taken on 2026-10-04 on a branch cut at `8b4b49914`; `check-artifact-unchanged.sh v1.9.0-rc.1 origin/main` exits 0 there, so `rc.1`'s verdict still describes what ships.

| Check | Verdict |
|---|---|
| Install pins | **PASS.** 20 pins across five pages bumped to `1.9.0` / `1.9.z`; `make release-pins-check` reads all 20 as `v1.9.0`, prepared. |
| Marketing reconciliation | **PASS after three edits.** The README's version table said `v2beta1` was unaffected by the `v2.0.0` removals, which it is not, and had no `v2` row; `why-gag.md` said the v2 API "has only just reached beta". `v2` now has a `features.md` entry. |
| Competitor claims | **PASS after three edits.** All 26 ARC cells re-read against `gha-runner-scale-set-0.15.0` (`1945b42`, released 2026-10-01) and re-stamped. Three clauses had gone wrong: a per-set listener throttle now exists (it still meters API calls, not worker onset); `AutoscalingRunnerSet` status lost its runner counts to metrics; and `Outdated` now reacts to a runner GitHub rejected as deprecated, parking the set rather than warning beforehand. No verdict flipped. |
| Roadmap and `features.md` | **PASS.** `check-roadmap.sh` passes as is and with `GAG_RELEASE_TAG=v1.9.0`, which expires the two `new in 1.7` chips. |
| Announce bar | **PASS.** `highlight_for` is `v1.9.0`; a `GAG_DOCS_RELEASE=v1.9.0` build renders `v1.9.0 is here.` with the 1.9 highlight. |
| Operator caveats | **PASS.** `operator-caveats-since.sh v1.8.0` reports the same four `upgrade.md` and one `troubleshooting.md` sections as at the candidate, plus the runner-version troubleshooting entry; all are in the notes. |
| Notes | **Validation** names `rc.1` and its three windows. `Everything since v1.8.0` reads 87, the count once this change squash-merges; re-derive it at the tag. The semver floor is unchanged: MINOR, from the same six commits. |

**Re-taken 2026-10-05 at `075502924`, for `rc.2`.** `check-artifact-unchanged.sh v1.9.0-rc.2 origin/main` exits 0 (29 files changed, none released), and `check-gates-green.sh` reads no gate not green: the seven path-skipped lanes ran in full on `aa1178e31`, and `e2e-calico` on `fc4c0d529`, with the released surface unchanged since each.
`make release-pins-check` still reads 20 pins as `v1.9.0`; `check-roadmap.sh` passes with and without `GAG_RELEASE_TAG=v1.9.0`; `highlight_for` still names `v1.9.0`; and `operator-caveats-since.sh v1.8.0` reports the same sections as on 2026-10-04.
The semver floor is unchanged, MINOR from the same six commits.
The marketing reconciliation found one gap, the Dragonfly mirror back end (#2027), which landed after the first pass and is now in the notes; nothing else that landed since ships to an operator.
The notes then took all four prose passes: `verify-claims` re-derived the commit count (107 once the notes PR merges; re-check at the tag) and corrected two claims about what the publish check and the earlier CI runs established, and `readability`, `deslop` and `semantic-remediation` repaired the sentences written for `rc.2`, leaving the prose reviewed at the candidate as it was.

## Candidate validation

### `v1.9.0-rc.2`

**PASSED in CI, validated 2026-10-05; promotion to `v1.9.0` is the maintainer's call.** Tagged at `65ba797c2`, the Q1158 merge, so it carries Q1158 and Q1160 on the same released surface as `rc.1`.
[Gate run 37380198658](https://github.com/actions-gateway/github-actions-gateway/actions/runs/37380198658) ran `main` at `aa1178e31`, after milestone 4 merged, and uploaded `gate-evidence-1` naming `v1.9.0-rc.2`, `65ba797c218d7f1d5422fee476e44a2dc77ed841` and all five legs.
`check-validated-candidate.sh v1.9.0`, run against that live artifact with a local-only `v1.9.0` tag on `aa1178e31`, reports `v1.9.0-rc.2 validated 65ba797c2 and still covers v1.9.0`; with the `dragonfly` line removed from the same evidence it exits 1 naming that leg.

| Leg | Verdict |
|---|---|
| `kata` | **PASS.** e2e run 37384333934 succeeded. `NodeShare` active and deriving 1500m where the templates ask 2 and 3; `Throughput` active on 397 samples. The quota rung bound at zero headroom (`withheldCapacity[quota]=2`, `advertisedCapacity=0`) and released when the quota was restored. |
| `crd-smoke` | **PASS.** The signed `v2` CRD artifact verified against the publish identity. |
| `soak` | **PASS.** Q1059: all five `v2beta1` kinds carried traffic. Q1060: the standing spec is identical at `v2alpha1` and `v2beta1`. Q1156: 8 standing objects and a `v2`-applied `EgressProxy` identical at `v2beta1` and `v2`, with no `finding` this time. |
| `dind` | **PASS.** e2e run 37387182676 succeeded on the shipped `privileged-dind` template (Q1159). |
| `dragonfly` | **PASS.** e2e run 37388708904 succeeded on Kata with the Dragonfly back end (Q1160), and every mirror instance served and refused uploads. |

Teardown exited 0 and `ops.sh at-rest` reported no instances afterwards.
An earlier CI run on the same candidate, 37357489219, also passed every leg but predates milestone 4 and so left no evidence; the two before it found the gate defects fixed in #2038, #2040 and #2042.

### `v1.9.0-rc.1`

**PASSED, validated 2026-10-03; promotion to `v1.9.0` is the maintainer's call.** `check-artifact-unchanged.sh v1.9.0-rc.1 origin/main` exited 0 at `7fbef71d2` before the re-run (18 files changed since the tag, none released), and the gate records `refs/validated/v1.9.0-rc.1` → `aff5f2268`.
It took three windows: the first never reached a runner, the second passed without the `v2` reading, and the third took it.

#### Second and third windows, 2026-10-03: PASS

Both ran from `main` after #2008, so the e2e worker logged `Current runner version: '2.337.0'`.
The third ran `validate-release.sh` from [PR #2024](https://github.com/actions-gateway/github-actions-gateway/pull/2024)'s head `b74be8a87`, which adds the Q1156 reading; the figures below are the third window's.

| Step | Verdict |
|---|---|
| e2e matrix | **PASS.** Run 37143257884, 3/3 jobs (the second window's run 37139731208 was also 3/3). |
| Sizing legs | **PASS.** `NodeShare` active and deriving 1500m where the templates ask 2 and 3; `Throughput` active on 346 samples. |
| Capacity | **PASS.** The quota rung bound at zero headroom (`withheldCapacity[quota]=2`, `advertisedCapacity=0`) and released when the quota was restored. |
| CRD smoke | **PASS.** The signed `actions-gateway-crds-v2.yaml` verified against the publish identity, applied server-side, and all five CRDs registered. |
| Q1059, Q1060 | **PASS.** All five `v2beta1` kinds carried traffic, and the standing `ActionsGateway` spec is identical at `v2alpha1` and `v2beta1`. |
| Q1156: `v2beta1` ↔ `v2` | **PASS on inspection; the gate recorded `finding`.** 5 of 8 standing objects identical at both versions, and an `EgressProxy` applied *at* `v2` read back identical at both. The three `ClusterRunnerTemplate`s differed only in zero values: `v2` adds an empty `podTemplate.metadata` (and `volumeClaimTemplate.metadata` on the two Kata templates) and drops an `env[].value: ""`. Both decode to the same object: `EnvVar.Value` is `omitempty` and `ObjectMeta` is a struct value Go never omits, so the webhook's Go round trip re-encodes them while a `v2beta1` read returns the stored bytes. Replaying the three logged pairs through the comparison #2024 now uses gives equal specs for all three. |
| Dispatched CI load | Informational, not read by the gate. |

**Definition of Done #2 is met** by Q1156 above: the read direction over every standing object, and the write direction through a `v2` apply, on the dogfood cluster.
**Definition of Done #5 is met**, read off the published artifacts rather than the source: every CRD in `actions-gateway-crds-v2.yaml` (sha256 `a0db0b6a…7c244`) serves `v2`, `v2alpha1` and `v2beta1` with `v2beta1` the storage version, and `charts/actions-gateway/crds/priorityclassallowlist-crd.yaml` at the tag serves `v2` and `v2beta1` with `v2beta1` stored.
That also confirms #1 off the shipped files.

#### First window, 2026-10-02: not taken


Not validated: the gate timed out on its test environment, not on the candidate.
Tagged 2026-10-02 at `aff5f2268`; dogfood run the same evening.

| Step | Verdict |
|---|---|
| Tag points at the target | **PASS.** `v1.9.0-rc.1^{commit}` and the verified target both `aff5f22685b9f9c0db5e874930bb8070cb9cbb40`, compared after creation and before the push. `main` had moved to `b004882d6` (Dependabot config), which `check-artifact-unchanged.sh` shows touches nothing released. |
| Publish pipeline | **PASS.** Run 37079877609 succeeded. |
| Artifacts and provenance | **PASS.** 9/9 assets, `draft: false`, `immutable: true`, `prerelease: true`; all eight signatures verified. `gmc`'s signer URI ends `publish.yml@refs/tags/v1.9.0-rc.1` and its `sourceRepositoryDigest` is the tagged commit; re-run against `unit-test.yml` as the signer it exits 1. |
| Deploy | **PASS.** The GMC rolled out and the e2e gateway's AGC reported `Ready`. |
| e2e matrix | **NOT TAKEN.** Run 37082062554: `changes` passed, and `e2e / e2e` stayed `queued` with no runner for 5400 s. The tenant's worker image is pinned to `dogfood-e2e-runner:2.335.1-2` in `deploy/dogfood-e2e/overlays/kata/kustomization.yaml`; the worker logged `Current runner version: '2.335.1'` and `Listening for Jobs` from 00:38:06Z and was never sent the job. That is Q1146's symptom on the dogfood-only pin, which open PR #2008 moves to `2.337.0-1`. The candidate's own default worker image is 2.337.0. |
| Sizing legs, CRD smoke | **NOT TAKEN.** The gate stops at the e2e leg. |
| Dispatched CI load | Informational, not read by the gate. Integration 4/4 on GAG; unit-test's `lint` hit its 10-minute job limit, as the `v1.8.0-rc.2` window's dispatch also failed `lint`. |

**Teardown did not complete, and `--reclaim` could not see it.** Both stop scripts refused on drains that the never-served job could not let converge, and the gate released its lease anyway, so `--reclaim` reported nothing to reclaim while three instances billed (Q1155, since fixed: a refused stop script now keeps the lease).
The e2e run was cancelled and both stop scripts were run by hand with the drain skips; `ops.sh at-rest` reported no instances at 22:01 PDT.

#2008 merged on 2026-10-03 and changes dogfood setup and no released file, so the candidate stood and the gate re-ran against it.


## What waits for `v2.0.0`

The storage advance and migration (Q1086) and the four removals ([Q273](../../queue/Q273.md), [Q264](../../queue/Q264.md), and `v2beta1` itself).
The validating webhook rules cannot silently un-match through those removals: `make webhook-versions-check` (Q1068) fails any rule naming only versions no CRD serves.
[v2-ga.md](../v2-ga.md#phase-3--the-storage-advance-and-the-coupled-removals) Phase 3 owns the ordering.
