package controller

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestGenerateEgressProxyCert(t *testing.T) {
	certPEM, keyPEM, err := generateEgressProxyCert("team-a", "shared-proxy")
	require.NoError(t, err)
	require.NotEmpty(t, certPEM)
	require.NotEmpty(t, keyPEM)

	cert, err := parseCertPEM(certPEM)
	require.NoError(t, err)

	// SANs cover every in-cluster DNS name for the derived <ep>-proxy Service so a
	// consumer can pin to this cert without a CA hierarchy.
	assert.Contains(t, cert.DNSNames, "shared-proxy")
	assert.Contains(t, cert.DNSNames, "shared-proxy.team-a")
	assert.Contains(t, cert.DNSNames, "shared-proxy.team-a.svc")
	assert.Contains(t, cert.DNSNames, "shared-proxy.team-a.svc.cluster.local")

	// Server-auth EKU and a forward-dated expiry well beyond the renew window.
	assert.Contains(t, cert.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	assert.True(t, cert.NotAfter.After(time.Now().Add(300*24*time.Hour)),
		"freshly issued cert should not be near expiry")
}

// selfSignedForTest mints a self-signed certificate from tmpl and returns its PEM
// and the PEM of its key.
func selfSignedForTest(t *testing.T, tmpl *x509.Certificate) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl.SerialNumber = big.NewInt(1)
	tmpl.NotBefore = time.Now().Add(-time.Minute)
	tmpl.NotAfter = time.Now().Add(365 * 24 * time.Hour)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return encodeCertPEM(der), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

// Consumers install the proxy cert as a trust root, so only the exact shape the GMC
// issues may pass: anything that could sign, or vouch for another host, must not.
func TestParseIssuedEgressProxyCert_AcceptsOnlyTheIssuedShape(t *testing.T) {
	const ns, svc = "team-a", "pool-proxy"
	certPEM, keyPEM, err := generateEgressProxyCert(ns, svc)
	require.NoError(t, err)
	_, otherKeyPEM, err := generateEgressProxyCert(ns, svc)
	require.NoError(t, err)
	caPEM, _ := selfSignedForTest(t, &x509.Certificate{
		Subject: pkix.Name{CommonName: "planted"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign,
	})
	names := egressProxyCertDNSNames(ns, svc)
	caLeafPEM, caLeafKeyPEM := selfSignedForTest(t, &x509.Certificate{
		DNSNames: names, IsCA: true, BasicConstraintsValid: true,
	})
	wideLeafPEM, wideLeafKeyPEM := selfSignedForTest(t, &x509.Certificate{
		DNSNames: append(slices.Clone(names), "api.github.com"), BasicConstraintsValid: true,
	})
	noBCPEM, noBCKeyPEM := selfSignedForTest(t, &x509.Certificate{DNSNames: names})

	tests := []struct {
		name      string
		cert, key []byte
		wantErr   bool
	}{
		{"GMC-issued leaf", certPEM, keyPEM, false},
		{"leaf with an appended CA", append(slices.Clone(certPEM), caPEM...), keyPEM, true},
		{"CA with the Service names", caLeafPEM, caLeafKeyPEM, true},
		{"leaf naming a GitHub host", wideLeafPEM, wideLeafKeyPEM, true},
		{"no basic constraints", noBCPEM, noBCKeyPEM, true},
		{"cert not paired with tls.key", certPEM, otherKeyPEM, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseIssuedEgressProxyCert(ns, svc, map[string][]byte{
				corev1.TLSCertKey: tc.cert, corev1.TLSPrivateKeyKey: tc.key,
			})
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
