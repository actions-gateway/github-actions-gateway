package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
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
	defer func() { _ = conn.Close() }()

	// Send CONNECT request.
	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)

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
	defer func() { _ = resp.Body.Close() }()
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
	defer func() { _ = conn.Close() }()

	// Target a port that has nothing listening.
	_, _ = fmt.Fprint(conn, "CONNECT 127.0.0.1:1 HTTP/1.1\r\nHost: 127.0.0.1:1\r\n\r\n")

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

	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Half-close the client side; relay goroutines must drain and exit.
	tc := conn.(*net.TCPConn)
	_ = tc.CloseWrite()

	// Give relay goroutines time to notice and exit.
	time.Sleep(50 * time.Millisecond)
	_ = tc.Close()
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
	defer func() { _ = resp.Body.Close() }()
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

	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Active gauge should be 1 while tunnel is open.
	require.Eventually(t, func() bool {
		return gaugeValue(t, reg, "actions_gateway_proxy_connections_active") == 1
	}, time.Second, 5*time.Millisecond)
	assert.Equal(t, float64(1), gaugeValue(t, reg, "actions_gateway_proxy_connections_total"))

	// Close connection — active gauge drops to 0.
	_ = conn.Close()
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
			_, _ = io.WriteString(w, mf.String())
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
	_ = ln.Close()
	return addr
}

// startServer launches srv.ListenAndServe on a background context and blocks
// until the server has bound all of its listeners — i.e. s.ready is closed.
// Returning only after ready closes gives the caller a happens-before edge to
// the bind-time writes ListenAndServe makes to the resolved s.Addr /
// s.HealthAddr / s.MetricsAddr fields (each is rewritten from a ":0" request to
// the concrete bound port). Without that edge, reading those fields from the
// test goroutine races the serve goroutine's writes under -race. s.ready is the
// same gate production consumers key off (the /readyz probe), so waiting on it
// is both correct and faithful — and since the listeners are in LISTEN state by
// the time it closes, a dial or request issued afterwards is accepted from the
// kernel backlog even before the Serve loop calls Accept. Cancelling the
// context and draining the serve goroutine are registered as a t.Cleanup.
func startServer(t *testing.T, srv *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-srv.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not bind its listeners (s.ready) within 2s")
	}
}

func TestServer_ListenAndServeShutdown(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	reg := prometheus.NewRegistry()
	srv := NewServer(freeAddr(t), freeAddr(t), 5*time.Second, nil, reg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()

	// Wait for both listeners to bind (s.ready) before cancelling.
	select {
	case <-srv.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not bind its listeners (s.ready) within 2s")
	}

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe did not return within 2s after context cancellation")
	}
}

// writeTestTLSCert generates a self-signed RSA cert for 127.0.0.1 and writes
// cert.pem/key.pem under t.TempDir(), returning their paths.
func writeTestTLSCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	require.NoError(t, os.WriteFile(certPath, certPEM, 0o600))

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	require.NoError(t, os.WriteFile(keyPath, keyPEM, 0o600))

	return certPath, keyPath
}

// TestProxy_TLS_RejectsHTTP2_ALPN guards against the regression fixed in PR #59
// (`fix(proxy): disable HTTP/2 on the TLS CONNECT listener`). A client offering
// both h2 and http/1.1 via ALPN must be downgraded to http/1.1, and a CONNECT
// over that handshake must succeed with `HTTP/1.1 200 Connection established`.
func TestProxy_TLS_RejectsHTTP2_ALPN(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	echoAddr := startEchoServer(t)
	certPath, keyPath := writeTestTLSCert(t)

	reg := prometheus.NewRegistry()
	srv := NewServer(freeAddr(t), freeAddr(t), 5*time.Second, nil, reg)
	srv.TLSCertFile = certPath
	srv.TLSKeyFile = keyPath

	startServer(t, srv)

	// Client advertises h2 first, then http/1.1. A correctly configured server
	// must select http/1.1.
	tlsConn, err := tls.Dial("tcp", srv.Addr, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // G402: test client probing a self-signed local listener
		NextProtos:         []string{"h2", "http/1.1"},
	})
	require.NoError(t, err)
	defer func() { _ = tlsConn.Close() }()

	assert.Equal(t, "http/1.1", tlsConn.ConnectionState().NegotiatedProtocol,
		"server must negotiate http/1.1 even when client offers h2 first — HTTP/2 must be disabled on the CONNECT listener")

	// CONNECT over the TLS tunnel must return the canonical HTTP/1.1 status line.
	_, err = fmt.Fprintf(tlsConn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	require.NoError(t, err)

	br := bufio.NewReader(tlsConn)
	statusLine, err := br.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "HTTP/1.1 200 Connection established\r\n", statusLine)
}

// histogramCount returns the sample count for the first metric in the named
// histogram, or 0 if the metric is not present.
func histogramCount(t *testing.T, reg *prometheus.Registry, name string) uint64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if h := m.GetHistogram(); h != nil {
				return h.GetSampleCount()
			}
		}
	}
	return 0
}

// TestProxy_HealthPortReadHeaderTimeout asserts the slowloris guard on the
// health server: an idle client that never sends headers must be disconnected
// by the server within ReadHeaderTimeout (M-17).
func TestProxy_HealthPortReadHeaderTimeout(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	reg := prometheus.NewRegistry()
	srv := NewServer(freeAddr(t), freeAddr(t), 5*time.Second, nil, reg)
	srv.ReadHeaderTimeout = 200 * time.Millisecond

	startServer(t, srv)

	c, err := net.Dial("tcp", srv.HealthAddr)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	// Send nothing. The server must close the connection after ReadHeaderTimeout.
	// Cap our read at 3s so a hung server fails the test deterministically.
	require.NoError(t, c.SetReadDeadline(time.Now().Add(3*time.Second)))
	start := time.Now()
	buf := make([]byte, 1)
	_, err = c.Read(buf)
	elapsed := time.Since(start)

	assert.Error(t, err, "server must close idle conn after ReadHeaderTimeout")
	assert.Less(t, elapsed, 2*time.Second,
		"connection closed at %s; expected close by server within ReadHeaderTimeout (200ms), not our 3s read deadline",
		elapsed,
	)
}

// TestProxy_ConnectPortReadHeaderTimeout asserts ReadHeaderTimeout on the
// CONNECT listener too. The proxy port is reachable from any worker pod in
// the tenant namespace, so the slowloris guard there matters most (M-17).
func TestProxy_ConnectPortReadHeaderTimeout(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	reg := prometheus.NewRegistry()
	srv := NewServer(freeAddr(t), freeAddr(t), 5*time.Second, nil, reg)
	srv.ReadHeaderTimeout = 200 * time.Millisecond

	startServer(t, srv)

	c, err := net.Dial("tcp", srv.Addr)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

	require.NoError(t, c.SetReadDeadline(time.Now().Add(3*time.Second)))
	start := time.Now()
	buf := make([]byte, 1)
	_, err = c.Read(buf)
	elapsed := time.Since(start)

	assert.Error(t, err)
	assert.Less(t, elapsed, 2*time.Second,
		"connection closed at %s; expected close by server within ReadHeaderTimeout (200ms)",
		elapsed,
	)
}

// TestProxy_TunnelIdleTimeout asserts that an established CONNECT tunnel is
// torn down after TunnelIdleTimeout of inactivity (M-18). With no data
// flowing in either direction the relay's Read deadline fires, io.Copy
// returns, and both ends are closed.
func TestProxy_TunnelIdleTimeout(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	echoAddr := startEchoServer(t)
	reg := prometheus.NewRegistry()
	srv := NewServer(freeAddr(t), freeAddr(t), 5*time.Second, nil, reg)
	srv.TunnelIdleTimeout = 100 * time.Millisecond

	startServer(t, srv)

	conn, err := net.Dial("tcp", srv.Addr)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Sit idle — no bytes written. After ~100ms the relay closes.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	start := time.Now()
	buf := make([]byte, 1)
	_, err = br.Read(buf)
	elapsed := time.Since(start)

	assert.Error(t, err, "tunnel must close on idle timeout")
	assert.Less(t, elapsed, 1*time.Second,
		"tunnel closed at %s; expected idle close within TunnelIdleTimeout (100ms)",
		elapsed,
	)
}

// TestProxy_TunnelLifetimeCap asserts MaxTunnelLifetime caps absolute tunnel
// duration even when traffic keeps the idle deadline refreshed (M-18).
func TestProxy_TunnelLifetimeCap(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	echoAddr := startEchoServer(t)
	reg := prometheus.NewRegistry()
	srv := NewServer(freeAddr(t), freeAddr(t), 5*time.Second, nil, reg)
	srv.MaxTunnelLifetime = 200 * time.Millisecond
	srv.TunnelIdleTimeout = 10 * time.Second // long enough that idle won't fire first

	startServer(t, srv)

	conn, err := net.Dial("tcp", srv.Addr)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	start := time.Now()
	buf := make([]byte, 1)
	_, err = br.Read(buf)
	elapsed := time.Since(start)

	assert.Error(t, err)
	assert.Less(t, elapsed, 1*time.Second,
		"tunnel closed at %s; expected hard-lifetime close (200ms) before idle (10s)",
		elapsed,
	)
}

// TestProxy_TunnelDurationHistogram asserts the tunnel_duration_seconds
// histogram records a sample for each completed tunnel.
func TestProxy_TunnelDurationHistogram(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	echoAddr := startEchoServer(t)
	reg := prometheus.NewRegistry()
	srv := NewServer(freeAddr(t), freeAddr(t), 5*time.Second, nil, reg)
	srv.TunnelIdleTimeout = 100 * time.Millisecond

	startServer(t, srv)

	conn, err := net.Dial("tcp", srv.Addr)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Let the idle timeout fire so the tunnel closes and the duration is observed.
	require.Eventually(t, func() bool {
		return histogramCount(t, reg, "actions_gateway_proxy_tunnel_duration_seconds") == 1
	}, 2*time.Second, 20*time.Millisecond,
		"expected one tunnel_duration_seconds sample after tunnel close",
	)
}

func TestServer_ListenAndServeBothServersReachable(t *testing.T) {
	// goleak registered first → runs last, after server and echo listener are cleaned up.
	t.Cleanup(func() { goleak.VerifyNone(t) })

	echoAddr := startEchoServer(t)
	reg := prometheus.NewRegistry()
	srv := NewServer(freeAddr(t), freeAddr(t), 5*time.Second, nil, reg)

	// startServer's cancel/drain cleanup is registered after goleak's and the
	// echo server's, so by LIFO it runs first — server stops before the echo
	// listener closes and before goleak's leak check.
	startServer(t, srv)

	// Health endpoint returns 200.
	resp, err := http.Get("http://" + srv.HealthAddr + "/healthz")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// CONNECT request through the proxy succeeds.
	conn, err := net.Dial("tcp", srv.Addr)
	require.NoError(t, err)
	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	connectResp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	_ = conn.Close()
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, connectResp.StatusCode)
}

// TestProxy_ReadyzHandler_GatesOnReadyChannel asserts handleReadyz returns 503
// while s.ready is open and 200 once it closes. Unit-level guarantee that the
// gate is wired correctly — the integration assertion that /readyz only flips
// after the CONNECT bind succeeds lives in TestServer_ReadyzImpliesConnectBound.
func TestProxy_ReadyzHandler_GatesOnReadyChannel(t *testing.T) {
	reg := prometheus.NewRegistry()
	srv := NewServer("", "", 5*time.Second, nil, reg)

	rec := httptest.NewRecorder()
	srv.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "/readyz must be 503 while ready channel is open")

	close(srv.ready)

	rec = httptest.NewRecorder()
	srv.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, rec.Code, "/readyz must be 200 once ready channel is closed")
}

// TestServer_ReadyzImpliesConnectBound is the regression test for Q42: any
// time /readyz returns 200, the CONNECT port must accept TCP connections.
// Before the fix, /readyz (or /healthz used as the readiness probe) could
// return 200 while the CONNECT serve goroutine had not yet bound :8080,
// causing worker pods to hit `connection refused` on rollouts.
func TestServer_ReadyzImpliesConnectBound(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	reg := prometheus.NewRegistry()
	srv := NewServer(freeAddr(t), freeAddr(t), 5*time.Second, nil, reg)

	startServer(t, srv)

	// Poll /readyz until it returns 200 — and for every 200 response, the
	// CONNECT port must already accept TCP. A single 200 paired with a
	// connect-refused on s.Addr would mean the gate is broken.
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + srv.HealthAddr + "/readyz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		// /readyz says ready — CONNECT port MUST be bound.
		c, err := net.DialTimeout("tcp", srv.Addr, 100*time.Millisecond)
		require.NoError(t, err, "/readyz returned 200 but CONNECT port refused TCP — Q42 regression")
		_ = c.Close()
		return true
	}, 2*time.Second, 10*time.Millisecond)
}
