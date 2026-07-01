package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// nearExpiryCertPEM returns a self-signed cert PEM that expires in 1 hour —
// well inside proxyCertRenewBefore/metricsCertRenewBefore (30 days) — so tests
// can drive the "near expiry" renewal branch without waiting on real time.
func nearExpiryCertPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "near-expiry"},
		NotBefore:             time.Now().Add(-2 * time.Hour),
		NotAfter:              time.Now().Add(1 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return buf.Bytes()
}

// These tests exercise ensureProxyCert / ensureMetricsCerts against a fake
// client — the issue/no-op/regenerate branches that carry the Q88 debug
// audit logging. They run without envtest (the apply* path is a plain
// CreateOrPatch on the fake client).

func TestEnsureProxyCert_IssuesWhenMissing(t *testing.T) {
	scheme := applyTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := applyTestReconciler(t, c, scheme)
	ag := applyTestAG()

	require.NoError(t, r.ensureProxyCert(context.Background(), ag))

	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: proxyTLSSecretName}, &sec))
	cert, err := parseCertPEM(sec.Data[corev1.TLSCertKey])
	require.NoError(t, err, "issued Secret must hold a parseable cert")
	assert.NotEmpty(t, sec.Data[corev1.TLSPrivateKeyKey], "issued Secret must hold a private key")
	require.NotNil(t, cert)
}

func TestEnsureProxyCert_NoOpWhenValid(t *testing.T) {
	scheme := applyTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := applyTestReconciler(t, c, scheme)
	ag := applyTestAG()

	// First call issues the cert.
	require.NoError(t, r.ensureProxyCert(context.Background(), ag))
	var first corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: proxyTLSSecretName}, &first))

	// Second call sees a valid, far-from-expiry cert and must leave it untouched.
	require.NoError(t, r.ensureProxyCert(context.Background(), ag))
	var second corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: proxyTLSSecretName}, &second))
	assert.Equal(t, first.Data[corev1.TLSCertKey], second.Data[corev1.TLSCertKey], "a valid cert must not be re-issued")
}

func TestEnsureProxyCert_RegeneratesWhenUnparseable(t *testing.T) {
	scheme := applyTestScheme(t)
	garbage := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-ns", Name: proxyTLSSecretName},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("not a cert"), corev1.TLSPrivateKeyKey: []byte("nope")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(garbage).Build()
	r := applyTestReconciler(t, c, scheme)
	ag := applyTestAG()

	require.NoError(t, r.ensureProxyCert(context.Background(), ag))

	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: proxyTLSSecretName}, &sec))
	_, err := parseCertPEM(sec.Data[corev1.TLSCertKey])
	require.NoError(t, err, "an unparseable cert must be regenerated into a parseable one")
}

// TestEnsureProxyCert_RegeneratesWhenNearExpiry verifies a parseable but
// soon-to-expire cert (inside proxyCertRenewBefore) is proactively re-issued
// rather than left to expire.
func TestEnsureProxyCert_RegeneratesWhenNearExpiry(t *testing.T) {
	scheme := applyTestScheme(t)
	ag := applyTestAG()
	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ag.Namespace, Name: proxyTLSSecretName},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       nearExpiryCertPEM(t),
			corev1.TLSPrivateKeyKey: []byte("placeholder"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stale).Build()
	r := applyTestReconciler(t, c, scheme)

	require.NoError(t, r.ensureProxyCert(context.Background(), ag))

	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: proxyTLSSecretName}, &sec))
	cert, err := parseCertPEM(sec.Data[corev1.TLSCertKey])
	require.NoError(t, err)
	assert.True(t, cert.NotAfter.After(time.Now().Add(300*24*time.Hour)), "the near-expiry cert must be replaced with a freshly issued, long-lived one")
}

func TestEnsureMetricsCerts_IssuesWhenMissing(t *testing.T) {
	scheme := applyTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := applyTestReconciler(t, c, scheme)
	ag := applyTestAG()

	require.NoError(t, r.ensureMetricsCerts(context.Background(), ag))

	for _, name := range []string{metricsTLSSecretName, metricsClientSecretName} {
		var sec corev1.Secret
		require.NoErrorf(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: name}, &sec), "Secret %s must be created", name)
		assert.NotEmptyf(t, sec.Data[corev1.TLSCertKey], "Secret %s must hold a cert", name)
	}

	// Second call with both Secrets present and the server cert far from expiry
	// must be a no-op.
	var before corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: metricsTLSSecretName}, &before))
	require.NoError(t, r.ensureMetricsCerts(context.Background(), ag))
	var after corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: metricsTLSSecretName}, &after))
	assert.Equal(t, before.Data[corev1.TLSCertKey], after.Data[corev1.TLSCertKey], "a valid metrics bundle must not be re-issued")
}
