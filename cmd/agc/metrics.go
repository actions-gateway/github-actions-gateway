package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// buildMetricsOptions configures the controller-runtime metrics server.
//
// When certDir contains the metrics mTLS server bundle (ca.crt + tls.crt +
// tls.key), the metrics endpoint is served over HTTPS requiring a client
// certificate signed by ca.crt — only the operator's scraper (holding the
// matching client cert published by the GMC) can read /metrics (Q69). No
// FilterProvider/TokenReview is used, so the AGC needs no extra RBAC.
//
// When the bundle is absent (local dev/test where the GMC has not mounted it),
// metrics fall back to plain HTTP. The GMC always mounts the bundle in
// production, so the effective default there is mTLS — mirroring the proxy's
// TLS-when-mounted pattern.
func buildMetricsOptions(certDir string, log logr.Logger) (metricsserver.Options, error) {
	opts := metricsserver.Options{BindAddress: metricsBindAddress}

	caPath := filepath.Join(certDir, "ca.crt")
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Info("metrics mTLS bundle absent; serving metrics over plain HTTP (dev/test only)", "dir", certDir)
			return opts, nil
		}
		return opts, fmt.Errorf("read metrics CA %s: %w", caPath, err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return opts, fmt.Errorf("no certificates parsed from %s", caPath)
	}

	opts.SecureServing = true
	opts.CertDir = certDir
	opts.CertName = "tls.crt"
	opts.KeyName = "tls.key"
	opts.TLSOpts = []func(*tls.Config){metricsClientCAVerifier(pool)}

	log.Info("serving metrics over mTLS", "addr", metricsBindAddress, "certDir", certDir)
	return opts, nil
}

// metricsClientCAVerifier returns a tls.Config mutator that requires and
// verifies a client certificate against pool, with a TLS 1.2 floor.
func metricsClientCAVerifier(pool *x509.CertPool) func(*tls.Config) {
	return func(c *tls.Config) {
		c.ClientAuth = tls.RequireAndVerifyClientCert
		c.ClientCAs = pool
		if c.MinVersion < tls.VersionTLS12 {
			c.MinVersion = tls.VersionTLS12
		}
	}
}
