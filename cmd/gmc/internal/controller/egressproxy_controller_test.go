package controller

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	gmcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func egressProxyTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, appsv1.AddToScheme(s))
	require.NoError(t, autoscalingv2.AddToScheme(s))
	require.NoError(t, policyv1.AddToScheme(s))
	require.NoError(t, networkingv1.AddToScheme(s))
	require.NoError(t, gmcv2.AddToScheme(s))
	return s
}

// TestEgressProxyReconcile_CreatesOwnedChildren drives a full reconcile against a
// fake client and asserts the proxy pool's children are created, owner-referenced,
// and the status contract is written. Complements the envtest coverage by
// exercising the same code path in the unit tier.
func TestEgressProxyReconcile_CreatesOwnedChildren(t *testing.T) {
	scheme := egressProxyTestScheme(t)
	ep := newEP("shared", "team-a", func(ep *gmcv2.EgressProxy) {
		ep.Spec.MinReplicas = ptr(int32(2))
	})

	cache := &IPRangeCache{}
	_, cidr, err := net.ParseCIDR("140.82.112.0/20")
	require.NoError(t, err)
	cache.Set([]net.IPNet{*cidr})

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ep).
		WithStatusSubresource(ep).
		Build()

	r := &EgressProxyReconciler{Client: c, Scheme: scheme, IPCache: cache, ProxyImage: "proxy:test"}
	ctx := context.Background()
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "shared"}})
	require.NoError(t, err)
	// No kubelet ⇒ 0 ready replicas ⇒ requeue to recheck readiness.
	assert.Positive(t, res.RequeueAfter)

	key := types.NamespacedName{Namespace: "team-a", Name: "shared-proxy"}
	var dep appsv1.Deployment
	require.NoError(t, c.Get(ctx, key, &dep))
	require.Len(t, dep.OwnerReferences, 1)
	assert.Equal(t, "EgressProxy", dep.OwnerReferences[0].Kind)
	assert.True(t, *dep.OwnerReferences[0].Controller)

	var svc corev1.Service
	require.NoError(t, c.Get(ctx, key, &svc))
	var hpa autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, c.Get(ctx, key, &hpa))
	var pdb policyv1.PodDisruptionBudget
	require.NoError(t, c.Get(ctx, key, &pdb))
	var np networkingv1.NetworkPolicy
	require.NoError(t, c.Get(ctx, key, &np))

	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "shared-proxy-tls"}, &sec))
	assert.Equal(t, corev1.SecretTypeTLS, sec.Type)
	assert.NotEmpty(t, sec.Data[corev1.TLSCertKey])

	// Metrics mTLS bundle (Q324): server bundle mounted into the proxy, scraper
	// client bundle published for monitoring — both owner-referenced for GC.
	var metricsServer corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "shared-metrics-tls"}, &metricsServer))
	assert.NotEmpty(t, metricsServer.Data[corev1.TLSCertKey])
	assert.NotEmpty(t, metricsServer.Data[metricsCACertKey])
	require.Len(t, metricsServer.OwnerReferences, 1)
	assert.Equal(t, "EgressProxy", metricsServer.OwnerReferences[0].Kind)
	var metricsClient corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "shared-metrics-client"}, &metricsClient))
	assert.NotEmpty(t, metricsClient.Data[corev1.TLSCertKey])
	assert.NotEmpty(t, metricsClient.Data[metricsCACertKey])

	// Status contract: Ready=False/ProxyNotReady, Degraded=False, observedGeneration set.
	var got gmcv2.EgressProxy
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "shared"}, &got))
	ready := meta.FindStatusCondition(got.Status.Conditions, gmcv2.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, gmcv2.ReasonProxyNotReady, ready.Reason)
	degraded := meta.FindStatusCondition(got.Status.Conditions, gmcv2.ConditionDegraded)
	require.NotNil(t, degraded)
	assert.Equal(t, metav1.ConditionFalse, degraded.Status)
	assert.Equal(t, got.Generation, got.Status.ObservedGeneration)
}

// TestEgressProxyReconcile_ReadyWhenReplicasMeetMin proves Ready flips True once the
// proxy Deployment reports enough ready replicas (simulated by pre-seeding the
// Deployment status, since the fake client runs no controllers).
func TestEgressProxyReconcile_ReadyWhenReplicasMeetMin(t *testing.T) {
	scheme := egressProxyTestScheme(t)
	ep := newEP("shared", "team-a", func(ep *gmcv2.EgressProxy) {
		ep.Spec.MinReplicas = ptr(int32(1))
	})
	dep := buildEgressProxyDeployment(ep, "proxy:test", nil)
	dep.Status.ReadyReplicas = 1

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ep, dep).
		WithStatusSubresource(ep).
		Build()

	r := &EgressProxyReconciler{Client: c, Scheme: scheme, ProxyImage: "proxy:test"}
	ctx := context.Background()
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "shared"}})
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter, "a ready CIDR-mode pool without an IP cache has nothing to re-check on a timer")

	var got gmcv2.EgressProxy
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "shared"}, &got))
	ready := meta.FindStatusCondition(got.Status.Conditions, gmcv2.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, gmcv2.ReasonProxyReady, ready.Reason)
	assert.Equal(t, int32(1), got.Status.ReadyReplicas)
}

// TestEgressProxyReconcile_ReadyFQDNModeRequeues proves a Ready FQDN-mode proxy
// still returns a periodic RequeueAfter (Q326): the CNI-native FQDN policy has no
// Owns() watch, so without the timer a Ready pool would go dormant and out-of-band
// policy drift would never be repaired.
func TestEgressProxyReconcile_ReadyFQDNModeRequeues(t *testing.T) {
	scheme := egressProxyTestScheme(t)
	ep := newEP("shared", "team-a", func(ep *gmcv2.EgressProxy) {
		ep.Spec.MinReplicas = ptr(int32(1))
		ep.Spec.EgressPolicyMode = gmcv2.EgressPolicyModeFQDN
	})
	dep := buildEgressProxyDeployment(ep, "proxy:test", nil)
	dep.Status.ReadyReplicas = 1

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ep, dep).
		WithStatusSubresource(ep).
		Build()

	// No IP cache: FQDN mode carries no CIDR staleness to track, yet must still requeue.
	r := &EgressProxyReconciler{Client: c, Scheme: scheme, ProxyImage: "proxy:test"}
	ctx := context.Background()
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "shared"}})
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter, "a ready FQDN-mode pool must keep a periodic drift re-check (Q326)")

	var got gmcv2.EgressProxy
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "shared"}, &got))
	ready := meta.FindStatusCondition(got.Status.Conditions, gmcv2.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status, "the requeue must come from the Ready path, not the not-ready backoff")
}

// TestEgressProxyReconcile_UnmanagedAutoscalingSkipsHPA proves managedAutoscaling:
// false (Q173) provisions the pool without an HPA — the Deployment, Service, PDB,
// and NetworkPolicy are still created — and seeds the Deployment's initial replica
// count from minReplicas.
func TestEgressProxyReconcile_UnmanagedAutoscalingSkipsHPA(t *testing.T) {
	scheme := egressProxyTestScheme(t)
	ep := newEP("shared", "team-a", func(ep *gmcv2.EgressProxy) {
		ep.Spec.MinReplicas = ptr(int32(2))
		ep.Spec.ManagedAutoscaling = ptr(false)
	})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ep).WithStatusSubresource(ep).Build()
	r := &EgressProxyReconciler{Client: c, Scheme: scheme, ProxyImage: "proxy:test"}
	ctx := context.Background()
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "shared"}})
	require.NoError(t, err)

	key := types.NamespacedName{Namespace: "team-a", Name: "shared-proxy"}
	var hpa autoscalingv2.HorizontalPodAutoscaler
	assert.Error(t, c.Get(ctx, key, &hpa), "no HPA should be provisioned with managedAutoscaling: false")

	var dep appsv1.Deployment
	require.NoError(t, c.Get(ctx, key, &dep))
	require.NotNil(t, dep.Spec.Replicas)
	assert.Equal(t, int32(2), *dep.Spec.Replicas, "minReplicas seeds the initial replica count")
	var svc corev1.Service
	require.NoError(t, c.Get(ctx, key, &svc))
	var pdb policyv1.PodDisruptionBudget
	require.NoError(t, c.Get(ctx, key, &pdb))
	var np networkingv1.NetworkPolicy
	require.NoError(t, c.Get(ctx, key, &np))
}

// TestEgressProxyReconcile_UnmanagedAutoscalingDeletesManagedHPA proves flipping
// managedAutoscaling true→false deletes the previously managed HPA, so it cannot
// fight the operator's replacement autoscaler over `.spec.replicas` (Q173).
func TestEgressProxyReconcile_UnmanagedAutoscalingDeletesManagedHPA(t *testing.T) {
	scheme := egressProxyTestScheme(t)
	ep := newEP("shared", "team-a", nil)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ep).WithStatusSubresource(ep).Build()
	r := &EgressProxyReconciler{Client: c, Scheme: scheme, ProxyImage: "proxy:test"}
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "shared"}}
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	key := types.NamespacedName{Namespace: "team-a", Name: "shared-proxy"}
	var hpa autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, c.Get(ctx, key, &hpa), "managed by default: the HPA is provisioned")

	var live gmcv2.EgressProxy
	require.NoError(t, c.Get(ctx, req.NamespacedName, &live))
	live.Spec.ManagedAutoscaling = ptr(false)
	require.NoError(t, c.Update(ctx, &live))
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)

	assert.Error(t, c.Get(ctx, key, &hpa), "flipping managedAutoscaling to false must delete the managed HPA")

	// And a second unmanaged reconcile is a steady-state no-op (delete tolerates absence).
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
}

// TestEgressProxyReconcile_UnmanagedScaleToZeroIsReady proves that with
// managedAutoscaling: false an external scale-to-zero is preserved (not restored to
// the floor) and reported Ready — the pool converged to what the external scaler
// asked for; it is not wedged (Q173).
func TestEgressProxyReconcile_UnmanagedScaleToZeroIsReady(t *testing.T) {
	scheme := egressProxyTestScheme(t)
	ep := newEP("shared", "team-a", func(ep *gmcv2.EgressProxy) {
		ep.Spec.MinReplicas = ptr(int32(2))
		ep.Spec.ManagedAutoscaling = ptr(false)
	})
	// Simulate the external autoscaler having scaled the pool to zero.
	dep := buildEgressProxyDeployment(ep, "proxy:test", nil)
	dep.Spec.Replicas = ptr(int32(0))

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ep, dep).WithStatusSubresource(ep).Build()
	r := &EgressProxyReconciler{Client: c, Scheme: scheme, ProxyImage: "proxy:test"}
	ctx := context.Background()
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "shared"}})
	require.NoError(t, err)

	var got appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "shared-proxy"}, &got))
	require.NotNil(t, got.Spec.Replicas)
	assert.Equal(t, int32(0), *got.Spec.Replicas, "an external scale-to-zero must not be restored to the floor")

	var gotEP gmcv2.EgressProxy
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "shared"}, &gotEP))
	ready := meta.FindStatusCondition(gotEP.Status.Conditions, gmcv2.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status, "0/0 ready is Ready: the pool converged to the external scaler's ask")
}

// TestEgressProxyReconcile_DeletingIsNoOp verifies a delete-in-flight object is not
// re-provisioned (owner-ref GC handles the children; no finalizer is used).
func TestEgressProxyReconcile_DeletingIsNoOp(t *testing.T) {
	scheme := egressProxyTestScheme(t)
	now := metav1.Now()
	ep := newEP("shared", "team-a", func(ep *gmcv2.EgressProxy) {
		ep.DeletionTimestamp = &now
		ep.Finalizers = []string{"kubernetes"} // a finalizer is required for the fake client to retain a deleting object
	})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ep).WithStatusSubresource(ep).Build()
	r := &EgressProxyReconciler{Client: c, Scheme: scheme, ProxyImage: "proxy:test"}
	ctx := context.Background()
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "shared"}})
	require.NoError(t, err)

	// No children created for a deleting object.
	var dep appsv1.Deployment
	err = c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "shared-proxy"}, &dep)
	assert.Error(t, err, "no Deployment should be provisioned for a deleting EgressProxy")
}

// TestEgressProxyReconcileResources_PolicyPrecedesDeployment fails the proxy
// Deployment create and asserts the proxy NetworkPolicy already exists, so a first
// provision that stops there leaves no proxy pod without a policy (Q1264).
func TestEgressProxyReconcileResources_PolicyPrecedesDeployment(t *testing.T) {
	scheme := egressProxyTestScheme(t)
	ep := newEP("shared", "team-a", nil)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ep).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					return errors.New("deployment create rejected")
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()
	r := &EgressProxyReconciler{Client: c, Scheme: scheme, IPCache: &IPRangeCache{}, ProxyImage: "proxy:test"}
	ctx := context.Background()

	err := r.reconcileResources(ctx, ep, nil, nil)
	var pe *provisioningError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, "apply proxy Deployment", pe.step)

	key := types.NamespacedName{Namespace: "team-a", Name: "shared-proxy"}
	require.NoError(t, c.Get(ctx, key, &networkingv1.NetworkPolicy{}), "the policy must exist before the Deployment is applied")
}

// TestEgressProxyReconcile_HoldsWhileAllowlistConfigMapUnread covers a GMC restart
// (Q1322). The allowlist's ConfigMap half is empty until its reconciler first runs, so
// a referrer's GitHub host that only the ConfigMap covers reads as refused in that
// window. Acting on it would narrow a live pool's policy on every restart, so the
// reconcile writes nothing and comes back; once the ConfigMap is applied it proceeds.
func TestEgressProxyReconcile_HoldsWhileAllowlistConfigMapUnread(t *testing.T) {
	scheme := egressProxyTestScheme(t)
	ep := newEP("shared", "team-a", func(ep *gmcv2.EgressProxy) {
		ep.Spec.EgressPolicyMode = gmcv2.EgressPolicyModeFQDN
		ep.Spec.DestinationFQDNs = []string{"proxy.golang.org"}
	})
	gw := ghesGateway("gw", "team-a", "https://ghes.corp.example/acme", "shared")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ep, gw).
		WithStatusSubresource(ep).
		Build()

	al := allowlist.NewEgressDestination([]string{"golang.org"}, nil)
	al.AwaitDynamic(time.Now().Add(time.Hour))
	r := &EgressProxyReconciler{Client: c, Scheme: scheme, ProxyImage: "proxy:test", EgressAllowlist: al}
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "shared"}}
	depKey := types.NamespacedName{Namespace: "team-a", Name: "shared-proxy"}

	res, err := r.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, allowlistPendingRequeue, res.RequeueAfter)
	var dep appsv1.Deployment
	require.True(t, apierrors.IsNotFound(c.Get(ctx, depKey, &dep)), "a held reconcile writes no child")
	var got gmcv2.EgressProxy
	require.NoError(t, c.Get(ctx, req.NamespacedName, &got))
	assert.Empty(t, got.Status.Conditions, "and reports nothing it has not decided")

	al.SetDynamic([]string{"corp.example"}, nil)
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, depKey, &dep))
	assert.Contains(t, dep.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{
		Name:  "PROXY_ALLOWED_HOST_SUFFIXES",
		Value: "api.github.com,github.com,codeload.github.com,objects.githubusercontent.com,actions.githubusercontent.com,blob.core.windows.net,ghes.corp.example,proxy.golang.org",
	})

	t.Run("a host the static base covers does not wait", func(t *testing.T) {
		static := allowlist.NewEgressDestination([]string{"corp.example"}, nil)
		static.AwaitDynamic(time.Now().Add(time.Hour))
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ep.DeepCopy(), gw.DeepCopy()).WithStatusSubresource(ep).Build()
		r := &EgressProxyReconciler{Client: c, Scheme: scheme, ProxyImage: "proxy:test", EgressAllowlist: static}
		_, err := r.Reconcile(ctx, req)
		require.NoError(t, err)
		require.NoError(t, c.Get(ctx, depKey, &appsv1.Deployment{}), "nothing is refused, so nothing is held")
	})

	t.Run("a host no ConfigMap could cover does not wait", func(t *testing.T) {
		pending := allowlist.NewEgressDestination([]string{"golang.org"}, nil)
		pending.AwaitDynamic(time.Now().Add(time.Hour))
		wild := ghesGateway("gw", "team-a", "https://*.corp.example/acme", "shared")
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ep.DeepCopy(), wild).WithStatusSubresource(ep).Build()
		r := &EgressProxyReconciler{Client: c, Scheme: scheme, ProxyImage: "proxy:test", EgressAllowlist: pending}
		_, err := r.Reconcile(ctx, req)
		require.NoError(t, err)
		require.NoError(t, c.Get(ctx, depKey, &appsv1.Deployment{}), "a wildcard is refused whatever the ConfigMap holds")
	})

	t.Run("a ConfigMap that is never read stops holding at the deadline", func(t *testing.T) {
		// Without a bound a pool with a refused host would never reconcile again:
		// no children, no status, and nothing saying why.
		unread := allowlist.NewEgressDestination([]string{"golang.org"}, nil)
		unread.AwaitDynamic(time.Now().Add(-time.Second))
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ep.DeepCopy(), gw.DeepCopy()).WithStatusSubresource(ep).Build()
		r := &EgressProxyReconciler{Client: c, Scheme: scheme, ProxyImage: "proxy:test", EgressAllowlist: unread}
		_, err := r.Reconcile(ctx, req)
		require.NoError(t, err)
		require.NoError(t, c.Get(ctx, depKey, &appsv1.Deployment{}), "the pool reconciles on the static list")
		var got gmcv2.EgressProxy
		require.NoError(t, c.Get(ctx, req.NamespacedName, &got))
		cond := meta.FindStatusCondition(got.Status.Conditions, gmcv2.ConditionGitHubEgressIncomplete)
		require.NotNil(t, cond)
		assert.Equal(t, gmcv2.ReasonGitHubHostNotAllowlisted, cond.Reason, "and says which host it left out")
	})
}

// TestEgressProxyReconcile_RefusedReferrerHostIsNotEmitted is the reconcile-level
// statement of Q1322: a tenant-chosen githubURL host the platform allowlist does not
// cover reaches neither the CONNECT allowlist nor the status as allowed.
func TestEgressProxyReconcile_RefusedReferrerHostIsNotEmitted(t *testing.T) {
	scheme := egressProxyTestScheme(t)
	ep := newEP("shared", "team-a", func(ep *gmcv2.EgressProxy) {
		ep.Spec.EgressPolicyMode = gmcv2.EgressPolicyModeFQDN
		ep.Spec.DestinationFQDNs = []string{"proxy.golang.org"}
	})
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ep,
			ghesGateway("allowed", "team-a", "https://ghes.corp.example/acme", "shared"),
			ghesGateway("refused", "team-a", "https://exfil.example.net/acme", "shared")).
		WithStatusSubresource(ep).
		Build()
	r := &EgressProxyReconciler{
		Client: c, Scheme: scheme, ProxyImage: "proxy:test",
		EgressAllowlist: allowlist.NewEgressDestination([]string{"golang.org", "corp.example"}, nil),
	}
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "shared"}}
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	var dep appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "shared-proxy"}, &dep))
	var suffixes string
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "PROXY_ALLOWED_HOST_SUFFIXES" {
			suffixes = e.Value
		}
	}
	assert.Contains(t, suffixes, "ghes.corp.example")
	assert.NotContains(t, suffixes, "exfil.example.net")

	var got gmcv2.EgressProxy
	require.NoError(t, c.Get(ctx, req.NamespacedName, &got))
	gap := meta.FindStatusCondition(got.Status.Conditions, gmcv2.ConditionGitHubEgressIncomplete)
	require.NotNil(t, gap)
	assert.Equal(t, metav1.ConditionTrue, gap.Status)
	assert.Equal(t, gmcv2.ReasonGitHubHostNotAllowlisted, gap.Reason)
	assert.Contains(t, gap.Message, "exfil.example.net")
	assert.NotContains(t, gap.Message, "ghes.corp.example", "an allowed host is not part of the gap")
}
