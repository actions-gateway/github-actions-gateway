package provisioner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// waitForCompletion blocks until the pod reaches a terminal phase, delegating to
// the event-driven Waiter when one is wired (production) and otherwise falling
// back to the poll loop (fake-client unit tests).
func (p *Provisioner) waitForCompletion(ctx context.Context, namespace, podName string) (PodOutcome, error) {
	if p.Waiter != nil {
		return p.Waiter.WaitForCompletion(ctx, namespace, podName)
	}
	return p.waitForPodCompletion(ctx, namespace, podName)
}

// waitForPodCompletion polls until the pod reaches a terminal phase. It is the
// fallback used when no Waiter is wired; production replaces it with the
// event-driven InformerPodWaiter (see Provisioner.Waiter).
//
// The last observed pod is retained so that a pod which disappears between two ticks —
// the shape a preemption takes, since the scheduler deletes its victim — can still be
// reported as preempted from the state this loop did see. Polling is inherently lossier
// than the informer here (a preemption that starts and completes inside one interval
// leaves nothing to observe), which is another reason production wires the Waiter.
func (p *Provisioner) waitForPodCompletion(ctx context.Context, namespace, podName string) (PodOutcome, error) {
	interval := p.PollInterval
	if interval == 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var preempted bool
	for {
		select {
		case <-ctx.Done():
			return PodOutcome{}, ctx.Err()
		case <-ticker.C:
			var pod corev1.Pod
			if err := p.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: podName}, &pod); err != nil {
				if client.IgnoreNotFound(err) == nil {
					// Pod was deleted externally; treat as completion.
					return PodOutcome{Phase: corev1.PodSucceeded, Preempted: preempted}, nil
				}
				return PodOutcome{}, fmt.Errorf("provisioner: watch pod: %w", err)
			}
			preempted = preempted || PreemptedByScheduler(&pod)
			if out, ok := terminalPhase(&pod); ok {
				out.Preempted = out.Preempted || preempted
				return out, nil
			}
		}
	}
}

// reapReasonJobAbandoned labels a worker pod deleted because the listener gave up on
// the job it was running. It joins the shared reap-reason vocabulary of
// actions_gateway_worker_pods_reaped_total and doubles as the AnnotationDeletionReason
// value, exactly as the reconciler reaper's reasons do.
const reapReasonJobAbandoned = "job_abandoned"

// abandonedWorkerCleanupTimeout bounds the reclaim below, which always runs on a
// detached context — the job context is cancelled by the time it is reached.
const abandonedWorkerCleanupTimeout = 10 * time.Second

// reclaimAbandonedWorker deletes the worker pod of a job the listener abandoned, so
// its slot and its node are freed rather than held until the kubelet enforces
// spec.maxWorkerLifetime — up to 12 hours by default (Q501). GitHub has already
// recycled such a job and redelivered it to a sibling session, so the pod is running
// work nothing will ever report.
//
// It is a no-op unless ctx carries listener.ErrJobAbandoned as its cancellation
// cause. A process-wide shutdown cancels every job context at once and is
// indistinguishable from this one by ctx.Err() alone; acting on it would kill every
// live job on an AGC rollout.
//
// The delete is graceful, so the wrapper's SIGTERM relay reaches the runner and the
// job is reported instead of being SIGKILLed (Q385). It is stamped as the AGC's own
// first — stamp-then-delete, as the reaper does — so Q502's graceful-deletion
// recovery cannot read it as a disruption and re-run a job the gateway deliberately
// gave up on and GitHub is already retrying.
func (p *Provisioner) reclaimAbandonedWorker(ctx context.Context, target Target, podName string, log *slog.Logger) {
	cause := context.Cause(ctx)
	if !errors.Is(cause, listener.ErrJobAbandoned) {
		return
	}
	key := target.Key()
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abandonedWorkerCleanupTimeout)
	defer cancel()

	var pod corev1.Pod
	if err := p.Client.Get(dctx, client.ObjectKey{Namespace: key.Namespace, Name: podName}, &pod); err != nil {
		if client.IgnoreNotFound(err) != nil {
			log.Warn("could not read the abandoned job's worker pod to reclaim it", "error", err)
		}
		return
	}
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[AnnotationDeletionReason] = reapReasonJobAbandoned
	if err := p.Client.Patch(dctx, &pod, patch); err != nil {
		if client.IgnoreNotFound(err) != nil {
			// Deleting an unstamped pod would look like a drain, so leave it to the
			// lifetime cap rather than trade an orphan for a spurious re-run.
			log.Warn("could not mark the abandoned job's worker pod for deletion; leaving it to spec.maxWorkerLifetime", "error", err)
		}
		return
	}
	if err := p.Client.Delete(dctx, &pod, client.Preconditions{UID: &pod.UID}); err != nil {
		if client.IgnoreNotFound(err) != nil {
			log.Warn("could not delete the abandoned job's worker pod", "error", err)
		}
		return
	}
	log.Info("reclaimed the worker pod of an abandoned job", "cause", cause)
	if p.Metrics != nil {
		p.Metrics.WorkerPodsReaped.WithLabelValues(key.Namespace, key.Name,
			target.PodOwnerLabels()[LabelRunnerSet], reapReasonJobAbandoned).Inc()
	}
}

// deletePod deletes a worker pod, tolerating NotFound (the reaper or an
// external actor may have removed it first).
func (p *Provisioner) deletePod(ctx context.Context, namespace, name string) error {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := p.Client.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
		p.logFor().Warn("failed to delete worker pod", "pod", name, "error", err)
		return err
	}
	return nil
}

func (p *Provisioner) deleteSecret(ctx context.Context, namespace, name string) error {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := p.Client.Delete(ctx, s); client.IgnoreNotFound(err) != nil {
		p.logFor().Warn("failed to delete job Secret", "secret", name, "error", err)
		return err
	}
	return nil
}
