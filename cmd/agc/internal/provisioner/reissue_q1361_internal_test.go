package provisioner

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TestReissueReplacement pins which worker counts as GitHub's re-issue of another's job:
// one created later carrying the same run ID and job name, for a worker whose
// containers never started.
func TestReissueReplacement(t *testing.T) {
	base := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	worker := func(name string, created time.Duration, runID, job string) corev1.Pod {
		p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: name, UID: types.UID(name),
			CreationTimestamp: metav1.NewTime(base.Add(created)),
			Annotations:       map[string]string{},
		}}
		if runID != "" {
			p.Annotations[AnnotationRunID] = runID
		}
		if job != "" {
			p.Annotations[annotationJobName] = job
		}
		return p
	}
	stuck := worker("worker-stuck", 0, "4242", "build")
	reissue := worker("worker-reissue", 5*time.Minute, "4242", "build")
	older := worker("worker-older", -time.Minute, "4242", "build")
	otherJob := worker("worker-other-job", 5*time.Minute, "4242", "test")
	otherRun := worker("worker-other-run", 5*time.Minute, "4343", "build")
	deleting := worker("worker-deleting", 5*time.Minute, "4242", "build")
	deleting.DeletionTimestamp = &metav1.Time{Time: base.Add(6 * time.Minute)}

	assert.Equal(t, "worker-reissue", ReissueReplacement(&stuck, []corev1.Pod{stuck, older, otherJob, otherRun, deleting, reissue}),
		"a later worker for the same run and job is the re-issue's")
	assert.Empty(t, ReissueReplacement(&stuck, []corev1.Pod{stuck, older, otherJob, otherRun, deleting}),
		"an earlier, a different-job, a different-run and a terminating worker are none of them a re-issue")

	started := stuck
	started.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  WorkerContainerName,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(base)}},
	}}
	assert.Empty(t, ReissueReplacement(&started, []corev1.Pod{started, reissue}),
		"a worker that started is not one GitHub gave up waiting on")

	noIdentity := worker("worker-no-identity", 0, "", "")
	assert.Empty(t, ReissueReplacement(&noIdentity, []corev1.Pod{noIdentity, worker("w2", 5*time.Minute, "", "")}),
		"workers with no run identity are never tied to each other")
}
