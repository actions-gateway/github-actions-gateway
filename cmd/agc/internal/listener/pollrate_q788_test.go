package listener_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// pollRateWindow is how long the rate probe observes the listener. Several times
// the empty-poll floor, so the measurement is not dominated by the poll in
// flight at either edge.
const pollRateWindow = 1500 * time.Millisecond

// TestListener_EmptyPollRateFloorAgainstNonBlockingServer is the Q788 guard for
// the v1 poll loop, matching the ScaleSet tier's Q287 floor. The stub answers
// every GET /message with 202 at once — the shape a real backend can also take
// (a GHES tenant with a short poll window, an intermediary that terminates the
// long poll) — and the loop must pace itself off broker.MinPollInterval rather
// than spinning, because a request storm against GitHub is answered by the rate
// limiter, not by us. Measured 2026-08-11 at 9,472 req/s for a single listener
// before the floor, and 9.3 req/s after.
//
// IsLastPoller reports true so idle shutdown stays suppressed and the loop keeps
// polling for the whole window (Q152).
func TestListener_EmptyPollRateFloorAgainstNonBlockingServer(t *testing.T) {
	oauthSrv := oauthStub()
	var polls atomic.Int32
	mux := &brokerMux{}
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		polls.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	brokerSrv := httptest.NewServer(mux)

	cfg := makeCfg(t, oauthSrv, brokerSrv)
	cfg.IsLastPoller = func() bool { return true }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runAndWait(ctx, cfg)

	// Sample after the session handshake so the window covers steady-state polling.
	for polls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	before := polls.Load()
	started := time.Now()
	time.Sleep(pollRateWindow)
	rate := float64(polls.Load()-before) / time.Since(started).Seconds()
	t.Logf("poll rate: %d polls in %v = %.1f req/s", polls.Load()-before, pollRateWindow, rate)

	// broker.MinPollInterval is 100ms → 10 req/s; generous slack for timer
	// coarseness on a loaded `-race` box, still two orders of magnitude under the
	// hot-loop rate.
	const maxRate = 25.0
	if rate > maxRate {
		t.Errorf("listener polled a non-blocking broker at %.1f req/s, want <= %.1f req/s — the empty-poll floor is not holding", rate, maxRate)
	}

	cancel()
	<-done

	closeHTTP(oauthSrv)
	closeHTTP(brokerSrv)
	goleak.VerifyNone(t)
}
