//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	v2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
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

// rsWithTierPriorityClass returns a RunnerSet naming the given PriorityClass in
// priorityTiers — the tenant-authored v2 tier route to a worker pod's
// priorityClassName (Q289): the AGC stamps the matched tier's class onto the pod,
// overriding the template.
func rsWithTierPriorityClass(name, ns, priorityClassName string) *agcv2alpha1.RunnerSet {
	return &agcv2alpha1.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agcv2alpha1.RunnerSetSpec{
			GatewayRef:   agcv2alpha1.ObjectRef{Name: "gateway"},
			RunnerLabels: []string{"self-hosted"},
			PriorityTiers: []agcv2alpha1.PriorityTier{
				{PriorityClassName: priorityClassName, Threshold: 5},
			},
		},
	}
}

// startPriorityClassAllowlistReconciler starts a PriorityClassAllowlistReconciler
// against the envtest apiserver for the duration of the test, wired to the given
// shared worker allowlist and watching the named cluster-scoped
// PriorityClassAllowlist. The infra allowlist is a private paired instance unless
// the caller needs it (startPriorityClassAllowlistReconcilerPair).
func startPriorityClassAllowlistReconciler(t *testing.T, al *allowlist.PriorityClassAllowlist, name string) {
	t.Helper()
	infra := allowlist.New(nil)
	allowlist.Pair(al, infra)
	startPriorityClassAllowlistReconcilerPair(t, al, infra, name)
}

// startPriorityClassAllowlistReconcilerPair is the two-allowlist form: both the
// worker and infra dynamic halves come from the one watched object (Q188/Q298).
func startPriorityClassAllowlistReconcilerPair(t *testing.T, al, infra *allowlist.PriorityClassAllowlist, name string) {
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
		Client:         mgr.GetClient(),
		Name:           name,
		Allowlist:      al,
		InfraAllowlist: infra,
	}).SetupWithManager(mgr)
	require.NoError(t, err)

	go func() { _ = mgr.Start(mgrCtx) }()
}

// waitForAllowed polls until the allowlist reports want for name, or fails the
// test. The watch is asynchronous, so enforcement only changes once the
// reconciler has observed the PriorityClassAllowlist event.
func waitForAllowed(t *testing.T, al *allowlist.PriorityClassAllowlist, name string, want bool) {
	t.Helper()
	err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true,
		func(context.Context) (bool, error) {
			return al.Allowed(name) == want, nil
		})
	require.NoErrorf(t, err, "allowlist never reached Allowed(%q)==%v; effective=%v", name, want, al.Names())
}

// TestIntegration_PriorityClassAllowlist_Watch exercises Q188 end to end against a
// real apiserver: the watched PriorityClassAllowlist augments the static flag
// allowlist without a restart, enforcement follows the live set, and a deleted
// object fails safe back to the static flag — never silently widening the
// guardrail.
//
// Since Q492 the watched object is a cluster-scoped CR rather than a ConfigMap, so
// the CRD schema rejects a malformed entry at write time; the reconciler's
// wholesale-rejection fail-safe is covered by its unit tests, which can construct
// an object the apiserver would refuse.
func TestIntegration_PriorityClassAllowlist_Watch(t *testing.T) {
	const (
		pcaName      = "gmc-q188-priority-class-allowlist"
		staticClass  = "runner-standard"
		dynamicClass = "runner-bursty"
	)

	// Shared allowlist seeded with the static flag value; dynamic half starts empty.
	al := allowlist.New([]string{staticClass})
	validator := webhookv1alpha1.NewActionsGatewayCustomValidatorWithAllowlist("", al)

	startPriorityClassAllowlistReconciler(t, al, pcaName)

	// Before the object exists: only the static class is allowed (fail-safe
	// default — no object, flag-only behavior).
	require.True(t, al.Allowed(staticClass), "static class must be allowed at startup")
	require.False(t, al.Allowed(dynamicClass), "no PriorityClassAllowlist means no dynamic additions")
	_, err := validator.ValidateCreate(ctx, agWithPriorityTier("static-ok", "team-a", staticClass))
	require.NoError(t, err)
	_, err = validator.ValidateCreate(ctx, agWithPriorityTier("dyn-rejected", "team-a", dynamicClass))
	require.Error(t, err, "the dynamic class must be rejected before the object is applied")

	// Apply an object adding the dynamic class — it must take effect without
	// restarting anything, and must NOT drop the static class.
	pca := &v2beta1.PriorityClassAllowlist{
		ObjectMeta: metav1.ObjectMeta{Name: pcaName},
		Spec:       v2beta1.PriorityClassAllowlistSpec{AllowedPriorityClasses: []string{dynamicClass}},
	}
	require.NoError(t, k8sClient.Create(ctx, pca))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pca) })
	waitForAllowed(t, al, dynamicClass, true)

	require.True(t, al.Allowed(staticClass), "static class must survive a dynamic augmentation")
	_, err = validator.ValidateCreate(ctx, agWithPriorityTier("dyn-ok", "team-a", dynamicClass))
	require.NoError(t, err, "the CR-sourced class must now be admitted")

	// The CRD schema is itself part of the guardrail: an invalid PriorityClass name
	// must be refused at write time, so a malformed value can never reach the
	// reconciler or the VAP that shares this object as its paramKind.
	bad := pca.DeepCopy()
	bad.Spec.AllowedPriorityClasses = []string{"Not A Valid Name!"}
	require.Error(t, k8sClient.Update(ctx, bad),
		"the CRD pattern must reject a non-DNS-1123 PriorityClass name")
	assert.True(t, al.Allowed(dynamicClass), "a refused write must not disturb the live allowlist")

	// Narrow the object: enforcement follows the live set down as well as up.
	narrowed := &v2beta1.PriorityClassAllowlist{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: pcaName}, narrowed))
	narrowed.Spec.AllowedPriorityClasses = nil
	require.NoError(t, k8sClient.Update(ctx, narrowed))
	waitForAllowed(t, al, dynamicClass, false)
	assert.True(t, al.Allowed(staticClass), "narrowing the dynamic set must not strip the static class")
	_, err = validator.ValidateCreate(ctx, agWithPriorityTier("static-still-ok", "team-a", staticClass))
	require.NoError(t, err, "the static flag allowlist must remain in force")

	// Restore, then delete the object entirely: fail safe back to the static flag.
	restored := &v2beta1.PriorityClassAllowlist{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: pcaName}, restored))
	restored.Spec.AllowedPriorityClasses = []string{dynamicClass}
	require.NoError(t, k8sClient.Update(ctx, restored))
	waitForAllowed(t, al, dynamicClass, true)

	require.NoError(t, k8sClient.Delete(ctx, restored))
	waitForAllowed(t, al, dynamicClass, false)
	require.True(t, al.Allowed(staticClass), "the static class must remain after the object is deleted")
}

// TestIntegration_PriorityClassAllowlist_PodTemplateSurface is the Q289 regression
// test against a real apiserver. Q132 gated v1 priorityTiers but left the OTHER
// tenant-reachable routes to a worker pod's priorityClassName ungated: the full
// podTemplate.spec, which the AGC copies verbatim into the pod (v1
// runnerGroups[].podTemplate and the v2 RunnerTemplate.podTemplate), and the v2
// RunnerSet's own priorityTiers, whose matched tier the AGC stamps over the
// template. The same platform allowlist — including its CR-sourced dynamic
// half — must govern all of them.
//
// The webhook server runs without TLS in envtest, so the validators are called
// directly; the allowlist they read is the live one the reconciler maintains.
func TestIntegration_PriorityClassAllowlist_PodTemplateSurface(t *testing.T) {
	const (
		ns           = "gmc-q289"
		pcaName      = "gmc-q289-priority-class-allowlist"
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
	rsValidator := &webhookv2alpha1.RunnerSetCustomValidator{PriorityClasses: al}

	startPriorityClassAllowlistReconciler(t, al, pcaName)

	// The headline claim: a tenant cannot preempt another tenant. No surface
	// admits system-cluster-critical, under the static allowlist as configured.
	_, err := v1Validator.ValidateCreate(ctx, agWithPodTemplatePriorityClass("v1-escalate", "team-a", escalation))
	require.Error(t, err, "v1 runnerGroups[].podTemplate must not admit %s", escalation)
	assert.Contains(t, err.Error(), "podTemplate.spec.priorityClassName")

	_, err = v2Validator.ValidateCreate(ctx, rtWithPodTemplatePriorityClass("v2-escalate", "team-a", escalation))
	require.Error(t, err, "v2 RunnerTemplate.podTemplate must not admit %s", escalation)
	assert.Contains(t, err.Error(), "podTemplate.spec.priorityClassName")

	_, err = rsValidator.ValidateCreate(ctx, rsWithTierPriorityClass("rs-escalate", "team-a", escalation))
	require.Error(t, err, "v2 RunnerSet.priorityTiers must not admit %s", escalation)
	assert.Contains(t, err.Error(), "priorityTiers[0]")

	// An unset priorityClassName stays admissible on both — the secure default must
	// not forbid ordinary, unprioritized worker pods.
	_, err = v1Validator.ValidateCreate(ctx, agWithPodTemplatePriorityClass("v1-unset", "team-a", ""))
	require.NoError(t, err)
	_, err = v2Validator.ValidateCreate(ctx, rtWithPodTemplatePriorityClass("v2-unset", "team-a", ""))
	require.NoError(t, err)

	// The allowlisted static class is admitted on every surface.
	_, err = v1Validator.ValidateCreate(ctx, agWithPodTemplatePriorityClass("v1-static", "team-a", staticClass))
	require.NoError(t, err)
	_, err = v2Validator.ValidateCreate(ctx, rtWithPodTemplatePriorityClass("v2-static", "team-a", staticClass))
	require.NoError(t, err)
	_, err = rsValidator.ValidateCreate(ctx, rsWithTierPriorityClass("rs-static", "team-a", staticClass))
	require.NoError(t, err)

	// The dynamic class is rejected until the platform admin adds it to the watched
	// PriorityClassAllowlist — and then admitted on every surface, with no restart.
	_, err = v1Validator.ValidateCreate(ctx, agWithPodTemplatePriorityClass("v1-dyn-early", "team-a", dynamicClass))
	require.Error(t, err)
	_, err = v2Validator.ValidateCreate(ctx, rtWithPodTemplatePriorityClass("v2-dyn-early", "team-a", dynamicClass))
	require.Error(t, err)
	_, err = rsValidator.ValidateCreate(ctx, rsWithTierPriorityClass("rs-dyn-early", "team-a", dynamicClass))
	require.Error(t, err)

	pca := &v2beta1.PriorityClassAllowlist{
		ObjectMeta: metav1.ObjectMeta{Name: pcaName},
		Spec:       v2beta1.PriorityClassAllowlistSpec{AllowedPriorityClasses: []string{dynamicClass}},
	}
	require.NoError(t, k8sClient.Create(ctx, pca))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pca) })
	waitForAllowed(t, al, dynamicClass, true)

	_, err = v1Validator.ValidateCreate(ctx, agWithPodTemplatePriorityClass("v1-dyn", "team-a", dynamicClass))
	require.NoError(t, err, "the CR-sourced class must reach the v1 podTemplate surface")
	_, err = v2Validator.ValidateCreate(ctx, rtWithPodTemplatePriorityClass("v2-dyn", "team-a", dynamicClass))
	require.NoError(t, err, "the CR-sourced class must reach the v2 podTemplate surface")
	_, err = rsValidator.ValidateCreate(ctx, rsWithTierPriorityClass("rs-dyn", "team-a", dynamicClass))
	require.NoError(t, err, "the CR-sourced class must reach the v2 RunnerSet tier surface")

	// A CR widening the allowlist must never reach the escalation class.
	_, err = v2Validator.ValidateCreate(ctx, rtWithPodTemplatePriorityClass("v2-escalate-2", "team-a", escalation))
	require.Error(t, err, "%s must stay rejected regardless of the dynamic set", escalation)
	_, err = rsValidator.ValidateCreate(ctx, rsWithTierPriorityClass("rs-escalate-2", "team-a", escalation))
	require.Error(t, err, "%s must stay rejected regardless of the dynamic set", escalation)
}
