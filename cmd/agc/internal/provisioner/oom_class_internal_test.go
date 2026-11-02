package provisioner

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// oomKilled marks pod as the kubelet does when its runner container is killed at its
// memory limit, which with restartPolicy Never fails the pod. The container ran, so it
// carries an ID and start time and podEverStarted reads it as started.
func oomKilled(pod *corev1.Pod, limit string) *corev1.Pod {
	pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(limit)}
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:        runnerContainer,
		ContainerID: "containerd://" + pod.Name,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: containerReasonOOMKilled, ExitCode: 137, StartedAt: metav1.NewTime(time.Unix(1700000000, 0)),
		}},
	}}
	return pod
}

// rightSizedAnnotations is a worker a Throughput profile built, for run 4242's job
// "build".
func rightSizedAnnotations(runID string) map[string]string {
	a := identityAnnotations()
	a[AnnotationRunID] = runID
	a[annotationJobName] = "build"
	a[AnnotationSizingProfile] = "Throughput"
	return a
}

func throughputRetry() *MemoryRetryPolicy { return &MemoryRetryPolicy{HeadroomPercent: 150} }

func TestNextMemoryLimit(t *testing.T) {
	cases := []struct {
		name, killed, want string
		policy             MemoryRetryPolicy
	}{
		{"Throughput default headroom", "2Gi", "3Gi", MemoryRetryPolicy{HeadroomPercent: 150}},
		{"Binpack headroom, rounded up to 64Mi", "3Gi", "4352Mi", MemoryRetryPolicy{HeadroomPercent: 140}},
		{"maxRequests caps it", "3Gi", "4Gi", MemoryRetryPolicy{HeadroomPercent: 150, Ceiling: resource.MustParse("4Gi")}},
		{"no ceiling caps at twice the killed limit", "1Gi", "2Gi", MemoryRetryPolicy{HeadroomPercent: 1000}},
		{"a limit at the ceiling cannot grow", "4Gi", "4Gi", MemoryRetryPolicy{HeadroomPercent: 150, Ceiling: resource.MustParse("4Gi")}},
		{"100% headroom does not grow", "2Gi", "2Gi", MemoryRetryPolicy{HeadroomPercent: 100}},
		{"a ceiling lowered below the killed limit", "4Gi", "3Gi", MemoryRetryPolicy{HeadroomPercent: 150, Ceiling: resource.MustParse("3Gi")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextMemoryLimit(resource.MustParse(tc.killed), tc.policy)
			assert.Zero(t, got.Cmp(resource.MustParse(tc.want)), "got %s, want %s", got.String(), tc.want)
		})
	}
}

func TestOOMKilledAwaitingRecovery(t *testing.T) {
	pod := func() *corev1.Pod { return oomKilled(scaleSetWorkerPod("w", map[string]string{}), "2Gi") }
	cases := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"runner killed for memory", pod(), true},
		{"already handled", func() *corev1.Pod {
			p := pod()
			p.Annotations[AnnotationEvictionHandledAt] = "2026-11-01T00:00:00Z"
			return p
		}(), false},
		{"not failed", func() *corev1.Pod { p := pod(); p.Status.Phase = corev1.PodRunning; return p }(), false},
		{"runner failed for another reason", func() *corev1.Pod {
			p := pod()
			p.Status.ContainerStatuses[0].State.Terminated.Reason = "Error"
			return p
		}(), false},
		// A sidecar killed for memory leaves the runner to report the job's own result,
		// so the pod's terminal transition says nothing about it.
		{"only a sidecar killed for memory", func() *corev1.Pod {
			p := pod()
			p.Status.ContainerStatuses[0].State.Terminated.Reason = "Completed"
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "dind",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: containerReasonOOMKilled}}}}
			return p
		}(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, oomKilledAwaitingRecovery(tc.pod))
		})
	}
}

// TestRecoverOOMKilled_RaisesTheLimitAndReruns is the class end to end: the runner of a
// right-sized worker is killed at its limit, the job's next attempt is given a strictly
// higher one in the ledger, and the run is re-run once on the scale-set budget under
// cause oom.
func TestRecoverOOMKilled_RaisesTheLimitAndReruns(t *testing.T) {
	ctx := context.Background()
	pod := oomKilled(scaleSetWorkerPod("runner-gpu-job1", rightSizedAnnotations("4242")), "2Gi")
	require.True(t, podEverStarted(pod), "the fixture must model a runner that ran, not the never-started arm's shape")
	p, target, m, rerunCount, paths := recoveryFixture(t, pod)
	target.spec.MemoryRetry = throughputRetry()

	for i := 0; i < 2; i++ {
		done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
		require.NoError(t, err)
		<-done
	}
	require.Zero(t, rerunCount.Load(), "the re-run waits in the ledger for the pending sweep")
	sweepPendingAndWait(t, p, ctx, 1)

	require.Equal(t, int64(1), rerunCount.Load(), "the killed run must be re-run exactly once")
	assert.Equal(t, "/repos/myorg/myrepo/actions/runs/4242/rerun-failed-jobs", <-paths)
	assert.Equal(t, float64(1), testutil.ToFloat64(
		m.EvictionRetries.WithLabelValues("team-a", "gpu", evictionTierScaleSet, recoveryCauseOOM)))
	assert.Contains(t, target.events, "MemoryLimitRaisedForRetry")

	l, _, err := p.readRerunLedger(ctx, target)
	require.NoError(t, err)
	f := l.MemoryFloors[failureKey("4242", "build")]
	require.NotNil(t, f, "the raised limit must be recorded for the job's next attempt")
	assert.Equal(t, "3Gi", f.Limit.String())
	assert.Equal(t, "2Gi", f.KilledLimit.String())
	assert.Equal(t, runnerContainer, f.Container)
}

// TestRecoverOOMKilled_NoBudgetPromisesNoRerun: a set with no re-run budget reports
// the run exhausted, never a raised limit "re-run automatically", and the breaker
// counts it not.
func TestRecoverOOMKilled_NoBudgetPromisesNoRerun(t *testing.T) {
	ctx := context.Background()
	p, target, m, rerunCount, _ := recoveryFixture(t,
		oomKilled(scaleSetWorkerPod("runner-gpu-job1", rightSizedAnnotations("4242")), "2Gi"))
	target.spec.MemoryRetry = throughputRetry()
	target.spec.MaxEvictionRetries = 0

	done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
	require.NoError(t, err)
	<-done

	assert.Zero(t, rerunCount.Load())
	assert.Equal(t, float64(1), testutil.ToFloat64(
		m.EvictionRetriesExhausted.WithLabelValues("team-a", "gpu", evictionTierScaleSet, recoveryCauseOOM)))
	assert.NotContains(t, target.events, "MemoryLimitRaisedForRetry")
	assert.Nil(t, p.classBreakers.byKey[classBreakerKey{set: target.Key(), cause: recoveryCauseOOM}],
		"a run that holds no slot must not count toward the breaker")
}

// TestRecoverOOMKilled_WithholdsWhatARerunRepeats: every shape whose next attempt
// would get the limit that killed this one is withheld before any slot is spent.
func TestRecoverOOMKilled_WithholdsWhatARerunRepeats(t *testing.T) {
	cases := []struct {
		name   string
		pod    *corev1.Pod
		policy *MemoryRetryPolicy
	}{
		{"the pod was not built by a sizing profile", func() *corev1.Pod {
			a := rightSizedAnnotations("4242")
			delete(a, AnnotationSizingProfile)
			return oomKilled(scaleSetWorkerPod("runner-gpu-job1", a), "2Gi")
		}(), throughputRetry()},
		{"the set's profile derives no limit", oomKilled(scaleSetWorkerPod("runner-gpu-job1", rightSizedAnnotations("4242")), "2Gi"), nil},
		{"the limit is at the profile's ceiling", oomKilled(scaleSetWorkerPod("runner-gpu-job1", rightSizedAnnotations("4242")), "4Gi"),
			&MemoryRetryPolicy{HeadroomPercent: 150, Ceiling: resource.MustParse("4Gi")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			p, target, m, rerunCount, _ := recoveryFixture(t, tc.pod)
			target.spec.MemoryRetry = tc.policy

			done, err := p.RecoverEvictedScaleSetWorkers(ctx, target)
			require.NoError(t, err)
			<-done

			assert.Zero(t, rerunCount.Load())
			assert.Zero(t, testutil.ToFloat64(
				m.EvictionRetries.WithLabelValues("team-a", "gpu", evictionTierScaleSet, recoveryCauseOOM)),
				"a withheld re-run must spend no slot")
			assert.Equal(t, float64(1), testutil.ToFloat64(
				m.EvictionRerunWithheld.WithLabelValues("team-a", "gpu", evictionTierScaleSet, recoveryCauseOOM, rerunWithheldReasonOwnLimit)))
			l, _, err := p.readRerunLedger(ctx, target)
			require.NoError(t, err)
			assert.Empty(t, l.MemoryFloors)
		})
	}
}

// TestRecoverOOMKilled_SecondKillIsSameClassAgain: one bump per job, so a job that is
// killed again at its raised limit is not re-run a second time.
func TestRecoverOOMKilled_SecondKillIsSameClassAgain(t *testing.T) {
	ctx := context.Background()
	p, target, m, rerunCount, _ := recoveryFixture(t,
		oomKilled(scaleSetWorkerPod("runner-gpu-job1", rightSizedAnnotations("4242")), "2Gi"),
		oomKilled(scaleSetWorkerPod("runner-gpu-job2", rightSizedAnnotations("4242")), "3Gi"))
	target.spec.MemoryRetry = throughputRetry()

	for _, name := range []string{"runner-gpu-job1", "runner-gpu-job2"} {
		var pod corev1.Pod
		require.NoError(t, p.Client.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: name}, &pod))
		done, err := p.RecoverDisruptedScaleSetWorker(ctx, target, &pod)
		require.NoError(t, err)
		<-done
	}
	sweepPendingAndWait(t, p, ctx, 1)

	assert.Equal(t, int64(1), rerunCount.Load())
	assert.Equal(t, float64(1), testutil.ToFloat64(
		m.EvictionRerunWithheld.WithLabelValues("team-a", "gpu", evictionTierScaleSet, recoveryCauseOOM, rerunWithheldReasonSameClassAgain)))
	l, _, err := p.readRerunLedger(ctx, target)
	require.NoError(t, err)
	assert.Equal(t, "3Gi", l.MemoryFloors[failureKey("4242", "build")].Limit.String(),
		"the second kill must not raise the limit again")
}

// TestRecoverOOMKilled_BreakerPausesAStampede: past classBreakerRuns distinct runs in
// the window, the class stops re-running, says so once, and gauges the pause until its
// cooldown ends, whether or not another kill comes.
func TestRecoverOOMKilled_BreakerPausesAStampede(t *testing.T) {
	ctx := context.Background()
	var pods []*corev1.Pod
	for i := 0; i <= classBreakerRuns+1; i++ {
		pods = append(pods, oomKilled(scaleSetWorkerPod(fmt.Sprintf("runner-gpu-job%d", i),
			rightSizedAnnotations(fmt.Sprint(5000+i))), "2Gi"))
	}
	p, target, m, rerunCount, _ := recoveryFixture(t, pods...)
	target.spec.MemoryRetry = throughputRetry()
	clock := newTestClock()
	p.now = clock.now
	paused := m.AutoRetryPaused.WithLabelValues("team-a", "gpu", recoveryCauseOOM)

	for _, pod := range pods {
		done, err := p.RecoverDisruptedScaleSetWorker(ctx, target, pod.DeepCopy())
		require.NoError(t, err)
		<-done
	}
	sweepPendingAndWait(t, p, ctx, classBreakerRuns+1)

	assert.Equal(t, int64(classBreakerRuns+1), rerunCount.Load(),
		"the fire that opens the breaker still runs; the one after it is withheld")
	assert.Equal(t, float64(1), testutil.ToFloat64(
		m.EvictionRerunWithheld.WithLabelValues("team-a", "gpu", evictionTierScaleSet, recoveryCauseOOM, rerunWithheldReasonBreakerOpen)))
	events := 0
	for _, e := range target.events {
		if e == "AutoRetryPaused" {
			events++
		}
	}
	assert.Equal(t, 1, events)
	assert.Equal(t, float64(1), testutil.ToFloat64(paused), "the pause reads as a state while the breaker is open")

	clock.advance(classBreakerCooldown - time.Second)
	p.closeExpiredBreakers()
	assert.Equal(t, float64(1), testutil.ToFloat64(paused))
	clock.advance(time.Second)
	p.closeExpiredBreakers()
	assert.Zero(t, testutil.ToFloat64(paused), "the sweep's tick drops it when the cooldown ends")
}

func TestClassBreakers(t *testing.T) {
	var c classBreakers
	key := classBreakerKey{cause: recoveryCauseOOM}
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < classBreakerRuns; i++ {
		assert.False(t, c.record(key, "same-run", t0), "one run firing repeatedly is one run")
	}
	for i := 0; i < classBreakerRuns-1; i++ {
		assert.False(t, c.record(key, fmt.Sprint(i), t0))
	}
	assert.False(t, c.open(key, t0))

	// Fires that left the window no longer count.
	later := t0.Add(classBreakerWindow)
	assert.False(t, c.record(key, "late-1", later))
	assert.False(t, c.open(key, later))

	for i := 0; i < classBreakerRuns; i++ {
		c.record(key, fmt.Sprint("burst-", i), later)
	}
	assert.True(t, c.open(key, later))
	assert.True(t, c.open(key, later.Add(classBreakerCooldown-time.Second)))
	assert.False(t, c.open(key, later.Add(classBreakerCooldown)), "the cooldown closes it")
	assert.False(t, c.record(key, "after", later.Add(classBreakerCooldown)), "and its window starts empty")
	assert.False(t, c.open(key, later.Add(classBreakerCooldown)))
	assert.False(t, c.open(classBreakerKey{cause: "other"}, later), "breakers are per cause")

	reopened := later.Add(time.Hour)
	for i := 0; i <= classBreakerRuns; i++ {
		c.record(key, fmt.Sprint("again-", i), reopened)
	}
	assert.Empty(t, c.closeExpired(reopened.Add(classBreakerCooldown-time.Second)))
	assert.Equal(t, []classBreakerKey{key}, c.closeExpired(reopened.Add(classBreakerCooldown)))
	assert.False(t, c.open(key, reopened.Add(classBreakerCooldown)))
	assert.Empty(t, c.closeExpired(reopened.Add(classBreakerCooldown)), "a closed breaker is reported once")
}

// TestProvisionScaleSetWorker_AppliesTheMemoryFloor: the job's next attempt is built at
// the raised limit, by a process that never saw the kill (a restart), and nothing else
// is.
func TestProvisionScaleSetWorker_AppliesTheMemoryFloor(t *testing.T) {
	ctx := context.Background()
	writer, target, _, _, _ := recoveryFixture(t)
	floor := func(requestsFollow bool) {
		_, err := writer.recordMemoryFloor(ctx, target, "4242", "build", memoryFloor{
			Pod: "runner-gpu-job1", Container: runnerContainer,
			KilledLimit: resource.MustParse("2Gi"), Limit: resource.MustParse("3Gi"), RequestsFollow: requestsFollow,
		})
		require.NoError(t, err)
	}
	floor(true)

	template := corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: runnerContainer,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
		},
	}}}}
	target.spec.PodTemplate = template
	restarted := NewProvisioner(writer.Client, nil, nil)

	provision := func(jobID, jobName string) corev1.ResourceRequirements {
		require.NoError(t, restarted.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{
			JobID: jobID, JITConfig: "eyJ4IjoxfQ==", Owner: "myorg", Repository: "myrepo", RunID: "4242", JobName: jobName,
		}))
		var pod corev1.Pod
		require.NoError(t, restarted.Client.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: scaleSetPodName("gpu", jobID)}, &pod))
		for _, c := range pod.Spec.Containers {
			if c.Name == runnerContainer {
				return c.Resources
			}
		}
		t.Fatal("no runner container")
		return corev1.ResourceRequirements{}
	}

	raised := provision("job-uuid-2", "build")
	assert.Equal(t, "3Gi", raised.Limits.Memory().String())
	assert.Equal(t, "3Gi", raised.Requests.Memory().String(), "a profile that sets requests equal to limits raises both")
	assert.Equal(t, "2Gi", target.spec.PodTemplate.Spec.Containers[0].Resources.Limits.Memory().String(),
		"the resolved spec must not be written through")

	other := provision("job-uuid-3", "test")
	assert.Equal(t, "2Gi", other.Limits.Memory().String(), "another job of the run keeps its limit")
}

// TestApplyMemoryFloor_NeverLowersALimit: history that has since moved past the floor
// wins, and Throughput's request stays the history's.
func TestApplyMemoryFloor_NeverLowersALimit(t *testing.T) {
	ctx := context.Background()
	p, target, _, _, _ := recoveryFixture(t)
	_, err := p.recordMemoryFloor(ctx, target, "4242", "build", memoryFloor{
		Pod: "w", Container: runnerContainer, KilledLimit: resource.MustParse("2Gi"), Limit: resource.MustParse("3Gi"),
	})
	require.NoError(t, err)
	spec := func(limit string) *ResolvedSpec {
		return &ResolvedSpec{PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: runnerContainer,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(limit)},
			},
		}}}}}
	}
	job := ScaleSetJob{RunID: "4242", JobName: "build"}
	log := p.logFor()

	higher := spec("4Gi")
	assert.Same(t, higher, p.applyMemoryFloor(ctx, target, higher, job, log))

	got := p.applyMemoryFloor(ctx, target, spec("2Gi"), job, log)
	assert.Equal(t, "3Gi", got.PodTemplate.Spec.Containers[0].Resources.Limits.Memory().String())
	assert.Equal(t, "1Gi", got.PodTemplate.Spec.Containers[0].Resources.Requests.Memory().String())
}

// TestProvisionScaleSetWorker_StampsTheRerunCause: a worker built for a run GAG has
// re-run carries the cause of that re-run, one for any other run carries none, and a
// tenant cannot set the annotation through its template.
func TestProvisionScaleSetWorker_StampsTheRerunCause(t *testing.T) {
	ctx := context.Background()
	writer, target, _, _, _ := recoveryFixture(t)
	_, _, err := writer.reserveScaleSetRerun(ctx, target, "4242", rerunDisruption{key: "w1", job: "build", failedAttempt: 1}, recoveryCauseEviction, 2, nil)
	require.NoError(t, err)
	_, _, err = writer.reserveScaleSetRerun(ctx, target, "4242", rerunDisruption{key: "w2", job: "build", failedAttempt: 2}, recoveryCauseOOM, 2, nil)
	require.NoError(t, err)

	target.spec.PodTemplate = corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationRerunCause: "forged"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: runnerContainer}}},
	}
	restarted := NewProvisioner(writer.Client, nil, nil)
	cause := func(jobID, runID string) (string, bool) {
		require.NoError(t, restarted.ProvisionScaleSetWorker(ctx, target, ScaleSetJob{
			JobID: jobID, JITConfig: "eyJ4IjoxfQ==", Owner: "myorg", Repository: "myrepo", RunID: runID, JobName: "build",
		}))
		var pod corev1.Pod
		require.NoError(t, restarted.Client.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: scaleSetPodName("gpu", jobID)}, &pod))
		v, ok := pod.Annotations[AnnotationRerunCause]
		return v, ok
	}

	got, ok := cause("job-uuid-2", "4242")
	assert.True(t, ok)
	assert.Equal(t, recoveryCauseOOM, got, "the latest re-run's cause")
	_, ok = cause("job-uuid-3", "5000")
	assert.False(t, ok, "a run GAG never re-ran carries no cause, and the tenant's value is dropped")
	assert.Equal(t, "forged", target.spec.PodTemplate.Annotations[AnnotationRerunCause], "the resolved spec must not be written through")
}
