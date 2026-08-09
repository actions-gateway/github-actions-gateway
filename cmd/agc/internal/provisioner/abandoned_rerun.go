package provisioner

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// defaultAbandonedRerunWaitWindow bounds how long a force-cancelled abandoned run
	// waits for capacity to return before its recovery is given up on. Capacity may
	// never return: the group can go idle, and pendingPodDeadline also reaps a pod no
	// amount of waiting will place (an unpullable image, a constraint no node
	// satisfies). Thirty minutes is well past the pod-scheduling timescales the
	// autoscaler paths work on, and short enough that a re-run still reads as recovery
	// of the job the operator dispatched rather than an unexplained later one.
	defaultAbandonedRerunWaitWindow = 30 * time.Minute
	// defaultAbandonedRerunPollInterval is how often the sweeper re-asks whether
	// capacity returned. The wait is minutes-scale and the check is one cached pod
	// list per waiting owner, so a fixed short interval costs nothing and keeps the
	// re-run close to the moment the pool reopened.
	defaultAbandonedRerunPollInterval = 30 * time.Second
)

// Outcome label values for actions_gateway_abandoned_run_rerun_waits_total: a worker
// pod bound for the owner and the re-run was handed to the retry budget, or the wait
// window closed with the pool still unable to place one.
const (
	abandonedRerunOutcomeCapacityReturned = "capacity_returned"
	abandonedRerunOutcomeExpired          = "expired"
)

// abandonedRerunKey identifies a waiting recovery by owner and workflow run.
//
// Keyed by run rather than by job because rerun-failed-jobs is a run-level call: two
// jobs of one run abandoned together must cost one re-run and one budget slot, not two.
type abandonedRerunKey struct {
	owner client.ObjectKey
	runID string
}

// pendingAbandonedRerun is one force-cancelled abandoned run waiting for capacity.
type pendingAbandonedRerun struct {
	target      Target
	owner, repo string
	runID       string
	abandonedAt time.Time
	// tier is the acquisition tier the abandonment was detected on, carried through
	// to the recovery's metric labels. Both tiers wait on the same evidence and
	// spend the same budget, so it labels reporting and nothing else (Q766).
	tier string
}

// registerAbandonedRerun records a force-cancelled run to re-run once capacity returns
// (Q691). Called only on the cancelled force-cancel outcome: that is the state measured
// to accept rerun-failed-jobs (2026-08-05), where the false-green completejob endings
// were refused 403, and it is the only outcome in which we know the run is concluded.
//
// The re-run is deliberately not fired here. The job was abandoned because its worker
// could not be placed, so re-queueing it immediately puts it back into the pool that
// was starved, and a shortage would compound into a re-run storm.
func (p *Provisioner) registerAbandonedRerun(target Target, owner, repo, runID, tier string) {
	key := abandonedRerunKey{owner: target.Key(), runID: runID}
	p.abandonedRerunsMu.Lock()
	defer p.abandonedRerunsMu.Unlock()
	if p.abandonedReruns == nil {
		p.abandonedReruns = make(map[abandonedRerunKey]pendingAbandonedRerun)
	}
	if _, waiting := p.abandonedReruns[key]; waiting {
		return
	}
	p.abandonedReruns[key] = pendingAbandonedRerun{
		target:      target,
		owner:       owner,
		repo:        repo,
		runID:       runID,
		abandonedAt: p.nowFn(),
		tier:        tier,
	}
}

// peekAbandonedRerun returns an entry without claiming it, so the sweep can decide
// against acting and leave it waiting for the next tick.
func (p *Provisioner) peekAbandonedRerun(key abandonedRerunKey) (pendingAbandonedRerun, bool) {
	p.abandonedRerunsMu.Lock()
	defer p.abandonedRerunsMu.Unlock()
	e, ok := p.abandonedReruns[key]
	return e, ok
}

// takeAbandonedRerun removes and returns an entry, reporting whether it was still
// waiting. Removing before the re-run fires is what keeps the recovery at-most-once:
// handleEviction's GitHub calls outlive this sweep pass, and an entry left in the map
// would be drained again on the next tick and spend a second budget slot.
func (p *Provisioner) takeAbandonedRerun(key abandonedRerunKey) (pendingAbandonedRerun, bool) {
	p.abandonedRerunsMu.Lock()
	defer p.abandonedRerunsMu.Unlock()
	e, ok := p.abandonedReruns[key]
	if ok {
		delete(p.abandonedReruns, key)
	}
	return e, ok
}

// pendingAbandonedRerunKeys snapshots the waiting keys so the sweep does not hold the
// lock across a pod list, a Resolve, or a GitHub call.
func (p *Provisioner) pendingAbandonedRerunKeys() []abandonedRerunKey {
	p.abandonedRerunsMu.Lock()
	defer p.abandonedRerunsMu.Unlock()
	keys := make([]abandonedRerunKey, 0, len(p.abandonedReruns))
	for k := range p.abandonedReruns {
		keys = append(keys, k)
	}
	return keys
}

// sweepAbandonedReruns is one pass over the waiting recoveries: fire the ones whose
// owner has placed a worker pod since the abandonment, drop the ones that have waited
// out the window, and leave the rest for the next tick.
//
// It returns one done channel per recovery it armed. Each recovery's GitHub calls run
// on handleEviction's own detached, bounded context and outlive this pass, so the
// caller decides whether to wait (tests) or move on (the sweeper loop).
func (p *Provisioner) sweepAbandonedReruns(ctx context.Context) []<-chan struct{} {
	var armed []<-chan struct{}
	for _, key := range p.pendingAbandonedRerunKeys() {
		e, ok := p.peekAbandonedRerun(key)
		if !ok {
			continue
		}
		// runID is left off the logger and named per line: handleEviction stamps its
		// own, and a logger carrying it too would emit the key twice.
		log := p.logFor().With("namespace", key.owner.Namespace, "owner", key.owner.Name)

		if p.nowFn().Sub(e.abandonedAt) >= p.abandonedRerunWaitWindow() {
			if _, claimed := p.takeAbandonedRerun(key); !claimed {
				continue
			}
			log.Warn("capacity never returned for an abandoned run within the wait window; the run stays cancelled and needs a manual re-run",
				"runID", key.runID, "tier", e.tier, "window", p.abandonedRerunWaitWindow())
			p.countAbandonedRerunWait(e.target, e.tier, abandonedRerunOutcomeExpired)
			continue
		}

		returned, err := p.capacityReturnedSince(ctx, e.target, e.abandonedAt)
		if err != nil {
			// Nothing was decided, so leave the entry in place and re-ask next tick
			// rather than spend its wait on an unreadable pod list.
			log.Warn("could not read the owner's worker pods; deferring the abandoned-run re-run",
				"runID", key.runID, "error", err)
			continue
		}
		if !returned {
			continue
		}

		// Removing the entry is the claim, so a recovery is armed at most once even if
		// two passes ever overlap.
		if _, claimed := p.takeAbandonedRerun(key); !claimed {
			continue
		}
		p.countAbandonedRerunWait(e.target, e.tier, abandonedRerunOutcomeCapacityReturned)
		log.Info("a worker pod was placed for this owner since the run was abandoned; re-running it",
			"runID", key.runID, "tier", e.tier)
		// The budget is the shared per-run_id one (Q106), so an abandoned run that is
		// re-run and abandoned again is capped exactly like a repeatedly evicted one —
		// and, since Q766 registers on both tiers, across the two of them together.
		// No retry delay: the force-cancel already concluded the run, which is the
		// state rerun-failed-jobs was measured to accept (Q683).
		armed = append(armed, p.handleEviction(ctx, e.target, e.owner, e.repo, e.runID, log,
			p.abandonedRerunMaxRetries(ctx, e.target), 0, e.tier, recoveryCauseAbandoned))
	}
	return armed
}

// abandonedRerunMaxRetries reads the owner's retry budget from its live spec, falling
// back to the provisioner-level default when the owner no longer resolves. A budget we
// cannot read must not become an unbounded one.
func (p *Provisioner) abandonedRerunMaxRetries(ctx context.Context, target Target) int {
	if spec, err := target.Resolve(ctx); err == nil {
		return spec.MaxEvictionRetries
	}
	return p.MaxEvictionRetries
}

// capacityReturnedSince reports whether any of the owner's worker pods bound to a node
// after t. Binding is the Q512 evidence-of-capacity test: the scheduler placing a pod
// of this owner's shape is what proves the pool can hold another one, and the pod's
// phase is irrelevant (one still pulling images proves as much as one running).
func (p *Provisioner) capacityReturnedSince(ctx context.Context, target Target, t time.Time) (bool, error) {
	var pods corev1.PodList
	if err := p.Client.List(ctx, &pods,
		client.InNamespace(target.Key().Namespace),
		client.MatchingLabels(target.PodOwnerLabels()),
	); err != nil {
		return false, err
	}
	for i := range pods.Items {
		if at, ok := PodScheduledAt(&pods.Items[i]); ok && at.After(t) {
			return true, nil
		}
	}
	return false, nil
}

// countAbandonedRerunWait records how a wait ended.
func (p *Provisioner) countAbandonedRerunWait(target Target, tier, outcome string) {
	if p.Metrics == nil || p.Metrics.AbandonedRunRerunWaits == nil {
		return
	}
	key := target.Key()
	p.Metrics.AbandonedRunRerunWaits.WithLabelValues(key.Namespace, key.Name, tier, outcome).Inc()
}

// abandonedRerunWaitWindow returns how long a recovery waits for capacity, honouring
// the AbandonedRerunWaitWindow override (zero means the default).
func (p *Provisioner) abandonedRerunWaitWindow() time.Duration {
	if p.AbandonedRerunWaitWindow > 0 {
		return p.AbandonedRerunWaitWindow
	}
	return defaultAbandonedRerunWaitWindow
}

// PodScheduledAt returns when the scheduler bound the pod — the PodScheduled=True
// condition's transition time — and whether it has been bound at all. Binding is what
// makes a pod evidence of placeable capacity, so this deliberately ignores the pod's
// phase: a bound pod still pulling images proves as much as a running one.
//
// Exported because both readers of that evidence need the same answer: the capacity
// gate's latch (Q512) and the abandoned-run re-run trigger (Q691).
func PodScheduledAt(pod *corev1.Pod) (time.Time, bool) {
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionTrue {
			return c.LastTransitionTime.Time, true
		}
	}
	return time.Time{}, false
}

// AbandonedRerunSweeper drives the capacity-returned trigger for force-cancelled
// abandoned runs (Q691). It implements sigs.k8s.io/controller-runtime/pkg/
// manager.Runnable; wire it with mgr.Add. The waiting set is per-process, like the
// eviction counters, so it runs on every replica (NeedLeaderElection is false).
type AbandonedRerunSweeper struct {
	p        *Provisioner
	interval time.Duration
}

// NewAbandonedRerunSweeper returns an AbandonedRerunSweeper for p using the default
// poll interval.
func NewAbandonedRerunSweeper(p *Provisioner) *AbandonedRerunSweeper {
	return &AbandonedRerunSweeper{p: p, interval: defaultAbandonedRerunPollInterval}
}

// Start runs the poll loop until ctx is cancelled. It satisfies
// sigs.k8s.io/controller-runtime/pkg/manager.Runnable.
func (s *AbandonedRerunSweeper) Start(ctx context.Context) error {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	log := s.p.logFor()
	log.Info("abandoned-run re-run sweeper started",
		"interval", s.interval, "waitWindow", s.p.abandonedRerunWaitWindow())
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			// The recoveries are deliberately not waited on: each outlives this tick
			// on handleEviction's own bounded context, and blocking here would stall
			// every other waiting run behind one slow GitHub call.
			if n := len(s.p.sweepAbandonedReruns(ctx)); n > 0 {
				log.Info("re-ran abandoned runs whose capacity returned", "count", n)
			}
		}
	}
}

// NeedLeaderElection reports that the sweeper runs on every replica, not only the
// leader: each AGC instance owns the recoveries for the jobs it provisioned.
func (s *AbandonedRerunSweeper) NeedLeaderElection() bool { return false }
