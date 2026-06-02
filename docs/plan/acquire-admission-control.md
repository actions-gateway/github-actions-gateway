# Pre-Acquisition Admission Control (Capacity-Gated `acquirejob`)

Status: ⓘ Design sketch — not started. Tracked as [Q59](../STATUS.md).

## The problem in one sentence

The AGC claims a job from GitHub (`acquirejob`) **before** it knows whether
it has capacity to place a worker pod for it, so under capacity pressure it
acquires jobs it cannot run — wasting the claim and, in the worst case,
getting the run cancelled rather than redelivered.

## Current behavior (verified against code)

The acquire-then-place ordering lives in the listener's `handleJob`
([`cmd/agc/internal/listener/goroutine.go`](../../cmd/agc/internal/listener/goroutine.go)):

1. Long-poll `GET /message` returns a job.
2. **`AcquireJob`** is called (`goroutine.go:341`) — this claims the job from
   GitHub. From here GitHub considers the job *owned by this session*.
3. A replacement listener is spawned (`SpawnReplacement`, line 359).
4. **`StartRenewLoop`** begins (`goroutine.go:370`) with `defer stop()`.
5. **`JobHandler`** runs (line 378) → the provisioner's `Provision`.

Only in step 5 does capacity get evaluated. In
[`cmd/agc/internal/provisioner/provisioner.go`](../../cmd/agc/internal/provisioner/provisioner.go):

- **Ceiling check** (`provisioner.go:232`, `ceilingCheck`): counts active pods
  against `maxWorkers` / `priorityTiers`. If the ceiling is reached the
  provisioner deletes the staged Secret and returns
  `concurrency ceiling reached`.
- **Quota retry** (`createPodWithQuotaRetry`): on a `403 Forbidden / exceeded
  quota` from the API server, retries the pod create in place up to
  `maxQuotaRetries` (default 5) with `quotaRetryDelay` (default 30s) — **while
  holding the GitHub job lock**.

### Two distinct failure shapes fall out of this ordering

1. **Ceiling-held job is dropped after acquisition.** When `ceilingCheck`
   returns `held`, `JobHandler` returns an error, `handleJob` returns, and the
   `defer stop()` halts renewal. The job was *already acquired* but no worker
   ever runs and the lock is no longer renewed. Per the lock-expiry contract in
   [`docs/design/04-operational-flows.md`](../design/04-operational-flows.md)
   and [`02-architecture.md` §2.2](../design/02-architecture.md), a job whose
   renewal window lapses is **cancelled** by GitHub — *not* redelivered. So a
   capacity rejection that arrives one step too late converts "GitHub would
   have handed this to another session in ~2 min" into "the run is cancelled
   and needs a manual re-run."

   > ⚠️ End-to-end verification needed (do not trust this source-read). The
   > exact GitHub outcome for *acquired → never renewed → no rerun called* must
   > be confirmed on a real broker session before treating cancellation as
   > certain. This is precisely the source-read-vs-exec gap CLAUDE.md calls out.

2. **Quota-rejected job blocks a goroutine holding the lock.** The acquire
   committed the claim, so the provisioner cannot cheaply hand the job back —
   it retries in place for up to `5 × 30s = 150s`, occupying a goroutine and a
   live 10-minute lock the whole time, betting that quota frees up. Reasonable
   as a last resort, but it only exists *because* we already acquired.

Both shapes share one root: **the capacity decision happens after the
irreversible step.**

## Proposed direction: gate capacity *before* `acquirejob`

Add an **admission check between `GET /message` returning a job and the
`AcquireJob` call** in `handleJob`. If the gate says "no capacity," the
listener does **not** acquire — it leaves the job in GitHub's queue, where the
existing redelivery contract hands it to another session (this one, on a later
poll, or a burst-spawned sibling) within the delivery window. This is a
**concurrency gate, not a queue** — see "Rejected alternative" below.

This is deliberately the smallest change that moves the decision to the right
side of the acquire. It does **not** remove the post-acquire `ceilingCheck` or
quota retry; those stay as the backstop for races the pre-check can't close.

### Sketch of the gate

```
GET /message returns job
        │
        ▼
  ┌─────────────────────────┐
  │ admit(rg) → ok | full    │   ← new: cheap, in-memory capacity probe
  └─────────────────────────┘
   ok │            │ full
      ▼            ▼
  AcquireJob   skip acquire; continue long-poll loop
                (job stays queued at GitHub → redelivered)
```

Key design questions to resolve in implementation:

1. **What does `admit` count, and from where?** The authoritative count today
   is `activePodCount` (a live `List` against the API server in the
   provisioner). Calling that synchronously on the hot path before *every*
   acquire adds an API round-trip per job and is racy under burst. Options:
   - **In-memory reservation counter** owned by the provisioner/multiplexer,
     incremented at admit and decremented on pod terminal state. Fast, but is
     soft state lost on AGC restart (acceptable — fail-safe, like the eviction
     counter; budget resets generously, never starves).
     **Leading candidate.**
   - **Informer-backed pod cache** (ties into [Q32](../STATUS.md), which already
     wants the provisioner to *watch* pods instead of polling). The admit check
     reads the cache instead of the API server. Best long-term; pairs naturally
     with Q32's `Owns(&Pod)`.
   - Keep the live `List` but move it pre-acquire. Simplest, slowest, racy.

2. **Reservation vs. observed count.** A pure observed-pod count
   double-admits under burst: N listeners can all see "room for one more"
   before any pod is created. The counter must reserve at admit time
   (`reserved + running < ceiling`) and release on terminal state or acquire
   failure. This reservation is the actual new primitive.

3. **Priority tiers.** `ceilingCheck` returns a `PriorityClass` based on
   cumulative count; the admit gate must reproduce the *last-tier ceiling*
   decision (admit iff below the final tier's ceiling) without needing to
   assign the class yet — class assignment can stay in the provisioner.

4. **Quota is not locally observable.** ResourceQuota exhaustion is only known
   when the API server rejects the create. The pre-acquire gate cannot predict
   it, so the post-acquire `createPodWithQuotaRetry` path must remain. The gate
   reduces how often we reach it, not whether it exists. Consider lowering the
   default `maxQuotaRetries`/`quotaRetryDelay` once admission control absorbs
   most of the pressure — but that's a follow-up, not part of this change.

5. **Where the gate lives.** `handleJob` already depends on `cfg` callbacks
   (`SpawnReplacement`, `JobHandler`). Add an `AdmitFunc`-style hook
   (`func(ctx) (release func(), ok bool)`) so the listener stays decoupled from
   the provisioner and the reservation lifecycle is explicit per the CLAUDE.md
   async/ownership conventions. `release` is called on acquire failure or pod
   terminal state.

### What this buys

- Capacity rejections leave the job **redeliverable** instead of acquired-then-
  cancelled (closes failure shape 1).
- Far fewer jobs reach the in-place quota-retry stall (mitigates shape 2).
- Steady-state behavior is unchanged — the gate is a no-op until pods approach
  the ceiling.

## Rejected alternative: a durable internal job queue

Adding the AGC's *own* persistent queue of pending/retrying jobs was
considered and rejected:

- **GitHub already is the durable queue.** Jobs queue in GitHub's broker;
  unacquired jobs are redelivered within the ~2-minute window. An internal
  queue duplicates that contract and creates a two-sources-of-truth
  reconciliation problem.
- **It breaks the stateless-AGC pillar.** The AGC is `replicas: 1` with an
  in-memory session registry precisely so it holds no durable state; HA is
  job-level via GitHub redelivery ([`02-architecture.md` §2.2](../design/02-architecture.md)).
  A persistent queue forces a datastore + leader election or a shared store.
- **Durability buys little here.** The one piece of state we lose on restart —
  retry counters — is fail-safe to lose (budgets reset generously).
- **Fairness/ordering needs don't exist by design.** Tenants are isolated into
  separate AGC instances; within a tenant GitHub owns ordering.

The valuable idea hiding inside "should we add a queue?" was *backpressure*,
and the cheap way to get backpressure is the admission gate above — not a
stateful queue subsystem. If a future requirement genuinely needs durable
cross-restart job state (it does not today), revisit this section before
building it.

## Relationship to Kueue (why an off-the-shelf k8s queue isn't the admission layer)

[Kueue](https://kueue.sigs.k8s.io/) is the popular Kubernetes-native job
queueing / quota manager (ClusterQueue, LocalQueue, ResourceFlavor, Cohort
borrowing, `WorkloadPriorityClass` preemption). It is a natural thing to reach
for when someone asks "why not just put a priority queue in front of the
runners?" — and it is sometimes layered under ARC for GPU/quota management
(e.g. the pattern vendors like Exostellar use; *specifics to verify in the
[Q60](../STATUS.md) competitive analysis*). So the design must say explicitly
why GAG does not delegate admission to it.

**Kueue gates the wrong layer for this problem.** Per its own docs, Kueue
"decides when a job should wait, when a job should be admitted to start (as in
pods can be created), and when a job should be preempted" — it operates *only*
on Kubernetes resources and controls **pod creation**. GAG's admission
decision, by contrast, has to happen one layer up: at `acquirejob` against the
**GitHub broker**, before any Kubernetes object exists. Kueue has no visibility
into the broker and cannot queue a job that is not yet a Kubernetes workload.

**Even as a complement, the layering fights the broker contract.** Suppose a
cluster already runs Kueue and GAG's worker pods participate in a ClusterQueue.
The moment Kueue *defers* a worker pod (its whole job), GAG is back in the
failure shape this plan exists to fix: the job was already claimed from GitHub
at `acquirejob`, the 10-minute lock is ticking, and the pod that would do the
work is sitting in a Kueue queue. GitHub's "claim within ~2 min, then you own
it and must run it" semantics are fundamentally incompatible with queueing the
work *after* the claim. Admission has to be decided **before** the claim —
upstream of anything Kueue can act on.

**Operational mismatch too.** Kueue requires cluster-admin install (CRDs,
webhooks, cluster-wide controller). GAG's stated requirement is self-service
tenant onboarding without cluster-admin involvement per team
([Appendix D intro](../design/appendix-d-alternatives-considered.md)). Making
Kueue a hard dependency regresses that.

**Where Kueue *is* complementary:** in clusters that already run it, GAG's
worker pods can still be Kueue-managed for cluster-level quota/preemption at the
pod layer — GAG's per-`RunnerGroup` admission gate (this plan) and Kueue's
cluster-wide quota are not mutually exclusive. GAG's gate decides *whether to
claim*; Kueue, if present, can still arbitrate the resulting pod against
cluster quota. The point is that Kueue **augments** the pod layer; it cannot
**replace** the pre-acquire gate. This is the comparison to land in the docs.

## Scope / testing

- **Code:** `handleJob` admit hook (`goroutine.go`), reservation counter +
  `release` lifecycle (provisioner or multiplexer), wiring in the multiplexer.
- **Metrics:** add `actions_gateway_jobs_admission_rejected_total{namespace,
  runner_group}` (gate said full, acquire skipped) and keep the existing
  ceiling/quota counters as the post-acquire backstop signal. A persistent gap
  between admission-rejected and the old ceiling-held counter tells operators
  the gate is working.
- **Docs to update when implemented:** [`02-architecture.md`](../design/02-architecture.md)
  (Session Multiplexer / Pod Provisioner prose + metrics table),
  [`04-operational-flows.md`](../design/04-operational-flows.md) (job-acquisition
  flow), [`03-api-contracts.md`](../design/03-api-contracts.md) if any
  RunnerGroup field is added, and a troubleshooting note for "jobs not being
  acquired despite queued work" (gate saturated).
- **Document the *why*, not just the *what* (required, not optional).** When
  this ships, the rationale must land in **human-facing docs**, not only this
  plan doc. Specifically:
  - Add a section to [`appendix-d-alternatives-considered.md`](../design/appendix-d-alternatives-considered.md)
    (it already runs D.1–D.4; this is a natural **D.5 — Kueue / k8s job-queue
    managers**) capturing two things: (1) why admission is gated *before*
    `acquirejob` rather than via a durable internal queue, and (2) the
    **Kueue comparison** from the "Relationship to Kueue" section above —
    Kueue gates pod creation *below* the broker layer, cannot see the
    `acquirejob` decision, and so augments rather than replaces GAG's gate.
  - Add a short line to the README's ARC/KEDA comparison block noting that GAG
    handles admission at the broker-claim layer (where a k8s queue manager like
    Kueue structurally cannot operate), linking to the appendix for depth.
  - Keep this plan doc's "Rejected alternative" and "Relationship to Kueue"
    sections as the source the appendix is distilled from.
- **Test tier:** unit-test the reservation arithmetic (double-admit under
  burst, release on failure, restart resets). The acquired-then-cancelled
  outcome and the redelivery-after-skip behavior are **Tier-A kind e2e**
  territory — they cross the real broker contract and cannot be proven by
  envtest. This is the same class of bug PR #59's planned
  `E2E_GMC_TenantProvisioning_*` test exists to catch.

## Open questions

- Confirm (live) whether a ceiling-held, already-acquired job is cancelled vs.
  redelivered. The whole priority of this work hinges on the answer.
- Decide reservation-counter vs. informer-cache for the gate's count source —
  ideally settle it *with* [Q32](../STATUS.md) so the provisioner grows one
  pod-watching mechanism, not two.
- Should the gate be per-`RunnerGroup` only, or also enforce an AGC-wide
  aggregate ceiling? Per-RG matches today's `ceilingCheck`; an aggregate cap
  would be new policy and should be raised separately.
