package provisioner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The out-of-memory class's wait for a start (Q1389). Until its JobStarted is handled a
// worker's pod names the job it was created for, which is often not the job its runner
// took, and acting on it would raise another job's limit and spend another run's slot. So
// a kill whose pod carries no start is left unclaimed until the start stamps it, which a
// Failed pod takes only in this state (MarkScaleSetJobStarted). Waiting delays no re-run:
// the class's re-run waits for the run to conclude, which needs the killed job to. A start
// that never comes raises nothing and re-runs nothing.

const (
	// startWait is how long a kill waits for its worker's start. GitHub concluded a killed
	// job 10 minutes after it started (measured 2026-11-24, three kills, each in the job's
	// first 17 s), so it outlasts the job.
	startWait = 15 * time.Minute
	// startWaitCap bounds the wait from the pod's end, however often the AGC restarts.
	startWaitCap = time.Hour
	// startWaitSlack is how much longer the reaper keeps the pod, for a scan that keeps
	// failing to end the wait.
	startWaitSlack = 5 * time.Minute
	// startWaitRetry is when a reconcile looks again at a kill whose recovery is due and
	// not yet claimed.
	startWaitRetry = 30 * time.Second
)

// claimCoversNoJob is the JobID of a claim that covers no job. An empty one covers the job
// the claimed worker was created for.
const claimCoversNoJob = "none"

// oomRetryable reports whether pod is an unclaimed out-of-memory kill the class may retry:
// a worker a sizing profile built, whose runner had a memory limit, and which no disruption
// arm takes. A worker with no runner name is never stamped with a start.
func oomRetryable(pod *corev1.Pod) bool {
	if _, disrupted := disruptionAwaitingRecovery(pod); disrupted {
		return false
	}
	limit := containerMemoryLimit(pod, runnerContainer)
	return oomKilledAwaitingRecovery(pod) && pod.Annotations[AnnotationSizingProfile] != "" &&
		pod.Annotations[AnnotationRunnerName] != "" && !limit.IsZero()
}

// oomAwaitingStart reports whether pod's kill waits for its worker's start.
func oomAwaitingStart(pod *corev1.Pod) bool {
	return oomRetryable(pod) && pod.Annotations[AnnotationStartedJobID] == ""
}

// StartedAfterFailure reports whether the update from before to after stamped a start on a
// Failed worker whose disruption is unclaimed, the edge that ends its wait.
func StartedAfterFailure(before, after *corev1.Pod) bool {
	_, handled := after.Annotations[AnnotationEvictionHandledAt]
	return after.Status.Phase == corev1.PodFailed && !handled &&
		before.Annotations[AnnotationStartedJobID] == "" && after.Annotations[AnnotationStartedJobID] != ""
}

// startWaitState records when this process first scanned each set. A kill's wait runs from
// there where that is later than the pod's end, because a restarted AGC scans before its
// listener has polled the queue the start is in.
type startWaitState struct {
	mu        sync.Mutex
	scannedAt map[string]time.Time
}

func (s *startWaitState) scanned(set string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scannedAt == nil {
		s.scannedAt = map[string]time.Time{}
	}
	if _, ok := s.scannedAt[set]; !ok {
		s.scannedAt[set] = now
	}
}

// deadline is when pod's wait ends, for a set this process has or has not scanned yet.
func (s *startWaitState) deadline(set string, pod *corev1.Pod, now time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	ended := PodTerminalTime(pod)
	from, ok := s.scannedAt[set]
	if !ok {
		from = now
	}
	if from.Before(ended) {
		from = ended
	}
	if limit := ended.Add(startWaitCap); from.Add(startWait).After(limit) {
		return limit
	}
	return from.Add(startWait)
}

// HoldUnclaimedOOM reports whether the reaper must keep pod, a terminal worker of target
// whose out-of-memory kill has not been claimed, until when, and when a reconcile should
// look at it again.
func (p *Provisioner) HoldUnclaimedOOM(target Target, pod *corev1.Pod, now time.Time) (until, look time.Time, hold bool) {
	if !oomRetryable(pod) {
		return time.Time{}, time.Time{}, false
	}
	deadline := p.startWaits.deadline(target.Key().String(), pod, now)
	if until = deadline.Add(startWaitSlack); !now.Before(until) {
		return time.Time{}, time.Time{}, false
	}
	look = now.Add(startWaitRetry)
	if oomAwaitingStart(pod) && deadline.After(look) {
		look = deadline
	}
	return until, look, true
}

// awaitStart handles d's pod, an out-of-memory kill with no start recorded. It returns the
// pod to recover where its start is recorded after all, and nil where there is nothing to
// do yet, or ever.
func (p *Provisioner) awaitStart(ctx context.Context, target Target, pod *corev1.Pod, log *slog.Logger) *corev1.Pod {
	if now := p.nowFn(); now.Before(p.startWaits.deadline(target.Key().String(), pod, now)) {
		log.Debug("runner was killed for memory before its job start was recorded; waiting for the start",
			"runner", pod.Annotations[AnnotationRunnerName])
		return nil
	}
	live := &corev1.Pod{}
	switch err := p.liveReader().Get(ctx, client.ObjectKeyFromObject(pod), live); {
	case apierrors.IsNotFound(err):
	case err != nil:
		log.Warn("could not read a killed worker whose wait for its job start is over; the next scan retries", "error", err)
		return nil
	case live.UID != pod.UID:
		// The name is another pod's now, and this one's deletion settles it.
		return nil
	case !oomAwaitingStart(live):
		return live
	}
	if p.reportUnstartedKill(ctx, target, pod, log) {
		if err := p.stampEvictionHandled(ctx, pod); err != nil {
			log.Debug("could not stamp the eviction-handled marker on a claimed scale-set worker", "error", err)
		}
	}
	return nil
}

// SettleDeletedKill settles pod, a deleted worker of target whose out-of-memory kill is
// unclaimed: one whose start is recorded is recovered, and one still waiting is reported,
// since nothing is left for the start to be stamped on. The returned channel closes when
// the recovery finishes.
func (p *Provisioner) SettleDeletedKill(ctx context.Context, target Target, pod *corev1.Pod) (<-chan struct{}, error) {
	if pod.Labels[LabelAcquisitionProtocol] != AcquisitionProtocolScaleSet || !oomRetryable(pod) {
		return closedChan(), nil
	}
	if oomAwaitingStart(pod) {
		p.reportUnstartedKill(ctx, target, pod, p.logForKey(target.Key()).With("podName", pod.Name))
		return closedChan(), nil
	}
	return p.RecoverDisruptedScaleSetWorker(ctx, target, pod)
}

// reportUnstartedKill claims pod's kill for no job and reports that the job it was running
// is unknown, reporting whether the claim was this call's.
func (p *Provisioner) reportUnstartedKill(ctx context.Context, target Target, pod *corev1.Pod, log *slog.Logger) bool {
	key := target.Key()
	switch err := p.claimDisruptionRecovery(ctx, target, pod.Name, claimCoversNoJob, recoveryCauseOOM); {
	case errors.Is(err, errRecoveryClaimHeld):
		return false
	case err != nil:
		log.Warn("could not claim a killed worker with no job start recorded; a later scan retries while its pod is kept", "error", err)
		return false
	}
	runner := pod.Annotations[AnnotationRunnerName]
	log.Warn("runner was killed for memory and no job start was ever recorded for it; no automatic re-run", "runner", runner)
	if p.Metrics != nil {
		p.Metrics.EvictionRecoveryIdentityUnknown.WithLabelValues(key.Namespace, key.Name, recoveryCauseOOM).Inc()
	}
	target.RecordEvent(corev1.EventTypeWarning, "EvictionRecoveryIdentityUnknown", "RecoverEvictedWorker",
		fmt.Sprintf("worker pod %s had its runner %s killed at its memory limit before the start of any job was recorded for it, so which job it was running is not known: no limit was raised and nothing was re-run, and a job it was running needs a manual re-run", pod.Name, runner))
	return true
}
