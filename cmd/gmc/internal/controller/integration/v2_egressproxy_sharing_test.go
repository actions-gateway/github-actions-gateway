//go:build integration

package integration_test

import (
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Cross-namespace EgressProxy sharing (Q166, M4, §H.9) against the real apiserver.
// The denial is a controller decision reached from two objects in two namespaces, so
// the tier has to be one where both exist and the reconciler actually runs; a fake
// client cannot show that the consent check is what gates provisioning.

const (
	shareProviderNS = "v2-share-provider"
	shareConsumerNS = "v2-share-consumer"
	sharedProxyName = "shared-pool"
)

func shareConfigMapName() string { return "proxy-share-" + shareProviderNS + "-" + sharedProxyName }

// gatewayCondition returns the named condition on a freshly-read gateway, or nil.
func gatewayCondition(t *testing.T, ns, name, condType string) *metav1.Condition {
	t.Helper()
	var ag v2alpha1.ActionsGateway
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ag); err != nil {
		return nil
	}
	return meta.FindStatusCondition(ag.Status.Conditions, condType)
}

// A gateway naming a proxy in another namespace that has granted nothing must not
// reach Ready, and must say why. This is the security property the whole milestone
// exists to enforce: naming a proxy from the consumer side authorizes nothing.
func TestV2_ProxySharing_UngrantedCrossNamespaceRefIsDenied(t *testing.T) {
	createNamespace(t, shareProviderNS)
	createNamespace(t, shareConsumerNS)
	startActionsGatewayV2Reconciler(t)

	ep := &v2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: sharedProxyName, Namespace: shareProviderNS},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	createGitHubAppSecret(t, shareConsumerNS, "share-creds")

	ag := newV2GatewayWired("share-gw", shareConsumerNS, "share-creds", "")
	ag.Spec.DefaultProxyRef = &v2alpha1.ProxyObjectRef{
		Name: sharedProxyName, Namespace: shareProviderNS,
	}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	require.Eventually(t, func() bool {
		c := gatewayCondition(t, shareConsumerNS, "share-gw", v2alpha1.ConditionDegraded)
		return c != nil && c.Reason == v2alpha1.ReasonProxyShareNotGranted
	}, 30*time.Second, 250*time.Millisecond,
		"gateway did not report ProxyShareNotGranted for an unconsented cross-namespace proxyRef")

	ready := gatewayCondition(t, shareConsumerNS, "share-gw", v2alpha1.ConditionReady)
	if ready != nil {
		assert.NotEqual(t, metav1.ConditionTrue, ready.Status,
			"gateway reached Ready despite an unconsented cross-namespace proxy")
	}

	// Fail-closed means no wiring, not merely a condition: nothing may have been
	// projected into the consumer namespace on the strength of an ungranted name.
	var cm corev1.ConfigMap
	err := k8sClient.Get(ctx, types.NamespacedName{
		Namespace: shareConsumerNS, Name: shareConfigMapName(),
	}, &cm)
	assert.True(t, apierrors.IsNotFound(err),
		"a CA projection exists for an ungranted reference (err=%v)", err)
}

// Granting consent flips the same reference, and the ingress rule that admits it
// carries both selectors in one peer. The two halves are asserted together because a
// grant that produces a condition change without the NetworkPolicy is a proxy the
// consumer still cannot reach.
func TestV2_ProxySharing_GrantAdmitsConsumerAndProjectsCA(t *testing.T) {
	const providerNS = "v2-share-grant-provider"
	const consumerNS = "v2-share-grant-consumer"
	createNamespace(t, providerNS)
	createNamespace(t, consumerNS)
	startEgressProxyReconciler(t, nil)
	createGitHubAppSecret(t, consumerNS, "share-creds")

	ep := &v2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: sharedProxyName, Namespace: providerNS},
		Spec: v2alpha1.EgressProxySpec{
			Sharing: &v2alpha1.ProxySharing{AllowedNamespaces: []string{consumerNS}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	ag := newV2GatewayWired("share-gw", consumerNS, "share-creds", "")
	ag.Spec.DefaultProxyRef = &v2alpha1.ProxyObjectRef{Name: sharedProxyName, Namespace: providerNS}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	// The CA lands in the consumer namespace, carrying the cert and the connection
	// facts the AGC resolves the reference from.
	cmKey := types.NamespacedName{
		Namespace: consumerNS,
		Name:      "proxy-share-" + providerNS + "-" + sharedProxyName,
	}
	var cm corev1.ConfigMap
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, cmKey, &cm) == nil && cm.Data["ca.crt"] != ""
	}, 30*time.Second, 250*time.Millisecond, "no CA projection appeared in the granted namespace")

	assert.Equal(t, sharedProxyName+"-proxy."+providerNS+".svc.cluster.local", cm.Data["proxy-host"])
	// The consumer's AGC reads the pool's audit setting only from here (Q1069); the
	// CRD defaults an unset spec.auditLogging to Off before the GMC ever sees it.
	assert.Equal(t, "Off", cm.Data["audit-logging"])
	assert.NotContains(t, cm.Data["ca.crt"], "PRIVATE KEY",
		"the projection leaked private key material into the consumer namespace")

	// The provider's ingress admits the consumer, with both selectors in one peer.
	var np networkingv1.NetworkPolicy
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{
			Namespace: providerNS, Name: sharedProxyName + "-proxy",
		}, &np) == nil
	}, 30*time.Second, 250*time.Millisecond)

	found := false
	for _, rule := range np.Spec.Ingress {
		for _, peer := range rule.From {
			if peer.NamespaceSelector == nil ||
				peer.NamespaceSelector.MatchLabels[corev1.LabelMetadataName] != consumerNS {
				continue
			}
			found = true
			require.Len(t, rule.From, 1,
				"the granted rule has more than one peer, so its selectors OR instead of AND")
			assert.NotNil(t, peer.PodSelector,
				"the granted peer admits every pod in the consumer namespace")
		}
	}
	assert.True(t, found, "the provider's NetworkPolicy does not admit the granted namespace")
}

// Withdrawing a grant has to revoke access, not merely stop refreshing it. The
// consumer treats the projection's presence as the grant, so a projection that
// outlives the grant is a live credential for a revoked reference.
func TestV2_ProxySharing_RevokedGrantDeletesProjection(t *testing.T) {
	const providerNS = "v2-share-revoke-provider"
	const consumerNS = "v2-share-revoke-consumer"
	createNamespace(t, providerNS)
	createNamespace(t, consumerNS)
	startEgressProxyReconciler(t, nil)
	createGitHubAppSecret(t, consumerNS, "share-creds")

	ep := &v2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: sharedProxyName, Namespace: providerNS},
		Spec: v2alpha1.EgressProxySpec{
			Sharing: &v2alpha1.ProxySharing{AllowedNamespaces: []string{consumerNS}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	ag := newV2GatewayWired("share-gw", consumerNS, "share-creds", "")
	ag.Spec.DefaultProxyRef = &v2alpha1.ProxyObjectRef{Name: sharedProxyName, Namespace: providerNS}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	cmKey := types.NamespacedName{
		Namespace: consumerNS,
		Name:      "proxy-share-" + providerNS + "-" + sharedProxyName,
	}
	var cm corev1.ConfigMap
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, cmKey, &cm) == nil
	}, 30*time.Second, 250*time.Millisecond, "no projection to revoke")

	// Withdraw consent.
	require.Eventually(t, func() bool {
		var live v2alpha1.EgressProxy
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Namespace: providerNS, Name: sharedProxyName,
		}, &live); err != nil {
			return false
		}
		live.Spec.Sharing = nil
		return k8sClient.Update(ctx, &live) == nil
	}, 10*time.Second, 250*time.Millisecond)

	require.Eventually(t, func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, cmKey, &cm))
	}, 30*time.Second, 250*time.Millisecond,
		"the CA projection outlived the grant that authorized it")

	// The provider's ingress must close again too.
	require.Eventually(t, func() bool {
		var np networkingv1.NetworkPolicy
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Namespace: providerNS, Name: sharedProxyName + "-proxy",
		}, &np); err != nil {
			return false
		}
		for _, rule := range np.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.NamespaceSelector != nil &&
					peer.NamespaceSelector.MatchLabels[corev1.LabelMetadataName] == consumerNS {
					return false
				}
			}
		}
		return true
	}, 30*time.Second, 250*time.Millisecond,
		"the provider still admits a namespace whose grant was withdrawn")
}

// A gateway-level defaultProxyRef to a granted pool must provision an AGC that can
// start (Q1209). The pod's volumes are the observable: a Secret or ConfigMap volume
// naming an object absent from the AGC's namespace holds the pod at
// ContainerCreating, and the pool's TLS Secret exists only in the provider's.
func TestV2_ProxySharing_GatewayDefaultProxyRefWiresAGCFromProjection(t *testing.T) {
	const providerNS = "v2-share-agc-provider"
	const consumerNS = "v2-share-agc-consumer"
	createNamespace(t, providerNS)
	createNamespace(t, consumerNS)
	startEgressProxyReconciler(t, nil)
	startActionsGatewayV2Reconciler(t)
	createGitHubAppSecret(t, consumerNS, "share-creds")

	ep := &v2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: sharedProxyName, Namespace: providerNS},
		Spec: v2alpha1.EgressProxySpec{
			Sharing: &v2alpha1.ProxySharing{AllowedNamespaces: []string{consumerNS}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	ag := newV2GatewayWired("share-gw", consumerNS, "share-creds", "")
	ag.Spec.DefaultProxyRef = &v2alpha1.ProxyObjectRef{Name: sharedProxyName, Namespace: providerNS}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	var dep appsv1.Deployment
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: consumerNS, Name: "share-gw-agc"}, &dep) == nil
	}, 60*time.Second, 250*time.Millisecond, "AGC Deployment was not created")

	proxyAddr := "https://" + sharedProxyName + "-proxy." + providerNS + ".svc.cluster.local:8080"
	assert.Equal(t, proxyAddr, envValue(&dep, "HTTPS_PROXY"), "the AGC dials the pool in the provider's namespace")
	assert.Equal(t, proxyAddr, envValue(&dep, "HTTP_PROXY"))
	assert.Empty(t, envValue(&dep, "PROXY_TLS_SECRET_NAME"),
		"the AGC was handed a Secret name that resolves only in the provider's namespace")

	var caVolume *corev1.Volume
	for i := range dep.Spec.Template.Spec.Volumes {
		v := &dep.Spec.Template.Spec.Volumes[i]
		if v.Name == "proxy-ca" {
			caVolume = v
		}
		var err error
		switch {
		case v.Secret != nil:
			err = k8sClient.Get(ctx, types.NamespacedName{Namespace: consumerNS, Name: v.Secret.SecretName}, &corev1.Secret{})
		case v.ConfigMap != nil:
			err = k8sClient.Get(ctx, types.NamespacedName{Namespace: consumerNS, Name: v.ConfigMap.Name}, &corev1.ConfigMap{})
		default:
			continue
		}
		assert.NoError(t, err, "AGC volume %q names an object absent from namespace %q, so the pod cannot start", v.Name, consumerNS)
	}
	require.NotNil(t, caVolume, "the proxied AGC mounts no proxy CA")
	require.NotNil(t, caVolume.ConfigMap, "the proxy CA must come from the share projection")
	assert.Equal(t, shareConfigMapNameFor(providerNS), caVolume.ConfigMap.Name)
	assert.Equal(t, []corev1.KeyToPath{{Key: "ca.crt", Path: "tls.crt"}}, caVolume.ConfigMap.Items,
		"the AGC reads the proxy CA from tls.crt under its fixed mount")
}

// Until the projection exists, the AGC is not provisioned: its CA volume would name
// a ConfigMap that is not there, which is the same wedge with a different object.
func TestV2_ProxySharing_GatewayDefaultProxyRefWaitsForProjection(t *testing.T) {
	const providerNS = "v2-share-wait-provider"
	const consumerNS = "v2-share-wait-consumer"
	createNamespace(t, providerNS)
	createNamespace(t, consumerNS)
	// No EgressProxy reconciler: nothing projects the share.
	startActionsGatewayV2Reconciler(t)
	createGitHubAppSecret(t, consumerNS, "share-creds")

	ep := &v2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: sharedProxyName, Namespace: providerNS},
		Spec: v2alpha1.EgressProxySpec{
			Sharing: &v2alpha1.ProxySharing{AllowedNamespaces: []string{consumerNS}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	ag := newV2GatewayWired("share-gw", consumerNS, "share-creds", "")
	ag.Spec.DefaultProxyRef = &v2alpha1.ProxyObjectRef{Name: sharedProxyName, Namespace: providerNS}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	require.Eventually(t, func() bool {
		c := gatewayCondition(t, consumerNS, "share-gw", v2alpha1.ConditionDegraded)
		return c != nil && c.Reason == v2alpha1.ReasonProxyShareNotGranted
	}, 30*time.Second, 250*time.Millisecond,
		"gateway did not fail closed while its proxy's share projection is absent")
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: consumerNS, Name: "share-gw-agc"}, &appsv1.Deployment{})
	assert.True(t, apierrors.IsNotFound(err), "an AGC was provisioned with no projection to mount (err=%v)", err)
}

func shareConfigMapNameFor(providerNS string) string {
	return "proxy-share-" + providerNS + "-" + sharedProxyName
}
