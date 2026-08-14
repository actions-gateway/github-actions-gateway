//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// admissionBypassLabel marks the namespaces whose v2 ActionsGateway/RunnerSet writes
// skip the validating webhooks, so a test can build the state Q791's admission guard
// makes unreachable through a validated write.
const admissionBypassLabel = "actions-gateway.test/skip-v2-admission"

// TestV2_ActionsGateway_ScaleSetNameCollision verifies Q849: a scale-set name shared
// across two tenants in one GitHub scope is reported on both gateways at reconcile,
// not only rejected at admission.
//
// The pair has to be built with the two v2 webhooks bypassed, and that is the finding
// rather than a testing convenience: Q791's guard is complete against validated writes
// in every apply order, so the only ways into this state are an upgrade from a release
// that predates the guard and a window with the webhook uninstalled. This test models
// the second, which produces the same stored objects as the first.
//
// The bypass is a namespaceSelector on the two webhook entries keyed to a label only
// this test's namespaces carry, rather than deleting the entries: a restore that does
// not run leaves every other namespace in the suite still validated.
func TestV2_ActionsGateway_ScaleSetNameCollision(t *testing.T) {
	const (
		nsA    = "v2-ssname-a"
		nsB    = "v2-ssname-b"
		shared = "shared-scale-set"
	)
	bypassV2Admission(t)
	createNamespaceWithLabels(t, nsA, map[string]string{admissionBypassLabel: "true"})
	createNamespaceWithLabels(t, nsB, map[string]string{admissionBypassLabel: "true"})
	createGitHubAppSecret(t, nsA, "github-app")
	createGitHubAppSecret(t, nsB, "github-app")

	// Two gateways in different namespaces, both bound to https://github.com/example-org
	// (newV2GatewayWired's URL) — one scale-set namespace at GitHub, two Kubernetes
	// namespaces. Direct egress keeps the fixture to the objects under test.
	for _, ns := range []string{nsA, nsB} {
		ag := newV2GatewayWired("gw", ns, "github-app", "")
		requireCreateEventually(t, ag)
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ag) })
	}

	// Each tenant claims the same first runnerLabel: the scale-set name at GitHub. The
	// second labels differ, which changes nothing — only the first is the name (Q726).
	rsA := newScaleSetRunnerSet("tenant-a-set", nsA, "gw", shared, "linux")
	rsB := newScaleSetRunnerSet("tenant-b-set", nsB, "gw", shared, "windows")
	for _, rs := range []*v2alpha1.RunnerSet{rsA, rsB} {
		requireCreateEventually(t, rs)
		rs := rs
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rs) })
	}

	startActionsGatewayV2Reconciler(t)

	requireCollision := func(ns string, status metav1.ConditionStatus, reason string, contains ...string) *metav1.Condition {
		t.Helper()
		var cond *metav1.Condition
		require.Eventually(t, func() bool {
			var got v2alpha1.ActionsGateway
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw"}, &got); err != nil {
				return false
			}
			c := findCondition(got.Status.Conditions, v2alpha1.ConditionScaleSetNameCollision)
			if c == nil || c.Status != status || c.Reason != reason {
				return false
			}
			for _, s := range contains {
				if !strings.Contains(c.Message, s) {
					return false
				}
			}
			cond = c
			return true
		}, 30*time.Second, 100*time.Millisecond,
			"%s/gw ScaleSetNameCollision=%s/%s message must contain %v", ns, status, reason, contains)
		return cond
	}

	// Both sides report it. Neither is "the offender" — GAG cannot pick which tenant
	// loses the name, so both operators get the signal.
	condA := requireCollision(nsA, metav1.ConditionTrue, v2alpha1.ReasonScaleSetNameShared,
		"tenant-a-set", shared, "github.com/example-org")
	condB := requireCollision(nsB, metav1.ConditionTrue, v2alpha1.ReasonScaleSetNameShared,
		"tenant-b-set", shared)

	// Non-enumeration (Q791's second boundary property, carried into status): the
	// gateway's conditions are readable by its own tenant, so naming the holder would
	// let either tenant probe the other's namespace and label usage. The GMC log keeps
	// the full pair for the platform admin.
	assert.NotContains(t, condA.Message, nsB, "tenant A's condition must not name tenant B's namespace")
	assert.NotContains(t, condA.Message, "tenant-b-set")
	assert.NotContains(t, condB.Message, nsA, "tenant B's condition must not name tenant A's namespace")
	assert.NotContains(t, condB.Message, "tenant-a-set")

	// A Warning Event fires on the transition, so the collision reaches `kubectl
	// describe` and an event pipeline, not only the condition.
	require.Eventually(t, func() bool {
		var events corev1.EventList
		if err := k8sClient.List(ctx, &events, client.InNamespace(nsA)); err != nil {
			return false
		}
		for i := range events.Items {
			e := &events.Items[i]
			if e.Reason == v2alpha1.ReasonScaleSetNameShared && e.Type == corev1.EventTypeWarning {
				return true
			}
		}
		return false
	}, 30*time.Second, 200*time.Millisecond, "a Warning Event with reason %s must be recorded on the gateway", v2alpha1.ReasonScaleSetNameShared)

	// The operator's fix: give one side a distinct first label. Both conditions clear,
	// which is what makes the signal trustworthy — a condition that never goes back to
	// False cannot tell a resolved collision from an unwatched one.
	require.Eventually(t, func() bool {
		var rs v2alpha1.RunnerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsB, Name: "tenant-b-set"}, &rs); err != nil {
			return false
		}
		rs.Spec.RunnerLabels = []string{"tenant-b-scale-set", "windows"}
		return k8sClient.Update(ctx, &rs) == nil
	}, 10*time.Second, 100*time.Millisecond, "renaming tenant B's scale set must succeed")

	requireCollision(nsA, metav1.ConditionFalse, v2alpha1.ReasonScaleSetNamesUnique, "github.com/example-org")
	requireCollision(nsB, metav1.ConditionFalse, v2alpha1.ReasonScaleSetNamesUnique)
}

// bypassV2Admission scopes the v2 ActionsGateway and RunnerSet validating webhooks
// away from the namespaces labelled admissionBypassLabel, and restores their original
// selectors on cleanup. Both entries are failurePolicy=Fail, so a namespace the
// selector excludes is exactly a namespace where those objects were never validated —
// the pre-Q791 / webhook-uninstalled state this test needs to construct.
func bypassV2Admission(t *testing.T) {
	t.Helper()
	const configName = "validating-webhook-configuration"
	bypassed := map[string]bool{
		"vactionsgateway-v2alpha1.kb.io": true,
		"vrunnerset-v2alpha1.kb.io":      true,
	}

	var original admissionv1.ValidatingWebhookConfiguration
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: configName}, &original))
	restore := map[string]*metav1.LabelSelector{}
	for i := range original.Webhooks {
		if bypassed[original.Webhooks[i].Name] {
			restore[original.Webhooks[i].Name] = original.Webhooks[i].NamespaceSelector
		}
	}
	require.Len(t, restore, len(bypassed), "both v2 webhook entries must be installed for the bypass to mean anything")

	patch := func(selector func(string) *metav1.LabelSelector) {
		require.Eventually(t, func() bool {
			var cfg admissionv1.ValidatingWebhookConfiguration
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: configName}, &cfg); err != nil {
				return false
			}
			for i := range cfg.Webhooks {
				if bypassed[cfg.Webhooks[i].Name] {
					cfg.Webhooks[i].NamespaceSelector = selector(cfg.Webhooks[i].Name)
				}
			}
			return k8sClient.Update(ctx, &cfg) == nil
		}, 10*time.Second, 100*time.Millisecond, "patching %s must succeed", configName)
	}

	t.Cleanup(func() { patch(func(name string) *metav1.LabelSelector { return restore[name] }) })
	patch(func(string) *metav1.LabelSelector {
		return &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key:      admissionBypassLabel,
			Operator: metav1.LabelSelectorOpDoesNotExist,
		}}}
	})
}

// requireCreateEventually creates obj, retrying while admission still rejects it: the
// apiserver caches webhook configuration, so a namespaceSelector patch does not take
// effect on the very next request. Retrying here rather than sleeping keeps the test
// honest about what it is waiting for.
func requireCreateEventually(t *testing.T, obj client.Object) {
	t.Helper()
	var last error
	require.Eventually(t, func() bool {
		last = k8sClient.Create(ctx, obj)
		return last == nil
	}, 30*time.Second, 200*time.Millisecond, "creating %s/%s must eventually be admitted; last error: %v",
		obj.GetNamespace(), obj.GetName(), last)
}
