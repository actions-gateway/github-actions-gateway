package main

import (
	"bufio"
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
