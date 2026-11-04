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

	agcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
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
// no namespace; vapProbeNamespace is only where this test's teardown probes go.
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
		// enforcement is gone: "high" is on the suite's RunnerSet webhook allowlist,
		// so once the policy stops answering, a probe naming it is admitted.
		for i := len(installed) - 1; i >= 0; i-- {
			if delErr := k8sClient.Delete(context.Background(), installed[i]); delErr != nil && !apierrors.IsNotFound(delErr) {
				t.Errorf("tearing down %s %q: %v", installed[i].GetKind(), installed[i].GetName(), delErr)
			}
		}
		attempt := 0
		gomega.NewWithT(t).Eventually(func() error {
			attempt++
			probe := guardedV2RS(vapProbeNamespace, fmt.Sprintf("guard-teardown-probe-%d", attempt), "high")
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
	var pca agcv2.PriorityClassAllowlist
	err := k8sClient.Get(ctx, types.NamespacedName{Name: vapParamName}, &pca)
	if apierrors.IsNotFound(err) {
		pca = agcv2.PriorityClassAllowlist{
			ObjectMeta: metav1.ObjectMeta{Name: vapParamName},
			Spec:       agcv2.PriorityClassAllowlistSpec{AllowedPriorityClasses: names},
		}
		require.NoError(t, k8sClient.Create(ctx, &pca))
		return
	}
	require.NoError(t, err)
	pca.Spec.AllowedPriorityClasses = names
	require.NoError(t, k8sClient.Update(ctx, &pca))
}

// TestGMC_PriorityClassGuard_GatesRunnerSetAndTemplateWrites verifies the Q289 VAP
// backstop (Appendix G §G.7) on the kinds that carry a PriorityClass: runnersets
// (priorityTiers) and runnertemplates (podTemplate.spec). The policy reads its
// allowlist from a paramKind PriorityClassAllowlist and must deny off-allowlist
// classes from any writer, admit allowlisted classes and class-free objects,
// re-validate stored objects on update, and fail closed for every matched write
// when the param object is missing.
func TestGMC_PriorityClassGuard_GatesRunnerSetAndTemplateWrites(t *testing.T) {
	installPriorityClassGuard(t)

	const ns = "pcguard"
	createNamespace(t, ns)

	runV2GuardSubtests(t, ns)

	t.Run("missing param object fails closed for every write", func(t *testing.T) {
		// parameterNotFoundAction: Deny is the never-silently-off contract: the
		// apiserver resolves binding params before any per-object filtering, so a
		// deleted param object denies EVERY matched write, class-free ones included,
		// until it is recreated. Loud and fail-closed by design; the object ships
		// with the policy, so this state is an explicit deletion.
		//
		// It is ALSO the Q492 regression test for the Q444 apiserver defect: with a
		// core-type paramKind, deleting and recreating the param is exactly the
		// transition that used to leave resolution permanently broken. The recovery
		// assertion below is what a ConfigMap paramKind could not satisfy.
		var pca agcv2.PriorityClassAllowlist
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
			rs := plainV2RS(ns, fmt.Sprintf("paramless-%d", attempt))
			lastErr = k8sClient.Create(ctx, rs)
			if lastErr == nil {
				_ = k8sClient.Delete(ctx, rs)
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
			return k8sClient.Create(ctx, plainV2RS(ns, fmt.Sprintf("recovered-%d", recovered)))
		}, 30*time.Second, 100*time.Millisecond).Should(gomega.Succeed(),
			"recreating the param object must restore admission of class-free RunnerSets")
	})
}

// plainV2RS returns a class-free v2 RunnerSet with a per-set runnerLabel.
func plainV2RS(ns, name string) *agcv2.RunnerSet {
	return &agcv2.RunnerSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: agcv2.RunnerSetSpec{
			GatewayRef:   agcv2.ObjectRef{Name: "gateway"},
			RunnerLabels: []string{name},
		},
	}
}

// guardedV2RS returns a v2 RunnerSet naming tierClass in priorityTiers, with
// a per-set runnerLabel — the suite's RunnerSet webhook enforces ScaleSet
// runnerLabel uniqueness per gateway, so reusing one label would trip that guard
// instead of the policy under test.
func guardedV2RS(ns, name, tierClass string) *agcv2.RunnerSet {
	rs := rsWithTierPriorityClass(name, ns, tierClass)
	rs.Spec.RunnerLabels = []string{name}
	return rs
}

// runV2GuardSubtests verifies the Q323 extension of the VAP backstop to the v2
// kinds: runnersets (priorityTiers route) and runnertemplates (podTemplate.spec
// route). Both kinds have failurePolicy=fail
// webhooks, so the policy is defense-in-depth: it must answer before the webhooks (VAPs run first in the validation phase), deny
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
		rs := &agcv2.RunnerSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "plain-rs"},
			Spec: agcv2.RunnerSetSpec{
				GatewayRef:   agcv2.ObjectRef{Name: "gateway"},
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

	t.Run("v2: deletion-only updates are exempt from re-validation", func(t *testing.T) {
		// Q518 on the v2 kinds: same wedge, same exemption. "high" stays on the
		// suite's RunnerSet webhook allowlist throughout, so every denial below is
		// the policy's.
		setGuardAllowlist(t, "high")
		rs := guardedV2RS(ns, "deletion-exempt", "high")
		rs.Finalizers = []string{"integration-test/hold"}
		gomega.NewWithT(t).Eventually(func() error {
			return k8sClient.Create(ctx, rs)
		}, 30*time.Second, 100*time.Millisecond).Should(gomega.Succeed(),
			"the allowlisted tier class must be admitted once the param update propagates")
		t.Cleanup(func() {
			var left agcv2.RunnerSet
			if getErr := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "deletion-exempt"}, &left); getErr == nil {
				left.Finalizers = nil
				_ = k8sClient.Update(context.Background(), &left)
				_ = k8sClient.Delete(context.Background(), &left)
			}
		})

		// Narrow the allowlist back to empty and wait for enforcement on the live
		// object. Each attempt bumps the tier threshold so it is a REAL spec change
		// even if an earlier attempt was admitted before the narrowed param
		// propagated. The threshold (not maxWorkers) is the mutation because CRD
		// CEL validation runs BEFORE validating admission: an update that trips a
		// CEL rule (maxWorkers != last threshold) never reaches the policy.
		setGuardAllowlist(t)
		threshold := int32(5)
		var lastUpdateErr error
		gomega.NewWithT(t).Eventually(func() bool {
			var live agcv2.RunnerSet
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "deletion-exempt"}, &live); err != nil {
				return false
			}
			threshold++
			live.Spec.PriorityTiers[0].Threshold = threshold
			lastUpdateErr = k8sClient.Update(ctx, &live)
			return lastUpdateErr != nil && strings.Contains(lastUpdateErr.Error(), "gag-priorityclass-allowlist-guard")
		}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeTrue(), func() string {
			return fmt.Sprintf("a spec update on the live RunnerSet must be denied by the policy once the class is removed; last error: %v", lastUpdateErr)
		})

		require.NoError(t, k8sClient.Delete(ctx, rs))
		var deleting agcv2.RunnerSet
		require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "deletion-exempt"}, &deleting))
		require.NotNil(t, deleting.DeletionTimestamp)

		specChange := deleting.DeepCopy()
		specChange.Spec.PriorityTiers[0].Threshold = 100 // distinct from every value the loop above may have stored
		err := k8sClient.Update(ctx, specChange)
		require.Error(t, err, "a spec change on a deleting RunnerSet must still be denied")
		require.ErrorContains(t, err, "gag-priorityclass-allowlist-guard")

		deleting.Finalizers = nil
		require.NoError(t, k8sClient.Update(ctx, &deleting),
			"the finalizer-removal update on a deleting RunnerSet must be admitted")
		gomega.NewWithT(t).Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "deletion-exempt"}, &agcv2.RunnerSet{}))
		}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())
	})
}
