# Worker Right-Sizing Profiles (Recommendations First)

> **Status: 🔄 Phase 1 shipped 2026-07-21; Phases 2–3 open — tracked as [Q359](../STATUS.md#Q359).**
> This doc is the design sketch and phase plan; each phase revises it with
> findings before the next begins.

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

### Phase 2 — recommendations in `RunnerSet.status` (M)

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

### Phase 3 — opt-in sizing profiles (M)

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
byte-identical to the template in all profiles.

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
2. Aggregation window and decay — rolling N jobs vs time-windowed. Phase 1
   sidestepped this by exporting histograms (the operator picks a PromQL
   window); Phase 2's status-persisted aggregates must actually pick one.
3. Whether `NodeShare` ships first as its own slice (it needs none of the
   observability machinery).
4. Where profile parameters live when the same `RunnerTemplate` backs
   differently-tuned `RunnerSet`s (current answer: on the `RunnerSet`).
