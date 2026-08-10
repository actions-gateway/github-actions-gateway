# Q223 — Worker Scale-Up Rate Limit (anti-stampede)

Status: ✅ Done — shipped 2026-07-06.
Archived on landing (no open Queue residual).

Design origin: [Appendix G.11](../../design/appendix-g-future-enhancements.md#g11-worker-scale-up-rate-limiting-anti-stampede).
This plan resolves the three open questions G.11 left for the Q223 implementation: the knob surface, how the ramp composes with the `WorkerQuotaPressure` backoff, and how it coexists with the node autoscaler.

## Goal

Give operators an **opt-in, default-off** per-RunnerGroup cap on the **rate** at which worker pods are *created*, to smooth cold-start stampedes on a shared, rate-sensitive egress path (NAT / SNAT gateway / stateful firewall conntrack / site-to-site VPN) when a burst of jobs is acquired at once.
It complements — does not replace — the existing `maxWorkers` / priority-tier **ceiling** (which bounds how *many* pods run concurrently): the ceiling bounds the count, this bounds the onset rate.

Non-goals (per G.11's decision table): image-pull storms (→ P2P / Q211), egress API-call volume (→ proxy rate limit G.2/Q19), and fairness/limited-seat problems (→ the existing quota ceiling).
This is only the middle rows of that table.

## Design decisions

### Knob surface (open question 1 — resolved)

A new optional struct field `scaleUp` on both the v1 `RunnerGroupSpec` and the v2 `RunnerSetSpec` (siblings of `maxWorkers` / `maxQuotaRetries`, which is where the other worker-pod tunables already live):

```yaml
spec:
  scaleUp:
    maxPerSecond: 10   # sustained token refill rate (worker pods/sec); required when scaleUp is set
    burst: 20          # token-bucket depth = largest instantaneous batch; optional, defaults to maxPerSecond
```

- `maxPerSecond` (int32, required within the struct, min 1): sustained creation rate once the burst is spent.
- `burst` (`*int32`, optional, min 1): bucket depth.
  Omitted ⇒ defaulted **in the AGC** to `maxPerSecond` (one second's worth of instantaneous start).
  CRD defaulting cannot reference another field, so the default is applied in the neutral spec resolution, not a `+kubebuilder:default`.
- `scaleUp` omitted entirely ⇒ **no limit** (the zero value / default): immediate provisioning, zero added latency — GAG's core behaviour is unchanged for every existing user.
  This is an availability knob, not a security one, so default-off does not touch the secure-by-default rule.

Integer `maxPerSecond` (not a float or a `Duration` interval) matches G.11's proposed naming and validates with standard integer markers.
Sub-1/sec ramps are a deliberate non-goal: onset smoothing for conntrack/SNAT/VPN operates comfortably at ≥1/sec, and anything slower is a *ceiling* problem (per G.11's own diagnostic), not a rate one.

### Where it gates (open question 2 — resolved)

The token is spent right before **pod creation** in the provisioner — `provision()` (classic) and `ProvisionScaleSetWorker()` (scale-set), immediately before `createPodWithQuotaRetry`, after the admission reservation (Q59) and the concurrency ceiling check.
When the bucket is empty the acquired job **waits** there (holding its Q59 slot and its GitHub job lock, which the renew loop keeps alive) until a token frees — the same "acquired job waits, holding its lock" shape the namespace-quota retry (`createPodWithQuotaRetry`) already uses, so it composes with `WorkerQuotaPressure` rather than adding a new state machine.
Gating at pod creation (not the pre-acquire admission gate) also means deduped fan-out losers — which admit but never create a pod — never spend a token, so the limiter tracks *actual* pod-creation rate.

### Coexistence with the node autoscaler (open question 3 — resolved)

Documented, not coded: the ramp bounds pod-*admission* rate, which indirectly eases the node-scale-up burst the cluster autoscaler / Karpenter react to; those tools retain their own independent rate controls.
The operator guidance is to reach for the ramp only for the shared-egress-onset case and to prefer workflow-level `concurrency:` or a capacity ceiling for the others.

### Config re-read (Q117)

The limiter re-reads `scaleUp` from the freshly-resolved spec on every job (via `ResolvedSpec`), so an edit to `maxPerSecond`/`burst` takes effect on the next job without an AGC restart — mirroring the admission gate and the other tunables.
The per-key `rate.Limiter` is updated in place (`SetLimit`/`SetBurst`) when the config changes.

## Implementation

Uses `golang.org/x/time/rate` (already vendored at the repo root; `x/time` is already in `cmd/agc/go.mod`).

1. **API** — `ScaleUpRateLimit` struct + `ScaleUp *ScaleUpRateLimit` field on `cmd/agc/api/v1alpha1` `RunnerGroupSpec` and `api/v2alpha1` `RunnerSetSpec`; regenerate deepcopy + CRD manifests + chart CRDs.
2. **Neutral seam** — `ScaleUpConfig` (MaxPerSecond, Burst) on `provisioner.ResolvedSpec` (nil = off), populated by both the v1 `runnerGroupTarget` and the v2 `runnerSetTarget` adapters (burst defaulted to maxPerSecond there).
3. **Limiter** — `scaleUpLimiter` (new `provisioner/scaleuplimiter.go`): per-owner token bucket, injectable clock + sleep for deterministic tests, zero-value ready (lazy map init) like `admissionGate`. `wait(ctx, key, cfg) (throttled, err)`.
4. **Wiring** — call `p.scaleUp.wait(...)` before `createPodWithQuotaRetry` in both provision paths; on `throttled` increment the metric; on ctx-cancel return the error (job abandoned, same as a quota ctx-cancel).
5. **Metric** — `actions_gateway_worker_scaleup_throttled_total{namespace, runner_group}` (G.11's `worker_scaleup_throttled_total`).
6. **Docs** — operator field docs in `docs/operations/`; Appendix G.11 status → implemented stub; this plan; STATUS Q223 row removed (isolated commit).

## Tests

- `scaleuplimiter_internal_test.go`: default-off = never throttles / unbounded; burst honored (first `burst` calls pass instantly, next throttles); sustained rate enforced (token refills after `1/maxPerSecond`); config change re-limits; ctx-cancel while waiting returns error and does not leak the reservation.
  All deterministic via injected clock/sleep — no cluster, no wall-clock sleeps.
- Provisioner integration: a fake-client `provision()` with a tiny `maxPerSecond`
  + `burst=1` observes the second concurrent pod creation waiting on the injected sleep (behaviour, not just "code runs").
- envtest (`cmd/agc/internal/controller/integration/`): CRD accepts `scaleUp`, round-trips `maxPerSecond`/`burst`, and rejects `maxPerSecond: 0` (min=1) — the apiserver-side defaulting/validation path.
