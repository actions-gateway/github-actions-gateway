# v1 / classic sunset — strategic architecture review (decision memo)

**Status:** ⓘ informational — strategy/review, read-only analysis.
No code changed.
Feeds the Q264 P5 cutover and the v1alpha1 removal-timeline decisions.
Complete as a review: both follow-ups it filed are parked in Deferred, waiting on [v2-ga.md](v2-ga.md) Phase 3 ([Q273](../STATUS.md#Q273), the v1 removal) and on upstream scale-set GA ([Q272](../STATUS.md#Q272)). **Date:** 2026-07-05. **Author view:** adversarial review of the hypothesis that v1 may be fundamentally limited by its protocol constraints — and that if so, we should sunset it faster and focus on v2/scale-set.
The review deliberately tests that hypothesis rather than assuming it.

> **Update (2026-07-05) — P4 is in, and green on the acceptance gate.** [Q264](q264-scale-set-protocol.md) **P4** (live dogfood, [PR #541](https://github.com/actions-gateway/github-actions-gateway/pull/541)) confirmed the scale-set path **eliminates the [Q224](archive/gke-dogfood-turnup-findings.md) fan-out distinct-delivery starvation by construction**: the single-acquirer listener assigned, ran, and terminally concluded **all 7** distinct jobs (**7/7**, 0 dedup / collision) where classic managed **2/7** across eight re-routes.
> This is the single most important piece of evidence for the "accelerate" case (§6.4). **Caveat — not a clean all-green sweep yet:** the residual non-green is *orthogonal to acquisition* (a self-referential `WORKER_MODE=scaleset` test leak — GAG dogfooding its own CI on its own scale-set worker — plus CPU-starved envtest), and gates the **P5 cutover**, not the acceptance verdict.
> Q224/Q264 stay open until the clean-green re-run.

---

## 0. TL;DR

- **"v1 is fundamentally limited" is too strong as a blanket statement, and — as usually framed — conflates two orthogonal things.** The honest verdict: the **classic acquisition protocol** is structurally capped at *reliable high-burst concurrency* — a real, GitHub-server-side, AGC-unfixable ceiling — but works correctly for **individual and low/moderate-concurrency workloads**, which is a large real segment.
  The **v1alpha1 API** is a separate question with a separate (mostly unrelated) answer.
- **Two axes, one documented binding — still two separately-gated decisions.** The roadmap already binds the *endpoints*: `v1alpha1` is Classic-only and `v2beta1` is ScaleSet-only, with `v2alpha1` as the **dual-protocol bridge** (default `Classic`, per-set opt-in `ScaleSet`; [Q264 §5a-U7/U8](archive/q264-scale-set-protocol-phases.md#5a-the-three-decisions--analysis-and-recommendations-2026-07-04)).
  So retiring the **v1alpha1 API** (Axis 1) and retiring the **classic protocol** (Axis 2) are *not the same lever* even though they share an end.
  The fan-out wall is an Axis-2 (protocol) property — a `v2alpha1` RunnerSet on `Classic` hits the identical wall, so moving to the v2 API does not, by itself, fix it; selecting the scale-set protocol does.
- **Recommendation (updated 2026-07-05 — full form in §6.2): v2/scale-set *is* the product.
  Make it the only front door and let migration discipline carry the trust signal.** P4 proved scale-set clears the ceiling classic structurally cannot (**7/7 vs 2/7**), and there are **no real v1 adopters to protect** (§6.3) — so the goal is not to keep classic alive as a safety valve, it is to make v2 the obvious on-ramp and retire v1 *cleanly*.
  Concretely:
  - **Route every new user to v2 now** and deprecate v1 loudly in the docs path — the failure mode is a *new* user landing on v1, not a v1 user complaining.
  - **Classic is terminal** (no further investment — the eight re-routes were the last of it) and **v2beta1 stays ScaleSet-only**; the earlier "retain the protocol field as a fallback to v2 GA" hedge protected users who don't exist.
    The graduation guardrail is *technical* (clean-green + a short stability soak), not user-migration.
  - **Commit v1 removal to a stated schedule gated on v2 *stability* (the v2beta1 graduation), not on adopters.** The discipline — announced, scheduled, with a working `gag-migrate` — *is* the "safe bet, won't strand you" signal a prospective **v2** adopter is actually reading.
  - **The real lever is v2 *maturation* (Q74 / v2beta1), not v1 removal** — "v2 is the front door" only pays off once v2 is beta-stable.
- **Building classic first was a defensible bet, not a mistake.** The scale-set protocol was undocumented-internal-to-ARC at design time; the official standalone client only reached Public Preview in 2026.
  The world changed — that is what makes Option E cheap *now*, not that it was obviously right *then*.
- **No security or secure-by-default regression** in the migration; the positioning story (the "virtual runners" identity) does shift and must be rewritten in lockstep with the default flip.

---

## 1. Untangle the axes first (the thing the question conflates)

The maintainer's question — "is v1 kneecapped by the protocol problems?" — folds two independent axes into one word ("v1").
The single most important thing this memo does is keep them apart.

| | **Axis 1 — API surface** | **Axis 2 — acquisition protocol** |
|---|---|---|
| The two ends | `v1alpha1` (frozen monolithic `ActionsGateway`/`RunnerGroup`) vs `v2alpha1` (decomposed `RunnerSet`/`RunnerTemplate`/`EgressProxy` — the Q74 v2 work) | **classic** per-runner broker (many-acquirers) vs **runner-scale-set** (single-acquirer) |
| What it governs | object shape, tenancy model, reusable templates, standalone egress proxy | *how the AGC acquires jobs from GitHub* — the fan-out mechanics |
| Where it lives | API group / kinds | `RunnerSet.spec.acquisitionProtocol` (v2alpha1 only; v1alpha1 is classic-only) |
| The fan-out wall? | **Axis-independent** | **This is where the wall lives** |

**Proof they are orthogonal.** The high-burst fan-out failure is a property of the **many-acquirers topology** (concurrency = registered runners = acquirers), not of the object schema.
By design decision ([Q264 §5a-U7](archive/q264-scale-set-protocol-phases.md#u7--where-the-protocol-selector-lives)), `acquisitionProtocol: ScaleSet` is **v2-exclusive**: a v2alpha1 RunnerSet set to `Classic` reproduces the identical wall, and v1alpha1 never gets ScaleSet at all.
So:

- **Moving from v1alpha1 → v2alpha1 does *not*, by itself, fix the fan-out wall.** It only *unlocks the option* to select the protocol that does — `v2alpha1` defaults to `Classic`, which carries the identical wall.
- **"Retire the classic protocol" and "remove the v1alpha1 API" are separately *gated*, even though the roadmap binds their endpoints.** Because `v2alpha1` carries *both* protocols, the v1alpha1 API can be removed first — tenants move to `v2alpha1: Classic` via `gag-migrate`, behaviour unchanged — while the classic *protocol* lives on.
  The classic machinery is removed only later, at the Q74 v2beta1 graduation (v2beta1 is ScaleSet-only, so that hop strips the field and ends classic).
  Same end, but the **API** removal is gated on *adoption* and the **protocol** removal on *scale-set maturity* — different evidence, different risk, so the memo treats them separately throughout.
  (This is also the two-step migration safety valve of §6.2: change the API first, change the protocol second, each reversible on its own.)

---

## 2. Does classic actually *work*? (per-workload verdict)

Grounded in the live dogfood series ([gke-dogfood.md](archive/gke-dogfood-turnup-findings.md) re-routes #3–#8), the Q260 saga ([q260-fanout-completion-reconciliation.md](archive/q260-fanout-completion-reconciliation.md), [q260-planid-dedup-refix.md](archive/q260-planid-dedup-refix.md)), and the lever spike ([q224-fanout-dispatch-lever-spike.md](q224-fanout-dispatch-lever-spike.md)).
The answer is **not binary** — it depends entirely on the concurrency class.

| Workload class | Works? | Evidence | Failure mode (if any) |
|---|---|---|---|
| **Individual jobs** | ✅ Yes | Every re-route: a job that lands a worker completes green; auth/renew/complete all correct. | — |
| **Low / moderate concurrency** (2–3 distinct jobs in flight) | ✅ Yes, with tuning | Re-route #7 held the pool **stably at `maxListeners=12`**; re-route #5 landed **3/3** jobs that received a planID green and held them past the 15-min timeout. | Residuals were **capacity** (Q248 SSD ceiling) + cold cache — now fixed, and *not* topology. |
| **High concurrent burst** (7+ distinct jobs, near `maxListeners`) | ❌ No — *reliably* | Re-route #8, **clean namespace**, all recycle/capacity/tax seams (Q259/Q266/Q267/Q248/Q265) resolved and quiet: **2/7 green, 5/7 wedged `in_progress` indefinitely**. | **GitHub-server-side fan-out distinct-delivery starvation** (see §4). |

**The counter-case is strong and must be stated plainly:** classic is a *working* system for the individual and low/moderate-concurrency segment — which, per the go-to-market ICP ([go-to-market.md](go-to-market.md) §3: platform teams on shared multi-tenant Kubernetes, compliance/egress-driven self-hosting), is a large and arguably *primary* real workload.
Writing the whole thing off because it fails at reliable 7-way-concurrent-matrix throughput conflates "fails the hardest CI-matrix stress test" with "does not work."

**But the ceiling is real and it is not a tuning artifact.** Re-route #8 is the decisive datum: on a pristine namespace with every AGC-side seam fixed, a ~7-job burst still stranded 5 jobs forever.
This is not "needs more fixes" — it is a wall (§4).

---

## 3. Does it *scale*? Does it *perform*?

### 3.1 Scaling — the rate-limit budget is the ceiling that grows with concurrency

- The GitHub installation budget is **~15,000 req/hr**; a classic long-poll session costs **~72 req/hr** (~50 s holds), giving a practical **~250-session** ceiling (~150 RunnerGroups at one baseline session each with headroom) ([03-api-contracts.md §3.5](../design/03-api-contracts.md#35-github-api-rate-limit-budget); [appendix-e-capacity-planning.md](../design/appendix-e-capacity-planning.md)).
- **Classic's budget scales *with acquisition concurrency*:** at burst the session count climbs toward `sum(maxListeners)` across groups — the very knob you must raise to chase throughput is the knob that consumes the rate-limit budget.
  This is a genuine scaling coupling.
- **Scale-set decouples them:** one session per group at **all** load levels (~72 polls/hr), because concurrency is expressed as the batch size / capacity header, not as a session count.
  The §3.5 rate-limit ceiling **stops scaling with acquisition concurrency** ([Q264 §3 "Improves"](archive/q264-scale-set-protocol-phases.md#improves)).
  This is a real, structural scaling win for scale-set, not a wash.

### 3.2 Performance / density — the story cuts both ways, and a common misread

- **At rest, classic's density was a genuine GAG differentiator:** a ~12 KiB listener goroutine in one shared pod vs ARC's always-on listener pod (+ cluster IP) per scale set ([appendix-d-alternatives-considered.md](../design/appendix-d-alternatives-considered.md) §D.3; the earlier "~256 MiB .NET / ~4,000×" framing was retired by #781 — ARC's scale-set listener is Go, and the ratio's denominator was never measured).
- **Correcting a tempting misread: scale-set does *not* serialize concurrency.** It is easy to conclude "one session per group ⇒ one job at a time."
  That is wrong.
  The scale-set listener **batch-acquires** and provisions **N workers in parallel**; concurrency is governed by `maxWorkers`/`priorityTiers` advertised as `X-ScaleSetMaxCapacity`, fully decoupled from the single session ([Q264 §2.3](archive/q264-scale-set-protocol-phases.md#23-batch-acquisition--the-call-that-kills-the-fan-out), live-confirmed §2b-1).
  One session ≠ one concurrent job.
- **Scale-set *improves* at-rest density** — one session/group instead of classic's reactive climb toward `maxListeners` sessions — while keeping the goroutine-listener footprint (a goroutine in the shared AGC pod, not a per-scale-set listener pod) ([Q264 §4.7](archive/q264-scale-set-protocol-phases.md#4-honest-cost-list-delta-vs-the-q260-4e-estimate), §3 "Improves").
  The "ARC's protocol, GAG's efficiency" pitch holds.
- **Overhead machinery classic carries that scale-set deletes by construction:** the single-use agent recycle (Q114: 2 REST calls + Secret rewrite + session re-create per job), planID dedup (#512/Q260), the renew loop (Q247), completion fan-out (Option A), and the multiplexer/self-heal ladder (Q152) — all gone under one session + per-job JIT config ([Q264 §3 "Discarded"/"Improves"](archive/q264-scale-set-protocol-phases.md#3-delta-from-todays-classic-machinery)).

**Net:** at rest, both are cheap and scale-set is *slightly* cheaper; at burst, classic's rate-limit budget and recycle churn grow with concurrency while scale-set's do not.
Performance is **not** where classic is kneecapped — the kneecap is dispatch reliability (§4), not footprint.

---

## 4. Is it kneecapped by the protocol? (the crux — structural ceiling vs remaining bugs)

**Yes — at high concurrency, structurally, and unfixable from the AGC side.** This is the central finding and it must be distinguished carefully from the *bugs that have already been fixed*.

**What was a fixable bug (all now fixed):**
- Completion accounting — losers silently abandoned → Option A winner-driven `completejob` per delivery, **live-confirmed GO** (re-route #5: `completejob` returns OK on a live sibling, jobs conclude green and survive the 15-min timeout; [Q260 §5 re-route #5](archive/q260-fanout-completion-reconciliation.md#re-route-5-confirmed-2026-07-04--go)).
- planID dedup keyed correctly (#512), recycle 422 churn (Q259), slot-stranding (Q266), token-400 ride-out (Q267), SSD capacity ceiling (Q248) — all resolved and confirmed quiet in re-route #8.
- The Q265 benchmark explicitly found **no completion-tax throughput wall** ([Q260 §7](archive/q260-fanout-completion-reconciliation.md#7-q265--fan-out-throughput-benchmark-2026-07-05-tax-wall-or-tuning)) — Option A's accounting is *not* the bottleneck.

**What is a structural ceiling (unfixable AGC-side):** *fan-out distinct-delivery starvation.* Mechanism, pinned to code and live logs (re-route #8):

1. GitHub fans **one** logical job's planID out as ~6 sibling deliveries; the AGC correctly dedups them and releases each via Option A.
2. The pool grows **only** on a *distinct*-planID win — `SpawnReplacement` runs only *after* a listener wins past the dedup gate ([goroutine.go:924](../../cmd/agc/internal/listener/goroutine.go); a deduped loser skips it entirely).
   So **F duplicate deliveries of one job grow the pool by exactly 1**, not F.
3. Fed duplicates of one job, the idle pool **stalls at 3 online sessions ≪ 48**, so GitHub never sees enough *distinct* idle runners to place the **5 other distinct jobs** — whose planIDs are therefore **never delivered** and wedge `in_progress` indefinitely.

The lever spike tested the two proposed AGC-side escape hatches against this and concluded: **no reliable AGC-side lever.** Unique/ephemeral names (H2) add zero distinct idle runners (a non-lever); a warm idle-*listener* baseline (H1) is at best a probabilistic green-*rate* stopgap whose efficacy is unconfirmed and whose favourable case **converges on reimplementing the scale-set capacity model on the fan-out-prone protocol** — strictly dominated by Option E ([q224-fanout-dispatch-lever-spike.md](q224-fanout-dispatch-lever-spike.md), verdict § up-front).
The binding constraint (whether GitHub *spreads* distinct jobs across a wide idle pool or *fans one out first*) is **server-side and unknowable/unchangeable from the AGC**.

> **Verdict:** classic is **structurally kneecapped at reliable high-burst concurrency**, not merely buggy.
> The distinction matters for the timeline: you cannot engineer your way out of it on the classic protocol; the only structural fix is single-acquirer topology (Option E / scale-set).
> Below that concurrency line, the protocol is not kneecapped at all.

**Now confirmed from the other side (P4, 2026-07-05).** The structural claim is no longer only theory: on the identical concurrent matrix that pinned classic at 2/7, the scale-set path concluded **all 7** distinct jobs (7/7, single acquirer, zero dedup) — the fan-out is gone *by construction*, exactly as §5a-U8 predicted.
That is the strongest possible evidence both that classic's ceiling is real (the same test, same load, opposite outcome) and that Option E is the fix, not merely *a* fix.

---

## 5. Was scale-set "what we should have built from the start"? (hindsight, fairly)

**No — building classic first was a defensible bet given what was knowable then.** Resisting hindsight bias:

- **The protocol was not available as a supported surface at design time.** The runner-scale-set protocol was undocumented-internal-to-ARC (no wire spec; the ARC Go client had to be read at a pinned tag).
  GitHub only published the **official standalone `actions/scaleset` client** (Public Preview, MIT) in 2026 — four releases v0.1.0–v0.4.0, Feb–May 2026 ([Q264 §4, §5a-U6](archive/q264-scale-set-protocol-phases.md#u6--wire-client-vendor-actionsscaleset-vs-gag-owned-implementation)).
  Building on it *then* would have meant reverse-engineering a **second** undocumented GitHub-internal protocol and betting the product on a moving, pre-1.0-adjacent target.
- **The classic virtual-runner bet bought real, still-valid merits:** the goroutines-in-one-shared-pod at-rest density advantage over ARC's pod-per-scale-set listeners, per-tenant egress isolation, multi-tenant self-service, and zero idle compute (appendix-d §D.3–D.4).
  None of that was free elsewhere.
- **The specific failure was not cleanly foreseeable from source.** The topology consequence (concurrency = acquirers) was arguably visible in principle, but the precise *distinct-delivery starvation* behaviour only emerged under live high-burst dogfood — source-reading alone would not have surfaced it (indeed the whole Q260→Q224 saga is a sequence of live re-routes correcting source-level assumptions).

**Fair conclusion:** scale-set is "what we'd build today," not "what we obviously should have built then."
The classic tier also paid for itself — the protocol knowledge, the egress/isolation/provisioner machinery, and the `scalesettest` fake all carry straight into Option E (Q264 §3 "Carried over intact"; §6 "P1/P2 are useful even if Option A wins").

---

## 6. The sunset decision — separated by axis

### 6.1 What the existing plan already commits to

- **Protocol** ([Q264 §5a-U8](archive/q264-scale-set-protocol-phases.md#u8--support-matrix-policy)): coexist behind `acquisitionProtocol` (default `Classic`) through P3–P4 → **flip default to `ScaleSet` at P5** (with the positioning-doc rewrite) → classic deprecated **one minor release** → classic machinery removed in an isolated PR **aligned with the Q74 v2beta1 graduation** (v2beta1 is ScaleSet-only).
- **API** ([v1alpha1-deprecation.md](../operations/v1alpha1-deprecation.md)): v1alpha1 already **deprecated but served**; removed "once v2 adoption is sufficient," announced as a **named release with ≥1 release of notice**; no action forced until then.
- **The coupling:** because ScaleSet is v2-exclusive, classic is v1alpha1's *only* acquisition path, so classic-machinery removal **is** the end of v1alpha1's ability to acquire jobs — the plan therefore sequences the classic-removal PR after v1alpha1 is itself deprecated, announcing both together (Q264 §5a-U8 "Consequence").

> **Design question this review raised, now resolved (2026-07-05): keep v2beta1 ScaleSet-only.** The question was whether to *retain* `acquisitionProtocol` (and a `Classic` fallback) through v2beta1 and drop it only at v2 GA, rather than stripping it at graduation (Q264 §5a-U7/U8).
> The retain-hedge existed to preserve a rollback fallback — but that fallback protects **v1/classic users who do not exist** (§6.3), so it buys almost nothing while muddying the "one protocol, strictly better than ARC" story that drives adoption.
> Against that: **P4 proved the protocol works live** (7/7), **GAG owns its own client** (not hostage to upstream's Preview cadence — "Preview" is a label on GitHub's *library*, while the *wire protocol* is what ARC runs in production at scale), and a cleaner beta surface is easier to commit to and support. **Decision: v2beta1 stays ScaleSet-only.** The residual risk — GitHub drifts the protocol post-graduation with no classic escape — is real but bounded (own-client + ARC-in-prod + a code fix is always shippable), and is managed with a **technical** guardrail rather than the retain-hedge: **do not graduate v2beta1 until the clean-green dogfood re-run plus a short stability soak of the scale-set path** — bet the beta on evidence, not one run.
> Consequence for the backlog: [Q272](../STATUS.md#Q272) (scale-set upstream maturity) is *not* a graduation blocker — it lifts the Preview caveat and triggers the U6 vendor-vs-own revisit, but the beta cut gates on GAG's own soak, not on GitHub's GA timeline.

### 6.2 The recommendation — v2 is the product; retire v1 on a stability schedule

An earlier draft of this memo recommended "hold both removals, keep classic as a safety valve, gate on an adoption signal."
That rested on an assumption since corrected: **there is no v1 adopter base to protect, and there won't be one** (§6.3).
Once that falls away, the safety-valve rationale collapses and the goal changes — from *protecting v1* to making **v2 the only sensible on-ramp** and using the v1→v2 handling as a forward-looking trust signal.
What that signal is *for* matters: its audience is not v1 users (there are none) but a **prospective v2 adopter** judging whether GAG is a safe multi-year bet — they read how the project treats its own deprecated version as a proxy for how it will treat *them* at the next version bump.
Handled poorly the cost is not complaints (no one to complain) — it is the adoption failure the maintainer named: **no adoption, or a new user landing on v1 instead of v2.**

1. **Make v2 the only front door now.** Route README / getting-started / onboarding / new-tenant templates entirely to v2, with a prominent "v1 is deprecated — start on v2" banner on the v1 pages.
   This directly kills the "new user adopts v1" failure mode and is cheap — it is *documentation and positioning*, not v1 runtime work.
2. **Commit v1 removal to a stated schedule gated on v2 *stability* — not adopters.** Replace the open-ended "removed once v2 adoption is sufficient" (which reads as indecision) with a **named removal release tied to the v2beta1 graduation**.
   Gating on v2 reaching beta is honest — you should not force anyone onto an alpha — and is *not* catering to imaginary users.
   The **discipline** (announced, scheduled, documented, backed by a working `gag-migrate`) *is* the "safe bet, won't strand you" signal, far more than the length of the window.
3. **Reinvest the freed effort into the migration mechanics as a showcase, not the v1 runtime.** `gag-migrate`, the migration guide, the deprecation notice, the "what's preserved / what changes" doc — make these *exemplary*, because that is the artifact a prospective adopter actually inspects.
   (This reframes the adoption-signal item [Q273](../STATUS.md#Q273) from "instrument who's using it" to "polish the migration story" — a better use of effort when there is no one to measure.)
4. **Classic is terminal — no further investment.** The eight re-routes were the last classic spend; the residual ceiling is structural (§4).
   This is the "structural ceiling → stop paying it down" disposition from [technical-debt.md](../development/technical-debt.md).
5. **v2beta1 stays ScaleSet-only** (§6.1), with a *technical* graduation guardrail (clean-green + a stability soak), not a user-migration one.
6. **The real lever is v2 *maturation*, not v1 removal.** "v2 is the front door" only converts to adoption once v2 is beta-stable, so the priority is the **v2beta1 graduation path** — Q74 and its blockers (Q191/Q196/Q197/Q242/Q243; Q224 is now addressed by the scale-set path) — not the v1 sunset.
   Retiring v1 is the *consequence* of v2 being ready, not a goal to chase on its own.

**The one risk to hold in view.** Do not let "deprecate v1 loudly" run ahead of v2 actually being ready to be the sole path — v2 is still alpha with open blockers.
Route new users to v2 in the docs *now* (safe: it is the better shape), but keep the loud removal *commitment* pinned to v2beta1 readiness.
And stay honest that GAG's weaker flank versus ARC is *maturity* (pre-1.0, a Public-Preview protocol dependency), not capability — which is precisely *why* the migration-discipline signal is worth the effort: it is how a young project earns "safe bet" trust before it has the track record to claim it.

### 6.3 Adoption reality — the fact that most changes the risk math

The go-to-market posture is **pre-adoption dogfooding**, and this is decision-load-bearing:

- A `v1.0.0` tag exists and `v1.1.0-rc.*` are cut, **but** the public site is **not launched**, seed channels are **not started** ("gated on site + 1.0 install path"), and there are **no external deployers yet** — the goal is still "first handful" ([go-to-market.md](go-to-market.md) §8 Phase 0–1).
  GAG is Apache-2.0, **non-commercial**, deliberately donation-ready, revenue explicitly out of scope.
- **Implication (this is the load-bearing turn):** the compat cost of retiring v1 is ~zero — no external user base holds classic or v1alpha1 in production, and none is expected to; new users should hop straight onto v2.
  That removes the "protect the v1 fallback" rationale **entirely** and *inverts* the risk.
  The danger is no longer "we strand a v1 user" — it is "a **new** user onboards onto v1, or doesn't adopt at all because the versioning story looks unsafe."
  So the disposition is not *hold*: it is **retire v1 cleanly on a stability-gated schedule, and make v2 the only marketed on-ramp now** (§6.2).
  Removal gates on **v2 reaching beta**, not an adoption count — there is no adopter base to measure, and waiting on one would itself read as indecision.
  The adoption signal (Q273) is therefore reframed from *"gate removal on measuring adopters"* to *"confirm dogfood-only so we can commit the schedule, and make the migration itself exemplary."*

### 6.4 Gating evidence (what must be true before each acceleration step)

| Step | Gate |
|---|---|
| Route new users to v2 + deprecate v1 loudly in the docs path | **Do now** — positioning/docs only, no technical gate; kills the "new user lands on v1" failure mode (§6.2 item 1). |
| Name ScaleSet the recommended (and, in v2beta1, the only) protocol | **✅ MET (2026-07-05):** P4 ran the full concurrent matrix on scale-set — **7/7** distinct jobs concluded vs classic **2/7** — the fan-out fix is confirmed by construction. |
| Graduate v2beta1 (ScaleSet-only) + flip the default | Clean-green dogfood re-run (P4 `WORKER_MODE` residual fixed on main) **+** a short **stability soak** of the scale-set path **+** the positioning-doc rewrite in the same change (Q264 §4.7). A *technical* gate, not adopter-gated. |
| Remove v1 (v1alpha1 API **and** classic machinery, together) | The v2beta1 graduation above **is** the classic removal (ScaleSet-only beta can't store a Classic set) **+** a **named removal release announced with ≥1 release of notice**. Not gated on an adoption count. |
| *(no longer a gate)* Scale-set upstream GA / [actions/scaleset#107](https://github.com/actions/scaleset/issues/107) documented | Q272 — lifts the Public-Preview caveat and triggers the U6 vendor-vs-own revisit; **not** a graduation or removal blocker (§6.1). |

---

## 7. Security & positioning flags

- **No secure-by-default regression in the migration.** Egress isolation holds (all new endpoints GitHub-hosted, both listener and worker traffic stay behind the per-tenant proxy), workers still never see the App token, the JIT-credential surface is unchanged, and the admission gate (Q59) *strengthens* under the capacity header ([Q264 §4 "Security check"](archive/q264-scale-set-protocol-phases.md#4-honest-cost-list-delta-vs-the-q260-4e-estimate)).
  The default stays `Classic` (the more-conservative value) until validated — no security property is relaxed to enable the flip.
- **Positioning identity shifts — and this is the adoption lever, not just hygiene.** The "thousands of goroutine-backed virtual runners" story retires; density-at-rest *improves*.
  The rewrite should make the **strictly-better-than-ARC** claim for the ICP, not hedge with situational trade-offs (the maintainer's point: "better in certain circumstances" doesn't move anyone off ARC).
  Going ScaleSet-only *sharpens* it — one protocol, no "why two?" — so the claim becomes: **everything ARC's scale-set model does, with one shared listener pod per tenant instead of an always-on listener pod + cluster IP per scale set, plus per-tenant egress isolation, self-service multi-tenancy, and zero-idle that ARC lacks natively.** Be honest about the weaker flank — GAG's *maturity* (pre-1.0, a Public-Preview protocol dependency), not its capability.
  The `why-gag` / vs-ARC pages must be rewritten **in the same PR that flips the default** (Q264 §4.7) — flipping first would leave the public positioning contradicting the shipped behaviour.
- **User-visible API regression to document, not hide:** ScaleSet collapses `runnerLabels` to a single `runs-on` label (the scale-set name) and raises the GHES floor to 3.9 — real trade-offs the migration comms must state (Q264 §4.2/§4.5).

---

## 8. Follow-up items

The direction in §6.2 turns two of these into near-term work and demotes the third:

1. **[Q273 — active] Make v2 the front door + an exemplary v1→v2 migration story (the adoption trust signal).** Route README / getting-started / onboarding / new-tenant templates to v2; add "v1 deprecated — start on v2" banners; and polish `gag-migrate` + the migration guide + the "what's preserved / what changes" doc to showcase quality (the artifact a prospective **v2** adopter inspects).
   Reframed from the earlier "instrument an adoption signal" — with no adopter base to measure, the value is *confirm dogfood-only so the removal schedule can be committed* and *make the migration itself the signal*.
   Partly gated on v2 being ready to be the sole path (§6.2 risk); the docs-routing slice is doable now.
   Ties to [go-to-market.md](go-to-market.md) Phase 1.
2. **[Q272 — Deferred / watch] Scale-set upstream maturity watch.** Track `actions/scaleset` reaching GA/v1.0 and the auto-assign contract ([actions/scaleset#107](https://github.com/actions/scaleset/issues/107)) getting documented.
   Per §6.1 this is **not** a graduation or removal blocker — it lifts the Public-Preview caveat and triggers the [Q264 §5a-U6](archive/q264-scale-set-protocol-phases.md#u6--wire-client-vendor-actionsscaleset-vs-gag-owned-implementation) vendor-vs-own revisit.
3. **[folded into the P5 positioning rewrite] Migration guide states *why v2/ScaleSet*, not a Classic-vs-ScaleSet chooser.** Classic is terminal (§6.2 item 4), so the guide should route everyone to ScaleSet and explain the "no fan-out ceiling" reason — *not* present the two protocols as a menu.
   Tracked in Q264 §4.7.

**Flagged, not actioned:** (a) the §6.4 gate table + the §6.1 v2beta1-ScaleSet-only resolution should be folded into [Q264 §5a-U8](archive/q264-scale-set-protocol-phases.md#u8--support-matrix-policy) by the Q264 owner (that plan is under active P3–P4 work in a parallel session); (b) no `CLAUDE.md` change and no new milestone — the retirement is already the Q264/Q74 structure, and the live-verify meta-lesson is already a CLAUDE.md rule.

**Net thesis (revised).** Not "hold and sequence" — **v2/scale-set is the product.** Make v2 the only front door now, treat classic as terminal, keep v2beta1 ScaleSet-only with a technical soak guardrail, retire v1 on a schedule gated on **v2 reaching beta** (not on adopters), and let migration discipline carry the "safe bet" signal.
The one lever that actually converts the strategy to adoption is **v2 maturation (v2beta1), not the v1 sunset.**

---

## 9. Answering the maintainer's questions directly

> **Does v1 actually work?** The **classic protocol** works for individual and low/moderate-concurrency workloads (a large real segment) and fails *reliably* only at high-burst concurrency.
> The **v1alpha1 API** works fine — it is just monolithic and superseded by the decomposed v2 shape.
>
> **Does it scale?
> Is it performant?** It performs well (density was a real differentiator; scale-set improves it slightly).
> It **scales sub-linearly on reliable concurrent-matrix throughput** because of the fan-out wall, and its rate-limit budget grows with acquisition concurrency.
> Footprint is not the limit.
>
> **Is it kneecapped by the protocol problems?** Yes — at high concurrency, **structurally and unfixably** from the AGC side.
> Not below that line.
>
> **Is v2/scale-set what we should have built from the start?** It is what we'd build *today*.
> Building classic first was a defensible bet — the protocol wasn't a supported surface then, and classic's merits were real.
>
> **Can we sunset v1 faster?** Yes — and we should, but the sharp move isn't "delete v1 fast," it's **"make v2 the only front door now."** With no v1 adopters to protect and P4 proving scale-set clears the ceiling classic can't, keeping classic alive as a fallback protects no one and muddies the ARC comparison.
> So: route every new user to v2 and deprecate v1 loudly today; treat classic as terminal; keep v2beta1 ScaleSet-only behind a technical soak guardrail; and commit v1 removal to a schedule gated on **v2 reaching beta**, not on an adoption count.
> Handle the v1→v2 migration *impeccably* — that discipline is the "safe bet, won't strand you" signal a prospective v2 adopter is actually reading.
> The real work is **maturing v2** (v2beta1); retiring v1 is its consequence, not a separate race.
