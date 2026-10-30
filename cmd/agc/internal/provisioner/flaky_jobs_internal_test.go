package provisioner

import (
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
			[]string{"namespace", "runner_group", "tier", "cause", "event", "result"}),
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

// flakyProvisioner is one AGC process; two on one client are a restart.
func flakyProvisioner(c client.Client) *Provisioner {
	return NewProvisioner(c, flakyMetrics(), nil)
}

// buildJob is attempt n of job "build" in run 900, triggered by a push.
func buildJob(n int) ScaleSetJob {
	return ScaleSetJob{
		JobID: fmt.Sprintf("job-%d", n), RunnerName: fmt.Sprintf("runner-%d", n),
		Owner: "org", Repository: "repo", RunID: "900", JobName: "build",
		EventName: "push", WorkflowRef: "org/repo/.github/workflows/ci.yml@refs/heads/main",
	}
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
	return testutil.ToFloat64(p.Metrics.EvictionRerunOutcomes.WithLabelValues("team-a", "gpu", evictionTierScaleSet, cause, "push", result))
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
		// attempt is GAG's re-run of the run, nil for none.
		attempt *rerunAttempt
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
		{name: "GAG's single-job re-run of another job did not carry it",
			attempt:     &rerunAttempt{Scope: rerunScopeJob, Cause: recoveryCauseDeletion, Jobs: []string{"lint"}},
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
						Attempts: map[string]*rerunAttempt{"1": tc.attempt}}
					return true
				}))
			}
			if tc.claim != "" {
				claimDisruption(t, p, target, "job-1", tc.claim)
			}

			require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, buildJob(1), "failed"))
			require.Contains(t, readFailures(t, p, target), "900/build", "the failure is recorded")
			require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, buildJob(2), jobResultSucceeded))

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
	p.SnapshotScaleSetWorker(scaleSetWorker("runner-1", corev1.PodFailed, "Evicted"))

	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, buildJob(1), "failed"))
	first := readFailures(t, p, target)["900/build"]
	require.NotNil(t, first)
	require.Contains(t, string(first.Context), "Evicted")

	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, buildJob(2), "failed"))
	f := readFailures(t, p, target)["900/build"]
	require.NotNil(t, f, "a failed later attempt keeps the record")
	assert.Equal(t, "job-2", f.JobID)
	assert.Empty(t, f.Cause, "the second failure had no disruption")
	assert.Equal(t, first.Context, f.Context, "the first failure's context is kept")
	assert.Equal(t, first.FailedAt, f.FailedAt)
	assert.Equal(t, []string{"1"}, f.PairedAttempts)
	assert.Equal(t, 1.0, outcome(p, recoveryCauseEviction, "failed"))
	assert.Equal(t, 1.0, rerunCount(p, rerunTriggerGAG))

	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, buildJob(3), jobResultSucceeded))
	assert.Equal(t, 1.0, rerunCount(p, rerunTriggerOther), "GAG's re-run was attributed already")
	assert.Equal(t, 1.0, flaky(p, rerunTriggerOther))
	assert.Equal(t, 1.0, outcome(p, recoveryCauseEviction, "failed"), "a human's re-run is not GAG's outcome")
	assert.Zero(t, outcome(p, recoveryCauseEviction, jobResultSucceeded))
}

// TestFlakyJobs_AReplayedCompletionChangesNothing replays each completion, in-process
// and after a restart, as a re-created session does from cursor 0.
func TestFlakyJobs_AReplayedCompletionChangesNothing(t *testing.T) {
	ctx := context.Background()
	c, p, target := flakyFixture(t)

	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, buildJob(1), "failed"))
	before := readLedgerData(t, c, target)
	require.NoError(t, flakyProvisioner(c).RecordScaleSetJobCompleted(ctx, target, buildJob(1), "failed"))
	assert.Equal(t, before, readLedgerData(t, c, target), "a replayed failure rewrites nothing")

	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, buildJob(2), jobResultSucceeded))
	restarted := flakyProvisioner(c)
	require.NoError(t, restarted.RecordScaleSetJobCompleted(ctx, target, buildJob(2), jobResultSucceeded))
	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, buildJob(2), jobResultSucceeded))
	assert.Equal(t, 1.0, rerunCount(p, rerunTriggerOther))
	assert.Zero(t, rerunCount(restarted, rerunTriggerOther), "a replayed success pairs nothing")
}

// TestFlakyJobs_AUsersCancelRecordsNothing: a cancelled completion is a failure only
// where a disruption claim names it, since GAG's own force-cancel after a deletion
// concludes the job cancelled.
func TestFlakyJobs_AUsersCancelRecordsNothing(t *testing.T) {
	ctx := context.Background()
	_, p, target := flakyFixture(t)
	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, buildJob(1), jobResultCanceled))
	assert.Empty(t, readFailures(t, p, target))

	claimDisruption(t, p, target, "job-1", recoveryCauseDeletion)
	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, buildJob(1), jobResultCanceled))
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

	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, buildJob(1), jobResultSucceeded))
	require.Equal(t, 1, gets, "the first completion loads the ledger")
	for n := 2; n < 10; n++ {
		job := buildJob(n)
		job.JobName = fmt.Sprintf("test-%d", n)
		require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, job, jobResultSucceeded))
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
	p.SnapshotScaleSetWorker(oom)
	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, buildJob(1), "failed"))
	assert.Equal(t, []string{"PlatformFaultOnFailedJob"}, target.events)
	var snap workerSnapshot
	require.NoError(t, json.Unmarshal(readFailures(t, p, target)["900/build"].Context, &snap))
	assert.Equal(t, "OOMKilled", snap.Containers[0].Reason)

	red := scaleSetWorker("runner-x", corev1.PodSucceeded, "")
	red.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: runnerContainer,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}}}}
	red.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "proxy-sidecar",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 143}}}}
	p.SnapshotScaleSetWorker(red)
	job := buildJob(1)
	job.JobID, job.RunnerName, job.JobName = "job-x", "runner-x", "test"
	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, job, "failed"))
	assert.Equal(t, []string{"PlatformFaultOnFailedJob"}, target.events, "a red test on a healthy worker is not a fault")
}

// TestFlakyJobs_ACompletionWithNoIdentityTakesThePods: the run and job come from the
// worker's annotations where the completion carries neither.
func TestFlakyJobs_ACompletionWithNoIdentityTakesThePods(t *testing.T) {
	ctx := context.Background()
	_, p, target := flakyFixture(t)
	p.SnapshotScaleSetWorker(scaleSetWorker("runner-1", corev1.PodSucceeded, ""))
	require.NoError(t, p.RecordScaleSetJobCompleted(ctx, target, ScaleSetJob{JobID: "job-1", RunnerName: "runner-1"}, "failed"))
	assert.Contains(t, readFailures(t, p, target), "900/build")
}

// TestFlakyJobs_SnapshotsOnlyTerminalTransitions: a snapshot is taken at a terminal
// phase or a deletion of a pod that never reached one, and never for a classic worker.
func TestFlakyJobs_SnapshotsOnlyTerminalTransitions(t *testing.T) {
	now := metav1.Now()
	for _, tc := range []struct {
		name string
		pod  func() *corev1.Pod
		want bool
	}{
		{"running", func() *corev1.Pod { return scaleSetWorker("r", corev1.PodRunning, "") }, false},
		{"succeeded", func() *corev1.Pod { return scaleSetWorker("r", corev1.PodSucceeded, "") }, true},
		{"failed", func() *corev1.Pod { return scaleSetWorker("r", corev1.PodFailed, "") }, true},
		{"deleted while running", func() *corev1.Pod {
			pod := scaleSetWorker("r", corev1.PodRunning, "")
			pod.DeletionTimestamp = &now
			return pod
		}, true},
		{"reaped after finishing", func() *corev1.Pod {
			pod := scaleSetWorker("r", corev1.PodSucceeded, "")
			pod.DeletionTimestamp = &now
			return pod
		}, false},
		{"classic worker", func() *corev1.Pod {
			pod := scaleSetWorker("r", corev1.PodFailed, "")
			delete(pod.Labels, LabelAcquisitionProtocol)
			return pod
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := flakyProvisioner(nil)
			p.SnapshotScaleSetWorker(tc.pod())
			assert.Equal(t, tc.want, p.workerSnapshots.take("team-a/r") != nil)
		})
	}
}

func TestFlakyJobs_SnapshotStoreIsBounded(t *testing.T) {
	var w workerSnapshots
	base := time.Now()
	for i := range maxWorkerSnapshots + 1 {
		w.put(fmt.Sprint(i), &workerSnapshot{takenAt: base.Add(time.Duration(i) * time.Second)})
	}
	assert.Len(t, w.byID, maxWorkerSnapshots)
	assert.Nil(t, w.take("0"), "the oldest snapshot is evicted")
	assert.NotNil(t, w.take(fmt.Sprint(maxWorkerSnapshots)))
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

	outcome, _, err := p.reserveScaleSetRerun(ctx, target, "78", rerunDisruption{key: "pod-a", failedAttempt: 1}, recoveryCauseEviction, 2)
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
			Event: "pull_request", WorkflowRef: strings.Repeat("w", 120), PairedAttempts: []string{"1", "2"}, Context: snap}
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
