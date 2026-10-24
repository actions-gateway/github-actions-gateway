//go:build integration

package integration_test

import (
	"fmt"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Proxy failure attribution, the pool side (Q1184), against the real apiserver: the
// CRD's cap on allowedNamespaces, and the Service shapes the fake client accepts
// without validating (port names, a headless clusterIP).

func attributionNamespaceNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("team-%02d", i)
	}
	return out
}

func TestV2_EgressProxy_Attribution_CapsAllowedNamespaces(t *testing.T) {
	const ns = "v2-ep-attr-cap"
	createNamespace(t, ns)
	pool := func(name, mode string, n int) *v2alpha1.EgressProxy {
		return &v2alpha1.EgressProxy{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: v2alpha1.EgressProxySpec{
				FailureAttribution: mode,
				Sharing:            &v2alpha1.ProxySharing{AllowedNamespaces: attributionNamespaceNames(n)},
			},
		}
	}

	err := k8sClient.Create(ctx, pool("over", "PerConsumerPort", 33))
	require.Error(t, err, "33 namespaces must be rejected while attribution is on")
	assert.Contains(t, err.Error(), "at most 32 namespaces")

	at := pool("at", "PerConsumerPort", 32)
	require.NoError(t, k8sClient.Create(ctx, at))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, at) })

	off := pool("off", "", 33)
	require.NoError(t, k8sClient.Create(ctx, off), "the cap binds only while attribution is on")
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, off) })
	assert.Equal(t, "Disabled", off.Spec.FailureAttribution, "the CRD defaults the field to Disabled")

	off.Spec.FailureAttribution = "PerConsumerPort"
	err = k8sClient.Update(ctx, off)
	require.Error(t, err, "turning attribution on over 32 namespaces must be rejected")
	assert.Contains(t, err.Error(), "at most 32 namespaces")
}

func TestV2_EgressProxy_Attribution_ProvisionsThePoolSide(t *testing.T) {
	const providerNS = "v2-attr-provider"
	const consumerNS = "v2-attr-consumer"
	createNamespace(t, providerNS)
	createNamespace(t, consumerNS)
	startEgressProxyReconciler(t, nil)
	createGitHubAppSecret(t, consumerNS, "attr-creds")

	ep := &v2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "attr-pool", Namespace: providerNS},
		Spec: v2alpha1.EgressProxySpec{
			FailureAttribution: "PerConsumerPort",
			Sharing:            &v2alpha1.ProxySharing{AllowedNamespaces: []string{consumerNS}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	ag := newV2GatewayWired("attr-gw", consumerNS, "attr-creds", "")
	ag.Spec.DefaultProxyRef = &v2alpha1.ProxyObjectRef{Name: "attr-pool", Namespace: providerNS}
	ag.Spec.ProxyFailureAttribution = "PerJob"
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	var svc corev1.Service
	require.Eventually(t, func() bool {
		if k8sClient.Get(ctx, types.NamespacedName{Namespace: providerNS, Name: "attr-pool-proxy"}, &svc) != nil {
			return false
		}
		for _, p := range svc.Spec.Ports {
			if p.Name == "attr-8100" {
				return p.TargetPort.IntVal == 8100
			}
		}
		return false
	}, 30*time.Second, 250*time.Millisecond, "the pool Service never gained the consumer's attributed port")

	var headless corev1.Service
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: providerNS, Name: "attr-pool-proxy-attr"}, &headless) == nil
	}, 30*time.Second, 250*time.Millisecond)
	assert.Equal(t, corev1.ClusterIPNone, headless.Spec.ClusterIP, "the read Service must be headless to name each replica")

	var clients corev1.Secret
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: providerNS, Name: "attr-pool-attribution-clients"}, &clients) == nil
	}, 30*time.Second, 250*time.Millisecond)
	assert.NotEmpty(t, clients.Data[consumerNS+".crt"])
}
