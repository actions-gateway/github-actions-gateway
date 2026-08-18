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

// TestBuildTrustPool_NilPEM verifies that an empty/missing PEM returns
// (nil, nil) so callers fall through to the default transport. This is the
// "local dev, no TLS proxy" case.
func TestBuildTrustPool_NilPEM(t *testing.T) {
	pool, err := BuildTrustPool(nil)
	if err != nil {
		t.Fatalf("nil PEM: unexpected error: %v", err)
	}
	if pool != nil {
		t.Fatalf("nil PEM: want nil pool, got %v", pool)
	}

	pool, err = BuildTrustPool([]byte{})
	if err != nil {
		t.Fatalf("empty PEM: unexpected error: %v", err)
	}
	if pool != nil {
		t.Fatalf("empty PEM: want nil pool, got %v", pool)
	}
}

// TestBuildTrustPool_InvalidPEM verifies that non-empty but
// unparseable input returns an error rather than silently producing a
// pool that contains only the system roots — which would let an attacker
// with any system-trusted cert impersonate the per-tenant proxy.
func TestBuildTrustPool_InvalidPEM(t *testing.T) {
	pool, err := BuildTrustPool([]byte("not a certificate"))
	if err == nil {
		t.Fatalf("invalid PEM: want error, got pool=%v", pool)
	}
	if pool != nil {
		t.Fatalf("invalid PEM: want nil pool on error, got %v", pool)
	}
}

// TestBuildTrustPool_ValidatesProxyLeaf verifies the core regression
// guard for PR #59's `fix(agc): append proxy CA to system pool instead of
// replacing it`: a leaf signed by the supplied proxy CA validates against
// the returned pool.
func TestBuildTrustPool_ValidatesProxyLeaf(t *testing.T) {
	proxyCA, proxyKey, proxyPEM := generateCA(t, "proxy-ca")
	leaf := generateLeaf(t, "proxy.tenant.svc.cluster.local", proxyCA, proxyKey)

	pool, err := BuildTrustPool(proxyPEM)
	if err != nil {
		t.Fatalf("BuildTrustPool: %v", err)
	}
	if pool == nil {
		t.Fatalf("BuildTrustPool: want non-nil pool")
	}

	if err := verify(t, leaf, pool, "proxy.tenant.svc.cluster.local"); err != nil {
		t.Fatalf("proxy leaf should verify against the combined pool: %v", err)
	}
}

// TestBuildTrustPool_RejectsUnrelatedCA verifies that a leaf signed
// by a CA that is neither in the system store nor the supplied proxy CA
// is rejected. Confirms BuildTrustPool does not over-trust.
func TestBuildTrustPool_RejectsUnrelatedCA(t *testing.T) {
	_, _, proxyPEM := generateCA(t, "proxy-ca")
	attackerCA, attackerKey, _ := generateCA(t, "attacker-ca")
	leaf := generateLeaf(t, "proxy.tenant.svc.cluster.local", attackerCA, attackerKey)

	pool, err := BuildTrustPool(proxyPEM)
	if err != nil {
		t.Fatalf("BuildTrustPool: %v", err)
	}

	if err := verify(t, leaf, pool, "proxy.tenant.svc.cluster.local"); err == nil {
		t.Fatalf("attacker-signed leaf should not verify against the combined pool")
	}
}

// TestBuildTrustPool_PreservesSystemRoots verifies that the returned
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
func TestBuildTrustPool_PreservesSystemRoots(t *testing.T) {
	sys, err := x509.SystemCertPool()
	if err != nil {
		t.Skipf("system cert pool unavailable on this platform: %v", err)
	}
	sysSubjects := len(sys.Subjects())
	if sysSubjects == 0 {
		t.Skip("system cert pool is empty; cannot verify superset property")
	}

	_, _, proxyPEM := generateCA(t, "proxy-ca")
	pool, err := BuildTrustPool(proxyPEM)
	if err != nil {
		t.Fatalf("BuildTrustPool: %v", err)
	}

	combined := len(pool.Subjects())
	if combined != sysSubjects+1 {
		t.Fatalf("combined pool: want %d subjects (system %d + 1 proxy), got %d",
			sysSubjects+1, sysSubjects, combined)
	}
}

// TestBuildTrustPool_ValidatesGHESLeaf is the Q536 guard: a leaf signed by the
// operator-supplied GitHub CA — a GHES appliance's certificate — validates against
// the pool, and does so alongside the proxy CA rather than in place of it. Before
// the second source existed, the same leaf was the one
// TestBuildTrustPool_RejectsUnrelatedCA required to fail.
func TestBuildTrustPool_ValidatesGHESLeaf(t *testing.T) {
	proxyCA, proxyKey, proxyPEM := generateCA(t, "proxy-ca")
	ghesCA, ghesKey, ghesPEM := generateCA(t, "corp-root-ca")
	proxyLeaf := generateLeaf(t, "proxy.tenant.svc.cluster.local", proxyCA, proxyKey)
	ghesLeaf := generateLeaf(t, "ghes.corp.example", ghesCA, ghesKey)

	pool, err := BuildTrustPool(proxyPEM, ghesPEM)
	if err != nil {
		t.Fatalf("BuildTrustPool: %v", err)
	}

	if err := verify(t, ghesLeaf, pool, "ghes.corp.example"); err != nil {
		t.Fatalf("GHES leaf should verify against the combined pool: %v", err)
	}
	if err := verify(t, proxyLeaf, pool, "proxy.tenant.svc.cluster.local"); err != nil {
		t.Fatalf("proxy leaf should still verify with a GitHub CA supplied too: %v", err)
	}
}

// TestBuildTrustPool_GHESOnly covers the direct-egress GHES gateway: no proxy CA is
// mounted, so the GitHub CA is the only supplied source and must still be appended
// to the system roots rather than replacing them.
func TestBuildTrustPool_GHESOnly(t *testing.T) {
	ghesCA, ghesKey, ghesPEM := generateCA(t, "corp-root-ca")
	leaf := generateLeaf(t, "ghes.corp.example", ghesCA, ghesKey)

	pool, err := BuildTrustPool(nil, ghesPEM)
	if err != nil {
		t.Fatalf("BuildTrustPool: %v", err)
	}
	if pool == nil {
		t.Fatalf("BuildTrustPool: want non-nil pool from a GitHub-CA-only call")
	}
	if err := verify(t, leaf, pool, "ghes.corp.example"); err != nil {
		t.Fatalf("GHES leaf should verify with no proxy CA supplied: %v", err)
	}
}

// TestBuildTrustPool_RejectsInvalidGitHubPEM verifies the opt-in fails loudly: a
// githubCABundleRef whose ConfigMap holds garbage must error rather than yield a
// system-roots-only pool the AGC would then run with, silently untrusting.
func TestBuildTrustPool_RejectsInvalidGitHubPEM(t *testing.T) {
	_, _, proxyPEM := generateCA(t, "proxy-ca")
	pool, err := BuildTrustPool(proxyPEM, []byte("-----BEGIN CERTIFICATE-----\nnope\n"))
	if err == nil {
		t.Fatalf("invalid GitHub CA PEM: want error, got pool=%v", pool)
	}
}
