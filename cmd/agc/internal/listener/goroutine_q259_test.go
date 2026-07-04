package listener_test

// Q259: a concurrent job burst must not collapse the pool to one online
// listener. The wedge was that GitHub's transient 422 "runner is currently
// running a job and cannot be deleted" (the ephemeral single-use JIT record
// lingering after job completion) made Pool.Recycle fail, so the post-job
// recycle returned an error and the listener goroutine exited — and a
// non-permanent replacement is never restarted, permanently dropping a polling
// slot. This test wires the real agent pool (with a registrar that simulates the
// transient 422) behind RecycleAgent and asserts the goroutine rides out the
// 422, keeps polling on a fresh session, and does not exit.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func q259Scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}

func TestListener_PostJobRecycleRidesOutRunnerBusy(t *testing.T) {
	oauthSrv := oauthStub()
	var createCalls, pollsOnFresh atomic.Int32
	var delivered atomic.Bool
	mux := &brokerMux{}
	brokerSrv := httptest.NewServer(mux)

	mux.SetCreate(sessionCounterCreate(&createCalls))
	mux.SetGetMessage(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sessionId") == "sess-1" {
			if !delivered.Swap(true) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(jobMsgWithURL(brokerSrv.URL))
				return
			}
			// The consumed session must not be polled again after the job.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		pollsOnFresh.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})

	// Real pool + stub registrar pointed at the test servers, so a recycled
	// agent's credentials still authenticate against oauthSrv.
	registrar := agentpool.NewStubRegistrarWithURLs(oauthSrv.URL+"/token", brokerSrv.URL)
	c := fake.NewClientBuilder().WithScheme(q259Scheme()).Build()
	pool := agentpool.NewPool(c, "default", "test-rg", "2.327.1",
		[]string{"self-hosted"}, registrar, agentpool.KeyTypeEd25519)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	require.NoError(t, pool.EnsureAgents(ctx, 1, "token"))

	poolAgent := pool.ClaimAgent()
	require.NotNil(t, poolAgent)
	oldID := poolAgent.AgentID
	// The job acquisition consumes the record; GitHub then refuses to delete it
	// for two deregister attempts (it still shows the runner "running"), forcing
	// Recycle's bounded retry to back off once and succeed on the second attempt.
	registrar.SimulateRunnerBusy(oldID, 2)

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.Agent = poolAgent
	cfg.IsLastPoller = func() bool { return true }
	cfg.MarkAgentConsumed = func() { pool.MarkConsumed(poolAgent) }
	cfg.RecycleAgent = func(rctx context.Context) (*agentpool.Agent, error) {
		return pool.Recycle(rctx, poolAgent, "token")
	}

	done := runAndWait(ctx, cfg)
	assert.Eventually(t, func() bool { return pollsOnFresh.Load() > 0 }, 18*time.Second, 20*time.Millisecond,
		"listener must recycle through the transient 422 and poll on a fresh session")
	assert.GreaterOrEqual(t, registrar.DeregisterCalls(), 2,
		"the transient 422 must have been retried, not treated as fatal")
	cancel()
	require.NoError(t, <-done, "a transient runner-busy 422 during recycle must not exit the goroutine")

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
}
