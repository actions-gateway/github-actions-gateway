# v1 / classic sunset — strategic architecture review (decision memo)

**Status:** ⓘ informational — strategy/review, read-only analysis. No code
changed. Feeds the Q264 P5 cutover and the v1alpha1 removal-timeline decisions.
**Date:** 2026-07-05. **Author view:** adversarial review of the hypothesis that
v1 may be fundamentally limited by its protocol constraints — and that if so, we
should sunset it faster and focus on v2/scale-set. The review deliberately tests
that hypothesis rather than assuming it.

> **Update (2026-07-05) — P4 is in, and green on the acceptance gate.**
> [Q264](q264-scale-set-protocol.md) **P4** (live dogfood,
> [PR #541](https://github.com/actions-gateway/github-actions-gateway/pull/541))
> confirmed the scale-set path **eliminates the [Q224](gke-dogfood.md) fan-out
> distinct-delivery starvation by construction**: the single-acquirer listener
> assigned, ran, and terminally concluded **all 7** distinct jobs (**7/7**, 0 dedup /
> collision) where classic managed **2/7** across eight re-routes. This is the single
> most important piece of evidence for the "accelerate" case (§6.4). **Caveat — not a
> clean all-green sweep yet:** the residual non-green is *orthogonal to acquisition*
> (a self-referential `WORKER_MODE=scaleset` test leak — GAG dogfooding its own CI on
> its own scale-set worker — plus CPU-starved envtest), and gates the **P5 cutover**,
> not the acceptance verdict. Q224/Q264 stay open until the clean-green re-run.

---

## 0. TL;DR

- **"v1 is fundamentally limited" is too strong as a blanket statement, and — as
  usually framed — conflates two orthogonal things.**
  The honest verdict: the **classic acquisition protocol** is structurally capped
  at *reliable high-burst concurrency* — a real, GitHub-server-side, AGC-unfixable
  ceiling — but works correctly for **individual and low/moderate-concurrency
  workloads**, which is a large real segment. The **v1alpha1 API** is a separate
  question with a separate (mostly unrelated) answer.
- **Two axes, one documented binding — still two separately-gated decisions.** The
  roadmap already binds the *endpoints*: `v1alpha1` is Classic-only and `v2beta1`
  is ScaleSet-only, with `v2alpha1` as the **dual-protocol bridge** (default
  `Classic`, per-set opt-in `ScaleSet`;
  [Q264 §5a-U7/U8](q264-scale-set-protocol.md#5a-the-three-decisions--analysis-and-recommendations-2026-07-04)).
  So retiring the **v1alpha1 API** (Axis 1) and retiring the **classic protocol**
  (Axis 2) are *not the same lever* even though they share an end: classic outlives
  the v1 API on `v2alpha1: Classic`, so the API can retire first (gated on adoption)
  and the classic machinery later (gated on scale-set maturity, at the v2beta1
  graduation). The fan-out wall is an Axis-2 (protocol) property — a `v2alpha1`
  RunnerSet on `Classic` hits the identical wall, so moving to the v2 API does not,
  by itself, fix it.
- **Recommendation, split by axis:**
  - **Protocol (Axis 2): accelerate *positioning*, not *removal*.** Endorse the
    existing plan's fastest safe point — flip the default to `ScaleSet` at P5,
    gated on P4 green — and additionally name ScaleSet the *recommended* protocol
    for concurrency-sensitive workloads the moment P4 is green. **Do not** pull
    classic-machinery *removal* forward: scale-set is GitHub Public Preview, and
    classic still serves the low/moderate segment and GHES at zero user cost.
  - **API (Axis 1): do not accelerate v1alpha1 *removal*.** Keep it gated on
    scale-set proving out *and* an adoption signal, precisely because the plan
    couples classic-machinery removal to v1alpha1 removal — pulling it forward
    would force a Preview-stage protocol onto v1 users at the API cutover.
- **Building classic first was a defensible bet, not a mistake.** The scale-set
  protocol was undocumented-internal-to-ARC at design time; the official standalone
  client only reached Public Preview in 2026. The world changed — that is what
  makes Option E cheap *now*, not that it was obviously right *then*.
- **No security or secure-by-default regression** in the migration; the
  positioning story (the "virtual runners" identity) does shift and must be
  rewritten in lockstep with the default flip.

---

## 1. Untangle the axes first (the thing the question conflates)

The maintainer's question — "is v1 kneecapped by the protocol problems?" — folds
two independent axes into one word ("v1"). The single most important thing this
memo does is keep them apart.

| | **Axis 1 — API surface** | **Axis 2 — acquisition protocol** |
|---|---|---|
| The two ends | `v1alpha1` (frozen monolithic `ActionsGateway`/`RunnerGroup`) vs `v2alpha1` (decomposed `RunnerSet`/`RunnerTemplate`/`EgressProxy` — the Q74 v2 work) | **classic** per-runner broker (many-acquirers) vs **runner-scale-set** (single-acquirer) |
| What it governs | object shape, tenancy model, reusable templates, standalone egress proxy | *how the AGC acquires jobs from GitHub* — the fan-out mechanics |
| Where it lives | API group / kinds | `RunnerSet.spec.acquisitionProtocol` (v2alpha1 only; v1alpha1 is classic-only) |
| The fan-out wall? | **Axis-independent** | **This is where the wall lives** |

**Proof they are orthogonal.** The high-burst fan-out failure is a property of the
**many-acquirers topology** (concurrency = registered runners = acquirers), not of
the object schema. By design decision
([Q264 §5a-U7](q264-scale-set-protocol.md#u7--where-the-protocol-selector-lives)),
`acquisitionProtocol: ScaleSet` is **v2-exclusive**: a v2alpha1 RunnerSet set to
`Classic` reproduces the identical wall, and v1alpha1 never gets ScaleSet at all.
So:

- **Moving from v1alpha1 → v2alpha1 does *not*, by itself, fix the fan-out wall.**
  It only *unlocks the option* to select the protocol that does — `v2alpha1`
  defaults to `Classic`, which carries the identical wall.
- **"Retire the classic protocol" and "remove the v1alpha1 API" are separately
  *gated*, even though the roadmap binds their endpoints.** Because `v2alpha1`
  carries *both* protocols, the v1alpha1 API can be removed first — tenants move to
  `v2alpha1: Classic` via `gag-migrate`, behaviour unchanged — while the classic
  *protocol* lives on. The classic machinery is removed only later, at the Q74
  v2beta1 graduation (v2beta1 is ScaleSet-only, so that hop strips the field and
  ends classic). Same end, but the **API** removal is gated on *adoption* and the
  **protocol** removal on *scale-set maturity* — different evidence, different risk,
  so the memo treats them separately throughout. (This is also the two-step
  migration safety valve of §6.2: change the API first, change the protocol second,
  each reversible on its own.)

---

## 2. Does classic actually *work*? (per-workload verdict)

Grounded in the live dogfood series
([gke-dogfood.md](gke-dogfood.md) re-routes #3–#8), the Q260 saga
([q260-fanout-completion-reconciliation.md](q260-fanout-completion-reconciliation.md),
[q260-planid-dedup-refix.md](q260-planid-dedup-refix.md)), and the lever spike
([q224-fanout-dispatch-lever-spike.md](q224-fanout-dispatch-lever-spike.md)). The
answer is **not binary** — it depends entirely on the concurrency class.

| Workload class | Works? | Evidence | Failure mode (if any) |
|---|---|---|---|
| **Individual jobs** | ✅ Yes | Every re-route: a job that lands a worker completes green; auth/renew/complete all correct. | — |
| **Low / moderate concurrency** (2–3 distinct jobs in flight) | ✅ Yes, with tuning | Re-route #7 held the pool **stably at `maxListeners=12`**; re-route #5 landed **3/3** jobs that received a planID green and held them past the 15-min timeout. | Residuals were **capacity** (Q248 SSD ceiling) + cold cache — now fixed, and *not* topology. |
| **High concurrent burst** (7+ distinct jobs, near `maxListeners`) | ❌ No — *reliably* | Re-route #8, **clean namespace**, all recycle/capacity/tax seams (Q259/Q266/Q267/Q248/Q265) resolved and quiet: **2/7 green, 5/7 wedged `in_progress` indefinitely**. | **GitHub-server-side fan-out distinct-delivery starvation** (see §4). |

**The counter-case is strong and must be stated plainly:** classic is a *working*
system for the individual and low/moderate-concurrency segment — which, per the
go-to-market ICP ([go-to-market.md](go-to-market.md) §3: platform teams on shared
multi-tenant Kubernetes, compliance/egress-driven self-hosting), is a large and
arguably *primary* real workload. Writing the whole thing off because it fails at
reliable 7-way-concurrent-matrix throughput conflates "fails the hardest CI-matrix
stress test" with "does not work."

**But the ceiling is real and it is not a tuning artifact.** Re-route #8 is the
decisive datum: on a pristine namespace with every AGC-side seam fixed, a ~7-job
burst still stranded 5 jobs forever. This is not "needs more fixes" — it is a wall
(§4).

---

## 3. Does it *scale*? Does it *perform*?

### 3.1 Scaling — the rate-limit budget is the ceiling that grows with concurrency

- The GitHub installation budget is **~15,000 req/hr**; a classic long-poll session
  costs **~72 req/hr** (~50 s holds), giving a practical **~250-session** ceiling
  (~150 RunnerGroups at one baseline session each with headroom)
  ([03-api-contracts.md §3.5](../design/03-api-contracts.md#35-github-api-rate-limit-budget);
  [appendix-e-capacity-planning.md](../design/appendix-e-capacity-planning.md)).
- **Classic's budget scales *with acquisition concurrency*:** at burst the session
  count climbs toward `sum(maxListeners)` across groups — the very knob you must
  raise to chase throughput is the knob that consumes the rate-limit budget. This
  is a genuine scaling coupling.
- **Scale-set decouples them:** one session per group at **all** load levels
  (~72 polls/hr), because concurrency is expressed as the batch size / capacity
  header, not as a session count. The §3.5 rate-limit ceiling **stops scaling with
  acquisition concurrency**
  ([Q264 §3 "Improves"](q264-scale-set-protocol.md#improves)). This is a real,
  structural scaling win for scale-set, not a wash.

### 3.2 Performance / density — the story cuts both ways, and a common misread

- **At rest, classic's density was a genuine GAG differentiator:** ~60 KiB per
  listener goroutine vs ARC's ~256 MiB .NET listener pod per scale set (~4,000×)
  ([appendix-d-alternatives-considered.md](../design/appendix-d-alternatives-considered.md) §D.3).
- **Correcting a tempting misread: scale-set does *not* serialize concurrency.** It
  is easy to conclude "one session per group ⇒ one job at a time." That is wrong.
  The scale-set listener **batch-acquires** and provisions **N workers in
  parallel**; concurrency is governed by `maxWorkers`/`priorityTiers` advertised as
  `X-ScaleSetMaxCapacity`, fully decoupled from the single session
  ([Q264 §2.3](q264-scale-set-protocol.md#23-batch-acquisition--the-call-that-kills-the-fan-out),
  live-confirmed §2b-1). One session ≠ one concurrent job.
- **Scale-set *improves* at-rest density** — one session/group instead of classic's
  reactive climb toward `maxListeners` sessions — while keeping the goroutine-listener
  footprint (a Go goroutine, not ARC's .NET pod)
  ([Q264 §4.7](q264-scale-set-protocol.md#4-honest-cost-list-delta-vs-the-q260-4e-estimate),
  §3 "Improves"). The "ARC's protocol, GAG's efficiency" pitch holds.
- **Overhead machinery classic carries that scale-set deletes by construction:** the
  single-use agent recycle (Q114: 2 REST calls + Secret rewrite + session re-create
  per job), planID dedup (#512/Q260), the renew loop (Q247), completion fan-out
  (Option A), and the multiplexer/self-heal ladder (Q152) — all gone under one
  session + per-job JIT config
  ([Q264 §3 "Discarded"/"Improves"](q264-scale-set-protocol.md#3-delta-from-todays-classic-machinery)).

**Net:** at rest, both are cheap and scale-set is *slightly* cheaper; at burst,
classic's rate-limit budget and recycle churn grow with concurrency while
scale-set's do not. Performance is **not** where classic is kneecapped — the kneecap
is dispatch reliability (§4), not footprint.

---

## 4. Is it kneecapped by the protocol? (the crux — structural ceiling vs remaining bugs)

**Yes — at high concurrency, structurally, and unfixable from the AGC side.** This
is the central finding and it must be distinguished carefully from the *bugs that
have already been fixed*.

**What was a fixable bug (all now fixed):**
- Completion accounting — losers silently abandoned → Option A winner-driven
  `completejob` per delivery, **live-confirmed GO** (re-route #5: `completejob`
  returns OK on a live sibling, jobs conclude green and survive the 15-min timeout;
  [Q260 §5 re-route #5](q260-fanout-completion-reconciliation.md#re-route-5-confirmed-2026-07-04--go)).
- planID dedup keyed correctly (#512), recycle 422 churn (Q259), slot-stranding
  (Q266), token-400 ride-out (Q267), SSD capacity ceiling (Q248) — all resolved and
  confirmed quiet in re-route #8.
- The Q265 benchmark explicitly found **no completion-tax throughput wall**
  ([Q260 §7](q260-fanout-completion-reconciliation.md#7-q265--fan-out-throughput-benchmark-2026-07-05-tax-wall-or-tuning)) —
  Option A's accounting is *not* the bottleneck.

**What is a structural ceiling (unfixable AGC-side):** *fan-out distinct-delivery
starvation.* Mechanism, pinned to code and live logs (re-route #8):

1. GitHub fans **one** logical job's planID out as ~6 sibling deliveries; the AGC
   correctly dedups them and releases each via Option A.
2. The pool grows **only** on a *distinct*-planID win — `SpawnReplacement` runs only
   *after* a listener wins past the dedup gate
   ([goroutine.go:924](../../cmd/agc/internal/listener/goroutine.go); a deduped loser
   skips it entirely). So **F duplicate deliveries of one job grow the pool by
   exactly 1**, not F.
3. Fed duplicates of one job, the idle pool **stalls at 3 online sessions ≪ 48**, so
   GitHub never sees enough *distinct* idle runners to place the **5 other distinct
   jobs** — whose planIDs are therefore **never delivered** and wedge `in_progress`
   indefinitely.

The lever spike tested the two proposed AGC-side escape hatches against this and
concluded: **no reliable AGC-side lever.** Unique/ephemeral names (H2) add zero
distinct idle runners (a non-lever); a warm idle-*listener* baseline (H1) is at best
a probabilistic green-*rate* stopgap whose efficacy is unconfirmed and whose
favourable case **converges on reimplementing the scale-set capacity model on the
fan-out-prone protocol** — strictly dominated by Option E
([q224-fanout-dispatch-lever-spike.md](q224-fanout-dispatch-lever-spike.md), verdict
§ up-front). The binding constraint (whether GitHub *spreads* distinct jobs across a
wide idle pool or *fans one out first*) is **server-side and unknowable/unchangeable
from the AGC**.

> **Verdict:** classic is **structurally kneecapped at reliable high-burst
> concurrency**, not merely buggy. The distinction matters for the timeline: you
> cannot engineer your way out of it on the classic protocol; the only structural
> fix is single-acquirer topology (Option E / scale-set). Below that concurrency
> line, the protocol is not kneecapped at all.

**Now confirmed from the other side (P4, 2026-07-05).** The structural claim is no
longer only theory: on the identical concurrent matrix that pinned classic at 2/7,
the scale-set path concluded **all 7** distinct jobs (7/7, single acquirer, zero
dedup) — the fan-out is gone *by construction*, exactly as §5a-U8 predicted. That is
the strongest possible evidence both that classic's ceiling is real (the same test,
same load, opposite outcome) and that Option E is the fix, not merely *a* fix.

---

## 5. Was scale-set "what we should have built from the start"? (hindsight, fairly)

**No — building classic first was a defensible bet given what was knowable then.**
Resisting hindsight bias:

- **The protocol was not available as a supported surface at design time.** The
  runner-scale-set protocol was undocumented-internal-to-ARC (no wire spec; the ARC
  Go client had to be read at a pinned tag). GitHub only published the **official
  standalone `actions/scaleset` client** (Public Preview, MIT) in 2026 — four
  releases v0.1.0–v0.4.0, Feb–May 2026
  ([Q264 §4, §5a-U6](q264-scale-set-protocol.md#u6--wire-client-vendor-actionsscaleset-vs-gag-owned-implementation)).
  Building on it *then* would have meant reverse-engineering a **second**
  undocumented GitHub-internal protocol and betting the product on a moving,
  pre-1.0-adjacent target.
- **The classic virtual-runner bet bought real, still-valid merits:** the ~4,000×
  at-rest density advantage over ARC, per-tenant egress isolation, multi-tenant
  self-service, and zero idle compute (appendix-d §D.3–D.4). None of that was free
  elsewhere.
- **The specific failure was not cleanly foreseeable from source.** The topology
  consequence (concurrency = acquirers) was arguably visible in principle, but the
  precise *distinct-delivery starvation* behaviour only emerged under live
  high-burst dogfood — source-reading alone would not have surfaced it (indeed the
  whole Q260→Q224 saga is a sequence of live re-routes correcting source-level
  assumptions).

**Fair conclusion:** scale-set is "what we'd build today," not "what we obviously
should have built then." The classic tier also paid for itself — the protocol
knowledge, the egress/isolation/provisioner machinery, and the `scalesettest` fake
all carry straight into Option E (Q264 §3 "Carried over intact"; §6 "P1/P2 are
useful even if Option A wins").

---

## 6. The sunset decision — separated by axis

### 6.1 What the existing plan already commits to

- **Protocol** ([Q264 §5a-U8](q264-scale-set-protocol.md#u8--support-matrix-policy)):
  coexist behind `acquisitionProtocol` (default `Classic`) through P3–P4 → **flip
  default to `ScaleSet` at P5** (with the positioning-doc rewrite) → classic
  deprecated **one minor release** → classic machinery removed in an isolated PR
  **aligned with the Q74 v2beta1 graduation** (v2beta1 is ScaleSet-only).
- **API** ([v1alpha1-deprecation.md](../operations/v1alpha1-deprecation.md)):
  v1alpha1 already **deprecated but served**; removed "once v2 adoption is
  sufficient," announced as a **named release with ≥1 release of notice**; no action
  forced until then.
- **The coupling:** because ScaleSet is v2-exclusive, classic is v1alpha1's *only*
  acquisition path, so classic-machinery removal **is** the end of v1alpha1's ability
  to acquire jobs — the plan therefore sequences the classic-removal PR after
  v1alpha1 is itself deprecated, announcing both together (Q264 §5a-U8 "Consequence").

> **Open design question this review surfaces (not decided here).** The plan makes
> **v2beta1 ScaleSet-only** (Q264 §5a-U7/U8): the graduation conversion strips
> `acquisitionProtocol` and drops the classic machinery. That binds a *third* thing to
> the beta cut — it removes the classic **fallback** at the exact version GAG signals
> as "stable," while scale-set is still GitHub **Public Preview** (contract undocumented,
> `actions/scaleset#107` unanswered, already drifted once). Under this decision,
> scale-set upstream maturity ([Q272](../STATUS.md#Q272)) and *"all tenants migrated off
> classic"* become **de-facto v2beta1 blockers**, not passive watches. The alternative —
> **retain `acquisitionProtocol` through v2beta1 and drop `Classic` only at v2 GA** —
> decouples graduation from scale-set maturity and preserves the rollback lever the rest
> of this memo argues for. P4-green materially de-risks the ScaleSet-only choice (the
> protocol works live), and GAG owns its own client (not hostage to upstream's cadence),
> so ScaleSet-only-beta is defensible — but it is a genuine call that revisits a signed-off
> decision, and belongs to the maintainer + the Q264 owner. Flagged, not resolved.

### 6.2 Should either be accelerated? — recommendation

**Protocol (Axis 2): accelerate the *positioning*, hold the *removal*.**

- **Accelerate now (gated on P4 green):** name `ScaleSet` the **recommended**
  protocol for concurrency-sensitive workloads in the operator docs the moment the
  Q224 matrix is green on the flagged path, and keep the P5 default flip on its
  existing schedule (it is already the fastest *safe* point — flipping before P4
  green would ship an unvalidated default). *Pro:* classic is structurally capped
  (§4); the eight re-routes are sunk cost if classic is retiring; the protocol is
  now officially supported; adoption is pre-launch so the compat cost is near-zero
  (§6.3).
- **Do NOT pull classic-machinery *removal* forward.** Keep it at v2beta1
  graduation. *Against acceleration:* scale-set is **Public Preview** — GitHub says
  interfaces "may change," the auto-assign contract this backend actually uses is
  *undocumented* and upstream issue [actions/scaleset#107](https://github.com/actions/scaleset/issues/107)
  has had **no maintainer response in a month**, and there is a demonstrated
  breaking precedent ([actions/scaleset#75](https://github.com/actions/scaleset/issues/75)/[#90](https://github.com/actions/scaleset/pull/90) silently
  broke the GHES acquire path)
  ([Q264 §5a-U6](q264-scale-set-protocol.md#u6--wire-client-vendor-actionsscaleset-vs-gag-owned-implementation)).
  Classic also *works* for the low/moderate segment (§2) and covers GHES back to the
  documented floor — keeping the machinery one deprecation window longer strands
  nobody and preserves a fallback if a Preview wire change bites. Ripping it out
  early trades a real safety valve for a maintenance saving that the v2beta1
  alignment already captures.

**API (Axis 1): do NOT accelerate v1alpha1 removal.**

- Keep removal gated on **both** an adoption signal **and** scale-set proving out —
  *not* pulled forward. The reason is the §6.1 coupling: because removing classic
  machinery ends v1alpha1 acquisition, an accelerated v1alpha1 removal would **force
  v1 users onto a Public-Preview protocol at the API cutover**. The design already
  provides the safety valve — v1 users migrate to v2 *first* (API change via
  `gag-migrate`) while staying on `Classic` protocol, *then* opt into `ScaleSet` as a
  separate, reversible field edit (Q264 §5a-U7). Preserve that two-step; do not
  collapse it by accelerating the API removal ahead of protocol maturity.
- Deprecating the *notice* faster is fine and cheap (it is already published); it is
  the *removal* that must stay gated.

### 6.3 Adoption reality — the fact that most changes the risk math

The go-to-market posture is **pre-adoption dogfooding**, and this is decision-load-bearing:

- A `v1.0.0` tag exists and `v1.1.0-rc.*` are cut, **but** the public site is **not
  launched**, seed channels are **not started** ("gated on site + 1.0 install path"),
  and there are **no external deployers yet** — the goal is still "first handful"
  ([go-to-market.md](go-to-market.md) §8 Phase 0–1). GAG is Apache-2.0,
  **non-commercial**, deliberately donation-ready, revenue explicitly out of scope.
- **Implication:** the compat/adoption cost of *both* sunsets is currently near-zero
  — there is no external user base holding classic or v1alpha1 in production. This is
  the strongest single argument *for* moving fast. But it cuts both ways: the same
  pre-adoption state means there is **no urgent user pain forcing removal either**, so
  the conservative "hold removal, accelerate positioning" split costs nothing to
  keep. The moment a real adopter lands, the calculus tightens — which is exactly why
  an **adoption signal should gate the removal decisions** (§6.4).

### 6.4 Gating evidence (what must be true before each acceleration step)

| Step | Gate |
|---|---|
| Name ScaleSet "recommended" for concurrency-sensitive workloads | **✅ MET (2026-07-05):** P4 ran the full concurrent matrix on the scale-set path — **7/7** distinct jobs concluded, vs classic **2/7** — so the fan-out fix is confirmed by construction. (The residual `WORKER_MODE` test leak gates the *P5 clean-green cutover*, not this recommend-in-docs step.) |
| Flip default to `ScaleSet` (P5) | Clean-green dogfood re-run (the P4 residual — `WORKER_MODE` test leak — fixed on main) **+** positioning-doc rewrite landed in the same PR (Q264 §4.7) — else the vs-ARC story self-contradicts |
| Remove classic machinery | Scale-set upstream **GA / v1.0** *or* the auto-assign contract ([actions/scaleset#107](https://github.com/actions/scaleset/issues/107)) documented **+** v2beta1 graduation reached **+** v1alpha1 deprecated |
| Remove v1alpha1 API | Adoption signal shows all known tenants migrated (or confirmed dogfood-only) **+** the classic-removal gate above |

---

## 7. Security & positioning flags

- **No secure-by-default regression in the migration.** Egress isolation holds (all
  new endpoints GitHub-hosted, both listener and worker traffic stay behind the
  per-tenant proxy), workers still never see the App token, the JIT-credential surface
  is unchanged, and the admission gate (Q59) *strengthens* under the capacity header
  ([Q264 §4 "Security check"](q264-scale-set-protocol.md#4-honest-cost-list-delta-vs-the-q260-4e-estimate)).
  The default stays `Classic` (the more-conservative value) until validated — no
  security property is relaxed to enable the flip.
- **Positioning identity shifts and must move in lockstep.** The "thousands of
  goroutine-backed virtual runners" story retires; density-at-rest *improves* but the
  narrative becomes "a lighter-weight ARC listener with GAG's isolation + scheduling."
  The `why-gag` / vs-ARC marketing pages must be rewritten **in the same PR that flips
  the default** (Q264 §4.7) — flipping first would leave the public positioning
  contradicting the shipped behaviour.
- **User-visible API regression to document, not hide:** ScaleSet collapses
  `runnerLabels` to a single `runs-on` label (the scale-set name) and raises the GHES
  floor to 3.9 — real trade-offs the migration comms must state (Q264 §4.2/§4.5).

---

## 8. Follow-up items (filed on the Queue)

The two gating-signal items are **filed** as deferred, trigger-based Queue entries;
the third is folded into the Q264 P5 scope (below). These are what operationalize the
§6.4 gates:

1. **[Q271, Decision-triggered] Adoption signal to make the "v2 adoption sufficient"
   gate measurable.** Instrument or record a concrete "known tenants / are we still
   dogfood-only?" signal so the §6.4 removal gates are *measured*, not guessed. Cheap;
   unblocks a confident v1alpha1-removal decision later. Ties to
   [go-to-market.md](go-to-market.md) Phase 1.
2. **[Q272, Event-triggered / watch] Scale-set upstream maturity watch.** Track
   `actions/scaleset` reaching GA/v1.0 and the auto-assign contract (upstream
   [actions/scaleset#107](https://github.com/actions/scaleset/issues/107)) getting
   documented — the §6.4 gate for classic-machinery *removal*, and the revisit trigger
   for the [Q264 §5a-U6](q264-scale-set-protocol.md#u6--wire-client-vendor-actionsscaleset-vs-gag-owned-implementation)
   vendor-vs-own decision. Until then, classic stays the fallback.
3. **[folded into Q264 P5, not a separate item] Fold this memo's per-workload
   viability table (§2) into the operator migration docs**, so operators self-select
   Classic vs ScaleSet by their concurrency profile rather than being told "just
   switch." This is a natural part of the P5 positioning-doc rewrite (Q264 §4.7),
   tracked there rather than as its own row.

**Two things deliberately *not* done here** (flagged, not actioned): (a) the §6.4
gating table should be folded into [Q264 §5a-U8](q264-scale-set-protocol.md#u8--support-matrix-policy)
as the explicit retirement gates — left for the Q264 owner, since that plan is under
active P3–P4 work in a parallel session; (b) no `CLAUDE.md` change and no new
milestone — the protocol/API retirement is already the Q264/Q74 milestone structure,
and the live-verify-don't-trust-source meta-lesson is already a CLAUDE.md rule.

No new *structural* work is proposed: the Q264 plan is sound and this review endorses
it. The only substantive recommendation that *differs* from a naive reading of the
maintainer's hypothesis is **"accelerate positioning, hold removal, and keep the two
axes' timelines separate"** — i.e. do not fast-sunset classic or v1alpha1 ahead of
P4-green + scale-set maturity + an adoption signal.

---

## 9. Answering the maintainer's questions directly

> **Does v1 actually work?** The **classic protocol** works for individual and
> low/moderate-concurrency workloads (a large real segment) and fails *reliably* only
> at high-burst concurrency. The **v1alpha1 API** works fine — it is just monolithic
> and superseded by the decomposed v2 shape.
>
> **Does it scale? Is it performant?** It performs well (density was a real
> differentiator; scale-set improves it slightly). It **scales sub-linearly on
> reliable concurrent-matrix throughput** because of the fan-out wall, and its
> rate-limit budget grows with acquisition concurrency. Footprint is not the limit.
>
> **Is it kneecapped by the protocol problems?** Yes — at high concurrency,
> **structurally and unfixably** from the AGC side. Not below that line.
>
> **Is v2/scale-set what we should have built from the start?** It is what we'd build
> *today*. Building classic first was a defensible bet — the protocol wasn't a
> supported surface then, and classic's merits were real.
>
> **Can we sunset v1 faster?** Partly, and only on one axis: **accelerate the
> scale-set *positioning*** (recommend it for concurrent workloads once P4 is green),
> but **hold both removals** (classic machinery and the v1alpha1 API) to the existing
> plan, gated on P4-green + scale-set maturity + an adoption signal. "v1 is
> fundamentally broken" is not the right frame; "classic is capped at scale and the
> sunset should be *sequenced*, not *rushed*" is.
