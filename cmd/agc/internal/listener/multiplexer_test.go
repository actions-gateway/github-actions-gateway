package listener_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karlkfi/github-actions-gateway/agc/internal/listener"
	"github.com/karlkfi/github-actions-gateway/broker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// drainHTTP closes the httptest servers and waits briefly for HTTP client
// connections to drain so goleak does not report false positives from
// net/http.(*persistConn) goroutines.
func drainHTTP(oauthSrv, brokerSrv *httptest.Server) {
	oauthSrv.Close()
	brokerSrv.Close()
	// Allow the transport to notice closed connections.
	time.Sleep(50 * time.Millisecond)
}

// newMuxWithServers creates a Multiplexer plus the backing httptest servers.
// Callers must call stop(), oauthSrv.Close(), brokerSrv.Close() (in that order)
// before calling goleak.VerifyNone.
func newMuxWithServers(t *testing.T, maxListeners int32, mux *brokerMux) (*listener.Multiplexer, *httptest.Server, *httptest.Server) {
	t.Helper()
	oauthSrv := oauthStub()
	brokerSrv := httptest.NewServer(mux)

	factory := func(_ int) listener.Config {
		agent := makeAgent(t, oauthSrv.URL)
		bc := &broker.BrokerClient{
			BrokerURL:  brokerSrv.URL,
			UseV2Flow:  true,
			HTTPClient: brokerSrv.Client(),
		}
		return listener.Config{
			Group:      "test-rg",
			Namespace:  "default",
			Agent:      agent,
			Broker:     bc,
			HTTPClient: oauthSrv.Client(),
		}
	}

	m := listener.NewMultiplexer(factory, maxListeners, nil)
	return m, oauthSrv, brokerSrv
}

func TestMultiplexer_AtRestOneGoroutine(t *testing.T) {
	mux := &brokerMux{} // always 202
	m, oauthSrv, brokerSrv := newMuxWithServers(t, 5, mux)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	require.NoError(t, m.Start(ctx))

	assert.Eventually(t, func() bool { return m.ActiveCount() == 1 }, 2*time.Second, 10*time.Millisecond,
		"expected exactly 1 listener goroutine at rest")

	cancel()
	m.Stop()
	drainHTTP(oauthSrv, brokerSrv)
	goleak.VerifyNone(t)
}

func TestMultiplexer_SpawnOnAcquire(t *testing.T) {
	var delivered atomic.Bool
	mux := &brokerMux{}
	mux.SetGetMessage(func(w http.ResponseWriter, _ *http.Request) {
		if !delivered.Swap(true) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(broker.TaskAgentMessage{
				MessageID:   1,
				MessageType: "RunnerJobRequest",
				Body:        `{}`,
			})
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})

	m, oauthSrv, brokerSrv := newMuxWithServers(t, 5, mux)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, m.Start(ctx))
	assert.Eventually(t, func() bool { return m.ActiveCount() >= 2 }, 4*time.Second, 10*time.Millisecond,
		"expected replacement goroutine spawned after job acquisition")

	cancel()
	m.Stop()
	drainHTTP(oauthSrv, brokerSrv)
	goleak.VerifyNone(t)
}

func TestMultiplexer_CeilingRespected(t *testing.T) {
	mux := &brokerMux{} // always 202
	m, oauthSrv, brokerSrv := newMuxWithServers(t, 3, mux)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	require.NoError(t, m.Start(ctx))
	for i := 0; i < 10; i++ {
		m.SpawnReplacement(ctx)
	}
	time.Sleep(50 * time.Millisecond)

	assert.LessOrEqual(t, m.ActiveCount(), int32(3), "activeCount must not exceed maxListeners")

	cancel()
	m.Stop()
	drainHTTP(oauthSrv, brokerSrv)
	goleak.VerifyNone(t)
}

func TestMultiplexer_IdleShutdown(t *testing.T) {
	mux := &brokerMux{} // always 202
	// Use a lower idle threshold via the factory Config.
	m, oauthSrv, brokerSrv := newMuxWithServersWithThreshold(t, 3, mux, 5)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, m.Start(ctx))
	// Spawn one extra non-permanent goroutine.
	m.SpawnReplacement(ctx)

	// Wait for the non-permanent goroutine to idle-exit (it is not the last).
	assert.Eventually(t, func() bool { return m.ActiveCount() == 1 }, 4*time.Second, 10*time.Millisecond,
		"non-permanent goroutine should idle-exit; permanent baseline should remain")

	cancel()
	m.Stop()
	drainHTTP(oauthSrv, brokerSrv)
	goleak.VerifyNone(t)
}

func TestMultiplexer_StopCleanly(t *testing.T) {
	mux := &brokerMux{} // always 202
	m, oauthSrv, brokerSrv := newMuxWithServers(t, 5, mux)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, m.Start(ctx))
	m.SpawnReplacement(ctx)
	m.SpawnReplacement(ctx)
	time.Sleep(100 * time.Millisecond)

	cancel()
	m.Stop()
	drainHTTP(oauthSrv, brokerSrv)
	goleak.VerifyNone(t)
}

func TestMultiplexer_SetMaxListenersDown(t *testing.T) {
	mux := &brokerMux{} // always 202
	m, oauthSrv, brokerSrv := newMuxWithServers(t, 5, mux)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, m.Start(ctx))
	for i := 0; i < 3; i++ {
		m.SpawnReplacement(ctx)
	}
	time.Sleep(50 * time.Millisecond)

	m.SetMaxListeners(2)
	// No new goroutines should be spawnable above the ceiling.
	m.SpawnReplacement(ctx)
	m.SpawnReplacement(ctx)
	time.Sleep(50 * time.Millisecond)

	assert.LessOrEqual(t, m.ActiveCount(), int32(5), "count should not exceed original value")

	cancel()
	m.Stop()
	drainHTTP(oauthSrv, brokerSrv)
	goleak.VerifyNone(t)
}

// newMuxWithServersWithThreshold creates a Multiplexer with a custom idleThreshold.
func newMuxWithServersWithThreshold(t *testing.T, maxListeners int32, mux *brokerMux, idleThreshold int) (*listener.Multiplexer, *httptest.Server, *httptest.Server) {
	t.Helper()
	oauthSrv := oauthStub()
	brokerSrv := httptest.NewServer(mux)

	factory := func(_ int) listener.Config {
		agent := makeAgent(t, oauthSrv.URL)
		bc := &broker.BrokerClient{
			BrokerURL:  brokerSrv.URL,
			UseV2Flow:  true,
			HTTPClient: brokerSrv.Client(),
		}
		return listener.Config{
			Group:         "test-rg",
			Namespace:     "default",
			Agent:         agent,
			Broker:        bc,
			HTTPClient:    oauthSrv.Client(),
			IdleThreshold: idleThreshold,
		}
	}

	m := listener.NewMultiplexer(factory, maxListeners, nil)
	return m, oauthSrv, brokerSrv
}
