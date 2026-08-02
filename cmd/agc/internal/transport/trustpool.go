// Package transport contains shared HTTP transport configuration helpers
// for the AGC, including the trust pool used to validate the per-tenant egress
// proxy's self-signed TLS certificate — and, on GHES, the private CA fronting the
// appliance — without losing the ability to validate upstream GitHub endpoints
// over the proxy's CONNECT tunnel.
package transport

import (
	"crypto/x509"
	"fmt"
)

// BuildTrustPool returns an x509 cert pool seeded from the system root store and
// extended with every supplied PEM. The resulting pool is intended to be used as
// the TLSClientConfig.RootCAs for the AGC's shared http.Transport.
//
// The sources are the per-tenant egress proxy's self-signed CA and, when the
// gateway sets spec.githubCABundleRef, the CA fronting its GHES appliance (Q536).
//
// Behavior:
//   - every PEM empty (or none supplied) returns (nil, nil). Callers treat this as
//     "nothing to add; use the default transport unchanged" — the local-dev /
//     no-TLS-proxy / public-GitHub case.
//   - each non-empty PEM contributes its certificate(s) to a pool that also holds
//     the system roots.
//   - a non-empty PEM containing no parseable certificates: returns an error.
//   - The system cert pool cannot be loaded: returns an error.
//
// Extending never replaces. That preserves the security property that only the
// supplied proxy CA can validate certificates for the proxy's *.svc.cluster.local
// hostname (no public CA will issue for that suffix), while still letting the same
// TLSClientConfig validate api.github.com and the actions pipelines endpoints over
// the CONNECT tunnel.
func BuildTrustPool(pems ...[]byte) (*x509.CertPool, error) {
	var pool *x509.CertPool
	for _, certPEM := range pems {
		if len(certPEM) == 0 {
			continue
		}
		if pool == nil {
			var err error
			if pool, err = x509.SystemCertPool(); err != nil {
				return nil, fmt.Errorf("load system CA pool: %w", err)
			}
		}
		if !pool.AppendCertsFromPEM(certPEM) {
			return nil, fmt.Errorf("CA PEM contained no valid certificates")
		}
	}
	return pool, nil
}
