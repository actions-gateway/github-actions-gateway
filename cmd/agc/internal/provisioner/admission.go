package provisioner

import (
	"context"
	"sync"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
)

// admissionGate is an in-memory, per-RunnerGroup reservation counter that gates
// job acquisition on available worker capacity (Q59).
//
// The provisioner's ceilingCheck runs *after* AcquireJob has already claimed the
// job from GitHub, so a job rejected there is dropped with its GitHub lock held
// — and a job whose lock lapses without renewal is cancelled rather than
// redelivered. The gate moves the capacity decision to *before* AcquireJob: a
// listener that cannot reserve a slot skips the claim entirely, leaving the job
// queued at GitHub for redelivery to a sibling session with capacity.
//
// The counter is deliberately soft state: it is reserved at admit time and
// released on acquire failure or job completion, and is lost on AGC restart.
// Losing it is fail-safe — a restart simply resets the budget, and the
// post-acquire ceilingCheck remains the authoritative backstop for the races a
// pure in-memory count cannot close (e.g. a sibling AGC, or the restart window).
// The zero value is ready to use.
type admissionGate struct {
	mu       sync.Mutex
	reserved map[string]int32 // key: namespace/name → in-flight admitted jobs
}

// admit reserves one worker slot for key when the in-flight count is below
// limit, returning an idempotent release func and ok=true. When bounded is false
// the group has no ceiling, so admission always succeeds with a no-op release.
// When the gate is full it returns nil, false and the caller must skip the
// acquire. Callers MUST call release exactly when the reserved work ends —
// acquire failure or pod terminal state — so the counter reflects only live
// in-flight jobs.
func (g *admissionGate) admit(key string, limit int32, bounded bool) (release func(), ok bool) {
	if !bounded {
		return func() {}, true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reserved == nil {
		g.reserved = make(map[string]int32)
	}
	if g.reserved[key] >= limit {
		return nil, false
	}
	g.reserved[key]++
	var once sync.Once
	return func() { once.Do(func() { g.release(key) }) }, true
}

// release frees one reserved slot for key, pruning the map entry once the count
// reaches zero so the map stays bounded by the number of currently-busy groups.
func (g *admissionGate) release(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reserved[key] <= 1 {
		delete(g.reserved, key)
		return
	}
	g.reserved[key]--
}

// reservedCount returns the current in-flight reservation count for key. Used by
// tests to assert the gate's arithmetic.
func (g *admissionGate) reservedCount(key string) int32 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reserved[key]
}

// WorkerCeiling returns the maximum concurrent worker pods rg may run, mirroring
// the admission gate / ceilingCheck hold decision: the maximum priority-tier
// threshold when tiers are set, else maxWorkers, else unbounded (bounded=false).
// Exported so the RunnerGroup reconciler can size the worker pool's quota
// footprint for the WorkerQuota{Pressure,Exceeded} conditions (Q82) against the
// same ceiling the gate enforces — one source of truth. It delegates to the
// neutral WorkerCeilingFromTiers so v1 and v2 compute the ceiling identically.
func WorkerCeiling(rg *v1alpha1.RunnerGroup) (limit int32, bounded bool) {
	return WorkerCeilingFromTiers(tierThresholds(rg.Spec.PriorityTiers), rg.Spec.MaxWorkers)
}

// AdmitFor returns an AdmitFunc for the v1 RunnerGroup controller, wrapping the
// RunnerGroup in the v1 Target adapter and delegating to Admit.
func (p *Provisioner) AdmitFor(snapshot *v1alpha1.RunnerGroup) runnercore.AdmitFunc {
	return p.Admit(p.runnerGroupTarget(snapshot))
}

// Admit returns an AdmitFunc bound to the given Target that gates job acquisition
// on three independent rungs, all re-read on every delivered job so spec and cluster
// changes take effect without an AGC restart (Q117):
//
//  1. Observed namespace-ResourceQuota headroom (#784) — Target.QuotaExhausted.
//  2. Observed placeability, opt-in per owner (Q405) — Target.CapacityDeclined.
//  3. The owner's declared worker ceiling (Q59) — Target.Ceiling, counted against
//     the in-memory reservation gate.
//
// The two observed rungs come first and reserve nothing: a job we decline for quota
// or capacity was never counted against the ceiling, so the reservation arithmetic is
// untouched. Without the quota rung, quota exhaustion is handled one layer down by
// createPodWithQuotaRetry — which holds the GitHub job lock across up to
// maxQuotaRetries × quotaRetryDelay (150s of a ~10-minute lock at the defaults) and,
// on budget exhaustion, drops the job *with the lock held*: precisely the failure the
// Q59 gate exists to prevent. Refusing to claim leaves the job queued at GitHub for a
// sibling with capacity.
//
// # Why the scheduler's verdict is a separate, opt-in rung
//
// The quota rung and WorkersUnschedulable look symmetrical and are not, which is why
// quota gates unconditionally and placeability only when the owner opts in:
//
//   - A ResourceQuota rejection is never an autoscaler input — no cluster autoscaler
//     adds a node because a namespace quota is full — so declining to claim forfeits
//     no capacity, and the condition is self-clearing: in-flight jobs complete and
//     release headroom.
//   - A Pending unschedulable pod, by contrast, *may be* the request for a node. On an
//     elastic cluster, gating on it would suppress the very signal cluster-autoscaler
//     needs and starve the tenant exactly when scale-up would have rescued it. On a
//     fixed-size cluster no actor is waiting on that pod and the same verdict is pure
//     waste. The asymmetry is the cluster's, not the signal's, so the owner asserts
//     which one it is via spec.capacityGate.mode and the AGC never auto-detects it
//     (docs/design/appendix-d-alternatives-considered.md §D.8).
//
// # Tier scope
//
// This is the per-delivered-job form of the ladder, used by the classic acquisition
// tier only. A ScaleSet-protocol RunnerSet never reaches it — it states its capacity
// as an integer once per long-poll instead — so every rung here has a counterpart in
// AdvertiseCapacity, and a rung added to one without the other ships to one tier
// (Q443).
//
// The returned AdmitFunc is safe for concurrent use across the owner's listeners.
// v1 wires it via AdmitFor; the v2 RunnerSet controller wires it directly with a
// RunnerSet-backed Target.
func (p *Provisioner) Admit(target Target) runnercore.AdmitFunc {
	key := target.Key().String()
	return func(ctx context.Context) (release func(), ok bool, reason string) {
		if !p.DisableQuotaAdmission {
			if exhausted, detail := target.QuotaExhausted(ctx); exhausted {
				// Per-delivery and high-volume while a tenant sits at its quota
				// ceiling: Debug, with the metric's reason label and the owner's
				// WorkerQuotaExceeded condition as the operator-facing signals.
				p.logForKey(target.Key()).Debug("job admission deferred: no namespace ResourceQuota headroom for another worker pod", "detail", detail)
				return nil, false, runnercore.AdmitReasonQuota
			}
		}
		if declined, detail := target.CapacityDeclined(ctx); declined {
			// Per-delivery and high-volume while a set's worker shape is unplaceable:
			// Debug, with the metric's reason label and the owner's
			// WorkerCapacityDeclined condition as the operator-facing signals.
			p.logForKey(target.Key()).Debug("job admission deferred: the cluster cannot place another worker pod for this owner", "detail", detail)
			return nil, false, runnercore.AdmitReasonCapacity
		}
		limit, bounded := target.Ceiling(ctx)
		release, ok = p.admission.admit(key, limit, bounded)
		if !ok {
			return nil, false, runnercore.AdmitReasonCeiling
		}
		return release, true, ""
	}
}

// CapacityAdvertisement is the scale-set tier's per-poll expression of the admission
// ladder: one integer, plus the accounting that produced it.
type CapacityAdvertisement struct {
	// Total is the number to advertise as X-ScaleSetMaxCapacity — the total jobs
	// GitHub may hold assigned for this set at once, so it bounds totalAssignedJobs
	// rather than granting a delta.
	Total int32
	// Ceiling is the declared worker ceiling (or the caller's unbounded default)
	// before any observed rung lowered it. Total <= Ceiling always.
	Ceiling int32
	// Withheld maps an AdmitReason* to the slots that rung removed from Ceiling, and
	// carries an explicit zero for every rung that was evaluated and did not bind, so
	// a reader never has to distinguish "not withholding" from "not evaluated".
	Withheld map[string]int32
}

// AdvertiseCapacity returns the per-poll capacity function for the scale-set
// acquisition tier: the integer counterpart of Admit, walking the same rungs against
// the same Target and re-reading them on every poll so spec and cluster changes take
// effect without an AGC restart (Q117).
//
// The two forms exist because the tiers decide at different granularity. Classic asks
// "may I claim this job?" once per delivered job; a ScaleSet set instead states, once
// per long-poll, how many jobs GitHub may keep assigned to it. Both are the same
// ladder, and a rung that is expressed in only one of them silently ships to only one
// tier — which is exactly how the quota rung came to be classic-only until Q443. Any
// rung added here must be added to Admit, and vice versa.
//
// unboundedDefault is the value to advertise when the owner declares no ceiling at all;
// the caller owns that policy (the scale-set reconciler passes the maxListeners-matched
// default).
//
// Rungs compose as a min() and can only ever LOWER the advertisement, so a rung that
// fails open leaves today's behavior exactly:
//
//  1. The declared worker ceiling (Q59) — Target.Ceiling, else unboundedDefault.
//  2. Observed namespace-ResourceQuota headroom (#784) — Target.QuotaCapacity,
//     converted from a headroom delta to a total by the owner's own in-flight worker
//     pods, and skipped entirely when DisableQuotaAdmission is set.
//  3. Observed placeability, opt-in per owner (Q405) — Target.DeclinedCapacity,
//     which bounds the total at the owner's own in-flight worker pods while the
//     cluster cannot place another one.
//
// Unlike the classic rung this reserves nothing and costs no claim: jobs beyond the
// advertised total stay queued at GitHub, so a quota-blocked job is never assigned in
// the first place. The trade is granularity — the decision is per poll for the whole
// set rather than per job, so recovery from a stale read is one poll interval.
func (p *Provisioner) AdvertiseCapacity(target Target, unboundedDefault int32) func(ctx context.Context) CapacityAdvertisement {
	return func(ctx context.Context) CapacityAdvertisement {
		ceiling, bounded := target.Ceiling(ctx)
		if !bounded {
			ceiling = unboundedDefault
		}
		if ceiling < 0 {
			ceiling = 0
		}
		adv := CapacityAdvertisement{Total: ceiling, Ceiling: ceiling, Withheld: map[string]int32{}}

		// Rung 2 — live namespace-ResourceQuota headroom. Skipped wholesale under the
		// AGC-wide DisableQuotaAdmission kill switch: the rung is then not evaluated at
		// all, so it publishes no series rather than a misleading zero.
		if !p.DisableQuotaAdmission {
			limit, bounded := target.QuotaCapacity(ctx, adv.Total)
			if adv.withhold(runnercore.AdmitReasonQuota, limit, bounded) {
				// Per-poll and steady while a tenant sits at its quota ceiling: Debug,
				// with the withheld gauge and the owner's WorkerQuotaExceeded condition
				// as the operator-facing signals.
				p.logForKey(target.Key()).Debug("advertising reduced scale-set capacity: namespace ResourceQuota headroom is below the worker ceiling",
					"ceiling", adv.Ceiling, "advertised", adv.Total)
			}
		}

		// Rung 3 — the owner's opt-in capacity gate (Q405). Always evaluated, because
		// "the gate is off" is a per-owner spec state the Target answers, not a rung
		// the provisioner can skip: a set that opts in mid-flight must start binding on
		// the next poll with no restart (Q117), and one that opts out keeps publishing
		// an explicit zero instead of freezing its last non-zero reading.
		declinedLimit, declinedBounded := target.DeclinedCapacity(ctx, adv.Total)
		if adv.withhold(runnercore.AdmitReasonCapacity, declinedLimit, declinedBounded) {
			p.logForKey(target.Key()).Debug("advertising reduced scale-set capacity: the cluster cannot place another worker pod for this set",
				"ceiling", adv.Ceiling, "advertised", adv.Total)
		}
		return adv
	}
}

// withhold folds one rung's answer into the advertisement and reports whether that
// rung actually lowered it.
//
// Every rung that is EVALUATED publishes an explicit zero even when it does not bind,
// so a reader of actions_gateway_scaleset_capacity_withheld never has to distinguish
// "this rung is not withholding" from "this rung was never evaluated" — the two are
// indistinguishable in an absent series, and one of them is a bug.
//
// Rungs compose as a min() and can only ever LOWER the total, so each rung's Withheld
// entry is its own marginal contribution and the entries sum to Ceiling - Total. A rung
// that fails open (bounded=false) leaves the advertisement exactly as the earlier rungs
// left it, which is why fail-open is the same thing as today's behavior here.
func (a *CapacityAdvertisement) withhold(reason string, limit int32, bounded bool) bool {
	if _, evaluated := a.Withheld[reason]; !evaluated {
		a.Withheld[reason] = 0
	}
	if !bounded || limit >= a.Total {
		return false
	}
	if limit < 0 {
		limit = 0
	}
	a.Withheld[reason] = a.Total - limit
	a.Total = limit
	return true
}
