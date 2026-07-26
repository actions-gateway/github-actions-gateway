//go:build integration

package integration_test

import (
	"fmt"
	"testing"
	"time"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestGMC_V1Provisioning_EveryManagedChildIsOwnerReferenced sweeps the tenant
// namespace after a full v1 provisioning reconcile and asserts that *every*
// GMC-managed child carries a controller owner reference to the ActionsGateway
// (Q394).
//
// This is the catch-all counterpart to the per-helper table in
// apply_helpers_ownerref_test.go: that table pins the helpers it enumerates, but a
// twelfth child added later through a brand-new code path would simply not appear
// in it. Here the assertion is driven by what is actually on the cluster — the
// managed owner labels every child carries — so a new un-owned child fails without
// anyone remembering to extend a list.
//
// The owner reference is what makes teardown leak-proof: the reconcileDelete
// finalizer is the primary, ordered, fail-closed path, but an operator who force-
// removes it (the failure mode the troubleshooting guide warns against) would
// otherwise strand credentialed ServiceAccounts, the AGC RoleBinding, the egress
// NetworkPolicies, and the proxy HPA/PDB/Service in a namespace the tenant still
// controls. With the reference in place, cascade GC reclaims them.
func TestGMC_V1Provisioning_EveryManagedChildIsOwnerReferenced(t *testing.T) {
	const nsName = "team-ownerref-gc"
	createNamespace(t, nsName)
	createGitHubAppSecret(t, nsName, "github-app")

	ag := newActionsGateway("gc-gateway", nsName, "github-app")
	// Include a RunnerGroup so applyRunnerGroup is exercised too — it is the one
	// child reconcileDelete deletes explicitly *and* waits on, so a regression
	// there is easy to miss.
	ag.Spec.RunnerGroups = []agcv1alpha1.RunnerGroupSpec{{
		RunnerLabels: []string{"linux"},
		WorkerImage:  "worker:test",
		PodTemplate: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runner", Image: "runner:test"}}},
		},
	}}
	require.NoError(t, k8sClient.Create(ctx, ag))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, ag) })

	startGMCReconciler(t, nil)

	g := gomega.NewWithT(t)

	// Wait for provisioning to settle on its last-applied child (the RunnerGroup
	// is applied after the Deployments) before sweeping, so the sweep cannot race
	// a half-finished reconcile and pass vacuously.
	g.Eventually(func() error {
		var rgs agcv1alpha1.RunnerGroupList
		if err := k8sClient.List(ctx, &rgs, client.InNamespace(nsName)); err != nil {
			return err
		}
		if len(rgs.Items) == 0 {
			return fmt.Errorf("no RunnerGroup provisioned yet")
		}
		return nil
	}, 30*time.Second, 50*time.Millisecond).Should(gomega.Succeed())

	// Every kind the v1 reconciler can apply into the tenant namespace. Listing by
	// kind (rather than naming objects) is deliberate: it picks up children this
	// test does not know about.
	lists := []client.ObjectList{
		&corev1.ServiceAccountList{},
		&rbacv1.RoleBindingList{},
		&rbacv1.RoleList{},
		&corev1.ServiceList{},
		&networkingv1.NetworkPolicyList{},
		&policyv1.PodDisruptionBudgetList{},
		&autoscalingv2.HorizontalPodAutoscalerList{},
		&appsv1.DeploymentList{},
		&corev1.SecretList{},
		&agcv1alpha1.RunnerGroupList{},
	}

	// The owner labels are stamped by componentLabels on every managed child, so
	// they select exactly the GMC-provisioned set — and exclude the test's own
	// GitHub App Secret and the namespace itself (platform-owned, Q130).
	sel := client.MatchingLabels{
		"actions-gateway/owner-name": ag.Name,
		"actions-gateway/owner-ns":   ag.Namespace,
	}

	swept := 0
	for _, list := range lists {
		require.NoError(t, k8sClient.List(ctx, list, client.InNamespace(nsName), sel))
		items, err := listItems(list)
		require.NoError(t, err)
		for _, obj := range items {
			swept++
			kind := fmt.Sprintf("%T", obj)
			refs := obj.GetOwnerReferences()
			if !assert.Len(t, refs, 1, "%s %q must carry exactly one owner reference (Q394)", kind, obj.GetName()) {
				continue
			}
			assert.Equal(t, "ActionsGateway", refs[0].Kind, "%s %q owner must be the ActionsGateway", kind, obj.GetName())
			assert.Equal(t, ag.Name, refs[0].Name, "%s %q owner must be this gateway", kind, obj.GetName())
			assert.Equal(t, ag.UID, refs[0].UID, "%s %q owner UID must match", kind, obj.GetName())
			require.NotNil(t, refs[0].Controller, "%s %q owner must set controller", kind, obj.GetName())
			assert.True(t, *refs[0].Controller, "%s %q owner must be the controller reference", kind, obj.GetName())
		}
	}

	// Guard against the sweep silently matching nothing (e.g. a label rename):
	// v1 provisions at least the two ServiceAccounts, the RoleBinding, three
	// NetworkPolicies, two Services, the PDB, the HPA, two Deployments, the cert
	// Secrets, and the RunnerGroup.
	require.GreaterOrEqual(t, swept, 12, "sweep must actually find the provisioned children")
}

// listItems extracts the items of an ObjectList as client.Objects.
func listItems(list client.ObjectList) ([]client.Object, error) {
	var out []client.Object
	err := meta.EachListItem(list, func(o runtime.Object) error {
		obj, ok := o.(client.Object)
		if !ok {
			return fmt.Errorf("list item %T is not a client.Object", o)
		}
		out = append(out, obj)
		return nil
	})
	return out, err
}
