//go:build integration

package integration_test

import (
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
