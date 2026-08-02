//go:build integration

package integration_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// A GHES appliance fronted by a private CA is not in the AGC's trust pool, which
// holds the system roots plus the egress proxy's own CA (Q536). spec.githubCABundleRef
// names a ConfigMap the GMC validates and mounts.
//
// These run against the real apiserver rather than the builder alone for two reasons
// the builder cannot cover: the CRD has to actually accept the new field, and the
// fail-closed path depends on an uncached read the fake client models differently.

// testCABundle returns a self-signed CA certificate in PEM form.
func testCABundle(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "corp-root-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// TestV2_GHESGateway_MountsPrivateCABundle: the referenced ConfigMap reaches the AGC
// pod as a key-pinned mount, and its name reaches the env the AGC's own provisioner
// reads to project the same bundle into worker pods.
func TestV2_GHESGateway_MountsPrivateCABundle(t *testing.T) {
	const ns = "v2-ghes-cabundle"
	createNamespaceWithLabels(t, ns, map[string]string{
		v2alpha1.SecurityProfileLabel: v2alpha1.SecurityProfileRestricted,
	})
	createGitHubAppSecret(t, ns, "github-app")

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "ghes-ca", Namespace: ns},
		Data:       map[string]string{"ca.crt": testCABundle(t)},
	}
	require.NoError(t, k8sClient.Create(ctx, cm))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cm) })

	ag := newV2GatewayWired("cagw", ns, "github-app", "")
	ag.Spec.GitHubURL = "https://ghes.example.com/example-org"
	ag.Spec.GitHubCABundleRef = &v2alpha1.LocalConfigMapReference{Name: "ghes-ca"}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startActionsGatewayV2Reconciler(t)

	g := gomega.NewWithT(t)
	g.Eventually(func() string {
		var dep appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "cagw-agc"}, &dep); err != nil {
			return ""
		}
		return envValue(&dep, "GITHUB_CA_CONFIGMAP_NAME")
	}, 15*time.Second, 100*time.Millisecond).Should(gomega.Equal("ghes-ca"),
		"the AGC must be told which ConfigMap holds the appliance's CA")

	var dep appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "cagw-agc"}, &dep))

	var src *corev1.ConfigMapVolumeSource
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == "github-ca" {
			src = v.ConfigMap
		}
	}
	require.NotNil(t, src, "the AGC pod must mount the CA bundle ConfigMap")
	require.Equal(t, "ghes-ca", src.Name)
	require.Len(t, src.Items, 1, "projection pinned to ca.crt")
	require.Equal(t, "ca.crt", src.Items[0].Key)

	var mounted bool
	for _, m := range dep.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == "github-ca" {
			mounted = m.ReadOnly && m.MountPath == "/etc/actions-gateway/github-ca"
		}
	}
	require.True(t, mounted, "the AGC container must mount the bundle read-only at the path it reads")
}

// TestV2_GHESGateway_MissingCABundleFailsClosed: a ref naming a ConfigMap that does
// not exist degrades the gateway and provisions no AGC, rather than creating a
// Deployment whose pod would sit at ContainerCreating with no explanation. Applying
// the ConfigMap then converges with no edit to the gateway.
//
// What drives that convergence here is the suite's 2s cache resync as much as the
// reconciler's own RequeueAfter — this suite cannot separate them (Q541), and the
// interval itself is pinned by the unit test. What this asserts is the property that
// matters to an operator: the gateway recovers on its own.
func TestV2_GHESGateway_MissingCABundleFailsClosed(t *testing.T) {
	const ns = "v2-ghes-cabundle-missing"
	createNamespaceWithLabels(t, ns, map[string]string{
		v2alpha1.SecurityProfileLabel: v2alpha1.SecurityProfileRestricted,
	})
	createGitHubAppSecret(t, ns, "github-app")

	ag := newV2GatewayWired("missingca", ns, "github-app", "")
	ag.Spec.GitHubURL = "https://ghes.example.com/example-org"
	ag.Spec.GitHubCABundleRef = &v2alpha1.LocalConfigMapReference{Name: "ghes-ca"}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startActionsGatewayV2Reconciler(t)

	g := gomega.NewWithT(t)
	g.Eventually(func() string {
		var got v2alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "missingca"}, &got); err != nil {
			return ""
		}
		c := meta.FindStatusCondition(got.Status.Conditions, v2alpha1.ConditionDegraded)
		if c == nil || c.Status != metav1.ConditionTrue {
			return ""
		}
		return c.Reason
	}, 15*time.Second, 100*time.Millisecond).Should(gomega.Equal(v2alpha1.ReasonCABundleNotFound))

	var dep appsv1.Deployment
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "missingca-agc"}, &dep)
	require.Error(t, err, "no AGC may be provisioned while the CA bundle does not resolve")

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "ghes-ca", Namespace: ns},
		Data:       map[string]string{"ca.crt": testCABundle(t)},
	}
	require.NoError(t, k8sClient.Create(ctx, cm))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cm) })

	g.Eventually(func() string {
		var dep appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "missingca-agc"}, &dep); err != nil {
			return ""
		}
		return envValue(&dep, "GITHUB_CA_CONFIGMAP_NAME")
	}, 20*time.Second, 250*time.Millisecond).Should(gomega.Equal("ghes-ca"),
		"the gateway must recover once the operator applies the ConfigMap")
}
