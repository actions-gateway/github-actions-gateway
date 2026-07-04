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

// TestProvisioner_ResultPodPhaseProxy pins the Q260 Option A pod-phase proxy the
// provisioner returns for the listener's fan-out completion of a fanned-out job's
// deduped sibling deliveries: a Failed worker pod yields broker.TaskResultFailed and
// any other terminal phase yields broker.TaskResultSucceeded — the honest proxy for
// a job whose real succeeded/failed the AGC cannot observe (only the worker's runner
// binary reports it, for the winner's own delivery).
func TestProvisioner_ResultPodPhaseProxy(t *testing.T) {
	defer goleak.VerifyNone(t)
	for _, tc := range []struct {
		name  string
		phase corev1.PodPhase
		want  broker.TaskResult
	}{
		{"succeeded", corev1.PodSucceeded, broker.TaskResultSucceeded},
		{"failed", corev1.PodFailed, broker.TaskResultFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
				r, err := p.HandlerFor(rg)(ctx, "", "plan-result-"+tc.name, stubPayload(1), "")
				done <- result{r, err}
			}()

			pod := waitForPodCreated(ctx, t, fc, "team-a")
			completePod(ctx, t, fc, "team-a", pod.Name, tc.phase)
			got := <-done
			require.NoError(t, got.err)
			assert.Equal(t, tc.want, got.result,
				"the pod-phase proxy must map %s → %s", tc.phase, tc.want)
		})
	}
}
