# Q260 — reconcile GitHub's per-delivery fan-out with AGC's one-runner-per-session model

**Status:** **Option A live-confirmed by re-route #5 (2026-07-04) and flipped ON by
default (`AGC_FANOUT_COMPLETION`, opt out with `=false`). Q260 DONE; Q224's fan-out
blocker cleared (a pristine full-matrix green is now gated only on `maxWorkers`
worker-capacity tuning, Q248 — not the accounting gap).** The winner
of a fanned-out job tracks every deduped sibling delivery and, on completion, fans a
`completejob` out to each (keyed on the sibling's own `RunnerRequestID`, with the
winner's pod-phase-proxy result); a late redelivery within the linger window is
resolved with the recorded terminal result. The one load-bearing assumption — does
GitHub honour a non-running delivery's completion, and does resolving *all* siblings
conclude the job? — is **confirmed YES**: re-route #5 observed `completejob` on a live
sibling's own job ID return **OK** (not "already resolved"), the winner's own delivery
carry the real workflow result, previously-wedged concurrent jobs conclude **green**,
the Q259 recycle 422 clear per job on winner completion, and **no** job cancel at the
~15-minute timeout. Completion is **per-delivery, not planID-scoped**, so the
secure-by-default concern (a green sibling proxy masking a red workflow) does not
arise — the flag is on by default. `TestAGC_Q260_FanoutCompletionReconciles` (the
gate) passes; the companion `…AccountingGap` (flag off) still asserts the pre-fix wedge.

This closed the last blocker for Q224 "route production CI green," after the earlier
Q260 work closed capacity (Q248), Secret/Pod collisions (#512), and the planID
dedup key ([`q260-planid-dedup-refix.md`](q260-planid-dedup-refix.md)). Full live
evidence: [`gke-dogfood.md`](gke-dogfood.md) re-route #4 (fails-today control) +
re-route #5 (Option A confirmed).

---

## 1. The protocol, from the code and the live evidence

Three id spaces are involved. Keeping them distinct is the whole game:

| id | source | scope | in the code |
|---|---|---|---|
| `RunnerRequestID` | broker `GET /message` body ([`RunnerJobRequestBody`](../../broker/types.go)) | **one per delivery** — distinct per sibling under fan-out | `jobBody.RunnerRequestID`; sent as `jobMessageId` (acquire) and `jobId` (renew/complete) |
| `planID` | `POST /acquirejob` **response** (`resp.Plan.PlanID`) | **one per logical job** — shared across all sibling deliveries | `provisioner`'s `job-<planID>` Secret/Pod name; the dedup key (#503→#512) |
| `sessionID` | `POST /session` | one per listener goroutine | the long-poll identity |

### What GitHub does (server behaviour — confirmed live)

Under a concurrent burst, GitHub's broker **fans one logical job (one `planID`) out
to N sibling sessions** as N separate `RunnerJobRequest` messages with **distinct
`RunnerRequestID`s**. Each sibling's `POST /acquirejob` **succeeds** and returns the
**same `planID`** (re-route #2/#4: 5 sessions all provisioning `job-<samePlanID>`).
So GitHub does **not** enforce single-acquisition per job — `acquirejob` is the
atomic claim of a **delivery**, not of the job (consistent with Investigation A,
[`03-api-contracts.md`](../design/03-api-contracts.md#33-re-implemented-broker-api-endpoints):
"`acquirejob` alone is the atomic claim", and `POST /acknowledge` is **not required**
for correct delivery — so the missing message-ack is *not* the bug).

Each delivery is an independent **assignment** on GitHub's side. GitHub expects each
acquired assignment to either be renewed + completed, or it reclaims it: an
assignment acquired but not started/renewed is **cancelled at the ~15-minute
unstarted-job timeout**, and a stale assignment triggers **redelivery** to another
session (the Q247 mechanism: "outlived the lock TTL → GitHub redelivered to a sibling").

Two more server facts, confirmed live in re-route #4:

- Completing **one** sibling delivery does **not** conclude the logical job. The
  winner's worker pod completes its own delivery with a real result, yet GitHub held
  the job `in_progress` / cancelled it.
- `completejob(result=skipped)` on a sibling returns **HTTP-OK but does not conclude
  the job** (14/15 OK; 1× `401 "Not authorized for this job"`). It merely acks that
  one delivery.
- The Q259 `422 "runner … is currently running a job and cannot be deleted"` on
  recycle is the same accounting seam from the runner side: GitHub still considers a
  deduped-away runner **assigned to the job**.

### What the AGC does (our behaviour — from the code)

`handleJob` ([`goroutine.go`](../../cmd/agc/internal/listener/goroutine.go)) per delivery:

1. `AcquireJob(jobMessageId = RunnerRequestID)` → learns `planID` + a per-delivery
   job token (`AcquireJobResponse.JobAuthToken`).
2. **Dedup gate** `ClaimJob(planID)` (post-acquire, #512): the **first** sibling to
   claim the `planID` **wins** and provisions; the **losers** skip provisioning and
   `return acquired=true` so their runner recycles. The claim lingers for
   `completedPodTTL` past completion so a late redelivery is deduped too.
3. Winner only: `SpawnReplacement` → `StartRenewLoop(jobId = RunnerRequestID)` →
   `JobHandler` (provisions the worker Pod; the worker's runner binary makes the
   winner delivery's `completejob`).

Critically: the loser path does **nothing** to its acquired delivery (default), and
the **JobHandler never learns the job's real succeeded/failed result** — the
provisioner returns `nil` even on `PodFailed`
([`provisioner.go`](../../cmd/agc/internal/provisioner/provisioner.go), step 5–7); only
the worker's runner binary reports the real result, and only for the winner delivery.

### Why this is GAG-specific — ARC has one acquirer

The gap is a consequence of GAG's **topology**, not a defect in GitHub's protocol.
The offer-to-many / redeliver-on-stale dispatch is an inherent, by-design race:
GitHub offers a queued job to any eligible idle session and treats `acquirejob` as
the claim, assuming each acquired delivery is either run-to-completion or reclaimed.

Modern ARC (`gha-runner-scale-set`) never surfaces it because it runs **one**
`Runner.Listener` per scale set
([`01-executive-summary.md`](../design/01-executive-summary.md),
[`appendix-f-cost-model.md`](../design/appendix-f-cost-model.md)): that single listener
acquires each job **once**, then spins a dedicated ephemeral pod to run it — a strict
1:1 acquire-to-run with **no sibling deliveries to reconcile**. ARC separates
*acquire* (one listener) from *run* (a fresh pod).

GAG instead runs a permanent baseline plus up to `maxListeners` concurrent
long-polling sessions per RunnerGroup, **each independently able to `acquirejob`**
([`02-architecture.md`](../design/02-architecture.md)). That is the ~60 KiB/session
virtual-runner model GAG is built on — and it is exactly what lets GitHub hand one
job to several sessions, all acquire it (shared planID), and leave N assignments for
one logical job. **The fan-out is intrinsic to the many-acquirers topology**; the
dedup and this completion reconciliation are the tax that topology pays. Option E
treats the cause instead of the symptom.

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

**The gap:** the AGC collapses N deliveries to one *runner*, but does nothing to
collapse them on *GitHub's* books. The winner resolves exactly **one** of N
assignments; the other N−1 are acquired-then-silently-abandoned, and each is a live
assignment GitHub is still waiting on. At the unstarted-job timeout the dangling
assignments cancel the whole job — the winner's completion of its own delivery does
not save it, because GitHub tracks completion **per delivery**, and N−1 deliveries
never reported anything.

This is **distinct from and beyond** the dedup (which is correct and done) and
distinct from Q259 (the recycle 422 is the same seam observed from the runner side).

---

## 3. Why envtest passed while production wedged — and the repro that fixes that

The default `brokertest.Server` modeled **neither** the fan-out **nor** per-delivery
completion: every `acquirejob` returned a fresh `test-plan-N`, and `completejob` was
just a counter. The Q260 envtest
([`q260_duplicate_delivery_test.go`](../../cmd/agc/internal/controller/integration/q260_duplicate_delivery_test.go))
forced a constant planID and asserted the **dedup** — then had the winner **block
forever** in `waitForCompletion`. It never modeled a job *concluding*, so the
accounting gap was invisible. That is exactly why the dedup looked green offline yet
wedged live.

**Repro landed in this PR** (opt-in, so existing tests are unaffected):

- `broker/brokertest/server.go` gains a **fan-out job-accounting model**
  (`EnableFanoutAccounting`, `EnqueueFanoutJob`, `JobState`,
  `ExpireUnstartedDeliveries`). It tracks a logical job per planID with one
  assignment per delivery, and encodes the invariant observed live: **a job
  concludes only when every acquired delivery is resolved with a consistent real
  (non-`skipped`) result; any acquired-but-unresolved delivery cancels the job at the
  unstarted timeout; a `skipped`-only job never goes green** (matching the #513
  flag-ON result).
- `cmd/agc/internal/listener/q260_fanout_accounting_test.go`:
  - `TestAGC_Q260_FanoutCompletionAccountingGap` (**green today**) drives the real
    dedup path against the model — winner completes its own delivery, siblings dedup
    — and asserts the job ends up **`cancelled`** at the timeout. This locks the
    wedge in as a deterministic, offline regression.
  - `TestAGC_Q260_FanoutCompletionReconciles` (the fix gate, **now un-skipped and
    green** with the flag on) asserts the job concludes **`completed`**. It failed
    against pre-fix code (`completed` vs `cancelled`) and passes now that Option A
    lands — validating the fix with no turn-up.

Model assumptions and their grounding are documented inline in `server.go`; the one
assumption the model *cannot* prove is §5's live-only unknown.

---

## 4. Design options

The AGC can only reach GitHub through the calls it already has (`acquirejob`,
`renewjob`, `completejob`, `deletesession`) — there is **no** "reject/un-acquire"
call. Once a loser has acquired, its assignment exists; the only levers are *how* and
*when* to resolve it.

### Option A — Winner fans completion out to all sibling deliveries (recommended)

Track, per `planID` claim, every deduped sibling delivery
`(RunnerRequestID, runServiceURL, jobToken)`. When the winner's job **finishes**,
the AGC issues `completejob` for **each** sibling delivery (and any late redelivery
that arrives during the linger window) so **no** assignment dangles. Losers do
**not** complete early.

- **Why it's not #513:** #513 completed the loser **immediately** on dedup, with
  `skipped`, **before** the job ran. That (a) acked the assignment with a non-result
  and (b) raced the winner. Option A completes siblings **after** the winner's real
  completion, so whichever delivery GitHub treats as authoritative gets resolved at
  the right time, and a planID-scoped completion (if that is how GitHub works) is
  harmless because the job is already done.
- **The result to report:** the AGC does **not** know the workflow's real
  succeeded/failed (§1). Options, in order of preference pending §5:
  1. **`skipped`** for the siblings — honest (they ran nothing) and lowest blast
     radius if completion is planID-scoped, but re-route #4 showed `skipped` does not
     *conclude* a delivery GitHub is waiting on. Likely insufficient alone.
  2. Report the **winner's pod phase** proxied to `succeeded`/`failed`
     (`PodFailed`→`failed`, else `succeeded`) — richer, but risks greening a red job
     **iff** completion is planID-scoped (a red workflow whose worker exited 0). Gate
     behind the flag; secure-by-default keeps it off until §5 clears it.
- **Cost:** N−1 extra `completejob` calls per fanned-out job, bounded by fan-out
  width; negligible against the hourly budget.
- **Late redelivery:** the linger claim (#512) already dedups a redelivery arriving
  after completion; extend the claim registry to remember the planID's terminal
  outcome for the linger window so the late delivery is resolved with the same result
  rather than left dangling.

### Option B — Loser abandons its own delivery immediately (the #513 path)

**Rejected — settled dead-end.** Live-tested in re-route #4: `completejob(skipped)`
immediately returns OK but does **not** conclude the job; worse, by acking the
delivery it **suppresses** the unstarted-timeout that would otherwise resolve it, so
a late redelivery re-assigns the already-run job and GitHub holds it **indefinitely
`in_progress`** — strictly worse than a terminal cancel. The per-loser-immediate
path is therefore **removed/unreachable**: Option A reuses the same
`broker.CompleteJob` plumbing but fires it from the **winner**, deferred to job
completion and fanned to all siblings, under the rescoped `AGC_FANOUT_COMPLETION`
flag — never per-loser at dedup time.

### Option C — Reduce fan-out width

**Rejected.** Fan-out is GitHub minting multiple messages for one job; it is inherent
to a multi-session pool and cannot be eliminated without serialising the pool (kills
throughput, the Q248 regression). Narrowing `maxListeners` only narrows, never
closes, the window.

### Option D — Another dedup key / pre-acquire dedup

**Rejected — settled.** planID is the correct key (#512, live-validated 0
collisions). The problem is completion accounting, not acquisition dedup. No new key
helps.

### Option E — single-acquirer topology / adopt the runner-scale-set protocol (treat the cause)

**Deferred — a v-next architectural pivot; the fallback if Option A proves infeasible
(§5).** The fan-out exists only because GAG runs *many* acquiring sessions per group on
the classic per-runner broker protocol, where **concurrency = number of registered
runners = number of acquirers**. So "one acquirer" and "N concurrent jobs" are mutually
exclusive on this protocol: a single classic session is a single runner and would
**serialize the whole group to one job at a time** (and there is no pre-acquire
single-flight key — planID is post-acquire and the message's `RunnerRequestID` differs
per sibling; the entire Q260 history). Getting one acquirer *with* concurrency requires
GitHub's **runner-scale-set message-queue protocol** (what ARC uses): one listener
long-polls a single job stream, `acquireJobs` claims a **batch**, and GAG creates one
worker pod per acquired job. One authoritative stream ⇒ no sibling deliveries ⇒ the
Q260 / Q247-completion / Q259-recycle class is eliminated **by construction**, not
reconciled. GAG would still beat ARC on footprint (a Go listener goroutine vs a
~256 MiB .NET process) and keep egress isolation + on-demand workers — "ARC's protocol,
GAG's efficiency."

**Cost:** a large rewrite of the acquisition tier and a partial redefinition of GAG. It
discards most of the classic-protocol machinery (per-agent JIT session model, agent
pool + single-use recycle Q114, the Multiplexer, the Q260 dedup, the Q247
renew-by-`RunnerRequestID` path); it means reverse-engineering and depending on a
**second** GitHub-internal protocol; it reworks registration/auth (register a scale
set, not N agents) and re-expresses the admission gate (Q59) + `priorityTiers` against a
dispatch model; and it collapses each group's acquisition to a **single point of
failure** (vs today's N independent sessions, each with its own Q137 revival). It also
retires the "thousands of goroutine-backed virtual runners" identity — density at rest
actually *improves* (one listener/group, not N), but the story becomes "a
lighter-weight ARC listener" rather than "cheap virtual runners." Revisit only if §5
rules Option A out.

**Recommendation: Option A** — **implemented**, behind the flag renamed/rescoped from
the per-loser `AGC_COMPLETE_ABANDONED_DELIVERIES` to the winner-driven
`AGC_FANOUT_COMPLETION`, **off by default** until re-route #5 confirms §5.

---

## 5. The one live-only unknown, and the feasibility caveat

Option A rests on a single unproven assumption:

> **Does `completejob` on a sibling delivery that never ran the job cause GitHub to
> stop waiting on that assignment — and does resolving *all* assignments let the job
> conclude green?**

Re-route #4 proved `skipped` **acks** a delivery (HTTP-OK) but does **not conclude**
the logical job. It did **not** test (a) resolving **every** sibling, nor (b) a
**real** result (`succeeded`/`failed`) on a sibling. Those are the open questions.

**Feasibility honesty (per the task's stop-condition):** there is a real chance this
is **not** fully reconcilable AGC-side. If GitHub always treats the **most-recent**
delivery as the job's authoritative assignment and **ignores older deliveries'
completions**, then new redeliveries keep arriving faster than the AGC can resolve
them, and no amount of sibling-completion converges — the fix would then require a
GitHub-server behaviour we cannot influence. The re-route #4 evidence (a late
redelivery re-assigning an already-run job) is *consistent with* that worst case, but
does not prove it, because #4 only ever completed siblings with `skipped`. This is
why the design ships behind a flag with a decisive live experiment rather than as a
default: **one re-route #5 settles feasibility.**

### Re-route #5 confirmed (2026-07-04) — GO

Enabled Option A by setting `AGC_EXTRA_AGC_FANOUT_COMPLETION=true` on the GMC pod (which
forwards `AGC_FANOUT_COMPLETION=true` to the AGC Deployments — no GMC code change), on a
fresh `agc:e2e-238b8df` (includes #521), on the re-route #4 stable capacity (non-preemptible
`workers-od` ×3 + default-pool 2), `spec.logLevel: debug`. Fired the same ~7-job concurrent
matrix (unit-test + integration reruns on sha 238b8df, green on GitHub-hosted). Observations:

1. **`completejob` on a live sibling returns OK** — at 16:37:07 a fanned-out job (planID
   `357b6d9e`, winner on ci-0) whose winner completed *naturally* fanned `completejob` out
   to **both** deduped siblings (jobIDs `34ad8db4` on ci-2, `f968c752` on ci-4) → **both
   `completed a deduped sibling delivery via completejob`** (HTTP OK), **not** "already
   resolved". So GitHub **accepts** the completion of a sibling delivery that never ran the
   job. (A sibling whose winner had been *concurrency-cancelled* by an unrelated Dependabot
   rebase returned "already resolved server-side" — GitHub had already torn that delivery
   down — which is why the winner must complete naturally for the clean signal.)
2. **Previously-wedged concurrent jobs conclude green** — `coverage` and `integration-test`
   (fanned-out) concluded **success**; all 6 unit jobs eventually ran (none stranded at the
   unstarted timeout once the pool recovered).
3. **Durability** — `coverage` stayed `success` past 16:47, i.e. beyond the ~15-minute
   unstarted-timeout of its siblings (acquired ~16:31) — the exact point re-route #4's
   winner-completed jobs were cancelled. Option A prevented the cancel.
4. **Q259 recycle 422 clears per job** — the "runner … is still running a job and cannot be
   deleted" churn dropped ~12× once winners began completing and fanning `completejob`; the
   pool recovered from a collapsed 2 sessions back toward `maxListeners`, draining the
   backlog. (The 422 is a *rolling* transient per job's in-flight siblings, cleared on that
   job's winner completion — not a permanent wedge.)

**Verdict: GO (design point 4).** Completion is **per-delivery, not planID-scoped**:
`completejob` on a sibling's own job ID resolves only that assignment, and the winner's own
delivery still carries the real workflow result reported by its runner binary — so the
pod-phase proxy on siblings cannot green a red workflow. The secure-by-default gate is
therefore cleared and the flag is flipped **on by default** (opt out `AGC_FANOUT_COMPLETION=false`).
Option E (Q264) is **not needed** — the many-acquirers topology is reconcilable AGC-side.

Confound handled: a Dependabot rebase merge-train briefly polluted the shared runner pool
with pull_request runs that concurrency-cancelled on each force-push (cancels in ~4 min,
distinct from the 15-min accounting timeout). The clean signal came from the **push**-event
238b8df reruns, which are concurrency-immune. Full evidence: [`gke-dogfood.md`](gke-dogfood.md)
re-route #5.

The alternative outcomes the experiment was designed to distinguish, for the record:
- **`skipped` acks but a real result concludes** → wire the pod-phase proxy (already the
  default in #521). Confirmed: the winner's real result concludes the job; siblings only
  need releasing.
- **Even resolving all siblings does not conclude the job** (most-recent-delivery authority)
  → would have been a **NO-GO**: gap GitHub-server-side, Q224 infeasible via many-acquirers,
  **Option E (single-acquirer topology)** the path. **This did not occur** — resolving all
  siblings concludes the job.

Also fix the orthogonal Q239 regression before #5 (the dogfood `RunnerTemplate`
reverted to the toolchain-less upstream image — `make: command not found`), so a
non-green result is attributable to accounting, not the runner image
([`gke-dogfood.md`](gke-dogfood.md) re-route #4 secondary observation).

---

## 6. Test strategy

- **Offline gate — ✅ green.** The fan-out accounting model + the two listener tests
  in §3. `TestAGC_Q260_FanoutCompletionReconciles` (the acceptance gate, `t.Skip`
  removed) now passes with the flag on; `…AccountingGap` (flag off) still asserts the
  cancelled wedge. No turn-up needed.
- **Option A unit/envtest coverage — ✅ landed.**
  - `TestAGC_Q260_WinnerCompletesEachSibling` (listener): the winner issues exactly
    one `completejob` per deduped sibling, keyed on the sibling's own
    `RunnerRequestID`, with the pod-phase-proxy result.
  - `TestMultiplexer_FanoutClaim_TracksSiblingsAndLateRedelivery` (claim registry):
    siblings are registered and returned to the winner on `Complete`; a late
    redelivery within the linger window is handed the recorded terminal result.
  - `TestAGC_Q260_WinnerCompletesDedupedSiblingDelivery` (envtest): the deduped
    sibling's delivery is *resolved* by the winner on completion (keyed on its own
    `jobId`, result `succeeded` from the winner's Succeeded pod), not
    skipped-and-forgotten; it is **not** completed while the winner is still running.
  - `TestProvisioner_ResultPodPhaseProxy` (provisioner): `PodFailed`→`failed`, else
    `succeeded`.
- **Live (re-route #5) — ✅ GO (2026-07-04).** §5. The only step that could not be done
  offline, and the go/no-go for Q224. Confirmed: `completejob` on a live sibling returns
  OK (9/9, 0 failures across 13 fan-outs), fanned-out jobs conclude green, the job
  survives past the 15-minute sibling timeout, and the Q259 recycle 422 clears per job.
  Completion is per-delivery, not planID-scoped. Flag flipped on by default.

---

## Ruled-out, for the record

- **#513 completejob-abandon (immediate loser `skipped`)** — live-tested worse than
  the default (indefinite `in_progress`). OFF. See Option B.
- **Another dedup key** — planID is correct and live-validated. See Option D.
- **Message-ack (`POST /acknowledge`)** — Investigation A confirmed not required for
  delivery; not the bug.
