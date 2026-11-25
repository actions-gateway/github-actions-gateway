package provisioner

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
)

// Q555: flaky-job detection. These pin the design's pairing table row by row, a
// replayed completion, a user's cancel, the snapshot's fault reading, and the cap
// evicting failure records before it refuses a budget.

func flakyMetrics() *runnercore.Metrics {
	return &runnercore.Metrics{
		EvictionRerunOutcomes: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "t_q555_outcomes_total"},
			[]string{"namespace", "runner_group", "cause", "event", "result"}),
		ScaleSetJobReruns: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "t_q555_reruns_total"},
			[]string{"namespace", "runner_set", "trigger"}),
		ScaleSetFlakyJobs: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "t_q555_flaky_total"},
			[]string{"namespace", "runner_set", "trigger"}),
	}
}

func flakyFixture(t *testing.T) (client.Client, *Provisioner, *stubTarget) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	return c, flakyProvisioner(c), &stubTarget{key: client.ObjectKey{Namespace: "team-a", Name: "gpu"}}
}

// flakyProvisioner is one AGC process; two on one client are a restart. GitHub lists
// attempt n of a run as one job "build" on runner-n, as buildJob(n) completes it.
func flakyProvisioner(c client.Client) *Provisioner {
	p := NewProvisioner(c, flakyMetrics(), nil)
	p.pairingDelay = 0
	gh := &runJobsStub{}
	for n := 1; n <= 9; n++ {
		gh.jobs = append(gh.jobs, listed(n, "build", fmt.Sprintf("runner-%d", n)))
	}
	gh.serve(p)
	return p
}

// buildJob is attempt n of job "build" in run 900, triggered by a push.
func buildJob(n int) ScaleSetJob {
	return ScaleSetJob{
		JobID: fmt.Sprintf("job-%d", n), RunnerName: fmt.Sprintf("runner-%d", n),
		Owner: "org", Repository: "repo", RunID: "900", JobName: "build",
		EventName: "push", WorkflowRef: "org/repo/.github/workflows/ci.yml@refs/heads/main",
	}
}

// completeJob records job's completion and waits for its pairing.
func completeJob(ctx context.Context, t *testing.T, p *Provisioner, target Target, job ScaleSetJob, result string) {
	t.Helper()
	done, err := p.RecordScaleSetJobCompleted(ctx, target, job, result)
	require.NoError(t, err)
	<-done
}

// claimDisruption records a recovery claim for jobID, as a disruption arm does.
func claimDisruption(t *testing.T, p *Provisioner, target Target, jobID, cause string) {
	t.Helper()
	require.NoError(t, p.claimDisruptionRecovery(context.Background(), target, "pod-"+jobID, jobID, cause))
}

func rerunCount(p *Provisioner, trigger string) float64 {
	return testutil.ToFloat64(p.Metrics.ScaleSetJobReruns.WithLabelValues("team-a", "gpu", trigger))
}

func flaky(p *Provisioner, trigger string) float64 {
	return testutil.ToFloat64(p.Metrics.ScaleSetFlakyJobs.WithLabelValues("team-a", "gpu", trigger))
}

func outcome(p *Provisioner, cause, result string) float64 {
	return testutil.ToFloat64(p.Metrics.EvictionRerunOutcomes.WithLabelValues("team-a", "gpu", cause, "push", result))
}

func readFailures(t *testing.T, p *Provisioner, target Target) map[string]*failureRecord {
	t.Helper()
	l, _, err := p.readRerunLedger(context.Background(), target)
	require.NoError(t, err)
	return l.Failures
}

// TestFlakyJobs_PairingTable walks the design's pairing table: a failed attempt of
// "build", then a later attempt that succeeds, under each shape of GAG's re-runs of
// the run.
func TestFlakyJobs_PairingTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		// attempt is GAG's re-run of the run, nil for none, and of the attempt that
		// failed unless ofAttempt says another.
		attempt   *rerunAttempt
		ofAttempt string
		// claim is the disruption a recovery claim names for the failed attempt.
		claim       string
		wantTrigger string
		wantFlaky   bool
		wantOutcome string
	}{
		{name: "the job was in GAG's re-run for a disruption",
			attempt: &rerunAttempt{Scope: rerunScopeFailedJobs, Cause: recoveryCauseEviction, Jobs: []string{"build"}},
			claim:   recoveryCauseEviction, wantTrigger: rerunTriggerGAG, wantOutcome: recoveryCauseEviction},
		{name: "GAG's failed-jobs re-run for another job carried it",
			attempt:     &rerunAttempt{Scope: rerunScopeFailedJobs, Cause: recoveryCauseDeletion, Jobs: []string{"lint"}},
			wantTrigger: rerunTriggerGAGCollateral, wantFlaky: true},
		{name: "GAG's single-job re-run of a job it needs took it along",
			attempt:     &rerunAttempt{Scope: rerunScopeJob, Cause: recoveryCauseDeletion, Jobs: []string{"lint"}},
			wantTrigger: rerunTriggerGAGCollateral, wantFlaky: true},
		{name: "GAG re-ran another attempt of the run",
			attempt:     &rerunAttempt{Scope: rerunScopeFailedJobs, Cause: recoveryCauseDeletion, Jobs: []string{"build"}},
			ofAttempt:   "4",
			wantTrigger: rerunTriggerOther, wantFlaky: true},
		{name: "GAG did not re-run the run",
			wantTrigger: rerunTriggerOther, wantFlaky: true},
		{name: "a disruption GAG did not re-run is not flaky",
			claim: recoveryCausePreemption, wantTrigger: rerunTriggerOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			_, p, target := flakyFixture(t)
			if tc.attempt != nil {
				require.NoError(t, p.updateRerunLedger(ctx, target, func(l *rerunLedger, now time.Time) bool {
					l.Reruns["900"] = &rerunRecord{ExpiresAt: now.Add(time.Hour), Count: 1,
						Attempts: map[string]*rerunAttempt{cmp.Or(tc.ofAttempt, "1"): tc.attempt}}
					return true
				}))
			}
			if tc.claim != "" {
				claimDisruption(t, p, target, "job-1", tc.claim)
			}

			completeJob(ctx, t, p, target, buildJob(1), "failed")
			require.Contains(t, readFailures(t, p, target), "900/build", "the failure is recorded")
			completeJob(ctx, t, p, target, buildJob(2), jobResultSucceeded)

			assert.NotContains(t, readFailures(t, p, target), "900/build", "a succeeded pairing deletes the record")
			for _, trigger := range []string{rerunTriggerGAG, rerunTriggerGAGCollateral, rerunTriggerOther} {
				want := 0.0
				if trigger == tc.wantTrigger {
					want = 1
				}
				assert.Equal(t, want, rerunCount(p, trigger), "reruns{trigger=%s}", trigger)
				if !tc.wantFlaky || trigger != tc.wantTrigger {
					want = 0
				}
				assert.Equal(t, want, flaky(p, trigger), "flaky{trigger=%s}", trigger)
			}
			if tc.wantOutcome != "" {
				assert.Equal(t, 1.0, outcome(p, tc.wantOutcome, jobResultSucceeded))
			} else {
				assert.Zero(t, testutil.CollectAndCount(p.Metrics.EvictionRerunOutcomes), "only GAG's own re-runs have an outcome")
			}
			if tc.wantFlaky {
				assert.Contains(t, target.events, "FlakyJobDetected")
			} else {
				assert.NotContains(t, target.events, "FlakyJobDetected")
			}
		})
	}
}

// TestFlakyJobs_ALaterFailureUpdatesTheRecord is the table's last row: GAG's re-run of
// a disrupted job fails again, which counts as that cause's failed outcome, keeps the
// first failure's context, and spends GAG's re-run so a further attempt that succeeds
// after a human re-run reads as the human's, and flaky.
func TestFlakyJobs_ALaterFailureUpdatesTheRecord(t *testing.T) {
	ctx := context.Background()
	_, p, target := flakyFixture(t)
	require.NoError(t, p.updateRerunLedger(ctx, target, func(l *rerunLedger, now time.Time) bool {
		l.Reruns["900"] = &rerunRecord{ExpiresAt: now.Add(time.Hour), Count: 1, Attempts: map[string]*rerunAttempt{
			"1": {Scope: rerunScopeFailedJobs, Cause: recoveryCauseEviction, Jobs: []string{"build"}}}}
		return true
	}))
	claimDisruption(t, p, target, "job-1", recoveryCauseEviction)
	p.SnapshotScaleSetWorker(context.Background(), nil, scaleSetWorker("runner-1", corev1.PodFailed, "Evicted"), false)

	completeJob(ctx, t, p, target, buildJob(1), "failed")
	first := readFailures(t, p, target)["900/build"]
	require.NotNil(t, first)
	require.Contains(t, string(first.Context), "Evicted")

	completeJob(ctx, t, p, target, buildJob(2), "failed")
	f := readFailures(t, p, target)["900/build"]
	require.NotNil(t, f, "a failed later attempt keeps the record")
	assert.Equal(t, "job-2", f.JobID)
	assert.Empty(t, f.Cause, "the second failure had no disruption")
	assert.Equal(t, first.Context, f.Context, "the first failure's context is kept")
	assert.Equal(t, first.FailedAt, f.FailedAt)
	assert.Equal(t, 1.0, outcome(p, recoveryCauseEviction, "failed"))
	assert.Equal(t, 1.0, rerunCount(p, rerunTriggerGAG))

	completeJob(ctx, t, p, target, buildJob(3), jobResultSucceeded)
	assert.Equal(t, 1.0, rerunCount(p, rerunTriggerOther), "GAG re-ran attempt 1 and not attempt 2")
	assert.Equal(t, 1.0, flaky(p, rerunTriggerOther))
	assert.Equal(t, 1.0, outcome(p, recoveryCauseEviction, "failed"), "a human's re-run is not GAG's outcome")
	assert.Zero(t, outcome(p, recoveryCauseEviction, jobResultSucceeded))
}

// TestFlakyJobs_AReplayedCompletionChangesNothing replays each completion, in-process
// and after a restart, as a re-created session does from cursor 0.
func TestFlakyJobs_AReplayedCompletionChangesNothing(t *testing.T) {
	ctx := context.Background()
	c, p, target := flakyFixture(t)

	completeJob(ctx, t, p, target, buildJob(1), "failed")
	before := readLedgerData(t, c, target)
	replayed := flakyProvisioner(c)
	completeJob(ctx, t, replayed, target, buildJob(1), "failed")
	assert.Equal(t, before, readLedgerData(t, c, target), "a replayed failure rewrites nothing")
	assert.Zero(t, replayed.HTTPClient.Transport.(*runJobsStub).gets.Load(), "and reads no job list")

	completeJob(ctx, t, p, target, buildJob(2), jobResultSucceeded)
	restarted := flakyProvisioner(c)
	completeJob(ctx, t, restarted, target, buildJob(2), jobResultSucceeded)
	completeJob(ctx, t, p, target, buildJob(2), jobResultSucceeded)
	assert.Equal(t, 1.0, rerunCount(p, rerunTriggerOther))
	assert.Zero(t, rerunCount(restarted, rerunTriggerOther), "a replayed success pairs nothing")
}

// TestFlakyJobs_AUsersCancelRecordsNothing: a cancelled completion is a failure only
// where a disruption claim names it, since GAG's own force-cancel after a deletion
// concludes the job cancelled.
func TestFlakyJobs_AUsersCancelRecordsNothing(t *testing.T) {
	ctx := context.Background()
	_, p, target := flakyFixture(t)
	completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
	assert.Empty(t, readFailures(t, p, target))

	claimDisruption(t, p, target, "job-1", recoveryCauseDeletion)
	completeJob(ctx, t, p, target, buildJob(1), jobResultCanceled)
	f := readFailures(t, p, target)["900/build"]
	require.NotNil(t, f)
	assert.Equal(t, recoveryCauseDeletion, f.Cause)
}

// TestFlakyJobs_ASucceededJobWithNoRecordCostsNoRead: the common completion reads the
// in-memory index, not the ledger, once this process has read the ledger.
func TestFlakyJobs_ASucceededJobWithNoRecordCostsNoRead(t *testing.T) {
	ctx := context.Background()
	var gets int
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.ConfigMap); ok {
				gets++
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	p := flakyProvisioner(c)
	target := &stubTarget{key: client.ObjectKey{Namespace: "team-a", Name: "gpu"}}

	completeJob(ctx, t, p, target, buildJob(1), jobResultSucceeded)
	require.Equal(t, 1, gets, "the first completion loads the ledger")
	for n := 2; n < 10; n++ {
		job := buildJob(n)
		job.JobName = fmt.Sprintf("test-%d", n)
		completeJob(ctx, t, p, target, job, jobResultSucceeded)
	}
	assert.Equal(t, 1, gets, "a succeeded job with no record reads nothing")
}

// TestFlakyJobs_ASnapshotNamesThePlatformFault: a worker killed for memory is a fault,
// with a Warning Event and its context recorded; a red test on a healthy worker is not.
func TestFlakyJobs_ASnapshotNamesThePlatformFault(t *testing.T) {
	ctx := context.Background()
	_, p, target := flakyFixture(t)

	oom := scaleSetWorker("runner-1", corev1.PodFailed, "")
	oom.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: runnerContainer,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}}}
	p.SnapshotScaleSetWorker(context.Background(), nil, oom, false)
	completeJob(ctx, t, p, target, buildJob(1), "failed")
	assert.Equal(t, []string{"PlatformFaultOnFailedJob"}, target.events)
	var snap workerSnapshot
	require.NoError(t, json.Unmarshal(readFailures(t, p, target)["900/build"].Context, &snap))
	assert.Equal(t, "OOMKilled", snap.Containers[0].Reason)

	red := scaleSetWorker("runner-x", corev1.PodSucceeded, "")
	red.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: runnerContainer,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}}}}
	red.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "proxy-sidecar",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 143}}}}
	p.SnapshotScaleSetWorker(context.Background(), nil, red, false)
	job := buildJob(1)
	job.JobID, job.RunnerName, job.JobName = "job-x", "runner-x", "test"
	completeJob(ctx, t, p, target, job, "failed")
	assert.Equal(t, []string{"PlatformFaultOnFailedJob"}, target.events, "a red test on a healthy worker is not a fault")
}

// TestFlakyJobs_ASidecarThatSurvivedANestedKillIsNotAFault (Q1395): a nested container's
// kill leaves OOMKilled on a sidecar that was never killed, so a sidecar's reason is a
// fault only with a SIGKILL's exit code.
func TestFlakyJobs_ASidecarThatSurvivedANestedKillIsNotAFault(t *testing.T) {
	at := time.Now().Truncate(time.Second)
	oomKilled := func(name string, code int32) corev1.ContainerStatus { return terminated(name, "OOMKilled", code, at) }
	restarted := func(code int32) corev1.ContainerStatus {
		cs := terminated("dind", "Error", 143, at)
		cs.RestartCount, cs.LastTerminationState = 1, oomKilled("dind", code).State
		return cs
	}
	for _, tc := range []struct {
		name          string
		init, regular []corev1.ContainerStatus
		wantFaults    []string
	}{
		{name: "a sidecar that stopped normally", init: []corev1.ContainerStatus{oomKilled("dind", 0)}},
		{name: "a sidecar that stopped on SIGTERM", init: []corev1.ContainerStatus{oomKilled("dind", 143)}},
		{name: "a sidecar that was killed", init: []corev1.ContainerStatus{oomKilled("dind", 137)},
			wantFaults: []string{"container dind OOMKilled"}},
		{name: "an earlier run that exited by itself", init: []corev1.ContainerStatus{restarted(1)}},
		{name: "an earlier run that was killed", init: []corev1.ContainerStatus{restarted(137)},
			wantFaults: []string{"container dind OOMKilled"}},
		{name: "a regular container that stopped normally", regular: []corev1.ContainerStatus{oomKilled("dind", 0)}},
		{name: "the runner, whatever it exited", regular: []corev1.ContainerStatus{oomKilled(runnerContainer, 0)},
			wantFaults: []string{"container runner OOMKilled"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			_, p, target := flakyFixture(t)
			pod := scaleSetWorker("runner-1", corev1.PodSucceeded, "")
			pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses = tc.init, tc.regular
			assert.Equal(t, tc.wantFaults, snapshotOf(pod, at).faults())

			<-p.SnapshotScaleSetWorker(ctx, nil, pod, false)
			completeJob(ctx, t, p, target, buildJob(1), "failed")
			wantEvents := 0
			if tc.wantFaults != nil {
				wantEvents = 1
			}
			assert.Len(t, target.events, wantEvents)
		})
	}
}

// staticProxy is an EgressProxyReader returning one readiness.
type staticProxy struct{ snap *ProxySnapshot }

func (s staticProxy) EgressProxyReadiness(context.Context) *ProxySnapshot { return s.snap }

// proxyTarget is a Target that reads its proxy's readiness, as the v2 one does.
type proxyTarget struct {
	*stubTarget
	staticProxy
}

// TestFlakyJobs_AnUnreadyColocatedProxyIsAPlatformFault (Q1237): a red test on a
// healthy worker is a platform fault when its colocated proxy pool was not Ready or
// had no ready replica, and never for a shared pool, whose readiness is unread.
func TestFlakyJobs_AnUnreadyColocatedProxyIsAPlatformFault(t *testing.T) {
	colocated := func(ready string, replicas int32) *ProxySnapshot {
		return &ProxySnapshot{Name: "ep", Mode: ProxyModeColocated, Ready: ready, Reason: "PodsUnavailable", ReadyReplicas: &replicas}
	}
	for _, tc := range []struct {
		name      string
		proxy     *ProxySnapshot
		wantFault string
	}{
		{name: "not Ready", proxy: colocated("False", 1), wantFault: "EgressProxy ep not Ready PodsUnavailable, 1 ready replicas"},
		{name: "no ready replicas", proxy: colocated("True", 0), wantFault: "EgressProxy ep has no ready replicas"},
		{name: "ready", proxy: colocated("True", 2)},
		{name: "unread colocated pool", proxy: &ProxySnapshot{Name: "ep", Mode: ProxyModeColocated}},
		{name: "shared pool", proxy: &ProxySnapshot{Name: "ep", Mode: ProxyModeShared}},
		{name: "direct egress"},
	} {
		for _, path := range []string{"snapshot", "completion"} {
			t.Run(tc.name+" via "+path, func(t *testing.T) {
				ctx := context.Background()
				c, p, stub := flakyFixture(t)
				target := proxyTarget{stub, staticProxy{tc.proxy}}
				red := scaleSetWorker("runner-1", corev1.PodSucceeded, "")
				red.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: runnerContainer,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}}}}
				if path == "snapshot" {
					p.SnapshotScaleSetWorker(ctx, staticProxy{tc.proxy}, red, false)
				} else {
					require.NoError(t, c.Create(ctx, red))
				}
				completeJob(ctx, t, p, target, buildJob(1), "failed")
				assert.Empty(t, p.workerSnapshots.recorded, "a finished worker's record waits for nothing")

				var snap workerSnapshot
				require.NoError(t, json.Unmarshal(readFailures(t, p, stub)["900/build"].Context, &snap))
				assert.Equal(t, tc.proxy, snap.Proxy)
				if tc.wantFault == "" {
					assert.Empty(t, stub.events)
					return
				}
				assert.Equal(t, []string{tc.wantFault}, snap.faults())
				assert.Equal(t, []string{"PlatformFaultOnFailedJob"}, stub.events)
			})
		}
	}
}

// TestFlakyJobs_AStartOnAFailedWorkerKeepsItsProxyReading: the start of a job whose runner
// was killed first snapshots its Failed worker again (Q1389), and what became of the proxy
// in between did not fail the job.
func TestFlakyJobs_AStartOnAFailedWorkerKeepsItsProxyReading(t *testing.T) {
	reading := func(ready string) staticProxy {
		return staticProxy{&ProxySnapshot{Name: "ep", Mode: ProxyModeColocated, Ready: ready, Reason: "PodsUnavailable"}}
	}
	const notReady = "EgressProxy ep not Ready PodsUnavailable"
	for _, tc := range []struct {
		name               string
		atFailure, atStart EgressProxyReader
		want               []string
	}{
		{"went down between", reading("True"), reading("False"), []string{"pod Failed"}},
		{"came up between", reading("False"), reading("True"), []string{"pod Failed", notReady}},
		{"down throughout", reading("False"), reading("False"), []string{"pod Failed", notReady}},
		{"referenced since", staticProxy{}, reading("False"), []string{"pod Failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			_, p, target := flakyFixture(t)
			pod := scaleSetWorker("runner-1", corev1.PodFailed, "")
			p.SnapshotScaleSetWorker(ctx, tc.atFailure, pod, false)
			started := pod.DeepCopy()
			started.Annotations[AnnotationStartedJobID] = "job-1"
			p.SnapshotScaleSetWorker(ctx, tc.atStart, started, false)
			completeJob(ctx, t, p, target, buildJob(1), "failed")
			var snap workerSnapshot
			require.NoError(t, json.Unmarshal(readFailures(t, p, target)["900/build"].Context, &snap))
			assert.Equal(t, tc.want, snap.faults())
		})
	}

	_, p, _ := flakyFixture(t)
	pod := scaleSetWorker("runner-1", corev1.PodFailed, "")
	p.SnapshotScaleSetWorker(context.Background(), reading("True"), pod, false)
	pod.Name = "another-worker"
	p.SnapshotScaleSetWorker(context.Background(), reading("False"), pod, false)
	assert.Contains(t, p.workerSnapshots.take("team-a/runner-1").faults(), notReady, "another pod's reading is its own")
}

// TestFlakyJobs_ACompletionWithNoIdentityTakesThePods: the run and job come from the
// worker's annotations where the completion carries neither.
func TestFlakyJobs_ACompletionWithNoIdentityTakesThePods(t *testing.T) {
	ctx := context.Background()
	_, p, target := flakyFixture(t)
	p.SnapshotScaleSetWorker(context.Background(), nil, scaleSetWorker("runner-1", corev1.PodSucceeded, ""), false)
	completeJob(ctx, t, p, target, ScaleSetJob{JobID: "job-1", RunnerName: "runner-1"}, "failed")
	assert.Contains(t, readFailures(t, p, target), "900/build")
}

// TestFlakyJobs_SnapshotsOnlyTerminalTransitions: a snapshot is taken at a terminal
// phase or a deletion of a pod that never reached one, and never for a classic worker.
func TestFlakyJobs_SnapshotsOnlyTerminalTransitions(t *testing.T) {
	deleting := func(pod *corev1.Pod) *corev1.Pod {
		now := metav1.Now()
		pod.DeletionTimestamp = &now
		return pod
	}
	for _, tc := range []struct {
		name string
		pod  func() *corev1.Pod
		gone bool
		want bool
	}{
		{"running", func() *corev1.Pod { return scaleSetWorker("r", corev1.PodRunning, "") }, false, false},
		{"succeeded", func() *corev1.Pod { return scaleSetWorker("r", corev1.PodSucceeded, "") }, false, true},
		{"failed", func() *corev1.Pod { return scaleSetWorker("r", corev1.PodFailed, "") }, false, true},
		{"deleted while running", func() *corev1.Pod { return deleting(scaleSetWorker("r", corev1.PodRunning, "")) }, false, false},
		{"finished after being deleted", func() *corev1.Pod { return deleting(scaleSetWorker("r", corev1.PodSucceeded, "")) }, false, true},
		{"gone without finishing", func() *corev1.Pod { return deleting(scaleSetWorker("r", corev1.PodRunning, "")) }, true, true},
		{"reaped after finishing", func() *corev1.Pod { return deleting(scaleSetWorker("r", corev1.PodSucceeded, "")) }, true, false},
		{"classic worker", func() *corev1.Pod {
			pod := scaleSetWorker("r", corev1.PodFailed, "")
			delete(pod.Labels, LabelAcquisitionProtocol)
			return pod
		}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := flakyProvisioner(nil)
			p.SnapshotScaleSetWorker(context.Background(), nil, tc.pod(), tc.gone)
			assert.Equal(t, tc.want, p.workerSnapshots.take("team-a/r") != nil)
		})
	}
}

func TestFlakyJobs_SnapshotStoreIsBounded(t *testing.T) {
	var w workerSnapshots
	base := time.Now()
	for i := range maxWorkerSnapshots + 1 {
		at := base.Add(time.Duration(i) * time.Second)
		w.put(fmt.Sprint("s", i), &workerSnapshot{Phase: "Failed", takenAt: at}, false)
		w.hold(fmt.Sprint("r", i), &recordedFailure{at: at})
	}
	assert.Len(t, w.byID, maxWorkerSnapshots)
	assert.Nil(t, w.take("s0"), "the oldest snapshot is evicted")
	assert.NotNil(t, w.take(fmt.Sprint("s", maxWorkerSnapshots)))
	assert.Len(t, w.recorded, maxWorkerSnapshots)
	assert.NotContains(t, w.recorded, "r0", "the oldest recorded failure is evicted")
}

// recordedContext is the context of the record for run 900's job "build".
func recordedContext(t *testing.T, p *Provisioner, target Target) workerSnapshot {
	t.Helper()
	f := readFailures(t, p, target)["900/build"]
	require.NotNil(t, f)
	var s workerSnapshot
	require.NoError(t, json.Unmarshal(f.Context, &s))
	return s
}

// finishWorker is pod after its runner exited, with the completion stamp a Running pod
// is given when its job completes.
func finishWorker(pod *corev1.Pod, at time.Time) *corev1.Pod {
	done := pod.DeepCopy()
	done.Annotations[AnnotationJobCompletedAt] = at.UTC().Format(time.RFC3339)
	done.Status.Phase = corev1.PodSucceeded
	done.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: runnerContainer, State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", FinishedAt: metav1.NewTime(at)}}}}
	return done
}

// terminated is a container status that exited with reason and code at the given time.
func terminated(name, reason string, code int32, at time.Time) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{Reason: reason, ExitCode: code, FinishedAt: metav1.NewTime(at)}}}
}

// TestFlakyJobs_ARunningWorkersRecordIsCompletedWhenItFinishes (Q1376): a job that ends
// on a step's result completes while its worker runs, so the record starts from the
// running pod and is rewritten at the worker's terminal transition. Only what happened
// before the completion is reported as a fault, and only once.
func TestFlakyJobs_ARunningWorkersRecordIsCompletedWhenItFinishes(t *testing.T) {
	end := time.Now().Truncate(time.Second)
	before, after := end.Add(-time.Minute), end.Add(time.Minute)
	oomSidecar := func(at time.Time) []corev1.ContainerStatus {
		return []corev1.ContainerStatus{terminated("dind", "OOMKilled", 137, at)}
	}
	crashedSidecar := func(at time.Time, state corev1.ContainerState) []corev1.ContainerStatus {
		crash := terminated("dind", "OOMKilled", 137, at)
		return []corev1.ContainerStatus{{Name: "dind", RestartCount: 1, State: state, LastTerminationState: crash.State}}
	}
	disrupted := func(at time.Time) []corev1.PodCondition {
		return []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue,
			Reason: "TerminationByKubelet", LastTransitionTime: metav1.NewTime(at)}}
	}
	for _, tc := range []struct {
		name   string
		worker func(running, finished *corev1.Pod)
		// wantEvents is the PlatformFaultOnFailedJob Events over the completion and the
		// finish together.
		wantEvents int
	}{
		{name: "a red test on a healthy worker", worker: func(_, _ *corev1.Pod) {}},
		{name: "a sidecar killed for memory before the job ended", wantEvents: 1, worker: func(_, f *corev1.Pod) {
			f.Status.InitContainerStatuses = oomSidecar(before)
		}},
		{name: "a sidecar killed for memory after the job ended", worker: func(_, f *corev1.Pod) {
			f.Status.InitContainerStatuses = oomSidecar(after)
		}},
		{name: "a fault the running worker already showed", wantEvents: 1, worker: func(r, f *corev1.Pod) {
			r.Status.InitContainerStatuses = oomSidecar(before)
			f.Status.InitContainerStatuses = oomSidecar(before)
		}},
		{name: "a sidecar's crash a deleted pod's status still shows", wantEvents: 1, worker: func(_, f *corev1.Pod) {
			f.Status.InitContainerStatuses = crashedSidecar(before, terminated("dind", "Error", 143, after).State)
		}},
		{name: "a sidecar's crash the running worker already showed", wantEvents: 1, worker: func(r, f *corev1.Pod) {
			r.Status.InitContainerStatuses = crashedSidecar(before, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
			f.Status.InitContainerStatuses = crashedSidecar(before, terminated("dind", "Error", 143, after).State)
		}},
		{name: "a sidecar's crash after the job ended", worker: func(_, f *corev1.Pod) {
			f.Status.InitContainerStatuses = crashedSidecar(after, terminated("dind", "Error", 143, after).State)
		}},
		{name: "a disruption before the job ended", wantEvents: 1, worker: func(_, f *corev1.Pod) {
			f.Status.Conditions = disrupted(before)
		}},
		{name: "a disruption after the job ended", worker: func(_, f *corev1.Pod) {
			f.Status.Conditions = disrupted(after)
		}},
		{name: "a sidecar killed for memory in the second the job ended", wantEvents: 1, worker: func(_, f *corev1.Pod) {
			f.Status.InitContainerStatuses = oomSidecar(end)
		}},
		{name: "a termination the kubelet gave no time", worker: func(_, f *corev1.Pod) {
			f.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: runnerContainer, State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{Reason: "ContainerStatusUnknown", ExitCode: 137}}}}
		}},
		{name: "a runner killed after the job ended", worker: func(_, f *corev1.Pod) {
			f.Status.Phase = corev1.PodFailed
			f.Status.ContainerStatuses = []corev1.ContainerStatus{terminated(runnerContainer, "Error", 143, after)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c, p, target := flakyFixture(t)
			p.now = func() time.Time { return end }
			running := scaleSetWorker("runner-1", corev1.PodRunning, "")
			finished := finishWorker(running, after)
			tc.worker(running, finished)
			require.NoError(t, c.Create(ctx, running))

			completeJob(ctx, t, p, target, buildJob(1), "failed")
			require.Equal(t, "Running", recordedContext(t, p, target).Phase, "the failure is recorded at its completion")
			for _, c := range recordedContext(t, p, target).Containers {
				assert.Empty(t, c.PreviousReason, "a running sidecar's crash is its last termination")
			}

			<-p.SnapshotScaleSetWorker(ctx, nil, finished, false)
			got := recordedContext(t, p, target)
			assert.Equal(t, string(finished.Status.Phase), got.Phase)
			want := snapshotOf(finished, end)
			require.Len(t, got.Containers, len(want.Containers))
			for i, c := range want.Containers {
				g := got.Containers[i]
				assert.Equal(t, []any{c.Name, c.Reason, c.ExitCode, c.PreviousReason, c.PreviousExitCode},
					[]any{g.Name, g.Reason, g.ExitCode, g.PreviousReason, g.PreviousExitCode})
			}
			var events []string
			for range tc.wantEvents {
				events = append(events, "PlatformFaultOnFailedJob")
			}
			assert.Equal(t, events, target.events)

			<-p.SnapshotScaleSetWorker(ctx, nil, finished, false)
			assert.Empty(t, p.workerSnapshots.byID, "a finished worker whose job completed leaves nothing behind")
			assert.Empty(t, p.workerSnapshots.recorded)
		})
	}
}

// TestFlakyJobs_ACrashTheFinishedStatusDroppedIsReportedOnce: the kubelet's last status
// for a restarted sidecar holds its shutdown alone, so the crash comes from an earlier
// read of the pod, the completion's or one after it.
func TestFlakyJobs_ACrashTheFinishedStatusDroppedIsReportedOnce(t *testing.T) {
	before := time.Now().Add(-time.Minute).Truncate(time.Second)
	crashed := scaleSetWorker("runner-1", corev1.PodRunning, "")
	crashed.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "dind", RestartCount: 1,
		State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		LastTerminationState: terminated("dind", "OOMKilled", 137, before).State}}
	finished := finishWorker(crashed, time.Now())
	finished.Status.InitContainerStatuses = []corev1.ContainerStatus{terminated("dind", "Error", 143, time.Now())}
	finished.Status.InitContainerStatuses[0].RestartCount = 1
	for _, seen := range []string{"at the completion", "while the completion was recorded", "after the completion",
		"after the completion on a worker being deleted", "at the completion of a worker that finished while it was recorded",
		"at the completion of a worker whose finish is in the cache alone"} {
		t.Run("crash seen "+seen, func(t *testing.T) {
			ctx := context.Background()
			running := scaleSetWorker("runner-1", corev1.PodRunning, "")
			if strings.HasPrefix(seen, "at the completion") {
				running = crashed.DeepCopy()
			}
			var p *Provisioner
			c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(running).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					pod := crashed.DeepCopy()
					switch seen {
					case "while the completion was recorded":
						require.NoError(t, c.Status().Update(ctx, pod))
					case "at the completion of a worker that finished while it was recorded":
						pod = finished.DeepCopy()
						delete(pod.Annotations, AnnotationJobCompletedAt)
						p.SnapshotScaleSetWorker(ctx, nil, pod, false)
					case "at the completion of a worker whose finish is in the cache alone":
						require.NoError(t, c.Status().Update(ctx, finished.DeepCopy()))
					}
					return c.Create(ctx, obj, opts...)
				},
			}).Build()
			p = flakyProvisioner(c)
			target := &stubTarget{key: client.ObjectKey{Namespace: "team-a", Name: "gpu"}}
			completeJob(ctx, t, p, target, buildJob(1), "failed")
			switch {
			case strings.HasPrefix(seen, "after the completion"):
				pod := crashed.DeepCopy()
				if strings.HasSuffix(seen, "being deleted") {
					pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
				}
				<-p.SnapshotScaleSetWorker(ctx, nil, pod, false)
				fallthrough
			case !strings.Contains(seen, "of a worker"):
				<-p.SnapshotScaleSetWorker(ctx, nil, finished, false)
			}

			require.Equal(t, "Succeeded", recordedContext(t, p, target).Phase)
			dind := recordedContext(t, p, target).Containers[0]
			assert.Equal(t, []any{"dind", "Error", int32(143), "OOMKilled", int32(137)},
				[]any{dind.Name, dind.Reason, dind.ExitCode, dind.PreviousReason, dind.PreviousExitCode})
			assert.Equal(t, []string{"PlatformFaultOnFailedJob"}, target.events)
			assert.Empty(t, p.workerSnapshots.recorded)
		})
	}
}

// TestCarryTerminations: a later read keeps what its own status still shows, and gains
// only what an earlier read's dated terminations add.
func TestCarryTerminations(t *testing.T) {
	t0 := time.Now()
	at := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }
	for _, tc := range []struct {
		name                 string
		earlier, later, want containerSnapshot
	}{
		{"a crash the shutdown replaced",
			containerSnapshot{Name: "dind", Reason: "OOMKilled", ExitCode: 137, finishedAt: at(1)},
			containerSnapshot{Name: "dind", Reason: "Error", ExitCode: 143, finishedAt: at(2)},
			containerSnapshot{Name: "dind", Reason: "Error", ExitCode: 143, finishedAt: at(2),
				PreviousReason: "OOMKilled", PreviousExitCode: 137, previousAt: at(1)}},
		{"a crash carried once already",
			containerSnapshot{Name: "dind", Reason: "Error", ExitCode: 143, finishedAt: at(2),
				PreviousReason: "OOMKilled", PreviousExitCode: 137, previousAt: at(1)},
			containerSnapshot{Name: "dind", Reason: "Error", ExitCode: 143, finishedAt: at(2)},
			containerSnapshot{Name: "dind", Reason: "Error", ExitCode: 143, finishedAt: at(2),
				PreviousReason: "OOMKilled", PreviousExitCode: 137, previousAt: at(1)}},
		{"a second crash the status still shows",
			containerSnapshot{Name: "dind", Reason: "OOMKilled", ExitCode: 137, finishedAt: at(1)},
			containerSnapshot{Name: "dind", Reason: "Error", ExitCode: 143, finishedAt: at(3),
				PreviousReason: "Error", PreviousExitCode: 42, previousAt: at(2)},
			containerSnapshot{Name: "dind", Reason: "Error", ExitCode: 143, finishedAt: at(3),
				PreviousReason: "Error", PreviousExitCode: 42, previousAt: at(2)}},
		{"a crash a running container's status dropped",
			containerSnapshot{Name: "dind", Reason: "OOMKilled", ExitCode: 137, finishedAt: at(1)},
			containerSnapshot{Name: "dind", RestartCount: 1},
			containerSnapshot{Name: "dind", RestartCount: 1, Reason: "OOMKilled", ExitCode: 137, finishedAt: at(1)}},
		{"a termination with no time",
			containerSnapshot{Name: "dind", Reason: "ContainerStatusUnknown", ExitCode: 137},
			containerSnapshot{Name: "dind", Reason: "Error", ExitCode: 143, finishedAt: at(2)},
			containerSnapshot{Name: "dind", Reason: "Error", ExitCode: 143, finishedAt: at(2)}},
		{"another container's",
			containerSnapshot{Name: "other", Reason: "OOMKilled", ExitCode: 137, finishedAt: at(1)},
			containerSnapshot{Name: "dind", Reason: "Error", ExitCode: 143, finishedAt: at(2)},
			containerSnapshot{Name: "dind", Reason: "Error", ExitCode: 143, finishedAt: at(2)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &workerSnapshot{Containers: []containerSnapshot{tc.later}}
			s.carryTerminations(&workerSnapshot{Containers: []containerSnapshot{tc.earlier}})
			assert.Equal(t, tc.want, s.Containers[0])
		})
	}
}

// TestFlakyJobs_AFinishedWorkersRecordKeepsTheCompletionsProxy: the proxy's readiness
// when the job ended is the one that can have failed it, not the one when its worker
// finished.
func TestFlakyJobs_AFinishedWorkersRecordKeepsTheCompletionsProxy(t *testing.T) {
	ctx := context.Background()
	c, p, stub := flakyFixture(t)
	proxy := func(ready string, replicas int32) *ProxySnapshot {
		return &ProxySnapshot{Name: "ep", Mode: ProxyModeColocated, Ready: ready, ReadyReplicas: &replicas}
	}
	running := scaleSetWorker("runner-1", corev1.PodRunning, "")
	require.NoError(t, c.Create(ctx, running))
	completeJob(ctx, t, p, proxyTarget{stub, staticProxy{proxy("False", 0)}}, buildJob(1), "failed")
	<-p.SnapshotScaleSetWorker(ctx, staticProxy{proxy("True", 2)}, finishWorker(running, time.Now()), false)

	got := recordedContext(t, p, stub)
	assert.Equal(t, "Succeeded", got.Phase)
	assert.Equal(t, proxy("False", 0), got.Proxy)
	assert.Equal(t, []string{"PlatformFaultOnFailedJob"}, stub.events, "the unready pool is reported once")
}

// TestFlakyJobs_AKilledWorkersSnapshotWaitsForItsCompletion: a kill that ends the runner
// inverts the order, the completion arriving when GitHub's job lock lapses, after the
// pod is reaped.
func TestFlakyJobs_AKilledWorkersSnapshotWaitsForItsCompletion(t *testing.T) {
	ctx := context.Background()
	_, p, target := flakyFixture(t)
	killed := scaleSetWorker("runner-1", corev1.PodFailed, "")
	killed.Status.ContainerStatuses = []corev1.ContainerStatus{terminated(runnerContainer, "OOMKilled", 137, time.Now())}

	<-p.SnapshotScaleSetWorker(ctx, nil, killed, false)
	completeJob(ctx, t, p, target, buildJob(1), "failed")

	got := recordedContext(t, p, target)
	assert.Equal(t, "Failed", got.Phase)
	assert.Equal(t, "OOMKilled", got.Containers[0].Reason)
	assert.Equal(t, []string{"PlatformFaultOnFailedJob"}, target.events)
	assert.Empty(t, p.workerSnapshots.byID)
	assert.Empty(t, p.workerSnapshots.recorded)
}

// TestFlakyJobs_AWorkerFinishingDuringTheRecordCompletesIt: the terminal transition can
// land between the completion's read of the pod and its ledger write.
func TestFlakyJobs_AWorkerFinishingDuringTheRecordCompletesIt(t *testing.T) {
	ctx := context.Background()
	running := scaleSetWorker("runner-1", corev1.PodRunning, "")
	var p *Provisioner
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(running).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			finished := finishWorker(running, time.Now())
			delete(finished.Annotations, AnnotationJobCompletedAt)
			p.SnapshotScaleSetWorker(ctx, nil, finished, false)
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
	p = flakyProvisioner(c)
	target := &stubTarget{key: client.ObjectKey{Namespace: "team-a", Name: "gpu"}}

	completeJob(ctx, t, p, target, buildJob(1), "failed")
	assert.Equal(t, "Succeeded", recordedContext(t, p, target).Phase)
	assert.Empty(t, p.workerSnapshots.byID)
	assert.Empty(t, p.workerSnapshots.recorded)
}

// TestFlakyJobs_AFinishedWorkerLeavesOtherRecordsAlone: a later attempt's worker is
// reported but does not replace the first failure's context, and a record paired while
// its worker shut down is not brought back.
func TestFlakyJobs_AFinishedWorkerLeavesOtherRecordsAlone(t *testing.T) {
	ctx := context.Background()
	c, p, target := flakyFixture(t)
	workers := map[int]*corev1.Pod{}
	for n := 1; n <= 2; n++ {
		workers[n] = scaleSetWorker(fmt.Sprintf("runner-%d", n), corev1.PodRunning, "")
		require.NoError(t, c.Create(ctx, workers[n]))
		completeJob(ctx, t, p, target, buildJob(n), "failed")
	}

	second := finishWorker(workers[2], time.Now())
	second.Status.InitContainerStatuses = []corev1.ContainerStatus{terminated("dind", "OOMKilled", 137, time.Now().Add(-time.Minute))}
	<-p.SnapshotScaleSetWorker(ctx, nil, second, false)
	assert.Equal(t, "Running", recordedContext(t, p, target).Phase, "the first failure's context is kept")
	assert.Equal(t, "worker-runner-1", recordedContext(t, p, target).Pod)
	assert.Equal(t, []string{"PlatformFaultOnFailedJob"}, target.events, "the later attempt's fault is reported")

	completeJob(ctx, t, p, target, buildJob(3), jobResultSucceeded)
	<-p.SnapshotScaleSetWorker(ctx, nil, finishWorker(workers[1], time.Now()), false)
	assert.Empty(t, readFailures(t, p, target), "a paired record is not brought back")
}

// TestFlakyJobs_ACompletedJobsWorkerIsNotSnapshotted: nothing will take the snapshot of
// a worker whose own job has completed, and a surplus stamp is another job's.
func TestFlakyJobs_ACompletedJobsWorkerIsNotSnapshotted(t *testing.T) {
	ctx := context.Background()
	c, p, target := flakyFixture(t)
	green := scaleSetWorker("runner-1", corev1.PodRunning, "")
	require.NoError(t, c.Create(ctx, green))
	completeJob(ctx, t, p, target, buildJob(1), jobResultSucceeded)
	<-p.SnapshotScaleSetWorker(ctx, nil, finishWorker(green, time.Now()), false)
	assert.Empty(t, p.workerSnapshots.byID)

	surplus := finishWorker(scaleSetWorker("runner-2", corev1.PodRunning, ""), time.Now())
	surplus.Annotations[AnnotationSurplusForJob] = "job-9"
	<-p.SnapshotScaleSetWorker(ctx, nil, surplus, false)
	assert.Contains(t, p.workerSnapshots.byID, "team-a/runner-2", "its runner may have taken another job")
}

// TestRerunLedger_CapEvictsFailuresBeforeBudgets: past half the cap the oldest failure
// records go, and the budget the write was for is reserved.
func TestRerunLedger_CapEvictsFailuresBeforeBudgets(t *testing.T) {
	ctx := context.Background()
	c, p, target := flakyFixture(t)
	now := p.nowFn().UTC()
	l := &rerunLedger{Version: rerunLedgerVersion, MinWriterVersion: rerunLedgerMinWriterVersion,
		Reruns: map[string]*rerunRecord{"77": {ExpiresAt: now.Add(time.Hour), Count: 1}}, Failures: map[string]*failureRecord{}}
	padding := json.RawMessage(`"` + strings.Repeat("x", rerunLedgerMaxBytes/4) + `"`)
	for i := range 4 {
		l.Failures[fmt.Sprintf("1/job-%d", i)] = &failureRecord{ExpiresAt: now.Add(time.Hour),
			FailedAt: now.Add(time.Duration(i) * time.Minute), Context: padding}
	}
	b, err := json.Marshal(l)
	require.NoError(t, err)
	writeLedger(t, c, target, string(b))

	outcome, _, err := p.reserveScaleSetRerun(ctx, target, "78", &rerunDisruption{key: "pod-a", failedAttempt: 1}, recoveryCauseEviction, 2, nil)
	require.NoError(t, err)
	assert.Equal(t, rerunReserved, outcome)
	got, _, err := p.readRerunLedger(ctx, target)
	require.NoError(t, err)
	assert.Contains(t, got.Reruns, "77")
	assert.Contains(t, got.Reruns, "78")
	for _, k := range []string{"1/job-0", "1/job-1", "1/job-2"} {
		assert.NotContains(t, got.Failures, k, "the oldest failures are evicted")
	}
	assert.Contains(t, got.Failures, "1/job-3", "only as many as the ceiling needs")
}

// TestRerunLedger_FailuresLeaveHalfTheCapToAnOlderWriter pins the ceiling v1.10.0
// needs: it keeps failure records without understanding them, cannot evict them, and
// refuses a write past the cap, so failures past half the cap would cost it its
// re-run reservations.
func TestRerunLedger_FailuresLeaveHalfTheCapToAnOlderWriter(t *testing.T) {
	ctx := context.Background()
	c, p, target := flakyFixture(t)
	snap := json.RawMessage(`"` + strings.Repeat("c", 1400) + `"`)
	require.NoError(t, p.updateRerunLedger(ctx, target, func(l *rerunLedger, now time.Time) bool {
		for i := range 400 {
			l.Failures[fmt.Sprintf("1/job-%03d", i)] = &failureRecord{ExpiresAt: now.Add(time.Hour),
				FailedAt: now.Add(time.Duration(i) * time.Second), Context: snap}
		}
		return true
	}))
	data := readLedgerData(t, c, target)
	assert.LessOrEqual(t, len(data), rerunLedgerMaxBytes/2, "an older writer keeps at least half the cap")
	got, _, err := p.readRerunLedger(ctx, target)
	require.NoError(t, err)
	assert.NotContains(t, got.Failures, "1/job-000")
	assert.Contains(t, got.Failures, "1/job-399", "the newest failures are kept")
	t.Logf("%d of 400 failures kept in %d bytes", len(got.Failures), len(data))
}

// TestRerunLedger_CapHoldsEnoughFailures measures how many failure records fit under
// their ceiling, half the cap, each with a snapshot of a typical worker: phase, reason, a truncated
// message, five conditions, and a runner and two sidecars.
func TestRerunLedger_CapHoldsEnoughFailures(t *testing.T) {
	const sample = 100
	pod := scaleSetWorker(strings.Repeat("r", 63), corev1.PodFailed, "Evicted")
	pod.Status.Message = strings.Repeat("m", 1000)
	pod.Spec.NodeName = strings.Repeat("n", 63)
	for _, c := range []corev1.PodConditionType{corev1.PodScheduled, corev1.PodInitialized, corev1.ContainersReady, corev1.PodReady, corev1.DisruptionTarget} {
		pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{Type: c, Status: corev1.ConditionFalse, Reason: "TerminationByKubelet"})
	}
	for _, name := range []string{"egress-proxy", "registry-mirror"} {
		pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses, corev1.ContainerStatus{Name: name,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 143}}})
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: runnerContainer, RestartCount: 0,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}}}
	snap, err := json.Marshal(snapshotOf(pod, time.Now()))
	require.NoError(t, err)

	l := &rerunLedger{Version: rerunLedgerVersion, MinWriterVersion: rerunLedgerMinWriterVersion, Failures: map[string]*failureRecord{}}
	for i := range sample {
		l.Failures[fmt.Sprintf("%d/%s", 20_000_000_000+i, strings.Repeat("j", 100))] = &failureRecord{
			ExpiresAt: time.Now(), FailedAt: time.Now(), JobID: "6f1c0000-0000-4000-8000-000000000000", Cause: recoveryCauseEviction,
			Event: "pull_request", WorkflowRef: strings.Repeat("w", 120), Runner: strings.Repeat("r", 63), Context: snap}
	}
	b, err := json.Marshal(l)
	require.NoError(t, err)
	fit := failureRecordsCeiling * sample / len(b)
	t.Logf("%d bytes per failure, %d failures fit", len(b)/sample, fit)
	assert.GreaterOrEqual(t, fit, 150)
}

// scaleSetWorker is a scale-set worker pod for runner, carrying run 900's identity.
func scaleSetWorker(runner string, phase corev1.PodPhase, reason string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "worker-" + runner,
			Labels:      map[string]string{LabelAcquisitionProtocol: AcquisitionProtocolScaleSet, LabelRunnerSet: "gpu"},
			Annotations: map[string]string{AnnotationRunnerName: runner, AnnotationRunID: "900", annotationJobName: "build"}},
		Status: corev1.PodStatus{Phase: phase, Reason: reason},
	}
}
