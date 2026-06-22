/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func actionsGatewayV2TestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, appsv1.AddToScheme(s))
	require.NoError(t, rbacv1.AddToScheme(s))
	require.NoError(t, networkingv1.AddToScheme(s))
	require.NoError(t, gmcv2alpha1.AddToScheme(s))
	return s
}

func v2Gateway(name, ns, secret, proxyRef string) *gmcv2alpha1.ActionsGateway {
	ag := &gmcv2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gmcv2alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv2alpha1.LocalSecretReference{Name: secret},
			GitHubURL:    "https://github.com/example-org",
		},
	}
	if proxyRef != "" {
		ag.Spec.DefaultProxyRef = &gmcv2alpha1.LocalObjectRef{Name: proxyRef}
	}
	return ag
}

// reconcileV2Gateway runs the reconciler twice (the first pass adds the finalizer
// and requeues; the second provisions), returning the last result.
func reconcileV2Gateway(t *testing.T, r *ActionsGatewayV2Reconciler, ns, name string) ctrl.Result {
	t.Helper()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	res, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	return res
}

func TestActionsGatewayV2Reconcile_ProvisionsControlPlane(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "team-a",
		Labels: map[string]string{gmcv2alpha1.SecurityProfileLabel: gmcv2alpha1.SecurityProfileRestricted},
	}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-app", Namespace: "team-a"}}
	proxy := &gmcv2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "team-a"}}
	ag := v2Gateway("gw", "team-a", "github-app", "shared")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ns, secret, proxy, ag).
		WithStatusSubresource(ag).
		Build()

	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test"}
	res := reconcileV2Gateway(t, r, "team-a", "gw")
	assert.Positive(t, res.RequeueAfter, "no kubelet ⇒ AGC not ready ⇒ requeue")

	ctx := context.Background()

	// AGC Deployment: per-gateway name, created, owned, egress + security-profile
	// wired, no creds in env. GATEWAY_NAME is threaded so the AGC scopes its
	// RunnerSet watch to this gateway (§H.16 #1).
	var dep appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: agcNameV2(ag)}, &dep))
	require.True(t, isControllerOwnedByGateway(dep.OwnerReferences, "gw"))
	env := agcEnv(&dep)
	assert.Equal(t, "https://shared-proxy.team-a.svc.cluster.local:8080", env["HTTPS_PROXY"])
	assert.Equal(t, "shared-proxy-tls", env["PROXY_TLS_SECRET_NAME"])
	assert.Equal(t, gmcv2alpha1.SecurityProfileRestricted, env["SECURITY_PROFILE"])
	assert.Equal(t, "gw", env["GATEWAY_NAME"], "AGC must be scoped to its gateway")
	assert.Equal(t, workerSANameV2(ag), env["WORKER_SERVICE_ACCOUNT"], "per-gateway worker SA")
	for _, k := range []string{"appId", "privateKey", "installationId"} {
		_, present := env[k]
		assert.False(t, present, "credential %q must never be an env var", k)
	}

	// Companion children, each per-gateway-named and owned.
	var sa corev1.ServiceAccount
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: agcNameV2(ag)}, &sa))
	assert.True(t, isControllerOwnedByGateway(sa.OwnerReferences, "gw"))
	var workerSA corev1.ServiceAccount
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: workerSANameV2(ag)}, &workerSA))
	assert.True(t, isControllerOwnedByGateway(workerSA.OwnerReferences, "gw"))
	var rb rbacv1.RoleBinding
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: agcNameV2(ag)}, &rb))
	assert.Equal(t, agcTenantRoleName, rb.RoleRef.Name)
	var wnp networkingv1.NetworkPolicy
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: workloadNPNameV2(ag)}, &wnp))
	assert.Empty(t, wnp.Spec.Ingress, "workload NetworkPolicy default-denies ingress")
	for _, name := range []string{metricsTLSSecretNameV2(ag), metricsClientSecretNameV2(ag)} {
		var sec corev1.Secret
		require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: name}, &sec))
		assert.True(t, isControllerOwnedByGateway(sec.OwnerReferences, "gw"))
	}

	// Per-gateway ClusterRoleBinding granting the AGC SA cluster-scoped read of
	// ClusterRunnerTemplate (M3b). Cluster-scoped, so it carries no owner ref (the
	// reconciler deletes it explicitly on gateway deletion).
	var crb rbacv1.ClusterRoleBinding
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: clusterRunnerTemplateReaderBindingName(ag)}, &crb))
	assert.Equal(t, clusterRunnerTemplateReaderRole, crb.RoleRef.Name)
	require.Len(t, crb.Subjects, 1)
	assert.Equal(t, agcNameV2(ag), crb.Subjects[0].Name)
	assert.Equal(t, "team-a", crb.Subjects[0].Namespace)

	// Status: Ready=False (no kubelet) / Degraded=False / CredentialUnavailable=False.
	var got gmcv2alpha1.ActionsGateway
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "gw"}, &got))
	assert.Equal(t, metav1.ConditionFalse, condStatus(got.Status.Conditions, gmcv2alpha1.ConditionReady))
	assert.Equal(t, metav1.ConditionFalse, condStatus(got.Status.Conditions, gmcv2alpha1.ConditionDegraded))
	assert.Equal(t, metav1.ConditionFalse, condStatus(got.Status.Conditions, gmcv2alpha1.ConditionCredentialUnavailable))
	assert.Equal(t, got.Generation, got.Status.ObservedGeneration)
}

func TestActionsGatewayV2Reconcile_FailsClosedWithoutCredential(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	proxy := &gmcv2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "team-a"}}
	ag := v2Gateway("gw", "team-a", "missing", "shared")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, proxy, ag).WithStatusSubresource(ag).Build()

	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test"}
	reconcileV2Gateway(t, r, "team-a", "gw")

	var got gmcv2alpha1.ActionsGateway
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "gw"}, &got))
	cred := meta.FindStatusCondition(got.Status.Conditions, gmcv2alpha1.ConditionCredentialUnavailable)
	require.NotNil(t, cred)
	assert.Equal(t, metav1.ConditionTrue, cred.Status)
	assert.Equal(t, gmcv2alpha1.ReasonSecretNotFound, cred.Reason)
	// Fail closed: no AGC Deployment.
	var dep appsv1.Deployment
	assert.Error(t, c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: agcNameV2(ag)}, &dep))
}

func TestActionsGatewayV2Reconcile_FailsClosedWithoutProxy(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-app", Namespace: "team-a"}}

	// (a) defaultProxyRef unset.
	agNoRef := v2Gateway("gw-noref", "team-a", "github-app", "")
	// (b) defaultProxyRef names an absent EgressProxy.
	agAbsent := v2Gateway("gw-absent", "team-a", "github-app", "absent")
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ns, secret, agNoRef, agAbsent).
		WithStatusSubresource(agNoRef, agAbsent).Build()

	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test"}
	for _, name := range []string{"gw-noref", "gw-absent"} {
		reconcileV2Gateway(t, r, "team-a", name)
		var got gmcv2alpha1.ActionsGateway
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: name}, &got))
		ready := meta.FindStatusCondition(got.Status.Conditions, gmcv2alpha1.ConditionReady)
		require.NotNil(t, ready, "%s", name)
		assert.Equal(t, metav1.ConditionFalse, ready.Status, "%s", name)
		assert.Equal(t, gmcv2alpha1.ReasonProxyNotFound, ready.Reason, "%s", name)
	}
}

func TestActionsGatewayV2Reconcile_RemovesFinalizerOnDelete(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	now := metav1.Now()
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	ag.Finalizers = []string{gmcv2alpha1.ActionsGatewayFinalizer}
	ag.DeletionTimestamp = &now
	// The cluster-scoped ClusterRoleBinding is not owner-GC'd, so reconcileDelete
	// must remove it explicitly; seed it to prove the cleanup.
	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: clusterRunnerTemplateReaderBindingName(ag)}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag, crb).WithStatusSubresource(ag).Build()

	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test"}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "gw"}})
	require.NoError(t, err)
	// Finalizer removed ⇒ the object is fully deleted from the fake client.
	var got gmcv2alpha1.ActionsGateway
	assert.Error(t, c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "gw"}, &got))
	// The per-gateway ClusterRoleBinding is explicitly deleted.
	var gotCRB rbacv1.ClusterRoleBinding
	assert.Error(t, c.Get(context.Background(), types.NamespacedName{Name: clusterRunnerTemplateReaderBindingName(ag)}, &gotCRB),
		"reconcileDelete must delete the cluster-scoped ClusterRoleBinding")
}

// TestActionsGatewayV2_PerGatewayNaming proves two gateways in one namespace
// derive disjoint child names so they never collide on a fixed name (§H.16 #1).
func TestActionsGatewayV2_PerGatewayNaming(t *testing.T) {
	a := v2Gateway("alpha", "team-a", "github-app", "shared")
	b := v2Gateway("beta", "team-a", "github-app", "shared")

	assert.NotEqual(t, agcNameV2(a), agcNameV2(b))
	assert.Equal(t, "alpha-agc", agcNameV2(a))
	assert.Equal(t, "beta-agc", agcNameV2(b))
	assert.NotEqual(t, workerSANameV2(a), workerSANameV2(b))
	assert.NotEqual(t, workloadNPNameV2(a), workloadNPNameV2(b))
	assert.NotEqual(t, metricsTLSSecretNameV2(a), metricsTLSSecretNameV2(b))
	assert.NotEqual(t, clusterRunnerTemplateReaderBindingName(a), clusterRunnerTemplateReaderBindingName(b))

	// The AGC pod `app` label value (= agcNameV2) must stay within the 63-char RFC
	// 1123 label-value ceiling even at the 52-char CR-name cap (§H.6).
	longest := v2Gateway("a23456789012345678901234567890123456789012345678901", "team-a", "github-app", "shared")
	require.Len(t, longest.Name, 51)
	assert.LessOrEqual(t, len(agcNameV2(longest)), 63, "AGC app label value must stay ≤ 63 chars")
}

// --- builder unit tests ---

func TestBuildAGCNetworkPolicyV2_ApiserverEgressScoping(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	// Default: any-destination apiserver egress.
	np := buildAGCNetworkPolicyV2(ag, nil)
	assert.Equal(t, map[string]string{"app": agcNameV2(ag)}, np.Spec.PodSelector.MatchLabels)
	apiRule := findApiserverEgressRule(np)
	require.NotNil(t, apiRule)
	assert.Empty(t, apiRule.To, "empty CIDR list keeps any-destination apiserver egress")

	// Scoped: the rule gains ipBlock peers.
	scoped := buildAGCNetworkPolicyV2(ag, []string{"10.0.0.0/24"})
	apiRule = findApiserverEgressRule(scoped)
	require.NotNil(t, apiRule)
	require.Len(t, apiRule.To, 1)
	require.NotNil(t, apiRule.To[0].IPBlock)
	assert.Equal(t, "10.0.0.0/24", apiRule.To[0].IPBlock.CIDR)
}

func TestBuildAGCDeploymentV2_TracingAndNoProxyWiring(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	ag.Spec.Tracing = gmcv2alpha1.TracingConfig{Endpoint: "otel:4317", Sampler: "always_on"}
	proxyNoProxy := []string{"10.20.0.0/16"}
	dep := buildAGCDeploymentV2(ag, "agc:test", "shared", gmcv2alpha1.SecurityProfileBaseline, proxyNoProxy, nil)
	env := agcEnv(dep)
	assert.Equal(t, "otel:4317", env["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"])
	assert.Equal(t, "always_on", env["OTEL_TRACES_SAMPLER"])
	// NO_PROXY merges the EgressProxy's CIDRs with the cluster-internal exclusions.
	assert.Contains(t, env["NO_PROXY"], "10.20.0.0/16")
	assert.Contains(t, env["NO_PROXY"], "svc.cluster.local")
}

func TestGenerateMetricsCertsV2_ParsesAndCoversAGCService(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	b, err := generateMetricsCertsV2("team-a", agcNameV2(ag))
	require.NoError(t, err)
	cert, err := parseCertPEM(b.serverCertPEM)
	require.NoError(t, err)
	// Cert SANs cover this gateway's per-gateway AGC Service name.
	assert.Contains(t, cert.DNSNames, agcNameV2(ag)+".team-a.svc")
	// CA + both leaves are present.
	assert.NotEmpty(t, b.caPEM)
	assert.NotEmpty(t, b.clientCertPEM)
}

// --- helpers ---

func isControllerOwnedByGateway(refs []metav1.OwnerReference, name string) bool {
	for _, r := range refs {
		if r.Kind == "ActionsGateway" && r.Name == name && r.Controller != nil && *r.Controller {
			return true
		}
	}
	return false
}

func agcEnv(dep *appsv1.Deployment) map[string]string {
	out := map[string]string{}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		out[e.Name] = e.Value
	}
	return out
}

func condStatus(conds []metav1.Condition, t string) metav1.ConditionStatus {
	if c := meta.FindStatusCondition(conds, t); c != nil {
		return c.Status
	}
	return ""
}

func findApiserverEgressRule(np *networkingv1.NetworkPolicy) *networkingv1.NetworkPolicyEgressRule {
	for i := range np.Spec.Egress {
		for _, p := range np.Spec.Egress[i].Ports {
			if p.Port != nil && p.Port.IntVal == 443 {
				return &np.Spec.Egress[i]
			}
		}
	}
	return nil
}
