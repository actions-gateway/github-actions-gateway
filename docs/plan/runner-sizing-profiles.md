# Worker Right-Sizing Profiles (Recommendations First)

> **Status: 🔄 Phases 1–3 shipped 2026-07-21/22 — tracked as [Q359](../STATUS.md#Q359).**
> Remaining: live dogfood validation of all three phases (rides the next
> dogfood session). This doc is the design sketch and phase record.

## Goal

Automatically right-size worker pod CPU/memory `requests`/`limits` per runner
shape from usage observed across the jobs that actually ran on that shape —
recommendations first, opt-in actuation later.

## Why

**This is a differentiator versus Actions Runner Controller (ARC).** ARC has no
sizing feedback loop: operators guess runner resource specs, and the guess is
rarely revisited. Our own dogfood proved both the value and the toil of doing it
manually — [dogfood-runner-rightsizing.md](dogfood-runner-rightsizing.md)
started from "every worker pod's original `requests`/`limits` were an unmeasured
guess" and spent multiple sessions measuring peaks and deriving values by hand.
This plan automates that loop for every tenant.

The payoff concentrates where nodes are expensive:

- **GPU bin-packing.** Jobs select a runner shape by label (e.g. GPU count);
  within a shape, CPU/memory demand varies. Right-sized CPU/memory requests keep
  the GPU count — not an inflated CPU ask — the binding constraint, maximizing
  runners per GPU node.
- **Throughput tuning.** The opposite trade is also legitimate: oversize the
  shape so jobs burst and finish faster (GPU idle time while CPU-bound steps run
  is waste). A profile makes the trade explicit instead of accidental.

## Constraints (why this is GAG-native, not a VPA bolt-on)

- **Stock VPA cannot target worker pods.** VPA's `targetRef` requires a
  controller with a `/scale` subresource to group pods; worker pods are owned by
  the `RunnerSet` CRD (`cmd/agc/internal/controller/runnerset_target.go`), which
  has no scale semantics — `replicas` is meaningless in a scale-to-zero design.
- **Actuation must happen at pod-build time.** Worker pods live minutes;
  evict-and-resize (VPA's actuation) is useless. The AGC provisioner already
  constructs every worker pod from the template
  (`cmd/agc/internal/provisioner/pod.go`), so applying a profile needs no
  webhook — it is a pod-build step.
- **GPU counts are never touched.** Extended resources are integer,
  job-selected via runner labels, and part of the shape's identity. Profiles
  shape CPU/memory *around* the fixed GPU allocation only.
- **Tenant-authored templates stay authoritative by default.** `podTemplate` on
  `RunnerTemplate` is the tenant's; any auto-apply is opt-in per `RunnerSet`
  and clamped (see Phase 3 safety rails).

## Phases

### Phase 1 — per-RunnerSet usage observability (M) — ✅ shipped 2026-07-21

The AGC samples CPU/memory usage for the worker pods it owns and aggregates per
`RunnerSet` × container. Implementation: `cmd/agc/internal/usage/` (a
`manager.Runnable` ticker poller wired in `main.go`); operator docs:
[worker-rightsizing.md](../operations/worker-rightsizing.md) +
[metrics reference](../operations/observability-metrics.md#worker-usage--right-sizing-metrics-q359).

Decisions taken at pickup (open questions 1–2):

- **Metrics source: `metrics.k8s.io`** (metrics-server), as the default
  candidate argued — typed clientset, one PodMetrics list per tick (default
  15s, `WORKER_USAGE_SAMPLE_INTERVAL`, `0`/`off` disables), RBAC =
  `pods.metrics.k8s.io` get/list added to the marker role **and** the
  hand-maintained `agc-tenant-role` fragment. Degrades gracefully when absent
  (throttled log + `…_poll_errors_total`); jobs shorter than one interval are
  counted in `…_jobs_unsampled_total` so coverage is judgeable.
- **Aggregation/export: Prometheus histograms of per-job peaks** rather than
  in-process p50/p95 summaries — `histogram_quantile` gives any-window
  quantiles, survives AGC restarts (rate/window queries), and aggregates across
  replicas; in-process state reduces to per-pod running peaks plus a max-peak
  gauge per RunnerSet × container (since-start; `max_over_time` bridges
  restarts). The in-memory aggregate window question (Q2) is thereby deferred
  to Phase 2, where status persistence genuinely needs one.
- **Ownership scoping:** candidate pods (label `actions-gateway.com/runner-set`)
  are matched against the RunnerSets in the manager cache, which is already
  namespace- and gateway-scoped — co-located multi-gateway AGCs never
  double-count.

**Acceptance:** metrics visible for a live RunnerSet after a handful of jobs;
recipe reproduces the dogfood derivation without ad-hoc `kubectl top` scraping.
Unit-tested (peak-max across ticks, single finalize, unsampled short jobs,
disappeared pods, unowned pods, poll errors); live-cluster validation rides the
next dogfood benchmark session.

### Phase 2 — recommendations in `RunnerSet.status` (M) — ✅ shipped 2026-07-21

- `status.sizingRecommendation`: per-container recommended `requests`/`limits`
  (derived from Phase 1 aggregates), plus sample count and window so operators
  can judge confidence.
- A condition (e.g. `SizingDrift`) when the template's ask deviates from the
  recommendation beyond a threshold in either direction (waste or OOM risk).
- **Persistence:** aggregates flush into status periodically and merge back on
  AGC restart — status is the store; no new backing store.
- Recommendations are advisory only in this phase; nothing changes pod specs.

**Acceptance:** a deliberately oversized RunnerSet shows a lower recommendation
and the drift condition within N jobs; restart does not zero the history.

Decisions taken at pickup (implementation: `cmd/agc/internal/usage/aggregate.go`
+ `cmd/agc/internal/controller/runnerset_sizing.go`; design summary in
[appendix H §H.7](../design/appendix-h-v2-api-decomposition.md#h7-reference-integrity--runtime-conditions-not-admission)):

- **Aggregation window (open question 2): unbounded-since-window-start, no
  decay.** The sampler keeps fixed-bucket per-job-peak histograms (the Phase 1
  Prometheus bucket edges, shared so the two views can't disagree) per
  RunnerSet × container. No rolling window: sizing wants the whole observed
  envelope, the operator judges staleness via `windowStart`/`sampleCount`, and
  a decay policy can arrive with Phase 3's confidence machinery if live use
  shows drift-over-time matters. Memory stays bounded by container-name
  cardinality regardless.
- **Persistence is approximate by design.** Status stores per container:
  observed p95 + max + count (+ window start), not the full histogram. On
  restart the sampler re-seeds 95% of the count at the p95 and the rest at the
  max — exactly preserving the two statistics the recommendation derives from
  (requests ≈ p95, memory limit ≈ max × 1.4) while keeping the API surface
  operator-meaningful instead of bucket arrays.
- **Derivation:** requests = p95 rounded up to 50m / 64Mi steps; memory limit =
  max × 1.4 (top of the dogfood-validated 1.3–1.4 band); never a CPU limit.
  Recommendation appears at ≥5 samples; `SizingDrift` judged at ≥20. Drift =
  ask ≥2× recommendation (waste) or memory limit < observed peak (OOM risk),
  compared against the template's declared resources or the provisioner's
  500m/1Gi gap-fill defaults when it declares none.
- **Warm-up safety:** the reconciler never overwrites
  `status.sizingRecommendation` with an empty snapshot, so a freshly restarted
  (or disabled) sampler cannot wipe the persisted store it would re-seed from.

### Phase 3 — opt-in sizing profiles (M) — ✅ shipped 2026-07-22

`RunnerSet.spec.sizing.profile`, applied by the provisioner at pod-build time:

| Profile | Behavior |
|---|---|
| `Static` (default) | Exactly what the template says — today's behavior. |
| `Binpack` | `requests` = `limits` = p95/max of observed usage → Guaranteed QoS, predictable packing, max runners per expensive node. |
| `Throughput` | `requests` from observed usage; `limits` with a configured headroom multiplier so jobs burst and finish faster. |
| `NodeShare` | `requests` = a configured per-runner share of node allocatable (the GPU case: allocatable ÷ GPUs). Needs **no usage history** — likely the highest value-to-effort profile and shippable independent of Phases 1–2. |

Safety rails: never modify extended resources (GPUs); clamp within configured
floor/ceiling; fall back to `Static` while sample count is below a confidence
minimum; admission-reject profile configs that would exceed the namespace
`ResourceQuota`/`LimitRange` shape rather than failing at provision time.

**Acceptance:** a `Binpack` RunnerSet provisions pods with derived resources; a
fresh RunnerSet (no history) provisions `Static` until confident; GPU counts
byte-identical to the template in all profiles. (All three asserted by the
envtest test `TestV2_RunnerSet_BinpackProfileProvisionsDerivedResources`.)

Decisions taken at pickup (implementation:
`cmd/agc/internal/controller/runnerset_sizing_profile.go`, applied in
`runnerSetTarget.Resolve`; design summary in
[appendix H §H.7](../design/appendix-h-v2-api-decomposition.md#h7-reference-integrity--runtime-conditions-not-admission)):

- **Actuation input is `status.sizingRecommendation`**, not the sampler's
  in-memory state: `Resolve` re-reads the RunnerSet per job anyway, the status
  IS the persisted store, and it makes actuation and the reported
  recommendation impossible to disagree — no new plumbing between sampler and
  provisioner.
- **Whole-pod confidence fallback.** `Binpack`/`Throughput` apply only when
  every template container has a ≥`MinSamplesForDrift` recommendation;
  otherwise the whole pod provisions `Static` (partial actuation would make
  QoS unpredictable — Guaranteed requires every container to carry
  requests==limits). Reported as `status.sizingProfileState`
  (`Active`/`AwaitingSamples`); while `Active`, `SizingDrift` reports
  `False/SizingProfileActive` instead of judging the now-bypassed template ask.
- **`NodeShare` declares the envelope** (`nodeShare.allocatable` +
  `workersPerNode`) rather than the AGC reading Node objects — the AGC is
  deliberately namespace-scoped (no cluster RBAC), and the operator knows
  which node shape the set's scheduling constraints target. Applied to the
  runner container only; sidecars are the operator's accounting.
- **Quota/LimitRange conflicts stay a runtime signal** — the planned
  admission-reject rail is deliberately NOT implemented: cross-object
  admission validation is what §H.7's "runtime conditions, not admission"
  philosophy avoids (apply-order coupling, GitOps hostility), and the existing
  `WorkerQuota*` conditions + quota retries already surface the conflict. The
  `maxRequests` clamp is the preventive knob.
- Open question 3 (ship `NodeShare` first) became moot — it shipped with the
  phase. Question 4 confirmed: profile parameters live on the `RunnerSet`
  (differently-tuned sets can share one template).

## Non-goals

- **AGC / GMC / proxy-pool autoscaling** — separate concerns:
  [Q360](../STATUS.md#Q360) (managed VPA opt-in for the control planes) and
  [Q173](../STATUS.md#Q173) (bring-your-own proxy autoscaler).
- **GPU-count autoscaling** — shapes are selected by jobs, never resized.
- **Making CI faster than GitHub-hosted** — same scope note as the
  [dogfood plan](dogfood-runner-rightsizing.md#scope-note--this-is-costcorrectness-not-speed):
  this is cost/correctness, not speed.
- **In-place resize of running job pods** (K8s ≥1.33 in-place pod resize) —
  plausible future extension once profiles exist; out of scope here.

## Open questions (settle at phase pickup)

1. ~~Metrics source~~ — settled in Phase 1: `metrics.k8s.io` (see the Phase 1
   decisions above).
2. ~~Aggregation window and decay~~ — settled in Phase 2:
   unbounded-since-window-start with no decay (see the Phase 2 decisions);
   revisit only if live use shows drift-over-time matters (Phase 3 clamps and
   confidence minimums are the safety net).
3. Whether `NodeShare` ships first as its own slice (it needs none of the
   observability machinery).
4. Where profile parameters live when the same `RunnerTemplate` backs
   differently-tuned `RunnerSet`s (current answer: on the `RunnerSet`).
