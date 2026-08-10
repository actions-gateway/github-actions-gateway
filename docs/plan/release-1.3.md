# Release 1.3 Milestone Definition

> **Status: no gating Queue row remains — both 2026-07-31 gates closed the same day they were filed.** Q550 and Q551 came out of the `v1.3.0-rc.2` validation window, where the RC gate's dispatched e2e job was wedged by the AGC itself: provisioning leaked runner registrations at GitHub (reap never deregistered them, and names derive from the job ID, so retries 409 against their own leftovers — Q550), and after four attempts the listener dropped the job permanently with no retry, condition, or Event (Q551).
> Both were availability bugs in the scale-set listener an ordinary tenant could hit — any burst of provisioning failures (quota, stockout, admission) starts the same cycle — so they gated the tag rather than riding the backlog.
>
> **Q550** ([plan](archive/q550-runner-registration-leak.md)) made the worker pod the record of its own registration: it carries the runner name, the reaper deregisters that record before deleting the pod, and a listener start sweeps records no live pod claims. **Q551** kept the skip that stops one stuck assignment wedging the batch, but now holds the job and re-offers it on a backoff, surfacing the stall as `JobProvisionStalled` plus a deferred-jobs gauge.
> Q550 removes most of what causes the collisions; Q551 makes what remains recoverable and visible.
>
> **The fixes are verified at unit and integration only.** Neither can be re-confirmed against the live API below a dogfood run, so the RC validation below is where that lands.
> The paragraph below records the pre-2026-07-31 history.
>
> **Previously: no gating Queue row remained.** The original six closed 2026-07-26 (Q359, Q400, Q404, Q411, Q412, Q393), and all four rows the [API review](#e-api-review-satisfied) opened closed 2026-07-28: Q485 with the `windowStartTime` rename shipped, Q484 with a CEL rule requiring `nodeShare.allocatable` to declare cpu, memory, or both, and Q481 (**ship `spec.sizing` as-is, deliberately**) and Q486 (**the two managed-autoscaler opt-ins keep their different shapes, deliberately**) with no API change at all.
>
> **The last gates to clear were all API-shape questions**, which is the pattern worth noticing: once the capability work landed, this release's residual risk was not anything unfinished but surface about to be frozen — cheap to fix until the tag, a conversion shim or a version bump afterwards.
>
> **The tag is still not cut.** The Definition of Done also requires the release-candidate dogfood validation in [operations/release.md](../operations/release.md), which can only run against the actual RC image and is deliberately not tracked as a Queue row.
> Residuals deferred out of 1.3 are under [Explicitly out of scope](#explicitly-out-of-scope).
>
> **Superseded 2026-08-03: `v1.3.0-rc.5`'s dogfood validation PASSED, and the ledger has no gating row left.** Exit 0 in 30m07s: the e2e matrix green on GAG runners (73/73 specs), both sizing profiles `Active`, the `NodeShare` derived value confirmed on a live worker at 1500m — which rc.4's pass could not check — and the signed v2 CRD artifact verified and registered.
> Details in [The rc.5 validation verdict](#the-rc5-validation-verdict-2026-08-03).
>
> Three runs were needed.
> Two died in the gate rather than the release: a watcher that treated a queued job's log 404 as a failed run, and a scale-up blocked by **`CPUS_ALL_REGIONS`** — a global 32-vCPU limit the cluster's own nodes saturated exactly, which no per-family or regional quota reading revealed.
> Both fixed; the quota is now 64. **Cut `v1.3.0` from `main`** so the line carries those harness fixes.
>
> **2026-08-02: `v1.3.0-rc.4` is published, verified, and its dogfood validation PASSED** — the first verdict any RC in this line has produced.
> Details in [The rc.4 validation verdict](#the-rc4-validation-verdict-2026-08-02); the paragraphs below record the earlier history.
>
> **`v1.3.0-rc.3` was published and verified; its dogfood validation was owed.** Tagged 2026-08-01 off `7c18872d`, it is the first RC carrying the Q550/Q551 fixes above — which is the reason it exists, since rc.2's own validation window is what exposed them and rc.2 therefore cannot be the artifact that clears them. `publish.yml` ran green and every artifact verification passed: `make verify-release` (five images, both charts), the v2 CRD manifest and `SHA256SUMS` blob signatures, an SBOM attestation spot-check on the amd64 manifest, SLSA provenance binding to `7c18872d`, and both arches on the index.
>
> **Neither earlier RC produced a validation verdict**, for unrelated reasons. rc.1 (2026-07-31, `2d85b4c6`) published and verified clean, but its validation aborted: the gate's then-repo-wide e2e routing caught concurrent sessions' CI, the teardown deleted the e2e AGC under a caught job, and the stranded queued runs wedged `main`'s e2e concurrency group until they were cancelled.
> That is fixed — the gate routes through a run-scoped `workflow_dispatch` input and `e2e-stop.sh` drains before deleting the AGC — and the orphaned-worker-pod product defect the incident exposed is Queue-tracked (GMC cascade reap). rc.2 then reached the live API and returned Q550 and Q551 instead of a verdict.
> So `validate-release.sh v1.3.0-rc.3` is the first run in a position to clear this gate.

The scope and Definition of Done for the `v1.3.0` tag.
Queue rows that block this tag carry the `1.3-gate` label in [docs/STATUS.md](../STATUS.md); this file is what that label points at, per the "scope the release in a plan doc first, then add the label" rule in [maintaining-backlog.md](../development/maintaining-backlog.md#dont-pre-assign-release-versions-to-backlog-items).

Cutting mechanics (pre-flight, tagging, verification, the dogfood release-candidate gate) live in [operations/release.md](../operations/release.md) and are not repeated here.

## Scope ledger

Planned vs delivered, per the [scope-ledger convention](../development/maintaining-backlog.md#cutting-a-release-the-scope-ledger).
The prose below carries the *why* of each; this table is the state.

| Q-ID | Item | Gates? | Status |
|---|---|---|---|
| Q359 | Worker right-sizing live-validated on dogfood ([§ A](#a-headline-feature-complete-satisfied)) | `1.3-gate` | ✅ shipped 2026-07-25 |
| Q411 | `v2alpha1` deprecation reaches the apiserver ([§ B](#b-deprecation-notice-satisfied)) | `1.3-gate` | ✅ shipped 2026-07-26 |
| Q412 | `v2.0.0` named as the removal release ([§ B](#b-deprecation-notice-satisfied)) | `1.3-gate` | ✅ shipped 2026-07-26 |
| Q393 | Announce bar derived from the git tags ([§ C](#c-release-mechanics-satisfied)) | `1.3-gate` | ✅ shipped 2026-07-26 |
| Q400 | Heavy path gates cover `api/` and `scaleset/` ([§ D](#d-gate-integrity-satisfied)) | `1.3-gate` | ✅ shipped 2026-07-26 |
| Q404 | Build-tagged Go files compiled and vetted in `make check` ([§ D](#d-gate-integrity-satisfied)) | `1.3-gate` | ✅ shipped 2026-07-26 |
| Q481 | `spec.sizing` shape ([§ E](#e-api-review-satisfied)) | `1.3-gate` | ✅ ship as-is, deliberately (2026-07-28) |
| Q484 | `nodeShare.allocatable` must declare cpu, memory, or both ([§ E](#e-api-review-satisfied)) | `1.3-gate` | ✅ shipped 2026-07-28 |
| Q485 | `windowStart` → `windowStartTime` rename ([§ E](#e-api-review-satisfied)) | `1.3-gate` | ✅ shipped 2026-07-28 |
| Q486 | The two managed-autoscaler opt-ins keep their different shapes ([§ E](#e-api-review-satisfied)) | `1.3-gate` | ✅ no API change, deliberately (2026-07-28) |
| Q550 | Scale-set runner registrations leak at GitHub | `1.3-gate` | ✅ shipped 2026-07-31 |
| Q551 | A job the listener cannot provision is skipped permanently | `1.3-gate` | ✅ shipped 2026-07-31 |
| Q576 | Ceiling-blocked provisioning retries hot, one deregister per attempt | `1.3-gate` | ✅ shipped 2026-08-01 |
| Q552 | GMC reverts a `kubectl rollout restart` of a managed AGC | `1.3-gate` | ✅ shipped 2026-08-01 |
| Q553 | AGC re-provisions jobs GitHub no longer has, livelocking a drain | `1.3-gate` | ✅ shipped 2026-08-01 |
| Q582 | v1/v2 proxy pools collide throughout migration coexistence | `1.3-gate` | ✅ shipped 2026-08-01 |
| Q575 | A worker whose `job-payload` secret is absent stalls | `1.3-gate` | ✅ shipped 2026-08-01 |
| Q577 | `stop.sh` leaves the pool up when its drain cannot converge | `1.3-gate` | ✅ closed 2026-08-02 — not a defect ([why](#q577-closed-the-behaviour-is-the-fail-safe-not-the-bug)) |
| Q583 | An AGC restart replays the queue and re-provisions jobs long gone | rides | ✅ shipped 2026-08-01 (measured, then fixed — see below) |
| Q547 | Deleting a v2 gateway orphans its in-flight worker pods, pinning a billable node | `1.3-gate` | ✅ shipped 2026-08-01 |
| Q536 | A GHES appliance behind a private CA cannot be reached | rides | ✅ shipped 2026-08-01 |
| Q600 | The Q260 burst test reads its peak-provisioner count before the winner reaches the handler | gates | ✅ shipped 2026-08-01 — it reddened `main`, which pre-flight requires green |
| Q602 | The Q583 restart test stops the listener before the abandon's message delete is issued | gates | ✅ shipped 2026-08-02 — same reason: it reddened `main` |
| Q603 | An AGC stopped between abandoning a job and its next delete cycle re-provisions it on restart | rides | 🔲 filed — the residual Q583 narrows but does not close |
| Q604 | `stallJob` installs its runner-name conflict after the job is already pollable | gates | ✅ shipped 2026-08-02 — same reason: it reddened `main` |
| Q406 | Capacity gate `AutoscalerVerdict` mode | rides | ⤴ punted — [Explicitly out of scope](#explicitly-out-of-scope) |
| [Q273](../STATUS.md#Q273), [Q264](../STATUS.md#Q264) | `v1alpha1` + `v2alpha1` + classic **removal** | rides | ⤴ punted to `v2.0.0` — [Explicitly out of scope](#explicitly-out-of-scope) |
| — | RC validated on dogfood ([§ A](#a-headline-feature-complete-satisfied)) | gates | ✅ **PASSED on `v1.3.0-rc.5`, 2026-08-03** ([verdict](#the-rc5-validation-verdict-2026-08-03)) — derived sizing confirmed at pod level, which rc.4's pass could not do |
| <a id="Q627"></a>Q627 | The dogfood `e2e` pool has one node of C2 headroom | rides | ✅ closed 2026-08-02 — pool re-created on `n2-standard-8`, verified Ready with the kata label at `N2_CPUS` 8/200 |

**Cut condition: zero open gating rows in this ledger** **plus the release-candidate dogfood validation**, the ledger's last row.
It has no Q-ID because it cannot be a Queue item — it only runs against a published RC — but it carries a row so its state is visible rather than buried in prose.
Read the ledger, not `grep '1.3-gate' docs/STATUS.md`, as authoritative: the rc.3-derived gates were filed as ordinary `bug` rows and pulled into scope by the decision recorded below, so the label grep under-reports them.

**Cut `v1.3.0` from `main`, not from the validated RC's commit.** rc.5's validation produced two fixes to the gate harness itself — the e2e watch's behaviour on a queued run, and the `e2e` pool's machine type ([Q627](#Q627)) — and the harness ships in the branch.
A `release-1.3` cut from rc.5's `a6f168ad` to make the tag match the validated artifact would strand both, so every future `v1.3.x` backport would re-run the gate with the bugs this release already paid to find.
The rule is written up under [Patch releases and backports](../operations/release.md#patch-releases-and-backports).
The delta this leaves to justify at the cut is small and entirely non-shipping: two test files, four docs, four dogfood scripts — no `api/`, no `cmd/` product code, no `config/crd`.

**No RC has produced a verdict yet.** rc.1 aborted when the gate's then-repo-wide e2e routing caught concurrent CI; rc.2 reached the live API and returned Q550 and Q551 instead of a result; rc.3 aborted at `start.sh`'s AGC wait, which raced every rollout and reported a healthy AGC as timed out (fixed in #1090).
Each was diagnosed on its own, which is how three consecutive misses went unremarked — the row exists so the fourth does not.

### What rc.4 carries, and the two scope calls behind it

rc.3's validation window produced five defects (Q575–Q578, Q580) plus a revive trigger on Q553.
Q575, Q576, Q578, and Q580 have shipped; Q577 is unblocked by Q575 landing, and closed at the gate as [not a defect](#q577-closed-the-behaviour-is-the-fail-safe-not-the-bug). **Q576, Q552, and Q553 gate rather than ride** for the same reason Q550 and Q551 did: each is an availability bug an ordinary tenant reaches, not a dogfood-harness artifact.
Q576 in particular spun a saturated scale set at ~0.8 provisioning attempts/s for 14 minutes, issuing 704 GitHub deregister calls for a single job.

**Q582 was pulled into scope by an explicit maintainer decision (2026-08-01), though it was not an rc.3 finding.** It surfaced while diagnosing the Q570 e2e flake: the v1 and v2 proxy pools both stamp `app: actions-gateway-proxy`, so during coexistence each pool's pods match the other's PDB and both HPAs wedge on `AmbiguousSelector` — neither autoscales.
The reasoning for gating on it: 1.3 *is* the `v2.0.0` deprecation notice, so shipping it with a documented v1→v2 migration path that silently disables autoscaling on both pools undercuts the release's own message.

**Q583 rides, and no longer waits on the gate.** Q553's give-up guard is process-local, so a restarted AGC polls from cursor 0 and re-provisions jobs long gone.
That was filed as an unproven mechanism to measure at the rc.4 gate — but the measurement was already in the repo: the Q264 P4 clean-green re-run (2026-07-05) reconnected a rebuilt AGC to an existing scale set and **briefly provisioned 7 workers** for the previous pass's jobs, and Q468 measured the queue retaining an unacked message across a 13 h session gap.
The premise is confirmed; what remains unproven is the `DeleteMessage` wire shape the *fix* would rely on, which [Investigation G](archive/q583-restart-replay.md#the-answer-replayed-delete-ok-pruned-2026-08-01) settled on 2026-08-01 in a single probe run against the live API: the replay is real, `DeleteMessage` answers 204, and deleting prunes the queue.
Neither measurement needed a dogfood cluster, so nothing here ever blocked on rc.4 — and Q583 still rides rather than gating, because it is a restart-time burst rather than a defect an ordinary tenant meets in steady state.

### What landed after that note, and the API surface rc.4 adds

Three things reached `main` after the paragraph above was written, and one of them moves the API surface, so the [API review](#e-api-review-satisfied) is re-run against `v1.3.0-rc.3..HEAD` rather than only against `v1.2.0`.

**Q547 gates, on the rc.1 incident's own terms.** Deleting a v2 `ActionsGateway` removed the AGC that is the tenant's only worker-pod reaper without removing its pods, whose node-disruption-safety annotations then held a billable node up to `maxWorkerLifetime` (12 h) later.
It is the product defect the rc.1 validation abort exposed, filed then and fixed now; v1 is unaffected, since its teardown cascades the pods off their owner reference.
Same test as Q550/Q551/Q576 — an availability and cost bug an ordinary tenant reaches, not a harness artifact.

**Q536 rides.** `ActionsGateway.spec.githubCABundleRef` names a ConfigMap holding a PEM bundle to trust when reaching a GitHub Enterprise Server appliance fronted by a private certificate authority.
It is additive in every sense that matters to a cut: a gateway with no ref renders a byte-identical Deployment, and an unresolvable ref fails closed (`Degraded`, `CABundleNotFound`/`CABundleInvalid`) rather than starting an AGC that would hang on the handshake.
Nothing already deployed changes behaviour, so it does not need to beat this tag — it simply arrived before it.

**Q600 gates, mechanically rather than on severity.** `main`'s `unit-test` leg went red on `7fec2ff8` — `TestMultiplexer_DuplicateJobDeliveryProvisionsOnce` under `-race`, `expected 1, actual 0` — and pre-flight requires a green `main` to tag.
It is a test-synchronization defect, not a product one: the test waited on the duplicate-delivery metric, which only the *losing* siblings increment, and then read the peak-provisioner count the *winner* produces several steps later.
Diagnosed by reproducing the interleaving directly (a delay at the winner's handler entry took it from 0 failures in 400 local runs to 5 of 5), fixed by waiting on the counter the invariant reads, and confirmed still able to catch the Q260 regression by deleting the claim gate and requiring red.
Filed to [flake watch](../STATUS.md#Q600); the weaker dedup predicate the measurement exposed was filed as Q601 and has since been fixed — per-session dedup registries, so the assertion counts distinct siblings rather than deliveries.

**Q602 gates for the same mechanical reason, and is worth more than its fix.** The next `main` run went red on a *different* `-race` test — `TestListener_AbandonedJobDoesNotSurviveARestart`, the Q583 fix's own — with `assert.Never` reporting that a restarted listener had provisioned a worker for a job the previous one gave up on.
It reproduced locally at 1 in 40.

The reason it matters is that **the test was not wrong about the product.** Its barrier was: `abandonDeferredBefore` calls `settle` and then increments the abandoned counter, so the counter is published last and looks like a valid gate.
But `settle` only marks the job concluded *in memory*; the wire delete that releases the message is issued by the next `flushDeletes` cycle, deliberately ("runs per cycle rather than only at settle time").
Stopping the listener inside that gap leaves the message unreleased, so it replays — and the restarted process, its abandoned set empty, provisions for a job GitHub has dropped.
That is Q553's own failure, reached through a window Q583 narrows rather than closes.

So the flake was a faithful, non-deterministic reproduction of a real (narrow) restart window.
The fix pins the test to the path Q583 actually repairs — wait for the stub's `delete-message` call, not the counter — and the residual window is filed as Q603 rather than absorbed into a green test — since [closed](archive/q603-settle-delete-gap.md) for every stop the process can see coming, with the hard-kill remainder closed in turn by Q606's persisted guards.
Deleting the `settle` call still turns the test red, now naming the missing release directly.

**Q604 is the third gate, and the one that says something about the suite.** The Q602 fix's own CI run went red again in the same test, but at a different point — its *setup*, `stallJob`, timing out on "the job that cannot register a runner name must be held for a re-offer".
The helper enqueues the job and only then installs the runner-name conflict meant to stall it, so a poll (20 ms) can assign and successfully provision it in between; it never defers, and the wait times out.
Widening that gap by 60 ms took it from 0 failures in 80 runs to 3 of 3, with CI's exact message.

What makes it worth recording is that **the codebase already knew.** `deferred_test.go` holds advertised capacity at zero until the conflict is staged, commenting that "the fake assigns a queued job as soon as a poll advertises a slot, so staging afterwards races the poll loop"; `conditions_test.go` stages the conflict before the listener starts; `ceiling_q576_test.go` and `listener_test.go` gate on capacity the same way.
Every site that meets this hazard mitigates it — except `stallJob`, which eight tests call.
The fix removes the hazard structurally instead of adding a fifth capacity dance: `scalesetstub.EnqueueStalledJob` registers the conflict inside the same locked section that makes the job pollable, so no window exists to lose.

**Three gates, one root shape.** Q600, Q602 and Q604 were all "the test synchronized on something adjacent to what it asserts", and all three surfaced in AGC listener tests within two `main` runs.
They were fixed individually because each had a distinct mechanism, but the pattern is worth watching: Q601 was a fourth weak barrier in the same family — an aggregate counter standing in for a headcount — and is now fixed too.

**API review verdict — ship as-is.** `scripts/release/api-surface-since.sh v1.3.0-rc.3` reports one field pair (`githubCABundleRef`, and the `LocalConfigMapReference.Name` it introduces) and four condition reasons (`CABundleInvalid`, `CABundleNotFound`, `GatewayTerminating`, `WorkerCeilingReached`); no enum, default, label, or annotation moved.
Applying the [checklist](../development/api-review.md#step-2--ask-these-of-each-addition): the field is optional with an unset meaning that is today's behaviour; it is a name-only local reference, the shape every other v2 reference already uses; and it carries its **own** type rather than reusing `LocalObjectRef` for the stated reason that a core referent gets the full 253-character DNS-subdomain budget instead of the 52-character v2 object budget (§H.6).
That is the distinction `LocalSecretReference` already publishes, so this is a third instance of a settled pattern, not a new one.
The reasons are additive condition vocabulary, which is not a freeze.
Nothing deferred, nothing filed.

### The rc.5 validation attempt (2026-08-02)

**`validate-release.sh v1.3.0-rc.5` FAILED at t+4m07s, and nothing it found implicates the release.** The tag published clean: all seven `publish.yml` jobs green on `a6f168ad`, five image signatures plus both charts and the v2 CRD artifact verified, SLSA provenance binding `publish.yml@refs/tags/v1.3.0-rc.5` to that commit, and both arches on every index.
The gate died in its own plumbing.

**rc.5 needs a verdict of its own.** It carries 26 commits past rc.4, including the Q603 listener-shutdown fix — a product change — so rc.4's pass does not transfer.
That is the decision the rc.4 verdict below said to make explicitly at the cut rather than inherit.

| Leg | Result |
|---|---|
| Lane settle | settled immediately — no in-flight `e2e-test.yml` run, no open PRs |
| Deploy rc.5 | GMC and both tenant AGCs rolled out |
| e2e matrix on GAG runners | **aborted at t+1m13s** — the watcher exited on a queued job's 404 |
| Sizing, CRD smoke | not reached |
| Teardown | drained, then scaled back to 0 nodes |

**Two defects, both in the gate, neither in the product — and they compound.**

`e2e-run-watch.sh` re-fetches each job's log every poll.
That endpoint 404s while a job is queued — the normal state for the minutes before a runner picks it up — and `collect_heartbeats` sent the *message* to `/dev/null` without neutralizing the *status*, so `pipefail` carried it out of the pipe, out of the assignment capturing it, and into `set -e`.
The gate then reported the still-queued run as "did not conclude success".
This is a Q615 regression: rc.4 predates the watcher and used `gh run watch`, which is why the same leg passed a day earlier.

The e2e node pool never scaled.
Its kata runner pod stayed `Pending` behind `FailedScaleUp: GCE quota exceeded`, and the autoscaler went into backoff (`2 in backoff after failed scale-up`).

**The capacity was there; the autoscaler could not reach it.** A direct probe — scaling the pool 0→1 by hand — brings up a `c2-standard-8` carrying `katacontainers.io/kata-runtime=true`, satisfying the pending pod's selector exactly, at `C2_CPUS` 8/8.
The cluster autoscaler asking for the same one node fails with `GCE quota exceeded` and then stays in backoff (`NotTriggerScaleUp: 1 in backoff after failed scale-up`) at `targetSize: 0`. **Why the two paths differ is unverified.** It is not simple exhaustion: at the second attempt every relevant quota had headroom — `C2_CPUS` 0/8, `INSTANCES` 3/24, `IN_USE_ADDRESSES` 3/16 — and the autoscaler still would not retry.

An earlier revision of this section called the refusal transient and said a full-length wait would have recovered it.
The [second attempt](#the-rc5-re-run-2026-08-02) disproves that: the gate waited 30+ minutes and the autoscaler never left backoff.
Recorded because the wrong version shipped once.

What is established about the pool is headroom, not a ceiling: `c2-standard-8` against `C2_CPUS` 8 is exactly one node and `maxNodeCount: 2` is unreachable, so a refused scale-up has nowhere to retry into.
Tracked as [Q627](#Q627).

**Resolved by re-shaping the pool, not by a quota grant.** A request to raise `C2_CPUS` 8→16 was **denied** on 2026-07-31 — while an identical 8→16 ask for `IN_USE_ADDRESSES` was approved 33 minutes earlier on the same project and region, so the size of the ask was not the discriminator.
Nor would a region move help: `C2_CPUS` is 8 in every region checked, a project default applied per-region rather than regional capacity.

The pool is now `n2-standard-8` — same 8 vCPU / 32 GB, on the nested-virt list, against an `N2_CPUS` default of 200.
Twenty-four nodes of headroom beyond the pool's max of 2, and n2 is the family this pool started on, so it is already proven here.
Verified by bringing one up: `Ready`, `n2-standard-8`, `katacontainers.io/kata-runtime=true`, at `N2_CPUS` 8/200.

`c2d` was tried first and is impossible: GCP rejects `--enable-nested-virtualization` for it outright, naming the families that can (A2, A3, C2, C3, C4, C4D, C4N, G2, H3, H4D, N1, N2, N4, N4D, Z3, M4 — no AMD).
The repo's own note had claimed `n2/n2d/c2/c2d`, which is where the wrong choice came from; that note is corrected in [gke-dogfood.md](gke-dogfood.md#part-f--e2e-on-gke-kata-containers) and in the two operations runbooks that repeated it.

**The teardown held.** `e2e-stop.sh` waited on the queued job rather than deleting the AGC under it — the 2026-07-31 incident's fix doing its job — and completed once the unschedulable run was cancelled by hand.

### The rc.5 validation verdict (2026-08-03)

**`validate-release.sh v1.3.0-rc.5` PASSED**, exit 0, 30m07s end to end.
This is the verdict the tag has been owed since it was cut, and it clears the ledger's last gating row.

| Leg | Result |
|---|---|
| Lane settle | instant — no in-flight `e2e-test.yml` run |
| Deploy rc.5 | GMC and both tenant AGCs rolled out, 3m38s |
| e2e matrix on GAG runners | **green — 73/73 specs, 62 ok, 0 failed, 11 skipped** |
| `NodeShare` (`gag-dogfood-e2e`) | `sizingProfileState=Active` — the hard-failure leg |
| `NodeShare` derived value | **`cpu request=1500m` on a live worker**, against templates asking 2 and 3 |
| `Throughput` (`gag-dogfood`) | `Active`, `sampleCounts=[158]` — actuating |
| Signed v2 CRD manifest | blob signature verified; all five v2 CRDs applied and registered |
| Teardown | drained to 0 nodes, exit 0 |

**This pass is strictly stronger than rc.4's.** rc.4 cleared `NodeShare`'s state assertion but reported `derived value NOT checked — no live worker pod was caught during the matrix`, leaving the pod-level envelope arithmetic unconfirmed ([Q448](../STATUS.md#Q448)).
This run caught a live worker: **1500m derived where the templates ask 2 and 3**.
The release's headline feature is now confirmed at the pod level on real GKE rather than inferred from a condition. `Throughput` at 158 samples additionally means this RC ran the repo's own CI on derived sizing — validated in use.

**What unblocked it was a quota nobody had measured.** Two earlier attempts died on `FailedScaleUp: GCE quota exceeded` while every per-family and regional quota sat near zero.
The binding constraint was **`CPUS_ALL_REGIONS`** — a *global* limit of 32, which the cluster's own nodes saturated exactly (2×`e2-standard-2` + 7×`e2-standard-4` = 32).
The autoscaler event never names the quota; the GKE `setSize` API does, in the 429 body.
Raised to 64 on 2026-08-03 and approved immediately.
Detail and the per-pool arithmetic: [gke-dogfood.md](gke-dogfood.md#part-f--e2e-on-gke-kata-containers).

**Cut `v1.3.0` from `main`.** Per [a release line carries its own validation harness](../operations/release.md#patch-releases-and-backports), the harness fixes this validation produced must be in the tag the line descends from.
The delta from rc.5 to `main` remains non-shipping: test files, docs, and dogfood scripts — no `api/`, no `cmd/` product code, no `config/crd`.

### The rc.5 re-run (2026-08-02)

Run against `main` at `c993c897`, with the watcher fix in. **No verdict: stopped by hand at t+33m with the e2e leg unable to start.** What it settled, and what it turned up, are both worth more than the missing verdict.

**The watcher fix is confirmed.** It sat in the e2e leg for 33 minutes where the first attempt died after 13 seconds.
That is the whole of what #1171 claimed.

**It also had no upper bound.** `watch_run` looped until the run reported `completed`, with no deadline, so a run that stayed `queued` held the gate — and its billable nodes — indefinitely.
Before the fix the gate failed instantly and wrongly; after it, it waited forever.
Neither is a bounded wait.
Q629 gave the watch a deadline (`E2E_RUN_WATCH_TIMEOUT`, default 90 minutes) that fails the leg and lets the EXIT trap tear the cluster down.

**The AGC stopped provisioning and reported itself healthy.** This is the finding, and it is in the product rather than the gate.
Three workers were provisioned and each reaped while `Pending`:

```
19:44:14  reaped …5861eaa6…  phase=Pending  reason=completed_pending
19:49:14  reaped …cb18c256…  phase=Pending  reason=completed_pending
19:58:46  reaped …ac9fa6a7…  phase=Pending  reason=pending_deadline
```

After the `pending_deadline` reap the listener logged nothing for 16 minutes and created no further worker — including the 10 minutes after a Ready kata node was present, so capacity was not the constraint.
Throughout, the RunnerSet reported `Ready=True … 1 job(s) assigned`, `WorkersUnschedulable=False`, and `JobProvisionStalled=False — no assigned job is waiting on a runner name or on worker capacity`.
A job was assigned and waiting the entire time.
The dispatched run never left `queued`.

**The mechanism, since confirmed in tests.** A reaped Pending worker is reported to the listener as a **successful job**. `InformerPodWaiter.onPodDelete` resolves the wait with `Phase: PodSucceeded` and `ExternallyDeleted` deliberately unset; the session then maps anything that is not `PodFailed` to `broker.TaskResultSucceeded`, and every disruption-recovery arm requires `PodFailed`, so none fires.
The assignment is concluded, its message deleted, and the job is never re-offered — while at GitHub the workflow job is still queued, no runner having ever registered.
That is the 16 minutes of silence: from the AGC's point of view there was nothing left to do.

Two characterisation tests pinned it, each shown to fail when the mechanism is removed: `TestInformerPodWaiter_Q628_DeletedPendingPodResolvesSucceeded` on the production waiter and `TestProvisioner_Q628_ReapedPendingWorkerReportsSucceeded` on the result mapping.

**Fixed (2026-08-03).** A pod removed before any container started now resolves with `PodOutcome.DeletedBeforeStart`, the session reports the job as `abandoned` rather than succeeded, and the listener releases its own delivery with `completejob` — the same remedy the Q260 fan-out already applies to a deduped sibling's identically dangling assignment, behind the same `AGC_FANOUT_COMPLETION` switch.
Disruption recovery is deliberately *not* armed: `rerun-failed-jobs` has no failed job to act on for a job that never ran, which is the exclusion `externallyDeletedBeforeTerminal` already encodes.
What the run service does with an `abandoned` completion was **measured 2026-08-04** (Q645, [Investigation H](q645-abandoned-completion.md#findings)): it concludes the run as `success` immediately, with no re-dispatch.
A job that never ran reports green, so the release as shipped was the wrong call for the winner's own delivery.
The remedy (Q676, measured 2026-08-04) is to report **nothing**: every accepted completejob value concluded the run `success` and told-nothing gets an honest run+job `cancelled` at ~15 minutes — see [the remedy measurements](q645-abandoned-completion.md#q676--the-remedy-measurements-2026-08-04).

**`JobProvisionStalled=False` was correct, not broken.** The condition covers the listener's *deferral* reasons — `name_conflict` and `ceiling`, jobs held before a worker exists.
A job whose worker was created and then reaped was never deferred, so the condition has nothing to report.
The gap is that no condition covers this outcome at all.

**Not a 1.3 regression.** The "deleted externally → treat as completion" semantics is present in `v1.2.0`; 1.3 refined the neighbouring recovery arms (Q497, Q502, Q575) without changing it.
Tracked as Q628, fixed below.

Class-wise this is Q550/Q551 territory — a listener availability bug reachable by any tenant whose workers go unschedulable in a burst — and both of those gated the tag.
It parts company with them on age: those were defects the RC introduced or exposed in new code, this one has shipped in every release to date.
Whether it gates is a decision for the cut.

### The rc.4 validation verdict (2026-08-02)

**`validate-release.sh v1.3.0-rc.4` PASSED**, exit 0, ~39 minutes end to end.
This is the **first RC in the 1.3 line to produce a verdict at all** — rc.1 aborted on routing, rc.2 returned Q550/Q551 instead of a result, rc.3 aborted at leg 1.
The gate's own row exists because three consecutive misses went unremarked; the fourth did not.

| Leg | Result |
|---|---|
| Lane settle | waited 7 min for `main`'s own e2e run, before any billable work |
| Deploy rc.4 | GMC and both tenant AGCs rolled out |
| e2e matrix on GAG runners | **green** — 30 steps executed, 0 failures |
| `NodeShare` (`gag-dogfood-e2e`) | `sizingProfileState=Active` — the hard-failure leg |
| `Throughput` (`gag-dogfood`) | `sizingProfileState=Active`, `sampleCounts=[127]` |
| Signed v2 CRD manifest | blob signature verified against the publish identity |
| Teardown | drained on the first confirm, 8 → 0 nodes |

**`Throughput` actuated, which this gate is not designed to prove.** The runbook expects `NOT VALIDATED THIS RUN` here: the profile needs ≥20 samples per template container and the gate's matrix is ~7 jobs.
It reported `Active` at 127 samples, because the sampler tracks every worker pod regardless of `spec.sizing` and the aggregate re-seeds from the persisted `status.sizingRecommendation` — so the CI tenant's ordinary traffic had already carried it over the threshold.
The consequence is stronger than a pass: **this RC ran CI on derived sizing**, so the release's headline feature is validated in use rather than merely configured.

**What this run did NOT establish.** `NodeShare` cleared its *state* assertion, but the leg reported `derived value NOT checked — no live worker pod was caught during the matrix`.
The envelope arithmetic at pod level is therefore unconfirmed by this run.
That is the same gap [Q448](../STATUS.md#Q448) already tracks, and it is worth stating rather than reading the green as total: the profile is provably `Active` and provably actuating, and the per-worker share it derives is not independently checked here.

**rc.4 is not `main`.** The tag points at `084f00a5`; `main` has since taken the Q596, Q603, Q605 and docs changes.
Nothing in that set is implicated in what the gate exercised, but a GA tag cut from a later commit carries code this validation did not cover — decide that explicitly at the cut rather than inheriting this pass.

### Q577 closed: the behaviour is the fail-safe, not the bug

The row asserted that `stop.sh` "leaves the pool up when its drain cannot converge", and was left open with an instruction to re-measure at this gate rather than build against it.
Both halves of that instruction paid off, in opposite directions.

**The gate did not exercise the failure branch.** The drain converged on the first confirm, so a clean teardown says nothing about what happens when it cannot — the run is evidence that the *causes* of non-convergence are fixed (Q553's wedge, Q575's `Pending` worker), not that the handling is correct.

**Reading the code settles it instead.** `drain_workers` deliberately refuses to scale down on timeout, and says why: scaling down evicts the tenant AGCs, and an AGC is the only thing that reaps worker pods, so the pool would keep its billable worker nodes up *indefinitely* rather than for the length of one drain.
It then prints which pods are stuck, whether the drain was moving at all, and three remedies keyed to that distinction — re-run, delete the stuck pods, or `SKIP_DRAIN=1` to accept stranding them.
Q580 already removed the one misleading suggestion it used to make.

So the behaviour the row names is real, intentional, and the safer of the two options; the alternative it implicitly asks for is worse.
Closed as not-a-defect.
The generalisable point is that a row asserting a defect is a claim about intent as well as behaviour, and this one was only ever checked against behaviour.

## What 1.3 means

Two things, one of which only a release can deliver.

**The headline is worker right-sizing.** Per-`RunnerSet` usage observability, recommendations surfaced in `RunnerSet.status`, and opt-in auto-apply sizing profiles, with the supporting managed-VPA and bring-your-own proxy autoscaler work alongside it.
This is the first capability in the project with no Actions Runner Controller (ARC) equivalent, so it is the release's positioning story, not just a changelog entry.
Plan: [runner-sizing-profiles.md](runner-sizing-profiles.md).

**1.3 is the deprecation notice for `v2.0.0`.** The project's stated policy is that API removals happen "on a named release announced at least one release ahead" ([roadmap.md](../roadmap.md), [v1alpha1-deprecation.md](../operations/v1alpha1-deprecation.md)).
Three removals are coupled and all land at `v2.0.0`:

| Removed at `v2.0.0` | Currently | Why it is coupled |
|---|---|---|
| `v1alpha1` (`actions-gateway.github.com`) | deprecated, served | already on the removal track |
| `v2alpha1` (`actions-gateway.com`) | deprecated, served | superseded by `v2beta1` as storage version |
| classic acquisition machinery | served | `v2beta1` is ScaleSet-only, so classic exists *only* to serve the two alpha versions |

The coupling is the load-bearing fact: because `v2beta1` is already ScaleSet-only, classic acquisition has no consumer other than `v1alpha1` and `v2alpha1` objects.
Removing those versions removes classic's entire reason to exist, so splitting them across releases would buy nothing and cost operators a second breaking migration.
1.3 announces all three; `v2.0.0` executes all three.

`v2.0.0` itself is gated on the `v2` (General Availability) API being available and validated.
That work is planned separately in [v2-ga.md](v2-ga.md) and is explicitly **not** part of 1.3.

## Definition of Done

All gating items closed, `make check` green, and the mandatory dogfood release-candidate validation from [release.md](../operations/release.md) passing on the latest RC.

### A. Headline feature complete (*satisfied*)

No open gating row: Q359 closed 2026-07-25.

> **The headline feature is fully live-validated, and the dogfood RC gate is satisfied on completion rate.** The second dogfood session (2026-07-25) ran the ScaleSet-migrated tenant to `sampleCount: 36` and confirmed both previously unexercised paths: all three `SizingDrift` states (`SizingWithinRange`, and `SizingDriftDetected` for both waste and OOM risk) and `Binpack` actuating at Guaranteed QoS with derived `requests == limits`.
> Detail: [runner-sizing-profiles.md](runner-sizing-profiles.md#both-20-sample-paths-confirmed-2026-07-25-second-session).
>
> **Completion rate, measured in the same session.** Before the migration, Classic orphaned 81% of the jobs it acquired (85 acquired, 16 worker pods).
> After it, the first 28 GAG jobs ran **28/28 green with zero orphans**.
> A further 14 jobs ran while the tenant was misconfigured mid-session (`maxWorkers` raised past the namespace `ResourceQuota`, an operator mistake made during the soak), of which 6 were non-green.
> That window is excluded from the rate and recorded separately in the plan doc rather than folded in, in either direction.
> Queued jobs also survived a 16-minute AGC outage intact instead of being burned, which Classic could not have done.
>
> **What the gate still needs at tag time** is a *release-candidate* run per [release.md](../operations/release.md) on the actual RC image.
> This session ran `e0acd60`, a pre-release build, so it establishes the tenant and the feature are sound; it does not stand in for validating the tagged artifact.

### B. Deprecation notice (*satisfied*)

No open gating row: Q411 and Q412 both closed 2026-07-26.
The notice now exists in both halves the policy needs, the API surface and the docs.

> **Q411 is closed (2026-07-26): the deprecation reaches the apiserver.** All five `actions-gateway.com` kinds carry `+kubebuilder:deprecatedversion` on `v2alpha1`, so the regenerated CRDs (and their chart copies) set `deprecated: true` plus a `deprecationWarning` naming `v2beta1` as the replacement and `v2.0.0` as the removal release.
> Verified against a real apiserver, not just the generated YAML: on a kind cluster carrying `api/config/crd`, a `v2alpha1` read *and* write each emit `Warning: actions-gateway.com/v2alpha1 RunnerTemplate is deprecated; use actions-gateway.com/v2beta1. v2alpha1 is served until v2.0.0, which removes it.`, the write still succeeds, and the same object read at `v2beta1` emits nothing.
> The warning names `v2.0.0` itself, so the API surface and the docs state one removal release rather than two half-answers. `check-v2-api-sync.sh` now normalises the marker as an entitled per-version difference, alongside `+kubebuilder:storageversion`.

> **Q412 is closed (2026-07-26): `v2.0.0` is named.** [v1alpha1-deprecation.md](../operations/v1alpha1-deprecation.md) is now the standing notice for all three removals rather than for `v1alpha1` alone: it leads with a what-`v2.0.0`-removes table (each row with its replacement and the move), states the coupling, and ends with a pre-upgrade checklist.
> The name is repeated wherever an operator forms a plan from the docs (README, roadmap, getting-started, install, upgrade, tenant-onboarding, migration-v1-to-v2, migration-from-arc, troubleshooting, why-gag) and in the design half (Appendix H, 03-api-contracts).
> Two stale statements were corrected in passing: "you can stay on `v1alpha1` indefinitely" (upgrade.md) and "Classic is slated for removal one *minor* release out" (tenant-onboarding, troubleshooting), which understated a major-tag removal.
> The `CiliumFQDN`/`CalicoFQDN` enum values were left saying "a future release (on the classic/`v1alpha1` deprecation clock)"; naming a release for them was filed as Q428 and is now **settled: `v3.0.0` at the earliest**, not `v2.0.0`.
> They are enum members of the beta version `v2beta1`, which `v2.0.0` keeps serving, and an API element is removable only by incrementing the version — so they outlive this release's bundle by a major tag.
> Stated for operators in [v1alpha1-deprecation.md](../operations/v1alpha1-deprecation.md#a-fourth-deprecation-on-a-different-clock-ciliumfqdn--calicofqdn).

The docs half of the notice already shipped as **Q409**: the ARC migration guide, getting-started, tenant onboarding, install, and the positioning pages were all re-routed onto `v2beta1`, leaving `v2alpha1` described only as the `gag-migrate` on-ramp.
That settles which version new tenants onboard on, which was the open question this release's deprecation decision needed answered.

### C. Release mechanics (*satisfied*)

No open gating row: Q393 closed 2026-07-26.

> The docs-site announce bar's version is now derived from the git tags at build time rather than hand-edited per release, so `v1.3.0` names itself with no pre-flight step and cannot ship the stale banner every prior stable tag did. `publish.yml`'s `announce-bar` job still gates the release, but now by building the site at the tag and asserting the *rendered* banner names it.
> Details: [website.md § The announce bar](../development/website.md#the-announce-bar).

### D. Gate integrity (*satisfied*)

No open gating row: Q400 and Q404 both closed 2026-07-26.

Both mattered for the same reason, which is why they were scoped together: a gate that never ran leaves `main` green on evidence it never gathered, and that undermines the "`main` is green" precondition that [release.md](../operations/release.md) pre-flight assumes.

Q404 closed 2026-07-26: `make check` compiled no build-tagged file, so a compile break in an `integration`/`e2e`/`load` package reached only CI's path-gated heavy tiers, which may not even run on the PR that introduced it. `make build-tags-check` now vets the workspace with every first-party tag enabled, in both the local gate and CI's `lint` job, and a coverage assertion fails the gate if a *new* build tag appears that its list does not cover, so the hole cannot reopen in a new shape.
Deliberately out of the fix: widening `golangci-lint` itself to the tagged trees, which needed its own triage pass and landed separately as Q430 (closed 2026-07-27) — `run.build-tags` now covers the same 102 files, and the 21 findings estimated here turned out to be 100 once golangci-lint's default `max-same-issues: 3` cap was lifted.
Detail: [testing.md § The build-tag gate](../development/testing.md#the-build-tag-gate).

Q400 closed 2026-07-26: `api/**` and `scaleset/**` were added to the integration, security-scan, and e2e filters, and `api/config/**` to manifest-validate — a fourth instance of the same gap, found while fixing the first three, where the workflow validates the five v2 CRDs by name but never gated on the directory holding them.
The residual risk that motivated the gate is unchanged and not retroactively addressed: the scaleset/api-only changes that merged since `v1.2.0` were never seen by those tiers, and this fix only stops new ones from slipping through.
The recurrence guard — linting the filters against `go.work` rather than maintaining them by hand — was Q429, deliberately left out of the gate because it is new tooling rather than a correctness fix.

Q429 closed 2026-07-26 anyway, inside the release: `make path-filters-check` (`scripts/ci/check-path-filters.sh`, also CI's `path-filters` job) now fails when a `go.work` module is missing from a filter whose jobs exercise the whole workspace, when a `filters:` block declares a filter the gate has not been told to treat as workspace-covering or narrow-by-design, or when a pattern names a path that no longer exists.
It reproduces the Q400 gap end-to-end: dropping `scaleset/**` from `integration-test.yml` fails the gate naming the module and the pattern to add.
What it deliberately does NOT decide is whether a narrow filter should have been widened — that judgement is still the reviewer's, and the Q400 residual risk above is unaffected.
Detail: [testing.md § The path-filter gate](../development/testing.md#the-path-filter-gate).

### E. API review (*satisfied*)

1.3 is the first release to run the [pre-release API review](../development/api-review.md), and it is also the release that motivated it: Q476 renamed `capacityGate.mode: On` to `Observe` days before this tag would have published it, caught by an unrelated conversation rather than by any step.

**Reviewed:** the surface `scripts/release/api-surface-since.sh v1.2.0` reports — the `RunnerSet` additions (`spec.sizing`, `spec.capacityGate`, `spec.maxWorkerLifetime`, `status.sizingRecommendation`, `status.sizingProfileState`), the `ActionsGateway` additions (`spec.agcAutoscaling`, `spec.clusterCapacity`), `EgressProxy`'s `spec.managedAutoscaling`, thirteen new condition types/reasons, and the `actions-gateway.com/migrated-from-namespace` label.
None of these has appeared in a tagged release, so all of them are in the cheap window until this tag.

**Found and fixed before the tag:** `capacityGate.mode: On` → `Observe` (Q476) — the value named *that* the gate was on rather than *how* it decides, which stops distinguishing anything once Q407's reserved `Probe`/`Provision` join the same axis.
And `status.sizingRecommendation[].windowStart` → `windowStartTime` (Q485, closed 2026-07-28) — upstream spells timestamp fields `somethingTime` (`startTime`, `lastTransitionTime`), and this is the API's only project-defined `metav1.Time`, so it sets the precedent every later one is read against.
The rename had to land in **both** v2alpha1 and v2beta1: the spoke↔hub conversion is a JSON round-trip (`api/v2alpha1/conversion.go`), so a tag renamed on one side only would have silently dropped the field on conversion rather than failing to compile.

**Found and shipped as-is, deliberately:** Q481 — `sizing.profile` carries two axes (where the request comes from; what limits follow) the same way `capacityGate.mode` did before Q470, leaving a Guaranteed node share and history-derived-requests-under-hand-set-limits without a profile of their own.
Gating because the tag freezes the shape either way. **Closed 2026-07-28 without an API change**, on three grounds: the cost that made Q470 worth a break is absent here — both axes are the set owner's own choice, so nothing asks a tenant to assert a fact they do not own; the axes are not orthogonal, so a split shape still needs a `only meaningful when …` CEL rule for the headroom percent (which is defined off the *observed peak*, and a peak exists only under the usage source); and both cells are reachable **additively** in any later minor, which is the difference that matters — Q470 had to beat its tag because its fix removed enum values, and this one does not.
The review also found the Guaranteed node share is reachable *today*, as a side effect of the limit-lift guard rather than by design — verified, pinned by `TestApplySizingProfileNodeShareLiftedLimitsReachGuaranteed`, and written up for operators in [worker-rightsizing.md](../operations/worker-rightsizing.md#getting-guaranteed-qos-out-of-nodeshare).
Full rationale, and the rule for extending the enum (`profile` is an intent enum: new values name a distinct operator intent, mechanism recombinations go in a sibling field): [appendix-h §H.7](../design/appendix-h-v2-api-decomposition.md#h7-reference-integrity--runtime-conditions-not-admission).

> The premise the row was filed under — "the sizing model is unvalidated" — **expired before the close, and in the direction that made shipping as-is easier, not harder.** `Binpack` was live-validated 2026-07-25 and `Throughput` on the dogfood `ci` tenant days later (Q449), which turns the derivation the split would have had to redefine from an unproven rule into a measured one. `NodeShare` is still envtest-only (Q448) — and it is the profile whose missing cell was the complaint, so the case for reshaping around it is weaker still.

**Also shipped as-is, deliberately:** Q486 — 1.3 publishes two managed-autoscaler opt-ins with **different shapes**: `EgressProxy.spec.managedAutoscaling` is a `*bool` defaulting `true` (an *opt-out* of a managed HorizontalPodAutoscaler, Q173), while `ActionsGateway.spec.agcAutoscaling` is a block whose *presence* is the opt-in into a managed VerticalPodAutoscaler and which carries its own `mode` enum (Q360).
An operator meeting both in one changelog can fairly ask why "managed autoscaling" is spelled two ways. **Closed 2026-07-28 without an API change**, on three grounds, each of which decides its own side independently:

1. **The direction is not a style choice — it follows what already ships.** The proxy pool's HPA was managed *before* 1.3; Q173 adds only the ability to stop managing it, so the field must default to today's behaviour and can only be an opt-out.
   The AGC VerticalPodAutoscaler is new, with no behaviour to preserve, so it defaults off.
   Reversing either is the actual defect: an opt-in `managedAutoscaling` deletes the HPA of every pool that upgrades, and a default-on `agcAutoscaling` hands the single AGC pod's requests to an autoscaler nobody asked for.
   Making them symmetric would mean breaking one of those two, so this difference survives any redesign.
2. **The container follows whether the opt-in carries knobs.** `managedAutoscaling` is a pure ownership toggle — the HPA's knobs (`minReplicas`, `maxReplicas`, `targetCPUUtilizationPercentage`) already exist as siblings and predate it, so a block would have to either move them (a wire break) or sit next to them holding nothing. `agcAutoscaling` carries `mode`, which is meaningful only when opted in; block presence gives that knob a home *and* is the switch, so there is no `enabled: true` + `mode:` pair to keep consistent — which is exactly the "sibling fields gated by one value" tell under [one field answers one question](../development/api-review.md#one-field-answers-one-question).
3. **Consistency is owed to the field's neighbours, not to the other CRD.** `managedAutoscaling` sits beside `managedNetworkPolicy` on the same `EgressProxySpec` — same `*bool`, same `+kubebuilder:default=true`, same "the GMC owns this object unless you say otherwise" meaning — and `managedNetworkPolicy` shipped in `v1.1.0`, so that pattern is already published and already learned.
   Reshaping `managedAutoscaling` to match a field on a different CRD would make it the odd one out in the object an operator actually reads it in.

> **The `*bool` was checked against [prefer a string enum](../development/api-review.md#prefer-a-string-enum-to-a-bool) rather than grandfathered.** It passes because the axis it names is genuinely two-valued: it answers "does the GMC own the `<name>-proxy` HPA?", and *who else* owns scaling is deliberately not our question — an external KEDA, VPA, or custom HPA targets the stable Deployment name without telling us.
> If we ever manage a second autoscaler flavour, that is a new sibling naming the mechanism, not a third value here — additively, in any later minor.

The accept is a real freeze: `managedAutoscaling` could not become a block later without a wire break.
What is bought for that is a field that matches its own object and preserves upgrade behaviour; what is paid is one cross-CRD asymmetry, mitigated by both shapes being documented where operators meet them ([tenant-onboarding.md](../operations/tenant-onboarding.md#letting-an-autoscaler-size-the-agc-agcautoscaling)) and by the rule now generalised in [api-review.md § Let the opt-in's direction follow what already ships](../development/api-review.md#let-the-opt-ins-direction-follow-what-already-ships).

**Found by the second pass, and now closed too:** the three further gating rows that second pass filed were Q485 and Q486 above, and one last one:

- **Q484 — fixed 2026-07-28.** A `nodeShare.allocatable` carrying neither cpu nor memory was admitted, and `sizingProfileState` then reported `Active` while nothing was derived.
  Fixed with the CEL rule the row scoped — `'cpu' in self || 'memory' in self`, on the `allocatable` field itself rather than on `sizing`, so ratcheting suppresses it on every write that does not touch the envelope.
  Declaring one of the two stays valid: the other resource keeps the template's ask.
  Rationale in [appendix-h §H.7](../design/appendix-h-v2-api-decomposition.md#h7-reference-integrity--runtime-conditions-not-admission); the rejection has an [operator runbook](../operations/troubleshooting.md#runnerset-rejected-nodeshareallocatable-declares-neither-cpu-nor-memory).
  This was a **validation tightening**, which is wire-breaking after a tag — the reason the row was 1.3-gating rather than ordinary backlog.

> **Q481 and Q484 both concerned `NodeShare`, and did not overlap.** Q481 asked whether the *shape* is right and answered yes; Q484 was a missing validation on a field that shape already has.
> Closing Q481 as ship-as-is neither fixed nor blocked it — worth stating because "the sizing shape was reviewed and accepted" is easy to misread as covering both.

**Accepted without change:** the bare-`string` enum fields (`capacityGate.mode`, `sizing.profile`, `clusterCapacity.nodeAutoscaling`, `status.sizingProfileState`) versus the named types used by `VPAUpdateMode` and `EgressPolicyMode`.
Wire-identical either way, so it is a Go-API break for `api` module consumers only and does not need to beat this tag.

## Explicitly out of scope

| Deferred | Was | Why out of 1.3 |
|---|---|---|
| Capacity gate `AutoscalerVerdict` mode | Q406 | The quota pre-claim rung and `SchedulerVerdict` (Q405) shipped; `AutoscalerVerdict` was M-sized and unstarted at the cut. Describe what shipped as exactly that rather than implying the full ladder. (It shipped after 1.3, on 2026-07-27.) |
| `v1alpha1` + `v2alpha1` + classic **removal** | [Q273](../STATUS.md#Q273), [Q264](../STATUS.md#Q264) | 1.3 is the *notice*. Executing the removal in the same release it is announced would violate the one-release-ahead policy. These land at `v2.0.0`. |
| `v2` GA API version | [v2-ga.md](v2-ga.md) | Gated on a beta soak that has not started. Deliberately slow: GA signs a permanent backward-compatibility contract. |

## Critical path & ordering

**Nothing is left to order.** Six gating items closed 2026-07-26: both gate-integrity items (Q400, Q404), both halves of the deprecation notice (Q411, Q412), and the announce bar (Q393); the API review's Q481 closed 2026-07-28 without an API change ([§ E](#e-api-review-satisfied)), as did Q486, and neither needed ordering because a no-op decision has no dependents; Q485 closed the same day with the `windowStartTime` rename shipped, and Q484 with a CEL rule on `nodeShare.allocatable`.
Each of the four was a self-contained change to one field or none, independent of everything else here.
Neither half Q412 named `v2.0.0` where operators plan from the docs, and Q411 put the same release into the apiserver warning, so an operator who never reads the docs still gets told.

The announce bar used to sit at the end of this list, to be landed "last, immediately before tagging" so it named the version being cut.
Q393 made its version derive from the tag list at build time, so it no longer needs a place in the ordering at all.

One thing that remains is not a Queue item at all: the release-candidate dogfood validation in [§ A](#a-headline-feature-complete-satisfied), which can only be run against the actual RC image.
It is last regardless — it validates the RC the other rows have already shaped.

## Guardrails

- Removing a served API group is a breaking change.
  That is why all three removals are pinned to a **major** tag rather than a minor, and why 1.3 must ship the notice rather than quietly reserving the right to remove later.
- The deprecation of `v2alpha1` does **not** shorten its served life: it stays served until `v2.0.0`, exactly like `v1alpha1`.
  Deprecation marks intent and emits an apiserver warning; it removes nothing.
- Nothing in 1.3 requires a tenant to re-apply anything.
  The `v2beta1` conversion webhook already round-trips every served version.
