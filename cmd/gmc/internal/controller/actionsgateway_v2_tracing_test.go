package controller

import (
	"context"
	"testing"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
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

const tracingTestEndpoint = "https://otel-collector.observability.svc.cluster.local:4317"

func collectorPeer() *gmcv2.EgressPeer {
	return &gmcv2.EgressPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: "observability"}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "otel-collector"}},
	}
}

func TestTracingEgressPort(t *testing.T) {
	for endpoint, want := range map[string]int32{
		"https://collector:4317":      4317,
		"http://collector:4318":       4318,
		"https://collector":           443,
		"http://collector":            443,
		"collector.observability:55":  55,
		"10.0.0.5:4317":               4317,
		"https://[fd00::1]:4317":      4317,
		"not a url at all":            443,
		"https://collector:notaport":  443,
		"unix:///var/run/otel.socket": 443,
	} {
		assert.Equal(t, want, tracingEgressPort(endpoint), endpoint)
	}
}

func TestTracingEgressRule(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "shared")

	_, ok := tracingEgressRule(ag)
	assert.False(t, ok, "no tracing block opens nothing")

	ag.Spec.Tracing = gmcv2.TracingConfig{Endpoint: tracingTestEndpoint}
	_, ok = tracingEgressRule(ag)
	assert.False(t, ok, "an endpoint alone opens nothing: the GMC never derives a peer from it")

	ag.Spec.Tracing.NetworkPolicy = collectorPeer()
	rule, ok := tracingEgressRule(ag)
	require.True(t, ok)
	require.Len(t, rule.Ports, 1)
	assert.Equal(t, 4317, rule.Ports[0].Port.IntValue(), "the port comes from the endpoint")
	require.Len(t, rule.To, 1)
	assert.Equal(t, ag.Spec.Tracing.NetworkPolicy.PodSelector, rule.To[0].PodSelector)
	assert.Equal(t, ag.Spec.Tracing.NetworkPolicy.NamespaceSelector, rule.To[0].NamespaceSelector,
		"one peer carrying both selectors ANDs them; two peers would reach any pod in the namespace")

	ag.Spec.Tracing.NetworkPolicy = &gmcv2.EgressPeer{CIDR: "10.0.5.7/32", Port: ptr(int32(14317))}
	rule, ok = tracingEgressRule(ag)
	require.True(t, ok)
	assert.Equal(t, 14317, rule.Ports[0].Port.IntValue(), "an explicit port wins over the endpoint's")
	assert.Equal(t, "10.0.5.7/32", rule.To[0].IPBlock.CIDR)
}

// The rule lands on the AGC's policy and never on the workload policy, whose pods run
// untrusted job code.
func TestTracingPeer_OpensTheAGCPolicyOnly(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	before := buildAGCNetworkPolicyV2(ag, nil, nil, false, nil).Spec.Egress
	workloadBefore := buildWorkloadNetworkPolicyV2(ag, nil, false, nil).Spec.Egress

	ag.Spec.Tracing = gmcv2.TracingConfig{Endpoint: tracingTestEndpoint, NetworkPolicy: collectorPeer()}
	want, ok := tracingEgressRule(ag)
	require.True(t, ok)

	assert.Equal(t, append(before, want), buildAGCNetworkPolicyV2(ag, nil, nil, false, nil).Spec.Egress)
	assert.Equal(t, workloadBefore, buildWorkloadNetworkPolicyV2(ag, nil, false, nil).Spec.Egress)
}

// A CIDR collector on 443 has the exact shape of the GitHub rule, so reading the policy
// must not take it for one.
func TestHasGitHubCIDREgressRule_IgnoresACollectorOn443(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	ag.Spec.Tracing = gmcv2.TracingConfig{
		Endpoint:      "https://collector.example.com",
		NetworkPolicy: &gmcv2.EgressPeer{CIDR: "10.0.5.7/32"},
	}
	named := operatorEgressRules(ag)
	require.Len(t, named, 1)
	assert.True(t, hasGitHubCIDREgressRule(named, nil), "the shape is GitHub's until the rule is named")
	assert.False(t, hasGitHubCIDREgressRule(named, named))
}

func tracingCAVolume(dep *appsv1.Deployment) *corev1.Volume {
	for i, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == tracingCAVolumeName {
			return &dep.Spec.Template.Spec.Volumes[i]
		}
	}
	return nil
}

func TestBuildAGCDeploymentV2_TracingCABundle(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	ag.Spec.Tracing = gmcv2.TracingConfig{Endpoint: tracingTestEndpoint}
	dep := proxiedAGCDeploymentV2(ag, nil)
	assert.Nil(t, tracingCAVolume(dep), "no ref, no mount")
	assert.NotContains(t, agcEnv(dep), "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE")

	ag.Spec.Tracing.CABundleRef = &gmcv2.LocalConfigMapReference{Name: "collector-ca"}
	dep = proxiedAGCDeploymentV2(ag, nil)

	vol := tracingCAVolume(dep)
	require.NotNil(t, vol)
	require.NotNil(t, vol.ConfigMap, "a certificate is public material, so the carrier is a ConfigMap")
	assert.Equal(t, "collector-ca", vol.ConfigMap.Name)
	assert.Equal(t, []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}, vol.ConfigMap.Items)

	container := dep.Spec.Template.Spec.Containers[0]
	assert.Contains(t, container.VolumeMounts, corev1.VolumeMount{
		Name: tracingCAVolumeName, MountPath: tracingCAMountPath, ReadOnly: true,
	})
	assert.Equal(t, tracingCAMountPath+"/ca.crt", agcEnv(dep)["OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE"])
	assert.NotContains(t, agcEnv(dep), "GITHUB_CA_CONFIGMAP_NAME",
		"the collector's bundle must not join the trust the AGC uses for GitHub")
}

func tracingCATestObjects(t *testing.T, cm *corev1.ConfigMap) (*gmcv2.ActionsGateway, []client.Object) {
	t.Helper()
	ag := v2Gateway("gw", "team-a", "github-app", "")
	ag.Spec.Tracing = gmcv2.TracingConfig{
		Endpoint:    tracingTestEndpoint,
		CABundleRef: &gmcv2.LocalConfigMapReference{Name: "collector-ca"},
	}
	objs := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-app", Namespace: "team-a"}},
		ag,
	}
	if cm != nil {
		objs = append(objs, cm)
	}
	return ag, objs
}

func TestActionsGatewayV2Reconcile_TracingCABundleFailsClosed(t *testing.T) {
	bundle := func(data map[string]string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "collector-ca", Namespace: "team-a"}, Data: data}
	}
	for name, tc := range map[string]struct {
		cm     *corev1.ConfigMap
		reason string
	}{
		"missing":   {nil, gmcv2.ReasonCABundleNotFound},
		"no key":    {bundle(map[string]string{"other": "x"}), gmcv2.ReasonCABundleInvalid},
		"not a PEM": {bundle(map[string]string{"ca.crt": "not a certificate"}), gmcv2.ReasonCABundleInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			scheme := actionsGatewayV2TestScheme(t)
			ag, objs := tracingCATestObjects(t, tc.cm)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(ag).Build()
			r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test"}

			res := reconcileV2Gateway(t, r, "team-a", "gw")
			assert.Equal(t, caBundleReprobeInterval, res.RequeueAfter)

			degraded := meta.FindStatusCondition(gatewayConditions(t, c, ag), gmcv2.ConditionDegraded)
			require.NotNil(t, degraded)
			assert.Equal(t, tc.reason, degraded.Reason)
			assert.Contains(t, degraded.Message, "tracing.caBundleRef", "the message names which of two refs failed")

			var dep appsv1.Deployment
			err := c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: agcNameV2(ag)}, &dep)
			assert.True(t, apierrors.IsNotFound(err), "a mount naming an absent ConfigMap would wedge the pod, so no AGC is provisioned")
		})
	}
}

func TestActionsGatewayV2Reconcile_TracingCABundleResolves(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ag, objs := tracingCATestObjects(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "collector-ca", Namespace: "team-a"},
		Data:       map[string]string{"ca.crt": caBundlePEM(t)},
	})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(ag).Build()
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test"}
	reconcileV2Gateway(t, r, "team-a", "gw")

	var dep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: agcNameV2(ag)}, &dep))
	assert.NotNil(t, tracingCAVolume(&dep))
}
