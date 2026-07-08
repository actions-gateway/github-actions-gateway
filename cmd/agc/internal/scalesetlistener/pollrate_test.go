package scalesetlistener_test

import (
	"strings"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/scaleset/scalesettest"
)

// pollRateWindow is how long each rate probe observes the listener. It must span several
// poll windows so the measurement is not dominated by the in-flight poll at either edge.
const pollRateWindow = 1500 * time.Millisecond

// TestListener_IdlePollRate is the Q287 guard: an idle listener must not hot-loop the
// message queue. The fake long-polls an empty queue (holding the request until a message
// lands or its poll window elapses), so an idle listener issues roughly one request per
// window rather than spinning as fast as the TCP stack allows.
//
// Before the fix the fake's 202 returned instantly and Listener.run re-polled with no
// pause: ~5,000 requests/second per listener, which burned CI CPU and amplified timing
// flakes across the suite. The ceiling below is deliberately loose (it must hold on a
// loaded CI box) while still being ~3 orders of magnitude under the hot-loop rate.
func TestListener_IdlePollRate(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)

	prov := &recordingProvisioner{srv: srv}
	startListener(t, srv, fixedCapacity(1), prov, nil)

	rate := measurePollRate(t, srv)

	// One poll per DefaultPollTimeout window, with slack for the polls straddling the
	// measurement edges and for a slow scheduler.
	maxRate := 4 * float64(time.Second) / float64(scalesettest.DefaultPollTimeout)
	if rate > maxRate {
		t.Errorf("idle listener polled at %.1f req/s, want <= %.1f req/s — the fake is not long-polling", rate, maxRate)
	}
}

// TestListener_EmptyPollRateFloorAgainstNonBlockingServer covers the listener's own
// defense in depth. The fake is put back into its old instantly-202 mode — the shape a
// real backend can also take (a GHES tenant with a short poll window, an intermediary
// that terminates the long poll, a backend that declines to hold a zero-capacity poll).
// Listener.run must still pace itself off minPollInterval rather than spinning, because
// a request storm against GitHub is answered by the rate limiter, not by us.
func TestListener_EmptyPollRateFloorAgainstNonBlockingServer(t *testing.T) {
	srv := scalesettest.New()
	t.Cleanup(srv.Close)
	srv.SetPollTimeout(0) // every empty poll answers 202 at once

	prov := &recordingProvisioner{srv: srv}
	startListener(t, srv, fixedCapacity(1), prov, nil)

	rate := measurePollRate(t, srv)

	// minPollInterval is 100ms → 10 req/s; allow generous slack for timer coarseness.
	const maxRate = 25.0
	if rate > maxRate {
		t.Errorf("listener polled a non-blocking server at %.1f req/s, want <= %.1f req/s — the empty-poll floor is not holding", rate, maxRate)
	}
}

// measurePollRate reports the listener's poll requests per second over pollRateWindow.
func measurePollRate(t *testing.T, srv *scalesettest.Server) float64 {
	t.Helper()
	before := pollCalls(srv)
	time.Sleep(pollRateWindow)
	polls := pollCalls(srv) - before

	rate := float64(polls) / pollRateWindow.Seconds()
	t.Logf("poll rate: %d polls in %v = %.1f req/s", polls, pollRateWindow, rate)
	return rate
}

// pollCalls counts the poll requests the fake has served.
func pollCalls(srv *scalesettest.Server) int {
	n := 0
	for _, c := range srv.Calls() {
		if strings.HasPrefix(c, "poll ") {
			n++
		}
	}
	return n
}
