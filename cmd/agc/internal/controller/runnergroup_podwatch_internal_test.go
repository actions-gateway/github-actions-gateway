package controller

import (
	"context"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func workerPod(name, ns, group string, phase corev1.PodPhase) *corev1.Pod {
	labels := map[string]string{}
	if group != "" {
		labels[provisioner.LabelRunnerGroup] = group
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

// preemptedWorkerPod is a worker pod carrying the DisruptionTarget condition
// kube-scheduler stamps on a preemption victim before deleting it.
func preemptedWorkerPod(name, ns, group string, phase corev1.PodPhase) *corev1.Pod {
	p := workerPod(name, ns, group, phase)
	p.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.DisruptionTarget,
		Status: corev1.ConditionTrue,
		Reason: corev1.PodReasonPreemptionByScheduler,
	}}
	return p
}

func TestPodToRunnerGroup_MapsLabelledPod(t *testing.T) {
	r := &RunnerGroupReconciler{}
	reqs := r.podToRunnerGroup(context.Background(),
		workerPod("runner-rg-abc", "tenant-ns", "my-rg", corev1.PodRunning))

	require.Len(t, reqs, 1)
	assert.Equal(t, "tenant-ns", reqs[0].Namespace)
	assert.Equal(t, "my-rg", reqs[0].Name)
}

func TestPodToRunnerGroup_IgnoresUnlabelledPod(t *testing.T) {
	r := &RunnerGroupReconciler{}
	reqs := r.podToRunnerGroup(context.Background(),
		workerPod("some-pod", "tenant-ns", "", corev1.PodRunning))

	assert.Nil(t, reqs)
}

func TestWorkerPodPredicate_CreateAndDelete(t *testing.T) {
	p := workerPodPredicate()
	labelled := workerPod("p", "ns", "my-rg", corev1.PodPending)
	unlabelled := workerPod("p", "ns", "", corev1.PodPending)

	assert.True(t, p.Create(event.CreateEvent{Object: labelled}), "create on worker pod")
	assert.False(t, p.Create(event.CreateEvent{Object: unlabelled}), "create on non-worker pod")
	assert.True(t, p.Delete(event.DeleteEvent{Object: labelled}), "delete on worker pod")
	assert.False(t, p.Delete(event.DeleteEvent{Object: unlabelled}), "delete on non-worker pod")
}

func TestWorkerPodPredicate_UpdateOnlyOnPhaseChange(t *testing.T) {
	p := workerPodPredicate()

	// Eviction: Running → Failed is a phase change and must wake the controller.
	evicted := p.Update(event.UpdateEvent{
		ObjectOld: workerPod("p", "ns", "my-rg", corev1.PodRunning),
		ObjectNew: workerPod("p", "ns", "my-rg", corev1.PodFailed),
	})
	assert.True(t, evicted, "phase change (eviction) should pass")

	// Status heartbeat with no phase change must not trigger a reconcile.
	noChange := p.Update(event.UpdateEvent{
		ObjectOld: workerPod("p", "ns", "my-rg", corev1.PodRunning),
		ObjectNew: workerPod("p", "ns", "my-rg", corev1.PodRunning),
	})
	assert.False(t, noChange, "no phase change should be filtered out")

	// A phase change on a pod without the label is still ignored.
	unlabelled := p.Update(event.UpdateEvent{
		ObjectOld: workerPod("p", "ns", "", corev1.PodRunning),
		ObjectNew: workerPod("p", "ns", "", corev1.PodFailed),
	})
	assert.False(t, unlabelled, "non-worker pod should be filtered out")
}

// TestWorkerPodPredicate_UpdateOnPreemptionMarker covers the one non-phase update the
// predicate must admit (Q497). A scheduler preemption stamps a DisruptionTarget
// condition and deletes the victim; a Pending worker stays Pending throughout, so on the
// phase-change edge alone the first event reaching the reconciler would be the Delete —
// by which point the pod is out of the cache and the recovery scan has nothing left to
// read the workflow-run identity off, and the displaced run needs a manual re-run.
func TestWorkerPodPredicate_UpdateOnPreemptionMarker(t *testing.T) {
	p := workerPodPredicate()

	// The edge itself: the marker appears, with no phase change at all.
	assert.True(t, p.Update(event.UpdateEvent{
		ObjectOld: workerPod("p", "ns", "my-rg", corev1.PodPending),
		ObjectNew: preemptedWorkerPod("p", "ns", "my-rg", corev1.PodPending),
	}), "a worker newly marked as a preemption victim must wake the reconciler")

	// Already marked, and still marked: a later heartbeat on a terminating victim adds
	// nothing, and admitting it would put the pod's whole grace period of updates on the
	// work queue.
	assert.False(t, p.Update(event.UpdateEvent{
		ObjectOld: preemptedWorkerPod("p", "ns", "my-rg", corev1.PodPending),
		ObjectNew: preemptedWorkerPod("p", "ns", "my-rg", corev1.PodPending),
	}), "a repeat of an already-observed preemption marker must be filtered out")

	// A preemption that also changes phase is already covered by the phase edge; it must
	// not be dropped by the new one.
	assert.True(t, p.Update(event.UpdateEvent{
		ObjectOld: preemptedWorkerPod("p", "ns", "my-rg", corev1.PodRunning),
		ObjectNew: preemptedWorkerPod("p", "ns", "my-rg", corev1.PodSucceeded),
	}), "a marked victim reaching a terminal phase must still wake the reconciler")

	// The label gate applies to the new edge exactly as to the old one.
	assert.False(t, p.Update(event.UpdateEvent{
		ObjectOld: workerPod("p", "ns", "", corev1.PodPending),
		ObjectNew: preemptedWorkerPod("p", "ns", "", corev1.PodPending),
	}), "a preempted pod that is not one of our workers must be filtered out")
}

func TestWorkerPodPredicate_IgnoresGeneric(t *testing.T) {
	p := workerPodPredicate()
	assert.False(t, p.Generic(event.GenericEvent{
		Object: workerPod("p", "ns", "my-rg", corev1.PodRunning),
	}))
}
