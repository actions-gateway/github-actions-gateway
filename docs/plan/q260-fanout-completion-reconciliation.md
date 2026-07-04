# Q260 — reconcile GitHub's per-delivery fan-out with AGC's one-runner-per-session model

**Status:** design + a cheap, deterministic offline repro (this PR). The repro
(`TestAGC_Q260_FanoutCompletionReconciles`, skipped) **fails today** and gates the
eventual fix; it needs no GKE turn-up. The production reconciliation fix is **not**
implemented here — it is designed below and left as a fast follow, because its one
load-bearing assumption (does GitHub honour a non-running delivery's completion?) is
only answerable by a live re-route #5. **Q224/Q260/Q242 stay open.**

This is the last blocker for Q224 "route production CI green," after the earlier
Q260 work closed capacity (Q248), Secret/Pod collisions (#512), and the planID
dedup key ([`q260-planid-dedup-refix.md`](q260-planid-dedup-refix.md)). Full live
evidence: [`gke-dogfood.md`](gke-dogfood.md) re-route #4 (2026-07-04).

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
  - `TestAGC_Q260_FanoutCompletionReconciles` (**skipped**, the fix gate) asserts the
    job concludes **`completed`**. It **fails today** (`completed` vs `cancelled`) and
    passes once the reconciliation lands — validating the fix with no turn-up.

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
`in_progress`** — strictly worse than a terminal cancel. Keep
`AGC_COMPLETE_ABANDONED_DELIVERIES` **OFF**. (The mechanism stays in the tree, off,
because Option A reuses the same `broker.CompleteJob` plumbing — just deferred and
fanned to all siblings rather than fired immediately per loser.)

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

**Recommendation: Option A**, behind the existing flag (renamed/rescoped from the
per-loser `AGC_COMPLETE_ABANDONED_DELIVERIES` to a winner-driven fan-out completion),
**off by default** until re-route #5 confirms §5.

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

### Re-route #5 must confirm

1. Enable Option A (winner fans `completejob` to all siblings on completion) via the
   `AGC_EXTRA_*` passthrough (no GMC code change — see re-route #4 note in
   [`gke-dogfood.md`](gke-dogfood.md)).
2. Fire the concurrent matrix on stable capacity (already fixed).
3. Capture, per fanned-out job: every `completejob` request/response (result value +
   status), whether the previously-cancelled jobs (`tidy-check`, etc.) now **conclude
   completed**, and whether the Q259 recycle 422 clears (GitHub no longer considers
   the deduped runners assigned).
4. **If a real result concludes the job but `skipped` does not** → Option A with the
   pod-phase proxy is the fix; wire the result and flip the flag on by default.
5. **If even resolving all siblings does not conclude the job** (most-recent-delivery
   authority) → **STOP**: the gap is GitHub-server-side, and Q224's "concurrent
   matrix green" is infeasible via GAG's *many-acquirers* model without GitHub changing
   its fan-out reconciliation. Reframe Q224 feasibility and record it — **Option E
   (single-acquirer topology) becomes the path**, at the cost of the parallel-acquire
   density model.

Also fix the orthogonal Q239 regression before #5 (the dogfood `RunnerTemplate`
reverted to the toolchain-less upstream image — `make: command not found`), so a
non-green result is attributable to accounting, not the runner image
([`gke-dogfood.md`](gke-dogfood.md) re-route #4 secondary observation).

---

## 6. Test strategy

- **Offline gate (this PR):** the fan-out accounting model + the two listener tests
  in §3. The skipped `…Reconciles` test is the fix's acceptance gate — remove the
  `t.Skip` when Option A lands; it must go green with no turn-up.
- **When implementing Option A:** add a listener/multiplexer unit test asserting the
  winner issues one `completejob` per deduped sibling on completion (and one for a
  late redelivery during the linger window), keyed on each sibling's own
  `RunnerRequestID`; extend the envtest to assert the deduped losers' deliveries are
  resolved (not merely skipped-and-forgotten).
- **Live (re-route #5):** §5. This is the only step that cannot be done offline, and
  it is the go/no-go for Q224.

---

## Ruled-out, for the record

- **#513 completejob-abandon (immediate loser `skipped`)** — live-tested worse than
  the default (indefinite `in_progress`). OFF. See Option B.
- **Another dedup key** — planID is correct and live-validated. See Option D.
- **Message-ack (`POST /acknowledge`)** — Investigation A confirmed not required for
  delivery; not the bug.
