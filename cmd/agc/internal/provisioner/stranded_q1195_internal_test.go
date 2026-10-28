package provisioner

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

// Q1195: a worker reaped Pending whose own job another runner took still costs the set
// a runner, and the job left without one is a different job, named only by the
// listener's in-flight set. Recovery follows the runners to that job and recovers its
// run instead.

// inFlightTarget is a stubTarget that can list the listener's in-flight jobs.
type inFlightTarget struct {
	*stubTarget
	jobs []OrphanedWorker
}

func (t *inFlightTarget) InFlightJobs(context.Context) ([]OrphanedWorker, error) {
	return t.jobs, nil
}

// startedOn marks a minted worker's runner as having started jobID.
func startedOn(pod *corev1.Pod, jobID string) *corev1.Pod {
	pod.Annotations[AnnotationStartedJobID] = jobID
	return pod
}

func inFlight(jobID, repository, runID string) OrphanedWorker {
	owner, repo, _ := strings.Cut(repository, "/")
	return OrphanedWorker{JobID: jobID, Owner: owner, Repository: repo, RunID: runID}
}

func TestRecoverAbandonedScaleSetWorker_Q1195_RecoversTheStrandedJob(t *testing.T) {
	jobs := []OrphanedWorker{
		inFlight("job-a", "myorg/repo-a", "111"),
		inFlight("job-b", "myorg/repo-b", "222"),
		inFlight("job-c", "myorg/repo-c", "333"),
	}
	for _, tc := range []struct {
		name    string
		workers []*corev1.Pod
		want    []string
	}{
		{
			// job-a runs on job-b's worker, so job-b has no runner.
			name:    "one hop",
			workers: []*corev1.Pod{startedOn(mintedWorker("job-b", "myorg/repo-a", "111"), "job-a")},
			want:    []string{"/repos/myorg/repo-b/actions/runs/222/force-cancel"},
		},
		{
			// job-a runs on job-b's worker, job-b on job-c's: job-c has no runner.
			name: "two hops",
			workers: []*corev1.Pod{
				startedOn(mintedWorker("job-b", "myorg/repo-a", "111"), "job-a"),
				startedOn(mintedWorker("job-c", "myorg/repo-b", "222"), "job-b"),
			},
			want: []string{"/repos/myorg/repo-c/actions/runs/333/force-cancel"},
		},
		{
			// A Running worker with no start recorded may already hold job-b, its
			// JobStarted still in flight, so job-b is not named.
			name: "a start may be unrecorded",
			workers: []*corev1.Pod{
				startedOn(mintedWorker("job-b", "myorg/repo-a", "111"), "job-a"),
				mintedWorker("job-d", "myorg/repo-d", "444"),
			},
		},
		{
			// A Pending worker cannot have started anything, so it does not hold the walk.
			name: "a Pending idle worker",
			workers: []*corev1.Pod{
				startedOn(mintedWorker("job-b", "myorg/repo-a", "111"), "job-a"),
				func() *corev1.Pod {
					pod := mintedWorker("job-d", "myorg/repo-d", "444")
					pod.Status.Phase = corev1.PodPending
					return pod
				}(),
			},
			want: []string{"/repos/myorg/repo-b/actions/runs/222/force-cancel"},
		},
		{
			// The worker holding job-a was created for a job the in-flight set does not
			// carry, so the job it left is unnamed.
			name:    "holder's own job unknown",
			workers: []*corev1.Pod{startedOn(mintedWorker("job-x", "myorg/repo-a", "111"), "job-a")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAbandonedScaleSetFixture(t, http.StatusAccepted, tc.workers...)
			target := &inFlightTarget{stubTarget: f.target, jobs: jobs}

			<-f.p.RecoverAbandonedScaleSetWorker(context.Background(), target, reapedPendingWorker(scaleSetPodName("gpu", "job-a")))

			forceCancel, rerun := f.calls.snapshot()
			assert.Equal(t, tc.want, forceCancel, "only the run of the job left without a runner is cancelled; run 111 is live")
			assert.Empty(t, rerun)
		})
	}
}

// A target that cannot list the in-flight set keeps Q1169's behaviour: the reaped
// worker's own run is spared and nothing else is named.
func TestRecoverAbandonedScaleSetWorker_Q1195_NoInFlightListerSparesAll(t *testing.T) {
	f := newAbandonedScaleSetFixture(t, http.StatusAccepted,
		startedOn(mintedWorker("job-b", "myorg/repo-a", "111"), "job-a"))

	<-f.p.RecoverAbandonedScaleSetWorker(context.Background(), f.target, reapedPendingWorker(scaleSetPodName("gpu", "job-a")))

	forceCancel, _ := f.calls.snapshot()
	assert.Empty(t, forceCancel)
}
