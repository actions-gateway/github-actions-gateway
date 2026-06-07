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
	"strings"
	"time"
)

func main() {
	// Structured JSON with an explicit level source. LOG_LEVEL (info|debug,
	// default info) gives the proxy the same single level knob as the
	// controllers (k8s audit F1) that the GMC can crank per tenant without a
	// code change (logging-audit Theme G); the previous nil HandlerOptions
	// hard-coded info with no level source.
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevelFromEnv()}))
	if err := run(log); err != nil {
		log.Error("startup failed", "error", err)
		os.Exit(1)
	}
}

// logLevelFromEnv maps LOG_LEVEL (info|debug, default info) to a slog.Level.
func logLevelFromEnv() slog.Level {
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		return slog.LevelDebug
	}
	return slog.LevelInfo
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
