# Q260 re-fix — re-key job-acquisition dedup on `planID`

**Status:** code-complete, awaiting dogfood re-route (do not close Q224/Q260 off code alone).

## Problem

The first Q260 fix (commit `c850764`, #503) deduped job acquisition by claiming the
per-delivery `RunnerRequestID` **before** `AcquireJob`. Live turn-up #2 (2026-07-03,
recorded in [`gke-dogfood.md`](gke-dogfood.md)) proved it **ineffective**: GitHub's
broker fan-out delivers one job to N sibling listener sessions as messages with
**distinct** `RunnerRequestID`s. Each sibling's `claimJob(distinctReqID)` succeeds, all
pass the gate, all acquire, and all collide on the **shared** per-job worker Secret
`job-<planID>` (`secrets "…" already exists`). One wins; the rest burn their runner slot
(busy but pod-less) and the pool collapses 8→2.

## Root cause — wrong key

- The colliding Secret is named from the job's **`planID`** — `resp.Plan.PlanID`, from
  the AcquireJob **response** ([`provisioner.go:541`](../../cmd/agc/internal/provisioner/provisioner.go)
  `secretName := "job-" + safeName(planID)`; the pod name too). `planID` is the
  codebase's per-job unique identity.
- The pre-acquire broker message ([`RunnerJobRequestBody`](../../broker/types.go)) carries
  only `RunnerRequestID` + `RunServiceURL` + `BillingOwnerID` — **no** plan id. Siblings
  get distinct `RunnerRequestID`s, so the pre-acquire gate never collapses the fan-out.
- `planID` is only known **post-acquire**, so the dedup must move to a post-acquire,
  pre-provision gate keyed on `planID`.

## Fix

Re-key the Multiplexer's shared in-flight claim registry from `RunnerRequestID` to
`planID`, and move the gate in `handleJob` from **pre-**`AcquireJob` to **post-acquire,
pre-provision**:

1. Parse job body → admission gate (Q59, unchanged) → `AcquireJob` (sets `planID`,
   `acquired=true`, `MarkAgentConsumed`).
2. **New gate:** `release, ok := cfg.ClaimJob(planID)`.
   - `ok=false` (a sibling already owns this planID): increment
     `jobs_duplicate_delivery_total`, skip `SpawnReplacement`/renew/provision, and
     `return acquired /*true*/, nil` — the caller recycles the consumed single-use runner
     back online, so the slot is reclaimed cleanly (no burned slot, no "already exists").
   - `ok=true`: `defer release()` (held for the whole job; released on completion or
     abandonment so a later GitHub redelivery is provisionable).
3. `SpawnReplacement` → renew loop → provision (unchanged), for the winner only.

### Decision: replace, not complement

The pre-acquire `RunnerRequestID` gate is removed, not kept alongside the planID gate:
- It never fired in production (siblings' ids differ — proven live).
- The planID gate subsumes its only correct case: any two deliveries that would collide
  on `job-<planID>` share a planID by definition, so identical-message redelivery is
  deduped too.
- One registry / one gate / one metric is simpler than two.

**Accepted trade-off:** planID is post-acquire, so losing siblings do briefly acquire
(marking the runner busy) before deduping — one recycle reclaims the slot; the pool no
longer collapses. A pre-acquire dedup would need a stable pre-acquire key;
`RunServiceURL`'s cross-sibling stability is unproven in the live evidence, so it is not
used (avoids shipping a third speculative key).

## Tests (must fail against `c850764`)

- **Unit** ([`multiplexer_test.go`](../../cmd/agc/internal/listener/multiplexer_test.go)):
  rework `TestMultiplexer_DuplicateJobDeliveryProvisionsOnce` so each sibling gets a
  **distinct** `RunnerRequestID` but AcquireJob returns a **constant** `planID`. Peak
  concurrent provisions must be 1; N-1 deduped. Against `c850764` (reqID key) all N
  provision → peak = N.
- **Unit** ([`goroutine_test.go`](../../cmd/agc/internal/listener/goroutine_test.go)):
  `TestListener_DuplicateJobDeliverySkipsAcquire` reworked to the post-acquire semantics —
  the loser **does** acquire (post-acquire gate) but does **not** provision, increments
  the metric, and returns `acquired=true` (runner recycles).
- **Integration/envtest**
  ([`controller/integration`](../../cmd/agc/internal/controller/integration)): real
  provisioner + real apiserver. Fix the AcquireJob response to a constant planID; drive
  the baseline session to win + hold the planID claim (blocked in `waitForCompletion`),
  then deliver the **same** planID job (distinct reqID) to the replacement session —
  assert exactly one `job-<planID>` Secret, the duplicate-delivery metric rises, and the
  loser is not wedged. Against `c850764` the loser (distinct reqID) is not deduped and
  hits the real `AlreadyExists`, so the metric stays flat → test fails.

## Docs

- `metrics.go` help + `docs/operations/observability.md`: metric now counts a
  post-acquire planID-claim skip (was pre-acquire).
- `docs/operations/troubleshooting.md`: update the Q260 section to the planID key.
- `docs/plan/gke-dogfood.md`: record the re-fix; Q224/Q260 stay open pending re-route.
