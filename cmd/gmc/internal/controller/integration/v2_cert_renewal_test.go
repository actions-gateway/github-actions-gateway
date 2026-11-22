//go:build integration

package integration_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// certRenewBefore is the reconcilers' 30-day renewal window (proxyCertRenewBefore,
// metricsCertRenewBefore). A cert inside it must be reissued; one past it, left alone.
const certRenewBefore = 30 * 24 * time.Hour

// leafCertPEM returns a self-signed basic-constraints leaf for dnsNames expiring at
// notAfter, PEM-encoded with its PKCS#8 key: the shape the proxy cert parser accepts,
// so the renewal decision turns on NotAfter alone.
func leafCertPEM(t *testing.T, dnsNames []string, notAfter time.Time) (certPEM, keyPEM []byte, serial *big.Int) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	serial, err = rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: dnsNames[0]},
		DNSNames:              dnsNames,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), serial
}

// secretCert parses the certificate under tls.crt of the named Secret.
func secretCert(ns, name string) (*x509.Certificate, string, error) {
	var sec corev1.Secret
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sec); err != nil {
		return nil, "", err
	}
	block, _ := pem.Decode(sec.Data[corev1.TLSCertKey])
	if block == nil {
		return nil, sec.ResourceVersion, fmt.Errorf("no PEM block in %s/%s", ns, name)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	return cert, sec.ResourceVersion, err
}

// overwriteCert replaces the named Secret's tls.crt and tls.key in place.
func overwriteCert(t *testing.T, ns, name string, certPEM, keyPEM []byte) {
	t.Helper()
	require.Eventually(t, func() bool {
		var sec corev1.Secret
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sec); err != nil {
			return false
		}
		sec.Data[corev1.TLSCertKey] = certPEM
		sec.Data[corev1.TLSPrivateKeyKey] = keyPEM
		return k8sClient.Update(ctx, &sec) == nil
	}, 5*time.Second, 50*time.Millisecond, "overwrite %s/%s", ns, name)
}

// requireReissued waits until the named Secret holds a cert other than serial that is
// valid past the renewal window.
func requireReissued(t *testing.T, ns, name string, serial *big.Int) {
	t.Helper()
	require.Eventually(t, func() bool {
		cert, _, err := secretCert(ns, name)
		return err == nil && cert.SerialNumber.Cmp(serial) != 0 && time.Until(cert.NotAfter) > certRenewBefore
	}, 20*time.Second, 100*time.Millisecond, "%s/%s should be reissued past the renewal window", ns, name)
}

// TestV2_EgressProxy_ProxyCertRenewal pins both arms of ensureProxyCert against the
// real apiserver: a cert inside the renewal window, or already expired, is reissued,
// and a cert outside it survives steady-state reconciles unwritten. The second arm
// is the one an always-reissue regression breaks, and it rolls every consumer's
// trust root when it does.
func TestV2_EgressProxy_ProxyCertRenewal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ns       string
		notAfter time.Duration
		reissue  bool
	}{
		{"near-expiry", "v2-ep-cert-near", 10 * 24 * time.Hour, true},
		{"expired", "v2-ep-cert-expired", -time.Hour, true},
		{"valid", "v2-ep-cert-valid", 200 * 24 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			createNamespace(t, tc.ns)
			ep := newV2EgressProxyObject(egressProxyName, tc.ns)
			require.NoError(t, k8sClient.Create(ctx, ep))
			t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })
			const syncPeriod = 2 * time.Second
			_, epCount := startCountingV2Reconcilers(t, nil, syncPeriod)

			secret := proxyTLSName(egressProxyName)
			svc := proxyChildName(egressProxyName)
			require.Eventually(t, func() bool { _, _, err := secretCert(tc.ns, secret); return err == nil },
				15*time.Second, 100*time.Millisecond, "the proxy TLS Secret should be issued")

			dns := []string{svc, svc + "." + tc.ns, svc + "." + tc.ns + ".svc", svc + "." + tc.ns + ".svc.cluster.local"}
			certPEM, keyPEM, serial := leafCertPEM(t, dns, time.Now().Add(tc.notAfter))
			overwriteCert(t, tc.ns, secret, certPEM, keyPEM)

			if tc.reissue {
				requireReissued(t, tc.ns, secret, serial)
				return
			}
			_, rv, err := secretCert(tc.ns, secret)
			require.NoError(t, err)
			epBefore := epCount.startedIn(tc.ns)
			time.Sleep(3 * syncPeriod)
			cert, rvAfter, err := secretCert(tc.ns, secret)
			require.NoError(t, err)
			assert.Equal(t, 0, cert.SerialNumber.Cmp(serial), "a valid proxy cert must be kept")
			assert.Equal(t, rv, rvAfter, "a valid proxy cert's Secret must not be rewritten")
			assert.GreaterOrEqual(t, epCount.startedIn(tc.ns)-epBefore, 2, "EgressProxy reconciles inside the window")
		})
	}
}

// TestV2_ActionsGateway_MetricsCertRenewal is the same pair for the gateway's AGC
// metrics mTLS bundle (ensureMetricsCerts): the server cert near expiry regenerates
// the bundle, and a valid one is left alone.
func TestV2_ActionsGateway_MetricsCertRenewal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ns       string
		notAfter time.Duration
		reissue  bool
	}{
		{"near-expiry", "v2-ag-mcert-near", 10 * 24 * time.Hour, true},
		{"valid", "v2-ag-mcert-valid", 200 * 24 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			createNamespace(t, tc.ns)
			createGitHubAppSecret(t, tc.ns, "github-app")
			createDirectV2Gateway(t, tc.ns, "github-app", nil)
			const syncPeriod = 2 * time.Second
			agCount, _ := startCountingV2Reconcilers(t, nil, syncPeriod)

			const server, client = "gw-agc-metrics-tls", "gw-agc-metrics-client"
			require.Eventually(t, func() bool {
				_, _, serr := secretCert(tc.ns, server)
				_, _, cerr := secretCert(tc.ns, client)
				return serr == nil && cerr == nil
			}, 15*time.Second, 100*time.Millisecond, "the metrics bundle should be issued")

			certPEM, keyPEM, serial := leafCertPEM(t, []string{"gw-agc." + tc.ns + ".svc"}, time.Now().Add(tc.notAfter))
			overwriteCert(t, tc.ns, server, certPEM, keyPEM)

			if tc.reissue {
				requireReissued(t, tc.ns, server, serial)
				return
			}
			_, rv, err := secretCert(tc.ns, server)
			require.NoError(t, err)
			agBefore := agCount.startedIn(tc.ns)
			time.Sleep(3 * syncPeriod)
			cert, rvAfter, err := secretCert(tc.ns, server)
			require.NoError(t, err)
			assert.Equal(t, 0, cert.SerialNumber.Cmp(serial), "a valid metrics cert must be kept")
			assert.Equal(t, rv, rvAfter, "a valid metrics cert's Secret must not be rewritten")
			assert.GreaterOrEqual(t, agCount.startedIn(tc.ns)-agBefore, 2, "ActionsGateway reconciles inside the window")
		})
	}
}
