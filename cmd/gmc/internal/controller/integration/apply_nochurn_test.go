//go:build integration

package integration_test

import (
	"testing"
	"time"

	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestGMC_ApplyHelpers_NoSteadyStateChurn proves the CreateOrPatch migration
// (audit A6 / Q65) does not churn child resources on every reconcile.
//
// The helpers replace whole Specs (Deployment, HPA, NetworkPolicy, …) with a
// builder-produced object that omits server-defaulted fields (e.g. a
// Deployment's rollout Strategy, an HPA's scaling Behavior). CreateOrPatch
// therefore emits a non-empty merge patch each reconcile, but the apiserver
// re-applies defaults and skips the write when the stored object is unchanged —
// so resourceVersion must stay stable. A fake client cannot exercise this
// (no defaulting, no no-op-write detection); only a real apiserver can, which
// is why this lives in the envtest tier.
//
// The reconciler runs with SyncPeriod=2s (see startGMCReconciler), so the
// Consistently window below spans several full periodic reconciles. If any
// apply* helper produced an effective write, the resourceVersion would advance
// and the assertion would fail.
func TestGMC_ApplyHelpers_NoSteadyStateChurn(t *testing.T) {
	const nsName = "team-nochurn"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("nochurn-gateway", nsName, "github-app")
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, ag)
	})

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	// The child resources whose apply* helpers replace a whole Spec are the
	// churn-prone ones; the simpler label-only helpers are included too so a
	// regression in any of them is caught. Each entry reuses a single typed
	// object across Get calls (Get overwrites it).
	targets := []struct {
		label string
		name  string
		obj   client.Object
	}{
		{"proxy Deployment", proxyName, &appsv1.Deployment{}},
		{"AGC Deployment", agcName, &appsv1.Deployment{}},
		{"proxy HPA", proxyName, &autoscalingv2.HorizontalPodAutoscaler{}},
		{"proxy Service", proxyName, &corev1.Service{}},
		{"proxy PDB", proxyName, &policyv1.PodDisruptionBudget{}},
		{"proxy NetworkPolicy", proxyName, &networkingv1.NetworkPolicy{}},
		{"workload NetworkPolicy", workloadName, &networkingv1.NetworkPolicy{}},
		{"AGC RoleBinding", agcName, &rbacv1.RoleBinding{}},
		{"AGC ServiceAccount", agcName, &corev1.ServiceAccount{}},
		{"worker ServiceAccount", workerSAName, &corev1.ServiceAccount{}},
	}

	// Wait until every target resource has been provisioned.
	for _, tt := range targets {
		key := types.NamespacedName{Namespace: nsName, Name: tt.name}
		g.Eventually(func() error {
			return k8sClient.Get(ctx, key, tt.obj)
		}, 15*time.Second, 25*time.Millisecond).Should(gomega.Succeed(), "%s should be created", tt.label)
	}

	// Let initial settling finish (finalizer add, first status write, IP-cache
	// population) before snapshotting the steady-state resourceVersions.
	time.Sleep(4 * time.Second)

	baseline := make(map[string]string, len(targets))
	for _, tt := range targets {
		key := types.NamespacedName{Namespace: nsName, Name: tt.name}
		require.NoError(t, k8sClient.Get(ctx, key, tt.obj))
		baseline[tt.label] = tt.obj.GetResourceVersion()
	}

	// Over the next 6s the reconciler runs several full periodic reconciles
	// (SyncPeriod=2s). No child resource's resourceVersion may advance.
	g.Consistently(func() error {
		for _, tt := range targets {
			key := types.NamespacedName{Namespace: nsName, Name: tt.name}
			if err := k8sClient.Get(ctx, key, tt.obj); err != nil {
				return err
			}
			if got := tt.obj.GetResourceVersion(); got != baseline[tt.label] {
				return &churnError{label: tt.label, want: baseline[tt.label], got: got}
			}
		}
		return nil
	}, 6*time.Second, 500*time.Millisecond).Should(gomega.Succeed(),
		"apply* helpers must not rewrite child resources on steady-state reconciles")
}

type churnError struct {
	label    string
	want, got string
}

func (e *churnError) Error() string {
	return e.label + " resourceVersion churned: baseline=" + e.want + " now=" + e.got
}
