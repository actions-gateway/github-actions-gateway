//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	webhookv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// startPriorityClassAllowlistReconciler starts a PriorityClassAllowlistReconciler
// against the envtest apiserver for the duration of the test, wired to the given
// shared allowlist and watching the named ConfigMap in the given namespace.
func startPriorityClassAllowlistReconciler(t *testing.T, al *allowlist.PriorityClassAllowlist, namespace, cmName string) {
	t.Helper()
	mgrCtx, mgrCancel := context.WithCancel(ctx)
	t.Cleanup(mgrCancel)

	skipNameValidation := true
	mgr, err := ctrl.NewManager(testEnv.Config, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
	})
	require.NoError(t, err)

	err = (&controller.PriorityClassAllowlistReconciler{
		Client:        mgr.GetClient(),
		ConfigMapName: cmName,
		Namespace:     namespace,
		Allowlist:     al,
	}).SetupWithManager(mgr)
	require.NoError(t, err)

	go func() { _ = mgr.Start(mgrCtx) }()
}

// waitForAllowed polls until the allowlist reports want for name, or fails the
// test. The watch is asynchronous, so enforcement only changes once the
// reconciler has observed the ConfigMap event.
func waitForAllowed(t *testing.T, al *allowlist.PriorityClassAllowlist, name string, want bool) {
	t.Helper()
	err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true,
		func(context.Context) (bool, error) {
			return al.Allowed(name) == want, nil
		})
	require.NoErrorf(t, err, "allowlist never reached Allowed(%q)==%v; effective=%v", name, want, al.Names())
}

// TestIntegration_PriorityClassAllowlist_ConfigMapWatch exercises Q188 end to end
// against a real apiserver: a watched ConfigMap augments the static flag
// allowlist without a restart, enforcement follows the live set, and a deleted or
// malformed ConfigMap fails safe back to the static flag — never silently
// widening the guardrail.
func TestIntegration_PriorityClassAllowlist_ConfigMapWatch(t *testing.T) {
	const (
		ns           = "gmc-q188"
		cmName       = "priority-class-allowlist"
		staticClass  = "runner-standard"
		dynamicClass = "runner-bursty"
	)

	// The GMC's own namespace, where only a platform admin can write the ConfigMap.
	createNamespace(t, ns)

	// Shared allowlist seeded with the static flag value; dynamic half starts empty.
	al := allowlist.New([]string{staticClass})
	validator := webhookv1alpha1.NewActionsGatewayCustomValidatorWithAllowlist("", al)

	startPriorityClassAllowlistReconciler(t, al, ns, cmName)

	// Before any ConfigMap exists: only the static class is allowed (fail-safe
	// default — no ConfigMap, flag-only behavior).
	require.True(t, al.Allowed(staticClass), "static class must be allowed at startup")
	require.False(t, al.Allowed(dynamicClass), "no ConfigMap means no dynamic additions")
	_, err := validator.ValidateCreate(ctx, agWithPriorityTier("static-ok", "team-a", staticClass))
	require.NoError(t, err)
	_, err = validator.ValidateCreate(ctx, agWithPriorityTier("dyn-rejected", "team-a", dynamicClass))
	require.Error(t, err, "the dynamic class must be rejected before the ConfigMap is applied")

	// Apply a valid ConfigMap adding the dynamic class — it must take effect
	// without restarting anything, and must NOT drop the static class.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: ns},
		Data:       map[string]string{controller.PriorityClassAllowlistConfigMapKey: dynamicClass},
	}
	require.NoError(t, k8sClient.Create(ctx, cm))
	waitForAllowed(t, al, dynamicClass, true)

	require.True(t, al.Allowed(staticClass), "static class must survive a dynamic augmentation")
	_, err = validator.ValidateCreate(ctx, agWithPriorityTier("dyn-ok", "team-a", dynamicClass))
	require.NoError(t, err, "the ConfigMap-sourced class must now be admitted")

	// Corrupt the ConfigMap (an invalid PriorityClass name): the reconciler must
	// fail safe to the static flag allowlist — the dynamic class is dropped, the
	// static class stays, and the malformed value never widens the allowlist.
	updateConfigMap(t, ns, cmName, map[string]string{
		controller.PriorityClassAllowlistConfigMapKey: "Not A Valid Name!",
	})
	waitForAllowed(t, al, dynamicClass, false)
	assert.True(t, al.Allowed(staticClass), "a malformed ConfigMap must not strip the static class")
	_, err = validator.ValidateCreate(ctx, agWithPriorityTier("dyn-rejected-again", "team-a", dynamicClass))
	require.Error(t, err, "after a malformed ConfigMap, the dynamic class must be rejected again (fail-safe)")
	_, err = validator.ValidateCreate(ctx, agWithPriorityTier("static-still-ok", "team-a", staticClass))
	require.NoError(t, err, "the static flag allowlist must remain in force on fail-safe")

	// Repair the ConfigMap: enforcement recovers without a restart.
	updateConfigMap(t, ns, cmName, map[string]string{
		controller.PriorityClassAllowlistConfigMapKey: dynamicClass,
	})
	waitForAllowed(t, al, dynamicClass, true)

	// Delete the ConfigMap entirely: fail safe back to the static flag allowlist.
	require.NoError(t, k8sClient.Delete(ctx, cm))
	waitForAllowed(t, al, dynamicClass, false)
	require.True(t, al.Allowed(staticClass), "the static class must remain after the ConfigMap is deleted")
}

func updateConfigMap(t *testing.T, ns, name string, data map[string]string) {
	t.Helper()
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &cm))
	cm.Data = data
	require.NoError(t, k8sClient.Update(ctx, &cm))
}
