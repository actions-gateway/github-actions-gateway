package controller

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// told is pod with the out-of-memory reporter's condition on its runner.
func told(pod *corev1.Pod, reason string) *corev1.Pod {
	pod = pod.DeepCopy()
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type: "oom.actions-gateway.com/runner", Status: corev1.ConditionTrue, Reason: reason})
	return pod
}

func TestWorkerPodPredicate_AdmitsAReasonOnAFailedWorker(t *testing.T) {
	p := runnerSetWorkerPodPredicate()
	admitted := func(phase corev1.PodPhase, reason string, change func(*corev1.Pod)) bool {
		before := killedWorkerPod("ns", "set", time.Now())
		before.Status.Phase = phase
		change(before)
		return p.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: told(before, reason)})
	}
	asIs := func(*corev1.Pod) {}
	assert.True(t, admitted(corev1.PodFailed, "ContainerLimit", asIs))
	assert.True(t, admitted(corev1.PodFailed, "ContainerLimit", func(pod *corev1.Pod) {
		pod.Annotations[provisioner.AnnotationEvictionHandledAt] = "2026-11-25T00:00:00Z"
	}), "on a worker whose recovery is claimed")
	assert.False(t, admitted(corev1.PodFailed, "Anything", asIs), "a reason the record has no words for")
	assert.False(t, admitted(corev1.PodRunning, "ContainerLimit", asIs), "a running worker's is read as it fails")
	assert.False(t, admitted(corev1.PodFailed, "ContainerLimit", func(pod *corev1.Pod) {
		delete(pod.Labels, provisioner.LabelRunnerSet)
	}), "not a worker")
}

// noteTarget records the notes of the Events a Target is asked to record.
type noteTarget struct {
	*runnerSetTarget
	notes []string
}

func (t *noteTarget) RecordEvent(_, _, _, note string) { t.notes = append(t.notes, note) }

// TestWorkerPodHandler_AReasonReachesTheRecordAlone: the condition comes to a worker whose
// recovery has started. It starts none, queues nothing, and is in the failed job's Event
// after the pod is gone.
func TestWorkerPodHandler_AReasonReachesTheRecordAlone(t *testing.T) {
	ctx := context.Background()
	pod := drainedScaleSetWorkerPod("runner-set-abc", "team-a", "set")
	killed := killedWorkerPod("team-a", "set", time.Now())
	pod.Annotations[provisioner.AnnotationRunnerName] = "runner-1"
	pod.Status = killed.Status
	// A drained pod, so that a recovery started from this event would re-run its run.
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue,
		Reason: "EvictionByEvictionAPI", LastTransitionTime: metav1.Now()}}
	r, q, reruns := podWatchFixture(t, pod)
	r.Provisioner.SnapshotScaleSetWorker(ctx, nil, pod, false)

	e := event.UpdateEvent{ObjectOld: pod, ObjectNew: told(pod, "AbovePods")}
	require.True(t, runnerSetWorkerPodPredicate().Update(e))
	r.workerPodHandler().Update(ctx, e, q)
	assert.Zero(t, q.Len(), "no reconcile")
	assert.Never(t, func() bool { return reruns.Load() > 0 }, 300*time.Millisecond, 20*time.Millisecond, "no recovery")
	var got corev1.Pod
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(pod), &got))
	assert.NotContains(t, got.Annotations, provisioner.AnnotationEvictionHandledAt)

	rs := &v2.RunnerSet{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "set"}, rs))
	target := &noteTarget{runnerSetTarget: r.provisionerTarget(rs)}
	_, err := r.Provisioner.RecordScaleSetJobCompleted(ctx, target,
		provisioner.ScaleSetJob{JobID: "job-1", RunnerName: "runner-1", RunID: "900", JobName: "build"}, "failed")
	require.NoError(t, err)
	require.Len(t, target.notes, 1)
	assert.Contains(t, target.notes[0], "container runner OOMKilled (a pod condition says memory ran out above the pods")
}

// TestWorkerPodHandler_AReasonThatComesWithTheFailureTakesNothingFromIt: one update can
// carry both, and the failure still starts recovery and a reconcile.
func TestWorkerPodHandler_AReasonThatComesWithTheFailureTakesNothingFromIt(t *testing.T) {
	ctx := context.Background()
	pod := drainedScaleSetWorkerPod("runner-set-abc", "team-a", "set")
	r, q, reruns := podWatchFixture(t, pod)
	running := pod.DeepCopy()
	running.Status.Phase = corev1.PodRunning

	r.workerPodHandler().Update(ctx, event.UpdateEvent{ObjectOld: running, ObjectNew: told(pod, "AbovePods")}, q)
	assert.Equal(t, 1, q.Len())
	assert.Eventually(t, func() bool { return reruns.Load() == 1 }, 5*time.Second, 20*time.Millisecond)
}

// TestWorkerPodHandler_AReasonEndsTheWaitOfAKill: under Act the condition is what starts
// the recovery of a kill waiting for it, still with no reconcile; under Record it is not.
func TestWorkerPodHandler_AReasonEndsTheWaitOfAKill(t *testing.T) {
	for mode, recovers := range map[provisioner.OOMKillReasonsMode]bool{
		provisioner.OOMKillReasonsAct: true, provisioner.OOMKillReasonsRecord: false,
	} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := context.Background()
			pod := killedWorkerPod("team-a", "set", time.Now())
			pod.Annotations[provisioner.AnnotationStartedJobID] = "job-1"
			r, q, _ := podWatchFixture(t, pod)
			r.Provisioner.OOMKillReasons = mode

			r.workerPodHandler().Update(ctx, event.UpdateEvent{ObjectOld: pod, ObjectNew: told(pod, "AbovePods")}, q)
			assert.Zero(t, q.Len(), "no reconcile")
			claimed := func() bool {
				var got corev1.Pod
				require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(pod), &got))
				_, ok := got.Annotations[provisioner.AnnotationEvictionHandledAt]
				return ok
			}
			if recovers {
				assert.Eventually(t, claimed, 5*time.Second, 20*time.Millisecond)
			} else {
				assert.Never(t, claimed, 300*time.Millisecond, 20*time.Millisecond)
			}
		})
	}
}
