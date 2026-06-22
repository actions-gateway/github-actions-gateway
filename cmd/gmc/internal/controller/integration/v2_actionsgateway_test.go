//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// These tests exercise the v2 ActionsGateway reconciler (Q164, M3a) end-to-end
// against the real apiserver: a v2 ActionsGateway is reconciled into the per-tenant
// AGC control plane (Deployment/SA/RoleBinding/Service, AGC+workload NetworkPolicies,
// metrics Secrets), every child owner-referenced for cascade GC, its egress wired
// through the EgressProxy named by defaultProxyRef, and the uniform status contract
// surfaced. envtest runs no kubelet, so the AGC Deployment never becomes ready —
// AGCAvailable/Ready stay False, which is the correct observation.

// startActionsGatewayV2Reconciler starts an ActionsGatewayV2Reconciler for the
// duration of a test against the envtest apiserver.
func startActionsGatewayV2Reconciler(t *testing.T) {
	t.Helper()
	mgrCtx, mgrCancel := context.WithCancel(ctx)
	t.Cleanup(mgrCancel)

	skipNameValidation := true
	syncPeriod := 2 * time.Second
	mgr, err := ctrl.NewManager(testEnv.Config, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		Cache:                  cache.Options{SyncPeriod: &syncPeriod},
	})
	require.NoError(t, err)

	err = (&controller.ActionsGatewayV2Reconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		AGCImage: "agc:test",
	}).SetupWithManager(mgr)
	require.NoError(t, err)

	go func() { _ = mgr.Start(mgrCtx) }()
}

// newV2ActionsGateway builds a v2 ActionsGateway referencing the given GitHub App
// Secret and (optionally) defaultProxyRef EgressProxy.
func newV2GatewayWired(name, ns, secretName, proxyRef string) *v2alpha1.ActionsGateway {
	ag := &v2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v2alpha1.ActionsGatewaySpec{
			GitHubAppRef: v2alpha1.LocalSecretReference{Name: secretName},
			GitHubURL:    "https://github.com/example-org",
		},
	}
	if proxyRef != "" {
		ag.Spec.DefaultProxyRef = &v2alpha1.LocalObjectRef{Name: proxyRef}
	}
	return ag
}

// newV2EgressProxyObject creates a bare EgressProxy CR (no reconciler needed — the
// ActionsGateway reconciler only reads its name + spec to wire the AGC's egress).
func newV2EgressProxyObject(name, ns string) *v2alpha1.EgressProxy {
	return &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
}

func hasGatewayOwnerRef(refs []metav1.OwnerReference, name string) bool {
	for _, r := range refs {
		if r.Kind == "ActionsGateway" && r.Name == name && r.Controller != nil && *r.Controller {
			return true
		}
	}
	return false
}

func TestV2_ActionsGateway_ProvisionsAGCControlPlane(t *testing.T) {
	const ns = "v2-ag-provision"
	createNamespaceWithLabels(t, ns, map[string]string{
		v2alpha1.SecurityProfileLabel: v2alpha1.SecurityProfileRestricted,
	})
	createGitHubAppSecret(t, ns, "github-app")
	require.NoError(t, k8sClient.Create(ctx, newV2EgressProxyObject("shared", ns)))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}})
	})

	ag := newV2GatewayWired("gw", ns, "github-app", "shared")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startActionsGatewayV2Reconciler(t)

	// Per-gateway derived names (§H.16 #1): every child is "<ag>-<suffix>" so two
	// gateways in one namespace never collide. The gateway is "gw".
	const (
		gwAGC         = "gw-agc"
		gwWorkerSA    = "gw-worker"
		gwWorkloadNP  = "gw-workload"
		gwMetricsTLS  = "gw-agc-metrics-tls"
		gwMetricsClnt = "gw-agc-metrics-client"
		crtReaderRole = "agc-clusterrunnertemplate-reader"
	)

	// The AGC Deployment is the last child applied, so its presence implies the
	// earlier children (SAs, RoleBinding, metrics Secrets, Service, NetworkPolicies)
	// already exist.
	var dep appsv1.Deployment
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: gwAGC}, &dep) == nil
	}, 15*time.Second, 100*time.Millisecond, "AGC Deployment should be created")

	// Owner reference for cascade GC.
	assert.True(t, hasGatewayOwnerRef(dep.OwnerReferences, "gw"), "AGC Deployment must be owned by the ActionsGateway")

	// Egress wiring: HTTP(S)_PROXY → the resolved EgressProxy Service; SECURITY_PROFILE
	// from the namespace label; credentials are NEVER in env.
	container := dep.Spec.Template.Spec.Containers[0]
	assert.Equal(t, "https://shared-proxy."+ns+".svc.cluster.local:8080", envValue(&dep, "HTTP_PROXY"))
	assert.Equal(t, "https://shared-proxy."+ns+".svc.cluster.local:8080", envValue(&dep, "HTTPS_PROXY"))
	assert.Equal(t, "shared-proxy-tls", envValue(&dep, "PROXY_TLS_SECRET_NAME"))
	assert.Equal(t, v2alpha1.SecurityProfileRestricted, envValue(&dep, "SECURITY_PROFILE"))
	assert.Equal(t, "gw", envValue(&dep, "GATEWAY_NAME"), "AGC must be scoped to its gateway")
	assert.Equal(t, gwWorkerSA, envValue(&dep, "WORKER_SERVICE_ACCOUNT"), "per-gateway worker SA")
	assert.Equal(t, "https://github.com/example-org", envValue(&dep, "GITHUB_ORG_URL"))
	for _, e := range container.Env {
		assert.NotContains(t, []string{"appId", "privateKey", "installationId"}, e.Name, "credentials must never be passed via env")
	}
	// Credential mounted as files (volume from the GitHub App Secret).
	foundCredVol := false
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == "github-app" {
			foundCredVol = true
		}
	}
	assert.True(t, foundCredVol, "GitHub App Secret must be mounted as a volume")

	// ServiceAccounts (per-gateway names).
	for _, sa := range []string{gwAGC, gwWorkerSA} {
		var got corev1.ServiceAccount
		require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: sa}, &got), "ServiceAccount %s", sa)
		assert.True(t, hasGatewayOwnerRef(got.OwnerReferences, "gw"))
	}

	// RoleBinding → agc-tenant-role (per-gateway binding name).
	var rb rbacv1.RoleBinding
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: gwAGC}, &rb))
	assert.Equal(t, "agc-tenant-role", rb.RoleRef.Name)
	assert.True(t, hasGatewayOwnerRef(rb.OwnerReferences, "gw"))

	// Per-gateway ClusterRoleBinding granting cluster-scoped read of
	// ClusterRunnerTemplate. Cluster-scoped, so no owner ref (deleted explicitly on
	// gateway deletion). Name embeds the namespace + gateway to stay globally unique.
	var crb rbacv1.ClusterRoleBinding
	crbName := crtReaderRole + "." + ns + ".gw"
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: crbName}, &crb))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: crbName}})
	})
	assert.Equal(t, crtReaderRole, crb.RoleRef.Name)
	require.Len(t, crb.Subjects, 1)
	assert.Equal(t, gwAGC, crb.Subjects[0].Name)
	assert.Empty(t, crb.OwnerReferences, "cluster-scoped binding carries no cross-scope owner ref")

	// Metrics Secrets (server bundle mounted into AGC + scraper client bundle).
	for _, name := range []string{gwMetricsTLS, gwMetricsClnt} {
		var sec corev1.Secret
		require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sec), "metrics Secret %s", name)
		assert.True(t, hasGatewayOwnerRef(sec.OwnerReferences, "gw"))
		assert.NotEmpty(t, sec.Data["ca.crt"])
	}

	// Workload NetworkPolicy: default-deny ingress + egress only to the proxy + DNS.
	var workloadNP networkingv1.NetworkPolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: gwWorkloadNP}, &workloadNP))
	assert.True(t, hasGatewayOwnerRef(workloadNP.OwnerReferences, "gw"))
	assert.Empty(t, workloadNP.Spec.Ingress, "workload NetworkPolicy must default-deny ingress")
	foundProxyEgress := false
	for _, rule := range workloadNP.Spec.Egress {
		for _, peer := range rule.To {
			if peer.PodSelector != nil && peer.PodSelector.MatchLabels["app"] == proxyName {
				foundProxyEgress = true
			}
		}
	}
	assert.True(t, foundProxyEgress, "workload egress must target the proxy pods")

	// AGC NetworkPolicy: owned, admits k8s API egress (per-gateway name + selector).
	var agcNP networkingv1.NetworkPolicy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: gwAGC}, &agcNP))
	assert.True(t, hasGatewayOwnerRef(agcNP.OwnerReferences, "gw"))
	assert.Equal(t, gwAGC, agcNP.Spec.PodSelector.MatchLabels["app"], "AGC NP must select this gateway's AGC pods")

	// Status: uniform contract. No kubelet ⇒ AGC never ready ⇒ Ready=False/AGCReady.
	require.Eventually(t, func() bool {
		var got v2alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw"}, &got); err != nil {
			return false
		}
		ready := findCondition(got.Status.Conditions, v2alpha1.ConditionReady)
		degraded := findCondition(got.Status.Conditions, v2alpha1.ConditionDegraded)
		cred := findCondition(got.Status.Conditions, v2alpha1.ConditionCredentialUnavailable)
		return ready != nil && ready.Status == metav1.ConditionFalse &&
			degraded != nil && degraded.Status == metav1.ConditionFalse &&
			cred != nil && cred.Status == metav1.ConditionFalse &&
			got.Status.ObservedGeneration == got.Generation
	}, 15*time.Second, 100*time.Millisecond, "status should be Ready=False (no kubelet), Degraded=False, CredentialUnavailable=False, observedGeneration set")
}

func TestV2_ActionsGateway_FailsClosedWithoutCredential(t *testing.T) {
	const ns = "v2-ag-no-cred"
	createNamespace(t, ns)
	require.NoError(t, k8sClient.Create(ctx, newV2EgressProxyObject("shared", ns)))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}})
	})

	ag := newV2GatewayWired("gw", ns, "missing-secret", "shared")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startActionsGatewayV2Reconciler(t)

	require.Eventually(t, func() bool {
		var got v2alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw"}, &got); err != nil {
			return false
		}
		cred := findCondition(got.Status.Conditions, v2alpha1.ConditionCredentialUnavailable)
		ready := findCondition(got.Status.Conditions, v2alpha1.ConditionReady)
		return cred != nil && cred.Status == metav1.ConditionTrue && cred.Reason == v2alpha1.ReasonSecretNotFound &&
			ready != nil && ready.Status == metav1.ConditionFalse
	}, 15*time.Second, 100*time.Millisecond, "missing credential must surface CredentialUnavailable=True / Ready=False")

	// No AGC Deployment is created while the credential is missing (fail closed).
	var dep appsv1.Deployment
	assert.Error(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw-agc"}, &dep))
}

func TestV2_ActionsGateway_FailsClosedWhenProxyMissing(t *testing.T) {
	const ns = "v2-ag-no-proxy"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")

	// defaultProxyRef names an EgressProxy that does not exist.
	ag := newV2GatewayWired("gw", ns, "github-app", "absent")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })

	startActionsGatewayV2Reconciler(t)

	require.Eventually(t, func() bool {
		var got v2alpha1.ActionsGateway
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw"}, &got); err != nil {
			return false
		}
		ready := findCondition(got.Status.Conditions, v2alpha1.ConditionReady)
		return ready != nil && ready.Status == metav1.ConditionFalse && ready.Reason == v2alpha1.ReasonProxyNotFound
	}, 15*time.Second, 100*time.Millisecond, "missing EgressProxy must surface Ready=False/ProxyNotFound")

	var dep appsv1.Deployment
	assert.Error(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw-agc"}, &dep), "no AGC Deployment while proxy unresolved")

	// Once the EgressProxy appears, the gateway flips to provisioning (the watch
	// re-enqueues; the AGC Deployment is created).
	require.NoError(t, k8sClient.Create(ctx, newV2EgressProxyObject("absent", ns)))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "absent", Namespace: ns}})
	})
	require.Eventually(t, func() bool {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw-agc"}, &dep) == nil
	}, 15*time.Second, 100*time.Millisecond, "AGC Deployment should be created once the proxy resolves")
}

// TestV2_MultiGateway_CoexistAndPerGatewayGC proves two ActionsGateways in one
// namespace provision disjoint, non-colliding control planes (§H.16 #1), and that
// deleting one cleans up only its own cluster-scoped ClusterRoleBinding — its
// neighbor's children are untouched. (envtest runs no garbage collector, so the
// owner-ref cascade of the namespaced children is verified by ownership + disjoint
// naming here and proven live in the kind e2e; the cluster-scoped binding has no
// owner ref and is removed by reconcileDelete, which this test drives directly.)
func TestV2_MultiGateway_CoexistAndPerGatewayGC(t *testing.T) {
	const ns = "v2-multigw"
	createNamespace(t, ns)
	createGitHubAppSecret(t, ns, "github-app")
	require.NoError(t, k8sClient.Create(ctx, newV2EgressProxyObject("shared", ns)))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), &v2alpha1.EgressProxy{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns}})
	})

	agA := newV2GatewayWired("alpha", ns, "github-app", "shared")
	agB := newV2GatewayWired("beta", ns, "github-app", "shared")
	require.NoError(t, k8sClient.Create(ctx, agA))
	require.NoError(t, k8sClient.Create(ctx, agB))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), agB)
		for _, n := range []string{"alpha", "beta"} {
			_ = k8sClient.Delete(context.Background(), &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "agc-clusterrunnertemplate-reader." + ns + "." + n}})
		}
	})

	startActionsGatewayV2Reconciler(t)

	// Both AGC Deployments come up under disjoint per-gateway names — no collision.
	for _, name := range []string{"alpha-agc", "beta-agc"} {
		var dep appsv1.Deployment
		require.Eventually(t, func() bool {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &dep) == nil
		}, 15*time.Second, 100*time.Millisecond, "AGC Deployment %s should be created", name)
		gw := name[:len(name)-len("-agc")]
		assert.True(t, hasGatewayOwnerRef(dep.OwnerReferences, gw), "%s owned by %s", name, gw)
	}

	// Each gateway has its own ClusterRoleBinding.
	crbAlpha := "agc-clusterrunnertemplate-reader." + ns + ".alpha"
	crbBeta := "agc-clusterrunnertemplate-reader." + ns + ".beta"
	for _, n := range []string{crbAlpha, crbBeta} {
		var crb rbacv1.ClusterRoleBinding
		require.Eventually(t, func() bool {
			return k8sClient.Get(ctx, types.NamespacedName{Name: n}, &crb) == nil
		}, 15*time.Second, 100*time.Millisecond, "ClusterRoleBinding %s should exist", n)
	}

	// Delete alpha: its finalizer-gated deletion drives reconcileDelete, which
	// removes alpha's ClusterRoleBinding and lets the object finalize away.
	require.NoError(t, k8sClient.Delete(ctx, agA))
	require.Eventually(t, func() bool {
		var got v2alpha1.ActionsGateway
		return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "alpha"}, &got))
	}, 15*time.Second, 100*time.Millisecond, "alpha should finalize away")

	// alpha's ClusterRoleBinding is gone; beta's remains (per-gateway GC isolation).
	var goneCRB rbacv1.ClusterRoleBinding
	assert.True(t, apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: crbAlpha}, &goneCRB)),
		"alpha's ClusterRoleBinding must be deleted")
	var betaCRB rbacv1.ClusterRoleBinding
	assert.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: crbBeta}, &betaCRB),
		"beta's ClusterRoleBinding must survive alpha's deletion")
	var betaDep appsv1.Deployment
	assert.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "beta-agc"}, &betaDep),
		"beta's AGC Deployment must survive alpha's deletion")
}
