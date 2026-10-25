//go:build integration

package integration_test

import (
	"fmt"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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
	startActionsGatewayV2Reconciler(t)
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

	// The opted-in namespace's workers may reach the attributed port range, which
	// the real apiserver must accept as an endPort rule.
	workloadRange := func() bool {
		var np networkingv1.NetworkPolicy
		if k8sClient.Get(ctx, types.NamespacedName{Namespace: consumerNS, Name: "attr-gw-workload"}, &np) != nil {
			return false
		}
		for _, rule := range np.Spec.Egress {
			for _, p := range rule.Ports {
				if p.Port != nil && p.Port.IntVal == 8100 && p.EndPort != nil && *p.EndPort == 8132 {
					return true
				}
			}
		}
		return false
	}
	require.Eventually(t, workloadRange, 30*time.Second, 250*time.Millisecond, "the workload policy never admitted the attributed port range")

	// Turning the gateway's opt-in off must clear the pool against real Secret
	// validation: the TLS Secret keeps its required keys, blanked.
	setGatewayAttribution(t, types.NamespacedName{Namespace: consumerNS, Name: "attr-gw"}, "Disabled")
	require.Eventually(t, func() bool {
		var s corev1.Secret
		if k8sClient.Get(ctx, types.NamespacedName{Namespace: providerNS, Name: "attr-pool-attribution-tls"}, &s) != nil {
			return false
		}
		return len(s.Data[corev1.TLSPrivateKeyKey]) == 0 &&
			k8sClient.Get(ctx, types.NamespacedName{Namespace: providerNS, Name: "attr-pool-proxy-attr"}, &corev1.Service{}) != nil
	}, 30*time.Second, 250*time.Millisecond, "the pool never cleared its attribution key material and read Service")
	require.Eventually(t, func() bool { return !workloadRange() }, 30*time.Second, 250*time.Millisecond,
		"the workload policy kept the attributed port range after the namespace opted out")
	var ready v2alpha1.EgressProxy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: providerNS, Name: "attr-pool"}, &ready))
	for _, c := range ready.Status.Conditions {
		if c.Type == "Degraded" {
			assert.Equal(t, metav1.ConditionFalse, c.Status, "the pool must not go Degraded clearing attribution: %s", c.Message)
		}
	}
}

// setGatewayAttribution sets a gateway's proxyFailureAttribution, rereading on a
// conflict: the gateway reconciler writes the same object.
func setGatewayAttribution(t *testing.T, key types.NamespacedName, value string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var ag v2alpha1.ActionsGateway
		if k8sClient.Get(ctx, key, &ag) != nil {
			return false
		}
		ag.Spec.ProxyFailureAttribution = value
		return k8sClient.Update(ctx, &ag) == nil
	}, 10*time.Second, 50*time.Millisecond, "could not set %s's proxyFailureAttribution to %s", key, value)
}

// A namespace with a RunnerSet that opts out keeps its port listening and admitted,
// and its workers' egress to the range, across both reconcilers on the real
// apiserver (Q1208). Nothing here waits out the hold: the unit tests release it on
// a fake clock.
func TestV2_EgressProxy_Attribution_HoldsAnOptedOutNamespacesPort(t *testing.T) {
	const providerNS = "v2-attr-hold-provider"
	const consumerNS = "v2-attr-hold-consumer"
	createNamespace(t, providerNS)
	createNamespace(t, consumerNS)
	startEgressProxyReconciler(t, nil)
	startActionsGatewayV2Reconciler(t)
	createGitHubAppSecret(t, consumerNS, "hold-creds")

	ep := &v2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "hold-pool", Namespace: providerNS},
		Spec: v2alpha1.EgressProxySpec{
			FailureAttribution: "PerConsumerPort",
			Sharing:            &v2alpha1.ProxySharing{AllowedNamespaces: []string{consumerNS}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })
	rs := &v2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "hold-rs", Namespace: consumerNS},
		Spec: v2alpha1.RunnerSetSpec{
			GatewayRef:        v2alpha1.ObjectRef{Name: "hold-gw"},
			RunnerLabels:      []string{"self-hosted"},
			MaxWorkerLifetime: &metav1.Duration{Duration: time.Hour},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rs) })
	ag := newV2GatewayWired("hold-gw", consumerNS, "hold-creds", "")
	ag.Spec.DefaultProxyRef = &v2alpha1.ProxyObjectRef{Name: "hold-pool", Namespace: providerNS}
	ag.Spec.ProxyFailureAttribution = "PerJob"
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	poolState := func() (ports, held string) {
		var dep appsv1.Deployment
		if k8sClient.Get(ctx, types.NamespacedName{Namespace: providerNS, Name: "hold-pool-proxy"}, &dep) != nil {
			return "", ""
		}
		for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
			if e.Name == "PROXY_ATTRIBUTION_PORTS" {
				ports = e.Value
			}
		}
		return ports, dep.Annotations["actions-gateway.com/attribution-held-until"]
	}
	workloadRange := func() bool {
		var np networkingv1.NetworkPolicy
		if k8sClient.Get(ctx, types.NamespacedName{Namespace: consumerNS, Name: "hold-gw-workload"}, &np) != nil {
			return false
		}
		for _, rule := range np.Spec.Egress {
			for _, p := range rule.Ports {
				if p.Port != nil && p.Port.IntVal == 8100 && p.EndPort != nil && *p.EndPort == 8132 {
					return true
				}
			}
		}
		return false
	}
	require.Eventually(t, func() bool { p, _ := poolState(); return p == consumerNS+"=8100" && workloadRange() },
		30*time.Second, 250*time.Millisecond, "the consumer was never given its port")

	setGatewayAttribution(t, types.NamespacedName{Namespace: consumerNS, Name: "hold-gw"}, "Disabled")
	require.Eventually(t, func() bool { _, h := poolState(); return h != "" }, 30*time.Second, 250*time.Millisecond,
		"the pool never recorded a hold for the departing namespace")
	ports, held := poolState()
	assert.Equal(t, consumerNS+"=8100", ports, "the held port keeps its listener")
	release, err := time.Parse(time.RFC3339, held[len(consumerNS)+1:])
	require.NoError(t, err, "hold %q", held)
	assert.WithinDuration(t, time.Now().Add(time.Hour), release, time.Minute, "held for the RunnerSet's maxWorkerLifetime")
	assert.Never(t, func() bool { return !workloadRange() }, 3*time.Second, 250*time.Millisecond,
		"the workers' egress to the range must outlive the opt-out while the port is held")
}
