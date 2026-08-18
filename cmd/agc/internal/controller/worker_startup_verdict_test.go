package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// startupPod builds a worker pod in the shape the kubelet reports for a given phase
// and PodScheduled status, with no container statuses. Tests add those.
func startupPod(phase corev1.PodPhase, scheduled corev1.ConditionStatus) *corev1.Pod {
	return &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: phase,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: scheduled, Reason: schedReasonFor(scheduled)},
			},
		},
	}
}

func schedReasonFor(s corev1.ConditionStatus) string {
	if s == corev1.ConditionFalse {
		return corev1.PodReasonUnschedulable
	}
	return ""
}

func waiting(reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:  "runner",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: reason + " detail"}},
	}
}

// TestPodStartupBackoff is the whole matcher, in both directions. The reasons it
// accepts are a deliberately short list and the exclusions are load-bearing: this
// signal has no time-based grace, so what the matcher refuses to accept IS the grace
// (Q714).
func TestPodStartupBackoff(t *testing.T) {
	for _, tt := range []struct {
		name     string
		pod      *corev1.Pod
		want     bool
		wantWhy  string
		contains string
	}{
		{
			name: "bound and backing off on the image is the verdict",
			pod: func() *corev1.Pod {
				p := startupPod(corev1.PodPending, corev1.ConditionTrue)
				p.Status.ContainerStatuses = []corev1.ContainerStatus{waiting("ImagePullBackOff")}
				return p
			}(),
			want:     true,
			contains: "ImagePullBackOff detail",
		},
		{
			name: "an init container counts — a worker whose sidecar cannot pull never runs its job",
			pod: func() *corev1.Pod {
				p := startupPod(corev1.PodPending, corev1.ConditionTrue)
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{waiting("ImagePullBackOff")}
				return p
			}(),
			want:     true,
			contains: "ImagePullBackOff detail",
		},
		{
			name: "ErrImagePull is one failed pull, not the kubelet's conclusion",
			pod: func() *corev1.Pod {
				p := startupPod(corev1.PodPending, corev1.ConditionTrue)
				p.Status.ContainerStatuses = []corev1.ContainerStatus{waiting("ErrImagePull")}
				return p
			}(),
			want:    false,
			wantWhy: "excluding the pre-backoff failure is this signal's entire grace",
		},
		{
			name: "a per-pod config error is not evidence about the next pod's shape",
			pod: func() *corev1.Pod {
				p := startupPod(corev1.PodPending, corev1.ConditionTrue)
				p.Status.ContainerStatuses = []corev1.ContainerStatus{waiting("CreateContainerConfigError")}
				return p
			}(),
			want:    false,
			wantWhy: "the matcher covers the image-pull family only",
		},
		{
			name: "ContainerCreating is a pod on its way up",
			pod: func() *corev1.Pod {
				p := startupPod(corev1.PodPending, corev1.ConditionTrue)
				p.Status.ContainerStatuses = []corev1.ContainerStatus{waiting("ContainerCreating")}
				return p
			}(),
			want: false,
		},
		{
			name: "an unbound pod is the scheduler's business, never this signal's",
			pod: func() *corev1.Pod {
				p := startupPod(corev1.PodPending, corev1.ConditionFalse)
				p.Status.ContainerStatuses = []corev1.ContainerStatus{waiting("ImagePullBackOff")}
				return p
			}(),
			want:    false,
			wantWhy: "the two signals read opposite halves of PodScheduled and must stay disjoint",
		},
		{
			name: "a Running pod whose sidecar is restarting is a job in progress",
			pod: func() *corev1.Pod {
				p := startupPod(corev1.PodRunning, corev1.ConditionTrue)
				p.Status.ContainerStatuses = []corev1.ContainerStatus{waiting("ImagePullBackOff")}
				return p
			}(),
			want: false,
		},
		{
			name: "no container statuses at all",
			pod:  startupPod(corev1.PodPending, corev1.ConditionTrue),
			want: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, detail := podStartupBackoff(tt.pod)
			assert.Equal(t, tt.want, got, tt.wantWhy)
			if tt.contains != "" {
				assert.Contains(t, detail, tt.contains)
			}
		})
	}
}

// TestPodStartedAt: the latch's release evidence. A pod counts as started the moment
// any of its containers has run, whatever the pod's phase — a probe job that ran and
// finished is the strongest evidence the cluster can run a worker, and a pod whose
// init container is running has already demonstrated it while still Pending.
func TestPodStartedAt(t *testing.T) {
	base := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	for _, tt := range []struct {
		name string
		pod  *corev1.Pod
		want time.Time
		ok   bool
	}{
		{
			name: "a running container",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(base)}},
			}}}},
			want: base, ok: true,
		},
		{
			name: "a terminated container — a probe that ran and finished still counts",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{StartedAt: metav1.NewTime(base)}},
			}}}},
			want: base, ok: true,
		},
		{
			name: "a restarted container, read through lastState",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{StartedAt: metav1.NewTime(base)}},
			}}}},
			want: base, ok: true,
		},
		{
			name: "the earliest start wins — the question is when the pod stopped being an unresolved probe",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				InitContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{StartedAt: metav1.NewTime(base)}},
				}},
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(base.Add(time.Minute))}},
				}},
			}},
			want: base, ok: true,
		},
		{
			name: "a container that only ever waited never started",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				waiting("ImagePullBackOff"),
			}}},
			ok: false,
		},
		{
			name: "no statuses",
			pod:  &corev1.Pod{},
			ok:   false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			at, ok := podStartedAt(tt.pod)
			assert.Equal(t, tt.ok, ok)
			if tt.ok {
				assert.True(t, tt.want.Equal(at), "want %s, got %s", tt.want, at)
			}
		})
	}
}
