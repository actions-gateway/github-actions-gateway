package provisioner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Eviction recovery on the scale-set tier (Q417).
//
// # Why this is not the classic mechanism
//
// On the classic tier, provision() holds a goroutine open for the whole job: it waits
// for the worker pod's terminal phase, reads reason == "Evicted" off it, and calls
// handleEviction with the owner/repo/run_id it parsed out of the AcquireJob payload it
// still has in hand. Every input is in memory, because the acquiring goroutine never
// left.
//
// ProvisionScaleSetWorker is fire-and-forget by design — the runner pulls and completes
// its own job through its own session, so there is no payload and no goroutine. Both of
// the classic mechanism's inputs are therefore missing, and the capability did not
// exist on the tier that v2beta1 is built on.
//
// # The shape that replaces it
//
// Identity moves onto the pod: the assignment message carries the workflow run
// (scaleset.JobMessage.RunIdentity), and ProvisionScaleSetWorker stamps it as the
// AnnotationRunID / AnnotationRepository annotations. Detection moves to the owning
// reconciler, in two paths that share one judge and one claim: its worker-pod watch
// hands each phase-changing event's pod to RecoverDisruptedScaleSetWorker as it
// arrives (Q1029), and RecoverEvictedScaleSetWorkers lists every reconcile for what no
// event reached. Both halves are durable rather than process-scoped, so an AGC that
// restarts between a worker's eviction and its recovery still recovers it — the
// property Q420 chose a pod annotation over in-memory state to get, for the same
// fire-and-forget reason.
//
// What is reused unchanged: handleEviction, reserveEvictionRetry, the sharded
// per-run-id lock, and the sweeper. The Q106 invariant — at most maxEvictionRetries
// re-runs per run_id — is a property of that shared budget, so it holds across both
// tiers at once rather than per tier.

// Eviction-recovery tier labels for the eviction metrics, so an operator can tell a
// classic recovery from a scale-set one — the two are detected by entirely different
// machinery, and "eviction recovery works" was true of only one of them before Q417.
const (
	evictionTierClassic  = "classic"
	evictionTierScaleSet = "scaleset"
)

// RecoverEvictedScaleSetWorkers finds this owner's scale-set worker pods that lost their
// job to a disruption — the kubelet's node-pressure eviction, kube-scheduler preemption
// (Q497), or an external graceful deletion such as a drain (Q502) — claims each one, and
// triggers the same automatic re-run the classic tier performs. A worker deleted before
// any container ran takes the same claim but a different action: its run is
// force-cancelled first, because nothing failed for rerun-failed-jobs to act on (Q766,
// abandoned_scaleset.go). It returns a done channel that closes once every recovery this
// call started has finished, so a caller may block on it (tests) or ignore it (the
// reconciler, which must not stall a reconcile on GitHub).
//
// It is safe and cheap to call every reconcile: the List is served from the shared
// informer cache, and a pod is acted on at most once ever (see
// AnnotationEvictionHandledAt). It is a no-op for an owner with no disrupted workers,
// and for the classic tier, whose pods carry no LabelAcquisitionProtocol.
//
// It is the slower of the two detection paths and the one that is not on a clock.
// RecoverDisruptedScaleSetWorker acts on the worker-pod watch event itself, which is
// what reaches a drained pod inside its teardown window (Q1029); this scan covers what
// no event reaches — a pod already terminal when the process started, or an event path
// that failed. The claim annotation arbitrates between the two.
//
// Call it BEFORE the reaper. Recovery reads the disrupted pod, and the reaper deletes
// terminal pods once spec.completedPodTTL elapses; reaping first would drop the
// evidence on any pod whose eviction the AGC did not observe promptly (a restart, a
// backlogged work queue), turning a recoverable job into a manual re-run. A preemption
// is on a tighter clock still — see disruptionAwaitingRecovery on why that one cause
// cannot be made restart-safe.
//
// A returned error means the scan itself failed, not that a recovery failed: an
// individual pod that cannot be claimed or has no run identity is logged, counted, and
// skipped, because one such pod must not stop the others from being recovered.
func (p *Provisioner) RecoverEvictedScaleSetWorkers(ctx context.Context, target Target) (<-chan struct{}, error) {
	key := target.Key()
	log := p.logForKey(key)

	selector := map[string]string{LabelAcquisitionProtocol: AcquisitionProtocolScaleSet}
	for k, v := range target.PodOwnerLabels() {
		selector[k] = v
	}
	var pods corev1.PodList
	if err := p.Client.List(ctx, &pods,
		client.InNamespace(key.Namespace),
		client.MatchingLabels(selector),
	); err != nil {
		return closedChan(), fmt.Errorf("provisioner: list scale-set worker pods: %w", err)
	}

	var recoverable []disruptedScaleSetWorker
	for i := range pods.Items {
		if d, ok := judgeScaleSetDisruption(log, &pods.Items[i]); ok {
			recoverable = append(recoverable, d)
		}
	}
	if len(recoverable) == 0 {
		return closedChan(), nil
	}

	// Resolve only once there is something to recover, so the common path costs one
	// cached List and nothing else. maxEvictionRetries/evictionRetryDelay come from the
	// same resolved spec the classic path passes to handleEviction, so a per-owner
	// override applies identically on both tiers.
	spec, err := target.Resolve(ctx)
	if err != nil {
		return closedChan(), fmt.Errorf("provisioner: resolve provisioning spec for eviction recovery: %w", err)
	}

	var recoveries []<-chan struct{}
	for _, d := range recoverable {
		recoveries = append(recoveries, p.recoverDisruptedScaleSetWorker(ctx, target, spec, log, d))
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

// RecoverDisruptedScaleSetWorker is RecoverEvictedScaleSetWorkers for one pod, handed
// straight from the owning reconciler's worker-pod watch with the event's own object
// (Q1029). The scan lists from the informer cache at the top of a reconcile, so a
// drained worker — readable only for the seconds between the kubelet publishing its
// terminal phase and removing the object — was recovered only when a reconcile
// happened to begin inside that window. A reconcile already in flight holds the next
// one past it: the queue runs one reconcile of a key at a time, and the listener
// bootstrap does GitHub I/O inside Reconcile. The watch event carries the pod at the
// moment it became recoverable, so this path claims it without waiting for the queue.
//
// pod is judged as delivered and mutated by the claim, so pass the event's object —
// never a re-read, which can already be NotFound — as a copy the cache does not own. A
// pod that is not a scale-set worker, or not a recoverable disruption, is a no-op. The
// claim arbitrates against the scan and against other replicas exactly as it does for
// the scan, so a pod both paths see is recovered once.
func (p *Provisioner) RecoverDisruptedScaleSetWorker(ctx context.Context, target Target, pod *corev1.Pod) (<-chan struct{}, error) {
	if pod.Labels[LabelAcquisitionProtocol] != AcquisitionProtocolScaleSet {
		return closedChan(), nil
	}
	log := p.logForKey(target.Key())
	d, ok := judgeScaleSetDisruption(log, pod)
	if !ok {
		return closedChan(), nil
	}
	spec, err := target.Resolve(ctx)
	if err != nil {
		return closedChan(), fmt.Errorf("provisioner: resolve provisioning spec for eviction recovery: %w", err)
	}
	return p.recoverDisruptedScaleSetWorker(ctx, target, spec, log, d), nil
}

// disruptedScaleSetWorker pairs a recoverable pod with the cause that made it
// recoverable, so the metrics and the operator-facing wording downstream say which
// disruption actually happened.
type disruptedScaleSetWorker struct {
	pod   *corev1.Pod
	cause string
	// abandoned routes the pod to the force-cancel path instead of straight to
	// rerun-failed-jobs: it never ran its job, so the run has to be concluded before
	// a re-run is legal (Q766, abandoned_scaleset.go).
	abandoned bool
}

// judgeScaleSetDisruption classifies one unclaimed scale-set worker pod against the
// recovery arms. A terminating worker neither arm accepted is logged, and saying so is
// the point: an unrecovered pod looks the same whether it was judged or never seen
// (Q549). Debug because declining is usually right — a cleanup delete of an
// already-failed pod is exactly what the ordering check exists to reject.
func judgeScaleSetDisruption(log *slog.Logger, pod *corev1.Pod) (disruptedScaleSetWorker, bool) {
	switch cause, ok := disruptionAwaitingRecovery(pod); {
	case ok:
		return disruptedScaleSetWorker{pod: pod, cause: cause}, true
	case abandonedAwaitingRecovery(pod):
		return disruptedScaleSetWorker{pod: pod, cause: recoveryCauseAbandoned, abandoned: true}, true
	case externallyDeletedTerminalWorker(pod):
		log.Debug("externally deleted scale-set worker did not qualify as a recoverable disruption; no automatic re-run",
			"podName", pod.Name,
			"deletionRequestedAt", deletionRequestedAt(pod).UTC().Format(time.RFC3339),
			"terminatedAt", podTerminationRecordTime(pod).UTC().Format(time.RFC3339))
	}
	return disruptedScaleSetWorker{}, false
}

// recoverDisruptedScaleSetWorker claims d's pod and starts its recovery — the per-pod
// step both detection paths share. The returned done channel closes when the recovery
// finishes, at once where the claim was lost or the pod carries nothing to re-run.
func (p *Provisioner) recoverDisruptedScaleSetWorker(ctx context.Context, target Target, spec *ResolvedSpec, log *slog.Logger, d disruptedScaleSetWorker) <-chan struct{} {
	key := target.Key()
	// cause is deliberately NOT on podLog: handleEviction puts it on every line it
	// emits, and a logger-level attribute would duplicate the key on those.
	pod, cause := d.pod, d.cause
	podLog := log.With("podName", pod.Name)

	// Claim before calling GitHub, under an optimistic lock: whoever wins the patch
	// owns this pod's single recovery attempt.
	if err := p.claimEvictionRecovery(ctx, pod); err != nil {
		switch {
		case apierrors.IsConflict(err):
			// A conflict that survived the re-read retry: the fresh object already
			// carries the claim, so the other detection path, another replica, or a
			// concurrent reconcile of the same owner owns this recovery. The mechanism
			// working, not an error.
			podLog.Debug("scale-set worker disruption already claimed elsewhere; skipping", "cause", cause, "error", err)
		case apierrors.IsNotFound(err):
			// The pod went away before the claim landed. What is lost is the lock, not
			// the record: an event caller still holds the run identity on its copy, but
			// re-running without the annotation would let a replica or the other path
			// re-run it again, so both callers decline. Surface it: this is a job that
			// will silently never be re-run, and the window is real (Q809 measured it on
			// the drain arm, where the kubelet removes the object seconds after the
			// container exits). The two paths are routinely concurrent claimants, so a
			// conflict whose re-read comes back NotFound lands here too and counts a run
			// the winner recovered; the metric row says so.
			podLog.Warn("scale-set worker disruption was lost before it could be claimed; its run will not be re-run automatically",
				"cause", cause, "error", err)
			if p.Metrics != nil {
				p.Metrics.EvictionRecoveryEvidenceLost.WithLabelValues(key.Namespace, key.Name, cause).Inc()
			}
			target.RecordEvent(corev1.EventTypeWarning, "EvictionRecoveryEvidenceLost", "RecoverEvictedWorker",
				fmt.Sprintf("worker pod %s was lost to %s, but its pod was deleted before the recovery could be claimed, so its job cannot be re-run automatically; a manual re-run is required", pod.Name, cause))
		default:
			podLog.Warn("could not claim scale-set worker disruption for recovery; skipping", "cause", cause, "error", err)
		}
		return closedChan()
	}

	// A never-started worker has no failed job for rerun-failed-jobs to act on, so it
	// takes the force-cancel-then-defer path rather than handleEviction — which also
	// owns its own identity read and its own identity-unknown reporting.
	if d.abandoned {
		return p.recoverAbandoned(ctx, target, pod, abandonedDetectionDeleted)
	}

	owner, repo, runID, ok := runIdentityFromPod(pod)
	if !ok {
		// The assignment message carried no complete run identity, so there is
		// nothing to re-run. Surface it: this is the one failure mode that makes the
		// whole mechanism silently inert, and an operator seeing disrupted jobs stay
		// failed needs to be told why rather than left to infer it.
		podLog.Warn("scale-set worker was disrupted but its run identity is unknown; automatic re-run skipped", "cause", cause)
		if p.Metrics != nil {
			p.Metrics.EvictionRecoveryIdentityUnknown.WithLabelValues(key.Namespace, key.Name, cause).Inc()
		}
		target.RecordEvent(corev1.EventTypeWarning, "EvictionRecoveryIdentityUnknown", "RecoverEvictedWorker",
			fmt.Sprintf("worker pod %s was lost to %s but carries no workflow-run identity, so its job cannot be re-run automatically; a manual re-run is required", pod.Name, cause))
		return closedChan()
	}

	return p.handleEviction(ctx, target, owner, repo, runID, podLog, spec.MaxEvictionRetries, spec.EvictionRetryDelay, evictionTierScaleSet, cause)
}

// disruptionAwaitingRecovery reports whether pod is a scale-set worker that lost its job
// to a disruption the gateway recovers from, and whose disruption has not been
// adjudicated yet. The returned cause labels which one, for the metrics and the
// operator-facing wording.
//
// Three causes qualify — the kubelet's node-pressure eviction (PodFailed + Status.Reason
// "Evicted"), kube-scheduler preemption (a DisruptionTarget=True/PreemptionByScheduler
// condition, Q497), and an external graceful deletion — a drain or a `kubectl delete
// pod` — whose victim reached PodFailed before the object went away (Q502; see
// deletion.go for the discriminator and its AGC-own-deletion exclusion). A pod deleted
// without ever publishing a terminal phase is NOT recovered here: it never ran its job
// to a reportable end, so there is no failed job for rerun-failed-jobs to act on. That
// shape has its own predicate and its own action, which concludes the run before
// re-running it (abandonedAwaitingRecovery, Q766). A human cancelling the run at GitHub
// deletes nothing, so it never enters any arm (measured, Q459).
//
// The full boundary, with the row-by-row reasoning for what is in and out, is the table
// in docs/design/04-operational-flows.md §4.2 ("Which disruptions are recovered").
//
// Three invariants live here rather than in the doc, because breaking each is a code
// change away:
//
//   - A pod already carrying AnnotationEvictionHandledAt is skipped. That is what makes
//     recovery at-most-once per disrupted pod across reconciles, restarts, and replicas.
//   - The preemption and deletion arms are on a deadline the eviction arm is not. An
//     evicted pod sits in PodFailed until the reaper takes it, so a late scan still
//     finds it; a preempted or drained pod is being deleted and is readable only until
//     the kubelet finishes tearing it down — the whole termination grace period for a
//     preemption victim (the condition is stamped before the delete), only the tail of
//     it for a drain (the terminal phase publishes as the container exits). Neither is
//     restart-safe, and cannot be made so — the evidence is the pod and the deletion
//     removes it. What keeps the windows reachable at all is the worker-pod watch
//     predicate admitting the update where a pod newly becomes a preemption victim, and
//     the phase-change edge for the drain shape — the events the reconciler hands to
//     RecoverDisruptedScaleSetWorker as they arrive. Do not narrow that predicate.
//   - The deletion arm shares the classic waiter's predicate
//     (externallyDeletedBeforeTerminal), which orders the mark against the pod's
//     terminal time. Without that, a cleanup delete of a pod whose job genuinely
//     failed earlier — by an operator, since the reaper stamps its own deletions —
//     would read as a disruption and re-run the failed job, and a deleted worker
//     whose container never started would re-run a job that never ran. The
//     never-started half of that is now recovered, but only by concluding the run
//     first; the exclusion here is what keeps it off THIS path (Q766).
func disruptionAwaitingRecovery(pod *corev1.Pod) (cause string, ok bool) {
	if _, handled := pod.Annotations[AnnotationEvictionHandledAt]; handled {
		return "", false
	}
	switch {
	case pod.Status.Phase == corev1.PodFailed && pod.Status.Reason == podReasonEvicted:
		return recoveryCauseEviction, true
	case PreemptedByScheduler(pod):
		return recoveryCausePreemption, true
	case pod.Status.Phase == corev1.PodFailed && externallyDeletedBeforeTerminal(pod):
		return recoveryCauseDeletion, true
	default:
		return "", false
	}
}

// externallyDeletedTerminalWorker reports whether pod is an unclaimed scale-set worker
// that something other than the AGC deleted and that reached PodFailed — the
// preconditions the two deletion-driven arms share (disruptionAwaitingRecovery's
// deletion case, and abandonedAwaitingRecovery). A pod matching it that neither arm
// accepted has been judged and declined.
//
// A drained worker is readable only briefly (RecoverDisruptedScaleSetWorker's doc has
// the window), and a judge that never got a look at it leaves the same silence a
// regressed discriminator does. Without a line for the
// declined case the two are indistinguishable, which is what left Q549 unattributable
// across three sightings.
func externallyDeletedTerminalWorker(pod *corev1.Pod) bool {
	if _, claimed := pod.Annotations[AnnotationEvictionHandledAt]; claimed {
		return false
	}
	if pod.DeletionTimestamp.IsZero() || deletedByAGC(pod) {
		return false
	}
	return pod.Status.Phase == corev1.PodFailed
}

// podReasonEvicted is the Pod.Status.Reason the kubelet sets when it evicts a pod
// under node pressure — the single signal both tiers branch on.
const podReasonEvicted = "Evicted"

// claimConflictRetries bounds the re-read retries claimEvictionRecovery makes against a
// conflict raised by a writer that is not a claimant. Small on purpose: the pod is being
// torn down while this runs, so a long retry only converts a Conflict into a NotFound.
const claimConflictRetries = 3

// claimEvictionRecovery stamps AnnotationEvictionHandledAt on pod under an optimistic
// lock, so exactly one caller ever proceeds to recover it. The optimistic lock (rather
// than a plain merge patch) is the whole point: two AGC replicas reconciling the same
// owner would otherwise both patch successfully and both call rerun-failed-jobs,
// spending two slots of one run's retry budget for one eviction.
//
// The lock arbitrates between claimants, but the apiserver raises the same Conflict for
// any concurrent write — and on the deletion arm the kubelet publishing the terminal
// phase is guaranteed to be racing, because that transition is the edge that triggers
// the reconcile. So a conflict is retried against a re-read pod, and only
// a fresh object that already carries the annotation ends the attempt: that is the one
// case where someone else really did claim it (Q809 — a kubelet status write cost a
// drained worker its recovery on run 31556806760).
//
// The returned error is the caller's verdict. A Conflict means the claim is genuinely
// another owner's; a NotFound means the pod went away, taking the only record of the
// disruption with it. pod is left holding whatever generation was last read, so a
// successful claim leaves the caller the identity annotations it needs.
func (p *Provisioner) claimEvictionRecovery(ctx context.Context, pod *corev1.Pod) error {
	for attempt := 0; ; attempt++ {
		patch := client.MergeFromWithOptions(pod.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[AnnotationEvictionHandledAt] = p.nowFn().UTC().Format(time.RFC3339)

		err := p.Client.Patch(ctx, pod, patch)
		if !apierrors.IsConflict(err) || attempt == claimConflictRetries {
			return err
		}

		var fresh corev1.Pod
		if getErr := p.Client.Get(ctx, client.ObjectKeyFromObject(pod), &fresh); getErr != nil {
			return getErr
		}
		if _, claimed := fresh.Annotations[AnnotationEvictionHandledAt]; claimed {
			return err
		}
		fresh.DeepCopyInto(pod)
	}
}

// runIdentityFromPod reads back the workflow-run identity ProvisionScaleSetWorker
// stamped on a scale-set worker pod, as the (owner, repo, run_id) triple
// rerun-failed-jobs addresses a run by. ok is false unless all three are present and
// the repository annotation is a well-formed "owner/repo" — a partial identity cannot
// name a run, and guessing at one would re-run the wrong thing.
func runIdentityFromPod(pod *corev1.Pod) (owner, repo, runID string, ok bool) {
	runID = pod.Annotations[AnnotationRunID]
	if runID == "" || runID == "0" {
		return "", "", "", false
	}
	o, r, found := strings.Cut(pod.Annotations[AnnotationRepository], "/")
	if !found || o == "" || r == "" {
		return "", "", "", false
	}
	return o, r, runID, true
}

// closedChan returns an already-closed done channel, for the paths that start no
// recovery at all. Returning a closed channel rather than nil means a caller can
// always receive from the result without a nil check.
func closedChan() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
