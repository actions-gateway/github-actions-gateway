package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// testCA is a throwaway certificate authority for the mTLS tests.
type testCA struct {
	cert *x509.Certificate
	key  *rsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "test-metrics-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// leaf signs a leaf certificate with the CA and returns it as a tls.Certificate.
// When server is true the cert gets a 127.0.0.1 IP SAN + ServerAuth; otherwise
// it gets ClientAuth.
func (ca *testCA) leaf(t *testing.T, cn string, server bool) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		BasicConstraintsValid: true,
	}
	if server {
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// writeServerBundle writes ca.crt/tls.crt/tls.key for a CA-signed server cert and
// returns (certFile, keyFile, caFile).
func writeServerBundle(t *testing.T, ca *testCA, server tls.Certificate) (certFile, keyFile, caFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	caFile = filepath.Join(dir, "ca.crt")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate[0]})
	require.NoError(t, os.WriteFile(certFile, certPEM, 0o600))

	keyDER, err := x509.MarshalPKCS8PrivateKey(server.PrivateKey)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	require.NoError(t, os.WriteFile(keyFile, keyPEM, 0o600))

	require.NoError(t, os.WriteFile(caFile, ca.pem, 0o600))
	return certFile, keyFile, caFile
}

// startMetricsProxy starts a Server with an mTLS metrics listener configured from
// the given CA, returning the running server. The caller's ctx cancel stops it.
func startMetricsProxy(t *testing.T, ca *testCA) *Server {
	t.Helper()
	server := ca.leaf(t, "127.0.0.1", true)
	certFile, keyFile, caFile := writeServerBundle(t, ca, server)

	reg := prometheus.NewRegistry()
	srv := NewServer(freeAddr(t), freeAddr(t), 5*time.Second, nil, reg)
	srv.MetricsAddr = freeAddr(t)
	srv.MetricsTLSCertFile = certFile
	srv.MetricsTLSKeyFile = keyFile
	srv.MetricsClientCAFile = caFile

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", srv.MetricsAddr, 50*time.Millisecond)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}, 2*time.Second, 10*time.Millisecond)
	return srv
}

// httpsClient builds an HTTPS client that trusts ca and optionally presents a
// client cert.
func httpsClient(ca *testCA, clientCert *tls.Certificate) *http.Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.pem)
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if clientCert != nil {
		cfg.Certificates = []tls.Certificate{*clientCert}
	}
	return &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
}

// TestProxy_MetricsMTLS_AcceptsValidClientCert verifies a scraper presenting a
// CA-signed client cert can read /metrics over the mTLS listener.
func TestProxy_MetricsMTLS_AcceptsValidClientCert(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	ca := newTestCA(t)
	srv := startMetricsProxy(t, ca)
	// Force a sample so the *Vec metric emits a series (no traffic has occurred).
	srv.connectionsTotal.WithLabelValues().Inc()

	clientCert := ca.leaf(t, "scraper", false)
	resp, err := httpsClient(ca, &clientCert).Get("https://" + srv.MetricsAddr + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "actions_gateway_proxy_",
		"metrics body should contain the proxy's Prometheus metrics")
}

// TestProxy_MetricsMTLS_RejectsNoClientCert verifies the TLS handshake itself
// rejects a scraper that presents no client cert.
func TestProxy_MetricsMTLS_RejectsNoClientCert(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	ca := newTestCA(t)
	srv := startMetricsProxy(t, ca)

	_, err := httpsClient(ca, nil).Get("https://" + srv.MetricsAddr + "/metrics")
	require.Error(t, err, "request without a client cert must fail the mTLS handshake")
}

// TestProxy_MetricsMTLS_RejectsWrongCA verifies a client cert signed by a
// different CA is rejected.
func TestProxy_MetricsMTLS_RejectsWrongCA(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	ca := newTestCA(t)
	srv := startMetricsProxy(t, ca)

	foreign := newTestCA(t)
	foreignCert := foreign.leaf(t, "scraper", false)
	// Trust the real server CA but present a foreign client cert.
	_, err := httpsClient(ca, &foreignCert).Get("https://" + srv.MetricsAddr + "/metrics")
	require.Error(t, err, "a client cert signed by an untrusted CA must be rejected")
}

// TestProxy_MetricsMTLS_NotOnHealthPort verifies that when the mTLS metrics
// listener is enabled, /metrics is no longer served plaintext on the health
// port (only /healthz and /readyz remain there).
func TestProxy_MetricsMTLS_NotOnHealthPort(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	ca := newTestCA(t)
	srv := startMetricsProxy(t, ca)

	resp, err := http.Get("http://" + srv.HealthAddr + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"/metrics must not be served plaintext on the health port when mTLS is enabled")

	healthResp, err := http.Get("http://" + srv.HealthAddr + "/healthz")
	require.NoError(t, err)
	defer healthResp.Body.Close()
	assert.Equal(t, http.StatusOK, healthResp.StatusCode, "/healthz must still answer plaintext for kubelet probes")
}
