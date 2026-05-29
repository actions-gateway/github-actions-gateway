// Package transport contains shared HTTP transport configuration helpers
// for the AGC, including the proxy CA trust pool used to validate the
// per-tenant egress proxy's self-signed TLS certificate without losing the
// ability to validate upstream GitHub endpoints over the proxy's CONNECT
// tunnel.
package transport

import (
	"crypto/x509"
	"fmt"
)

// BuildProxyTrustPool returns an x509 cert pool seeded from the system root
// store and extended with the supplied proxy CA PEM. The resulting pool is
// intended to be used as the TLSClientConfig.RootCAs for the AGC's shared
// http.Transport.
//
// Behavior:
//   - certPEM == nil (or empty) returns (nil, nil). Callers treat this as
//     "no proxy CA mounted; use the default transport unchanged" — the
//     local-dev / no-TLS-proxy case.
//   - certPEM contains at least one valid PEM-encoded certificate: the
//     returned pool contains the system roots plus the supplied cert(s).
//   - certPEM is non-empty but contains no parseable certificates: returns
//     an error.
//   - The system cert pool cannot be loaded: returns an error.
//
// The combined pool preserves the security property that only the supplied
// proxy CA can validate certificates for the proxy's *.svc.cluster.local
// hostname (no public CA will issue for that suffix), while still letting
// the same TLSClientConfig validate api.github.com and the actions
// pipelines endpoints over the CONNECT tunnel.
func BuildProxyTrustPool(certPEM []byte) (*x509.CertPool, error) {
	if len(certPEM) == 0 {
		return nil, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system CA pool: %w", err)
	}
	if !pool.AppendCertsFromPEM(certPEM) {
		return nil, fmt.Errorf("proxy CA PEM contained no valid certificates")
	}
	return pool, nil
}
