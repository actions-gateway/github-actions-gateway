package listener_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/actions-gateway/github-actions-gateway/broker/brokertest"
	"github.com/actions-gateway/github-actions-gateway/githubapp"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fanoutMux builds a Multiplexer whose listener goroutines talk to a
// fan-out-accounting brokertest.Server. The winner's JobHandler simulates the
// worker pod's runner binary: it blocks until release is closed (so every sibling
// has acquired and been deduped first), then completes its OWN delivery via
// completejob with result succeeded — exactly what the real worker reports to the
// run service. Losing siblings are deduped by the planID claim and never run the
// JobHandler, so their acquired deliveries are left unresolved.
func fanoutMux(t *testing.T, srv *brokertest.Server, maxListeners int32, m *listener.Metrics, release <-chan struct{}) *listener.Multiplexer {
	t.Helper()
	factory := func(idx int) listener.Config {
		agent := &agentpool.Agent{
			Index:         idx,
			AgentID:       42,
			RunnerVersion: "2.335.1",
			PrivateKey:    testRSAKey,
			Creds: &githubapp.RunnerCredentials{
				ClientID:         "stub-client",
				AuthorizationURL: srv.URL + "token",
			},
		}
		bc := &broker.Client{
			BrokerURL:  srv.URL,
			UseV2Flow:  true,
			HTTPClient: srv.HTTPClient(),
		}
		return listener.Config{
			Group:         "test-rg",
			Namespace:     "default",
			Agent:         agent,
			Broker:        bc,
			HTTPClient:    srv.HTTPClient(),
			Metrics:       m,
			IdleThreshold: 1_000_000, // never idle-exit during the assertions
			RenewInterval: time.Hour, // no renewal traffic during the test
			JobHandler: func(ctx context.Context, runServiceURL, planID string, payload []byte, _ string) error {
				// Recover this delivery's own RunnerRequestID (the fan-out acquire
				// response embeds it) so the "worker" completes the delivery it ran.
				var acq struct {
					RunnerRequestID string `json:"runnerRequestId"`
				}
				_ = json.Unmarshal(payload, &acq)
				select {
				case <-release:
				case <-ctx.Done():
					return nil
				}
				_ = bc.CompleteJob(ctx, runServiceURL, broker.CompleteJobRequest{
					PlanID: planID,
					JobID:  acq.RunnerRequestID,
					Result: broker.TaskResultSucceeded,
				})
				return nil
			},
		}
	}
	mgr := listener.NewMultiplexer(factory, maxListeners, nil)
	mgr.RestartDelay = time.Millisecond
	return mgr
}

// driveFanout brings up N concurrent pollers, enqueues one logical job fanned out
// to all N, waits until the winner is running and the N-1 losers are deduped, then
// releases the winner to complete its own delivery. It returns once the winner's
// completejob has been recorded. planID identifies the logical job for the caller's
// JobState assertion.
func driveFanout(t *testing.T, srv *brokertest.Server, mgr *listener.Multiplexer, m *listener.Metrics, planID string, n int, release chan struct{}) {
	t.Helper()
	srv.EnableFanoutAccounting()
	srv.EnqueueFanoutJob(planID, n)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, mgr.Start(ctx))
	// Pre-scale to N concurrent pollers (the burst), so all N deliveries are handed
	// out near-simultaneously rather than waiting on the winner's SpawnReplacement.
	for i := 0; i < n-1; i++ {
		mgr.SpawnReplacement(ctx)
	}

	// The N-1 losing siblings are deduped on the shared planID.
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(m.JobsDuplicateDeliveryTotal.WithLabelValues("default", "test-rg")) >= float64(n-1)
	}, 5*time.Second, 10*time.Millisecond, "the N-1 losing siblings must be deduped on planID")

	// Release the winner to complete its own delivery (the worker finishing).
	close(release)
	require.Eventually(t, func() bool {
		return srv.CompleteJobCalls() >= 1
	}, 5*time.Second, 10*time.Millisecond, "the winner must complete its own delivery via completejob")
	last, ok := srv.LastCompleteJob()
	require.True(t, ok)
	require.Equal(t, broker.TaskResultSucceeded, last.Result, "the winner reports a real terminal result")
}

// TestAGC_Q260_FanoutCompletionAccountingGap reproduces the remaining Q224 blocker
// deterministically, offline: GitHub fans one logical job out to N sibling sessions
// as N deliveries (distinct RunnerRequestIDs, shared planID); the AGC correctly
// dedups to ONE winner that runs the job to completion — yet the job is CANCELLED at
// the ~15-minute unstarted-job timeout, because the N-1 deduped-away sibling
// deliveries were acquired and then silently abandoned, and GitHub is still waiting
// on them.
//
// This is the accounting gap the default stub could not surface — it modeled neither
// the fan-out nor the per-delivery completion — which is why the Q260 dedup passed
// envtest yet wedged production (re-route #4, 2026-07-04). The companion test
// TestAGC_Q260_FanoutCompletionReconciles asserts the post-fix invariant and is the
// gate for the reconciliation fix designed in
// docs/plan/q260-fanout-completion-reconciliation.md.
func TestAGC_Q260_FanoutCompletionAccountingGap(t *testing.T) {
	const (
		planID = "plan-fanout"
		n      = 4
	)
	srv := brokertest.New()
	t.Cleanup(srv.Close)

	m := newTestMetrics()
	release := make(chan struct{})
	mgr := fanoutMux(t, srv, n, m, release)
	t.Cleanup(mgr.Stop)

	driveFanout(t, srv, mgr, m, planID, n, release)

	// The winner ran the job to completion, so before the timeout the job is not yet
	// concluded (its own delivery is done, but the N-1 sibling deliveries dangle).
	require.Equal(t, "in_progress", srv.JobState(planID),
		"a single sibling's completion must not conclude the job while others dangle")

	// Fire GitHub's unstarted-job timeout. The dangling sibling deliveries cancel the
	// whole job — even though the winner completed it. THIS is the Q260 wedge.
	srv.ExpireUnstartedDeliveries(planID)
	assert.Equal(t, "cancelled", srv.JobState(planID),
		"today: the deduped-away sibling deliveries are never resolved, so GitHub cancels the completed job at its unstarted-job timeout (Q260 accounting gap)")
}

// TestAGC_Q260_FanoutCompletionReconciles is the fix gate: once the AGC reconciles
// GitHub's per-delivery fan-out with its one-runner-per-session model (resolving the
// deduped-away sibling deliveries so none dangle), a job the single deduped runner
// completed must conclude as completed across ALL its sibling deliveries — no
// unstarted-timeout cancel.
//
// SKIPPED until that reconciliation lands: it FAILS against today's code (the winner
// completes only its own delivery; the siblings dangle and the job cancels — see
// TestAGC_Q260_FanoutCompletionAccountingGap). Remove the Skip when implementing the
// design in docs/plan/q260-fanout-completion-reconciliation.md; it validates the fix
// with no GKE turn-up.
func TestAGC_Q260_FanoutCompletionReconciles(t *testing.T) {
	t.Skip("Q260: gated on the fan-out completion reconciliation fix — see docs/plan/q260-fanout-completion-reconciliation.md")

	const (
		planID = "plan-fanout-fixed"
		n      = 4
	)
	srv := brokertest.New()
	t.Cleanup(srv.Close)

	m := newTestMetrics()
	release := make(chan struct{})
	mgr := fanoutMux(t, srv, n, m, release)
	t.Cleanup(mgr.Stop)

	driveFanout(t, srv, mgr, m, planID, n, release)

	// After the fix, the deduped-away sibling deliveries are resolved too, so none
	// dangle — and the job the winner completed concludes as completed even after the
	// unstarted-job timeout fires.
	srv.ExpireUnstartedDeliveries(planID)
	assert.Equal(t, "completed", srv.JobState(planID),
		"post-fix: reconciling all sibling deliveries lets the completed job conclude green")
}
