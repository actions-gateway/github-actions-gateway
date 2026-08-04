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

// Q628/Q676: a worker reaped before it ran leaves this session's OWN delivery with
// nothing to report it — the runner binary never registered. The Q628 fix released
// it with completejob(abandoned); measured live (Q645 Investigation H and the Q676
// remedy runs), completing the winner's own sole delivery concludes the whole run
// as SUCCESS — a false green — abandoned and canceled alike, while failed is
// refused with a 401. So the listener must report NOTHING for its own unrun
// delivery, in both AGC_FANOUT_COMPLETION states, and leave the job to the acquire
// lock's lapse.
//
// One delivery, no fan-out: this is the plain single-delivery path, not a Q260
// shape — the sibling fan-out completion stays on and is covered by the Q260
// accounting tests.
func TestAGC_Q628_UnrunWorkerCompletesNothing(t *testing.T) {
	for _, tc := range []struct {
		name             string
		fanoutCompletion bool
	}{
		{name: "fanout completion on", fanoutCompletion: true},
		{name: "fanout completion off", fanoutCompletion: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const planID = "plan-q628-nothing"
			srv := brokertest.New()
			t.Cleanup(srv.Close)
			srv.EnableFanoutAccounting()
			srv.EnqueueFanoutJob(planID, 1)
			// A second job behind the first: the single listener runs handleJob
			// synchronously in its poll loop, so acquiring this one proves the
			// abandoned job's handleJob — where the pre-Q676 release ran — returned.
			srv.EnqueueFanoutJob(planID+"-next", 1)

			m := newTestMetrics()
			mgr := unrunWorkerMux(t, srv, m, tc.fanoutCompletion)
			t.Cleanup(mgr.Stop)

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			require.NoError(t, mgr.Start(ctx))

			require.Eventually(t, func() bool {
				return srv.AcquireJobCalls() >= 2
			}, 5*time.Second, 10*time.Millisecond,
				"both deliveries must be acquired — the second proves the first's handleJob returned")

			assert.Zero(t, srv.CompleteJobCalls(),
				"no completejob may be sent for the winner's own unrun delivery: every accepted value concludes the run green (Q676)")
			assert.Empty(t, srv.DeliveryResults(planID),
				"the assignment stays unresolved for GitHub's lock lapse to reclaim")
		})
	}
}
