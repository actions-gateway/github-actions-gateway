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

// Q628. A worker pod that never leaves Pending is deleted by the reaper at
// spec.pendingPodDeadline, and the session goroutine must not report the job it
// never ran as a success — that concluded the assignment and left the workflow job
// queued at GitHub with no runner ever registered. Measured on the v1.3.0-rc.5
// dogfood gate, where three workers were reaped this way and the listener then went
// silent for 16 minutes with capacity available.
//
// The mapping under test is provisioner.go's pod-phase proxy. abandoned, not failed:
// no step ran, so there is no failure to report — and it is the value the listener
// keys its own assignment release on.
func TestProvisioner_Q628_ReapedPendingWorkerReportsAbandoned(t *testing.T) {
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

	assert.Equal(t, broker.TaskResultAbandoned, got.result,
		"Q628: a worker reaped while Pending ran no step, so the job is abandoned — "+
			"reporting it succeeded concluded an assignment whose job never ran")
}
