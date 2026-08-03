package listener_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/runnercore"
	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/actions-gateway/github-actions-gateway/broker/brokertest"
	"github.com/actions-gateway/github-actions-gateway/githubapp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unrunWorkerMux builds a single-listener Multiplexer whose JobHandler reports what
// the provisioner reports when a worker pod is removed before it ever runs: abandoned,
// and no completejob of its own — the worker's runner binary never started, so nothing
// inside the pod reported anything.
func unrunWorkerMux(t *testing.T, srv *brokertest.Server, m *runnercore.Metrics, fanoutCompletion bool) *listener.Multiplexer {
	t.Helper()
	factory := func(idx int) listener.Config {
		return listener.Config{
			Group:     "test-rg",
			Namespace: "default",
			Agent: &agentpool.Agent{
				Index:         idx,
				AgentID:       42,
				RunnerVersion: "2.335.1",
				PrivateKey:    testRSAKey,
				Creds: &githubapp.RunnerCredentials{
					ClientID:         "stub-client",
					AuthorizationURL: srv.URL + "token",
				},
			},
			Broker: &broker.Client{
				BrokerURL:  srv.URL,
				UseV2Flow:  true,
				HTTPClient: srv.HTTPClient(),
			},
			HTTPClient:       srv.HTTPClient(),
			Metrics:          m,
			IdleThreshold:    1_000_000,
			RenewInterval:    time.Hour,
			FanoutCompletion: fanoutCompletion,
			JobHandler: func(context.Context, string, string, []byte, string) (broker.TaskResult, error) {
				return broker.TaskResultAbandoned, nil
			},
		}
	}
	mgr := listener.NewMultiplexer(factory, 1, nil)
	mgr.RestartDelay = time.Millisecond
	return mgr
}

// Q628: a worker reaped before it ran leaves this session's OWN delivery with nothing
// to report it — the runner binary never registered — so unless the listener releases
// the assignment, GitHub holds the job on an acquired-but-unresolved delivery and
// cancels the whole run at its ~15-minute unstarted-job timeout. Measured on the
// v1.3.0-rc.5 dogfood gate: three workers reaped while Pending, the run stuck queued,
// and the AGC reporting itself healthy throughout.
//
// One delivery, no fan-out: this is the plain single-delivery path, not a Q260 shape.
func TestAGC_Q628_UnrunWorkerReleasesItsOwnDelivery(t *testing.T) {
	const planID = "plan-q628-release"
	srv := brokertest.New()
	t.Cleanup(srv.Close)
	srv.EnableFanoutAccounting()
	reqIDs := srv.EnqueueFanoutJob(planID, 1)

	m := newTestMetrics()
	mgr := unrunWorkerMux(t, srv, m, true)
	t.Cleanup(mgr.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, mgr.Start(ctx))

	require.Eventually(t, func() bool {
		return len(srv.DeliveryResults(planID)) >= 1
	}, 5*time.Second, 10*time.Millisecond,
		"the session must release the assignment of a job its worker never ran")

	assert.Equal(t, broker.TaskResultAbandoned, srv.DeliveryResults(planID)[reqIDs[0]],
		"the release reports abandoned: the assignment was real, but no step ran")

	// The harm the release prevents. With the delivery resolved, the unstarted-job
	// timeout has nothing dangling to cancel.
	srv.ExpireUnstartedDeliveries(planID)
	assert.NotEqual(t, "cancelled", srv.JobState(planID),
		"a released assignment must not leave GitHub cancelling the run at its unstarted-job timeout")
}

// The pre-fix shape, kept as the negative control: with the AGC's own completejob
// switched off (AGC_FANOUT_COMPLETION=false), the unrun delivery dangles and the
// unstarted-job timeout cancels the run. It is what the opt-out buys, and it fails
// if the release above ever starts happening unconditionally.
func TestAGC_Q628_OptOutLeavesTheAssignmentDangling(t *testing.T) {
	const planID = "plan-q628-optout"
	srv := brokertest.New()
	t.Cleanup(srv.Close)
	srv.EnableFanoutAccounting()
	srv.EnqueueFanoutJob(planID, 1)

	m := newTestMetrics()
	mgr := unrunWorkerMux(t, srv, m, false)
	t.Cleanup(mgr.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, mgr.Start(ctx))

	require.Eventually(t, func() bool {
		return srv.AcquireJobCalls() >= 1
	}, 5*time.Second, 10*time.Millisecond, "the delivery must be acquired before it can dangle")

	require.Eventually(t, func() bool {
		srv.ExpireUnstartedDeliveries(planID)
		return srv.JobState(planID) == "cancelled"
	}, 5*time.Second, 10*time.Millisecond,
		"with the release opted out, the unrun delivery dangles and the run is cancelled at the unstarted-job timeout")
	assert.Empty(t, srv.DeliveryResults(planID), "nothing released the assignment")
}
