package provisioner

import (
	"context"
	"fmt"
	"strings"
	"sync"
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
// reconciler, which already watches worker pods for phase changes and already lists
// them every reconcile to reap them. Both halves are durable rather than
// process-scoped, so an AGC that restarts between a worker's eviction and its recovery
// still recovers it — the property Q420 chose a pod annotation over in-memory state to
// get, for the same fire-and-forget reason.
//
// What is reused unchanged: handleEviction, reserveEvictionRetry, the sharded
// per-run-id lock, and the sweeper. The Q106 invariant — at most maxEvictionRetries
// re-runs per run_id — is a property of that shared budget, so it holds across both
// tiers at once rather than per tier.

// evictionRecoveryAPIBudget is how long a detached recovery may spend on GitHub after
// its evictionRetryDelay has elapsed. Recovery outlives the reconcile that started it
// (the reconcile context is cancelled the moment Reconcile returns, while
// handleEviction deliberately waits out the retry delay first), so it runs on a
// context detached from the caller's and bounded by delay + this budget. The bound is
// what keeps a wedged GitHub call from leaving a goroutine behind indefinitely.
const evictionRecoveryAPIBudget = 60 * time.Second

// Eviction-recovery tier labels for the eviction metrics, so an operator can tell a
// classic recovery from a scale-set one — the two are detected by entirely different
// machinery, and "eviction recovery works" was true of only one of them before Q417.
const (
	evictionTierClassic  = "classic"
	evictionTierScaleSet = "scaleset"
)

// RecoverEvictedScaleSetWorkers finds this owner's scale-set worker pods that the
// kubelet evicted, claims each one, and triggers the same automatic re-run the classic
// tier performs. It returns a done channel that closes once every recovery this call
// started has finished, so a caller may block on it (tests) or ignore it (the
// reconciler, which must not stall a reconcile on GitHub).
//
// It is safe and cheap to call every reconcile: the List is served from the shared
// informer cache, and a pod is acted on at most once ever (see
// AnnotationEvictionHandledAt). It is a no-op for an owner with no evicted workers,
// and for the classic tier, whose pods carry no LabelAcquisitionProtocol.
//
// Call it BEFORE the reaper. Recovery reads the evicted pod, and the reaper deletes
// terminal pods once spec.completedPodTTL elapses; reaping first would drop the
// evidence on any pod whose eviction the AGC did not observe promptly (a restart, a
// backlogged work queue), turning a recoverable job into a manual re-run.
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

	var evicted []*corev1.Pod
	for i := range pods.Items {
		if evictedAwaitingRecovery(&pods.Items[i]) {
			evicted = append(evicted, &pods.Items[i])
		}
	}
	if len(evicted) == 0 {
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

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), spec.EvictionRetryDelay+evictionRecoveryAPIBudget)
	var wg sync.WaitGroup
	for _, pod := range evicted {
		podLog := log.With("podName", pod.Name)

		// Claim before calling GitHub, under an optimistic lock: whoever wins the patch
		// owns this pod's single recovery attempt. A lost race (another replica, or a
		// concurrent reconcile of the same owner) is the mechanism working, not an
		// error.
		if err := p.claimEvictionRecovery(rctx, pod); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				podLog.Debug("scale-set worker eviction already claimed elsewhere; skipping", "error", err)
				continue
			}
			podLog.Warn("could not claim scale-set worker eviction for recovery; skipping", "error", err)
			continue
		}

		owner, repo, runID, ok := runIdentityFromPod(pod)
		if !ok {
			// The assignment message carried no complete run identity, so there is
			// nothing to re-run. Surface it: this is the one failure mode that makes the
			// whole mechanism silently inert, and an operator seeing evicted jobs stay
			// failed needs to be told why rather than left to infer it.
			podLog.Warn("scale-set worker was evicted but its run identity is unknown; automatic re-run skipped")
			if p.Metrics != nil {
				p.Metrics.EvictionRecoveryIdentityUnknown.WithLabelValues(key.Namespace, key.Name).Inc()
			}
			target.RecordEvent(corev1.EventTypeWarning, "EvictionRecoveryIdentityUnknown", "RecoverEvictedWorker",
				fmt.Sprintf("worker pod %s was evicted but carries no workflow-run identity, so its job cannot be re-run automatically; a manual re-run is required", pod.Name))
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			p.handleEviction(rctx, target, owner, repo, runID, podLog, spec.MaxEvictionRetries, spec.EvictionRetryDelay, evictionTierScaleSet)
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		cancel()
		close(done)
	}()
	return done, nil
}

// evictedAwaitingRecovery reports whether pod is a scale-set worker the kubelet
// evicted and whose eviction has not been adjudicated yet.
//
// The phase/reason test is the classic path's, verbatim: PodFailed with
// Status.Reason == "Evicted" is the kubelet's node-pressure eviction, and it is
// precisely the case where nothing inside the pod got to run. That is what makes
// firing a re-run here safe from double-reporting, and it is the intent rather than a
// reliance on the rerun call's benign 404/410 handling:
//
//   - A node-pressure eviction SIGKILLs the pod. The runner reports nothing, GitHub
//     concludes the job only when the job lock lapses, and without this mechanism the
//     job stays failed until a human re-runs it. This is the case handled here.
//   - A graceful deletion (an API-initiated eviction such as `kubectl drain`, or the
//     reaper) sends SIGTERM, which Q385's relay hands to the runner so it reports its
//     own outcome. The pod is deleted rather than left PodFailed/Evicted, so it fails
//     this test and no second report is produced. Deletion is deliberately NOT covered
//     here: covering it would double-report exactly the case the relay already owns.
//
// A pod already carrying AnnotationEvictionHandledAt is skipped, which is what makes
// recovery at-most-once per evicted pod across reconciles, restarts, and replicas.
func evictedAwaitingRecovery(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodFailed || pod.Status.Reason != podReasonEvicted {
		return false
	}
	_, handled := pod.Annotations[AnnotationEvictionHandledAt]
	return !handled
}

// podReasonEvicted is the Pod.Status.Reason the kubelet sets when it evicts a pod
// under node pressure — the single signal both tiers branch on.
const podReasonEvicted = "Evicted"

// claimEvictionRecovery stamps AnnotationEvictionHandledAt on pod under an optimistic
// lock, so exactly one caller ever proceeds to recover it. The optimistic lock (rather
// than a plain merge patch) is the whole point: two AGC replicas reconciling the same
// owner would otherwise both patch successfully and both call rerun-failed-jobs,
// spending two slots of one run's retry budget for one eviction.
//
// A Conflict means someone else claimed it; a NotFound means the pod was reaped first.
// Both are returned as-is for the caller to recognise and skip.
func (p *Provisioner) claimEvictionRecovery(ctx context.Context, pod *corev1.Pod) error {
	patch := client.MergeFromWithOptions(pod.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[AnnotationEvictionHandledAt] = p.nowFn().UTC().Format(time.RFC3339)
	return p.Client.Patch(ctx, pod, patch)
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
