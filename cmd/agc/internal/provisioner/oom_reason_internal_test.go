package provisioner

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Q1403: the out-of-memory reporter's answer in a failed job's record and Event, and the
// limits on repeating it (docs/design/04-operational-flows.md §4.2).

// oomWorker is a Failed worker whose runner the kubelet reports killed for memory.
func oomWorker() *corev1.Pod {
	pod := scaleSetWorker("runner-1", corev1.PodFailed, "")
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{oomContainer(runnerContainer, "containerd://1")}
	return pod
}

func oomContainer(name, id string) corev1.ContainerStatus {
	cs := terminated(name, "OOMKilled", 137, time.Now().Truncate(time.Second))
	cs.State.Terminated.ContainerID = id
	return cs
}

// saying is pod with the reporter's condition for container, in place of one it had.
func saying(pod *corev1.Pod, container, reason string) *corev1.Pod {
	status := corev1.ConditionTrue
	if reason == "Unknown" {
		status = corev1.ConditionUnknown
	}
	return withCondition(pod, "oom.actions-gateway.com/"+container, reason, status)
}

func withCondition(pod *corev1.Pod, typ, reason string, status corev1.ConditionStatus) *corev1.Pod {
	pod = pod.DeepCopy()
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodConditionType(typ), Status: status, Reason: reason,
		Message: "words of whoever wrote it", LastTransitionTime: metav1.Now()}}
	return pod
}

// failedWith records the failed job of a worker whose snapshot was taken as the pod stood
// at each of reads, the first as it failed, and returns its Event and its recorded reasons.
func failedWith(t *testing.T, reads ...*corev1.Pod) (event string, reasons map[string]string) {
	t.Helper()
	ctx := context.Background()
	_, p, target := flakyFixture(t)
	p.SnapshotScaleSetWorker(ctx, nil, reads[0], false)
	for _, pod := range reads[1:] {
		p.AddOOMReasons(pod)
	}
	completeJob(ctx, t, p, target, buildJob(1), "failed")
	require.LessOrEqual(t, len(target.messages), 1)
	if len(target.messages) == 1 {
		event = target.messages[0]
	}
	return event, recordedReasons(t, p, target)
}

func recordedReasons(t *testing.T, p *Provisioner, target Target) map[string]string {
	t.Helper()
	var snap workerSnapshot
	require.NoError(t, json.Unmarshal(readFailures(t, p, target)["900/build"].Context, &snap))
	reasons := map[string]string{}
	for _, c := range snap.Containers {
		if c.OOMReason != "" {
			reasons[c.Name] = c.OOMReason
		}
	}
	return reasons
}

const killedWithNoReason = `Job "build" of run 900 failed on worker worker-runner-1, which shows: pod Failed; container runner OOMKilled.`

// TestOOMReason_IsSaidBesideTheKill: the condition comes after the snapshot, and the pod is
// gone before the job is red.
func TestOOMReason_IsSaidBesideTheKill(t *testing.T) {
	for reason, words := range map[string]string{
		"ContainerLimit": "its own memory limit ran out, or those of its pod's containers together",
		"PodLimit":       "the memory limit in its pod's spec.resources ran out",
		"PodsLimit":      "the memory limit that the node's pods share ran out, or that of those of one QoS class",
		"AbovePods":      "memory ran out above the pods, which on an ordinary node is the node's",
		"Unknown":        "which limit ran out is not known",
	} {
		t.Run(reason, func(t *testing.T) {
			pod := oomWorker()
			want := strings.TrimSuffix(killedWithNoReason, ".") + " (a pod condition says " + words + ")."
			for name, reads := range map[string][]*corev1.Pod{
				"after the snapshot": {pod, saying(pod, runnerContainer, reason)},
				"at the snapshot":    {saying(pod, runnerContainer, reason)},
			} {
				event, reasons := failedWith(t, reads...)
				assert.Equal(t, want, event, name)
				assert.Equal(t, map[string]string{runnerContainer: reason}, reasons, name)
			}
		})
	}
}

// TestOOMReason_IsSaidOfTheContainerItsTypeNames: the sidecar's answer is not the runner's.
func TestOOMReason_IsSaidOfTheContainerItsTypeNames(t *testing.T) {
	pod := oomWorker()
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{oomContainer("dind", "containerd://2")}
	event, reasons := failedWith(t, pod, saying(pod, "dind", "PodLimit"))
	assert.Contains(t, event, "container dind OOMKilled (a pod condition says the memory limit in its pod's spec.resources ran out); container runner OOMKilled.")
	assert.Equal(t, map[string]string{"dind": "PodLimit"}, reasons)
}

// TestOOMReason_AddsNothingButWordsToAKill is the first three of the design's limits: the
// fault and its Event are the kubelet's, a condition's own text is never repeated, and a
// container that restarted is recorded as without a condition.
func TestOOMReason_AddsNothingButWordsToAKill(t *testing.T) {
	const runnerCondition = "oom.actions-gateway.com/runner"
	sidecar := func(change func(*corev1.ContainerStatus)) *corev1.Pod {
		pod := scaleSetWorker("runner-1", corev1.PodFailed, "")
		dind := oomContainer("dind", "containerd://2")
		change(&dind)
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{dind}
		return pod
	}
	exited := oomWorker()
	exited.Status.ContainerStatuses[0].State.Terminated.Reason = "Error"
	green := scaleSetWorker("runner-1", corev1.PodSucceeded, "")
	green.Status.ContainerStatuses = []corev1.ContainerStatus{terminated(runnerContainer, "Completed", 0, time.Now())}
	const podFailed = `Job "build" of run 900 failed on worker worker-runner-1, which shows: pod Failed`
	for _, tc := range []struct {
		name  string
		pod   *corev1.Pod
		said  *corev1.Pod
		event string
	}{
		{"a runner the kubelet does not report killed", exited, saying(exited, runnerContainer, "ContainerLimit"),
			podFailed + "; container runner exited 137."},
		{"a healthy worker", green, saying(green, runnerContainer, "ContainerLimit"), ""},
		{"a container the pod has not", oomWorker(), saying(oomWorker(), "other", "ContainerLimit"), killedWithNoReason},
		{"a reason with no words", oomWorker(), withCondition(oomWorker(), runnerCondition, "Anything", corev1.ConditionTrue), killedWithNoReason},
		{"a log reason, which is PR 9's", oomWorker(), withCondition(oomWorker(), runnerCondition, "LogPodLimit", corev1.ConditionTrue), killedWithNoReason},
		{"a condition that is False", oomWorker(), withCondition(oomWorker(), runnerCondition, "ContainerLimit", corev1.ConditionFalse), killedWithNoReason},
		{"a condition of another domain", oomWorker(), withCondition(oomWorker(), "oom.example.com/runner", "ContainerLimit", corev1.ConditionTrue), killedWithNoReason},
		{"a sidecar that survived a nested kill", sidecar(func(cs *corev1.ContainerStatus) { cs.State.Terminated.ExitCode = 143 }),
			saying(sidecar(func(cs *corev1.ContainerStatus) { cs.State.Terminated.ExitCode = 143 }), "dind", "ContainerLimit"), podFailed + "."},
		{"a sidecar that restarted", sidecar(func(cs *corev1.ContainerStatus) { cs.RestartCount = 1 }),
			saying(sidecar(func(cs *corev1.ContainerStatus) { cs.RestartCount = 1 }), "dind", "ContainerLimit"), podFailed + "; container dind OOMKilled."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, reads := range map[string][]*corev1.Pod{
				"after the snapshot": {tc.pod, tc.said}, "at the snapshot": {tc.said},
			} {
				event, reasons := failedWith(t, reads...)
				assert.Equal(t, tc.event, event, name)
				assert.Empty(t, reasons, name)
			}
		})
	}
}

// TestOOMReason_TheFirstForAContainerStands: a condition that flaps changes nothing after
// the first reason, which only an unknown one gives way.
func TestOOMReason_TheFirstForAContainerStands(t *testing.T) {
	for _, tc := range []struct {
		name string
		said []string
		want string
	}{
		{"one reason then another", []string{"AbovePods", "ContainerLimit"}, "AbovePods"},
		{"a reason then Unknown", []string{"PodLimit", "Unknown"}, "PodLimit"},
		{"Unknown then a reason", []string{"Unknown", "PodsLimit"}, "PodsLimit"},
		{"Unknown, a reason, another", []string{"Unknown", "PodsLimit", "AbovePods"}, "PodsLimit"},
		{"Unknown twice", []string{"Unknown", "Unknown"}, "Unknown"},
		{"Unknown, then no condition", []string{"Unknown", ""}, "Unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := oomWorker()
			reads := []*corev1.Pod{pod}
			for _, reason := range tc.said {
				read := pod
				if reason != "" {
					read = saying(pod, runnerContainer, reason)
				}
				reads = append(reads, read)
			}
			_, reasons := failedWith(t, reads...)
			assert.Equal(t, tc.want, reasons[runnerContainer], "added to one snapshot")

			// A start recorded on the Failed worker snapshots it again.
			ctx := context.Background()
			_, p, target := flakyFixture(t)
			for _, read := range reads {
				p.SnapshotScaleSetWorker(ctx, nil, read, false)
			}
			completeJob(ctx, t, p, target, buildJob(1), "failed")
			assert.Equal(t, tc.want, recordedReasons(t, p, target)[runnerContainer], "over a snapshot each")
		})
	}
}

// TestOOMReason_IsOfOneContainerID: a reason read of another container is no answer.
func TestOOMReason_IsOfOneContainerID(t *testing.T) {
	pod, other := oomWorker(), oomWorker()
	other.Status.ContainerStatuses[0].State.Terminated.ContainerID = "containerd://other"
	_, reasons := failedWith(t, pod, saying(other, runnerContainer, "AbovePods"), saying(pod, runnerContainer, "PodLimit"))
	assert.Equal(t, map[string]string{runnerContainer: "PodLimit"}, reasons, "another ID's reason is not added, and is not the first")

	other = oomWorker()
	other.Name = "worker-of-the-same-runner-name"
	_, reasons = failedWith(t, pod, saying(other, runnerContainer, "AbovePods"))
	assert.Empty(t, reasons, "nor another pod's")

	pod.Status.ContainerStatuses[0].State.Terminated.ContainerID = ""
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{oomContainer("dind", "")}
	_, reasons = failedWith(t, pod, saying(pod, runnerContainer, "PodLimit"))
	assert.Equal(t, map[string]string{runnerContainer: "PodLimit"}, reasons, "nor, where no ID was reported, another container's")
}

// duringCompletion is a target that calls do while a completion reads its worker.
type duringCompletion struct {
	*stubTarget
	do func()
}

func (d duringCompletion) EgressProxyReadiness(context.Context) *ProxySnapshot {
	d.do()
	return nil
}

// TestOOMReason_IsCarriedOverAWorkersReads: a sidecar killed while the runner works on is
// read more than once, and the job may fail between.
func TestOOMReason_IsCarriedOverAWorkersReads(t *testing.T) {
	ctx := context.Background()
	pod := scaleSetWorker("runner-1", corev1.PodRunning, "")
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{oomContainer("dind", "containerd://2")}
	pod = saying(pod, "dind", "PodLimit")
	finishedAs := func(change func(*corev1.Pod)) *corev1.Pod {
		finished := pod.DeepCopy()
		finished.Status.Phase = corev1.PodFailed
		change(finished)
		return finished
	}
	dind := func(pod *corev1.Pod) *corev1.ContainerStatus { return &pod.Status.InitContainerStatuses[0] }
	for _, tc := range []struct {
		name   string
		change func(*corev1.Pod)
		want   map[string]string
	}{
		{"another reason by its end", func(pod *corev1.Pod) { *pod = *saying(pod, "dind", "AbovePods") }, map[string]string{"dind": "PodLimit"}},
		{"its status dropped by then", func(pod *corev1.Pod) {
			dind(pod).State, pod.Status.Conditions = corev1.ContainerState{}, nil
		}, map[string]string{"dind": "PodLimit"}},
		{"restarted by then", func(pod *corev1.Pod) {
			dind(pod).RestartCount, dind(pod).LastTerminationState = 1, dind(pod).State
			dind(pod).State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		}, map[string]string{}},
	} {
		for _, when := range []string{"after the completion", "during it"} {
			t.Run(tc.name+" "+when, func(t *testing.T) {
				c, p, stub := flakyFixture(t)
				require.NoError(t, c.Create(ctx, pod.DeepCopy()))
				end := func() { <-p.SnapshotScaleSetWorker(ctx, nil, finishedAs(tc.change), false) }
				if when == "during it" {
					completeJob(ctx, t, p, duringCompletion{stub, end}, buildJob(1), "failed")
				} else {
					completeJob(ctx, t, p, stub, buildJob(1), "failed")
					end()
				}
				require.Empty(t, p.workerSnapshots.recorded)
				assert.Equal(t, tc.want, recordedReasons(t, p, stub))
			})
		}
	}
}

// TestOOMReason_IsNoPartOfTheFault: the job fails while its worker's pod still runs, and
// the condition comes with the pod's end. The kill was reported at the completion.
func TestOOMReason_IsNoPartOfTheFault(t *testing.T) {
	ctx := context.Background()
	c, p, target := flakyFixture(t)
	running := oomWorker()
	running.Status.Phase = corev1.PodRunning
	require.NoError(t, c.Create(ctx, running))
	completeJob(ctx, t, p, target, buildJob(1), "failed")
	require.Equal(t, []string{"PlatformFaultOnFailedJob"}, target.events)

	finished := saying(running, runnerContainer, "ContainerLimit")
	finished.Status.Phase = corev1.PodFailed
	<-p.SnapshotScaleSetWorker(ctx, nil, finished, false)
	assert.Equal(t, []string{"PlatformFaultOnFailedJob"}, target.events, "the kill is reported once")
	assert.Equal(t, map[string]string{runnerContainer: "ContainerLimit"}, recordedReasons(t, p, target))
}

// TestOOMReason_OnlyWhileTheSnapshotIsHeld: what comes after the completion is not recorded,
// and a snapshot being encoded is not written to, which -race reads.
func TestOOMReason_OnlyWhileTheSnapshotIsHeld(t *testing.T) {
	ctx := context.Background()
	pod := oomWorker()
	said := saying(pod, runnerContainer, "ContainerLimit")

	_, p, target := flakyFixture(t)
	p.AddOOMReasons(said)
	assert.Empty(t, p.workerSnapshots.byID, "a condition makes no snapshot")
	p.SnapshotScaleSetWorker(ctx, nil, pod, false)
	completeJob(ctx, t, p, target, buildJob(1), "failed")
	p.AddOOMReasons(said)
	assert.Equal(t, killedWithNoReason, strings.Join(target.messages, ""))
	assert.Empty(t, recordedReasons(t, p, target))
	assert.Empty(t, p.workerSnapshots.byID)

	for range 50 {
		p.SnapshotScaleSetWorker(ctx, nil, pod, false)
		var wg sync.WaitGroup
		wg.Go(func() { p.AddOOMReasons(said) })
		s := p.workerSnapshots.take("team-a/runner-1")
		_, err := json.Marshal(s)
		require.NoError(t, err)
		wg.Wait()
	}
}

// TestOOMReason_KeepsTheSnapshotItAddsTo: the egress proxy was read as the worker failed,
// and the fault the job met is that reading's.
func TestOOMReason_KeepsTheSnapshotItAddsTo(t *testing.T) {
	ctx := context.Background()
	_, p, _ := flakyFixture(t)
	proxy := &ProxySnapshot{Name: "ep", Mode: ProxyModeColocated, Ready: "True"}
	p.SnapshotScaleSetWorker(ctx, staticProxy{proxy}, oomWorker(), false)
	later := oomWorker()
	later.Status.Reason = "Later"
	p.AddOOMReasons(saying(later, runnerContainer, "PodLimit"))
	s := p.workerSnapshots.take("team-a/runner-1")
	require.NotNil(t, s)
	assert.Equal(t, proxy, s.Proxy)
	assert.Empty(t, s.Reason)
	assert.Equal(t, "PodLimit", s.Containers[0].OOMReason)
}

// TestOOMReason_GivesWayToTheEventsLength: the API server refuses a note over 1024
// characters, so one that the words make too long goes without them.
func TestOOMReason_GivesWayToTheEventsLength(t *testing.T) {
	long := strings.Repeat("p", 57)
	for reason := range oomReasonWords {
		s := &workerSnapshot{Pod: long, Containers: []containerSnapshot{
			{Name: runnerContainer, OOMReason: reason}, {Name: strings.Repeat("s", 63), OOMReason: reason}}}
		_, p, target := flakyFixture(t)
		faults := []string{"pod Failed", s.Containers[0].oomFault(), s.Containers[1].oomFault()}
		p.reportFailedJob(target, "900", strings.Repeat("j", 255), "", s, faults, p.logForKey(target.key))
		require.Len(t, target.messages, 1)
		assert.LessOrEqual(t, len(target.messages[0]), 1024)
		assert.Contains(t, target.messages[0], "a pod condition says", "two kills' words fit")

		p.reportFailedJob(target, "900", strings.Repeat("j", 780), "", s, faults, p.logForKey(target.key))
		require.Len(t, target.messages, 2)
		assert.LessOrEqual(t, len(target.messages[1]), 1024)
		assert.NotContains(t, target.messages[1], "a pod condition says")
		assert.Contains(t, target.messages[1], "container runner OOMKilled")
	}
}

func TestOOMReasonAppeared(t *testing.T) {
	pod := oomWorker()
	running := oomWorker()
	running.Status.Phase = corev1.PodRunning
	claimed := oomWorker()
	claimed.Annotations[AnnotationEvictionHandledAt] = "2026-11-25T00:00:00Z"
	for _, tc := range []struct {
		name          string
		before, after *corev1.Pod
		want          bool
	}{
		{"on a Failed worker", pod, saying(pod, runnerContainer, "ContainerLimit"), true},
		{"on one whose recovery is claimed", claimed, saying(claimed, runnerContainer, "ContainerLimit"), true},
		{"with another reason", saying(pod, runnerContainer, "Unknown"), saying(pod, runnerContainer, "PodLimit"), true},
		{"as it was", saying(pod, runnerContainer, "PodLimit"), saying(pod, runnerContainer, "PodLimit"), false},
		{"gone", saying(pod, runnerContainer, "PodLimit"), pod, false},
		{"on a running worker", running, saying(running, runnerContainer, "ContainerLimit"), false},
		{"with no words", pod, saying(pod, runnerContainer, "Anything"), false},
		{"of another domain", pod, withCondition(pod, "Ready", "ContainerLimit", corev1.ConditionTrue), false},
	} {
		assert.Equal(t, tc.want, OOMReasonAppeared(tc.before, tc.after), tc.name)
	}
}
