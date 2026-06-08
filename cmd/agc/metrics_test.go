package main

import (
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
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildMetricsOptions_AbsentDirFallsBackToPlain(t *testing.T) {
	opts, err := buildMetricsOptions(filepath.Join(t.TempDir(), "does-not-exist"), logr.Discard())
	require.NoError(t, err)
	assert.False(t, opts.SecureServing, "metrics must fall back to plain HTTP when no bundle is mounted")
	assert.Equal(t, metricsBindAddress, opts.BindAddress)
	assert.Empty(t, opts.TLSOpts)
}

func TestBuildMetricsOptions_WithBundleEnablesMTLS(t *testing.T) {
	caPEM, serverKP, _ := genCABundle(t)
	dir := t.TempDir()
	writeKeyPair(t, dir, serverKP, caPEM)

	opts, err := buildMetricsOptions(dir, logr.Discard())
	require.NoError(t, err)
	assert.True(t, opts.SecureServing, "metrics must be served securely when the bundle is present")
	assert.Equal(t, dir, opts.CertDir)
	assert.Equal(t, "tls.crt", opts.CertName)
	assert.Equal(t, "tls.key", opts.KeyName)
	require.Len(t, opts.TLSOpts, 1, "expected one TLS mutator enforcing mTLS")

	cfg := &tls.Config{}
	opts.TLSOpts[0](cfg)
	assert.Equal(t, tls.RequireAndVerifyClientCert, cfg.ClientAuth)
	assert.NotNil(t, cfg.ClientCAs, "client CA pool must be set")
	assert.GreaterOrEqual(t, cfg.MinVersion, uint16(tls.VersionTLS12))
}

// TestMetricsClientCAVerifier_Handshake proves the mutator's tls.Config actually
// requires a CA-signed client cert end-to-end.
func TestMetricsClientCAVerifier_Handshake(t *testing.T) {
	caPEM, serverKP, clientKP := genCABundle(t)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(caPEM))

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverKP}}
	metricsClientCAVerifier(pool)(srv.TLS)
	srv.StartTLS()
	defer srv.Close()

	withCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{clientKP},
		ServerName:   "127.0.0.1",
	}}}
	resp, err := withCert.Get(srv.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	noCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    pool,
		ServerName: "127.0.0.1",
	}}}
	_, err = noCert.Get(srv.URL)
	assert.Error(t, err, "handshake must fail without a client cert")
}

// genCABundle returns a CA PEM plus a server and client tls.Certificate signed by it.
func genCABundle(t *testing.T) (caPEM []byte, server, client tls.Certificate) {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	leaf := func(cn string, isServer bool) tls.Certificate {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		require.NoError(t, err)
		tmpl := &x509.Certificate{
			SerialNumber: serial,
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    time.Now().Add(-time.Minute),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		}
		if isServer {
			tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		} else {
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		require.NoError(t, err)
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	}
	return caPEM, leaf("127.0.0.1", true), leaf("scraper", false)
}

// writeKeyPair writes tls.crt/tls.key/ca.crt for the given server cert into dir.
func writeKeyPair(t *testing.T, dir string, server tls.Certificate, caPEM []byte) {
	t.Helper()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate[0]})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.crt"), certPEM, 0o600))
	keyDER, err := x509.MarshalPKCS8PrivateKey(server.PrivateKey)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.key"), keyPEM, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ca.crt"), caPEM, 0o600))
}
