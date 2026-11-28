package provisioner

import (
	"context"
	"sync"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
)

// admissionGate is an in-memory, per-owner reservation counter that gates job
// acquisition on the worker ceiling before AcquireJob, so a listener with no free
// slot leaves the job queued at GitHub instead of claiming and then dropping it.
//
// It is soft state, lost on AGC restart, and the post-acquire ceilingCheck stays
// the authoritative backstop. The zero value is ready to use.
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

// Admit returns an AdmitFunc bound to target that decides, per delivered job,
// whether a listener may claim it. It walks four rungs in order and re-reads each
// on every call, so spec and cluster changes apply without an AGC restart:
//
//  1. Namespace ResourceQuota headroom: Target.QuotaExhausted.
//  2. Placeability, opt-in per owner: Target.CapacityDeclined.
//  3. The declared worker ceiling: Target.Ceiling, reserved in the in-memory gate.
//  4. The opt-in worker-pod creation-rate limit: Target.ScaleUpLimit, a token
//     taken from the bucket.
//
// Rungs 1 and 2 reserve nothing, so they run before the ceiling. The rate rung
// must stay last: a refusal has no path to refund the token it took.
//
// On admission the caller calls release with the outcome when the reserved work
// ends. AdmitAborted refunds the token, AdmitProvisioned leaves it spent, and the
// call is idempotent.
//
// This is the classic tier's form of the ladder and AdvertiseCapacity is the
// scale-set tier's; a rung added to one must be added to the other.
//
// The returned AdmitFunc is safe for concurrent use across the owner's listeners.
// Design rationale: docs/design/04-operational-flows.md, section 4.2 step 2a.
func (p *Provisioner) Admit(target Target) runnercore.AdmitFunc {
	key := target.Key().String()
	return func(ctx context.Context) (release func(runnercore.AdmitOutcome), ok bool, reason string) {
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
		freeSlot, ok := p.admission.admit(key, limit, bounded)
		if !ok {
			return nil, false, runnercore.AdmitReasonCeiling
		}
		// Read the rate config once and reuse it for the refund, so the token goes
		// back to the bucket that took it even if the owner edits spec.scaleUp while
		// the job is in flight.
		rateCfg := target.ScaleUpLimit(ctx)
		if !p.scaleUp.allow(key, rateCfg) {
			// Hand the reservation straight back: this job takes no slot. The closure is
			// idempotent, so this nets to zero and the later release the caller would
			// have made is not owed.
			freeSlot()
			// Per-delivery and high-volume while a set ramps: Debug, with the metric's
			// reason label as the operator-facing signal. Self-clearing — the bucket
			// refills at maxPerSecond — so unlike the rungs above it needs no condition.
			p.logForKey(target.Key()).Debug("job admission deferred: the scale-up rate limit has no token for another worker pod")
			if p.Metrics != nil {
				p.Metrics.ScaleUpThrottled.WithLabelValues(target.Key().Namespace, target.Key().Name).Inc()
			}
			return nil, false, runnercore.AdmitReasonScaleUp
		}
		// One closure for both spends, so the caller frees the slot and settles the
		// token in a single idempotent call. AdmitAborted refunds the token because
		// no worker pod was ever asked for; AdmitProvisioned leaves it spent.
		var once sync.Once
		return func(outcome runnercore.AdmitOutcome) {
			once.Do(func() {
				freeSlot()
				if outcome == runnercore.AdmitAborted {
					p.scaleUp.refund(key, rateCfg)
				}
			})
		}, true, ""
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

// AdvertiseCapacity returns the scale-set tier's per-poll capacity function: the
// integer counterpart of Admit, walking the same rungs against the same Target and
// re-reading them on every poll, so spec and cluster changes apply without an AGC
// restart. A rung added here must be added to Admit, and vice versa;
// TestAdmissionRungParity_* holds the pairing.
//
// unboundedDefault is advertised when the owner declares no ceiling.
//
// The result is the minimum of the rungs, so a rung can only lower it and one that
// fails open contributes no bound:
//
//  1. The declared worker ceiling: Target.Ceiling, else unboundedDefault.
//  2. Namespace ResourceQuota headroom: Target.QuotaCapacity, skipped when
//     DisableQuotaAdmission is set.
//  3. Placeability, opt-in per owner: Target.DeclinedCapacity.
//  4. The opt-in worker-pod creation-rate limit: Target.ScaleUpLimit against the
//     token bucket.
//
// The advertisement is a total, so rungs 2 and 4, which read a delta (how many
// more pods fit), add the owner's in-flight worker pods. Advertising rung 4's free
// tokens directly would cap the set at burst.
//
// Nothing is reserved and no token is taken. Design rationale:
// docs/design/04-operational-flows.md, "The ladder as an integer".
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

		// Rung 3: the owner's opt-in capacity gate. Evaluated even when the gate is
		// off, so a set that opts out publishes an explicit zero instead of freezing
		// its last non-zero reading.
		declinedLimit, declinedBounded := target.DeclinedCapacity(ctx, adv.Total)
		if adv.withhold(runnercore.AdmitReasonCapacity, declinedLimit, declinedBounded) {
			p.logForKey(target.Key()).Debug("advertising reduced scale-set capacity: the cluster cannot place another worker pod for this set",
				"ceiling", adv.Ceiling, "advertised", adv.Total)
		}

		// Rung 4: the owner's opt-in scale-up rate limit, always evaluated for the
		// same reason as rung 3. No ScaleUpThrottled increment here: a counter ticking
		// once per long-poll would measure the poll interval, not the throttling.
		rateLimit, rateBounded := p.scaleUpCapacity(ctx, target, adv.Total)
		if adv.withhold(runnercore.AdmitReasonScaleUp, rateLimit, rateBounded) {
			p.logForKey(target.Key()).Debug("advertising reduced scale-set capacity: the scale-up token bucket is below the worker ceiling",
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

// scaleUpCapacity is the integer form of the scale-up rate rung: the total worker
// pods this owner may have assigned given its token bucket, never above max.
//
// It reads the bucket without taking a token — the take happens later, at pod
// creation, because this tier states a capacity per long-poll and has no per-job
// decision point to charge. The delta the bucket answers with (free tokens: how many
// more pods may be created NOW) becomes a total by adding the owner's own in-flight
// worker pods, which are the creations the bucket has already been charged for.
//
// bounded=false when the owner declared no rate limit, or when the pod count could
// not be read: fail-open like every other observed rung, since a set whose pods it
// cannot list must not be starved of assignments. The count is only taken for an
// owner that actually opted in, so the default-off path costs one cache-free spec
// read and no List at all.
func (p *Provisioner) scaleUpCapacity(ctx context.Context, target Target, max int32) (limit int32, bounded bool) {
	key := target.Key()
	free, limited := p.scaleUp.tokens(key.String(), target.ScaleUpLimit(ctx))
	if !limited {
		return 0, false
	}
	active, err := p.activePodCount(ctx, key.Namespace, target.PodOwnerLabels())
	if err != nil {
		return 0, false
	}
	limit = active + free
	if limit > max {
		limit = max
	}
	return limit, true
}
