package provisioner_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Q683. A worker reaped while Pending leaves a job nothing will ever report, and no
// completejob value ends it honestly (Q645/Q676) — told nothing, GitHub cancels run
// and job at its ~15-minute unstarted-job timeout. Measured live 2026-08-05 (the
// Q645 plan doc): a standalone REST force-cancel in that state is accepted 202 and
// concludes run AND job as cancelled in about a second, so the provisioner issues
// it before reporting abandoned. Identity comes from the acquire payload's github
// context; without it the call is skipped and the timeout stays the backstop.
func TestProvisioner_Q683_ReapedPendingWorkerForceCancelsRun(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name             string
		payload          []byte
		wantForceCancels int64
		wantPath         string
	}{
		{
			name:             "full identity force-cancels the run",
			payload:          stubPayloadFull("o", "r", 4242),
			wantForceCancels: 1,
			wantPath:         "/repos/o/r/actions/runs/4242/force-cancel",
		},
		{
			name: "identity unknown skips the call",
			// Top-level run_id only: repoInfo resolves no owner/repo, so there is
			// no endpoint to address and the unstarted-job timeout is the ending.
			payload:          stubPayload(1),
			wantForceCancels: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var forceCancels atomic.Int64
			var gotPath, gotAuth atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				forceCancels.Add(1)
				gotPath.Store(r.URL.Path)
				gotAuth.Store(r.Header.Get("Authorization"))
				w.WriteHeader(http.StatusAccepted)
			}))
			t.Cleanup(srv.Close)

			fc := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&corev1.Pod{}).Build()
			p := newProvisioner(fc)
			p.GitHubAPIURL = srv.URL
			p.TokenFunc = func(context.Context) (string, error) { return "tok-q683", nil }
			rg := newRG("mygroup", "team-a")

			type result struct {
				result broker.TaskResult
				err    error
			}
			done := make(chan result, 1)
			go func() {
				r, err := p.HandlerFor(rg)(ctx, "", "plan-q683", tc.payload, "")
				done <- result{r, err}
			}()

			pod := waitForPodCreated(ctx, t, fc, "team-a")
			// The reaper's pending_deadline delete: held Pending, no container ever ran.
			pod.Status.Phase = corev1.PodPending
			require.NoError(t, fc.Status().Update(ctx, pod))
			require.NoError(t, fc.Delete(ctx, pod))

			got := <-done
			require.NoError(t, got.err)
			assert.Equal(t, broker.TaskResultAbandoned, got.result,
				"the pod-phase proxy for a reaped-Pending worker stays abandoned (Q628)")

			assert.Equal(t, tc.wantForceCancels, forceCancels.Load(), "force-cancel calls")
			if tc.wantForceCancels > 0 {
				assert.Equal(t, tc.wantPath, gotPath.Load(),
					"the run named by the payload's github context must be the one cancelled")
				assert.Equal(t, "token tok-q683", gotAuth.Load(),
					"the call carries the installation token")
			}
		})
	}
}
