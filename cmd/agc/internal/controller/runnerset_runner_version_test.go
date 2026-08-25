package controller

import (
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/api/apisidecar"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The AGC half of Q792: the wrapper's self-report reaches status, and the cases where
// it must NOT are pinned as hard as the case where it must. The field is a diagnostic
// for an operator debugging a custom image, and every way of turning tenant-controlled
// bytes into something that reads like a verdict is a defect.

// terminatedWorker builds a terminal worker pod whose runner container carries msg as
// its termination message, terminated at the given time.
func terminatedWorker(name, msg string, at time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: apisidecar.RunnerContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Message:    msg,
					FinishedAt: metav1.NewTime(at),
				}},
			}},
		},
	}
}

func TestWorkerReportVersion(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "a report is read",
			pod:  terminatedWorker("w", `{"runnerVersion":"2.335.1"}`, now),
			want: "2.335.1",
		},
		{
			name: "an empty message is not a version",
			pod:  terminatedWorker("w", "", now),
		},
		{
			name: "unparseable is the ordinary case, since the message is tenant-writable",
			pod:  terminatedWorker("w", "runner 2.335.1\nsome job output", now),
		},
		{
			name: "valid JSON with no version reports nothing",
			pod:  terminatedWorker("w", `{"somethingElse":"x"}`, now),
		},
		{
			name: "an explicitly empty version reports nothing",
			pod:  terminatedWorker("w", `{"runnerVersion":""}`, now),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := workerReportVersion(tt.pod)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.want != "", ok)
		})
	}
}

// TestWorkerReportVersionIgnoresSidecars pins which container is read. A worker pod
// may carry tenant sidecars and kubelet records a termination message for any of them
// that sets one, so taking the first terminated container would let a sidecar answer
// a question about the runner.
func TestWorkerReportVersionIgnoresSidecars(t *testing.T) {
	pod := terminatedWorker("w", `{"runnerVersion":"2.335.1"}`, time.Now())
	pod.Status.ContainerStatuses = append([]corev1.ContainerStatus{{
		Name: "tenant-sidecar",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Message: `{"runnerVersion":"9.9.9"}`,
		}},
	}}, pod.Status.ContainerStatuses...)

	got, ok := workerReportVersion(pod)

	assert.True(t, ok)
	assert.Equal(t, "2.335.1", got, "the runner container answers, not whichever container terminated first")
}

// TestObservedRunnerNewestWins pins the accumulator. A set whose workerImage changed
// has pods of both versions retained at once, and the current image is what an
// operator is asking about.
func TestObservedRunnerNewestWins(t *testing.T) {
	base := time.Now()
	var o observedRunner

	o.observe(terminatedWorker("older", `{"runnerVersion":"2.320.0"}`, base))
	o.observe(terminatedWorker("newer", `{"runnerVersion":"2.335.1"}`, base.Add(time.Minute)))
	o.observe(terminatedWorker("oldest", `{"runnerVersion":"2.300.0"}`, base.Add(-time.Hour)))

	assert.Equal(t, "2.335.1", o.version)
}

// TestObservedRunnerIgnoresUnreported keeps a pod with no usable report from clearing
// one already seen: the reap walk sees every terminal pod, and most of them will have
// nothing to say.
func TestObservedRunnerIgnoresUnreported(t *testing.T) {
	base := time.Now()
	var o observedRunner

	o.observe(terminatedWorker("good", `{"runnerVersion":"2.335.1"}`, base))
	o.observe(terminatedWorker("silent", "", base.Add(time.Hour)))
	o.observe(terminatedWorker("garbage", "not json", base.Add(2*time.Hour)))

	assert.Equal(t, "2.335.1", o.version, "a later pod with nothing to report must not erase a real observation")
}

// TestApplyObservedRunnerVersionIsSticky pins the field against reap-cycle flapping.
// Once every terminal pod has aged past completedPodTTL the walk sees no reports at
// all, and the answer is a property of the image the set runs rather than of which
// pods happen to be retained.
func TestApplyObservedRunnerVersionIsSticky(t *testing.T) {
	rs := &v2alpha1.RunnerSet{}
	rs.Status.ObservedRunnerVersion = "2.335.1"

	applyObservedRunnerVersion(rs, observedRunner{})

	assert.Equal(t, "2.335.1", rs.Status.ObservedRunnerVersion,
		"a pass that saw no reports must leave the last observation standing")
}

// TestApplyObservedRunnerVersionPublishes is the ordinary path.
func TestApplyObservedRunnerVersionPublishes(t *testing.T) {
	rs := &v2alpha1.RunnerSet{}

	applyObservedRunnerVersion(rs, observedRunner{version: "2.335.1", at: time.Now()})

	assert.Equal(t, "2.335.1", rs.Status.ObservedRunnerVersion)
}
