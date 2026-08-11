# Worker Right-Sizing Profiles (Recommendations First)

> **Status: ✅ Complete — Phases 1–3 shipped 2026-07-21/22 and live-validated across three dogfood sessions (two on 2026-07-25, one on 2026-07-28).
> Residuals: [Q416](../STATUS.md#Q416) and, for the one profile still short of a live run, [Q448](../STATUS.md#Q448).** Usage observability, the status recommendation and its derivation, restart persistence, and the below-confidence fallback were confirmed in the first dogfood session.
> The two behaviours gated on 20 sampled jobs, the `SizingDrift` verdict and `Binpack` actuating, were confirmed in a second session after Q399 migrated the tenant off the Classic protocol (which had orphaned 81% of the jobs it acquired, capping samples at 10); [`Throughput` actuated live](#throughput-actuating-live-2026-07-28) on the same tenant three days later.
> See [Live validation](#live-validation-2026-07-25) and [Both ≥20-sample paths confirmed](#both-20-sample-paths-confirmed-2026-07-25-second-session). **`NodeShare` is the one actuating profile with no live run yet** — it carries envtest confidence only, tracked as Q448.
> Both bugs this validation surfaced are Classic-tier defects rather than sizing gaps: [Q398](#editing-a-classic-runnerset-needed-an-explicit-version-fixed) is fixed, and Q416 waits on a Classic operator report.
> This doc is the design sketch, the phase record, and the validation record.

## Goal

Automatically right-size worker pod CPU/memory `requests`/`limits` per runner shape from usage observed across the jobs that actually ran on that shape — recommendations first, opt-in actuation later.

## Why

**This is a differentiator versus Actions Runner Controller (ARC).** ARC has no sizing feedback loop: operators guess runner resource specs, and the guess is rarely revisited.
Our own dogfood proved both the value and the toil of doing it manually — [dogfood-runner-rightsizing.md](dogfood-runner-rightsizing.md) started from "every worker pod's original `requests`/`limits` were an unmeasured guess" and spent multiple sessions measuring peaks and deriving values by hand.
This plan automates that loop for every tenant.

The payoff concentrates where nodes are expensive:

- **GPU bin-packing.** Jobs select a runner shape by label (e.g.
  GPU count); within a shape, CPU/memory demand varies.
  Right-sized CPU/memory requests keep the GPU count — not an inflated CPU ask — the binding constraint, maximizing runners per GPU node.
- **Throughput tuning.** The opposite trade is also legitimate: oversize the shape so jobs burst and finish faster (GPU idle time while CPU-bound steps run is waste).
  A profile makes the trade explicit instead of accidental.

## Constraints (why this is GAG-native, not a VPA bolt-on)

> The durable, post-ship version of this argument — including the alternatives considered (stock VPA, custom recommender, GitOps loop, standalone webhook-actuated tool) and the extraction path — lives in [appendix D §D.7](../design/appendix-d-alternatives-considered.md#d7-worker-right-sizing-why-built-in-not-bolted-on) and [appendix G §G.15](../design/appendix-g-future-enhancements.md#g15-extract-the-batch-right-sizer-into-a-standalone--reusable-tool).

- **Stock VPA cannot target worker pods.** VPA's `targetRef` requires a controller with a `/scale` subresource to group pods; worker pods are owned by the `RunnerSet` CRD (`cmd/agc/internal/controller/runnerset_target.go`), which has no scale semantics — `replicas` is meaningless in a scale-to-zero design.
- **Actuation must happen at pod-build time.** Worker pods live minutes; evict-and-resize (VPA's actuation) is useless.
  The AGC provisioner already constructs every worker pod from the template (`cmd/agc/internal/provisioner/pod.go`), so applying a profile needs no webhook — it is a pod-build step.
- **GPU counts are never touched.** Extended resources are integer, job-selected via runner labels, and part of the shape's identity.
  Profiles shape CPU/memory *around* the fixed GPU allocation only.
- **Tenant-authored templates stay authoritative by default.** `podTemplate` on `RunnerTemplate` is the tenant's; any auto-apply is opt-in per `RunnerSet` and clamped (see Phase 3 safety rails).

## Phases

### Phase 1 — per-RunnerSet usage observability (M) — ✅ shipped 2026-07-21

The AGC samples CPU/memory usage for the worker pods it owns and aggregates per `RunnerSet` × container.
Implementation: `cmd/agc/internal/usage/` (a `manager.Runnable` ticker poller wired in `main.go`); operator docs: [worker-rightsizing.md](../operations/worker-rightsizing.md) + [metrics reference](../operations/observability-metrics.md#worker-usage--right-sizing-metrics-q359).

Decisions taken at pickup (open questions 1–2):

- **Metrics source: `metrics.k8s.io`** (metrics-server), as the default candidate argued — typed clientset, one PodMetrics list per tick (default 15s, `WORKER_USAGE_SAMPLE_INTERVAL`, `0`/`off` disables), RBAC = `pods.metrics.k8s.io` get/list added to the marker role **and** the hand-maintained `agc-tenant-role` fragment.
  Degrades gracefully when absent (throttled log + `…_poll_errors_total`); jobs shorter than one interval are counted in `…_jobs_unsampled_total` so coverage is judgeable.
- **Aggregation/export: Prometheus histograms of per-job peaks** rather than in-process p50/p95 summaries — `histogram_quantile` gives any-window quantiles, survives AGC restarts (rate/window queries), and aggregates across replicas; in-process state reduces to per-pod running peaks plus a max-peak gauge per RunnerSet × container (since-start; `max_over_time` bridges restarts).
  The in-memory aggregate window question (Q2) is thereby deferred to Phase 2, where status persistence genuinely needs one.
- **Ownership scoping:** candidate pods (label `actions-gateway.com/runner-set`) are matched against the RunnerSets in the manager cache, which is already namespace- and gateway-scoped — co-located multi-gateway AGCs never double-count.

**Acceptance:** metrics visible for a live RunnerSet after a handful of jobs; recipe reproduces the dogfood derivation without ad-hoc `kubectl top` scraping.
Unit-tested (peak-max across ticks, single finalize, unsampled short jobs, disappeared pods, unowned pods, poll errors); live-cluster validation rides the next dogfood benchmark session.

### Phase 2 — recommendations in `RunnerSet.status` (M) — ✅ shipped 2026-07-21

- `status.sizingRecommendation`: per-container recommended `requests`/`limits` (derived from Phase 1 aggregates), plus sample count and window so operators can judge confidence.
- A condition (e.g. `SizingDrift`) when the template's ask deviates from the recommendation beyond a threshold in either direction (waste or OOM risk).
- **Persistence:** aggregates flush into status periodically and merge back on AGC restart — status is the store; no new backing store.
- Recommendations are advisory only in this phase; nothing changes pod specs.

**Acceptance:** a deliberately oversized RunnerSet shows a lower recommendation and the drift condition within N jobs; restart does not zero the history.

Decisions taken at pickup (implementation: `cmd/agc/internal/usage/aggregate.go`
+ `cmd/agc/internal/controller/runnerset_sizing.go`; design summary in [appendix H §H.7](../design/appendix-h-v2-api-decomposition.md#h7-reference-integrity--runtime-conditions-not-admission)):

- **Aggregation window (open question 2): unbounded-since-window-start, no decay.** The sampler keeps fixed-bucket per-job-peak histograms (the Phase 1 Prometheus bucket edges, shared so the two views can't disagree) per RunnerSet × container.
  No rolling window: sizing wants the whole observed envelope, the operator judges staleness via `windowStartTime`/`sampleCount`, and a decay policy can arrive with Phase 3's confidence machinery if live use shows drift-over-time matters.
  Memory stays bounded by container-name cardinality regardless.
- **Persistence is approximate by design.** Status stores per container: observed p95 + max + count (+ window start), not the full histogram.
  On restart the sampler re-seeds 95% of the count at the p95 and the rest at the max — exactly preserving the two statistics the recommendation derives from (requests ≈ p95, memory limit ≈ max × 1.4) while keeping the API surface operator-meaningful instead of bucket arrays.
- **Derivation:** requests = p95 rounded up to 50m / 64Mi steps; memory limit = max × 1.4 (top of the dogfood-validated 1.3–1.4 band); never a CPU limit.
  Recommendation appears at ≥5 samples; `SizingDrift` judged at ≥20.
  Drift = ask ≥2× recommendation (waste) or memory limit < observed peak (OOM risk), compared against the template's declared resources or the provisioner's 500m/1Gi gap-fill defaults when it declares none.
- **Warm-up safety:** the reconciler never overwrites `status.sizingRecommendation` with an empty snapshot, so a freshly restarted (or disabled) sampler cannot wipe the persisted store it would re-seed from.

### Phase 3 — opt-in sizing profiles (M) — ✅ shipped 2026-07-22

`RunnerSet.spec.sizing.profile`, applied by the provisioner at pod-build time:

| Profile | Behavior |
|---|---|
| `Static` (default) | Exactly what the template says — today's behavior. |
| `Binpack` | `requests` = `limits` = p95/max of observed usage → Guaranteed QoS, predictable packing, max runners per expensive node. |
| `Throughput` | `requests` from observed usage; `limits` with a configured headroom multiplier so jobs burst and finish faster. |
| `NodeShare` | `requests` = a configured per-runner share of node allocatable (the GPU case: allocatable ÷ GPUs). Needs **no usage history** — likely the highest value-to-effort profile and shippable independent of Phases 1–2. |

Safety rails: never modify extended resources (GPUs); clamp within configured floor/ceiling; fall back to `Static` while sample count is below a confidence minimum; admission-reject profile configs that would exceed the namespace `ResourceQuota`/`LimitRange` shape rather than failing at provision time.

**Acceptance:** a `Binpack` RunnerSet provisions pods with derived resources; a fresh RunnerSet (no history) provisions `Static` until confident; GPU counts byte-identical to the template in all profiles.
(All three asserted by the envtest test `TestV2_RunnerSet_BinpackProfileProvisionsDerivedResources`.)

**Every profile now has an envtest that provisions a real pod** — the other two were unit-tested only until 2026-07-26, which left the actuation path (the transform reached through `Resolve` at pod build, not the resource arithmetic in isolation) unexercised for both:

| Test | Pins |
|---|---|
| `TestV2_RunnerSet_ThroughputProfileProvisionsBurstableResources` | Requests from the history; the CPU limit **removed** so jobs burst (Burstable, not Guaranteed); memory limit = observed peak × headroom, driven at both the 150% default and an explicit `limitHeadroomPercent`; GPU byte-identical. |
| `TestV2_RunnerSet_NodeShareProfileDividesTheNodeEnvelope` | `Active` with **zero** samples — the property that separates it from the history-based profiles; allocatable ÷ `workersPerNode` on the runner container only, with the sidecar's ask untouched; a template limit below the derived request lifted to it; `maxRequests` clamping the derived share, and the lift following the clamp. |

Each assertion was confirmed to fail against a mutated implementation before being trusted (headroom constant, the CPU-limit `delete`, the runner-container guard, and the clamp call each removed in turn).

**Resolved for `Throughput` on 2026-07-28** ([Throughput actuating live](#throughput-actuating-live-2026-07-28), Q449). `Binpack` was already live ([Live validation](#live-validation-2026-07-25)), so `NodeShare` is now the only profile carrying envtest confidence rather than dogfood confidence — and the more consequential one to leave there, since it needs no warm-up and so is the profile an operator can enable on day one.
The live path is below.

### The live path, and why the RC gate could not have found this

Auditing what `validate-release.sh` would actually exercise turned up something worse than the missing envtests: **neither dogfood tenant declared `spec.sizing` at all**, so the gate provisioned every worker at the `Static` default and would have passed an RC having validated none of the four profiles.
The failure mode is quiet by construction — `Static` yields a valid, healthy pod, the matrix goes green, and nothing in the gate looks wrong.
It is the Q400/Q404 shape again: a gate that cannot observe the thing it gates.

Three changes close it, and they are asymmetric because the profiles are:

| Profile | Where | Why there |
|---|---|---|
| `Throughput` | `ci` tenant (`scripts/dogfood/setup.sh`) | Needs ≥20 samples/container. The ~7-job e2e matrix cannot reach that in one run; the always-on CI tenant accrues them organically (it hit 36 on 07-25) and `status.sizingRecommendation` survives restart and re-apply. **Deployed and live-validated 2026-07-28** (Q449) — see [below](#throughput-actuating-live-2026-07-28). |
| `NodeShare` | e2e tenant (`deploy/dogfood-e2e/base/resources.yaml`) | Needs no history, so it actuates on the first job — the only profile a single gate run can validate outright. |
| `Binpack` | — | Already live-validated; not re-run. |

`Throughput` also happens to be what the `ci` template already encodes by hand — requests-only CPU, memory limit above the request for OOM headroom (Q248) — so the profile and the measured shape agree, and `minRequests`/`maxRequests` bound a bad derivation away from starving or over-asking on a real CI tenant.

**The e2e envelope is a deliberate lower bound.** `allocatable.cpu: 1500m / workersPerNode: 1` sits below both variants' static runner request (kata 2, dind 3), so actuation can only ever *reduce* a worker's ask — a wrong guess cannot make the release's own e2e gate unschedulable.
It is CPU-only for the same reason.
The true e2e-node allocatable has not been measured (the pool is `n2-standard-8` since Q627, the same 8 vCPU / 32 GB shape), and NodeShare divides into the runner container only, leaving the dind sidecar and Kata's 250m RuntimeClass overhead as the operator's accounting; the honest envelope therefore needs one `kubectl get node` reading first ([Q448](../STATUS.md#Q448)).
The trivial `÷1` divisor is fine here on purpose: the envtests above already cover the arithmetic (8÷4, 32Gi÷4), so the dogfood leg buys the thing envtest cannot — actuation through the real provisioner, on a real GKE node, for a real job.

Decisions taken at pickup (implementation: `cmd/agc/internal/controller/runnerset_sizing_profile.go`, applied in `runnerSetTarget.Resolve`; design summary in [appendix H §H.7](../design/appendix-h-v2-api-decomposition.md#h7-reference-integrity--runtime-conditions-not-admission)):

- **Actuation input is `status.sizingRecommendation`**, not the sampler's in-memory state: `Resolve` re-reads the RunnerSet per job anyway, the status IS the persisted store, and it makes actuation and the reported recommendation impossible to disagree — no new plumbing between sampler and provisioner.
- **Whole-pod confidence fallback.** `Binpack`/`Throughput` apply only when every template container has a ≥`MinSamplesForDrift` recommendation; otherwise the whole pod provisions `Static` (partial actuation would make QoS unpredictable — Guaranteed requires every container to carry requests==limits).
  Reported as `status.sizingProfileState` (`Active`/`AwaitingSamples`); while `Active`, `SizingDrift` reports `False/SizingProfileActive` instead of judging the now-bypassed template ask.
- **`NodeShare` declares the envelope** (`nodeShare.allocatable` + `workersPerNode`) rather than the AGC reading Node objects — the AGC is deliberately namespace-scoped (no cluster RBAC), and the operator knows which node shape the set's scheduling constraints target.
  Applied to the runner container only; sidecars are the operator's accounting.
- **Quota/LimitRange conflicts stay a runtime signal** — the planned admission-reject rail is deliberately NOT implemented: cross-object admission validation is what §H.7's "runtime conditions, not admission" philosophy avoids (apply-order coupling, GitOps hostility), and the existing `WorkerQuota*` conditions + quota retries already surface the conflict.
  The `maxRequests` clamp is the preventive knob. **Amended (Q489):** those cover the conflicts admission *rejects*.
  The one that rejects nothing — an injected CPU limit cancelling `Throughput`, whose mechanism is that limit's absence — got its own condition, `SizingProfileOverridden`, computed by comparing the pods the profile built against what the apiserver admitted rather than by reading any policy object.
- Open question 3 (ship `NodeShare` first) became moot — it shipped with the phase.
  Question 4 confirmed: profile parameters live on the `RunnerSet` (differently-tuned sets can share one template).

## Live validation (2026-07-25)

Run against the dogfood cluster rebuilt from zero the same session (see the [GKE dogfood runbook](gke-dogfood.md)), on a control-plane image built from `e0acd60`.
Every published image predated these phases, so the validation image had to be built by hand.

> **That hand-build does not need repeating.** The `e0acd60` image is still in GHCR (`ghcr.io/actions-gateway/agc:e0acd60d49fb7fb956b0f5380acdb6d69cac65ec`) and the dogfood cluster's GMC is already pinned to it, so a follow-up session reuses it as-is.
> Still true that no *cut release* carries this code: `v1.2.0` and `v1.2.0-rc.1` both predate Phase 2 (2026-07-21), as does `setup.sh`'s default `GAG_IMAGE_TAG`.
> Check with `git merge-base --is-ancestor <sizing-commit> <ref>` before assuming a tag has it.

Tenant: `gag-dogfood`, `RunnerSet` `ci`, runner container asking `cpu: 2` / `memory: 2Gi` with a `3Gi` memory limit.
Load was the repo's own CI, dispatched onto GAG with `unit-test.yml -f target_gag=true`.

### Prerequisites behaved as documented

- `metrics.k8s.io` is served by GKE's own metrics-server addon; no install step.
  (It is *not* Available for roughly the first two minutes of a new cluster's life, so a from-zero bootstrap's preflight reports it missing and then it self-resolves; that artifact is tracked on the dogfood runbook, not here.)
- The AGC's ServiceAccount could `list pods.metrics.k8s.io` in the tenant namespace with no manual RBAC, confirming the shipped grant.
- The sampler announced itself at startup: `worker usage sampler started`, `interval 15`.

### Phase 1 — usage observability: confirmed

Scraped from the AGC's `/metrics` over mTLS.
All seven metric families are exported with live data, labelled by namespace, runner set, and container:

| Signal | Observed |
|---|---|
| `..._cpu_peak_cores{container="runner"}` | `3.744` cores |
| `..._memory_peak_bytes{container="runner"}` | `2.383e+09` (≈2.22 GiB) |
| `..._job_cpu_peak_cores` / `..._job_memory_peak_bytes` | per-job histograms populating the expected buckets |
| `..._jobs_sampled_total` | counting finalized jobs |
| `..._jobs_unsampled_total` | non-zero — short jobs correctly excluded |
| `..._poll_errors_total` | absent (never incremented) |

Two independent things worth recording:

- **The measured peak matches the earlier hand-derivation.** The [dogfood right-sizing exercise](dogfood-runner-rightsizing.md) put heavy CI jobs at roughly 3.8 vCPU / 2.1 GiB by scraping `kubectl top` by hand; the sampler independently measured 3.74 cores / 2.22 GiB on the same workload.
  That is the feature reproducing, automatically, the number it was built to stop people deriving manually.
- **`jobs_unsampled_total` earns its place.** Some CI jobs finish inside one 15s sample interval, and the counter makes that visible instead of silently biasing the histogram toward long jobs.

### Phase 2 — recommendation in status: confirmed

`status.sizingRecommendation` appeared on the `runner` container at exactly `sampleCount: 5` (`MinSamplesForRecommendation`), carrying `windowStartTime` and both raw statistics beside the derived values (the capture below spells that field `windowStart`, its name at observation time; [Q485](../STATUS.md) renamed it before the 1.3 tag, values unchanged):

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

- `requests` = p95 rounded **up** to the step: 3745m → 3750m (50m step), 2273Mi → 2304Mi (64Mi step, 36 × 64).
- memory `limit` = max × 1.4 rounded up: 2273 × 1.4 = 3182.2 → 3200Mi (50 × 64).
- **No CPU limit is recommended**, as designed.
- `observedP95` equals `observedPeak` here because five samples is too few to separate them — which is exactly why `sampleCount` is published alongside.

`SizingDrift` was `False` with reason `InsufficientSamples` and a message naming the threshold, the documented behaviour below 20 samples.

**The dogfood template is under-asking, not over-asking.** The measured recommendation (3750m CPU / 2304Mi) sits *above* the template's `cpu: 2` / `memory: 2Gi`.
Drift only flags waste (ask ≥ 2× recommendation) or OOM risk (memory limit below the observed peak), so an under-provisioned CPU *request* is deliberately not flagged: CPU is compressible, and the job bursts into idle node capacity rather than failing.
Worth stating plainly because it is easy to read a `SizingWithinRange` verdict as "the template is right-sized" when it means "the template is not wasteful and will not OOM".

### Phase 2 — restart persistence: confirmed

Deleting the AGC pod mid-run (`dogfood-agc-…-227d6` → `…-fstfb`) left `status.sizingRecommendation` **byte-identical** across the restart — `sampleCount: 10`, the same `observedPeak`/`observedP95`, the same derived `requests`/`limits`, and the same `windowStartTime`.
That is the warm-up safety rail doing its job: a freshly-started sampler holds an empty aggregate, and the reconciler declines to overwrite the persisted store with it.

**The re-seed is real, not just a preserved field.** The next job to finalize after the restart took `sampleCount` from 10 to **11**, rather than back to 1 — so `seedFromStatus` (`cmd/agc/internal/usage/sampler.go`) genuinely rebuilt the in-memory aggregate from the persisted statistics, and the reconciler was not merely declining to touch a stale field.

**The Prometheus metrics do reset**, and should not be mistaken for data loss.
The gauges and histograms are in-process and start empty on a new pod, which is why the metric is documented as "since the AGC last restarted" and the operator recipe reaches for `max_over_time` to bridge restarts.
The *recommendation* is what survives, because status — not the metric — is the store.

The practical consequence is that **the two views disagree immediately after a restart, and both are right**.
Measured one job after the restart above:

| View | Value |
|---|---|
| `status.sizingRecommendation[].sampleCount` (persisted store) | 11 |
| `actions_gateway_worker_usage_jobs_sampled_total` (in-process) | 1 |

Anyone cross-checking the sampler by comparing those two numbers will conclude it lost history.
It did not — the counter is scoped to the current process while the recommendation carries the whole window.

### Phase 3 — below-confidence fallback: confirmed

With `spec.sizing.profile: Binpack` set while no container had reached `MinSamplesForDrift` samples:

- `status.sizingProfileState` reported `AwaitingSamples`.
- Worker pods provisioned the template's static ask verbatim (`requests cpu 2 / memory 2Gi`, `limits memory 3Gi`) at **Burstable** QoS — i.e. `Static`, not a partially-derived shape.
  Guaranteed QoS would have indicated the profile actuating early.
- No `SizingDrift` condition was set, matching the deliberate "no data, no noise" branch rather than a `False/InsufficientSamples` on every set.

### Editing a Classic RunnerSet needed an explicit version (fixed)

Enabling the profile the obvious way failed:

```console
$ kubectl patch runnerset ci -n gag-dogfood --type=merge \
    -p '{"spec":{"sizing":{"profile":"Binpack"}}}'
The RunnerSet "ci" is invalid: spec: Invalid value: a v2beta1 runner set must
declare exactly one runnerLabel: v2beta1 is ScaleSet-only and the scale set's
name is its single runs-on match target (Q264)
```

The object is a `Classic` set with three `runnerLabels` — a shape v2alpha1 allowed and v2beta1, at the time, did not.
(Q726 has since removed the v2beta1 rule entirely; the transcript above is the state as measured.)
Unqualified `kubectl` addresses the storage version (v2beta1), so the write was rejected even though the field being set had nothing to do with labels or protocol.
Qualifying the resource worked:

```bash
kubectl patch runnersets.v2alpha1.actions-gateway.com ci -n gag-dogfood \
  --type=merge -p '{"spec":{"sizing":{"profile":"Binpack"}}}'
```

This was not specific to sizing — it blocked *any* unqualified edit of a Classic multi-label RunnerSet, including `kubectl edit` and re-`apply`.
Filed as Q398 and **since fixed**: the single-label rule moved from `v2beta1`'s `spec` onto its `runnerLabels` field, so CRD validation ratcheting suppresses it while the labels are unchanged.
The first `kubectl patch` above now succeeds; qualifying to `v2alpha1` is still required to edit such a set's **labels**.
Rationale and the general rule for hub-only constraints: [v2beta1.md](v2beta1.md#6-q74--the-graduation-cut).

### The ≥20-sample paths (reached 2026-07-25, second session)

Two behaviours need `MinSamplesForDrift` (20) sampled jobs on a template container.
The first session topped out at **10**; the follow-up session, run on the ScaleSet-migrated tenant, reached **36** and validated both.
Results are in [Both ≥20-sample paths confirmed](#both-20-sample-paths-confirmed-2026-07-25-second-session) below; the account of why the first session could not reach them follows.

- the actual `SizingDrift` verdict (`SizingWithinRange` vs `SizingDriftDetected`), as opposed to the `InsufficientSamples` state that was confirmed;
- `Binpack` actuating — `sizingProfileState: Active` with derived `requests`==`limits` at Guaranteed QoS.

The cap was **not** the sizing code.
Since only finalized jobs become samples, sample accrual was bounded by the tenant's job-completion rate, which was catastrophically low.
Diagnosed and fixed under Q399: the tenant moved off the Classic protocol to a single-label ScaleSet ([gke-dogfood B7](gke-dogfood.md#b7-create-the-v2-tenant-objects)).
The measurement is recorded here because it bounds what any future validation run on this cluster can expect.

**Measured across the whole 2026-07-25 session** (GitHub jobs API for all 18 dispatched runs, cross-checked against GKE Cloud Logging for the AGC and every worker container):

| | |
|---|---|
| GAG-targeted job records | 85 |
| Distinct worker pods that ever started | 16 |
| Jobs with `steps > 0` (actually executed) | 16 |
| Jobs with `started_at` + runner assigned + **zero steps** | 69 (81%) |
| Q254 `job_not_found` teardowns, whole session | 2 |

The two counts of 16 are independent and agree exactly.

**The failure shape is acquire-without-provision, and it is Classic-only.** `AcquireJob` is what flips a job to `in_progress` at GitHub and stamps the runner name; it succeeded 85 times.
Only 16 of those acquisitions ever got a worker pod.
The other 69 sat with zero steps until GitHub's own timers ended them, visible as exact `+10:00` (lock lapse) and `+15:00` (unstarted-job) deltas, e.g. run `30146417937`'s `coverage` started 06:07:42 and "failed" at 06:17:42 having run nothing.
The tenant's `RunnerSet` was `acquisitionProtocol: Classic` with three `runnerLabels`; the ScaleSet listener is single-acquirer and lives in a separate package (`cmd/agc/internal/scalesetlistener/`) that shares none of the classic acquire path, which is why Q264 P4 measured 7/7 there against Classic's 2/7.

**The Q254 teardown was a red herring.** It appears in the original Q399 report because it is the loudest line in the AGC log, but it fired exactly twice in 4.75 hours.
It cannot account for 69 orphaned jobs.

Two further Classic-only listener defects surfaced while measuring, both left in place because Classic is terminal (`docs/plan/v1-classic-sunset-review.md`):

- [`session.go`](../../cmd/agc/internal/listener/session.go) `healSession` deletes the old broker session *before* refreshing the token, so on the `unauthorized` heal path the DELETE presents the already-expired token, returns 401, and leaks the session server-side; the follow-up `CreateSession` then collides with it (409) and the listener exits.
  Observed at 04:12 and 05:02.
- [`multiplexer.go`](../../cmd/agc/internal/listener/multiplexer.go) restarts only the permanent baseline goroutine, so both of those listener slots stayed dead for the rest of the session.

When picking this up again, note that the drift verdict is judged against the **template's** ask, not against new samples — so once a set is past 20 samples, both branches can be exercised in seconds by editing `RunnerTemplate` resources, with no further soak:

- `SizingDriftDetected` (waste) needs an ask ≥2× the recommendation.
  On the dogfood shape only **memory** can do this: 2× the ~2.3Gi recommendation still fits an `e2-standard-4`, whereas 2× the ~3.75-core CPU recommendation exceeds node allocatable and would leave worker pods `Pending` instead.
- `SizingDriftDetected` (OOM risk) needs a memory *limit* below the observed peak, which will genuinely OOM-kill jobs — do it on a throwaway set.

> Both predictions above held, with one correction: the OOM-risk branch does **not** need a throwaway set.
> The verdict is a pure reconciler computation over `status.sizingRecommendation` and the template, so with an empty job queue the condition can be produced and reverted without a single pod being built from the bad limit.
> See below.

## Both ≥20-sample paths confirmed (2026-07-25, second session)

Run on the same tenant after [Q399](gke-dogfood.md#b7-create-the-v2-tenant-objects) migrated it to a single-label ScaleSet, on the same `e0acd60` control-plane image.
The soak reached `sampleCount: 36`; the recommendation stabilised at:

```json
{ "container": "runner",
  "observedPeak": { "cpu": "3740m", "memory": "2399Mi" },
  "requests":     { "cpu": "3750m", "memory": "2432Mi" },
  "limits":       { "memory": "3392Mi" },
  "sampleCount": 36 }
```

**The measurement reproduced independently.** The first session measured a 3745m CPU peak on a Classic tenant; this one measured **3740m** on a ScaleSet tenant, from a fresh window with a rebuilt aggregate.
Two independent runs, five milli-cores apart, agreeing with the ~3.8 vCPU hand-derivation the feature was built to replace.

### The `SizingDrift` verdict: all three states

Judged against the template's ask, so each state is one `RunnerTemplate` edit:

| Template ask | `SizingDrift` | Message |
|---|---|---|
| `cpu 2 / mem 2Gi`, limit `3Gi` (baseline) | `False` / `SizingWithinRange` | template container resources are within the drift thresholds of the measured recommendation |
| memory request → `6Gi` | `True` / `SizingDriftDetected` | container runner: memory request 6Gi is >=2x the recommended 2432Mi (waste) |
| memory limit → `2Gi` | `True` / `SizingDriftDetected` | container runner: memory limit 2Gi is below the observed per-job peak 2399Mi (OOM risk) |

Both `SizingDriftDetected` messages name the measured numbers rather than emitting a generic warning, so an operator can act on the condition alone.

### `Binpack` actuating

With `spec.sizing: {profile: Binpack, maxRequests: {cpu: "3"}}` and every template container past 20 samples:

- `status.sizingProfileState` → **`Active`** (previously only `AwaitingSamples` had been observed).
- `SizingDrift` → `False` / **`SizingProfileActive`**, the documented supersession: pods run derived values, so comparing the static ask would mislead.
- The next worker pod provisioned at **Guaranteed QoS** with `requests == limits == {cpu: 3, memory: 3392Mi}`, while pods created before the flip stayed `Burstable` on the template's static values, so the transform applies at pod build, not retroactively.

**The `maxRequests` clamp is not optional on this shape.** The raw CPU recommendation (3750m) exceeds an `e2-standard-4`'s ~3.4 vCPU allocatable, so unclamped `Binpack` would derive an unschedulable pod and leave every worker `Pending`.
Clamping to `cpu: "3"` kept pods schedulable and exercised the safety rail in the same step.
Any tenant whose measured peak approaches node allocatable needs the same clamp.
Worth stating in the operator doc, because the failure is silent until pods stop scheduling.

### Ordering constraint

`SizingProfileState: Active` short-circuits the drift judgment ([`runnerset_sizing.go`](../../cmd/agc/internal/controller/runnerset_sizing.go)), so the drift branches must be exercised **before** enabling a profile, or the condition reports `SizingProfileActive` instead of a verdict.

### Session artifacts worth not repeating

- **Size the system pool from `required_system_nodes`, not by hand.** A manual resize to 1 node left the AGC unschedulable (one `e2-standard-2` cannot hold GMC + Athens + AGC), and the tenant ran with no controller for 16 minutes. `scripts/dogfood/start.sh` derives the count and warns on a low pin.
- **`maxWorkers` is bounded by the namespace `ResourceQuota`, not just nodes.** Raising it 8 → 16 against a `pods: 12` quota put the AGC into a provision/quota-deny/retry/abandon loop and stalled sample accrual entirely.
  The quota conditions reported it precisely (`WorkerQuotaExceeded=True`, `QuotaExhausted`, with `WorkerQuotaPressure` correctly `Superseded`), which is the capacity observability doing its job on a real fault.
- **A Running scale-set worker has no reap deadline, so an idle one is immortal.** After that churn, 8 worker pods remained alive with **zero** jobs outstanding, occupying 10 of 12 quota slots and all 6 worker nodes until deleted by hand.
  The reaper ([`runner_shared.go`](../../cmd/agc/internal/controller/runner_shared.go)) switches on pod phase: terminal phases get `completedTTL`, `Pending` gets `pendingDeadline`, and `PodRunning` is counted as active and `continue`d with **no deadline of any kind**.
  So the Pending orphans in that same window *were* reaped (`reason=pending_deadline` in the AGC log); the ones that survived were the workers that had reached Running, registered, and then sat at `Listening for Jobs` forever because their assignment was gone.
  On classic this cannot happen: `provision()` blocks on the pod's terminal state, so a Running worker is always owned by a goroutine.
  It is specific to the scale-set tier's fire-and-forget provisioning.
  Filed as Q420 and **fixed 2026-07-26**, independently of Q417 (which shipped later the same day): the fix needed a durable deadline rather than a pod watch, so it did not have to wait for one.
  The scale-set listener's completion path stamps `actions-gateway.com/job-completed-at` on the job's worker pod, and the reaper deletes a pod still Running five minutes later (`reason=orphaned_running`, `WorkerPodOrphanedRunning` Warning Event).
  Putting the deadline on the pod rather than in AGC memory is what makes it survive an AGC restart, and set-once semantics keep a completion replayed to a re-created session from pushing it back.
  Flow: [04-operational-flows.md](../design/04-operational-flows.md#orphaned-running-worker-pod-scaleset-tier).

## `Throughput` actuating live (2026-07-28)

Q449: the committed `spec.sizing` block reached the live `ci` tenant, and `Throughput` provisioned real worker pods.
Same cluster and control-plane image (`e0acd60`) as the 07-25 run.

**Derivation, end to end.** From a 36-sample history (`observedPeak` `cpu 3751m` / `memory 2399Mi`, `requests` `cpu 3800m` / `memory 2432Mi`), every post-patch worker pod provisioned:

| Field | Value | Why |
|---|---|---|
| `requests.cpu` | `3` | Derived 3800m **clamped** by `maxRequests.cpu: "3"`. |
| `requests.memory` | `2432Mi` | Derived; the `1Gi` `minRequests` floor is not binding. |
| `limits.cpu` | *absent* | Throughput deletes it so jobs burst. |
| `limits.memory` | `3598Mi` | `observedPeak` 2399Mi × the 150% default headroom. |
| QoS | `Burstable` | Not Guaranteed — the property separating it from `Binpack`. |

`status.sizingProfileState` → `Active` and `SizingDrift` → `False` / `SizingProfileActive` the moment the spec landed; the template's single `runner` container was already past 20 samples, so the whole-pod confidence gate passed with no warm-up.
Seven pods actuated, all scheduled (none `Pending`), and a real CI job ran to completion on one (`Completed`, exit 0) — so `cpu: 3` schedules on an `e2-standard-4` and the memory envelope holds under the repo's own load.

**The `maxRequests` clamp is load-bearing for `Throughput` too**, not just `Binpack`: the raw 3800m derivation exceeds an `e2-standard-4`'s ~3.4 vCPU allocatable.
Throughput dropping the *CPU limit* does not help — schedulability is decided by the *request*.
The operator doc's clamp warning was scoped to `Binpack` and has been broadened to both history-based profiles.

**Pods created before the patch stayed `Static`** (`cpu 2` / `memory 2Gi`, limit `3Gi`) — actuation is a pod-build transform, not retroactive, matching the 07-25 `Binpack` observation.

### Why this could not be deployed until now

The cluster was at **0 nodes across every pool** (normal at-rest state after `stop.sh`), so the GMC pod had been `Pending` ~10h and `webhook-service` had no endpoints.
The `RunnerSet` CRD stores `v2beta1` with `Webhook` conversion, and `vrunnerset-v2alpha1.kb.io` is `failurePolicy: Fail` with `matchPolicy: Equivalent` — so a write to *either* served version is routed through the down webhook. **No CR write of any kind can land while the dogfood cluster is stopped.** Bring the cluster up first; there is no offline path.

### The multi-day soak was not needed

Q449 was filed expecting a days-long accrual before the RC.
It was not required: the aggregate is **cumulative with no TTL or eviction**, and `seedFromStatus` ([`aggregate.go`](../../cmd/agc/internal/usage/aggregate.go)) rebuilds it from persisted `status.sizingRecommendation`.
All 36 samples from 07-25 were still counted after the stop, so the deploy was a single short window — start, patch, verify, stop — rather than a soak. **Any future sizing deploy on this tenant inherits the same property**: history is durable across stop/start, so it never has to be re-earned.

What survives is the *summary*, not the raw distribution, and that is by design: `seed` reconstructs the histogram from the persisted `(sampleCount, observedP95, observedPeak)` triple by putting 95% of the mass at the p95 and the rest at the max.
Sample count and peak come back exact and the p95 to bucket resolution — precisely the three statistics the recommendation reads — while the shape below the p95 is deliberately not persisted.
So "durable" means the *derivation* is reproducible across a stop, not that the tenant's job history is archived; a stop still costs the sub-p95 detail, which nothing downstream uses.

### `start.sh` does not re-apply the CRs

The reason the committed config sat undeployed: `start.sh` resizes the pool, waits for readiness, and dispatches workflows — it never calls `apply_cr`, which lives only in `setup.sh`'s `main`.
A CR-only change therefore reaches the cluster via a `setup.sh` re-run or a targeted patch, never via a start.
Worth knowing before assuming a committed CR edit is live.

## Non-goals

- **AGC / GMC / proxy-pool autoscaling** — separate concerns, both shipped: the managed VPA opt-in for the control planes (Q360: [`ActionsGateway.spec.agcAutoscaling`](../design/appendix-e-capacity-planning.md#e11-managed-vertical-right-sizing-of-the-control-planes) and the chart's `vpa.enabled`) and the bring-your-own proxy autoscaler (Q173: [`EgressProxy.spec.managedAutoscaling`](v2-api.md#bring-your-own-proxy-autoscaler-q173--shipped)).
- **GPU-count autoscaling** — shapes are selected by jobs, never resized.
- **Making CI faster than GitHub-hosted** — same scope note as the [dogfood plan](dogfood-runner-rightsizing.md#scope-note--this-is-costcorrectness-not-speed): this is cost/correctness, not speed.
- **In-place resize of running job pods** (K8s ≥1.33 in-place pod resize) — plausible future extension once profiles exist; out of scope here.

## Open questions (settle at phase pickup)

1. ~~Metrics source~~ — settled in Phase 1: `metrics.k8s.io` (see the Phase 1 decisions above).
2. ~~Aggregation window and decay~~ — settled in Phase 2: unbounded-since-window-start with no decay (see the Phase 2 decisions); revisit only if live use shows drift-over-time matters (Phase 3 clamps and confidence minimums are the safety net).
3. Whether `NodeShare` ships first as its own slice (it needs none of the observability machinery).
4. Where profile parameters live when the same `RunnerTemplate` backs differently-tuned `RunnerSet`s (current answer: on the `RunnerSet`).
