package controller

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

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// GHES private-CA trust (Q536). The gateway names a ConfigMap holding the CA that
// fronts its appliance; the GMC validates it, mounts it on the AGC, and names it in
// the env so the AGC's provisioner can project the same bundle into worker pods.

// caBundlePEM returns a self-signed CA certificate in PEM form, standing in for the
// internal CA an operator would put in the ConfigMap.
func caBundlePEM(t *testing.T) string {
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

// ghesGatewayWithCABundle is a directly-egressing GHES gateway naming a CA bundle
// ConfigMap.
func ghesGatewayWithCABundle(name, ns, cmName string) *gmcv2alpha1.ActionsGateway {
	ag := ghesGateway(name, ns, "https://ghes.corp.example/example-org", "")
	ag.Spec.GitHubCABundleRef = &gmcv2alpha1.LocalConfigMapReference{Name: cmName}
	return ag
}

func agcCAVolume(dep *appsv1.Deployment) *corev1.Volume {
	for i, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == githubCAVolumeName {
			return &dep.Spec.Template.Spec.Volumes[i]
		}
	}
	return nil
}

// TestActionsGatewayV2Reconcile_GHESCABundleMounted is the Q536 acceptance: the
// referenced ConfigMap reaches the AGC pod as a read-only, key-pinned mount and its
// name reaches the env the provisioner reads.
func TestActionsGatewayV2Reconcile_GHESCABundleMounted(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-app", Namespace: "team-a"}}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "ghes-ca", Namespace: "team-a"},
		Data:       map[string]string{githubCABundleKey: caBundlePEM(t)},
	}
	ag := ghesGatewayWithCABundle("gw", "team-a", "ghes-ca")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ns, secret, cm, ag).WithStatusSubresource(ag).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test"}
	reconcileV2Gateway(t, r, "team-a", "gw")

	var dep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "team-a", Name: agcNameV2(ag)}, &dep))

	vol := agcCAVolume(&dep)
	require.NotNil(t, vol, "AGC pod must mount the githubCABundleRef ConfigMap")
	require.NotNil(t, vol.ConfigMap, "the CA bundle is public material and travels in a ConfigMap")
	assert.Equal(t, "ghes-ca", vol.ConfigMap.Name)
	assert.Equal(t, []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}, vol.ConfigMap.Items,
		"projection is pinned to ca.crt, not the whole ConfigMap")

	var mount *corev1.VolumeMount
	for i, m := range dep.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == githubCAVolumeName {
			mount = &dep.Spec.Template.Spec.Containers[0].VolumeMounts[i]
		}
	}
	require.NotNil(t, mount, "the AGC container must mount the CA bundle volume")
	assert.Equal(t, githubCAMountPath, mount.MountPath)
	assert.True(t, mount.ReadOnly)

	assert.Equal(t, "ghes-ca", agcEnv(&dep)["GITHUB_CA_CONFIGMAP_NAME"],
		"the AGC needs the name to project the same bundle into worker pods")
	assert.Equal(t, metav1.ConditionFalse,
		condStatus(gatewayConditions(t, c, ag), gmcv2alpha1.ConditionDegraded))
}

// TestActionsGatewayV2Reconcile_NoCABundleRefLeavesPodUnchanged is the negative
// half: a gateway that sets no ref must produce the Deployment it produces today.
func TestActionsGatewayV2Reconcile_NoCABundleRefLeavesPodUnchanged(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-app", Namespace: "team-a"}}
	ag := v2Gateway("gw", "team-a", "github-app", "")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ns, secret, ag).WithStatusSubresource(ag).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test"}
	reconcileV2Gateway(t, r, "team-a", "gw")

	var dep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "team-a", Name: agcNameV2(ag)}, &dep))
	assert.Nil(t, agcCAVolume(&dep), "no ref ⇒ no CA volume")
	_, present := agcEnv(&dep)["GITHUB_CA_CONFIGMAP_NAME"]
	assert.False(t, present, "no ref ⇒ no GITHUB_CA_CONFIGMAP_NAME")
}

// TestActionsGatewayV2Reconcile_CABundleFailsClosed covers the two unresolvable
// cases. Provisioning an AGC with a mount it cannot read would wedge the pod at
// ContainerCreating with no explanation, so the gateway degrades instead — and
// polls, because the GMC does not watch tenant ConfigMaps.
func TestActionsGatewayV2Reconcile_CABundleFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cm     *corev1.ConfigMap
		reason string
	}{
		{
			name:   "ConfigMap absent",
			reason: gmcv2alpha1.ReasonCABundleNotFound,
		},
		{
			name: "no ca.crt key",
			cm: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "ghes-ca", Namespace: "team-a"},
				Data:       map[string]string{"bundle.pem": "irrelevant"},
			},
			reason: gmcv2alpha1.ReasonCABundleInvalid,
		},
		{
			name: "ca.crt is not a certificate",
			cm: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "ghes-ca", Namespace: "team-a"},
				Data:       map[string]string{githubCABundleKey: "-----BEGIN CERTIFICATE-----\nnope\n"},
			},
			reason: gmcv2alpha1.ReasonCABundleInvalid,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := actionsGatewayV2TestScheme(t)
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-app", Namespace: "team-a"}}
			ag := ghesGatewayWithCABundle("gw", "team-a", "ghes-ca")

			objs := []client.Object{ns, secret, ag}
			if tc.cm != nil {
				objs = append(objs, tc.cm)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(objs...).WithStatusSubresource(ag).Build()
			r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test"}
			res := reconcileV2Gateway(t, r, "team-a", "gw")

			assert.Equal(t, githubCABundleReprobeInterval, res.RequeueAfter,
				"an unresolved bundle must be re-read; nothing watches tenant ConfigMaps")

			conds := gatewayConditions(t, c, ag)
			degraded := meta.FindStatusCondition(conds, gmcv2alpha1.ConditionDegraded)
			require.NotNil(t, degraded)
			assert.Equal(t, metav1.ConditionTrue, degraded.Status)
			assert.Equal(t, tc.reason, degraded.Reason)
			assert.Equal(t, metav1.ConditionFalse, condStatus(conds, gmcv2alpha1.ConditionReady))

			var dep appsv1.Deployment
			err := c.Get(context.Background(),
				types.NamespacedName{Namespace: "team-a", Name: agcNameV2(ag)}, &dep)
			assert.True(t, apierrors.IsNotFound(err),
				"fail closed: no AGC is provisioned while the bundle does not resolve")
		})
	}
}

// TestActionsGatewayV2Reconcile_CABundleRecovers proves the poll converges: the
// operator applies the missing ConfigMap and the next reconcile provisions, with no
// edit to the gateway.
func TestActionsGatewayV2Reconcile_CABundleRecovers(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-app", Namespace: "team-a"}}
	ag := ghesGatewayWithCABundle("gw", "team-a", "ghes-ca")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ns, secret, ag).WithStatusSubresource(ag).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test"}
	reconcileV2Gateway(t, r, "team-a", "gw")

	ctx := context.Background()
	require.NoError(t, c.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "ghes-ca", Namespace: "team-a"},
		Data:       map[string]string{githubCABundleKey: caBundlePEM(t)},
	}))
	reconcileV2Gateway(t, r, "team-a", "gw")

	var dep appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: agcNameV2(ag)}, &dep))
	require.NotNil(t, agcCAVolume(&dep), "the AGC must provision once the ConfigMap exists")
}

// gatewayConditions re-reads the gateway's conditions after a reconcile.
func gatewayConditions(t *testing.T, c client.Client, ag *gmcv2alpha1.ActionsGateway) []metav1.Condition {
	t.Helper()
	var got gmcv2alpha1.ActionsGateway
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: ag.Namespace, Name: ag.Name}, &got))
	return got.Status.Conditions
}
