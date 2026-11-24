package provisioner

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// waitFixture is a set whose worker gpu-1, created for job "lint" of run 4242, was killed
// for memory just now with no start recorded.
type waitFixture struct {
	c      client.WithWatch
	p      *Provisioner
	target *stubTarget
	clock  *testClock
	killed *corev1.Pod
	newP   func() *Provisioner
}

func newWaitFixture(t *testing.T, change func(*corev1.Pod), others ...client.Object) *waitFixture {
	t.Helper()
	// Every run is in progress in its first attempt.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"in_progress","run_attempt":1}`)
	}))
	t.Cleanup(srv.Close)
	f := &waitFixture{clock: newTestClock()}
	f.killed = oomKilled(scaleSetWorkerPod(scaleSetPodName("gpu", "job-lint"), rightSizedAnnotations("4242")), "2Gi")
	f.killed.Annotations[AnnotationRunnerName] = "gpu-1"
	f.killed.Annotations[annotationJobName] = "lint"
	f.killed.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.NewTime(f.clock.now())
	if change != nil {
		change(f.killed)
	}
	f.c = fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(append(others, f.killed)...).Build()
	f.target = &stubTarget{key: client.ObjectKey{Namespace: "team-a", Name: "gpu"},
		spec: &ResolvedSpec{MaxEvictionRetries: 2, MemoryRetry: throughputRetry()}}
	f.newP = func() *Provisioner {
		p := ledgerProvisioner(f.c, srv)
		p.now = f.clock.now
		p.Metrics.EvictionRecoveryIdentityUnknown = evictionRecoveryMetrics().EvictionRecoveryIdentityUnknown
		return p
	}
	f.p = f.newP()
	return f
}

// live is the killed pod as the API has it.
func (f *waitFixture) live(t *testing.T) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	require.NoError(t, f.c.Get(context.Background(), client.ObjectKeyFromObject(f.killed), pod))
	return pod
}

func (f *waitFixture) scan(t *testing.T) {
	t.Helper()
	done, err := f.p.RecoverEvictedScaleSetWorkers(context.Background(), f.target)
	require.NoError(t, err)
	<-done
}

// watch hands the pod to the watch path, as an update to it does.
func (f *waitFixture) watch(t *testing.T) {
	t.Helper()
	done, err := f.p.RecoverDisruptedScaleSetWorker(context.Background(), f.target, f.live(t))
	require.NoError(t, err)
	<-done
}

// start handles the JobStarted of job on the killed worker's runner.
func (f *waitFixture) start(t *testing.T, job, runID string) {
	t.Helper()
	require.NoError(t, f.p.MarkScaleSetJobStarted(context.Background(), f.target, ScaleSetJob{
		JobID: "job-" + job, RunnerName: "gpu-1", Owner: "myorg", Repository: "myrepo", RunID: runID, JobName: job}))
}

func (f *waitFixture) claims(t *testing.T) map[string]recoveryClaim {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: "team-a",
		Name: scaleSetRecoveryClaimsConfigMapName("gpu")}, cm); err != nil {
		return nil
	}
	l, err := decodeRecoveryClaims(cm.Data[recoveryClaimDataKey])
	require.NoError(t, err)
	return l.Claims
}

// requireNothing asserts that nothing acted on the kill.
func (f *waitFixture) requireNothing(t *testing.T) {
	t.Helper()
	l := readLedger(t, f.p, f.target)
	assert.Empty(t, l.MemoryFloors, "no limit is raised")
	assert.Empty(t, l.Pending, "no re-run is parked")
	assert.Empty(t, l.Reruns, "no slot is spent")
	assert.Empty(t, f.target.events)
	assert.Empty(t, f.claims(t))
	assert.Empty(t, f.live(t).Annotations[AnnotationEvictionHandledAt])
}

func (f *waitFixture) unknown() float64 {
	return testutil.ToFloat64(f.p.Metrics.EvictionRecoveryIdentityUnknown.WithLabelValues("team-a", "gpu", recoveryCauseOOM))
}

// TestOOMStartWait_WaitsForTheStart is Q1389: a kill whose pod names only the job it was
// created for is left alone by both detection paths, even where another worker has started
// that job.
func TestOOMStartWait_WaitsForTheStart(t *testing.T) {
	holder := scaleSetWorkerPod(scaleSetPodName("gpu", "job-build"), rightSizedAnnotations("4242"))
	holder.Status.Phase = corev1.PodRunning
	holder.Annotations[AnnotationRunnerName] = "gpu-2"
	holder.Annotations[AnnotationStartedJobID] = "job-lint"
	for name, others := range map[string][]client.Object{"alone": nil, "its created-for job runs elsewhere": {holder}} {
		t.Run(name, func(t *testing.T) {
			f := newWaitFixture(t, nil, others...)
			f.watch(t)
			f.scan(t)
			f.requireNothing(t)
		})
	}
}

// TestOOMStartWait_TheStartNamesTheJob: the start stamps the Failed pod, and the class then
// acts once, on the job the runner held.
func TestOOMStartWait_TheStartNamesTheJob(t *testing.T) {
	for _, tc := range []struct{ name, job, runID string }{
		{"another job of the run", "build", "4242"},
		{"a job of another run", "build", "9999"},
		{"the job it was created for", "lint", "4242"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWaitFixture(t, nil)
			f.scan(t)
			f.start(t, tc.job, tc.runID)
			f.watch(t)
			f.scan(t)

			key := failureKey(tc.runID, tc.job)
			l := readLedger(t, f.p, f.target)
			require.Len(t, l.MemoryFloors, 1)
			require.Contains(t, l.MemoryFloors, key)
			require.Len(t, l.Pending, 1)
			require.Contains(t, l.Pending, key)
			require.Len(t, l.Reruns, 1)
			assert.Equal(t, 1, l.Reruns[tc.runID].Count)
			assert.Equal(t, []string{"MemoryLimitRaisedForRetry"}, f.target.events)
			assert.Equal(t, "job-"+tc.job, f.claims(t)[f.killed.Name].JobID)
		})
	}
}

// TestOOMStartWait_AStampedKillActsAtOnce is the control.
func TestOOMStartWait_AStampedKillActsAtOnce(t *testing.T) {
	f := newWaitFixture(t, func(pod *corev1.Pod) {
		pod.Annotations[AnnotationStartedJobID] = "job-build"
		pod.Annotations[annotationJobName] = "build"
	})
	f.watch(t)
	assert.Contains(t, readLedger(t, f.p, f.target).MemoryFloors, failureKey("4242", "build"))
}

// TestOOMStartWait_OnlyARetryableKillWaits: a kill the class raises nothing for, or whose
// worker a start cannot be matched to, is handled at once as before.
func TestOOMStartWait_OnlyARetryableKillWaits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*corev1.Pod)
		events []string
	}{
		{"no memory limit", func(p *corev1.Pod) { p.Spec.Containers[0].Resources.Limits = nil }, nil},
		{"no sizing profile", func(p *corev1.Pod) { delete(p.Annotations, AnnotationSizingProfile) }, []string{"EvictionRerunWithheld"}},
		{"no runner name", func(p *corev1.Pod) { delete(p.Annotations, AnnotationRunnerName) }, []string{"MemoryLimitRaisedForRetry"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWaitFixture(t, tc.change)
			f.scan(t)
			assert.Len(t, f.claims(t), 1)
			assert.Equal(t, tc.events, f.target.events)
		})
	}
}

// TestOOMStartWait_ATerminalPodNotWaitingIsNotStamped: the start of any other finished
// worker changes nothing, as before.
func TestOOMStartWait_ATerminalPodNotWaitingIsNotStamped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*corev1.Pod)
	}{
		{"evicted", func(p *corev1.Pod) { p.Status.Reason = podReasonEvicted }},
		{"already handled", func(p *corev1.Pod) { p.Annotations[AnnotationEvictionHandledAt] = "2026-11-01T00:00:00Z" }},
		{"succeeded", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }},
		{"failed on its own", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.Reason = "Error" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWaitFixture(t, tc.change)
			f.start(t, "build", "4242")
			assert.Empty(t, f.live(t).Annotations[AnnotationStartedJobID])
			assert.Equal(t, "lint", f.live(t).Annotations[annotationJobName])
		})
	}
}

// TestOOMStartWait_AStartWithNoIdentity: the pod's created-for identity is removed, so the
// class reports the job unknown instead of acting on it.
func TestOOMStartWait_AStartWithNoIdentity(t *testing.T) {
	f := newWaitFixture(t, nil)
	f.start(t, "build", "")
	f.watch(t)
	f.scan(t)
	assert.Equal(t, []string{"EvictionRecoveryIdentityUnknown"}, f.target.events)
	assert.Equal(t, float64(1), f.unknown())
	assert.Empty(t, readLedger(t, f.p, f.target).MemoryFloors)
}

// TestOOMStartWait_GivesUp: a start that never comes raises nothing, re-runs nothing, and
// is reported once; the claim that says so covers no job, so the created-for job keeps its
// own recovery and its own failure's attribution.
func TestOOMStartWait_GivesUp(t *testing.T) {
	ctx := context.Background()
	f := newWaitFixture(t, nil)
	f.scan(t)
	f.clock.advance(startWait - time.Second)
	f.scan(t)
	f.requireNothing(t)

	f.clock.advance(time.Second)
	f.scan(t)
	f.scan(t)
	f.start(t, "build", "4242")
	f.scan(t)
	assert.Equal(t, []string{"EvictionRecoveryIdentityUnknown"}, f.target.events)
	assert.Contains(t, f.target.messages[0], "gpu-1")
	assert.Equal(t, float64(1), f.unknown())
	l := readLedger(t, f.p, f.target)
	assert.Empty(t, l.MemoryFloors)
	assert.Empty(t, l.Pending)
	assert.Empty(t, l.Reruns)
	assert.NotEmpty(t, f.live(t).Annotations[AnnotationEvictionHandledAt])

	assert.Empty(t, f.p.recoveryAlreadyClaimed(ctx, f.target, map[string]bool{"job-lint": true}))
	assert.Empty(t, f.p.disruptionCause(ctx, f.target, "job-lint"))
}

// scanStale is scan with stale as the only pod the cache holds.
func (f *waitFixture) scanStale(t *testing.T, stale *corev1.Pod) {
	t.Helper()
	f.p.APIReader = f.c
	f.p.Client = interceptor.NewClient(f.c, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if pods, ok := list.(*corev1.PodList); ok {
				pods.Items = []corev1.Pod{*stale.DeepCopy()}
				return nil
			}
			return c.List(ctx, list, opts...)
		},
	})
	f.scan(t)
}

// TestOOMStartWait_AStaleScanReadsThePodBeforeGivingUp: the scan's cached pod can predate
// the stamp.
func TestOOMStartWait_AStaleScanReadsThePodBeforeGivingUp(t *testing.T) {
	f := newWaitFixture(t, nil)
	f.scan(t)
	stale := f.live(t)
	f.start(t, "build", "4242")
	f.clock.advance(startWait)
	f.scanStale(t, stale)
	assert.Contains(t, readLedger(t, f.p, f.target).MemoryFloors, failureKey("4242", "build"))
	assert.Equal(t, []string{"MemoryLimitRaisedForRetry"}, f.target.events)
}

// TestOOMStartWait_AStaleScanLeavesANamesakeAlone: a replayed assignment's pod takes the
// killed one's name.
func TestOOMStartWait_AStaleScanLeavesANamesakeAlone(t *testing.T) {
	for name, start := range map[string]bool{"started": true, "not started": false} {
		t.Run(name, func(t *testing.T) {
			f := newWaitFixture(t, nil)
			f.scan(t)
			stale := f.live(t)
			stale.UID = "the deleted pod's"
			if start {
				f.start(t, "build", "4242")
			}
			f.clock.advance(startWait)
			f.scanStale(t, stale)
			f.requireNothing(t)
		})
	}
}

// TestOOMStartWait_ARestartWaitsAgain: a new process scans before its listener has polled,
// so the wait runs from its first scan of the set, and the start acts once.
func TestOOMStartWait_ARestartWaitsAgain(t *testing.T) {
	f := newWaitFixture(t, nil)
	f.scan(t)
	f.clock.advance(startWait + time.Minute)
	f.p = f.newP()
	f.watch(t)
	f.scan(t)
	f.requireNothing(t)

	f.clock.advance(startWait - time.Second)
	f.scan(t)
	f.requireNothing(t)
	f.start(t, "build", "4242")
	f.watch(t)
	f.p = f.newP()
	f.scan(t)
	assert.Equal(t, 1, readLedger(t, f.p, f.target).Reruns["4242"].Count)
	assert.Equal(t, []string{"MemoryLimitRaisedForRetry"}, f.target.events)
}

// TestOOMStartWait_RestartsDoNotExtendItPastTheCap bounds how long the pod is kept.
func TestOOMStartWait_RestartsDoNotExtendItPastTheCap(t *testing.T) {
	f := newWaitFixture(t, nil)
	for waited := time.Duration(0); waited < startWaitCap; waited += 10 * time.Minute {
		f.p = f.newP()
		f.scan(t)
		f.requireNothing(t)
		f.clock.advance(10 * time.Minute)
	}
	f.p = f.newP()
	f.scan(t)
	assert.Equal(t, []string{"EvictionRecoveryIdentityUnknown"}, f.target.events)
}

// TestOOMStartWait_AnAGCDownPastTheCapGivesUpOnItsFirstScan is the cap's price.
func TestOOMStartWait_AnAGCDownPastTheCapGivesUpOnItsFirstScan(t *testing.T) {
	f := newWaitFixture(t, nil)
	f.clock.advance(startWaitCap - time.Minute)
	f.p = f.newP()
	f.scan(t)
	f.requireNothing(t)

	f.clock.advance(time.Minute)
	f.p = f.newP()
	f.scan(t)
	assert.Equal(t, []string{"EvictionRecoveryIdentityUnknown"}, f.target.events)
}

// TestOOMStartWait_AWatchAheadOfTheFirstScanWaits: an informer's creates reach the watch
// before a reconcile has scanned the set.
func TestOOMStartWait_AWatchAheadOfTheFirstScanWaits(t *testing.T) {
	f := newWaitFixture(t, nil)
	f.clock.advance(startWait + time.Minute)
	f.watch(t)
	f.requireNothing(t)
	_, look, hold := f.p.HoldUnclaimedOOM(f.target, f.killed, f.clock.now())
	assert.True(t, hold)
	assert.Equal(t, f.clock.now().Add(startWait), look)
}

// TestOOMStartWait_ADeletedPodIsSettled: a waiting kill is reported as unknown once, one
// whose start is recorded is recovered once, and nothing else is touched.
func TestOOMStartWait_ADeletedPodIsSettled(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*corev1.Pod)
		events []string
	}{
		{"waiting", nil, []string{"EvictionRecoveryIdentityUnknown"}},
		{"stamped", func(p *corev1.Pod) { p.Annotations[AnnotationStartedJobID] = "job-build" }, []string{"MemoryLimitRaisedForRetry"}},
		{"handled", func(p *corev1.Pod) { p.Annotations[AnnotationEvictionHandledAt] = "2026-11-01T00:00:00Z" }, nil},
		{"not a kill", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.Reason = "Error" }, nil},
		{"another tier's", func(p *corev1.Pod) { delete(p.Labels, LabelAcquisitionProtocol) }, nil},
		{"evicted", func(p *corev1.Pod) { p.Status.Reason = podReasonEvicted }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWaitFixture(t, tc.change)
			for range 2 {
				done, err := f.p.SettleDeletedKill(context.Background(), f.target, f.killed.DeepCopy())
				require.NoError(t, err)
				<-done
			}
			assert.Equal(t, tc.events, f.target.events)
			assert.Len(t, f.claims(t), len(tc.events))
			assert.Empty(t, f.p.recoveryAlreadyClaimed(context.Background(), f.target, map[string]bool{"job-lint": true}))
		})
	}
}

// TestOOMStartWait_AGivenUpPodIsNotReportedAgain: the marker is best-effort, so the claim
// is what keeps the delete report from repeating the give-up's.
func TestOOMStartWait_AGivenUpPodIsNotReportedAgain(t *testing.T) {
	f := newWaitFixture(t, nil)
	f.scan(t)
	f.clock.advance(startWait)
	f.scan(t)
	_, err := f.p.SettleDeletedKill(context.Background(), f.target, f.killed)
	require.NoError(t, err)
	assert.Equal(t, float64(1), f.unknown())
}

// TestHoldUnclaimedOOM: the reaper keeps an unclaimed kill past its wait, and is told when
// the next reconcile should look.
func TestHoldUnclaimedOOM(t *testing.T) {
	for _, tc := range []struct {
		name        string
		change      func(*corev1.Pod)
		waited      time.Duration
		hold        bool
		until, look time.Duration // from the pod's end
	}{
		{"waiting", nil, time.Minute, true, startWait + startWaitSlack, startWait},
		{"wait over, not given up", nil, startWait, true, startWait + startWaitSlack, startWait + startWaitRetry},
		{"stamped, not claimed", func(p *corev1.Pod) { p.Annotations[AnnotationStartedJobID] = "job-build" }, time.Minute,
			true, startWait + startWaitSlack, time.Minute + startWaitRetry},
		{"hold's last second", nil, startWait + startWaitSlack - time.Second, true, startWait + startWaitSlack, startWait + startWaitSlack - time.Second + startWaitRetry},
		{"hold over", nil, startWait + startWaitSlack, false, 0, 0},
		{"handled", func(p *corev1.Pod) { p.Annotations[AnnotationEvictionHandledAt] = "2026-11-01T00:00:00Z" }, time.Minute, false, 0, 0},
		{"no memory limit", func(p *corev1.Pod) { p.Spec.Containers[0].Resources.Limits = nil }, time.Minute, false, 0, 0},
		{"evicted", func(p *corev1.Pod) { p.Status.Reason = podReasonEvicted }, time.Minute, false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWaitFixture(t, tc.change)
			ended := f.clock.now()
			f.p.startWaits.scanned(f.target.Key().String(), ended)
			until, look, hold := f.p.HoldUnclaimedOOM(f.target, f.killed, ended.Add(tc.waited))
			require.Equal(t, tc.hold, hold)
			if hold {
				assert.Equal(t, tc.until, until.Sub(ended))
				assert.Equal(t, tc.look, look.Sub(ended))
			}
		})
	}
}

// TestStartedAfterFailure is the update that ends a wait, and the ones that do not.
func TestStartedAfterFailure(t *testing.T) {
	pod := func(phase corev1.PodPhase, started, handled string) *corev1.Pod {
		p := &corev1.Pod{Status: corev1.PodStatus{Phase: phase}}
		p.Annotations = map[string]string{}
		if started != "" {
			p.Annotations[AnnotationStartedJobID] = started
		}
		if handled != "" {
			p.Annotations[AnnotationEvictionHandledAt] = handled
		}
		return p
	}
	assert.True(t, StartedAfterFailure(pod(corev1.PodFailed, "", ""), pod(corev1.PodFailed, "job", "")))
	assert.False(t, StartedAfterFailure(pod(corev1.PodRunning, "", ""), pod(corev1.PodRunning, "job", "")), "a running worker's start")
	assert.False(t, StartedAfterFailure(pod(corev1.PodFailed, "job", ""), pod(corev1.PodFailed, "job", "")), "already stamped")
	assert.False(t, StartedAfterFailure(pod(corev1.PodFailed, "", ""), pod(corev1.PodFailed, "job", "now")), "claimed")
	assert.False(t, StartedAfterFailure(pod(corev1.PodFailed, "", ""), pod(corev1.PodFailed, "", "")))
}
