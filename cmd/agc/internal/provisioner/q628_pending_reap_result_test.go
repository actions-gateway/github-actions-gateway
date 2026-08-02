package provisioner_test

import (
	"context"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Q628 characterisation. A worker pod that never leaves Pending is deleted by the
// reaper at spec.pendingPodDeadline. This pins what the session goroutine then
// reports for a job that never ran a single step.
//
// This asserts CURRENT behaviour, not desired behaviour: the job is reported to
// the listener as SUCCEEDED, so the assignment is concluded and never re-offered,
// while the workflow job is still queued at GitHub with no runner ever registered.
// Measured on the v1.3.0-rc.5 dogfood gate, where three workers were reaped this
// way and the listener then went silent for 16 minutes with capacity available.
//
// The mapping under test is provisioner.go's pod-phase proxy: anything that is not
// PodFailed becomes TaskResultSucceeded, and every disruption-recovery arm requires
// PodFailed, so a reaped Pending worker matches none of them. When the fix lands,
// this test flips — a job whose worker never started is not a success.
func TestProvisioner_Q628_ReapedPendingWorkerReportsSucceeded(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
	p := newProvisioner(fc)
	rg := newRG("mygroup", "team-a")

	type result struct {
		result broker.TaskResult
		err    error
	}
	done := make(chan result, 1)
	go func() {
		r, err := p.HandlerFor(rg)(ctx, "", "plan-q628", stubPayload(1), "")
		done <- result{r, err}
	}()

	pod := waitForPodCreated(ctx, t, fc, "team-a")

	// Hold it Pending, as an unschedulable worker is: it never scheduled, so no
	// container ever ran. Then delete it — the reaper's pending_deadline delete,
	// which resolves the waiting session through the same path as any external
	// deletion, and is explicitly named as such in InformerPodWaiter.onPodDelete.
	pod.Status.Phase = corev1.PodPending
	require.NoError(t, fc.Status().Update(ctx, pod))
	require.NoError(t, fc.Delete(ctx, pod))

	got := <-done
	require.NoError(t, got.err, "the session must not surface an error for a reaped worker")

	assert.Equal(t, broker.TaskResultSucceeded, got.result,
		"Q628: a worker reaped while Pending is reported as a SUCCEEDED job, so the "+
			"listener concludes an assignment whose job never ran")
}
