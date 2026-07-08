//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	gmcv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/api/v1alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/controller"
	webhookv1alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/v1alpha1"
	webhookv2alpha1 "github.com/actions-gateway/github-actions-gateway/gmc/internal/webhook/v2alpha1"
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

// agWithPodTemplatePriorityClass returns an ActionsGateway whose single RunnerGroup
// names the given PriorityClass in podTemplate.spec — the v1 route to a worker pod's
// priorityClassName that Q132 left ungated (Q289). An empty name leaves it unset.
func agWithPodTemplatePriorityClass(name, ns, priorityClassName string) *gmcv1alpha1.ActionsGateway {
	ag := newActionsGateway(name, ns, "github-app")
	ag.Spec.RunnerGroups = []agcv1alpha1.RunnerGroupSpec{
		{
			MaxListeners: 1,
			RunnerLabels: []string{"self-hosted"},
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					PriorityClassName: priorityClassName,
					Containers:        []corev1.Container{{Name: "runner", Image: "runner:test"}},
				},
			},
		},
	}
	return ag
}

// rtWithPodTemplatePriorityClass returns a namespaced RunnerTemplate naming the given
// PriorityClass in podTemplate.spec — the v2 route (Q289). Empty leaves it unset.
func rtWithPodTemplatePriorityClass(name, ns, priorityClassName string) *agcv2alpha1.RunnerTemplate {
	rt := runnerTemplateWithContainer(ns, name, corev1.Container{Name: "runner", Image: "runner:test"})
	rt.Spec.PodTemplate.Spec.PriorityClassName = priorityClassName
	return rt
}

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

// TestIntegration_PriorityClassAllowlist_PodTemplateSurface is the Q289 regression
// test against a real apiserver. Q132 gated priorityTiers but left the OTHER route to
// a worker pod's priorityClassName ungated: the full podTemplate.spec, which the AGC
// copies verbatim into the pod. The same platform allowlist — including its
// ConfigMap-sourced dynamic half — must now govern both v1 runnerGroups[].podTemplate
// and the v2 RunnerTemplate.podTemplate.
//
// The webhook server runs without TLS in envtest, so the validators are called
// directly; the allowlist they read is the live one the reconciler maintains.
func TestIntegration_PriorityClassAllowlist_PodTemplateSurface(t *testing.T) {
	const (
		ns           = "gmc-q289"
		cmName       = "priority-class-allowlist"
		staticClass  = "runner-standard"
		dynamicClass = "runner-bursty"
		// Present in every Kubernetes cluster, value 2000000000, preemptionPolicy
		// PreemptLowerPriority — and NOT restricted to kube-system. The escalation a
		// tenant could reach with zero setup before this gate existed.
		escalation = "system-cluster-critical"
	)

	createNamespace(t, ns)

	al := allowlist.New([]string{staticClass})
	v1Validator := webhookv1alpha1.NewActionsGatewayCustomValidatorWithAllowlist("", al)
	v2Validator := &webhookv2alpha1.RunnerTemplateCustomValidator{PriorityClasses: al}

	startPriorityClassAllowlistReconciler(t, al, ns, cmName)

	// The headline claim: a tenant cannot preempt another tenant. Neither surface
	// admits system-cluster-critical, under the static allowlist as configured.
	_, err := v1Validator.ValidateCreate(ctx, agWithPodTemplatePriorityClass("v1-escalate", "team-a", escalation))
	require.Error(t, err, "v1 runnerGroups[].podTemplate must not admit %s", escalation)
	assert.Contains(t, err.Error(), "podTemplate.spec.priorityClassName")

	_, err = v2Validator.ValidateCreate(ctx, rtWithPodTemplatePriorityClass("v2-escalate", "team-a", escalation))
	require.Error(t, err, "v2 RunnerTemplate.podTemplate must not admit %s", escalation)
	assert.Contains(t, err.Error(), "podTemplate.spec.priorityClassName")

	// An unset priorityClassName stays admissible on both — the secure default must
	// not forbid ordinary, unprioritized worker pods.
	_, err = v1Validator.ValidateCreate(ctx, agWithPodTemplatePriorityClass("v1-unset", "team-a", ""))
	require.NoError(t, err)
	_, err = v2Validator.ValidateCreate(ctx, rtWithPodTemplatePriorityClass("v2-unset", "team-a", ""))
	require.NoError(t, err)

	// The allowlisted static class is admitted on both.
	_, err = v1Validator.ValidateCreate(ctx, agWithPodTemplatePriorityClass("v1-static", "team-a", staticClass))
	require.NoError(t, err)
	_, err = v2Validator.ValidateCreate(ctx, rtWithPodTemplatePriorityClass("v2-static", "team-a", staticClass))
	require.NoError(t, err)

	// The dynamic class is rejected until the platform admin adds it to the watched
	// ConfigMap — and then admitted on both surfaces, with no restart.
	_, err = v1Validator.ValidateCreate(ctx, agWithPodTemplatePriorityClass("v1-dyn-early", "team-a", dynamicClass))
	require.Error(t, err)
	_, err = v2Validator.ValidateCreate(ctx, rtWithPodTemplatePriorityClass("v2-dyn-early", "team-a", dynamicClass))
	require.Error(t, err)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: ns},
		Data:       map[string]string{controller.PriorityClassAllowlistConfigMapKey: dynamicClass},
	}
	require.NoError(t, k8sClient.Create(ctx, cm))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, cm) })
	waitForAllowed(t, al, dynamicClass, true)

	_, err = v1Validator.ValidateCreate(ctx, agWithPodTemplatePriorityClass("v1-dyn", "team-a", dynamicClass))
	require.NoError(t, err, "the ConfigMap-sourced class must reach the v1 podTemplate surface")
	_, err = v2Validator.ValidateCreate(ctx, rtWithPodTemplatePriorityClass("v2-dyn", "team-a", dynamicClass))
	require.NoError(t, err, "the ConfigMap-sourced class must reach the v2 podTemplate surface")

	// A ConfigMap widening the allowlist must never reach the escalation class.
	_, err = v2Validator.ValidateCreate(ctx, rtWithPodTemplatePriorityClass("v2-escalate-2", "team-a", escalation))
	require.Error(t, err, "%s must stay rejected regardless of the dynamic set", escalation)
}

func updateConfigMap(t *testing.T, ns, name string, data map[string]string) {
	t.Helper()
	var cm corev1.ConfigMap
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &cm))
	cm.Data = data
	require.NoError(t, k8sClient.Update(ctx, &cm))
}
