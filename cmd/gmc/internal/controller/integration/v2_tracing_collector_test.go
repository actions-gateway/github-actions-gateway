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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// TestV2_Tracing_EndpointAdmission runs each spelling through the real apiserver. The rejected
// ones are those the exporter sends to 127.0.0.1:4317 (otlptracegrpc v1.47.0, the table in
// Q1281's PR), and the accepted ones are what must not be refused: a scheme-less name dials
// that name, and the exporter trims surrounding whitespace.
func TestV2_Tracing_EndpointAdmission(t *testing.T) {
	const ns = "v2-tracing-endpoint"
	createNamespace(t, ns)

	for _, endpoint := range []string{
		"https://otel-collector.observability.svc.cluster.local:4317",
		"http://otel-collector.observability.svc.cluster.local:4317",
		"otel-collector.observability.svc.cluster.local:4317",
		"otel-collector.observability:4317",
		"otel-collector",
		"10.0.0.5",
		"https://10.0.0.5:4317",
		"https://_collector:4317",
		"http://127.0.0.1:4317",
		"otel-collector:4317#frag",
		"http://[::1]:4317",
		"http://[fe80::1%25eth0]:4317",
		"//otel-collector:4317",
		"dns:///otel-collector:4317",
		" http://otel-collector:4317",
		"http://otel-collector:4317\n",
		"otel-collector:4317 ",
		" otel-collector:4317",
		"otel-collector:4317\n",
		"http://otel-collector:4317 ",
		"http://otel-collector:4317/",
		"HTTP://otel-collector:4317",
		"http://user:pw@otel-collector:4317",
		// These parse and fail in the AGC's log rather than being misread.
		"https://c:70000",
		"c:70000",
		"unix:///var/run/otel.sock",
		"otel-collector:4317/v1/traces",
		"otel-collector:notaport",
		"grpc://otel-collector:4317",
		"ftp://otel-collector:4317",
		"coll ector",
		"otel-collector/v1",
	} {
		t.Run("accepts "+endpoint, func(t *testing.T) {
			ag := newV2GatewayWired("endpoint-ok", ns, "github-app", "")
			ag.Spec.Tracing = v2.TracingConfig{Endpoint: endpoint}
			require.NoError(t, k8sClient.Create(ctx, ag))
			require.NoError(t, k8sClient.Delete(context.Background(), ag))
			require.Eventually(t, func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: ag.Name}, &v2.ActionsGateway{}))
			}, 15*time.Second, 100*time.Millisecond)
		})
	}
	for _, endpoint := range []string{
		"10.0.0.5:4317",
		"[::1]:4317",
		":4317",
		" ",
		"\n",
		"http://local host:4317",
		"http://otel-collector:notaport",
		"http://otel-collector:4317/%zz",
		"a%zz",
		"_collector:4317",
		"my-collector_1.ns:4317",
		"//otel-collector:notaport",
		"//local host:4317",
		"//[::1",
		"//local host",
		// Over-rejected, because isURL does not split a fragment and CEL has no url.Parse: the
		// exporter delivers these. Nothing but a typo writes them.
		"http://otel-collector:4317#frag",
		"http://otel-collector:4317#",
		"HTTP://otel-collector:4317#f",
		"https://otel-collector:4317#frag",
		"//otel-collector:4317#frag",
		// Over-rejected and harmless: the exporter dials these and delivers nowhere.
		"otel-collector/v1:4317",
		"otel-collector/:4317",
		"coll?x=a:b",
		"%41",
		"a%20b",
		// Delivered to a collector on the AGC pod's own loopback, by the misread alone.
		// http://127.0.0.1:4317 says so on purpose, and is accepted.
		"127.0.0.1:4317",
		"3collector:4317",
	} {
		t.Run("rejects "+endpoint, func(t *testing.T) {
			ag := newV2GatewayWired("endpoint-bad", ns, "github-app", "")
			ag.Spec.Tracing = v2.TracingConfig{Endpoint: endpoint}
			err := k8sClient.Create(ctx, ag)
			if err == nil {
				t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })
			}
			require.Error(t, err)
			assert.True(t, apierrors.IsInvalid(err), "expected Invalid, got %v", err)
			assert.Contains(t, err.Error(), "endpoint does not parse as a URL")
		})
	}
}

// TestV2_Tracing_StoredBadEndpointKeepsWorking: a gateway stored before the rule keeps working,
// which only a stored object can show, so the CRD is edited to admit one. The rule passes an
// unchanged value itself, because ratcheting misses the status subresource on Kubernetes 1.31
// and 1.32, where a stored value froze the gateway's status (measured on 1.31.0 and 1.32.0;
// 1.33.0 ratchets it). This suite runs 1.35, so only the optionalOldSelf pin below can fail here.
func TestV2_Tracing_StoredBadEndpointKeepsWorking(t *testing.T) {
	const ns = "v2-tracing-ratchet"
	createNamespaceWithLabels(t, ns, map[string]string{
		v2.SecurityProfileLabel: v2.SecurityProfileRestricted,
	})
	createGitHubAppSecret(t, ns, "github-app")

	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
	crdKey := types.NamespacedName{Name: "actionsgateways.actions-gateway.com"}
	endpointRules := func(versions []any) (map[string]any, []any) {
		for _, v := range versions {
			version := v.(map[string]any)
			if version["name"] != "v2" {
				continue
			}
			field, found, err := unstructured.NestedMap(version, "schema", "openAPIV3Schema", "properties", "spec", "properties", "tracing", "properties", "endpoint")
			require.NoError(t, err)
			require.True(t, found)
			rules, _, _ := unstructured.NestedSlice(field, "x-kubernetes-validations")
			return version, rules
		}
		t.Fatal("no v2 version in the CRD")
		return nil, nil
	}
	setRules := func(rules []any) {
		require.Eventually(t, func() bool {
			if err := k8sClient.Get(ctx, crdKey, crd); err != nil {
				return false
			}
			versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
			for i, v := range versions {
				version := v.(map[string]any)
				if version["name"] != "v2" {
					continue
				}
				path := []string{"schema", "openAPIV3Schema", "properties", "spec", "properties", "tracing", "properties", "endpoint", "x-kubernetes-validations"}
				if len(rules) == 0 {
					unstructured.RemoveNestedField(version, path...)
				} else if err := unstructured.SetNestedSlice(version, rules, path...); err != nil {
					return false
				}
				versions[i] = version
			}
			if err := unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions"); err != nil {
				return false
			}
			return k8sClient.Update(ctx, crd) == nil
		}, 15*time.Second, 200*time.Millisecond, "update the CRD")
	}
	require.NoError(t, k8sClient.Get(ctx, crdKey, crd))
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	_, original := endpointRules(versions)
	require.Len(t, original, 1, "the rule this test exercises must exist")
	require.Equal(t, true, original[0].(map[string]any)["optionalOldSelf"], "an unchanged value must pass on the status path too")
	t.Cleanup(func() { setRules(original) })

	const bad = "10.0.0.5:4317"
	newGateway := func(name string) *v2.ActionsGateway {
		ag := newV2GatewayWired(name, ns, "github-app", "")
		ag.Spec.Tracing = v2.TracingConfig{Endpoint: bad}
		return ag
	}

	setRules(nil)
	stored := newGateway("stored")
	require.Eventually(t, func() bool { return k8sClient.Create(ctx, stored) == nil },
		15*time.Second, 200*time.Millisecond, "the CRD with no rule must admit the value")
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), stored) })
	reconciled := newGateway("reconciled")
	require.NoError(t, k8sClient.Create(ctx, reconciled))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), reconciled) })

	setRules(original)
	require.Eventually(t, func() bool {
		probe := newGateway("probe")
		err := k8sClient.Create(ctx, probe)
		if err == nil {
			_ = k8sClient.Delete(ctx, probe)
		}
		return apierrors.IsInvalid(err)
	}, 15*time.Second, 200*time.Millisecond, "the restored rule must reject a new object")

	update := func(edit func(*v2.ActionsGateway)) error {
		var got v2.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "stored"}, &got); err != nil {
			return err
		}
		edit(&got)
		return k8sClient.Update(ctx, &got)
	}
	require.NoError(t, update(func(*v2.ActionsGateway) {}), "re-applying it unchanged must succeed")
	require.NoError(t, update(func(ag *v2.ActionsGateway) { ag.Spec.LogLevel = "debug" }),
		"editing another field must succeed")
	require.NoError(t, update(func(ag *v2.ActionsGateway) { ag.Spec.Tracing.Sampler = "always_on" }),
		"editing a sibling tracing field must succeed")
	require.NoError(t, update(func(ag *v2.ActionsGateway) { ag.Finalizers = append(ag.Finalizers, "example.com/hold") }),
		"a metadata write, as the GMC makes, must succeed")
	require.NoError(t, update(func(ag *v2.ActionsGateway) { ag.Finalizers = nil }))
	var got v2.ActionsGateway
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "stored"}, &got))
	got.Status.Conditions = []metav1.Condition{{Type: "Probe", Status: metav1.ConditionTrue, Reason: "Probe", LastTransitionTime: metav1.Now()}}
	require.NoError(t, k8sClient.Status().Update(ctx, &got), "a status write, as the GMC makes, must succeed")

	err := update(func(ag *v2.ActionsGateway) { ag.Spec.Tracing.Endpoint = "10.0.0.6:4317" })
	require.Error(t, err, "changing it to another misread value must be rejected")
	assert.True(t, apierrors.IsInvalid(err), "expected Invalid, got %v", err)
	require.NoError(t, update(func(ag *v2.ActionsGateway) { ag.Spec.Tracing.Endpoint = "https://10.0.0.5:4317" }),
		"fixing it must succeed")

	startActionsGatewayV2Reconciler(t)
	require.Eventually(t, func() bool {
		var got v2.ActionsGateway
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "reconciled"}, &got) == nil && len(got.Status.Conditions) > 0
	}, 30*time.Second, 200*time.Millisecond, "the GMC must be able to write status on a stored value")
}

// TestV2_Tracing_MissingBundleIsAdvisory: a ref naming no ConfigMap sets the advisory
// condition and still provisions the AGC, and applying the ConfigMap clears it with no edit
// to the gateway.
func TestV2_Tracing_MissingBundleIsAdvisory(t *testing.T) {
	const ns = "v2-tracing-missing-ca"
	createNamespaceWithLabels(t, ns, map[string]string{
		v2.SecurityProfileLabel: v2.SecurityProfileRestricted,
	})
	createGitHubAppSecret(t, ns, "github-app")

	ag := newV2GatewayWired("otel", ns, "github-app", "")
	ag.Spec.Tracing = v2.TracingConfig{
		Endpoint:    "https://collector.observability.svc.cluster.local:4317",
		CABundleRef: &v2.LocalConfigMapReference{Name: "collector-ca"},
	}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startActionsGatewayV2Reconciler(t)

	unresolved := func() *metav1.Condition {
		var got v2.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "otel"}, &got); err != nil {
			return nil
		}
		return findCondition(got.Status.Conditions, v2.ConditionTracingCABundleUnresolved)
	}
	require.Eventually(t, func() bool {
		c := unresolved()
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == v2.ReasonCABundleNotFound
	}, 15*time.Second, 100*time.Millisecond, "a missing bundle must report")

	var dep appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "otel-agc"}, &dep),
		"the AGC must be provisioned without the bundle")

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "collector-ca", Namespace: ns},
		Data:       map[string]string{"ca.crt": testCABundle(t)},
	}
	require.NoError(t, k8sClient.Create(ctx, cm))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cm) })
	require.Eventually(t, func() bool {
		c := unresolved()
		return c != nil && c.Status == metav1.ConditionFalse
	}, 30*time.Second, 200*time.Millisecond, "applying the ConfigMap must clear the condition")
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
