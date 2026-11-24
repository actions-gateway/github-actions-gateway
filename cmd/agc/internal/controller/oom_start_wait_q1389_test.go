package controller

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// killedWorkerPod is a worker whose runner was killed for memory at ended with no job start
// recorded, as the out-of-memory class waits on it (Q1389).
func killedWorkerPod(ns, set string, ended time.Time) *corev1.Pod {
	pod := workerPod(ns, set, "killed", corev1.PodFailed, ended.Add(-time.Minute), ended)
	pod.Labels[provisioner.LabelAcquisitionProtocol] = provisioner.AcquisitionProtocolScaleSet
	pod.Annotations = map[string]string{
		provisioner.AnnotationRunnerName:    "gpu-1",
		provisioner.AnnotationSizingProfile: "Throughput",
	}
	pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}
	pod.Status.ContainerStatuses[0].ContainerID = "containerd://killed"
	pod.Status.ContainerStatuses[0].State.Terminated.Reason = "OOMKilled"
	return pod
}

// deletedFor marks a worker as one the AGC deletes for reason.
func deletedFor(reason string) func(*corev1.Pod) {
	return func(pod *corev1.Pod) { pod.Annotations[provisioner.AnnotationDeletionReason] = reason }
}

func TestWorkerPodPredicate_AdmitsAStartOnAFailedWorker(t *testing.T) {
	p := workerPodPhaseChangePredicate(provisioner.LabelRunnerSet)
	admitted := func(phase corev1.PodPhase, change func(*corev1.Pod)) bool {
		before := killedWorkerPod("ns", "set", time.Now())
		before.Status.Phase = phase
		after := before.DeepCopy()
		change(after)
		return p.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after})
	}
	start := func(pod *corev1.Pod) { pod.Annotations[provisioner.AnnotationStartedJobID] = "job" }
	assert.True(t, admitted(corev1.PodFailed, start), "the start that ends a kill's wait")
	assert.False(t, admitted(corev1.PodRunning, start), "a running worker's start adds no reconcile")
	assert.False(t, admitted(corev1.PodFailed, func(pod *corev1.Pod) {
		pod.Annotations[provisioner.AnnotationJobCompletedAt] = "2026-11-24T00:00:00Z"
	}), "any other annotation")
}

// TestReaper_KeepsAKillAwaitingItsStart: a waiting kill outlasts any completedPodTTL, and
// the pass that keeps it schedules the next one at the end of the wait.
func TestReaper_KeepsAKillAwaitingItsStart(t *testing.T) {
	now := time.Now().Add(time.Hour).Truncate(time.Second)
	for _, tc := range []struct {
		name   string
		ttl    *metav1.Duration
		ended  time.Duration
		change func(*corev1.Pod)
		kept   bool
		next   time.Duration
	}{
		{"TTL of zero", &metav1.Duration{}, time.Minute, nil, true, 14 * time.Minute},
		{"default TTL, past it", nil, 10 * time.Minute, nil, true, 5 * time.Minute},
		{"a TTL longer than the hold", &metav1.Duration{Duration: time.Hour}, time.Minute, nil, true, 14 * time.Minute},
		{"wait over and not given up", &metav1.Duration{}, 15 * time.Minute, nil, true, 30 * time.Second},
		{"hold ending, a longer TTL", &metav1.Duration{Duration: time.Hour}, 19*time.Minute + 50*time.Second, nil, true, 30 * time.Second},
		{"hold over", &metav1.Duration{}, 20 * time.Minute, nil, false, 0},
		{"hold over, a longer TTL", &metav1.Duration{Duration: time.Hour}, 25 * time.Minute, nil, true, 35 * time.Minute},
		{"stamped and not claimed", &metav1.Duration{}, time.Minute, func(p *corev1.Pod) {
			p.Annotations[provisioner.AnnotationStartedJobID] = "job"
		}, true, 30 * time.Second},
		{"handled", &metav1.Duration{}, time.Minute, func(p *corev1.Pod) {
			p.Annotations[provisioner.AnnotationEvictionHandledAt] = "2026-11-24T00:00:00Z"
		}, false, 0},
		{"failed on its own", &metav1.Duration{}, time.Minute, func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.Reason = "Error"
		}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := rsObj("set", "default", func(rs *v2.RunnerSet) { rs.Spec.CompletedPodTTL = tc.ttl })
			pod := killedWorkerPod("default", "set", now.Add(-tc.ended))
			if tc.change != nil {
				tc.change(pod)
			}
			fb := fake.NewClientBuilder().WithScheme(runnerSetTestScheme(t)).WithObjects(rs).Build()
			prov := provisioner.NewProvisioner(fb, nil, slog.Default())
			r := &RunnerSetReconciler{Client: fb, Log: slog.Default(), Now: func() time.Time { return now },
				Provisioner: prov, Metrics: reapTestMetrics(), Recorder: events.NewFakeRecorder(4)}
			// The set's first scan, at the real clock, came before the kill.
			_, err := prov.RecoverEvictedScaleSetWorkers(context.Background(), r.provisionerTarget(rs))
			require.NoError(t, err)
			require.NoError(t, fb.Create(context.Background(), pod))

			next, _, err := r.reapWorkerPods(context.Background(), slog.Default(), rs, &observedRunner{})
			require.NoError(t, err)
			err = fb.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{})
			assert.Equal(t, tc.kept, err == nil)
			assert.Equal(t, !tc.kept, apierrors.IsNotFound(err))
			assert.Equal(t, tc.next, next)
		})
	}
}

// TestWorkerPodDelete_SettlesAKillAwaitingItsStart drives the watch's delete: the pod's
// kill can no longer be matched to a job, unless the set or its gateway went with it.
func TestWorkerPodDelete_SettlesAKillAwaitingItsStart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*corev1.Pod)
		set     func(*v2.RunnerSet)
		noSet   bool
		reports bool
	}{
		{"deleted by someone", nil, nil, false, true},
		{"reaped unclaimed", deletedFor(reapReasonCompletedTTL), nil, false, true},
		{"deleted with its gateway", deletedFor(reapReasonGatewayDeleted), nil, false, false},
		{"its set is gone", nil, nil, true, false},
		{"its set is being deleted", nil, func(rs *v2.RunnerSet) {
			rs.Finalizers = []string{"test.actions-gateway.com/hold"}
			rs.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runnerSetTestScheme(t)
			b := fake.NewClientBuilder().WithScheme(scheme)
			if !tc.noSet {
				b = b.WithObjects(rsObj("set", "default", tc.set))
			}
			fb := b.Build()
			recorded := make(chan eventRecord, 4)
			r := &RunnerSetReconciler{Client: fb, Log: slog.Default(), eventCh: recorded,
				Provisioner: provisioner.NewProvisioner(fb, nil, slog.Default())}
			pod := killedWorkerPod("default", "set", time.Now())
			if tc.change != nil {
				tc.change(pod)
			}
			q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
			t.Cleanup(q.ShutDown)
			r.workerPodHandler().Delete(context.Background(), event.DeleteEvent{Object: pod}, q)
			if !tc.reports {
				assert.Never(t, func() bool { return len(recorded) > 0 }, 300*time.Millisecond, 20*time.Millisecond)
				return
			}
			require.Eventually(t, func() bool { return len(recorded) == 1 }, 5*time.Second, 10*time.Millisecond)
			assert.Equal(t, "EvictionRecoveryIdentityUnknown", (<-recorded).reason)
		})
	}
}
