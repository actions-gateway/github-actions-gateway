package listener_test

// Q267: the broker-credential recycle seam (the Q259/Q114 family). When a
// listener recycles its single-use JIT agent it registers a fresh runner record
// and immediately exchanges the record's clientId for a broker OAuth token.
// GitHub's token endpoint returns a transient 400 "Registration … was not found"
// in the propagation window between generate-jitconfig creating the record and
// the OAuth service recognizing it. healSession rides out a token 400 on the
// STORED credentials (via one recycle), but the FRESH-credential exchange inside
// recycleAndRestart treated that same 400 as fatal: the goroutine exited and its
// slot churned a new record. Under a wide maxListeners the exits multiply stale
// records and hold the online pool near zero — the wide-pool collapse re-route #7
// pinned to "broker token exchange rejected … Registration … was not found".
//
// This test drives the real post-job recycle path with an OAuth stub that 400s
// the freshly recycled agent's first token exchange (propagation lag) and then
// succeeds, and asserts the goroutine rides it out, polls on a fresh session, and
// does not exit — instead of failing the exchange and dropping its slot.

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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestListener_PostJobRecycleRidesOutRegistrationPropagation(t *testing.T) {
	// Stateful OAuth stub: the startup exchange (call 1) succeeds; the exchange
	// for the freshly recycled agent (call 2) 400s once with GitHub's transient
	// "Registration … was not found" body (generate-jitconfig → OAuth propagation
	// lag), then every later exchange succeeds. refreshBrokerToken is called once
	// at startup and once per recycle and nowhere else on the job path, so the
	// call count is deterministic.
	var tokenCalls atomic.Int32
	oauthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if tokenCalls.Add(1) == 2 {
			http.Error(w,
				`{"error":"invalid_client","error_description":"Registration 12345 was not found"}`,
				http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token": "stub-runner-token",
			"token_type":   "Bearer",
		})
	}))

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
	// agent's fresh credentials authenticate against oauthSrv.
	registrar := agentpool.NewStubRegistrarWithURLs(oauthSrv.URL+"/token", brokerSrv.URL)
	c := fake.NewClientBuilder().WithScheme(q259Scheme()).Build()
	pool := agentpool.NewPool(c, "default", "test-rg", "2.327.1",
		[]string{"self-hosted"}, registrar, agentpool.KeyTypeEd25519)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	require.NoError(t, pool.EnsureAgents(ctx, 1, "token"))

	poolAgent := pool.ClaimAgent()
	require.NotNil(t, poolAgent)

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.Agent = poolAgent
	cfg.IsLastPoller = func() bool { return true }
	cfg.MarkAgentConsumed = func() { pool.MarkConsumed(poolAgent) }
	cfg.RecycleAgent = func(rctx context.Context) (*agentpool.Agent, error) {
		return pool.Recycle(rctx, poolAgent, "token")
	}
	// Tiny backoff so the propagation retry is fast and deterministic under test.
	cfg.TokenPropagationRetryBackoff = 5 * time.Millisecond

	done := runAndWait(ctx, cfg)
	assert.Eventually(t, func() bool { return pollsOnFresh.Load() > 0 }, 18*time.Second, 20*time.Millisecond,
		"listener must ride out the transient post-recycle 'Registration not found' 400 and poll on a fresh session")
	assert.GreaterOrEqual(t, int(tokenCalls.Load()), 3,
		"the propagation 400 must have been retried, not treated as fatal")
	cancel()
	require.NoError(t, <-done,
		"a transient registration-propagation 400 during the post-recycle token exchange must not exit the goroutine")

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
}
