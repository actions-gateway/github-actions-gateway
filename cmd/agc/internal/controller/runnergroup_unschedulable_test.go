package controller

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// unschedRG builds a RunnerGroup with the given pendingPodDeadline (controls the
// scheduling grace, which is half the deadline).
func unschedRG(ns, name string, deadline time.Duration) *v1alpha1.RunnerGroup {
	return &v1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       v1alpha1.RunnerGroupSpec{PendingPodDeadline: &metav1.Duration{Duration: deadline}},
	}
}

// pendingPod builds a Pending worker pod for rgName created at `created`, with an
// optional PodScheduled condition (status/reason).
func pendingPod(ns, rgName, name string, created time.Time, schedStatus corev1.ConditionStatus, schedReason, schedMsg string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			Labels:            map[string]string{provisioner.LabelRunnerGroup: rgName},
			CreationTimestamp: metav1.NewTime(created),
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	if schedReason != "" || schedStatus != "" {
		p.Status.Conditions = []corev1.PodCondition{{
			Type:    corev1.PodScheduled,
			Status:  schedStatus,
			Reason:  schedReason,
			Message: schedMsg,
		}}
	}
	return p
}

func unschedReconciler(t *testing.T, now time.Time, objs ...client.Object) *RunnerGroupReconciler {
	t.Helper()
	return &RunnerGroupReconciler{
		Client: fake.NewClientBuilder().WithScheme(wqScheme(t)).WithObjects(objs...).Build(),
		Now:    func() time.Time { return now },
	}
}

// TestEvalUnschedulable_NoPods: a group with no worker pods is schedulable.
func TestEvalUnschedulable_NoPods(t *testing.T) {
	now := time.Now()
	rg := unschedRG("ns", "rg", 10*time.Minute)
	r := unschedReconciler(t, now)
	st := r.evalWorkersUnschedulable(context.Background(), rg)
	assert.False(t, st.unschedulable)
	assert.Equal(t, v1alpha1.ReasonWorkersSchedulable, st.reason)
}

// TestEvalUnschedulable_PastGrace: a Pending+Unschedulable pod older than the
// grace (deadline/2 = 5m) trips the condition and names the pod.
func TestEvalUnschedulable_PastGrace(t *testing.T) {
	now := time.Now()
	rg := unschedRG("ns", "rg", 10*time.Minute) // grace 5m
	pod := pendingPod("ns", "rg", "worker-stuck", now.Add(-6*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "0/3 nodes are available: 3 node(s) had untolerated taint")
	r := unschedReconciler(t, now, pod)

	st := r.evalWorkersUnschedulable(context.Background(), rg)
	assert.True(t, st.unschedulable)
	assert.Equal(t, v1alpha1.ReasonPodsUnschedulable, st.reason)
	assert.Contains(t, st.message, "worker-stuck")
	assert.Contains(t, st.message, "untolerated taint")
}

// TestEvalUnschedulable_WithinGrace: an unschedulable pod younger than the grace
// does NOT trip the condition yet, and a re-check is scheduled at the crossing.
func TestEvalUnschedulable_WithinGrace(t *testing.T) {
	now := time.Now()
	rg := unschedRG("ns", "rg", 10*time.Minute) // grace 5m
	pod := pendingPod("ns", "rg", "worker-young", now.Add(-2*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "no nodes")
	r := unschedReconciler(t, now, pod)

	st := r.evalWorkersUnschedulable(context.Background(), rg)
	assert.False(t, st.unschedulable)
	assert.InDelta(t, (3 * time.Minute).Seconds(), st.requeueAfter.Seconds(), 1,
		"should re-check ~3m from now (5m grace − 2m age)")
}

// TestEvalUnschedulable_PendingButNotUnschedulable: a pod stuck Pending for a
// non-scheduling reason (e.g. image pull — no Unschedulable verdict) past the
// grace does NOT trip WorkersUnschedulable.
func TestEvalUnschedulable_PendingButNotUnschedulable(t *testing.T) {
	now := time.Now()
	rg := unschedRG("ns", "rg", 10*time.Minute)
	// PodScheduled=True (scheduled, but containers not yet running) — not a
	// scheduling failure.
	pod := pendingPod("ns", "rg", "worker-imgpull", now.Add(-6*time.Minute),
		corev1.ConditionTrue, "", "")
	r := unschedReconciler(t, now, pod)

	st := r.evalWorkersUnschedulable(context.Background(), rg)
	assert.False(t, st.unschedulable, "only a PodScheduled=Unschedulable verdict trips the condition")
}

// TestEvalUnschedulable_NotUnschedulableReason: PodScheduled=False with some other
// reason (not Unschedulable) must not trip the condition.
func TestEvalUnschedulable_NotUnschedulableReason(t *testing.T) {
	now := time.Now()
	rg := unschedRG("ns", "rg", 10*time.Minute)
	pod := pendingPod("ns", "rg", "worker-other", now.Add(-6*time.Minute),
		corev1.ConditionFalse, "SchedulerError", "transient")
	r := unschedReconciler(t, now, pod)

	st := r.evalWorkersUnschedulable(context.Background(), rg)
	assert.False(t, st.unschedulable)
}

// TestEvalUnschedulable_QuotaExhaustionDoesNotTrigger documents the Q157
// non-double-report property: ResourceQuota exhaustion blocks pod admission, so no
// worker pod is ever created. With no pods the condition stays False even though
// the WorkerQuota ladder would separately report the exhaustion.
func TestEvalUnschedulable_QuotaExhaustionDoesNotTrigger(t *testing.T) {
	now := time.Now()
	rg := unschedRG("ns", "rg", 10*time.Minute)
	// A namespace ResourceQuota that admits nothing — present in the cluster but,
	// crucially, no worker Pod object exists because admission rejected creation.
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "team-quota", Namespace: "ns"},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("0")},
			Used: corev1.ResourceList{corev1.ResourcePods: resource.MustParse("0")},
		},
	}
	r := unschedReconciler(t, now, quota)

	st := r.evalWorkersUnschedulable(context.Background(), rg)
	assert.False(t, st.unschedulable, "quota exhaustion must not surface as WorkersUnschedulable")
}

// TestEvalUnschedulable_RunningPodIgnored: a Running pod is never unschedulable.
func TestEvalUnschedulable_RunningPodIgnored(t *testing.T) {
	now := time.Now()
	rg := unschedRG("ns", "rg", 10*time.Minute)
	pod := pendingPod("ns", "rg", "worker-run", now.Add(-6*time.Minute),
		corev1.ConditionFalse, corev1.PodReasonUnschedulable, "stale verdict")
	pod.Status.Phase = corev1.PodRunning
	r := unschedReconciler(t, now, pod)

	st := r.evalWorkersUnschedulable(context.Background(), rg)
	assert.False(t, st.unschedulable)
}

// TestUnschedulableGrace_HalfDeadline verifies the grace is half the effective
// pending-pod deadline and never non-positive.
func TestUnschedulableGrace_HalfDeadline(t *testing.T) {
	assert.Equal(t, 5*time.Minute, unschedulableGrace(unschedRG("ns", "rg", 10*time.Minute)))
	// Default deadline (10m) when unset → 5m grace.
	assert.Equal(t, provisioner.DefaultPendingPodDeadline/2,
		unschedulableGrace(&v1alpha1.RunnerGroup{ObjectMeta: metav1.ObjectMeta{Name: "rg", Namespace: "ns"}}))
}
