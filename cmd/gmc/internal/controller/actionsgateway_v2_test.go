package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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
			Credentials: gmcv2alpha1.GitHubCredentials{
				Type:      gmcv2alpha1.CredentialTypeGitHubApp,
				GitHubApp: &gmcv2alpha1.LocalSecretReference{Name: secret},
			},
			GitHubURL: "https://github.com/example-org",
		},
	}
	if proxyRef != "" {
		ag.Spec.DefaultProxyRef = &gmcv2alpha1.ProxyObjectRef{Name: proxyRef}
	}
	return ag
}

// v2WorkloadIdentityGateway is a fixture for the no-PEM delegation method (Q201):
// no GitHub App Secret, App identity + Vault transit signer inline. proxyRef ""
// leaves egress direct.
func v2WorkloadIdentityGateway(name, ns, proxyRef string) *gmcv2alpha1.ActionsGateway {
	ag := &gmcv2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gmcv2alpha1.ActionsGatewaySpec{
			Credentials: gmcv2alpha1.GitHubCredentials{
				Type: gmcv2alpha1.CredentialTypeWorkloadIdentity,
				WorkloadIdentity: &gmcv2alpha1.WorkloadIdentity{
					AppID:          424242,
					InstallationID: 99,
					Signer: gmcv2alpha1.ExternalSigner{
						Provider: gmcv2alpha1.SignerProviderVault,
						Vault: &gmcv2alpha1.VaultSigner{
							Address: "https://vault.vault.svc:8200",
							KeyName: "agc",
							Auth:    gmcv2alpha1.VaultKubernetesAuth{Role: "agc"},
						},
					},
				},
			},
			GitHubURL: "https://github.com/example-org",
		},
	}
	if proxyRef != "" {
		ag.Spec.DefaultProxyRef = &gmcv2alpha1.ProxyObjectRef{Name: proxyRef}
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
	// Proxied: proxyMode Proxied, EgressUnattributed=False (Q168).
	assert.Equal(t, gmcv2alpha1.ProxyModeProxied, got.Status.ProxyMode)
	assert.Equal(t, metav1.ConditionFalse, condStatus(got.Status.Conditions, gmcv2alpha1.ConditionEgressUnattributed))
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

// TestActionsGatewayV2Reconcile_FailsClosedWithAbsentProxyRef: a defaultProxyRef
// that names a *missing* EgressProxy is an operator error and still fails closed
// (ProxyNotFound, no AGC) — it must not silently fall back to direct egress (Q168).
func TestActionsGatewayV2Reconcile_FailsClosedWithAbsentProxyRef(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-app", Namespace: "team-a"}}
	agAbsent := v2Gateway("gw-absent", "team-a", "github-app", "absent")
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ns, secret, agAbsent).
		WithStatusSubresource(agAbsent).Build()

	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test"}
	reconcileV2Gateway(t, r, "team-a", "gw-absent")
	var got gmcv2alpha1.ActionsGateway
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "gw-absent"}, &got))
	ready := meta.FindStatusCondition(got.Status.Conditions, gmcv2alpha1.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, gmcv2alpha1.ReasonProxyNotFound, ready.Reason)
	// Fail closed: no AGC Deployment.
	var dep appsv1.Deployment
	assert.Error(t, c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: agcNameV2(agAbsent)}, &dep))
}

// TestActionsGatewayV2Reconcile_DirectEgressWhenNoProxyRef: a gateway with no
// defaultProxyRef egresses directly (Q168, §H.10) — it provisions the AGC control
// plane (no proxy env), reports proxyMode Direct + EgressUnattributed=True, and is
// NOT Degraded/ProxyNotFound. Its AGC + workload NetworkPolicies carry the GitHub
// allowlist from the IP cache; restriction is preserved.
func TestActionsGatewayV2Reconcile_DirectEgressWhenNoProxyRef(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-app", Namespace: "team-a"}}
	ag := v2Gateway("gw", "team-a", "github-app", "") // no defaultProxyRef ⇒ direct
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ns, secret, ag).WithStatusSubresource(ag).Build()

	_, cidr, err := net.ParseCIDR("140.82.112.0/20")
	require.NoError(t, err)
	cache := &IPRangeCache{}
	cache.Set([]net.IPNet{*cidr})

	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test", IPCache: cache}
	reconcileV2Gateway(t, r, "team-a", "gw")
	ctx := context.Background()

	// AGC Deployment provisioned with no proxy env (direct egress).
	var dep appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: agcNameV2(ag)}, &dep))
	env := agcEnv(&dep)
	assert.NotContains(t, env, "HTTPS_PROXY", "direct egress: AGC has no proxy env")
	assert.NotContains(t, env, "PROXY_TLS_SECRET_NAME")

	// AGC + workload NetworkPolicies carry the GitHub CIDR allowlist (restriction).
	var anp, wnp networkingv1.NetworkPolicy
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: agcNameV2(ag)}, &anp))
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: workloadNPNameV2(ag)}, &wnp))
	assert.True(t, hasGitHubCIDREgress(&anp, "140.82.112.0/20"), "direct AGC NP allows GitHub")
	assert.True(t, hasGitHubCIDREgress(&wnp, "140.82.112.0/20"), "direct workload NP allows GitHub")
	assert.NotNil(t, findApiserverEgressRule(&anp), "AGC NP keeps apiserver egress")

	// Status: proxyMode Direct, EgressUnattributed=True, not Degraded.
	var got gmcv2alpha1.ActionsGateway
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "gw"}, &got))
	assert.Equal(t, gmcv2alpha1.ProxyModeDirect, got.Status.ProxyMode)
	unattr := meta.FindStatusCondition(got.Status.Conditions, gmcv2alpha1.ConditionEgressUnattributed)
	require.NotNil(t, unattr)
	assert.Equal(t, metav1.ConditionTrue, unattr.Status)
	assert.Equal(t, gmcv2alpha1.ReasonDirectEgress, unattr.Reason)
	assert.Equal(t, metav1.ConditionFalse, condStatus(got.Status.Conditions, gmcv2alpha1.ConditionDegraded))
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

// deletingV2Gateway returns a v2 ActionsGateway that is mid-deletion: it carries
// the cleanup finalizer and a deletion timestamp, the precondition for
// reconcileDelete (mirrors v1's deletingAG).
func deletingV2Gateway() *gmcv2alpha1.ActionsGateway {
	now := metav1.Now()
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	ag.Finalizers = []string{gmcv2alpha1.ActionsGatewayFinalizer}
	ag.DeletionTimestamp = &now
	return ag
}

// TestActionsGatewayV2ReconcileDelete_FailClosedOnDeleteError verifies the Q125
// fail-closed teardown ported to v2 (Q328): when a teardown delete returns a
// non-NotFound error, reconcileDelete must return an error (so the work requeues),
// must NOT remove the finalizer, and must emit a TeardownIncomplete event —
// otherwise a transient API failure would orphan a live, credentialed AGC
// Deployment.
func TestActionsGatewayV2ReconcileDelete_FailClosedOnDeleteError(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ag := deletingV2Gateway()
	agcDep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: agcNameV2(ag), Namespace: ag.Namespace}}

	deleteErr := errors.New("apiserver unavailable")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag, agcDep).WithStatusSubresource(ag).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				// Fail only the AGC Deployment delete; everything else passes through
				// so the test exercises the "some deletes succeed, one fails" path.
				if dep, ok := obj.(*appsv1.Deployment); ok && dep.GetName() == agcNameV2(ag) {
					return deleteErr
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()
	rec := events.NewFakeRecorder(8)
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test", Recorder: rec}

	_, err := r.reconcileDelete(context.Background(), ag)
	require.Error(t, err, "a failed teardown delete must surface an error to requeue")

	var fetched gmcv2alpha1.ActionsGateway
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: ag.Name}, &fetched))
	assert.Contains(t, fetched.Finalizers, gmcv2alpha1.ActionsGatewayFinalizer,
		"finalizer must be retained while a delete is unconfirmed")

	// The orphan-prevention invariant: the AGC Deployment that failed to delete is
	// still present (not abandoned silently).
	var dep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: agcNameV2(ag)}, &dep),
		"the AGC Deployment must still exist after a failed delete")

	select {
	case msg := <-rec.Events:
		assert.Contains(t, msg, "TeardownIncomplete", "the event must carry the TeardownIncomplete reason")
	default:
		t.Fatal("expected a TeardownIncomplete Warning event on the ActionsGateway")
	}
}

// TestActionsGatewayV2ReconcileDelete_RetainsFinalizerWhileChildLingers verifies
// the verify-gone half of the Q328 teardown: a child whose delete was accepted but
// that lingers (held by a foreign finalizer) keeps the gateway's finalizer in place
// with a TeardownIncomplete event and a poll requeue; once the child drains, the
// next pass removes the finalizer.
func TestActionsGatewayV2ReconcileDelete_RetainsFinalizerWhileChildLingers(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ag := deletingV2Gateway()
	held := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:       agcNameV2(ag),
		Namespace:  ag.Namespace,
		Finalizers: []string{"example.com/hold"},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ag, held).WithStatusSubresource(ag).Build()
	rec := events.NewFakeRecorder(8)
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test", Recorder: rec}

	res, err := r.reconcileDelete(context.Background(), ag)
	require.NoError(t, err, "a lingering child is a wait, not an error")
	assert.Positive(t, res.RequeueAfter, "teardown must poll until the lingering child is gone")

	var fetched gmcv2alpha1.ActionsGateway
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: ag.Name}, &fetched))
	assert.Contains(t, fetched.Finalizers, gmcv2alpha1.ActionsGatewayFinalizer,
		"finalizer must be retained while a child lingers")

	select {
	case msg := <-rec.Events:
		assert.Contains(t, msg, "TeardownIncomplete")
		assert.Contains(t, msg, "Deployment "+agcNameV2(ag), "the event must name the lingering child")
	default:
		t.Fatal("expected a TeardownIncomplete Warning event naming the lingering child")
	}

	// The child drains (its holder removes the finalizer) → the next pass confirms
	// every child gone and removes the gateway's finalizer.
	var dep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: agcNameV2(ag)}, &dep))
	dep.Finalizers = nil
	require.NoError(t, c.Update(context.Background(), &dep))

	_, err = r.reconcileDelete(context.Background(), ag)
	require.NoError(t, err)
	getErr := c.Get(context.Background(), types.NamespacedName{Namespace: ag.Namespace, Name: ag.Name}, &fetched)
	assert.Error(t, getErr, "the gateway must finalize away once every child is verifiably gone")
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
	// Default: any-destination apiserver egress (proxied mode, no GitHub rule).
	np := buildAGCNetworkPolicyV2(ag, nil, nil, false, nil)
	assert.Equal(t, map[string]string{"app": agcNameV2(ag)}, np.Spec.PodSelector.MatchLabels)
	apiRule := findApiserverEgressRule(np)
	require.NotNil(t, apiRule)
	assert.Empty(t, apiRule.To, "empty CIDR list keeps any-destination apiserver egress")

	// Scoped: the rule gains ipBlock peers.
	scoped := buildAGCNetworkPolicyV2(ag, []string{"10.0.0.0/24"}, nil, false, nil)
	apiRule = findApiserverEgressRule(scoped)
	require.NotNil(t, apiRule)
	require.Len(t, apiRule.To, 1)
	require.NotNil(t, apiRule.To[0].IPBlock)
	assert.Equal(t, "10.0.0.0/24", apiRule.To[0].IPBlock.CIDR)
}

// TestBuildAGCNetworkPolicyV2_DirectEgressGitHubRule: in direct mode the AGC policy
// additively permits the GitHub CIDRs on 443 (so the AGC reaches GitHub directly),
// keeps the apiserver egress, and omits the GitHub rule when the cache is empty
// (fail-closed) — never opening egress wide (Q168, §H.10).
func TestBuildAGCNetworkPolicyV2_DirectEgressGitHubRule(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	_, cidr, err := net.ParseCIDR("140.82.112.0/20")
	require.NoError(t, err)

	direct := buildAGCNetworkPolicyV2(ag, nil, []net.IPNet{*cidr}, true, nil)
	require.NotNil(t, findApiserverEgressRule(direct), "apiserver egress stays mandatory in direct mode")
	assert.True(t, hasGitHubCIDREgress(direct, "140.82.112.0/20"), "direct mode permits the GitHub CIDR on 443")

	// Empty cache ⇒ no GitHub rule (fail-closed until the refresh patches it).
	directEmpty := buildAGCNetworkPolicyV2(ag, nil, nil, true, nil)
	assert.False(t, hasGitHubCIDREgress(directEmpty, "140.82.112.0/20"))

	// Proxied mode never carries a GitHub rule even if CIDRs are supplied.
	proxied := buildAGCNetworkPolicyV2(ag, nil, []net.IPNet{*cidr}, false, nil)
	assert.False(t, hasGitHubCIDREgress(proxied, "140.82.112.0/20"))
}

// TestBuildAGCNetworkPolicyV2_VaultEgress: a workload-identity gateway whose Vault
// signer declares a NetworkPolicy peer gains a scoped AGC→Vault egress rule, in both
// CIDR (external) and selector (in-cluster) form, on the Vault API port parsed from the
// address; a workload-identity gateway without the peer, and any possession-model
// (githubApp) gateway, keep default-deny (no Vault rule) (Q202).
func TestBuildAGCNetworkPolicyV2_VaultEgress(t *testing.T) {
	// CIDR form (external Vault): scoped ipBlock peer on the address port (8200).
	wiCIDR := v2WorkloadIdentityGateway("gw", "team-a", "")
	wiCIDR.Spec.Credentials.WorkloadIdentity.Signer.Vault.NetworkPolicy = &gmcv2alpha1.EgressPeer{
		CIDR: "10.0.5.7/32",
	}
	np := buildAGCNetworkPolicyV2(wiCIDR, nil, nil, true, nil)
	rule := findVaultEgressRule(np)
	require.NotNil(t, rule, "CIDR-form WI gateway gains a Vault egress rule")
	require.Len(t, rule.To, 1)
	require.NotNil(t, rule.To[0].IPBlock)
	assert.Equal(t, "10.0.5.7/32", rule.To[0].IPBlock.CIDR)
	require.Len(t, rule.Ports, 1)
	assert.Equal(t, int32(8200), rule.Ports[0].Port.IntVal, "port parsed from the signer address")
	// The apiserver + DNS egress (default-deny baseline) are untouched — Vault is additive.
	require.NotNil(t, findApiserverEgressRule(np))

	// Selector form (in-cluster Vault): pod + namespace selector peer, default https port.
	wiSel := v2WorkloadIdentityGateway("gw", "team-a", "")
	wiSel.Spec.Credentials.WorkloadIdentity.Signer.Vault.Address = "https://vault.vault.svc"
	wiSel.Spec.Credentials.WorkloadIdentity.Signer.Vault.NetworkPolicy = &gmcv2alpha1.EgressPeer{
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "vault"}},
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "vault"}},
	}
	rule = findVaultEgressRule(buildAGCNetworkPolicyV2(wiSel, nil, nil, false, nil))
	require.NotNil(t, rule, "selector-form WI gateway gains a Vault egress rule")
	require.Len(t, rule.To, 1)
	assert.Nil(t, rule.To[0].IPBlock)
	require.NotNil(t, rule.To[0].PodSelector)
	assert.Equal(t, "vault", rule.To[0].PodSelector.MatchLabels["app.kubernetes.io/name"])
	require.NotNil(t, rule.To[0].NamespaceSelector)
	assert.Equal(t, int32(443), rule.Ports[0].Port.IntVal, "no explicit address port ⇒ https default")

	// Explicit EgressPeer.Port (Q204) overrides the address-derived port. The shared
	// descriptor lets a peer pin its own port; for Vault the override wins over the address.
	wiPort := v2WorkloadIdentityGateway("gw", "team-a", "")
	wiPort.Spec.Credentials.WorkloadIdentity.Signer.Vault.Address = "https://vault.vault.svc"
	wiPort.Spec.Credentials.WorkloadIdentity.Signer.Vault.NetworkPolicy = &gmcv2alpha1.EgressPeer{
		CIDR: "10.0.5.7/32",
		Port: ptr(int32(8201)),
	}
	rule = findVaultEgressRule(buildAGCNetworkPolicyV2(wiPort, nil, nil, true, nil))
	require.NotNil(t, rule, "explicit-port WI gateway gains a Vault egress rule")
	require.Len(t, rule.Ports, 1)
	assert.Equal(t, int32(8201), rule.Ports[0].Port.IntVal, "explicit EgressPeer.Port wins over the derived port")

	// WI gateway without a NetworkPolicy peer: default-deny preserved (no Vault rule).
	wiNone := v2WorkloadIdentityGateway("gw", "team-a", "")
	assert.Nil(t, findVaultEgressRule(buildAGCNetworkPolicyV2(wiNone, nil, nil, true, nil)),
		"WI gateway without networkPolicy keeps default-deny")

	// Possession-model (githubApp) gateway: never gains a Vault egress rule.
	pem := v2Gateway("gw", "team-a", "github-app", "")
	assert.Nil(t, findVaultEgressRule(buildAGCNetworkPolicyV2(pem, nil, nil, true, nil)),
		"PEM gateway keeps default-deny — no Vault egress")
}

// findVaultEgressRule returns the AGC→Vault egress rule (the rule whose single peer is
// neither DNS, the apiserver, nor a GitHub CIDR), or nil. It keys on the Vault peer being
// a single To with a non-DNS port — distinct from the DNS (53), apiserver (443/6443 with no
// To or apiserver CIDRs), and GitHub (443 ipBlock) rules in shape.
func findVaultEgressRule(np *networkingv1.NetworkPolicy) *networkingv1.NetworkPolicyEgressRule {
	for i := range np.Spec.Egress {
		e := &np.Spec.Egress[i]
		// DNS rule carries port 53; skip it.
		isDNS := false
		for _, p := range e.Ports {
			if p.Port != nil && p.Port.IntVal == 53 {
				isDNS = true
			}
		}
		if isDNS || len(e.To) != 1 {
			continue
		}
		peer := e.To[0]
		// A selector peer is unambiguously Vault (apiserver/GitHub/DNS use ipBlock or
		// kube-dns selectors with port 53). A single ipBlock peer that is not a GitHub
		// 443 rule and not the DNS link-local block is the external-Vault rule.
		if peer.PodSelector != nil || peer.NamespaceSelector != nil {
			if peer.NamespaceSelector == nil || peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" {
				return e
			}
			continue
		}
		if peer.IPBlock != nil && peer.IPBlock.CIDR != dnsNodeLocalCIDR {
			return e
		}
	}
	return nil
}

// TestBuildWorkloadNetworkPolicyV2_DirectEgress: direct mode keeps the DNS + proxy
// rules and adds the GitHub-CIDR rule; proxied mode carries no GitHub rule (Q168).
func TestBuildWorkloadNetworkPolicyV2_DirectEgress(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	_, cidr, err := net.ParseCIDR("140.82.112.0/20")
	require.NoError(t, err)

	direct := buildWorkloadNetworkPolicyV2(ag, []net.IPNet{*cidr}, true, nil)
	assert.True(t, hasGitHubCIDREgress(direct, "140.82.112.0/20"), "direct mode permits the GitHub CIDR")
	assert.True(t, hasDNSEgress(direct), "DNS egress is preserved in direct mode")

	proxied := buildWorkloadNetworkPolicyV2(ag, []net.IPNet{*cidr}, false, nil)
	assert.False(t, hasGitHubCIDREgress(proxied, "140.82.112.0/20"), "proxied mode carries no direct GitHub rule")
	assert.True(t, hasDNSEgress(proxied))
}

// hasGitHubCIDREgress reports whether np has an egress rule with an ipBlock peer
// for cidr.
func hasGitHubCIDREgress(np *networkingv1.NetworkPolicy, cidr string) bool {
	for _, e := range np.Spec.Egress {
		for _, peer := range e.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == cidr {
				return true
			}
		}
	}
	return false
}

// hasDNSEgress reports whether np permits DNS (port 53) egress.
func hasDNSEgress(np *networkingv1.NetworkPolicy) bool {
	for _, e := range np.Spec.Egress {
		for _, p := range e.Ports {
			if p.Port != nil && p.Port.IntVal == 53 {
				return true
			}
		}
	}
	return false
}

func TestBuildAGCDeploymentV2_TracingAndNoProxyWiring(t *testing.T) {
	// A managed-distribution API server ClusterIP (Q465): the v2 path must exempt it
	// too, or a proxied v2 tenant's AGC dials the API server through the proxy.
	t.Setenv(apiServerHostEnv, gkeAPIServerIP)
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	ag.Spec.Tracing = gmcv2alpha1.TracingConfig{Endpoint: "otel:4317", Sampler: "always_on"}
	proxy := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "team-a"},
		Spec:       gmcv2alpha1.EgressProxySpec{NoProxyCIDRs: []string{"10.20.0.0/16"}},
	}
	dep := buildAGCDeploymentV2(ag, "agc:test", proxy, gmcv2alpha1.SecurityProfileBaseline, nil)
	env := agcEnv(dep)
	assert.Equal(t, "otel:4317", env["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"])
	assert.Equal(t, "always_on", env["OTEL_TRACES_SAMPLER"])
	// NO_PROXY merges the EgressProxy's CIDRs with the cluster-internal exclusions.
	assert.Contains(t, env["NO_PROXY"], "10.20.0.0/16")
	assert.Contains(t, env["NO_PROXY"], "svc.cluster.local")
	assert.Contains(t, strings.Split(env["NO_PROXY"], ","), gkeAPIServerIP)
	assert.NotContains(t, env["NO_PROXY"], "10.96.0.0/12")
	// Proxied: the AGC's own egress is wired through the EgressProxy Service.
	assert.Contains(t, env["HTTPS_PROXY"], "shared-proxy.team-a.svc.cluster.local")
	assert.Equal(t, "shared-proxy-tls", env["PROXY_TLS_SECRET_NAME"])
}

// TestBuildAGCDeploymentV2_DirectEgress: with no proxy the AGC Deployment carries no
// HTTP(S)_PROXY/PROXY_TLS_SECRET_NAME env and mounts no proxy-CA volume, so its own
// control-plane egress goes directly to GitHub (Q168, §H.10).
func TestBuildAGCDeploymentV2_DirectEgress(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "")
	dep := buildAGCDeploymentV2(ag, "agc:test", nil, gmcv2alpha1.SecurityProfileBaseline, nil)
	env := agcEnv(dep)
	assert.NotContains(t, env, "HTTP_PROXY")
	assert.NotContains(t, env, "HTTPS_PROXY")
	assert.NotContains(t, env, "PROXY_TLS_SECRET_NAME")
	// No proxy-CA volume/mount when there is no proxy TLS Secret to mount.
	for _, v := range dep.Spec.Template.Spec.Volumes {
		assert.NotEqual(t, proxyCACertVolumeName, v.Name, "direct mode must not mount a proxy-CA volume")
	}
}

// TestBuildAGCDeploymentV2_APIBaseURL: githubURL determines the REST API base the
// AGC's token exchange addresses (Q506). Without the injection the AGC defaults to
// api.github.com, so a GHES gateway never mints an installation token.
func TestBuildAGCDeploymentV2_APIBaseURL(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "")
	dep := buildAGCDeploymentV2(ag, "agc:test", nil, gmcv2alpha1.SecurityProfileBaseline, nil)
	env := agcEnv(dep)
	require.Contains(t, env, "GITHUB_API_BASE_URL",
		"an absent var silently defaults to api.github.com")
	assert.Equal(t, "https://api.github.com", env["GITHUB_API_BASE_URL"],
		"a public-GitHub gateway keeps the value the AGC already defaulted to")

	ghes := v2Gateway("gw", "team-a", "github-app", "")
	ghes.Spec.GitHubURL = "https://ghes.example.com/example-org"
	depGHES := buildAGCDeploymentV2(ghes, "agc:test", nil, gmcv2alpha1.SecurityProfileBaseline, nil)
	assert.Equal(t, "https://ghes.example.com/api/v3", agcEnv(depGHES)["GITHUB_API_BASE_URL"])
}

// TestBuildAGCDeploymentV2_WorkloadIdentity: a delegation-model AGC carries the
// signer config env, projects a Vault-audience ServiceAccount token (not a Secret
// mount), and mounts NO GitHub App credential Secret — the App key never enters the
// pod (Q201). The rollout annotation that records the credential Secret is omitted.
func TestBuildAGCDeploymentV2_WorkloadIdentity(t *testing.T) {
	ag := v2WorkloadIdentityGateway("gw", "team-a", "")
	dep := buildAGCDeploymentV2(ag, "agc:test", nil, gmcv2alpha1.SecurityProfileBaseline, nil)

	env := agcEnv(dep)
	assert.Equal(t, "WorkloadIdentity", env["CREDENTIAL_TYPE"])
	assert.Equal(t, "424242", env["GITHUB_APP_ID"])
	assert.Equal(t, "99", env["GITHUB_INSTALLATION_ID"])
	assert.Equal(t, "https://vault.vault.svc:8200", env["VAULT_ADDR"])
	assert.Equal(t, "agc", env["VAULT_TRANSIT_KEY"])
	assert.Equal(t, "agc", env["VAULT_AUTH_ROLE"])
	assert.Equal(t, vaultTokenMountDir+"/"+vaultTokenFile, env["VAULT_SA_TOKEN_PATH"])
	// Optional mounts unset in the fixture ⇒ not threaded (AGC defaults them).
	assert.NotContains(t, env, "VAULT_TRANSIT_MOUNT")
	assert.NotContains(t, env, "VAULT_AUTH_MOUNT")

	// No GitHub App credential Secret volume/mount: there is no key to mount.
	for _, v := range dep.Spec.Template.Spec.Volumes {
		assert.NotEqual(t, agcCredsVolumeName, v.Name, "workload identity must not mount a GitHub App Secret")
	}
	// A projected ServiceAccount token volume, audience-scoped to Vault, is present.
	var vaultVol *corev1.Volume
	for i := range dep.Spec.Template.Spec.Volumes {
		if dep.Spec.Template.Spec.Volumes[i].Name == vaultTokenVolumeName {
			vaultVol = &dep.Spec.Template.Spec.Volumes[i]
		}
	}
	require.NotNil(t, vaultVol, "expected a projected Vault token volume")
	require.NotNil(t, vaultVol.Projected)
	require.Len(t, vaultVol.Projected.Sources, 1)
	sat := vaultVol.Projected.Sources[0].ServiceAccountToken
	require.NotNil(t, sat)
	assert.Equal(t, vaultTokenAudience, sat.Audience)
	assert.Equal(t, vaultTokenFile, sat.Path)
	require.NotNil(t, sat.ExpirationSeconds)
	// And it is mounted read-only at the path VAULT_SA_TOKEN_PATH points into.
	var mounted bool
	for _, m := range dep.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == vaultTokenVolumeName {
			mounted = true
			assert.Equal(t, vaultTokenMountDir, m.MountPath)
			assert.True(t, m.ReadOnly)
		}
	}
	assert.True(t, mounted, "Vault token volume must be mounted into the AGC container")

	// No github-app-secret rollout annotation (no Secret to rotate).
	_, ok := dep.Spec.Template.Annotations["actions-gateway/github-app-secret"]
	assert.False(t, ok, "workload identity has no GitHub App Secret annotation")
}

// TestActionsGatewayV2Reconcile_WorkloadIdentityProvisions: a workload-identity
// gateway holds NO App Secret, yet must provision a full AGC control plane and clear
// CredentialUnavailable — it does not fail closed on the absent Secret (Q201).
func TestActionsGatewayV2Reconcile_WorkloadIdentityProvisions(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	ag := v2WorkloadIdentityGateway("gw", "team-a", "") // no Secret, direct egress
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, ag).WithStatusSubresource(ag).Build()

	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test"}
	reconcileV2Gateway(t, r, "team-a", "gw")

	var got gmcv2alpha1.ActionsGateway
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "gw"}, &got))
	cred := meta.FindStatusCondition(got.Status.Conditions, gmcv2alpha1.ConditionCredentialUnavailable)
	require.NotNil(t, cred)
	assert.Equal(t, metav1.ConditionFalse, cred.Status, "workload identity needs no Secret; CredentialUnavailable must be cleared")

	// The AGC Deployment is provisioned (not failed closed).
	var dep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: agcNameV2(ag)}, &dep))
	assert.Equal(t, "WorkloadIdentity", agcEnv(&dep)["CREDENTIAL_TYPE"])
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

// TestAGCResources_NilOverrideReturnsDefaults asserts that a v2 ActionsGateway
// with no spec.agcResources override reproduces the platform default AGC
// container resources unchanged.
func TestAGCResources_NilOverrideReturnsDefaults(t *testing.T) {
	res := agcResources(nil)
	assert.Equal(t, resource.MustParse("500m"), res.Requests[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("2Gi"), res.Requests[corev1.ResourceMemory])
	assert.Equal(t, resource.MustParse("2"), res.Limits[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("4Gi"), res.Limits[corev1.ResourceMemory])
}

// TestAGCResources_PartialOverrideMergesPerKey asserts the override is merged
// key-by-key over the defaults (mirroring proxyResources' semantics): a tenant
// that only overrides one knob keeps the platform default for every other key.
func TestAGCResources_PartialOverrideMergesPerKey(t *testing.T) {
	override := &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")},
	}
	res := agcResources(override)
	// Overridden key applied.
	assert.Equal(t, resource.MustParse("8Gi"), res.Limits[corev1.ResourceMemory])
	// Every other key keeps the default.
	assert.Equal(t, resource.MustParse("500m"), res.Requests[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("2Gi"), res.Requests[corev1.ResourceMemory])
	assert.Equal(t, resource.MustParse("2"), res.Limits[corev1.ResourceCPU])
}

// TestAGCResources_FullOverrideWins asserts an override that sets every key
// fully replaces the defaults.
func TestAGCResources_FullOverrideWins(t *testing.T) {
	override := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("8Gi"),
		},
	}
	res := agcResources(override)
	assert.Equal(t, resource.MustParse("1"), res.Requests[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("1Gi"), res.Requests[corev1.ResourceMemory])
	assert.Equal(t, resource.MustParse("4"), res.Limits[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("8Gi"), res.Limits[corev1.ResourceMemory])
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

// TestActionsGatewayV2Reconcile_PreservesEgressWhenCacheEmpty is the Q246/Q61
// regression: a reconcile that runs before the IP-range cache has completed its first
// api.github.com/meta fetch (an empty snapshot — the state at GMC startup) must not
// blank an already-programmed direct-egress GitHub allowlist. Before the fix the
// per-CR reconcile rebuilt the workload/AGC NetworkPolicies from the empty cache and
// stripped every GitHub CIDR, default-denying worker and AGC egress to GitHub until
// IPRangeReconciler repatched — the live-observed "release-asset download times out"
// symptom.
func TestActionsGatewayV2Reconcile_PreservesEgressWhenCacheEmpty(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-app", Namespace: "team-a"}}
	ag := v2Gateway("gw", "team-a", "github-app", "") // no defaultProxyRef ⇒ direct
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ns, secret, ag).WithStatusSubresource(ag).Build()

	_, cidr, err := net.ParseCIDR("140.82.112.0/20")
	require.NoError(t, err)
	cache := &IPRangeCache{}
	cache.Set([]net.IPNet{*cidr})

	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test", IPCache: cache}
	ctx := context.Background()

	// Steady state: cache populated ⇒ NPs carry the GitHub allowlist.
	reconcileV2Gateway(t, r, "team-a", "gw")
	var anp, wnp networkingv1.NetworkPolicy
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: agcNameV2(ag)}, &anp))
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: workloadNPNameV2(ag)}, &wnp))
	require.True(t, hasGitHubCIDREgress(&anp, "140.82.112.0/20"), "precondition: AGC NP programmed")
	require.True(t, hasGitHubCIDREgress(&wnp, "140.82.112.0/20"), "precondition: workload NP programmed")

	// Simulate a GMC restart: the in-memory cache starts empty until the first fetch.
	cache.Set(nil)
	reconcileV2Gateway(t, r, "team-a", "gw")

	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: agcNameV2(ag)}, &anp))
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: workloadNPNameV2(ag)}, &wnp))
	assert.True(t, hasGitHubCIDREgress(&anp, "140.82.112.0/20"),
		"empty cache must not blank the already-programmed AGC GitHub allowlist (Q246/Q61)")
	assert.True(t, hasGitHubCIDREgress(&wnp, "140.82.112.0/20"),
		"empty cache must not blank the already-programmed workload GitHub allowlist (Q246/Q61)")
	// The apiserver egress rule is unrelated to the cache and stays either way.
	assert.NotNil(t, findApiserverEgressRule(&anp), "AGC NP keeps apiserver egress")
}

// TestActionsGatewayV2Reconcile_FailsClosedOnFreshInstallEmptyCache asserts the other
// half of the Q246/Q61 contract: a first-ever reconcile with an empty cache still
// creates the direct-egress NetworkPolicies, fail-closed (no GitHub rule). That is
// safe — no worker pod exists that early — and secure-by-default (egress is never
// opened); IPRangeReconciler patches the GitHub rule in once its first fetch lands.
func TestActionsGatewayV2Reconcile_FailsClosedOnFreshInstallEmptyCache(t *testing.T) {
	scheme := actionsGatewayV2TestScheme(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-app", Namespace: "team-a"}}
	ag := v2Gateway("gw", "team-a", "github-app", "") // direct
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ns, secret, ag).WithStatusSubresource(ag).Build()

	cache := &IPRangeCache{} // never fetched ⇒ empty snapshot
	r := &ActionsGatewayV2Reconciler{Client: c, Scheme: scheme, AGCImage: "agc:test", IPCache: cache}
	ctx := context.Background()

	reconcileV2Gateway(t, r, "team-a", "gw")

	var anp, wnp networkingv1.NetworkPolicy
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: agcNameV2(ag)}, &anp),
		"AGC NP is created even before the cache is ready")
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: workloadNPNameV2(ag)}, &wnp),
		"workload NP is created even before the cache is ready")
	assert.False(t, hasGitHubCIDREgress(&wnp, "140.82.112.0/20"), "fresh install: no GitHub rule yet (fail-closed)")
	// Egress is restricted, not open: workers still reach DNS and the apiserver rule
	// remains on the AGC NP; the GitHub allowlist simply arrives on the IPRange patch.
	assert.True(t, hasDNSEgress(&wnp), "workload NP still permits DNS")
	assert.NotNil(t, findApiserverEgressRule(&anp), "AGC NP keeps apiserver egress")
}

// TestBuildAGCDeploymentV2_RenderIsDeterministic: identical inputs must produce a
// byte-identical Deployment, because the pod template is hashed into a
// pod-template-hash and any difference rolls the tenant's control plane (Q587).
// The fixture is loaded with every map-valued input the builder touches —
// resourceAttributes, nodeSelector, the agcResources overlay — since a map ranged
// into an ordered field is how that difference gets in. Go randomizes map
// iteration per range, so one process re-rendering is a valid probe.
func TestBuildAGCDeploymentV2_RenderIsDeterministic(t *testing.T) {
	ag := v2Gateway("gw", "team-a", "github-app", "shared")
	ag.Spec.Tracing = gmcv2alpha1.TracingConfig{
		Endpoint:           "otel:4317",
		Sampler:            "parentbased_traceidratio",
		SamplerArg:         "0.1",
		ResourceAttributes: map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5"},
	}
	ag.Spec.AGCResources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")},
	}
	ag.Spec.Scheduling = &gmcv2alpha1.PodScheduling{
		NodeSelector: map[string]string{"k1": "v1", "k2": "v2", "k3": "v3", "k4": "v4"},
		Tolerations:  []corev1.Toleration{{Key: "t1", Operator: corev1.TolerationOpExists}},
	}
	proxy := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "team-a"},
		Spec:       gmcv2alpha1.EgressProxySpec{NoProxyCIDRs: []string{"10.20.0.0/16", "10.30.0.0/16"}},
	}
	extraEnv := []corev1.EnvVar{
		{Name: "GITHUB_API_BASE_URL", Value: "http://fakegithub:8080"},
		{Name: "WRAPPER_IMAGE", Value: "wrapper:test"},
	}

	render := func() string {
		b, err := json.Marshal(buildAGCDeploymentV2(ag.DeepCopy(), "agc:test", proxy.DeepCopy(),
			gmcv2alpha1.SecurityProfileBaseline, extraEnv))
		require.NoError(t, err)
		return string(b)
	}
	first := render()
	for i := range 100 {
		require.Equal(t, first, render(), "render %d differs from the first", i)
	}
}
