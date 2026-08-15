# Q224 — AGC-side lever spike: can we beat GitHub's fan-out distinct-delivery starvation without Option E?

**Status:** spike **complete**, no code changed.
This is the escape-hatch check the [re-route #8 conclusion](archive/gke-dogfood-turnup-findings.md) invited: #8 isolated the last Q224 blocker to **GitHub's server-side fan-out distinct-delivery starvation** on GAG's many-acquirers topology and called [Option E / Q264](q264-scale-set-protocol.md) the structural fix.
Before the user commits to (or declines) the Q264 scale-set rewrite, this spike tests two proposed AGC-side levers against the mechanism and the existing live evidence.

**Verdict up front: no AGC-side lever provides a *reliable* fix; #530/§5 stands — [Option E (Q264)](q264-scale-set-protocol.md) is the only structural fix.**

- **H2 (unique/ephemeral runner names): NON-LEVER (reasoned, high confidence).** Renaming the same reactively-grown slots adds **zero** distinct idle runners — the actual binding constraint — while defeating the [Q114](../STATUS.md) recycle optimization and worsening the stale-runner-record clutter that already confounds re-routes.
  The #8 orphaning is runner-**id** churn on recycle, not name reuse; unique names do not fix it and can worsen it.
- **H1 (warm idle *listener* baseline): at best a probabilistic stopgap, not a fix — and its efficacy is unconfirmed (the #8 data leans *against* it).** Note the reframe: the lever the dispatch problem needs is a warm baseline of idle **long-poll listeners** (a *new* `minIdleListeners`-style knob), **not** Q261/[G.12](../design/appendix-g-future-enhancements.md#g12-warm-worker-pool-minidleworkers) `minIdleWorkers` (warm worker **pods**), which presents no extra idle runners to GitHub and so does nothing for dispatch starvation.
  Even in its favourable case, H1 converges on reimplementing the scale-set capacity model on top of the fan-out-prone offer-to-many protocol, *without* the single authoritative stream or capacity header — strictly dominated by Option E.
- **Combination H1+H2** = "a warm baseline of unique ephemeral non-recycling runners" = a poor-man's scale set on the wrong protocol.
  Dominated by Option E.

**Recommendation: do not invest in H1/H2 as the Q224 fix.
Proceed to the Q264 go/no-go.** H1 (the warm-listener-baseline form only) may be worth a small, flagged, opt-in *stopgap* to raise dogfood green-*rate* while Q264 is evaluated — but only if a live probe (§5, designed and ready, not run this session) first confirms a warm-wide classic pool actually spreads distinct jobs.
The verdict does **not** hinge on that probe (§5.3).

---

## 1. The problem, restated and grounded in code

Re-route #8's decisive AGC debug-log analysis ([gke-dogfood.md](archive/gke-dogfood-turnup-findings.md), re-route #8): a ~7-job burst to an idle pool at `maxListeners = 48`, clean namespace, all recycle/capacity/tax seams (Q259/Q266/Q267/Q248/Q265) resolved and quiet.
Outcome — **2/7 green, 5/7 wedged `in_progress` indefinitely**:

- GitHub fanned **one** job's planID (`coverage`, `62d2c792`) out as ~6 sibling deliveries; the AGC deduped every sibling on planID and released each via Option A `completejob` — **correct**.
- The **other 5 jobs' own planIDs were never delivered** to the AGC.
  GitHub had marked those 5 jobs `in_progress`/`started` on the recycled stable-named runners (`ci8-1`, `ci8-2` each carried 3 assignments) and left them dangling.
- The online-idle pool **stalled at 3 active sessions ≪ 48** — "a fan-out *duplicate* delivery does not grow the demand-driven 1:1 replacement pool the way a distinct acquisition does" — so GitHub never saw enough distinct idle runners to place the 5 distinct jobs.

**The binding constraint is the number of *distinct idle long-poll sessions* the AGC presents to GitHub at burst.** Two facts decide whether an AGC lever can move it — one AGC-side (§2, ours to change), one GitHub-side (§4, the crux we cannot change).

## 2. Why only 3 of 48 listeners were online-idle at burst (AGC-side, from code)

Pinned to the multiplexer/listener code, not assumed:

1. **Baseline is exactly one permanent poller.** `Multiplexer.Start` spawns a single permanent baseline goroutine (`m.spawn(ctx, true)`, [`multiplexer.go:152`](../../cmd/agc/internal/listener/multiplexer.go)); there is **no** `minIdleListeners` / eager-warm knob.
   `maxListeners` is only a *ceiling* (`SpawnReplacement` no-ops at the cap, [`multiplexer.go:174`](../../cmd/agc/internal/listener/multiplexer.go)).
2. **Growth is reactive and 1:1, triggered only by a distinct-planID acquisition.** `SpawnReplacement` is called by a listener **after it wins a job** ([`goroutine.go:924`](../../cmd/agc/internal/listener/goroutine.go)) — *after* the `ClaimJob` dedup gate.
   A **deduped loser skips SpawnReplacement entirely** (`goroutine.go:866`: "SpawnReplacement/renew/provision are all skipped for the loser"; the loser returns at `goroutine.go:909`).
   So **F duplicate deliveries of one job grow the pool by exactly 1** (the single winner), not by F.
3. **Therefore, from an idle start, the pool can only grow as fast as *distinct* planIDs are won.** When GitHub feeds the small idle pool duplicates of one job (§1), each duplicate is a loser → no growth → the pool never climbs toward 48.
   `maxListeners` width is **not** the lever (#8 confirmed a 3-session stall at 48); the reactive, winner-only, demand-driven growth is.

This is genuinely AGC-side and changeable — an eager warm baseline of idle listeners *would* present more distinct idle runners at t=0 (that is H1).
Whether that helps is decided entirely by §4.

## 3. H2 — unique/ephemeral runner names: NON-LEVER

Today the agent name is deterministic by slot index — `fmt.Sprintf("%s-%d", groupName, index)` →`ci8-1`, `ci8-2` ([`pool.go:328`](../../cmd/agc/internal/agentpool/pool.go)) — and [Q114](../STATUS.md) single-use recycle **re-registers the same name** (same index) after each job.
H2 proposes a unique/ephemeral name per registration so GitHub "distributes" rather than "piles."

It fails on the mechanism:

- **Names are not the binding constraint; distinct *idle sessions* are (§1–§2).** Unique names relabel the same ~3 reactively-grown slots.
  Three uniquely-named idle runners are still three idle runners — GitHub has the same three targets.
- **The #8 pile-up is runner-*id* churn, not name reuse.** `generate-jitconfig` mints a **new runner id every registration even for the same name**; GitHub tracks assignments by id.
  On recycle, the old id's staged assignments **orphan** (the new same-named runner is a different id).
  Unique names change the label but not the id-churn — every recycle is still a brand-new runner GitHub has never balanced to.
  If anything, unique names make it *worse*: they forfeit the Q114 same-name fast-path and multiply distinct runner records (the stale-`ci-N` clutter that guard-blocked cleanup already amplified in re-routes #6/#7).
- **Secure-by-default / cost:** no security change, but a strict regression in registration churn and record hygiene for no dispatch benefit.

**H2 verdict: reject.** It does not add idle capacity, does not fix id-churn orphaning, and worsens record clutter.

## 4. H1 — warm idle *listener* baseline: the crux, and why it is not a reliable fix

### 4.1 The reframe: warm *listeners*, not Q261 warm *workers*

The task framed H1 as "warm/eager idle pool (Q261)."
That conflates two different things and the distinction is load-bearing:

- **Warm idle *worker pods*** = [Q261/G.12](../design/appendix-g-future-enhancements.md#g12-warm-worker-pool-minidleworkers) `minIdleWorkers`.
  A pre-scheduled idle **pod** cuts pod-schedule latency after a job is *acquired*.
  It is **not** a long-poll session and presents **no** extra idle runner to GitHub → **zero** effect on dispatch starvation.
  Reviving Q261 as specified does **not** address Q224's blocker.
- **Warm idle *listeners*** = a *new* `minIdleListeners` / `baselineListeners` knob that eagerly maintains N idle long-poll sessions (each a distinct registered idle runner) instead of the single reactive baseline (§2).
  *This* is what H1 needs, and it does not exist today.

So even to *try* H1 faithfully you build the warm-listener baseline, not Q261.

### 4.2 Efficacy hinges on one GitHub-side unknown

With N warm distinct idle classic listeners and N queued jobs, GitHub's classic offer-to-many broker does one of two things:

- **(a) Spread** — assign job-i to runner-i (1:1 load-balance). → H1 helps.
- **(b) Fan-out-first** — offer job-1 to *all* N idle pollers (N-way race, 1 winner, N−1 dedup losers), only advancing to job-2 as pollers free up. → H1 fails: more warm runners just get more duplicates of the front-of-queue job.

We have **no direct classic-protocol evidence** of (a) vs (b) on a *warm, wide, non-churning* pool — every Q260 observation was on a small, reactive, churning pool where the two are hard to separate.
But the existing evidence **leans toward (b)**:

- **#8 is the strongest tell.** Faced with 7 queued jobs and a small idle pool, GitHub fed the ~3 idle slots **~6 duplicates of ONE planID** while **5 distinct planIDs went undelivered**.
  If GitHub load-balanced distinct jobs across idle runners (a), those 5 planIDs would have gone to the idle slots; instead the same job was re-offered/fanned.
  That is the signature of (b).
  *(Consistent-with, not proof: the 5 could in principle have been withheld by an unrelated per-group serialization — which is exactly what the §5 probe would settle.)*
- **The scale-set 1:1 result does *not* transfer.** Investigation E2 ([q264 §2b](archive/q264-scale-set-protocol-phases.md#2b-investigation-e2--capacity-gating-recovery-and-a-real-runner-2026-07-04)) showed GitHub delivering distinct jobs 1:1 — but on the **auto-assign scale-set backend** (server pushes assignments up to a capacity header), a *different dispatch algorithm* from classic offer-to-many long-poll.
  It proves GitHub *can* spread when it owns the assignment; it says nothing about classic pollers.

### 4.3 Even the favourable case is not a reliable fix

Suppose the §5 probe shows (a) — warm-wide classic spreads.
H1 would still be a **probability improvement, not a structural guarantee**, because GAG's topology re-enters churn the moment load is sustained:

- Every completed job **recycles** its runner ([Q114](../STATUS.md) single-use): deregister → re-register, a seconds-long window in which that runner is absent and any job GitHub stages to its old id **orphans** (the #8 failure, unchanged).
- Sustaining N warm idle listeners under continuous burst means continuously re-warming through that churn.

To make H1 reliable you would additionally need **non-recycling ephemeral runners** (one runner per job, never reused) **plus** a continuously-maintained warm baseline **plus** capacity-accurate advertisement — i.e. you reconstruct the **scale-set capacity model** on top of the **fan-out-prone offer-to-many protocol**, keeping the very race Option E removes by construction.
That is strictly more work than Option E for a strictly worse result (no single authoritative stream, no capacity header, the fan-out race still latent).

**H1 verdict: not a reliable fix.** Its efficacy is unconfirmed and the evidence leans against it; even its best case converges on a dominated reimplementation of Option E. Its only defensible use is a small, opt-in, flagged **stopgap** to raise dogfood green-*rate* while Q264 is decided — contingent on the §5 probe first confirming (a).

## 5. The decisive experiment (designed, ready to run — not run this session)

Because GitHub's server-side distribution (§4.2, (a) vs (b)) is the one crux a fake cannot prove, the confirmatory step is a **live wire probe against real GitHub** — a classic analogue of the [Investigation E/E2](archive/q264-scale-set-protocol-phases.md#2a-investigation-e--live-wire-probe-2026-07-04) scale-set probes.

### 5.1 Design

Reuses existing tooling: the classic broker client ([`broker/client.go`](../../broker/client.go)), the probe's session lifecycle and its two-session delivery-observation primitive (`investigateJobDelivery`, [`cmd/probe/main.go`](../../cmd/probe/main.go)), and `config.sh`/JIT registration ([`scripts/dev/probe-investigations-cd.sh`](../../scripts/dev/probe-investigations-cd.sh)).
New: a `classic_dispatch.go` scenario that

1. **Registers M distinct classic runners** into a runner group carrying a dedicated label, varying the naming axis: **stable/recycled** (`probe-N`) vs **unique/ephemeral** (`probe-<nonce>`) — the H2 variable.
2. **Opens all M long-poll sessions and confirms all M are idle-polling (warm)** *before* queuing — the H1 variable (contrast: reactively grow from 1).
3. **Queues N distinct jobs** near-simultaneously via a fixture workflow (`classic-dispatch-probe.yml`, an N-job `matrix` on `runs-on: [self-hosted, <label>]`), analogous to `scaleset-probe.yml`.
4. **Records, per delivery**: which session/runner received it, the `planID` (post-`AcquireJob`) and `RunnerRequestID`, and time.
   **Metric:** distinct planIDs delivered vs duplicate deliveries, and their distribution across the M sessions.
5. **Cleans up** all M runner records (a fresh dedicated group/label to avoid the shared-repo `ci-N` clutter hazard).

Run the 2×2: {stable, unique} × {warm, reactive}, N ≈ M ≈ 6–8.
Same App creds as the scale-set probe (App `actions-gateway-test`, id 3752347; PEM in keychain), repo-scoped to `github-actions-gateway` (bypasses org runner-group policy, per [q264 §2a-6](archive/q264-scale-set-protocol-phases.md#2a-investigation-e--live-wire-probe-2026-07-04)).

### 5.2 Reading the result

- **Warm/stable and warm/unique both still fan out** (multiple sessions get the same planID; distinct planIDs starve) → **(b) confirmed; H1 and H2 both fail; #530/§5 and this verdict are airtight; Option E is the only fix.**
- **Warm spreads (distinct planID per idle session), stable or unique** → **(a); H1 is a real lever** for green-*rate* (still a stopgap per §4.3, not a structural fix).
  This is the single result that would upgrade H1 from "stopgap" to "worth a flagged implementation."
- **Unique spreads but stable does not** → H2 matters after all (contradicts §3); would warrant reopening the naming analysis.

### 5.3 Why it was not run here, and why the verdict holds without it

- **This is a spike; the deliverable is the verdict + design** (per the task and the CLAUDE.md fix/flag/defer discipline).
  Creds are usable — the [q264 §5](archive/q264-scale-set-protocol-phases.md#5-load-bearing-unknowns) "STOP if no creds" bar is *not* what gates this; the value-of-information does.
- **The result does not move the Q264 go/no-go.** By §4.3, even the favourable (a) outcome yields only a probabilistic stopgap that converges on a dominated Option E reimplementation — so the *structural* verdict (Option E is the only reliable fix) is invariant to the probe.
  Running it would refine H1's stopgap value, not decide Q264.
- **Cost/risk:** a faithful 2×2 warm-burst run needs M live registrations, a new fixture, tight burst timing, and cleanup on the **shared** dogfood repo — the exact clutter/coordination hazard flagged across re-routes #6–#8.
  Better run as its own focused live session with the user aware.

**If the user wants the empirical confirmation** (e.g. to justify a green-rate stopgap while deciding Q264), running §5.1's `classic-dispatch-probe` is the next step — it is fully specified above.

## 6. Verdict and recommendation

| Lever | Verdict | Basis |
|---|---|---|
| **H2 — unique/ephemeral names** | **Reject (non-lever)** | Adds no idle sessions; #8 orphaning is id-churn not name reuse; worsens record clutter. Reasoned from code + #8 (§3). |
| **H1 — warm idle *listener* baseline** (≠ Q261 warm *workers*) | **Not a reliable fix; at best a flagged stopgap, efficacy unconfirmed** | Binding constraint is idle-session count (§2), but GitHub-side spread-vs-fanout is the crux and #8 leans fan-out; even favourable case = dominated Option E reimplementation (§4). |
| **H1+H2 combined** | **Dominated by Option E** | = poor-man's scale set on the fan-out-prone protocol (§4.3). |
| **Option E — scale-set single-acquirer** ([Q264](q264-scale-set-protocol.md)) | **The only structural fix** | One authoritative stream, no sibling deliveries, no per-name recycle — eliminates the class by construction. Spike + probes done, viable. |

**Bottom line:** #530's conclusion survives the escape-hatch check.
No AGC-side lever makes the classic many-acquirers protocol *reliably* full-matrix green under fan-out.
The user has a clean go/no-go on the [Q264](q264-scale-set-protocol.md) rewrite: **go** if reliable concurrent full-matrix green on GitHub-hosted-parity is required; a small flagged **warm-listener stopgap** (H1, pending the §5 probe) is the only interim AGC-side option, and it does not close the gap.
Q224/Q242 stay open, blocked on this dispatch topology, not on any recycle/capacity/tax seam.
