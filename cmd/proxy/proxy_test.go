package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func newTestServer(t *testing.T) (*Server, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	srv := NewServer("", "", 5*time.Second, nil, reg)
	return srv, reg
}

func gaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == name {
			for _, m := range mf.GetMetric() {
				switch mf.GetType() {
				case dto.MetricType_GAUGE:
					return m.GetGauge().GetValue()
				case dto.MetricType_COUNTER:
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// startEchoServer starts a TCP server that echoes back whatever it receives.
func startEchoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func TestProxy_Connect(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	echoAddr := startEchoServer(t)
	srv, _ := newTestServer(t)

	ts := httptest.NewServer(http.HandlerFunc(srv.handleConnect))
	t.Cleanup(ts.Close)

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()

	// Send CONNECT request.
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)

	// Read 200 response.
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Now the tunnel is established — send data and expect it echoed back.
	msg := "hello proxy"
	_, err = fmt.Fprint(conn, msg)
	require.NoError(t, err)

	buf := make([]byte, len(msg))
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	assert.Equal(t, msg, string(buf))
}

func TestProxy_NonConnectMethod(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	srv, _ := newTestServer(t)
	ts := httptest.NewServer(http.HandlerFunc(srv.handleConnect))
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

func TestProxy_DialFailure(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	srv, _ := newTestServer(t)
	// Very short dial timeout so the test doesn't block long.
	srv.DialTimeout = 100 * time.Millisecond

	ts := httptest.NewServer(http.HandlerFunc(srv.handleConnect))
	t.Cleanup(ts.Close)

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()

	// Target a port that has nothing listening.
	fmt.Fprint(conn, "CONNECT 127.0.0.1:1 HTTP/1.1\r\nHost: 127.0.0.1:1\r\n\r\n")

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestProxy_HalfClose(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	echoAddr := startEchoServer(t)
	srv, _ := newTestServer(t)

	ts := httptest.NewServer(http.HandlerFunc(srv.handleConnect))
	t.Cleanup(ts.Close)

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	require.NoError(t, err)

	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Half-close the client side; relay goroutines must drain and exit.
	tc := conn.(*net.TCPConn)
	_ = tc.CloseWrite()

	// Give relay goroutines time to notice and exit.
	time.Sleep(50 * time.Millisecond)
	tc.Close()
	// goleak.VerifyNone above catches any leaked goroutines.
}

func TestProxy_HealthEndpoint(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	_, reg := newTestServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/metrics", metricsHandler(reg))

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestProxy_Metrics(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	echoAddr := startEchoServer(t)
	srv, reg := newTestServer(t)

	ts := httptest.NewServer(http.HandlerFunc(srv.handleConnect))
	t.Cleanup(ts.Close)

	// Before any connection.
	assert.Equal(t, float64(0), gaugeValue(t, reg, "actions_gateway_proxy_connections_active"))
	assert.Equal(t, float64(0), gaugeValue(t, reg, "actions_gateway_proxy_connections_total"))

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	require.NoError(t, err)

	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Active gauge should be 1 while tunnel is open.
	require.Eventually(t, func() bool {
		return gaugeValue(t, reg, "actions_gateway_proxy_connections_active") == 1
	}, time.Second, 5*time.Millisecond)
	assert.Equal(t, float64(1), gaugeValue(t, reg, "actions_gateway_proxy_connections_total"))

	// Close connection — active gauge drops to 0.
	conn.Close()
	require.Eventually(t, func() bool {
		return gaugeValue(t, reg, "actions_gateway_proxy_connections_active") == 0
	}, time.Second, 5*time.Millisecond)
	assert.Equal(t, float64(1), gaugeValue(t, reg, "actions_gateway_proxy_connections_total"))
}

// metricsHandler returns a handler that serves gathered metrics from reg.
func metricsHandler(reg *prometheus.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mfs, _ := reg.Gather()
		for _, mf := range mfs {
			io.WriteString(w, mf.String())
		}
	})
}

// §6 — ListenAndServe lifecycle

// freeAddr returns an available 127.0.0.1 address by briefly binding then releasing it.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestServer_ListenAndServeShutdown(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	reg := prometheus.NewRegistry()
	srv := NewServer(freeAddr(t), freeAddr(t), 5*time.Second, nil, reg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()

	// Wait for proxy port to accept connections before cancelling.
	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", srv.Addr, 50*time.Millisecond)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}, 2*time.Second, 10*time.Millisecond)

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe did not return within 2s after context cancellation")
	}
}

func TestServer_ListenAndServeBothServersReachable(t *testing.T) {
	// goleak registered first → runs last, after server and echo listener are cleaned up.
	t.Cleanup(func() { goleak.VerifyNone(t) })

	echoAddr := startEchoServer(t)
	reg := prometheus.NewRegistry()
	srv := NewServer(freeAddr(t), freeAddr(t), 5*time.Second, nil, reg)

	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.ListenAndServe(ctx) }()
	// cancel registered after goleak and echo ln.Close → runs before them (LIFO).
	t.Cleanup(func() {
		cancel()
		<-serverDone
	})

	// Wait for proxy port to be ready.
	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", srv.Addr, 50*time.Millisecond)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}, 2*time.Second, 10*time.Millisecond)

	// Health endpoint returns 200.
	resp, err := http.Get("http://" + srv.HealthAddr + "/healthz")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// CONNECT request through the proxy succeeds.
	conn, err := net.Dial("tcp", srv.Addr)
	require.NoError(t, err)
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	connectResp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	conn.Close()
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, connectResp.StatusCode)
}
