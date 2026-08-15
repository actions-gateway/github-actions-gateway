# Q260 re-fix — re-key job-acquisition dedup on `planID`

**Status:** the planID dedup is live-validated **effective** — the dedup fires on the shared `planID` and the burst-start Secret-collision collapse does **not** recur — and as of re-route #4 (2026-07-04) the **capacity (Q248) and collision residuals are both fixed** (3 non-preempted nodes; **0 Secret/Pod `already exists` in two bursts**).
But the concurrent matrix is still **not fully green**: the remaining blocker has shifted to GitHub's broker **fan-out completion/assignment accounting**, which is distinct from and beyond this dedup, and which the #513 completejob-abandon flag does **not** resolve (it makes the end-state worse — keep it OFF).
Full evidence in [`gke-dogfood.md`](gke-dogfood-turnup-findings.md) re-route #4 (and the re-route #4 update block below).
**Q224/Q260/Q242 stay open.**

> **Update (redelivery residual code-complete).** Residual (2) — the late-redelivery Pod-collision — is **fixed in code** (this PR): the released `planID` claim now **lingers** for the pod's `completedPodTTL` window, so a late redelivery is deduped instead of colliding on the winner's lingering Completed pod.
> See Follow-up item 1 below.
> The deeper completion-vs-15-min-cancel *accounting* gap (item 2) now has its run-service protocol call — `broker.CompleteJob` + a guarded loser-abandon path (a follow-up PR to #512) — but it stays **off by default** pending live confirmation of GitHub's per-delivery completion semantics.
> Residual (1) (Q248 capacity) is a cluster task for the combined re-route #4.
> Q224/Q260/Q242 stay open until that turn-up reconfirms green (and, for item 2, confirms the semantics).
>
> **Update (re-route #4, 2026-07-04 — live-tested; #513 flag does NOT resolve, keep OFF).** The combined capacity + flag-on/flag-off turn-up ran on stable non-preemptible capacity (full evidence in [`gke-dogfood.md`](gke-dogfood-turnup-findings.md) re-route #4).
> Findings: **capacity (Q248) and collisions (#512) are both FIXED** — 3 non-preempted nodes, and **0 Secret/Pod `already exists` collisions in both bursts** (10 dedup events each).
> But **neither flag state reaches green**: the blocker is now GitHub's broker **fan-out completion/assignment accounting**, distinct from and beyond this dedup.
> With the flag **ON**, `completejob(result= skipped)` returns HTTP-OK (14/15; 1× `401 "Not authorized for this job"`) **but does not conclude the job** — a late redelivery re-assigns the already-run job to a replacement session and GitHub holds it **indefinitely `in_progress`**; by acking the delivery it even *suppresses* the 15-min unstarted-cancel that would otherwise resolve it.
> With the flag **OFF** (control, only the completejob path differs) the same jobs reach **terminal `failure`/`cancelled`** (the Q259 recycle 422 blocks listener reuse → trivial jobs cancelled at the unstarted-timeout).
> **The flag makes the end-state worse, so it stays OFF by default.** Semantics answered: the run service **accepts** the `skipped` serialization but does not finalize the job on that call, and job-scoped completion auth is unreliable (the 401).
> **Item 2's completejob-abandon approach is therefore not the fix** — the real work is reconciling GitHub's per-delivery fan-out with the AGC's one-runner-per-session model (runner recycle + job completion), tracked under Q260/Q224.
> Q224/Q260/Q242 stay open.

## Follow-up (post-#3): close the residual before re-validating green

1. **Release the `planID` claim only after the worker Pod is garbage-collected**, not at job completion — so a post-completion redelivery of a slow job is deduped rather than colliding on the lingering Completed pod. ✅ **DONE (code-complete, awaits re-route #4).** The Multiplexer's shared claim registry now **retains** a released `planID` claim for a linger window sized to the owner's `completedPodTTL` (the exact window the winner's terminal pod lingers before the reaper GCs it), instead of deleting it on completion.
   A late redelivery arriving during that window is deduped at the post-acquire `planID` gate (counted on `actions_gateway_jobs_duplicate_delivery_total`) and never re-enters the provisioner — so there is **no `create Pod … already exists`** and no error surfaced as a job cancel.
   Once the linger elapses (pod reaped), the `planID` is reclaimable, so a genuine redelivery after GC still provisions.
   Expired lingering entries are swept lazily, keeping the map bounded.
   - Wiring: `Multiplexer.ClaimLinger` set from `provisioner.EffectiveCompletedPodTTL(rg)` (v1) / `CompletedPodTTLOrDefault(rs.Spec.CompletedPodTTL)` (v2).
     `ClaimLinger == 0` (owner reaps terminal pods synchronously, so none linger) keeps the original delete-on-completion path.
   - Regression: `TestAGC_Q260_LateRedeliveryAfterCompletionDedups` (envtest — winner completes, its pod lingers, a late redelivery of the same planID must be deduped, not a 2nd `create Pod`) + Multiplexer unit tests for the linger/sweep semantics.
     The envtest **fails against the pre-fix delete-on-completion behavior** with the exact residual signature (`create Pod runner-…-<planid>: pods "…" already exists`).
2. **Reconcile GitHub's per-delivery job-assignment timeout with the dedup-to-one-delivery model.** ➜ **Designed + repro landed in [`q260-fanout-completion-reconciliation.md`](q260-fanout-completion-reconciliation.md).** The #513 immediate-loser-`skipped` path below is a **confirmed dead-end** (re-route #4); the recommended fix is instead a **winner-driven completion fan-out** to all sibling deliveries on completion, gated pending a live re-route #5 (one unknown: does GitHub honour a non-running delivery's completion?).
   An offline, deterministic repro of the accounting gap now fails-today and gates that fix — no turn-up needed.
   The remainder of this item is the superseded #513 record.

   A fanned-out job whose winner completes via one delivery can still be cancelled at the 15-min unstarted-timeout on a *deduped* sibling delivery (observed: `tidy-check`'s pod reported "Job completed" yet GitHub cancelled the job).
   Investigate whether the loser should acknowledge/complete its delivery rather than silently skip.
   - **Mechanism landed, guarded (PR follow-up to #512).** `broker.Client.CompleteJob` (`POST {run_service_url}/completejob`, job-scoped auth like `renewjob`) was added, and the `handleJob` loser path now — *when enabled* — releases its acquired-but-unrun assignment via `completejob` on the loser's **own** `jobID` (`RunnerRequestID`, distinct per sibling) with result `skipped`, tracked by `actions_gateway_abandoned_delivery_completions_total`.
     Keying on the loser's own `jobID` (not the shared planID) is safe **iff** the run service scopes completion per-delivery, which the `renewjob` per-`(planID, jobID)` model (Q247, live-confirmed) strongly implies but does **not** prove for the *completion* path.
   - **OFF BY DEFAULT** (`AGC_COMPLETE_ABANDONED_DELIVERIES=true`; `listener.Config.CompleteAbandonedDeliveries`).
     Rationale: if completion is scoped by `planID` (not `jobID`), the loser's call would finalize the winner's still-running job — a strict regression on live workflows.
     Secure-by-default keeps it opt-in until confirmed.
   - **STILL NEEDS item 1 (a live dogfood turn-up)** to confirm: (a) that a deduped `acquirejob` really creates an independently-tracked assignment GitHub cancels at 15 min even after the winner completes; (b) that `completejob` on the loser's own `jobID` resolves *only* that assignment (not the winner's); and (c) the exact `result` wire serialization (`skipped` lowercase vs PascalCase vs integer enum — currently modeled, unverified).
     The turn-up that gathers this evidence should also flip the flag on and validate the fix — do both in one turn-up.
     Capture the run-service request/response in `gke-dogfood.md`.
3. **Stable worker capacity for the re-validation** (Q248): the spot pool preempted to 1 node, confounding throughput.
   Re-run on non-spot or ≥3 held nodes so a non-green result can be attributed cleanly.
   *(Cluster task; owned by the dispatcher's combined capacity + re-route #4 turn-up, not this code change.)*

## Problem

The first Q260 fix (commit `c850764`, #503) deduped job acquisition by claiming the per-delivery `RunnerRequestID` **before** `AcquireJob`.
Live turn-up #2 (2026-07-03, recorded in [`gke-dogfood.md`](gke-dogfood-turnup-findings.md)) proved it **ineffective**: GitHub's broker fan-out delivers one job to N sibling listener sessions as messages with **distinct** `RunnerRequestID`s.
Each sibling's `claimJob(distinctReqID)` succeeds, all pass the gate, all acquire, and all collide on the **shared** per-job worker Secret `job-<planID>` (`secrets "…" already exists`).
One wins; the rest burn their runner slot (busy but pod-less) and the pool collapses 8→2.

## Root cause — wrong key

- The colliding Secret is named from the job's **`planID`** — `resp.Plan.PlanID`, from the AcquireJob **response** ([`provisioner.go:541`](../../../cmd/agc/internal/provisioner/provisioner.go) `secretName := "job-" + safeName(planID)`; the pod name too).
  `planID` is the codebase's per-job unique identity.
- The pre-acquire broker message ([`RunnerJobRequestBody`](../../../broker/types.go)) carries only `RunnerRequestID` + `RunServiceURL` + `BillingOwnerID` — **no** plan id.
  Siblings get distinct `RunnerRequestID`s, so the pre-acquire gate never collapses the fan-out.
- `planID` is only known **post-acquire**, so the dedup must move to a post-acquire, pre-provision gate keyed on `planID`.

## Fix

Re-key the Multiplexer's shared in-flight claim registry from `RunnerRequestID` to `planID`, and move the gate in `handleJob` from **pre-**`AcquireJob` to **post-acquire, pre-provision**:

1. Parse job body → admission gate (Q59, unchanged) → `AcquireJob` (sets `planID`, `acquired=true`, `MarkAgentConsumed`).
2. **New gate:** `release, ok := cfg.ClaimJob(planID)`.
   - `ok=false` (a sibling already owns this planID): increment `jobs_duplicate_delivery_total`, skip `SpawnReplacement`/renew/provision, and `return acquired /*true*/, nil` — the caller recycles the consumed single-use runner back online, so the slot is reclaimed cleanly (no burned slot, no "already exists").
   - `ok=true`: `defer release()` (held for the whole job; released on completion or abandonment so a later GitHub redelivery is provisionable).
3. `SpawnReplacement` → renew loop → provision (unchanged), for the winner only.

### Decision: replace, not complement

The pre-acquire `RunnerRequestID` gate is removed, not kept alongside the planID gate:
- It never fired in production (siblings' ids differ — proven live).
- The planID gate subsumes its only correct case: any two deliveries that would collide on `job-<planID>` share a planID by definition, so identical-message redelivery is deduped too.
- One registry / one gate / one metric is simpler than two.

**Accepted trade-off:** planID is post-acquire, so losing siblings do briefly acquire (marking the runner busy) before deduping — one recycle reclaims the slot; the pool no longer collapses.
A pre-acquire dedup would need a stable pre-acquire key; `RunServiceURL`'s cross-sibling stability is unproven in the live evidence, so it is not used (avoids shipping a third speculative key).

## Tests (must fail against `c850764`)

- **Unit** ([`multiplexer_test.go`](../../../cmd/agc/internal/listener/multiplexer_test.go)): rework `TestMultiplexer_DuplicateJobDeliveryProvisionsOnce` so each sibling gets a **distinct** `RunnerRequestID` but AcquireJob returns a **constant** `planID`.
  Peak concurrent provisions must be 1; N-1 deduped.
  Against `c850764` (reqID key) all N provision → peak = N.
- **Unit** ([`goroutine_test.go`](../../../cmd/agc/internal/listener/goroutine_test.go)): `TestListener_DuplicateJobDeliverySkipsAcquire` reworked to the post-acquire semantics — the loser **does** acquire (post-acquire gate) but does **not** provision, increments the metric, and returns `acquired=true` (runner recycles).
- **Integration/envtest** ([`controller/integration`](../../../cmd/agc/internal/controller/integration)): real provisioner + real apiserver.
  Fix the AcquireJob response to a constant planID; drive the baseline session to win + hold the planID claim (blocked in `waitForCompletion`), then deliver the **same** planID job (distinct reqID) to the replacement session — assert exactly one `job-<planID>` Secret, the duplicate-delivery metric rises, and the loser is not wedged.
  Against `c850764` the loser (distinct reqID) is not deduped and hits the real `AlreadyExists`, so the metric stays flat → test fails.

## Docs

- `metrics.go` help + `docs/operations/observability.md`: metric now counts a post-acquire planID-claim skip (was pre-acquire).
- `docs/operations/troubleshooting.md`: update the Q260 section to the planID key.
- `docs/plan/gke-dogfood.md`: record the re-fix; Q224/Q260 stay open pending re-route.
