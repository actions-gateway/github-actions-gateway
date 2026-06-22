//go:build integration

package integration_test

import (
	"testing"

	gmcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// These tests prove the v2alpha1 (actions-gateway.com) GMC kinds install into the
// real apiserver and round-trip alongside v1alpha1 (Q149, M1 exit criterion), and
// that the CEL/structural validation behaves under real-apiserver semantics —
// defaulting and the per-field immutability transitions only the apiserver applies.
// No reconciler is exercised: M1 is the API foundation only. The v1alpha1 validating
// webhook is scoped to group actions-gateway.github.com, so it never intercepts
// these v2 (actions-gateway.com) objects.

func newV2ActionsGateway(ns, name string) *gmcv2alpha1.ActionsGateway {
	return &gmcv2alpha1.ActionsGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: gmcv2alpha1.ActionsGatewaySpec{
			GitHubAppRef: gmcv2alpha1.LocalSecretReference{Name: "acme-github-app"},
			GitHubURL:    "https://github.com/acme",
		},
	}
}

func TestV2_ActionsGateway_RoundTripAndDefaulting(t *testing.T) {
	const ns = "v2-ag-rt"
	createNamespace(t, ns)

	ag := newV2ActionsGateway(ns, "acme")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	var got gmcv2alpha1.ActionsGateway
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "acme"}, &got))
	// securityProfile is no longer a per-gateway field in v2: it relocated to the
	// namespace (actions-gateway.com/security-profile label, Q175 / §H.16 #7).
	assert.Equal(t, "info", got.Spec.LogLevel, "logLevel should default to info")
	assert.Equal(t, "acme-github-app", got.Spec.GitHubAppRef.Name)
}

func TestV2_ActionsGateway_GitHubURLImmutable(t *testing.T) {
	const ns = "v2-ag-immutable"
	createNamespace(t, ns)

	ag := newV2ActionsGateway(ns, "acme")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	var got gmcv2alpha1.ActionsGateway
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "acme"}, &got))

	// githubURL is frozen by a CEL transition rule (self == oldSelf): rebinding a
	// running gateway's GitHub org is a footgun (§H.15).
	got.Spec.GitHubURL = "https://github.com/other-org"
	err := k8sClient.Update(ctx, &got)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "expected Invalid mutating githubURL, got %v", err)
}

func TestV2_ActionsGateway_GitHubAppRefNameMutable(t *testing.T) {
	const ns = "v2-ag-rotate"
	createNamespace(t, ns)

	ag := newV2ActionsGateway(ns, "acme")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	var got gmcv2alpha1.ActionsGateway
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "acme"}, &got))

	// githubAppRef.name is deliberately mutable — the supported credential-rotation path.
	got.Spec.GitHubAppRef.Name = "acme-github-app-rotated"
	require.NoError(t, k8sClient.Update(ctx, &got), "rotating githubAppRef.name must be allowed")
}

func TestV2_ActionsGateway_NameMaxLengthRejected(t *testing.T) {
	const ns = "v2-ag-name"
	createNamespace(t, ns)

	longName := "a23456789012345678901234567890123456789012345678901234" // 54 chars
	require.Len(t, longName, 54)
	ag := newV2ActionsGateway(ns, longName)
	err := k8sClient.Create(ctx, ag)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "expected Invalid for over-length name, got %v", err)
}

func TestV2_EgressProxy_RoundTripAndDefaulting(t *testing.T) {
	const ns = "v2-ep-rt"
	createNamespace(t, ns)

	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: ns},
		Spec:       gmcv2alpha1.EgressProxySpec{},
	}
	require.NoError(t, k8sClient.Create(ctx, ep))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ep) })

	var got gmcv2alpha1.EgressProxy
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "shared"}, &got))
	require.NotNil(t, got.Spec.MinReplicas)
	require.NotNil(t, got.Spec.MaxReplicas)
	require.NotNil(t, got.Spec.TargetCPUUtilizationPercentage)
	require.NotNil(t, got.Spec.ManagedNetworkPolicy)
	assert.Equal(t, int32(2), *got.Spec.MinReplicas)
	assert.Equal(t, int32(10), *got.Spec.MaxReplicas)
	assert.Equal(t, int32(60), *got.Spec.TargetCPUUtilizationPercentage)
	assert.True(t, *got.Spec.ManagedNetworkPolicy, "managedNetworkPolicy should default to true (secure default)")
}

func TestV2_EgressProxy_MinReplicasExceedingMaxRejected(t *testing.T) {
	const ns = "v2-ep-replicas"
	createNamespace(t, ns)

	minR := int32(5)
	maxR := int32(2)
	ep := &gmcv2alpha1.EgressProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: ns},
		Spec:       gmcv2alpha1.EgressProxySpec{MinReplicas: &minR, MaxReplicas: &maxR},
	}
	err := k8sClient.Create(ctx, ep)
	require.Error(t, err)
	assert.True(t, apierrors.IsInvalid(err), "expected Invalid for minReplicas > maxReplicas, got %v", err)
}

// TestV2_CoexistsWithV1 proves both API groups are served at once: a v1alpha1 and a
// v2alpha1 ActionsGateway live in the same namespace without contention (they are
// distinct CRDs in distinct groups). This is the M1 no-behavior-change non-goal.
func TestV2_CoexistsWithV1(t *testing.T) {
	const ns = "v2-coexist"
	createNamespace(t, ns)

	v1ag := newActionsGateway("legacy", ns, "github-app")
	require.NoError(t, k8sClient.Create(ctx, v1ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, v1ag) })

	v2ag := newV2ActionsGateway(ns, "modern")
	require.NoError(t, k8sClient.Create(ctx, v2ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, v2ag) })

	var gotV1 gmcv1alpha1.ActionsGateway
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "legacy"}, &gotV1))
	var gotV2 gmcv2alpha1.ActionsGateway
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "modern"}, &gotV2))
}
