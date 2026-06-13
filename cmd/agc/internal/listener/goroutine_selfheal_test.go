package listener_test

// Q114 self-heal tests: single-use JIT agents are re-registered after each
// job, and stale sessions (401 / 200-with-EOF GetMessage loops) heal in place
// instead of looping forever. See docs/plan/q114-jit-agent-selfheal.md.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/agc/internal/listener"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// sessionCounterCreate returns a CreateSession handler that issues
// "sess-1", "sess-2", … and counts calls.
func sessionCounterCreate(createCalls *atomic.Int32) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		n := createCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionId": fmt.Sprintf("sess-%d", n)})
	}
}

// createSessionAgentID extracts agent.id from a CreateSession request body.
func createSessionAgentID(r *http.Request) int64 {
	var body struct {
		Agent struct {
			ID int64 `json:"id"`
		} `json:"agent"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body.Agent.ID
}

func TestListener_PostJobRecyclesAgent(t *testing.T) {
	oauthSrv := oauthStub()
	var createCalls, recycles, consumedMarks, pollsOnFresh atomic.Int32
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
			// The old session must not be polled again after the job.
			t.Error("listener polled the consumed session after job completion")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		pollsOnFresh.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.IsLastListener = func() bool { return true }
	cfg.MarkAgentConsumed = func() { consumedMarks.Add(1) }
	cfg.JobHandler = func(_ context.Context, _, _ string, _ []byte, _ string) error {
		assert.Equal(t, int32(1), consumedMarks.Load(),
			"agent must be marked consumed before the job handler blocks")
		return nil
	}
	cfg.RecycleAgent = func(_ context.Context) (*agentpool.Agent, error) {
		recycles.Add(1)
		fresh := makeAgent(t, oauthSrv.URL)
		fresh.AgentID = 43
		return fresh, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := runAndWait(ctx, cfg)
	assert.Eventually(t, func() bool { return pollsOnFresh.Load() > 0 }, 4*time.Second, 10*time.Millisecond,
		"listener should poll on a fresh session after post-job recycle")
	assert.Equal(t, int32(1), recycles.Load(), "exactly one recycle after the job")
	assert.GreaterOrEqual(t, createCalls.Load(), int32(2), "a fresh session must be created post-job")
	cancel()
	require.NoError(t, <-done, "post-job recycle must not exit the goroutine")

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

func TestListener_Poll401TokenRefreshWithoutRecycle(t *testing.T) {
	oauthSrv := oauthStub()
	var createCalls, recycles, pollsAfterHeal atomic.Int32
	var sent401 atomic.Bool
	mux := &brokerMux{}

	mux.SetCreate(sessionCounterCreate(&createCalls))
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		if !sent401.Swap(true) {
			// Expired broker OAuth token signature: a one-off 401.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		pollsAfterHeal.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	brokerSrv := httptest.NewServer(mux)

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.IsLastListener = func() bool { return true }
	cfg.RecycleAgent = func(_ context.Context) (*agentpool.Agent, error) {
		recycles.Add(1)
		return makeAgent(t, oauthSrv.URL), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := runAndWait(ctx, cfg)
	assert.Eventually(t, func() bool { return pollsAfterHeal.Load() > 0 }, 4*time.Second, 10*time.Millisecond,
		"polling should resume after a token-refresh heal")
	assert.GreaterOrEqual(t, createCalls.Load(), int32(2), "heal must recreate the session")
	assert.Zero(t, recycles.Load(), "a 401 fixed by a fresh token must not recycle the agent")
	cancel()
	require.NoError(t, <-done)

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

func TestListener_Poll401DeadAgentRecycles(t *testing.T) {
	oauthSrv := oauthStub()
	var createCalls, recycles, pollsOnFresh atomic.Int32
	mux := &brokerMux{}

	// Agent 42 (the stored credentials) gets one session, then is dead: its
	// GetMessage 401s and so does any further CreateSession. Agent 43 (the
	// recycled registration) works.
	mux.SetCreate(func(w http.ResponseWriter, r *http.Request) {
		id := createSessionAgentID(r)
		n := createCalls.Add(1)
		if id == 42 && n > 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionId": fmt.Sprintf("sess-%d", n)})
	})
	mux.SetGetMessage(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sessionId") == "sess-1" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		pollsOnFresh.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	brokerSrv := httptest.NewServer(mux)

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.IsLastListener = func() bool { return true }
	cfg.RecycleAgent = func(_ context.Context) (*agentpool.Agent, error) {
		recycles.Add(1)
		fresh := makeAgent(t, oauthSrv.URL)
		fresh.AgentID = 43
		return fresh, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := runAndWait(ctx, cfg)
	assert.Eventually(t, func() bool { return pollsOnFresh.Load() > 0 }, 4*time.Second, 10*time.Millisecond,
		"polling should resume on the recycled agent's session")
	assert.Equal(t, int32(1), recycles.Load(),
		"a 401 that persists across a fresh token must recycle the agent")
	cancel()
	require.NoError(t, <-done)

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

func TestListener_EOFHealsAfterThreshold(t *testing.T) {
	oauthSrv := oauthStub()
	clk := newFakeClock(time.Now())
	var createCalls, eofPolls atomic.Int32
	var eofsAtHeal atomic.Int32
	mux := &brokerMux{}

	mux.SetCreate(func(w http.ResponseWriter, _ *http.Request) {
		if createCalls.Add(1) == 2 {
			eofsAtHeal.Store(eofPolls.Load())
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionId": fmt.Sprintf("sess-%d", createCalls.Load())})
	})
	mux.SetGetMessage(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sessionId") == "sess-1" {
			// GitHub's deleted-JIT-runner signature: 200 with an empty body.
			eofPolls.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	brokerSrv := httptest.NewServer(mux)

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.IsLastListener = func() bool { return true }
	cfg.Clock = clk

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Drive the inter-EOF backoff waits (15–30 s each) forward.
	advanceDone := make(chan struct{})
	go func() {
		defer close(advanceDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Millisecond):
				clk.Advance(31 * time.Second)
			}
		}
	}()

	done := runAndWait(ctx, cfg)
	assert.Eventually(t, func() bool { return createCalls.Load() >= 2 }, 8*time.Second, 10*time.Millisecond,
		"repeated empty 200s must heal the session")
	assert.GreaterOrEqual(t, eofsAtHeal.Load(), int32(3),
		"heal must wait for the EOF threshold, not fire on the first blip")
	cancel()
	require.NoError(t, <-done)
	<-advanceDone
	clk.Stop()

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

func TestListener_StartupStaleAgentRecyclesOnce(t *testing.T) {
	oauthSrv := oauthStub()
	var recycles, polls atomic.Int32
	mux := &brokerMux{}

	// The stored agent (42) was consumed before a restart: CreateSession
	// rejects it. The recycled agent (43) works.
	mux.SetCreate(func(w http.ResponseWriter, r *http.Request) {
		if createSessionAgentID(r) == 42 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		defaultSession(w)
	})
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		polls.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	brokerSrv := httptest.NewServer(mux)

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.IsLastListener = func() bool { return true }
	cfg.RecycleAgent = func(_ context.Context) (*agentpool.Agent, error) {
		recycles.Add(1)
		fresh := makeAgent(t, oauthSrv.URL)
		fresh.AgentID = 43
		return fresh, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := runAndWait(ctx, cfg)
	assert.Eventually(t, func() bool { return polls.Load() > 0 }, 4*time.Second, 10*time.Millisecond,
		"startup with stale credentials should recycle and reach the poll loop")
	assert.Equal(t, int32(1), recycles.Load())
	cancel()
	require.NoError(t, <-done)

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}

func TestListener_PostJobRecycleFailureExitsRetriable(t *testing.T) {
	oauthSrv := oauthStub()
	var delivered atomic.Bool
	mux := &brokerMux{}
	brokerSrv := httptest.NewServer(mux)

	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		if !delivered.Swap(true) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(jobMsgWithURL(brokerSrv.URL))
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.IsLastListener = func() bool { return true }
	cfg.JobHandler = func(_ context.Context, _, _ string, _ []byte, _ string) error { return nil }
	cfg.RecycleAgent = func(_ context.Context) (*agentpool.Agent, error) {
		return nil, errors.New("github registration API down")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := <-runAndWait(ctx, cfg)
	require.Error(t, err, "a failed post-job recycle must exit the goroutine")
	var nre *listener.NonRetriableError
	assert.False(t, errors.As(err, &nre),
		"the exit must be retriable so the multiplexer's backoff paces the retry")

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}
