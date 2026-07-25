# Worker Right-Sizing Profiles (Recommendations First)

> **Status: 🔄 Phases 1–3 shipped 2026-07-21/22 — tracked as [Q359](../STATUS.md#Q359).**
> Live-validated on dogfood 2026-07-25: usage observability, the status
> recommendation and its derivation, restart persistence, and the profile's
> below-confidence fallback all behave as specified. The two behaviours gated on
> 20 sampled jobs — the `SizingDrift` verdict and `Binpack` actuating — are
> still unvalidated, because job completion on dogfood capped the sample count
> at 10 ([Q399](../STATUS.md#Q399)). See
> [Live validation](#live-validation-2026-07-25). This doc is the design sketch,
> the phase record, and now the validation record.

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

> The durable, post-ship version of this argument — including the alternatives
> considered (stock VPA, custom recommender, GitOps loop, standalone
> webhook-actuated tool) and the extraction path — lives in
> [appendix D §D.7](../design/appendix-d-alternatives-considered.md#d7-worker-right-sizing-why-built-in-not-bolted-on)
> and [appendix G §G.15](../design/appendix-g-future-enhancements.md#g15-extract-the-batch-right-sizer-into-a-standalone--reusable-tool).

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

## Live validation (2026-07-25)

Run against the dogfood cluster rebuilt from zero the same session (see the
[GKE dogfood runbook](gke-dogfood.md)), on a control-plane image built from
`e0acd60`.
Every published image predated these phases, so the validation image had to be
built by hand — worth knowing before the next attempt, since neither the pinned
`GAG_IMAGE_TAG` default nor the newest release carries this code.

Tenant: `gag-dogfood`, `RunnerSet` `ci`, runner container asking `cpu: 2` /
`memory: 2Gi` with a `3Gi` memory limit. Load was the repo's own CI, dispatched
onto GAG with `unit-test.yml -f target_gag=true`.

### Prerequisites behaved as documented

- `metrics.k8s.io` is served by GKE's own metrics-server addon; no install step.
  (It is *not* Available for roughly the first two minutes of a new cluster's
  life, so a from-zero bootstrap's preflight reports it missing and then it
  self-resolves; that artifact is tracked on the dogfood runbook, not here.)
- The AGC's ServiceAccount could `list pods.metrics.k8s.io` in the tenant
  namespace with no manual RBAC, confirming the shipped grant.
- The sampler announced itself at startup: `worker usage sampler started`,
  `interval 15`.

### Phase 1 — usage observability: confirmed

Scraped from the AGC's `/metrics` over mTLS. All seven metric families are
exported with live data, labelled by namespace, runner set, and container:

| Signal | Observed |
|---|---|
| `..._cpu_peak_cores{container="runner"}` | `3.744` cores |
| `..._memory_peak_bytes{container="runner"}` | `2.383e+09` (≈2.22 GiB) |
| `..._job_cpu_peak_cores` / `..._job_memory_peak_bytes` | per-job histograms populating the expected buckets |
| `..._jobs_sampled_total` | counting finalized jobs |
| `..._jobs_unsampled_total` | non-zero — short jobs correctly excluded |
| `..._poll_errors_total` | absent (never incremented) |

Two independent things worth recording:

- **The measured peak matches the earlier hand-derivation.** The
  [dogfood right-sizing exercise](dogfood-runner-rightsizing.md) put heavy CI
  jobs at roughly 3.8 vCPU / 2.1 GiB by scraping `kubectl top` by hand; the
  sampler independently measured 3.74 cores / 2.22 GiB on the same workload.
  That is the feature reproducing, automatically, the number it was built to
  stop people deriving manually.
- **`jobs_unsampled_total` earns its place.** Some CI jobs finish inside one
  15s sample interval, and the counter makes that visible instead of silently
  biasing the histogram toward long jobs.

### Phase 2 — recommendation in status: confirmed

`status.sizingRecommendation` appeared on the `runner` container at exactly
`sampleCount: 5` (`MinSamplesForRecommendation`), carrying `windowStart` and
both raw statistics beside the derived values:

```json
{
  "container": "runner",
  "observedPeak":  { "cpu": "3745m", "memory": "2273Mi" },
  "observedP95":   { "cpu": "3745m", "memory": "2273Mi" },
  "requests":      { "cpu": "3750m", "memory": "2304Mi" },
  "limits":        { "memory": "3200Mi" },
  "sampleCount": 5,
  "windowStart": "2026-07-25T02:12:25Z"
}
```

Every derivation rule the docs state is reproduced by these numbers:

- `requests` = p95 rounded **up** to the step: 3745m → 3750m (50m step),
  2273Mi → 2304Mi (64Mi step, 36 × 64).
- memory `limit` = max × 1.4 rounded up: 2273 × 1.4 = 3182.2 → 3200Mi (50 × 64).
- **No CPU limit is recommended**, as designed.
- `observedP95` equals `observedPeak` here because five samples is too few to
  separate them — which is exactly why `sampleCount` is published alongside.

`SizingDrift` was `False` with reason `InsufficientSamples` and a message
naming the threshold, the documented behaviour below 20 samples.

**The dogfood template is under-asking, not over-asking.** The measured
recommendation (3750m CPU / 2304Mi) sits *above* the template's `cpu: 2` /
`memory: 2Gi`. Drift only flags waste (ask ≥ 2× recommendation) or OOM risk
(memory limit below the observed peak), so an under-provisioned CPU *request*
is deliberately not flagged: CPU is compressible, and the job bursts into idle
node capacity rather than failing. Worth stating plainly because it is easy to
read a `SizingWithinRange` verdict as "the template is right-sized" when it
means "the template is not wasteful and will not OOM".

### Phase 2 — restart persistence: confirmed

Deleting the AGC pod mid-run (`dogfood-agc-…-227d6` → `…-fstfb`) left
`status.sizingRecommendation` **byte-identical** across the restart —
`sampleCount: 10`, the same `observedPeak`/`observedP95`, the same derived
`requests`/`limits`, and the same `windowStart`. That is the warm-up safety
rail doing its job: a freshly-started sampler holds an empty aggregate, and the
reconciler declines to overwrite the persisted store with it.

**The re-seed is real, not just a preserved field.** The next job to finalize
after the restart took `sampleCount` from 10 to **11**, rather than back to 1 —
so `seedFromStatus` (`cmd/agc/internal/usage/sampler.go`) genuinely rebuilt the
in-memory aggregate from the persisted statistics, and the reconciler was not
merely declining to touch a stale field.

**The Prometheus metrics do reset**, and should not be mistaken for data loss.
The gauges and histograms are in-process and start empty on a new pod, which is
why the metric is documented as "since the AGC last restarted" and the operator
recipe reaches for `max_over_time` to bridge restarts. The *recommendation* is
what survives, because status — not the metric — is the store.

The practical consequence is that **the two views disagree immediately after a
restart, and both are right**. Measured one job after the restart above:

| View | Value |
|---|---|
| `status.sizingRecommendation[].sampleCount` (persisted store) | 11 |
| `actions_gateway_worker_usage_jobs_sampled_total` (in-process) | 1 |

Anyone cross-checking the sampler by comparing those two numbers will conclude
it lost history. It did not — the counter is scoped to the current process
while the recommendation carries the whole window.

### Phase 3 — below-confidence fallback: confirmed

With `spec.sizing.profile: Binpack` set while no container had reached
`MinSamplesForDrift` samples:

- `status.sizingProfileState` reported `AwaitingSamples`.
- Worker pods provisioned the template's static ask verbatim
  (`requests cpu 2 / memory 2Gi`, `limits memory 3Gi`) at **Burstable** QoS —
  i.e. `Static`, not a partially-derived shape. Guaranteed QoS would have
  indicated the profile actuating early.
- No `SizingDrift` condition was set, matching the deliberate "no data, no
  noise" branch rather than a `False/InsufficientSamples` on every set.

### Editing a Classic RunnerSet needs an explicit version

Enabling the profile the obvious way fails:

```console
$ kubectl patch runnerset ci -n gag-dogfood --type=merge \
    -p '{"spec":{"sizing":{"profile":"Binpack"}}}'
The RunnerSet "ci" is invalid: spec: Invalid value: a v2beta1 runner set must
declare exactly one runnerLabel: v2beta1 is ScaleSet-only and the scale set's
name is its single runs-on match target (Q264)
```

The object is a `Classic` set with three `runnerLabels` — a shape v2alpha1
allows and v2beta1 does not. Unqualified `kubectl` addresses the storage
version (v2beta1), so the write is rejected even though the field being set has
nothing to do with labels or protocol. Qualifying the resource works:

```bash
kubectl patch runnersets.v2alpha1.actions-gateway.com ci -n gag-dogfood \
  --type=merge -p '{"spec":{"sizing":{"profile":"Binpack"}}}'
```

This is not specific to sizing — it blocks *any* unqualified edit of a Classic
multi-label RunnerSet, including `kubectl edit` and re-`apply`. Filed as
[Q398](../STATUS.md#Q398).

### Not reached: the ≥20-sample paths

Two behaviours need `MinSamplesForDrift` (20) sampled jobs on a template
container, and the session topped out at **10**:

- the actual `SizingDrift` verdict (`SizingWithinRange` vs `SizingDriftDetected`),
  as opposed to the `InsufficientSamples` state that was confirmed;
- `Binpack` actuating — `sizingProfileState: Active` with derived
  `requests`==`limits` at Guaranteed QoS.

The cap was **not** the sizing code. Roughly 10 of ~44 GAG jobs dispatched
across nine `workflow_dispatch` rounds actually finalized; the rest report a
`started_at` with no conclusion, and the AGC logged the Q254 teardown path
(`RenewJob: job lock definitively lost … broker: job not found (HTTP 404)`),
which is what a job disposed of GitHub-side looks like from the gateway. Since
only finalized jobs become samples, sample accrual is bounded by that
completion rate. Tracked separately as [Q399](../STATUS.md#Q399) — it is a
dogfood reliability question, not a right-sizing one, and it will throttle any
future validation that needs a job population.

When picking this up again, note that the drift verdict is judged against the
**template's** ask, not against new samples — so once a set is past 20 samples,
both branches can be exercised in seconds by editing `RunnerTemplate`
resources, with no further soak:

- `SizingDriftDetected` (waste) needs an ask ≥2× the recommendation. On the
  dogfood shape only **memory** can do this: 2× the ~2.3Gi recommendation still
  fits an `e2-standard-4`, whereas 2× the ~3.75-core CPU recommendation exceeds
  node allocatable and would leave worker pods `Pending` instead.
- `SizingDriftDetected` (OOM risk) needs a memory *limit* below the observed
  peak, which will genuinely OOM-kill jobs — do it on a throwaway set.

## Non-goals

- **AGC / GMC / proxy-pool autoscaling** — separate concerns, both shipped:
  the managed VPA opt-in for the control planes (Q360:
  [`ActionsGateway.spec.agcAutoscaling`](../design/appendix-e-capacity-planning.md#e11-managed-vertical-right-sizing-of-the-control-planes)
  and the chart's `vpa.enabled`) and the bring-your-own proxy autoscaler (Q173:
  [`EgressProxy.spec.managedAutoscaling`](v2-api.md#bring-your-own-proxy-autoscaler-q173--shipped)).
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
