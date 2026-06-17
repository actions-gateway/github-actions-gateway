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

	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Cert Secret names the GMC manages. Kept as literals (mirroring the unexported
// controller constants) so a rename there is caught by these tests failing.
const (
	proxyTLSSecret      = "actions-gateway-proxy-tls"
	metricsTLSSecret    = "actions-gateway-metrics-tls"
	metricsClientSecret = "actions-gateway-metrics-client"

	// certRenewBefore mirrors the controller's proxyCertRenewBefore /
	// metricsCertRenewBefore (both 30 days). Used only to assert a *regenerated*
	// cert lands well beyond the window; the controller owns the real threshold.
	certRenewBefore = 30 * 24 * time.Hour
)

// makeSelfSignedCertPEM generates a throwaway self-signed RSA cert+key with the
// given NotAfter and returns the PEM-encoded cert, PEM-encoded PKCS#8 key, and the
// serial number. NotBefore is backdated an hour so the cert is currently valid
// (unless NotAfter itself is in the past). No real credential is read — this is
// generated in-test, satisfying the renewal decision's only input: tls.crt's
// NotAfter. The cert and key are a matching pair so the apiserver accepts the
// kubernetes.io/tls Secret.
func makeSelfSignedCertPEM(t *testing.T, notAfter time.Time) (certPEM, keyPEM []byte, serial *big.Int) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	serial, err = rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "cert-renewal-test"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, serial
}

// seedTLSSecret pre-creates a kubernetes.io/tls Secret with the given cert/key
// (plus any extra data keys) BEFORE the reconciler starts, so the reconciler's
// first pass sees a pre-existing cert and exercises the renewal decision rather
// than the first-generation path.
func seedTLSSecret(t *testing.T, ns, name string, certPEM, keyPEM []byte, extra map[string][]byte) {
	t.Helper()
	data := map[string][]byte{
		corev1.TLSCertKey:       certPEM,
		corev1.TLSPrivateKeyKey: keyPEM,
	}
	for k, v := range extra {
		data[k] = v
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Type:       corev1.SecretTypeTLS,
		Data:       data,
	}
	require.NoError(t, k8sClient.Create(ctx, secret))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, secret) })
}

// certSerialFromSecret reads the named Secret and returns the serial number of
// the cert under tls.crt.
func certSerialFromSecret(t *testing.T, ns, name string) (*big.Int, error) {
	t.Helper()
	var sec corev1.Secret
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sec); err != nil {
		return nil, err
	}
	block, _ := pem.Decode(sec.Data[corev1.TLSCertKey])
	if block == nil {
		return nil, fmt.Errorf("no PEM block in %s/%s tls.crt", ns, name)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	return cert.SerialNumber, nil
}

// waitForProvisioned blocks until the reconciler has provisioned the proxy
// Deployment, proving a full reconcile ran. Used by the "left unchanged" tests
// so a passing assertion can't be the vacuous result of the reconciler never
// having reconciled the ActionsGateway at all.
func waitForProvisioned(t *testing.T, g *gomega.WithT, ns string) {
	t.Helper()
	g.Eventually(func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyName},
			&appsv1.Deployment{})
	}, 20*time.Second, 50*time.Millisecond).Should(gomega.Succeed(),
		"reconciler should provision the proxy Deployment")
}

// TestGMC_ProxyCert_RenewsWhenNearExpiry seeds the proxy TLS Secret with a cert
// inside (and past) the 30-day renew-before window, then asserts the reconciler
// regenerates it — a new serial and a far-future NotAfter. This covers the
// silent-skip-at-expiry failure mode: if the renewal branch were dead, the cert
// would keep its near-expiry serial and the proxy would suffer an mTLS outage.
func TestGMC_ProxyCert_RenewsWhenNearExpiry(t *testing.T) {
	cases := []struct {
		name     string
		ns       string
		notAfter time.Time
	}{
		// Inside the 30-day window: 10 days of remaining lifetime.
		{"near-expiry", "team-proxycert-near", time.Now().Add(10 * 24 * time.Hour)},
		// Already expired: negative remaining lifetime.
		{"expired", "team-proxycert-expired", time.Now().Add(-1 * time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			createNamespace(t, tc.ns)
			createGitHubAppSecret(t, tc.ns, "github-app")

			oldCertPEM, oldKeyPEM, seededSerial := makeSelfSignedCertPEM(t, tc.notAfter)
			seedTLSSecret(t, tc.ns, proxyTLSSecret, oldCertPEM, oldKeyPEM, nil)

			ag := newActionsGateway("proxycert-gateway", tc.ns, "github-app")
			require.NoError(t, k8sClient.Create(ctx, ag))
			t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

			startGMCReconciler(t, nil)
			g := gomega.NewWithT(t)

			// The reconciler must replace the seeded near-expiry cert with a fresh
			// one: different serial, and a NotAfter well beyond the renew window.
			g.Eventually(func() error {
				var sec corev1.Secret
				if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: tc.ns, Name: proxyTLSSecret}, &sec); err != nil {
					return err
				}
				block, _ := pem.Decode(sec.Data[corev1.TLSCertKey])
				if block == nil {
					return fmt.Errorf("no PEM block in regenerated cert")
				}
				cert, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					return err
				}
				if cert.SerialNumber.Cmp(seededSerial) == 0 {
					return fmt.Errorf("serial unchanged %s: cert was not regenerated", seededSerial)
				}
				if time.Until(cert.NotAfter) <= certRenewBefore {
					return fmt.Errorf("regenerated cert still within renew window: NotAfter=%s", cert.NotAfter)
				}
				return nil
			}, 20*time.Second, 50*time.Millisecond).Should(gomega.Succeed())
		})
	}
}

// TestGMC_ProxyCert_LeftUnchangedWhenValid seeds a freshly-valid proxy cert (far
// outside the renew window), lets the reconciler run a full provision plus
// several periodic reconciles, and asserts the Secret is never rewritten — same
// serial, stable resourceVersion. This covers the over-eager-renewal failure
// mode: a renewal branch that fired unconditionally would churn the Secret (and
// roll the proxy pods) on every reconcile.
func TestGMC_ProxyCert_LeftUnchangedWhenValid(t *testing.T) {
	const nsName = "team-proxycert-valid"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	certPEM, keyPEM, seededSerial := makeSelfSignedCertPEM(t, time.Now().Add(200*24*time.Hour))
	seedTLSSecret(t, nsName, proxyTLSSecret, certPEM, keyPEM, nil)

	ag := newActionsGateway("proxycert-gateway", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	startGMCReconciler(t, nil)
	g := gomega.NewWithT(t)

	// Prove a full reconcile actually ran before asserting no-op on the cert.
	waitForProvisioned(t, g, nsName)

	// Settle, then snapshot the cert Secret's resourceVersion.
	time.Sleep(4 * time.Second)
	var baseline corev1.Secret
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyTLSSecret}, &baseline))

	serial, err := certSerialFromSecret(t, nsName, proxyTLSSecret)
	require.NoError(t, err)
	require.Equal(t, 0, serial.Cmp(seededSerial), "seeded cert must be left in place")

	// Over several SyncPeriod=2s reconciles the cert Secret must not be rewritten.
	g.Consistently(func() error {
		var cur corev1.Secret
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: proxyTLSSecret}, &cur); err != nil {
			return err
		}
		if cur.ResourceVersion != baseline.ResourceVersion {
			return fmt.Errorf("proxy cert Secret churned: baseline rv=%s now rv=%s", baseline.ResourceVersion, cur.ResourceVersion)
		}
		return nil
	}, 6*time.Second, 500*time.Millisecond).Should(gomega.Succeed(),
		"a valid proxy cert must not be re-issued on steady-state reconciles")
}

// TestGMC_MetricsCert_RenewsWhenNearExpiry seeds the metrics server Secret with a
// near-expiry (and expired) server cert — plus the client Secret, since the
// renewal decision only evaluates expiry when BOTH bundle Secrets exist — then
// asserts the whole bundle is regenerated (new server-cert serial, far-future
// NotAfter).
func TestGMC_MetricsCert_RenewsWhenNearExpiry(t *testing.T) {
	cases := []struct {
		name     string
		ns       string
		notAfter time.Time
	}{
		{"near-expiry", "team-metricscert-near", time.Now().Add(10 * 24 * time.Hour)},
		{"expired", "team-metricscert-expired", time.Now().Add(-1 * time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			createNamespace(t, tc.ns)
			createGitHubAppSecret(t, tc.ns, "github-app")

			serverCertPEM, serverKeyPEM, seededSerial := makeSelfSignedCertPEM(t, tc.notAfter)
			// Seed both bundle Secrets — the decision short-circuits to regenerate
			// if either is missing, so to exercise the *expiry* branch both must
			// already be present.
			seedTLSSecret(t, tc.ns, metricsTLSSecret, serverCertPEM, serverKeyPEM, map[string][]byte{"ca.crt": serverCertPEM})
			clientCertPEM, clientKeyPEM, _ := makeSelfSignedCertPEM(t, tc.notAfter)
			seedTLSSecret(t, tc.ns, metricsClientSecret, clientCertPEM, clientKeyPEM, map[string][]byte{"ca.crt": serverCertPEM})

			ag := newActionsGateway("metricscert-gateway", tc.ns, "github-app")
			require.NoError(t, k8sClient.Create(ctx, ag))
			t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

			startGMCReconciler(t, nil)
			g := gomega.NewWithT(t)

			g.Eventually(func() error {
				var sec corev1.Secret
				if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: tc.ns, Name: metricsTLSSecret}, &sec); err != nil {
					return err
				}
				block, _ := pem.Decode(sec.Data[corev1.TLSCertKey])
				if block == nil {
					return fmt.Errorf("no PEM block in regenerated server cert")
				}
				cert, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					return err
				}
				if cert.SerialNumber.Cmp(seededSerial) == 0 {
					return fmt.Errorf("server cert serial unchanged %s: bundle was not regenerated", seededSerial)
				}
				if time.Until(cert.NotAfter) <= certRenewBefore {
					return fmt.Errorf("regenerated server cert still within renew window: NotAfter=%s", cert.NotAfter)
				}
				return nil
			}, 20*time.Second, 50*time.Millisecond).Should(gomega.Succeed())
		})
	}
}

// TestGMC_MetricsCert_LeftUnchangedWhenValid seeds a freshly-valid metrics bundle
// (both Secrets, server cert far outside the renew window) and asserts neither
// Secret is rewritten across several reconciles — the over-eager-renewal guard
// for the metrics path.
func TestGMC_MetricsCert_LeftUnchangedWhenValid(t *testing.T) {
	const nsName = "team-metricscert-valid"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	serverCertPEM, serverKeyPEM, seededSerial := makeSelfSignedCertPEM(t, time.Now().Add(200*24*time.Hour))
	seedTLSSecret(t, nsName, metricsTLSSecret, serverCertPEM, serverKeyPEM, map[string][]byte{"ca.crt": serverCertPEM})
	clientCertPEM, clientKeyPEM, _ := makeSelfSignedCertPEM(t, time.Now().Add(200*24*time.Hour))
	seedTLSSecret(t, nsName, metricsClientSecret, clientCertPEM, clientKeyPEM, map[string][]byte{"ca.crt": serverCertPEM})

	ag := newActionsGateway("metricscert-gateway", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	startGMCReconciler(t, nil)
	g := gomega.NewWithT(t)

	waitForProvisioned(t, g, nsName)

	time.Sleep(4 * time.Second)
	var serverBaseline, clientBaseline corev1.Secret
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: metricsTLSSecret}, &serverBaseline))
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: metricsClientSecret}, &clientBaseline))

	serial, err := certSerialFromSecret(t, nsName, metricsTLSSecret)
	require.NoError(t, err)
	require.Equal(t, 0, serial.Cmp(seededSerial), "seeded server cert must be left in place")

	g.Consistently(func() error {
		var server, client corev1.Secret
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: metricsTLSSecret}, &server); err != nil {
			return err
		}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: metricsClientSecret}, &client); err != nil {
			return err
		}
		if server.ResourceVersion != serverBaseline.ResourceVersion {
			return fmt.Errorf("metrics server Secret churned: baseline rv=%s now rv=%s", serverBaseline.ResourceVersion, server.ResourceVersion)
		}
		if client.ResourceVersion != clientBaseline.ResourceVersion {
			return fmt.Errorf("metrics client Secret churned: baseline rv=%s now rv=%s", clientBaseline.ResourceVersion, client.ResourceVersion)
		}
		return nil
	}, 6*time.Second, 500*time.Millisecond).Should(gomega.Succeed(),
		"a valid metrics bundle must not be re-issued on steady-state reconciles")
}
