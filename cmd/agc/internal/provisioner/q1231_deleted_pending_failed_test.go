package provisioner_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Q1231. A real kubelet deleting a worker still waiting on its image publishes PodFailed
// with the deletion mark before the object goes, and rewrites the waiting container as
// Terminated/ContainerStatusUnknown with no start, no exit and no container ID. The
// capture is testdata/deleted_pending_pod_failed.json, taken on kind v1.36.1 with a
// plain graceful delete, which is what the reaper issues. The classic waiter resolves on
// that Failed, so it has to read the pod as never started there, or the job is reported
// failed and its run is left for GitHub's ~15-minute unstarted-job timeout (Q683).
func TestProvisioner_Q1231_KubeletFailedOnDeletedPendingWorkerForceCancelsRun(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	var captured corev1.Pod
	raw, err := os.ReadFile("testdata/deleted_pending_pod_failed.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &captured))
	require.Equal(t, corev1.PodFailed, captured.Status.Phase, "the capture is the kubelet's terminal publish")
	require.NotNil(t, captured.DeletionTimestamp, "the capture carries the deletion mark")

	var forceCancels atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r/actions/runs/4242/force-cancel" {
			forceCancels.Add(1)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	p.GitHubAPIURL = srv.URL
	p.TokenFunc = func(context.Context) (string, error) { return "tok-q1231", nil }
	rg := newRG("mygroup", "team-a")

	type result struct {
		result broker.TaskResult
		err    error
	}
	done := make(chan result, 1)
	go func() {
		r, err := rg.handler(p)(ctx, "", "plan-q1231", stubPayloadFull("o", "r", 4242), "")
		done <- result{r, err}
	}()

	pod := waitForPodCreated(ctx, t, fc, "team-a")
	// Held by a finalizer so the delete leaves the object readable with its mark, as the
	// kubelet's grace period does, then given the kubelet's terminal status.
	pod.Finalizers = []string{"test.actions-gateway.com/hold"}
	require.NoError(t, fc.Update(ctx, pod))
	require.NoError(t, fc.Delete(ctx, pod))
	require.NoError(t, fc.Get(ctx, client.ObjectKeyFromObject(pod), pod))
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = captured.Status.ContainerStatuses
	require.NoError(t, fc.Status().Update(ctx, pod))

	got := <-done
	require.NoError(t, got.err)
	assert.Equal(t, broker.TaskResultAbandoned, got.result,
		"a worker deleted before it ran is abandoned, whether the waiter saw the delete or the kubelet's Failed first")
	assert.Equal(t, int64(1), forceCancels.Load(), "the abandoned job's run is force-cancelled")
}
