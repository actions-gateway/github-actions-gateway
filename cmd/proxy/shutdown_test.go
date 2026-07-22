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
