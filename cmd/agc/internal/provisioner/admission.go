package provisioner

import (
	"context"
	"sync"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
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

// admissionCeiling returns the worker ceiling the gate enforces for rg, mirroring
// ceilingCheck's hold decision exactly: a job is held there once the active-pod
// count reaches *every* priority-tier threshold (i.e. the maximum threshold), or
// reaches maxWorkers when no tiers are set. bounded is false when the group has
// neither — an unbounded group, where the gate admits unconditionally.
//
// The maximum tier threshold is computed rather than assuming the last tier so
// the ceiling is correct even if an invalid (non-ascending) spec slips past
// validation; with the validated strictly-ascending order the maximum is the
// last tier, matching ceilingCheck.
func admissionCeiling(rg *v1alpha1.RunnerGroup) (limit int32, bounded bool) {
	if len(rg.Spec.PriorityTiers) > 0 {
		var max int32
		for _, tier := range rg.Spec.PriorityTiers {
			if tier.Threshold > max {
				max = tier.Threshold
			}
		}
		return max, true
	}
	if rg.Spec.MaxWorkers != nil {
		return *rg.Spec.MaxWorkers, true
	}
	return 0, false
}

// AdmitFor returns an AdmitFunc bound to snapshot that gates job acquisition on
// the group's worker ceiling via the in-memory reservation counter (Q59).
//
// Like HandlerFor, snapshot is the listener-start RunnerGroup used only for
// identity; the ceiling is read from the freshly cached RunnerGroup on each call
// so maxWorkers / priorityTiers edits take effect without an AGC restart (Q117).
// The returned AdmitFunc is safe for concurrent use across the group's listeners.
func (p *Provisioner) AdmitFor(snapshot *v1alpha1.RunnerGroup) listener.AdmitFunc {
	key := snapshot.Namespace + "/" + snapshot.Name
	return func(ctx context.Context) (release func(), ok bool) {
		rg := p.currentRunnerGroup(ctx, snapshot)
		limit, bounded := admissionCeiling(rg)
		return p.admission.admit(key, limit, bounded)
	}
}
