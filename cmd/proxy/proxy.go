// Package main implements a minimal stateless HTTPS CONNECT proxy.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server is a minimal stateless HTTPS CONNECT proxy.
// It handles only CONNECT tunneling — no TLS termination, no inspection.
type Server struct {
	// Addr is the listen address for CONNECT requests. Default ":8080".
	Addr string
	// HealthAddr is the listen address for /healthz and /readyz (plaintext,
	// kubelet probes). Default ":8081".
	HealthAddr string
	// MetricsAddr is the listen address for the mTLS /metrics endpoint. When the
	// metrics mTLS files below are all set, /metrics is served here over HTTPS
	// requiring a CA-signed client cert; otherwise /metrics is served plaintext
	// on HealthAddr (dev/test fallback).
	MetricsAddr string
	// MetricsTLSCertFile / MetricsTLSKeyFile / MetricsClientCAFile configure the
	// mTLS metrics listener. All three must be set to enable it: the first two
	// are the server cert+key, the third is the CA that scraper client certs are
	// verified against (Q69).
	MetricsTLSCertFile  string
	MetricsTLSKeyFile   string
	MetricsClientCAFile string
	// DialTimeout is the upstream TCP dial timeout. Default 10s.
	DialTimeout time.Duration
	// ReadHeaderTimeout caps how long the server waits for request headers on
	// both the CONNECT and health listeners. Default 5s. Mitigates slowloris.
	ReadHeaderTimeout time.Duration
	// HTTPIdleTimeout caps idle keep-alive on both HTTP listeners. Default 60s.
	// Distinct from TunnelIdleTimeout, which applies to the hijacked CONNECT relay.
	HTTPIdleTimeout time.Duration
	// MaxTunnelLifetime is the hard upper bound on a single CONNECT tunnel.
	// Default 6h. A stalled long-poll cannot tie up a relay goroutine beyond this.
	MaxTunnelLifetime time.Duration
	// TunnelIdleTimeout is the per-direction idle deadline applied to the
	// hijacked CONNECT relay. Reset on every successful read. Default 5m.
	TunnelIdleTimeout time.Duration
	Log               *slog.Logger
	// TLSCertFile and TLSKeyFile enable TLS on the CONNECT listener when both are set.
	// The health port always remains plaintext.
	TLSCertFile string
	TLSKeyFile  string

	connectionsActive *prometheus.GaugeVec
	connectionsTotal  *prometheus.CounterVec
	dialErrors        *prometheus.CounterVec
	tunnelDuration    *prometheus.HistogramVec

	// metricsGatherer is the registry the /metrics endpoint serves — the same
	// one the proxy metrics above are registered on, so a custom registry (tests,
	// or multiple in-process servers) is served consistently instead of always
	// falling back to the global default registry.
	metricsGatherer prometheus.Gatherer

	// ready is closed by ListenAndServe once the CONNECT listener has bound.
	// /readyz returns 200 only after this channel is closed, so the kubelet
	// keeps the pod out of the Service EndpointSlice until workers can
	// actually reach the CONNECT port. Mirrors the §11.D GMC webhook
	// readiness fix.
	ready chan struct{}
}

const (
	defaultReadHeaderTimeout = 5 * time.Second
	defaultHTTPIdleTimeout   = 60 * time.Second
	defaultMaxTunnelLifetime = 6 * time.Hour
	defaultTunnelIdleTimeout = 5 * time.Minute
)

// NewServer returns a Server with metrics registered on reg.
func NewServer(addr, healthAddr string, dialTimeout time.Duration, log *slog.Logger, reg prometheus.Registerer) *Server {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	active := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "actions_gateway_proxy_connections_active",
		Help: "Currently active CONNECT tunnels.",
	}, nil)
	total := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "actions_gateway_proxy_connections_total",
		Help: "Total CONNECT tunnels opened.",
	}, nil)
	dialErr := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "actions_gateway_proxy_dial_errors_total",
		Help: "Upstream dial failures.",
	}, nil)
	tunnelDur := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "actions_gateway_proxy_tunnel_duration_seconds",
		Help:    "Duration of CONNECT tunnels in seconds, observed at tunnel close.",
		Buckets: []float64{0.1, 0.5, 1, 5, 10, 60, 300, 1800, 3600, 21600},
	}, nil)
	reg.MustRegister(active, total, dialErr, tunnelDur)

	// Serve from the same registry the metrics were registered on. A
	// *prometheus.Registry satisfies both Registerer and Gatherer; the default
	// registerer is also the default gatherer.
	gatherer, ok := reg.(prometheus.Gatherer)
	if !ok {
		gatherer = prometheus.DefaultGatherer
	}

	return &Server{
		Addr:              addr,
		HealthAddr:        healthAddr,
		DialTimeout:       dialTimeout,
		Log:               log,
		connectionsActive: active,
		connectionsTotal:  total,
		dialErrors:        dialErr,
		tunnelDuration:    tunnelDur,
		metricsGatherer:   gatherer,
		ready:             make(chan struct{}),
	}
}

// ListenAndServe starts both the CONNECT listener and the health server.
// Blocks until ctx is cancelled.
//
// Both listeners are bound synchronously before either serve loop starts and
// before s.ready is closed. Binding the CONNECT socket puts it in LISTEN state
// at the kernel level, so workers can complete the TCP handshake the instant
// /readyz flips to 200 — no race window where the EndpointSlice points at a
// pod whose CONNECT port is still unbound.
func (s *Server) ListenAndServe(ctx context.Context) error {
	metricsEnabled := s.MetricsTLSCertFile != "" && s.MetricsTLSKeyFile != "" && s.MetricsClientCAFile != ""

	// Build the metrics mTLS config first so a bad cert fails fast, before any
	// listener is bound.
	var metricsTLS *tls.Config
	if metricsEnabled {
		var err error
		metricsTLS, err = s.metricsTLSConfig()
		if err != nil {
			return fmt.Errorf("configure metrics mTLS: %w", err)
		}
	}

	connectLn, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("bind connect listener: %w", err)
	}
	s.Addr = connectLn.Addr().String()

	healthLn, err := net.Listen("tcp", s.HealthAddr)
	if err != nil {
		_ = connectLn.Close()
		return fmt.Errorf("bind health listener: %w", err)
	}
	s.HealthAddr = healthLn.Addr().String()

	var metricsLn net.Listener
	if metricsEnabled {
		metricsLn, err = net.Listen("tcp", s.MetricsAddr)
		if err != nil {
			_ = connectLn.Close()
			_ = healthLn.Close()
			return fmt.Errorf("bind metrics listener: %w", err)
		}
		s.MetricsAddr = metricsLn.Addr().String()
	}

	close(s.ready)

	readHeaderTimeout := s.ReadHeaderTimeout
	if readHeaderTimeout == 0 {
		readHeaderTimeout = defaultReadHeaderTimeout
	}
	httpIdleTimeout := s.HTTPIdleTimeout
	if httpIdleTimeout == 0 {
		httpIdleTimeout = defaultHTTPIdleTimeout
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", s.handleReadyz)
	if !metricsEnabled {
		// Dev/test fallback: serve metrics plaintext on the health port when no
		// mTLS bundle is configured. In production the GMC always mounts the
		// bundle, so metrics are served over mTLS on the dedicated listener below.
		mux.Handle("/metrics", promhttp.HandlerFor(s.metricsGatherer, promhttp.HandlerOpts{}))
	}
	healthSrv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       httpIdleTimeout,
	}

	proxySrv := &http.Server{
		Handler: http.HandlerFunc(s.handleConnect),
		// ReadHeaderTimeout caps the CONNECT request-line + headers read.
		// ReadTimeout is intentionally NOT set — the CONNECT body is hijacked
		// and a non-zero ReadTimeout would cap the post-handshake tunnel
		// lifetime to a fixed value. Per-tunnel deadlines live in handleConnect.
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       httpIdleTimeout,
		// CONNECT is HTTP/1.1-only. Without disabling HTTP/2, Go's http.Server
		// negotiates h2 via ALPN when TLS is configured; the AGC's HTTPS proxy
		// client then sends an HTTP/1.1 CONNECT line over what is now an HTTP/2
		// connection and the proxy responds with an HTTP/2 SETTINGS frame —
		// surfaced to the client as `malformed HTTP response`.
		//
		// MinVersion is pinned to TLS 1.2 to match the metrics listener
		// (metricsTLSConfig) rather than inheriting Go's default floor: the
		// worker→proxy CONNECT leg carries every tenant's GitHub-bound traffic,
		// so its TLS floor is a tenant-isolation boundary and must be explicit
		// and modern, not whatever the toolchain happens to default to.
		TLSConfig:    &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
	}

	var metricsSrv *http.Server
	if metricsEnabled {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", promhttp.HandlerFor(s.metricsGatherer, promhttp.HandlerOpts{}))
		metricsSrv = &http.Server{
			Handler:           metricsMux,
			TLSConfig:         metricsTLS,
			ReadHeaderTimeout: readHeaderTimeout,
			IdleTimeout:       httpIdleTimeout,
			// HTTP/1.1 only — consistent with the CONNECT listener and avoids the
			// h2-over-ALPN surprise documented on proxySrv below.
			TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
		}
	}

	errCh := make(chan error, 3)
	go func() { errCh <- healthSrv.Serve(healthLn) }()
	if s.TLSCertFile != "" && s.TLSKeyFile != "" {
		go func() { errCh <- proxySrv.ServeTLS(connectLn, s.TLSCertFile, s.TLSKeyFile) }()
	} else {
		go func() { errCh <- proxySrv.Serve(connectLn) }()
	}
	if metricsSrv != nil {
		// Cert+key live in metricsSrv.TLSConfig.Certificates, so ServeTLS gets "".
		go func() { errCh <- metricsSrv.ServeTLS(metricsLn, "", "") }()
	}

	select {
	case <-ctx.Done():
		_ = proxySrv.Shutdown(context.Background())
		_ = healthSrv.Shutdown(context.Background())
		if metricsSrv != nil {
			_ = metricsSrv.Shutdown(context.Background())
		}
		return nil
	case err := <-errCh:
		return err
	}
}

// metricsTLSConfig builds the mTLS server config for the metrics listener:
// the server cert+key plus the CA that scraper client certificates are verified
// against. RequireAndVerifyClientCert means the TLS handshake itself rejects any
// client that does not present a CA-signed certificate.
func (s *Server) metricsTLSConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(s.MetricsTLSCertFile, s.MetricsTLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load metrics server cert/key: %w", err)
	}
	caPEM, err := os.ReadFile(s.MetricsClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read metrics client CA %s: %w", s.MetricsClientCAFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no certificates parsed from %s", s.MetricsClientCAFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	}, nil
}

// handleReadyz returns 200 only once the CONNECT listener has bound. The
// readiness probe gates a worker pod's HTTPS_PROXY traffic on the proxy
// kernel socket being in LISTEN state — without this, kubelet adds the
// pod to the Service EndpointSlice as soon as the health port is up,
// and concurrent worker traffic races the proxy serve goroutine.
func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	select {
	case <-s.ready:
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "only CONNECT is supported", http.StatusMethodNotAllowed)
		return
	}

	dialTimeout := s.DialTimeout
	if dialTimeout == 0 {
		dialTimeout = 10 * time.Second
	}

	upstream, err := net.DialTimeout("tcp", r.Host, dialTimeout)
	if err != nil {
		s.dialErrors.WithLabelValues().Inc()
		log := s.Log
		if log == nil {
			log = slog.Default()
		}
		log.Error("upstream dial failed", "host", r.Host, "error", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.Close() }()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		// The client is already gone or the conn is broken: counting and
		// tunneling a connection whose CONNECT-200 reply never landed would
		// dirty the metrics and immediately die in io.Copy. Bail before either.
		// The deferred conn.Close()/upstream.Close() handle cleanup.
		log := s.Log
		if log == nil {
			log = slog.Default()
		}
		log.Debug("CONNECT response write failed", "host", r.Host, "error", err)
		return
	}

	s.connectionsTotal.WithLabelValues().Inc()
	s.connectionsActive.WithLabelValues().Inc()
	defer s.connectionsActive.WithLabelValues().Dec()

	maxLifetime := s.MaxTunnelLifetime
	if maxLifetime == 0 {
		maxLifetime = defaultMaxTunnelLifetime
	}
	idleTimeout := s.TunnelIdleTimeout
	if idleTimeout == 0 {
		idleTimeout = defaultTunnelIdleTimeout
	}
	hardDeadline := time.Now().Add(maxLifetime)

	start := time.Now()
	defer func() {
		s.tunnelDuration.WithLabelValues().Observe(time.Since(start).Seconds())
	}()

	clientSrc := &idleDeadlineConn{Conn: conn, idle: idleTimeout, hardDeadline: hardDeadline}
	upstreamSrc := &idleDeadlineConn{Conn: upstream, idle: idleTimeout, hardDeadline: hardDeadline}

	done := make(chan struct{}, 2)
	relay := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}
	go relay(upstream, clientSrc)
	go relay(conn, upstreamSrc)
	<-done
}

// idleDeadlineConn refreshes the underlying conn's read deadline on every
// Read so an idle stream is torn down after `idle` of inactivity, while
// hardDeadline imposes an absolute upper bound on tunnel lifetime.
type idleDeadlineConn struct {
	net.Conn
	idle         time.Duration
	hardDeadline time.Time
}

func (c *idleDeadlineConn) Read(p []byte) (int, error) {
	deadline := time.Now().Add(c.idle)
	if !c.hardDeadline.IsZero() && deadline.After(c.hardDeadline) {
		deadline = c.hardDeadline
	}
	_ = c.Conn.SetReadDeadline(deadline)
	return c.Conn.Read(p)
}
