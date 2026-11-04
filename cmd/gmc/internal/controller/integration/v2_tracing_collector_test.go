//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	v2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TestV2_Tracing_Admission: both fields are only meaningful with an endpoint that uses
// them, and a mismatch would otherwise export untrusted or not at all with no visible
// signal, because a span the exporter drops never reaches a condition.
func TestV2_Tracing_Admission(t *testing.T) {
	const ns = "v2-tracing-admission"
	createNamespace(t, ns)

	const https = "https://collector.observability.svc.cluster.local:4317"
	bundle := &v2.LocalConfigMapReference{Name: "collector-ca"}
	peer := &v2.EgressPeer{CIDR: "10.0.5.7/32"}
	insecure := true

	for name, tc := range map[string]struct {
		tracing v2.TracingConfig
		reject  string
	}{
		"bundle with TLS":     {tracing: v2.TracingConfig{Endpoint: https, CABundleRef: bundle}},
		"peer with TLS":       {tracing: v2.TracingConfig{Endpoint: https, NetworkPolicy: peer}},
		"peer over plaintext": {tracing: v2.TracingConfig{Endpoint: "http://collector:4317", NetworkPolicy: peer}},
		"peer with bare host": {tracing: v2.TracingConfig{Endpoint: "collector:4317", NetworkPolicy: peer}},
		"bundle over http": {
			tracing: v2.TracingConfig{Endpoint: "http://collector:4317", CABundleRef: bundle},
			reject:  "caBundleRef needs an https:// endpoint",
		},
		"bundle with a bare host": {
			tracing: v2.TracingConfig{Endpoint: "collector:4317", CABundleRef: bundle},
			reject:  "caBundleRef needs an https:// endpoint",
		},
		"bundle with insecure": {
			tracing: v2.TracingConfig{Endpoint: https, Insecure: &insecure, CABundleRef: bundle},
			reject:  "caBundleRef needs an https:// endpoint",
		},
		"bundle with no endpoint": {
			tracing: v2.TracingConfig{CABundleRef: bundle},
			reject:  "caBundleRef needs an https:// endpoint",
		},
		"peer with no endpoint": {
			tracing: v2.TracingConfig{NetworkPolicy: peer},
			reject:  "networkPolicy needs an endpoint",
		},
		"peer with both forms": {
			tracing: v2.TracingConfig{Endpoint: https, NetworkPolicy: &v2.EgressPeer{
				CIDR:        "10.0.5.7/32",
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "otel-collector"}},
			}},
			reject: "exactly one of cidr or a pod/namespace selector",
		},
	} {
		t.Run(name, func(t *testing.T) {
			ag := newV2GatewayWired("tracing-admission", ns, "github-app", "")
			ag.Spec.Tracing = tc.tracing
			err := k8sClient.Create(ctx, ag)
			if tc.reject == "" {
				require.NoError(t, err)
				require.NoError(t, k8sClient.Delete(context.Background(), ag))
				require.Eventually(t, func() bool {
					return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: ag.Name}, &v2.ActionsGateway{}))
				}, 15*time.Second, 100*time.Millisecond)
				return
			}
			require.Error(t, err)
			assert.True(t, apierrors.IsInvalid(err), "expected Invalid, got %v", err)
			assert.Contains(t, err.Error(), tc.reject)
		})
	}
}

// TestV2_Tracing_PeerAndBundleReachTheAGC: the two fields arrive on the AGC's pod and
// policy through the real apiserver, and the rule lands on the AGC's policy alone, so a
// worker pod's untrusted job code still has no route to the collector.
func TestV2_Tracing_PeerAndBundleReachTheAGC(t *testing.T) {
	const ns = "v2-tracing-collector"
	createNamespaceWithLabels(t, ns, map[string]string{
		v2.SecurityProfileLabel: v2.SecurityProfileRestricted,
	})
	createGitHubAppSecret(t, ns, "github-app")

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "collector-ca", Namespace: ns},
		Data:       map[string]string{"ca.crt": testCABundle(t)},
	}
	require.NoError(t, k8sClient.Create(ctx, cm))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cm) })

	ag := newV2GatewayWired("otel", ns, "github-app", "")
	ag.Spec.Tracing = v2.TracingConfig{
		Endpoint:      "https://collector.observability.svc.cluster.local:4317",
		CABundleRef:   &v2.LocalConfigMapReference{Name: "collector-ca"},
		NetworkPolicy: &v2.EgressPeer{CIDR: "10.0.5.7/32"},
	}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startActionsGatewayV2Reconciler(t)

	var dep appsv1.Deployment
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "otel-agc"}, &dep) == nil
	}, 15*time.Second, 100*time.Millisecond, "the gateway must provision its AGC")
	assert.Equal(t, "/etc/actions-gateway/tracing-ca/ca.crt", envValue(&dep, "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE"))

	var src *corev1.ConfigMapVolumeSource
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == "tracing-ca" {
			src = v.ConfigMap
		}
	}
	require.NotNil(t, src, "the AGC pod must mount the collector's CA bundle")
	assert.Equal(t, "collector-ca", src.Name)

	hasCollectorRule := func(np *networkingv1.NetworkPolicy) bool {
		for _, e := range np.Spec.Egress {
			if len(e.To) == 1 && e.To[0].IPBlock != nil && e.To[0].IPBlock.CIDR == "10.0.5.7/32" &&
				len(e.Ports) == 1 && e.Ports[0].Port.IntVal == 4317 {
				return true
			}
		}
		return false
	}
	var agcPolicy, workloadPolicy networkingv1.NetworkPolicy
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "otel-agc"}, &agcPolicy) == nil
	}, 15*time.Second, 100*time.Millisecond, "the gateway must provision its AGC NetworkPolicy")
	assert.True(t, hasCollectorRule(&agcPolicy), "the AGC policy must open the collector on the endpoint's port")

	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "otel-workload"}, &workloadPolicy) == nil
	}, 15*time.Second, 100*time.Millisecond, "the gateway must provision its workload NetworkPolicy")
	assert.False(t, hasCollectorRule(&workloadPolicy), "worker pods must not reach the collector")
}
