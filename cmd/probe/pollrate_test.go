package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/broker/brokertest"
)

// TestAbandonedProbe_EmptyPollRate is the Q788 guard for the probe's two broker
// poll loops. brokertest answers GET /message with 202 at once instead of
// holding the poll, so a loop that re-polls an empty answer with no pause spins
// as fast as the loopback stack allows — measured at ~6,000 req/s per loop
// before the floor, which burned CI CPU and amplified timing flakes across the
// whole suite.
//
// The scenario queues no fixture job, so awaitDelivery spins for its full
// delivery timeout with the observer spinning alongside it: both loops are under
// the measurement, and the run ends on the delivery-timeout branch.
func TestAbandonedProbe_EmptyPollRate(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	bs := brokertest.New()
	t.Cleanup(bs.Close)
	rest := newAbandonedRESTStub(t, key, bs.URL)

	const timeout = time.Second
	cfg, err := parseAbandonedConfig(testAbandonedEnv(t, map[string]string{
		"PROBE_ABANDONED_TIMEOUT": timeout.String(),
		"PROBE_ABANDONED_WINDOW":  "2s",
	}))
	if err != nil {
		t.Fatalf("parseAbandonedConfig: %v", err)
	}

	started := time.Now()
	if err := runAbandonedProbe(context.Background(), discardLogger(), cfg,
		staticTokenProvider{token: "install-token"}, rest.srv.URL); err == nil {
		t.Fatal("expected the delivery-timeout error")
	}
	elapsed := time.Since(started)

	// Two loops at broker.MinPollInterval (100ms) is 20 req/s; the ceiling leaves
	// room for timer coarseness and for the polls in flight at either edge, while
	// staying two orders of magnitude under the unpaced rate.
	rate := float64(bs.GetMessageCalls()) / elapsed.Seconds()
	t.Logf("poll rate: %d polls in %v = %.1f req/s", bs.GetMessageCalls(), elapsed, rate)
	const maxRate = 60.0
	if rate > maxRate {
		t.Errorf("probe polled a non-blocking broker at %.1f req/s, want <= %.1f req/s — the empty-poll floor is not holding", rate, maxRate)
	}
}
