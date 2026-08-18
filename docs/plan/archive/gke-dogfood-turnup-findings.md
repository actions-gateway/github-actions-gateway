# GKE dogfood — turn-up findings and validation history (archived)

Archived turn-up history for the [GKE dogfood runbook](../gke-dogfood.md).
This is the chronological validation log — the `v1.1.0-rc.6` turn-up, the eight classic-protocol fan-out re-routes that isolated the Q224 distinct-delivery starvation, the Q264 P4 ScaleSet clean-green close-out, and the root-cause write-ups for Q246/Q247/Q254/Q259/Q260/Q265/Q266/Q267.
It was split out of the runbook (Q336) so the operational steps stay lean; nothing here is open work.
The runbook's status header carries the current-state summary.

> **Status: complete (2026-07-07).** Every finding below is resolved.
> See the [runbook](../gke-dogfood.md) for the live operational reference.

## Turn-up validation timeline

**Validated on `v1.1.0-rc.6` (2026-07-01).** Control plane (GMC + AGC roll to rc.6, gateway `Ready=True`, App-Secret credential path, Q229 egress-DNS token fetch, baseline listener online — the multiplexer keeps **one** idle listener and scales up to `maxListeners` on job demand, so a single online runner at rest is healthy, not stuck), **production CI routing** (`GAG_RUNNER` → `["self-hosted","linux","gag-ci"]`; a `gh run rerun` dispatched its job to `gag-ci`, the runner went busy, the `workers` spot pool autoscaled `0 → 1`), and **Q235 worker-wrapper injection**: with the `RunnerTemplate` runner container named but image-less, the AGC gap-filled the bare upstream `ghcr.io/actions/actions-runner` (Q233), injected `ghcr.io/actions-gateway/wrapper:v1.1.0-rc.6` as a read-only OCI image volume at `/opt/actions-gateway` (native image volume, no initContainer), and set the container command to `/opt/actions-gateway/wrapper`. rc.6's headline delta over rc.5 is the **Q247 job-renewal fix**, live-validated here: the full privileged-DinD e2e ran green end-to-end on GAG (jobs renewed with the acquire response's job-scoped token by `RunnerRequestID`, with bounded `RenewJob` calls so a hung renewal can't wedge the loop). rc.6 also carries the Q242 G.1 egress destination allowlist, a no-op for this direct-egress dogfood.

**Production CI green — per-job yes, concurrent matrix blocked on Q259 (2026-07-01).** A same-day turn-up routed the real repo's CI to `gag-ci` (`GAG_RUNNER → ["self-hosted","linux","gag-ci"]`) and confirmed **every** migrated job — `vendor-check`, `tidy-check`, `unit-test` (`-race`), `coverage`, `integration-test`, `lint`, `shellcheck` — runs **green** on `gag-ci` when given a worker (verified via single-job reruns).
**Q246 held** (the `dogfood-workload` NetworkPolicy carried 7340 GitHub CIDR egress peers throughout, never blanked; no release-asset download timeout — shellcheck's release tarball and setup-go's toolchain both fetched fine) and **Q247 held for jobs that run** (`integration-test`, ~12 min, renewed its lock and completed green; RunnerSet recovered to baseline with no orphaned pods).
**But the *concurrent* full matrix does not go green:** bursting all jobs onto `gag-ci` at once serializes to ~1 worker even with ample node room (nodes at 35%/9% CPU, zero Pending pods after lowering worker CPU requests 2→1 and pre-scaling the `workers` pool).
Under the burst the AGC agent-pool cannot recycle consumed runners (GitHub `422 "Runner … is currently running a job and cannot be deleted"`), so online listeners are not replenished, GitHub dispatches ~1 job at a time, and the queued jobs hit GitHub's ~15-min unstarted-job timeout (cancelled) while a stuck job's token is invalidated (`RenewJob 401 "Not authorized for this job"` → 600s death).
Root cause is an **AGC concurrency / agent-pool recycling issue under burst load** (Q247/Q249/Q254 family) — **not** node capacity (Q248) and **not** a Q242/Q246 defect — tracked as **Q259**.
One earlier run also hit a transient **spot-VM preemption** (the `workers` pool is spot).
Evidence: runs `28513106734` (unit-test.yml) and `28510907609` (integration-test.yml).
Until Q259 is fixed, Q224's "route production CI green" is **not** met, so Q224 and Q242 stay open.

**Q259 root cause + fix (code-fixed 2026-07-01; live re-validation pending).** Traced end-to-end: after a single-use JIT runner completes a job, GitHub auto-removes the ephemeral record but for a few to tens of seconds still answers a delete with `422 "Runner … is currently running a job and cannot be deleted"`, and — because the AGC re-registers under a **stable name** (Q114) — the lingering record makes the re-registration `409`.
`Pool.Recycle`'s 409-resolution deregister then hit the same `422` and returned it as a **fatal** error, so the post-job recycle failed, the listener goroutine exited, and the Multiplexer does **not** restart a non-permanent replacement — every completed job permanently dropped a polling slot until only the permanent baseline remained, collapsing GitHub dispatch to ~1 online runner.
Under a burst all agents hit this window at once.
**Fix (`cmd/agc/internal/agentpool`):** a typed `RunnerBusyError` for the transient `422`, and a **bounded, jittered backoff** in `Pool.Recycle` that waits for GitHub to release the just-consumed runner before re-registering (ctx-cancellable; on give-up the existing `actions_gateway_agent_recycle_errors_total` fires).
Q114 single-use + stable-name and secure-by-default are preserved (`generate-jitconfig` 409s *before* minting, so retries orphan nothing).
Unit + listener-suite regression tests cover the retry-through and bounded-give-up paths.
**The live symptom only reproduced under real burst, so end-to-end confirmation is deferred to the next dogfood turn-up — Q224/Q242 are NOT yet unblocked/closed.**

**Q259 fix live-validated present, but concurrent matrix STILL wedges — new root cause Q260 (2026-07-03).** The next turn-up deployed the Q259 fix to the AGC (image `ghcr.io/actions-gateway/agc:e2e-2310a31`, built from `main`@`2310a31` = #500; GMC/proxy/wrapper stayed `v1.1.0-rc.6`) and re-routed the matrix.
The Q259 fix **is** present and behaves as designed — the recycle path now logs `agentpool: recycle of parked consumed agent failed; will retry next reconcile` (the bounded retry) instead of a fatal listener exit.
Individual jobs still run **green** (`lint` completed `success` on `gag-ci`).
**But the concurrent burst does not go green — it wedges the same way, and the dominant cause is NOT the post-job recycle Q259 fixed.** Reproduced across **two independent bursts** (a 7-job unit-test+integration burst, then a 6-job unit-test burst against an already-warm pool — so not a cold-start artifact): at burst start **multiple listener agents acquire and try to provision the *identical* job**.
The AGC logs 5 distinct sessions (`agentIndex` 1–5, 5 different `sessionId`s) all failing `provisioner: create Secret job-<jobid>-<suffix>: secrets "…" already exists` for the **same** worker Secret name (e.g.
`job-d03513f7-aa20-416c-a037-197a4a4c9d06-980b169`).
One agent wins and runs the job; the other 4–5 burn runner slots (GitHub shows them `busy` but `offline`, no worker pod) and their sessions die.
Net effect: **only ~1 worker pod ever runs**, the remaining jobs are stranded `in_progress` (assigned to the now-dead duplicate runners) until GitHub's ~15-min unstarted-job timeout, and the pool collapses to `activeSessions=1` (baseline listener).
The Q259 `422 "…still running a job and cannot be deleted"` recycle churn is also still present (e.g.
`runner id 1828`), but it is now the *secondary* symptom — the primary wedge is **duplicate job acquisition under burst** (multiple `AcquireJob`/provision on one job message), distinct from the post-job recycle Q259 addressed.
**Not capacity:** worker nodes were pre-scaled (`workers` pool → 3 spot `e2-standard-4`; the `SSD_TOTAL_GB` regional quota of 500 GB caps pre-scaling at ~3–4 workers — see Q248), zero Pending worker pods from capacity.
Tracked as **Q260**.
**Q224's "route production CI green" is still not met, so Q224 and Q242 remain open/blocked.** Evidence: AGC logs (`agc:e2e-2310a31`), reruns of unit-test.yml `28671804298` + integration-test.yml `28671804300`, and unit-test.yml `28547170012` (2nd burst).

**Q260 fix (code-complete 2026-07-03; live re-validation pending).** The AGC now deduplicates a job across the sibling listener sessions of one RunnerGroup **before** `AcquireJob`.
The Multiplexer owns a per-group in-flight claim registry keyed by the job's `RunnerRequestID` (present in the broker message pre-acquire); `handleJob` claims the id before acquiring, and a sibling handed the same fan-out delivery finds it already claimed and **skips the acquire entirely** — so its runner stays online and idle instead of going `busy` but pod-less, and no two sessions ever reach the colliding per-job worker Secret.
The claim is released when the job finishes (or the acquire is abandoned), so a later GitHub redelivery is still provisionable.
A new counter `actions_gateway_jobs_duplicate_delivery_total{namespace,runner_group}` records each deduplicated delivery (steady low rate under bursts = the gate working).
The dedup runs before the Q59 admission gate, so a duplicate costs neither a capacity slot nor an acquire.
Regression tests: a single-listener gate test (`TestListener_DuplicateJobDeliverySkipsAcquire`) and a Multiplexer concurrency test (`TestMultiplexer_DuplicateJobDeliveryProvisionsOnce`) that fails without the fix (all 5 sibling sessions provision one job; peak-concurrent-provisions = 5) and passes with it (= 1).
**The wedge only reproduces under a real burst, so end-to-end confirmation is deferred to the next dogfood turn-up — Q224/Q242 are NOT yet unblocked/closed.** The Q259 `422 "still running"` recycle churn is a separate, secondary symptom and is unaffected by this fix.

**Q260 fix live-validated INEFFECTIVE — the concurrent matrix STILL wedges (2026-07-03, re-route #2).** A fresh AGC image built off `main`@`c850764` (=#503, the Q260 dedup fix) — `ghcr.io/actions-gateway/agc:e2e-c850764` (digest `sha256:989644a114e39f98108125a2ed4157aec8a8b4611abd68f6e84d747745efcc19`), GMC/proxy/wrapper unchanged at `v1.1.0-rc.6` — was deployed by patching the GMC's `AGC_IMAGE` env; the CI AGC rolled to it (verified: running pod's `imageID` matches the pushed digest), gateway `Ready=True`, baseline listener online.
The concurrent matrix was re-routed (`GAG_RUNNER → ["self-hosted","linux","gag-ci"]`) and a 7-job burst fired (rerun of unit-test.yml `28678275088` = 6 jobs + rerun of integration-test.yml `28678275106` = 1 job).
Worker capacity was **not** the constraint: `workers` pool pre-scaled to 3 `e2-standard-4`, worker CPU requests lowered `2→1`, zero Pending worker pods.
**The burst wedged exactly as before.** At burst start `activeSessions` scaled up to 8 (`maxListeners`) as designed, but then **5 distinct sibling sessions** (`agentIndex` 2–6, 5 different `sessionId`s) all failed `provisioner: create Secret job-3e6a971f-62ec-4bba-bdd5-b928ba9e63f7-9a91092: secrets "…" already exists` on the **identical** worker Secret — i.e. 6 sessions raced the *same* job (1 won, 5 burned their runner slot).
The pool then collapsed `activeSessions 8 → 2`; GitHub showed 5 runners `busy:true` but `status:offline` (ci-2…ci-6) with **no** worker pod; only ~1–2 worker pods ever ran; and `unit-test`/`vendor-check`/`tidy-check`/ `coverage`/`lint` stranded `in_progress` on the dead runners.
The Q259 `422 "…still running a job and cannot be deleted"` recycle churn (runner ids 1884/1886/1887) and the Q254 `RenewJob: job lock definitively lost` (`job_not_found` 404, cancelling the winning worker) both reappeared as secondary symptoms.

**Root cause — the Q260 dedup keys on the wrong identifier.** The Multiplexer's in-flight claim registry keys on `RunnerRequestID` and claims it **pre-**`AcquireJob` ([`goroutine.go:570`](../../../cmd/agc/internal/listener/goroutine.go)).
But the colliding per-job worker Secret is named from the job's **`planID`** (`resp.Plan.PlanID`, from the AcquireJob **response**), and the pre-acquire broker message ([`RunnerJobRequestBody`](../../../broker/types.go), fields `RunnerRequestID` + `RunServiceURL` + `BillingOwnerID`) carries **no** plan id.
GitHub's broker fan-out delivers one job (one `planID`) to sibling sessions as messages with **distinct** `RunnerRequestID`s — so each sibling's `claimJob(distinctReqID)` succeeds, all pass the gate, all acquire, and all collide on the shared `planID` Secret.
Since the claim registry is shared across siblings (`cfg.ClaimJob = m.claimJob`), 5 sessions passing the gate proves their `RunnerRequestID`s differed; and `RunnerRequestID` is non-empty (RenewJob keys on it and single jobs renew fine), ruling out the empty-key path.
The fix's model — "same delivery ⇒ same `RunnerRequestID`" — does not hold in production; the regression test `TestMultiplexer_DuplicateJobDeliveryProvisionsOnce` feeds one **shared** `RunnerRequestID` to all 5 sessions, so it passes green while the live broker assigns per-delivery ids and the wedge survives.
**A working dedup must key on job identity that (a) collapses across fan-out siblings and (b) determines the Secret** — i.e. `planID`, which is only known *post*-acquire.
Candidate fixes for the Q260 follow-up: claim on `planID` immediately after `AcquireJob` but before provisioning, releasing the acquire + deregistering the runner cleanly on a lost claim (so the slot isn't left `busy`/offline); or investigate whether the per-job `RunServiceURL` (documented "per-job … must not be cached globally across jobs") is stable across siblings and usable as the pre-acquire dedup key.
**Q224's "route production CI green" is still NOT met — Q224 and Q242 remain open/blocked, and Q260 is reopened (its first fix is ineffective).**

**Q260 re-fix (code-complete 2026-07-03; live re-validation pending).** The dedup is re-keyed from `RunnerRequestID` to **`planID`** and moved from pre-`AcquireJob` to **post-acquire, pre-provision** ([`goroutine.go`](../../../cmd/agc/internal/listener/goroutine.go) handleJob; the Multiplexer's shared claim registry now holds planIDs).
Because planID is only known post-acquire, a losing sibling still acquires, then finds the planID already claimed and **skips provisioning**, returning `acquired=true` so its consumed single-use runner is recycled back online (slot reclaimed cleanly) — no collision on the `job-<planID>` Secret, no burned slot.
The pre-acquire RunnerRequestID gate is **removed** (it never fired in production — siblings' ids differ — and the planID gate subsumes its only correct case, since any two deliveries that would collide on the Secret share a planID).
`actions_gateway_jobs_duplicate_delivery_total` is retained, now counting a post-acquire planID-claim skip.
Regression tests re-key onto the live shape (distinct RunnerRequestIDs, one shared planID) and were **verified to fail against the c850764 behaviour**: the reworked Multiplexer unit test (`TestMultiplexer_DuplicateJobDeliveryProvisionsOnce`, peak-provisions 1 vs 5), the single-listener gate test (`TestListener_DuplicateJobDeliverySkipsProvisioning`, now asserts the loser *does* acquire but does not provision and keys on `plan-stub`), and a new **envtest** integration test (`TestAGC_Q260_DuplicateDeliveryDedupsOnPlanID`) that drives the real provisioner + API server: one session wins and holds the planID claim while a distinct-RunnerRequestID sibling is deduped rather than hitting the real Secret `AlreadyExists`.
**The wedge only reproduces under a real burst, so end-to-end confirmation is still deferred to the next dogfood turn-up — Q224/Q242 stay open/blocked and Q260 stays open until then.** Plan: [`q260-planid-dedup-refix.md`](q260-planid-dedup-refix.md).

**Q260 planID dedup live-validated EFFECTIVE — the burst-start collapse does NOT recur; but the matrix still isn't fully green, now blocked by capacity + a late-redelivery edge (2026-07-04, re-route #3).** A fresh AGC image built off `main`@`1f4111b` (=#508, the planID re-fix) — `ghcr.io/actions-gateway/agc:e2e-1f4111b` (manifest-list digest `sha256:b0848e970e0fca62d0b649fa5620467580914d79e21e04c24ddcd16171be40dd`, amd64 manifest `sha256:03bc3ee2…`), GMC/proxy/wrapper unchanged at `v1.1.0-rc.6` — was deployed by patching the GMC's `AGC_IMAGE` env; the CI AGC rolled to it (verified: the running pod's `imageID` matches the pushed digest), gateway `Ready=True`, baseline listener online.
The dogfood `RunnerTemplate` was re-pinned to the build-capable `ghcr.io/actions-gateway/dogfood-runner:2.335.1` (Q239, avoids `make: command not found`), worker CPU request already `1`, `default-pool→2`, `workers` pre-scaled `→3`, and `spec.logLevel: debug` set on the CR so the dedup skip (a Debug line) is observable.
The SAME comparable 7-job burst as #505 was fired at `23:45:33Z` — reruns of the two completed `main` runs **on the exact deployed commit `1f4111b`**: unit-test.yml `28687585802` (6 gag-ci jobs) + integration-test.yml `28687585839` (1 job) — after flipping `GAG_RUNNER → ["self-hosted","linux","gag-ci"]`.

**The Q260-specific wedge is gone.** The prior turn-ups' signature — 5 sibling sessions **simultaneously** colliding on the shared `job-<planID>` **Secret** at burst start → instant collapse to `activeSessions 1-2` → nothing completes — did **not** occur:
- **Dedup fired on the shared planID:** `duplicate_delivery` gate skipped **2** deliveries, both for planID `b8321da3` (agentIndex 1 @`23:47:45` and agentIndex 3 @`23:51:28`, with **distinct** `RunnerRequestID`s) — exactly the fan-out the old pre-acquire RunnerRequestID key missed.
  The planID key collapses it, as designed.
- **Zero Secret collisions at burst start** (was 5): 5 `job-<planID>` Secrets created cleanly; `activeSessions` scaled up to **4 concurrent busy runners** (ci-0..3, = `maxWorkers`), holding rather than collapsing.
- **Two full CI jobs completed GREEN under the burst** — `coverage` and `integration-test` both `success` on `gag-ci` (a first for these turn-ups; prior wedges completed ~nothing).

**But the concurrent matrix still does NOT go fully green.** Final tally: **2 success** (coverage, integration-test), **2 cancelled** (tidy-check, vendor-check), **1 failure** (unit-test), 2 (shellcheck, lint) still in-progress at teardown.
The residual blockers are **distinct from the Q260 dedup wedge**:
1. **Capacity starvation (Q248) — dominant.** The pre-scaled 3 spot `workers` nodes were **preempted** mid-burst down to **1** node (spot; node set `cd55/hsms/l5w6 → gwzz`, autoscaler reported "1 in backoff after failed scale-up"; `SSD_TOTAL_GB=500` caps the pool at ~3-4 regardless).
   With ~1 concurrent worker slot the 7 jobs serialized: `unit-test` died at **exactly 600s** (the initial AcquireJob lock TTL) having run **zero steps** because its assigned runner never got a pod; `tidy-check`/`vendor-check` were cancelled at **exactly 15:00** (GitHub's unstarted-job timeout).
   This is capacity, not Q260.
2. **Late-redelivery Pod-collision (new Q260 follow-up).** The **2** collisions that *did* occur were on **`create Pod`** (not Secret) and **late** (`23:59:18`/`:19`), both for the single **slow** planID `b8321da3`: GitHub redelivered that one job repeatedly over ~12 min (`23:47`→`23:59`); the planID claim is released when the winner completes, so a post-completion redelivery passes the gate and collides on the winner's **not-yet-GC'd Completed pod**.
   That winner pod **ran the job to completion** ("Raising job completed against run service" / "Job completed" @`23:59:14`, **no** renewal/lock errors) — yet GitHub still **cancelled** `tidy-check` at the 15-min unstarted-timeout, a completion-vs-timeout **accounting gap** under fan-out.
   Milder than the Secret-collapse (the job already ran) but it still burns runner slots and yields a cancelled job.
   The Q259 `422 "…still running a job and cannot be deleted"` recycle churn (4 listener exit-on-recycle events) is present as before — unchanged secondary symptom.

**Verdict.** The planID stable-key model is **correct** — the dedup fired on the shared planID and prevented the burst-start Secret-collision collapse — so do **not** hunt for a different dedup key.
But **Q224's "route production CI green" is still NOT met:** full green is blocked by (1) Q248 spot-node capacity → serialized execution → 600s/15-min timeouts, and (2) the late-redelivery Pod-collision + completion-accounting residual.
**Q224/Q260/Q242 stay open/blocked.** A clean green re-validation needs **stable worker capacity** (non-spot, or ≥3 held nodes) so throughput isn't the confound, plus addressing the redelivery-accounting edge (release the planID claim only after the worker Pod is GC'd, and/or reconcile GitHub's per-delivery job-assignment timeout with the AGC's dedup-to-one-delivery model).
Evidence: AGC debug logs (`agc:e2e-1f4111b`), reruns unit-test.yml `28687585802` + integration-test.yml `28687585839` (both on `sha 1f4111b`), burst `23:45:33Z`–`00:03Z`.

**Q260 redelivery residual — code-complete (2026-07-03; awaits combined re-route #4).** The late-redelivery **Pod-collision** from residual (2) is now **fixed in code**: the AGC Multiplexer's shared `planID` claim registry **retains** a released claim for a linger window sized to the owner's `completedPodTTL` (the exact window the winner's terminal pod lingers before the reaper GCs it), instead of freeing it on completion.
A post-completion redelivery arriving during that window is deduped at the post-acquire `planID` gate — counted on `actions_gateway_jobs_duplicate_delivery_total`, no re-provision, **no `create Pod … already exists`**, no error surfaced as a cancel.
Regression: `TestAGC_Q260_LateRedeliveryAfterCompletionDedups` (envtest) reproduces the exact `create Pod runner-…-<planid>: pods "…" already exists` against the pre-fix behavior and passes with the fix.
The deeper **completion-vs-15-min-cancel accounting gap** (the winner's pod completes yet GitHub cancels the job on a deduped sibling delivery) now has its run-service protocol call — `broker.CompleteJob` + a guarded loser-abandon path, described next — but it stays **off by default** pending live confirmation of the completion semantics.
See [`q260-planid-dedup-refix.md`](q260-planid-dedup-refix.md) Follow-up item 2.
This code lands ahead of the dispatcher's combined **capacity (Q248) + re-route #4** turn-up, which re-validates green on stable worker capacity.
**Q224/Q260/Q242 stay open until then.**

**Follow-up mechanism landed (guarded), pending this turn-up's confirmation.** The completion-accounting residual now has a code path: the deduped loser can release its acquired-but-unrun assignment via `completejob` on its own `jobID` (result `skipped`), so GitHub does not cancel the job at the 15-min unstarted-timeout.
It is **off by default** (`AGC_COMPLETE_ABANDONED_DELIVERIES=true`) because the run service's per-delivery *completion* semantics are not yet live-confirmed.
**Next turn-up: enable the flag via the existing `AGC_EXTRA_*` passthrough — set `AGC_EXTRA_AGC_COMPLETE_ABANDONED_DELIVERIES=true` on the GMC pod (GMC run with `--allow-agc-extra-env`), which the GMC forwards verbatim (prefix-stripped) to the AGC Deployment env; no GMC code change needed.** Then re-fire the burst on stable (non-spot) capacity and capture the `completejob` request/response + whether the previously-cancelled job (`tidy-check`) now concludes instead of cancelling.
If completion turns out to be planID-scoped (would cancel the winner), revert the flag and pursue the claim-release-post-GC path instead.
See [`q260-planid-dedup-refix.md`](q260-planid-dedup-refix.md) follow-up item 2.

**Combined capacity fix + flag-on/flag-off comparison — capacity & collisions FIXED, but still NOT green; the blocker is now GitHub's broker fan-out completion/assignment accounting (2026-07-04, re-route #4).** Ran the same ~7-job concurrent matrix twice on **stable, non-preemptible** worker capacity with a fresh AGC built off `main`@`4602429` (= HEAD, includes #512 late-redelivery claim linger and #513 guarded completejob-abandon) — `ghcr.io/actions-gateway/agc:e2e-4602429` (amd64 digest `sha256:55a88007…`), GMC/proxy/wrapper `v1.1.0-rc.6`, `spec.logLevel: debug`, `RunnerTemplate` pinned to `dogfood-runner:2.335.1` (Q239), worker CPU request `1`.
The AGC pod's `imageID` matched the pushed digest; gateway `Ready=True`.

**Capacity (Q248 residual) — FIXED.** Replaced the spot `workers` pool (which preempted `3 → 1` mid-burst in #3) with a **non-preemptible `workers-od` pool** (`e2-standard-4 ×3`, on-demand, taint `dedicated=workers:NoSchedule`; spot `workers` scaled to `0`, `default-pool → 2`).
SSD math: `3×100 + 2×50 + 20 = 420 GB < 500` (`SSD_TOTAL_GB` quota; disks are `pd-balanced`, which counts against it).
Result: **3 `workers-od` nodes stayed Ready across all 58 monitor samples — zero preemption, zero spot nodes**; peak node utilization **34 % CPU / 27 % mem**; peak per-pod memory **~3.8 GiB** (under the 8 GiB limit, no OOM); peak `activeSessions` **5**.
So the #3 failure mode (preemption → serialized jobs → 600 s lock-TTL / 15-min unstarted cancels *from capacity starvation*) **did not recur**.
`workers-od` fixed at `min=max=3` to stay under quota; 4 concurrent worker pods fit comfortably at CPU request `1`.

**#512 dedup — FIXED (again), 0 collisions in both bursts.** Each burst fanned out 3 planIDs with **5 sibling redeliveries each** (distinct `RunnerRequestID`s); the post-acquire planID gate deduped all of them (**10 dedup events per burst**) with **zero `create Secret … already exists` and zero `create Pod … already exists`**.
The planID key and the claim-linger are working as designed.

**Burst #4a — flag ON** (`AGC_COMPLETE_ABANDONED_DELIVERIES=true`, forwarded via the GMC `AGC_EXTRA_*` passthrough with `--allow-agc-extra-env`): reruns `28694212343` (6 gag-ci jobs) + `28694212356` (integration) at `04:11:48Z`.
The #513 path was **exercised**: **15 `completejob` calls — 14 returned OK, 1 returned `401 "Not authorized for this job"`** (planID `eba8f94d`; its winner pod kept running, so the 401 did **not** finalize the winner — a per-delivery auth edge, not the feared planID-scoped regression).
**Outcome: 2/7 green** (`coverage` on ci-1, `integration-test`); **5/7 wedged INDEFINITELY `in_progress`** (`tidy-check`, `shellcheck`, `unit-test`, `vendor-check`, `lint`) under the replacement session `ci-2`.
Their winner pods **ran** (6 Succeeded, 1 Failed) — yet GitHub never concluded the jobs (confirmed via the Checks API, not just the runs API, whose aggregate froze at a stale `completed/success` when `coverage` finished).
**The `completejob(result=skipped)` call returns OK but does not transition the job to completed** — it merely acks that one delivery, so a late redelivery re-assigns the already-run job to `ci-2` and GitHub holds it `in_progress`.
Worse, by acking the delivery it **suppresses the 15-min unstarted-timeout that would otherwise resolve the job**, yielding an *indefinite* limbo.

**Burst #4b — flag OFF** (control; the *only* difference from #4a is the completejob path): reruns `28693708850` + `28693708839` at `05:00:10Z`.
AGC logs confirmed the clean control: **10 dedup events, 0 `completejob` calls, 0 collisions**.
**Outcome: 1/7 green** (`integration-test`); `coverage`=**failure**, `unit-test`=**failure**, `vendor-check`=**cancelled**, `shellcheck`=**cancelled**, `lint`/`tidy-check` in_progress → cancel.
Crucially these are **terminal** states, not the indefinite wedge.
The Q259 recycle churn was present **and blocking**: GitHub's fan-out marks `ci-1/2/3` as *"runner … is still running a job and cannot be deleted"* (422), so the AGC cannot recycle those listener slots → the trivial jobs never get a slot and are cancelled at the 15-min unstarted-timeout; the jobs that *did* run concluded `failure` (the completion-accounting mismatch, same class as Q247 but at the assignment level — not a real test failure: identical commits pass on GitHub-hosted and `coverage` passed green in #4a).

**Verdict.** Neither flag state reaches green — the same jobs go `in_progress`-forever (flag on) or `failure`/`cancelled` (flag off).
**Capacity (Q248) and collisions (#512) are both fixed and off the critical path.** The remaining blocker is **GitHub's broker fan-out completion/assignment accounting**: one job is delivered to N sibling listener sessions as independent assignments, and neither the winner's completion nor the losers' `completejob(skipped)` reconciles GitHub's per-delivery view — so runners can't recycle (Q259 422) and jobs don't conclude as success.
This is **distinct from and beyond** the Q260 dedup.
**The #513 flag does not help and makes the end-state worse (indefinite `in_progress` vs terminal cancel/fail) — keep `AGC_COMPLETE_ABANDONED_DELIVERIES` OFF by default (secure-by-default confirmed by live evidence).** `completejob` semantics answered: the run service **accepts** the `skipped` result serialization (14/15 HTTP-OK, so wire format is fine) but does **not** conclude the job on that call; and 1/15 returned `401`, so job-scoped auth for the completion path is not reliable.
**Q224/Q260/Q242 stay open**, now blocked on the fan-out accounting rather than capacity/collisions.
Evidence: AGC debug logs (`agc:e2e-4602429`), flag-on reruns `28694212343`/`28694212356` (burst `04:11:48Z`), flag-off reruns `28693708850`/`28693708839` (burst `05:00:10Z`).

**Re-route #5 — Q260 Option A CONFIRMED (GO); the fan-out accounting gap is closed AGC-side (2026-07-04).** Deployed a fresh `ghcr.io/actions-gateway/agc:e2e-238b8df` (amd64 digest `sha256:611632e7…`, includes #521 winner-driven Option A) via the GMC `AGC_IMAGE` env patch, on the same re-route #4 stable capacity (non-preemptible `workers-od` ×3 + default-pool 2, worker cpu req 1, 5 nodes Ready throughout).
Enabled Option A with **`AGC_EXTRA_AGC_FANOUT_COMPLETION=true`** on the GMC pod (GMC v1.1.0-rc.6 already run with `--allow-agc-extra-env`), which forwards `AGC_FANOUT_COMPLETION=true` to the AGC Deployment — no GMC code change.
`spec.logLevel: debug`.
RunnerTemplate was already pinned to `dogfood-runner:2.335.1` in the persisted CR (Q239 not regressed this time — the toolchain image was present).
Fired the same ~7-job matrix (unit-test `28712011706` + integration `28712011697` reruns on sha `238b8df`, both green on GitHub-hosted; **push** events, so concurrency-immune).

**The one live-only unknown is answered YES.** At `16:37:07Z` a fanned-out job (planID `357b6d9e`, winner on ci-0) whose winner completed **naturally** fanned `completejob` out to **both** deduped siblings (jobIDs `34ad8db4` on ci-2, `f968c752` on ci-4) → **both returned OK** (`completed a deduped sibling delivery via completejob`), **not** "already resolved".
GitHub **accepts** the completion of a sibling delivery that never ran the job.
Cumulative over the burst: **9 `completejob` OK, 0 failures, 2 already-resolved** (siblings whose winners were concurrency-cancelled — see confound), across **13** deduped fan-out deliveries.
Completion is **per-delivery, not planID-scoped**: `completejob` on a sibling's own job ID resolved only that assignment, and the winner's own delivery still carried the real workflow result reported by its runner binary — so the pod-phase proxy on siblings **cannot** green a red workflow.
The secure-by-default concern is cleared; the flag is flipped **on by default** (`AGC_FANOUT_COMPLETION`, opt out `=false`).

**Jobs conclude green and stay green.** `coverage` (16:37:04Z), `unit-test` (`-race`, 16:52:29Z) and `integration-test` all concluded **success** — the previously-wedged class.
Crucially `coverage` stayed `success` **past `16:47Z`**, beyond the ~15-minute unstarted-timeout of its siblings (acquired ~16:31Z) — the exact point re-route #4's winner-completed jobs were cancelled.
**Option A prevented the cancel.**

**Q259 recycle 422 clears per job.** The "runner … is still running a job and cannot be deleted" churn (121 hits in the 6 min before the first winner completed) dropped ~12× once winners began fanning `completejob`; the AGC pool recovered from a collapsed **2 active sessions back to 5** and drained its backlog.
The 422 is a **rolling transient** — each fanned-out job's in-flight siblings 422 until that job's winner completes and resolves them — not the permanent wedge of re-route #4.

**Confound (handled).** A Dependabot rebase merge-train briefly shared the runner pool: its `pull_request` CI runs (SHAs `81b0d30`/`d160ae3`/…) were concurrency-cancelled on each rebase force-push, cancelling in ~4 min — **distinct** from the 15-min accounting timeout, and (because the delivery is torn down first) the reason 2 sibling `completejob`s hit "already resolved".
Attempting to cancel the interfering runs was denied (shared workload).
The clean signal came from the **push**-event `238b8df` reruns, which cannot be concurrency-cancelled.
Their slower jobs (`vendor-check`, `lint`, `tidy-check`, `shellcheck`) ran long on a cold Athens cache and, 38 min in, were still **in_progress** — **not** the Q260 accounting cancel (which did not recur) but a **separate worker-capacity limit**: with `maxWorkers=4` saturated the AGC logged repeated `job admission rejected: worker capacity full` and those jobs cycled through redeliveries without ever landing a worker slot.
So the ~7-job matrix did **not** cleanly sweep all-green in this window; a pristine full-matrix green is gated on worker-capacity tuning (`maxWorkers`, Q248), which is **distinct from and beyond** the Q260 accounting fix confirmed here.

**Verdict: GO (design §5 point 4).** Resolving all sibling deliveries lets a fanned-out job conclude green (3/3 that landed workers concluded — `coverage`, `unit-test` `-race`, `integration-test`), `completejob` on live siblings returns OK (9/9, 0 failures), the job survives past the 15-minute timeout, and the Q259 422 clears per job.
The many-acquirers topology is reconcilable AGC-side; **Option E (Q264) is not needed** and is demoted.
**Q260 DONE; Q224's fan-out blocker cleared** (residual: the `maxWorkers` capacity sweep, Q248).
Evidence: AGC debug logs (`agc:e2e-238b8df`), reruns `28712011706`/`28712011697` (burst `16:24:00Z`), fan-out completion `16:37:07Z`.

**Secondary observation — dogfood RunnerTemplate reverted to the bare upstream image (Q239 regression).** The `shellcheck` job failed `make: command not found` because the CI `RunnerTemplate` runner container is image-less, so the AGC gap-fills the bare upstream `ghcr.io/actions/actions-runner:2.335.1` (no build toolchain) rather than the build-capable `dogfood-runner` image (Q239, validated 2026-06-29).
This blocks green CI independently of Q260: a future turn-up must run `scripts/dogfood/setup.sh` with `DOGFOOD_RUNNER_IMAGE` exported (or patch the `RunnerTemplate` `workerImage`) so `make`-based jobs can pass.
Not a new bug — the cluster lost the Q239 config across a re-setup.

**Re-route #6 — Q265 fan-out throughput benchmark: the completion tax is NOT the wall (2026-07-05).** Built `agc:e2e-cacd4c6` (amd64 `sha256:ec25509…`, HEAD/#523, Option A default-on), deployed via the GMC `AGC_IMAGE` patch with the explicit `AGC_FANOUT_COMPLETION` env **removed** (verifying the *shipped* default — confirmed live).
Same re-route #4/#5 stable capacity (non-preemptible `workers-od` ×3 + default-pool 2; spot `workers` pinned 0/0 to remove the preemption confound).
The Q265 lever: `maxListeners` set **far above** `maxWorkers × fan-out-width` (48, then 16; fan-out ≈ 6) so listener supply could not bottleneck; `maxWorkers = 4` (SSD-bounded).
Fired the same ~7-job matrix, sampling online runners (≈ active sessions), busy workers, worker-pod occupancy, and AGC debug markers every 15 s.

**Both bursts collapsed to ~1 busy worker — but NOT via the tax.** Run 1 (`maxListeners` 48): peak 2 online / **1** worker.
Run 2 (`maxListeners` 16): peak 3 online / **1** worker.
In **neither** run did the pool reach the `maxWorkers = 4` ceiling — `job admission rejected: worker capacity full` fired **0** times, so the `completejob` tax was **never the binding constraint**.
The dominant signal was the **agent-recycle registration-conflict seam** (Q259/Q114): `recycle blocked by still-running consumed runner; backing off and retrying` then `deregister conflicting runner record "ci-N": runner is still running a job and cannot be deleted` → **fatal listener exit** (41 / 38 occurrences).
Option A itself worked (warm-up 5×, run 2 2× `completed a deduped sibling delivery via completejob`, per-delivery).

**Mechanism (fan-out slot-stranding, not `completejob` cost):** a deduped loser's single-use slot is 422-blocked ("assigned to the job") for the **winner's entire job runtime** (minutes), which exceeds the bounded recycle backoff (tens of seconds) — the backoff exhausts, the recycle fails, the listener exits, and each fanned-out job loses F−1 slots faster than winners complete.
A classic-protocol topology cost (single-use recycle + many-acquirers fan-out), fixable AGC-side; **not** a tax wall.

**Honest bounds:** SSD quota caps `maxWorkers` ≈ 4 (can't prove no-wall at a *wide* pool), and 47 stale offline `ci-*` runner records (prior re-routes + this session's `maxListeners` changes) inflated the 409/422 conflict rate — cleaning them (mass runner-record delete) was **denied by the write-safety guard**, so a clean-namespace run was impossible in-session. re-route #5 (cleaner namespace, `maxListeners` 8, longer jobs) *recovered* to 5 sessions — consistent with the collapse being *provoked* by clutter + over-cranked `maxListeners` + short jobs, not purely inherent.
**Verdict:** no completion- tax wall → **Q264 stays deferred**; fix the recycle slot-stranding seam (new Queue item) and re-benchmark on a fresh clean namespace before any Option E reconsideration.
Full analysis + method + results table: [`q260-fanout-completion-reconciliation.md`](q260-fanout-completion-reconciliation.md) §7.
Evidence: AGC debug logs (`agc:e2e-cacd4c6`), reruns `28726094554`/`28726094563` (run 1, `02:52Z`) and `28725801848`/`28725801860` (run 2, `03:00Z`).

**Re-route #7 — Q248 disk right-size (SSD ceiling GONE, no quota bump) + Q224/Q266 re-benchmark (2026-07-05).** Two goals in one turn-up: (1) lift the `maxWorkers ≈ 4` ceiling that re-route #6 attributed to the `SSD_TOTAL_GB = 500` quota, *without* a quota bump; (2) re-benchmark the fan-out matrix at the wider pool now that Q266 (#525) fixed the loser slot-stranding recycle.

**Q248 — disk class was the ceiling; `pd-standard` removes it (FIXED + PROVEN).** The ~4-node cap was **not** a quota shortage: each worker node's 100 GB boot disk was **`pd-balanced`** (SSD-class), which counts against the 500 GB `SSD_TOTAL_GB` quota, so ~4 worker nodes exhausted it.
The CI workload is tiny, so SSD-class worker disks buy nothing.
Recreated `workers-od` as **`e2-standard-4 ×4` with `--disk-type=pd-standard`** (HDD; counts against `DISKS_TOTAL_GB = 4096`, **not** the SSD quota).
Live proof with 4 worker + 2 system nodes up: `DISKS_TOTAL_GB` = **400** (the 4×100 GB worker disks, now HDD), `SSD_TOTAL_GB` = **220/500** (system only — workers no longer counted), `CPUS` = **24/200**.
Under the old `pd-balanced` config those 4 workers would have pushed SSD to ~620 > 500 → blocked.
Worker-node utilization during jobs: **2–5 % CPU / 8–14 % mem** — confirming the SSD-class disk was pure waste.
**`maxWorkers` is now CPU/mem-bound (room for ~48 `e2-standard-4` nodes), not SSD-bound.** Persisted: `scripts/dogfood/setup.sh`
+ the Part A4 recipe above now provision `--disk-type=pd-standard`, `max-nodes=8`, `maxWorkers=8`; details in [`dogfood-runner-rightsizing.md`](../dogfood-runner-rightsizing.md#node-pool-disk-class-the-real-maxworkers-ceiling-q248-2026-07-05).

**Deploy.** Built `agc:e2e-f681d9d` (amd64 `sha256:86aa1b1e…`, HEAD/#525 = Option A default-on + Q266 loser-recycle-defer), deployed via the GMC `AGC_IMAGE` patch with **no** explicit `AGC_FANOUT_COMPLETION` env (verifying the shipped default; imageID matched the pushed digest).
RunnerTemplate already pinned `dogfood-runner:2.335.1` (Q239 not regressed), worker CPU request 1.

**Re-benchmark — Q266's targeted seam is GONE, but a clean "holds at `maxWorkers`" measurement STILL could not be obtained (same confound class as #6).** Fired the ~6-job `unit-test.yml` matrix (push-event reruns of `28731081406`/`28731081446`, sha `f681d9d`).
- **`maxListeners = 48` (the "≫ maxWorkers×fan-out" lever):** the pool **collapsed to online = 0**.
  The AGC registered ~44–48 `ci-*` runner records but kept **zero online** at GitHub; the dominant seam was the **broker-credential recycle** (`broker token exchange rejected … "Registration … was not found"` 400) — the Q259/Q114 registration churn, *worsened* by the wide `maxListeners` multiplying stale records (mass runner-record delete stays **guard-denied**, so no clean namespace).
  *(A premature AGC restart I issued mid-run also orphaned in-flight jobs — my confound, not the code's.)*
- **`maxListeners = 12` (moderate, post-restart, 0 broker rejections):** the pool **held stably — no fatal collapse**.
  Over the wave: **dedup 7, Option A `completejob` 5** (fan-out completion firing), **deduped losers PARK** (busy-at-GitHub but pod-less, the Q266 defer behaviour), **0 `deregister conflicting`/`recycle blocked`/fatal listener exits** (Q265 had 41/38), **0 `worker capacity full`**.
  **But throughput serialized to ~1 concurrent worker pod** (peak busy runners 4, peak concurrent pods 1): at fan-out width ≈ 6, a `maxListeners = 12` pool admits only ≈ 12/6 = **2 concurrent fan-out jobs**, and the AGC held **~0 online *idle* listeners**, so GitHub trickled jobs one fan-out per wave and several jobs stranded `in_progress`/`queued` waiting for a slot.

**Verdict.** **Q248: DONE** (disk ceiling removed, no quota bump).
**Q266: its seam is eliminated** — the fatal deregister-conflict listener exits that collapsed the pool in #6 are **0**; losers park, Option A + dedup work live.
**Q224: NOT green — do not close.** The residual is neither the Q266 slot-stranding seam nor the `completejob` tax (0 capacity rejections) but a **two-way bind in the many-acquirers fan-out**: throughput needs `maxListeners ≈ maxWorkers × fan-out`, yet a wide `maxListeners` multiplies GitHub runner records and inflates the registration/recycle churn that keeps the **online idle pool near 0** — so *neither* a low nor a high `maxListeners` yields a wide, stable, online-and-idle pool.
The un-cleanable stale-record clutter (guard-blocked) compounds it.
A truly clean measurement needs (a) the **online-session / broker-credential recycle seam** fixed (Q259/Q114 family) so listeners actually stay online, and (b) a **clean-namespace** run — both still blocked in-session.
This reinforces (does not force) the [Q264](../q264-scale-set-protocol.md) scale-set case (one acquirer, no fan-out, no per-listener record multiplication), which stays **deferred** — no `completejob`-tax wall was seen.
Evidence: AGC debug logs (`agc:e2e-f681d9d`, digest `86aa1b1e`), reruns `28731081406`/`28731081446` (bursts `06:29Z`/`06:45Z`); quota `DISKS_TOTAL_GB=400`, `SSD_TOTAL_GB=220/500`.

**Follow-up — the online-session/broker-credential recycle seam is now fixed code-side (Q267, 2026-07-05).** Root cause of the `broker token exchange rejected … "Registration … was not found"` (400) churn: after a recycle re-registers a fresh runner record, the immediate OAuth token-exchange for that record's client credential can 400 for a brief `generate-jitconfig` → OAuth-service propagation window.
`recycleAndRestart` treated that transient 400 as **fatal** — the listener exited and churned a new record, and at a wide `maxListeners` the exits multiplied stale records and held the online pool near zero.
`healSession` rides out a token 400 on *stored* creds (via one recycle), but the *fresh*-cred exchange had no retry.
The fix (`refreshBrokerTokenAfterRecycle`) rides out the transient with a bounded, jittered retry of the **same** fresh credential — no re-registration, so no record leak — counted by `actions_gateway_broker_token_propagation_retries_total`.
Proven by an **offline** regression test (`goroutine_q267_test.go`, no cluster needed).
What remains for a clean "holds at `maxWorkers`" measurement is operational, not code: a **live wide-pool re-benchmark** on a **clean namespace** (the guard-blocked mass runner-record delete).
Both are dispatcher-tracked follow-ups, not blockers on Q267's merge.

**Re-route #8 — Q267 live-reconfirmed: the wide pool HOLDS (collapse seam gone); Q224 still NOT green, now cleanly isolated to GitHub's fan-out distinct-delivery starvation (2026-07-05).** The definitive clean-namespace close-out.
Built `agc:e2e-63cddfc` (amd64 `sha256:ab7811e7…`, index `sha256:be26d80c…`, HEAD/#528 = Option A default-on + Q266 loser-defer + Q267 token-400 ride-out), deployed via the GMC `AGC_IMAGE` patch with **no** explicit `AGC_FANOUT_COMPLETION` env (shipped default-on confirmed: `os.Getenv("AGC_FANOUT_COMPLETION") != "false"`; the AGC pod's `imageID` matched the pushed digest).
Ran from a **fresh tenant** — a new `ActionsGateway/dogfood8` + `RunnerSet/ci8` with a **new label `gag-ci8`** so the AGC's runner records are `ci8-N`, sidestepping the guard-un-cleanable stale `ci-N` clutter that confounded #6/#7 (the old `dogfood`/`ci` CI gateway was deleted so its AGC couldn't churn the stale records).
Capacity: non-preemptible `workers-od` **`e2-standard-4 ×4`, `pd-standard`** ([Q248](../dogfood-runner-rightsizing.md), 6 nodes Ready throughout, zero preemption), `default-pool` 2, worker CPU request 1.
The Q265 lever at its widest: **`maxListeners = 48`** (the exact config that collapsed to `online = 0` in #7), `maxWorkers = 8`, `spec.logLevel: debug`.
**No mid-run AGC restart** (the #7 confound).
Fired the same ~7-job matrix as #5/#6/#7 — push-event reruns of `28734640377` (unit-test, 6 gag jobs) + `28734640415` (integration) **on the exact deployed commit `63cddfc`**, concurrency-immune — at `08:50:22Z`.

**Q267 — the wide-pool collapse seam is GONE (live-confirmed).** Over a **20-minute stable window** (`08:59`–`09:19`) the pool held steady and every collapse marker stayed at **zero**: **0** `broker token exchange … "Registration … was not found"` (400), **0** Q267 ride-out retries needed, **0** fatal `deregister conflicting …` listener exits (re-route #6 had 41/38), **0** `worker capacity full`. re-route #7's `maxListeners = 48` collapsed to `online = 0` **from exactly this seam**; #8 at the same width does **not** collapse.
**Honest nuance:** the token-400 condition did not *arise* at all (0 occurrences), so Q267's ride-out *retry path* was not exercised live — the churn that triggers it did not materialize, because Q266 parks deduped losers instead of recycling them (far fewer re-registrations) and the clean namespace removed the stale-record amplifier.
So the wide-pool hold is a property of the combined **Q266 + Q267 + clean-namespace** stack; the retry logic itself remains covered by the offline repro (`goroutine_q267_test.go`, #528).
Either way the load-bearing question — *does the wide pool still collapse?* — is answered **no**.

**Q260 / Q266 — all firing correctly.** `duplicate_delivery` dedup **5**, Option A `completed a deduped sibling delivery via completejob` **5** (per-delivery), **0** `create Secret … already exists`, **0** `create Pod … already exists`, **0** `fanout_loser_recycle_deferred fallback_timeout`.
The **2** logical jobs whose planIDs the AGC actually received (`62d2c792`, `17c6c7dd`) ran to completion and concluded **success** (`coverage`, `integration-test`).

**Q224 — NOT green; the residual is now cleanly isolated.** Final tally: **2/7 success** (`coverage`, `integration-test`), **5/7 wedged `in_progress` indefinitely** (`shellcheck`, `vendor-check`, `tidy-check`, `unit-test`, `lint`) — at `10:02Z` (>1h) the run-level status had frozen at `completed/success` (aggregated from `coverage`) while the **jobs API** still showed the 5 as `in_progress`, the exact re-route #4 "runs-API aggregate froze, jobs never conclude" signature.
**Mechanism (from the AGC debug logs, decisive):** the AGC **only ever saw 2 distinct planIDs**.
GitHub's broker fanned **one** planID (`coverage`'s `62d2c792`) out as ~6 sibling deliveries to the recycling listener slots; the AGC deduped every sibling on planID and released each via Option A `completejob` — **correct**.
But the **5 other jobs' own planIDs were never delivered** to the AGC, while GitHub had marked those 5 jobs `in_progress`/`started` on the recycled stable-named runners (`ci8-1` and `ci8-2` each carried 3 job assignments).
The **online-idle pool stalled at 3 active sessions — far below `maxListeners = 48`** — because a fan-out *duplicate* delivery does not grow the demand-driven 1:1 replacement pool the way a distinct acquisition does, so the pool could never present enough distinct idle slots for GitHub to place the 5 distinct jobs.
Net: **distinct-job-delivery starvation + pool-growth stall** → indefinite `in_progress` wedge.

**Attribution (what it is and isn't).** It is **not** a recycle-churn seam — Q259/Q266/Q267 markers were all **0** (no token-400, no fatal deregister, no collapse).
It is **not** the `completejob` tax — **0** `worker capacity full` (re-route #6's finding holds).
It is **not** an AGC code bug — deduping identical planIDs is correct, and the 5 distinct planIDs simply never arrived.
It **is** GitHub's server-side fan-out job-assignment interacting with GAG's **many-acquirers + stable-name single-use recycle** ([Q114](../../queue/README.md)) topology — the class [Option E / Q264](../q264-scale-set-protocol.md) (one acquirer, one authoritative job stream, **no** sibling deliveries and **no** per-name recycling) eliminates *by construction*.
Because the pool self-limited to **3 ≪ 48**, `maxListeners` width is **not** the binding constraint (a moderate `maxListeners` reproduces the same 3-session stall), so no re-run at a narrower width was warranted.

**Verdict.** **Q267: DONE** (wide pool holds at `maxListeners = 48`; the #7 collapse seam is gone).
**Q224: NOT green — do not close.** The blocker is no longer any of the now-fixed recycle seams (Q259/Q266/Q267) or capacity (Q248) or the `completejob` tax (Q265) — it is the **fan-out distinct-delivery starvation** intrinsic to the many-acquirers topology.
This **refines** re-route #5's "reconcilable AGC-side": Option A's *accounting* is correct (a job that *receives its planID* concludes green — 2/2 here), but the fan-out *dispatch* stochastically starves distinct jobs (#5 got 3/7, #8 got 2/7 with 5 indefinitely wedged), so a *reliable* full-matrix green is **not** achievable on the classic many-acquirers protocol.
This is a real throughput/assignment **wall** — distinct from the `completejob`-tax wall re-route #6 ruled out — and it **strengthens the [Q264](../q264-scale-set-protocol.md) (Option E, single-acquirer scale-set) case**, which stays a deferred v-next decision.
**Q224/Q242 stay open.** Evidence: AGC debug logs (`agc:e2e-63cddfc`), reruns `28734640377`/`28734640415` (burst `08:50:22Z`), 20-min stable time series `08:59`–`09:19Z` (online=3/idle=3, all collapse markers 0), terminal job state `10:02Z`.

**Q264 P4 — the ScaleSet path turn-up: fan-out starvation ELIMINATED by construction (2026-07-05).** The first live dogfood of the runner-scale-set acquisition protocol ([Q264](../../queue/Q264.md) Option E), and the definitive answer to the Q224 fan-out class that classic stalled on at 2/7 across eight re-routes.
Built `agc:e2e-8a29b75` (index `sha256:4c88631d…`, amd64 `sha256:91dd52ad…`, HEAD/#537 = all P3 landed) **and the matching P3c wrapper** `wrapper:e2e-8a29b75` (index `sha256:0040ae1e…`), both deployed via the GMC `AGC_IMAGE`/`WRAPPER_IMAGE` env patch.
**The wrapper bump is mandatory:** the scale-set worker runs `run.sh --jitconfig` only when the wrapper honors `WORKER_MODE=scaleset`; the stale rc.6 wrapper ran the classic payload path and **every** worker errored `read payload: open …/job-payload/payload: no such file` — the AGC set the env correctly, the old wrapper ignored it.
A fresh **ScaleSet tenant** replaced the classic one: `ActionsGateway/dogfoodss` (repo-scoped, **direct-egress** to match the classic baseline, `logLevel: debug`), `RunnerTemplate/default-ss` (build-capable `dogfood-runner:2.335.1` + Athens), `RunnerSet/ciss` (`acquisitionProtocol: ScaleSet`, single `runnerLabels: [gag-scaleset2]`, `maxWorkers: 8`).
**The pinned rc.6 v2 CRD chart had to be `helm upgrade`d to HEAD first** — it predated the `acquisitionProtocol` field, so the RunnerSet apply 400'd `unknown field "spec.acquisitionProtocol"`.
Capacity: non-preemptible `workers-od` `e2-standard-4 ×4` `pd-standard` ([Q248](../dogfood-runner-rightsizing.md)), `default-pool` 2, worker CPU req 1.
Fired the same ~7-job matrix as re-routes #5–#8 — push-event reruns of unit-test.yml `28752455482` (6 jobs) + integration-test.yml `28752455509` (1 job) on sha `4ea41f6`, routed via `GAG_RUNNER → "gag-scaleset2"`.

**Q224 GATE — MET.
The starvation is gone.** The single-acquirer listener received **7 distinct `JobAssigned` messages** and provisioned **7 distinct worker pods in 3 seconds** (19:55:54–19:55:57Z), one per job — **0 dedup, 0 `create Secret … already exists`, 0 `create Pod … already exists`**.
All 7 jobs **ran** and all 7 reached a **terminal conclusion**; **none wedged `in_progress` indefinitely** — the exact opposite of classic (#5/#8: **2/7**, 5 jobs starved forever because their distinct planIDs never arrived).
One acquirer, one authoritative queue, no sibling deliveries → the fan-out distinct-delivery starvation **cannot occur**, confirmed live.
Capacity gating held: `maxWorkers=8` advertised as `X-ScaleSetMaxCapacity`; GitHub assigned exactly the 7 queued jobs (≤ capacity), one worker each.
**U3 core settled:** the `run.sh --jitconfig` worker ran in a real pod, connected, pulled its job, executed, and **its runner reported its own true terminal result** — the data plane the classic AGC never saw.

**But a pristine all-7-green sweep was NOT obtained — 3 green, 4 non-green, every non-green ORTHOGONAL to acquisition.** `shellcheck`/`tidy-check`/`vendor-check` ✅ (the last confirms Athens under the scale-set worker).
`unit-test` ❌ + `coverage` ❌: a **self-referential dogfooding artifact** — the provisioner sets `WORKER_MODE=scaleset` on the runner *container*, the job's `go test` inherits it, and the `cmd/worker` tests take the jitconfig branch and fail their classic-payload assertions (`TestRun_ReadPayloadErrorIsWrapped`, …); `cover-check` runs the same suite.
This bites **only** because GAG dogfoods its own CI on its own scale-set worker — a normal tenant is unaffected.
**Fix = the `cmd/worker` tests must pin `WORKER_MODE` (Queue item); it must land on `main` before a clean-green dogfood re-run is possible.** `integration-test` ❌: envtest `context canceled` under node CPU saturation (98–101% during the 7-wide burst) — a capacity confound ([Q248](../dogfood-runner-rightsizing.md)).
`lint` (cancelled): hit its own `timeout-minutes: 10` (golangci-lint pathologically slow on the saturated node) — a mundane timeout, **not** a lock lapse.
**U5 was not tested** (no mid-job eviction; the two ~10-min durations were natural runtime / a job timeout).

**Minor findings (Queue):** the `scaleset` client maps **every** 409 to `SessionConflictError` ("session already exists"), mislabeling a `generatejitconfig` runner-name conflict; and the listener retries `generatejitconfig` with no backoff and never advances the cursor on a persistent 409 → a tight ~1/s replay loop that can wedge a batch on a stale registered runner-name (surfaced when a mid-flight pod delete left a JIT runner registered — sidestepped by switching to a fresh scale-set name `gag-scaleset2`/scaleSetID 4).

**Verdict (first pass — superseded by the clean-green re-run below).** The ScaleSet path **eliminates the Q224 fan-out starvation by construction — proven live (7/7 assigned+ran+concluded vs classic 2/7)**; the [Q264](../../queue/Q264.md)/Option E structural claim is CONFIRMED.
This pass left Q224 open pending the `WORKER_MODE` test fix on `main` + a clean re-run on adequate CPU — **both delivered the same day (all 7 green), so Q224 is now CLOSED.** First-pass evidence: AGC debug logs (`agc:e2e-8a29b75`, scaleSetID 4), reruns `28752455482`/`28752455509` (burst `19:55:54Z`, sha `4ea41f6`).

**Q264 P4 — CLEAN-GREEN re-run: the run that CLOSES Q224 (2026-07-05).** With the two first-pass confounds fixed on `main` (Q269 `WORKER_MODE` test leak, #542; Q270 `generatejitconfig` 409/no-backoff-wedge hardening, #544), the 7-job matrix was re-run and **every job concluded GREEN** on the ScaleSet path.
Rebuilt AGC + wrapper off `main`@`2025557` — `agc:e2e-2025557` (index `sha256:cef2a16b…`, amd64 `sha256:229435a5…`) + `wrapper:e2e-2025557` (`sha256:7974a83b…`) — via the GMC `AGC_IMAGE`/`WRAPPER_IMAGE` patch.
The first-pass `dogfoodss` tenant had been torn down to a bare orphaned `ciss`; the gateway (`dogfoodss`, repo-scoped, direct-egress, `logLevel: debug`) + template (`default-ss`, `dogfood-runner:2.335.1` + Athens, worker CPU req 1) were recreated, and `ciss` was reset to a **fresh scale-set label `gag-scaleset3`** (scaleSetID 5).
**Why a fresh label mattered:** reconnecting to the first-pass `gag-scaleset2` (scaleSetID 4) **replayed** the old sha-`4ea41f6` `JobAssigned` messages from the scale-set-scoped queue (the documented recovery-replay, §2b-3), which briefly provisioned 7 pre-Q269 workers — a new label = a new scale-set object = an empty queue, so the re-run ran only the intended `main` jobs.
Capacity: `workers-od` `e2-standard-4 ×6` `pd-standard` (bumped 4→6 for CPU headroom vs the first pass's 98–101% saturation), `maxWorkers: 8`.
Fired via Q271 opt-in routing (`workflow_dispatch` + `target_gag=true`, `GAG_RUNNER → "gag-scaleset3"`): unit-test.yml `28759754797` (6 GAG jobs) + integration-test.yml `28759755655` (1).

**Result — 7/7 GREEN, 0 dedup / 0 wedge.** The single-acquirer listener took 7 distinct `JobAssigned` and provisioned **7 distinct worker pods in ~2 s** (00:11:30–32Z), one per job, **0 dedup / 0 `already exists` / 0 jitconfig conflict / 0 cursor wedge** (the AGC log shows zero conflict/wedge lines across the run — Q270 holds).
All 7 GAG jobs ran on `gag-scaleset3-<jobUUID>` runners and concluded `success`: `unit-test` ✅ + `coverage` ✅ (**Q269 fix holds** — the two first-pass `WORKER_MODE` failures are gone), `shellcheck`/`tidy-check`/`vendor-check` ✅, `lint` ✅ (no `timeout-minutes: 10` lapse — the 6-node headroom removed the CPU saturation), `integration-test` ✅ (no envtest `context canceled`).
Worker pods reaped `phase: Succeeded` (runners exited 0).
**Q224 is CLOSED; [Q264](../../queue/Q264.md) P4 fully validated; P5 UNBLOCKED; [Q242](q242-g1-proxy-destination-allowlist.md) concurrent-green achieved.** Evidence: AGC debug logs (`agc:e2e-2025557`, scaleSetID 5), runs `28759754797`/`28759755655` (burst `00:11:30Z`, sha `2025557`).

**Operational note (2026-07-03; resolved on-demand 2026-07-07, Q231):** the `gag-dogfood-e2e` tenant's `dogfood-e2e-agc` pod (~500m CPU) does not fit alongside the CI AGC + GMC + Athens on a single `e2-standard-2` system node — with it running the CI AGC stays `Pending` (`Insufficient cpu`).
**Resolved:** the e2e tenant is now **on-demand** (Part F F3) — `e2e-start.sh` spins the AGC up per run and `e2e-stop.sh` deletes the `ActionsGateway` to tear it down, so it no longer stands alongside CI by default.
A turn-up that runs CI **and** e2e concurrently still needs the headroom: scale `default-pool` to **2** nodes (the `SSD_TOTAL_GB=500` quota then bounds the `workers` pool to ~3).

**Build-capable runner image (Q239).** The bare upstream `actions-runner` has no build toolchain (`make`, a C compiler), so this repo's `make`-based jobs fail `exit 127: make: command not found` on it — the workflows assume `make` is preinstalled, as on GitHub-hosted `ubuntu-latest`.
The fix is a build-capable `workerImage`: [`scripts/dogfood/runner/Dockerfile`](../../../scripts/dogfood/runner/Dockerfile) adds `build-essential` (+ `curl`/`xz`/`sudo` for the shellcheck job's pinned-binary self-install) on top of the pinned upstream runner.
Build and push it with [`scripts/dogfood/runner-build.sh`](../../../scripts/dogfood/runner-build.sh), then export `DOGFOOD_RUNNER_IMAGE=ghcr.io/actions-gateway/dogfood-runner:<tag>` before running `scripts/dogfood/setup.sh` — the `RunnerTemplate` pins it and the AGC still injects the Q235 wrapper on top.
**Validated `2026-06-29`:** the `shellcheck` job, which failed `make: command not found` on the bare image, ran green on `dogfood-runner:2.335.1` with the wrapper injected (`make` 4.3, `gcc` 13.3.0).

**Release-asset egress is already allowlisted (Q246 — misdiagnosis).** GitHub *release-asset* downloads (the `shellcheck` tarball, `setup-go`'s Go toolchain) 302-redirect from `github.com` to `objects.githubusercontent.com` → `185.199.108.0/22`.
That is GitHub-dedicated space (not shared Fastly), and the worker egress `NetworkPolicy` **already permits it**: the GMC IP-range feed merges GitHub `/meta`'s `api`+`actions`+`web` keys, and the `web` range contains `185.199.108.0/22` ([`ipranges.go`](../../../cmd/gmc/internal/controller/ipranges.go)).
So Q246's original "workers can't reach the CDN, add it to the egress allowlist" premise is wrong — do **not** widen the allowlist or bake the asset into the runner image.

**Q246 root cause — the Q61 cold-start cache race (confirmed live, fixed).** A cold live run on `gag-dogfood` (`2026-07-01`, direct-egress gateway) settled it — full evidence in [`archive/q246-release-asset-timeout-live-diagnosis.md`](q246-release-asset-timeout-live-diagnosis.md).
Four live observations: (1) a workload-labelled pod downloads the shellcheck release asset over the 302→`objects.githubusercontent.com`→`185.199.108.133` hop in **0.32 s (HTTP 200)** when the NP is programmed — egress is not the problem; (2) scaling the system pool 0→1 forced a fresh GMC and the `dogfood-workload` NP **dropped from 7337 CIDRs to 1 for ~25 s** — the per-CR reconcile (`ActionsGatewayV2Reconciler.applyNetworkPolicy`, a `CreateOrPatch` that overwrites `Spec.Egress` wholesale) rebuilt the policy from the still-empty IP-range cache before `IPRangeReconciler`'s first `/meta` fetch landed and repatched; (3) with the GitHub rule absent the identical download **times out (`curl` rc=28)** — the exact Q246 symptom — and returns to HTTP 200 once restored; (4) a *warm* GMC restart did **not** blank (the fetch won the race), so the window's width scales with GMC-startup + fetch latency, which the Q247 node CPU exhaustion lengthens (and can re-trigger by restarting GMC mid-run).
So the cause is **(a) the Q61 race**; **(b) Q247 CPU is only an amplifier** (already fixed; node right-sizing is Q248).
Egress succeeds regardless of CPU whenever the NP carries the rule.
**Fix:** the per-CR reconcile now **preserves an existing direct-egress NP's allowlist while the cache is empty** instead of blanking it (a not-yet-created NP stays fail-closed) — so no GMC restart, under any load, strips a live worker's or the AGC's GitHub egress.
Secure-by-default preserved (egress is never widened).
Regression tests in `actionsgateway_v2_test.go`.

**Q247 root cause — RenewJob used the wrong jobId (fixed).** Every job routed to a GAG runner failed at the *job-lifecycle* level: the worker ran the full job, then `JobRunner.CompleteJobAsync` threw `TaskOrchestrationJobNotFoundException` ("workflow instance not found"), the run showed `conclusion: failure` with no failed step and no logs, and multiple worker pods appeared for one job.
Root cause: the AGC's per-job renewal loop ([`goroutine.go`](../../../cmd/agc/internal/listener/goroutine.go), `handleJob`) sent the broker envelope's numeric `MessageID` as the run-service `jobId` instead of the job's `RunnerRequestID` — the value `AcquireJob` already sends as `jobMessageId` and the value the run service keys `/renewjob` on.
The run service does not recognize the envelope id, so `RenewJob` never renewed the lock; the error was swallowed as non-fatal and the worker kept running.
On any job that outlived GitHub's lock TTL, GitHub recycled the job and redelivered it to a **sibling** session — a duplicate worker pod — while the original ran to completion and orphaned at CompleteJob.
Short jobs finished before the TTL lapsed, which is why only the long (~10 min) e2e job exposed it deterministically and the general non-e2e jobs hit it intermittently (the "stuck at N active sessions after recycling" symptom in Q247).
The one-line fix (renew by `RunnerRequestID`) plus a full-`Run` regression test landed in the AGC listener; a green dogfood e2e on GAG (PR #476's branch) is the live confirmation.
This was **not** the DinD/config/egress/CPU issue the co-occurring node exhaustion suggested — it reproduces in isolation against the broker HTTP stub.

**Q247 residual — an unbounded renewal call wedges the loop (fixed).** After the jobId fix, a live dogfood run still failed at *exactly* the ~10-minute mark (job started 03:21:27Z, GitHub marked it `failure` at 03:31:27Z = 600s) with a single worker pod that ran well past the cutoff.
The job died at the *initial* AcquireJob lock TTL, meaning no renewal ever landed even with the correct jobId.
Cause: the per-job renewal loop (`StartRenewLoop`) ran each `RenewJob` call inline with **no per-call timeout**, unlike `AcquireJob`/`createSession`, which bound every call with `controlPlaneTimeout`.
Under the e2e's node CPU/egress saturation a single `/renewjob` call black-holes (TCP accepted, no response), and because the next tick cannot fire until the call returns, that one wedged call starves *every* subsequent renewal until the lock lapses at 600s.
Fix: bound each renewal call with the same `controlPlaneTimeout` (30s) — a hung call now aborts (counted as the existing non-fatal `renew_job_errors_total`) and the loop issues the next renewal on schedule, so one slow renewal costs one renewal, not all of them.
Regression test asserts a second renewal fires while the first is still hung (impossible if the loop is wedged).
This is the co-occurring node-exhaustion interaction the original Q247 note flagged, now closed as a code fix rather than a capacity workaround.

**Q247 auth — RenewJob used the wrong token (fixed).** With both prior fixes live (`agc:q247-3edc85e`), the renewal loop fired correctly but *every* `RenewJob` was rejected by GitHub with `401 {"source":"actions-run-service","errorMessage": "Not authorized for this job"}`, repeating every ~40s for both agent indices, and long jobs again failed at *exactly* 600s.
Root cause: `RenewJob` authenticated with the broker session (OAuth) token — the same token used for `CreateSession`/ `GetMessage`/`AcquireJob` — but the run service authorizes *per-job* lock renewal only with the job-scoped token it issues in the `acquirejob` response (the `SystemVssConnection` endpoint's `AccessToken`).
It accepts the session token to *claim* a job but rejects it to *renew* one, which is why acquire succeeded and every renewal 401'd from the first call (ruling out token expiry).
This mirrors the real runner, which renews via a `RunServer` connection built from the message's `SystemVssConnection` endpoint (`VssUtil.GetVssCredential`), not the listener OAuth token.
Fix: `AcquireJob` now parses the endpoint token (`AcquireJobResponse.JobAuthToken`) and the listener threads it into every `RenewJob` call as the `Authorization` bearer (`RenewJobRequest.AuthToken`), falling back to the session token only when absent.
Merge gate: a full-`Run` listener test drives a simulated >10-minute job whose renew endpoint 401s on any non-job token and asserts every renewal is authorized; the fakegithub broker and the broker-compat suite (new contract C16) model the same auth.
The remaining defense-in-depth gap — tearing down the worker when a lock is *definitively* lost after sustained renewal failure — is tracked as Q254.

**`vendor-check` / `tidy-check` unblocked by Athens (Q244, implemented).** An Athens in-cluster Go module proxy (`deploy/athens/`, applied by `dogfood/setup.sh`) caches Go modules so workers never need to reach `proxy.golang.org` directly.
Athens pods (app=athens) are not covered by the workload NetworkPolicy and have free egress; workers reach Athens via an additive NetworkPolicy (port 3000) and are wired with `GOPROXY=http://go-module-proxy.gag-dogfood.svc.cluster.local:3000,off` plus `GONOSUMDB=*` in the RunnerTemplate.

**Background (for reference):** GKE Dataplane V2's *managed* Cilium does not expose the `cilium.io/v2 CiliumNetworkPolicy` CRD (dropped since GKE 1.21.5-gke.1300), so an `EgressProxy` with `egressPolicyMode: CiliumFQDN` goes `Degraded` (`no matches for kind "CiliumNetworkPolicy"`, verified 2026-06-29).
`destinationCIDRs` is no substitute for `proxy.golang.org`/`sum.golang.org` (Google-fronted ⇒ a CIDR allowlist opens all of Google's frontend).
The FQDN intent/mechanism split (Q245) remains open.
Detail + provider matrix: [Q242 plan § Provider FQDN-egress fragmentation](q242-g1-proxy-destination-allowlist.md#provider-fqdn-egress-fragmentation-post-implementation-finding).
