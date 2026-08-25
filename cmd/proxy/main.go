// Command proxy is a minimal stateless HTTPS CONNECT proxy for GitHub Actions workers.
//
// Environment variables:
//
//	PROXY_PORT                   - CONNECT listener port (default 8080)
//	PROXY_HEALTH_PORT            - Health (/healthz, /readyz) port (default 8081)
//	PROXY_METRICS_PORT           - mTLS /metrics port (default 8443)
//	PROXY_DIAL_TIMEOUT           - Upstream TCP dial timeout (default 10s)
//	PROXY_SHUTDOWN_DRAIN_TIMEOUT - Total graceful drain budget on SIGTERM, linger included; in-flight CONNECT tunnels still open when it expires are cut (default 45s)
//	PROXY_SHUTDOWN_LINGER        - Ceiling on holding the CONNECT listener open after SIGTERM so endpoint removal can propagate; exits early once arrivals go quiet. Spent inside the drain budget. Negative disables (default 10s)
//	PROXY_TLS_CERT_FILE          - Path to CONNECT TLS certificate; enables TLS when paired with PROXY_TLS_KEY_FILE
//	PROXY_TLS_KEY_FILE           - Path to CONNECT TLS private key;  enables TLS when paired with PROXY_TLS_CERT_FILE
//	PROXY_METRICS_TLS_CERT_FILE  - Path to metrics server cert; enables mTLS metrics with the key + client CA below
//	PROXY_METRICS_TLS_KEY_FILE   - Path to metrics server key
//	PROXY_METRICS_CLIENT_CA_FILE - Path to CA that scraper client certs are verified against
//	PROXY_ALLOWED_HOST_SUFFIXES  - Comma-separated CONNECT destination allowlist by DNS host suffix (Q242 G.1); empty ⇒ transport-only (NetworkPolicy is the gate)
//	PROXY_ALLOWED_CIDRS          - Comma-separated CONNECT destination allowlist by IP range (CIDR); empty ⇒ no CIDR allowance
//	PROXY_AUDIT_LOGGING          - Per-connection egress audit record: Off (default) or Connections; anything unrecognized is Off (Q564 G.3)
//	POD_NAMESPACE                - Tenant namespace stamped on the audit record; downward-API supplied, empty omits the field
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	// Structured JSON with an explicit level source. LOG_LEVEL (info|debug,
	// default info) gives the proxy the same single level knob as the
	// controllers (k8s audit F1) that the GMC can crank per tenant without a
	// code change (logging-audit Theme G).
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

	var drainTimeout time.Duration
	if v := os.Getenv("PROXY_SHUTDOWN_DRAIN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse PROXY_SHUTDOWN_DRAIN_TIMEOUT: %w", err)
		}
		drainTimeout = d
	}

	// Zero means "unset ⇒ default", so an operator disabling the linger (a
	// truncated node-shutdown window is the motivating case — see
	// docs/operations/troubleshooting.md) passes a negative duration.
	var shutdownLinger time.Duration
	if v := os.Getenv("PROXY_SHUTDOWN_LINGER"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse PROXY_SHUTDOWN_LINGER: %w", err)
		}
		if d == 0 {
			d = -1
		}
		shutdownLinger = d
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

	// CONNECT destination allowlist (Q242 G.1). The GMC injects the full permitted
	// set (GitHub hosts/ranges + any operator-allowlisted destinations); empty
	// leaves the proxy transport-only.
	srv.AllowedHostSuffixes = splitList(os.Getenv("PROXY_ALLOWED_HOST_SUFFIXES"))
	cidrs, err := parseCIDRList(os.Getenv("PROXY_ALLOWED_CIDRS"))
	if err != nil {
		return fmt.Errorf("parse PROXY_ALLOWED_CIDRS: %w", err)
	}
	srv.AllowedCIDRs = cidrs

	// Per-connection egress audit (Q564 G.3). Absent env ⇒ Off, so a proxy the
	// GMC has not opted in — and one run standalone — records nothing per
	// connection. The namespace comes from the downward API, never the request.
	srv.AuditLogging = parseAuditMode(os.Getenv("PROXY_AUDIT_LOGGING"))
	srv.Namespace = os.Getenv("POD_NAMESPACE")

	srv.ShutdownDrainTimeout = drainTimeout
	srv.ShutdownLinger = shutdownLinger

	tlsEnabled := srv.TLSCertFile != "" && srv.TLSKeyFile != ""
	metricsMTLS := srv.MetricsTLSCertFile != "" && srv.MetricsTLSKeyFile != "" && srv.MetricsClientCAFile != ""
	log.Info("proxy starting", "proxyPort", proxyPort, "healthPort", healthPort,
		"metricsPort", metricsPort, "tls", tlsEnabled, "metricsMTLS", metricsMTLS,
		"auditLogging", string(srv.AuditLogging))
	// Every rollout, node drain, eviction, and scale-down delivers SIGTERM here.
	// Without a handler the default action terminates the process outright and
	// every in-flight CONNECT tunnel dies with it, cutting live CI egress
	// mid-request (Q384); NotifyContext turns it into the cancellation that
	// ListenAndServe drains on.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return srv.ListenAndServe(ctx)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// splitList parses a comma-separated env value into a trimmed, empty-free slice.
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseCIDRList parses a comma-separated list of CIDRs, failing closed on any
// malformed entry (the GMC webhook validates these before injection, so a bad
// value here means a config bug, not tenant input).
func parseCIDRList(s string) ([]*net.IPNet, error) {
	items := splitList(s)
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]*net.IPNet, 0, len(items))
	for _, item := range items {
		_, n, err := net.ParseCIDR(item)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", item, err)
		}
		out = append(out, n)
	}
	return out, nil
}
