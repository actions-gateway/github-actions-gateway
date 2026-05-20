// Package main implements a minimal stateless HTTPS CONNECT proxy.
package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server is a minimal stateless HTTPS CONNECT proxy.
// It handles only CONNECT tunneling — no TLS termination, no inspection.
type Server struct {
	// Addr is the listen address for CONNECT requests. Default ":8080".
	Addr string
	// HealthAddr is the listen address for /healthz and /metrics. Default ":8081".
	HealthAddr  string
	// DialTimeout is the upstream TCP dial timeout. Default 10s.
	DialTimeout time.Duration
	Log         *slog.Logger

	connectionsActive *prometheus.GaugeVec
	connectionsTotal  *prometheus.CounterVec
	dialErrors        *prometheus.CounterVec
}

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
	reg.MustRegister(active, total, dialErr)

	return &Server{
		Addr:              addr,
		HealthAddr:        healthAddr,
		DialTimeout:       dialTimeout,
		Log:               log,
		connectionsActive: active,
		connectionsTotal:  total,
		dialErrors:        dialErr,
	}
}

// ListenAndServe starts both the CONNECT listener and the health server.
// Blocks until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/metrics", promhttp.Handler())
	healthSrv := &http.Server{Addr: s.HealthAddr, Handler: mux}

	proxySrv := &http.Server{
		Addr:    s.Addr,
		Handler: http.HandlerFunc(s.handleConnect),
	}

	errCh := make(chan error, 2)
	go func() { errCh <- healthSrv.ListenAndServe() }()
	go func() { errCh <- proxySrv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		_ = proxySrv.Shutdown(context.Background())
		_ = healthSrv.Shutdown(context.Background())
		return nil
	case err := <-errCh:
		return err
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
		http.Error(w, "upstream dial: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")

	s.connectionsTotal.WithLabelValues().Inc()
	s.connectionsActive.WithLabelValues().Inc()
	defer s.connectionsActive.WithLabelValues().Dec()

	done := make(chan struct{}, 2)
	relay := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}
	go relay(upstream, conn)
	go relay(conn, upstream)
	<-done
}
