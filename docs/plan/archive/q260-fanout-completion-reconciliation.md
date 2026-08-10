# Q260 — reconcile GitHub's per-delivery fan-out with AGC's one-runner-per-session model

**Status:** **Option A live-confirmed by re-route #5 (2026-07-04) and flipped ON by default (`AGC_FANOUT_COMPLETION`, opt out with `=false`).
Q260 DONE; Q224's fan-out blocker cleared (a pristine full-matrix green is now gated only on `maxWorkers` worker-capacity tuning, Q248 — not the accounting gap).** The winner of a fanned-out job tracks every deduped sibling delivery and, on completion, fans a `completejob` out to each (keyed on the sibling's own `RunnerRequestID`, with the winner's pod-phase-proxy result); a late redelivery within the linger window is resolved with the recorded terminal result.
The one load-bearing assumption — does GitHub honour a non-running delivery's completion, and does resolving *all* siblings conclude the job? — is **confirmed YES**: re-route #5 observed `completejob` on a live sibling's own job ID return **OK** (not "already resolved"), the winner's own delivery carry the real workflow result, previously-wedged concurrent jobs conclude **green**, the Q259 recycle 422 clear per job on winner completion, and **no** job cancel at the ~15-minute timeout.
Completion is **per-delivery, not planID-scoped**, so the secure-by-default concern (a green sibling proxy masking a red workflow) does not arise — the flag is on by default. `TestAGC_Q260_FanoutCompletionReconciles` (the gate) passes; the companion `…AccountingGap` (flag off) still asserts the pre-fix wedge.

This closed the last blocker for Q224 "route production CI green," after the earlier Q260 work closed capacity (Q248), Secret/Pod collisions (#512), and the planID dedup key ([`q260-planid-dedup-refix.md`](q260-planid-dedup-refix.md)).
Full live evidence: [`gke-dogfood.md`](gke-dogfood-turnup-findings.md) re-route #4 (fails-today control) + re-route #5 (Option A confirmed).

---

## 1. The protocol, from the code and the live evidence

Three id spaces are involved.
Keeping them distinct is the whole game:

| id | source | scope | in the code |
|---|---|---|---|
| `RunnerRequestID` | broker `GET /message` body ([`RunnerJobRequestBody`](../../../broker/types.go)) | **one per delivery** — distinct per sibling under fan-out | `jobBody.RunnerRequestID`; sent as `jobMessageId` (acquire) and `jobId` (renew/complete) |
| `planID` | `POST /acquirejob` **response** (`resp.Plan.PlanID`) | **one per logical job** — shared across all sibling deliveries | `provisioner`'s `job-<planID>` Secret/Pod name; the dedup key (#503→#512) |
| `sessionID` | `POST /session` | one per listener goroutine | the long-poll identity |

### What GitHub does (server behaviour — confirmed live)

Under a concurrent burst, GitHub's broker **fans one logical job (one `planID`) out to N sibling sessions** as N separate `RunnerJobRequest` messages with **distinct `RunnerRequestID`s**.
Each sibling's `POST /acquirejob` **succeeds** and returns the **same `planID`** (re-route #2/#4: 5 sessions all provisioning `job-<samePlanID>`).
So GitHub does **not** enforce single-acquisition per job — `acquirejob` is the atomic claim of a **delivery**, not of the job (consistent with Investigation A, [`03-api-contracts.md`](../../design/03-api-contracts.md#33-re-implemented-broker-api-endpoints): "`acquirejob` alone is the atomic claim", and `POST /acknowledge` is **not required** for correct delivery — so the missing message-ack is *not* the bug).

Each delivery is an independent **assignment** on GitHub's side.
GitHub expects each acquired assignment to either be renewed + completed, or it reclaims it: an assignment acquired but not started/renewed is **cancelled at the ~15-minute unstarted-job timeout**, and a stale assignment triggers **redelivery** to another session (the Q247 mechanism: "outlived the lock TTL → GitHub redelivered to a sibling").

Two more server facts, confirmed live in re-route #4:

- Completing **one** sibling delivery does **not** conclude the logical job.
  The winner's worker pod completes its own delivery with a real result, yet GitHub held the job `in_progress` / cancelled it.
- `completejob(result=skipped)` on a sibling returns **HTTP-OK but does not conclude the job** (14/15 OK; 1× `401 "Not authorized for this job"`).
  It merely acks that one delivery.
- The Q259 `422 "runner … is currently running a job and cannot be deleted"` on recycle is the same accounting seam from the runner side: GitHub still considers a deduped-away runner **assigned to the job**.

### What the AGC does (our behaviour — from the code)

`handleJob` ([`goroutine.go`](../../../cmd/agc/internal/listener/goroutine.go)) per delivery:

1. `AcquireJob(jobMessageId = RunnerRequestID)` → learns `planID` + a per-delivery job token (`AcquireJobResponse.JobAuthToken`).
2. **Dedup gate** `ClaimJob(planID)` (post-acquire, #512): the **first** sibling to claim the `planID` **wins** and provisions; the **losers** skip provisioning and `return acquired=true` so their runner recycles.
   The claim lingers for `completedPodTTL` past completion so a late redelivery is deduped too.
3. Winner only: `SpawnReplacement` → `StartRenewLoop(jobId = RunnerRequestID)` → `JobHandler` (provisions the worker Pod; the worker's runner binary makes the winner delivery's `completejob`).

Critically: the loser path does **nothing** to its acquired delivery (default), and the **JobHandler never learns the job's real succeeded/failed result** — the provisioner returns `nil` even on `PodFailed` ([`provisioner.go`](../../../cmd/agc/internal/provisioner/provisioner.go), step 5–7); only the worker's runner binary reports the real result, and only for the winner delivery.

### Why this is GAG-specific — ARC has one acquirer

The gap is a consequence of GAG's **topology**, not a defect in GitHub's protocol.
The offer-to-many / redeliver-on-stale dispatch is an inherent, by-design race: GitHub offers a queued job to any eligible idle session and treats `acquirejob` as the claim, assuming each acquired delivery is either run-to-completion or reclaimed.

Modern ARC (`gha-runner-scale-set`) never surfaces it because it runs **one** `Runner.Listener` per scale set ([`01-executive-summary.md`](../../design/01-executive-summary.md), [`appendix-f-cost-model.md`](../../design/appendix-f-cost-model.md)): that single listener acquires each job **once**, then spins a dedicated ephemeral pod to run it — a strict 1:1 acquire-to-run with **no sibling deliveries to reconcile**.
ARC separates *acquire* (one listener) from *run* (a fresh pod).

GAG instead runs a permanent baseline plus up to `maxListeners` concurrent long-polling sessions per RunnerGroup, **each independently able to `acquirejob`** ([`02-architecture.md`](../../design/02-architecture.md)).
That is the ~60 KiB/session virtual-runner model GAG is built on — and it is exactly what lets GitHub hand one job to several sessions, all acquire it (shared planID), and leave N assignments for one logical job. **The fan-out is intrinsic to the many-acquirers topology**; the dedup and this completion reconciliation are the tax that topology pays.
Option E treats the cause instead of the symptom.

---

## 2. The accounting gap (exact)

```
        one logical job = planID P
                 │  GitHub fans out (burst)
     ┌───────────┼───────────┬───────────┐
   req-1       req-2       req-3       req-4        ← distinct RunnerRequestIDs
     │           │           │           │            (independent assignments)
  acquire     acquire     acquire     acquire       ← ALL succeed, all return planID P
     │           │           │           │
  ClaimJob(P)  ClaimJob(P)  ClaimJob(P)  ClaimJob(P)
   WINS         loser        loser        loser
     │           │           │           │
  provision   recycle      recycle      recycle     ← losers do NOTHING to their assignment
     │        (silent)     (silent)     (silent)
  worker runs
  completejob(req-1, succeeded)   ← only the winner's OWN delivery is resolved
     │
     ▼
  GitHub: job P still has req-2/req-3/req-4 = acquired-but-unresolved assignments
     │
     ▼   (~15 min)
  unstarted-job timeout on a dangling sibling  →  JOB CANCELLED
                                                  (even though req-1 completed it)
```

**The gap:** the AGC collapses N deliveries to one *runner*, but does nothing to collapse them on *GitHub's* books.
The winner resolves exactly **one** of N assignments; the other N−1 are acquired-then-silently-abandoned, and each is a live assignment GitHub is still waiting on.
At the unstarted-job timeout the dangling assignments cancel the whole job — the winner's completion of its own delivery does not save it, because GitHub tracks completion **per delivery**, and N−1 deliveries never reported anything.

This is **distinct from and beyond** the dedup (which is correct and done) and distinct from Q259 (the recycle 422 is the same seam observed from the runner side).

---

## 3. Why envtest passed while production wedged — and the repro that fixes that

The default `brokertest.Server` modeled **neither** the fan-out **nor** per-delivery completion: every `acquirejob` returned a fresh `test-plan-N`, and `completejob` was just a counter.
The Q260 envtest ([`q260_duplicate_delivery_test.go`](../../../cmd/agc/internal/controller/integration/q260_duplicate_delivery_test.go)) forced a constant planID and asserted the **dedup** — then had the winner **block forever** in `waitForCompletion`.
It never modeled a job *concluding*, so the accounting gap was invisible.
That is exactly why the dedup looked green offline yet wedged live.

**Repro landed in this PR** (opt-in, so existing tests are unaffected):

- `broker/brokertest/server.go` gains a **fan-out job-accounting model** (`EnableFanoutAccounting`, `EnqueueFanoutJob`, `JobState`, `ExpireUnstartedDeliveries`).
  It tracks a logical job per planID with one assignment per delivery, and encodes the invariant observed live: **a job concludes only when every acquired delivery is resolved with a consistent real (non-`skipped`) result; any acquired-but-unresolved delivery cancels the job at the unstarted timeout; a `skipped`-only job never goes green** (matching the #513 flag-ON result).
- `cmd/agc/internal/listener/q260_fanout_accounting_test.go`:
  - `TestAGC_Q260_FanoutCompletionAccountingGap` (**green today**) drives the real dedup path against the model — winner completes its own delivery, siblings dedup — and asserts the job ends up **`cancelled`** at the timeout.
    This locks the wedge in as a deterministic, offline regression.
  - `TestAGC_Q260_FanoutCompletionReconciles` (the fix gate, **now un-skipped and green** with the flag on) asserts the job concludes **`completed`**.
    It failed against pre-fix code (`completed` vs `cancelled`) and passes now that Option A lands — validating the fix with no turn-up.

Model assumptions and their grounding are documented inline in `server.go`; the one assumption the model *cannot* prove is §5's live-only unknown.

---

## 4. Design options

The AGC can only reach GitHub through the calls it already has (`acquirejob`, `renewjob`, `completejob`, `deletesession`) — there is **no** "reject/un-acquire" call.
Once a loser has acquired, its assignment exists; the only levers are *how* and *when* to resolve it.

### Option A — Winner fans completion out to all sibling deliveries (recommended)

Track, per `planID` claim, every deduped sibling delivery `(RunnerRequestID, runServiceURL, jobToken)`.
When the winner's job **finishes**, the AGC issues `completejob` for **each** sibling delivery (and any late redelivery that arrives during the linger window) so **no** assignment dangles.
Losers do **not** complete early.

- **Why it's not #513:** #513 completed the loser **immediately** on dedup, with `skipped`, **before** the job ran.
  That (a) acked the assignment with a non-result and (b) raced the winner.
  Option A completes siblings **after** the winner's real completion, so whichever delivery GitHub treats as authoritative gets resolved at the right time, and a planID-scoped completion (if that is how GitHub works) is harmless because the job is already done.
- **The result to report:** the AGC does **not** know the workflow's real succeeded/failed (§1).
  Options, in order of preference pending §5:
  1. **`skipped`** for the siblings — honest (they ran nothing) and lowest blast radius if completion is planID-scoped, but re-route #4 showed `skipped` does not *conclude* a delivery GitHub is waiting on.
     Likely insufficient alone.
  2. Report the **winner's pod phase** proxied to `succeeded`/`failed` (`PodFailed`→`failed`, else `succeeded`) — richer, but risks greening a red job **iff** completion is planID-scoped (a red workflow whose worker exited 0).
     Gate behind the flag; secure-by-default keeps it off until §5 clears it.
- **Cost:** N−1 extra `completejob` calls per fanned-out job, bounded by fan-out width; negligible against the hourly budget.
- **Late redelivery:** the linger claim (#512) already dedups a redelivery arriving after completion; extend the claim registry to remember the planID's terminal outcome for the linger window so the late delivery is resolved with the same result rather than left dangling.

### Option B — Loser abandons its own delivery immediately (the #513 path)

**Rejected — settled dead-end.** Live-tested in re-route #4: `completejob(skipped)` immediately returns OK but does **not** conclude the job; worse, by acking the delivery it **suppresses** the unstarted-timeout that would otherwise resolve it, so a late redelivery re-assigns the already-run job and GitHub holds it **indefinitely `in_progress`** — strictly worse than a terminal cancel.
The per-loser-immediate path is therefore **removed/unreachable**: Option A reuses the same `broker.CompleteJob` plumbing but fires it from the **winner**, deferred to job completion and fanned to all siblings, under the rescoped `AGC_FANOUT_COMPLETION` flag — never per-loser at dedup time.

### Option C — Reduce fan-out width

**Rejected.** Fan-out is GitHub minting multiple messages for one job; it is inherent to a multi-session pool and cannot be eliminated without serialising the pool (kills throughput, the Q248 regression).
Narrowing `maxListeners` only narrows, never closes, the window.

### Option D — Another dedup key / pre-acquire dedup

**Rejected — settled.** planID is the correct key (#512, live-validated 0 collisions).
The problem is completion accounting, not acquisition dedup.
No new key helps.

### Option E — single-acquirer topology / adopt the runner-scale-set protocol (treat the cause)

**Deferred — a v-next architectural pivot; the fallback if Option A proves infeasible (§5).** The fan-out exists only because GAG runs *many* acquiring sessions per group on the classic per-runner broker protocol, where **concurrency = number of registered runners = number of acquirers**.
So "one acquirer" and "N concurrent jobs" are mutually exclusive on this protocol: a single classic session is a single runner and would **serialize the whole group to one job at a time** (and there is no pre-acquire single-flight key — planID is post-acquire and the message's `RunnerRequestID` differs per sibling; the entire Q260 history).
Getting one acquirer *with* concurrency requires GitHub's **runner-scale-set message-queue protocol** (what ARC uses): one listener long-polls a single job stream, `acquireJobs` claims a **batch**, and GAG creates one worker pod per acquired job.
One authoritative stream ⇒ no sibling deliveries ⇒ the Q260 / Q247-completion / Q259-recycle class is eliminated **by construction**, not reconciled.
GAG would still beat ARC on footprint (a Go listener goroutine vs a ~256 MiB .NET process) and keep egress isolation + on-demand workers — "ARC's protocol, GAG's efficiency."

**Cost:** a large rewrite of the acquisition tier and a partial redefinition of GAG.
It discards most of the classic-protocol machinery (per-agent JIT session model, agent pool + single-use recycle Q114, the Multiplexer, the Q260 dedup, the Q247 renew-by-`RunnerRequestID` path); it means reverse-engineering and depending on a **second** GitHub-internal protocol; it reworks registration/auth (register a scale set, not N agents) and re-expresses the admission gate (Q59) + `priorityTiers` against a dispatch model; and it collapses each group's acquisition to a **single point of failure** (vs today's N independent sessions, each with its own Q137 revival).
It also retires the "thousands of goroutine-backed virtual runners" identity — density at rest actually *improves* (one listener/group, not N), but the story becomes "a lighter-weight ARC listener" rather than "cheap virtual runners."
Revisit only if §5 rules Option A out.

**Recommendation: Option A** — **implemented**, behind the flag renamed/rescoped from the per-loser `AGC_COMPLETE_ABANDONED_DELIVERIES` to the winner-driven `AGC_FANOUT_COMPLETION`, **off by default** until re-route #5 confirms §5.

---

## 5. The one live-only unknown, and the feasibility caveat

Option A rests on a single unproven assumption:

> **Does `completejob` on a sibling delivery that never ran the job cause GitHub to stop waiting on that assignment — and does resolving *all* assignments let the job conclude green?**

Re-route #4 proved `skipped` **acks** a delivery (HTTP-OK) but does **not conclude** the logical job.
It did **not** test (a) resolving **every** sibling, nor (b) a **real** result (`succeeded`/`failed`) on a sibling.
Those are the open questions.

**Feasibility honesty (per the task's stop-condition):** there is a real chance this is **not** fully reconcilable AGC-side.
If GitHub always treats the **most-recent** delivery as the job's authoritative assignment and **ignores older deliveries' completions**, then new redeliveries keep arriving faster than the AGC can resolve them, and no amount of sibling-completion converges — the fix would then require a GitHub-server behaviour we cannot influence.
The re-route #4 evidence (a late redelivery re-assigning an already-run job) is *consistent with* that worst case, but does not prove it, because #4 only ever completed siblings with `skipped`.
This is why the design ships behind a flag with a decisive live experiment rather than as a default: **one re-route #5 settles feasibility.**

### Re-route #5 confirmed (2026-07-04) — GO

Enabled Option A by setting `AGC_EXTRA_AGC_FANOUT_COMPLETION=true` on the GMC pod (which forwards `AGC_FANOUT_COMPLETION=true` to the AGC Deployments — no GMC code change), on a fresh `agc:e2e-238b8df` (includes #521), on the re-route #4 stable capacity (non-preemptible `workers-od` ×3 + default-pool 2), `spec.logLevel: debug`.
Fired the same ~7-job concurrent matrix (unit-test + integration reruns on sha 238b8df, green on GitHub-hosted).
Observations:

1. **`completejob` on a live sibling returns OK** — at 16:37:07 a fanned-out job (planID `357b6d9e`, winner on ci-0) whose winner completed *naturally* fanned `completejob` out to **both** deduped siblings (jobIDs `34ad8db4` on ci-2, `f968c752` on ci-4) → **both `completed a deduped sibling delivery via completejob`** (HTTP OK), **not** "already resolved".
   So GitHub **accepts** the completion of a sibling delivery that never ran the job.
   (A sibling whose winner had been *concurrency-cancelled* by an unrelated Dependabot rebase returned "already resolved server-side" — GitHub had already torn that delivery down — which is why the winner must complete naturally for the clean signal.)
2. **Previously-wedged concurrent jobs conclude green** — `coverage` and `integration-test` (fanned-out) concluded **success**; all 6 unit jobs eventually ran (none stranded at the unstarted timeout once the pool recovered).
3. **Durability** — `coverage` stayed `success` past 16:47, i.e. beyond the ~15-minute unstarted-timeout of its siblings (acquired ~16:31) — the exact point re-route #4's winner-completed jobs were cancelled.
   Option A prevented the cancel.
4. **Q259 recycle 422 clears per job** — the "runner … is still running a job and cannot be deleted" churn dropped ~12× once winners began completing and fanning `completejob`; the pool recovered from a collapsed 2 sessions back toward `maxListeners`, draining the backlog.
   (The 422 is a *rolling* transient per job's in-flight siblings, cleared on that job's winner completion — not a permanent wedge.)

**Verdict: GO (design point 4).** Completion is **per-delivery, not planID-scoped**: `completejob` on a sibling's own job ID resolves only that assignment, and the winner's own delivery still carries the real workflow result reported by its runner binary — so the pod-phase proxy on siblings cannot green a red workflow.
The secure-by-default gate is therefore cleared and the flag is flipped **on by default** (opt out `AGC_FANOUT_COMPLETION=false`).
Option E (Q264) is **not needed** — the many-acquirers topology is reconcilable AGC-side.

Confound handled: a Dependabot rebase merge-train briefly polluted the shared runner pool with pull_request runs that concurrency-cancelled on each force-push (cancels in ~4 min, distinct from the 15-min accounting timeout).
The clean signal came from the **push**-event 238b8df reruns, which are concurrency-immune.
Full evidence: [`gke-dogfood.md`](gke-dogfood-turnup-findings.md) re-route #5.

The alternative outcomes the experiment was designed to distinguish, for the record:
- **`skipped` acks but a real result concludes** → wire the pod-phase proxy (already the default in #521).
  Confirmed: the winner's real result concludes the job; siblings only need releasing.
- **Even resolving all siblings does not conclude the job** (most-recent-delivery authority) → would have been a **NO-GO**: gap GitHub-server-side, Q224 infeasible via many-acquirers, **Option E (single-acquirer topology)** the path. **This did not occur** — resolving all siblings concludes the job.

Also fix the orthogonal Q239 regression before #5 (the dogfood `RunnerTemplate` reverted to the toolchain-less upstream image — `make: command not found`), so a non-green result is attributable to accounting, not the runner image ([`gke-dogfood.md`](gke-dogfood-turnup-findings.md) re-route #4 secondary observation).

---

## 6. Test strategy

- **Offline gate — ✅ green.** The fan-out accounting model + the two listener tests in §3. `TestAGC_Q260_FanoutCompletionReconciles` (the acceptance gate, `t.Skip` removed) now passes with the flag on; `…AccountingGap` (flag off) still asserts the cancelled wedge.
  No turn-up needed.
- **Option A unit/envtest coverage — ✅ landed.**
  - `TestAGC_Q260_WinnerCompletesEachSibling` (listener): the winner issues exactly one `completejob` per deduped sibling, keyed on the sibling's own `RunnerRequestID`, with the pod-phase-proxy result.
  - `TestMultiplexer_FanoutClaim_TracksSiblingsAndLateRedelivery` (claim registry): siblings are registered and returned to the winner on `Complete`; a late redelivery within the linger window is handed the recorded terminal result.
  - `TestAGC_Q260_WinnerCompletesDedupedSiblingDelivery` (envtest): the deduped sibling's delivery is *resolved* by the winner on completion (keyed on its own `jobId`, result `succeeded` from the winner's Succeeded pod), not skipped-and-forgotten; it is **not** completed while the winner is still running.
  - `TestProvisioner_ResultPodPhaseProxy` (provisioner): `PodFailed`→`failed`, else `succeeded`.
- **Live (re-route #5) — ✅ GO (2026-07-04).** §5.
  The only step that could not be done offline, and the go/no-go for Q224.
  Confirmed: `completejob` on a live sibling returns OK (9/9, 0 failures across 13 fan-outs), fanned-out jobs conclude green, the job survives past the 15-minute sibling timeout, and the Q259 recycle 422 clears per job.
  Completion is per-delivery, not planID-scoped.
  Flag flipped on by default.

---

## 7. Q265 — fan-out throughput benchmark (2026-07-05): tax wall or tuning?

> **Superseded by §10 (2026-07-06):** this section records the first (confounded) benchmark, re-route #6.
> The clean wide-pool run and the final Q265 verdict — classic **2/7** vs ScaleSet **7/7**, comparison closed, Q265 DONE — are in §10.


Q260 proved Option A's *accounting* is correct (§5, re-route #5).
Q265 asks the *throughput* question that gates [Q224](gke-dogfood-turnup-findings.md) and the Option A-vs-Option E ([Q264](../q264-scale-set-protocol.md)) fork: on a warm, right-sized pool with `maxListeners ≫ maxWorkers × fan-out-width` (so listener supply is not the bottleneck), does the busy-worker pool **hold near `maxWorkers`** under sustained fan-out burst (→ Option A sufficient; re-route #5's "2/8" was tuning), or does the completion tax **serialize throughput and collapse the pool** (→ hard wall; revive Q264)?

**Verdict up front: the completion tax is NOT the wall — but a *clean* "holds at maxWorkers" measurement could not be obtained on the dogfood cluster, so this is a bounded result, not a full clearance.** Across two sustained bursts (`maxListeners` = 48 and 16, `maxWorkers` = 4, non-preemptible `workers-od` ×3) the active-session pool collapsed to ~1 busy worker **both** times — but the collapse was driven by the **agent-recycle registration-conflict seam** (Q259/Q114), **not** the `completejob` tax: the worker-capacity ceiling (`job admission rejected: worker capacity full`) was **never reached** (0 admission rejections in either run), so the completion tax was **never the binding constraint**.
There is therefore **no evidence of an Option A completion-tax throughput wall**, and **Q264 revival is not triggered by the tax**.
The residual throughput blocker is a **fixable AGC recycle-robustness gap**, not an architectural wall.

### Method

- HEAD `cacd4c6` (#523, Option A default-on).
  Built `agc:e2e-cacd4c6` (amd64, `sha256:ec25509…`), deployed via the GMC `AGC_IMAGE` patch with the explicit `AGC_FANOUT_COMPLETION` env **removed** — verifying the *shipped* default-on (confirmed live: warm-up 5×, run 2 2× `completed a deduped sibling delivery via completejob`).
- Capacity = the re-route #4/#5 stable pool: non-preemptible `workers-od` (`e2-standard-4` ×3, on-demand), default-pool 2, spot `workers` pinned to 0/0 (no preemption confound).
  SSD `3×100 + 2×50 + 20 = 420 < 500`.
- The Q265 lever: `maxListeners` set **far above** `maxWorkers × fan-out-width` (48, then 16; fan-out width ≈ 6 per re-route #4) so listener supply could not bottleneck. `maxWorkers = 4` (SSD-bounded — see caveat).
- Load: the same ~7-job concurrent matrix as re-route #5 (`unit-test.yml` 6 jobs + `integration-test.yml`), push-event reruns (concurrency-immune).
- Sampled every 15 s: online runners (≈ active listener sessions), busy runners, worker-pod occupancy (`actions-gateway/plan-id`), + AGC debug-log marker counts.

### Results

| Run | `maxListeners` | peak online (sessions) | peak busy workers | worker-capacity rejections | deregister-conflict fatal seam | `completejob` (Option A) |
|---|---|---|---|---|---|---|
| 1 | 48 | 2 | **1** | **0** | 41 | 0 (no winner completed) |
| 2 | 16 | 3 | **1** | **0** | 38 | 2 |

Both bursts collapsed the active-session pool to 0 within ~1–2 min and never provisioned more than 1 concurrent worker; **neither** reached the `maxWorkers = 4` ceiling.
The dominant signal was recycle churn: `recycle blocked by still-running consumed runner; backing off and retrying` (the Q259 bounded-backoff path) followed by `deregister conflicting runner record "ci-N": runner is still running a job and cannot be deleted` → **fatal listener exit**.

### Mechanism (attributed) — fan-out *slot-stranding*, not `completejob` cost

1. GitHub fans one job to F ≈ 6 sibling deliveries; 1 wins and provisions a worker (job runs for **minutes**), F−1 are deduped losers.
2. A deduped loser immediately tries to **recycle** its single-use slot, but GitHub still considers that runner **assigned to the job** (the Q259 422) until the winner completes **and** Option A fans `completejob` to release the sibling.
3. So each loser slot is 422-blocked for the **winner's entire job runtime**, which **exceeds** the bounded recycle backoff (`registerAgentWithBusyRetry`, tens of seconds, [`pool.go:382`](../../../cmd/agc/internal/agentpool/pool.go)).
   The backoff **exhausts**, the recycle fails, and the listener goroutine **exits** ([`pool.go:344`](../../../cmd/agc/internal/agentpool/pool.go)).
4. Each fanned-out job thus strands and eventually loses F−1 listener slots; under sustained burst this collapses the pool faster than winners complete.

This is a **classic-protocol topology cost** (single-use recycle Q114 + many-acquirers fan-out) that Option A's *accounting* fix does not touch.
It is **fixable AGC-side** (do not eagerly recycle a deduped loser until its winner completes; or hold the slot across the winner's runtime instead of exiting on backoff exhaustion) — a bounded fix, tracked as a new Queue item.
It is **not** a `completejob`-tax wall and does **not** force Option E — though Option E (scale-set, one acquirer, **no** agent recycle) would eliminate this seam *and* the completion tax by construction (a modest long-term point in E's favour, not a forcing one).

### Honest bounds — the measurement is confounded + capacity-bounded

A clean "pool holds at maxWorkers" measurement was **not** achieved:

- **SSD quota caps `maxWorkers` ≈ 4** (500 GB `SSD_TOTAL_GB`; `pd-balanced` disks count against it).
  A 4-worker pool cannot prove no-wall at a *wide* pool; the tax, even shown non-binding here, is untested at `maxWorkers ≫ 4`.
- **Stale runner-record clutter** (47 offline `ci-*` records — prior re-routes + this session's `maxListeners` changes) inflates the 409/422 recycle-conflict rate.
  Cleaning it (mass runner-record delete) was **denied by the write-safety guard** on the shared repo, so a clean-namespace run was impossible in-session. re-route #5 (which *recovered* to 5 sessions) ran on a cleaner namespace, `maxListeners = 8`, longer jobs — consistent with the collapse being *provoked* (not purely inherent) by clutter + over-cranked `maxListeners` + short/failing jobs.
- So the two live possibilities — (a) clutter-only artifact, Option A sufficient with namespace hygiene; (b) a fundamental slot-stranding gap needing the recycle fix — **cannot be distinguished without a clean run**. **Both** agree the tax is not the wall.

### Recommendation

- **Q264 stays DEFERRED.** No completion-tax wall observed; the tax is never the binding constraint.
  Do not start the Option E rewrite on this evidence.
- **Fix the recycle slot-stranding seam** (new Queue item, Q259/Q114 family), then **re-benchmark on a fresh, clean dogfood namespace** at moderate `maxListeners` (≈ 8–16), with `maxWorkers` widened, to obtain the clean "holds at maxWorkers" measurement Q265 set out to get. **Update (re-route #7, 2026-07-05):** the `maxWorkers` widening needs **no SSD-quota bump** — right-sizing the worker boot disk `pd-balanced → pd-standard` takes it off the SSD quota (Q248, done).
  The re-benchmark then confirmed Q266's seam is gone but surfaced the *real* residual: the online-session / broker-credential recycle churn keeps the online idle pool near 0 (see §8 and re-route #7), so the clean measurement is still gated on that seam + a clean namespace.
- **Q224's throughput residual** stays open, now attributed to (a) the recycle seam above and (b) worker capacity ([Q248](../../STATUS.md)) — both tuning/fix, not architectural.

Full live evidence: [`gke-dogfood.md`](gke-dogfood-turnup-findings.md) re-route #6.

## 8. Q266 — the slot-stranding recycle fix (2026-07-04)

The seam attributed in §7 is now fixed AGC-side (Q266), offline-tested, awaiting a live re-benchmark.

**The fix — defer the loser's recycle until its winner completes.** The loser's `422` cannot clear until the winner fans `completejob` out to that delivery (§5, Option A) — so recycling *before* the winner concludes is guaranteed to fail.
Instead of recycling eagerly into that `422`, blowing the bounded backoff, and exiting the listener, a deduped loser now **holds its slot** and waits for the winner's conclusion signal, then recycles in place and resumes polling.
The wait reuses the #512 claim registry: when a loser loses the `planID` claim to a still-running winner, `claimJob` hands back the claim's `WinnerConcluded` channel (closed exactly once when the winner's `Complete` runs), and the loser blocks on it before returning to the recycle path ([`multiplexer.go`](../../../cmd/agc/internal/listener/multiplexer.go), [`goroutine.go`](../../../cmd/agc/internal/listener/goroutine.go)).

Key properties:

- **No net capacity regression.** The deduped-loser goroutines were *already* occupying their listener slots as pollers; holding them blocked (rather than exiting) keeps the slots they had.
  It counts as a poller = **false** while parked (`SetPolling(false)` is set before the job), so a parked loser is never mistaken for available polling capacity.
- **Worker capacity is freed.** A loser provisions no pod, so it **releases its `Admit` worker-capacity reservation before parking** — otherwise F−1 losers would pin the tight `maxWorkers` ceiling ([Q248](../../STATUS.md)) with runners that do nothing.
- **Bounded fallback for a stuck winner.** If the winner never concludes (crash/hang), the wait is capped at `defaultLoserRecycleDeferTimeout` (16 min, just past GitHub's ~15-minute unstarted-job timeout that force-releases the assignment), so a loser slot can never leak.
- **Only under Option A.** The defer applies only when `AGC_FANOUT_COMPLETION` is enabled (the default) — that is what clears the loser's `422`.
  With it off, losers fall back to the eager-recycle path (documented as the worse opt-out).
- **Observability.** Each defer increments `actions_gateway_fanout_loser_recycle_deferred_total{outcome}` — `winner_concluded` (normal), `fallback_timeout` (alert-worthy: stuck winners), `context_cancelled` (shutdown).

**Regression test.** `TestListener_Q266_FanoutLoserDefersRecycleUntilWinnerCompletes` ([`goroutine_q266_test.go`](../../../cmd/agc/internal/listener/goroutine_q266_test.go)) drives a sustained fan-out burst through the `brokertest` fan-out model with a `RecycleAgent` that `422`s until the winner concludes (the live mechanism), and asserts the pool **holds** — no loser strands+exits while the winner runs, and each loser recycles in place on the winner's conclusion.
It FAILS against pre-Q266 behaviour (the eager losers exit) and needs no GKE turn-up.

**Live re-benchmark (2026-07-05, [`gke-dogfood.md`](gke-dogfood-turnup-findings.md) re-route #7).** Q266's targeted seam is **confirmed eliminated live**: at moderate `maxListeners = 12` the fatal `deregister conflicting`/`recycle blocked` listener exits that collapsed the pool in §7 (41/38) were **0**; deduped losers **park** (busy-at-GitHub, pod-less) instead of exiting; Option A `completejob` (5) and dedup (7) fired. **But full-matrix green / "holds at `maxWorkers`" was STILL not obtained.** The residual is neither Q266's seam nor the `completejob` tax (0 `worker capacity full`) — it is a **two-way bind**: throughput needs `maxListeners ≈ maxWorkers × fan-out`, yet a wide `maxListeners` (48) multiplies GitHub runner records and inflates the **broker-credential / registration recycle churn** (Q259/Q114 — `"Registration … was not found"`) that keeps the **online idle pool near 0**, collapsing to `online = 0`; a moderate `maxListeners` (12) is stable but serializes to ≈ `maxListeners / fan-out ≈ 2` concurrent jobs.
Un-cleanable stale records (guard-blocked mass-delete) compound it.
A clean measurement needs the **online-session / broker-credential recycle seam** fixed *and* a clean namespace — both still blocked in-session, so [Q224](../../STATUS.md) full-matrix green **cannot yet be claimed**.
Separately, the `maxWorkers ≈ 4` SSD ceiling §7's honest-bounds flagged is **resolved** — not via an SSD-quota bump but by right-sizing the worker boot disk to `pd-standard` (off the SSD quota entirely), see [`dogfood-runner-rightsizing.md`](../dogfood-runner-rightsizing.md#node-pool-disk-class-the-real-maxworkers-ceiling-q248-2026-07-05).

## 9. Re-route #8 — Q267 confirmed, and the residual isolated to fan-out *dispatch* (2026-07-05)

The clean-namespace wide-pool close-out ([`gke-dogfood.md`](gke-dogfood-turnup-findings.md) re-route #8, `agc:e2e-63cddfc`, fresh `dogfood8`/`ci8`/`gag-ci8`, `maxListeners = 48`, `maxWorkers = 8`, non-preemptible `pd-standard` capacity, no mid-run restart) closes out the seam accounting from re-route #7:

- **The broker-credential recycle collapse seam is GONE.** At the exact `maxListeners = 48` that collapsed #7's pool to `online = 0`, the pool **held** for a 20-minute window with **0** token-400 (`"Registration … was not found"`), **0** Q267 ride-out retries, **0** fatal `deregister conflicting` exits, **0** `worker capacity full`.
  (Nuance: the token-400 *condition never arose* — 0 occurrences — because Q266 parks losers instead of recycling and the clean namespace removed the stale-record amplifier; so the wide-pool hold is a property of the **Q266 + Q267 + clean-ns** stack, and Q267's retry path stays covered by its offline repro, not exercised live.) **Q267: DONE.**

- **Option A's accounting is correct — but fan-out *dispatch* starves distinct jobs.** The AGC received **only 2 distinct planIDs**; both ran and concluded **green**.
  GitHub fanned **one** planID out as ~6 sibling deliveries (all deduped + `completejob`-released, correctly), but the **other 5 jobs' planIDs were never delivered** while GitHub marked those jobs `in_progress` on the recycled stable-named runners — leaving them wedged `in_progress` **indefinitely** (run-level status froze `completed/success` while the jobs API stayed `in_progress` >1h).
  The online-idle pool **stalled at 3 sessions ≪ 48** because a duplicate delivery does not grow the demand-driven 1:1 replacement pool.
  This **refines §5's "reconcilable AGC-side"**: the *completion* accounting is reconcilable (a job that *receives its planID* concludes green), but the fan-out *dispatch* stochastically starves distinct jobs (#5 got 3/7 green, #8 got 2/7 with 5 wedged), so a **reliable** full-matrix green is **not** achievable on the classic many-acquirers protocol.

- **Consequence for Q264.** This is a real throughput/assignment **wall** — distinct from the `completejob`-tax wall §7 ruled out (0 capacity rejections) — driven by GitHub's server-side fan-out assignment against GAG's many-acquirers + stable-name single-use ([Q114](../../STATUS.md)) topology.
  It **strengthens the [Option E / Q264](../q264-scale-set-protocol.md) case** (one acquirer, one authoritative stream, no sibling deliveries, no per-name recycle) — which eliminates the class by construction — though Q264 stays a deferred v-next decision, not force-triggered. **Q224/Q242 stay open**, now blocked on the fan-out dispatch topology, not on any recycle/capacity/tax seam (all resolved).
  Evidence: AGC debug logs (`agc:e2e-63cddfc`), reruns `28734640377`/`28734640415` (burst `08:50:22Z`).

- **AGC-side escape-hatch spike — none found (2026-07-05).** #8's "Option E is the structural fix" conclusion was stress-tested for an AGC-side lever before the Q264 go/no-go, in [`q224-fanout-dispatch-lever-spike.md`](../q224-fanout-dispatch-lever-spike.md): unique/ephemeral names are a non-lever (add no distinct idle sessions; #8 orphaning is runner-id churn), and a warm idle **listener** baseline (≠ Q261 warm *worker* pods) is at best a probabilistic green-rate stopgap that converges on a dominated reimplementation of the scale-set model. **Verdict: Option E ([Q264](../q264-scale-set-protocol.md)) is the only reliable fix — #530 stands.**

## 10. Q265 — close-out: comparison settled, ScaleSet decisively wins (2026-07-06)

Q265 asked for a head-to-head fan-out (Option A / classic) vs scale-set throughput benchmark at a wide worker pool. **Both sides have now been measured live on the dogfood cluster under the clean conditions Q265 required — the two confounders that previously blocked a clean "holds at `maxWorkers`" run (the Q266 loser-slot-stranding seam §8 and the Q267 online-session / broker-credential recycle seam §9) are fixed, and the SSD `maxWorkers` cap is gone (workers on `pd-standard`, [Q248](../../STATUS.md)).
No further turn-up is needed: the comparison is decided.**

### The measured comparison

| Path | Config | Distinct jobs run to green | Binding limit |
|---|---|---|---|
| **Classic / Option A fan-out** | re-route #8: clean namespace (`dogfood8`/`ci8`/`gag-ci8`), `maxListeners = 48`, `maxWorkers = 8`, `pd-standard`, all recycle/capacity/tax seams (Q259/Q266/Q267/Q248/Q265) quiet | **2/7** — 5 jobs wedged `in_progress` indefinitely | GitHub server-side **fan-out distinct-delivery starvation** (§9): only 2 distinct planIDs ever delivered; the pool self-limited to 3 ≪ 48 sessions, so a wider pool does not help |
| **ScaleSet** ([Q264](../q264-scale-set-protocol.md) P4) | clean-green re-run: fresh scale-set label, `maxWorkers = 8`, same 7-job matrix | **7/7** — 7 distinct `JobAssigned`, 7 worker pods in ~2 s, 0 dedup / 0 wedge | none — one acquirer, one authoritative queue, no sibling deliveries |

The clean wide-pool run Q265 set out to obtain **is** re-route #8: with Q266 + Q267 + a clean namespace + `pd-standard`, the pool *held* (Q267 done, 0 collapse markers over a 20-minute window) — proving the classic **2/7** ceiling is **not** a recycle-churn or `completejob`-tax artifact but the intrinsic many-acquirers **dispatch** wall.
ScaleSet clears exactly that wall by construction.

### Verdict — Q265 DONE

- **No completion-tax throughput wall exists** (re-route #6, §7: 0 `worker capacity full` in every run) — Option A's *accounting* is correct.
- **But classic cannot reliably clear a high-concurrency burst:** fan-out distinct-delivery starvation caps it at 2–3/7 regardless of pool width — a GitHub server-side limit with **no AGC-side lever** ([`q224-fanout-dispatch-lever-spike.md`](../q224-fanout-dispatch-lever-spike.md)).
- **ScaleSet clears it decisively: 7/7 vs classic 2/7**, proven live.

**Strategic close.** [Q264](../q264-scale-set-protocol.md) P5 has already flipped the default acquisition protocol to ScaleSet (PR #553); classic — the Option A fan-out path — is **deprecated**.
This benchmark therefore **closes the comparison and confirms the ScaleSet default decision**; it is *not* a reason to revive or further tune Option A. Option A remains correct and supported for the deprecated classic protocol (its accounting fix stands unchanged), but the throughput ceiling measured here is exactly why the product front door is now ScaleSet.

A fresh confirming turn-up was **not** run: both sides are already measured under clean, confounder-free conditions on the shared dogfood cluster, and re-running the deprecated classic path would only reproduce **2/7** at cost.
Evidence: [`gke-dogfood.md`](gke-dogfood-turnup-findings.md) re-route #8 (classic **2/7**) + Q264 P4 clean-green (ScaleSet **7/7**).

---

## Ruled-out, for the record

- **#513 completejob-abandon (immediate loser `skipped`)** — live-tested worse than the default (indefinite `in_progress`).
  OFF.
  See Option B.
- **Another dedup key** — planID is correct and live-validated.
  See Option D.
- **Message-ack (`POST /acknowledge`)** — Investigation A confirmed not required for delivery; not the bug.
