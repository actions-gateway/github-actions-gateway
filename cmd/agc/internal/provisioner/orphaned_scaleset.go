package provisioner

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Orphaned-worker recovery on the scale-set tier (Q844).
//
// # The gap this closes
//
// Every arm of disruptionAwaitingRecovery reads its discriminator off a pod that still
// exists, and two of the causes it recognises DELETE their victim: kube-scheduler
// preemption stamps its condition and removes the pod within the termination grace
// period, and an external graceful deletion publishes the terminal phase as the
// container exits, shortly before the object goes away. An AGC that is down across that
// window never lists the pod at all, so it issues no re-run and the displaced run needs
// a manual one.
//
// What is lost is the discriminator, not the run: GitHub still holds the run, concluded
// failure by the Q385 SIGTERM relay, and rerun-failed-jobs still accepts it. Nor does
// the listener's assignment replay cover it — replay recovers a job still ASSIGNED at
// GitHub, and the relay's conclusion is what stops it being one.
//
// # What replaces the pod as the record
//
// The listener persists the run identity of every job whose worker it built, in the
// per-RunnerSet guard ConfigMap it already writes ahead of each message delete
// (scalesetlistener.GuardState.InFlight, Q606). An entry is added on a successful
// provision and dropped when the job concludes, so an entry that outlives its worker POD
// is a run whose worker went away with nobody watching.
//
// # Why the verdict is taken once per process, before the reaper
//
// The owning reconciler reads the ConfigMap; it never writes it, so the listener's poll
// goroutine stays the single writer. Three orderings make the reading sound, and all
// three are properties of where the call sits in Reconcile:
//
//   - BEFORE the reaper, so a genuinely failed job's pod — which sits in PodFailed until
//     completedPodTTL elapses — is still there to vote "not a disruption".
//   - BEFORE the listener starts, so the entry is still in the ConfigMap. The preempted
//     job's own JobCompleted replays seconds later and retires it.
//   - ONCE per process, because the failure being closed is exclusively "the AGC was
//     absent". An entry this process created is covered by the live pod-watch recovery,
//     so re-examining one can only produce a false positive.
//
// # What it deliberately does not do
//
// It does not name the cause. Which of preemption, drain, node loss, or a hand-run
// delete took the worker is precisely what the missing pod no longer says, so the
// recovery reports cause="vanished" rather than guessing.
//
// It does not force-cancel first. Whether a container ever ran is a fact about the pod
// (Q766 reads it off podEverStarted), so a worker deleted before it started takes this
// path too and its re-run is refused by GitHub for having no failed job — the existing
// error path logs and drops that, leaving the pre-Q844 outcome rather than a wrong one.

// OrphanedWorker is one job a previous AGC process provisioned a worker for and never
// saw conclude — the identity half of a scalesetlistener.InFlightJob, taken as a
// provisioner-local type so this package does not depend on the listener's.
type OrphanedWorker struct {
	JobID      string
	Owner      string
	Repository string
	RunID      string
}

// orphanScanState records which owners have already had their stored in-flight set
// adjudicated by this process. Bounded by the number of RunnerSets rather than by
// uptime, and deliberately in memory: it is a fact about this process, and a restart is
// exactly when the question deserves asking again.
type orphanScanState struct {
	mu   sync.Mutex
	seen map[string]bool
}

// claim reports whether key's scan is this caller's to run, and marks it taken.
func (s *orphanScanState) claim(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[key] {
		return false
	}
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	s.seen[key] = true
	return true
}

// RecoverOrphanedScaleSetWorkers triggers the automatic re-run for every job in inFlight
// whose worker pod no longer exists, and is a no-op on every reconcile after the first
// for a given owner. inFlight is the set a previous process persisted; the caller reads
// it from the guard ConfigMap.
//
// Call it from the same place as RecoverEvictedScaleSetWorkers and BEFORE the reaper —
// the whole discriminator is whether the worker pod is still there, and the reaper is
// the one thing in this process that would remove a terminal one.
//
// It returns a done channel that closes once every recovery it started has finished, so
// a caller may block on it (tests) or ignore it (the reconciler, which must not stall a
// reconcile on GitHub). A returned error means the pod scan itself failed and no verdict
// was taken; the owner's claim is released so the next reconcile retries it.
func (p *Provisioner) RecoverOrphanedScaleSetWorkers(ctx context.Context, target Target, inFlight []OrphanedWorker) (<-chan struct{}, error) {
	if len(inFlight) == 0 {
		return closedChan(), nil
	}
	key := target.Key()
	if !p.orphanScans.claim(key.String()) {
		return closedChan(), nil
	}
	log := p.logForKey(key)

	live, err := p.liveScaleSetWorkerPodNames(ctx, target)
	if err != nil {
		p.orphanScans.release(key.String())
		return closedChan(), err
	}

	spec, err := target.Resolve(ctx)
	if err != nil {
		p.orphanScans.release(key.String())
		return closedChan(), fmt.Errorf("provisioner: resolve provisioning spec for orphaned-worker recovery: %w", err)
	}

	var recoveries []<-chan struct{}
	for _, w := range inFlight {
		podName := scaleSetPodName(key.Name, w.JobID)
		if live[podName] {
			continue
		}
		podLog := log.With("podName", podName, "jobID", w.JobID)
		podLog.Warn("worker pod for an unconcluded job was gone when this AGC started; its run lost its worker unobserved",
			"runID", w.RunID)
		target.RecordEvent(corev1.EventTypeWarning, "OrphanedWorkerRecovered", "RecoverOrphanedWorker",
			fmt.Sprintf("worker pod %s was gone when this controller started and its job never concluded, so run %s is being re-run; the disruption's cause was lost with the pod", podName, w.RunID))
		recoveries = append(recoveries,
			p.handleEviction(ctx, target, w.Owner, w.Repository, w.RunID, podLog,
				spec.MaxEvictionRetries, spec.EvictionRetryDelay, evictionTierScaleSet, recoveryCauseVanished))
	}

	done := make(chan struct{})
	go func() {
		for _, r := range recoveries {
			<-r
		}
		close(done)
	}()
	return done, nil
}

// release returns an owner's claim so a scan that could not read the cluster is retried
// rather than silently skipped for the life of the process.
func (s *orphanScanState) release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.seen, key)
}

// liveScaleSetWorkerPodNames names this owner's scale-set worker pods, in any phase. One
// List answers a whole in-flight set, rather than a Get per entry.
//
// It goes through the uncached reader, unlike the disruption scan's own List: here the
// verdict is driven by a pod NOT being listed, so an informer cache that has not caught
// up would read as every worker in the set having been disrupted. Once per process per
// set, so the uncached read costs nothing in steady state.
func (p *Provisioner) liveScaleSetWorkerPodNames(ctx context.Context, target Target) (map[string]bool, error) {
	key := target.Key()
	selector := map[string]string{LabelAcquisitionProtocol: AcquisitionProtocolScaleSet}
	for k, v := range target.PodOwnerLabels() {
		selector[k] = v
	}
	reader := client.Reader(p.Client)
	if p.APIReader != nil {
		reader = p.APIReader
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods,
		client.InNamespace(key.Namespace),
		client.MatchingLabels(selector),
	); err != nil {
		return nil, fmt.Errorf("provisioner: list scale-set worker pods for orphaned-worker recovery: %w", err)
	}
	names := make(map[string]bool, len(pods.Items))
	for i := range pods.Items {
		names[pods.Items[i].Name] = true
	}
	return names, nil
}
