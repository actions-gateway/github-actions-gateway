# v2 GA graduation plan (`v2beta1` → `v2`)

The last rung of the graduation ladder defined in [v2-api.md § API maturity & graduation](v2-api.md#api-maturity--graduation-v2alpha1--v2beta1--v2), and the release that executes the three coupled removals announced by [release-1.3.md](archive/release-1.3.md).

This plan starts **after `v1.3.0` ships**.
It is deliberately unhurried: General Availability (GA) signs a permanent backward-compatibility contract on a five-kind API surface, and the contract cannot be walked back.

> **Status: Phases 1 and 2 are done.** The soak's measurable criteria were read in the `v1.8.0-rc.1` window on 2026-09-14 and both came back positive ([the readings](#soak-readings)), and Q413 then shipped Phase 2 for 1.9: `v2` served beside `v2beta1` on every kind, storage unmoved.
> **Scoped 2026-10-09**, after `v1.9.0` tagged on 2026-10-05: the [scope ledger](#scope-ledger) is the release's record.
> Phase 3's storage advance ships first, in its own release: Q1086 landed it for [1.10](archive/release-1.10.md), `v2` storing on every kind and `gag-migrate storage-version` rewriting what is stored, decided by the maintainer on 2026-10-09 ([why it cannot share an apply with the removals](#the-storage-advance-needs-a-state-between-two-applies)).
> The removals follow in `v2.0.0`: Q273 removed `v1alpha1` and Q264 removed classic on `main`, and Q1167 removed `v2alpha1`, `v2beta1` and the hub; Phase 4's docs landed, Q1284 set one Kubernetes floor at 1.35, Q1312 added two stricter admission rules, the 2026-11-09 security audit gated Q1321, which extended the GMC's confinement policy, and Q1322, which added eight stricter admission rules, Q1220's decision gated Q1330, which refuses fork pull request jobs by default, and the tag is [Q1107](../queue/Q1107.md).
> The Phase 2 alias decision is [taken](#decided-v2-omits-ciliumfqdncalicofqdn) (Q452, 2026-09-08).
> The phase table below is the real state.

## Status at a glance

| Phase | Scope | Sz | Status |
|---|---|---|---|
| 0 | Soak criteria + Definition of Done audit recorded (this change) | S | ✅ Done — this change |
| 1 | Beta soak: accumulate the evidence that `v2beta1`'s shape is right | M | ✅ Done 2026-09-14 — criterion 1 elapsed, criteria 2 and 3 read positive ([the readings](#soak-readings)); criterion 4's two open items are Phases 2 and 4 themselves |
| 2 | Add `v2` to each kind and serve it beside `v2beta1`; extend conversion coverage. **Ships in 1.9, not 2.0** | M | ✅ Done in Q413: `v2` is a second conversion spoke and the hub stays at `v2beta1` ([why](#the-hub-stays-at-v2beta1)) |
| 3 | Mark `v2` storage, migrate stored objects, then drop `v2beta1`, `v2alpha1`, `v1alpha1`, and classic | M | ❌ Open: the storage advance landed for 1.10 in Q1086, and the release gate's `storage` leg takes its dogfood reading (Q1171); Q273's `v1alpha1` removal and Q264's classic removal are on `main`, and so is Q1167's drop of the older versions; capability parity **cleared**: Q417/Q443/Q446 cleared the audit's three rows (2026-07-26), Q766 closed the abandoned-run asymmetry inside 1.4, and Q713 put the duration and latency series on both tiers (2026-08-11). See the [parity table](#capability-parity-is-a-precondition-of-the-removal) |
| 4 | Operator docs, migration guide, and the `v2.0.0` cut | S | ❌ Open: the operator docs and the migration guide landed, and the cut remains ([Q1107](../queue/Q1107.md)) |

## Scope ledger

What the `v2.0.0` tag waits for, per the [scope-ledger convention](../development/maintaining-backlog.md#cutting-a-release-the-scope-ledger).
Phase 2 is not here: Q413 gated 1.9, and [release-1.9.md](archive/release-1.9.md#scope-ledger) carries it, as it did Q1150, the validators' retype onto `v2`.
Nor is the storage advance: Q1086 gated 1.10, and [release-1.10.md](archive/release-1.10.md#scope-ledger) carries it.
`make release-ladder-check` holds the `Gates?` column to each row's labels in both directions.

| Q-ID | Item | Gates? | Status |
|---|---|---|---|
| Q1167 | Drop `v2alpha1`, `v2beta1` and the conversion hub once both controllers are on `v2` (Phase 3) | `2.0-gate` | ✅ landed: every CRD serves `v2` alone, with the two upgrade findings in [the split](archive/q1167-v2-retype-split.md#what-compiles-and-still-breaks) |
| Q273 | Remove the `v1alpha1` group and `gag-migrate`'s fan-out (Phase 3) | `2.0-gate` | ✅ landed: no `actions-gateway.github.com` kind, controller, CRD or webhook is left, and `gag-migrate` keeps only `storage-version` |
| Q1275 | Give the shipped alerts and dashboards scale-set twins before Q264 | `2.0-gate` | ✅ landed: `ActionsGatewayScaleSetNoSession` pages on a listener holding no session and `ActionsGatewayMessagePollFailing` tickets an open one whose polls fail, both reading series the scale-set listener writes; the tenant and platform dashboards chart what a scale set has queued at GitHub |
| Q1274 | Port the e2e suite from classic to the scale-set tier, so Q264 can clear the merge queue's e2e lanes | `2.0-gate` | ✅ landed: every fake-GitHub spec runs on the scale-set tier, and the specs only classic could express are deleted; the live-GitHub suite's port is [Q1272](../queue/Q1272.md) |
| Q264 | Remove the classic acquisition machinery (Phase 3) | `2.0-gate` | ✅ landed: no AGC or GMC code reads `acquisitionProtocol` or `maxListeners`, and no shipped alert or dashboard panel reads a series it removed; what operators could still see of the tier went with Q1273, and the classic code nothing can reach is [Q1272](../queue/Q1272.md), not a gate |
| Q1262 | Retype the GMC onto `api/v2` (Phase 3) | `2.0-gate` | ✅ landed, with the classic-reading trio left on `v2alpha1` for Q1167 ([why](archive/q1167-v2-retype-split.md#what-q1262-left-on-the-older-versions)) |
| Q1263 | Retype the AGC onto `api/v2` (Phase 3) | `2.0-gate` | ✅ landed: no AGC code imports `v2alpha1` or `v2beta1` outside the two conversion tests Q1167 deletes and the integration suite scheme they need, and the `RunnerSet` selectable field is declared at `v2`, the version the AGC watches |
| Q1258 | Strip the Q147 legacy tenant-label dual-read (Phase 3's last step, below) | `2.0-gate` | ✅ landed: the admission policies and the GMC read only the `actions-gateway.com` tenant marker, privileged grant and downgrade annotation |
| Q1271 | The GMC deletes its Secret-reader RoleBinding from ex-tenant namespaces | `2.0-gate` | ✅ landed: the GMC deletes its own binding once a namespace loses the tenant marker, through a delete-only exception in `gmc-tenant-resource-guard` |
| Q1273 | Retire the classic tier ledgers, runbooks and unread AGC env vars | `2.0-gate` | ✅ landed: the tier ledgers are gone and their tier-free checks stay as `metric-reference-check` and `event-runbook-check`, the GMC no longer injects `GITHUB_ORG_URL` or `GITHUB_RUNNER_VERSION`, the operator pages describe the scale-set tier, and the Q1011 agent-identity guard is removed, so `RunnerSet` names may repeat across namespaces |
| Q1285 | Check other tightened CEL rules for the 1.31/1.32 status-write refusal | `2.0-gate` | ✅ landed: gated because a hit freezes a stored gateway's status on versions the charts allow; no rule tightened since v1.10.0 refuses a reachable stored value, and the endpoint rule carries the fix |
| Q1278 | Trace the scale-set provision path; the AGC emits no spans after Q264 | `2.0-gate` | ✅ landed: each scale-set job is a `Provisioner.ProvisionScaleSetWorker` span with `stageJobSecret`, `countActivePods` and `createPod` children; reaching a collector takes `spec.tracing.networkPolicy` from Q1281 |
| Q1281 | An operator-named `spec.tracing` peer the GMC turns into an AGC-only egress rule, and a collector CA path so a proxied tenant can export over TLS | `2.0-gate` | ✅ landed: `spec.tracing.networkPolicy` and `caBundleRef`, with the spec on the Calico lane and the endpoint gaps in [Q1282](../queue/Q1282.md) |
| Q1283 | `spec.tracing.endpoint` rejects a value the exporter would misread | `2.0-gate` | ✅ landed: gated because tightening costs least while a major is open; an unchanged value passes the rule, so a stored one keeps working |
| Q1290 | First-label prefix overlap lets a sweep deregister another set's runners | `2.0-gate` | ✅ landed: gated because one tenant's first label could cost another a job with no visible failure; the sweep deletes only names shaped like the ones it mints, which covers a pair already running, and what github.com does with such a job stays unmeasured ([why](../design/05-security.md#cross-tenant-deletion-of-a-runner-record-by-first-label-overlap)) |
| Q1291 | Drop the one-valued `tier` label from the disruption metrics | `2.0-gate` | ✅ landed: no disruption series carries `tier`, the two tenant panels group on `cause` and `outcome`, and `upgrade.md` warns that a `tier` filter now returns nothing; the `tier` log field stays |
| Q1284 | Raise the Kubernetes floor to a version CI tests | `2.0-gate` | ✅ landed: gated because the charts accepted versions the docs ruled out and CI did not test; the charts, the preflight and the docs state 1.35, a test holds them to the e2e node image, and `upgrade.md` puts the cluster upgrade first |
| Q1312 | Reject out-of-order `priorityTiers` and an insecure `https://` tracing endpoint | `2.0-gate` | ✅ landed: gated 2026-11-09 by the pre-`v2.0.0` API review, because a stricter rule needs a major and the next one after this is `v3`; both rules pass an unchanged stored value, and `upgrade.md` names how to find a stored object each one now refuses |
| Q1321 | Make the GMC confinement claim in the security design true | `2.0-gate` | ✅ landed: gated 2026-11-09 by the [security audit](security-audit-2026-11.md#confinement-claim), because General Availability is when the claim becomes a commitment; the guard matches ConfigMaps, CNI policies and ServiceMonitors, a test holds the ClusterRole's write grants to the shipped policies, and the security design names what stays cluster-wide |
| Q1322 | Eight stricter admission rules from the security audit | `2.0-gate` | ✅ landed: all eight rules from the [security audit](security-audit-2026-11.md#stricter-admission-rules), gated 2026-11-09 because a stricter rule needs a major and the next one after this is `v3` |
| Q1330 | Refuse fork pull request jobs by default on every `RunnerSet` | `2.0-gate` | ✅ landed: gated 2026-11-10 by Q1220's decision, because a refusing default breaks fork CI and the next major after this is `v3`; `spec.forkPullRequests` defaults to `Refuse`, a refused job's run is force-cancelled with a check run naming the field where the App may write one, and `upgrade.md` names the patch that restores fork CI |
| [Q1107](../queue/Q1107.md) | Operator docs, migration guide, and the cut (Phase 4) | `2.0-gate` | 🔲 open: the docs landed, with the upgrade and rollback order measured against a running AGC, and the cut remains |
| Q555 | Detect flaky jobs and record each failure's platform context | `2.0-gate` | ✅ landed on the scale-set tier: the failure ledger's `failures` map, the worker snapshot, the pairing and its three metrics; the dogfood readings it rests on are [Q1236](../queue/Q1236.md) |
| Q1183 | Retry classified platform failures, starting with OOM under right-sizing | `2.0-gate` | ✅ landed on the scale-set tier: the out-of-memory class, its stampede breaker and the per-job memory floor, without R12's dogfood reproduction; the deferred wait landed in Q1244 and the misclassification alert in Q1245, neither gated |
| Q1181 | Skip known-futile re-runs on the existing disruption arms | `2.0-gate` | ✅ landed on the scale-set tier: `run_cancelled` on every arm, `attempt_limit`, `run_too_old` and `superseded` before the budget is reserved; classic takes none, and Q264 removes it |
| Q1178 | Label egress proxy outcome metrics by allowlist entry and outcome | `2.0-gate` | ✅ |
| Q1180 | Dogfood keeps no AGC or proxy metrics past the pod's life | `2.0-gate` | ✅ applied 2026-11-02: all 15 scrapes in a 15-minute window landed with `up` at `1`, and one scrape carried 405 samples, 77 of them `actions_gateway_*`; dogfood runs no `EgressProxy`, so the proxy half has nothing to scrape ([measurements](../../deploy/dogfood-metrics/README.md#applied-and-what-it-measured)) |
| - | RC validated on dogfood | gates | 🔲 no candidate cut |
| [Q275](../queue/Q275.md) | Reconcile the capacity and density docs with the ScaleSet default | rides | 🚫 blocked on Q264, whose removal makes the classic framing false |

**Scoped 2026-10-09, and nothing rides yet.** The removals are gated on the 1.10 tag as well as on their rows, since `main`'s next tag once one lands is `v2.0.0`.
`scripts/release/semver-floor.sh v1.9.0` read `FLOOR: NONE` over the two commits since the tag, so the scope is the gating rows alone.
The ledger takes a row for each item that merges onto the released surface before the tag, as 1.9's did.

**Widened 2026-10-25** by the maintainer to gate on automatic recovery: Q555's flaky-job detection and the open rows split from it, retry (Q1183, Q1181), labelled proxy outcome metrics (Q1178) and dogfood metric retention (Q1180).
Q1184, the proxy failure attribution retry reads, is not here: it gates 1.10.
Unlike the removals, none of these waits on the 1.10 tag.

One row ended with the release rather than shipping in it.
Q416, a classic-listener defect parked until classic removal slipped, was retired when Q264 landed.

## Why this is gated on a soak, not a date

The graduation ladder in [v2-api.md](v2-api.md#api-maturity--graduation-v2alpha1--v2beta1--v2) sets the contract at each level:

| Level | Contract |
|---|---|
| `v2alpha1` | may change incompatibly or be dropped without notice |
| `v2beta1` | won't be removed; changes carry a migration path; production-relyable |
| `v2` (GA) | backward-compatible, effectively frozen |

The jump that matters is beta to GA.
Beta still permits a shape fix *with* a migration path; GA does not.
So the question this plan answers is not "has enough time passed" but "do we have evidence the shape is right."
Cutting GA early converts every remaining design mistake into a permanent one.

## Phase 1 — the soak (what "well validated" means)

GA is blocked until **all** of the following hold.
These are the evidence bar, and none of them is a calendar check.

1. **No incompatible `v2beta1` shape change has been needed** across at least two minor releases of real use.
   A field addition is fine.
   A field whose meaning, type, or defaulting had to change is a soak reset, because it is exactly the class of change GA forbids.
2. **Every kind has carried real traffic.** All five kinds exercised on the dogfood cluster, not just the two the CI path happens to use.
   An unexercised kind is an unvalidated kind.
3. **The conversion webhook has round-tripped every served version under real objects**, not only envtest fixtures, including objects created before the `v2beta1` graduation.
4. **The v2 GA Definition of Done in [v2-api.md](v2-api.md#definition-of-done-v2-ga) audits clean.** That list predates this plan and is authoritative; Phase 0 records the audit below rather than restating the criteria.

### Soak readings

Criteria 2 and 3 are settled by measurement, so this table holds the measurements rather than an opinion about them.
Q1048's mirror-client census rides along in the same window because it needs the same dogfood kubectl.

The dogfood gate writes one record per reading to `tmp/soak-readings.jsonl` and `scripts/dogfood/soak-readings.sh` renders them as the rows below, so a reading taken on a host that keeps the stream is never transcribed by hand; the note under the table names the one exception.
A window is billable and cannot be replayed; a reading that lives only in a terminal is a reading lost.

**A reading that was not taken is not a pass.** 🔲 says the window could not produce the measurement, which leaves the criterion exactly as unmet as before it ran. ⚠️ is not a failure either: a negative reading is the shape defect this soak exists to find while beta still permits fixing it.

**Nothing in this table gates a release.** These are soak readings for the `v2` graduation, so a 🔲 is a measurement this project still wants, never a task standing between a candidate and its tag.
What gates 1.8 is its own [definition of done](archive/release-1.8.md#definition-of-done), and Q1048 does not appear in that release's scope ledger at all.

| Reading | Criterion | Verdict | What it found | Window |
|---|---|---|---|---|
| Q1048 | mirror-client-census | ✅ Taken, positive | every client that reached a mirror is a workload-labelled pod or the kubelet | `v1.10.0-rc.2` on `gag-dogfood`, 2026-10-27 |
| Q1048 | mirror-client-census-dragonfly | ✅ Taken, positive | every client that reached a mirror is a workload-labelled pod or the kubelet | `v1.10.0-rc.2` on `gag-dogfood`, 2026-10-27 |
| Q1059 | criterion-2-every-kind | ✅ Taken, positive | all five v2beta1 kinds present; EgressProxy reconciled to Ready | `v1.8.0-rc.1` on `gag-dogfood`, 2026-09-14 |
| Q1060 | criterion-3-conversion-round-trip | ✅ Taken, positive | the standing tenant's ActionsGateway spec is identical across v2alpha1 and v2beta1 | `v1.8.0-rc.1` on `gag-dogfood`, 2026-09-14 |

The Q1059 and Q1060 rows are `scripts/dogfood/soak-readings.sh`'s output pasted unedited, which is the point of it existing.
The two Q1048 rows are in its format but transcribed from the gate's job log (run 40407403332), taken before the CI gate uploaded the readings stream as an artifact ([Q1223](../queue/Q1223.md)).

**What criterion 2's reading covers.** The window manufactured a `v2beta1` `EgressProxy` in the standing tenant, which is the one kind `setup.sh` deliberately never creates, and it reconciled to Ready; the other four were already carrying this cluster's real CI traffic.
So all five kinds have now reconciled on the dogfood cluster rather than only in envtest.

**What criterion 3's reading covers, and what it does not.** One real object, the standing tenant's `ActionsGateway`, read back at both served versions of the v2 CRD with identical specs, so the hop is lossless over an object that has lived on this cluster since 2026-07-25.
It is one kind, not five, and this window did not establish whether that object predates the `v2beta1` graduation.
The criterion's own wording asks for every served *version* rather than every kind, which this meets; whether it should also ask for every kind is a question for whoever closes Phase 1, not something the reading settles.

**Q1048's census is not a criterion.** It rides along because it needs the same dogfood kubectl.
Through the 1.8 cycle it refused on a link-local `169.254.4.6` that resolved to no pod and no node, and that refusal was recorded as taken-nothing rather than as a pass.
Since the proxy logs the User-Agent, that address is exempted as the kubelet's `kube-probe/` `GET /v2/`, and the census has graded a pass on both back ends in the `v1.9.0-rc.2` and `v1.10.0-rc.2` windows.

### Definition of Done audit (as of this change)

| DoD item | State |
|---|---|
| M1, M2, M3a, M3b, M5 shipped | ✅ Satisfied — see [v2-api.md](v2-api.md) milestones |
| Graduated `v2alpha1` → `v2beta1` (webhook + storage migration) | ✅ Satisfied — Q74 |
| Graduated `v2beta1` → `v2` | ❌ This plan, Phase 2 |
| `v1alpha1` deprecated **with a named removal release** | ✅ Satisfied: Q412 (2026-07-26) names `v2.0.0` across the operator and design docs, for `v1alpha1`, `v2alpha1`, and classic together. The notice ships with `v1.3.0`, one release ahead. |
| ≥1 representative tenant migrated v1→v2 with the tool for real | ✅ **Satisfied: Q415 (2026-07-28).** A live `v1alpha1` privileged-DinD tenant on the GKE dogfood cluster was migrated with `gag-migrate --apply` and then ran a real GitHub Actions DinD job green on the scale-set path, with a green baseline before the migration for attribution. Evidence: [q415-migrate-dogfood-validation.md](archive/q415-migrate-dogfood-validation.md#part-2--the-job-half-run-after-the-workflow-reached-main-2026-07-28). Found four defects — Q463, Q465, Q466, Q467 — none of which gate this row. |
| Operator docs updated | ❌ Phase 4 |
| Cross-namespace sharing (M4), direct egress | Not GA gates, by the DoD |

## Phase 2 — the graduation hop

Mechanically identical to the `v2alpha1` → `v2beta1` hop, which is the useful part: the machinery already exists and does not need redesigning.
Per [v2-api.md](v2-api.md#api-maturity--graduation-v2alpha1--v2beta1--v2), each hop is:

1. Add the `v2` version to each of the five kinds and mark it `+kubebuilder:storageversion`.
2. Extend the existing `Hub`/`Convertible` conversion webhook to round-trip the new served set.
3. Storage-migrate stored objects, then drop the superseded served version.

**This hop is split across two releases, which the previous two hops were not.** Step 1's storage marker and all of step 3 move to Phase 3 and the `v2.0.0` tag; 1.9 ships step 1 without the marker, plus step 2.
Rule #4b requires a release serving both versions before storage advances, and what that buys is a rollback destination for the largest upgrade this project asks anyone to make ([release-ladder.md](release-ladder.md#why-19-exists-the-storage-version-cannot-advance-in-the-same-release-that-introduces-v2)).

`PriorityClassAllowlist` gained `v2` too, a sixth kind the five-kind list predates (Q492 added it at `v2beta1` only).
Left at `v2beta1` alone, it would reach `v2.0.0` with no successor version to store at, and it would owe that release the same Rule #4b overlap.
It has no conversion webhook: its schema is identical at both versions, so the apiserver's `None` strategy rewrites `apiVersion` alone.

### The hub stays at `v2beta1`

This plan said the hub moves to `v2` and was free to, on the reasoning that nothing ties the hub to the storage version.
That half is true, and moving it buys nothing: `v2.0.0` drops every other served version, leaving nothing to convert between, so a hub moved in 1.9 would be torn down one release later.

Keeping it also keeps the one lossy value off the shared path.
`v2` cannot hold a `CiliumFQDN`/`CalicoFQDN` alias ([decided below](#decided-v2-omits-ciliumfqdncalicofqdn)), so a `v2` read carries it in the conversion annotation `conversion.actions-gateway.com/egress-policy-mode`, shows `FQDN`, and a `v2` write restores it. controller-runtime routes a spoke-to-spoke conversion through the hub, so a `v2` hub would put that annotation on every `v2alpha1` and `v2beta1` conversion of a stored alias, including the `v2alpha1` conversion the validating webhooks see: they are registered at `v2alpha1` with `matchPolicy: Equivalent`, which is how Q1085's guard sees a `v2beta1` write.
With the hub at `v2beta1`, only a request that names `v2` meets the annotation.

**Why a `v2` read carries the alias rather than refusing it.** Q413 first refused it, failing the conversion.
Its review measured, in envtest, that a refusal fails every `v2` `LIST` holding the object, the metadata-client `LIST` and `DELETECOLLECTION` included.
Those are the calls the namespace deleter makes, and it and the garbage collector list at the group's preferred version, which serving `v2` makes `v2` (read from kube-controller-manager source, not observed on a control plane).
One stored alias would leave its namespace `Terminating` and every `EgressProxy` untracked by the collector.

Two project-specific constraints carry over from the last hop and should be read before starting: shared version-neutral code lives in `api/apiconditions` with one-line re-exports per version, and `check-v2-api-sync.sh` gates every shared v2 file.
Getting this wrong is the most likely way to break the hop.

### Decided: `v2` omits `CiliumFQDN`/`CalicoFQDN`

Q452, decided 2026-09-08, together with the question it turned out to rest on ([v2beta1 retirement](v2beta1-retirement.md)).

The question was whether GA `v2` defines the two deprecated `EgressProxy.spec.egressPolicyMode` aliases.
It was first answered *yes*, on the reasoning that `v2beta1` keeps serving them until `v3.0.0` and every served version must be able to represent every stored object.
The reasoning was sound and the premise was not a fact: **whether `v2.0.0` keeps serving `v2beta1` was a plan choice, not an API-contract requirement**, and the choice was changed.
`v2beta1` is no longer served past `v2.0.0`.

That removes the constraint outright.
The aliases are enum members of `v2alpha1` and `v2beta1`, Phase 3 removes both, and `v2` never defines them, so nothing that survives `v2.0.0` can name one.
The GA enum is `CIDR;FQDN`.

**The aliases are no longer on their own clock.** [Q428](../operations/v1alpha1-deprecation.md#the-ciliumfqdn--calicofqdn-aliases-ride-the-v200-clock) had them removable no earlier than `v3.0.0`, derived from the survival premise above.
They now ride the same `v2.0.0` clock as `v1alpha1`, `v2alpha1` and classic acquisition, and the operator instruction changes with it: migrating stops being optional and becomes part of the `v1`→`v2` migration.

**What this obliges, and it is the whole cost.** The storage migration in step 3 must not meet a stored `EgressProxy` naming an alias, because `v2` cannot represent one: the migration would store the `FQDN` view, and once `v2beta1` is gone nothing restores the pinned backend from the conversion annotation.
A refusing conversion would be worse, because the conversion contract cannot report a single object as absent.
Read from the vendored [`apiextensions/v1` types](../../vendor/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1/types.go) on 2026-09-07: `convertedObjects` "must also have the same size as the input list with the same objects in the same order", so the webhook cannot omit one; `ConversionResponse.Result` is a single `metav1.Status` for the whole request, so it cannot fail one; and on failure `convertedObjects` is "otherwise empty", so the failure takes the whole batch.
`ConversionRequest.Objects` is a list, so one refused object would break `kubectl get egressproxies` at `v2` for the cluster rather than for itself; Q413's review measured exactly that in envtest, for typed and metadata-client `LIST`s alike.
Carrying the alias avoids the failure while `v2beta1` is served and cannot outlive it, which is what makes the pre-upgrade check load-bearing rather than a courtesy; it is tracked as Q1085 alongside the deprecation notice and the admission change that precede it.

**Why the migration is expected to be a no-op in practice.** No chart, overlay or e2e manifest in the tree sets an alias, measured 2026-09-07, and [Q245](q245-fqdn-intent-backend-split.md#migration--compatibility) recorded the only known consumers as tests and docs.
That is a floor rather than a rate, since an external adopter is unknowable for a public project, which is why the check runs rather than being reasoned away.

## Phase 3 — the storage advance and the coupled removals

**Phase 3 opens by marking `v2` the storage version and migrating stored objects**, which 1.9 deliberately did not do; it ships in 1.10, ahead of the removals ([why](#the-storage-advance-needs-a-state-between-two-applies)).
That ordering is the whole reason 1.9 exists, and it is also what makes the alias check in Q1085 load-bearing: the migration is the moment a pool's pinned backend stops being recoverable from the conversion annotation.

`v2.0.0` then executes all three removals announced by [release-1.3.md](archive/release-1.3.md), plus a fourth decided later:

- `v1alpha1` (the `actions-gateway.github.com` group) — Q273, landed
- `v2alpha1` — this plan
- classic acquisition machinery and the transitional `acquisitionProtocol` / `maxListeners` fields — Q264, landed; the field declarations leave with `v2alpha1`
- `v2beta1`, and with it the `CiliumFQDN`/`CalicoFQDN` aliases: decided 2026-09-08, [v2beta1-retirement.md](v2beta1-retirement.md), and announced separately because `release-1.3.md` did not carry it

`v2beta1` is the one removal Rule 4b constrains, and 1.9 is the release that satisfies it: it serves `v2beta1` and `v2` side by side without moving storage, so this tag is free to advance.

They are one bundle because `v2beta1` is already ScaleSet-only: classic acquisition exists solely to serve `v1alpha1` and `v2alpha1` objects, so removing those versions removes classic's only consumer.
Sequencing within the release still matters, since the Q147 dual-read window closes exactly when `v1alpha1` is removed.
Order: storage-migrate first, drop served versions second, then strip the dual-read arms from the `ValidatingAdmissionPolicy` objects and the validating webhook.
Dropping a served version cannot silently un-match a validating webhook: `make webhook-versions-check` fails any rule naming only versions no CRD serves (Q1068), and Q1150 retyped the validators onto `v2` in 1.9, ahead of the removal.

### The storage advance needs a state between two applies

**Found 2026-10-09, scoping this release.** The order above cannot be one CustomResourceDefinition apply.
The apiserver refuses to remove a version from a CRD's `spec.versions` while its `status.storedVersions` still lists it; the `storedVersions` godoc in `k8s.io/apiextensions-apiserver` v0.37.0, as vendored, states the contract.
Every cluster that has run any 1.x release lists `v2beta1` there, because `v2beta1` is the storage version today, and a cluster that stored objects before `v2beta1` became storage may list an older version as well.
So a single `v2.0.0` apply that marks `v2` storage and drops the other three versions is rejected on every such cluster, mid-upgrade.

Between those two applies sits a state where `v2` is storage, every version is still served, and Q1086's sweep, `gag-migrate storage-version`, rewrites each object and prunes `storedVersions` to `["v2"]`.
CRDs here are applied server-side from a release asset ([upgrade.md](../operations/upgrade.md#non-breaking-v2alpha1-crds-ship-in-a-separate-opt-in-chart)), so that state has to be something an operator applies.
There were two places it could live, and the maintainer chose a release on 2026-10-09 ([release-ladder.md](release-ladder.md#why-110-exists-the-storage-advance-cannot-share-an-apply-with-the-removals)):

- **A `1.10` minor**, chosen.
  It marks `v2` storage, ships the sweep, and keeps every version served; `v2.0.0` is then removals only, a single apply.
  Rollback from `1.10` reaches `v1.9.0`, which serves `v2` and so reads what `1.10` stored.
- **A two-phase `v2.0.0` upgrade.** The release would ship two CRD bundles, and its upgrade would be three steps: the storage-advance apply, the sweep, then the final apply.
  No extra release, at the cost of the one breaking upgrade becoming a procedure an operator can stop halfway through.

Rule 4b does not decide it: `v1.9.0` already served `v2` beside `v2beta1`, so the advance is free to land in either place.

### Capability parity is a precondition of the removal

Removing classic must not delete a capability along with it.
Dropping a served API version is a contract change operators can plan for; silently losing a behaviour they rely on is not, and it is exactly what the deprecation policy exists to prevent.

| Capability | State | Gate |
|---|---|---|
| Eviction recovery (detect an evicted worker, rerun the job, per-run retry budget) | ✅ **Both tiers.** Q417 ported it: the scale-set assignment's run identity is stamped on the worker pod, the owning reconciler detects `PodFailed`/`Evicted` and claims the pod set-once before calling `rerun-failed-jobs`, and the Q106 per-run budget is shared across tiers. | Cleared. Design: [04-operational-flows.md § On the scale-set tier](../design/04-operational-flows.md#on-the-scale-set-tier-q417). Plan: [scaleset-eviction-recovery.md](scaleset-eviction-recovery.md). |
| Pre-claim quota gate (refuse work the namespace `ResourceQuota` cannot place, rather than claim it and stall) | ✅ **Both tiers.** Q443 ported it: the ladder `Provisioner.Admit` walks per delivered job is also expressed as an integer (`AdvertiseCapacity`), and the scale-set tier advertises `min(ceiling, own in-flight pods + quota headroom)` as `X-ScaleSetMaxCapacity` — so a quota-blocked job is never assigned at all. | Cleared. Design: [04-operational-flows.md § The ladder as an integer](../design/04-operational-flows.md#the-ladder-as-an-integer-scale-set-tier-q443). Plan: [capacity-aware-intake.md §9a](capacity-aware-intake.md#9a-the-shipped-quota-rung-was-classic-only-q443). |
| Abandoned-run force-cancel and automatic re-run (a worker removed before it ran) | ✅ **Both tiers.** Q683 and Q691 shipped classic-only inside 1.4 and Q766 ported both in the same release, before any tag published the asymmetry. The scale-set detection reads the run identity from the worker pod's annotations where classic reads it from the payload it holds, which is what the `tier` label on `abandoned_run_force_cancels_total` splits. | Cleared. Design: [04-operational-flows.md § On the scale-set tier](../design/04-operational-flows.md#on-the-scale-set-tier-q766). Plan: [release-1.4.md](archive/release-1.4.md). |
| Job duration and pod-creation latency (`job_duration_seconds`, `pod_creation_latency_seconds`) | ✅ **Both tiers.** Both are observed off the shared pod informer, which sees a scale-set worker pod and a classic one identically, so neither depends on the waiter a fire-and-forget provision never registers. `job_duration_seconds` is worker pod lifetime on both tiers, one span rather than one per tier. | **Cleared** by Q713 (2026-08-11). |
| Restart-safe disruption recovery (a preempted or drained worker whose AGC was down for the teardown) | ✅ **Both tiers.** Q844 ported it: the classic provisioning goroutine reads the disruption off the resolving event it already watches, while a scale-set worker is readable only while it terminates, so an AGC down for that window listed no pod and issued no re-run. The listener now persists the run identity behind every worker it builds in the per-`RunnerSet` guard ConfigMap, and the reconciler re-runs any whose pod is gone at startup, reported as `eviction_retries_total{cause="vanished"}` because which disruption took the worker went with the pod. | Cleared. Design: [04-operational-flows.md](../design/04-operational-flows.md#why-preemption-deletes-rather-than-evicts-and-what-that-costs-us). Plan: [archive/q844-owed-rerun-tombstone.md](archive/q844-owed-rerun-tombstone.md). |
| Poll-error rate observability (`message_poll_errors_total`) | ✅ **Both tiers.** Q446 closed the counter half: `handlePollError` increments the same `actions_gateway_message_poll_errors_total{namespace, reason}` series the classic listener writes, under the same reason vocabulary (`rate_limited`, `timeout`, `other`), with the 401/403 and 404/410 heal branches counting nothing exactly as classic does — so an existing dashboard or alert keeps its meaning. The conditions (`Degraded`/`Unauthorized`, `RateLimited` after a sustained episode) stay as the state half. | Cleared. Metric: [observability-metrics.md](../operations/observability-metrics.md). |

### What this audit checked, and found already covered

Recorded so the cut does not re-derive it.
Method: walk the tier seams — the `ScaleSet` early return in `runnerset_controller.go`, `provision()` versus `ProvisionScaleSetWorker`, and the `listener/` versus `scalesetlistener/` packages — then cross-check every capability the README, roadmap, and why-gag present as a property of the system.

**Confirmed on both tiers** (wired before the protocol route, or ported): worker-capacity conditions `WorkerQuotaPressure`/`WorkerQuotaExceeded`/ `WorkersUnschedulable` (Q303, explicitly "identical to the classic path"), the opt-in scale-up rate limit `spec.scaleUp`, the measured sizing recommendation and `SizingDrift` condition (Q359), the worker-pod reaper including `orphaned_running` (Q420), and eviction recovery (Q417).

**Correctly absent from the scale-set tier** — artifacts of the many-acquirers and JIT-agent models that `ScaleSet` removes by construction, so they should disappear *with* classic rather than be ported: `jobs_duplicate_delivery_total`, `abandoned_delivery_completions_total`, `fanout_loser_recycle_deferred_total`, `agent_recycles_total`, `agent_recycle_errors_total`, `broker_token_propagation_retries_total`, `broker_session_leaks_total`, `renew_job_errors_total`, and `renew_job_teardowns_total`.
Each measures a race or a repair that only exists because many sessions acquire against one pool, or a call the AGC only makes on the classic tier: a scale-set runner renews and completes its own job, so there is no `renewjob` for the gateway to fail at.

The last two joined the list from the Q776 re-walk below.
The two are reconciled by that gate in both directions, so a name here that the ledger does not call classic-only fails.

**Alerting has its analog:** `job_acquisition_errors_total` is classic-only, and [observability-alerting.md](../operations/observability-alerting.md) ships `actions_gateway:scaleset_provision_success_rate:rate5m` in place of the classic `job_acquisition_success_rate`, which Q264 removed with the series it read.
`active_sessions` is likewise classic-only, and `ActionsGatewayScaleSetNoSession` reads its substitute, `scaleset_session_open` (Q1275).

Both were genuine gates, not nice-to-haves: eviction recovery and the pre-claim quota gate are headline capabilities in [01-executive-summary.md](../design/01-executive-summary.md), [README.md](../../README.md), and [why-gag.md](../why-gag.md), all of which describe them as properties of the system rather than of one acquisition tier.
Removing classic before Q417 and Q443 landed would have made those claims false at the same moment the only tier that satisfied them disappeared.
With both ports in, the claims survive the cut.

The two were found the same way and a day apart, which is the reusable lesson: a capability wired into a call site the default tier never reaches reads as shipped from every angle except the one that matters — the tier tenants actually run.
A claim about what GAG does is not verified until it names the acquisition tier it was verified on.

One residual is worth knowing about but does not gate the removal, because classic shares it: a worker pod force-deleted with no grace period (or lost with its node) leaves no `Failed`/`Evicted` object and no chance for the runner to report, so neither tier recovers it.
Q435 measured the adjacent orphan-reclaim question and [Q438](archive/q438-worker-lifetime-deadline.md) closed its residual with a provision-time worker lifetime cap.

Any further capability found to be classic-only before the cut joins this table and gates the same removal.

**Four joined it after the audit, which is the rule working rather than failing.** The abandoned-run recovery opened and closed inside 1.4: Q683 and Q691 shipped classic-only, Q766 ported them, and no tag ever published the asymmetry.
Q713 was the third and the longest-lived: the duration and latency series were the one capability with no scale-set analog to substitute, so it took a port rather than a caveat, and it closed 2026-08-11 by moving both observations onto the shared pod informer.
Q844 was the fourth, on 2026-08-14: restart-safe disruption recovery was classic-only because a scale-set worker is readable only while it terminates, and the ScaleSet tier now persists the run behind every worker it builds.
Read the table, not this section's history, for the state of the gate.

The audit's method has a known blind spot both instances share.
It walked the tier seams once, in July, and a capability added to `provision()` afterwards is classic-only from birth without anything re-walking them.
Q683, Q691 and Q713 all arrived that way, and Q844 made four on 2026-08-14, found by hand rather than by any gate.

**The metric surface re-walked itself until classic left.** Q776 closed that half with a ledger giving every `actions_gateway_*` series a tier, and Q851 took it inside the series to the label values; Q1273 retired both once one tier remained.
The re-walk it required found two: `renew_job_errors_total` and `renew_job_teardowns_total` were classic-only by construction and on no list, and `eviction_recovery_evidence_lost_total` reached no operator doc at all.

That gate covers metrics, not capabilities, so the manual step survives for anything with no series behind it: Q844 had one only because the recovery it ported reports through a counter that already spanned both tiers.
Q774 now gates scope statements mechanically as `make design-scope-check`, which fires on an admission rejection or a changed default added under `docs/design/` with no operator doc touched; adding a row to the table above is still a manual step in the change that creates the asymmetry, and the [doc-update matrix](../development/doc-update-matrix.md) is where that obligation is written down.

## Phase 4 — docs and the cut

Operator-facing surface that must be current before the tag:

- [v1alpha1-deprecation.md](../operations/v1alpha1-deprecation.md) — becomes a historical record rather than an active notice.
- [migration-v1-to-v2.md](../operations/migration-v1-to-v2.md) — the pre-upgrade migration becomes mandatory rather than at-your-convenience.
- [upgrade.md](../operations/upgrade.md) — the upgrade-past-removal path.
- [roadmap.md](../roadmap.md) — the graduation line stops being forward-looking.

## Guardrails

- **A soak reset is not a failure.** If Phase 1 surfaces a shape problem, fixing it in beta with a migration path is the system working.
  Shipping GA over a known shape problem to hold a date is the failure.
- **Deprecation is not removal.** `v2alpha1` stays fully served between `v1.3.0` and `v2.0.0`.
  Nothing is forced on any operator before the major tag.
- **`v2.0.0` is the only breaking release.** Everything between it and `v1.3.0` stays backward-compatible, so operators get exactly one migration event.
