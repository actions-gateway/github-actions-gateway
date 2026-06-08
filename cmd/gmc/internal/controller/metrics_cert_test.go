package controller

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateMetricsCerts_Valid(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	b, err := generateMetricsCerts(ag)
	require.NoError(t, err)
	require.NotNil(t, b)

	for name, pemBytes := range map[string][]byte{
		"ca": b.caPEM, "serverCert": b.serverCertPEM, "serverKey": b.serverKeyPEM,
		"clientCert": b.clientCertPEM, "clientKey": b.clientKeyPEM,
	} {
		assert.NotEmpty(t, pemBytes, "%s PEM must be non-empty", name)
	}

	caCert, err := parseCertPEM(b.caPEM)
	require.NoError(t, err)
	assert.True(t, caCert.IsCA, "CA cert must have IsCA set")

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(b.caPEM))

	// Server cert chains to the CA and is valid for server auth.
	serverCert, err := parseCertPEM(b.serverCertPEM)
	require.NoError(t, err)
	_, err = serverCert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	assert.NoError(t, err, "server cert must verify for ServerAuth against the CA")
	assert.Contains(t, serverCert.DNSNames, fmt.Sprintf("%s.%s.svc", proxyServiceName, ag.Namespace),
		"server cert must list the proxy Service FQDN as a SAN")
	assert.Contains(t, serverCert.DNSNames, fmt.Sprintf("%s.%s.svc", agcAppName, ag.Namespace),
		"server cert must list the AGC Service FQDN as a SAN")

	// Client cert chains to the CA and is valid for client auth.
	clientCert, err := parseCertPEM(b.clientCertPEM)
	require.NoError(t, err)
	_, err = clientCert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	assert.NoError(t, err, "client cert must verify for ClientAuth against the CA")
	assert.Equal(t, metricsScraperCN, clientCert.Subject.CommonName)

	// The server cert+key form a usable TLS keypair.
	_, err = tls.X509KeyPair(b.serverCertPEM, b.serverKeyPEM)
	assert.NoError(t, err)
}

// TestGenerateMetricsCerts_MTLSHandshake proves the bundle actually drives a
// working mTLS exchange: an httptest server configured with the server cert and
// RequireAndVerifyClientCert accepts the scraper client cert and rejects a
// request without one.
func TestGenerateMetricsCerts_MTLSHandshake(t *testing.T) {
	ag := newTestAG("gateway", "team-a")
	b, err := generateMetricsCerts(ag)
	require.NoError(t, err)

	serverKP, err := tls.X509KeyPair(b.serverCertPEM, b.serverKeyPEM)
	require.NoError(t, err)
	caPool := x509.NewCertPool()
	require.True(t, caPool.AppendCertsFromPEM(b.caPEM))

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverKP},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	defer srv.Close()

	sni := fmt.Sprintf("%s.%s.svc", proxyServiceName, ag.Namespace)

	// With the scraper client cert → success.
	clientKP, err := tls.X509KeyPair(b.clientCertPEM, b.clientKeyPEM)
	require.NoError(t, err)
	withCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:      caPool,
		Certificates: []tls.Certificate{clientKP},
		ServerName:   sni,
	}}}
	resp, err := withCert.Get(srv.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Without a client cert → handshake rejected.
	noCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    caPool,
		ServerName: sni,
	}}}
	_, err = noCert.Get(srv.URL)
	assert.Error(t, err, "server must reject a request that presents no client cert")
}
