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
func (p *Provisioner) AdmitFor(snapshot *v1alpha1.RunnerGroup) listener.AdmitFunc {
	return p.Admit(p.runnerGroupTarget(snapshot))
}

// Admit returns an AdmitFunc bound to the given Target that gates job acquisition
// on the owner's worker ceiling via the in-memory reservation counter (Q59). The
// ceiling is re-read from the fresh spec on each call so maxWorkers/priorityTiers
// edits take effect without an AGC restart (Q117). The returned AdmitFunc is safe
// for concurrent use across the owner's listeners. v1 wires it via AdmitFor; the
// v2 RunnerSet controller wires it directly with a RunnerSet-backed Target.
func (p *Provisioner) Admit(target Target) listener.AdmitFunc {
	key := target.Key().String()
	return func(ctx context.Context) (release func(), ok bool) {
		limit, bounded := target.Ceiling(ctx)
		return p.admission.admit(key, limit, bounded)
	}
}
