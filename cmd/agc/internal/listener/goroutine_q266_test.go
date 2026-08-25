package listener_test

// Q266: under a sustained fan-out burst the pool must not collapse. GitHub fans one
// logical job (one planID) out to N sibling sessions as N deliveries with distinct
// RunnerRequestIDs; the planID dedup (Q260) collapses them onto ONE winner, but each
// of the N-1 losers already ran AcquireJob, so GitHub considers each loser's deduped
// runner ASSIGNED to the job. A loser's recycle therefore 422s ("runner is currently
// running a job and cannot be deleted") for the WINNER'S ENTIRE RUNTIME — far past the
// bounded Q259 recycle backoff. Recycling eagerly there exhausts the backoff and exits
// the listener (a non-permanent replacement is never restarted), so under sustained
// burst enough losers strand+exit to collapse the pool (the 2/8 seen at re-route #5).
//
// The fix (Q266) defers each loser's recycle until its winner concludes — the point at
// which the winner fans completjob out to the loser's delivery (Option A), releasing
// GitHub's assignment so the 422 clears — holding the slot in the meantime instead of
// recycling into a guaranteed-422-then-exit. This test drives the burst with a
// RecycleAgent that 422s until the winner concludes (the live mechanism) and asserts
// the losers HOLD their slots rather than strand+exit. It FAILS against pre-Q266 code
// (the eager losers exit) and needs no GKE turn-up.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
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

func TestListener_Q266_FanoutLoserDefersRecycleUntilWinnerCompletes(t *testing.T) {
	const (
		planID = "plan-q266"
		n      = 5
	)
	srv := brokertest.New()
	t.Cleanup(srv.Close)
	srv.EnableFanoutAccounting()
	srv.EnqueueFanoutJob(planID, n)

	m := newTestMetrics()

	release := make(chan struct{})  // gates the winner's "worker" until the burst has deduped
	var winnerConcluded atomic.Bool // set true when the winner's job concludes (its assignment — and the losers' — released)
	var exits atomic.Int32          // goroutine exits (Multiplexer calls ReleaseAgent once per exit)
	var admitHeld atomic.Int32      // worker reservations currently held (Q248 starvation guard)

	factory := func(idx int) listener.Config {
		agent := &agentpool.Agent{
			Index:         idx,
			Name:          fmt.Sprintf("test-rg-%d", idx),
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
			Group:            "test-rg",
			Namespace:        "default",
			Agent:            agent,
			Broker:           bc,
			HTTPClient:       srv.HTTPClient(),
			Metrics:          m,
			IdleThreshold:    1_000_000, // never idle-exit during the assertions
			RenewInterval:    time.Hour, // no renewal traffic during the test
			FanoutCompletion: true,
			// Long enough that the winner-conclusion signal always wins the race in the
			// happy path — the fallback is exercised by its own unit assertions, not here.
			LoserRecycleDeferTimeout: 30 * time.Second,
			// Unlimited worker capacity; the closure only lets us observe reservations so
			// we can prove a deferred loser frees its slot before parking (does not pin it).
			Admit: func(_ context.Context) (func(), bool, string) {
				admitHeld.Add(1)
				var once sync.Once
				return func() { once.Do(func() { admitHeld.Add(-1) }) }, true, ""
			},
			MarkAgentConsumed: func() {},
			// One ReleaseAgent call per goroutine exit — our collapse detector.
			ReleaseAgent: func() { exits.Add(1) },
			// Models the live 422: the loser's deduped runner is still assigned to the
			// (running) winner's job on GitHub's books, so its deregister 422s until the
			// winner concludes and releases the assignment (via its Option A completjob).
			RecycleAgent: func(_ context.Context) (*agentpool.Agent, error) {
				if !winnerConcluded.Load() {
					return nil, &agentpool.RunnerBusyError{AgentID: agent.AgentID}
				}
				fresh := *agent // assignment released → recycle succeeds; keep polling
				return &fresh, nil
			},
			// Only the winner runs the handler (losers return before provisioning). It
			// blocks until release, completes its OWN delivery like the real worker's
			// runner binary, marks the job concluded, then returns its pod-phase proxy.
			JobHandler: func(ctx context.Context, runServiceURL, planID string, payload []byte, _ string) (broker.TaskResult, error) {
				var acq struct {
					RunnerRequestID string `json:"runnerRequestId"`
				}
				_ = json.Unmarshal(payload, &acq)
				select {
				case <-release:
				case <-ctx.Done():
					return "", nil
				}
				_ = bc.CompleteJob(ctx, runServiceURL, broker.CompleteJobRequest{
					PlanID: planID,
					JobID:  acq.RunnerRequestID,
					Result: broker.TaskResultSucceeded,
				})
				// The winner concluding releases every deduped sibling's assignment too
				// (it fans completjob out to each on conclusion), so their 422s now clear.
				winnerConcluded.Store(true)
				return broker.TaskResultSucceeded, nil
			},
		}
	}

	mgr := listener.NewMultiplexer(factory, n, nil)
	mgr.RestartDelay = time.Millisecond
	t.Cleanup(mgr.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, mgr.Start(ctx))
	// Pre-scale to N concurrent pollers (the burst), so all N deliveries are handed out
	// near-simultaneously rather than serialized behind the winner's SpawnReplacement.
	for i := 0; i < n-1; i++ {
		mgr.SpawnReplacement(ctx)
	}

	// The N-1 losing siblings are deduped on the shared planID.
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(m.JobsDuplicateDeliveryTotal.WithLabelValues("default", "test-rg")) >= float64(n-1)
	}, 5*time.Second, 10*time.Millisecond, "the N-1 losing siblings must be deduped on planID")

	// THE Q266 INVARIANT: while the winner runs, no deduped loser strands+exits — each
	// holds its slot waiting for the winner to conclude. Pre-Q266 the losers recycle
	// eagerly into an unclearable 422, exhaust the bounded backoff, and exit HERE.
	require.Never(t, func() bool { return exits.Load() > 0 }, 1*time.Second, 20*time.Millisecond,
		"deduped losers must hold their slots (not recycle-and-exit) while the winner runs — the pool must not collapse")

	// Q248 starvation guard: a parked loser must FREE its worker reservation, not pin
	// it while waiting — otherwise N-1 losers would exhaust a tight maxWorkers ceiling
	// with runners that provision nothing. Only the winner's reservation remains.
	require.Eventually(t, func() bool { return admitHeld.Load() == 1 }, 2*time.Second, 20*time.Millisecond,
		"a deferred loser must release its worker reservation before parking; only the winner keeps one")

	// Let the winner finish. It completes its own delivery and fans completjob out to
	// the N-1 deduped siblings (Option A), releasing every assignment so the losers'
	// 422s clear and they recycle back to polling — the pool recovers in place.
	close(release)

	require.Eventually(t, func() bool {
		return testutil.ToFloat64(m.FanoutLoserRecycleDeferredTotal.WithLabelValues("default", "test-rg", "winner_concluded")) >= float64(n-1)
	}, 5*time.Second, 10*time.Millisecond, "every deferred loser must resume on the winner's conclusion (winner_concluded outcome)")

	// The winner's own delivery plus the N-1 fanned-out sibling completions = N.
	require.Eventually(t, func() bool {
		return srv.CompleteJobCalls() >= n
	}, 5*time.Second, 10*time.Millisecond, "the winner must complete its own delivery and fan completjob out to each deduped sibling")

	// The pool held across the whole burst: not one listener exited. (Teardown exits
	// happen later, in t.Cleanup's mgr.Stop — after this assertion.)
	assert.Equal(t, int32(0), exits.Load(),
		"no listener exited: deferring each loser's recycle until its winner completed held the pool near target")
	assert.Equal(t, int32(n), mgr.ActiveCount(),
		"the pool holds all N slots — the N-1 deferred losers recycled in place rather than stranding and exiting")
}
