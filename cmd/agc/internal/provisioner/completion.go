package provisioner

import (
	"context"
	"fmt"
	"time"

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
