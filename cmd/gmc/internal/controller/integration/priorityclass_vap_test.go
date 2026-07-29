//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	agcv1alpha1 "github.com/actions-gateway/github-actions-gateway/agc/api/v1alpha1"
	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	agcv2beta1 "github.com/actions-gateway/github-actions-gateway/api/v2beta1"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// priorityClassGuardManifestPath is the real shipped policy — loading it (rather
// than reconstructing the objects in Go) means a CEL typo or a wrong
// matchCondition in the deployed artifact is caught here.
const priorityClassGuardManifestPath = "../../../config/admission-policy/priorityclass-allowlist-guard.yaml"

// vapParamName is the fixed name the shipped manifest's binding paramRef points
// at. Since Q492 the param is a CLUSTER-scoped PriorityClassAllowlist, so it has
// no namespace; vapProbeNamespace is only where this test's probe RunnerGroups go.
const (
	vapParamName      = "gag-priorityclass-allowlist"
	vapProbeNamespace = "gmc-system"
)

// installPriorityClassGuard applies the real VAP + binding + default (empty)
// param object. Objects are installed idempotently; the param's contents are
// owned (and mutated) by the test that calls this.
//
// The policy is torn down again when the calling test finishes: since Q323 it
// matches v2 runnersets/runnertemplates too, so leaving it behind (with whatever
// allowlist the last subtest set) would deny class-naming v2 writes in unrelated
// later tests (e.g. the migration apply-path test). Teardown waits until a
// class-naming probe write is no longer denied by this policy — binding deletion
// propagates asynchronously.
func installPriorityClassGuard(t *testing.T) {
	t.Helper()
	createNamespace(t, vapProbeNamespace)

	f, err := os.Open(priorityClassGuardManifestPath)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	dec := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
	var installed []*unstructured.Unstructured
	for {
		u := &unstructured.Unstructured{}
		if decErr := dec.Decode(u); decErr != nil {
			if decErr == io.EOF {
				break
			}
			require.NoError(t, decErr)
		}
		if len(u.Object) == 0 {
			continue
		}
		if createErr := k8sClient.Create(ctx, u); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			require.NoError(t, createErr)
		}
		installed = append(installed, u)
	}

	t.Cleanup(func() {
		// Delete in reverse manifest order (binding before policy), then confirm
		// enforcement is gone: RunnerGroups have no webhook, so once the policy
		// stops answering, a probe naming an arbitrary class is admitted.
		for i := len(installed) - 1; i >= 0; i-- {
			if delErr := k8sClient.Delete(context.Background(), installed[i]); delErr != nil && !apierrors.IsNotFound(delErr) {
				t.Errorf("tearing down %s %q: %v", installed[i].GetKind(), installed[i].GetName(), delErr)
			}
		}
		attempt := 0
		gomega.NewWithT(t).Eventually(func() error {
			attempt++
			probe := guardedRG(vapProbeNamespace, fmt.Sprintf("guard-teardown-probe-%d", attempt), "guard-teardown-class", "")
			if createErr := k8sClient.Create(context.Background(), probe); createErr != nil {
				return createErr
			}
			_ = k8sClient.Delete(context.Background(), probe)
			return nil
		}, 30*time.Second, 100*time.Millisecond).Should(gomega.Succeed(),
			"the policy must stop denying class-naming writes once uninstalled")
	})
}

// setGuardAllowlist writes the param object's spec.allowedPriorityClasses,
// creating it if a prior step deleted it.
func setGuardAllowlist(t *testing.T, names ...string) {
	t.Helper()
	var pca agcv2beta1.PriorityClassAllowlist
	err := k8sClient.Get(ctx, types.NamespacedName{Name: vapParamName}, &pca)
	if apierrors.IsNotFound(err) {
		pca = agcv2beta1.PriorityClassAllowlist{
			ObjectMeta: metav1.ObjectMeta{Name: vapParamName},
			Spec:       agcv2beta1.PriorityClassAllowlistSpec{AllowedPriorityClasses: names},
		}
		require.NoError(t, k8sClient.Create(ctx, &pca))
		return
	}
	require.NoError(t, err)
	pca.Spec.AllowedPriorityClasses = names
	require.NoError(t, k8sClient.Update(ctx, &pca))
}

// guardedRG returns a RunnerGroup naming tierClass in priorityTiers (when
// non-empty) and podClass in podTemplate.spec (when non-empty) — the two routes
// the policy gates.
func guardedRG(ns, name, tierClass, podClass string) *agcv1alpha1.RunnerGroup {
	rg := &agcv1alpha1.RunnerGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: agcv1alpha1.RunnerGroupSpec{
			MaxListeners: 1,
			RunnerLabels: []string{"self-hosted"},
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					PriorityClassName: podClass,
					Containers:        []corev1.Container{{Name: "runner", Image: "runner:test"}},
				},
			},
		},
	}
	if tierClass != "" {
		maxWorkers := int32(5)
		rg.Spec.MaxWorkers = &maxWorkers
		rg.Spec.PriorityTiers = []agcv1alpha1.PriorityTier{{PriorityClassName: tierClass, Threshold: 5}}
	}
	return rg
}

// TestGMC_PriorityClassGuard_GatesDirectRunnerGroupWrites verifies the Q289 VAP
// backstop (Appendix G §G.7): RunnerGroups have no validating webhook, so a
// principal with direct runnergroups RBAC bypasses the ActionsGateway webhook's
// PriorityClass allowlist — unless this policy denies the write. The policy reads
// its allowlist from a paramKind PriorityClassAllowlist and must (1) deny
// off-allowlist classes on both the priorityTiers and podTemplate.spec routes,
// (2) admit allowlisted classes and class-free RunnerGroups, (3) fail closed — for
// every runnergroups write — when the param object is missing, and (4) re-validate
// stored objects on update, which a webhook alone cannot do for the bypass path.
func TestGMC_PriorityClassGuard_GatesDirectRunnerGroupWrites(t *testing.T) {
	installPriorityClassGuard(t)

	const ns = "pcguard"
	const escalation = "system-cluster-critical"
	const allowed = "runner-standard"
	createNamespace(t, ns)

	g := gomega.NewWithT(t)

	// Gate: wait until the policy is enforced (VAP enforcement is not instantaneous
	// after the binding is created). The shipped param object is empty, so a
	// tier naming any class must become Forbidden.
	g.Eventually(func() bool {
		err := k8sClient.Create(ctx, guardedRG(ns, "probe", escalation, ""))
		if err == nil {
			// Enforcement not active yet; remove the probe and keep polling.
			_ = k8sClient.Delete(ctx, guardedRG(ns, "probe", escalation, ""))
			return false
		}
		return apierrors.IsForbidden(err)
	}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(),
		"a tier naming a class must be denied under the shipped empty allowlist")

	t.Run("empty allowlist denies both routes", func(t *testing.T) {
		err := k8sClient.Create(ctx, guardedRG(ns, "tier-escalate", escalation, ""))
		require.True(t, apierrors.IsForbidden(err), "tier route: want Forbidden, got: %v", err)
		err = k8sClient.Create(ctx, guardedRG(ns, "pod-escalate", "", escalation))
		require.True(t, apierrors.IsForbidden(err), "podTemplate route: want Forbidden, got: %v", err)
	})

	t.Run("class-free RunnerGroups are never subject to the policy", func(t *testing.T) {
		rg := guardedRG(ns, "plain", "", "")
		require.NoError(t, k8sClient.Create(ctx, rg))
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rg) })
	})

	// Allowlist the class — both routes must admit it, live.
	setGuardAllowlist(t, allowed, "runner-bursty")
	g.Eventually(func() error {
		return k8sClient.Create(ctx, guardedRG(ns, "tier-allowed", allowed, ""))
	}, 30*time.Second, 100*time.Millisecond).Should(gomega.Succeed(),
		"an allowlisted tier class must be admitted once the param object lists it")

	t.Run("allowlisted class admitted on both routes", func(t *testing.T) {
		rg := guardedRG(ns, "pod-allowed", "", "runner-bursty")
		require.NoError(t, k8sClient.Create(ctx, rg))
	})

	t.Run("escalation stays denied regardless of the dynamic set", func(t *testing.T) {
		err := k8sClient.Create(ctx, guardedRG(ns, "tier-escalate-2", escalation, ""))
		require.True(t, apierrors.IsForbidden(err), "want Forbidden, got: %v", err)
	})

	t.Run("update re-validates stored objects", func(t *testing.T) {
		// The stored-object narrowing a webhook cannot provide on the bypass path:
		// shrink the allowlist, then touch the previously-admitted RunnerGroup — the
		// write must be denied even though the class was legal when stored.
		setGuardAllowlist(t, "runner-bursty")
		gomega.NewWithT(t).Eventually(func() bool {
			var rg agcv1alpha1.RunnerGroup
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "tier-allowed"}, &rg); err != nil {
				return false
			}
			rg.Spec.MaxListeners = 2
			err := k8sClient.Update(ctx, &rg)
			return apierrors.IsForbidden(err)
		}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(),
			"an update to a stored RunnerGroup naming a now-off-allowlist class must be denied")
	})

	t.Run("missing param object fails closed for every write", func(t *testing.T) {
		// parameterNotFoundAction: Deny is the never-silently-off contract: the
		// apiserver resolves binding params before any per-object filtering, so a
		// deleted param object denies EVERY runnergroups write — including
		// class-free ones — until it is recreated. Loud and fail-closed by design;
		// the object ships with the policy, so this state is an explicit deletion.
		//
		// It is ALSO the Q492 regression test for the Q444 apiserver defect: with a
		// core-type paramKind, deleting and recreating the param is exactly the
		// transition that used to leave resolution permanently broken. The recovery
		// assertion below is what a ConfigMap paramKind could not satisfy.
		var pca agcv2beta1.PriorityClassAllowlist
		require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: vapParamName}, &pca))
		require.NoError(t, k8sClient.Delete(ctx, &pca))

		// A create that slips through before the apiserver observes the deletion is
		// removed and retried under a fresh name — a fixed name would turn every
		// subsequent attempt into AlreadyExists and never observe the denial. The
		// param-not-found denial does not carry StatusReasonForbidden (unlike a
		// validation-failure denial), so it is matched by the denying policy's name.
		attempt := 0
		var lastErr error
		gomega.NewWithT(t).Eventually(func() bool {
			attempt++
			rg := guardedRG(ns, fmt.Sprintf("paramless-%d", attempt), "", "")
			lastErr = k8sClient.Create(ctx, rg)
			if lastErr == nil {
				_ = k8sClient.Delete(ctx, rg)
				return false
			}
			return strings.Contains(lastErr.Error(), "gag-priorityclass-allowlist-guard") &&
				strings.Contains(lastErr.Error(), "denied request")
		}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(), func() string {
			return fmt.Sprintf("parameterNotFoundAction: Deny must deny even a class-free write when the param object is gone; last create error: %v", lastErr)
		})

		// Recreating the param restores normal admission — class-free writes are
		// admitted again without touching the policy or binding. This is the
		// property Q444 destroyed for a ConfigMap paramKind.
		setGuardAllowlist(t)
		recovered := 0
		gomega.NewWithT(t).Eventually(func() error {
			recovered++
			return k8sClient.Create(ctx, guardedRG(ns, fmt.Sprintf("recovered-%d", recovered), "", ""))
		}, 30*time.Second, 100*time.Millisecond).Should(gomega.Succeed(),
			"recreating the param object must restore admission of class-free RunnerGroups")
	})

	// Q323: the same policy now also matches the v2 kinds; exercised here (not as
	// a separate Test function) so the guard and its param namespace are installed
	// exactly once — see runV2GuardSubtests.
	runV2GuardSubtests(t, ns)
}

// guardedV2RS returns a v2alpha1 RunnerSet naming tierClass in priorityTiers, with
// a per-set runnerLabel — the suite's RunnerSet webhook enforces ScaleSet
// runnerLabel uniqueness per gateway, so reusing one label would trip that guard
// instead of the policy under test.
func guardedV2RS(ns, name, tierClass string) *agcv2alpha1.RunnerSet {
	rs := rsWithTierPriorityClass(name, ns, tierClass)
	rs.Spec.RunnerLabels = []string{name}
	return rs
}

// runV2GuardSubtests verifies the Q323 extension of the VAP backstop to the v2
// kinds: runnersets (priorityTiers route) and runnertemplates (podTemplate.spec
// route) — both across v2alpha1 and v2beta1. Unlike v1 runnergroups, the v2 kinds
// DO have failurePolicy=fail webhooks, so here the policy is defense-in-depth: it
// must answer before the webhooks (VAPs run first in the validation phase), deny
// off-allowlist classes from any writer, and admit class-free objects untouched.
// The suite's RunnerSet webhook allowlist contains exactly "high", which lets the
// test tell the two layers apart by which one denies.
//
// Runs as a subtest section of the parent VAP test rather than its own Test
// function: the policy install must happen exactly once, and its teardown waits
// for enforcement to actually stop, so a second install racing that teardown
// would see nondeterministic admission.
func runV2GuardSubtests(t *testing.T, ns string) {
	setGuardAllowlist(t)

	g := gomega.NewWithT(t)

	// Gate: wait for the empty allowlist to be observed. "high" is ON the suite's
	// RunnerSet webhook allowlist, so a denial naming the policy can only be the VAP.
	probeAttempt := 0
	g.Eventually(func() bool {
		probeAttempt++
		rs := guardedV2RS(ns, fmt.Sprintf("probe-%d", probeAttempt), "high")
		err := k8sClient.Create(ctx, rs)
		if err == nil {
			_ = k8sClient.Delete(ctx, rs)
			return false
		}
		return strings.Contains(err.Error(), "gag-priorityclass-allowlist-guard")
	}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(),
		"a v2 RunnerSet tier class must be denied by the policy under the empty allowlist")

	t.Run("v2: empty allowlist denies the tier route by policy, not webhook", func(t *testing.T) {
		err := k8sClient.Create(ctx, guardedV2RS(ns, "tier-escalate", "high"))
		require.True(t, apierrors.IsForbidden(err), "want Forbidden, got: %v", err)
		require.ErrorContains(t, err, "gag-priorityclass-allowlist-guard")
	})

	t.Run("v2: empty allowlist denies the podTemplate route before the webhook", func(t *testing.T) {
		// The suite's RunnerTemplate webhook (nil allowlist) would also deny this
		// class — but VAPs run before validating webhooks, so the policy answers,
		// proving runnertemplates are matched.
		err := k8sClient.Create(ctx, rtWithPodTemplatePriorityClass("pod-escalate", ns, "high"))
		require.Error(t, err)
		require.ErrorContains(t, err, "gag-priorityclass-allowlist-guard")
	})

	t.Run("v2: class-free objects are never subject to the policy", func(t *testing.T) {
		rs := &agcv2alpha1.RunnerSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "plain-rs"},
			Spec: agcv2alpha1.RunnerSetSpec{
				GatewayRef:   agcv2alpha1.ObjectRef{Name: "gateway"},
				RunnerLabels: []string{"plain-rs"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, rs))
		require.NoError(t, k8sClient.Create(ctx, rtWithPodTemplatePriorityClass("plain-rt", ns, "")))
	})

	t.Run("v2: allowlisted tier class admitted end-to-end", func(t *testing.T) {
		setGuardAllowlist(t, "high")
		gomega.NewWithT(t).Eventually(func() error {
			return k8sClient.Create(ctx, guardedV2RS(ns, "tier-allowed", "high"))
		}, 30*time.Second, 100*time.Millisecond).Should(gomega.Succeed(),
			"an allowlisted tier class must be admitted once the param object lists it")

		// The layering proof for runnertemplates: with the class on the VAP
		// allowlist, the denial falls through to the webhook (whose allowlist in
		// this suite is nil) — the policy no longer answers.
		var lastErr error
		gomega.NewWithT(t).Eventually(func() bool {
			lastErr = k8sClient.Create(ctx, rtWithPodTemplatePriorityClass("pod-webhook-denied", ns, "high"))
			return lastErr != nil && !strings.Contains(lastErr.Error(), "gag-priorityclass-allowlist-guard")
		}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(), func() string {
			return fmt.Sprintf("with the class allowlisted in the policy, the webhook must be the denier; last error: %v", lastErr)
		})
	})

	t.Run("v2: v2beta1 writes are matched too", func(t *testing.T) {
		setGuardAllowlist(t)
		attempt := 0
		var lastErr error
		gomega.NewWithT(t).Eventually(func() bool {
			attempt++
			rs := &agcv2beta1.RunnerSet{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: fmt.Sprintf("beta-tier-%d", attempt)},
				Spec: agcv2beta1.RunnerSetSpec{
					GatewayRef:    agcv2beta1.ObjectRef{Name: "gateway"},
					RunnerLabels:  []string{fmt.Sprintf("beta-tier-%d", attempt)},
					PriorityTiers: []agcv2beta1.PriorityTier{{PriorityClassName: "high", Threshold: 5}},
				},
			}
			lastErr = k8sClient.Create(ctx, rs)
			if lastErr == nil {
				_ = k8sClient.Delete(ctx, rs)
				return false
			}
			return strings.Contains(lastErr.Error(), "gag-priorityclass-allowlist-guard")
		}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(), func() string {
			return fmt.Sprintf("a v2beta1 RunnerSet tier class must be denied by the policy; last error: %v", lastErr)
		})
	})
}
