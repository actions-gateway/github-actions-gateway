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
	"net"
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func egressProxyTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, appsv1.AddToScheme(s))
	require.NoError(t, autoscalingv2.AddToScheme(s))
	require.NoError(t, policyv1.AddToScheme(s))
	require.NoError(t, networkingv1.AddToScheme(s))
	require.NoError(t, gmcv2alpha1.AddToScheme(s))
	return s
}

// TestEgressProxyReconcile_CreatesOwnedChildren drives a full reconcile against a
// fake client and asserts the proxy pool's children are created, owner-referenced,
// and the status contract is written. Complements the envtest coverage by
// exercising the same code path in the unit tier.
func TestEgressProxyReconcile_CreatesOwnedChildren(t *testing.T) {
	scheme := egressProxyTestScheme(t)
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
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

	// Status contract: Ready=False/ProxyNotReady, Degraded=False, observedGeneration set.
	var got gmcv2alpha1.EgressProxy
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "shared"}, &got))
	ready := meta.FindStatusCondition(got.Status.Conditions, gmcv2alpha1.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, gmcv2alpha1.ReasonProxyNotReady, ready.Reason)
	degraded := meta.FindStatusCondition(got.Status.Conditions, gmcv2alpha1.ConditionDegraded)
	require.NotNil(t, degraded)
	assert.Equal(t, metav1.ConditionFalse, degraded.Status)
	assert.Equal(t, got.Generation, got.Status.ObservedGeneration)
}

// TestEgressProxyReconcile_ReadyWhenReplicasMeetMin proves Ready flips True once the
// proxy Deployment reports enough ready replicas (simulated by pre-seeding the
// Deployment status, since the fake client runs no controllers).
func TestEgressProxyReconcile_ReadyWhenReplicasMeetMin(t *testing.T) {
	scheme := egressProxyTestScheme(t)
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
		ep.Spec.MinReplicas = ptr(int32(1))
	})
	dep := buildEgressProxyDeployment(ep, "proxy:test")
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
	assert.Zero(t, res.RequeueAfter, "a ready pool does not requeue on a timer")

	var got gmcv2alpha1.EgressProxy
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "shared"}, &got))
	ready := meta.FindStatusCondition(got.Status.Conditions, gmcv2alpha1.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, gmcv2alpha1.ReasonProxyReady, ready.Reason)
	assert.Equal(t, int32(1), got.Status.ReadyReplicas)
}

// TestEgressProxyReconcile_DeletingIsNoOp verifies a delete-in-flight object is not
// re-provisioned (owner-ref GC handles the children; no finalizer is used).
func TestEgressProxyReconcile_DeletingIsNoOp(t *testing.T) {
	scheme := egressProxyTestScheme(t)
	now := metav1.Now()
	ep := newEP("shared", "team-a", func(ep *gmcv2alpha1.EgressProxy) {
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
