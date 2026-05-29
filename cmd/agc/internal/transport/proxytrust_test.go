package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

// generateCA returns a self-signed CA cert and its private key.
func generateCA(t *testing.T, commonName string) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return cert, key, pemBytes
}

// generateLeaf returns a leaf cert signed by the given CA.
func generateLeaf(t *testing.T, dnsName string, ca *x509.Certificate, caKey *ecdsa.PrivateKey) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{dnsName},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf cert: %v", err)
	}
	return leaf
}

func verify(t *testing.T, leaf *x509.Certificate, pool *x509.CertPool, dnsName string) error {
	t.Helper()
	_, err := leaf.Verify(x509.VerifyOptions{
		Roots:       pool,
		DNSName:     dnsName,
		CurrentTime: time.Now(),
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return err
}

// TestBuildProxyTrustPool_NilPEM verifies that an empty/missing PEM returns
// (nil, nil) so callers fall through to the default transport. This is the
// "local dev, no TLS proxy" case.
func TestBuildProxyTrustPool_NilPEM(t *testing.T) {
	pool, err := BuildProxyTrustPool(nil)
	if err != nil {
		t.Fatalf("nil PEM: unexpected error: %v", err)
	}
	if pool != nil {
		t.Fatalf("nil PEM: want nil pool, got %v", pool)
	}

	pool, err = BuildProxyTrustPool([]byte{})
	if err != nil {
		t.Fatalf("empty PEM: unexpected error: %v", err)
	}
	if pool != nil {
		t.Fatalf("empty PEM: want nil pool, got %v", pool)
	}
}

// TestBuildProxyTrustPool_InvalidPEM verifies that non-empty but
// unparseable input returns an error rather than silently producing a
// pool that contains only the system roots — which would let an attacker
// with any system-trusted cert impersonate the per-tenant proxy.
func TestBuildProxyTrustPool_InvalidPEM(t *testing.T) {
	pool, err := BuildProxyTrustPool([]byte("not a certificate"))
	if err == nil {
		t.Fatalf("invalid PEM: want error, got pool=%v", pool)
	}
	if pool != nil {
		t.Fatalf("invalid PEM: want nil pool on error, got %v", pool)
	}
}

// TestBuildProxyTrustPool_ValidatesProxyLeaf verifies the core regression
// guard for PR #59's `fix(agc): append proxy CA to system pool instead of
// replacing it`: a leaf signed by the supplied proxy CA validates against
// the returned pool.
func TestBuildProxyTrustPool_ValidatesProxyLeaf(t *testing.T) {
	proxyCA, proxyKey, proxyPEM := generateCA(t, "proxy-ca")
	leaf := generateLeaf(t, "proxy.tenant.svc.cluster.local", proxyCA, proxyKey)

	pool, err := BuildProxyTrustPool(proxyPEM)
	if err != nil {
		t.Fatalf("BuildProxyTrustPool: %v", err)
	}
	if pool == nil {
		t.Fatalf("BuildProxyTrustPool: want non-nil pool")
	}

	if err := verify(t, leaf, pool, "proxy.tenant.svc.cluster.local"); err != nil {
		t.Fatalf("proxy leaf should verify against the combined pool: %v", err)
	}
}

// TestBuildProxyTrustPool_RejectsUnrelatedCA verifies that a leaf signed
// by a CA that is neither in the system store nor the supplied proxy CA
// is rejected. Confirms BuildProxyTrustPool does not over-trust.
func TestBuildProxyTrustPool_RejectsUnrelatedCA(t *testing.T) {
	_, _, proxyPEM := generateCA(t, "proxy-ca")
	attackerCA, attackerKey, _ := generateCA(t, "attacker-ca")
	leaf := generateLeaf(t, "proxy.tenant.svc.cluster.local", attackerCA, attackerKey)

	pool, err := BuildProxyTrustPool(proxyPEM)
	if err != nil {
		t.Fatalf("BuildProxyTrustPool: %v", err)
	}

	if err := verify(t, leaf, pool, "proxy.tenant.svc.cluster.local"); err == nil {
		t.Fatalf("attacker-signed leaf should not verify against the combined pool")
	}
}

// TestBuildProxyTrustPool_PreservesSystemRoots verifies that the returned
// pool still trusts certs chaining to the system root store — i.e. the
// supplied proxy CA is *appended* rather than *replacing* the system
// roots. This is the second half of PR #59's `fix(agc): append proxy CA
// to system pool instead of replacing it`: if the system roots were lost,
// AGC would be unable to validate api.github.com over the proxy CONNECT
// tunnel.
//
// We exercise this by capturing the system pool directly and confirming
// the combined pool's subject set is a strict superset (system subjects
// plus the proxy CA's subject).
func TestBuildProxyTrustPool_PreservesSystemRoots(t *testing.T) {
	sys, err := x509.SystemCertPool()
	if err != nil {
		t.Skipf("system cert pool unavailable on this platform: %v", err)
	}
	sysSubjects := len(sys.Subjects())
	if sysSubjects == 0 {
		t.Skip("system cert pool is empty; cannot verify superset property")
	}

	_, _, proxyPEM := generateCA(t, "proxy-ca")
	pool, err := BuildProxyTrustPool(proxyPEM)
	if err != nil {
		t.Fatalf("BuildProxyTrustPool: %v", err)
	}

	combined := len(pool.Subjects())
	if combined != sysSubjects+1 {
		t.Fatalf("combined pool: want %d subjects (system %d + 1 proxy), got %d",
			sysSubjects+1, sysSubjects, combined)
	}
}
