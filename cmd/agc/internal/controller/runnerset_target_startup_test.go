package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// startupWorker is a worker of set ns/set placed at placed, running since started where
// non-zero, else backing off on its runner image.
func startupWorker(name string, placed, started time.Time) *corev1.Pod {
	pod := startupPod(corev1.PodPending, corev1.ConditionTrue)
	pod.ObjectMeta = metav1.ObjectMeta{Namespace: "ns", Name: name, Labels: map[string]string{provisioner.LabelRunnerSet: "set"}}
	pod.Status.Conditions[0].LastTransitionTime = metav1.NewTime(placed)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{waiting("ImagePullBackOff")}
	if !started.IsZero() {
		pod.Status.Phase = corev1.PodRunning
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "runner",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(started)}}}}
	}
	return pod
}

func TestRunnerSetTarget_WorkersFailingToStart(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	at := func(minutes int) time.Time { return now.Add(time.Duration(minutes) * time.Minute) }
	sidecarUp := startupWorker("w1", at(-5), time.Time{})
	sidecarUp.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "dind",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(at(-4))}}}}
	unschedulable := startupPod(corev1.PodPending, corev1.ConditionFalse)
	unschedulable.ObjectMeta = metav1.ObjectMeta{Namespace: "ns", Name: "w1", Labels: map[string]string{provisioner.LabelRunnerSet: "set"}}
	unschedulable.CreationTimestamp = metav1.NewTime(at(-60))
	deleting := startupWorker("w1", at(-5), time.Time{})
	deleting.DeletionTimestamp, deleting.Finalizers = &metav1.Time{Time: at(-1)}, []string{"test/hold"}
	otherSet := startupWorker("w1", at(-5), time.Time{})
	otherSet.Labels[provisioner.LabelRunnerSet] = "other"

	for _, tc := range []struct {
		name string
		pods []client.Object
		want bool
	}{
		{"a placed worker cannot pull its image", []client.Object{startupWorker("w1", at(-5), time.Time{})}, true},
		{"workers that started before it was placed are still running",
			[]client.Object{startupWorker("w1", at(-5), time.Time{}), startupWorker("w2", at(-60), at(-59))}, true},
		{"its own sidecar started, its runner did not", []client.Object{sidecarUp}, true},
		{"a worker started since, so the failure is not every pod's",
			[]client.Object{startupWorker("w1", at(-5), time.Time{}), startupWorker("w2", at(-4), at(-3))}, false},
		{"a worker started since the oldest of two failures",
			[]client.Object{startupWorker("w1", at(-5), time.Time{}), startupWorker("w2", at(-4), at(-3)), startupWorker("w3", at(-2), time.Time{})}, false},
		{"an unschedulable worker is a full cluster, not a broken worker", []client.Object{unschedulable}, false},
		{"a worker being reaped", []client.Object{deleting}, false},
		{"another set's worker", []client.Object{otherSet}, false},
		{"no workers", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detail, failing := quotaTarget(t, "ns", tc.pods...).WorkersFailingToStart(context.Background())
			assert.Equal(t, tc.want, failing)
			if tc.want {
				assert.Contains(t, detail, "w1 (ImagePullBackOff detail)")
			}
		})
	}
}

func TestRunnerSetTarget_WorkersFailingToStart_AnUnreadableListIsNoVerdict(t *testing.T) {
	target := quotaTarget(t, "ns", startupWorker("w1", time.Now(), time.Time{}))
	target.client = interceptor.NewClient(target.client.(client.WithWatch), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("list failed")
		},
	})
	_, failing := target.WorkersFailingToStart(context.Background())
	assert.False(t, failing)
}
