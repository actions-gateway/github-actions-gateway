package controller

import (
	"context"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// Q1237: a failed job's worker snapshot carries its set's egress proxy readiness,
// read from the cache: a colocated pool's Ready condition and readyReplicas, and a
// shared pool's name alone, since its namespace is outside the cache.

func proxyObj(name, ns string, ready metav1.ConditionStatus, readyReplicas int32) *v2.EgressProxy {
	return &v2.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: v2.EgressProxyStatus{
			Conditions:    []metav1.Condition{{Type: v2.ConditionReady, Status: ready, Reason: "PodsUnavailable"}},
			ReadyReplicas: readyReplicas,
		},
	}
}

func TestRunnerSetTarget_EgressProxyReadiness(t *testing.T) {
	const ns = "team-a"
	withProxyRef := func(name, proxyNS string) func(*v2.RunnerSet) {
		return func(rs *v2.RunnerSet) {
			rs.Spec.ProxyRef = &v2.ProxyObjectRef{Name: name, Namespace: proxyNS}
		}
	}
	for _, tc := range []struct {
		name string
		objs []client.Object
		want *provisioner.ProxySnapshot
	}{
		{name: "a colocated proxyRef reads the pool's readiness",
			objs: []client.Object{rsObj("set", ns, withProxyRef("ep", "")), proxyObj("ep", ns, metav1.ConditionFalse, 0)},
			want: &provisioner.ProxySnapshot{Name: "ep", Mode: provisioner.ProxyModeColocated,
				Ready: "False", Reason: "PodsUnavailable", ReadyReplicas: ptr.To[int32](0)}},
		{name: "the gateway's defaultProxyRef is followed",
			objs: []client.Object{rsObj("set", ns, nil), gwObj("gw", ns, "ep"), proxyObj("ep", ns, metav1.ConditionTrue, 2)},
			want: &provisioner.ProxySnapshot{Name: "ep", Mode: provisioner.ProxyModeColocated,
				Ready: "True", Reason: "PodsUnavailable", ReadyReplicas: ptr.To[int32](2)}},
		{name: "a proxyRef naming its own namespace is colocated",
			objs: []client.Object{rsObj("set", ns, withProxyRef("ep", ns)), proxyObj("ep", ns, metav1.ConditionTrue, 1)},
			want: &provisioner.ProxySnapshot{Name: "ep", Mode: provisioner.ProxyModeColocated,
				Ready: "True", Reason: "PodsUnavailable", ReadyReplicas: ptr.To[int32](1)}},
		{name: "a pool in another namespace is shared, with no readiness",
			objs: []client.Object{rsObj("set", ns, withProxyRef("ep", "platform")), proxyObj("ep", "platform", metav1.ConditionFalse, 0)},
			want: &provisioner.ProxySnapshot{Name: "ep", Mode: provisioner.ProxyModeShared}},
		{name: "a colocated pool the cache does not hold has no readiness",
			objs: []client.Object{rsObj("set", ns, withProxyRef("ep", ""))},
			want: &provisioner.ProxySnapshot{Name: "ep", Mode: provisioner.ProxyModeColocated}},
		{name: "direct egress has no proxy",
			objs: []client.Object{rsObj("set", ns, nil), gwObj("gw", ns, "")}},
		{name: "an unreadable set has no proxy",
			objs: []client.Object{gwObj("gw", ns, "ep"), proxyObj("ep", ns, metav1.ConditionFalse, 0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := quotaTarget(t, ns, tc.objs...)
			assert.Equal(t, tc.want, target.EgressProxyReadiness(context.Background()))
		})
	}
}

// eventTarget records the reasons of the Events a Target is asked to record.
type eventTarget struct {
	*runnerSetTarget
	reasons []string
}

func (t *eventTarget) RecordEvent(_, reason, _, _ string) { t.reasons = append(t.reasons, reason) }

// TestWorkerPodHandler_SnapshotsTheSetsProxyReadiness is the watch half of Q1237: a
// worker's terminal transition, seen by the pod handler, snapshots its set's
// colocated proxy, so the failed job reads as a platform fault after the pod is gone.
func TestWorkerPodHandler_SnapshotsTheSetsProxyReadiness(t *testing.T) {
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "runner-set-abc", Namespace: "team-a",
			Labels: map[string]string{
				provisioner.LabelRunnerSet:           "set",
				provisioner.LabelAcquisitionProtocol: provisioner.AcquisitionProtocolScaleSet,
			},
			Annotations: map[string]string{provisioner.AnnotationRunnerName: "runner-1"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}}},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{Name: "runner",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}}}}},
	}
	r, q, _ := podWatchFixture(t, pod)
	require.NoError(t, r.Create(ctx, proxyObj("ep", "team-a", metav1.ConditionFalse, 0)))
	gw := &v2.ActionsGateway{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "gw"}, gw))
	gw.Spec.DefaultProxyRef = &v2.ProxyObjectRef{Name: "ep"}
	require.NoError(t, r.Update(ctx, gw))

	running := pod.DeepCopy()
	running.Status.Phase = corev1.PodRunning
	r.workerPodHandler().Update(ctx, event.UpdateEvent{ObjectOld: running, ObjectNew: pod}, q)
	require.NoError(t, r.Delete(ctx, pod))

	rs := &v2.RunnerSet{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "set"}, rs))
	target := &eventTarget{runnerSetTarget: r.provisionerTarget(rs)}
	require.NoError(t, r.Provisioner.RecordScaleSetJobCompleted(ctx, target,
		provisioner.ScaleSetJob{JobID: "job-1", RunnerName: "runner-1", RunID: "900", JobName: "build"}, "failed"))
	assert.Equal(t, []string{"PlatformFaultOnFailedJob"}, target.reasons,
		"the unready colocated proxy in the snapshot the handler took is a platform fault")
}
