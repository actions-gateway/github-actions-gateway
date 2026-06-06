// Command proxy is a minimal stateless HTTPS CONNECT proxy for GitHub Actions workers.
//
// Environment variables:
//
//	PROXY_PORT                   - CONNECT listener port (default 8080)
//	PROXY_HEALTH_PORT            - Health (/healthz, /readyz) port (default 8081)
//	PROXY_METRICS_PORT           - mTLS /metrics port (default 8443)
//	PROXY_DIAL_TIMEOUT           - Upstream TCP dial timeout (default 10s)
//	PROXY_TLS_CERT_FILE          - Path to CONNECT TLS certificate; enables TLS when paired with PROXY_TLS_KEY_FILE
//	PROXY_TLS_KEY_FILE           - Path to CONNECT TLS private key;  enables TLS when paired with PROXY_TLS_CERT_FILE
//	PROXY_METRICS_TLS_CERT_FILE  - Path to metrics server cert; enables mTLS metrics with the key + client CA below
//	PROXY_METRICS_TLS_KEY_FILE   - Path to metrics server key
//	PROXY_METRICS_CLIENT_CA_FILE - Path to CA that scraper client certs are verified against
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("startup failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	proxyPort := envOr("PROXY_PORT", "8080")
	healthPort := envOr("PROXY_HEALTH_PORT", "8081")
	metricsPort := envOr("PROXY_METRICS_PORT", "8443")

	dialTimeout := 10 * time.Second
	if v := os.Getenv("PROXY_DIAL_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse PROXY_DIAL_TIMEOUT: %w", err)
		}
		dialTimeout = d
	}

	srv := NewServer(
		":"+proxyPort,
		":"+healthPort,
		dialTimeout,
		log,
		nil,
	)
	srv.TLSCertFile = os.Getenv("PROXY_TLS_CERT_FILE")
	srv.TLSKeyFile = os.Getenv("PROXY_TLS_KEY_FILE")
	srv.MetricsAddr = ":" + metricsPort
	srv.MetricsTLSCertFile = os.Getenv("PROXY_METRICS_TLS_CERT_FILE")
	srv.MetricsTLSKeyFile = os.Getenv("PROXY_METRICS_TLS_KEY_FILE")
	srv.MetricsClientCAFile = os.Getenv("PROXY_METRICS_CLIENT_CA_FILE")

	tlsEnabled := srv.TLSCertFile != "" && srv.TLSKeyFile != ""
	metricsMTLS := srv.MetricsTLSCertFile != "" && srv.MetricsTLSKeyFile != "" && srv.MetricsClientCAFile != ""
	log.Info("proxy starting", "proxyPort", proxyPort, "healthPort", healthPort,
		"metricsPort", metricsPort, "tls", tlsEnabled, "metricsMTLS", metricsMTLS)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	return srv.ListenAndServe(ctx)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
