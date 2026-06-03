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

func TestWorkerPodPredicate_IgnoresGeneric(t *testing.T) {
	p := workerPodPredicate()
	assert.False(t, p.Generic(event.GenericEvent{
		Object: workerPod("p", "ns", "my-rg", corev1.PodRunning),
	}))
}
