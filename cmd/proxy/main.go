// Command proxy is a minimal stateless HTTPS CONNECT proxy for GitHub Actions workers.
//
// Environment variables:
//
//	PROXY_PORT         - CONNECT listener port (default 8080)
//	PROXY_HEALTH_PORT  - Health + metrics port (default 8081)
//	PROXY_DIAL_TIMEOUT - Upstream TCP dial timeout (default 10s)
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

	log.Info("proxy starting", "proxyPort", proxyPort, "healthPort", healthPort)
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
