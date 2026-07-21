package listener_test

// Q222: SIGTERM during the post-job agent recycle must still delete the broker
// session. The recycle hands session ownership off by clearing the goroutine's
// sessionID before calling recycleAndRestart, so the exit defer will not
// re-delete a session the recycle already deleted. When the context is cancelled
// mid-window that handoff strands the session: the recycle's own best-effort
// DELETE runs on the already-cancelled context and fails instantly, the recycle
// returns, and the exit defer sees an empty sessionID and skips the DELETE
// entirely. The session is leaked permanently — not late — which is the shape
// the integration test's SIGTERM assertion kept timing out on.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/agentpool"
	"github.com/actions-gateway/github-actions-gateway/broker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListener_SIGTERMDuringPostJobRecycleDeletesSession cancels the listener
// context while the goroutine is inside its post-job recycle window and asserts
// the session is still deleted on exit.
func TestListener_SIGTERMDuringPostJobRecycleDeletesSession(t *testing.T) {
	oauthSrv := oauthStub()

	var deletes atomic.Int32
	var (
		deliverOnce sync.Once
		jobStarted  = make(chan struct{})
	)
	mux := &brokerMux{
		onDelete: func(w http.ResponseWriter, _ *http.Request) {
			deletes.Add(1)
			w.WriteHeader(http.StatusOK)
		},
	}
	brokerSrv := httptest.NewServer(mux)
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		delivered := false
		deliverOnce.Do(func() {
			delivered = true
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(jobMsgWithURL(brokerSrv.URL))
		})
		if !delivered {
			w.WriteHeader(http.StatusAccepted)
		}
	})

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	// Block inside the job handler until the listener context is cancelled, so
	// SIGTERM lands with the job in flight and the post-job recycle immediately
	// downstream of it.
	cfg.JobHandler = func(jobCtx context.Context, _, _ string, _ []byte, _ string) (broker.TaskResult, error) {
		close(jobStarted)
		<-jobCtx.Done()
		return "", jobCtx.Err()
	}
	// A recycle attempted on a cancelled context fails, exactly as in production.
	cfg.RecycleAgent = func(rctx context.Context) (*agentpool.Agent, error) {
		if err := rctx.Err(); err != nil {
			return nil, err
		}
		return makeAgent(t, oauthSrv.URL), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := runAndWait(ctx, cfg)

	select {
	case <-jobStarted:
	case <-time.After(20 * time.Second):
		t.Fatal("job handler never started")
	}

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err, "cancellation is a clean exit")
	case <-time.After(20 * time.Second):
		t.Fatal("listener goroutine did not exit after cancellation")
	}

	assert.Positive(t, deletes.Load(),
		"the session must be deleted on shutdown even when SIGTERM lands in the post-job recycle window")

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
}

// TestListener_ExitDeleteRetriesTransientFailure pins the retry on the exit
// DELETE. It is the only delete a session ever gets, so swallowing one transient
// failure as "best-effort" leaks the session just as permanently as never trying.
func TestListener_ExitDeleteRetriesTransientFailure(t *testing.T) {
	oauthSrv := oauthStub()

	var deleteAttempts atomic.Int32
	mux := &brokerMux{
		onDelete: func(w http.ResponseWriter, _ *http.Request) {
			// Fail the first attempt the way a broker tearing down its own fleet
			// would, then accept.
			if deleteAttempts.Add(1) == 1 {
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		},
	}
	brokerSrv := httptest.NewServer(mux)

	cfg := makeCfg(t, oauthSrv, brokerSrv)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := runAndWait(ctx, cfg)

	// Let the goroutine reach the poll loop, then shut it down.
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("listener goroutine did not exit after cancellation")
	}

	assert.GreaterOrEqual(t, deleteAttempts.Load(), int32(2),
		"a transient DELETE failure must be retried, not swallowed as best-effort")

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
}
