//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	v2 "github.com/actions-gateway/github-actions-gateway/api/v2"
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
// The pair is built with the two v2 webhooks bypassed so it is deterministic: Q791's
// guard catches a pair applied one object after the other in either order, so the
// ways into this state are an upgrade from a release that predates the guard, a
// window with the webhook uninstalled, and a concurrent create
// (TestV2_ScaleSetNameGuard_ConcurrentPairIsReported, Q1325). This test models the
// second, which produces the same stored objects as the other two.
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
	for _, rs := range []*v2.RunnerSet{rsA, rsB} {
		requireCreateEventually(t, rs)
		rs := rs
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rs) })
	}

	startActionsGatewayV2Reconciler(t)

	requireCollision := func(ns string, status metav1.ConditionStatus, reason string, contains ...string) *metav1.Condition {
		t.Helper()
		var cond *metav1.Condition
		require.Eventually(t, func() bool {
			var got v2.ActionsGateway
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw"}, &got); err != nil {
				return false
			}
			c := findCondition(got.Status.Conditions, v2.ConditionScaleSetNameCollision)
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
	condA := requireCollision(nsA, metav1.ConditionTrue, v2.ReasonScaleSetNameShared,
		"tenant-a-set", shared, "github.com/example-org")
	condB := requireCollision(nsB, metav1.ConditionTrue, v2.ReasonScaleSetNameShared,
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
			if e.Reason == v2.ReasonScaleSetNameShared && e.Type == corev1.EventTypeWarning {
				return true
			}
		}
		return false
	}, 30*time.Second, 200*time.Millisecond, "a Warning Event with reason %s must be recorded on the gateway", v2.ReasonScaleSetNameShared)

	// The operator's fix: give one side a distinct first label. Both conditions clear,
	// which is what makes the signal trustworthy — a condition that never goes back to
	// False cannot tell a resolved collision from an unwatched one.
	require.Eventually(t, func() bool {
		var rs v2.RunnerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: nsB, Name: "tenant-b-set"}, &rs); err != nil {
			return false
		}
		rs.Spec.RunnerLabels = []string{"tenant-b-scale-set", "windows"}
		return k8sClient.Update(ctx, &rs) == nil
	}, 10*time.Second, 100*time.Millisecond, "renaming tenant B's scale set must succeed")

	requireCollision(nsA, metav1.ConditionFalse, v2.ReasonScaleSetNamesUnique, "github.com/example-org")
	requireCollision(nsB, metav1.ConditionFalse, v2.ReasonScaleSetNamesUnique)
}

// bypassV2Admission scopes the v2 ActionsGateway and RunnerSet validating webhooks
// away from the namespaces labelled admissionBypassLabel, and restores their original
// selectors on cleanup. Both entries are failurePolicy=Fail, so a namespace the
// selector excludes is exactly a namespace where those objects were never validated —
// the pre-Q791 / webhook-uninstalled state this test needs to construct.
func bypassV2Admission(t *testing.T) {
	t.Helper()
	bypassAdmission(t, "vactionsgateway-v2.kb.io", "vrunnerset-v2.kb.io")
}

// bypassAdmission is bypassV2Admission for the named validating webhook entries.
func bypassAdmission(t *testing.T, webhooks ...string) {
	t.Helper()
	const configName = "validating-webhook-configuration"
	bypassed := map[string]bool{}
	for _, name := range webhooks {
		bypassed[name] = true
	}

	var original admissionv1.ValidatingWebhookConfiguration
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: configName}, &original))
	restore := map[string]*metav1.LabelSelector{}
	for i := range original.Webhooks {
		if bypassed[original.Webhooks[i].Name] {
			restore[original.Webhooks[i].Name] = original.Webhooks[i].NamespaceSelector
		}
	}
	require.Len(t, restore, len(bypassed), "every named webhook entry must be installed for the bypass to mean anything")

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

// TestV2_ScaleSetNameCollisionAcrossGitHubAlias covers Q1322 through the apiserver. A
// gateway spelling public GitHub www.github.com is addressed by the AGC as the same
// Actions service as github.com, so a scale-set name claimed under both spellings is
// one name. The scope key used to follow the spelling, so the guard compared the two
// as different orgs.
//
// The pair is stored with admission bypassed, which is the state an upgrade leaves:
// both objects were admitted by a release that did not fold the alias. Three things
// then have to hold. The reconciler reports the pair, so an operator can find it. A
// new claim on the shared name is refused. And an edit that leaves a stored claim
// alone is still admitted, for both tenants, so neither is locked out of its own
// RunnerSet by the other's spelling. The last block covers the gateway's own rule: a
// new gateway under an alias is refused, and the stored one stays editable.
func TestV2_ScaleSetNameCollisionAcrossGitHubAlias(t *testing.T) {
	const (
		nsPlain = "v2-ssalias-plain"
		nsAlias = "v2-ssalias-www"
		shared  = "alias-shared-scale-set"
	)
	bypassV2Admission(t)
	createNamespaceWithLabels(t, nsPlain, map[string]string{admissionBypassLabel: "true"})
	createNamespaceWithLabels(t, nsAlias, map[string]string{admissionBypassLabel: "true"})
	createGitHubAppSecret(t, nsPlain, "github-app")
	createGitHubAppSecret(t, nsAlias, "github-app")

	plain := newV2GatewayWired("gw", nsPlain, "github-app", "")
	plain.Spec.GitHubURL = "https://github.com/alias-org"
	alias := newV2GatewayWired("gw", nsAlias, "github-app", "")
	alias.Spec.GitHubURL = "https://www.github.com/alias-org"
	rsPlain := newScaleSetRunnerSet("plain-set", nsPlain, "gw", shared, "linux")
	rsAlias := newScaleSetRunnerSet("alias-set", nsAlias, "gw", shared, "linux")
	for _, obj := range []client.Object{plain, alias, rsPlain, rsAlias} {
		requireCreateEventually(t, obj)
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), obj) })
	}

	startActionsGatewayV2Reconciler(t)
	for _, ns := range []string{nsPlain, nsAlias} {
		require.Eventually(t, func() bool {
			var got v2.ActionsGateway
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gw"}, &got); err != nil {
				return false
			}
			c := findCondition(got.Status.Conditions, v2.ConditionScaleSetNameCollision)
			return c != nil && c.Status == metav1.ConditionTrue && c.Reason == v2.ReasonScaleSetNameShared &&
				strings.Contains(c.Message, "github.com/alias-org") && !strings.Contains(c.Message, "www.")
		}, 30*time.Second, 100*time.Millisecond,
			"%s/gw must report the pair under the folded scope github.com/alias-org", ns)
	}

	// Put both namespaces back under admission. The rejection below is also the
	// evidence that it is: the apiserver caches namespace labels and webhook
	// configuration, so the update that follows means nothing until a write here has
	// been seen to be refused.
	for _, name := range []string{nsPlain, nsAlias} {
		require.Eventually(t, func() bool {
			var ns corev1.Namespace
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &ns); err != nil {
				return false
			}
			delete(ns.Labels, admissionBypassLabel)
			return k8sClient.Update(ctx, &ns) == nil
		}, 10*time.Second, 100*time.Millisecond, "removing the bypass label from %s must succeed", name)
	}
	for _, ns := range []string{nsPlain, nsAlias} {
		var last error
		require.Eventually(t, func() bool {
			thief := newScaleSetRunnerSet("second-claim", ns, "gw", shared)
			last = k8sClient.Create(ctx, thief)
			if last == nil {
				_ = k8sClient.Delete(ctx, thief)
			}
			return last != nil && strings.Contains(last.Error(), "is already")
		}, 20*time.Second, 200*time.Millisecond,
			"a new claim on the shared name in %s must be refused once admission is back; last error: %v", ns, last)
	}

	for _, key := range []types.NamespacedName{{Namespace: nsPlain, Name: "plain-set"}, {Namespace: nsAlias, Name: "alias-set"}} {
		var rs v2.RunnerSet
		require.NoError(t, k8sClient.Get(ctx, key, &rs))
		rs.Spec.RunnerLabels = append(rs.Spec.RunnerLabels, "unrelated-edit")
		require.NoError(t, k8sClient.Update(ctx, &rs),
			"%s: an edit that keeps the first label and the gateway must be admitted", key)
	}

	// The exemption is for an unchanged claim only: moving a set that holds another
	// name onto the shared one is a new claim.
	mover := newScaleSetRunnerSet("mover", nsAlias, "gw", "alias-mover-own-name")
	require.NoError(t, k8sClient.Create(ctx, mover))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), mover) })
	mover.Spec.RunnerLabels = []string{shared}
	err := k8sClient.Update(ctx, mover)
	require.Error(t, err, "an update that moves a set onto the shared name must be refused")
	assert.Contains(t, err.Error(), "github.com/alias-org")

	// The gateway half. A new gateway may not take an alias spelling, and the one
	// stored under it stays editable: githubURL is immutable, so a rule that ran on
	// update would refuse every later write to it.
	fresh := newV2GatewayWired("gw-new", nsAlias, "github-app", "")
	fresh.Spec.GitHubURL = "https://api.github.com/alias-new-org"
	err = k8sClient.Create(ctx, fresh)
	if err == nil {
		_ = k8sClient.Delete(ctx, fresh)
	}
	require.Error(t, err, "a new gateway spelling public GitHub api.github.com must be refused")
	assert.Contains(t, err.Error(), "alias of public GitHub")
	assert.Contains(t, err.Error(), "https://github.com/alias-new-org")

	var stored v2.ActionsGateway
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: nsAlias, Name: "gw"}, &stored))
	require.Equal(t, "https://www.github.com/alias-org", stored.Spec.GitHubURL)
	edited := stored.DeepCopy()
	edited.Annotations = map[string]string{"example.com/unrelated-edit": "true"}
	require.NoError(t, k8sClient.Patch(ctx, edited, client.MergeFrom(&stored)),
		"an edit to a gateway stored under the alias must be admitted")
}
