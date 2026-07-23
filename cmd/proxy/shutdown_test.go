package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// openTunnel establishes a CONNECT tunnel through srv to dest and returns the
// hijacked client connection.
func openTunnel(t *testing.T, srv *Server, dest string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", dest, dest)
	require.NoError(t, err)

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return conn
}

// readyzStatus returns the current /readyz status code.
func readyzStatus(t *testing.T, srv *Server) int {
	t.Helper()
	resp, err := http.Get("http://" + srv.HealthAddr + "/readyz") //nolint:noctx // short-lived probe in test
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestServer_ShutdownWaitsForInFlightTunnel is the Q384 regression test.
//
// http.Server.Shutdown neither closes nor waits for hijacked connections, and
// every CONNECT tunnel is hijacked — so before the fix, ListenAndServe returned
// (and the process exited) while tunnels were still relaying, cutting live CI
// egress on every rollout.
//
// The assertion that matters is not "it shut down cleanly" — a shutdown test
// asserting only that passes against the bug. It is that bytes still round-trip
// through an established tunnel AFTER shutdown has demonstrably begun.
func TestServer_ShutdownWaitsForInFlightTunnel(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	echoAddr := startEchoServer(t)

	srv := NewServer(testListenAddr, testListenAddr, 5*time.Second, nil, prometheus.NewRegistry())
	// Linger off: this case is about Q384's tunnel drain, and the Q386 linger
	// would only add a fixed delay in front of what it asserts. The linger has
	// its own cases below.
	srv.ShutdownLinger = -1
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe(ctx) }()

	select {
	case <-srv.ready:
	case err := <-served:
		t.Fatalf("ListenAndServe returned before binding its listeners: %v", err)
	}

	conn := openTunnel(t, srv, echoAddr)

	cancel()

	// The direct Q384 assertion. Without tunnel tracking, Shutdown returns as
	// soon as the (untracked) hijacked connection is out of its way and
	// ListenAndServe returns within microseconds — in production that is the
	// process exiting out from under a live tunnel. The window is generous
	// relative to the microseconds the buggy path takes.
	select {
	case err := <-served:
		t.Fatalf("ListenAndServe returned while a tunnel was still open: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	// Draining is also observable to the kubelet: /readyz must fail so no new
	// CONNECT traffic is steered here while the tunnel finishes.
	assert.Equal(t, http.StatusServiceUnavailable, readyzStatus(t, srv),
		"/readyz must fail once draining starts")

	// The load-bearing assertion: the tunnel still carries traffic mid-drain.
	msg := "still flowing during shutdown"
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))
	_, err := io.WriteString(conn, msg)
	require.NoError(t, err, "writing to an in-flight tunnel during drain must succeed")

	buf := make([]byte, len(msg))
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err, "an in-flight tunnel must survive graceful shutdown")
	assert.Equal(t, msg, string(buf))

	// Closing the client ends the relay, which completes the drain.
	require.NoError(t, conn.Close())
	select {
	case err := <-served:
		assert.NoError(t, err, "ListenAndServe must return cleanly once tunnels drain")
	case <-time.After(10 * time.Second):
		t.Fatal("ListenAndServe did not return after the last tunnel closed")
	}
}

// TestServer_ShutdownDrainDeadlineCutsTunnel verifies the drain is bounded: a
// tunnel that never finishes must not hold the process past its deadline, or a
// single long-lived stream would keep the pod alive until SIGKILL and defeat
// terminationGracePeriodSeconds entirely.
func TestServer_ShutdownDrainDeadlineCutsTunnel(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	echoAddr := startEchoServer(t)

	srv := NewServer(testListenAddr, testListenAddr, 5*time.Second, nil, prometheus.NewRegistry())
	srv.ShutdownDrainTimeout = 250 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe(ctx) }()

	select {
	case <-srv.ready:
	case err := <-served:
		t.Fatalf("ListenAndServe returned before binding its listeners: %v", err)
	}

	// Idle but open: the relay goroutines are blocked in io.Copy and nothing
	// will ever finish them, so only the deadline can end this tunnel.
	conn := openTunnel(t, srv, echoAddr)

	start := time.Now()
	cancel()

	select {
	case err := <-served:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("drain deadline was not enforced: ListenAndServe never returned")
	}
	assert.Less(t, time.Since(start), 5*time.Second,
		"shutdown must be bounded by ShutdownDrainTimeout, not wait for the tunnel")

	// The tunnel was force-closed, so the client side is now dead.
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err := conn.Read(make([]byte, 1))
	assert.Error(t, err, "a tunnel outliving the drain deadline must be force-closed")
}

// startDrainingServer boots a proxy, waits for it to bind, and returns it plus a
// cancel func and the channel ListenAndServe returns on.
func startDrainingServer(t *testing.T, tune func(*Server)) (*Server, context.CancelFunc, chan error) {
	t.Helper()

	srv := NewServer(testListenAddr, testListenAddr, 5*time.Second, nil, prometheus.NewRegistry())
	tune(srv)

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe(ctx) }()

	select {
	case <-srv.ready:
	case err := <-served:
		cancel()
		t.Fatalf("ListenAndServe returned before binding its listeners: %v", err)
	}
	return srv, cancel, served
}

// TestServer_ShutdownLingerServesLateConnection is the Q386 regression test.
//
// Endpoint removal is a control loop concurrent with SIGTERM, not a predecessor,
// so a kube-proxy that has not yet applied our removal keeps steering NEW
// connections here. Q384's drain covers only tunnels already established; before
// the linger, the listener closed as soon as shutdown began and those arrivals
// were refused outright.
//
// The assertion that matters is not that shutdown was graceful — it is that a
// CONNECT opened AFTER shutdown demonstrably began still completes.
func TestServer_ShutdownLingerServesLateConnection(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	echoAddr := startEchoServer(t)
	srv, cancel, served := startDrainingServer(t, func(s *Server) {
		s.ShutdownLinger = 5 * time.Second
		s.quiescence = 300 * time.Millisecond
	})

	cancel()

	// Shutdown has begun — observable to the kubelet, which is what starts the
	// endpoint-removal clock this linger is waiting on. A preStop sleep cannot
	// do this: the process does not know it is terminating until SIGTERM.
	require.Eventually(t, func() bool {
		return readyzStatus(t, srv) == http.StatusServiceUnavailable
	}, 5*time.Second, 10*time.Millisecond, "/readyz must fail as soon as draining starts")

	// The load-bearing assertion: a brand-new tunnel, opened after /readyz began
	// failing, is still served rather than refused.
	conn := openTunnel(t, srv, echoAddr)

	msg := "late arrival must still be tunneled"
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))
	_, err := io.WriteString(conn, msg)
	require.NoError(t, err)

	buf := make([]byte, len(msg))
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err, "a connection arriving during the linger must be served end to end")
	assert.Equal(t, msg, string(buf))

	require.NoError(t, conn.Close())
	select {
	case err := <-served:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("ListenAndServe did not return after the linger and drain completed")
	}
}

// TestServer_ShutdownLingerExitsOnQuiescence is the half that makes this cheaper
// than a fixed preStop sleep: an idle proxy waits out the quiescence interval and
// leaves, rather than burning the whole ceiling on every rollout.
func TestServer_ShutdownLingerExitsOnQuiescence(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	_, cancel, served := startDrainingServer(t, func(s *Server) {
		s.ShutdownLinger = 30 * time.Second // never reached
		s.quiescence = 300 * time.Millisecond
	})

	start := time.Now()
	cancel()

	select {
	case err := <-served:
		assert.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("linger did not exit on quiescence; it waited for the ceiling")
	}
	elapsed := time.Since(start)

	// The floor: quiescence is measured from shutdown start, so even a proxy with
	// no arrivals at all holds the listener open this long. Exiting instantly
	// would walk straight back into the race.
	assert.GreaterOrEqual(t, elapsed, 300*time.Millisecond,
		"an idle proxy must still hold the listener open for the quiescence interval")
	assert.Less(t, elapsed, 10*time.Second,
		"quiescence must end the linger well before the ceiling")
}

// TestServer_ShutdownLingerExtendsWhileArrivalsContinue verifies the other
// direction: while connections keep landing, each arrival is fresh evidence that
// some dataplane has not converged, so the linger must keep extending instead of
// leaving after one quiet-looking interval.
func TestServer_ShutdownLingerExtendsWhileArrivalsContinue(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	srv, cancel, served := startDrainingServer(t, func(s *Server) {
		s.ShutdownLinger = 2 * time.Second
		s.quiescence = 300 * time.Millisecond
	})

	start := time.Now()
	cancel()

	// Keep handing the pod new connections for well past the quiescence interval.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for time.Since(start) < 1200*time.Millisecond {
			if c, err := net.Dial("tcp", srv.Addr); err == nil {
				_ = c.Close()
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	select {
	case err := <-served:
		assert.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("linger never ended")
	}
	<-done

	assert.Greater(t, time.Since(start), 900*time.Millisecond,
		"continued arrivals must extend the linger past a single quiescence interval")
}

// TestServer_ShutdownLingerBoundedByDrainBudget is the arithmetic that keeps the
// pod inside terminationGracePeriodSeconds: the linger is spent INSIDE
// ShutdownDrainTimeout, never added on top of it. A linger that outlived the
// drain budget would push the whole sequence past the grace period and be
// SIGKILLed — the exact failure the drain exists to prevent.
func TestServer_ShutdownLingerBoundedByDrainBudget(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	_, cancel, served := startDrainingServer(t, func(s *Server) {
		s.ShutdownDrainTimeout = 400 * time.Millisecond
		s.ShutdownLinger = 30 * time.Second // would dominate if it were additive
		s.quiescence = 30 * time.Second     // and quiescence would never fire
	})

	start := time.Now()
	cancel()

	select {
	case err := <-served:
		assert.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("linger escaped the drain budget")
	}
	assert.Less(t, time.Since(start), 5*time.Second,
		"the drain budget must cap the linger, not run after it")
}

// TestServer_ShutdownLingerDisabled covers the truncated-shutdown-window escape
// hatch (PROXY_SHUTDOWN_LINGER negative): on a node with only seconds of notice,
// an operator can spend the whole budget draining instead of waiting.
func TestServer_ShutdownLingerDisabled(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	_, cancel, served := startDrainingServer(t, func(s *Server) {
		s.ShutdownLinger = -1
		s.quiescence = 30 * time.Second // proves the linger is skipped, not satisfied
	})

	start := time.Now()
	cancel()

	select {
	case err := <-served:
		assert.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("a disabled linger must not wait at all")
	}
	assert.Less(t, time.Since(start), 2*time.Second)
}

// TestTunnelTracker_DrainedWithNoTunnels verifies the common case — a rollout
// with no CONNECT traffic in flight — completes immediately rather than
// burning the whole drain budget.
func TestTunnelTracker_DrainedWithNoTunnels(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	var tr tunnelTracker
	select {
	case <-tr.drained():
	case <-time.After(5 * time.Second):
		t.Fatal("drained() must close immediately when no tunnels are registered")
	}
	assert.Equal(t, 0, tr.closeAll())
}

// TestTunnelTracker_DrainedWaitsThenReleases pins the tracker contract directly:
// drained() stays open while a tunnel is registered and closes on release.
func TestTunnelTracker_DrainedWaitsThenReleases(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	var tr tunnelTracker
	tn := tr.add()

	done := tr.drained()
	select {
	case <-done:
		t.Fatal("drained() closed while a tunnel was still registered")
	case <-time.After(50 * time.Millisecond):
	}

	tr.release(tn)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drained() did not close after the last tunnel was released")
	}
}
